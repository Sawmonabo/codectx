package worker

import (
	"bytes"
	"sync"

	ts "github.com/tree-sitter/go-tree-sitter"

	"github.com/Sawmonabo/codectx/internal/provider/treesitter/flow"
)

// javascriptLowering lowers JavaScript callables: function declarations and
// expressions, arrow functions, methods, generator functions (async is a
// modifier of these kinds), class static blocks, and the program itself,
// whose top-level code is one function as the hosted engine's per-file
// program method is.
var javascriptLowering = Lowering{
	callable: set("program", "function_declaration", "function_expression", "arrow_function", "method_definition",
		"generator_function", "generator_function_declaration", "class_static_block"),
	lower: lowerJavaScript,
}

// lowerJavaScript lowers one JavaScript callable's parameters and body into b.
//
// # Node granularity
//
// The goldens render nodes by source text, so the granularity is exact:
//
//   - Every parameter bound name is one defining node after Entry, spanning
//     its identifier; in the program, every import binding is one, spanning
//     the local name.
//   - A statement is one node: an expression statement spans its expression
//     (a comma sequence at statement level is one statement per element), a
//     declarator spans the declarator, return, throw, break and continue span
//     the statement (kind Jump), a class declaration spans the declaration.
//     An expression statement that is an assignment or update of an
//     identifier is its defining node, not a second node. `var x;` is a
//     hoisted declaration and no node; `let x;` defines x.
//   - A function declaration is one defining node at the start of its block
//     (it is hoisted), spanning the declaration.
//   - A condition is one Branch node spanning the condition without its
//     parentheses: if, while, do…while, for. A switch is a Stmt node for the
//     discriminant then one Branch node per case test, in source order (each
//     test is evaluated only when the previous failed); a case body's end
//     flows into the next body when it does not break. A for…in/for…of loop
//     is a Stmt node for the iterated expression, evaluated once, then a
//     Branch head spanning the loop's left side.
//   - `&&`, `||`, `??` and the conditional operator: the deciding operand is
//     a Branch node spanning it, created after the nodes of everything it
//     evaluates; each conditionally evaluated operand is a Stmt node spanning
//     it. A logical assignment `x ??= y` is a Branch node spanning x, then a
//     Stmt node spanning the whole expression that conditionally defines x.
//     An optional chain is a Branch node spanning the receiver before each
//     `?.`, and one Stmt node spanning the whole chain on the non-nullish
//     path.
//   - Destructuring is one defining node per bound name, spanning the name,
//     so D ≤ N. A default (`a = e` in a pattern or parameter) is a Branch
//     node spanning the element that defines the name from the incoming
//     value, then a Stmt node spanning e that defines it from the default.
//   - A nested callable or class is its own function; in the enclosing
//     function its creating expression is one Stmt node spanning it, which
//     Uses every enclosing variable referenced inside it, resolved with its
//     own scopes so a name it declares shadows. Class field initializers are
//     part of that class node's captures, not lowered as control flow.
//   - A loop whose condition is the literal `true`, or a for loop without a
//     condition, has no exit edge: only a break leaves it. Its head is a Stmt
//     node spanning `true`, or the `for` keyword.
//
// # Uses
//
// Only an identifier resolving to a variable declared in this function is a
// Use. A node Uses every variable read inside its own span (the value it
// computes derives from them, since the lowering introduces no
// temporaries), plus every read evaluated since the previous node and not
// yet attached, so a read before an embedded assignment is attributed to the
// node that runs after it. A destructuring or for…in/for…of defining node
// also Uses the variables of the value it destructures. A property write
// (`o.p = v`, `a[i] = v`) Uses o, a, i and v and defines nothing.
//
// # Exceptions
//
// MayThrow is given to every node whose own evaluation — the part of the
// source evaluated since the previous node — contains a call, `new`,
// `await`, `yield`, a spread, a property read or write, a destructuring
// element, a for…of iterator step (including for await), or a class
// declaration's heritage; a throw statement's node is also a Throw. The
// Builder applies it only inside an open catch or finally frame.
//
// # Scoping
//
// `var` and a function's parameters are function-scoped and hoisted, as are
// import bindings in the program; `let`, `const`, `using`, `class` and a
// function declaration in a block are block-scoped and bound from the block's
// start (the strict-mode rule, which modules and classes impose); a catch
// parameter is scoped to its clause and a `for (let …)` binding to its loop;
// a switch body is one block. Parameter defaults see the parameters only.
func lowerJavaScript(l *Lowering, b *flow.Builder, fn *ts.Node, src []byte) {
	cur := fn.Walk()
	defer cur.Close()
	j := jsLower{b: b, src: src, k: jsSyntaxOf(l), cur: cur, first: -1}
	k := j.k
	switch fn.KindId() {
	case k.program:
		j.hoistVars(fn)
		start, list := j.kids(fn)
		for i := range list {
			if list[i].KindId() == k.importStatement {
				j.imports(&list[i])
			}
		}
		j.done(start)
		j.block(fn)
	case k.classStaticBlock:
		body := fn.ChildByFieldId(k.fBody)
		j.hoistVars(body)
		j.block(body)
	default:
		if ps := fn.ChildByFieldId(k.fParameters); ps != nil {
			start, list := j.kids(ps)
			for i := range list {
				j.declarePattern(&list[i], false)
			}
			for i := range list {
				j.reset()
				j.bind(&list[i], 0, 0)
			}
			j.done(start)
		} else if p := fn.ChildByFieldId(k.fParameter); p != nil {
			v := j.declare(p)
			j.def(j.node(flow.Stmt, p, 0, 0), v)
		}
		body := fn.ChildByFieldId(k.fBody)
		if body.KindId() == k.statementBlock {
			j.hoistVars(body)
			j.block(body)
			return
		}
		j.reset()
		j.value(body, false)
		j.node(flow.Stmt, body, 0, len(j.reads))
	}
}

// jsBind is one name in scope: src[start:end] names variable v, or v is -1
// for a name declared inside a nested callable, which shadows without being
// a variable of the function being lowered.
type jsBind struct {
	start, end uint32
	v          int32
}

// jsLower is the state of lowering one JavaScript callable.
type jsLower struct {
	b   *flow.Builder
	src []byte
	k   *jsSyntax
	cur *ts.TreeCursor
	// buf is a stack of child lists; kids pushes one and done pops it.
	buf []ts.Node
	// binds is the scope chain, innermost last; fnMark is where the
	// innermost function scope begins, the target of var hoisting.
	binds  []jsBind
	fnMark int
	// shadow is non-zero while walking a nested callable or class for its
	// captures: declarations then bind -1 and no node is created.
	shadow int
	// reads are the variables read by the current statement, in evaluation
	// order; reads[flushed:] are not yet attached to a node.
	reads   []int32
	flushed int
	// throws counts throwing constructs evaluated by the current statement;
	// those past thrown are not yet attached to a node.
	throws, thrown int
	// first is the first node created since the last open, or -1.
	first int32
	// opt holds the fringes saved at each `?.` of the optional chains being
	// lowered, innermost chain last.
	opt []flow.Fringe
	// labels are the statement labels the next statement takes.
	labels []string
}

