package delta_test

import "testing"

// TestFullBuildReadsEachTableOncePerWalk builds relinkBase in full and counts
// each member's table reads. A full build walks the members three times, each
// reading every table once: the hierarchy, the resolution and the second
// publication merge (publishSites). The first merge, which publishes the
// targeted declarations of a.go, b.go and util/u.go, reads the endpoints the
// resolution spilled (target_spill.go) and no table.
//
// Mutation that fails it: publishTargets reads the member's table instead of
// the spilled endpoints (a.go, b.go and util/u.go are read four times).
func TestFullBuildReadsEachTableOncePerWalk(t *testing.T) {
	s := newSession(t)
	b := s.only(s.advance(relinkBase))
	s.check("full", []linkBuild{b})
	for _, p := range []string{"a.go", "b.go", "c.go", "d.go", "e.go", "util/u.go"} {
		if n := b.reads[p]; n != 3 {
			t.Errorf("the full build read %s's table %d times, want 3 (one per walk)", p, n)
		}
	}
}
