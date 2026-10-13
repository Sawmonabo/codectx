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
// file; only the first key of each block is held in memory. Beside them is the
// identity table (idTable), which spells a member ordinal or a table index
// as the file or declaration it denotes, so a lookup can be recorded under a
// key that means the same thing in every build (trace.go). A static-family
// unit's state keeps all three, and a refresh of the same layout reads them
// back instead of building them (refresh.layout).
type hierarchy struct {
	decls *index
	links *index
	// ids translates the unit's ordinals to identities (idTable).
	ids *idTable
	// goModule is a Go unit's module path, read from the go.mod at its
	// project root goRoot ("." for the repository root); empty when there
	// is none, and then no Go import path names a package of the unit.
	goModule, goRoot string
	// goInfo is the rest of what a Go unit's go.mod, go.work and vendor
	// manifest say (goModuleInfo); goExt, while the hierarchy is built, the
	// Go packages outside the unit read for it (goExternal).
	goInfo *goModuleInfo
	goExt  *goExternal
	// goSurface is the digest of what the build read from the packages
	// outside a Go unit (goExternal.surface), empty for any other unit.
	goSurface string
	// goSpellings counts the unit's Go methods by how exactly their
	// method-set keys spell their signatures (goMethodKey).
	goSpellings [3]uint32
	// goMemo remembers Go lookups while the hierarchy is built (goMemo).
	goMemo goMemos
	// members is every member's projection with a table, by FileID
	// (projection.go).
	members *pagination.SortedRun[projection]
	// note, while a member's reads are recorded (record), is told each
	// read by its trace key.
	note func(TraceKey) error
}

// record has the indexes report every key looked up, and goPackages every
// read of the module path, to note by trace key until record(nil) stops
// it: the reads of the one member being resolved. A unit's members are
// resolved one at a time, so one recorder serves the hierarchy.
func (h *hierarchy) record(note func(TraceKey) error) {
	h.note = note
	if note == nil {
		h.decls.trace, h.links.trace = nil, nil
		return
	}
	trace := func(scope string) func(string) error {
		return func(key string) error {
			k, err := h.ids.traceKey(scope, key)
			if err != nil {
				return err
			}
			return note(k)
		}
	}
	h.decls.trace, h.links.trace = trace(scopeDecls), trace(scopeLinks)
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
	// (Sig, Import.Original), importRequired in Flags for a `require`.
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
	// keySuper + refKey(type): a declared or satisfied direct supertype,
	// flagInterface in Flags when it is an interface or trait.
	keySuper = "p\x00"
	// keyExternalSuper + refKey(type): a supertype outside the unit, its
	// qualified name in Aux (externalName), flagInterface in Flags when the
	// source proves it an interface or trait (externalInterface). A type of
	// any family but Go has those its own declaration and its impls write; a Go
	// type has every external type it embeds, directly or through an
	// embedded type of the unit (satisfy), flagInterface when the embedding
	// type is an interface.
	keyExternalSuper = "px\x00"
	// keyParam + refKey(callable): one declared parameter of the callable
	// (Decl.Parameters), its position in Aux (basePosition), its name in
	// Sig, its Parameter.Index in MinArgs and its marks in Flags
	// (paramVariadic, paramKeywordOnly, paramReturned).
	keyParam = "pa\x00"
	// keyMacro + name: a C or C++ object-like macro of that name whose
	// replacement is one identifier (Decl.Alias), its file's path in Aux and
	// the identifier in Sig.
	keyMacro = "ma\x00"
	// keyMacroVisibility + name: a C or C++ object-like macro of that name
	// whose replacement states a visibility (Decl.Visibility), its file's
	// path in Aux and the visibility in Sig.
	keyMacroVisibility = "mv\x00"
	// keyJavaMain + refKey(type): a Java method the type declares that a
	// class can be launched by (Decl.Main), its form in Flags.
	keyJavaMain = "jm\x00"
)

// The flags of an entry.
const (
	flagAbstract byte = 1 << iota
	flagStatic
	flagPrototype
	flagVirtual
	flagFinal
	flagInternal
	// flagInterface marks a type declaration of kind interface (a Rust
	// trait included), and a supertype entry naming one.
	flagInterface
	// flagExactReturn marks a callable whose result is an object of exactly
	// the type it returns (Decl.Returns marked wire.ExactObject: a C++
	// function returning a class type by value), so a call on that result
	// runs that type's method.
	flagExactReturn
)

// The marks of an import entry (keyImport).
const (
	// importRequired marks an ECMAScript `require` binding (Import.Syntax
	// ImportRequire), whose namespace binding (`const x = require("m")`)
	// is the module's whole value as well as its exports object.
	importRequired byte = 1 << iota
)

