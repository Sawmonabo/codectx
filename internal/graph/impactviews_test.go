package graph

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
)

// chainRequest is the outgoing call walk from the head of a chain fixture.
func chainRequest(chain []model.NodeID, limit int) model.ImpactRequest {
	return model.ImpactRequest{GenerationID: 1, Start: chain[:1], Direction: model.DirectionOutgoing,
		Relations: []model.RelationKind{model.RelCalls}, Page: model.PageRequest{Limit: limit}}
}

// readEntries pages the entries view of req to its end and answers every entry
// by ordinal and the answer token.
func readEntries(t *testing.T, e *Engine, req model.ImpactRequest) (map[int64]model.ImpactEntry, string) {
	t.Helper()
	out, answer := map[int64]model.ImpactEntry{}, ""
	for page := 0; ; page++ {
		if page > 1000 {
			t.Fatal("the entries view did not end in 1000 pages")
		}
		res, err := e.Impact(context.Background(), req)
		if err != nil {
			t.Fatalf("entries page %d: %v", page, err)
		}
		if answer == "" {
			answer = res.Answer
		}
		for _, en := range res.Entries {
			out[en.Ordinal] = en
		}
		if res.Meta.NextCursor == "" {
			return out, answer
		}
		req.GenerationID, req.Page.Cursor = 0, res.Meta.NextCursor
	}
}

// TestARouteLongerThanAPathBoundIsServedWhole pins that the routes view serves
// every relation of a route, however long: the end of a 70-call chain is
// served the whole chain, on a later page of the view.
//
// Mutation caught: the old 64-relation clip restored, either in routesOf
// (skipping a route longer than model.MaxRelationsPerPath) or in
// ImpactRoute.Validate (refusing one), so the deepest entries are served no
// route or the page is refused.
func TestARouteLongerThanAPathBoundIsServedWhole(t *testing.T) {
	const hops = 70
	f, chain := newChainFixture(hops)
	e, _ := chainEngine(t, f, 16, 0)
	req := chainRequest(chain, 16)
	entries, answer := readEntries(t, e, req)
	_, routes := explainImpact(t, e, req, answer)
	for ord, en := range entries {
		rs := routes[ord]
		if int64(len(rs)) != en.RouteCount || len(rs) != 1 {
			t.Fatalf("entry %d names %d routes and was served %d, want its one", ord, en.RouteCount, len(rs))
		}
		if want := chain[:1+len(rs[0].Relations)]; want[len(want)-1] != en.NodeID {
			t.Fatalf("entry %d (%s) was served a %d-relation route, which ends elsewhere", ord, en.Name,
				len(rs[0].Relations))
		}
		if len(rs[0].Relations) == hops && !slices.Equal(rs[0].Relations[60:], []model.RelationID{
			fixtureRelationID(60), fixtureRelationID(61), fixtureRelationID(62), fixtureRelationID(63),
			fixtureRelationID(64), fixtureRelationID(65), fixtureRelationID(66), fixtureRelationID(67),
			fixtureRelationID(68), fixtureRelationID(69)}) {
			t.Fatalf("the %d-relation route ends %v, want the chain's last ten relations", hops, rs[0].Relations[60:])
		}
	}
	if len(entries) != hops {
		t.Fatalf("the answer served %d entities, want %d", len(entries), hops)
	}
}

