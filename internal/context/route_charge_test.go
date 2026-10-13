package context

import (
	"fmt"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
)

// TestAnEntrysChargeDoesNotMoveWithItsRoutes pins the packer's charge: an
// entry costs its fixed fields and reasons, never its routes, so the same file
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
