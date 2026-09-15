package bench

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/Sawmonabo/codectx/internal/config"
	"github.com/Sawmonabo/codectx/internal/index"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/process"
	"github.com/Sawmonabo/codectx/internal/provider"
	"github.com/Sawmonabo/codectx/internal/provider/filesystem"
	"github.com/Sawmonabo/codectx/internal/provider/manifest"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/wire"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/worker"
	"github.com/Sawmonabo/codectx/internal/snapshot"
	"github.com/Sawmonabo/codectx/internal/storage/sqlite"
	"github.com/Sawmonabo/codectx/internal/workspace"
)

// benchRoot is this test binary's scratch root: the shared budget fixture, the
// corpus it is generated into and the release binary built to measure it all
// live under it, and nothing else in the package writes outside it or a
// t.TempDir.
//
// It carries the process id because more than one bench test binary can be
// live at once -- `go test ./...` beside a targeted rerun of this package, a
// CI stage beside a developer's shell -- and over a fixed path whichever
// started second would clear the corpus out from under the first's remaining
// rows. That is exactly the failure a full-suite race run was seen to hit:
// rows late in the Section 23.2 table refused with CTX_PATH_ESCAPE because the
// repository they were measuring had been removed mid-run.
//
// Its lifetime is TestMain's rather than any row's: the fixture deliberately
// outlives the test or benchmark that happens to build it (see fixture), so no
// t.Cleanup may own it. It is cleared on entry -- a pid can be reused after a
// crashed run -- and released after the last row returns.
var benchRoot = filepath.Join(os.TempDir(), fmt.Sprintf("codectx-bench-l2-%d", os.Getpid()))

