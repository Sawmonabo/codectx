package link

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
)

// Request inputs of handlers named across files (docs/providers-link.md,
// Request inputs across files). A Django view or a Starlette endpoint is
// often a function of a module no route of its own file names, and an
// Express handler a function a controller module exports: its file cannot
// tell it is a handler, so the structural provider records the reads of
// its first parameter under each request table its language has
// (RuleHandler), and the linker publishes them only when a route of
// another file names the function as a handler of that framework. A route
// names its handlers through imports (Table.RouteHandlers); each is indexed
// under keyRouteHandler by the module and name it reaches, and the
// handler's own file looks itself up there and resolves the route's import
// as the route walk does, so the read publishes only for the declaration
// that import resolves to.

// RouteHandler is one handler a route of the file names through an import:
// its route's framework, and the module as the import writes it (a
// relative Python module keeps its dots) and the name in it, joined by a
// NUL.
type RouteHandler struct {
	Framework string `json:"framework"`
	Imported  string `json:"imported"`
}

// keyRouteHandler + a module (a dotted Python module, or an ECMAScript
// module path without extension) + NUL + a name: a route names that
// module's name as a handler; Aux is the route's file, Sig the framework
// and, for Python, NUL and the handler as written.
const keyRouteHandler = "rh\x00"

// indexPythonHandlers adds each handler a Python member's routes name
// through an import to the module index, under the absolute module the
// import names.
func indexPythonHandlers(file int32, t *Table, add func(entry) error) error {
	for _, h := range t.RouteHandlers {
		mod, name, ok := strings.Cut(h.Imported, "\x00")
		if !ok || name == "" {
			continue
		}
		if strings.HasPrefix(mod, ".") {
			if !pythonRelativeFits(t.Path, mod) {
				continue
			}
			mod = absolutePythonModule(t.Path, mod)
		}
		if mod == "" {
			continue
		}
		e := entry{Key: keyRouteHandler + mod + "\x00" + name, Ref: Ref{File: file, Index: -1}, Aux: t.Path,
			Sig: h.Framework + "\x00" + h.Imported}
		if err := add(e); err != nil {
			return err
		}
	}
	return nil
}

// addJSHandlers adds each handler an ECMAScript member's routes name
// through an import, under each module the import's specifier resolves to.
func addJSHandlers(s interface{ Add(entry) error }, file int32, t *Table, es *esResolver) error {
	if familyOf(t.Language) != "js" {
		return nil
	}
	for _, h := range t.RouteHandlers {
		spec, name, ok := strings.Cut(h.Imported, "\x00")
		if !ok || name == "" {
			continue
		}
		for _, base := range es.targets(t.Path, spec) {
			e := entry{Key: keyRouteHandler + base + "\x00" + name, Ref: Ref{File: file, Index: -1}, Aux: t.Path,
				Sig: h.Framework}
			if err := s.Add(e); err != nil {
				return err
			}
		}
	}
	return nil
}

// pyImportedRefs are the declarations a handler a Python route names
// through an import (raw, a module as written, NUL, a name), written in the
// file at p, resolves to through the module index: the nearest, as an
// import's lookup keeps them.
func pyImportedRefs(mods *index, p, raw string) ([]Ref, error) {
	mod, name, ok := strings.Cut(raw, "\x00")
	if !ok || name == "" {
		return nil, nil
	}
	exact := strings.HasPrefix(mod, ".")
	if exact {
		if !pythonRelativeFits(p, mod) {
			return nil, nil
		}
		mod = absolutePythonModule(p, mod)
	}
	if mod == "" {
		return nil, nil
	}
	var hits []entry
	near := false
	err := mods.lookup(keyPyModule+mod+"\x00"+name, func(e entry) error {
		if e.Ref.Index >= 0 {
			hits = append(hits, e)
			near = near || hitNear(p, e.Aux, exact)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	var out []Ref
	for _, e := range hits {
		if hitNear(p, e.Aux, exact) || !near && !exact {
			out = append(out, e.Ref)
		}
	}
	return out, nil
}

// pyHandlerCheck decides, once per declaration and framework, whether a
// route of another file names a declaration of a Python member as a
// handler of that framework.
type pyHandlerCheck struct {
	mods  *index
	file  int32
	t     *Table
	named map[pyNamedKey]bool
}

type pyNamedKey struct {
	owner int32
	fw    string
}

// isNamed reports that a route of framework fw names declaration owner: an
// entry under any dotted name the member can be imported as, whose import,
// resolved from the route's file, reaches owner.
func (c *pyHandlerCheck) isNamed(owner int32, fw string) (bool, error) {
	k := pyNamedKey{owner, fw}
	if v, ok := c.named[k]; ok {
		return v, nil
	}
	if owner < 0 || int(owner) >= len(c.t.Decls) || c.t.Decls[owner].Qualified == "" {
		return false, nil
	}
	q, self := c.t.Decls[owner].Qualified, Ref{File: c.file, Index: owner}
	named := false
	err := pythonModuleNames(c.t.Path, func(name, _ string) error {
		return c.mods.lookup(keyRouteHandler+name+"\x00"+q, func(e entry) error {
			f, raw, _ := strings.Cut(e.Sig, "\x00")
			if named || f != fw {
				return nil
			}
			refs, err := pyImportedRefs(c.mods, e.Aux, raw)
			named = slices.Contains(refs, self)
			return err
		})
	})
	if err != nil {
		return false, err
	}
	c.named[k] = named
	return named, nil
}

// jsHandlerInputs emits one FactInput for each pending handler read of an
// ECMAScript member whose function a route of another file names, through
// an import of this module under a name it exports the function by, as a
// handler of the read's framework.
func (h *hierarchy) jsHandlerInputs(_ context.Context, file int32, t *Table, emit func(Binding) error) error {
	if familyOf(t.Language) != "js" {
		return nil
	}
	base := stripExtension(t.Path)
	named := map[pyNamedKey]bool{}
	for i := range t.Inputs {
		p := &t.Inputs[i]
		if p.Rule != RuleHandler || p.Owner < 0 || int(p.Owner) >= len(t.Decls) {
			continue
		}
		k := pyNamedKey{p.Owner, p.Framework}
		ok, seen := named[k]
		if !seen {
			for _, name := range jsExportNames(t, int(p.Owner)) {
				if err := h.decls.lookup(keyRouteHandler+base+"\x00"+name, func(e entry) error {
					ok = ok || e.Sig == p.Framework
					return nil
				}); err != nil {
					return err
				}
			}
			named[k] = ok
		}
		if ok {
			if err := emitHandlerInput(file, int32(i), p, emit); err != nil {
				return err
			}
		}
	}
	return nil
}

// emitHandlerInput emits the FactInput of pending handler read i.
func emitHandlerInput(file, i int32, p *PendingInput, emit func(Binding) error) error {
	raw, err := json.Marshal(inputFact{Framework: p.Framework, Source: p.Source, Key: p.Key})
	if err != nil {
		return internalErr("input fact: " + err.Error())
	}
	return emit(Binding{Kind: FactInput, Site: Ref{File: file, Index: i}, Target: Ref{File: -1, Index: -1},
		Detail: DetailImport, External: string(raw)})
}
