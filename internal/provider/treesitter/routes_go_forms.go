package treesitter

import (
	"slices"
	"strings"

	"github.com/Sawmonabo/codectx/internal/provider/treesitter/wire"
)

// goRecvObj is the router object a Go call's receiver is: one this file
// builds, or the router a chi `Route` or `Group` closure receives when the
// call is written inside that closure on its parameter (closureParam). The
// innermost such closure whose parameter the receiver names decides.
func (s *routeScan) goRecvObj(r *wire.Ref, site *wire.Site) *routeObj {
	if o := s.objOf(site.Receiver); o != nil {
		return o
	}
	if !r.Qualified || r.Qualifier == "" || len(s.closures) == 0 {
		return nil
	}
	var best wire.Span
	var found *routeObj
	for at, o := range s.closures {
		if at.Start > r.Start || r.End > at.End || s.closureParam(at) != r.Qualifier {
			continue
		}
		if found == nil || at.End-at.Start < best.End-best.Start {
			best, found = at, o
		}
	}
	return found
}

// closureParam is the name of a Go function literal's first parameter, as
// written: `r` in `func(r chi.Router) {...}`, or empty.
func (s *routeScan) closureParam(at wire.Span) string {
	t := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s.text(at)), "func"))
	t, ok := strings.CutPrefix(t, "(")
	if !ok {
		return ""
	}
	t = strings.TrimLeft(t, " \t\r\n")
	n := 0
	for n < len(t) && isIdent(t[n], n == 0) && t[n] != '$' {
		n++
	}
	if n == 0 || t[:n] == "_" {
		return ""
	}
	return t[:n]
}

// chiClosure records the router a chi closure receives. `Route(pattern, fn)`
// "mounts a sub-Router along a pattern string": fn's router is a child of
// the parent at the pattern, resolved when the pattern is a literal.
// `Group(fn)` "adds a new inline-Router along the current routing path":
// fn's router has exactly the parent's prefix. Either returns that router,
// which the site's result is.
func (s *routeScan) chiClosure(r *wire.Ref, site *wire.Site, i int, parent *routeObj) {
	args := positional(s.siteArgs(site))
	fn, prefixed := 0, r.Name == "Route"
	if prefixed {
		fn = 1
	}
	if len(args) <= fn {
		return
	}
	a := routeAttach{parent: parent, known: !prefixed}
	if prefixed {
		a.prefix, a.known = stringLiteral("go", args[0].text)
	}
	o := &routeObj{fw: fwChi, ownKnown: true, atts: []routeAttach{a}}
	s.objs[i] = o
	for _, f := range s.b.ex.functions {
		if f.Span == args[fn].at {
			s.closures[f.Span] = o
			return
		}
	}
}

// gorillaMatchers are the gorilla/mux route matchers a registration chain
// may pass through before its handler: each returns the route it is called
// on.
var gorillaMatchers = []string{"Methods", "Path", "PathPrefix", "Host", "Schemes", "Headers", "HeadersRegexp",
	"Queries", "MatcherFunc", "Name", "BuildVarsFunc"}

// gorillaChain reads a gorilla/mux route built by a chain of matchers and
// closed by its handler: `r.PathPrefix("/static/").Handler(h)`,
// `r.Methods("GET").Path("/x").HandlerFunc(h)`, from a router or from
// `r.NewRoute()`. The route is the one `Path` or `PathPrefix` the chain
// names (a chain with none or with several is not read), its methods those
// `Methods` names, and the router the receiver of the chain's first call.
func (s *routeScan) gorillaChain(r *wire.Ref, site *wire.Site, _ int) {
	if !r.Qualified || r.Name != "Handler" && r.Name != "HandlerFunc" {
		return
	}
	hargs := positional(s.siteArgs(site))
	if len(hargs) != 1 {
		return
	}
	var route *routeArg
	var methods []string
	methodsKnown := true
	first := -1
	i := s.resultSite(site.Receiver)
	for budget := len(s.link.Sites); budget > 0; budget-- {
		ref := s.siteRef(i)
		if ref == nil || !ref.Qualified {
			return
		}
		if ref.Name == "NewRoute" {
			first = i
			break
		}
		if !slices.Contains(gorillaMatchers, ref.Name) {
			return
		}
		args := positional(s.siteArgs(&s.link.Sites[i]))
		switch ref.Name {
		case "Path", "PathPrefix":
			if route != nil || len(args) != 1 {
				return
			}
			route = &args[0]
		case "Methods":
			for _, a := range args {
				m, ok := s.goMethod(a.text)
				if !ok {
					methodsKnown = false
					break
				}
				methods = append(methods, m)
			}
		}
		first = i
		// The chain goes on while the receiver is another matcher's (or
		// NewRoute's) result; otherwise the receiver is the router.
		next := s.resultSite(s.link.Sites[i].Receiver)
		nr := s.siteRef(next)
		if nr == nil || !nr.Qualified || nr.Name != "NewRoute" && !slices.Contains(gorillaMatchers, nr.Name) {
			break
		}
		i = next
	}
	if route == nil || first < 0 {
		return
	}
	if !methodsKnown {
		methods = nil
	}
	fref := s.siteRef(first)
	fsite := &s.link.Sites[first]
	obj := s.goRecvObj(fref, fsite)
	switch {
	case obj != nil && obj.fw != fwGorilla:
		return
	case obj == nil && s.goTypeFramework(fsite.Receiver) != fwGorilla && !s.frameworks[fwGorilla]:
		return
	}
	rt := s.routeOf(*route)
	if obj == nil && !rt.literal {
		return
	}
	s.emit(fwGorilla, methods, rt, obj, refSpan(r), s.handlerAt(hargs[0]), fref.Qualifier)
}
