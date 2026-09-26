package flow

import (
	"slices"
	"strings"
	"unsafe"
)

// Builder constructs one function's Graph. It is language-neutral: every
// lowering drives the same calls, in source order, and never builds edges
// itself. A Builder is obtained from Arena.Begin, is valid until the next
// Begin, and is finished exactly once with Finish.
//
// # Fringe
//
// The builder keeps a current fringe: the set of nodes whose normal
// completion flows to whatever comes next. Begin sets it to {Entry}. Node
// adds an edge from every fringe member to the new node and makes the fringe
// {new node}. An empty fringe means the code being lowered is unreachable;
// its nodes are still created, with no predecessor.
//
// Fringe handles form a LIFO stack. If/else is lowered as
//
//	c := b.Node(Branch, cond)
//	p := b.Push()         // {c}
//	... lower then ...
//	t := b.Push()         // end of then
//	b.Restore(p)          // back to {c}
//	... lower else ...    // absent: the fringe stays {c}
//	b.Merge(t)            // end of else ∪ end of then
//	b.Pop(p)
//
// # Jump frames
//
// Loops, switches and labelled blocks open a Frame, a stack entry that
// collects the jumps targeting it. A loop:
//
//	f := b.OpenLoop(labels...)
//	h := b.Node(Branch, cond)  // loop head
//	... lower body ...
//	b.ContinueHere(f)          // continue target: the post statement, or
//	                           // the head when there is none
//	... lower post, then b.Close(h) ...
//	b.Restore(exitHandle)      // the head's false edge, saved with Push
//	b.CloseFrame(f)            // ∪ every break
//
// Break, Continue, Return and Throw each move the whole current fringe to
// their destination and empty it. A break or continue that names no open
// frame (or no label in scope) increments Unresolved, and its node keeps no
// successor.
//
// # Exceptions
//
// Inside an open catch or finally frame, MayThrow gives a node an exceptional
// edge to the innermost such frame's handler or finally. Every throwing node
// inside a try reaches the handler — a deliberate divergence from the hosted
// engine, which wires only the try body's last statement to its handlers.
// Implicit exceptions of nodes outside every try are not modelled; only an
// explicit Throw reaches Exit from there. try/catch/finally is lowered as
//
//	f := b.OpenFinally()       // only when there is a finally
//	c := b.OpenCatch()         // only when there is a catch
//	... lower the try body: MayThrow(n) on each throwing node,
//	    Throw() after a throw statement's node ...
//	t := b.Push()
//	b.EnterHandler(c)
//	... lower the catch binding and body ...
//	b.Merge(t)
//	b.Pop(t)
//	normal := b.EnterFinally(f)
//	... lower the finally body ...
//	b.CloseFinally(f, normal)
//
// Any break, continue, return or throw whose destination lies outside an open
// finally is intercepted by the innermost such finally and re-issued from
// the finally's exit fringe by CloseFinally.
//
// # Goto
//
// Labels are function-scoped: Label names the next node created, and Goto
// resolves at Finish against labels declared before or after it. A goto to
// no label increments Unresolved.
//
// # Go defer
//
// `defer f(x)` is one Stmt node at the defer statement's position carrying the
// uses of the call's function value and operands, which Go evaluates there.
// The deferred execution itself is not a CFG node; a function literal it
// defers is its own function. recover is not modelled.
//
// # Defects
//
// A call that violates this contract (a second Def on one node, a handle or
// frame used after it was popped, a frame closed out of order) is a lowering
// defect, never a property of the input, and panics.
type Builder struct {
	a *Arena

	// Construction scratch. Every list below is truncated by reset, keeps its
	// capacity across functions, and is dropped by release. Elements hold no
	// pointer except names.
	nodes []node
	// in marks the members of cur, so no fringe holds a node twice.
	in  []bool
	cur []int32
	// edges are the raw (from, to) edges, packed as pack does; Finish sorts
	// and deduplicates them.
	edges []uint64
	// uses are the raw (node, variable) reads, packed as pack does; Finish
	// sorts and deduplicates them into useOff and useVars.
	uses []uint64
	vars []Span
	// saved holds every pushed fringe back to back; savedOff[h] is where
	// handle h starts, and it ends where h+1 starts (or at len(saved)).
	saved    []int32
	savedOff []int32
	frames   []frame
	// items is the store of every node list a frame or goto collects, as
	// singly linked cells; -1 ends a list.
	items []item
	// recs is the store of every finally frame's intercepted jumps.
	recs []rec
	// names holds every frame label and goto label name. It is the one
	// pointer-bearing scratch list: a label is compared by its text. reset
	// clears it so it never keeps a caller's strings alive.
	names  []string
	labels []label
	gotos  []gotoJump
	// pending is the index in labels of the first label waiting for the next
	// node.
	pending int
	useOff  []int32
	useVars []int32

	defs, unresolved int
	finished         bool
	g                Graph
}

