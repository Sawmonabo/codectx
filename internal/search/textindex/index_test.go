package textindex

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
)

func openWritable(t *testing.T, dir string, searchThreads int) *Index {
	t.Helper()
	x, err := Open(Options{
		Dir: dir, Writable: true, WriterThreads: 1, WriterMemoryBytes: WriterMemoryFloor(1),
		MergeThreads: 1, SearchThreads: searchThreads, QuerySlots: 1, ViewCacheBytes: 64 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = x.Close() })
	return x
}

func openReadOnly(t *testing.T, dir string, searchThreads int) *Index {
	t.Helper()
	x, err := Open(Options{Dir: dir, SearchThreads: searchThreads, QuerySlots: 1, ViewCacheBytes: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = x.Close() })
	return x
}

// visibleOf is the bitmap holding doc ids first..last.
func visibleOf(first, last uint64) []byte {
	b := make([]byte, last/8+1)
	for id := first; id <= last; id++ {
		b[id/8] |= 1 << (id % 8)
	}
	return b
}

func bitmap(ids []uint64) []byte {
	var hi uint64
	for _, id := range ids {
		hi = max(hi, id)
	}
	b := make([]byte, hi/8+1)
	for _, id := range ids {
		b[id/8] |= 1 << (id % 8)
	}
	return b
}

// corpus is a generated fixture: units of documents, doc ids from 1.
type corpus struct {
	units []string
	docs  map[string][]Document
}

var syllables = []string{"ka", "lo", "mi", "ne", "pu", "ra", "si", "to", "vu", "ze", "bo", "di"}

// genCorpus builds n documents in units of about unitSize. Words follow a
// skewed distribution, lengths vary widely, about four in five documents
// share node ids with others (some across units), the rest are file
// documents keyed by path and start byte. The word "hot" is in every body,
// many times in a few, so its blocks carry very different bounds.
func genCorpus(seed uint64, n, unitSize int) corpus {
	rng := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	vocab := make([]string, 0, 240)
	for _, a := range syllables {
		for _, b := range syllables {
			vocab = append(vocab, a+b)
		}
	}
	word := func() string {
		u := rng.Float64()
		return vocab[int(float64(len(vocab))*u*u*u)]
	}
	camel := func() string {
		a, b := word(), word()
		return a + strings.ToUpper(b[:1]) + b[1:]
	}
	kinds := []model.NodeKind{model.NodeFunction, model.NodeMethod, model.NodeClass}
	c := corpus{docs: map[string][]Document{}}
	for i := 0; i < n; i++ {
		u := fmt.Sprintf("unit%03d", i/unitSize)
		if len(c.units) == 0 || c.units[len(c.units)-1] != u {
			c.units = append(c.units, u)
		}
		var body []string
		for k := 0; k < 2+rng.IntN(60); k++ {
			body = append(body, word())
		}
		hot := 1
		if rng.IntN(20) == 0 {
			hot = 2 + rng.IntN(12)
		}
		for k := 0; k < hot; k++ {
			body = append(body, "hot")
		}
		path := fmt.Sprintf("pkg%d/sub%d/f%d.go", rng.IntN(4), rng.IntN(4), rng.IntN(10))
		d := Document{
			DocID:     uint64(i + 1),
			SearchKey: fmt.Sprintf("sk%05d", i),
			FileID:    model.FileID("file:" + path),
			Path:      path,
			Bytes:     model.ByteRange{Start: uint64(rng.IntN(50)), End: uint64(50 + rng.IntN(50))},
			Body:      strings.Join(body, " "),
		}
		if rng.IntN(5) == 0 {
			d.Kind = model.NodeFile
		} else {
			d.NodeID = model.NodeID(fmt.Sprintf("node%d", rng.IntN(n/2)))
			d.Kind = kinds[rng.IntN(len(kinds))]
			d.Name = camel()
			d.QualifiedName = "pkg." + d.Name
			d.Signature = "func " + d.Name + "(" + word() + " int)"
		}
		c.docs[u] = append(c.docs[u], d)
	}
	return c
}

