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
	// line and lineStart are the cached line number and its first byte.
	line      uint32
	lineStart int
}

// NewCursor returns a cursor over one file's exact bytes.
func NewCursor(data []byte) *Cursor {
	return &Cursor{data: data, line: 1, lineStart: 0}
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
	return uint64(start + offset), nil
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
// offset must be a rune boundary: Section 16.2 rejects an offset into a
// continuation byte rather than serving from the middle of a character.
func (c *Cursor) PositionAt(offset uint64) (model.Position, error) {
	if offset > uint64(len(c.data)) {
		return model.Position{}, invalid("byte offset %d is past the %d-byte file", offset, len(c.data))
	}
	idx := int(offset)
	if idx < len(c.data) && !utf8.RuneStart(c.data[idx]) {
		return model.Position{}, invalid("byte offset %d is inside a UTF-8 sequence", offset)
	}
	// Counting from the start is correct regardless of the cache, and the cache
	// makes the common forward case cheap.
	line, start := uint32(1), 0
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
	current, start := uint32(1), 0
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
		if int(column) < len(content) && !utf8.RuneStart(content[column]) {
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

func invalid(format string, args ...any) *model.Error {
	return &model.Error{Code: model.CodeArgumentInvalid, Message: fmt.Sprintf(format, args...)}
}
