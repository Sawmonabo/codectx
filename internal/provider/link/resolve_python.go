package link

import (
	"context"
	"encoding/json"
	"path"
	"path/filepath"
	"slices"
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
// to keys for the merge. Field-based resolution (a function written to the
// field a member site calls through) answers a member site only when its
// receiver's type bound nothing: a type that declares or inherits the method
// decides the call, and one that does not leaves an instance attribute
// (`self.cb = f; self.cb()`), which only the field can tell.
func resolvePythonSite(ctx context.Context, h *hierarchy, t *types, site Ref, tab *Table,
	keys *pagination.ExternalSort[pyRecord], emit func(Binding) error) error {
	s := &tab.Sites[site.Index]
	bound := false
	bind := func(detail string) func(Ref) error {
		return func(target Ref) error {
			bound = true
			return emit(Binding{Site: site, Target: target, Detail: detail})
		}
	}
	if s.Receiver.Kind == wire.TypeModule {
		// A name an import introduced: a module (`import a.b as m; m.f()`)
		// or, on a bare site, the imported entity itself (`from a.b import
		// f; f()`, spelled "a.b.f").
		return lookupPython(site, tab.Path, s.Shape == wire.ShapeBare, s.Receiver.Name, s.Callee, keys)
	}
	switch s.Shape {
	case wire.ShapeBare, wire.ShapeConstructor:
		for _, c := range visibleCandidates(tab, s) {
			if err := bind(DetailDirect)(Ref{File: site.File, Index: constructed(tab, c)}); err != nil {
				return err
			}
		}
	case wire.ShapeStatic:
		// `mod.f()` through an import, or `mod.C.m()` through a class of a
		// module, which calls the method C declares or inherits.
		if err := lookupPython(site, tab.Path, false, s.Qualifier, s.Callee, keys); err != nil {
			return err
		}
		return h.types(ctx, site.File, s.Qualifier, func(typ Ref) error {
			return h.declared(typ, s.Callee, entryRef(bind(DetailDirect)))
		})
	case wire.ShapeMember:
		if err := resolvePythonReceiver(ctx, h, t, site, tab, s, bind); err != nil {
			return err
		}
	}
	return t.functions(ctx, site, func(target Ref, detail string) error {
		if detail == DetailField && s.Shape == wire.ShapeMember && bound {
			return nil
		}
		return emit(Binding{Site: site, Target: target, Detail: detail})
	})
}

// visibleCandidates are the in-file candidates of bare site s that its name
// can denote where it is written: those of the innermost scope enclosing the
// call that declares one (Table.Visible), or, in a class body's own code, a
// def or class that body declares (the class namespace that code reads
// first). A same-named declaration of another function, or a method, is not
// what a bare name denotes.
func visibleCandidates(tab *Table, s *Site) []int32 {
	var out []int32
	best := -1
	for _, c := range s.Candidates {
		if c < 0 || int(c) >= len(tab.Decls) {
			continue
		}
		p := tab.Decls[c].Parent
		inClassBody := p >= 0 && p == s.Caller && isType(tab.Decls[p].Kind)
		if !inClassBody && !tab.Visible(c, s.Caller) {
			continue
		}
		// depth is how many scopes out from the caller the candidate's
		// scope is; the module is the outermost.
		depth := 0
		for i := s.Caller; i >= 0 && i != p && int(i) < len(tab.Decls); i = tab.Decls[i].Parent {
			depth++
		}
		switch {
		case best < 0 || depth < best:
			best, out = depth, append(out[:0], c)
		case depth == best:
			out = append(out, c)
		}
	}
	return out
}

// entryRef adapts a binder of Refs to the hierarchy's entries.
func entryRef(bind func(Ref) error) func(entry) error {
	return func(e entry) error { return bind(e.Ref) }
}

// resolvePythonReceiver binds a member site by its receiver's types: a
// receiver the file names as an instance (`self`, an annotation) by
// class-hierarchy analysis in method resolution order (dispatchPython), a
// class object (`A.make()`, `cls.make()` of a classmethod's class written
// as a class) by the method that class declares or inherits alone, one the
// inference recovered (a parameter, another call's result, a field) by flow,
// and `super()` by what follows the enclosing class in method resolution
// order.
func resolvePythonReceiver(ctx context.Context, h *hierarchy, t *types, site Ref, tab *Table, s *Site,
	bind func(string) func(Ref) error) error {
	switch s.Receiver.Kind {
	case wire.TypeNamed:
		return h.types(ctx, site.File, s.Receiver.Name, func(typ Ref) error {
			return dispatchPython(ctx, h, typ, s.Callee, bind(DetailHierarchy))
		})
	case wire.TypeClass:
		return h.types(ctx, site.File, s.Receiver.Name, func(typ Ref) error {
			return h.declared(typ, s.Callee, entryRef(bind(DetailDirect)))
		})
	case wire.TypeSuper:
		return resolvePythonSuper(ctx, h, site, tab, s, bind(DetailHierarchy))
	}
	// The inference's receiver values: an instance dispatches, and a
	// constructed instance (valObject) or a class object (valExact) calls
	// its type's own or inherited method.
	return t.seek(site, func(r recv) error {
		if r.Role != roleReceiver {
			return nil
		}
		switch r.X.Kind {
		case valInstance:
			return dispatchPython(ctx, h, r.X.Ref, s.Callee, bind(DetailFlow))
		case valObject, valExact:
			return h.declared(r.X.Ref, s.Callee, entryRef(bind(DetailFlow)))
		}
		return nil
	})
}

// dispatchPython yields every method a call of method on an instance of the
// Python class typ may run: for typ and each of its subtypes, the
// declaration that type's method resolution order reaches first
// (hierarchy.declared), each once. A method declared abstract is yielded
// only when no type reaches one that is not.
func dispatchPython(ctx context.Context, h *hierarchy, typ Ref, method string, yield func(Ref) error) error {
	var concrete, abstract []Ref
	seen := map[Ref]bool{}
	err := pythonSubtypes(ctx, h, typ, func(sub Ref) error {
		return h.declared(sub, method, func(e entry) error {
			if seen[e.Ref] {
				return nil
			}
			seen[e.Ref] = true
			if e.Flags&flagAbstract != 0 {
				abstract = append(abstract, e.Ref)
			} else {
				concrete = append(concrete, e.Ref)
			}
			return nil
		})
	})
	if err != nil {
		return err
	}
	if len(concrete) == 0 {
		concrete = abstract
	}
	for _, m := range concrete {
		if err := yield(m); err != nil {
			return err
		}
	}
	return nil
}

// pythonSubtypes yields typ and every class below it in the unit's
// hierarchy, each once.
func pythonSubtypes(ctx context.Context, h *hierarchy, typ Ref, yield func(Ref) error) error {
	seen := map[Ref]bool{typ: true}
	queue := []Ref{typ}
	for len(queue) > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		t := queue[0]
		queue = queue[1:]
		if err := yield(t); err != nil {
			return err
		}
		err := h.links.lookup(keySub+refKey(t), func(e entry) error {
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

// resolvePythonSuper binds `super().m()` written in class B: for B and each
// class below it, the instance's possible types, the first declaration of m
// that type's method resolution order reaches after B. A class below B
// precedes B in every such order, so no override below B is reached, while a
// sibling base a subclass lists after B is (cooperative multiple
// inheritance). A type whose bases admit no order reaches nothing; for B
// itself the nearest declaration along each of its bases' paths answers.
func resolvePythonSuper(ctx context.Context, h *hierarchy, site Ref, tab *Table, s *Site, bind func(Ref) error) error {
	class := enclosingClass(tab, s.Caller)
	if class < 0 {
		return nil
	}
	b := Ref{File: site.File, Index: class}
	return pythonSubtypes(ctx, h, b, func(sub Ref) error {
		order, err := h.pythonMRO(sub)
		if err != nil {
			return err
		}
		if order == nil {
			if sub != b {
				return nil
			}
			return h.links.lookup(keySuper+refKey(b), func(base entry) error {
				return h.declared(base.Ref, s.Callee, entryRef(bind))
			})
		}
		after := slices.Index(order, b) + 1
		if after == 0 {
			return nil
		}
		for _, t := range order[after:] {
			found := false
			err := h.methods(t, s.Callee, func(e entry) error {
				found = true
				return bind(e.Ref)
			})
			if err != nil || found {
				return err
			}
		}
		return nil
	})
}

// pythonMRO is the method resolution order of typ when typ is a Python
// class: its C3 linearisation (the Python Language Reference §3.3 and "The
// Python 2.3 Method Resolution Order"), typ first, over the bases the unit
// declares in the order each class lists them. A base outside the unit
// (`object`, a library class) has no declaration and no place in it. nil
// when typ is no Python class, or when its bases admit no linearisation (a
// cycle, or two bases each ordered before the other), where Python would
// refuse to create the class and the caller reads the supertypes instead.
func (h *hierarchy) pythonMRO(typ Ref) ([]Ref, error) {
	language, err := h.fileLanguage(typ.File)
	if err != nil || language != "python" {
		return nil, err
	}
	memo := map[Ref][]Ref{}
	active := map[Ref]bool{}
	var linearise func(t Ref) ([]Ref, error)
	linearise = func(t Ref) ([]Ref, error) {
		if l, ok := memo[t]; ok {
			return l, nil
		}
		if active[t] {
			return nil, nil
		}
		active[t] = true
		defer delete(active, t)
		bases, err := h.bases(t)
		if err != nil {
			return nil, err
		}
		seqs := make([][]Ref, 0, len(bases)+1)
		for _, b := range bases {
			l, err := linearise(b)
			if err != nil || l == nil {
				return nil, err
			}
			seqs = append(seqs, l)
		}
		merged, ok := mergeC3(append(seqs, bases))
		if !ok {
			return nil, nil
		}
		memo[t] = append([]Ref{t}, merged...)
		return memo[t], nil
	}
	return linearise(typ)
}

// bases are the direct supertypes of typ in the order its declaration lists
// them (addBase), each once.
func (h *hierarchy) bases(typ Ref) ([]Ref, error) {
	var es []entry
	if err := h.links.lookup(keySuper+refKey(typ), func(e entry) error {
		es = append(es, e)
		return nil
	}); err != nil {
		return nil, err
	}
	slices.SortStableFunc(es, func(a, b entry) int { return strings.Compare(a.Aux, b.Aux) })
	out := make([]Ref, 0, len(es))
	for _, e := range es {
		if !slices.Contains(out, e.Ref) {
			out = append(out, e.Ref)
		}
	}
	return out, nil
}

// mergeC3 is C3's merge of seqs: repeatedly the first head of a sequence that
// is in no sequence's tail, removed from every head; false when sequences
// remain and every head is in some tail.
func mergeC3(seqs [][]Ref) ([]Ref, bool) {
	var out []Ref
	for {
		seqs = slices.DeleteFunc(seqs, func(s []Ref) bool { return len(s) == 0 })
		if len(seqs) == 0 {
			return out, true
		}
		var next Ref
		ok := false
		for _, s := range seqs {
			inTail := slices.ContainsFunc(seqs, func(o []Ref) bool { return slices.Contains(o[1:], s[0]) })
			if !inTail {
				next, ok = s[0], true
				break
			}
		}
		if !ok {
			return nil, false
		}
		out = append(out, next)
		for i, s := range seqs {
			if s[0] == next {
				seqs[i] = s[1:]
			}
		}
	}
}

// fileLanguage is the language of a member file, as the hierarchy indexed it.
func (h *hierarchy) fileLanguage(file int32) (string, error) {
	var language string
	err := h.decls.lookup(keyFile+refKey(Ref{File: file}), func(e entry) error {
		language, _, _ = strings.Cut(e.Aux, "\x00")
		return nil
	})
	return language, err
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
