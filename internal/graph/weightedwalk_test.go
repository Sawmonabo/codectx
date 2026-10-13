package graph

import (
	"context"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
)

// TestAHeavierRootTwoHopsAwayLendsMoreThanALighterOneNextToIt protects the
// weighted walk's best-first order: a node keeps what a heavier root lends it
// over a longer route even when a lighter root reached it first.
//
// `light` (weight 0.3) calls `target` directly; `heavy` (weight 0.9) reaches
// it through `middle`. Under the rule below the one-hop route lends
// 0.3 x 0.9 = 270 000 and the two-hop route 0.9 x (0.9 x 0.9 x 0.8) = 583 200,
// so the label a ranking pass must be able to choose is the farther one.
//
// Mutation it catches: walk breadth-first and keep a node's first label (or
// settle labels by depth rather than by score). `target` is then reached at
// depth 1 from `light` before `heavy`'s route arrives, and only the 270 000
// label is served.
func TestAHeavierRootTwoHopsAwayLendsMoreThanALighterOneNextToIt(t *testing.T) {
	f := &graphFixture{nodes: map[model.NodeID]model.Node{}, evidence: map[model.RelationID][]model.Evidence{},
		binding: model.Binding{
			RepositoryID: model.RepositoryID(fixtureID("repo-1")),
			SnapshotID:   model.SnapshotID(fixtureID("snap-1")),
			GenerationID: 1,
			AnalysisKey:  model.AnalysisKey(fixtureID("akey-1")),
		}}
	for _, n := range []string{"light", "heavy", "middle", "target"} {
		id := fixtureNodeID(n)
		f.nodes[id] = model.Node{ID: id, Kind: model.NodeFunction, Name: n, QualifiedName: n,
			Language: "go", SemanticSource: model.SemanticCanonical}
	}
	for i, e := range [][2]string{{"light", "target"}, {"heavy", "middle"}, {"middle", "target"}} {
		f.relations = append(f.relations, model.Relation{ID: fixtureRelationID(i + 1),
			From: fixtureNodeID(e[0]), Kind: model.RelCalls, To: fixtureNodeID(e[1])})
	}
	limits := fixtureLimits()
	limits.MaxDepth, limits.MaxVisited, limits.MaxEdges = 0, 0, 0
	e, err := New(Options{Adjacency: f, Reader: memGraphFor(f), Limits: limits})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	const light, heavy = 300_000, 900_000
	res, err := e.WeightedWalk(context.Background(), WeightedWalkRequest{
		Roots: []WeightedRoot{
			{Node: fixtureNodeID("light"), Weight: light},
			{Node: fixtureNodeID("heavy"), Weight: heavy},
		},
		Relations: []model.RelationKind{model.RelCalls},
		Direction: model.DirectionBoth,
		Rule: LabelRule{Scale: 1_000_000, Hop: map[model.RelationKind]int64{model.RelCalls: 900_000},
			Decay: 800_000, Classes: 4, Policy: "test"},
	})
	if err != nil {
		t.Fatalf("weighted walk: %v", err)
	}
	if res.Meta.Truncated {
		t.Fatalf("the walk was cut (%s); the case needs it whole", res.Meta.TruncationReason)
	}
	var best *WeightedEntry
	for i := range res.Entries {
		en := &res.Entries[i]
		if en.NodeID != fixtureNodeID("target") {
			continue
		}
		if best == nil || en.ScoreMicros > best.ScoreMicros {
			best = en
		}
	}
	if best == nil {
		t.Fatal("target, reached from both roots, is not served")
	}
	if best.RootWeight != heavy || best.Depth != 2 || best.ScoreMicros != 583_200 {
		t.Fatalf("target's best label is weight %d at depth %d scoring %d; the heavier root's two-hop route "+
			"lends weight %d at depth 2 scoring 583200", best.RootWeight, best.Depth, best.ScoreMicros, heavy)
	}
	if best.Route == nil || len(best.Route.Relations) != 2 {
		t.Fatalf("the best label's route is %+v; it is the two hops heavy -> middle -> target", best.Route)
	}
}
