package snapshot

import (
	"context"
	"strings"
	"testing"
)

// TestADuplicateInsideOneWindowIsStagedOnce proves that content a capture
// meets twice before the window flushes -- a vendored copy, a repeated
// licence -- costs only the hash pass that discovers it is a duplicate: one
// staged object, one sync and one publication, with both puts answered by the
// same record.
//
// mutation: dropping the pending check in Batch.put stages the second copy
// as an object of its own, so the batch holds two staged objects and its
// flush syncs and publishes the same content twice.
func TestADuplicateInsideOneWindowIsStagedOnce(t *testing.T) {
	ctx := context.Background()
	cas, err := OpenCAS(t.TempDir())
	if err != nil {
		t.Fatalf("OpenCAS: %v", err)
	}
	b := cas.NewBatch()
	defer b.Discard()
	if b.window < 2 {
		t.Skipf("a sync window of %d flushes after every put; there is no window to share", b.window)
	}
	var syncs []string
	b.onSync = func(hash string) { syncs = append(syncs, hash) }
	body := "package a\n\n// Licensed under the same terms as every copy.\n"
	first, err := b.Put(ctx, strings.NewReader(body))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	second, err := b.Put(ctx, strings.NewReader(body))
	if err != nil {
		t.Fatalf("Put(duplicate): %v", err)
	}
	if second.Hash != first.Hash || second.Size != first.Size {
		t.Fatalf("the duplicate put answered %+v, want the first put's record %+v", second, first)
	}
	if len(b.open) != 1 {
		t.Fatalf("the batch holds %d staged objects for one content, want 1", len(b.open))
	}
	if err := b.Barrier(ctx); err != nil {
		t.Fatalf("Barrier: %v", err)
	}
	if len(syncs) != 1 || syncs[0] != first.Hash {
		t.Fatalf("the barrier synced and published %v, want %s exactly once", syncs, first.Hash)
	}
	rc, err := cas.Open(ctx, first)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	rc.Close()
}
