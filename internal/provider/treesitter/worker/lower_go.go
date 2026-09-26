package worker

import (
	"sync"

	ts "github.com/tree-sitter/go-tree-sitter"

	"github.com/Sawmonabo/codectx/internal/provider/treesitter/flow"
)

// goLowering lowers Go functions, methods and function literals.
var goLowering = Lowering{
	language:  "go",
	callables: []string{"function_declaration", "method_declaration", "func_literal"},
	lower:     lowerGo,
}

// lowerGo lowers one Go callable's parameters and body into b.
//
// # Node granularity
//
// Nodes are rendered by their source text, so the spans below are part of
// the contract:
//
//   - After Entry, one Stmt node per named receiver, parameter and named
//     result, in declaration order, spanning the identifier and defining it.
//     A bare return uses every named result.
//   - A simple statement (expression, send, go, defer, inc/dec, assignment,
//     short variable declaration, a var spec) with at most one variable
//     target is one node spanning the statement (a var declaration of one
//     spec spans the declaration). `x++` uses then defines x on that node.
//   - A statement with k > 1 variable targets is k Stmt nodes, one per target,
//     spanning the target identifier and defining it. Each carries the uses
//     of its own value (its paired right-hand expression, or the whole right
//     side of a multi-value call) and, for `op=`, its target. Go evaluates
//     every operand before writing any target, and a node's uses are read
//     before its own definition only, so the nodes are ordered such that no
//     node reads a variable an earlier node of the statement wrote. Only a
//     true cycle (a swap) cannot be ordered: the target taken to break it
//     also uses its own variable, and a later node's read of that target
//     stays and resolves through its node, which read the old value before
//     writing it — a sound over-approximation. The uses of field, index and
//     indirect targets (`x.f`, `a[i]`, `*p`: uses of x, a, i, p) and of `_`
//     targets' values ride on the first node.
//   - A field, index or indirect target (`x.f = e`, `a[i] = e`, `*p = e`,
//     `x.f++`, `for a[i] = range xs`) writes part of its base variable: the
//     write uses the base and is a non-killing may-definition of it, so a
//     later use sees the write and every definition before it. Taking an
//     address (`&x`, `&x.f`, `&a[i]`) uses its operand's variables and
//     may-defines its base variable, by the address-taking rule of
//     Lowering. A statement's may-definitions ride on its last node.
//   - A condition (if, for, a case value list, a type-case type list) is one
//     Branch node spanning it. Every `&&`/`||` in an expression is hoisted
//     before the node that owns the expression: each operand that is not
//     itself `&&`/`||` becomes a Branch node carrying its uses, wired by
//     short-circuit (the left operand of && reaches the right one when true
//     and skips it when false; || the reverse), and the owning node, whose
//     value is computed from them, carries every use of the expression,
//     the operands' included.
//   - A for statement without a condition has a Branch head spanning the
//     `for` keyword; with one, the condition's first node is the head, the
//     target of the back edge. A range loop is a Stmt node for the range
//     expression, evaluated once, which defines an iteration variable of the
//     lowering's own; a Branch head spanning the `range` keyword; then one
//     node per key/value target. The head and every key/value node use the
//     iteration variable, so they depend on the range expression as it was
//     evaluated, never on a write to its variables in the body.
//   - An expression switch has a Stmt head for its tag, a type switch one
//     spanning `x := v.(type)` that defines the alias (one variable for all
//     clauses: no clause can reach another's uses); the head has one
//     successor, the first case condition. Every case condition (a case
//     value list, a type-case type list) is a Branch node that also uses the
//     tag's variables, since it compares against the tag. A select has a
//     Branch head spanning the `select` keyword that uses every clause's
//     channel operand and sent value, which Go evaluates on entering it.
//     Case conditions are tested in source order with default last; each
//     select clause's send or receive is one node. `select {}` blocks
//     forever and is lowered as a self-loop.
//   - return, break, continue, goto and fallthrough are Jump nodes spanning
//     the statement; `panic(...)` as a statement is a Jump node followed by
//     Throw. Every label is its own Stmt node spanning the label identifier,
//     before the statement it labels, so a goto always lands on it.
//
// Declarations of constants and types create no node; they only shadow.
//
// # Variables
//
// Scoping follows Go's blocks: the function block holds the receiver,
// parameters, results and the body's top level; if, for, switch and select
// statements and each case clause open implicit blocks. `:=` declares a new
// variable for each name not already declared in the innermost block (an
// inner-block `:=` shadows), after its right side is read. `_` is never a
// variable. An identifier is a use only when it resolves to a variable
// declared in the function being lowered; package-level names, constants
// and types are not. A composite-literal key that is a bare identifier
// resolving to a variable is a use (a map key is an expression; a struct
// field key cannot be told apart without types).
//
// A function literal is its own function, in which enclosing variables are
// free. In the enclosing function it is part of the expression that creates
// it, resolved with the literal's own declarations shadowing: the node owning
// that expression uses every enclosing variable the literal (or a literal
// nested in it) reads, and carries a non-killing may-definition of every
// enclosing variable it writes (by assignment, op=, ++/--, a range or
// receive target, through a field, index or pointer, or by taking its
// address). The literal may run
// at any later point, or never, so a use after the creating node sees that
// may-definition and every definition that reached the creating node.
// `defer func() {...}()` is therefore one node carrying the captures.
func lowerGo(l *Lowering, b *flow.Builder, fn *ts.Node, src []byte, s *Scratch) {
	cur := s.cursor(fn)
	g := &goLower{l: l, b: b, src: src, k: goSyntaxOf(), cur: cur}
	k := g.k
	g.open()
	if r := fn.ChildByFieldId(k.fReceiver); r != nil {
		g.params(r, false)
	}
	g.params(fn.ChildByFieldId(k.fParameters), false)
	if r := fn.ChildByFieldId(k.fResult); r != nil && r.KindId() == k.parameterList {
		g.params(r, true)
	}
	if body := fn.ChildByFieldId(k.fBody); body != nil {
		g.stmts(body)
	}
	g.close()
}

