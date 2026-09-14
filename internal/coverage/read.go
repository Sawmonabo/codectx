package coverage

import (
	"context"
	"encoding/base64"
	"fmt"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/source"
)

// hexDigits is the alphabet every 64-character identifier in this system is
// written in, in ascending order, which is also the order the store's keyset
// pages in. previousFileID decrements in it.
const hexDigits = "0123456789abcdef"

// Read serves one bounded, lossless chunk of pinned source and issues its
// receipt.
//
// Order is the whole invariant (Sections 16.2 and 16.3), and every step below
// is an existing call this package does not reimplement:
//
//  1. Validate, then Session: the actor, TTL and closed checks gate everything
//     that follows. An expired session must not reach the CAS, so the partially
//     populated record Session returns beside CTX_SESSION_EXPIRED is not used --
//     the error aborts the read.
//  2. Echo any receipts the client carried, through the one shared
//     confirmReceipts path, BEFORE the unconfirmed cap is counted: the cure for
//     the cap is confirming, and counting first would make a client that echoes
//     its receipts in the same call unable to ever recover.
//  3. The cap itself, as backpressure.
//  4. maxRawForWire sizes the chunk once, from the real checkpoint prefix, so
//     the slicer and the serializer cannot disagree and a maximum-size chunk
//     that does not start on a checkpoint does not blow the CAS read ceiling.
//  5. Source.Read for the verified bytes and positions, source.PlanChunk for
//     every boundary rule.
//  6. IssueChunk persists the issued row first; only then is the receipt signed
//     and the response serialized, so a failed encode or a broken pipe leaves a
//     chunk that was never credited rather than credit for bytes never seen.
//
// The reported Coverage is the state before this chunk: issuing grants nothing,
// confirming does. At end of file the zero-length chunk is still issued -- an
// empty file's coverage can come from nothing else.
func (s *Service) Read(ctx context.Context, req model.ReadChunkRequest) (model.ReadChunkResponse, error) {
	if err := req.Validate(); err != nil {
		return model.ReadChunkResponse{}, err
	}
	rec, err := s.sessions.Session(ctx, req.SessionID, req.ActorID)
	if err != nil {
		return model.ReadChunkResponse{}, err
	}
	if len(req.ConfirmReceipts) > 0 {
		if err := s.confirmReceipts(ctx, rec, req.ConfirmReceipts); err != nil {
			return model.ReadChunkResponse{}, err
		}
	}
	if limit := s.limits.MaxUnconfirmedChunksPerSession; limit > 0 {
		outstanding, err := s.sessions.UnconfirmedChunks(ctx, req.SessionID)
		if err != nil {
			return model.ReadChunkResponse{}, err
		}
		if outstanding >= int64(limit) {
			return model.ReadChunkResponse{}, &model.Error{Code: model.CodeResourceLimit, Message: fmt.Sprintf(
				"session holds %d unconfirmed chunks, at the ceiling of %d; confirm the receipts already issued rather than raising the cap",
				outstanding, limit)}
		}
	}

	// The session's own record of the file: the pinned content hash, the stored
	// size the window is clamped to, and the coverage state as it stands before
	// this chunk. One round trip, and no second coverage-state switch.
	cov, err := s.coverageFor(ctx, req.SessionID, req.ActorID, req.FileID)
	if err != nil {
		return model.ReadChunkResponse{}, err
	}
	if cov.Size < 0 {
		return model.ReadChunkResponse{}, &model.Error{Code: model.CodeInternal, Message: fmt.Sprintf(
			"session records a negative size %d for a scoped file", cov.Size)}
	}
	size := uint64(cov.Size)
	if req.Offset > size {
		return model.ReadChunkResponse{}, &model.Error{Code: model.CodeArgumentInvalid, Message: fmt.Sprintf(
			"read_chunk.offset is %d, past the %d-byte file", req.Offset, size)}
	}

	src, err := s.open(ctx, rec.Binding.SnapshotID)
	if err != nil {
		return model.ReadChunkResponse{}, err
	}
	if src == nil {
		return model.ReadChunkResponse{}, &model.Error{Code: model.CodeInternal,
			Message: "the snapshot source opener returned no source and no error"}
	}

	// Where the view will really start reading. The prefix between it and the
	// offset is read and verified along with the chunk, so it spends the same
	// CAS budget and maxRawForWire has to subtract it.
	checkpoint, err := src.Checkpoint(ctx, req.FileID, req.Offset)
	if err != nil {
		return model.ReadChunkResponse{}, err
	}
	if checkpoint > req.Offset {
		return model.ReadChunkResponse{}, &model.Error{Code: model.CodeInternal, Message: fmt.Sprintf(
			"checkpoint at byte %d is after the %d-byte read offset it precedes", checkpoint, req.Offset)}
	}
	prefix := req.Offset - checkpoint
	raw, err := maxRawForWire(req.MaxBytes, req.Offset, prefix, s.limits)
	if err != nil {
		return model.ReadChunkResponse{}, err
	}
	end := req.Offset + uint64(raw)
	if end > size {
		end = size
	}

	// The read starts at the checkpoint rather than the offset. The bytes are
	// the same ones the view would read either way -- it seeks back to this same
	// checkpoint -- but asking for them explicitly yields a window that begins
	// on a line boundary, which is the one thing source.NewCursorAt needs to
	// report whole-file lines and columns without rescanning from byte zero.
	// That matters because PlanChunk may end the chunk short of the window, and
	// the response's line range has to describe the chunk, not the window.
	window, _, err := src.Read(ctx, req.FileID, model.ByteRange{Start: checkpoint, End: end})
	if err != nil {
		return model.ReadChunkResponse{}, err
	}
	if uint64(len(window.Bytes)) != end-checkpoint {
		return model.ReadChunkResponse{}, &model.Error{Code: model.CodeInternal, Message: fmt.Sprintf(
			"the source returned %d bytes for the %d-byte window [%d,%d)", len(window.Bytes), end-checkpoint, checkpoint, end)}
	}
	cursor, err := source.NewCursorAt(window.Bytes, checkpoint, window.Start.Line)
	if err != nil {
		return model.ReadChunkResponse{}, err
	}
	// PlanChunk owns every boundary rule -- the rune boundary, the complete
	// line, the over-budget split, the base64 fallback and the zero-length end
	// of file -- and needs exactly [offset, min(offset+raw, size)).
	chunk, err := source.PlanChunk(window.Bytes[prefix:], req.Offset, size, raw)
	if err != nil {
		return model.ReadChunkResponse{}, err
	}
	start, err := cursor.PositionAt(chunk.Range.Start)
	if err != nil {
		return model.ReadChunkResponse{}, err
	}
	stop, err := cursor.PositionAt(chunk.Range.End)
	if err != nil {
		return model.ReadChunkResponse{}, err
	}

	// window.Bytes aliases the view's own checkpoint-prefixed buffer, so nothing
	// that outlives this call may hold it. Both encodings below copy the bytes
	// into a fresh string, which is why no clone is taken: cloning would spend a
	// second copy, and keeping the slice would pin the whole prefixed window.
	body := window.Bytes[prefix : prefix+(chunk.Range.End-chunk.Range.Start)]
	content := string(body)
	if chunk.Encoding == model.EncodingBase64 {
		content = base64.StdEncoding.EncodeToString(body)
	}

	chunkID, err := model.NewRandomID()
	if err != nil {
		return model.ReadChunkResponse{}, err
	}
	expires := s.now().Add(s.limits.ReceiptTTL)
	issued := model.IssuedChunk{
		ID: chunkID, SessionID: rec.ID, ActorID: rec.ActorID, FileID: req.FileID,
		ContentHash: cov.ContentHash, Bytes: chunk.Range, ExpiresAt: expires,
	}
	// IssueChunk is also the scope check: it refuses a file or hash outside the
	// pinned scope and a chunk past the stored size, so neither is repeated here.
	if err := s.sessions.IssueChunk(ctx, issued); err != nil {
		return model.ReadChunkResponse{}, err
	}
	receipt, err := s.encodeReceipt(receiptPayload{
		SessionID: rec.ID, ActorID: rec.ActorID, SnapshotID: rec.Binding.SnapshotID,
		FileID: req.FileID, ContentHash: cov.ContentHash,
		Start: chunk.Range.Start, End: chunk.Range.End, ChunkID: chunkID,
	}, expires)
	if err != nil {
		return model.ReadChunkResponse{}, err
	}

	resp := model.ReadChunkResponse{
		Binding:     rec.Binding,
		FileID:      req.FileID,
		ContentHash: cov.ContentHash,
		ByteRange:   chunk.Range,
		LineRange:   model.SourceRange{Start: start, End: stop},
		Encoding:    chunk.Encoding,
		Content:     content,
		PartialLine: chunk.PartialLine,
		NextOffset:  chunk.NextOffset,
		Receipt:     receipt,
		Coverage:    cov.State,
	}
	if err := resp.Validate(); err != nil {
		return model.ReadChunkResponse{}, err
	}
	return resp, nil
}

