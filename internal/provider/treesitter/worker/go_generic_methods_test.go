package worker

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	ts "github.com/tree-sitter/go-tree-sitter"

	"github.com/Sawmonabo/codectx/internal/provider/treesitter/wire"
)

// TestGoGenericMethodAndTheMethodAfterItAreDeclaredExactly protects the Go
// 1.27 generic method (MethodDecl takes [ TypeParameters ] after the method
// name): the method and every declaration after it are extracted with their
// exact spans, and the file parses clean. Mutation: link the released
// grammar, which has no type parameters on method_declaration; its error
// cascade swallows the generic method and the normal method after it, so
// both declarations are missing and the parse reports syntax errors.
func TestGoGenericMethodAndTheMethodAfterItAreDeclaredExactly(t *testing.T) {
	// The generic method is the specification's own example.
	const generic = "func (l List[E]) Apply[F any](f func(E) F) List[F] {\n\tr := make(List[F], len(l))\n\tfor i, x := range l {\n\t\tr[i] = f(x)\n\t}\n\treturn r\n}"
	const after = "func (l List[E]) Len() int { return len(l) }"
	const src = "package p\n\ntype List[E any] []E\n\n" + generic + "\n\n" + after + "\n"
	w := &state{parsers: map[string]*ts.Parser{}, queries: map[string]*ts.Query{}}
	defer w.close()
	var out bytes.Buffer
	done, fail, err := w.parse(&out, wire.Request{Language: "go", Path: "p.go", SourceBytes: uint64(len(src))}, []byte(src))
	if err != nil || fail != nil {
		t.Fatalf("parse: %v, %+v", err, fail)
	}
	if done.SyntaxErrors {
		t.Fatal("a file with a generic method reports syntax errors")
	}
	decls := goDecls(t, &out)
	for _, m := range []struct{ name, text string }{{"Apply", generic}, {"Len", after}} {
		d, ok := decls[m.name]
		if !ok {
			t.Fatalf("method %s is not declared; declared: %v", m.name, decls)
		}
		start := uint32(strings.Index(src, m.text))
		end := start + uint32(len(m.text))
		nameStart := start + uint32(strings.Index(m.text, ") "+m.name)+2)
		if d.Kind != "method" || d.Qualified != "p.List."+m.name || d.Impl != "List" || d.Start != start || d.End != end ||
			d.NameStart != nameStart || d.NameEnd != nameStart+uint32(len(m.name)) {
			t.Fatalf("method %s is %+v, want a p.List method spanning [%d, %d) named at %d", m.name, d, start, end, nameStart)
		}
	}
}

// goDecls returns the worker answer's declarations by name.
func goDecls(t *testing.T, r io.Reader) map[string]wire.Decl {
	t.Helper()
	out := map[string]wire.Decl{}
	for {
		kind, payload, err := wire.ReadMessage(r, 0)
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		if kind != wire.KindDecl {
			continue
		}
		var d wire.Decl
		if err := json.Unmarshal(payload, &d); err != nil {
			t.Fatal(err)
		}
		out[d.Name] = d
	}
}
