package search

import (
	"cmp"
	"container/heap"
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
	"github.com/Sawmonabo/codectx/internal/source"
)

// ranked is one candidate in the external sort's record: the Section 14.2 sort
// tuple, the rowid that hydrates it, and the folded occurrence count. Nothing
// else -- carrying name/qualified name/signature on the ORDERING tuple would
// size every comparison by the widest symbol in the corpus; the servable facts
// ride alongside on scored instead. Ordering compares only
// integers and exact strings; no float reaches a comparison (digest §4).
type ranked struct {
	Tier        model.SearchTier
	ScoreMicros int64
	Path        string
	StartByte   uint64
	NodeID      model.NodeID
	SearchKey   string
	RowID       int64
	Occurrences int64
}

// cmpBounded compares two candidates on the bounded part of the Section 14.2
// tie-break order: tier rank, descending ScoreMicros, path, start byte. It
// returns 0 when those four keys are equal; cmpScored below breaks that tie on
// the candidate's content, and only then on the identity keys.
//
// Every comparison is on an int, an int64, a uint64 or an exact string in Go's
// byte order. The struct carries no float field at all, which is what makes
// "nothing compares floats" (digest §4) structural rather than a convention a
// later edit could break: a score reaches this function only after
// quantizeScore has turned it into an int64.
func (a ranked) cmpBounded(b ranked) int {
	if ar, br := a.Tier.Rank(), b.Tier.Rank(); ar != br {
		return cmp.Compare(ar, br)
	}
	if a.ScoreMicros != b.ScoreMicros {
		return cmp.Compare(b.ScoreMicros, a.ScoreMicros)
	}
	if a.Path != b.Path {
		return cmp.Compare(a.Path, b.Path)
	}
	return cmp.Compare(a.StartByte, b.StartByte)
}

// cmpScored is the TOTAL order the answer is served in, and it is a pure
// function of the repository's CONTENT. That is the whole point of the keys
// that follow the bounded four.
//
// NodeID and SearchKey cannot carry the tail of this order on their own:
// model.NewFileID hashes the repository together with the path, so a node id
// and a heading's search key both change when the same tree is indexed at a
// different root. Ordering on them made the served sequence a function of
// WHERE the tree lived and of the order documents happened to be interned,
// which is how a delta re-index and a fresh index of a byte-identical tree
// came to serve different pages. End byte, kind, name, qualified name and
// signature are derived from the file's bytes alone, so they order two
// candidates the same way in every index of the same content. NodeID and
// SearchKey stay last purely as a totality backstop.
//
// Comparing the served strings costs no extra memory: scored already embeds
// the SearchHit it will serve. The bounded `ranked` tuple still carries none
// of them, so the external sort's run buffer is unchanged.
func cmpScored(a, b scored) int {
	if c := a.ranked.cmpBounded(b.ranked); c != 0 {
		return c
	}
	if c := cmp.Compare(spanEnd(a.Span), spanEnd(b.Span)); c != 0 {
		return c
	}
	if c := cmp.Compare(a.Hit.Kind, b.Hit.Kind); c != 0 {
		return c
	}
	if c := cmp.Compare(a.Hit.Name, b.Hit.Name); c != 0 {
		return c
	}
	if c := cmp.Compare(a.Hit.QualifiedName, b.Hit.QualifiedName); c != 0 {
		return c
	}
	if c := cmp.Compare(a.Hit.Signature, b.Hit.Signature); c != 0 {
		return c
	}
	if c := cmp.Compare(a.NodeID, b.NodeID); c != 0 {
		return c
	}
	return cmp.Compare(a.SearchKey, b.SearchKey)
}

// spanEnd is the hydration interval's end byte, or -1 when a candidate carries
// no span. An exact-tier candidate may have none; -1 is below every real end
// byte and is the same value in every index, so the order stays total and
// content-derived either way.
func spanEnd(s *model.ByteRange) int64 {
	if s == nil {
		return -1
	}
	return int64(s.End)
}

