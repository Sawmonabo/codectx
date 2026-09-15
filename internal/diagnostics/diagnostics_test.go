package diagnostics

import (
	"context"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Sawmonabo/codectx/internal/config"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/toolchain"
)

// This is the package's ONE scenario table. Every lane adds its rows under its
// own marker and nowhere else; a per-lane test file is a finding, and so is a
// row that re-asserts Validate(), a getter, an enum spelling or forwarding.
// Each row names, in its comment, the failure mode it protects against.

type scenario struct {
	name string
	run  func(t *testing.T)
}

func TestDiagnostics(t *testing.T) {
	for _, s := range scenarios {
		t.Run(s.name, func(t *testing.T) { s.run(t) })
	}
}

var scenarios = []scenario{
	// L0 rows
	{
		// Failure mode: a metric this host cannot read is defaulted to zero,
		// so a missing resident-set reading renders as a process using no
		// memory and an operator reads a broken sampler as a healthy one.
		// Section 23 is explicit that nil and zero are different answers. The
		// unreadable path is driven through the sampler's own reader seam so
		// the invariant is proved on the host that runs the suite, not only on
		// the platforms that happen to lack the measurement.
		name: "unmeasurable metric is absent, not zero",
		run: func(t *testing.T) {
			blind := NewHostSampler(HostSamplerOptions{})
			blind.parentRSS = func() *uint64 { return nil }
			got, err := blind.Sample(context.Background())
			if err != nil {
				t.Fatalf("Sample on a host that cannot read its own RSS: %v", err)
			}
			if got.ParentRSSBytes != nil {
				t.Fatalf("unmeasurable parent RSS reported as %d, want absent", *got.ParentRSSBytes)
			}
			if got.GoManagedBytes == nil || *got.GoManagedBytes == 0 {
				t.Fatal("Go-managed bytes are measurable in-process and must be reported")
			}
			if err := got.Validate(); err != nil {
				t.Fatalf("partial report must still validate: %v", err)
			}

			real, err := NewHostSampler(HostSamplerOptions{}).Sample(context.Background())
			if err != nil {
				t.Fatalf("Sample on this host: %v", err)
			}
			if runtime.GOOS == "linux" && (real.ParentRSSBytes == nil || *real.ParentRSSBytes == 0) {
				t.Fatal("this host exposes /proc/self/statm, so parent RSS must be a real figure")
			}
		},
	},
	// L1 rows
	{
		// Failure mode: a figure this host cannot read is reported as a
		// passing check with a zero value, so an operator reads "0 bytes
		// free, minimum not breached" as a healthy disk. Section 22 requires
		// an unmeasurable check be unavailable with a reason, and an
		// unavailable check must not degrade the report either -- otherwise
		// every host without the measurement reports a broken workspace.
		name: "doctor reports an unmeasurable figure unavailable, never pass",
		run: func(t *testing.T) {
			opts := Options{Build: model.CurrentBuildInfo(), Workspace: &fakeWorkspace{free: nil}}
			opts.Config.Resources.MinFreeDiskBytes = 1 << 30
			rep, err := newTestService(t, opts).Doctor(context.Background(), model.DoctorRequest{})
			if err != nil {
				t.Fatalf("Doctor: %v", err)
			}
			got := checkNamed(t, rep, checkFreeDisk)
			if got.State != model.CheckUnavailable {
				t.Fatalf("free space state is %q, want %q", got.State, model.CheckUnavailable)
			}
			if got.Detail == "" {
				t.Fatal("an unavailable check must carry the reason it is unavailable")
			}
			if rep.State != model.CheckPass {
				t.Fatalf("report state is %q; an unavailable measurement is an answer, not a defect", rep.State)
			}
		},
	},
	{
		// Failure mode: an ordinary doctor skips the whole-database work but
		// reports the rows it skipped as `pass`, so an operator reads "the
		// index database passed its integrity checks" off a run that read no
		// page of it. Ruling QP-A: nothing is dropped from the report and
		// nothing skipped is claimed as verified -- each such row is
		// `unverified` and names the flag that verifies it.
		name: "an ordinary doctor reports the whole-database checks unverified, never passed",
		run: func(t *testing.T) {
			ordinary := &fakeStore{}
			rep, err := newTestService(t, Options{Build: model.CurrentBuildInfo(), Store: ordinary}).Doctor(context.Background(), model.DoctorRequest{})
			if err != nil {
				t.Fatalf("ordinary Doctor: %v", err)
			}
			if ordinary.statCalls != 0 {
				t.Fatalf("an ordinary doctor made %d Stats calls; the row counts are O(rows) and belong to --deep", ordinary.statCalls)
			}
			for _, name := range []string{checkStorageIntegrity, checkStorageAccount} {
				got := checkNamed(t, rep, name)
				if got.State != model.CheckUnverified {
					t.Fatalf("%s state is %q, want %q", name, got.State, model.CheckUnverified)
				}
				if !strings.Contains(got.Detail, shallowUnverified) {
					t.Fatalf("%s detail %q does not say it was %q", name, got.Detail, shallowUnverified)
				}
			}
			if rep.State != model.CheckPass {
				t.Fatalf("report state is %q; a deliberately skipped check is not a defect", rep.State)
			}

			deep := &fakeStore{}
			deepRep, err := newTestService(t, Options{Build: model.CurrentBuildInfo(), Store: deep}).Doctor(context.Background(), model.DoctorRequest{Deep: true})
			if err != nil {
				t.Fatalf("deep Doctor: %v", err)
			}
			if deep.statCalls != 1 {
				t.Fatalf("a deep doctor made %d Stats calls, want exactly 1", deep.statCalls)
			}

			// The retained-object check is a bounded sample either way, so it
			// reports what it sampled and is `unverified` in exactly one case:
			// an empty sample, which without the retained-object count cannot
			// be told from a store whose every object is unreadable. Documenting
			// it as a deep-only walk would be wrong, and only a populated
			// fixture catches that -- the empty one above passes either way.
			if got := checkNamed(t, rep, checkSourceRetention); got.State != model.CheckUnverified {
				t.Fatalf("source_retention on an empty sample is %q, want %q", got.State, model.CheckUnverified)
			}
			populated := &fakeStore{hashes: []string{"a", "b"}}
			popRep, err := newTestService(t, Options{Build: model.CurrentBuildInfo(), Store: populated}).Doctor(context.Background(), model.DoctorRequest{})
			if err != nil {
				t.Fatalf("ordinary Doctor on a populated store: %v", err)
			}
			if got := checkNamed(t, popRep, checkSourceRetention); got.State != model.CheckPass {
				t.Fatalf("source_retention state is %q on a store whose sample verified, want %q", got.State, model.CheckPass)
			}
			if populated.statCalls != 0 {
				t.Fatalf("the populated ordinary doctor made %d Stats calls, want 0", populated.statCalls)
			}
			for _, name := range []string{checkStorageIntegrity, checkStorageAccount} {
				if got := checkNamed(t, deepRep, name); got.State != model.CheckPass {
					t.Fatalf("deep %s state is %q, want %q", name, got.State, model.CheckPass)
				}
			}
		},
	},
	{
		// Failure mode: the expensive integrity pass runs on every ordinary
		// call, so `version`, `status` and `search` each pay for a full
		// database and content-addressed-storage scan. Section 22's last line
		// reserves that work for --deep, and the fake records what the store
		// was actually asked for.
		name: "doctor scans the database only when deep is asked for",
		run: func(t *testing.T) {
			ordinary := &fakeStore{}
			if _, err := newTestService(t, Options{Store: ordinary}).Doctor(context.Background(), model.DoctorRequest{}); err != nil {
				t.Fatalf("ordinary Doctor: %v", err)
			}
			if ordinary.checks != 1 || ordinary.deepChecks != 0 {
				t.Fatalf("an ordinary doctor ran %d quick and %d deep integrity checks, want 1 and 0", ordinary.checks, ordinary.deepChecks)
			}
			deep := &fakeStore{}
			if _, err := newTestService(t, Options{Store: deep}).Doctor(context.Background(), model.DoctorRequest{Deep: true}); err != nil {
				t.Fatalf("deep Doctor: %v", err)
			}
			if deep.deepChecks != 1 || deep.checks != 0 {
				t.Fatalf("a deep doctor ran %d quick and %d deep integrity checks, want 0 and 1", deep.checks, deep.deepChecks)
			}
			if ordinary.sampleLimit >= deep.sampleLimit {
				t.Fatalf("ordinary sampled %d objects and deep sampled %d; deep must widen the sample", ordinary.sampleLimit, deep.sampleLimit)
			}
		},
	},
	{
		// Failure mode: a probe's own message is passed through into the
		// report, so the one command an operator runs on a sick workspace
		// prints SQL text, the private absolute data directory and an
		// analyzer's product name. Section 21 closes that channel everywhere
		// else; a check's detail and remediation are generated from the typed
		// code alone.
		name: "doctor never repeats a probe's own text",
		run: func(t *testing.T) {
			leak := &model.Error{Code: model.CodeStorageCorrupt,
				Message:     `quick_check: SELECT * FROM search_fts failed at /home/private/.local/share/codectx/codectx.db (acme-analyzer 4.1)`,
				Remediation: "inspect /home/private/.local/share/codectx by hand"}
			opts := Options{
				Store:     &fakeStore{err: leak},
				Workspace: &fakeWorkspace{writeErr: leak, readErr: leak, freeErr: leak},
				Sampler:   &fakeSampler{err: leak},
				Toolchain: &fakeToolchain{err: leak},
			}
			rep, err := newTestService(t, opts).Doctor(context.Background(), model.DoctorRequest{})
			if err != nil {
				t.Fatalf("a failing probe must produce a report, not an error: %v", err)
			}
			if rep.State != model.CheckFail {
				t.Fatalf("report state is %q, want %q when every probe failed", rep.State, model.CheckFail)
			}
			for _, c := range rep.Checks {
				for _, secret := range []string{"SELECT", "search_fts", "quick_check", "/home/private", "acme-analyzer"} {
					for field, value := range map[string]string{"detail": c.Detail, "remediation": c.Remediation, "code": c.Code} {
						if strings.Contains(value, secret) {
							t.Fatalf("check %q %s repeated %q from the probe's own message: %q", c.Name, field, secret, value)
						}
					}
				}
			}
			if got := checkNamed(t, rep, checkStorageIntegrity); got.Code != model.CodeStorageCorrupt || got.Remediation == "" {
				t.Fatalf("the typed code and a generated remediation must survive: %+v", got)
			}
		},
	},
	{
		// Failure mode: `--scip-index typo.scip` plans no unit, the command
		// exits 0, and the repository reads as fully indexed while its
		// cross-file symbols are missing -- a typo'd path is today
		// indistinguishable from no path at all. The three states must carry
		// different states and codes, and a build that cannot tell them apart
		// must say so rather than pass.
		name: "doctor distinguishes a supplied index that resolved to nothing",
		run: func(t *testing.T) {
			run := func(t *testing.T, store StoreReader) model.DoctorCheck {
				t.Helper()
				rep, err := newTestService(t, Options{Store: store}).Doctor(context.Background(), model.DoctorRequest{})
				if err != nil {
					t.Fatalf("Doctor: %v", err)
				}
				return checkNamed(t, rep, checkSuppliedIndex)
			}
			none := run(t, &fakeStore{active: 7})
			imported := run(t, &fakeStore{active: 7, supplied: []SuppliedIndex{{Path: "out/index.scip", Resolved: true}}})
			typo := run(t, &fakeStore{active: 7, supplied: []SuppliedIndex{{Path: "out/indx.scip"}}})
			if none.State != model.CheckPass || none.Code != "" {
				t.Fatalf("no supplied index must pass with no code, got %+v", none)
			}
			if imported.State != model.CheckPass {
				t.Fatalf("an imported supplied index must pass, got %+v", imported)
			}
			if typo.State == none.State || typo.Code == none.Code {
				t.Fatalf("a path that resolved to nothing reports %q/%q, indistinguishable from no path at all %q/%q",
					typo.State, typo.Code, none.State, none.Code)
			}
			if typo.Code != model.CodeScopeIncomplete || !strings.Contains(typo.Detail, "out/indx.scip") {
				t.Fatalf("the unresolved path and its code must be named: %+v", typo)
			}
			// A store that records no supplied path cannot tell the two
			// apart, and must report that rather than pass silently -- the
			// composition root's adapter has to forward these probes.
			if bare := run(t, bareStore{}); bare.State != model.CheckUnavailable {
				t.Fatalf("a store with no supplied-index probe reports %q, want %q", bare.State, model.CheckUnavailable)
			}
		},
	},
	// L2 rows
	{
		// Failure mode: a host that cannot observe a running process tree
		// reports the worker and analyzer figures as zero instead of as
		// absent, so an operator diagnosing memory pressure reads "the
		// children use no memory" from a sampler that simply cannot see them
		// -- and on a host that can see them, a tree measured only after its
		// children have exited reports the same zero for a tree that was
		// there. Both halves are the same Section 23 invariant, and the
		// unmeasurable half is driven through the sampler's reader seam so it
		// is proved on this host rather than only on darwin and windows.
		name: "a live tree is measured and an unobservable one is absent, not zero",
		run: func(t *testing.T) {
			blind := NewHostSampler(HostSamplerOptions{})
			blind.treeRSS = func() (*uint64, *uint64) { return nil, nil }
			got, err := blind.Sample(context.Background())
			if err != nil {
				t.Fatalf("Sample on a host with no process accounting: %v", err)
			}
			if got.BaseWorkerRSSBytes != nil || got.NativeWorkerBytes != nil {
				t.Fatalf("unobservable tree reported as base=%v native=%v, want both absent",
					deref(got.BaseWorkerRSSBytes), deref(got.NativeWorkerBytes))
			}

			// A real child, still running while the sweep happens: the tree
			// figure must exceed nothing at all, and the parent's own reading
			// must not be what is counted for it.
			if runtime.GOOS != "linux" {
				return
			}
			child := exec.Command("/bin/sh", "-c", "sleep 5")
			if err := child.Start(); err != nil {
				t.Skipf("no /bin/sh on this host: %v", err)
			}
			defer func() { child.Process.Kill(); child.Wait() }()
			var native uint64
			deadline := time.Now().Add(5 * time.Second)
			for time.Now().Before(deadline) {
				live, err := NewHostSampler(HostSamplerOptions{}).Sample(context.Background())
				if err != nil {
					t.Fatalf("Sample with a live child: %v", err)
				}
				if live.NativeWorkerBytes == nil {
					t.Fatal("this host exposes /proc, so a live descendant must be measured, not absent")
				}
				if native = *live.NativeWorkerBytes; native > 0 {
					break
				}
			}
			if native == 0 {
				t.Fatal("a descendant that is still running was summed as zero bytes")
			}
		},
	},
	{
		// Failure mode: a watch process dies, its heartbeat row stays behind,
		// and every other process keeps reporting its last pending-event
		// count as live watch coverage -- "0 pending" from a watcher that
		// stopped watching reads as "this workspace is caught up" forever,
		// which is false readiness of exactly the kind Section 13.2 forbids.
		// The writer's own expiry is the only death signal, so a row past it
		// must report no figure at all and must warn, while a live one
		// reports the figure and a never-watched workspace says so without
		// warning.
		name: "an expired watch heartbeat is never reported as live coverage",
		run: func(t *testing.T) {
			now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
			pending := int64(7)
			pass := now.Add(-time.Second)
			at := func(d time.Duration) *WatchHeartbeat {
				return &WatchHeartbeat{WriterPID: 4321, LastPassAt: &pass,
					PendingEvents: &pending, ExpiresAt: now.Add(d)}
			}
			run := func(t *testing.T, hb *WatchHeartbeat) (model.ResourceReport, model.DoctorCheck) {
				t.Helper()
				svc := newTestService(t, Options{Store: &fakeStore{heartbeat: hb}})
				res, err := svc.Resources(context.Background())
				if err != nil {
					t.Fatalf("Resources: %v", err)
				}
				rep, err := svc.Doctor(context.Background(), model.DoctorRequest{})
				if err != nil {
					t.Fatalf("Doctor: %v", err)
				}
				return res, checkNamed(t, rep, checkWatchHeartbeat)
			}

			liveRes, liveCheck := run(t, at(time.Minute))
			if liveRes.PendingEvents == nil || *liveRes.PendingEvents != pending {
				t.Fatalf("a live heartbeat must report its pending count, got %v", liveRes.PendingEvents)
			}
			if liveCheck.State != model.CheckPass || !strings.Contains(liveCheck.Detail, "4321") {
				t.Fatalf("a live watch must pass and name the process holding it: %+v", liveCheck)
			}

			deadRes, deadCheck := run(t, at(-time.Second))
			if deadRes.PendingEvents != nil {
				t.Fatalf("an expired heartbeat reported %d pending events as live coverage", *deadRes.PendingEvents)
			}
			if deadCheck.State != model.CheckWarn || deadCheck.Code != model.CodeSnapshotChanged {
				t.Fatalf("a watch that stopped refreshing must warn: %+v", deadCheck)
			}

			noneRes, noneCheck := run(t, nil)
			if noneRes.PendingEvents != nil {
				t.Fatalf("a workspace with no heartbeat reported %d pending events", *noneRes.PendingEvents)
			}
			if noneCheck.State != model.CheckUnavailable {
				t.Fatalf("a workspace nobody watches is unavailable, not a defect: %+v", noneCheck)
			}
			// A composition root that forgets to forward the probe must say
			// so rather than report a workspace as unwatched.
			svc := newTestService(t, Options{Store: bareStore{}})
			rep, err := svc.Doctor(context.Background(), model.DoctorRequest{})
			if err != nil {
				t.Fatalf("Doctor: %v", err)
			}
			if bare := checkNamed(t, rep, checkWatchHeartbeat); bare.State != model.CheckUnavailable {
				t.Fatalf("a store with no heartbeat probe reports %q, want %q", bare.State, model.CheckUnavailable)
			}
		},
	},
	// FX-H-X1 rows
	{
		// Failure mode: ADR-0004 Decision 1a tells an operator that a receipt
		// acknowledged under `normal` can be lost to a machine-level crash and
		// that `full` closes that window, while no doctor row said which mode
		// the workspace is running in -- so the one fact the decision asks an
		// operator to check was unreadable from the product. The mode is
		// reported by BOTH readings of the storage check, because an ordinary
		// doctor is the call an operator actually makes.
		// FX-H-X7 (REV-H3b D4 residual): the row reported the CONFIGURED mode,
		// which verifies nothing -- it re-prints an input. The fixture is
		// therefore MISMATCHED on purpose: configured `normal`, live `full`.
		// A phrase that echoes configuration prints normal and fails here.
		name: "the storage check reads the live durability mode back",
		run: func(t *testing.T) {
			cfg := config.Defaults()
			cfg.Storage.Synchronous = config.SynchronousNormal
			svc := newTestService(t, Options{Config: cfg,
				Store: &fakeStore{synchronous: config.SynchronousFull}})
			for _, req := range []model.DoctorRequest{{}, {Deep: true}} {
				rep, err := svc.Doctor(context.Background(), req)
				if err != nil {
					t.Fatalf("Doctor(deep=%v): %v", req.Deep, err)
				}
				got := checkNamed(t, rep, checkStorageIntegrity)
				if !strings.Contains(got.Detail, "storage.synchronous reads back full") {
					t.Fatalf("storage_integrity (deep=%v) detail %q does not report the mode the store "+
						"read back (full); the configured mode is normal", req.Deep, got.Detail)
				}
			}
			// The honesty half: a store that cannot answer must drop the
			// read-back claim rather than print configuration as measurement.
			bare := newTestService(t, Options{Config: cfg, Store: bareStore{}})
			rep, err := bare.Doctor(context.Background(), model.DoctorRequest{})
			if err != nil {
				t.Fatalf("Doctor with a bare store: %v", err)
			}
			got := checkNamed(t, rep, checkStorageIntegrity)
			if strings.Contains(got.Detail, "reads back") || !strings.Contains(got.Detail, "as configured") {
				t.Fatalf("a store that cannot read the mode back reports %q; it must name the configured "+
					"mode as configured", got.Detail)
			}
		},
	},
	// L3a rows
	// L3b rows
	// L4 rows
	// L5 rows
	// L6 rows
	// L7 rows (none: docs carry no test rows)
}

