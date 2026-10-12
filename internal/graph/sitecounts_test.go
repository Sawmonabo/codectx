package graph

import (
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
)

// TestSiteCountsQuoteTheProjectTotals protects the figures a callers answer
// with no caller quotes: the project's call sites, not one unit's.
//
// The stored `calls` rows each stand for a disjoint set of the linker's units
// (the fresh fold, and one row per degraded unit), so the project's figure is
// their sum per key. A row of another capability is not part of it.
//
// Mutation proof: quote the first `calls` row carrying counts, as siteCounts
// once did, and the notice says "sites 10, ... open sites 4" instead of the
// sums.
func TestSiteCountsQuoteTheProjectTotals(t *testing.T) {
	t.Parallel()
	calls := string(model.RelCalls)
	caps := []model.CapabilityState{
		{ProviderID: "link", Capability: calls, State: model.CapabilityFresh,
			Details: map[string]string{"sites": "10", "bound_sites": "6", "open_sites": "4", "scopes": "3"}},
		{ProviderID: "link", Capability: string(model.RelImports), State: model.CapabilityFresh,
			Details: map[string]string{"sites": "1000"}},
		{ProviderID: "link", Capability: calls, Scope: "project:go:b", State: model.CapabilityPartial,
			Details: map[string]string{"sites": "5", "bound_sites": "2", "open_sites": "3"}},
	}
	want := "the project's call sites are sites 15, bound sites 8, open sites 7"
	if got := siteCounts(caps); got != want {
		t.Fatalf("siteCounts = %q, want %q", got, want)
	}
}
