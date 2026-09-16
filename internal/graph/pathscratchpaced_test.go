package graph

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"github.com/Sawmonabo/codectx/internal/storage/pacedvfs"
)

// alone runs the caller in a child copy of the test binary with no other test
// selected, and reports whether this call is that child.
//
// Registration is a process-wide, once-only act, so "this open path registers
// the shim itself" is only observable in a process where nothing else has
// registered. Run among its package's other tests the assertion below holds
// whatever the open path does, which is no assertion at all: the child is
// what gives it its meaning.
func alone(t *testing.T, name string) bool {
	t.Helper()
	const marker = "CODECTX_PACED_REGISTRATION_CHILD"
	if os.Getenv(marker) != "" {
		return true
	}
	cmd := exec.Command(os.Args[0], "-test.run", "^"+name+"$", "-test.v")
	cmd.Env = append(os.Environ(), marker+"=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the open path did not register the file-system shim: %v\n%s", err, out)
	}
	return false
}

// The requirement: the traversal scratch's writes are paced whatever else the
// process has opened. It is the surface that spills gigabytes -- a search's
// settled set, parent edges and cost buckets are external memory -- and if its
// open path did not register the file-system shim itself, it would be paced
// only because the application happens to register before the store opens.
// A run that reaches a search on another path would then hand the kernel a
// whole spill at once, which is the stall the shim exists to remove.
//
// The test opens the scratch on its own in a process where nothing else has
// opened a database -- see alone -- and asserts the shim saw the writes.
// Mutation: drop pacedvfs.Register() from pathScratch.open; no window is
// counted, and the whole package fails, not only this test under -run.
func TestTheTraversalScratchIsPacedWithoutTheApplicationsWiring(t *testing.T) {
	if !alone(t, "TestTheTraversalScratchIsPacedWithoutTheApplicationsWiring") {
		return
	}
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
