package worker

import (
	"bytes"
	"sync"

	ts "github.com/tree-sitter/go-tree-sitter"

	"github.com/Sawmonabo/codectx/internal/provider/treesitter/flow"
)

// javascriptLowering lowers JavaScript callables: function declarations and
// expressions, arrow functions, methods, generator functions (async is a
// modifier of these kinds), class bodies, and the program itself, whose
// top-level code is one function. A class body is the unit of the class's
// field initializers and static blocks.
var javascriptLowering = Lowering{language: "javascript", callables: jsCallables, lower: jsJavaScript.lower}

// typescriptLowering and tsxLowering lower TypeScript and TSX with the
// JavaScript lowering, on their own grammars' tables: the callables are
// JavaScript's (a signature without a body is not one), and the TypeScript
// forms are lowered as TypeScript in lowerJavaScript states. TSX adds JSX,
// which the JavaScript lowering already handles.
var (
	typescriptLowering = Lowering{language: "typescript", callables: jsCallables, ambient: tsAmbient, lower: jsTypeScript.lower}
	tsxLowering        = Lowering{language: "tsx", callables: jsCallables, ambient: tsAmbient, lower: jsTSX.lower}
)

// tsAmbient is the TypeScript `declare` form: nothing under it runs, so a
// function or class body written there is not a callable.
var tsAmbient = []string{"ambient_declaration"}

// jsCallables are the callable kinds of every grammar the JavaScript lowering
// runs on.
var jsCallables = []string{"program", "function_declaration", "function_expression", "arrow_function",
	"method_definition", "generator_function", "generator_function_declaration", "class_body"}

// jsJavaScript, jsTypeScript and jsTSX are the syntax tables of the grammars
// the JavaScript lowering runs on.
var (
	jsJavaScript = jsGrammar{language: "javascript"}
	jsTypeScript = jsGrammar{language: "typescript"}
	jsTSX        = jsGrammar{language: "tsx"}
)

// jsGrammar is the syntax table of one grammar the JavaScript lowering runs
// on, resolved once, on first use.
type jsGrammar struct {
	language string
	once     sync.Once
	s        *jsSyntax
}

// lower is the Lowering.lower of the grammar: lowerJavaScript over its table.
func (g *jsGrammar) lower(l *Lowering, b *flow.Builder, fn *ts.Node, src []byte) {
	g.once.Do(func() { g.s = resolveJSSyntax(g.language) })
	lowerJavaScript(l, b, fn, src, g.s)
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
//   - A function declaration is hoisted: one node at the start of its block,
//     spanning its name, defines the name. At the declaration's own position
//     a Stmt node spanning the declaration creates the function (see nested
//     callables below) and may-defines the name, so a use after the
//     declaration sees the captures read there.
//   - A condition is one Branch node spanning the condition without its
//     parentheses: if, while, do…while, for. A switch is a Stmt node for the
//     discriminant then one Branch node per case test, in source order (each
//     test is evaluated only when the previous failed), that Uses the
//     discriminant's reads with its own, since it compares the two; a case
//     body's end
//     flows into the next body when it does not break. A for…in/for…of loop
//     is a Stmt node for the iterated expression, evaluated once, then a
//     Branch head spanning the head clause from the left side to the end of
//     the iterated expression (whether another element is assigned), which
//     defines nothing: each name the left side binds is defined on the body
//     path, after the head, by the destructuring rule below (a bare
//     identifier is one defining node spanning it), so the exit edge carries
//     the definitions from before the loop.
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
//     Elements are lowered in source order: the language assigns them one at
//     a time, each step may throw, and a default or computed key reads the
//     elements assigned before it. Every element node carries every read of
//     the incoming value, so each one reads the right-hand values as they
//     were before any element of the statement was written — directly, or,
//     for a variable an earlier element wrote (`[x, y] = [y, x]`), through
//     that element's node, which read the old value before writing it: the
//     rule both lowerings apply to a cyclic multi-assignment, a sound
//     over-approximation.
//   - A nested callable or class is its own function; in the enclosing
//     function its creating expression is one Stmt node spanning it, which
//     Uses every enclosing variable referenced inside it, resolved with its
//     own scopes so a name it declares shadows, and may-defines (MayDef, a
//     non-killing definition) every enclosing variable assigned inside it:
//     when the closure runs is unknown, so a use after its creation sees both
//     the closure's write and every definition reaching the creation. A
//     class's field initializers and static blocks, static and instance
//     alike, are its class body's unit, lowered in source order: each
//     initializer is the nodes of its value then one Stmt node spanning the
//     field definition, and each static block is a block with its own var
//     scope. The class node in the enclosing function also captures them.
//   - An expression lowered for its value and ended by a node spanning it (an
//     expression statement, a conditional operand, an arrow's expression
//     body, a default export) takes no second node when the last node its own
//     lowering made already spans it, as an optional chain, a nested
//     callable or class, or an assignment does.
//   - A statement that stands alone as the body of an if, else, loop, with
//     or label is scoped to an implicit block of its own, as a braced body
//     is: a function declaration there is hoisted within it and binds its
//     name there, never an enclosing variable of the same name.
//   - `export { … }`, `export * from …` and `export { … } from …` evaluate
//     nothing where they stand (they declare the module's export bindings),
//     so they make no node, as an import statement makes none.
//   - A loop whose condition is the literal `true`, or a for loop without a
//     condition, has no exit edge: only a break leaves it. Its head is a Stmt
//     node spanning `true`, or the `for` keyword.
//
// # Uses
//
// Only an identifier resolving to a variable declared in this function is a
// Use. A node Uses every variable read inside its own span (the value it
// computes derives from them, since the lowering introduces no temporaries)
// and no read outside it: a read belongs to the node that evaluates it, and
// a read no node of its own evaluates (the receiver of a call, an operand of
// a plain operator) belongs to the node of the enclosing expression that
// consumes it. An assignment, compound assignment or update of an identifier
// embedded in a larger expression is its own defining node, and the variable
// it defined is a read of the node that evaluates the enclosing expression,
// since the expression's value is the value assigned. A node that defines v
// also Uses v when its statement read v before it: the enclosing expression
// consumes that earlier value only after the definition has overwritten it,
// so the defining node carries it, read before its write (`f(x, x = 1)`),
// the rule a destructuring swap follows. A destructuring or for…in/for…of
// defining node also Uses the variables of the value it destructures, and a
// destructuring element those of its computed key and property target. A
// property write (`o.p = v`, `a[i] = v`) is a Stmt node spanning the
// assignment that Uses o, a, i and v and defines nothing.
//
// # Exceptions
//
// MayThrow is given to every node whose own evaluation — the part of the
// source evaluated since the previous node — contains a call, `new`,
// `await`, `yield`, a spread, a property read or write, a destructuring
// element, a for…of iterator step (including for await), or a class
// declaration's heritage; a throw statement's node is also a Throw. A plain
// or compound property write throws when the value is stored, so its throw
// is counted after its right side, on the write's node. The Builder applies MayThrow
// only inside an open catch or finally frame, where the throwing node's own
// definitions do not reach the handler.
//
// # Scoping
//
// `var` and a function's parameters are function-scoped and hoisted, as are
// import bindings in the program; `let`, `const`, `using`, `class` and a
// function declaration in a block are block-scoped and bound from the block's
// start (the strict-mode rule, which modules and classes impose); a catch
// parameter is scoped to its clause and a `for (let …)` binding to its loop;
// a switch body is one block. Parameter defaults see the parameters only.
//
// # TypeScript
//
// Syntax the compiler erases produces no node and binds no variable (the
// handbook's "Erased Types"): type annotations, type parameters and
// arguments, interfaces, type aliases, `declare` declarations, overload and
// method signatures without a body, abstract members, index signatures,
// `implements` clauses, `import type`, a `type` import specifier, and a
// `const enum` (removed at compilation, the handbook's "const enums"); an
// erased subtree is skipped wherever it stands, so a `typeof x` inside a
// type reads nothing. `e!`, `e as T`, `e satisfies T`, `<T>e` and `f<T>`
// evaluate to their operand and lower as it: a node the enclosing construct
// makes for the whole expression (a condition's Branch, a statement's node)
// spans the expression as written, wrapper included, and a node the
// operand's own lowering made (an optional chain, a nested callable, an
// assignment) stands for the wrapper. An assignment target is read through
// the wrappers, so `x! = e` defines x.
//
// The forms that run: a parameter wrapper (required, optional, or a
// parameter property such as `public x`) is its pattern, and its default is
// a default by the rule above, the Branch spanning the whole parameter;
// its decorators are evaluated where the class is created, with the
// class's other decorators, heritage and computed names. A decorator
// application is a call: a class node whose class, members or parameters
// carry a decorator may throw, and an exported class's decorators are read
// by its node. (This holds for JavaScript decorators too.) `public_field_definition`
// is the field initializer of the class body's unit. An abstract class is a
// class. The compiler emits an enum, a namespace and `import x = …` as a
// `var` with an assignment where the declaration stands, so their names are
// function-scoped and hoisted like var, and a second declaration of the
// same name merges into the same variable:
//
//   - An enum is one Stmt node spanning its name that Uses and defines the
//     name (the emitted `E || (E = {})`), then one Stmt node per member
//     initializer, spanning the member, in order; every member's bare name
//     is bound to no variable throughout the body, since inside an
//     initializer it names the member (the handbook's "Enums").
//   - A namespace with a body runs its body once, immediately, where it
//     stands (the emitted immediately invoked function), so it is lowered
//     inline, not as a callable: one Stmt node spanning the leftmost name
//     of its path that Uses and defines that name, then the body as a block
//     with its own var scope. A namespace named by a string is ambient.
//   - `import x = require(m)` is one Stmt node spanning x that defines it
//     and may throw; `import x = A.B` is one spanning x that Uses A and
//     defines x, and may throw when it reads a property.
//   - `export = e` evaluates e like a default export.
func lowerJavaScript(l *Lowering, b *flow.Builder, fn *ts.Node, src []byte, k *jsSyntax) {
	cur := fn.Walk()
	defer cur.Close()
	j := jsLower{l: l, b: b, src: src, k: k, cur: cur, first: -1, last: -1, stmtNo: 1}
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
	case k.classBody:
		j.classBody(fn)
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
		j.valueNode(body)
	}
}

