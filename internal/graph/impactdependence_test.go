package graph

import (
	"context"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
)

// dependenceFixture is a graph of the named nodes and the given relations,
// relation i carrying fixtureRelationID(i).
func dependenceFixture(nodes map[string]model.NodeKind, rels [][3]string) *graphFixture {
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
	for name, kind := range nodes {
		id := fixtureNodeID(name)
		f.nodes[id] = model.Node{ID: id, Kind: kind, Name: name, QualifiedName: name, Language: "go",
			SemanticSource: model.SemanticCanonical}
	}
	for i, r := range rels {
		f.relations = append(f.relations, model.Relation{ID: fixtureRelationID(i),
			From: fixtureNodeID(r[0]), Kind: model.RelationKind(r[1]), To: fixtureNodeID(r[2])})
	}
	return f
}

// incomingImpact is the incoming impact of seed over the default allowlist:
// the entities a change to seed may need modified.
func incomingImpact(t *testing.T, f *graphFixture, seed string) []model.ImpactEntry {
	t.Helper()
	e, _ := probedImpactEngine(t, f)
	res, err := e.Impact(context.Background(), model.ImpactRequest{GenerationID: 1,
		Start: []model.NodeID{fixtureNodeID(seed)}, Direction: model.DirectionIncoming})
	if err != nil {
		t.Fatalf("impact of %s: %v", seed, err)
	}
	if res.Meta.Truncated {
		t.Fatalf("the impact of %s is truncated (%q); every bound is unlimited here", seed, res.Meta.TruncationReason)
	}
	return res.Entries
}

func entryOf(entries []model.ImpactEntry, name string) (model.ImpactEntry, bool) {
	for _, e := range entries {
		if e.NodeID == fixtureNodeID(name) {
			return e, true
		}
	}
	return model.ImpactEntry{}, false
}

// TestImpactFollowsDataFlowForward: a value read through data flow is
// affected by a change to its source, and so is the function that defines
// it. `v data_flows_to w` is stored from the dependency, so the incoming half
// of the walk takes it out of v, reports w as incoming (may need
// modification), and goes on from w to `g writes w`.
//
// Mutations caught: data_flows_to left out of ImpactRelations (w and g are
// missing); the orientation rule dropped (orient never decides, so the
// incoming walk reads only v's incoming list and misses w); impactDirection
// without the flip (w reports outgoing).
func TestImpactFollowsDataFlowForward(t *testing.T) {
	f := dependenceFixture(map[string]model.NodeKind{
		"v": model.NodeVariable, "w": model.NodeVariable, "g": model.NodeFunction, "u": model.NodeVariable,
	}, [][3]string{
		{"v", string(model.RelDataFlowsTo), "w"},
		{"g", string(model.RelWrites), "w"},
		// u's value flows INTO v: v depends on u, so u is something a change
		// to v may need read, never modified, and an incoming walk does not
		// reach it.
		{"u", string(model.RelDataFlowsTo), "v"},
	})
	entries := incomingImpact(t, f, "v")
	w, ok := entryOf(entries, "w")
	if !ok {
		t.Fatalf("w, which v's value flows to, is not affected: %+v", entries)
	}
	if w.Direction != model.DirectionIncoming {
		t.Errorf("w reports %q; a value v flows to may need modification (incoming)", w.Direction)
	}
	if _, ok := entryOf(entries, "g"); !ok {
		t.Errorf("g, which defines w, is not affected: %+v", entries)
	}
	if _, ok := entryOf(entries, "u"); ok {
		t.Errorf("u, whose value flows into v, is reported affected by an incoming walk: %+v", entries)
	}
}

// TestImpactFollowsControlDependence: a statement whose execution a variable
// decides is affected by a change to that variable. `x control_depends_on c`
// is stored dependent first, so the incoming walk reaches x from c.
//
// Mutation caught: control_depends_on left out of ImpactRelations (x is
// missing).
func TestImpactFollowsControlDependence(t *testing.T) {
	f := dependenceFixture(map[string]model.NodeKind{"c": model.NodeVariable, "x": model.NodeVariable},
		[][3]string{{"x", string(model.RelControlDependsOn), "c"}})
	x, ok := entryOf(incomingImpact(t, f, "c"), "x")
	if !ok {
		t.Fatal("x, which control-depends on c, is not affected")
	}
	if x.Direction != model.DirectionIncoming {
		t.Errorf("x reports %q; a control-dependent statement may need modification (incoming)", x.Direction)
	}
}

// TestImpactRanksCallImpactAboveDependence is the ranking rule
// (dependenceHorizon): every node a call route reaches outranks a node only a
// program-dependence hop reaches, even when the call route is the deeper one.
// s's callers are three deep; d is one data-flow hop from s.
//
// Mutation caught: a program-dependence hop costing its traversal cost in
// impact (impactCost returning Cost): d, at cost 2, then ties c2's score and
// outranks it by depth.
func TestImpactRanksCallImpactAboveDependence(t *testing.T) {
	f := dependenceFixture(map[string]model.NodeKind{
		"s": model.NodeFunction, "c1": model.NodeFunction, "c2": model.NodeFunction, "c3": model.NodeFunction,
		"d": model.NodeVariable,
	}, [][3]string{
		{"c1", string(model.RelCalls), "s"},
		{"c2", string(model.RelCalls), "c1"},
		{"c3", string(model.RelCalls), "c2"},
		{"s", string(model.RelDataFlowsTo), "d"},
	})
	entries := incomingImpact(t, f, "s")
	var order []model.NodeID
	for _, e := range entries {
		order = append(order, e.NodeID)
	}
	want := []model.NodeID{fixtureNodeID("c1"), fixtureNodeID("c2"), fixtureNodeID("c3"), fixtureNodeID("d")}
	if len(order) != len(want) {
		t.Fatalf("impact of s lists %d entries, want %d: %+v", len(order), len(want), entries)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("entry %d is %s, want %s: the call chain must rank above the data-flow-only node (%+v)",
				i, order[i], want[i], entries)
		}
	}
}
