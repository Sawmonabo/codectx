package diagnostics

import (
	"context"
	"runtime"
	"testing"
	"time"

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
			blind := &HostSampler{parentRSS: func() *uint64 { return nil }}
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

			real, err := NewHostSampler().Sample(context.Background())
			if err != nil {
				t.Fatalf("Sample on this host: %v", err)
			}
			if runtime.GOOS == "linux" && (real.ParentRSSBytes == nil || *real.ParentRSSBytes == 0) {
				t.Fatal("this host exposes /proc/self/statm, so parent RSS must be a real figure")
			}
		},
	},
	// L1 rows
	// L2 rows
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
}

func (f *fakeStore) Check(_ context.Context, deep bool) error {
	if deep {
		f.deepChecks++
	} else {
		f.checks++
	}
	return f.err
}

func (f *fakeStore) Stats(context.Context) (StoreStats, error) { return f.stats, f.err }

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
	free     *uint64
	freeErr  error
}

func (f *fakeWorkspace) Writable(context.Context, string) error { return f.writeErr }

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