// goTarget is one variable target of a statement while its nodes are
// ordered: its identifier, its index among the statement's targets, its
// variable, and its uses in goLower.buf[lo:hi].
type goTarget struct {
	id     *ts.Node
	left   int
	v      int32
	lo, hi int
	isNew  bool
	done   bool
}

// goLower is the state of lowering one Go callable.
type goLower struct {
	l   *Lowering
	b   *flow.Builder
	src []byte
	k   *goSyntax
	// cur walks one node's children at a time; eachField resets it.
	cur *ts.TreeCursor
	// binds is the scope chain; marks are block starts.
	binds scope
	marks []int
	// results are the named result variables a bare return uses.
	results []int32
	// buf collects variable uses, and may the variables written through a
	// field, index or pointer or by a function literal, before they are
	// attached to a node.
	buf, may []int32
	// lefts, rights and targets are one statement's operands.
	lefts, rights []*ts.Node
	targets       []goTarget
	// hs holds the case-condition fringes of the open switches.
	hs []flow.Fringe
	// marking records the next node created in first.
	marking bool
	first   int32
}

func (g *goLower) span(n *ts.Node) flow.Span { return spanOf(n) }

func (g *goLower) text(n *ts.Node) []byte { return textOf(g.src, n) }

func (g *goLower) blank(name []byte) bool { return len(name) == 1 && name[0] == '_' }

// node creates a node of kind k spanning n.
func (g *goLower) node(k flow.Kind, n *ts.Node) int32 { return g.nodeSpan(k, g.span(n)) }

func (g *goLower) nodeSpan(k flow.Kind, s flow.Span) int32 {
	id := g.b.Node(k, s)
	if g.marking {
		g.first, g.marking = id, false
	}
	return id
}

// token is n's anonymous child of token kind id, or nil.
func (g *goLower) token(n *ts.Node, id uint16) *ts.Node {
	for i := range n.ChildCount() {
		if c := n.Child(i); c.KindId() == id {
			return c
		}
	}
	return nil
}

func (g *goLower) open() { g.marks = append(g.marks, len(g.binds)) }

func (g *goLower) close() {
	top := len(g.marks) - 1
	g.binds = g.binds[:g.marks[top]]
	g.marks = g.marks[:top]
}

// bind makes name resolve to v in the innermost block; _ binds nothing.
func (g *goLower) bind(name []byte, v int32) {
	if !g.blank(name) {
		g.binds = append(g.binds, binding{name: name, v: v})
	}
}

// declare declares identifier id as a new variable in the innermost block
// and returns it; _ declares nothing and yields -1.
func (g *goLower) declare(id *ts.Node) int32 {
	name := g.text(id)
	if g.blank(name) {
		return -1
	}
	v := g.b.Var()
	g.bind(name, v)
	return v
}

// bindNames binds every child of n in field "name" to no variable.
func (g *goLower) bindNames(n *ts.Node) {
	g.eachField(n, g.k.fName, func(c *ts.Node) { g.bind(g.text(c), -1) })
}

// eachField calls visit with every child of n in field, in source order.
// visit must not walk the tree itself: the walk holds g.cur.
func (g *goLower) eachField(n *ts.Node, field uint16, visit func(c *ts.Node)) {
	c := g.cur
	c.Reset(*n)
	for ok := c.GotoFirstChild(); ok; ok = c.GotoNextSibling() {
		if c.FieldId() == field {
			visit(c.Node())
		}
	}
}

// variable is the variable identifier id resolves to, or -1.
func (g *goLower) variable(id *ts.Node) int32 { return g.binds.lookup(g.text(id)) }

// declaredHere reports whether name is declared in the innermost block.
func (g *goLower) declaredHere(name []byte) bool {
	return g.binds.find(name, g.marks[len(g.marks)-1]) >= 0
}

// params declares every named parameter of list, each as a defining node.
func (g *goLower) params(list *ts.Node, result bool) {
	if list == nil {
		return
	}
	for i := range list.NamedChildCount() {
		g.eachField(list.NamedChild(i), g.k.fName, func(id *ts.Node) {
			v := g.declare(id)
			if v < 0 {
				return
			}
			g.b.Def(g.node(flow.Stmt, id), v)
			if result {
				g.results = append(g.results, v)
			}
		})
	}
}

func (g *goLower) isShort(n *ts.Node) bool {
	k := g.k
	if n.KindId() != k.binaryExpression {
		return false
	}
	op := n.ChildByFieldId(k.fOperator)
	return op != nil && (op.KindId() == k.and || op.KindId() == k.or)
}

// collect appends to buf every variable n reads, short-circuit operands
// included, and to may the base variable of every address n takes; it
// resolves a function literal's captures: its reads into buf, its writes into
// may.
func (g *goLower) collect(n *ts.Node) {
	switch {
	case n.KindId() == g.k.identifier:
		if v := g.variable(n); v >= 0 {
			g.buf = append(g.buf, v)
		}
	case g.l.isCallable(n):
		g.scan(n, len(g.binds))
	case g.isAddress(n):
		if e := n.ChildByFieldId(g.k.fOperand); e != nil {
			g.collect(e)
			if v := g.baseVar(e); v >= 0 {
				g.may = append(g.may, v)
			}
		}
	default:
		for i := range n.NamedChildCount() {
			g.collect(n.NamedChild(i))
		}
	}
}

// uses records every variable e reads as a use of node id, and every
// enclosing variable a function literal in e writes as a may-definition.
func (g *goLower) uses(id int32, e *ts.Node) {
	lo, mlo := len(g.buf), len(g.may)
	if e != nil {
		g.collect(e)
	}
	g.useAll(id, lo, len(g.buf))
	g.mayAll(id, mlo)
	g.buf = g.buf[:lo]
}

