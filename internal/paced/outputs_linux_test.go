//go:build linux

package paced

import (
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// writeOutput writes size bytes of a foreign writer's output, as a child
// process would: no sync, nothing handed to the disk by the writer itself.
func writeOutput(t *testing.T, path string, size int64) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	chunk := make([]byte, 1<<20)
	if _, err := rand.Read(chunk); err != nil {
		t.Fatal(err)
	}
	for written := int64(0); written < size; {
		n := min(int64(len(chunk)), size-written)
		if _, err := f.Write(chunk[:n]); err != nil {
			t.Fatal(err)
		}
		written += n
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// The requirement: nothing a foreign writer leaves in its output directory can
// block the pacer. The entries are the writer's vocabulary, not this process's,
// and opening a named pipe blocks until a writer arrives -- so an open-first
// pacer wedges its poll goroutine on the first one, and Stop, which the runner
// defers after every child exits and which joins that goroutine, hangs the run
// with nothing to time it out.
//
// Mutation: open the path before settling its kind (os.Open, then f.Stat and
// the IsRegular check) and this fails on its deadline; Stop is called from a
// goroutine so the failure is a report rather than a hung package.
func TestTheOutputPacerIsNotBlockedByANamedPipeAmongTheOutputs(t *testing.T) {
	dir := t.TempDir()
	if err := unix.Mkfifo(filepath.Join(dir, "pipe"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A regular output beside the pipe: the pacer must still do its work, not
	// merely survive.
	writeOutput(t, filepath.Join(dir, "stream"), 4*Window)

	before := OutputWindows()
	o := StartOutputs([]string{dir})
	// Long enough for the poll goroutine to walk the directory on its own,
	// which is where the open-first pacer wedges -- not only in Stop's final
	// sweep.
	time.Sleep(4 * outputPoll)
	stopped := make(chan struct{})
	go func() { defer close(stopped); o.Stop() }()
	select {
	case <-stopped:
	case <-time.After(30 * time.Second):
		t.Fatal("the pacer never stopped: a named pipe among the named outputs blocked it, and the run " +
			"that deferred Stop would have hung with no timeout")
	}
	if got := OutputWindows() - before; got == 0 {
		t.Fatal("the pacer handed no window to the disk: it skipped the regular output beside the pipe")
	}
}

// The requirement: Stop's stated contract -- the caller's outputs are on the
// disk when it returns -- holds on a platform with no range writeback, where
// the file's own sync is the only call that can honour it. A range write that
// reports false must therefore not advance the offset the pacer believes it
// has handed over, or the final sweep finds nothing left to owe and syncs
// nothing, and a child's whole output stays in the page cache for the kernel's
// flusher to submit in one burst -- the stall the pacer exists to remove.
//
// Mutation: advance the offset on the failure (o.sent[path] = end before the
// break) and the final sweep returns at end == sent: nothing is synced and the
// writer's whole output is still dirty when Stop returns.
func TestStopHandsTheOutputsOverWhereThePlatformHasNoRangeWriteback(t *testing.T) {
	dir := t.TempDir()
	var fs unix.Statfs_t
	if err := unix.Statfs(dir, &fs); err != nil {
		t.Fatal(err)
	}
	if fs.Type == tmpfsMagic {
		t.Skip("the temporary directory is memory-backed; nothing is ever written back from it")
	}
	// The platform this runs on has range writeback; the contract under test
	// is the one where it does not, so the fallback is forced.
	restore := writeRange
	writeRange = func(int, int64, int64) bool { return false }
	defer func() { writeRange = restore }()

	const size = 24 << 20
	path := filepath.Join(dir, "stream")
	writeOutput(t, path, size)

	o := StartOutputs([]string{dir})
	// Several polls hand over nothing: without range writeback there is no
	// window to submit, and a sync on every poll would be a policy nobody
	// asked for.
	time.Sleep(4 * outputPoll)

	o.Stop()

	// What is still dirty when Stop returns is what a truncate cancels: the
	// kernel drops those pages instead of writing them, and counts the bytes.
	cancelledBefore := procIO(t, "cancelled_write_bytes")
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(0); err != nil {
		t.Fatal(err)
	}
	f.Close()
	cancelled := procIO(t, "cancelled_write_bytes") - cancelledBefore
	t.Logf("still dirty when the pacer stopped %d MiB of %d", cancelled>>20, size>>20)
	if cancelled > size/4 {
		t.Fatalf("%d MiB of a %d MiB output were still in the page cache when the pacer stopped; "+
			"Stop returned before the writer's bytes reached the disk", cancelled>>20, size>>20)
	}
	if o.sent[path] != size {
		t.Fatalf("the pacer believes it handed over %d bytes of a %d byte output", o.sent[path], size)
	}
}
