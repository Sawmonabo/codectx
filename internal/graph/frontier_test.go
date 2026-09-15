package graph

import (
	"context"
	"testing"

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

// TestImpactAndPackageDepsResumeAcrossPages is the R3 proof for the two
// endpoints that had no continuation at all: impact and the package rollup used
// to walk the WHOLE reachable subgraph on one page, holding every admitted edge
// and every affected node in impactAccumulator.byNode/order/edges, and then
// minted either a ranked-tail spool (impact) or nothing (the rollup). Both now
// run the same page-bounded, spooled, cursor-resumable walk a traversal runs:
// the accumulator stops at the page's item bound and the frontier plus the
// cumulative admitted set go to the continuation spool.
//
// What the union of pages preserves, and what this asserts:
//   - the rollup's PairCount and EvidenceCount SUM, per pair, to the whole-walk
//     totals -- every edge is admitted on exactly one page, so the aggregate is
//     split across pages rather than lost;
//   - impact's affected-node SET is exactly the whole-walk set.
//
// What it deliberately does not assert is entry identity: a node admitted on an
// early page is reported again if a later page's frontier reaches it, because
// the accumulator is page-scoped by construction while the admitted set lives on
// the spool. That is the cost of a page-bounded aggregate and it is documented
// on walkImpact.
//
// Mutation proof (each fails this test):
//   - drop the `len(a.edges) >= a.pageItems` stop in impactAccumulator.Visit and
//     the walk is single-shot again: no page fills, sawPageFull is false;
//   - drop `Resume: resume` from either expand call and the resumed page
//     restarts from the seeds, so the pair counts sum to more than the whole
//     walk's and the same edges are counted twice;
//   - drop the continueWalk call in either endpoint and the pages stop after the
//     first, so the union is a strict subset of the whole-walk answer.
func TestImpactAndPackageDepsResumeAcrossPages(t *testing.T) {
	f := newGraphFixture(t)
	engine := func(t *testing.T, pageItems int) *Engine {
		t.Helper()
		signer, err := pagination.OpenSigner(t.TempDir())
		if err != nil {
			t.Fatalf("open signer: %v", err)
		}
		store := newFixtureLeases()
		spools, err := pagination.NewSpools(t.TempDir(), 8<<20, store)
		if err != nil {
			t.Fatalf("new spools: %v", err)
		}
		limits := fixtureLimits()
		// Unlimited depth, visited and edge budgets: this row isolates the page
		// item bound as the thing that ends a page.
		limits.MaxDepth, limits.MaxVisited, limits.MaxEdges = 0, 0, 0
		limits.MaxPageItems = pageItems
		e, err := New(Options{Adjacency: f, Signer: signer, Spools: spools,
			Leases: pagination.NewLeases(store, limits.CursorTTL), Limits: limits})
		if err != nil {
			t.Fatalf("new engine: %v", err)
		}
		return e
	}
	seeds := []model.NodeID{fixtureNodeID("n-a"), fixtureNodeID("n-wide")}
	// A page bound far above the fixture's edge count is the single-shot answer
	// the pages are compared against.
	const wholePage = 100000
	const pageItems = 5

	t.Run("package dependencies", func(t *testing.T) {
		req := model.GraphRequest{GenerationID: 1, Start: seeds,
			Direction: model.DirectionOutgoing, Relations: []model.RelationKind{model.RelCalls}}
		whole, err := engine(t, wholePage).PackageDependencies(context.Background(), req)
		if err != nil {
			t.Fatalf("whole-walk rollup: %v", err)
		}
		if whole.Meta.NextCursor != "" || whole.Meta.Truncated {
			t.Fatalf("the ground truth must be one complete answer, got truncated=%v reason=%q",
				whole.Meta.Truncated, whole.Meta.TruncationReason)
		}
		if len(whole.Items) == 0 {
			t.Fatal("the whole-walk rollup found no package pairs; the proof would be vacuous")
		}

		type pair struct{ from, to model.NodeID }
		summed := map[pair]model.PackageEdge{}
		paged := engine(t, pageItems)
		pages, sawPageFull := 0, false
		for {
			pages++
			if pages > 200 {
				t.Fatalf("the paged rollup did not terminate after %d pages", pages-1)
			}
			res, err := paged.PackageDependencies(context.Background(), req)
			if err != nil {
				t.Fatalf("page %d: %v", pages, err)
			}
			if res.Meta.TruncationReason == reasonPageFull {
				sawPageFull = true
				if res.Meta.NextCursor == "" {
					t.Fatalf("page %d filled its page with no continuation: the frontier is unreachable", pages)
				}
			}
			for _, item := range res.Items {
				k := pair{item.FromNodeID, item.ToNodeID}
				acc := summed[k]
				acc.FromNodeID, acc.ToNodeID = item.FromNodeID, item.ToNodeID
				acc.FromPath, acc.ToPath = item.FromPath, item.ToPath
				acc.PairCount += item.PairCount
				acc.EvidenceCount += item.EvidenceCount
				summed[k] = acc
			}
			if res.Meta.NextCursor == "" {
				break
			}
			req.Page = model.PageRequest{Cursor: res.Meta.NextCursor}
			req.GenerationID = 0
		}
		if !sawPageFull {
			t.Fatal("no page stopped on its item bound; the proof did not exercise the bound it names")
		}
		if len(summed) != len(whole.Items) {
			t.Fatalf("the pages aggregated %d distinct pairs, the whole walk %d", len(summed), len(whole.Items))
		}
		for _, want := range whole.Items {
			got, ok := summed[pair{want.FromNodeID, want.ToNodeID}]
			if !ok {
				t.Fatalf("pair %s -> %s is in the whole-walk rollup but no page carried it", want.FromPath, want.ToPath)
			}
			if got.PairCount != want.PairCount || got.EvidenceCount != want.EvidenceCount {
				t.Errorf("pair %s -> %s summed to (%d edges, %d evidence) across pages, want the whole walk's (%d, %d)",
					want.FromPath, want.ToPath, got.PairCount, got.EvidenceCount, want.PairCount, want.EvidenceCount)
			}
		}
	})

	t.Run("impact", func(t *testing.T) {
		// The request's own page.limit is wire-bounded at model.MaxPageItems, so
		// the page size under test is the ENGINE's configured bound and the
		// request leaves it unset -- which is also what keeps the query hash
		// identical across the pages of one walk.
		// n-hub joins the seeds here: the impact allowlist is narrower than the
		// rollup's relation filter, and the hub is what makes the blast radius
		// larger than one page of it.
		req := model.ImpactRequest{GenerationID: 1, Direction: model.DirectionBoth,
			Start: append(append([]model.NodeID(nil), seeds...), fixtureNodeID("n-hub"))}
		whole, err := engine(t, wholePage).Impact(context.Background(), req)
		if err != nil {
			t.Fatalf("whole-walk impact: %v", err)
		}
		if whole.Meta.NextCursor != "" {
			t.Fatal("the ground truth must be one complete answer")
		}
		if len(whole.Entries) == 0 {
			t.Fatal("the whole-walk impact found no affected entities; the proof would be vacuous")
		}
		wholeSet := map[model.NodeID]bool{}
		for _, entry := range whole.Entries {
			wholeSet[entry.NodeID] = true
		}

		paged := engine(t, pageItems)
		seen := map[model.NodeID]bool{}
		pages := 0
		for {
			pages++
			if pages > 200 {
				t.Fatalf("the paged impact did not terminate after %d pages", pages-1)
			}
			res, err := paged.Impact(context.Background(), req)
			if err != nil {
				t.Fatalf("page %d: %v", pages, err)
			}
			if err := res.Validate(); err != nil {
				t.Fatalf("page %d does not satisfy its own contract: %v", pages, err)
			}
			if len(res.Entries) > pageItems {
				t.Fatalf("page %d served %d entries over a page bound of %d: the accumulator is not page-bounded",
					pages, len(res.Entries), pageItems)
			}
			for _, entry := range res.Entries {
				seen[entry.NodeID] = true
			}
			if res.Meta.NextCursor == "" {
				break
			}
			req.Page = model.PageRequest{Cursor: res.Meta.NextCursor}
			req.GenerationID = 0
		}
		if pages < 2 {
			// The truncation REASON cannot carry this assertion: the fixture's
			// generation has deferred dependence units, and markTruncated keeps
			// the first reason, so reasonDependence is reported in place of the
			// page stop. A second page existing at all is the observable that
			// the page bound and the continuation both worked.
			t.Fatalf("the paged impact answered in %d page(s); the proof needs a continuation to consume", pages)
		}
		for id := range wholeSet {
			if !seen[id] {
				t.Errorf("node %s is affected in the whole-walk answer but no page reported it", id)
			}
		}
		for id := range seen {
			if !wholeSet[id] {
				t.Errorf("node %s was reported by a page but is not in the whole-walk answer", id)
			}
		}
	})
}
