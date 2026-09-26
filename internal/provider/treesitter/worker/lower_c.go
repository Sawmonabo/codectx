package worker

import (
	"strconv"
	"sync"

	ts "github.com/tree-sitter/go-tree-sitter"

	"github.com/Sawmonabo/codectx/internal/provider/treesitter/flow"
)

// cLowering lowers C function definitions.
var cLowering = Lowering{
	language:  "c",
	callables: []string{"function_definition"},
	lower:     lowerC,
}

// cppLowering lowers C++ function definitions (a definition inside a class
// body is a method) and lambda expressions, over the same implementation as
// cLowering with the C++ additions resolved against the C++ grammar only.
var cppLowering = Lowering{
	language:  "cpp",
	callables: []string{"function_definition", "lambda_expression"},
	lower:     lowerC,
}

// Synthetic frame labels. Each begins with a space, which no C or C++
// identifier can contain, so no source label ever names these frames.
const (
	// cLeaveLabel names the block frame around a `__try` body, the target of
	// `__leave`.
	cLeaveLabel = " __leave"
	// cCatchLabel names the block frame collecting the ends of a try
	// statement's handler bodies.
	cCatchLabel = " catch"
	// cSkipLabel names the block frame that parks a switch's no-match path
	// while the statements before its first case label are lowered.
	cSkipLabel = " skip"
	// cCasePrefix begins the goto label of a case label nested below another
	// statement of its switch body; the case label's start byte follows.
	cCasePrefix = " case"
)

// lowerC lowers one C or C++ callable's parameters and body into b.
//
// # Node granularity
//
// The goldens render nodes by source text, so the spans are part of the
// contract:
//
//   - After Entry, one Stmt node per named parameter, in declaration order,
//     spanning its identifier and defining it (C17 §6.9.1). An identifier
//     list of an old-style definition names the parameters the same way. A
//     C++ default argument is evaluated by the caller and makes no node. A
//     constructor's member initializers follow, one Stmt node per
//     initializer spanning it ([class.base.init]).
//   - A statement is one node: an expression statement spans its
//     expression (a comma expression at statement level is one statement
//     per operand), an initialized declarator spans the init declarator and
//     defines the name, return, co_return, throw, break, continue, goto and
//     `__leave` span the statement (kind Jump). An expression statement
//     that is an assignment or update of an identifier is its defining
//     node. A declarator without an initializer makes no node (the object's
//     value is indeterminate, C17 §6.7.9p10), unless a variable-length array
//     size is evaluated there: then one Stmt node spans the declarator and
//     reads the size. A structured binding is one defining node per name,
//     spanning the name, each reading the initializer. A `static` local is a
//     variable whose initializer is a defining node at its position; the
//     value it keeps across calls is not modelled.
//   - A condition is one Branch node spanning the condition without its
//     parentheses: if, while, do…while, for; a C++ condition that declares
//     a variable is a Branch node spanning the declaration that defines it.
//     An init-statement of a C++ if, switch or range for is lowered before
//     the condition as a statement. A loop whose condition is `true` or a
//     nonzero integer literal (`while (1)`), and a for without a condition,
//     has no exit edge: its head is a Stmt node spanning the literal, or the
//     `for` keyword.
//   - `&&`, `||` and the conditional operator: the deciding operand is a
//     Branch node spanning it, created after the nodes of everything it
//     evaluates; each conditionally evaluated operand is a Stmt node
//     spanning it (C17 §6.5.13–§6.5.15). The two-operand conditional
//     `a ?: b` evaluates only b conditionally.
//   - A switch is a Stmt node for the controlling expression, then one
//     Branch node per case label spanning the label's value, in source
//     order, each Using the controlling expression's variables (the outcome
//     compares against it; case values are constants), then the case
//     bodies in source order, each entered from its label and falling into
//     the next body unless it jumps (C17 §6.8.4.2). `default` is taken when
//     no label matches, whatever its position. A case or default label
//     nested below another statement of the switch body (a label inside a
//     loop of the body, or inside a preprocessor conditional) is reached
//     through the builder's Label and Goto: its Branch node jumps to a
//     label node spanning the `case` or `default` keyword, named by
//     cCasePrefix and the label's start byte, a name built only for such
//     nested labels (two small allocations per nested label). Statements
//     before the first case label are reachable only by a jump into them.
//   - A C++ range for ([stmt.ranged]) is a Stmt node for the range
//     expression, evaluated once, which defines an iteration variable of the
//     lowering's own; a Branch head spanning from the declarator to the end
//     of the range expression, which Uses it; then one defining node per
//     bound name spanning the name, which Uses it too.
//   - Every label is its own Stmt node spanning the label identifier, before
//     the statement it labels, so a goto always lands on it (C17 §6.8.6.1).
//     An assembly statement with goto labels is a Branch node spanning the
//     expression, with an edge to each label and one to the next statement.
//   - A preprocessor conditional inside a function body (`#if`, `#ifdef`,
//     `#ifndef`, `#elif`, `#elifdef`, `#elifndef`, `#else`) keeps every
//     branch as reachable code, since the lowering cannot know which one
//     the build selects: a Branch node spans the directive's condition (the
//     macro name of an `#ifdef` form, the `#else` keyword for `#else`), each
//     arm is a path from it and the arms are merged after the directive, so
//     no arm sees another arm's definitions. A declaration in one arm and a
//     redeclaration of the same name in a sibling arm at the same block
//     level are one variable, so a use after the directive sees both. The
//     condition reads no variable. Every other preprocessor line inside a
//     body produces no node.
//   - A lambda or a nested function definition is its own function; in the
//     enclosing function its creating expression is one Stmt node spanning
//     it (see Captures).
//   - An expression lowered for its value and ended by a node spanning it
//     takes no second node when the last node its own lowering made already
//     spans it.
//
// Kinds that make no node: type definitions and type specifiers
// (struct, union, enum, class), preprocessor lines other than
// conditionals, linkage specifications, and the C++ using, alias,
// namespace, static_assert, template and concept declarations. A
// `co_yield` statement is a plain Stmt node and `co_await` a plain operand
// ([expr.await], [expr.yield]); the suspension is not a CFG edge. Every kind
// not named above is a plain Stmt node carrying its reads and falling
// through.
//
// # Uses
//
// Only an identifier resolving to a variable declared in this function is
// a Use; a qualified name, a field name, `this` and the operand of sizeof,
// alignof and offsetof (unevaluated, C17 §6.5.3.4p2) are not. A node Uses
// every variable read inside its own span; an assignment, compound
// assignment or update of an identifier embedded in a larger expression is
// its own defining node, and the variable it defined is a read of the node
// that evaluates the enclosing expression. A node that defines v also Uses
// v when its statement read v before it. A write through `*p`, `p->f`,
// `a[i]` or `s.f` (and an assembly output operand) Uses its operands and is
// a non-killing may-definition of the base variable on the node that
// consumes it. So is taking an address, `&x` (`&s.f`, `&a[i]`), by the
// address-taking rule of Lowering: it Uses its operands and may-defines its
// base variable. A C++ reference binding and pointer aliasing are not
// modelled. Destructors,
// setjmp/longjmp and signal handlers are not modelled. A GNU statement
// expression `({ … })` is lowered as its block in place; the node of the
// enclosing expression Uses its last statement's reads.
//
// # Exceptions
//
// C++ ([except]): inside a try block MayThrow is given to every node whose
// own evaluation contains a call, a `new`, a direct initialization or a
// range for's iterator step; a throw statement's node is also a Throw. A
// try statement's handlers are tested in order after the Handler node
// spanning the first `catch` keyword: each typed clause is a Branch node
// spanning its parameter list that defines the caught name, true into its
// body, false to the next clause; the fringe that matches no clause ends in
// Throw, and `catch (...)` ends the chain. A function-try-block covers the
// member initializers and the body; for a constructor or destructor the end
// of a handler rethrows ([except.handle]/15), otherwise it returns.
//
// Structured exceptions (both grammars): inside a `__try` body every call
// and every dereference (`*p`, `->`, `[]`) may raise. `__try/__finally` is
// a finally; `__try/__except (filter)` is a catch whose filter is a Branch
// node spanning the filter expression, true into the handler, false
// rethrowing; resumption at the fault is not modelled. `__leave` breaks to
// the end of the `__try` body. The builder applies MayThrow only inside an
// open catch or finally frame, so outside every try a call throws nowhere;
// a dereference inside a C++ try nested in a `__try` is also given
// MayThrow, an over-approximation.
//
// # Scoping
//
// A compound statement, and each selection and iteration statement with its
// sub-statements, is a block (C17 §6.8p3, §6.8.4p3, §6.8.5p5); a name is in
// scope from its declarator on, so its initializer already sees it
// (§6.2.1p7). A C++ catch parameter is scoped to its handler, a condition
// declaration to its statement, a range for's names to its loop. A name a
// block declares without it being a variable (a function prototype) shadows
// with no variable.
//
// # Captures
//
// A lambda's creating node ([expr.prim.lambda.capture]) Uses every
// enclosing variable its capture list names, its init-captures read and its
// body references, resolved with the lambda's own scopes so a name it
// declares shadows. A variable captured by reference (named `&x`, or
// implicitly under a `&` default) that the body writes, or whose address it
// takes, is a may-definition
// on the creating node; a by-copy capture never is. A nested function
// definition (a GNU extension) accesses every enclosing variable by
// reference.
func lowerC(l *Lowering, b *flow.Builder, fn *ts.Node, src []byte, s *Scratch) {
	k := cSyntaxOf(l.language)
	cur := s.cursor(fn)
	c := cLower{l: l, b: b, src: src, k: k, cur: cur, binds: &s.scope, pp: -1, first: -1, last: -1, stmtNo: 1}
	if fn.KindId() == k.lambdaExpression {
		if d := fn.ChildByFieldId(k.fDeclarator); d != nil {
			c.params(d.ChildByFieldId(k.fParameters))
		}
		c.block(fn.ChildByFieldId(k.fBody))
		return
	}
	c.params(c.parameters(fn.ChildByFieldId(k.fDeclarator)))
	body := fn.ChildByFieldId(k.fBody)
	if body == nil {
		return
	}
	if body.KindId() == k.tryStatement {
		c.tryStmt(body, c.structor(fn))
		return
	}
	start, list := c.kids(fn)
	for i := range list {
		if list[i].KindId() == k.fieldInitializerList {
			c.initializers(&list[i])
		}
	}
	c.done(start)
	c.block(body)
}

