package treesitter

import (
	"cmp"
	"regexp"
	"slices"
	"strings"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider/link"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/wire"
)

// routeLinks is the routing a Python file's route table leaves open, for
// the linker to join across files (link.Routing): the open objects, the
// routes on them and the mounts of objects this file does not build, each
// mount by its worker site until linkRouting maps it into the table's sites.
type routeLinks struct {
	routing link.Routing
	// sites is each mount's worker site index, -1 for one that names no
	// site (a Django include or root urlconf).
	sites []int
}

// pendingMount is a mount call whose mounted object is an argument this
// file does not build (`app.include_router(users.router)`).
type pendingMount struct {
	site, arg int
	fw        string
	parent    *routeObj
	prefix    string
	known     bool
	override  bool
	// asgi reports an ASGI mount (Starlette's Mount, `app.mount`), which
	// mounts any ASGI application, router or route list.
	asgi bool
	// target is, for a mount whose argument the file reads as written,
	// the module and attribute it names, NUL-joined; site is then -1.
	target string
	// join is how a Go mount reaches the callable it mounts the routing of
	// (link.MountJoin); function is a JoinFunction's function value as
	// written.
	join     link.MountJoin
	function string
	at       wire.Span
}

// routeChain is one way an object reaches the top of its chain inside the
// file: top, an open object (nil for an application root), and the prefix
// between, absolute when top is nil, else below top's own.
type routeChain struct {
	top    *routeObj
	prefix string
	known  bool
	// root is the application root a chain with no top is anchored at,
	// nil when the program serves it.
	root *routeObj
}

// chains are the ways o reaches the top of its in-file chain: an open
// object (no attachment, no root) is its own top; a root, or an object the
// file serves, is anchored at the application with its prefix; an object
// attached once continues through its parent. An object attached several
// times, or along a cycle, has no single in-file chain and is left to its
// in-file resolution (unresolved), never guessed.
func (s *routeScan) chains(o *routeObj, way []*routeObj) (routeChain, bool) {
	if o == nil || slices.Contains(way, o) {
		return routeChain{}, false
	}
	switch len(o.atts) {
	case 0:
		if o.root {
			return routeChain{prefix: o.own, known: o.ownKnown, root: o}, true
		}
		return routeChain{top: o, known: true}, true
	case 1:
	default:
		return routeChain{}, false
	}
	a := o.atts[0]
	pre, known := a.prefix, a.known
	if !a.override {
		pre, known = joinPrefix(pre, o.own), known && o.ownKnown
	}
	if a.served {
		return routeChain{prefix: pre, known: known}, true
	}
	up, ok := s.chains(a.parent, append(way, o))
	if !ok {
		return routeChain{}, false
	}
	return routeChain{top: up.top, root: up.root, prefix: joinPrefix(up.prefix, pre), known: up.known && known}, true
}

