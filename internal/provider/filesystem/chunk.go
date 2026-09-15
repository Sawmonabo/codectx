package filesystem

import (
	"bytes"
	"context"
	"io"
	"strconv"
	"unicode/utf8"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/source"
)

// Chunking bounds of Section 11.2: a lexical chunk is at most 32 KiB, is cut
// on a line boundary when one fits, overlaps the previous chunk by at most
// two lines, and those lines are themselves bounded so a pathological line
// cannot turn the overlap into a second copy of the chunk. A line longer than
// a chunk is split at a legal boundary rather than producing an oversized
// document.
const (
	ChunkBytes      = model.MaxSearchBodyBytes
	MaxOverlapLines = 2
	MaxOverlapBytes = 1024
	// binarySniffBytes is how much of a file's head the binary decision
	// inspects.
	binarySniffBytes = 8000
	// binaryNonTextPercent is the share of the sniffed head that must be
	// bytes no text encoding the index reads can produce before the file is
	// judged binary and left without a lexical index.
	binaryNonTextPercent = 30
	// binaryNULRun is a run of NUL bytes long enough to be a binary
	// container's padding rather than a stray byte in a text file.
	binaryNULRun = 4
)

// chunker streams one file through a window of ChunkBytes and emits the
// lexical documents of its owning unit. It never holds more than one window
// plus the overlap carried into the next.
type chunker struct {
	r        io.Reader
	size     uint64
	buf      []byte
	filled   int
	start    uint64
	eof      bool
	consumed uint64
	// scratch holds the sanitized copy of a chunk that is not valid UTF-8.
	// It is reused across chunks, so a file of such chunks costs one extra
	// window, not one per chunk.
	scratch []byte
}

// newChunker primes the window so the caller can sniff the head before any
// fact is emitted.
func newChunker(r io.Reader, size uint64) (*chunker, error) {
	c := &chunker{r: r, size: size, buf: make([]byte, ChunkBytes)}
	return c, c.fill()
}

// head is the currently buffered bytes, for sniffing.
func (c *chunker) head() []byte { return c.buf[:c.filled] }