// --- deterministic fakes for the four frozen interfaces ---------------------
//
// They are values, not mocks: a row sets the fields it cares about and reads
// calls back off the recorder. A lane that needs another field adds it here
// rather than declaring a second fake.

type fakeSampler struct {
	report model.ResourceReport
	err    error
}

func (f *fakeSampler) Sample(context.Context) (model.ResourceReport, error) {
	return f.report, f.err
}

type fakeStore struct {
	stats  StoreStats
	active model.GenerationID
	blob   model.BlobRecord
	err    error
	// deepChecks and checks record what a doctor call actually asked the
	// store for, which is how a row proves an ordinary call runs no scan.
	checks     int
	deepChecks int
	// statCalls records Stats calls. Stats is eleven count(*) scans, so an
	// ordinary doctor must make none of them.
	statCalls int
	// L1: the two optional probes doctor.go asserts for. supplied is what
	// SuppliedIndexes reports; sampleLimit records the sample size the caller
	// asked for, so a row can prove --deep widens it.
	supplied    []SuppliedIndex
	sampleLimit int
	// synchronous is the LIVE durability mode this store reads back, which a
	// row sets to something the configuration does not say so that a phrase
	// echoing the configuration cannot pass. Empty means the read failed.
	synchronous string
	// hashes is what SampleBlobs reports. Nil is the ordinary fixture (an
	// empty store); a row that needs a populated one sets it.
	hashes []string
	// heartbeat is the watch heartbeat the store holds, absent when nil.
	heartbeat *WatchHeartbeat
}