// cLower is the state of lowering one C or C++ callable.
type cLower struct {
	l   *Lowering
	b   *flow.Builder
	src []byte
	k   *cSyntax
	cur *ts.TreeCursor
	// buf is a stack of child lists; kids pushes one and done pops it.
	buf []ts.Node
	// binds is the scope chain; blockMark is where the innermost block's
	// bindings begin, and pp where the innermost preprocessor conditional at
	// that block level began, or -1.
	binds     *scope
	blockMark int
	pp        int
	// shadow is non-zero while walking a nested callable for its captures:
	// declarations then bind -1 and no node is created.
	shadow int
	// reads are the variables read by the current statement from base on;
	// seen[v] == stmtNo marks v as one of them. base is non-zero inside a
	// statement expression, whose statements keep the enclosing reads.
	reads  []int32
	seen   []int
	stmtNo int
	base   int
	// may are the may-definitions the next node created carries.
	may []int32
	// writes are the enclosing variables written inside the nested callable
	// whose captures are being collected.
	writes []int32
	// throws counts throwing constructs evaluated by the current statement;
	// those past thrown are not yet attached to a node. seh is the depth of
	// open `__try` bodies, where a dereference may raise.
	throws, thrown int
	seh            int
	// first is the first node created since the last open, or -1; last is
	// the node created last, or -1, and lastSpan its span.
	first    int32
	last     int32
	lastSpan flow.Span
	// hs holds the case-label fringes of the open switches.
	hs []flow.Fringe
}

// kids pushes n's named, non-extra children onto buf and returns the stack
// mark and the list; done(mark) pops them.
func (c *cLower) kids(n *ts.Node) (int, []ts.Node) { return c.collect(n, true, 0, 0, 0) }

// body is kids without the children in fields f1, f2 and f3 (0 matches no
// field): a case label's value, a directive's condition and alternative.
func (c *cLower) body(n *ts.Node, f1, f2, f3 uint16) (int, []ts.Node) {
	return c.collect(n, true, f1, f2, f3)
}

// tokens is kids with the anonymous children included.
func (c *cLower) tokens(n *ts.Node) (int, []ts.Node) { return c.collect(n, false, 0, 0, 0) }

func (c *cLower) collect(n *ts.Node, named bool, f1, f2, f3 uint16) (int, []ts.Node) {
	start := len(c.buf)
	cur := c.cur
	cur.Reset(*n)
	if cur.GotoFirstChild() {
		for {
			x := cur.Node()
			f := cur.FieldId()
			if (x.IsNamed() || !named) && !x.IsExtra() && (f == 0 || f != f1 && f != f2 && f != f3) {
				c.buf = append(c.buf, *x)
			}
			if !cur.GotoNextSibling() {
				break
			}
		}
	}
	return start, c.buf[start:]
}

func (c *cLower) done(mark int) { c.buf = c.buf[:mark] }

func (c *cLower) text(n *ts.Node) []byte { return textOf(c.src, n) }

// cScope is a scope opened by open and closed by close.
type cScope struct{ mark, block int }

func (c *cLower) open() cScope {
	s := cScope{mark: c.binds.mark(), block: c.blockMark}
	c.blockMark = c.binds.mark()
	return s
}

func (c *cLower) close(s cScope) {
	c.binds.truncate(s.mark)
	c.blockMark = s.block
}

// declare binds name in the innermost scope: a new variable, the variable a
// sibling arm of the enclosing preprocessor conditional declared under the
// same name at this block level, or -1 inside a nested callable.
func (c *cLower) declare(name *ts.Node) int32 {
	t := c.text(name)
	v := int32(-1)
	switch {
	case c.shadow > 0:
	case c.pp >= 0 && c.pp >= c.blockMark:
		if i := c.binds.find(t, c.pp); i >= 0 {
			v = c.binds.at(i).v
			break
		}
		v = c.b.Var()
	default:
		v = c.b.Var()
	}
	c.binds.push(t, v)
	return v
}

// hide binds name to no variable.
func (c *cLower) hide(name *ts.Node) { c.binds.push(c.text(name), -1) }

func (c *cLower) lookup(name *ts.Node) int32 { return c.binds.lookup(c.text(name)) }

func (c *cLower) ref(name *ts.Node) { c.read(c.lookup(name)) }

// read records a read of v, unless v is -1.
func (c *cLower) read(v int32) {
	if v < 0 {
		return
	}
	c.reads = append(c.reads, v)
	if int(v) >= len(c.seen) {
		c.seen = append(c.seen, make([]int, int(v)+1-len(c.seen))...)
	}
	c.seen[v] = c.stmtNo
}

// reset starts a statement: nothing is read and no throw is pending.
func (c *cLower) reset() {
	c.reads, c.throws, c.thrown = c.reads[:c.base], 0, 0
	if c.base == 0 {
		c.may = c.may[:0]
	}
	c.stmtNo++
}

// node creates a node spanning n that Uses reads[from:to], carries the
// pending may-definitions, and MayThrow when a throwing construct was
// evaluated since the previous node.
func (c *cLower) node(kind flow.Kind, n *ts.Node, from, to int) int32 {
	return c.nodeAt(kind, spanOf(n), from, to)
}

func (c *cLower) nodeAt(kind flow.Kind, s flow.Span, from, to int) int32 {
	id := c.b.Node(kind, s)
	if c.first < 0 {
		c.first = id
	}
	for _, v := range c.reads[from:to] {
		c.b.Use(id, v)
	}
	for _, v := range c.may {
		c.b.MayDef(id, v)
	}
	c.may = c.may[:0]
	if c.throws > c.thrown {
		c.b.MayThrow(id)
	}
	c.thrown = c.throws
	c.last, c.lastSpan = id, s
	return id
}

// def records that node n defines v and, when the statement read v before
// n, that n Uses v.
func (c *cLower) def(n, v int32) {
	if v < 0 {
		return
	}
	c.b.Def(n, v)
	if int(v) < len(c.seen) && c.seen[v] == c.stmtNo {
		c.b.Use(n, v)
	}
}

// mark starts tracking the first node created; unmark returns it (-1 if
// none) and restores the enclosing tracking.
func (c *cLower) mark() int32 {
	s := c.first
	c.first = -1
	return s
}