// dedupKey is the digest §4 deduplication key: the node id when the candidate
// has one, else its file and start byte. Within one generation a path names
// exactly one file (model.NewFileID hashes the repository and the path), so
// the path stands in for the file id, which the frozen ranked struct does not
// carry.
func dedupKey(r ranked) string {
	if r.NodeID != "" {
		return "node\x00" + string(r.NodeID)
	}
	return "file\x00" + r.Path + "\x00" + strconv.FormatUint(r.StartByte, 10)
}

// boundReasons appends the unique reasons of add to have, truncating each to
// model.MaxReasonBytes and the list to model.MaxReasonsPerEntry so a folded
// hit can never fail model.SearchHit.Validate.
func boundReasons(have []string, add []string) []string {
	out := slices.Clone(have)
	for _, r := range add {
		if len(out) >= model.MaxReasonsPerEntry {
			break
		}
		if len(r) > model.MaxReasonBytes {
			r = r[:model.MaxReasonBytes]
		}
		if r == "" || slices.Contains(out, r) {
			continue
		}
		out = append(out, r)
	}
	return out
}

// scored is a ranked candidate together with everything needed to serve it:
// the bounded ranking reasons that survive deduplication, the hit as it will
// be served, and the byte interval its source range is hydrated from.
//
// The servable facts travel WITH the candidate rather than in a side map keyed
// by dedupKey. That map was one entry per distinct result, held for the whole
// answer -- the largest heap structure of a wide query -- and the external sort
// below cannot bound the ranking without also bounding it.
type scored struct {
	ranked
	Reasons []string
	Hit     model.SearchHit
	Span    *model.ByteRange
	// Folded is the highest ScoreMicros seen under this candidate's
	// deduplication key so far. It is kept SEPARATE from ranked.ScoreMicros
	// on purpose: ranked.ScoreMicros stays the candidate's OWN score for the
	// whole deduplication pass, so fold picks its survivor from a key that
	// never moves, and the fold is therefore a commutative, associative
	// minimum over the group instead of a left-to-right reduction whose
	// result depends on arrival order. results() promotes Folded into
	// ScoreMicros once the pass is closed, before the set is ordered by rank.
	Folded int64
}

// collector deduplicates candidates as they arrive and orders the distinct
// result set. Section 14.4 orders deduplication BEFORE paging, so folding
// cannot happen after a page boundary has already been drawn; and because a
// fold can change a survivor's tier and score, the whole set is ordered once
// at the end rather than maintained as a partial order while it mutates.
//
// Neither the number of distinct keys nor the number of candidates is bounded,
// and neither is held in heap HERE. Candidates stream into a disk-backed
// external sort whose working set is the run budget derived from
// resources.query_memory_bytes plus a capped merge fan-in, so peak RSS of the
// fold is a function of that admission and not of how many matches the query
// has.
//
// No candidate-sized heap structure survives upstream of this collector
// either: the exact tiers decide the cross-tier "most specific tier wins" rule
// from predicates over the candidate in hand (exact.go), so a one-character
// qualified_name_prefix that range-scans a corpus-sized slice of node_ids
// costs disk in this fold and one page of stored nodes upstream.
//
// TWO PASSES ARE NECESSARY, not a shortcut. fold sets ScoreMicros = max(a, b)
// and score is cmpScored's second key, so a fold MOVES its survivor's rank: two
// records with one deduplication key can sit arbitrarily far apart in rank
// order and folding equal-ranked neighbours would be wrong. Pass 1 sorts by
// the deduplication key and folds; pass 2 sorts the folded stream by rank with
// no fold.
type collector struct {
	dir       string
	runBytes  int64
	dedup     *pagination.ExternalSort[scored]
	truncated bool
	reason    string
	// peak is the largest in-memory working set either pass held. It is the
	// structural memory invariant: it must stay within the envelope the run
	// budget and the merge fan-in define however many candidates arrive, which
	// total allocation volume (which grows with the input even for a perfect
	// external sort) could never show.
	peak int
}

