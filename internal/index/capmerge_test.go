package index

import (
	"io"
	"log/slog"
	"maps"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider"
)

// TestCapabilityFoldSumsEqualCountsAndKeepsLimits protects the project totals
// a folded row publishes.
//
// Two units that each counted ten call sites counted twenty between them, so
// the fold of their fresh rows must say twenty whichever arrived first. A
// limit both units report beside the count of what crossed it, and the
// largest record one of them saw, are not counts: the fold keeps the limit as
// it is and the larger of the two sizes.
//
// Mutation proof: restore `if a == b { return a, false }` at the top of
// mergeDetailValue and `sites` folds to 10; drop the extremeDetail case and
// the limit folds to 8192 and the largest size to 12000.
func TestCapabilityFoldSumsEqualCountsAndKeepsLimits(t *testing.T) {
	t.Parallel()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	unit := func(scope, largest string) model.CapabilityState {
		return model.CapabilityState{ProviderID: "link", Capability: string(model.RelCalls), Scope: scope,
			State: model.CapabilityFresh, Details: map[string]string{
				"sites": "10", "open_sites": "3",
				"over_max_provider_record_bytes": "1", "max_provider_record_bytes": "4096",
				provider.DetailLargestRecordBytes: largest}}
	}
	a, b := unit("pkg:go:a", "5000"), unit("pkg:go:b", "7000")

	folded := func(rows ...model.CapabilityState) map[string]string {
		r := newCapabilityReport()
		for _, row := range rows {
			r.add(row)
		}
		out := r.finish(log)
		if len(out) != 1 || out[0].Scope != provider.ScopeWorkspace {
			t.Fatalf("the report published %+v, want one workspace row", out)
		}
		return out[0].Details
	}
	forward, reverse := folded(a, b), folded(b, a)
	if !maps.Equal(forward, reverse) {
		t.Fatalf("the fold published %v forward and %v in reverse; it must be a function of the set", forward, reverse)
	}
	want := map[string]string{"sites": "20", "open_sites": "6", "over_max_provider_record_bytes": "2",
		"max_provider_record_bytes": "4096", provider.DetailLargestRecordBytes: "7000", scopesDetail: "2"}
	if !maps.Equal(forward, want) {
		t.Fatalf("the fold published %v, want %v", forward, want)
	}
}
