package worker

import (
	"sync"

	ts "github.com/tree-sitter/go-tree-sitter"

	"github.com/Sawmonabo/codectx/internal/provider/treesitter/flow"
)

// javaLowering lowers Java callables: methods, constructors, compact
// constructors, lambda expressions and static initializers, and every type
// body — a class body (a record's and an anonymous class's included), an
// enum body, an interface body and an annotation type body — as the unit of
// its field initializers, enum constants and instance initializer blocks.
var javaLowering = Lowering{
	language: "java",
	callables: []string{"method_declaration", "constructor_declaration", "compact_constructor_declaration",
		"lambda_expression", "static_initializer", "class_body", "enum_body", "interface_body", "annotation_type_body"},
	lower: lowerJava,
}

// yieldLabel names the frame of every switch expression. No Java identifier
// contains a space, so no labelled statement can take it, and a yield's
// Break(yieldLabel) finds the innermost switch expression through any loop,
// switch statement or labelled block inside the arm (JLS §14.21 The yield
// statement: a yield leaves the innermost enclosing switch expression).
const yieldLabel = " yield"

// lowerJava lowers one Java callable's parameters and body into b.
//
// # Node granularity
//
// The goldens render nodes by source text, so the granularity is exact:
//
//   - Every parameter name is one defining node after Entry, spanning its
//     identifier; a compact constructor's parameters are its record's
//     components (JLS §8.10.4), spanning the identifiers in the record
//     header. A method or constructor without a body lowers to Entry → Exit.
//     A receiver parameter and an `_` name declare nothing.
//   - A statement is one node: an expression statement spans its
//     expression, a declarator with an initializer spans the declarator,
//     return, throw, yield, break and continue span the statement (kind
//     Jump), an explicit constructor invocation spans itself, a local class,
//     record, enum or interface declaration spans the declaration. An
//     expression statement that assigns or updates a local is its defining
//     node. A declarator without an initializer makes no node: it executes
//     nothing (JLS §14.4.2), and the variable is not definitely assigned.
//   - A condition is one Branch node spanning the condition without its
//     parentheses: if, while, do, for. A loop whose condition is the literal
//     `true`, or a for without one, has no exit edge: its head is a Stmt node
//     spanning `true` or the `for` keyword (JLS §14.21).
//   - An enhanced for (JLS §14.14.2) is a Stmt node spanning the iterated
//     expression, evaluated once, which defines an iteration variable of the
//     lowering's own; a Branch head spanning the header from its first
//     modifier or type to the end of the iterated expression, which uses
//     that variable; then, on the body path, one node spanning the loop
//     variable's name that defines it and uses the iteration variable.
//   - A switch (JLS §14.11 statement, §15.28 expression) is a Stmt node for
//     its selector, then one Branch node per `case` label in source order,
//     spanning the label, each tested only when the previous failed; a
//     label's node uses the selector's variables, since it compares against
//     the selector, and its own. A pattern label is followed on its match
//     path by one defining node per pattern variable, spanning the
//     variable's identifier and using the selector's variables, then, for a
//     guard, a Branch node spanning the guard expression whose false edge
//     joins the next label's test. Several labels of one group all enter its
//     body. `default` (and `case null, default`) is taken when no label
//     matched. In the colon form a body's end falls through into the next
//     body; in the arrow form it leaves the switch. A switch expression
//     without `default` is exhaustive, and its no-match path throws (JLS
//     §15.28.2). An arrow arm's expression is a Stmt node spanning it, and
//     the yield of its value; `yield e;` is a Jump node spanning the
//     statement. Every yield breaks to the switch expression's frame (see
//     yieldLabel); the value is no temporary: the node consuming the switch
//     expression spans it and uses every variable read inside it.
//   - `&&`, `||` and the conditional operator: the deciding operand is a
//     Branch node spanning it, created after the nodes of everything it
//     evaluates; each conditionally evaluated operand is a Stmt node spanning
//     it.
//   - `x instanceof T v` and a record pattern define each pattern variable on
//     one node spanning its identifier, using the variables of the tested
//     expression, before the node that decides on the test.
//   - try (JLS §14.20): each catch clause's type test is a Branch node
//     spanning its catch type (a multi-catch `A | B` is one test), in order;
//     its match path defines the parameter on a node spanning its name; the
//     path matching no clause is rethrown. A clause catching `Throwable`
//     catches everything: its test is a Stmt node and ends the chain. The
//     builder's Handler spans the first `catch` keyword, or `finally`.
//   - try-with-resources (JLS §14.20.3.2): each resource declaring a
//     variable is an acquiring node spanning the resource from its name to
//     the end of its initializer, defining the variable; a resource naming an
//     existing variable or field acquires nothing. Each resource then opens a
//     finally whose close is one Stmt node spanning the resource that uses
//     its variable and may throw; the resources close in reverse order, and the
//     statement's catch clauses and finally enclose all of them. A
//     resource's Handler spans the `;` or `)` that ends the resource. The
//     null test before close is not modelled.
//   - `synchronized (e) { … }` (JLS §14.19) is a Stmt node spanning e, which
//     may throw, then the block; the monitor exit is not a node.
//   - `assert c : m;` (JLS §14.10) is a Branch node spanning c whose false
//     path is a Stmt node spanning m, when present, then a throw. It is
//     lowered as if assertions were enabled.
//   - A type body unit lowers, in source order, each field or constant
//     declarator with an initializer (its value's nodes, then a Stmt node
//     spanning the declarator), each enum constant (a Stmt node spanning it)
//     and each instance initializer block. Static initializers, methods,
//     constructors and nested types are their own callables.
//   - An expression lowered for its value and ended by a node spanning it
//     takes no second node when the last node its own lowering made already
//     spans it.
//
// Statement kinds: every kind of the grammar's statement supertype is
// handled above. An empty statement (`;`) makes no node; package, import
// and module declarations do not occur in a callable; any kind the lowering
// does not name (an error node included) is one Stmt node spanning it, with
// its uses, falling through. Expression kinds other than the ones above
// (unary, arithmetic and comparison operators, method references, array
// creation, class literals, templates, literals, `this`) carry no control
// flow and define no local: they contribute their reads and throws to the
// node that consumes them.
//
// # Uses
//
// Only an identifier resolving to a local variable or parameter declared in
// this function is a Use; a field, `this.x` included, is not a variable. A
// node Uses every variable read inside its own span and no read outside it
// (the seed rule). An assignment or update of a local embedded in a larger
// expression is its own defining node, and the enclosing expression's node
// reads the variable it defined. A node that defines v also Uses v when its
// statement read v before it. A write through a field or array element of a
// local (`o.f = v`, `a[i] += v`, `a[i]++`) is a Stmt node spanning the
// assignment that uses its operands and may-defines (MayDef, non-killing)
// the local at the base of the target.
//
// A lambda (its own function) is one Stmt node spanning it in the enclosing
// function; an anonymous class's body is its own unit, and the object
// creation expression is one Stmt node spanning the creation. Either node
// Uses every enclosing variable referenced inside, resolved with the nested
// code's own declarations shadowing (a local or parameter of a method of the
// anonymous class, or one of its fields). A captured local is effectively
// final (JLS §15.27.2, §8.1.3), so no nested code assigns it; a write
// through its field or array element inside (`arr[0] = 1`, `box.v = 2`) is
// the through-a-target rule: a MayDef of the local on the creating node,
// since when the nested code runs is unknown. A local class declaration's
// node is its creating node.
//
// # Exceptions
//
// MayThrow is given to every node whose own evaluation — the source
// evaluated since the previous node — contains a method or constructor
// invocation, an instance or array creation, an array access, a field access
// through a receiver other than `this` or `super`, a cast, an enhanced-for
// iterator step (the head) or a resource close; a monitor expression may
// throw. Division, unboxing and string conversion are not counted. A throw
// statement's node, a failed assertion and an unmatched catch chain are a
// Throw. The Builder applies MayThrow only inside an open catch or finally
// frame.
//
// # Scoping
//
// A local's scope runs from its declarator to the end of its block (JLS
// §6.3); a for header's locals are scoped to the loop, a catch parameter to
// its clause, a resource to the try block and its closes, a switch group's
// pattern variables to its guard and body. A pattern variable of
// instanceof is bound in the innermost enclosing block from its test on
// (an over-approximation of the flow scoping of JLS §6.3.1: a later pattern
// of the same name is a new variable that shadows it).
func lowerJava(l *Lowering, b *flow.Builder, fn *ts.Node, src []byte, s *Scratch) {
	cur := s.cursor(fn)
	j := javaLower{l: l, b: b, src: src, k: javaSyntaxOf(), cur: cur, binds: &s.scope, first: -1, last: -1, stmtNo: 1}
	k := j.k
	switch fn.KindId() {
	case k.classBody, k.enumBody, k.interfaceBody, k.annotationTypeBody:
		j.typeBody(fn)
	case k.staticInitializer:
		if body := firstNamed(fn); body != nil {
			j.block(body)
		}
	case k.lambda:
		switch ps := fn.ChildByFieldId(k.fParameters); ps.KindId() {
		case k.identifier:
			j.param(ps)
		case k.inferredParameters:
			start, list := j.kids(ps)
			for i := range list {
				j.param(&list[i])
			}
			j.done(start)
		default:
			j.params(ps)
		}
		body := fn.ChildByFieldId(k.fBody)
		if body.KindId() == k.block {
			j.block(body)
			return
		}
		j.reset()
		j.valueNode(body)
	case k.compactCtor:
		if rec := fn.Parent(); rec != nil {
			if rec = rec.Parent(); rec != nil && rec.KindId() == k.recordDecl {
				j.params(rec.ChildByFieldId(k.fParameters))
			}
		}
		j.block(fn.ChildByFieldId(k.fBody))
	default:
		body := fn.ChildByFieldId(k.fBody)
		if body == nil {
			return
		}
		j.params(fn.ChildByFieldId(k.fParameters))
		j.block(body)
	}
}

