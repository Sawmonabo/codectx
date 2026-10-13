package snapshot

import (
	"bytes"
	"context"
	"io"
	"math/rand/v2"
	"os"
	"strings"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/source"
)

// framedBlobs are contents whose block structure exercises every frame shape:
// no block, a short block, exact block multiples, a one-byte last block, and
// blocks that do and do not shrink under the compressor, mixed in one blob.
func framedBlobs() map[string][]byte {
	rng := rand.New(rand.NewPCG(1, 2))
	noise := func(n int) []byte {
		b := make([]byte, n)
		for i := range b {
			b[i] = byte(rng.Uint32())
		}
		return b
	}
	code := []byte(strings.Repeat("func handler(w http.ResponseWriter, r *http.Request) {\n\treturn\n}\n", 4000))
	mixed := append(append(append([]byte{}, code[:source.BlockBytes]...), noise(source.BlockBytes)...), code[:1000]...)
	return map[string][]byte{
		"empty":            {},
		"one byte":         {'x'},
		"exact block":      code[:source.BlockBytes],
		"block plus one":   code[:source.BlockBytes+1],
		"compressible":     code,
		"incompressible":   noise(3*source.BlockBytes + 17),
		"mixed raw frames": mixed,
	}
}

// TestFramedObjectsRoundTripExactly proves the stored form is invisible: every
// byte a full stream or any range read returns is the byte that was put, at
// every block boundary and for frames stored compressed and stored as
// plaintext alike.
//
// mutation: a frame offset that skips or repeats one byte (an off-by-one in
// the prefix sum), the raw bit ignored on read, or a range read that decodes
// from the first frame instead of the first covering one -- each returns bytes
// that differ from the input.
func TestFramedObjectsRoundTripExactly(t *testing.T) {
	ctx := context.Background()
	cas, err := OpenCAS(t.TempDir())
	if err != nil {
		t.Fatalf("OpenCAS: %v", err)
	}
	for name, data := range framedBlobs() {
		rec, err := cas.Put(ctx, bytes.NewReader(data))
		if err != nil {
			t.Fatalf("%s: Put: %v", name, err)
		}
		rc, err := cas.Open(ctx, rec)
		if err != nil {
			t.Fatalf("%s: Open: %v", name, err)
		}
		got, err := io.ReadAll(rc)
		rc.Close()
		if err != nil || !bytes.Equal(got, data) {
			t.Fatalf("%s: full stream returned %d bytes (%v), want the %d put", name, len(got), err, len(data))
		}
		size := uint64(len(data))
		ranges := []model.ByteRange{{Start: 0, End: size}, {Start: size, End: size}}
		for _, edge := range []uint64{source.BlockBytes - 1, source.BlockBytes, source.BlockBytes + 1, 2 * source.BlockBytes} {
			if edge < size {
				ranges = append(ranges, model.ByteRange{Start: edge, End: min(edge+70000, size)}, model.ByteRange{Start: edge - min(edge, 5), End: edge + 1})
			}
		}
		for _, r := range ranges {
			got, err := cas.ReadRange(ctx, rec, r)
			if err != nil || !bytes.Equal(got, data[r.Start:r.End]) {
				t.Fatalf("%s: ReadRange[%d,%d) = %d bytes (%v), want the stored ones", name, r.Start, r.End, len(got), err)
			}
		}
	}
}

