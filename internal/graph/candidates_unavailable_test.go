package graph

import (
	"context"
	"errors"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
)

// undeclaredReader is a packed reader that does not look declarations up by
// name: embedding the interface alone hides every optional seam, the
// declaration lookup among them.
type undeclaredReader struct{ GraphReader }

// TestAReaderWithoutTheDeclarationLookupAnswersCandidatesUnavailable pins that
// both answers built on name-only candidates say the same thing when the reader
// cannot list them: a direct candidate lookup and an impact walk that names
// may_call are both refused as unavailable, with the same code and message,
// rather than one of them reading as "no possible callers". A walk that does
// not name may_call needs no candidates and still answers on the same reader.
//
// Mutation caught: expandOpenSites returning nil for a reader without the seam
// (the impact answer then lists the precise caller alone and reads as
// complete).
func TestAReaderWithoutTheDeclarationLookupAnswersCandidatesUnavailable(t *testing.T) {
	f := newCallFixture("candidates_unavailable", fixtureScenario{
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
	reader := undeclaredReader{f.reader()}
	if declarationReader(reader) != nil {
		t.Fatal("the reader still offers the declaration lookup; the test proves nothing")
	}
	e, err := New(Options{Adjacency: f, Reader: reader, Limits: fixtureLimits()})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	ctx := context.Background()

	_, candErr := Candidates(ctx, reader, model.CandidateRequest{Name: "handle", Shape: model.CallShapeBare, Language: "python",
		Page: model.PageRequest{Limit: model.MaxPageItems}})
	var want *model.Error
	if !errors.As(candErr, &want) || want.Code != model.CodeProviderUnavailable {
		t.Fatalf("Candidates on a reader without the lookup = %v, want %s", candErr, model.CodeProviderUnavailable)
	}

	start := []model.NodeID{f.id(t, "base")}
	_, impactErr := e.Impact(ctx, model.ImpactRequest{Start: start, Direction: model.DirectionIncoming,
		Relations: []model.RelationKind{model.RelCalls, model.RelMayCall}})
	var got *model.Error
	if !errors.As(impactErr, &got) || got.Code != want.Code || got.Message != want.Message {
		t.Fatalf("a may_call impact on a reader without the lookup = %v, want the candidates answer %v",
			impactErr, candErr)
	}

	res, err := e.Impact(ctx, model.ImpactRequest{Start: start, Direction: model.DirectionIncoming,
		Relations: []model.RelationKind{model.RelCalls}})
	if err != nil {
		t.Fatalf("a precise impact on a reader without the lookup: %v", err)
	}
	if len(res.Entries) != 1 || res.Entries[0].NodeID != f.id(t, "handle") {
		t.Fatalf("the precise impact answered %+v, want the one caller handle", res.Entries)
	}
}
