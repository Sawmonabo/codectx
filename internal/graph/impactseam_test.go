package graph

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
)

// TestAnImpactOnAReaderWithoutTheDeclarationLookupSaysWhatItCannotAnswer pins
// both answers an impact request gets from a reader that cannot look
// declarations up by name. A request naming may_call is refused with the
// candidates answer before any walk runs (the probe sees no edge), not after
// the precise walk was paid for. A request that does not name it is answered,
// with the notice that its open-site counts are absent rather than zero.
//
// Mutations caught: openSiteSeam not refusing may_call (the refusal then comes
// from expandOpenSites after the walk, and the probe sees its edges); the
// notice left off (the answer's missing open_sites_by_depth reads as no open
// sites).
func TestAnImpactOnAReaderWithoutTheDeclarationLookupSaysWhatItCannotAnswer(t *testing.T) {
	f := newCallFixture("impact_seam", fixtureScenario{
		Nodes: []fixtureDecl{
			{Name: "base", Kind: model.NodeFunction, File: "lib.py", Bytes: [2]uint64{0, 30}},
			{Name: "handle", Kind: model.NodeFunction, File: "lib.py", Bytes: [2]uint64{40, 70}},
			{Name: "run", Kind: model.NodeFunction, File: "a.py", Bytes: [2]uint64{0, 40}},
			{Key: "site", Name: "handle", Kind: model.NodeFunction, File: "a.py", Bytes: [2]uint64{20, 30},
				Metadata: openSite(model.CallShapeBare, 0)},
		},
		Relations: []fixtureFact{
			{From: "handle", Kind: model.RelCalls, To: "base", Occurrences: site(50)},
			{From: "run", Kind: model.RelCalls, To: "site", Occurrences: site(20)},
		},
	})
	e, err := New(Options{Adjacency: f, Reader: undeclaredReader{f.reader()}, Limits: fixtureLimits()})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	ctx := context.Background()
	start := []model.NodeID{f.id(t, "base")}

	e.probe = &heapProbe{}
	res, err := e.Impact(ctx, model.ImpactRequest{Start: start, Direction: model.DirectionIncoming,
		Relations: []model.RelationKind{model.RelCalls}})
	if err != nil {
		t.Fatalf("a precise impact on a reader without the lookup: %v", err)
	}
	if e.probe.Rollup.PeakLiveEdges == 0 {
		t.Fatal("the precise walk's edges never reached the probe; the refusal check below proves nothing")
	}
	if len(res.OpenSitesByDepth) != 0 || !slices.Contains(res.Meta.Notices, openSitesUncounted) {
		t.Fatalf("the precise impact answered open sites %v with notices %q, want none counted and the notice saying so",
			res.OpenSitesByDepth, res.Meta.Notices)
	}

	e.probe = &heapProbe{}
	_, err = e.Impact(ctx, model.ImpactRequest{Start: start, Direction: model.DirectionIncoming,
		Relations: []model.RelationKind{model.RelCalls, model.RelMayCall}})
	var got *model.Error
	if !errors.As(err, &got) || got.Code != model.CodeProviderUnavailable {
		t.Fatalf("a may_call impact on a reader without the lookup = %v, want %s", err, model.CodeProviderUnavailable)
	}
	if e.probe.Rollup.PeakLiveEdges != 0 {
		t.Fatalf("the may_call request was refused after a walk that held %d edges, want it refused before any walk",
			e.probe.Rollup.PeakLiveEdges)
	}
}
