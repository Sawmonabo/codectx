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
// Inside an open catch or finally frame, MayThrow records a node as throwing
// to the innermost such frame. Every node of a try body that may throw is
// recorded, not only the body's last statement. Implicit exceptions of nodes
// outside every try are not modelled; only an explicit Throw reaches Exit
// from there. try/catch/finally is lowered as
//
//	f := b.OpenFinally()       // only when there is a finally
//	c := b.OpenCatch()         // only when there is a catch
//	... lower the try body: MayThrow(n) on each throwing node,
//	    Throw() after a throw statement's node ...
//	t := b.Push()
//	b.EnterHandler(c, catchKeyword)
//	... lower the catch binding and body ...
//	b.Merge(t)
//	b.Pop(t)
//	normal := b.EnterFinally(f, finallyKeyword)
//	... lower the finally body ...
//	b.CloseFinally(f, normal)
//
// A MayThrow source and a Throw source differ: a throw statement's node
// completed normally and then threw, so its definitions hold where the throw
// lands; a MayThrow source threw part-way, before any definition it makes.
// The builder therefore lands every MayThrow source of a catch or finally on
// one Handler node it creates there (see Handler), and a Throw source joins
// the catch or finally directly. A node that ends the try body normally and
// may also throw reaches a finally both ways: directly, carrying its
// definitions, and through the Handler, carrying its entry values.
//
// Any break, continue, return or throw whose destination lies outside an open
// finally is intercepted by the innermost such finally and re-issued from
// the finally's exit fringe by CloseFinally. So is a goto whose label lies
// outside it (see Goto).
//
// # Goto
//
// Labels are function-scoped: Label creates the label's own node on the
// current fringe, and Goto resolves at Finish against labels declared before
// or after it. A goto to no label increments Unresolved. A goto issued in the
// try part of a finally (its try body and catch handlers) leaves the finally
// unless the first declaration of its label lies in that try part too; every
// label of the try part is declared by EnterFinally, so EnterFinally decides.
// A leaving goto enters the finally there, as an intercepted jump does, and
// CloseFinally re-issues it from the finally's exit fringe as a goto of the
// same label, which an enclosing finally examines in turn. A goto issued in a
// finally body is outside that finally.
//
// # May-definitions
//
// Def is a killing definition: it ends every path it lies on, and a node may
// make several, one per variable. MayDef is a χ: the node reads the
// variable's prior version and defines the next one, which may still hold
// the old value. A use pairs with every killing definition reaching it
// through any number of may-definitions and with the nearest may-definition
// on each path, and the may-defining node pairs with what reaches it by the
// same rule (see DefUse). The lowerings use MayDef only for writes that may
// leave the old value in place. A closure's write to an enclosing variable
// is a may-definition of that variable at the node that creates the
// closure, in every lowering. A write through a field, index or pointer is a
// may-definition of its base variable (p in `*p = 2`), never of the local a
// pointer refers to, which is unknown without points-to analysis. Taking a
// local's address, or borrowing it mutably, is a may-definition of that
// local at the node that evaluates it; the lowerings state the one rule and
// what it gives up. A value a node yields beside a local it assigns is a
// second killing definition, never a may-definition.
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
// A call that violates this contract is a lowering defect, never a property
// of the input, and panics: a node or variable id
// out of range, Node given the Entry, Exit or Handler kind, a frame closed
// out of order, or a Frame handle used after its frame was closed, or a
// Fringe handle used after it was popped (directly or by popping an earlier
// handle). Handles are issued from one increasing sequence per function and
// never reissued within it, so a stale handle is detected even when its
// stack slot has been reused.
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
	// uses, defs and mayDefs are the raw (node, variable) reads, killing
	// definitions and may-definitions, packed as pack does; Finish sorts and
	// deduplicates them into useOff/useVars, defOff/defVars and
	// mayOff/mayVars.
	uses, defs, mayDefs []uint64
	// saved holds every pushed fringe back to back; savedOff[i] is where the
	// i-th live saved fringe starts, and it ends where the next starts (or at
	// len(saved)). savedGen[i] is its handle, ascending along the stack.
	saved    []int32
	savedOff []int32
	savedGen []int32
	frames   []frame
	// items is the store of every node list a frame or goto collects, as
	// singly linked cells; -1 ends a list.
	items []item
	// recs is the store of every finally frame's intercepted jumps.
	recs []rec
	// names holds every frame label and goto label name. It is the one
	// pointer-bearing scratch list: a label is compared by its text. reset
	// clears it so it never keeps a caller's strings alive.
	names           []string
	labels          []label
	gotos           []pendingGoto
	useOff, useVars []int32
	defOff, defVars []int32
	mayOff, mayVars []int32

	// vars is the variable count.
	vars int
	// gen is the last Fringe or Frame handle issued.
	gen int32
	// high is the scratch high-water sampled so far; see scratchHigh.
	high       int
	unresolved int
	finished   bool
	// sorted reports that labels is sorted by name, stably, so each name's
	// labels stay in declaration order; Label clears it.
	sorted bool
	g      Graph
}

