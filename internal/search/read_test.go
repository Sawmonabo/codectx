package search

import (
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/search/textindex"
	"github.com/Sawmonabo/codectx/internal/snapshot"
	"github.com/Sawmonabo/codectx/internal/storage/sqlite"
)

// readAll runs ReadAnswer over the fixture's generation to its end and answers
// every hit it handed over and every request it sent the index.
func readAll(t *testing.T, f *fixture, s *Service, query string, first int, budget int64) ([]model.SearchHit, []textindex.Request) {
	t.Helper()
	var reqs []textindex.Request
	prev := observeIndexRequest
	observeIndexRequest = func(r textindex.Request) { reqs = append(reqs, r) }
	defer func() { observeIndexRequest = prev }()
	var hits []model.SearchHit
	end, err := s.ReadAnswer(f.ctx, pinned(t, f), model.SearchRequest{Query: query}, first, budget,
		func(step []model.SearchHit) (bool, error) {
			hits = append(hits, step...)
			return true, nil
		})
	if err != nil {
		t.Fatalf("%q: ReadAnswer(first %d, budget %d): %v", query, first, budget, err)
	}
	if end.Stuck {
		t.Fatalf("%q: ReadAnswer(first %d, budget %d) reported a stuck read", query, first, budget)
	}
	return hits, reqs
}

// pinned is a reader over the fixture's generation, closed when the test ends.
func pinned(t *testing.T, f *fixture) *sqlite.PinnedReader {
	t.Helper()
	reader, err := f.store.PinGeneration(f.ctx, f.repo, f.gen, time.Minute)
	if err != nil {
		t.Fatalf("PinGeneration: %v", err)
	}
	t.Cleanup(func() { reader.Close() })
	return reader
}

// TestReadAnswerServesThePagedSequence proves the in-process read hands over
// exactly the hits, in exactly the order, that walking Search's pages serves,
// whatever the step sizes: pagingDocs scores in tied runs of six, so steps of
// 2 and 3 that then double split those runs at step boundaries, and "zeta"
// adds the exact tier and the exact fold in front of the lexical tier. The
// budgets cover a read that never grows (one byte) and one that always does.
//
// Mutation: resume a step strictly below the last served score instead of
// after the last served result (dropping the rest of a tied run across the
// step boundary), and the read loses hits the pages serve.
func TestReadAnswerServesThePagedSequence(t *testing.T) {
	f := newFixtureOf(t, pagingDocs())
	s := newService(t, f.opts)
	for _, query := range []string{"omega", "zeta"} {
		for _, first := range []int{2, 3} {
			paged, _ := pagedReads(t, f.ctx, s, query, first)
			if len(paged) != 24 {
				t.Fatalf("%q: the pages served %d hits, want the 24 nodes", query, len(paged))
			}
			for _, budget := range []int64{1, math.MaxInt64} {
				got, _ := readAll(t, f, s, query, first, budget)
				if !reflect.DeepEqual(got, paged) {
					for i := range min(len(got), len(paged)) {
						if !reflect.DeepEqual(got[i], paged[i]) {
							t.Fatalf("%q first %d budget %d: hit %d = %+v, the pages serve %+v",
								query, first, budget, i, got[i], paged[i])
						}
					}
					t.Fatalf("%q first %d budget %d: the read handed over %d hits, the pages %d",
						query, first, budget, len(got), len(paged))
				}
			}
		}
	}
}

// TestReadAnswerDoublesItsDepth proves the read's cost: with nothing filtered
// and room in the budget, each step asks the index for twice the previous
// step's results, so reading the 24 "omega" results from a first depth of 1
// (N = 24P) takes the five steps of depths 1, 2, 4, 8, 16, where the paged walk
// at P = 1 sends one index request per result.
//
// Mutation: grow the depth linearly (P, 2P, 3P, ...): the read then takes seven
// steps. Mutation: never grow: it takes one step per result, as the pages do.
func TestReadAnswerDoublesItsDepth(t *testing.T) {
	f := newFixtureOf(t, pagingDocs())
	s := newService(t, f.opts)
	_, pages := pagedReads(t, f.ctx, s, "omega", 1)
	pagedRequests := 0
	for _, p := range pages {
		pagedRequests += len(p.reqs)
	}
	got, reqs := readAll(t, f, s, "omega", 1, math.MaxInt64)
	if len(got) != 24 {
		t.Fatalf("the read handed over %d hits, want 24", len(got))
	}
	want := []int{2, 3, 5, 9, 17}
	depths := make([]int, len(reqs))
	for i, r := range reqs {
		depths[i] = r.Depth
	}
	if !reflect.DeepEqual(depths, want) {
		t.Fatalf("the read asked the index for depths %v, want %v (steps of 1, 2, 4, 8, 16, each asking one more)",
			depths, want)
	}
	if len(reqs) >= pagedRequests {
		t.Fatalf("the read sent %d index requests, the paged walk %d; growing the depth must score less",
			len(reqs), pagedRequests)
	}
}

