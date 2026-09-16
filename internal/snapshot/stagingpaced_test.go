package snapshot

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

// The requirement: a capture's staging database is paced whatever else the
// process has opened. It holds one row per eligible path of the repository and
// is written before any store need be open, so if its open path did not
// register the file-system shim itself it would be paced only because the
// application happens to register first -- and a capture reached on another
// path would hand the kernel a repository-sized file at once.
//
// The test opens the staging on its own in a process where nothing else has
// opened a database -- see alone -- and asserts the shim saw the writes.
// Mutation: drop pacedvfs.Register() from openStaging; no window is counted,
// and the whole package fails, not only this test under -run.
func TestTheCaptureStagingIsPacedWithoutTheApplicationsWiring(t *testing.T) {
	if !alone(t, "TestTheCaptureStagingIsPacedWithoutTheApplicationsWiring") {
		return
	}
	ctx := context.Background()
	s, err := openStaging(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	before := pacedvfs.Windows()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	// More than one window of paths, so the staging's own pages reach the file
	// rather than sitting in its page cache.
	dir := strings.Repeat("d/", 128)
	for i := 0; i < 64<<10; i++ {
		if _, err := tx.ExecContext(ctx, `INSERT INTO entries(path) VALUES(?)`, dir+strconv.Itoa(i)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if got := pacedvfs.Windows() - before; got == 0 {
		t.Fatalf("the shim counted no window of the staging's writes: the capture opened a database "+
			"the shim never saw (windows %d)", got)
	}
}
