package graph

import (
	"context"
	"strings"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
)

// TestReferencePageBoundIsDisclosedNotClamped is the class-G proof for
// References. Its page bound used to be clamped twice in silence -- once
// against the configured ceiling, once against the wire ceiling -- so a caller
// that asked for 50 occurrences and was served 200 could not tell a clamped
// page from the end of the answer. It is now RESOLVED and disclosed, the same
// way a traversal's bounds are.
//
// Mutation (resolvePageItems replaced by the old `if pageLimit <= 0 ||
// pageLimit > e.limits.MaxPageItems` clamp): Notices is empty and this fails.
func TestReferencePageBoundIsDisclosedNotClamped(t *testing.T) {
	f := newGraphFixture(t)
	limits := fixtureLimits()
	limits.MaxPageItems = 5
	e, err := New(Options{Adjacency: f, Reader: memGraphFor(f), Limits: limits})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	page, err := e.References(context.Background(), model.ReferenceRequest{
		NodeID:         fixtureNodeID("n-b"),
		Operation:      model.ReferenceReferences,
		SemanticSource: model.SemanticCanonical,
		Page:           model.PageRequest{Limit: 50},
	})
	if err != nil {
		t.Fatalf("References: %v", err)
	}
	found := false
	for _, n := range page.Meta.Notices {
		if strings.Contains(n, "page.limit: requested 50, effective 5") {
			found = true
		}
	}
	if !found {
		t.Fatalf("a page bound clamped from 50 to 5 was not disclosed; notices = %q", page.Meta.Notices)
	}
}
