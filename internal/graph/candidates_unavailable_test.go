package graph

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
)

// undeclaredReader is a packed reader that does not look declarations up by
// name: embedding the interface alone hides every optional seam, the
// declaration lookup among them.
type undeclaredReader struct{ GraphReader }

// openSiteFixture is `handle` calling `base`, and `run` holding one open call
// site that names `handle` by name: with the declaration lookup, `run` is a
// name-only (`may_call`) caller of `handle`; precisely, `handle` has none.
func openSiteFixture(scenario string) *callFixture {
	return newCallFixture(scenario, fixtureScenario{
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
}

// undeclaredEngine is the fixture's engine over a reader without the
// declaration lookup.
func undeclaredEngine(t *testing.T, f *callFixture) (*Engine, GraphReader) {
	t.Helper()
	reader := undeclaredReader{f.reader()}
	if declarationReader(reader) != nil {
		t.Fatal("the reader still offers the declaration lookup; the test proves nothing")
	}
	e, err := New(Options{Adjacency: f, Reader: reader, Limits: fixtureLimits()})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	return e, reader
}

// disclosesUnavailable reports whether meta carries the name-only
// disclosure for kind: the unavailable completeness row and its notice.
func disclosesUnavailable(meta model.QueryMeta, kind model.RelationKind) bool {
	row := false
	for _, c := range meta.Completeness {
		if c.Capability == string(kind) && c.State == model.CapabilityUnavailable &&
			c.DiagnosticCode == model.CodeProviderUnavailable {
			row = true
		}
	}
	notice := false
	for _, n := range meta.Notices {
		if strings.Contains(n, "`"+string(kind)+"` rows are unavailable") {
			notice = true
		}
	}
	return row && notice
}

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
	f := openSiteFixture("candidates_unavailable")
	e, reader := undeclaredEngine(t, f)
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

// TestACallWalkWithoutTheLookupDisclosesOrRefusesItsNameOnlyRows pins the call
// walk's two answers on a reader that cannot list candidates. A callers walk
// over `calls` lists name-only callers after the precise ones by design, so it
// is answered and says those rows are unavailable; a walk that names
// `may_call` asked for them, so it is refused before walking, with the
// candidate lookup's own error.
//
// Mutations caught: the disclosure dropped (the answer lists no caller and
// reads as complete), and the early refusal dropped (a request naming
// `may_call` is answered without it).
func TestACallWalkWithoutTheLookupDisclosesOrRefusesItsNameOnlyRows(t *testing.T) {
	f := openSiteFixture("call_walk_unavailable")
	ctx := context.Background()
	req := model.GraphRequest{Start: []model.NodeID{f.id(t, "handle")}, Direction: model.DirectionIncoming,
		Relations: []model.RelationKind{model.RelCalls}}
	withLookup, err := f.engine(t).Neighbors(ctx, req)
	if err != nil {
		t.Fatalf("callers with the lookup: %v", err)
	}
	if withLookup.Counts.MayCall == 0 {
		t.Fatalf("with the lookup the walk lists no name-only caller (%+v); the fixture proves nothing",
			withLookup.Counts)
	}

	e, _ := undeclaredEngine(t, f)
	res, err := e.Neighbors(ctx, req)
	if err != nil {
		t.Fatalf("callers over calls without the lookup: %v", err)
	}
	if err := res.Validate(); err != nil {
		t.Fatalf("the disclosed answer does not validate: %v", err)
	}
	if !disclosesUnavailable(res.Meta, model.RelMayCall) {
		t.Fatalf("callers without the lookup answered %d rows with completeness %+v and notices %q: "+
			"its name-only callers read as none", len(res.Relations), res.Meta.Completeness, res.Meta.Notices)
	}

	req.Relations = []model.RelationKind{model.RelCalls, model.RelMayCall}
	_, err = e.Neighbors(ctx, req)
	var typed *model.Error
	if !errors.As(err, &typed) || typed.Code != model.CodeProviderUnavailable || typed.Message != candidatesUnavailableWhy {
		t.Fatalf("a walk naming may_call without the lookup = %v, want the candidate lookup's refusal", err)
	}
}

// TestAPathWithoutTheLookupSaysItsNameOnlySearchDidNotRun pins the path
// answer's disclosure: with no precise route, a path over `calls` searches
// name-only calls next, so a reader that cannot list candidates answers "no
// route" and says the name-only search is unavailable.
//
// Mutation caught: the disclosure dropped, so "no route" reads as no call
// linking the two at all.
func TestAPathWithoutTheLookupSaysItsNameOnlySearchDidNotRun(t *testing.T) {
	f := openSiteFixture("path_unavailable")
	ctx := context.Background()
	req := model.PathRequest{From: f.id(t, "run"), To: f.id(t, "handle"),
		Relations: []model.RelationKind{model.RelCalls}}
	withLookup, err := f.engine(t).ShortestPath(ctx, req)
	if err != nil {
		t.Fatalf("path with the lookup: %v", err)
	}
	if len(withLookup.Paths) == 0 {
		t.Fatal("with the lookup no name-only route was found; the fixture proves nothing")
	}
	e, _ := undeclaredEngine(t, f)
	res, err := e.ShortestPath(ctx, req)
	if err != nil {
		t.Fatalf("path without the lookup: %v", err)
	}
	if len(res.Paths) != 0 || !disclosesUnavailable(res.Meta, model.RelMayCall) {
		t.Fatalf("path without the lookup answered %d routes with completeness %+v and notices %q",
			len(res.Paths), res.Meta.Completeness, res.Meta.Notices)
	}
}

// TestReferencesWithoutTheLookupSayTheirNameOnlyReferencesAreUnavailable pins
// the references answer's disclosure: a symbol's references list the callers
// of the open sites that name it after the precise ones, so a reader that
// cannot list candidates answers the precise references and says the
// name-only ones are unavailable.
//
// Mutation caught: the disclosure dropped, so a symbol only open sites call
// reads as referenced by nothing.
func TestReferencesWithoutTheLookupSayTheirNameOnlyReferencesAreUnavailable(t *testing.T) {
	f := openSiteFixture("references_unavailable")
	ctx := context.Background()
	req := model.ReferenceRequest{NodeID: f.id(t, "handle"), Operation: model.ReferenceReferences,
		SemanticSource: model.SemanticCanonical}
	withLookup, err := f.engine(t).References(ctx, req)
	if err != nil {
		t.Fatalf("references with the lookup: %v", err)
	}
	if len(withLookup.Items) == 0 {
		t.Fatal("with the lookup handle has no name-only reference; the fixture proves nothing")
	}
	e, _ := undeclaredEngine(t, f)
	page, err := e.References(ctx, req)
	if err != nil {
		t.Fatalf("references without the lookup: %v", err)
	}
	if len(page.Items) != 0 || !disclosesUnavailable(page.Meta, model.RelMayCall) {
		t.Fatalf("references without the lookup answered %d items with completeness %+v and notices %q",
			len(page.Items), page.Meta.Completeness, page.Meta.Notices)
	}
}

// TestPossibleRowsRefuseAReaderWithoutTheLookup pins that the possible-row
// producer never answers an empty list for a reader that cannot list
// candidates: every caller then chooses to refuse or disclose, and a new
// caller cannot silently read "none".
//
// Mutation caught: possibleRows returning an empty list for that reader.
func TestPossibleRowsRefuseAReaderWithoutTheLookup(t *testing.T) {
	f := openSiteFixture("possible_rows_unavailable")
	e, reader := undeclaredEngine(t, f)
	_, err := e.possibleRows(context.Background(), reader, tailCallers, []model.NodeID{f.id(t, "handle")}, false, 0)
	var typed *model.Error
	if !errors.As(err, &typed) || typed.Code != model.CodeProviderUnavailable {
		t.Fatalf("possibleRows without the lookup = %v, want %s", err, model.CodeProviderUnavailable)
	}
}