// newCollector opens the deduplication pass. dir is where runs spill (the
// spool directory, so a query's temporary files live in one place) and
// runBytes is the in-memory run budget, derived by the caller from
// resources.query_memory_bytes.
func newCollector(dir string, runBytes int64) (*collector, error) {
	c := &collector{dir: dir, runBytes: runBytes}
	sorter, err := newScoredSort(dir, "searchdedup-", runBytes,
		func(a, b scored) int { return strings.Compare(dedupKey(a.ranked), dedupKey(b.ranked)) })
	if err != nil {
		return nil, err
	}
	c.dedup = sorter.WithFold(foldScored)
	return c, nil
}

// newScoredSort opens one disk-backed pass over candidates with the shared
// codec and run budget. ExternalSort takes the codec as a parameter, so the
// packed candidate format in codec.go is supplied here and no other caller of
// the sort is affected. Both passes that exist go through it: the
// deduplication pass of the first request, and the ranking sort a
// continuation runs once over the raw candidate spool (ADR-0007 Decision 3).
func newScoredSort(dir, prefix string, runBytes int64, compare func(a, b scored) int) (*pagination.ExternalSort[scored], error) {
	sorter, err := pagination.NewExternalSort(dir, prefix, 0, encodeScored, decodeScored, compare)
	if err != nil {
		return nil, err
	}
	return sorter.WithRunBytes(runBytes, sizeOfScored), nil
}

// sizeOfScored charges one buffered candidate against the run budget: its
// variable-length strings plus a fixed allowance for the struct, the pointers
// and the reasons slice. It is an estimate by construction -- the record is
// encoded only when the run spills -- and it errs high.
func sizeOfScored(v scored) int64 {
	n := len(v.Path) + len(v.SearchKey) + len(v.NodeID) + len(v.Hit.Path) + len(v.Hit.Name) +
		len(v.Hit.QualifiedName) + len(v.Hit.Signature) + len(v.Hit.FileID)
	for _, r := range v.Reasons {
		n += len(r) + 16
	}
	return int64(n) + 256
}

// add folds one candidate into the set. A repeat of a key folds into the
// entry already held; a new key is always admitted.
func (c *collector) add(r ranked, f hitFacts, reasons ...string) error {
	return c.dedup.Add(scored{ranked: r, Reasons: boundReasons(nil, reasons), Hit: f.hit, Span: f.span, Folded: r.ScoreMicros})
}

// Close releases the deduplication pass's spill files.
func (c *collector) Close() error { return c.dedup.Close() }

// fold merges two candidates for the same entity. It is a MINIMUM over the
// group under cmpScored plus three accumulators, so it is commutative and
// associative: the survivor and everything it carries are the same whatever
// order the external sort happens to present a key's arrivals in. That is what
// makes the served answer a pure function of the repository's content, and it
// is why nothing here reads an accumulated field.
//
//   - The survivor is whichever sorts first, so it keeps the LOWEST tier rank
//     seen (tier rank is cmpScored's first key) and, within a tier, the highest
//     OWN score. Comparing accumulated scores instead would let a low-scoring
//     candidate that had already absorbed a high score out-rank a genuinely
//     higher-scoring sibling, which is order-dependent.
//   - Folded is the highest score of the two: digest §4 zeroes the exact tiers
//     "unless the document also matched lexically, keeping that score", and the
//     lexical score is the non-zero one. results() promotes it.
//   - Occurrences sum, and the reasons of both sides survive up to the Section
//     14.3 bound.
//
// The SERVABLE facts travel with the survivor -- they are not taken from the
// left side. Two candidates under one deduplication key do NOT always describe
// the same servable entity: the key groups them, it does not promise they
// agree on name, range or score. Taking the left side's facts serves whichever
// member the lexical walk reaches first, which is doc_id order -- and a delta
// re-index carries doc_ids forward while a fresh index assigns them anew, so
// one tree answers with different rows depending on how it was indexed. The
// rule therefore holds for ANY key whose members differ in the facts served,
// not for one provider's shape.
func fold(a, b scored) scored {
	keep, other := a, b
	if cmpScored(b, a) < 0 {
		keep, other = b, a
	}
	keep.Folded = max(a.Folded, b.Folded)
	keep.Occurrences = a.Occurrences + b.Occurrences
	keep.Reasons = mergeReasons(keep.Reasons, other.Reasons)
	return keep
}

