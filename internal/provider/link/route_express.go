package link

import (
	"context"
	"encoding/json"
	"slices"
	"strings"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/wire"
)

// Express routers across files (docs/providers-link.md, Route prefixes): a
// router a module binds and exports (`module.exports = router`, `export
// default router`, `export const admin = Router()`) is mounted where another
// file imports it and passes the binding to `use` (`app.use("/users",
// require("./routes/users"))`). The mount names the module as written and
// the export it reads (RouteMount.Target); the walk resolves the module as
// the file's imports resolve (hierarchy.esModules), follows re-exports as
// ECMA-262's ResolveExport does, and reads the router's routing from the
// hierarchy's links under keyJSRouting, so the file walking records each
// read.

// keyJSRouting + a module path without extension + NUL + an export name:
// the open router that export of that module is, as a routePayload in Sig.
const keyJSRouting = "jr\x00"

// addJSRouting adds an ECMAScript member's open routers to the hierarchy's
// links, each under every name the module exports its binding by, and what
// it sets on the NestJS applications it creates (addNestApp).
func addJSRouting(links interface{ Add(entry) error }, file int32, t *Table) error {
	rt := &t.Routing
	if familyOf(t.Language) != "js" {
		return nil
	}
	if err := addNestApp(links, file, t); err != nil {
		return err
	}
	if len(rt.Objects) == 0 {
		return nil
	}
	base := stripExtension(t.Path)
	for i, o := range rt.Objects {
		if o.Decl < 0 || int(o.Decl) >= len(t.Decls) {
			continue
		}
		names := jsExportNames(t, int(o.Decl))
		if len(names) == 0 {
			continue
		}
		p := routePayload{Path: t.Path, Object: o, Language: t.Language}
		for _, r := range rt.Routes {
			if r.Object == int32(i) {
				p.Routes = append(p.Routes, r)
			}
		}
		for _, m := range rt.Mounts {
			if m.Object != int32(i) || m.Target == "" {
				continue
			}
			// The walk reads the mount's target, never where it is.
			m.Range, m.Site, m.Arg = model.SourceRange{}, 0, 0
			p.Mounts = append(p.Mounts, payloadMount{RouteMount: m, Key: m.Target})
		}
		raw, err := json.Marshal(p)
		if err != nil {
			return internalErr("routing payload: " + err.Error())
		}
		for _, name := range names {
			e := entry{Key: keyJSRouting + base + "\x00" + name, Ref: Ref{File: file, Index: o.Decl}, Sig: string(raw)}
			if err := links.Add(e); err != nil {
				return err
			}
		}
	}
	return nil
}

