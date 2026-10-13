package treesitter

import (
	"cmp"
	"slices"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider/link"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/lang"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/wire"
	"github.com/Sawmonabo/codectx/internal/reconcile"
)

// detailReturn is the evidence detail of a value created inside a callable
// flowing into the callable's return node.
const detailReturn = "return"

// Return flow (docs/providers-treesitter.md, Return flow). A callable's
// return node stands for the value it returns. Only values created inside
// the callable flow into it, so a walk that entered the callable through one
// call site's argument can never leave through another site's result: the
// arguments' own way back is the callee's return summary, a parameter that
// reaches a return (link.Parameter.Returned), which the linker publishes per
// bound site.
//
// For each analysed function that is the callable of a declaration of the
// file (callableOf), with E its entry variables (its parameters and the
// variables it captures from an enclosing function) and Flows its
// data_flows_to pairs:
//   - F is every variable reachable from E over Flows, B every variable that
//     reaches a returning node over Flows (wire.Function.Returned);
//   - a parameter in B is Returned;
//   - a variable of the function's own outside F that reaches a return
//     directly, or through a flow into a variable of B ∩ F, flows into the
//     return node, evidenced at that returning node or that flow's node.
//
// Each call site of the table takes its Results, the variables the
// innermost node of the innermost function evaluating it defines, and Into,
// the callable whose return that node is, unless another call between the
// two consumes the site's value first.

// returnFlows publishes every analysed callable's return node and the
// created values flowing into it, marks each Returned parameter, and fills
// each site's Results and Into. It runs after tableVariables, on the
// endpoints vars resolved, and does nothing for a file with no link table.
func (b *builder) returnFlows(vars map[wire.Span]fileVar, byStart []int) error {
	t := b.table
	if t == nil || len(b.ex.functions) == 0 {
		return nil
	}
	fns := b.ex.functions
	spans := make([]wire.Span, len(fns))
	for i := range fns {
		spans[i] = fns[i].Span
	}
	parent := innermost(spans, spans, true)
	callable := make([]int32, len(fns))
	rets := make(map[int32]returnNode)
	for i := range fns {
		callable[i] = b.callableOf(byStart, spans, parent, i)
		if d := callable[i]; d >= 0 && t.Decls[d].Return == nil {
			r, err := b.returnNode(int(d))
			if err != nil {
				return err
			}
			if r.ep.Node != "" {
				rets[d] = r
				t.Decls[d].Return = &r.ep
			}
		}
	}
	published := map[int32]bool{}
	publish := func(d int32) {
		if !published[d] {
			published[d] = true
			r := rets[d]
			b.putNode(dependence, r.res, &r.ep.Range, r.key, nil)
		}
	}
	endpoint := func(s wire.Span) (link.Endpoint, bool) {
		fv, ok := vars[s]
		return fv.ep, ok && fv.resolved && fv.ep.Node != ""
	}
	for i := range fns {
		d := callable[i]
		ret, ok := rets[d]
		if d < 0 || !ok || fns[i].Failed {
			continue
		}
		f := &fns[i]
		created := created(f, &t.Decls[d])
		_, ownerKey, _ := b.owner(byStart, f.Span)
		for _, c := range created {
			from, ok := endpoint(f.Vars[c.Var])
			if !ok {
				continue
			}
			rng, err := b.rangeOf(f.Nodes[c.Node].Start, f.Nodes[c.Node].End)
			if err != nil {
				return err
			}
			publish(d)
			b.putRelation(dependence, from.Node, model.RelDataFlowsTo, ret.ep.Node, rng, ownerKey, detailReturn)
		}
	}
	return b.siteResults(spans, callable, rets, endpoint, publish)
}

// callableOf is the declaration whose callable function i is, or -1: the
// innermost declaration of a callable kind containing the function, when no
// enclosing function lies inside that declaration too (a nested function's
// innermost declaration is its enclosing function's).
func (b *builder) callableOf(byStart []int, spans []wire.Span, parent []int, i int) int32 {
	d := b.enclosing(byStart, spans[i])
	if d < 0 {
		return -1
	}
	decl := &b.decls[d]
	switch decl.kind {
	case model.NodeFunction, model.NodeMethod, model.NodeTest:
	default:
		return -1
	}
	if p := parent[i]; p >= 0 && spans[p].Start >= decl.Start && spans[p].End <= decl.End {
		return -1
	}
	if b.constructor(decl) {
		return -1
	}
	return d
}

