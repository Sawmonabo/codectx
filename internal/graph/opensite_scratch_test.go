package graph

import (
	"context"
	"slices"
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

// TestImpactCountsOpenSitesTheSameAfterTheSetsSpill pins the open-site count
// across its two homes: the sets start in heap and move to a scratch database
// past openSiteHeapEntries, and the count per depth must not change. A site
// is counted once, at the first depth a node it names is reached -- siteH
// names a depth-1 handle and a depth-2 handle and counts at depth 1 alone.
//
// Mutations it catches, in the spilled run: not consulting the counted sites
// in scratch (siteH counted again at depth 2), losing the level's nodes when
// they move (depth 1 counts nothing), or reading only the first name of a
// level from scratch (depth 1 counts one), or reading a level's nodes back
// from scratch without a field a site's fit reads, such as the file (every
// depth counts nothing).
func TestImpactCountsOpenSitesTheSameAfterTheSetsSpill(t *testing.T) {
	f := newCallFixture("open_site_spill", fixtureScenario{
		Nodes: []fixtureDecl{
			{Name: "base", Kind: model.NodeFunction, File: "lib.py", Bytes: [2]uint64{0, 30}},
			{Name: "handle", Kind: model.NodeFunction, File: "lib.py", Bytes: [2]uint64{40, 70}},
			{Name: "other", Kind: model.NodeFunction, File: "lib.py", Bytes: [2]uint64{80, 110}},
			{Key: "handle2", Name: "handle", Kind: model.NodeFunction, File: "lib2.py", Bytes: [2]uint64{0, 30}},
			{Name: "top", Kind: model.NodeFunction, File: "lib3.py", Bytes: [2]uint64{0, 30}},
			{Name: "run", Kind: model.NodeFunction, File: "app.py", Bytes: [2]uint64{0, 90}},
			{Key: "siteH", Name: "handle", Kind: model.NodeFunction, File: "app.py", Bytes: [2]uint64{10, 20},
				Metadata: openSite(model.CallShapeBare, 0)},
			{Key: "siteO", Name: "other", Kind: model.NodeFunction, File: "app.py", Bytes: [2]uint64{30, 40},
				Metadata: openSite(model.CallShapeBare, 0)},
			{Key: "siteT", Name: "top", Kind: model.NodeFunction, File: "app.py", Bytes: [2]uint64{50, 60},
				Metadata: openSite(model.CallShapeBare, 0)},
		},
		Relations: []fixtureFact{
			{From: "handle", Kind: model.RelCalls, To: "base", Occurrences: site(50)},
			{From: "other", Kind: model.RelCalls, To: "base", Occurrences: site(90)},
			{From: "handle2", Kind: model.RelCalls, To: "handle", Occurrences: site(10)},
			{From: "top", Kind: model.RelCalls, To: "handle2", Occurrences: site(10)},
			{From: "run", Kind: model.RelCalls, To: "siteH", Occurrences: site(10)},
			{From: "run", Kind: model.RelCalls, To: "siteO", Occurrences: site(30)},
			{From: "run", Kind: model.RelCalls, To: "siteT", Occurrences: site(50)},
		},
	})
	want := []int64{2, 0, 1}
	for _, bound := range []int{openSiteHeapEntries, 1} {
		saved := openSiteHeapEntries
		openSiteHeapEntries = bound
		res, err := f.engine(t).Impact(context.Background(), model.ImpactRequest{
			Start: []model.NodeID{f.id(t, "base")}, Direction: model.DirectionIncoming})
		openSiteHeapEntries = saved
		if err != nil {
			t.Fatalf("impact(base), heap bound %d: %v", bound, err)
		}
		if !slices.Equal(res.OpenSitesByDepth, want) {
			t.Errorf("impact(base) with a heap bound of %d counts open sites %v, want %v", bound, res.OpenSitesByDepth, want)
		}
	}
}
