package treesitter

import (
	"bytes"
	"cmp"
	"slices"
	"strings"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider"
	"github.com/Sawmonabo/codectx/internal/provider/link"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/wire"
)

// detailLinkFailure is the structure row's disclosure of a file whose link
// pass failed in the worker (wire.Done.LinkFailure): the file stores no link
// table, so the linking provider's `calls` for its unit is partial, while the
// file's structural facts stand complete.
const detailLinkFailure = "link_failure"

// linkTable joins the worker's link tables to the declarations and call
// references the builder validated and resolved, as the file's link.Table.
// It runs after refs, once every declaration has its identity.
//
// An Export joins the declaration whose name token it names, a Site the call
// reference whose callee token it names. Every call reference is a site, with
// the reference's caller, range, callee and in-file candidates (targets). A
// joined site takes the worker's shape, signature and types, and the
// worker's qualifier where it names one (a static site's), else the
// reference's, so QualifierIsImport always describes the qualifier stored;
// one no Site joins takes its shape from the reference (a qualified call is a
// member call, any other a bare one), keeps the reference's qualifier and
// carries no recovered type. A
// TypeResult names a worker site and is remapped to that site's index in
// Table.Sites. An entry that joins nothing, joins what an earlier entry
// already joined, names a range that does not fit the pinned bytes, or names
// a site that joined nothing is dropped and counted in Table.Dropped, never
// fatal: the table is the file's best account, and a non-zero count makes
// the unit's `calls` partial.
//
// A file whose module has no identity (build gave up on an over-long path)
// has no endpoint to link from and stores no table; neither does a file whose
// dependence or link pass failed, and the worker sending link tables for one
// is a protocol violation.
func (b *builder) linkTable() error {
	wl := b.ex.link
	if wl == nil {
		return nil
	}
	if b.ex.done.DependenceFailure != "" || b.ex.done.LinkFailure != "" {
		return outputInvalid("a file whose dependence or link pass failed also sent link tables")
	}
	t := &link.Table{Path: b.fv.Path, Language: b.lang.Name, Package: b.ex.done.Package, Imports: b.linkImports(),
		Supertypes: wl.Supertypes, Module: link.Endpoint{Node: b.module.Node.ID, CanonicalKey: b.module.CanonicalKey, Kind: b.module.Node.Kind,
			Name: b.module.Node.Name, Qualified: b.module.Node.QualifiedName, Range: *b.fileRng}}

	// siteAt maps each worker site to its index in t.Sites, -1 for one that
	// joined no call reference; it is filled before any type is remapped,
	// because a type may name a later site.
	siteAt := make([]int32, len(wl.Sites))
	byName := make(map[wire.Span]int, len(wl.Sites))
	for i, s := range wl.Sites {
		siteAt[i] = -1
		if _, dup := byName[s.Name]; dup {
			t.Dropped++
			continue
		}
		byName[s.Name] = i
	}
	t.Sites = make([]link.Site, 0, len(b.ex.refs))
	joined := make([]int, 0, len(b.ex.refs))
	for _, r := range b.ex.refs {
		if r.Kind != "call" {
			continue
		}
		rng, err := b.rangeOf(r.Start, r.End)
		if err != nil {
			return err
		}
		s := link.Site{Caller: int32(r.Scope), Range: *rng, NameStart: r.NameStart, NameEnd: r.NameEnd, Callee: r.Name,
			Shape: wire.ShapeBare, Qualifier: r.Qualifier, QualifierIsImport: r.QualifierIsImport}
		if r.Qualified {
			s.Shape = wire.ShapeMember
		}
		for _, c := range b.targets(r) {
			s.Candidates = append(s.Candidates, int32(c))
		}
		w := -1
		if i, ok := byName[wire.Span{Start: r.NameStart, End: r.NameEnd}]; ok && siteAt[i] < 0 {
			// The first reference at a callee token claims the worker's site;
			// a second reference at the same token keeps its own shape.
			w, siteAt[i] = i, int32(len(t.Sites))
			s.Shape, s.Qualifier, s.Signature = wl.Sites[i].Shape, cmp.Or(wl.Sites[i].Qualifier, r.Qualifier), wl.Sites[i].Signature
		}
		joined = append(joined, w)
		t.Sites = append(t.Sites, s)
	}
	for _, i := range byName {
		if siteAt[i] < 0 {
			t.Dropped++
		}
	}
	typ := func(x wire.Type) wire.Type {
		if x.Kind != wire.TypeResult {
			return x
		}
		if uint64(x.Index) < uint64(len(siteAt)) && siteAt[x.Index] >= 0 {
			x.Index = uint32(siteAt[x.Index])
			return x
		}
		t.Dropped++
		return wire.Type{}
	}
	for i, w := range joined {
		if w < 0 {
			continue
		}
		ws := &wl.Sites[w]
		t.Sites[i].Receiver = typ(ws.Receiver)
		if len(ws.Args) > 0 {
			t.Sites[i].Args = make([]wire.Type, len(ws.Args))
			for j, a := range ws.Args {
				t.Sites[i].Args[j] = typ(a)
			}
		}
	}

	t.Decls = make([]link.Decl, len(b.decls))
	for i := range b.decls {
		d := &b.decls[i]
		t.Decls[i] = link.Decl{Endpoint: link.Endpoint{Node: d.res.Node.ID, CanonicalKey: d.res.CanonicalKey, Kind: d.kind,
			Name: d.Name, Qualified: d.Qualified, Range: *d.rng}, Parent: int32(d.Parent), Exported: d.Exported,
			Prototype: d.Prototype, Default: d.Parent < 0 && d.Exported && ecmaScript[b.lang.Name] && exportsDefault(b.src, d.Start)}
	}
	for _, x := range wl.Exports {
		i, ok := b.nameAt[x.Name]
		if !ok || t.Decls[i].Described {
			t.Dropped++
			continue
		}
		d := &t.Decls[i]
		d.Described, d.Receiver, d.Signature, d.Params, d.Variadic = true, x.Receiver, x.Signature, x.Params, x.Variadic
		d.Static, d.Abstract, d.Returns = x.Static, x.Abstract, typ(x.Returns)
	}

	if len(wl.Values) > 0 {
		byStart := b.declsByStart()
		t.Values = make([]link.Value, 0, len(wl.Values))
		for _, v := range wl.Values {
			rng, err := b.rangeOf(v.At.Start, v.At.End)
			if err != nil {
				t.Dropped++
				continue
			}
			t.Values = append(t.Values, link.Value{Caller: b.enclosing(byStart, v.At), Range: *rng, Field: v.Field, Type: typ(v.Type)})
		}
	}
	b.table = t
	return nil
}