// constructor reports a declaration the language makes a constructor that
// returns no value, whose call's value is the instance it initialises and
// never what its body returns: a Python `__init__` (the Python Language
// Reference §3.3.1: no non-None value may be returned), a Java or C++
// constructor, a method named as the type that declares it (JLS §8.8.7; C++
// [class.ctor]). An ECMAScript `constructor` may return an object that then
// is the construction's value (ECMA-262 [[Construct]]), so it keeps its
// return node.
func (b *builder) constructor(decl *declFact) bool {
	if decl.kind != model.NodeMethod {
		return false
	}
	switch b.lang.Name {
	case "python":
		return decl.Name == "__init__"
	case "java", "cpp":
		p := decl.Parent
		return p >= 0 && p < len(b.decls) && b.decls[p].Name == decl.Name
	}
	return false
}

// returnNode is a callable's return node: its endpoint as the link table
// names it, its resolution and its native key.
type returnNode struct {
	ep  link.Endpoint
	res model.Resolution
	key string
}

// returnNode resolves declaration d's return node: a variable named
// "return" at the declaration's name token, keyed by the callable's
// descriptor path and its disambiguator with the return mark, so a call in
// another file that reads it keeps the node through a body edit (ADR-0014);
// a callable without a descriptor path keeps a file-scoped key. It answers
// an empty endpoint when the key does not fit.
func (b *builder) returnNode(d int) (returnNode, error) {
	decl := &b.decls[d]
	key := b.rangeKey("ret:", decl.NameStart, decl.NameEnd)
	rng := decl.nameRng
	if rng == nil {
		rng = decl.rng
	}
	if key == "" || rng == nil {
		b.dropped[dependence]++
		return returnNode{}, nil
	}
	const name = "return"
	qualified := name
	if decl.Qualified != "" {
		qualified = bound(decl.Qualified+b.lang.Separator+name, model.MaxQualifiedNameBytes)
	}
	c := model.NodeCandidate{ProviderID: lang.ProviderID, ScopeKey: b.scope, NativeKey: key, Kind: model.NodeVariable,
		Language: b.lang.Name, Name: name, QualifiedName: qualified, FileID: b.fv.ID, ContentHash: b.fv.ContentHash, Range: rng}
	if decl.descriptor != "" {
		c.Descriptor, c.Disambiguator = decl.descriptor, reconcile.ReturnDisambiguator(decl.disambiguator)
	}
	res, err := b.resolve(c)
	if err != nil {
		return returnNode{}, err
	}
	res.Node.NameRange = rng
	return returnNode{ep: link.Endpoint{Node: res.Node.ID, CanonicalKey: res.CanonicalKey, Kind: model.NodeVariable,
		Name: name, Qualified: qualified, Range: *rng, NameRange: rng}, res: res, key: key}, nil
}

// created marks each parameter of decl in f whose value reaches a return
// Returned, and answers the values created inside f that flow into its
// return node, each a variable of f and the node evidencing it.
func created(f *wire.Function, decl *link.Decl) []wire.Access {
	n := len(f.Vars)
	param := make([]int, n)
	for v := range param {
		param[v] = -1
	}
	for j, p := range decl.Parameters {
		s := wire.Span{Start: uint32(p.Range.Start.Byte), End: uint32(p.Range.End.Byte)}
		if v, ok := slices.BinarySearchFunc(f.Vars, s, cmpSpan); ok {
			param[v] = j
		}
	}
	own := func(v uint32) bool { s := f.Vars[v]; return s.Start >= f.Span.Start && s.End <= f.Span.End }
	// fromEntry is F, toReturn is B.
	fromEntry, toReturn := make([]bool, n), make([]bool, n)
	var seeds []uint32
	for v := range uint32(n) {
		if param[v] >= 0 || !own(v) {
			seeds = append(seeds, v)
		}
	}
	reach(f.Flows, seeds, fromEntry, false)
	seeds = seeds[:0]
	for _, a := range f.Returned {
		seeds = append(seeds, a.Var)
	}
	reach(f.Flows, seeds, toReturn, true)
	for v, j := range param {
		if j >= 0 && toReturn[v] {
			decl.Parameters[j].Returned = true
		}
	}
	var out []wire.Access
	for _, a := range f.Returned {
		if !fromEntry[a.Var] && own(a.Var) {
			out = append(out, a)
		}
	}
	for _, p := range f.Flows {
		if toReturn[p.To] && fromEntry[p.To] && !fromEntry[p.From] && own(p.From) {
			out = append(out, wire.Access{Var: p.From, Node: p.Node})
		}
	}
	return out
}

