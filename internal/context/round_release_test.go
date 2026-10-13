package context

import (
	stdcontext "context"
	"slices"
	"strings"
	"testing"

	"github.com/Sawmonabo/codectx/internal/graph"
	"github.com/Sawmonabo/codectx/internal/model"
)

// TestAFinishedRoundsRunsAreReleasedBeforeTheNextRound pins that a compile's
// temporary disk is its largest round's runs, not the sum of its rounds. A
// released run goes back to the scratch pool under the sort area and stays on
// disk for the next taker, so what release buys is reuse: the next round takes
// the surfaces the finished round gave back instead of growing the pool. The
// fixture is the oversized-witness one, which runs more than one round
// (TestALowerFloorFollowsAWitnessTheSliceCannotHold), with every sort's run
// buffer at one record so each sort spills to pooled files.
//
// Every round opens the graph twice, at its P-A and at its P-C, so the pool
// counted at those two opens brackets that round's P-A. A later round's P-A is
// much smaller than the whole first round, so with the first round's runs
// released it finds every surface it needs in the pool and the pool does not
// grow between the two opens.
//
// Mutation caught: dropping the round release in the compile loop. The first
// round's runs are then still leased, so each later round's P-A creates new
// pool files (4 and 5 more on this fixture).
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
	c := intCompiler(t, fx, fx.Now)
	c.runRecords = 1
	var pooled []int
	open := c.graph
	c.graph = func(ctx stdcontext.Context, gen model.GenerationID) (*graph.Engine, func() error, error) {
		pooled = append(pooled, pooledRunFiles(t, c.sortDir))
		return open(ctx, gen)
	}
	var observed []sortObservation
	c.observeSorts = func(o []sortObservation) { observed = o }
	rounds := len(roundStops(t, fx, intCompiler(t, fx, fx.Now), req))
	if rounds < 2 {
		t.Fatalf("the fixture ran %d round(s); it needs two to say anything", rounds)
	}
	if _, err := c.Compile(fx.ctx, req); err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if len(pooled) != 2*rounds {
		t.Fatalf("the compile opened the graph %d times over %d rounds, want two per round", len(pooled), rounds)
	}
	// Each round's P-A opens its seed sort first; every one of them must have
	// spilled, or a later round's P-A took nothing from the pool to prove reuse.
	var seedSpills []int
	for _, o := range observed {
		if o.Name == "scope-seed" {
			seedSpills = append(seedSpills, o.Spilled)
		}
	}
	if len(seedSpills) != rounds || slices.Contains(seedSpills, 0) {
		t.Fatalf("the rounds' P-A seed sorts spilled %v runs, want a spill in each of %d rounds", seedSpills, rounds)
	}
	if pooled[2] == 0 {
		t.Fatalf("the pool %v holds no run file the first round gave back", pooled)
	}
	for r := 1; r < rounds; r++ {
		if before, after := pooled[2*r], pooled[2*r+1]; after != before {
			t.Errorf("round %d's P-A grew the pool from %d to %d run files (pool at every graph open %v): "+
				"the earlier rounds' runs were not released before it", r+1, before, after, pooled)
		}
	}
}
