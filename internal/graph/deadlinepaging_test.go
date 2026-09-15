package graph

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
)

// TestADeadlineSplitWalkAlwaysAdvances is the livelock proof, and the
// cross-REQUEST half of the append-only walk state's invariant.
//
// Ruling P3 lets the query deadline end a PAGE of an impact walk and carry the
// frontier forward in a cursor, even when the page served nothing. Taken alone
// that rule has a hole: a page whose FIRST adjacency read already runs past the
// deadline admits nothing and leaves the frontier and the keyset position
// exactly where it found them, so the cursor it mints is the one it was given.
// A client following the chain then pages forever -- measured as a
// visited_count frozen at 101 for five thousand pages -- and no bound in the
// system ever ends it.
//
// Two shapes are asserted, because both must hold:
//
//	progress: when the clock passes the deadline on the N-th round trip of each
//	  page (N > 1, so each page still reaches an edge), the chain reaches
//	  exhaustion; the pages' union is the unbounded walk's answer, in the same
//	  global order, with no entity listed twice; visited_count reaches the
//	  fixture's distinct node count across the REQUESTS the walk was split over
//	  (the cross-request half of visitedgrowth_test.go's (d)); and the records a
//	  resume decodes stay flat as the pages go on -- they are a function of the
//	  frontier, never of the cumulative set behind it.
//
//	no progress: when the clock passes the deadline on EVERY round trip, so no
//	  page can reach an edge at all, the answer TERMINATES -- truncated, with
//	  reasonDeadlineStalled and no cursor -- instead of minting the cursor again.
//
// Mutations, run and pasted in the lane report:
//   - walkStalled's call site deleted in impact.go (the deadline checked, and a
//     continuation minted, before the page's first admission): the no-progress
//     case pages to the cap and fails.
//   - rankedSpoolHeader made to stream the whole spool instead of stopping after
//     the header record: the chain reads 880 380 800 bytes of an 8 716 646 byte
//     spool over 102 pages and (1c) fails.
//   - walkrun.go's internal-leg append deleted (`o.Visited.appendRun` given nil):
//     NOT detected here, and this is a real gap rather than a slack assertion.
//     The whole walk of this fixture completes inside request ONE -- page 1
//     admits all 20 101 nodes and serves none, and pages 2..102 are pure
//     ranked-tail reads that walk nothing -- so re-admission inside a request is
//     prevented by the in-memory visitedSet the legs share, not by the run the
//     append persists. Deleting the append drops page 1's appended bytes from
//     2 473 737 to 921 552 and leaves visited_count at 20 101 and the page union
//     unchanged, because no later REQUEST ever sweeps the persisted set for
//     admission. The cross-request half of (d) therefore needs a fixture with at
//     least three deadline-split legs in which a later REQUEST reaches a node an
//     earlier one admitted; it is not proven by this test and is not claimed.
func TestADeadlineSplitWalkAlwaysAdvances(t *testing.T) {
	// 100 mid nodes, 200 leaves each: 20 100 admitted nodes over three levels.
	const mids, fanOut = 100, 200
	const admitted = mids*(1+fanOut) + 1 // the seed is admitted too
	const pageCap = 5000

	f := newReachableFixture(t, mids, fanOut)
	signer, err := pagination.OpenSigner(t.TempDir())
	if err != nil {
		t.Fatalf("open signer: %v", err)
	}
	store := newFixtureLeases()
	spoolDir := t.TempDir()
	spools, err := pagination.NewSpools(spoolDir, 0, store)
	if err != nil {
		t.Fatalf("new spools: %v", err)
	}
	limits := fixtureLimits()
	limits.MaxDepth, limits.MaxVisited, limits.MaxEdges = 0, 0, 0
	limits.MaxPageItems = 200
	limits.QueryTimeout = time.Minute
	limits.FrontierBytes = 8 << 20

	base := model.ImpactRequest{GenerationID: 1,
		Start:     []model.NodeID{fixtureNodeID("bound-seed")},
		Direction: model.DirectionOutgoing, Relations: []model.RelationKind{model.RelCalls}}

	whole, err := New(Options{Adjacency: f, Signer: signer, Spools: spools,
		Leases: pagination.NewLeases(store, limits.CursorTTL), Limits: limits})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	ground, err := whole.Impact(context.Background(), base)
	if err != nil {
		t.Fatalf("unbounded impact: %v", err)
	}
	// The unbounded run is itself paged by MaxPageItems; walk it to the end so
	// the comparison is against the WHOLE ranking.
	unbounded := append([]model.ImpactEntry(nil), ground.Entries...)
	for req, cur := base, ground.Meta.NextCursor; cur != ""; {
		req.Page, req.GenerationID = model.PageRequest{Cursor: cur}, 0
		res, err := whole.Impact(context.Background(), req)
		if err != nil {
			t.Fatalf("unbounded impact continuation: %v", err)
		}
		unbounded = append(unbounded, res.Entries...)
		cur = res.Meta.NextCursor
	}
	if len(unbounded) != admitted-1 {
		// The seed itself is walked but is not an affected entity.
		t.Fatalf("the unbounded walk served %d entities; the fixture holds %d", len(unbounded), admitted-1)
	}

	t.Run("progress", func(t *testing.T) {
		clock := time.Now()
		calls, fired := 0, false
		// The clock passes the deadline on the 512th round trip of each page:
		// the counter is reset below before every request, so each page reads a
		// five hundred and eleven batches -- more than the widest level of this
		// fixture needs -- before its own fresh deadline expires. A jump on a
		// page's FIRST read is the no-progress case below -- a different
		// assertion, because such a page cannot advance at all.
		slow := slowAdjacency{graphFixture: f, clock: &clock, calls: &calls,
			jump: 2 * time.Minute, fired: &fired, every: 512}
		probe := &heapProbe{}
		paged, err := New(Options{Adjacency: slow, Signer: signer, Spools: spools,
			Leases: pagination.NewLeases(store, limits.CursorTTL), Limits: limits,
			Now: func() time.Time { return clock }})
		if err != nil {
			t.Fatalf("new engine: %v", err)
		}
		paged.probe = probe

		type sample struct {
			page            int
			resumeRecords   int64
			visitedBytes    int64
			spoolRead       int64
			spoolWritten    int64
			latency         time.Duration
			served, visited int64
		}
		var (
			samples   []sample
			entries   []model.ImpactEntry
			maxSeen   int64
			req       = base
			lastRec   int64
			lastByte  int64
			lastRead  int64
			lastWrite int64
		)
		for pages := 1; ; pages++ {
			if pages > pageCap {
				t.Fatalf("the deadline-split walk did not terminate after %d pages: visited_count reached %d of %d",
					pages-1, maxSeen, admitted)
			}
			calls = 0
			start := time.Now()
			res, err := paged.Impact(context.Background(), req)
			elapsed := time.Since(start)
			if err != nil {
				t.Fatalf("page %d: a deadline threw the answer away instead of ending the page: %v", pages, err)
			}
			if res.Meta.Truncated && res.Meta.TruncationReason == reasonDeadlineStalled {
				t.Fatalf("page %d reported no progress though the clock jumps only every 512th read of the page: %s",
					pages, res.Meta.TruncationReason)
			}
			if res.VisitedCount > maxSeen {
				maxSeen = res.VisitedCount
			}
			samples = append(samples, sample{page: pages,
				resumeRecords: probe.ResumeRecords - lastRec, visitedBytes: probe.VisitedBytes - lastByte,
				spoolRead: probe.SpoolBytesRead - lastRead, spoolWritten: probe.SpoolBytesWritten - lastWrite,
				latency: elapsed, served: int64(len(res.Entries)), visited: res.VisitedCount})
			lastRec, lastByte = probe.ResumeRecords, probe.VisitedBytes
			lastRead, lastWrite = probe.SpoolBytesRead, probe.SpoolBytesWritten
			entries = append(entries, res.Entries...)
			if res.Meta.NextCursor == "" {
				break
			}
			req.Page, req.GenerationID = model.PageRequest{Cursor: res.Meta.NextCursor}, 0
		}

		// The union is the unbounded answer: same set, same global order, no
		// entity twice. Per-page ranking would reorder it; a re-walked leg
		// would duplicate it.
		if len(entries) != len(unbounded) {
			t.Fatalf("the pages served %d affected entit(ies), the unbounded walk %d",
				len(entries), len(unbounded))
		}
		seen := make(map[model.NodeID]bool, len(entries))
		for i, want := range unbounded {
			if entries[i].NodeID != want.NodeID || entries[i].ScoreMicros != want.ScoreMicros ||
				entries[i].Depth != want.Depth {
				t.Fatalf("at rank %d the pages report %s (score %d, depth %d), the unbounded walk %s (score %d, depth %d)",
					i, entries[i].NodeID, entries[i].ScoreMicros, entries[i].Depth,
					want.NodeID, want.ScoreMicros, want.Depth)
			}
			if seen[entries[i].NodeID] {
				t.Fatalf("node %s is listed twice across the pages", entries[i].NodeID)
			}
			seen[entries[i].NodeID] = true
		}

		// (d) across REQUESTS: every distinct node once, whatever the walk was
		// split into. More than the fixture holds is a node re-admitted after
		// an earlier request had already reported it.
		if maxSeen != admitted {
			t.Fatalf("the walk admitted %d nodes across %d pages; the fixture holds %d",
				maxSeen, len(samples), admitted)
		}

		// The page count is bounded by the answer, not by the walk: one page per
		// MaxPageItems rows of the ranking, plus the deadline legs the walk took.
		if bound := len(unbounded)/limits.MaxPageItems + 1 + len(samples)/2; len(samples) > bound {
			t.Fatalf("the walk took %d pages to serve %d rows at %d rows a page",
				len(samples), len(unbounded), limits.MaxPageItems)
		}

		// (1b): the records a resume decodes are a function of the FRONTIER the
		// page before it stopped at, never of the cumulative set behind it. The
		// whole chain therefore decodes at most one record per node the walk
		// ever admitted; a resume that replayed the cumulative set would decode
		// that many on EVERY leg, which is the quadratic shape this state
		// layout closed.
		var resumed int
		for _, s := range samples {
			if s.resumeRecords > 0 {
				resumed++
			}
		}
		if resumed == 0 {
			t.Fatalf("no page resumed a walk at all: the deadline never split it, so a per-resume "+
				"bound over %d pages proves nothing", len(samples))
		}
		if probe.ResumeRecords > maxSeen {
			t.Fatalf("the chain decoded %d continuation records over %d resume(s) for a walk that "+
				"admitted %d nodes: a resume is replaying the cumulative set, not the frontier",
				probe.ResumeRecords, resumed, maxSeen)
		}

		// (1c): the RANKED tail is read-once, not copied forward. The ranking
		// is settled by the page that spills it, so every page after that one
		// must write nothing and read only its own page out of the one spool.
		// A build that copies the remainder into a fresh spool per page reads
		// -- and writes -- the whole unserved remainder on every page, which is
		// the falling per-page latency this measurement replaced and a
		// quadratic walk to completion.
		var ranked int
		for _, s := range samples {
			if s.spoolWritten == 0 && s.spoolRead == 0 {
				continue
			}
			if ranked++; ranked == 1 {
				// The page that SPILLS the remainder writes it once; that is
				// the one page whose cost is the whole tail.
				continue
			}
			if s.spoolWritten != 0 {
				t.Fatalf("page %d rewrote %d byte(s) of the ranked spool: the ranking is settled, so a "+
					"page after the spill must copy nothing", s.page, s.spoolWritten)
			}
		}
		if ranked < 3 {
			t.Fatalf("only %d page(s) touched the ranked spool: a per-page bound over them proves nothing", ranked)
		}
		// The whole chain reads the spool about ONCE: its bytes, plus the O(1)
		// header record each page reads to check the spool's kind. A
		// remainder-copying build reads it once per page instead.
		if bound := probe.SpoolBytesWritten + int64(len(samples))*1024; probe.SpoolBytesRead > bound {
			t.Fatalf("the chain read %d byte(s) of a %d-byte ranked spool over %d pages (bound %d): "+
				"a page is reading the remainder behind it, not its own page",
				probe.SpoolBytesRead, probe.SpoolBytesWritten, len(samples), bound)
		}

		for _, s := range samples {
			if s.page <= 3 || s.page%50 == 0 || s.page == len(samples) {
				t.Logf("page %3d: resume records %5d, visited bytes appended %7d, spool bytes read %8d written %8d, served %3d, visited %6d, latency %v",
					s.page, s.resumeRecords, s.visitedBytes, s.spoolRead, s.spoolWritten, s.served, s.visited, s.latency)
			}
		}
		assertNoRetainedState(t, spoolDir)
	})

	t.Run("no-progress", func(t *testing.T) {
		clock := time.Now()
		calls, fired := 0, false
		// The clock passes the deadline on EVERY round trip, so no page can
		// reach an edge: the walk cannot advance and must say so.
		slow := slowAdjacency{graphFixture: f, clock: &clock, calls: &calls,
			jump: 2 * time.Minute, fired: &fired, every: 1}
		paged, err := New(Options{Adjacency: slow, Signer: signer, Spools: spools,
			Leases: pagination.NewLeases(store, limits.CursorTTL), Limits: limits,
			Now: func() time.Time { return clock }})
		if err != nil {
			t.Fatalf("new engine: %v", err)
		}
		req := base
		for pages := 1; ; pages++ {
			if pages > pageCap {
				t.Fatalf("a walk that cannot advance minted %d cursors: every page admitted nothing and "+
					"handed back the cursor it was given", pages-1)
			}
			res, err := paged.Impact(context.Background(), req)
			if err != nil {
				t.Fatalf("page %d: %v", pages, err)
			}
			if res.Meta.NextCursor == "" {
				if !res.Meta.Truncated || res.Meta.TruncationReason != reasonDeadlineStalled {
					t.Fatalf("page %d ended the answer with reason %q, want %q",
						pages, res.Meta.TruncationReason, reasonDeadlineStalled)
				}
				if pages > 4 {
					t.Fatalf("the walk minted %d cursors before reporting that it could not advance", pages-1)
				}
				t.Logf("terminated after %d page(s) with %q", pages, res.Meta.TruncationReason)
				break
			}
			req.Page, req.GenerationID = model.PageRequest{Cursor: res.Meta.NextCursor}, 0
		}
		// NOT swept. reasonDeadlineStalled ends the ANSWER, not the walk: the
		// page that reports it mints no cursor, so the one the caller is
		// holding is the whole remedy the reason offers and the state it names
		// has to outlive this request. It is reclaimed by its lease, like every
		// other continuation the caller never comes back for; the resume half
		// of the remedy is exercised in TestAMidLevelStallEndsTheAnswer.
		if !hasRetainedState(t, spoolDir) {
			t.Fatal("the stalled answer released the state its own cursor names: the remedy it " +
				"reports -- present that cursor again under a longer timeout -- cannot work")
		}
	})

}

