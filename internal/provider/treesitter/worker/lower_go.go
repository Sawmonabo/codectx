package worker

import (
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
//     writing it — a sound over-approximation. Uses of
//     field, index and indirect targets (`x.f`, `a[i]`, `*p`: uses of x, a,
//     i, p, never a definition) and of `_` targets' values ride on the first
//     node.
//   - A condition (if, for, a case value list, a type-case type list) is one
//     Branch node spanning it. Every `&&`/`||` in an expression is hoisted
//     before the node that owns the expression: each operand that is not
//     itself `&&`/`||` becomes a Branch node carrying its uses, wired by
//     short-circuit (the left operand of && reaches the right one when true
//     and skips it when false; || the reverse), and the owning node carries
//     the remaining uses.
//   - A for statement without a condition has a Branch head spanning the
//     `for` keyword; with one, the condition's first node is the head, the
//     target of the back edge. A range loop is a Stmt node for the range
//     expression (evaluated once), a Branch head spanning the `range`
//     keyword, then one node per key/value target.
//   - An expression switch has a Branch head for its tag, a type switch one
//     spanning `x := v.(type)` that defines the alias (one variable for all
//     clauses: no clause can reach another's uses), a select one spanning
//     the `select` keyword. Case conditions are tested in source order with
//     default last; each select clause's send or receive is one node.
//     `select {}` blocks forever and is lowered as a self-loop.
//   - return, break, continue, goto and fallthrough are Jump nodes spanning
//     the statement; `panic(...)` as a statement is a Jump node followed by
//     Throw. A labelled statement with no statement is a Stmt node spanning
//     it, so a goto to it has a target.
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
// it: the node owning that expression uses every enclosing variable the
// literal (or a literal nested in it) reads or writes, resolved with the
// literal's own declarations shadowing. `defer func() {...}()` is therefore
// one node carrying the captures.
func lowerGo(l *Lowering, b *flow.Builder, fn *ts.Node, src []byte) {
	g := &goLower{l: l, b: b, src: src}
	g.open()
	if r := fn.ChildByFieldName("receiver"); r != nil {
		g.params(r, false)
	}
	g.params(fn.ChildByFieldName("parameters"), false)
	if r := fn.ChildByFieldName("result"); r != nil && r.Kind() == "parameter_list" {
		g.params(r, true)
	}
	if body := fn.ChildByFieldName("body"); body != nil {
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
	// binds is the scope chain; marks are block starts.
	binds scope
	marks []int
	// results are the named result variables a bare return uses.
	results []int32
	// buf collects variable uses before they are attached to a node.
	buf []int32
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

// token is n's anonymous child spelled kind, or nil.
func (g *goLower) token(n *ts.Node, kind string) *ts.Node {
	for i := range n.ChildCount() {
		if c := n.Child(i); !c.IsNamed() && c.Kind() == kind {
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
	v := g.b.Var(g.span(id))
	g.bind(name, v)
	return v
}

// bindNames binds every child of n in field "name" to no variable.
func (g *goLower) bindNames(n *ts.Node) {
	for i := range n.ChildCount() {
		if n.FieldNameForChild(uint32(i)) == "name" {
			g.bind(g.text(n.Child(i)), -1)
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
		d := list.NamedChild(i)
		for j := range d.ChildCount() {
			if d.FieldNameForChild(uint32(j)) != "name" {
				continue
			}
			id := d.Child(j)
			v := g.declare(id)
			if v < 0 {
				continue
			}
			g.b.Def(g.node(flow.Stmt, id), v)
			if result {
				g.results = append(g.results, v)
			}
		}
	}
}

func (g *goLower) isShort(n *ts.Node) bool {
	if n.Kind() != "binary_expression" {
		return false
	}
	op := n.ChildByFieldName("operator")
	return op != nil && (op.Kind() == "&&" || op.Kind() == "||")
}

// collect appends to buf every variable n reads, skipping hoisted
// short-circuit operands and resolving a function literal's captures.
func (g *goLower) collect(n *ts.Node) {
	switch {
	case n.Kind() == "identifier":
		if v := g.variable(n); v >= 0 {
			g.buf = append(g.buf, v)
		}
	case g.l.isCallable(n):
		g.scan(n, len(g.binds))
	case g.isShort(n):
	default:
		for i := range n.NamedChildCount() {
			g.collect(n.NamedChild(i))
		}
	}
}

// uses records every variable e reads as a use of node id.
func (g *goLower) uses(id int32, e *ts.Node) {
	lo := len(g.buf)
	if e != nil {
		g.collect(e)
	}
	for _, v := range g.buf[lo:] {
		g.b.Use(id, v)
	}
	g.buf = g.buf[:lo]
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
	and := n.ChildByFieldName("operator").Kind() == "&&"
	first, lt, lf := g.chain(n.ChildByFieldName("left"))
	if and {
		g.b.Restore(lt)
	} else {
		g.b.Restore(lf)
	}
	_, rt, rf := g.chain(n.ChildByFieldName("right"))
	if and {
		g.b.Restore(lf)
		g.b.Merge(rf)
		return first, rt, g.b.Push()
	}
	g.b.Restore(lt)
	g.b.Merge(rt)
	return first, g.b.Push(), rf
}

// cond lowers condition e as one Branch node after its hoisted operands.
func (g *goLower) cond(e *ts.Node) {
	g.hoist(e)
	g.uses(g.node(flow.Branch, e), e)
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
	for i := range n.NamedChildCount() {
		list := n.NamedChild(i)
		if list.Kind() != "statement_list" {
			continue
		}
		for j := range list.NamedChildCount() {
			s := list.NamedChild(j)
			if s.Kind() == "comment" {
				continue
			}
			g.stmt(s, nil)
			fell = s.Kind() == "fallthrough_statement"
		}
	}
	return fell
}

// label is the label a break, continue or goto names, or "".
func (g *goLower) label(s *ts.Node) string {
	for i := range s.NamedChildCount() {
		if c := s.NamedChild(i); c.Kind() == "label_name" {
			return string(g.text(c))
		}
	}
	return ""
}

// stmt lowers statement s; labels are the labels naming it.
func (g *goLower) stmt(s *ts.Node, labels []string) {
	b := g.b
	switch s.Kind() {
	case "comment", "empty_statement":
	case "block":
		g.block(s)
	case "expression_statement":
		e := firstNamed(s)
		g.hoist(e)
		if g.isPanic(e) {
			g.uses(g.node(flow.Jump, s), e)
			b.Throw()
			return
		}
		g.uses(g.node(flow.Stmt, s), e)
	case "send_statement", "go_statement", "defer_statement":
		g.hoist(s)
		g.uses(g.node(flow.Stmt, s), s)
	case "inc_statement", "dec_statement":
		e := firstNamed(s)
		g.hoist(e)
		id := g.node(flow.Stmt, s)
		g.uses(id, e)
		if x := g.l.unparen(e); x.Kind() == "identifier" {
			if v := g.variable(x); v >= 0 {
				b.Def(id, v)
			}
		}
	case "assignment_statement":
		g.fill(s.ChildByFieldName("left"), "")
		g.assign(s, s.ChildByFieldName("right"), s.ChildByFieldName("operator").Kind() != "=", false)
	case "short_var_declaration":
		g.fill(s.ChildByFieldName("left"), "")
		g.assign(s, s.ChildByFieldName("right"), false, true)
	case "var_declaration":
		for i := range s.NamedChildCount() {
			switch c := s.NamedChild(i); c.Kind() {
			case "var_spec":
				g.fill(c, "name")
				g.assign(s, c.ChildByFieldName("value"), false, true)
			case "var_spec_list":
				for j := range c.NamedChildCount() {
					if spec := c.NamedChild(j); spec.Kind() == "var_spec" {
						g.fill(spec, "name")
						g.assign(spec, spec.ChildByFieldName("value"), false, true)
					}
				}
			}
		}
	case "const_declaration", "type_declaration":
		for i := range s.NamedChildCount() {
			g.bindNames(s.NamedChild(i))
		}
	case "return_statement":
		g.hoist(s)
		id := g.node(flow.Jump, s)
		g.uses(id, s)
		if firstNamed(s) == nil {
			for _, v := range g.results {
				b.Use(id, v)
			}
		}
		b.Return()
	case "break_statement":
		g.node(flow.Jump, s)
		b.Break(g.label(s))
	case "continue_statement":
		g.node(flow.Jump, s)
		b.Continue(g.label(s))
	case "goto_statement":
		g.node(flow.Jump, s)
		b.Goto(g.label(s))
	case "fallthrough_statement":
		// The enclosing switch carries the fringe into the next clause.
		g.node(flow.Jump, s)
	case "labeled_statement":
		g.labeled(s, labels)
	case "if_statement":
		g.ifStmt(s)
	case "for_statement":
		g.forStmt(s, labels)
	case "expression_switch_statement":
		g.switchStmt(s, labels, false)
	case "type_switch_statement":
		g.switchStmt(s, labels, true)
	case "select_statement":
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
	if e == nil || e.Kind() != "call_expression" {
		return false
	}
	f := e.ChildByFieldName("function")
	return f != nil && f.Kind() == "identifier" && string(g.text(f)) == "panic" && g.binds.find(g.text(f), 0) < 0
}

// labeled lowers a labelled statement. Go's break and continue name only an
// enclosing for, switch or select, so the labels reach those frames; every
// label also names the next node for goto.
func (g *goLower) labeled(s *ts.Node, labels []string) {
	name := string(g.text(s.ChildByFieldName("label")))
	labels = append(labels, name)
	g.b.Label(name)
	var inner *ts.Node
	for i := range s.NamedChildCount() {
		if c := s.NamedChild(i); c.Kind() != "label_name" && c.Kind() != "comment" {
			inner = c
			break
		}
	}
	if inner == nil || inner.Kind() == "empty_statement" {
		g.node(flow.Stmt, s)
		return
	}
	g.stmt(inner, labels)
}

func (g *goLower) ifStmt(s *ts.Node) {
	b := g.b
	g.open()
	if init := s.ChildByFieldName("initializer"); init != nil {
		g.stmt(init, nil)
	}
	g.cond(s.ChildByFieldName("condition"))
	p := b.Push()
	g.block(s.ChildByFieldName("consequence"))
	t := b.Push()
	b.Restore(p)
	if alt := s.ChildByFieldName("alternative"); alt != nil {
		g.stmt(alt, nil)
	}
	b.Merge(t)
	b.Pop(p)
	g.close()
}

func (g *goLower) forStmt(s *ts.Node, labels []string) {
	b := g.b
	body := s.ChildByFieldName("body")
	var clause, cond, update *ts.Node
	for i := range s.NamedChildCount() {
		if c := s.NamedChild(i); c.Kind() != "block" && c.Kind() != "comment" {
			clause = c
			break
		}
	}
	g.open()
	switch {
	case clause == nil:
	case clause.Kind() == "range_clause":
		g.rangeLoop(clause, body, labels)
		g.close()
		return
	case clause.Kind() == "for_clause":
		if init := clause.ChildByFieldName("initializer"); init != nil {
			g.stmt(init, nil)
		}
		cond, update = clause.ChildByFieldName("condition"), clause.ChildByFieldName("update")
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
// key and value definitions each iteration.
func (g *goLower) rangeLoop(rc, body *ts.Node, labels []string) {
	b := g.b
	right := rc.ChildByFieldName("right")
	g.hoist(right)
	g.uses(g.node(flow.Stmt, right), right)
	f := b.OpenLoop(labels...)
	head := g.node(flow.Branch, g.token(rc, "range"))
	exit := b.Push()
	if left := rc.ChildByFieldName("left"); left != nil {
		define := g.token(rc, ":=") != nil
		for i := range left.NamedChildCount() {
			t := left.NamedChild(i)
			x := g.l.unparen(t)
			if t.Kind() == "comment" || (x.Kind() == "identifier" && g.blank(g.text(x))) {
				continue
			}
			switch {
			case define:
				v := g.declare(x)
				b.Def(g.node(flow.Stmt, x), v)
			case x.Kind() == "identifier":
				id := g.node(flow.Stmt, x)
				if v := g.variable(x); v >= 0 {
					b.Def(id, v)
				}
			default:
				g.hoist(t)
				g.uses(g.node(flow.Stmt, t), t)
			}
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
func (g *goLower) switchStmt(s *ts.Node, labels []string, typed bool) {
	b := g.b
	g.open()
	if init := s.ChildByFieldName("initializer"); init != nil {
		g.stmt(init, nil)
	}
	if value := s.ChildByFieldName("value"); value != nil {
		g.hoist(value)
		if typed {
			g.typeSwitchHead(s, value)
		} else {
			g.uses(g.node(flow.Branch, value), value)
		}
	}
	f := b.OpenSwitch(labels...)
	base := len(g.hs)
	for i := range s.NamedChildCount() {
		switch c := s.NamedChild(i); c.Kind() {
		case "expression_case":
			g.cond(c.ChildByFieldName("value"))
		case "type_case":
			g.nodeSpan(flow.Branch, g.fieldSpan(c, "type"))
		default:
			continue
		}
		g.hs = append(g.hs, b.Push())
	}
	dflt := b.Push()
	first := dflt
	if len(g.hs) > base {
		first = g.hs[base]
	}
	var ft flow.Fringe
	fell, hasDefault, k := false, false, base
	for i := range s.NamedChildCount() {
		c := s.NamedChild(i)
		switch c.Kind() {
		case "expression_case", "type_case":
			b.Restore(g.hs[k])
			k++
		case "default_case":
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

// typeSwitchHead creates the Branch node spanning `x := v.(type)`, which
// uses v and defines the alias x.
func (g *goLower) typeSwitchHead(s, value *ts.Node) {
	start, end := value.StartByte(), value.EndByte()
	alias := s.ChildByFieldName("alias")
	if alias != nil {
		start = alias.StartByte()
	}
	for i := range s.ChildCount() {
		if c := s.Child(i); !c.IsNamed() && c.Kind() == "type" {
			end = c.EndByte()
			if rp := s.Child(i + 1); rp != nil && rp.Kind() == ")" {
				end = rp.EndByte()
			}
			break
		}
	}
	head := g.nodeSpan(flow.Branch, flow.Span{Start: uint32(start), End: uint32(end)})
	g.uses(head, value)
	if alias == nil {
		return
	}
	if id := firstNamed(alias); id != nil && id.Kind() == "identifier" {
		if v := g.declare(id); v >= 0 {
			g.b.Def(head, v)
		}
	}
}

// fieldSpan spans every child of n in field name.
func (g *goLower) fieldSpan(n *ts.Node, name string) flow.Span {
	s := g.span(n)
	found := false
	for i := range n.ChildCount() {
		if n.FieldNameForChild(uint32(i)) != name {
			continue
		}
		c := n.Child(i)
		if !found {
			s.Start = uint32(c.StartByte())
			found = true
		}
		s.End = uint32(c.EndByte())
	}
	return s
}

// selectStmt lowers a select: a head reaching every clause, each clause its
// communication node then its body.
func (g *goLower) selectStmt(s *ts.Node, labels []string) {
	b := g.b
	f := b.OpenSwitch(labels...)
	head := g.node(flow.Branch, s.Child(0))
	d := b.Push()
	clauses := false
	for i := range s.NamedChildCount() {
		c := s.NamedChild(i)
		if c.Kind() != "communication_case" && c.Kind() != "default_case" {
			continue
		}
		clauses = true
		b.Restore(d)
		g.open()
		if comm := c.ChildByFieldName("communication"); comm != nil {
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
	if c.Kind() == "receive_statement" {
		if left := c.ChildByFieldName("left"); left != nil {
			g.fill(left, "")
			g.assign(c, c.ChildByFieldName("right"), false, g.token(c, ":=") != nil)
			return
		}
	}
	g.hoist(c)
	g.uses(g.node(flow.Stmt, c), c)
}

// fill sets lefts to n's children in field name, or to its named children
// when name is "".
func (g *goLower) fill(n *ts.Node, name string) {
	g.lefts = g.lefts[:0]
	for i := range n.ChildCount() {
		c := n.Child(i)
		if name == "" && c.IsNamed() && c.Kind() != "comment" || name != "" && n.FieldNameForChild(uint32(i)) == name {
			g.lefts = append(g.lefts, c)
		}
	}
}

// assign lowers a statement assigning right (an expression list, one
// expression, or nil) to lefts; compound targets are also read, define
// declares every target not declared in the innermost block.
func (g *goLower) assign(whole, right *ts.Node, compound, define bool) {
	b := g.b
	g.rights = g.rights[:0]
	switch {
	case right == nil:
	case right.Kind() == "expression_list":
		for i := range right.NamedChildCount() {
			if c := right.NamedChild(i); c.Kind() != "comment" {
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
	lo := len(g.buf)
	// Non-variable targets' operands and values come first: the statement's
	// first node carries them.
	g.targets = g.targets[:0]
	for i, n := range g.lefts {
		x := g.l.unparen(n)
		t := goTarget{id: x, left: i, v: -1}
		if x.Kind() == "identifier" {
			switch name := g.text(x); {
			case g.blank(name):
			case define && !g.declaredHere(name):
				t.isNew = true
			default:
				t.v = g.variable(x)
			}
		} else {
			g.collect(n)
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
		for _, v := range g.buf[lo:] {
			b.Use(id, v)
		}
		if len(g.targets) == 1 {
			b.Def(id, g.targets[0].v)
		}
		g.buf = g.buf[:lo]
		return
	}
	g.order(lo, extra)
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
// value. buf[lo:extra] are the first node's own uses.
func (g *goLower) order(lo, extra int) {
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
			for _, v := range g.buf[lo:extra] {
				b.Use(id, v)
			}
		}
		for _, v := range g.buf[t.lo:t.hi] {
			b.Use(id, v)
		}
		if cycle {
			b.Use(id, t.v)
		}
		b.Def(id, t.v)
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
// variable, with the literal's own declarations shadowing.
func (g *goLower) scan(n *ts.Node, base int) {
	if g.l.isCallable(n) {
		g.open()
		for _, field := range [...]string{"parameters", "result"} {
			if list := n.ChildByFieldName(field); list != nil && list.Kind() == "parameter_list" {
				for i := range list.NamedChildCount() {
					g.bindNames(list.NamedChild(i))
				}
			}
		}
		if body := n.ChildByFieldName("body"); body != nil {
			g.scan(body, base)
		}
		g.close()
		return
	}
	switch n.Kind() {
	case "identifier":
		if i := g.binds.find(g.text(n), 0); i >= 0 && i < base && g.binds[i].v >= 0 {
			g.buf = append(g.buf, g.binds[i].v)
		}
		return
	case "block", "if_statement", "for_statement", "expression_switch_statement", "select_statement",
		"expression_case", "default_case", "type_case", "communication_case":
		g.open()
		g.scanChildren(n, base)
		g.close()
		return
	case "type_switch_statement":
		g.open()
		for _, field := range [...]string{"initializer", "value"} {
			if c := n.ChildByFieldName(field); c != nil {
				g.scan(c, base)
			}
		}
		if alias := n.ChildByFieldName("alias"); alias != nil {
			for i := range alias.NamedChildCount() {
				g.bind(g.text(alias.NamedChild(i)), -1)
			}
		}
		for i := range n.NamedChildCount() {
			if c := n.NamedChild(i); c.Kind() == "type_case" || c.Kind() == "default_case" {
				g.scan(c, base)
			}
		}
		g.close()
		return
	case "short_var_declaration":
		g.scan(n.ChildByFieldName("right"), base)
		left := n.ChildByFieldName("left")
		for i := range left.NamedChildCount() {
			if id := left.NamedChild(i); id.Kind() == "identifier" && !g.declaredHere(g.text(id)) {
				g.bind(g.text(id), -1)
			}
		}
		return
	case "var_spec":
		if value := n.ChildByFieldName("value"); value != nil {
			g.scan(value, base)
		}
		g.bindNames(n)
		return
	case "const_spec", "type_spec", "type_alias":
		g.bindNames(n)
		return
	case "range_clause", "receive_statement":
		if left := n.ChildByFieldName("left"); left != nil && g.token(n, ":=") != nil {
			g.scan(n.ChildByFieldName("right"), base)
			for i := range left.NamedChildCount() {
				g.bind(g.text(left.NamedChild(i)), -1)
			}
			return
		}
	}
	g.scanChildren(n, base)
}

func (g *goLower) scanChildren(n *ts.Node, base int) {
	for i := range n.NamedChildCount() {
		g.scan(n.NamedChild(i), base)
	}
}
