package graph

import (
	"context"
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/Sawmonabo/codectx/internal/config"
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
// endpoints that had no honest continuation: impact and the package rollup used
// to walk the whole reachable subgraph on one page, holding every admitted edge
// and every affected node in heap, and then ranked whatever chunk the page had
// read. Both now run ruling P2's shape -- the request that mints the answer
// walks to COMPLETION, streams every admitted edge into a disk-backed sort,
// ranks the whole answer once, serves the first page and spools the globally
// ranked remainder behind the `r` cursor.
//
// What that makes provable, and what this asserts for BOTH lists of BOTH
// endpoints: the pages CONCATENATE to the single-shot answer -- same records,
// same global order, each exactly once -- rather than merely covering the same
// set. A page-ranked answer satisfies set equality and fails every assertion
// below.
//
// Mutation proofs (each fails this test):
//   - drop pass 2 of the rank (return pass 1's folded run from rankImpact or
//     rankPairs): the pages carry the identity order, not the ranked one;
//   - drop `.WithFold(foldImpact)` from pass 1: a node reached by two edges is
//     listed twice, so the pages hold more records than the single-shot answer;
//   - drop `.WithFold(foldPair)` from the pair sort: the pair counts are 1 each
//     instead of the whole walk's sums.
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
		// bound as the thing that ends a page.
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
	// A page bound far above the fixture's record count is the single-shot
	// answer the pages are compared against.
	const wholePage = 100000
	// Two: small enough that BOTH of impact's lists -- the affected entities
	// and the package pairs the same walk rolled up -- span several pages of the
	// fixture, which is what makes the order assertions below load-bearing.
	const pageItems = 2

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
		if len(whole.Items) <= pageItems {
			t.Fatalf("the whole-walk rollup found %d pair(s); the proof needs more than one page of %d",
				len(whole.Items), pageItems)
		}

		var concatenated []model.PackageEdge
		paged := engine(t, pageItems)
		pages := 0
		for {
			pages++
			if pages > 200 {
				t.Fatalf("the paged rollup did not terminate after %d pages", pages-1)
			}
			res, err := paged.PackageDependencies(context.Background(), req)
			if err != nil {
				t.Fatalf("page %d: %v", pages, err)
			}
			if len(res.Items) > pageItems {
				t.Fatalf("page %d served %d pairs over a page bound of %d", pages, len(res.Items), pageItems)
			}
			concatenated = append(concatenated, res.Items...)
			if res.Meta.NextCursor == "" {
				break
			}
			req.Page = model.PageRequest{Cursor: res.Meta.NextCursor}
			req.GenerationID = 0
		}
		if pages < 2 {
			t.Fatalf("the paged rollup answered in %d page(s); the proof needs a continuation to consume", pages)
		}
		// ORDER and identity, not set equality: every pair once, in the
		// single-shot rank, with the whole walk's exact counts.
		if len(concatenated) != len(whole.Items) {
			t.Fatalf("the pages carried %d pair(s), the single-shot answer %d: a pair is duplicated or missing",
				len(concatenated), len(whole.Items))
		}
		for i, want := range whole.Items {
			if concatenated[i] != want {
				t.Fatalf("at rank %d the pages carry %+v, the single-shot answer %+v", i, concatenated[i], want)
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
		if len(whole.Entries) <= pageItems || len(whole.Packages) == 0 {
			t.Fatalf("the whole-walk impact found %d entr(ies) and %d package pair(s); the proof needs more than one page of %d entries and at least one pair",
				len(whole.Entries), len(whole.Packages), pageItems)
		}

		paged := engine(t, pageItems)
		var entries []model.ImpactEntry
		var packages []model.PackageEdge
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
			if len(res.Entries) > pageItems || len(res.Packages) > pageItems {
				t.Fatalf("page %d served %d entries and %d pairs over a page bound of %d",
					pages, len(res.Entries), len(res.Packages), pageItems)
			}
			entries = append(entries, res.Entries...)
			packages = append(packages, res.Packages...)
			if res.Meta.NextCursor == "" {
				break
			}
			req.Page = model.PageRequest{Cursor: res.Meta.NextCursor}
			req.GenerationID = 0
		}
		if pages < 2 {
			t.Fatalf("the paged impact answered in %d page(s); the proof needs a continuation to consume", pages)
		}
		// BOTH lists concatenate to the single-shot answer in the single-shot
		// order. Uniqueness is implied and asserted separately, because a
		// duplicate is the specific defect ruling P2 removes.
		if len(entries) != len(whole.Entries) {
			t.Fatalf("the pages served %d affected entit(ies), the single-shot answer %d: an entity is duplicated or missing",
				len(entries), len(whole.Entries))
		}
		seen := map[model.NodeID]bool{}
		for i, want := range whole.Entries {
			if entries[i].NodeID != want.NodeID || entries[i].ScoreMicros != want.ScoreMicros ||
				entries[i].Depth != want.Depth || entries[i].Direction != want.Direction {
				t.Fatalf("at rank %d the pages report %s (score %d, depth %d, %s), the single-shot answer %s (score %d, depth %d, %s)",
					i, entries[i].NodeID, entries[i].ScoreMicros, entries[i].Depth, entries[i].Direction,
					want.NodeID, want.ScoreMicros, want.Depth, want.Direction)
			}
			if seen[entries[i].NodeID] {
				t.Fatalf("node %s is listed twice across the pages", entries[i].NodeID)
			}
			seen[entries[i].NodeID] = true
		}
		if len(packages) != len(whole.Packages) {
			t.Fatalf("the pages carried %d package pair(s), the single-shot answer %d",
				len(packages), len(whole.Packages))
		}
		for i, want := range whole.Packages {
			if packages[i] != want {
				t.Fatalf("at rank %d the pages carry the pair %+v, the single-shot answer %+v", i, packages[i], want)
			}
		}
	})
}

