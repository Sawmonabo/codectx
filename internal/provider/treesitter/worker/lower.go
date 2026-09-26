package worker

import (
	"sync"
	"unsafe"

	ts "github.com/tree-sitter/go-tree-sitter"

	"github.com/Sawmonabo/codectx/internal/provider/treesitter/flow"
)

// Grammar returns the pinned parse-tree language registered under name, the
// same table the worker parses with.
func Grammar(name string) (*ts.Language, bool) {
	g, ok := grammars[name]
	if !ok {
		return nil, false
	}
	return g.tsLanguage(), true
}

// Lowering turns one language's callables into flow graphs. A callable is a
// function, method, function-valued literal, or a unit a language runs as
// code of its own (a JavaScript class body: its field initializers and static
// blocks); each one, nested or not, is its own function, and its enclosing
// function sees only the expression that creates it. A Lowering holds no
// per-function state and is safe to share.
//
// # Uses: values travel through variables
//
// Every lowering applies one rule for which variables a node Uses. A value
// computed at one node and used at another always travels through a
// variable: a named one, or one the lowering owns (flow.Builder.Var, bound to
// no name). No node re-reads names on another node's behalf. A node Uses
// what its own evaluation reads: the reads that no node of their own
// carries.
//
// A nested value-producing construct that a lowering lowers to nodes of its
// own hands its value to the node consuming it through an owned result
// variable. Every node that yields the value defines that variable, and the
// consumer Uses it:
//
//   - a switch or match expression: each arm's result node, and the `yield`
//     or valued `break` that leaves it;
//   - a conditional (`c ? a : b`, a Python `a if c else b`, a valued `if` in
//     Rust): each arm's result node;
//   - a block, loop or labelled block consumed as a value: its tail node,
//     and each valued `break` that leaves it;
//   - a try block consumed as a value: its tail node, and each `?` that
//     completes it with its error;
//   - a GNU C statement expression: the node of its last statement;
//   - a short-circuit operator (`&&`, `||`, `and`, `or`, `??`) in value
//     position: each operand node, on the path where it decides the value:
//     the deciding operand's node defines the variable, and the operand
//     evaluated after it defines it again, so SSA merges the two;
//   - an embedded assignment (`y = (x = a) * 2`, `f(o.f = a)`, `x := e`): the
//     assignment's node. A local target is the node's definition, so the
//     node may-defines the owned result (see below); a field, element or
//     pointer target only may-defines its base, and the node defines the
//     result. The consumer Uses the result in both cases, never the target:
//     in `(x := 1) + (x := 2)` it depends on both assignments;
//   - a callable's creation (a lambda, closure, local function, anonymous
//     class, generator, comprehension, async block): the creating node,
//     whose value is the created callable; it Uses the enclosing variables
//     the callable captures, since reading them is its own evaluation.
//
// A node defines at most one variable (flow.Builder.Def). A yielding node
// that already defines a variable (an embedded assignment to a local, an arm
// whose result is one, `c ? (x = 1) : 2`) may-defines the result instead.
// The result has no definition outside its construct's yielding nodes and
// each consumer reads it once, right after they run, so the may-definition
// reaches the consumer exactly where a definition would.
//
// SSA merges the yielding definitions at the consumer, and the yielding
// nodes carry the control dependence on the selector or condition that
// chose them, as `if (c) y = 1; else y = 2;` already does: in `y = switch
// (x) { case 1 -> { while (c) { yield 2; } yield 3; } default -> x; }` the
// declarator depends on both yields and on the default arm's node through
// the result variable, the yields depend on c through control, and the
// selector is read once, at its own node.
//
// A lowering may instead fold a construct into one node, with no nodes for
// its parts: the construct's reads are then that node's own evaluation, and
// no owned variable exists. Each lowering states exactly which constructs it
// folds and which it lowers to nodes.
//
// A read the consumer folds and that its evaluation makes before an
// embedded assignment redefines the variable (`y = x + (x = 1)`, `f(x, x =
// 1)`) is carried by the assignment's node: that node Uses the earlier
// value and hands it on through an owned variable it may-defines, and the
// consumer Uses that variable in place of the name, so the folded read
// pairs with the definition that reached it rather than the one after it.
// The hand-off is made only when the assignment's node runs whenever the
// consumer does. An assignment inside a conditionally evaluated operand (a
// short-circuit operand after the deciding one, a conditional's arm, a later
// operand of a chained comparison, an optional chain's tail, a statement of
// a GNU C statement expression that some path from the expression's start
// to its value skips) leaves the read on the consumer, where the name still
// has its earlier definition on the path that skips the assignment: `y = x +
// (c and (x := 1))` pairs y with the x before it when c is false, and with
// the assignment, whose value the operator's result also carries, when c is
// true. Where the language leaves the order of the two unsequenced or
// indeterminately sequenced (C and C++), the lowering takes source order and
// says so.
//
// A value evaluated once and used by several later nodes is evaluated at a
// node of its own, which defines an owned variable; the later nodes Use that
// variable, never the names the value was computed from. That covers a
// switch's selector and its case labels, a match scrutinee or subject and
// its arms and patterns, a with statement's entered value and its targets, a
// Go select clause's operands, and a loop's iterable (see Iteration).
//
// Each read is resolved in the scope where it occurs, on the node that
// carries it: a binding scoped to an arm (a pattern variable, a guard's
// binding, a match capture) is read by the arm's own nodes, so `return
// switch (o) { case Integer i when i > 0 -> i; … }` pairs the arm's result
// node i with the return through the result variable.
//
// # Names that resolve to no variable
//
// A name can resolve to no variable of the function being lowered: a field
// or member named without its object, a global or module name, a name no
// declaration in scope binds, an import, a name a macro introduces, or a
// binding that shadows without being a variable (scope.lookup returns -1
// for all of them). Such a name is state the dependence facts do not
// track, in every position: a read of it Uses nothing, a write to it
// (plain, compound, an increment, an embedded assignment, a deletion, an
// iteration target) defines and may-defines nothing, and a callable that
// captures it captures nothing. The node the construct makes is still made,
// with the reads and definitions of its other operands. -1 never reaches
// flow.Builder (whose Use, MayDef and Def reject it) and never indexes a
// table of the lowering's own; each lowering states where it filters it.
//
// # Iteration
//
// Every loop over an iterable evaluates the iterable once, before the first
// iteration, at a node of its own, in every language: the Java enhanced for
// (JLS §14.14.2), the C++ range for ([stmt.ranged]), the Python for and
// async for (Language Reference §8.3) and each for clause of a comprehension
// (§6.2.4; a later clause's iterable once per iteration of the clause
// before it), the JavaScript for…in, for…of and for await…of (ECMA-262
// §14.7.5.6, ForIn/OfHeadEvaluation), the Rust for (The Rust Reference,
// Iterator loops: `IntoIterator::into_iter` once) and the Go range clause
// (The Go Programming Language Specification, For statements with range
// clause). That node is a Stmt node spanning the iterated expression; it
// Uses the expression's reads and defines an iteration variable the lowering
// owns. The loop head Uses only that variable, never the names read in the
// iterated expression, and each per-iteration binding node Uses it too.
//
// The iterator is created once, so a body that rebinds the iterated name
// does not change the iteration, and `for k in d: d[k] = f(k)`, whose body
// may-defines d, does not reach the head's next step. A head that read the
// iterated names would pair with every such definition. Go's one exception,
// a range expression the specification does not evaluate at all, is stated
// in its lowering.
//
// # Spans
//
// Every node that spans an expression (a condition, a selector, a case
// label's value, an operand, an arm's result) spans it with every enclosing
// pair of parentheses stripped (unparen): each pair the grammar parses as a
// parenthesized expression, the statement's own parentheses included,
// however deeply they nest. A GNU C statement expression's own `(` `)` is
// such a pair, so a condition `({ …; e; })` spans the compound statement
// `{ …; e; }`.
//
// # Address-taking
//
// Every lowering applies one rule. A local gets a non-killing
// may-definition, which kills nothing, at the node that evaluates the
// operation, when any of these happens to it:
//
//   - its address is taken: `&x` in Go, C and C++, and `&x.f` and `&x[i]`,
//     whose base local is x;
//   - it is mutably borrowed: Rust `&mut x` (`&mut x.f`, `&mut x[i]`), and a
//     `ref mut` binding in a pattern, which borrows the matched local;
//   - a C++ reference to a non-const type is bound to it: `T &r = x`,
//     `T &&r = std::move(x)`, a range for's `auto &e : x`;
//   - in C and C++, it is declared as an array and is evaluated anywhere
//     other than as the operand of `sizeof`, `&` or a subscript, where it
//     decays to its address.
//
// Whatever receives the address or reference may write through it, and a
// may-definition of the base local is the conservative account of that
// write: for a pointer or slice base the write lands in the object it refers
// to, not in the local, so the may-definition over-approximates rather than
// states where the storage is. That node also Uses the operand's variables.
// The node is the one the evaluating expression attaches to, never "the next
// node made": a lowering records the pending may-definition by position, as
// it records reads, so an operand evaluated later cannot take it. An address
// a nested callable takes of an enclosing local is one of the callable's
// writes, a may-definition on the node that creates it, as its other writes
// are.
//
// Given up, each for the stated reason:
//
//   - a write through a pointer or reference (`*p = 2`, `r = 2` for a C++
//     reference r): without points-to analysis its target is unknown, and
//     making every such write a may-definition of every address-taken local
//     would connect every indirect write to every such local. The write
//     is still a may-definition of the variable it writes through (p in
//     `*p = 2` and `p->f = 2`), as every write through a field, index or
//     pointer target is of its base variable, so a later read through p
//     depends on it; what is given up is the local p points to;
//   - the implicit receiver borrow of a method call (a Go pointer-receiver
//     method, a Rust auto-referenced `&mut self` method, a C++ non-const
//     member function): whether the call borrows depends on the method's
//     signature;
//   - a C++ reference to a const type (`const T &r = x`, a range for's
//     `const auto &e : x`, `const T &&r`) is a use only, never a
//     may-definition: the language makes it a read-only view, as it makes a
//     Rust shared borrow, and on common code the may-definition would only
//     add false pairs. What that gives up is a write after casting the const
//     away and a write to a `mutable` member;
//   - a Rust shared borrow `&x` of an interior-mutable type (Cell, RefCell,
//     an atomic), which can write: whether it can depends on the type.
//
// A Rust `move` closure's writes are not definitions of the outer variable:
// the closure writes its own copy. Its reads are still the creating node's
// reads.
type Lowering struct {
	// language names the grammar the kinds below are resolved against.
	language string
	// callables are the named node kinds that begin a function.
	callables []string
	// ambient are the named node kinds whose subtrees hold declarations only,
	// never code that runs (a TypeScript `declare` block): Functions does not
	// descend into them, so a callable written there is not a function.
	ambient []string
	// lower drives b over fn's parameters and body in source order, keeping
	// every reusable list in s. It must not descend into a nested callable
	// (l.isCallable reports one) beyond the expression that creates it.
	// Begin and Finish are Lower's.
	lower func(l *Lowering, b *flow.Builder, fn *ts.Node, src []byte, s *Scratch)

	// once resolves callable and paren against the grammar, so every shared
	// helper compares kind ids rather than kind strings.
	once sync.Once
	// callable and opaque are indexed by kind id; opaque marks the ambient
	// kinds.
	callable, opaque []bool
	// paren is the kind id of a parenthesized expression.
	paren uint16
}

