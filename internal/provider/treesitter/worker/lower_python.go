package worker

import (
	"sync"

	ts "github.com/tree-sitter/go-tree-sitter"

	"github.com/Sawmonabo/codectx/internal/provider/treesitter/flow"
)

// pythonLowering lowers Python callables: the module, whose top-level code is
// one function; function definitions (async is a modifier of the kind);
// lambdas; class definitions, whose body is code run once, at definition
// time, in a scope of its own; and the four comprehensions, each an implicit
// function of its own (§6.2.4; section numbers are those of The Python
// Language Reference, version 3.13).
var pythonLowering = Lowering{
	language: "python",
	callables: []string{"module", "function_definition", "lambda", "class_definition", "list_comprehension",
		"set_comprehension", "dictionary_comprehension", "generator_expression"},
	lower: lowerPython,
}

// lowerPython lowers one Python callable's parameters and body into b.
//
// # Node granularity
//
// The goldens render nodes by source text, so the granularity is exact:
//
//   - Every parameter bound name is one defining node after Entry, spanning
//     its identifier. A default value, a decorator and a class's bases are
//     evaluated where the callable is created, so they belong to the
//     enclosing function, not to the callable's own graph.
//   - A statement is one node: an expression statement spans its expression
//     (a bare tuple statement `a, b` spans the statement), return, raise,
//     break and continue span the statement (kind Jump), an import makes one
//     defining node per bound name, spanning the local name (`import a.b`
//     binds a; `from m import *` is one Stmt node spanning the statement and
//     defines nothing), a type alias spans the statement and defines its
//     name. An assignment or augmented assignment to one identifier, one
//     attribute or one subscript is its own node spanning the statement;
//     an unpacking or a chained assignment (`a = b = e`) is one defining node
//     per target, spanning the target, in source order, each using every read
//     of the right side. An annotation without a value (`x: int`) evaluates
//     nothing and makes no node. pass, global and nonlocal make no node.
//   - `del t` (§7.5): with one target, one node spanning the statement (so
//     does `del(x)`: parentheses around a target are transparent); with
//     several, one node per target spanning it, parentheses included; the
//     elements of a tuple or list target are targets of their own, each
//     spanning itself (`del [a]` makes a node spanning a). Deleting a name
//     is a killing definition of it that carries no value, so no use after
//     it pairs with a definition before it; deleting an attribute or
//     subscript is a write through it (see Uses).
//   - A condition is one Branch node spanning it without its parentheses: if,
//     each elif (evaluated only when the conditions before it were false),
//     while. `while True:` has no exit edge, only a break leaves it: its head
//     is a Stmt node spanning True. A loop's else body is lowered from the
//     head's false edge after the loop's frame closes, so a break skips it
//     and a break or continue inside it targets the enclosing loop.
//   - `for t in e:` (§8.3) follows Iteration (see Lowering): a Stmt node
//     spanning e, evaluated once, defines the loop's iteration variable; the
//     Branch head, spanning from the start of t to the end of e (whether
//     another element is assigned), Uses only that variable and defines
//     nothing. Each name t binds is defined on the body path by the
//     unpacking rule above, each binding node Using the iteration variable
//     and none of e's reads, so the exit edge carries the definitions from
//     before the loop. async for is the same.
//   - `and`, `or` and the conditional expression: the deciding operand is a
//     Branch node spanning it, created after the nodes of everything it
//     evaluates; each conditionally evaluated operand is a Stmt node spanning
//     it. A chained comparison `a < b < c` evaluates c only when `a < b`
//     holds (§6.10): each comparison but the last is a Branch node spanning
//     its two operands, and each operand after it a Stmt node spanning it.
//   - `x := e` is a defining node spanning the assignment expression, and the
//     node evaluating the enclosing expression reads x.
//   - try (§8.4): the handler is entered through the builder's Handler node,
//     spanning the first `except` keyword; for an except* clause that is
//     `except` alone, since the grammar makes `except` and `*` two tokens
//     (`except*` renders `except`). Each except or except* clause
//     with a type is a Branch node spanning its type expression, in source
//     order. Except clauses: the first that matches runs; an exception no
//     clause matches is re-raised (Throw) from the last test's false edge,
//     and a bare `except:` catches everything and ends the chain. Except*
//     clauses (§8.4.2): every clause matching a part of the group runs, so
//     each test is reached from the previous test's false edge and from the
//     previous body's end, and after the last clause the statement both
//     completes and re-raises what no clause matched. `except E as n`
//     defines n on a node spanning n, and n is deleted however the clause
//     ends (§8.4.1): the body runs in a finally whose body is a killing
//     definition of n on a Stmt node spanning the whole clause, and whose
//     Handler spans the `as` keyword. The else body runs from the try
//     body's normal end, with the handlers closed, then joins the handler
//     exits; finally is the builder's finally, its Handler spanning the
//     `finally` keyword.
//   - with (§8.5), per item in order: the context expression's nodes, then
//     one acquiring Stmt node spanning the item (entering the manager), which
//     Uses the context expression's reads alone, defines the `as` target
//     when it is a name and may-defines a variable of the lowering's own
//     holding the manager. The rest of the statement is a finally: its
//     Handler spans the token introducing the item (the `with` keyword for
//     the first, the preceding comma for each later one), a pattern or
//     reference target is bound inside it by the unpacking rule, from the
//     item's reads (a failing assignment runs `__exit__`), each binding
//     node spanning its target (`o.p` in `with m as o.p`) and Using the
//     reads of the target's object and index itself, which the acquiring
//     node does not, since they are evaluated after `__enter__`; and its
//     exit is one Stmt node spanning from the `with` keyword to the end of
//     the item, which Uses the manager's variable only: `__exit__` is
//     called on the manager entered, not on the context expression's
//     variables, which the exit's span holds but does not evaluate again.
//     The statement after the with follows the exit when the body completed
//     normally or an exception reached the finally, because `__exit__` may
//     suppress it; a break, continue or return alone is re-issued from the
//     exit and never falls through. Items close in reverse.
//   - match (§8.6): the subjects are one Stmt node spanning them. Each case is
//     one node spanning its patterns, in source order, that Uses the
//     subjects' reads and every value its patterns read (a dotted name, a
//     class): a Branch whose false edge reaches the next case, or a Stmt with
//     no false edge when the pattern is irrefutable (a capture, the wildcard,
//     or an irrefutable group, or-pattern or as-pattern) and there is no
//     guard. Every capture is a defining node spanning the captured name, on
//     the taken path, using the subjects' reads; a guard is a Branch node
//     spanning its expression, whose false edge also reaches the next case
//     (captures made before it stay bound). A case body ends the match: there
//     is no fallthrough, and no case matching falls out of the statement.
//   - `assert c, m` (§7.3) is a Branch node spanning c whose false path is a
//     Stmt node spanning m, when present, then a Throw.
//   - A nested callable is its own function; in the enclosing function its
//     creating expression or statement is one Stmt node spanning it (a
//     decorated definition spans the decorators too, §8.7) after the nodes
//     of the parts evaluated there (decorators, defaults, bases, a
//     comprehension's first iterable), which Uses the reads of those parts
//     and every enclosing variable read inside it, and may-defines (MayDef,
//     a non-killing definition) every enclosing variable it assigns —
//     through nonlocal, through global when the enclosing function is the
//     module, or through `:=` in a comprehension (§6.12). A def or class
//     statement's node also defines its name. The node consuming the
//     created value (a return, an assignment) Uses the parts' reads, not the
//     captures (see Uses).
//   - A comprehension's graph is its clauses as nested loops: each `for`
//     clause is a Branch head spanning the clause whose false edge returns to
//     the enclosing clause's head (the outermost's leaves) and which defines
//     nothing; its target is defined on the body path by the unpacking rule,
//     one defining node per bound name spanning it, as a for statement's
//     is. A `for` clause after the first is preceded by a Stmt node spanning
//     its iterable, and its head and target nodes Use that iterable's reads;
//     the first clause's iterable is the enclosing function's, so its head
//     and target nodes Use nothing. Each `if` clause is a Branch node
//     spanning its condition whose false edge continues with the next
//     element; the element is one Stmt node spanning
//     the comprehension's body expression (a key: value pair for a
//     dictionary). The first iterable belongs to the enclosing function.
//   - A lambda's body is one node spanning it, its value.
//   - yield and await are plain expressions: their statement's node.
//   - An expression lowered for its value and ended by a node spanning it (an
//     expression statement, a conditional operand, a lambda body) takes no
//     second node when the last node its own lowering made already spans it.
//   - print and exec statements, and every kind not named here, are plain
//     nodes: one Stmt node spanning the statement with its reads, falling
//     through. Annotations are not evaluated where they stand: a function's,
//     a parameter's and an annotated assignment's type are read by no node.
//
// # Uses
//
// A node's Uses follow the consumption rule (see Lowering). Only an
// identifier resolving to a variable of this function is a Use; an
// attribute name and a keyword argument's name are not reads. What that
// rule makes of Python's constructs:
//
//   - A statement's node Uses the reads of its own evaluation, operands
//     that are nodes of their own included when their value is its value:
//     both operands of `and` and `or`, the arms of a conditional expression
//     (not its condition, a Branch node of its own), and every operand of a
//     chained comparison, which short-circuits like `and` (§6.10), so
//     `return a < b < c` Uses a, b and c.
//   - An assignment expression `x := e` carries e's reads on its own
//     defining node; the node consuming it reads x.
//   - A nested callable's creating node Uses its captures and the node
//     consuming the created value does not repeat them; the consumer still
//     Uses the reads of the parts evaluated where the callable is created
//     (a default, a base, a comprehension's first iterable), which are its
//     statement's own evaluation (see Node granularity).
//   - A node that defines v also Uses v when its statement read v before
//     it, unless the bullets above leave that read to a node of its own (a
//     capture, a condition, an assignment expression's value): in `y = x +
//     (x := 1)` the assignment expression's node Uses the x read before it.
//   - An unpacking target node Uses the variables of the value it unpacks;
//     a for target's nodes Use the iteration variable instead (see Node
//     granularity).
//   - A write through an attribute or subscript (`o.p = v`, `a[i] += v`,
//     `del o.p`) Uses the object, index and value and may-defines (MayDef)
//     the base variable of the target (o, a): the object it names is
//     changed and a later read of it sees the write, while the binding is
//     not replaced.
//   - Deleting a name reads nothing: `del x` removes the binding (§7.5),
//     so its node, and the node deleting an except clause's name, Uses
//     nothing and is a killing definition that carries no value.
//   - A match capture is a variable of the function like any other name
//     (§4.2.1), so the guard and body read the same variable and no read
//     resolves in a scope of the arm's own.
//
// # Exceptions
//
// MayThrow is given to every node whose own evaluation — the part of the
// source evaluated since the previous node — contains a call, an attribute or
// subscript read or write, `await`, `yield`, a star or double-star unpacking,
// an unpacking assignment, an import, a for loop's iterator creation or step,
// a context manager's enter or exit, a class creation, a comprehension's
// creation, or a class, mapping, sequence or dotted-value pattern; a raise
// statement's node is also a Throw. The Builder applies MayThrow only inside
// an open catch or finally frame.
//
// # Scoping
//
// A name bound anywhere in a callable's own code (§4.2.1: a parameter, an
// assignment, augmented assignment or `:=` target, a for, with or except
// target, a del target, an import, a def or class name, a type alias, a match
// capture) is local to the whole callable (§4.2.2), so each callable's own
// code is scanned for its binding sites before it is lowered; nested
// callables are not scanned, except that `:=` inside a comprehension binds in
// the nearest enclosing callable that is not a comprehension (§6.12). A name
// declared global or nonlocal in a callable is not its local (§7.12, §7.13).
// A class body's names are visible only to the class body itself, never to
// the callables nested in it (§4.2.2), and a comprehension's for targets are
// its own. A name a global statement declares anywhere in the module's
// nested functions and classes is a variable of the module, even when the
// module's own code never binds it. Blocks introduce no scope. A nested
// callable reads an enclosing variable wherever it names it, except as a
// name a plain assignment, `:=`, or a for, with, except, del or case
// capture target binds, which only writes it; an augmented assignment
// reads and writes it.
func lowerPython(l *Lowering, b *flow.Builder, fn *ts.Node, src []byte, s *Scratch) {
	j := &s.py
	j.start(l, b, fn, src, s)
	defer j.finish()
	k := j.k
	id := fn.KindId()
	j.module = id == k.module
	j.pushFrame(fn)
	switch id {
	case k.module:
		j.block(fn)
	case k.functionDefinition:
		j.params(fn.ChildByFieldId(k.fParameters))
		j.block(fn.ChildByFieldId(k.fBody))
	case k.classDefinition:
		j.block(fn.ChildByFieldId(k.fBody))
	case k.lambda:
		j.params(fn.ChildByFieldId(k.fParameters))
		j.reset()
		j.valueNode(fn.ChildByFieldId(k.fBody))
	default:
		j.comprehension(fn)
	}
}

