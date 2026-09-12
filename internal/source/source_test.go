package source

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
)

// sample is the one shared source fixture. It carries every shape that makes
// byte-authoritative positions hard: a multi-byte BMP rune, a non-BMP rune that
// is one UTF-32 code point but a UTF-16 surrogate pair, CRLF line breaks that
// are two bytes and one line break, and a final line with no newline.
//
//	line 1: "h" "é"(2 bytes) "llo"   CRLF
//	line 2: "𝄞"(4 bytes, 2 UTF-16 units) "x"  CRLF
//	line 3: "tail"                    (no newline)
const sample = "héllo\r\n\U0001D11Ex\r\ntail"

// TestColumnConversion protects the shared position contract of Section 9.3:
// provider coordinates in UTF-8, UTF-16 or UTF-32 must resolve to the exact
// byte offset, and a coordinate that does not land on a boundary must be
// rejected rather than guessed. A wrong offset here silently attributes a fact
// to the wrong span of source, which no later validation can detect.
func TestColumnConversion(t *testing.T) {
	data := []byte(sample)
	line2 := uint64(len("héllo\r\n"))
	for _, tc := range []struct {
		name    string
		line    uint32
		column  uint32
		enc     ColumnEncoding
		want    uint64
		wantErr bool
	}{
		{name: "utf8 start of file", line: 1, column: 0, enc: UTF8, want: 0},
		{name: "utf8 after a two-byte rune", line: 1, column: 3, enc: UTF8, want: 3},
		{name: "utf8 into a continuation byte", line: 1, column: 2, enc: UTF8, wantErr: true},
		{name: "utf16 before the surrogate pair", line: 2, column: 0, enc: UTF16, want: line2},
		{name: "utf16 after the surrogate pair", line: 2, column: 2, enc: UTF16, want: line2 + 4},
		{name: "utf16 inside the surrogate pair", line: 2, column: 1, enc: UTF16, wantErr: true},
		{name: "utf32 after the non-BMP rune", line: 2, column: 1, enc: UTF32, want: line2 + 4},
		{name: "utf16 at the end of a CRLF line", line: 2, column: 3, enc: UTF16, want: line2 + 5},
		{name: "utf16 past the end of a CRLF line", line: 2, column: 4, enc: UTF16, wantErr: true},
		{name: "end of the final line without a newline", line: 3, column: 4, enc: UTF8, want: uint64(len(sample))},
		{name: "line zero is not one-based", line: 0, column: 0, enc: UTF8, wantErr: true},
		{name: "line past the end of file", line: 9, column: 0, enc: UTF8, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A fresh cursor and a reused one must agree: the cursor caches the
			// last resolved line, and a stale cache would corrupt every later
			// conversion in the same document.
			for _, c := range []*Cursor{NewCursor(data), reusedCursor(data)} {
				got, err := c.Offset(tc.line, tc.column, tc.enc)
				if tc.wantErr {
					if err == nil {
						t.Fatalf("Offset(%d,%d,%s) = %d, want a rejection", tc.line, tc.column, tc.enc, got)
					}
					var typed *model.Error
					if !errors.As(err, &typed) {
						t.Fatalf("Offset returned %v, want a typed model error", err)
					}
					continue
				}
				if err != nil {
					t.Fatalf("Offset(%d,%d,%s): %v", tc.line, tc.column, tc.enc, err)
				}
				if got != tc.want {
					t.Fatalf("Offset(%d,%d,%s) = %d, want %d", tc.line, tc.column, tc.enc, got, tc.want)
				}
			}
		})
	}
}

// reusedCursor returns a cursor whose cached line is deliberately far from the
// next lookup, so a forward-only optimization that forgets to rescan is caught.
func reusedCursor(data []byte) *Cursor {
	c := NewCursor(data)
	_, _ = c.Offset(3, 0, UTF8)
	return c
}

// TestPositionAtRejectsContinuationByte protects the Section 16.2 rule that a
// read offset landing inside a UTF-8 sequence is rejected: serving from there
// would emit a byte sequence that is not valid text and is not the file's
// content at any boundary.
func TestPositionAtRejectsContinuationByte(t *testing.T) {
	c := NewCursor([]byte(sample))
	if _, err := c.PositionAt(2); err == nil {
		t.Fatal("PositionAt(2) accepted an offset inside a two-byte rune")
	}
	pos, err := c.PositionAt(uint64(len(sample)))
	if err != nil {
		t.Fatalf("PositionAt(EOF): %v", err)
	}
	if pos.Line != 3 || pos.Column != 4 {
		t.Fatalf("PositionAt(EOF) = line %d column %d, want line 3 column 4", pos.Line, pos.Column)
	}
}

