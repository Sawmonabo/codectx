package worker

import (
	"bytes"
	"slices"
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
//     A receiver parameter and an `_` name declare nothing; neither is
//     observable in any pair (a receiver has no name a body can read), so
//     no golden case pins them.
//   - A statement is one node: an expression statement spans its
//     expression, a declarator with an initializer spans the declarator,
//     return, throw, yield, break and continue span the statement (kind
//     Jump), an explicit constructor invocation spans itself, its `;`
//     included, as the grammar holds it, a local class,
//     record, enum or interface declaration spans the declaration. An
//     expression statement that assigns or updates a local is its defining
//     node. A declarator without an initializer makes no node: it executes
//     nothing (JLS §14.4.2), and the variable is not definitely assigned, so
//     no use can read a definition made there (JLS §16) and no pair could
//     show one; no golden case pins it.
//   - A condition is one Branch node spanning the condition without its
//     parentheses: if, while, do, for. It is always made, after the nodes
//     its own evaluation makes (an `&&` or `||` operand's nodes, an
//     instanceof's tested value and pattern variables), and the back edge of a while or basic for
//     re-enters the first of those, since the condition is evaluated again
//     (JLS §14.12, §14.14.1.2). A loop whose condition is the literal
//     `true`, or a for without one, has no exit edge: its head is a Stmt node
//     spanning `true` or the `for` keyword (JLS §14.22).
//   - An enhanced for (JLS §14.14.2) follows the iteration model (see
//     Iteration in Lowering): a Stmt node spanning the iterated expression,
//     evaluated once, which Uses its reads and defines an iteration variable
//     of the lowering's own; a Branch head spanning the header from its
//     first modifier or type to the end of the iterated expression, which
//     Uses only that variable; then, on the body path, one node spanning the
//     loop variable's name that defines it and Uses only the iteration
//     variable. The back edge re-enters the head, never the iterated
//     expression.
//   - A switch (JLS §14.11 statement, §15.28 expression) is a Stmt node for
//     its selector, evaluated once, which defines an owned variable (see
//     Uses), then one Branch node per `case` label in source order,
//     spanning the grammar's label, which begins at its `case` keyword
//     (`case null`, `case String s`) and holds its guard and neither the
//     `:` nor the `->` after it, each tested only when the previous failed; a label
//     listing several constants (`case 2, 3`) is one label and one test
//     (JLS §14.11.1). A label's node Uses the selector's variable, since it
//     compares against the selector, and its constants' reads. A pattern
//     label is followed on its match path by one defining node per pattern
//     variable, spanning the variable's identifier and using the selector's
//     variable, then, for a guard, a Branch node spanning the
//     guard expression whose false edge joins the label's own no-match
//     path: the next label's test, or, after the last label, the default
//     group or the switch's no-match exit.
//     Several labels of one group all enter its body. `default` makes no
//     node: the last label's false edge, the path on which no label
//     matched, enters the group holding it, together with a colon-form
//     fall-through into that group. `case null, default` (JLS §14.11.1)
//     matches every value, null included, so it is a `default` label and
//     makes no node either; the grammar holds it as a `case` label whose
//     last expression is the identifier `default` (see isDefault). In the colon
//     form a body's end falls through into the next body; in the arrow form
//     it leaves the switch. A switch expression without `default` is
//     exhaustive, and its no-match path throws (JLS §15.28.2); so does that
//     of a switch statement without `default` whose labels include a pattern
//     or `null`, an enhanced switch statement, which is exhaustive too (JLS
//     §14.11.2, §14.11.3: MatchException); each such throw is a Throw from the
//     last label's false edge, with no node of its own. Any other switch
//     statement without `default` is left when no label matches, and so is every
//     switch whose tree holds a syntax error (an error or missing node), whose
//     recovery may have dropped its `default`. An arrow arm's expression is a Stmt node spanning it, and
//     the yield of its value; `yield e;` is a Jump node spanning the
//     statement. Every yield breaks to the switch expression's frame (see
//     yieldLabel). The arm result nodes and the yields define the switch
//     expression's owned result variable, which the node consuming it Uses.
//   - `&&`, `||` and the conditional operator: the deciding operand is a
//     Branch node spanning it, created after the nodes of everything it
//     evaluates; each conditionally evaluated operand is a Stmt node spanning
//     it. For `&&` and `||` the deciding operand's node and the second
//     operand's node, for `c ? a : b` each arm's node, define the owned
//     result variable the consumer Uses, in a condition as in any other
//     position.
//   - `x instanceof T v` and a record pattern evaluate the tested expression
//     once, at a Stmt node spanning it that defines an owned variable, then
//     define each pattern variable on one node spanning its identifier,
//     using that variable, before the node that decides on the test, into
//     which the test itself is folded. An instanceof without a pattern is
//     folded whole. The deciding node is the one consuming the test: a
//     condition's Branch through any `!` or parentheses (`!` swaps the
//     pattern variable sets and makes no node), an `&&` or `||` deciding
//     operand's Branch, or the node of the statement holding the test.
//   - try (JLS §14.20): each catch clause's type test is a Branch node
//     spanning its catch type (a multi-catch `A | B` is one test), in order;
//     its match path defines the parameter on a node spanning its name; the
//     path matching no clause, the last test's false edge, is rethrown by a
//     Throw with no node of its own. EnterHandler has popped the catch
//     frame, so the Throw goes to an enclosing catch or to Exit, and the
//     statement's own finally, open around its catch clauses, intercepts it
//     as a Throw source. A clause catching `Throwable`
//     catches everything: its test is a Stmt node and ends the chain. The
//     builder's Handler spans the first `catch` keyword, or `finally`.
//   - try-with-resources (JLS §14.20.3.1, the basic form; the extended
//     form of §14.20.3.2 adds the statement's own catch clauses and
//     finally around it): each resource declaring a
//     variable is an acquiring node spanning the resource from its name to
//     the end of its initializer, defining the variable; a resource naming an
//     existing variable or field acquires nothing. Each resource then opens a
//     finally whose close is one Stmt node spanning the resource that uses
//     its variable (see Uses) and may throw; the resources close in reverse
//     order, and the statement's catch clauses and finally enclose all of
//     them. A resource's Handler spans the `;` or `)` that ends the
//     resource. A close runs inside its own finally body, which intercepts
//     nothing, so the close's own MayThrow edges into the Handler of the
//     resource acquired before it, while every throw its finally re-issues
//     from the close (CloseFinally re-issues each intercepted MayThrow as a
//     throw) is a Throw source of the next frame out: the outer resource's
//     finally, which intercepts it as an edge from the close into its body
//     that coincides with normal completion, never a second edge into its
//     Handler; or, for the first resource of the extended form, the
//     statement's catch, whose Throw sources enter its first clause test
//     directly. The null test
//     before close is not modelled.
//   - `synchronized (e) { … }` (JLS §14.19) is a Stmt node spanning e, which
//     may throw, then the block; the monitor exit is not a node.
//   - `assert c : m;` (JLS §14.10) is a Branch node spanning the `assert`
//     keyword, whether assertions are enabled, whose false edge skips the
//     statement: a disabled assertion evaluates neither operand. Its true
//     edge reaches a Branch node spanning c whose false path is a Stmt node
//     spanning m, when present, then a Throw with no node of its own, from
//     m's node or, without m, from c's false edge.
//   - A type body unit lowers, in source order, each field or constant
//     declarator with an initializer (its value's nodes, then a Stmt node
//     spanning the declarator), each enum constant (a Stmt node spanning it)
//     and each instance initializer block. Static initializer blocks,
//     methods, constructors and nested types are their own callables. A
//     static field's initializer, a constant's and an enum constant run at
//     class initialization (JLS §12.4.2), the others at instance creation
//     (§12.5), yet the unit holds both, in source order: no local or pattern
//     variable crosses from one initializer to the next, no initializer
//     expression throws outside a try, and an instance initializer block
//     must be able to complete normally (§8.6), so the unit's pairs are
//     exactly each initializer's own, and the order between them adds only
//     unconditional edges.
//   - A lambda's expression body is lowered for its value: its nodes, then
//     a Stmt node spanning the body that Uses what they hand on, by the rule
//     below; a block body is lowered as a block.
//   - An expression lowered for its value and ended by a node spanning it
//     takes no second node when the last node its own lowering made already
//     spans it: that node holds the value.
//
// Statement kinds: every kind of the grammar's statement supertype is
// handled above. An empty statement (`;`), the body of an if, else, loop or
// label included, makes no node; package, import
// and module declarations do not occur in a callable; any kind the lowering
// does not name is one Stmt node spanning it, with its uses, falling
// through. An ERROR node of the parser's recovery is such a kind wherever it
// stands, whether the parser made it an extra or not: it is lowered for its
// value as any unnamed kind is, so a construct inside it that makes a node
// anywhere (an embedded assignment, a lambda) makes one there and defines
// what it defines there, and the rest folds into the Stmt node spanning it,
// which is not made when the construct's node already spans it. Every
// lowering keeps an ERROR node, extra or not, and makes its nodes as this
// rule does: a construct inside it that its language lowers to nodes of its
// own makes them there (Python lowers each named child of a
// statement-position ERROR as a statement, which a statement the recovery
// kept needs), and the ERROR node is a Stmt node spanning it. Java,
// JavaScript and Python leave that node out when a construct's node already
// spans the ERROR node; C, Go and Rust always make it, which gives a second
// node over that span only for an ERROR node no wider than its one
// construct. Expression kinds other than the ones above
// (method and constructor invocations, an object creation without a class
// body, field and array access, casts, unary, arithmetic and comparison
// operators, the instanceof test, method references, array creation, class
// literals, templates, literals, `this`) carry no control flow and define
// no local: each is folded into the node that consumes it, whose own
// evaluation its reads and throws are.
//
// # Uses
//
// Only an identifier resolving to a local variable or parameter declared in
// this function is a Use; a field, `this.x` included, is not a variable. A
// local constant variable (`final int k = 1`, JLS §4.12.4) is a local
// variable like any other: a case label naming it Uses it.
// Values travel through variables (see Lowering): a node Uses what its own
// evaluation reads, each read resolved in the scope where it occurs, and a
// nested construct lowered to nodes of its own hands its value to its
// consumer through an owned result variable. In Java the constructs lowered
// to nodes are the switch expression, the conditional, `&&` and `||`, an
// embedded assignment or update, a lambda and an anonymous class creation;
// every other expression is folded (see Statement kinds). So:
//
//   - The node consuming a switch expression Uses its result variable, which
//     each arrow arm's result node and each yield define, read in the arm's
//     own scope. A switch's selector node defines an owned variable, and
//     each label and pattern variable node Uses it, never the selector's
//     names; a label's node also Uses its constants' reads, not its guard's,
//     which are the guard's Branch node's.
//   - The node consuming `c ? a : b` Uses the result each arm's node
//     defines; the condition is a Branch node of its own. The node consuming
//     `a && b` or `a || b` Uses the result the deciding operand's Branch node
//     defines and the second operand's node defines again, so SSA merges the
//     two.
//   - An assignment or update embedded in a larger expression (`(x = a) *
//     2`, `f(o.f = a)`, `g(i++)`) is its own node, which hands its value to
//     its consumer through an owned result, never through its target: one
//     of a local defines both the local and the result, one of a field or
//     element target may-defines its base and defines the result. So `(x =
//     1) + (x = 2)` depends on both. At statement level it is the
//     statement's node and hands on nothing.
//   - A node may make several killing definitions. A yielding node that
//     defines a variable of its own (an embedded assignment to a local, an
//     arm's result node that is itself one or a creation) defines the
//     construct's result too (see Lowering).
//   - The node deciding on `x instanceof P` with a pattern Uses the tested
//     value's owned variable, as each pattern variable's defining node does:
//     the tested expression is evaluated once, at its own node (JLS
//     §15.20.2).
//   - A resource's close Uses only its variable, or, for a resource naming
//     an existing variable or field, that expression's reads: the
//     initializer was evaluated once, at the acquiring node.
//
// An assignment or update of a local carries the reads of the local that
// its statement made before it and no node carries (see Lowering): in `y =
// x + (x = 2)` the node `x = 2` Uses x's earlier value and defines an
// owned variable holding it, which the declarator Uses in place of x, since
// Java evaluates the left operand first (JLS §15.7.1). The hand-off is made
// only when the assignment runs whenever the read's consumer does: in `y = x
// + (c ? (x = 1) : 0)`, and inside an `&&` or `||` right operand, the
// declarator keeps its read of x, which sees the parameter when the arm is
// skipped and the assignment when it runs. A write through a
// field or array element of a local (`o.f = v`, `a[i] += v`, `a[i]++`) is a
// Stmt node spanning the assignment that Uses its operands, the local at the
// base of the target included, since the object or array reference is
// evaluated first (JLS §15.26.1), and may-defines that local (see
// May-definitions in Lowering).
//
// A lambda (its own function) is one Stmt node spanning it in the enclosing
// function; an anonymous class's body is its own unit, and the object
// creation expression is one Stmt node spanning the creation. Either node
// defines an owned result, the created value, which its consumer Uses, and
// Uses every enclosing variable referenced inside (the creation also its
// arguments' reads), its own evaluation, resolved with the nested code's own
// declarations shadowing (a local or parameter of a method of the anonymous
// class, or one of its fields). The walk that collects them treats a plain
// assignment's local target as written, not read; a compound assignment and
// an update read it too. A captured local is effectively final (JLS
// §15.27.2, §8.1.3), so no nested code of a valid program assigns it; in a
// program the compiler rejects for doing so, a plain assignment there is
// neither a Use nor a MayDef of the local. A write through its
// field or array element inside (`arr[0] = 1`, `box.v = 2`) is the
// through-a-target rule: a MayDef of the local on the creating node, since
// when the nested code runs is unknown, and a Use of it there too, since the
// write reads the local to reach its field or element. A local class
// declaration's node is its creating node: it Uses the captures and
// may-defines the locals written through them, but defines no result, since
// a declaration has no value.
//
// A name that resolves to no variable (see Names that resolve to no
// variable in Lowering) is -1 from lookup: a field or inherited member named
// by its simple name, an undeclared name, and, in a capture walk, every name
// the nested code declares. It takes every position a local takes: an
// assignment target, a compound assignment, an increment, an embedded
// assignment or update, a name nested code captures, and the base of a field
// or element target. It is filtered where it enters: read drops it, so no
// node Uses it and it never indexes at; holds rejects it, so earlier hands
// nothing off; def drops it, so no node defines it; and assign, update and
// cap test base's result before a MayDef or a writes entry. Every other
// variable the builder or at receives is a fresh Var, a result, or a reads
// or writes entry that passed those filters. The node the construct makes is
// still made, with its other operands' reads, and an embedded assignment or
// update to such a name still defines its owned result, which its consumer
// Uses: in `g(x = a)` for a field x, the node `x = a` Uses a, defines
// neither x nor anything x names, and defines the result `g(…)` Uses (hand
// defines it whatever the target resolves to).
//
// # Exceptions
//
// MayThrow is given to every node whose own evaluation — the source
// evaluated since the previous node — contains a method or constructor
// invocation, an instance or array creation, an array access, a field access
// through a receiver other than `this` or `super`, a cast, an enhanced-for
// iterator step (the head) or a resource close; a monitor expression may
// throw. Division, unboxing and string conversion are not counted. A throw
// statement's node is a Throw, and also MayThrow when its operand holds such
// a construct (`throw new E()`: the creation may throw before the throw), so
// inside a try with catch clauses it has an edge into the Handler, and its
// Throw enters the first clause test directly. A construct evaluated before
// a nested node (a switch expression's selector, an arm, an operand lowered
// to nodes) is that node's, never again its consumer's. A failed
// assertion, an unmatched catch chain and an exhaustive switch's no-match
// path are a Throw with no node of its own. The Builder applies MayThrow only inside an open catch or finally
// frame.
//
// # Scoping
//
// A local's scope runs from its declarator to the end of its block (JLS
// §6.3), and one declared in a switch block statement group to the end of
// the switch block; a for header's locals are scoped to the loop, a catch
// parameter to its clause, a resource to the try block and its closes, a
// case label's pattern variables, and those its guard introduces when true,
// to its guard and its rule or group.
//
// A pattern variable's scope follows JLS §6.3.1 and §6.3.2. Each
// expression introduces a set of pattern variables when true and a set
// when false: `x instanceof P` introduces P's variables when true, `!a`
// swaps a's sets, `a && b` introduces the union of both when-true sets
// when true and puts a's when-true set in scope in b, `a || b` does the
// same with the when-false sets, a parenthesized expression passes its
// sets on, and `a ? b : c` puts a's when-true set in scope in b and its
// when-false set in c and introduces nothing; no other expression
// introduces any. An if statement's condition puts its when-true set in
// scope in the then statement and its when-false set in the else
// statement. After the statement, in the rest of the enclosing block or
// switch group, the if introduces its condition's when-false set when the
// then statement cannot complete normally and the else statement, if any,
// can, and its when-true set when only the else statement cannot (§6.3.2.2).
// A while or basic for condition puts its when-true set in scope in the
// body (and the update clause), and a while, do or basic for introduces its
// condition's when-false set when its body holds no break that leaves it
// (§6.3.2.3 to §6.3.2.5). A labelled statement introduces what its
// statement does when that statement holds no break that leaves it
// (§6.3.2.7). Whether a statement can complete normally is decided
// syntactically by JLS §14.22 (see completes), with every break taken as
// reachable. No pattern variable outlives its statement otherwise, so none
// crosses from one member of a type body to the next. The walk that
// collects nested code's reads applies the same sets, with each pattern
// variable shadowing an enclosing local only inside its scope.
func lowerJava(l *Lowering, b *flow.Builder, fn *ts.Node, src []byte, s *Scratch) {
	j := &s.java
	j.begin(l, b, src, s.cursor(fn), &s.scope)
	defer j.end()
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

// javaLower is the state of lowering one Java callable, kept in the
// worker's Scratch and reset in place by begin for every function.
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
	// reads are the variables the current statement has read that no node
	// carries yet, in evaluation order. at[v] is the index of v's first read
	// among them while reads[at[v]] is v (see holds); a read a node took
	// away with the reads after it leaves a stale index, which holds rejects.
	reads []int32
	at    []int32
	// conds holds, innermost last, where reads stood when each conditionally
	// evaluated operand being lowered began (an `&&` or `||` right operand,
	// a conditional's arm): a read before it belongs to a consumer that runs
	// when the operand is skipped.
	conds []int
	// stack saves the enclosing statement's reads across a switch
	// expression's statements.
	stack []int32
	// results holds the owned result variable of each switch expression
	// being lowered, innermost last: its arm result nodes and its yields
	// define it.
	results []int32
	// throws counts throwing constructs evaluated by the current statement;
	// those past thrown are not yet attached to a node.
	throws, thrown int
	// first is the first node created since the last open, or -1.
	first int32
	// last is the node created last, or -1, and lastSpan its span.
	last     int32
	lastSpan flow.Span
	// labels is the stack of the labels of the labelled statements being
	// lowered; the next statement takes labels[labelAt:], and labelAt is
	// len(labels) between statements.
	labels  []string
	labelAt int
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
	// pats holds the pattern variables the expressions being lowered
	// introduce (see Scoping): an expression's sets begin where pats ended
	// before it, the when-true set first, and expr returns where the
	// when-false set begins. A variable is in scope only while scopePats has
	// bound it.
	pats []binding
	// hide indexes the bindings a switch block statement group's end takes
	// out of scope while the group's locals stay (see unbind).
	hide []int32
}

// begin resets j in place for the next function: every scalar is set anew
// and every list truncated, never reallocated, so nothing a previous
// function left behind is read. The lists holding nodes or views of the
// source (buf, labels, caseBinds, pats) clear what they drop whenever they
// shrink, so they hold none between functions.
func (j *javaLower) begin(l *Lowering, b *flow.Builder, src []byte, cur *ts.TreeCursor, binds *scope) {
	j.l, j.b, j.src, j.k, j.cur, j.binds = l, b, src, javaSyntaxOf(), cur, binds
	j.shadow, j.throws, j.thrown, j.labelAt = 0, 0, 0, 0
	j.first, j.last, j.lastSpan = -1, -1, flow.Span{}
	clear(j.buf)
	clear(j.labels)
	clear(j.caseBinds)
	clear(j.pats)
	j.buf, j.labels, j.caseBinds, j.pats = j.buf[:0], j.labels[:0], j.caseBinds[:0], j.pats[:0]
	j.reads, j.stack, j.writes, j.hide, j.results = j.reads[:0], j.stack[:0], j.writes[:0], j.hide[:0], j.results[:0]
	j.conds = j.conds[:0]
	j.arms, j.groupAt, j.frames = j.arms[:0], j.groupAt[:0], j.frames[:0]
}

// end releases the source and the per-call handles once the function is
// lowered, so the lowering state keeps nothing of the file alive.
func (j *javaLower) end() {
	j.l, j.b, j.src, j.cur, j.binds = nil, nil, nil, nil, nil
}

// javaArm is one switch label's match fringe and the index of its group.
type javaArm struct {
	group int
	fr    flow.Fringe
}

// kids pushes n's named, non-extra children onto buf and returns the stack
// mark and the list; done(mark) pops them. A list stays valid across nested
// kids calls. An extra (a comment) is left out, except an ERROR node the
// parser made an extra, which its recovery does when it wraps what it could
// not parse or a token it skipped: it is lowered as a kind the lowering does
// not name is (see Statement kinds).
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
			if x := c.Node(); x.IsNamed() && (!x.IsExtra() || x.IsError()) && (f == 0 || c.FieldId() == f) {
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

// done pops the child list kids or fieldKids pushed at mark, clearing the
// nodes it drops.
func (j *javaLower) done(mark int) {
	clear(j.buf[mark:])
	j.buf = j.buf[:mark]
}

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
	if int(v) >= len(j.at) {
		j.at = append(j.at, make([]int32, int(v)+1-len(j.at))...)
	}
	if !j.holds(v) {
		j.at[v] = int32(len(j.reads))
	}
	j.reads = append(j.reads, v)
}

// holds reports whether reads hold v, first at reads[at[v]]. A name that
// resolves to no variable of this function (v is -1: a field named without
// `this`, an undeclared name) is never held, as read never records it.
func (j *javaLower) holds(v int32) bool {
	return v >= 0 && int(v) < len(j.at) && int(j.at[v]) < len(j.reads) && j.reads[j.at[v]] == v
}

// earlier makes node id, which defines the local v and whose own reads
// begin at m, carry the reads of v the statement made before it and no node
// carries (`y = x + (x = 1)`, `f(x, x = 1)`): id Uses v's earlier value and
// defines an owned variable holding it, which replaces those reads, so
// the node folding them pairs with the definition that reached them (see
// Uses in Lowering). Only the reads made since the innermost conditionally
// evaluated operand began are handed off: their consumer runs only when id
// does, while an earlier read's consumer also runs on the path that skips
// id, where v keeps its earlier definition.
func (j *javaLower) earlier(id, v int32, m int) {
	if !j.holds(v) {
		return
	}
	from := int(j.at[v])
	if c := len(j.conds); c > 0 && j.conds[c-1] > from {
		from = j.conds[c-1]
	}
	t := int32(-1)
	for i := from; i < m; i++ {
		if j.reads[i] != v {
			continue
		}
		if t < 0 {
			j.b.Use(id, v)
			t = j.b.Var()
			j.b.Def(id, t)
			if int(t) >= len(j.at) {
				j.at = append(j.at, make([]int32, int(t)+1-len(j.at))...)
			}
			j.at[t] = int32(i)
		}
		j.reads[i] = t
	}
}

// reset starts a statement: nothing is read and no throw is pending.
func (j *javaLower) reset() {
	j.reads, j.throws, j.thrown = j.reads[:0], 0, 0
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

// def records that node n defines v, unless v is -1.
func (j *javaLower) def(n, v int32) {
	if v >= 0 {
		j.b.Def(n, v)
	}
}

// valueNode lowers n for its value and ends it with a Stmt node spanning n,
// unless the last node that lowering made already spans n, or n without its
// parentheses, with no throw evaluated after it. It returns the node that
// holds n's value; n's reads are that node's and its nested nodes', so none
// is left for a later node.
func (j *javaLower) valueNode(n *ts.Node) int32 {
	m := len(j.pats)
	id, _ := j.exprNode(n)
	j.cutPats(m)
	return id
}

// exprNode is valueNode leaving n's pattern variable sets on pats, as expr
// does, and returning where the when-false set begins too.
func (j *javaLower) exprNode(n *ts.Node) (int32, int) {
	m, last := len(j.reads), j.last
	mid := j.expr(n)
	id := j.last
	if u := j.l.unparen(n); j.last == last || j.lastSpan != spanOf(u) || j.thrown != j.throws {
		id = j.node(flow.Stmt, u, m, len(j.reads))
	}
	j.reads = j.reads[:m]
	return id, mid
}

// arm is valueNode for a conditionally evaluated operand (see earlier).
func (j *javaLower) arm(n *ts.Node) int32 {
	j.conds = append(j.conds, len(j.reads))
	id := j.valueNode(n)
	j.conds = j.conds[:len(j.conds)-1]
	return id
}

// hand makes node id, which holds the value of a nested construct whose
// reads begin at r, define a new owned result variable, and leaves the
// consumer that variable to read in place of the construct's reads, which
// id and the construct's other nodes carry (see Uses).
func (j *javaLower) hand(id int32, r int) {
	j.reads = j.reads[:r]
	res := j.b.Var()
	j.b.Def(id, res)
	j.read(res)
}

// scopePats binds pats[from:to] in the innermost scope.
func (j *javaLower) scopePats(from, to int) {
	for _, p := range j.pats[from:to] {
		j.binds.bind(p.name, p.v)
	}
}

// keep keeps one set of the expression whose sets begin at from and whose
// when-false set begins at mid: the when-true set, or the when-false set
// moved to from. It returns where the kept set ends.
func (j *javaLower) keep(from, mid int, whenTrue bool) int {
	if whenTrue {
		j.cutPats(mid)
		return mid
	}
	n := copy(j.pats[from:], j.pats[mid:])
	j.cutPats(from + n)
	return from + n
}

// cutPats truncates pats to m, clearing what it drops: a pattern variable's
// name is a view of the source.
func (j *javaLower) cutPats(m int) {
	clear(j.pats[m:])
	j.pats = j.pats[:m]
}

// swap exchanges the sets of the expression whose sets begin at from and
// whose when-false set begins at mid, the sets of its complement, and
// returns where the new when-false set begins.
func (j *javaLower) swap(from, mid int) int {
	f := len(j.pats) - mid
	slices.Reverse(j.pats[from:])
	slices.Reverse(j.pats[from : from+f])
	slices.Reverse(j.pats[from+f:])
	return from + f
}

// scoped lowers s, a statement standing alone as the body of an if, else,
// loop or label, in a scope of its own holding pats[from:to].
func (j *javaLower) scoped(s *ts.Node, from, to int) {
	mark := j.binds.mark()
	j.scopePats(from, to)
	j.stmt(s)
	j.binds.truncate(mark)
}

// introduceIf binds in the innermost scope what an if statement introduces
// (JLS §6.3.2.2; see Scoping), its condition's sets being pats[m:mid] when
// true and pats[mid:] when false, then drops both sets.
func (j *javaLower) introduceIf(cons, alt *ts.Node, m, mid int) {
	end := len(j.pats)
	switch {
	case alt == nil:
		if mid < end && !j.completes(cons) {
			j.scopePats(mid, end)
		}
	case m < end:
		c, a := j.completes(cons), j.completes(alt)
		if c && !a {
			j.scopePats(m, mid)
		} else if !c && a {
			j.scopePats(mid, end)
		}
	}
	j.cutPats(m)
}

// introduceLoop binds in the innermost scope the when-false set pats[mid:]
// of a while, do or basic for condition when body holds no break leaving it
// (JLS §6.3.2.3 to §6.3.2.5), then drops the condition's sets from m on.
func (j *javaLower) introduceLoop(body *ts.Node, m, mid int) {
	if mid < len(j.pats) && !j.breaks(body, nil) {
		j.scopePats(mid, len(j.pats))
	}
	j.cutPats(m)
}

// hideFrom records every binding from index from on for unbind.
func (j *javaLower) hideFrom(from int) {
	for i := from; i < j.binds.mark(); i++ {
		j.hide = append(j.hide, int32(i))
	}
}

// unbind ends the scope of every binding hide[hm:] indexes while the
// bindings after them stay in scope: it binds each one's name again to what
// it shadowed, or to no variable. No later binding of the group can shadow
// one of them, since Java forbids redeclaring a name within its scope (JLS
// §6.4).
func (j *javaLower) unbind(hm int) {
	for _, i := range j.hide[hm:] {
		v := int32(-1)
		if p := j.binds.shadowed(int(i)); p >= 0 {
			v = j.binds.at(p).v
		}
		j.binds.bind(j.binds.at(int(i)).name, v)
	}
	j.hide = j.hide[:hm]
}

// completes reports whether statement n can complete normally, by JLS
// §14.22 for the statements that decide it here: a jump cannot; a block can
// when its last statement can; an if with an else when either branch can; a
// while or basic for whose condition is absent or the literal true, and a
// do whose condition is that literal, only through a break whose target it
// is; a labelled statement when its statement can or a break names its
// label; synchronized when its block can; a try when its finally block, if
// any, can and its block or a catch block can. Every other statement, a
// switch statement included, is taken to complete normally, and every break
// is taken as reachable.
func (j *javaLower) completes(n *ts.Node) bool {
	k := j.k
	switch n.KindId() {
	case k.returnStmt, k.throwStmt, k.breakStmt, k.continueStmt, k.yieldStmt:
		return false
	case k.block:
		last := lastNamed(n)
		return last == nil || j.completes(last)
	case k.ifStmt:
		alt := n.ChildByFieldId(k.fAlternative)
		return alt == nil || j.completes(n.ChildByFieldId(k.fConsequence)) || j.completes(alt)
	case k.whileStmt, k.forStmt, k.doStmt:
		if c := n.ChildByFieldId(k.fCondition); c != nil && j.l.unparen(c).KindId() != k.trueLit {
			return true
		}
		return j.breaks(n.ChildByFieldId(k.fBody), n)
	case k.labeledStmt:
		body := lastNamed(n)
		return body == nil || j.completes(body) || j.breaks(body, n)
	case k.synchronizedStmt:
		return j.completes(n.ChildByFieldId(k.fBody))
	case k.tryStmt, k.tryWithResources:
		ok, fin := j.completes(n.ChildByFieldId(k.fBody)), true
		start, list := j.kids(n)
		for i := range list {
			switch c := &list[i]; c.KindId() {
			case k.catchClause:
				ok = ok || j.completes(c.ChildByFieldId(k.fBody))
			case k.finallyClause:
				if b := firstNamed(c); b != nil {
					fin = j.completes(b)
				}
			}
		}
		j.done(start)
		return ok && fin
	}
	return true
}

// breaks reports whether body holds a break statement (JLS §14.15),
// outside the callables nested in it, that leaves body when target is nil,
// or whose target is target, the statement body lies in, otherwise.
func (j *javaLower) breaks(body, target *ts.Node) bool {
	k := j.k
	loop := target != nil && j.breakable(target)
	c := j.cur
	c.Reset(*body)
	// inner counts the loops and switches between body and the node.
	inner := 0
	for {
		n := c.Node()
		if n.KindId() == k.breakStmt {
			if l := firstNamed(n); l == nil {
				if inner == 0 && (target == nil || loop) {
					return true
				}
			} else if target == nil && !j.declared(n, l, body) || target != nil && j.names(target, l) {
				return true
			}
		}
		if n.IsNamed() && !j.l.isCallable(n) && c.GotoFirstChild() {
			if j.breakable(n) {
				inner++
			}
			continue
		}
		for !c.GotoNextSibling() {
			if !c.GotoParent() {
				return false
			}
			if j.breakable(c.Node()) {
				inner--
			}
		}
	}
}

// breakable reports whether n is the target of an unlabelled break inside
// it: a loop or a switch.
func (j *javaLower) breakable(n *ts.Node) bool {
	switch k := j.k; n.KindId() {
	case k.whileStmt, k.doStmt, k.forStmt, k.enhancedFor, k.switchExpr:
		return true
	}
	return false
}

// declared reports whether label l of break brk is declared by a labelled
// statement from brk's parent up to body, body included.
func (j *javaLower) declared(brk, l, body *ts.Node) bool {
	if brk.Id() == body.Id() {
		return false
	}
	for p := brk.Parent(); p != nil; p = p.Parent() {
		if p.KindId() == j.k.labeledStmt && bytes.Equal(j.text(firstNamed(p)), j.text(l)) {
			return true
		}
		if p.Id() == body.Id() {
			return false
		}
	}
	return false
}

// names reports whether label l labels target: target is, or is directly
// wrapped by, a labelled statement naming l.
func (j *javaLower) names(target, l *ts.Node) bool {
	p := target
	if p.KindId() != j.k.labeledStmt {
		p = p.Parent()
	}
	for ; p != nil && p.KindId() == j.k.labeledStmt; p = p.Parent() {
		if bytes.Equal(j.text(firstNamed(p)), j.text(l)) {
			return true
		}
	}
	return false
}

// lastNamed is n's last named child that is not an extra, or nil.
func lastNamed(n *ts.Node) *ts.Node {
	for i := n.NamedChildCount(); i > 0; i-- {
		if c := n.NamedChild(i - 1); !c.IsExtra() {
			return c
		}
	}
	return nil
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

// stmt lowers one statement.
func (j *javaLower) stmt(n *ts.Node) {
	k := j.k
	from := j.labelAt
	labels := j.labels[from:]
	j.labelAt = len(j.labels)
	j.reset()
	// The grammar's empty statement is the anonymous `;` token; a block's
	// child list skips it, but the body of an if, else or loop is the token
	// itself, and it executes nothing (JLS §14.6).
	if !n.IsNamed() {
		return
	}
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
		j.labeled(n, from)
	case k.returnStmt, k.throwStmt, k.yieldStmt:
		if e := firstNamed(n); e != nil {
			j.value(e)
		}
		id := j.node(flow.Jump, n, 0, len(j.reads))
		switch n.KindId() {
		case k.returnStmt:
			j.b.Return()
		case k.throwStmt:
			j.b.Throw()
		default:
			// A yield hands its operand's value to the innermost switch
			// expression.
			if r := len(j.results); r > 0 {
				j.b.Def(id, j.results[r-1])
			}
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
	cons, alt := n.ChildByFieldId(k.fConsequence), n.ChildByFieldId(k.fAlternative)
	m := len(j.pats)
	mid := j.expr(cond)
	j.node(flow.Branch, cond, 0, len(j.reads))
	p := j.b.Push()
	j.scoped(cons, m, mid)
	t := j.b.Push()
	j.b.Restore(p)
	if alt != nil {
		j.scoped(alt, mid, len(j.pats))
	}
	j.b.Merge(t)
	j.b.Pop(p)
	j.introduceIf(cons, alt, m, mid)
}

// head lowers a loop condition as the loop's decision node, or as a Stmt
// node without an exit edge when it is the literal true. It reports whether
// the loop exits through it, and leaves the condition's pattern variable
// sets on pats, returning where the when-false set begins.
func (j *javaLower) head(cond *ts.Node) (exits bool, mid int) {
	j.reset()
	if cond.KindId() == j.k.trueLit {
		j.node(flow.Stmt, cond, 0, 0)
		return false, len(j.pats)
	}
	mid = j.expr(cond)
	j.node(flow.Branch, cond, 0, len(j.reads))
	return true, mid
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
	m := len(j.pats)
	exits, mid := j.head(j.l.unparen(n.ChildByFieldId(k.fCondition)))
	h := j.close(saved)
	var exit flow.Fringe
	if exits {
		exit = j.b.Push()
	}
	body := n.ChildByFieldId(k.fBody)
	j.scoped(body, m, mid)
	j.b.ContinueHere(f)
	j.loopEnd(f, h, exits, exit)
	j.introduceLoop(body, m, mid)
}

func (j *javaLower) doStmt(n *ts.Node, labels []string) {
	k := j.k
	f := j.b.OpenLoop(labels...)
	saved := j.open()
	body := n.ChildByFieldId(k.fBody)
	j.scoped(body, 0, 0)
	j.b.ContinueHere(f)
	m := len(j.pats)
	exits, mid := j.head(j.l.unparen(n.ChildByFieldId(k.fCondition)))
	h := j.close(saved)
	var exit flow.Fringe
	if exits {
		exit = j.b.Push()
	}
	j.loopEnd(f, h, exits, exit)
	j.introduceLoop(body, m, mid)
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
	exits, m := false, len(j.pats)
	mid := m
	if cond := n.ChildByFieldId(k.fCondition); cond == nil {
		j.reset()
		j.node(flow.Stmt, n.Child(0), 0, 0)
	} else {
		exits, mid = j.head(j.l.unparen(cond))
	}
	h := j.close(saved)
	var exit flow.Fringe
	if exits {
		exit = j.b.Push()
	}
	// The condition's when-true set is in scope in the body and the update
	// clause (JLS §6.3.2.5).
	j.scopePats(m, mid)
	body := n.ChildByFieldId(k.fBody)
	j.scoped(body, 0, 0)
	j.b.ContinueHere(f)
	start, ups := j.fieldKids(n, k.fUpdate)
	for i := range ups {
		j.reset()
		j.exprStmt(&ups[i])
	}
	j.done(start)
	j.loopEnd(f, h, exits, exit)
	// Only the header's locals and the when-true set end with the loop; the
	// when-false set is introduced after it.
	j.binds.truncate(mark)
	j.introduceLoop(body, m, mid)
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
	j.scoped(n.ChildByFieldId(k.fBody), 0, 0)
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
	// The selector is evaluated once, at its node, which defines an owned
	// variable every label and pattern variable node Uses.
	sv := j.b.Var()
	j.b.Def(j.node(flow.Stmt, sel, m, len(j.reads)), sv)
	j.reads = j.reads[:m]
	var f flow.Frame
	if expr {
		f = j.b.OpenBlock(yieldLabel)
	} else {
		f = j.b.OpenSwitch(labels...)
	}
	base := j.b.Push()
	aBase, cBase, gBase := len(j.arms), len(j.caseBinds), len(j.groupAt)
	start, groups := j.kids(n.ChildByFieldId(k.fBody))
	dflt, enhanced := -1, false
	for g := range groups {
		j.groupAt = append(j.groupAt, len(j.caseBinds))
		s, list := j.kids(&groups[g])
		for i := range list {
			c := &list[i]
			if c.KindId() != k.switchLabel {
				continue
			}
			if j.isDefault(c) {
				dflt = g
				continue
			}
			if j.caseLabel(c, g, sv) {
				enhanced = true
			}
		}
		j.done(s)
	}
	// One scope holds the whole switch block: a local declared in a
	// statement group is in scope in the groups after it (JLS §6.3).
	bm := j.binds.mark()
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
		// A group's pattern variables, and those its statements introduce,
		// are in scope for the rest of the group only; its locals stay for
		// the rest of the switch block. An arrow rule declares no local its
		// successors see, so its scope simply ends.
		gm, hm := j.binds.mark(), len(j.hide)
		for _, cb := range j.caseBinds[j.groupAt[gBase+g]:j.groupAt[gBase+g+1]] {
			j.binds.bind(cb.name, cb.v)
		}
		if !arrow {
			j.hideFrom(gm)
		}
		s, list := j.kids(grp)
		for i := range list {
			c := &list[i]
			before := j.binds.mark()
			switch {
			case c.KindId() == k.switchLabel:
			case arrow && expr && c.KindId() == k.expressionStmt:
				j.reset()
				j.b.Def(j.valueNode(firstNamed(c)), j.results[len(j.results)-1])
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
				if c.KindId() != k.localVarDecl {
					j.hideFrom(before)
				}
			}
		}
		j.done(s)
		if arrow {
			j.binds.truncate(gm)
		} else {
			j.unbind(hm)
		}
	}
	j.binds.truncate(bm)
	if dflt < 0 {
		if (expr || enhanced) && !n.HasError() {
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
	clear(j.caseBinds[cBase:])
	j.arms, j.caseBinds, j.groupAt = j.arms[:aBase], j.caseBinds[:cBase], j.groupAt[:gBase]
}

// isDefault reports whether switch label c matches every value: `default`,
// or `case null, default` (JLS §14.11.1), which the grammar holds as a
// `case` label whose last expression is the identifier `default`, a reserved
// word no variable can bear.
func (j *javaLower) isDefault(c *ts.Node) bool {
	if j.hasToken(c, j.k.defaultKw) {
		return true
	}
	last := lastNamed(c)
	return last != nil && last.KindId() == j.k.identifier && string(j.text(last)) == "default"
}

// caseLabel lowers one `case` label of group g: its test, its pattern
// variables and its guard, and reports whether the label makes its switch
// an enhanced one (JLS §14.11.2): a pattern, or `null`. sv is the variable
// the selector's node defines. The group's pattern variables are the label's
// and those its guard introduces when true.
func (j *javaLower) caseLabel(c *ts.Node, g int, sv int32) bool {
	k := j.k
	j.reset()
	j.read(sv)
	sel := len(j.reads)
	var pattern, guard *ts.Node
	enhanced := false
	start, list := j.kids(c)
	for i := range list {
		switch e := &list[i]; e.KindId() {
		case k.pattern:
			pattern, enhanced = e, true
		case k.guard:
			guard = e
		default:
			if e.KindId() == k.nullLit {
				enhanced = true
			}
			j.value(e)
		}
	}
	j.node(flow.Branch, c, 0, len(j.reads))
	p := j.b.Push()
	m := len(j.pats)
	if pattern != nil {
		j.bindPattern(pattern, 0, sel)
	}
	mid := len(j.pats)
	if guard != nil {
		if e := firstNamed(guard); e != nil {
			mark := j.binds.mark()
			j.scopePats(m, mid)
			j.reset()
			mid = j.expr(e)
			j.node(flow.Branch, e, 0, len(j.reads))
			j.binds.truncate(mark)
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
	j.caseBinds = append(j.caseBinds, j.pats[m:mid]...)
	j.cutPats(m)
	j.done(start)
	return enhanced
}

// bindPattern adds every variable pattern p binds to pats, a new variable
// or, in a capture walk, -1, and outside a capture walk defines each on one
// node spanning its identifier that Uses reads[from:to]. None is in scope
// until scopePats binds it.
func (j *javaLower) bindPattern(p *ts.Node, from, to int) {
	k := j.k
	switch p.KindId() {
	case k.identifier:
		v := int32(-1)
		if j.shadow == 0 {
			v = j.b.Var()
		}
		j.pats = append(j.pats, binding{name: view(j.text(p)), v: v, prev: -1})
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
// closed in reverse order (JLS §14.20.3.1).
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

// labeled hands a loop or switch its labels, labels[from:] and its own; any
// other statement is a block frame only a labelled break targets, and keeps
// what it introduces unless a break leaves it (JLS §6.3.2.7). A loop labelled
// twice takes both labels for break and continue alike; a continue naming the
// outer one is a compile-time error (JLS §14.16: its target must be the loop
// itself, not a labelled statement), so resolving it to the loop affects no
// valid program.
func (j *javaLower) labeled(n *ts.Node, from int) {
	k := j.k
	start, list := j.kids(n)
	if len(list) < 2 {
		// The labelled statement is the empty statement, the anonymous `;`
		// token, which executes nothing (JLS §14.6) and makes no node.
		j.done(start)
		return
	}
	top := len(j.labels)
	j.labels = append(j.labels, view(j.text(&list[0])))
	body := &list[len(list)-1]
	switch body.KindId() {
	case k.whileStmt, k.doStmt, k.forStmt, k.enhancedFor, k.switchExpr, k.labeledStmt:
		j.labelAt = from
		j.stmt(body)
	default:
		f := j.b.OpenBlock(j.labels[from:]...)
		mark := j.binds.mark()
		j.stmt(body)
		if j.binds.mark() > mark && j.breaks(body, nil) {
			j.binds.truncate(mark)
		}
		j.b.CloseFrame(f)
	}
	j.labels[top] = ""
	j.labels, j.labelAt = j.labels[:top], top
	j.done(start)
}

// assertStmt lowers `assert c;` and `assert c : m;`: a Branch on the
// `assert` keyword, whether assertions are enabled, whose false edge skips
// the statement (JLS §14.10), then the assertion itself.
func (j *javaLower) assertStmt(n *ts.Node) {
	start, list := j.kids(n)
	if len(list) == 0 {
		j.done(start)
		j.valueNode(n)
		return
	}
	j.node(flow.Branch, n.Child(0), 0, 0)
	skip := j.b.Push()
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
	j.b.Merge(skip)
	j.b.Pop(skip)
	j.done(start)
}

// value lowers an expression evaluated for its value: reads are recorded,
// throwing constructs counted, and nodes created for every decision, every
// conditionally evaluated operand, every definition and every nested
// callable. The pattern variables it introduces go out of scope with it.
func (j *javaLower) value(n *ts.Node) {
	m := len(j.pats)
	j.expr(n)
	j.cutPats(m)
}

// expr is value leaving the pattern variables n introduces on pats (see
// Scoping): the when-true set from where pats ended before the call, then
// the when-false set, which begins at the index it returns.
func (j *javaLower) expr(n *ts.Node) int {
	k := j.k
	m := len(j.pats)
	if j.l.isCallable(n) {
		r, w := len(j.reads), len(j.writes)
		j.shadow++
		j.cap(n)
		j.shadow--
		id := j.node(flow.Stmt, n, r, len(j.reads))
		j.closure(id, w)
		j.hand(id, r)
		return m
	}
	switch n.KindId() {
	case k.identifier:
		j.ref(n)
	case k.parenthesized:
		if e := firstNamed(n); e != nil {
			return j.expr(e)
		}
	case k.unary:
		o := n.ChildByFieldId(k.fOperand)
		if n.ChildByFieldId(k.fOperator).KindId() == k.not {
			// JLS §6.3.1.3: `!a` swaps a's sets.
			return j.swap(m, j.expr(o))
		}
		j.value(o)
	case k.binary:
		left, right := n.ChildByFieldId(k.fLeft), n.ChildByFieldId(k.fRight)
		switch op := n.ChildByFieldId(k.fOperator).KindId(); op {
		case k.and, k.or:
			// JLS §6.3.1.1, §6.3.1.2: a's when-true (&&) or when-false (||)
			// set is in scope in b, and the operator introduces the union
			// of both operands' such sets.
			and := op == k.and
			// Each operand's node defines the operator's result on the path
			// where it decides the value.
			r, res := len(j.reads), j.b.Var()
			lo := j.keep(m, j.expr(left), and)
			j.b.Def(j.node(flow.Branch, left, r, len(j.reads)), res)
			j.reads = j.reads[:r]
			mark := j.binds.mark()
			j.scopePats(m, lo)
			p := j.b.Push()
			j.conds = append(j.conds, len(j.reads))
			id, mid := j.exprNode(right)
			j.conds = j.conds[:len(j.conds)-1]
			j.b.Def(id, res)
			end := j.keep(lo, mid, and)
			j.b.Merge(p)
			j.b.Pop(p)
			j.binds.truncate(mark)
			j.read(res)
			if and {
				return end
			}
			return m
		default:
			j.value(left)
			j.value(right)
		}
	case k.ternary:
		// JLS §6.3.1.4: the condition's when-true set is in scope in the
		// second operand, its when-false set in the third.
		cond := n.ChildByFieldId(k.fCondition)
		r := len(j.reads)
		mid := j.expr(cond)
		j.node(flow.Branch, cond, r, len(j.reads))
		j.reads = j.reads[:r]
		// Each arm's node defines the conditional's result.
		res := j.b.Var()
		mark := j.binds.mark()
		p := j.b.Push()
		j.scopePats(m, mid)
		j.b.Def(j.arm(n.ChildByFieldId(k.fConsequence)), res)
		j.binds.truncate(mark)
		t := j.b.Push()
		j.b.Restore(p)
		j.scopePats(mid, len(j.pats))
		j.b.Def(j.arm(n.ChildByFieldId(k.fAlternative)), res)
		j.binds.truncate(mark)
		j.b.Merge(t)
		j.b.Pop(p)
		j.cutPats(m)
		j.read(res)
	case k.assignment, k.update:
		// An embedded assignment's value reaches its consumer through an
		// owned result, never through its target (see Uses).
		r := len(j.reads)
		if n.KindId() == k.assignment {
			j.hand(j.assign(n), r)
		} else {
			j.hand(j.update(n), r)
		}
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
			id := j.node(flow.Stmt, n, m, len(j.reads))
			j.closure(id, w)
			j.hand(id, m)
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
		// JLS §6.3.1.5: the pattern's variables are introduced when true.
		left, p := n.ChildByFieldId(k.fLeft), n.ChildByFieldId(k.fName)
		if p == nil {
			p = n.ChildByFieldId(k.fPattern)
		}
		if p == nil {
			j.value(left)
			break
		}
		// The tested value is evaluated once, at its node, which defines an
		// owned variable the pattern variable nodes and the test Use.
		tv := j.b.Var()
		j.b.Def(j.valueNode(left), tv)
		r := len(j.reads)
		j.read(tv)
		j.bindPattern(p, r, len(j.reads))
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
	return len(j.pats)
}

// switchValue lowers a switch expression inside a larger expression. Its
// arms run statements, which reset the statement's reads, so the reads of
// the enclosing statement are saved on stack around it and restored after
// it, followed by the owned result variable its arm result nodes and yields
// define, which the node consuming its value Uses (see Uses). A throwing
// construct the statement evaluated before the switch expression is carried
// by the selector's node, the first node made after it (see Exceptions), so
// none is pending after the switch and the consuming node carries only what
// its statement evaluates after it.
func (j *javaLower) switchValue(n *ts.Node) {
	base := len(j.stack)
	j.stack = append(j.stack, j.reads...)
	throws := j.throws
	res := j.b.Var()
	j.results = append(j.results, res)
	j.switchBlock(n, nil, true)
	j.results = j.results[:len(j.results)-1]
	j.reads = j.reads[:0]
	for _, v := range j.stack[base:] {
		j.read(v)
	}
	j.stack = j.stack[:base]
	j.read(res)
	j.throws, j.thrown = throws, throws
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
// and returns it.
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
		id := j.node(flow.Stmt, n, m, len(j.reads))
		j.def(id, v)
		j.earlier(id, v, m)
		return id
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
	return id
}

// update lowers `x++`, `--x` and their field and array forms as one node
// spanning n and returns it.
func (j *javaLower) update(n *ts.Node) int32 {
	k := j.k
	arg := j.l.unparen(firstNamed(n))
	m := len(j.reads)
	if arg.KindId() == k.identifier {
		v := j.lookup(arg)
		j.read(v)
		id := j.node(flow.Stmt, n, m, len(j.reads))
		j.def(id, v)
		j.earlier(id, v, m)
		return id
	}
	j.reference(arg)
	if j.targetThrows(arg) {
		j.throws++
	}
	id := j.node(flow.Stmt, n, m, len(j.reads))
	if v := j.base(arg); v >= 0 {
		j.b.MayDef(id, v)
	}
	return id
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
	case k.assignment:
		// A plain assignment writes its local target without reading it; a
		// compound one reads it too (JLS §15.26.1, §15.26.2).
		t := j.l.unparen(n.ChildByFieldId(k.fLeft))
		if t.KindId() != k.identifier {
			if v := j.base(t); v >= 0 {
				j.writes = append(j.writes, v)
			}
			j.cap(t)
		} else if n.ChildByFieldId(k.fOperator).KindId() != k.assignOp {
			j.cap(t)
		}
		j.cap(n.ChildByFieldId(k.fRight))
	case k.update:
		t := j.l.unparen(firstNamed(n))
		if t.KindId() != k.identifier {
			if v := j.base(t); v >= 0 {
				j.writes = append(j.writes, v)
			}
		}
		j.cap(t)
	case k.parenthesized, k.unary, k.binary, k.ternary, k.instanceofExpr:
		m := len(j.pats)
		j.capTest(n)
		j.cutPats(m)
	case k.guard:
		// A guard's when-true set is in scope in its rule or group, as the
		// label's pattern variables are.
		m := len(j.pats)
		if e := firstNamed(n); e != nil {
			j.scopePats(m, j.capTest(e))
		}
		j.cutPats(m)
		return
	case k.ifStmt:
		cons, alt := n.ChildByFieldId(k.fConsequence), n.ChildByFieldId(k.fAlternative)
		m := len(j.pats)
		mid := j.capTest(j.l.unparen(n.ChildByFieldId(k.fCondition)))
		j.capScoped(cons, m, mid)
		if alt != nil {
			j.capScoped(alt, mid, len(j.pats))
		}
		j.introduceIf(cons, alt, m, mid)
		return
	case k.whileStmt:
		m := len(j.pats)
		mid := j.capTest(j.l.unparen(n.ChildByFieldId(k.fCondition)))
		body := n.ChildByFieldId(k.fBody)
		j.capScoped(body, m, mid)
		j.introduceLoop(body, m, mid)
		return
	case k.doStmt:
		body := n.ChildByFieldId(k.fBody)
		j.capScoped(body, 0, 0)
		m := len(j.pats)
		j.introduceLoop(body, m, j.capTest(j.l.unparen(n.ChildByFieldId(k.fCondition))))
		return
	case k.forStmt:
		start, inits := j.fieldKids(n, k.fInit)
		for i := range inits {
			j.cap(&inits[i])
		}
		j.done(start)
		m := len(j.pats)
		mid := m
		if cond := n.ChildByFieldId(k.fCondition); cond != nil {
			mid = j.capTest(j.l.unparen(cond))
		}
		j.scopePats(m, mid)
		start, ups := j.fieldKids(n, k.fUpdate)
		for i := range ups {
			j.cap(&ups[i])
		}
		j.done(start)
		body := n.ChildByFieldId(k.fBody)
		j.capScoped(body, 0, 0)
		j.binds.truncate(mark)
		j.introduceLoop(body, m, mid)
		return
	case k.switchGroup:
		hm := len(j.hide)
		start, list := j.kids(n)
		for i := range list {
			before := j.binds.mark()
			j.cap(&list[i])
			if list[i].KindId() != k.localVarDecl {
				j.hideFrom(before)
			}
		}
		j.done(start)
		j.unbind(hm)
		return
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
		// The label is list[0]; a labelled empty statement has nothing after
		// it.
		start, list := j.kids(n)
		if len(list) > 1 {
			body := &list[len(list)-1]
			before := j.binds.mark()
			j.cap(body)
			if j.binds.mark() > before && j.breaks(body, nil) {
				j.binds.truncate(before)
			}
		}
		j.done(start)
		return
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
	case k.pattern:
		// A case label's pattern: its variables are in scope in the label's
		// guard and its rule or group.
		m := len(j.pats)
		j.bindPattern(n, 0, 0)
		j.scopePats(m, len(j.pats))
		j.cutPats(m)
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

// capTest is cap for an expression whose pattern variable sets matter (JLS
// §6.3.1), leaving them on pats as expr does and returning where the
// when-false set begins.
func (j *javaLower) capTest(n *ts.Node) int {
	k := j.k
	m := len(j.pats)
	switch n.KindId() {
	case k.parenthesized:
		if e := firstNamed(n); e != nil {
			return j.capTest(e)
		}
		return m
	case k.unary:
		o := n.ChildByFieldId(k.fOperand)
		if n.ChildByFieldId(k.fOperator).KindId() == k.not {
			return j.swap(m, j.capTest(o))
		}
		j.cap(o)
		return m
	case k.binary:
		left, right := n.ChildByFieldId(k.fLeft), n.ChildByFieldId(k.fRight)
		switch op := n.ChildByFieldId(k.fOperator).KindId(); op {
		case k.and, k.or:
			and := op == k.and
			lo := j.keep(m, j.capTest(left), and)
			mark := j.binds.mark()
			j.scopePats(m, lo)
			end := j.keep(lo, j.capTest(right), and)
			j.binds.truncate(mark)
			if and {
				return end
			}
			return m
		}
		j.cap(left)
		j.cap(right)
		return m
	case k.ternary:
		mid := j.capTest(n.ChildByFieldId(k.fCondition))
		j.capScoped(n.ChildByFieldId(k.fConsequence), m, mid)
		j.capScoped(n.ChildByFieldId(k.fAlternative), mid, len(j.pats))
		j.cutPats(m)
		return m
	case k.instanceofExpr:
		j.cap(n.ChildByFieldId(k.fLeft))
		if name := n.ChildByFieldId(k.fName); name != nil {
			j.bindPattern(name, 0, 0)
		} else if p := n.ChildByFieldId(k.fPattern); p != nil {
			j.bindPattern(p, 0, 0)
		}
		return len(j.pats)
	}
	j.cap(n)
	return len(j.pats)
}

// capScoped is cap for n in a scope of its own holding pats[from:to].
func (j *javaLower) capScoped(n *ts.Node, from, to int) {
	mark := j.binds.mark()
	j.scopePats(from, to)
	j.cap(n)
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
	annotation, markerAnnotation, classLiteral, this, super, trueLit, parenthesized, unary, nullLit,
	switchGroup uint16

	and, or, not, assignOp, defaultKw uint16

	fAlternative, fArguments, fArray, fBody, fCondition, fConsequence, fDeclarator, fIndex, fInit, fLeft,
	fName, fObject, fOperand, fOperator, fParameters, fPattern, fResources, fRight, fUpdate, fValue uint16

	// scope marks, by kind id, the statements whose declarations a capture
	// walk drops at their end; cap drops those of the statements it names
	// itself (if, while, do, for, a labelled statement and a switch block
	// statement group) by their own rules.
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
		s.parenthesized, s.unary, s.nullLit = kind("parenthesized_expression"), kind("unary_expression"), kind("null_literal")
		s.switchGroup = kind("switch_block_statement_group")
		s.and, s.or, s.not = tok("&&"), tok("||"), tok("!")
		s.assignOp, s.defaultKw = tok("="), tok("default")
		s.fAlternative, s.fArguments, s.fArray, s.fBody = field("alternative"), field("arguments"), field("array"), field("body")
		s.fCondition, s.fConsequence, s.fDeclarator = field("condition"), field("consequence"), field("declarator")
		s.fIndex, s.fInit, s.fLeft, s.fName = field("index"), field("init"), field("left"), field("name")
		s.fObject, s.fOperand, s.fOperator = field("object"), field("operand"), field("operator")
		s.fParameters = field("parameters")
		s.fPattern, s.fResources, s.fRight = field("pattern"), field("resources"), field("right")
		s.fUpdate, s.fValue = field("update"), field("value")
		s.scope = make([]bool, tl.NodeKindCount())
		for _, name := range []string{"block", "constructor_body", "switch_block", "switch_rule",
			"enhanced_for_statement", "catch_clause", "try_with_resources_statement"} {
			s.scope[kind(name)] = true
		}
		javaSyntaxTable = s
	})
	return javaSyntaxTable
}
