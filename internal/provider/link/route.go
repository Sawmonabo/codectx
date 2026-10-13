package link

import (
	"cmp"
	"encoding/json"
	"slices"
	"strings"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/lang"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/wire"
)

// Routing is what a file's framework route table left open inside the file
// (docs/providers-link.md, Route prefixes): the application and router
// objects a module-level binding holds that the file mounts nowhere, the
// routes registered on them, and the mounts whose mounted object the file
// does not build. The linker joins them across files: a mount's argument
// resolves through the unit's Python module index to another file's object,
// and a walk from each application root down the mounts gives every route
// below it its full path.
type Routing struct {
	Objects []RouteObject `json:"objects,omitempty"`
	Routes  []RouteEntry  `json:"routes,omitempty"`
	Mounts  []RouteMount  `json:"mounts,omitempty"`
	// Settings are the settings modules the file names in a literal
	// DJANGO_SETTINGS_MODULE (`os.environ.setdefault(...)`,
	// `os.environ[...] = ...`), as dotted names.
	Settings []string `json:"settings,omitempty"`
}

// RouteObject is one object a module-level binding holds (Decl indexes
// Decls) that the file attaches nowhere and that is no application root: a
// Flask blueprint, a FastAPI router, a Django `urlpatterns`. Own is the
// prefix its construction declares, OwnKnown false when that is not a
// literal.
type RouteObject struct {
	Decl      int32  `json:"decl"`
	Framework string `json:"framework"`
	Own       string `json:"own,omitempty"`
	OwnKnown  bool   `json:"own_known,omitempty"`
}

// RouteEntry is one route registered, inside the file, on Objects[Object]:
// its framework, method and route as written, Rel the path below the
// object's own prefix (every in-file prefix between the route and the
// object joined to the route), Known false when any of them, or the route,
// is not a literal. Handlers are the declarations it routes to (indexes of
// Decls), and Imported the handlers it names through an import, each a
// module as the import writes it (a relative one keeps its dots) and a name
// in it joined by a NUL, which the walk looks up in the module index.
type RouteEntry struct {
	Object    int32    `json:"object"`
	Framework string   `json:"framework"`
	Method    string   `json:"method,omitempty"`
	Route     string   `json:"route"`
	Rel       string   `json:"rel,omitempty"`
	Known     bool     `json:"known,omitempty"`
	Handlers  []int32  `json:"handlers,omitempty"`
	Imported  []string `json:"imported,omitempty"`
}

// RouteMount is one mount of an object the file does not build: the call
// at Range whose argument Arg of site Site (an index of Sites) is the
// mounted object (`app.include_router(users.router)`), or, with Module set,
// the module whose urlconf it includes (Django `include("blog.urls")`), or,
// with Root set, the root urlconf a settings module names (ROOT_URLCONF).
// Prefix is the mount's own prefix and Override that it replaces the
// mounted object's own (Flask `url_prefix=` at registration). Object is
// the open object the parent's chain reaches inside the file, -1 when it
// reaches an application root; Base is the parent's prefix, absolute when
// Object is -1, else below that object's own; BaseKnown false when a prefix
// on the way is not a literal.
type RouteMount struct {
	Site        int32             `json:"site"`
	Arg         int32             `json:"arg"`
	Module      string            `json:"module,omitempty"`
	Root        bool              `json:"root,omitempty"`
	Framework   string            `json:"framework"`
	Prefix      string            `json:"prefix,omitempty"`
	PrefixKnown bool              `json:"prefix_known,omitempty"`
	Override    bool              `json:"override,omitempty"`
	Object      int32             `json:"object"`
	Base        string            `json:"base,omitempty"`
	BaseKnown   bool              `json:"base_known,omitempty"`
	Range       model.SourceRange `json:"range"`
}

// djangoURLConf is the binding a Django urlconf module holds its routes in:
// "Django loads that Python module and looks for the variable urlpatterns".
const djangoURLConf = "urlpatterns"

