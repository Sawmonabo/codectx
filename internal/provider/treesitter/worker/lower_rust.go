package worker

import (
	"sync"

	ts "github.com/tree-sitter/go-tree-sitter"

	"github.com/Sawmonabo/codectx/internal/provider/treesitter/flow"
)

// rustLowering lowers Rust functions and methods, closures, and async and gen
// blocks: a block whose body runs only when the future or generator it makes
// is polled or iterated is its own function, as a closure is.
var rustLowering = Lowering{
	language:  "rust",
	callables: []string{"function_item", "closure_expression", "async_block", "gen_block"},
	lower:     lowerRust,
}

// rsTryLabel names the frame of a try block. No Rust label can contain a
// space, so the innermost-named-frame search for it always finds the
// innermost try block, whatever loops lie between.
const rsTryLabel = " try"

// lowerRust lowers one Rust callable's parameters and body into b.
//
// # Node granularity
//
// The goldens render nodes by source text, so the granularity is exact:
//
//   - Every name a parameter pattern binds is one defining node after Entry,
//     spanning the name; `self` is a variable, defined by a node spanning the
//     `self` token of the receiver.
//   - A statement is one node: an expression statement, or a block's tail
//     expression, spans the expression without its `;`; a tail is a node
//     even when it reads nothing, as a literal `2` is. A let declaration
//     spans the declaration. An assignment or compound assignment is its own
//     defining node, spanning it; return, break and continue are Jump nodes
//     spanning the expression. A macro invocation is one node spanning it,
//     read as Macro invocations states: a Stmt node, or a Branch when a jump
//     in its token tree can leave it. Its expansion is not lowered, so a
//     panicking macro (panic!, unreachable!, todo!, assert!) is a plain node
//     that falls through, as a call does.
//   - Values travel through variables (see Lowering). A node Uses what its
//     own evaluation reads. Folded into the node consuming them, so that
//     their reads are that node's own: identifiers, calls, field, index, unary, borrow,
//     range, tuple, array, struct and cast expressions, literals, await and
//     yield, and a macro invocation no jump can leave. Lowered to nodes of
//     their own, each handing its value to its consumer through a result
//     variable the lowering owns, which every node yielding the value
//     defines and the consumer Uses: an if (each arm's tail node), a match
//     (each arm's result node), a block, unsafe or const block (its tail
//     node), a loop (each valued break), a labelled block (its tail node and
//     each valued break leaving it), a try block (its tail node and each `?`
//     completing it), `e?` (its node, whose value is the Ok value), `&&` and
//     `||` (each operand node, on the path where it decides the value), a
//     closure, async or gen block (its creating node, whose value is the
//     created callable), and a macro invocation a jump can leave (its
//     node). An if, match, loop, while, for or block makes no node spanning
//     itself, in any position: its conditions, heads, statements and arm
//     results are its nodes. A while or for loop, an assignment, a compound
//     assignment and a jump yield `()` or `!`, so they hand over nothing. flow
//     gives a node one definition, so a yielding node already defining
//     another variable (the `?` completing a try block, which defines its own
//     Ok value) may-defines the result: the result is defined only by its
//     construct's yields, each reaching the consumer on its own path, so the
//     may-definition adds no pair a definition would not. A jump in a macro's
//     token tree defines no result, since which tokens make the jumped value
//     is known only to the expansion.
//   - `e?` is a Branch node spanning the whole `e?`, after e's nodes, whose
//     successors are Exit (inside a try block: the end of that block) and the
//     rest of the enclosing expression, so everything after it depends on it.
//   - `&&` and `||`: the left operand is a Branch node spanning it, the right
//     operand, evaluated only on the other path, a Stmt node spanning it when
//     it is folded. The left operand's node defines the operator's result and
//     the right operand's defines it again, so a node spanning the operator
//     (a compound left operand, `a && b` in `a && b || c`, or the condition)
//     Uses the result, never the operands' names.
//   - An if condition, a while condition and a match guard are one Branch
//     node spanning the condition. A let chain (`a && let P = v && b`) is one
//     Branch per member in source order; each member's false edge leaves the
//     chain. A let condition `let P = v` is a Branch spanning it, which Uses
//     v's reads and defines an owned variable holding v, followed on the
//     taken path by one defining node per name P binds, spanning the name and
//     Using that variable (so D ≤ N). A let chain's bindings are in scope for
//     its later members and the body only.
//   - `loop` has a Stmt head spanning the `loop` keyword and no exit edge:
//     only a break leaves it. A while loop's back edge targets the first node
//     its condition makes. A for loop follows Iteration (see Lowering; The
//     Rust Reference, Expressions › Loops and other breakable expressions ›
//     Iterator loops: `IntoIterator::into_iter` is called once): a node for
//     the iterated expression, Using its reads and defining an iteration
//     variable of the lowering's own (the node yielding its value when that
//     node spans it, as a `?` does, whose result is then that variable); a
//     Branch head spanning the `for` keyword that Uses that variable and
//     nothing else; then one defining node per name the pattern binds, each
//     Using that variable and nothing else, so the head and the names depend
//     on the iterated value as it was before the loop, never on a write to
//     its variables in the body.
//   - A match evaluates its scrutinee once, at a node of its own (a Stmt node
//     spanning it, or the node yielding its value when that spans it) that
//     defines an owned variable; then its arms in source order: a Branch node
//     spanning the arm's pattern, then one defining node per name the pattern
//     binds, then a Branch for the guard, then the arm's result. The pattern
//     Branch and every binding node Use the scrutinee's variable, never the
//     names it was computed from; the guard and the result read the arm's
//     bindings in the arm's scope. A failed pattern or guard goes to the next
//     arm's pattern; no arm falls into another. A match has no jump frame, so
//     a break inside an arm leaves the enclosing loop. Exhaustiveness is not
//     checked here, so the last arm keeps its false edge, out of the match,
//     unless its pattern is irrefutable by syntax alone: the wildcard `_` (in
//     any arm, which also makes every later arm unreachable, so they lower to
//     nothing) or, in the last arm, a bare identifier, which is then a Stmt
//     node defining the name. A bare identifier in an earlier arm may name a
//     constant or a unit variant (`None`): by The Rust Reference, Patterns ›
//     Identifier patterns, a path pattern takes precedence, which only name
//     resolution decides. So it is a Branch that also defines the name it
//     would bind, a variable of the arm that is unread when the name is a
//     variant.
//   - A labelled block `'a: { … }` opens a block frame named by its label;
//     `break 'a v` leaves it, its node defining the block's result.
//   - A block's tail is its last statement when that is an expression, or an
//     expression statement without its `;` (the grammar makes a trailing
//     `if c { a } else { b }` one).
//   - `let P = v else { … };` is a Branch node spanning the declaration that
//     Uses v's reads and defines an owned variable holding v; the else block,
//     which the language requires to diverge, is lowered on its false edge
//     and ended by a return, so a block that ends in a plain panicking-macro
//     node never falls into the bindings; that return is an edge to Exit and
//     makes no node. The taken path is one defining node per name, each Using
//     the owned variable. Any other let with an initializer is one node
//     spanning the declaration that defines a bare identifier pattern, or,
//     for a destructuring pattern, defines an owned variable holding the
//     value, followed by one defining node per bound name Using it. `let x;`
//     declares x and makes no node. A destructuring assignment is likewise a
//     node for the assignment, defining an owned variable, then one node per
//     target, each Using it (a place target also Uses its own operands); its
//     assignees are those of the Reference's "Destructuring assignments": the
//     elements of a tuple `(a, b) = e` or array `[a, b] = e`, the arguments
//     of a tuple struct `P(a, b) = e`, and the fields of a struct `S { x, f:
//     y } = e` (the shorthand's name, the named field's value), never a rest
//     `..`, the path or a field name.
//   - A nested callable (closure, async or gen block) is its own function;
//     its creating expression is one Stmt node spanning it, which Uses every
//     enclosing variable referenced inside it, resolved with its own scopes
//     so a name it declares shadows, and may-defines every enclosing variable
//     it assigns (directly or through a field, index or dereference), since
//     when it runs is unknown. A `move` closure, async or gen block writes
//     its own copies, by Lowering's rule: its creating node keeps the reads
//     and may-defines nothing.
//   - A try block `try { … }` (the unstable `try_blocks` feature of The Rust
//     Unstable Book; The Rust Reference does not define it) opens a frame
//     named rsTryLabel; a `?` inside it breaks to the block's end. unsafe and
//     const blocks are inline blocks.
//   - A write through a field, index or dereference target (`x.f = e`,
//     `a[i] += e`, `*p = e`) Uses the target's operands and is a non-killing
//     may-definition of its base variable, found through field, index and
//     `*` only (`-x` and `!x` are values, no place). The address-taking rule
//     of Lowering applies as stated there, and nothing else is a
//     definition: a mutable borrow `&mut x` (`&mut x.f`, `&mut a[i]`) Uses
//     and may-defines x's base variable at the node evaluating it, the first
//     node created after it whose span holds it; a `ref mut` binding in a
//     pattern may-defines the base variable of the matched place (x in
//     `if let Some(ref mut y) = x`) at the binding's node. What that rule
//     gives up stays given up, for its stated reasons: a write through a
//     reference as a definition of the local it refers to (the variable
//     written through is may-defined, as above), a method call's implicit
//     borrow of its receiver (`v.push(1)`), whose borrowing depends on the method's signature, and
//     a shared borrow `&x` of an interior-mutable type, whose writing
//     depends on the type.
//
// # Statement and expression kinds
//
// Handled: let_declaration, expression_statement, macro_invocation, a
// block's tail expression; if_expression, let_condition, let_chain,
// else_clause, match_expression, while_expression, loop_expression,
// for_expression, block (labelled or not), unsafe_block, const_block,
// try_block, try_expression, return_expression, break_expression,
// continue_expression, assignment_expression, compound_assignment_expr,
// binary_expression (`&&`, `||`), closure_expression, async_block, gen_block.
// Plain (reads recorded, falls through): await_expression, yield_expression,
// call_expression, field_expression, index_expression, unary_expression,
// reference_expression (a `&mut` one also may-defines its base),
// range_expression, tuple_expression, array_expression, struct_expression,
// type_cast_expression, generic_function,
// parenthesized_expression, identifier, self, scoped_identifier and
// metavariable (never a read), literals, unit_expression, and any kind not
// named here, an error node included. No node, only shadowing:
// function_item, const_item, static_item and struct_item bind their name to
// no variable from the start of their block (items are not ordered);
// associated_type, attribute_item, empty_statement, enum_item,
// extern_crate_declaration, foreign_mod_item, function_signature_item,
// impl_item, inner_attribute_item, macro_definition, mod_item, trait_item,
// type_item, union_item and use_declaration make no node.
//
// # Uses
//
// Only an identifier or `self` resolving to a variable declared in this
// function is a Use; a path (`a::b`), a field name, a type and a label never
// are, in a macro's token tree too (see Macro invocations). A node defining a
// variable Uses only what its own evaluation reads: in `(a, b) = (b, a)` the
// target nodes Use the assignment's owned variable, never the a and b the
// assignment read.
//
// # Exceptions
//
// Rust has no try/catch: a panic unwinds out of the function and is not
// modelled, as the seed does outside a try, so no node is MayThrow.
//
// # Scoping
//
// A block is a scope; `let` declares a new variable for each name its
// pattern binds after its initializer is read, so a shadowing `let` is a new
// variable; a match arm, an if-let consequence, a while-let body and a for
// body scope the names their pattern binds. A nested function item cannot
// capture locals: it creates no node in the enclosing function and its name
// shadows as a non-variable. Every identifier in a pattern binds, except the
// path of a tuple-struct or struct pattern.
//
// # Macro invocations
//
// A macro's expansion is not known here, so its token tree (The Rust
// Reference, Macros › Macro invocation) is read as the Rust tokens it holds,
// nested trees included:
//
//   - An identifier or `self` resolving to a variable is a Use, except a name
//     after `.`, `::` or a label's `'`, or before `::`, `!`, `:` or a single
//     `=`: a field or method name, a path segment, a label, a macro's name, a
//     field or binding name, and a named argument or assignment target.
//   - `&` or `&&` then `mut` borrows the base variable of the name after it
//     (`&mut x`, `&mut x.f`, `&mut *x`), by the address-taking rule of
//     Lowering: a may-definition on the node that evaluates the invocation.
//   - Every string literal that is not a byte or C string is read as a format
//     string, by the std::fmt library documentation (Named parameters, Width,
//     Precision): `{x}`, `{x:…}` and a `name$` width or precision capture the
//     variable they name, unless the same tree level passes a named argument
//     of that name; `{{` is a literal brace and a numbered argument captures
//     nothing. Which macros format is not known, so a literal a macro does not
//     format is read too.
//   - A `?` after an operand (The try propagation expression), a return, and a
//     break or continue can leave the invocation. The invocation is then a
//     Branch node, Using the tree's reads, whose successors are its
//     fall-through and each such jump's target, as a `?` is lowered. A jump
//     stays inside the tree when the tree itself holds its target: nothing
//     leaves a closure (a `|…|` or `||` after a non-operand, running to the
//     next `,` or `;` of its level), the `{…}` tree after `fn`, or an async or
//     gen block; an unlabelled break or continue does not leave the `{…}` tree
//     after `loop`, `while` or `for`, a labelled one does not leave the tree
//     that declares its label, and a `?` does not leave a try block.
//   - A name the tree binds (a closure parameter, a `let` or other pattern in
//     it) is not scoped: a later token of that name still resolves to the
//     enclosing variable, which over-approximates the tree's reads.
func lowerRust(l *Lowering, b *flow.Builder, fn *ts.Node, src []byte, s *Scratch) {
	r := &s.rs
	r.reset(l, b, fn, src, s)
	defer r.detach()
	k := r.k
	switch fn.KindId() {
	case k.functionItem, k.closureExpression:
		if ps := fn.ChildByFieldId(k.fParameters); ps != nil {
			r.params(ps)
		}
		r.body(fn.ChildByFieldId(k.fBody))
	default:
		r.body(r.firstKid(fn))
	}
}

