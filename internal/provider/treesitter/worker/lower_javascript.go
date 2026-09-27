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
func (g *jsGrammar) lower(l *Lowering, b *flow.Builder, fn *ts.Node, src []byte, s *Scratch) {
	g.once.Do(func() { g.s = resolveJSSyntax(g.language) })
	lowerJavaScript(l, b, fn, src, s, g.s)
}

// lowerJavaScript lowers one JavaScript callable's parameters and body into b.
//
// # Node granularity
//
// The goldens render nodes by source text, so the granularity is exact:
//
//   - Every parameter bound name is one defining node after Entry, spanning
//     its identifier, in parameter order and before any node of the body,
//     the hoisted nodes of the body's function declarations included; in the
//     program, every binding of an ES import statement is one, spanning the
//     local name, before the program's hoisted nodes. A TypeScript import-equals
//     declaration has no such node: it is one node where it stands (see
//     TypeScript below).
//   - A statement is one node: an expression statement spans its expression
//     (a comma sequence at statement level is one statement per element), a
//     declarator whose name is an identifier spans the declarator (a
//     destructuring declarator makes no node of its own: its initializer's
//     node and one node per bound name, by the destructuring rule below),
//     return, throw, break and continue span the grammar's statement node,
//     its `;` included (`return c;`), with kind Jump, and a class declaration
//     spans the declaration, its leading decorators included (`@d class A
//     {}`; the decorators written before `export` belong to the export
//     statement and lie outside it), creates the class and defines its name. A break or continue whose
//     label names no enclosing statement (an early error, ECMA-262 §8.3.2
//     ContainsUndefinedBreakTarget, §8.3.3) is lowered as the tree stands:
//     an unresolved jump, counted, with no successor. An expression
//     statement that is an assignment, compound assignment or update is its
//     assignment's node (or, for a destructuring, the right side's node and
//     the element nodes), not a second node. `var x;` is a hoisted
//     declaration and no node; `let x;` defines x.
//   - A function declaration is hoisted: one node at the start of its block,
//     after the parameter nodes when the block is the function's body and
//     before every other node of the block, spanning its name, defines the
//     name. At the declaration's own position
//     a Stmt node spanning the declaration creates the function (see nested
//     callables below) and may-defines the name, so a use after the
//     declaration sees the captures read there.
//   - A condition is one Branch node spanning the condition as Spans in
//     Lowering states: if, while, do…while, for. A switch is a Stmt node for
//     the discriminant, spanned the same way (`switch (x)` spans x), which
//     evaluates it once and defines an owned variable with its value, then
//     one Branch node per case test, spanned the same way, in source order
//     (each test is evaluated only when the previous failed), that Uses that
//     variable and its own reads, since it compares the two (ECMA-262
//     §14.12.3 CaseClauseIsSelected); a case body's end flows into the next
//     body when it does not break. `default` makes no node: the last case
//     test's false edge enters the default's body wherever the default
//     stands, and with no default it goes to the switch's exit (§14.12.2
//     CaseBlockEvaluation). A loop's back edge enters the first node of its
//     head: the first node a while's or for's condition's lowering makes
//     (an embedded assignment's node, a short-circuit's deciding Branch),
//     not necessarily the loop's Branch; a do…while's body's first node (its
//     condition's when the body makes none), a continue there entering the
//     condition's first node; and a for loop's continue enters its update
//     (its condition's first node when it has none). A for…in, for…of or
//     for await…of loop
//     follows the iteration model (see Iteration in Lowering; ECMA-262
//     §14.7.5.6 ForIn/OfHeadEvaluation): a Stmt node spanning the iterated
//     expression Uses its reads and defines the iteration variable, then a
//     Branch head spanning the head clause from the left side to the end of
//     the iterated expression (whether another element is assigned) Uses
//     only that variable and defines nothing; the left side is the
//     grammar's left field, the bound pattern or target, so `const`, `let`,
//     `var` and `await` lie before the span (`for (const x of xs)`'s head
//     is `x of xs`). On the body path, after the
//     head, the left side is assigned (§14.7.5.7 ForIn/OfBodyEvaluation):
//     each name it binds is defined by the destructuring rule below (a bare
//     identifier is one defining node spanning it), and a property target
//     `o.p` is evaluated anew on every iteration, one Stmt node spanning it
//     that Uses o and may-defines it; each of these nodes Uses the iteration
//     variable, never the iterated expression's names. The exit edge carries
//     the definitions from before the loop.
//   - `&&`, `||`, `??` and the conditional operator: the deciding operand is
//     a Branch node spanning it, created after the nodes of everything it
//     evaluates; each conditionally evaluated operand is a Stmt node spanning
//     it. A logical assignment `x ??= y` is a Branch node spanning x, then a
//     Stmt node spanning the whole expression that conditionally defines x.
//     An optional chain is a Branch node spanning the receiver before each
//     `?.`, and one Stmt node spanning the whole chain on the non-nullish
//     path. Every operand spans its expression as Spans in Lowering states.
//   - An assignment, compound assignment or update, wherever it stands, is
//     one Stmt node spanning it that Uses its right side's value and defines
//     its target: an identifier target is killed, and a property target
//     (`o.p = v`, `a[i] += v`, `o.p++`) may-defines its base variable (o, a;
//     see Lowering), as every write through a field or element is. A property
//     target's object and index are read by the write node itself, their
//     reads staying on it when the right side makes nodes of its own (`o.p =
//     c ? 1 : 2`: the write Uses o, the condition's Branch only c). A compound
//     property assignment is first a Stmt node spanning the target, the
//     property read, whose result carries the reference and the old value to
//     the write node; the write node reads o only as its may-definition's
//     prior version, so it pairs with o by the May-definitions rule.
//   - A delete of a property (`delete o.p`, `delete o[k]`, `delete o?.p`)
//     is folded into the node that evaluates it, as a unary operator is, and
//     that node may-defines the property's base variable (o), as every write
//     through a property does: the delete removes a property of the object o
//     holds (ECMA-262 §13.5.1.2). The node is the one whose reads include the
//     operand's, recorded by position, so a node an operand evaluated later
//     makes never takes it. Through an optional chain (`delete o?.[k]`),
//     whose receiver and chain are nodes of their own, it is still the node
//     evaluating the delete (the statement's, `delete o?.[k]`), which the
//     nullish and the non-nullish paths both reach, never the receiver's
//     Branch or the chain's node. Inside a nested callable the delete is one of its
//     writes, which its creating node may-defines. `delete x` of a name
//     writes nothing: it is sloppy code only and removes no variable of the
//     function.
//   - Destructuring is one defining node per bound name, spanning the name
//     alone, with the TypeScript wrappers around it stripped (`[g!] = x`
//     makes g's node spanning g), so D ≤ N. The value destructured is evaluated once, at its own node:
//     a declarator's initializer or an assignment's right side is a Stmt node
//     spanning it (unless its own lowering made one) that defines an owned
//     variable, the incoming value every element Uses; a for…in/for…of left
//     side's incoming value is the iteration variable, and a parameter's (the
//     argument) or a catch parameter's (the thrown value) reads none. A
//     default (`a = e` in a pattern or parameter) is a Branch node spanning
//     the element (for a property element `k: a = e`, its value `a = e`, the
//     key outside it) that defines the name from the incoming value and Uses
//     only that value, then a Stmt node spanning e that defines it from the
//     default and Uses only e's reads, since e is evaluated after the test;
//     for a nested pattern the two define an owned variable instead, the
//     nested elements' incoming value. A nested pattern without a default,
//     and a computed key, are folded into the element nodes under them: each
//     Uses the enclosing incoming value and the key's reads. A catch
//     parameter is bound by this rule on the handler path, after the
//     Handler. Elements are lowered in source order: the language assigns
//     them one at a time, each step may throw, and a default or computed key
//     reads the elements assigned before it; since every element reads the
//     value evaluated once, a swap `[x, y] = [y, x]` reads the values from
//     before the statement.
//   - A nested callable or class is its own function; in the enclosing
//     function its creating expression is one Stmt node spanning it, which
//     Uses every enclosing variable read inside it (its own evaluation),
//     resolved with its own scopes so a name it declares shadows, and
//     may-defines every enclosing variable assigned inside it and the base
//     variable of every property it writes (a χ, see May-definitions in
//     Lowering); a plain assignment or binding target is written and not
//     read, while a compound assignment or an update also reads its
//     variable: when the closure runs is unknown, so a use after its
//     creation pairs with the creation and, through it, with the killing
//     definitions reaching the creation, and the creating node pairs with
//     those definitions too. A class's field initializers and static
//     blocks, static and instance alike, are its class body's unit, lowered
//     in source order: each initializer is the nodes of its value then one Stmt node
//     spanning the field definition, which ends before its `;` (the grammar
//     makes the `;` a sibling in the class body) and Uses the value's reads
//     but defines no variable (it writes a property of the instance or the
//     class, which no variable holds), and each static block is a
//     block with its own var scope. The class node in the enclosing function
//     also captures them.
//   - An expression lowered for its value and ended by a node spanning it (an
//     expression statement, a conditional operand, an arrow's expression
//     body, a default export, a destructured value) takes no second node when
//     the last node its own lowering made already spans it, as an optional
//     chain, a nested callable or class, or an assignment does.
//   - A statement that stands alone as the body of an if, else, loop, with
//     or label is scoped to an implicit block of its own, as a braced body
//     is: a function declaration there is hoisted within it and binds its
//     name there, never an enclosing variable of the same name. This is the
//     strict rule (see Scoping) and holds in sloppy code too: the second,
//     function-scoped `var` binding ECMA-262 Annex B.3.2.1 gives a block's
//     function declaration in sloppy code, when no early error or enclosing
//     lexical name of the same name forbids it, is given up, since whether
//     code is sloppy depends on the file being a script rather than a module
//     and on directives the lowering does not track. So after the block the
//     name is the enclosing variable of that name, or resolves to no
//     variable when there is none, and never pairs with the declaration.
//   - A with statement (ECMA-262 §14.11) is a Stmt node spanning its object,
//     which is evaluated and read, then its body. Given up: that a name in
//     the body resolves to one of the object's properties, not to the
//     variable it names; it depends on the object's shape at run time, and
//     `with` is excluded from strict code (§14.11.1), so modules and classes
//     never contain it. A body name is the variable it names.
//   - A default export of an expression is the node of that expression,
//     spanning it and not the export statement (`export default x + 1;`
//     makes x + 1).
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
// Use. A name that resolves to none (see Names that resolve to no variable
// in Lowering: a global or other free name, a name declared only inside a
// nested callable, an enum member's name inside an initializer, a name only
// a TypeScript `declare` form introduces) is filtered where it enters, by
// the lookup's -1: read drops it, so reads never holds it, and def drops
// it, so as an assignment's, compound or logical assignment's, update's,
// destructuring element's, default's or for…in/for…of left side's target
// it defines nothing while the node is still made with its other reads;
// mayDefBase drops it as a property write's base, and capTarget as a
// nested callable's write, so the creating node may-defines nothing for
// it. Every other flow.Builder.Def takes an owned variable or one def
// admitted.
//
// Values travel through variables (see Uses in Lowering): a node Uses what
// its own evaluation reads, the reads no node of their own carries, and the
// result variables of the nested constructs whose values it consumes.
//
// Lowered to nodes, each handing its value on through an owned result
// variable that the consumer Uses:
//
//   - a conditional: each arm's node defines the result; the condition is
//     read by its Branch alone;
//   - `&&`, `||`, `??`, wherever it stands (a condition included): the
//     deciding operand's Branch defines the result, which is its value on
//     the path where it decides, and the other operand's node defines it on
//     the other path;
//   - an optional chain: the Branch before each `?.` defines the result
//     (undefined on its nullish exit) and hands the receiver on through it,
//     and the chain's node, which Uses it, defines it again; that node also
//     Uses the results of the constructs lowered after the `?.` whose
//     values it consumes, as every consumer does (`g?.(x = 1)` Uses the
//     result of x = 1's node);
//   - a logical assignment: its Branch and its write node, as for `??`;
//   - an assignment, compound assignment or update: its node defines the
//     result, the value assigned, a killing definition beside the
//     identifier target's own, so two assignments to one variable in one
//     expression hand on two values;
//   - a destructuring assignment: the right side's node, whose result is
//     the assignment's value; it is one variable, the incoming value every
//     element Uses;
//   - a nested callable's or class's creation: the creating node, whose
//     result is the created value; its captures are its own evaluation and
//     the consumer never repeats them.
//
// Where a construct's yielding node is one its own lowering already made
// and that node already defines a variable (an assignment, a creation
// standing for a conditional's arm), the node Defs the result too: a node
// may make several killing definitions, and only the construct's own
// yielding nodes define its result.
//
// Folded into the node that evaluates them, their reads being its own
// evaluation: identifiers, literals and templates, plain unary and binary
// operators, a comma sequence in value position, property access and calls
// outside an optional chain, `new`, `await`, `yield`, spread, array and
// object literals, JSX elements, and the TypeScript wrappers that evaluate
// to their operand.
//
// A read the consumer folds that the statement makes before a node
// redefines its variable (`f(x, x = 1)`) is carried by that node, as Uses in
// Lowering states: the node Uses v and defines an owned variable that
// replaces the held reads of v. The hand-off is made only for reads the
// node runs after whenever their consumer does: a node inside an operand
// evaluated on some paths only (a short-circuit operand after the deciding
// one, a conditional's arm, an optional chain's tail after a `?.`, a
// logical assignment's right side and write, a default) takes over only the
// reads made inside that operand, and a read before it stays on the
// consumer, where `y = x + (c && (x = 1))` pairs y with the x before it
// and, through the operator's result, with the assignment. JavaScript evaluates operands left to right
// (ECMA-262 §13.3.8.1 ArgumentListEvaluation, §13.15.4), so source order is
// the language's. A read another node carried (a condition's, a capture) is
// no longer held.
//
// # Exceptions
//
// MayThrow is given to every node whose own evaluation — the part of the
// source evaluated since the previous node — contains a call, `new`,
// `await`, `yield`, a spread, a property read or write, a destructuring
// element, a for…of iterator step (including for await) on the loop's head,
// the iterator's creation on a for…of's iterated expression's node (GetIterator
// calls the value's iterator method, ECMA-262 §7.4.3, and throws when there is
// none), or the creation of
// a class that evaluates an `extends` expression, a computed key, a static
// field initializer, a static block or a decorator (ECMA-262 §15.7.14); a
// throw statement's node is also a Throw. A property write throws when the
// value is stored, after its right side, so its throw is on the write's
// node. A compound property assignment reads the property before its right
// side is evaluated (§13.15.2), so that read throws on its own node before
// the right side's nodes, and the store throws on the write's node after
// them. The Builder applies MayThrow only inside an open catch or finally
// frame, where the throwing node's own definitions do not reach the handler.
// A catch clause has a Handler node only when some node reached it by
// MayThrow; a throw statement's node enters the clause's first node
// directly. With neither, the clause's nodes are still made, with no
// predecessor.
// A for…in's iterated expression and head are no throw points: the keys are
// enumerated without a call of the program's own code, a proxy's traps aside,
// which are given up as an operator's ToPrimitive is. A per-iteration binding
// of a bare identifier is none either: it initializes or assigns a variable,
// and only a destructuring element reads a property. An enum's and a
// namespace's own node is none: the emitted call invokes the function the
// emit has just created, which is always callable, and the code its body runs
// is the member initializers' and the body's own nodes.
// An operator is not counted, arithmetic and comparison alike, although
// either can throw through ToPrimitive (§7.1.1) when an operand is an
// object whose valueOf or toString throws, and `in` and `instanceof` throw
// on a non-object: counting them would make almost every node a throw
// point, and the calls they make are those of the operand's own methods.
//
// # Scoping
//
// `var` and a function's parameters are function-scoped and hoisted, as are
// import bindings in the program; `let`, `const`, `using`, `class`, a
// TypeScript enum or namespace, and a function declaration in a block are
// block-scoped and bound from the block's
// start (the strict-mode rule, which modules and classes impose); a catch
// parameter is scoped to its clause and a `for (let …)` binding to its loop;
// a switch body is one block. Parameter defaults see the parameters only.
// The typescript and tsx grammars parse a using declaration (ES2026, TS 5.2)
// as an assignment carrying `using`, `using a = e, b = f` as a comma sequence
// whose first element is one: each such statement is that declaration, its
// names bound from the block's start and each declarator's node spanning it
// from its name, as the javascript grammar's using declaration is. The
// disposal a using declaration schedules for the block's exit (a call of the
// value's dispose method, which may throw) makes no node and is no throw
// point: the call is the value's own method, given up as an operator's
// ToPrimitive is (see Exceptions).
//
// # TypeScript
//
// Syntax the compiler erases, emitting no code for it, produces no node and
// binds no variable: type annotations, type parameters and arguments,
// interfaces, type aliases, `declare` declarations, overload and method
// signatures without a body, abstract members, index signatures,
// `implements` clauses, `import type`, a `type` import specifier, and a
// `const enum` (its uses are replaced by the members' values); an erased
// subtree is skipped wherever it stands, so a `typeof x` inside a type reads
// nothing. `e!`, `e as T`, `e satisfies T`, `<T>e` and `f<T>` evaluate to
// their operand and lower as it (the capture collection of a nested
// callable or class, too, walks only the operand, never the type): a node
// the enclosing construct
// makes for the whole expression (a condition's Branch, a statement's node)
// spans the expression as written, wrapper included, a destructuring
// element's node, which spans the bound name, excepted, and a node the
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
// class. The compiler emits an enum and a namespace as a variable declared
// where the declaration stands, a `var` at a file's top level and a `let`
// everywhere else, then an assignment to it, so the name is block-scoped
// and bound from its block's start (at a file's top level the program's
// block is its function scope, so the two agree); a later enum or namespace
// of a name its block already binds (an enum or namespace before it, or the
// class or function a namespace merges with) declares no second variable and
// assigns the same one. `import x = …` is emitted as a `var`, so its name is
// function-scoped and hoisted like var; no pair can show the hoisting, since
// a var of the same name before it is the same variable either way and a use
// before it meets no definition, so that rule has no golden case:
//
//   - An enum is one Stmt node spanning its name that Uses and defines the
//     name (the emitted `E || (E = {})`), then one Stmt node per member
//     initializer, spanning the member, in order, which defines no variable
//     (the emitted body writes a property of the invoked function's own
//     parameter); every member's bare name
//     is bound to no variable throughout the body, since inside an
//     initializer it names the member (the emitted body assigns each member
//     as a property of the enum object and reads a member name as that
//     property). A member's node Uses its initializer's reads only, never
//     E: the emitted body reaches the enum object through the invoked
//     function's own parameter E, not the enclosing variable. A member
//     without an initializer makes no node: its value is a constant the
//     compiler computes, and no source expression of it is evaluated.
//   - A namespace with a body runs its body once, immediately, where it
//     stands (the emitted immediately invoked function), so it is lowered
//     inline, not as a callable: one Stmt node spanning the leftmost name
//     of its path that Uses and defines that name, then the body as a block
//     with its own var scope. A namespace the grammar places in an
//     expression statement is that namespace, with no node of its own for
//     the statement. A namespace named by a string is ambient: only a
//     declaration file writes one without `declare`, and nothing in it runs.
//   - `import x = require(m)` is one Stmt node spanning x, where it stands
//     (in the program too, never also an import binding after Entry), that
//     defines it and may throw; `import x = A.B` is one spanning x that
//     Uses A and defines x, and may throw when it reads a property.
//   - `export = e` evaluates e like a default export.
func lowerJavaScript(l *Lowering, b *flow.Builder, fn *ts.Node, src []byte, s *Scratch, k *jsSyntax) {
	// The state is s.js, reset in place: every list keeps its capacity, and
	// the lists holding nodes or views of the previous function's source are
	// cleared first, so nothing of it outlives its function. seen is
	// truncated too, so the markers the previous function left are zeroed
	// as it grows again and stmtNo can start over.
	j := &s.js
	clear(j.buf[:cap(j.buf)])
	clear(j.lab[:cap(j.lab)])
	*j = jsLower{l: l, b: b, src: src, k: k, cur: s.cursor(fn), buf: j.buf[:0], binds: &s.scope,
		reads: j.reads[:0], seen: j.seen[:0], stmtNo: 1, writes: j.writes[:0], first: -1, last: -1,
		opt: j.opt[:0], lab: j.lab[:0], match: j.match[:0], optR: -1, may: j.may[:0]}
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
			mark := j.binds.mark()
			j.fnMark = mark
			body := m.ChildByFieldId(k.fBody)
			j.hoistVars(body)
			j.block(body)
			j.binds.truncate(mark)
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
	binds  *scope
	fnMark int
	// shadow is non-zero while walking a nested callable or class for its
	// captures: declarations then bind -1 and no node is created.
	shadow int
	// reads are the variables read by the current statement, in evaluation
	// order; seen[v].stmt == stmtNo marks v as one of them, first recorded
	// at reads[seen[v].at], stmtNo numbering the statements.
	reads  []int32
	seen   []jsSeen
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
	// may are the pending may-definitions a delete of a property makes of
	// its base variable, each with the position in reads where the delete's
	// operand began: the node whose reads include it, the one evaluating the
	// delete, takes it (see nodeAt).
	may []jsMay
	// last is the node created last, or -1, and lastSpan its span.
	last     int32
	lastSpan flow.Span
	// opt holds the fringes saved at each `?.` of the optional chains being
	// lowered, innermost chain last.
	opt []flow.Fringe
	// labels are the statement labels the next statement takes, the top of
	// lab, the stack of the labels of the labelled statements being lowered.
	labels, lab []string
	// match holds the fringes of the case tests of the switches being
	// lowered.
	match []flow.Fringe
	// optR is the result variable of the optional chain being lowered, or -1
	// before its first `?.`.
	optR int32
	// condFrom is where in reads the innermost conditionally evaluated
	// operand of the statement began, or 0.
	condFrom int
	// decorated counts the decorators collected that are evaluated where the
	// class being collected is created: a class whose collection counted one
	// may throw there. A nested callable's parameters and body and a field's
	// initializer run later, so collecting them leaves it unchanged.
	decorated int
}

