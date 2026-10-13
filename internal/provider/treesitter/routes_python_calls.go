package treesitter

import (
	"strings"

	"github.com/Sawmonabo/codectx/internal/provider/treesitter/wire"
)

// methodWebSocket is the method a WebSocket route carries: an ASGI
// connection of scope type "websocket", routed apart from the HTTP routes of
// the same path, so a WebSocket route and an HTTP GET route on one path are
// two endpoints.
const methodWebSocket = "WEBSOCKET"

// pyCallRoute reads one call as a route registered by a call rather than a
// decorator:
//
//   - Flask `x.add_url_rule(rule, endpoint, view_func, ..., methods=...)`,
//     the only way to register a class-based view. Its methods are the
//     `methods=` literal; when it passes none, Flask takes the view's own
//     `methods` attribute, which a `View.as_view(...)` view carries, so
//     such a route names no method, and any other view is GET;
//   - FastAPI `x.add_api_route(path, endpoint, ..., methods=...)`, GET when
//     it passes no methods, and `x.add_api_websocket_route(path, endpoint)`.
//
// As for decorators, a receiver this file does not build is read only when
// the framework is imported and the route is a literal.
func (s *routeScan) pyCallRoute(r *wire.Ref, site *wire.Site, _ int) {
	if !r.Qualified {
		return
	}
	obj := s.objOf(site.Receiver)
	fw := fwFlask
	if r.Name != "add_url_rule" {
		fw = fwFastAPI
	}
	switch {
	case r.Name != "add_url_rule" && r.Name != "add_api_route" && r.Name != "add_api_websocket_route":
		return
	case obj != nil && obj.fw != fw, obj == nil && !s.frameworks[fw]:
		return
	}
	args := s.siteArgs(site)
	if args == nil {
		return
	}
	var route, view routeArg
	var ok, hasView bool
	var methods []string
	switch r.Name {
	case "add_url_rule":
		route, ok = argument(args, 0, "rule")
		view, hasView = argument(args, 2, "view_func")
		if m, given := argument(args, -1, "methods"); given {
			methods, _ = s.literalMethods(m.text)
		} else if !hasView || asView(view).text == view.text {
			methods = []string{"GET"}
		}
	case "add_api_route":
		route, ok = argument(args, 0, "path")
		view, hasView = argument(args, 1, "endpoint")
		methods = s.pyMethods("api_route", args, "api_route")
	case "add_api_websocket_route":
		route, ok = argument(args, 0, "path")
		view, hasView = argument(args, 1, "endpoint")
		methods = []string{methodWebSocket}
	}
	if !ok {
		return
	}
	rt := s.routeOf(route)
	if obj == nil && !rt.literal {
		return
	}
	var handlers []handler
	if hasView {
		handlers = s.handlerAt(asView(view))
	}
	s.emit(fw, methods, rt, obj, refSpan(r), handlers, r.Qualifier)
}

// djangoApplications records the sites that call Django's application
// factories, `get_wsgi_application()` from django.core.wsgi and
// `get_asgi_application()` from django.core.asgi, imported by name or called
// through their module: the object a WSGI or ASGI server serves.
func (s *routeScan) djangoApplications() {
	s.calls(func(r *wire.Ref, site *wire.Site, i int) {
		switch s.pyTarget(site, i) {
		case "django.core.wsgi.get_wsgi_application", "django.core.asgi.get_asgi_application":
			s.apps = append(s.apps, i)
		}
	})
}

// pyName reports text is one Python identifier.
func pyName(text string) bool {
	if text == "" || strings.ContainsAny(text, ".") {
		return false
	}
	for i := 0; i < len(text); i++ {
		if !isIdent(text[i], i == 0) || text[i] == '$' {
			return false
		}
	}
	return true
}
