package paced

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// gateSleep replaces the reclaimer's wait with one that records what it was
// asked to wait for and then blocks until the test lets it through, so the
// test observes the reclaimer stopped at a window boundary rather than racing
// it. It restores the real wait and releases the reclaimer when the test ends.
func gateSleep(t *testing.T) (waits func() []time.Duration, release func()) {
	t.Helper()
	Drain()
	var mu sync.Mutex
	var seen []time.Duration
	gate := make(chan struct{})
	var once sync.Once
	open := func() { once.Do(func() { close(gate) }) }
	restore := reclaim.sleep
	reclaim.sleep = func(d time.Duration) {
		mu.Lock()
		seen = append(seen, d)
		mu.Unlock()
		<-gate
	}
	t.Cleanup(func() {
		open()
		Drain()
		reclaim.sleep = restore
	})
	return func() []time.Duration {
		mu.Lock()
		defer mu.Unlock()
		return append([]time.Duration(nil), seen...)
	}, open
}

// The requirement: a run never waits for the disk to take space back, and the
// space goes back at the measured pace rather than in a burst. A burst of
// frees leaves a host that discards freed blocks into a sparse image owing
// work it never reports, and every disk request in flight a minute later waits
// a minute -- which is what an index run of a repository did three times. So a
// removal renames its file into the to-free set and returns with the space
// still occupied, and the one reclaimer releases one window per interval.
//
// Mutation: free at the call site instead of queueing (make RemoveFor call
// freeInPlace unconditionally) and the caller returns having freed the lot.
func TestARemovalReturnsWithNothingFreedAndTheReclaimerPacesTheFreeing(t *testing.T) {
	served := t.TempDir()
	set := filepath.Join(served, "to-free")
	RegisterToFree(served, func() (string, error) { return set, os.MkdirAll(set, 0o700) })
	waits, release := gateSleep(t)

	const windows = 4
	path := filepath.Join(served, "big")
	writeFile(t, path, windows*Window)
	beforeSteps, freed := steps.Load(), FreedByPurpose()[AnalyzerOutput]

	if err := RemoveFor(AnalyzerOutput, path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("the removed path is still named: %v", err)
	}
	// The reclaimer cannot be past its first wait, so at most one window of
	// the file can have gone: anything less pending is the caller having
	// freed it on its own path.
	pending, err := PendingFreeBytes()
	if err != nil {
		t.Fatal(err)
	}
	if want := int64(windows-1) * Window; pending < want {
		t.Fatalf("the removal returned with only %d bytes still to free of %d; a removal renames and returns, it does not free",
			pending, int64(windows)*Window)
	}

	release()
	Drain()
	if got := steps.Load() - beforeSteps; got != windows {
		t.Fatalf("the reclaimer freed %d windows; want %d", got, windows)
	}
	if got := FreedByPurpose()[AnalyzerOutput] - freed; got != int64(windows)*Window {
		t.Fatalf("attributed %d bytes to the purpose; want %d", got, int64(windows)*Window)
	}
	got := waits()
	if len(got) != windows {
		t.Fatalf("the reclaimer waited %d times for %d windows; want one wait per window", len(got), windows)
	}
	for i, d := range got {
		if d != FreeInterval {
			t.Fatalf("wait %d was %v; want %v", i, d, FreeInterval)
		}
	}
	if pending, err := PendingFreeBytes(); err != nil || pending != 0 {
		t.Fatalf("after draining, %d bytes await freeing (%v); want none", pending, err)
	}
}

// The requirement: emptying a file before unlinking it must never empty an
// object another name still reaches. A content-addressed store publishes a
// blob by linking its staging file to the object's final name, so from the
// link until the staging name is removed the two are one object; shrinking the
// staging name there would leave the published blob on the disk, named by its
// content hash, holding none of it. A reader would serve the wrong bytes for a
// hash that says what they must be.
//
// Mutation: make shrinkable always report true and the published object is
// emptied by the removal of the name beside it.
func TestARemovalNeverEmptiesAnObjectAnotherNameStillReaches(t *testing.T) {
	dir := t.TempDir()
	published := filepath.Join(dir, "blob")
	const size = 2 * Window
	writeFile(t, published, size)
	staged := filepath.Join(dir, "staged")
	if err := os.Link(published, staged); err != nil {
		t.Fatal(err)
	}

	if err := RemoveFor(Materialization, staged); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(staged); !os.IsNotExist(err) {
		t.Fatalf("the removed name survived: %v", err)
	}
	st, err := os.Stat(published)
	if err != nil {
		t.Fatal(err)
	}
	if st.Size() != size {
		t.Fatalf("the published object is %d bytes after the name beside it was removed; want %d", st.Size(), int64(size))
	}
}
