package search

import (
	"bytes"
	"context"
	"encoding/json"
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
// else -- carrying name/qualified name/signature on every candidate would size
// the sort's run buffer by the widest symbol in the corpus, and
// SearchDocuments exists to hydrate one chunk instead. Ordering compares only
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

// less is the total Section 14.2 tie-break order: tier rank, descending
// ScoreMicros, path, start byte, NodeID, search key.
//
// Every comparison is on an int, an int64, a uint64 or an exact string in Go's
// byte order. The struct carries no float field at all, which is what makes
// "nothing compares floats" (digest §4) structural rather than a convention a
// later edit could break: a score reaches this function only after
// quantizeScore has turned it into an int64.
func (a ranked) less(b ranked) bool {
	if ar, br := a.Tier.Rank(), b.Tier.Rank(); ar != br {
		return ar < br
	}
	if a.ScoreMicros != b.ScoreMicros {
		return a.ScoreMicros > b.ScoreMicros
	}
	if a.Path != b.Path {
		return a.Path < b.Path
	}
	if a.StartByte != b.StartByte {
		return a.StartByte < b.StartByte
	}
	if a.NodeID != b.NodeID {
		return a.NodeID < b.NodeID
	}
	return a.SearchKey < b.SearchKey
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
	Reasons []string         `json:"reasons,omitempty"`
	Hit     model.SearchHit  `json:"hit"`
	Span    *model.ByteRange `json:"span,omitempty"`
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
// ONE candidate-sized heap structure survives upstream of this collector, and
// it is named rather than denied: exactCandidates' `seen` set holds one
// model.NodeID per distinct candidate across the exact tiers (exact.go), which
// is what decides the cross-tier "most specific tier wins" rule and cannot be
// decided from a streamed page. A one-character qualified_name_prefix that
// range-scans a corpus-sized slice of node_ids therefore costs disk in this
// fold and heap in `seen` -- the ledgered residual recorded for the exact
// tiers, not a bound this collector removes.
//
// TWO PASSES ARE NECESSARY, not a shortcut. fold sets ScoreMicros = max(a, b)
// and score is less's second key, so a fold MOVES its survivor's rank: two
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
	sorter, err := c.newSort("searchdedup-",
		func(a, b scored) int { return strings.Compare(dedupKey(a.ranked), dedupKey(b.ranked)) })
	if err != nil {
		return nil, err
	}
	c.dedup = sorter.WithFold(foldScored)
	return c, nil
}

// newSort opens one pass of the two-pass sort with the shared codec and
// budget. The codec is JSON: these records never leave the process, and a
// codec that cannot drift from the struct beats the bytes a packed one saves.
func (c *collector) newSort(prefix string, compare func(a, b scored) int) (*pagination.ExternalSort[scored], error) {
	sorter, err := pagination.NewExternalSort(c.dir, prefix, 0,
		func(v scored) ([]byte, error) {
			b, err := json.Marshal(v)
			if err != nil {
				return nil, &model.Error{Code: model.CodeInternal, Message: "search: encoding a candidate: " + err.Error()}
			}
			return b, nil
		},
		func(b []byte) (scored, error) {
			var v scored
			if err := json.Unmarshal(b, &v); err != nil {
				return scored{}, &model.Error{Code: model.CodeStorageCorrupt, Message: "search: a spilled candidate is not readable"}
			}
			return v, nil
		}, compare)
	if err != nil {
		return nil, err
	}
	return sorter.WithRunBytes(c.runBytes, sizeOfScored), nil
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
	return c.dedup.Add(scored{ranked: r, Reasons: boundReasons(nil, reasons), Hit: f.hit, Span: f.span})
}

// Close releases the deduplication pass's spill files.
func (c *collector) Close() error { return c.dedup.Close() }

// fold merges two candidates for the same entity. The survivor is whichever
// sorts first, so it keeps the LOWEST tier rank seen (tier rank is less's
// first key). Its ScoreMicros is the highest of the two: digest §4 zeroes the
// exact tiers "unless the document also matched lexically, keeping that
// score", and the lexical score is the non-zero one. Occurrences sum, and the
// reasons of both sides survive up to the Section 14.3 bound.
//
// The SERVABLE facts come from the left side always: every candidate under one
// key describes the same entity, and the first writer wins exactly as the side
// map it replaces did. a is the accumulated left because the external sort
// folds a key's arrivals left to right in arrival order.
func fold(a, b scored) scored {
	keep := a
	if b.ranked.less(a.ranked) {
		keep = b
	}
	keep.ScoreMicros = max(a.ScoreMicros, b.ScoreMicros)
	keep.Occurrences = a.Occurrences + b.Occurrences
	keep.Reasons = boundReasons(a.Reasons, b.Reasons)
	keep.Hit, keep.Span = a.Hit, a.Span
	return keep
}

// foldScored adapts fold to the sort's fold signature. It cannot fail: every
// part of the fold is total.
func foldScored(a, b scored) (scored, error) { return fold(a, b), nil }

// results ends the deduplication pass and orders the distinct set by the full
// Section 14.2 chain. The order is total (SearchKey is unique within a
// deduplication key), so no stable sort is needed to make it deterministic.
//
// The answer is a re-iterable sorted run on disk, not a slice: the caller
// streams one page out of it and streams the remainder straight into the
// continuation spool, so no part of the flow ever holds the whole answer.
// Close the run; the collector's own pass is released by Close.
func (c *collector) results() (*pagination.SortedRun[scored], error) {
	deduped, err := c.dedup.Sorted()
	if err != nil {
		return nil, err
	}
	defer deduped.Close()
	byRank, err := c.newSort("searchrank-", func(a, b scored) int {
		switch {
		case a.ranked.less(b.ranked):
			return -1
		case b.ranked.less(a.ranked):
			return 1
		}
		return 0
	})
	if err != nil {
		return nil, err
	}
	defer byRank.Close()
	if err := deduped.Each(byRank.Add); err != nil {
		return nil, err
	}
	run, err := byRank.Sorted()
	if err != nil {
		return nil, err
	}
	c.peak = max(c.dedup.PeakLiveRecords(), byRank.PeakLiveRecords())
	return run, nil
}

// peakLiveRecords reports that high-water mark.
func (c *collector) peakLiveRecords() int { return c.peak }

// truncation reports whether the candidate set was bounded away and why, for
// QueryMeta.Truncated / TruncationReason.
func (c *collector) truncation() (bool, string) { return c.truncated, c.reason }

// maxRangeWindowBytes is the digest §4 bound on one hydration read. It is
// source.CheckpointBytes: a blob carries a line checkpoint at least every that
// many bytes, so resolving one offset reads at most one checkpoint interval
// and never scans a file from byte zero.
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
// returned rather than swallowed into a nil Range.
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
			return err
		}
		hits[i].Range = rng
	}
	return nil
}

