package index

import (
	"strings"
	"testing"

	"github.com/Sawmonabo/codectx/internal/config"
)

// TestRefusedRetentionPolicyReachesStatus protects the ONE route a retention
// sweep that never ran has into the product: Coordinator.retain records the
// failure on retentionState, and Status projects it as a warning.
//
// Failure mode it protects. retain may not fail the run that published, so a
// sweep that the store refuses is otherwise invisible: every generation
// publishes successfully, `codectx index status` reports a healthy binding, and
// the store grows without bound because nothing is ever reclaimed. A refused
// policy is the case that matters precisely because it fails every pass
// identically -- there is no later pass that quietly fixes it. Deleting either
// the record call (retention.go) or the project call (status.go) restores that
// silence while leaving every other index test green, which is why this row
// asserts the warning text end to end rather than the two halves separately.
//
// A negative retain_refs is the refusal the store actually implements
// (sqlite.Store.RetainByRef validates the policy before it touches a row), so
// the sweep fails for the product's own reason and not for a stubbed one.
func TestRefusedRetentionPolicyReachesStatus(t *testing.T) {
	f := newFixture(t, map[string]string{"pkg/a.go": "package pkg\n\nfunc A() {}\n"})
	// Set before the coordinator is built: retain reads the policy out of the
	// coordinator's own configuration on every pass.
	f.cfg.Index.RetainRefs = config.Limit(-1)
	c := f.coordinator(f.providers(false))

	if _, err := c.Refresh(f.ctx, nil); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	st, err := f.status(c)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	const want = "the last retention sweep did not finish"
	for _, w := range st.Warnings {
		if strings.Contains(w, want) {
			if err := st.Validate(); err != nil {
				t.Fatalf("the status carrying the retention warning does not validate: %v", err)
			}
			return
		}
	}
	t.Fatalf("the generation published with retention refusing every sweep and status warned %v; "+
		"a store that reclaims nothing must say so", st.Warnings)
}