// The module index's routing keys (buildModules, indexPythonRouting).
const (
	// keyPyRouting + a dotted module name + NUL + a module-level binding's
	// qualified name: the open routing object that binding holds, Aux the
	// directory the name is read from, Sig its routePayload.
	keyPyRouting = "pr\x00"
	// keyPyNamedSettings + a dotted module name: a member file naming that
	// module in its DJANGO_SETTINGS_MODULE, which makes it a live settings
	// module.
	keyPyNamedSettings = "ps\x00"
)

// routePayload is what one open object of a file holds, as the module index
// carries it: the object, its routes and the mounts made on it, each mount
// with the key its argument is looked up under.
type routePayload struct {
	Path     string         `json:"p"`
	Object   RouteObject    `json:"o"`
	Routes   []RouteEntry   `json:"r,omitempty"`
	Mounts   []payloadMount `json:"m,omitempty"`
	Language string         `json:"l"`
}

// payloadMount is a mount and the lookup its argument makes.
type payloadMount struct {
	RouteMount
	Key   string `json:"k"`
	Exact bool   `json:"x,omitempty"`
}

// mountKey is the module index key a mount's argument is looked up under,
// and whether it is a relative module answered by the repository root
// alone: an included urlconf's `urlpatterns`; a module attribute
// (`users.router`, a name `from routers.users import router` binds, which
// the worker spells as the TypeModule "routers.users.router"); false when
// the argument names neither.
func mountKey(t *Table, m *RouteMount) (string, bool, bool) {
	if m.Module != "" {
		return m.Module + "\x00" + djangoURLConf, false, true
	}
	ty, ok := mountArgument(t, m)
	if !ok || ty.Kind != wire.TypeModule || ty.Name == "" {
		return "", false, false
	}
	name := ty.Name
	dots := len(name) - len(strings.TrimLeft(name, "."))
	rest := name[dots:]
	j := strings.LastIndexByte(rest, '.')
	mod, attr := name[:dots], rest
	if j >= 0 {
		mod, attr = name[:dots]+rest[:j], rest[j+1:]
	}
	if attr == "" || mod == "" {
		return "", false, false
	}
	exact := dots > 0
	if exact {
		if !pythonRelativeFits(t.Path, mod) {
			return "", false, false
		}
		if mod = absolutePythonModule(t.Path, mod); mod == "" {
			return "", false, false
		}
	}
	return mod + "\x00" + attr, exact, true
}

// mountArgument is the type of the argument a mount passes (its argument Arg
// of site Site), false when the mount names no argument of t.
func mountArgument(t *Table, m *RouteMount) (wire.Type, bool) {
	if m.Module != "" || m.Site < 0 || int(m.Site) >= len(t.Sites) || m.Arg < 0 || int(m.Arg) >= len(t.Sites[m.Site].Args) {
		return wire.Type{}, false
	}
	return t.Sites[m.Site].Args[m.Arg], true
}

// indexPythonRouting adds a Python member's routing to the module index:
// each open object under every dotted name its file can be imported as,
// with its payload, and each settings module the file names.
func indexPythonRouting(file int32, t *Table, add func(entry) error) error {
	rt := &t.Routing
	for _, s := range rt.Settings {
		if err := add(entry{Key: keyPyNamedSettings + s, Ref: Ref{File: file, Index: -1}, Aux: t.Path}); err != nil {
			return err
		}
	}
	if len(rt.Objects) == 0 {
		return nil
	}
	payloads := make([]string, len(rt.Objects))
	for i, o := range rt.Objects {
		p := routePayload{Path: t.Path, Object: o, Language: t.Language}
		for _, r := range rt.Routes {
			if r.Object == int32(i) {
				p.Routes = append(p.Routes, r)
			}
		}
		for k := range rt.Mounts {
			m := &rt.Mounts[k]
			if m.Object != int32(i) || m.Root {
				continue
			}
			key, exact, ok := mountKey(t, m)
			if !ok {
				continue
			}
			pm := payloadMount{RouteMount: *m, Key: key, Exact: exact}
			// The walk reads the mount's key, never its range or the site
			// it was read from: none enters the module index, so an edit
			// that only moves the mount re-runs no reader.
			pm.Range, pm.Site, pm.Arg = model.SourceRange{}, 0, 0
			p.Mounts = append(p.Mounts, pm)
		}
		raw, err := json.Marshal(p)
		if err != nil {
			return internalErr("routing payload: " + err.Error())
		}
		payloads[i] = string(raw)
	}
	return pythonModuleNames(t.Path, func(name, root string) error {
		for i, o := range rt.Objects {
			if o.Decl < 0 || int(o.Decl) >= len(t.Decls) || t.Decls[o.Decl].Qualified == "" {
				continue
			}
			e := entry{Key: keyPyRouting + name + "\x00" + t.Decls[o.Decl].Qualified, Ref: Ref{File: file, Index: o.Decl},
				Aux: root, Sig: payloads[i]}
			if err := add(e); err != nil {
				return err
			}
		}
		return nil
	})
}

