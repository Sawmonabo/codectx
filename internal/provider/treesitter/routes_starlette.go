package treesitter

import (
	"strings"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/wire"
)

// routeList is a route list one object holds: the bytes of the list literal
// passed as its `routes=` (or of the one module-level binding that holds
// it), whose Route, WebSocketRoute and Mount entries are mounted on obj.
type routeList struct {
	at  wire.Span
	obj *routeObj
}

// starletteObjects builds the file's Starlette objects and records the
// route list each holds: `Starlette(debug, routes, ...)` is an application
// root; `Router(routes, ...)` a router and `Mount(path, app, routes, ...)`
// one mounted at its path, each resolved through the list or the object it
// is an entry of; a FastAPI application takes `routes=` by keyword too.
func (s *routeScan) starletteObjects() {
	s.calls(func(_ *wire.Ref, site *wire.Site, i int) {
		var o *routeObj
		pos := -1
		switch s.pyTarget(site, i) {
		case "starlette.applications.Starlette":
			o, pos = &routeObj{fw: fwStarlette, root: true, ownKnown: true}, 1
		case "starlette.routing.Router":
			o, pos = &routeObj{fw: fwStarlette, ownKnown: true}, 0
		case "starlette.routing.Mount":
			o, pos = &routeObj{fw: fwStarlette, ownKnown: true}, 2
		default:
			if o = s.objs[i]; o == nil || o.fw != fwFastAPI || !o.root {
				return
			}
		}
		s.objs[i] = o
		if at, ok := s.routeListAt(site, pos); ok {
			s.lists = append(s.lists, routeList{at: at, obj: o})
		}
	})
}

// routeListAt is the bytes of the route list a construction passes at
// position pos or as `routes=`: a list or tuple literal written in place,
// or a name the module binds exactly once, whose binding holds the list.
// Any other expression names no list this file can read.
func (s *routeScan) routeListAt(site *wire.Site, pos int) (wire.Span, bool) {
	args := s.siteArgs(site)
	if args == nil {
		return wire.Span{}, false
	}
	a, ok := argument(args, pos, "routes")
	switch {
	case !ok:
		return wire.Span{}, false
	case strings.HasPrefix(a.text, "[") || strings.HasPrefix(a.text, "("):
		return a.at, true
	case !pyName(a.text):
		return wire.Span{}, false
	}
	found := -1
	for i := range s.b.decls {
		d := &s.b.decls[i]
		if d.Parent < 0 && d.kind == model.NodeVariable && d.Name == a.text {
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

// listOwner is the object whose route list holds the entry at: the
// innermost list around it. known is false when no list holds it; owner is
// nil with known true when two objects hold that one list (a binding passed
// to both), which mounts the entry at several places.
func (s *routeScan) listOwner(at wire.Span) (owner *routeObj, known bool) {
	var best wire.Span
	for _, l := range s.lists {
		if l.at.Start > at.Start || at.End > l.at.End {
			continue
		}
		switch {
		case !known || l.at.End-l.at.Start < best.End-best.Start:
			owner, known, best = l.obj, true, l.at
		case l.at == best && l.obj != owner:
			owner = nil
		}
	}
	return owner, known
}

// starletteRoutes reads the file's Starlette route entries:
// `Route(path, endpoint, methods=...)`, `WebSocketRoute(path, endpoint)`
// and `Mount(path, app=..., routes=...)`. Each is mounted on the object
// whose route list holds it. A Route that passes no methods takes GET when
// its endpoint is a function and every method when it is a class (an ASGI
// application), so an endpoint this file does not declare as a function
// names no method. A Mount attaches the object it passes as `app` under its
// own path. Every Mount is attached before any route is read, since a list
// bound to a name is written before the Mount that holds it.
func (s *routeScan) starletteRoutes() {
	for _, mounts := range []bool{true, false} {
		s.starletteEntries(mounts)
	}
}

// starletteEntries reads the file's Mount entries, or its Route and
// WebSocketRoute entries.
func (s *routeScan) starletteEntries(mounts bool) {
	s.calls(func(r *wire.Ref, site *wire.Site, i int) {
		target := s.pyTarget(site, i)
		switch target {
		case "starlette.routing.Route", "starlette.routing.WebSocketRoute":
			if mounts {
				return
			}
		case "starlette.routing.Mount":
			if !mounts {
				return
			}
		default:
			return
		}
		args := s.siteArgs(site)
		if args == nil {
			return
		}
		path, ok := argument(args, 0, "path")
		if !ok {
			return
		}
		rt := s.routeOf(path)
		owner, known := s.listOwner(refSpan(r))
		if target == "starlette.routing.Mount" {
			s.starletteMount(site, args, s.objs[i], rt, owner, known)
			return
		}
		endpoint, ok := argument(args, 1, "endpoint")
		if !ok {
			return
		}
		handlers := s.handlerAt(endpoint)
		methods := []string{methodWebSocket}
		if target == "starlette.routing.Route" {
			methods = nil
			if m, given := argument(args, -1, "methods"); given {
				methods, _ = s.literalMethods(m.text)
			} else if len(handlers) == 1 && s.functionHandler(handlers[0]) {
				methods = []string{"GET"}
			}
		}
		if owner == nil && !rt.literal {
			return
		}
		s.emit(fwStarlette, methods, rt, owner, refSpan(r), handlers, "")
	})
}

// starletteMount attaches a Mount to the list that holds it, at its path,
// and the object it passes as `app` (position 1) under it.
func (s *routeScan) starletteMount(site *wire.Site, args []routeArg, o *routeObj, rt routeText, owner *routeObj, known bool) {
	if o == nil {
		return
	}
	if known {
		o.atts = append(o.atts, routeAttach{parent: owner, prefix: rt.value, known: owner != nil && rt.literal})
	}
	n := 0
	for k, a := range args {
		if a.kw == "*" {
			break
		}
		if a.kw == "app" || a.kw == "" && n == 1 {
			if c := s.objOf(argType(site, k)); c != nil && c != o {
				c.atts = append(c.atts, routeAttach{parent: o, known: true})
			}
		}
		if a.kw == "" {
			n++
		}
	}
}

// functionHandler reports a handler that is a function this file declares
// (a declaration of kind function or method, or a function literal).
func (s *routeScan) functionHandler(h handler) bool {
	switch {
	case h.fn != nil:
		return true
	case h.decl >= 0 && h.decl < len(s.b.decls):
		k := s.b.decls[h.decl].kind
		return k == model.NodeFunction || k == model.NodeMethod
	case h.ref != nil && !h.ref.Qualified:
		ds := s.b.byName[h.ref.Name]
		if len(ds) != 1 {
			return false
		}
		k := s.b.decls[ds[0]].kind
		return k == model.NodeFunction
	}
	return false
}
