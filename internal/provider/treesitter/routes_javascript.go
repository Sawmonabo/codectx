package treesitter

import (
	"slices"
	"strings"

	"github.com/Sawmonabo/codectx/internal/provider/treesitter/wire"
)

// expressVerbs are the Express routing methods of an application, a router
// and a `route(path)`: each names its HTTP method except `all`, which takes
// any.
var expressVerbs = []string{"get", "post", "put", "delete", "patch", "options", "head", "all"}

// express reads the file's Express routes: `express()` is an application
// root, `express.Router()` (or an imported `Router()`) a router that
// resolves when this file mounts it once with `use`.
func (s *routeScan) express() {
	if !s.frameworks[fwExpress] {
		return
	}
	s.calls(func(r *wire.Ref, site *wire.Site, i int) {
		switch s.jsTarget(site, i) {
		case "express":
			s.objs[i] = &routeObj{fw: fwExpress, root: true, ownKnown: true}
		case "express.Router":
			s.objs[i] = &routeObj{fw: fwExpress, ownKnown: true}
		}
	})
	s.calls(func(r *wire.Ref, site *wire.Site, _ int) {
		if r.Qualified && r.Name == "use" {
			s.expressMount(site)
		}
	})
	s.calls(s.expressRoute)
}

// jsTarget is what an ECMAScript site calls, through the module it was
// imported from: "express" for `express()`, "express.Router" for
// `express.Router()` or `Router()` imported from it; empty when the callee
// is not an imported name.
func (s *routeScan) jsTarget(site *wire.Site, i int) string {
	if site.Receiver.Kind != wire.TypeModule {
		return ""
	}
	// A named import is "module#name", a default import "module#default",
	// a namespace import or a require the module alone.
	name := site.Receiver.Name
	if m, export, ok := strings.Cut(name, "#"); ok {
		name = m
		if export != "default" {
			name += "." + export
		}
	}
	switch site.Shape {
	case wire.ShapeBare, wire.ShapeIndirect:
		return name
	case wire.ShapeStatic, wire.ShapeMember:
		return name + "." + s.callee(i)
	}
	return ""
}

// expressMount records `parent.use([path,] ..., router)`: each argument that
// is an object this file builds is mounted at the path, the root when the
// call names none.
func (s *routeScan) expressMount(site *wire.Site) {
	parent := s.objOf(site.Receiver)
	s.attachArgs(site, func(o *routeObj, at int) (routeAttach, bool) {
		if at == 0 {
			return routeAttach{parent: parent, known: true}, true
		}
		args := s.siteArgs(site)
		if len(args) == 0 {
			return routeAttach{parent: parent}, true
		}
		v, ok := stringLiteral(s.b.lang.Name, args[0].text)
		return routeAttach{parent: parent, prefix: v, known: ok}, true
	})
}

// expressRoute reads one call as an Express route: `x.get(path, ...,
// handler)` and the other verbs, `x.use(path, ..., handler)`, and
// `x.route(path).get(..., handler)` chains. The handler is the last
// argument; the ones before it are middleware. A receiver this file does not
// build is read only with a literal route, so a same-named method of an
// unrelated object is not read as a route.
func (s *routeScan) expressRoute(r *wire.Ref, site *wire.Site, i int) {
	if !r.Qualified || !(slices.Contains(expressVerbs, r.Name) || r.Name == "use") {
		return
	}
	args := positional(s.siteArgs(site))
	if len(args) == 0 {
		return
	}
	var methods []string
	if r.Name != "all" && r.Name != "use" {
		methods = []string{strings.ToUpper(r.Name)}
	}
	if chain := s.routeChain(s.resultSite(site.Receiver), len(s.link.Sites)); chain >= 0 && r.Name != "use" {
		chained := &s.link.Sites[chain]
		cargs := positional(s.siteArgs(chained))
		if len(cargs) == 0 {
			return
		}
		ref := s.siteRef(chain)
		obj := s.objOf(chained.Receiver)
		n := len(s.out)
		for _, route := range s.expressPaths(cargs[0]) {
			if obj == nil && !route.literal {
				continue
			}
			s.emit(fwExpress, methods, route, obj, refSpan(r), s.handlerAt(args[len(args)-1]), refQualifier(ref))
		}
		if len(s.out) > n {
			s.expressMiddleware(site, args, 0)
		}
		return
	}
	obj := s.objOf(site.Receiver)
	if r.Name == "use" {
		// Every function `use` passes is middleware (Express reads a
		// first argument that is no function as the path), on an object
		// this file builds or at a literal path.
		if _, path := stringLiteral(s.b.lang.Name, args[0].text); obj != nil || path {
			s.expressMiddleware(site, args, 0)
		}
	}
	if len(args) < 2 {
		return
	}
	last := args[len(args)-1]
	if r.Name == "use" {
		// `use` mounts a router or an application when it passes one, and
		// otherwise registers middleware at a path: an endpoint only when the
		// path is a literal and the handler is not an imported module.
		if s.objOf(argType(site, len(args)-1)) != nil || argType(site, len(args)-1).Kind == wire.TypeModule {
			return
		}
		if _, imported := s.imports[last.text]; imported {
			return
		}
	}
	n := len(s.out)
	for _, route := range s.expressPaths(args[0]) {
		if !route.literal && (obj == nil || r.Name == "use") {
			continue
		}
		s.emit(fwExpress, methods, route, obj, refSpan(r), s.handlerAt(last), r.Qualifier)
	}
	if r.Name != "use" && len(s.out) > n {
		// The functions before the handler are the route's middleware.
		s.expressMiddleware(site, args, 1)
	}
}