// javaLower is the state of lowering one Java callable.
type javaLower struct {
	l   *Lowering
	b   *flow.Builder
	src []byte
	k   *javaSyntax
	cur *ts.TreeCursor
	// buf is a stack of child lists; kids and fieldKids push one and done
	// pops it.
	buf []ts.Node
	// binds is the scope chain, innermost last.
	binds *scope
	// shadow is non-zero while walking nested code for its captures:
	// declarations then bind -1 and no node is created.
	shadow int
	// reads are the variables read by the current statement, in evaluation
	// order; seen[v] == stmtNo marks v as one of them.
	reads  []int32
	seen   []int
	stmtNo int
	// stack saves reads across a switch expression's statements and holds a
	// switch's selector reads while its labels are lowered.
	stack []int32
	// throws counts throwing constructs evaluated by the current statement;
	// those past thrown are not yet attached to a node.
	throws, thrown int
	// first is the first node created since the last open, or -1.
	first int32
	// last is the node created last, or -1, and lastSpan its span.
	last     int32
	lastSpan flow.Span
	// labels are the statement labels the next statement takes.
	labels []string
	// arms are the match fringes of the switches being lowered, each tagged
	// with its group; caseBinds and groupAt hold each group's pattern
	// variables; frames holds the finally frames of the resources being
	// lowered.
	arms      []javaArm
	caseBinds []binding
	groupAt   []int
	frames    []flow.Frame
	// writes are the enclosing locals written through a field or array
	// element inside the nested code whose captures are being collected.
	writes []int32
}

// javaArm is one switch label's match fringe and the index of its group.
type javaArm struct {
	group int
	fr    flow.Fringe
}

// kids pushes n's named, non-extra children onto buf and returns the stack
// mark and the list; done(mark) pops them. A list stays valid across nested
// kids calls.
func (j *javaLower) kids(n *ts.Node) (int, []ts.Node) {
	return j.fieldKids(n, 0)
}

// fieldKids is kids restricted to the children in field f; f 0 takes every
// named child.
func (j *javaLower) fieldKids(n *ts.Node, f uint16) (int, []ts.Node) {
	start := len(j.buf)
	c := j.cur
	c.Reset(*n)
	if c.GotoFirstChild() {
		for {
			if x := c.Node(); x.IsNamed() && !x.IsExtra() && (f == 0 || c.FieldId() == f) {
				j.buf = append(j.buf, *x)
			}
			if !c.GotoNextSibling() {
				break
			}
		}
	}
	return start, j.buf[start:]
}

// hasToken reports whether n has an anonymous child of kind id.
func (j *javaLower) hasToken(n *ts.Node, id uint16) bool {
	c := j.cur
	c.Reset(*n)
	if !c.GotoFirstChild() {
		return false
	}
	for {
		if x := c.Node(); !x.IsNamed() && x.KindId() == id {
			return true
		}
		if !c.GotoNextSibling() {
			return false
		}
	}
}

func (j *javaLower) done(mark int) { j.buf = j.buf[:mark] }

func (j *javaLower) text(n *ts.Node) []byte { return textOf(j.src, n) }

// declare binds name in the innermost scope: a new variable, or -1 while
// captures are collected.
func (j *javaLower) declare(name *ts.Node) int32 {
	v := int32(-1)
	if j.shadow == 0 {
		v = j.b.Var()
	}
	j.binds.push(j.text(name), v)
	return v
}

func (j *javaLower) lookup(name *ts.Node) int32 { return j.binds.lookup(j.text(name)) }

// ref records a read of name when it resolves to a variable of this function.
func (j *javaLower) ref(name *ts.Node) { j.read(j.lookup(name)) }

// read records a read of v, unless v is -1.
func (j *javaLower) read(v int32) {
	if v < 0 {
		return
	}
	j.reads = append(j.reads, v)
	if int(v) >= len(j.seen) {
		j.seen = append(j.seen, make([]int, int(v)+1-len(j.seen))...)
	}
	j.seen[v] = j.stmtNo
}

// reset starts a statement: nothing is read and no throw is pending.
func (j *javaLower) reset() {
	j.reads, j.throws, j.thrown = j.reads[:0], 0, 0
	j.stmtNo++
}

// node creates a node spanning n that Uses reads[from:to], and MayThrow when
// a throwing construct was evaluated since the previous node.
func (j *javaLower) node(kind flow.Kind, n *ts.Node, from, to int) int32 {
	return j.nodeAt(kind, spanOf(n), from, to)
}

func (j *javaLower) nodeAt(kind flow.Kind, s flow.Span, from, to int) int32 {
	id := j.b.Node(kind, s)
	if j.first < 0 {
		j.first = id
	}
	for _, v := range j.reads[from:to] {
		j.b.Use(id, v)
	}
	if j.throws > j.thrown {
		j.b.MayThrow(id)
	}
	j.thrown = j.throws
	j.last, j.lastSpan = id, s
	return id
}

