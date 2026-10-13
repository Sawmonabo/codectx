package index

import (
	"maps"
	"slices"
	"strconv"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
)

// TestCollapseScopesFoldsEveryScopesDetails protects the aggregate above
// model.MaxCapabilityStates: a collapsed row stands for every scope it counts,
// so its counts are the sum over those scopes and its reasons their union,
// while the failure message stays the exemplar scope's, paired with the code
// the row publishes. The result must not depend on the order the rows came in.
//
// Mutations it catches: publishing the exemplar row's details alone (units
// failed 2 instead of 9, one reason instead of three), folding the other
// scopes' failure messages into the exemplar's, or merging in arrival order
// so a reversed input publishes different details.
func TestCollapseScopesFoldsEveryScopesDetails(t *testing.T) {
	scopes := []struct{ scope, failed, reason, message string }{
		{"pkg:go:a", "2", "timeout", "a timed out"},
		{"pkg:go:b", "3", "output_invalid", "b wrote garbage"},
		{"pkg:go:c", "4", "timeout", "c timed out"},
	}
	var rows []model.CapabilityState
	for _, s := range scopes {
		rows = append(rows, model.CapabilityState{ProviderID: "p", Capability: "references", Scope: s.scope,
			State: model.CapabilityFailed, DiagnosticCode: model.CodeProviderTimeout,
			Details: map[string]string{unitsFailedDetail: s.failed, "reason": s.reason, failureMessageDetail: s.message}})
	}
	rows = append(rows, model.CapabilityState{ProviderID: "p", Capability: "references", Scope: "pkg:go:d",
		State: model.CapabilityFailed, DiagnosticCode: model.CodeProviderTimeout,
		Details: map[string]string{"reason": "oom"}})
	forward := collapseScopes(rows)
	reversed := slices.Clone(rows)
	slices.Reverse(reversed)
	backward := collapseScopes(reversed)
	if len(forward) != 1 || len(backward) != 1 {
		t.Fatalf("one provider capability and state collapse to %d and %d rows, want 1", len(forward), len(backward))
	}
	got := forward[0]
	want := map[string]string{
		unitsFailedDetail:    strconv.Itoa(2 + 3 + 4),
		"reason":             "oom" + detailSeparator + "output_invalid" + detailSeparator + "timeout",
		failureMessageDetail: "a timed out",
		scopeKeyDetail:       "pkg:go:a",
		scopesDetail:         "4",
	}
	for k, v := range want {
		if got.Details[k] != v {
			t.Errorf("detail %s = %q, want %q", k, got.Details[k], v)
		}
	}
	if !maps.Equal(got.Details, backward[0].Details) {
		t.Errorf("the collapsed details depend on arrival order:\n%v\n%v", got.Details, backward[0].Details)
	}
}