// reach marks in seen every variable reachable from seeds over flows,
// forward (From to To) or, with back, backward.
func reach(flows []wire.Pair, seeds []uint32, seen []bool, back bool) {
	next := make(map[uint32][]uint32)
	for _, p := range flows {
		if back {
			next[p.To] = append(next[p.To], p.From)
		} else {
			next[p.From] = append(next[p.From], p.To)
		}
	}
	stack := append([]uint32(nil), seeds...)
	for _, v := range seeds {
		seen[v] = true
	}
	for len(stack) > 0 {
		v := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		for _, w := range next[v] {
			if !seen[w] {
				seen[w] = true
				stack = append(stack, w)
			}
		}
	}
}

// siteResults fills each site's Results and Into: the innermost analysed
// function containing the site, the innermost node of that function
// containing it, the variables that node defines, and the callable whose
// returning node it is; nothing when another site between the two holds
// the site's value first, or when no node of that function evaluates the
// site.
func (b *builder) siteResults(spans []wire.Span, callable []int32, rets map[int32]returnNode,
	endpoint func(wire.Span) (link.Endpoint, bool), publish func(int32)) error {
	t := b.table
	fns := b.ex.functions
	sites := make([]wire.Span, len(t.Sites))
	for i := range t.Sites {
		r := t.Sites[i].Range
		sites[i] = wire.Span{Start: uint32(r.Start.Byte), End: uint32(r.End.Byte)}
	}
	fnAt := innermost(spans, sites, false)
	outer := innermost(sites, sites, true)
	bySite := make([][]int, len(fns))
	for s, fi := range fnAt {
		if fi >= 0 && !fns[fi].Failed {
			bySite[fi] = append(bySite[fi], s)
		}
	}
	for fi, list := range bySite {
		if len(list) == 0 {
			continue
		}
		f := &fns[fi]
		queries := make([]wire.Span, len(list))
		for k, s := range list {
			queries[k] = sites[s]
		}
		nodeAt := innermost(f.Nodes, queries, false)
		for k, s := range list {
			n := nodeAt[k]
			if n < 0 {
				continue
			}
			node := f.Nodes[n]
			if o := outer[s]; o >= 0 && sites[o].Start >= node.Start && sites[o].End <= node.End {
				continue
			}
			site := &t.Sites[s]
			for _, w := range f.Writes {
				if int(w.Node) != n {
					continue
				}
				if ep, ok := endpoint(f.Vars[w.Var]); ok && !slices.ContainsFunc(site.Results,
					func(e link.Endpoint) bool { return e.Node == ep.Node }) {
					site.Results = append(site.Results, ep)
				}
			}
			if d := callable[fi]; d >= 0 && slices.Contains(f.Returns, uint32(n)) {
				if _, ok := rets[d]; ok {
					site.Into = d + 1
					publish(d)
				}
			}
		}
	}
	return nil
}

// innermost answers, for each query, the index of the innermost item that
// contains it, -1 when none does; with strict, an item equal to the query
// does not contain it. The items are laminar (two are nested or disjoint),
// as the spans of one file's syntax are.
func innermost(items, queries []wire.Span, strict bool) []int {
	order := func(spans []wire.Span) []int {
		o := make([]int, len(spans))
		for i := range o {
			o[i] = i
		}
		slices.SortStableFunc(o, func(a, b int) int {
			return cmp.Or(cmp.Compare(spans[a].Start, spans[b].Start), cmp.Compare(spans[b].End, spans[a].End))
		})
		return o
	}
	io, qo := order(items), order(queries)
	out := make([]int, len(queries))
	var stack []int
	k := 0
	for _, q := range qo {
		qs := queries[q]
		for ; k < len(io) && items[io[k]].Start <= qs.Start; k++ {
			s := items[io[k]]
			for len(stack) > 0 && items[stack[len(stack)-1]].End <= s.Start {
				stack = stack[:len(stack)-1]
			}
			stack = append(stack, io[k])
		}
		out[q] = -1
		for j := len(stack) - 1; j >= 0; j-- {
			s := items[stack[j]]
			if s.Start <= qs.Start && s.End >= qs.End && !(strict && s == qs) {
				out[q] = stack[j]
				break
			}
		}
	}
	return out
}

// cmpSpan orders spans by start, then end.
func cmpSpan(a, b wire.Span) int {
	return cmp.Or(cmp.Compare(a.Start, b.Start), cmp.Compare(a.End, b.End))
}
