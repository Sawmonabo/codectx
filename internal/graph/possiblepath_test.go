package graph

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/Sawmonabo/codectx/internal/config"
	"github.com/Sawmonabo/codectx/internal/model"
)

// manyHandles is `main` whose `obj.handle()` no analysis bound, and width
// methods named handle, each in its own file, none of which reaches `leaf`.
func manyHandles(width int) *callFixture {
	nodes := []fixtureDecl{
		{Name: "main", Kind: model.NodeFunction, File: "main.py", Bytes: [2]uint64{0, 40}},
		{Key: "site", Name: "handle", Kind: model.NodeFunction, File: "main.py", Bytes: [2]uint64{20, 30},
			Metadata: openSite(model.CallShapeMember, 0)},
		{Name: "leaf", Kind: model.NodeFunction, File: "leaf.py", Bytes: [2]uint64{0, 20}},
	}
	for i := range width {
		nodes = append(nodes, fixtureDecl{Key: fmt.Sprintf("h%04d", i), Name: "handle", Kind: model.NodeMethod,
			File: fmt.Sprintf("p%04d.py", i), Bytes: [2]uint64{0, 30}})
	}
	return newCallFixture(fmt.Sprintf("many_handles_%d", width), fixtureScenario{Nodes: nodes,
		Relations: []fixtureFact{{From: "main", Kind: model.RelCalls, To: "site", Occurrences: site(20)}}})
}

// callWalk is the path walk ShortestPath builds for an outgoing `calls`
// search over f, on its own scratch.
func callWalk(t *testing.T, f *callFixture, frontier int64, maxEdges config.Limit) *pathWalk {
	t.Helper()
	return walkOver(t, f.reader(), model.DirectionOutgoing, frontier, maxEdges)
}

// walkOver is the path walk over reader in direction, on its own scratch.
func walkOver(t *testing.T, reader GraphReader, direction model.Direction, frontier int64,
	maxEdges config.Limit) *pathWalk {
	t.Helper()
	sc, err := openPathScratch(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("scratch: %v", err)
	}
	t.Cleanup(func() { sc.close() })
	kinds := []model.RelationKind{model.RelCalls}
	codes, absent := kindCodesFor(reader, kinds)
	return &pathWalk{reader: reader, kinds: kinds, kindCodes: codes, kindsAbsent: absent,
		direction: direction, descent: pathDescentFor(reader, direction, kinds),
		maxEdges: maxEdges, frontierBytes: frontier, sc: sc, scCtx: context.Background()}
}

// TestThePossibleSearchSpillsLikeThePreciseOne pins that the search taking
// name-only calls is the disk-backed walk: every candidate of main's open
// site settles out of one cost bucket, read in chunks no larger than the
// frontier budget plus one group, so the walk's peak chunk does not grow when
// the candidates grow 32-fold.
//
// Mutations caught: a possible search held in memory (the candidates never
// settle through the frontier, and the walk visits fewer than width nodes);
// reading a whole bucket per chunk (the peak exceeds the budget and grows).
func TestThePossibleSearchSpillsLikeThePreciseOne(t *testing.T) {
	const frontier = 512
	const oneGroup = edgeRowOverheadBytes + 4*model.MaxIdentifierBytes
	peaks := map[int]int64{}
	for _, width := range []int{16, 512} {
		f := manyHandles(width)
		w := callWalk(t, f, frontier, 0)
		w.possible = true
		if err := w.run(context.Background(), f.id(t, "main"), f.id(t, "leaf")); err != nil {
			t.Fatalf("width %d: walk: %v", width, err)
		}
		if w.visited < int64(width) {
			t.Fatalf("width %d: the walk settled %d node(s); every candidate must settle through the frontier",
				width, w.visited)
		}
		if w.peakChunkBytes > frontier+oneGroup {
			t.Fatalf("width %d: peak chunk held %d bytes, over the %d-byte budget plus one group",
				width, w.peakChunkBytes, frontier+oneGroup)
		}
		peaks[width] = w.peakChunkBytes
	}
	if peaks[512] > peaks[16] {
		t.Fatalf("the peak chunk grew from %d to %d bytes when the candidates grew 32x", peaks[16], peaks[512])
	}
}