// kids pushes n's named, non-extra children (comments are extras) onto buf
// and returns the stack mark and the list; done(mark) pops them. A list stays
// valid across nested kids calls: later pushes never overwrite it.
func (j *jsLower) kids(n *ts.Node) (int, []ts.Node) {
	start := len(j.buf)
	c := j.cur
	c.Reset(*n)
	if c.GotoFirstChild() {
		for {
			if x := c.Node(); x.IsNamed() && !x.IsExtra() {
				j.buf = append(j.buf, *x)
			}
			if !c.GotoNextSibling() {
				break
			}
		}
	}
	return start, j.buf[start:]
}

func (j *jsLower) done(mark int) { j.buf = j.buf[:mark] }

// firstKid is n's first named, non-extra child, or nil.
func (j *jsLower) firstKid(n *ts.Node) *ts.Node {
	start, list := j.kids(n)
	defer j.done(start)
	if len(list) == 0 {
		return nil
	}
	x := list[0]
	return &x
}

// inner strips parentheses.
func (j *jsLower) inner(n *ts.Node) *ts.Node {
	for n != nil && n.KindId() == j.k.parenthesizedExpression {
		n = j.firstKid(n)
	}
	return n
}

// jsSpan is n's byte range.
func jsSpan(n *ts.Node) flow.Span {
	return flow.Span{Start: uint32(n.StartByte()), End: uint32(n.EndByte())}
}

func (j *jsLower) text(n *ts.Node) []byte { return j.src[n.StartByte():n.EndByte()] }

// declare binds name in the innermost scope: a new variable, or -1 inside a
// nested callable.
func (j *jsLower) declare(name *ts.Node) int32 {
	v := int32(-1)
	if j.shadow == 0 {
		v = j.b.Var(jsSpan(name))
	}
	j.binds = append(j.binds, jsBind{uint32(name.StartByte()), uint32(name.EndByte()), v})
	return v
}

// lookup resolves name through the scope chain: its variable, or -1 when it
// is declared inside a nested callable or not in this function at all.
func (j *jsLower) lookup(name *ts.Node) int32 {
	t := j.text(name)
	for i := len(j.binds) - 1; i >= 0; i-- {
		if bb := j.binds[i]; bytes.Equal(j.src[bb.start:bb.end], t) {
			return bb.v
		}
	}
	return -1
}

// boundSince reports whether name is bound in binds[from:].
func (j *jsLower) boundSince(from int, name *ts.Node) bool {
	t := j.text(name)
	for _, bb := range j.binds[from:] {
		if bytes.Equal(j.src[bb.start:bb.end], t) {
			return true
		}
	}
	return false
}

// ref records a read of name when it resolves to a variable of this function.
func (j *jsLower) ref(name *ts.Node) {
	if v := j.lookup(name); v >= 0 {
		j.reads = append(j.reads, v)
	}
}

// reset starts a statement: nothing is pending.
func (j *jsLower) reset() {
	j.reads, j.flushed, j.throws, j.thrown = j.reads[:0], 0, 0, 0
}

// node creates a node spanning n that Uses reads[from:to] and every pending
// read, and MayThrow when a throwing construct is pending.
func (j *jsLower) node(kind flow.Kind, n *ts.Node, from, to int) int32 {
	id := j.b.Node(kind, jsSpan(n))
	if j.first < 0 {
		j.first = id
	}
	if from <= j.flushed && to >= j.flushed {
		from, to = min(from, j.flushed), len(j.reads)
	} else {
		for _, v := range j.reads[j.flushed:] {
			j.b.Use(id, v)
		}
	}
	for _, v := range j.reads[from:to] {
		j.b.Use(id, v)
	}
	j.flushed = len(j.reads)
	if j.throws > j.thrown {
		j.b.MayThrow(id)
	}
	j.thrown = j.throws
	return id
}

func (j *jsLower) def(n, v int32) {
	if v >= 0 {
		j.b.Def(n, v)
	}
}

// open starts tracking the first node created; close returns it (-1 if none)
// and restores the enclosing tracking.
func (j *jsLower) open() int32 {
	s := j.first
	j.first = -1
	return s
}

func (j *jsLower) close(saved int32) int32 {
	h := j.first
	if saved >= 0 {
		j.first = saved
	}
	return h
}

// arm lowers a conditionally evaluated operand as a node spanning it.
func (j *jsLower) arm(n *ts.Node) {
	m := len(j.reads)
	j.value(n, false)
	j.node(flow.Stmt, n, m, len(j.reads))
}

// block lowers a statement list in its own lexical scope: its lexical names
// are bound from the start and its function declarations defined there.
func (j *jsLower) block(n *ts.Node) {
	mark := len(j.binds)
	start, list := j.kids(n)
	for i := range list {
		j.predeclare(&list[i])
	}
	for i := range list {
		j.hoistFunction(&list[i])
	}
	for i := range list {
		j.stmt(&list[i])
	}
	j.done(start)
	j.binds = j.binds[:mark]
}

// predeclare binds the block-scoped names statement n declares.
func (j *jsLower) predeclare(n *ts.Node) {
	k := j.k
	switch n.KindId() {
	case k.lexicalDeclaration, k.usingDeclaration:
		start, list := j.kids(n)
		for i := range list {
			j.declarePattern(list[i].ChildByFieldId(k.fName), false)
		}
		j.done(start)
	case k.classDeclaration, k.functionDeclaration, k.generatorFunctionDeclaration:
		j.declare(n.ChildByFieldId(k.fName))
	case k.exportStatement:
		if d := n.ChildByFieldId(k.fDeclaration); d != nil {
			j.predeclare(d)
		}
	}
}

// hoistFunction emits the defining node of a function declaration at the
// start of its block.
func (j *jsLower) hoistFunction(n *ts.Node) {
	k := j.k
	if n.KindId() == k.exportStatement {
		if n = n.ChildByFieldId(k.fDeclaration); n == nil {
			return
		}
	}
	if id := n.KindId(); id != k.functionDeclaration && id != k.generatorFunctionDeclaration {
		return
	}
	j.reset()
	j.capFunction(n)
	j.def(j.node(flow.Stmt, n, 0, len(j.reads)), j.lookup(n.ChildByFieldId(k.fName)))
}