// Fringe is a handle to a saved fringe on the builder's LIFO stack.
type Fringe int32

// Frame is a handle to an open jump frame (loop, switch, block, catch or
// finally) on the builder's frame stack.
type Frame int32

// frameKind is what an open frame is the target of.
type frameKind uint8

const (
	loopFrame frameKind = iota
	switchFrame
	blockFrame
	catchFrame
	finallyFrame
)

// jumpKind is the kind of a jump a finally frame intercepts and re-issues.
type jumpKind uint8

const (
	breakJump jumpKind = iota
	continueJump
	throwJump
	returnJump
)

// toExit is the destination frame index of a jump that goes to Exit.
const toExit int32 = -1

// frame is one open jump frame.
type frame struct {
	// lab0, lab1 delimit the frame's labels in names.
	lab0, lab1 int32
	// sources heads the item list of nodes jumping to this frame: breaks for
	// a loop, switch or block, throwers for a catch, intercepted sources for
	// a finally.
	sources int32
	// conts heads a loop's pending continues.
	conts int32
	// recs heads a finally's intercepted jumps in Builder.recs.
	recs    int32
	kind    frameKind
	entered bool
}

// item is one cell of a node list.
type item struct{ node, next int32 }

// rec is one distinct intercepted jump: its kind and destination frame index
// (toExit for Exit).
type rec struct {
	target, next int32
	kind         jumpKind
}

// label binds names[name] to node, -1 until the next node is created.
type label struct{ name, node int32 }

// gotoJump is one Goto: the label name it targets and the item list of the
// nodes it moves.
type gotoJump struct{ name, sources int32 }

// reset starts a function spanning fn on arena a: it discards every node,
// variable, fringe, frame, label and pending goto, keeps the scratch lists'
// capacity, creates Entry (span fn) and Exit (span {fn.End, fn.End}) and sets
// the fringe to {Entry}. Arena.Begin calls it after its own reclaim.
func (b *Builder) reset(a *Arena, fn Span) {
	clear(b.names)
	*b = Builder{
		a:        a,
		nodes:    append(b.nodes[:0], node{span: fn, def: -1, kind: Entry}, node{span: Span{fn.End, fn.End}, def: -1, kind: Exit}),
		in:       append(b.in[:0], false, false),
		cur:      b.cur[:0],
		edges:    b.edges[:0],
		uses:     b.uses[:0],
		vars:     b.vars[:0],
		saved:    b.saved[:0],
		savedOff: b.savedOff[:0],
		frames:   b.frames[:0],
		items:    b.items[:0],
		recs:     b.recs[:0],
		names:    b.names[:0],
		labels:   b.labels[:0],
		gotos:    b.gotos[:0],
		useOff:   b.useOff[:0],
		useVars:  b.useVars[:0],
	}
	b.add(EntryNode)
}

// scratchBytes is the construction scratch in use since reset, in bytes; it
// is what Arena.ScratchBytes reports.
func (b *Builder) scratchBytes() int {
	return b.scratchSize(func(n, _ int) int { return n })
}

// scratchRetained is the capacity, in bytes, the scratch lists hold; it is
// part of Arena.Retained.
func (b *Builder) scratchRetained() int {
	return b.scratchSize(func(_, c int) int { return c })
}

