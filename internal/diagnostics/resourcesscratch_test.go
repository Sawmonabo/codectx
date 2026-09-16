package diagnostics

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/Sawmonabo/codectx/internal/paced"
	"github.com/Sawmonabo/codectx/internal/scratch"
)

// TestTheResourcesBlockDisclosesEveryPoolAndWhatWasFreedFor is what makes the
// reuse design checkable from outside the process.
//
// A run that reuses its working files instead of freeing them reports a small
// freed_bytes. On its own that reads as a run that did little work. The disk
// those files hold has to be disclosed beside it, or the figure that matters
// is invisible: scratch_bytes is the space the store is keeping precisely so
// it never has to free it.
//
// It has to be the sum over EVERY pool. A store's surfaces are pooled under
// more than one directory -- the data directory, the continuation store's, a
// provider's work directory -- so a block that reported one of them would
// understate what the run is holding, which is the one direction this figure
// must never err in.
//
// And freed_by_purpose is what keeps the small freed_bytes honest: it says the
// freeing that did happen was an analyzer's output or a copied source tree,
// never a working file the run will need again.
//
// Mutation: sum one arena instead of scratch.All(), or drop the per-purpose
// map, and this fails.
func TestTheResourcesBlockDisclosesEveryPoolAndWhatWasFreedFor(t *testing.T) {
	const each = 128 << 10
	var held int64
	for range 2 {
		lease, f, err := scratch.For(t.TempDir()).TakeFile(scratch.SortRun)
		if err != nil {
			t.Fatalf("take: %v", err)
		}
		if _, err := f.Write(make([]byte, each)); err != nil {
			t.Fatalf("write: %v", err)
		}
		lease.Release()
		held += each
	}

	// A foreign writer's output, which is removed rather than pooled.
	out := t.TempDir()
	if err := os.WriteFile(filepath.Join(out, "index"), make([]byte, 64<<10), 0o600); err != nil {
		t.Fatalf("write output: %v", err)
	}
	if err := paced.RemoveAllFor(paced.AnalyzerOutput, out); err != nil {
		t.Fatalf("remove output: %v", err)
	}

	res, err := newTestService(t, Options{}).Resources(context.Background())
	if err != nil {
		t.Fatalf("Resources: %v", err)
	}
	if res.ScratchBytes == nil {
		t.Fatal("the resources block reports no scratch_bytes; the disk the run is holding instead of freeing is undisclosed")
	}
	if got := int64(*res.ScratchBytes); got < held {
		t.Fatalf("scratch_bytes is %d with %d bytes pooled under two directories: the block is summing one pool and understating what the run holds", got, held)
	}
	n, ok := res.FreedByPurpose[string(paced.AnalyzerOutput)]
	if !ok {
		t.Fatalf("freed_by_purpose does not account %s; a small freed_bytes with no breakdown cannot be told from a run that froze", paced.AnalyzerOutput)
	}
	if n < 64<<10 {
		t.Fatalf("freed_by_purpose accounts %d bytes of %s, want at least the %d removed", n, paced.AnalyzerOutput, 64<<10)
	}
}
