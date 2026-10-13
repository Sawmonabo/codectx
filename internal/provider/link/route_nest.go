package link

import (
	"encoding/json"
	"slices"
	"strings"
)

// NestJS routes (docs/providers-link.md, NestJS paths). Nest serves a
// route at the path RoutePathFactory.create builds: the URI version, the
// module's RouterModule path, the controller's path and the method's path,
// each joined as `stripEndSlash(a) + addLeadingSlash(b)` with every array
// element a path of its own, then the application's global prefix
// (packages/core/router/route-path-factory.ts). The controller's file
// holds the controller's and the method's paths and versions; the global
// prefix and the versioning are set on the application another file
// creates (`NestFactory.create`), and the RouterModule path on the module
// whose `controllers` list the controller, by a `RouterModule.register`
// route tree (packages/core/router/router-module.ts). The structural
// provider records the first in the controller's table (Routing.Nest) and
// the rest in the table of the file that creates, declares or registers
// them (Routing.NestApp); the linker joins them when every application and
// module path the unit has is decided, and publishes nothing otherwise,
// leaving the structural provider's `unresolved` endpoint as the route's
// only one.

// NestRoute is one NestJS route decorator of the file on a method of a
// `@Controller` class: its method and route as written, the controller's
// paths and the decorator's own (each as written in a literal, before
// Nest's leading slash), Known false when either is no literal, the
// versions the method's `@Version` and the controller's `version` name,
// and the handler, the decorated method (an index of Decls).
type NestRoute struct {
	Method      string       `json:"method,omitempty"`
	Route       string       `json:"route"`
	Controllers []string     `json:"controllers,omitempty"`
	Paths       []string     `json:"paths,omitempty"`
	Known       bool         `json:"known,omitempty"`
	Version     *NestVersion `json:"version,omitempty"`
	Controller  *NestVersion `json:"controller_version,omitempty"`
	Handler     int32        `json:"handler"`
}

// NestVersion is a version a decorator or an option names: each version a
// string or an array of them writes, NestNeutral for VERSION_NEUTRAL;
// Known false when it is no literal. An empty Values with Known is a
// version written as the empty string, which Nest reads as none.
type NestVersion struct {
	Values []string `json:"values,omitempty"`
	Known  bool     `json:"known,omitempty"`
}

// NestNeutral is VERSION_NEUTRAL among a version's values: a route served
// with no version segment.
const NestNeutral = "\x00neutral"

// NestApp is what a file sets on the NestJS applications it creates and
// declares of their modules: Apps is how many `NestFactory.create` calls it
// makes; Prefixes are its `setGlobalPrefix` calls on one of them, in source
// order, and Versioning its `enableVersioning` calls. Open reports a call
// that sets either on an application the file does not create (a helper's
// parameter), which no table ties to an application. RouterModule reports a
// file naming Nest's RouterModule and Routers its `RouterModule.register`
// calls; Modules are its `@Module` classes whose controllers it can read,
// ModulesUnknown one whose controllers it cannot (route_nest_modules.go).
type NestApp struct {
	Apps           int              `json:"apps,omitempty"`
	Prefixes       []NestPrefix     `json:"prefixes,omitempty"`
	Versioning     []NestVersioning `json:"versioning,omitempty"`
	Open           bool             `json:"open,omitempty"`
	RouterModule   bool             `json:"router_module,omitempty"`
	Routers        []NestRouter     `json:"routers,omitempty"`
	Modules        []NestModule     `json:"modules,omitempty"`
	ModulesUnknown bool             `json:"modules_unknown,omitempty"`
}

// NestPrefix is one `setGlobalPrefix(prefix[, options])` call: Known when
// the prefix is a literal and the call passes no options (an `exclude`
// list is matched against each route at run time).
type NestPrefix struct {
	Value string `json:"value,omitempty"`
	Known bool   `json:"known,omitempty"`
}

// NestVersioning is one `enableVersioning(options)` call, Known when its
// options are literal: URI when the type is VersioningType.URI (the
// default), the only type that puts the version in the path; Prefix the
// segment before a version ("v" unless `prefix:` says otherwise, empty for
// `prefix: false`); Default the `defaultVersion`, nil when it names none.
type NestVersioning struct {
	Known   bool         `json:"known,omitempty"`
	URI     bool         `json:"uri,omitempty"`
	Prefix  string       `json:"prefix,omitempty"`
	Default *NestVersion `json:"default,omitempty"`
}

