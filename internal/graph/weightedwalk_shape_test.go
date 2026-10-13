package graph

import (
	"context"
	"fmt"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
)

// shapeWalk runs an unbounded weighted walk over reader r of fixture f from
// the named roots, every root of weight Scale, across kinds in both
// directions, and answers every served entry by node name.
func shapeWalk(t *testing.T, f *graphFixture, r GraphReader, kinds []model.RelationKind,
	peer map[model.RelationKind]bool, roots ...string) map[string][]WeightedEntry {
	t.Helper()
	limits := fixtureLimits()
	limits.MaxDepth, limits.MaxVisited, limits.MaxEdges = 0, 0, 0
	e, err := New(Options{Adjacency: f, Reader: r, Limits: limits})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	hop := map[model.RelationKind]int64{}
	for _, k := range kinds {
		hop[k] = 900_000
	}
	hop[model.RelExtends] = 940_000
	var start []WeightedRoot
	for _, n := range roots {
		start = append(start, WeightedRoot{Node: fixtureNodeID(n), Weight: 1_000_000, Named: true})
	}
	res, err := e.WeightedWalk(context.Background(), WeightedWalkRequest{Roots: start, Relations: kinds,
		Direction: model.DirectionBoth,
		Rule:      LabelRule{Scale: 1_000_000, Hop: hop, Decay: 800_000, Classes: 4, Peer: peer, Policy: "test"}})
	if err != nil {
		t.Fatalf("weighted walk: %v", err)
	}
	if res.Meta.Truncated || res.Meta.NextCursor != "" {
		t.Fatalf("the walk was cut or paged (%q); the case needs it whole on one page", res.Meta.TruncationReason)
	}
	out := map[string][]WeightedEntry{}
	for _, en := range res.Entries {
		for id, n := range f.nodes {
			if id == en.NodeID {
				out[n.Name] = append(out[n.Name], en)
			}
		}
	}
	return out
}

// TestAWeightedWalkMarksASiblingReachedUpAndBackDown: Dog extends Animal and
// Cat extends Animal, and Dog also calls Animal. From Dog, Cat is reached up
// `extends` and straight back down it (a sibling, Peer set), and through the
// call to Animal and then down `extends` (a subtype of what Dog uses, Peer
// clear). Both labels are served: the extends route scores higher, but its
// owed turn makes it no stand-in for the call route.
//
// Mutations caught: the turn not recorded (no label of Cat carries Peer); the
// owed turn left out of dominance, so Animal's `extends` label drops the
// call label and Cat has no label without Peer.
func TestAWeightedWalkMarksASiblingReachedUpAndBackDown(t *testing.T) {
	f := dependenceFixture(map[string]model.NodeKind{"Dog": model.NodeClass, "Animal": model.NodeClass,
		"Cat": model.NodeClass}, [][3]string{
		{"Dog", string(model.RelExtends), "Animal"},
		{"Cat", string(model.RelExtends), "Animal"},
		{"Dog", string(model.RelCalls), "Animal"},
	})
	got := shapeWalk(t, f, memGraphFor(f), []model.RelationKind{model.RelCalls, model.RelExtends},
		map[model.RelationKind]bool{model.RelExtends: true}, "Dog")
	var peer, plain bool
	for _, en := range got["Cat"] {
		peer = peer || en.Peer
		plain = plain || !en.Peer
	}
	if !peer || !plain {
		t.Fatalf("Cat's labels %+v: want one reached up and down extends (Peer) and one through the call (not Peer)",
			got["Cat"])
	}
	for _, en := range got["Animal"] {
		if en.Peer {
			t.Errorf("Animal, the supertype itself, is served as a sibling: %+v", en)
		}
	}
}

// TestAWeightedWalkNeverLeavesACalleeThroughAnotherSite is the two-phase
// slice in the weighted walk, over the impact walk's call-context fixture: a
// route from a1 enters f through p and reaches f's return node, and never
// leaves through site 2's return flow, so neither r2 nor a2 (site 2's
// argument, read backwards from p) is reached. Seeded at x as well, m is
// reached by a route outside any call, which leaves through both sites.
//
// Mutations caught: the descended flag ignored in relax (r2 is reached from
// a1); the descended flag left out of dominance, so a1's shallower descended
// label at m stands for x's and r2 is missing when x is a root too.
func TestAWeightedWalkNeverLeavesACalleeThroughAnotherSite(t *testing.T) {
	f, g := callContextFixture(t)
	kinds := []model.RelationKind{model.RelDataFlowsTo}
	got := shapeWalk(t, f, g, kinds, nil, "a1")
	for _, n := range []string{"p", "m", "ret", "r1"} {
		if len(got[n]) == 0 {
			t.Errorf("the walk from a1 does not reach %s; served %v", n, got)
		}
	}
	for _, n := range []string{"r2", "a2"} {
		if len(got[n]) > 0 {
			t.Errorf("the walk from a1 reaches %s, another call site's side: an unrealizable path", n)
		}
	}
	both := shapeWalk(t, f, g, kinds, nil, "a1", "x")
	if len(both["r2"]) == 0 {
		t.Errorf("the walk from a1 and x does not reach r2; x reaches f's return outside any call")
	}
}

// TestAWeightedWalkServesARouteHoweverLong: a call chain longer than
// model.MaxRelationsPerPath is walked to its end, and the last node is served
// with its whole route, every relation in order from the root.
//
// Mutation caught: route() serving no route past model.MaxRelationsPerPath
// (the end of the chain then has no path).
func TestAWeightedWalkServesARouteHoweverLong(t *testing.T) {
	const chain = model.MaxRelationsPerPath + 6
	nodes := map[string]model.NodeKind{}
	var rels [][3]string
	name := func(i int) string { return fmt.Sprintf("c%03d", i) }
	for i := 0; i <= chain; i++ {
		nodes[name(i)] = model.NodeFunction
		if i > 0 {
			rels = append(rels, [3]string{name(i - 1), string(model.RelCalls), name(i)})
		}
	}
	f := dependenceFixture(nodes, rels)
	limits := fixtureLimits()
	limits.MaxDepth, limits.MaxVisited, limits.MaxEdges = 0, 0, 0
	e, err := New(Options{Adjacency: f, Reader: memGraphFor(f), Limits: limits})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	res, err := e.WeightedWalk(context.Background(), WeightedWalkRequest{
		Roots:     []WeightedRoot{{Node: fixtureNodeID(name(0)), Weight: 1_000_000, Named: true}},
		Relations: []model.RelationKind{model.RelCalls}, Direction: model.DirectionOutgoing,
		Rule: LabelRule{Scale: 1_000_000, Hop: map[model.RelationKind]int64{model.RelCalls: 1_000_000},
			Decay: 1_000_000, Classes: 4, Policy: "test"}})
	if err != nil {
		t.Fatalf("weighted walk: %v", err)
	}
	for _, en := range res.Entries {
		if en.NodeID != fixtureNodeID(name(chain)) {
			continue
		}
		if len(en.Paths) != 1 || len(en.Paths[0].Relations) != chain {
			t.Fatalf("the end of a %d-call chain is served with paths %+v, want its whole route", chain, en.Paths)
		}
		for i, id := range en.Paths[0].Relations {
			if want := fixtureRelationID(i); id != want {
				t.Fatalf("hop %d of the route is %s, want %s", i, id, want)
			}
		}
		return
	}
	t.Fatalf("the end of a %d-call chain was not reached (%d entries, truncated %v)", chain, len(res.Entries),
		res.Meta.Truncated)
}
