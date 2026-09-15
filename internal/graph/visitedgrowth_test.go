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

// TestANeighboursPageAppendsOnlyItsOwnAdmissions is the same append-only
// invariant on the OTHER paged walk: the neighbours endpoint, which serves
// callers and callees a page at a time and carries its cumulative admitted-node
// set forward across REQUESTS rather than across the internal legs of one.
//
// Impact's measurement above drives a single request split into legs, so it
// proves what a LEG appends. A neighbours walk never chains in process: every
// page is its own request, and the run each page appends is written by
// nextTraversalCursor before the token it hands back is signed. That makes the
// per-page shape visible, and it is the shape a client actually pays:
//
//	(a) the bytes a page grows the cumulative set by are bounded by what THAT
//	    page admitted -- a build that re-wrote the whole set per page would grow
//	    its write with the page number, which is the quadratic walk this layout
//	    closed;
//	(b) the records a page decodes on RESUME are a function of the frontier the
//	    page before it stopped at and never of the cumulative set behind it, so
//	    on this chain-shaped fixture they are flat from the second page to the
//	    last.
//
// The fixture is newConvergentAdjacency: a chain whose every link also calls one
// shared sink, so the walk is many pages deep with a frontier of one or two
// nodes -- which is what makes (b) a constant rather than a per-level width.
//
// Mutation, run and pasted in the lane report: cursor.go's
// `store.appendRun(c.Visited)` given the whole cumulative set (the store
// streamed into a slice) instead of the page's own admissions -- (a) fails.
func TestANeighboursPageAppendsOnlyItsOwnAdmissions(t *testing.T) {
	a := newConvergentAdjacencyOfLength(250)
	signer, err := pagination.OpenSigner(t.TempDir())
	if err != nil {
		t.Fatalf("open signer: %v", err)
	}
	leases := newFixtureLeases()
	spools, err := pagination.NewSpools(t.TempDir(), 1<<20, leases)
	if err != nil {
		t.Fatalf("new spools: %v", err)
	}
	limits := fixtureLimits()
	limits.MaxDepth, limits.MaxVisited, limits.MaxEdges = 0, 0, 0
	limits.MaxPageItems = 4 // a handful of relations a page: the walk pays a request per handful a page: the walk pays a request per edge
	limits.QueryTimeout = 10 * time.Minute
	limits.FrontierBytes = 64 << 10

	probe := &heapProbe{}
	e, err := New(Options{Adjacency: a, Signer: signer, Spools: spools,
		Leases: pagination.NewLeases(leases, limits.CursorTTL), Limits: limits})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	e.probe = probe

	type sample struct {
		page          int
		admitted      int64
		visitedBytes  int64
		resumeRecords int64
	}
	var (
		samples []sample
		req     = model.GraphRequest{GenerationID: 1, Start: []model.NodeID{fixtureNodeID("c-0000")},
			Direction: model.DirectionOutgoing, Relations: []model.RelationKind{model.RelCalls}}
		lastByte, lastRec, lastVisited int64
	)
	for pages := 1; ; pages++ {
		if pages > 5000 {
			t.Fatalf("the neighbours walk did not terminate after %d pages", pages-1)
		}
		res, err := e.Neighbors(context.Background(), req)
		if err != nil {
			t.Fatalf("page %d: %v", pages, err)
		}
		samples = append(samples, sample{page: pages,
			admitted:      res.VisitedCount - lastVisited,
			visitedBytes:  probe.VisitedBytes - lastByte,
			resumeRecords: probe.ResumeRecords - lastRec,
		})
		lastVisited, lastByte, lastRec = res.VisitedCount, probe.VisitedBytes, probe.ResumeRecords
		if res.Meta.NextCursor == "" {
			break
		}
		req.Page, req.GenerationID = model.PageRequest{Cursor: res.Meta.NextCursor}, 0
	}
	if len(samples) < 100 {
		t.Fatalf("the walk took %d pages; a per-page bound read at pages 2, 50 and 100 needs at least a hundred",
			len(samples))
	}

	// (a). One node id, its separator, the filter words a run's probes may
	// dirty and the O(1) manifest an append rewrites -- charged per node the
	// PAGE admitted, because that is what the page hands the store.
	const manifestCeiling = 128
	perNode := int64(len(fixtureNodeID("c-0000")) + 1 + visitedFilterProbes*8 + manifestCeiling)
	for _, s := range samples {
		if bound := s.admitted * perNode; s.visitedBytes > bound {
			t.Fatalf("page %d admitted %d node(s) and grew the cumulative set by %d bytes; its own "+
				"admissions bound that at %d. A page re-writing the whole set is what the excess reads as",
				s.page, s.admitted, s.visitedBytes, bound)
		}
	}

	// (b). The records a resume decodes never grow with the page number: this
	// chain stops at a frontier of a handful of nodes whatever page it is on,
	// so no page may decode more than that frontier holds. A resume that
	// replayed the cumulative set instead would decode one record per node
	// admitted so far, which on the last page here is two hundred and fifty.
	const frontierRecords = 8
	for _, s := range samples {
		if s.resumeRecords > frontierRecords {
			t.Fatalf("page %d decoded %d continuation record(s) on resume for a walk whose frontier "+
				"holds at most %d: a resume is replaying the cumulative set, not the frontier",
				s.page, s.resumeRecords, frontierRecords)
		}
	}
	// Flat, and not merely bounded, once the walk has reached its steady state:
	// pages 50 and 100 stop at the same frontier and must cost the same.
	if got, want := samples[99].resumeRecords, samples[49].resumeRecords; got != want {
		t.Fatalf("page 100 decoded %d continuation record(s) on resume, page 50 decoded %d: the cost of "+
			"a resume is moving with the page number", got, want)
	}

	for _, s := range samples {
		if s.page <= 3 || s.page == 50 || s.page == 100 || s.page == len(samples) {
			t.Logf("page %3d: admitted %d, visited bytes appended %4d, resume records %2d",
				s.page, s.admitted, s.visitedBytes, s.resumeRecords)
		}
	}
}

