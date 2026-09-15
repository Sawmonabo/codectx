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
	// Check runs the Section 12.1 integrity checks. With deep false it reads
	// the database header, the schema fingerprint and the journal mode and
	// nothing else; deep adds quick_check, the foreign key check and the FTS5
	// external-content check, each of which walks the whole database. An
	// ordinary doctor call must pass false: Section 22's last line forbids a
	// full database scan on an ordinary command.
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

// StoreSizer reports only the on-disk sizes -- two file stats, no query. It is
// an optional interface beside StoreReader because StoreReader is frozen, and
// it exists so a shallow doctor can still report database and write-ahead-log
// bytes (and warn on a WAL past its high-water mark) without paying for Stats,
// whose eleven `count(*)` scans are O(rows). A store that does not implement it
// reports the accounting sizes as unavailable rather than as zero.
type StoreSizer interface {
	StoreSizes(ctx context.Context) (databaseBytes, walBytes int64, err error)
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
//
// Readable and Writable are two probes and not one on purpose. Writable proves
// a directory by creating and removing a file in it, which is the only honest
// proof and is exactly what must never happen inside the repository; Readable
// opens the directory and reads one entry, which is what a capture needs from
// the workspace root and all this product ever asks of it.
type WorkspaceProber interface {
	Readable(ctx context.Context, dir string) error
	Writable(ctx context.Context, dir string) error
	FreeDiskBytes(ctx context.Context, dir string) (*uint64, error)
}

// Options are the dependencies of a Service. Every field is required except
// Now, which defaults to time.Now.
type Options struct {
	Config config.Config
	Build  model.BuildInfo
	Repo   model.RepositoryID
	// Root is the absolute workspace root. The data-directory check probes it
	// too: a workspace whose root this process cannot read produces an empty
	// capture that reads as a repository with nothing in it, and the data
	// directory being writable says nothing about it. It is probed for
	// READability only -- Section 6 forbids this product writing anything into
	// the repository, so the write probe the data directory gets must never be
	// aimed at the workspace.
	Root      string
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
	case opts.Root == "":
		return nil, missingDependency("workspace root")
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