// def records that node n defines v and, when the statement read v before
// n, that n Uses v.
func (j *javaLower) def(n, v int32) {
	if v < 0 {
		return
	}
	j.b.Def(n, v)
	if int(v) < len(j.seen) && j.seen[v] == j.stmtNo {
		j.b.Use(n, v)
	}
}

// valueNode lowers n for its value and ends it with a Stmt node spanning n,
// unless the last node that lowering made already spans n, or n without its
// parentheses, with no throw evaluated after it.
func (j *javaLower) valueNode(n *ts.Node) {
	m, last := len(j.reads), j.last
	j.value(n)
	if j.last != last && j.lastSpan == spanOf(j.l.unparen(n)) && j.thrown == j.throws {
		return
	}
	j.node(flow.Stmt, n, m, len(j.reads))
}

func (j *javaLower) open() int32 {
	s := j.first
	j.first = -1
	return s
}

func (j *javaLower) close(saved int32) int32 {
	h := j.first
	if saved >= 0 {
		j.first = saved
	}
	return h
}

// param declares and defines one parameter name.
func (j *javaLower) param(name *ts.Node) {
	if name == nil || name.KindId() != j.k.identifier {
		return
	}
	j.reset()
	v := j.declare(name)
	j.def(j.node(flow.Stmt, name, 0, 0), v)
}

// params lowers a formal parameter list.
func (j *javaLower) params(ps *ts.Node) {
	if ps == nil {
		return
	}
	k := j.k
	start, list := j.kids(ps)
	for i := range list {
		switch p := &list[i]; p.KindId() {
		case k.formalParameter:
			j.param(p.ChildByFieldId(k.fName))
		case k.spreadParameter:
			s, ds := j.kids(p)
			for d := range ds {
				if ds[d].KindId() == k.variableDeclarator {
					j.param(ds[d].ChildByFieldId(k.fName))
				}
			}
			j.done(s)
		}
	}
	j.done(start)
}

// typeBody lowers a type body's unit: field and constant initializers, enum
// constants and instance initializer blocks, in source order.
func (j *javaLower) typeBody(n *ts.Node) {
	k := j.k
	start, list := j.kids(n)
	for i := range list {
		switch m := &list[i]; m.KindId() {
		case k.fieldDecl, k.constantDecl:
			s, ds := j.fieldKids(m, k.fDeclarator)
			for d := range ds {
				if v := ds[d].ChildByFieldId(k.fValue); v != nil {
					j.reset()
					j.value(v)
					j.node(flow.Stmt, &ds[d], 0, len(j.reads))
				}
			}
			j.done(s)
		case k.enumConstant:
			j.reset()
			if a := m.ChildByFieldId(k.fArguments); a != nil {
				j.value(a)
			}
			j.node(flow.Stmt, m, 0, len(j.reads))
		case k.enumBodyDecls:
			j.typeBody(m)
		case k.block:
			j.block(m)
		}
	}
	j.done(start)
}

// block lowers a statement list in its own scope.
func (j *javaLower) block(n *ts.Node) {
	mark := j.binds.mark()
	start, list := j.kids(n)
	for i := range list {
		j.stmt(&list[i])
	}
	j.done(start)
	j.binds.truncate(mark)
}

// sub lowers n, a statement standing alone as the body of an if, else, loop
// or label, in a scope of its own.
func (j *javaLower) sub(n *ts.Node) {
	mark := j.binds.mark()
	j.stmt(n)
	j.binds.truncate(mark)
}

// stmt lowers one statement.
func (j *javaLower) stmt(n *ts.Node) {
	k := j.k
	labels := j.labels
	j.labels = nil
	j.reset()
	switch n.KindId() {
	case k.expressionStmt:
		if e := firstNamed(n); e != nil {
			j.exprStmt(e)
		}
	case k.localVarDecl:
		j.declaration(n)
	case k.block:
		j.block(n)
	case k.ifStmt:
		j.ifStmt(n)
	case k.whileStmt:
		j.whileStmt(n, labels)
	case k.doStmt:
		j.doStmt(n, labels)
	case k.forStmt:
		j.forStmt(n, labels)
	case k.enhancedFor:
		j.enhancedFor(n, labels)
	case k.switchExpr:
		j.switchBlock(n, labels, false)
	case k.tryStmt, k.tryWithResources:
		j.tryStmt(n)
	case k.labeledStmt:
		j.labeled(n, labels)
	case k.returnStmt, k.throwStmt, k.yieldStmt:
		if e := firstNamed(n); e != nil {
			j.value(e)
		}
		j.node(flow.Jump, n, 0, len(j.reads))
		switch n.KindId() {
		case k.returnStmt:
			j.b.Return()
		case k.throwStmt:
			j.b.Throw()
		default:
			j.b.Break(yieldLabel)
		}
	case k.breakStmt:
		j.node(flow.Jump, n, 0, 0)
		j.b.Break(j.label(n))
	case k.continueStmt:
		j.node(flow.Jump, n, 0, 0)
		j.b.Continue(j.label(n))
	case k.synchronizedStmt:
		start, list := j.kids(n)
		for i := range list {
			if list[i].KindId() == k.parenthesized {
				e := j.l.unparen(&list[i])
				j.value(e)
				j.throws++
				j.node(flow.Stmt, e, 0, len(j.reads))
			}
		}
		j.done(start)
		j.block(n.ChildByFieldId(k.fBody))
	case k.assertStmt:
		j.assertStmt(n)
	case k.classDecl, k.recordDecl, k.enumDecl, k.interfaceDecl, k.annotationTypeDecl:
		w := len(j.writes)
		j.shadow++
		j.capType(n)
		j.shadow--
		j.closure(j.node(flow.Stmt, n, 0, len(j.reads)), w)
	case k.explicitCtorCall:
		if o := n.ChildByFieldId(k.fObject); o != nil {
			j.value(o)
		}
		j.value(n.ChildByFieldId(k.fArguments))
		j.throws++
		j.node(flow.Stmt, n, 0, len(j.reads))
	default:
		j.valueNode(n)
	}
}

// label is a break or continue statement's label, or "".
func (j *javaLower) label(n *ts.Node) string {
	if l := firstNamed(n); l != nil {
		return view(j.text(l))
	}
	return ""
}

// exprStmt lowers an expression evaluated for its effect. An assignment or
// update makes the node spanning it, which is the statement's node.
func (j *javaLower) exprStmt(e *ts.Node) {
	k := j.k
	switch u := j.l.unparen(e); u.KindId() {
	case k.assignment:
		j.assign(u)
	case k.update:
		j.update(u)
	default:
		j.valueNode(e)
	}
}

// declaration lowers a local variable declaration, one declarator at a time.
func (j *javaLower) declaration(n *ts.Node) {
	k := j.k
	start, list := j.fieldKids(n, k.fDeclarator)
	for i := range list {
		d := &list[i]
		name, val := d.ChildByFieldId(k.fName), d.ChildByFieldId(k.fValue)
		m := len(j.reads)
		if val != nil {
			j.value(val)
		}
		v := int32(-1)
		if name.KindId() == k.identifier {
			v = j.declare(name)
		}
		if val != nil {
			j.def(j.node(flow.Stmt, d, m, len(j.reads)), v)
		}
	}
	j.done(start)
}

