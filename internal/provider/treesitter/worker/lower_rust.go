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
//     expression, spans the expression without its `;` and Uses every read
//     inside it; a let declaration spans the declaration. An assignment or
//     compound assignment is its own defining node, spanning it; return,
//     break and continue are Jump nodes spanning the expression. A macro
//     invocation is one Stmt node spanning it whose Uses are the identifiers
//     of its token tree that resolve to a variable; its expansion is not
//     lowered, so a panicking macro (panic!, unreachable!, todo!, assert!) is
//     a plain node that falls through, as a call does. A string literal's
//     implicit format captures (`"{x}"`) are text and not Uses.
//   - An expression lowered for its value ends in the node that consumes it
//     (a let, an assignment, a jump, a statement) and takes no second node
//     when the last node its own lowering made already spans it, as a `?`, a
//     closure or an assignment does. A consuming node Uses every read inside
//     its span: the value of an if, a match, a loop's valued break or a
//     block's tail derives from them, since the lowering introduces no
//     temporaries.
//   - `e?` is a Branch node spanning the whole `e?`, after e's nodes, whose
//     successors are Exit (inside a try block: the end of that block) and the
//     rest of the enclosing expression, so everything after it depends on it.
//   - `&&` and `||`: the left operand is a Branch node spanning it, the right
//     operand a Stmt node spanning it, evaluated only on the short-circuit
//     path.
//   - An if condition, a while condition and a match guard are one Branch
//     node spanning the condition. A let chain (`a && let P = v && b`) is one
//     Branch per member in source order; each member's false edge leaves the
//     chain. A let condition `let P = v` is a Branch spanning it, followed on
//     the taken path by one defining node per name P binds, spanning the name
//     and Using v's reads (the destructuring rule, so D ≤ N). A let chain's
//     bindings are in scope for its later members and the body only.
//   - `loop` has a Stmt head spanning the `loop` keyword and no exit edge:
//     only a break leaves it. A while loop's back edge targets the first node
//     its condition makes. A for loop is a Stmt node for the iterated
//     expression, evaluated once, which defines an iteration variable of the
//     lowering's own; a Branch head spanning the `for` keyword that Uses it;
//     then one defining node per name the pattern binds, Using it, so the
//     head and the names depend on the iterated value as it was before the
//     loop, never on a write to its variables in the body.
//   - A match is a Stmt node for the scrutinee, then its arms in source
//     order: a Branch node spanning the arm's pattern, then one defining node
//     per name the pattern binds, then a Branch for the guard, then the arm's
//     value. The pattern Branch and every binding node Use the scrutinee's
//     reads, since the pattern tests it. A failed pattern or guard goes to
//     the next arm's pattern; no arm falls into another. A match has no jump
//     frame, so a break inside an arm leaves the enclosing loop. Exhaustiveness
//     is not checked here, so the last arm keeps its false edge, out of the
//     match, unless its pattern is irrefutable by syntax alone: the wildcard
//     `_` (in any arm, which also makes every later arm unreachable, so they
//     lower to nothing) or, in the last arm, a bare identifier, which is then
//     a Stmt node defining the name. A bare identifier in an earlier arm may
//     name a constant or a unit variant (`None`), so it is a Branch that also
//     defines the name it would bind.
//   - A labelled block `'a: { … }` opens a block frame named by its label;
//     `break 'a v` leaves it with its value read by the consuming node.
//   - `let P = v else { … };` is a Branch node spanning the declaration; the
//     else block, which the language requires to diverge, is lowered on its
//     false edge and ended by a return, so a block that ends in a plain
//     panicking-macro node never falls into the bindings; the taken path is
//     one defining node per name. Any other let with an initializer is one
//     node spanning the declaration that defines a bare identifier pattern,
//     or, for a destructuring pattern, a node that defines nothing followed
//     by one defining node per bound name. `let x;` declares x and makes no
//     node. A destructuring assignment `(a, b) = e` is likewise a node for
//     the assignment then one node per target.
//   - A nested callable (closure, async or gen block) is its own function;
//     its creating expression is one Stmt node spanning it, which Uses every
//     enclosing variable referenced inside it, resolved with its own scopes
//     so a name it declares shadows, and may-defines every enclosing variable
//     it assigns (directly or through a field, index or dereference), since
//     when it runs is unknown.
//   - A try block `try { … }` opens a frame named rsTryLabel; a `?` inside
//     it breaks to the block's end. unsafe and const blocks are inline blocks.
//   - A write through a field, index or dereference target (`x.f = e`,
//     `a[i] += e`, `*p = e`) Uses the target's operands and is a non-killing
//     may-definition of its base variable. A method call or a `&mut x`
//     argument is not a definition of the receiver or of x, as a call is not
//     one in either seed lowering.
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
// reference_expression, range_expression, tuple_expression, array_expression,
// struct_expression, type_cast_expression, generic_function,
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
// are. A node that defines v also Uses v when its statement read v before
// it, since the value the statement consumes was read before the definition
// overwrote it (the seed's rule).
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
func lowerRust(l *Lowering, b *flow.Builder, fn *ts.Node, src []byte) {
	cur := fn.Walk()
	defer cur.Close()
	r := rsLower{l: l, b: b, src: src, k: rsSyntaxOf(), cur: cur, first: -1, last: -1}
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

// rsLower is the state of lowering one Rust callable.
type rsLower struct {
	l   *Lowering
	b   *flow.Builder
	src []byte
	k   *rsSyntax
	cur *ts.TreeCursor
	// buf is a stack of child lists; kids pushes one and done pops it.
	buf []ts.Node
	// binds is the scope chain, innermost last.
	binds scope
	// shadow is non-zero while walking a nested callable for its captures:
	// declarations then bind -1 and no node is created.
	shadow int
	// reads are the variables read by the statements being lowered, in
	// evaluation order; a node Uses a window of them.
	reads []int32
	// base is where the reads of the innermost statement begin; see def.
	base int
	// depth counts the enclosing expressions whose consuming node reads
	// every read inside them; while it is non-zero a statement keeps its
	// reads for that node.
	depth int
	// writes are the enclosing variables assigned inside the nested callable
	// whose captures are being collected.
	writes []int32
	// first is the first node created since the last open, or -1.
	first int32
	// last is the node created last, or -1, lastSpan its span, and lastDef
	// whether it defines a variable.
	last     int32
	lastSpan flow.Span
	lastDef  bool
	// hs holds the saved false edges of the conditions and match arms being
	// lowered; ends holds the saved ends of the open matches' arms.
	hs, ends []flow.Fringe
	// tries counts the open try blocks.
	tries int
}

// kids pushes n's named, non-extra children (comments are extras) onto buf
// and returns the stack mark and the list; done(mark) pops them. A list stays
// valid across nested kids calls: later pushes never overwrite it.
func (r *rsLower) kids(n *ts.Node) (int, []ts.Node) {
	start := len(r.buf)
	c := r.cur
	c.Reset(*n)
	if c.GotoFirstChild() {
		for {
			if x := c.Node(); x.IsNamed() && !x.IsExtra() {
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
	r.binds = append(r.binds, binding{name: r.text(name), v: v})
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

// node creates a node spanning s that Uses reads[from:to].
func (r *rsLower) node(kind flow.Kind, s flow.Span, from, to int) int32 {
	id := r.b.Node(kind, s)
	if r.first < 0 {
		r.first = id
	}
	for _, v := range r.reads[from:to] {
		r.b.Use(id, v)
	}
	r.last, r.lastSpan, r.lastDef = id, s, false
	return id
}

// def records that node n defines v and, when the innermost statement read
// v before n, that n Uses v.
func (r *rsLower) def(n, v int32) {
	if v < 0 {
		return
	}
	r.b.Def(n, v)
	if n == r.last {
		r.lastDef = true
	}
	for _, u := range r.reads[r.base:] {
		if u == v {
			r.b.Use(n, v)
			return
		}
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
	if c := r.firstKid(n); c != nil && c.KindId() == r.k.label {
		return string(r.text(c))
	}
	return ""
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
		r.block(n)
	default:
		r.sub(n)
	}
}

// pattern binds every name pattern p binds, in source order. With nodes and
// outside a nested callable, each name is one Stmt node spanning it that
// defines it and Uses reads[from:to]. An or-pattern binds the names of its
// first alternative, since every alternative binds the same ones.
func (r *rsLower) pattern(p *ts.Node, from, to int, nodes bool) {
	if p == nil {
		return
	}
	k := r.k
	switch p.KindId() {
	case k.identifier, k.shorthandFieldIdentifier, k.self:
		v := r.declare(p)
		if nodes && r.shadow == 0 {
			r.def(r.node(flow.Stmt, spanOf(p), from, to), v)
		}
	case k.fieldPattern:
		if q := p.ChildByFieldId(k.fPattern); q != nil {
			r.pattern(q, from, to, nodes)
		} else if name := p.ChildByFieldId(k.fName); name != nil {
			r.pattern(name, from, to, nodes)
		}
	case k.orPattern:
		if q := r.firstKid(p); q != nil {
			r.pattern(q, from, to, nodes)
		}
	case k.tupleStructPattern, k.structPattern, k.capturedPattern, k.mutPattern, k.refPattern, k.referencePattern,
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

// predeclare binds the value name an item declares, from its block's start.
func (r *rsLower) predeclare(n *ts.Node) {
	k := r.k
	switch n.KindId() {
	case k.functionItem, k.constItem, k.staticItem, k.structItem:
		if name := n.ChildByFieldId(k.fName); name != nil {
			r.binds = append(r.binds, binding{name: r.text(name), v: -1})
		}
	}
}

// block lowers a block in its own scope; a labelled block is a block frame.
func (r *rsLower) block(n *ts.Node) {
	if n == nil {
		return
	}
	k := r.k
	mark := len(r.binds)
	start, list := r.kids(n)
	for i := range list {
		r.predeclare(&list[i])
	}
	var f flow.Frame
	labelled := false
	for i := range list {
		c := &list[i]
		if c.KindId() == k.label {
			f, labelled = r.b.OpenBlock(string(r.text(c))), true
			continue
		}
		r.stmt(c)
	}
	if labelled {
		r.b.CloseFrame(f)
	}
	r.done(start)
	r.binds = r.binds[:mark]
}

// stmt lowers one statement of a block, or its tail expression.
func (r *rsLower) stmt(n *ts.Node) {
	k := r.k
	id := n.KindId()
	if int(id) < len(k.item) && k.item[id] {
		return
	}
	m, saved := len(r.reads), r.base
	if r.depth == 0 {
		r.base = m
	}
	switch id {
	case k.expressionStatement:
		if e := r.firstKid(n); e != nil {
			r.expr(e)
		}
	case k.letDeclaration:
		r.let(n)
	default:
		r.expr(n)
	}
	r.base = saved
	if r.depth == 0 {
		r.reads = r.reads[:m]
	}
}

// sub lowers e, a match arm's value or a closure's expression body, as a
// statement of its own for def's earlier-read rule.
func (r *rsLower) sub(e *ts.Node) {
	saved := r.base
	if r.depth == 0 {
		r.base = len(r.reads)
	}
	r.expr(e)
	r.base = saved
}

// expr lowers e as a statement: evaluated for its effect, or as a block's
// value, which the enclosing consuming node reads.
func (r *rsLower) expr(e *ts.Node) {
	if e == nil {
		return
	}
	k := r.k
	u := r.l.unparen(e)
	switch u.KindId() {
	case k.ifExpression:
		r.ifExpr(u)
	case k.matchExpression:
		r.matchExpr(u)
	case k.loopExpression:
		r.loopExpr(u)
	case k.whileExpression:
		r.whileExpr(u)
	case k.forExpression:
		r.forExpr(u)
	case k.block:
		r.block(u)
	case k.unsafeBlock:
		r.block(r.firstKid(u))
	case k.constBlock:
		r.block(u.ChildByFieldId(k.fBody))
	case k.tryBlock:
		f := r.b.OpenBlock(rsTryLabel)
		r.tries++
		r.block(r.firstKid(u))
		r.tries--
		r.b.CloseFrame(f)
	case k.assignmentExpression:
		r.assign(u)
	case k.compoundAssignmentExpr:
		r.compound(u)
	case k.returnExpression, k.breakExpression, k.continueExpression:
		r.jump(u)
	case k.macroInvocation:
		m := len(r.reads)
		r.tokens(u)
		r.node(flow.Stmt, spanOf(u), m, len(r.reads))
	default:
		r.valueNode(e)
	}
}

// valueNode lowers n for its value and ends it with a Stmt node spanning n,
// unless the last node that lowering made already spans n.
func (r *rsLower) valueNode(n *ts.Node) {
	if n == nil {
		return
	}
	m, last := len(r.reads), r.last
	r.value(n)
	if r.spans(last, n) {
		return
	}
	r.node(flow.Stmt, spanOf(n), m, len(r.reads))
}

// value lowers an expression evaluated for its value: its reads are recorded
// for the consuming node, and nodes are created for every decision, jump,
// definition, nested callable and statement inside it.
func (r *rsLower) value(n *ts.Node) {
	if n == nil {
		return
	}
	k := r.k
	if r.l.isCallable(n) {
		r.closure(n)
		return
	}
	switch n.KindId() {
	case k.identifier, k.self:
		r.read(r.lookup(n))
	case k.ifExpression, k.matchExpression, k.loopExpression, k.whileExpression, k.forExpression, k.block,
		k.unsafeBlock, k.constBlock, k.tryBlock, k.assignmentExpression, k.compoundAssignmentExpr,
		k.returnExpression, k.breakExpression, k.continueExpression:
		r.depth++
		r.expr(n)
		r.depth--
	case k.tryExpression:
		m := len(r.reads)
		if e := r.firstKid(n); e != nil {
			r.value(e)
		}
		r.node(flow.Branch, spanOf(n), m, len(r.reads))
		p := r.b.Push()
		if r.tries > 0 {
			r.b.Break(rsTryLabel)
		} else {
			r.b.Return()
		}
		r.b.Restore(p)
		r.b.Pop(p)
	case k.binaryExpression:
		left, right := n.ChildByFieldId(k.fLeft), n.ChildByFieldId(k.fRight)
		if op := n.ChildByFieldId(k.fOperator).KindId(); op == k.and || op == k.or {
			m, last := len(r.reads), r.last
			r.value(left)
			if !r.spans(last, left) {
				r.node(flow.Branch, spanOf(left), m, len(r.reads))
			}
			p := r.b.Push()
			r.valueNode(right)
			r.b.Merge(p)
			r.b.Pop(p)
			return
		}
		r.value(left)
		r.value(right)
	case k.macroInvocation:
		r.tokens(n)
	case k.typeCastExpression:
		r.value(n.ChildByFieldId(k.fValue))
	case k.genericFunction:
		r.value(n.ChildByFieldId(k.fFunction))
	case k.structExpression:
		r.value(n.ChildByFieldId(k.fBody))
	case k.scopedIdentifier, k.label:
	default:
		start, list := r.kids(n)
		for i := range list {
			r.value(&list[i])
		}
		r.done(start)
	}
}

// tokens records, as reads, every identifier of macro invocation or token
// tree n's token trees that resolves to a variable; the macro's name is not
// one.
func (r *rsLower) tokens(n *ts.Node) {
	k := r.k
	tree := n.KindId() == k.tokenTree
	start, list := r.kids(n)
	for i := range list {
		switch c := &list[i]; c.KindId() {
		case k.tokenTree:
			r.tokens(c)
		case k.identifier, k.self:
			if tree {
				r.read(r.lookup(c))
			}
		}
	}
	r.done(start)
}

// closure creates the node spanning n, a nested callable: it Uses n's
// captures and may-defines every enclosing variable n assigns.
func (r *rsLower) closure(n *ts.Node) int32 {
	m, w := len(r.reads), len(r.writes)
	r.capFunction(n)
	id := r.node(flow.Stmt, spanOf(n), m, len(r.reads))
	for _, v := range r.writes[w:] {
		r.b.MayDef(id, v)
	}
	r.writes = r.writes[:w]
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
// saves its false edge in hs.
func (r *rsLower) condPart(c *ts.Node) {
	k := r.k
	m, last := len(r.reads), r.last
	if c.KindId() == k.letCondition {
		r.value(c.ChildByFieldId(k.fValue))
		e := len(r.reads)
		r.node(flow.Branch, spanOf(c), m, e)
		r.hs = append(r.hs, r.b.Push())
		r.pattern(c.ChildByFieldId(k.fPattern), m, e, true)
		return
	}
	r.value(c)
	if !r.spans(last, c) {
		r.node(flow.Branch, spanOf(c), m, len(r.reads))
	}
	r.hs = append(r.hs, r.b.Push())
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

func (r *rsLower) ifExpr(n *ts.Node) {
	k := r.k
	mark := len(r.binds)
	fm := r.cond(n.ChildByFieldId(k.fCondition))
	r.block(n.ChildByFieldId(k.fConsequence))
	r.binds = r.binds[:mark]
	t := r.b.Push()
	r.falses(fm)
	if alt := n.ChildByFieldId(k.fAlternative); alt != nil {
		if s := r.firstKid(alt); s != nil {
			r.expr(s)
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

func (r *rsLower) loopExpr(n *ts.Node) {
	f := r.openLoop(n)
	h := r.node(flow.Stmt, spanOf(r.token(n, r.k.loopKw)), 0, 0)
	r.block(n.ChildByFieldId(r.k.fBody))
	r.b.ContinueHere(f)
	r.b.Close(h)
	r.b.CloseFrame(f)
}

func (r *rsLower) whileExpr(n *ts.Node) {
	k := r.k
	f := r.openLoop(n)
	mark := len(r.binds)
	saved := r.open()
	fm := r.cond(n.ChildByFieldId(k.fCondition))
	h := r.close(saved)
	r.block(n.ChildByFieldId(k.fBody))
	r.binds = r.binds[:mark]
	r.b.ContinueHere(f)
	r.b.Close(h)
	r.falses(fm)
	r.b.CloseFrame(f)
	r.release(fm)
}

func (r *rsLower) forExpr(n *ts.Node) {
	k := r.k
	val := n.ChildByFieldId(k.fValue)
	m, last := len(r.reads), r.last
	r.value(val)
	d := r.last
	if !r.spans(last, val) || r.lastDef {
		d = r.node(flow.Stmt, spanOf(val), m, len(r.reads))
	}
	iter := r.b.Var()
	r.b.Def(d, iter)
	f := r.openLoop(n)
	h := r.node(flow.Branch, spanOf(r.token(n, k.forKw)), 0, 0)
	r.b.Use(h, iter)
	exit := r.b.Push()
	mark, im := len(r.binds), len(r.reads)
	r.reads = append(r.reads, iter)
	r.pattern(n.ChildByFieldId(k.fPattern), im, im+1, true)
	r.reads = r.reads[:im]
	r.block(n.ChildByFieldId(k.fBody))
	r.binds = r.binds[:mark]
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

func (r *rsLower) matchExpr(n *ts.Node) {
	k := r.k
	val := n.ChildByFieldId(k.fValue)
	sm, last := len(r.reads), r.last
	r.value(val)
	se := len(r.reads)
	if !r.spans(last, val) {
		r.node(flow.Stmt, spanOf(val), sm, se)
	}
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
		mark := len(r.binds)
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
			r.pattern(pat, sm, se, true)
		}
		if guard != nil {
			r.cond(guard)
		}
		r.sub(arm.ChildByFieldId(k.fValue))
		r.binds = r.binds[:mark]
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
	r.hs, r.ends = r.hs[:base], r.ends[:eb]
}

// let lowers a let declaration.
func (r *rsLower) let(n *ts.Node) {
	k := r.k
	pat, val := n.ChildByFieldId(k.fPattern), n.ChildByFieldId(k.fValue)
	if val == nil {
		r.pattern(pat, 0, 0, false)
		return
	}
	m := len(r.reads)
	r.value(val)
	e := len(r.reads)
	if alt := n.ChildByFieldId(k.fAlternative); alt != nil {
		r.node(flow.Branch, spanOf(n), m, e)
		p := r.b.Push()
		r.block(alt)
		// The language requires the else block to diverge; a block ending
		// in a plain panicking-macro node still has a fringe, which returns.
		r.b.Return()
		r.b.Restore(p)
		r.b.Pop(p)
		r.pattern(pat, m, e, true)
		return
	}
	id := r.node(flow.Stmt, spanOf(n), m, e)
	if pat.KindId() == k.identifier {
		r.def(id, r.declare(pat))
		return
	}
	r.pattern(pat, m, e, true)
}

// assign lowers `left = right`.
func (r *rsLower) assign(n *ts.Node) {
	k := r.k
	left, right := r.l.unparen(n.ChildByFieldId(k.fLeft)), n.ChildByFieldId(k.fRight)
	m := len(r.reads)
	switch left.KindId() {
	case k.identifier:
		r.value(right)
		r.def(r.node(flow.Stmt, spanOf(n), m, len(r.reads)), r.lookup(left))
	case k.tupleExpression, k.arrayExpression:
		r.value(right)
		e := len(r.reads)
		r.node(flow.Stmt, spanOf(n), m, e)
		r.targets(left, m, e)
	default:
		r.value(left)
		r.value(right)
		id := r.node(flow.Stmt, spanOf(n), m, len(r.reads))
		if v := r.baseVar(left); v >= 0 {
			r.b.MayDef(id, v)
		}
	}
}

// targets lowers the targets of a destructuring assignment whose value read
// reads[from:to]: one node per target, in source order.
func (r *rsLower) targets(t *ts.Node, from, to int) {
	k := r.k
	switch t = r.l.unparen(t); t.KindId() {
	case k.identifier:
		r.def(r.node(flow.Stmt, spanOf(t), from, to), r.lookup(t))
	case k.tupleExpression, k.arrayExpression:
		start, list := r.kids(t)
		for i := range list {
			r.targets(&list[i], from, to)
		}
		r.done(start)
	default:
		m := len(r.reads)
		r.reads = append(r.reads, r.reads[from:to]...)
		r.value(t)
		id := r.node(flow.Stmt, spanOf(t), m, len(r.reads))
		if v := r.baseVar(t); v >= 0 {
			r.b.MayDef(id, v)
		}
	}
}

// compound lowers `left op= right`: the target is read, then written.
func (r *rsLower) compound(n *ts.Node) {
	k := r.k
	left, right := r.l.unparen(n.ChildByFieldId(k.fLeft)), n.ChildByFieldId(k.fRight)
	m := len(r.reads)
	if left.KindId() == k.identifier {
		v := r.lookup(left)
		r.read(v)
		r.value(right)
		r.def(r.node(flow.Stmt, spanOf(n), m, len(r.reads)), v)
		return
	}
	r.value(left)
	r.value(right)
	id := r.node(flow.Stmt, spanOf(n), m, len(r.reads))
	if v := r.baseVar(left); v >= 0 {
		r.b.MayDef(id, v)
	}
}

// jump lowers return, break and continue: a Jump node Using the value's
// reads, then the transfer.
func (r *rsLower) jump(n *ts.Node) {
	k := r.k
	m := len(r.reads)
	label := ""
	start, list := r.kids(n)
	for i := range list {
		if c := &list[i]; c.KindId() == k.label {
			label = string(r.text(c))
		} else {
			r.value(c)
		}
	}
	r.done(start)
	r.node(flow.Jump, spanOf(n), m, len(r.reads))
	switch n.KindId() {
	case k.returnExpression:
		r.b.Return()
	case k.breakExpression:
		r.b.Break(label)
	default:
		r.b.Continue(label)
	}
}

// baseVar is the variable a field, index or dereference target writes
// through (x in `x.f`, `x[i].f`, `*x`, `(*x).f`), or -1.
func (r *rsLower) baseVar(t *ts.Node) int32 {
	k := r.k
	for t = r.l.unparen(t); t != nil; t = r.l.unparen(t) {
		switch t.KindId() {
		case k.identifier, k.self:
			return r.lookup(t)
		case k.fieldExpression:
			t = t.ChildByFieldId(k.fValue)
		case k.indexExpression, k.unaryExpression:
			t = r.firstKid(t)
		default:
			return -1
		}
	}
	return -1
}

// capFunction collects the enclosing variables nested callable fn
// references, resolving names through its own parameters and scopes first.
func (r *rsLower) capFunction(fn *ts.Node) {
	k := r.k
	r.shadow++
	mark := len(r.binds)
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
	r.binds = r.binds[:mark]
	r.shadow--
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
		r.tokens(n)
	case k.assignmentExpression:
		r.capWrite(n.ChildByFieldId(k.fLeft), false)
		r.cap(n.ChildByFieldId(k.fRight))
	case k.compoundAssignmentExpr:
		r.capWrite(n.ChildByFieldId(k.fLeft), true)
		r.cap(n.ChildByFieldId(k.fRight))
	case k.block:
		mark := len(r.binds)
		start, list := r.kids(n)
		for i := range list {
			r.predeclare(&list[i])
		}
		for i := range list {
			r.cap(&list[i])
		}
		r.done(start)
		r.binds = r.binds[:mark]
	case k.letDeclaration:
		if v := n.ChildByFieldId(k.fValue); v != nil {
			r.cap(v)
		}
		if alt := n.ChildByFieldId(k.fAlternative); alt != nil {
			r.cap(alt)
		}
		r.pattern(n.ChildByFieldId(k.fPattern), 0, 0, false)
	case k.letCondition:
		r.cap(n.ChildByFieldId(k.fValue))
		r.pattern(n.ChildByFieldId(k.fPattern), 0, 0, false)
	case k.ifExpression, k.whileExpression:
		mark := len(r.binds)
		r.cap(n.ChildByFieldId(k.fCondition))
		if c := n.ChildByFieldId(k.fConsequence); c != nil {
			r.cap(c)
		}
		if body := n.ChildByFieldId(k.fBody); body != nil {
			r.cap(body)
		}
		r.binds = r.binds[:mark]
		if alt := n.ChildByFieldId(k.fAlternative); alt != nil {
			r.cap(alt)
		}
	case k.forExpression:
		r.cap(n.ChildByFieldId(k.fValue))
		mark := len(r.binds)
		r.pattern(n.ChildByFieldId(k.fPattern), 0, 0, false)
		r.cap(n.ChildByFieldId(k.fBody))
		r.binds = r.binds[:mark]
	case k.matchArm:
		mark := len(r.binds)
		pat, guard := r.armPattern(n.ChildByFieldId(k.fPattern))
		if pat != nil {
			r.pattern(pat, 0, 0, false)
		}
		if guard != nil {
			r.cap(guard)
		}
		r.cap(n.ChildByFieldId(k.fValue))
		r.binds = r.binds[:mark]
	case k.typeCastExpression:
		r.cap(n.ChildByFieldId(k.fValue))
	case k.genericFunction:
		r.cap(n.ChildByFieldId(k.fFunction))
	case k.structExpression:
		r.cap(n.ChildByFieldId(k.fBody))
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
	switch t = r.l.unparen(t); t.KindId() {
	case k.identifier:
		if v := r.lookup(t); v >= 0 {
			if read {
				r.read(v)
			}
			r.writes = append(r.writes, v)
		}
	case k.tupleExpression, k.arrayExpression:
		start, list := r.kids(t)
		for i := range list {
			r.capWrite(&list[i], false)
		}
		r.done(start)
	default:
		r.cap(t)
		if v := r.baseVar(t); v >= 0 {
			r.writes = append(r.writes, v)
		}
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
	mutPattern, refPattern, referencePattern, tuplePattern, slicePattern uint16

	and, or, loopKw, forKw uint16

	fAlternative, fBody, fCondition, fConsequence, fFunction, fLeft, fName, fOperator, fParameters, fPattern,
	fRight, fType, fValue uint16

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
		s.and, s.or, s.loopKw, s.forKw = tok("&&"), tok("||"), tok("loop"), tok("for")
		s.fAlternative, s.fBody, s.fCondition = field("alternative"), field("body"), field("condition")
		s.fConsequence, s.fFunction, s.fLeft, s.fName = field("consequence"), field("function"), field("left"), field("name")
		s.fOperator, s.fParameters, s.fPattern = field("operator"), field("parameters"), field("pattern")
		s.fRight, s.fType, s.fValue = field("right"), field("type"), field("value")
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
