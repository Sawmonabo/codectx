package graph

import (
	"context"
	"testing"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
)

// ctxAdjacency is the half of the shipped storage reader an in-memory fixture
// otherwise omits: a real SQLite read returns the context's OWN error, untyped,
// when the deadline falls inside the query (sqlite/open.go's wrap passes
// context.DeadlineExceeded through deliberately, so a caller can tell it from a
// crash). It is what exercises the engine's classification of a RAW deadline
// arriving from below, rather than of a typed one the engine synthesized.
//
// Checking the context at the TOP of the read is what makes the deadline land
// inside it rather than on a checkWalk between reads.
type ctxAdjacency struct{ *graphFixture }

func (a *ctxAdjacency) NodesByID(ctx context.Context, ids []model.NodeID) ([]model.Node, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return a.graphFixture.NodesByID(ctx, ids)
}

// ctxLeases is the same fidelity for the lease store: sqlite refuses a write on
// an expired context with the raw context error. It matters because the cursor
// is MINTED after the deadline has already passed -- the mint is the last thing
// a timed-out page does -- so a lease store that ignored the clock could not
// show that the mint itself was what failed.
type ctxLeases struct{ *fixtureLeases }

func (l *ctxLeases) AcquireLease(ctx context.Context, lease model.Lease, ownerRef string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return l.fixtureLeases.AcquireLease(ctx, lease, ownerRef)
}

func (l *ctxLeases) LeaseExpiry(ctx context.Context, id string) (time.Time, error) {
	if err := ctx.Err(); err != nil {
		return time.Time{}, err
	}
	return l.fixtureLeases.LeaseExpiry(ctx, id)
}

// newStorageDeadlineEngine builds an engine over adj with the given per-request
// query timeout and the full continuation machinery.
func newStorageDeadlineEngine(t *testing.T, f *graphFixture, adj Adjacency, timeout time.Duration) *Engine {
	t.Helper()
	signer, err := pagination.OpenSigner(t.TempDir())
	if err != nil {
		t.Fatalf("open signer: %v", err)
	}
	store := &ctxLeases{newFixtureLeases()}
	spools, err := pagination.NewSpools(t.TempDir(), 0, store)
	if err != nil {
		t.Fatalf("new spools: %v", err)
	}
	limits := fixtureLimits()
	limits.MaxDepth, limits.MaxVisited, limits.MaxEdges = 0, 0, 0
	limits.MaxPageItems = 200
	limits.QueryTimeout = timeout
	limits.FrontierBytes = 8 << 20
	e, err := New(Options{Adjacency: adj, Reader: memGraphFor(f), Signer: signer, Spools: spools,
		Leases: pagination.NewLeases(store, limits.CursorTTL), Limits: limits})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	return e
}