// linkRoutes builds the file's open routing: each open object a
// module-level binding holds, each route whose chain reaches one, each
// mount whose parent's chain resolves and whose mounted object is an
// argument the file does not build, and Django's urlconf, includes and
// settings chain (djangoLinks).
func (s *routeScan) linkRoutes() *routeLinks {
	out := &routeLinks{}
	index := map[*routeObj]int32{}
	open := func(o *routeObj) (int32, bool) {
		if i, ok := index[o]; ok {
			return i, true
		}
		d := o.bound - 1
		if d < 0 {
			i, ok := s.objSite(o)
			if !ok {
				return -1, false
			}
			r := s.siteRef(i)
			if r == nil {
				return -1, false
			}
			if d = s.binding(refSpan(r)); d < 0 {
				return -1, false
			}
		}
		out.routing.Objects = append(out.routing.Objects, link.RouteObject{Decl: int32(d), Framework: o.fw, Own: o.own,
			OwnKnown: o.ownKnown, Root: o.root})
		index[o] = int32(len(out.routing.Objects) - 1)
		return index[o], true
	}
	for _, f := range s.out {
		if f.obj == nil {
			continue
		}
		c, ok := s.chains(f.obj, nil)
		if !ok {
			continue
		}
		top := c.top
		switch {
		case top == nil && asgiRoot(c.root):
			// An ASGI application another file may mount: its routes
			// are kept below it, beside the paths this file resolves.
			top = c.root
		case top == nil || f.meta.Prefix == model.PrefixResolved:
			continue
		}
		oi, ok := open(top)
		if !ok {
			continue
		}
		route := f.meta.Route
		out.routing.Routes = append(out.routing.Routes, link.RouteEntry{Object: oi, Framework: f.meta.Framework,
			Method: f.meta.Method, Route: route, Rel: joinPrefix(c.prefix, f.value), Known: c.known && f.literal,
			Handlers: s.declHandlers(f.handlers), Imported: s.importedHandlers(f.handlers)})
	}
	for _, m := range s.pending {
		c, ok := s.chains(m.parent, nil)
		if !ok {
			continue
		}
		mount := link.RouteMount{Site: -1, Arg: int32(m.arg), Framework: m.fw, Prefix: m.prefix, PrefixKnown: m.known,
			Override: m.override, Object: -1, Base: c.prefix, BaseKnown: c.known, ASGI: m.asgi, Target: m.target}
		if c.top != nil {
			oi, ok := open(c.top)
			if !ok {
				continue
			}
			mount.Object = oi
		} else if asgiRoot(c.root) {
			if oi, ok := open(c.root); ok {
				mount.RootObject = oi + 1
			}
		}
		rng, err := s.b.rangeOf(m.at.Start, m.at.End)
		if err != nil {
			continue
		}
		mount.Range = *rng
		out.routing.Mounts = append(out.routing.Mounts, mount)
		out.sites = append(out.sites, m.site)
	}
	s.djangoLinks(out)
	if len(out.routing.Objects)+len(out.routing.Mounts)+len(out.routing.Settings) == 0 {
		return nil
	}
	return out
}

// objSite is the worker site that builds o.
func (s *routeScan) objSite(o *routeObj) (int, bool) {
	for i, x := range s.objs {
		if x == o {
			return i, true
		}
	}
	return -1, false
}

// declHandlers are the declarations of this file a route's handlers name:
// a declaration, or a bare name the file declares exactly once at module
// level.
func (s *routeScan) declHandlers(hs []handler) []int32 {
	var out []int32
	for _, h := range hs {
		switch {
		case h.decl >= 0:
			out = append(out, int32(h.decl))
		case h.ref != nil && !h.ref.Qualified:
			found := -1
			for _, d := range s.b.byName[h.ref.Name] {
				if s.b.decls[d].Parent < 0 {
					if found >= 0 {
						found = -2
						break
					}
					found = d
				}
			}
			if found >= 0 {
				out = append(out, int32(found))
			}
		}
	}
	return out
}

// importedHandlers are the handlers a route names through an import of
// this file: a name `from m import f` binds (`f`), or a name of a module an
// import binds (`views.f` after `from . import views` or `import views`),
// each spelled as the module the import writes and the name, joined by a
// NUL.
func (s *routeScan) importedHandlers(hs []handler) []string {
	var out []string
	for _, h := range hs {
		if h.ref == nil {
			continue
		}
		r := h.ref
		if !r.Qualified {
			imp, ok := s.imports[r.Name]
			if !ok || imp.path == "" {
				continue
			}
			out = append(out, imp.path+"\x00"+cmp.Or(imp.original, r.Name))
			continue
		}
		imp, ok := s.imports[r.Qualifier]
		if !ok || imp.path == "" || strings.Contains(r.Qualifier, ".") {
			continue
		}
		mod := imp.path
		if s.fromImport(r.Qualifier) {
			// `from m import views` binds the submodule m.views.
			name := cmp.Or(imp.original, r.Qualifier)
			if strings.Trim(mod, ".") == "" {
				mod += name
			} else {
				mod += "." + name
			}
		}
		out = append(out, mod+"\x00"+r.Name)
	}
	return out
}

