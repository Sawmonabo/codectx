package graph

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
)

// TestResumableFrontierCompletesAcrossPages is the row-13 proof: a walk whose
// visited budget is far smaller than the graph still returns EVERY edge, across
// pages, and returns each exactly once.
//
// Before this wave, spending max_visited_nodes returned a truncated answer with
// no continuation (cursor.go withheld the token, traverse.go minted one only
// for a full page), so the edges past the budget were unreachable at any page
// size. The mutation that proves this row: restore that withholding -- make
// traverse.go mint only when `reason == reasonPageFull`, or reinstate the
// `b.visited >= MaxVisited` guard in nextTraversalCursor -- and the union of the
// pages is a strict subset of the unlimited answer, so this test fails.
//
// Peak heap here is the per-page frontier plus the spool write buffer, not the
// graph: the frontier and the visited set live in the continuation spool
// between pages, which is what makes a 50-node budget legal over a 500+ node
// fixture in the first place.
func TestResumableFrontierCompletesAcrossPages(t *testing.T) {
	f := newGraphFixture(t)
	if len(f.nodes) < 500 {
		t.Fatalf("fixture has %d nodes; this proof needs a graph larger than the budgets it sets", len(f.nodes))
	}

	engine := func(t *testing.T, maxVisited int) *Engine {
		t.Helper()
		signer, err := pagination.OpenSigner(t.TempDir())
		if err != nil {
			t.Fatalf("open signer: %v", err)
		}
		store := newFixtureLeases()
		spools, err := pagination.NewSpools(t.TempDir(), 1<<20, store)
		if err != nil {
			t.Fatalf("new spools: %v", err)
		}
		limits := fixtureLimits()
		// Unlimited depth and edges: this row isolates the visited budget.
		limits.MaxDepth, limits.MaxEdges = 0, 0
		limits.MaxVisited = maxVisited
		limits.MaxPageItems = 2000
		e, err := New(Options{Adjacency: f, Signer: signer, Spools: spools,
			Leases: pagination.NewLeases(store, limits.CursorTTL), Limits: limits})
		if err != nil {
			t.Fatalf("new engine: %v", err)
		}
		return e
	}

	req := model.GraphRequest{GenerationID: 1,
		Start:     []model.NodeID{fixtureNodeID("n-a"), fixtureNodeID("n-wide")},
		Direction: model.DirectionOutgoing, Relations: []model.RelationKind{model.RelCalls}}

	// Ground truth: the same walk with no visited budget at all.
	whole, err := engine(t, 0).Neighbors(context.Background(), req)
	if err != nil {
		t.Fatalf("unlimited walk: %v", err)
	}
	if whole.Meta.Truncated {
		t.Fatalf("the unlimited walk must be complete, got %q", whole.Meta.TruncationReason)
	}
	if len(whole.Relations) == 0 {
		t.Fatal("the unlimited walk returned no edges; the proof would be vacuous")
	}

	paged := engine(t, 50)
	seen := map[model.RelationID]int{}
	sawVisitedStop := false
	pages := 0
	for {
		pages++
		if pages > 200 {
			t.Fatalf("the paged walk did not terminate after %d pages", pages-1)
		}
		res, err := paged.Neighbors(context.Background(), req)
		if err != nil {
			t.Fatalf("page %d: %v", pages, err)
		}
		if res.Meta.TruncationReason == reasonVisitedBudget {
			sawVisitedStop = true
			if res.Meta.NextCursor == "" {
				t.Fatalf("page %d spent the visited budget with no continuation: the frontier is unreachable",
					pages)
			}
		}
		for _, rel := range res.Relations {
			seen[rel.ID]++
		}
		if res.Meta.NextCursor == "" {
			break
		}
		// A cursor pins its own generation, so a continuation names the cursor
		// and nothing else the cursor already carries.
		req.Page = model.PageRequest{Cursor: res.Meta.NextCursor}
		req.GenerationID = 0
	}
	if !sawVisitedStop {
		t.Fatal("no page stopped on the visited budget; the proof did not exercise the bound it names")
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("relation %s was returned %d times across pages, want exactly once", id, n)
		}
	}
	for _, rel := range whole.Relations {
		if seen[rel.ID] == 0 {
			t.Errorf("relation %s is in the unlimited answer but no page returned it", rel.ID)
		}
	}
	if len(seen) != len(whole.Relations) {
		t.Errorf("the pages returned %d distinct edges, the unlimited walk %d", len(seen), len(whole.Relations))
	}
}

