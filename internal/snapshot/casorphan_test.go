package snapshot

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestSweepOrphansKeepsEverythingButUnnamedSettledContent pins the three-way
// decision SweepOrphans makes about a published object, because two of the
// three outcomes destroy source that is still in use:
//
//   - an object no blobs row names, settled past the grace, is the orphan a
//     rolled-back or crashed capture left and is the ONLY thing removed;
//   - an object no row names YET but younger than the grace is kept, because
//     Put and Barrier publish before the commit that names the object, so a
//     young unnamed file may be a publication whose commit is in flight;
//   - an object a row names is kept whatever its age -- rows are the
//     authority, and a blob mid-grace-protocol has one.
//
// The grace clause is the load-bearing one: dropping it deletes content a
// generation is about to reference, and case two is the row that fails.
func TestSweepOrphansKeepsEverythingButUnnamedSettledContent(t *testing.T) {
	dir := t.TempDir()
	c, err := OpenCAS(filepath.Join(dir, "cas"))
	if err != nil {
		t.Fatalf("open CAS: %v", err)
	}
	ctx := context.Background()
	const grace = time.Hour
	now := time.Now()

	put := func(content string, age time.Duration) string {
		t.Helper()
		rec, err := c.Put(ctx, strings.NewReader(content))
		if err != nil {
			t.Fatalf("put %q: %v", content, err)
		}
		p, err := c.path(rec.Hash)
		if err != nil {
			t.Fatalf("path: %v", err)
		}
		stamp := now.Add(-age)
		if err := os.Chtimes(p, stamp, stamp); err != nil {
			t.Fatalf("stamp %q: %v", content, err)
		}
		return rec.Hash
	}

	orphan := put("content no commit ever named", 3*time.Hour)
	inflight := put("content published a minute ago", time.Minute)
	named := put("content a generation names", 3*time.Hour)

	known := func(_ context.Context, hashes []string) (map[string]struct{}, error) {
		out := map[string]struct{}{}
		for _, h := range hashes {
			if h == named {
				out[h] = struct{}{}
			}
		}
		return out, nil
	}

	removed, err := c.SweepOrphans(ctx, known, now, grace, 2)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if removed != 1 {
		t.Errorf("swept %d objects, want exactly the one orphan", removed)
	}
	for _, in := range []string{inflight, named} {
		if has, err := c.Has(in); err != nil || !has {
			t.Errorf("object %s was removed (err %v); only unnamed settled content may go", in[:8], err)
		}
	}
	if has, err := c.Has(orphan); err != nil || has {
		t.Errorf("orphan %s survived (present %v, err %v)", orphan[:8], has, err)
	}
	// <cas>/tmp belongs to snapshot.Sweep; the bucket filter must not reach it.
	if _, err := os.Stat(c.tmp); err != nil {
		t.Errorf("the CAS temporary directory did not survive the sweep: %v", err)
	}
}

// TestSweepOrphansRemovesNothingWhenTheIndexCannotAnswer pins the direction of
// an oracle failure. The sweep's whole authority is "no row names this file",
// so an unreadable or partial answer read as "nothing is named" would delete
// every settled object in the store. It must remove nothing and report.
func TestSweepOrphansRemovesNothingWhenTheIndexCannotAnswer(t *testing.T) {
	dir := t.TempDir()
	c, err := OpenCAS(filepath.Join(dir, "cas"))
	if err != nil {
		t.Fatalf("open CAS: %v", err)
	}
	ctx := context.Background()
	rec, err := c.Put(ctx, strings.NewReader("content the index cannot be asked about"))
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	p, err := c.path(rec.Hash)
	if err != nil {
		t.Fatalf("path: %v", err)
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(p, old, old); err != nil {
		t.Fatalf("stamp: %v", err)
	}
	unreadable := errors.New("the index could not be read")
	removed, err := c.SweepOrphans(ctx, func(context.Context, []string) (map[string]struct{}, error) {
		return nil, unreadable
	}, time.Now(), time.Hour, 16)
	if !errors.Is(err, unreadable) {
		t.Fatalf("sweep error %v, want the oracle's failure reported", err)
	}
	if removed != 0 {
		t.Errorf("swept %d objects on an unreadable index, want none", removed)
	}
	if has, err := c.Has(rec.Hash); err != nil || !has {
		t.Errorf("settled content was removed without an answer from the index (present %v, err %v)", has, err)
	}
}
