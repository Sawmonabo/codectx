package worker

import (
	"cmp"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/Sawmonabo/codectx/internal/provider/treesitter/flow"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/wire"
)

// depLists is the dependence pass's reusable state beside the worker's arena
// and lowering scratch: the file's callables, the message being encoded and
// the projection's lists. Each list is truncated per function and keeps its
// capacity across a file's functions; the worker drops them all at the file
// boundary (state.release). The zero value is ready to use.
type depLists struct {
	// callables are the file's callables in the lowering's preorder, collected
	// before the first is lowered.
	callables []Node
	// msg is the encoding of the function message being sent.
	msg []byte
	// fn is the message being projected; its lists are reused.
	fn wire.Function
	// varAt maps a flow variable to its index in fn.Vars, or -1 for a variable
	// the lowering owns; nodeAt maps a flow node to its index in fn.Nodes, or
	// -1 for a node no fact cites.
	varAt, nodeAt []int32
	// order is the sort scratch for the named variables and the cited nodes.
	order []int32
	// links are the def-use pairs' contributions to each node's Src, sorted by
	// node (see linkOwned); linkOff indexes them per node.
	links   []uint64
	linkOff []int32
	// src holds Src(n) for every node n, srcOff indexing it per node; seen
	// stamps the nodes and variables one node's walk has met.
	src          []uint32
	srcOff       []int32
	seenN, seenV []int32
	// stack is the walk's stack of nodes.
	stack []int32
}

// dependence runs the file's dependence pass over flat, the kept parse's flat
// array in language, after the structural facts were sent: one KindFunction
// message per callable, in preorder. A failure outside every function (no
// lowering, or a panic in the lowering's resolution or the callable walk)
// sends no message and is returned as the reason Done discloses; the callables
// are collected before the first is lowered, so no message precedes such a
// failure. The error is a write failure, which ends the loop.
func (w *state) dependence(out io.Writer, flat *Flat, language string, src []byte) (failure string, err error) {
	l, failure := w.walk(flat, language)
	if failure != "" {
		return failure, nil
	}
	return "", w.functions(out, l, src)
}

// walk resolves language's lowering and collects flat's callables, recovering
// a panic in either.
func (w *state) walk(flat *Flat, language string) (l *Lowering, failure string) {
	defer func() {
		if r := recover(); r != nil {
			l, failure = nil, "the dependence pass panicked outside every function: "+panicText(r)
		}
	}()
	l, ok := LoweringFor(language)
	if !ok {
		return nil, "the language " + language + " has no lowering"
	}
	w.collect(l, flat)
	return l, ""
}

// collect sets w.dep.callables to every callable of flat in l's preorder.
func (w *state) collect(l *Lowering, flat *Flat) {
	w.dep.callables = w.dep.callables[:0]
	// The visit never fails, so neither does the walk.
	_ = l.Functions(flat.Root(), func(fn Node) error {
		w.dep.callables = append(w.dep.callables, fn)
		return nil
	})
}

// functions lowers, analyses and sends every collected callable in turn. A
// function that fails is sent as failed and the next one is still analysed.
func (w *state) functions(out io.Writer, l *Lowering, src []byte) error {
	for _, fn := range w.dep.callables {
		if err := w.function(out, l, fn, src); err != nil {
			return err
		}
	}
	return nil
}

// function sends fn's message: its facts, or, when its lowering, analysis or
// projection panicked, its span, Failed and the recovered value's text, with
// whatever the panic left half-encoded discarded (ADR-0012 decision 9).
func (w *state) function(out io.Writer, l *Lowering, fn Node, src []byte) error {
	if cause, failed := w.analyse(l, fn, src); failed {
		f := wire.Function{Span: wireSpan(spanOf(fn)), Failed: true, Cause: cause}
		w.dep.msg = wire.AppendFunction(w.dep.msg[:0], &f)
	}
	return wire.WriteMessage(out, wire.KindFunction, w.dep.msg)
}

// analyse lowers fn into the worker's arena, runs post-dominators, control
// dependence and def-use, and encodes the projection into w.dep.msg. It
// recovers a panic anywhere in that and reports it as fn's failure.
func (w *state) analyse(l *Lowering, fn Node, src []byte) (cause string, failed bool) {
	defer func() {
		if r := recover(); r != nil {
			cause, failed = panicText(r), true
		}
	}()
	g := l.Lower(fn, src, &w.arena, &w.scratch)
	pd := flow.PostDominators(g, &w.arena)
	cd := flow.ControlDependence(g, pd, &w.arena)
	du := flow.DefUse(g, &w.arena)
	w.dep.msg = wire.AppendFunction(w.dep.msg[:0], w.dep.project(g, cd, du, spanOf(fn)))
	return "", false
}

// panicText is a recovered value's text, made valid UTF-8 as the wire
// requires of a cause.
func panicText(r any) string { return strings.ToValidUTF8(fmt.Sprint(r), "\uFFFD") }

// wireSpan is s on the wire.
func wireSpan(s flow.Span) wire.Span { return wire.Span{Start: s.Start, End: s.End} }