// scratchSize sums, over every scratch list, count(len, cap) elements times
// the element size. A label name counts as its string header; its text is
// the caller's.
func (b *Builder) scratchSize(count func(n, c int) int) int {
	return count(len(b.nodes), cap(b.nodes))*int(unsafe.Sizeof(node{})) +
		count(len(b.in), cap(b.in)) +
		count(len(b.cur), cap(b.cur))*4 +
		count(len(b.edges), cap(b.edges))*8 +
		count(len(b.uses), cap(b.uses))*8 +
		count(len(b.vars), cap(b.vars))*int(unsafe.Sizeof(Span{})) +
		count(len(b.saved), cap(b.saved))*4 +
		count(len(b.savedOff), cap(b.savedOff))*4 +
		count(len(b.frames), cap(b.frames))*int(unsafe.Sizeof(frame{})) +
		count(len(b.items), cap(b.items))*int(unsafe.Sizeof(item{})) +
		count(len(b.recs), cap(b.recs))*int(unsafe.Sizeof(rec{})) +
		count(len(b.names), cap(b.names))*int(unsafe.Sizeof("")) +
		count(len(b.labels), cap(b.labels))*int(unsafe.Sizeof(label{})) +
		count(len(b.gotos), cap(b.gotos))*int(unsafe.Sizeof(gotoJump{})) +
		count(len(b.useOff), cap(b.useOff))*4 +
		count(len(b.useVars), cap(b.useVars))*4
}

// release drops every scratch list's backing array. Arena.Begin calls it,
// before reset, when the release rule fires.
func (b *Builder) release() {
	*b = Builder{}
}

// Node creates a node of kind k spanning s, adds an edge from every current
// fringe member to it, and makes the fringe {n}. With an empty fringe the node
// has no predecessor. Every label pending from Label targets it. It returns
// the node id.
func (b *Builder) Node(k Kind, s Span) int32 {
	if k == Entry || k == Exit {
		panic("flow: Builder.Node given the Entry or Exit kind")
	}
	n := int32(len(b.nodes))
	b.nodes = append(b.nodes, node{span: s, def: -1, kind: k})
	b.in = append(b.in, false)
	for _, m := range b.cur {
		b.edges = append(b.edges, pack(m, n))
	}
	b.clearCur()
	b.add(n)
	for i := b.pending; i < len(b.labels); i++ {
		b.labels[i].node = n
	}
	b.pending = len(b.labels)
	return n
}

// Var declares a variable whose declaring identifier spans decl and returns
// its dense id. Scoping is the lowering's: it maps names to ids.
func (b *Builder) Var(decl Span) int32 {
	b.vars = append(b.vars, decl)
	return int32(len(b.vars) - 1)
}

// Use records that node n reads variable v, before n's own definition is
// written. A repeated use is recorded once.
func (b *Builder) Use(n, v int32) {
	b.checkNode(n)
	b.checkVar(v)
	b.uses = append(b.uses, pack(n, v))
}

// Def records that node n defines variable v. A node defines at most one
// variable; a second call on the same node panics.
func (b *Builder) Def(n, v int32) {
	b.checkNode(n)
	b.checkVar(v)
	if b.nodes[n].def != -1 {
		panic("flow: Builder.Def called twice on one node")
	}
	b.nodes[n].def = v
	b.defs++
}

// Push saves a copy of the current fringe and returns its handle. The current
// fringe is unchanged.
func (b *Builder) Push() Fringe {
	b.savedOff = append(b.savedOff, int32(len(b.saved)))
	b.saved = append(b.saved, b.cur...)
	return Fringe(len(b.savedOff) - 1)
}

// Restore makes the current fringe a copy of the saved fringe f.
func (b *Builder) Restore(f Fringe) {
	s := b.savedFringe(f)
	b.clearCur()
	for _, n := range s {
		b.add(n)
	}
}

// Merge adds every member of the saved fringe f to the current fringe.
func (b *Builder) Merge(f Fringe) {
	for _, n := range b.savedFringe(f) {
		b.add(n)
	}
}