// LoweringFor returns the lowering of language, for the languages that have
// one: c, cpp, go, java, javascript, python, rust, tsx and typescript.
func LoweringFor(language string) (*Lowering, bool) {
	var l *Lowering
	switch language {
	case "c":
		l = &cLowering
	case "cpp":
		l = &cppLowering
	case "go":
		l = &goLowering
	case "java":
		l = &javaLowering
	case "javascript":
		l = &javascriptLowering
	case "python":
		l = &pythonLowering
	case "rust":
		l = &rustLowering
	case "tsx":
		l = &tsxLowering
	case "typescript":
		l = &typescriptLowering
	default:
		return nil, false
	}
	l.resolve()
	return l, true
}

// resolve resolves the lowering's kind ids once. A kind the grammar does not
// define is a lowering defect and panics, so a misspelt kind can never
// silently match nothing.
func (l *Lowering) resolve() {
	l.once.Do(func() {
		tl := mustGrammar(l.language)
		l.callable = make([]bool, tl.NodeKindCount())
		for _, name := range l.callables {
			l.callable[mustKind(tl, l.language, name, true)] = true
		}
		l.opaque = make([]bool, tl.NodeKindCount())
		for _, name := range l.ambient {
			l.opaque[mustKind(tl, l.language, name, true)] = true
		}
		l.paren = mustKind(tl, l.language, "parenthesized_expression", true)
	})
}

