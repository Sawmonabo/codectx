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
// budget, the bytes one step may hold (stepBytes): resuming costs the library a
// rescoring of the ranking above the position, so pages of a fixed size make
// reading N results cost about N²/(2·first) document scorings and doubling makes
// it about 4N. When the next doubling does not fit, the read keeps the largest
// depth that does, never less than first, so a small budget costs more steps
// and never a refused or different answer.
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
	if err := filter.pin(ctx, s.store, reader.Binding().SnapshotID); err != nil {
		return end, err
	}
	var pos searchPosition
	var served int64
	depth := first
	sizer := stepSizer{first: first, budget: budget}
	for {
		searchCtx, searchCancel := model.QueryDeadline(ctx, s.timeout)
		ans, err := s.collect(searchCtx, reader, req, filter, pos, depth)
		searchCancel()
		if err != nil {
			return end, err
		}
		if ans.lexical != nil {
			s.log.Debug("read a step of a search answer in process", "component", "search", "depth", depth,
				"hits", len(ans.hits), "segments", ans.lexical.Stats.Segments, "heaps", ans.lexical.Stats.Heaps,
				"cap", ans.lexical.Stats.Cap, "marked", ans.lexical.Stats.Marked,
				"docs_scored", ans.lexical.Stats.DocsScored)
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
		sizer.observe(ans.lexical, depth, served, served+int64(len(ans.hits)))
		served += int64(len(ans.hits))
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

// stepSizer admits the depth of each step of an in-process read against the
// bytes one step may hold.
//
// One step of depth d asks the index for d+1 results; the library gives every
// segment it scores a heap of d+2 places, doubled on each rescoring a marked
// group forces, and the resume position marks the groups served before it.
// The step's peak is stepBytes. Every input is what the index reported for the
// read's last step: the segment count, how many segments it scored at once,
// the heap places per segment over the depth asked, the marked groups per
// result served, and the largest mean string bytes of a result. Until an index
// page has been read nothing is known about the library's side and the depth
// stays where it started.
type stepSizer struct {
	first    int
	budget   int64
	shape    stepShape
	measured bool
}

// stepShape is what one step's memory scales with.
type stepShape struct {
	segments, heaps int64
	// capNum/capDen is the heap places per segment over the depth+2 asked.
	capNum, capDen int64
	// markedNum/markedDen is the marked groups per result served before the
	// step; served is the results served before the next step.
	markedNum, markedDen int64
	served               int64
	strBytes             int64
}

// observe takes the measurements of the last index page of a step of depth d
// that began after `before` results and ended after `after`.
func (z *stepSizer) observe(p *textindex.Page, d int, before, after int64) {
	if p == nil {
		return
	}
	sh := &z.shape
	z.measured = true
	sh.segments = max(sh.segments, p.Stats.Segments, 1)
	sh.heaps = max(sh.heaps, min(p.Stats.Heaps, sh.segments), 1)
	if p.Stats.Cap > 0 && (sh.capDen == 0 || p.Stats.Cap*sh.capDen > sh.capNum*(int64(d)+2)) {
		sh.capNum, sh.capDen = max(p.Stats.Cap, int64(d)+2), int64(d)+2
	}
	if before > 0 && p.Stats.Marked > 0 && (sh.markedDen == 0 || p.Stats.Marked*sh.markedDen > sh.markedNum*before) {
		sh.markedNum, sh.markedDen = p.Stats.Marked, before
	}
	sh.served = after
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

// The library's memory, computed from the structures of src/search.rs on a
// 64-bit target.
const (
	// heapPlaceBytes is one heap place of a segment being scored: an OrdKey
	// (a score and nine u64 ranks plus a u32 doc: 88 bytes) in a BTreeSet,
	// whose nodes hold eleven keys in 984 bytes and are at least half full (at
	// most 200 bytes a key), and in a HashMap from the fold group (24 bytes)
	// to a Slot (104 bytes), whose 128-byte buckets run at 7/8 load and
	// double (at most 296 bytes an entry); and the 24-byte run entry the heap
	// becomes, written before the heap is freed. 200+296+24 = 520.
	heapPlaceBytes = 520
	// runEntryBytes is one place of a scored segment's run, held until the
	// merge: a score, a count, a doc and a flag.
	runEntryBytes = 24
	// keyPlaceBytes is one run entry's fold key while the occurrence counts
	// are summed, one segment at a time: its doc and two 24-byte string
	// headers, beside the key's string bytes.
	keyPlaceBytes = 56
	// resultBytes is one served result in the merge, beside its strings: the
	// met-group set (a 32-byte key, 76 bytes a bucket at worst), the merged
	// entry (280), its strings slot (224), the key index (94), the per-segment
	// request (24), the count (16), and the 240-byte result twice as the page
	// is assembled. Rounded up from 1194.
	resultBytes = 1200
	// resultStringCopies is how many copies of a served result's strings the
	// library holds: the result's own, its key in the met-group set, and its
	// key in the merged entry and the key index.
	resultStringCopies = 3
	// markedBytes is one group the resume position marked: a 24-byte entity
	// in a HashSet whose buckets run at 7/8 load and double.
	markedBytes = 58
)

// encodedHitOverhead is the bytes one result takes in the encoded page beyond
// its strings: the doc id, eight string lengths, the byte range, the score and
// the count as varints, and the exactness flag (at most 10+8·3+20+10+10+1 = 75).
const encodedHitOverhead = 80

// goResultBytes is what one result of a step costs on this side: the encoded
// page twice (the library's buffer, which grows by doubling, and the copy cgo
// hands over) and a third time as the decoded strings; the decoded index hit
// and its kept copy; the served hit and the copy hydration fills; the span, the
// source range and the reason the served hit carries; and hydration's index and
// span lists.
func goResultBytes(strBytes int64) int64 {
	encoded := strBytes + encodedHitOverhead
	return 2*encoded + strBytes +
		2*int64(unsafe.Sizeof(textindex.Hit{})) +
		2*int64(unsafe.Sizeof(model.SearchHit{})) +
		int64(unsafe.Sizeof(model.ByteRange{})) + int64(unsafe.Sizeof(model.SourceRange{})) +
		int64(unsafe.Sizeof("")) + int64(len(reasonFor(model.TierLexical))) +
		int64(unsafe.Sizeof(0)) + int64(unsafe.Sizeof(model.ByteRange{}))
}

// stepBytes is the peak memory of one step of depth d over an index of the
// given shape: the heaps of the segments scored at once and every segment's
// run, each of d+2 places scaled by the rescoring the last step needed; one
// segment's fold keys; the d+1 results served, on both sides of the boundary;
// and the groups the resume position will have marked. It saturates rather
// than overflows.
func stepBytes(sh stepShape, d int) int64 {
	places := int64(d) + 2
	if sh.capDen > 0 {
		places = mulSat(places, sh.capNum) / sh.capDen
	}
	perPlace := addSat(mulSat(sh.segments, runEntryBytes), mulSat(sh.heaps, heapPlaceBytes))
	perPlace = addSat(perPlace, keyPlaceBytes+sh.strBytes)
	perResult := resultBytes + resultStringCopies*sh.strBytes + goResultBytes(sh.strBytes)
	total := addSat(mulSat(places, perPlace), mulSat(int64(d)+1, perResult))
	if sh.markedDen > 0 {
		marked := mulSat(addSat(sh.served, int64(d)), sh.markedNum) / sh.markedDen
		total = addSat(total, mulSat(marked, markedBytes))
	}
	return total
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
