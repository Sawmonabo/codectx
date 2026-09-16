package paced

import (
	"os"
	"path/filepath"
	"strings"
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
		// Bounded: a reclaimer that will not run out of work is the failure
		// the test that used this gate reports, and holding the package open
		// behind it would replace that failure line with a timeout dump.
		drainWithin(10 * time.Second)
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

// drainWithin waits up to d for the reclaimer to run out of freeable work and
// reports whether it did. A reclaimer that retries one entry forever never
// returns, so every wait in this package is bounded and ends in a failure
// line rather than in the package's timeout.
func drainWithin(d time.Duration) ([]Stuck, bool) {
	done := make(chan []Stuck, 1)
	go func() { done <- Drain() }()
	select {
	case stuck := <-done:
		return stuck, true
	case <-time.After(d):
		return nil, false
	}
}

// mustDrain is drainWithin with the failure line.
func mustDrain(t *testing.T, d time.Duration) []Stuck {
	t.Helper()
	stuck, ok := drainWithin(d)
	if !ok {
		pending, _ := PendingFreeBytes()
		t.Fatalf("the reclaimer did not run out of freeable work in %v and %d bytes still await freeing: "+
			"an entry it cannot free is stopping the ones queued behind it", d, pending)
	}
	return stuck
}

// The requirement: one queued removal the filesystem refuses must not stop
// every other removal in the process, and must not spend a core retrying
// itself. A child a walk cannot unlink -- a device error, a read-only mount, a
// directory a failed child left without write permission -- is reachable on
// any tree the product removes, and the reclaimer is the one place in the
// process that gives space back: an entry it will not pass over holds every
// other caller's disk for the life of the run, keeps `pending_free_bytes`
// climbing with no explanation, and never lets an operator's request to give
// the space back return.
//
// So the failure is recorded with its reason, the entries behind it are freed
// at the pace, and the stuck one is retried at the next wake rather than
// immediately.
//
// Mutation: drop the stuck check from nextEntry, so the first entry is
// returned again and again, and the file queued behind it is never reached.
func TestAQueuedEntryThatCannotBeFreedDoesNotStopTheOnesBehindIt(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("a process with the override capability unlinks from a directory it may not write, " +
			"so the refusal this test is built on never happens and it would pass vacuously")
	}
	served := t.TempDir()
	set := filepath.Join(served, "to-free")
	RegisterToFree(served, func() (string, error) { return set, os.MkdirAll(set, 0o700) })
	// Registered before the reclaimer is gated, and so before anything under
	// the set can be left unwritable: the temporary directory's own removal
	// runs after this and would fail on it.
	t.Cleanup(func() {
		_ = filepath.WalkDir(served, func(path string, d os.DirEntry, err error) error {
			if err == nil && d.IsDir() {
				_ = os.Chmod(path, 0o700)
			}
			return nil
		})
	})
	waits, release := gateSleep(t)

	// A tree holding one child that cannot be unlinked, because unlinking
	// needs write on the directory holding it. The refusal is inside the
	// tree rather than on it: a removal renames the tree aside first, and a
	// rename into another parent needs write on what it moves.
	tree := filepath.Join(served, "refused")
	inner := filepath.Join(tree, "inner")
	if err := os.MkdirAll(inner, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(inner, "child"), 4096)
	if err := os.Chmod(inner, 0o500); err != nil {
		t.Fatal(err)
	}
	// The purposes are read in name order, so the refused tree is the entry
	// the reclaimer reaches first and the file is the one queued behind it.
	if err := RemoveAllFor(AnalyzerOutput, tree); err != nil {
		t.Fatal(err)
	}
	const windows = 4
	behind := filepath.Join(served, "behind")
	writeFile(t, behind, windows*Window)
	freedBefore := FreedByPurpose()[Materialization]
	if err := RemoveFor(Materialization, behind); err != nil {
		t.Fatal(err)
	}

	release()
	stuck := mustDrain(t, 10*time.Second)

	if got := FreedByPurpose()[Materialization] - freedBefore; got != windows*Window {
		t.Fatalf("the file queued behind the refused tree gave back %d bytes; want %d", got, int64(windows*Window))
	}
	if got := len(waits()); got < windows {
		t.Fatalf("freeing the file behind the refused tree waited %d times for %d windows", got, windows)
	}
	if len(stuck) != 1 {
		t.Fatalf("the reclaimer reported %v as stuck; want exactly the refused tree", stuck)
	}
	if !strings.HasPrefix(stuck[0].Entry, string(AnalyzerOutput)+string(filepath.Separator)) {
		t.Fatalf("the stuck entry is named %q; want it named under %s", stuck[0].Entry, AnalyzerOutput)
	}
	if stuck[0].Reason == "" {
		t.Fatal("the stuck entry carries no reason, so what is holding the space is not disclosed")
	}
	refused := filepath.Join(set, stuck[0].Entry)
	if _, err := os.Stat(refused); err != nil {
		t.Fatalf("the stuck entry is not where it was reported: %v", err)
	}
	pending, err := PendingFreeBytes()
	if err != nil {
		t.Fatal(err)
	}
	if pending != 4096 {
		t.Fatalf("%d bytes await freeing; want the refused child's 4096 and nothing else", pending)
	}

	// The next wake retries it: the reason it could not be freed has gone, so
	// the entry goes.
	if err := os.Chmod(filepath.Join(refused, "inner"), 0o700); err != nil {
		t.Fatal(err)
	}
	reclaim.wake()
	if left := mustDrain(t, 10*time.Second); len(left) != 0 {
		t.Fatalf("the entry was still refused after what stopped it was undone: %v", left)
	}
	if pending, err := PendingFreeBytes(); err != nil || pending != 0 {
		t.Fatalf("after the retry, %d bytes await freeing (%v); want none", pending, err)
	}
}

