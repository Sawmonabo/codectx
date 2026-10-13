package treesitter

import (
	"strings"

	"github.com/Sawmonabo/codectx/internal/provider/link"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/wire"
)

// Go routing across files (docs/providers-link.md, Route prefixes). A Go
// router reaches another file through a call: a callable registers routes
// on a router it receives as a parameter, or builds and returns one; a
// file passes a router to a call, mounts a call's result (chi `Mount`), or
// hands chi's `Route` or `Group` a function value. goLinks leaves each open
// for the linker, which resolves the call and joins the callable's routing
// under the router's prefix.

// goFns is the file's analysed functions with the callable each is the
// body of (builder.callableOf), -1 for a function literal or a function no
// callable declares.
type goFns struct {
	callable []int32
}

// goFnsOf builds the file's goFns on first use.
func (s *routeScan) goFnsOf() *goFns {
	if s.fns != nil {
		return s.fns
	}
	fns := s.b.ex.functions
	spans := make([]wire.Span, len(fns))
	for i := range fns {
		spans[i] = fns[i].Span
	}
	parent := innermost(spans, spans, true)
	byStart := s.b.declsByStart()
	g := &goFns{callable: make([]int32, len(fns))}
	for i := range fns {
		g.callable[i] = s.b.callableOf(byStart, spans, parent, i)
	}
	s.fns = g
	return g
}

// innermostFn is the analysed function whose span holds at most tightly,
// or -1.
func (s *routeScan) innermostFn(at wire.Span) int {
	best := -1
	for i := range s.b.ex.functions {
		f := s.b.ex.functions[i].Span
		if f.Start <= at.Start && at.End <= f.End && (best < 0 || f.End-f.Start < s.b.ex.functions[best].Span.End-s.b.ex.functions[best].Span.Start) {
			best = i
		}
	}
	return best
}

// goParamObj is the router a Go callable receives as its parameter name,
// written at at with the declared type typ, a router type of a framework
// the file imports: one object per callable and parameter. It is nil
// unless name, at at, is that parameter's value for certain: at is in the
// callable's own body (not a function literal inside it), the body
// declares no other variable of that name, and nothing assigns the
// parameter.
func (s *routeScan) goParamObj(name string, at wire.Span, typ wire.Type) *routeObj {
	fw := s.goTypeFramework(typ)
	if s.link == nil || fw == "" || !goIdent(name) {
		return nil
	}
	fi := s.innermostFn(at)
	if fi < 0 {
		return nil
	}
	d := s.goFnsOf().callable[fi]
	f := &s.b.ex.functions[fi]
	if d < 0 || f.Failed {
		return nil
	}
	var param *wire.Parameter
	for k := range s.link.Parameters {
		p := &s.link.Parameters[k]
		if i, ok := s.b.nameAt[p.Callable]; ok && i == int(d) && s.text(p.Name) == name {
			param = p
		}
	}
	if param == nil {
		return nil
	}
	v := -1
	for k, x := range f.Vars {
		if s.text(x) != name {
			continue
		}
		if x != param.Name {
			return nil
		}
		v = k
	}
	if v < 0 {
		return nil
	}
	for _, w := range f.Writes {
		if int(w.Var) == v && f.Nodes[w.Node] != param.Name {
			return nil
		}
	}
	key := [2]int{int(d), int(param.Index)}
	if o := s.params[key]; o != nil {
		return o
	}
	if s.params == nil {
		s.params = map[[2]int]*routeObj{}
	}
	o := &routeObj{fw: fw, ownKnown: true, callable: int(d) + 1, param: param.Index}
	s.params[key] = o
	return o
}

// goIdent reports a Go identifier.
func goIdent(name string) bool {
	if name == "" || name == "_" {
		return false
	}
	for i, c := range name {
		if !(c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || i > 0 && c >= '0' && c <= '9' || c > 0x7f) {
			return false
		}
	}
	return true
}

// goPassed records each call that passes a router of the file, built in it
// or received as a parameter, as an argument of a call that is no
// framework's own (`users.Register(v1)`): the callee's parameter receives
// it, and the linker joins the callee's routing on that parameter below
// the router.
func (s *routeScan) goPassed() {
	s.calls(func(r *wire.Ref, site *wire.Site, i int) {
		if r.Qualified && s.goImported(r.Qualifier) != "" || s.goTypeFramework(site.Receiver) != "" ||
			s.goRecvObj(r, site) != nil {
			return
		}
		args := s.siteArgs(site)
		if len(args) != len(site.Args) {
			return
		}
		for a, arg := range args {
			if arg.kw != "" {
				continue
			}
			o := s.objOf(site.Args[a])
			if o == nil {
				o = s.goParamObj(arg.text, arg.at, site.Args[a])
			}
			if o != nil {
				s.pending = append(s.pending, pendingMount{site: i, arg: a, fw: o.fw, parent: o, known: true,
					join: link.JoinArgument, at: refSpan(r)})
			}
		}
	})
}

