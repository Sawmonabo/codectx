package link

import (
	"encoding/json"
	"slices"
	"strings"
)

// NestJS module paths (docs/providers-link.md, NestJS paths). A route of a
// controller is served under the path RouterModule gives the module whose
// `controllers` list the controller (Nest's MODULE_PATH metadata, read per
// module by RoutesResolver): a `RouterModule.register(routes)` call
// flattens its route tree (flattenRoutePaths) and sets, for each module in
// order, the normalized path (router-module.ts), so a later entry of one
// module replaces an earlier one. A module no call names has no path.

// NestRef names a class: a module-level declaration of the file (Local, an
// index of Decls), or, with Local -1, the export Name of the module Spec an
// import binds it from.
type NestRef struct {
	Local int32  `json:"local"`
	Spec  string `json:"spec,omitempty"`
	Name  string `json:"name,omitempty"`
}

// NestModule is one `@Module` class (Decl) and the controllers its
// `controllers` array lists.
type NestModule struct {
	Decl        int32     `json:"decl"`
	Controllers []NestRef `json:"controllers,omitempty"`
}

// NestRouter is one `RouterModule.register(routes)` call: the route tree
// flattened as flattenRoutePaths does, each module with the path
// RouterModule gives it, in that order; Known false when the tree is no
// literal the file holds. Import is the binding a tree another module
// exports is imported by, which the linker follows (nestTreeOf).
type NestRouter struct {
	Entries []NestRouterEntry `json:"entries,omitempty"`
	Known   bool              `json:"known,omitempty"`
	Import  *NestRef          `json:"import,omitempty"`
}

// NestTreeDecl is a module-level binding (Decl) whose initializer is a
// route tree, flattened as a register call's (Router).
type NestTreeDecl struct {
	Decl   int32      `json:"decl"`
	Router NestRouter `json:"router"`
}

// NestRouterEntry is one module and its RouterModule path, as the flattened
// tree holds it before Nest normalizes it.
type NestRouterEntry struct {
	Path   string  `json:"path"`
	Module NestRef `json:"module"`
}

const (
	// keyNestModule + a controller's name in the module that declares or
	// exports it (a local class's own name, an import's original): one
	// module listing it, Ref the module's class, Sig a nestListing.
	keyNestModule = "nm\x00"
	// keyNestRouter: one `RouterModule.register` call, Sig a nestRouterOf.
	keyNestRouter = "nr\x00"
	// keyNestTree + a module path without extension + NUL + an export
	// name: the route tree that export of that module is, Sig a
	// nestRouterOf.
	keyNestTree = "nt\x00"
	// keyJSRenamed + a name: an ECMAScript module exporting what that name
	// denotes (its own declaration, an import, or another module's export)
	// under another name, Aux the new name.
	keyJSRenamed = "jn\x00"
)

// nestListing is one controller a module lists: the reference as the
// module's file writes it, and that file's path, which its imports resolve
// from.
type nestListing struct {
	Path string  `json:"path"`
	Ref  NestRef `json:"ref"`
}

// nestRouterOf is one register call with the path of the file making it;
// file is that member's ordinal, set as it is read.
type nestRouterOf struct {
	Path   string     `json:"path"`
	Router NestRouter `json:"router"`
	file   int32
}