// useAll records buf[lo:hi] as uses of node id.
func (g *goLower) useAll(id int32, lo, hi int) {
	for _, v := range g.buf[lo:hi] {
		g.b.Use(id, v)
	}
}

// mayAll records may[lo:] as may-definitions of node id and truncates may
// to lo.
func (g *goLower) mayAll(id int32, lo int) {
	for _, v := range g.may[lo:] {
		g.b.MayDef(id, v)
	}
	g.may = g.may[:lo]
}

// baseIdent is the identifier a field, index or indirect target writes
// through (x in `x.f`, `x[i].f`, `*x`, `(*x).f`), or nil.
func (g *goLower) baseIdent(n *ts.Node) *ts.Node {
	k := g.k
	for n = g.l.unparen(n); n != nil; n = g.l.unparen(n) {
		switch n.KindId() {
		case k.identifier:
			return n
		case k.selectorExpression, k.indexExpression:
			n = n.ChildByFieldId(k.fOperand)
		case k.unaryExpression:
			if op := n.ChildByFieldId(k.fOperator); op == nil || op.KindId() != k.star {
				return nil
			}
			n = n.ChildByFieldId(k.fOperand)
		default:
			return nil
		}
	}
	return nil
}

// isAddress reports whether n takes an address, `&e`.
func (g *goLower) isAddress(n *ts.Node) bool {
	if n.KindId() != g.k.unaryExpression {
		return false
	}
	op := n.ChildByFieldId(g.k.fOperator)
	return op != nil && op.KindId() == g.k.amp
}

// baseVar is the variable a field, index or indirect target n writes
// through, or -1.
func (g *goLower) baseVar(n *ts.Node) int32 {
	if id := g.baseIdent(n); id != nil {
		return g.variable(id)
	}
	return -1
}

// hoist lowers every outermost && and || under n, in source order, into
// operand nodes, leaving the fringe where the owning node begins.
func (g *goLower) hoist(n *ts.Node) {
	switch {
	case n == nil || g.l.isCallable(n):
	case g.isShort(n):
		first, t, f := g.chain(n)
		g.b.Restore(t)
		g.b.Merge(f)
		g.b.Pop(first)
	default:
		for i := range n.NamedChildCount() {
			g.hoist(n.NamedChild(i))
		}
	}
}

// chain lowers the short-circuit expression n and returns saved fringes: t
// where n is true, f where it is false, and first, the earliest handle it
// pushed, which releases them all.
func (g *goLower) chain(n *ts.Node) (first, t, f flow.Fringe) {
	n = g.l.unparen(n)
	if !g.isShort(n) {
		g.hoist(n)
		g.uses(g.node(flow.Branch, n), n)
		t = g.b.Push()
		return t, t, t
	}
	k := g.k
	and := n.ChildByFieldId(k.fOperator).KindId() == k.and
	first, lt, lf := g.chain(n.ChildByFieldId(k.fLeft))
	if and {
		g.b.Restore(lt)
	} else {
		g.b.Restore(lf)
	}
	_, rt, rf := g.chain(n.ChildByFieldId(k.fRight))
	if and {
		g.b.Restore(lf)
		g.b.Merge(rf)
		return first, rt, g.b.Push()
	}
	g.b.Restore(lt)
	g.b.Merge(rt)
	return first, g.b.Push(), rf
}

// cond lowers condition e as one Branch node after its hoisted operands and
// returns the node.
func (g *goLower) cond(e *ts.Node) int32 {
	g.hoist(e)
	id := g.node(flow.Branch, e)
	g.uses(id, e)
	return id
}

// block lowers a braced block in its own scope.
func (g *goLower) block(n *ts.Node) {
	g.open()
	g.stmts(n)
	g.close()
}

// stmts lowers the statement list under n (a block or a case clause) and
// reports whether it ends with fallthrough.
func (g *goLower) stmts(n *ts.Node) (fell bool) {
	k := g.k
	for i := range n.NamedChildCount() {
		list := n.NamedChild(i)
		if list.KindId() != k.statementList {
			continue
		}
		for j := range list.NamedChildCount() {
			s := list.NamedChild(j)
			if s.KindId() == k.comment {
				continue
			}
			g.stmt(s, nil)
			fell = s.KindId() == k.fallthroughStatement
		}
	}
	return fell
}

// label is the label a break, continue or goto names, or "".
func (g *goLower) label(s *ts.Node) string {
	for i := range s.NamedChildCount() {
		if c := s.NamedChild(i); c.KindId() == g.k.labelName {
			return view(g.text(c))
		}
	}
	return ""
}