func (j *javaLower) ifStmt(n *ts.Node) {
	k := j.k
	cond := j.l.unparen(n.ChildByFieldId(k.fCondition))
	j.value(cond)
	j.node(flow.Branch, cond, 0, len(j.reads))
	p := j.b.Push()
	j.sub(n.ChildByFieldId(k.fConsequence))
	t := j.b.Push()
	j.b.Restore(p)
	if alt := n.ChildByFieldId(k.fAlternative); alt != nil {
		j.sub(alt)
	}
	j.b.Merge(t)
	j.b.Pop(p)
}

// head lowers a loop condition as the loop's decision node, or as a Stmt
// node without an exit edge when it is the literal true. It reports whether
// the loop exits through it.
func (j *javaLower) head(cond *ts.Node) bool {
	j.reset()
	if cond.KindId() == j.k.trueLit {
		j.node(flow.Stmt, cond, 0, 0)
		return false
	}
	j.value(cond)
	j.node(flow.Branch, cond, 0, len(j.reads))
	return true
}

// loopEnd closes a loop whose back edge targets h.
func (j *javaLower) loopEnd(f flow.Frame, h int32, exits bool, exit flow.Fringe) {
	j.b.Close(h)
	if exits {
		j.b.Restore(exit)
	}
	j.b.CloseFrame(f)
	if exits {
		j.b.Pop(exit)
	}
}

func (j *javaLower) whileStmt(n *ts.Node, labels []string) {
	k := j.k
	f := j.b.OpenLoop(labels...)
	saved := j.open()
	exits := j.head(j.l.unparen(n.ChildByFieldId(k.fCondition)))
	h := j.close(saved)
	var exit flow.Fringe
	if exits {
		exit = j.b.Push()
	}
	j.sub(n.ChildByFieldId(k.fBody))
	j.b.ContinueHere(f)
	j.loopEnd(f, h, exits, exit)
}

func (j *javaLower) doStmt(n *ts.Node, labels []string) {
	k := j.k
	f := j.b.OpenLoop(labels...)
	saved := j.open()
	j.sub(n.ChildByFieldId(k.fBody))
	j.b.ContinueHere(f)
	exits := j.head(j.l.unparen(n.ChildByFieldId(k.fCondition)))
	h := j.close(saved)
	var exit flow.Fringe
	if exits {
		exit = j.b.Push()
	}
	j.loopEnd(f, h, exits, exit)
}

func (j *javaLower) forStmt(n *ts.Node, labels []string) {
	k := j.k
	mark := j.binds.mark()
	start, inits := j.fieldKids(n, k.fInit)
	for i := range inits {
		if inits[i].KindId() == k.localVarDecl {
			j.stmt(&inits[i])
		} else {
			j.reset()
			j.exprStmt(&inits[i])
		}
	}
	j.done(start)
	f := j.b.OpenLoop(labels...)
	saved := j.open()
	exits := false
	if cond := n.ChildByFieldId(k.fCondition); cond == nil {
		j.reset()
		j.node(flow.Stmt, n.Child(0), 0, 0)
	} else {
		exits = j.head(j.l.unparen(cond))
	}
	h := j.close(saved)
	var exit flow.Fringe
	if exits {
		exit = j.b.Push()
	}
	j.sub(n.ChildByFieldId(k.fBody))
	j.b.ContinueHere(f)
	start, ups := j.fieldKids(n, k.fUpdate)
	for i := range ups {
		j.reset()
		j.exprStmt(&ups[i])
	}
	j.done(start)
	j.loopEnd(f, h, exits, exit)
	j.binds.truncate(mark)
}

// enhancedFor lowers `for (T x : e) body` (see Node granularity).
func (j *javaLower) enhancedFor(n *ts.Node, labels []string) {
	k := j.k
	mark := j.binds.mark()
	val := n.ChildByFieldId(k.fValue)
	j.value(val)
	it := j.b.Var()
	j.def(j.node(flow.Stmt, val, 0, len(j.reads)), it)
	f := j.b.OpenLoop(labels...)
	j.reset()
	j.throws++
	h := j.nodeAt(flow.Branch, flow.Span{Start: uint32(firstNamed(n).StartByte()), End: uint32(val.EndByte())}, 0, 0)
	j.b.Use(h, it)
	exit := j.b.Push()
	if name := n.ChildByFieldId(k.fName); name.KindId() == k.identifier {
		j.reset()
		v := j.declare(name)
		x := j.node(flow.Stmt, name, 0, 0)
		j.b.Use(x, it)
		j.def(x, v)
	}
	j.sub(n.ChildByFieldId(k.fBody))
	j.b.ContinueHere(f)
	j.loopEnd(f, h, true, exit)
	j.binds.truncate(mark)
}

// switchBlock lowers a switch statement (expr false) or a switch expression
// (expr true); see Node granularity.
func (j *javaLower) switchBlock(n *ts.Node, labels []string, expr bool) {
	k := j.k
	sel := j.l.unparen(n.ChildByFieldId(k.fCondition))
	m := len(j.reads)
	j.value(sel)
	j.node(flow.Stmt, sel, m, len(j.reads))
	sBase := len(j.stack)
	j.stack = append(j.stack, j.reads[m:]...)
	sEnd := len(j.stack)
	var f flow.Frame
	if expr {
		f = j.b.OpenBlock(yieldLabel)
	} else {
		f = j.b.OpenSwitch(labels...)
	}
	base := j.b.Push()
	aBase, cBase, gBase := len(j.arms), len(j.caseBinds), len(j.groupAt)
	start, groups := j.kids(n.ChildByFieldId(k.fBody))
	dflt := -1
	for g := range groups {
		j.groupAt = append(j.groupAt, len(j.caseBinds))
		s, list := j.kids(&groups[g])
		for i := range list {
			c := &list[i]
			if c.KindId() != k.switchLabel {
				continue
			}
			if j.hasToken(c, k.defaultKw) {
				dflt = g
				continue
			}
			j.caseLabel(c, g, sBase, sEnd)
		}
		j.done(s)
	}
	j.groupAt = append(j.groupAt, len(j.caseBinds))
	noMatch := j.b.Push()
	ai := aBase
	for g := range groups {
		grp := &groups[g]
		arrow := grp.KindId() == k.switchRule
		merged := !arrow && g > 0
		for ; ai < len(j.arms) && j.arms[ai].group == g; ai++ {
			if merged {
				j.b.Merge(j.arms[ai].fr)
			} else {
				j.b.Restore(j.arms[ai].fr)
			}
			merged = true
		}
		if g == dflt {
			if merged {
				j.b.Merge(noMatch)
			} else {
				j.b.Restore(noMatch)
			}
		}
		mark := j.binds.mark()
		for _, cb := range j.caseBinds[j.groupAt[gBase+g]:j.groupAt[gBase+g+1]] {
			j.binds.bind(cb.name, cb.v)
		}
		s, list := j.kids(grp)
		for i := range list {
			c := &list[i]
			switch {
			case c.KindId() == k.switchLabel:
			case arrow && expr && c.KindId() == k.expressionStmt:
				j.reset()
				j.valueNode(firstNamed(c))
				j.b.Break(yieldLabel)
			case arrow:
				j.stmt(c)
				if expr {
					j.b.Break(yieldLabel)
				} else {
					j.b.Break("")
				}
			default:
				j.stmt(c)
			}
		}
		j.done(s)
		j.binds.truncate(mark)
	}
	if dflt < 0 {
		if expr {
			t := j.b.Push()
			j.b.Restore(noMatch)
			j.b.Throw()
			j.b.Restore(t)
		} else {
			j.b.Merge(noMatch)
		}
	}
	j.b.CloseFrame(f)
	j.b.Pop(base)
	j.done(start)
	j.arms, j.caseBinds, j.groupAt = j.arms[:aBase], j.caseBinds[:cBase], j.groupAt[:gBase]
	j.stack = j.stack[:sBase]
}

