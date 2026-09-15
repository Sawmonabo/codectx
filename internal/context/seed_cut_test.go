package context

import (
	"strings"
	"testing"
)

// TestTheSeedCutSurvivesIntoAValidManifestExclusion protects row 20's seed cut
// end to end, through the one place it can go wrong silently.
//
// Failure mode it guards: seed discovery used to stop at model.MaxSeeds without
// saying so. Reporting the cut is only an improvement if the row it produces is
// a VALID exclusion — model.ContextReference refuses a reference that names
// neither a node, a file nor a path, so a cut row carrying only a reason would
// convert a silent truncation into a failed context compile every time a task
// named more than MaxSeeds identities. No compiler fixture exceeds 64 seeds, so
// nothing else in this package would catch that.
func TestTheSeedCutSurvivesIntoAValidManifestExclusion(t *testing.T) {
	var s seedSet
	s.noteCut(originChangedFile, "the captured working-tree changes")
	s.noteCut(originChangedFile, "the captured working-tree changes")
	if !s.Unresolved {
		t.Fatal("a seed cut left the scope reported as complete")
	}
	if len(s.Excluded) != 1 {
		t.Fatalf("one step's cut produced %d exclusion rows, want exactly one", len(s.Excluded))
	}

	p, err := buildPlan(s.Excluded, nil, resolvedBudget{MaxBytes: 1 << 20, MaxTokens: 1 << 20, MaxFiles: 8, MaxSlices: 8})
	if err != nil {
		t.Fatalf("budgeting the cut row failed: %v", err)
	}
	if len(p.Excluded) != 1 {
		t.Fatalf("the plan carries %d exclusions, want the cut row", len(p.Excluded))
	}
	if err := p.Excluded[0].Validate(); err != nil {
		t.Fatalf("the cut exclusion is not a valid manifest row, so reporting the cut would fail the compile: %v", err)
	}
	if !strings.Contains(p.Excluded[0].Reason, "seed bound") {
		t.Fatalf("the exclusion reason %q does not name the bound that cut the step", p.Excluded[0].Reason)
	}
}
