package graph

import (
	"context"
	"fmt"
	"runtime"
	"testing"
	"time"

	"github.com/Sawmonabo/codectx/internal/pagination"

	"github.com/Sawmonabo/codectx/internal/model"
)

// syntheticNodes is the size of the walk the cumulative set is measured
// against: 200 000 nodes already admitted by earlier pages of one continuation
// chain. A map of that many 64-hex NodeIDs is tens of megabytes of heap, which
// is the structure this set exists to keep off the heap.
const syntheticNodes = 200_000

// syntheticID is one node of the synthetic graph, spelled the way model.NodeID
// is: a 64-character hex identifier.
func syntheticID(i int) model.NodeID {
	return model.NodeID(fmt.Sprintf("%064x", i))
}

// spooledSet is a stand-in for the continuation spool: it replays every node an
// earlier page admitted, one at a time, holding none of them. The real stream
// is pagination.Spools.Open over a disk file (cursor.go), which has exactly
// this shape -- a forward-only replay of one record at a time.
func spooledSet(n int) visitedStream {
	return func(_ context.Context, fn func(model.NodeID) error) error {
		for i := 0; i < n; i++ {
			if err := fn(syntheticID(i)); err != nil {
				return err
			}
		}
		return nil
	}
}

// TestVisitedSetHeapIsBoundedByTheFrontNotTheWalk is the no-OOM invariant: a
// page resuming a walk that has already admitted 200 000 nodes answers
// membership for every one of them while holding only its own page's worth of
// them in heap.
//
// Before this set, the resume materialized the whole cumulative set into a
// map[NodeID]bool on EVERY page, so peak heap grew with the walk and a
// repository-sized traversal could not finish.
func TestVisitedSetHeapIsBoundedByTheFrontNotTheWalk(t *testing.T) {
	ctx := context.Background()
	set := newVisitedSet(spooledSet(syntheticNodes))

	var before runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)

	// A page's worth of work: levels of candidates probed against the spooled
	// set. Every candidate below IS in that set, so each level's answers are
	// correct only if the stream is consulted.
	const levels, perLevel = 40, 50
	hits := 0
	for l := 0; l < levels; l++ {
		candidates := make([]model.NodeID, 0, perLevel)
		for k := 0; k < perLevel; k++ {
			candidates = append(candidates, syntheticID((l*perLevel+k)*37%syntheticNodes))
		}
		if err := set.warm(ctx, candidates); err != nil {
			t.Fatalf("warm level %d: %v", l, err)
		}
		for _, id := range candidates {
			if set.has(id) {
				hits++
				continue
			}
			// Not yet admitted: this page admits it, and it must stay
			// answered from the front for the rest of the page.
			set.add(id)
		}
	}
	if hits != levels*perLevel {
		t.Fatalf("the spooled set answered %d of %d candidates; every one of them was admitted by an earlier page",
			hits, levels*perLevel)
	}

	var after runtime.MemStats
	runtime.GC()
	runtime.KeepAlive(set)
	runtime.ReadMemStats(&after)
	grew := int64(after.HeapAlloc) - int64(before.HeapAlloc)
	t.Logf("cumulative set = %d nodes; front after %d levels = %d carried + %d added + %d probed entries, %d bytes of heap",
		syntheticNodes, levels, len(set.carried), len(set.added), len(set.probed), grew)

	// A map of 200 000 64-hex ids costs upward of 20 MB. The front here holds
	// at most levels*perLevel ids plus one level of probe answers, so one
	// megabyte is a bound that a walk-sized structure cannot meet and a
	// page-sized one clears by an order of magnitude.
	const bound = 1 << 20
	if grew > bound {
		t.Fatalf("the visited set held %d bytes of heap for a %d-node walk; the front must be bounded by the page (bound %d bytes)",
			grew, syntheticNodes, bound)
	}
	// The front is what this page appends to the run store, and it must be
	// only this page's own admissions -- never the cumulative set, every
	// earlier page's run of which stays where that page wrote it.
	if got := len(set.addedNodes()); got > levels*perLevel {
		t.Fatalf("the page offered %d newly admitted nodes; it admitted at most %d", got, levels*perLevel)
	}
}

