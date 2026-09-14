package diagnostics

import (
	"context"
	"time"

	"github.com/Sawmonabo/codectx/internal/config"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/toolchain"
)

// The four narrow interfaces below are the whole dependency surface of this
// package. They are deliberately interfaces and not the concrete types the
// composition root owns: a *sqlite.Store cannot be faked, and the Section 22
// check list has to be provable without a database, a tool store or a live
// child process on the machine running the test.
//
// Every method here is satisfied by a type that already exists at this commit,
// so composing them is wiring and not new storage work.

// Sampler measures the Section 23 host metrics.
//
// A nil pointer field in the returned report means "not measurable on this
// host"; a zero value is a real measurement of zero. Collapsing the two would
// let a missing resident-set reading render as a process using no memory, which
// Section 23 forbids explicitly. An implementation therefore leaves a field nil
// rather than defaulting it, and it never returns an error for a metric it
// simply cannot read -- the absence is the answer.
type Sampler interface {
	Sample(ctx context.Context) (model.ResourceReport, error)
}

// StoreReader is the read surface diagnostics needs from the index store. Each
// method is an existing *sqlite.Store method with the same name and signature
// apart from Stats, whose result is restated here as StoreStats so this package
// does not import internal/storage/sqlite (see doc.go); the composition root
// adapts it.
type StoreReader interface {
	// Check runs the Section 12.1 integrity checks. deep adds the expensive
	// FTS5 external-content check, and an ordinary doctor call must pass
	// false: Section 22's last line forbids a full database scan on an
	// ordinary command.
	Check(ctx context.Context, deep bool) error
	// Stats reports row counts and the on-disk database and WAL sizes.
	Stats(ctx context.Context) (StoreStats, error)
	// ActiveGeneration reports the repository's active-pointer target.
	ActiveGeneration(ctx context.Context, repo model.RepositoryID) (model.GenerationID, error)
	// Blob reads one content-addressed blob record, which is how a CAS probe
	// confirms that a sampled object is still recorded as ready.
	Blob(ctx context.Context, hash string) (model.BlobRecord, error)
}

// StoreStats is sqlite.Stats restated as a dependency-free value. It carries
// the counts and sizes Section 22 and Section 23 report and nothing else.
type StoreStats struct {
	Generations   int64
	Snapshots     int64
	Files         int64
	Blobs         int64
	Units         int64
	NodeFacts     int64
	RelationFacts int64
	Evidence      int64
	SearchUnits   int64
	Leases        int64
	Sessions      int64
	DatabaseBytes int64
	WALBytes      int64
}

// ToolchainReporter reports one status per lock entry. The lock entry name is
// what a check prints for a managed tool; the dependence backend is reported in
// the `engine <version> <digest>` form the provider already uses, never by the
// backend's product name.
type ToolchainReporter interface {
	Statuses(ctx context.Context) ([]toolchain.Status, error)
}

// WorkspaceProber answers the questions about the filesystem that neither the
// store nor the tool store can: whether the workspace and the data directory
// are readable and writable, and how much space is left under the data
// directory. FreeDiskBytes returns a nil count on a host where free space
// cannot be determined -- again, absent rather than zero.
type WorkspaceProber interface {
	Writable(ctx context.Context, dir string) error
	FreeDiskBytes(ctx context.Context, dir string) (*uint64, error)
}

// Options are the dependencies of a Service. Every field is required except
// Now, which defaults to time.Now.
type Options struct {
	Config    config.Config
	Build     model.BuildInfo
	Repo      model.RepositoryID
	Sampler   Sampler
	Store     StoreReader
	Toolchain ToolchainReporter
	Workspace WorkspaceProber
	// Now is the clock every check and the report's CheckedAt read (L1). A
	// test supplies a fixed one so a doctor report is deterministic.
	Now func() time.Time
}

// Service produces the Section 22 doctor report and the Section 23 resource
// block. It holds no mutable state and is safe for concurrent use.
type Service struct {
	opts Options
}

// New validates the dependency set and builds the service. A missing dependency
// is a composition defect, not a user error, so it is CTX_INTERNAL.
func New(opts Options) (*Service, error) {
	switch {
	case opts.Sampler == nil:
		return nil, missingDependency("sampler")
	case opts.Store == nil:
		return nil, missingDependency("store reader")
	case opts.Toolchain == nil:
		return nil, missingDependency("toolchain reporter")
	case opts.Workspace == nil:
		return nil, missingDependency("workspace prober")
	case opts.Repo == "":
		return nil, missingDependency("repository id")
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Service{opts: opts}, nil
}

// missingDependency is the composition refusal New returns.
func missingDependency(what string) error {
	return &model.Error{Code: model.CodeInternal,
		Message:     "diagnostics: " + what + " is required",
		Remediation: "this is a composition defect; report it with the command you ran"}
}

// notImplemented is what a stub below returns until its lane lands. It is
// CTX_INTERNAL because no caller input can correct it, and it names the lane so
// an operator who somehow reaches it learns what is absent rather than that
// something broke. A stub never panics.
func notImplemented(op, lane string) error {
	return &model.Error{Code: model.CodeInternal,
		Message:     "diagnostics: " + op + " has no implementation in this build",
		Remediation: "this operation waits on lane " + lane}
}

// Frozen by L0, implemented by the named lane. A lane edits only its own file.
//
//	sample.go     -- L2: the Sampler implementation, simultaneous tree sampling
//	resources.go  -- L2: func (s *Service) Resources(ctx) (model.ResourceReport, error)
//	doctor.go     -- L1: func (s *Service) Doctor(ctx, model.DoctorRequest) (model.DoctorReport, error)
//	grace.go      -- L3a: quarantine -> trash -> grace -> recheck -> delete
//	sweep.go      -- L3b: the five caller-less helpers plus the two unreclaimed trees
//	internal/graph/overview.go         -- L5: func (e *Engine) Overview(ctx, model.OverviewRequest) (model.Page[model.OverviewItem], error)
//	internal/cli/doctor.go, repomap.go -- L6
//	internal/app/*, internal/cli/root.go -- INT, and INT only
//
// Ruling Q1 widens the status facade. L0 freezes the request type
// (model.StatusRequest, internal/model/status.go) and this signature; INT
// re-points the facade, the CLI reader and the MCP tool input:
//
//	func (s *Services) IndexStatus(ctx context.Context, req model.StatusRequest) (model.IndexStatus, error)