// declarePattern binds every name a binding pattern declares. With merge (var
// hoisting) a name already bound in the function scope is the same variable.
func (j *jsLower) declarePattern(p *ts.Node, merge bool) {
	k := j.k
	switch p.KindId() {
	case k.identifier, k.shorthandPropertyIdentifierPattern:
		if !merge || !j.boundSince(j.fnMark, p) {
			j.declare(p)
		}
	case k.assignmentPattern, k.objectAssignmentPattern:
		j.declarePattern(p.ChildByFieldId(k.fLeft), merge)
	case k.pairPattern:
		j.declarePattern(p.ChildByFieldId(k.fValue), merge)
	case k.objectPattern, k.arrayPattern, k.restPattern:
		start, list := j.kids(p)
		for i := range list {
			j.declarePattern(&list[i], merge)
		}
		j.done(start)
	}
}

// hoistVars binds every `var` name under statement n into the function
// scope, without entering nested callables or classes.
func (j *jsLower) hoistVars(n *ts.Node) {
	k := j.k
	switch n.KindId() {
	case k.variableDeclaration:
		start, list := j.kids(n)
		for i := range list {
			j.declarePattern(list[i].ChildByFieldId(k.fName), true)
		}
		j.done(start)
	case k.forInStatement:
		if kw := n.ChildByFieldId(k.fKind); kw != nil && kw.KindId() == k.varKw {
			j.declarePattern(n.ChildByFieldId(k.fLeft), true)
		}
		j.hoistVars(n.ChildByFieldId(k.fBody))
	case k.program, k.statementBlock, k.ifStatement, k.elseClause, k.forStatement, k.whileStatement, k.doStatement,
		k.labeledStatement, k.withStatement, k.tryStatement, k.catchClause, k.finallyClause, k.switchStatement,
		k.switchBody, k.switchCase, k.switchDefault, k.exportStatement:
		start, list := j.kids(n)
		for i := range list {
			j.hoistVars(&list[i])
		}
		j.done(start)
	}
}

// imports binds and defines the local names of one import statement.
func (j *jsLower) imports(n *ts.Node) {
	k := j.k
	start, list := j.kids(n)
	for i := range list {
		c := &list[i]
		if c.KindId() != k.importClause {
			continue
		}
		s2, parts := j.kids(c)
		for p := range parts {
			switch part := &parts[p]; part.KindId() {
			case k.identifier:
				j.importName(part)
			case k.namespaceImport:
				if id := j.firstKid(part); id != nil {
					j.importName(id)
				}
			case k.namedImports:
				s3, specs := j.kids(part)
				for s := range specs {
					local := specs[s].ChildByFieldId(k.fAlias)
					if local == nil {
						local = specs[s].ChildByFieldId(k.fName)
					}
					if local.KindId() == k.identifier {
						j.importName(local)
					}
				}
				j.done(s3)
			}
		}
		j.done(s2)
	}
	j.done(start)
}

func (j *jsLower) importName(id *ts.Node) {
	v := j.declare(id)
	j.def(j.node(flow.Stmt, id, 0, 0), v)
}

// stmt lowers one statement.
func (j *jsLower) stmt(n *ts.Node) {
	k := j.k
	labels := j.labels
	j.labels = nil
	j.reset()
	switch n.KindId() {
	case k.expressionStatement:
		if e := j.firstKid(n); e != nil {
			j.exprStmt(e)
		}
	case k.lexicalDeclaration, k.usingDeclaration:
		j.declaration(n, false)
	case k.variableDeclaration:
		j.declaration(n, true)
	case k.functionDeclaration, k.generatorFunctionDeclaration, k.emptyStatement, k.debuggerStatement,
		k.importStatement, k.hashBangLine:
	case k.classDeclaration:
		j.capClass(n)
		j.def(j.node(flow.Stmt, n, 0, len(j.reads)), j.lookup(n.ChildByFieldId(k.fName)))
	case k.statementBlock:
		j.block(n)
	case k.ifStatement:
		j.ifStmt(n)
	case k.forStatement:
		j.forStmt(n, labels)
	case k.forInStatement:
		j.forIn(n, labels)
	case k.whileStatement:
		j.whileStmt(n, labels)
	case k.doStatement:
		j.doStmt(n, labels)
	case k.switchStatement:
		j.switchStmt(n, labels)
	case k.tryStatement:
		j.tryStmt(n)
	case k.labeledStatement:
		j.labeled(n, labels)
	case k.returnStatement:
		if e := j.firstKid(n); e != nil {
			j.value(e, false)
		}
		j.node(flow.Jump, n, 0, len(j.reads))
		j.b.Return()
	case k.throwStatement:
		if e := j.firstKid(n); e != nil {
			j.value(e, false)
		}
		j.node(flow.Jump, n, 0, len(j.reads))
		j.b.Throw()
	case k.breakStatement:
		j.node(flow.Jump, n, 0, 0)
		j.b.Break(j.label(n))
	case k.continueStatement:
		j.node(flow.Jump, n, 0, 0)
		j.b.Continue(j.label(n))
	case k.withStatement:
		obj := j.inner(n.ChildByFieldId(k.fObject))
		j.value(obj, false)
		j.node(flow.Stmt, obj, 0, len(j.reads))
		j.stmt(n.ChildByFieldId(k.fBody))
	case k.exportStatement:
		if d := n.ChildByFieldId(k.fDeclaration); d != nil {
			j.stmt(d)
		} else if v := n.ChildByFieldId(k.fValue); v != nil {
			j.value(v, false)
			j.node(flow.Stmt, v, 0, len(j.reads))
		}
	default:
		j.value(n, false)
		j.node(flow.Stmt, n, 0, len(j.reads))
	}
}

// label is a break or continue statement's label, or "".
func (j *jsLower) label(n *ts.Node) string {
	if l := n.ChildByFieldId(j.k.fLabel); l != nil {
		return string(j.text(l))
	}
	return ""
}

// exprStmt lowers an expression evaluated for its effect.
func (j *jsLower) exprStmt(e *ts.Node) {
	k := j.k
	if e.KindId() == k.sequenceExpression {
		start, list := j.kids(e)
		for i := range list {
			j.exprStmt(&list[i])
		}
		j.done(start)
		return
	}
	m := len(j.reads)
	saved := j.open()
	j.value(e, false)
	made := j.close(saved) >= 0
	switch e.KindId() {
	case k.assignmentExpression, k.augmentedAssignmentExpression, k.updateExpression:
		if made && j.flushed == len(j.reads) && j.thrown == j.throws {
			return
		}
	}
	j.node(flow.Stmt, e, m, len(j.reads))
}