// keyNestApp: a member's NestApp, as JSON in Sig; every member that sets
// one is read by every NestJS route of the unit.
const keyNestApp = "na\x00"

// nestConfig is one application's global prefix and versioning.
type nestConfig struct {
	prefix     string
	versioning *NestVersioning
}

// addNestApp adds the member's NestApp to the hierarchy's links: its
// settings and flags under keyNestApp, its modules and routers under keys
// of their own (addNestModules).
func addNestApp(links interface{ Add(entry) error }, file int32, t *Table) error {
	if t.Routing.NestApp == nil {
		return nil
	}
	a := *t.Routing.NestApp
	if err := addNestModules(links, file, t, &a); err != nil {
		return err
	}
	a.Routers, a.Modules = nil, nil
	raw, err := json.Marshal(a)
	if err != nil {
		return internalErr("nest application: " + err.Error())
	}
	return links.Add(entry{Key: keyNestApp, Ref: Ref{File: file, Index: -1}, Sig: string(raw)})
}

// nestUnit is what the unit's files set on its NestJS applications: the
// distinct configurations of the applications it creates, and whether
// RouterModule is in use, with its route trees (routers).
type nestUnit struct {
	configs []nestConfig
	router  bool
	routers []nestRouterOf
}

// nestUnitOf reads every member's NestApp: the distinct configurations of
// the applications the unit creates, and false when any is undecided (no
// application created, a setting made where no table ties it to an
// application, a file creating several applications and setting either,
// two settings of one file that disagree, a setting that is no literal)
// or, with RouterModule in use, a module's controllers or a route tree are
// no literal.
func (h *hierarchy) nestUnitOf() (*nestUnit, bool, error) {
	u := &nestUnit{}
	ok, unknownModules := true, false
	err := h.links.lookup(keyNestApp, func(e entry) error {
		var a NestApp
		if err := json.Unmarshal([]byte(e.Sig), &a); err != nil {
			return internalErr("nest application: " + err.Error())
		}
		c, decided := a.config()
		switch {
		case !decided:
			ok = false
		case a.Apps > 0 && !slices.ContainsFunc(u.configs, c.equal):
			u.configs = append(u.configs, c)
		}
		u.router = u.router || a.RouterModule
		unknownModules = unknownModules || a.ModulesUnknown
		return nil
	})
	if err != nil || !ok || len(u.configs) == 0 || u.router && unknownModules {
		return nil, false, err
	}
	if u.router {
		if u.routers, ok, err = h.nestRouters(); err != nil || !ok {
			return nil, false, err
		}
	}
	return u, true, nil
}

// config is the configuration every application a's file creates has.
func (a *NestApp) config() (nestConfig, bool) {
	if a.Open || a.Apps > 1 && len(a.Prefixes)+len(a.Versioning) > 0 {
		return nestConfig{}, false
	}
	var c nestConfig
	for i, p := range a.Prefixes {
		if !p.Known || i > 0 && p.Value != c.prefix {
			return nestConfig{}, false
		}
		c.prefix = p.Value
	}
	for i := range a.Versioning {
		v := &a.Versioning[i]
		if !v.Known || i > 0 && !c.versioning.equal(v) {
			return nestConfig{}, false
		}
		c.versioning = v
	}
	return c, true
}

func (c nestConfig) equal(o nestConfig) bool {
	return c.prefix == o.prefix && c.versioning.equal(o.versioning)
}

func (v *NestVersioning) equal(o *NestVersioning) bool {
	if v == nil || o == nil {
		return v == o
	}
	return v.Known == o.Known && v.URI == o.URI && v.Prefix == o.Prefix && v.Default.equal(o.Default)
}

func (v *NestVersion) equal(o *NestVersion) bool {
	if v == nil || o == nil {
		return v == o
	}
	return v.Known == o.Known && slices.Equal(v.Values, o.Values)
}

