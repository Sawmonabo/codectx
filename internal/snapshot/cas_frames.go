package snapshot

import (
	"encoding/binary"
	"errors"
	"io"
	"os"
	"sync"

	"github.com/klauspost/compress/zstd"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/source"
)

// A published object is the blob's plaintext blocks, each stored as one
// independently decodable frame, followed by a frame index and a four-byte
// trailer mark:
//
//	frame 0 | frame 1 | ... | frame n-1 | word 0 | ... | word n-1 | "cxfi"
//
// n is the blob's block count, which every reader already knows from the
// record's plaintext size, so the index needs no count of its own. Word i is a
// little-endian uint32: its low 31 bits are frame i's stored length and its top
// bit marks a frame stored as plaintext, which is what a block that does not
// shrink under the compressor becomes. Frames are contiguous from byte zero, so
// frame i starts at the sum of the lengths before it.
//
// Everything a reader verifies is still over plaintext: each decoded frame
// must be exactly its block's length and match its block digest, and a full
// stream must match the whole-file digest. A damaged frame therefore fails at
// its own block, and a range read decodes only the frames that cover it.
//
// The index lives in the object rather than in the database so the object is
// self-describing: whether an object already published under a hash is sound
// is answered from the object alone, and nothing about it depends on the
// compressor producing the same bytes from one build to the next.

// frameMark ends every object. A file without it is not an object this store
// wrote, or one whose tail was lost.
var frameMark = [4]byte{'c', 'x', 'f', 'i'}

const (
	frameWordBytes = 4
	frameRawBit    = 1 << 31
)

// frameCodec is the one compressor and decompressor the process shares. Both
// are safe for concurrent use, so a capture's parallel puts and a query's
// parallel range reads share them without a lock, and neither allocates per
// call once warm. The encoder runs at the compressor's default level: it is
// paid once per new content, while every read pays the same decode cost at
// any level. Frame checksums are off because every decoded block is checked
// against its SHA-256 digest anyway. The decoder never produces more than the
// destination's capacity, which is one block, so a damaged frame cannot make
// it allocate past that.
var frameCodec = sync.OnceValues(func() (*zstd.Encoder, *zstd.Decoder) {
	enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault), zstd.WithEncoderCRC(false),
		zstd.WithLowerEncoderMem(true))
	if err != nil {
		panic("content store compressor: " + err.Error())
	}
	dec, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(0), zstd.WithDecodeAllCapLimit(true),
		zstd.WithDecoderMaxMemory(model.BlobBlockBytes))
	if err != nil {
		panic("content store decompressor: " + err.Error())
	}
	return enc, dec
})

// blockCount is the number of blocks of a blob of size bytes.
func blockCount(size int64) int {
	return int((size + source.BlockBytes - 1) / source.BlockBytes)
}

// encodeFrames writes the object layout of the plaintext src holds in
// [0, size) into dst from byte zero and returns the object's length. Bytes of
// dst past that length are the caller's to trim. Memory is one block and one
// frame.
func encodeFrames(src, dst *os.File, size int64) (int64, error) {
	enc, _ := frameCodec()
	n := blockCount(size)
	words := make([]byte, n*frameWordBytes+len(frameMark))
	var plain [source.BlockBytes]byte
	frame := make([]byte, 0, source.BlockBytes)
	var at int64
	for i := range n {
		want := blockLen(size, i)
		if _, err := src.ReadAt(plain[:want], int64(i)*source.BlockBytes); err != nil {
			return 0, err
		}
		frame = enc.EncodeAll(plain[:want], frame[:0])
		word := uint32(len(frame))
		out := frame
		if len(frame) >= want {
			word, out = uint32(want)|frameRawBit, plain[:want]
		}
		if _, err := dst.WriteAt(out, at); err != nil {
			return 0, err
		}
		binary.LittleEndian.PutUint32(words[i*frameWordBytes:], word)
		at += int64(len(out))
	}
	copy(words[n*frameWordBytes:], frameMark[:])
	if _, err := dst.WriteAt(words, at); err != nil {
		return 0, err
	}
	return at + int64(len(words)), nil
}

// frames is a parsed frame index: frame i occupies [off[i], off[i+1]) of the
// object and raw[i] says it is stored as plaintext.
type frames struct {
	off []int64
	raw []bool
}

// readFrames parses the index of the object f holds for a blob of size
// plaintext bytes. The object must be exactly its frames, its index and its
// mark: a shorter file, a missing mark, or frames that do not end where the
// index begins are an integrity failure, reported before any byte is served.
func readFrames(f *os.File, size int64, hash string) (frames, error) {
	info, err := f.Stat()
	if err != nil {
		return frames{}, ioError("CAS stat", err)
	}
	return parseFrames(f, info.Size(), size, hash)
}

func parseFrames(r io.ReaderAt, objSize, size int64, hash string) (frames, error) {
	n := blockCount(size)
	indexLen := int64(n*frameWordBytes + len(frameMark))
	if objSize < indexLen {
		return frames{}, integrity("blob object is shorter than its frame index").WithDetail("content_hash", hash)
	}
	words := make([]byte, indexLen)
	if _, err := r.ReadAt(words, objSize-indexLen); err != nil {
		if errors.Is(err, io.EOF) {
			return frames{}, integrity("blob object is shorter than its frame index").WithDetail("content_hash", hash)
		}
		return frames{}, ioError("CAS read", err)
	}
	if [4]byte(words[n*frameWordBytes:]) != frameMark {
		return frames{}, integrity("blob object does not end in a frame index").WithDetail("content_hash", hash)
	}
	fr := frames{off: make([]int64, n+1), raw: make([]bool, n)}
	for i := range n {
		word := binary.LittleEndian.Uint32(words[i*frameWordBytes:])
		length := int64(word &^ frameRawBit)
		fr.raw[i] = word&frameRawBit != 0
		if length == 0 || length > source.BlockBytes || (fr.raw[i] && length != int64(blockLen(size, i))) {
			return frames{}, integrity("blob frame %d has an impossible stored length", i).WithDetail("content_hash", hash)
		}
		fr.off[i+1] = fr.off[i] + length
	}
	if fr.off[n] != objSize-indexLen {
		return frames{}, integrity("blob object's frames do not end where its frame index begins").WithDetail("content_hash", hash)
	}
	return fr, nil
}

// decodeFrame decodes frame i, held in stored, into dst, whose capacity must
// be exactly the block's length, and returns the block. A frame that does not
// decode, or decodes to any other length, is an integrity failure at block i.
func (fr frames) decodeFrame(stored, dst []byte, i int, hash string) ([]byte, error) {
	want := cap(dst)
	if fr.raw[i] {
		return append(dst[:0], stored...), nil
	}
	_, dec := frameCodec()
	out, err := dec.DecodeAll(stored, dst[:0])
	if err != nil || len(out) != want {
		return nil, integrity("blob block %d does not decode to its recorded length", i).WithDetail("content_hash", hash)
	}
	return out, nil
}