// caseLabel lowers one `case` label of group g: its test, its pattern
// variables and its guard. The selector's reads are stack[from:to].
func (j *javaLower) caseLabel(c *ts.Node, g, from, to int) {
	k := j.k
	j.reset()
	for _, v := range j.stack[from:to] {
		j.read(v)
	}
	sel := len(j.reads)
	var pattern, guard *ts.Node
	start, list := j.kids(c)
	for i := range list {
		switch e := &list[i]; e.KindId() {
		case k.pattern:
			pattern = e
		case k.guard:
			guard = e
		default:
			j.value(e)
		}
	}
	j.node(flow.Branch, c, 0, len(j.reads))
	p := j.b.Push()
	mark := j.binds.mark()
	if pattern != nil {
		j.bindPattern(pattern, 0, sel)
	}
	if guard != nil {
		if e := firstNamed(guard); e != nil {
			j.reset()
			j.value(e)
			j.node(flow.Branch, e, 0, len(j.reads))
		}
	}
	arm := j.b.Push()
	j.arms = append(j.arms, javaArm{group: g, fr: arm})
	if pattern != nil || guard != nil {
		j.b.Restore(p)
		if guard != nil {
			j.b.Merge(arm)
		}
	}
	for i := mark; i < j.binds.mark(); i++ {
		j.caseBinds = append(j.caseBinds, j.binds.at(i))
	}
	j.binds.truncate(mark)
	j.done(start)
}

// bindPattern declares every variable pattern p binds and, outside a
// capture walk, defines each on one node spanning its identifier that Uses
// reads[from:to].
func (j *javaLower) bindPattern(p *ts.Node, from, to int) {
	k := j.k
	switch p.KindId() {
	case k.identifier:
		v := j.declare(p)
		if j.shadow == 0 {
			j.def(j.node(flow.Stmt, p, from, to), v)
		}
	case k.pattern:
		if c := firstNamed(p); c != nil {
			j.bindPattern(c, from, to)
		}
	case k.typePattern, k.recordPatternComponent:
		start, list := j.kids(p)
		for i := range list {
			if list[i].KindId() == k.identifier {
				j.bindPattern(&list[i], from, to)
			}
		}
		j.done(start)
	case k.recordPattern:
		start, list := j.kids(p)
		for i := range list {
			if list[i].KindId() == k.recordPatternBody {
				s, cs := j.kids(&list[i])
				for c := range cs {
					j.bindPattern(&cs[c], from, to)
				}
				j.done(s)
			}
		}
		j.done(start)
	}
}

// tryStmt lowers try, try/catch/finally and try-with-resources.
func (j *javaLower) tryStmt(n *ts.Node) {
	k := j.k
	start, list := j.kids(n)
	var catches []ts.Node
	var fin *ts.Node
	for i := range list {
		switch list[i].KindId() {
		case k.catchClause:
			if catches == nil {
				catches = list[i:]
			}
		case k.finallyClause:
			fin = &list[i]
		}
	}
	var ff, cf flow.Frame
	if fin != nil {
		ff = j.b.OpenFinally()
	}
	if catches != nil {
		cf = j.b.OpenCatch()
	}
	if rs := n.ChildByFieldId(k.fResources); rs != nil {
		j.resources(rs, n.ChildByFieldId(k.fBody))
	} else {
		j.block(n.ChildByFieldId(k.fBody))
	}
	if catches != nil {
		t := j.b.Push()
		j.b.EnterHandler(cf, spanOf(catches[0].Child(0)))
		acc, all := t, false
		for i := range catches {
			c := &catches[i]
			if c.KindId() != k.catchClause {
				continue
			}
			j.reset()
			param := firstNamed(c)
			ct := firstNamed(param)
			for ct != nil && ct.KindId() != k.catchType {
				ct = ct.NextNamedSibling()
			}
			all = ct != nil && j.catchesAll(ct)
			kind := flow.Branch
			if all {
				kind = flow.Stmt
			}
			if ct != nil {
				j.node(kind, ct, 0, 0)
			}
			p := j.b.Push()
			mark := j.binds.mark()
			if name := param.ChildByFieldId(k.fName); name.KindId() == k.identifier {
				v := j.declare(name)
				j.def(j.node(flow.Stmt, name, 0, 0), v)
			}
			j.block(c.ChildByFieldId(k.fBody))
			j.binds.truncate(mark)
			j.b.Merge(acc)
			acc = j.b.Push()
			if all {
				break
			}
			j.b.Restore(p)
		}
		if !all {
			j.b.Throw()
			j.b.Restore(acc)
		}
		j.b.Pop(t)
	}
	if fin != nil {
		normal := j.b.EnterFinally(ff, spanOf(fin.Child(0)))
		j.block(firstNamed(fin))
		j.b.CloseFinally(ff, normal)
	}
	j.done(start)
}

// catchesAll reports whether a catch type is Throwable, which every
// exception is (JLS §11.1.1).
func (j *javaLower) catchesAll(ct *ts.Node) bool {
	t := j.text(ct)
	return string(t) == "Throwable" || string(t) == "java.lang.Throwable"
}

// resources lowers a resource specification and the try block it guards:
// each resource's acquisition, then a finally per resource around the rest,
// closed in reverse order (JLS §14.20.3.2).
func (j *javaLower) resources(spec, body *ts.Node) {
	k := j.k
	mark, fBase := j.binds.mark(), len(j.frames)
	start, list := j.kids(spec)
	for i := range list {
		r := &list[i]
		if val := r.ChildByFieldId(k.fValue); val != nil {
			j.reset()
			j.value(val)
			name := r.ChildByFieldId(k.fName)
			v := int32(-1)
			if name.KindId() == k.identifier {
				v = j.declare(name)
			}
			j.def(j.nodeAt(flow.Stmt, flow.Span{Start: uint32(name.StartByte()), End: uint32(r.EndByte())}, 0, len(j.reads)), v)
		}
		j.frames = append(j.frames, j.b.OpenFinally())
	}
	j.block(body)
	for i := len(list) - 1; i >= 0; i-- {
		r := &list[i]
		at := spanOf(r)
		if end := r.NextSibling(); end != nil {
			at = spanOf(end)
		}
		normal := j.b.EnterFinally(j.frames[fBase+i], at)
		j.reset()
		if name := r.ChildByFieldId(k.fName); name != nil {
			j.ref(name)
		} else if c := firstNamed(r); c != nil {
			j.value(c)
		}
		j.throws++
		j.node(flow.Stmt, r, 0, len(j.reads))
		j.b.CloseFinally(j.frames[fBase+i], normal)
	}
	j.done(start)
	j.frames = j.frames[:fBase]
	j.binds.truncate(mark)
}