// routeFact is the payload of one FactRoute (Binding.External): one
// endpoint the walk found below a root mount of the site's file, which
// the publication mints. Identity separates two endpoints of one mount
// that share a descriptor (the path, or for an unresolved one the route's
// file and object).
type routeFact struct {
	Framework string `json:"f"`
	Method    string `json:"m,omitempty"`
	Route     string `json:"r"`
	Path      string `json:"p,omitempty"`
	Identity  string `json:"i"`
	Set       int64  `json:"n,omitempty"`
}

// routeFound is one route the walk reached from the root mount mount,
// before its set is counted.
type routeFound struct {
	mount    int32
	file     int32
	path     string
	object   string
	route    RouteEntry
	full     string
	known    bool
	handlers []Ref
}

// routeHop is one object a walk descends through: its file and binding.
type routeHop struct{ file, decl int32 }

// emitPythonRoutes walks down from each mount of the member at ordinal file
// that is anchored at an application root (a mount on a root, or, for a
// live settings module, its ROOT_URLCONF) and emits one FactRoute per route
// it reaches and handler, on the mount: every lookup goes through mods, so
// the node being emitted (the file's) records it. A walk follows each mount
// through the module index to the objects its argument names (the nearest,
// as an import's lookup keeps them; several give several paths), joins the
// mount's prefix to the object's own (or replaces it, for a Flask
// registration's `url_prefix=`), and descends into the mounts made on that
// object; a binding already on the walk's way ends it, so a cycle ends. A
// prefix that is not a literal leaves every route below it unresolved, and
// such a route is still published, with no path. The walk is bounded by the
// mounts the unit holds, never by a depth.
func emitPythonRoutes(file int32, t *Table, mods *index, emit func(Binding) error) error {
	rt := &t.Routing
	if len(rt.Mounts) == 0 {
		return nil
	}
	live := false
	// found holds the routes every root mount of the file reaches, so a
	// route two mounts reach carries the size of the whole set.
	var found []routeFound
	for i := range rt.Mounts {
		m := &rt.Mounts[i]
		if m.Object >= 0 {
			continue
		}
		if m.Root && !live {
			named, err := settingsNamed(t.Path, mods)
			if err != nil {
				return err
			}
			if !named {
				continue
			}
			live = true
		}
		key, exact, ok := mountKey(t, m)
		if !ok {
			continue
		}
		n := len(found)
		w := routeWalk{mods: mods, found: &found}
		if err := w.mount(t.Path, payloadMount{RouteMount: *m, Key: key, Exact: exact}, m.Base, m.BaseKnown, nil); err != nil {
			return err
		}
		for k := n; k < len(found); k++ {
			found[k].mount = int32(i)
		}
	}
	return emitRoutes(file, found, emit)
}

// settingsNamed reports that some member names the module of the file at p
// in its DJANGO_SETTINGS_MODULE, under any dotted name the file can be
// imported as.
func settingsNamed(p string, mods *index) (bool, error) {
	named := false
	err := pythonModuleNames(p, func(name, _ string) error {
		return mods.lookup(keyPyNamedSettings+name, func(entry) error { named = true; return nil })
	})
	return named, err
}

// routeWalk is one walk down from a root mount.
type routeWalk struct {
	mods  *index
	found *[]routeFound
}

