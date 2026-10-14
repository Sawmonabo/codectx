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

// resolvePython resolves every site of a Python unit: direct binding
// through imports and modules, a call of a class as its construction,
// flow-based type inference (inferTypes), field-based resolution of a
// receiver that recovered nothing, and the method a receiver's class
// declares or inherits in method resolution order, over the unit's own
// hierarchy (emitPython).
func resolvePython(ctx context.Context, u *unit, emit func(Binding) error) error {
	h, err := buildHierarchy(ctx, u, nil)
	if err != nil {
		return err
	}
	defer h.close()
	mods, err := buildModules(ctx, u, h)
	if err != nil {
		return err
	}
	defer mods.close()
	t, err := inferTypes(ctx, u, h, nil, nil, nil)
	if err != nil {
		return err
	}
	defer t.close()
	return emitPython(ctx, u, &flowEmit{h: h, mods: mods, t: t, emit: emit})
}

// emitPython emits the facts of the nodes e.only selects (every node when
// nil), whose sites the inference e.t bound.
//
// It reads the selected members' tables. Each site that stays inside its
// file (a bare call of a name the file declares, a receiver whose type the
// file or the inference recovered, a called value the inference traced)
// binds while its table is in hand. A site that names a module (`mod.f()`,
// a name a `from mod import f` introduced, which the worker spells as a
// TypeModule) and each import look the name up in the module index (every
// member's declarations and module under each dotted module name its path
// can be imported as, buildModules), one point lookup each, which the node
// making it records. The hits are settled per site and per import. A second
// read of the selected members emits the argument flows of every site bound
// to a declaration of the unit, which needs the site's arguments and what
// the lookups bound.
func emitPython(ctx context.Context, u *unit, e *flowEmit) error {
	h, emit := e.h, e.emit
	hits, err := pagination.NewExternalSort(filepath.Join(u.dir, "python-hits"), 0,
		encodeJSON[pyHit], decodeJSON[pyHit], comparePyHit)
	if err != nil {
		return err
	}
	defer hits.Close()
	calls, err := pagination.NewExternalSort(filepath.Join(u.dir, "python-calls"), 0,
		encodeJSON[pyCall], decodeJSON[pyCall], comparePyCall)
	if err != nil {
		return err
	}
	defer calls.Close()
	calls.WithFold(keepFirst[pyCall])
	call := func(b Binding, receiverFirst bool) error {
		if err := emit(b); err != nil {
			return err
		}
		return calls.Add(pyCall{Site: b.Site, Target: b.Target, First: receiverFirst})
	}
	look := func(r pyRecord) error { return lookPythonFollowing(e.h, e.mods, r, hits) }
	e.start()
	defer e.stop()
	err = u.filesOf(ctx, e.only, func(file int32, _ model.FileVersion, tab *Table, sel selection) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if sel.has(fileNode) {
			e.at(Ref{File: file, Index: fileNode})
			if err := h.hierarchyFacts(ctx, file, tab, emit); err != nil {
				return err
			}
			if err := addPythonImports(file, tab, look); err != nil {
				return err
			}
			if err := emitPythonRoutes(file, tab, e.mods, emit); err != nil {
				return err
			}
			if err := emitPythonInputs(file, tab, e.h, e.mods, emit); err != nil {
				return err
			}
		}
		r := pySite{ctx: ctx, h: h, t: e.t, fc: contextOf(file, tab), tab: tab, look: look, hits: hits, emit: emit, call: call}
		for i := range tab.Sites {
			if !sel.has(int32(i)) {
				continue
			}
			e.site(Ref{File: file, Index: int32(i)}, tab.Path)
			if err := r.resolve(Ref{File: file, Index: int32(i)}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if err := settlePythonHits(ctx, hits, emit, call); err != nil {
		return err
	}
	return pythonFlows(ctx, u, e, calls)
}

// lookPython answers the lookup r from the module index: one hit per record
// the index holds under r's key (pyModuleEntry), each read from the
// directory its module name is read from.
func lookPython(mods *index, r pyRecord, hits *pagination.ExternalSort[pyHit]) error {
	group := hitSite
	if r.Import {
		group = hitImport
	}
	return mods.lookup(keyPyModule+r.Key, func(e entry) error {
		d := pyModuleRecord(e)
		near := strings.HasPrefix(r.Root, d.Root)
		if r.Exact {
			near = d.Root == ""
		}
		return hits.Add(pyHit{Group: group, Site: r.Ref, Target: d.Ref, Init: r.Init, Class: d.Class, Near: near, Exact: r.Exact,
			Unbound: d.Unbound, Probe: r.Probe, Rank: r.Rank})
	})
}

// The flags of a Python record of the module index.
const (
	pyFlagClass   byte = 1
	pyFlagUnbound byte = 2
)

// pyModuleEntry is the module index's entry of the declaration or module
// record d.
func pyModuleEntry(d pyRecord) entry {
	var flags byte
	if d.Class {
		flags |= pyFlagClass
	}
	if d.Unbound {
		flags |= pyFlagUnbound
	}
	return entry{Key: keyPyModule + d.Key, Ref: d.Ref, Aux: d.Root, Flags: flags}
}

// pyModuleRecord is the record pyModuleEntry made e of.
func pyModuleRecord(e entry) pyRecord {
	return pyRecord{Key: strings.TrimPrefix(e.Key, keyPyModule), Ref: e.Ref, Root: e.Aux, Class: e.Flags&pyFlagClass != 0,
		Unbound: e.Flags&pyFlagUnbound != 0}
}

// pyRecord is one record of the module index: a declaration, or a file's
// module, indexed under one dotted module name its file can be imported as,
// or a lookup of a name in a module, by a site or an import.
type pyRecord struct {
	// Key is the dotted module name and the qualified name inside it,
	// joined by a NUL: "pkg.mod\x00Class.method"; a module's own Key has
	// nothing after the NUL.
	Key string `json:"k"`
	// Lookup marks a lookup; a declaration or a module otherwise.
	Lookup bool `json:"l,omitempty"`
	// Ref is the declaration (Index -1 for a module), the looking-up site,
	// or the looking-up import (an index into Table.Imports).
	Ref Ref `json:"r"`
	// Root is, for a declaration or a module, the directory its module name
	// is read from ("" for the repository root, else ending in "/"); for a
	// lookup, the looking-up file's path.
	Root string `json:"o,omitempty"`
	// Class marks a declaration of a class, and Unbound one of an instance
	// method, which a call through its class passes the receiver as the
	// first argument.
	Class   bool `json:"c,omitempty"`
	Unbound bool `json:"u,omitempty"`
	// Init marks a lookup of a constructed class's `__init__`.
	Init bool `json:"i,omitempty"`
	// Exact marks a relative import's lookup, whose module is named from
	// the repository root and answered by that root alone.
	Exact bool `json:"x,omitempty"`
	// Probe marks a site's lookup of the module it calls through itself,
	// which says whether the module is the unit's.
	Probe bool `json:"p,omitempty"`
	// Import marks an import's lookup, of Rank: 0 the declaration the
	// import names, 1 the submodule it names, 2 the module it imports from.
	Import bool  `json:"m,omitempty"`
	Rank   uint8 `json:"n,omitempty"`
}

// The groups of the module merge's hits.
const (
	hitSite   uint8 = 0 // a call site's
	hitImport uint8 = 1 // an import's
)

// pyHit is one record a lookup met, or a site's sentinel.
type pyHit struct {
	// Group and Site are the looking-up site or import.
	Group  uint8 `json:"g,omitempty"`
	Site   Ref   `json:"s"`
	Target Ref   `json:"t"`
	Init   bool  `json:"i,omitempty"`
	Class  bool  `json:"c,omitempty"`
	// Near reports that the declaration's module is read from a directory
	// the looking-up file sits under, the import's own source root.
	Near    bool  `json:"n,omitempty"`
	Exact   bool  `json:"x,omitempty"`
	Unbound bool  `json:"u,omitempty"`
	Probe   bool  `json:"p,omitempty"`
	Rank    uint8 `json:"k,omitempty"`
	// Sentinel is the one record every site that looks a name up through a
	// module has, so the site is settled when its lookups meet nothing.
	Sentinel bool `json:"e,omitempty"`
}

func comparePyHit(a, b pyHit) int {
	if a.Group != b.Group {
		return int(a.Group) - int(b.Group)
	}
	if c := compareRef(a.Site, b.Site); c != 0 {
		return c
	}
	if c := compareRef(a.Target, b.Target); c != 0 {
		return c
	}
	for _, c := range []int{cmpBool(a.Init, b.Init), cmpBool(a.Class, b.Class), cmpBool(a.Near, b.Near), cmpBool(a.Exact, b.Exact),
		cmpBool(a.Unbound, b.Unbound), cmpBool(a.Probe, b.Probe), int(a.Rank) - int(b.Rank), cmpBool(a.Sentinel, b.Sentinel)} {
		if c != 0 {
			return c
		}
	}
	return 0
}

// pyCall is a call fact to a declaration of the unit, which the flow pass
// reads: First says the site passes the target's receiver as its first
// argument.
type pyCall struct {
	Site   Ref  `json:"s"`
	Target Ref  `json:"t"`
	First  bool `json:"f,omitempty"`
}

func comparePyCall(a, b pyCall) int {
	if c := compareRef(a.Site, b.Site); c != 0 {
		return c
	}
	if c := compareRef(a.Target, b.Target); c != 0 {
		return c
	}
	return cmpBool(a.First, b.First)
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
// file can be imported as, keyed by its qualified name (a module-level
// function or class by its name, a method by "Class.method"), and the file's
// module under each such name with nothing after the NUL.
func indexPythonDecls(file int32, tab *Table, add func(pyRecord) error) error {
	return pythonModuleNames(tab.Path, func(name, root string) error {
		if err := add(pyRecord{Key: name + "\x00", Ref: Ref{File: file, Index: -1}, Root: root}); err != nil {
			return err
		}
		for i, d := range tab.Decls {
			if d.Qualified == "" {
				continue
			}
			rec := pyRecord{Key: name + "\x00" + d.Qualified, Ref: Ref{File: file, Index: int32(i)}, Root: root,
				Class: d.Kind == model.NodeClass, Unbound: d.Kind == model.NodeMethod && !d.Static}
			if err := add(rec); err != nil {
				return err
			}
		}
		return nil
	})
}

// addPythonImports adds the lookups of each import of tab, the table of the
// member at ordinal file: `import a.b`, `import a.b as c` (Original
// ImportAll: the name is the module itself) and `from a import *` look up
// the module they name; `from a import b` looks up the declaration b of module a
// (Rank 0), else the submodule a.b (Rank 1), else the module a itself, which
// the statement imports whatever b is (Rank 2). A relative module is named
// from the repository root (absolutePythonModule) and answered by that root
// alone; one that climbs above the root looks nothing up.
func addPythonImports(file int32, tab *Table, look func(pyRecord) error) error {
	for i, imp := range tab.Imports {
		mod, exact := imp.Path, strings.HasPrefix(imp.Path, ".")
		if exact {
			if !pythonRelativeFits(tab.Path, mod) {
				continue
			}
			mod = absolutePythonModule(tab.Path, mod)
		}
		ref := Ref{File: file, Index: int32(i)}
		add := func(key string, rank uint8) error {
			return look(pyRecord{Key: key, Lookup: true, Import: true, Rank: rank, Ref: ref, Root: tab.Path, Exact: exact})
		}
		if imp.Name == "" || imp.Name == ImportAll || imp.Original == ImportAll {
			if mod == "" {
				continue
			}
			if err := add(mod+"\x00", 2); err != nil {
				return err
			}
			continue
		}
		member := imp.Original
		if member == "" {
			member = imp.Name
		}
		sub := member
		if mod != "" {
			sub = mod + "." + member
			if err := add(mod+"\x00"+member, 0); err != nil {
				return err
			}
			if err := add(mod+"\x00", 2); err != nil {
				return err
			}
		}
		if err := add(sub+"\x00", 1); err != nil {
			return err
		}
	}
	return nil
}

// pythonRelativeFits reports that the relative module rel, imported by the
// file at importer, does not climb above the repository root.
func pythonRelativeFits(importer, rel string) bool {
	dots := len(rel) - len(strings.TrimLeft(rel, "."))
	depth := 0
	if dir := path.Dir(importer); dir != "." {
		depth = strings.Count(dir, "/") + 1
	}
	return dots-1 <= depth
}

// pySite resolves the sites of one member's table.
type pySite struct {
	ctx  context.Context
	h    *hierarchy
	t    *types
	fc   fileCtx
	tab  *Table
	look func(pyRecord) error
	hits *pagination.ExternalSort[pyHit]
	emit func(Binding) error
	// call emits a FactCall to a declaration of the unit and records it for
	// the flow pass, with whether the site passes the receiver first.
	call func(b Binding, receiverFirst bool) error
}

// exactCall is a binder of the exact calls of site by detail.
func (r *pySite) exactCall(site Ref, detail string) func(Ref) error {
	return func(target Ref) error {
		return r.call(Binding{Kind: FactCall, Site: site, Target: target, Detail: detail, Dispatch: model.DispatchExact}, false)
	}
}

// resolve binds one site of r.tab: what the file and the inference answer
// is emitted here, and a module lookup is added to the merge. A computed
// site binds nothing. Field-based resolution (a function written to the
// field a member site calls through) reaches a member site only when its
// receiver recovered nothing (inference.addFieldWatches).
func (r *pySite) resolve(site Ref) error {
	tab := r.tab
	s := &tab.Sites[site.Index]
	if s.Computed {
		return nil
	}
	if s.Receiver.Kind == wire.TypeModule {
		// A name an import introduced: a module (`import a.b as m; m.f()`)
		// or, on a bare site, the imported entity itself (`from a.b import
		// f; f()`, spelled "a.b.f").
		return lookupPython(site, tab.Path, s.Shape == wire.ShapeBare, s.Receiver.Name, s.Callee, true, r.look, r.hits)
	}
	if pythonBuiltinCall(tab, s) {
		return r.emit(Binding{Kind: FactExternalSite, Site: site, Detail: DetailDirect})
	}
	switch s.Shape {
	case wire.ShapeBare, wire.ShapeConstructor:
		for _, c := range visibleCandidates(tab, s) {
			if err := r.exactCall(site, DetailDirect)(Ref{File: site.File, Index: constructed(tab, c)}); err != nil {
				return err
			}
		}
	case wire.ShapeStatic:
		// `mod.f()` through an import, or `mod.C.m()` through a class of a
		// module, which calls the method C declares or inherits.
		if err := lookupPython(site, tab.Path, false, s.Qualifier, s.Callee, false, r.look, r.hits); err != nil {
			return err
		}
		return r.h.types(site.File, s.Qualifier, func(typ Ref) error {
			return r.class(site, s, typ, DetailDirect)
		})
	case wire.ShapeMember:
		if err := r.receiver(site, s); err != nil {
			return err
		}
	}
	// A method value taken through its class (`f = C.m; f(obj)`) passes its
	// receiver first; one taken through an instance is bound to it.
	return r.t.functions(site, func(target Ref, detail string, receiverFirst bool) error {
		return r.call(Binding{Kind: FactCall, Site: site, Target: target, Detail: detail, Dispatch: model.DispatchExact}, receiverFirst)
	})
}

// receiver binds a member site by its receiver's types: a receiver the file
// names as an instance (`self`, an annotation) by the method its class
// declares or inherits in method resolution order, virtual; a class object
// (`A.make()`, `cls.make()` of a classmethod's class written as a class) by
// that method, exact; one the inference recovered (a parameter, another
// call's result, a field) by flow, virtual for an instance a declaration
// types and exact for a construction's result or a class object; and
// `super()` by what follows the enclosing class in method resolution order,
// exact. A receiver type outside the project, and a class of the unit that
// reaches the method only through a supertype outside it, call the external
// method (externalReceiver, types.outsideCall).
func (r *pySite) receiver(site Ref, s *Site) error {
	switch s.Receiver.Kind {
	case wire.TypeNamed:
		found := false
		err := r.h.types(site.File, s.Receiver.Name, func(typ Ref) error {
			found = true
			return r.instance(site, s, typ, DetailHierarchy)
		})
		if err != nil || found {
			return err
		}
		_, err = externalReceiver(r.h, r.fc, site, s, []wire.Type{s.Receiver}, r.emit)
		return err
	case wire.TypeClass:
		return r.h.types(site.File, s.Receiver.Name, func(typ Ref) error {
			return r.class(site, s, typ, DetailDirect)
		})
	case wire.TypeSuper:
		return r.super(site, s)
	case wire.TypeUnion:
		if _, err := externalReceiver(r.h, r.fc, site, s, typeMembers(r.tab, s.Receiver), r.emit); err != nil {
			return err
		}
	}
	return r.t.seek(site, func(v recv) error {
		if v.Role != roleReceiver {
			return nil
		}
		detail := DetailFlow
		if v.Via {
			detail = DetailReceiverValue
		}
		switch v.X.Kind {
		case valInstance:
			return r.instance(site, s, v.X.Ref, detail)
		case valObject, valExact:
			return r.object(site, s, v.X.Ref, detail)
		case valClass:
			return r.class(site, s, v.X.Ref, DetailFlow)
		}
		return nil
	})
}

// instance binds site s on an instance of typ that a subtype's instance may
// stand for: the method typ declares or inherits, virtual when the language
// lets an override run (hierarchy.overridable), else the external method.
func (r *pySite) instance(site Ref, s *Site, typ Ref, detail string) error {
	var found []entry
	if err := r.h.declared(typ, s.Callee, func(e entry) error { found = append(found, e); return nil }); err != nil {
		return err
	}
	if len(found) == 0 {
		_, err := r.t.outsideCall(r.h, r.fc.family, site, typ, s.Callee, r.emit)
		return err
	}
	virtual, err := r.h.overridable(r.fc.family, typ, s.Callee, found)
	if err != nil {
		return err
	}
	d := model.DispatchExact
	if virtual {
		d = model.DispatchVirtual
	}
	for _, e := range found {
		if err := r.call(Binding{Kind: FactCall, Site: site, Target: e.Ref, Detail: detail, Dispatch: d}, false); err != nil {
			return err
		}
	}
	return r.ahead(site, typ, typ, s.Callee, detail, d)
}

// ahead emits, beside the binding of site to the first class of the unit
// along typ's order after from that defines method, a call to the method of
// each base outside the unit the order places ahead of that class
// (pythonAhead): the true callee is one of them or that class, a set the
// site's targets count.
func (r *pySite) ahead(site, typ, from Ref, method, detail string, d model.Dispatch) error {
	bases, err := r.h.pythonAhead(typ, from, method, true)
	if err != nil {
		return err
	}
	for _, q := range bases {
		err := r.emit(Binding{Kind: FactCall, Site: site, Detail: detail, Dispatch: d,
			External: externalMethod(r.fc.family, q, method)})
		if err != nil {
			return err
		}
	}
	return nil
}

// object binds site s on an instance of exactly typ: the method typ declares
// or inherits, exact, else the external method.
func (r *pySite) object(site Ref, s *Site, typ Ref, detail string) error {
	found := false
	err := r.h.declared(typ, s.Callee, func(e entry) error {
		found = true
		return r.exactCall(site, detail)(e.Ref)
	})
	if err != nil {
		return err
	}
	if found {
		return r.ahead(site, typ, typ, s.Callee, detail, model.DispatchExact)
	}
	_, err = r.t.outsideCall(r.h, r.fc.family, site, typ, s.Callee, r.emit)
	return err
}

// class binds site s on the class object typ: the method typ declares or
// inherits, exact, an instance method taking the site's first argument as
// its receiver; else the external method.
func (r *pySite) class(site Ref, s *Site, typ Ref, detail string) error {
	found := false
	err := r.h.declared(typ, s.Callee, func(e entry) error {
		found = true
		b := Binding{Kind: FactCall, Site: site, Target: e.Ref, Detail: detail, Dispatch: model.DispatchExact}
		return r.call(b, e.Flags&flagStatic == 0)
	})
	if err != nil {
		return err
	}
	if found {
		return r.ahead(site, typ, typ, s.Callee, detail, model.DispatchExact)
	}
	_, err = r.t.outsideCall(r.h, r.fc.family, site, typ, s.Callee, r.emit)
	return err
}

// super binds `super().m()` (resolvePythonSuper), exact; when no class of
// the unit after the enclosing one declares m, the external method of the
// enclosing class's supertypes outside the project.
func (r *pySite) super(site Ref, s *Site) error {
	found := false
	bind := r.exactCall(site, DetailHierarchy)
	err := resolvePythonSuper(r.ctx, r.h, site, r.tab, s, func(target Ref) error {
		found = true
		return bind(target)
	}, func(q string) error {
		return r.emit(Binding{Kind: FactCall, Site: site, Detail: DetailHierarchy, Dispatch: model.DispatchExact,
			External: externalMethod(r.fc.family, q, s.Callee)})
	})
	if err != nil || found {
		return err
	}
	class := enclosingClass(r.tab, s.Caller)
	if class < 0 {
		return nil
	}
	_, err = r.t.outsideCall(r.h, r.fc.family, site, Ref{File: site.File, Index: class}, s.Callee, r.emit)
	return err
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
func resolvePythonSuper(ctx context.Context, h *hierarchy, site Ref, tab *Table, s *Site, bind func(Ref) error, external func(string) error) error {
	class := enclosingClass(tab, s.Caller)
	if class < 0 {
		return nil
	}
	b := Ref{File: site.File, Index: class}
	// external is told, once each, the bases outside the unit some
	// subtype's order places after B and ahead of the class it binds.
	told := map[string]bool{}
	return pythonSubtypes(ctx, h, b, func(sub Ref) error {
		ahead, err := h.pythonAhead(sub, b, s.Callee, false)
		if err != nil {
			return err
		}
		for _, q := range ahead {
			if !told[q] {
				told[q] = true
				if err := external(q); err != nil {
					return err
				}
			}
		}
		order, err := h.pythonUnitOrder(sub)
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
func mergeC3[T comparable](seqs [][]T) ([]T, bool) {
	var out []T
	for {
		seqs = slices.DeleteFunc(seqs, func(s []T) bool { return len(s) == 0 })
		if len(seqs) == 0 {
			return out, true
		}
		var next T
		ok := false
		for _, s := range seqs {
			inTail := slices.ContainsFunc(seqs, func(o []T) bool { return slices.Contains(o[1:], s[0]) })
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

// pyBase is one class of a Python method resolution order with the bases
// outside the unit placed (pythonFullMRO): a class of the unit (ref), or a
// base outside it, its qualified name in external.
type pyBase struct {
	ref      Ref
	external string
}

// pythonFullMRO is typ's C3 linearisation with every base outside the unit
// placed where the linearisation puts it ("The Python 2.3 Method Resolution
// Order": the merge of the bases' linearisations and the bases as listed,
// local precedence order and monotonicity kept). The unit has no
// declaration of a base outside it, so its own bases are not known: it
// enters the merge as the linearisation of itself alone. Its unknown
// ancestors are all outside the unit and follow it in the true order, and
// a member they define is reached through it (attribute lookup on the base
// finds it), so the order decides, for each class of the unit, which bases
// outside the unit precede it. That is exact unless a base outside the unit
// is itself an ancestor of another one the hierarchy names, which no
// declaration read can show: the true order then places that ancestor
// after its descendant, which this order does not know, so it may list
// that ancestor ahead of a class of the unit the true order puts first.
// nil when typ is no Python class or its bases admit no linearisation
// (Python refuses to create such a class).
func (h *hierarchy) pythonFullMRO(typ Ref) ([]pyBase, error) {
	language, err := h.fileLanguage(typ.File)
	if err != nil || language != "python" {
		return nil, err
	}
	memo := map[Ref][]pyBase{}
	active := map[Ref]bool{}
	var linearise func(t Ref) ([]pyBase, error)
	linearise = func(t Ref) ([]pyBase, error) {
		if l, ok := memo[t]; ok {
			return l, nil
		}
		if active[t] {
			return nil, nil
		}
		active[t] = true
		defer delete(active, t)
		bases, err := h.writtenBases(t)
		if err != nil {
			return nil, err
		}
		seqs := make([][]pyBase, 0, len(bases)+1)
		for _, b := range bases {
			if b.external != "" {
				seqs = append(seqs, []pyBase{b})
				continue
			}
			l, err := linearise(b.ref)
			if err != nil || l == nil {
				return nil, err
			}
			seqs = append(seqs, l)
		}
		merged, ok := mergeC3(append(seqs, bases))
		if !ok {
			return nil, nil
		}
		memo[t] = append([]pyBase{{ref: t}}, merged...)
		return memo[t], nil
	}
	return linearise(typ)
}

// pythonUnitOrder is typ's method resolution order over the classes of the
// unit: pythonFullMRO's without the bases outside the unit, else, when the
// bases outside the unit admit no linearisation, pythonMRO's.
func (h *hierarchy) pythonUnitOrder(typ Ref) ([]Ref, error) {
	full, err := h.pythonFullMRO(typ)
	if err != nil || full == nil {
		if err != nil {
			return nil, err
		}
		return h.pythonMRO(typ)
	}
	out := make([]Ref, 0, len(full))
	for _, b := range full {
		if b.external == "" {
			out = append(out, b.ref)
		}
	}
	return out, nil
}

// pythonAhead is the bases outside the unit that typ's full order
// (pythonFullMRO) places after the class from and ahead of the first class
// of the unit after it that defines method, when a lookup starting past
// from reaches that class (`super()` in from); with self, the lookup starts
// at from itself (a call on from's instance, from being typ), and from
// defining method leaves none. Each may hold the method instead of that
// class, so the call's targets are a set of them and that class. Empty when
// no such class follows, or when the order does not exist.
func (h *hierarchy) pythonAhead(typ, from Ref, method string, self bool) ([]string, error) {
	full, err := h.pythonFullMRO(typ)
	if err != nil || full == nil {
		return nil, err
	}
	start := slices.IndexFunc(full, func(b pyBase) bool { return b.external == "" && b.ref == from })
	if start < 0 {
		return nil, nil
	}
	if self {
		defines := false
		if err := h.methods(from, method, func(entry) error { defines = true; return nil }); err != nil || defines {
			return nil, err
		}
	}
	var ahead []string
	for _, b := range full[start+1:] {
		if b.external != "" {
			ahead = append(ahead, b.external)
			continue
		}
		defines := false
		if err := h.methods(b.ref, method, func(entry) error { defines = true; return nil }); err != nil {
			return nil, err
		}
		if defines {
			return ahead, nil
		}
	}
	return nil, nil
}

// writtenBases are typ's direct bases, of the unit (keySuper, position in
// Aux) and outside it (keyExternalSuper, position in Sig), in the order its
// declaration lists them, each once.
func (h *hierarchy) writtenBases(typ Ref) ([]pyBase, error) {
	type placed struct {
		pos string
		b   pyBase
	}
	var all []placed
	if err := h.links.lookup(keySuper+refKey(typ), func(e entry) error {
		all = append(all, placed{e.Aux, pyBase{ref: e.Ref}})
		return nil
	}); err != nil {
		return nil, err
	}
	if err := h.links.lookup(keyExternalSuper+refKey(typ), func(e entry) error {
		all = append(all, placed{e.Sig, pyBase{external: e.Aux}})
		return nil
	}); err != nil {
		return nil, err
	}
	slices.SortStableFunc(all, func(a, b placed) int { return strings.Compare(a.pos, b.pos) })
	out := make([]pyBase, 0, len(all))
	for _, p := range all {
		if !slices.Contains(out, p.b) {
			out = append(out, p.b)
		}
	}
	return out, nil
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
// is tried as one too. With probe, the site is bound through the module
// alone: it gets a sentinel hit and the lookups of the modules it may name,
// which say whether a module of that name is the unit's (settlePython).
func lookupPython(site Ref, importer string, bare bool, module, callee string, probe bool,
	look func(pyRecord) error, hits *pagination.ExternalSort[pyHit]) error {
	if module == "" {
		return nil
	}
	exact := strings.HasPrefix(module, ".")
	if exact {
		if !pythonRelativeFits(importer, module) {
			return nil
		}
		if module = absolutePythonModule(importer, module); module == "" {
			return nil
		}
	}
	if probe {
		if err := hits.Add(pyHit{Site: site, Sentinel: true}); err != nil {
			return err
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
			if err := look(pyRecord{Key: mod + "\x00" + q, Lookup: true, Ref: site, Root: importer, Init: init, Exact: exact}); err != nil {
				return err
			}
		}
		return nil
	}
	known := func(mod string) error {
		if !probe || mod == "" {
			return nil
		}
		return look(pyRecord{Key: mod + "\x00", Lookup: true, Probe: true, Ref: site, Root: importer, Exact: exact})
	}
	mod, last := module, ""
	if i := strings.LastIndexByte(module, '.'); i >= 0 {
		mod, last = module[:i], module[i+1:]
	}
	if bare {
		if last == "" {
			return nil
		}
		if err := known(mod); err != nil {
			return err
		}
		return add(mod, last)
	}
	if err := known(module); err != nil {
		return err
	}
	if err := add(module, callee); err != nil {
		return err
	}
	if last != "" {
		if err := known(mod); err != nil {
			return err
		}
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

// settlePythonHits settles the hits the lookups met (lookPython), with each
// module-bound site's sentinel, sorted by site or import: per site
// (settlePython) and per import (settlePythonImport). A canceled context
// stops it between groups.
func settlePythonHits(ctx context.Context, hits *pagination.ExternalSort[pyHit], emit func(Binding) error,
	call func(Binding, bool) error) error {
	settled, err := hits.Sorted()
	if err != nil {
		return err
	}
	defer settled.Close()
	var group []pyHit
	flush := func() error {
		if len(group) == 0 {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if group[0].Group == hitImport {
			return settlePythonImport(group, emit)
		}
		return settlePython(group, emit, call)
	}
	err = settled.Each(func(x pyHit) error {
		if len(group) > 0 && (group[0].Group != x.Group || group[0].Site != x.Site) {
			if err := flush(); err != nil {
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
	return flush()
}

// nearest is the hits a lookup keeps of group: those read from the
// looking-up file's source root when any is, else every hit of a
// non-relative import.
func nearest(group []pyHit) []pyHit {
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
	return kept
}

// settlePython emits one site's module hits (nearest), exact, and of those
// no class whose file supplied the class's `__init__`; an instance method
// reached through its class takes the site's first argument as its
// receiver. A site bound through a module alone that meets nothing, through
// a module no member file is, is a proven external call (FactExternalSite).
func settlePython(group []pyHit, emit func(Binding) error, call func(Binding, bool) error) error {
	sentinel, known := false, false
	var found []pyHit
	for _, x := range group {
		switch {
		case x.Sentinel:
			sentinel = true
		case x.Probe:
			known = true
		default:
			found = append(found, x)
		}
	}
	kept := nearest(found)
	for _, x := range kept {
		if x.Class && !x.Init && initFrom(kept, x.Target.File) {
			continue
		}
		b := Binding{Kind: FactCall, Site: x.Site, Target: x.Target, Detail: DetailDirect, Dispatch: model.DispatchExact}
		if err := call(b, x.Unbound && !x.Init); err != nil {
			return err
		}
	}
	if len(kept) == 0 && sentinel && !known {
		return emit(Binding{Kind: FactExternalSite, Site: group[0].Site, Detail: DetailDirect})
	}
	return nil
}

// settlePythonImport emits one import's FactImport to each file its
// nearest hits of the best rank are in: the file declaring the name it
// imports, else the submodule's, else the module's it imports from.
func settlePythonImport(group []pyHit, emit func(Binding) error) error {
	kept := nearest(group)
	if len(kept) == 0 {
		return nil
	}
	best := kept[0].Rank
	for _, x := range kept {
		best = min(best, x.Rank)
	}
	var files []int32
	for _, x := range kept {
		if x.Rank != best || slices.Contains(files, x.Target.File) {
			continue
		}
		files = append(files, x.Target.File)
		b := Binding{Kind: FactImport, Site: x.Site, Target: Ref{File: x.Target.File, Index: -1}, Detail: DetailImport}
		if err := emit(b); err != nil {
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

// pythonFlows reads the tables of the members e.only selects a site of a
// second time and emits the argument and return flows of every call fact of
// a selected site with arguments or a result to a declaration of the unit
// (emitFlows), the sites asked in
// ascending order against calls, sorted by site, each site's parameter
// lookups recorded for it.
func pythonFlows(ctx context.Context, u *unit, e *flowEmit, calls *pagination.ExternalSort[pyCall]) error {
	run, err := calls.Sorted()
	if err != nil {
		return err
	}
	defer run.Close()
	if run.Len() == 0 {
		return nil
	}
	at := newSeeker(run, func(c pyCall) Ref { return c.Site })
	defer at.stop()
	e.start()
	defer e.stop()
	return u.filesOf(ctx, e.only, func(file int32, _ model.FileVersion, tab *Table, sel selection) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		for i := range tab.Sites {
			s := &tab.Sites[i]
			if (len(s.Arguments) == 0 && len(s.Nested) == 0 && !hasResult(s)) || !sel.has(int32(i)) {
				continue
			}
			site := Ref{File: file, Index: int32(i)}
			e.at(site)
			err := at.each(site, func(c pyCall) error { return emitFlows(e.h, site, s, c.Target, c.First, e.emit) })
			if err != nil {
				return err
			}
		}
		return nil
	})
}