// goMountResult records a chi `Mount(prefix, f())` on parent whose mounted
// router is the result of a call that is no framework's own and builds no
// object of the file: the linker joins the router the callee returns.
func (s *routeScan) goMountResult(r *wire.Ref, site *wire.Site, parent *routeObj) {
	if parent == nil || parent.fw != fwChi {
		return
	}
	j := s.resultSite(argType(site, 1))
	if j < 0 || s.objs[j] != nil {
		return
	}
	jr := s.siteRef(j)
	if jr == nil || jr.Qualified && s.goImported(jr.Qualifier) != "" {
		return
	}
	args := positional(s.siteArgs(site))
	if len(args) != 2 {
		return
	}
	prefix, known := stringLiteral("go", args[0].text)
	s.pending = append(s.pending, pendingMount{site: j, arg: -1, fw: fwChi, parent: parent, prefix: prefix, known: known,
		join: link.JoinResult, at: refSpan(r)})
}

// goReturned marks each router the file builds and attaches nowhere that
// its callable returns: the router's construction is the whole value
// assigned to one variable, that variable is assigned nowhere else, and
// every return of the callable is `return` of it.
func (s *routeScan) goReturned() {
	for i, o := range s.objs {
		if o == nil || o.root || len(o.atts) > 0 || o.callable > 0 {
			continue
		}
		r := s.siteRef(i)
		if r == nil {
			continue
		}
		fi := s.innermostFn(refSpan(r))
		if fi < 0 {
			continue
		}
		d := s.goFnsOf().callable[fi]
		f := &s.b.ex.functions[fi]
		if d < 0 || f.Failed || len(f.Returns) == 0 {
			continue
		}
		n := -1
		for k, x := range f.Nodes {
			if x.Start <= r.Start && r.End <= x.End && (n < 0 || x.End-x.Start < f.Nodes[n].End-f.Nodes[n].Start) {
				n = k
			}
		}
		if n < 0 || !s.goReturnsDefinedAt(f, n, r) {
			continue
		}
		o.callable, o.returned = int(d)+1, true
	}
}

// goReturnsDefinedAt reports that node n of f assigns the call r alone to
// one variable that nothing else assigns and that every return of f
// returns.
func (s *routeScan) goReturnsDefinedAt(f *wire.Function, n int, r *wire.Ref) bool {
	v := -1
	for _, w := range f.Writes {
		if int(w.Node) == n {
			if v >= 0 && int(w.Var) != v {
				return false
			}
			v = int(w.Var)
		}
	}
	if v < 0 {
		return false
	}
	for _, w := range f.Writes {
		if int(w.Var) == v && int(w.Node) != n {
			return false
		}
	}
	name := s.text(f.Vars[v])
	rest, ok := goAssigned(s.text(f.Nodes[n]), name)
	callee := r.Name
	if r.Qualified {
		callee = r.Qualifier + "." + r.Name
	}
	if !ok || !goSoleCall(rest, callee) {
		return false
	}
	for _, rn := range f.Returns {
		if strings.Join(strings.Fields(s.text(f.Nodes[rn])), " ") != "return "+name {
			return false
		}
	}
	return true
}

// goAssigned is the value statement assigns to name alone, `name := x`,
// `name = x` or `var name = x`, trimmed.
func goAssigned(statement, name string) (string, bool) {
	t := strings.TrimSpace(statement)
	if rest, ok := strings.CutPrefix(t, "var "); ok {
		t = strings.TrimSpace(rest)
	}
	rest, ok := strings.CutPrefix(t, name)
	if !ok {
		return "", false
	}
	rest = strings.TrimSpace(rest)
	for _, op := range []string{":=", "="} {
		if v, ok := strings.CutPrefix(rest, op); ok {
			return strings.TrimSpace(v), true
		}
	}
	return "", false
}

// goSoleCall reports that x is one call of callee: callee, then an
// argument list whose closing parenthesis ends x.
func goSoleCall(x, callee string) bool {
	rest, ok := strings.CutPrefix(x, callee)
	if !ok {
		return false
	}
	rest = strings.TrimSpace(rest)
	if !strings.HasPrefix(rest, "(") {
		return false
	}
	depth := 0
	for i := 0; i < len(rest); i++ {
		switch c := rest[i]; c {
		case '"', '\'', '`':
			end := strings.IndexByte(rest[i+1:], c)
			for c != '`' && end >= 0 && strings.HasSuffix(rest[i+1:i+1+end], "\\") {
				next := strings.IndexByte(rest[i+2+end:], c)
				if next < 0 {
					return false
				}
				end += 1 + next
			}
			if end < 0 {
				return false
			}
			i += end + 1
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return i == len(rest)-1
			}
		}
	}
	return false
}

