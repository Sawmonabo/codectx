package flow

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
}

// Fringe is a handle to a saved fringe on the builder's LIFO stack.
type Fringe int32

// Frame is a handle to an open jump frame (loop, switch, block, catch or
// finally) on the builder's frame stack.
type Frame int32

// reset starts a function spanning fn on arena a: it discards every node,
// variable, fringe, frame, label and pending goto, keeps the scratch lists'
// capacity, creates Entry (span fn) and Exit (span {fn.End, fn.End}) and sets
// the fringe to {Entry}. Arena.Begin calls it after its own reclaim.
func (b *Builder) reset(a *Arena, fn Span) {
	panic("flow: Builder.reset is not implemented")
}

// scratchBytes is the construction scratch in use since reset, in bytes; it
// is what Arena.ScratchBytes reports.
func (b *Builder) scratchBytes() int {
	panic("flow: Builder.scratchBytes is not implemented")
}

// scratchRetained is the capacity, in bytes, the scratch lists hold; it is
// part of Arena.Retained.
func (b *Builder) scratchRetained() int {
	panic("flow: Builder.scratchRetained is not implemented")
}

// release drops every scratch list's backing array. Arena.Begin calls it,
// before reset, when the release rule fires.
func (b *Builder) release() {
	panic("flow: Builder.release is not implemented")
}

// Node creates a node of kind k spanning s, adds an edge from every current
// fringe member to it, and makes the fringe {n}. With an empty fringe the node
// has no predecessor. Every label pending from Label targets it. It returns
// the node id.
func (b *Builder) Node(k Kind, s Span) int32 {
	panic("flow: Builder.Node is not implemented")
}

// Var declares a variable whose declaring identifier spans decl and returns
// its dense id. Scoping is the lowering's: it maps names to ids.
func (b *Builder) Var(decl Span) int32 {
	panic("flow: Builder.Var is not implemented")
}

// Use records that node n reads variable v, before n's own definition is
// written. A repeated use is recorded once.
func (b *Builder) Use(n, v int32) {
	panic("flow: Builder.Use is not implemented")
}

// Def records that node n defines variable v. A node defines at most one
// variable; a second call on the same node panics.
func (b *Builder) Def(n, v int32) {
	panic("flow: Builder.Def is not implemented")
}

// Push saves a copy of the current fringe and returns its handle. The current
// fringe is unchanged.
func (b *Builder) Push() Fringe {
	panic("flow: Builder.Push is not implemented")
}

// Restore makes the current fringe a copy of the saved fringe f.
func (b *Builder) Restore(f Fringe) {
	panic("flow: Builder.Restore is not implemented")
}

// Merge adds every member of the saved fringe f to the current fringe.
func (b *Builder) Merge(f Fringe) {
	panic("flow: Builder.Merge is not implemented")
}

// Pop releases f and every handle pushed after it.
func (b *Builder) Pop(f Fringe) {
	panic("flow: Builder.Pop is not implemented")
}

// Close adds an edge from every current fringe member to node to (a loop's
// back edge, a fallthrough into a known node), then empties the fringe.
func (b *Builder) Close(to int32) {
	panic("flow: Builder.Close is not implemented")
}

// Reachable reports whether the current fringe is non-empty.
func (b *Builder) Reachable() bool {
	panic("flow: Builder.Reachable is not implemented")
}

// OpenLoop opens a loop frame: the target of an unlabelled break or continue,
// and of a labelled one naming any of labels.
func (b *Builder) OpenLoop(labels ...string) Frame {
	panic("flow: Builder.OpenLoop is not implemented")
}

// OpenSwitch opens a switch or select frame: the target of an unlabelled
// break and of a break naming any of labels. Continue passes through it to
// the enclosing loop.
func (b *Builder) OpenSwitch(labels ...string) Frame {
	panic("flow: Builder.OpenSwitch is not implemented")
}

// OpenBlock opens a labelled non-loop statement's frame: only a break naming
// one of labels targets it.
func (b *Builder) OpenBlock(labels ...string) Frame {
	panic("flow: Builder.OpenBlock is not implemented")
}

