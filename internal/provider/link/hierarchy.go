package link

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path"
	"slices"
	"sort"
	"strings"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/lang"
	"github.com/Sawmonabo/codectx/internal/scratch"
)

// hierarchy is one unit's class hierarchy: every type declaration, the
// subtype relation its tables declare (wire.Supertype) and, for Go, the
// interfaces each type's method set satisfies. It answers class-hierarchy
// analysis for every resolver.
//
// Nothing of it is resident. It is two sorted, block-indexed files in the
// unit's scratch directory: decls, written from one pass over the tables (the
// files, the type declarations and the free functions, each under the keys a
// name as written can reach it by), and links, written from a second pass
// that resolves each method's receiver, each declared supertype and Go's
// method-set satisfaction against decls (the methods of each type, its
// subtypes and its supertypes). Every lookup reads one block range of one
// file; only the first key of each block is held in memory.
type hierarchy struct {
	decls *index
	links *index
}

// The keys of the two indexes. A family groups the languages whose files
// name each other's declarations (familyOf). A refKey is a declaration's Ref
// in eight bytes, so a type's methods, subtypes and supertypes sort together.
const (
	// keyFile + refKey(file, 0): the file's language (Aux) and its scopes
	// (Sig, joined by "\n"), the context a name written in it is read in.
	keyFile = "f\x00"
	// keyType + family + scope + name: a type declaration visible under its
	// simple name in that scope. C and C++ types carry their namespace in
	// Aux under the empty scope instead.
	keyType = "t\x00"
	// keyTypeQualified + family + a qualified suffix of two or more
	// components: a type declaration a qualified name reaches.
	keyTypeQualified = "tq\x00"
	// keyTypeName + family + name: every type declaration of that simple
	// name in the unit, read only when nothing in scope declares it.
	keyTypeName = "tn\x00"
	// keyFunc + family + scope + name: a free function visible under its
	// simple name in that scope (Go's package, C's and C++'s one linkage
	// scope with the namespace in Aux, Rust's file), its signature in Sig.
	keyFunc = "u\x00"
	// keyFuncQualified + family + a qualified suffix of two or more
	// components: a free function a qualified name reaches (`pkg.F`,
	// `ns::f`, an in-file Rust `m::f`).
	keyFuncQualified = "uq\x00"
	// keyFuncModule + a Rust file module's name + name: a free function a
	// `module::f` path reaches.
	keyFuncModule = "um\x00"
	// keyMethod + refKey(type) + name: a method or constructor the type
	// declares, its signature in Sig.
	keyMethod = "m\x00"
	// keySub + refKey(type): a declared or satisfied direct subtype.
	keySub = "d\x00"
	// keySuper + refKey(type): a declared or satisfied direct supertype.
	keySuper = "p\x00"
)

// The flags of an entry.
const (
	flagAbstract byte = 1 << iota
	flagStatic
	flagHeader
)

// typeKinds are the declaration kinds a type name denotes; a Rust trait is
// an interface.
var typeKinds = []model.NodeKind{model.NodeClass, model.NodeInterface, model.NodeStruct, model.NodeEnum}

func isType(k model.NodeKind) bool { return slices.Contains(typeKinds, k) }

func isCallable(k model.NodeKind) bool { return k == model.NodeFunction || k == model.NodeMethod }

// familyOf groups the languages whose files name each other's declarations:
// a header is C or C++ by the repository's census, and the ECMAScript
// dialects import one another.
func familyOf(language string) string {
	switch language {
	case "c", "cpp":
		return "c"
	case "javascript", "typescript", "tsx":
		return "js"
	}
	return language
}

// separatorOf is the language's qualified-name separator.
func separatorOf(language string) string {
	if l, ok := lang.Lookup(language); ok {
		return l.Separator
	}
	return "."
}

