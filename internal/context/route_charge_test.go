package context

import (
	"fmt"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
)

// TestAnEntrysChargeDoesNotMoveWithItsRoutes pins the packer's charge: an
// entry costs its fixed fields, never its routes, so the same file
// reached by one route or by fifty is one group size, and more roots reaching
// an entry can never push its file out of a slice (ADR-0016 decision 7).
//
// Mutation caught: measuring the entry with its routes (or with its real route
// count's digits), so fifty routes charge more than one.
func TestAnEntrysChargeDoesNotMoveWithItsRoutes(t *testing.T) {
	route := func(i int) model.RelationPath {
		return model.RelationPath{Relations: []model.RelationID{model.RelationID(model.H("relation", fmt.Sprint(i)))}}
	}
	base := candidate{NodeID: model.NodeID(model.H("node")), FileID: model.FileID(model.H("file")),
		Requirement: model.RequirementSymbol, SizeBytes: 4096, Reasons: []string{"calls it (outgoing, depth 1)"}}
	one, many := base, base
	one.Paths = []model.RelationPath{route(0)}
	for i := range 50 {
		many.Paths = append(many.Paths, route(i))
	}
	a, err := measureEntry(one, 3, true)
	if err != nil {
		t.Fatalf("measureEntry(one route): %v", err)
	}
	b, err := measureEntry(many, 3, true)
	if err != nil {
		t.Fatalf("measureEntry(fifty routes): %v", err)
	}
	if a.EstimatedBytes != b.EstimatedBytes || a.EstimatedTokens != b.EstimatedTokens {
		t.Fatalf("one route charges %d bytes (%d tokens), fifty charge %d (%d): the routes moved the charge",
			a.EstimatedBytes, a.EstimatedTokens, b.EstimatedBytes, b.EstimatedTokens)
	}
	if a.RouteCount != 1 || b.RouteCount != 50 || len(b.EvidencePaths) != 50 {
		t.Fatalf("the entries count %d and %d routes and carry %d, want 1, 50 and all 50 stored",
			a.RouteCount, b.RouteCount, len(b.EvidencePaths))
	}
}

// TestAnEntrysChargeDoesNotMoveWithItsReasons pins the same for reasons: an
// entry explained by one short reason and one explained by every reason it
// may carry, each at the byte bound, cost the same, and both keep their
// reasons whole for the reasons view.
//
// Mutation caught: measuring the entry with its reasons (or with its real
// reason count's digits).
func TestAnEntrysChargeDoesNotMoveWithItsReasons(t *testing.T) {
	base := candidate{NodeID: model.NodeID(model.H("node")), FileID: model.FileID(model.H("file")),
		Requirement: model.RequirementSymbol, SizeBytes: 4096}
	short, long := base, base
	short.Reasons = []string{"x"}
	for i := range model.MaxReasonsPerEntry {
		long.Reasons = append(long.Reasons, fmt.Sprintf("%0*d", model.MaxReasonBytes, i))
	}
	a, err := measureEntry(short, 3, true)
	if err != nil {
		t.Fatalf("measureEntry(one reason): %v", err)
	}
	b, err := measureEntry(long, 3, true)
	if err != nil {
		t.Fatalf("measureEntry(every reason): %v", err)
	}
	if a.EstimatedBytes != b.EstimatedBytes || a.EstimatedTokens != b.EstimatedTokens {
		t.Fatalf("one reason charges %d bytes (%d tokens), %d charge %d (%d): the reasons moved the charge",
			a.EstimatedBytes, a.EstimatedTokens, len(long.Reasons), b.EstimatedBytes, b.EstimatedTokens)
	}
	if a.ReasonCount != 1 || b.ReasonCount != int64(len(long.Reasons)) || len(b.Reasons) != len(long.Reasons) {
		t.Fatalf("the entries count %d and %d reasons and carry %d, want 1, %d and all stored",
			a.ReasonCount, b.ReasonCount, len(b.Reasons), len(long.Reasons))
	}
}
