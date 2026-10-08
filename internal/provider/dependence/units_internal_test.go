package dependence

import (
	"os"
	"path/filepath"
	"testing"
)

// TestChildProjectsKeepsEveryPart pins the one invariant of the subdivision
// boundary.
//
// Requirement: every source-bearing subdirectory of a crashed unit is returned.
//
// Mutation that fails it: slice the list to a fixed 512 with no word said. The
// tail of a large unit's source is then never parsed by the only run that could
// still analyse it, and the unit seals looking merely subdivided. No other
// assertion sees a dropped part.
func TestChildProjectsKeepsEveryPart(t *testing.T) {
	root := t.TempDir()
	const parts = 600
	for i := range parts {
		if err := os.MkdirAll(filepath.Join(root, "p"+itoa(int64(i))), 0o700); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
	}
	children, err := childProjects(root)
	if err != nil {
		t.Fatalf("childProjects: %v", err)
	}
	if len(children) != parts {
		t.Fatalf("childProjects returned %d of %d parts: a subdivided unit may not drop source", len(children), parts)
	}
}