// rsLower is the state of lowering one Rust callable. A worker's Scratch
// holds one, which reset readies for every callable it lowers.
type rsLower struct {
	l   *Lowering
	b   *flow.Builder
	src []byte
	k   *rsSyntax
	cur *ts.TreeCursor
	// buf is a stack of child lists; kids pushes one and done pops it.
	buf []ts.Node
	// binds is the scope chain, innermost last.
	binds *scope
	// shadow is non-zero while walking a nested callable for its captures:
	// declarations then bind -1 and no node is created.
	shadow int
	// reads are the variables read by the expression being lowered and not
	// yet carried by a node, in evaluation order; a node Uses a window of
	// them and the window is dropped once the node is made.
	reads []int32
	// frames are the open loops, labelled blocks and try blocks, innermost
	// last, each with the result variable its valued breaks define.
	frames []rsFrame
	// writes are the enclosing variables assigned inside the nested callable
	// whose captures are being collected.
	writes []int32
	// first is the first node created since the last open, or -1.
	first int32
	// last is the node created last, or -1, lastSpan its span, lastDef
	// whether it defines a variable, and lastVar that variable.
	last     int32
	lastSpan flow.Span
	lastDef  bool
	lastVar  int32
	// borrows are the base variables of the `&mut` expressions evaluated
	// and not yet attached to a node, each with the start of its span.
	borrows []rsBorrow
	// hs holds the saved false edges of the conditions and match arms being
	// lowered; ends holds the saved ends of the open matches' arms.
	hs, ends []flow.Fringe
	// tries counts the open try blocks.
	tries int
	// matched is the base variable of the place the pattern being bound
	// matches (x in `let P = x.f`), or -1: a `ref mut` binding in it
	// mutably borrows that variable.
	matched int32
}

// reset readies r, the worker's Rust state, to lower fn: every scalar is set
// anew and every list truncated in place, keeping its capacity. The lists
// hold no pointer into the Go heap (a parse-tree node refers to the tree's
// own memory), so truncation leaves nothing reachable from the file.
func (r *rsLower) reset(l *Lowering, b *flow.Builder, fn *ts.Node, src []byte, s *Scratch) {
	r.l, r.b, r.src, r.k, r.cur, r.binds = l, b, src, rsSyntaxOf(), s.cursor(fn), &s.scope
	r.buf, r.reads, r.writes, r.borrows = r.buf[:0], r.reads[:0], r.writes[:0], r.borrows[:0]
	r.hs, r.ends, r.frames = r.hs[:0], r.ends[:0], r.frames[:0]
	r.shadow, r.tries = 0, 0
	r.first, r.last, r.lastSpan, r.lastDef, r.lastVar, r.matched = -1, -1, flow.Span{}, false, -1, -1
}

// detach drops r's references to the function just lowered, so the
// Scratch holds neither its source nor its builder until the next one.
func (r *rsLower) detach() { r.l, r.b, r.src, r.cur, r.binds = nil, nil, nil, nil, nil }

// kids pushes n's named, non-extra children (comments are extras) onto buf
// and returns the stack mark and the list; done(mark) pops them. A list stays
// valid across nested kids calls: later pushes never overwrite it.
func (r *rsLower) kids(n *ts.Node) (int, []ts.Node) { return r.children(n, true) }

// toks is kids for every child, named or not: the tokens of a token tree.
func (r *rsLower) toks(n *ts.Node) (int, []ts.Node) { return r.children(n, false) }

// children pushes n's non-extra children onto buf, only the named ones when
// named is set, and returns the stack mark and the list.
func (r *rsLower) children(n *ts.Node, named bool) (int, []ts.Node) {
	start := len(r.buf)
	c := r.cur
	c.Reset(*n)
	if c.GotoFirstChild() {
		for {
			if x := c.Node(); (x.IsNamed() || !named) && !x.IsExtra() {
				r.buf = append(r.buf, *x)
			}
			if !c.GotoNextSibling() {
				break
			}
		}
	}
	return start, r.buf[start:]
}

func (r *rsLower) done(mark int) { r.buf = r.buf[:mark] }

func (r *rsLower) text(n *ts.Node) []byte { return textOf(r.src, n) }

// declare binds name in the innermost scope: a new variable, or -1 inside a
// nested callable.
func (r *rsLower) declare(name *ts.Node) int32 {
	v := int32(-1)
	if r.shadow == 0 {
		v = r.b.Var()
	}
	r.binds.push(r.text(name), v)
	return v
}

// lookup resolves name through the scope chain: its variable, or -1.
func (r *rsLower) lookup(name *ts.Node) int32 { return r.binds.lookup(r.text(name)) }

// read records a read of v, unless v is -1.
func (r *rsLower) read(v int32) {
	if v >= 0 {
		r.reads = append(r.reads, v)
	}
}

// rsFrame is one open loop, labelled block or try block: its label's bytes
// in the source (from == to when it has none; a try block answers to
// rsTryLabel, which no source label equals), whether it is a loop, and the
// result variable a valued break leaving it defines, or -1 when its value is
// not consumed or it has none (a while or for loop).
type rsFrame struct {
	from, to uint32
	loop     bool
	dst      int32
}