// TestFramedObjectIsSmallerAndNeverLarger proves the store compresses what
// shrinks and stores the rest as plaintext: compressible source takes a
// fraction of its size, and content that does not shrink costs exactly its
// plaintext plus the frame index, never a frame larger than its block.
//
// mutation: storing every block as plaintext fails the first check; keeping a
// compressed frame that is larger than its block fails the second.
func TestFramedObjectIsSmallerAndNeverLarger(t *testing.T) {
	ctx := context.Background()
	cas, err := OpenCAS(t.TempDir())
	if err != nil {
		t.Fatalf("OpenCAS: %v", err)
	}
	blobs := framedBlobs()
	objectSize := func(data []byte) int64 {
		t.Helper()
		rec, err := cas.Put(ctx, bytes.NewReader(data))
		if err != nil {
			t.Fatalf("Put: %v", err)
		}
		p, _ := cas.path(rec.Hash)
		info, err := os.Stat(p)
		if err != nil {
			t.Fatalf("stat: %v", err)
		}
		return info.Size()
	}
	code := blobs["compressible"]
	if got := objectSize(code); got*4 > int64(len(code)) {
		t.Fatalf("compressible source of %d bytes is stored in %d, want under a quarter of it", len(code), got)
	}
	noise := blobs["incompressible"]
	want := int64(len(noise) + blockCount(int64(len(noise)))*frameWordBytes + len(frameMark))
	if got := objectSize(noise); got != want {
		t.Fatalf("incompressible content of %d bytes is stored in %d, want exactly %d (plaintext frames and the index)", len(noise), got, want)
	}
}

// TestDamagedFrameFailsOnlyItsOwnBlock proves frames are independent: a range
// read decodes only the frames covering it, so damage inside the first frame
// leaves a later block readable and verified, while every read that touches
// the damaged block, and a full stream, fails as a source integrity error.
//
// mutation: a range read that decodes the object from its first byte, or one
// shared stream across blocks, makes the third block's read fail too; a decode
// error not mapped to an integrity failure fails the code checks.
func TestDamagedFrameFailsOnlyItsOwnBlock(t *testing.T) {
	ctx := context.Background()
	cas, err := OpenCAS(t.TempDir())
	if err != nil {
		t.Fatalf("OpenCAS: %v", err)
	}
	data := framedBlobs()["compressible"]
	rec, err := cas.Put(ctx, bytes.NewReader(data))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	p, _ := cas.path(rec.Hash)
	if err := os.Chmod(p, 0o600); err != nil {
		t.Fatal(err)
	}
	fh, err := os.OpenFile(p, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	var b [1]byte
	if _, err := fh.ReadAt(b[:], 12); err != nil {
		t.Fatal(err)
	}
	if _, err := fh.WriteAt([]byte{^b[0]}, 12); err != nil {
		t.Fatal(err)
	}
	fh.Close()

	third := model.ByteRange{Start: 2 * source.BlockBytes, End: 2*source.BlockBytes + 100}
	if got, err := cas.ReadRange(ctx, rec, third); err != nil || !bytes.Equal(got, data[third.Start:third.End]) {
		t.Fatalf("ReadRange in an intact block = %d bytes, %v; want it served without the damaged frame", len(got), err)
	}
	_, err = cas.ReadRange(ctx, rec, model.ByteRange{Start: 0, End: 10})
	wantCode(t, err, model.CodeSourceIntegrity)
	rc, err := cas.Open(ctx, rec)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	_, err = io.ReadAll(rc)
	rc.Close()
	wantCode(t, err, model.CodeSourceIntegrity)
}

// TestTruncatedObjectIsRefused proves the frame index makes an object
// self-checking: an object that lost its last byte is refused by a read
// before any byte is served, and by a later put of the same content, which
// must not accept it as already published.
//
// mutation: parseFrames without the mark check or without the check that the
// frames end where the index begins accepts the cut object; checkExisting
// reduced to a regular-file check accepts it at publication.
func TestTruncatedObjectIsRefused(t *testing.T) {
	ctx := context.Background()
	cas, err := OpenCAS(t.TempDir())
	if err != nil {
		t.Fatalf("OpenCAS: %v", err)
	}
	data := framedBlobs()["mixed raw frames"]
	rec, err := cas.Put(ctx, bytes.NewReader(data))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	p, _ := cas.path(rec.Hash)
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(p, info.Size()-1); err != nil {
		t.Fatal(err)
	}
	_, err = cas.ReadRange(ctx, rec, model.ByteRange{Start: 0, End: 10})
	wantCode(t, err, model.CodeSourceIntegrity)
	_, err = cas.Put(ctx, bytes.NewReader(data))
	wantCode(t, err, model.CodeSourceIntegrity)
}