// coverageFor returns this actor's coverage record for one file in a single
// round trip.
//
// Sessions.Coverage pages by keyset with an exclusive lower bound on file_id,
// so the bound that isolates one file is its immediate predecessor. Identifiers
// are fixed-width 64-character lowercase hex over a 32-byte key, so no other
// identifier can sort between previousFileID(id) and id, and a page of one
// therefore holds that file or the file is not in the session's scope.
func (s *Service) coverageFor(ctx context.Context, session model.SessionID, actor string, file model.FileID) (model.FileCoverage, error) {
	page, err := s.sessions.Coverage(ctx, session, actor, previousFileID(file), 1)
	if err != nil {
		return model.FileCoverage{}, err
	}
	if len(page) == 0 || page[0].FileID != file {
		return model.FileCoverage{}, &model.Error{Code: model.CodeScopeIncomplete,
			Message: "file is not in this session's pinned scope"}
	}
	return page[0], nil
}

// previousFileID returns the identifier immediately below id in the store's
// ordering, or the empty bound when id is the lowest identifier there is. It is
// an ordinary decrement in the hex alphabet, which is order-isomorphic to the
// raw 32-byte key the store actually compares. A non-hex character means the id
// is malformed; the empty bound then lets the store report that itself rather
// than growing a second identifier validator here.
func previousFileID(id model.FileID) model.FileID {
	digits := []byte(id)
	for i := len(digits) - 1; i >= 0; i-- {
		d := indexHex(digits[i])
		switch {
		case d < 0:
			return ""
		case d > 0:
			digits[i] = hexDigits[d-1]
			return model.FileID(digits)
		default:
			digits[i] = hexDigits[len(hexDigits)-1]
		}
	}
	return ""
}

func indexHex(c byte) int {
	for i := 0; i < len(hexDigits); i++ {
		if hexDigits[i] == c {
			return i
		}
	}
	return -1
}