// openFrame opens the frame of a loop (loop set) or block named by label
// node lab, which is nil when it has none, whose valued breaks define dst.
func (r *rsLower) openFrame(lab *ts.Node, loop bool, dst int32) {
	f := rsFrame{loop: loop, dst: dst}
	if lab != nil {
		f.from, f.to = uint32(lab.StartByte()), uint32(lab.EndByte())
	}
	r.frames = append(r.frames, f)
}

// closeFrame closes the innermost frame.
func (r *rsLower) closeFrame() { r.frames = r.frames[:len(r.frames)-1] }

// target is the result variable of the frame a jump naming label leaves to
// (the innermost loop when label is "", the innermost try block when it is
// rsTryLabel, otherwise the innermost frame so labelled), or -1.
func (r *rsLower) target(label string) int32 {
	for i := len(r.frames) - 1; i >= 0; i-- {
		f := r.frames[i]
		var name string
		if f.to > f.from {
			name = view(r.src[f.from:f.to])
		} else if !f.loop {
			name = rsTryLabel
		}
		if label == "" && f.loop || label != "" && name == label {
			return f.dst
		}
	}
	return -1
}

// rsBorrow is one pending mutable borrow: the variable it may write and
// where its expression starts.
type rsBorrow struct {
	v  int32
	at uint32
}

// node creates a node spanning s that Uses reads[from:to] and may-defines
// every pending mutable borrow inside s.
func (r *rsLower) node(kind flow.Kind, s flow.Span, from, to int) int32 {
	id := r.b.Node(kind, s)
	if r.first < 0 {
		r.first = id
	}
	for _, v := range r.reads[from:to] {
		r.b.Use(id, v)
	}
	kept := r.borrows[:0]
	for _, w := range r.borrows {
		if w.at >= s.Start && w.at < s.End {
			r.b.MayDef(id, w.v)
		} else {
			kept = append(kept, w)
		}
	}
	r.borrows = kept
	r.last, r.lastSpan, r.lastDef = id, s, false
	return id
}

// def records that node n defines v, unless v is -1.
func (r *rsLower) def(n, v int32) {
	if v < 0 {
		return
	}
	r.b.Def(n, v)
	if n == r.last {
		r.lastDef, r.lastVar = true, v
	}
}

// yield makes node n, the node made last, which yields a construct's value,
// define the construct's result variable v (nothing when v is -1). flow
// gives a node one definition, so a node already defining another variable
// (the `?` that also completes a try block) may-defines v: v is defined only
// by its construct's yields, each of which reaches the consumer on its own
// path, so the may-definition adds no pair a definition would not.
func (r *rsLower) yield(n, v int32) {
	switch {
	case v < 0:
	case n == r.last && r.lastDef:
		r.b.MayDef(n, v)
	default:
		r.def(n, v)
	}
}

// open starts tracking the first node created; close returns it (-1 if none)
// and restores the enclosing tracking.
func (r *rsLower) open() int32 {
	s := r.first
	r.first = -1
	return s
}

func (r *rsLower) close(saved int32) int32 {
	h := r.first
	if saved >= 0 {
		r.first = saved
	}
	return h
}

// spans reports whether the last node, made after node last, spans n.
func (r *rsLower) spans(last int32, n *ts.Node) bool {
	return r.last != last && r.lastSpan == spanOf(r.l.unparen(n))
}

// labelOf is the label of a loop, block, break or continue, or "".
func (r *rsLower) labelOf(n *ts.Node) string {
	if c := r.labelNode(n); c != nil {
		return view(r.text(c))
	}
	return ""
}

// labelNode is the label node of a loop, block, break or continue, or nil.
func (r *rsLower) labelNode(n *ts.Node) *ts.Node {
	if c := r.firstKid(n); c != nil && c.KindId() == r.k.label {
		return c
	}
	return nil
}

// token is n's child of kind id, named or not, or nil, found in one walk of
// the reused cursor.
func (r *rsLower) token(n *ts.Node, id uint16) *ts.Node {
	c := r.cur
	c.Reset(*n)
	for ok := c.GotoFirstChild(); ok; ok = c.GotoNextSibling() {
		if x := c.Node(); x.KindId() == id {
			return x
		}
	}
	return nil
}

// firstKid is n's first named child that is not an extra (a comment), or
// nil, found in one walk of the reused cursor.
func (r *rsLower) firstKid(n *ts.Node) *ts.Node {
	c := r.cur
	c.Reset(*n)
	for ok := c.GotoFirstChild(); ok; ok = c.GotoNextSibling() {
		if x := c.Node(); x.IsNamed() && !x.IsExtra() {
			return x
		}
	}
	return nil
}

// params defines every name a parameter list binds, in declaration order.
func (r *rsLower) params(ps *ts.Node) {
	k := r.k
	start, list := r.kids(ps)
	for i := range list {
		switch p := &list[i]; p.KindId() {
		case k.parameter:
			r.pattern(p.ChildByFieldId(k.fPattern), 0, 0, true)
		case k.selfParameter:
			if s := r.token(p, k.self); s != nil {
				r.pattern(s, 0, 0, true)
			}
		case k.attributeItem, k.variadicParameter:
		default:
			// A closure parameter without a type is a bare pattern; a
			// parameter list's bare type (an anonymous parameter) binds
			// nothing, since a type is no pattern kind the walk binds.
			if ps.KindId() == k.closureParameters {
				r.pattern(p, 0, 0, true)
			}
		}
	}
	r.done(start)
}

// body lowers a callable's body: a block, or a closure's expression.
func (r *rsLower) body(n *ts.Node) {
	switch {
	case n == nil || !n.IsNamed():
	case n.KindId() == r.k.block:
		r.block(n, -1)
	default:
		r.into(n, -1)
	}
}

// pattern binds every name pattern p binds, in source order. With nodes and
// outside a nested callable, each name is one Stmt node spanning it that
// defines it and Uses reads[from:to]. An or-pattern binds the names of its
// first alternative, since every alternative binds the same ones, in the
// same binding modes.
func (r *rsLower) pattern(p *ts.Node, from, to int, nodes bool) {
	if p == nil {
		return
	}
	k := r.k
	switch p.KindId() {
	case k.identifier, k.shorthandFieldIdentifier, k.self:
		r.bindName(p, from, to, nodes, false)
	case k.fieldPattern:
		if q := p.ChildByFieldId(k.fPattern); q != nil {
			r.pattern(q, from, to, nodes)
		} else if name := p.ChildByFieldId(k.fName); name != nil {
			// The shorthand `ref mut x` of a struct pattern.
			borrow := r.token(p, k.refKw) != nil && r.token(p, k.mutableSpecifier) != nil
			r.bindName(name, from, to, nodes, borrow)
		}
	case k.orPattern:
		if q := r.firstKid(p); q != nil {
			r.pattern(q, from, to, nodes)
		}
	case k.refPattern:
		if q := r.firstKid(p); q != nil && q.KindId() == k.mutPattern {
			r.refMut(q, from, to, nodes)
		} else {
			r.pattern(q, from, to, nodes)
		}
	case k.tupleStructPattern, k.structPattern, k.capturedPattern, k.mutPattern, k.referencePattern,
		k.tuplePattern, k.slicePattern:
		typ := p.ChildByFieldId(k.fType)
		start, list := r.kids(p)
		for i := range list {
			if typ == nil || list[i].Id() != typ.Id() {
				r.pattern(&list[i], from, to, nodes)
			}
		}
		r.done(start)
	}
}

// bindName binds the name n, one Stmt node as pattern says. A borrow binding
// is a `ref mut` one: it mutably borrows the matched place, so, by the
// address-taking rule of Lowering, its node may-defines that place's base
// variable, matched; inside a nested callable that variable is one of the
// callable's writes.
func (r *rsLower) bindName(n *ts.Node, from, to int, nodes, borrow bool) {
	v := r.declare(n)
	borrow = borrow && r.matched >= 0
	switch {
	case r.shadow > 0:
		if borrow {
			r.writes = append(r.writes, r.matched)
		}
	case nodes:
		id := r.node(flow.Stmt, spanOf(n), from, to)
		r.def(id, v)
		if borrow {
			r.b.MayDef(id, r.matched)
		}
	}
}

// refMut binds mp, the `mut x` or `mut x @ q` of a `ref mut` identifier
// pattern (Reference "Identifier patterns"): x is the `ref mut` binding, and
// q's names bind as q says.
func (r *rsLower) refMut(mp *ts.Node, from, to int, nodes bool) {
	k := r.k
	start, list := r.kids(mp)
	for i := range list {
		switch c := &list[i]; c.KindId() {
		case k.mutableSpecifier:
		case k.identifier:
			r.bindName(c, from, to, nodes, true)
		case k.capturedPattern:
			cs, sub := r.kids(c)
			for j := range sub {
				if j == 0 && sub[j].KindId() == k.identifier {
					r.bindName(&sub[j], from, to, nodes, true)
				} else {
					r.pattern(&sub[j], from, to, nodes)
				}
			}
			r.done(cs)
		default:
			r.pattern(c, from, to, nodes)
		}
	}
	r.done(start)
}

// patternOf binds p as pattern does, where p matches a place whose base
// variable is place, or a value when place is -1: the variable p's `ref mut`
// bindings borrow. The caller resolves place before p binds anything, so a
// name p rebinds still names the matched variable.
func (r *rsLower) patternOf(p *ts.Node, from, to int, nodes bool, place int32) {
	saved := r.matched
	r.matched = place
	r.pattern(p, from, to, nodes)
	r.matched = saved
}

// predeclare binds the value name an item declares, from its block's start.
func (r *rsLower) predeclare(n *ts.Node) {
	k := r.k
	switch n.KindId() {
	case k.functionItem, k.constItem, k.staticItem, k.structItem:
		if name := n.ChildByFieldId(k.fName); name != nil {
			r.binds.push(r.text(name), -1)
		}
	}
}

