package treesitter

import (
	"slices"
	"strings"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/wire"
)

// goRouteForm is one Go registration method: where its method, route and
// handler arguments are. method is the argument naming the method, -1 when
// the verb names it (verb) or none does; handler -1 is the last argument.
type goRouteForm struct {
	verb    string
	method  int
	route   int
	handler int
	// pattern reports a route whose literal may lead with its method,
	// Go 1.22's `"GET /x"`.
	pattern bool
}

// goForms are each Go framework's registration methods by name.
var goForms = map[string]map[string]goRouteForm{
	fwNetHTTP: {
		"Handle":     {method: -1, handler: 1, pattern: true},
		"HandleFunc": {method: -1, handler: 1, pattern: true},
	},
	fwGorilla: {
		"Handle":     {method: -1, handler: 1},
		"HandleFunc": {method: -1, handler: 1},
	},
	fwChi: goVerbForms([]string{"Get", "Post", "Put", "Delete", "Patch", "Head", "Options", "Connect", "Trace"}, 1, map[string]goRouteForm{
		"Handle":     {method: -1, handler: 1, pattern: true},
		"HandleFunc": {method: -1, handler: 1, pattern: true},
		"Method":     {method: 0, route: 1, handler: 2},
		"MethodFunc": {method: 0, route: 1, handler: 2},
	}),
	fwGin: goVerbForms([]string{"GET", "POST", "PUT", "DELETE", "PATCH", "HEAD", "OPTIONS"}, -1, map[string]goRouteForm{
		"Any":    {method: -1, handler: -1},
		"Handle": {method: 0, route: 1, handler: -1},
	}),
	fwEcho: goVerbForms([]string{"GET", "POST", "PUT", "DELETE", "PATCH", "HEAD", "OPTIONS", "CONNECT", "TRACE"}, 1, map[string]goRouteForm{
		"Any": {method: -1, handler: 1},
		"Add": {method: 0, route: 1, handler: 2},
	}),
}

// goVerbForms adds to forms one form per verb, each naming its method, with
// its handler at position handler.
func goVerbForms(verbs []string, handler int, forms map[string]goRouteForm) map[string]goRouteForm {
	for _, v := range verbs {
		forms[v] = goRouteForm{verb: strings.ToUpper(v), method: -1, handler: handler}
	}
	return forms
}

// goRouterTypes are the router types of each framework, as a receiver's
// declared type names them: evidence of the framework on a receiver this
// file does not build (a parameter, a field).
var goRouterTypes = map[string][]string{
	fwNetHTTP: {"ServeMux"},
	fwGorilla: {"Router"},
	fwChi:     {"Router", "Mux"},
	fwGin:     {"Engine", "RouterGroup", "IRouter", "IRoutes"},
	fwEcho:    {"Echo", "Group"},
}

// goRoutes reads the file's Go routes: `http.Handle` and `http.HandleFunc`
// on the default mux, an application root; ServeMux, gorilla/mux and chi
// routers, which resolve when this file serves or mounts them once; gin and
// echo engines, roots, and their groups.
func (s *routeScan) goRoutes() {
	defaultMux := &routeObj{fw: fwNetHTTP, root: true, ownKnown: true}
	s.goObjects()
	s.methodsOn = map[int]int{}
	s.calls(func(r *wire.Ref, site *wire.Site, i int) {
		if r.Qualified && r.Name == "Methods" {
			if on := s.resultSite(site.Receiver); on >= 0 {
				if _, dup := s.methodsOn[on]; !dup {
					s.methodsOn[on] = i
				}
			}
		}
	})
	s.calls(func(r *wire.Ref, site *wire.Site, _ int) { s.goMount(r, site, defaultMux) })
	s.calls(func(r *wire.Ref, site *wire.Site, i int) { s.goRoute(r, site, i, defaultMux) })
}

// goImported is the framework the import a qualifier names belongs to.
func (s *routeScan) goImported(qualifier string) string {
	imp, ok := s.imports[qualifier]
	if !ok {
		return ""
	}
	return frameworkOf("go", imp.path)
}

