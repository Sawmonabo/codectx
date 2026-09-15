package dependence

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
)

// TestChildProjectsKeepsEveryPart pins the one invariant of the subdivision
// boundary: every source-bearing subdirectory of a crashed unit is returned.
// The list used to be sliced to a fixed 512 with no word said, so the tail of
// a large unit's source was never parsed by the only run that could still
// analyse it and the unit sealed looking merely subdivided. No other assertion
// sees a dropped part.
func TestChildProjectsKeepsEveryPart(t *testing.T) {
	root := t.TempDir()
	const parts = 600
	for i := range parts {
		if err := os.MkdirAll(filepath.Join(root, "p"+itoa(int64(i))), 0o700); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
	}
	children, err := childProjects(root, Unit{ScopeKey: "pkg:go:", Family: FamilyGo})
	if err != nil {
		t.Fatalf("childProjects: %v", err)
	}
	if len(children) != parts {
		t.Fatalf("childProjects returned %d of %d parts: a subdivided unit may not drop source", len(children), parts)
	}
}

// TestAClippedFactIsDisclosedOnEveryCapability protects the contract
// index.max_evidence_per_fact carries in internal/config/config.go: "the cut is
// reported on the unit's capability detail, never silent".
//
// The failure mode: the import removes occurrences from facts it then publishes
// whole-looking. A consumer that counts occurrences — a reviewer asking how
// many call sites reach a sink — reads a number the clip decided, and with
// every capability left fresh there is nothing on the generation that says so.
// A log line is not that disclosure: it reaches no reader of the index.
func TestAClippedFactIsDisclosedOnEveryCapability(t *testing.T) {
	for _, c := range []struct {
		name    string
		pub     publication
		want    model.CapabilityStateValue
		wantDet string
	}{
		{name: "clipped", pub: publication{ClippedEvidence: 7}, want: model.CapabilityPartial, wantDet: "7"},
		{name: "not clipped", pub: publication{}, want: model.CapabilityFresh},
	} {
		t.Run(c.name, func(t *testing.T) {
			rows := c.pub.capabilities("scope")
			if len(rows) != len(Capabilities) {
				t.Fatalf("got %d capability rows, want %d", len(rows), len(Capabilities))
			}
			for _, row := range rows {
				if row.State != c.want {
					t.Errorf("%s = %q, want %q", row.Capability, row.State, c.want)
				}
				if got := row.Details[model.DetailEvidenceClipped]; got != c.wantDet {
					t.Errorf("%s detail %s = %q, want %q", row.Capability,
						model.DetailEvidenceClipped, got, c.wantDet)
				}
			}
		})
	}
}