// block lowers a block in its own scope, its tail defining dst; a labelled
// block is a block frame whose valued breaks define dst too.
func (r *rsLower) block(n *ts.Node, dst int32) {
	if n == nil {
		return
	}
	k := r.k
	mark := r.binds.mark()
	lab := r.labelOf(n)
	start, list := r.kids(n)
	for i := range list {
		r.predeclare(&list[i])
	}
	var f flow.Frame
	if lab != "" {
		f = r.b.OpenBlock(lab)
		r.openFrame(r.labelNode(n), false, dst)
	}
	for i := range list {
		if c := &list[i]; c.KindId() != k.label {
			d := int32(-1)
			if i == len(list)-1 && r.yields(c) {
				d = dst
			}
			r.stmt(c, d)
		}
	}
	if lab != "" {
		r.closeFrame()
		r.b.CloseFrame(f)
	}
	r.done(start)
	r.binds.truncate(mark)
}

// yields reports whether statement c, the last of its block, is the block's
// tail, whose value is the block's: an expression, or an expression
// statement without its `;` (the grammar makes a trailing `if c { a } else
// { b }` one).
func (r *rsLower) yields(c *ts.Node) bool {
	k := r.k
	switch id := c.KindId(); {
	case id == k.letDeclaration:
		return false
	case id == k.expressionStatement:
		return r.token(c, k.semi) == nil
	case int(id) < len(k.item) && k.item[id]:
		return false
	}
	return true
}

// stmt lowers one statement of a block, or its tail expression, whose value
// defines dst.
func (r *rsLower) stmt(n *ts.Node, dst int32) {
	k := r.k
	id := n.KindId()
	if int(id) < len(k.item) && k.item[id] {
		return
	}
	m := len(r.reads)
	switch id {
	case k.expressionStatement:
		r.into(r.firstKid(n), dst)
	case k.letDeclaration:
		r.let(n)
	default:
		r.into(n, dst)
	}
	r.reads = r.reads[:m]
}

// into lowers e so that every node yielding its value defines dst (-1 when
// the value is not consumed): a construct lowered to nodes passes dst to its
// yields, and a folded expression is one Stmt node spanning it, Using its
// reads, that defines dst. A while or for loop, an assignment and a jump
// yield no value.
func (r *rsLower) into(e *ts.Node, dst int32) {
	if e == nil {
		return
	}
	k := r.k
	u := r.l.unparen(e)
	if r.l.isCallable(u) {
		r.yield(r.closure(u), dst)
		return
	}
	switch u.KindId() {
	case k.ifExpression:
		r.ifExpr(u, dst)
	case k.matchExpression:
		r.matchExpr(u, dst)
	case k.loopExpression:
		r.loopExpr(u, dst)
	case k.whileExpression:
		r.whileExpr(u)
	case k.forExpression:
		r.forExpr(u)
	case k.block:
		r.block(u, dst)
	case k.unsafeBlock:
		r.block(r.firstKid(u), dst)
	case k.constBlock:
		r.block(u.ChildByFieldId(k.fBody), dst)
	case k.tryBlock:
		f := r.b.OpenBlock(rsTryLabel)
		r.openFrame(nil, false, dst)
		r.tries++
		r.block(r.firstKid(u), dst)
		r.tries--
		r.closeFrame()
		r.b.CloseFrame(f)
	case k.assignmentExpression:
		r.assign(u)
	case k.compoundAssignmentExpr:
		r.compound(u)
	case k.returnExpression, k.breakExpression, k.continueExpression:
		r.jump(u)
	case k.tryExpression:
		r.try(u, dst)
	case k.macroInvocation:
		m := len(r.reads)
		id := r.macro(u)
		if id < 0 {
			id = r.node(flow.Stmt, spanOf(u), m, len(r.reads))
			r.reads = r.reads[:m]
		}
		r.yield(id, dst)
	case k.binaryExpression:
		if op := u.ChildByFieldId(k.fOperator).KindId(); op == k.and || op == k.or {
			r.lazy(u, dst)
			return
		}
		fallthrough
	default:
		m := len(r.reads)
		r.value(u)
		r.yield(r.node(flow.Stmt, spanOf(u), m, len(r.reads)), dst)
		r.reads = r.reads[:m]
	}
}

// valueVar lowers n, a value evaluated once and used by later nodes, at a
// node of its own that defines an owned variable, and returns it: the node
// that yields n's value when it spans n (a `?`, a closure), otherwise a Stmt
// node spanning n that Uses n's reads.
func (r *rsLower) valueVar(n *ts.Node) int32 {
	m, last := len(r.reads), r.last
	r.value(n)
	if len(r.reads) == m+1 && r.spans(last, n) && r.lastDef && r.lastVar == r.reads[m] {
		v := r.reads[m]
		r.reads = r.reads[:m]
		return v
	}
	id := r.node(flow.Stmt, spanOf(r.l.unparen(n)), m, len(r.reads))
	r.reads = r.reads[:m]
	v := r.b.Var()
	r.def(id, v)
	return v
}

// value lowers an operand evaluated for its value. A folded operand's reads
// are recorded for the node consuming it; a construct lowered to nodes of its
// own hands its value over through a result variable the lowering owns,
// which its yields define and which is recorded as the consumer's read.
func (r *rsLower) value(n *ts.Node) {
	if n == nil {
		return
	}
	k := r.k
	if r.l.isCallable(n) {
		v := r.b.Var()
		r.def(r.closure(n), v)
		r.read(v)
		return
	}
	switch n.KindId() {
	case k.identifier, k.self:
		r.read(r.lookup(n))
	case k.whileExpression, k.forExpression, k.assignmentExpression, k.compoundAssignmentExpr,
		k.returnExpression, k.breakExpression, k.continueExpression:
		r.into(n, -1)
	case k.ifExpression, k.matchExpression, k.loopExpression, k.block, k.unsafeBlock, k.constBlock,
		k.tryBlock, k.tryExpression:
		v := r.b.Var()
		r.into(n, v)
		r.read(v)
	case k.binaryExpression:
		if op := n.ChildByFieldId(k.fOperator).KindId(); op == k.and || op == k.or {
			v := r.b.Var()
			r.lazy(n, v)
			r.read(v)
			return
		}
		r.value(n.ChildByFieldId(k.fLeft))
		r.value(n.ChildByFieldId(k.fRight))
	case k.macroInvocation:
		if id := r.macro(n); id >= 0 {
			v := r.b.Var()
			r.def(id, v)
			r.read(v)
		}
	case k.typeCastExpression:
		r.value(n.ChildByFieldId(k.fValue))
	case k.genericFunction:
		r.value(n.ChildByFieldId(k.fFunction))
	case k.structExpression:
		r.value(n.ChildByFieldId(k.fBody))
	case k.referenceExpression:
		v := n.ChildByFieldId(k.fValue)
		r.value(v)
		if r.token(n, k.mutableSpecifier) != nil {
			if b := r.baseVar(v); b >= 0 {
				r.borrows = append(r.borrows, rsBorrow{v: b, at: uint32(n.StartByte())})
			}
		}
	case k.scopedIdentifier, k.label:
	default:
		start, list := r.kids(n)
		for i := range list {
			r.value(&list[i])
		}
		r.done(start)
	}
}

// try lowers `e?`: a Branch node spanning it, after e's nodes, Using e's
// reads and defining dst, the Ok value; its successors are Exit (inside a
// try block: the end of that block, whose result it defines too) and the
// rest of the enclosing expression.
func (r *rsLower) try(n *ts.Node, dst int32) {
	m := len(r.reads)
	if e := r.firstKid(n); e != nil {
		r.value(e)
	}
	id := r.node(flow.Branch, spanOf(n), m, len(r.reads))
	r.reads = r.reads[:m]
	r.yield(id, dst)
	p := r.b.Push()
	if r.tries > 0 {
		r.yield(id, r.target(rsTryLabel))
		r.b.Break(rsTryLabel)
	} else {
		r.b.Return()
	}
	r.b.Restore(p)
	r.b.Pop(p)
}

// lazy lowers `a && b` or `a || b`, whose value defines dst: the left
// operand is a Branch node spanning it (or the node its own lowering made
// that spans it), which defines dst on the path where it decides the value;
// the right operand, evaluated only on the other path, defines dst again.
func (r *rsLower) lazy(n *ts.Node, dst int32) {
	left, right := n.ChildByFieldId(r.k.fLeft), n.ChildByFieldId(r.k.fRight)
	m, last := len(r.reads), r.last
	r.value(left)
	id := r.last
	if !r.spans(last, left) {
		id = r.node(flow.Branch, spanOf(r.l.unparen(left)), m, len(r.reads))
	}
	r.reads = r.reads[:m]
	r.yield(id, dst)
	p := r.b.Push()
	r.into(right, dst)
	r.b.Merge(p)
	r.b.Pop(p)
}

// The jumps a token-tree region lets leave the macro invocation it is in.
const (
	rsEscTry    = 1 << iota // a `?`
	rsEscReturn             // a return
	rsEscLoop               // an unlabelled break or continue
	rsEscLabel              // a labelled break or continue
	rsEscAll    = rsEscTry | rsEscReturn | rsEscLoop | rsEscLabel
)