// hydrate resolves one byte interval to a Section 9.3 source range. Each
// endpoint is resolved from the last line checkpoint at or before it, so both
// reads are bounded by one checkpoint interval; a node that spans megabytes
// costs two small reads rather than one read of its whole extent. The one
// position implementation (source.Cursor) does the counting -- this never
// counts lines itself.
func (h *hydrator) hydrate(ctx context.Context, file model.FileID, span model.ByteRange) (*model.SourceRange, error) {
	rec, err := h.blob(ctx, file)
	if err != nil {
		return nil, err
	}
	if span.End > uint64(rec.Size) {
		return nil, &model.Error{Code: model.CodeArgumentInvalid,
			Message: "search: a search document ends at byte " + strconv.FormatUint(span.End, 10) +
				" past the " + strconv.FormatInt(rec.Size, 10) + "-byte file it names"}
	}
	idx := checkpointIndex(rec)
	start, err := h.positionAt(ctx, rec, idx, span.Start)
	if err != nil {
		return nil, err
	}
	end := start
	if span.End != span.Start {
		if end, err = h.positionAt(ctx, rec, idx, span.End); err != nil {
			return nil, err
		}
	}
	rng := &model.SourceRange{Start: start, End: end}
	if err := rng.Validate("search_hit.range"); err != nil {
		return nil, err
	}
	return rng, nil
}

// positionAt converts one byte offset to a line and byte column, reading only
// from the nearest line boundary at or before it.
//
// A checkpoint interval wider than maxRangeWindowBytes used to be a typed
// CTX_RESOURCE_LIMIT, which failed the WHOLE answer -- observed on a real
// repository as `search` refusing every hit because one file's checkpoints did
// not reach byte 1.7 M. A generation whose blob records carry sparse (or no)
// checkpoints is exactly the input the scale posture says must still be
// served, so the gap is now WALKED instead of refused: one window is read at a
// time and the anchor advances to the LAST line boundary inside it, which
// keeps the invariant source.NewCursorAt requires (a window that begins on a
// line boundary) while the peak stays one window however far the nearest
// checkpoint is. Nothing is approximated -- the position is still counted by
// the one position implementation, over a window that starts on a line start.
//
// The single case the walk cannot shorten is a LINE longer than one window --
// a minified bundle. There is no line boundary to advance to, so the final
// read is bounded by that line instead of by the window. That is a read this
// answer was asked for, and one line of one file is not a repository-sized
// structure; refusing it would fail the answer for the shape of someone's
// source.
func (h *hydrator) positionAt(ctx context.Context, rec model.BlobRecord, idx source.Index, offset uint64) (model.Position, error) {
	cp := idx.CheckpointFor(offset)
	for offset-cp.Byte > maxRangeWindowBytes {
		window, err := h.content.ReadRange(ctx, rec, model.ByteRange{Start: cp.Byte, End: cp.Byte + maxRangeWindowBytes})
		if err != nil {
			return model.Position{}, err
		}
		last := bytes.LastIndexByte(window, '\n')
		if last < 0 {
			break
		}
		cp = source.Checkpoint{Byte: cp.Byte + uint64(last) + 1,
			Line: cp.Line + uint32(bytes.Count(window, []byte{'\n'}))}
	}
	data, err := h.content.ReadRange(ctx, rec, model.ByteRange{Start: cp.Byte, End: offset})
	if err != nil {
		return model.Position{}, err
	}
	cur, err := source.NewCursorAt(data, cp.Byte, cp.Line)
	if err != nil {
		return model.Position{}, err
	}
	return cur.PositionAt(offset)
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
// stopped asking from a query that ran past resources.query_timeout: the
// latter is an explicit incomplete answer the operator can act on by raising
// the timeout or narrowing the query.
func contextErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return &model.Error{Code: model.CodeQueryDeadline, Retryable: true,
			Message:     "the query exceeded its time budget before it could answer completely",
			Remediation: "narrow the query or raise resources.query_timeout"}
	}
	return model.Canceled(err)
}