// mergeReasons unions two reason lists in a fixed order. The union is sorted
// before it is bounded so that WHICH reasons survive an overflow -- and the
// order they are served in -- depend on the reasons themselves and not on the
// order the fold happened to see them in. Reasons are part of the served hit,
// so an arrival-ordered list would leave the very non-determinism the fold
// above removes.
func mergeReasons(a, b []string) []string {
	all := slices.Concat(a, b)
	slices.Sort(all)
	return boundReasons(nil, all)
}

// foldScored adapts fold to the sort's fold signature. It cannot fail: every
// part of the fold is total.
func foldScored(a, b scored) (scored, error) { return fold(a, b), nil }

// deduped ends the deduplication pass and answers the distinct candidate set
// as a re-iterable sorted run on disk, in DEDUPLICATION-KEY order.
//
// It no longer ranks. ADR-0007 Decision 3 moves the rank order off the first
// page's critical path: the caller streams this run once, selects the page
// through a bounded heap under cmpScored and lays the same records into the
// raw continuation spool, and the external sort by rank runs at most once,
// on the first continuation, over that spool. A first page that is the whole
// answer therefore pays no ranking sort and writes no spool at all.
//
// A survivor's folded score is final once this pass is closed; promoting it
// into ScoreMicros is the caller's first act on every record it takes out of
// this run, because that promoted score is cmpScored's second key and the
// value the raw spool must carry.
//
// Close the run; the collector's own pass is released by Close.
func (c *collector) deduped() (*pagination.SortedRun[scored], error) {
	run, err := c.dedup.Sorted()
	if err != nil {
		return nil, err
	}
	c.peak = c.dedup.PeakLiveRecords()
	return run, nil
}

// pageHeap selects the first page out of an unordered candidate stream: a
// bounded heap of one page plus one entry, ordered by cmpScored itself, which
// is the SAME total order the continuation's external sort applies. One
// comparator is what makes the heap's page the sorted run's first page by
// construction rather than by coincidence (ADR-0007 Decision 3).
//
// Less inverts cmpScored, so the root is the candidate that sorts LAST and is
// the one an overflowing offer evicts. Memory is the page bound, never the
// match count; this is a selection, not a top-k cap, because every candidate
// is also spooled and served.
type pageHeap struct {
	items []scored
	limit int
}

func (h *pageHeap) Len() int           { return len(h.items) }
func (h *pageHeap) Less(i, j int) bool { return cmpScored(h.items[i], h.items[j]) > 0 }
func (h *pageHeap) Swap(i, j int)      { h.items[i], h.items[j] = h.items[j], h.items[i] }
func (h *pageHeap) Push(x any)         { h.items = append(h.items, x.(scored)) }

func (h *pageHeap) Pop() any {
	last := h.items[len(h.items)-1]
	h.items = h.items[:len(h.items)-1]
	return last
}

// offer admits one candidate and evicts the worst held once the heap would
// hold more than one page plus one entry. The plus one is what lets the caller
// recognize, on the arrival that first overflows the page, that the heap holds
// EXACTLY the candidates seen so far -- the moment it can open the raw spool
// and lay them all into it without a second walk of the candidate set.
func (h *pageHeap) offer(v scored) {
	heap.Push(h, v)
	if len(h.items) > h.limit+1 {
		heap.Pop(h)
	}
}

// full reports whether the heap holds more candidates than one page.
func (h *pageHeap) full() bool { return len(h.items) > h.limit }

// page is the first page in served order: the held candidates ordered by
// cmpScored, cut to the page bound.
func (h *pageHeap) page() []scored {
	out := slices.Clone(h.items)
	slices.SortFunc(out, cmpScored)
	if len(out) > h.limit {
		out = out[:h.limit]
	}
	return out
}

// peakLiveRecords reports that high-water mark.
func (c *collector) peakLiveRecords() int { return c.peak }

// truncation reports whether the candidate set was bounded away and why, for
// QueryMeta.Truncated / TruncationReason.
func (c *collector) truncation() (bool, string) { return c.truncated, c.reason }