func (c *cLower) unmark(saved int32) int32 {
	h := c.first
	if saved >= 0 {
		c.first = saved
	}
	return h
}

// params declares every named parameter of the parameter list ps, each as a
// defining node spanning its identifier.
func (c *cLower) params(ps *ts.Node) {
	if ps == nil {
		return
	}
	k := c.k
	start, list := c.kids(ps)
	for i := range list {
		p := &list[i]
		id := p
		if p.KindId() != k.identifier {
			// A parameter declared as a function is a pointer (C17 §6.7.6.3p8).
			if id, _ = c.name(p.ChildByFieldId(k.fDeclarator)); id == nil {
				continue
			}
		}
		c.reset()
		v := c.declare(id)
		c.def(c.node(flow.Stmt, id, c.base, c.base), v)
	}
	c.done(start)
}

// parameters is the parameter list of the innermost function declarator of
// a definition's declarator chain (a function returning a function pointer
// has an outer one), or nil.
func (c *cLower) parameters(d *ts.Node) *ts.Node {
	k := c.k
	var ps *ts.Node
	for d != nil {
		switch d.KindId() {
		case k.functionDeclarator:
			ps = d.ChildByFieldId(k.fParameters)
			d = d.ChildByFieldId(k.fDeclarator)
		case k.pointerDeclarator, k.arrayDeclarator:
			d = d.ChildByFieldId(k.fDeclarator)
		case k.parenthesizedDeclarator, k.attributedDeclarator, k.referenceDeclarator:
			d = c.inner(d)
		default:
			return ps
		}
	}
	return ps
}

// structor reports whether a C++ definition is a constructor or destructor:
// it has no return type and is not a conversion function.
func (c *cLower) structor(fn *ts.Node) bool {
	k := c.k
	d := fn.ChildByFieldId(k.fDeclarator)
	return fn.ChildByFieldId(k.fType) == nil && d != nil && d.KindId() != k.operatorCast
}

// inner is the declarator a parenthesized, attributed, reference or
// variadic declarator wraps.
func (c *cLower) inner(d *ts.Node) *ts.Node {
	k := c.k
	start, list := c.kids(d)
	defer c.done(start)
	for i := range list {
		if id := list[i].KindId(); id != k.attributeDeclaration && id != k.msCallModifier {
			x := list[i]
			return &x
		}
	}
	return nil
}

// name is the identifier declarator d declares, or nil for a qualified,
// field or operator name, which is no local; fn reports that it declares a
// function (a block-scope prototype) rather than an object: the type
// constructor nearest the identifier is a function declarator, so
// `int (*fp)(int)` declares an object and `int g(int)` a function.
func (c *cLower) name(d *ts.Node) (id *ts.Node, fn bool) {
	k := c.k
	for d != nil {
		switch d.KindId() {
		case k.identifier:
			return d, fn
		case k.functionDeclarator:
			fn = true
			d = d.ChildByFieldId(k.fDeclarator)
		case k.pointerDeclarator, k.arrayDeclarator:
			fn = false
			d = d.ChildByFieldId(k.fDeclarator)
		case k.initDeclarator:
			d = d.ChildByFieldId(k.fDeclarator)
		case k.referenceDeclarator:
			fn = false
			d = c.inner(d)
		case k.parenthesizedDeclarator, k.attributedDeclarator, k.variadicDeclarator:
			d = c.inner(d)
		default:
			return nil, false
		}
	}
	return nil, false
}

// variable is the identifier declarator d declares as a variable, or nil.
func (c *cLower) variable(d *ts.Node) *ts.Node {
	if id, fn := c.name(d); !fn {
		return id
	}
	return nil
}

// binding is the structured binding declarator below reference
// declarators, or nil.
func (c *cLower) binding(d *ts.Node) *ts.Node {
	k := c.k
	for d != nil && d.KindId() == k.referenceDeclarator {
		d = c.inner(d)
	}
	if d != nil && d.KindId() == k.structuredBindingDeclarator {
		return d
	}
	return nil
}

// sizes reads every array size along declarator d.
func (c *cLower) sizes(d *ts.Node) {
	k := c.k
	for d != nil {
		switch d.KindId() {
		case k.arrayDeclarator:
			if s := d.ChildByFieldId(k.fSize); s != nil {
				c.value(s)
			}
			d = d.ChildByFieldId(k.fDeclarator)
		case k.pointerDeclarator:
			d = d.ChildByFieldId(k.fDeclarator)
		case k.parenthesizedDeclarator, k.attributedDeclarator, k.referenceDeclarator:
			d = c.inner(d)
		default:
			return
		}
	}
}

// initializers lowers a member initializer list: one throwing Stmt node per
// initializer.
func (c *cLower) initializers(n *ts.Node) {
	start, list := c.kids(n)
	for i := range list {
		c.reset()
		c.children(&list[i])
		c.throws++
		c.node(flow.Stmt, &list[i], c.base, len(c.reads))
	}
	c.done(start)
}

// block lowers a compound statement in its own scope.
func (c *cLower) block(n *ts.Node) {
	s := c.open()
	start, list := c.kids(n)
	for i := range list {
		c.stmt(&list[i])
	}
	c.done(start)
	c.close(s)
}

// sub lowers a sub-statement of a selection or iteration statement, a block
// of its own.
func (c *cLower) sub(n *ts.Node) {
	s := c.open()
	c.stmt(n)
	c.close(s)
}

// stmt lowers one statement.
func (c *cLower) stmt(n *ts.Node) {
	k := c.k
	c.reset()
	id := n.KindId()
	if c.l.isCallable(n) {
		c.closure(n)
		return
	}
	if int(id) < len(k.noNode) && k.noNode[id] {
		return
	}
	switch id {
	case k.expressionStatement:
		if e := firstNamed(n); e != nil {
			c.exprStmt(e)
		}
	case k.declaration:
		c.declaration(n)
	case k.compoundStatement:
		c.block(n)
	case k.ifStatement:
		c.ifStmt(n)
	case k.whileStatement:
		c.whileStmt(n)
	case k.doStatement:
		c.doStmt(n)
	case k.forStatement:
		c.forStmt(n)
	case k.forRangeLoop:
		c.forRange(n)
	case k.switchStatement:
		c.switchStmt(n)
	case k.caseStatement:
		c.b.Label(c.caseName(n), spanOf(n.Child(0)))
		c.caseBody(n)
	case k.labeledStatement:
		name := n.ChildByFieldId(k.fLabel)
		c.b.Label(view(c.text(name)), spanOf(name))
		start, list := c.body(n, k.fLabel, 0, 0)
		for i := range list {
			c.stmt(&list[i])
		}
		c.done(start)
	case k.attributedStatement:
		start, list := c.kids(n)
		for i := range list {
			if list[i].KindId() != k.attributeDeclaration {
				c.stmt(&list[i])
			}
		}
		c.done(start)
	case k.gotoStatement:
		c.node(flow.Jump, n, c.base, c.base)
		c.b.Goto(view(c.text(n.ChildByFieldId(k.fLabel))))
	case k.breakStatement:
		c.node(flow.Jump, n, c.base, c.base)
		c.b.Break("")
	case k.continueStatement:
		c.node(flow.Jump, n, c.base, c.base)
		c.b.Continue("")
	case k.sehLeaveStatement:
		c.node(flow.Jump, n, c.base, c.base)
		c.b.Break(cLeaveLabel)
	case k.returnStatement, k.coReturnStatement:
		if e := firstNamed(n); e != nil {
			c.value(e)
		}
		c.node(flow.Jump, n, c.base, len(c.reads))
		c.b.Return()
	case k.throwStatement:
		if e := firstNamed(n); e != nil {
			c.value(e)
		}
		c.node(flow.Jump, n, c.base, len(c.reads))
		c.b.Throw()
	case k.tryStatement:
		c.tryStmt(n, false)
	case k.sehTryStatement:
		c.sehTry(n)
	case k.preprocIf, k.preprocIfdef:
		c.preproc(n)
	default:
		c.valueNode(n)
	}
}

// exprStmt lowers an expression evaluated for its effect.
func (c *cLower) exprStmt(e *ts.Node) {
	k := c.k
	m := len(c.reads)
	made := false
	switch u := c.l.unparen(e); u.KindId() {
	case k.commaExpression:
		c.exprStmt(u.ChildByFieldId(k.fLeft))
		c.reset()
		c.exprStmt(u.ChildByFieldId(k.fRight))
		return
	case k.assignmentExpression:
		made, _ = c.assign(u)
	case k.updateExpression:
		made, _ = c.update(u)
	case k.gnuAsmExpression:
		c.asm(u)
		return
	default:
		c.valueNode(e)
		return
	}
	if made && c.thrown == c.throws && len(c.may) == 0 {
		return
	}
	c.node(flow.Stmt, e, m, len(c.reads))
}