// convergentAdjacency is a chain whose every node also calls one shared sink,
// and the sink calls one terminal node. Walking it from the head reaches the
// sink on the FIRST page and re-encounters it on every page after that, which
// is the only shape that exercises the cross-page half of the visited set: the
// sink is not in the resuming page's front, so only a probe against the spooled
// cumulative set can say it was already admitted.
//
// If that probe answers wrong, the sink is re-admitted and re-expanded, and its
// edge to the terminal node is emitted once per page -- which is what the
// exactly-once assertion below catches.
type convergentAdjacency struct {
	binding model.Binding
	rels    []model.Relation
	nodes   map[model.NodeID]model.Node
}

// convergentChain is long enough that the walk needs several pages at the page
// size the test sets.
const convergentChain = 60

func newConvergentAdjacency() *convergentAdjacency {
	return newConvergentAdjacencyOfLength(convergentChain)
}

// newConvergentAdjacencyOfLength is the same shape at a chosen length, so a
// measurement that reads a per-page figure at a named page can make the walk
// long enough to reach it.
func newConvergentAdjacencyOfLength(links int) *convergentAdjacency {
	return newBackEdgeAdjacency(links, 0)
}

// newBackEdgeAdjacency is that shape WIDENED: every chain link also calls
// `width` leaves of its own, so a level is wide enough to spill the frontier
// ceiling and split ONE request into several internal legs, while the shared
// sink every link calls is still admitted at level 1 and expanded at level 2 --
// long off the frontier by the time the deeper levels reach it again. A width
// of zero is the plain convergent chain, which is what the constructor above
// asks for.
func newBackEdgeAdjacency(links, width int) *convergentAdjacency {
	a := &convergentAdjacency{
		binding: model.Binding{
			RepositoryID: model.RepositoryID(fixtureID("repo-1")),
			SnapshotID:   model.SnapshotID(fixtureID("snap-1")),
			GenerationID: 1,
			AnalysisKey:  model.AnalysisKey(fixtureID("akey-1")),
		},
		nodes: map[model.NodeID]model.Node{},
	}
	add := func(name string) model.NodeID {
		id := fixtureNodeID(name)
		a.nodes[id] = model.Node{ID: id, Kind: model.NodeFunction, Name: name, QualifiedName: name,
			Language: "go", SemanticSource: model.SemanticCanonical}
		return id
	}
	sink, terminal := add("c-sink"), add("c-terminal")
	edge := func(from, to model.NodeID) {
		a.rels = append(a.rels, model.Relation{ID: fixtureRelationID(len(a.rels) + 1),
			From: from, To: to, Kind: model.RelCalls})
	}
	prev := add("c-0000")
	for i := 1; i < links; i++ {
		next := add(fmt.Sprintf("c-%04d", i))
		edge(prev, next)
		edge(prev, sink)
		for j := 0; j < width; j++ {
			edge(prev, add(fmt.Sprintf("w-%04d-%03d", i-1, j)))
		}
		prev = next
	}
	edge(prev, sink)
	for j := 0; j < width; j++ {
		edge(prev, add(fmt.Sprintf("w-%04d-%03d", links-1, j)))
	}
	edge(sink, terminal)
	return a
}
func (a *convergentAdjacency) Edges(_ context.Context, nodes []model.NodeID, _ model.Direction,
	_ []model.RelationKind, after model.RelationID, limit int) ([]model.Relation, error) {
	want := map[model.NodeID]bool{}
	for _, n := range nodes {
		want[n] = true
	}
	var out []model.Relation
	for _, r := range a.rels {
		if r.ID <= after || !want[r.From] {
			continue
		}
		out = append(out, r)
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (a *convergentAdjacency) NodesByID(_ context.Context, ids []model.NodeID) ([]model.Node, error) {
	out := make([]model.Node, 0, len(ids))
	for _, id := range ids {
		if n, ok := a.nodes[id]; ok {
			out = append(out, n)
		}
	}
	return out, nil
}

func (a *convergentAdjacency) EvidenceFor(context.Context, []model.RelationID, int) (map[model.RelationID][]model.EvidenceID, error) {
	return map[model.RelationID][]model.EvidenceID{}, nil
}

func (a *convergentAdjacency) Capabilities(context.Context) ([]model.CapabilityState, error) {
	return nil, nil
}

func (a *convergentAdjacency) Binding() model.Binding { return a.binding }

// TestSpooledVisitedSetAnswersACrossPageRevisit proves the half of the set that
// lives on disk: a node admitted by page 1 is recognised as already admitted by
// a later page, which holds none of that page's state in heap.
func TestSpooledVisitedSetAnswersACrossPageRevisit(t *testing.T) {
	a := newConvergentAdjacency()
	engine := func(t *testing.T, pageItems int) *Engine {
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
		limits.MaxDepth, limits.MaxVisited, limits.MaxEdges = 0, 0, 0
		limits.MaxPageItems = pageItems
		e, err := New(Options{Adjacency: a, Signer: signer, Spools: spools,
			Leases: pagination.NewLeases(store, limits.CursorTTL), Limits: limits})
		if err != nil {
			t.Fatalf("new engine: %v", err)
		}
		return e
	}
	req := model.GraphRequest{GenerationID: 1, Start: []model.NodeID{fixtureNodeID("c-0000")},
		Direction: model.DirectionOutgoing, Relations: []model.RelationKind{model.RelCalls}}

	whole, err := engine(t, 2000).Neighbors(context.Background(), req)
	if err != nil {
		t.Fatalf("unlimited walk: %v", err)
	}
	if whole.Meta.Truncated {
		t.Fatalf("the unlimited walk must be complete, got %q", whole.Meta.TruncationReason)
	}

	paged := engine(t, 8)
	seen := map[model.RelationID]int{}
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
		for _, rel := range res.Relations {
			seen[rel.ID]++
		}
		if res.Meta.NextCursor == "" {
			break
		}
		req.Page = model.PageRequest{Cursor: res.Meta.NextCursor}
		req.GenerationID = 0
	}
	if pages < 3 {
		t.Fatalf("the walk took %d pages; a cross-page revisit needs the sink admitted on an earlier one", pages)
	}
	if len(seen) != len(whole.Relations) {
		t.Fatalf("the pages returned %d distinct edges, the unlimited walk %d", len(seen), len(whole.Relations))
	}
	for _, rel := range whole.Relations {
		if seen[rel.ID] != 1 {
			t.Errorf("edge %s was returned %d times across %d pages, want exactly once: the sink was re-admitted, so the spooled visited set did not answer a cross-page revisit",
				rel.ID, seen[rel.ID], pages)
		}
	}
}

// TestWarmStopsWhenEveryCandidateIsAnswered is the F29 proof, extended by the
// S4 fix. The membership sweep is one sequential pass over the cumulative
// spool per level, and levels per page are bounded only by the page item
// ceiling, so a sweep that always runs to the end of the spool costs
// O(levels x |visited|) per page. Two things now bound it: the sweep ends at
// the last candidate it was looking for, and a candidate the page's membership
// summary proves absent never starts a sweep at all.
//
// The second is the one that matters, and the second half of this test is the
// S4 proof: a CHAIN -- which is what a call chain is -- reaches freshly
// admitted nodes at every level, so no early exit can fire and every level
// used to read the whole spool. Each record the stream delivers is one
// json.Unmarshal in the real stream (cursor.go), so the count below is an
// honest proxy for spool record decodes.
//
// Mutations: deleting `return errWarmComplete` from warm's callback makes the
// first count the whole set; deleting the `v.filter.mayHold` guard makes the
// chain count levels x size (4 000 000 here), which is the regression.
func TestWarmStopsWhenEveryCandidateIsAnswered(t *testing.T) {
	const size = 1000
	read := 0
	spool := func(_ context.Context, fn func(model.NodeID) error) error {
		for i := 0; i < size; i++ {
			read++
			if err := fn(model.NodeID(fmt.Sprintf("n-%04d", i))); err != nil {
				return err
			}
		}
		return nil
	}
	v := newVisitedSet(spool)
	// One candidate, early in the spool: the sweep must stop at it.
	if err := v.warm(context.Background(), []model.NodeID{"n-0002"}); err != nil {
		t.Fatalf("warm: %v", err)
	}
	if !v.has("n-0002") {
		t.Fatal("warm did not answer a candidate the spool holds")
	}
	if read != 3 {
		t.Fatalf("the sweep read %d of %d records to answer one candidate at position 3; want 3", read, size)
	}
	// A candidate the spool does not hold is a full pass with no summary: the
	// pass IS the answer. This is the cost the filter below removes.
	read = 0
	if err := v.warm(context.Background(), []model.NodeID{"n-absent"}); err != nil {
		t.Fatalf("warm: %v", err)
	}
	if v.has("n-absent") || read != size {
		t.Fatalf("an unanswerable candidate read %d records and has=%v; want a full pass and false",
			read, v.has("n-absent"))
	}

	// The S4 case: a chain-shaped page over a 20 000-node cumulative set,
	// one page of 200 levels, one freshly reached node per level.
	const chainNodes, levels = 20_000, 200
	chainRead := 0
	chain := func(_ context.Context, fn func(model.NodeID) error) error {
		for i := 0; i < chainNodes; i++ {
			chainRead++
			if err := fn(syntheticID(i)); err != nil {
				return err
			}
		}
		return nil
	}
	// Sixteen bits per node of the cumulative set, the density the store's
	// frozen geometry is budgeted at (visitedstore.go).
	filter := newFrozenVisitedFilter(chainNodes*16, visitedFilterProbes)
	if filter == nil {
		t.Fatal("no membership summary was built for a 20 000-node walk")
	}
	for i := 0; i < chainNodes; i++ {
		filter.addWords(syntheticID(i), nil)
	}
	c := newVisitedSet(chain)
	c.filter = filter
	for l := 0; l < levels; l++ {
		// Fresh at every level: these ids are past the end of the spooled set,
		// exactly as a call chain's next hop is.
		fresh := syntheticID(chainNodes + l)
		if err := c.warm(context.Background(), []model.NodeID{fresh}); err != nil {
			t.Fatalf("warm level %d: %v", l, err)
		}
		if c.has(fresh) {
			t.Fatalf("level %d: a node no page admitted was reported as already admitted", l)
		}
		c.add(fresh)
	}
	// Before the summary: levels x chainNodes = 4 000 000 decodes for this one
	// page. The summary proves each candidate absent from heap, so the spool is
	// never opened. A handful of false positives would be correct but slower;
	// at sixteen bits per node they do not appear at this size.
	t.Logf("chain page: %d levels over a %d-node spooled set read %d records (was %d)",
		levels, chainNodes, chainRead, levels*chainNodes)
	if chainRead > levels {
		t.Fatalf("a chain-shaped page of %d levels read %d spool records over a %d-node set; the membership summary must answer a freshly reached node without a sweep",
			levels, chainRead, chainNodes)
	}
	// The summary may never turn a node the spool HOLDS into a miss: a false
	// negative would re-admit a node an earlier page already emitted.
	for i := 0; i < chainNodes; i += 997 {
		if !filter.mayHold(syntheticID(i)) {
			t.Fatalf("the membership summary lost node %d: a Bloom filter has no false negatives", i)
		}
	}
}

// TestWarmAnswersAStreamOfSeveralAscendingRuns is the Defect B proof. The
// cumulative set warm sweeps is a CONCATENATION of ascending runs, not one
// ascending sequence: every leg of the walk appends one run of its own
// admissions to the retained run store (visitedstore.go) and nothing merges
// them. A merge-join that assumed one global order advanced past a
// candidate answered by an EARLIER block and then stopped at the first block
// that ran past the largest candidate, reporting a node the walk had already
// admitted as absent -- re-admitting it on a later page and reporting the same
// entity twice.
//
// The blocks below are written by the production writer in the order the
// production walk writes them: a second link whose admissions sort below the
// first link's is ordinary (the walk admits whatever the graph reaches next),
// and it is what makes the stream non-monotonic.
//
// Mutation (`next = 0` on a descending key deleted, or `errWarmComplete`
// returned on `next >= len(sorted)` again): the first candidate below is
// reported absent, which is the assertion this test leads with.
func TestWarmAnswersAStreamOfSeveralAscendingRuns(t *testing.T) {
	store, err := openVisitedStore(t.TempDir(), 0, nil)
	if err != nil {
		t.Fatalf("open visited store: %v", err)
	}
	defer store.close()
	// Link 1 admitted the high ids, link 2 the low ones. Each run is
	// ascending; the stream is not.
	if _, err := store.appendRun([]model.NodeID{"n-0500", "n-0600"}); err != nil {
		t.Fatalf("append link 1: %v", err)
	}
	if _, err := store.appendRun([]model.NodeID{"n-0100", "n-0200"}); err != nil {
		t.Fatalf("append link 2: %v", err)
	}
	v := newVisitedSet(store.stream)
	// Both candidates were admitted; the sweep must answer both. n-0100 is the
	// one the single-order join loses: n-0500 answers the larger candidate and
	// leaves the join past it, and the pass used to end there.
	if err := v.warm(context.Background(), []model.NodeID{"n-0100", "n-0600"}); err != nil {
		t.Fatalf("warm: %v", err)
	}
	for _, id := range []model.NodeID{"n-0100", "n-0600"} {
		if !v.has(id) {
			t.Fatalf("node %s was admitted by the walk but the sweep reported it absent: it would be admitted again and reported twice", id)
		}
	}
	// A node no run holds is still absent: the run-aware join must not turn
	// a restart into a false positive.
	if err := v.warm(context.Background(), []model.NodeID{"n-0300"}); err != nil {
		t.Fatalf("warm: %v", err)
	}
	if v.has("n-0300") {
		t.Fatal("a node no run holds was reported as already admitted")
	}
}

// resumeSpooledWalk drives a real paged walk over the convergent fixture for
// `pages` pages and reopens the continuation the last one minted through the
// production resume, so a test can read exactly what the next page would.
func resumeSpooledWalk(t *testing.T, pages int) *resumeState {
	t.Helper()
	a := newConvergentAdjacency()
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
	limits.MaxPageItems = 8
	e, err := New(Options{Adjacency: a, Signer: signer, Spools: spools,
		Leases: pagination.NewLeases(store, limits.CursorTTL), Limits: limits})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	kinds := []model.RelationKind{model.RelCalls}
	start := []model.NodeID{fixtureNodeID("c-0000")}
	req := model.GraphRequest{GenerationID: 1, Start: start,
		Direction: model.DirectionOutgoing, Relations: kinds}
	cursor := ""
	for page := 1; page <= pages; page++ {
		res, err := e.Neighbors(context.Background(), req)
		if err != nil {
			t.Fatalf("page %d: %v", page, err)
		}
		if res.Meta.NextCursor == "" {
			t.Fatalf("page %d ended the walk; this proof needs a continuation over a spooled visited set", page)
		}
		cursor = res.Meta.NextCursor
		req = model.GraphRequest{Start: start, Direction: model.DirectionOutgoing,
			Relations: kinds, Page: model.PageRequest{Cursor: cursor}}
	}
	queryHash := traversalQueryHash(model.DirectionOutgoing, kinds, start, 0, limits.MaxPageItems)
	s, err := e.resumeTraversal(context.Background(), cursor, neighborsEndpoint, queryHash,
		time.Now().Add(time.Minute))
	if err != nil {
		t.Fatalf("resume the continuation: %v", err)
	}
	t.Cleanup(s.Release)
	return s
}

// TestTheRetainedVisitedRunsAreAscending is the A4 proof, carried over to the
// append-only store. The membership sweep is a MERGE-JOIN over the cumulative
// set (visited.go warm), and it reads that set for what it is: a CONCATENATION
// of ascending runs, one per page, where a key below its predecessor starts the
// next run and restarts the join at the smallest unanswered candidate. Both
// halves are correctness invariants, not tidiness ones. A run that is not
// ascending makes the join advance past a candidate it could have answered and
// report a node the walk HAS admitted as absent -- a cross-page RE-ADMISSION,
// the same entity listed twice. More runs than pages means the store took ids
// out of order, which costs the join a restart per stray key.
//
// Mutation (visitedstore.go appendRun given its ids reversed, or cursor.go
// handing it an unsorted slice): the run under the stray key is descending and
// the first assertion fails; the run count passes the second.
func TestTheRetainedVisitedRunsAreAscending(t *testing.T) {
	// Three pages, so the store under test holds several runs: each page
	// appended its OWN admissions and copied nothing forward.
	const pages = 3
	s := resumeSpooledWalk(t, pages)
	var prev model.NodeID
	runs, n := 0, 0
	if err := s.Visited(context.Background(), func(id model.NodeID) error {
		switch {
		case n == 0 || id < prev:
			runs++
		case id == prev:
			return fmt.Errorf("the retained runs repeat node %s: a run is a page's own "+
				"admissions, and a node two pages both admitted is the re-admission "+
				"the store exists to prevent", id)
		}
		prev, n = id, n+1
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if n < 2 {
		t.Fatalf("the runs held %d record(s); an order invariant needs at least two", n)
	}
	if runs > pages {
		t.Fatalf("the store holds %d ascending run(s) over %d page(s); a page appends ONE run, "+
			"so more of them means ids were appended out of order and the merge-join restarts "+
			"on every stray key", runs, pages)
	}
	t.Logf("%d retained record(s) in %d ascending run(s)", n, runs)
}

// TestResumePopulatesTheMembershipSummaryFromTheSpool is the A16 proof. The
// filter is the only thing standing between a chain-shaped page and levels x
// |visited| record decodes, and it is built in ONE place: the resume replay
// that already decodes every spool record to rebuild the frontier
// (resumeTraversal). Every other test in this file hands `c.filter` a filter it
// built by hand, so a one-sided edit -- the replay stops calling add, or starts
// summarising the frontier records instead of the visited section -- is a
// silent false negative that no assertion here would catch: the walk stays
// CORRECT (a filter miss only costs a sweep) and simply gets slow again.
//
// So this drives a real paged walk and then opens its continuation through the
// production resume, asserting that the summary describes exactly the stream
// the same resume hands the walk.
//
// Mutation (`s.Filter.add(r.Node)` deleted from resumeTraversal): every node the
// stream replays is reported absent by the summary, which is the assertion
// below.
func TestResumePopulatesTheMembershipSummaryFromTheSpool(t *testing.T) {
	// Two pages, so the cursor under test names a spool whose visited SECTION
	// is non-empty: page 1 spills the nodes it admitted, page 2 merges its own
	// into them. A summary over an empty stream would satisfy the loop below
	// vacuously, which is what the count guards.
	s := resumeSpooledWalk(t, 2)
	if s.Filter == nil {
		t.Fatal("the resume built no membership summary; every level of the next page would sweep the whole spool")
	}
	replayed := 0
	if err := s.Visited(context.Background(), func(id model.NodeID) error {
		replayed++
		if !s.Filter.mayHold(id) {
			return fmt.Errorf("the summary reports node %s absent, but the resume's own stream replays it: "+
				"a false negative makes a level skip the sweep that answers it, and the node is admitted twice", id)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if replayed == 0 {
		t.Fatal("the continuation replayed no visited nodes; the assertion above would be vacuous")
	}
	t.Logf("the resume summarised %d spooled visited nodes", replayed)
}