// Pop releases f and every handle pushed after it.
func (b *Builder) Pop(f Fringe) {
	b.savedFringe(f)
	b.saved = b.saved[:b.savedOff[f]]
	b.savedOff = b.savedOff[:f]
}

// Close adds an edge from every current fringe member to node to (a loop's
// back edge, a fallthrough into a known node), then empties the fringe.
func (b *Builder) Close(to int32) {
	b.checkNode(to)
	b.edgesTo(b.cur, to)
	b.clearCur()
}

// Reachable reports whether the current fringe is non-empty.
func (b *Builder) Reachable() bool { return len(b.cur) > 0 }

// OpenLoop opens a loop frame: the target of an unlabelled break or continue,
// and of a labelled one naming any of labels.
func (b *Builder) OpenLoop(labels ...string) Frame {
	return b.open(loopFrame, labels)
}

// OpenSwitch opens a switch or select frame: the target of an unlabelled
// break and of a break naming any of labels. Continue passes through it to
// the enclosing loop.
func (b *Builder) OpenSwitch(labels ...string) Frame {
	return b.open(switchFrame, labels)
}

// OpenBlock opens a labelled non-loop statement's frame: only a break naming
// one of labels targets it.
func (b *Builder) OpenBlock(labels ...string) Frame {
	return b.open(blockFrame, labels)
}

// ContinueHere adds every continue pending on loop frame f to the current
// fringe. Call it where the continue target begins, so the next node created
// receives them. Every loop calls it exactly where its continue target is; a
// continue issued on f after it is a lowering defect CloseFrame reports.
func (b *Builder) ContinueHere(f Frame) {
	if f < 0 || int(f) >= len(b.frames) || b.frames[f].kind != loopFrame {
		panic("flow: Builder.ContinueHere on a frame that is not an open loop")
	}
	fr := &b.frames[f]
	b.addList(fr.conts)
	fr.conts = -1
}

// CloseFrame adds every break pending on f to the current fringe and pops f,
// which must be the innermost open frame.
func (b *Builder) CloseFrame(f Frame) {
	fr := b.innermost(f)
	switch fr.kind {
	case loopFrame:
		if fr.conts != -1 {
			panic("flow: Builder.CloseFrame on a loop with pending continues: ContinueHere was not called, or a continue followed it")
		}
	case switchFrame, blockFrame:
	default:
		panic("flow: Builder.CloseFrame on a catch or finally frame")
	}
	b.addList(fr.sources)
	b.frames = b.frames[:f]
}

// Break moves the current fringe to the break target and empties it. label ""
// is the innermost loop or switch; otherwise the innermost frame naming
// label. No match increments Unresolved.
func (b *Builder) Break(label string) {
	t := toExit - 1
	for i := int32(len(b.frames)) - 1; i >= 0; i-- {
		fr := &b.frames[i]
		if label == "" && (fr.kind == loopFrame || fr.kind == switchFrame) ||
			label != "" && fr.kind != catchFrame && fr.kind != finallyFrame && b.named(fr, label) {
			t = i
			break
		}
	}
	b.jump(breakJump, t)
}

// Continue moves the current fringe to a loop's continue target and empties
// it. label "" is the innermost loop; otherwise the innermost loop naming
// label. No match increments Unresolved.
func (b *Builder) Continue(label string) {
	t := toExit - 1
	for i := int32(len(b.frames)) - 1; i >= 0; i-- {
		fr := &b.frames[i]
		if fr.kind == loopFrame && (label == "" || b.named(fr, label)) {
			t = i
			break
		}
	}
	b.jump(continueJump, t)
}

// Return moves the current fringe to Exit and empties it.
func (b *Builder) Return() {
	b.jump(returnJump, toExit)
}

// Throw moves the current fringe to the innermost open catch frame's handler,
// else to Exit, and empties it.
func (b *Builder) Throw() {
	b.jump(throwJump, b.catchBelow(int32(len(b.frames))))
}

