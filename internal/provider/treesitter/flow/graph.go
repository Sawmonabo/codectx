package flow

// Span is a source byte range [Start, End).
type Span struct{ Start, End uint32 }

// Kind classifies a CFG node.
type Kind uint8

const (
	// Entry is node EntryNode; its span is the function's.
	Entry Kind = iota
	// Exit is node ExitNode, the synthetic exit; its span is empty at the
	// function's end.
	Exit
	// Stmt is a node with at most one normal successor: an expression, a
	// declaration, an assignment, a call.
	Stmt
	// Branch may choose among its successors: a condition, a loop head, a
	// switch or select head, a short-circuit operand, a conditional operator,
	// an optional chain.
	Branch
	// Jump transfers control away from the fall-through: return, break,
	// continue, goto, throw, panic, fallthrough.
	Jump
	// Handler is the landing of exceptional edges, created by the Builder at
	// a catch or finally (Builder.EnterHandler, Builder.EnterFinally). Every
	// edge into it is exceptional: its predecessor threw before any
	// definition it makes, so data flow into a Handler takes each
	// predecessor's ENTRY values, never its Def or MayDefs. It defines and
	// uses nothing.
	Handler
)

// Fixed node ids every Graph carries.
const (
	EntryNode int32 = 0
	ExitNode  int32 = 1
)

// node is one node's metadata, written by Builder into construction scratch.
type node struct {
	span Span
	// def is the variable the node defines, -1 if none.
	def  int32
	kind Kind
}

// Graph is a read-only view of one function's control-flow graph and its
// per-node def/use record. It is produced by Builder.Finish and is valid
// until the next Arena.Begin.
//
// Layout: nodes, useOff/uses and mayOff/mayDefs live in construction
// scratch; the two CSRs (succOff/succ, predOff/pred) live in the slabs
// Arena.Bytes counts. For node n, its uses are uses[useOff[n]:useOff[n+1]],
// its may-definitions mayDefs[mayOff[n]:mayOff[n+1]], its successors
// succ[succOff[n]:succOff[n+1]] and its predecessors
// pred[predOff[n]:predOff[n+1]]; each list is sorted ascending with no
// duplicates.
type Graph struct {
	nodes                  []node
	useOff, uses           []int32
	mayOff, mayDefs        []int32
	succOff, succ          []int32
	predOff, pred          []int32
	vars, defs, unresolved int
}

// Len is the node count N, Entry and Exit included.
func (g *Graph) Len() int { return len(g.nodes) }

// Kind is node n's kind.
func (g *Graph) Kind(n int32) Kind { return g.nodes[n].kind }

// Span is node n's source range. Entry spans the function; Exit is the empty
// range at the function's end.
func (g *Graph) Span(n int32) Span { return g.nodes[n].span }

// Def is the one variable node n defines, or -1. A multi-assignment or a
// destructuring lowers to one node per target, so one is always enough.
func (g *Graph) Def(n int32) int32 { return g.nodes[n].def }

// Uses are the distinct variables node n reads, ascending. Every use is read
// BEFORE n's own definitions (its Def and its MayDefs) are written, so
// `x = x + 1` uses the previous x.
func (g *Graph) Uses(n int32) []int32 { return window(g.uses, g.useOff, n) }

// MayDefs are the distinct variables node n may define without killing the
// definitions that reach it, ascending; see Builder.MayDef. It never holds
// Def(n).
func (g *Graph) MayDefs(n int32) []int32 { return window(g.mayDefs, g.mayOff, n) }

// Succ are node n's successors, ascending, without duplicates; a self-loop
// is allowed. It is the graph as lowered: no exit augmentation.
func (g *Graph) Succ(n int32) []int32 { return window(g.succ, g.succOff, n) }

// Pred are node n's predecessors, ascending, without duplicates; the exact
// reverse of Succ.
func (g *Graph) Pred(n int32) []int32 { return window(g.pred, g.predOff, n) }

// Vars is the variable count; variable ids are dense in [0, Vars()).
func (g *Graph) Vars() int { return g.vars }

// Defs is the definition count D, the number of nodes whose Def is not -1.
// D ≤ N structurally. May-definitions are not counted.
func (g *Graph) Defs() int { return g.defs }

// Unresolved counts jumps whose target was not found (a break, continue or
// goto naming no open frame or label). Such a node keeps no successor; the
// count makes the loss visible rather than silent.
func (g *Graph) Unresolved() int { return g.unresolved }

// window returns list[off[n]:off[n+1]] with its capacity capped, so an append
// by a caller can never write into the next node's entries.
func window(list, off []int32, n int32) []int32 {
	lo, hi := off[n], off[n+1]
	return list[lo:hi:hi]
}

// Edges is an arena-backed list of (from, to) node pairs, sorted ascending by
// (from, to), without duplicates. It is valid until the next Arena.Begin.
//
// Each pair is packed as uint64(uint32(from))<<32 | uint64(uint32(to)), so
// the natural uint64 order is the (from, to) order.
type Edges struct{ pairs []uint64 }

// Len is the pair count.
func (e Edges) Len() int { return len(e.pairs) }

// At is the i-th pair.
func (e Edges) At(i int) (from, to int32) {
	return unpack(e.pairs[i])
}

// pack encodes one pair in the Edges order.
func pack(from, to int32) uint64 { return uint64(uint32(from))<<32 | uint64(uint32(to)) }

// unpack decodes a pair encoded by pack.
func unpack(p uint64) (hi, lo int32) { return int32(uint32(p >> 32)), int32(uint32(p)) }