// fileCtx is the context a name written in one member file is read in.
type fileCtx struct {
	language, family, sep string
	// scopes are the scopes a simple name is visible in: Go's directory
	// and package, Java's package, Rust's and every other language's file,
	// and for C and C++ the namespaces the file declares in, "" first.
	scopes []string
}

// contextOf is t's fileCtx.
func contextOf(t *Table) fileCtx {
	fc := fileCtx{language: t.Language, family: familyOf(t.Language), sep: separatorOf(t.Language)}
	switch fc.family {
	case "go":
		pkg := ""
		for _, d := range t.Decls {
			if p, _, ok := strings.Cut(d.Qualified, "."); ok && d.Qualified != d.Name {
				pkg = p
				break
			}
		}
		fc.scopes = []string{path.Dir(t.Path) + "\x00" + pkg}
	case "java":
		pkg := ""
		for i := range t.Decls {
			if t.Decls[i].Parent < 0 {
				pkg = namespaceOf(&t.Decls[i], ".")
				break
			}
		}
		fc.scopes = []string{pkg}
	case "c":
		fc.scopes = []string{""}
		for i := range t.Decls {
			if ns := namespaceOf(&t.Decls[i], fc.sep); ns != "" && !slices.Contains(fc.scopes, ns) {
				fc.scopes = append(fc.scopes, ns)
			}
		}
	default:
		fc.scopes = []string{t.Path}
	}
	return fc
}

// namespaceOf is what d's qualified name prefixes its own name with, or ""
// when the qualified name does not end in it (a cut qualified name is no
// scope).
func namespaceOf(d *Decl, sep string) string {
	if ns, ok := strings.CutSuffix(d.Qualified, sep+d.Name); ok {
		return ns
	}
	return ""
}

// declScope is the scope d is visible in under its simple name, by t's
// language: Go's directory and package, Java's package (a nested type's is
// its outermost type's), C's and C++'s one linkage scope, and the file for
// every other language.
func declScope(t *Table, fc fileCtx) string {
	switch fc.family {
	case "go", "java":
		return fc.scopes[0]
	case "c":
		return ""
	}
	return t.Path
}

// visible reports that a C or C++ declaration in namespace ns is visible
// from code in namespace from: ns encloses it.
func visible(ns, from string) bool {
	return ns == "" || ns == from || strings.HasPrefix(from, ns+"::")
}

// rustModule is the module name a Rust file's path gives it: its stem, its
// directory for a mod.rs, and "crate" for a crate root.
func rustModule(p string) string {
	switch stem := strings.TrimSuffix(path.Base(p), ".rs"); stem {
	case "lib", "main":
		return "crate"
	case "mod":
		return path.Base(path.Dir(p))
	default:
		return stem
	}
}

// isHeader reports a path its language's grammar declares a header.
func isHeader(language, p string) bool {
	l, ok := lang.Lookup(language)
	return ok && slices.Contains(l.Headers, strings.ToLower(path.Ext(p)))
}

// qualifiedSuffixes yields every suffix of q of two or more components.
func qualifiedSuffixes(q, sep string, yield func(string)) {
	for {
		_, rest, ok := strings.Cut(q, sep)
		if !ok {
			return
		}
		yield(q)
		q = rest
	}
}

// typeName is a type as written reduced to the declaration it names: no
// pointer, reference, slice, mutability or trait-object marker, no
// arguments of a generic.
func typeName(written string) string {
	s := strings.TrimSpace(written)
	for {
		trimmed := strings.TrimLeft(s, "*&[] ")
		for _, p := range []string{"mut ", "dyn ", "impl ", "const ", "struct ", "class ", "enum ", "union ", "typename "} {
			trimmed = strings.TrimPrefix(trimmed, p)
		}
		if trimmed == s {
			break
		}
		s = trimmed
	}
	if i := strings.IndexAny(s, "<[("); i >= 0 {
		s = s[:i]
	}
	return strings.TrimRight(s, "*& ")
}