// labeled hands a loop or switch its labels; any other statement is a block
// frame only a labelled break targets.
func (j *javaLower) labeled(n *ts.Node, labels []string) {
	k := j.k
	start, list := j.kids(n)
	if len(list) < 2 {
		j.done(start)
		j.valueNode(n)
		return
	}
	labels = append(labels, view(j.text(&list[0])))
	body := &list[len(list)-1]
	switch body.KindId() {
	case k.whileStmt, k.doStmt, k.forStmt, k.enhancedFor, k.switchExpr, k.labeledStmt:
		j.labels = labels
		j.stmt(body)
	default:
		f := j.b.OpenBlock(labels...)
		j.sub(body)
		j.b.CloseFrame(f)
	}
	j.done(start)
}

// assertStmt lowers `assert c;` and `assert c : m;`.
func (j *javaLower) assertStmt(n *ts.Node) {
	start, list := j.kids(n)
	if len(list) == 0 {
		j.done(start)
		j.valueNode(n)
		return
	}
	j.value(&list[0])
	j.node(flow.Branch, &list[0], 0, len(j.reads))
	p := j.b.Push()
	if len(list) > 1 {
		j.reset()
		j.value(&list[1])
		j.node(flow.Stmt, &list[1], 0, len(j.reads))
	}
	j.b.Throw()
	j.b.Restore(p)
	j.b.Pop(p)
	j.done(start)
}

// value lowers an expression evaluated for its value: reads are recorded,
// throwing constructs counted, and nodes created for every decision, every
// conditionally evaluated operand, every definition and every nested
// callable.
func (j *javaLower) value(n *ts.Node) {
	k := j.k
	if j.l.isCallable(n) {
		m, w := len(j.reads), len(j.writes)
		j.shadow++
		j.cap(n)
		j.shadow--
		j.closure(j.node(flow.Stmt, n, m, len(j.reads)), w)
		return
	}
	switch n.KindId() {
	case k.identifier:
		j.ref(n)
	case k.binary:
		left, right := n.ChildByFieldId(k.fLeft), n.ChildByFieldId(k.fRight)
		switch n.ChildByFieldId(k.fOperator).KindId() {
		case k.and, k.or:
			m := len(j.reads)
			j.value(left)
			j.node(flow.Branch, left, m, len(j.reads))
			p := j.b.Push()
			j.valueNode(right)
			j.b.Merge(p)
			j.b.Pop(p)
		default:
			j.value(left)
			j.value(right)
		}
	case k.ternary:
		cond := n.ChildByFieldId(k.fCondition)
		m := len(j.reads)
		j.value(cond)
		j.node(flow.Branch, cond, m, len(j.reads))
		p := j.b.Push()
		j.valueNode(n.ChildByFieldId(k.fConsequence))
		t := j.b.Push()
		j.b.Restore(p)
		j.valueNode(n.ChildByFieldId(k.fAlternative))
		j.b.Merge(t)
		j.b.Pop(p)
	case k.assignment:
		j.read(j.assign(n))
	case k.update:
		j.read(j.update(n))
	case k.methodInvocation:
		if o := n.ChildByFieldId(k.fObject); o != nil {
			j.value(o)
		}
		j.value(n.ChildByFieldId(k.fArguments))
		j.throws++
	case k.objectCreation:
		m := len(j.reads)
		var body *ts.Node
		start, list := j.kids(n)
		for i := range list {
			if list[i].KindId() == k.classBody {
				body = &list[i]
			} else {
				j.value(&list[i])
			}
		}
		j.throws++
		if body != nil {
			w := len(j.writes)
			j.shadow++
			j.cap(body)
			j.shadow--
			j.closure(j.node(flow.Stmt, n, m, len(j.reads)), w)
		}
		j.done(start)
	case k.fieldAccess:
		o := n.ChildByFieldId(k.fObject)
		j.value(o)
		if id := o.KindId(); id != k.this && id != k.super {
			j.throws++
		}
	case k.arrayAccess:
		j.value(n.ChildByFieldId(k.fArray))
		j.value(n.ChildByFieldId(k.fIndex))
		j.throws++
	case k.cast:
		j.value(n.ChildByFieldId(k.fValue))
		j.throws++
	case k.arrayCreation:
		j.children(n)
		j.throws++
	case k.instanceofExpr:
		m := len(j.reads)
		j.value(n.ChildByFieldId(k.fLeft))
		if name := n.ChildByFieldId(k.fName); name != nil {
			j.bindPattern(name, m, len(j.reads))
		} else if p := n.ChildByFieldId(k.fPattern); p != nil {
			j.bindPattern(p, m, len(j.reads))
		}
	case k.switchExpr:
		j.switchValue(n)
	case k.methodReference:
		if c := firstNamed(n); c != nil {
			j.value(c)
		}
	case k.annotation, k.markerAnnotation, k.classLiteral, k.this, k.super:
	default:
		j.children(n)
	}
}

// switchValue lowers a switch expression inside a larger expression. Its
// arms run statements, which reset the statement's reads, so the reads of
// the enclosing statement are saved around it, then every read inside it is
// added, as the node consuming its value reads them. A throw pending before
// the switch expression stays pending, so the consuming node also carries it.
func (j *javaLower) switchValue(n *ts.Node) {
	base := len(j.stack)
	j.stack = append(j.stack, j.reads...)
	throws, thrown := j.throws, j.thrown
	j.switchBlock(n, nil, true)
	j.reads = append(j.reads[:0], j.stack[base:]...)
	j.stack = j.stack[:base]
	j.stmtNo++
	for _, v := range j.reads {
		j.seen[v] = j.stmtNo
	}
	mark, w := j.binds.mark(), len(j.writes)
	j.shadow++
	j.cap(n)
	j.shadow--
	// The switch expression's own writes were lowered as nodes already.
	j.binds.truncate(mark)
	j.writes = j.writes[:w]
	j.throws, j.thrown = throws, thrown
}

// closure makes node id, which creates a lambda, an anonymous class or a
// local class, may-define every enclosing local written through a field or
// array element inside it (writes[w:]), then drops those writes.
func (j *javaLower) closure(id int32, w int) {
	for _, v := range j.writes[w:] {
		j.b.MayDef(id, v)
	}
	j.writes = j.writes[:w]
}

// children lowers n's named children for their values.
func (j *javaLower) children(n *ts.Node) {
	start, list := j.kids(n)
	for i := range list {
		j.value(&list[i])
	}
	j.done(start)
}

// base is the local at the base of a field or array target (`o` in
// `o.f.g`, `a` in `a[i][j]`), or -1.
func (j *javaLower) base(t *ts.Node) int32 {
	k := j.k
	for {
		switch t = j.l.unparen(t); t.KindId() {
		case k.fieldAccess:
			t = t.ChildByFieldId(k.fObject)
		case k.arrayAccess:
			t = t.ChildByFieldId(k.fArray)
		case k.identifier:
			return j.lookup(t)
		default:
			return -1
		}
	}
}

// reference evaluates a field or array target's object and index; the
// caller counts its throw.
func (j *javaLower) reference(t *ts.Node) {
	k := j.k
	switch t.KindId() {
	case k.fieldAccess:
		j.value(t.ChildByFieldId(k.fObject))
	case k.arrayAccess:
		j.value(t.ChildByFieldId(k.fArray))
		j.value(t.ChildByFieldId(k.fIndex))
	}
}