// macro lowers macro invocation n's token tree. When no jump in it can
// leave the invocation, it is folded: its reads and mutable borrows are
// recorded for the consuming node and macro returns -1. Otherwise macro
// creates the Branch node spanning n that Uses the tree's reads, issues every
// such jump from it, leaves the fringe on its fall-through, and returns the
// node. Inside a nested callable being walked for its captures nothing
// jumps: the jumps are the callable's own.
func (r *rsLower) macro(n *ts.Node) int32 {
	t := r.token(n, r.k.tokenTree)
	if t == nil {
		return -1
	}
	esc := rsEscAll
	if r.shadow > 0 {
		esc = 0
	}
	m := len(r.reads)
	if !r.tokens(t, esc, false, 0) {
		return -1
	}
	id := r.node(flow.Branch, spanOf(n), m, len(r.reads))
	r.reads = r.reads[:m]
	from := r.b.Push()
	r.tokens(t, esc, true, from)
	r.b.Restore(from)
	r.b.Pop(from)
	return id
}

// tokens walks token tree t, whose tokens may make the jumps in esc leave the
// invocation, and reports whether one of them does. Without emit it records
// the tree's reads, mutable borrows and format captures; with emit it records
// nothing and issues each jump that leaves from the saved fringe from.
func (r *rsLower) tokens(t *ts.Node, esc int, emit bool, from flow.Fringe) bool {
	k := r.k
	jumps, lits := false, false
	// own is what this level's tokens may make leave: esc, or nothing within
	// a closure. next is what the next `{…}` tree's tokens may: a loop, fn,
	// async, gen or try head before it narrows own.
	own, next := esc, esc
	// label is a label `'a:` declares, for the loop or block after it.
	var label []byte
	params, closure := false, false
	start, list := r.toks(t)
	for i := range list {
		c := &list[i]
		id := c.KindId()
		var prev, after *ts.Node
		if i > 0 {
			prev = &list[i-1]
		}
		if i+1 < len(list) {
			after = &list[i+1]
		}
		switch {
		case params:
			// A closure's parameters bind; none is a read.
			params = id != k.pipe
		case id == k.tokenTree:
			sub := own
			if r.src[c.StartByte()] == '{' {
				sub = own & next
				switch {
				case prev != nil && (prev.KindId() == k.asyncKw || prev.KindId() == k.genKw):
					sub = 0
				case prev != nil && prev.KindId() == k.identifier && r.word(prev, "move") && i > 1 &&
					(list[i-2].KindId() == k.asyncKw || list[i-2].KindId() == k.genKw):
					sub = 0
				case prev != nil && prev.KindId() == k.identifier && r.word(prev, "try"):
					sub &^= rsEscTry
				}
				next = own
			}
			m := r.binds.mark()
			if label != nil && r.src[c.StartByte()] == '{' {
				r.binds.push(label, -1)
				label = nil
			}
			if r.tokens(c, sub, emit, from) {
				jumps = true
			}
			r.binds.truncate(m)
		case id == k.pipe || id == k.or:
			if prev == nil || !r.operand(prev) {
				closure, params, own, next = true, id == k.pipe, 0, 0
			}
		case id == k.comma || id == k.semi:
			if closure {
				closure, own = false, esc
			}
			next, label = own, nil
		case id == k.loopKw || id == k.whileKw || id == k.forKw:
			next = own &^ rsEscLoop
		case id == k.fnKw:
			next = 0
		case id == k.quote:
			if after != nil && after.KindId() == k.identifier && i+2 < len(list) && list[i+2].KindId() == k.colon {
				label = r.src[c.StartByte():after.EndByte()]
			}
		case id == k.question || id == k.returnKw || id == k.breakKw || id == k.continueKw:
			lab, ok := r.leaves(list, i, own)
			if !ok {
				break
			}
			jumps = true
			if !emit {
				break
			}
			r.b.Restore(from)
			switch id {
			case k.question:
				if r.tries > 0 {
					r.b.Break(rsTryLabel)
				} else {
					r.b.Return()
				}
			case k.returnKw:
				r.b.Return()
			case k.breakKw:
				r.b.Break(view(lab))
			default:
				r.b.Continue(view(lab))
			}
		case emit:
		case id == k.identifier || id == k.self:
			if !r.named(prev, after) {
				r.read(r.lookup(c))
			}
		case id == k.mutableSpecifier:
			if prev != nil && (prev.KindId() == k.amp || prev.KindId() == k.and) {
				r.tokenBorrow(list[i+1:], prev)
			}
		case id == k.stringLiteral || id == k.rawStringLiteral:
			lits = true
		}
	}
	if lits {
		r.formats(list)
	}
	r.done(start)
	return jumps
}

// leaves reports whether jump token list[i] (`?`, return, break or continue)
// leaves the invocation, given own, the jumps its region lets leave, and
// returns the label a break or continue names, or nil. A `?` is the operator
// only after an operand (a bound `?Sized` follows `:` or `+`); a labelled
// jump stays in the tree when the tree declares its label.
func (r *rsLower) leaves(list []ts.Node, i int, own int) ([]byte, bool) {
	k := r.k
	switch list[i].KindId() {
	case k.question:
		return nil, own&rsEscTry != 0 && i > 0 && r.operand(&list[i-1])
	case k.returnKw:
		return nil, own&rsEscReturn != 0
	}
	if i+2 < len(list) && list[i+1].KindId() == k.quote && list[i+2].KindId() == k.identifier {
		lab := r.src[list[i+1].StartByte():list[i+2].EndByte()]
		return lab, own&rsEscLabel != 0 && r.binds.innermost(lab) < 0
	}
	return nil, own&rsEscLoop != 0
}

// operand reports whether token c ends an operand, so that a `?` after it is
// the question mark operator and a `|` or `||` after it is binary.
func (r *rsLower) operand(c *ts.Node) bool {
	k := r.k
	switch id := c.KindId(); {
	case id == k.question || id == k.awaitKw:
		return true
	case !c.IsNamed() || id == k.mutableSpecifier:
		return false
	case id == k.identifier:
		return !r.word(c, "move")
	}
	return true
}

// word reports whether token c's text is w.
func (r *rsLower) word(c *ts.Node, w string) bool { return string(r.text(c)) == w }

// named reports whether the name between tokens prev and after is a name
// rather than a read: a field or method name after `.`, a path segment after
// or before `::`, a label after `'`, a macro's name before `!`, a field or
// binding name before `:`, or a named argument or assignment target before a
// single `=`.
func (r *rsLower) named(prev, after *ts.Node) bool {
	k := r.k
	if prev != nil {
		if id := prev.KindId(); id == k.dot || id == k.pathSep || id == k.quote {
			return true
		}
	}
	if after != nil {
		if id := after.KindId(); id == k.pathSep || id == k.bang || id == k.colon || id == k.eq {
			return true
		}
	}
	return false
}

// tokenBorrow records the mutable borrow `&mut` (amp is its `&` or `&&`)
// makes of the base variable of the tokens rest after it (x in `&mut x`,
// `&mut x.f`, `&mut *x`): a pending may-definition at amp's position, or,
// inside a nested callable, one of its writes.
func (r *rsLower) tokenBorrow(rest []ts.Node, amp *ts.Node) {
	k := r.k
	j := 0
	for j < len(rest) && rest[j].KindId() == k.deref {
		j++
	}
	if j == len(rest) || rest[j].KindId() != k.identifier && rest[j].KindId() != k.self ||
		j+1 < len(rest) && rest[j+1].KindId() == k.pathSep {
		return
	}
	v := r.lookup(&rest[j])
	switch {
	case v < 0:
	case r.shadow > 0:
		r.writes = append(r.writes, v)
	default:
		r.borrows = append(r.borrows, rsBorrow{v: v, at: uint32(amp.StartByte())})
	}
}

// formats reads the variables the format strings of one token-tree level
// capture: every string literal that is not a byte or C string. A name the
// level passes as a named argument (`x = …`) is that argument, not a capture.
func (r *rsLower) formats(list []ts.Node) {
	k := r.k
	m := r.binds.mark()
	for i := range list {
		if list[i].KindId() == k.identifier && i+1 < len(list) && list[i+1].KindId() == k.eq {
			r.binds.push(r.text(&list[i]), -1)
		}
	}
	for i := range list {
		lit := &list[i]
		if id := lit.KindId(); id != k.stringLiteral && id != k.rawStringLiteral {
			continue
		}
		if b := r.src[lit.StartByte()]; b == 'b' || b == 'c' {
			continue
		}
		start, parts := r.kids(lit)
		for j := range parts {
			if parts[j].KindId() == k.stringContent {
				r.captures(r.text(&parts[j]))
			}
		}
		r.done(start)
	}
	r.binds.truncate(m)
}

// captures reads the variables format string text s captures: the argument
// of `{x}` or `{x:…}`, and every `name$` width or precision in a format
// spec. `{{` is a literal brace; a numbered argument captures nothing.
func (r *rsLower) captures(s []byte) {
	for i := 0; i < len(s); i++ {
		if s[i] != '{' {
			continue
		}
		if i+1 < len(s) && s[i+1] == '{' {
			i++
			continue
		}
		e := rsNameEnd(s, i+1)
		if e > i+1 && (e == len(s) || s[e] == '}' || s[e] == ':') && string(s[i+1:e]) != "self" {
			r.read(r.binds.lookup(s[i+1 : e]))
		}
		for i = e; i < len(s) && s[i] != '}'; {
			n := rsNameEnd(s, i)
			if n == i {
				i++
				continue
			}
			if n < len(s) && s[n] == '$' {
				r.read(r.binds.lookup(s[i:n]))
			}
			i = n
		}
	}
}

// rsNameEnd is the end of the identifier starting at s[i], or i when none
// does: a letter, `_` or a non-ASCII byte, then those or digits.
func rsNameEnd(s []byte, i int) int {
	if i >= len(s) || !rsNameByte(s[i]) {
		return i
	}
	for i++; i < len(s) && (rsNameByte(s[i]) || '0' <= s[i] && s[i] <= '9'); i++ {
	}
	return i
}

