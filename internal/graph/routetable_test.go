package graph

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
)

// newChainFixture is a seed and hops nodes after it, each calling the next, so
// the node at depth d is reached by exactly one route: the first d relations.
func newChainFixture(hops int) (*graphFixture, []model.NodeID) {
	f := &graphFixture{
		nodes:    make(map[model.NodeID]model.Node, hops+1),
		evidence: map[model.RelationID][]model.Evidence{},
		binding: model.Binding{
			RepositoryID: model.RepositoryID(fixtureID("repo-1")),
			SnapshotID:   model.SnapshotID(fixtureID("snap-1")),
			GenerationID: 1,
			AnalysisKey:  model.AnalysisKey(fixtureID("akey-1")),
		},
	}
	chain := make([]model.NodeID, 0, hops+1)
	for i := 0; i <= hops; i++ {
		name := fmt.Sprintf("chain-%06d", i)
		id := fixtureNodeID(name)
		f.nodes[id] = model.Node{ID: id, Kind: model.NodeFunction, Name: name, QualifiedName: name,
			Language: "go", SemanticSource: model.SemanticCanonical}
		chain = append(chain, id)
	}
	for i := 0; i < hops; i++ {
		f.relations = append(f.relations, model.Relation{ID: fixtureRelationID(i),
			From: chain[i], Kind: model.RelCalls, To: chain[i+1]})
	}
	return f, chain
}

// TestARouteSplitAcrossRequestsIsRebuiltExactly pins the route table across
// the two boundaries a route crosses: a deadline that splits the walk over
// several requests, so one route's admissions are written by different legs,
// and a ranked answer served over several pages, so a later page reads its
// routes from the table the continuation names rather than from the walk.
// Every entry's served route must be exactly the chain prefix that reached it.
//
// Mutations caught: the table reopened by a later leg without counting the rows
// an earlier leg wrote (routeTable.open setting rows to 0), so a later
// admission takes an id that already names another row and its route is
// rebuilt through the wrong parents; and the table not handed to the ranked
// continuation (adoptRoutes answering ""), so the second page cannot read the
// routes of its records.
func TestARouteSplitAcrossRequestsIsRebuiltExactly(t *testing.T) {
	const hops = 12
	f, chain := newChainFixture(hops)
	signer, err := pagination.OpenSigner(t.TempDir())
	if err != nil {
		t.Fatalf("open signer: %v", err)
	}
	store := newFixtureLeases()
	spools, err := pagination.NewSpools(t.TempDir(), 0, store)
	if err != nil {
		t.Fatalf("new spools: %v", err)
	}
	limits := fixtureLimits()
	limits.MaxDepth, limits.MaxVisited, limits.MaxEdges = 0, 0, 0
	limits.MaxPageItems = 4
	limits.QueryTimeout = time.Minute
	limits.CursorTTL = time.Hour

	clock := time.Now()
	calls, fired := 0, false
	slow := slowAdjacency{graphFixture: f, clock: &clock, calls: &calls,
		jump: 2 * time.Minute, fired: &fired, every: 2}
	e, err := New(Options{Adjacency: slow, Reader: slow.reader(memGraphFor(f)), Signer: signer, Spools: spools,
		Leases: pagination.NewLeases(store, limits.CursorTTL), Limits: limits,
		Now: func() time.Time { return clock }})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	req := model.ImpactRequest{GenerationID: 1, Start: chain[:1], Direction: model.DirectionOutgoing,
		Relations: []model.RelationKind{model.RelCalls}}
	walkPages, rankedPages := 0, 0
	got := map[model.NodeID][]model.RelationID{}
	for page := 1; ; page++ {
		if page > 200 {
			t.Fatal("the answer did not end in 200 pages")
		}
		res, err := e.Impact(context.Background(), req)
		if err != nil {
			t.Fatalf("page %d: %v", page, err)
		}
		if len(res.Entries) == 0 {
			walkPages++
		} else {
			rankedPages++
		}
		for _, en := range res.Entries {
			if len(en.Paths) != 1 {
				t.Fatalf("page %d: %s carries %d routes, want its one", page, en.Name, len(en.Paths))
			}
			got[en.NodeID] = en.Paths[0].Relations
		}
		if res.Meta.NextCursor == "" {
			if res.Meta.Truncated {
				t.Fatalf("the answer ended truncated: %q", res.Meta.TruncationReason)
			}
			break
		}
		req = model.ImpactRequest{Start: chain[:1], Direction: model.DirectionOutgoing,
			Relations: []model.RelationKind{model.RelCalls},
			Page:      model.PageRequest{Cursor: res.Meta.NextCursor}}
	}
	if walkPages < 2 || rankedPages < 2 {
		t.Fatalf("the walk took %d deadline-split requests and %d ranked pages; the case needs at least two of each",
			walkPages, rankedPages)
	}
	if len(got) != hops {
		t.Fatalf("the answer served %d entities, want the %d nodes after the seed", len(got), hops)
	}
	for d := 1; d <= hops; d++ {
		want := make([]model.RelationID, 0, d)
		for i := 0; i < d; i++ {
			want = append(want, fixtureRelationID(i))
		}
		if !slices.Equal(got[chain[d]], want) {
			t.Fatalf("the node at depth %d was served the route %v, want %v", d, got[chain[d]], want)
		}
	}
}
