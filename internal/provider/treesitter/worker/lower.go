package worker

import (
	"bytes"
	"sync"

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
//   - a C++ reference is bound to it: `T &r = x`;
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
	// lower drives b over fn's parameters and body in source order. It must
	// not descend into a nested callable (l.isCallable reports one) beyond
	// the expression that creates it. Begin and Finish are Lower's.
	lower func(l *Lowering, b *flow.Builder, fn *ts.Node, src []byte)

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
// its tree was parsed from. The graph is valid until the next a.Begin.
func (l *Lowering) Lower(fn *ts.Node, src []byte, a *flow.Arena) *flow.Graph {
	b := a.Begin(spanOf(fn))
	l.lower(l, b, fn, src)
	return b.Finish()
}

// spanOf is n's byte range.
func spanOf(n *ts.Node) flow.Span {
	return flow.Span{Start: uint32(n.StartByte()), End: uint32(n.EndByte())}
}

// textOf is n's source text in src, the source its tree was parsed from.
func textOf(src []byte, n *ts.Node) []byte { return src[n.StartByte():n.EndByte()] }

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

// binding is one name in scope: name, a slice of the source, resolves to
// variable v, or v is -1 for a name that shadows without being a variable of
// the function being lowered (a constant, a type, or a name a nested
// callable declares while its captures are resolved).
type binding struct {
	name []byte
	v    int32
}

// scope is a lowering's scope chain, innermost binding last; a block's
// bindings are truncated away when it closes.
type scope []binding

// find is the index of the innermost binding of name in s[from:], or -1.
func (s scope) find(name []byte, from int) int {
	for i := len(s) - 1; i >= from; i-- {
		if bytes.Equal(s[i].name, name) {
			return i
		}
	}
	return -1
}

// lookup is the variable name resolves to, or -1 when it is not a variable
// of the function being lowered.
func (s scope) lookup(name []byte) int32 {
	if i := s.find(name, 0); i >= 0 {
		return s[i].v
	}
	return -1
}