// expressPaths are the routes a path argument names: one per element of an
// array literal (Express takes "an array of combinations" of strings,
// patterns and regular expressions, each matched on its own), else the
// argument itself.
func (s *routeScan) expressPaths(a routeArg) []routeText {
	if !strings.HasPrefix(a.text, "[") {
		return []routeText{s.routeOf(a)}
	}
	es := elements(a.text)
	out := make([]routeText, 0, len(es))
	for _, e := range es {
		v, ok := stringLiteral(s.b.lang.Name, e)
		out = append(out, routeText{written: e, value: v, literal: ok})
	}
	return out
}

// routeChain is the `route(path)` site a chain of Express verbs hangs from,
// `x.route(path)` in `x.route(path).get(h).post(h)`, or -1. budget bounds
// the walk by the file's sites.
func (s *routeScan) routeChain(i, budget int) int {
	for ; i >= 0 && budget > 0; budget-- {
		r := s.siteRef(i)
		if r == nil || !r.Qualified {
			return -1
		}
		switch {
		case r.Name == "route":
			return i
		case slices.Contains(expressVerbs, r.Name):
			i = s.resultSite(s.link.Sites[i].Receiver)
		default:
			return -1
		}
	}
	return -1
}

// refQualifier is a call reference's receiver as written, empty for none.
func refQualifier(r *wire.Ref) string {
	if r == nil {
		return ""
	}
	return r.Qualifier
}

// expressMiddleware records as middleware each function a call passes from
// argument position from on: a function literal, a name or a wrapper call
// handlerAt reads, an array literal's elements each (Express flattens
// arrays of handlers); a router, an application or an imported module is
// mounted, not run as middleware.
func (s *routeScan) expressMiddleware(site *wire.Site, args []routeArg, from int) {
	for k := from; k < len(args); k++ {
		if s.objOf(argType(site, k)) != nil || argType(site, k).Kind == wire.TypeModule {
			continue
		}
		for _, a := range argItems(args[k]) {
			s.b.middleware = append(s.b.middleware, s.handlerAt(a)...)
		}
	}
}

// argItems are the elements of an array literal argument, each with its
// span, or the argument alone when it is not one.
func argItems(a routeArg) []routeArg {
	text := a.text
	if len(text) < 2 || text[0] != '[' || text[len(text)-1] != ']' || uint32(len(text)) != a.at.End-a.at.Start {
		return []routeArg{a}
	}
	var out []routeArg
	off := 1
	for _, e := range splitTop(text[1 : len(text)-1]) {
		lead := len(e) - len(strings.TrimLeft(e, " \t\n\r"))
		if v := strings.TrimSpace(e); v != "" {
			start := a.at.Start + uint32(off+lead)
			out = append(out, routeArg{at: wire.Span{Start: start, End: start + uint32(len(v))}, text: v})
		}
		off += len(e) + 1
	}
	return out
}
