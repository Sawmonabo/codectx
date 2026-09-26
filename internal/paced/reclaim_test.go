package paced

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Every test here constructs its own reclaimer, so nothing another test
// registered, queued or left stuck is in what it reads.

// wakeUp is the wake a new removal gives the worker, which is when a stuck
// entry is retried.
func wakeUp(r *reclaimer) {
	r.mu.Lock()
	r.wakeLocked()
	r.mu.Unlock()
}

// gateSleep replaces r's wait with one that records what it was asked to wait
// for and then blocks until the test lets it through, so the test observes the
// reclaimer stopped at a window boundary rather than racing it. It releases
// the reclaimer when the test ends.
func gateSleep(t *testing.T, r *reclaimer) (waits func() []time.Duration, release func()) {
	t.Helper()
	var mu sync.Mutex
	var seen []time.Duration
	gate := make(chan struct{})
	var once sync.Once
	open := func() { once.Do(func() { close(gate) }) }
	r.sleep = func(d time.Duration) {
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
		drainWithin(r, 10*time.Second)
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
// a minute. So a removal renames its file into the to-free set and returns with the space
// still occupied, and the one reclaimer releases one window per interval.
//
// Mutation: free at the call site instead of queueing (make remove call
// freeInPlace unconditionally) and the caller returns having freed the lot.
func TestARemovalReturnsWithNothingFreedAndTheReclaimerPacesTheFreeing(t *testing.T) {
	r := newReclaimer()
	useTurnDir(r, t.TempDir())
	served := t.TempDir()
	set := filepath.Join(served, "to-free")
	r.register(served, func() (string, error) { return set, os.MkdirAll(set, 0o700) })
	waits, release := gateSleep(t, r)

	const windows = 4
	path := filepath.Join(served, "big")
	writeFile(t, path, windows*Window)
	beforeSteps, freed := steps.Load(), FreedByPurpose()[AnalyzerOutput]

	if err := r.remove(AnalyzerOutput, path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("the removed path is still named: %v", err)
	}
	// The reclaimer cannot be past its first wait, so at most one window of
	// the file can have gone: anything less pending is the caller having
	// freed it on its own path.
	pending, err := r.pendingFreeBytes()
	if err != nil {
		t.Fatal(err)
	}
	if want := int64(windows-1) * Window; pending < want {
		t.Fatalf("the removal returned with only %d bytes still to free of %d; a removal renames and returns, it does not free",
			pending, int64(windows)*Window)
	}

	release()
	r.drain()
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
	// Each wait is the remainder of an interval since the host was last
	// handed a window, which is the whole interval when nothing has taken a
	// turn since and never more.
	for i, d := range got {
		if d <= 0 || d > FreeInterval {
			t.Fatalf("wait %d was %v; want a wait of at most one %v interval", i, d, FreeInterval)
		}
	}
	if pending, err := r.pendingFreeBytes(); err != nil || pending != 0 {
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
	r := newReclaimer()
	useTurnDir(r, t.TempDir())
	dir := t.TempDir()
	published := filepath.Join(dir, "blob")
	const size = 2 * Window
	writeFile(t, published, size)
	staged := filepath.Join(dir, "staged")
	if err := os.Link(published, staged); err != nil {
		t.Fatal(err)
	}

	if err := r.remove(Materialization, staged); err != nil {
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

// drainWithin waits up to d for r to run out of freeable work and reports
// whether it did. A reclaimer that retries one entry forever never returns,
// so every wait in this package is bounded and ends in a failure line rather
// than in the package's timeout.
func drainWithin(r *reclaimer, d time.Duration) ([]Stuck, bool) {
	done := make(chan []Stuck, 1)
	go func() { done <- r.drain() }()
	select {
	case stuck := <-done:
		return stuck, true
	case <-time.After(d):
		return nil, false
	}
}

// mustDrain is drainWithin with the failure line.
func mustDrain(t *testing.T, r *reclaimer, d time.Duration) []Stuck {
	t.Helper()
	stuck, ok := drainWithin(r, d)
	if !ok {
		pending, _ := r.pendingFreeBytes()
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
// immediately. A refused unlink also costs no wait: the refused child here is
// one the process may not truncate, so without the check before the charge
// its whole length would be waited for at every wake it is retried, holding
// everything queued behind it for as long.
//
// Mutations: drop the tried check from nextEntries, so the first entry is
// returned again and again, and the file queued behind it is never reached;
// drop the unlinkRefused check from freeFile, and the refused child's two
// windows are waited for before its unlink fails.
func TestAQueuedEntryThatCannotBeFreedDoesNotStopTheOnesBehindIt(t *testing.T) {
	r := newReclaimer()
	useTurnDir(r, t.TempDir())
	if os.Geteuid() == 0 {
		t.Skip("a process with the override capability unlinks from a directory it may not write, " +
			"so the refusal this test is built on never happens and it would pass vacuously")
	}
	served := t.TempDir()
	set := filepath.Join(served, "to-free")
	r.register(served, func() (string, error) { return set, os.MkdirAll(set, 0o700) })
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
	waits, release := gateSleep(t, r)

	// A tree holding one child that cannot be unlinked, because unlinking
	// needs write on the directory holding it, and cannot be truncated,
	// because it is read-only. The refusal is inside the tree rather than on
	// it: a removal renames the tree aside first, and a rename into another
	// parent needs write on what it moves.
	tree := filepath.Join(served, "refused")
	inner := filepath.Join(tree, "inner")
	if err := os.MkdirAll(inner, 0o700); err != nil {
		t.Fatal(err)
	}
	const refusedSize = 2 * Window
	child := filepath.Join(inner, "child")
	writeFile(t, child, refusedSize)
	if err := os.Chmod(child, 0o400); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(inner, 0o500); err != nil {
		t.Fatal(err)
	}
	// The purposes are read in name order, so the refused tree is the entry
	// the reclaimer reaches first and the file is the one queued behind it.
	if err := r.remove(AnalyzerOutput, tree); err != nil {
		t.Fatal(err)
	}
	const windows = 4
	behind := filepath.Join(served, "behind")
	writeFile(t, behind, windows*Window)
	freedBefore := FreedByPurpose()[Materialization]
	if err := r.remove(Materialization, behind); err != nil {
		t.Fatal(err)
	}

	release()
	stuck := mustDrain(t, r, 10*time.Second)

	if got := FreedByPurpose()[Materialization] - freedBefore; got != windows*Window {
		t.Fatalf("the file queued behind the refused tree gave back %d bytes; want %d", got, int64(windows*Window))
	}
	if got := len(waits()); got != windows {
		t.Fatalf("freeing the file behind the refused tree waited %d times for its %d windows: "+
			"an unlink the directory refuses was charged to the pace", got, windows)
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
	pending, err := r.pendingFreeBytes()
	if err != nil {
		t.Fatal(err)
	}
	if pending != refusedSize {
		t.Fatalf("%d bytes await freeing; want the refused child's %d and nothing else", pending, int64(refusedSize))
	}

	// The next wake retries it: the reason it could not be freed has gone, so
	// the entry goes.
	if err := os.Chmod(filepath.Join(refused, "inner"), 0o700); err != nil {
		t.Fatal(err)
	}
	wakeUp(r)
	if left := mustDrain(t, r, 10*time.Second); len(left) != 0 {
		t.Fatalf("the entry was still refused after what stopped it was undone: %v", left)
	}
	if pending, err := r.pendingFreeBytes(); err != nil || pending != 0 {
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
	r := newReclaimer()
	useTurnDir(r, t.TempDir())
	if os.Geteuid() == 0 {
		t.Skip("a process with the override capability opens a read-only file for writing, " +
			"so the file this test is built on is truncated a window at a time and it would pass vacuously")
	}
	served := t.TempDir()
	set := filepath.Join(served, "to-free")
	r.register(served, func() (string, error) { return set, os.MkdirAll(set, 0o700) })
	waits, release := gateSleep(t, r)

	const windows = 4
	path := filepath.Join(served, "published")
	writeFile(t, path, windows*Window)
	if err := os.Chmod(path, 0o400); err != nil {
		t.Fatal(err)
	}
	freedBefore := FreedByPurpose()[Materialization]
	if err := r.remove(Materialization, path); err != nil {
		t.Fatal(err)
	}

	if !waitFor(10*time.Second, func() bool { return len(waits()) > 0 }) {
		t.Fatal("the reclaimer never reached a wait for a file it cannot truncate")
	}
	pending, err := r.pendingFreeBytes()
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
	mustDrain(t, r, 10*time.Second)
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("the file survived its removal: %v", err)
	}
	if got := len(waits()); got != windows {
		t.Fatalf("the file's %d windows were given back over %d waits; want one wait per window", windows, got)
	}
	if got := FreedByPurpose()[Materialization] - freedBefore; got != int64(windows)*Window {
		t.Fatalf("attributed %d bytes to the purpose; want %d", got, int64(windows)*Window)
	}
	if pending, err := r.pendingFreeBytes(); err != nil || pending != 0 {
		t.Fatalf("after draining, %d bytes await freeing (%v); want none", pending, err)
	}
}

// The requirement: an entry the reclaimer cannot even STAT is stuck, not gone.
//
// `freeQueued` frees an entry, and on a failure asks the filesystem whether the
// entry is still there: one that has gone is not stuck, because freeing a
// file shrinks it before unlinking it and a shrink that failed under an
// unlink that succeeded holds no byte. But "gone" is exactly one answer --
// the entry does not exist -- and every other answer is the filesystem
// refusing to say. Reading a refusal as "gone" passes the entry straight back
// to the queue, which returns it again at once: `Drain` never returns, the
// operator is told nothing is stuck, one core spins for the life of the
// process, and every removal queued behind it is never reached.
//
// The refusal here is a purpose directory that may be read but not searched,
// which is what a tree the reclaimer can list and cannot descend looks like;
// the same shape reaches it from a device error or a stale network handle,
// neither of which a test can construct. The entry is one a dead run left
// queued, in place before the set is first used, so the refusal is there
// before the reclaimer can reach the entry.
//
// Mutation: treat every stat error as gone (`if _, gone := os.Lstat(q.path);
// gone != nil`) and this test fails on the wait -- Drain does not return and
// the entry behind is never freed.
func TestAnEntryTheReclaimerCannotStatIsRecordedStuckRatherThanRetriedForever(t *testing.T) {
	r := newReclaimer()
	useTurnDir(r, t.TempDir())
	if os.Geteuid() == 0 {
		t.Skip("a process with the override capability searches a directory that grants nobody search, " +
			"so the refusal this test is built on never happens and it would pass vacuously")
	}
	served := t.TempDir()
	set := filepath.Join(served, "to-free")
	r.register(served, func() (string, error) { return set, os.MkdirAll(set, 0o700) })
	// Registered before anything under the set is made unsearchable, so the
	// temporary directory's own removal -- which runs after this -- is not the
	// thing that fails.
	t.Cleanup(func() {
		_ = filepath.WalkDir(served, func(path string, d os.DirEntry, err error) error {
			if err == nil && d.IsDir() {
				_ = os.Chmod(path, 0o700)
			}
			return nil
		})
	})
	refusedDir := filepath.Join(set, string(AnalyzerOutput))
	if err := os.MkdirAll(refusedDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(refusedDir, "1.1"), 4096)
	// Read but not searched: the reclaimer lists the entry and cannot stat it.
	if err := os.Chmod(refusedDir, 0o400); err != nil {
		t.Fatal(err)
	}
	// Queued behind it, under the purpose that reads second, and smaller than
	// a window so the freeing takes no turn and the test waits on nothing.
	const behindSize = 4096
	behind := filepath.Join(served, "behind")
	writeFile(t, behind, behindSize)
	freedBefore := FreedByPurpose()[Materialization]
	if err := r.remove(Materialization, behind); err != nil {
		t.Fatal(err)
	}

	stuck := mustDrain(t, r, 10*time.Second)

	if got := FreedByPurpose()[Materialization] - freedBefore; got != behindSize {
		t.Fatalf("the file queued behind the unstattable entry gave back %d bytes; want %d", got, int64(behindSize))
	}
	if len(stuck) != 1 {
		t.Fatalf("the reclaimer reported %v as stuck; want exactly the entry it could not stat", stuck)
	}
	if !strings.HasPrefix(stuck[0].Entry, string(AnalyzerOutput)+string(filepath.Separator)) {
		t.Fatalf("the stuck entry is named %q; want it named under %s", stuck[0].Entry, AnalyzerOutput)
	}
	if !strings.Contains(stuck[0].Reason, "permission denied") {
		t.Fatalf("the stuck entry's reason is %q; want the refusal the filesystem gave", stuck[0].Reason)
	}

	// The next wake retries it: what stopped it has gone, so the entry goes.
	if err := os.Chmod(refusedDir, 0o700); err != nil {
		t.Fatal(err)
	}
	wakeUp(r)
	if left := mustDrain(t, r, 10*time.Second); len(left) != 0 {
		t.Fatalf("the entry was still refused after what stopped it was undone: %v", left)
	}
	if pending, err := r.pendingFreeBytes(); err != nil || pending != 0 {
		t.Fatalf("after the retry, %d bytes await freeing (%v); want none", pending, err)
	}
}
