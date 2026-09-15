package retention

import (
	"context"
	"os"
	"path/filepath"
	"strings"
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
	{
		// Failure mode: pruning runs before expiry, or with a retention window
		// this pass invented rather than the configured one, and the FK cascade
		// from a closed session takes its audit evidence with it -- a capsule
		// that cited that session is then unreproducible. The configured window
		// arriving at PruneSessions is obligation 15's actual proof: the
		// `storage.closed_session_retention` key had no reader at all.
		name: "sweeps run in dependency order and pruning uses the configured retention",
		run: func(t *testing.T) {
			sessions := &fakeSessions{expired: 3, pruned: 2}
			spools := &fakeSpools{swept: 4096}
			snapshots := &fakeSnapshots{}
			tools := &fakeTools{collected: 1}
			dataDir := t.TempDir()
			c := newTestCollector(t, Options{
				Sessions: sessions, Spools: spools, Snapshot: snapshots, Tools: tools,
				Config: RetentionConfig{DataDir: dataDir, ClosedSessionRetention: 48 * time.Hour, BatchLimit: 7},
			})
			if _, err := c.sweep(context.Background()); err != nil {
				t.Fatalf("sweep: %v", err)
			}
			if got := strings.Join(sessions.calls, ","); got != "expire,prune" {
				t.Errorf("session calls %q, want expiry before pruning", got)
			}
			if sessions.retention != 48*time.Hour {
				t.Errorf("PruneSessions retention %v, want the configured 48h", sessions.retention)
			}
			if spools.calls != 1 || tools.calls != 1 || snapshots.dataDir != dataDir {
				t.Errorf("spool sweeps %d, tool collections %d, snapshot data dir %q: every landed helper must get exactly one caller",
					spools.calls, tools.calls, snapshots.dataDir)
			}
		},
	},
	{
		// Failure mode: retention deletes the payload a running language server
		// is executing. Four inputs to the one invariant: a pinned digest and a
		// staging copy a live start may still be filling both survive; a server
		// the pinned set does not name (an overridden or platform-unsupported
		// one, which (*Resolver).pinned refuses yet which still runs) is left
		// alone; and an oracle that names nothing -- absent or empty -- removes
		// nothing rather than reading "nothing is pinned" as "delete
		// everything". Only a superseded digest under a pinned name goes.
		name: "a work-directory sweep never removes a payload that may be live",
		run: func(t *testing.T) {
			const pinnedDigest, staleDigest = "aaaa", "bbbb"
			base := time.Now()
			build := func(t *testing.T) (dataDir, pinnedDir, staleDir, ghostDir, configOld, configNew string) {
				t.Helper()
				dataDir = t.TempDir()
				pinnedDir = filepath.Join(dataDir, "lsp", "jdtls", pinnedDigest)
				staleDir = filepath.Join(dataDir, "lsp", "jdtls", staleDigest)
				// A server the pinned set does not name: an overridden or
				// platform-unsupported one is exactly this, and still live.
				ghostDir = filepath.Join(dataDir, "lsp", "overridden")
				configOld, configNew = filepath.Join(pinnedDir, "config-old"), filepath.Join(pinnedDir, "config-new")
				for _, d := range []string{staleDir, ghostDir, configOld, configNew} {
					if err := os.MkdirAll(d, 0o700); err != nil {
						t.Fatalf("fixture: %v", err)
					}
				}
				if err := os.Chtimes(configOld, base.Add(-time.Hour), base.Add(-time.Hour)); err != nil {
					t.Fatalf("fixture: %v", err)
				}
				return dataDir, pinnedDir, staleDir, ghostDir, configOld, configNew
			}
			exists := func(path string) bool { _, err := os.Lstat(path); return err == nil }

			dataDir, pinnedDir, staleDir, ghostDir, configOld, configNew := build(t)
			pins := &fakeToolPins{pinned: map[string]string{"jdtls": pinnedDigest}}
			c := newTestCollector(t, Options{Tools: pins, Config: RetentionConfig{DataDir: dataDir},
				Now: func() time.Time { return base }})
			if _, err := c.sweep(context.Background()); err != nil {
				t.Fatalf("sweep: %v", err)
			}
			if !exists(pinnedDir) || !exists(configNew) || !exists(ghostDir) {
				t.Errorf("removed something that may be live: pinned digest=%v, staging a live start may hold=%v, a server the pinned set does not name=%v",
					exists(pinnedDir), exists(configNew), exists(ghostDir))
			}
			if exists(staleDir) || exists(configOld) {
				t.Errorf("unreclaimed: superseded digest=%v abandoned staging=%v", exists(staleDir), exists(configOld))
			}

			// Both shapes of "the pinned set is unknown": a collector that
			// implements no oracle at all, and one whose oracle came back
			// empty. Neither may be read as "nothing is pinned".
			for _, tools := range []ToolCollector{&fakeTools{}, &fakeToolPins{}} {
				dataDir, _, staleDir, ghostDir, _, _ = build(t)
				c = newTestCollector(t, Options{Tools: tools, Config: RetentionConfig{DataDir: dataDir},
					Now: func() time.Time { return base }})
				if _, err := c.sweep(context.Background()); err != nil {
					t.Fatalf("sweep without a pin oracle: %v", err)
				}
				if !exists(staleDir) || !exists(ghostDir) {
					t.Errorf("%T named no pinned payload yet reclaimed superseded=%v unnamed=%v; it must reclaim nothing",
						tools, !exists(staleDir), !exists(ghostDir))
				}
			}
		},
	},
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

// fakeBlobs and fakeObjects stand in for the two halves of the grace protocol:
// the store phases and the CAS. They advance no state of their own: the
// protocol's invariants live in SQL and are proved against the real store in
// storage/sqlite/store_test.go, so a collector-level assertion on a fake would
// only restate that Collect called a method. They exist so a sweep row can
// drive a whole pass without a database behind it.

type fakeBlobs struct {
	quarantined, trashed, restored int64
	deleted                        []string
	err                            error
}

func (f *fakeBlobs) QuarantineBlobs(context.Context, time.Time, int) (int64, error) {
	return f.quarantined, f.err
}

func (f *fakeBlobs) TrashBlobs(context.Context, int) (int64, int64, error) {
	return f.trashed, f.restored, f.err
}

func (f *fakeBlobs) CollectBlobs(context.Context, time.Time, int) ([]string, int64, error) {
	return f.deleted, 0, f.err
}

type fakeObjects struct {
	err error
}

func (f *fakeObjects) Remove(string) error { return f.err }

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
	if opts.Blobs == nil {
		opts.Blobs = &fakeBlobs{}
	}
	if opts.Objects == nil {
		opts.Objects = &fakeObjects{}
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

// fakeToolPins is fakeTools plus the ToolPins oracle sweep.go asserts for; L3b
// declares it here because its row must drive both shapes of the tool
// dependency, one that names the pinned payloads and one that cannot.
// L3b.
type fakeToolPins struct {
	fakeTools
	pinned map[string]string
}

func (f *fakeToolPins) PinnedFingerprints() map[string]string { return f.pinned }
