// Package source owns byte/range/encoding conversion for the whole
// application. It is the one implementation of the Section 9.3 coordinate
// contract: byte offsets are authoritative, lines are one-based, columns are
// zero-based UTF-8 byte columns, CRLF is two bytes and one line break, and an
// EOF position is valid.
//
// Providers hand it UTF-8, UTF-16 or UTF-32 coordinates and it converts them
// against the exact bytes. It never guesses an unspecified encoding and never
// repairs a coordinate that does not land on a boundary: a silently adjusted
// position attributes a fact to source it does not describe, and nothing
// downstream can detect that.
package source

import (
	"bytes"
	"fmt"
	"unicode/utf8"

	"github.com/Sawmonabo/codectx/internal/model"
)

// ColumnEncoding is the code-unit encoding a provider's columns are counted in.
// SCIP defines all three explicitly; LSP defaults to UTF-16 and negotiates.
type ColumnEncoding string

const (
	UTF8  ColumnEncoding = "utf8"
	UTF16 ColumnEncoding = "utf16"
	UTF32 ColumnEncoding = "utf32"
)

// Valid reports whether e is a known encoding.
func (e ColumnEncoding) Valid() bool {
	return e == UTF8 || e == UTF16 || e == UTF32
}

// Cursor converts coordinates against one file's exact bytes.
//
// It caches the most recently resolved line so a document's occurrences, which
// arrive in roughly increasing order, cost one forward scan overall instead of
// one whole-file scan each. The cache is only ever an optimization: a lookup
// before the cached line rescans from the start, so the result never depends on
// the order of the calls.
//
// A Cursor borrows its buffer and must not outlive it. It is not safe for
// concurrent use.
type Cursor struct {
	data []byte
	// base is the file offset of data[0] and baseLine the one-based line that
	// starts there. Both are zero and one for a whole-file cursor.
	base     uint64
	baseLine uint32
	// line and lineStart are the cached line number and its first byte, the
	// latter relative to data.
	line      uint32
	lineStart int
}

// NewCursor returns a cursor over one file's exact bytes.
func NewCursor(data []byte) *Cursor {
	return &Cursor{data: data, baseLine: 1, line: 1, lineStart: 0}
}

// NewCursorAt returns a cursor over a window of a file: data holds the bytes
// beginning at startByte, which is on the one-based line startLine, and before
// holds the BoundaryContext bytes preceding startByte, fewer only when they
// begin at a line start or the start of the file. A position on startLine
// reports its column from startByte, so a window that begins mid-line is the
// caller's to correct.
//
// This is what makes the sparse checkpoints of an Index usable. Serving a range
// from the middle of a large file would otherwise have to rescan the file from
// byte zero to learn a line number, or grow a second position implementation
// that counts lines its own way and disagrees with this one at the first CRLF.
// Both coordinates are reported in whole-file terms.
//
// A window that starts inside a UTF-8 sequence is rejected. Every later
// offset of an accepted window is then judged from the window alone: a
// well-formed sequence covering a later byte cannot begin before a legal
// start. The window's own end is taken as the file's, so a caller that asks
// for a position in its last BoundaryContext bytes either holds the window to
// the end of the file or asks at a boundary PlanChunk already chose.
func NewCursorAt(before, data []byte, startByte uint64, startLine uint32) (*Cursor, error) {
	if startLine == 0 {
		return nil, invalid("line numbers are one-based; a window cannot start at line 0")
	}
	if !boundary(before, data) {
		return nil, invalid("the window starts at byte %d, inside a UTF-8 sequence", startByte)
	}
	return &Cursor{data: data, base: startByte, baseLine: startLine, line: startLine, lineStart: 0}, nil
}