func refKey(r Ref) string {
	var b [8]byte
	binary.BigEndian.PutUint32(b[:4], uint32(r.File))
	binary.BigEndian.PutUint32(b[4:], uint32(r.Index))
	return string(b[:])
}

// buildHierarchy reads the unit's tables into its hierarchy.
func buildHierarchy(ctx context.Context, u *unit) (*hierarchy, error) {
	h := &hierarchy{}
	ok := false
	defer func() {
		if !ok {
			_ = h.close()
		}
	}()
	decls, err := newEntrySort(u.dir)
	if err != nil {
		return nil, err
	}
	defer decls.Close()
	err = u.Files(ctx, func(file int32, _ model.FileVersion, t *Table) error {
		return addDecls(decls, file, t)
	})
	if err != nil {
		return nil, err
	}
	if h.decls, err = writeIndex(u.dir, decls); err != nil {
		return nil, err
	}

	links, err := newEntrySort(u.dir)
	if err != nil {
		return nil, err
	}
	defer links.Close()
	// methodSets pairs each Go interface's abstract methods with each Go
	// type's methods under one name-and-signature key, interfaces first.
	methodSets, err := newEntrySort(u.dir)
	if err != nil {
		return nil, err
	}
	defer methodSets.Close()
	err = u.Files(ctx, func(file int32, _ model.FileVersion, t *Table) error {
		return h.addLinks(ctx, links, methodSets, file, t)
	})
	if err != nil {
		return nil, err
	}
	if err := h.satisfy(u.dir, links, methodSets); err != nil {
		return nil, err
	}
	if h.links, err = writeIndex(u.dir, links); err != nil {
		return nil, err
	}
	ok = true
	return h, nil
}

// addDecls adds t's file context, type declarations and free functions.
func addDecls(s *pagination.ExternalSort[entry], file int32, t *Table) error {
	fc := contextOf(t)
	if err := s.Add(entry{Key: keyFile + refKey(Ref{File: file}), Aux: t.Language, Sig: strings.Join(fc.scopes, "\n")}); err != nil {
		return err
	}
	header := isHeader(t.Language, t.Path)
	for i := range t.Decls {
		d := &t.Decls[i]
		ref := Ref{File: file, Index: int32(i)}
		ns := ""
		if fc.family == "c" {
			ns = namespaceOf(d, fc.sep)
		}
		scope := declScope(t, fc)
		var keys []string
		var prefix, qualifiedPrefix string
		switch {
		case isType(d.Kind):
			prefix, qualifiedPrefix = keyType, keyTypeQualified
			keys = append(keys, keyTypeName+fc.family+"\x00"+d.Name)
		case isCallable(d.Kind) && d.Receiver == "" && (d.Parent < 0 || !isType(t.Decls[d.Parent].Kind)):
			prefix, qualifiedPrefix = keyFunc, keyFuncQualified
			if fc.family == "rust" && d.Parent < 0 {
				keys = append(keys, keyFuncModule+rustModule(t.Path)+"\x00"+d.Name)
			}
		default:
			continue
		}
		keys = append(keys, prefix+fc.family+"\x00"+scope+"\x00"+d.Name)
		qualifiedSuffixes(d.Qualified, fc.sep, func(q string) { keys = append(keys, qualifiedPrefix+fc.family+"\x00"+q) })
		e := entry{Ref: ref, Aux: ns, Sig: d.Signature, Flags: flagsOf(d, header)}
		for _, k := range keys {
			e.Key = k
			if err := s.Add(e); err != nil {
				return err
			}
		}
	}
	return nil
}

func flagsOf(d *Decl, header bool) byte {
	var f byte
	if d.Abstract {
		f |= flagAbstract
	}
	if d.Static {
		f |= flagStatic
	}
	if header {
		f |= flagHeader
	}
	return f
}