// maxRangeWindowBytes is the size of ONE hydration read, not a bound on the
// work hydration will do: positionAt walks a span of any length in windows of
// this size, so no input can make it refuse or truncate an answer and there is
// nothing here for an operator to raise. It is source.CheckpointBytes because
// that is the checkpoint spacing, so a file with line boundaries costs exactly
// one read per endpoint and a file without them costs one read per window.
const maxRangeWindowBytes = source.CheckpointBytes

// fileReader, blobReader and ContentReader are the three narrow reads range
// hydration needs. They are interfaces, not the concrete *sqlite.PinnedReader,
// *sqlite.Store and *snapshot.CAS, so this package stays free of a snapshot
// import; ContentReader is exported because Options.Content is how the
// composition root hands the service the CAS it already opens.
type fileReader interface {
	File(ctx context.Context, id model.FileID) (model.FileVersion, error)
}

type blobReader interface {
	Blob(ctx context.Context, hash string) (model.BlobRecord, error)
}

// ContentReader reads one bounded byte window out of the content-addressed
// store. *snapshot.CAS satisfies it.
type ContentReader interface {
	ReadRange(ctx context.Context, rec model.BlobRecord, r model.ByteRange) ([]byte, error)
}

// hydrator fills SearchHit.Range for the hits of ONE page. Returning a nil
// Range is a silent capability reduction (digest §4), so this runs for every
// hit that is actually served -- at most model.MaxPageItems of them -- and not
// for the candidates that never reach a page.
type hydrator struct {
	files   fileReader
	blobs   blobReader
	content ContentReader
	// blobOf caches one BlobRecord per file for the life of one page: the
	// hits of a page cluster in a handful of files, and re-reading the same
	// blob row per hit would be the dominant cost of hydration.
	blobOf map[model.FileID]model.BlobRecord
}

func newHydrator(files fileReader, blobs blobReader, content ContentReader) *hydrator {
	return &hydrator{files: files, blobs: blobs, content: content, blobOf: make(map[model.FileID]model.BlobRecord)}
}

// hydratePage fills the Range of every hit from the byte interval of the
// document it was ranked from; spans[i] belongs to hits[i]. A failure is
// returned rather than swallowed into a nil Range, EXCEPT the one class
// blobFault names: an integrity fault of the content store is charged to the
// hit it belongs to and the rest of the page is still answered.
func (h *hydrator) hydratePage(ctx context.Context, hits []model.SearchHit, spans []model.ByteRange) error {
	if len(hits) != len(spans) {
		return &model.Error{Code: model.CodeInternal,
			Message: "search: hydration was given " + strconv.Itoa(len(spans)) + " byte ranges for " + strconv.Itoa(len(hits)) + " hits"}
	}
	for i := range hits {
		if err := ctx.Err(); err != nil {
			return contextErr(err)
		}
		rng, err := h.hydrate(ctx, hits[i].FileID, spans[i])
		if err != nil {
			if reason, ok := blobFault(err); ok {
				hits[i].Range = nil
				hits[i].MarkUnresolved(model.SearchHitFieldRange, reason)
				continue
			}
			return err
		}
		hits[i].Range = rng
	}
	return nil
}

// blobFault reports whether err is an integrity fault of the CONTENT STORE --
// the blob absent from the content-addressed store, a blob row the snapshot no
// longer retains or that is not ready, a short read, or a block digest that
// does not verify -- and, if so, the reason to publish on the hit.
//
// Exactly ONE typed code qualifies: model.CodeSourceIntegrity. Everything else
// still fails the whole answer, deliberately:
//   - model.CodeCanceled / model.CodeQueryDeadline -- the caller stopped asking
//     or the query ran out of time; flagging those would publish a truncated
//     answer as a complete one with a per-hit footnote.
//   - model.CodeArgumentInvalid -- a document that claims bytes past its file,
//     or blob metadata that does not validate. That is a corrupt index, not a
//     silently clamped range, and it stays an error.
//   - model.CodeInternal, model.CodeDiskFull, model.CodeResourceLimit and any
//     untyped error -- a defect or an environment failure that is not a
//     property of the one blob this hit names.
//
// One unreadable blob must not cost the caller every other hit; a corrupt
// index, a cancelled query and a programmer error must still be loud.
func blobFault(err error) (string, bool) {
	var typed *model.Error
	if !errors.As(err, &typed) || typed.Code != model.CodeSourceIntegrity {
		return "", false
	}
	return typed.Code + ": " + typed.Message, true
}

