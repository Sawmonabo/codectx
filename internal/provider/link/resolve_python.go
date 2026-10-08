package link

import (
	"cmp"
	"context"
	"encoding/json"
	"path"
	"path/filepath"
	"strings"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/wire"
)

// resolvePython resolves the sites of a Python unit: direct binding through
// imports and modules, a call of a class as its construction, flow-based type
// inference (inferTypes), field-based resolution and class-hierarchy
// analysis in method resolution order.
//
// It reads the unit's tables once. Each site that stays inside its file (a
// bare call of a name the file declares, a receiver whose type the file or
// the inference recovered, a called value the inference traced) binds while
// its table is in hand. A site that names a module (`mod.f()`, a name a
// `from mod import f` introduced, which the worker spells as a TypeModule)
// becomes a lookup that the unit's declarations answer through one ordered
// merge: every declaration is indexed under each dotted module name its path
// can be imported as, and both kinds of record are sorted together, so a
// lookup meets the declarations of its key with nothing else resident.
func resolvePython(ctx context.Context, u *unit, emit func(Binding) error) error {
	h, err := buildHierarchy(ctx, u)
	if err != nil {
		return err
	}
	defer h.close()
	t, err := inferTypes(ctx, u, h)
	if err != nil {
		return err
	}
	defer t.close()
	keys, err := pagination.NewExternalSort(filepath.Join(u.dir, "python-modules"), 0,
		encodeJSON[pyRecord], decodeJSON[pyRecord], comparePyRecord)
	if err != nil {
		return err
	}
	defer keys.Close()
	err = u.Files(ctx, func(file int32, fv model.FileVersion, tab *Table) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := indexPythonDecls(file, tab, keys); err != nil {
			return err
		}
		for i := range tab.Sites {
			site := Ref{File: file, Index: int32(i)}
			if err := resolvePythonSite(ctx, h, t, site, tab, keys, emit); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	return joinPythonModules(ctx, u.dir, keys, emit)
}

// pyRecord is one record of the module merge: a declaration indexed under one
// dotted module name its file can be imported as, or a site's lookup of a
// name in a module. Records of one Key meet in the merge, declarations first.
type pyRecord struct {
	// Key is the dotted module name and the qualified name inside it,
	// joined by a NUL: "pkg.mod\x00Class.method".
	Key string `json:"k"`
	// Lookup marks a site's lookup; a declaration otherwise.
	Lookup bool `json:"l,omitempty"`
	// Ref is the declaration, or the looking-up site.
	Ref Ref `json:"r"`
	// Root is, for a declaration, the directory its module name is read
	// from ("" for the repository root, else ending in "/"); for a lookup,
	// the looking-up file's path.
	Root string `json:"o,omitempty"`
	// Class marks a declaration of a class.
	Class bool `json:"c,omitempty"`
	// Init marks a lookup of a constructed class's `__init__`.
	Init bool `json:"i,omitempty"`
	// Exact marks a relative import's lookup, whose module is named from
	// the repository root and answered by that root alone.
	Exact bool `json:"x,omitempty"`
}

func comparePyRecord(a, b pyRecord) int {
	if c := strings.Compare(a.Key, b.Key); c != 0 {
		return c
	}
	if a.Lookup != b.Lookup {
		if !a.Lookup {
			return -1
		}
		return 1
	}
	if c := compareRef(a.Ref, b.Ref); c != 0 {
		return c
	}
	if c := strings.Compare(a.Root, b.Root); c != 0 {
		return c
	}
	if c := cmpBool(a.Class, b.Class); c != 0 {
		return c
	}
	if c := cmpBool(a.Init, b.Init); c != 0 {
		return c
	}
	return cmpBool(a.Exact, b.Exact)
}

// pyHit is one declaration a module lookup met.
type pyHit struct {
	Site   Ref  `json:"s"`
	Target Ref  `json:"t"`
	Init   bool `json:"i,omitempty"`
	Class  bool `json:"c,omitempty"`
	// Near reports that the declaration's module is read from a directory
	// the looking-up file sits under, the import's own source root.
	Near  bool `json:"n,omitempty"`
	Exact bool `json:"x,omitempty"`
}

func comparePyHit(a, b pyHit) int {
	if c := compareRef(a.Site, b.Site); c != 0 {
		return c
	}
	if c := compareRef(a.Target, b.Target); c != 0 {
		return c
	}
	for _, c := range []int{cmpBool(a.Init, b.Init), cmpBool(a.Class, b.Class), cmpBool(a.Near, b.Near), cmpBool(a.Exact, b.Exact)} {
		if c != 0 {
			return c
		}
	}
	return 0
}

func compareRef(a, b Ref) int {
	if c := cmp.Compare(a.File, b.File); c != 0 {
		return c
	}
	return cmp.Compare(a.Index, b.Index)
}

func cmpBool(a, b bool) int {
	switch {
	case a == b:
		return 0
	case !a:
		return -1
	}
	return 1
}

func encodeJSON[T any](v T) ([]byte, error) { return json.Marshal(v) }

func decodeJSON[T any](b []byte) (T, error) {
	var v T
	err := json.Unmarshal(b, &v)
	return v, err
}

// pythonModuleNames yields each dotted module name the file at p can be
// imported as, with the directory that name is read from: "src/pkg/mod.py"
// is "src.pkg.mod" from the root, "pkg.mod" from "src/" and "mod" from
// "src/pkg/". A package's `__init__` is the package itself. The source roots
// a project puts on its import path are not in its tables, so every directory
// the path passes through is one.
func pythonModuleNames(p string, yield func(name, root string) error) error {
	segs := pythonModulePath(p)
	for i := range segs {
		root := ""
		if i > 0 {
			root = strings.Join(segs[:i], "/") + "/"
		}
		if err := yield(strings.Join(segs[i:], "."), root); err != nil {
			return err
		}
	}
	return nil
}

// pythonModulePath is the module path of the file at p as path segments,
// empty when the file is the repository root's own package.
func pythonModulePath(p string) []string {
	p = strings.TrimSuffix(strings.TrimSuffix(p, ".pyi"), ".py")
	segs := strings.Split(p, "/")
	if segs[len(segs)-1] == "__init__" {
		segs = segs[:len(segs)-1]
	}
	return segs
}

// indexPythonDecls adds every declaration of tab under each module name its
// file can be imported as, keyed by its qualified name: a module-level
// function or class by its name, a method by "Class.method".
func indexPythonDecls(file int32, tab *Table, keys *pagination.ExternalSort[pyRecord]) error {
	return pythonModuleNames(tab.Path, func(name, root string) error {
		for i, d := range tab.Decls {
			if d.Qualified == "" {
				continue
			}
			rec := pyRecord{Key: name + "\x00" + d.Qualified, Ref: Ref{File: file, Index: int32(i)}, Root: root,
				Class: d.Kind == model.NodeClass}
			if err := keys.Add(rec); err != nil {
				return err
			}
		}
		return nil
	})
}

// resolvePythonSite binds one site of tab, the table of site.File: what the
// file and the inference answer is emitted here, and a module lookup is added
// to keys for the merge.
func resolvePythonSite(ctx context.Context, h *hierarchy, t *types, site Ref, tab *Table,
	keys *pagination.ExternalSort[pyRecord], emit func(Binding) error) error {
	s := &tab.Sites[site.Index]
	bind := func(detail string) func(Ref) error {
		return func(target Ref) error { return emit(Binding{Site: site, Target: target, Detail: detail}) }
	}
	if s.Receiver.Kind == wire.TypeModule {
		// A name an import introduced: a module (`import a.b as m; m.f()`)
		// or, on a bare site, the imported entity itself (`from a.b import
		// f; f()`, spelled "a.b.f").
		return lookupPython(site, tab.Path, s.Shape == wire.ShapeBare, s.Receiver.Name, s.Callee, keys)
	}
	switch s.Shape {
	case wire.ShapeBare, wire.ShapeConstructor:
		for _, c := range s.Candidates {
			if err := emit(Binding{Site: site, Target: Ref{File: site.File, Index: constructed(tab, c)}, Detail: DetailDirect}); err != nil {
				return err
			}
		}
	case wire.ShapeStatic:
		// `mod.f()` through an import, or `C.m()` through a class.
		if err := lookupPython(site, tab.Path, false, s.Qualifier, s.Callee, keys); err != nil {
			return err
		}
		if err := h.types(ctx, site.File, s.Qualifier, func(typ Ref) error {
			return h.dispatch(ctx, typ, s.Callee, bind(DetailHierarchy))
		}); err != nil {
			return err
		}
	case wire.ShapeMember:
		if err := resolvePythonReceiver(ctx, h, t, site, tab, s, bind); err != nil {
			return err
		}
	}
	if s.Shape == wire.ShapeStatic {
		return nil
	}
	return t.functions(ctx, site, func(target Ref, detail string) error {
		return emit(Binding{Site: site, Target: target, Detail: detail})
	})
}

// resolvePythonReceiver binds a member site by its receiver's types, each
// dispatched in the class hierarchy: a receiver the file names (`self`,
// `cls`, an annotation) by class-hierarchy analysis, one the inference
// recovered (a parameter, another call's result, a field) by flow, and
// `super()` by the bases of the enclosing class.
func resolvePythonReceiver(ctx context.Context, h *hierarchy, t *types, site Ref, tab *Table, s *Site,
	bind func(string) func(Ref) error) error {
	switch s.Receiver.Kind {
	case wire.TypeNamed:
		return h.types(ctx, site.File, s.Receiver.Name, func(typ Ref) error {
			return h.dispatch(ctx, typ, s.Callee, bind(DetailHierarchy))
		})
	case wire.TypeSuper:
		return resolvePythonSuper(ctx, h, site, tab, s, bind(DetailHierarchy))
	}
	return t.receivers(ctx, site, func(typ Ref) error {
		return h.dispatch(ctx, typ, s.Callee, bind(DetailFlow))
	})
}

// resolvePythonSuper binds `super().m()` in class B: m as each base of B
// declares or inherits it, and its overrides in their subtypes except the
// one B declares, which follows B in no method resolution order.
func resolvePythonSuper(ctx context.Context, h *hierarchy, site Ref, tab *Table, s *Site, bind func(Ref) error) error {
	class := enclosingClass(tab, s.Caller)
	for _, st := range tab.Supertypes {
		if st.Type != s.Receiver.Name {
			continue
		}
		if err := h.types(ctx, site.File, st.Super, func(typ Ref) error {
			return h.dispatch(ctx, typ, s.Callee, func(target Ref) error {
				if class >= 0 && target.File == site.File && tab.Decls[target.Index].Parent == class {
					return nil
				}
				return bind(target)
			})
		}); err != nil {
			return err
		}
	}
	return nil
}

// enclosingClass is the class declaration enclosing declaration i of tab, or
// -1.
func enclosingClass(tab *Table, i int32) int32 {
	for i >= 0 && int(i) < len(tab.Decls) {
		if tab.Decls[i].Kind == model.NodeClass {
			return i
		}
		i = tab.Decls[i].Parent
	}
	return -1
}

// constructed is what a bare call of declaration c of tab calls: the
// `__init__` a class declares, else c itself.
func constructed(tab *Table, c int32) int32 {
	if tab.Decls[c].Kind != model.NodeClass {
		return c
	}
	for i, d := range tab.Decls {
		if d.Parent == c && d.Name == "__init__" {
			return int32(i)
		}
	}
	return c
}

// lookupPython adds the lookups of a site through the module name module, as
// imported: for a bare site, module names the imported entity itself, so the
// callee is its last component. Each lookup also asks for the `__init__` of
// what it finds, which a construction binds instead of a class. A dotted name
// can also end in a class (`from pkg import C; C.m()`), so its last component
// is tried as one too.
func lookupPython(site Ref, importer string, bare bool, module, callee string, keys *pagination.ExternalSort[pyRecord]) error {
	if module == "" {
		return nil
	}
	exact := strings.HasPrefix(module, ".")
	if exact {
		if module = absolutePythonModule(importer, module); module == "" {
			return nil
		}
	}
	add := func(mod, qualified string) error {
		if mod == "" {
			return nil
		}
		for _, init := range []bool{false, true} {
			q := qualified
			if init {
				q += ".__init__"
			}
			if err := keys.Add(pyRecord{Key: mod + "\x00" + q, Lookup: true, Ref: site, Root: importer, Init: init, Exact: exact}); err != nil {
				return err
			}
		}
		return nil
	}
	mod, last := module, ""
	if i := strings.LastIndexByte(module, '.'); i >= 0 {
		mod, last = module[:i], module[i+1:]
	}
	if bare {
		if last == "" {
			return nil
		}
		return add(mod, last)
	}
	if err := add(module, callee); err != nil {
		return err
	}
	if last != "" {
		return add(mod, last+"."+callee)
	}
	return nil
}

// absolutePythonModule is the relative module rel (".mod", "..pkg.mod", ".")
// imported by the file at importer, named from the repository root, or empty
// when it climbs above the root.
func absolutePythonModule(importer, rel string) string {
	dots := len(rel) - len(strings.TrimLeft(rel, "."))
	dir := strings.Split(path.Dir(importer), "/")
	if path.Dir(importer) == "." {
		dir = nil
	}
	if dots-1 > len(dir) {
		return ""
	}
	segs := dir[:len(dir)-(dots-1)]
	if rest := rel[dots:]; rest != "" {
		segs = append(segs, strings.Split(rest, ".")...)
	}
	return strings.Join(segs, ".")
}

// joinPythonModules merges the module index with the lookups. Each key's
// declarations precede its lookups, and only one key's declarations are
// held. The hits are sorted by site and settled per site: a declaration read
// from the looking-up file's own source root is preferred over one read from
// another, a relative import takes only the root's, and a class whose
// `__init__` was found is constructed through it.
func joinPythonModules(ctx context.Context, dir string, keys *pagination.ExternalSort[pyRecord], emit func(Binding) error) error {
	hits, err := pagination.NewExternalSort(filepath.Join(dir, "python-hits"), 0,
		encodeJSON[pyHit], decodeJSON[pyHit], comparePyHit)
	if err != nil {
		return err
	}
	defer hits.Close()
	sorted, err := keys.Sorted()
	if err != nil {
		return err
	}
	defer sorted.Close()
	var key string
	var decls []pyRecord
	err = sorted.Each(func(r pyRecord) error {
		if r.Key != key {
			if err := ctx.Err(); err != nil {
				return err
			}
			key, decls = r.Key, decls[:0]
		}
		if !r.Lookup {
			decls = append(decls, r)
			return nil
		}
		for _, d := range decls {
			near := strings.HasPrefix(r.Root, d.Root)
			if r.Exact {
				near = d.Root == ""
			}
			if err := hits.Add(pyHit{Site: r.Ref, Target: d.Ref, Init: r.Init, Class: d.Class, Near: near, Exact: r.Exact}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	settled, err := hits.Sorted()
	if err != nil {
		return err
	}
	defer settled.Close()
	var group []pyHit
	err = settled.Each(func(x pyHit) error {
		if len(group) > 0 && group[0].Site != x.Site {
			if err := settlePythonSite(group, emit); err != nil {
				return err
			}
			group = group[:0]
		}
		group = append(group, x)
		return nil
	})
	if err != nil {
		return err
	}
	return settlePythonSite(group, emit)
}

// settlePythonSite emits one site's module hits: those read from the
// looking-up file's source root when any is, else every hit of a non-relative
// import; and of those, no class whose file supplied the class's `__init__`.
func settlePythonSite(group []pyHit, emit func(Binding) error) error {
	near := false
	for _, x := range group {
		near = near || x.Near
	}
	kept := group[:0:0]
	for _, x := range group {
		if x.Near || (!near && !x.Exact) {
			kept = append(kept, x)
		}
	}
	for _, x := range kept {
		if x.Class && !x.Init && initFrom(kept, x.Target.File) {
			continue
		}
		if err := emit(Binding{Site: x.Site, Target: x.Target, Detail: DetailDirect}); err != nil {
			return err
		}
	}
	return nil
}

// initFrom reports an `__init__` hit among hits declared in file.
func initFrom(hits []pyHit, file int32) bool {
	for _, x := range hits {
		if x.Init && x.Target.File == file {
			return true
		}
	}
	return false
}