// build adds units to an index in dir, committing every unitsPerCommit units,
// then releases the writer. keep selects the units added (nil: all).
func (c corpus) build(t *testing.T, dir string, unitsPerCommit int, keep func(string) bool) {
	t.Helper()
	x := openWritable(t, dir, 1)
	ctx := context.Background()
	pending := 0
	for _, u := range c.units {
		if keep != nil && !keep(u) {
			continue
		}
		if err := x.AddUnit(ctx, u, c.docs[u]); err != nil {
			t.Fatal(err)
		}
		pending++
		if pending == unitsPerCommit {
			if err := x.Commit(ctx, "1"); err != nil {
				t.Fatal(err)
			}
			pending = 0
		}
	}
	if err := x.Commit(ctx, "1"); err != nil {
		t.Fatal(err)
	}
	if err := x.Close(); err != nil {
		t.Fatal(err)
	}
}

// visible is the bitmap of every document of the units keep selects.
func (c corpus) visible(keep func(string) bool) []byte {
	var ids []uint64
	for _, u := range c.units {
		if keep == nil || keep(u) {
			for _, d := range c.docs[u] {
				ids = append(ids, d.DocID)
			}
		}
	}
	return bitmap(ids)
}

// hiddenEvery hides every k-th unit (k = 0 hides nothing).
func hiddenEvery(k int) func(string) bool {
	return func(u string) bool {
		var n int
		fmt.Sscanf(u, "unit%d", &n)
		return k == 0 || n%k != 0
	}
}

func search(t *testing.T, v *View, r Request) Page {
	t.Helper()
	p, err := v.Search(context.Background(), r)
	if err != nil {
		t.Fatalf("search %+v: %v", r, err)
	}
	return p
}

// samePage reports how two pages of one request differ: their hits (every
// served field and the score) and whether more results exist. Occurrence
// counts are compared when both sides call theirs exact.
func samePage(a, b Page) error {
	if a.More != b.More {
		return fmt.Errorf("more %v against %v", a.More, b.More)
	}
	if len(a.Hits) != len(b.Hits) {
		return fmt.Errorf("%d hits against %d", len(a.Hits), len(b.Hits))
	}
	for i := range a.Hits {
		x, y := a.Hits[i], b.Hits[i]
		xo, yo := x.Occurrences, y.Occurrences
		bothExact := x.OccurrencesExact && y.OccurrencesExact
		x.Occurrences, y.Occurrences, x.OccurrencesExact, y.OccurrencesExact = 0, 0, false, false
		if x != y {
			return fmt.Errorf("hit %d: %+v against %+v", i, x, y)
		}
		if bothExact && xo != yo {
			return fmt.Errorf("hit %d: exact occurrences %d against %d", i, xo, yo)
		}
	}
	return nil
}

var engineQueries = []string{"hot", "kalo", "mika pune", "hot rasi", "kaloMika", "toVu zebo", "dibo"}

// TestPrunedPagesEqualExhaustiveScoring compares every pruned page with the
// exhaustive reference (IgnoreSummaries) on a fixture of several segments,
// for both length rules, pages 1 to 5 of 10 and of 200 results, with kind and
// path filters, with hidden units, at one and four search threads, and checks
// that pruning skipped blocks under every thread count, view and length rule,
// so no part of the comparison is vacuous. A block is skipped only once a
// segment's heap is full and the block's bound falls below the heap's worst
// score: on this fixture that happens on the pages of 10, while the pages of
// 200 fill their heaps late and every remaining block's bound still beats the
// worst score.
//
// Mutation caught: a block bound computed with the other length rule's
// minimum length (the bound then undercuts true scores and a page loses or
// reorders results).
func TestPrunedPagesEqualExhaustiveScoring(t *testing.T) {
	c := genCorpus(1, 6000, 100)
	dir := t.TempDir()
	c.build(t, dir, 20, nil)
	filters := []struct {
		kinds    []model.NodeKind
		prefixes []string
	}{
		{},
		{kinds: []model.NodeKind{model.NodeFunction, model.NodeMethod}},
		{prefixes: []string{"pkg1", "pkg3/sub2"}},
		{kinds: []model.NodeKind{model.NodeFile}, prefixes: []string{"pkg2"}},
	}
	for _, threads := range []int{1, 4} {
		x := openReadOnly(t, dir, threads)
		for gen, hide := range []int{0, 4} {
			v, err := x.OpenView(model.GenerationID(gen+1), c.visible(hiddenEvery(hide)))
			if err != nil {
				t.Fatal(err)
			}
			for _, rule := range []LengthRule{LengthTotal, LengthWeighted} {
				skipped := false
				for _, q := range engineQueries {
					for fi, f := range filters {
						for _, size := range []int{10, 200} {
							for pageNo := 1; pageNo <= 5; pageNo++ {
								r := Request{Query: q, Kinds: f.kinds, PathPrefixes: f.prefixes, Depth: size * pageNo, LengthRule: rule}
								pruned := search(t, v, r)
								r.IgnoreSummaries = true
								full := search(t, v, r)
								if err := samePage(pruned, full); err != nil {
									t.Fatalf("threads %d hide %d rule %d query %q filter %d depth %d: %v",
										threads, hide, rule, q, fi, r.Depth, err)
								}
								if pruned.Stats.SegmentsWithoutSummary != 0 {
									t.Fatalf("a committed segment has no summary: %+v", pruned.Stats)
								}
								if pruned.Stats.BlocksScored < pruned.Stats.BlocksTotal {
									skipped = true
								}
							}
						}
					}
				}
				if !skipped {
					t.Fatalf("threads %d hide %d rule %d: no search skipped a block, so the comparison proves nothing about pruning",
						threads, hide, rule)
				}
			}
			v.Close()
		}
	}
}

