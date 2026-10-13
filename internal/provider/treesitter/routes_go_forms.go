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

// goMethodList are the methods a slice literal of methods names
// (`[]string{http.MethodGet, "POST"}`), nil when any is not a literal or a
// net/http method constant: the endpoint then names no method.
func (s *routeScan) goMethodList(text string) []string {
	open := strings.IndexByte(text, '{')
	if open < 0 || !strings.HasSuffix(text, "}") {
		return nil
	}
	var out []string
	for _, e := range elements(text[open:]) {
		m, ok := s.goMethod(e)
		if !ok {
			return nil
		}
		out = append(out, m)
	}
	return out
}

// stripPrefix mounts the handler `http.StripPrefix(prefix, h)` passes, when
// it is a router this file builds, below parent at the stripped prefix: the
// router sees the request's path with the prefix removed, so its routes'
// full paths are the prefix joined to them. The registration's own pattern
// only selects which requests reach it.
func (s *routeScan) stripPrefix(site *wire.Site, parent *routeObj) {
	k := s.resultSite(argType(site, 1))
	ref := s.siteRef(k)
	if ref == nil || !ref.Qualified || ref.Name != "StripPrefix" || s.goImported(ref.Qualifier) != fwNetHTTP {
		return
	}
	strip := &s.link.Sites[k]
	child := s.objOf(argType(strip, 1))
	if child == nil {
		return
	}
	a := routeAttach{parent: parent}
	if args := positional(s.siteArgs(strip)); len(args) == 2 {
		a.prefix, a.known = stringLiteral("go", args[0].text)
	}
	child.atts = append(child.atts, a)
}

// goServerLiterals serves each router a net/http Server literal names as
// its Handler (`&http.Server{Addr: a, Handler: r}`), which the program then
// serves: the router named by a call written there, or by a name the file
// assigns exactly one router to.
func (s *routeScan) goServerLiterals() {
	httpName := ""
	for name, imp := range s.imports {
		if strings.Trim(imp.path, "\"`") == "net/http" {
			httpName = name
		}
	}
	if httpName == "" || s.link == nil || !strings.Contains(string(s.b.src), "Server{") {
		return
	}
	sc := &inputScan{b: s.b}
	sc.lex()
	for i := 0; i+4 < len(sc.toks); i++ {
		if !sc.ident(i) || sc.text(i) != httpName || !sc.punct(i+1, '.') || !sc.ident(i+2) || sc.text(i+2) != "Server" ||
			!sc.punct(i+3, '{') || sc.match[i+3] < 0 {
			continue
		}
		end := int(sc.match[i+3])
		for k := i + 4; k+2 < end; k++ {
			if sc.match[k] > 0 {
				k = int(sc.match[k])
				continue
			}
			if !(sc.punct(k-1, '{') || sc.punct(k-1, ',')) || !sc.ident(k) || sc.text(k) != "Handler" || !sc.punct(k+1, ':') {
				continue
			}
			if o := s.objectAt(sc, k+2, end); o != nil {
				o.atts = append(o.atts, routeAttach{served: true, known: true})
			}
		}
		i = end
	}
}

// objectAt is the router the expression at token k (before token end)
// denotes: a call written there that builds one, or a name the file
// assigns exactly one router to, by `:=`, `=` or `var … =`.
func (s *routeScan) objectAt(sc *inputScan, k, end int) *routeObj {
	if site, ok := s.callAt(sc, k, end); ok {
		return s.objs[site]
	}
	if !sc.ident(k) || !(sc.punct(k+1, ',') || sc.punct(k+1, '}') || k+1 == end) {
		return nil
	}
	name := sc.text(k)
	var found *routeObj
	for j := 0; j+2 < len(sc.toks); j++ {
		if !sc.ident(j) || sc.text(j) != name || sc.punct(j-1, '.') {
			continue
		}
		v := j + 1
		if sc.punct(v, ':') {
			v++
		}
		if !sc.punct(v, '=') || sc.punct(v+1, '=') {
			continue
		}
		site, ok := s.callAt(sc, v+1, len(sc.toks))
		if !ok {
			continue
		}
		o := s.objs[site]
		if o == nil {
			continue
		}
		if found != nil && found != o {
			return nil
		}
		found = o
	}
	return found
}

// callAt is the worker site of the call written at token k: a name or a
// dotted name followed by its argument list.
func (s *routeScan) callAt(sc *inputScan, k, end int) (int, bool) {
	if sc.punct(k, '&') {
		k++
	}
	if !sc.ident(k) {
		return 0, false
	}
	for sc.punct(k+1, '.') && sc.ident(k+2) {
		k += 2
	}
	if k+1 >= end || !sc.punct(k+1, '(') {
		return 0, false
	}
	site, ok := s.siteAt[wire.Span{Start: sc.toks[k].start, End: sc.toks[k].end}]
	return site, ok
}