// Offset converts a one-based line and a column counted in enc to the byte
// offset it names. The returned offset may equal the file size: an EOF position
// is valid.
func (c *Cursor) Offset(line, column uint32, enc ColumnEncoding) (uint64, error) {
	if !enc.Valid() {
		return 0, invalid("column encoding %q is not utf8, utf16 or utf32; an unspecified encoding is never guessed", enc)
	}
	start, err := c.lineStartOf(line)
	if err != nil {
		return 0, err
	}
	content := c.data[start:lineContentEnd(c.data, start)]
	offset, err := columnOffset(content, column, enc)
	if err != nil {
		return 0, invalid("line %d column %d (%s): %v", line, column, enc, err)
	}
	return c.base + uint64(start+offset), nil
}

// SourceRange converts a provider's four coordinates to the half-open byte
// range plus line/column context of Section 9.3. Providers use this rather than
// two Offset calls so the well-ordering check lives in one place.
func (c *Cursor) SourceRange(startLine, startColumn, endLine, endColumn uint32, enc ColumnEncoding) (model.SourceRange, error) {
	startByte, err := c.Offset(startLine, startColumn, enc)
	if err != nil {
		return model.SourceRange{}, err
	}
	endByte, err := c.Offset(endLine, endColumn, enc)
	if err != nil {
		return model.SourceRange{}, err
	}
	if endByte < startByte {
		return model.SourceRange{}, invalid("range ends at byte %d before it starts at %d", endByte, startByte)
	}
	// The byte columns are recomputed from the offsets rather than carried over
	// from the provider's units, which is what makes the returned range
	// byte-authoritative in every encoding.
	start, err := c.PositionAt(startByte)
	if err != nil {
		return model.SourceRange{}, err
	}
	end, err := c.PositionAt(endByte)
	if err != nil {
		return model.SourceRange{}, err
	}
	r := model.SourceRange{Start: start, End: end}
	if err := r.Validate("source_range"); err != nil {
		return model.SourceRange{}, err
	}
	return r, nil
}

// PositionAt reports the line and UTF-8 byte column of one byte offset. The
// offset must be a boundary: Section 16.2 rejects an offset inside a
// well-formed UTF-8 sequence rather than serving from the middle of a
// character.
func (c *Cursor) PositionAt(offset uint64) (model.Position, error) {
	if offset < c.base {
		return model.Position{}, invalid("byte offset %d is before the window, which starts at %d", offset, c.base)
	}
	if offset-c.base > uint64(len(c.data)) {
		return model.Position{}, invalid("byte offset %d is past the %d bytes at offset %d", offset, len(c.data), c.base)
	}
	idx := int(offset - c.base)
	if !boundary(c.data[max(0, idx-BoundaryContext):idx], c.data[idx:]) {
		return model.Position{}, invalid("byte offset %d is inside a UTF-8 sequence", offset)
	}
	// Counting from the start is correct regardless of the cache, and the cache
	// makes the common forward case cheap.
	line, start := c.baseLine, 0
	if c.lineStart <= idx {
		line, start = c.line, c.lineStart
	}
	for {
		next := bytes.IndexByte(c.data[start:], '\n')
		if next < 0 || start+next >= idx {
			break
		}
		start += next + 1
		line++
	}
	c.line, c.lineStart = line, start
	return model.Position{Byte: offset, Line: line, Column: uint32(idx - start)}, nil
}

// lineStartOf returns the first byte of a one-based line. The position just
// past a trailing newline is a valid empty final line.
func (c *Cursor) lineStartOf(line uint32) (int, error) {
	if line < 1 {
		return 0, invalid("line %d is not one-based", line)
	}
	if line < c.baseLine {
		return 0, invalid("line %d is before line %d, where this window starts", line, c.baseLine)
	}
	current, start := c.baseLine, 0
	if c.line <= line {
		current, start = c.line, c.lineStart
	}
	for current < line {
		next := bytes.IndexByte(c.data[start:], '\n')
		if next < 0 {
			return 0, invalid("line %d is past the end of a %d-line file", line, current)
		}
		start += next + 1
		current++
	}
	if start > len(c.data) {
		return 0, invalid("line %d is past the end of the file", line)
	}
	c.line, c.lineStart = current, start
	return start, nil
}