// stmt lowers statement s; labels are the labels naming it.
func (g *goLower) stmt(s *ts.Node, labels []string) {
	b, k := g.b, g.k
	switch s.KindId() {
	case k.comment, k.emptyStatement:
	case k.block:
		g.block(s)
	case k.expressionStatement:
		e := firstNamed(s)
		g.hoist(e)
		if g.isPanic(e) {
			g.uses(g.node(flow.Jump, s), e)
			b.Throw()
			return
		}
		g.uses(g.node(flow.Stmt, s), e)
	case k.sendStatement, k.goStatement, k.deferStatement:
		g.hoist(s)
		g.uses(g.node(flow.Stmt, s), s)
	case k.incStatement, k.decStatement:
		e := firstNamed(s)
		g.hoist(e)
		id := g.node(flow.Stmt, s)
		g.uses(id, e)
		if x := g.l.unparen(e); x.KindId() == k.identifier {
			if v := g.variable(x); v >= 0 {
				b.Def(id, v)
			}
		} else if v := g.baseVar(x); v >= 0 {
			b.MayDef(id, v)
		}
	case k.assignmentStatement:
		g.fill(s.ChildByFieldId(k.fLeft), 0)
		g.assign(s, s.ChildByFieldId(k.fRight), s.ChildByFieldId(k.fOperator).KindId() != k.assign, false)
	case k.shortVarDeclaration:
		g.fill(s.ChildByFieldId(k.fLeft), 0)
		g.assign(s, s.ChildByFieldId(k.fRight), false, true)
	case k.varDeclaration:
		for i := range s.NamedChildCount() {
			switch c := s.NamedChild(i); c.KindId() {
			case k.varSpec:
				g.fill(c, k.fName)
				g.assign(s, c.ChildByFieldId(k.fValue), false, true)
			case k.varSpecList:
				for j := range c.NamedChildCount() {
					if spec := c.NamedChild(j); spec.KindId() == k.varSpec {
						g.fill(spec, k.fName)
						g.assign(spec, spec.ChildByFieldId(k.fValue), false, true)
					}
				}
			}
		}
	case k.constDeclaration, k.typeDeclaration:
		for i := range s.NamedChildCount() {
			g.bindNames(s.NamedChild(i))
		}
	case k.returnStatement:
		g.hoist(s)
		id := g.node(flow.Jump, s)
		g.uses(id, s)
		if firstNamed(s) == nil {
			for _, v := range g.results {
				b.Use(id, v)
			}
		}
		b.Return()
	case k.breakStatement:
		g.node(flow.Jump, s)
		b.Break(g.label(s))
	case k.continueStatement:
		g.node(flow.Jump, s)
		b.Continue(g.label(s))
	case k.gotoStatement:
		g.node(flow.Jump, s)
		b.Goto(g.label(s))
	case k.fallthroughStatement:
		// The enclosing switch carries the fringe into the next clause.
		g.node(flow.Jump, s)
	case k.labeledStatement:
		g.labeled(s, labels)
	case k.ifStatement:
		g.ifStmt(s)
	case k.forStatement:
		g.forStmt(s, labels)
	case k.expressionSwitchStatement:
		g.switchStmt(s, labels, false)
	case k.typeSwitchStatement:
		g.switchStmt(s, labels, true)
	case k.selectStatement:
		g.selectStmt(s, labels)
	default:
		// A statement the parser could not recognise keeps its reads.
		g.hoist(s)
		g.uses(g.node(flow.Stmt, s), s)
	}
}

// isPanic reports whether e calls the predeclared panic.
func (g *goLower) isPanic(e *ts.Node) bool {
	e = g.l.unparen(e)
	if e == nil || e.KindId() != g.k.callExpression {
		return false
	}
	f := e.ChildByFieldId(g.k.fFunction)
	return f != nil && f.KindId() == g.k.identifier && string(g.text(f)) == "panic" && g.binds.find(g.text(f), 0) < 0
}

// labeled lowers a labelled statement. Go's break and continue name only an
// enclosing for, switch or select, so the labels reach those frames; every
// label is also its own node, the target of goto.
func (g *goLower) labeled(s *ts.Node, labels []string) {
	k := g.k
	id := s.ChildByFieldId(k.fLabel)
	name := view(g.text(id))
	labels = append(labels, name)
	g.b.Label(name, g.span(id))
	var inner *ts.Node
	for i := range s.NamedChildCount() {
		if c := s.NamedChild(i); c.KindId() != k.labelName && c.KindId() != k.comment {
			inner = c
			break
		}
	}
	if inner != nil && inner.KindId() != k.emptyStatement {
		g.stmt(inner, labels)
	}
}

func (g *goLower) ifStmt(s *ts.Node) {
	b, k := g.b, g.k
	g.open()
	if init := s.ChildByFieldId(k.fInitializer); init != nil {
		g.stmt(init, nil)
	}
	g.cond(s.ChildByFieldId(k.fCondition))
	p := b.Push()
	g.block(s.ChildByFieldId(k.fConsequence))
	t := b.Push()
	b.Restore(p)
	if alt := s.ChildByFieldId(k.fAlternative); alt != nil {
		g.stmt(alt, nil)
	}
	b.Merge(t)
	b.Pop(p)
	g.close()
}

func (g *goLower) forStmt(s *ts.Node, labels []string) {
	b, k := g.b, g.k
	body := s.ChildByFieldId(k.fBody)
	var clause, cond, update *ts.Node
	for i := range s.NamedChildCount() {
		if c := s.NamedChild(i); c.KindId() != k.block && c.KindId() != k.comment {
			clause = c
			break
		}
	}
	g.open()
	switch {
	case clause == nil:
	case clause.KindId() == k.rangeClause:
		g.rangeLoop(clause, body, labels)
		g.close()
		return
	case clause.KindId() == k.forClause:
		if init := clause.ChildByFieldId(k.fInitializer); init != nil {
			g.stmt(init, nil)
		}
		cond, update = clause.ChildByFieldId(k.fCondition), clause.ChildByFieldId(k.fUpdate)
	default:
		cond = clause
	}
	f := b.OpenLoop(labels...)
	var head int32
	var exit flow.Fringe
	if cond != nil {
		g.marking = true
		g.cond(cond)
		head = g.first
		exit = b.Push()
	} else {
		head = g.node(flow.Branch, s.Child(0))
	}
	g.block(body)
	b.ContinueHere(f)
	if update != nil {
		g.stmt(update, nil)
	}
	b.Close(head)
	if cond != nil {
		b.Restore(exit)
	}
	b.CloseFrame(f)
	if cond != nil {
		b.Pop(exit)
	}
	g.close()
}

