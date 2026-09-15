package dependence

import (
	"os"
	"path/filepath"
	"testing"
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
