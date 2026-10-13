package link_test

import (
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
)

// TestGoRowsCountInexactSignatures protects the count a Go unit's
// implements and overrides rows give of its methods whose method-set key is
// not an exact identity: one naming a type by its unresolved spelling
// (several imports whose clause is unread, so `yaml` is one of them), one
// whose signature the form cannot spell (a length that is no integer
// constant expression), and none for the exact ones.
//
// Mutation: count every method -> Exact is counted. Mutation: count an
// unknown signature as unresolved -> unresolved is 2 and unknown 0.
// Mutation: drop the details -> the rows carry none.
func TestGoRowsCountInexactSignatures(t *testing.T) {
	l := linkSources(t, newStructural(t), map[string]string{
		"go.mod": "module example.com/m\n\ngo 1.27\n",
		"p.go": `package p

import (
	"example.com/ext/one"
	"example.com/ext/two"
)

var _ = one.X
var _ = two.Y

type T struct{}

func (T) Load(n yaml.Node) {}

func (T) Sized(b [len("x")]byte) {}

func (T) Exact(n int) {}
`,
	})
	for _, kind := range []model.RelationKind{model.RelImplements, model.RelOverrides} {
		var row *model.CapabilityState
		for i := range l.rows {
			if l.rows[i].Capability == string(kind) {
				row = &l.rows[i]
			}
		}
		if row == nil {
			t.Fatalf("no %s row", kind)
		}
		if got := row.Details["go_method_signatures_unresolved"]; got != "1" {
			t.Errorf("%s row: go_method_signatures_unresolved = %q, want 1", kind, got)
		}
		if got := row.Details["go_method_signatures_unknown"]; got != "1" {
			t.Errorf("%s row: go_method_signatures_unknown = %q, want 1", kind, got)
		}
	}
}