// hydrate resolves one byte interval to a Section 9.3 source range. Each
// endpoint is resolved by walking forward from the last line checkpoint at or
// before it, one bounded window at a time; the walk that resolved the start is
// then CONTINUED to the end rather than restarted, so a node whose two
// endpoints sit inside the same long line costs one pass over the span and not
// two. The one position implementation (package source) does the counting --
// this never counts lines itself.
func (h *hydrator) hydrate(ctx context.Context, file model.FileID, span model.ByteRange) (*model.SourceRange, error) {
	rec, err := h.blob(ctx, file)
	if err != nil {
		return nil, err
	}
	// Both halves are refused here, and BOTH are needed: an inverted span
	// (Start > End) whose End is inside the file would otherwise reach
	// positionAt with an offset the window loop can never advance to -- the
	// window clamps at rec.Size while w.At() stays below Start, so the loop
	// computes an empty window, passes the length check and spins forever.
	// A typed refusal is what this used to return and what it owes.
	if span.End > uint64(rec.Size) || span.Start > span.End {
		return nil, &model.Error{Code: model.CodeArgumentInvalid,
			Message: "search: a search document spans bytes " + strconv.FormatUint(span.Start, 10) +
				" to " + strconv.FormatUint(span.End, 10) + ", which is not an interval inside the " +
				strconv.FormatInt(rec.Size, 10) + "-byte file it names"}
	}
	idx := checkpointIndex(rec)
	start, w, err := h.positionAt(ctx, rec, idx, nil, span.Start)
	if err != nil {
		return nil, err
	}
	end := start
	if span.End != span.Start {
		if end, _, err = h.positionAt(ctx, rec, idx, w, span.End); err != nil {
			return nil, err
		}
	}
	rng := &model.SourceRange{Start: start, End: end}
	if err := rng.Validate("search_hit.range"); err != nil {
		return nil, err
	}
	return rng, nil
}

