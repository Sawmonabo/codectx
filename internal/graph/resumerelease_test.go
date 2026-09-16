package graph

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
)

// A RETRYABLE failure on a resumed traversal page must leave the continuation
// adoptable. `traverse` consumes the cursor's spool, its retained state and its
// lease through one deferred `resume.Release()`, and an unguarded release runs
// on the error return too: the caller is told CTX_WORKSPACE_BUSY -- an error
// whose whole remedy is "present this cursor again" -- and the retry is then
// refused as CTX_CURSOR_INVALID, so a long walk is unrecoverable after one
// contended edge read. The guard is `terminalOutcome`, the same one impact and
// the shortest path already apply.
//
// Mutation proof: drop the guard in traverse.go (`defer resume.Release()`) and
// this fails on the retry with
//
//	the retry of a busy page was refused: CTX_CURSOR_INVALID
//
// It is injected on the PACKED reader rather than on the delivery adjacency:
// the walk reads structure through GraphReader, so a failure on a delivery
// method is a failure on a port the resumed page never calls and the case
// would prove nothing.
type busyOnceReader struct {
	GraphReader
	fail bool
	// failAt is the adjacency read of the CURRENT page the failure lands on,
	// counted from one; zero means the page's first read. calls is reset by the
	// test at each page boundary.
	failAt int
	calls  int
	// injected counts the failures actually served, so a case cannot pass by
	// never reaching the read it means to interrupt.
	injected int
}

func (b *busyOnceReader) Neighbours(ctx context.Context, refs []NodeRef, direction model.Direction,
	kinds []KindCode, from EdgePos, fn func(Edge) error) (EdgePos, error) {
	b.calls++
	if b.fail && b.calls >= max(1, b.failAt) {
		b.fail = false
		b.injected++
		return EdgePos{}, &model.Error{Code: model.CodeWorkspaceBusy, Retryable: true,
			Message: "the adjacency store was contended for this read"}
	}
	return b.GraphReader.Neighbours(ctx, refs, direction, kinds, from, fn)
}

func TestBusyResumedTraversalPageKeepsItsContinuation(t *testing.T) {
	f := newGraphFixture(t)
	adj := &busyOnceReader{GraphReader: memGraphFor(f)}
	signer, err := pagination.OpenSigner(t.TempDir())
	if err != nil {
		t.Fatalf("open signer: %v", err)
	}
	store := newFixtureLeases()
	spools, err := pagination.NewSpools(t.TempDir(), 64<<20, store)
	if err != nil {
		t.Fatalf("new spools: %v", err)
	}
	limits := fixtureLimits()
	// One relation per page, so the walk is spread over many continuations and
	// the page under test is a RESUMED one.
	limits.MaxPageItems = 1
	limits.QueryTimeout = time.Minute
	e, err := New(Options{Adjacency: f, Reader: adj, Signer: signer, Spools: spools,
		Leases: pagination.NewLeases(store, limits.CursorTTL), Limits: limits})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}

	req := model.GraphRequest{GenerationID: 1, Start: []model.NodeID{fixtureNodeID("n-a")},
		Direction: model.DirectionOutgoing, Relations: []model.RelationKind{model.RelCalls}}
	first, err := e.Neighbors(context.Background(), req)
	if err != nil {
		t.Fatalf("first page: %v", err)
	}
	if first.Meta.NextCursor == "" {
		t.Fatalf("the first page minted no continuation: this proof needs a RESUMED page")
	}

	// A resumed page that serves out of the sorted level reads no adjacency at
	// all -- that is what makes a page a seek and its own records -- so the
	// failure is injected on every page until one of them reaches the scan
	// that collects the next level. That page is the subject.
	req.Page, req.GenerationID = model.PageRequest{Cursor: first.Meta.NextCursor}, 0
	for page := 2; ; page++ {
		if page > 200 {
			t.Fatalf("no resumed page reached an adjacency read: the failure was never injected")
		}
		adj.fail = true
		res, perr := e.Neighbors(context.Background(), req)
		if perr != nil {
			err = perr
			break
		}
		adj.fail = false
		if res.Meta.NextCursor == "" {
			t.Fatalf("the walk ended without a resumed page reaching an adjacency read")
		}
		req.Page = model.PageRequest{Cursor: res.Meta.NextCursor}
	}
	var typed *model.Error
	if !errors.As(err, &typed) || typed.Code != model.CodeWorkspaceBusy || !typed.Retryable {
		t.Fatalf("the contended page reported %v, not a retryable CTX_WORKSPACE_BUSY", err)
	}

	// The retry the error invites: the SAME cursor, against a healthy store.
	retry, err := e.Neighbors(context.Background(), req)
	if err != nil {
		if errors.As(err, &typed) && typed.Code == model.CodeCursorInvalid {
			t.Fatalf("the retry of a busy page was refused: %s -- the continuation "+
				"was released on a non-terminal outcome, so the walk behind it is unrecoverable",
				typed.Code)
		}
		t.Fatalf("the retry of a busy page failed: %v", err)
	}
	if err := retry.Validate(); err != nil {
		t.Fatalf("the retried page does not satisfy its own contract: %v", err)
	}
	if len(retry.Relations) == 0 {
		t.Fatalf("the retried page served no relation: the resumed walk lost its frontier")
	}
}