// TestPossibleRoutesListEveryEqualCostRoute pins parity with the precise
// search, which lists every equal-cost route up to the configured count:
// main's `obj.handle()` may call A.handle or B.handle, each of which calls
// leaf, so path(main, leaf) is both routes, each possible, at one name-only
// hop plus one call, with one notice per route.
//
// Mutation caught: the possible search returning one route.
func TestPossibleRoutesListEveryEqualCostRoute(t *testing.T) {
	f := newCallFixture("possible_ties", fixtureScenario{
		Nodes: []fixtureDecl{
			{Name: "main", Kind: model.NodeFunction, File: "main.py", Bytes: [2]uint64{0, 40}},
			{Key: "site", Name: "handle", Kind: model.NodeFunction, File: "main.py", Bytes: [2]uint64{20, 30},
				Metadata: openSite(model.CallShapeMember, 0)},
			{Key: "A.handle", Name: "handle", Kind: model.NodeMethod, File: "a.py", Bytes: [2]uint64{0, 30}},
			{Key: "B.handle", Name: "handle", Kind: model.NodeMethod, File: "b.py", Bytes: [2]uint64{0, 30}},
			{Name: "leaf", Kind: model.NodeFunction, File: "leaf.py", Bytes: [2]uint64{0, 20}},
		},
		Relations: []fixtureFact{
			{From: "main", Kind: model.RelCalls, To: "site", Occurrences: site(20)},
			{From: "A.handle", Kind: model.RelCalls, To: "leaf", Occurrences: site(5)},
			{From: "B.handle", Kind: model.RelCalls, To: "leaf", Occurrences: site(5)},
		},
	})
	res, err := f.engine(t).ShortestPath(context.Background(), model.PathRequest{From: f.id(t, "main"),
		To: f.id(t, "leaf"), Relations: []model.RelationKind{model.RelCalls}})
	if err != nil {
		t.Fatalf("path(main, leaf): %v", err)
	}
	repo := f.binding.RepositoryID
	want := map[string][]model.RelationID{}
	for _, m := range []string{"A.handle", "B.handle"} {
		want[m] = []model.RelationID{model.NewRelationID(repo, f.id(t, "main"), model.RelMayCall, f.id(t, m)),
			f.rel(t, m, model.RelCalls, "leaf")}
	}
	if len(res.Paths) != 2 {
		t.Fatalf("path(main, leaf) = %+v, want both equal-cost possible routes", res.Paths)
	}
	for _, p := range res.Paths {
		if !p.Possible || p.CostUnits != Cost(model.RelMayCall)+Cost(model.RelCalls) ||
			!(slices.Equal(p.Relations, want["A.handle"]) || slices.Equal(p.Relations, want["B.handle"])) {
			t.Errorf("route %+v is not one of %v, possible, at a hop plus a call", p, want)
		}
	}
	if slices.Equal(res.Paths[0].Relations, res.Paths[1].Relations) {
		t.Errorf("both routes are %v; want one through each candidate", res.Paths[0].Relations)
	}
	n := 0
	for _, note := range res.Meta.Notices {
		if strings.Contains(note, "route 1 of 2 takes one call") || strings.Contains(note, "route 2 of 2 takes one call") {
			n++
		}
	}
	if n != 2 {
		t.Errorf("notices %q, want one counting the name-only calls of each route", res.Meta.Notices)
	}
}

// TestAPageNeverOvershootsTheEdgeBudgetOnAnOverridesList pins exact paging
// through a descent: speak's virtual call reaches Animal.sound's six
// overrides, and an edge budget of two per page splits that list across pages
// by the scan position inside it. Every page spends at most the budget, and
// the pages together still reach the last override.
//
// Mutation caught: reading a dispatch state's overrides list whole (the page
// that reads it spends six edges).
func TestAPageNeverOvershootsTheEdgeBudgetOnAnOverridesList(t *testing.T) {
	nodes := []fixtureDecl{
		{Name: "Animal.sound", Kind: model.NodeMethod, File: "a.py", Bytes: [2]uint64{0, 30}},
		{Name: "speak", Kind: model.NodeFunction, File: "s.py", Bytes: [2]uint64{0, 30}},
	}
	rels := []fixtureFact{{From: "speak", Kind: model.RelCalls, To: "Animal.sound", Occurrences: virtualAt(5)}}
	for i := range 6 {
		key := fmt.Sprintf("Dog%d.sound", i)
		nodes = append(nodes, fixtureDecl{Key: key, Name: "sound", Kind: model.NodeMethod,
			File: fmt.Sprintf("d%d.py", i), Bytes: [2]uint64{0, 30}})
		rels = append(rels, fixtureFact{From: key, Kind: model.RelOverrides, To: "Animal.sound"})
	}
	f := newCallFixture("override_pages", fixtureScenario{Nodes: nodes, Relations: rels})
	const budget = 2
	w := callWalk(t, f, 1<<20, budget)
	base := *w
	for page := 0; ; page++ {
		if page == 50 {
			t.Fatalf("no page settled the target in 50 pages")
		}
		if err := w.run(context.Background(), f.id(t, "speak"), f.id(t, "Dog5.sound")); err != nil {
			t.Fatalf("page %d: %v", page, err)
		}
		if w.pageEdges > budget {
			t.Fatalf("page %d spent %d edges, over the per-page budget of %d", page, w.pageEdges, budget)
		}
		if w.targetSettled {
			if page < 2 {
				t.Fatalf("the target settled on page %d; the proof needs the list split across pages", page)
			}
			return
		}
		if !w.edgesHit {
			t.Fatalf("page %d ended neither on the target nor on the edge budget", page)
		}
		next := base
		next.resumed = true
		next.visited, next.spent, next.bucketCost, next.lastSettled = w.visited, w.spent, w.bucketCost, w.lastSettled
		w = &next
	}
}