// asm lowers an assembly statement: one node, a Branch with an edge to each
// goto label when it has any.
func (c *cLower) asm(e *ts.Node) {
	k := c.k
	m := len(c.reads)
	c.value(e)
	labels := e.ChildByFieldId(k.fGotoLabels)
	if labels == nil {
		c.node(flow.Stmt, e, m, len(c.reads))
		return
	}
	c.node(flow.Branch, e, m, len(c.reads))
	start, list := c.kids(labels)
	for i := range list {
		p := c.b.Push()
		c.b.Goto(view(c.text(&list[i])))
		c.b.Restore(p)
		c.b.Pop(p)
	}
	c.done(start)
}

// valueNode lowers n for its value and ends it with a Stmt node spanning n,
// unless the last node that lowering made already spans n, or n without its
// parentheses, with nothing pending after it.
func (c *cLower) valueNode(n *ts.Node) {
	m, last := len(c.reads), c.last
	c.value(n)
	if c.last != last && c.lastSpan == spanOf(c.l.unparen(n)) && c.thrown == c.throws && len(c.may) == 0 {
		return
	}
	c.node(flow.Stmt, n, m, len(c.reads))
}

// declaration lowers a block-scope declaration: one node per initialized
// declarator (see Node granularity).
func (c *cLower) declaration(n *ts.Node) {
	k := c.k
	cur := c.cur
	start := len(c.buf)
	cur.Reset(*n)
	if cur.GotoFirstChild() {
		for {
			if cur.FieldId() == k.fDeclarator {
				c.buf = append(c.buf, *cur.Node())
			}
			if !cur.GotoNextSibling() {
				break
			}
		}
	}
	list := c.buf[start:]
	for i := range list {
		d := &list[i]
		m := len(c.reads)
		if d.KindId() != k.initDeclarator {
			id, fn := c.name(d)
			if fn {
				c.hide(id)
				continue
			}
			v := int32(-1)
			if id != nil {
				v = c.declare(id)
			}
			c.sizes(d)
			// A declaration-level initializer (a C++ condition declaration)
			// initializes the declarator.
			if val := n.ChildByFieldId(k.fValue); val != nil {
				c.value(val)
				c.def(c.node(flow.Stmt, n, m, len(c.reads)), v)
			} else if len(c.reads) > m {
				c.node(flow.Stmt, d, m, len(c.reads))
			}
			continue
		}
		inner, val := d.ChildByFieldId(k.fDeclarator), d.ChildByFieldId(k.fValue)
		if sb := c.binding(inner); sb != nil {
			c.value(val)
			to := len(c.reads)
			s2, names := c.kids(sb)
			for j := range names {
				v := c.declare(&names[j])
				c.def(c.node(flow.Stmt, &names[j], m, to), v)
			}
			c.done(s2)
			continue
		}
		v := int32(-1)
		if id := c.variable(inner); id != nil {
			v = c.declare(id)
		}
		c.sizes(inner)
		c.value(val)
		if val.KindId() == k.argumentList || val.KindId() == k.initializerList && k.cpp {
			c.throws++
		}
		c.def(c.node(flow.Stmt, d, m, len(c.reads)), v)
	}
	c.done(start)
}

// cond lowers a condition as one Branch node (after an init-statement, for
// a C++ condition clause) and returns the node.
func (c *cLower) cond(n *ts.Node) int32 {
	k := c.k
	c.reset()
	if n.KindId() == k.conditionClause {
		if init := n.ChildByFieldId(k.fInitializer); init != nil {
			c.initStmt(init)
			c.reset()
		}
		n = n.ChildByFieldId(k.fValue)
		if n.KindId() == k.declaration {
			return c.condDecl(n, flow.Branch)
		}
	}
	e := c.l.unparen(n)
	m := len(c.reads)
	c.value(e)
	return c.node(flow.Branch, e, m, len(c.reads))
}

// condDecl lowers a C++ condition declaration `T x = e` as one node of kind
// kind spanning it that defines x.
func (c *cLower) condDecl(d *ts.Node, kind flow.Kind) int32 {
	k := c.k
	m := len(c.reads)
	decl, val := d.ChildByFieldId(k.fDeclarator), d.ChildByFieldId(k.fValue)
	if decl != nil && decl.KindId() == k.initDeclarator {
		decl, val = decl.ChildByFieldId(k.fDeclarator), decl.ChildByFieldId(k.fValue)
	}
	v := int32(-1)
	if id := c.variable(decl); id != nil {
		v = c.declare(id)
	}
	if val != nil {
		c.value(val)
	}
	id := c.node(kind, d, m, len(c.reads))
	c.def(id, v)
	return id
}

// initStmt lowers a C++ init-statement.
func (c *cLower) initStmt(init *ts.Node) {
	if s := firstNamed(init); s != nil {
		c.stmt(s)
	}
}

func (c *cLower) ifStmt(n *ts.Node) {
	k := c.k
	s := c.open()
	c.cond(n.ChildByFieldId(k.fCondition))
	p := c.b.Push()
	c.sub(n.ChildByFieldId(k.fConsequence))
	t := c.b.Push()
	c.b.Restore(p)
	if alt := n.ChildByFieldId(k.fAlternative); alt != nil {
		if e := firstNamed(alt); e != nil {
			c.sub(e)
		}
	}
	c.b.Merge(t)
	c.b.Pop(p)
	c.close(s)
}

// forever reports whether a loop condition is `true` or a nonzero decimal
// or octal integer literal.
func (c *cLower) forever(e *ts.Node) bool {
	switch e.KindId() {
	case c.k.trueLit:
		return true
	case c.k.numberLiteral:
		nonzero := false
		for _, ch := range c.text(e) {
			if ch < '0' || ch > '9' {
				return false
			}
			nonzero = nonzero || ch != '0'
		}
		return nonzero
	}
	return false
}

// head lowers a loop condition as the loop's decision node, or as a Stmt
// node without an exit edge when it is always true. It reports whether the
// loop exits through it.
func (c *cLower) head(cond *ts.Node) bool {
	k := c.k
	c.reset()
	e := cond
	if e.KindId() == k.conditionClause && e.ChildByFieldId(k.fInitializer) == nil {
		e = e.ChildByFieldId(k.fValue)
	}
	if e = c.l.unparen(e); c.forever(e) {
		c.node(flow.Stmt, e, c.base, c.base)
		return false
	}
	c.cond(cond)
	return true
}

// loopEnd closes a loop whose back edge targets h.
func (c *cLower) loopEnd(f flow.Frame, h int32, exits bool, exit flow.Fringe) {
	c.b.Close(h)
	if exits {
		c.b.Restore(exit)
	}
	c.b.CloseFrame(f)
	if exits {
		c.b.Pop(exit)
	}
}

func (c *cLower) whileStmt(n *ts.Node) {
	k := c.k
	s := c.open()
	f := c.b.OpenLoop()
	saved := c.mark()
	exits := c.head(n.ChildByFieldId(k.fCondition))
	h := c.unmark(saved)
	var exit flow.Fringe
	if exits {
		exit = c.b.Push()
	}
	c.sub(n.ChildByFieldId(k.fBody))
	c.b.ContinueHere(f)
	c.loopEnd(f, h, exits, exit)
	c.close(s)
}

func (c *cLower) doStmt(n *ts.Node) {
	k := c.k
	f := c.b.OpenLoop()
	saved := c.mark()
	c.sub(n.ChildByFieldId(k.fBody))
	c.b.ContinueHere(f)
	exits := c.head(n.ChildByFieldId(k.fCondition))
	h := c.unmark(saved)
	var exit flow.Fringe
	if exits {
		exit = c.b.Push()
	}
	c.loopEnd(f, h, exits, exit)
}

