package graph

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
)

// TestReferencePageBoundIsDisclosedNotClamped is the class-G proof for
// References. The failure mode: a page bound clamped twice in silence -- once
// against the configured ceiling, once against the wire ceiling -- so a caller
// that asked for 50 occurrences and was served 200 cannot tell a clamped page
// from the end of the answer. The bound is RESOLVED and disclosed, the same way
// a traversal's bounds are.
//
// Mutation (resolvePageItems replaced by the old `if pageLimit <= 0 ||
// pageLimit > e.limits.MaxPageItems` clamp): Notices is empty and this fails.
func TestReferencePageBoundIsDisclosedNotClamped(t *testing.T) {
	f := newGraphFixture(t)
	limits := fixtureLimits()
	limits.MaxPageItems = 5
	e, err := New(Options{Adjacency: f, Reader: memGraphFor(f), Limits: limits})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	page, err := e.References(context.Background(), model.ReferenceRequest{
		NodeID:         fixtureNodeID("n-b"),
		Operation:      model.ReferenceReferences,
		SemanticSource: model.SemanticCanonical,
		Page:           model.PageRequest{Limit: 50},
	})
	if err != nil {
		t.Fatalf("References: %v", err)
	}
	found := false
	for _, n := range page.Meta.Notices {
		if strings.Contains(n, "page.limit: requested 50, effective 5") {
			found = true
		}
	}
	if !found {
		t.Fatalf("a page bound clamped from 50 to 5 was not disclosed; notices = %q", page.Meta.Notices)
	}
}

// refsEngine builds a reference engine over f with continuations wired, whose
// reader assigns surrogates in the given order. It returns the engine and the
// lease store, which is what a test asserting "this answer retained nothing"
// reads.
func refsEngine(t *testing.T, f *graphFixture, order MemoryGraphOrder, pageItems int) (*Engine, *fixtureLeases) {
	t.Helper()
	signer, err := pagination.OpenSigner(t.TempDir())
	if err != nil {
		t.Fatalf("open signer: %v", err)
	}
	store := newFixtureLeases()
	spools, err := pagination.NewSpools(t.TempDir(), 64<<20, store)
	if err != nil {
		t.Fatalf("new spools: %v", err)
	}
	limits := fixtureLimits()
	limits.MaxPageItems = pageItems
	nodes := make([]model.Node, 0, len(f.nodes))
	for _, n := range f.nodes {
		nodes = append(nodes, n)
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].ID < nodes[j].ID })
	reader := NewMemoryGraphOrdered(f.binding, nodes, append([]model.Relation(nil), f.relations...), order)
	e, err := New(Options{Adjacency: f, Reader: reader, Signer: signer, Spools: spools,
		Leases: pagination.NewLeases(store, limits.CursorTTL), Limits: limits})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	return e, store
}

func refsPage(t *testing.T, e *Engine, node, cursor string) model.Page[model.ReferenceOccurrence] {
	t.Helper()
	page, err := e.References(context.Background(), model.ReferenceRequest{
		NodeID:         fixtureNodeID(node),
		Operation:      model.ReferenceReferences,
		SemanticSource: model.SemanticCanonical,
		Page:           model.PageRequest{Cursor: cursor},
	})
	if err != nil {
		t.Fatalf("References(%s): %v", node, err)
	}
	return page
}

// TestReferencePagesAreIdenticalUnderEitherSurrogateOrder is the identity proof
// across the surrogate/canonical boundary. The packed reader streams a node's
// list in SURROGATE order, which the store assigns and may renumber; the
// answer's order is the canonical relation id and always was. The same fixture
// read through a reader whose surrogates ascend with the canonical ids and
// through one whose surrogates REVERSE them must therefore serve the same page,
// byte for byte -- a caller's saved page cannot be reshuffled by a reindex.
//
// Mutation (the sort's comparator made a constant, which leaves the stable sort
// and the merge's run tie-break holding arrival order -- that is, surrogate
// order): the reversed page lists the two relations the other way round and
// this fails.
func TestReferencePagesAreIdenticalUnderEitherSurrogateOrder(t *testing.T) {
	f := newGraphFixture(t)
	canonical, _ := refsEngine(t, f, MemoryGraphOrder{}, 200)
	reversed, _ := refsEngine(t, f, MemoryGraphOrder{
		Nodes:     func(a, b model.NodeID) int { return -cmp.Compare(a, b) },
		Relations: func(a, b model.RelationID) int { return -cmp.Compare(a, b) },
	}, 200)

	// n-z is called by both n-p and n-q, so its answer holds more than one
	// relation and an order is observable at all.
	a, b := refsPage(t, canonical, "n-z", ""), refsPage(t, reversed, "n-z", "")
	if len(a.Items) < 2 {
		t.Fatalf("the fixture served %d occurrences for n-z; an order needs at least two", len(a.Items))
	}
	wantJSON, gotJSON := mustJSON(t, a.Items), mustJSON(t, b.Items)
	if wantJSON != gotJSON {
		t.Fatalf("the reversed surrogate order served a different page\n canonical: %s\n reversed:  %s", wantJSON, gotJSON)
	}
}