// The marks of a parameter entry (keyParam).
const (
	paramVariadic byte = 1 << iota
	paramKeywordOnly
	// paramReturned marks a parameter whose value reaches its callable's
	// return (Parameter.Returned): a body edit that changes it changes the
	// entry, so every member whose argument flows read it re-runs.
	paramReturned
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
	// anonymous maps, for a Java file, the byte offset of the name token of
	// each anonymous class body's declaration (a class named `new T`, or the
	// enum constant whose body it is) to its index in decls
	// (anonymousType), and synthetic the offset of each body name with no
	// such declaration to its rank (javaAnonymous).
	anonymous, synthetic map[uint64]int32
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
		a := anonymousNames(t)
		fc.anonymous, fc.synthetic = a.decl, a.rank
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

// buildHierarchy reads the unit's members into its hierarchy: each member's
// table once, or, for a member from holds (a refresh's members whose digest
// held), the projection its predecessor stored, and in the second pass the
// projections the first kept (projection.go). from is nil on a full build.
func buildHierarchy(ctx context.Context, u *unit, from *heldProjections) (*hierarchy, error) {
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
	if h.ids, err = newIDTable(u.dir); err != nil {
		return nil, err
	}
	es, err := newESResolver(ctx, u)
	if err != nil {
		return nil, err
	}
	defer es.close()
	projections, err := newProjectionSort(u.dir)
	if err != nil {
		return nil, err
	}
	defer projections.Close()
	held, stop, err := from.open()
	if err != nil {
		return nil, err
	}
	defer stop()
	var file int32 = -1
	err = u.members(func(fv model.FileVersion) error {
		file++
		t, raw, err := held(fv.ID)
		if err != nil {
			return err
		}
		if t == nil {
			full, ok, err := u.tables.Table(ctx, u.gen, fv)
			if err != nil {
				return err
			}
			if ok {
				t = full.project()
				if raw, err = t.encodeProjected(); err != nil {
					return err
				}
			}
		}
		if err := h.ids.add(fv.ID, t); err != nil {
			return err
		}
		if t == nil {
			return nil
		}
		if err := projections.Add(projection{File: fv.ID, Raw: raw}); err != nil {
			return err
		}
		return addDecls(ctx, decls, file, t, es)
	})
	if err == nil {
		err = h.ids.finish()
	}
	if err != nil {
		return nil, err
	}
	if h.members, err = projections.Sorted(); err != nil {
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
	if u.family == FamilyGo {
		if h.goExt, err = newGoExternal(ctx, h, u); err != nil {
			return nil, err
		}
		h.goSurface = h.goExt.surface()
		// The memos answer for this build's indexes and packages alone.
		h.goMemo = goMemos{}
		defer func() {
			h.goExt.close()
			h.goExt = nil
			h.goMemo = goMemos{}
		}()
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
	if h.goExt != nil {
		if err := h.goExt.materialize(ctx, methodSets); err != nil {
			return nil, err
		}
		if err := links.Add(entry{Key: keyGoCoverage, MinArgs: uint32(h.goExt.read), MaxArgs: uint32(h.goExt.unavailable)}); err != nil {
			return nil, err
		}
	}
	err = h.projected(func(file int32, t *Table) error {
		return h.addLinks(ctx, links, methodSets, file, t)
	})
	if err != nil {
		return nil, err
	}
	if h.goExt != nil {
		err := links.Add(entry{Key: keyGoCoverage, Tag: 1, MinArgs: h.goSpellings[goSpellingUnresolved],
			MaxArgs: h.goSpellings[goSpellingUnknown]})
		if err != nil {
			return nil, err
		}
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

// projected yields, in ordinal order, each member with a table and its
// projection, read from h's projections alone.
func (h *hierarchy) projected(yield func(file int32, t *Table) error) error {
	var ord int32
	return h.members.Each(func(p projection) error {
		for ; ; ord++ {
			id, err := h.ids.file(ord)
			if err != nil {
				return err
			}
			if id == p.File {
				break
			}
		}
		t, err := decodeProjected(p.Raw)
		if err != nil {
			return err
		}
		err = yield(ord, t)
		ord++
		return err
	})
}

// addDecls adds t's file context, type declarations and free functions,
// and, through es, what an ECMAScript file's specifiers resolve to.
func addDecls(ctx context.Context, s *pagination.ExternalSort[entry], file int32, t *Table, es *esResolver) error {
	if err := es.add(ctx, s, t); err != nil {
		return err
	}
	if err := addJSHandlers(s, file, t, es); err != nil {
		return err
	}
	fc := contextOf(file, t)
	fileKey := refKey(Ref{File: file})
	if err := s.Add(entry{Key: keyFile + fileKey, Aux: t.Language + "\x00" + t.Path, Sig: strings.Join(fc.scopes, "\n")}); err != nil {
		return err
	}
	for _, imp := range t.Imports {
		if imp.Name == "" {
			continue
		}
		if fc.family == "go" && imp.Name != ImportAll {
			// The file's named imports listed together, which a Go
			// identity read in a context from the index needs (goUnitNames).
			if err := s.Add(entry{Key: keyImport + fileKey + goImportList, Aux: imp.Path, Sig: imp.Name}); err != nil {
				return err
			}
		}
		e := entry{Key: keyImport + fileKey + imp.Name, Ref: Ref{File: file, Index: imp.Scope}, Aux: imp.Path, Sig: imp.Original}
		if imp.Syntax == ImportRequire {
			e.Flags = importRequired
		}
		if err := s.Add(e); err != nil {
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
		if err := addParameters(s, ref, d); err != nil {
			return err
		}
		if fc.family == "go" && strings.HasPrefix(d.Identity, "=") && isType(d.Kind) {
			if err := s.Add(entry{Key: keyGoAlias + declScope(t, fc) + "\x00" + d.Name, Ref: ref, Sig: d.Identity[1:]}); err != nil {
				return err
			}
		}
		if fc.family == "go" && strings.HasPrefix(d.Identity, "#") && d.Kind == model.NodeConstant {
			if err := s.Add(entry{Key: keyGoConst + declScope(t, fc) + "\x00" + d.Name, Ref: ref, Sig: d.Identity[1:]}); err != nil {
				return err
			}
		}
		if err := addRustMacro(s, fc, ref, t, d); err != nil {
			return err
		}
		if err := addRustConstructor(s, fc, ref, t, d); err != nil {
			return err
		}
		if fc.family == "c" && d.Alias != "" {
			if err := s.Add(entry{Key: keyMacro + d.Name, Ref: ref, Aux: t.Path, Sig: d.Alias}); err != nil {
				return err
			}
		}
		if fc.family == "c" && d.Visibility != "" {
			if err := s.Add(entry{Key: keyMacroVisibility + d.Name, Ref: ref, Aux: t.Path, Sig: d.Visibility}); err != nil {
				return err
			}
		}
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
		return addExports(s, file, t, bases, fc.family == "python", es)
	}
	return nil
}

// addParameters adds the declared parameters of the declaration d at ref
// (keyParam), in declaration order.
func addParameters(s *pagination.ExternalSort[entry], ref Ref, d *Decl) error {
	for pos, p := range d.Parameters {
		e := entry{Key: keyParam + refKey(ref), Ref: ref, Aux: basePosition(pos), Sig: p.Name, MinArgs: p.Index}
		if p.Variadic {
			e.Flags |= paramVariadic
		}
		if p.KeywordOnly {
			e.Flags |= paramKeywordOnly
		}
		if p.Returned {
			e.Flags |= paramReturned
		}
		if err := s.Add(e); err != nil {
			return err
		}
	}
	return nil
}

// internalEntry is declEntry for a C or C++ function with internal linkage,
// carrying its namespace and file in Aux.
func internalEntry(ref Ref, d *Decl, aux string) entry {
	e := declEntry(ref, d)
	e.Aux = aux
	return e
}

// declEntry is the entry of declaration d at ref, with what a lookup
// filters and answers on without reading d's table: its signature, its
// flags, the argument counts a call of it may pass (wire.Export.Params) and
// the type it returns when the worker named one.
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
	if d.Kind == model.NodeInterface {
		f |= flagInterface
	}
	if d.Returns.Kind == wire.TypeNamed && d.Returns.Index == wire.ExactObject {
		f |= flagExactReturn
	}
	return f
}

// addLinks adds t's methods under the types that declare them, a Java
// type's fields under it (keyGoField), and its declared supertypes in both
// directions, those outside the unit under keyExternalSuper. A Go table's
// methods and embedded types also go to methodSets, and its embeddings are
// promotion, not subtyping (goEmbeddings). The routing and request-input
// facts other files' walks read go to links too (addSpringLinks,
// addGoRouting, addJSRouting).
func (h *hierarchy) addLinks(ctx context.Context, links, methodSets *pagination.ExternalSort[entry], file int32, t *Table) error {
	fc := contextOf(file, t)
	if err := addSpringLinks(links, file, t); err != nil {
		return err
	}
	if err := addGoRouting(links, file, t); err != nil {
		return err
	}
	if err := addJSRouting(links, file, t); err != nil {
		return err
	}
	for i := range t.Decls {
		d := &t.Decls[i]
		if fc.family == "java" && d.Kind == model.NodeField && d.Parent >= 0 && isType(t.Decls[d.Parent].Kind) {
			f := entry{Key: keyGoField + refKey(Ref{File: file, Index: d.Parent}) + d.Name, Ref: Ref{File: file, Index: int32(i)}}
			if d.Returns.Kind == wire.TypeNamed {
				f.Sig = d.Returns.Name
			}
			if err := links.Add(f); err != nil {
				return err
			}
			continue
		}
		if !callable(d) {
			continue
		}
		ref := Ref{File: file, Index: int32(i)}
		e := declEntry(ref, d)
		// A method an interface declares without an export (a Go method
		// specification, a pure member the worker described nothing of) has
		// no body a call could run but an implementation's.
		if p := d.Parent; !d.Described && p >= 0 && t.Decls[p].Kind == model.NodeInterface {
			e.Flags |= flagAbstract
		}
		counted := false
		add := func(typ Ref) error {
			e.Key = keyMethod + refKey(typ) + d.Name
			if err := links.Add(e); err != nil {
				return err
			}
			if fc.family != "go" {
				return nil
			}
			key, spelling, err := goMethodKey(ctx, h.goTypes(), goUnitNames{h: h, fc: fc}, d.Name, d.Identity, ref)
			if err != nil {
				return err
			}
			if !counted {
				// A method is counted once, whatever types its receiver
				// names.
				h.goSpellings[spelling]++
				counted = true
			}
			return addGoMethod(methodSets, typ, d.Name, key, e)
		}
		// A Java anonymous class body's method is its synthetic type's,
		// whatever declaration encloses it (an initializer block's is its
		// class).
		if p := d.Parent; p >= 0 && isType(t.Decls[p].Kind) && !anonymousReceiver(fc, d) {
			if err := add(Ref{File: file, Index: p}); err != nil {
				return err
			}
			if d.Main != 0 {
				err := links.Add(entry{Key: keyJavaMain + refKey(Ref{File: file, Index: p}), Ref: ref, Flags: d.Main})
				if err != nil {
					return err
				}
			}
			continue
		}
		if d.Receiver == "" {
			continue
		}
		typed := false
		err := h.typesIn(ctx, fc, d.Receiver, func(te entry) error {
			typed = true
			return add(te.Ref)
		})
		if err != nil {
			return err
		}
		if !typed {
			// A function stored on the prototype of an imported function
			// (`const C = require("./c"); C.prototype.m = function () {}`)
			// is a method of that function's objects.
			if err := h.importedConstructors(fc, d.Receiver, func(fe entry) error { return add(fe.Ref) }); err != nil {
				return err
			}
		}
	}
	if fc.family == "go" {
		if err := h.goEmbeddings(ctx, methodSets, fc, t); err != nil {
			return err
		}
		return h.goConversions(ctx, links, fc, t)
	}
	for i, st := range t.Supertypes {
		var supers []entry
		err := h.typesIn(ctx, fc, st.Super, func(super entry) error {
			supers = append(supers, super)
			return nil
		})
		if err != nil {
			return err
		}
		external, ok := "", false
		if len(supers) == 0 {
			if external, ok, err = h.externalName(ctx, fc, st.Super); err != nil {
				return err
			}
			if !ok {
				// A supertype that names nothing the unit or the language
				// knows leaves the file's other rows linked.
				continue
			}
		}
		err = h.typesIn(ctx, fc, st.Type, func(sub entry) error {
			if ok {
				var flags byte
				if st.Interface || externalInterface(fc.family, sub.Flags) {
					flags = flagInterface
				}
				return links.Add(entry{Key: keyExternalSuper + refKey(sub.Ref), Ref: sub.Ref, Aux: external,
					Sig: basePosition(i), Flags: flags})
			}
			for _, super := range supers {
				if err := addBase(links, sub.Ref, super.Ref, i, super.Flags&flagInterface); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// addSubtype records that sub is a direct subtype of super by a relation no
// declaration of sub lists in an order (Go's method-set satisfaction).
func addSubtype(links *pagination.ExternalSort[entry], sub, super Ref, flags byte) error {
	return addBase(links, sub, super, -1, flags)
}

// addBase records that sub is a direct subtype of super, which sub's file
// lists as its supertype row pos (-1 for none). A class's rows are listed in
// the order its declaration names its bases, so the keySuper entry carries
// pos (basePosition) and a lookup can read the bases as written, which
// Python's method resolution order is computed from. flags is
// flagInterface when super is an interface or trait.
func addBase(links *pagination.ExternalSort[entry], sub, super Ref, pos int, flags byte) error {
	if sub == super {
		return nil
	}
	if err := links.Add(entry{Key: keySub + refKey(super), Ref: sub}); err != nil {
		return err
	}
	e := entry{Key: keySuper + refKey(sub), Ref: super, Flags: flags}
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
	return h.typeEntries(ctx, file, name, func(e entry) error { return yield(e.Ref) })
}

// typeEntries is types yielding each type's entry, whose Flags say whether
// it is an interface.
func (h *hierarchy) typeEntries(ctx context.Context, file int32, name string, yield func(entry) error) error {
	fc, found, err := h.fileContext(file)
	if err != nil || !found {
		return err
	}
	return h.typesIn(ctx, fc, name, yield)
}

// fileContext is the context of the member file at ordinal file as the
// index holds it (keyFile), without its imports and declarations, which a
// lookup through the context reads from the index instead; found is false
// for a member without a table.
func (h *hierarchy) fileContext(file int32) (fc fileCtx, found bool, err error) {
	err = h.decls.lookup(keyFile+refKey(Ref{File: file}), func(e entry) error {
		language, p, _ := strings.Cut(e.Aux, "\x00")
		fc = fileCtx{file: file, language: language, family: familyOf(language), sep: separatorOf(language), path: p,
			scopes: strings.Split(e.Sig, "\n")}
		found = true
		return nil
	})
	return fc, found, err
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
		if ref, ok := anonymousType(fc, name); ok {
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

// anonymousType is the type of the Java anonymous class body a name written
// in the file of fc denotes: the Java worker names each anonymous class body
// (and each enum constant's body) `#n`, n the byte offset of the name token
// of the declaration the structural provider publishes for it (a class
// named `new T`, or the enum constant), a name no Java identifier can spell
// (JLS §3.8). Its methods are its own and it is a subtype of the type it
// instantiates (JLS §15.9.5), so a call on that type dispatches into them
// and a call on any other subtype does not. A body with no such declaration
// (a creation the structural query does not capture) is a synthetic type of
// the hierarchy alone: its Ref's Index is -2-k, below the module's -1, k its
// rank among the file's bodies with none (javaAnonymous), so an edit that
// moves the body keeps its Ref.
func anonymousType(fc fileCtx, name string) (Ref, bool) {
	digits, ok := strings.CutPrefix(name, "#")
	if !ok {
		return Ref{}, false
	}
	n, err := strconv.ParseUint(digits, 10, 31)
	if err != nil {
		return Ref{}, false
	}
	if i, ok := fc.anonymous[n]; ok {
		return Ref{File: fc.file, Index: i}, true
	}
	if k, ok := fc.synthetic[n]; ok {
		return Ref{File: fc.file, Index: -2 - k}, true
	}
	// A name no type name of the table carries: no method or supertype row
	// is written under it, so it denotes a type with nothing in it.
	return Ref{File: fc.file, Index: int32(-2 - len(fc.synthetic))}, true
}

// anonymousClass reports the declaration of a Java anonymous class body: a
// class whose name, `new T`, no Java identifier can spell.
func anonymousClass(d *Decl) bool {
	return d.Kind == model.NodeClass && strings.HasPrefix(d.Name, "new ")
}

// DetailSupertypeAnonymous: an `extends` from a Java anonymous class body
// to the type outside the project it instantiates, which the creation
// proves neither a class nor an interface (JLS §15.9.5: the body extends a
// class, or extends Object and implements an interface).
const DetailSupertypeAnonymous = "supertype_anonymous"

// anonymousReceiver reports a method of a Java anonymous class body: its
// receiver is the body's synthetic type (anonymousType).
func anonymousReceiver(fc fileCtx, d *Decl) bool {
	if fc.family != "java" {
		return false
	}
	_, ok := anonymousType(fc, d.Receiver)
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

// importedConstructors yields the functions an ECMAScript import of fc's
// file binding local denotes: what the module exports under the name the
// import names, its whole value for a `require` namespace binding. Every
// ordinary function is a constructor (ECMA-262 §10.2: a function object
// with [[Construct]] and a `prototype`), so `new C()` of an imported
// function makes an object of it and `C.prototype.m = …` in the importing
// file stores a method of it.
func (h *hierarchy) importedConstructors(fc fileCtx, local string, yield func(entry) error) error {
	if fc.family != "js" {
		return nil
	}
	return h.importsAt(fc, local, func(p string, _ int32) error {
		original, required, err := h.importOriginal(fc, local, p)
		name := local
		switch {
		case err != nil:
			return err
		case original == ImportAll && required:
			name = wire.ExportAssignment
		case original == ImportAll:
			return nil
		case original != "":
			name = original
		}
		return h.modules(fc, p, func(base string) error {
			return h.moduleExport(base, name, func(e entry) error {
				if e.Aux != exportFunc {
					return nil
				}
				return yield(e)
			})
		})
	})
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
		original, required, err := h.importOriginal(fc, local, p)
		switch {
		case err != nil:
			return err
		case original == ImportAll && required:
			// `const C = require("./c")` binds the module's whole value
			// (`module.exports = C`, `export = C`).
			local = wire.ExportAssignment
		case original == ImportAll:
			return nil
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
	return h.modules(fc, spec, func(base string) error {
		return h.moduleExport(base, name, func(e entry) error {
			if e.Aux != exportType {
				return nil
			}
			return yield(e)
		})
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
	if err != nil || indirect || name == "default" || name == wire.ExportAssignment {
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
func addExports(s *pagination.ExternalSort[entry], file int32, t *Table, bases []string, python bool, es *esResolver) error {
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
		for _, target := range es.targets(t.Path, spec) {
			var err error
			if original == "*" && name != "*" {
				err = add(keyExport, name, entry{Aux: exportModule, Sig: target})
			} else {
				err = add(keyReexport, name, entry{Aux: target, Sig: original})
			}
			if err != nil {
				return err
			}
		}
		return nil
	}
	for _, x := range t.Reexports {
		if x.From != "" {
			if err := from(x.Name, x.From, x.Original); err != nil {
				return err
			}
			continue
		}
		if x.Name == x.Original {
			// The declaration's own name: exportedByName decides it for a
			// declaration the provider marks exported. One it does not (a
			// CommonJS `module.exports = { helper }`) is exported here.
			if slices.ContainsFunc(t.Decls, func(d Decl) bool { return d.Parent < 0 && d.Name == x.Name && d.Exported }) {
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
// import, "*" for a namespace import), else empty; required reports a
// `require` binding (importRequired).
func (h *hierarchy) importOriginal(fc fileCtx, local, p string) (original string, required bool, err error) {
	err = h.decls.lookup(keyImport+refKey(Ref{File: fc.file})+local, func(e entry) error {
		if e.Aux == p && e.Sig != "" {
			original, required = e.Sig, e.Flags&importRequired != 0
		}
		return nil
	})
	return original, required, err
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

// overridable reports that a call of method on a receiver of static type
// typ can run an override of what it names (DispatchVirtual), given found,
// the nearest declarations of it typ declares or inherits, by the rule of
// the call's family:
//   - Java: one of them is an instance method that is not Final (JLS
//     §8.4.8): a static method hides and a final or private one, or one of a
//     final class, has no override;
//   - C and C++: one of them is not Final, and typ or a supertype declares
//     the method virtual ([class.virtual]): a member overriding a virtual
//     one is virtual without saying so, and a non-virtual call runs the
//     member the static type names;
//   - Go: one of them is an interface's method; a concrete type's method is
//     promoted into the types embedding it, never overridden;
//   - ECMAScript: unless the method is private (`#p`), whose name is its
//     class body's alone;
//   - every other family: always (a Rust call reads its receiver's kind
//     instead, staticFile.dispatchOf).
//
// When the unit declares none (typ inherits the method from outside the
// unit, whose rules are unknown), an override can run.
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
	case "js":
		return !strings.HasPrefix(method, "#"), nil
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
	if h == nil {
		return nil
	}
	h.decls.close()
	h.links.close()
	h.ids.close()
	h.members.Close()
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
	// trace, when set, is told every key looked up, before the lookup: the
	// hierarchy's current-member recorder (hierarchy.record).
	trace func(key string) error
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
	if ix == nil {
		return nil
	}
	if ix.trace != nil {
		if err := ix.trace(key); err != nil {
			return err
		}
	}
	if len(ix.first) == 0 {
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

// each yields every entry of the index, in order.
func (ix *index) each(yield func(entry) error) error {
	if ix == nil {
		return nil
	}
	r := bufio.NewReader(io.NewSectionReader(ix.f, 0, ix.size))
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
		if err := yield(e); err != nil {
			return err
		}
	}
}

func (ix *index) close() {
	if ix != nil && ix.lease != nil {
		ix.lease.Release()
		ix.lease = nil
	}
}

// The hierarchy's answers the resolvers of both families share.

// parameters yields the declared parameters of the callable declaration at
// callable, in declaration order, each with its position in that
// declaration's Decl.Parameters, through a lookup the trace records, so a
// change of the callee's parameters re-runs every member whose argument
// flows read them. A callable with no parameters yields none.
func (h *hierarchy) parameters(callable Ref, yield func(paramInfo) error) error {
	return h.decls.lookup(keyParam+refKey(callable), func(e entry) error {
		if len(e.Aux) != 8 {
			return errMalformedEntry
		}
		return yield(paramInfo{Pos: int32(binary.BigEndian.Uint64([]byte(e.Aux))), Name: e.Sig, Index: e.MinArgs,
			Variadic: e.Flags&paramVariadic != 0, KeywordOnly: e.Flags&paramKeywordOnly != 0,
			Returned: e.Flags&paramReturned != 0})
	})
}

// externalName is the qualified name of the type written as written in a
// file of fc when it names a type outside the project: no declaration of
// the unit, through the file's scope and imports, is that type, and the
// language names it through an import, a qualified name or its implicit
// scope (the external key's qualified name, externalType). ok is false when
// written names a type of the unit, or nothing the language can name
// outside it.
//
// A qualified name whose first component an import binds is read with that
// component replaced by what the import denotes; a simple name an import
// binds is what the import denotes. Otherwise, by family:
//   - Java: a qualified name as written; a simple name is java.lang's (JLS
//     §7.3), unless an on-demand import may supply it;
//   - Go: a qualified name only through its import (a package outside the
//     unit's module); a simple name only when it is predeclared (`error`),
//     since any other is a type parameter or a dot import's;
//   - Python: a qualified name as written; a simple name is a builtin's
//     (`builtins.list`), unless a star import may supply it;
//   - ECMAScript: as written, a global (`Error`);
//   - Rust: a qualified name as written unless it starts in the crate
//     (`crate`, `self`, `super`, a crate of the unit); a global path
//     (`::std::fmt::Display`) without its leading `::` unless its crate is
//     the unit's (rustGlobalExternal); a simple name as written (the
//     prelude's), unless a glob of a path outside the crate may supply it;
//   - C and C++: a qualified name as written; a simple name only through a
//     using-declaration, since any other may be a template parameter.
//
// Every read goes through the indexes, so a member's trace records it.
func (h *hierarchy) externalName(ctx context.Context, fc fileCtx, written string) (qualified string, ok bool, err error) {
	name := typeName(written)
	if name == "" {
		return "", false, nil
	}
	inUnit := false
	if err := h.typesIn(ctx, fc, name, func(entry) error {
		inUnit = true
		return nil
	}); err != nil || inUnit {
		return "", false, err
	}
	type imported struct{ path, original string }
	var imps []imported
	collect := func(local string) error {
		imps = imps[:0]
		return h.decls.lookup(keyImport+refKey(Ref{File: fc.file})+local, func(e entry) error {
			imps = append(imps, imported{path: e.Aux, original: e.Sig})
			return nil
		})
	}
	if global, ok := strings.CutPrefix(name, "::"); ok && fc.family == "rust" {
		return h.rustGlobalExternal(global)
	}
	if head, rest, isQualified := strings.Cut(name, fc.sep); isQualified {
		if err := collect(head); err != nil {
			return "", false, err
		}
		if len(imps) > 1 {
			return "", false, nil
		}
		if len(imps) == 1 {
			return h.externalImported(fc, imps[0].path, imps[0].original, head, rest)
		}
		switch fc.family {
		case "go":
			return "", false, nil
		case "rust":
			if head == "crate" || head == "self" || head == "super" {
				return "", false, nil
			}
			crate := false
			if err := h.decls.lookup(keyRustCrate+head, func(entry) error {
				crate = true
				return nil
			}); err != nil || crate {
				return "", false, err
			}
		}
		return name, true, nil
	}
	if err := collect(name); err != nil {
		return "", false, err
	}
	if len(imps) > 1 {
		return "", false, nil
	}
	if len(imps) == 1 {
		return h.externalImported(fc, imps[0].path, imps[0].original, name, "")
	}
	switch fc.family {
	case "go":
		if goUniverseTypes[name] {
			return name, true, nil
		}
		return "", false, nil
	case "c":
		return "", false, nil
	case "js":
		return name, true, nil
	}
	// A name an import of every member of a path outside the unit may
	// supply is not known to be the implicit scope's.
	open := false
	err = h.decls.lookup(keyImport+refKey(Ref{File: fc.file})+ImportAll, func(e entry) error {
		if fc.family != "rust" || !rustInCrate(e.Aux) {
			open = true
		}
		return nil
	})
	if err != nil || open {
		return "", false, err
	}
	switch fc.family {
	case "java":
		return "java.lang." + name, true, nil
	case "python":
		return "builtins." + name, true, nil
	}
	return name, true, nil
}

// goUniverseTypes are Go's predeclared interface types ("Predeclared
// identifiers"), the only types outside every package a method call can be
// made on.
var goUniverseTypes = map[string]bool{"error": true, "any": true, "comparable": true}

// rustInCrate reports a Rust path that starts in the current crate.
func rustInCrate(p string) bool {
	head, _, _ := strings.Cut(p, "::")
	return head == "crate" || head == "self" || head == "super"
}

// externalImported is externalName of a name whose first component local an
// import of fc's file binds to path p (Import.Path), denoting original in p's
// module when the import renames it (Import.Original), followed by rest
// (empty for a simple name).
func (h *hierarchy) externalImported(fc fileCtx, p, original, local, rest string) (string, bool, error) {
	join := func(parts ...string) string {
		var kept []string
		for _, part := range parts {
			if part != "" {
				kept = append(kept, part)
			}
		}
		return strings.Join(kept, fc.sep)
	}
	switch fc.family {
	case "go":
		inUnit := false
		if err := h.goPackages(p, func(string) error {
			inUnit = true
			return nil
		}); err != nil || inUnit || rest == "" {
			return "", false, err
		}
		return p + "." + rest, true, nil
	case "python":
		if strings.HasPrefix(p, ".") {
			return "", false, nil
		}
		switch {
		case original == "*" && rest == "":
			// `import m as local` binds a module, never a type.
			return "", false, nil
		case original == "*":
			return join(p, rest), true, nil
		case rest == "":
			// `from m import C [as D]`.
			return join(p, cmp.Or(original, local)), true, nil
		case original == "" && (p == local || strings.HasPrefix(p, local+".")):
			// `import a.b`, binding a: the name is written in full.
			return join(local, rest), true, nil
		case original != "":
			return join(p, original, rest), true, nil
		}
		// `from m import sub`, a submodule: its names are m.sub's.
		return join(p, local, rest), true, nil
	case "js":
		if outside, err := h.esOutside(fc.path, p); err != nil || !outside {
			return "", false, err
		}
		switch original {
		case "*":
			if rest == "" {
				return "", false, nil
			}
			return join(p, rest), true, nil
		case "":
			return join(p, local, rest), true, nil
		}
		return join(p, original, rest), true, nil
	case "rust":
		if rustInCrate(p) {
			return "", false, nil
		}
		return join(p, rest), true, nil
	case "java", "c":
		if fc.family == "c" && strings.HasPrefix(p, "<") || p == "" {
			return "", false, nil
		}
		return join(p, rest), true, nil
	}
	return "", false, nil
}

// externalInterface reports that the source proves an external supertype
// of a type whose entry carries flags, of family, an interface or trait: an
// interface's or trait's supertype, a Rust impl's trait, or a supertype row
// marked wire.Supertype.Interface, whose entry carries flagInterface.
func externalInterface(family string, flags byte) bool {
	return flags&flagInterface != 0 || family == "rust"
}

// externalSupers is every supertype outside the unit of the type typ of
// family that may declare method, each once (externalSupertypes), and for
// Java java.lang.Object when none of them is a class and method is one of
// Object's (JLS §8.1.4: a class with no superclass of the unit that names
// one outside it extends Object, and an interface has Object's public
// members, §9.2). A Go type's are those it embeds, directly or by promotion.
func (h *hierarchy) externalSupers(family string, typ Ref, method string) ([]entry, error) {
	var out []entry
	err := h.externalSupertypes(typ, family != "go", func(e entry) error {
		out = append(out, e)
		return nil
	})
	if err != nil {
		return nil, err
	}
	if family == "java" && javaObjectMethods[method] && !slices.ContainsFunc(out, func(e entry) bool { return e.Flags&flagInterface == 0 }) {
		out = append(out, entry{Aux: "java.lang.Object"})
	}
	return out, nil
}

// externalSupertypes yields each supertype outside the unit (keyExternalSuper)
// of typ and, when along, of each of its supertypes of the unit, breadth
// first, each qualified name once: its entry's Aux is the qualified name
// (externalName) and its Flags hold flagInterface when the source proves it
// an interface or trait.
func (h *hierarchy) externalSupertypes(typ Ref, along bool, yield func(entry) error) error {
	names := map[string]bool{}
	seen := map[Ref]bool{typ: true}
	queue := []Ref{typ}
	for len(queue) > 0 {
		t := queue[0]
		queue = queue[1:]
		err := h.links.lookup(keyExternalSuper+refKey(t), func(e entry) error {
			if names[e.Aux] {
				return nil
			}
			names[e.Aux] = true
			return yield(e)
		})
		if err != nil || !along {
			return err
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

// javaObjectMethods are the methods java.lang.Object declares (JLS §4.3.2).
var javaObjectMethods = map[string]bool{"clone": true, "equals": true, "finalize": true, "getClass": true,
	"hashCode": true, "notify": true, "notifyAll": true, "toString": true, "wait": true}

// hierarchyFacts emits the hierarchy facts of the declarations of the member
// at ordinal file, whose table is t: FactExtends and FactImplements for each
// type's supertypes, FactOverrides for each method that directly overrides
// or implements a supertype's (ADR-0015 decision 2), with the reads the trace
// records.
//
// A type extends each declared supertype that is a class, or that is an
// interface when it is one itself (a Java or TypeScript interface's
// extended interfaces, a Rust supertrait, a Go interface's embedded
// interfaces), and implements each other interface or trait it declares, or
// that a Go type satisfies by its method set (DetailMethodSet). A Go struct's
// embedded types are promotion and no supertype. A supertype outside the
// unit is its external type node (externalType).
//
// A method overrides, along each of its type's direct supertypes, the
// nearest method of its name that it can override: in Java one that is
// neither static, final nor private, of the same signature; in C++ a virtual
// one of the same signature; in Python the first along the method
// resolution order; in Rust a trait's, by a method of the trait's impl
// (Decl.Override); in ECMAScript one of the same name; in Go each method of
// an interface its type satisfies. A method that overrides none of the
// unit's and that the language marks as overriding (Decl.Override) overrides
// the method of its name of the one supertype outside the unit its type has
// (externalSupers), none when it has several. Go publishes nothing toward a
// type outside the unit, whose method set no table holds.
//
// The file's request inputs Spring decides by type (springInputFacts), the
// routes its Go and Express root mounts reach in other files (goRouteFacts,
// jsRouteFacts) and the reads of its functions a route of another file
// names as Express handlers (jsHandlerInputs) are emitted here too, on the
// file's own reads of the indexes.
func (h *hierarchy) hierarchyFacts(ctx context.Context, file int32, t *Table, emit func(Binding) error) error {
	fc := contextOf(file, t)
	if err := h.springInputFacts(ctx, file, t, emit); err != nil {
		return err
	}
	if err := h.goRouteFacts(ctx, file, t, emit); err != nil {
		return err
	}
	if err := h.jsRouteFacts(ctx, file, t, emit); err != nil {
		return err
	}
	if err := h.jsHandlerInputs(ctx, file, t, emit); err != nil {
		return err
	}
	for i := range t.Decls {
		if err := ctx.Err(); err != nil {
			return err
		}
		d := &t.Decls[i]
		ref := Ref{File: file, Index: int32(i)}
		var err error
		switch {
		case isType(d.Kind) && fc.family == "go":
			err = h.goSupertypeFacts(ctx, fc, t, ref, d, emit)
		case isType(d.Kind):
			err = h.supertypeFacts(fc, ref, d, emit)
		case isCallable(d.Kind):
			err = h.overrideFacts(ctx, fc, t, ref, d, emit)
		}
		if err == nil {
			err = h.macroInputFacts(fc, ref, d, emit)
		}
		if err == nil && d.MainInherited {
			err = h.javaMainFacts(ref, emit)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// supertypeFacts emits the supertype facts of the type d at ref of a family
// but Go: its declared supertypes of the unit (keySuper) and outside it
// (keyExternalSuper), wherever the unit declares them (a Rust impl in another
// file).
func (h *hierarchy) supertypeFacts(fc fileCtx, ref Ref, d *Decl, emit func(Binding) error) error {
	relation := func(super byte) Fact {
		if super&flagInterface != 0 && d.Kind != model.NodeInterface {
			return FactImplements
		}
		return FactExtends
	}
	err := h.links.lookup(keySuper+refKey(ref), func(e entry) error {
		return emit(Binding{Kind: relation(e.Flags), Site: ref, Target: e.Ref, Detail: DetailSupertype})
	})
	if err != nil {
		return err
	}
	return h.links.lookup(keyExternalSuper+refKey(ref), func(e entry) error {
		detail := DetailSupertypeClass
		switch {
		case anonymousClass(d):
			detail = DetailSupertypeAnonymous
		case e.Flags&flagInterface != 0:
			detail = DetailSupertypeInterface
		}
		return emit(Binding{Kind: relation(e.Flags), Site: ref, Detail: detail, External: externalType(fc.family, e.Aux)})
	})
}

// goSupertypeFacts emits the supertype facts of the Go type d at ref: an
// interface extends each interface it embeds, of the unit or outside it, and
// a type that is no interface implements each interface its method set
// satisfies.
func (h *hierarchy) goSupertypeFacts(ctx context.Context, fc fileCtx, t *Table, ref Ref, d *Decl, emit func(Binding) error) error {
	if d.Kind != model.NodeInterface {
		err := h.links.lookup(keySuper+refKey(ref), func(e entry) error {
			if e.Aux != "" {
				return nil
			}
			return emit(Binding{Kind: FactImplements, Site: ref, Target: e.Ref, Detail: DetailMethodSet})
		})
		if err != nil {
			return err
		}
		return h.goExternalImplements(fc, ref, emit)
	}
	for _, st := range t.Supertypes {
		sub := false
		if err := h.typesIn(ctx, fc, st.Type, func(e entry) error {
			sub = sub || e.Ref == ref
			return nil
		}); err != nil {
			return err
		}
		if !sub {
			continue
		}
		found := false
		err := h.typesIn(ctx, fc, st.Super, func(e entry) error {
			found = true
			if e.Ref == ref || e.Flags&flagInterface == 0 {
				return nil
			}
			return emit(Binding{Kind: FactExtends, Site: ref, Target: e.Ref, Detail: DetailSupertype})
		})
		if err != nil || found {
			if err != nil {
				return err
			}
			continue
		}
		q, ok, err := h.externalName(ctx, fc, st.Super)
		if err != nil {
			return err
		}
		if ok {
			err := emit(Binding{Kind: FactExtends, Site: ref, Detail: DetailSupertypeInterface,
				External: externalType(fc.family, q)})
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// owners yields the types the method d of t, in context fc, is declared on:
// its enclosing type, a Java anonymous class body's synthetic type, or the
// types its receiver names.
func (h *hierarchy) owners(ctx context.Context, fc fileCtx, t *Table, d *Decl, yield func(Ref) error) error {
	if p := d.Parent; p >= 0 && int(p) < len(t.Decls) && isType(t.Decls[p].Kind) && !anonymousReceiver(fc, d) {
		return yield(Ref{File: fc.file, Index: p})
	}
	if anonymousReceiver(fc, d) {
		ref, _ := anonymousType(fc, d.Receiver)
		return yield(ref)
	}
	if d.Receiver == "" {
		return nil
	}
	return h.typesIn(ctx, fc, d.Receiver, func(e entry) error { return yield(e.Ref) })
}

// overrideFacts emits what the method d at ref directly overrides
// (hierarchyFacts).
func (h *hierarchy) overrideFacts(ctx context.Context, fc fileCtx, t *Table, ref Ref, d *Decl, emit func(Binding) error) error {
	switch {
	case d.Static && (fc.family == "java" || fc.family == "c"):
		// A static method hides; it overrides nothing.
		return nil
	case fc.family == "rust":
		// Only an item of a trait's impl implements, and only its trait's.
		return h.rustTraitOverrides(ctx, fc, ref, d, emit)
	case fc.family == "js" && strings.HasPrefix(d.Name, "#"):
		// A private method's name is its class body's alone (ECMA-262
		// §15.7.1, PrivateName): it overrides nothing.
		return nil
	case fc.family == "go" && d.Parent >= 0 && int(d.Parent) < len(t.Decls) && t.Decls[d.Parent].Kind == model.NodeInterface:
		// An interface's method specification implements nothing.
		return nil
	}
	sig := declEntry(ref, d).Sig
	return h.owners(ctx, fc, t, d, func(owner Ref) error {
		var targets []Ref
		add := func(e entry) {
			if e.Ref != ref && !slices.Contains(targets, e.Ref) {
				targets = append(targets, e.Ref)
			}
		}
		detail := DetailSupertype
		switch fc.family {
		case "go":
			detail = DetailMethodSet
			err := h.links.lookup(keySuper+refKey(owner), func(iface entry) error {
				if iface.Aux != "" || iface.Ref == owner {
					return nil
				}
				return h.methods(iface.Ref, d.Name, func(e entry) error {
					add(e)
					return nil
				})
			})
			if err != nil {
				return err
			}
		default:
			full, err := h.pythonFullMRO(owner)
			if err != nil {
				return err
			}
			if full != nil {
				// The first class along the order, bases outside the unit
				// placed, that defines the method (PEP 698); a base outside
				// the unit ahead of it is externalOverrides'.
				for _, base := range full[1:] {
					if base.external != "" {
						continue
					}
					if err := h.methods(base.ref, d.Name, func(e entry) error {
						add(e)
						return nil
					}); err != nil {
						return err
					}
					if len(targets) > 0 {
						break
					}
				}
				break
			}
			order, err := h.pythonMRO(owner)
			if err != nil {
				return err
			}
			if order != nil {
				for _, base := range order[1:] {
					if err := h.methods(base, d.Name, func(e entry) error {
						add(e)
						return nil
					}); err != nil || len(targets) > 0 {
						if err != nil {
							return err
						}
						break
					}
				}
				break
			}
			err = h.links.lookup(keySuper+refKey(owner), func(super entry) error {
				if fc.family == "c" {
					// [class.virtual]: only a virtual member is overridden.
					if v, err := h.virtual(super.Ref, d.Name); err != nil || !v {
						return err
					}
				}
				return h.declared(super.Ref, d.Name, func(e entry) error {
					if overrides(fc.family, sig, e) {
						add(e)
					}
					return nil
				})
			})
			if err != nil {
				return err
			}
		}
		for _, target := range targets {
			if err := emit(Binding{Kind: FactOverrides, Site: ref, Target: target, Detail: detail}); err != nil {
				return err
			}
		}
		if fc.family == "go" {
			return h.goExternalOverrides(fc, owner, ref, d, emit)
		}
		return h.externalOverrides(fc, owner, ref, d, len(targets) > 0, emit)
	})
}

// overrides reports that a method of signature sig, of family, can override
// the supertype's method e of its name: in Java one that is neither static
// nor final nor private (JLS §8.4.8.1), and in Java and C++ one of the same
// signature when both are spelled.
func overrides(family, sig string, e entry) bool {
	switch family {
	case "java":
		if e.Flags&(flagStatic|flagFinal) != 0 {
			return false
		}
	case "c":
		if e.Flags&flagStatic != 0 {
			return false
		}
	default:
		return true
	}
	return sig == "" || e.Sig == "" || sig == e.Sig
}
