package graph

import (
	"context"
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
		assertNoRetainedState(t, spoolDir)
	})

}