// Fringe is a handle to a saved fringe on the builder's LIFO stack. It is
// opaque: valid from the Push that returned it until it is popped.
type Fringe int32

// Frame is a handle to an open jump frame (loop, switch, block, catch or
// finally) on the builder's frame stack. It is opaque: valid from the Open
// call that returned it until the frame is closed.
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
	// gotoJump's rec target is the index in names of the goto's label.
	gotoJump
)

// toExit is the destination frame index of a jump that goes to Exit.
const toExit int32 = -1

// frame is one open jump frame.
type frame struct {
	// lab0, lab1 delimit the frame's labels in names.
	lab0, lab1 int32
	// sources heads the item list of nodes jumping to this frame: breaks for
	// a loop, switch or block, Throw sources for a catch, intercepted jump
	// and Throw sources for a finally.
	sources int32
	// mayThrow heads a catch's or finally's MayThrow sources, which land on
	// the frame's Handler node.
	mayThrow int32
	// conts heads a loop's pending continues.
	conts int32
	// recs heads a finally's intercepted jumps in Builder.recs.
	recs int32
	// gotoAt is the number of gotos issued before the frame opened, and
	// nodeAt the number of nodes: a finally examines the gotos from gotoAt
	// on, and a label whose node is at or past nodeAt was declared inside
	// it.
	gotoAt, nodeAt int32
	// gen is the frame's handle.
	gen     int32
	kind    frameKind
	entered bool
	// suspended hides the frame from Break and Continue (Suspend).
	suspended bool
}

// item is one cell of a node list.
type item struct{ node, next int32 }

// rec is one distinct intercepted jump: its kind and destination frame index
// (toExit for Exit), or for a goto the index in names of its label.
type rec struct {
	target, next int32
	kind         jumpKind
}

// label binds names[name] to its node.
type label struct{ name, node int32 }

// pendingGoto is one Goto: the label name it targets and the item list of
// the nodes it moves. left marks one a finally took over at EnterFinally;
// the finally re-issues it and it resolves nothing itself.
type pendingGoto struct {
	name, sources int32
	left          bool
}