// ecmaScript are the languages whose `export default` names a declaration.
var ecmaScript = map[string]bool{"javascript": true, "typescript": true, "tsx": true}

// exportsDefault reports that the exported declaration starting at start is
// the declaration of an `export default` statement: the grammar puts the
// keyword `default` right before the declaration and `export` right before
// it, with only whitespace and comments between the tokens (a decorator is
// part of the class declaration it decorates). A line comment between them
// is not read back and leaves the declaration a named export only.
func exportsDefault(src []byte, start uint32) bool {
	before := src[:start]
	for _, word := range []string{"default", "export"} {
		before = trimBlockComments(before)
		if !bytes.HasSuffix(before, []byte(word)) {
			return false
		}
		before = before[:len(before)-len(word)]
		if n := len(before); n > 0 && isIdentByte(before[n-1]) {
			return false
		}
	}
	return true
}

// trimBlockComments drops the whitespace and block comments b ends with.
func trimBlockComments(b []byte) []byte {
	for {
		b = bytes.TrimRight(b, " \t\r\n\f\v")
		if !bytes.HasSuffix(b, []byte("*/")) {
			return b
		}
		i := bytes.LastIndex(b[:len(b)-2], []byte("/*"))
		if i < 0 {
			return b
		}
		b = b[:i]
	}
}

func isIdentByte(c byte) bool {
	return c == '_' || c == '$' || c >= 0x80 || '0' <= c && c <= '9' || 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z'
}