// declaration lowers a var, let, const or using declaration.
func (j *jsLower) declaration(n *ts.Node, isVar bool) {
	k := j.k
	start, list := j.kids(n)
	for i := range list {
		d := &list[i]
		name, val := d.ChildByFieldId(k.fName), d.ChildByFieldId(k.fValue)
		m := len(j.reads)
		switch {
		case val == nil && isVar:
		case val == nil:
			j.def(j.node(flow.Stmt, d, m, m), j.lookup(name))
		case name.KindId() == k.identifier:
			j.value(val, false)
			j.def(j.node(flow.Stmt, d, m, len(j.reads)), j.lookup(name))
		default:
			j.value(val, false)
			j.bind(name, m, len(j.reads))
		}
	}
	j.done(start)
}

func (j *jsLower) ifStmt(n *ts.Node) {
	k := j.k
	cond := j.inner(n.ChildByFieldId(k.fCondition))
	j.value(cond, false)
	j.node(flow.Branch, cond, 0, len(j.reads))
	p := j.b.Push()
	j.stmt(n.ChildByFieldId(k.fConsequence))
	t := j.b.Push()
	j.b.Restore(p)
	if alt := n.ChildByFieldId(k.fAlternative); alt != nil {
		if s := j.firstKid(alt); s != nil {
			j.stmt(s)
		}
	}
	j.b.Merge(t)
	j.b.Pop(p)
}

// isTrue reports whether a loop condition is the literal true.
func (j *jsLower) isTrue(cond *ts.Node) bool { return j.inner(cond).KindId() == j.k.trueLit }

// head lowers a loop condition as the loop's decision node, or as a Stmt
// node without an exit edge when it is the literal true. It reports whether
// the loop exits through it.
func (j *jsLower) head(cond *ts.Node) bool {
	j.reset()
	if j.isTrue(cond) {
		j.node(flow.Stmt, cond, 0, 0)
		return false
	}
	j.value(cond, false)
	j.node(flow.Branch, cond, 0, len(j.reads))
	return true
}

// loopEnd closes a loop whose back edge targets h: the continue target is the
// current point, exits (the head's false edge when exits) and breaks leave.
func (j *jsLower) loopEnd(f flow.Frame, h int32, exits bool, exit flow.Fringe) {
	j.b.Close(h)
	if exits {
		j.b.Restore(exit)
	}
	j.b.CloseFrame(f)
	if exits {
		j.b.Pop(exit)
	}
}

func (j *jsLower) whileStmt(n *ts.Node, labels []string) {
	k := j.k
	f := j.b.OpenLoop(labels...)
	saved := j.open()
	exits := j.head(j.inner(n.ChildByFieldId(k.fCondition)))
	h := j.close(saved)
	var exit flow.Fringe
	if exits {
		exit = j.b.Push()
	}
	j.stmt(n.ChildByFieldId(k.fBody))
	j.b.ContinueHere(f)
	j.loopEnd(f, h, exits, exit)
}

func (j *jsLower) doStmt(n *ts.Node, labels []string) {
	k := j.k
	f := j.b.OpenLoop(labels...)
	saved := j.open()
	j.stmt(n.ChildByFieldId(k.fBody))
	j.b.ContinueHere(f)
	exits := j.head(j.inner(n.ChildByFieldId(k.fCondition)))
	h := j.close(saved)
	var exit flow.Fringe
	if exits {
		exit = j.b.Push()
	}
	j.loopEnd(f, h, exits, exit)
}

func (j *jsLower) forStmt(n *ts.Node, labels []string) {
	k := j.k
	mark := len(j.binds)
	switch init := n.ChildByFieldId(k.fInitializer); init.KindId() {
	case k.lexicalDeclaration:
		j.predeclare(init)
		j.stmt(init)
	case k.variableDeclaration:
		j.stmt(init)
	case k.emptyStatement:
	default:
		j.reset()
		j.exprStmt(init)
	}
	f := j.b.OpenLoop(labels...)
	saved := j.open()
	exits := false
	if cond := n.ChildByFieldId(k.fCondition); cond.KindId() == k.emptyStatement {
		j.reset()
		j.node(flow.Stmt, n.Child(0), 0, 0)
	} else {
		exits = j.head(cond)
	}
	h := j.close(saved)
	var exit flow.Fringe
	if exits {
		exit = j.b.Push()
	}
	j.stmt(n.ChildByFieldId(k.fBody))
	j.b.ContinueHere(f)
	if inc := n.ChildByFieldId(k.fIncrement); inc != nil {
		j.reset()
		j.exprStmt(inc)
	}
	j.loopEnd(f, h, exits, exit)
	j.binds = j.binds[:mark]
}

// forIn lowers for…in, for…of and for await…of.
func (j *jsLower) forIn(n *ts.Node, labels []string) {
	k := j.k
	mark := len(j.binds)
	left := j.inner(n.ChildByFieldId(k.fLeft))
	kw := n.ChildByFieldId(k.fKind)
	if kw != nil && kw.KindId() != k.varKw {
		j.declarePattern(left, false)
	}
	of := n.ChildByFieldId(k.fOperator).KindId() == k.ofKw
	if v := n.ChildByFieldId(k.fValue); v != nil {
		j.reset()
		j.value(v, false)
		d := j.node(flow.Stmt, v, 0, len(j.reads))
		if left.KindId() == k.identifier {
			j.def(d, j.lookup(left))
		}
	}
	j.reset()
	right := n.ChildByFieldId(k.fRight)
	j.value(right, false)
	if of {
		j.throws++
	}
	j.node(flow.Stmt, right, 0, len(j.reads))
	rEnd := len(j.reads)
	f := j.b.OpenLoop(labels...)
	if of {
		j.throws++
	}
	var h int32
	pattern := false
	switch left.KindId() {
	case k.identifier:
		h = j.node(flow.Branch, left, 0, rEnd)
		j.def(h, j.lookup(left))
	case k.memberExpression, k.subscriptExpression:
		j.target(left)
		h = j.node(flow.Branch, left, 0, rEnd)
	default:
		h = j.node(flow.Branch, left, 0, rEnd)
		pattern = true
	}
	exit := j.b.Push()
	if pattern {
		j.bind(left, 0, rEnd)
	}
	j.stmt(n.ChildByFieldId(k.fBody))
	j.b.ContinueHere(f)
	j.loopEnd(f, h, true, exit)
	j.binds = j.binds[:mark]
}