// TestDepthBoundIsReportedNotSilent is the row-14 proof. A walk that ran out of
// depth with nodes still unexpanded used to fall out of the loop with
// Truncated=false and no reason at all: a partial answer that read as a whole
// one. It now reports reasonDepth, and offers no continuation -- see
// walkState.DepthLimited for why one would be a cursor chain that never ends.
//
// Mutation: delete the `state.DepthLimited` branch in traverse and Truncated
// goes false, which fails the first assertion here.
func TestDepthBoundIsReportedNotSilent(t *testing.T) {
	f := newGraphFixture(t)
	limits := fixtureLimits()
	// Two hops over a fixture that is deeper than two hops: the third level is
	// admitted into the frontier and never expanded.
	limits.MaxDepth = 2
	limits.MaxVisited, limits.MaxEdges = 0, 0
	limits.MaxPageItems = 2000
	e, err := New(Options{Adjacency: f, Limits: limits})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	res, err := e.Neighbors(context.Background(), model.GraphRequest{GenerationID: 1,
		Start:     []model.NodeID{fixtureNodeID("n-a")},
		Direction: model.DirectionOutgoing, Relations: []model.RelationKind{model.RelCalls}})
	if err != nil {
		t.Fatalf("Neighbors: %v", err)
	}
	if !res.Meta.Truncated || res.Meta.TruncationReason != reasonDepth {
		t.Fatalf("truncated = %v, reason = %q; want a depth-limited walk to say so",
			res.Meta.Truncated, res.Meta.TruncationReason)
	}
	if res.Meta.NextCursor != "" {
		t.Error("a depth-limited walk offered a continuation; resuming it can only stop at the same bound")
	}
	if res.MaxDepth != 2 {
		t.Errorf("meta echoes max_depth %d, want the effective bound 2", res.MaxDepth)
	}

	// The same walk with the bound removed is complete: the truncation above is
	// the bound's doing, not the fixture running out of graph.
	limits.MaxDepth = 0
	deep, err := New(Options{Adjacency: f, Limits: limits})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	whole, err := deep.Neighbors(context.Background(), model.GraphRequest{GenerationID: 1,
		Start:     []model.NodeID{fixtureNodeID("n-a")},
		Direction: model.DirectionOutgoing, Relations: []model.RelationKind{model.RelCalls}})
	if err != nil {
		t.Fatalf("unlimited walk: %v", err)
	}
	if whole.Meta.Truncated {
		t.Fatalf("the unlimited walk is truncated (%q); the depth row proves nothing",
			whole.Meta.TruncationReason)
	}
	if len(whole.Relations) <= len(res.Relations) {
		t.Errorf("unlimited walk returned %d edges, the depth-bounded one %d; the bound cut nothing",
			len(whole.Relations), len(res.Relations))
	}
}

