package filesystem

import (
	"bytes"
	"context"
	"io"
	"strconv"

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
	// binarySniffBytes is how much of a file's head is inspected for a NUL
	// byte, Git's own heuristic for binary content.
	binarySniffBytes = 8000
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

// each emits every chunk of the file through emit. It returns the number of
// chunks skipped because their bytes were not UTF-8 text: those bytes are
// still served losslessly by source reads, but a lexical index of them
// would misrepresent the source.
func (c *chunker) each(ctx context.Context, emit func(model.ByteRange, []byte) error) (skipped int, err error) {
	for {
		if err := ctx.Err(); err != nil {
			return skipped, model.Canceled(err)
		}
		window := c.buf[:c.filled]
		chunk, err := source.PlanChunk(window, c.start, c.size, uint32(ChunkBytes))
		if err != nil {
			return skipped, err
		}
		if chunk.Range.End == chunk.Range.Start {
			return skipped, nil
		}
		body := window[:chunk.Range.End-chunk.Range.Start]
		if chunk.Encoding == model.EncodingUTF8 {
			if err := emit(chunk.Range, body); err != nil {
				return skipped, err
			}
		} else {
			skipped++
		}
		if chunk.NextOffset == nil {
			return skipped, nil
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
			return skipped, err
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

// looksBinary applies the NUL heuristic to a file's head.
func looksBinary(head []byte) bool {
	if len(head) > binarySniffBytes {
		head = head[:binarySniffBytes]
	}
	return bytes.IndexByte(head, 0) >= 0
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
