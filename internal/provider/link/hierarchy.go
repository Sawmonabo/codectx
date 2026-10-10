package link

import (
	"bufio"
	"cmp"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/lang"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/wire"
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
	// goModule is a Go unit's module path, read from the go.mod at its
	// project root goRoot ("." for the repository root); empty when there
	// is none, and then no Go import path names a package of the unit.
	goModule, goRoot string
}

// The keys of the two indexes. A family groups the languages whose files
// name each other's declarations (familyOf). A refKey is a declaration's Ref
// in eight bytes, so a type's methods, subtypes and supertypes sort together.
const (
	// keyFile + refKey(file, 0): the file's language and path (Aux, joined
	// by a NUL) and its scopes (Sig, joined by "\n"), the context a name
	// written in it is read in.
	keyFile = "f\x00"
	// keyImport + refKey(file, 0) + name: the declaration the import is
	// written in (Ref.Index, -1 the module), the path a name the file's
	// imports introduce denotes (Aux), ImportAll for an import of every member, and
	// the name it denotes in that path's module when the import renames it
	// (Sig, Import.Original).
	keyImport = "i\x00"
	// keyGoDir + directory: a Go package declared in that directory, its
	// name in Aux, which an import path ending in the directory reaches.
	keyGoDir = "g\x00"
	// keyPath + a suffix of a member file's path at a component boundary:
	// that file, which a C or C++ include of the suffix names.
	keyPath = "x\x00"
	// keyType + family + scope + name: a type declaration visible under its
	// simple name in that scope. C and C++ types carry their namespace in
	// Aux under the empty scope instead.
	keyType = "t\x00"
	// keyTypeQualified + family + a qualified suffix of two or more
	// components: a type declaration a qualified name reaches.
	keyTypeQualified = "tq\x00"
	// keyTypeName + family + name: every type declaration of that simple
	// name in the unit, read only for a name no scope or import of the file
	// can supply (typesIn).
	keyTypeName = "tn\x00"
	// keyTypeModule + a Rust module (rustModuleKey) + name: a type that module
	// declares, which a `use` or a `module::T` path reaches.
	keyTypeModule = "tm\x00"
	// keyExport + a module path without extension + name: what an
	// ECMAScript module exports itself under that name (its own declaration,
	// under its name or one an `export` statement gives it) or a Python
	// module declares, which an import of that module reaches (moduleBases).
	// Aux is its kind (exportType, exportFunc, exportModule); a module's Sig
	// is that module's path.
	keyExport = "te\x00"
	// keyReexport + a module path without extension + name: an ECMAScript
	// module's re-export of the name Sig of the module whose path is Aux,
	// name "*" for `export * from`.
	keyReexport = "tr\x00"
	// keyFunc + family + scope + name: a free function visible under its
	// simple name in that scope (Go's package, C's and C++'s one external
	// linkage scope with the namespace in Aux, Rust's file), its signature
	// in Sig.
	keyFunc = "u\x00"
	// keyFuncQualified + family + a qualified suffix of two or more
	// components: a free function a qualified name reaches (`pkg.F`,
	// `ns::f`).
	keyFuncQualified = "uq\x00"
	// keyFuncModule + a Rust module (rustModuleKey) + name: a free function
	// that module declares, which a `use` or a `module::f` path reaches.
	keyFuncModule = "um\x00"
	// keyRustModule + a Rust module (rustModuleKey): a module of the unit, a
	// file's or an inline or declared `mod` item's, which a path's first
	// component names before any crate of that name (rustPath).
	keyRustModule = "rm\x00"
	// keyRustCrate + a crate's name: the crate (rustFile) of the library a
	// Cargo.toml of the unit declares under that name, in Aux (rustCrates).
	keyRustCrate = "rc\x00"
	// keyFuncInternal + scope + name, and + scope + each qualified suffix of
	// two or more components: a C or C++ function with internal linkage,
	// which no other translation unit names. The scope is its file's path,
	// and, for a function a header declares, also "" (every file that
	// includes the header, which no table says transitively); Aux is its
	// namespace and its file's path, joined by a NUL.
	keyFuncInternal = "ui\x00"
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
	flagPrototype
	flagVirtual
	flagFinal
	flagInternal
)

// typeKinds are the declaration kinds a type name denotes; a Rust trait is
// an interface.
var typeKinds = []model.NodeKind{model.NodeClass, model.NodeInterface, model.NodeStruct, model.NodeEnum}

func isType(k model.NodeKind) bool { return slices.Contains(typeKinds, k) }

// isCallable reports the declaration kinds a call can reach; a test is a
// function the extractor marks as a test.
func isCallable(k model.NodeKind) bool {
	return k == model.NodeFunction || k == model.NodeMethod || k == model.NodeTest
}

// callable reports a declaration a call can reach: one of a callable kind,
// or a field the worker describes as a function of a type (an ECMAScript
// class field holding an arrow function or a function expression, which
// `this.handler()` calls as it would a method).
func callable(d *Decl) bool {
	return isCallable(d.Kind) || d.Kind == model.NodeField && d.Described && d.Receiver != ""
}