// addNestModules adds the modules of t's NestApp a, one entry per
// controller each lists, and a's routers.
func addNestModules(links interface{ Add(entry) error }, file int32, t *Table, a *NestApp) error {
	for _, m := range a.Modules {
		for _, c := range m.Controllers {
			name := c.Name
			if c.Local >= 0 {
				if int(c.Local) >= len(t.Decls) {
					continue
				}
				name = t.Decls[c.Local].Name
			}
			raw, err := json.Marshal(nestListing{Path: t.Path, Ref: c})
			if err != nil {
				return internalErr("nest module: " + err.Error())
			}
			if err := links.Add(entry{Key: keyNestModule + name, Ref: Ref{File: file, Index: m.Decl},
				Sig: string(raw)}); err != nil {
				return err
			}
		}
	}
	for _, r := range a.Routers {
		raw, err := json.Marshal(nestRouterOf{Path: t.Path, Router: r})
		if err != nil {
			return internalErr("nest router: " + err.Error())
		}
		if err := links.Add(entry{Key: keyNestRouter, Ref: Ref{File: file, Index: -1}, Sig: string(raw)}); err != nil {
			return err
		}
	}
	base := stripExtension(t.Path)
	for _, tr := range a.Trees {
		if tr.Decl < 0 || int(tr.Decl) >= len(t.Decls) {
			continue
		}
		raw, err := json.Marshal(nestRouterOf{Path: t.Path, Router: tr.Router})
		if err != nil {
			return internalErr("nest tree: " + err.Error())
		}
		for _, name := range jsExportNames(t, int(tr.Decl)) {
			if err := links.Add(entry{Key: keyNestTree + base + "\x00" + name, Ref: Ref{File: file, Index: tr.Decl},
				Sig: string(raw)}); err != nil {
				return err
			}
		}
	}
	return nil
}

// addJSRenames adds each renaming export of an ECMAScript member under the
// name it renames (keyJSRenamed): `export { A as B }`, `export { A as B }
// from`, `export { default as B } from`, so a controller a barrel renames
// is found under the name a module lists it by (modulePaths).
func addJSRenames(links interface{ Add(entry) error }, file int32, t *Table) error {
	for _, x := range t.Reexports {
		if x.Name == x.Original || x.Name == "*" || x.Original == "*" || x.Name == "" || x.Original == "" {
			continue
		}
		if err := links.Add(entry{Key: keyJSRenamed + x.Original, Ref: Ref{File: file, Index: -1}, Aux: x.Name}); err != nil {
			return err
		}
	}
	return nil
}

