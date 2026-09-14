package retention

import (
	"context"
	"testing"
	"time"
)

// This is the package's ONE scenario table. Every lane adds its rows under its
// own marker and nowhere else; a per-lane test file is a finding, and so is a
// row that re-asserts Validate(), a getter or forwarding. Each row names, in
// its comment, the failure mode it protects against.
//
// L0 carries no row here: its one row is the resource-sampling invariant in
// internal/diagnostics. The table and its fakes exist so L3a and L3b add rows
// rather than a harness.

type scenario struct {
	name string
	run  func(t *testing.T)
}

func TestRetention(t *testing.T) {
	if len(scenarios) == 0 {
		t.Skip("no rows yet: L3a and L3b own this table's rows")
	}
	for _, s := range scenarios {
		t.Run(s.name, func(t *testing.T) { s.run(t) })
	}
}

var scenarios = []scenario{
	// L3a rows
	// L3b rows
}

// --- deterministic fakes for the frozen interfaces --------------------------
//
// Each records what the collector asked it to do, which is how a row proves the
// pass ran in dependency order and left a live resource alone.

type fakeSessions struct {
	expired, pruned int64
	err             error
	// calls records the order of the two calls, so a row can prove that
	// expiry precedes pruning rather than merely that both happened.
	calls     []string
	retention time.Duration
}

func (f *fakeSessions) ExpireSessions(_ context.Context, _ time.Time, _ int) (int64, error) {
	f.calls = append(f.calls, "expire")
	return f.expired, f.err
}

func (f *fakeSessions) PruneSessions(_ context.Context, _ time.Time, retention time.Duration, _ int) (int64, error) {
	f.calls = append(f.calls, "prune")
	f.retention = retention
	return f.pruned, f.err
}

type fakeSpools struct {
	swept int64
	err   error
	calls int
}

func (f *fakeSpools) Sweep(context.Context, time.Time) (int64, error) {
	f.calls++
	return f.swept, f.err
}

type fakeSnapshots struct {
	err     error
	dataDir string
}

func (f *fakeSnapshots) SweepSnapshots(dataDir string) error {
	f.dataDir = dataDir
	return f.err
}

type fakeTools struct {
	collected int
	err       error
	calls     int
}

func (f *fakeTools) GC(context.Context) (int, error) {
	f.calls++
	return f.collected, f.err
}

// newTestCollector builds a Collector over the fakes with a fixed clock and a
// temporary data directory. A row replaces the dependency it drives.
func newTestCollector(t *testing.T, opts Options) *Collector {
	t.Helper()
	if opts.Sessions == nil {
		opts.Sessions = &fakeSessions{}
	}
	if opts.Spools == nil {
		opts.Spools = &fakeSpools{}
	}
	if opts.Snapshot == nil {
		opts.Snapshot = &fakeSnapshots{}
	}
	if opts.Tools == nil {
		opts.Tools = &fakeTools{}
	}
	if opts.Config.DataDir == "" {
		opts.Config.DataDir = t.TempDir()
	}
	if opts.Config.BatchLimit == 0 {
		opts.Config.BatchLimit = 200
	}
	if opts.Config.ClosedSessionRetention == 0 {
		opts.Config.ClosedSessionRetention = 7 * 24 * time.Hour
	}
	if opts.Config.GraceWindow == 0 {
		opts.Config.GraceWindow = time.Hour
	}
	if opts.Now == nil {
		fixed := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
		opts.Now = func() time.Time { return fixed }
	}
	c, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}