// TestIndexMatchesRecomputation protects CAS integrity metadata: the streaming
// pass that computes the whole-file digest, the fixed 64-KiB block digests and
// the sparse line checkpoints must produce exactly what a straightforward
// whole-buffer recomputation produces. A block digest that does not match its
// bytes would let range serving verify a block that was never checked, and a
// wrong checkpoint would report the wrong line for served bytes.
func TestIndexMatchesRecomputation(t *testing.T) {
	var buf bytes.Buffer
	for i := 0; buf.Len() < 3*BlockBytes; i++ {
		buf.WriteString(strings.Repeat("x", 1+i%97))
		if i%3 == 0 {
			buf.WriteString("\r\n")
		} else {
			buf.WriteString("\n")
		}
	}
	data := buf.Bytes()

	idx, err := BuildIndex(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("BuildIndex: %v", err)
	}
	if idx.Size != uint64(len(data)) {
		t.Fatalf("Size = %d, want %d", idx.Size, len(data))
	}
	whole := sha256.Sum256(data)
	if idx.ContentHash != hex.EncodeToString(whole[:]) {
		t.Fatalf("ContentHash = %s, want %s", idx.ContentHash, hex.EncodeToString(whole[:]))
	}

	wantBlocks := (len(data) + BlockBytes - 1) / BlockBytes
	if len(idx.Blocks) != wantBlocks {
		t.Fatalf("got %d block digests, want %d", len(idx.Blocks), wantBlocks)
	}
	for i, got := range idx.Blocks {
		end := min((i+1)*BlockBytes, len(data))
		sum := sha256.Sum256(data[i*BlockBytes : end])
		if got != hex.EncodeToString(sum[:]) {
			t.Fatalf("block %d digest = %s, want %s", i, got, hex.EncodeToString(sum[:]))
		}
	}

	for _, cp := range idx.Checkpoints {
		if cp.Byte != 0 && data[cp.Byte-1] != '\n' {
			t.Fatalf("checkpoint at byte %d is not a line start", cp.Byte)
		}
		// The line number a checkpoint claims must equal the number of line
		// breaks before it, counted the obvious way.
		want := uint32(bytes.Count(data[:cp.Byte], []byte("\n"))) + 1
		if cp.Line != want {
			t.Fatalf("checkpoint at byte %d claims line %d, want %d", cp.Byte, cp.Line, want)
		}
	}
	if len(idx.Checkpoints) == 0 || idx.Checkpoints[0].Byte != 0 || idx.Checkpoints[0].Line != 1 {
		t.Fatalf("checkpoints must start at byte 0 line 1, got %v", idx.Checkpoints)
	}
	if got := idx.CheckpointFor(idx.Size); got.Byte > idx.Size {
		t.Fatalf("CheckpointFor(EOF) returned byte %d beyond the file", got.Byte)
	}
}

// TestPlanChunkBoundaries protects the Section 16.2 chunking contract: a chunk
// ends on a complete UTF-8 sequence and a complete line when it can, a line
// longer than the budget still makes progress and is flagged partial, invalid
// UTF-8 is carried losslessly as base64, and an offset into a continuation byte
// is rejected.
func TestPlanChunkBoundaries(t *testing.T) {
	text := []byte("alpha\r\nbeta\r\ngamma")
	chunk, err := PlanChunk(text[:9], 0, uint64(len(text)), 9)
	if err != nil {
		t.Fatalf("PlanChunk: %v", err)
	}
	if chunk.Range.End != 7 || chunk.PartialLine {
		t.Fatalf("chunk = %+v, want a complete first line ending at byte 7", chunk)
	}
	if chunk.Encoding != model.EncodingUTF8 || chunk.NextOffset == nil || *chunk.NextOffset != 7 {
		t.Fatalf("chunk = %+v, want utf8 with next offset 7", chunk)
	}

	long := []byte("éééé")
	chunk, err = PlanChunk(long[:5], 0, uint64(len(long)), 5)
	if err != nil {
		t.Fatalf("PlanChunk on a long line: %v", err)
	}
	if chunk.Range.End != 4 || !chunk.PartialLine {
		t.Fatalf("chunk = %+v, want a partial line cut at the rune boundary 4", chunk)
	}

	binary := []byte{0xff, 0xfe, 'a', '\n'}
	chunk, err = PlanChunk(binary, 0, uint64(len(binary)), 4)
	if err != nil {
		t.Fatalf("PlanChunk on invalid UTF-8: %v", err)
	}
	if chunk.Encoding != model.EncodingBase64 {
		t.Fatalf("chunk encoding = %s, want base64 for invalid UTF-8", chunk.Encoding)
	}

	if _, err := PlanChunk([]byte(sample)[2:], 2, uint64(len(sample)), 16); err == nil {
		t.Fatal("PlanChunk accepted an offset inside a UTF-8 sequence")
	}

	tail := []byte("alpha\nbeta")
	chunk, err = PlanChunk(tail, 0, uint64(len(tail)), 64)
	if err != nil {
		t.Fatalf("PlanChunk on a file without a trailing newline: %v", err)
	}
	if chunk.PartialLine || chunk.NextOffset != nil || chunk.Range.End != uint64(len(tail)) {
		t.Fatalf("chunk = %+v, want the whole file, complete, at end of file", chunk)
	}

	empty, err := PlanChunk(nil, 0, 0, 64)
	if err != nil {
		t.Fatalf("PlanChunk on an empty file: %v", err)
	}
	if empty.Range.Start != 0 || empty.Range.End != 0 || empty.NextOffset != nil {
		t.Fatalf("empty-file chunk = %+v, want the zero-length EOF chunk", empty)
	}
}
