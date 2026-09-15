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
	{
		// Failure mode: the orphan sweep is wired with a window of its own --
		// zero, or the raw config value the grace phases already rescued --
		// and it then removes content published seconds ago whose naming
		// commit has not landed yet, or never runs because its count is
		// dropped on the floor. The resolved window and the batch reaching the
		// sweep, and its count reaching the Report an operator reads, are what
		// make the CAS half of Section 10.4 a reclaim rather than a call.
		name: "the orphan sweep is driven with the resolved grace window and its count is reported",
		run: func(t *testing.T) {
			objects := &fakeObjects{swept: 5}
			blobs := &fakeBlobs{}
			c := newTestCollector(t, Options{Blobs: blobs, Objects: objects,
				Config: RetentionConfig{GraceWindow: 6 * time.Hour, BatchLimit: 9}})
			report, err := c.grace(context.Background(), Report{})
			if err != nil {
				t.Fatalf("grace: %v", err)
			}
			if objects.grace != 6*time.Hour || objects.batch != 9 {
				t.Errorf("sweep window %v batch %d, want the configured 6h and 9", objects.grace, objects.batch)
			}
			if report.OrphanObjectsSwept != 5 {
				t.Errorf("Report.OrphanObjectsSwept %d, want the 5 the sweep reclaimed", report.OrphanObjectsSwept)
			}
			if objects.oracle == nil {
				t.Fatal("the sweep was handed no oracle; it would then read every object as unnamed")
			}
			// A collector built without a window must not hand the sweep a
			// zero one: every object on disk is then past its grace.
			// Built through New rather than the helper, which supplies a
			// window of its own and would hide the fallback.
			zero := &fakeObjects{}
			c, err = New(Options{Sessions: &fakeSessions{}, Spools: &fakeSpools{}, Snapshot: &fakeSnapshots{},
				Tools: &fakeTools{}, Blobs: blobs, Objects: zero,
				Config: RetentionConfig{DataDir: t.TempDir(), BatchLimit: 9}})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if _, err := c.grace(context.Background(), Report{}); err != nil {
				t.Fatalf("grace: %v", err)
			}
			if zero.grace != defaultGraceWindow {
				t.Errorf("sweep window %v for an unconfigured collector, want the %v fallback", zero.grace, defaultGraceWindow)
			}
		},
	},
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
	// FX-H-X1 rows
	{
		// Failure mode: the CAS orphan sweep walks all 256 buckets and every
		// object in them, and a collection pass runs after EVERY activation --
		// so an incremental refresh of one file paid for a full walk of the
		// store, under the workspace lock and the indexing mutex. It is gated
		// to once per blob-grace window, and both directions of that gate are
		// invariants: a second pass inside the window must NOT re-walk, and a
		// pass after the window must, or the cadence would be a cap and
		// orphans would never be reclaimed at all.
		name: "the orphan sweep runs once per grace window, and still runs after it",
		run: func(t *testing.T) {
			const window = time.Hour
			base := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
			now := base
			objects := &fakeObjects{swept: 3}
			dataDir := t.TempDir()
			c := newTestCollector(t, Options{Objects: objects,
				Config: RetentionConfig{DataDir: dataDir, GraceWindow: window},
				Now:    func() time.Time { return now }})

			report, err := c.grace(context.Background(), Report{})
			if err != nil {
				t.Fatalf("first pass: %v", err)
			}
			now = base.Add(window / 2)
			report, err = c.grace(context.Background(), report)
			if err != nil {
				t.Fatalf("second pass: %v", err)
			}
			if objects.calls != 1 {
				t.Fatalf("two activations inside one %v grace window walked the whole store %d times, want 1",
					window, objects.calls)
			}
			if report.OrphanObjectsSwept != 3 {
				t.Fatalf("the pass that did sweep reported %d objects, want the 3 it reclaimed",
					report.OrphanObjectsSwept)
			}
			// The skipped pass must be readable AS skipped: its count is the
			// carried 3 and a pass that walked every bucket and found nothing
			// would report the same count, so without this flag an operator
			// cannot tell "nothing to reclaim" from "not looked at yet".
			if report.OrphanSweepRan {
				t.Fatalf("a pass inside the %v window reported the orphan sweep as %q; it did not run",
					window, report.OrphanSweepPhrase())
			}
			if _, err := os.Stat(filepath.Join(dataDir, orphanSweepPath)); err != nil {
				t.Fatalf("the sweep recorded no stamp, so the gate has nothing to read next pass: %v", err)
			}

			// Past the window the sweep runs again: the cadence delays work,
			// it never drops it.
			now = base.Add(window + time.Second)
			report, err = c.grace(context.Background(), report)
			if err != nil {
				t.Fatalf("third pass: %v", err)
			}
			if objects.calls != 2 {
				t.Fatalf("a pass %v after the last sweep ran %d sweeps, want a second one: the cadence "+
					"must delay the walk, not cancel it", window+time.Second, objects.calls)
			}
			if report.OrphanObjectsSwept != 6 {
				t.Fatalf("the second sweep's %d objects did not reach the report", report.OrphanObjectsSwept)
			}
			if !report.OrphanSweepRan {
				t.Fatalf("the pass past the window walked the store but reported the orphan sweep as %q",
					report.OrphanSweepPhrase())
			}

			// A clock stepped backwards leaves a stamp in the future, which a
			// plain "now - stamp < window" test reads as "not due" for an
			// unbounded time. The gate never fails closed.
			now = base.Add(-24 * time.Hour)
			if _, err := c.grace(context.Background(), Report{}); err != nil {
				t.Fatalf("pass under a backwards clock: %v", err)
			}
			if objects.calls != 3 {
				t.Fatalf("a stamp dated in the future disabled the sweep (%d calls): the gate must sweep "+
					"on any stamp it cannot trust", objects.calls)
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

func (f *fakeBlobs) KnownBlobs(context.Context, []string) (map[string]struct{}, error) {
	return nil, f.err
}

type fakeObjects struct {
	err error
	// swept is what the orphan sweep reports, and grace/batch record the
	// window and working set it was actually given.
	swept int64
	// calls counts the sweeps, which is what a cadence row reads: the sweep
	// walks the whole store, so how OFTEN it runs is an invariant of its own.
	calls int
	grace time.Duration
	batch int
	// oracle is the func the collector handed over, so a row can prove the
	// sweep was wired to the store's own KnownBlobs rather than to nothing.
	oracle func(context.Context, []string) (map[string]struct{}, error)
}

func (f *fakeObjects) Remove(string) error { return f.err }

func (f *fakeObjects) SweepOrphans(_ context.Context, known func(context.Context, []string) (map[string]struct{}, error),
	_ time.Time, grace time.Duration, batch int) (int64, error) {
	f.oracle, f.grace, f.batch = known, grace, batch
	f.calls++
	return f.swept, f.err
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