func (c *cLower) forStmt(n *ts.Node) {
	k := c.k
	s := c.open()
	if init := n.ChildByFieldId(k.fInitializer); init != nil {
		if init.KindId() == k.declaration {
			c.stmt(init)
		} else {
			c.reset()
			c.exprStmt(init)
		}
	}
	f := c.b.OpenLoop()
	saved := c.mark()
	exits := false
	if cond := n.ChildByFieldId(k.fCondition); cond == nil {
		c.reset()
		c.node(flow.Stmt, n.Child(0), c.base, c.base)
	} else {
		exits = c.head(cond)
	}
	h := c.unmark(saved)
	var exit flow.Fringe
	if exits {
		exit = c.b.Push()
	}
	c.sub(n.ChildByFieldId(k.fBody))
	c.b.ContinueHere(f)
	if upd := n.ChildByFieldId(k.fUpdate); upd != nil {
		c.reset()
		c.exprStmt(upd)
	}
	c.loopEnd(f, h, exits, exit)
	c.close(s)
}

// forRange lowers a C++ range for (see Node granularity).
func (c *cLower) forRange(n *ts.Node) {
	k := c.k
	s := c.open()
	if init := n.ChildByFieldId(k.fInitializer); init != nil {
		c.initStmt(init)
	}
	c.reset()
	right := n.ChildByFieldId(k.fRight)
	c.value(right)
	c.throws++
	rn := c.node(flow.Stmt, right, c.base, len(c.reads))
	iter := c.b.Var()
	c.b.Def(rn, iter)
	f := c.b.OpenLoop()
	c.reset()
	c.throws++
	decl := n.ChildByFieldId(k.fDeclarator)
	h := c.nodeAt(flow.Branch, flow.Span{Start: uint32(decl.StartByte()), End: uint32(right.EndByte())}, c.base, c.base)
	c.b.Use(h, iter)
	exit := c.b.Push()
	c.reset()
	if sb := c.binding(decl); sb != nil {
		start, names := c.kids(sb)
		for i := range names {
			v := c.declare(&names[i])
			id := c.node(flow.Stmt, &names[i], c.base, c.base)
			c.def(id, v)
			c.b.Use(id, iter)
		}
		c.done(start)
	} else if name := c.variable(decl); name != nil {
		v := c.declare(name)
		id := c.node(flow.Stmt, name, c.base, c.base)
		c.def(id, v)
		c.b.Use(id, iter)
	}
	c.sub(n.ChildByFieldId(k.fBody))
	c.b.ContinueHere(f)
	c.loopEnd(f, h, true, exit)
	c.close(s)
}

// caseName is the goto label of a nested case or default label.
func (c *cLower) caseName(n *ts.Node) string {
	return cCasePrefix + strconv.FormatUint(uint64(n.StartByte()), 10)
}

// caseBody lowers the statements a case or default label heads.
func (c *cLower) caseBody(n *ts.Node) {
	start, list := c.body(n, c.k.fValue, 0, 0)
	for i := range list {
		c.stmt(&list[i])
	}
	c.done(start)
}

// switchStmt lowers a switch (see Node granularity).
func (c *cLower) switchStmt(n *ts.Node) {
	k := c.k
	s := c.open()
	c.reset()
	cond := n.ChildByFieldId(k.fCondition)
	m := len(c.reads)
	if cond.KindId() == k.conditionClause {
		if init := cond.ChildByFieldId(k.fInitializer); init != nil {
			c.initStmt(init)
			c.reset()
		}
		cond = cond.ChildByFieldId(k.fValue)
	}
	if cond.KindId() == k.declaration {
		c.condDecl(cond, flow.Stmt)
		c.reads = c.reads[:m]
		if id := c.variable(cond.ChildByFieldId(k.fDeclarator)); id != nil {
			c.ref(id)
		}
	} else {
		e := c.l.unparen(cond)
		c.value(e)
		c.node(flow.Stmt, e, m, len(c.reads))
	}
	tag := [2]int{m, len(c.reads)}
	f := c.b.OpenSwitch()
	base := len(c.hs)
	start, list := c.kids(n.ChildByFieldId(k.fBody))
	top := false
	var nested *ts.Node
	for i := range list {
		cs := &list[i]
		if cs.KindId() != k.caseStatement {
			nested = c.nestedCases(cs, tag, nested)
			continue
		}
		if v := cs.ChildByFieldId(k.fValue); v != nil {
			c.node(flow.Branch, v, tag[0], tag[1])
			c.hs = append(c.hs, c.b.Push())
		} else {
			top = true
		}
		s2, body := c.body(cs, k.fValue, 0, 0)
		for j := range body {
			nested = c.nestedCases(&body[j], tag, nested)
		}
		c.done(s2)
	}
	noMatch := c.b.Push()
	first := noMatch
	if len(c.hs) > base {
		first = c.hs[base]
	}
	// The no-match path goes to the default label, or leaves the switch; the
	// fringe is then empty for the statements before the first case label.
	var skip flow.Frame
	switch {
	case nested != nil:
		c.b.Goto(c.caseName(nested))
	case top:
		skip = c.b.OpenBlock(cSkipLabel)
		c.b.Break(cSkipLabel)
	default:
		c.b.Break("")
	}
	started, next := false, base
	for i := range list {
		cs := &list[i]
		if cs.KindId() != k.caseStatement {
			c.stmt(cs)
			continue
		}
		h := noMatch
		if cs.ChildByFieldId(k.fValue) != nil {
			h = c.hs[next]
			next++
		}
		if !started && top {
			pre := c.b.Push()
			c.b.CloseFrame(skip)
			c.b.Restore(h)
			c.b.Merge(pre)
			c.b.Pop(pre)
		} else {
			c.b.Merge(h)
		}
		started = true
		c.caseBody(cs)
	}
	c.done(start)
	c.b.CloseFrame(f)
	c.b.Pop(first)
	c.hs = c.hs[:base]
	c.close(s)
}

// nestedCases emits, for every case label nested below statement n (not in
// a nested switch or callable), its Branch node and a jump to its label
// node, in source order, and returns the nested default label (or dflt).
func (c *cLower) nestedCases(n *ts.Node, tag [2]int, dflt *ts.Node) *ts.Node {
	k := c.k
	if n.KindId() == k.switchStatement || c.l.isCallable(n) {
		return dflt
	}
	if n.KindId() == k.caseStatement {
		if v := n.ChildByFieldId(k.fValue); v != nil {
			c.node(flow.Branch, v, tag[0], tag[1])
			p := c.b.Push()
			c.b.Goto(c.caseName(n))
			c.b.Restore(p)
			c.b.Pop(p)
		} else if dflt == nil {
			dflt = n
		}
	}
	start, list := c.kids(n)
	for i := range list {
		dflt = c.nestedCases(&list[i], tag, dflt)
	}
	c.done(start)
	return dflt
}

// preproc lowers a preprocessor conditional inside a body: every arm is a
// path (see Node granularity).
func (c *cLower) preproc(n *ts.Node) {
	saved := c.pp
	if c.pp < c.blockMark {
		c.pp = c.binds.mark()
	}
	c.arms(n)
	c.pp = saved
}

func (c *cLower) arms(n *ts.Node) {
	k := c.k
	var at flow.Span
	last := false
	switch n.KindId() {
	case k.preprocIf, k.preprocElif:
		at = spanOf(n.ChildByFieldId(k.fCondition))
	case k.preprocIfdef, k.preprocElifdef:
		at = spanOf(n.ChildByFieldId(k.fName))
	default:
		// `#else`: its arm is the only path on; nothing skips it.
		at, last = spanOf(n.Child(0)), true
	}
	c.reset()
	c.nodeAt(flow.Branch, at, c.base, c.base)
	p := c.b.Push()
	start, list := c.body(n, k.fCondition, k.fName, k.fAlternative)
	for i := range list {
		c.stmt(&list[i])
	}
	c.done(start)
	if alt := n.ChildByFieldId(k.fAlternative); alt != nil {
		t := c.b.Push()
		c.b.Restore(p)
		c.arms(alt)
		c.b.Merge(t)
	} else if !last {
		c.b.Merge(p)
	}
	c.b.Pop(p)
}