// hasRetainedState reports whether any continuation state is still on disk.
func hasRetainedState(t *testing.T, dir string) bool {
	t.Helper()
	left, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read the spool store: %v", err)
	}
	for _, e := range left {
		if strings.HasPrefix(e.Name(), "spool-") {
			return true
		}
	}
	return false
}

// TestAMidLevelStallEndsTheAnswer is the other half of the livelock proof, and
// the shape TestADeadlineSplitWalkAlwaysAdvances cannot see.
//
// Its no-progress case stalls from the very first round trip, so the cursor
// every page resumes was minted at a LEVEL BOUNDARY and carries an EMPTY keyset
// position. That is the one shape in which a guard comparing a zero-valued
// accumulator position against the cursor's could ever match. The common shape
// is the opposite: a walk runs, admits edges, and the deadline lands INSIDE a
// multi-owner adjacency chunk, so the continuation names a real frontier node
// and relation id. Here the first page does exactly that, and only then does
// every later read run past the deadline.
//
// Asserted: page 1 mints a continuation whose LastOwner is a real node, and the
// page that follows it -- the FIRST one to make no progress -- is the one that
// reports it: truncated, reasonDeadlineStalled, no cursor. The page count is
// the assertion, because it is what the seeding buys.
//
// Mutation (applied, run, reverted in one command): the accumulator's keyset
// seeding dropped (`newImpactAccumulator` ignoring resume). Measured, and
// stated here rather than the stronger claim: the chain does NOT spin forever
// on this fixture, it costs one page and the level position. Page 2 admits
// nothing, the guard cannot fire because the accumulator's position is zero
// while the cursor's is a real node, so page 2 mints LastOwner:"" -- throwing
// away the keyset position and asking page 3 to re-scan the level from its
// beginning -- and page 3 is where the (now matching, because both are empty)
// guard fires. The case fails with "reported it could not advance on page 3".
func TestAMidLevelStallEndsTheAnswer(t *testing.T) {
	// The seed's own level must be WIDER than one adjacency read, so that the
	// first page stops inside it and the continuation carries a real keyset
	// position rather than the empty one a level boundary mints.
	const mids, fanOut = 300, 2
	const pageCap = 400

	f := newReachableFixture(t, mids, fanOut)
	signer, err := pagination.OpenSigner(t.TempDir())
	if err != nil {
		t.Fatalf("open signer: %v", err)
	}
	store := newFixtureLeases()
	spoolDir := t.TempDir()
	spools, err := pagination.NewSpools(spoolDir, 0, store)
	if err != nil {
		t.Fatalf("new spools: %v", err)
	}
	limits := fixtureLimits()
	limits.MaxDepth, limits.MaxVisited, limits.MaxEdges = 0, 0, 0
	limits.MaxPageItems = 50
	limits.QueryTimeout = time.Minute
	limits.FrontierBytes = 8 << 20
	// The stall jumps the clock two minutes per adjacency read, and the state
	// the remedy adopts stays adoptable only while its LEASE lives. That bound
	// is real and documented (docs/queries.md); it is not what this case is
	// measuring, so the TTL is put well clear of the jumps.
	limits.CursorTTL = time.Hour

	clock := time.Now()
	calls, fired := 0, false
	// One real round trip, then the clock is past the deadline on every read:
	// page 1 admits the seed's children and stops with a keyset position, and
	// no page after it can reach an edge at all.
	slow := slowAdjacency{graphFixture: f, clock: &clock, calls: &calls,
		jump: 2 * time.Minute, fired: &fired, stallAfter: 1}
	paged, err := New(Options{Adjacency: slow, Signer: signer, Spools: spools,
		Leases: pagination.NewLeases(store, limits.CursorTTL), Limits: limits,
		Now: func() time.Time { return clock }})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}

	req := model.ImpactRequest{GenerationID: 1,
		Start:     []model.NodeID{fixtureNodeID("bound-seed")},
		Direction: model.DirectionOutgoing, Relations: []model.RelationKind{model.RelCalls}}
	first, err := paged.Impact(context.Background(), req)
	if err != nil {
		t.Fatalf("page 1: %v", err)
	}
	if first.Meta.NextCursor == "" {
		t.Fatalf("page 1 ended the answer with %q: the stall must begin AFTER a page that made progress",
			first.Meta.TruncationReason)
	}
	if owner := cursorKeysetOwner(t, paged, first.Meta.NextCursor); owner == "" {
		t.Fatal("page 1 stopped at a level boundary: this case only proves anything when the " +
			"continuation carries a real mid-level keyset position")
	} else {
		t.Logf("page 1 stopped mid-level at owner %q", owner)
	}

	req.Page, req.GenerationID = model.PageRequest{Cursor: first.Meta.NextCursor}, 0
	for pages := 2; ; pages++ {
		if pages > pageCap {
			t.Fatalf("a mid-level stall minted %d cursors: every page admitted nothing and handed "+
				"back the cursor it was given", pages-1)
		}
		res, err := paged.Impact(context.Background(), req)
		if err != nil {
			t.Fatalf("page %d: %v", pages, err)
		}
		if res.Meta.NextCursor == "" {
			if !res.Meta.Truncated || res.Meta.TruncationReason != reasonDeadlineStalled {
				t.Fatalf("page %d ended the answer with reason %q, want %q",
					pages, res.Meta.TruncationReason, reasonDeadlineStalled)
			}
			if pages > 2 {
				t.Fatalf("a mid-level stall reported it could not advance on page %d: the FIRST page "+
					"that admits nothing must report it, and a later one only can by first "+
					"discarding the keyset position and re-scanning the level", pages)
			}
			t.Logf("terminated after %d page(s) with %q", pages, res.Meta.TruncationReason)
			break
		}
		req.Page = model.PageRequest{Cursor: res.Meta.NextCursor}
	}

	// The remedy the reason promises, exercised. reasonDeadlineStalled ends the
	// ANSWER but not the walk: the page that reports it mints no cursor of its
	// own, so the one the caller is still holding has to stay adoptable. A
	// fresh request gets a fresh deadline, and here also a reader that does not
	// run past it, so the same cursor carries the walk to exhaustion instead of
	// costing it from the seed.
	//
	// Mutation (applied, run, reverted in one command): the stalled path's flag
	// dropped, so the named-return defer's terminalOutcome(nil) releases the
	// retained walk and its lease -- `resume: CTX_CURSOR_INVALID: continuation
	// state has expired or was released`.
	warm, err := New(Options{Adjacency: f, Signer: signer, Spools: spools,
		Leases: pagination.NewLeases(store, limits.CursorTTL), Limits: limits,
		Now: func() time.Time { return clock }})
	if err != nil {
		t.Fatalf("new warm engine: %v", err)
	}
	served, resumed := 0, model.ImpactRequest{
		Start: req.Start, Direction: req.Direction, Relations: req.Relations,
		Page: model.PageRequest{Cursor: first.Meta.NextCursor}}
	for pages := 1; ; pages++ {
		if pages > pageCap {
			t.Fatalf("the resumed walk never reached exhaustion in %d pages", pageCap)
		}
		res, err := warm.Impact(context.Background(), resumed)
		if err != nil {
			t.Fatalf("resume: %v", err)
		}
		served += len(res.Entries)
		if res.Meta.NextCursor == "" {
			if res.Meta.Truncated {
				t.Fatalf("the resumed walk ended truncated with %q; the remedy must reach the whole answer",
					res.Meta.TruncationReason)
			}
			t.Logf("the held cursor resumed and served %d entities over %d page(s)", served, pages)
			break
		}
		resumed.Page, resumed.GenerationID = model.PageRequest{Cursor: res.Meta.NextCursor}, 0
	}
	// The remedy must not lose the leg that ran before the stall: the answer it
	// reaches is the whole one, not the remainder.
	if want := unboundedImpactCount(t, f, signer, store, spoolDir, limits, req.Start); served != want {
		t.Fatalf("the resumed walk served %d entities; the unbounded walk serves %d", served, want)
	}
}

