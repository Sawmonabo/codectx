package index

import (
	"testing"

	"github.com/Sawmonabo/codectx/internal/provider"
)

// How many units a generation builds at once comes from the machine, never
// from a typed-in ceiling. Mutation: put any fixed ceiling back into
// workerCount -- the eight this replaces -- and the sixteen-core row fails,
// which is a machine building half the units it can run while the audit
// table calls the figure derived.
func TestWorkerCountComesFromTheMachine(t *testing.T) {
	for _, tc := range []struct {
		name       string
		configured int
		cpus       int
		want       int
	}{
		{"a sixteen-core machine builds sixteen", 0, 16, 16},
		{"a one-core machine builds one", 0, 1, 1},
		{"a machine with more cores than live sinks stops at the ceiling", 0, provider.MaxLiveSinks * 2, provider.MaxLiveSinks},
		{"a stated count is honoured under the same ceiling", 3, 16, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := workerCount(tc.configured, tc.cpus); got != tc.want {
				t.Fatalf("workerCount(%d, %d) = %d, want %d", tc.configured, tc.cpus, got, tc.want)
			}
		})
	}
}
