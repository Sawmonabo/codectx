package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"

	"github.com/Sawmonabo/codectx/internal/diagnostics"
	"github.com/Sawmonabo/codectx/internal/diskfree"
	"github.com/Sawmonabo/codectx/internal/index"
	"github.com/Sawmonabo/codectx/internal/ledger"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/paced"
	"github.com/Sawmonabo/codectx/internal/snapshot"
	"github.com/Sawmonabo/codectx/internal/storage/sqlite"
	"github.com/Sawmonabo/codectx/internal/toolchain"
)

// The adapters below are the whole of what internal/diagnostics and
// internal/retention know about this application's concrete types. Both
// packages depend on narrow interfaces and never on *sqlite.Store,
// *toolchain.Resolver or *snapshot.CAS (their doc.go states the rule), so the
// composition root is where a concrete type meets a frozen interface. Each
// adapter here forwards and adapts shape; none of them implements a probe, a
// measurement or a policy of its own.

// storeReader adapts *sqlite.Store to diagnostics.StoreReader. Check,
// ActiveGeneration and Blob are forwarded by embedding -- their signatures
// already match -- and only Stats needs a body, because diagnostics restates
// sqlite.Stats as its own dependency-free StoreStats.
type storeReader struct{ *sqlite.Store }

// Stats restates sqlite.Stats as diagnostics.StoreStats field for field. It is
// a copy rather than a shared type because internal/diagnostics must not import
// a storage package; a field added to one side and not the other fails to
// compile here, which is where it should fail.
func (r storeReader) Stats(ctx context.Context) (diagnostics.StoreStats, error) {
	s, err := r.Store.Stats(ctx)
	if err != nil {
		return diagnostics.StoreStats{}, err
	}
	return diagnostics.StoreStats{
		Generations:   s.Generations,
		Snapshots:     s.Snapshots,
		Files:         s.Files,
		Blobs:         s.Blobs,
		Units:         s.Units,
		NodeFacts:     s.NodeFacts,
		RelationFacts: s.RelationFacts,
		Evidence:      s.Evidence,
		SearchUnits:   s.SearchUnits,
		Leases:        s.Leases,
		Sessions:      s.Sessions,
		DatabaseBytes: s.DatabaseBytes,
		WALBytes:      s.WALBytes,
	}, nil
}

// SampleBlobs is forwarded by the embedded *sqlite.Store, which is what makes
// the doctor's content-addressed-store sample a real check rather than a
// permanently unavailable one: the check discovers the capability by asserting
// it on the reader it was handed.
var _ diagnostics.StoreReader = storeReader{}

// StoreSizes is forwarded by the embedded *sqlite.Store. The assertion is what
// keeps a shallow doctor's accounting row a real measurement: without it a
// signature drift would leave the optional interface unsatisfied and the check
// permanently `unavailable`, with nothing failing to compile.
var _ diagnostics.StoreSizer = storeReader{}

// SuppliedIndexes restates (*sqlite.Store).SuppliedIndexes in the doctor's own
// vocabulary, for the same reason Stats does: internal/diagnostics must not
// import a storage package, so the two structurally identical types meet here.
//
// It is a written adapter rather than an embedded forward because the return
// element type differs, and an embedded method with the wrong signature would
// silently fail the optional-interface assertion in checkSuppliedIndex and
// leave the check `unavailable` forever with nothing failing to compile. The
// assertion below is what makes that a build error instead.
func (r storeReader) SuppliedIndexes(ctx context.Context, gen model.GenerationID) ([]diagnostics.SuppliedIndex, error) {
	rows, err := r.Store.SuppliedIndexes(ctx, gen)
	if err != nil {
		return nil, err
	}
	out := make([]diagnostics.SuppliedIndex, 0, len(rows))
	for _, row := range rows {
		out = append(out, diagnostics.SuppliedIndex{Path: row.Path, Resolved: row.Resolved})
	}
	return out, nil
}

// The assertion the exported interface exists for: a signature drift here is a
// build failure, not a doctor check that quietly reports unavailable forever.
var _ diagnostics.SuppliedIndexReader = storeReader{}

// WatchHeartbeats restates (*sqlite.Store).WatchHeartbeats in the doctor's own
// vocabulary. It is written rather than embedded for the same reason
// SuppliedIndexes is: the result type differs, and an embedded forward with the
// wrong signature would satisfy nothing, fail no build, and leave both the
// `watch_heartbeat` check and the resource block's pending-event count
// permanently absent with nothing to show for it.
//
// Every row is carried through, expired ones included and in the order the
// store read them: absent, live and expired are three answers the check renders
// differently, and judging any of them here would lose the distinction before it
// reaches the renderer.
func (r storeReader) WatchHeartbeats(ctx context.Context, repo model.RepositoryID) ([]diagnostics.WatchHeartbeat, error) {
	rows, err := r.Store.WatchHeartbeats(ctx, repo)
	if err != nil {
		return nil, err
	}
	out := make([]diagnostics.WatchHeartbeat, 0, len(rows))
	for _, hb := range rows {
		out = append(out, diagnostics.WatchHeartbeat{
			WriterPID:     hb.WriterPID,
			LastPassAt:    hb.LastPassAt,
			PendingEvents: hb.PendingEvents,
			ExpiresAt:     hb.ExpiresAt,
		})
	}
	return out, nil
}