// unboundedImpactCount is how many entities the same query answers with no
// deadline in its way, paged to exhaustion.
func unboundedImpactCount(t *testing.T, f *graphFixture, signer *pagination.Signer,
	store pagination.LeaseStore, spoolDir string, limits Limits, start []model.NodeID) int {
	t.Helper()
	spools, err := pagination.NewSpools(t.TempDir(), 0, store)
	if err != nil {
		t.Fatalf("new spools: %v", err)
	}
	e, err := New(Options{Adjacency: f, Signer: signer, Spools: spools,
		Leases: pagination.NewLeases(store, limits.CursorTTL), Limits: limits})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	req := model.ImpactRequest{GenerationID: 1, Start: start,
		Direction: model.DirectionOutgoing, Relations: []model.RelationKind{model.RelCalls}}
	total := 0
	for {
		res, err := e.Impact(context.Background(), req)
		if err != nil {
			t.Fatalf("unbounded impact: %v", err)
		}
		total += len(res.Entries)
		if res.Meta.NextCursor == "" {
			if res.Meta.Truncated {
				t.Fatalf("the unbounded walk is truncated with %q", res.Meta.TruncationReason)
			}
			return total
		}
		req.Page, req.GenerationID = model.PageRequest{Cursor: res.Meta.NextCursor}, 0
	}
}