// MayThrow gives node n an exceptional edge to the innermost open catch
// frame's handler or finally frame's finally, whichever is inner. Outside
// every such frame it does nothing. The current fringe is unchanged. A
// finally whose body is being lowered is not a destination: a node in it
// throws to the frames outside.
func (b *Builder) MayThrow(n int32) {
	b.checkNode(n)
	for i := int32(len(b.frames)) - 1; i >= 0; i-- {
		fr := &b.frames[i]
		switch {
		case fr.kind == catchFrame:
			fr.sources = b.push(fr.sources, n)
			return
		case fr.kind == finallyFrame && !fr.entered:
			b.intercept(i, throwJump, b.catchBelow(i), []int32{n})
			return
		}
	}
}

// OpenCatch opens a catch frame: the destination of Throw and MayThrow inside
// the try body.
func (b *Builder) OpenCatch() Frame {
	return b.open(catchFrame, nil)
}

// EnterHandler makes the current fringe every node that threw into catch
// frame f and pops f, which must be the innermost open frame.
func (b *Builder) EnterHandler(f Frame) {
	fr := b.innermost(f)
	if fr.kind != catchFrame {
		panic("flow: Builder.EnterHandler on a frame that is not a catch")
	}
	b.clearCur()
	b.addList(fr.sources)
	b.frames = b.frames[:f]
}

// OpenFinally opens a finally frame that intercepts every jump or throw
// whose destination lies outside it.
func (b *Builder) OpenFinally() Frame {
	return b.open(finallyFrame, nil)
}

// EnterFinally adds every intercepted source to the current fringe, so the
// finally body's first node receives normal completion and every
// interception. normal reports whether normal completion reached the
// finally, i.e. whether the fringe was non-empty on entry. From here until
// CloseFinally the frame intercepts nothing: a jump inside the finally body
// goes to the frames outside it.
func (b *Builder) EnterFinally(f Frame) (normal bool) {
	fr := b.innermost(f)
	if fr.kind != finallyFrame || fr.entered {
		panic("flow: Builder.EnterFinally on a frame that is not an unentered finally")
	}
	fr.entered = true
	normal = len(b.cur) > 0
	b.addList(fr.sources)
	return normal
}

// CloseFinally pops finally frame f and re-issues, from the finally body's
// exit fringe, every intercepted break, continue, return and throw, and
// every intercepted MayThrow source as a throw, toward its original
// destination, searching only the frames outside f (an outer finally
// intercepts again). The current fringe becomes that exit fringe if normal,
// else empty. Each distinct (kind, destination) is re-issued once.
func (b *Builder) CloseFinally(f Frame, normal bool) {
	fr := b.innermost(f)
	if fr.kind != finallyFrame || !fr.entered {
		panic("flow: Builder.CloseFinally on a frame that is not an entered finally")
	}
	r := fr.recs
	b.frames = b.frames[:f]
	for ; r != -1; r = b.recs[r].next {
		b.deliver(b.recs[r].kind, b.recs[r].target, b.cur)
	}
	if !normal {
		b.clearCur()
	}
}

// Label names the next node created as the target of goto name. Labels are
// function-scoped. A label no node follows before Finish names Exit, where
// the rest of the body falls; a name declared twice targets its first
// declaration.
func (b *Builder) Label(name string) {
	b.names = append(b.names, name)
	b.labels = append(b.labels, label{name: int32(len(b.names) - 1), node: -1})
}

// Goto moves the current fringe to label name's node and empties it; the
// label may be declared before or after, and resolves at Finish. No such
// label increments Unresolved.
func (b *Builder) Goto(name string) {
	b.names = append(b.names, name)
	g := gotoJump{name: int32(len(b.names) - 1), sources: -1}
	for _, n := range b.cur {
		g.sources = b.push(g.sources, n)
	}
	b.gotos = append(b.gotos, g)
	b.clearCur()
}

