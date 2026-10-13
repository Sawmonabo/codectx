package graph

import (
	"context"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
)

// TestImpactExpandsEveryChunkOfEveryOpenSiteLevel pins the scratch-backed
// open-site expansion: the affected set's best records live in a scratch
// table read back a chunk at a time, and every node of every level must still
// be expanded, at the depth and on the route of its own best record. With a
// chunk of one node, each level here spans two chunks and the second level is
// reached only through the first.
//
// Mutations it catches: reading only the first chunk of a level (handleB's
// caller runB is missing), losing a parent's record across chunks (a wrong
// depth), or never reading the level the expansion itself wrote (mainX, two
// name-only hops out, is missing).
func TestImpactExpandsEveryChunkOfEveryOpenSiteLevel(t *testing.T) {
	saved := openSiteChunk
	openSiteChunk = 1
	t.Cleanup(func() { openSiteChunk = saved })
	f := newCallFixture("open_site_chunks", fixtureScenario{
		Nodes: []fixtureDecl{
			{Name: "base", Kind: model.NodeFunction, File: "lib.py", Bytes: [2]uint64{0, 30}},
			{Name: "handleA", Kind: model.NodeFunction, File: "lib.py", Bytes: [2]uint64{40, 70}},
			{Name: "handleB", Kind: model.NodeFunction, File: "lib.py", Bytes: [2]uint64{80, 110}},
			{Name: "runA", Kind: model.NodeFunction, File: "a.py", Bytes: [2]uint64{0, 40}},
			{Key: "siteA", Name: "handleA", Kind: model.NodeFunction, File: "a.py", Bytes: [2]uint64{20, 30},
				Metadata: openSite(model.CallShapeBare, 0)},
			{Name: "runB", Kind: model.NodeFunction, File: "b.py", Bytes: [2]uint64{0, 40}},
			{Key: "siteB", Name: "handleB", Kind: model.NodeFunction, File: "b.py", Bytes: [2]uint64{20, 30},
				Metadata: openSite(model.CallShapeBare, 0)},
			{Name: "mainX", Kind: model.NodeFunction, File: "x.py", Bytes: [2]uint64{0, 40}},
			{Key: "siteX", Name: "runA", Kind: model.NodeFunction, File: "x.py", Bytes: [2]uint64{20, 30},
				Metadata: openSite(model.CallShapeBare, 0)},
		},
		Relations: []fixtureFact{
			{From: "handleA", Kind: model.RelCalls, To: "base", Occurrences: site(50)},
			{From: "handleB", Kind: model.RelCalls, To: "base", Occurrences: site(90)},
			{From: "runA", Kind: model.RelCalls, To: "siteA", Occurrences: site(20)},
			{From: "runB", Kind: model.RelCalls, To: "siteB", Occurrences: site(20)},
			{From: "mainX", Kind: model.RelCalls, To: "siteX", Occurrences: site(20)},
		},
	})
	res, err := f.engine(t).Impact(context.Background(), model.ImpactRequest{
		Start: []model.NodeID{f.id(t, "base")}, Direction: model.DirectionIncoming,
		Relations: []model.RelationKind{model.RelCalls, model.RelMayCall}})
	if err != nil {
		t.Fatalf("impact(base): %v", err)
	}
	depth := map[model.NodeID]int{}
	for _, en := range res.Entries {
		depth[en.NodeID] = en.Depth
	}
	for name, want := range map[string]int{"handleA": 1, "handleB": 1, "runA": 2, "runB": 2, "mainX": 3} {
		if got, ok := depth[f.id(t, name)]; !ok || got != want {
			t.Errorf("%s is affected at depth %d (listed %v), want depth %d", name, got, ok, want)
		}
	}
}
