package graph

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
)

// ctxAdjacency is the half of the shipped storage reader that every in-memory
// fixture was missing: a real SQLite read returns the context's OWN error,
// untyped, when the deadline falls inside the query (sqlite/open.go's wrap
// passes context.DeadlineExceeded through deliberately, so a caller can tell it
// from a crash). Every fixture Adjacency returned rows regardless of the clock,
// so no graph test ever exercised the classification of a RAW deadline arriving
// from below -- which is why the engine shipped treating it as fatal and threw
// away the page, the frontier and the cursor with it.
//
// Checking the context at the TOP of the read is what makes the deadline land
// inside it rather than on a checkWalk between reads: the engine's own checks
// synthesize a typed error and were always handled.
type ctxAdjacency struct {
	*graphFixture
	mu    sync.Mutex
	calls int
}

func (a *ctxAdjacency) Edges(ctx context.Context, nodes []model.NodeID, direction model.Direction,
	kinds []model.RelationKind, after model.RelationID, limit int) ([]model.Relation, error) {
	a.mu.Lock()
	a.calls++
	a.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return a.graphFixture.Edges(ctx, nodes, direction, kinds, after, limit)
}

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
func newStorageDeadlineEngine(t *testing.T, adj Adjacency, timeout time.Duration) *Engine {
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
	e, err := New(Options{Adjacency: adj, Signer: signer, Spools: spools,
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
	ref := newStorageDeadlineEngine(t, &ctxAdjacency{graphFixture: f}, 10*time.Minute)
	start := time.Now()
	want := drainImpact(t, ref, req, "reference")
	budget := time.Since(start) / 6
	if len(want) == 0 {
		t.Fatalf("reference answer is empty; the fixture proves nothing")
	}
	if budget < 5*time.Millisecond {
		budget = 5 * time.Millisecond
	}

	// The subject: a sixth of the whole answer's wall clock per request, so the
	// deadline lands inside a read on every page and the walk needs several --
	// while every page still advances, which is the condition under which a
	// continuation, not the terminal stalled reason, is the right answer.
	e := newStorageDeadlineEngine(t, &ctxAdjacency{graphFixture: f}, budget)
	got := drainImpact(t, e, req, "deadline-split")

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
func drainImpact(t *testing.T, e *Engine, req func(string) model.ImpactRequest, label string) []model.NodeID {
	t.Helper()
	const pageCap = 2000
	var (
		order []model.NodeID
		seen  = map[model.NodeID]int{}
		next  string
	)
	for page := 1; ; page++ {
		if page > pageCap {
			t.Fatalf("%s: the cursor chain did not end within %d pages", label, pageCap)
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
				t.Fatalf("%s page %d: truncated (%s) with no continuation",
					label, page, res.Meta.TruncationReason)
			}
			return order
		}
		next = res.Meta.NextCursor
	}
}
