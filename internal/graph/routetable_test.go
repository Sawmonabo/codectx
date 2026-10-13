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

// explainImpact reads the whole reasons view and routes view of an answer, a
// page at a time, keyed by entry ordinal: from the answer token when there is
// one, and otherwise -- an answer served inline, or an engine with no spool,
// retains none -- by asking req afresh for each view.
func explainImpact(t *testing.T, e *Engine, req model.ImpactRequest, answer string) (map[int64][]string,
	map[int64][]model.ImpactRoute) {
	t.Helper()
	reasons, routes := map[int64][]string{}, map[int64][]model.ImpactRoute{}
	for _, view := range []model.ImpactView{model.ImpactViewReasons, model.ImpactViewRoutes} {
		r := req
		r.View, r.Page.Cursor = view, answer
		if answer != "" {
			r.GenerationID = 0
		}
		for page := 0; ; page++ {
			if page > 10_000 {
				t.Fatalf("the %s view did not end in 10000 pages", view)
			}
			res, err := e.Impact(context.Background(), r)
			if err != nil {
				t.Fatalf("%s view page %d: %v", view, page, err)
			}
			// Only the view asked for: an answer served inline carries every
			// view in each response, and taking both here would count it twice.
			if view == model.ImpactViewReasons {
				for _, x := range res.Reasons {
					reasons[x.Entry] = append(reasons[x.Entry], x.Reason)
				}
			} else {
				for _, x := range res.Routes {
					routes[x.Entry] = append(routes[x.Entry], x)
				}
			}
			if res.Meta.NextCursor == "" {
				break
			}
			r.Page.Cursor = res.Meta.NextCursor
		}
	}
	return reasons, routes
}

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
	for i := range hops {
		f.relations = append(f.relations, model.Relation{ID: fixtureRelationID(i),
			From: chain[i], Kind: model.RelCalls, To: chain[i+1]})
	}
	return f, chain
}

// TestARouteSplitAcrossRequestsIsRebuiltExactly pins the route table across
// the two boundaries a route crosses: a deadline that splits the walk over
// several requests, so one route's admissions are written by different legs,
// and a routes view served over several pages, so a later page reads its
// routes from the table the answer names rather than from the walk. Every
// entry's served route must be exactly the chain prefix that reached it.
//
// Mutations caught: the table reopened by a later leg without counting the rows
// an earlier leg wrote (routeTable.open setting rows to 0), so a later
// admission takes an id that already names another row and its route is
// rebuilt through the wrong parents; the table not handed to the answer
// (adoptRoutes answering ""), so a routes page cannot read the routes of its
// records; and the answer token left at the continuation's offset
// (traversalCursor.answer not resetting RankOffset to RankStart), so the
// routes view does not start at the answer's first record.
func TestARouteSplitAcrossRequestsIsRebuiltExactly(t *testing.T) {
	const hops = 12
	f, chain := newChainFixture(hops)
	e, _ := chainEngine(t, f, 4, 2)
	req := model.ImpactRequest{GenerationID: 1, Start: chain[:1], Direction: model.DirectionOutgoing,
		Relations: []model.RelationKind{model.RelCalls}}
	walkPages, answer := 0, ""
	ordinals := map[int64]model.NodeID{}
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
		}
		if answer == "" {
			answer = res.Answer
		}
		for _, en := range res.Entries {
			ordinals[en.Ordinal] = en.NodeID
			if en.RouteCount != 1 {
				t.Fatalf("page %d: %s names %d routes, want its one", page, en.Name, en.RouteCount)
			}
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
	if walkPages < 2 {
		t.Fatalf("the walk took %d deadline-split requests; the case needs at least two", walkPages)
	}
	if len(ordinals) != hops {
		t.Fatalf("the answer served %d entities, want the %d nodes after the seed", len(ordinals), hops)
	}
	_, routes := explainImpact(t, e, req, answer)
	got := map[model.NodeID][]model.RelationID{}
	for ord, rs := range routes {
		if len(rs) != 1 {
			t.Fatalf("entry %d was served %d routes, want its one", ord, len(rs))
		}
		got[ordinals[ord]] = rs[0].Relations
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

// chainEngine is an engine over f with spools, a signer and leases, pages of
// limit items, and -- when every is positive -- a clock that jumps past the
// query deadline on every every-th adjacency read, so the walk splits across
// requests. It answers the engine and its spool root.
func chainEngine(t *testing.T, f *graphFixture, limit, every int) (*Engine, string) {
	t.Helper()
	signer, err := pagination.OpenSigner(t.TempDir())
	if err != nil {
		t.Fatalf("open signer: %v", err)
	}
	store := newFixtureLeases()
	root := t.TempDir()
	spools, err := pagination.NewSpools(root, 0, store)
	if err != nil {
		t.Fatalf("new spools: %v", err)
	}
	limits := fixtureLimits()
	limits.MaxDepth, limits.MaxVisited, limits.MaxEdges = 0, 0, 0
	limits.MaxPageItems = limit
	limits.QueryTimeout = time.Minute
	limits.CursorTTL = time.Hour
	clock := time.Now()
	var adj Adjacency = f
	var reader GraphReader = memGraphFor(f)
	if every > 0 {
		calls, fired := 0, false
		slow := slowAdjacency{graphFixture: f, clock: &clock, calls: &calls,
			jump: 2 * time.Minute, fired: &fired, every: every}
		adj, reader = slow, slow.reader(reader)
	}
	e, err := New(Options{Adjacency: adj, Reader: reader, Signer: signer, Spools: spools,
		Leases: pagination.NewLeases(store, limits.CursorTTL), Limits: limits,
		Now: func() time.Time { return clock }})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	return e, root
}