// tryStmt lowers a C++ try statement or function-try-block; rethrow makes
// the end of every handler rethrow (a constructor's or destructor's
// function-try-block).
func (c *cLower) tryStmt(n *ts.Node, rethrow bool) {
	k := c.k
	cf := c.b.OpenCatch()
	start, list := c.kids(n)
	for i := range list {
		if list[i].KindId() == k.fieldInitializerList {
			c.initializers(&list[i])
		}
	}
	c.block(n.ChildByFieldId(k.fBody))
	t := c.b.Push()
	entered, caught := false, false
	var bf flow.Frame
	for i := range list {
		cl := &list[i]
		if cl.KindId() != k.catchClause {
			continue
		}
		if !entered {
			c.b.EnterHandler(cf, spanOf(cl.Child(0)))
			bf = c.b.OpenBlock(cCatchLabel)
			entered = true
		}
		s := c.open()
		ps := cl.ChildByFieldId(k.fParameters)
		all := c.catchAll(ps)
		var p flow.Fringe
		if !all {
			c.reset()
			v := int32(-1)
			if first := firstNamed(ps); first != nil {
				if id := c.variable(first.ChildByFieldId(k.fDeclarator)); id != nil {
					v = c.declare(id)
				}
			}
			c.def(c.node(flow.Branch, ps, c.base, c.base), v)
			p = c.b.Push()
		}
		c.block(cl.ChildByFieldId(k.fBody))
		if rethrow {
			c.b.Throw()
		} else {
			c.b.Break(cCatchLabel)
		}
		c.close(s)
		if all {
			caught = true
			break
		}
		c.b.Restore(p)
		c.b.Pop(p)
	}
	c.done(start)
	if !entered {
		c.b.EnterHandler(cf, spanOf(n.Child(0)))
		bf = c.b.OpenBlock(cCatchLabel)
	}
	if !caught {
		c.b.Throw()
	}
	c.b.CloseFrame(bf)
	c.b.Merge(t)
	c.b.Pop(t)
}

// catchAll reports whether a handler's parameter list is `...`.
func (c *cLower) catchAll(ps *ts.Node) bool {
	start, list := c.tokens(ps)
	defer c.done(start)
	for i := range list {
		if !list[i].IsNamed() && list[i].KindId() == c.k.ellipsis {
			return true
		}
	}
	return false
}

// sehTry lowers `__try` with its `__finally` or `__except` clause.
func (c *cLower) sehTry(n *ts.Node) {
	k := c.k
	start, list := c.kids(n)
	var fin, exc *ts.Node
	for i := range list {
		switch list[i].KindId() {
		case k.sehFinallyClause:
			fin = &list[i]
		case k.sehExceptClause:
			exc = &list[i]
		}
	}
	var ff, cf flow.Frame
	switch {
	case fin != nil:
		ff = c.b.OpenFinally()
	case exc != nil:
		cf = c.b.OpenCatch()
	}
	lf := c.b.OpenBlock(cLeaveLabel)
	c.seh++
	c.block(n.ChildByFieldId(k.fBody))
	c.seh--
	c.b.CloseFrame(lf)
	switch {
	case fin != nil:
		normal := c.b.EnterFinally(ff, spanOf(fin.Child(0)))
		c.block(fin.ChildByFieldId(k.fBody))
		c.b.CloseFinally(ff, normal)
	case exc != nil:
		t := c.b.Push()
		c.b.EnterHandler(cf, spanOf(exc.Child(0)))
		c.reset()
		filter := c.l.unparen(exc.ChildByFieldId(k.fFilter))
		c.value(filter)
		c.node(flow.Branch, filter, c.base, len(c.reads))
		p := c.b.Push()
		c.block(exc.ChildByFieldId(k.fBody))
		e := c.b.Push()
		c.b.Restore(p)
		c.b.Throw()
		c.b.Merge(e)
		c.b.Merge(t)
		c.b.Pop(p)
		c.b.Pop(t)
	}
	c.done(start)
}

// value lowers an expression evaluated for its value: reads are recorded,
// throwing constructs counted, and nodes created for every decision, every
// conditionally evaluated operand, every definition of an identifier and
// every nested callable.
func (c *cLower) value(n *ts.Node) {
	k := c.k
	if c.l.isCallable(n) {
		c.closure(n)
		return
	}
	switch n.KindId() {
	case k.identifier:
		c.ref(n)
	case k.assignmentExpression:
		_, v := c.assign(n)
		c.read(v)
	case k.updateExpression:
		_, v := c.update(n)
		c.read(v)
	case k.binaryExpression:
		left, right := n.ChildByFieldId(k.fLeft), n.ChildByFieldId(k.fRight)
		if op := n.ChildByFieldId(k.fOperator).KindId(); op == k.and || op == k.or {
			m := len(c.reads)
			c.value(left)
			c.node(flow.Branch, left, m, len(c.reads))
			p := c.b.Push()
			c.valueNode(right)
			c.b.Merge(p)
			c.b.Pop(p)
			return
		}
		c.value(left)
		c.value(right)
	case k.conditionalExpression:
		cond := n.ChildByFieldId(k.fCondition)
		m := len(c.reads)
		c.value(cond)
		c.node(flow.Branch, cond, m, len(c.reads))
		p := c.b.Push()
		if cons := n.ChildByFieldId(k.fConsequence); cons != nil {
			c.valueNode(cons)
		}
		t := c.b.Push()
		c.b.Restore(p)
		c.valueNode(n.ChildByFieldId(k.fAlternative))
		c.b.Merge(t)
		c.b.Pop(p)
	case k.callExpression, k.newExpression:
		c.children(n)
		c.throws++
	case k.pointerExpression:
		switch n.ChildByFieldId(k.fOperator).KindId() {
		case k.amp:
			c.target(n.ChildByFieldId(k.fArgument))
		default:
			c.value(n.ChildByFieldId(k.fArgument))
			if c.seh > 0 {
				c.throws++
			}
		}
	case k.fieldExpression:
		c.value(n.ChildByFieldId(k.fArgument))
		if c.seh > 0 && n.ChildByFieldId(k.fOperator).KindId() == k.arrow {
			c.throws++
		}
	case k.subscriptExpression:
		c.children(n)
		if c.seh > 0 {
			c.throws++
		}
	case k.initializerPair:
		c.value(n.ChildByFieldId(k.fValue))
	case k.gnuAsmOutputOperand:
		c.target(n.ChildByFieldId(k.fValue))
	case k.compoundStatement:
		c.stmtExpr(n)
	case k.sizeofExpression, k.alignofExpression, k.offsetofExpression, k.gnuAsmGotoList, k.qualifiedIdentifier,
		k.fieldIdentifier, k.this:
	default:
		c.children(n)
	}
}

// children lowers n's named children for their values.
func (c *cLower) children(n *ts.Node) {
	start, list := c.kids(n)
	for i := range list {
		c.value(&list[i])
	}
	c.done(start)
}

// stmtExpr lowers a statement expression's block in place; the enclosing
// expression's node reads its last statement's reads.
func (c *cLower) stmtExpr(n *ts.Node) {
	base, throws, thrown := c.base, c.throws, c.thrown
	c.base = len(c.reads)
	c.block(n)
	c.base, c.throws, c.thrown = base, throws, thrown
}

// assign lowers `left = right` and `left op= right`. made reports that it
// created the node spanning n; v is the variable an identifier target
// defines, else -1.
func (c *cLower) assign(n *ts.Node) (made bool, v int32) {
	k := c.k
	left, right := c.l.unparen(n.ChildByFieldId(k.fLeft)), n.ChildByFieldId(k.fRight)
	m := len(c.reads)
	if left.KindId() != k.identifier {
		c.value(right)
		c.target(left)
		return false, -1
	}
	v = c.lookup(left)
	if n.ChildByFieldId(k.fOperator).KindId() != k.assign {
		c.read(v)
	}
	c.value(right)
	c.def(c.node(flow.Stmt, n, m, len(c.reads)), v)
	return true, v
}

// update lowers `x++`, `--x` and their indirect forms; made and v are as
// for assign.
func (c *cLower) update(n *ts.Node) (made bool, v int32) {
	arg := c.l.unparen(n.ChildByFieldId(c.k.fArgument))
	if arg.KindId() != c.k.identifier {
		c.target(arg)
		return false, -1
	}
	m := len(c.reads)
	v = c.lookup(arg)
	c.read(v)
	c.def(c.node(flow.Stmt, n, m, len(c.reads)), v)
	return true, v
}

// target lowers a write through a field, index or pointer target, or the
// operand of an address-taking: its operands are read and its base variable
// may-defined by the node that consumes it.
func (c *cLower) target(t *ts.Node) {
	c.value(t)
	if id := c.baseIdent(t); id != nil {
		if v := c.lookup(id); v >= 0 {
			c.may = append(c.may, v)
		}
	}
}