// TestReferenceSecondPageIsServedByOffset proves the continuation reads its own
// byte range out of the spool the first page wrote, and walks nothing. The two
// pages must CONCATENATE to the single-shot answer: the same occurrences, in
// the same order, each listed exactly once.
//
// Mutation (resumedReferencePage opens at offset 0 instead of the cursor's):
// page two re-serves page one's relation and the duplicate check fails.
func TestReferenceSecondPageIsServedByOffset(t *testing.T) {
	f := newGraphFixture(t)
	// THREE pages, not two: with only two, page two begins at the first record
	// of the spool, which is where a reader that ignored the offset would start
	// anyway, and the offset would carry no weight.
	widenReferences(t, f, "n-z", 2)
	whole, _ := refsEngine(t, f, MemoryGraphOrder{}, 200)
	want := refsPage(t, whole, "n-z", "")
	if len(want.Items) < 4 {
		t.Fatalf("the fixture served %d occurrences for n-z; three pages need at least four", len(want.Items))
	}

	// One occurrence per page, so the page ends on the first relation boundary
	// with the second relation still to come.
	paged, leases := refsEngine(t, f, MemoryGraphOrder{}, 1)
	first := refsPage(t, paged, "n-z", "")
	if first.Meta.NextCursor == "" {
		t.Fatalf("a page holding %d of %d occurrences offered no continuation", len(first.Items), len(want.Items))
	}
	if leases.liveCount() != 1 {
		t.Fatalf("liveCount = %d, want the one lease the continuation retains", leases.liveCount())
	}
	got := append([]model.ReferenceOccurrence(nil), first.Items...)
	for cursor := first.Meta.NextCursor; cursor != ""; {
		page := refsPage(t, paged, "n-z", cursor)
		got, cursor = append(got, page.Items...), page.Meta.NextCursor
	}
	if s, w := mustJSON(t, got), mustJSON(t, want.Items); s != w {
		t.Fatalf("the pages do not concatenate to the single-shot answer\n want: %s\n got:  %s", w, s)
	}
	if leases.liveCount() != 0 {
		t.Fatalf("liveCount = %d after the last page; an exhausted answer must retain nothing", leases.liveCount())
	}
}

// TestReferenceListThatFitsOnePageRetainsNothing is the degree-one proof: an
// answer served whole adopts no spool and mints no lease, so a small reference
// query pays for its own list and nothing else.
//
// Mutation (firstReferencePage spools and mints its lease unconditionally
// instead of only when relations remain): a lease is live and this fails.
func TestReferenceListThatFitsOnePageRetainsNothing(t *testing.T) {
	f := newGraphFixture(t)
	// n-b is called by n-a and by nothing else.
	e, leases := refsEngine(t, f, MemoryGraphOrder{}, 200)
	page := refsPage(t, e, "n-b", "")
	if len(page.Items) == 0 {
		t.Fatalf("n-b has no reference occurrences; the case proves nothing")
	}
	if page.Meta.NextCursor != "" || page.Meta.Truncated {
		t.Fatalf("a complete answer offered a continuation (%q) or reported truncation (%v)",
			page.Meta.NextCursor, page.Meta.Truncated)
	}
	if leases.liveCount() != 0 {
		t.Fatalf("liveCount = %d, want none: a list that fits one page retains nothing", leases.liveCount())
	}
}

