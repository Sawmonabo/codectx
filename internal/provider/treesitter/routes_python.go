package treesitter

import (
	"cmp"
	"slices"
	"strings"

	"github.com/Sawmonabo/codectx/internal/provider/treesitter/wire"
)

// The Python route forms: Flask and FastAPI decorators on the handler, and
// Django urlconf entries.
var (
	// flaskVerbs are the Flask route decorators: `route` takes `methods=`
	// and defaults to GET, the method shortcuts name their own.
	flaskVerbs = []string{"route", "get", "post", "put", "delete", "patch"}
	// fastAPIVerbs are the FastAPI path operation decorators: `api_route`
	// takes `methods=` and defaults to GET, the others name their own.
	fastAPIVerbs = []string{"api_route", "get", "post", "put", "delete", "patch", "options", "head", "trace"}
	// djangoRoutes are the Django urlconf functions that declare one route.
	djangoRoutes = []string{"path", "re_path", "url"}
)

// python reads the file's Flask, FastAPI and Django routes.
func (s *routeScan) python() {
	if s.frameworks[fwFlask] || s.frameworks[fwFastAPI] {
		s.pyObjects()
		s.pyMounts()
		for i := range s.b.decls {
			for _, d := range s.b.decls[i].Decorators {
				s.pyDecorator(i, d)
			}
		}
	}
	if s.frameworks[fwDjango] {
		s.django()
	}
}

// pyTarget is what a Python site calls, through the module it was imported
// from: "flask.Flask" for `Flask(...)` after `from flask import Flask` and
// for `flask.Flask(...)` after `import flask`; empty when the callee is not
// an imported name.
func (s *routeScan) pyTarget(site *wire.Site, i int) string {
	if site.Receiver.Kind != wire.TypeModule {
		return ""
	}
	switch site.Shape {
	case wire.ShapeBare, wire.ShapeIndirect:
		return site.Receiver.Name
	case wire.ShapeStatic, wire.ShapeMember:
		return site.Receiver.Name + "." + s.callee(i)
	}
	return ""
}

// pyObjects builds the file's Flask and FastAPI applications and routers:
// `Flask(...)` and `FastAPI(...)` are roots; a `Blueprint` carries its own
// `url_prefix=`, an `APIRouter` its own `prefix=`.
func (s *routeScan) pyObjects() {
	s.calls(func(r *wire.Ref, site *wire.Site, i int) {
		switch s.pyTarget(site, i) {
		case "flask.Flask", "flask.app.Flask":
			s.objs[i] = &routeObj{fw: fwFlask, root: true, ownKnown: true}
		case "fastapi.FastAPI", "fastapi.applications.FastAPI":
			s.objs[i] = &routeObj{fw: fwFastAPI, root: true, ownKnown: true}
		case "flask.Blueprint", "flask.blueprints.Blueprint":
			o := &routeObj{fw: fwFlask}
			// Blueprint(name, import_name, static_folder, static_url_path,
			// template_folder, url_prefix, ...).
			o.own, o.ownKnown = s.ownPrefix(site, 5, "url_prefix")
			s.objs[i] = o
		case "fastapi.APIRouter", "fastapi.routing.APIRouter":
			o := &routeObj{fw: fwFastAPI}
			o.own, o.ownKnown = s.ownPrefix(site, -1, "prefix")
			s.objs[i] = o
		}
	})
}

// ownPrefix is the prefix a construction declares at position pos or
// keyword kw: none is the empty prefix; one that is not a literal, or
// arguments the worker did not send, are not known.
func (s *routeScan) ownPrefix(site *wire.Site, pos int, kw string) (string, bool) {
	args := s.siteArgs(site)
	if args == nil {
		return "", len(site.Args) == 0
	}
	a, ok := argument(args, pos, kw)
	if !ok {
		return "", true
	}
	return stringLiteral(s.b.lang.Name, a.text)
}

