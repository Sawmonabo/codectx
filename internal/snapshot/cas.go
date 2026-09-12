package snapshot

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"hash"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/source"
)

// CAS is the local content-addressed store of Section 10.3: raw immutable blob
// files named by their validated SHA-256, published atomically and never
// replaced. Integrity metadata (block digests, line checkpoints) is not stored
// here; it is persisted through storage and handed back to Open and ReadRange,
// which verify the bytes they expose against it.
type CAS struct {
	dir string
	tmp string
}

// OpenCAS opens or creates the store at dir (0700).
func OpenCAS(dir string) (*CAS, error) {
	if !filepath.IsAbs(dir) {
		return nil, invalid("the CAS directory must be an absolute path")
	}
	tmp := filepath.Join(dir, "tmp")
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		return nil, ioError("CAS directory", err)
	}
	return &CAS{dir: dir, tmp: tmp}, nil
}

// path derives the blob location from a validated digest and nothing else.
func (c *CAS) path(hash string) (string, error) {
	if !model.ValidHexID(hash) {
		return "", integrity("content hash is not a lowercase SHA-256 hex digest")
	}
	return filepath.Join(c.dir, hash[:2], hash), nil
}

// Has reports whether the blob file for hash is present. Presence is not
// verification; Open and ReadRange are.
func (c *CAS) Has(hash string) (bool, error) {
	p, err := c.path(hash)
	if err != nil {
		return false, err
	}
	_, err = os.Lstat(p)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return false, ioError("CAS stat", err)
}

// Put streams r into a private temporary file while computing the whole-file
// digest, the 64-KiB block digests and the sparse line checkpoints in one pass
// (source.BuildIndex), flushes it, and publishes it under its digest without
// replacing content already there. The returned record is what the caller
// persists with storage.PutBlob before any manifest names it.
func (c *CAS) Put(ctx context.Context, r io.Reader) (model.BlobRecord, error) {
	return c.put(ctx, r, "")
}

// put is Put with an optional expected digest: when want is set, content whose
// digest differs is discarded unpublished and reported as an integrity
// failure. Repair uses this so a wrong reconstruction never enters the store.
func (c *CAS) put(ctx context.Context, r io.Reader, want string) (model.BlobRecord, error) {
	if err := ctx.Err(); err != nil {
		return model.BlobRecord{}, err
	}
	tmp, err := os.CreateTemp(c.tmp, "put-*")
	if err != nil {
		return model.BlobRecord{}, ioError("CAS temporary file", err)
	}
	published := false
	defer func() {
		tmp.Close()
		if !published {
			os.Remove(tmp.Name())
		}
	}()

	idx, err := source.BuildIndex(io.TeeReader(r, tmp))
	if err != nil {
		return model.BlobRecord{}, ioError("CAS write", err)
	}
	if want != "" && idx.ContentHash != want {
		return model.BlobRecord{}, integrity("reconstructed content hashes to a different digest than the manifest records")
	}
	if err := tmp.Sync(); err != nil {
		return model.BlobRecord{}, ioError("CAS sync", err)
	}
	if err := tmp.Chmod(0o400); err != nil {
		return model.BlobRecord{}, ioError("CAS chmod", err)
	}
	if err := tmp.Close(); err != nil {
		return model.BlobRecord{}, ioError("CAS close", err)
	}
	rec := recordOf(idx)
	final, err := c.path(rec.Hash)
	if err != nil {
		return model.BlobRecord{}, err
	}
	if err := os.MkdirAll(filepath.Dir(final), 0o700); err != nil {
		return model.BlobRecord{}, ioError("CAS bucket", err)
	}
	if err := c.publish(tmp.Name(), final, rec.Size); err != nil {
		return model.BlobRecord{}, err
	}
	published = true
	// The temporary name is gone after a rename and redundant after a link;
	// either way nothing else references it.
	os.Remove(tmp.Name())
	if err := syncDir(filepath.Dir(final)); err != nil {
		return model.BlobRecord{}, ioError("CAS directory sync", err)
	}
	return rec, nil
}

// publish links tmp to final atomically. An existing final is the same
// content by construction of the name, so it is kept and only its size is
// checked; a filesystem without hard links falls back to a rename that is
// attempted only while final is absent.
func (c *CAS) publish(tmp, final string, size int64) error {
	err := os.Link(tmp, final)
	if err == nil {
		return nil
	}
	if errors.Is(err, fs.ErrExist) {
		return c.checkExisting(final, size)
	}
	if _, statErr := os.Lstat(final); statErr == nil {
		return c.checkExisting(final, size)
	}
	if err := os.Rename(tmp, final); err != nil {
		return ioError("CAS publish", err)
	}
	return nil
}

func (c *CAS) checkExisting(final string, size int64) error {
	info, err := os.Lstat(final)
	if err != nil {
		return ioError("CAS stat", err)
	}
	if !info.Mode().IsRegular() || info.Size() != size {
		return integrity("a different object is already published under this content hash")
	}
	return nil
}

// recordOf converts the streaming index to the storage record. A source
// checkpoint marks where a line begins, so the line containing that byte
// starts at the byte itself.
func recordOf(idx source.Index) model.BlobRecord {
	rec := model.BlobRecord{Hash: idx.ContentHash, Size: int64(idx.Size), BlockDigests: idx.Blocks}
	for _, cp := range idx.Checkpoints {
		rec.LineCheckpoints = append(rec.LineCheckpoints, model.LineCheckpoint{ByteOffset: cp.Byte, LineNumber: cp.Line, LineStartByte: cp.Byte})
	}
	return rec
}