// reset starts a function spanning fn on arena a: it discards every node,
// variable, fringe, frame, label and pending goto, keeps the scratch lists'
// capacity, creates Entry (span fn) and Exit (span {fn.End, fn.End}) and sets
// the fringe to {Entry}. Arena.Begin calls it after its own reclaim, and
// after reading the previous function's scratch high-water.
func (b *Builder) reset(a *Arena, fn Span) {
	clear(b.names)
	*b = Builder{
		a:        a,
		nodes:    append(b.nodes[:0], node{span: fn, kind: Entry}, node{span: Span{fn.End, fn.End}, kind: Exit}),
		in:       append(b.in[:0], false, false),
		cur:      b.cur[:0],
		edges:    b.edges[:0],
		uses:     b.uses[:0],
		defs:     b.defs[:0],
		mayDefs:  b.mayDefs[:0],
		saved:    b.saved[:0],
		savedOff: b.savedOff[:0],
		savedGen: b.savedGen[:0],
		frames:   b.frames[:0],
		items:    b.items[:0],
		recs:     b.recs[:0],
		names:    b.names[:0],
		labels:   b.labels[:0],
		gotos:    b.gotos[:0],
		useOff:   b.useOff[:0],
		useVars:  b.useVars[:0],
		defOff:   b.defOff[:0],
		defVars:  b.defVars[:0],
		mayOff:   b.mayOff[:0],
		mayVars:  b.mayVars[:0],
	}
	b.add(EntryNode)
}

// scratchHigh is the construction scratch high-water since reset, in bytes;
// it is what Arena.ScratchBytes reports.
//
// Only cur, saved/savedOff/savedGen and frames shrink during construction,
// and edges, uses, defs and mayDefs when Finish deduplicates them; every other list
// only grows. mark samples the in-use total immediately before each such
// shrink and at the end of Finish, so between two samples the total only
// grows and the largest sample (or the current total, before Finish) is the
// exact high-water.
func (b *Builder) scratchHigh() int { return max(b.high, b.scratchBytes()) }

// mark raises the high-water to the scratch in use now.
func (b *Builder) mark() { b.high = max(b.high, b.scratchBytes()) }

// scratchBytes is the construction scratch in use now, in bytes.
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
		count(len(b.defs), cap(b.defs))*8 +
		count(len(b.mayDefs), cap(b.mayDefs))*8 +
		count(len(b.saved), cap(b.saved))*4 +
		count(len(b.savedOff), cap(b.savedOff))*4 +
		count(len(b.savedGen), cap(b.savedGen))*4 +
		count(len(b.frames), cap(b.frames))*int(unsafe.Sizeof(frame{})) +
		count(len(b.items), cap(b.items))*int(unsafe.Sizeof(item{})) +
		count(len(b.recs), cap(b.recs))*int(unsafe.Sizeof(rec{})) +
		count(len(b.names), cap(b.names))*int(unsafe.Sizeof("")) +
		count(len(b.labels), cap(b.labels))*int(unsafe.Sizeof(label{})) +
		count(len(b.gotos), cap(b.gotos))*int(unsafe.Sizeof(pendingGoto{})) +
		count(len(b.useOff), cap(b.useOff))*4 +
		count(len(b.useVars), cap(b.useVars))*4 +
		count(len(b.defOff), cap(b.defOff))*4 +
		count(len(b.defVars), cap(b.defVars))*4 +
		count(len(b.mayOff), cap(b.mayOff))*4 +
		count(len(b.mayVars), cap(b.mayVars))*4
}

// release drops every scratch list's backing array. Arena.Begin calls it,
// before reset, when the release rule fires.
func (b *Builder) release() {
	*b = Builder{}
}

// Node creates a node of kind k spanning s, adds an edge from every current
// fringe member to it, and makes the fringe {n}. With an empty fringe the node
// has no predecessor. It returns the node id. Entry, Exit and Handler nodes
// are the builder's own; Node panics on those kinds.
func (b *Builder) Node(k Kind, s Span) int32 {
	if k == Entry || k == Exit || k == Handler {
		panic("flow: Builder.Node given the Entry, Exit or Handler kind")
	}
	n := b.newNode(k, s)
	b.edgesTo(b.cur, n)
	// A fringe of one is replaced by {n} after the node and its edges grew
	// the scratch, so the total does not drop and needs no sample.
	if len(b.cur) > 1 {
		b.mark()
	}
	b.dropCur()
	b.add(n)
	return n
}