// goLinks builds the Go file's open routing: each parameter or returned
// router of a callable, the routes whose in-file chain reaches one, and
// each Go mount (pendingMount.join) whose parent's chain reaches one or an
// application root.
func (s *routeScan) goLinks() *routeLinks {
	out := &routeLinks{}
	index := map[*routeObj]int32{}
	open := func(o *routeObj) (int32, bool) {
		if i, ok := index[o]; ok {
			return i, true
		}
		if o.callable == 0 {
			return -1, false
		}
		out.routing.Objects = append(out.routing.Objects, link.RouteObject{Decl: int32(o.callable - 1), Framework: o.fw,
			Own: o.own, OwnKnown: o.ownKnown, Param: o.param, Returned: o.returned})
		index[o] = int32(len(out.routing.Objects) - 1)
		return index[o], true
	}
	for _, f := range s.out {
		if f.obj == nil {
			continue
		}
		c, ok := s.chains(f.obj, nil)
		if !ok || c.top == nil {
			continue
		}
		oi, ok := open(c.top)
		if !ok {
			continue
		}
		out.routing.Routes = append(out.routing.Routes, link.RouteEntry{Object: oi, Framework: f.meta.Framework,
			Method: f.meta.Method, Route: f.meta.Route, Rel: joinPrefix(c.prefix, f.value), Known: c.known && f.literal,
			Handlers: s.declHandlers(f.handlers), Named: s.goNamed(f.handlers)})
	}
	for _, m := range s.pending {
		if m.join == link.JoinNone {
			continue
		}
		c, ok := s.chains(m.parent, nil)
		if !ok {
			continue
		}
		mount := link.RouteMount{Site: -1, Arg: int32(m.arg), Framework: m.fw, Prefix: m.prefix, PrefixKnown: m.known,
			Object: -1, Base: c.prefix, BaseKnown: c.known, Join: m.join}
		if m.join == link.JoinFunction {
			mount.Call = &link.MountCall{Function: m.function}
		}
		if c.top != nil {
			oi, ok := open(c.top)
			if !ok {
				continue
			}
			mount.Object = oi
		}
		rng, err := s.b.rangeOf(m.at.Start, m.at.End)
		if err != nil {
			continue
		}
		mount.Range = *rng
		out.routing.Mounts = append(out.routing.Mounts, mount)
		out.sites = append(out.sites, m.site)
	}
	if len(out.routing.Objects)+len(out.routing.Mounts) == 0 {
		return nil
	}
	return out
}

// goNamed are the handlers a Go route names that the file does not
// declare, as written: a qualified name (`handlers.List`), or a bare name
// of another file of the package.
func (s *routeScan) goNamed(hs []handler) []string {
	var out []string
	for _, h := range hs {
		switch {
		case h.decl >= 0 || h.ref == nil:
		case h.ref.Qualified:
			out = append(out, h.ref.Qualifier+"."+h.ref.Name)
		case len(s.declHandlers([]handler{h})) == 0:
			out = append(out, h.ref.Name)
		}
	}
	return out
}

// goMountCall gives a Go argument or result mount the call at its site
// (link.CallOf), false when that site's receiver is a call's result, which
// drops the mount.
func goMountCall(m *link.RouteMount, sites []link.Site) bool {
	switch m.Join {
	case link.JoinArgument, link.JoinResult:
		if m.Site < 0 || int(m.Site) >= len(sites) {
			return false
		}
		c, ok := link.CallOf(&sites[m.Site])
		if !ok {
			return false
		}
		m.Call = c
	}
	return true
}

// goFunctionValue is the function argument a names, as written: the
// worker's function value, or a function of a package the file imports
// (`users.Routes`), which the worker leaves untyped, when no variable of a
// function around it is named as that import.
func (s *routeScan) goFunctionValue(t wire.Type, a routeArg) (string, bool) {
	if t.Kind == wire.TypeFunction && t.Name != "" {
		return t.Name, true
	}
	pkg, name, ok := strings.Cut(a.text, ".")
	if t.Kind != wire.TypeUnknown || !ok || !goIdent(pkg) || !goIdent(name) {
		return "", false
	}
	if _, imported := s.imports[pkg]; !imported || s.goImported(pkg) != "" {
		return "", false
	}
	for i := range s.b.ex.functions {
		f := &s.b.ex.functions[i]
		if f.Span.Start > a.at.Start || a.at.End > f.Span.End {
			continue
		}
		for _, v := range f.Vars {
			if s.text(v) == pkg {
				return "", false
			}
		}
	}
	return a.text, true
}