// TestMain makes this test binary the parser worker, as the codectx binary
// is in production (ruling R8-1), and owns benchRoot's lifetime.
func TestMain(m *testing.M) {
	// A parser worker returns here: it runs no row, so it must never create or
	// release the root its parent is measuring against.
	if len(os.Args) > 1 && os.Args[1] == wire.Subcommand {
		os.Exit(worker.Main(context.Background(), os.Stdin, os.Stdout, os.Stderr))
	}
	if err := os.RemoveAll(benchRoot); err != nil {
		fmt.Fprintf(os.Stderr, "clear the bench root %s: %v\n", benchRoot, err)
		os.Exit(1)
	}
	if err := os.MkdirAll(benchRoot, 0o700); err != nil {
		fmt.Fprintf(os.Stderr, "create the bench root %s: %v\n", benchRoot, err)
		os.Exit(1)
	}
	code := m.Run()
	// os.Exit skips deferred calls, so the release is written out here: a run
	// leaves no tree behind, and repeated runs never accumulate.
	if err := os.RemoveAll(benchRoot); err != nil {
		fmt.Fprintf(os.Stderr, "release the bench root %s: %v\n", benchRoot, err)
	}
	os.Exit(code)
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

// TestIncrementalReuse measures the Section 13.1 plateau at the other end of
// the product: a no-op refresh over an already indexed repository must reuse
// every unit, parse nothing and rewrite no lexical body, and must therefore
// cost a small fraction of the cold index.
//
// Failure mode: a coordinator that rebuilds units whose inputs did not move
// turns every refresh into a cold index -- the "secretly rerun every base
// provider over the full repository on each small edit" Section 13.1 forbids.
// It is measured here rather than only asserted in internal/index because the
// cost, not the count, is what makes the difference visible: a reuse path that
// still parses would pass a count assertion if it rebuilt nothing but read
// everything.
func TestIncrementalReuse(t *testing.T) {
	if testing.Short() {
		t.Skip("incremental reuse benchmark; run without -short")
	}
	const files = 400
	ctx := context.Background()
	dir := t.TempDir()
	repoDir, dataDir := filepath.Join(dir, "repo"), filepath.Join(dir, "data")
	source, err := os.ReadFile(filepath.Join("..", "provider", "treesitter", "testdata", "sample.go"))
	if err != nil {
		t.Fatal(err)
	}
	for i := range files {
		abs := filepath.Join(repoDir, "pkg", fmt.Sprintf("f%03d", i), "sample.go")
		if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, source, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(repoDir, "go.mod"), []byte("module example.com/bench\n\ngo 1.27\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := config.Defaults()
	cfg.Storage.DataDir = dataDir
	cfg.Providers.SCIP.Enabled = config.Disabled
	cfg.Providers.LSP.Enabled = config.Disabled
	cfg.Providers.Dependence.Enabled = config.Disabled

	cas, err := snapshot.OpenCAS(snapshot.CASDir(dataDir))
	if err != nil {
		t.Fatal(err)
	}
	store, err := sqlite.Open(ctx, filepath.Join(dataDir, "codectx.db"), sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	lock, err := snapshot.LockWorkspace(ctx, dataDir, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	root, err := workspace.Discover(repoDir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		t.Fatal(err)
	}
	runner, err := process.NewRunner(process.Limits{MaxConcurrent: 4, MemoryBudgetBytes: 4 << 30, DiskBudgetBytes: 1 << 30})
	if err != nil {
		t.Fatal(err)
	}
	parsers := filepath.Join(dataDir, "parsers")
	if err := os.MkdirAll(parsers, 0o700); err != nil {
		t.Fatal(err)
	}
	ts, err := treesitter.New(treesitter.Options{MaxWorkers: 2, MaxParseFileBytes: cfg.Workspace.MaxParseFileBytes,
		WorkerIdleTTL: time.Minute, ParseTimeout: time.Minute, WorkerMemoryBytes: 256 << 20,
		Worker: treesitter.WorkerCommand{Path: exe, Args: []string{wire.Subcommand}}, Runner: runner, WorkDir: parsers})
	if err != nil {
		t.Fatal(err)
	}
	defer ts.Close()
	fs, err := filesystem.New(filesystem.Options{MaxSearchFileBytes: cfg.Workspace.MaxSearchFileBytes})
	if err != nil {
		t.Fatal(err)
	}
	mf, err := manifest.New(manifest.Options{MaxParseFileBytes: cfg.Workspace.MaxParseFileBytes})
	if err != nil {
		t.Fatal(err)
	}
	registry, err := provider.NewRegistry(fs, mf, ts)
	if err != nil {
		t.Fatal(err)
	}
	pool, err := provider.NewPool(cfg.Index.QueueBytes)
	if err != nil {
		t.Fatal(err)
	}
	c, err := index.New(index.Options{Root: root, Config: cfg, Store: store, Registry: registry,
		CAS: cas, Lock: lock, Pool: pool})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	started := time.Now()
	cold, err := c.Index(ctx, model.IndexRequest{})
	if err != nil {
		t.Fatalf("cold index: %v", err)
	}
	coldFor := time.Since(started)
	started = time.Now()
	refreshed, err := c.Refresh(ctx, nil)
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	refreshFor := time.Since(started)
	t.Logf("cold: %d units built, %d files parsed in %s; refresh: %d reused, %d built, %d parsed in %s",
		cold.UnitsBuilt, cold.FilesParsed, coldFor, refreshed.UnitsReused, refreshed.UnitsBuilt,
		refreshed.FilesParsed, refreshFor)
	// That the refresh rebuilt and reparsed nothing is proven by
	// TestIncrementalScenario's no-op leg in internal/index, which also checks
	// unit identity and the FTS document count; repeating those counts here
	// would add nothing. The reuse count stays because the ratio below is only
	// meaningful as a statement about the same work.
	if refreshed.UnitsReused != cold.UnitsBuilt {
		t.Fatalf("the refresh reused %d of the %d units the cold index built", refreshed.UnitsReused, cold.UnitsBuilt)
	}
	// The cost, not the count, is what this test exists for. A generous margin
	// keeps it from failing on a loaded machine, where the measured ratio was
	// 13x.
	if refreshFor*4 > coldFor {
		t.Fatalf("the no-op refresh took %s against a %s cold index; reuse is not paying for itself", refreshFor, coldFor)
	}
}