// mount follows mount m written in the file at from, under the parent
// prefix base, past the bindings on the way.
func (w *routeWalk) mount(from string, m payloadMount, base string, known bool, way []routeHop) error {
	var hits []entry
	near := false
	err := w.mods.lookup(keyPyRouting+m.Key, func(e entry) error {
		hits = append(hits, e)
		near = near || hitNear(from, e.Aux, m.Exact)
		return nil
	})
	if err != nil {
		return err
	}
	for _, e := range hits {
		if !(hitNear(from, e.Aux, m.Exact) || !near && !m.Exact) {
			continue
		}
		hop := routeHop{file: e.Ref.File, decl: e.Ref.Index}
		if slices.Contains(way, hop) {
			continue
		}
		var p routePayload
		if err := json.Unmarshal([]byte(e.Sig), &p); err != nil {
			return internalErr("routing payload: " + err.Error())
		}
		if p.Object.Framework != m.Framework {
			continue
		}
		pre, preKnown := joinPrefix(m.Prefix, p.Object.Own), m.PrefixKnown && p.Object.OwnKnown
		if m.Override {
			pre, preKnown = m.Prefix, m.PrefixKnown
		}
		full, fullKnown := joinPrefix(base, pre), known && preKnown
		object := m.Key[strings.IndexByte(m.Key, 0)+1:]
		for _, r := range p.Routes {
			hs, err := w.handlers(e.Ref.File, p.Path, r)
			if err != nil {
				return err
			}
			*w.found = append(*w.found, routeFound{file: e.Ref.File, path: p.Path, object: object, route: r,
				full: joinRoute(full, r.Rel), known: fullKnown && r.Known, handlers: hs})
		}
		next := append(slices.Clone(way), hop)
		for _, cm := range p.Mounts {
			if err := w.mount(p.Path, cm, joinPrefix(full, cm.Base), fullKnown && cm.BaseKnown, next); err != nil {
				return err
			}
		}
	}
	return nil
}