// fromImport reports a local name a `from … import` statement binds, as
// the statement is written.
func (s *routeScan) fromImport(name string) bool {
	for _, imp := range s.b.ex.imports {
		if imp.End > uint32(len(s.b.src)) || imp.Start > imp.End || !slices.Contains(imp.Names, name) {
			continue
		}
		return strings.HasPrefix(string(s.b.src[imp.Start:imp.End]), "from")
	}
	return false
}

// recordMount records a mount call whose argument at position arg is not an
// object this file builds but a module attribute (the worker's TypeModule),
// which the linker resolves.
func (s *routeScan) recordMount(r *wire.Ref, site *wire.Site, i, arg int, fw string, parent *routeObj, prefix string,
	known, override bool) {
	if parent == nil || argType(site, arg).Kind != wire.TypeModule {
		return
	}
	s.pending = append(s.pending, pendingMount{site: i, arg: arg, fw: fw, parent: parent, prefix: prefix, known: known,
		override: override, at: refSpan(r)})
}

// recordASGIMount is recordMount for an ASGI mount. A keyword argument,
// which the worker types by no position, is read as written: a dotted name
// whose head an import binds names that module's attribute.
func (s *routeScan) recordASGIMount(r *wire.Ref, site *wire.Site, i, arg int, fw string, parent *routeObj, prefix string,
	known bool) {
	n := len(s.pending)
	s.recordMount(r, site, i, arg, fw, parent, prefix, known, false)
	if len(s.pending) > n {
		s.pending[n].asgi = true
		return
	}
	args := s.siteArgs(site)
	if parent == nil || arg < 0 || arg >= len(args) || args[arg].kw == "" || args[arg].kw == "*" {
		return
	}
	parts := strings.Split(args[arg].text, ".")
	for _, p := range parts {
		if !pyName(p) {
			return
		}
	}
	target, ok := (&inputScan{b: s.b, rs: s}).importedName(parts)
	if !ok {
		return
	}
	s.pending = append(s.pending, pendingMount{site: -1, arg: -1, fw: fw, parent: parent, prefix: prefix, known: known,
		asgi: true, target: target, at: refSpan(r)})
}

// asgiRoot reports an ASGI application root (FastAPI's or Starlette's),
// which another file may mount under a path.
func asgiRoot(o *routeObj) bool {
	return o != nil && o.root && asgiFramework(o.fw)
}

// asgiFramework reports an ASGI framework: a FastAPI application is a
// Starlette application, and either mounts the other's objects.
func asgiFramework(fw string) bool { return fw == fwFastAPI || fw == fwStarlette }

// djangoContainer is a list of Django urlconf entries: the module's
// `urlpatterns` (top), or a list an entry includes in place
// (`include([...])`) or through a module-level binding (`include(extra)`),
// mounted at the including entry's route.
type djangoContainer struct {
	at        wire.Span
	top       bool
	including wire.Span
	prefix    string
	known     bool
}

