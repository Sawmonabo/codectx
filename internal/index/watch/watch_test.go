package watch_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Sawmonabo/codectx/internal/index/watch"
	"github.com/Sawmonabo/codectx/internal/workspace"
)

// TestWatchOverflowCollapsesToFullReconciliation protects the Section 13.2
// overflow contract against the one failure that is silent: a watch window
// that exceeds index.watch_pending_paths emits the paths it happened to
// retain instead of collapsing to a single full-reconciliation flag. The
// coordinator refreshes exactly the paths a batch names, so a truncated list
// is an assertion that every path it omits is unchanged -- the index would
// then publish coverage as fresh over files it never re-read.
//
// It is the one test in this package, and it runs against the real fsnotify
// backend rather than a fake: the bound is only meaningful against the event
// stream the operating system actually delivers.
func TestWatchOverflowCollapsesToFullReconciliation(t *testing.T) {
	dir := t.TempDir()
	// A seed file so the fixture is a populated repository rather than a bare
	// directory; the watch set is every directory the policy admits, so the
	// root is watched either way.
	if err := os.WriteFile(filepath.Join(dir, "seed.go"), []byte("package p\n"), 0o600); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	root, err := workspace.Discover(dir)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	defer root.Close()

	const maxPaths = 4
	w, err := watch.New(watch.Options{
		Root:   root,
		Policy: workspace.Policy{MaxFiles: 1000, DataDir: filepath.Join(t.TempDir(), "data")},
		// A short debounce keeps the test quick; the reconcile interval is
		// pushed out of the way so the only batch under test is the one the
		// overflow produces.
		Debounce:  50 * time.Millisecond,
		Reconcile: time.Hour,
		MaxPaths:  maxPaths,
		MaxBytes:  1 << 20,
	})
	if err != nil {
		t.Fatalf("new watcher: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	batches := make(chan watch.Batch, 8)
	done := make(chan error, 1)
	go func() {
		done <- w.Run(ctx, func(b watch.Batch) error {
			batches <- b
			return nil
		})
	}()

	// The watch is placed by Run; write until the overflow batch arrives so the
	// test does not race the first Add.
	//
	// A non-overflow batch is a legal intermediate result and must not fail the
	// test. arm() fixes the debounce window from the first event rather than
	// sliding it, so whenever the burst is slower than the window -- which is
	// what CPU contention under a full-suite run does -- the window closes with
	// fewer than MaxPaths paths and the watcher correctly emits what it holds.
	// The invariant under test is that a burst past the bound *does* collapse,
	// not that the first batch is the one that collapses.
	var got watch.Batch
	deadline := time.After(20 * time.Second)
	writes := 0
	for !got.Overflow {
		for n := 0; n < 4*maxPaths; n++ {
			name := filepath.Join(dir, "f"+string(rune('a'+writes%26))+string(rune('a'+n%26))+".go")
			if err := os.WriteFile(name, []byte("package p\n"), 0o600); err != nil {
				t.Fatalf("write %d: %v", n, err)
			}
			writes++
		}
		select {
		case got = <-batches:
		case <-time.After(250 * time.Millisecond):
		case <-deadline:
			t.Fatalf("%d writes past a bound of %d never collapsed to a full reconciliation",
				writes, maxPaths)
		}
	}

	if len(got.Paths) != 0 {
		t.Errorf("an overflow batch carries %d paths; a collapsed batch makes no claim about individual paths and must carry none",
			len(got.Paths))
	}
	if got.Reason == "" {
		t.Errorf("an overflow batch carries no reason; status cannot report why coverage collapsed")
	}

	complete, pending, reconciled := w.Coverage()
	if !complete {
		t.Errorf("coverage is incomplete after a clean watch placement over a small tree")
	}
	if pending != 0 {
		t.Errorf("pending = %d after the batch was emitted, want 0; a released window must not keep charging its bound", pending)
	}
	if reconciled.IsZero() {
		t.Errorf("last reconciliation is zero after the watch set was derived")
	}

	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Errorf("Run returned nil on cancellation, want the typed cancellation")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
}