// cursorKeysetOwner is the frontier node a continuation resumes its level from.
// An empty owner means the page that minted it stopped on a level boundary.
func cursorKeysetOwner(t *testing.T, e *Engine, token string) model.NodeID {
	t.Helper()
	payload, err := e.signer.Verify(token, pagination.PurposeCursor, e.now())
	if err != nil {
		t.Fatalf("verify cursor: %v", err)
	}
	var c traversalCursor
	if err := json.Unmarshal(payload, &c); err != nil {
		t.Fatalf("decode cursor: %v", err)
	}
	return c.LastOwner
}

// TestALevelBoundaryDeadlineKeepsTheWholeAnswer is the cross-request half of
// the level-boundary rule.
//
// A deadline can stop a walk BETWEEN levels: the level just finished, the next
// one is standing in the frontier and has not been read at all. The keyset
// position the page last emitted belongs to the finished level, so a
// continuation carrying it hands levelEdges a filter rather than a resume
// point, and every row of the NEW level whose owner sorts below that node is
// dropped. Dropped silently: those owners are already in the cumulative visited
// set, so no later page can reach them, and the answer ends untruncated -- a
// short answer presented as a whole one, which is the one failure the paging
// contract may not have.
//
// traverse.go applies the rule to the traversal endpoint and walkrun.go to a
// walk's internal links; continueWalk, which mints the impact and package
// continuations, did not.
//
// Mutation (applied, run, reverted in one command): continueWalk's
// LevelBoundary branch deleted -- the resumed walk serves 38 of the 56 entities
// the unbounded walk serves, with no truncation reason, and this case fails.
func TestALevelBoundaryDeadlineKeepsTheWholeAnswer(t *testing.T) {
	// A seed whose whole level fits in ONE adjacency read: the deadline then
	// lands after the level is finished and before the next is read, which is
	// the level boundary this case needs.
	const mids, fanOut = 8, 6

	f := newReachableFixture(t, mids, fanOut)
	signer, err := pagination.OpenSigner(t.TempDir())
	if err != nil {
		t.Fatalf("open signer: %v", err)
	}
	store := newFixtureLeases()
	spoolDir := t.TempDir()
	spools, err := pagination.NewSpools(spoolDir, 0, store)
	if err != nil {
		t.Fatalf("new spools: %v", err)
	}
	limits := fixtureLimits()
	limits.MaxDepth, limits.MaxVisited, limits.MaxEdges = 0, 0, 0
	limits.MaxPageItems = 8
	limits.QueryTimeout = time.Minute
	limits.FrontierBytes = 8 << 20
	limits.CursorTTL = time.Hour

	clock := time.Now()
	calls, fired := 0, false
	slow := slowAdjacency{graphFixture: f, clock: &clock, calls: &calls,
		jump: 2 * time.Minute, fired: &fired, stallAfter: 1}
	paged, err := New(Options{Adjacency: slow, Signer: signer, Spools: spools,
		Leases: pagination.NewLeases(store, limits.CursorTTL), Limits: limits,
		Now: func() time.Time { return clock }})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	start := []model.NodeID{fixtureNodeID("bound-seed")}
	first, err := paged.Impact(context.Background(), model.ImpactRequest{GenerationID: 1,
		Start: start, Direction: model.DirectionOutgoing,
		Relations: []model.RelationKind{model.RelCalls}})
	if err != nil {
		t.Fatalf("page 1: %v", err)
	}
	if first.Meta.NextCursor == "" {
		t.Fatalf("page 1 ended the answer with %q; it must hand the standing frontier on",
			first.Meta.TruncationReason)
	}
	if owner := cursorKeysetOwner(t, paged, first.Meta.NextCursor); owner != "" {
		t.Fatalf("the level-boundary continuation carries keyset owner %q: the level it names has "+
			"not been read, so any position in it is a filter over rows nobody has seen", owner)
	}

	// The rest of the walk, read by a reader that does not run past the
	// deadline, must reach the whole answer.
	warm, err := New(Options{Adjacency: f, Signer: signer, Spools: spools,
		Leases: pagination.NewLeases(store, limits.CursorTTL), Limits: limits,
		Now: func() time.Time { return clock }})
	if err != nil {
		t.Fatalf("new warm engine: %v", err)
	}
	served := 0
	req := model.ImpactRequest{Start: start, Direction: model.DirectionOutgoing,
		Relations: []model.RelationKind{model.RelCalls},
		Page:      model.PageRequest{Cursor: first.Meta.NextCursor}}
	for pages := 1; pages <= 400; pages++ {
		res, err := warm.Impact(context.Background(), req)
		if err != nil {
			t.Fatalf("resume page %d: %v", pages, err)
		}
		served += len(res.Entries)
		if res.Meta.NextCursor == "" {
			if res.Meta.Truncated {
				t.Fatalf("the resumed walk ended truncated with %q", res.Meta.TruncationReason)
			}
			break
		}
		req.Page = model.PageRequest{Cursor: res.Meta.NextCursor}
	}
	if want := unboundedImpactCount(t, f, signer, store, spoolDir, limits, start); served != want {
		t.Fatalf("the walk a level-boundary deadline split served %d entities and reported no "+
			"truncation; the unbounded walk serves %d", served, want)
	}
	t.Logf("the level-boundary split served the whole answer: %d entities", served)
}