func (j *jsLower) switchStmt(n *ts.Node, labels []string) {
	k := j.k
	disc := j.inner(n.ChildByFieldId(k.fValue))
	j.value(disc, false)
	j.node(flow.Stmt, disc, 0, len(j.reads))
	mark := len(j.binds)
	start, cases := j.kids(n.ChildByFieldId(k.fBody))
	// A case's statements are its named children after the test value.
	body := func(c *ts.Node) (int, []ts.Node) {
		s, list := j.kids(c)
		if c.KindId() == k.switchCase && len(list) > 0 {
			list = list[1:]
		}
		return s, list
	}
	for c := range cases {
		s, list := body(&cases[c])
		for i := range list {
			j.predeclare(&list[i])
		}
		j.done(s)
	}
	for c := range cases {
		s, list := body(&cases[c])
		for i := range list {
			j.hoistFunction(&list[i])
		}
		j.done(s)
	}
	f := j.b.OpenSwitch(labels...)
	match := make([]flow.Fringe, len(cases))
	pushed := false
	var firstPush flow.Fringe
	for c := range cases {
		if cases[c].KindId() != k.switchCase {
			continue
		}
		j.reset()
		v := cases[c].ChildByFieldId(k.fValue)
		j.value(v, false)
		j.node(flow.Branch, v, 0, len(j.reads))
		match[c] = j.b.Push()
		if !pushed {
			firstPush, pushed = match[c], true
		}
	}
	noMatch := j.b.Push()
	if !pushed {
		firstPush = noMatch
	}
	hasDefault := false
	for c := range cases {
		h := match[c]
		if cases[c].KindId() != k.switchCase {
			h, hasDefault = noMatch, true
		}
		if c == 0 {
			j.b.Restore(h)
		} else {
			j.b.Merge(h)
		}
		s, list := body(&cases[c])
		for i := range list {
			j.stmt(&list[i])
		}
		j.done(s)
	}
	if !hasDefault {
		j.b.Merge(noMatch)
	}
	j.b.CloseFrame(f)
	j.b.Pop(firstPush)
	j.done(start)
	j.binds = j.binds[:mark]
}

func (j *jsLower) tryStmt(n *ts.Node) {
	k := j.k
	handler, fin := n.ChildByFieldId(k.fHandler), n.ChildByFieldId(k.fFinalizer)
	var ff, cf flow.Frame
	if fin != nil {
		ff = j.b.OpenFinally()
	}
	if handler != nil {
		cf = j.b.OpenCatch()
	}
	j.block(n.ChildByFieldId(k.fBody))
	if handler != nil {
		t := j.b.Push()
		j.b.EnterHandler(cf)
		mark := len(j.binds)
		if p := handler.ChildByFieldId(k.fParameter); p != nil {
			j.reset()
			j.declarePattern(p, false)
			j.bind(p, 0, 0)
		}
		j.block(handler.ChildByFieldId(k.fBody))
		j.binds = j.binds[:mark]
		j.b.Merge(t)
		j.b.Pop(t)
	}
	if fin != nil {
		normal := j.b.EnterFinally(ff)
		j.block(fin.ChildByFieldId(k.fBody))
		j.b.CloseFinally(ff, normal)
	}
}

// labeled collects a statement's labels: a loop or switch takes them as its
// frame's names, any other statement is a block frame only a labelled break
// targets.
func (j *jsLower) labeled(n *ts.Node, labels []string) {
	k := j.k
	labels = append(labels, string(j.text(n.ChildByFieldId(k.fLabel))))
	body := n.ChildByFieldId(k.fBody)
	switch body.KindId() {
	case k.forStatement, k.forInStatement, k.whileStatement, k.doStatement, k.switchStatement, k.labeledStatement:
		j.labels = labels
		j.stmt(body)
	default:
		f := j.b.OpenBlock(labels...)
		j.stmt(body)
		j.b.CloseFrame(f)
	}
}

// value lowers an expression evaluated for its value: reads are recorded,
// throwing constructs counted, and nodes created for every decision, every
// conditionally evaluated operand, every definition and every nested
// callable or class. inChain reports that n is the receiver of an enclosing
// member access or call of the same optional chain.
func (j *jsLower) value(n *ts.Node, inChain bool) {
	k := j.k
	id := n.KindId()
	if k.isCallable(id) {
		m := len(j.reads)
		j.capFunction(n)
		j.node(flow.Stmt, n, m, len(j.reads))
		return
	}
	switch id {
	case k.identifier, k.shorthandPropertyIdentifier:
		j.ref(n)
	case k.binaryExpression:
		left, right := n.ChildByFieldId(k.fLeft), n.ChildByFieldId(k.fRight)
		switch n.ChildByFieldId(k.fOperator).KindId() {
		case k.and, k.or, k.nullish:
			m := len(j.reads)
			j.value(left, false)
			j.node(flow.Branch, left, m, len(j.reads))
			p := j.b.Push()
			j.arm(right)
			j.b.Merge(p)
			j.b.Pop(p)
		default:
			j.value(left, false)
			j.value(right, false)
		}
	case k.ternaryExpression:
		cond := n.ChildByFieldId(k.fCondition)
		m := len(j.reads)
		j.value(cond, false)
		j.node(flow.Branch, cond, m, len(j.reads))
		p := j.b.Push()
		j.arm(n.ChildByFieldId(k.fConsequence))
		t := j.b.Push()
		j.b.Restore(p)
		j.arm(n.ChildByFieldId(k.fAlternative))
		j.b.Merge(t)
		j.b.Pop(p)
	case k.assignmentExpression:
		j.assign(n)
	case k.augmentedAssignmentExpression:
		j.augment(n)
	case k.updateExpression:
		arg := j.inner(n.ChildByFieldId(k.fArgument))
		if arg.KindId() == k.identifier {
			m := len(j.reads)
			j.ref(arg)
			j.def(j.node(flow.Stmt, n, m, len(j.reads)), j.lookup(arg))
			return
		}
		j.target(arg)
	case k.memberExpression, k.subscriptExpression, k.callExpression:
		j.chain(n, inChain)
	case k.newExpression, k.awaitExpression, k.yieldExpression, k.spreadElement:
		j.children(n, true)
		j.throws++
	case k.class:
		m := len(j.reads)
		j.capClass(n)
		j.node(flow.Stmt, n, m, len(j.reads))
	case k.jsxOpeningElement, k.jsxSelfClosingElement:
		j.jsxElement(n, true)
	case k.jsxClosingElement:
	default:
		j.children(n, true)
	}
}

// children walks n's named children, lowering them (lower) or collecting
// their captures.
func (j *jsLower) children(n *ts.Node, lower bool) {
	start, list := j.kids(n)
	for i := range list {
		if lower {
			j.value(&list[i], false)
		} else {
			j.cap(&list[i])
		}
	}
	j.done(start)
}