// rangeLoop lowers `for k, v := range x`: x once, then a head, then the
// key and value definitions each iteration. The range expression's node
// defines an iteration variable of its own that the head and every key and
// value node use, so each depends on x as it was evaluated before the loop,
// never on a definition of x in the body.
func (g *goLower) rangeLoop(rc, body *ts.Node, labels []string) {
	b, k := g.b, g.k
	right := rc.ChildByFieldId(k.fRight)
	g.hoist(right)
	rangeNode := g.node(flow.Stmt, right)
	g.uses(rangeNode, right)
	iter := b.Var()
	b.Def(rangeNode, iter)
	f := b.OpenLoop(labels...)
	head := g.node(flow.Branch, g.token(rc, k.rangeKw))
	b.Use(head, iter)
	exit := b.Push()
	if left := rc.ChildByFieldId(k.fLeft); left != nil {
		define := g.token(rc, k.define) != nil
		for i := range left.NamedChildCount() {
			t := left.NamedChild(i)
			x := g.l.unparen(t)
			if t.KindId() == k.comment || (x.KindId() == k.identifier && g.blank(g.text(x))) {
				continue
			}
			var id int32
			switch {
			case define:
				v := g.declare(x)
				id = g.node(flow.Stmt, x)
				b.Def(id, v)
			case x.KindId() == k.identifier:
				id = g.node(flow.Stmt, x)
				if v := g.variable(x); v >= 0 {
					b.Def(id, v)
				}
			default:
				g.hoist(t)
				id = g.node(flow.Stmt, t)
				g.uses(id, t)
				if v := g.baseVar(x); v >= 0 {
					b.MayDef(id, v)
				}
			}
			b.Use(id, iter)
		}
	}
	g.block(body)
	b.ContinueHere(f)
	b.Close(head)
	b.Restore(exit)
	b.CloseFrame(f)
	b.Pop(exit)
}

// switchStmt lowers an expression or type switch: its head, then each case
// condition in source order (default last), then the clause bodies, each
// entered from its condition and from a fallthrough out of the clause before.
// The head evaluates the tag once and has one successor, the first case
// condition, so it is a Stmt; every case condition compares against the tag
// and uses the tag's variables. No variable is defined between the head and
// the conditions (a function literal's write there is a may-definition, which
// kills nothing), so each condition sees the tag's variables exactly as the
// head read them.
func (g *goLower) switchStmt(s *ts.Node, labels []string, typed bool) {
	b, k := g.b, g.k
	g.open()
	if init := s.ChildByFieldId(k.fInitializer); init != nil {
		g.stmt(init, nil)
	}
	// The tag's uses stay in buf[tag:tagEnd] until every case condition has
	// them. They are read before a type switch declares its alias, so
	// `switch x := x.(type)` reads the outer x.
	tag := len(g.buf)
	if value := s.ChildByFieldId(k.fValue); value != nil {
		g.hoist(value)
		mlo := len(g.may)
		g.collect(value)
		var head int32
		if typed {
			head = g.typeSwitchHead(s, value, tag)
		} else {
			head = g.node(flow.Stmt, value)
			g.useAll(head, tag, len(g.buf))
		}
		g.mayAll(head, mlo)
	}
	tagEnd := len(g.buf)
	f := b.OpenSwitch(labels...)
	base := len(g.hs)
	for i := range s.NamedChildCount() {
		var id int32
		switch c := s.NamedChild(i); c.KindId() {
		case k.expressionCase:
			id = g.cond(c.ChildByFieldId(k.fValue))
		case k.typeCase:
			id = g.nodeSpan(flow.Branch, g.fieldSpan(c, k.fType))
		default:
			continue
		}
		g.useAll(id, tag, tagEnd)
		g.hs = append(g.hs, b.Push())
	}
	g.buf = g.buf[:tag]
	dflt := b.Push()
	first := dflt
	if len(g.hs) > base {
		first = g.hs[base]
	}
	var ft flow.Fringe
	fell, hasDefault, next := false, false, base
	for i := range s.NamedChildCount() {
		c := s.NamedChild(i)
		switch c.KindId() {
		case k.expressionCase, k.typeCase:
			b.Restore(g.hs[next])
			next++
		case k.defaultCase:
			b.Restore(dflt)
			hasDefault = true
		default:
			continue
		}
		if fell {
			b.Merge(ft)
		}
		g.open()
		fell = g.stmts(c)
		g.close()
		if fell {
			ft = b.Push()
		} else {
			b.Break("")
		}
	}
	switch {
	case !hasDefault:
		b.Restore(dflt)
	case fell:
		b.Break("")
	}
	b.CloseFrame(f)
	b.Pop(first)
	g.hs = g.hs[:base]
	g.close()
}

// typeSwitchHead creates the Stmt node spanning `x := v.(type)`, which uses
// v's variables, buf[tag:], and then declares and defines the alias x.
func (g *goLower) typeSwitchHead(s, value *ts.Node, tag int) int32 {
	start, end := value.StartByte(), value.EndByte()
	k := g.k
	alias := s.ChildByFieldId(k.fAlias)
	if alias != nil {
		start = alias.StartByte()
	}
	for i := range s.ChildCount() {
		if c := s.Child(i); c.KindId() == k.typeKw {
			end = c.EndByte()
			if rp := s.Child(i + 1); rp != nil && rp.KindId() == k.rparen {
				end = rp.EndByte()
			}
			break
		}
	}
	head := g.nodeSpan(flow.Stmt, flow.Span{Start: uint32(start), End: uint32(end)})
	g.useAll(head, tag, len(g.buf))
	if alias == nil {
		return head
	}
	if id := firstNamed(alias); id != nil && id.KindId() == k.identifier {
		if v := g.declare(id); v >= 0 {
			g.b.Def(head, v)
		}
	}
	return head
}

// fieldSpan spans every child of n in field, or n when there is none.
func (g *goLower) fieldSpan(n *ts.Node, field uint16) flow.Span {
	s := g.span(n)
	found := false
	g.eachField(n, field, func(c *ts.Node) {
		if !found {
			s.Start = uint32(c.StartByte())
			found = true
		}
		s.End = uint32(c.EndByte())
	})
	return s
}