// lineContentEnd returns the end of a line's content: the offset of its
// terminating "\n", minus the preceding "\r" of a CRLF pair, or the end of the
// file for a final line without a newline. CRLF is one line break, so neither
// of its two bytes is part of the line's content.
func lineContentEnd(data []byte, start int) int {
	next := bytes.IndexByte(data[start:], '\n')
	if next < 0 {
		return len(data)
	}
	end := start + next
	if end > start && data[end-1] == '\r' {
		end--
	}
	return end
}

// columnOffset converts a column counted in enc to a byte offset within one
// line's content. A column that lands between the units of one code point is
// rejected, as is a column past the end of the line: both mean the provider's
// coordinates do not describe these bytes.
func columnOffset(content []byte, column uint32, enc ColumnEncoding) (int, error) {
	if enc == UTF8 {
		if int(column) > len(content) {
			return 0, fmt.Errorf("column is past the %d-byte line", len(content))
		}
		// content begins at a legal line start and ends at a line break or the
		// end of the file, neither of which a sequence crosses.
		if !boundary(content[max(0, int(column)-BoundaryContext):column], content[column:]) {
			return 0, fmt.Errorf("column is inside a UTF-8 sequence")
		}
		return int(column), nil
	}
	var units uint32
	for i := 0; i < len(content); {
		if units == column {
			return i, nil
		}
		r, size := utf8.DecodeRune(content[i:])
		if r == utf8.RuneError && size <= 1 {
			return 0, fmt.Errorf("the line is not valid UTF-8, so a %s column cannot be counted", enc)
		}
		width := uint32(1)
		if enc == UTF16 && r > 0xFFFF {
			width = 2
		}
		if units+width > column {
			return 0, fmt.Errorf("column falls inside a single code point")
		}
		units += width
		i += size
	}
	if units == column {
		return len(content), nil
	}
	return 0, fmt.Errorf("column is past the end of the line, which holds %d %s units", units, enc)
}

// BoundaryContext is how many bytes on each side of an offset decide whether
// it is a boundary: a UTF-8 sequence is at most utf8.UTFMax bytes long.
const BoundaryContext = utf8.UTFMax - 1

// boundary reports whether the offset between before and after is a legal
// boundary (Section 16.2). It is illegal only when it is inside a well-formed
// UTF-8 sequence: when the byte at it continues a sequence that begins at most
// BoundaryContext bytes earlier and decodes well-formed past it. A
// continuation byte that belongs to no well-formed sequence is a legal
// boundary -- a stray byte of invalid UTF-8, served as base64 -- so every byte
// of every file can be served; rejecting every continuation byte instead would
// leave a long enough run of them with no legal boundary at all.
//
// before holds the bytes preceding the offset, of which only the last
// BoundaryContext matter; fewer are enough when they begin at a line start or
// the start of the file, since no sequence crosses a line break. after holds the bytes from the offset to the end of the file, or at
// least BoundaryContext of them: a sequence cut short by the end of after is
// judged as if the file ended there.
func boundary(before, after []byte) bool {
	if len(after) == 0 || utf8.RuneStart(after[0]) {
		return true
	}
	// Only the nearest byte that is not a continuation byte can begin a
	// sequence covering the offset: an earlier one's sequence would have to
	// run through it.
	for k := 1; k <= BoundaryContext && k <= len(before); k++ {
		if lead := before[len(before)-k]; utf8.RuneStart(lead) {
			var seq [utf8.UTFMax]byte
			n := copy(seq[:], before[len(before)-k:])
			n += copy(seq[n:], after)
			// An invalid encoding decodes with size 1, so size > k holds
			// exactly for a well-formed sequence that runs past the offset.
			_, size := utf8.DecodeRune(seq[:n])
			return size <= k
		}
	}
	return true
}

