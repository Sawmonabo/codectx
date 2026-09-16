package graph

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/Sawmonabo/codectx/internal/storage/pacedvfs"
)

// The requirement: the traversal scratch's writes are paced whatever else the
// process has opened. It is the surface that spills gigabytes -- a search's
// settled set, parent edges and cost buckets are external memory -- and if its
// open path did not register the file-system shim itself, it would be paced
// only because the application happens to register before the store opens.
// A run that reaches a search on another path would then hand the kernel a
// whole spill at once, which is the stall the shim exists to remove.
//
// The test opens the scratch on its own, as a package with no application
// wiring does, and asserts the shim saw the writes.
// Mutation: drop pacedvfs.Register() from pathScratch.open and run this test
// alone (-run TestTheTraversalScratchIsPacedWithoutTheApplicationsWiring, so
// no other test in the package registers first); no window is counted.
func TestTheTraversalScratchIsPacedWithoutTheApplicationsWiring(t *testing.T) {
	ctx := context.Background()
	s, err := openPathScratch(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	before := pacedvfs.Windows()
	// More than one window of node keys, so the scratch's own pages reach the
	// file rather than sitting in its page cache.
	node := strings.Repeat("n", 512)
	for i := 0; i < 64<<10; i++ {
		if err := s.exec(ctx, `INSERT INTO settled(node, dist, depth) VALUES(?, 1, 1)`,
			node+strconv.Itoa(i)); err != nil {
			t.Fatal(err)
		}
	}
	if got := pacedvfs.Windows() - before; got == 0 {
		t.Fatalf("the shim counted no window of the scratch's writes: the traversal scratch opened "+
			"a database the shim never saw (windows %d)", got)
	}
}