// ContinueHere adds every continue pending on loop frame f to the current
// fringe. Call it where the continue target begins, so the next node created
// receives them.
func (b *Builder) ContinueHere(f Frame) {
	panic("flow: Builder.ContinueHere is not implemented")
}

// CloseFrame adds every break pending on f to the current fringe and pops f,
// which must be the innermost open frame.
func (b *Builder) CloseFrame(f Frame) {
	panic("flow: Builder.CloseFrame is not implemented")
}

// Break moves the current fringe to the break target and empties it. label ""
// is the innermost loop or switch; otherwise the innermost frame naming
// label. No match increments Unresolved.
func (b *Builder) Break(label string) {
	panic("flow: Builder.Break is not implemented")
}

// Continue moves the current fringe to a loop's continue target and empties
// it. label "" is the innermost loop; otherwise the innermost loop naming
// label. No match increments Unresolved.
func (b *Builder) Continue(label string) {
	panic("flow: Builder.Continue is not implemented")
}

// Return moves the current fringe to Exit and empties it.
func (b *Builder) Return() {
	panic("flow: Builder.Return is not implemented")
}

// Throw moves the current fringe to the innermost open catch frame's handler,
// else to Exit, and empties it.
func (b *Builder) Throw() {
	panic("flow: Builder.Throw is not implemented")
}

// MayThrow gives node n an exceptional edge to the innermost open catch
// frame's handler or finally frame's finally, whichever is inner. Outside
// every such frame it does nothing. The current fringe is unchanged.
func (b *Builder) MayThrow(n int32) {
	panic("flow: Builder.MayThrow is not implemented")
}

// OpenCatch opens a catch frame: the destination of Throw and MayThrow inside
// the try body.
func (b *Builder) OpenCatch() Frame {
	panic("flow: Builder.OpenCatch is not implemented")
}

// EnterHandler makes the current fringe every node that threw into catch
// frame f and pops f, which must be the innermost open frame.
func (b *Builder) EnterHandler(f Frame) {
	panic("flow: Builder.EnterHandler is not implemented")
}

// OpenFinally opens a finally frame that intercepts every jump or throw
// whose destination lies outside it.
func (b *Builder) OpenFinally() Frame {
	panic("flow: Builder.OpenFinally is not implemented")
}

// EnterFinally adds every intercepted source to the current fringe, so the
// finally body's first node receives normal completion and every
// interception. normal reports whether normal completion reached the
// finally, i.e. whether the fringe was non-empty on entry.
func (b *Builder) EnterFinally(f Frame) (normal bool) {
	panic("flow: Builder.EnterFinally is not implemented")
}

// CloseFinally pops finally frame f and re-issues, from the finally body's
// exit fringe, every intercepted break, continue, return and throw, and
// every intercepted MayThrow source as a throw, toward its original
// destination, searching only the frames outside f (an outer finally
// intercepts again). The current fringe becomes that exit fringe if normal,
// else empty.
func (b *Builder) CloseFinally(f Frame, normal bool) {
	panic("flow: Builder.CloseFinally is not implemented")
}

// Label names the next node created as the target of goto name. Labels are
// function-scoped.
func (b *Builder) Label(name string) {
	panic("flow: Builder.Label is not implemented")
}

// Goto moves the current fringe to label name's node and empties it; the
// label may be declared before or after, and resolves at Finish. No such
// label increments Unresolved.
func (b *Builder) Goto(name string) {
	panic("flow: Builder.Goto is not implemented")
}

// Finish first routes the fringe still open at the end of the body to Exit
// (falling off the end returns), then resolves every goto, deduplicates edges and uses, builds the
// successor and predecessor CSRs in the slabs (counted in Arena.Bytes) and
// returns the Graph. Node metadata, uses and variable spans stay in scratch.
// Finish does not add the exit augmentation; PostDominators does.
func (b *Builder) Finish() *Graph {
	panic("flow: Builder.Finish is not implemented")
}
