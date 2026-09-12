package bench

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/Sawmonabo/codectx/internal/process"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/wire"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/worker"
)

// TestMain makes this test binary the parser worker, as the codectx binary
// is in production (ruling R8-1).
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == wire.Subcommand {
		os.Exit(worker.Main(context.Background(), os.Stdin, os.Stdout, os.Stderr))
	}
	os.Exit(m.Run())
}

// Plateau parameters. The warm-up excludes the parses during which the
// worker's allocator and the parser's stack reach their steady size; the
// tolerance is what a stable native process drifts by under a glibc arena.
const (
	parses    = 600
	sampleAt  = 50
	warmUp    = 200
	tolerance = 8 << 20
)

// TestParserResourcePlateau parses the same source repeatedly through the
// real worker path and asserts that the worker's resident set stops growing:
// after warm-up, no sample exceeds the first post-warm-up sample by more than
// the tolerance. It then closes the provider and asserts that every worker
// it started has exited.
//
// Failure mode: a native object the worker did not close (a tree, a query
// cursor) grows the worker's RSS by a bounded amount per parse, which over
// hundreds of parses of one file is a slope, not a plateau; a worker the
// parent stopped tracking without stopping would still be alive after Close.
func TestParserResourcePlateau(t *testing.T) {
	if testing.Short() {
		t.Skip("resource plateau benchmark; run without -short")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		t.Fatal(err)
	}
	runner, err := process.NewRunner(process.Limits{MaxConcurrent: 2, MemoryBudgetBytes: 2 << 30, DiskBudgetBytes: 1 << 30})
	if err != nil {
		t.Fatal(err)
	}
	p, err := treesitter.New(treesitter.Options{
		MaxWorkers: 1, WorkerIdleTTL: time.Minute, ParseTimeout: time.Minute, WorkerMemoryBytes: 256 << 20,
		Worker: treesitter.WorkerCommand{Path: exe, Args: []string{wire.Subcommand}}, Runner: runner, WorkDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	// One source per language, each large enough that a per-parse leak of a
	// tree or a query cursor is visible against the allocator's noise.
	sources := map[string][]byte{}
	for _, name := range []string{"sample.go", "sample.py", "sample.ts", "sample.rs", "sample.cpp"} {
		src, err := os.ReadFile(filepath.Join("..", "provider", "treesitter", "testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		sources[name] = bytes.Repeat(src, 64)
	}
	names := []string{"sample.go", "sample.py", "sample.ts", "sample.rs", "sample.cpp"}

	ctx := context.Background()
	var baseline int64 = -1
	var pids []int
	for i := 1; i <= parses; i++ {
		name := names[i%len(names)]
		probe, err := p.ParseProbe(ctx, name, sources[name])
		if err != nil {
			t.Fatalf("parse %d (%s): %v", i, name, err)
		}
		if probe.Declarations == 0 {
			t.Fatalf("parse %d (%s) extracted nothing", i, name)
		}
		if i%sampleAt != 0 {
			continue
		}
		s := p.Stats()
		if s.WorkerRSSBytes < 0 {
			t.Skip("worker RSS is not measurable on this platform")
		}
		pids = append(pids, s.WorkerPIDs...)
		t.Logf("parse %4d: worker rss %8d KiB, parent rss %8d KiB, workers started %d exited %d",
			i, s.WorkerRSSBytes>>10, s.ParentRSSBytes>>10, s.WorkersStarted, s.WorkersExited)
		if i < warmUp {
			continue
		}
		if baseline < 0 {
			baseline = s.WorkerRSSBytes
			continue
		}
		if s.WorkerRSSBytes > baseline+tolerance {
			t.Fatalf("worker rss grew from %d to %d bytes after warm-up: no plateau", baseline, s.WorkerRSSBytes)
		}
	}
	if s := p.Stats(); s.WorkersStarted != 1 {
		t.Fatalf("workers started = %d; the plateau must be measured on one long-lived worker", s.WorkersStarted)
	}

	p.Close()
	s := p.Stats()
	if s.Processes != 0 || s.WorkersExited != s.WorkersStarted {
		t.Fatalf("after Close: %d live, %d started, %d exited", s.Processes, s.WorkersStarted, s.WorkersExited)
	}
	for _, pid := range pids {
		if pid <= 0 {
			t.Fatalf("worker reported no pid")
		}
		// The runner has reaped the child; the pid must no longer exist (or
		// belong to a process this test cannot signal, which is not ours).
		if err := syscall.Kill(pid, 0); err == nil {
			t.Fatalf("worker pid %d is still alive after Close", pid)
		} else if !errors.Is(err, syscall.ESRCH) && !errors.Is(err, syscall.EPERM) {
			t.Fatalf("probing pid %d: %v", pid, err)
		}
	}
}