// Finish first routes the fringe still open at the end of the body to Exit
// (falling off the end returns), then resolves every goto, deduplicates edges and uses, builds the
// successor and predecessor CSRs in the slabs (counted in Arena.Bytes) and
// returns the Graph. Node metadata, uses and variable spans stay in scratch.
// Finish does not add the exit augmentation; PostDominators does. Every
// frame must be closed; a second Finish panics.
func (b *Builder) Finish() *Graph {
	if b.finished {
		panic("flow: Builder.Finish called twice")
	}
	if len(b.frames) > 0 {
		panic("flow: Builder.Finish with an open frame")
	}
	b.finished = true
	b.edgesTo(b.cur, ExitNode)
	b.clearCur()
	for i := b.pending; i < len(b.labels); i++ {
		b.labels[i].node = ExitNode
	}
	b.pending = len(b.labels)
	b.resolveGotos()

	n := len(b.nodes)
	slices.Sort(b.edges)
	b.edges = slices.Compact(b.edges)
	succOff, succ := b.a.Int32s(n+1), b.a.Int32s(len(b.edges))
	predOff, pred := b.a.Int32s(n+1), b.a.Int32s(len(b.edges))
	for i, e := range b.edges {
		from, to := unpack(e)
		succ[i] = to
		succOff[from+1]++
		predOff[to+1]++
	}
	prefix(succOff)
	prefix(predOff)
	// Filling in (from, to) order leaves each predecessor list ascending;
	// predOff[to] serves as the cursor and is shifted back afterwards.
	for _, e := range b.edges {
		from, to := unpack(e)
		pred[predOff[to]] = from
		predOff[to]++
	}
	copy(predOff[1:], predOff[:n])
	predOff[0] = 0

	slices.Sort(b.uses)
	b.uses = slices.Compact(b.uses)
	b.useOff = resize(b.useOff, n+1)
	b.useVars = resize(b.useVars, len(b.uses))
	for i, u := range b.uses {
		from, v := unpack(u)
		b.useVars[i] = v
		b.useOff[from+1]++
	}
	prefix(b.useOff)

	b.g = Graph{
		nodes:      b.nodes,
		useOff:     b.useOff,
		uses:       b.useVars,
		succOff:    succOff,
		succ:       succ,
		predOff:    predOff,
		pred:       pred,
		vars:       b.vars,
		defs:       b.defs,
		unresolved: b.unresolved,
	}
	return &b.g
}

// resolveGotos adds an edge from every goto source to its label's node, and
// counts every goto whose label no Label declared.
func (b *Builder) resolveGotos() {
	byName := func(x, y label) int { return strings.Compare(b.names[x.name], b.names[y.name]) }
	slices.SortStableFunc(b.labels, byName)
	for _, g := range b.gotos {
		i, ok := slices.BinarySearchFunc(b.labels, b.names[g.name], func(l label, name string) int {
			return strings.Compare(b.names[l.name], name)
		})
		if !ok {
			b.unresolved++
			continue
		}
		for c := g.sources; c != -1; c = b.items[c].next {
			b.edges = append(b.edges, pack(b.items[c].node, b.labels[i].node))
		}
	}
}

// jump moves the current fringe as a jump of kind k to destination frame t
// (toExit for Exit, below toExit when no destination was found) and empties
// it. An unresolved jump is counted and its sources keep no successor.
func (b *Builder) jump(k jumpKind, t int32) {
	if t < toExit {
		b.unresolved++
	} else {
		b.deliver(k, t, b.cur)
	}
	b.clearCur()
}

// deliver sends sources as a jump of kind k to destination frame t (toExit
// for Exit). The innermost unentered finally between the top of the frame
// stack and t intercepts it; otherwise the sources join t's list for k, or
// get an edge to Exit. No source moves nothing: an unreachable jump, or a
// finally whose body cannot complete, records no interception an outer
// finally would re-issue.
func (b *Builder) deliver(k jumpKind, t int32, sources []int32) {
	if len(sources) == 0 {
		return
	}
	for i := int32(len(b.frames)) - 1; i > t; i-- {
		if fr := &b.frames[i]; fr.kind == finallyFrame && !fr.entered {
			b.intercept(i, k, t, sources)
			return
		}
	}
	if t == toExit {
		b.edgesTo(sources, ExitNode)
		return
	}
	fr := &b.frames[t]
	for _, n := range sources {
		if k == continueJump {
			fr.conts = b.push(fr.conts, n)
		} else {
			fr.sources = b.push(fr.sources, n)
		}
	}
}