// classBody lowers a class body's unit: every field initializer and static
// block, in source order.
func (j *jsLower) classBody(n *ts.Node) {
	k := j.k
	start, list := j.kids(n)
	for i := range list {
		switch m := &list[i]; m.KindId() {
		case k.fieldDefinition, k.publicFieldDefinition:
			if v := m.ChildByFieldId(k.fValue); v != nil {
				j.reset()
				j.value(v, false)
				j.node(flow.Stmt, m, 0, len(j.reads))
			}
		case k.classStaticBlock:
			mark := len(j.binds)
			j.fnMark = mark
			body := m.ChildByFieldId(k.fBody)
			j.hoistVars(body)
			j.block(body)
			j.binds = j.binds[:mark]
		}
	}
	j.done(start)
}

// jsLower is the state of lowering one JavaScript callable.
type jsLower struct {
	l   *Lowering
	b   *flow.Builder
	src []byte
	k   *jsSyntax
	cur *ts.TreeCursor
	// buf is a stack of child lists; kids pushes one and done pops it.
	buf []ts.Node
	// binds is the scope chain, innermost last; fnMark is where the
	// innermost function scope begins, the target of var hoisting.
	binds  scope
	fnMark int
	// shadow is non-zero while walking a nested callable or class for its
	// captures: declarations then bind -1 and no node is created.
	shadow int
	// reads are the variables read by the current statement, in evaluation
	// order; seen[v] == stmtNo marks v as one of them, stmtNo numbering the
	// statements.
	reads  []int32
	seen   []int
	stmtNo int
	// writes are the enclosing variables assigned inside the nested callable
	// or class whose captures are being collected; closure may-defines them
	// on its creating node.
	writes []int32
	// throws counts throwing constructs evaluated by the current statement;
	// those past thrown are not yet attached to a node.
	throws, thrown int
	// first is the first node created since the last open, or -1.
	first int32
	// last is the node created last, or -1, and lastSpan its span.
	last     int32
	lastSpan flow.Span
	// opt holds the fringes saved at each `?.` of the optional chains being
	// lowered, innermost chain last.
	opt []flow.Fringe
	// labels are the statement labels the next statement takes.
	labels []string
	// held keeps the reads of the discriminants of the switches whose case
	// tests are being lowered; match holds their case tests' fringes.
	held  []int32
	match []flow.Fringe
	// decorated counts the decorators collected: a class whose collection
	// counted one may throw where it is created.
	decorated int
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

func (j *jsLower) text(n *ts.Node) []byte { return textOf(j.src, n) }

// declare binds name in the innermost scope: a new variable, or -1 inside a
// nested callable.
func (j *jsLower) declare(name *ts.Node) int32 {
	v := int32(-1)
	if j.shadow == 0 {
		v = j.b.Var()
	}
	j.binds = append(j.binds, binding{name: j.text(name), v: v})
	return v
}

// lookup resolves name through the scope chain: its variable, or -1 when it
// is declared inside a nested callable or not in this function at all.
func (j *jsLower) lookup(name *ts.Node) int32 { return j.binds.lookup(j.text(name)) }

// boundSince reports whether name is bound in binds[from:].
func (j *jsLower) boundSince(from int, name *ts.Node) bool {
	return j.binds.find(j.text(name), from) >= 0
}

// ref records a read of name when it resolves to a variable of this function.
func (j *jsLower) ref(name *ts.Node) { j.read(j.lookup(name)) }

// read records a read of v, unless v is -1.
func (j *jsLower) read(v int32) {
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
func (j *jsLower) reset() {
	j.reads, j.throws, j.thrown = j.reads[:0], 0, 0
	j.stmtNo++
}

// node creates a node spanning n that Uses reads[from:to], and MayThrow when
// a throwing construct was evaluated since the previous node: that throw
// happens before this node's definitions, which is what the Handler sees.
func (j *jsLower) node(kind flow.Kind, n *ts.Node, from, to int) int32 {
	return j.nodeAt(kind, spanOf(n), from, to)
}

// nodeAt is node over the span s.
func (j *jsLower) nodeAt(kind flow.Kind, s flow.Span, from, to int) int32 {
	id := j.b.Node(kind, s)
	if j.first < 0 {
		j.first = id
	}
	j.uses(id, from, to)
	if j.throws > j.thrown {
		j.b.MayThrow(id)
	}
	j.thrown = j.throws
	j.last, j.lastSpan = id, s
	return id
}

// uses records reads[from:to] as Uses of node id.
func (j *jsLower) uses(id int32, from, to int) {
	for _, v := range j.reads[from:to] {
		j.b.Use(id, v)
	}
}

// def records that node n defines v and, when the statement read v before
// n, that n Uses v (see Uses in lowerJavaScript).
func (j *jsLower) def(n, v int32) {
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
// parentheses, with no throw evaluated after it: that node then stands for
// n. A read-back recorded after it (an assignment's variable) is read by the
// enclosing expression's node, which reads every variable read inside it; at
// statement level nothing encloses n.
func (j *jsLower) valueNode(n *ts.Node) {
	m, last := len(j.reads), j.last
	j.value(n, false)
	if j.last != last && j.lastSpan == spanOf(j.strip(n)) && j.thrown == j.throws {
		return
	}
	j.node(flow.Stmt, n, m, len(j.reads))
}

// strip is n without parentheses and without the TypeScript wrappers that
// evaluate to their operand.
func (j *jsLower) strip(n *ts.Node) *ts.Node {
	k := j.k
	for n != nil {
		switch n.KindId() {
		case k.parenthesizedExpression:
			n = firstNamed(n)
		case k.nonNullExpression, k.asExpression, k.satisfiesExpression, k.typeAssertion, k.instantiationExpression:
			o := j.operand(n)
			if o == nil {
				return n
			}
			n = o
		default:
			return n
		}
	}
	return n
}

// operand is the expression a transparent TypeScript wrapper evaluates: its
// first named child that is not a type argument list or a comment.
func (j *jsLower) operand(n *ts.Node) *ts.Node {
	for i := range n.NamedChildCount() {
		if c := n.NamedChild(i); !c.IsExtra() && c.KindId() != j.k.typeArguments {
			return c
		}
	}
	return nil
}

// erased reports whether n is syntax the TypeScript compiler erases.
func (j *jsLower) erased(n *ts.Node) bool {
	id := n.KindId()
	if int(id) >= len(j.k.erased) || !n.IsNamed() {
		return false
	}
	if id == j.k.enumDeclaration {
		return j.hasTok(n, j.k.constKw)
	}
	return j.k.erased[id] || id == j.k.importStatement && j.hasTok(n, j.k.typeKw)
}

// hasTok reports whether one of n's anonymous children is the token id; an
// id the grammar lacks (0) is never one.
func (j *jsLower) hasTok(n *ts.Node, id uint16) bool {
	if id == 0 {
		return false
	}
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

// root is the leftmost identifier of a dotted name (a namespace path, an
// import alias's target), or n itself.
func (j *jsLower) root(n *ts.Node) *ts.Node {
	k := j.k
	for n != nil && (n.KindId() == k.nestedIdentifier || n.KindId() == k.memberExpression) {
		n = n.ChildByFieldId(k.fObject)
	}
	return n
}

// paramPattern is the pattern a TypeScript parameter wrapper binds.
func (j *jsLower) paramPattern(p *ts.Node) *ts.Node {
	if pat := p.ChildByFieldId(j.k.fPattern); pat != nil {
		return pat
	}
	return p.ChildByFieldId(j.k.fName)
}

// namespaceOf is the namespace n declares, as a declaration or as the
// expression of an expression statement, or nil.
func (j *jsLower) namespaceOf(n *ts.Node) *ts.Node {
	k := j.k
	if n.KindId() == k.expressionStatement {
		if n = firstNamed(n); n == nil {
			return nil
		}
	}
	if id := n.KindId(); id == k.internalModule || id == k.module {
		return n
	}
	return nil
}

// enum lowers an enum declaration (see TypeScript in lowerJavaScript).
func (j *jsLower) enum(n *ts.Node) {
	k := j.k
	name := n.ChildByFieldId(k.fName)
	v := j.lookup(name)
	j.read(v)
	j.def(j.node(flow.Stmt, name, 0, 0), v)
	j.enumMembers(n, true)
}

// enumMembers binds every member name of an enum to no variable, then walks
// the member initializers in order, lowering (lower) or collecting the
// captures of each.
func (j *jsLower) enumMembers(n *ts.Node, lower bool) {
	k := j.k
	body := n.ChildByFieldId(k.fBody)
	if body == nil {
		return
	}
	mark := len(j.binds)
	start, list := j.kids(body)
	for i := range list {
		member := &list[i]
		if member.KindId() == k.enumAssignment {
			member = member.ChildByFieldId(k.fName)
		}
		j.binds = append(j.binds, binding{name: j.text(member), v: -1})
	}
	for i := range list {
		m := &list[i]
		val := m.ChildByFieldId(k.fValue)
		if m.KindId() != k.enumAssignment || val == nil {
			continue
		}
		if lower {
			j.reset()
			j.value(val, false)
			j.node(flow.Stmt, m, 0, len(j.reads))
		} else {
			j.cap(val)
		}
	}
	j.done(start)
	j.binds = j.binds[:mark]
}

// namespace lowers a namespace with a body inline (see TypeScript in
// lowerJavaScript).
func (j *jsLower) namespace(n *ts.Node) {
	k := j.k
	name, body := j.root(n.ChildByFieldId(k.fName)), n.ChildByFieldId(k.fBody)
	if name == nil || name.KindId() != k.identifier || body == nil {
		return
	}
	v := j.lookup(name)
	j.read(v)
	j.def(j.node(flow.Stmt, name, 0, 0), v)
	mark, savedFn := len(j.binds), j.fnMark
	j.fnMark = mark
	j.hoistVars(body)
	j.block(body)
	j.binds, j.fnMark = j.binds[:mark], savedFn
}

// importEquals lowers `import x = require(m)` and `import x = A.B`.
func (j *jsLower) importEquals(n *ts.Node) {
	k := j.k
	start, list := j.kids(n)
	for i := range list {
		c := &list[i]
		switch c.KindId() {
		case k.importRequireClause:
			if id := firstNamed(c); id != nil {
				j.throws++
				j.def(j.node(flow.Stmt, id, 0, 0), j.lookup(id))
			}
		case k.identifier, k.nestedIdentifier:
			if n.KindId() != k.importAlias || i == 0 {
				continue
			}
			if c.KindId() == k.nestedIdentifier {
				j.throws++
			}
			j.ref(j.root(c))
			j.def(j.node(flow.Stmt, &list[0], 0, len(j.reads)), j.lookup(&list[0]))
		}
	}
	j.done(start)
}

// hoistEquals binds, like var, the name an import-equals declaration or an
// import alias declares.
func (j *jsLower) hoistEquals(n *ts.Node) {
	k := j.k
	start, list := j.kids(n)
	for i := range list {
		switch c := &list[i]; {
		case c.KindId() == k.importRequireClause:
			if id := firstNamed(c); id != nil {
				j.declarePattern(id, true)
			}
		case n.KindId() == k.importAlias && i == 0:
			j.declarePattern(c, true)
		}
	}
	j.done(start)
}

// decorators reads the decorators among n's children.
func (j *jsLower) decorators(n *ts.Node) {
	start, list := j.kids(n)
	for i := range list {
		if list[i].KindId() == j.k.decorator {
			j.cap(&list[i])
		}
	}
	j.done(start)
}

// readBack records v, the variable an embedded assignment just defined, as a
// read of the node evaluating the enclosing expression.
func (j *jsLower) readBack(v int32) { j.read(v) }

// closure creates the node spanning n, a nested callable or class: it Uses
// n's captures and may-defines every enclosing variable n assigns.
func (j *jsLower) closure(n *ts.Node) int32 { return j.closureFrom(n, len(j.reads)) }

// closureFrom is closure whose node also Uses reads[m:], read before n (an
// exported class's decorators).
func (j *jsLower) closureFrom(n *ts.Node, m int) int32 {
	w := len(j.writes)
	if j.l.isCallable(n) {
		j.capFunction(n)
	} else {
		j.capClass(n)
	}
	id := j.node(flow.Stmt, n, m, len(j.reads))
	for _, v := range j.writes[w:] {
		j.b.MayDef(id, v)
	}
	j.writes = j.writes[:w]
	return id
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
	case k.classDeclaration, k.abstractClassDeclaration, k.functionDeclaration, k.generatorFunctionDeclaration:
		j.declare(n.ChildByFieldId(k.fName))
	case k.exportStatement:
		if d := n.ChildByFieldId(k.fDeclaration); d != nil {
			j.predeclare(d)
		}
	}
}

// hoistFunction emits a function declaration's hoisted defining node, spanning
// its name, at the start of its block.
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
	name := n.ChildByFieldId(k.fName)
	j.def(j.node(flow.Stmt, name, 0, 0), j.lookup(name))
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
	case k.requiredParameter, k.optionalParameter:
		j.declarePattern(j.paramPattern(p), merge)
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
	case k.enumDeclaration:
		if !j.erased(n) {
			j.declarePattern(n.ChildByFieldId(k.fName), true)
		}
	case k.internalModule, k.module, k.expressionStatement:
		// A namespace's own var names stay in its body's scope.
		if ns := j.namespaceOf(n); ns != nil && ns.ChildByFieldId(k.fBody) != nil {
			if name := j.root(ns.ChildByFieldId(k.fName)); name != nil && name.KindId() == k.identifier {
				j.declarePattern(name, true)
			}
		}
	case k.importStatement, k.importAlias:
		if !j.erased(n) {
			j.hoistEquals(n)
		}
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

// imports binds and defines the local names of one import statement, walking
// its clause as the extraction does; a `type` specifier binds nothing.
func (j *jsLower) imports(n *ts.Node) {
	k := j.k
	if j.erased(n) {
		return
	}
	start, list := j.kids(n)
	for i := range list {
		c := &list[i]
		if c.KindId() != k.importClause {
			continue
		}
		importClauseBindings(j.cur, c, &k.imports, func(local *ts.Node, typeOnly bool) {
			if !typeOnly {
				j.importName(local)
			}
		})
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
	if j.erased(n) {
		return
	}
	switch n.KindId() {
	case k.expressionStatement:
		if e := firstNamed(n); e != nil {
			j.exprStmt(e)
		}
	case k.lexicalDeclaration, k.usingDeclaration:
		j.declaration(n, false)
	case k.variableDeclaration:
		j.declaration(n, true)
	case k.emptyStatement, k.debuggerStatement, k.hashBangLine:
	case k.importStatement, k.importAlias:
		// An ES import's bindings are the program's, defined at its start.
		j.importEquals(n)
	case k.enumDeclaration:
		j.enum(n)
	case k.internalModule, k.module:
		j.namespace(n)
	case k.functionDeclaration, k.generatorFunctionDeclaration:
		id := j.closure(n)
		if v := j.lookup(n.ChildByFieldId(k.fName)); v >= 0 {
			j.b.MayDef(id, v)
		}
	case k.classDeclaration, k.abstractClassDeclaration:
		j.def(j.closure(n), j.lookup(n.ChildByFieldId(k.fName)))
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
		if e := firstNamed(n); e != nil {
			j.value(e, false)
		}
		j.node(flow.Jump, n, 0, len(j.reads))
		j.b.Return()
	case k.throwStatement:
		if e := firstNamed(n); e != nil {
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
		j.valueNode(j.l.unparen(n.ChildByFieldId(k.fObject)))
		j.sub(n.ChildByFieldId(k.fBody))
	case k.exportStatement:
		// An export with neither a declaration nor a value evaluates nothing.
		d := n.ChildByFieldId(k.fDeclaration)
		switch {
		case d != nil && (d.KindId() == k.classDeclaration || d.KindId() == k.abstractClassDeclaration):
			m, dec := len(j.reads), j.decorated
			j.decorators(n)
			if j.decorated > dec {
				j.throws++
			}
			j.def(j.closureFrom(d, m), j.lookup(d.ChildByFieldId(k.fName)))
		case d != nil:
			j.stmt(d)
		case n.ChildByFieldId(k.fValue) != nil:
			j.valueNode(n.ChildByFieldId(k.fValue))
		case j.hasTok(n, k.eqTok):
			if e := firstNamed(n); e != nil {
				j.valueNode(e)
			}
		}
	default:
		j.valueNode(n)
	}
}

// sub lowers n, a statement standing alone as the body of an if, else, loop,
// with or label, in an implicit block of its own: a declaration there binds
// its names in that block, and a function declaration is hoisted within it.
func (j *jsLower) sub(n *ts.Node) {
	mark := len(j.binds)
	j.predeclare(n)
	j.hoistFunction(n)
	j.stmt(n)
	j.binds = j.binds[:mark]
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
			j.reset()
			j.exprStmt(&list[i])
		}
		j.done(start)
		return
	}
	m := len(j.reads)
	// made reports that the assignment made the statement's own node; the
	// variable it defined is not read back, since nothing encloses it.
	made := false
	switch u := j.strip(e); u.KindId() {
	case k.internalModule, k.module:
		j.namespace(u)
		return
	case k.assignmentExpression:
		made, _ = j.assign(u)
	case k.augmentedAssignmentExpression:
		made, _ = j.augment(u)
	case k.updateExpression:
		made, _ = j.update(u)
	default:
		j.valueNode(e)
		return
	}
	if made && j.thrown == j.throws {
		return
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
	cond := j.l.unparen(n.ChildByFieldId(k.fCondition))
	j.value(cond, false)
	j.node(flow.Branch, cond, 0, len(j.reads))
	p := j.b.Push()
	j.sub(n.ChildByFieldId(k.fConsequence))
	t := j.b.Push()
	j.b.Restore(p)
	if alt := n.ChildByFieldId(k.fAlternative); alt != nil {
		if s := firstNamed(alt); s != nil {
			j.sub(s)
		}
	}
	j.b.Merge(t)
	j.b.Pop(p)
}

// isTrue reports whether a loop condition is the literal true.
func (j *jsLower) isTrue(cond *ts.Node) bool { return j.l.unparen(cond).KindId() == j.k.trueLit }

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

func (j *jsLower) doStmt(n *ts.Node, labels []string) {
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
	j.sub(n.ChildByFieldId(k.fBody))
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
	left := j.strip(n.ChildByFieldId(k.fLeft))
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
	property := left.KindId() == k.memberExpression || left.KindId() == k.subscriptExpression
	if property {
		j.target(left)
	}
	head := flow.Span{Start: uint32(n.ChildByFieldId(k.fLeft).StartByte()), End: uint32(right.EndByte())}
	h := j.nodeAt(flow.Branch, head, 0, len(j.reads))
	exit := j.b.Push()
	if !property {
		j.bind(left, 0, rEnd)
	}
	j.sub(n.ChildByFieldId(k.fBody))
	j.b.ContinueHere(f)
	j.loopEnd(f, h, true, exit)
	j.binds = j.binds[:mark]
}

func (j *jsLower) switchStmt(n *ts.Node, labels []string) {
	k := j.k
	disc := j.l.unparen(n.ChildByFieldId(k.fValue))
	j.value(disc, false)
	j.node(flow.Stmt, disc, 0, len(j.reads))
	// Each case test compares the discriminant's value with its own, so its
	// Branch reads what the discriminant read; held keeps those reads past
	// the resets of the hoisted declarations and the tests.
	hb := len(j.held)
	j.held = append(j.held, j.reads...)
	he := len(j.held)
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
	// match[c] is the fringe of case c's test; a nested switch in a body
	// pushes past it, so it is indexed from fb, not sliced.
	fb := len(j.match)
	j.match = append(j.match, make([]flow.Fringe, len(cases))...)
	pushed := false
	var firstPush flow.Fringe
	for c := range cases {
		if cases[c].KindId() != k.switchCase {
			continue
		}
		j.reset()
		for _, dv := range j.held[hb:he] {
			j.read(dv)
		}
		v := cases[c].ChildByFieldId(k.fValue)
		j.value(v, false)
		j.node(flow.Branch, v, 0, len(j.reads))
		j.match[fb+c] = j.b.Push()
		if !pushed {
			firstPush, pushed = j.match[fb+c], true
		}
	}
	j.held = j.held[:hb]
	noMatch := j.b.Push()
	if !pushed {
		firstPush = noMatch
	}
	hasDefault := false
	for c := range cases {
		h := j.match[fb+c]
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
	j.match = j.match[:fb]
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
		j.b.EnterHandler(cf, spanOf(handler.Child(0)))
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
		normal := j.b.EnterFinally(ff, spanOf(fin.Child(0)))
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
		j.sub(body)
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
	if j.l.isCallable(n) {
		j.closure(n)
		return
	}
	if j.erased(n) {
		return
	}
	switch id {
	case k.identifier, k.shorthandPropertyIdentifier:
		j.ref(n)
	case k.nonNullExpression, k.asExpression, k.satisfiesExpression, k.typeAssertion, k.instantiationExpression:
		if o := j.operand(n); o != nil {
			j.value(o, inChain)
		}
	case k.binaryExpression:
		left, right := n.ChildByFieldId(k.fLeft), n.ChildByFieldId(k.fRight)
		switch n.ChildByFieldId(k.fOperator).KindId() {
		case k.and, k.or, k.nullish:
			m := len(j.reads)
			j.value(left, false)
			j.node(flow.Branch, left, m, len(j.reads))
			p := j.b.Push()
			j.valueNode(right)
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
		j.valueNode(n.ChildByFieldId(k.fConsequence))
		t := j.b.Push()
		j.b.Restore(p)
		j.valueNode(n.ChildByFieldId(k.fAlternative))
		j.b.Merge(t)
		j.b.Pop(p)
	case k.assignmentExpression:
		_, v := j.assign(n)
		j.readBack(v)
	case k.augmentedAssignmentExpression:
		_, v := j.augment(n)
		j.readBack(v)
	case k.updateExpression:
		_, v := j.update(n)
		j.readBack(v)
	case k.memberExpression, k.subscriptExpression, k.callExpression:
		j.chain(n, inChain)
	case k.newExpression, k.awaitExpression, k.yieldExpression, k.spreadElement:
		j.children(n, true)
		j.throws++
	case k.class:
		j.closure(n)
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

// reference evaluates a property reference's object and index and reports
// whether t is one; any other target is evaluated as a value.
func (j *jsLower) reference(t *ts.Node) bool {
	k := j.k
	switch t.KindId() {
	case k.memberExpression:
		j.value(t.ChildByFieldId(k.fObject), false)
	case k.subscriptExpression:
		j.value(t.ChildByFieldId(k.fObject), false)
		j.value(t.ChildByFieldId(k.fIndex), false)
	default:
		j.value(t, false)
		return false
	}
	return true
}

// target evaluates the reference of a property access that reads or writes
// before anything else is evaluated (an update, a compound assignment's read,
// a for…in/for…of or destructuring target): it may throw there.
func (j *jsLower) target(t *ts.Node) {
	if j.reference(t) {
		j.throws++
	}
}

// assign lowers `left = right`. made reports that it created the node
// spanning n or, for a destructuring, the element nodes that stand for it; v
// is the variable an identifier target defines, else -1.
func (j *jsLower) assign(n *ts.Node) (made bool, v int32) {
	k := j.k
	left, right := j.strip(n.ChildByFieldId(k.fLeft)), n.ChildByFieldId(k.fRight)
	m := len(j.reads)
	switch left.KindId() {
	case k.identifier:
		j.value(right, false)
		v = j.lookup(left)
		j.def(j.node(flow.Stmt, n, m, len(j.reads)), v)
		return true, v
	case k.objectPattern, k.arrayPattern:
		j.value(right, false)
		saved := j.open()
		j.bind(left, m, len(j.reads))
		return j.close(saved) >= 0, -1
	default:
		// The store happens after the right side is evaluated, so its throw
		// belongs to the write's node, not to a node the right side makes.
		property := j.reference(left)
		j.value(right, false)
		if property {
			j.throws++
		}
		return false, -1
	}
}

// update lowers `x++`, `--x` and their property forms; made and v are as for
// assign.
func (j *jsLower) update(n *ts.Node) (made bool, v int32) {
	k := j.k
	arg := j.strip(n.ChildByFieldId(k.fArgument))
	if arg.KindId() != k.identifier {
		j.target(arg)
		return false, -1
	}
	m := len(j.reads)
	j.ref(arg)
	v = j.lookup(arg)
	j.def(j.node(flow.Stmt, n, m, len(j.reads)), v)
	return true, v
}

// augment lowers a compound or logical assignment; made and v are as for
// assign. A logical one always makes its conditional node spanning n.
func (j *jsLower) augment(n *ts.Node) (made bool, v int32) {
	k := j.k
	left, right := j.strip(n.ChildByFieldId(k.fLeft)), n.ChildByFieldId(k.fRight)
	op := n.ChildByFieldId(k.fOperator).KindId()
	logical := op == k.andAssign || op == k.orAssign || op == k.nullishAssign
	m := len(j.reads)
	v = -1
	property := false
	switch {
	case left.KindId() == k.identifier:
		v = j.lookup(left)
		j.ref(left)
	case logical:
		j.target(left)
	default:
		property = j.reference(left)
	}
	if !logical {
		// As for a plain property write, the throw of the read and the store
		// is counted after the right side, on the node spanning n that the
		// enclosing expression makes, not on a node the right side makes.
		j.value(right, false)
		if left.KindId() != k.identifier {
			if property {
				j.throws++
			}
			return false, -1
		}
		j.def(j.node(flow.Stmt, n, m, len(j.reads)), v)
		return true, v
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
	return true, v
}

// bind lowers a binding or assignment pattern whose incoming value read
// reads[from:to]: one defining node per bound name, in source order, each
// using all of reads[from:to] (see Destructuring in lowerJavaScript).
func (j *jsLower) bind(p *ts.Node, from, to int) {
	k := j.k
	switch p.KindId() {
	case k.identifier, k.shorthandPropertyIdentifierPattern:
		j.def(j.node(flow.Stmt, p, from, to), j.lookup(p))
	case k.memberExpression, k.subscriptExpression:
		m := len(j.reads)
		j.target(p)
		j.uses(j.node(flow.Stmt, p, from, to), m, len(j.reads))
	case k.assignmentPattern:
		j.defaulted(p.ChildByFieldId(k.fLeft), p.ChildByFieldId(k.fRight), p, from, to)
	case k.requiredParameter, k.optionalParameter:
		if v := p.ChildByFieldId(k.fValue); v != nil {
			j.defaulted(j.paramPattern(p), v, p, from, to)
		} else {
			j.bind(j.paramPattern(p), from, to)
		}
	case k.restPattern:
		if c := firstNamed(p); c != nil {
			j.bind(c, from, to)
		}
	case k.objectPattern, k.arrayPattern:
		start, list := j.kids(p)
		for i := range list {
			c := &list[i]
			j.throws++
			switch c.KindId() {
			case k.pairPattern:
				val := c.ChildByFieldId(k.fValue)
				if key := c.ChildByFieldId(k.fKey); key.KindId() == k.computedPropertyName {
					// The element's nodes read the key and, copied after
					// it, the incoming value.
					m := len(j.reads)
					j.value(key, false)
					j.reads = append(j.reads, j.reads[from:to]...)
					j.bind(val, m, len(j.reads))
				} else {
					j.bind(val, from, to)
				}
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

// capFunction collects the enclosing variables a nested callable or a class
// static block references, resolving names through its own parameters and
// scopes first. A computed method name is evaluated where the method is
// created.
func (j *jsLower) capFunction(fn *ts.Node) {
	k := j.k
	id := fn.KindId()
	if id == k.methodDefinition {
		if nm := fn.ChildByFieldId(k.fName); nm.KindId() == k.computedPropertyName {
			j.cap(nm)
		}
	}
	// Parameter decorators are evaluated with the class, outside the method.
	if ps := fn.ChildByFieldId(k.fParameters); ps != nil {
		start, list := j.kids(ps)
		for i := range list {
			if pid := list[i].KindId(); pid == k.requiredParameter || pid == k.optionalParameter {
				j.decorators(&list[i])
			}
		}
		j.done(start)
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
	dec := j.decorated
	j.shadow++
	mark := len(j.binds)
	if nm := n.ChildByFieldId(k.fName); nm != nil {
		j.declare(nm)
	}
	for i := range list {
		switch list[i].KindId() {
		case k.identifier:
		case k.classBody:
			j.children(&list[i], false)
		default:
			j.cap(&list[i])
		}
	}
	j.done(start)
	j.binds = j.binds[:mark]
	j.shadow--
	if j.shadow == 0 && j.decorated > dec {
		j.throws++
	}
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
	case k.requiredParameter, k.optionalParameter:
		j.patternExprs(j.paramPattern(p))
		if v := p.ChildByFieldId(k.fValue); v != nil {
			j.cap(v)
		}
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
	if j.l.isCallable(n) {
		j.capFunction(n)
		return
	}
	if j.erased(n) {
		return
	}
	switch id {
	case k.identifier, k.shorthandPropertyIdentifier, k.shorthandPropertyIdentifierPattern:
		j.ref(n)
	case k.decorator:
		j.decorated++
		j.children(n, false)
	case k.enumDeclaration:
		j.enumMembers(n, false)
	case k.internalModule, k.module:
		if body := n.ChildByFieldId(k.fBody); body != nil {
			mark, savedFn := len(j.binds), j.fnMark
			j.fnMark = mark
			j.hoistVars(body)
			j.cap(body)
			j.binds, j.fnMark = j.binds[:mark], savedFn
		}
	case k.assignmentExpression, k.augmentedAssignmentExpression:
		j.capWrite(n.ChildByFieldId(k.fLeft))
		j.children(n, false)
	case k.updateExpression:
		j.capWrite(n.ChildByFieldId(k.fArgument))
		j.children(n, false)
	case k.classDeclaration, k.abstractClassDeclaration, k.class:
		j.capClass(n)
	case k.classStaticBlock:
		j.capFunction(n)
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
		left := j.strip(n.ChildByFieldId(k.fLeft))
		if kw := n.ChildByFieldId(k.fKind); kw == nil {
			j.capWrite(left)
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

// capWrite records, as writes, the enclosing variables an assignment target
// inside a nested callable or class binds: an identifier, or every name of a
// destructuring target. A property target writes no variable.
func (j *jsLower) capWrite(t *ts.Node) {
	k := j.k
	switch t = j.strip(t); t.KindId() {
	case k.identifier, k.shorthandPropertyIdentifierPattern:
		if v := j.lookup(t); v >= 0 {
			j.writes = append(j.writes, v)
		}
	case k.assignmentPattern, k.objectAssignmentPattern:
		j.capWrite(t.ChildByFieldId(k.fLeft))
	case k.pairPattern:
		j.capWrite(t.ChildByFieldId(k.fValue))
	case k.objectPattern, k.arrayPattern, k.restPattern:
		start, list := j.kids(t)
		for i := range list {
			j.capWrite(&list[i])
		}
		j.done(start)
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
// resolved once per grammar by name so the walk compares integers rather
// than converting every node's kind to a string. A kind only some of the
// grammars define (a JSX kind, a TypeScript kind) is optional: in a grammar
// without it its id is 0, the end-of-input symbol, which no named node ever
// has, so a comparison against it never matches and the construct simply
// cannot occur. Every other kind is one every grammar must define.
type jsSyntax struct {
	program, statementBlock, expressionStatement, lexicalDeclaration, variableDeclaration, usingDeclaration,
	variableDeclarator, functionDeclaration, generatorFunctionDeclaration, classDeclaration, class, classHeritage,
	ifStatement, elseClause, forStatement, forInStatement, whileStatement, doStatement, switchStatement,
	switchBody, switchCase, switchDefault, tryStatement, catchClause, finallyClause, labeledStatement,
	breakStatement, continueStatement, returnStatement, throwStatement, emptyStatement, debuggerStatement,
	withStatement, importStatement, exportStatement, hashBangLine, importClause,
	identifier, shorthandPropertyIdentifier, shorthandPropertyIdentifierPattern, parenthesizedExpression,
	sequenceExpression, assignmentExpression, augmentedAssignmentExpression, binaryExpression, ternaryExpression,
	updateExpression, callExpression, newExpression, awaitExpression, yieldExpression, memberExpression,
	subscriptExpression, spreadElement, functionExpression, generatorFunction, methodDefinition,
	classBody, fieldDefinition, classStaticBlock, trueLit, objectPattern, arrayPattern, assignmentPattern, objectAssignmentPattern,
	pairPattern, restPattern, computedPropertyName, jsxOpeningElement, jsxSelfClosingElement,
	jsxClosingElement uint16

	// The TypeScript kinds, optional.
	requiredParameter, optionalParameter, publicFieldDefinition, abstractClassDeclaration, enumDeclaration,
	enumAssignment, internalModule, module, nestedIdentifier, importRequireClause, importAlias, nonNullExpression,
	asExpression, satisfiesExpression, typeAssertion, instantiationExpression, typeArguments, decorator uint16
	// erased is indexed by kind id: the kinds the TypeScript compiler erases.
	erased []bool
	// imports is the import-clause table the extraction shares.
	imports importSyntax

	and, or, nullish, andAssign, orAssign, nullishAssign, varKw, ofKw, constKw, eqTok, typeKw uint16

	fAlternative, fArgument, fArguments, fBody, fCondition, fConsequence, fDeclaration, fFinalizer,
	fFunction, fHandler, fIncrement, fIndex, fInitializer, fKey, fKind, fLabel, fLeft, fName, fObject,
	fOperator, fOptionalChain, fParameter, fParameters, fPattern, fRight, fValue uint16
}

// jsErased are the kinds the TypeScript compiler erases (see TypeScript in
// lowerJavaScript), absent from the JavaScript grammar.
var jsErased = []string{"type_annotation", "type_arguments", "type_parameters", "opting_type_annotation",
	"omitting_type_annotation", "adding_type_annotation", "asserts_annotation", "type_predicate_annotation",
	"implements_clause", "interface_declaration", "type_alias_declaration", "ambient_declaration",
	"function_signature", "method_signature", "abstract_method_signature", "index_signature"}

// resolveJSSyntax resolves language's table. A required name the grammar
// does not define is a lowering defect and panics, so a misspelt kind can
// never silently match nothing.
func resolveJSSyntax(language string) *jsSyntax {
	tl := mustGrammar(language)
	kind := func(name string) uint16 { return mustKind(tl, language, name, true) }
	opt := func(name string) uint16 { return tl.IdForNodeKind(name, true) }
	tok := func(name string) uint16 { return mustKind(tl, language, name, false) }
	field := func(name string) uint16 { return mustField(tl, language, name) }
	s := &jsSyntax{}
	s.program, s.statementBlock, s.expressionStatement = kind("program"), kind("statement_block"), kind("expression_statement")
	s.lexicalDeclaration, s.variableDeclaration, s.usingDeclaration = kind("lexical_declaration"), kind("variable_declaration"), opt("using_declaration")
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
	s.hashBangLine, s.importClause, s.imports = kind("hash_bang_line"), kind("import_clause"), resolveImportSyntax(tl, language)
	s.identifier, s.shorthandPropertyIdentifier = kind("identifier"), kind("shorthand_property_identifier")
	s.shorthandPropertyIdentifierPattern = kind("shorthand_property_identifier_pattern")
	s.parenthesizedExpression, s.sequenceExpression = kind("parenthesized_expression"), kind("sequence_expression")
	s.assignmentExpression, s.augmentedAssignmentExpression = kind("assignment_expression"), kind("augmented_assignment_expression")
	s.binaryExpression, s.ternaryExpression, s.updateExpression = kind("binary_expression"), kind("ternary_expression"), kind("update_expression")
	s.callExpression, s.newExpression, s.awaitExpression = kind("call_expression"), kind("new_expression"), kind("await_expression")
	s.yieldExpression, s.memberExpression, s.subscriptExpression = kind("yield_expression"), kind("member_expression"), kind("subscript_expression")
	s.spreadElement, s.functionExpression, s.generatorFunction = kind("spread_element"), kind("function_expression"), kind("generator_function")
	s.methodDefinition, s.classStaticBlock, s.trueLit = kind("method_definition"), kind("class_static_block"), kind("true")
	s.classBody, s.fieldDefinition = kind("class_body"), opt("field_definition")
	s.objectPattern, s.arrayPattern, s.assignmentPattern = kind("object_pattern"), kind("array_pattern"), kind("assignment_pattern")
	s.objectAssignmentPattern, s.pairPattern, s.restPattern = kind("object_assignment_pattern"), kind("pair_pattern"), kind("rest_pattern")
	s.computedPropertyName, s.jsxOpeningElement = kind("computed_property_name"), opt("jsx_opening_element")
	s.jsxSelfClosingElement, s.jsxClosingElement = opt("jsx_self_closing_element"), opt("jsx_closing_element")
	s.and, s.or, s.nullish = tok("&&"), tok("||"), tok("??")
	s.andAssign, s.orAssign, s.nullishAssign = tok("&&="), tok("||="), tok("??=")
	s.varKw, s.ofKw = tok("var"), tok("of")
	s.requiredParameter, s.optionalParameter = opt("required_parameter"), opt("optional_parameter")
	s.publicFieldDefinition, s.abstractClassDeclaration = opt("public_field_definition"), opt("abstract_class_declaration")
	s.enumDeclaration, s.enumAssignment = opt("enum_declaration"), opt("enum_assignment")
	s.internalModule, s.module, s.nestedIdentifier = opt("internal_module"), opt("module"), opt("nested_identifier")
	s.importRequireClause, s.importAlias = opt("import_require_clause"), opt("import_alias")
	s.nonNullExpression, s.asExpression = opt("non_null_expression"), opt("as_expression")
	s.satisfiesExpression, s.typeAssertion = opt("satisfies_expression"), opt("type_assertion")
	s.instantiationExpression, s.typeArguments, s.decorator = opt("instantiation_expression"), opt("type_arguments"), kind("decorator")
	s.erased = make([]bool, tl.NodeKindCount())
	for _, name := range jsErased {
		if id := opt(name); id != 0 {
			s.erased[id] = true
		}
	}
	s.constKw, s.eqTok, s.typeKw = tok("const"), tok("="), tl.IdForNodeKind("type", false)
	s.fPattern = tl.FieldIdForName("pattern")
	s.fAlternative, s.fArgument, s.fArguments = field("alternative"), field("argument"), field("arguments")
	s.fBody, s.fCondition, s.fConsequence, s.fDeclaration = field("body"), field("condition"), field("consequence"), field("declaration")
	s.fFinalizer, s.fFunction, s.fHandler, s.fIncrement = field("finalizer"), field("function"), field("handler"), field("increment")
	s.fIndex, s.fInitializer, s.fKey, s.fKind, s.fLabel = field("index"), field("initializer"), field("key"), field("kind"), field("label")
	s.fLeft, s.fName, s.fObject, s.fOperator = field("left"), field("name"), field("object"), field("operator")
	s.fOptionalChain, s.fParameter, s.fParameters = field("optional_chain"), field("parameter"), field("parameters")
	s.fRight, s.fValue = field("right"), field("value")
	return s
}