// nestRouters are the unit's register calls, each tree another module
// exports followed to it (nestTreeOf), false when any tree is no literal.
func (h *hierarchy) nestRouters() ([]nestRouterOf, bool, error) {
	var out []nestRouterOf
	err := h.links.lookup(keyNestRouter, func(e entry) error {
		var r nestRouterOf
		if err := json.Unmarshal([]byte(e.Sig), &r); err != nil {
			return internalErr("nest router: " + err.Error())
		}
		r.file = e.Ref.File
		out = append(out, r)
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	for i := range out {
		if imp := out[i].Router.Import; imp != nil {
			r, ok, err := h.nestTreeOf(out[i].Path, *imp)
			if err != nil || !ok {
				return nil, false, err
			}
			out[i] = r
		}
		if !out[i].Router.Known {
			return nil, false, nil
		}
	}
	return out, true, nil
}

// nestTreeOf is the route tree the import imp of the member at path p
// binds: the one tree binding the module it names exports under that name,
// through re-exports (jsExported); false when it reaches none or several.
func (h *hierarchy) nestTreeOf(p string, imp NestRef) (nestRouterOf, bool, error) {
	var hits []entry
	err := h.esModules(p, imp.Spec, func(mod string) error {
		return h.jsExported(keyNestTree, mod, imp.Name, map[string]bool{}, func(e entry) error {
			if !slices.ContainsFunc(hits, func(x entry) bool { return x.Ref == e.Ref }) {
				hits = append(hits, e)
			}
			return nil
		})
	})
	if err != nil || len(hits) != 1 {
		return nestRouterOf{}, false, err
	}
	var r nestRouterOf
	if err := json.Unmarshal([]byte(hits[0].Sig), &r); err != nil {
		return nestRouterOf{}, false, internalErr("nest tree: " + err.Error())
	}
	r.file = hits[0].Ref.File
	return r, true, nil
}

// nestModulePaths are a controller's RouterModule paths, known false when
// they are undecided.
type nestModulePaths struct {
	paths []string
	known bool
}

// modulePaths are the RouterModule paths the controller declared at ctrl
// in the member at ordinal file is served under, one per module listing
// it: the path the last entry of that module in a register call gives it,
// the empty path when no call names the module, and the empty path alone
// when RouterModule is not in use. A module lists the controller by a name
// the controller's file exports it under or declares it by, and the
// listing resolves to it. false when no module lists the controller, a
// tree's entry names no class the unit declares, or two register calls
// give one module different paths (which holds is the order Nest
// initializes the modules in).
func (h *hierarchy) modulePaths(u *nestUnit, file int32, t *Table, ctrl int32) ([]string, bool, error) {
	if !u.router {
		return []string{""}, true, nil
	}
	target := Ref{File: file, Index: ctrl}
	names, err := h.nestNames(t, ctrl)
	if err != nil {
		return nil, false, err
	}
	var modules []Ref
	for _, name := range names {
		var listings []entry
		if err := h.links.lookup(keyNestModule+name, func(e entry) error {
			listings = append(listings, e)
			return nil
		}); err != nil {
			return nil, false, err
		}
		for _, e := range listings {
			var l nestListing
			if err := json.Unmarshal([]byte(e.Sig), &l); err != nil {
				return nil, false, internalErr("nest module: " + err.Error())
			}
			refs, err := h.nestResolve(e.Ref.File, l.Path, l.Ref)
			if err != nil {
				return nil, false, err
			}
			if slices.Contains(refs, target) && !slices.Contains(modules, e.Ref) {
				modules = append(modules, e.Ref)
			}
		}
	}
	if len(modules) == 0 {
		return nil, false, nil
	}
	found := make([][]string, len(modules))
	for _, r := range u.routers {
		last := make([]int, len(modules))
		for i := range last {
			last[i] = -1
		}
		for k, en := range r.Router.Entries {
			refs, err := h.nestResolve(r.file, r.Path, en.Module)
			if err != nil {
				return nil, false, err
			}
			if len(refs) == 0 {
				return nil, false, nil
			}
			for i, m := range modules {
				if slices.Contains(refs, m) {
					last[i] = k
				}
			}
		}
		for i, k := range last {
			if k < 0 {
				continue
			}
			if p := NestNormalize(r.Router.Entries[k].Path); !slices.Contains(found[i], p) {
				found[i] = append(found[i], p)
			}
		}
	}
	var out []string
	for _, f := range found {
		if len(f) > 1 {
			return nil, false, nil
		}
		p := ""
		if len(f) == 1 {
			p = f[0]
		}
		if !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	return out, true, nil
}

// nestNames are the names a module may list the controller declared at
// ctrl by: its own, each its module exports it under, and each a renaming
// export of any of them gives it (keyJSRenamed), transitively. Each listing
// found under one is confirmed by resolving it (modulePaths), so a name
// another class shares adds a lookup, never a match.
func (h *hierarchy) nestNames(t *Table, ctrl int32) ([]string, error) {
	names := append([]string{t.Decls[ctrl].Name}, jsExportNames(t, int(ctrl))...)
	slices.Sort(names)
	names = slices.Compact(names)
	for i := 0; i < len(names); i++ {
		if err := h.links.lookup(keyJSRenamed+names[i], func(e entry) error {
			if !slices.Contains(names, e.Aux) {
				names = append(names, e.Aux)
			}
			return nil
		}); err != nil {
			return nil, err
		}
	}
	return names, nil
}

// nestResolve is the class declarations a reference written in the member
// at ordinal file (path p) names: its own declaration, or the export its
// import reads, resolved as the member's imports resolve (esModules,
// moduleExport).
func (h *hierarchy) nestResolve(file int32, p string, r NestRef) ([]Ref, error) {
	if r.Local >= 0 {
		return []Ref{{File: file, Index: r.Local}}, nil
	}
	var out []Ref
	err := h.esModules(p, r.Spec, func(mod string) error {
		return h.moduleExport(mod, r.Name, func(e entry) error {
			if e.Aux == exportType && !slices.Contains(out, e.Ref) {
				out = append(out, e.Ref)
			}
			return nil
		})
	})
	return out, err
}

// NestNormalize is Nest's normalizePath: a leading slash, no trailing one,
// runs of slashes as one; `/` for the empty path.
func NestNormalize(p string) string {
	if p == "" {
		return "/"
	}
	p = "/" + strings.TrimRight(p, "/")
	for strings.Contains(p, "//") {
		p = strings.ReplaceAll(p, "//", "/")
	}
	return p
}