// Var declares a variable and returns its dense id. Scoping is the
// lowering's: it maps names to ids.
func (b *Builder) Var() int32 {
	b.vars++
	return int32(b.vars - 1)
}

// Use records that node n reads variable v, before n's own definitions are
// written. A repeated use is recorded once.
func (b *Builder) Use(n, v int32) {
	b.checkNode(n)
	b.checkVar(v)
	b.uses = append(b.uses, pack(n, v))
}

// MayDef records that node n may define variable v WITHOUT killing the
// definitions that reach it: a χ, which reads v's prior version at n and
// defines the next, so a later use sees n and the killing definitions behind
// it (see DefUse). A node may carry several; a repeat is recorded once.
// A MayDef of a variable n Defs is redundant and dropped at Finish, since
// the killing Def wins. n's uses are still read before any of its
// definitions. See the May-definitions section for what the lowerings
// record with it.
func (b *Builder) MayDef(n, v int32) {
	b.checkNode(n)
	b.checkVar(v)
	b.mayDefs = append(b.mayDefs, pack(n, v))
}

// Def records that node n defines variable v, killing every definition of
// v that reaches n. A node may define several variables, each killing: an
// assignment that yields its value defines the local and the result its
// consumer reads. A repeat is recorded once.
func (b *Builder) Def(n, v int32) {
	b.checkNode(n)
	b.checkVar(v)
	b.defs = append(b.defs, pack(n, v))
}

// Push saves a copy of the current fringe and returns its handle. The current
// fringe is unchanged.
func (b *Builder) Push() Fringe {
	b.gen++
	b.savedOff = append(b.savedOff, int32(len(b.saved)))
	b.savedGen = append(b.savedGen, b.gen)
	b.saved = append(b.saved, b.cur...)
	return Fringe(b.gen)
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
	i := b.savedSlot(f)
	b.mark()
	b.saved = b.saved[:b.savedOff[i]]
	b.savedOff = b.savedOff[:i]
	b.savedGen = b.savedGen[:i]
}

// Close adds an edge from every current fringe member to node to (a loop's
// back edge, a fallthrough into a known node), then empties the fringe.
func (b *Builder) Close(to int32) {
	b.checkNode(to)
	b.edgesTo(b.cur, to)
	b.clearCur()
}

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
// receives them. Every loop calls it where its continue target is. A continue
// issued on f after that is still pending at CloseFrame, a lowering defect
// CloseFrame reports: code the language lowers after the continue target but
// outside the body (a for loop's update) runs with f suspended.
func (b *Builder) ContinueHere(f Frame) {
	fr := b.frame(f)
	if fr.kind != loopFrame {
		panic("flow: Builder.ContinueHere on a frame that is not an open loop")
	}
	b.addList(fr.conts)
	fr.conts = -1
}

// Suspend hides the open loop, switch or block frame f from Break and
// Continue until Resume(f), so a jump in code the language places outside
// f's body though it lowers it while f is open -- a loop's condition or
// update holding a statement expression -- targets the frame enclosing f.
func (b *Builder) Suspend(f Frame) { b.suspend(f, true) }

// Resume makes the frame f that Suspend hid a jump target again.
func (b *Builder) Resume(f Frame) { b.suspend(f, false) }

func (b *Builder) suspend(f Frame, on bool) {
	fr := b.frame(f)
	if fr.kind == catchFrame || fr.kind == finallyFrame {
		panic("flow: Builder.Suspend or Resume on a catch or finally frame")
	}
	if fr.suspended == on {
		panic("flow: Builder.Suspend on a suspended frame, or Resume on one that is not")
	}
	fr.suspended = on
}

// frame returns the open frame f.
func (b *Builder) frame(f Frame) *frame {
	i, ok := slices.BinarySearchFunc(b.frames, int32(f), func(fr frame, g int32) int { return int(fr.gen - g) })
	if !ok {
		panic("flow: a Frame handle used after its frame was closed")
	}
	return &b.frames[i]
}