// goObjects builds the file's routers and engines, and the groups and
// subrouters derived from them, in site order, so a group's parent is
// built before it.
func (s *routeScan) goObjects() {
	s.calls(func(r *wire.Ref, site *wire.Site, i int) {
		if !r.Qualified {
			return
		}
		switch fw := s.goImported(r.Qualifier); {
		case fw == fwNetHTTP && r.Name == "NewServeMux",
			fw == fwGorilla && r.Name == "NewRouter",
			fw == fwChi && (r.Name == "NewRouter" || r.Name == "NewMux"):
			s.objs[i] = &routeObj{fw: fw, ownKnown: true}
		case fw == fwGin && (r.Name == "New" || r.Name == "Default"),
			fw == fwEcho && r.Name == "New":
			s.objs[i] = &routeObj{fw: fw, root: true, ownKnown: true}
		case fw != "":
		case r.Name == "Group":
			// gin `r.Group("/v1", ...)`, echo `e.Group("/v1", ...)`.
			if parent := s.objOf(site.Receiver); parent != nil && (parent.fw == fwGin || parent.fw == fwEcho) {
				s.objs[i] = s.child(parent, site)
			}
		case r.Name == "Subrouter":
			// gorilla `r.PathPrefix("/api").Subrouter()`.
			p := s.resultSite(site.Receiver)
			if pr := s.siteRef(p); pr != nil && pr.Qualified && pr.Name == "PathPrefix" {
				if parent := s.objOf(s.link.Sites[p].Receiver); parent != nil && parent.fw == fwGorilla {
					s.objs[i] = s.child(parent, &s.link.Sites[p])
				}
			}
		}
	})
}

// child is an object built under parent, mounted at the literal
// prefix the call at site prefixSite passes first.
func (s *routeScan) child(parent *routeObj, prefixSite *wire.Site) *routeObj {
	a := routeAttach{parent: parent}
	if args := positional(s.siteArgs(prefixSite)); len(args) > 0 {
		a.prefix, a.known = stringLiteral(s.b.lang.Name, args[0].text)
	}
	return &routeObj{fw: parent.fw, ownKnown: true, atts: []routeAttach{a}}
}

// goMount records where the file attaches each router: served as the
// program's handler by net/http (`ListenAndServe(addr, r)`,
// `ListenAndServeTLS(addr, cert, key, r)`, `Serve(l, r)`, `ServeTLS(l, r,
// cert, key)`), mounted at a prefix by chi (`Mount(prefix, r)`), or
// registered as another router's handler (`Handle(pattern, r)`), which
// passes it the full path, so its prefix is the parent's.
func (s *routeScan) goMount(r *wire.Ref, site *wire.Site, defaultMux *routeObj) {
	if !r.Qualified {
		return
	}
	fw := s.goImported(r.Qualifier)
	served := map[string]int{"ListenAndServe": 1, "ListenAndServeTLS": 3, "Serve": 1, "ServeTLS": 1}
	if at, ok := served[r.Name]; ok && fw == fwNetHTTP {
		s.attachArgs(site, func(_ *routeObj, i int) (routeAttach, bool) {
			return routeAttach{served: true, known: true}, i == at
		})
		return
	}
	parent := s.objOf(site.Receiver)
	if fw == fwNetHTTP {
		parent = defaultMux
	} else if fw != "" {
		return
	}
	switch r.Name {
	case "Mount":
		s.attachArgs(site, func(_ *routeObj, i int) (routeAttach, bool) {
			a := routeAttach{parent: parent}
			if args := positional(s.siteArgs(site)); len(args) > 1 {
				a.prefix, a.known = stringLiteral(s.b.lang.Name, args[0].text)
			}
			return a, i == 1
		})
	case "Handle":
		s.attachArgs(site, func(_ *routeObj, i int) (routeAttach, bool) {
			return routeAttach{parent: parent, known: true}, i == 1
		})
	}
}