// A link is one contribution of a def-use pair (p, n) to Src(n), packed as
// the node n in the high half and, in the low half, either a named variable's
// message index or, with linkOwned set, the node p, whose Src flows on
// through an owned variable.
const linkOwned = 1 << 31

// project is g's dependence facts as the function message spanning fn, by the
// projection of docs/providers-treesitter.md (Dependence facts): the named
// variables, the cited nodes, and the control, flow, read and write facts
// between them, each sent once per node. The message and its lists are d's,
// valid until the next project.
func (d *depLists) project(g *flow.Graph, cd, du flow.Edges, fn flow.Span) *wire.Function {
	f := &d.fn
	*f = wire.Function{
		Span:       wireSpan(fn),
		Unresolved: uint32(g.Unresolved()),
		Vars:       f.Vars[:0],
		Nodes:      f.Nodes[:0],
		Control:    f.Control[:0],
		Flows:      f.Flows[:0],
		Reads:      f.Reads[:0],
		Writes:     f.Writes[:0],
	}
	d.variables(g)
	d.sources(g, du)
	n := int32(g.Len())
	for u := range n {
		for _, v := range g.Uses(u) {
			if x := d.varAt[v]; x >= 0 {
				f.Reads = append(f.Reads, wire.Access{Var: uint32(x), Node: uint32(u)})
			}
		}
		d.writes(g, u, func(y uint32, may bool) {
			f.Writes = append(f.Writes, wire.Access{Var: y, Node: uint32(u), May: may})
			for _, x := range d.srcOf(u) {
				if x != y {
					f.Flows = append(f.Flows, wire.Pair{From: x, To: y, Node: uint32(u)})
				}
			}
		})
	}
	for i := range cd.Len() {
		c, dep := cd.At(i)
		ys := d.srcOf(c)
		if len(ys) == 0 {
			continue
		}
		control := func(x uint32) {
			for _, y := range ys {
				if x != y {
					f.Control = append(f.Control, wire.Pair{From: x, To: y, Node: uint32(dep)})
				}
			}
		}
		d.writes(g, dep, func(x uint32, _ bool) { control(x) })
		for _, x := range d.srcOf(dep) {
			control(x)
		}
	}
	d.nodes(g)
	f.Control = pairs(f.Control, d.nodeAt)
	f.Flows = pairs(f.Flows, d.nodeAt)
	f.Reads = accesses(f.Reads, d.nodeAt)
	f.Writes = accesses(f.Writes, d.nodeAt)
	return f
}

// writes calls visit with each named variable node u defines (may false) or
// may define (may true): W(u).
func (d *depLists) writes(g *flow.Graph, u int32, visit func(x uint32, may bool)) {
	for _, v := range g.Defs(u) {
		if x := d.varAt[v]; x >= 0 {
			visit(uint32(x), false)
		}
	}
	for _, v := range g.MayDefs(u) {
		if x := d.varAt[v]; x >= 0 {
			visit(uint32(x), true)
		}
	}
}

// variables numbers g's named variables in the order of their declaring
// spans into d.varAt and d.fn.Vars. Two variables declared by one identifier
// are one variable of the message, since the parent mints one identity from
// the span.
func (d *depLists) variables(g *flow.Graph) {
	d.varAt = sized(d.varAt, g.Vars())
	d.order = d.order[:0]
	for v := range int32(g.Vars()) {
		if _, named := g.Declared(v); named {
			d.order = append(d.order, v)
		}
	}
	declared := func(v int32) flow.Span { s, _ := g.Declared(v); return s }
	slices.SortFunc(d.order, func(a, b int32) int { return bySpan(declared(a), declared(b), a, b) })
	for _, v := range d.order {
		s := wireSpan(declared(v))
		if k := len(d.fn.Vars); k == 0 || d.fn.Vars[k-1] != s {
			d.fn.Vars = append(d.fn.Vars, s)
		}
		d.varAt[v] = int32(len(d.fn.Vars) - 1)
	}
}

// nodes numbers every node a fact cites in the order of their spans into
// d.nodeAt and d.fn.Nodes, two nodes of one span being one node of the
// message, as the parent publishes one evidence range for both.
func (d *depLists) nodes(g *flow.Graph) {
	d.nodeAt = sized(d.nodeAt, g.Len())
	cite := func(n uint32) { d.nodeAt[n] = 0 }
	for _, p := range d.fn.Control {
		cite(p.Node)
	}
	for _, p := range d.fn.Flows {
		cite(p.Node)
	}
	for _, a := range d.fn.Reads {
		cite(a.Node)
	}
	for _, a := range d.fn.Writes {
		cite(a.Node)
	}
	d.order = d.order[:0]
	for n, at := range d.nodeAt {
		if at == 0 {
			d.order = append(d.order, int32(n))
		}
	}
	slices.SortFunc(d.order, func(a, b int32) int { return bySpan(g.Span(a), g.Span(b), a, b) })
	for _, n := range d.order {
		s := wireSpan(g.Span(n))
		if k := len(d.fn.Nodes); k == 0 || d.fn.Nodes[k-1] != s {
			d.fn.Nodes = append(d.fn.Nodes, s)
		}
		d.nodeAt[n] = int32(len(d.fn.Nodes) - 1)
	}
}