// chain lowers a member access, subscript or call, one link of a possibly
// optional chain. At the chain's outermost link every `?.` inside it is
// closed: one node spans the chain on the non-nullish path, joined with the
// nullish exits.
func (j *jsLower) chain(n *ts.Node, inChain bool) {
	k := j.k
	base := len(j.opt)
	m := len(j.reads)
	id := n.KindId()
	recv := n.ChildByFieldId(k.fObject)
	if id == k.callExpression {
		recv = n.ChildByFieldId(k.fFunction)
	}
	j.value(recv, true)
	if n.ChildByFieldId(k.fOptionalChain) != nil {
		j.node(flow.Branch, recv, m, len(j.reads))
		j.opt = append(j.opt, j.b.Push())
	}
	switch id {
	case k.subscriptExpression:
		j.value(n.ChildByFieldId(k.fIndex), false)
	case k.callExpression:
		j.value(n.ChildByFieldId(k.fArguments), false)
	}
	j.throws++
	if inChain || len(j.opt) == base {
		return
	}
	j.node(flow.Stmt, n, m, len(j.reads))
	for i := len(j.opt) - 1; i >= base; i-- {
		j.b.Merge(j.opt[i])
	}
	j.b.Pop(j.opt[base])
	j.opt = j.opt[:base]
}

// target evaluates a property write's reference: the object and index are
// read, and the write may throw.
func (j *jsLower) target(t *ts.Node) {
	k := j.k
	switch t.KindId() {
	case k.memberExpression:
		j.value(t.ChildByFieldId(k.fObject), false)
	case k.subscriptExpression:
		j.value(t.ChildByFieldId(k.fObject), false)
		j.value(t.ChildByFieldId(k.fIndex), false)
	default:
		j.value(t, false)
		return
	}
	j.throws++
}

func (j *jsLower) assign(n *ts.Node) {
	k := j.k
	left, right := j.inner(n.ChildByFieldId(k.fLeft)), n.ChildByFieldId(k.fRight)
	m := len(j.reads)
	switch left.KindId() {
	case k.identifier:
		j.value(right, false)
		j.def(j.node(flow.Stmt, n, m, len(j.reads)), j.lookup(left))
	case k.objectPattern, k.arrayPattern:
		j.value(right, false)
		j.bind(left, m, len(j.reads))
	case k.memberExpression, k.subscriptExpression:
		j.target(left)
		j.value(right, false)
	default:
		j.value(right, false)
	}
}

func (j *jsLower) augment(n *ts.Node) {
	k := j.k
	left, right := j.inner(n.ChildByFieldId(k.fLeft)), n.ChildByFieldId(k.fRight)
	op := n.ChildByFieldId(k.fOperator).KindId()
	logical := op == k.andAssign || op == k.orAssign || op == k.nullishAssign
	m := len(j.reads)
	v := int32(-1)
	if left.KindId() == k.identifier {
		v = j.lookup(left)
		j.ref(left)
	} else {
		j.target(left)
	}
	if !logical {
		j.value(right, false)
		if left.KindId() == k.identifier {
			j.def(j.node(flow.Stmt, n, m, len(j.reads)), v)
		}
		return
	}
	j.node(flow.Branch, left, m, len(j.reads))
	p := j.b.Push()
	m2 := len(j.reads)
	j.value(right, false)
	if left.KindId() != k.identifier {
		j.throws++
	}
	j.def(j.node(flow.Stmt, n, m2, len(j.reads)), v)
	j.b.Merge(p)
	j.b.Pop(p)
}

// bind lowers a binding or assignment pattern whose incoming value read
// reads[from:to]: one defining node per bound name.
func (j *jsLower) bind(p *ts.Node, from, to int) {
	k := j.k
	switch p.KindId() {
	case k.identifier, k.shorthandPropertyIdentifierPattern:
		j.def(j.node(flow.Stmt, p, from, to), j.lookup(p))
	case k.memberExpression, k.subscriptExpression:
		j.target(p)
		j.node(flow.Stmt, p, from, to)
	case k.parenthesizedExpression:
		j.bind(j.inner(p), from, to)
	case k.assignmentPattern:
		j.defaulted(p.ChildByFieldId(k.fLeft), p.ChildByFieldId(k.fRight), p, from, to)
	case k.restPattern:
		if c := j.firstKid(p); c != nil {
			j.bind(c, from, to)
		}
	case k.objectPattern, k.arrayPattern:
		start, list := j.kids(p)
		for i := range list {
			c := &list[i]
			j.throws++
			switch c.KindId() {
			case k.pairPattern:
				if key := c.ChildByFieldId(k.fKey); key.KindId() == k.computedPropertyName {
					j.value(key, false)
				}
				j.bind(c.ChildByFieldId(k.fValue), from, to)
			case k.objectAssignmentPattern:
				j.defaulted(c.ChildByFieldId(k.fLeft), c.ChildByFieldId(k.fRight), c, from, to)
			default:
				j.bind(c, from, to)
			}
		}
		j.done(start)
	}
}

// defaulted lowers a pattern element with a default: whole decides whether
// the incoming value is undefined, the default is evaluated only then.
func (j *jsLower) defaulted(left, dflt, whole *ts.Node, from, to int) {
	k := j.k
	named := left.KindId() == k.identifier || left.KindId() == k.shorthandPropertyIdentifierPattern
	v := int32(-1)
	if named {
		v = j.lookup(left)
	}
	j.def(j.node(flow.Branch, whole, from, to), v)
	p := j.b.Push()
	m := len(j.reads)
	j.value(dflt, false)
	j.def(j.node(flow.Stmt, dflt, m, len(j.reads)), v)
	j.b.Merge(p)
	j.b.Pop(p)
	if named {
		return
	}
	// A nested pattern destructures either value: the default's reads and,
	// copied after them, the incoming value's.
	j.reads = append(j.reads, j.reads[from:to]...)
	j.bind(left, m, len(j.reads))
}

// capFunction collects the enclosing variables a nested callable references,
// resolving names through its own parameters and scopes first. A computed
// method name is evaluated where the method is created.
func (j *jsLower) capFunction(fn *ts.Node) {
	k := j.k
	id := fn.KindId()
	if id == k.methodDefinition {
		if nm := fn.ChildByFieldId(k.fName); nm.KindId() == k.computedPropertyName {
			j.cap(nm)
		}
	}
	j.shadow++
	mark, savedFn := len(j.binds), j.fnMark
	j.fnMark = mark
	if id == k.functionExpression || id == k.generatorFunction {
		if nm := fn.ChildByFieldId(k.fName); nm != nil {
			j.declare(nm)
		}
	}
	if ps := fn.ChildByFieldId(k.fParameters); ps != nil {
		start, list := j.kids(ps)
		for i := range list {
			j.declarePattern(&list[i], false)
		}
		for i := range list {
			j.patternExprs(&list[i])
		}
		j.done(start)
	} else if p := fn.ChildByFieldId(k.fParameter); p != nil {
		j.declare(p)
	}
	body := fn.ChildByFieldId(k.fBody)
	if body.KindId() == k.statementBlock {
		j.hoistVars(body)
	}
	j.cap(body)
	j.binds, j.fnMark = j.binds[:mark], savedFn
	j.shadow--
}