// The same compile-time assertion, for the same reason.
var _ diagnostics.WatchHeartbeatReader = storeReader{}

// runLedger adapts the run ledger beside the index store to
// diagnostics.RunLedger, and is the one place a recorded run becomes the rows
// the model carries.
//
// It opens the ledger for each call rather than holding a connection open,
// because the report is a one-shot answer and the open is read-only: a status
// surface must be able to read a run that another process is writing and must
// never become a second writer of the file. A workspace that has recorded no
// run reports no rows and no failure, which is the reader's own answer.
type runLedger struct{ dir string }

func (l runLedger) LatestRun(ctx context.Context, repo model.RepositoryID,
	generation model.GenerationID) (*model.RunRecord, []model.StageRecord, int64, error) {
	reader, recorded, err := ledger.OpenReader(ctx, l.dir)
	if err != nil || !recorded {
		return nil, nil, 0, err
	}
	defer reader.Close()
	view, found, err := reader.LatestRun(ctx, string(repo), int64(generation))
	if err != nil || !found {
		return nil, nil, 0, err
	}
	return runRecord(view)
}

// Run is the run with this identifier, which is how a run that has just ended
// reports on itself: it knows its own id, and the latest run of the repository
// may be another live run of this same process.
func (l runLedger) Run(ctx context.Context, runID string) (*model.RunRecord, []model.StageRecord, int64, error) {
	reader, recorded, err := ledger.OpenReader(ctx, l.dir)
	if err != nil || !recorded {
		return nil, nil, 0, err
	}
	defer reader.Close()
	view, found, err := reader.Run(ctx, runID)
	if err != nil || !found {
		return nil, nil, 0, err
	}
	return runRecord(view)
}

// runRecord is the one place a read run becomes the rows the model carries,
// whichever question selected it.
func runRecord(view ledger.RunView) (*model.RunRecord, []model.StageRecord, int64, error) {
	run := model.RunRecord{
		RunID:               view.Run.RunID,
		Kind:                string(view.Run.Kind),
		RepositoryID:        view.Run.RepositoryID,
		GenerationID:        view.Run.GenerationID,
		StartedAt:           view.Run.StartedAt,
		FinishedAt:          view.Run.FinishedAt,
		WallMS:              view.Run.WallMS,
		Outcome:             string(view.Run.Outcome),
		FileCount:           view.Run.FileCount,
		SourceBytes:         view.Run.SourceBytes,
		UnitsPlanned:        view.Run.UnitsPlanned,
		UnitsSucceeded:      view.Run.UnitsSucceeded,
		UnitsFailed:         view.Run.UnitsFailed,
		UnitsSubdivided:     view.Run.UnitsSubdivided,
		EventsDropped:       view.Run.EventsDropped,
		ProcessPeakRSSBytes: view.Run.ProcessPeakRSSBytes,
	}
	stages := make([]model.StageRecord, 0, len(view.Spans))
	for _, span := range view.Spans {
		stages = append(stages, stageRecord(span))
	}
	return &run, stages, view.SpansOmitted, nil
}

var _ diagnostics.RunLedger = runLedger{}

// And the reader a run reports on itself through.
var _ index.RunLedgerReader = runLedger{}

// toolchainReporter adapts *toolchain.Resolver to diagnostics.ToolchainReporter.
// Resolver.Status returns its rows directly and reports no error; the interface
// carries one because a reporter that has to read a store may fail, and
// widening an existing signature to match would have changed every caller of
// Status for one consumer's benefit.
type toolchainReporter struct{ r *toolchain.Resolver }

func (t toolchainReporter) Statuses(ctx context.Context) ([]toolchain.Status, error) {
	return t.r.Status(ctx), nil
}

// Selected forwards to SelectedTools, the one implementation of the repository
// -> lock entry mapping. `codectx tools prefetch --for-repo` reaches it through
// internal/cli; the doctor reaches it here, because internal/diagnostics must
// not import a command package. Neither side owns a second copy.
func (t toolchainReporter) Selected(ctx context.Context, root string) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, model.Canceled(err)
	}
	return SelectedTools(root)
}

// workspaceProber answers the two filesystem questions neither the store nor
// the tool store can. It is a value with no state: the directory is the
// caller's argument, so one prober serves the data directory and the workspace
// root alike.
type workspaceProber struct{}