// waitFor polls until the condition holds or the bound passes, and reports
// whether it held. It is how a test observes the reclaimer stopped at a
// window boundary without racing it.
func waitFor(d time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(time.Millisecond)
	}
	return true
}

// The requirement: a file the process may unlink but may not truncate must
// wait its own size's worth of windows BEFORE it is unlinked. Every published
// blob is such a file -- a content-addressed store makes its objects
// read-only at publication and nothing bounds a blob's size, so the largest
// file in a repository is one of them -- and the unlink of a whole one hands
// the host its entire length in a single act. On a host that discards freed
// blocks into a sparse image that is the burst which stalls every writer on
// the machine for about a minute, a minute later, with nothing inside the
// machine able to observe or wait for it. Charging the pace after the unlink
// waits for space that has already gone.
//
// Mutation: charge after the unlink (move r.charge below os.Remove in
// freeFile) and the whole file is gone before the reclaimer's first wait.
func TestAFileThatCannotBeTruncatedWaitsItsSizeBeforeItIsUnlinked(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("a process with the override capability opens a read-only file for writing, " +
			"so the file this test is built on is truncated a window at a time and it would pass vacuously")
	}
	served := t.TempDir()
	set := filepath.Join(served, "to-free")
	RegisterToFree(served, func() (string, error) { return set, os.MkdirAll(set, 0o700) })
	waits, release := gateSleep(t)

	const windows = 4
	path := filepath.Join(served, "published")
	writeFile(t, path, windows*Window)
	if err := os.Chmod(path, 0o400); err != nil {
		t.Fatal(err)
	}
	freedBefore := FreedByPurpose()[Materialization]
	if err := RemoveFor(Materialization, path); err != nil {
		t.Fatal(err)
	}

	if !waitFor(10*time.Second, func() bool { return len(waits()) > 0 }) {
		t.Fatal("the reclaimer never reached a wait for a file it cannot truncate")
	}
	pending, err := PendingFreeBytes()
	if err != nil {
		t.Fatal(err)
	}
	if want := int64(windows) * Window; pending != want {
		t.Fatalf("at the reclaimer's first wait %d bytes of %d await freeing: "+
			"a file that cannot be truncated was handed to the host before the pace waited for it", pending, want)
	}
	if got := FreedByPurpose()[Materialization] - freedBefore; got != 0 {
		t.Fatalf("%d bytes are counted as given back while the file is still whole on the disk", got)
	}

	release()
	mustDrain(t, 10*time.Second)
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("the file survived its removal: %v", err)
	}
	if got := len(waits()); got != windows {
		t.Fatalf("the file's %d windows were given back over %d waits; want one wait per window", windows, got)
	}
	if got := FreedByPurpose()[Materialization] - freedBefore; got != int64(windows)*Window {
		t.Fatalf("attributed %d bytes to the purpose; want %d", got, int64(windows)*Window)
	}
	if pending, err := PendingFreeBytes(); err != nil || pending != 0 {
		t.Fatalf("after draining, %d bytes await freeing (%v); want none", pending, err)
	}
}
