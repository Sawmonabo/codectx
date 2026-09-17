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
// It also protects the row's REMEDIATION reaching that text. Failure mode: the
// provider's one sentence about what to do is carried all the way to the row
// and then not printed, so the human surfaces report a failure with no way to
// act on it while the JSON one carries the answer.
//
// It also protects the `failed` section: the reasons `status` reads off the run
// rows, each with its scope, its code, its message and its remediation, and the
// count of the reasons that did not fit the page. Failure mode: a bounded
// failure list served as the whole of it, or not printed at all, is the one
// thing the surface whose job is reporting failures must never be.
//
// The third figure is the row's units_running: a scope of this capability is
// still being built in the background, and a text surface that says only
// "partial" leaves an operator escalating work that is about to finish.
//
// Mutation proof: in writeCapabilities, drop the per-row loop under the tally;
// for the remediation, drop the `st.Remediation` line beneath it; for the
// running count, drop the `st.UnitsRunning` branch in capabilityFigures; for
// the reasons, drop the writeFailedUnits call from writeIndexStatus.
func TestEveryTextSurfaceRendersOneCapabilityRowsOwnFigures(t *testing.T) {
	t.Parallel()
	row := model.CapabilityState{ProviderID: "scip", Capability: "precise_definitions",
		Scope: "workspace", State: model.CapabilityPartial,
		DiagnosticCode: model.CodeProviderUnavailable,
		Remediation:    "install the analyzer for this language family, or disable the provider",
		UnitsRunning:   1,
		Details: map[string]string{model.DetailUnitsPlanned: "11", model.DetailUnitsFailed: "2",
			model.DetailScopeKey: "pkg:go:refused"}}
	fresh := model.CapabilityState{ProviderID: "structural", Capability: "outline",
		Scope: "workspace", State: model.CapabilityFresh}

	// The reasons `status` reads off the run rows of the active generation:
	// the capability row above names ONE exemplar scope per provider
	// capability, and this is every failed scope with its own reason.
	failed := model.RunFailure{ProviderID: "scip", ScopeKey: "pkg:java:app",
		Code: model.CodeProviderUnavailable, Message: "the analyzer produced no facts for this unit",
		Remediation: "check that this unit's source files hold definitions this language family parses"}

	var status bytes.Buffer
	if err := writeIndexStatus(&status, model.IndexStatus{Completeness: []model.CapabilityState{fresh, row},
		FailedUnits: []model.RunFailure{failed}, FailedUnitsOmitted: 2}); err != nil {
		t.Fatalf("status: %v", err)
	}
	var completion strings.Builder
	writeCapabilities(&completion, []model.CapabilityState{fresh, row})

	for name, out := range map[string]string{"status": status.String(), "the completion block": completion.String()} {
		for _, want := range []string{"scip/precise_definitions", "2 of 11 units failed", "pkg:go:refused",
			"remediation: install the analyzer", "1 unit is still building"} {
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
	// The failed section belongs to `status` alone -- the completion block
	// reports the run it just did, not the run rows of a generation -- so it
	// is asserted outside the loop above.
	for _, want := range []string{failed.ScopeKey, string(failed.Code), failed.Message,
		"remediation: check that this unit's", "2 further reasons are not shown"} {
		if !strings.Contains(status.String(), want) {
			t.Errorf("status does not report %q, so the reason this scope has no facts reached no operator:\n%s",
				want, status.String())
		}
	}
	// Raw analyzer output stays on the run row: Section 6 keeps it out of
	// every ordinary surface, and this one prints a reason per failed scope.
	if strings.Contains(status.String(), model.DetailStderrTail) {
		t.Errorf("status names the standard-error detail, which never leaves the run row:\n%s", status.String())
	}
}