// goRoute reads one call as a Go route registration.
func (s *routeScan) goRoute(r *wire.Ref, site *wire.Site, i int, defaultMux *routeObj) {
	if !r.Qualified {
		return
	}
	var obj *routeObj
	fw := s.goImported(r.Qualifier)
	switch {
	case fw == fwNetHTTP:
		obj = defaultMux
	case fw != "":
		return
	default:
		if obj = s.objOf(site.Receiver); obj != nil {
			fw = obj.fw
		} else if fw = s.goTypeFramework(site.Receiver); fw == "" {
			candidates := map[string][]string{}
			for name, forms := range goForms {
				if name != fwNetHTTP {
					for verb := range forms {
						candidates[name] = append(candidates[name], verb)
					}
				}
			}
			fw = s.soleFramework(r.Name, candidates)
		}
	}
	form, ok := goForms[fw][r.Name]
	if !ok {
		return
	}
	args := positional(s.siteArgs(site))
	if len(args) <= max(form.route, form.handler, form.method, 1) {
		return
	}
	route := s.routeOf(args[form.route])
	if obj == nil && !route.literal {
		return
	}
	var methods []string
	switch {
	case form.verb != "":
		methods = []string{form.verb}
	case form.method >= 0:
		if m, ok := s.goMethod(args[form.method].text); ok {
			methods = []string{m}
		}
	case form.pattern && route.literal:
		if m, rest, ok := goPattern(route.value); ok {
			methods = []string{m}
			route = routeText{written: rest, value: rest, literal: true}
		}
	}
	if fw == fwGorilla {
		methods = s.gorillaMethods(i)
	}
	if route.literal && !strings.HasPrefix(route.value, "/") {
		// A Go pattern may lead with a host, `example.com/x`: the route is
		// the pattern, the path the part from its first slash.
		if j := strings.IndexByte(route.value, '/'); j > 0 {
			route.path = route.value[j:]
		}
	}
	h := args[len(args)-1]
	if form.handler >= 0 {
		h = args[form.handler]
	}
	s.emit(fw, methods, route, obj, refSpan(r), s.handlerAt(h), r.Qualifier)
}

// goTypeFramework is the framework whose router type a receiver's declared
// type names, `*mux.Router` or `chi.Router`, or empty.
func (s *routeScan) goTypeFramework(t wire.Type) string {
	if t.Kind != wire.TypeNamed {
		return ""
	}
	pkg, name, ok := strings.Cut(t.Name, ".")
	if !ok {
		return ""
	}
	fw := s.goImported(pkg)
	if slices.Contains(goRouterTypes[fw], name) {
		return fw
	}
	return ""
}

// goPattern splits a Go 1.22 pattern that leads with its method,
// `"GET /x"`, into the method and the rest.
func goPattern(p string) (string, string, bool) {
	m, rest, ok := strings.Cut(p, " ")
	if !ok || m == "" {
		return "", "", false
	}
	for i := 0; i < len(m); i++ {
		if m[i] < 'A' || m[i] > 'Z' {
			return "", "", false
		}
	}
	return m, strings.TrimLeft(rest, " \t"), true
}

// goMethod is the method an argument names: a string literal, or net/http's
// `http.MethodGet` and the other method constants.
func (s *routeScan) goMethod(text string) (string, bool) {
	if v, ok := stringLiteral("go", text); ok && v != "" && len(v) <= model.MaxIdentifierBytes {
		return strings.ToUpper(v), true
	}
	q, name, ok := strings.Cut(text, ".")
	if !ok || s.goImported(q) != fwNetHTTP || !strings.HasPrefix(name, "Method") || len(name) == len("Method") {
		return "", false
	}
	if len(name) > model.MaxIdentifierBytes {
		return "", false
	}
	return strings.ToUpper(name[len("Method"):]), true
}

// gorillaMethods are the methods a gorilla/mux route's `.Methods(...)` names
// on the registration's result (site i), nil when it names none or any of
// them is not a literal or a net/http method constant.
func (s *routeScan) gorillaMethods(i int) []string {
	m, ok := s.methodsOn[i]
	if !ok {
		return nil
	}
	args := positional(s.siteArgs(&s.link.Sites[m]))
	out := make([]string, 0, len(args))
	for _, a := range args {
		method, ok := s.goMethod(a.text)
		if !ok {
			return nil
		}
		out = append(out, method)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