// selectStmt lowers a select: a head reaching every clause, each clause its
// communication node then its body. Go evaluates every clause's channel
// operand and every send's value once, in source order, on entering the
// select, and the choice among the clauses depends on them, so the head uses
// them all.
func (g *goLower) selectStmt(s *ts.Node, labels []string) {
	b, k := g.b, g.k
	f := b.OpenSwitch(labels...)
	head := g.node(flow.Branch, s.Child(0))
	lo, mlo := len(g.buf), len(g.may)
	for i := range s.NamedChildCount() {
		c := s.NamedChild(i)
		if c.KindId() != k.communicationCase {
			continue
		}
		switch comm := c.ChildByFieldId(k.fCommunication); {
		case comm == nil:
		case comm.KindId() == k.receiveStatement:
			if right := comm.ChildByFieldId(k.fRight); right != nil {
				g.collect(right)
			}
		default:
			g.collect(comm)
		}
	}
	g.useAll(head, lo, len(g.buf))
	g.mayAll(head, mlo)
	g.buf = g.buf[:lo]
	d := b.Push()
	clauses := false
	for i := range s.NamedChildCount() {
		c := s.NamedChild(i)
		if c.KindId() != k.communicationCase && c.KindId() != k.defaultCase {
			continue
		}
		clauses = true
		b.Restore(d)
		g.open()
		if comm := c.ChildByFieldId(k.fCommunication); comm != nil {
			g.comm(comm)
		}
		g.stmts(c)
		g.close()
		b.Break("")
	}
	if !clauses {
		b.Close(head)
	}
	b.CloseFrame(f)
	b.Pop(d)
}

// comm lowers a select clause's send or receive.
func (g *goLower) comm(c *ts.Node) {
	k := g.k
	if c.KindId() == k.receiveStatement {
		if left := c.ChildByFieldId(k.fLeft); left != nil {
			g.fill(left, 0)
			g.assign(c, c.ChildByFieldId(k.fRight), false, g.token(c, k.define) != nil)
			return
		}
	}
	g.hoist(c)
	g.uses(g.node(flow.Stmt, c), c)
}

// fill sets lefts to n's children in field, or to its named children other
// than comments when field is 0, the id of no field.
func (g *goLower) fill(n *ts.Node, field uint16) {
	g.lefts = g.lefts[:0]
	if field != 0 {
		g.eachField(n, field, func(c *ts.Node) { g.lefts = append(g.lefts, c) })
		return
	}
	for i := range n.ChildCount() {
		if c := n.Child(i); c.IsNamed() && c.KindId() != g.k.comment {
			g.lefts = append(g.lefts, c)
		}
	}
}

// assign lowers a statement assigning right (an expression list, one
// expression, or nil) to lefts; compound targets are also read, define
// declares every target not declared in the innermost block. A field, index
// or indirect target is read and may-defines its base variable; every
// may-definition of the statement rides on its last node, where every target
// has been written.
func (g *goLower) assign(whole, right *ts.Node, compound, define bool) {
	b := g.b
	g.rights = g.rights[:0]
	switch {
	case right == nil:
	case right.KindId() == g.k.expressionList:
		for i := range right.NamedChildCount() {
			if c := right.NamedChild(i); c.KindId() != g.k.comment {
				g.rights = append(g.rights, c)
			}
		}
	default:
		g.rights = append(g.rights, right)
	}
	for _, n := range g.lefts {
		g.hoist(n)
	}
	for _, n := range g.rights {
		g.hoist(n)
	}
	paired := len(g.rights) == len(g.lefts)
	lo, mlo := len(g.buf), len(g.may)
	// Non-variable targets' operands and values come first: the statement's
	// first node carries them.
	g.targets = g.targets[:0]
	for i, n := range g.lefts {
		x := g.l.unparen(n)
		t := goTarget{id: x, left: i, v: -1}
		if x.KindId() == g.k.identifier {
			switch name := g.text(x); {
			case g.blank(name):
			case define && !g.declaredHere(name):
				t.isNew = true
			default:
				t.v = g.variable(x)
			}
		} else {
			g.collect(n)
			if v := g.baseVar(x); v >= 0 {
				g.may = append(g.may, v)
			}
		}
		if t.isNew || t.v >= 0 {
			g.targets = append(g.targets, t)
		} else if paired {
			g.collect(g.rights[i])
		}
	}
	extra := len(g.buf)
	if !paired {
		for _, n := range g.rights {
			g.collect(n)
		}
	}
	shared := len(g.buf)
	for i := range g.targets {
		t := &g.targets[i]
		if !paired {
			t.lo, t.hi = extra, shared
			continue
		}
		t.lo = len(g.buf)
		g.collect(g.rights[t.left])
		if compound {
			g.buf = append(g.buf, t.v)
		}
		t.hi = len(g.buf)
	}
	for i := range g.targets {
		if t := &g.targets[i]; t.isNew {
			t.v = g.declare(t.id)
		}
	}
	if len(g.targets) <= 1 {
		id := g.node(flow.Stmt, whole)
		g.useAll(id, lo, len(g.buf))
		if len(g.targets) == 1 {
			b.Def(id, g.targets[0].v)
		}
		g.mayAll(id, mlo)
		g.buf = g.buf[:lo]
		return
	}
	g.order(lo, extra, mlo)
	g.buf = g.buf[:lo]
}