// fill tops the window up to its capacity or end of file.
func (c *chunker) fill() error {
	for c.filled < len(c.buf) && !c.eof {
		n, err := c.r.Read(c.buf[c.filled:])
		c.filled += n
		c.consumed += uint64(n)
		if err == io.EOF {
			c.eof = true
			break
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// lossy counts what sanitizeUTF8 had to substitute so the unit can disclose
// it: how many chunks carried bytes that are not UTF-8 under any reading, and
// how many bytes were substituted across those chunks. Bytes is counted per
// emitted chunk, so a byte that falls in the overlap two chunks share is
// counted in both -- it is a measure of the substitution done, not of the
// distinct bytes of the file.
type lossy struct {
	Chunks int
	Bytes  int
	// NULs counts NUL bytes replaced across the emitted chunks. They are
	// counted apart from Bytes because a NUL is well-formed UTF-8: the file
	// is not mis-encoded, it simply carries a byte a text index cannot hold,
	// and the two disclosures answer different questions.
	NULs int
}

// each emits every chunk of the file through emit. Every byte of the file is
// covered by the chunks it emits: a chunk whose bytes are not valid UTF-8 is
// still indexed, with the offending bytes substituted by sanitizeUTF8, and the
// substitution is counted in the returned lossy so the unit can report it. No
// content is left out -- the original bytes also remain served losslessly (as
// base64) by source reads.
func (c *chunker) each(ctx context.Context, emit func(model.ByteRange, []byte) error) (lost lossy, err error) {
	for {
		if err := ctx.Err(); err != nil {
			return lost, model.Canceled(err)
		}
		window := c.buf[:c.filled]
		chunk, err := source.PlanChunk(window, c.start, c.size, uint32(ChunkBytes))
		if err != nil {
			return lost, err
		}
		if chunk.Range.End == chunk.Range.Start {
			return lost, nil
		}
		// body stays a slice of the window: overlap and the carry below read
		// it, and the next chunk's boundary rules are stated over the file's
		// own bytes. A chunk that is not valid UTF-8 is indexed from a
		// sanitized copy held in scratch instead.
		body := window[:chunk.Range.End-chunk.Range.Start]
		text := body
		// A NUL is valid UTF-8, so the encoding alone does not say whether a
		// chunk needs sanitising: a text file with a stray NUL would carry it
		// straight into the index body. Both conditions are checked.
		if chunk.Encoding != model.EncodingUTF8 || bytes.IndexByte(body, 0) >= 0 {
			var substituted, nuls int
			c.scratch, substituted, nuls = sanitizeUTF8(c.scratch, body)
			text = c.scratch
			lost.Chunks++
			lost.Bytes += substituted
			lost.NULs += nuls
		}
		if err := emit(chunk.Range, text); err != nil {
			return lost, err
		}
		if chunk.NextOffset == nil {
			return lost, nil
		}
		keep := 0
		if !chunk.PartialLine {
			keep = overlap(body)
		}
		next := chunk.Range.End - uint64(keep)
		drop := int(next - c.start)
		c.filled = copy(c.buf, c.buf[drop:c.filled])
		c.start = next
		if err := c.fill(); err != nil {
			return lost, err
		}
	}
}

// overlap returns how many trailing bytes of a line-complete chunk the next
// chunk repeats: up to MaxOverlapLines whole lines, provided together they
// stay within MaxOverlapBytes and leave the next chunk making progress.
func overlap(body []byte) int {
	if len(body) == 0 || body[len(body)-1] != '\n' {
		return 0
	}
	end := len(body) - 1 // the trailing newline
	start := len(body)
	for lines := 0; lines < MaxOverlapLines; lines++ {
		prev := bytes.LastIndexByte(body[:end], '\n')
		lineStart := prev + 1
		if len(body)-lineStart > MaxOverlapBytes {
			break
		}
		start = lineStart
		if prev < 0 {
			break
		}
		end = prev
	}
	ov := len(body) - start
	if ov >= len(body) {
		return 0
	}
	return ov
}

// sanitizeUTF8 returns a valid-UTF-8 copy of src in which every byte that is
// not part of a well-formed UTF-8 sequence, and every NUL, is replaced byte
// for byte by a space, along with how many bytes of each kind were replaced.
// dst is reused as the buffer.
//
// The substitution is byte-for-byte, not U+FFFD, for two reasons. A search
// document's body is bounded at model.MaxSearchBodyBytes, which is exactly
// ChunkBytes, so any expanding transcode would turn a full chunk holding one
// bad byte into a record storage refuses -- the same content loss this
// replaced, relocated. And keeping the length equal to the chunk's byte range
// means the body's offsets remain the file's offsets: model.SearchUnit.Bytes
// describes exactly the bytes the body stands for. A space is the neutral
// substitute: the replaced bytes are text in no encoding the index claims to
// read, and a space can only separate tokens, never join two into one that the
// file does not contain.
func sanitizeUTF8(dst, src []byte) (out []byte, substituted, nuls int) {
	dst = append(dst[:0], src...)
	for i := 0; i < len(dst); {
		if dst[i] == 0 {
			dst[i] = ' '
			nuls++
			i++
			continue
		}
		if dst[i] < utf8.RuneSelf {
			i++
			continue
		}
		r, size := utf8.DecodeRune(dst[i:])
		if r == utf8.RuneError && size <= 1 {
			dst[i] = ' '
			substituted++
			i++
			continue
		}
		i += size
	}
	return dst, substituted, nuls
}

// looksBinary decides whether a file's head is content no text index can
// read. A single NUL byte does not decide it: a source file can carry one and
// still be text the caller expects to search, and refusing it a lexical index
// skips that file's content under default settings. What decides it is the
// proportion of the sniffed head that is not text under any encoding the index
// reads -- NUL, a C0 control that is not whitespace, DEL, or a byte that is
// not part of a well-formed UTF-8 sequence -- or a run of NULs long enough to
// be a container's padding.
//
// Counting invalid UTF-8 towards the proportion is what separates a binary
// from a legacy single-byte encoding: a Windows-1252 document has a fraction
// of a percent of such bytes, while arbitrary binary content is roughly half
// of them. A UTF-16 file is caught by the proportion too, its NULs being
// interleaved rather than in a run.
func looksBinary(head []byte) bool {
	if len(head) > binarySniffBytes {
		head = head[:binarySniffBytes]
	}
	if len(head) == 0 {
		return false
	}
	nonText, run := 0, 0
	for i := 0; i < len(head); {
		b := head[i]
		switch {
		case b == 0:
			run++
			if run >= binaryNULRun {
				return true
			}
			nonText++
			i++
			continue
		case b == 0x7f || (b < 0x20 && b != '\t' && b != '\n' && b != '\r' && b != '\f' && b != '\v' && b != '\b' && b != 0x1b):
			nonText++
			i++
		case b < utf8.RuneSelf:
			i++
		default:
			r, size := utf8.DecodeRune(head[i:])
			if r == utf8.RuneError && size <= 1 {
				// A sequence cut short by the edge of the sniffed head is the
				// window's doing, not the file's, so it ends the count here
				// rather than being charged against the file.
				if len(head)-i < utf8.UTFMax && utf8.RuneStart(b) {
					i = len(head)
					break
				}
				nonText++
			}
			i += size
		}
		run = 0
	}
	return nonText*100 > len(head)*binaryNonTextPercent
}

// chunkDocument is the search document of one source chunk. Its key derives
// from the file, its content hash and the exact byte range, so the same bytes
// at the same offsets always produce the same document.
func chunkDocument(file model.FileVersion, node model.NodeID, r model.ByteRange, body []byte) model.SearchUnit {
	return model.SearchUnit{
		ID:     model.H("search-chunk-v1", string(file.ID), file.ContentHash, strconv.FormatUint(r.Start, 10), strconv.FormatUint(r.End, 10)),
		NodeID: node, FileID: file.ID, Path: file.Path, Kind: model.NodeFile,
		Bytes: r, Body: string(body),
	}
}