// TestPrunedPhrasePagesEqualExhaustiveScoring compares quoted-phrase pages
// with the exhaustive reference under both length rules.
//
// Mutation caught: a phrase term given the stored summary's block bound (the
// bound of its first token's single-term frequency, not of its exact phrase
// frequency), which mis-orders or wrongly skips blocks.
func TestPrunedPhrasePagesEqualExhaustiveScoring(t *testing.T) {
	c := genCorpus(2, 5000, 100)
	dir := t.TempDir()
	c.build(t, dir, 15, nil)
	x := openReadOnly(t, dir, 1)
	v, err := x.OpenView(1, c.visible(hiddenEvery(5)))
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	for _, rule := range []LengthRule{LengthTotal, LengthWeighted} {
		for _, q := range []string{`"hot hot"`, `"kalo kalo"`, `"func kaka"`, `"hot hot hot" kalo`, `mika "kaka kalo"`, `"pkg kakaKaka"`} {
			for pageNo := 1; pageNo <= 3; pageNo++ {
				r := Request{Query: q, Depth: 200 * pageNo, LengthRule: rule}
				pruned := search(t, v, r)
				r.IgnoreSummaries = true
				full := search(t, v, r)
				if err := samePage(pruned, full); err != nil {
					t.Fatalf("rule %d query %q page %d: %v", rule, q, pageNo, err)
				}
				if len(full.Hits) == 0 {
					t.Fatalf("query %q matched nothing: the fixture does not exercise phrases", q)
				}
			}
		}
	}
}

// TestHiddenUnitsRankLikeAnIndexOfTheVisibleOnes checks that a view with 10%
// and with 50% of its units hidden ranks exactly like a fresh index holding
// only the visible units' documents: the same hits with the same scores.
//
// Mutation caught: hidden documents counted in df (or in N or the length
// sums), which shifts every score of the view with hidden units.
func TestHiddenUnitsRankLikeAnIndexOfTheVisibleOnes(t *testing.T) {
	c := genCorpus(3, 4000, 50)
	allDir := t.TempDir()
	c.build(t, allDir, 16, nil)
	all := openReadOnly(t, allDir, 1)
	for gen, every := range []int{10, 2} {
		keep := hiddenEvery(every)
		onlyDir := t.TempDir()
		c.build(t, onlyDir, 9, keep)
		only := openReadOnly(t, onlyDir, 1)
		hv, err := all.OpenView(model.GenerationID(gen+1), c.visible(keep))
		if err != nil {
			t.Fatal(err)
		}
		ov, err := only.OpenView(1, c.visible(keep))
		if err != nil {
			t.Fatal(err)
		}
		for _, rule := range []LengthRule{LengthTotal, LengthWeighted} {
			for _, q := range engineQueries {
				for _, depth := range []int{200, 1000} {
					r := Request{Query: q, Depth: depth, LengthRule: rule}
					a, b := search(t, hv, r), search(t, ov, r)
					if err := samePage(a, b); err != nil {
						t.Fatalf("1 in %d units hidden, rule %d query %q depth %d: %v", every, rule, q, depth, err)
					}
					if a.MatchesExact && b.MatchesExact && a.Matches != b.Matches {
						t.Fatalf("1 in %d units hidden, query %q: %d matches against %d", every, q, a.Matches, b.Matches)
					}
				}
			}
		}
		hv.Close()
		ov.Close()
	}
}