// addLinks adds t's methods under the types that declare them, its declared
// supertypes in both directions, and, for Go, its method-set entries.
func (h *hierarchy) addLinks(ctx context.Context, links, methodSets *pagination.ExternalSort[entry], file int32, t *Table) error {
	fc := contextOf(t)
	header := isHeader(t.Language, t.Path)
	for i := range t.Decls {
		d := &t.Decls[i]
		if !isCallable(d.Kind) {
			continue
		}
		ref := Ref{File: file, Index: int32(i)}
		e := entry{Ref: ref, Sig: d.Signature, Flags: flagsOf(d, header)}
		add := func(typ Ref) error {
			e.Key = keyMethod + refKey(typ) + d.Name
			if err := links.Add(e); err != nil {
				return err
			}
			if fc.family != "go" {
				return nil
			}
			// Go declares a method without a body only in an interface.
			m := entry{Key: d.Name + "\x00" + d.Signature, Ref: typ}
			if !d.Abstract {
				m.Tag = 1
			}
			return methodSets.Add(m)
		}
		if p := d.Parent; p >= 0 && isType(t.Decls[p].Kind) {
			if err := add(Ref{File: file, Index: p}); err != nil {
				return err
			}
			continue
		}
		if d.Receiver == "" {
			continue
		}
		err := h.typesIn(ctx, fc, d.Receiver, func(te entry) error { return add(te.Ref) })
		if err != nil {
			return err
		}
	}
	for _, st := range t.Supertypes {
		err := h.typesIn(ctx, fc, st.Type, func(sub entry) error {
			return h.typesIn(ctx, fc, st.Super, func(super entry) error {
				return addSubtype(links, sub.Ref, super.Ref)
			})
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func addSubtype(links *pagination.ExternalSort[entry], sub, super Ref) error {
	if sub == super {
		return nil
	}
	if err := links.Add(entry{Key: keySub + refKey(super), Ref: sub}); err != nil {
		return err
	}
	return links.Add(entry{Key: keySuper + refKey(sub), Ref: super})
}

// satisfy derives Go's implicit interface satisfaction: a type satisfies an
// interface when its methods include every method the interface declares,
// by name and signature. methodSets holds, under each name-and-signature
// key, the interfaces declaring it (tag 0) before the types declaring it
// (tag 1). Each interface's method keys are sorted under its own refKey,
// ahead of its pairs with types under refKey(interface) + refKey(type); a
// pair that matches as many distinct keys as the interface has is a
// subtype.
func (h *hierarchy) satisfy(dir string, links, methodSets *pagination.ExternalSort[entry]) error {
	run, err := methodSets.Sorted()
	if err != nil {
		return err
	}
	defer run.Close()
	pairs, err := newEntrySort(dir)
	if err != nil {
		return err
	}
	defer pairs.Close()
	var key string
	// ifaces are the interfaces declaring the current key's method: what
	// one name and signature pairs with, never more than the key's own.
	var ifaces []Ref
	err = run.Each(func(e entry) error {
		if e.Key != key {
			key, ifaces = e.Key, ifaces[:0]
		}
		if e.Tag == 0 {
			ifaces = append(ifaces, e.Ref)
			return pairs.Add(entry{Key: refKey(e.Ref), Sig: e.Key})
		}
		for _, i := range ifaces {
			if err := pairs.Add(entry{Key: refKey(i) + refKey(e.Ref), Sig: e.Key}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	counted, err := pairs.Sorted()
	if err != nil {
		return err
	}
	defer counted.Close()
	var iface, pair, method string
	want, n := 0, 0
	return counted.Each(func(e entry) error {
		if len(e.Key) == len(refKey(Ref{})) {
			if e.Key != iface {
				iface, want, method = e.Key, 0, ""
			}
			if e.Sig != method {
				method = e.Sig
				want++
			}
			return nil
		}
		if e.Key != pair {
			pair, n, method = e.Key, 0, ""
		}
		if e.Sig == method {
			return nil
		}
		method = e.Sig
		if n++; pair[:8] != iface || n != want {
			return nil
		}
		return addSubtype(links, refOf(pair[8:]), refOf(pair[:8]))
	})
}

func refOf(k string) Ref {
	return Ref{File: int32(binary.BigEndian.Uint32([]byte(k[:4]))), Index: int32(binary.BigEndian.Uint32([]byte(k[4:8])))}
}

// types yields the type declarations a type name as written in a table of
// the member file may denote, each once.
func (h *hierarchy) types(ctx context.Context, file int32, name string, yield func(Ref) error) error {
	var fc fileCtx
	found := false
	err := h.decls.lookup(keyFile+refKey(Ref{File: file}), func(e entry) error {
		fc = fileCtx{language: e.Aux, family: familyOf(e.Aux), sep: separatorOf(e.Aux), scopes: strings.Split(e.Sig, "\n")}
		found = true
		return nil
	})
	if err != nil || !found {
		return err
	}
	return h.typesIn(ctx, fc, name, func(e entry) error { return yield(e.Ref) })
}

// typesIn yields the type declarations name, as written in a file of
// context fc, denotes: a qualified name the declarations whose qualified
// name ends in it; a simple name the declarations of that name in one of
// the file's scopes, and when none, every type of that name in the unit.
func (h *hierarchy) typesIn(ctx context.Context, fc fileCtx, written string, yield func(entry) error) error {
	name := typeName(written)
	if name == "" {
		return nil
	}
	if strings.Contains(name, fc.sep) {
		return h.decls.lookup(keyTypeQualified+fc.family+"\x00"+name, yield)
	}
	found := false
	hit := func(e entry) error {
		found = true
		return yield(e)
	}
	if fc.family == "c" {
		err := h.decls.lookup(keyType+fc.family+"\x00\x00"+name, func(e entry) error {
			if slices.ContainsFunc(fc.scopes, func(from string) bool { return visible(e.Aux, from) }) {
				return hit(e)
			}
			return nil
		})
		if err != nil {
			return err
		}
	} else {
		for _, s := range fc.scopes {
			if err := h.decls.lookup(keyType+fc.family+"\x00"+s+"\x00"+name, hit); err != nil {
				return err
			}
		}
	}
	if found {
		return nil
	}
	return h.decls.lookup(keyTypeName+fc.family+"\x00"+name, yield)
}

// methods yields the methods typ itself declares named method. A C or C++
// type's declaration in a header is not yielded when a translation unit
// defines the same method of it: that is the definition the declaration
// announces.
func (h *hierarchy) methods(typ Ref, method string, yield func(entry) error) error {
	return preferDefinitions(func(f func(entry) error) error {
		return h.links.lookup(keyMethod+refKey(typ)+method, f)
	}, yield)
}

// preferDefinitions yields what each yields, without its header entries
// when it yields any entry outside a header. Only the entries of one name in
// one scope are held.
func preferDefinitions(each func(func(entry) error) error, yield func(entry) error) error {
	var headers, sources []entry
	err := each(func(e entry) error {
		if e.Flags&flagHeader != 0 {
			headers = append(headers, e)
		} else {
			sources = append(sources, e)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if len(sources) == 0 {
		sources = headers
	}
	for _, e := range sources {
		if err := yield(e); err != nil {
			return err
		}
	}
	return nil
}

// declared yields the declarations of method a call on typ reaches without
// dispatch: typ's own, else the nearest along each supertype path, each
// once.
func (h *hierarchy) declared(typ Ref, method string, yield func(entry) error) error {
	seen := map[Ref]bool{typ: true}
	queue := []Ref{typ}
	done := map[Ref]bool{}
	for len(queue) > 0 {
		t := queue[0]
		queue = queue[1:]
		found := false
		err := h.methods(t, method, func(e entry) error {
			found = true
			if done[e.Ref] {
				return nil
			}
			done[e.Ref] = true
			return yield(e)
		})
		if err != nil {
			return err
		}
		if found {
			continue
		}
		err = h.links.lookup(keySuper+refKey(t), func(e entry) error {
			if !seen[e.Ref] {
				seen[e.Ref] = true
				queue = append(queue, e.Ref)
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// dispatch yields every method declaration a call of method on a receiver
// whose static type is the declaration typ may reach: the method typ
// declares or inherits, and every override in a subtype of typ, each once.
// A declaration without a body is yielded only when no declaration with one
// is reachable, since a call runs a body.
func (h *hierarchy) dispatch(ctx context.Context, typ Ref, method string, yield func(Ref) error) error {
	return h.dispatchEntries(ctx, typ, method, func(e entry) error { return yield(e.Ref) })
}

func (h *hierarchy) dispatchEntries(ctx context.Context, typ Ref, method string, yield func(entry) error) error {
	var concrete, abstract []entry
	collect := func(e entry) error {
		if e.Flags&flagAbstract != 0 {
			abstract = append(abstract, e)
		} else {
			concrete = append(concrete, e)
		}
		return nil
	}
	if err := h.declared(typ, method, collect); err != nil {
		return err
	}
	seen := map[Ref]bool{typ: true}
	queue := []Ref{typ}
	for len(queue) > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		t := queue[0]
		queue = queue[1:]
		err := h.links.lookup(keySub+refKey(t), func(e entry) error {
			if seen[e.Ref] {
				return nil
			}
			seen[e.Ref] = true
			queue = append(queue, e.Ref)
			return h.methods(e.Ref, method, collect)
		})
		if err != nil {
			return err
		}
	}
	out := concrete
	if len(out) == 0 {
		out = abstract
	}
	yielded := map[Ref]bool{}
	for _, e := range out {
		if yielded[e.Ref] {
			continue
		}
		yielded[e.Ref] = true
		if err := yield(e); err != nil {
			return err
		}
	}
	return nil
}

// close releases what the hierarchy spilled.
func (h *hierarchy) close() error {
	h.decls.close()
	h.links.close()
	return nil
}

// entry is one record of a hierarchy index, or of a sort the resolvers join
// over: a key, the declaration (or site) it names, and the facts a lookup
// filters or answers on without reading that declaration's table.
type entry struct {
	Key string
	// Tag orders entries of one key: in a join, the side read first.
	Tag   byte
	Ref   Ref
	Aux   string
	Sig   string
	Flags byte
}

func compareEntry(a, b entry) int {
	if c := strings.Compare(a.Key, b.Key); c != 0 {
		return c
	}
	if a.Tag != b.Tag {
		return int(a.Tag) - int(b.Tag)
	}
	if a.Ref.File != b.Ref.File {
		return int(a.Ref.File) - int(b.Ref.File)
	}
	if a.Ref.Index != b.Ref.Index {
		return int(a.Ref.Index) - int(b.Ref.Index)
	}
	if c := strings.Compare(a.Aux, b.Aux); c != 0 {
		return c
	}
	if c := strings.Compare(a.Sig, b.Sig); c != 0 {
		return c
	}
	return int(a.Flags) - int(b.Flags)
}

func encodeEntry(e entry) ([]byte, error) {
	b := make([]byte, 0, len(e.Key)+len(e.Aux)+len(e.Sig)+3*binary.MaxVarintLen32+10)
	b = binary.AppendUvarint(b, uint64(len(e.Key)))
	b = append(b, e.Key...)
	b = append(b, e.Tag)
	b = binary.BigEndian.AppendUint32(b, uint32(e.Ref.File))
	b = binary.BigEndian.AppendUint32(b, uint32(e.Ref.Index))
	b = binary.AppendUvarint(b, uint64(len(e.Aux)))
	b = append(b, e.Aux...)
	b = binary.AppendUvarint(b, uint64(len(e.Sig)))
	b = append(b, e.Sig...)
	return append(b, e.Flags), nil
}

var errMalformedEntry = errors.New("link: malformed hierarchy entry")

func decodeEntry(b []byte) (entry, error) {
	var e entry
	str := func() (string, bool) {
		n, w := binary.Uvarint(b)
		if w <= 0 || uint64(len(b)-w) < n {
			return "", false
		}
		s := string(b[w : w+int(n)])
		b = b[w+int(n):]
		return s, true
	}
	var ok bool
	if e.Key, ok = str(); !ok || len(b) < 9 {
		return e, errMalformedEntry
	}
	e.Tag = b[0]
	e.Ref = Ref{File: int32(binary.BigEndian.Uint32(b[1:5])), Index: int32(binary.BigEndian.Uint32(b[5:9]))}
	b = b[9:]
	if e.Aux, ok = str(); !ok {
		return e, errMalformedEntry
	}
	if e.Sig, ok = str(); !ok || len(b) != 1 {
		return e, errMalformedEntry
	}
	e.Flags = b[0]
	return e, nil
}

func newEntrySort(dir string) (*pagination.ExternalSort[entry], error) {
	return pagination.NewExternalSort(dir, 0, encodeEntry, decodeEntry, compareEntry)
}

// indexBlockBytes is the span of an index file one in-memory key locates:
// the read a lookup makes beyond its own records.
const indexBlockBytes = 32 << 10

// index is a sorted entry sequence written to one scratch file, located by
// the first key of each block.
type index struct {
	lease *scratch.Lease
	f     *os.File
	// size is the file's logical length: the pooled surface is never
	// truncated, and the bytes past size are a previous tenant's.
	size  int64
	first []string
	off   []int64
}

// writeIndex writes s's sorted entries, each distinct one once, to a file
// taken from dir's scratch arena.
func writeIndex(dir string, s *pagination.ExternalSort[entry]) (*index, error) {
	run, err := s.Sorted()
	if err != nil {
		return nil, err
	}
	defer run.Close()
	lease, f, err := scratch.For(dir).TakeFile(scratch.SortRun)
	if err != nil {
		return nil, err
	}
	ix := &index{lease: lease, f: f}
	w := bufio.NewWriter(f)
	var prev entry
	have := false
	block := int64(-indexBlockBytes)
	err = run.Each(func(e entry) error {
		if have && compareEntry(prev, e) == 0 {
			return nil
		}
		prev, have = e, true
		b, _ := encodeEntry(e)
		if ix.size-block >= indexBlockBytes {
			block = ix.size
			ix.first = append(ix.first, e.Key)
			ix.off = append(ix.off, ix.size)
		}
		n, err := w.Write(binary.AppendUvarint(nil, uint64(len(b))))
		if err != nil {
			return err
		}
		m, err := w.Write(b)
		ix.size += int64(n + m)
		return err
	})
	if err == nil {
		err = w.Flush()
	}
	if err != nil {
		ix.close()
		return nil, err
	}
	return ix, nil
}

// lookup yields every entry whose key is key, in order.
func (ix *index) lookup(key string, yield func(entry) error) error {
	if ix == nil || len(ix.first) == 0 {
		return nil
	}
	// Every block before the one preceding the first block whose first key
	// is at least key ends below key.
	i := max(sort.SearchStrings(ix.first, key)-1, 0)
	r := bufio.NewReader(io.NewSectionReader(ix.f, ix.off[i], ix.size-ix.off[i]))
	var buf []byte
	for {
		n, err := binary.ReadUvarint(r)
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		buf = slices.Grow(buf[:0], int(n))[:n]
		if _, err := io.ReadFull(r, buf); err != nil {
			return err
		}
		e, err := decodeEntry(buf)
		if err != nil {
			return err
		}
		switch c := strings.Compare(e.Key, key); {
		case c > 0:
			return nil
		case c == 0:
			if err := yield(e); err != nil {
				return err
			}
		}
	}
}

func (ix *index) close() {
	if ix != nil && ix.lease != nil {
		ix.lease.Release()
		ix.lease = nil
	}
}
