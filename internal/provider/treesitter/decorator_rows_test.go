package treesitter_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
)

// TestEveryDecoratorIsPublishedPastTheMetadataBound indexes a Python function
// with 300 decorators, whose list encodes past model.MaxMetadataBytes, and
// asserts every decorator is published, in source order, as a metadata row
// on the function, with a decorators_rows marker in its metadata, and that
// the file's structure is not reported partial for it.
// Mutation caught: keeping the longest prefix that fits the bound (the
// decorators past it are lost and the file reports partial), or rows out of
// source order.
func TestEveryDecoratorIsPublishedPastTheMetadataBound(t *testing.T) {
	const n = 300
	var src strings.Builder
	var want []string
	for i := range n {
		name := fmt.Sprintf("registered_with_a_long_decorator_name_%04d", i)
		want = append(want, name)
		fmt.Fprintf(&src, "@%s\n", name)
	}
	src.WriteString("def handler():\n    return 1\n")
	c, result := extractedRun(t, newProvider(t), "app/handlers.py", src.String())
	fact := c.fact(t, model.NodeFunction, "handler")
	meta := metadata(t, fact.Node)
	if _, inline := meta[model.MetaDecorators]; inline {
		t.Fatalf("the decorators are inline in %d bytes of metadata; they do not fit", len(fact.Node.Metadata))
	}
	if got := meta[model.MetadataRowsKey(model.MetaDecorators)]; got != float64(n) {
		t.Fatalf("decorators_rows = %v, want %d", got, n)
	}
	var rows []model.MetadataRow
	for _, ev := range fact.Evidence {
		if r, ok := model.ParseMetadataRow(ev.Detail); ok && r.Field == model.MetaDecorators {
			rows = append(rows, r)
		}
	}
	raw, err := model.JoinMetadataRows(rows)
	if err != nil {
		t.Fatalf("join %d decorator rows: %v", len(rows), err)
	}
	var decs []model.Decorator
	if err := json.Unmarshal(raw, &decs); err != nil {
		t.Fatal(err)
	}
	if len(decs) != n {
		t.Fatalf("published %d decorators, want %d", len(decs), n)
	}
	for i, d := range decs {
		if d.Name != want[i] {
			t.Fatalf("decorator %d is %q, want %q: source order", i, d.Name, want[i])
		}
	}
	if row := structureRow(t, result); row.State != model.CapabilityFresh {
		t.Errorf("structure is %s/%s, want fresh: no decorator was dropped", row.State, row.DiagnosticCode)
	}
}

// fact is the one published node fact of kind named name.
func (c *capture) fact(t *testing.T, kind model.NodeKind, name string) model.NodeFact {
	t.Helper()
	var out []model.NodeFact
	for _, f := range c.nodes {
		if f.Node.Kind == kind && f.Node.Name == name {
			out = append(out, f)
		}
	}
	if len(out) != 1 {
		t.Fatalf("%d %s facts named %q, want 1", len(out), kind, name)
	}
	return out[0]
}
