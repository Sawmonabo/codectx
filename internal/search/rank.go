package search

import (
	"context"
	"errors"
	"slices"
	"sort"
	"strconv"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/source"
)

// ranked is one candidate in the bounded top-K heap: the Section 14.2 sort
// tuple, the rowid that hydrates it, and the folded occurrence count. Nothing
// else -- name/qualified name/signature would make 2000 entries ~30 MB against
// a 33 MB query_memory_bytes ceiling, and SearchDocuments exists to hydrate
// one page instead. Ordering compares only integers and exact strings; no
// float reaches a comparison (digest §4).
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

// maxRankedHits bounds the heap (digest §6).
const maxRankedHits = 10 * model.MaxPageItems

// truncationRankedSetFull is the QueryMeta.TruncationReason a caller sees when
// the candidate set outgrew maxRankedHits. Section 14.3 forbids a silent drop:
// the answer says it is incomplete and why.
var truncationRankedSetFull = "the ranked candidate set reached its bound of " +
	strconv.Itoa(maxRankedHits) + " distinct results; narrow the query or add a filter"

// scored is a ranked candidate with the bounded ranking reasons that survive
// deduplication. Reasons live beside ranked rather than inside it because the
// frozen struct is sized for 2000 entries under the query memory ceiling.
type scored struct {
	ranked
	Reasons []string
}

// collector deduplicates candidates as they arrive and bounds the distinct
// result set. Section 14.4 orders deduplication BEFORE paging, so folding
// cannot happen after a page boundary has already been drawn; and because a
// fold can change a survivor's tier and score, the whole set is ordered once
// at the end rather than maintained as a partial order while it mutates.
//
// The bound is on DISTINCT keys, which is what maxRankedHits means: 2000
// ranked structs plus their reasons sort in microseconds and stay three orders
// of magnitude under resources.query_memory_bytes, so a literal heap would buy
// nothing and would additionally have to track each key's position in the
// backing array to fold into it.
type collector struct {
	index     map[string]int
	items     []scored
	truncated bool
	reason    string
}

func newCollector() *collector { return &collector{index: make(map[string]int)} }

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

// add folds one candidate into the set. Beyond maxRankedHits distinct keys a
// NEW key is refused and the set is marked truncated; an existing key still
// folds, because dropping a fold would understate an occurrence count that is
// already represented in the answer.
func (c *collector) add(r ranked, reasons ...string) {
	key := dedupKey(r)
	if i, ok := c.index[key]; ok {
		c.items[i] = fold(c.items[i], scored{ranked: r, Reasons: reasons})
		return
	}
	if len(c.items) >= maxRankedHits {
		c.truncated, c.reason = true, truncationRankedSetFull
		return
	}
	c.index[key] = len(c.items)
	c.items = append(c.items, scored{ranked: r, Reasons: boundReasons(nil, reasons)})
}

// fold merges two candidates for the same entity. The survivor is whichever
// sorts first, so it keeps the LOWEST tier rank seen (tier rank is less's
// first key). Its ScoreMicros is the highest of the two: digest §4 zeroes the
// exact tiers "unless the document also matched lexically, keeping that
// score", and the lexical score is the non-zero one. Occurrences sum, and the
// reasons of both sides survive up to the Section 14.3 bound.
func fold(a, b scored) scored {
	keep := a
	if b.ranked.less(a.ranked) {
		keep = b
	}
	keep.ScoreMicros = max(a.ScoreMicros, b.ScoreMicros)
	keep.Occurrences = a.Occurrences + b.Occurrences
	keep.Reasons = boundReasons(a.Reasons, b.Reasons)
	return keep
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

// results orders the deduplicated set by the full Section 14.2 chain. The
// order is total (SearchKey is unique), so sort.SliceStable is not needed to
// make it deterministic.
func (c *collector) results() []scored {
	out := slices.Clone(c.items)
	sort.Slice(out, func(i, j int) bool { return out[i].ranked.less(out[j].ranked) })
	return out
}

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
// from the nearest checkpoint at or before it. A checkpoint interval wider
// than maxRangeWindowBytes (one line longer than the whole interval) is a
// typed resource limit: serving a position from a truncated window would
// report a wrong line, and Section 14.2 promises linked source positions, not
// plausible ones.
func (h *hydrator) positionAt(ctx context.Context, rec model.BlobRecord, idx source.Index, offset uint64) (model.Position, error) {
	cp := idx.CheckpointFor(offset)
	if offset-cp.Byte > maxRangeWindowBytes {
		return model.Position{}, &model.Error{Code: model.CodeResourceLimit,
			Message:     "search: resolving a source position at byte " + strconv.FormatUint(offset, 10) + " would read past the bounded window",
			Remediation: "re-index the file so its line checkpoints cover it"}
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