// TestFrontierBytesSpillsAndTerminates is the row-16 proof: a frontier byte
// budget smaller than a single edge row must still return every edge, across
// pages, and must terminate. Admitting at least one edge per level-read is what
// guarantees that -- without it the resumed page re-reads the same level from
// the same keyset position, admits nothing and mints again forever.
//
// Mutation: drop the `spent > 0` guard in levelEdges and this test fails on the
// page ceiling below, having returned no edges at all.
func TestFrontierBytesSpillsAndTerminates(t *testing.T) {
	f := newGraphFixture(t)
	signer, err := pagination.OpenSigner(t.TempDir())
	if err != nil {
		t.Fatalf("open signer: %v", err)
	}
	store := newFixtureLeases()
	spools, err := pagination.NewSpools(t.TempDir(), 1<<20, store)
	if err != nil {
		t.Fatalf("new spools: %v", err)
	}
	limits := fixtureLimits()
	limits.MaxDepth, limits.MaxVisited, limits.MaxEdges = 0, 0, 0
	limits.MaxPageItems = 2000
	// Below edgeRowOverheadBytes, so EVERY row on its own exceeds the budget.
	limits.FrontierBytes = 1
	e, err := New(Options{Adjacency: f, Signer: signer, Spools: spools,
		Leases: pagination.NewLeases(store, limits.CursorTTL), Limits: limits})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	req := model.GraphRequest{GenerationID: 1,
		Start:     []model.NodeID{fixtureNodeID("n-a")},
		Direction: model.DirectionOutgoing, Relations: []model.RelationKind{model.RelCalls}}
	seen := map[model.RelationID]int{}
	for page := 1; ; page++ {
		if page > 500 {
			t.Fatalf("the walk did not terminate after %d pages with %d edges seen", page-1, len(seen))
		}
		res, err := e.Neighbors(context.Background(), req)
		if err != nil {
			t.Fatalf("page %d: %v", page, err)
		}
		for _, rel := range res.Relations {
			seen[rel.ID]++
		}
		if res.Meta.NextCursor == "" {
			break
		}
		req.Page = model.PageRequest{Cursor: res.Meta.NextCursor}
		req.GenerationID = 0
	}
	if len(seen) == 0 {
		t.Fatal("a one-byte frontier budget returned no edges at all")
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("relation %s was returned %d times, want exactly once", id, n)
		}
	}
}

// slowAdjacency is the fixture reader with a clock attached: after `trigger`
// Edges round trips it pushes the shared clock past the engine's deadline,
// ONCE. That is what makes the deadline arrive at a point the test chooses --
// mid-walk, with edges already admitted -- instead of at whatever point a real
// clock happens to reach, and the one-shot jump is what lets the continuation
// finish on a fresh deadline.
type slowAdjacency struct {
	*graphFixture
	clock   *time.Time
	calls   *int
	trigger int
	jump    time.Duration
	fired   *bool
}

func (s slowAdjacency) Edges(ctx context.Context, nodes []model.NodeID, dir model.Direction,
	kinds []model.RelationKind, after model.RelationID, limit int) ([]model.Relation, error) {
	*s.calls++
	if !*s.fired && *s.calls >= s.trigger {
		*s.fired = true
		*s.clock = s.clock.Add(s.jump)
	}
	return s.graphFixture.Edges(ctx, nodes, dir, kinds, after, limit)
}

// TestDeadlineEndsThePageNotTheAnswer is the F8 proof. Ruling Q4 makes
// query_timeout end a PAGE, not an answer: a walk that runs out of time with
// edges already admitted must return them, say so, and hand back a cursor --
// the same contract the page, visited, edge and frontier-byte stops keep.
// Before this it returned model.GraphResult{} and CTX_QUERY_DEADLINE, so a walk
// too big for one timeout could never progress at all.
//
// Mutation (`return walkState{}, err` restored at expand's loop-top check, i.e.
// deadlineStop deleted): the first page below fails with CTX_QUERY_DEADLINE and
// no answer, which is the assertion this test leads with.
func TestDeadlineEndsThePageNotTheAnswer(t *testing.T) {
	f := newGraphFixture(t)
	signer, err := pagination.OpenSigner(t.TempDir())
	if err != nil {
		t.Fatalf("open signer: %v", err)
	}
	store := newFixtureLeases()
	spools, err := pagination.NewSpools(t.TempDir(), 1<<20, store)
	if err != nil {
		t.Fatalf("new spools: %v", err)
	}
	limits := fixtureLimits()
	limits.MaxDepth, limits.MaxVisited, limits.MaxEdges = 0, 0, 0
	limits.MaxPageItems = 2000
	limits.QueryTimeout = time.Minute

	req := model.GraphRequest{GenerationID: 1,
		Start:     []model.NodeID{fixtureNodeID("n-a")},
		Direction: model.DirectionOutgoing, Relations: []model.RelationKind{model.RelCalls}}

	// Ground truth: the same walk that never runs out of time.
	whole, err := New(Options{Adjacency: f, Signer: signer, Spools: spools,
		Leases: pagination.NewLeases(store, limits.CursorTTL), Limits: limits})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	all, err := whole.Neighbors(context.Background(), req)
	if err != nil {
		t.Fatalf("unlimited walk: %v", err)
	}
	if all.Meta.Truncated || len(all.Relations) == 0 {
		t.Fatalf("ground truth is truncated (%q) or empty (%d edges)",
			all.Meta.TruncationReason, len(all.Relations))
	}

	// A real starting instant: the engine builds a context.WithDeadline from
	// this clock, and a fake epoch would make that context already expired
	// against the real one the runtime enforces it on.
	// Two round trips is level 0 (the rows, then the empty page that ends its
	// keyset walk), so trigger 2 lands the deadline at level 1's loop-top check
	// and 3 and 4 land it INSIDE level 1's reader loop -- the two stops are
	// different code paths and each must keep the same contract.
	for _, trigger := range []int{2, 3, 4} {
		t.Run(fmt.Sprintf("deadline-after-%d-reads", trigger), func(t *testing.T) {
			deadlinePageCase(t, f, signer, spools, store, limits, req, all, trigger)
		})
	}
}

