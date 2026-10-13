package link

import (
	"context"
	"encoding/json"
	"slices"
	"strconv"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/wire"
)

// Go routes across files (docs/providers-link.md, Route prefixes): a Go
// router reaches another file through a call, never through a module
// attribute. A function that registers routes on a router it receives
// (`func Register(r chi.Router)`, `rg *gin.RouterGroup`), or builds and
// returns (`func Routes() chi.Router`), leaves them open on that parameter
// or result; a file that passes a router to such a call, or mounts its
// result, joins them. The walk resolves the call as the file's own
// resolution does (staticFile.resolve), in the file's context, binds the
// argument to the parameter by the callee's declared parameters
// (argumentFlows), and reads the callee's open routing from the hierarchy's
// links under keyGoRouting, so the file walking records each read.

// keyGoRouting + refKey(callable) is the open Go routing of that callable:
// its parameter and returned routers, their routes and the mounts made on
// them, as a goRoutePayload in Sig.
const keyGoRouting = "gr\x00"

// MountCall is the Go call a mount names, as written and with no position:
// what resolving it in its file reads. Receiver is never a call's result,
// which names a site by index: such a call is not carried, and its mount
// is not published. Spreads is each argument's spread, when the worker sent
// argument positions; Function is, for JoinFunction, the function value as
// written.
type MountCall struct {
	Shape     wire.Shape    `json:"shape,omitempty"`
	Callee    string        `json:"callee,omitempty"`
	Qualifier string        `json:"qualifier,omitempty"`
	Receiver  wire.Type     `json:"receiver"`
	Described bool          `json:"described,omitempty"`
	Args      int32         `json:"args,omitempty"`
	Spreads   []wire.Spread `json:"spreads,omitempty"`
	Function  string        `json:"function,omitempty"`
}

// CallOf is site s as a mount carries it, false when its receiver is a
// call's result.
func CallOf(s *Site) (*MountCall, bool) {
	if s.Receiver.Kind == wire.TypeResult {
		return nil, false
	}
	c := &MountCall{Shape: s.Shape, Callee: s.Callee, Qualifier: s.Qualifier, Receiver: s.Receiver, Described: s.Described,
		Args: int32(len(s.Args))}
	if len(s.Arguments) > 0 {
		c.Spreads = make([]wire.Spread, len(s.Arguments))
		for i, a := range s.Arguments {
			c.Spreads[i] = a.Spread
		}
	}
	return c, true
}

// site is the call as a site of a table of its own.
func (c *MountCall) site() Site {
	s := Site{Caller: -1, Callee: c.Callee, Shape: c.Shape, Qualifier: c.Qualifier, Receiver: c.Receiver,
		Described: c.Described, Args: make([]wire.Type, c.Args)}
	if len(c.Spreads) > 0 {
		s.Arguments = make([]Argument, len(c.Spreads))
		for i, sp := range c.Spreads {
			s.Arguments[i].Spread = sp
		}
	}
	return s
}

// goRoutePayload is a callable's open Go routing as keyGoRouting carries
// it: the file, the callable's qualified name and its open objects.
type goRoutePayload struct {
	Path     string          `json:"p"`
	Callable string          `json:"c"`
	Objects  []goRouteObject `json:"o"`
}

// goRouteObject is one open object of a callable, the routes on it and the
// Go mounts made on it, each with no range or site.
type goRouteObject struct {
	Object RouteObject  `json:"o"`
	Routes []RouteEntry `json:"r,omitempty"`
	Mounts []RouteMount `json:"m,omitempty"`
}

// addGoRouting adds a Go member's open routing to the hierarchy's links:
// one keyGoRouting entry per callable with a parameter or returned router.
func addGoRouting(links interface{ Add(entry) error }, file int32, t *Table) error {
	rt := &t.Routing
	if familyOf(t.Language) != "go" || len(rt.Objects) == 0 {
		return nil
	}
	byDecl := map[int32]*goRoutePayload{}
	var order []int32
	for i, o := range rt.Objects {
		if o.Param == 0 && !o.Returned || o.Decl < 0 || int(o.Decl) >= len(t.Decls) {
			continue
		}
		p, ok := byDecl[o.Decl]
		if !ok {
			p = &goRoutePayload{Path: t.Path, Callable: t.Decls[o.Decl].Qualified}
			byDecl[o.Decl] = p
			order = append(order, o.Decl)
		}
		obj := goRouteObject{Object: o}
		for _, r := range rt.Routes {
			if r.Object == int32(i) {
				obj.Routes = append(obj.Routes, r)
			}
		}
		for _, m := range rt.Mounts {
			if m.Object != int32(i) || m.Join == JoinNone || m.Call == nil {
				continue
			}
			// The walk reads the mount's call, never where it is (the
			// projection already dropped its site).
			m.Range = model.SourceRange{}
			obj.Mounts = append(obj.Mounts, m)
		}
		p.Objects = append(p.Objects, obj)
	}
	for _, d := range order {
		raw, err := json.Marshal(byDecl[d])
		if err != nil {
			return internalErr("go routing payload: " + err.Error())
		}
		ref := Ref{File: file, Index: d}
		if err := links.Add(entry{Key: keyGoRouting + refKey(ref), Ref: ref, Sig: string(raw)}); err != nil {
			return err
		}
	}
	return nil
}