// SampleBlobs is the optional hash source for the content-addressed-storage
// check (L1). It reports no hashes: a store with nothing retained is a
// legitimate state, and the rows that care assert on sampleLimit.
func (f *fakeStore) SampleBlobs(_ context.Context, limit int) ([]string, error) {
	f.sampleLimit = limit
	if len(f.hashes) > limit {
		return f.hashes[:limit], f.err
	}
	return f.hashes, f.err
}

// SuppliedIndexes is the optional supplied-index probe (L1).
func (f *fakeStore) SuppliedIndexes(context.Context, model.GenerationID) ([]SuppliedIndex, error) {
	return f.supplied, f.err
}

// WatchHeartbeat is the optional watch-liveness probe. A nil heartbeat is a
// workspace no watch ever ran in, which is a different answer from an expired
// one.
func (f *fakeStore) WatchHeartbeat(context.Context, model.RepositoryID) (WatchHeartbeat, bool, error) {
	if f.heartbeat == nil {
		return WatchHeartbeat{}, false, f.err
	}
	return *f.heartbeat, true, f.err
}

func (f *fakeStore) SynchronousMode(context.Context) (string, error) {
	if f.synchronous == "" {
		return "", f.err
	}
	return f.synchronous, f.err
}

// bareStore is a StoreReader implementing the frozen four methods and none of
// the optional probes, which is what the composition root hands diagnostics if
// its adapter forgets to forward them (L1). It exists to prove that case
// reports unavailable with a reason rather than passing silently.
type bareStore struct{}