// mustGrammar is language's grammar; an unlinked one is a lowering defect.
func mustGrammar(language string) *ts.Language {
	tl, ok := Grammar(language)
	if !ok {
		panic("worker: the " + language + " grammar is not linked")
	}
	return tl
}

// mustKind is the id of the node kind name (named, or an anonymous token)
// in tl, the grammar of language.
func mustKind(tl *ts.Language, language, name string, named bool) uint16 {
	v := tl.IdForNodeKind(name, named)
	if v == 0 {
		panic("worker: the " + language + " grammar has no kind " + name)
	}
	return v
}

// mustField is the id of the field name in tl, the grammar of language.
func mustField(tl *ts.Language, language, name string) uint16 {
	v := tl.FieldIdForName(name)
	if v == 0 {
		panic("worker: the " + language + " grammar has no field " + name)
	}
	return v
}

// isCallable reports whether n begins a function of this language. An error
// node's id lies outside the grammar's kind count.
func (l *Lowering) isCallable(n *ts.Node) bool {
	id := n.KindId()
	return n.IsNamed() && int(id) < len(l.callable) && l.callable[id]
}

// isAmbient reports whether n is of an ambient kind.
func (l *Lowering) isAmbient(n *ts.Node) bool {
	id := n.KindId()
	return n.IsNamed() && int(id) < len(l.opaque) && l.opaque[id]
}