// goRouteFacts walks down from each Go mount of the member at ordinal file
// anchored at an application root, or at a router the program serves, and
// emits one FactRoute per route it reaches and handler, on the mount, as
// emitPythonRoutes does. A mount whose call resolves to several callables
// walks each; a callable already on the walk's way ends it, so recursion
// ends.
func (h *hierarchy) goRouteFacts(ctx context.Context, file int32, t *Table, emit func(Binding) error) error {
	if familyOf(t.Language) != "go" {
		return nil
	}
	fc := contextOf(file, t)
	var found []routeFound
	for i := range t.Routing.Mounts {
		m := &t.Routing.Mounts[i]
		if m.Join == JoinNone || m.Object >= 0 || m.Call == nil {
			continue
		}
		n := len(found)
		w := goWalk{h: h, ctx: ctx, found: &found}
		if err := w.mount(fc, t.Path, m, m.Base, m.BaseKnown, nil); err != nil {
			return err
		}
		for k := n; k < len(found); k++ {
			found[k].mount = int32(i)
		}
	}
	return emitRoutes(file, found, emit)
}

// goWalk is one walk down from a Go root mount.
type goWalk struct {
	h     *hierarchy
	ctx   context.Context
	found *[]routeFound
}

// goHop is one open object a walk descends through: the callable and its
// parameter, or its result.
type goHop struct {
	callable Ref
	param    uint32
	returned bool
}

// resolver is a resolution of calls written in the file of context fc.
func (w *goWalk) resolver(fc fileCtx, path string) *staticFile {
	return &staticFile{h: w.h, ctx: w.ctx, file: fc.file, t: &Table{Path: path, Language: fc.language}, fc: fc,
		returns: map[int32][]returned{}}
}

// targets yields each callable mount m, written in the file of fc, reaches,
// with the parameter its object is (wire.Parameter.Index), 0 for a result.
func (w *goWalk) targets(fc fileCtx, path string, m *RouteMount, yield func(Ref, uint32) error) error {
	r := w.resolver(fc, path)
	if m.Join == JoinFunction {
		// chi hands the router to the function's first parameter.
		return r.function(-1, m.Call.Function, func(e entry) error { return yield(e.Ref, 1) })
	}
	s := m.Call.site()
	r.t.Sites = []Site{s}
	return r.resolve(0, nil, siteOut{call: func(e entry, _ string, _ model.Dispatch) error {
		if m.Join == JoinResult {
			return yield(e.Ref, 0)
		}
		var params []paramInfo
		if err := w.h.parameters(e.Ref, func(p paramInfo) error {
			params = append(params, p)
			return nil
		}); err != nil {
			return err
		}
		index, bound := uint32(0), false
		err := argumentFlows(&s, params, r.receiverFirst(&s, e), func(arg, pos int32) error {
			for _, p := range params {
				if arg == m.Arg && p.Pos == pos && !p.Variadic {
					index, bound = p.Index, true
				}
			}
			return nil
		})
		if err != nil || !bound {
			return err
		}
		return yield(e.Ref, index)
	}})
}

// mount follows mount m written in the file of fc (path), under the parent
// prefix base, past the objects on the way.
func (w *goWalk) mount(fc fileCtx, path string, m *RouteMount, base string, known bool, way []goHop) error {
	return w.targets(fc, path, m, func(callable Ref, param uint32) error {
		var raw string
		if err := w.h.links.lookup(keyGoRouting+refKey(callable), func(e entry) error {
			raw = e.Sig
			return nil
		}); err != nil || raw == "" {
			return err
		}
		var p goRoutePayload
		if err := json.Unmarshal([]byte(raw), &p); err != nil {
			return internalErr("go routing payload: " + err.Error())
		}
		tfc, ok, err := w.h.fileContext(callable.File)
		if err != nil || !ok {
			return err
		}
		for _, o := range p.Objects {
			hop := goHop{callable: callable, param: o.Object.Param, returned: o.Object.Returned}
			if !goMountable(m, o.Object, param) || slices.Contains(way, hop) {
				continue
			}
			pre, preKnown := joinPrefix(m.Prefix, o.Object.Own), m.PrefixKnown && o.Object.OwnKnown
			full, fullKnown := joinPrefix(base, pre), known && preKnown
			object := p.Callable + "#" + strconv.FormatUint(uint64(o.Object.Param), 10)
			for _, r := range o.Routes {
				hs, err := w.handlers(tfc, p.Path, callable.File, r)
				if err != nil {
					return err
				}
				*w.found = append(*w.found, routeFound{file: callable.File, path: p.Path, object: object, route: r,
					full: joinRoute(full, r.Rel), known: fullKnown && r.Known, handlers: hs})
			}
			next := append(slices.Clone(way), hop)
			for k := range o.Mounts {
				cm := &o.Mounts[k]
				if err := w.mount(tfc, p.Path, cm, joinPrefix(full, cm.Base), fullKnown && cm.BaseKnown, next); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// goMountable reports that mount m, whose call reached a callable's
// parameter param (0 for its result), mounts that callable's object o: the
// object is that parameter, or the result for a mounted result, and of the
// mount's framework, since a router of one framework registers its routes
// on a router of another only by being served under it whole.
func goMountable(m *RouteMount, o RouteObject, param uint32) bool {
	if o.Framework != m.Framework {
		return false
	}
	if m.Join == JoinResult {
		return o.Returned
	}
	return o.Param == param && param > 0
}

// handlers are the declarations route r of the member at ordinal file (in
// the file of fc, at path) routes to: those its file declares, and each
// one it names as written, read in that file's context.
func (w *goWalk) handlers(fc fileCtx, path string, file int32, r RouteEntry) ([]Ref, error) {
	out := make([]Ref, 0, len(r.Handlers)+len(r.Named))
	for _, h := range r.Handlers {
		out = append(out, Ref{File: file, Index: h})
	}
	if len(r.Named) > 0 {
		res := w.resolver(fc, path)
		for _, name := range r.Named {
			if err := res.function(-1, name, func(e entry) error {
				out = append(out, e.Ref)
				return nil
			}); err != nil {
				return nil, err
			}
		}
	}
	slices.SortFunc(out, compareRef)
	return slices.Compact(out), nil
}