// mountPrefix is the prefix a mount call passes at position pos or keyword
// kw: whether it passes one, the literal, and whether it is known.
func (s *routeScan) mountPrefix(site *wire.Site, pos int, kw string) (given bool, prefix string, known bool) {
	args := s.siteArgs(site)
	if args == nil {
		return false, "", false
	}
	a, ok := argument(args, pos, kw)
	if !ok {
		return false, "", true
	}
	v, lit := stringLiteral(s.b.lang.Name, a.text)
	return true, v, lit
}

// pyMounts records where the file mounts each object: a Flask
// `register_blueprint(bp, url_prefix=...)`, whose prefix replaces the
// blueprint's own; a FastAPI `include_router(r, prefix=...)`, whose prefix
// is added before the router's own; a FastAPI `mount(path, app)`.
func (s *routeScan) pyMounts() {
	s.calls(func(r *wire.Ref, site *wire.Site, i int) {
		if !r.Qualified {
			return
		}
		parent := s.objOf(site.Receiver)
		switch r.Name {
		case "register_blueprint":
			s.attachArgs(site, func(o *routeObj, at int) (routeAttach, bool) {
				if at != 0 || o.fw != fwFlask || o.root {
					return routeAttach{}, false
				}
				given, prefix, known := s.mountPrefix(site, -1, "url_prefix")
				return routeAttach{parent: parent, prefix: prefix, known: known, override: given}, true
			})
		case "include_router":
			s.attachArgs(site, func(o *routeObj, at int) (routeAttach, bool) {
				if at != 0 || o.fw != fwFastAPI || o.root {
					return routeAttach{}, false
				}
				_, prefix, known := s.mountPrefix(site, -1, "prefix")
				return routeAttach{parent: parent, prefix: prefix, known: known}, true
			})
		case "mount":
			s.attachArgs(site, func(o *routeObj, at int) (routeAttach, bool) {
				if at != 1 || o.fw != fwFastAPI {
					return routeAttach{}, false
				}
				given, prefix, known := s.mountPrefix(site, 0, "path")
				return routeAttach{parent: parent, prefix: prefix, known: known && given}, true
			})
		}
	})
}

// pyDecorator reads one decorator of declaration decl as a Flask or FastAPI
// route: a call of a route verb on a receiver, `@app.route("/x")`. The
// receiver is the object the worker traced it to when this file builds it;
// otherwise the framework is the one imported framework with that verb, and
// the route must be a literal, so a same-named method of an unrelated
// object is not read as a route.
func (s *routeScan) pyDecorator(decl int, d wire.Decorator) {
	dot := strings.LastIndexByte(d.Name, '.')
	if dot <= 0 {
		return
	}
	verb, receiver := d.Name[dot+1:], d.Name[:dot]
	call := s.decoratorCall(d, verb, receiver)
	if call == nil {
		return
	}
	var obj *routeObj
	if site, _ := s.site(call); site != nil {
		obj = s.objOf(site.Receiver)
	}
	fw := ""
	switch {
	case obj != nil:
		fw = obj.fw
	default:
		fw = s.soleFramework(verb, map[string][]string{fwFlask: flaskVerbs, fwFastAPI: fastAPIVerbs})
	}
	args := s.decoratorArgs(d)
	var route routeArg
	var ok bool
	var methods []string
	switch fw {
	case fwFlask:
		if !slices.Contains(flaskVerbs, verb) {
			return
		}
		route, ok = argument(args, 0, "rule")
		methods = s.pyMethods(verb, args, "route")
	case fwFastAPI:
		if !slices.Contains(fastAPIVerbs, verb) {
			return
		}
		route, ok = argument(args, 0, "path")
		methods = s.pyMethods(verb, args, "api_route")
	default:
		return
	}
	if !ok {
		return
	}
	r := s.routeOf(route)
	if obj == nil && !r.literal {
		return
	}
	s.emit(fw, methods, r, obj, wire.Span{Start: d.Start, End: d.End}, []handler{declHandler(decl)}, receiver)
}