// kids pushes n's named, non-extra children (comments are extras) onto buf
// and returns the stack mark and the list; done(mark) pops them. A list stays
// valid across nested kids calls: later pushes never overwrite it. An ERROR
// node the parser made an extra, which its recovery does when it wraps what
// it could not parse or a token it skipped, is kept: it is lowered as a kind
// the lowering does not name is.
func (j *jsLower) kids(n *ts.Node) (int, []ts.Node) {
	start := len(j.buf)
	c := j.cur
	c.Reset(*n)
	if c.GotoFirstChild() {
		for {
			if x := c.Node(); x.IsNamed() && (!x.IsExtra() || x.IsError()) {
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
	j.binds.push(j.text(name), v)
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
	j.mark(v, len(j.reads)-1)
}

// jsSeen marks a variable as read by statement stmt, first at reads[at].
type jsSeen struct {
	stmt, at int
}

// jsMay is a pending may-definition of v made by an operand whose reads begin
// at reads[at].
type jsMay struct {
	at int
	v  int32
}

// reset starts a statement: nothing is read and no throw or may-definition
// is pending.
func (j *jsLower) reset() {
	j.reads, j.throws, j.thrown, j.condFrom = j.reads[:0], 0, 0, 0
	j.may = j.may[:0]
	j.stmtNo++
}

// drop removes reads[m:], the reads a node just made carries: no later node
// Uses them. A variable read only there is no longer one of the statement's
// reads, so a later definition does not take it (see def).
func (j *jsLower) drop(m int) {
	for _, v := range j.reads[m:] {
		if j.seen[v].at >= m {
			j.seen[v].stmt = 0
		}
	}
	j.reads = j.reads[:m]
}

// yieldTo makes node id, which carries reads[m:], yield the value of the
// expression being lowered into the owned result variable r: id defines r and
// its reads are dropped. The consumer reads r once every yielding node is
// made.
func (j *jsLower) yieldTo(id int32, m int, r int32) {
	j.b.Def(id, r)
	j.drop(m)
}

// give is yieldTo into a new result variable, which the consumer reads in
// place of reads[m:].
func (j *jsLower) give(id int32, m int) {
	r := j.b.Var()
	j.yieldTo(id, m, r)
	j.read(r)
}

// node creates a node spanning n that Uses reads[from:to], may-defines what
// the pending may-definitions made within those reads name, and MayThrow when
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
	kept := 0
	for _, m := range j.may {
		if from <= m.at && m.at < to {
			j.b.MayDef(id, m.v)
			continue
		}
		j.may[kept] = m
		kept++
	}
	j.may = j.may[:kept]
	if j.throws > j.thrown {
		j.b.MayThrow(id)
	}
	j.thrown = j.throws
	j.last, j.lastSpan = id, s
	return id
}

// pending reports whether a may-definition made at reads[from] or later
// waits for a node.
func (j *jsLower) pending(from int) bool {
	for _, m := range j.may {
		if m.at >= from {
			return true
		}
	}
	return false
}

// uses records reads[from:to] as Uses of node id.
func (j *jsLower) uses(id int32, from, to int) {
	for _, v := range j.reads[from:to] {
		j.b.Use(id, v)
	}
}

// def records that node n defines v. When the statement still holds a read
// of v from before n, made where n runs whenever its consumer does, n reads
// that earlier value before overwriting it and hands it on through an owned
// variable, which replaces those reads (see Uses in lowerJavaScript).
func (j *jsLower) def(n, v int32) {
	if v < 0 {
		return
	}
	j.b.Def(n, v)
	if int(v) >= len(j.seen) || j.seen[v].stmt != j.stmtNo {
		return
	}
	// Only the reads made since the innermost conditionally evaluated
	// operand began are handed off: n runs whenever their consumer does.
	at := max(j.seen[v].at, j.condFrom)
	for at < len(j.reads) && j.reads[at] != v {
		at++
	}
	if at == len(j.reads) {
		return
	}
	j.b.Use(n, v)
	t := j.b.Var()
	j.b.Def(n, t)
	for i := at; i < len(j.reads); i++ {
		if j.reads[i] == v {
			j.reads[i] = t
		}
	}
	if j.seen[v].at >= j.condFrom {
		j.seen[v].stmt = 0
	}
	j.mark(t, at)
}

// enter begins an operand evaluated only on some paths through the
// expression (a short-circuit operand after the deciding one, a
// conditional's arm, an optional chain's tail, a logical assignment's right
// side, a default): a definition inside it takes over no read made before
// it. It returns the enclosing start, which the caller restores.
func (j *jsLower) enter() int {
	saved := j.condFrom
	j.condFrom = len(j.reads)
	return saved
}

// mark records that v is one of the statement's reads, first at reads[at].
func (j *jsLower) mark(v int32, at int) {
	if int(v) >= len(j.seen) {
		j.seen = append(j.seen, make([]jsSeen, int(v)+1-len(j.seen))...)
	}
	if j.seen[v].stmt != j.stmtNo {
		j.seen[v] = jsSeen{stmt: j.stmtNo, at: at}
	}
}

// valueNode lowers n for its value and ends it with a Stmt node spanning n,
// unless the last node that lowering made already spans n, or n without its
// parentheses, with no throw evaluated after it: that node then stands for
// n. It returns the node standing for n and the start of the reads that
// carry n's value, which that node Uses (a node that stood for n left its
// result there).
func (j *jsLower) valueNode(n *ts.Node) (int32, int) {
	m, last := len(j.reads), j.last
	j.value(n, false)
	if j.last != last && j.lastSpan == spanOf(j.strip(n)) && j.thrown == j.throws && !j.pending(m) {
		return j.last, m
	}
	return j.node(flow.Stmt, j.l.unparen(n), m, len(j.reads)), m
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
	mark := j.binds.mark()
	start, list := j.kids(body)
	for i := range list {
		member := &list[i]
		if member.KindId() == k.enumAssignment {
			member = member.ChildByFieldId(k.fName)
		}
		j.binds.push(j.text(member), -1)
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
	j.binds.truncate(mark)
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
	mark, savedFn := j.binds.mark(), j.fnMark
	j.fnMark = mark
	j.hoistVars(body)
	j.block(body)
	j.binds.truncate(mark)
	j.fnMark = savedFn
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
	j.drop(m)
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
	mark := j.binds.mark()
	start, list := j.kids(n)
	for i := range list {
		j.predeclare(&list[i], mark)
	}
	for i := range list {
		j.hoistFunction(&list[i])
	}
	for i := range list {
		j.stmt(&list[i])
	}
	j.done(start)
	j.binds.truncate(mark)
}

// predeclare binds the block-scoped names statement n declares, in the block
// whose bindings start at mark. An enum or a namespace whose name the block
// already binds (an enum or namespace before it, or the class or function a
// namespace merges with) merges into that variable.
func (j *jsLower) predeclare(n *ts.Node, mark int) {
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
			j.predeclare(d, mark)
		}
	case k.enumDeclaration:
		if name := n.ChildByFieldId(k.fName); !j.erased(n) && !j.boundSince(mark, name) {
			j.declare(name)
		}
	case k.internalModule, k.module, k.expressionStatement:
		// Only a TypeScript grammar has namespaces; the guard keeps an
		// expression statement of the other grammars from being inspected.
		if k.internalModule == 0 {
			return
		}
		if ns := j.namespaceOf(n); ns != nil && ns.ChildByFieldId(k.fBody) != nil {
			if name := j.root(ns.ChildByFieldId(k.fName)); name != nil && name.KindId() == k.identifier && !j.boundSince(mark, name) {
				j.declare(name)
			}
		} else if n.KindId() == k.expressionStatement {
			if e := firstNamed(n); e != nil {
				j.declareUsing(e, false)
			}
		}
	}
}

// declareUsing binds the names of a using declaration as the typescript and
// tsx grammars parse it: an assignment carrying `using` (`using a = e`), or a
// comma sequence whose first element is one (`using a = e, b = f`), every
// element after it being another declarator of the same declaration. using
// reports whether an earlier element of the sequence carried `using`; the
// result is whether e or an element of it did.
func (j *jsLower) declareUsing(e *ts.Node, using bool) bool {
	k := j.k
	switch e.KindId() {
	case k.sequenceExpression:
		start, list := j.kids(e)
		for i := range list {
			using = j.declareUsing(&list[i], using)
		}
		j.done(start)
	case k.assignmentExpression:
		using = using || j.hasTok(e, k.usingKw)
		if left := j.strip(e.ChildByFieldId(k.fLeft)); using && left.KindId() == k.identifier {
			j.declare(left)
		}
	}
	return using
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
	mark := j.binds.mark()
	j.predeclare(n, mark)
	j.hoistFunction(n)
	j.stmt(n)
	j.binds.truncate(mark)
}

// label is a break or continue statement's label, or "".
func (j *jsLower) label(n *ts.Node) string {
	if l := n.ChildByFieldId(j.k.fLabel); l != nil {
		return view(j.text(l))
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
	// An assignment, compound assignment or update makes the nodes that
	// stand for the statement; its result variable goes unread.
	switch u := j.strip(e); u.KindId() {
	case k.internalModule, k.module:
		j.namespace(u)
	case k.assignmentExpression:
		j.assign(u)
	case k.augmentedAssignmentExpression:
		j.augment(u)
	case k.updateExpression:
		j.update(u)
	default:
		j.valueNode(e)
	}
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
			// The initializer is evaluated once, at its own node, whose
			// result is every element's incoming value.
			id, a := j.valueNode(val)
			j.give(id, a)
			j.bind(name, m, m+1)
			j.drop(m)
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
	mark := j.binds.mark()
	switch init := n.ChildByFieldId(k.fInitializer); init.KindId() {
	case k.lexicalDeclaration:
		j.predeclare(init, mark)
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
	j.binds.truncate(mark)
}

// forIn lowers for…in, for…of and for await…of.
func (j *jsLower) forIn(n *ts.Node, labels []string) {
	k := j.k
	mark := j.binds.mark()
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
	// ForIn/OfHeadEvaluation (ECMA-262 §14.7.5.6) evaluates the iterated
	// expression once; its node defines the iteration variable it, which the
	// head and every per-iteration node Use in its place.
	j.reset()
	right := j.l.unparen(n.ChildByFieldId(k.fRight))
	j.value(right, false)
	if of {
		j.throws++
	}
	it := j.b.Var()
	j.b.Def(j.node(flow.Stmt, right, 0, len(j.reads)), it)
	f := j.b.OpenLoop(labels...)
	j.reset()
	j.read(it)
	if of {
		j.throws++
	}
	head := flow.Span{Start: uint32(n.ChildByFieldId(k.fLeft).StartByte()), End: uint32(right.EndByte())}
	h := j.nodeAt(flow.Branch, head, 0, len(j.reads))
	exit := j.b.Push()
	// ForIn/OfBodyEvaluation (§14.7.5.7) assigns the next value once one
	// exists, evaluating a property target anew on every iteration: bind
	// makes that target's node, which Uses it and the target's reads.
	j.reset()
	j.read(it)
	j.bind(left, 0, len(j.reads))
	j.sub(n.ChildByFieldId(k.fBody))
	j.b.ContinueHere(f)
	j.loopEnd(f, h, true, exit)
	j.binds.truncate(mark)
}

func (j *jsLower) switchStmt(n *ts.Node, labels []string) {
	k := j.k
	disc := j.l.unparen(n.ChildByFieldId(k.fValue))
	j.value(disc, false)
	// The discriminant is evaluated once, at its own node, which defines
	// dv; each case test compares dv's value with its own, so its Branch
	// Uses dv.
	dv := j.b.Var()
	j.b.Def(j.node(flow.Stmt, disc, 0, len(j.reads)), dv)
	mark := j.binds.mark()
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
			j.predeclare(&list[i], mark)
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
		j.read(dv)
		v := j.l.unparen(cases[c].ChildByFieldId(k.fValue))
		j.value(v, false)
		j.node(flow.Branch, v, 0, len(j.reads))
		j.match[fb+c] = j.b.Push()
		if !pushed {
			firstPush, pushed = j.match[fb+c], true
		}
	}
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
	j.binds.truncate(mark)
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
		mark := j.binds.mark()
		if p := handler.ChildByFieldId(k.fParameter); p != nil {
			j.reset()
			j.declarePattern(p, false)
			j.bind(p, 0, 0)
		}
		j.block(handler.ChildByFieldId(k.fBody))
		j.binds.truncate(mark)
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
	// labels, the labels of the labelled statements this one is the body of,
	// is the top of lab; this label is pushed after them. The frame a loop,
	// a switch or the block below opens copies the names, so they are popped
	// once n is lowered.
	start := len(j.lab) - len(labels)
	j.lab = append(j.lab, view(j.text(n.ChildByFieldId(k.fLabel))))
	labels = j.lab[start:]
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
	j.lab = j.lab[:start]
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
		m := len(j.reads)
		j.give(j.closure(n), m)
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
			// The deciding operand's Branch yields the value on the path
			// where it decides it, the other operand's node on the other.
			m, r := len(j.reads), j.b.Var()
			left = j.l.unparen(left)
			j.value(left, false)
			j.yieldTo(j.node(flow.Branch, left, m, len(j.reads)), m, r)
			p := j.b.Push()
			saved := j.enter()
			id, a := j.valueNode(j.l.unparen(right))
			j.condFrom = saved
			j.yieldTo(id, a, r)
			j.b.Merge(p)
			j.b.Pop(p)
			j.read(r)
		default:
			j.value(left, false)
			j.value(right, false)
		}
	case k.ternaryExpression:
		// The condition is its Branch's own evaluation; each arm's node
		// yields the value.
		cond := j.l.unparen(n.ChildByFieldId(k.fCondition))
		m, r := len(j.reads), j.b.Var()
		j.value(cond, false)
		j.node(flow.Branch, cond, m, len(j.reads))
		j.drop(m)
		p := j.b.Push()
		saved := j.enter()
		id, a := j.valueNode(j.l.unparen(n.ChildByFieldId(k.fConsequence)))
		j.condFrom = saved
		j.yieldTo(id, a, r)
		t := j.b.Push()
		j.b.Restore(p)
		saved = j.enter()
		id, a = j.valueNode(j.l.unparen(n.ChildByFieldId(k.fAlternative)))
		j.condFrom = saved
		j.yieldTo(id, a, r)
		j.b.Merge(t)
		j.b.Pop(p)
		j.read(r)
	case k.assignmentExpression:
		j.assign(n)
	case k.augmentedAssignmentExpression:
		j.augment(n)
	case k.updateExpression:
		j.update(n)
	case k.memberExpression, k.subscriptExpression, k.callExpression:
		j.chain(n, inChain)
	case k.newExpression, k.awaitExpression, k.yieldExpression, k.spreadElement:
		j.children(n, true)
		j.throws++
	case k.unaryExpression:
		// A delete of a property writes through its base variable: the
		// node evaluating the delete, whose reads include the operand's,
		// may-defines it.
		m := len(j.reads)
		j.children(n, true)
		if v := j.deleted(n); v >= 0 {
			j.may = append(j.may, jsMay{at: m, v: v})
		}
	case k.class:
		m := len(j.reads)
		j.give(j.closure(n), m)
	case k.jsxOpeningElement, k.jsxSelfClosingElement:
		j.jsxElement(n, true)
	case k.jsxClosingElement:
	default:
		j.children(n, true)
	}
}