// The kinds of an export (keyExport's Aux).
const (
	exportType   = "type"
	exportFunc   = "func"
	exportModule = "module"
)

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
	file                        int32
	language, family, sep, path string
	// scopes are the scopes a simple name is visible in: Go's directory
	// and package, Java's package, Rust's and every other language's file,
	// and for C and C++ the namespaces the file declares in, "" first.
	scopes []string
	// imports are the file's imports, nil for a context read back from the
	// index (types), where they are not known; closed is shared by every
	// copy of the context and answers, once asked, whether each of them
	// names a declaration or file of the unit (closedImports).
	imports []Import
	closed  *closedState
	// decls are the file's declarations, nil for a context read back from
	// the index; a Rust path written inside an inline module is read from
	// that module (rustScope).
	decls []Decl
}

// closedState caches a file's closedImports answer.
type closedState struct{ asked, closed bool }

// contextOf is t's fileCtx: Go's directory and package clause, Java's
// package clause, the namespaces a C or C++ file declares in and those its
// using-directives name, and every other language's file.
func contextOf(file int32, t *Table) fileCtx {
	fc := fileCtx{file: file, language: t.Language, family: familyOf(t.Language), sep: separatorOf(t.Language), path: t.Path,
		imports: t.Imports, closed: &closedState{}, decls: t.Decls}
	switch fc.family {
	case "go":
		fc.scopes = []string{path.Dir(t.Path) + "\x00" + t.Package}
	case "java":
		fc.scopes = []string{t.Package}
	case "c":
		fc.scopes = []string{""}
		add := func(ns string) {
			if ns != "" && !slices.Contains(fc.scopes, ns) {
				fc.scopes = append(fc.scopes, ns)
			}
		}
		for i := range t.Decls {
			add(namespaceOf(&t.Decls[i], fc.sep))
		}
		for _, imp := range t.Imports {
			if imp.Name == ImportAll {
				add(imp.Path)
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
// enclosing parentheses (Go's `(*T)`), pointer, reference, slice, mutability
// or trait-object marker, no arguments of a generic.
func typeName(written string) string {
	s := strings.TrimSpace(written)
	for {
		trimmed := strings.TrimLeft(s, "(*&[] ")
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
	return strings.TrimRight(s, "*&) ")
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
	if err := h.readGoModule(ctx, u); err != nil {
		return nil, err
	}
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
	if u.family == FamilyRust {
		if err := rustCrates(ctx, u, decls); err != nil {
			return nil, err
		}
	}
	if h.decls, err = writeIndex(u.dir, decls); err != nil {
		return nil, err
	}

	links, err := newEntrySort(u.dir)
	if err != nil {
		return nil, err
	}
	defer links.Close()
	// methodSets holds each Go type's own methods and embedded types, from
	// which satisfy derives the method sets and Go's implicit interface
	// satisfaction.
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
	fc := contextOf(file, t)
	fileKey := refKey(Ref{File: file})
	if err := s.Add(entry{Key: keyFile + fileKey, Aux: t.Language + "\x00" + t.Path, Sig: strings.Join(fc.scopes, "\n")}); err != nil {
		return err
	}
	for _, imp := range t.Imports {
		if imp.Name == "" {
			continue
		}
		if err := s.Add(entry{Key: keyImport + fileKey + imp.Name, Ref: Ref{File: file, Index: imp.Scope}, Aux: imp.Path, Sig: imp.Original}); err != nil {
			return err
		}
	}
	for p := t.Path; ; {
		if err := s.Add(entry{Key: keyPath + p, Ref: Ref{File: file}}); err != nil {
			return err
		}
		_, rest, ok := strings.Cut(p, "/")
		if !ok {
			break
		}
		p = rest
	}
	if fc.family == "go" {
		if err := s.Add(entry{Key: keyGoDir + path.Dir(t.Path), Aux: t.Package}); err != nil {
			return err
		}
	}
	if fc.family == "rust" {
		// The file's module and every module item it declares.
		if err := s.Add(entry{Key: keyRustModule + rustModuleKey(rustFile(t.Path))}); err != nil {
			return err
		}
		for i := range t.Decls {
			d := &t.Decls[i]
			if d.Kind != model.NodeModule {
				continue
			}
			if m, ok := rustDeclModule(t, d); ok {
				if err := s.Add(entry{Key: keyRustModule + m + "::" + d.Name}); err != nil {
					return err
				}
			}
		}
	}
	// bases are the module paths an import names this file by, for the
	// families whose imports name modules.
	var bases []string
	if fc.family == "js" || fc.family == "python" {
		bases = moduleBases(t.Path, fc.family == "python")
	}
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
			if fc.family == "rust" {
				if m, ok := rustDeclModule(t, d); ok {
					keys = append(keys, keyTypeModule+m+"\x00"+d.Name)
				}
			}
		case fc.family == "c" && d.Internal && isCallable(d.Kind) && d.Receiver == "":
			if err := addInternal(s, t, d, internalEntry(ref, d, ns+"\x00"+t.Path)); err != nil {
				return err
			}
			continue
		case isCallable(d.Kind) && d.Receiver == "" && (d.Parent < 0 || !isType(t.Decls[d.Parent].Kind)) && !(d.Macro && fc.family == "rust"):
			prefix, qualifiedPrefix = keyFunc, keyFuncQualified
			if fc.family == "rust" {
				if m, ok := rustDeclModule(t, d); ok {
					keys = append(keys, keyFuncModule+m+"\x00"+d.Name)
				}
			}
		default:
			continue
		}
		keys = append(keys, prefix+fc.family+"\x00"+scope+"\x00"+d.Name)
		// A Rust path is read through the module it names (keyFuncModule,
		// keyTypeModule), never by a suffix of a qualified name, which any
		// module of the same name in any crate would match.
		if fc.family != "rust" {
			qualifiedSuffixes(d.Qualified, fc.sep, func(q string) { keys = append(keys, qualifiedPrefix+fc.family+"\x00"+q) })
		}
		e := declEntry(ref, d)
		e.Aux = ns
		for _, k := range keys {
			e.Key = k
			if err := s.Add(e); err != nil {
				return err
			}
		}
	}
	if len(bases) > 0 {
		return addExports(s, file, t, bases, fc.family == "python")
	}
	return nil
}

// declEntry is the entry of declaration d at ref, with what a lookup
// filters and answers on without reading d's table: its signature, its
// flags, the argument counts a call of it may pass (wire.Export.Params) and
// the type it returns when the worker named one.
// internalEntry is declEntry for a C or C++ function with internal linkage,
// carrying its namespace and file in Aux.
func internalEntry(ref Ref, d *Decl, aux string) entry {
	e := declEntry(ref, d)
	e.Aux = aux
	return e
}

func declEntry(ref Ref, d *Decl) entry {
	e := entry{Ref: ref, Sig: d.Signature, Flags: flagsOf(d)}
	if d.Described && callable(d) {
		// A variable-arity parameter is counted in Params and may receive
		// no argument.
		fixed := d.Params
		if d.Variadic {
			fixed -= min(fixed, 1)
		} else {
			e.MaxArgs = d.Params + 1
		}
		e.MinArgs = fixed - min(d.Optional, fixed)
	}
	if d.Returns.Kind == wire.TypeNamed {
		e.Returns = d.Returns.Name
	}
	return e
}

func flagsOf(d *Decl) byte {
	var f byte
	if d.Abstract {
		f |= flagAbstract
	}
	if d.Static {
		f |= flagStatic
	}
	if d.Prototype {
		f |= flagPrototype
	}
	if d.Virtual {
		f |= flagVirtual
	}
	if d.Final {
		f |= flagFinal
	}
	if d.Internal {
		f |= flagInternal
	}
	return f
}

// addLinks adds t's methods under the types that declare them and its
// declared supertypes in both directions. A Go table's methods and embedded
// types also go to methodSets, and its embeddings are promotion, not
// subtyping (goEmbeddings).
func (h *hierarchy) addLinks(ctx context.Context, links, methodSets *pagination.ExternalSort[entry], file int32, t *Table) error {
	fc := contextOf(file, t)
	for i := range t.Decls {
		d := &t.Decls[i]
		if !callable(d) {
			continue
		}
		ref := Ref{File: file, Index: int32(i)}
		e := declEntry(ref, d)
		add := func(typ Ref) error {
			e.Key = keyMethod + refKey(typ) + d.Name
			if err := links.Add(e); err != nil {
				return err
			}
			if fc.family != "go" {
				return nil
			}
			return addGoMethod(methodSets, typ, d.Name, e)
		}
		// A Java anonymous class body's method is its synthetic type's,
		// whatever declaration encloses it (an initializer block's is its
		// class).
		if p := d.Parent; p >= 0 && isType(t.Decls[p].Kind) && !anonymousReceiver(fc, d) {
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
	if fc.family == "go" {
		return h.goEmbeddings(ctx, methodSets, fc, t)
	}
	for i, st := range t.Supertypes {
		err := h.typesIn(ctx, fc, st.Type, func(sub entry) error {
			return h.typesIn(ctx, fc, st.Super, func(super entry) error {
				return addBase(links, sub.Ref, super.Ref, i)
			})
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// addSubtype records that sub is a direct subtype of super by a relation no
// declaration of sub lists in an order (Go's method-set satisfaction).
func addSubtype(links *pagination.ExternalSort[entry], sub, super Ref) error {
	return addBase(links, sub, super, -1)
}

// addBase records that sub is a direct subtype of super, which sub's file
// lists as its supertype row pos (-1 for none). A class's rows are listed in
// the order its declaration names its bases, so the keySuper entry carries
// pos (basePosition) and a lookup can read the bases as written, which
// Python's method resolution order is computed from.
func addBase(links *pagination.ExternalSort[entry], sub, super Ref, pos int) error {
	if sub == super {
		return nil
	}
	if err := links.Add(entry{Key: keySub + refKey(super), Ref: sub}); err != nil {
		return err
	}
	e := entry{Key: keySuper + refKey(sub), Ref: super}
	if pos >= 0 {
		e.Aux = basePosition(pos)
	}
	return links.Add(e)
}

// basePosition is a supertype row's position as a string that orders as the
// position does.
func basePosition(pos int) string {
	return string(binary.BigEndian.AppendUint64(nil, uint64(pos)))
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
		language, p, _ := strings.Cut(e.Aux, "\x00")
		fc = fileCtx{file: file, language: language, family: familyOf(language), sep: separatorOf(language), path: p,
			scopes: strings.Split(e.Sig, "\n")}
		found = true
		return nil
	})
	if err != nil || !found {
		return err
	}
	return h.typesIn(ctx, fc, name, func(e entry) error { return yield(e.Ref) })
}

// typesIn yields the type declarations name, as written in a file of
// context fc, denotes.
//
// A simple name is read, in order, in the file's scopes; through the import
// that introduces it (a Java single-type import, a C++ using-declaration, a
// Rust `use`, an ECMAScript or Python import of the module declaring it),
// which decides alone when there is one; and through the file's imports of
// every member of a path. A name none of these can supply falls back to
// every type of that name in the unit only in Go, Rust, C and C++, and only
// when every import of the file names a declaration or file of the unit
// (closedImports), so that the name cannot come from outside it: in Java
// such a name is a type of the file's package or of java.lang, and in
// ECMAScript and Python a global, never a unit type of another module.
//
// A Java anonymous class body's name (anonymousType) denotes its synthetic
// type alone.
//
// A qualified name whose first component an import introduces is read with
// that component replaced by what it denotes: a Go package by its import
// path, a Rust path by its module, an ECMAScript or Python module by its
// path, a Java or C++ name by the import's qualified name. Any other
// qualified name is the declarations whose qualified name ends in it.
func (h *hierarchy) typesIn(ctx context.Context, fc fileCtx, written string, yield func(entry) error) error {
	name := typeName(written)
	if name == "" {
		return nil
	}
	if fc.family == "java" {
		if ref, ok := anonymousType(fc.file, name); ok {
			return yield(entry{Ref: ref})
		}
	}
	if head, rest, ok := strings.Cut(name, fc.sep); ok {
		return h.qualifiedTypes(fc, name, head, rest, yield)
	}
	found := false
	hit := func(e entry) error {
		found = true
		return yield(e)
	}
	if fc.family == "c" {
		// A type of internal linkage (one in an unnamed namespace) that the
		// file itself declares decides alone: no other file's is named
		// there ([basic.link]). Another file's is read with the others,
		// since a header the file includes may declare it.
		var others []entry
		own := false
		err := h.decls.lookup(keyType+fc.family+"\x00\x00"+name, func(e entry) error {
			if !slices.ContainsFunc(fc.scopes, func(from string) bool { return visible(e.Aux, from) }) {
				return nil
			}
			if e.Flags&flagInternal != 0 && e.Ref.File == fc.file {
				own = true
				return hit(e)
			}
			others = append(others, e)
			return nil
		})
		if err != nil {
			return err
		}
		if !own {
			for _, e := range others {
				if err := hit(e); err != nil {
					return err
				}
			}
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
	imported := false
	err := h.importsAt(fc, name, func(p string, scope int32) error {
		imported = true
		return h.importedType(fc, name, p, scope, yield)
	})
	if err != nil || imported {
		return err
	}
	all := false
	err = h.importsAt(fc, ImportAll, func(p string, scope int32) error {
		all = true
		return h.memberType(fc, p, scope, name, hit)
	})
	if err != nil || found || all {
		return err
	}
	switch fc.family {
	case "java", "js", "python":
		return nil
	}
	if closed, err := h.closedImports(fc); err != nil || !closed {
		return err
	}
	return h.decls.lookup(keyTypeName+fc.family+"\x00"+name, yield)
}

// anonymousType is the synthetic type of the Java anonymous class body a
// name written in file denotes: the Java worker names each anonymous class
// body (and each enum constant's body) of a file `#k`, k its one-based
// ordinal in source order, a name no Java identifier can spell (JLS §3.8).
// Its methods are its own and it is a subtype of the type it instantiates
// (JLS §15.9.5), so a call on that type dispatches into them and a call on
// any other subtype does not. It has no declaration: its Ref's Index is
// -1-k, below the module's -1, and it is only ever a key of the hierarchy.
func anonymousType(file int32, name string) (Ref, bool) {
	digits, ok := strings.CutPrefix(name, "#")
	if !ok {
		return Ref{}, false
	}
	k, err := strconv.ParseInt(digits, 10, 32)
	if err != nil || k < 1 {
		return Ref{}, false
	}
	return Ref{File: file, Index: int32(-1 - k)}, true
}

// anonymousReceiver reports a method of a Java anonymous class body: its
// receiver is the body's synthetic type (anonymousType).
func anonymousReceiver(fc fileCtx, d *Decl) bool {
	if fc.family != "java" {
		return false
	}
	_, ok := anonymousType(fc.file, d.Receiver)
	return ok
}

// closedImports reports that every import of fc's file names a declaration
// or a file of the unit: a Go import a package of the unit, a C or C++
// include a member file (by a suffix of its path), a using-declaration a
// type or function of the unit, a Rust `use` a type or function of the
// module its path names. An import of every member of a path never reaches
// here (typesIn returns before), and a context without its imports (one
// read back from the index) is not closed. The answer is cached per file.
func (h *hierarchy) closedImports(fc fileCtx) (bool, error) {
	if fc.closed == nil {
		return false, nil
	}
	if fc.closed.asked {
		return fc.closed.closed, nil
	}
	closed := true
	for _, imp := range fc.imports {
		found := false
		hit := func(entry) error {
			found = true
			return nil
		}
		var err error
		switch fc.family {
		case "go":
			err = h.goPackages(imp.Path, func(string) error { return hit(entry{}) })
		case "c":
			if imp.Name == "" {
				err = h.decls.lookup(keyPath+strings.Trim(imp.Path, "<>"), hit)
				break
			}
			if err = h.decls.lookup(keyTypeQualified+fc.family+"\x00"+imp.Path, hit); err == nil && !found {
				err = h.decls.lookup(keyFuncQualified+fc.family+"\x00"+imp.Path, hit)
			}
		case "rust":
			module, last, ok := cutLast(imp.Path, fc.sep)
			if !ok {
				break
			}
			if err = h.rustModuleDecls(keyTypeModule, fc, imp.Scope, module, last, hit); err == nil && !found {
				err = h.rustModuleDecls(keyFuncModule, fc, imp.Scope, module, last, hit)
			}
		}
		if err != nil {
			return false, err
		}
		if !found {
			closed = false
			break
		}
	}
	fc.closed.asked, fc.closed.closed = true, closed
	return closed, nil
}

// qualifiedTypes is typesIn for a qualified name, head its first component
// and rest the others.
func (h *hierarchy) qualifiedTypes(fc fileCtx, name, head, rest string, yield func(entry) error) error {
	imported := false
	err := h.importsAt(fc, head, func(p string, scope int32) error {
		imported = true
		switch fc.family {
		case "go":
			return h.goPackages(p, func(scope string) error {
				return h.decls.lookup(keyType+fc.family+"\x00"+scope+"\x00"+rest, yield)
			})
		case "js", "python":
			// The import binds head to a module: `ns.C` of `import * as ns`,
			// `m.C` of `import a.b as m`; Python's `import a.b` binds a, and
			// a.b.C is then read as written.
			module, last, _ := cutLast(name, fc.sep)
			if fc.family == "python" && (p == head || strings.HasPrefix(p, head+".")) {
				return h.moduleType(fc, module, last, yield)
			}
			if module != head {
				if fc.family == "js" {
					return nil
				}
				p += fc.sep + strings.TrimPrefix(module, head+fc.sep)
			}
			return h.moduleType(fc, p, last, yield)
		}
		return h.importedType(fc, "", p+fc.sep+rest, scope, yield)
	})
	if err != nil || imported {
		return err
	}
	if fc.family == "rust" {
		module, last, _ := cutLast(name, fc.sep)
		found := false
		err := h.rustModuleDecls(keyTypeModule, fc, -1, module, last, func(e entry) error {
			found = true
			return yield(e)
		})
		if err != nil || found {
			return err
		}
	}
	return h.decls.lookup(keyTypeQualified+fc.family+"\x00"+name, yield)
}

// importedType yields the type declarations the path p an import binds the
// type name local to denotes. An ECMAScript or Python import's path is its
// module, which exports the type under the name the import names
// (Import.Original: `Foo` for `import { Foo as Bar }` and `from m import Foo
// as Bar`, `default` for a default import), the local name when it does not
// rename it; a namespace import binds a module, never a type.
func (h *hierarchy) importedType(fc fileCtx, local, p string, scope int32, yield func(entry) error) error {
	switch fc.family {
	case "rust":
		module, last, ok := cutLast(p, fc.sep)
		if !ok {
			return nil
		}
		return h.rustModuleDecls(keyTypeModule, fc, scope, module, last, yield)
	case "js", "python":
		original, err := h.importOriginal(fc, local, p)
		switch {
		case err != nil || original == ImportAll:
			return err
		case original != "":
			local = original
		}
		return h.moduleType(fc, p, local, yield)
	case "go":
		// A Go import binds a package, never a type.
		return nil
	}
	return h.decls.lookup(keyTypeQualified+fc.family+"\x00"+p, yield)
}

// memberType yields the type declarations named name among the members an
// import of every member of p reaches.
func (h *hierarchy) memberType(fc fileCtx, p string, scope int32, name string, yield func(entry) error) error {
	switch fc.family {
	case "go":
		return h.goPackages(p, func(scope string) error {
			return h.decls.lookup(keyType+fc.family+"\x00"+scope+"\x00"+name, yield)
		})
	case "rust":
		return h.rustModuleDecls(keyTypeModule, fc, scope, p, name, yield)
	case "js", "python":
		return h.moduleType(fc, p, name, yield)
	case "c":
		// A using-directive's namespace is one of the file's scopes.
		return nil
	}
	return h.decls.lookup(keyTypeQualified+fc.family+"\x00"+p+fc.sep+name, yield)
}

// moduleType yields the top-level type named name of the module an
// ECMAScript or Python import spec written in fc's file names, following
// the module's re-exports.
func (h *hierarchy) moduleType(fc fileCtx, spec, name string, yield func(entry) error) error {
	base, ok := moduleBase(fc.path, spec, fc.family == "python")
	if !ok {
		return nil
	}
	return h.moduleExport(base, name, func(e entry) error {
		if e.Aux != exportType {
			return nil
		}
		return yield(e)
	})
}

// moduleExport yields what the module whose path is base exports as name
// (resolveExport), nothing when the name resolves to nothing or ambiguously.
func (h *hierarchy) moduleExport(base, name string, yield func(entry) error) error {
	res, _, err := h.resolveExport(base, name, map[string]bool{})
	if err != nil {
		return err
	}
	for _, e := range res {
		if err := yield(e); err != nil {
			return err
		}
	}
	return nil
}

// resolveExport resolves the export name of the module whose path is base
// as ECMA-262 §16.2.1.6.3 ResolveExport does: what the module exports itself
// under the name; else what a re-export of the name resolves to in its
// module; else, for any name but `default`, what the modules its `export *`
// statements name resolve it to, when they agree, and ambiguous when two
// resolve it to different exports. seen holds the module and name pairs
// already asked along the resolution, so a cycle resolves to nothing. A
// Python module exports its own declarations only.
func (h *hierarchy) resolveExport(base, name string, seen map[string]bool) (res []entry, ambiguous bool, err error) {
	key := base + "\x00" + name
	if seen[key] {
		return nil, false, nil
	}
	seen[key] = true
	err = h.decls.lookup(keyExport+key, func(e entry) error {
		res = append(res, e)
		return nil
	})
	if err != nil || len(res) > 0 {
		return res, false, err
	}
	indirect := false
	err = h.decls.lookup(keyReexport+key, func(e entry) error {
		indirect = true
		r, amb, err := h.resolveExport(e.Aux, e.Sig, seen)
		res, ambiguous = append(res, r...), ambiguous || amb
		return err
	})
	if err != nil || indirect || name == "default" {
		return res, ambiguous, err
	}
	err = h.decls.lookup(keyReexport+base+"\x00*", func(e entry) error {
		if ambiguous {
			return nil
		}
		r, amb, err := h.resolveExport(e.Aux, name, seen)
		switch {
		case err != nil:
			return err
		case amb:
			ambiguous, res = true, nil
		case len(r) == 0:
		case res == nil:
			res = r
		case !sameExports(res, r):
			ambiguous, res = true, nil
		}
		return nil
	})
	return res, ambiguous, err
}

// sameExports reports that a and b are one resolution: the same
// declarations or module, in any order.
func sameExports(a, b []entry) bool {
	key := func(es []entry) []string {
		out := make([]string, len(es))
		for i, e := range es {
			out[i] = refKey(e.Ref) + e.Aux + "\x00" + e.Sig
		}
		slices.Sort(out)
		return slices.Compact(out)
	}
	return slices.Equal(key(a), key(b))
}

// addExports adds what a module exports itself and what it re-exports,
// under each module path bases names its file by. A Python module exports
// each module-level type and callable it declares. An ECMAScript module
// exports a module-level declaration under its own name (exportedByName),
// under the name a local `export` statement gives it (`export default C`,
// `export { a as b }`), a module as a namespace (`export * as ns from`),
// and re-exports another module's name (`export { x } from`, `export *
// from`, a local export of an import binding).
func addExports(s *pagination.ExternalSort[entry], file int32, t *Table, bases []string, python bool) error {
	add := func(prefix, name string, e entry) error {
		for _, b := range bases {
			e.Key = prefix + b + "\x00" + name
			if err := s.Add(e); err != nil {
				return err
			}
		}
		return nil
	}
	decl := func(name string, i int) error {
		d := &t.Decls[i]
		e := declEntry(Ref{File: file, Index: int32(i)}, d)
		e.Aux = exportFunc
		if isType(d.Kind) {
			e.Aux = exportType
		}
		return add(keyExport, name, e)
	}
	for i := range t.Decls {
		d := &t.Decls[i]
		if d.Parent >= 0 || !isType(d.Kind) && !isCallable(d.Kind) || !python && !exportedByName(t, i) {
			continue
		}
		if err := decl(d.Name, i); err != nil {
			return err
		}
	}
	if python {
		return nil
	}
	from := func(name, spec, original string) error {
		target, ok := moduleBase(t.Path, spec, false)
		switch {
		case !ok:
			return nil
		case original == "*" && name != "*":
			return add(keyExport, name, entry{Aux: exportModule, Sig: target})
		}
		return add(keyReexport, name, entry{Aux: target, Sig: original})
	}
	for _, x := range t.Reexports {
		if x.From != "" {
			if err := from(x.Name, x.From, x.Original); err != nil {
				return err
			}
			continue
		}
		if x.Name == x.Original {
			// The declaration's own name: exportedByName decides it.
			if slices.ContainsFunc(t.Decls, func(d Decl) bool { return d.Parent < 0 && d.Name == x.Name }) {
				continue
			}
		}
		local := false
		for i := range t.Decls {
			if d := &t.Decls[i]; d.Parent < 0 && d.Name == x.Original && (isType(d.Kind) || isCallable(d.Kind)) {
				local = true
				if err := decl(x.Name, i); err != nil {
					return err
				}
			}
		}
		if local {
			continue
		}
		for _, imp := range t.Imports {
			if imp.Name != x.Original {
				continue
			}
			original := imp.Original
			if original == "" {
				original = imp.Name
			}
			if err := from(x.Name, imp.Path, original); err != nil {
				return err
			}
		}
	}
	return nil
}

// exportedByName reports that ECMAScript declaration i of t is exported
// under its own name: the structural provider marks it exported, and either
// an `export` statement names it as itself (an exported declaration, `export
// { a }`) or none renames it. A declaration only `export { a as b }` names
// is exported as b alone.
func exportedByName(t *Table, i int) bool {
	d := &t.Decls[i]
	if !d.Exported {
		return false
	}
	renamed := false
	for _, x := range t.Reexports {
		if x.From != "" || x.Original != d.Name {
			continue
		}
		if x.Name == d.Name {
			return true
		}
		renamed = true
	}
	return !renamed
}

// importOriginal is the name the import of fc's file binding local to path p
// denotes in p's module when the import renames it ("default" for a default
// import, "*" for a namespace import), else empty.
func (h *hierarchy) importOriginal(fc fileCtx, local, p string) (string, error) {
	original := ""
	err := h.decls.lookup(keyImport+refKey(Ref{File: fc.file})+local, func(e entry) error {
		if e.Aux == p && e.Sig != "" {
			original = e.Sig
		}
		return nil
	})
	return original, err
}

// imports yields the path each import of fc's file binding name denotes,
// ImportAll naming the imports of every member.
func (h *hierarchy) imports(fc fileCtx, name string, yield func(string) error) error {
	return h.importsAt(fc, name, func(p string, _ int32) error { return yield(p) })
}

// importsAt is imports with the declaration each import is written in (-1
// the module, Import.Scope).
func (h *hierarchy) importsAt(fc fileCtx, name string, yield func(p string, scope int32) error) error {
	return h.decls.lookup(keyImport+refKey(Ref{File: fc.file})+name, func(e entry) error { return yield(e.Aux, e.Ref.Index) })
}

// methods yields the methods typ itself declares named method. A C++
// method's prototype is not yielded when a definition of the same method of
// typ is declared: that is the definition the prototype announces.
func (h *hierarchy) methods(typ Ref, method string, yield func(entry) error) error {
	return preferDefinitions(func(f func(entry) error) error {
		return h.links.lookup(keyMethod+refKey(typ)+method, f)
	}, yield)
}

// preferDefinitions yields what each yields, without its prototypes when
// it yields any definition. Only the entries of one name in one scope are
// held. A definition takes from each prototype of its signature what C++
// writes on the declaration alone: `virtual` and `final`, which a definition
// outside its class cannot repeat, and default arguments, which a call may
// leave out (the fewest arguments any of them accepts).
func preferDefinitions(each func(func(entry) error) error, yield func(entry) error) error {
	var prototypes, definitions []entry
	err := each(func(e entry) error {
		if e.Flags&flagPrototype != 0 {
			prototypes = append(prototypes, e)
		} else {
			definitions = append(definitions, e)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if len(definitions) == 0 {
		definitions = prototypes
	} else {
		for i := range definitions {
			d := &definitions[i]
			for _, p := range prototypes {
				if p.Sig == d.Sig {
					d.Flags |= p.Flags & (flagVirtual | flagFinal)
					d.MinArgs = min(d.MinArgs, p.MinArgs)
				}
			}
		}
	}
	for _, e := range definitions {
		if err := yield(e); err != nil {
			return err
		}
	}
	return nil
}

// declared yields the declarations of method a call on typ reaches without
// dispatch: typ's own, else, for a Python class, the first declaration along
// its method resolution order (pythonMRO), else the nearest along each
// supertype path, each once.
func (h *hierarchy) declared(typ Ref, method string, yield func(entry) error) error {
	found := false
	err := h.methods(typ, method, func(e entry) error {
		found = true
		return yield(e)
	})
	if err != nil || found {
		return err
	}
	order, err := h.pythonMRO(typ)
	if err != nil {
		return err
	}
	if order != nil {
		for _, t := range order[1:] {
			err := h.methods(t, method, func(e entry) error {
				found = true
				return yield(e)
			})
			if err != nil || found {
				return err
			}
		}
		return nil
	}
	seen := map[Ref]bool{typ: true}
	queue := []Ref{typ}
	done := map[Ref]bool{}
	for len(queue) > 0 {
		t := queue[0]
		queue = queue[1:]
		found := false
		if t != typ {
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
	return h.dispatchEntries(ctx, "", typ, method, func(e entry, _ bool) error { return yield(e.Ref) })
}

// dispatchEntries is dispatch for a call written in a file of family, which
// reaches the overrides in typ's subtypes only when the nearest declarations
// can be overridden (overridable); down reports, with each entry, that it
// did. The family "" overrides by name alone.
func (h *hierarchy) dispatchEntries(ctx context.Context, family string, typ Ref, method string,
	yield func(e entry, down bool) error,
) error {
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
	down, err := h.overridable(family, typ, method, slices.Concat(concrete, abstract))
	if err != nil {
		return err
	}
	seen := map[Ref]bool{typ: true}
	queue := []Ref{typ}
	for down && len(queue) > 0 {
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
		if err := yield(e, down); err != nil {
			return err
		}
	}
	return nil
}

// overridable reports that a call of method on typ reaches the overrides
// in typ's subtypes, given found, the nearest declarations of it typ
// declares or inherits, by the rule of the call's family:
//   - Java: one of them is an instance method that is not Final (JLS
//     §8.4.8): a static method hides and a final or private one, or one of a
//     final class, has no override;
//   - C and C++: one of them is not Final, and typ or a supertype declares
//     the method virtual ([class.virtual]): a member overriding a virtual
//     one is virtual without saying so, and a non-virtual call runs the
//     member the static type names;
//   - Go: one of them is an interface's method; a concrete type's method is
//     promoted into the types embedding it, never overridden;
//   - every other family: always.
//
// When the unit declares none (typ inherits the method from outside the
// unit, whose rules are unknown), the call reaches every override.
func (h *hierarchy) overridable(family string, typ Ref, method string, found []entry) (bool, error) {
	if len(found) == 0 {
		return true, nil
	}
	switch family {
	case "java":
		return slices.ContainsFunc(found, func(e entry) bool { return e.Flags&(flagStatic|flagFinal) == 0 }), nil
	case "go":
		return slices.ContainsFunc(found, func(e entry) bool { return e.Flags&flagAbstract != 0 }), nil
	case "c":
		if !slices.ContainsFunc(found, func(e entry) bool { return e.Flags&flagFinal == 0 }) {
			return false, nil
		}
		return h.virtual(typ, method)
	}
	return true, nil
}

// virtual reports that typ or one of its supertypes declares method
// virtual. Every declaration is read, the prototypes a definition replaces
// included (methods drops them): only the declaration inside the class can
// say `virtual`, never a definition outside it.
func (h *hierarchy) virtual(typ Ref, method string) (bool, error) {
	seen := map[Ref]bool{typ: true}
	queue := []Ref{typ}
	for len(queue) > 0 {
		t := queue[0]
		queue = queue[1:]
		found := false
		err := h.links.lookup(keyMethod+refKey(t)+method, func(e entry) error {
			found = found || e.Flags&flagVirtual != 0
			return nil
		})
		if err != nil || found {
			return found, err
		}
		err = h.links.lookup(keySuper+refKey(t), func(e entry) error {
			if !seen[e.Ref] {
				seen[e.Ref] = true
				queue = append(queue, e.Ref)
			}
			return nil
		})
		if err != nil {
			return false, err
		}
	}
	return false, nil
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
	// MinArgs and MaxArgs bound the arguments a call of the declaration may
	// pass: at least MinArgs and, when MaxArgs is non-zero, fewer than
	// MaxArgs. The zero values accept any count, as for a declaration the
	// worker described nothing of (declEntry).
	MinArgs, MaxArgs uint32
	// Returns is the type the declaration returns, as its own file writes
	// it, when the worker named one.
	Returns string
}

// fits reports that a call passing n arguments may call e.
func (e entry) fits(n int) bool {
	return n >= int(e.MinArgs) && (e.MaxArgs == 0 || n < int(e.MaxArgs))
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
	if a.Flags != b.Flags {
		return int(a.Flags) - int(b.Flags)
	}
	if a.MinArgs != b.MinArgs {
		return cmp.Compare(a.MinArgs, b.MinArgs)
	}
	if a.MaxArgs != b.MaxArgs {
		return cmp.Compare(a.MaxArgs, b.MaxArgs)
	}
	return strings.Compare(a.Returns, b.Returns)
}

func encodeEntry(e entry) ([]byte, error) {
	b := make([]byte, 0, len(e.Key)+len(e.Aux)+len(e.Sig)+len(e.Returns)+6*binary.MaxVarintLen32+10)
	b = binary.AppendUvarint(b, uint64(len(e.Key)))
	b = append(b, e.Key...)
	b = append(b, e.Tag)
	b = binary.BigEndian.AppendUint32(b, uint32(e.Ref.File))
	b = binary.BigEndian.AppendUint32(b, uint32(e.Ref.Index))
	b = binary.AppendUvarint(b, uint64(len(e.Aux)))
	b = append(b, e.Aux...)
	b = binary.AppendUvarint(b, uint64(len(e.Sig)))
	b = append(b, e.Sig...)
	b = append(b, e.Flags)
	b = binary.AppendUvarint(b, uint64(e.MinArgs))
	b = binary.AppendUvarint(b, uint64(e.MaxArgs))
	b = binary.AppendUvarint(b, uint64(len(e.Returns)))
	return append(b, e.Returns...), nil
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
	if e.Sig, ok = str(); !ok || len(b) < 1 {
		return e, errMalformedEntry
	}
	e.Flags = b[0]
	b = b[1:]
	count := func() (uint32, bool) {
		n, w := binary.Uvarint(b)
		if w <= 0 || n > 1<<32-1 {
			return 0, false
		}
		b = b[w:]
		return uint32(n), true
	}
	if e.MinArgs, ok = count(); !ok {
		return e, errMalformedEntry
	}
	if e.MaxArgs, ok = count(); !ok {
		return e, errMalformedEntry
	}
	if e.Returns, ok = str(); !ok || len(b) != 0 {
		return e, errMalformedEntry
	}
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