// djangoLinks adds the file's Django routing: the `urlpatterns` binding as
// an open object, the routes its entries reach with the prefixes of the
// lists that include them, each `include("dotted.module")` as a mount of
// that module's urlconf, the module the file's literal ROOT_URLCONF names
// as a root mount, and the settings modules its literal
// DJANGO_SETTINGS_MODULE names.
func (s *routeScan) djangoLinks(out *routeLinks) {
	s.djangoSettings(out)
	if !s.frameworks[fwDjango] {
		return
	}
	conf := -1
	for i := range s.b.decls {
		d := &s.b.decls[i]
		if d.Parent < 0 && d.kind == model.NodeVariable && d.Name == "urlpatterns" {
			if conf >= 0 {
				return
			}
			conf = i
		}
	}
	if conf < 0 {
		return
	}
	cd := &s.b.decls[conf]
	containers := []djangoContainer{{at: wire.Span{Start: cd.Start, End: cd.End}, top: true, known: true}}
	for _, e := range s.djangoEntries {
		if e.include == nil {
			continue
		}
		pre, known := e.route.value, e.route.literal && !e.regex
		switch {
		case e.include.list.End > 0:
			containers = append(containers, djangoContainer{at: e.include.list, including: e.at, prefix: pre, known: known})
		case e.include.binding != "":
			if at, ok := s.moduleBinding(e.include.binding); ok {
				containers = append(containers, djangoContainer{at: at, including: e.at, prefix: pre, known: known})
			}
		}
	}
	object := int32(len(out.routing.Objects))
	out.routing.Objects = append(out.routing.Objects, link.RouteObject{Decl: int32(conf), Framework: fwDjango, OwnKnown: true})
	used := false
	for _, e := range s.djangoEntries {
		for _, c := range djangoChains(containers, e.at, nil) {
			known := c.known && e.route.literal && !e.regex
			if e.include == nil {
				out.routing.Routes = append(out.routing.Routes, link.RouteEntry{Object: object, Framework: fwDjango,
					Route: e.route.route(), Rel: joinPrefix(c.prefix, e.route.value), Known: known,
					Handlers: s.declHandlers(e.handlers), Imported: s.importedHandlers(e.handlers)})
				used = true
				continue
			}
			if e.include.module == "" {
				continue
			}
			rng, err := s.b.rangeOf(e.at.Start, e.at.End)
			if err != nil {
				continue
			}
			out.routing.Mounts = append(out.routing.Mounts, link.RouteMount{Site: -1, Arg: -1, Module: e.include.module,
				Framework: fwDjango, Prefix: e.route.value, PrefixKnown: e.route.literal && !e.regex, Object: object,
				Base: c.prefix, BaseKnown: c.known, Range: *rng})
			out.sites = append(out.sites, -1)
			used = true
		}
	}
	if !used {
		out.routing.Objects = out.routing.Objects[:object]
	}
}

// djangoChains are the prefixes the entry at reaches the top urlconf by:
// through each list around it that holds it, innermost first, and each
// entry including that list; a list included from two entries gives two.
func djangoChains(cs []djangoContainer, at wire.Span, way []int) []routeChain {
	best := -1
	for i, c := range cs {
		if c.at.Start <= at.Start && at.End <= c.at.End && c.at != at &&
			(best < 0 || c.at.End-c.at.Start < cs[best].at.End-cs[best].at.Start) {
			best = i
		}
	}
	if best < 0 {
		return nil
	}
	var out []routeChain
	for i, c := range cs {
		if c.at != cs[best].at || slices.Contains(way, i) {
			continue
		}
		if c.top {
			out = append(out, routeChain{known: true})
			continue
		}
		for _, up := range djangoChains(cs, c.including, append(way, i)) {
			out = append(out, routeChain{prefix: joinPrefix(up.prefix, c.prefix), known: up.known && c.known})
		}
	}
	return out
}

// moduleBinding is the bytes of the one module-level binding named name.
func (s *routeScan) moduleBinding(name string) (wire.Span, bool) {
	found := -1
	for i := range s.b.decls {
		d := &s.b.decls[i]
		if d.Parent < 0 && d.kind == model.NodeVariable && d.Name == name {
			if found >= 0 {
				return wire.Span{}, false
			}
			found = i
		}
	}
	if found < 0 {
		return wire.Span{}, false
	}
	d := &s.b.decls[found]
	return wire.Span{Start: d.Start, End: d.End}, true
}

// djangoSettingsAssign is the literal subscript form of naming the settings
// module, `os.environ["DJANGO_SETTINGS_MODULE"] = "mysite.settings"`.
var djangoSettingsAssign = regexp.MustCompile(`(?m)^[ \t]*os\.environ\[\s*["']DJANGO_SETTINGS_MODULE["']\s*\]\s*=\s*["']([A-Za-z_][\w.]*)["']`)