// nestRouteFacts emits, for each NestJS route of the member at ordinal file,
// one FactRoute per path the unit's applications serve it at, under each
// RouterModule path of its controller (modulePaths), on the route's own
// mount and to its handler: nothing when any application or module path is
// undecided or the route's paths or version are no literal.
func (h *hierarchy) nestRouteFacts(file int32, t *Table, emit func(Binding) error) error {
	rt := &t.Routing
	if len(rt.Nest) == 0 {
		return nil
	}
	u, ok, err := h.nestUnitOf()
	if err != nil || !ok {
		return err
	}
	modules := map[int32]nestModulePaths{}
	for i := range rt.Mounts {
		m := &rt.Mounts[i]
		if m.Nest <= 0 || int(m.Nest) > len(rt.Nest) {
			continue
		}
		r := &rt.Nest[m.Nest-1]
		if r.Handler < 0 || int(r.Handler) >= len(t.Decls) || t.Decls[r.Handler].Parent < 0 {
			continue
		}
		ctrl := t.Decls[r.Handler].Parent
		if _, done := modules[ctrl]; !done {
			ps, known, err := h.modulePaths(u, file, t, ctrl)
			if err != nil {
				return err
			}
			modules[ctrl] = nestModulePaths{ps, known}
		}
		mps := modules[ctrl]
		var paths []string
		decided := r.Known && mps.known
		for _, c := range u.configs {
			for _, mp := range mps.paths {
				ps, known := nestPaths(r, c, mp)
				decided = decided && known
				for _, p := range ps {
					if !slices.Contains(paths, p) {
						paths = append(paths, p)
					}
				}
			}
		}
		if !decided {
			continue
		}
		var set int64
		if len(paths) > 1 {
			set = int64(len(paths))
		}
		for _, p := range paths {
			raw, err := json.Marshal(routeFact{Framework: nestFramework, Method: r.Method, Route: r.Route, Path: p,
				Identity: p, Set: set})
			if err != nil {
				return internalErr("route fact: " + err.Error())
			}
			b := Binding{Kind: FactRoute, Site: Ref{File: file, Index: int32(i)}, Target: Ref{File: file, Index: r.Handler},
				Detail: DetailDirect, External: string(raw)}
			if err := emit(b); err != nil {
				return err
			}
		}
	}
	return nil
}

// nestFramework is the framework name NestJS endpoints carry.
const nestFramework = "nestjs"

// nestPaths are the paths an application of configuration c serves route
// r at under the RouterModule path modulePath (empty for none), as
// RoutePathFactory.create builds them, and false when the version it needs
// is no literal.
func nestPaths(r *NestRoute, c nestConfig, modulePath string) ([]string, bool) {
	paths := []string{""}
	if v := c.versioning; v != nil && v.URI {
		version := r.Version
		if version == nil || len(version.Values) == 0 && version.Known {
			// `methodVersion || controllerVersion`: a method's empty
			// version is none, and the controller's falls back to the
			// default only when it writes none.
			version = r.Controller
			if version == nil {
				version = v.Default
			}
		}
		if version != nil {
			if !version.Known {
				return nil, false
			}
			if len(version.Values) > 0 {
				var vs []string
				for _, x := range version.Values {
					if x == NestNeutral {
						vs = append(vs, "")
					} else {
						vs = append(vs, "/"+v.Prefix+x)
					}
				}
				paths = vs
			}
		}
	}
	controllers := make([]string, len(r.Controllers))
	for i, p := range r.Controllers {
		controllers[i] = nestLeadingSlash(p)
	}
	methods := make([]string, len(r.Paths))
	for i, p := range r.Paths {
		if p == "" {
			p = "/"
		}
		methods[i] = nestLeadingSlash(p)
	}
	if len(methods) == 0 {
		methods = []string{"/"}
	}
	if modulePath != "" {
		paths = nestAppend(paths, []string{modulePath})
	}
	paths = nestAppend(paths, controllers)
	paths = nestAppend(paths, methods)
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		if c.prefix != "" {
			p = strings.TrimSuffix(c.prefix, "/") + p
		}
		if p == "" {
			p = "/"
		}
		p = nestLeadingSlash(p)
		if p != "/" {
			p = strings.TrimSuffix(p, "/")
		}
		if !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	return out, true
}

// nestAppend is appendToAllIfDefined over an array fragment: every path
// joined with every fragment.
func nestAppend(paths, fragments []string) []string {
	out := make([]string, 0, len(paths)*len(fragments))
	for _, p := range paths {
		for _, f := range fragments {
			out = append(out, strings.TrimSuffix(p, "/")+nestLeadingSlash(f))
		}
	}
	return out
}

// nestLeadingSlash is Nest's addLeadingSlash: a path that starts with
// neither `/` nor `{/` gains a leading slash; the empty path stays empty.
func nestLeadingSlash(p string) string {
	if p == "" || p[0] == '/' || strings.HasPrefix(p, "{/") {
		return p
	}
	return "/" + p
}