func deadlinePageCase(t *testing.T, f *graphFixture, signer *pagination.Signer,
	spools *pagination.Spools, store *fixtureLeases, limits Limits,
	req model.GraphRequest, all model.GraphResult, trigger int) {
	t.Helper()
	clock := time.Now()
	calls, fired := 0, false
	slow := slowAdjacency{graphFixture: f, clock: &clock, calls: &calls,
		trigger: trigger, jump: 2 * time.Minute, fired: &fired}
	e, err := New(Options{Adjacency: slow, Signer: signer, Spools: spools,
		Leases: pagination.NewLeases(store, limits.CursorTTL), Limits: limits,
		Now: func() time.Time { return clock }})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}

	first, err := e.Neighbors(context.Background(), req)
	if err != nil {
		t.Fatalf("a deadline threw the page away instead of ending it: %v", err)
	}
	if len(first.Relations) == 0 {
		t.Fatal("the deadline-stopped page returned no edges; it must return what it read")
	}
	if !first.Meta.Truncated || first.Meta.TruncationReason != reasonDeadline {
		t.Fatalf("truncated = %v, reason = %q; want %q",
			first.Meta.Truncated, first.Meta.TruncationReason, reasonDeadline)
	}
	if first.Meta.NextCursor == "" {
		t.Fatal("the deadline-stopped page minted no cursor; its frontier is unreachable")
	}

	// The continuation runs on a fresh deadline and finishes the walk: the
	// stop ended a page, so the answer is still reachable.
	seen := map[model.RelationID]int{}
	for _, rel := range first.Relations {
		seen[rel.ID]++
	}
	req.Page = model.PageRequest{Cursor: first.Meta.NextCursor}
	req.GenerationID = 0
	for page := 2; ; page++ {
		if page > 200 {
			t.Fatalf("the walk did not terminate after %d pages", page-1)
		}
		res, err := e.Neighbors(context.Background(), req)
		if err != nil {
			t.Fatalf("page %d: %v", page, err)
		}
		for _, rel := range res.Relations {
			seen[rel.ID]++
		}
		if res.Meta.NextCursor == "" {
			break
		}
		req.Page = model.PageRequest{Cursor: res.Meta.NextCursor}
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("relation %s was returned %d times across pages, want exactly once", id, n)
		}
	}
	if len(seen) != len(all.Relations) {
		t.Errorf("the deadline-paged walk returned %d distinct edges, the unlimited one %d",
			len(seen), len(all.Relations))
	}
}

// TestFrontierBytesMustBePositive is the F30 proof: frontier_bytes is the only
// bound on how much of one level is held in heap, so zero there is not
// "unlimited" the way a scale cap's zero is -- it is no ceiling at all.
func TestFrontierBytesMustBePositive(t *testing.T) {
	f := newGraphFixture(t)
	limits := fixtureLimits()
	limits.FrontierBytes = 0
	if _, err := New(Options{Adjacency: f, Limits: limits}); err == nil {
		t.Fatal("New accepted frontier_bytes = 0: a level would have no heap bound at all")
	}
}