// deleted is the base variable of the property a unary expression n deletes
// (o in `delete o.p`, `delete o[k]`, `delete o?.p`), or -1 when n is no
// delete of a property or its base is no variable.
func (j *jsLower) deleted(n *ts.Node) int32 {
	k := j.k
	if n.ChildByFieldId(k.fOperator).KindId() != k.deleteKw {
		return -1
	}
	switch t := j.strip(n.ChildByFieldId(k.fArgument)); t.KindId() {
	case k.memberExpression, k.subscriptExpression:
		return j.base(t)
	}
	return -1
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
	base, outer, outerFrom := len(j.opt), j.optR, j.condFrom
	if !inChain {
		j.optR = -1
	}
	m := len(j.reads)
	id := n.KindId()
	recv := n.ChildByFieldId(k.fObject)
	if id == k.callExpression {
		recv = n.ChildByFieldId(k.fFunction)
	}
	j.value(recv, true)
	// A TypeScript optional call `f?.(…)` carries its `?.` as an anonymous
	// token, not in the optional_chain field.
	// The Branch before a `?.` yields the chain's value, undefined, on its
	// nullish exit, and hands the receiver on through the same variable.
	if n.ChildByFieldId(k.fOptionalChain) != nil || id == k.callExpression && j.hasTok(n, k.optTok) {
		if j.optR < 0 {
			j.optR = j.b.Var()
		}
		j.yieldTo(j.node(flow.Branch, j.l.unparen(recv), m, len(j.reads)), m, j.optR)
		j.read(j.optR)
		j.opt = append(j.opt, j.b.Push())
		// What follows the `?.` runs only on the non-nullish path.
		j.condFrom = len(j.reads)
	}
	switch id {
	case k.subscriptExpression:
		j.value(n.ChildByFieldId(k.fIndex), false)
	case k.callExpression:
		j.value(n.ChildByFieldId(k.fArguments), false)
	}
	j.throws++
	if inChain {
		return
	}
	if len(j.opt) == base {
		j.optR, j.condFrom = outer, outerFrom
		return
	}
	j.yieldTo(j.node(flow.Stmt, n, m, len(j.reads)), m, j.optR)
	j.read(j.optR)
	for i := len(j.opt) - 1; i >= base; i-- {
		j.b.Merge(j.opt[i])
	}
	j.b.Pop(j.opt[base])
	j.opt = j.opt[:base]
	j.optR, j.condFrom = outer, outerFrom
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

// assign lowers `left = right`, the node spanning n or, for a destructuring,
// the right side's node and the element nodes, and leaves the value's result
// variable as its only read past the reads before it.
func (j *jsLower) assign(n *ts.Node) {
	k := j.k
	left, right := j.strip(n.ChildByFieldId(k.fLeft)), n.ChildByFieldId(k.fRight)
	m := len(j.reads)
	switch left.KindId() {
	case k.identifier:
		j.value(right, false)
		// An assignment carrying `using` is a using declaration's
		// declarator (see Scoping in lowerJavaScript), which spans from
		// its name, as a declarator does.
		span := spanOf(n)
		if j.hasTok(n, k.usingKw) {
			span.Start = uint32(left.StartByte())
		}
		j.carry(j.nodeAt(flow.Stmt, span, m, len(j.reads)), m, left)
	case k.objectPattern, k.arrayPattern:
		// The right side is evaluated once, at its own node, whose result
		// is the incoming value of every element and the assignment's
		// value.
		id, a := j.valueNode(right)
		j.give(id, a)
		j.bind(left, m, m+1)
		j.drop(m + 1)
	default:
		// The store happens after the right side is evaluated, so its throw
		// belongs to the write's node, not to a node the right side makes.
		if j.reference(left) {
			j.value(right, false)
			j.throws++
		} else {
			j.value(right, false)
		}
		id := j.node(flow.Stmt, n, m, len(j.reads))
		j.mayDefBase(id, left)
		j.give(id, m)
	}
}

// update lowers `x++`, `--x` and their property forms, one node spanning n
// that reads and writes the target; its result is left as for assign.
func (j *jsLower) update(n *ts.Node) {
	k := j.k
	arg := j.strip(n.ChildByFieldId(k.fArgument))
	m := len(j.reads)
	if arg.KindId() == k.identifier {
		j.ref(arg)
		j.carry(j.node(flow.Stmt, n, m, len(j.reads)), m, arg)
		return
	}
	j.target(arg)
	id := j.node(flow.Stmt, n, m, len(j.reads))
	j.mayDefBase(id, arg)
	j.give(id, m)
}

// augment lowers a compound or logical assignment; its result is left as
// for assign.
func (j *jsLower) augment(n *ts.Node) {
	k := j.k
	left, right := j.strip(n.ChildByFieldId(k.fLeft)), n.ChildByFieldId(k.fRight)
	op := n.ChildByFieldId(k.fOperator).KindId()
	logical := op == k.andAssign || op == k.orAssign || op == k.nullishAssign
	named := left.KindId() == k.identifier
	m := len(j.reads)
	if !logical {
		if named {
			j.ref(left)
		} else if j.reference(left) {
			// ECMA-262 §13.15.2: the reference's value is read (GetValue)
			// before the right side is evaluated, so the property read
			// throws on a node of its own, spanning the target, before any
			// node the right side makes; its result carries the reference
			// and the old value to the write.
			j.throws++
			j.give(j.node(flow.Stmt, left, m, len(j.reads)), m)
		}
		// The store (PutValue) follows the right side, so its throw is on
		// the write's node.
		j.value(right, false)
		if !named {
			j.throws++
		}
		id := j.node(flow.Stmt, n, m, len(j.reads))
		if named {
			j.carry(id, m, left)
		} else {
			j.mayDefBase(id, left)
			j.give(id, m)
		}
		return
	}
	// The Branch spanning the target yields its old value when it decides
	// the result; the write node spanning n yields the assigned value.
	if named {
		j.ref(left)
	} else {
		j.target(left)
	}
	r := j.b.Var()
	j.yieldTo(j.node(flow.Branch, left, m, len(j.reads)), m, r)
	p := j.b.Push()
	saved := j.enter()
	if !named {
		// The write stores through the reference the Branch evaluated.
		j.read(r)
	}
	j.value(right, false)
	if !named {
		j.throws++
	}
	id := j.node(flow.Stmt, n, m, len(j.reads))
	if named {
		j.def(id, j.lookup(left))
	} else {
		j.mayDefBase(id, left)
	}
	j.condFrom = saved
	j.yieldTo(id, m, r)
	j.b.Merge(p)
	j.b.Pop(p)
	j.read(r)
}

// carry makes node id, which carries reads[m:], define the variable the
// identifier x names and yield its value into an owned result, both killing
// definitions, which the consumer reads in place of reads[m:]: never x
// itself, which a later assignment in the same expression may overwrite.
func (j *jsLower) carry(id int32, m int, x *ts.Node) {
	j.def(id, j.lookup(x))
	j.give(id, m)
}

// mayDefBase records that node id, a write through the property target t,
// may-defines the variable at t's base (o in `o.p`, `o[i].q`), as every
// write through a field, index or pointer target is a may-definition of its
// base variable (see Lowering).
func (j *jsLower) mayDefBase(id int32, t *ts.Node) {
	if v := j.base(t); v >= 0 {
		j.b.MayDef(id, v)
	}
}

// base is the variable at the base of the property target t, or -1.
func (j *jsLower) base(t *ts.Node) int32 {
	k := j.k
	t = j.strip(t)
	for t != nil && (t.KindId() == k.memberExpression || t.KindId() == k.subscriptExpression) {
		t = j.strip(t.ChildByFieldId(k.fObject))
	}
	if t == nil || t.KindId() != k.identifier {
		return -1
	}
	return j.lookup(t)
}

// bind lowers a binding or assignment pattern whose incoming value is carried
// by reads[from:to] (the result variable of the node that evaluated it, an
// iteration variable, or nothing for an argument or a thrown value): one
// defining node per bound name, in source order, each using all of
// reads[from:to] (see Destructuring in lowerJavaScript). An element is read
// through the transparent wrappers, so `[y!] = a` defines y.
func (j *jsLower) bind(p *ts.Node, from, to int) {
	k := j.k
	p = j.strip(p)
	switch p.KindId() {
	case k.identifier, k.shorthandPropertyIdentifierPattern:
		j.def(j.node(flow.Stmt, p, from, to), j.lookup(p))
	case k.memberExpression, k.subscriptExpression:
		m := len(j.reads)
		j.target(p)
		id := j.node(flow.Stmt, p, from, to)
		j.uses(id, m, len(j.reads))
		j.mayDefBase(id, p)
		j.drop(m)
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
					// The element's nodes read the key, their own
					// evaluation, and the incoming value.
					m := len(j.reads)
					j.value(key, false)
					j.reads = append(j.reads, j.reads[from:to]...)
					j.bind(val, m, len(j.reads))
					j.drop(m)
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
	left = j.strip(left)
	named := left.KindId() == k.identifier || left.KindId() == k.shorthandPropertyIdentifierPattern
	v := int32(-1)
	if named {
		v = j.lookup(left)
	}
	// A nested pattern destructures either value: the Branch and the
	// default's node yield it into u.
	u := v
	if !named {
		u = j.b.Var()
	}
	br := j.node(flow.Branch, whole, from, to)
	j.def(br, u)
	p := j.b.Push()
	saved := j.enter()
	id, m := j.valueNode(j.l.unparen(dflt))
	j.condFrom = saved
	j.def(id, u)
	j.drop(m)
	j.b.Merge(p)
	j.b.Pop(p)
	if named {
		return
	}
	j.read(u)
	j.bind(left, m, m+1)
	j.drop(m)
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
	// The parameters' defaults and the body run when the callable is
	// called, not where an enclosing class is created, so a decorator in
	// them is not counted for that class.
	dec := j.decorated
	j.shadow++
	mark, savedFn := j.binds.mark(), j.fnMark
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
	j.binds.truncate(mark)
	j.fnMark = savedFn
	j.shadow--
	j.decorated = dec
}

// capClass collects a class's captures: its heritage, computed member names,
// field initializers and members. The class's own name is bound inside it.
// Where the class is created (ECMA-262 §15.7.14 ClassDefinitionEvaluation)
// its `extends` expression, its computed keys, its static field initializers
// and its static blocks are evaluated, and each may throw; a TypeScript
// `implements` clause is erased and evaluates nothing.
func (j *jsLower) capClass(n *ts.Node) {
	k := j.k
	start, list := j.kids(n)
	if j.shadow == 0 {
		for i := range list {
			switch c := &list[i]; c.KindId() {
			case k.classHeritage:
				if k.extendsClause == 0 || j.childOfKind(c, k.extendsClause) {
					j.throws++
				}
			case k.classBody:
				if j.createThrows(c) {
					j.throws++
				}
			}
		}
	}
	dec := j.decorated
	j.shadow++
	mark := j.binds.mark()
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
	j.binds.truncate(mark)
	j.shadow--
	if j.shadow == 0 && j.decorated > dec {
		j.throws++
	}
}

// childOfKind reports whether one of n's named children is of kind id.
func (j *jsLower) childOfKind(n *ts.Node, id uint16) bool {
	start, list := j.kids(n)
	defer j.done(start)
	for i := range list {
		if list[i].KindId() == id {
			return true
		}
	}
	return false
}

// createThrows reports whether class body n evaluates, where its class is
// created, a computed key, a static field initializer or a static block.
func (j *jsLower) createThrows(n *ts.Node) bool {
	k := j.k
	start, list := j.kids(n)
	defer j.done(start)
	for i := range list {
		switch m := &list[i]; m.KindId() {
		case k.classStaticBlock:
			return true
		case k.methodDefinition, k.fieldDefinition, k.publicFieldDefinition:
			name := m.ChildByFieldId(k.fName)
			if name == nil {
				name = m.ChildByFieldId(k.fProperty)
			}
			if name != nil && name.KindId() == k.computedPropertyName {
				return true
			}
			if m.KindId() != k.methodDefinition && m.ChildByFieldId(k.fValue) != nil && j.hasTok(m, k.staticKw) {
				return true
			}
		}
	}
	return false
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
			mark, savedFn := j.binds.mark(), j.fnMark
			j.fnMark = mark
			j.hoistVars(body)
			j.cap(body)
			j.binds.truncate(mark)
			j.fnMark = savedFn
		}
	case k.nonNullExpression, k.asExpression, k.satisfiesExpression, k.typeAssertion, k.instantiationExpression:
		// The wrapper evaluates its operand; its type is no code.
		if o := j.operand(n); o != nil {
			j.cap(o)
		}
	case k.assignmentExpression:
		j.capTarget(n.ChildByFieldId(k.fLeft))
		j.cap(n.ChildByFieldId(k.fRight))
	case k.augmentedAssignmentExpression, k.updateExpression:
		// A compound or logical assignment and an update read the variable
		// they write.
		t := n.ChildByFieldId(k.fLeft)
		if t == nil {
			t = n.ChildByFieldId(k.fArgument)
		}
		j.capTarget(t)
		if u := j.strip(t); u.KindId() == k.identifier {
			j.ref(u)
		}
		if r := n.ChildByFieldId(k.fRight); r != nil {
			j.cap(r)
		}
	case k.fieldDefinition, k.publicFieldDefinition:
		// A field's initializer runs where the class is created (a static
		// one, a class-level throw of its own: see capClass) or constructed,
		// never as one of the class's decorator applications, so a
		// decorator inside it is not counted for the class.
		v := n.ChildByFieldId(k.fValue)
		start, list := j.kids(n)
		for i := range list {
			dec := j.decorated
			j.cap(&list[i])
			if v != nil && list[i].StartByte() == v.StartByte() {
				j.decorated = dec
			}
		}
		j.done(start)
	case k.classDeclaration, k.abstractClassDeclaration, k.class:
		j.capClass(n)
	case k.classStaticBlock:
		j.capFunction(n)
	case k.statementBlock:
		mark := j.binds.mark()
		start, list := j.kids(n)
		for i := range list {
			j.predeclare(&list[i], mark)
		}
		for i := range list {
			j.cap(&list[i])
		}
		j.done(start)
		j.binds.truncate(mark)
	case k.switchStatement:
		j.cap(n.ChildByFieldId(k.fValue))
		mark := j.binds.mark()
		body := n.ChildByFieldId(k.fBody)
		start, cases := j.kids(body)
		for c := range cases {
			s, list := j.kids(&cases[c])
			for i := range list {
				j.predeclare(&list[i], mark)
			}
			j.done(s)
		}
		j.done(start)
		j.cap(body)
		j.binds.truncate(mark)
	case k.forStatement:
		mark := j.binds.mark()
		if init := n.ChildByFieldId(k.fInitializer); init.KindId() == k.lexicalDeclaration {
			j.predeclare(init, mark)
		}
		j.children(n, false)
		j.binds.truncate(mark)
	case k.forInStatement:
		mark := j.binds.mark()
		left := j.strip(n.ChildByFieldId(k.fLeft))
		if kw := n.ChildByFieldId(k.fKind); kw == nil {
			j.capTarget(left)
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
		j.binds.truncate(mark)
	case k.catchClause:
		mark := j.binds.mark()
		if p := n.ChildByFieldId(k.fParameter); p != nil {
			j.declarePattern(p, false)
			j.patternExprs(p)
		}
		j.cap(n.ChildByFieldId(k.fBody))
		j.binds.truncate(mark)
	case k.variableDeclarator:
		j.patternExprs(n.ChildByFieldId(k.fName))
		if v := n.ChildByFieldId(k.fValue); v != nil {
			j.cap(v)
		}
	case k.unaryExpression:
		// A delete of a property is a write through its base variable.
		j.children(n, false)
		if v := j.deleted(n); v >= 0 {
			j.writes = append(j.writes, v)
		}
	case k.jsxOpeningElement, k.jsxSelfClosingElement:
		j.jsxElement(n, false)
	case k.jsxClosingElement:
	default:
		j.children(n, false)
	}
}

// capTarget collects an assignment target inside a nested callable or class:
// the enclosing variables it binds (an identifier, or every name of a
// destructuring target) are writes and not reads, since a plain assignment
// or a binding only writes them; a default, a computed key and the object and
// index of a property target are evaluated, so their references are reads,
// and a property target writes its base variable (a may-definition, as
// outside a callable).
func (j *jsLower) capTarget(t *ts.Node) {
	k := j.k
	switch t = j.strip(t); t.KindId() {
	case k.identifier, k.shorthandPropertyIdentifierPattern:
		if v := j.lookup(t); v >= 0 {
			j.writes = append(j.writes, v)
		}
	case k.assignmentPattern, k.objectAssignmentPattern:
		j.capTarget(t.ChildByFieldId(k.fLeft))
		j.cap(t.ChildByFieldId(k.fRight))
	case k.pairPattern:
		if key := t.ChildByFieldId(k.fKey); key.KindId() == k.computedPropertyName {
			j.cap(key)
		}
		j.capTarget(t.ChildByFieldId(k.fValue))
	case k.objectPattern, k.arrayPattern, k.restPattern:
		start, list := j.kids(t)
		for i := range list {
			j.capTarget(&list[i])
		}
		j.done(start)
	case k.memberExpression, k.subscriptExpression:
		j.cap(t)
		if v := j.base(t); v >= 0 {
			j.writes = append(j.writes, v)
		}
	default:
		j.cap(t)
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
	updateExpression, unaryExpression, callExpression, newExpression, awaitExpression, yieldExpression, memberExpression,
	subscriptExpression, spreadElement, functionExpression, generatorFunction, methodDefinition,
	classBody, fieldDefinition, classStaticBlock, trueLit, objectPattern, arrayPattern, assignmentPattern, objectAssignmentPattern,
	pairPattern, restPattern, computedPropertyName, jsxOpeningElement, jsxSelfClosingElement,
	jsxClosingElement uint16

	// The TypeScript kinds, optional.
	requiredParameter, optionalParameter, publicFieldDefinition, abstractClassDeclaration, enumDeclaration,
	enumAssignment, internalModule, module, nestedIdentifier, importRequireClause, importAlias, nonNullExpression,
	asExpression, satisfiesExpression, typeAssertion, instantiationExpression, typeArguments, decorator,
	extendsClause uint16
	// erased is indexed by kind id: the kinds the TypeScript compiler erases.
	erased []bool
	// imports is the import-clause table the extraction shares.
	imports importSyntax

	and, or, nullish, andAssign, orAssign, nullishAssign, varKw, ofKw, constKw, eqTok, staticKw, usingKw, deleteKw uint16
	// typeKw and optTok (the anonymous `?.` of a TypeScript optional call)
	// are optional.
	typeKw, optTok uint16

	fAlternative, fArgument, fArguments, fBody, fCondition, fConsequence, fDeclaration, fFinalizer,
	fFunction, fHandler, fIncrement, fIndex, fInitializer, fKey, fKind, fLabel, fLeft, fName, fObject,
	fOperator, fOptionalChain, fParameter, fParameters, fPattern, fProperty, fRight, fValue uint16
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
	s.unaryExpression = kind("unary_expression")
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
	s.constKw, s.eqTok, s.staticKw, s.usingKw, s.deleteKw = tok("const"), tok("="), tok("static"), tok("using"), tok("delete")
	s.typeKw, s.optTok = tl.IdForNodeKind("type", false), tl.IdForNodeKind("?.", false)
	s.extendsClause = opt("extends_clause")
	s.fAlternative, s.fArgument, s.fArguments = field("alternative"), field("argument"), field("arguments")
	s.fBody, s.fCondition, s.fConsequence, s.fDeclaration = field("body"), field("condition"), field("consequence"), field("declaration")
	s.fFinalizer, s.fFunction, s.fHandler, s.fIncrement = field("finalizer"), field("function"), field("handler"), field("increment")
	s.fIndex, s.fInitializer, s.fKey, s.fKind, s.fLabel = field("index"), field("initializer"), field("key"), field("kind"), field("label")
	s.fLeft, s.fName, s.fObject, s.fOperator = field("left"), field("name"), field("object"), field("operator")
	s.fOptionalChain, s.fParameter, s.fParameters = field("optional_chain"), field("parameter"), field("parameters")
	s.fPattern = field("pattern")
	s.fProperty, s.fRight, s.fValue = field("property"), field("right"), field("value")
	return s
}