// rsNameByte reports whether b may start an identifier.
func rsNameByte(b byte) bool {
	return b == '_' || 'a' <= b && b <= 'z' || 'A' <= b && b <= 'Z' || b >= 0x80
}

// closure creates the node spanning n, a nested callable: it Uses n's
// captures and may-defines every enclosing variable n assigns. Its value, the
// created callable, reaches a consumer through the result variable the
// caller makes it define.
func (r *rsLower) closure(n *ts.Node) int32 {
	m, w := len(r.reads), len(r.writes)
	r.capFunction(n)
	id := r.node(flow.Stmt, spanOf(n), m, len(r.reads))
	for _, v := range r.writes[w:] {
		r.b.MayDef(id, v)
	}
	r.writes, r.reads = r.writes[:w], r.reads[:m]
	return id
}

// cond lowers an if or while condition or a match guard, leaving the fringe
// on the taken path, and returns the index in hs where its false edges begin.
func (r *rsLower) cond(c *ts.Node) int {
	mark := len(r.hs)
	if c == nil {
		// An error-recovered tree without a condition: both edges leave.
		r.hs = append(r.hs, r.b.Push())
		return mark
	}
	if c.KindId() == r.k.letChain {
		start, list := r.kids(c)
		for i := range list {
			r.condPart(&list[i])
		}
		r.done(start)
		return mark
	}
	r.condPart(c)
	return mark
}

// condPart lowers one condition or let-chain member as a Branch node and
// saves its false edge in hs. A let condition's Branch evaluates its value
// once and defines an owned variable its bindings Use.
func (r *rsLower) condPart(c *ts.Node) {
	k := r.k
	m, last := len(r.reads), r.last
	if c.KindId() == k.letCondition {
		val := c.ChildByFieldId(k.fValue)
		r.value(val)
		place := r.baseVar(val)
		id := r.node(flow.Branch, spanOf(c), m, len(r.reads))
		r.reads = r.reads[:m]
		s := r.b.Var()
		r.def(id, s)
		r.hs = append(r.hs, r.b.Push())
		r.bindFrom(c.ChildByFieldId(k.fPattern), s, place)
		return
	}
	r.value(c)
	if !r.spans(last, c) {
		r.node(flow.Branch, spanOf(c), m, len(r.reads))
	}
	r.reads = r.reads[:m]
	r.hs = append(r.hs, r.b.Push())
}

// bindFrom binds pattern p, which matches the value in variable s (a place
// whose base variable is place, or -1): one defining node per name, each
// Using s.
func (r *rsLower) bindFrom(p *ts.Node, s, place int32) {
	m := len(r.reads)
	r.reads = append(r.reads, s)
	r.patternOf(p, m, m+1, true, place)
	r.reads = r.reads[:m]
}

// falses makes the fringe the union of the false edges hs[mark:].
func (r *rsLower) falses(mark int) {
	r.b.Restore(r.hs[mark])
	for _, h := range r.hs[mark+1:] {
		r.b.Merge(h)
	}
}

// release pops the false edges hs[mark:] and every handle pushed after them.
func (r *rsLower) release(mark int) {
	r.b.Pop(r.hs[mark])
	r.hs = r.hs[:mark]
}

func (r *rsLower) ifExpr(n *ts.Node, dst int32) {
	k := r.k
	mark := r.binds.mark()
	fm := r.cond(n.ChildByFieldId(k.fCondition))
	r.block(n.ChildByFieldId(k.fConsequence), dst)
	r.binds.truncate(mark)
	t := r.b.Push()
	r.falses(fm)
	if alt := n.ChildByFieldId(k.fAlternative); alt != nil {
		if s := r.firstKid(alt); s != nil {
			r.into(s, dst)
		}
	}
	r.b.Merge(t)
	r.release(fm)
}

// openLoop opens the loop frame of n, named by its label if it has one.
func (r *rsLower) openLoop(n *ts.Node) flow.Frame {
	if lab := r.labelOf(n); lab != "" {
		return r.b.OpenLoop(lab)
	}
	return r.b.OpenLoop()
}

func (r *rsLower) loopExpr(n *ts.Node, dst int32) {
	f := r.openLoop(n)
	r.openFrame(r.labelNode(n), true, dst)
	h := r.node(flow.Stmt, spanOf(r.token(n, r.k.loopKw)), 0, 0)
	r.block(n.ChildByFieldId(r.k.fBody), -1)
	r.closeFrame()
	r.b.ContinueHere(f)
	r.b.Close(h)
	r.b.CloseFrame(f)
}

func (r *rsLower) whileExpr(n *ts.Node) {
	k := r.k
	f := r.openLoop(n)
	r.openFrame(r.labelNode(n), true, -1)
	mark := r.binds.mark()
	saved := r.open()
	fm := r.cond(n.ChildByFieldId(k.fCondition))
	h := r.close(saved)
	r.block(n.ChildByFieldId(k.fBody), -1)
	r.binds.truncate(mark)
	r.closeFrame()
	r.b.ContinueHere(f)
	r.b.Close(h)
	r.falses(fm)
	r.b.CloseFrame(f)
	r.release(fm)
}

func (r *rsLower) forExpr(n *ts.Node) {
	k := r.k
	iter := r.valueVar(n.ChildByFieldId(k.fValue))
	f := r.openLoop(n)
	r.openFrame(r.labelNode(n), true, -1)
	h := r.node(flow.Branch, spanOf(r.token(n, k.forKw)), 0, 0)
	r.b.Use(h, iter)
	exit := r.b.Push()
	mark := r.binds.mark()
	r.bindFrom(n.ChildByFieldId(k.fPattern), iter, -1)
	r.block(n.ChildByFieldId(k.fBody), -1)
	r.binds.truncate(mark)
	r.closeFrame()
	r.b.ContinueHere(f)
	r.b.Close(h)
	r.b.Restore(exit)
	r.b.CloseFrame(f)
	r.b.Pop(exit)
}

// armPattern splits a match arm's pattern into its pattern (nil for the
// wildcard `_`, an anonymous token) and its guard (nil if none). The pattern
// precedes the guard, so a named first child that is not the guard is it.
func (r *rsLower) armPattern(mp *ts.Node) (pat, guard *ts.Node) {
	guard = mp.ChildByFieldId(r.k.fCondition)
	if pat = r.firstKid(mp); pat != nil && guard != nil && pat.Id() == guard.Id() {
		pat = nil
	}
	return pat, guard
}

// matchExpr lowers a match whose arms' results define dst. The scrutinee
// is evaluated once, at its own node defining an owned variable, which every
// arm's pattern node and binding Use.
func (r *rsLower) matchExpr(n *ts.Node, dst int32) {
	k := r.k
	val := n.ChildByFieldId(k.fValue)
	s := r.valueVar(val)
	place := r.baseVar(val)
	sm := len(r.reads)
	r.reads = append(r.reads, s)
	se := sm + 1
	anchor := r.b.Push()
	base, eb := len(r.hs), len(r.ends)
	live := true
	start, arms := r.kids(n.ChildByFieldId(k.fBody))
	for i := range arms {
		arm := &arms[i]
		if arm.KindId() != k.matchArm {
			continue
		}
		pat, guard := r.armPattern(arm.ChildByFieldId(k.fPattern))
		mark := r.binds.mark()
		switch {
		case pat == nil:
		case pat.KindId() == k.identifier && i == len(arms)-1:
			r.pattern(pat, sm, se, true)
		case pat.KindId() == k.identifier:
			id := r.node(flow.Branch, spanOf(pat), sm, se)
			r.def(id, r.declare(pat))
			r.hs = append(r.hs, r.b.Push())
		default:
			r.node(flow.Branch, spanOf(pat), sm, se)
			r.hs = append(r.hs, r.b.Push())
			r.patternOf(pat, sm, se, true, place)
		}
		if guard != nil {
			r.cond(guard)
		}
		r.into(arm.ChildByFieldId(k.fValue), dst)
		r.binds.truncate(mark)
		r.ends = append(r.ends, r.b.Push())
		if len(r.hs) == base {
			live = false
			break
		}
		r.falses(base)
		r.hs = r.hs[:base]
	}
	r.done(start)
	if !live {
		r.b.Restore(r.ends[eb])
	}
	for _, e := range r.ends[eb:] {
		r.b.Merge(e)
	}
	r.b.Pop(anchor)
	r.hs, r.ends, r.reads = r.hs[:base], r.ends[:eb], r.reads[:sm]
}

// let lowers a let declaration. A pattern other than a bare identifier
// matches the value its node evaluates once, through an owned variable that
// node defines and every binding Uses.
func (r *rsLower) let(n *ts.Node) {
	k := r.k
	pat, val := n.ChildByFieldId(k.fPattern), n.ChildByFieldId(k.fValue)
	if val == nil {
		r.pattern(pat, 0, 0, false)
		return
	}
	m := len(r.reads)
	r.value(val)
	place := r.baseVar(val)
	if alt := n.ChildByFieldId(k.fAlternative); alt != nil {
		id := r.node(flow.Branch, spanOf(n), m, len(r.reads))
		r.reads = r.reads[:m]
		s := r.b.Var()
		r.def(id, s)
		p := r.b.Push()
		r.block(alt, -1)
		// The language requires the else block to diverge; a block ending
		// in a plain panicking-macro node still has a fringe, which returns.
		r.b.Return()
		r.b.Restore(p)
		r.b.Pop(p)
		r.bindFrom(pat, s, place)
		return
	}
	id := r.node(flow.Stmt, spanOf(n), m, len(r.reads))
	r.reads = r.reads[:m]
	if pat.KindId() == k.identifier {
		r.def(id, r.declare(pat))
		return
	}
	s := r.b.Var()
	r.def(id, s)
	r.bindFrom(pat, s, place)
}

