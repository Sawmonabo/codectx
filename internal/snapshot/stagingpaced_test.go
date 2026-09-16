package snapshot

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/Sawmonabo/codectx/internal/storage/pacedvfs"
)

// The requirement: a capture's staging database is paced whatever else the
// process has opened. It holds one row per eligible path of the repository and
// is written before any store need be open, so if its open path did not
// register the file-system shim itself it would be paced only because the
// application happens to register first -- and a capture reached on another
// path would hand the kernel a repository-sized file at once.
//
// The test opens the staging on its own, as a package with no application
// wiring does, and asserts the shim saw the writes.
// Mutation: drop pacedvfs.Register() from openStaging and run this test alone
// (-run TestTheCaptureStagingIsPacedWithoutTheApplicationsWiring, so no other
// test in the package registers first); no window is counted.
func TestTheCaptureStagingIsPacedWithoutTheApplicationsWiring(t *testing.T) {
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