// TestImpactPagesOnRawStorageDeadline is the certification failure, reproduced.
//
// On the real r3 repository `codectx impact <hub> --depth 0 --visited 0
// --edges 0 --json` returned, after the full query timeout, ok:false with
// data:null and a bare CTX_QUERY_DEADLINE: no cursor, no partial answer, and no
// way for the caller to carry the walk on. The same request with --visited
// 10000 returned a cursor, so the paging machinery was sound; what was not
// sound was the SHAPE of the deadline. A deadline the engine noticed itself is
// a typed CTX_QUERY_DEADLINE and ends the page with a continuation; a deadline
// that arrives from the storage reader, or that trips the lease write minting
// the cursor, is the context's own untyped error and was fatal.
//
// The invariant: inside a graph endpoint a deadline of ANY shape ends the PAGE
// with a cursor (or with the typed stalled reason), never the answer with an
// error. So this asserts, against a reader and a lease store that both behave
// as SQLite does:
//
//	(a) the interrupted page returns no error, is truncated with a named
//	    reason, and carries a continuation;
//	(b) following that continuation reaches exhaustion, and the union of the
//	    pages is exactly the uninterrupted answer -- same set, same ORDER, no
//	    entity listed twice.
//
// Mutation, run and pasted in the lane report: isDeadline reverted to the
// typed-only test at traverse.go's readChunk (the shipped behaviour).
func TestImpactPagesOnRawStorageDeadline(t *testing.T) {
	const mids, fanOut = 40, 60
	f := newReachableFixture(t, mids, fanOut)
	seed := fixtureNodeID("bound-seed")
	req := func(cursor string) model.ImpactRequest {
		return model.ImpactRequest{Start: []model.NodeID{seed}, Direction: model.DirectionBoth,
			Page: model.PageRequest{Limit: 200, Cursor: cursor}}
	}

	// The reference: the same walk with a deadline no read can reach. Its wall
	// clock also CALIBRATES the subject's budget, so the case holds its shape
	// under -race and on a loaded machine instead of pinning a constant that a
	// ten-times-slower build turns into a walk that cannot advance at all.
	ref := newStorageDeadlineEngine(t, f, &ctxAdjacency{graphFixture: f}, 10*time.Minute)
	start := time.Now()
	want, stalledRef := drainImpact(t, ref, req, "reference")
	if stalledRef {
		t.Fatalf("the reference walk stalled under a ten-minute budget")
	}
	budget := time.Since(start) / 40
	if len(want) == 0 {
		t.Fatalf("reference answer is empty; the fixture proves nothing")
	}
	if budget < 5*time.Millisecond {
		budget = 5 * time.Millisecond
	}

	// The subject: a fortieth of the whole answer's wall clock per request, so
	// the deadline lands inside a read on every page and the walk needs several
	// -- while every page still advances, which is the condition under which a
	// continuation, not the terminal stalled reason, is the right answer.
	//
	// A fortieth rather than a sixth: a budget that small reaches EVERY step of
	// the level pipeline, the transition between a level's collect and its
	// serve included. A deadline there has no resumable half to stop at, and a
	// walk that answers it with a state it never reached ends the chain
	// believing it is exhausted -- the short answer this case measures.
	//
	// A budget so small that the chain cannot advance is a LEGITIMATE outcome
	// in either of the two shapes drainImpact reports -- the walk leg's
	// terminal reasonDeadlineStalled with no cursor, which the real binary
	// returns on r3 under a 2s budget, or a rank leg that keeps timing out,
	// re-sorting the same retained input from scratch each time -- but
	// neither serves the answer, so the union assertion below would have
	// nothing to compare. Rather than tolerate it and assert less, the budget
	// is raised and the chain re-run: the invariant stays the full one, and the
	// case stops depending on how loaded the machine is, which under -race it
	// otherwise is (there the first budget reaches the rank leg and cannot
	// finish it, and the second does).
	var got []model.NodeID
	for attempt, stalled := 0, true; stalled; attempt++ {
		if attempt == 3 {
			t.Fatalf("no page could advance at %v per request", budget)
		}
		if attempt > 0 {
			budget *= 4
		}
		e := newStorageDeadlineEngine(t, f, &ctxAdjacency{graphFixture: f}, budget)
		got, stalled = drainImpact(t, e, req, "deadline-split")
	}

	if len(got) != len(want) {
		t.Fatalf("deadline-split answer has %d entries, the reference has %d", len(got), len(want))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("entry %d is %q, the reference has %q: the pages are not the reference ORDER",
				i, got[i], want[i])
		}
	}
}