// assign lowers `left = right`. Its value is `()`, so it yields none. A
// destructuring assignment's node evaluates the value once and defines an
// owned variable every target Uses.
func (r *rsLower) assign(n *ts.Node) {
	k := r.k
	left, right := r.l.unparen(n.ChildByFieldId(k.fLeft)), n.ChildByFieldId(k.fRight)
	m := len(r.reads)
	switch left.KindId() {
	case k.identifier:
		r.value(right)
		r.def(r.node(flow.Stmt, spanOf(n), m, len(r.reads)), r.lookup(left))
	case k.tupleExpression, k.arrayExpression, k.callExpression, k.structExpression:
		r.value(right)
		id := r.node(flow.Stmt, spanOf(n), m, len(r.reads))
		r.reads = r.reads[:m]
		s := r.b.Var()
		r.def(id, s)
		r.targets(left, s)
	default:
		r.value(left)
		r.value(right)
		id := r.node(flow.Stmt, spanOf(n), m, len(r.reads))
		if v := r.baseVar(left); v >= 0 {
			r.b.MayDef(id, v)
		}
	}
	r.reads = r.reads[:m]
}

// assignees pushes onto buf the assignees destructuring assignee t holds, in
// source order, and returns the stack mark and the list, or ok false when t
// is no destructuring assignee. By the Reference's "Destructuring
// assignments", one is a tuple or array (slice) expression, whose elements
// are the assignees; a call, the tuple-struct form `P(a, b)`, whose
// arguments are; or a struct expression `S { x, f: y }`, whose assignees are
// the shorthand field's name and the named field's value, never the field
// name. The callee path and struct name are no assignees, and neither is a
// rest `..` nor an attribute.
func (r *rsLower) assignees(t *ts.Node) (mark int, list []ts.Node, ok bool) {
	k := r.k
	holder := t
	switch t.KindId() {
	case k.tupleExpression, k.arrayExpression:
	case k.callExpression:
		holder = t.ChildByFieldId(k.fArguments)
	case k.structExpression:
		holder = t.ChildByFieldId(k.fBody)
	default:
		return 0, nil, false
	}
	if holder == nil {
		return len(r.buf), nil, true
	}
	start, kids := r.kids(holder)
	n := start
	for i := range kids {
		c := kids[i]
		switch c.KindId() {
		case k.attributeItem, k.baseFieldInitializer:
			continue
		case k.rangeExpression:
			if c.NamedChildCount() == 0 {
				continue
			}
		case k.shorthandFieldInitializer:
			// Its name follows any attributes.
			name := c.NamedChild(c.NamedChildCount() - 1)
			if name == nil {
				continue
			}
			c = *name
		case k.fieldInitializer:
			v := c.ChildByFieldId(k.fValue)
			if v == nil {
				continue
			}
			c = *v
		}
		r.buf[n] = c
		n++
	}
	r.buf = r.buf[:n]
	return start, r.buf[start:], true
}

// targets lowers the targets of a destructuring assignment whose value is in
// variable s: one node per target, in source order, each Using s; a place
// target also Uses its own operands and may-defines its base variable.
func (r *rsLower) targets(t *ts.Node, s int32) {
	k := r.k
	t = r.l.unparen(t)
	if start, list, ok := r.assignees(t); ok {
		for i := range list {
			r.targets(&list[i], s)
		}
		r.done(start)
		return
	}
	m := len(r.reads)
	r.read(s)
	if t.KindId() == k.identifier {
		r.def(r.node(flow.Stmt, spanOf(t), m, len(r.reads)), r.lookup(t))
	} else {
		r.value(t)
		id := r.node(flow.Stmt, spanOf(t), m, len(r.reads))
		if v := r.baseVar(t); v >= 0 {
			r.b.MayDef(id, v)
		}
	}
	r.reads = r.reads[:m]
}

// compound lowers `left op= right`: the target is read, then written. Its
// value is `()`, so it yields none.
func (r *rsLower) compound(n *ts.Node) {
	k := r.k
	left, right := r.l.unparen(n.ChildByFieldId(k.fLeft)), n.ChildByFieldId(k.fRight)
	m := len(r.reads)
	if left.KindId() == k.identifier {
		v := r.lookup(left)
		r.read(v)
		r.value(right)
		r.def(r.node(flow.Stmt, spanOf(n), m, len(r.reads)), v)
	} else {
		r.value(left)
		r.value(right)
		id := r.node(flow.Stmt, spanOf(n), m, len(r.reads))
		if v := r.baseVar(left); v >= 0 {
			r.b.MayDef(id, v)
		}
	}
	r.reads = r.reads[:m]
}

// jump lowers return, break and continue: a Jump node Using the value's
// reads, then the transfer. A valued break's node defines the result
// variable of the loop or labelled block it leaves; the jump itself is of
// type `!` and yields nothing. The label, a kid value skips, is no read.
func (r *rsLower) jump(n *ts.Node) {
	k := r.k
	m := len(r.reads)
	label := r.labelOf(n)
	start, list := r.kids(n)
	for i := range list {
		r.value(&list[i])
	}
	r.done(start)
	id := r.node(flow.Jump, spanOf(n), m, len(r.reads))
	r.reads = r.reads[:m]
	switch n.KindId() {
	case k.returnExpression:
		r.b.Return()
	case k.breakExpression:
		if len(list) > 0 && (len(list) > 1 || list[0].KindId() != k.label) {
			r.yield(id, r.target(label))
		}
		r.b.Break(label)
	default:
		r.b.Continue(label)
	}
}

// baseVar is the variable a field, index or dereference place writes
// through (x in `x.f`, `x[i].f`, `*x`, `(*x).f`), or -1. Only `*` of the
// unary operators yields a place: `-x` and `!x` are values, so `&mut !b`
// borrows a temporary, never b.
func (r *rsLower) baseVar(t *ts.Node) int32 {
	k := r.k
	for t = r.l.unparen(t); t != nil; t = r.l.unparen(t) {
		switch t.KindId() {
		case k.identifier, k.self:
			return r.lookup(t)
		case k.fieldExpression:
			t = t.ChildByFieldId(k.fValue)
		case k.indexExpression:
			t = r.firstKid(t)
		case k.unaryExpression:
			if r.token(t, k.deref) == nil {
				return -1
			}
			t = r.firstKid(t)
		default:
			return -1
		}
	}
	return -1
}

// capFunction collects the enclosing variables nested callable fn
// references, resolving names through its own parameters and scopes first.
// A `move` closure, async block or gen block captures by value, so it writes
// its own copies: its reads are kept and its writes, its nested callables'
// included, are dropped (Reference "Closure expressions", "Async blocks").
func (r *rsLower) capFunction(fn *ts.Node) {
	k := r.k
	w := len(r.writes)
	r.shadow++
	mark := r.binds.mark()
	if fn.KindId() == k.closureExpression {
		if ps := fn.ChildByFieldId(k.fParameters); ps != nil {
			r.params(ps)
		}
		if body := fn.ChildByFieldId(k.fBody); body != nil && body.IsNamed() {
			r.cap(body)
		}
	} else if body := r.firstKid(fn); body != nil {
		r.cap(body)
	}
	r.binds.truncate(mark)
	r.shadow--
	if r.token(fn, k.moveKw) != nil {
		r.writes = r.writes[:w]
	}
}

// cap collects the references n makes to variables of the function being
// lowered, honouring every scope n opens.
func (r *rsLower) cap(n *ts.Node) {
	if n == nil {
		return
	}
	k := r.k
	id := n.KindId()
	if id == k.functionItem {
		return
	}
	if r.l.isCallable(n) {
		r.capFunction(n)
		return
	}
	switch id {
	case k.identifier, k.self:
		r.read(r.lookup(n))
	case k.macroInvocation:
		r.macro(n)
	case k.assignmentExpression:
		r.capWrite(n.ChildByFieldId(k.fLeft), false)
		r.cap(n.ChildByFieldId(k.fRight))
	case k.compoundAssignmentExpr:
		r.capWrite(n.ChildByFieldId(k.fLeft), true)
		r.cap(n.ChildByFieldId(k.fRight))
	case k.block:
		mark := r.binds.mark()
		start, list := r.kids(n)
		for i := range list {
			r.predeclare(&list[i])
		}
		for i := range list {
			r.cap(&list[i])
		}
		r.done(start)
		r.binds.truncate(mark)
	case k.letDeclaration:
		v := n.ChildByFieldId(k.fValue)
		if v != nil {
			r.cap(v)
		}
		if alt := n.ChildByFieldId(k.fAlternative); alt != nil {
			r.cap(alt)
		}
		r.patternOf(n.ChildByFieldId(k.fPattern), 0, 0, false, r.baseVar(v))
	case k.letCondition:
		v := n.ChildByFieldId(k.fValue)
		r.cap(v)
		r.patternOf(n.ChildByFieldId(k.fPattern), 0, 0, false, r.baseVar(v))
	case k.ifExpression, k.whileExpression:
		mark := r.binds.mark()
		r.cap(n.ChildByFieldId(k.fCondition))
		if c := n.ChildByFieldId(k.fConsequence); c != nil {
			r.cap(c)
		}
		if body := n.ChildByFieldId(k.fBody); body != nil {
			r.cap(body)
		}
		r.binds.truncate(mark)
		if alt := n.ChildByFieldId(k.fAlternative); alt != nil {
			r.cap(alt)
		}
	case k.forExpression:
		r.cap(n.ChildByFieldId(k.fValue))
		mark := r.binds.mark()
		r.pattern(n.ChildByFieldId(k.fPattern), 0, 0, false)
		r.cap(n.ChildByFieldId(k.fBody))
		r.binds.truncate(mark)
	case k.matchExpression:
		v := n.ChildByFieldId(k.fValue)
		r.cap(v)
		body := n.ChildByFieldId(k.fBody)
		if body == nil {
			return
		}
		place := r.baseVar(v)
		start, arms := r.kids(body)
		for i := range arms {
			arm := &arms[i]
			if arm.KindId() != k.matchArm {
				continue
			}
			mark := r.binds.mark()
			pat, guard := r.armPattern(arm.ChildByFieldId(k.fPattern))
			if pat != nil {
				r.patternOf(pat, 0, 0, false, place)
			}
			if guard != nil {
				r.cap(guard)
			}
			r.cap(arm.ChildByFieldId(k.fValue))
			r.binds.truncate(mark)
		}
		r.done(start)
	case k.typeCastExpression:
		r.cap(n.ChildByFieldId(k.fValue))
	case k.genericFunction:
		r.cap(n.ChildByFieldId(k.fFunction))
	case k.structExpression:
		r.cap(n.ChildByFieldId(k.fBody))
	case k.referenceExpression:
		v := n.ChildByFieldId(k.fValue)
		r.cap(v)
		if r.token(n, k.mutableSpecifier) != nil {
			if b := r.baseVar(v); b >= 0 {
				r.writes = append(r.writes, b)
			}
		}
	case k.scopedIdentifier, k.label:
	default:
		start, list := r.kids(n)
		for i := range list {
			r.cap(&list[i])
		}
		r.done(start)
	}
}

