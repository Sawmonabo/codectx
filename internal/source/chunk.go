package source

import (
	"bytes"
	"unicode/utf8"

	"github.com/Sawmonabo/codectx/internal/model"
)

// Chunk is the boundary decision for one bounded source read (Section 16.2).
// It carries no bytes: the caller already holds the window and slices it.
type Chunk struct {
	Range       model.ByteRange
	Encoding    model.ChunkEncoding
	PartialLine bool
	// NextOffset is the offset of the next chunk, or nil at end of file.
	NextOffset *uint64
}

// PlanChunk chooses the end of one chunk starting at windowStart.
//
// window must hold exactly the bytes [windowStart, min(windowStart+maxBytes,
// fileSize)) of the file: the plan only ever shrinks the window, so no
// lookahead beyond it is needed.
//
// The rules are Section 16.2's, in order: reject an offset inside a UTF-8
// sequence; prefer a complete UTF-8 sequence and a complete line when they fit;
// split a line longer than the budget at a valid boundary and report
// partial_line rather than returning no progress; carry invalid UTF-8
// losslessly as base64 with byte-based ranges instead of coercing it into
// replacement characters; and treat the zero-length position at end of file as
// a valid response.
func PlanChunk(window []byte, windowStart, fileSize uint64, maxBytes uint32) (Chunk, error) {
	if windowStart > fileSize {
		return Chunk{}, invalid("offset %d is past the %d-byte file", windowStart, fileSize)
	}
	available := fileSize - windowStart
	if uint64(len(window)) > available {
		return Chunk{}, invalid("the window holds %d bytes but only %d remain in the file", len(window), available)
	}
	if uint64(len(window)) < min(available, uint64(maxBytes)) {
		return Chunk{}, invalid("the window holds %d bytes, short of the %d the chunk may cover", len(window), min(available, uint64(maxBytes)))
	}
	if windowStart == fileSize {
		// The EOF chunk of an empty or fully read file. Section 16.3 requires
		// this zero-length receipt to exist and to be confirmable.
		return Chunk{
			Range:    model.ByteRange{Start: windowStart, End: windowStart},
			Encoding: model.EncodingUTF8,
		}, nil
	}
	// The budget must hold at least one code point, but only for a chunk that
	// must carry bytes: Sections 16.2 and 16.3 make the zero-length end-of-file
	// receipt valid on its own terms, and refusing it would make an empty file
	// unreadable and unconfirmable.
	if maxBytes < utf8.UTFMax {
		return Chunk{}, invalid("a chunk budget of %d bytes cannot carry one UTF-8 code point", maxBytes)
	}
	if !utf8.RuneStart(window[0]) {
		return Chunk{}, invalid("offset %d is inside a UTF-8 sequence", windowStart)
	}

	end := len(window)
	if uint64(end) > uint64(maxBytes) {
		end = int(maxBytes)
	}
	atEOF := windowStart+uint64(end) == fileSize
	body := window[:end]

	partial := false
	if !atEOF {
		// Back off to a rune boundary, whatever the window holds. The trim is
		// not about decodability -- it is what keeps the next chunk's start
		// legal: a boundary in the middle of a UTF-8 sequence is rejected by
		// the guard above, so a chunk cut mid-sequence would fail the very
		// next call and with it the whole unit. Bytes that are not text at
		// all cost nothing here: the trim only ever moves the boundary, it
		// never drops a byte, and the next chunk carries what it gave back.
		if trimmed := trimToRuneBoundary(body); trimmed != len(body) {
			end = trimmed
			body = window[:end]
		}
		if lineEnd := bytes.LastIndexByte(body, '\n'); lineEnd >= 0 {
			end = lineEnd + 1
			body = window[:end]
		} else {
			// A single line longer than the budget: cut it where it is legal
			// and say so, rather than returning zero progress while waiting
			// for a newline that may never come.
			partial = true
		}
	}
	// A final line without a newline is preserved as it is, not reported as
	// partial: partial_line means a line was split, and this one was not.
	if end == 0 {
		return Chunk{}, invalid("a %d-byte budget cannot make progress at offset %d", maxBytes, windowStart)
	}

	chunk := Chunk{
		Range:       model.ByteRange{Start: windowStart, End: windowStart + uint64(end)},
		Encoding:    model.EncodingUTF8,
		PartialLine: partial,
	}
	if !utf8.Valid(body) {
		chunk.Encoding = model.EncodingBase64
	}
	if chunk.Range.End < fileSize {
		next := chunk.Range.End
		chunk.NextOffset = &next
	}
	return chunk, nil
}

// trimToRuneBoundary returns the length of the longest prefix of body that
// ends on a rune boundary, so the next chunk starts on a byte utf8.RuneStart
// accepts. It walks back over a run of continuation bytes of any length, not
// just the at most three a well-formed sequence can have: a file may hold an
// arbitrarily long run of them, and stopping the walk early would hand the
// next chunk an illegal start offset and fail the unit.
//
// It never returns zero. When body holds no rune boundary after its first
// byte, every cut but the full window would leave the chunk making no
// progress, so the window is kept whole; its trailing bytes are not a
// sequence under any reading and the caller carries them as base64 or
// sanitises them.
func trimToRuneBoundary(body []byte) int {
	last := -1
	for i := len(body) - 1; i >= 0; i-- {
		if utf8.RuneStart(body[i]) {
			last = i
			break
		}
	}
	if last <= 0 {
		return len(body)
	}
	// A complete sequence ending exactly at the window edge is already on a
	// boundary. Anything else -- a sequence cut short by the edge, an invalid
	// start byte, or stray continuation bytes after a complete rune -- is
	// given back to the next chunk, which then starts at last.
	if r, size := utf8.DecodeRune(body[last:]); (r != utf8.RuneError || size > 1) && last+size == len(body) {
		return len(body)
	}
	return last
}