// drainImpact follows the cursor chain to exhaustion and returns the entity ids
// in served order, failing on the three shapes the invariant forbids: an error
// instead of a page, a truncated page with no continuation, and an entity
// served twice.
//
// It reports whether the chain FAILED TO ADVANCE at this budget instead of
// running to exhaustion; the caller decides what that means for its assertion
// (the case above raises the budget and re-runs). Two shapes count:
//
//	(a) the engine's own terminal reasonDeadlineStalled, minted when the walk
//	    could not move and so hands back no continuation at all;
//	(b) stallStreak pages IN A ROW that each handed back a continuation yet
//	    advanced NOTHING -- not the cumulative visited set, not the edges read,
//	    not one more entity served. A walk leg that moves nothing is (a); this
//	    is the rank leg, whose continuation names the retained input alone, so
//	    a budget too small to finish the sort re-sorts from scratch forever.
//
// Progress, not a page count, is what bounds the chain, because no page count
// can be calibrated here: under -race the rank leg at the derived budget cannot
// finish its sort and re-runs without limit (measured: past 2000 pages), which
// is a budget outcome, not a length.
//
// The streak has to be long, and that is the whole subtlety of this guard. A
// SINGLE non-advancing page is ordinary even at a budget that works -- the
// unmutated chain shows exactly one, the rank leg's first try -- and returning
// on it would raise the budget past the regime where the deadline lands inside
// a storage read at all, which is the regime the case exists to test. Ending
// the drain early there hid the isDeadline mutation completely: the mutated
// engine loses its remainder only under the tight budget, so the escalated
// re-run agreed with the reference and the case passed. A budget that truly
// cannot finish the sort cannot finish it on any attempt, so a long streak
// separates the two without weakening either.
//
// The absolute cap is only a runaway guard: it is far above any page count this
// fixture's 2440 entities can reach while pages keep advancing, and it names
// the failure instead of leaving a broken chain to the package timeout.
func drainImpact(t *testing.T, e *Engine, req func(string) model.ImpactRequest,
	label string) ([]model.NodeID, bool) {
	t.Helper()
	const (
		pageCap     = 5000
		stallStreak = 64
	)
	// The walk's cumulative accounting, which every continuation carries: a page
	// that leaves all three unchanged did no work the next page can build on.
	type progress struct{ visited, edges, served int64 }
	var (
		order []model.NodeID
		seen  = map[model.NodeID]int{}
		next  string
		last  progress
		stall int
		// Page 1 has nothing to be compared against: walkStalled needs a resume
		// state, so the first page always mints a continuation.
		havePrev bool
	)
	for page := 1; ; page++ {
		if page > pageCap {
			t.Fatalf("%s: the cursor chain still advanced after %d pages", label, pageCap)
		}
		res, err := e.Impact(context.Background(), req(next))
		if err != nil {
			t.Fatalf("%s page %d: %v", label, page, err)
		}
		for _, entry := range res.Entries {
			if first, dup := seen[entry.NodeID]; dup {
				t.Fatalf("%s page %d: %s was already served on page %d", label, page, entry.NodeID, first)
			}
			seen[entry.NodeID] = page
			order = append(order, entry.NodeID)
		}
		if res.Meta.NextCursor == "" {
			if res.Meta.Truncated {
				// The ONE truncation allowed to end a chain without a
				// continuation: the page could not advance, so the cursor it
				// would mint is the one the caller already holds and stays
				// adoptable. Any other truncation here is a lost remainder.
				if res.Meta.TruncationReason != reasonDeadlineStalled {
					t.Fatalf("%s page %d: truncated (%s) with no continuation",
						label, page, res.Meta.TruncationReason)
				}
				t.Logf("%s: stalled on page %d after %d entries", label, page, len(order))
				return order, true
			}
			t.Logf("%s: exhausted in %d pages, %d entries", label, page, len(order))
			return order, false
		}
		cur := progress{res.VisitedCount, res.EdgeCount, int64(len(order))}
		if havePrev && cur == last {
			if stall++; stall >= stallStreak {
				t.Logf("%s: %d pages in a row advanced nothing by page %d (visited %d, edges %d, served %d)",
					label, stall, page, cur.visited, cur.edges, cur.served)
				return order, true
			}
		} else {
			stall = 0
		}
		last, havePrev = cur, true
		next = res.Meta.NextCursor
	}
}
