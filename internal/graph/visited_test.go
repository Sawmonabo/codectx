package graph

import (
	"context"
	"fmt"
	"runtime"
	"testing"

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
	// The front is what the next spill writes, and it must be only this page's
	// own admissions -- never the cumulative set, which is copied spool to
	// spool instead.
	if got := len(set.newlyAdmitted()); got > levels*perLevel {
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
	prev := add("c-00")
	for i := 1; i < convergentChain; i++ {
		next := add(fmt.Sprintf("c-%02d", i))
		edge(prev, next)
		edge(prev, sink)
		prev = next
	}
	edge(prev, sink)
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
	req := model.GraphRequest{GenerationID: 1, Start: []model.NodeID{fixtureNodeID("c-00")},
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

// TestWarmStopsWhenEveryCandidateIsAnswered is the F29 proof. The membership
// sweep is one sequential pass over the cumulative spool per level, and levels
// per page are bounded only by the page item ceiling, so a sweep that always
// runs to the end of the spool costs O(levels x |visited|) per page. It now
// ends at the last candidate it was looking for.
//
// Mutation (`return errWarmComplete` deleted from warm's callback): the read
// count below becomes the whole set.
func TestWarmStopsWhenEveryCandidateIsAnswered(t *testing.T) {
	const size = 1000
	read := 0
	v := newVisitedSet(func(ctx context.Context, fn func(model.NodeID) error) error {
		for i := 0; i < size; i++ {
			read++
			if err := fn(model.NodeID(fmt.Sprintf("n-%04d", i))); err != nil {
				return err
			}
		}
		return nil
	})
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
	// A candidate the spool does not hold is still a full pass: the pass IS
	// the answer, so this is the bound, not a regression.
	read = 0
	if err := v.warm(context.Background(), []model.NodeID{"n-absent"}); err != nil {
		t.Fatalf("warm: %v", err)
	}
	if v.has("n-absent") || read != size {
		t.Fatalf("an unanswerable candidate read %d records and has=%v; want a full pass and false",
			read, v.has("n-absent"))
	}
}
