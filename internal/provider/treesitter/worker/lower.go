package worker

import (
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
	return ts.NewLanguage(g.language()), true
}

// Lowering turns one language's callables into flow graphs. A callable is a
// function, method or function-valued literal; each one, nested or not, is
// its own function, and its enclosing function sees only the expression that
// creates it. A Lowering holds no state and is safe to share.
type Lowering struct {
	// callable is the set of named node kinds that begin a function.
	callable map[string]bool
	// lower drives b over fn's parameters and body in source order. It must
	// not descend into a nested callable (l.callable reports one) beyond the
	// expression that creates it. Begin and Finish are Lower's.
	lower func(l *Lowering, b *flow.Builder, fn *ts.Node, src []byte)
}

// LoweringFor returns the lowering of language, for the languages that have
// one: go and javascript.
func LoweringFor(language string) (*Lowering, bool) {
	switch language {
	case "go":
		return &goLowering, true
	case "javascript":
		return &javascriptLowering, true
	}
	return nil, false
}

// isCallable reports whether n begins a function of this language.
func (l *Lowering) isCallable(n *ts.Node) bool { return n.IsNamed() && l.callable[n.Kind()] }

// Functions calls visit with every callable under root, root included, in
// preorder, nested callables included, in one tree-cursor walk. It stops at
// and returns the first error visit returns.
func (l *Lowering) Functions(root *ts.Node, visit func(fn *ts.Node) error) error {
	c := root.Walk()
	defer c.Close()
	for {
		if n := c.Node(); l.isCallable(n) {
			if err := visit(n); err != nil {
				return err
			}
		}
		if c.GotoFirstChild() {
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
	b := a.Begin(flow.Span{Start: uint32(fn.StartByte()), End: uint32(fn.EndByte())})
	l.lower(l, b, fn, src)
	return b.Finish()
}
