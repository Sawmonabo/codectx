package search

// The in-process read of one search answer (ADR-0016 decision 3): the context
// compiler reads the lexical matches of a task in score order down to its own
// stop, through this call rather than through wire pages.

import (
	"context"
	"math"
	"math/bits"
	"unsafe"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/search/textindex"
	"github.com/Sawmonabo/codectx/internal/storage/sqlite"
)

// ReadEnd is how an in-process read ended.
type ReadEnd struct {
	// Truncated says a query deadline ended the read: the hits handed over are
	// the ones found before it, and the answer may hold more.
	Truncated bool
	// Stuck says a step served nothing and named no later position, so the
	// read could not advance; the answer may hold more.
	Stuck bool
}

// ReadAnswer reads req's whole answer in served order over the caller's pinned
// reader, handing its hits to yield in pages of `first` until yield answers
// false, the answer ends, or a deadline ends it. It is the sequence Search serves page by
// page -- the exact tiers, then the lexical results less the nodes the exact
// tiers served -- with the same hits, the same order and the same ties; only
// the cost differs.
//
// It is an in-process call, so the wire page bound does not apply to it: req's
// Page must be empty, and the read carries its position in memory instead of a
// signed cursor. A step's hits are hydrated and validated one page at a time,
// just before that page is handed over, so a caller that stops mid-step reads
// nothing from storage past the page it stopped on, and hydration holds one
// page's blobs at a time. The first step reads `first` results. Each later step resumes
// after the last result served and asks for twice as many, while the step fits
// budget, the bytes one step may hold: resuming costs the library a
// rescoring of the ranking above the position, so pages of a fixed size make
// reading N results cost about N²/(2·first) document scorings and doubling makes
// it about 4N. The budget is a bound, not an estimate: every index request of a
// step carries it, and the library fits its heaps to it and ends a page early,
// with an exact prefix, rather than hold more. Before a step the read admits
// the largest of 2d, d, d/2, ... whose computed peak (stepBytes) fits, never
// less than first, so a small budget costs more steps and never a refused or
// different answer.
//
// resources.query_timeout, when set, bounds each step's candidate search, as it
// bounds each page of Search; a step it ends hands over what it found and ends
// the read with Truncated.
func (s *Service) ReadAnswer(ctx context.Context, reader *sqlite.PinnedReader, req model.SearchRequest, first int,
	budget int64, yield func([]model.SearchHit) (bool, error)) (ReadEnd, error) {
	var end ReadEnd
	if req.Page != (model.PageRequest{}) {
		return end, &model.Error{Code: model.CodeInternal,
			Message: "search: an in-process read carries its position itself and takes no page request"}
	}
	if first <= 0 {
		return end, &model.Error{Code: model.CodeInternal, Message: "search: an in-process read starts at a positive depth"}
	}
	req.GenerationID = reader.Binding().GenerationID
	if err := req.Validate(); err != nil {
		return end, err
	}
	filter, err := newHitFilter(req)
	if err != nil {
		return end, err
	}
	if err := filter.pin(ctx, s.store, reader, reader.Binding().SnapshotID); err != nil {
		return end, err
	}
	var pos searchPosition
	depth := first
	sizer := stepSizer{first: first, budget: budget}
	for {
		searchCtx, searchCancel := model.QueryDeadline(ctx, s.timeout)
		// The in-process read holds no cursor, so it neither reads nor writes a
		// spool: each step walks the exact tiers from its position, and the
		// steps double, so the walks cost about N·log(N/first) in all.
		ans, err := s.collect(searchCtx, reader, req, filter, pos, depth,
			pageBound{maxBytes: max(budget, 1), markedBound: sizer.shape.marked}, exactSource{})
		searchCancel()
		if err == nil {
			err = filter.tag.Err()
		}
		if err != nil {
			return end, err
		}
		if ans.lexical != nil {
			s.log.Debug("read a step of a search answer in process", "component", "search", "depth", depth,
				"hits", len(ans.hits), "segments", ans.lexical.Stats.Segments, "heaps", ans.lexical.Stats.Heaps,
				"cap", ans.lexical.Stats.Cap, "marked", ans.lexical.Stats.Marked,
				"bytes", ans.lexical.Stats.Bytes, "docs_scored", ans.lexical.Stats.DocsScored)
		}
		more, err := s.handOver(ctx, reader, ans, first, yield)
		if err != nil || !more {
			return end, err
		}
		if ans.deadline {
			end.Truncated = true
			return end, nil
		}
		if ans.next == nil {
			return end, nil
		}
		if len(ans.hits) == 0 && samePosition(*ans.next, pos) {
			end.Stuck = true
			return end, nil
		}
		sizer.observe(ans.lexical)
		pos = *ans.next
		depth = sizer.next(depth)
	}
}

// handOver hydrates, validates and yields one step's hits a page of `size` at
// a time, and answers whether the caller wants more.
func (s *Service) handOver(ctx context.Context, reader *sqlite.PinnedReader, ans searchAnswer, size int,
	yield func([]model.SearchHit) (bool, error)) (bool, error) {
	if len(ans.hits) == 0 {
		return yield(nil)
	}
	for from := 0; from < len(ans.hits); from += size {
		to := min(from+size, len(ans.hits))
		page, err := s.hydrated(ctx, reader, ans.hits[from:to], ans.spans[from:to])
		if err != nil {
			return false, err
		}
		for i := range page {
			if err := page[i].Validate(); err != nil {
				return false, err
			}
		}
		more, err := yield(page)
		if err != nil || !more {
			return false, err
		}
	}
	return true, nil
}

