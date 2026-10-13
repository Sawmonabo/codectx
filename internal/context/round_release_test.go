package context

import (
	stdcontext "context"
	"os"
	"strings"
	"testing"

	"github.com/Sawmonabo/codectx/internal/graph"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
)

// TestAFinishedRoundsRunsAreReleasedBeforeTheNextRound pins that a compile's
// temporary disk is its largest round's runs, not the sum of its rounds: every
// run file the first round wrote is gone by the time the last round reads the
// graph. The fixture is the oversized-witness one, which runs more than one
// round (TestALowerFloorFollowsAWitnessTheSliceCannotHold), under the smallest
// run budget so its sorts spill to files.
//
// Mutation caught: dropping the round release in the compile loop, so the
// first round's run files are still in the sort area when the last round runs.
func TestAFinishedRoundsRunsAreReleasedBeforeTheNextRound(t *testing.T) {
	wide := seedFile("internal/pay/b.go", seedHead)
	wide.content = "package pay\n\n" + strings.Repeat("// a witness whose group the slice cannot hold\n", 64) +
		"func f() {}\n"
	specs := []fixtureFileSpec{
		seedFile("internal/pay/a.go", seedHead), wide, seedFile("internal/pay/c.go", seedHead),
		seedFile("internal/pay/d.go", seedHalf),
		seedFile("internal/pay/t1.go", seedTail), seedFile("internal/pay/t2.go", seedTail),
	}
	slice, err := wireEncodedBytes(int64(len(wide.content)))
	if err != nil {
		t.Fatalf("wireEncodedBytes: %v", err)
	}
	req := model.ContextRequest{Task: seedTask, Phase: model.PhaseVerify,
		Budget: model.Budget{MaxFiles: 2, MaxBytes: slice, MaxSlices: 8, MaxEstimatedTokens: 1 << 20}}
	fx := newFixtureFrom(t, specs, nil)
	fx.Cfg.Resources.QueryMemoryBytes = 1
	c := intCompiler(t, fx, fx.Now)
	if c.cfg.Resources.QueryMemoryBytes != 1 || pagination.SortRunBytes(1) <= 0 {
		t.Fatal("the compiler does not run under the smallest run budget")
	}
	// The graph is opened by every round's P-A and P-C, so a listing of the
	// sort area at each open sees each round's live runs.
	var listings [][]string
	open := c.graph
	c.graph = func(ctx stdcontext.Context, gen model.GenerationID) (*graph.Engine, func() error, error) {
		names, err := os.ReadDir(c.sortDir)
		if err != nil {
			t.Fatalf("list the sort area: %v", err)
		}
		var l []string
		for _, n := range names {
			l = append(l, n.Name())
		}
		listings = append(listings, l)
		return open(ctx, gen)
	}
	if stops := roundStops(t, fx, intCompiler(t, fx, fx.Now), req); len(stops) < 2 {
		t.Fatalf("the fixture ran %d round(s); it needs two to say anything", len(stops))
	}
	if _, err := c.Compile(fx.ctx, req); err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if len(listings) < 3 || len(listings[1]) == 0 {
		t.Fatalf("the sort area listings %q show no first-round run to release", listings)
	}
	// listings[1] is the first round's P-C: every run its P-A wrote is live.
	last := map[string]bool{}
	for _, n := range listings[len(listings)-1] {
		last[n] = true
	}
	for _, n := range listings[1] {
		if last[n] {
			t.Errorf("the first round's run %s is still in the sort area when the last round reads the graph", n)
		}
	}
}
