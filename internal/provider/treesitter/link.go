package treesitter

import (
	"cmp"
	"slices"
	"strings"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider"
	"github.com/Sawmonabo/codectx/internal/provider/link"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/lang"
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
// worker's qualifier where it names one (a static site's: Base, not a, for
// `a.Base::f()`), else the reference's; QualifierIsImport, which the
// reference's qualifier decides, is kept only when that is the one stored;
// one no Site joins takes its shape from the reference (a qualified call is a
// member call, any other a bare one), keeps the reference's qualifier and
// carries no recovered type. A
// TypeResult names a worker site and is remapped to that site's index in
// Table.Sites, inside a union too; the unions and re-exports are copied. An entry that joins nothing, joins what an earlier entry
// already joined, names a range that does not fit the pinned bytes, or names
// a site that joined nothing is dropped and counted in Table.Dropped, never
// fatal: the table is the file's best account, and a non-zero count makes
// the unit's `calls` partial. Each callable whose analysis failed in the
// worker is counted there too: the worker's linker never saw it, so its
// values and its sites' recovered receiver and argument types are missing
// from the table, and the sites resolve on less than the file says.
//
// A joined site also takes the worker's arguments (wire.Site.Arguments), each
// with its range, keyword, spread and reads; a read is a variable endpoint
// carrying its identifier's range here, which functions fills with the
// variable node the dependence facts publish and drops when no node was
// minted for it (tableVariables). The worker's computed sites, calls whose
// callee names nothing, follow the reference-joined sites as sites of their
// own (link.Site.Computed). A declaration takes its export's override mark
// and alias, its property mark, the export mark of a CommonJS export (OR'd
// into the structural one), and its declared parameters
// (wire.Link.Parameters), each a
// variable endpoint functions fills the same way. A dynamic import becomes a
// link.DynamicImport, or, when its path folds to a constant, an ordinary
// import appended to Table.Imports.
// A value whose expression declares a function (wire.Value.Declares)
// carries that declaration (link.Value.Declares); one whose declaration
// joined nothing is dropped and counted, never resolved by its name.
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
	imports, err := b.linkImports()
	if err != nil {
		return err
	}
	t := &link.Table{Path: b.fv.Path, Language: b.lang.Name, Package: b.ex.done.Package, Imports: imports,
		Supertypes: wl.Supertypes, Module: link.Endpoint{Node: b.module.Node.ID, CanonicalKey: b.module.CanonicalKey, Kind: b.module.Node.Kind,
			Name: b.module.Node.Name, Qualified: b.module.Node.QualifiedName, Range: *b.fileRng}}
	for i := range b.ex.functions {
		if b.ex.functions[i].Failed {
			t.Dropped++
		}
	}

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
	// A call in a route handler written as a function literal is made from
	// the handler's node, as the structural provider's own call is
	// (literalAround).
	handler := map[model.NodeID]int32{}
	for _, l := range b.literals {
		if _, ok := handler[l.id]; !ok {
			t.Handlers = append(t.Handlers, l.ep)
			handler[l.id] = int32(len(t.Handlers))
		}
	}
	handlerOf := func(r wire.Ref) int32 {
		if id, ok := b.literalAround(r); ok {
			return handler[id]
		}
		return 0
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
		s.Handler = handlerOf(r)
		for _, c := range b.targets(r) {
			s.Candidates = append(s.Candidates, int32(c))
		}
		w := -1
		if i, ok := byName[wire.Span{Start: r.NameStart, End: r.NameEnd}]; ok && siteAt[i] < 0 {
			// The first reference at a callee token claims the worker's site;
			// a second reference at the same token keeps its own shape.
			w, siteAt[i] = i, int32(len(t.Sites))
			s.Described = true
			s.Shape, s.Qualifier, s.Signature = wl.Sites[i].Shape, cmp.Or(wl.Sites[i].Qualifier, r.Qualifier), wl.Sites[i].Signature
			s.QualifierIsImport = s.QualifierIsImport && s.Qualifier == r.Qualifier
		}
		joined = append(joined, w)
		t.Sites = append(t.Sites, s)
	}
	for _, i := range byName {
		if siteAt[i] < 0 {
			t.Dropped++
		}
	}
	// result remaps a TypeResult to its site's index in t.Sites; typ also
	// checks a TypeUnion's index. A union is remapped in place, keeping its
	// index, and a union member that is itself a union is dropped.
	result := func(x wire.Type) wire.Type {
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
	if len(wl.Unions) > 0 {
		t.Unions = make([][]wire.Type, len(wl.Unions))
		for i, u := range wl.Unions {
			t.Unions[i] = make([]wire.Type, 0, len(u))
			for _, x := range u {
				if x.Kind == wire.TypeUnion {
					t.Dropped++
					continue
				}
				if x = result(x); x.Kind != wire.TypeUnknown {
					t.Unions[i] = append(t.Unions[i], x)
				}
			}
		}
	}
	typ := func(x wire.Type) wire.Type {
		if x.Kind == wire.TypeUnion {
			if uint64(x.Index) < uint64(len(t.Unions)) {
				return x
			}
			t.Dropped++
			return wire.Type{}
		}
		return result(x)
	}
	for i, w := range joined {
		if w < 0 {
			continue
		}
		ws := &wl.Sites[w]
		t.Sites[i].Receiver = typ(ws.Receiver)
		t.Sites[i].Args, t.Sites[i].Arguments = b.linkArgs(ws, typ, &t.Dropped)
	}
	byStart := b.declsByStart()
	for i := range wl.Computed {
		ws := &wl.Computed[i]
		rng, err := b.rangeOf(ws.Name.Start, ws.Name.End)
		if err != nil || ws.Shape != wire.ShapeIndirect {
			t.Dropped++
			continue
		}
		s := link.Site{Caller: b.enclosing(byStart, ws.Name), Range: *rng, NameStart: ws.Name.Start, NameEnd: ws.Name.End,
			Callee: bound(string(b.src[ws.Name.Start:ws.Name.End]), model.MaxNameBytes), Shape: wire.ShapeIndirect,
			Described: true, Computed: true}
		s.Handler = handlerOf(wire.Ref{Start: ws.Name.Start, End: ws.Name.End, Scope: int(s.Caller)})
		s.Args, s.Arguments = b.linkArgs(ws, typ, &t.Dropped)
		t.Sites = append(t.Sites, s)
	}

	t.Decls = make([]link.Decl, len(b.decls))
	for i := range b.decls {
		d := &b.decls[i]
		t.Decls[i] = link.Decl{Endpoint: b.declEndpoint(i), Parent: int32(d.Parent), Exported: d.Exported,
			Prototype: d.Prototype, Macro: d.Macro, MacroInput: d.MacroInput, MacroLocal: d.MacroLocal,
			MacroTranscribes: d.MacroTranscribes}
	}
	for _, x := range wl.Exports {
		i, ok := b.nameAt[x.Name]
		if !ok || t.Decls[i].Described {
			t.Dropped++
			continue
		}
		d := &t.Decls[i]
		d.Described, d.Receiver, d.Signature, d.Params, d.Optional, d.Variadic = true, x.Receiver, x.Signature, x.Params, x.Optional, x.Variadic
		d.Static, d.Abstract, d.Virtual, d.Final, d.Internal = x.Static, x.Abstract, x.Virtual, x.Final, x.Internal
		d.Returns = typ(x.Returns)
		d.Override, d.Alias, d.Property, d.Trait = x.Override, x.Alias, x.Property, x.Trait
		d.Exported = d.Exported || x.Exported
	}
	for _, p := range wl.Parameters {
		i, ok := b.nameAt[p.Callable]
		if !ok {
			t.Dropped++
			continue
		}
		rng, err := b.rangeOf(p.Name.Start, p.Name.End)
		if err != nil {
			t.Dropped++
			continue
		}
		t.Decls[i].Parameters = append(t.Decls[i].Parameters, link.Parameter{
			Endpoint: link.Endpoint{Kind: model.NodeVariable, Name: bound(string(b.src[p.Name.Start:p.Name.End]), model.MaxNameBytes), Range: *rng},
			Index:    p.Index, Variadic: p.Variadic, KeywordOnly: p.KeywordOnly})
	}
	t.Reexports = wl.Reexports
	for _, x := range wl.DynamicImports {
		rng, err := b.rangeOf(x.At.Start, x.At.End)
		if err != nil {
			t.Dropped++
			continue
		}
		scope := b.enclosing(byStart, x.At)
		if !x.Constant {
			t.DynamicImports = append(t.DynamicImports, link.DynamicImport{Caller: scope, Range: *rng,
				Fragments: append([]string{}, x.Fragments...)})
			continue
		}
		if len(x.Fragments) != 1 || x.Fragments[0] == "" || len(x.Fragments[0]) > model.MaxQualifiedNameBytes {
			t.Dropped++
			continue
		}
		t.Imports = append(t.Imports, link.Import{Path: x.Fragments[0], Scope: scope, Range: *rng})
	}

	if len(wl.Values) > 0 {
		t.Values = make([]link.Value, 0, len(wl.Values))
		for _, v := range wl.Values {
			rng, err := b.rangeOf(v.At.Start, v.At.End)
			if err != nil {
				t.Dropped++
				continue
			}
			lv := link.Value{Caller: b.enclosing(byStart, v.At), Range: *rng, Field: v.Field, Type: typ(v.Type)}
			if v.Declares != (wire.Span{}) {
				// The value is the declaration its expression declares, or,
				// when that joined nothing, no value to resolve by name.
				i, ok := b.nameAt[v.Declares]
				if !ok {
					t.Dropped++
					continue
				}
				lv.Declares = int32(i) + 1
			}
			t.Values = append(t.Values, lv)
		}
	}
	b.table = t
	return nil
}

// linkArgs is a worker site's argument types, remapped by typ, and its
// arguments: each argument's range, keyword and spread, and its reads as
// variable endpoints carrying the identifier's range, which tableVariables
// completes. Arguments that are not one per argument type, or one whose
// range does not fit, leave the site without arguments and count once in
// dropped; a read whose range does not fit is left out and counted.
func (b *builder) linkArgs(ws *wire.Site, typ func(wire.Type) wire.Type, dropped *int) ([]wire.Type, []link.Argument) {
	var args []wire.Type
	if len(ws.Args) > 0 {
		args = make([]wire.Type, len(ws.Args))
		for j, a := range ws.Args {
			args[j] = typ(a)
		}
	}
	if len(ws.Arguments) == 0 {
		return args, nil
	}
	if len(ws.Arguments) != len(ws.Args) {
		*dropped++
		return args, nil
	}
	out := make([]link.Argument, len(ws.Arguments))
	for j, a := range ws.Arguments {
		rng, err := b.rangeOf(a.At.Start, a.At.End)
		if err != nil {
			*dropped++
			return args, nil
		}
		out[j] = link.Argument{Range: *rng, Keyword: a.Keyword, Spread: a.Spread}
		for _, r := range a.Reads {
			rr, err := b.rangeOf(r.Start, r.End)
			if err != nil || r.End <= r.Start {
				*dropped++
				continue
			}
			out[j].Reads = append(out[j].Reads, link.Endpoint{Kind: model.NodeVariable, Range: *rr})
		}
	}
	return args, out
}

// declEndpoint is declaration i's link endpoint.
func (b *builder) declEndpoint(i int) link.Endpoint {
	d := &b.decls[i]
	return link.Endpoint{Node: d.res.Node.ID, CanonicalKey: d.res.CanonicalKey, Kind: d.kind, Name: d.Name,
		Qualified: d.Qualified, Range: *d.rng, NameRange: d.nameRng}
}

// linkImports is the names the file's import records introduce, each with
// the path it denotes, the declaration it is written in and the record's
// range (link.Import). The worker's record is one statement:
// its path as written and the local names it binds. A Rust use tree is
// split into one path per leaf (lang.RustUse). An import of every member of a path (a Go
// dot import, a Java on-demand import `a.b.*`, a C++ using-directive, a Rust
// glob, a Python `from m import *`) is named link.ImportAll, which the
// statement's own bytes tell; an import binding no name (an include, an
// ECMAScript side-effect import) has an empty name.
func (b *builder) linkImports() ([]link.Import, error) {
	var out []link.Import
	byStart := b.declsByStart()
	for _, imp := range b.ex.imports {
		rng, err := b.rangeOf(imp.Start, imp.End)
		if err != nil {
			return nil, err
		}
		text := string(b.src[imp.Start:imp.End])
		names := imp.Names
		scope := b.enclosing(byStart, wire.Span{Start: imp.Start, End: imp.End})
		switch b.lang.Name {
		case "rust":
			// A glob's name is lang.RustGlob, which is link.ImportAll.
			lang.RustUse(imp.Path, func(name, p string) {
				out = append(out, link.Import{Name: name, Path: p, Scope: scope, Range: *rng})
			})
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
			out = append(out, link.Import{Path: imp.Path, Scope: scope, Range: *rng})
		}
		// A Python `import a.b as c` binds c to the module a.b itself, which
		// `from a.b import c` never does: its name denotes the whole module.
		wholeModule := b.lang.Name == "python" && strings.HasPrefix(strings.TrimSpace(text), "import")
		for i, n := range names {
			li := link.Import{Name: n, Path: imp.Path, Scope: scope, Range: *rng}
			switch {
			case wholeModule && n != link.ImportAll:
				li.Original = "*"
			case len(imp.Originals) == len(names):
				li.Original = imp.Originals[i]
			}
			out = append(out, li)
		}
	}
	return out, nil
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
// link.TableKind, when the builder joined one, and its link.MemberDigest
// beside it under link.DigestKind, which a linking unit's delta reads in
// place of the table.
func (b *builder) storeTable(sink provider.StateSink) error {
	if b.table == nil {
		return nil
	}
	payload, err := b.table.Encode()
	if err != nil {
		return &model.Error{Code: model.CodeInternal, Message: "link table of " + bound(b.fv.Path, 256) + ": " + err.Error()}
	}
	digest, err := link.MemberDigest(b.table)
	if err != nil {
		return &model.Error{Code: model.CodeInternal, Message: "link table digest of " + bound(b.fv.Path, 256) + ": " + err.Error()}
	}
	b.table = nil
	if err := sink.PutState(b.ctx, link.TableKind, payload); err != nil {
		return err
	}
	return sink.PutState(b.ctx, link.DigestKind, []byte(digest))
}
