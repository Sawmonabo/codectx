package workflow

import (
	"strings"
	"testing"

	"github.com/Sawmonabo/codectx/internal/config"
	"github.com/Sawmonabo/codectx/internal/model"
)

// TestObservationReferencesAreUnlimitedByDefault protects the rule that
// workflow.max_observation_references is a caller ceiling and not a scale
// refusal.
//
// The count that matters is the scope review's aggregate: a review carries its
// references inside its eight categories, and a complete review of a large
// scope legitimately carries hundreds. The service used to compare that
// aggregate against a hard 64 and refuse the attestation -- which refused a
// review for the size of the scope it attested to, and no other assertion in
// this package sees it because every fixture review cites a handful of files.
//
// This service ceiling is now the ONLY one: the model's per-list ceiling is
// gone, and TestObservationReferencesCarryNoModelCeiling in internal/model
// holds that half.
//
// Second failure mode: a caller that DID set a ceiling must be told the ceiling
// and the count, or raising it is a guess.
func TestObservationReferencesAreUnlimitedByDefault(t *testing.T) {
	const aggregate = 512 // eight review categories citing 64 read files each

	unlimited := &Service{limits: Limits{MaxObservationReferences: config.Unlimited}}
	if err := unlimited.checkReferenceCount(aggregate, "scope review"); err != nil {
		t.Fatalf("the default (unlimited) reference ceiling refused %d references: %v", aggregate, err)
	}

	bounded := &Service{limits: Limits{MaxObservationReferences: config.Limit(64)}}
	err := bounded.checkReferenceCount(aggregate, "scope review")
	if err == nil {
		t.Fatalf("a caller-set ceiling of 64 admitted %d references without a word", aggregate)
	}
	typed, ok := err.(*model.Error)
	if !ok || typed.Code != model.CodeResourceLimit {
		t.Fatalf("an over-ceiling attestation reported %v, want a %s error", err, model.CodeResourceLimit)
	}
	if typed.Details["limit_value"] != "64" || typed.Details["references"] != "512" {
		t.Fatalf("the report names limit_value %q and references %q, want the caller's 64 and the count 512",
			typed.Details["limit_value"], typed.Details["references"])
	}
	if !strings.Contains(typed.Remediation, "workflow.max_observation_references") {
		t.Fatalf("the report does not name the key to raise: %q", typed.Remediation)
	}
	// Exactly at the ceiling is still admitted: Exceeded is strict.
	if err := bounded.checkReferenceCount(64, "observation"); err != nil {
		t.Fatalf("a count exactly at the caller's ceiling was refused: %v", err)
	}
}
