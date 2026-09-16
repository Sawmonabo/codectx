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
