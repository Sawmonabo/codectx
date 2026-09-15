package snapshot

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// TestBatchSyncsTheBucketOfADeduplicatedBlob pins the half of the barrier
// contract the durability test does not reach: the already-published path.
//
// Section 10.3 requires that everything a published generation names is on disk
// before that generation is visible, and a bucket entry is only on disk once
// its DIRECTORY has been fsynced. A batch that meets content an earlier
// generation already published pays no byte sync -- correctly -- but it is
// still about to commit a manifest naming that entry, and the batch that first
// published it may have died before its own Barrier. Returning before
// recording the bucket left Barrier with nothing to sync for that blob.
func TestBatchSyncsTheBucketOfADeduplicatedBlob(t *testing.T) {
	ctx := context.Background()
	cas, err := OpenCAS(t.TempDir())
	if err != nil {
		t.Fatalf("OpenCAS: %v", err)
	}
	body := "package a\n\nfunc A() {}\n"
	rec, err := cas.Put(ctx, strings.NewReader(body))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	// A second, independent batch meeting content that is already published.
	b := cas.NewBatch()
	defer b.Discard()
	again, err := b.Put(ctx, strings.NewReader(body))
	if err != nil {
		t.Fatalf("Put(deduplicated): %v", err)
	}
	if again.Hash != rec.Hash {
		t.Fatalf("hash = %q, want the published %q", again.Hash, rec.Hash)
	}
	path, err := cas.path(rec.Hash)
	if err != nil {
		t.Fatalf("path: %v", err)
	}
	if _, ok := b.dirty[filepath.Dir(path)]; !ok {
		t.Fatalf("the batch recorded %d bucket directories, none of them %q: "+
			"Barrier would fsync no directory for a blob the manifest names",
			len(b.dirty), filepath.Dir(path))
	}
	if err := b.Barrier(ctx); err != nil {
		t.Fatalf("Barrier: %v", err)
	}
}