// CloseFrame adds every break pending on f to the current fringe and pops f,
// which must be the innermost open frame.
func (b *Builder) CloseFrame(f Frame) {
	fr, i := b.innermost(f)
	if fr.suspended {
		panic("flow: Builder.CloseFrame on a suspended frame")
	}
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
	b.popFrame(i)
}

// Break moves the current fringe to the break target and empties it. label ""
// is the innermost loop or switch; otherwise the innermost frame naming
// label. A suspended frame is passed over. No match increments Unresolved.
func (b *Builder) Break(label string) {
	t := toExit - 1
	for i := int32(len(b.frames)) - 1; i >= 0; i-- {
		fr := &b.frames[i]
		if fr.suspended {
			continue
		}
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
// label. A suspended frame is passed over. No match increments Unresolved.
func (b *Builder) Continue(label string) {
	t := toExit - 1
	for i := int32(len(b.frames)) - 1; i >= 0; i-- {
		fr := &b.frames[i]
		if fr.kind == loopFrame && !fr.suspended && (label == "" || b.named(fr, label)) {
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

// MayThrow records node n as throwing part-way, before any definition it
// makes, to the innermost open catch or unentered finally frame, whichever
// is inner: at EnterHandler or EnterFinally, n gets an edge to that frame's
// Handler node. A finally so reached re-issues the throw from its exit
// fringe, as it does an intercepted Throw. Outside every such frame MayThrow
// does nothing. The current fringe is unchanged. A finally whose body is
// being lowered is not a destination: a node in it throws to the frames
// outside.
func (b *Builder) MayThrow(n int32) {
	b.checkNode(n)
	for i := int32(len(b.frames)) - 1; i >= 0; i-- {
		fr := &b.frames[i]
		switch {
		case fr.kind == catchFrame:
			fr.mayThrow = b.push(fr.mayThrow, n)
			return
		case fr.kind == finallyFrame && !fr.entered:
			fr.mayThrow = b.push(fr.mayThrow, n)
			b.record(fr, throwJump, b.catchBelow(i))
			return
		}
	}
}

// OpenCatch opens a catch frame: the destination of Throw and MayThrow inside
// the try body.
func (b *Builder) OpenCatch() Frame {
	return b.open(catchFrame, nil)
}

// EnterHandler begins catch frame f's handler and pops f, which must be the
// innermost open frame. When a node reached f by MayThrow, it creates ONE
// Handler node spanning at (the catch keyword) with an edge from each such
// node, and no edge from the current fringe. The current fringe becomes
// that Handler, if any, plus every Throw source of f.
func (b *Builder) EnterHandler(f Frame, at Span) {
	fr, i := b.innermost(f)
	if fr.kind != catchFrame {
		panic("flow: Builder.EnterHandler on a frame that is not a catch")
	}
	h := b.handler(fr.mayThrow, at)
	b.clearCur()
	if h != -1 {
		b.add(h)
	}
	b.addList(fr.sources)
	b.popFrame(i)
}

// OpenFinally opens a finally frame that intercepts every jump or throw
// whose destination lies outside it.
func (b *Builder) OpenFinally() Frame {
	return b.open(finallyFrame, nil)
}

// EnterFinally adds every intercepted jump and Throw source, and the sources
// of every goto that leaves the finally (see Goto), to the current fringe
// and, when a node reached f by MayThrow, ONE Handler node spanning
// at (the finally keyword) with an edge from each such node and none from
// the fringe. The finally body's first node so receives normal completion,
// every interception and the Handler. normal reports whether normal
// completion reached the finally, i.e. whether the fringe was non-empty on
// entry. From here until CloseFinally the frame intercepts nothing: a jump
// inside the finally body goes to the frames outside it.
func (b *Builder) EnterFinally(f Frame, at Span) (normal bool) {
	fr, _ := b.innermost(f)
	if fr.kind != finallyFrame || fr.entered {
		panic("flow: Builder.EnterFinally on a frame that is not an unentered finally")
	}
	fr.entered = true
	normal = len(b.cur) > 0
	b.leaveGotos(fr)
	b.addList(fr.sources)
	if h := b.handler(fr.mayThrow, at); h != -1 {
		b.add(h)
	}
	return normal
}

// Thrown reports whether an exception reached finally frame f, entered and
// not yet closed: an intercepted Throw from a reachable point, or a MayThrow
// source. A lowering whose finally can end the exception (a Python with
// statement whose exit may suppress it) passes normal || Thrown(f) to
// CloseFinally, since execution may then continue after the statement.
func (b *Builder) Thrown(f Frame) bool {
	fr, _ := b.innermost(f)
	if fr.kind != finallyFrame || !fr.entered {
		panic("flow: Builder.Thrown on a frame that is not an entered finally")
	}
	for r := fr.recs; r != -1; r = b.recs[r].next {
		if b.recs[r].kind == throwJump {
			return true
		}
	}
	return false
}

// CloseFinally pops finally frame f and re-issues, from the finally body's
// exit fringe, every intercepted break, continue, return, throw and leaving
// goto, and every intercepted MayThrow source as a throw, toward its original
// destination, searching only the frames outside f (an outer finally
// intercepts again). The current fringe becomes that exit fringe if normal,
// else empty. Each distinct (kind, destination) is re-issued once.
func (b *Builder) CloseFinally(f Frame, normal bool) {
	fr, i := b.innermost(f)
	if fr.kind != finallyFrame || !fr.entered {
		panic("flow: Builder.CloseFinally on a frame that is not an entered finally")
	}
	r := fr.recs
	b.popFrame(i)
	for ; r != -1; r = b.recs[r].next {
		if b.recs[r].kind == gotoJump {
			b.addGoto(b.recs[r].target, b.cur)
			continue
		}
		b.deliver(b.recs[r].kind, b.recs[r].target, b.cur)
	}
	if !normal {
		b.clearCur()
	}
}

// Label creates a Stmt node spanning at (the label identifier) on the
// current fringe exactly as Node does, makes the fringe {it}, and names it
// the target of goto name. It returns the node id. Labels are
// function-scoped; a name declared twice targets its first declaration.
func (b *Builder) Label(name string, at Span) int32 {
	n := b.Node(Stmt, at)
	b.names = append(b.names, name)
	b.labels = append(b.labels, label{name: int32(len(b.names) - 1), node: n})
	b.sorted = false
	return n
}

// Goto moves the current fringe to label name's node and empties it; the
// label may be declared before or after, and resolves at Finish, through
// every finally the goto leaves (see Goto). No such label increments
// Unresolved.
func (b *Builder) Goto(name string) {
	b.names = append(b.names, name)
	b.addGoto(int32(len(b.names)-1), b.cur)
	b.clearCur()
}

// addGoto records a goto of the label names[name] moving sources.
func (b *Builder) addGoto(name int32, sources []int32) {
	g := pendingGoto{name: name, sources: -1}
	for _, n := range sources {
		g.sources = b.push(g.sources, n)
	}
	b.gotos = append(b.gotos, g)
}

// leaveGotos hands finally frame fr, being entered, every reachable goto
// issued in its try part whose label's first declaration does not lie in
// that try part: its sources join fr's, and fr records it for CloseFinally
// to re-issue. A goto an inner finally already took over is skipped; its
// re-issue, a later goto, is examined instead.
func (b *Builder) leaveGotos(fr *frame) {
	for i := int(fr.gotoAt); i < len(b.gotos); i++ {
		g := &b.gotos[i]
		if g.left || g.sources == -1 {
			continue
		}
		if l, ok := b.labelNamed(b.names[g.name]); ok && l.node >= fr.nodeAt {
			continue
		}
		for c := g.sources; c != -1; c = b.items[c].next {
			fr.sources = b.push(fr.sources, b.items[c].node)
		}
		g.left = true
		b.record(fr, gotoJump, g.name)
	}
}

// labelNamed is the first-declared label named name. It sorts labels by name
// first when a Label since the last sort left them unsorted; the sort is
// stable, so each name's labels stay in declaration order across sorts.
func (b *Builder) labelNamed(name string) (label, bool) {
	if !b.sorted {
		slices.SortStableFunc(b.labels, func(x, y label) int { return strings.Compare(b.names[x.name], b.names[y.name]) })
		b.sorted = true
	}
	i, ok := slices.BinarySearchFunc(b.labels, name, func(l label, name string) int {
		return strings.Compare(b.names[l.name], name)
	})
	if !ok {
		return label{}, false
	}
	return b.labels[i], true
}

// Finish first routes the fringe still open at the end of the body to Exit
// (falling off the end returns), then resolves every goto, deduplicates
// edges, uses and may-definitions (dropping a MayDef of the node's own Def),
// builds the successor and predecessor CSRs in the slabs (counted in
// Arena.Bytes) and returns the Graph. Node metadata, uses and
// may-definitions stay in scratch. Finish does not add the exit
// augmentation; PostDominators does. Every frame must be closed; a second
// Finish panics.
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
	b.resolveGotos()

	// Deduplication shrinks edges, uses, defs and mayDefs; nothing in scratch
	// grows until the CSRs below, so one sample here covers all four.
	b.mark()
	n := len(b.nodes)
	slices.Sort(b.edges)
	b.edges = slices.Compact(b.edges)
	slices.Sort(b.uses)
	b.uses = slices.Compact(b.uses)
	slices.Sort(b.defs)
	b.defs = slices.Compact(b.defs)
	slices.Sort(b.mayDefs)
	b.mayDefs = slices.Compact(b.mayDefs)
	// A may-definition of a variable its node also defines is dropped: the
	// killing definition wins. Both lists are sorted by (node, variable), so
	// one merge walk finds every such pair.
	d := 0
	b.mayDefs = slices.DeleteFunc(b.mayDefs, func(p uint64) bool {
		for d < len(b.defs) && b.defs[d] < p {
			d++
		}
		return d < len(b.defs) && b.defs[d] == p
	})
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

	b.useOff, b.useVars = varCSR(b.uses, n, b.useOff, b.useVars)
	b.defOff, b.defVars = varCSR(b.defs, n, b.defOff, b.defVars)
	b.mayOff, b.mayVars = varCSR(b.mayDefs, n, b.mayOff, b.mayVars)
	b.mark()

	b.g = Graph{
		nodes:      b.nodes,
		useOff:     b.useOff,
		uses:       b.useVars,
		defOff:     b.defOff,
		defs:       b.defVars,
		mayOff:     b.mayOff,
		mayDefs:    b.mayVars,
		succOff:    succOff,
		succ:       succ,
		predOff:    predOff,
		pred:       pred,
		vars:       b.vars,
		unresolved: b.unresolved,
	}
	return &b.g
}

// resolveGotos adds an edge from every goto source to its label's node, and
// counts every goto whose label no Label declared. A goto a finally took
// over resolves through its re-issue instead.
func (b *Builder) resolveGotos() {
	for _, g := range b.gotos {
		if g.left {
			continue
		}
		l, ok := b.labelNamed(b.names[g.name])
		if !ok {
			b.unresolved++
			continue
		}
		for c := g.sources; c != -1; c = b.items[c].next {
			b.edges = append(b.edges, pack(b.items[c].node, l.node))
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
// as one it re-issues at CloseFinally.
func (b *Builder) intercept(i int32, k jumpKind, t int32, sources []int32) {
	fr := &b.frames[i]
	for _, n := range sources {
		fr.sources = b.push(fr.sources, n)
	}
	b.record(fr, k, t)
}

// record adds the jump (k, t) to finally frame fr's re-issues, once per
// distinct pair; two gotos of one label text are one pair.
func (b *Builder) record(fr *frame, k jumpKind, t int32) {
	for r := fr.recs; r != -1; r = b.recs[r].next {
		if rc := b.recs[r]; rc.kind == k && (rc.target == t || k == gotoJump && b.names[rc.target] == b.names[t]) {
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
	b.gen++
	lab0 := int32(len(b.names))
	b.names = append(b.names, labels...)
	b.frames = append(b.frames, frame{
		lab0: lab0, lab1: int32(len(b.names)),
		sources: -1, mayThrow: -1, conts: -1, recs: -1,
		gotoAt: int32(len(b.gotos)), nodeAt: int32(len(b.nodes)),
		gen: b.gen, kind: k,
	})
	return Frame(b.gen)
}

// popFrame pops frame index i, the innermost.
func (b *Builder) popFrame(i int32) {
	b.mark()
	b.frames = b.frames[:i]
}

// named reports whether frame fr is named by label.
func (b *Builder) named(fr *frame, label string) bool {
	return slices.Contains(b.names[fr.lab0:fr.lab1], label)
}

// innermost returns frame f, which must be the innermost open frame, and its
// index.
func (b *Builder) innermost(f Frame) (*frame, int32) {
	i := int32(len(b.frames)) - 1
	if i < 0 || b.frames[i].gen != int32(f) {
		panic("flow: frame closed out of order or after it was closed")
	}
	return &b.frames[i], i
}

// savedSlot is the stack index of the saved fringe f.
func (b *Builder) savedSlot(f Fringe) int {
	i, ok := slices.BinarySearch(b.savedGen, int32(f))
	if !ok {
		panic("flow: fringe handle used after it was popped")
	}
	return i
}

// savedFringe returns the saved fringe f.
func (b *Builder) savedFringe(f Fringe) []int32 {
	i := b.savedSlot(f)
	end := int32(len(b.saved))
	if i+1 < len(b.savedOff) {
		end = b.savedOff[i+1]
	}
	return b.saved[b.savedOff[i]:end]
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

// clearCur empties the current fringe, sampling the high-water first.
func (b *Builder) clearCur() {
	if len(b.cur) > 0 {
		b.mark()
	}
	b.dropCur()
}

// dropCur empties the current fringe without sampling.
func (b *Builder) dropCur() {
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

// newNode appends a node of kind k spanning s, with no edge and outside the
// fringe, and returns its id.
func (b *Builder) newNode(k Kind, s Span) int32 {
	n := int32(len(b.nodes))
	b.nodes = append(b.nodes, node{span: s, kind: k})
	b.in = append(b.in, false)
	return n
}

// handler creates the Handler node spanning at with an edge from every node
// of the item list headed at c, or returns -1 when the list is empty. It
// takes no edge from the current fringe.
func (b *Builder) handler(c int32, at Span) int32 {
	if c == -1 {
		return -1
	}
	h := b.newNode(Handler, at)
	for ; c != -1; c = b.items[c].next {
		b.edges = append(b.edges, pack(b.items[c].node, h))
	}
	return h
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
	if v < 0 || int(v) >= b.vars {
		panic("flow: variable id out of range")
	}
}

// varCSR builds the per-node variable CSR of the sorted, deduplicated
// (node, variable) pairs into off (n+1 offsets) and vals, reusing their
// capacity.
func varCSR(pairs []uint64, n int, off, vals []int32) ([]int32, []int32) {
	off = resize(off, n+1)
	vals = resize(vals, len(pairs))
	for i, p := range pairs {
		from, v := unpack(p)
		vals[i] = v
		off[from+1]++
	}
	prefix(off)
	return off, vals
}

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