// pyLower is the state of lowering one Python callable.
type pyLower struct {
	l   *Lowering
	b   *flow.Builder
	src []byte
	k   *pySyntax
	cur *ts.TreeCursor
	// buf is a stack of child lists; kids pushes one and done pops it.
	buf []ts.Node
	// binds is the scope chain, innermost binding last; frames marks where
	// each callable's bindings begin. frames[0] is the callable being
	// lowered; the frames above it belong to nested callables whose captures
	// are being collected, and bind -1.
	binds  *scope
	frames []pyFrame
	// module reports that the callable being lowered is the module, whose
	// variables a nested callable's global declaration names.
	module bool
	// shadow is non-zero while walking a nested callable for its captures.
	shadow int
	// walrusOnly is non-zero while a binding-site scan is inside a
	// comprehension, where only `:=` binds in the scanned callable; with
	// outerWalrus the scanned callable is that comprehension, so a `:=` name
	// is a write to an enclosing callable.
	walrusOnly  int
	outerWalrus bool
	// reads are the variables read by the current statement that its later
	// nodes may still Use, in evaluation order: a construct whose reads a
	// node of its own carries and the consumer does not repeat (a
	// callable's captures, a conditional expression's condition, an
	// assignment expression's value) truncates them away after that node.
	// seen[v] is v's first position in reads for statement stmt; pending
	// checks it against reads. stmtNo increases across every function the
	// Scratch lowers and is never reset, so an entry an earlier function
	// left in seen names another statement and marks nothing.
	reads  []int32
	seen   []pySeen
	stmtNo int
	// writes are the enclosing variables assigned inside the nested callable
	// whose captures are being collected; closure may-defines them.
	writes []int32
	// throws counts throwing constructs evaluated by the current statement;
	// those past thrown are not yet attached to a node.
	throws, thrown int
	// first is the first node created since the last open, or -1.
	first int32
	// last is the node created last, or -1, and lastSpan its span.
	last     int32
	lastSpan flow.Span
	// hold is a stack of saved fringes that a construct merges at its end:
	// the arm ends of an if chain, the case ends of a match, the handler ends
	// of a try, the false exits of a chained comparison.
	hold []flow.Fringe
	// fins are the finally frames of the with items being lowered, and mgrs
	// the variable holding each item's manager.
	fins []flow.Frame
	mgrs []int32
	// subj is a stack of the subject reads of the match statements being
	// lowered.
	subj []int32
}

// start resets j in place to lower fn: every scalar is set anew and every
// list truncated, keeping its capacity; stmtNo only advances (see seen).
func (j *pyLower) start(l *Lowering, b *flow.Builder, fn *ts.Node, src []byte, s *Scratch) {
	clear(j.buf)
	*j = pyLower{
		l: l, b: b, src: src, k: pySyntaxOf(), cur: s.cursor(fn), binds: &s.scope,
		buf: j.buf[:0], frames: j.frames[:0], reads: j.reads[:0], seen: j.seen, stmtNo: j.stmtNo + 1,
		writes: j.writes[:0], first: -1, last: -1, hold: j.hold[:0], fins: j.fins[:0], mgrs: j.mgrs[:0],
		subj: j.subj[:0],
	}
}

// finish drops j's references to the function's source and builder, so the
// Scratch holds nothing of the file past its lowering.
func (j *pyLower) finish() {
	j.l, j.b, j.src, j.cur, j.binds = nil, nil, nil, nil, nil
}

// pySeen is where a variable was first read in reads, and in which
// statement.
type pySeen struct {
	stmt int
	at   int32
}

// pyFrame is one callable's scope: its bindings start at binds[mark]. A
// class body's scope is visible only from the class body itself.
type pyFrame struct {
	mark  int
	class bool
}