// samePosition reports whether two positions name the same place.
func samePosition(a, b searchPosition) bool {
	if a.phase != b.phase || a.rank != b.rank || a.node != b.node || (a.after == nil) != (b.after == nil) {
		return false
	}
	return a.after == nil || *a.after == *b.after
}

// pageBound is what one step's index requests are held to: the bytes the
// step may hold (zero: unbounded, as a wire page is) and an upper bound on the
// groups the resume position marks.
type pageBound struct {
	maxBytes, markedBound int64
}

// stepSizer admits the depth of each step of an in-process read against the
// bytes one step may hold.
//
// The library enforces the bound on every request; the sizer only picks the
// depth to ask for, so a step is not cut short by the bound it was sized for.
// Its inputs are what the index reported for the read's last index page: the
// segment count, how many segments it scored at once, the bound on the groups
// the next position marks (exact, not extrapolated: every group a later
// position marks is either marked already or a run entry the page took), and
// the largest mean string bytes of a result. Until an index page has been read
// the depth stays where it started.
type stepSizer struct {
	first    int
	budget   int64
	shape    stepShape
	measured bool
}

// stepShape is what one step's memory scales with.
type stepShape struct {
	segments, heaps int64
	// marked bounds the groups the next step's resume position marks.
	marked   int64
	strBytes int64
}

// observe takes the measurements of the last index page of a step.
func (z *stepSizer) observe(p *textindex.Page) {
	if p == nil {
		return
	}
	sh := &z.shape
	z.measured = true
	sh.segments = max(sh.segments, p.Stats.Segments, 1)
	sh.heaps = max(sh.heaps, min(p.Stats.Heaps, sh.segments), 1)
	sh.marked = p.Stats.NewMarks
	if len(p.Hits) > 0 {
		var sum int64
		for _, h := range p.Hits {
			sum += hitStringBytes(h)
		}
		sh.strBytes = max(sh.strBytes, sum/int64(len(p.Hits)))
	}
}

// next answers the depth of the step after one of depth d: the largest of 2d,
// d, d/2, ... that fits the budget, and never less than first.
func (z *stepSizer) next(d int) int {
	if !z.measured || z.budget <= 0 {
		return d
	}
	n := d
	if n <= math.MaxInt/2 {
		n *= 2
	}
	for n > z.first && stepBytes(z.shape, n) > z.budget {
		n /= 2
	}
	return max(n, z.first)
}

// hitStringBytes is the bytes of a result's strings.
func hitStringBytes(h textindex.Hit) int64 {
	return int64(len(h.SearchKey) + len(h.NodeID) + len(h.FileID) + len(h.Path) + len(h.Kind) + len(h.Name) +
		len(h.QualifiedName) + len(h.Signature))
}

// The library's charges, the constants of src/search.rs (where each is
// derived from its structures) on a 64-bit target: a heap place of a segment
// being scored, a run place of every segment, a marked group, and a served
// result beyond its strings, with the copies of its strings the library and
// the caller hold.
const (
	heapPlaceBytes     = 520
	runEntryBytes      = 24
	markedBytes        = 58
	resultBytes        = 1095
	resultStringCopies = 7
	// callerStringCopies is the caller's share of resultStringCopies: the
	// copy cgo hands over and the decoded strings.
	callerStringCopies = 2
)

// encodedHitOverhead is the bytes one result takes in the encoded page beyond
// its strings: the doc id, eight string lengths, the byte range, the score and
// the count as varints, and the exactness flag (at most 10+8·3+20+10+10+1 = 75).
const encodedHitOverhead = 80

// callerResultBytes is what this side holds per result of a step beyond the
// result's strings, which the library charges against the step's bound: the
// encoded fields of the copy cgo hands over; the decoded index hit and its
// kept copy; the served hit and the copy hydration fills; the span and the
// source range the served hit carries; and hydration's index and span lists.
func callerResultBytes() int64 {
	return encodedHitOverhead +
		2*int64(unsafe.Sizeof(textindex.Hit{})) +
		2*int64(unsafe.Sizeof(model.SearchHit{})) +
		int64(unsafe.Sizeof(model.ByteRange{})) + int64(unsafe.Sizeof(model.SourceRange{})) +
		int64(unsafe.Sizeof(0)) + int64(unsafe.Sizeof(model.ByteRange{}))
}

// stepBytes is the peak the library charges one step of depth d over an index
// of the given shape: the larger of its scoring peak (the heaps of the segments
// scored at once and every segment's run, each of d+2 places, beside the marked
// groups) and its merge peak (the runs and marked groups beside the d+1 served
// results, the caller's copies included). It saturates rather than overflows.
func stepBytes(sh stepShape, d int) int64 {
	places := int64(d) + 2
	marked := mulSat(sh.marked, markedBytes)
	runs := mulSat(places, mulSat(sh.segments, runEntryBytes))
	scoring := addSat(addSat(runs, mulSat(places, mulSat(sh.heaps, heapPlaceBytes))), marked)
	perResult := addSat(resultBytes+callerResultBytes(), mulSat(resultStringCopies, sh.strBytes))
	merging := addSat(addSat(runs, marked), mulSat(int64(d)+1, perResult))
	return max(scoring, merging)
}

// addSat adds two non-negative numbers, saturating at math.MaxInt64.
func addSat(a, b int64) int64 {
	if a > math.MaxInt64-b {
		return math.MaxInt64
	}
	return a + b
}

// mulSat multiplies two non-negative numbers, saturating at math.MaxInt64.
func mulSat(a, b int64) int64 {
	hi, lo := bits.Mul64(uint64(a), uint64(b))
	if hi != 0 || lo > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(lo)
}
