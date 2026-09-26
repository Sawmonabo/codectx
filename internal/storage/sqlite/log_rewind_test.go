//go:build linux

package sqlite_test

import (
	"os"
	"testing"

	"github.com/Sawmonabo/codectx/internal/paced"
	store "github.com/Sawmonabo/codectx/internal/storage/sqlite"
)

// TestTheLogIsRewoundAndNeverTruncated seals two generations into one store,
// each larger than the process's disk window, and watches the write-ahead log
// across the commits and checkpoints between them.
//
// Requirement: a run reuses the space it already holds and frees nothing in
// the middle of its work. The log is the largest thing a run would otherwise
// free: a writer that truncates it at every reset hands the filesystem the
// whole log's blocks, up to the log bound, several times per run, and on a
// host that discards freed blocks into a sparse image that free stalls every
// process on the machine for about a minute, minutes later, with nothing in
// the machine able to observe or wait for it. Rewinding the log in place
// costs nothing: the file keeps its high-water length and every later group
// writes over the space it already holds.
//
// Mutation that fails it: restore the writer's `journal_size_limit` of 0 in
// Open, which truncates the log at every reset.
func TestTheLogIsRewoundAndNeverTruncated(t *testing.T) {
	// A writer cache a few windows wide makes the run commit in several
	// groups, each leaving a log past the window, which is the shape the
	// measured index run had: the log was reset, and truncated, three times.
	const cacheKiB = 16 << 10
	dir := t.TempDir()
	path := dir + "/rewind.db"
	freedBefore := paced.FreedBytes()
	f := newFixtureWithOptions(t, path, store.Options{WriterCacheKiB: cacheKiB})

	var high int64
	sealRepositoryInto(t, f, 600, 40, "", func() {
		st, err := os.Stat(path + "-wal")
		if err != nil {
			return
		}
		if st.Size() < high {
			t.Fatalf("the log shrank from %d to %d bytes mid-run: it is being truncated, not rewound", high, st.Size())
		}
		high = st.Size()
	})
	if err := f.s.Flush(f.ctx); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	st, err := os.Stat(path + "-wal")
	if err != nil {
		t.Fatalf("the log is gone after the run: %v", err)
	}
	if st.Size() < high {
		t.Errorf("the log shrank from %d to %d bytes at the run's last commit and checkpoint: it is being truncated, not rewound", high, st.Size())
	}
	// Without a log larger than the window a truncation would free less than
	// one window, and the mutation above would pass with nothing proven.
	if high <= paced.Window {
		t.Fatalf("the log reached only %d bytes, which is not past the %d-byte window: the fixture proves nothing", high, paced.Window)
	}
	if freed := paced.FreedBytes() - freedBefore; freed != 0 {
		t.Errorf("the store freed %d bytes of disk during the run; a run that reuses its space frees none", freed)
	}
	t.Logf("log high-water %.1f MiB across the run's groups, never shrunk; bytes freed %d",
		float64(high)/(1<<20), paced.FreedBytes()-freedBefore)
}