// countedDecls counts the rows a reader hands out of one name's declarations,
// so a test can see how much of that list one page read.
type countedDecls struct {
	*locatedGraph
	name string
	rows int
}

func (c *countedDecls) Declarations(ctx context.Context, name string, kinds []model.NodeKind, after model.NodeID,
	limit int) ([]Declaration, model.NodeID, error) {
	page, next, err := c.locatedGraph.Declarations(ctx, name, kinds, after, limit)
	if name == c.name {
		c.rows += len(page)
	}
	return page, next, err
}

// pagesOf runs w to the end page by page, as continuations would, and
// returns the declaration rows each page read; it fails a page that spends
// more than the edge budget.
func pagesOf(t *testing.T, w *pathWalk, reader *countedDecls, from, to model.NodeID, budget int64) ([]int, *pathWalk) {
	t.Helper()
	base := *w
	var rows []int
	for page := 0; ; page++ {
		if page == 1000 {
			t.Fatalf("the walk did not finish in 1000 pages")
		}
		reader.rows = 0
		if err := w.run(context.Background(), from, to); err != nil {
			t.Fatalf("page %d: %v", page, err)
		}
		rows = append(rows, reader.rows)
		if w.pageEdges > budget {
			t.Fatalf("page %d spent %d edges, over the budget of %d", page, w.pageEdges, budget)
		}
		if !w.edgesHit {
			return rows, w
		}
		next := base
		next.resumed = true
		next.visited, next.spent, next.bucketCost, next.lastSettled = w.visited, w.spent, w.bucketCost, w.lastSettled
		w = &next
	}
}

// TestNameOnlyStepsPageTheirLists pins that a name-only step streams the
// lists it reads: a name with five pages of declarations, as an open site's
// candidates (forward) and as the sites naming a method (backward), is read
// a page at a time across pages the edge budget ends, and the steps resume
// inside the list. Each page reads at most two declaration pages, and the
// pages together take every step exactly once.
//
// Mutation caught: reading a name's whole list per step (a page reads all
// five declaration pages, and a resumed page reads them again).
func TestNameOnlyStepsPageTheirLists(t *testing.T) {
	width := 5 * model.MaxPageItems
	const budget = 50
	limit := 2 * model.MaxPageItems

	f := manyHandles(width)
	reader := &countedDecls{locatedGraph: f.reader(), name: "handle"}
	w := walkOver(t, reader, model.DirectionOutgoing, 1<<20, budget)
	w.possible = true
	rows, last := pagesOf(t, w, reader, f.id(t, "main"), f.id(t, "leaf"), budget)
	for page, n := range rows {
		if n > limit {
			t.Errorf("forward: page %d read %d declaration rows; a step reads one page of a name at a time", page, n)
		}
	}
	// main's call into the open site, then one step per candidate, once each.
	if last.spent != int64(1+width) {
		t.Errorf("forward: the pages spent %d edges, want %d: every step taken exactly once", last.spent, 1+width)
	}

	nodes := []fixtureDecl{
		{Name: "handle", Kind: model.NodeMethod, File: "lib.py", Bytes: [2]uint64{0, 30}},
		{Name: "lonely", Kind: model.NodeFunction, File: "lonely.py", Bytes: [2]uint64{0, 20}},
	}
	var rels []fixtureFact
	for i := range width {
		s, c := fmt.Sprintf("s%04d", i), fmt.Sprintf("c%04d", i)
		nodes = append(nodes,
			fixtureDecl{Name: c, Kind: model.NodeFunction, File: c + ".py", Bytes: [2]uint64{0, 40}},
			fixtureDecl{Key: s, Name: "handle", Kind: model.NodeFunction, File: c + ".py", Bytes: [2]uint64{20, 30},
				Metadata: openSite(model.CallShapeMember, 0)})
		rels = append(rels, fixtureFact{From: c, Kind: model.RelCalls, To: s, Occurrences: site(20)})
	}
	g := newCallFixture("many_sites", fixtureScenario{Nodes: nodes, Relations: rels})
	reader = &countedDecls{locatedGraph: g.reader(), name: "handle"}
	w = walkOver(t, reader, model.DirectionIncoming, 1<<20, budget)
	w.possible = true
	rows, last = pagesOf(t, w, reader, g.id(t, "handle"), g.id(t, "lonely"), budget)
	for page, n := range rows {
		if n > limit {
			t.Errorf("backward: page %d read %d declaration rows; the sites naming a method are read a page at a time",
				page, n)
		}
	}
	if last.spent != int64(width) {
		t.Errorf("backward: the pages spent %d edges, want %d: one step per site's caller, once each", last.spent, width)
	}
}
