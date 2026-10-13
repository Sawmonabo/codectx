package worker

import (
	"sync"

	"github.com/Sawmonabo/codectx/internal/provider/treesitter/flow"
)

// A callable returns a value from its returning nodes (wire.Function.Returns):
// every node a return statement moved to Exit (flow.Graph.Returns), and, when
// its body is an expression, the nodes control falls off the end from
// (flow.Graph.Falls), which yield that expression's value. The body is an
// expression for:
//   - an ECMAScript arrow function whose body is not a statement block
//     (`x => x + 1`);
//   - a Python lambda;
//   - a Java lambda expression whose body is not a block;
//   - a Rust closure whose body is not a block, and a Rust function or
//     closure whose body block has a tail: its last statement is an
//     expression, or an expression statement without its `;` (the Rust
//     Reference, Block expressions), which the Rust lowering lowers as the
//     block's value (rsLower.yields).
//
// Every other callable's fall-through returns no value: a Go, C or C++
// function, a Python def, an ECMAScript function or method with a block
// body, whose end returns nothing (undefined, None, void).

// bodyKinds are one language's kind and field ids valueBody reads, resolved
// once against the pinned grammar.
type bodyKinds struct {
	// fn is the callable kind whose body may be an expression, field its
	// body field and block the block kind a statement body is.
	fn, field, block uint16
	// always reports a callable whose body is always an expression (a
	// Python lambda).
	always bool
}

var (
	bodyOnce  sync.Once
	bodyTable map[string]bodyKinds
)

// bodyKindsOf resolves the tables once. A name the grammar does not define is
// a defect and panics, as a lowering's kind table does.
func bodyKindsOf(language string) (bodyKinds, bool) {
	bodyOnce.Do(func() {
		bodyTable = map[string]bodyKinds{}
		for _, language := range []string{"javascript", "typescript", "tsx"} {
			tl := mustGrammar(language)
			bodyTable[language] = bodyKinds{fn: mustKind(tl, language, "arrow_function", true),
				field: mustField(tl, language, "body"), block: mustKind(tl, language, "statement_block", true)}
		}
		tl := mustGrammar("python")
		bodyTable["python"] = bodyKinds{fn: mustKind(tl, "python", "lambda", true), always: true}
		tl = mustGrammar("java")
		bodyTable["java"] = bodyKinds{fn: mustKind(tl, "java", "lambda_expression", true),
			field: mustField(tl, "java", "body"), block: mustKind(tl, "java", "block", true)}
	})
	k, ok := bodyTable[language]
	return k, ok
}

// valueBody reports whether fn, a callable of l's language, has an
// expression body, so that its fall-through nodes are returning nodes.
func valueBody(l *Lowering, fn Node) bool {
	if l.language == "rust" {
		return rustValueBody(fn)
	}
	k, ok := bodyKindsOf(l.language)
	if !ok || fn.KindId() != k.fn {
		return false
	}
	if k.always {
		return true
	}
	body := fn.ChildByFieldId(k.field)
	return !body.IsNull() && body.KindId() != k.block
}

// rustValueBody reports whether a Rust function or closure fn yields its
// body's value: a closure's body is an expression, or the body block has a
// tail, which rsLower.yields decides as the lowering does.
func rustValueBody(fn Node) bool {
	k := rsSyntaxOf()
	if id := fn.KindId(); id != k.functionItem && id != k.closureExpression {
		return false
	}
	body := fn.ChildByFieldId(k.fBody)
	switch {
	case body.IsNull():
		return false
	case body.KindId() != k.block:
		return fn.KindId() == k.closureExpression
	}
	var last Node
	for i := range body.NamedChildCount() {
		if c := body.NamedChild(i); !c.IsExtra() && c.KindId() != k.label {
			last = c
		}
	}
	if last.IsNull() {
		return false
	}
	switch id := last.KindId(); {
	case id == k.letDeclaration:
		return false
	case id == k.expressionStatement:
		for i := range last.ChildCount() {
			if last.Child(i).KindId() == k.semi {
				return false
			}
		}
		return true
	case int(id) < len(k.item) && k.item[id]:
		return false
	}
	return true
}

// returning appends to dst every returning node of g, the graph of fn, a
// callable of l's language: g.Returns, and g.Falls when fn's body is an
// expression. Entry is never one: a body that lowered to no node yields
// nothing the analysis names.
func returning(l *Lowering, fn Node, g *flow.Graph, dst []int32) []int32 {
	dst = append(dst, g.Returns()...)
	if valueBody(l, fn) {
		for _, n := range g.Falls() {
			if n != flow.EntryNode {
				dst = append(dst, n)
			}
		}
	}
	return dst
}