// unparen strips parentheses around n.
func (l *Lowering) unparen(n *ts.Node) *ts.Node {
	for n != nil && n.KindId() == l.paren {
		n = firstNamed(n)
	}
	return n
}

// Functions calls visit with every callable under root, root included, in
// preorder, nested callables included, in one tree-cursor walk. It stops at
// and returns the first error visit returns. A unit with no code of its own
// (a class body without initializers or static blocks) is still visited and
// lowers to Entry -> Exit, so a count of callables counts it. The subtree of
// an ambient kind is not walked.
func (l *Lowering) Functions(root *ts.Node, visit func(fn *ts.Node) error) error {
	c := root.Walk()
	defer c.Close()
	for {
		n := c.Node()
		if l.isCallable(n) {
			if err := visit(n); err != nil {
				return err
			}
		}
		if !l.isAmbient(n) && c.GotoFirstChild() {
			continue
		}
		for !c.GotoNextSibling() {
			if !c.GotoParent() {
				return nil
			}
		}
	}
}

// Lower builds fn's graph in a: Begin over fn's byte range, the language's
// lowering, Finish. fn must be a node Functions visited, and src the source
// its tree was parsed from, unchanged while fn is lowered. The graph is valid
// until the next a.Begin. s is the worker's lowering scratch; one Scratch
// serves every language and every function a worker lowers.
func (l *Lowering) Lower(fn *ts.Node, src []byte, a *flow.Arena, s *Scratch) *flow.Graph {
	b := a.Begin(spanOf(fn))
	s.scope.truncate(0)
	l.lower(l, b, fn, src, s)
	s.scope.truncate(0)
	return b.Finish()
}

// Scratch is one worker's reusable lowering state, the pointer-bearing
// counterpart of flow.Arena: the tree cursor every lowering walks with, the
// scope chain every lowering resolves names through, and each language's
// lowering state, whose lists keep their capacity from one
// function to the next. The zero value is ready to use; Close releases the
// cursor. It is not safe for concurrent use: one Scratch per worker, beside
// its Arena.
//
// Each language's state is created on first use and, at the start of every
// function, reset in place: every scalar is set anew and every list is
// truncated to length zero, never reallocated, so a worker in steady state
// allocates nothing per function beyond what a larger function than any
// before it needs. c serves c and cpp; js serves javascript, typescript and
// tsx.
type Scratch struct {
	// cur is the cursor, created by the first Lower and Reset to each
	// function's node after it.
	cur *ts.TreeCursor
	// scope is the scope chain of the function being lowered, empty between
	// functions.
	scope scope
	c     cLower
	gol   goLower
	java  javaLower
	js    jsLower
	py    pyLower
	rs    rsLower
}