// jsExportNames are the names the module of t exports its module-level
// binding i by: its own, when it is exported by name, and each a local
// export statement or a CommonJS assignment gives it (`export default
// router`, `module.exports = router`, `module.exports = { router }`).
func jsExportNames(t *Table, i int) []string {
	d := &t.Decls[i]
	if d.Parent >= 0 {
		return nil
	}
	var out []string
	if exportedByName(t, i) {
		out = append(out, d.Name)
	}
	for _, x := range t.Reexports {
		if x.From == "" && x.Original == d.Name {
			out = append(out, x.Name)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// jsRouteFacts walks down from each Express mount of the member at ordinal
// file made on an application root and emits one FactRoute per route it
// reaches and handler, on the mount, as emitPythonRoutes does, and the
// paths of its NestJS routes (nestRouteFacts).
func (h *hierarchy) jsRouteFacts(_ context.Context, file int32, t *Table, emit func(Binding) error) error {
	if familyOf(t.Language) != "js" {
		return nil
	}
	var found []routeFound
	for i := range t.Routing.Mounts {
		m := &t.Routing.Mounts[i]
		if m.Object >= 0 || m.Target == "" || m.Join != JoinNone {
			continue
		}
		n := len(found)
		w := jsWalk{h: h, found: &found}
		if err := w.mount(t.Path, m, m.Base, m.BaseKnown, nil); err != nil {
			return err
		}
		for k := n; k < len(found); k++ {
			found[k].mount = int32(i)
		}
	}
	if err := emitRoutes(file, found, emit); err != nil {
		return err
	}
	return h.nestRouteFacts(file, t, emit)
}

// jsWalk is one walk down from an Express root mount.
type jsWalk struct {
	h     *hierarchy
	found *[]routeFound
}

// mount follows mount m written in the file at from, under the parent
// prefix base, past the bindings on the way.
func (w *jsWalk) mount(from string, m *RouteMount, base string, known bool, way []routeHop) error {
	spec, name, ok := strings.Cut(m.Target, "\x00")
	if !ok {
		return nil
	}
	return w.h.esModules(from, spec, func(mod string) error {
		var hits []entry
		if err := w.h.jsRouters(mod, name, map[string]bool{}, func(e entry) error {
			hits = append(hits, e)
			return nil
		}); err != nil {
			return err
		}
		for _, e := range hits {
			hop := routeHop{file: e.Ref.File, decl: e.Ref.Index}
			if slices.Contains(way, hop) {
				continue
			}
			var p routePayload
			if err := json.Unmarshal([]byte(e.Sig), &p); err != nil {
				return internalErr("routing payload: " + err.Error())
			}
			if !mountable(*m, p.Object) {
				continue
			}
			pre, preKnown := joinPrefix(m.Prefix, p.Object.Own), m.PrefixKnown && p.Object.OwnKnown
			full, fullKnown := joinPrefix(base, pre), known && preKnown
			for _, r := range p.Routes {
				hs, err := w.handlers(e.Ref.File, p.Path, r)
				if err != nil {
					return err
				}
				*w.found = append(*w.found, routeFound{file: e.Ref.File, path: p.Path, object: name, route: r,
					full: joinRoute(full, r.Rel), known: fullKnown && r.Known, handlers: hs})
			}
			next := append(slices.Clone(way), hop)
			for k := range p.Mounts {
				cm := &p.Mounts[k].RouteMount
				if err := w.mount(p.Path, cm, joinPrefix(full, cm.Base), fullKnown && cm.BaseKnown, next); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// jsRouters yields the open router the module whose path is base exports
// as name: its own, else what a re-export of the name resolves to, else,
// for any name but `default` and the module's whole value, what its `export
// *` statements resolve it to when exactly one module supplies it (as
// resolveExport decides an export). seen ends a cycle.
func (h *hierarchy) jsRouters(base, name string, seen map[string]bool, yield func(entry) error) error {
	key := base + "\x00" + name
	if seen[key] {
		return nil
	}
	seen[key] = true
	found := false
	if err := h.links.lookup(keyJSRouting+key, func(e entry) error {
		found = true
		return yield(e)
	}); err != nil || found {
		return err
	}
	indirect := false
	if err := h.decls.lookup(keyReexport+key, func(e entry) error {
		indirect = true
		return h.jsRouters(e.Aux, e.Sig, seen, yield)
	}); err != nil || indirect || name == "default" || name == wire.ExportAssignment {
		return err
	}
	var hits []entry
	if err := h.decls.lookup(keyReexport+base+"\x00*", func(e entry) error {
		return h.jsRouters(e.Aux, name, seen, func(r entry) error {
			if !slices.ContainsFunc(hits, func(x entry) bool { return x.Ref == r.Ref }) {
				hits = append(hits, r)
			}
			return nil
		})
	}); err != nil || len(hits) != 1 {
		return err
	}
	return yield(hits[0])
}

// handlers are the declarations route r of the member at ordinal file
// (path p) routes to: those its file declares, and each one it imports,
// resolved as an import of that file resolves (esModules, moduleExport).
func (w *jsWalk) handlers(file int32, p string, r RouteEntry) ([]Ref, error) {
	out := make([]Ref, 0, len(r.Handlers)+len(r.Imported))
	for _, h := range r.Handlers {
		out = append(out, Ref{File: file, Index: h})
	}
	for _, raw := range r.Imported {
		spec, name, ok := strings.Cut(raw, "\x00")
		if !ok || name == "" {
			continue
		}
		err := w.h.esModules(p, spec, func(mod string) error {
			return w.h.moduleExport(mod, name, func(e entry) error {
				if e.Ref.Index >= 0 {
					out = append(out, e.Ref)
				}
				return nil
			})
		})
		if err != nil {
			return nil, err
		}
	}
	slices.SortFunc(out, compareRef)
	return slices.Compact(out), nil
}