// positionAt converts one byte offset to a line and byte column, holding one
// window of the file at a time however far the nearest line checkpoint is.
//
// Resolving the offset in ONE read used to be a typed CTX_RESOURCE_LIMIT
// whenever the span exceeded the CAS read ceiling, which failed the WHOLE
// answer -- observed on real repositories as `search` refusing every hit
// because one file (a minified bundle, a generated data file, anything that is
// one line of megabytes) put 4.2 M bytes between the nearest checkpoint and a
// hit. Checkpoints are placed at line starts, so a file with no line boundary
// for megabytes genuinely has no nearer anchor, and no indexing setting can
// create one. The span is therefore STREAMED: source.Walker keeps only the
// current line and where it started while each window is read, counted and
// dropped. Peak heap is one window, the read cost is O(span), and there is no
// bound on the span at all -- nothing is approximated, and the position is
// still counted by the one position implementation.
//
// w, when it is not nil and has already passed the nearest checkpoint,
// continues a walk in the same file instead of starting a second one; the
// walker it returns is positioned at offset for the next endpoint.
func (h *hydrator) positionAt(ctx context.Context, rec model.BlobRecord, idx source.Index,
	w *source.Walker, offset uint64) (model.Position, *source.Walker, error) {
	if cp := idx.CheckpointFor(offset); w == nil || cp.Byte > w.At() {
		fresh, err := source.NewWalker(cp)
		if err != nil {
			return model.Position{}, nil, err
		}
		w = fresh
	}
	for w.At() < offset {
		// The window stops one byte PAST the offset where the file has one:
		// that byte is what rejects an offset inside a UTF-8 sequence, and
		// reading it here costs no extra call.
		end := min(min(w.At()+maxRangeWindowBytes, offset+1), uint64(rec.Size))
		window, err := h.content.ReadRange(ctx, rec, model.ByteRange{Start: w.At(), End: end})
		if err != nil {
			return model.Position{}, nil, err
		}
		// ReadRange returns exactly the interval asked for, so a short read
		// is a store that no longer holds the blob it recorded; the walk
		// would otherwise stall or slice past the window it was handed.
		if uint64(len(window)) != end-w.At() {
			return model.Position{}, nil, &model.Error{Code: model.CodeSourceIntegrity,
				Message: "search: the content store returned " + strconv.Itoa(len(window)) + " of the " +
					strconv.FormatUint(end-w.At(), 10) + " bytes at offset " + strconv.FormatUint(w.At(), 10) +
					" of a " + strconv.FormatInt(rec.Size, 10) + "-byte blob",
				Remediation: "run `codectx doctor --deep` to verify the content store"}
		}
		if consumed := offset - w.At(); consumed < uint64(len(window)) {
			w.Advance(window[:consumed])
			pos, err := w.PositionAt(window[consumed:])
			return pos, w, err
		}
		w.Advance(window)
	}
	// The walk landed exactly on the offset -- a checkpoint at it, or a
	// previous endpoint -- so the byte at it has not been read yet.
	var next []byte
	if offset < uint64(rec.Size) {
		read, err := h.content.ReadRange(ctx, rec, model.ByteRange{Start: offset, End: offset + 1})
		if err != nil {
			return model.Position{}, nil, err
		}
		// Length-checked for the same reason the window read above is, and on
		// the COMMON path: this branch is taken whenever the walk landed
		// exactly on the offset, which is every hit at a checkpointed line
		// start. A short read here would hand PositionAt an empty lookahead,
		// silently skipping the UTF-8 continuation-byte rejection instead of
		// faulting the store that no longer holds the blob it recorded.
		if len(read) != 1 {
			return model.Position{}, nil, &model.Error{Code: model.CodeSourceIntegrity,
				Message: "search: the content store returned " + strconv.Itoa(len(read)) +
					" of the 1 byte at offset " + strconv.FormatUint(offset, 10) +
					" of a " + strconv.FormatInt(rec.Size, 10) + "-byte blob",
				Remediation: "run `codectx doctor --deep` to verify the content store"}
		}
		next = read
	}
	pos, err := w.PositionAt(next)
	return pos, w, err
}

// blob resolves a file to its retained blob metadata, once per page.
func (h *hydrator) blob(ctx context.Context, file model.FileID) (model.BlobRecord, error) {
	if rec, ok := h.blobOf[file]; ok {
		return rec, nil
	}
	fv, err := h.files.File(ctx, file)
	if err != nil {
		return model.BlobRecord{}, err
	}
	rec, err := h.blobs.Blob(ctx, fv.ContentHash)
	if err != nil {
		return model.BlobRecord{}, err
	}
	h.blobOf[file] = rec
	return rec, nil
}

// checkpointIndex adapts a blob's sparse line checkpoints to the one
// implementation of "the nearest point a range read can start scanning from".
// A checkpoint's LineStartByte, not its ByteOffset, is the window start:
// source.NewCursorAt rejects a window that does not begin on a line boundary.
func checkpointIndex(rec model.BlobRecord) source.Index {
	idx := source.Index{Size: uint64(rec.Size), ContentHash: rec.Hash, Blocks: rec.BlockDigests}
	for _, cp := range rec.LineCheckpoints {
		idx.Checkpoints = append(idx.Checkpoints, source.Checkpoint{Byte: cp.LineStartByte, Line: cp.LineNumber})
	}
	return idx
}

// contextErr maps a context failure to the digest §6 codes. model.Canceled
// reports CTX_CANCELED for both causes, but Section 22 separates a caller who
// stopped asking from a query that ran past its deadline: the latter is an
// explicit incomplete answer the operator can act on. The remediation names
// both places that deadline can come from, because either can be the one that
// fired -- resources.query_timeout is unlimited by default, so on a shipped
// configuration it is usually the caller's own `--timeout`.
func contextErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return &model.Error{Code: model.CodeQueryDeadline, Retryable: true,
			Message:     "the query exceeded its time budget before it could answer completely",
			Remediation: "narrow the query, or raise the deadline you set with --timeout or resources.query_timeout"}
	}
	return model.Canceled(err)
}