// TestReadAnswerHoldsToItsBudget proves the step sizing: under a budget too
// small for any growth every index request carries the bound, the read still
// hands over the paged answer, and stepSizer admits the largest doubling that
// fits, keeping the depth there rather than falling back to the first, and
// counts the marked-group bound.
//
// Mutation: let the sizer ignore the budget, and next(4) under a one-byte
// budget answers 8. Mutation: fall back to the first depth when a doubling
// does not fit, and next(4) answers 2. Mutation: drop the marked groups from
// stepBytes.
func TestReadAnswerHoldsToItsBudget(t *testing.T) {
	f := newFixtureOf(t, pagingDocs())
	s := newService(t, f.opts)
	paged, _ := pagedReads(t, f.ctx, s, "omega", 2)
	got, reqs := readAll(t, f, s, "omega", 2, 1)
	if !reflect.DeepEqual(got, paged) {
		t.Fatalf("under a one-byte budget the read handed over %d hits, the pages %d, or another order",
			len(got), len(paged))
	}
	for i, r := range reqs {
		if r.MaxBytes != 1 {
			t.Fatalf("under a one-byte budget request %d carries a bound of %d", i, r.MaxBytes)
		}
	}

	shape := stepShape{segments: 1, heaps: 1, strBytes: 100}
	z := stepSizer{first: 2, budget: stepBytes(shape, 4), shape: shape, measured: true}
	if n := z.next(2); n != 4 {
		t.Fatalf("next(2) = %d under a budget that holds a step of 4, want 4", n)
	}
	if n := z.next(4); n != 4 {
		t.Fatalf("next(4) = %d under a budget that holds 4 and not 8, want the largest admitted depth 4", n)
	}
	z.budget = 1
	if n := z.next(4); n != 2 {
		t.Fatalf("next(4) = %d under a one-byte budget, want the first depth 2", n)
	}
	marked := shape
	marked.marked = 1 << 20
	if stepBytes(marked, 4) <= stepBytes(shape, 4) {
		t.Fatal("a million marked groups add nothing to a step's admitted bytes")
	}
}

// burstDocs is a corpus whose groups each hold several documents of one
// score, so every position deep in the answer marks a burst of groups at once.
// Every signature is padded to about 3,000 bytes, so a result's charge (about
// 1,095 + 912 + 7 x 3,100 = 25 KB) dominates the heaps and marked groups.
func burstDocs() []doc {
	pad := strings.Repeat("p", 3000)
	var out []doc
	for i := range 40 {
		name := fmt.Sprintf("Burst%02d", i)
		out = append(out, doc{path: fmt.Sprintf("pkg/b%02d.go", i), name: name,
			qualifiedName: "pkg.burst." + name, signature: "func " + name + "(" + pad + ")",
			body: "func " + name + "() { " + strings.Repeat("omega ", 1+i%3) + "}", occurrences: 4})
	}
	return out
}

// TestReadAnswerHoldsTheLibraryToItsBound proves the bound is enforced by the
// library, not estimated: under a bound that holds only a few results, every
// index page charges at most the bytes its request allowed -- the marked
// groups of a deep position and any rescoring included -- and the pages ended
// early by the bound still hand over exactly the paged answer.
//
// Mutation: let the library ignore max_bytes (size the heap and the merge
// from the depth alone), and a page charges more than its request allowed.
func TestReadAnswerHoldsTheLibraryToItsBound(t *testing.T) {
	f := newFixtureOf(t, burstDocs())
	s := newService(t, f.opts)
	paged, _ := pagedReads(t, f.ctx, s, "omega", 2)
	if len(paged) != 40 {
		t.Fatalf("the pages served %d hits, want the 40 groups", len(paged))
	}
	// At 60 KiB the first request of a step (depth 3) fits two results and not
	// three, so its page ends early; the next request, its bound less the 7.5
	// KB each kept hit holds, fits one; every page stays within its bound; and
	// the heap fits at four places and the sizer keeps the depth at 2.
	const bound = 60 << 10
	var over []string
	stopped := 0
	prev := observeIndexPage
	observeIndexPage = func(r textindex.Request, p textindex.Page) {
		if r.MaxBytes <= 0 {
			over = append(over, "a request of the bounded read carries no bound")
			return
		}
		if p.Stats.Bytes > r.MaxBytes {
			over = append(over, fmt.Sprintf("a page charged %d bytes against a bound of %d (marked %d, cap %d)",
				p.Stats.Bytes, r.MaxBytes, p.Stats.Marked, p.Stats.Cap))
		}
		if p.More && len(p.Hits) < r.Depth {
			stopped++
		}
	}
	defer func() { observeIndexPage = prev }()
	got, _ := readAll(t, f, s, "omega", 2, bound)
	if len(over) > 0 {
		t.Fatal(strings.Join(over, "; "))
	}
	if stopped == 0 {
		t.Fatal("no page ended early at the bound; the test proves nothing about it")
	}
	if !reflect.DeepEqual(got, paged) {
		t.Fatalf("under the bound the read handed over %d hits, the pages %d, or another order", len(got), len(paged))
	}
}