// intercept records sources as entering finally frame i and the jump (k, t)
// as one it re-issues at CloseFinally, once per distinct pair.
func (b *Builder) intercept(i int32, k jumpKind, t int32, sources []int32) {
	fr := &b.frames[i]
	for _, n := range sources {
		fr.sources = b.push(fr.sources, n)
	}
	for r := fr.recs; r != -1; r = b.recs[r].next {
		if b.recs[r].kind == k && b.recs[r].target == t {
			return
		}
	}
	b.recs = append(b.recs, rec{target: t, next: fr.recs, kind: k})
	fr.recs = int32(len(b.recs) - 1)
}

// catchBelow is the innermost catch frame below frame index i, or toExit.
func (b *Builder) catchBelow(i int32) int32 {
	for i--; i >= 0; i-- {
		if b.frames[i].kind == catchFrame {
			return i
		}
	}
	return toExit
}

// open pushes a frame of kind k named by labels.
func (b *Builder) open(k frameKind, labels []string) Frame {
	lab0 := int32(len(b.names))
	b.names = append(b.names, labels...)
	b.frames = append(b.frames, frame{lab0: lab0, lab1: int32(len(b.names)), sources: -1, conts: -1, recs: -1, kind: k})
	return Frame(len(b.frames) - 1)
}

// named reports whether frame fr is named by label.
func (b *Builder) named(fr *frame, label string) bool {
	return slices.Contains(b.names[fr.lab0:fr.lab1], label)
}

// innermost returns frame f, which must be the innermost open frame.
func (b *Builder) innermost(f Frame) *frame {
	if int(f) != len(b.frames)-1 {
		panic("flow: frame closed out of order or after it was closed")
	}
	return &b.frames[f]
}

// savedFringe returns the saved fringe f.
func (b *Builder) savedFringe(f Fringe) []int32 {
	if f < 0 || int(f) >= len(b.savedOff) {
		panic("flow: fringe handle used after it was popped")
	}
	end := int32(len(b.saved))
	if int(f)+1 < len(b.savedOff) {
		end = b.savedOff[f+1]
	}
	return b.saved[b.savedOff[f]:end]
}

// add makes n a member of the current fringe unless it already is.
func (b *Builder) add(n int32) {
	if !b.in[n] {
		b.in[n] = true
		b.cur = append(b.cur, n)
	}
}

// addList adds every node of the item list headed at c to the current
// fringe.
func (b *Builder) addList(c int32) {
	for ; c != -1; c = b.items[c].next {
		b.add(b.items[c].node)
	}
}

// clearCur empties the current fringe.
func (b *Builder) clearCur() {
	for _, n := range b.cur {
		b.in[n] = false
	}
	b.cur = b.cur[:0]
}

// push prepends n to the item list headed at head and returns the new head.
func (b *Builder) push(head, n int32) int32 {
	b.items = append(b.items, item{node: n, next: head})
	return int32(len(b.items) - 1)
}

// edgesTo adds an edge from every node of sources to to.
func (b *Builder) edgesTo(sources []int32, to int32) {
	for _, n := range sources {
		b.edges = append(b.edges, pack(n, to))
	}
}

func (b *Builder) checkNode(n int32) {
	if n < 0 || int(n) >= len(b.nodes) {
		panic("flow: node id out of range")
	}
}

func (b *Builder) checkVar(v int32) {
	if v < 0 || int(v) >= len(b.vars) {
		panic("flow: variable id out of range")
	}
}

// unpack decodes a pair encoded by pack.
func unpack(p uint64) (hi, lo int32) { return int32(uint32(p >> 32)), int32(uint32(p)) }

// prefix turns per-node counts stored at off[n+1] into CSR offsets.
func prefix(off []int32) {
	for i := 1; i < len(off); i++ {
		off[i] += off[i-1]
	}
}

// resize returns s resized to n zeroed elements, reusing its capacity.
func resize(s []int32, n int) []int32 {
	s = slices.Grow(s[:0], n)[:n]
	clear(s)
	return s
}
