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
//     A variable target is a target identifier that names a variable of
//     the function, or that `:=` declares; `_`, a name that resolves to no
//     variable, and a field, index or indirect target are not variable
//     targets, whatever the statement's syntactic target count: `n, y = y, n`
//     with n resolving to no variable has one variable target, so it is one
//     node spanning the statement, which Uses the reads of both paired
//     values (y, the value paired with n; n itself reads nothing) and
//     defines y.
//   - A statement with k > 1 variable targets is k Stmt nodes, one per target,
//     spanning the target identifier and defining it. Each carries the uses
//     of its own value (its paired right-hand expression, or the whole right
//     side of a multi-value call, whose reads every target node carries) and,
//     for `op=`, its target. The may-definitions of an unpaired right side
//     ride on the first node only (see the address-taking bullet), since Go
//     evaluates it once. Go evaluates
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
//     `x.f++`, `for a[i] = range xs`) writes through its base variable (The
//     Go Programming Language Specification, "Assignment statements"): the
//     write uses the base and may-defines it, a χ (see Lowering,
//     May-definitions): a later use pairs with the write and, through it,
//     with the killing definitions of the base that reach it. For a struct
//     or array base the write is to part of the variable; for a pointer,
//     slice or map base it lands in the object the base refers to, and the
//     may-definition of the base is the conservative account of it that
//     Lowering's address-taking rule states. The variable a pointer points
//     to is not written: after `p := &x; *p = 2`, `*p = 2` may-defines p, and
//     its write to x is given up (see Lowering), so it reaches no use of x.
//     A statement's write-through may-definitions ride on its last node,
//     where every target has been written.
//   - Taking an address (`&x`, `&x.f`, `&a[i]`; the specification's
//     "Address operators") uses its operand's variables and may-defines its
//     base variable, by the address-taking rule of Lowering, on the node
//     that evaluates it, never on the last node of its statement: in a
//     statement with several targets, the node of the target whose paired
//     value holds it ("Short variable declarations": `x, y := g(&x), 2` puts
//     it on x's node), or the first node for an unpaired right side and for
//     a non-variable target's operands; a hoisted `&&`/`||` operand's own
//     Branch node, not the node that owns the expression; a select's head
//     for its channel operands and sent values ("Select statements": they are
//     evaluated on entering the select), not the clause. A function literal's
//     writes follow the same rule. A may-definition of a variable on the
//     node of its statement that assigns it, or on a later one, is dropped:
//     the assignment follows every evaluation of the statement, so it
//     overwrites what the evaluation wrote.
//   - A condition (if, for, a case value list, a type-case type list) is one
//     Branch node spanning it with every enclosing pair of parentheses
//     stripped (see Lowering, Spans): an if or for condition spans its
//     expression so, a case's value list of one value spans that value so
//     (`case (1):` spans `1`), and a list of several spans the list. Every
//     `&&`/`||` in an expression is hoisted before the node that owns the
//     expression: each operand that is not itself `&&`/`||` becomes a
//     Branch node carrying its own reads, wired by short-circuit (the left
//     operand of && reaches the right one when true and skips it when
//     false; || the reverse), and defining the expression's result variable,
//     a variable the lowering owns; the owning node Uses that variable,
//     never the operands' names (see Lowering, Uses). In value position
//     (`x := a && b`) the last operand's Branch reaches the owning node
//     whether it is true or false: its two edges are one after
//     deduplication, so it has one successor and controls nothing.
//   - A for statement without a condition (`for {`, `for i := 0; ; i++ {`)
//     has a Branch head spanning the `for` keyword whose one successor is
//     the body: the specification makes an absent condition equivalent to
//     true, so the head has no exit edge, and the loop is left only by a
//     jump. With a condition, the condition's first node is the head, the
//     target of the back edge.
//   - A range loop follows the iteration model of Lowering (the
//     specification's "For statements with range clause": the range
//     expression x is evaluated once, before beginning the loop). It is a
//     Stmt node spanning x, which Uses x's reads and defines an iteration
//     variable of the lowering's own; a Branch head spanning the `range`
//     keyword; then one node per key/value target. The head and every
//     key/value node Use the iteration variable and none of x's reads, so
//     they depend on x as it was evaluated, never on a write to its
//     variables in the body. A `_` target makes no node, since `_` is never
//     a variable (for `for _, v := range xs` the value node v follows the
//     head directly; the specification makes a trailing `_` equivalent to
//     omitting it). An identifier target, declared by `:=` or
//     assigned by `=`, spans the identifier and defines it; a field, index or
//     indirect target (`for a[i] = range xs`) spans the target, Uses its own
//     operands, which Go evaluates on every iteration as in an assignment,
//     and may-defines its base, as a write through it does. The same model
//     covers every kind of x: an integer n (Go evaluates n once and the
//     iteration values run from 0 to n-1), a channel, and a function
//     (range-over-func, `for k, v := range seq`): seq is evaluated once at
//     x's node, the loop body is the yield function Go synthesises, and each
//     call of it is one iteration, which is the loop's own control flow; a
//     function literal written as x is created at x's node, which carries
//     its captures and its writes as any creating node does.
//     The specification's one exception, "if at most one iteration variable
//     is present and x or len(x) is constant, the range expression is not
//     evaluated", is not applied: telling whether len(x) is constant needs
//     x's type, which the lowering does not have, so x's node Uses x's reads
//     in every case. That adds a pair only where nothing is read. A constant
//     name is not a variable, so a constant expression holds a variable only
//     as the operand of `len` or `cap` on an array or pointer-to-array
//     expression holding no receive or non-constant call ("Length and
//     capacity"), or of `unsafe.Sizeof`, `Alignof` or `Offsetof` ("Package
//     unsafe"), none of which evaluates it; the same holds for len(x) itself
//     when x is such an array expression (`range a`, `range len(a)`). The
//     one addition is a definition of such an operand's variable pairing
//     with x's node, and through the iteration variable with the head and
//     the one target, in a loop with at most one iteration variable. The
//     over-approximation adds pairs and gives up none.
//   - An expression switch has a Stmt head for its tag, which Go evaluates
//     exactly once ("Switch statements"), spanning the tag expression with
//     every enclosing pair of parentheses stripped (see Lowering, Spans): in
//     `switch x {` the head is `x`, never the `switch` keyword or the
//     statement. The head Uses the tag's reads and defines a variable the
//     lowering owns, holding the tag's value. A type switch's head spans
//     `x := v.(type)`, Uses v's reads, and defines the alias x, which holds
//     the switched value; without an alias it spans `v.(type)`, from the
//     guard's value through the closing parenthesis, and defines an owned
//     variable. A switch
//     without a tag, which the specification makes equivalent to `true`,
//     has no head: its first case condition is its first node, after its
//     init statement's. The head has one successor, the first case
//     condition. Every case condition (a case value list, a type-case type
//     list) is a Branch node that Uses its own values' reads and the
//     variable holding the tag, never the tag's names, since it compares
//     against the tag's value as the head evaluated it. Case conditions are
//     tested in source order with default last. `default` makes no node:
//     wherever it stands, its body is entered from the last case
//     condition's false edge (from what precedes the first case condition
//     when there is no case); with no default, that edge leaves the
//     switch.
//   - The specification declares a type switch's alias anew in the implicit
//     block of each clause ("Type switches"); the lowering declares one
//     variable, defined at the head, for all clauses. The pairs are the same:
//     a type switch admits no fallthrough ("Fallthrough statements") and a
//     goto cannot jump into a block ("Goto statements"), so no clause reaches
//     another clause's uses, and each clause's uses see the head's
//     definition as they would see their own clause's. A read in a clause
//     resolves in the clause's scope; the clause's own block already
//     declares the alias, so only a declaration in a block nested in the
//     clause shadows it.
//   - A select has a Branch head spanning the `select` keyword. Go evaluates
//     every clause's channel operand and sent value exactly once, in source
//     order, on entering the select ("Select statements"), so their `&&`/`||`
//     operands are hoisted before the head, and the head Uses their reads,
//     carries the may-definitions of what is not hoisted (a hoisted
//     operand's are on its own Branch node), and defines a variable the
//     lowering owns, holding the evaluated operands. Each clause's send or
//     receive is one node spanning the send or receive statement (`c <- v`,
//     `<-c`, `n = <-c`, `x := <-c`), which Uses that variable and none of the
//     operands' names, and Uses and writes a receive's left-hand side, which
//     Go evaluates and assigns only when the clause is chosen (a receive
//     with two variable targets is two nodes, each spanning its target
//     identifier and Using the variable). `select {}`
//     blocks forever and is lowered as a self-loop.
//   - return, break, continue, goto and fallthrough are Jump nodes spanning
//     the statement; `panic(...)` as a statement is a Jump node followed by
//     Throw. Every label is its own Stmt node spanning the label identifier,
//     before the statement it labels, so a goto always lands on it. A break
//     or continue label must be that of an enclosing for, switch or select
//     ("Break statements", "Continue statements"), so a label names the
//     frame of the statement it labels directly and no other: in `L: M:
//     for`, only M names the loop, L is a goto target only, and a break or
//     continue naming L is unresolved.
//   - A statement of a kind the lowering does not name, an ERROR node the
//     parser's recovery leaves among a block's statements included, is one
//     Stmt node spanning it, after its hoisted `&&`/`||` operands, that Uses
//     every variable read under it, so a garbled statement drops no read.
//     What it spans is the parser's recovery, not a rule of the lowering:
//     the node spans the ERROR node itself, never more, and every
//     statement the recovery keeps whole beside it is lowered as that
//     statement is: when the recovery of `g(x) y` keeps `g(x)` an
//     expression statement, the ERROR node's node spans y alone.
//
// Declarations of constants and types create no node; they only shadow.
//
// # Uses
//
// Every node follows the rule that values travel through variables (see
// Lowering). What it settles in Go:
//
//   - Lowered to nodes of their own, handing their value on through a
//     variable the lowering owns: an `&&`/`||` expression (its operand nodes
//     define the result, the owning node Uses it); an expression switch's
//     tag and a type switch's guard (the head defines it, every case
//     condition Uses it); a select's clause operands (the head defines it,
//     every clause node Uses it); a range expression (the iteration
//     variable).
//   - Folded into the node that owns the expression, whose own evaluation
//     their reads are: a function literal (Go has no node for its creation:
//     the owning node creates the value and consumes it, as `h := func()
//     {...}` does, so no result variable exists and the node Uses the
//     captures, as a creating node does); every other expression, a call, a
//     composite literal and an address-taking included; the uses of a
//     statement's field, index, indirect and `_` targets, which ride on its
//     first node. An `&&`/`||` expression the walk had not hoisted would be
//     folded too, its operands' reads and may-definitions the collecting
//     node's own, so no read is dropped; no source reaches that, since every
//     node hoists the expression it evaluates before collecting its reads.
//   - An if, for or switch init statement is a node of its own, lowered in
//     the statement's implicit block before the condition or tag; the
//     condition or tag does not Use the init statement's reads, and a
//     variable it declares resolves in that block.
//   - No node of a body Uses a condition's, a tag's, a select head's or a
//     range expression's reads, and those nodes Use nothing of the body.
//   - A go or defer statement evaluates the function value and the call's
//     parameters where it executes ("Go statements", "Defer statements"),
//     and the call runs later. Its node Uses those reads; a function literal
//     among them is created there, so its captures are the node's uses and
//     its writes are the node's may-definitions, the account of a call that
//     runs at any later point, or never.
//
// # Variables
//
// Scoping follows Go's blocks: the function block holds the receiver,
// parameters, results and the body's top level; if, for, switch and select
// statements and each case clause open implicit blocks. `:=` declares a new
// variable for each name not already declared in the innermost block (an
// inner-block `:=` shadows), after its right side is read. `_` is never a
// variable. A variable a for clause declares is one variable for the whole
// loop, although the specification gives each iteration its own copy ("For
// statements with for clause": each iteration's copy is initialised to the
// previous iteration's value before the post statement runs): the copy
// holds the value the variable had, so no read sees a different
// definition; a function literal that captures it and runs later writes
// its own iteration's copy, which the may-definition of the one variable
// covers as a superset. An identifier is a use only when it resolves to a
// variable declared in the function being lowered; package-level names,
// constants and types are not. A composite-literal key that is a bare
// identifier resolving to a variable is a use (a map key is an expression;
// a struct field key cannot be told apart without types).
//
// A name that resolves to no variable of the function (a package-level
// name, an undeclared name, a constant or type) follows Lowering's "Names
// that resolve to no variable". In Go it can stand as a target of an
// assignment of one or several targets, an `op=` target, a `++`/`--`
// operand, a range target assigned by `=`, a select receive's target, the
// base of a field, index or indirect write or of `&`, and a function
// literal's captured read or write. The filter sits at resolution:
// variable, baseVar and captured yield -1 for such a name (declare yields
// it for `_` alone), and every caller tests the variable before it reaches
// the Builder or buf, may, bases and results; assign keeps such a target
// out of its targets, so its paired value's reads ride on the statement's
// first node and it defines nothing. No list of the lowering is indexed by
// a variable. Go has no embedded assignment and no deletion of a name.
//
// A function literal is its own function, in which enclosing variables are
// free. In the enclosing function it is part of the expression that creates
// it, resolved with the literal's own declarations shadowing: the node owning
// that expression uses every enclosing variable the literal (or a literal
// nested in it) reads, and carries a may-definition of every enclosing
// variable it writes (by assignment, op=, ++/--, a range or receive target,
// through a field, index or pointer, or by taking its address). The literal
// may run at any later point, or never, so the may-definition is a χ (see
// Lowering, May-definitions): the creating node pairs with the definitions
// of the variable that reach it, whether or not the literal reads it, and a
// use after the creating node pairs with that may-definition and, through
// it, with the killing definitions that reached the creating node.
// `defer func() {...}()` is therefore one node carrying the captures.
func lowerGo(l *Lowering, b *flow.Builder, fn *ts.Node, src []byte, s *Scratch) {
	// The state is s.gol, reset in place: every list keeps its capacity, and
	// the lists holding nodes or views of the previous function's source are
	// cleared first, so nothing of it outlives its function.
	g := &s.gol
	clear(g.lefts[:cap(g.lefts)])
	clear(g.rights[:cap(g.rights)])
	clear(g.targets[:cap(g.targets)])
	clear(g.lab[:cap(g.lab)])
	shorts := g.shorts
	if shorts == nil {
		shorts = map[uintptr]int32{}
	}
	clear(shorts)
	*g = goLower{shorts: shorts, l: l, b: b, src: src, k: goSyntaxOf(), cur: s.cursor(fn), binds: &s.scope,
		marks: g.marks[:0], results: g.results[:0], buf: g.buf[:0], may: g.may[:0], bases: g.bases[:0],
		lefts: g.lefts[:0], rights: g.rights[:0], targets: g.targets[:0], hs: g.hs[:0], lab: g.lab[:0]}
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
// variable, its uses in goLower.buf[lo:hi], and the may-definitions its
// paired value makes in goLower.may[mlo:mhi].
type goTarget struct {
	id       *ts.Node
	left     int
	v        int32
	lo, hi   int
	mlo, mhi int
	isNew    bool
	done     bool
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
	binds *scope
	marks []int
	// results are the named result variables a bare return uses.
	results []int32
	// buf collects variable uses, and may the variables an evaluation
	// may-defines (the base of an address it takes, an enclosing variable a
	// function literal writes), before they are attached to the node that
	// evaluates them.
	buf, may []int32
	// bases are the base variables one assignment writes through a field,
	// index or indirect target; they are may-defined where every target has
	// been written.
	bases []int32
	// lefts, rights and targets are one statement's operands.
	lefts, rights []*ts.Node
	targets       []goTarget
	// shorts maps each `&&`/`||` expression hoist has lowered, by node id,
	// to its owned result variable, which every operand node defines and the
	// node owning the expression Uses.
	shorts map[uintptr]int32
	// hs holds the case-condition fringes of the open switches.
	hs []flow.Fringe
	// lab is the stack of the labels of the labelled statements being
	// lowered; the label of the statement being lowered is its top.
	lab []string
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

func (g *goLower) open() { g.marks = append(g.marks, g.binds.mark()) }

func (g *goLower) close() {
	top := len(g.marks) - 1
	g.binds.truncate(g.marks[top])
	g.marks = g.marks[:top]
}

// bind makes name resolve to v in the innermost block; _ binds nothing.
func (g *goLower) bind(name []byte, v int32) {
	if !g.blank(name) {
		g.binds.push(name, v)
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

// collect appends to buf every variable n reads and to may the base
// variable of every address n takes; it resolves a function literal's
// captures: its reads into buf, its writes into may. A short-circuit
// expression hoist lowered to operand nodes, which carry its reads and
// may-definitions, adds only its result variable; one hoist did not lower is
// folded, its operands' reads and may-definitions collected as the node's
// own, so no read is dropped whatever the caller. No source reaches the
// fold: every caller hoists the subtree it collects first, and hoist
// descends into every named child collect does except a function literal,
// which collect resolves through scan rather than through this case.
func (g *goLower) collect(n *ts.Node) {
	switch {
	case n.KindId() == g.k.identifier:
		if v := g.variable(n); v >= 0 {
			g.buf = append(g.buf, v)
		}
	case g.l.isCallable(n):
		g.scan(n, g.binds.mark())
	case g.isShort(n):
		if r, ok := g.shorts[n.Id()]; ok {
			g.buf = append(g.buf, r)
			return
		}
		// Not hoisted: the expression is folded into the collecting node, and
		// its operands' reads and may-definitions are that node's own.
		for i := range n.NamedChildCount() {
			g.collect(n.NamedChild(i))
		}
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
// operand nodes that each define the expression's result variable, leaving
// the fringe where the owning node begins.
func (g *goLower) hoist(n *ts.Node) {
	switch {
	case n == nil || g.l.isCallable(n):
	case g.isShort(n):
		r := g.b.Var()
		g.shorts[n.Id()] = r
		first, t, f := g.chain(n, r)
		g.b.Restore(t)
		g.b.Merge(f)
		g.b.Pop(first)
	default:
		for i := range n.NamedChildCount() {
			g.hoist(n.NamedChild(i))
		}
	}
}

// chain lowers the short-circuit expression n, whose operand nodes define
// result variable r, and returns saved fringes: t where n is true, f where it
// is false, and first, the earliest handle it pushed, which releases them
// all.
func (g *goLower) chain(n *ts.Node, r int32) (first, t, f flow.Fringe) {
	n = g.l.unparen(n)
	if !g.isShort(n) {
		g.hoist(n)
		id := g.node(flow.Branch, n)
		g.uses(id, n)
		g.b.Def(id, r)
		t = g.b.Push()
		return t, t, t
	}
	k := g.k
	and := n.ChildByFieldId(k.fOperator).KindId() == k.and
	first, lt, lf := g.chain(n.ChildByFieldId(k.fLeft), r)
	if and {
		g.b.Restore(lt)
	} else {
		g.b.Restore(lf)
	}
	_, rt, rf := g.chain(n.ChildByFieldId(k.fRight), r)
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
// returns the node. It spans e with its enclosing parentheses stripped, and a
// case's value list of one value spans that value so.
func (g *goLower) cond(e *ts.Node) int32 {
	g.hoist(e)
	at := e
	if e.KindId() == g.k.expressionList && e.NamedChildCount() == 1 {
		at = e.NamedChild(0)
	}
	id := g.node(flow.Branch, g.l.unparen(at))
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

// stmt lowers statement s; labels holds the label naming it directly, if
// any, which a for, switch or select gives its frame.
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
		g.assign(s, s.ChildByFieldId(k.fRight), s.ChildByFieldId(k.fOperator).KindId() != k.assign, false, -1)
	case k.shortVarDeclaration:
		g.fill(s.ChildByFieldId(k.fLeft), 0)
		g.assign(s, s.ChildByFieldId(k.fRight), false, true, -1)
	case k.varDeclaration:
		for i := range s.NamedChildCount() {
			switch c := s.NamedChild(i); c.KindId() {
			case k.varSpec:
				g.fill(c, k.fName)
				g.assign(s, c.ChildByFieldId(k.fValue), false, true, -1)
			case k.varSpecList:
				for j := range c.NamedChildCount() {
					if spec := c.NamedChild(j); spec.KindId() == k.varSpec {
						g.fill(spec, k.fName)
						g.assign(spec, spec.ChildByFieldId(k.fValue), false, true, -1)
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
		g.labeled(s)
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

// labeled lowers a labelled statement. Its label is its own node, the
// target of goto. A break or continue label must be that of an enclosing
// for, switch or select ("Break statements", "Continue statements"), so the
// label reaches the frame of the statement it labels directly and no other:
// in `L: M: for`, L labels the labelled statement `M: for …`, and a break or
// continue naming L is unresolved, as the compiler rejects it.
func (g *goLower) labeled(s *ts.Node) {
	k := g.k
	id := s.ChildByFieldId(k.fLabel)
	name := view(g.text(id))
	g.b.Label(name, g.span(id))
	var inner *ts.Node
	for i := range s.NamedChildCount() {
		if c := s.NamedChild(i); c.KindId() != k.labelName && c.KindId() != k.comment {
			inner = c
			break
		}
	}
	if inner != nil && inner.KindId() != k.emptyStatement {
		// The label is the top of lab, passed with its capacity cut so a
		// labelled statement nested in inner pushes past it; the frame inner
		// opens copies it, so it is popped once inner is lowered.
		top := len(g.lab)
		g.lab = append(g.lab, name)
		g.stmt(inner, g.lab[top:top+1:top+1])
		g.lab = g.lab[:top]
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
// Uses x's reads and defines an iteration variable of its own that the head
// and every key and value node, of every target form, Use, so each depends
// on x as it was evaluated before the loop, never on a definition of x in
// the body. A `_` target makes no node.
func (g *goLower) rangeLoop(rc, body *ts.Node, labels []string) {
	b, k := g.b, g.k
	right := rc.ChildByFieldId(k.fRight)
	g.hoist(right)
	rangeNode := g.node(flow.Stmt, g.l.unparen(right))
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
				id = g.node(flow.Stmt, x)
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
// The head evaluates the tag once, defines the variable holding it, and has
// one successor, the first case condition, so it is a Stmt; every case
// condition compares against the tag and Uses that variable.
func (g *goLower) switchStmt(s *ts.Node, labels []string, typed bool) {
	b, k := g.b, g.k
	g.open()
	if init := s.ChildByFieldId(k.fInitializer); init != nil {
		g.stmt(init, nil)
	}
	tag := int32(-1)
	if value := s.ChildByFieldId(k.fValue); value != nil {
		g.hoist(value)
		var head int32
		if typed {
			head = g.typeSwitchHead(s, value)
		} else {
			head = g.node(flow.Stmt, g.l.unparen(value))
		}
		// The tag is read before a type switch declares its alias, so
		// `switch x := x.(type)` reads the outer x.
		g.uses(head, value)
		if typed {
			tag = g.alias(s, head)
		}
		if tag < 0 {
			tag = b.Var()
			b.Def(head, tag)
		}
	}
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
		if tag >= 0 {
			b.Use(id, tag)
		}
		g.hs = append(g.hs, b.Push())
	}
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

// typeSwitchHead creates the Stmt node spanning `x := v.(type)`.
func (g *goLower) typeSwitchHead(s, value *ts.Node) int32 {
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
	return g.nodeSpan(flow.Stmt, flow.Span{Start: uint32(start), End: uint32(end)})
}

// alias declares type switch s's alias x, defines it on head and returns it,
// or returns -1 when s has none. The alias holds the switched value from the
// head on, and no case condition writes it, so the type cases Use it as the
// variable holding the tag.
func (g *goLower) alias(s *ts.Node, head int32) int32 {
	alias := s.ChildByFieldId(g.k.fAlias)
	if alias == nil {
		return -1
	}
	id := firstNamed(alias)
	if id == nil || id.KindId() != g.k.identifier {
		return -1
	}
	v := g.declare(id)
	if v >= 0 {
		g.b.Def(head, v)
	}
	return v
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
// select, and the choice among the clauses depends on them, so their
// short-circuit operands are hoisted before the head, the head Uses them all
// and defines the variable holding them, and every clause's node Uses it.
func (g *goLower) selectStmt(s *ts.Node, labels []string) {
	b, k := g.b, g.k
	for i := range s.NamedChildCount() {
		g.hoist(g.entered(s.NamedChild(i)))
	}
	f := b.OpenSwitch(labels...)
	head := g.node(flow.Branch, s.Child(0))
	lo, mlo := len(g.buf), len(g.may)
	for i := range s.NamedChildCount() {
		if e := g.entered(s.NamedChild(i)); e != nil {
			g.collect(e)
		}
	}
	g.useAll(head, lo, len(g.buf))
	g.mayAll(head, mlo)
	g.buf = g.buf[:lo]
	held := b.Var()
	b.Def(head, held)
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
			g.comm(comm, held)
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

// entered is what Go evaluates of select clause c on entering the select:
// a receive's right-hand side, a send statement whole, or nil for a default
// clause.
func (g *goLower) entered(c *ts.Node) *ts.Node {
	k := g.k
	if c.KindId() != k.communicationCase {
		return nil
	}
	comm := c.ChildByFieldId(k.fCommunication)
	if comm != nil && comm.KindId() == k.receiveStatement {
		return comm.ChildByFieldId(k.fRight)
	}
	return comm
}

// comm lowers a select clause's send or receive. Its channel operand and
// sent value were evaluated on entering the select, at the head, which
// defined held to hold them, so the clause's node Uses held and none of
// their names, and carries the reads and writes of a receive's left-hand
// side, which Go evaluates and assigns when the clause is chosen.
func (g *goLower) comm(c *ts.Node, held int32) {
	k := g.k
	if c.KindId() == k.receiveStatement {
		if left := c.ChildByFieldId(k.fLeft); left != nil {
			g.fill(left, 0)
			g.assign(c, c.ChildByFieldId(k.fRight), false, g.token(c, k.define) != nil, held)
			return
		}
	}
	g.b.Use(g.node(flow.Stmt, c), held)
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
// or indirect target's operands are read on the statement's first node, with
// the values paired with targets that name no variable, and it may-defines
// its base variable on the statement's last node, where every target has
// been written; a statement of at most one variable target is one node,
// which carries both. A
// may-definition an evaluation makes (an address taken, a function literal's
// write) lands on the node that evaluates it: a paired value's on its
// target's node, a non-variable target's operands' and an unpaired right
// side's on the first node. held, when not -1, is the variable a select's
// head defined when it evaluated right on entering the select: the clause's
// nodes Use it in place of right, whose reads, may-definitions and hoisted
// operands are the head's.
func (g *goLower) assign(whole, right *ts.Node, compound, define bool, held int32) {
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
	if held < 0 {
		for _, n := range g.rights {
			g.hoist(n)
		}
	}
	paired := len(g.rights) == len(g.lefts)
	lo, mlo := len(g.buf), len(g.may)
	// value collects a right-hand expression.
	value := func(n *ts.Node) {
		if held >= 0 {
			g.buf = append(g.buf, held)
			return
		}
		g.collect(n)
	}
	// Non-variable targets' operands and values come first: the statement's
	// first node carries them.
	g.targets = g.targets[:0]
	g.bases = g.bases[:0]
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
				g.bases = append(g.bases, v)
			}
		}
		if t.isNew || t.v >= 0 {
			g.targets = append(g.targets, t)
		} else if paired {
			value(g.rights[i])
		}
	}
	extra := len(g.buf)
	if !paired {
		for _, n := range g.rights {
			value(n)
		}
	}
	shared, mshared := len(g.buf), len(g.may)
	for i := range g.targets {
		t := &g.targets[i]
		if !paired {
			t.lo, t.hi, t.mlo, t.mhi = extra, shared, mshared, mshared
			continue
		}
		t.lo, t.mlo = len(g.buf), len(g.may)
		value(g.rights[t.left])
		if compound {
			g.buf = append(g.buf, t.v)
		}
		t.hi, t.mhi = len(g.buf), len(g.may)
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
		for _, v := range g.bases {
			b.MayDef(id, v)
		}
		g.buf = g.buf[:lo]
		return
	}
	g.order(lo, extra, mlo, mshared)
	g.buf, g.may = g.buf[:lo], g.may[:mlo]
}

// order emits one node per variable target (assign calls it only for more
// than one; a target that is `_`, names no variable or writes through a base
// is not among them), keeping the statement's
// invariant: every target node reads the right-hand values as they were
// before any target of the statement was written. It repeatedly takes the
// first remaining target no other remaining target reads, so no node reads
// a variable an earlier node wrote. In a cycle it takes the first remaining
// target and gives its node a use of its own variable: that use is read
// before the node's definition, so the remaining targets' reads of the
// variable, which stay, resolve to this node and through it to the old
// value. buf[lo:extra] are the first node's own uses and may[mlo:mshared]
// its may-definitions; each target's node carries its own; bases are the
// last node's. A may-definition of a variable a node of the statement has
// already defined, that node included, is dropped: the statement assigns
// the variable after every evaluation, so what the evaluation wrote is
// overwritten.
func (g *goLower) order(lo, extra, mlo, mshared int) {
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
			g.evalMay(id, mlo, mshared)
		}
		g.useAll(id, t.lo, t.hi)
		g.evalMay(id, t.mlo, t.mhi)
		if cycle {
			b.Use(id, t.v)
		}
		b.Def(id, t.v)
		if n == len(g.targets)-1 {
			for _, v := range g.bases {
				b.MayDef(id, v)
			}
		}
	}
}

// evalMay records may[lo:hi] as may-definitions of node id of a statement
// being ordered, except those of a variable a target node made so far
// defines (see order).
func (g *goLower) evalMay(id int32, lo, hi int) {
	for _, v := range g.may[lo:hi] {
		if !g.assigned(v) {
			g.b.MayDef(id, v)
		}
	}
}

// assigned reports whether a target node of the statement being ordered
// made so far defines v.
func (g *goLower) assigned(v int32) bool {
	for i := range g.targets {
		if t := &g.targets[i]; t.done && t.v == v {
			return true
		}
	}
	return false
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
		return g.binds.at(i).v
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
