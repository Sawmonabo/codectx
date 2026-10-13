package graph

import (
	"fmt"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
)

// TestWeightedWalkTakesMoreRootsThanTheWireCeiling pins that the in-process
// weighted walk has no root-count ceiling while every root still meets the id
// contract.
//
// Mutation caught (first case): validating the whole root list as one impact
// request, so model.MaxStartNodes+1 roots are refused and a compile with that
// many roots fails instead of planning.
// Mutation caught (second case): screening only the first window of roots, so
// a malformed id past model.MaxStartNodes is accepted.
func TestWeightedWalkTakesMoreRootsThanTheWireCeiling(t *testing.T) {
	roots := make([]WeightedRoot, model.MaxStartNodes+1)
	for i := range roots {
		roots[i] = WeightedRoot{Node: model.NodeID(fmt.Sprintf("%064x", i+1)), Weight: 1}
	}
	req := WeightedWalkRequest{
		Roots: roots, Relations: []model.RelationKind{model.RelCalls}, Direction: model.DirectionBoth,
		Rule: LabelRule{Scale: 1_000_000, Hop: map[model.RelationKind]int64{model.RelCalls: 900_000},
			Decay: 1_000_000, Classes: 4, Policy: "test"},
	}
	if err := req.validate(); err != nil {
		t.Fatalf("%d roots: validate() = %v, want nil", len(roots), err)
	}
	roots[len(roots)-1].Node = "not-an-id"
	if err := req.validate(); err == nil {
		t.Fatalf("a malformed root past the first %d was accepted", model.MaxStartNodes)
	}
}
