package graph

import (
	"context"
	"testing"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
)

// TestAWalkWritesItsVisitedSetOnce is the append-only walk state's invariant,
// and it is a correctness assertion rather than a benchmark: a walk that is
// split into many legs must write each admitted node ONCE, so what the
// cumulative set costs is a function of what the walk admitted and never of how
// many legs it took. The layout before this one re-wrote the WHOLE cumulative
// set at every leg, which reaches exactly the same answer and pages a walk to
// completion in quadratic time -- measured on a real repository at 0.31 s/page
// over the first fifty pages and 2.20 s/page by page 400. Nothing but a
// measurement can tell the two builds apart, which is why the bytes are probed.
//
// The walk below is split into many legs by a small frontier byte ceiling:
// every time the ceiling stops a level, runWalkToCompletion appends that leg's
// own admissions as one more ascending run and carries on. Two things are then
// asserted over a twenty-thousand-node walk:
//
//	(a) the bytes the cumulative set grew by are bounded by what the walk
//	    ADMITTED times a per-node ceiling read off the layout -- a per-leg
//	    rewrite is several times that;
//	(d) visited_count is EXACTLY the fixture's node count, so no node was
//	    admitted a second time across a leg boundary. It is asserted on the
//	    count and not on the served rows on purpose: rankImpact folds by node
//	    identity, so a re-admitted entity collapses back into one ranked row and
//	    the only symptoms are the inflated count and the wasted re-expansion.
//
// (b) -- what a RESUME decodes -- and the per-PAGE shape of (a) are not proven
// here; see the lane report. (c), the incremental re-adoption of the retained
// directory, is TestReadoptingAGrownDirectoryChargesItOnce in
// internal/pagination.
//
// Mutations, run and pasted in the lane report:
//   - the whole visited set re-appended per leg (walkrun.go's
//     `state.Admitted.addedNodes()` replaced by the cumulative set): (a) fails.
//   - the leg's append deleted (walkrun.go `o.Visited.appendRun`): (d) fails --
//     the walk re-admits nodes an earlier leg had already reported.
func TestAWalkWritesItsVisitedSetOnce(t *testing.T) {
	// 100 mid nodes, 200 leaves each: 20 100 admitted nodes over three levels.
	const mids, fanOut = 100, 200
	const admitted = mids*(1+fanOut) + 1 // the seed is admitted too

	f := newReachableFixture(t, mids, fanOut)
	signer, err := pagination.OpenSigner(t.TempDir())
	if err != nil {
		t.Fatalf("open signer: %v", err)
	}
	leases := newFixtureLeases()
	spools, err := pagination.NewSpools(t.TempDir(), 0, leases)
	if err != nil {
		t.Fatalf("new spools: %v", err)
	}
	limits := fixtureLimits()
	limits.MaxDepth, limits.MaxVisited, limits.MaxEdges = 0, 0, 0
	limits.MaxPageItems = 200
	limits.QueryTimeout = 10 * time.Minute
	// The frontier ceiling is what splits this walk into legs: it is far below
	// what twenty thousand leaves need, so the walk crosses it many times and
	// every crossing appends another run to the same store.
	limits.FrontierBytes = 64 << 10

	probe := &heapProbe{}
	e, err := New(Options{Adjacency: f, Signer: signer, Spools: spools,
		Leases: pagination.NewLeases(leases, limits.CursorTTL), Limits: limits})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	e.probe = probe

	res, err := e.Impact(context.Background(), model.ImpactRequest{
		GenerationID: 1, Start: []model.NodeID{fixtureNodeID("bound-seed")},
		Direction: model.DirectionOutgoing, Relations: []model.RelationKind{model.RelCalls}})
	if err != nil {
		t.Fatalf("impact: %v", err)
	}

	// (d). Equality, never "at least": a walk that re-admitted a node reports
	// MORE than the fixture holds, and that is the whole symptom.
	if res.VisitedCount != admitted {
		t.Fatalf("the walk admitted %d nodes; the fixture holds %d. A difference above it is a "+
			"node re-admitted after an earlier leg had already reported it",
			res.VisitedCount, admitted)
	}

	// Non-vacuity: the walk really was split. A single-leg walk appends nothing
	// at all -- the last leg's admissions are never written, because nothing
	// resumes from them -- so a positive count IS the proof that legs happened.
	if probe.VisitedBytes == 0 {
		t.Fatalf("the walk wrote no cumulative set at all: it took one leg, and a per-leg bound " +
			"over one leg proves nothing")
	}

	// (a). One node id, its separator, the filter words its probes may dirty
	// and the manifest an append rewrites -- charged per admitted node, because
	// a leg may admit as few as one and rewrites the manifest either way.
	const manifestCeiling = 128
	perNode := int64(len(fixtureNodeID("bound-seed")) + 1 + visitedFilterProbes*8 + manifestCeiling)
	if bound := res.VisitedCount * perNode; probe.VisitedBytes > bound {
		t.Fatalf("a %d-node walk grew its cumulative set by %d bytes; what it admitted bounds "+
			"that at %d. A set re-written once per leg is what the excess reads as",
			res.VisitedCount, probe.VisitedBytes, bound)
	}
	t.Logf("%d nodes admitted, cumulative set grew by %d bytes (ceiling %d)",
		res.VisitedCount, probe.VisitedBytes, res.VisitedCount*perNode)
}