// baseIdent is the identifier a field, index or indirect target writes
// through (x in `x.f`, `x->f`, `x[i]`, `*x`, `*(T *)x`), or nil.
func (c *cLower) baseIdent(n *ts.Node) *ts.Node {
	k := c.k
	for n = c.l.unparen(n); n != nil; n = c.l.unparen(n) {
		switch n.KindId() {
		case k.identifier:
			return n
		case k.fieldExpression, k.subscriptExpression:
			n = n.ChildByFieldId(k.fArgument)
		case k.pointerExpression:
			if n.ChildByFieldId(k.fOperator).KindId() != k.star {
				return nil
			}
			n = n.ChildByFieldId(k.fArgument)
		case k.castExpression:
			n = n.ChildByFieldId(k.fValue)
		default:
			return nil
		}
	}
	return nil
}

// closure creates the node spanning n, a lambda or nested function: it Uses
// n's captures and may-defines every variable n writes by reference.
func (c *cLower) closure(n *ts.Node) int32 {
	m, w := len(c.reads), len(c.writes)
	c.capture(n)
	id := c.node(flow.Stmt, n, m, len(c.reads))
	for _, v := range c.writes[w:] {
		c.b.MayDef(id, v)
	}
	c.writes = c.writes[:w]
	return id
}

// capture collects a nested callable's captures (see Captures): its reads
// into reads, the enclosing variables it writes by reference into writes.
func (c *cLower) capture(n *ts.Node) {
	k := c.k
	w := len(c.writes)
	lambda := n.KindId() == k.lambdaExpression
	var cs *ts.Node
	byRef := false
	if lambda {
		if cs = n.ChildByFieldId(k.fCaptures); cs != nil {
			byRef = c.captureList(cs)
		}
	}
	c.shadow++
	s := c.open()
	if lambda {
		if cs != nil {
			start, list := c.kids(cs)
			for i := range list {
				if list[i].KindId() == k.lambdaCaptureInitializer {
					c.hide(list[i].ChildByFieldId(k.fLeft))
				}
			}
			c.done(start)
		}
		if d := n.ChildByFieldId(k.fDeclarator); d != nil {
			c.capParams(d.ChildByFieldId(k.fParameters))
		}
	} else {
		c.capParams(c.parameters(n.ChildByFieldId(k.fDeclarator)))
	}
	if body := n.ChildByFieldId(k.fBody); body != nil {
		c.cap(body)
	}
	c.close(s)
	c.shadow--
	if !lambda {
		return
	}
	kept := w
	for _, v := range c.writes[w:] {
		if c.byReference(cs, v, byRef) {
			c.writes[kept] = v
			kept++
		}
	}
	c.writes = c.writes[:kept]
}

// captureList records the reads of a lambda's explicit captures and
// init-captures and reports whether its default capture is `&`.
func (c *cLower) captureList(cs *ts.Node) bool {
	k := c.k
	byRef := false
	start, list := c.kids(cs)
	for i := range list {
		switch x := &list[i]; x.KindId() {
		case k.lambdaDefaultCapture:
			t := c.text(x)
			byRef = len(t) > 0 && t[0] == '&'
		case k.identifier:
			c.ref(x)
		case k.lambdaCaptureInitializer:
			c.cap(x.ChildByFieldId(k.fRight))
		}
	}
	c.done(start)
	return byRef
}

// byReference reports whether the lambda with capture list cs captures v by
// reference: named `&x`, or not named at all under a `&` default.
func (c *cLower) byReference(cs *ts.Node, v int32, byRef bool) bool {
	if cs == nil {
		return false
	}
	k := c.k
	start, list := c.tokens(cs)
	defer c.done(start)
	amp := false
	for i := range list {
		x := &list[i]
		switch {
		case !x.IsNamed():
			amp = x.KindId() == k.amp
			continue
		case x.KindId() == k.identifier && c.lookup(x) == v:
			return amp
		}
		amp = false
	}
	return byRef
}

// capParams hides every parameter name of ps inside a nested callable.
func (c *cLower) capParams(ps *ts.Node) {
	if ps == nil {
		return
	}
	k := c.k
	start, list := c.kids(ps)
	for i := range list {
		p := &list[i]
		if p.KindId() == k.identifier {
			c.hide(p)
		} else if id, _ := c.name(p.ChildByFieldId(k.fDeclarator)); id != nil {
			c.hide(id)
		}
	}
	c.done(start)
}

// cap collects the references n makes to variables of the function being
// lowered, honouring every scope n opens.
func (c *cLower) cap(n *ts.Node) {
	k := c.k
	if c.l.isCallable(n) {
		c.capture(n)
		return
	}
	switch n.KindId() {
	case k.identifier:
		c.ref(n)
	case k.assignmentExpression:
		c.capWrite(n.ChildByFieldId(k.fLeft))
		c.capKids(n)
	case k.updateExpression:
		c.capWrite(n.ChildByFieldId(k.fArgument))
		c.capKids(n)
	case k.gnuAsmOutputOperand:
		c.capWrite(n.ChildByFieldId(k.fValue))
		c.capKids(n)
	case k.pointerExpression:
		if n.ChildByFieldId(k.fOperator).KindId() == k.amp {
			c.capWrite(n.ChildByFieldId(k.fArgument))
		}
		c.capKids(n)
	case k.compoundStatement, k.ifStatement, k.whileStatement, k.forStatement, k.switchStatement:
		s := c.open()
		c.capKids(n)
		c.close(s)
	case k.forRangeLoop:
		s := c.open()
		if init := n.ChildByFieldId(k.fInitializer); init != nil {
			c.cap(init)
		}
		c.cap(n.ChildByFieldId(k.fRight))
		c.capNames(n.ChildByFieldId(k.fDeclarator))
		c.cap(n.ChildByFieldId(k.fBody))
		c.close(s)
	case k.catchClause:
		s := c.open()
		if first := firstNamed(n.ChildByFieldId(k.fParameters)); first != nil {
			c.capNames(first.ChildByFieldId(k.fDeclarator))
		}
		c.cap(n.ChildByFieldId(k.fBody))
		c.close(s)
	case k.declaration:
		cur := c.cur
		start := len(c.buf)
		cur.Reset(*n)
		if cur.GotoFirstChild() {
			for {
				if cur.FieldId() == k.fDeclarator || cur.FieldId() == k.fValue {
					c.buf = append(c.buf, *cur.Node())
				}
				if !cur.GotoNextSibling() {
					break
				}
			}
		}
		list := c.buf[start:]
		for i := range list {
			d := &list[i]
			switch {
			case d.KindId() == k.initDeclarator:
				c.capNames(d.ChildByFieldId(k.fDeclarator))
				c.cap(d.ChildByFieldId(k.fValue))
			default:
				c.capNames(d)
			}
		}
		c.done(start)
	case k.sizeofExpression, k.alignofExpression, k.offsetofExpression, k.gnuAsmGotoList, k.qualifiedIdentifier,
		k.fieldIdentifier, k.this:
	case k.fieldExpression:
		c.cap(n.ChildByFieldId(k.fArgument))
	case k.initializerPair:
		c.cap(n.ChildByFieldId(k.fValue))
	default:
		c.capKids(n)
	}
}

func (c *cLower) capKids(n *ts.Node) {
	start, list := c.kids(n)
	for i := range list {
		c.cap(&list[i])
	}
	c.done(start)
}

// capNames hides every name declarator d declares, reading its array sizes.
func (c *cLower) capNames(d *ts.Node) {
	if d == nil {
		return
	}
	if sb := c.binding(d); sb != nil {
		start, list := c.kids(sb)
		for i := range list {
			c.hide(&list[i])
		}
		c.done(start)
		return
	}
	c.capSizes(d)
	if id, _ := c.name(d); id != nil {
		c.hide(id)
	}
}

// capSizes collects the captures of the array sizes along declarator d.
func (c *cLower) capSizes(d *ts.Node) {
	k := c.k
	for d != nil {
		switch d.KindId() {
		case k.arrayDeclarator:
			if s := d.ChildByFieldId(k.fSize); s != nil {
				c.cap(s)
			}
			d = d.ChildByFieldId(k.fDeclarator)
		case k.pointerDeclarator:
			d = d.ChildByFieldId(k.fDeclarator)
		case k.parenthesizedDeclarator, k.attributedDeclarator, k.referenceDeclarator:
			d = c.inner(d)
		default:
			return
		}
	}
}