// TestAReasonsPageResumesInsideAnEntry pins the reasons view across a page
// that ends inside one entry's reasons: every entry reached over two relation
// kinds has two reasons, and a page of three ends one reason into the second
// entry, so page two must start at that entry's second reason.
//
// Mutation caught: the reasons view's continuation dropping the skip it
// carries (servePageOf or the cursor ignoring ItemSkip), so page two repeats
// the reason page one already served and the answer serves one reason twice.
func TestAReasonsPageResumesInsideAnEntry(t *testing.T) {
	f, chain := newChainFixture(3)
	for i := 0; i < 3; i++ {
		f.relations = append(f.relations, model.Relation{ID: fixtureRelationID(100 + i),
			From: chain[i], Kind: model.RelImports, To: chain[i+1]})
	}
	e, _ := chainEngine(t, f, 3, 0)
	req := chainRequest(chain, 3)
	req.Relations = []model.RelationKind{model.RelCalls, model.RelImports}
	entries, answer := readEntries(t, e, req)
	r := req
	r.GenerationID, r.View, r.Page.Cursor = 0, model.ImpactViewReasons, answer
	type at struct{ entry, index int64 }
	seen := map[at]bool{}
	pages := 0
	for {
		res, err := e.Impact(context.Background(), r)
		if err != nil {
			t.Fatalf("reasons page %d: %v", pages, err)
		}
		pages++
		for _, x := range res.Reasons {
			k := at{x.Entry, x.Index}
			if seen[k] {
				t.Fatalf("reason %d of entry %d was served twice", x.Index, x.Entry)
			}
			seen[k] = true
		}
		if res.Meta.NextCursor == "" {
			break
		}
		r.Page.Cursor = res.Meta.NextCursor
	}
	total := int64(0)
	for ord, en := range entries {
		if en.ReasonCount != 2 {
			t.Fatalf("entry %d names %d reasons; reached over two kinds it has two", ord, en.ReasonCount)
		}
		for i := int64(0); i < en.ReasonCount; i++ {
			if !seen[at{ord, i}] {
				t.Fatalf("reason %d of entry %d was never served", i, ord)
			}
		}
		total += en.ReasonCount
	}
	if int64(len(seen)) != total || pages < 2 {
		t.Fatalf("the reasons view served %d reasons over %d pages, want %d over at least two", len(seen), pages, total)
	}
}

// TestAViewContinuationIsRefusedForAnotherView pins that a continuation of one
// view presented with another is refused: its position counts the records of
// its own view, which would start the other view mid-answer.
//
// Mutation caught: traversalCursor.position accepting a continuation whatever
// view it is presented with.
func TestAViewContinuationIsRefusedForAnotherView(t *testing.T) {
	f, chain := newChainFixture(12)
	e, _ := chainEngine(t, f, 4, 0)
	req := chainRequest(chain, 4)
	res, err := e.Impact(context.Background(), req)
	if err != nil {
		t.Fatalf("first page: %v", err)
	}
	if res.Meta.NextCursor == "" {
		t.Fatal("a 12-entity answer at a page of four minted no continuation")
	}
	req.GenerationID, req.View, req.Page.Cursor = 0, model.ImpactViewRoutes, res.Meta.NextCursor
	_, err = e.Impact(context.Background(), req)
	var me *model.Error
	if !errors.As(err, &me) || me.Code != model.CodeArgumentInvalid {
		t.Fatalf("the entries continuation presented with the routes view answered %v, want it refused", err)
	}
}

// TestAnAnswerIsReleasedOnceEveryViewItReadsIsDone pins the answer's release:
// a client that names only the entries view leaves nothing behind once its
// entries end, and a client that reads all three leaves nothing once the last
// of them ends -- neither waits for the lease to expire.
//
// Mutations caught: the views a request does not read left unmarked at mint
// (serveRankedRun ignoring ImpactRequest.Views), so the entries-only answer
// outlives its last page; and finishView not releasing when the last view
// ends, so the full read leaves its spool and route table behind.
func TestAnAnswerIsReleasedOnceEveryViewItReadsIsDone(t *testing.T) {
	f, chain := newChainFixture(12)
	e, root := chainEngine(t, f, 4, 0)
	req := chainRequest(chain, 4)
	req.Views = []model.ImpactView{model.ImpactViewEntries}
	if _, answer := readEntries(t, e, req); answer == "" {
		t.Fatal("a paged answer minted no answer token")
	}
	if left := spoolEntries(t, root); len(left) != 0 {
		t.Fatalf("an entries-only answer left %v behind after its last page", left)
	}
	req.Views = nil
	_, answer := readEntries(t, e, req)
	if left := spoolEntries(t, root); len(left) == 0 {
		t.Fatal("an answer whose reasons and routes are still to be read was released with its entries")
	}
	explainImpact(t, e, req, answer)
	if left := spoolEntries(t, root); len(left) != 0 {
		t.Fatalf("an answer read through all three views left %v behind", left)
	}
}

// entriesOnly is the Views of a request that reads the entries view alone, so
// its answer is released when its entries end.
var entriesOnly = []model.ImpactView{model.ImpactViewEntries}