// cursor is s's tree cursor reset to fn.
func (s *Scratch) cursor(fn *ts.Node) *ts.TreeCursor {
	if s.cur == nil {
		s.cur = fn.Walk()
	} else {
		s.cur.Reset(*fn)
	}
	return s.cur
}

// Close releases the cursor. s stays usable: the next Lower creates another.
func (s *Scratch) Close() {
	if s.cur != nil {
		s.cur.Close()
		s.cur = nil
	}
}

// spanOf is n's byte range.
func spanOf(n *ts.Node) flow.Span {
	return flow.Span{Start: uint32(n.StartByte()), End: uint32(n.EndByte())}
}

// textOf is n's source text in src, the source its tree was parsed from.
func textOf(src []byte, n *ts.Node) []byte { return src[n.StartByte():n.EndByte()] }

// view is b as a string without a copy: a label or a scope key names the
// source bytes themselves. It is sound because the source is not modified
// while a file is lowered, and nothing holds a view past the function it was
// made for: the builder and every Scratch list drop or overwrite them at the
// next function.
func view(b []byte) string { return unsafe.String(unsafe.SliceData(b), len(b)) }

// firstNamed is n's first named child that is not an extra (a comment), or
// nil.
func firstNamed(n *ts.Node) *ts.Node {
	for i := range n.NamedChildCount() {
		if c := n.NamedChild(i); !c.IsExtra() {
			return c
		}
	}
	return nil
}

// binding is one name in scope: name, a view of the source, resolves to
// variable v, or v is -1 for a name that shadows without being a variable of
// the function being lowered (a constant, a type, or a name a nested
// callable declares while its captures are resolved). prev is the index of
// the binding of the same name it shadows, or -1.
type binding struct {
	name string
	v    int32
	prev int32
}

// scope is the scope chain every lowering resolves names through, innermost
// binding last, with a hash index from each name to its innermost binding. A
// block's bindings are truncated away when it closes, and truncation restores
// the bindings they shadowed from their prev links, so a lookup is O(1)
// expected and a truncation costs O(bindings removed). It lives in the
// worker's Scratch and is emptied for every function; its keys are views of
// the source, like its names.
type scope struct {
	binds []binding
	// index maps a name to the index of its innermost binding; it holds an
	// entry for exactly the names binds holds.
	index map[string]int32
}

// mark is the scope's length, the point truncate returns to.
func (s *scope) mark() int { return len(s.binds) }

// push binds name, a slice of the source, to v in the innermost scope.
func (s *scope) push(name []byte, v int32) { s.bind(view(name), v) }

// bind is push for a name that is already a view of the source.
func (s *scope) bind(name string, v int32) {
	if s.index == nil {
		s.index = make(map[string]int32)
	}
	prev, ok := s.index[name]
	if !ok {
		prev = -1
	}
	s.index[name] = int32(len(s.binds))
	s.binds = append(s.binds, binding{name: name, v: v, prev: prev})
}

// truncate drops every binding from index m on, innermost first, restoring
// the index entry each one shadowed.
func (s *scope) truncate(m int) {
	for i := len(s.binds) - 1; i >= m; i-- {
		if b := s.binds[i]; b.prev < 0 {
			delete(s.index, b.name)
		} else {
			s.index[b.name] = b.prev
		}
	}
	clear(s.binds[m:])
	s.binds = s.binds[:m]
}

// innermost is the index of name's innermost binding, or -1.
func (s *scope) innermost(name []byte) int {
	if i, ok := s.index[string(name)]; ok {
		return int(i)
	}
	return -1
}

// shadowed is the index of the binding binding i shadows, or -1.
func (s *scope) shadowed(i int) int { return int(s.binds[i].prev) }

// at is binding i.
func (s *scope) at(i int) binding { return s.binds[i] }

// find is the index of the innermost binding of name in binds[from:], or -1:
// the innermost binding has the largest index, so none lies at or past from
// when it does not.
func (s *scope) find(name []byte, from int) int {
	if i := s.innermost(name); i >= from {
		return i
	}
	return -1
}

// lookup is the variable name resolves to, or -1 when it is not a variable
// of the function being lowered.
func (s *scope) lookup(name []byte) int32 {
	if i := s.innermost(name); i >= 0 {
		return s.binds[i].v
	}
	return -1
}