// TestReferencePagesResumeAcrossADeferredBatch protects the one thing a paged
// reference answer must never do: lose or duplicate an occurrence. A batch of
// relations is hydrated as a unit, and when its FIRST relation does not fit the
// page the batch that preceded it partly filled, the batch consumes nothing.
// That is not a stuck cursor -- the offset still names the batch's start, so
// the deferred relation is the next page's first -- and answering
// CTX_INTERNAL there makes a whole class of ordinary reference lists
// unreadable past their second page.
//
// The counts below are chosen so the case arises on a RESUMED page: relations
// carrying no evidence row contribute no occurrence, so a batch of four can
// leave the page half full, and the next batch opens on a relation whose three
// occurrences overflow it. The per-page counts are asserted because a fixture
// that stopped producing the deferred batch would leave this passing on a case
// it no longer exercises.
//
// Mutation (the `break` on b.full restored to the internalErr it replaced):
// page two answers CTX_INTERNAL and this fails.
func TestReferencePagesResumeAcrossADeferredBatch(t *testing.T) {
	f := newGraphFixture(t)
	// n-sink is referenced by nothing, so the list below is the whole answer
	// and its batch arithmetic is a fact of this test rather than of the
	// fixture's other scenarios.
	addReferences(t, f, "n-sink", []int{1, 1, 1, 1, 1, 0, 0, 1, 3, 1, 1, 1})
	whole, _ := refsEngine(t, f, MemoryGraphOrder{}, 200)
	want := refsPage(t, whole, "n-sink", "")

	// Four occurrences a page, which is also the hydration batch: batch one of
	// page two serves two occurrences and batch two opens on the relation
	// carrying three.
	paged, leases := refsEngine(t, f, MemoryGraphOrder{}, 4)
	page := refsPage(t, paged, "n-sink", "")
	got, sizes := append([]model.ReferenceOccurrence(nil), page.Items...), []int{len(page.Items)}
	for cursor := page.Meta.NextCursor; cursor != ""; {
		page = refsPage(t, paged, "n-sink", cursor)
		got, sizes = append(got, page.Items...), append(sizes, len(page.Items))
		cursor = page.Meta.NextCursor
	}
	if fmt.Sprint(sizes) != fmt.Sprint([]int{4, 2, 4, 2}) {
		t.Fatalf("page sizes = %v, want [4 2 4 2]: the deferred batch is what page two's short page is", sizes)
	}
	if s, w := mustJSON(t, got), mustJSON(t, want.Items); s != w {
		t.Fatalf("the pages do not concatenate to the single-shot answer\n want: %s\n got:  %s", w, s)
	}
	if leases.liveCount() != 0 {
		t.Fatalf("liveCount = %d after the last page; an exhausted answer must retain nothing", leases.liveCount())
	}
}

// addReferences appends one calling relation per entry of rows, from a caller
// of its own to node, carrying that many evidence rows. A relation with zero
// rows is a canonical reference with no location: it is consumed and reported
// as no occurrence, which is what lets a hydration batch fill less of a page
// than it holds relations.
func addReferences(t *testing.T, f *graphFixture, node string, rows []int) {
	t.Helper()
	proto := protoEvidence(t, f)
	target := fixtureNodeID(node)
	for i, n := range rows {
		name := fmt.Sprintf("n-defer-src-%d", i)
		src := fixtureNodeID(name)
		f.nodes[src] = model.Node{ID: src, Kind: model.NodeFunction, Name: name,
			QualifiedName: name, Language: "go", SemanticSource: model.SemanticCanonical}
		rel := model.RelationID(fmt.Sprintf("%064x", 0xe000+i))
		f.relations = append(f.relations, model.Relation{
			ID: rel, From: src, Kind: model.RelCalls, To: target})
		for j := 0; j < n; j++ {
			row := proto
			row.ID = model.EvidenceID(fixtureID(fmt.Sprintf("ev-defer-%d-%d", i, j)))
			row.RelationID = rel
			f.evidence[rel] = append(f.evidence[rel], row)
		}
	}
	sort.Slice(f.relations, func(i, j int) bool { return f.relations[i].ID < f.relations[j].ID })
}

// protoEvidence is the fixture's own evidence row, copied by the helpers that
// mint new ones so a minted row carries a valid precision, file and range.
func protoEvidence(t *testing.T, f *graphFixture) model.Evidence {
	t.Helper()
	for _, r := range f.relations {
		if rows := f.evidence[r.ID]; len(rows) > 0 {
			return rows[0]
		}
	}
	t.Fatalf("the fixture carries no evidence row to model a new one on")
	return model.Evidence{}
}

// widenReferences adds n further evidence-bearing callers of node to the
// fixture. Their relation ids sort after every declared edge, so no existing
// scenario's keyset order moves and no other answer gains an occurrence.
func widenReferences(t *testing.T, f *graphFixture, node string, n int) {
	t.Helper()
	proto := protoEvidence(t, f)
	target := fixtureNodeID(node)
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("n-page-src-%d", i)
		src := fixtureNodeID(name)
		f.nodes[src] = model.Node{ID: src, Kind: model.NodeFunction, Name: name,
			QualifiedName: name, Language: "go", SemanticSource: model.SemanticCanonical}
		rel := model.RelationID(fmt.Sprintf("%064x", 0xf000+i))
		f.relations = append(f.relations, model.Relation{
			ID: rel, From: src, Kind: model.RelCalls, To: target})
		row := proto
		row.ID = model.EvidenceID(fixtureID(fmt.Sprintf("ev-page-%d", i)))
		row.RelationID = rel
		f.evidence[rel] = []model.Evidence{row}
	}
	sort.Slice(f.relations, func(i, j int) bool { return f.relations[i].ID < f.relations[j].ID })
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}