// indexOf is the inverse: the stored record as the source package's index,
// so block coverage and checkpoint lookup reuse one implementation.
func indexOf(rec model.BlobRecord) source.Index {
	idx := source.Index{Size: uint64(rec.Size), ContentHash: rec.Hash, Blocks: rec.BlockDigests}
	for _, cp := range rec.LineCheckpoints {
		idx.Checkpoints = append(idx.Checkpoints, source.Checkpoint{Byte: cp.LineStartByte, Line: cp.LineNumber})
	}
	return idx
}

// Open streams the blob described by rec, verifying each 64-KiB block digest
// as it passes and the whole-file digest and size at end of file. A mismatch
// is CTX_SOURCE_INTEGRITY at the offending block, before any later byte is
// exposed. Memory is one block.
func (c *CAS) Open(ctx context.Context, rec model.BlobRecord) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := rec.Validate(); err != nil {
		return nil, err
	}
	f, err := c.open(rec.Hash)
	if err != nil {
		return nil, err
	}
	return &verifyingReader{f: f, rec: rec, whole: sha256.New()}, nil
}

func (c *CAS) open(hash string) (*os.File, error) {
	p, err := c.path(hash)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(p)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, integrity("blob is missing from the content-addressed store").WithDetail("content_hash", hash)
		}
		return nil, ioError("CAS open", err)
	}
	return f, nil
}

// verifyingReader serves a blob one verified block at a time.
type verifyingReader struct {
	f      *os.File
	rec    model.BlobRecord
	whole  hash.Hash
	block  int
	buf    []byte
	pos    int
	buffer [source.BlockBytes]byte
	done   bool
}

func (r *verifyingReader) Read(p []byte) (int, error) {
	for r.pos == len(r.buf) {
		if r.done {
			return 0, io.EOF
		}
		if err := r.fill(); err != nil {
			return 0, err
		}
	}
	n := copy(p, r.buf[r.pos:])
	r.pos += n
	return n, nil
}

// fill reads and verifies the next block, or finishes the whole-file check
// when every block has been served.
func (r *verifyingReader) fill() error {
	if r.block == len(r.rec.BlockDigests) {
		// Every recorded block is verified. The file must end exactly here and
		// the aggregate digest must match; extra bytes or a different whole
		// digest mean the blocks and the object disagree.
		var probe [1]byte
		if n, err := r.f.Read(probe[:]); n > 0 || (err != nil && !errors.Is(err, io.EOF)) {
			return integrity("blob is longer than its recorded size").WithDetail("content_hash", r.rec.Hash)
		}
		if hex.EncodeToString(r.whole.Sum(nil)) != r.rec.Hash {
			return integrity("blob content does not match its content hash").WithDetail("content_hash", r.rec.Hash)
		}
		r.done = true
		r.buf, r.pos = nil, 0
		return nil
	}
	want := blockLen(r.rec.Size, r.block)
	n, err := io.ReadFull(r.f, r.buffer[:want])
	if err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
			return integrity("blob is shorter than its recorded size").WithDetail("content_hash", r.rec.Hash)
		}
		return ioError("CAS read", err)
	}
	if err := verifyBlock(r.buffer[:n], r.rec, r.block); err != nil {
		return err
	}
	r.whole.Write(r.buffer[:n])
	r.buf, r.pos = r.buffer[:n], 0
	r.block++
	return nil
}

func (r *verifyingReader) Close() error { return r.f.Close() }

// blockLen is the size of block i of a blob of size bytes.
func blockLen(size int64, i int) int {
	remaining := size - int64(i)*source.BlockBytes
	if remaining > source.BlockBytes {
		return source.BlockBytes
	}
	return int(remaining)
}

func verifyBlock(data []byte, rec model.BlobRecord, i int) error {
	sum := sha256.Sum256(data)
	want, err := hex.DecodeString(rec.BlockDigests[i])
	if err != nil || !bytes.Equal(sum[:], want) {
		return integrity("blob block %d does not match its recorded digest", i).WithDetail("content_hash", rec.Hash)
	}
	return nil
}

// ReadRange returns the bytes [r.Start, r.End) of the blob, verifying exactly
// the blocks that cover the interval against rec and nothing else: it never
// rehashes the file and makes no claim about blocks it did not read. The
// interval is bounded by model.MaxRawChunkBytes, so one call reads at most
// that plus two partial blocks.
func (c *CAS) ReadRange(ctx context.Context, rec model.BlobRecord, r model.ByteRange) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := rec.Validate(); err != nil {
		return nil, err
	}
	if err := r.Validate("byte_range"); err != nil {
		return nil, err
	}
	if r.End-r.Start > model.MaxRawChunkBytes {
		return nil, resourceLimit("byte range spans %d bytes, over the %d-byte read ceiling", r.End-r.Start, model.MaxRawChunkBytes)
	}
	idx := indexOf(rec)
	first, last, err := idx.BlockRange(r.Start, r.End)
	if err != nil {
		return nil, err
	}
	if first == last {
		// The empty interval at any valid offset, including EOF of an empty
		// file: nothing to verify and nothing to return but the offset itself.
		return []byte{}, nil
	}
	f, err := c.open(rec.Hash)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	start := int64(first) * source.BlockBytes
	end := min(int64(last)*source.BlockBytes, rec.Size)
	buf := make([]byte, end-start)
	if _, err := f.ReadAt(buf, start); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, integrity("blob is shorter than its recorded size").WithDetail("content_hash", rec.Hash)
		}
		return nil, ioError("CAS read", err)
	}
	for i := first; i < last; i++ {
		off := int64(i-first) * source.BlockBytes
		if err := verifyBlock(buf[off:off+int64(blockLen(rec.Size, i))], rec, i); err != nil {
			return nil, err
		}
	}
	return buf[r.Start-uint64(start) : r.End-uint64(start)], nil
}