// Lookbehind keeps the last BoundaryContext bytes of a stream consumed in
// pieces of any length: the bytes before the stream's current offset that
// boundary needs. Its zero value is a stream at a line start or the start of
// the file, where none are needed.
type Lookbehind struct {
	b [BoundaryContext]byte
	n int
}

// Push records p as the next bytes consumed.
func (l *Lookbehind) Push(p []byte) {
	if len(p) >= BoundaryContext {
		l.n = copy(l.b[:], p[len(p)-BoundaryContext:])
		return
	}
	kept := min(l.n, BoundaryContext-len(p))
	copy(l.b[:], l.b[l.n-kept:l.n])
	l.n = kept + copy(l.b[kept:], p)
}

// Bytes returns the kept bytes, valid until the next Push.
func (l *Lookbehind) Bytes() []byte { return l.b[:l.n] }

func invalid(format string, args ...any) *model.Error {
	return &model.Error{Code: model.CodeArgumentInvalid, Message: fmt.Sprintf(format, args...)}
}

// Walker resolves one byte offset's line and byte column from a blob read in
// chunks, for the caller that cannot hold the bytes between its anchor and the
// offset at once.
//
// Cursor is the right shape when the window is small: it borrows the whole
// window and answers repeatedly inside it. It cannot serve a window that does
// not begin on a line boundary, and one line CAN be longer than any read
// ceiling -- a minified bundle or a generated data file is a single line of
// megabytes, and no line checkpoint exists inside it to anchor on. A Walker
// keeps only the anchor (the current line and where it started) and consumes
// the span one chunk at a time, so the peak is the chunk and the cost is
// O(span) bytes read however far the nearest checkpoint is. The counting rules
// are the same ones Cursor applies: lines are one-based, "\n" ends a line and
// the "\r" of a CRLF belongs to the line it terminates, so a column is the
// count of bytes since the last "\n".
//
// A Walker is single-use and moves only forward.
type Walker struct {
	// at is the file offset the walker has consumed up to.
	at uint64
	// line is the one-based line holding at, and lineStart the file offset of
	// that line's first byte.
	line      uint32
	lineStart uint64
	// tail is the bytes consumed before at, which decide whether at is a
	// boundary. A walk starts at a line start, which needs none.
	tail Lookbehind
}

// NewWalker starts a walk at a checkpoint, whose Byte must be the first byte of
// the one-based line Line -- exactly what Index.CheckpointFor returns.
func NewWalker(cp Checkpoint) (*Walker, error) {
	if cp.Line == 0 {
		return nil, invalid("line numbers are one-based; a walk cannot start at line 0")
	}
	return &Walker{at: cp.Byte, line: cp.Line, lineStart: cp.Byte}, nil
}

// At reports the file offset the walk has reached: the offset the next chunk
// must begin at, and the offset PositionAt describes.
func (w *Walker) At() uint64 { return w.at }

// Advance consumes the chunk of file bytes beginning at At().
func (w *Walker) Advance(chunk []byte) {
	if last := bytes.LastIndexByte(chunk, '\n'); last >= 0 {
		w.line += uint32(bytes.Count(chunk, []byte{'\n'}))
		w.lineStart = w.at + uint64(last) + 1
	}
	w.at += uint64(len(chunk))
	w.tail.Push(chunk)
}

// PositionAt reports the position of the offset the walk has reached. next
// holds the file's bytes from that offset -- BoundaryContext of them, fewer
// only at the end of the file -- so the offset is rejected inside a
// well-formed UTF-8 sequence rather than served from the middle of a
// character, as Cursor.PositionAt does. The bytes before it are the ones the
// walk consumed.
func (w *Walker) PositionAt(next []byte) (model.Position, error) {
	if !boundary(w.tail.Bytes(), next) {
		return model.Position{}, invalid("byte offset %d is inside a UTF-8 sequence", w.at)
	}
	return model.Position{Byte: w.at, Line: w.line, Column: uint32(w.at - w.lineStart)}, nil
}