func summaryFiles(t *testing.T, dir string) []string {
	t.Helper()
	m, err := filepath.Glob(filepath.Join(dir, "*.cxsum"))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// TestMergedSegmentIsSummarizedAndStaleSummariesAreNeverRead deletes
// documents, forces a merge of every segment and checks that the merged
// segment has a summary and its pages equal exhaustive scoring. It then makes
// one segment's summary file a copy of another segment's (its header names
// the other segment) and checks that the file is not read: the next open
// builds the segment's own summary in its place, and pages still equal
// exhaustive scoring.
//
// Mutation caught: skipping the summary rebuild on merge (the merged segment
// is then served without one), and reading a summary whose header names
// another segment (its bounds belong to other documents and skip blocks that
// hold results, and the open keeps the file).
func TestMergedSegmentIsSummarizedAndStaleSummariesAreNeverRead(t *testing.T) {
	ctx := context.Background()
	c := genCorpus(4, 3000, 100)
	compare := func(t *testing.T, v *View) Page {
		t.Helper()
		var last Page
		for _, rule := range []LengthRule{LengthTotal, LengthWeighted} {
			for _, q := range engineQueries {
				for _, depth := range []int{200, 600} {
					r := Request{Query: q, Depth: depth, LengthRule: rule}
					pruned := search(t, v, r)
					r.IgnoreSummaries = true
					if err := samePage(pruned, search(t, v, r)); err != nil {
						t.Fatalf("rule %d query %q depth %d: %v", rule, q, depth, err)
					}
					last = pruned
				}
			}
		}
		return last
	}

	t.Run("merge", func(t *testing.T) {
		dir := t.TempDir()
		c.build(t, dir, 10, nil)
		x := openWritable(t, dir, 1)
		var gone []uint64
		for id := uint64(1); id <= 3000; id += 7 {
			gone = append(gone, id)
		}
		if err := x.DeleteDocs(ctx, gone); err != nil {
			t.Fatal(err)
		}
		if err := x.Commit(ctx, "2"); err != nil {
			t.Fatal(err)
		}
		if err := x.mergeAll(ctx); err != nil {
			t.Fatal(err)
		}
		v, err := x.OpenView(2, visibleOf(1, 3000))
		if err != nil {
			t.Fatal(err)
		}
		defer v.Close()
		st := compare(t, v).Stats
		if st.Segments != 1 || st.SegmentsWithoutSummary != 0 {
			t.Fatalf("after the merge: %+v, want one summarized segment", st)
		}
		if files := summaryFiles(t, dir); len(files) != 1 {
			t.Fatalf("summary files after the merge: %v, want only the merged segment's", files)
		}
	})

	t.Run("stale", func(t *testing.T) {
		dir := t.TempDir()
		c.build(t, dir, 15, nil)
		x := openReadOnly(t, dir, 1)
		v, err := x.OpenView(1, visibleOf(1, 3000))
		if err != nil {
			t.Fatal(err)
		}
		st := compare(t, v).Stats
		v.Close()
		if err := x.Close(); err != nil {
			t.Fatal(err)
		}
		files := summaryFiles(t, dir)
		if st.Segments != 2 || st.SegmentsWithoutSummary != 0 || len(files) != 2 {
			t.Fatalf("before: %+v and summary files %v, want two summarized segments", st, files)
		}
		other, err := os.ReadFile(files[0])
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(files[1], other, 0o644); err != nil {
			t.Fatal(err)
		}
		x = openReadOnly(t, dir, 1)
		v, err = x.OpenView(1, visibleOf(1, 3000))
		if err != nil {
			t.Fatal(err)
		}
		defer v.Close()
		if st := compare(t, v).Stats; st.SegmentsWithoutSummary != 0 {
			t.Fatalf("the open did not rebuild the summary naming another segment: %+v", st)
		}
		if again, err := os.ReadFile(files[1]); err != nil || bytes.Equal(again, other) {
			t.Fatalf("the summary naming another segment is still in place (read error %v)", err)
		}
	})
}

// TestSummariesOfAnEarlierFormatAreRebuiltAtOpen gives every segment a summary
// file of an earlier format (its magic) and opens the index read-only, then
// writable: each open builds the summaries again, so the pages skip blocks
// and resumed pages skip segments on their key filters, and every page equals
// exhaustive scoring. An open whose directory refuses writes keeps the old
// files and scores every segment exhaustively, still exactly.
//
// Mutation caught: no summary build at open (every segment is served without
// a summary), and a loader that accepts the earlier format.
func TestSummariesOfAnEarlierFormatAreRebuiltAtOpen(t *testing.T) {
	c := genCorpus(1, 6000, 100)
	dir := t.TempDir()
	c.build(t, dir, 20, nil)
	age := func() {
		t.Helper()
		files := summaryFiles(t, dir)
		if len(files) < 3 {
			t.Fatalf("summary files %v, want one per segment of at least three", files)
		}
		for _, f := range files {
			b, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			copy(b, "CXBLKSUM")
			if err := os.WriteFile(f, b, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	// check serves every engine query page by page against exhaustive scoring
	// and returns the summed stats of the pruned pages.
	check := func(t *testing.T, x *Index) Stats {
		t.Helper()
		v, err := x.OpenView(1, visibleOf(1, 6000))
		if err != nil {
			t.Fatal(err)
		}
		defer v.Close()
		var sum Stats
		for _, q := range engineQueries {
			var at *Position
			for range 4 {
				r := Request{Query: q, Depth: 10, After: at}
				pruned := search(t, v, r)
				r.IgnoreSummaries = true
				if err := samePage(pruned, search(t, v, r)); err != nil {
					t.Fatalf("query %q: %v", q, err)
				}
				sum.Segments = pruned.Stats.Segments
				sum.SegmentsWithoutSummary = pruned.Stats.SegmentsWithoutSummary
				sum.BlocksTotal += pruned.Stats.BlocksTotal
				sum.BlocksScored += pruned.Stats.BlocksScored
				sum.KeySkips += pruned.Stats.KeySkips
				if !pruned.More || len(pruned.Hits) == 0 {
					break
				}
				a := pruned.Hits[len(pruned.Hits)-1].At()
				at = &a
			}
		}
		return sum
	}

	if os.Geteuid() != 0 {
		age()
		if err := os.Chmod(dir, 0o555); err != nil {
			t.Fatal(err)
		}
		x := openReadOnly(t, dir, 1)
		st := check(t, x)
		if err := x.Close(); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if st.SegmentsWithoutSummary != st.Segments || st.BlocksScored != st.BlocksTotal || st.KeySkips != 0 {
			t.Fatalf("an open that cannot write served summaries of the earlier format: %+v", st)
		}
	}
	for _, open := range []func(*testing.T, string, int) *Index{openReadOnly, openWritable} {
		age()
		x := open(t, dir, 1)
		st := check(t, x)
		if err := x.Close(); err != nil {
			t.Fatal(err)
		}
		if st.SegmentsWithoutSummary != 0 || st.BlocksScored >= st.BlocksTotal || st.KeySkips == 0 {
			t.Fatalf("after the open: %+v, want every segment summarized, blocks skipped and keys filtered", st)
		}
		for _, f := range summaryFiles(t, dir) {
			b, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			if bytes.HasPrefix(b, []byte("CXBLKSUM")) {
				t.Fatalf("%s keeps the earlier format", filepath.Base(f))
			}
		}
	}
}

// TestPanicInsideAnExportReturnsAnError panics inside the library's export
// guard and checks that the call returns an internal error naming the panic
// and that the process carries on: a second call behaves the same.
//
// Mutation caught: an export without the panic guard (the unwind reaches the
// C boundary and aborts the test process).
func TestPanicInsideAnExportReturnsAnError(t *testing.T) {
	for range 2 {
		err := panicSelfTest()
		var e *model.Error
		if !errors.As(err, &e) || e.Code != model.CodeInternal || !strings.Contains(e.Message, "panicked on purpose") {
			t.Fatalf("panic self-test returned %v, want an internal error naming the panic", err)
		}
	}
}

// TestResumedPagesEqualTheDeepReadsSuffix compares every resumed page with the
// suffix of one deep read: for positions drawn at random and at the edges of
// tied-score runs, Search(After: p, Depth: d) holds exactly the d results the
// deep read ranks after p, with More exactly when more follow, under both
// length rules, with and without a kind filter, over several segments with
// hidden units, where about four in five documents share a node with others
// (so a node's best document can sit before the position while another of its
// documents scores after it). Each resumed page is also compared with the same
// request scored exhaustively (IgnoreSummaries). A position whose document the
// view does not hold is refused as ErrUnknownPosition.
//
// Mutation caught: a position compared by score alone (a tied run split across
// two pages loses or repeats its members at the edge).
// Mutation caught: a position applied per document rather than per folded
// result (a node served before the position comes back through a lower-scoring
// document of it).
//
// Every page's block counts cover the same scoring passes: the exhaustive page
// scores every block it counts, and the pruned page never scores more than it
// counts, on pages that rescored (a heap grown past depth+1) as on the rest.
//
// The pruned pages skip segments on their key filters (KeySkips) where the
// exhaustive reference looks every key up, and serve the same results.
//
// Mutation caught: blocks_total counted over the last pass while
// blocks_scored sums every pass (a rescored exhaustive page then reports more
// blocks scored than exist).
// Mutation caught: a key filter built from or probed with the wrong column's
// values (it rejects a segment that marked the group, and a group served
// before the position comes back).
func TestResumedPagesEqualTheDeepReadsSuffix(t *testing.T) {
	c := genCorpus(4, 5000, 100)
	dir := t.TempDir()
	c.build(t, dir, 15, nil)
	x := openReadOnly(t, dir, 2)
	v, err := x.OpenView(1, c.visible(hiddenEvery(4)))
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	rng := rand.New(rand.NewPCG(4, 4))
	ties, rescored := 0, 0
	var skips, lookups int64
	for _, rule := range []LengthRule{LengthTotal, LengthWeighted} {
		for _, kinds := range [][]model.NodeKind{nil, {model.NodeFunction, model.NodeFile}} {
			for _, q := range engineQueries {
				deep := search(t, v, Request{Query: q, Kinds: kinds, Depth: 1 << 20, LengthRule: rule})
				if deep.More || len(deep.Hits) < 2 {
					t.Fatalf("query %q: the deep read holds %d hits (more %v); the test needs the whole ranking",
						q, len(deep.Hits), deep.More)
				}
				var at []int
				for i := 0; i+1 < len(deep.Hits) && len(at) < 6; i++ {
					if deep.Hits[i].ScoreMicros == deep.Hits[i+1].ScoreMicros {
						at = append(at, i)
						ties++
					}
				}
				at = append(at, 0, len(deep.Hits)-1)
				for range 8 {
					at = append(at, rng.IntN(len(deep.Hits)))
				}
				for _, i := range at {
					p := deep.Hits[i].At()
					for _, d := range []int{1, 7, 60} {
						end := min(i+1+d, len(deep.Hits))
						want := Page{Hits: deep.Hits[i+1 : end], More: end < len(deep.Hits)}
						r := Request{Query: q, Kinds: kinds, Depth: d, LengthRule: rule, After: &p}
						got := search(t, v, r)
						if err := samePage(got, want); err != nil {
							t.Fatalf("rule %d kinds %v query %q after hit %d depth %d: %v", rule, kinds, q, i, d, err)
						}
						skips += got.Stats.KeySkips
						lookups += got.Stats.KeyLookups
						if got.Stats.BlocksScored > got.Stats.BlocksTotal {
							t.Fatalf("query %q after hit %d depth %d: %d blocks scored of %d", q, i, d,
								got.Stats.BlocksScored, got.Stats.BlocksTotal)
						}
						r.IgnoreSummaries = true
						full := search(t, v, r)
						if err := samePage(full, want); err != nil {
							t.Fatalf("rule %d kinds %v query %q after hit %d depth %d, exhaustive: %v",
								rule, kinds, q, i, d, err)
						}
						if full.Stats.BlocksScored != full.Stats.BlocksTotal {
							t.Fatalf("query %q after hit %d depth %d, exhaustive: %d blocks scored of %d (cap %d)",
								q, i, d, full.Stats.BlocksScored, full.Stats.BlocksTotal, full.Stats.Cap)
						}
						if full.Stats.KeySkips != 0 {
							t.Fatalf("query %q after hit %d depth %d, exhaustive: %d segments skipped on a key filter",
								q, i, d, full.Stats.KeySkips)
						}
						if full.Stats.Cap > int64(d)+1 {
							rescored++
						}
					}
				}
			}
		}
	}
	if ties == 0 {
		t.Fatal("no tied scores in the fixture: the test proves nothing about ties at a page edge")
	}
	if rescored == 0 {
		t.Fatal("no page rescored: the test proves nothing about block counts across passes")
	}
	if skips == 0 || lookups == 0 {
		t.Fatalf("key filters skipped %d segments and %d keys were looked up: the test needs both", skips, lookups)
	}
	t.Logf("key filters skipped %d segments; %d keys were looked up", skips, lookups)

	// A hidden document is not a position of this view.
	hidden := c.docs[c.units[0]][0].DocID
	_, err = v.Search(context.Background(), Request{Query: "hot", Depth: 5, After: &Position{ScoreMicros: 1, DocID: hidden}})
	if !errors.Is(err, ErrUnknownPosition) {
		t.Fatalf("a position naming a hidden document returned %v, want ErrUnknownPosition", err)
	}
}

// TestABackgroundMergesSegmentIsSummarizedBeforeItServes commits most units
// in one segment, then eight units one commit each into a writer that stays
// open: the eighth commit lets the merge policy merge the eight small segments
// in the background and publish the merged segment with no commit of ours
// after it. Views are opened until one shows the merge; its pages must already
// find a summary for every segment, skip blocks and skip segments on their key
// filters, and equal exhaustive scoring.
//
// Mutation caught: summaries built only at commits, releases and opens (the
// merged segment serves without one until the writer's next commit).
func TestABackgroundMergesSegmentIsSummarizedBeforeItServes(t *testing.T) {
	ctx := context.Background()
	c := genCorpus(1, 6000, 100)
	dir := t.TempDir()
	x := openWritable(t, dir, 1)
	const per = 3
	small := len(c.units) - 8*per
	add := func(units []string) {
		for _, u := range units {
			if err := x.AddUnit(ctx, u, c.docs[u]); err != nil {
				t.Fatal(err)
			}
		}
		if err := x.Commit(ctx, "1"); err != nil {
			t.Fatal(err)
		}
	}
	add(c.units[:small])
	for i := small; i < len(c.units); i += per {
		add(c.units[i : i+per])
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		v, err := x.OpenView(1, visibleOf(1, 6000))
		if err != nil {
			t.Fatal(err)
		}
		probe := search(t, v, Request{Query: "hot", Depth: 10})
		if probe.Stats.Segments < 9 {
			var skipped bool
			var skips int64
			for _, q := range engineQueries {
				var at *Position
				for range 4 {
					r := Request{Query: q, Depth: 10, After: at}
					pruned := search(t, v, r)
					r.IgnoreSummaries = true
					if err := samePage(pruned, search(t, v, r)); err != nil {
						t.Fatalf("query %q: %v", q, err)
					}
					if pruned.Stats.SegmentsWithoutSummary != 0 {
						t.Fatalf("a merged segment served without a summary: %+v", pruned.Stats)
					}
					skipped = skipped || pruned.Stats.BlocksScored < pruned.Stats.BlocksTotal
					skips += pruned.Stats.KeySkips
					if !pruned.More || len(pruned.Hits) == 0 {
						break
					}
					a := pruned.Hits[len(pruned.Hits)-1].At()
					at = &a
				}
			}
			v.Close()
			if !skipped || skips == 0 {
				t.Fatalf("after the merge no page skipped a block (%v) or a segment on its key filter (%d)", skipped, skips)
			}
			return
		}
		v.Close()
		if time.Now().After(deadline) {
			t.Fatalf("no background merge published within 30s: %d segments", probe.Stats.Segments)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
