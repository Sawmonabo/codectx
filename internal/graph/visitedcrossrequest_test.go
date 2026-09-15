package graph

import (
	"context"
	"testing"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
)

// slowConvergent is the convergent fixture with a clock attached, the same
// shape slowAdjacency gives the owner-major graphFixture: every every-th
// adjacency read pushes the shared clock past the engine's deadline, which is
// what splits a walk across REQUESTS at a point the test chooses.
type slowConvergent struct {
	*convergentAdjacency
	clock *time.Time
	calls *int
	jump  time.Duration
	every int
}

func (s slowConvergent) Edges(ctx context.Context, nodes []model.NodeID, dir model.Direction,
	kinds []model.RelationKind, after model.RelationID, limit int) ([]model.Relation, error) {
	*s.calls++
	if s.every > 0 && *s.calls%s.every == 0 {
		*s.clock = s.clock.Add(s.jump)
	}
	return s.convergentAdjacency.Edges(ctx, nodes, dir, kinds, after, limit)
}

// TestABackEdgeReachedByALaterRequestIsNeverReadmitted is the CROSS-REQUEST
// half of the append-only walk state's exactly-once invariant -- (d) in
// visitedgrowth_test.go's terms -- and it is the case three earlier attempts
// were masked on.
//
// The property is that a node admitted by one request is not admitted again by
// a later one. Proving it needs a node whose earlier admission lives ONLY in
// the persisted runs:
//
//   - expand builds a fresh visitedSet per leg (traverse.go), so cross-leg
//     membership really does flow through the store rather than through shared
//     heap;
//   - but visitedSet.carried holds every node of the RESUMED FRONTIER and
//     answers from heap, so a node still on the frontier at the boundary is
//     masked. The node has to be already EXPANDED, which in a BFS means an edge
//     from a strictly deeper level back to a shallower node;
//   - and it must be admitted in a NON-FINAL internal leg, because the final
//     leg's admissions are appended by cursor.go before the token is signed and
//     the mutation below does not touch that path.
//
// newBackEdgeAdjacency is that shape: the shared sink is admitted at level 1,
// expanded at level 2 and re-reached from every level after it, while the wide
// leaves spill the frontier ceiling so one request splits into several internal
// legs and the clock splits the walk across requests.
//
// Mutation, run BEFORE the assertions were written and pasted in the lane
// report: walkrun.go's internal-leg append given nil. The walk then reports
// 837 nodes admitted where the fixture holds 822 -- fifteen re-admissions and
// one extra page of re-expansion. The SERVED rows stay 821 and distinct under
// the mutation, which is why this is asserted on visited_count as well:
// rankImpact folds by node identity, so a re-admitted entity collapses back
// into one ranked row and the inflated count is the only faithful symptom.
func TestABackEdgeReachedByALaterRequestIsNeverReadmitted(t *testing.T) {
	const links, width = 20, 40
	// The chain's nodes, its wide leaves, plus the shared sink and its terminal.
	const admitted = links + links*width + 2

	f := newBackEdgeAdjacency(links, width)
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
	limits.QueryTimeout = time.Minute
	limits.CursorTTL = time.Hour
	limits.FrontierBytes = 8 << 10

	base := model.ImpactRequest{GenerationID: 1,
		Start:     []model.NodeID{fixtureNodeID("c-0000")},
		Direction: model.DirectionOutgoing, Relations: []model.RelationKind{model.RelCalls}}

	clock := time.Now()
	calls := 0
	slow := slowConvergent{convergentAdjacency: f, clock: &clock, calls: &calls,
		jump: 2 * time.Minute, every: 64}
	probe := &heapProbe{}
	e, err := New(Options{Adjacency: slow, Signer: signer, Spools: spools,
		Leases: pagination.NewLeases(leases, limits.CursorTTL), Limits: limits,
		Now: func() time.Time { return clock }})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	e.probe = probe

	var (
		pages   int
		visited int64
		entries []model.ImpactEntry
		req     = base
	)
	for {
		pages++
		if pages > 500 {
			t.Fatalf("no termination after %d pages, visited %d", pages-1, visited)
		}
		calls = 0
		res, err := e.Impact(context.Background(), req)
		if err != nil {
			t.Fatalf("page %d: %v", pages, err)
		}
		if res.VisitedCount > visited {
			visited = res.VisitedCount
		}
		entries = append(entries, res.Entries...)
		if res.Meta.NextCursor == "" {
			t.Logf("page %d final: truncated=%v reason=%q", pages, res.Meta.Truncated, res.Meta.TruncationReason)
			break
		}
		req.Page, req.GenerationID = model.PageRequest{Cursor: res.Meta.NextCursor}, 0
	}
	// Non-vacuity, both halves of the premise: the walk really was split
	// across requests, and it really did append runs at internal leg
	// boundaries. A one-request or one-leg walk proves nothing here.
	if pages < 2 {
		t.Fatalf("the walk answered in one request, so nothing crossed a request boundary")
	}
	if probe.VisitedBytes == 0 {
		t.Fatalf("the walk appended no runs at all, so no admission ever had to survive a leg")
	}

	// No node served twice. The ranking folds by node identity, so this holds
	// even under a re-admission; it is asserted because a build that served the
	// same entity on two pages would be the loudest form of the same defect.
	seen := make(map[model.NodeID]bool, len(entries))
	for _, en := range entries {
		if seen[en.NodeID] {
			t.Fatalf("node %s is listed twice across the %d pages", en.NodeID, pages)
		}
		seen[en.NodeID] = true
	}
	if len(entries) != admitted-1 {
		// The seed is walked but is not an affected entity.
		t.Fatalf("the pages served %d affected entit(ies); the fixture holds %d",
			len(entries), admitted-1)
	}

	// No node ADMITTED twice. Equality, never "at least": a walk that
	// re-admitted the sink after an earlier request had already reported it
	// counts it again and re-expands it, and visited_count is where that shows.
	if visited != admitted {
		t.Fatalf("the walk admitted %d nodes across %d requests; the fixture holds %d. The excess "+
			"is a node reached again by a later request whose earlier admission lives only in the "+
			"persisted runs", visited, pages, admitted)
	}
	t.Logf("%d pages, %d nodes admitted exactly once, %d served, %d bytes of runs appended",
		pages, visited, len(entries), probe.VisitedBytes)
}