// order emits one node per variable target, keeping the statement's
// invariant: every target node reads the right-hand values as they were
// before any target of the statement was written. It repeatedly takes the
// first remaining target no other remaining target reads, so no node reads
// a variable an earlier node wrote. In a cycle it takes the first remaining
// target and gives its node a use of its own variable: that use is read
// before the node's definition, so the remaining targets' reads of the
// variable, which stay, resolve to this node and through it to the old
// value. buf[lo:extra] are the first node's own uses; may[mlo:] are the last
// node's may-definitions.
func (g *goLower) order(lo, extra, mlo int) {
	b := g.b
	for n := range g.targets {
		pick, cycle := -1, false
		for i := range g.targets {
			if !g.targets[i].done && !g.readByOther(i) {
				pick = i
				break
			}
		}
		if pick < 0 {
			for i := range g.targets {
				if !g.targets[i].done {
					pick, cycle = i, true
					break
				}
			}
		}
		t := &g.targets[pick]
		t.done = true
		id := g.node(flow.Stmt, t.id)
		if n == 0 {
			g.useAll(id, lo, extra)
		}
		g.useAll(id, t.lo, t.hi)
		if cycle {
			b.Use(id, t.v)
		}
		b.Def(id, t.v)
		if n == len(g.targets)-1 {
			g.mayAll(id, mlo)
		}
	}
}

// readByOther reports whether a remaining target other than i reads
// target i's variable.
func (g *goLower) readByOther(i int) bool {
	v := g.targets[i].v
	for j := range g.targets {
		t := &g.targets[j]
		if j == i || t.done {
			continue
		}
		for _, u := range g.buf[t.lo:t.hi] {
			if u == v {
				return true
			}
		}
	}
	return false
}

// scan resolves a function literal's captures: every identifier under n
// that resolves to a binding below base (the enclosing function's) and is a
// variable, with the literal's own declarations shadowing. A capture read
// goes to buf; a capture written (an assignment, ++/-- or range target, the
// base of a field, index or indirect target, or the base of an address the
// literal takes) goes to may, and to buf as well when the write also reads
// it (op=, ++/--, and an address-taking, which reads its operand).
func (g *goLower) scan(n *ts.Node, base int) {
	k := g.k
	if g.l.isCallable(n) {
		g.open()
		for _, field := range [...]uint16{k.fParameters, k.fResult} {
			if list := n.ChildByFieldId(field); list != nil && list.KindId() == k.parameterList {
				for i := range list.NamedChildCount() {
					g.bindNames(list.NamedChild(i))
				}
			}
		}
		if body := n.ChildByFieldId(k.fBody); body != nil {
			g.scan(body, base)
		}
		g.close()
		return
	}
	switch n.KindId() {
	case k.identifier:
		if v := g.captured(n, base); v >= 0 {
			g.buf = append(g.buf, v)
		}
		return
	case k.assignmentStatement:
		op := n.ChildByFieldId(k.fOperator)
		read := op != nil && op.KindId() != k.assign
		if left := n.ChildByFieldId(k.fLeft); left != nil {
			for i := range left.NamedChildCount() {
				g.scanWrite(left.NamedChild(i), base, read)
			}
		}
		if right := n.ChildByFieldId(k.fRight); right != nil {
			g.scan(right, base)
		}
		return
	case k.incStatement, k.decStatement:
		if e := firstNamed(n); e != nil {
			g.scanWrite(e, base, true)
		}
		return
	case k.unaryExpression:
		if g.isAddress(n) {
			if e := n.ChildByFieldId(k.fOperand); e != nil {
				g.scanWrite(e, base, true)
			}
			return
		}
	case k.block, k.ifStatement, k.forStatement, k.expressionSwitchStatement, k.selectStatement,
		k.expressionCase, k.defaultCase, k.typeCase, k.communicationCase:
		g.open()
		g.scanChildren(n, base)
		g.close()
		return
	case k.typeSwitchStatement:
		g.open()
		for _, field := range [...]uint16{k.fInitializer, k.fValue} {
			if c := n.ChildByFieldId(field); c != nil {
				g.scan(c, base)
			}
		}
		if alias := n.ChildByFieldId(k.fAlias); alias != nil {
			for i := range alias.NamedChildCount() {
				g.bind(g.text(alias.NamedChild(i)), -1)
			}
		}
		for i := range n.NamedChildCount() {
			if c := n.NamedChild(i); c.KindId() == k.typeCase || c.KindId() == k.defaultCase {
				g.scan(c, base)
			}
		}
		g.close()
		return
	case k.shortVarDeclaration:
		g.scan(n.ChildByFieldId(k.fRight), base)
		left := n.ChildByFieldId(k.fLeft)
		for i := range left.NamedChildCount() {
			if id := left.NamedChild(i); id.KindId() == k.identifier && !g.declaredHere(g.text(id)) {
				g.bind(g.text(id), -1)
			}
		}
		return
	case k.varSpec:
		if value := n.ChildByFieldId(k.fValue); value != nil {
			g.scan(value, base)
		}
		g.bindNames(n)
		return
	case k.constSpec, k.typeSpec, k.typeAlias:
		g.bindNames(n)
		return
	case k.rangeClause, k.receiveStatement:
		left := n.ChildByFieldId(k.fLeft)
		if left == nil {
			break
		}
		if right := n.ChildByFieldId(k.fRight); right != nil {
			g.scan(right, base)
		}
		define := g.token(n, k.define) != nil
		for i := range left.NamedChildCount() {
			if define {
				g.bind(g.text(left.NamedChild(i)), -1)
			} else {
				g.scanWrite(left.NamedChild(i), base, false)
			}
		}
		return
	}
	g.scanChildren(n, base)
}

// captured is the enclosing variable identifier id resolves to inside a
// function literal whose own bindings start at base, or -1.
func (g *goLower) captured(id *ts.Node, base int) int32 {
	if i := g.binds.find(g.text(id), 0); i >= 0 && i < base {
		return g.binds[i].v
	}
	return -1
}