// A retryable failure must also leave a continuation minted in the COLLECTING
// state adoptable, and that is a strictly harder case than the serving one
// above: a resumed collecting page does not merely read its own level, it
// FINISHES it -- and finishing a level releases the frontier file the level was
// collected from, which is the same file the cursor the caller still holds
// resumes the scan out of.
//
// Deleted, the retry is not an error at all. The walk asks whether the level
// below has a frontier, finds no file, and reads that as "the walk is
// exhausted": the page comes back short, untruncated, with no continuation --
// a fraction of the answer presented as the whole of it.
//
// The failure is therefore injected on the page's SECOND adjacency read, so it
// lands after the resumed level was served and the next one had begun.
//
// Mutation proof (fails this test): release the entry level's frontier
// unconditionally in traverse.go's serve (`o.Retain.releaseFrontier` without
// the isHeld guard) -- `the walk served 4 relations across a retried collecting
// page, want the 48 the unbounded walk serves`.
func TestBusyCollectingTraversalPageKeepsItsFrontier(t *testing.T) {
	f := newGraphFixture(t)
	signer, err := pagination.OpenSigner(t.TempDir())
	if err != nil {
		t.Fatalf("open signer: %v", err)
	}
	store := newFixtureLeases()
	spools, err := pagination.NewSpools(t.TempDir(), 64<<20, store)
	if err != nil {
		t.Fatalf("new spools: %v", err)
	}
	limits := fixtureLimits()
	limits.MaxDepth, limits.MaxVisited, limits.MaxEdges = 0, 0, 0
	limits.MaxPageItems, limits.QueryTimeout = 2000, time.Minute
	req := model.GraphRequest{GenerationID: 1, Start: []model.NodeID{fixtureNodeID("n-a")},
		Direction: model.DirectionOutgoing, Relations: []model.RelationKind{model.RelCalls}}

	// Ground truth: the same walk, one page, no deadline and no contention.
	whole, err := New(Options{Adjacency: f, Reader: memGraphFor(f), Signer: signer, Spools: spools,
		Leases: pagination.NewLeases(store, limits.CursorTTL), Limits: limits})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	all, err := whole.Neighbors(context.Background(), req)
	if err != nil {
		t.Fatalf("unbounded walk: %v", err)
	}
	if all.Meta.NextCursor != "" || len(all.Relations) == 0 {
		t.Fatalf("the unbounded walk is paged (%d relations, cursor %t)",
			len(all.Relations), all.Meta.NextCursor != "")
	}

	// A deadline placed inside the level's COLLECT is what mints a collecting
	// continuation, so the first page is run at several placements until one
	// does. The clock jumps once (trigger), so the resumed page below runs on a
	// fresh deadline and stops only where the contention puts it.
	for trigger := 2; ; trigger++ {
		if trigger > 12 {
			t.Fatalf("no deadline placement minted a COLLECTING continuation")
		}
		clock := time.Now()
		calls, fired := 0, false
		slow := slowAdjacency{graphFixture: f, clock: &clock, calls: &calls,
			trigger: trigger, jump: 2 * time.Minute, fired: &fired}
		adj := &busyOnceReader{GraphReader: slow.reader(memGraphFor(f)), failAt: 2}
		e, err := New(Options{Adjacency: slow, Reader: adj, Signer: signer, Spools: spools,
			Leases: pagination.NewLeases(store, limits.CursorTTL), Limits: limits,
			Now: func() time.Time { return clock }})
		if err != nil {
			t.Fatalf("new engine: %v", err)
		}
		first, err := e.Neighbors(context.Background(), req)
		if err != nil {
			t.Fatalf("the deadline-stopped page failed: %v", err)
		}
		if first.Meta.NextCursor == "" {
			continue
		}
		next := req
		next.Page, next.GenerationID = model.PageRequest{Cursor: first.Meta.NextCursor}, 0
		if cursorLevelState(t, e, first.Meta.NextCursor) != levelCollecting {
			continue
		}

		// The subject: a page resumed into a COLLECTING level, which finishes
		// that level -- releasing the frontier it was collected from -- and is
		// then interrupted on the read that begins the level below.
		seen := map[model.RelationID]bool{}
		for _, rel := range first.Relations {
			seen[rel.ID] = true
		}
		adj.fail, adj.calls = true, 0
		if _, err := e.Neighbors(context.Background(), next); err == nil {
			continue
		} else {
			var typed *model.Error
			if !errors.As(err, &typed) || !typed.Retryable {
				t.Fatalf("the contended page reported %v, not a retryable failure", err)
			}
		}

		// The retry the error invites: the SAME cursor, against a healthy store.
		for page := 2; ; page++ {
			if page > 200 {
				t.Fatalf("the retried walk did not end in %d pages", page)
			}
			res, err := e.Neighbors(context.Background(), next)
			if err != nil {
				t.Fatalf("the retry of the contended page failed: %v", err)
			}
			for _, rel := range res.Relations {
				seen[rel.ID] = true
			}
			if res.Meta.NextCursor == "" {
				if res.Meta.Truncated {
					t.Fatalf("the retried walk ended truncated with %q", res.Meta.TruncationReason)
				}
				break
			}
			next.Page = model.PageRequest{Cursor: res.Meta.NextCursor}
		}
		if len(seen) != len(all.Relations) {
			t.Fatalf("the walk served %d relations across a retried collecting page, want the %d "+
				"the unbounded walk serves: the retry resumed a level whose frontier had been released",
				len(seen), len(all.Relations))
		}
		return
	}
}