// linkImports is the names the file's import records introduce, each with
// the path it denotes (link.Import). The worker's record is one statement:
// its path as written and the local names it binds. A Rust use tree is
// split into one path per leaf. An import of every member of a path (a Go
// dot import, a Java on-demand import `a.b.*`, a C++ using-directive, a Rust
// glob, a Python `from m import *`) is named link.ImportAll, which the
// statement's own bytes tell; an import binding no name (an include, an
// ECMAScript side-effect import) has an empty name.
func (b *builder) linkImports() []link.Import {
	var out []link.Import
	for _, imp := range b.ex.imports {
		text := string(b.src[imp.Start:imp.End])
		names := imp.Names
		switch b.lang.Name {
		case "rust":
			rustUse("", imp.Path, func(name, p string) { out = append(out, link.Import{Name: name, Path: p}) })
			continue
		case "go":
			if len(names) == 1 && names[0] == "." {
				names = []string{link.ImportAll}
			}
		case "java", "python":
			if strings.Contains(text, "*") {
				names = []string{link.ImportAll}
			}
		case "c", "cpp":
			switch f := strings.Fields(text); {
			case len(f) > 2 && f[0] == "using" && f[1] == "namespace":
				names = []string{link.ImportAll}
			case len(f) < 2 || f[0] != "using":
				// An include binds no name.
				names = nil
			}
		}
		if len(names) == 0 {
			out = append(out, link.Import{Path: imp.Path})
		}
		for _, n := range names {
			out = append(out, link.Import{Name: n, Path: imp.Path})
		}
	}
	return out
}

// rustUse yields every leaf of the Rust use tree p, written inside a list
// whose path so far is prefix: the name it binds and the full path it names
// (`a::{b, c as d}` binds b to a::b and d to a::c). A glob yields the module
// it imports from as link.ImportAll; `self` in a
// list binds the list's own path; `as _` binds nothing.
func rustUse(prefix, p string, yield func(name, path string)) {
	join := func(a, b string) string {
		if a == "" {
			return b
		}
		if b == "" {
			return a
		}
		return a + "::" + b
	}
	p = strings.TrimSpace(p)
	if i := strings.IndexByte(p, '{'); i >= 0 {
		prefix = join(prefix, strings.TrimSuffix(strings.TrimSpace(p[:i]), "::"))
		inner := strings.TrimSuffix(strings.TrimSpace(p[i+1:]), "}")
		depth, from := 0, 0
		for j := 0; j <= len(inner); j++ {
			if j < len(inner) {
				switch inner[j] {
				case '{':
					depth++
					continue
				case '}':
					depth--
					continue
				case ',':
					if depth > 0 {
						continue
					}
				default:
					continue
				}
			}
			if item := strings.TrimSpace(inner[from:j]); item != "" {
				rustUse(prefix, item, yield)
			}
			from = j + 1
		}
		return
	}
	if f := strings.Fields(p); len(f) >= 3 && f[len(f)-2] == "as" {
		if alias := f[len(f)-1]; alias != "_" {
			yield(alias, join(prefix, strings.Join(f[:len(f)-2], "")))
		}
		return
	}
	leaf := p
	if i := strings.LastIndex(p, "::"); i >= 0 {
		leaf = p[i+2:]
	}
	switch leaf {
	case "*":
		yield(link.ImportAll, join(prefix, strings.TrimSuffix(strings.TrimSuffix(p, "*"), "::")))
	case "self":
		if prefix != "" {
			yield(prefix[strings.LastIndex(prefix, ":")+1:], prefix)
		}
	default:
		yield(leaf, join(prefix, p))
	}
}

// enclosing is the index of the innermost declaration whose range contains
// s, -1 for the module. Of the declarations starting at or before s, the one
// with the greatest start that contains it is the innermost; among equal
// starts the later declaration is nested in the earlier one.
func (b *builder) enclosing(byStart []int, s wire.Span) int32 {
	n, _ := slices.BinarySearchFunc(byStart, s.Start+1, func(i int, start uint32) int { return cmp.Compare(b.decls[i].Start, start) })
	for k := n - 1; k >= 0; k-- {
		if b.decls[byStart[k]].End >= s.End {
			return int32(byStart[k])
		}
	}
	return -1
}

// storeTable stores the file's link table with the unit, under
// link.TableKind, when the builder joined one.
func (b *builder) storeTable(sink provider.StateSink) error {
	if b.table == nil {
		return nil
	}
	payload, err := b.table.Encode()
	if err != nil {
		return &model.Error{Code: model.CodeInternal, Message: "link table of " + bound(b.fv.Path, 256) + ": " + err.Error()}
	}
	b.table = nil
	return sink.PutState(b.ctx, link.TableKind, payload)
}