// pyMethods are the methods a Python route decorator names: the verb's own,
// or for the generic form (general) its `methods=` literal, GET when it
// passes none; nil, one endpoint with no method, when `methods=` is not a
// literal.
func (s *routeScan) pyMethods(verb string, args []routeArg, general string) []string {
	if verb != general {
		return []string{strings.ToUpper(verb)}
	}
	a, ok := argument(args, -1, "methods")
	if !ok {
		return []string{"GET"}
	}
	methods, lit := s.literalMethods(a.text)
	if !lit {
		return nil
	}
	return methods
}

// decoratorCall is the call reference a decorator makes, `receiver.verb(...)`
// inside its span, or nil when the decorator is no such call.
func (s *routeScan) decoratorCall(d wire.Decorator, verb, receiver string) *wire.Ref {
	for j := range s.b.ex.refs {
		r := &s.b.ex.refs[j]
		if r.Kind == "call" && r.Qualified && r.Name == verb && r.Qualifier == receiver &&
			d.Start <= r.Start && r.End <= d.End {
			return r
		}
	}
	return nil
}

// soleFramework is the one imported framework whose verbs include verb, or
// empty when none or several do.
func (s *routeScan) soleFramework(verb string, verbs map[string][]string) string {
	found := ""
	for fw, vs := range verbs {
		if s.frameworks[fw] && slices.Contains(vs, verb) {
			if found != "" {
				return ""
			}
			found = fw
		}
	}
	return found
}

// django reads the file's urlconf entries: `path(route, view)`,
// `re_path(route, view)` and `url(regex, view)` imported from django.urls or
// django.conf.urls. A route's prefix is always unresolved: the urlconf that
// includes this one is chosen by the project's settings and its `include()`
// elsewhere. An entry whose view is an `include(...)` mounts another urlconf
// and is no endpoint; `View.as_view()` routes to the class.
func (s *routeScan) django() {
	s.calls(func(r *wire.Ref, site *wire.Site, _ int) {
		if r.Qualified {
			return
		}
		imp, ok := s.imports[r.Name]
		if !ok || frameworkOf("python", imp.path) != fwDjango || !slices.Contains(djangoRoutes, cmp.Or(imp.original, r.Name)) {
			return
		}
		args := s.siteArgs(site)
		if args == nil {
			return
		}
		route, ok := argument(args, 0, "route")
		if !ok {
			if route, ok = argument(args, 0, "regex"); !ok {
				return
			}
		}
		view, ok := argument(args, 1, "view")
		if !ok {
			return
		}
		if inc, ok := s.imports[calleeName(view.text)]; ok && frameworkOf("python", inc.path) == fwDjango &&
			cmp.Or(inc.original, calleeName(view.text)) == "include" {
			return
		}
		s.emitPrefixed(fwDjango, nil, s.routeOf(route), "", false, refSpan(r), s.handlerAt(asView(view)), "")
	})
}

// calleeName is the name a call expression as written calls, `include` for
// `include("app.urls")`, or empty when text is no call of a name.
func calleeName(text string) string {
	i := strings.IndexByte(text, '(')
	if i <= 0 || !strings.HasSuffix(text, ")") {
		return ""
	}
	return strings.TrimSpace(text[:i])
}

// asView is a Django view argument with its `.as_view(...)` call removed,
// so a class-based view routes to its class.
func asView(a routeArg) routeArg {
	i := strings.Index(a.text, ".as_view(")
	if i <= 0 || !strings.HasSuffix(a.text, ")") {
		return a
	}
	name := strings.TrimRight(a.text[:i], " \t\n")
	return routeArg{kw: a.kw, at: wire.Span{Start: a.at.Start, End: a.at.Start + uint32(len(name))}, text: name}
}