func (bareStore) Check(context.Context, bool) error         { return nil }
func (bareStore) Stats(context.Context) (StoreStats, error) { return StoreStats{}, nil }
func (bareStore) Blob(context.Context, string) (model.BlobRecord, error) {
	return model.BlobRecord{}, nil
}

func (bareStore) ActiveGeneration(context.Context, model.RepositoryID) (model.GenerationID, error) {
	return 7, nil
}

// checkNamed returns the one check called name, failing the row if the report
// does not carry it: a check that silently stopped being produced would
// otherwise read as a passing assertion.
func checkNamed(t *testing.T, r model.DoctorReport, name string) model.DoctorCheck {
	t.Helper()
	for _, c := range r.Checks {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("report carries no check named %q", name)
	return model.DoctorCheck{}
}

func (f *fakeStore) Check(_ context.Context, deep bool) error {
	if deep {
		f.deepChecks++
	} else {
		f.checks++
	}
	return f.err
}

func (f *fakeStore) Stats(context.Context) (StoreStats, error) {
	f.statCalls++
	return f.stats, f.err
}

// StoreSizes is the optional cheap-size probe a shallow accounting check reads
// instead of Stats. It is two file stats in the real store and counts nothing.
func (f *fakeStore) StoreSizes(context.Context) (int64, int64, error) {
	return f.stats.DatabaseBytes, f.stats.WALBytes, f.err
}

func (f *fakeStore) ActiveGeneration(context.Context, model.RepositoryID) (model.GenerationID, error) {
	return f.active, f.err
}

func (f *fakeStore) Blob(context.Context, string) (model.BlobRecord, error) {
	return f.blob, f.err
}

type fakeToolchain struct {
	statuses []toolchain.Status
	err      error
}

func (f *fakeToolchain) Statuses(context.Context) ([]toolchain.Status, error) {
	return f.statuses, f.err
}

type fakeWorkspace struct {
	writeErr error
	readErr  error
	free     *uint64
	freeErr  error
}

func (f *fakeWorkspace) Writable(context.Context, string) error { return f.writeErr }

func (f *fakeWorkspace) Readable(context.Context, string) error { return f.readErr }

func (f *fakeWorkspace) FreeDiskBytes(context.Context, string) (*uint64, error) {
	return f.free, f.freeErr
}

// newTestService builds a Service over the fakes with a fixed clock. A row that
// needs a different dependency replaces it on the returned Options copy.
func newTestService(t *testing.T, opts Options) *Service {
	t.Helper()
	if opts.Sampler == nil {
		opts.Sampler = &fakeSampler{}
	}
	if opts.Store == nil {
		opts.Store = &fakeStore{}
	}
	if opts.Toolchain == nil {
		opts.Toolchain = &fakeToolchain{}
	}
	if opts.Workspace == nil {
		opts.Workspace = &fakeWorkspace{}
	}
	if opts.Root == "" {
		opts.Root = t.TempDir()
	}
	if opts.Repo == "" {
		opts.Repo = model.RepositoryID("00000000000000000000000000000000000000000000000000000000000000ab")
	}
	if opts.Now == nil {
		fixed := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
		opts.Now = func() time.Time { return fixed }
	}
	s, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

// deref renders a possibly absent byte count for a failure message.
func deref(p *uint64) any {
	if p == nil {
		return "absent"
	}
	return *p
}
