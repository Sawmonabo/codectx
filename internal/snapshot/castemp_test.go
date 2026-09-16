package snapshot

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/Sawmonabo/codectx/internal/paced"
)

// pattern is len bytes of repeating content, produced without holding them.
type pattern struct{ left int64 }

func (p *pattern) Read(b []byte) (int, error) {
	if p.left <= 0 {
		return 0, io.EOF
	}
	if int64(len(b)) > p.left {
		b = b[:p.left]
	}
	for i := range b {
		b[i] = byte('a' + (p.left+int64(i))%23)
	}
	p.left -= int64(len(b))
	return len(b), nil
}

// contentTemps is every surface of the store's blob-staging pool, with its
// current length.
func contentTemps(t *testing.T, dataDir string) map[string]int64 {
	t.Helper()
	out := map[string]int64{}
	matches, err := filepath.Glob(filepath.Join(dataDir, "scratch", "*", "content-temp", "*"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	for _, m := range matches {
		st, err := os.Stat(m)
		if err != nil {
			t.Fatalf("stat %s: %v", m, err)
		}
		out[m] = st.Size()
	}
	return out
}

// TestRecapturingPublishedContentFreesNothing is the reason the blob staging
// surface is pooled at all.
//
// A capture cannot know whether the store already holds a file's content until
// it has read the file and hashed it, so it streams EVERY file in the
// workspace into a temporary first. When the content turns out to be already
// published, that temporary used to be removed with its bytes — so re-indexing
// an unchanged repository handed the filesystem the whole repository's worth
// of deallocation, one file at a time, having produced no new object at all.
// On a host that discards freed blocks under a sparse image that stalls every
// writer on the machine for about a minute.
//
// The surface is taken from the store's pool instead: the second capture
// writes over the first one's bytes and gives the surface back at its length.
// Nothing is freed, which is what the paced step count proves — a step is one
// window of disk handed back.
func TestRecapturingPublishedContentFreesNothing(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	c, err := OpenCAS(CASDir(dataDir))
	if err != nil {
		t.Fatalf("OpenCAS: %v", err)
	}
	// Larger than the pacing window, so a removal of this surface would be
	// unmistakable in the bytes freed rather than lost in rounding.
	const size = paced.Window + 1<<20

	first, err := c.Put(ctx, &pattern{left: size})
	if err != nil {
		t.Fatalf("first capture: %v", err)
	}

	// The second capture finds the content published and stages it anyway.
	before := paced.FreedBytes()
	second, err := c.Put(ctx, &pattern{left: size})
	if err != nil {
		t.Fatalf("second capture: %v", err)
	}
	if second.Hash != first.Hash || second.Size != size {
		t.Fatalf("the second capture recorded %s/%d, want %s/%d", second.Hash[:8], second.Size, first.Hash[:8], size)
	}
	if freed := paced.FreedBytes() - before; freed != 0 {
		t.Fatalf("re-capturing published content freed %d bytes of disk; it must free none", freed)
	}

	pool := contentTemps(t, dataDir)
	if len(pool) != 1 {
		t.Fatalf("the blob staging pool holds %d surfaces after two captures, want the one that is reused: %v", len(pool), pool)
	}
	var held int64
	for _, n := range pool {
		held = n
	}
	if held < size {
		t.Fatalf("the pooled surface is %d bytes, want at least the %d it staged: it was emptied instead of kept", held, size)
	}

	// A third capture takes that same surface and leaves it no smaller: the
	// pool is reused, not grown and not shrunk.
	if _, err := c.Put(ctx, &pattern{left: size}); err != nil {
		t.Fatalf("third capture: %v", err)
	}
	after := contentTemps(t, dataDir)
	if len(after) != len(pool) {
		t.Fatalf("a third capture grew the pool to %d surfaces, want %d: each capture is taking a new one", len(after), len(pool))
	}
	for path, n := range after {
		if was, ok := pool[path]; ok && n < was {
			t.Fatalf("pooled surface %s shrank from %d to %d bytes", path, was, n)
		}
	}
}
