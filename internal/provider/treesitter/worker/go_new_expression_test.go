package worker

import (
	"bytes"
	"testing"

	ts "github.com/tree-sitter/go-tree-sitter"

	"github.com/Sawmonabo/codectx/internal/provider/treesitter/wire"
)

// TestGoNewOfAnExpressionKeepsTheFactsAfterIt protects the Go 1.26 form of
// new whose operand is an expression ("Allocation": new(x) allocates a
// variable initialised to x): the file parses clean, and the call after it
// keeps its site and its arguments' reads.
//
// Mutation: link a grammar whose new takes only a type -> `new(42)` is an
// error and `new(n + 1)` an error cascade that swallows the statements after
// it, so the parse reports syntax errors and g's argument b reads no
// variable: b's declaration is lost.
func TestGoNewOfAnExpressionKeepsTheFactsAfterIt(t *testing.T) {
	const src = `package p

func g(a, b *int) {}

func f(n int) {
	a := new(42)
	b := new(n + 1)
	g(a, b)
}
`
	w := &state{parsers: map[string]*ts.Parser{}, queries: map[string]*ts.Query{}}
	defer w.close()
	var out bytes.Buffer
	done, fail, err := w.parse(&out, wire.Request{Language: "go", Path: "p.go", SourceBytes: uint64(len(src))}, []byte(src))
	if err != nil || fail != nil {
		t.Fatalf("parse: %v, %+v", err, fail)
	}
	if done.SyntaxErrors {
		t.Fatal("new of an expression reports syntax errors")
	}
	l, fns := linkOf(t, "go", "p.go", src)
	tok := func(needle string) wire.Span {
		s := textAt(t, src, needle, 0)
		s.End = s.Start + 1
		return s
	}
	wantArguments(t, src, fns, siteNamed(l, tok("g(a, b)")),
		[]argWant{{at: "a", reads: []wire.Span{tok("a := new")}}, {at: "b", reads: []wire.Span{tok("b := new")}}})
}
