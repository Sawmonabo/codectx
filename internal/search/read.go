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
	// Stuck says a step served nothing and named no later position, so the
	// read could not advance; the answer may hold more.
	Stuck bool
}

// ReadAnswer reads req's whole answer in served order over the caller's pinned
// reader, handing each step's hits to yield until yield answers false, the
// answer ends, or a deadline ends it. It is the sequence Search serves page by
// page -- the exact tiers, then the lexical results less the nodes the exact
// tiers served -- with the same hits, the same order and the same ties; only
// the cost differs.
//
// It is an in-process call, so the wire page bound does not apply to it: req's
// Page must be empty, and the read carries its position in memory instead of a
// signed cursor. The first step reads `first` results. Each later step resumes
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
// the read, as a deadline ends Search's answer with no continuation.
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
	depth := first
	sizer := stepSizer{first: first, budget: budget}
	for {
		searchCtx, searchCancel := model.QueryDeadline(ctx, s.timeout)
		ans, err := s.collect(searchCtx, reader, req, filter, pos, depth)
		searchCancel()
		if err != nil {
			return end, err
		}
		hits, err := s.hydrated(ctx, reader, ans.hits, ans.spans)
		if err != nil {
			return end, err
		}
		for i := range hits {
			if err := hits[i].Validate(); err != nil {
				return end, err
			}
		}
		if ans.lexical != nil {
			s.log.Debug("read a step of a search answer in process", "component", "search", "depth", depth,
				"hits", len(hits), "segments", ans.lexical.Stats.Segments, "docs_scored", ans.lexical.Stats.DocsScored)
		}
		more, err := yield(hits)
		if err != nil || !more {
			return end, err
		}
		if ans.next == nil {
			return end, nil
		}
		if len(hits) == 0 && samePosition(*ans.next, pos) {
			end.Stuck = true
			return end, nil
		}
		pos = *ans.next
		sizer.observe(ans.lexical)
		depth = sizer.next(depth)
	}
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
// One step of depth d asks the index for d+1 results and the library keeps
// d+2 places in every segment's heap; the step's peak is stepBytes. The
// segment count is the one the last index page reported, and a result's string
// bytes are the largest mean the read has met over its index pages, so the
// estimate follows the index being read rather than a guess about it. Until an
// index page has been read nothing is known about the library's side and the
// depth stays where it started.
type stepSizer struct {
	first    int
	budget   int64
	segments int64
	strBytes int64
}

// observe takes the measurements of the last index page a step read.
func (z *stepSizer) observe(p *textindex.Page) {
	if p == nil {
		return
	}
	z.segments = max(z.segments, p.Stats.Segments, 1)
	if len(p.Hits) == 0 {
		return
	}
	var sum int64
	for _, h := range p.Hits {
		sum += hitStringBytes(h)
	}
	z.strBytes = max(z.strBytes, sum/int64(len(p.Hits)))
}

// next answers the depth of the step after one of depth d: the largest of 2d,
// d, d/2, ... that fits the budget, and never less than first.
func (z *stepSizer) next(d int) int {
	if z.segments == 0 || z.budget <= 0 {
		return d
	}
	n := d
	if n <= math.MaxInt/2 {
		n *= 2
	}
	for n > z.first && stepBytes(z.segments, n, z.strBytes) > z.budget {
		n /= 2
	}
	return max(n, z.first)
}

// hitStringBytes is the bytes of a result's strings.
func hitStringBytes(h textindex.Hit) int64 {
	return int64(len(h.SearchKey) + len(h.NodeID) + len(h.FileID) + len(h.Path) + len(h.Kind) + len(h.Name) +
		len(h.QualifiedName) + len(h.Signature))
}

// The library's cost of one heap place of one segment, computed from the
// structures of src/search.rs on a 64-bit target. A place is an OrdKey (a score
// and nine u64 ranks plus a u32 doc: 88 bytes) held twice while the segment is
// scored: in a BTreeSet, whose nodes hold eleven keys in 984 bytes and are at
// least half full, so at most 200 bytes a key, and in a HashMap from the fold
// group (24 bytes) to a Slot (104 bytes), whose 128-byte buckets run at 7/8
// load and double, so at most 296 bytes an entry. Resolving the heap adds the
// Slot list (104), the per-column string table (8 columns of 24-byte headers:
// 192), the resolved tuple (a 224-byte Hit with its count, flag and group: 264)
// and the segment's kept list (240). The fold across segments keys a HashMap by
// the group (32 bytes) to a cloned Hit with its counts (240), 622 bytes a
// bucket at worst, and sorts them in a list of 240-byte entries. Together:
// 200+296+104+192+264+240+622+240 = 2158, rounded up.
const libraryPlaceBytes = 2160

// libraryStringCopies is how many copies of a result's strings the library
// holds at its peak: the resolved hit, its clone in the fold, and the fold key.
const libraryStringCopies = 3

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
// given segment count whose results carry strBytes of strings on average:
// every segment's heap of d+2 places, and the d+1 results the step hands over.
// It saturates rather than overflows.
func stepBytes(segments int64, d int, strBytes int64) int64 {
	place := libraryPlaceBytes + libraryStringCopies*strBytes
	library := mulSat(mulSat(segments, int64(d)+2), place)
	served := mulSat(int64(d)+1, goResultBytes(strBytes))
	if library > math.MaxInt64-served {
		return math.MaxInt64
	}
	return library + served
}

// mulSat multiplies two non-negative numbers, saturating at math.MaxInt64.
func mulSat(a, b int64) int64 {
	hi, lo := bits.Mul64(uint64(a), uint64(b))
	if hi != 0 || lo > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(lo)
}