// targetThrows reports whether reading or writing target t may throw: an
// array element, or a field through a receiver other than this or super.
func (j *javaLower) targetThrows(t *ts.Node) bool {
	k := j.k
	switch t.KindId() {
	case k.arrayAccess:
		return true
	case k.fieldAccess:
		id := t.ChildByFieldId(k.fObject).KindId()
		return id != k.this && id != k.super
	}
	return false
}

// assign lowers `left = right` and `left op= right` as one node spanning n
// and returns the local it defines, or -1.
func (j *javaLower) assign(n *ts.Node) int32 {
	k := j.k
	left, right := j.l.unparen(n.ChildByFieldId(k.fLeft)), n.ChildByFieldId(k.fRight)
	compound := n.ChildByFieldId(k.fOperator).KindId() != k.assignOp
	m := len(j.reads)
	if left.KindId() == k.identifier {
		v := j.lookup(left)
		if compound {
			j.read(v)
		}
		j.value(right)
		j.def(j.node(flow.Stmt, n, m, len(j.reads)), v)
		return v
	}
	j.reference(left)
	throws := j.targetThrows(left)
	if compound && throws {
		j.throws++
	}
	j.value(right)
	// The store happens after the right side is evaluated (JLS §15.26.1),
	// so its throw belongs to the write's node.
	if throws {
		j.throws++
	}
	id := j.node(flow.Stmt, n, m, len(j.reads))
	if v := j.base(left); v >= 0 {
		j.b.MayDef(id, v)
	}
	return -1
}

// update lowers `x++`, `--x` and their field and array forms as one node
// spanning n and returns the local it defines, or -1.
func (j *javaLower) update(n *ts.Node) int32 {
	k := j.k
	arg := j.l.unparen(firstNamed(n))
	m := len(j.reads)
	if arg.KindId() == k.identifier {
		v := j.lookup(arg)
		j.read(v)
		j.def(j.node(flow.Stmt, n, m, len(j.reads)), v)
		return v
	}
	j.reference(arg)
	if j.targetThrows(arg) {
		j.throws++
	}
	id := j.node(flow.Stmt, n, m, len(j.reads))
	if v := j.base(arg); v >= 0 {
		j.b.MayDef(id, v)
	}
	return -1
}

// cap collects the references n makes to variables of the function being
// lowered, honouring every declaration n makes: names it declares bind -1,
// and a scope's bindings are dropped when it ends.
func (j *javaLower) cap(n *ts.Node) {
	k := j.k
	mark := j.binds.mark()
	switch n.KindId() {
	case k.identifier:
		j.ref(n)
	case k.assignment, k.update:
		t := n.ChildByFieldId(k.fLeft)
		if n.KindId() == k.update {
			t = firstNamed(n)
		}
		if t = j.l.unparen(t); t.KindId() != k.identifier {
			if v := j.base(t); v >= 0 {
				j.writes = append(j.writes, v)
			}
		}
		start, list := j.kids(n)
		for i := range list {
			j.cap(&list[i])
		}
		j.done(start)
	case k.methodInvocation:
		if o := n.ChildByFieldId(k.fObject); o != nil {
			j.cap(o)
		}
		j.cap(n.ChildByFieldId(k.fArguments))
	case k.fieldAccess:
		j.cap(n.ChildByFieldId(k.fObject))
	case k.methodReference:
		if c := firstNamed(n); c != nil {
			j.cap(c)
		}
	case k.labeledStmt:
		start, list := j.kids(n)
		if len(list) > 0 {
			j.cap(&list[len(list)-1])
		}
		j.done(start)
	case k.breakStmt, k.continueStmt, k.annotation, k.markerAnnotation:
	case k.variableDeclarator:
		if v := n.ChildByFieldId(k.fValue); v != nil {
			j.cap(v)
		}
		j.capName(n.ChildByFieldId(k.fName))
		return
	case k.formalParameter, k.catchFormalParameter:
		j.capName(n.ChildByFieldId(k.fName))
		return
	case k.inferredParameters:
		start, list := j.kids(n)
		for i := range list {
			j.capName(&list[i])
		}
		j.done(start)
		return
	case k.resource:
		if v := n.ChildByFieldId(k.fValue); v != nil {
			j.cap(v)
			j.capName(n.ChildByFieldId(k.fName))
		} else if c := firstNamed(n); c != nil {
			j.cap(c)
		}
		return
	case k.instanceofExpr:
		j.cap(n.ChildByFieldId(k.fLeft))
		if name := n.ChildByFieldId(k.fName); name != nil {
			j.capName(name)
		} else if p := n.ChildByFieldId(k.fPattern); p != nil {
			j.bindPattern(p, 0, 0)
		}
		return
	case k.pattern:
		j.bindPattern(n, 0, 0)
		return
	case k.lambda:
		if ps := n.ChildByFieldId(k.fParameters); ps.KindId() == k.identifier {
			j.capName(ps)
		} else {
			j.cap(ps)
		}
		j.cap(n.ChildByFieldId(k.fBody))
	case k.methodDecl, k.ctorDecl, k.compactCtor:
		if ps := n.ChildByFieldId(k.fParameters); ps != nil {
			j.cap(ps)
		}
		if body := n.ChildByFieldId(k.fBody); body != nil {
			j.cap(body)
		}
	case k.classDecl, k.recordDecl, k.enumDecl, k.interfaceDecl, k.annotationTypeDecl:
		j.capType(n)
	case k.classBody, k.enumBody, k.enumBodyDecls, k.interfaceBody, k.annotationTypeBody:
		j.capBody(n)
	case k.enumConstant:
		if a := n.ChildByFieldId(k.fArguments); a != nil {
			j.cap(a)
		}
		if body := n.ChildByFieldId(k.fBody); body != nil {
			j.cap(body)
		}
	default:
		start, list := j.kids(n)
		for i := range list {
			j.cap(&list[i])
		}
		j.done(start)
		if id := int(n.KindId()); id >= len(k.scope) || !k.scope[id] {
			return
		}
	}
	j.binds.truncate(mark)
}

// capName declares a name a capture walk meets, unless it is `_`.
func (j *javaLower) capName(name *ts.Node) {
	if name != nil && name.KindId() == j.k.identifier {
		j.declare(name)
	}
}

// capType collects a local type declaration's captures: its record
// components (fields of its body) and its body.
func (j *javaLower) capType(n *ts.Node) {
	k := j.k
	mark := j.binds.mark()
	if ps := n.ChildByFieldId(k.fParameters); ps != nil {
		j.cap(ps)
	}
	if body := n.ChildByFieldId(k.fBody); body != nil {
		j.cap(body)
	}
	j.binds.truncate(mark)
}

// capBody collects a type body's captures. Its fields and enum constants
// shadow an enclosing local throughout the body, whatever their position.
func (j *javaLower) capBody(n *ts.Node) {
	k := j.k
	mark := j.binds.mark()
	start, list := j.kids(n)
	for i := range list {
		switch m := &list[i]; m.KindId() {
		case k.fieldDecl, k.constantDecl:
			s, ds := j.fieldKids(m, k.fDeclarator)
			for d := range ds {
				j.capName(ds[d].ChildByFieldId(k.fName))
			}
			j.done(s)
		case k.enumConstant:
			j.capName(m.ChildByFieldId(k.fName))
		}
	}
	for i := range list {
		switch m := &list[i]; m.KindId() {
		case k.fieldDecl, k.constantDecl:
			s, ds := j.fieldKids(m, k.fDeclarator)
			for d := range ds {
				if v := ds[d].ChildByFieldId(k.fValue); v != nil {
					j.cap(v)
				}
			}
			j.done(s)
		default:
			j.cap(m)
		}
	}
	j.done(start)
	j.binds.truncate(mark)
}

