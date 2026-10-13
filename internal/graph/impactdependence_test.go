package graph

import (
	"context"
	"fmt"
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
// (dependenceTier): every node a call route reaches outranks a node only a
// program-dependence hop reaches, at every depth. s's callers form a chain
// 1 000 deep; d is one data-flow hop from s. A data-flow-only node also
// reports a lower score than every call-reached one, so the served score
// order and the rank agree.
//
// Mutations caught: impactAdd or kindCosts.add not setting the tier (d, at
// cost 2, ties c2's score and outranks it by depth); a fixed surcharge in
// place of the tier, of any size below the chain's cost (d outranks the deep
// end of the chain); scoreMicros ignoring the tier (d's score is above the
// chain's tail).
func TestImpactRanksCallImpactAboveDependence(t *testing.T) {
	const chain = 1000
	nodes := map[string]model.NodeKind{"s": model.NodeFunction, "d": model.NodeVariable}
	rels := [][3]string{{"s", string(model.RelDataFlowsTo), "d"}}
	name := func(i int) string { return fmt.Sprintf("c%d", i) }
	prev := "s"
	for i := 1; i <= chain; i++ {
		nodes[name(i)] = model.NodeFunction
		rels = append(rels, [3]string{name(i), string(model.RelCalls), prev})
		prev = name(i)
	}
	entries := incomingImpact(t, dependenceFixture(nodes, rels), "s")
	if len(entries) != chain+1 {
		t.Fatalf("impact of s lists %d entries, want %d", len(entries), chain+1)
	}
	for i := 0; i < chain; i++ {
		if entries[i].NodeID != fixtureNodeID(name(i+1)) {
			t.Fatalf("entry %d is %s, want %s: the call chain must rank above the data-flow-only node",
				i, entries[i].NodeID, name(i+1))
		}
	}
	last, tail := entries[chain], entries[chain-1]
	if last.NodeID != fixtureNodeID("d") {
		t.Fatalf("the last entry is %s, want the data-flow-only node d", last.NodeID)
	}
	if last.ScoreMicros >= tail.ScoreMicros {
		t.Errorf("d scores %d, not below the chain's deepest caller (%d): the served score contradicts the rank",
			last.ScoreMicros, tail.ScoreMicros)
	}
}

// dispatchDependenceFixture is a call scenario whose reader reports dispatch,
// so a one-direction impact over it takes the dispatch step beside the
// dependence rules. extra relations are appended to the scenario's own.
func dispatchDependenceFixture(extra ...fixtureFact) *callFixture {
	decl := func(name string, kind model.NodeKind, at uint64) fixtureDecl {
		return fixtureDecl{Name: name, Kind: kind, File: "zoo.py", Bytes: [2]uint64{at, at + 5}}
	}
	call := func(from, to string, d model.Dispatch, at uint64) fixtureFact {
		return fixtureFact{From: from, Kind: model.RelCalls, To: to,
			Occurrences: []fixtureOccurrence{{Bytes: [2]uint64{at, at + 3}, Dispatch: d}}}
	}
	sc := fixtureScenario{
		Nodes: []fixtureDecl{
			decl("Animal.sound", model.NodeMethod, 0), decl("Dog.sound", model.NodeMethod, 10),
			decl("speak", model.NodeFunction, 20), decl("r", model.NodeFunction, 30),
			decl("m", model.NodeFunction, 40), decl("h", model.NodeVariable, 50),
			decl("src", model.NodeVariable, 60),
		},
		Relations: append([]fixtureFact{
			{From: "Dog.sound", Kind: model.RelOverrides, To: "Animal.sound",
				Occurrences: []fixtureOccurrence{{Bytes: [2]uint64{10, 15}}}},
			call("speak", "Animal.sound", model.DispatchVirtual, 21),
			call("r", "Animal.sound", model.DispatchExact, 31),
			call("r", "m", model.DispatchExact, 34),
			call("m", "Animal.sound", model.DispatchVirtual, 41),
		}, extra...),
	}
	return newCallFixture("dependence_dispatch", sc)
}

// TestImpactDependenceSkipsTheBaseOfACallersClimb: an incoming impact of an
// override climbs to its base only to list the base's virtual callers; the
// base is a route, not an affected entity, so what the base's value flows to
// is not affected either.
//
// Mutation caught: orient taking a reversed kind's entry from a stepBase
// owner (h, which Animal.sound's value flows to, is listed).
func TestImpactDependenceSkipsTheBaseOfACallersClimb(t *testing.T) {
	f := dispatchDependenceFixture(fixtureFact{From: "Animal.sound", Kind: model.RelDataFlowsTo, To: "h",
		Occurrences: []fixtureOccurrence{{Bytes: [2]uint64{2, 4}}}})
	res, err := f.engine(t).Impact(context.Background(), model.ImpactRequest{
		Start: []model.NodeID{f.id(t, "Dog.sound")}, Direction: model.DirectionIncoming})
	if err != nil {
		t.Fatalf("impact(Dog.sound): %v", err)
	}
	got := map[string]bool{}
	for _, en := range res.Entries {
		got[f.name(en.NodeID)] = true
	}
	if !got["speak"] {
		t.Fatalf("impact(Dog.sound) = %v: speak, a virtual caller through the base, is missing, so the climb did not run", got)
	}
	if got["h"] || got["Animal.sound"] {
		t.Errorf("impact(Dog.sound) = %v: the base and what its value flows to are routes, not affected", got)
	}
}

// TestImpactDependenceServesADescendingNodesEdgesOnce: an outgoing impact
// admits Animal.sound first as an exact call's target and again, through m's
// virtual call, only to descend to its overrides. The one data flow into it
// is walked at the first admission; the second must not walk it again, so
// adding that one relation adds exactly one admitted edge.
//
// Mutation caught: orient taking a reversed kind's entry from a stepDescend
// owner (the flow is served twice and the edge count grows by two).
func TestImpactDependenceServesADescendingNodesEdgesOnce(t *testing.T) {
	edges := func(f *callFixture) int64 {
		t.Helper()
		res, err := f.engine(t).Impact(context.Background(), model.ImpactRequest{
			Start: []model.NodeID{f.id(t, "r")}, Direction: model.DirectionOutgoing})
		if err != nil {
			t.Fatalf("impact(r): %v", err)
		}
		descended := false
		for _, en := range res.Entries {
			descended = descended || f.name(en.NodeID) == "Dog.sound"
		}
		if !descended {
			t.Fatalf("impact(r) never descended to Dog.sound, so the re-admission this test is about did not happen")
		}
		return res.EdgeCount
	}
	without := edges(dispatchDependenceFixture())
	with := edges(dispatchDependenceFixture(fixtureFact{From: "src", Kind: model.RelDataFlowsTo, To: "Animal.sound",
		Occurrences: []fixtureOccurrence{{Bytes: [2]uint64{61, 63}}}}))
	if with-without != 1 {
		t.Errorf("one data flow into Animal.sound added %d admitted edges, want 1", with-without)
	}
}