// djangoSettings reads the file's Django settings chain: the settings
// modules a literal DJANGO_SETTINGS_MODULE names, through
// `os.environ.setdefault("DJANGO_SETTINGS_MODULE", "x")` or
// `os.environ["DJANGO_SETTINGS_MODULE"] = "x"` in a file that imports os, and
// the root urlconf a module-level literal ROOT_URLCONF names, a root mount
// the linker walks when another file names this module as the settings.
// Anything that is not a literal names nothing.
func (s *routeScan) djangoSettings(out *routeLinks) {
	if imp, ok := s.imports["os"]; ok && imp.path == "os" {
		for _, m := range djangoSettingsAssign.FindAllSubmatch(s.b.src, -1) {
			out.routing.Settings = append(out.routing.Settings, string(m[1]))
		}
		s.calls(func(r *wire.Ref, site *wire.Site, _ int) {
			if !r.Qualified || r.Name != "setdefault" || r.Qualifier != "os.environ" {
				return
			}
			args := positional(s.siteArgs(site))
			if len(args) != 2 {
				return
			}
			k, ok1 := stringLiteral("python", args[0].text)
			v, ok2 := stringLiteral("python", args[1].text)
			if ok1 && ok2 && k == "DJANGO_SETTINGS_MODULE" && pyDotted(v) {
				out.routing.Settings = append(out.routing.Settings, v)
			}
		})
		slices.Sort(out.routing.Settings)
		out.routing.Settings = slices.Compact(out.routing.Settings)
	}
	for i := range s.b.decls {
		d := &s.b.decls[i]
		if d.Parent >= 0 || d.kind != model.NodeVariable || d.Name != "ROOT_URLCONF" || d.NameEnd > d.End {
			continue
		}
		value, ok := strings.CutPrefix(strings.TrimSpace(string(s.b.src[d.NameEnd:d.End])), "=")
		if !ok {
			continue
		}
		v, lit := stringLiteral("python", value)
		if !lit || !pyDotted(v) {
			continue
		}
		rng, err := s.b.rangeOf(d.Start, d.End)
		if err != nil {
			continue
		}
		out.routing.Mounts = append(out.routing.Mounts, link.RouteMount{Site: -1, Arg: -1, Module: v, Root: true,
			Framework: fwDjango, PrefixKnown: true, Object: -1, Base: "/", BaseKnown: true, Range: *rng})
		out.sites = append(out.sites, -1)
	}
}

// pyDotted reports a dotted Python module name, `mysite.settings`.
func pyDotted(v string) bool {
	if v == "" {
		return false
	}
	for p := range strings.SplitSeq(v, ".") {
		if !pyName(p) {
			return false
		}
	}
	return true
}

// linkRouting is the file's open routing as the link table carries it,
// each mount's worker site mapped to the table's site (siteAt) and each Go
// mount given its call (goMountCall); a mount whose site joined no call
// reference is dropped.
func (b *builder) linkRouting(siteAt []int32, sites []link.Site) link.Routing {
	if b.links == nil {
		return link.Routing{}
	}
	r := b.links.routing
	mounts := r.Mounts[:0:0]
	for k, m := range r.Mounts {
		if w := b.links.sites[k]; w >= 0 {
			if w >= len(siteAt) || siteAt[w] < 0 {
				continue
			}
			m.Site = siteAt[w]
		}
		if !goMountCall(&m, sites) {
			continue
		}
		mounts = append(mounts, m)
	}
	r.Mounts = mounts
	return r
}

// mayNameSettings reports a Python file that can hold a piece of Django's
// settings chain: one importing os, or binding ROOT_URLCONF at module level.
func (b *builder) mayNameSettings() bool {
	for _, imp := range b.ex.imports {
		if imp.Path == "os" {
			return true
		}
	}
	for i := range b.decls {
		if b.decls[i].Parent < 0 && b.decls[i].Name == "ROOT_URLCONF" {
			return true
		}
	}
	return false
}
