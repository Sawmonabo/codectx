package source

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"sort"
	"unicode/utf8"
)

const (
	// BlockBytes is the fixed CAS block size of Section 10.3. Range serving
	// verifies the blocks it exposes against these digests, so the size is part
	// of the stored metadata contract and is not configurable.
	BlockBytes = 65536
	// CheckpointBytes is the minimum distance between sparse line checkpoints.
	// Serving seeks to the nearest checkpoint at or before a range and scans
	// forward from there, so this trades a bounded scan against a bounded
	// number of stored checkpoints.
	CheckpointBytes = 65536
)

// Checkpoint records that a line begins at a byte offset. Checkpoints are
// sparse: they exist so line and column information for a served range can be
// derived without scanning from byte zero.
type Checkpoint struct {
	Byte uint64 `json:"byte"`
	Line uint32 `json:"line"`
}

// Index is the integrity and position metadata for one file, all of it computed
// in a single streaming pass at CAS insertion time. Section 10.3 forbids
// rehashing a file or scanning from byte zero for every chunk, which is exactly
// what these two structures avoid.
type Index struct {
	Size        uint64       `json:"size"`
	ContentHash string       `json:"content_hash"`
	Blocks      []string     `json:"blocks"`
	Checkpoints []Checkpoint `json:"checkpoints"`
	// Lines is the number of lines that hold bytes: the number of line breaks
	// plus one for a final line without a newline. An empty file has none, and
	// a trailing newline does not open a line of its own. It is not the highest
	// valid line number, because the EOF position after a trailing newline is
	// itself a valid position.
	Lines uint32 `json:"lines"`
	// ValidUTF8 records whether the whole file decodes as UTF-8. A file that
	// does not is served losslessly as base64 rather than coerced.
	ValidUTF8 bool `json:"valid_utf8"`
}

// BuildIndex streams r once, computing the whole-file digest, the fixed-size
// block digests, the sparse line checkpoints and the UTF-8 validity of the
// content together. It retains only the metadata, never the file body.
func BuildIndex(r io.Reader) (Index, error) {
	idx := Index{ValidUTF8: true, Checkpoints: []Checkpoint{{Byte: 0, Line: 1}}}
	whole := sha256.New()
	block := sha256.New()

	buf := make([]byte, BlockBytes)
	var (
		blockFill      int
		nextCheckpoint = uint64(CheckpointBytes)
		// line is the one-based number of the line currently being read.
		line = uint32(1)
		// lastByte is the final byte seen, which decides whether the file ends
		// with a line break.
		lastByte byte
		// carry holds the bytes of a UTF-8 sequence split across two reads.
		carry  [utf8.UTFMax]byte
		nCarry int
	)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			whole.Write(chunk)

			// Line accounting and checkpoints, both derived from the same scan.
			base := idx.Size
			for i := 0; i < len(chunk); {
				next := bytes.IndexByte(chunk[i:], '\n')
				if next < 0 {
					break
				}
				i += next + 1
				line++
				lineStart := base + uint64(i)
				if lineStart >= nextCheckpoint {
					idx.Checkpoints = append(idx.Checkpoints, Checkpoint{Byte: lineStart, Line: line})
					nextCheckpoint = lineStart + CheckpointBytes
				}
			}
			lastByte = chunk[len(chunk)-1]
			if idx.ValidUTF8 {
				nCarry = scanUTF8(&idx, carry[:], nCarry, chunk)
			}

			// Fixed-size blocks, independent of how the reader chunked the
			// stream: a block digest must depend only on the file's bytes.
			for off := 0; off < len(chunk); {
				take := min(BlockBytes-blockFill, len(chunk)-off)
				block.Write(chunk[off : off+take])
				blockFill += take
				off += take
				if blockFill == BlockBytes {
					idx.Blocks = append(idx.Blocks, hex.EncodeToString(block.Sum(nil)))
					block.Reset()
					blockFill = 0
				}
			}
			idx.Size += uint64(n)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return Index{}, err
		}
	}
	if blockFill > 0 {
		idx.Blocks = append(idx.Blocks, hex.EncodeToString(block.Sum(nil)))
	}
	if nCarry > 0 {
		// The file ends in the middle of a UTF-8 sequence.
		idx.ValidUTF8 = false
	}
	if idx.Size > 0 {
		// line is the number of the line after the last break, which holds
		// bytes only when the file does not end with one.
		idx.Lines = line
		if lastByte == '\n' {
			idx.Lines--
		}
	}
	idx.ContentHash = hex.EncodeToString(whole.Sum(nil))
	return idx, nil
}

// scanUTF8 validates chunk as a continuation of the stream, carrying a sequence
// split across reads. It returns the number of carried bytes and clears
// idx.ValidUTF8 on the first invalid sequence.
func scanUTF8(idx *Index, carry []byte, nCarry int, chunk []byte) int {
	for _, b := range chunk {
		if nCarry == 0 {
			if b < utf8.RuneSelf {
				continue
			}
			if !utf8.RuneStart(b) {
				idx.ValidUTF8 = false
				return 0
			}
			carry[0] = b
			nCarry = 1
			continue
		}
		if utf8.RuneStart(b) {
			idx.ValidUTF8 = false
			return 0
		}
		carry[nCarry] = b
		nCarry++
		if utf8.FullRune(carry[:nCarry]) {
			if r, size := utf8.DecodeRune(carry[:nCarry]); r == utf8.RuneError && size <= 1 {
				idx.ValidUTF8 = false
				return 0
			}
			nCarry = 0
			continue
		}
		if nCarry == utf8.UTFMax {
			idx.ValidUTF8 = false
			return 0
		}
	}
	return nCarry
}

// CheckpointFor returns the last checkpoint at or before offset: the nearest
// point a range read can start scanning from.
func (i Index) CheckpointFor(offset uint64) Checkpoint {
	if len(i.Checkpoints) == 0 {
		return Checkpoint{Byte: 0, Line: 1}
	}
	n := sort.Search(len(i.Checkpoints), func(k int) bool { return i.Checkpoints[k].Byte > offset })
	if n == 0 {
		return i.Checkpoints[0]
	}
	return i.Checkpoints[n-1]
}

// BlockRange returns the half-open index range of the blocks covering the byte
// range [start,end). Range serving verifies exactly these blocks; it does not
// claim that any block it did not read was revalidated.
func (i Index) BlockRange(start, end uint64) (int, int) {
	if end <= start {
		return 0, 0
	}
	first := int(start / BlockBytes)
	last := int((end - 1) / BlockBytes)
	if last >= len(i.Blocks) {
		last = len(i.Blocks) - 1
	}
	if first > last {
		return 0, 0
	}
	return first, last + 1
}