// TestASmallPageNeighboursWalkReachesEveryNode is the page-limit half of the
// level-boundary rule, and it is a silent-truncation proof.
//
// The page item limit can spend its last item on one level and stop the visitor
// on the very FIRST row of the next. Nothing on that new level has advanced the
// keyset position, so the continuation carries the previous level's -- and
// levelEdges applies a carried position as a FILTER, dropping every row of the
// new level whose owner sorts below it. Those owners are already in the
// cumulative visited set, so no later page can reach them: the walk ends with
// no cursor, no truncation reason, and a fraction of the graph. A short answer
// presented as a whole one is the one failure the paging contract may not have.
//
// Measured before the fix, on this owner-major fixture (57 reachable nodes):
//
//	limit 1: 38 pages, visited 39, reason "", cursor false
//	limit 2: 19 pages, visited 39, reason "", cursor false
//	limit 4: 10 pages, visited 39, reason "", cursor false
//	limit 8:  5 pages, visited 39, reason "", cursor false
//
// (limit 3 happened to reach 57 -- whether the loss occurs at all depends on
// where the item limit falls relative to a level edge, which is why the case
// sweeps several limits rather than picking one.)
//
// Mutation (applied, run, reverted in one command): expand's errStopExpansion
// branch returning LevelBoundary:false as it did -- every limit but 3 fails
// with the figures above.
func TestASmallPageNeighboursWalkReachesEveryNode(t *testing.T) {
	// 8 mids x 6 leaves plus the seed: 57 nodes the walk must visit, over a
	// fixture whose reader is owner-major, so nothing here depends on the
	// order a fixture happens to return rows in.
	const want = 57
	f := newReachableFixture(t, 8, 6)
	for _, limit := range []int{1, 2, 3, 4, 8} {
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
		limits.MaxPageItems = limit
		limits.QueryTimeout = 10 * time.Minute
		e, err := New(Options{Adjacency: f, Signer: signer, Spools: spools,
			Leases: pagination.NewLeases(leases, limits.CursorTTL), Limits: limits})
		if err != nil {
			t.Fatalf("new engine: %v", err)
		}
		req := model.GraphRequest{GenerationID: 1,
			Start:     []model.NodeID{fixtureNodeID("bound-seed")},
			Direction: model.DirectionOutgoing, Relations: []model.RelationKind{model.RelCalls}}
		var res model.GraphResult
		for pages := 1; ; pages++ {
			if pages > 5000 {
				t.Fatalf("limit %d: the walk did not terminate after %d pages", limit, pages-1)
			}
			res, err = e.Neighbors(context.Background(), req)
			if err != nil {
				t.Fatalf("limit %d page %d: %v", limit, pages, err)
			}
			if res.Meta.NextCursor == "" {
				break
			}
			req.Page, req.GenerationID = model.PageRequest{Cursor: res.Meta.NextCursor}, 0
		}
		if res.VisitedCount != want {
			t.Fatalf("at page limit %d the walk ended having visited %d of %d nodes, with "+
				"truncation_reason %q and no cursor: a walk that stops short must say so",
				limit, res.VisitedCount, want, res.Meta.TruncationReason)
		}
		if res.Meta.Truncated {
			t.Fatalf("at page limit %d the complete walk reports truncation %q",
				limit, res.Meta.TruncationReason)
		}
	}
}
