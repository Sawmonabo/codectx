package treesitter_test

import (
	"strings"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
)

// TestADeclarationNodeCarriesItsNameTokenRange asserts that the node of a
// Go, Python and Java declaration carries the exact byte range of its name
// token, inside and narrower than the declaration's own range, with line and
// column derived from the same bytes: the `Foo` of `func Foo()`.
//
// Mutations it catches: leaving the name range absent; publishing the
// declaration's range as its name range; deriving it from any offset other
// than the worker's name token (the first occurrence of the name in the file
// is a comment, so a text search for the name lands on the wrong bytes).
func TestADeclarationNodeCarriesItsNameTokenRange(t *testing.T) {
	p := newProvider(t)
	cases := []struct {
		file, src string
		kind      model.NodeKind
		name      string
		// line is the one-based line the name token is on.
		line uint32
	}{
		{"names.go", "package names\n\n// Foo says hello.\nfunc Foo() int {\n\treturn 1\n}\n", model.NodeFunction, "Foo", 4},
		{"names.py", "# foo says hello.\ndef foo():\n    return 1\n", model.NodeFunction, "foo", 2},
		{"Names.java", "// run says hello.\nclass Names {\n    void run() {\n    }\n}\n", model.NodeMethod, "run", 3},
	}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			c := extracted(t, p, tc.file, tc.src)
			n := c.node(t, tc.kind, tc.name)
			if n.NameRange == nil {
				t.Fatalf("%s carries no name range", tc.name)
			}
			if n.Range == nil {
				t.Fatalf("%s carries no range", tc.name)
			}
			// The declaration's token, not the comment's earlier mention.
			lineStart := 0
			for i := uint32(1); i < tc.line; i++ {
				lineStart += strings.IndexByte(tc.src[lineStart:], '\n') + 1
			}
			start := uint64(lineStart + strings.Index(tc.src[lineStart:], tc.name))
			end := start + uint64(len(tc.name))
			got := *n.NameRange
			if got.Start.Byte != start || got.End.Byte != end {
				t.Fatalf("%s name range is bytes [%d,%d), want [%d,%d) (%q)", tc.name,
					got.Start.Byte, got.End.Byte, start, end, tc.src[start:end])
			}
			if got.Start.Line != tc.line || got.Start.Column != uint32(start)-uint32(lineStart) {
				t.Errorf("%s name starts at line %d column %d, want line %d column %d", tc.name,
					got.Start.Line, got.Start.Column, tc.line, uint32(start)-uint32(lineStart))
			}
			if *n.Range == got {
				t.Errorf("%s name range equals its declaration range %+v", tc.name, got)
			}
			if n.Range.Start.Byte > start || n.Range.End.Byte < end {
				t.Errorf("%s name range [%d,%d) lies outside its declaration [%d,%d)", tc.name,
					start, end, n.Range.Start.Byte, n.Range.End.Byte)
			}
		})
	}
}