// Writable reports whether dir can be written by this process, by creating and
// removing a file inside it rather than by reading a permission bit: a mode
// that says yes on a read-only mount, an exhausted disk or a directory owned by
// another user is exactly the false pass Section 22 exists to prevent.
//
// The probe file is removed on every path. A leftover would be the one piece of
// litter a diagnostic command adds to the directory it is diagnosing.
//
// Every failure is returned as a typed model.Error carrying the bare syscall
// cause and no path: the doctor renders this error's text as a check detail and
// ships it in the --json envelope, and os.CreateTemp's own *fs.PathError spells
// out the absolute data directory, which ordinary output must never carry.
//
// Note the deliberate asymmetry with FreeDiskBytes below: a directory this
// process cannot write is a failure and is returned as an error, while a figure
// the host will not report is unavailable and is returned as a nil pointer.
// They are different answers to different questions; neither should be made to
// match the other.
func (workspaceProber) Writable(ctx context.Context, dir string) error {
	if err := ctx.Err(); err != nil {
		return model.Canceled(err)
	}
	if dir == "" {
		return &model.Error{Code: model.CodeInternal,
			Message:     "app: the directory to probe for writability is empty",
			Remediation: "this is a composition defect; report it with the command you ran"}
	}
	f, err := os.CreateTemp(dir, ".codectx-probe-*")
	if err != nil {
		return probeError("create a probe file", err)
	}
	name := f.Name()
	closeErr := f.Close()
	rmErr := paced.Remove(name)
	if closeErr != nil {
		// Close reports the write-back failure on the filesystems that defer
		// it, so it is answered before the removal's own error.
		return probeError("close the probe file", closeErr)
	}
	if rmErr != nil {
		return probeError("remove the probe file", rmErr)
	}
	return nil
}

// Readable reports whether dir can be listed by this process. It opens the
// directory and reads one entry: the workspace root is proved by reading
// because Section 6 forbids this product writing anything into the repository,
// and reading an entry is the whole of what a capture asks of the root.
//
// Like Writable it returns a typed model.Error carrying the bare syscall cause
// and no path, because the doctor renders this text as a check detail.
func (workspaceProber) Readable(ctx context.Context, dir string) error {
	if err := ctx.Err(); err != nil {
		return model.Canceled(err)
	}
	if dir == "" {
		return &model.Error{Code: model.CodeInternal,
			Message:     "app: the directory to probe for readability is empty",
			Remediation: "this is a composition defect; report it with the command you ran"}
	}
	f, err := os.Open(dir)
	if err != nil {
		return probeError("open the directory", err)
	}
	defer f.Close()
	// One entry, never the whole listing: this is a permission proof, and a
	// repository root holds an unbounded number of entries. io.EOF is an empty
	// but perfectly readable directory.
	if _, err := f.ReadDir(1); err != nil && !errors.Is(err, io.EOF) {
		return probeError("read the directory", err)
	}
	return nil
}

// probeError states a probe failure without naming the directory it probed. A
// full disk keeps its own family because the remediation differs; everything
// else is the exit-10 class carrying only the syscall's own cause, which is the
// same shape internal/snapshot's ioError produces for the same reason.
func probeError(op string, err error) error {
	cause := err
	var pe *fs.PathError
	var le *os.LinkError
	var se *os.SyscallError
	switch {
	case errors.As(err, &pe):
		cause = pe.Err
	case errors.As(err, &le):
		cause = le.Err
	case errors.As(err, &se):
		cause = se.Err
	}
	if errors.Is(err, syscall.ENOSPC) {
		return &model.Error{Code: model.CodeDiskFull,
			Message:     "app: could not " + op + ": the disk is full",
			Remediation: "free disk space or move storage.data_dir to a larger volume"}
	}
	return &model.Error{Code: model.CodeInternal,
		Message:     fmt.Sprintf("app: could not %s: %v", op, cause),
		Remediation: "check the directory's ownership and permissions, and that its filesystem is writable"}
}

// FreeDiskBytes reports the space available to this user under dir, or nil on a
// host or filesystem that does not expose it. Nil is "not measurable here", and
// the check that reads it renders that as `unavailable`; returning zero would
// say the disk is full.
func (workspaceProber) FreeDiskBytes(ctx context.Context, dir string) (*uint64, error) {
	if err := ctx.Err(); err != nil {
		return nil, model.Canceled(err)
	}
	if dir == "" {
		return nil, nil
	}
	free, ok := diskfree.Available(dir)
	if !ok {
		return nil, nil
	}
	return &free, nil
}

// snapshotSweeper adapts the package-level snapshot.Sweep to the collector's
// SnapshotSweeper interface. The method name differs because `Sweep` is already
// taken on the spool sweeper the same Options carries, and a collector holding
// two identically named dependencies reads as one.
type snapshotSweeper struct{}

func (snapshotSweeper) SweepSnapshots(dataDir string) error { return snapshot.Sweep(dataDir) }

// diagnosticsTempDirs are the directories whose bytes count against
// resources.max_temp_bytes, in the layout internal/snapshot and this file own
// between them. The sampler is handed the paths rather than the data directory
// so no second copy of the layout exists to drift from the first.
func diagnosticsTempDirs(dataDir string) []string {
	return []string{
		snapshot.StagingDir(dataDir),
		snapshot.MaterializeDir(dataDir),
		filepath.Join(dataDir, workDirName, spoolsDirName),
	}
}