// capWrite records, as a write, the enclosing variable an assignment target
// inside a nested callable names or writes through.
func (c *cLower) capWrite(t *ts.Node) {
	if id := c.baseIdent(t); id != nil {
		if v := c.lookup(id); v >= 0 {
			c.writes = append(c.writes, v)
		}
	}
}

// cSyntax holds the kind and field ids the C-family lowering matches,
// resolved once per grammar by name so the walk compares integers. A C++
// kind is 0 in the C table, an id no node has.
type cSyntax struct {
	cpp bool
	// noNode is indexed by kind id: the statement kinds that make no node.
	noNode []bool

	compoundStatement, expressionStatement, declaration, initDeclarator, ifStatement, whileStatement,
	doStatement, forStatement, switchStatement, caseStatement, labeledStatement, attributedStatement,
	gotoStatement, breakStatement, continueStatement, returnStatement, sehTryStatement, sehExceptClause,
	sehFinallyClause, sehLeaveStatement, preprocIf, preprocIfdef, preprocElif, preprocElifdef,
	attributeDeclaration, msCallModifier, identifier, fieldIdentifier, functionDeclarator, pointerDeclarator,
	arrayDeclarator, parenthesizedDeclarator, attributedDeclarator, commaExpression, assignmentExpression,
	updateExpression, binaryExpression, conditionalExpression, callExpression, pointerExpression,
	fieldExpression, subscriptExpression, castExpression, sizeofExpression, alignofExpression,
	offsetofExpression, initializerList, initializerPair, gnuAsmExpression, gnuAsmOutputOperand,
	gnuAsmGotoList, trueLit, numberLiteral uint16

	// C++ only.
	lambdaExpression, lambdaCaptureInitializer, lambdaDefaultCapture, tryStatement, catchClause,
	throwStatement, coReturnStatement, forRangeLoop, conditionClause, fieldInitializerList, newExpression,
	referenceDeclarator, variadicDeclarator, structuredBindingDeclarator, qualifiedIdentifier, operatorCast,
	argumentList, this uint16

	and, or, assign, star, arrow, amp, ellipsis uint16

	fAlternative, fArgument, fBody, fCondition, fConsequence, fDeclarator, fFilter, fGotoLabels,
	fInitializer, fLabel, fLeft, fName, fOperator, fParameters, fRight, fSize, fType, fUpdate, fValue uint16

	// C++ only.
	fCaptures uint16
}

var (
	cSyntaxOnce, cppSyntaxOnce   sync.Once
	cSyntaxTable, cppSyntaxTable *cSyntax
)

// cSyntaxOf resolves language's table once. A name the grammar does not
// define is a lowering defect and panics, so a misspelt kind can never
// silently match nothing.
func cSyntaxOf(language string) *cSyntax {
	if language == "cpp" {
		cppSyntaxOnce.Do(func() { cppSyntaxTable = resolveCSyntax(language) })
		return cppSyntaxTable
	}
	cSyntaxOnce.Do(func() { cSyntaxTable = resolveCSyntax(language) })
	return cSyntaxTable
}

func resolveCSyntax(language string) *cSyntax {
	tl := mustGrammar(language)
	kind := func(name string) uint16 { return mustKind(tl, language, name, true) }
	tok := func(name string) uint16 { return mustKind(tl, language, name, false) }
	field := func(name string) uint16 { return mustField(tl, language, name) }
	s := &cSyntax{cpp: language == "cpp", noNode: make([]bool, tl.NodeKindCount())}
	noNode := []string{"type_definition", "struct_specifier", "union_specifier", "enum_specifier",
		"linkage_specification", "preproc_call", "preproc_def", "preproc_function_def", "preproc_include"}
	s.compoundStatement, s.expressionStatement, s.declaration = kind("compound_statement"), kind("expression_statement"), kind("declaration")
	s.initDeclarator, s.ifStatement, s.whileStatement = kind("init_declarator"), kind("if_statement"), kind("while_statement")
	s.doStatement, s.forStatement, s.switchStatement = kind("do_statement"), kind("for_statement"), kind("switch_statement")
	s.caseStatement, s.labeledStatement, s.attributedStatement = kind("case_statement"), kind("labeled_statement"), kind("attributed_statement")
	s.gotoStatement, s.breakStatement, s.continueStatement = kind("goto_statement"), kind("break_statement"), kind("continue_statement")
	s.returnStatement, s.sehTryStatement, s.sehExceptClause = kind("return_statement"), kind("seh_try_statement"), kind("seh_except_clause")
	s.sehFinallyClause, s.sehLeaveStatement = kind("seh_finally_clause"), kind("seh_leave_statement")
	s.preprocIf, s.preprocIfdef, s.preprocElif, s.preprocElifdef = kind("preproc_if"), kind("preproc_ifdef"), kind("preproc_elif"), kind("preproc_elifdef")
	s.attributeDeclaration, s.msCallModifier = kind("attribute_declaration"), kind("ms_call_modifier")
	s.identifier, s.fieldIdentifier, s.functionDeclarator = kind("identifier"), kind("field_identifier"), kind("function_declarator")
	s.pointerDeclarator, s.arrayDeclarator = kind("pointer_declarator"), kind("array_declarator")
	s.parenthesizedDeclarator, s.attributedDeclarator = kind("parenthesized_declarator"), kind("attributed_declarator")
	s.commaExpression, s.assignmentExpression, s.updateExpression = kind("comma_expression"), kind("assignment_expression"), kind("update_expression")
	s.binaryExpression, s.conditionalExpression, s.callExpression = kind("binary_expression"), kind("conditional_expression"), kind("call_expression")
	s.pointerExpression, s.fieldExpression, s.subscriptExpression = kind("pointer_expression"), kind("field_expression"), kind("subscript_expression")
	s.castExpression, s.sizeofExpression, s.alignofExpression = kind("cast_expression"), kind("sizeof_expression"), kind("alignof_expression")
	s.offsetofExpression, s.initializerList, s.initializerPair = kind("offsetof_expression"), kind("initializer_list"), kind("initializer_pair")
	s.gnuAsmExpression, s.gnuAsmOutputOperand, s.gnuAsmGotoList = kind("gnu_asm_expression"), kind("gnu_asm_output_operand"), kind("gnu_asm_goto_list")
	s.trueLit, s.numberLiteral = kind("true"), kind("number_literal")
	s.and, s.or, s.assign, s.star, s.arrow, s.amp = tok("&&"), tok("||"), tok("="), tok("*"), tok("->"), tok("&")
	s.fAlternative, s.fArgument, s.fBody, s.fCondition = field("alternative"), field("argument"), field("body"), field("condition")
	s.fConsequence, s.fDeclarator, s.fFilter, s.fGotoLabels = field("consequence"), field("declarator"), field("filter"), field("goto_labels")
	s.fInitializer, s.fLabel, s.fLeft, s.fName = field("initializer"), field("label"), field("left"), field("name")
	s.fOperator, s.fParameters, s.fRight, s.fSize = field("operator"), field("parameters"), field("right"), field("size")
	s.fType, s.fUpdate, s.fValue = field("type"), field("update"), field("value")
	if s.cpp {
		s.lambdaExpression, s.lambdaCaptureInitializer = kind("lambda_expression"), kind("lambda_capture_initializer")
		s.lambdaDefaultCapture, s.tryStatement, s.catchClause = kind("lambda_default_capture"), kind("try_statement"), kind("catch_clause")
		s.throwStatement, s.coReturnStatement, s.forRangeLoop = kind("throw_statement"), kind("co_return_statement"), kind("for_range_loop")
		s.conditionClause, s.fieldInitializerList, s.newExpression = kind("condition_clause"), kind("field_initializer_list"), kind("new_expression")
		s.referenceDeclarator, s.variadicDeclarator = kind("reference_declarator"), kind("variadic_declarator")
		s.structuredBindingDeclarator, s.qualifiedIdentifier = kind("structured_binding_declarator"), kind("qualified_identifier")
		s.operatorCast, s.argumentList, s.this = kind("operator_cast"), kind("argument_list"), kind("this")
		s.ellipsis = tok("...")
		s.fCaptures = field("captures")
		noNode = append(noNode, "class_specifier", "using_declaration", "alias_declaration", "namespace_definition",
			"namespace_alias_definition", "static_assert_declaration", "template_declaration", "template_instantiation",
			"concept_definition")
	}
	for _, name := range noNode {
		s.noNode[kind(name)] = true
	}
	return s
}