// capWrite records, as writes, the enclosing variables an assignment target
// inside a nested callable binds: an identifier (read too when read is set),
// every target of a destructuring assignment, or the base of a field, index
// or dereference target, whose operands are read.
func (r *rsLower) capWrite(t *ts.Node, read bool) {
	k := r.k
	t = r.l.unparen(t)
	if t.KindId() == k.identifier {
		if v := r.lookup(t); v >= 0 {
			if read {
				r.read(v)
			}
			r.writes = append(r.writes, v)
		}
		return
	}
	if start, list, ok := r.assignees(t); ok {
		for i := range list {
			r.capWrite(&list[i], false)
		}
		r.done(start)
		return
	}
	r.cap(t)
	if v := r.baseVar(t); v >= 0 {
		r.writes = append(r.writes, v)
	}
}

// rsSyntax holds the kind and field ids the Rust lowering matches, resolved
// once by name against the pinned grammar so the walk compares integers
// rather than converting every node's kind to a string.
type rsSyntax struct {
	functionItem, closureExpression, closureParameters, parameter, selfParameter, variadicParameter,
	attributeItem, self, identifier, block, label, expressionStatement, letDeclaration, letCondition,
	letChain, ifExpression, whileExpression, loopExpression, forExpression, matchExpression, matchArm,
	breakExpression, continueExpression, returnExpression, tryExpression, tryBlock, unsafeBlock, constBlock,
	assignmentExpression, compoundAssignmentExpr, binaryExpression, fieldExpression, indexExpression,
	unaryExpression, macroInvocation, tokenTree, tupleExpression, arrayExpression, typeCastExpression,
	genericFunction, structExpression, scopedIdentifier, constItem, staticItem, structItem,
	shorthandFieldIdentifier, fieldPattern, orPattern, tupleStructPattern, structPattern, capturedPattern,
	mutPattern, refPattern, referencePattern, tuplePattern, slicePattern, referenceExpression, mutableSpecifier,
	callExpression, rangeExpression, shorthandFieldInitializer, fieldInitializer, baseFieldInitializer uint16

	and, or, loopKw, forKw, deref, moveKw, refKw uint16

	// The tokens a macro's token tree is read by.
	question, returnKw, breakKw, continueKw, whileKw, fnKw, asyncKw, genKw, awaitKw, dot, pathSep, eq, colon, bang,
	quote, amp, pipe, comma, semi, stringLiteral, rawStringLiteral, stringContent uint16

	fAlternative, fArguments, fBody, fCondition, fConsequence, fFunction, fLeft, fName, fOperator, fParameters,
	fPattern, fRight, fType, fValue uint16

	// item is indexed by kind id: the statement kinds that make no node.
	item []bool
}

var (
	rsSyntaxOnce  sync.Once
	rsSyntaxTable *rsSyntax
)

// rsSyntaxOf resolves the table once. A name the grammar does not define is
// a lowering defect and panics, so a misspelt kind can never silently match
// nothing.
func rsSyntaxOf() *rsSyntax {
	rsSyntaxOnce.Do(func() {
		const language = "rust"
		tl := mustGrammar(language)
		kind := func(name string) uint16 { return mustKind(tl, language, name, true) }
		tok := func(name string) uint16 { return mustKind(tl, language, name, false) }
		field := func(name string) uint16 { return mustField(tl, language, name) }
		s := &rsSyntax{}
		s.functionItem, s.closureExpression, s.closureParameters = kind("function_item"), kind("closure_expression"), kind("closure_parameters")
		s.parameter, s.selfParameter, s.variadicParameter = kind("parameter"), kind("self_parameter"), kind("variadic_parameter")
		s.attributeItem, s.self, s.identifier, s.block, s.label = kind("attribute_item"), kind("self"), kind("identifier"), kind("block"), kind("label")
		s.expressionStatement, s.letDeclaration = kind("expression_statement"), kind("let_declaration")
		s.letCondition, s.letChain, s.ifExpression = kind("let_condition"), kind("let_chain"), kind("if_expression")
		s.whileExpression, s.loopExpression, s.forExpression = kind("while_expression"), kind("loop_expression"), kind("for_expression")
		s.matchExpression, s.matchArm = kind("match_expression"), kind("match_arm")
		s.breakExpression, s.continueExpression, s.returnExpression = kind("break_expression"), kind("continue_expression"), kind("return_expression")
		s.tryExpression, s.tryBlock, s.unsafeBlock, s.constBlock = kind("try_expression"), kind("try_block"), kind("unsafe_block"), kind("const_block")
		s.assignmentExpression, s.compoundAssignmentExpr = kind("assignment_expression"), kind("compound_assignment_expr")
		s.binaryExpression, s.fieldExpression, s.indexExpression = kind("binary_expression"), kind("field_expression"), kind("index_expression")
		s.unaryExpression, s.macroInvocation, s.tokenTree = kind("unary_expression"), kind("macro_invocation"), kind("token_tree")
		s.tupleExpression, s.arrayExpression, s.typeCastExpression = kind("tuple_expression"), kind("array_expression"), kind("type_cast_expression")
		s.genericFunction, s.structExpression, s.scopedIdentifier = kind("generic_function"), kind("struct_expression"), kind("scoped_identifier")
		s.constItem, s.staticItem, s.structItem = kind("const_item"), kind("static_item"), kind("struct_item")
		s.shorthandFieldIdentifier, s.fieldPattern, s.orPattern = kind("shorthand_field_identifier"), kind("field_pattern"), kind("or_pattern")
		s.tupleStructPattern, s.structPattern, s.capturedPattern = kind("tuple_struct_pattern"), kind("struct_pattern"), kind("captured_pattern")
		s.mutPattern, s.refPattern, s.referencePattern = kind("mut_pattern"), kind("ref_pattern"), kind("reference_pattern")
		s.tuplePattern, s.slicePattern = kind("tuple_pattern"), kind("slice_pattern")
		s.referenceExpression, s.mutableSpecifier = kind("reference_expression"), kind("mutable_specifier")
		s.callExpression, s.rangeExpression = kind("call_expression"), kind("range_expression")
		s.shorthandFieldInitializer, s.fieldInitializer = kind("shorthand_field_initializer"), kind("field_initializer")
		s.baseFieldInitializer = kind("base_field_initializer")
		s.and, s.or, s.loopKw, s.forKw = tok("&&"), tok("||"), tok("loop"), tok("for")
		s.deref, s.moveKw, s.refKw = tok("*"), tok("move"), tok("ref")
		s.fAlternative, s.fArguments, s.fBody, s.fCondition = field("alternative"), field("arguments"), field("body"), field("condition")
		s.fConsequence, s.fFunction, s.fLeft, s.fName = field("consequence"), field("function"), field("left"), field("name")
		s.fOperator, s.fParameters, s.fPattern = field("operator"), field("parameters"), field("pattern")
		s.fRight, s.fType, s.fValue = field("right"), field("type"), field("value")
		s.question, s.returnKw, s.breakKw, s.continueKw = tok("?"), tok("return"), tok("break"), tok("continue")
		s.whileKw, s.fnKw, s.asyncKw, s.genKw, s.awaitKw = tok("while"), tok("fn"), tok("async"), tok("gen"), tok("await")
		s.dot, s.pathSep, s.eq, s.colon, s.bang = tok("."), tok("::"), tok("="), tok(":"), tok("!")
		s.quote, s.amp, s.pipe, s.comma, s.semi = tok("'"), tok("&"), tok("|"), tok(","), tok(";")
		s.stringLiteral, s.rawStringLiteral, s.stringContent = kind("string_literal"), kind("raw_string_literal"), kind("string_content")
		s.item = make([]bool, tl.NodeKindCount())
		for _, name := range [...]string{"associated_type", "attribute_item", "const_item", "empty_statement", "enum_item",
			"extern_crate_declaration", "foreign_mod_item", "function_item", "function_signature_item", "impl_item",
			"inner_attribute_item", "macro_definition", "mod_item", "static_item", "struct_item", "trait_item",
			"type_item", "union_item", "use_declaration"} {
			s.item[kind(name)] = true
		}
		rsSyntaxTable = s
	})
	return rsSyntaxTable
}
