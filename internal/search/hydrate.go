package search

// Source-range hydration of the hits one page serves.

import (
	"context"
	"errors"
	"strconv"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/source"
)

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
// Range is a silent capability reduction (Section 14.2), so this runs for every
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
	// A typed refusal is what it owes.
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
// Resolving the offset in ONE read would be a typed CTX_RESOURCE_LIMIT
// whenever the span exceeded the CAS read ceiling, and it would fail the WHOLE
// answer: on real repositories one file (a minified bundle, a generated data
// file, anything that is one line of megabytes) puts 4.2 M bytes between the
// nearest checkpoint and a hit. Checkpoints are placed at line starts, so a file with no line boundary
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
	// The bytes from the offset that decide whether it is inside a
	// well-formed UTF-8 sequence: source.BoundaryContext of them, fewer only
	// at the end of the file. The walker holds the bytes before it.
	ahead := min(offset+source.BoundaryContext, uint64(rec.Size))
	for w.At() < offset {
		// The window runs PAST the offset to ahead where it can: reading the
		// deciding bytes here costs no extra call.
		end := min(w.At()+maxRangeWindowBytes, ahead)
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
			if end == ahead {
				pos, err := w.PositionAt(window[consumed:])
				return pos, w, err
			}
			// The window ends between the offset and ahead: the deciding
			// bytes are read below.
			break
		}
		w.Advance(window)
	}
	// The walk is exactly on the offset -- a checkpoint at it, a previous
	// endpoint, or a window that ended short of ahead -- so the bytes that
	// decide it have not all been read yet.
	var next []byte
	if offset < ahead {
		read, err := h.content.ReadRange(ctx, rec, model.ByteRange{Start: offset, End: ahead})
		if err != nil {
			return model.Position{}, nil, err
		}
		// Length-checked for the same reason the window read above is, and on
		// the COMMON path: this branch is taken whenever the walk landed
		// exactly on the offset, which is every hit at a checkpointed line
		// start. A short read here would hand PositionAt a truncated
		// lookahead, silently skipping the rejection of an offset inside a
		// UTF-8 sequence instead of faulting the store that no longer holds
		// the blob it recorded.
		if uint64(len(read)) != ahead-offset {
			return model.Position{}, nil, &model.Error{Code: model.CodeSourceIntegrity,
				Message: "search: the content store returned " + strconv.Itoa(len(read)) +
					" of the " + strconv.FormatUint(ahead-offset, 10) + " bytes at offset " +
					strconv.FormatUint(offset, 10) + " of a " + strconv.FormatInt(rec.Size, 10) + "-byte blob",
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
// A checkpoint's LineStartByte, not its ByteOffset, is the walk's start:
// source.NewWalker counts lines from a line start, which also needs no bytes
// before it to be a boundary.
func checkpointIndex(rec model.BlobRecord) source.Index {
	idx := source.Index{Size: uint64(rec.Size), ContentHash: rec.Hash, Blocks: rec.BlockDigests}
	for _, cp := range rec.LineCheckpoints {
		idx.Checkpoints = append(idx.Checkpoints, source.Checkpoint{Byte: cp.LineStartByte, Line: cp.LineNumber})
	}
	return idx
}

// contextErr maps a context failure to the Section 22 codes. model.Canceled
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