// scanWrite resolves target t of a write inside a function literal: a
// captured variable t names is written, and read too when read is set; a
// field, index or indirect target reads its operands and writes its captured
// base.
func (g *goLower) scanWrite(t *ts.Node, base int, read bool) {
	if t.KindId() == g.k.comment {
		return
	}
	if x := g.l.unparen(t); x.KindId() == g.k.identifier {
		if v := g.captured(x, base); v >= 0 {
			if read {
				g.buf = append(g.buf, v)
			}
			g.may = append(g.may, v)
		}
		return
	}
	g.scan(t, base)
	if id := g.baseIdent(t); id != nil {
		if v := g.captured(id, base); v >= 0 {
			g.may = append(g.may, v)
		}
	}
}

func (g *goLower) scanChildren(n *ts.Node, base int) {
	for i := range n.NamedChildCount() {
		g.scan(n.NamedChild(i), base)
	}
}

// goSyntax holds the kind and field ids the Go lowering matches, resolved
// once by name against the pinned grammar so the walk compares integers
// rather than converting every node's kind or field to a string.
type goSyntax struct {
	identifier, parameterList, binaryExpression, unaryExpression, selectorExpression, indexExpression,
	callExpression, expressionList, comment, statementList, block, labelName, emptyStatement,
	expressionStatement, sendStatement, goStatement, deferStatement, incStatement, decStatement,
	assignmentStatement, shortVarDeclaration, varDeclaration, varSpec, varSpecList, constDeclaration,
	typeDeclaration, constSpec, typeSpec, typeAlias, returnStatement, breakStatement, continueStatement,
	gotoStatement, fallthroughStatement, labeledStatement, ifStatement, forStatement, forClause, rangeClause,
	expressionSwitchStatement, typeSwitchStatement, expressionCase, typeCase, defaultCase, selectStatement,
	communicationCase, receiveStatement uint16

	and, or, star, amp, assign, define, rangeKw, typeKw, rparen uint16

	fAlias, fAlternative, fBody, fCommunication, fCondition, fConsequence, fFunction, fInitializer, fLabel,
	fLeft, fName, fOperand, fOperator, fParameters, fReceiver, fResult, fRight, fType, fUpdate, fValue uint16
}

var (
	goSyntaxOnce  sync.Once
	goSyntaxTable *goSyntax
)

// goSyntaxOf resolves the table once. A name the grammar does not define is
// a lowering defect and panics, so a misspelt kind can never silently match
// nothing.
func goSyntaxOf() *goSyntax {
	goSyntaxOnce.Do(func() {
		const language = "go"
		tl := mustGrammar(language)
		kind := func(name string) uint16 { return mustKind(tl, language, name, true) }
		tok := func(name string) uint16 { return mustKind(tl, language, name, false) }
		field := func(name string) uint16 { return mustField(tl, language, name) }
		s := &goSyntax{}
		s.identifier, s.parameterList, s.binaryExpression = kind("identifier"), kind("parameter_list"), kind("binary_expression")
		s.unaryExpression, s.selectorExpression, s.indexExpression = kind("unary_expression"), kind("selector_expression"), kind("index_expression")
		s.callExpression, s.expressionList, s.comment = kind("call_expression"), kind("expression_list"), kind("comment")
		s.statementList, s.block, s.labelName, s.emptyStatement = kind("statement_list"), kind("block"), kind("label_name"), kind("empty_statement")
		s.expressionStatement, s.sendStatement, s.goStatement = kind("expression_statement"), kind("send_statement"), kind("go_statement")
		s.deferStatement, s.incStatement, s.decStatement = kind("defer_statement"), kind("inc_statement"), kind("dec_statement")
		s.assignmentStatement, s.shortVarDeclaration = kind("assignment_statement"), kind("short_var_declaration")
		s.varDeclaration, s.varSpec, s.varSpecList = kind("var_declaration"), kind("var_spec"), kind("var_spec_list")
		s.constDeclaration, s.typeDeclaration = kind("const_declaration"), kind("type_declaration")
		s.constSpec, s.typeSpec, s.typeAlias = kind("const_spec"), kind("type_spec"), kind("type_alias")
		s.returnStatement, s.breakStatement, s.continueStatement = kind("return_statement"), kind("break_statement"), kind("continue_statement")
		s.gotoStatement, s.fallthroughStatement, s.labeledStatement = kind("goto_statement"), kind("fallthrough_statement"), kind("labeled_statement")
		s.ifStatement, s.forStatement, s.forClause, s.rangeClause = kind("if_statement"), kind("for_statement"), kind("for_clause"), kind("range_clause")
		s.expressionSwitchStatement, s.typeSwitchStatement = kind("expression_switch_statement"), kind("type_switch_statement")
		s.expressionCase, s.typeCase, s.defaultCase = kind("expression_case"), kind("type_case"), kind("default_case")
		s.selectStatement, s.communicationCase, s.receiveStatement = kind("select_statement"), kind("communication_case"), kind("receive_statement")
		s.and, s.or, s.star, s.amp, s.assign = tok("&&"), tok("||"), tok("*"), tok("&"), tok("=")
		s.define, s.rangeKw, s.typeKw, s.rparen = tok(":="), tok("range"), tok("type"), tok(")")
		s.fAlias, s.fAlternative, s.fBody, s.fCommunication = field("alias"), field("alternative"), field("body"), field("communication")
		s.fCondition, s.fConsequence, s.fFunction = field("condition"), field("consequence"), field("function")
		s.fInitializer, s.fLabel, s.fLeft, s.fName = field("initializer"), field("label"), field("left"), field("name")
		s.fOperand, s.fOperator, s.fParameters = field("operand"), field("operator"), field("parameters")
		s.fReceiver, s.fResult, s.fRight = field("receiver"), field("result"), field("right")
		s.fType, s.fUpdate, s.fValue = field("type"), field("update"), field("value")
		goSyntaxTable = s
	})
	return goSyntaxTable
}