// handlers are the declarations route r of the member at ordinal file
// (path p) routes to: those it names in its own file, and each one an
// import names, looked up in the module index (the nearest, as an import's
// lookup keeps them).
func (w *routeWalk) handlers(file int32, p string, r RouteEntry) ([]Ref, error) {
	out := make([]Ref, 0, len(r.Handlers)+len(r.Imported))
	for _, h := range r.Handlers {
		out = append(out, Ref{File: file, Index: h})
	}
	for _, raw := range r.Imported {
		mod, name, ok := strings.Cut(raw, "\x00")
		if !ok || name == "" {
			continue
		}
		exact := strings.HasPrefix(mod, ".")
		if exact {
			if !pythonRelativeFits(p, mod) {
				continue
			}
			mod = absolutePythonModule(p, mod)
		}
		if mod == "" {
			continue
		}
		var hits []entry
		near := false
		err := w.mods.lookup(keyPyModule+mod+"\x00"+name, func(e entry) error {
			if e.Ref.Index >= 0 {
				hits = append(hits, e)
				near = near || hitNear(p, e.Aux, exact)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		for _, e := range hits {
			if hitNear(p, e.Aux, exact) || !near && !exact {
				out = append(out, e.Ref)
			}
		}
	}
	slices.SortFunc(out, compareRef)
	return slices.Compact(out), nil
}

// hitNear reports a hit read from the looking-up file's own source root: a
// relative module's from the repository root alone, any other's from a
// directory the file sits under.
func hitNear(from, root string, exact bool) bool {
	if exact {
		return root == ""
	}
	return strings.HasPrefix(from, root)
}

// joinPrefix joins two prefixes with one slash between them.
func joinPrefix(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	}
	return strings.TrimSuffix(a, "/") + "/" + strings.TrimPrefix(b, "/")
}

// joinRoute is a route's full path under prefix: an empty route is the
// prefix itself, the root when there is none.
func joinRoute(prefix, route string) string {
	if route == "" {
		return cmp.Or(prefix, "/")
	}
	return joinPrefix(prefix, route)
}

// emitRoutes emits the FactRoutes of the file's root mounts, each on the
// mount it was reached from: one per route reached and handler (one with no
// target for a route with no handler), each with its set, the number of
// paths the walks gave the same route.
func emitRoutes(file int32, found []routeFound, emit func(Binding) error) error {
	type routeKey struct {
		file   int32
		object string
		route  string
		method string
	}
	set := map[routeKey]int64{}
	for _, f := range found {
		set[routeKey{f.file, f.object, f.route.Route, f.route.Method}]++
	}
	for _, f := range found {
		rf := routeFact{Framework: f.route.Framework, Method: f.route.Method, Route: f.route.Route}
		if f.known {
			rf.Path, rf.Identity = f.full, f.full
		} else {
			rf.Identity = "unresolved:" + f.path + "#" + f.object
		}
		if n := set[routeKey{f.file, f.object, f.route.Route, f.route.Method}]; n >= 2 {
			rf.Set = n
		}
		raw, err := json.Marshal(rf)
		if err != nil {
			return internalErr("route fact: " + err.Error())
		}
		b := Binding{Kind: FactRoute, Site: Ref{File: file, Index: f.mount}, Target: Ref{File: -1, Index: -1},
			Detail: DetailImport, External: string(raw)}
		if len(f.handlers) == 0 {
			if err := emit(b); err != nil {
				return err
			}
			continue
		}
		for _, h := range f.handlers {
			b.Target = h
			if err := emit(b); err != nil {
				return err
			}
		}
	}
	return nil
}

// routes publishes the endpoints the FactRoutes of mount m, group, found:
// one `endpoint` node per distinct route fact, in this file's scope,
// located at the mount, keyed by its descriptor and its identity (its full
// path, or for an unresolved one the route's file and object), with the
// route's metadata, defined by the file's module and routing to each
// handler, with the set size when the route reached several paths.
func (f *filePublication) routes(m *RouteMount, group []idBinding) error {
	var order []string
	handlers := map[string][]idBinding{}
	for _, b := range group {
		if b.Kind != FactRoute {
			continue
		}
		if _, seen := handlers[b.External]; !seen {
			order = append(order, b.External)
		}
		handlers[b.External] = append(handlers[b.External], b)
	}
	for _, raw := range order {
		var rf routeFact
		if err := json.Unmarshal([]byte(raw), &rf); err != nil {
			return internalErr("route fact: " + err.Error())
		}
		meta := model.EndpointMetadata{Framework: rf.Framework, Method: rf.Method, Route: rf.Route,
			Prefix: model.PrefixUnresolved}
		if rf.Path != "" {
			meta.Prefix, meta.Path = model.PrefixResolved, rf.Path
		}
		if err := meta.Validate(); err != nil {
			// A route or a path over its bound is not published, as the
			// structural provider leaves it out.
			continue
		}
		body, err := json.Marshal(meta)
		if err != nil {
			return internalErr("endpoint metadata: " + err.Error())
		}
		descriptor := rf.Route
		if rf.Method != "" {
			descriptor = rf.Method + " " + rf.Route
		}
		n, err := f.mint(model.NodeCandidate{ProviderID: ProviderID, ScopeKey: lang.ScopePrefix + f.fv.Path,
			NativeKey: fitKey("endpoint:" + descriptor + "@" + rf.Identity), Kind: model.NodeEndpoint,
			Language: f.t.Language, Name: nodeName(descriptor, "endpoint")}, m.Range, body)
		if err != nil {
			return err
		}
		f.node(occurrenceOf(f.fv, f.t, f.t.Module))
		f.relation(f.t.Module.Node, model.RelDefines, n, located(m.Range), "", "", "", 0)
		for _, b := range handlers[raw] {
			if b.Target != "" {
				f.relation(n, model.RelRoutesTo, b.Target, located(m.Range), "", b.Detail, "", rf.Set)
			}
		}
	}
	return nil
}

// summary is the routing as the interface summary hashes it: every range
// cleared, since no summary covers a byte range, and every mount's site and
// argument index, which only locate the argument the interface hashes
// beside the routing (Interface): a call inserted before a mount shifts
// them and changes nothing another file reads.
func (r Routing) summary() Routing {
	if len(r.Mounts) == 0 {
		return r
	}
	r.Mounts = slices.Clone(r.Mounts)
	for i := range r.Mounts {
		r.Mounts[i].Range, r.Mounts[i].Site, r.Mounts[i].Arg = model.SourceRange{}, 0, 0
	}
	return r
}
