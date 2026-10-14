package cli

import (
	"strings"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
)

// TestASearchHitsReasonIsItsTier proves the REASON column says why each hit
// was found from the hit's tier, which is the whole explanation a hit has: an
// exact-tier hit scores 0 and the column names its tier; a hit whose range the
// content store could not supply leads with that fault and counts the tier.
//
// Mutations: render no reason for a hit (the exact hit's zero score is left
// unexplained); put the tier before the fault (the LINE column's "!" is not
// explained first).
func TestASearchHitsReasonIsItsTier(t *testing.T) {
	hit := model.SearchHit{Path: "pkg/a.go", Kind: model.NodeFunction, Tier: model.TierExactName}
	if got, want := reasonCell(hit), "matched the exact_name tier"; got != want {
		t.Errorf("reasonCell(exact_name hit) = %q, want %q", got, want)
	}
	hit.UnresolvedFields = map[string]string{model.SearchHitFieldRange: "CTX_SOURCE_INTEGRITY: blob missing"}
	got := reasonCell(hit)
	if !strings.HasPrefix(got, "range unresolved: CTX_SOURCE_INTEGRITY: blob missing") || !strings.HasSuffix(got, "(+1 more)") {
		t.Errorf("reasonCell(faulted hit) = %q, want the fault first and the tier counted", got)
	}
}