// TestARetriedCollectingPageDoesNotReCollectACommittedLevel is the other half
// of the collecting retry, and the one the caller cannot detect. A page
// resumed into a COLLECTING level may commit that level's transition and then
// fail retryably on the level below; the remedy the error names is to present
// the SAME cursor again, which still says "collecting".
//
// Collecting it again is not idempotent. The committed transition already
// applied this level's admissions to the visited bits, and the direction dedup
// rule reads exactly those bits, so the second scan of the same frontier drops
// the incoming edges the first one kept. finish() then rewrites sorted.<level>
// strictly smaller while writeAdmitted reuses the admitted file that is
// already there, so the frontier, the admitted count and the visited count all
// stand: the retried walk serves FEWER relations and reports itself complete
// and untruncated. A caller has nothing to compare against, which is why this
// is proved against the same walk run in one page.
//
// The visited count is asserted with the relations: the level being served is
// what closeGroup charges the budget from, and a leg that re-presents a serving
// or collecting cursor after the next level has committed finds the frontier
// bitset rebuilt around that next level unless it is realigned first.
//
// Mutation proof (fails this test): drop the committed() check at the head of
// collect, so the retry re-collects the level -- `the retried walk served 47
// relations, want the 48 the unbounded walk serves`.
func TestARetriedCollectingPageDoesNotReCollectACommittedLevel(t *testing.T) {
	f := newGraphFixture(t)
	signer, err := pagination.OpenSigner(t.TempDir())
	if err != nil {
		t.Fatalf("open signer: %v", err)
	}
	store := newFixtureLeases()
	spools, err := pagination.NewSpools(t.TempDir(), 64<<20, store)
	if err != nil {
		t.Fatalf("new spools: %v", err)
	}
	limits := fixtureLimits()
	limits.MaxDepth, limits.MaxVisited, limits.MaxEdges = 0, 0, 0
	limits.MaxPageItems, limits.QueryTimeout = 2000, time.Minute
	// DirectionBoth: the direction dedup rule -- the thing a re-collect
	// silently re-applies against bits that have moved on -- is skipped
	// outright on a one-direction walk.
	req := model.GraphRequest{GenerationID: 1, Start: []model.NodeID{fixtureNodeID("n-a")},
		Direction: model.DirectionBoth, Relations: []model.RelationKind{model.RelCalls}}

	whole, err := New(Options{Adjacency: f, Reader: memGraphFor(f), Signer: signer, Spools: spools,
		Leases: pagination.NewLeases(store, limits.CursorTTL), Limits: limits})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	all, err := whole.Neighbors(context.Background(), req)
	if err != nil {
		t.Fatalf("unbounded walk: %v", err)
	}
	if all.Meta.NextCursor != "" || len(all.Relations) == 0 {
		t.Fatalf("the unbounded walk is paged (%d relations, cursor %t)",
			len(all.Relations), all.Meta.NextCursor != "")
	}

	for trigger := 2; ; trigger++ {
		if trigger > 12 {
			t.Fatalf("no deadline placement minted a COLLECTING continuation")
		}
		clock := time.Now()
		calls, fired := 0, false
		slow := slowAdjacency{graphFixture: f, clock: &clock, calls: &calls,
			trigger: trigger, jump: 2 * time.Minute, fired: &fired}
		// The failure lands on the SECOND adjacency read of the resumed page:
		// by then the level the cursor names has been collected, its
		// transition has committed and the level below has begun.
		adj := &busyOnceReader{GraphReader: slow.reader(memGraphFor(f)), failAt: 2}
		e, err := New(Options{Adjacency: slow, Reader: adj, Signer: signer, Spools: spools,
			Leases: pagination.NewLeases(store, limits.CursorTTL), Limits: limits,
			Now: func() time.Time { return clock }})
		if err != nil {
			t.Fatalf("new engine: %v", err)
		}
		first, err := e.Neighbors(context.Background(), req)
		if err != nil {
			t.Fatalf("the deadline-stopped page failed: %v", err)
		}
		if first.Meta.NextCursor == "" || cursorLevelState(t, e, first.Meta.NextCursor) != levelCollecting {
			continue
		}
		next := req
		next.Page, next.GenerationID = model.PageRequest{Cursor: first.Meta.NextCursor}, 0

		adj.fail, adj.calls = true, 0
		if _, err := e.Neighbors(context.Background(), next); err == nil {
			continue
		} else if typed := (*model.Error)(nil); !errors.As(err, &typed) || !typed.Retryable {
			t.Fatalf("the contended page reported %v, not a retryable failure", err)
		}
		if adj.injected != 1 {
			t.Fatalf("the contended read was never served; this case proves nothing")
		}

		// The retry the error invites: the same cursor, a healthy store.
		served := append([]model.Relation(nil), first.Relations...)
		var last model.GraphResult
		for page := 2; ; page++ {
			if page > 200 {
				t.Fatalf("the retried walk did not end in %d pages", page)
			}
			last, err = e.Neighbors(context.Background(), next)
			if err != nil {
				t.Fatalf("the retry of the contended page failed: %v", err)
			}
			served = append(served, last.Relations...)
			if last.Meta.NextCursor == "" {
				break
			}
			next.Page = model.PageRequest{Cursor: last.Meta.NextCursor}
		}
		seen := map[model.RelationID]bool{}
		for _, rel := range served {
			if seen[rel.ID] {
				t.Fatalf("relation %s was served twice across the retried walk", rel.ID)
			}
			seen[rel.ID] = true
		}
		if len(seen) != len(all.Relations) {
			t.Fatalf("the retried walk served %d relations, want the %d the unbounded walk serves: "+
				"the retry re-collected a level whose transition had already committed",
				len(seen), len(all.Relations))
		}
		if last.Meta.Truncated {
			t.Fatalf("the retried walk ended truncated with %q", last.Meta.TruncationReason)
		}
		if last.VisitedCount != all.VisitedCount {
			t.Fatalf("the retried walk reports visited_count %d, want the %d the unbounded walk "+
				"reports: the served level's admissions were charged against another level's frontier",
				last.VisitedCount, all.VisitedCount)
		}
		return
	}
}

// cursorLevelState is the level state a continuation resumes in.
func cursorLevelState(t *testing.T, e *Engine, token string) levelState {
	t.Helper()
	payload, err := e.signer.Verify(token, pagination.PurposeCursor, e.now())
	if err != nil {
		t.Fatalf("verify cursor: %v", err)
	}
	var c traversalCursor
	if err := json.Unmarshal(payload, &c); err != nil {
		t.Fatalf("decode cursor: %v", err)
	}
	return c.LevelState
}