// capClass collects a class's captures: its heritage, computed member names,
// field initializers and members. The class's own name is bound inside it.
// A heritage expression is evaluated, and may throw, where the class is
// created.
func (j *jsLower) capClass(n *ts.Node) {
	k := j.k
	start, list := j.kids(n)
	if j.shadow == 0 {
		for i := range list {
			if list[i].KindId() == k.classHeritage {
				j.throws++
			}
		}
	}
	j.shadow++
	mark := len(j.binds)
	if nm := n.ChildByFieldId(k.fName); nm != nil {
		j.declare(nm)
	}
	for i := range list {
		if list[i].KindId() != k.identifier {
			j.cap(&list[i])
		}
	}
	j.done(start)
	j.binds = j.binds[:mark]
	j.shadow--
}

// patternExprs collects the captures of a binding pattern's defaults and
// computed keys; its names are bound by declarePattern.
func (j *jsLower) patternExprs(p *ts.Node) {
	k := j.k
	switch p.KindId() {
	case k.assignmentPattern, k.objectAssignmentPattern:
		j.patternExprs(p.ChildByFieldId(k.fLeft))
		j.cap(p.ChildByFieldId(k.fRight))
	case k.pairPattern:
		if key := p.ChildByFieldId(k.fKey); key.KindId() == k.computedPropertyName {
			j.cap(key)
		}
		j.patternExprs(p.ChildByFieldId(k.fValue))
	case k.objectPattern, k.arrayPattern, k.restPattern:
		start, list := j.kids(p)
		for i := range list {
			j.patternExprs(&list[i])
		}
		j.done(start)
	}
}

// cap collects the references n makes to variables of the function being
// lowered, honouring every scope n opens.
func (j *jsLower) cap(n *ts.Node) {
	k := j.k
	id := n.KindId()
	if k.isCallable(id) {
		j.capFunction(n)
		return
	}
	switch id {
	case k.identifier, k.shorthandPropertyIdentifier, k.shorthandPropertyIdentifierPattern:
		j.ref(n)
	case k.classDeclaration, k.class:
		j.capClass(n)
	case k.statementBlock:
		mark := len(j.binds)
		start, list := j.kids(n)
		for i := range list {
			j.predeclare(&list[i])
		}
		for i := range list {
			j.cap(&list[i])
		}
		j.done(start)
		j.binds = j.binds[:mark]
	case k.switchStatement:
		j.cap(n.ChildByFieldId(k.fValue))
		mark := len(j.binds)
		body := n.ChildByFieldId(k.fBody)
		start, cases := j.kids(body)
		for c := range cases {
			s, list := j.kids(&cases[c])
			for i := range list {
				j.predeclare(&list[i])
			}
			j.done(s)
		}
		j.done(start)
		j.cap(body)
		j.binds = j.binds[:mark]
	case k.forStatement:
		mark := len(j.binds)
		if init := n.ChildByFieldId(k.fInitializer); init.KindId() == k.lexicalDeclaration {
			j.predeclare(init)
		}
		j.children(n, false)
		j.binds = j.binds[:mark]
	case k.forInStatement:
		mark := len(j.binds)
		left := j.inner(n.ChildByFieldId(k.fLeft))
		if kw := n.ChildByFieldId(k.fKind); kw == nil {
			j.cap(left)
		} else {
			if kw.KindId() != k.varKw {
				j.declarePattern(left, false)
			}
			j.patternExprs(left)
		}
		if v := n.ChildByFieldId(k.fValue); v != nil {
			j.cap(v)
		}
		j.cap(n.ChildByFieldId(k.fRight))
		j.cap(n.ChildByFieldId(k.fBody))
		j.binds = j.binds[:mark]
	case k.catchClause:
		mark := len(j.binds)
		if p := n.ChildByFieldId(k.fParameter); p != nil {
			j.declarePattern(p, false)
			j.patternExprs(p)
		}
		j.cap(n.ChildByFieldId(k.fBody))
		j.binds = j.binds[:mark]
	case k.variableDeclarator:
		j.patternExprs(n.ChildByFieldId(k.fName))
		if v := n.ChildByFieldId(k.fValue); v != nil {
			j.cap(v)
		}
	case k.jsxOpeningElement, k.jsxSelfClosingElement:
		j.jsxElement(n, false)
	case k.jsxClosingElement:
	default:
		j.children(n, false)
	}
}

// jsxElement walks a JSX tag: its name is a reference unless it is an
// intrinsic tag (a lowercase or hyphenated name), then its attributes.
func (j *jsLower) jsxElement(n *ts.Node, lower bool) {
	k := j.k
	name := n.ChildByFieldId(k.fName)
	start, list := j.kids(n)
	for i := range list {
		c := &list[i]
		if name != nil && c.StartByte() == name.StartByte() {
			if c.KindId() == k.identifier {
				t := j.text(c)
				if len(t) == 0 || t[0] >= 'a' && t[0] <= 'z' || bytes.IndexByte(t, '-') >= 0 {
					continue
				}
			} else if c.KindId() != k.memberExpression {
				continue
			}
		}
		if lower {
			j.value(c, false)
		} else {
			j.cap(c)
		}
	}
	j.done(start)
}

// jsSyntax holds the kind and field ids the JavaScript lowering matches,
// resolved once by name against the pinned grammar so the walk compares
// integers rather than converting every node's kind to a string.
type jsSyntax struct {
	// callable is indexed by kind id: the Lowering's callable set.
	callable []bool

	program, statementBlock, expressionStatement, lexicalDeclaration, variableDeclaration, usingDeclaration,
	variableDeclarator, functionDeclaration, generatorFunctionDeclaration, classDeclaration, class, classHeritage,
	ifStatement, elseClause, forStatement, forInStatement, whileStatement, doStatement, switchStatement,
	switchBody, switchCase, switchDefault, tryStatement, catchClause, finallyClause, labeledStatement,
	breakStatement, continueStatement, returnStatement, throwStatement, emptyStatement, debuggerStatement,
	withStatement, importStatement, exportStatement, hashBangLine, importClause, namespaceImport, namedImports,
	identifier, shorthandPropertyIdentifier, shorthandPropertyIdentifierPattern, parenthesizedExpression,
	sequenceExpression, assignmentExpression, augmentedAssignmentExpression, binaryExpression, ternaryExpression,
	updateExpression, callExpression, newExpression, awaitExpression, yieldExpression, memberExpression,
	subscriptExpression, spreadElement, functionExpression, generatorFunction, methodDefinition,
	classStaticBlock, trueLit, objectPattern, arrayPattern, assignmentPattern, objectAssignmentPattern,
	pairPattern, restPattern, computedPropertyName, jsxOpeningElement, jsxSelfClosingElement,
	jsxClosingElement uint16

	and, or, nullish, andAssign, orAssign, nullishAssign, varKw, ofKw uint16

	fAlias, fAlternative, fArgument, fArguments, fBody, fCondition, fConsequence, fDeclaration, fFinalizer,
	fFunction, fHandler, fIncrement, fIndex, fInitializer, fKey, fKind, fLabel, fLeft, fName, fObject,
	fOperator, fOptionalChain, fParameter, fParameters, fRight, fValue uint16
}