// javaSyntax holds the kind and field ids the Java lowering matches,
// resolved once by name against the pinned grammar so the walk compares
// integers rather than converting every node's kind to a string.
type javaSyntax struct {
	classBody, enumBody, enumBodyDecls, interfaceBody, annotationTypeBody, staticInitializer, lambda,
	compactCtor, methodDecl, ctorDecl, recordDecl, classDecl, enumDecl, interfaceDecl, annotationTypeDecl,
	fieldDecl, constantDecl, enumConstant, block, formalParameter, spreadParameter, inferredParameters,
	variableDeclarator, localVarDecl, expressionStmt, ifStmt, whileStmt, doStmt, forStmt, enhancedFor,
	switchExpr, switchRule, switchLabel, guard, pattern, typePattern, recordPattern, recordPatternBody,
	recordPatternComponent, tryStmt, tryWithResources, resource, catchClause, catchFormalParameter, catchType,
	finallyClause, labeledStmt, returnStmt, throwStmt, yieldStmt, breakStmt, continueStmt, synchronizedStmt,
	assertStmt, explicitCtorCall, identifier, assignment, update, binary, ternary, instanceofExpr,
	methodInvocation, objectCreation, fieldAccess, arrayAccess, cast, arrayCreation, methodReference,
	annotation, markerAnnotation, classLiteral, this, super, trueLit, parenthesized uint16

	and, or, assignOp, defaultKw uint16

	fAlternative, fArguments, fArray, fBody, fCondition, fConsequence, fDeclarator, fIndex, fInit, fLeft,
	fName, fObject, fOperator, fParameters, fPattern, fResources, fRight, fUpdate, fValue uint16

	// scope marks, by kind id, the statements whose declarations a capture
	// walk drops at their end.
	scope []bool
}

var (
	javaSyntaxOnce  sync.Once
	javaSyntaxTable *javaSyntax
)

// javaSyntaxOf resolves the table once. A name the grammar does not define
// is a lowering defect and panics.
func javaSyntaxOf() *javaSyntax {
	javaSyntaxOnce.Do(func() {
		const language = "java"
		tl := mustGrammar(language)
		kind := func(name string) uint16 { return mustKind(tl, language, name, true) }
		tok := func(name string) uint16 { return mustKind(tl, language, name, false) }
		field := func(name string) uint16 { return mustField(tl, language, name) }
		s := &javaSyntax{}
		s.classBody, s.enumBody, s.enumBodyDecls = kind("class_body"), kind("enum_body"), kind("enum_body_declarations")
		s.interfaceBody, s.annotationTypeBody = kind("interface_body"), kind("annotation_type_body")
		s.staticInitializer, s.lambda = kind("static_initializer"), kind("lambda_expression")
		s.compactCtor, s.methodDecl = kind("compact_constructor_declaration"), kind("method_declaration")
		s.ctorDecl, s.recordDecl, s.classDecl = kind("constructor_declaration"), kind("record_declaration"), kind("class_declaration")
		s.enumDecl, s.interfaceDecl = kind("enum_declaration"), kind("interface_declaration")
		s.annotationTypeDecl, s.fieldDecl = kind("annotation_type_declaration"), kind("field_declaration")
		s.constantDecl, s.enumConstant, s.block = kind("constant_declaration"), kind("enum_constant"), kind("block")
		s.formalParameter, s.spreadParameter = kind("formal_parameter"), kind("spread_parameter")
		s.inferredParameters, s.variableDeclarator = kind("inferred_parameters"), kind("variable_declarator")
		s.localVarDecl, s.expressionStmt = kind("local_variable_declaration"), kind("expression_statement")
		s.ifStmt, s.whileStmt, s.doStmt = kind("if_statement"), kind("while_statement"), kind("do_statement")
		s.forStmt, s.enhancedFor = kind("for_statement"), kind("enhanced_for_statement")
		s.switchExpr, s.switchRule, s.switchLabel = kind("switch_expression"), kind("switch_rule"), kind("switch_label")
		s.guard, s.pattern, s.typePattern = kind("guard"), kind("pattern"), kind("type_pattern")
		s.recordPattern, s.recordPatternBody = kind("record_pattern"), kind("record_pattern_body")
		s.recordPatternComponent, s.tryStmt = kind("record_pattern_component"), kind("try_statement")
		s.tryWithResources, s.resource = kind("try_with_resources_statement"), kind("resource")
		s.catchClause, s.catchFormalParameter = kind("catch_clause"), kind("catch_formal_parameter")
		s.catchType, s.finallyClause, s.labeledStmt = kind("catch_type"), kind("finally_clause"), kind("labeled_statement")
		s.returnStmt, s.throwStmt, s.yieldStmt = kind("return_statement"), kind("throw_statement"), kind("yield_statement")
		s.breakStmt, s.continueStmt = kind("break_statement"), kind("continue_statement")
		s.synchronizedStmt, s.assertStmt = kind("synchronized_statement"), kind("assert_statement")
		s.explicitCtorCall, s.identifier = kind("explicit_constructor_invocation"), kind("identifier")
		s.assignment, s.update, s.binary = kind("assignment_expression"), kind("update_expression"), kind("binary_expression")
		s.ternary, s.instanceofExpr = kind("ternary_expression"), kind("instanceof_expression")
		s.methodInvocation, s.objectCreation = kind("method_invocation"), kind("object_creation_expression")
		s.fieldAccess, s.arrayAccess, s.cast = kind("field_access"), kind("array_access"), kind("cast_expression")
		s.arrayCreation, s.methodReference = kind("array_creation_expression"), kind("method_reference")
		s.annotation, s.markerAnnotation, s.classLiteral = kind("annotation"), kind("marker_annotation"), kind("class_literal")
		s.this, s.super, s.trueLit = kind("this"), kind("super"), kind("true")
		s.parenthesized = kind("parenthesized_expression")
		s.and, s.or, s.assignOp, s.defaultKw = tok("&&"), tok("||"), tok("="), tok("default")
		s.fAlternative, s.fArguments, s.fArray, s.fBody = field("alternative"), field("arguments"), field("array"), field("body")
		s.fCondition, s.fConsequence, s.fDeclarator = field("condition"), field("consequence"), field("declarator")
		s.fIndex, s.fInit, s.fLeft, s.fName = field("index"), field("init"), field("left"), field("name")
		s.fObject, s.fOperator, s.fParameters = field("object"), field("operator"), field("parameters")
		s.fPattern, s.fResources, s.fRight = field("pattern"), field("resources"), field("right")
		s.fUpdate, s.fValue = field("update"), field("value")
		s.scope = make([]bool, tl.NodeKindCount())
		for _, name := range []string{"block", "constructor_body", "switch_block_statement_group", "switch_rule",
			"for_statement", "enhanced_for_statement", "catch_clause", "try_with_resources_statement", "if_statement",
			"while_statement"} {
			s.scope[kind(name)] = true
		}
		javaSyntaxTable = s
	})
	return javaSyntaxTable
}