// kids pushes n's named, non-extra children onto buf and returns the stack
// mark and the list; done(mark) pops them. A list stays valid across nested
// kids calls: later pushes never overwrite it.
func (j *pyLower) kids(n *ts.Node) (int, []ts.Node) {
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

// done pops the lists pushed since mark, clearing them so no node of the
// file outlives it in buf.
func (j *pyLower) done(mark int) {
	clear(j.buf[mark:])
	j.buf = j.buf[:mark]
}

func (j *pyLower) text(n *ts.Node) []byte { return textOf(j.src, n) }

// token is the span of n's first anonymous child of kind id, and whether n
// has one.
func (j *pyLower) token(n *ts.Node, id uint16) (flow.Span, bool) {
	c := j.cur
	c.Reset(*n)
	if !c.GotoFirstChild() {
		return flow.Span{}, false
	}
	for {
		if x := c.Node(); !x.IsNamed() && x.KindId() == id {
			return spanOf(x), true
		}
		if !c.GotoNextSibling() {
			return flow.Span{}, false
		}
	}
}

// pushFrame opens the scope of callable fn and binds its locals: the
// variables of the function being lowered when it is the first frame,
// shadows (-1) for a nested callable.
func (j *pyLower) pushFrame(fn *ts.Node) {
	j.frames = append(j.frames, pyFrame{mark: j.binds.mark(), class: fn.KindId() == j.k.classDefinition})
	j.collect(fn)
}

// popFrame closes the innermost scope.
func (j *pyLower) popFrame() {
	f := j.frames[len(j.frames)-1]
	j.binds.truncate(f.mark)
	j.frames = j.frames[:len(j.frames)-1]
}

// resolve is the variable name resolves to, searching the frames from index
// from outward and skipping every class frame but the innermost one. It
// follows name's shadowing chain from its innermost binding, so it visits
// only the bindings of name.
func (j *pyLower) resolve(name []byte, from int) int32 {
	inner := len(j.frames) - 1
	end := j.binds.mark()
	if from+1 < len(j.frames) {
		end = j.frames[from+1].mark
	}
	i := from
	for x := j.binds.innermost(name); x >= 0; x = j.binds.shadowed(x) {
		if x >= end {
			continue
		}
		for i >= 0 && j.frames[i].mark > x {
			i--
		}
		if i < 0 {
			return -1
		}
		if f := j.frames[i]; !f.class || i == inner {
			return j.binds.at(x).v
		}
	}
	return -1
}

// lookup resolves name from the innermost frame: its variable, or -1 when it
// is local to a nested callable or not a variable of this function.
func (j *pyLower) lookup(name *ts.Node) int32 { return j.resolve(j.text(name), len(j.frames)-1) }

// frameMark is where the innermost frame's bindings begin.
func (j *pyLower) frameMark() int { return j.frames[len(j.frames)-1].mark }

// bindAs binds name in the innermost frame to v unless it is bound there.
func (j *pyLower) bindAs(name []byte, v int32) {
	if j.binds.find(name, j.frameMark()) < 0 {
		j.binds.push(name, v)
	}
}

// site records a binding site of name in the innermost frame: a new local,
// or, when name is declared global or nonlocal there and resolves to a
// variable of this function, a write the creating node may-defines.
func (j *pyLower) site(name *ts.Node) {
	t := j.text(name)
	if i := j.binds.find(t, j.frameMark()); i >= 0 {
		if j.shadow > 0 && j.binds.at(i).v >= 0 {
			j.writes = append(j.writes, j.binds.at(i).v)
		}
		return
	}
	v := int32(-1)
	if j.shadow == 0 {
		v = j.b.Var()
	}
	j.binds.push(t, v)
}

// collect binds callable fn's locals in the innermost frame: first its
// global and nonlocal names, then its parameters, then every other binding
// site of its own code.
func (j *pyLower) collect(fn *ts.Node) {
	k := j.k
	switch fn.KindId() {
	case k.module:
		j.scanKids(fn)
		j.moduleGlobals(fn)
	case k.functionDefinition, k.classDefinition:
		body := fn.ChildByFieldId(k.fBody)
		j.declarations(body)
		if ps := fn.ChildByFieldId(k.fParameters); ps != nil {
			j.paramSites(ps)
		}
		j.scan(body)
	case k.lambda:
		if ps := fn.ChildByFieldId(k.fParameters); ps != nil {
			j.paramSites(ps)
		}
		j.scan(fn.ChildByFieldId(k.fBody))
	default:
		// A comprehension: its for targets are its own; a `:=` inside it
		// binds in an enclosing callable.
		start, list := j.kids(fn)
		for i := range list {
			if list[i].KindId() == k.forInClause {
				j.targetSites(list[i].ChildByFieldId(k.fLeft))
			}
		}
		saved := j.outerWalrus
		j.walrusOnly++
		j.outerWalrus = true
		j.compParts(list, true)
		j.walrusOnly--
		j.outerWalrus = saved
		j.done(start)
	}
}

// declarations binds the names the global and nonlocal statements of a
// callable's own statements declare: a global name to the module's variable
// when the module is being lowered and the callable is nested in it, a
// nonlocal name to what it resolves to in the enclosing function scopes.
func (j *pyLower) declarations(n *ts.Node) {
	k := j.k
	start, list := j.kids(n)
	for i := range list {
		c := &list[i]
		switch c.KindId() {
		case k.globalStatement, k.nonlocalStatement:
			global := c.KindId() == k.globalStatement
			s2, names := j.kids(c)
			for x := range names {
				t := j.text(&names[x])
				v := int32(-1)
				switch {
				case global && j.module && len(j.frames) > 1:
					v = j.frameVar(0, t)
				case !global && len(j.frames) > 1:
					v = j.resolve(t, len(j.frames)-2)
				}
				j.bindAs(t, v)
			}
			j.done(s2)
		case k.block, k.ifStatement, k.elifClause, k.elseClause, k.forStatement, k.whileStatement, k.tryStatement,
			k.exceptClause, k.finallyClause, k.withStatement, k.matchStatement, k.caseClause:
			j.declarations(c)
		}
	}
	j.done(start)
}

// moduleGlobals binds, as module variables, the names the global statements
// of every function and class nested in the module declare, at any depth: a
// global statement makes its names the module's (§7.12), so a function may
// assign a module variable the module's own code never binds. n is the
// module or one of its statements.
func (j *pyLower) moduleGlobals(n *ts.Node) {
	k := j.k
	start, list := j.kids(n)
	for i := range list {
		c := &list[i]
		switch c.KindId() {
		case k.globalStatement:
			s2, names := j.kids(c)
			for x := range names {
				j.site(&names[x])
			}
			j.done(s2)
		case k.functionDefinition, k.classDefinition:
			j.moduleGlobals(c.ChildByFieldId(k.fBody))
		case k.decoratedDefinition, k.block, k.ifStatement, k.elifClause, k.elseClause, k.forStatement,
			k.whileStatement, k.tryStatement, k.exceptClause, k.finallyClause, k.withStatement, k.matchStatement,
			k.caseClause:
			j.moduleGlobals(c)
		}
	}
	j.done(start)
}

// frameVar is name's binding in frame i alone, or -1.
func (j *pyLower) frameVar(i int, name []byte) int32 {
	end := j.binds.mark()
	if i+1 < len(j.frames) {
		end = j.frames[i+1].mark
	}
	for x := j.binds.innermost(name); x >= j.frames[i].mark; x = j.binds.shadowed(x) {
		if x < end {
			return j.binds.at(x).v
		}
	}
	return -1
}

// paramSites binds every name a parameter list binds.
func (j *pyLower) paramSites(ps *ts.Node) {
	start, list := j.kids(ps)
	for i := range list {
		if id := j.paramName(&list[i]); id != nil {
			j.targetSites(id)
		}
	}
	j.done(start)
}

// paramName is the name or pattern parameter p binds, or nil for a
// separator.
func (j *pyLower) paramName(p *ts.Node) *ts.Node {
	k := j.k
	switch p.KindId() {
	case k.identifier, k.tuplePattern:
		return p
	case k.defaultParameter, k.typedDefaultParameter:
		return p.ChildByFieldId(k.fName)
	case k.typedParameter, k.listSplatPattern, k.dictionarySplatPattern:
		if c := firstNamed(p); c != nil {
			return j.paramName(c)
		}
	}
	return nil
}

// params emits one defining node per bound parameter name, in order.
func (j *pyLower) params(ps *ts.Node) {
	if ps == nil {
		return
	}
	start, list := j.kids(ps)
	for i := range list {
		if id := j.paramName(&list[i]); id != nil {
			j.reset()
			j.bind(id, nil, 0, 0)
		}
	}
	j.done(start)
}

// defaults calls eval on every parameter default of ps, which is evaluated
// where the callable is created.
func (j *pyLower) defaults(ps *ts.Node, lower bool) {
	if ps == nil {
		return
	}
	k := j.k
	start, list := j.kids(ps)
	for i := range list {
		if id := list[i].KindId(); id == k.defaultParameter || id == k.typedDefaultParameter {
			j.eval(list[i].ChildByFieldId(k.fValue), lower)
		}
	}
	j.done(start)
}

// targetSites records the binding sites of an assignment target.
func (j *pyLower) targetSites(t *ts.Node) {
	k := j.k
	if t == nil {
		return
	}
	switch t.KindId() {
	case k.identifier:
		j.site(t)
	case k.patternList, k.tuplePattern, k.listPattern, k.tuple, k.list, k.expressionList, k.parenthesizedExpression,
		k.listSplatPattern, k.listSplat, k.asPatternTarget:
		start, list := j.kids(t)
		for i := range list {
			j.targetSites(&list[i])
		}
		j.done(start)
	default:
		j.scan(t)
	}
}

// scan records the binding sites of n, a part of the innermost frame's own
// code: it does not enter a nested callable beyond the parts evaluated where
// the callable is created, except to find the `:=` names of a comprehension.
func (j *pyLower) scan(n *ts.Node) {
	k := j.k
	if n == nil {
		return
	}
	id := n.KindId()
	if j.l.isCallable(n) {
		switch {
		case id == k.functionDefinition || id == k.classDefinition || id == k.lambda:
			if j.walrusOnly > 0 {
				return
			}
			if id != k.lambda {
				j.site(n.ChildByFieldId(k.fName))
			}
			if id == k.classDefinition {
				j.scan(n.ChildByFieldId(k.fSuperclasses))
				return
			}
			if ps := n.ChildByFieldId(k.fParameters); ps != nil {
				start, list := j.kids(ps)
				for i := range list {
					if d := list[i].KindId(); d == k.defaultParameter || d == k.typedDefaultParameter {
						j.scan(list[i].ChildByFieldId(k.fValue))
					}
				}
				j.done(start)
			}
		default:
			start, list := j.kids(n)
			if j.walrusOnly > 0 {
				for i := range list {
					j.scan(&list[i])
				}
			} else {
				j.firstIterable(list, func(r *ts.Node) { j.scan(r) })
				j.walrusOnly++
				j.compParts(list, false)
				j.walrusOnly--
			}
			j.done(start)
		}
		return
	}
	if id == k.namedExpression {
		name := n.ChildByFieldId(k.fName)
		if j.walrusOnly > 0 && j.outerWalrus {
			if v := j.resolve(j.text(name), len(j.frames)-2); j.shadow > 0 && v >= 0 {
				j.writes = append(j.writes, v)
			}
		} else {
			j.site(name)
		}
		j.scan(n.ChildByFieldId(k.fValue))
		return
	}
	if j.walrusOnly > 0 {
		j.scanKids(n)
		return
	}
	switch id {
	case k.identifier, k.globalStatement, k.nonlocalStatement, k.typeKind:
	case k.assignment, k.augmentedAssignment:
		j.targetSites(n.ChildByFieldId(k.fLeft))
		j.scan(n.ChildByFieldId(k.fRight))
	case k.forStatement:
		j.targetSites(n.ChildByFieldId(k.fLeft))
		j.scan(n.ChildByFieldId(k.fRight))
		j.scan(n.ChildByFieldId(k.fBody))
		j.scan(n.ChildByFieldId(k.fAlternative))
	case k.asPattern:
		// The `as` target of a with item or an except clause; a case
		// pattern's as-pattern is walked by pattern.
		start, list := j.kids(n)
		for i := range list {
			if list[i].KindId() == k.asPatternTarget {
				j.targetSites(&list[i])
			} else {
				j.scan(&list[i])
			}
		}
		j.done(start)
	case k.deleteStatement:
		start, list := j.kids(n)
		for i := range list {
			j.targetSites(&list[i])
		}
		j.done(start)
	case k.importStatement, k.importFromStatement, k.futureImportStatement:
		start, list := j.kids(n)
		mod := n.ChildByFieldId(k.fModuleName)
		for i := range list {
			if mod != nil && list[i].StartByte() == mod.StartByte() {
				continue
			}
			if nm := j.importName(&list[i]); nm != nil {
				j.site(nm)
			}
		}
		j.done(start)
	case k.typeAliasStatement:
		if nm := j.aliasName(n); nm != nil {
			j.site(nm)
		}
	case k.caseClause:
		start, list := j.kids(n)
		for i := range list {
			if list[i].KindId() == k.casePattern {
				j.pattern(&list[i], patSites, true)
			} else {
				j.scan(&list[i])
			}
		}
		j.done(start)
	default:
		j.scanKids(n)
	}
}

func (j *pyLower) scanKids(n *ts.Node) {
	start, list := j.kids(n)
	for i := range list {
		j.scan(&list[i])
	}
	j.done(start)
}

// firstIterable calls f with each expression of a comprehension's first
// iterable, which the enclosing callable evaluates; list is the
// comprehension's children.
func (j *pyLower) firstIterable(list []ts.Node, f func(*ts.Node)) {
	k := j.k
	for i := range list {
		if list[i].KindId() != k.forInClause {
			continue
		}
		left := list[i].ChildByFieldId(k.fLeft)
		start, parts := j.kids(&list[i])
		for p := range parts {
			if parts[p].StartByte() != left.StartByte() {
				f(&parts[p])
			}
		}
		j.done(start)
		return
	}
}

// compParts scans for binding sites (scanning, or inside a binding-site
// scan) or collects the captures of every part of a comprehension but its
// first iterable: the element, every clause's target, every later iterable
// and every condition. list is the comprehension's children.
func (j *pyLower) compParts(list []ts.Node, scanning bool) {
	k := j.k
	firstSeen := false
	for i := range list {
		c := &list[i]
		if c.KindId() != k.forInClause {
			j.part(c, scanning, false)
			continue
		}
		left := c.ChildByFieldId(k.fLeft)
		start, parts := j.kids(c)
		for p := range parts {
			isLeft := parts[p].StartByte() == left.StartByte()
			if !isLeft && !firstSeen {
				continue
			}
			j.part(&parts[p], scanning, isLeft)
		}
		j.done(start)
		firstSeen = true
	}
}

// part scans a comprehension part for binding sites, or collects its
// captures while the comprehension's frame is open; target marks a for
// clause's target.
func (j *pyLower) part(n *ts.Node, scanning, target bool) {
	switch {
	case scanning || j.walrusOnly > 0:
		j.scan(n)
	case target:
		j.capTarget(n)
	default:
		j.cap(n)
	}
}

// importName is the local name one import clause binds: an alias, or the
// first component of a dotted name; nil for a wildcard.
func (j *pyLower) importName(n *ts.Node) *ts.Node {
	k := j.k
	switch n.KindId() {
	case k.aliasedImport:
		return n.ChildByFieldId(k.fAlias)
	case k.dottedName:
		return firstNamed(n)
	}
	return nil
}

// aliasName is the name a type alias statement defines.
func (j *pyLower) aliasName(n *ts.Node) *ts.Node {
	k := j.k
	t := firstNamed(n.ChildByFieldId(k.fLeft))
	if t != nil && t.KindId() == k.genericType {
		t = firstNamed(t)
	}
	if t != nil && t.KindId() == k.identifier {
		return t
	}
	return nil
}

// ref records a read of name when it resolves to a variable of this function.
func (j *pyLower) ref(name *ts.Node) { j.read(j.lookup(name)) }

// read records a read of v, unless v is -1.
func (j *pyLower) read(v int32) {
	if v < 0 {
		return
	}
	if !j.pending(v) {
		if int(v) >= len(j.seen) {
			j.seen = append(j.seen, make([]pySeen, int(v)+1-len(j.seen))...)
		}
		j.seen[v] = pySeen{stmt: j.stmtNo, at: int32(len(j.reads))}
	}
	j.reads = append(j.reads, v)
}

// pending reports whether v is one of reads. reads is only ever cut back
// to a prefix, so v is in it exactly when its first recorded position
// still holds it.
func (j *pyLower) pending(v int32) bool {
	if int(v) >= len(j.seen) {
		return false
	}
	s := j.seen[v]
	return s.stmt == j.stmtNo && int(s.at) < len(j.reads) && j.reads[s.at] == v
}

// reset starts a statement: nothing is read and no throw is pending.
func (j *pyLower) reset() {
	j.reads, j.throws, j.thrown = j.reads[:0], 0, 0
	j.stmtNo++
}

// node creates a node spanning n that Uses reads[from:to], and MayThrow when
// a throwing construct was evaluated since the previous node.
func (j *pyLower) node(kind flow.Kind, n *ts.Node, from, to int) int32 {
	return j.nodeAt(kind, spanOf(n), from, to)
}

// nodeAt is node over the span s.
func (j *pyLower) nodeAt(kind flow.Kind, s flow.Span, from, to int) int32 {
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
// n in a read still pending, that n Uses v.
func (j *pyLower) def(n, v int32) {
	if v < 0 {
		return
	}
	j.b.Def(n, v)
	if j.pending(v) {
		j.b.Use(n, v)
	}
}

// open starts tracking the first node created; close returns it (-1 if none)
// and restores the enclosing tracking.
func (j *pyLower) open() int32 {
	s := j.first
	j.first = -1
	return s
}

func (j *pyLower) close(saved int32) int32 {
	h := j.first
	if saved >= 0 {
		j.first = saved
	}
	return h
}

// valueNode lowers n for its value and ends it with a Stmt node spanning n,
// unless the last node that lowering made already spans n, or n without its
// parentheses, with no throw evaluated after it.
func (j *pyLower) valueNode(n *ts.Node) {
	m, last := len(j.reads), j.last
	j.value(n)
	if j.last != last && j.lastSpan == spanOf(j.l.unparen(n)) && j.thrown == j.throws {
		return
	}
	j.node(flow.Stmt, n, m, len(j.reads))
}

// merge joins the fringes held since base into the current fringe and drops
// them from the hold stack; the caller pops the handles.
func (j *pyLower) merge(base int) {
	for i := len(j.hold) - 1; i >= base; i-- {
		j.b.Merge(j.hold[i])
	}
	j.hold = j.hold[:base]
}

// block lowers a statement list; Python blocks open no scope.
func (j *pyLower) block(n *ts.Node) {
	if n == nil {
		return
	}
	start, list := j.kids(n)
	for i := range list {
		j.stmt(&list[i])
	}
	j.done(start)
}

// stmt lowers one statement.
func (j *pyLower) stmt(n *ts.Node) {
	k := j.k
	j.reset()
	switch n.KindId() {
	case k.expressionStatement:
		start, list := j.kids(n)
		if len(list) == 1 {
			j.exprStmt(&list[0])
		} else {
			for i := range list {
				j.value(&list[i])
			}
			j.node(flow.Stmt, n, 0, len(j.reads))
		}
		j.done(start)
	case k.passStatement, k.globalStatement, k.nonlocalStatement:
	case k.importStatement, k.importFromStatement, k.futureImportStatement:
		j.imports(n)
	case k.typeAliasStatement:
		id := j.node(flow.Stmt, n, 0, 0)
		if nm := j.aliasName(n); nm != nil {
			j.def(id, j.lookup(nm))
		}
	case k.returnStatement:
		j.children(n, true)
		j.node(flow.Jump, n, 0, len(j.reads))
		j.b.Return()
	case k.raiseStatement:
		j.children(n, true)
		j.node(flow.Jump, n, 0, len(j.reads))
		j.b.Throw()
	case k.breakStatement:
		j.node(flow.Jump, n, 0, 0)
		j.b.Break("")
	case k.continueStatement:
		j.node(flow.Jump, n, 0, 0)
		j.b.Continue("")
	case k.deleteStatement:
		j.deleteStmt(n)
	case k.assertStatement:
		j.assertStmt(n)
	case k.ifStatement:
		j.ifStmt(n)
	case k.whileStatement:
		j.whileStmt(n)
	case k.forStatement:
		j.forStmt(n)
	case k.tryStatement:
		j.tryStmt(n)
	case k.withStatement:
		j.withStmt(n)
	case k.matchStatement:
		j.matchStmt(n)
	case k.functionDefinition, k.classDefinition:
		j.def(j.closure(n, n, len(j.reads)), j.lookup(n.ChildByFieldId(k.fName)))
	case k.decoratedDefinition:
		from := len(j.reads)
		start, list := j.kids(n)
		for i := range list {
			if list[i].KindId() == k.decorator {
				j.children(&list[i], true)
			}
		}
		j.done(start)
		d := n.ChildByFieldId(k.fDefinition)
		j.throws++ // applying the decorators calls them
		j.def(j.closure(d, n, from), j.lookup(d.ChildByFieldId(k.fName)))
	default:
		j.valueNode(n)
	}
}

// exprStmt lowers an expression evaluated for its effect.
func (j *pyLower) exprStmt(e *ts.Node) {
	k := j.k
	switch e.KindId() {
	case k.assignment:
		j.assign(e)
	case k.augmentedAssignment:
		j.augment(e)
	default:
		j.valueNode(e)
	}
}

// imports binds and defines the local names of one import statement.
func (j *pyLower) imports(n *ts.Node) {
	k := j.k
	j.throws++
	start, list := j.kids(n)
	mod := n.ChildByFieldId(k.fModuleName)
	for i := range list {
		c := &list[i]
		if mod != nil && c.StartByte() == mod.StartByte() {
			continue
		}
		if c.KindId() == k.wildcardImport {
			j.node(flow.Stmt, n, 0, 0)
			continue
		}
		if nm := j.importName(c); nm != nil {
			j.def(j.node(flow.Stmt, nm, 0, 0), j.lookup(nm))
		}
	}
	j.done(start)
}

// assign lowers `t = e`, `t1 = t2 = e` and `t: T = e`.
func (j *pyLower) assign(n *ts.Node) {
	k := j.k
	base := len(j.buf)
	r := n
	for r != nil && r.KindId() == k.assignment {
		j.buf = append(j.buf, *r.ChildByFieldId(k.fLeft))
		r = r.ChildByFieldId(k.fRight)
	}
	targets := j.buf[base:]
	if r == nil {
		// An annotation alone evaluates nothing.
		j.done(base)
		return
	}
	j.value(r)
	end := len(j.reads)
	var whole *ts.Node
	if len(targets) == 1 {
		whole = n
	}
	for i := range targets {
		j.bind(&targets[i], whole, 0, end)
	}
	j.done(base)
}

// augment lowers `t op= e`: the target is evaluated, and read, first
// (§7.2.1).
func (j *pyLower) augment(n *ts.Node) {
	k := j.k
	left, right := n.ChildByFieldId(k.fLeft), n.ChildByFieldId(k.fRight)
	if left.KindId() == k.identifier {
		j.ref(left)
		j.value(right)
		j.def(j.node(flow.Stmt, n, 0, len(j.reads)), j.lookup(left))
		return
	}
	if j.reference(left) {
		j.throws++
	}
	j.value(right)
	j.throws++
	j.mayDefBase(j.node(flow.Stmt, n, 0, len(j.reads)), left)
}

// reference evaluates an attribute's object or a subscript's value and
// indices and reports whether t is one; any other target is evaluated as a
// value.
func (j *pyLower) reference(t *ts.Node) bool {
	k := j.k
	switch t.KindId() {
	case k.attribute:
		j.value(t.ChildByFieldId(k.fObject))
	case k.subscript:
		j.children(t, true)
	default:
		j.value(t)
		return false
	}
	return true
}

// mayDefBase may-defines, on node id, the variable at the base of the
// attribute or subscript target t.
func (j *pyLower) mayDefBase(id int32, t *ts.Node) {
	k := j.k
	for t != nil {
		switch t.KindId() {
		case k.attribute:
			t = t.ChildByFieldId(k.fObject)
		case k.subscript:
			t = t.ChildByFieldId(k.fValue)
		case k.parenthesizedExpression:
			t = firstNamed(t)
		case k.identifier:
			if v := j.lookup(t); v >= 0 {
				j.b.MayDef(id, v)
			}
			return
		default:
			return
		}
	}
}

// bind lowers the binding of target t to a value whose reads are
// reads[from:to]: one node per bound name or written reference, in source
// order, each using reads[from:to] (see Node granularity). A single
// identifier, attribute or subscript target's node spans whole when it is
// not nil.
func (j *pyLower) bind(t *ts.Node, whole *ts.Node, from, to int) {
	k := j.k
	span := t
	if whole != nil {
		span = whole
	}
	switch t.KindId() {
	case k.identifier:
		j.def(j.node(flow.Stmt, span, from, to), j.lookup(t))
	case k.attribute, k.subscript:
		m := len(j.reads)
		j.reference(t)
		j.throws++
		id := j.node(flow.Stmt, span, from, to)
		for _, v := range j.reads[m:] {
			j.b.Use(id, v)
		}
		j.mayDefBase(id, t)
	case k.patternList, k.tuplePattern, k.listPattern, k.tuple, k.list, k.expressionList:
		j.throws++
		start, list := j.kids(t)
		for i := range list {
			j.bind(&list[i], nil, from, to)
		}
		j.done(start)
	case k.parenthesizedExpression, k.listSplatPattern, k.listSplat:
		if c := firstNamed(t); c != nil {
			j.bind(c, whole, from, to)
		}
	default:
		j.value(t)
		j.node(flow.Stmt, span, from, len(j.reads))
	}
}

// deleteStmt lowers `del t, …`.
func (j *pyLower) deleteStmt(n *ts.Node) {
	k := j.k
	start, list := j.kids(n)
	if len(list) == 1 && list[0].KindId() == k.expressionList {
		s2, targets := j.kids(&list[0])
		j.delete(targets, n)
		j.done(s2)
	} else {
		j.delete(list, n)
	}
	j.done(start)
}

// delete deletes each target: a name is killed, a reference written. A
// target spans stmt, the statement, when it is the statement's only one, and
// itself otherwise, parentheses included; the elements of a tuple or list
// target are targets of their own, each spanning itself.
func (j *pyLower) delete(targets []ts.Node, stmt *ts.Node) {
	k := j.k
	for i := range targets {
		t := &targets[i]
		span := t
		if len(targets) == 1 && stmt != nil {
			span = stmt
		}
		u := j.l.unparen(t)
		if u == nil {
			continue
		}
		j.reset()
		switch u.KindId() {
		case k.identifier:
			j.def(j.node(flow.Stmt, span, 0, 0), j.lookup(u))
		case k.attribute, k.subscript:
			j.reference(u)
			j.throws++
			j.mayDefBase(j.node(flow.Stmt, span, 0, len(j.reads)), u)
		case k.tuple, k.list:
			start, list := j.kids(u)
			j.delete(list, nil)
			j.done(start)
		default:
			j.value(u)
			j.node(flow.Stmt, span, 0, len(j.reads))
		}
	}
}

// assertStmt lowers `assert c, m`: when c is false, m is evaluated and
// AssertionError raised.
func (j *pyLower) assertStmt(n *ts.Node) {
	start, list := j.kids(n)
	cond := j.l.unparen(&list[0])
	j.value(cond)
	j.node(flow.Branch, cond, 0, len(j.reads))
	p := j.b.Push()
	if len(list) > 1 {
		j.reset()
		j.valueNode(&list[1])
	}
	j.b.Throw()
	j.b.Restore(p)
	j.b.Pop(p)
	j.done(start)
}

// cond lowers a condition as one Branch node spanning it.
func (j *pyLower) cond(c *ts.Node) {
	c = j.l.unparen(c)
	m := len(j.reads)
	j.value(c)
	j.node(flow.Branch, c, m, len(j.reads))
}

func (j *pyLower) ifStmt(n *ts.Node) {
	k := j.k
	j.cond(n.ChildByFieldId(k.fCondition))
	p := j.b.Push()
	base := len(j.hold)
	j.block(n.ChildByFieldId(k.fConsequence))
	j.hold = append(j.hold, j.b.Push())
	j.b.Restore(p)
	start, list := j.kids(n)
	for i := range list {
		switch alt := &list[i]; alt.KindId() {
		case k.elifClause:
			j.reset()
			j.cond(alt.ChildByFieldId(k.fCondition))
			q := j.b.Push()
			j.block(alt.ChildByFieldId(k.fConsequence))
			j.hold = append(j.hold, j.b.Push())
			j.b.Restore(q)
		case k.elseClause:
			j.block(alt.ChildByFieldId(k.fBody))
		}
	}
	j.done(start)
	j.merge(base)
	j.b.Pop(p)
}

// head lowers a while condition as the loop's decision node, or as a Stmt
// node without an exit edge when it is the literal True. It reports whether
// the loop exits through it.
func (j *pyLower) head(cond *ts.Node) bool {
	j.reset()
	cond = j.l.unparen(cond)
	if cond.KindId() == j.k.trueLit {
		j.node(flow.Stmt, cond, 0, 0)
		return false
	}
	j.cond(cond)
	return true
}

// loopEnd closes a loop whose back edge targets h: the continue target is
// the current point. The loop's frame closes before its else body, so a
// break or continue in the else body targets the enclosing loop (§8.2,
// §8.3). The else body runs from the head's false edge, saved in exit, when
// the loop exits through its head, and is unreachable otherwise; the loop's
// own breaks leave past it.
func (j *pyLower) loopEnd(n *ts.Node, f flow.Frame, h int32, exits bool, exit flow.Fringe) {
	j.b.ContinueHere(f)
	j.b.Close(h)
	if !exits {
		// The fringe is empty here: nothing leaves through the head.
		exit = j.b.Push()
	}
	j.b.CloseFrame(f)
	breaks := j.b.Push()
	j.b.Restore(exit)
	if alt := n.ChildByFieldId(j.k.fAlternative); alt != nil {
		j.block(alt.ChildByFieldId(j.k.fBody))
	}
	j.b.Merge(breaks)
	j.b.Pop(exit)
}

func (j *pyLower) whileStmt(n *ts.Node) {
	k := j.k
	f := j.b.OpenLoop()
	saved := j.open()
	exits := j.head(n.ChildByFieldId(k.fCondition))
	h := j.close(saved)
	var exit flow.Fringe
	if exits {
		exit = j.b.Push()
	}
	j.block(n.ChildByFieldId(k.fBody))
	j.loopEnd(n, f, h, exits, exit)
}

// forStmt lowers for and async for.
func (j *pyLower) forStmt(n *ts.Node) {
	k := j.k
	left, right := n.ChildByFieldId(k.fLeft), n.ChildByFieldId(k.fRight)
	j.value(right)
	j.throws++ // iter()
	it := j.b.Var()
	j.def(j.node(flow.Stmt, right, 0, len(j.reads)), it)
	f := j.b.OpenLoop()
	j.reset()
	j.throws++ // next()
	h := j.nodeAt(flow.Branch, flow.Span{Start: uint32(left.StartByte()), End: uint32(right.EndByte())}, 0, 0)
	j.b.Use(h, it)
	exit := j.b.Push()
	j.reset()
	j.read(it)
	j.bind(left, nil, 0, len(j.reads))
	j.block(n.ChildByFieldId(k.fBody))
	j.loopEnd(n, f, h, true, exit)
}

func (j *pyLower) tryStmt(n *ts.Node) {
	k := j.k
	start, list := j.kids(n)
	var firstExcept, elseC, finC *ts.Node
	for i := range list {
		switch c := &list[i]; c.KindId() {
		case k.exceptClause:
			if firstExcept == nil {
				firstExcept = c
			}
		case k.elseClause:
			elseC = c
		case k.finallyClause:
			finC = c
		}
	}
	var ff, cf flow.Frame
	if finC != nil {
		ff = j.b.OpenFinally()
	}
	if firstExcept != nil {
		cf = j.b.OpenCatch()
	}
	j.block(n.ChildByFieldId(k.fBody))
	if firstExcept != nil {
		tEnd := j.b.Push()
		j.b.EnterHandler(cf, spanOf(firstExcept.Child(0)))
		base := len(j.hold)
		// Mixing except and except* in one try is a syntax error, so the
		// first clause decides.
		if _, star := j.token(firstExcept, k.star); star {
			for i := range list {
				if c := &list[i]; c.KindId() == k.exceptClause {
					j.except(c, true)
				}
			}
			// §8.4.2: what no clause matched is re-raised after the last
			// one, and when every part matched the statement completes.
			p := j.b.Push()
			j.b.Throw()
			j.b.Restore(p)
			j.b.Pop(p)
		} else {
			caught := false
			for i := range list {
				if c := &list[i]; c.KindId() == k.exceptClause && !caught {
					caught = j.except(c, false)
				}
			}
			if !caught {
				j.b.Throw()
			}
		}
		j.merge(base)
		hx := j.b.Push()
		j.b.Restore(tEnd)
		if elseC != nil {
			j.block(elseC.ChildByFieldId(k.fBody))
		}
		j.b.Merge(hx)
		j.b.Pop(tEnd)
	} else if elseC != nil {
		j.block(elseC.ChildByFieldId(k.fBody))
	}
	if finC != nil {
		normal := j.b.EnterFinally(ff, spanOf(finC.Child(0)))
		s2, fl := j.kids(finC)
		j.block(&fl[len(fl)-1])
		j.done(s2)
		j.b.CloseFinally(ff, normal)
	}
	j.done(start)
}

// except lowers one except or except* clause on the handler fringe; star
// reports an except* clause. A typed clause's test is a Branch. An except
// clause's body's end is held and the fringe becomes the test's false edge;
// an except* clause's body's end joins that false edge, since every except*
// clause that matches a part of the group runs (§8.4.2). It reports whether
// the clause catches everything, leaving its body's end as the fringe. The
// grammar parses `except E as n` with E and n as one as-pattern value.
//
// With `as n`, n is bound first, and the body runs in a finally whose body
// is the node deleting n (§8.4.1: n is deleted however the clause ends), a
// Stmt node spanning the clause; the finally's Handler spans the `as`
// keyword.
func (j *pyLower) except(c *ts.Node, star bool) bool {
	k := j.k
	j.reset()
	// The clause's block is its last child; list stays on the stack until
	// the clause is lowered.
	start, list := j.kids(c)
	defer j.done(start)
	body := &list[len(list)-1]
	tests := list[:len(list)-1]
	if len(tests) == 0 {
		j.block(body)
		return true
	}
	var lo, hi, alias *ts.Node
	var as flow.Span
	for i := range tests {
		x := &tests[i]
		if x.KindId() == k.asPattern {
			as, _ = j.token(x, k.asKw)
			x, alias = j.asParts(x)
		}
		if lo == nil {
			lo = x
		}
		hi = x
		j.value(x)
	}
	j.nodeAt(flow.Branch, flow.Span{Start: uint32(lo.StartByte()), End: uint32(hi.EndByte())}, 0, len(j.reads))
	miss := j.b.Push()
	v := int32(-1)
	if alias != nil {
		j.reset()
		if alias.KindId() == k.identifier {
			v = j.lookup(alias)
		}
		j.bind(alias, nil, 0, 0)
	}
	if v >= 0 {
		f := j.b.OpenFinally()
		j.block(body)
		normal := j.b.EnterFinally(f, as)
		j.reset()
		j.b.Def(j.node(flow.Stmt, c, 0, 0), v)
		j.b.CloseFinally(f, normal)
	} else {
		j.block(body)
	}
	if star {
		j.b.Merge(miss)
		j.b.Pop(miss)
	} else {
		j.hold = append(j.hold, j.b.Push())
		j.b.Restore(miss)
	}
	return false
}

// asParts splits an as-pattern value `e as t` of a with item or an except
// clause into e and the target t, which the grammar wraps in an
// as_pattern_target node holding one expression.
func (j *pyLower) asParts(v *ts.Node) (e, t *ts.Node) {
	start, list := j.kids(v)
	x := list[0]
	t = firstNamed(&list[1])
	j.done(start)
	return &x, t
}

// withStmt lowers with and async with (see Node granularity).
func (j *pyLower) withStmt(n *ts.Node) {
	k := j.k
	var kw flow.Span
	var clause ts.Node
	c := j.cur
	c.Reset(*n)
	for ok := c.GotoFirstChild(); ok; ok = c.GotoNextSibling() {
		switch x := c.Node(); {
		case !x.IsNamed() && x.KindId() == k.withKw:
			kw = spanOf(x)
		case x.KindId() == k.withClause:
			clause = *x
		}
	}
	start, items := j.kids(&clause)
	base := len(j.fins)
	for i := range items {
		j.reset()
		mgr, target := j.withParts(&items[i])
		j.value(mgr)
		j.throws++ // __enter__
		a := j.node(flow.Stmt, &items[i], 0, len(j.reads))
		// The manager is defined only here, so a may-definition loses
		// nothing; a is the node's one killing definition, the target's.
		m := j.b.Var()
		j.b.MayDef(a, m)
		j.mgrs = append(j.mgrs, m)
		var t *ts.Node
		if target != nil {
			t = j.l.unparen(target)
		}
		if t != nil && t.KindId() == k.identifier {
			j.def(a, j.lookup(t))
			t = nil
		}
		j.fins = append(j.fins, j.b.OpenFinally())
		if t != nil {
			// A failing assignment to a pattern or reference target runs
			// __exit__ (§8.5), so it is bound inside the item's finally.
			j.bind(t, nil, 0, len(j.reads))
		}
	}
	j.block(n.ChildByFieldId(k.fBody))
	for i := len(items) - 1; i >= 0; i-- {
		at := kw
		if i > 0 {
			at = spanOf(items[i].PrevSibling())
		}
		normal := j.b.EnterFinally(j.fins[base+i], at)
		j.reset()
		j.throws++ // __exit__
		x := j.nodeAt(flow.Stmt, flow.Span{Start: kw.Start, End: uint32(items[i].EndByte())}, 0, 0)
		j.b.Use(x, j.mgrs[base+i])
		// __exit__ may suppress an exception that reached it, and then the
		// statement completes; a break, continue or return that reached it
		// alone is re-issued and never falls through (§8.5).
		j.b.CloseFinally(j.fins[base+i], normal || j.b.Thrown(j.fins[base+i]))
	}
	j.fins, j.mgrs = j.fins[:base], j.mgrs[:base]
	j.done(start)
}

// withParts is a with item's context expression and its `as` target, or nil.
func (j *pyLower) withParts(item *ts.Node) (mgr, target *ts.Node) {
	v := item.ChildByFieldId(j.k.fValue)
	if v.KindId() != j.k.asPattern {
		return v, nil
	}
	return j.asParts(v)
}

// pattern modes: bind a case pattern's captures as locals, read the values
// it compares against, or define its captures.
const (
	patSites = iota
	patReads
	patDefs
)

// pattern walks case pattern p in mode. capture reports that a bare dotted
// name in p's position is a capture (it is a value everywhere else: a class
// name, a mapping key).
func (j *pyLower) pattern(p *ts.Node, mode int, capture bool) {
	k := j.k
	switch p.KindId() {
	case k.dottedName:
		start, parts := j.kids(p)
		switch {
		case capture && len(parts) == 1:
			j.capture(&parts[0], mode)
		case mode == patReads:
			j.ref(&parts[0])
			if len(parts) > 1 {
				j.throws++
			}
		}
		j.done(start)
	case k.asPattern:
		start, list := j.kids(p)
		for i := range list {
			if list[i].KindId() == k.identifier {
				j.capture(&list[i], mode)
			} else {
				j.pattern(&list[i], mode, true)
			}
		}
		j.done(start)
	case k.classPattern:
		if mode == patReads {
			j.throws++
		}
		start, list := j.kids(p)
		for i := range list {
			j.pattern(&list[i], mode, i > 0)
		}
		j.done(start)
	case k.keywordPattern:
		start, list := j.kids(p)
		for i := 1; i < len(list); i++ {
			j.pattern(&list[i], mode, true)
		}
		j.done(start)
	case k.splatPattern:
		if c := firstNamed(p); c != nil {
			j.capture(c, mode)
		}
	case k.dictPattern:
		if mode == patReads {
			j.throws++
		}
		start, list := j.kids(p)
		for i := range list {
			id := list[i].KindId()
			j.pattern(&list[i], mode, id == k.casePattern || id == k.splatPattern)
		}
		j.done(start)
	case k.listPattern, k.tuplePattern:
		if mode == patReads {
			j.throws++
		}
		j.patternKids(p, mode)
	case k.casePattern, k.unionPattern:
		j.patternKids(p, mode)
	}
}

func (j *pyLower) patternKids(p *ts.Node, mode int) {
	start, list := j.kids(p)
	for i := range list {
		j.pattern(&list[i], mode, true)
	}
	j.done(start)
}

// capture handles one captured name in mode: a binding site, nothing to
// read, or a defining node using the match subjects' reads, held in
// reads[0:].
func (j *pyLower) capture(name *ts.Node, mode int) {
	switch mode {
	case patSites:
		j.site(name)
	case patDefs:
		j.def(j.node(flow.Stmt, name, 0, len(j.reads)), j.lookup(name))
	}
}

// irrefutable reports whether case pattern p always matches (§8.6.3).
func (j *pyLower) irrefutable(p *ts.Node) bool {
	k := j.k
	start, list := j.kids(p)
	r := false
	switch p.KindId() {
	case k.casePattern:
		// The wildcard `_` is the one pattern with no named node.
		if len(list) == 0 {
			r = true
		} else {
			r = len(list) == 1 && j.irrefutable(&list[0])
		}
	case k.dottedName:
		r = len(list) == 1
	case k.asPattern:
		r = len(list) > 0 && list[0].KindId() == k.casePattern && j.irrefutable(&list[0])
	case k.unionPattern:
		_, r = j.token(p, k.underscore)
		for i := range list {
			r = r || j.irrefutable(&list[i])
		}
	case k.tuplePattern:
		// A parenthesized group `(p)`, not a one-element sequence `(p,)`.
		_, comma := j.token(p, k.comma)
		r = len(list) == 1 && !comma && j.irrefutable(&list[0])
	}
	j.done(start)
	return r
}

func (j *pyLower) matchStmt(n *ts.Node) {
	k := j.k
	body := n.ChildByFieldId(k.fBody)
	start, list := j.kids(n)
	var lo, hi *ts.Node
	for i := range list {
		if list[i].StartByte() == body.StartByte() {
			continue
		}
		if lo == nil {
			lo = &list[i]
		}
		hi = &list[i]
		j.value(&list[i])
	}
	j.nodeAt(flow.Stmt, flow.Span{Start: uint32(lo.StartByte()), End: uint32(hi.EndByte())}, 0, len(j.reads))
	j.done(start)
	sb := len(j.subj)
	j.subj = append(j.subj, j.reads...)
	subj := func() {
		j.reset()
		j.reads = append(j.reads, j.subj[sb:]...)
	}
	mark := j.b.Push()
	base := len(j.hold)
	s2, cases := j.kids(body)
	for c := range cases {
		cc := &cases[c]
		if cc.KindId() != k.caseClause {
			continue
		}
		s3, parts := j.kids(cc)
		subj()
		var plo, phi *ts.Node
		for i := range parts {
			if parts[i].KindId() == k.casePattern {
				if plo == nil {
					plo = &parts[i]
				}
				phi = &parts[i]
				j.pattern(&parts[i], patReads, true)
			}
		}
		guard := cc.ChildByFieldId(k.fGuard)
		irref := plo == phi && plo != nil && j.irrefutable(plo)
		kind := flow.Branch
		if irref {
			kind = flow.Stmt
		}
		j.nodeAt(kind, flow.Span{Start: uint32(plo.StartByte()), End: uint32(phi.EndByte())}, 0, len(j.reads))
		var miss flow.Fringe
		if !irref {
			miss = j.b.Push()
		}
		subj()
		for i := range parts {
			if parts[i].KindId() == k.casePattern {
				j.pattern(&parts[i], patDefs, true)
			}
		}
		var gmiss flow.Fringe
		if guard != nil {
			j.reset()
			j.cond(firstNamed(guard))
			gmiss = j.b.Push()
		}
		j.block(cc.ChildByFieldId(k.fConsequence))
		j.done(s3)
		j.hold = append(j.hold, j.b.Push())
		switch {
		case !irref:
			j.b.Restore(miss)
			if guard != nil {
				j.b.Merge(gmiss)
			}
		case guard != nil:
			j.b.Restore(gmiss)
		}
	}
	j.done(s2)
	j.merge(base)
	j.b.Pop(mark)
	j.subj = j.subj[:sb]
}

// comprehension lowers a comprehension's own graph (see Node granularity).
func (j *pyLower) comprehension(fn *ts.Node) {
	k := j.k
	body := fn.ChildByFieldId(k.fBody)
	start, list := j.kids(fn)
	// The clauses follow the element.
	clauses := list
	for i := range list {
		if list[i].StartByte() == body.StartByte() {
			clauses = list[i+1:]
			break
		}
	}
	j.clauses(clauses, 0, body)
	j.done(start)
}

// clauses lowers comprehension clauses cs[i:] around the element body.
func (j *pyLower) clauses(cs []ts.Node, i int, body *ts.Node) {
	k := j.k
	if i == len(cs) {
		j.reset()
		j.valueNode(body)
		return
	}
	c := &cs[i]
	if c.KindId() == k.ifClause {
		j.reset()
		j.cond(firstNamed(c))
		p := j.b.Push()
		j.clauses(cs, i+1, body)
		j.b.Merge(p)
		j.b.Pop(p)
		return
	}
	left := c.ChildByFieldId(k.fLeft)
	j.reset()
	if i > 0 {
		start, parts := j.kids(c)
		var lo, hi *ts.Node
		for p := range parts {
			if parts[p].StartByte() == left.StartByte() {
				continue
			}
			if lo == nil {
				lo = &parts[p]
			}
			hi = &parts[p]
			j.value(&parts[p])
		}
		j.throws++
		j.nodeAt(flow.Stmt, flow.Span{Start: uint32(lo.StartByte()), End: uint32(hi.EndByte())}, 0, len(j.reads))
		j.done(start)
	}
	rEnd := len(j.reads)
	j.throws++
	h := j.node(flow.Branch, c, 0, rEnd)
	exit := j.b.Push()
	j.bind(left, nil, 0, rEnd)
	j.clauses(cs, i+1, body)
	j.b.Close(h)
	j.b.Restore(exit)
	j.b.Pop(exit)
}

// closure creates the node spanning at for nested callable n, after the
// nodes of the parts evaluated where n is created: it Uses the reads from
// reads[from:] on, which are those of the parts evaluated before it (a
// decorated definition's decorators) and of its defaults, bases or first
// iterable, and n's captures, and may-defines every enclosing variable n
// assigns. The captures are then dropped from reads: the node consuming
// the created value does not repeat them.
func (j *pyLower) closure(n, at *ts.Node, from int) int32 {
	w := len(j.writes)
	parts := j.nested(n, true)
	id := j.node(flow.Stmt, at, from, len(j.reads))
	j.reads = j.reads[:parts]
	for _, v := range j.writes[w:] {
		j.b.MayDef(id, v)
	}
	j.writes = j.writes[:w]
	return id
}

// eval lowers x for its value (lower) or collects its captures.
func (j *pyLower) eval(x *ts.Node, lower bool) {
	if x == nil {
		return
	}
	if lower {
		j.value(x)
	} else {
		j.cap(x)
	}
}

// nested evaluates the parts of nested callable n evaluated where it is
// created (lowered when lower, else captured), then collects the captures of
// its own code in a frame of its own. It returns the length reads had
// before the captures: every read from there on is one.
func (j *pyLower) nested(n *ts.Node, lower bool) int {
	k := j.k
	id := n.KindId()
	switch id {
	case k.functionDefinition, k.lambda:
		j.defaults(n.ChildByFieldId(k.fParameters), lower)
	case k.classDefinition:
		j.eval(n.ChildByFieldId(k.fSuperclasses), lower)
		j.throws++
	default:
		start, list := j.kids(n)
		j.firstIterable(list, func(r *ts.Node) { j.eval(r, lower) })
		j.done(start)
		j.throws++
	}
	parts := len(j.reads)
	j.shadow++
	j.pushFrame(n)
	switch id {
	case k.functionDefinition, k.classDefinition, k.lambda:
		j.cap(n.ChildByFieldId(k.fBody))
	default:
		start, list := j.kids(n)
		j.compParts(list, false)
		j.done(start)
	}
	j.popFrame()
	j.shadow--
	return parts
}

// value lowers an expression evaluated for its value: reads are recorded,
// throwing constructs counted, and nodes created for every decision, every
// conditionally evaluated operand, every definition and every nested
// callable.
func (j *pyLower) value(n *ts.Node) {
	k := j.k
	if n == nil {
		return
	}
	if j.l.isCallable(n) {
		j.closure(n, n, len(j.reads))
		return
	}
	switch n.KindId() {
	case k.identifier:
		j.ref(n)
	case k.attribute:
		j.value(n.ChildByFieldId(k.fObject))
		j.throws++
	case k.subscript, k.await, k.yield, k.listSplat, k.dictionarySplat:
		j.children(n, true)
		j.throws++
	case k.call:
		j.value(n.ChildByFieldId(k.fFunction))
		j.value(n.ChildByFieldId(k.fArguments))
		j.throws++
	case k.keywordArgument:
		j.value(n.ChildByFieldId(k.fValue))
	case k.booleanOperator:
		left := n.ChildByFieldId(k.fLeft)
		m := len(j.reads)
		j.value(left)
		j.node(flow.Branch, left, m, len(j.reads))
		p := j.b.Push()
		j.valueNode(n.ChildByFieldId(k.fRight))
		j.b.Merge(p)
		j.b.Pop(p)
	case k.conditionalExpression:
		// Children: the value if true, the condition, the value if false.
		start, list := j.kids(n)
		// The condition is a node of its own; the consumer Uses the arms.
		m := len(j.reads)
		j.value(&list[1])
		j.node(flow.Branch, &list[1], m, len(j.reads))
		j.reads = j.reads[:m]
		p := j.b.Push()
		j.valueNode(&list[0])
		t := j.b.Push()
		j.b.Restore(p)
		j.valueNode(&list[2])
		j.b.Merge(t)
		j.b.Pop(p)
		j.done(start)
	case k.comparisonOperator:
		j.comparison(n)
	case k.namedExpression:
		m := len(j.reads)
		j.value(n.ChildByFieldId(k.fValue))
		v := j.lookup(n.ChildByFieldId(k.fName))
		j.def(j.node(flow.Stmt, n, m, len(j.reads)), v)
		// The consumer reads the name, whose definition carries the
		// value's reads.
		j.reads = j.reads[:m]
		j.read(v)
	case k.typeKind:
	default:
		j.children(n, true)
	}
}

// comparison lowers a comparison; a chain of more than one short-circuits
// (§6.10).
func (j *pyLower) comparison(n *ts.Node) {
	start, ops := j.kids(n)
	if len(ops) <= 2 {
		for i := range ops {
			j.value(&ops[i])
		}
		j.done(start)
		return
	}
	var mark flow.Fringe
	s0 := len(j.reads)
	j.value(&ops[0])
	s1 := len(j.reads)
	j.value(&ops[1])
	base := len(j.hold)
	for i := 2; i < len(ops); i++ {
		j.nodeAt(flow.Branch, flow.Span{Start: uint32(ops[i-2].StartByte()), End: uint32(ops[i-1].EndByte())}, s0, len(j.reads))
		h := j.b.Push()
		if i == 2 {
			mark = h
		}
		j.hold = append(j.hold, h)
		s0, s1 = s1, len(j.reads)
		j.valueNode(&ops[i])
	}
	j.merge(base)
	j.b.Pop(mark)
	j.done(start)
}

// children walks n's named children, lowering them (lower) or collecting
// their captures.
func (j *pyLower) children(n *ts.Node, lower bool) {
	start, list := j.kids(n)
	for i := range list {
		if lower {
			j.value(&list[i])
		} else {
			j.cap(&list[i])
		}
	}
	j.done(start)
}

// cap collects the references n makes to variables of the function being
// lowered, honouring every scope n opens. A name that a plain assignment,
// `:=`, or a for, with, except or del target binds is written there, not
// read (its write was recorded when the callable's sites were scanned); an
// augmented assignment's target is read too.
func (j *pyLower) cap(n *ts.Node) {
	k := j.k
	if n == nil {
		return
	}
	if j.l.isCallable(n) {
		j.nested(n, false)
		return
	}
	switch n.KindId() {
	case k.identifier:
		j.ref(n)
	case k.assignment:
		j.capTarget(n.ChildByFieldId(k.fLeft))
		j.cap(n.ChildByFieldId(k.fRight))
	case k.namedExpression:
		j.cap(n.ChildByFieldId(k.fValue))
	case k.forStatement:
		j.capTarget(n.ChildByFieldId(k.fLeft))
		j.cap(n.ChildByFieldId(k.fRight))
		j.cap(n.ChildByFieldId(k.fBody))
		j.cap(n.ChildByFieldId(k.fAlternative))
	case k.asPattern:
		// The `as` target of a with item or an except clause; a case
		// pattern's as-pattern is walked by pattern.
		start, list := j.kids(n)
		for i := range list {
			if list[i].KindId() == k.asPatternTarget {
				j.capTarget(&list[i])
			} else {
				j.cap(&list[i])
			}
		}
		j.done(start)
	case k.deleteStatement:
		start, list := j.kids(n)
		for i := range list {
			j.capTarget(&list[i])
		}
		j.done(start)
	case k.caseClause:
		// A case pattern reads its values and binds its captures; the
		// pattern's throw count belongs to the nested callable, not to
		// the creating node.
		start, list := j.kids(n)
		for i := range list {
			if list[i].KindId() == k.casePattern {
				t := j.throws
				j.pattern(&list[i], patReads, true)
				j.throws = t
			} else {
				j.cap(&list[i])
			}
		}
		j.done(start)
	case k.attribute:
		j.cap(n.ChildByFieldId(k.fObject))
	case k.keywordArgument:
		j.cap(n.ChildByFieldId(k.fValue))
	case k.dottedName:
		// A dotted value pattern or class name reads its first component.
		j.ref(firstNamed(n))
	case k.keywordPattern:
		start, list := j.kids(n)
		for i := 1; i < len(list); i++ {
			j.cap(&list[i])
		}
		j.done(start)
	case k.importStatement, k.importFromStatement, k.futureImportStatement, k.globalStatement, k.nonlocalStatement,
		k.typeKind:
	default:
		j.children(n, false)
	}
}

// capTarget collects the references of a binding target: a name is written
// and records nothing, an attribute's object and a subscript's value and
// index are read, and a pattern's elements are targets in turn.
func (j *pyLower) capTarget(t *ts.Node) {
	k := j.k
	if t == nil {
		return
	}
	switch t.KindId() {
	case k.identifier:
	case k.attribute:
		j.cap(t.ChildByFieldId(k.fObject))
	case k.patternList, k.tuplePattern, k.listPattern, k.tuple, k.list, k.expressionList, k.parenthesizedExpression,
		k.listSplatPattern, k.listSplat, k.asPatternTarget:
		start, list := j.kids(t)
		for i := range list {
			j.capTarget(&list[i])
		}
		j.done(start)
	default:
		j.cap(t)
	}
}

// pySyntax holds the kind and field ids the Python lowering matches, resolved
// once by name against the pinned grammar so the walk compares integers
// rather than converting every node's kind to a string.
type pySyntax struct {
	module, block, expressionStatement, assignment, augmentedAssignment, namedExpression, returnStatement,
	raiseStatement, passStatement, breakStatement, continueStatement, globalStatement, nonlocalStatement,
	deleteStatement, assertStatement, importStatement, importFromStatement, futureImportStatement, aliasedImport,
	dottedName, wildcardImport, typeAliasStatement, ifStatement, elifClause, elseClause, whileStatement,
	forStatement, tryStatement, exceptClause, finallyClause, withStatement, withClause, matchStatement, caseClause,
	casePattern, functionDefinition, classDefinition, decoratedDefinition, decorator, lambda, forInClause, ifClause,
	identifier, attribute, subscript, call, keywordArgument, await, yield, listSplat, dictionarySplat,
	booleanOperator, comparisonOperator, conditionalExpression, typeKind, asPattern, asPatternTarget, patternList,
	tuplePattern, listPattern, tuple, list, expressionList, parenthesizedExpression, listSplatPattern,
	dictionarySplatPattern, typedParameter, defaultParameter, typedDefaultParameter, classPattern, keywordPattern,
	splatPattern, dictPattern, unionPattern, genericType, trueLit uint16

	withKw, comma, underscore, star, asKw uint16

	fAlias, fAlternative, fArguments, fBody, fCondition, fConsequence, fDefinition, fFunction, fGuard, fLeft,
	fModuleName, fName, fObject, fParameters, fRight, fSuperclasses, fValue uint16
}

var (
	pySyntaxOnce  sync.Once
	pySyntaxTable *pySyntax
)

// pySyntaxOf resolves the table once. A name the grammar does not define is
// a lowering defect and panics, so a misspelt kind can never silently match
// nothing.
func pySyntaxOf() *pySyntax {
	pySyntaxOnce.Do(func() {
		const language = "python"
		tl := mustGrammar(language)
		kind := func(name string) uint16 { return mustKind(tl, language, name, true) }
		tok := func(name string) uint16 { return mustKind(tl, language, name, false) }
		field := func(name string) uint16 { return mustField(tl, language, name) }
		s := &pySyntax{}
		s.module, s.block, s.expressionStatement = kind("module"), kind("block"), kind("expression_statement")
		s.assignment, s.augmentedAssignment, s.namedExpression = kind("assignment"), kind("augmented_assignment"), kind("named_expression")
		s.returnStatement, s.raiseStatement, s.passStatement = kind("return_statement"), kind("raise_statement"), kind("pass_statement")
		s.breakStatement, s.continueStatement = kind("break_statement"), kind("continue_statement")
		s.globalStatement, s.nonlocalStatement = kind("global_statement"), kind("nonlocal_statement")
		s.deleteStatement, s.assertStatement = kind("delete_statement"), kind("assert_statement")
		s.importStatement, s.importFromStatement = kind("import_statement"), kind("import_from_statement")
		s.futureImportStatement, s.aliasedImport = kind("future_import_statement"), kind("aliased_import")
		s.dottedName, s.wildcardImport, s.typeAliasStatement = kind("dotted_name"), kind("wildcard_import"), kind("type_alias_statement")
		s.ifStatement, s.elifClause, s.elseClause = kind("if_statement"), kind("elif_clause"), kind("else_clause")
		s.whileStatement, s.forStatement, s.tryStatement = kind("while_statement"), kind("for_statement"), kind("try_statement")
		s.exceptClause, s.finallyClause = kind("except_clause"), kind("finally_clause")
		s.withStatement, s.withClause = kind("with_statement"), kind("with_clause")
		s.matchStatement, s.caseClause, s.casePattern = kind("match_statement"), kind("case_clause"), kind("case_pattern")
		s.functionDefinition, s.classDefinition = kind("function_definition"), kind("class_definition")
		s.decoratedDefinition, s.decorator, s.lambda = kind("decorated_definition"), kind("decorator"), kind("lambda")
		s.forInClause, s.ifClause, s.identifier = kind("for_in_clause"), kind("if_clause"), kind("identifier")
		s.attribute, s.subscript, s.call = kind("attribute"), kind("subscript"), kind("call")
		s.keywordArgument, s.await, s.yield = kind("keyword_argument"), kind("await"), kind("yield")
		s.listSplat, s.dictionarySplat = kind("list_splat"), kind("dictionary_splat")
		s.booleanOperator, s.comparisonOperator = kind("boolean_operator"), kind("comparison_operator")
		s.conditionalExpression, s.typeKind = kind("conditional_expression"), kind("type")
		s.asPattern, s.asPatternTarget, s.patternList = kind("as_pattern"), kind("as_pattern_target"), kind("pattern_list")
		s.tuplePattern, s.listPattern, s.tuple, s.list = kind("tuple_pattern"), kind("list_pattern"), kind("tuple"), kind("list")
		s.expressionList, s.parenthesizedExpression = kind("expression_list"), kind("parenthesized_expression")
		s.listSplatPattern, s.dictionarySplatPattern = kind("list_splat_pattern"), kind("dictionary_splat_pattern")
		s.typedParameter, s.defaultParameter = kind("typed_parameter"), kind("default_parameter")
		s.typedDefaultParameter, s.classPattern = kind("typed_default_parameter"), kind("class_pattern")
		s.keywordPattern, s.splatPattern, s.dictPattern = kind("keyword_pattern"), kind("splat_pattern"), kind("dict_pattern")
		s.unionPattern, s.genericType, s.trueLit = kind("union_pattern"), kind("generic_type"), kind("true")
		s.withKw, s.comma, s.underscore = tok("with"), tok(","), tok("_")
		// The `*` of `except*` is a token of its own that the grammar maps to
		// the one public `*` kind.
		s.star, s.asKw = tok("*"), tok("as")
		s.fAlias, s.fAlternative, s.fArguments = field("alias"), field("alternative"), field("arguments")
		s.fBody, s.fCondition, s.fConsequence = field("body"), field("condition"), field("consequence")
		s.fDefinition, s.fFunction, s.fGuard, s.fLeft = field("definition"), field("function"), field("guard"), field("left")
		s.fModuleName, s.fName, s.fObject = field("module_name"), field("name"), field("object")
		s.fParameters, s.fRight, s.fSuperclasses, s.fValue = field("parameters"), field("right"), field("superclasses"), field("value")
		pySyntaxTable = s
	})
	return pySyntaxTable
}