// TestReadAnswerHydratesOnlyWhatItHandsOver proves a step is hydrated a page
// at a time: a caller that stops after the first page of the third step (8
// hits at pages of 2, over steps of 2, 4 and 8) costs the content store
// exactly the reads a wire page of those 8 hits costs, not the 14 hits the
// three steps hold.
//
// Mutation: hydrate the whole step before handing its first page over.
func TestReadAnswerHydratesOnlyWhatItHandsOver(t *testing.T) {
	f := newFixtureOf(t, pagingDocs())
	cas, ok := f.opts.Content.(*snapshot.CAS)
	if !ok {
		t.Fatalf("the fixture's content reader is %T, want the CAS", f.opts.Content)
	}
	counted := &countingCAS{cas: cas}
	opts := f.opts
	opts.Content = counted
	s := newService(t, opts)
	handed := 0
	_, err := s.ReadAnswer(f.ctx, pinned(t, f), model.SearchRequest{Query: "omega"}, 2, math.MaxInt64,
		func(page []model.SearchHit) (bool, error) {
			if len(page) > 2 {
				t.Fatalf("a page of %d hits was handed over, want at most 2", len(page))
			}
			handed += len(page)
			return handed < 8, nil
		})
	if err != nil {
		t.Fatalf("ReadAnswer: %v", err)
	}
	if handed != 8 {
		t.Fatalf("the read handed over %d hits before the caller stopped, want 8", handed)
	}
	read := counted.reads
	counted.reads = 0
	page, err := s.Search(f.ctx, model.SearchRequest{Query: "omega", Page: model.PageRequest{Limit: 8}})
	if err != nil || len(page.Items) != 8 {
		t.Fatalf("Search(limit 8) = %d hits, %v", len(page.Items), err)
	}
	if counted.reads == 0 {
		t.Fatal("hydrating 8 hits read nothing from the content store; the test proves nothing")
	}
	if read != counted.reads {
		t.Fatalf("the read stopped after 8 hits made %d content reads, a page of those 8 makes %d",
			read, counted.reads)
	}
}

// TestReadAnswerGrowsPastTheWirePage proves the read is in process: from the
// wire ceiling it asks the index for more than model.MaxPageItems results in
// one step, while Search still refuses a page over the ceiling.
//
// Mutation: route the read's depth through the wire clamp (Service.pageLimit),
// and no request asks for more than model.MaxPageItems+1.
func TestReadAnswerGrowsPastTheWirePage(t *testing.T) {
	const n = model.MaxPageItems + 5
	docs := make([]doc, 0, n)
	for i := range n {
		name := fmt.Sprintf("Fn%03d", i)
		docs = append(docs, doc{path: fmt.Sprintf("pkg/f%03d.go", i), name: name,
			qualifiedName: "pkg." + name, signature: "func " + name + "()",
			body: "func " + name + "() { omega }", occurrences: 1})
	}
	f := newFixtureOf(t, docs)
	s := newService(t, f.opts)
	got, reqs := readAll(t, f, s, "omega", model.MaxPageItems, math.MaxInt64)
	if len(got) != n {
		t.Fatalf("the read handed over %d hits, want %d", len(got), n)
	}
	deepest := 0
	for _, r := range reqs {
		deepest = max(deepest, r.Depth)
	}
	if deepest <= model.MaxPageItems+1 {
		t.Fatalf("the deepest index request asks for %d results; the in-process read must grow past the wire page %d",
			deepest, model.MaxPageItems)
	}
	_, err := s.Search(f.ctx, model.SearchRequest{Query: "omega", Page: model.PageRequest{Limit: model.MaxPageItems + 1}})
	assertCode(t, "a wire page over the ceiling", err, model.CodeArgumentInvalid)

	_, err = s.ReadAnswer(f.ctx, pinned(t, f), model.SearchRequest{Query: "omega",
		Page: model.PageRequest{Limit: model.MaxPageItems}}, model.MaxPageItems, math.MaxInt64,
		func([]model.SearchHit) (bool, error) { return true, nil })
	assertCode(t, "an in-process read given a page request", err, model.CodeInternal)
}