// sources computes Src(n) for every node n of g into d.src: the named
// variables whose value reaches n. Each def-use pair (p, n) contributes,
// for every variable v that p defines or may define and that n uses or may
// define (a may-definition reads v's prior version), v itself when it is
// named and Src(p) when it is owned. Src is the least fixed point of those
// equations: Src(n) is every named contribution to a node that reaches n
// through owned contributions, n included, which one walk per node collects,
// each node and variable met once, so a cycle of owned variables ends.
func (d *depLists) sources(g *flow.Graph, du flow.Edges) {
	n := g.Len()
	d.links = d.links[:0]
	for i := range du.Len() {
		p, u := du.At(i)
		carried := func(v int32) {
			if !holds(g.Uses(u), v) && !holds(g.MayDefs(u), v) {
				return
			}
			if x := d.varAt[v]; x >= 0 {
				d.links = append(d.links, uint64(u)<<32|uint64(x))
			} else {
				d.links = append(d.links, uint64(u)<<32|linkOwned|uint64(p))
			}
		}
		for _, v := range g.Defs(p) {
			carried(v)
		}
		for _, v := range g.MayDefs(p) {
			carried(v)
		}
	}
	slices.Sort(d.links)
	d.links = slices.Compact(d.links)
	d.linkOff = offsets(d.linkOff, n, len(d.links), func(i int) int { return int(d.links[i] >> 32) })

	d.src = d.src[:0]
	d.srcOff = append(d.srcOff[:0], 0)
	d.seenN = sized(d.seenN, n)
	d.seenV = sized(d.seenV, len(d.fn.Vars))
	for u := range int32(n) {
		start := len(d.src)
		d.seenN[u] = u
		d.stack = append(d.stack[:0], u)
		for len(d.stack) > 0 {
			m := d.stack[len(d.stack)-1]
			d.stack = d.stack[:len(d.stack)-1]
			for _, l := range d.links[d.linkOff[m]:d.linkOff[m+1]] {
				lo := uint32(l)
				switch {
				case lo&linkOwned != 0:
					if p := int32(lo &^ linkOwned); d.seenN[p] != u {
						d.seenN[p] = u
						d.stack = append(d.stack, p)
					}
				case d.seenV[lo] != u:
					d.seenV[lo] = u
					d.src = append(d.src, lo)
				}
			}
		}
		slices.Sort(d.src[start:])
		d.srcOff = append(d.srcOff, int32(len(d.src)))
	}
}

// srcOf is Src(n), ascending.
func (d *depLists) srcOf(n int32) []uint32 { return d.src[d.srcOff[n]:d.srcOff[n+1]] }

// holds reports whether the ascending list s holds v.
func holds(s []int32, v int32) bool {
	_, ok := slices.BinarySearch(s, v)
	return ok
}

// offsets is the n+1 offsets of a list of k entries sorted by key, entry i's
// key being key(i) in [0, n): entries of key m lie at [off[m], off[m+1]).
func offsets(off []int32, n, k int, key func(i int) int) []int32 {
	off = slices.Grow(off[:0], n+1)[:n+1]
	clear(off)
	for i := range k {
		off[key(i)+1]++
	}
	for m := range n {
		off[m+1] += off[m]
	}
	return off
}

// sized is s resized to n entries, each -1.
func sized(s []int32, n int) []int32 {
	s = slices.Grow(s[:0], n)[:n]
	for i := range s {
		s[i] = -1
	}
	return s
}

// bySpan orders two items by span, then by id.
func bySpan(a, b flow.Span, ia, ib int32) int {
	return cmp.Or(cmp.Compare(a.Start, b.Start), cmp.Compare(a.End, b.End), cmp.Compare(ia, ib))
}

// pairs renumbers each pair's flow node to its message node and sorts and
// deduplicates them, so a repeated fact at one node is sent once.
func pairs(ps []wire.Pair, nodeAt []int32) []wire.Pair {
	for i := range ps {
		ps[i].Node = uint32(nodeAt[ps[i].Node])
	}
	slices.SortFunc(ps, func(a, b wire.Pair) int {
		return cmp.Or(cmp.Compare(a.Node, b.Node), cmp.Compare(a.From, b.From), cmp.Compare(a.To, b.To))
	})
	return slices.Compact(ps)
}

// accesses renumbers and deduplicates as pairs does; a variable both defined
// and may-defined at one message node (two flow nodes of one span) is sent
// once, as the killing definition.
func accesses(as []wire.Access, nodeAt []int32) []wire.Access {
	for i := range as {
		as[i].Node = uint32(nodeAt[as[i].Node])
	}
	slices.SortFunc(as, func(a, b wire.Access) int {
		if c := cmp.Or(cmp.Compare(a.Node, b.Node), cmp.Compare(a.Var, b.Var)); c != 0 {
			return c
		}
		switch {
		case a.May == b.May:
			return 0
		case b.May:
			return -1
		}
		return 1
	})
	return slices.CompactFunc(as, func(a, b wire.Access) bool { return a.Node == b.Node && a.Var == b.Var })
}