// exactFillAdjacency serves a fixed containment row set through the keyset
// protocol so a read can be given a budget that its rows fill EXACTLY. The
// fixture's own graph cannot express that: the rollup derives the budget from
// the batch size, so no fixture shape makes the two land on the same number.
type exactFillAdjacency struct {
	*graphFixture
	rows []model.Relation
}

func (a exactFillAdjacency) Edges(_ context.Context, _ []model.NodeID, _ model.Direction,
	_ []model.RelationKind, after model.RelationID, limit int) ([]model.Relation, error) {
	out := a.rows
	for len(out) > 0 && out[0].ID <= after {
		out = out[1:]
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return append([]model.Relation(nil), out...), nil
}

// TestContainmentReadEndsOnTheEmptyPage is F7. streamContainsEdges used to exit
// its loop the moment the budget was reached, so a containment set whose rows
// fill the budget exactly reported complete=false -- and both callers (the
// rollup and the repository map) refuse an incomplete containment read
// outright, turning a whole answer into CTX_RESOURCE_LIMIT.
//
// Mutation: replace the one-row probe in streamContainsEdges with
// `return false, nil` and the exact-fill leg below fails.
func TestContainmentReadEndsOnTheEmptyPage(t *testing.T) {
	f := newGraphFixture(t)
	rows := make([]model.Relation, 4)
	for i := range rows {
		rows[i] = model.Relation{
			ID:   fixtureRelationID(0xe00 + i),
			From: fixtureNodeID("n-a"), To: fixtureNodeID("n-b"), Kind: model.RelContains,
		}
	}
	e, err := New(Options{Adjacency: exactFillAdjacency{f, rows}, Limits: fixtureLimits()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	batch := []model.NodeID{fixtureNodeID("n-b")}
	for _, tc := range []struct {
		name     string
		budget   int64
		wantRead int
		wantDone bool
	}{
		{name: "the budget is exactly filled", budget: int64(len(rows)), wantRead: len(rows), wantDone: true},
		{name: "a row is left past the budget", budget: int64(len(rows)) - 1, wantRead: len(rows) - 1, wantDone: false},
		{name: "the budget is unlimited", budget: 0, wantRead: len(rows), wantDone: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			read := 0
			done, err := e.streamContainsEdges(context.Background(), batch, model.DirectionIncoming,
				[]model.RelationKind{model.RelContains}, config.Limit(tc.budget),
				func(model.Relation) error { read++; return nil })
			if err != nil {
				t.Fatalf("streamContainsEdges: %v", err)
			}
			if read != tc.wantRead || done != tc.wantDone {
				t.Fatalf("read %d row(s), complete=%v; want %d row(s), complete=%v",
					read, done, tc.wantRead, tc.wantDone)
			}
		})
	}
}

// widePackageCount is the affected-package fan-out of the VF1/VF2 shape. It is
// deliberately above model.MaxRecordsPerResult so a single-shot answer would
// carry more package pairs than one result may hold.
const widePackageCount = model.MaxRecordsPerResult + 284

// TestImpactPagesPastTheResultRecordCap is VF1 and VF2 from the real-repository
// verification. On a 4019-file repository `impact --depth 0 --visited 0
// --edges 0` was REFUSED on page 1 -- `impact_result.packages has 1284
// entries, limit 1000` -- because the whole walk's rollup was built into one
// result; on another repository the same walk silently stopped at exactly 1000
// affected entities with truncation_reason "affected entity record limit
// reached", no next_cursor, and visited/edge counters frozen across five
// pages, so the rest of the blast radius was unreachable at any page size.
//
// The record cap is a per-PAGE bound, not a ceiling on the answer: the walk
// stops at the page's item bound and mints a continuation, so neither list can
// reach the cap and no page can report "record limit" with no cursor at all.
//
// Mutation: restore the single-shot walk (the `len(a.edges) >= a.pageItems`
// stop in impactAccumulator.Visit) and page 1 fails its own contract with
// CTX_ARGUMENT_INVALID, which is the refusal this row reproduces.
func TestImpactPagesPastTheResultRecordCap(t *testing.T) {
	f := newGraphFixture(t)
	seed := fixtureNodeID("vf-seed")
	seedPkg := fixtureNodeID("vf-pkg-seed")
	addNode := func(id model.NodeID, kind model.NodeKind, name string) {
		f.nodes[id] = model.Node{ID: id, Kind: kind, Name: name, QualifiedName: name,
			Language: "go", SemanticSource: model.SemanticCanonical}
	}
	addRel := func(kind model.RelationKind, from, to model.NodeID) {
		f.relations = append(f.relations, model.Relation{
			ID: fixtureRelationID(len(f.relations)), Kind: kind, From: from, To: to})
	}
	addNode(seed, model.NodeFunction, "vf-seed")
	addNode(seedPkg, model.NodePackage, "vf-pkg-seed")
	addRel(model.RelContains, seedPkg, seed)
	for i := 0; i < widePackageCount; i++ {
		callee := fixtureNodeID(fmt.Sprintf("vf-callee-%d", i))
		pkg := fixtureNodeID(fmt.Sprintf("vf-pkg-%04d", i))
		addNode(callee, model.NodeFunction, fmt.Sprintf("vf-callee-%d", i))
		addNode(pkg, model.NodePackage, fmt.Sprintf("vf-pkg-%04d", i))
		addRel(model.RelCalls, seed, callee)
		addRel(model.RelContains, pkg, callee)
	}
	sort.Slice(f.relations, func(i, j int) bool { return f.relations[i].ID < f.relations[j].ID })

	signer, err := pagination.OpenSigner(t.TempDir())
	if err != nil {
		t.Fatalf("open signer: %v", err)
	}
	store := newFixtureLeases()
	spools, err := pagination.NewSpools(t.TempDir(), 32<<20, store)
	if err != nil {
		t.Fatalf("new spools: %v", err)
	}
	limits := fixtureLimits()
	// The refused invocation's own bounds: every count budget unlimited.
	limits.MaxDepth, limits.MaxVisited, limits.MaxEdges = 0, 0, 0
	e, err := New(Options{Adjacency: f, Signer: signer, Spools: spools,
		Leases: pagination.NewLeases(store, limits.CursorTTL), Limits: limits})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}

	req := model.ImpactRequest{GenerationID: 1, Start: []model.NodeID{seed},
		Direction: model.DirectionOutgoing, Relations: []model.RelationKind{model.RelCalls}}
	type pair struct{ from, to model.NodeID }
	pkgs := map[pair]bool{}
	entries := map[model.NodeID]bool{}
	pages := 0
	for {
		pages++
		if pages > 200 {
			t.Fatalf("the paged impact did not terminate after %d pages", pages-1)
		}
		res, err := e.Impact(context.Background(), req)
		if err != nil {
			t.Fatalf("page %d: %v", pages, err)
		}
		// The refusal VF1 reproduces is exactly this contract check.
		if err := res.Validate(); err != nil {
			t.Fatalf("page %d does not satisfy its own contract: %v", pages, err)
		}
		if len(res.Packages) > limits.MaxPageItems || len(res.Entries) > limits.MaxPageItems {
			t.Fatalf("page %d carried %d entries and %d package pairs over a page bound of %d",
				pages, len(res.Entries), len(res.Packages), limits.MaxPageItems)
		}
		if res.Meta.NextCursor == "" && len(pkgs)+len(res.Packages) < widePackageCount {
			// VF2: a page that leaves affected packages unserved and offers no
			// continuation makes the rest of the blast radius unreachable.
			t.Fatalf("page %d served the last of %d package pair(s) with no continuation (truncated=%v reason=%q)",
				pages, len(pkgs)+len(res.Packages), res.Meta.Truncated, res.Meta.TruncationReason)
		}
		for _, p := range res.Packages {
			pkgs[pair{p.FromNodeID, p.ToNodeID}] = true
		}
		for _, entry := range res.Entries {
			entries[entry.NodeID] = true
		}
		if res.Meta.NextCursor == "" {
			break
		}
		req.Page = model.PageRequest{Cursor: res.Meta.NextCursor}
		req.GenerationID = 0
	}
	if len(pkgs) != widePackageCount {
		t.Errorf("the pages aggregated %d distinct package pairs, want the whole walk's %d",
			len(pkgs), widePackageCount)
	}
	if len(entries) != widePackageCount {
		t.Errorf("the pages served %d distinct affected entities, want %d", len(entries), widePackageCount)
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
