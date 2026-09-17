package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
)

// TestEveryTextSurfaceRendersOneCapabilityRowsOwnFigures guards the one
// counting rule at the two text surfaces: `codectx status` and the completion
// block of an indexing run render the figures the published capability row
// carries, and neither recomputes them.
//
// Failure mode: the text output printed only a tally of states, so a provider
// that failed two of eleven planned scopes and one that failed two of two read
// identically -- "1 partial" -- while the JSON envelope of the same generation
// carried units_planned and units_failed. An operator comparing the two
// surfaces of one generation saw two different reports, and the human one gave
// no way to tell a mostly healthy capability from a dead one.
//
// Mutation proof: in writeCapabilities, drop the per-row loop under the tally.
func TestEveryTextSurfaceRendersOneCapabilityRowsOwnFigures(t *testing.T) {
	t.Parallel()
	row := model.CapabilityState{ProviderID: "scip", Capability: "precise_definitions",
		Scope: "workspace", State: model.CapabilityPartial,
		DiagnosticCode: model.CodeProviderUnavailable,
		Details: map[string]string{model.DetailUnitsPlanned: "11", model.DetailUnitsFailed: "2",
			"scope_key": "pkg:go:refused"}}
	fresh := model.CapabilityState{ProviderID: "structural", Capability: "outline",
		Scope: "workspace", State: model.CapabilityFresh}

	var status bytes.Buffer
	if err := writeIndexStatus(&status, model.IndexStatus{Completeness: []model.CapabilityState{fresh, row}}); err != nil {
		t.Fatalf("status: %v", err)
	}
	var completion strings.Builder
	writeCapabilities(&completion, []model.CapabilityState{fresh, row})

	for name, out := range map[string]string{"status": status.String(), "the completion block": completion.String()} {
		for _, want := range []string{"scip/precise_definitions", "2 of 11 units failed", "pkg:go:refused"} {
			if !strings.Contains(out, want) {
				t.Errorf("%s does not report %q, so the row's own figures reached no operator:\n%s", name, want, out)
			}
		}
		// A fresh capability has nothing to act on; listing every one of them
		// would bury the degradations the lines exist for.
		if strings.Contains(out, "structural/outline") {
			t.Errorf("%s lists a fresh capability row, burying the degradations:\n%s", name, out)
		}
	}
}