// isCallable reports whether kind id begins a function. An error node's id
// lies outside the grammar's kind count.
func (s *jsSyntax) isCallable(id uint16) bool { return int(id) < len(s.callable) && s.callable[id] }

var (
	jsSyntaxOnce  sync.Once
	jsSyntaxTable *jsSyntax
)

// jsSyntaxOf resolves the table once. A name the grammar does not define is
// a lowering defect and panics, so a misspelt kind can never silently match
// nothing.
func jsSyntaxOf(l *Lowering) *jsSyntax {
	jsSyntaxOnce.Do(func() {
		tl, ok := Grammar("javascript")
		if !ok {
			panic("worker: the javascript grammar is not linked")
		}
		id := func(name string, named bool) uint16 {
			v := tl.IdForNodeKind(name, named)
			if v == 0 {
				panic("worker: the javascript grammar has no kind " + name)
			}
			return v
		}
		kind := func(name string) uint16 { return id(name, true) }
		tok := func(name string) uint16 { return id(name, false) }
		field := func(name string) uint16 {
			v := tl.FieldIdForName(name)
			if v == 0 {
				panic("worker: the javascript grammar has no field " + name)
			}
			return v
		}
		s := &jsSyntax{callable: make([]bool, tl.NodeKindCount())}
		for name := range l.callable {
			s.callable[kind(name)] = true
		}
		s.program, s.statementBlock, s.expressionStatement = kind("program"), kind("statement_block"), kind("expression_statement")
		s.lexicalDeclaration, s.variableDeclaration, s.usingDeclaration = kind("lexical_declaration"), kind("variable_declaration"), kind("using_declaration")
		s.variableDeclarator, s.functionDeclaration = kind("variable_declarator"), kind("function_declaration")
		s.generatorFunctionDeclaration, s.classDeclaration = kind("generator_function_declaration"), kind("class_declaration")
		s.class, s.classHeritage, s.ifStatement, s.elseClause = kind("class"), kind("class_heritage"), kind("if_statement"), kind("else_clause")
		s.forStatement, s.forInStatement, s.whileStatement = kind("for_statement"), kind("for_in_statement"), kind("while_statement")
		s.doStatement, s.switchStatement, s.switchBody = kind("do_statement"), kind("switch_statement"), kind("switch_body")
		s.switchCase, s.switchDefault, s.tryStatement = kind("switch_case"), kind("switch_default"), kind("try_statement")
		s.catchClause, s.finallyClause, s.labeledStatement = kind("catch_clause"), kind("finally_clause"), kind("labeled_statement")
		s.breakStatement, s.continueStatement, s.returnStatement = kind("break_statement"), kind("continue_statement"), kind("return_statement")
		s.throwStatement, s.emptyStatement, s.debuggerStatement = kind("throw_statement"), kind("empty_statement"), kind("debugger_statement")
		s.withStatement, s.importStatement, s.exportStatement = kind("with_statement"), kind("import_statement"), kind("export_statement")
		s.hashBangLine, s.importClause, s.namespaceImport, s.namedImports = kind("hash_bang_line"), kind("import_clause"), kind("namespace_import"), kind("named_imports")
		s.identifier, s.shorthandPropertyIdentifier = kind("identifier"), kind("shorthand_property_identifier")
		s.shorthandPropertyIdentifierPattern = kind("shorthand_property_identifier_pattern")
		s.parenthesizedExpression, s.sequenceExpression = kind("parenthesized_expression"), kind("sequence_expression")
		s.assignmentExpression, s.augmentedAssignmentExpression = kind("assignment_expression"), kind("augmented_assignment_expression")
		s.binaryExpression, s.ternaryExpression, s.updateExpression = kind("binary_expression"), kind("ternary_expression"), kind("update_expression")
		s.callExpression, s.newExpression, s.awaitExpression = kind("call_expression"), kind("new_expression"), kind("await_expression")
		s.yieldExpression, s.memberExpression, s.subscriptExpression = kind("yield_expression"), kind("member_expression"), kind("subscript_expression")
		s.spreadElement, s.functionExpression, s.generatorFunction = kind("spread_element"), kind("function_expression"), kind("generator_function")
		s.methodDefinition, s.classStaticBlock, s.trueLit = kind("method_definition"), kind("class_static_block"), kind("true")
		s.objectPattern, s.arrayPattern, s.assignmentPattern = kind("object_pattern"), kind("array_pattern"), kind("assignment_pattern")
		s.objectAssignmentPattern, s.pairPattern, s.restPattern = kind("object_assignment_pattern"), kind("pair_pattern"), kind("rest_pattern")
		s.computedPropertyName, s.jsxOpeningElement = kind("computed_property_name"), kind("jsx_opening_element")
		s.jsxSelfClosingElement, s.jsxClosingElement = kind("jsx_self_closing_element"), kind("jsx_closing_element")
		s.and, s.or, s.nullish = tok("&&"), tok("||"), tok("??")
		s.andAssign, s.orAssign, s.nullishAssign = tok("&&="), tok("||="), tok("??=")
		s.varKw, s.ofKw = tok("var"), tok("of")
		s.fAlias, s.fAlternative, s.fArgument, s.fArguments = field("alias"), field("alternative"), field("argument"), field("arguments")
		s.fBody, s.fCondition, s.fConsequence, s.fDeclaration = field("body"), field("condition"), field("consequence"), field("declaration")
		s.fFinalizer, s.fFunction, s.fHandler, s.fIncrement = field("finalizer"), field("function"), field("handler"), field("increment")
		s.fIndex, s.fInitializer, s.fKey, s.fKind, s.fLabel = field("index"), field("initializer"), field("key"), field("kind"), field("label")
		s.fLeft, s.fName, s.fObject, s.fOperator = field("left"), field("name"), field("object"), field("operator")
		s.fOptionalChain, s.fParameter, s.fParameters = field("optional_chain"), field("parameter"), field("parameters")
		s.fRight, s.fValue = field("right"), field("value")
		jsSyntaxTable = s
	})
	return jsSyntaxTable
}
