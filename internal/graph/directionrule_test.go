package graph

import (
	"context"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
)

// TestBothDirectionWalkEmitsEveryRelationOnce is the direction dedup rule
// (ADR-0005, docs/queries.md), which is the whole of what keeps a
// `--direction both` walk from serving one relation twice now that no
// cumulative set of served relations exists.
//
// A walk that reads both directions meets every relation TWICE: once in the
// outgoing list of its source and once in the incoming list of its target. The
// rule decides which copy is the one that counts, and it has to decide the same
// way in the two shapes that differ:
//
//   - a pair on ONE level (a -> b, both admitted from the seed): neither end is
//     scanned before the other, so the SOURCE's outgoing copy is kept and the
//     target's incoming copy is dropped;
//   - a pair ACROSS levels (c -> a, with a one level above c): a's scan met the
//     relation as incoming while c was still unvisited and kept it there, so
//     c's own outgoing copy is dropped when c is scanned.
//
// Serving one of them twice inflates edge_count, lists the relation twice on a
// page, and -- because a page is a prefix of a level -- can put the same
// relation on two different pages of one answer. This asserts the exact set
// once, which is what separates "kept the right copy" from "kept both" and from
// "kept neither".
//
// Mutation proof (fails this test): drop the incoming half of the rule in
// keepEntry (`if !e.Outgoing { return false, nil }` deleted), so an incoming
// copy is kept whenever the outgoing one was kept at an earlier level.
func TestBothDirectionWalkEmitsEveryRelationOnce(t *testing.T) {
	f := &graphFixture{
		nodes:    map[model.NodeID]model.Node{},
		evidence: map[model.RelationID][]model.Evidence{},
		binding: model.Binding{
			RepositoryID: model.RepositoryID(fixtureID("repo-1")),
			SnapshotID:   model.SnapshotID(fixtureID("snap-1")),
			GenerationID: 1,
			AnalysisKey:  model.AnalysisKey(fixtureID("akey-1")),
		},
	}
	for _, name := range []string{"d-seed", "d-a", "d-b", "d-c"} {
		id := fixtureNodeID(name)
		f.nodes[id] = model.Node{ID: id, Kind: model.NodeFunction, Name: name,
			QualifiedName: name, Language: "go", SemanticSource: model.SemanticCanonical}
	}
	edge := func(i int, from, to string) model.Relation {
		return model.Relation{ID: fixtureRelationID(i), From: fixtureNodeID(from),
			To: fixtureNodeID(to), Kind: model.RelCalls}
	}
	f.relations = []model.Relation{
		edge(0, "d-seed", "d-a"),
		edge(1, "d-seed", "d-b"),
		// The same-level pair: d-a and d-b are both admitted from the seed.
		edge(2, "d-a", "d-b"),
		edge(3, "d-b", "d-c"),
		// The cross-level pair: d-c is admitted one level below d-a.
		edge(4, "d-c", "d-a"),
	}

	limits := fixtureLimits()
	limits.MaxDepth, limits.MaxVisited, limits.MaxEdges = 0, 0, 0
	e, err := New(Options{Adjacency: f, Reader: memGraphFor(f), Limits: limits})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	res, err := e.Neighbors(context.Background(), model.GraphRequest{GenerationID: 1,
		Start:     []model.NodeID{fixtureNodeID("d-seed")},
		Direction: model.DirectionBoth, Relations: []model.RelationKind{model.RelCalls}})
	if err != nil {
		t.Fatalf("Neighbors: %v", err)
	}
	if res.Meta.Truncated {
		t.Fatalf("the walk is truncated (%q); every bound is unlimited here", res.Meta.TruncationReason)
	}
	seen := map[model.RelationID]int{}
	for _, rel := range res.Relations {
		seen[rel.ID]++
	}
	for i := range f.relations {
		switch n := seen[fixtureRelationID(i)]; n {
		case 1:
		case 0:
			t.Errorf("relation %d was never served: the rule dropped both copies of it", i)
		default:
			t.Errorf("relation %d was served %d times: the rule kept more than one copy", i, n)
		}
	}
	if len(res.Relations) != len(f.relations) {
		t.Fatalf("the walk served %d edge(s) for a fixture of %d", len(res.Relations), len(f.relations))
	}
	// edge_count is the same fact the answer discloses, so a duplicate the set
	// test above cannot see -- one served on a later page of a longer walk --
	// would still show here.
	if res.EdgeCount != int64(len(f.relations)) {
		t.Errorf("edge_count is %d for %d relations", res.EdgeCount, len(f.relations))
	}
	// Every node of the fixture is admitted exactly once, seeds included.
	if res.VisitedCount != int64(len(f.nodes)) {
		t.Errorf("visited_count is %d for %d nodes", res.VisitedCount, len(f.nodes))
	}
}
