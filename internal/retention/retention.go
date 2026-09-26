// Package retention is the process-level collector.
//
// Charter, and the line a reviewer checks: this package composes primitives
// that already landed -- (*sqlite.Store).RetainByRef, ExpireSessions and
// PruneSessions, snapshot.Sweep, (*pagination.Spools).Sweep and
// (*toolchain.Resolver).GC -- under one stated lock order, and it owns the one
// thing nothing owns, the Section 10.4 blob grace protocol. It must not
// re-derive retention-by-ref ranking (sqlite/retention.go owns it), re-implement
// the tool-store sweep (toolchain's store.gc already sweeps staging and the
// versions the lock no longer names; this package gives it a schedule), or open
// a store, a lock or a process of its own.
//
// Import rule, checkable by a reviewer: this package imports internal/model,
// internal/config and the standard library, and depends on the narrow
// interfaces frozen below rather than on *sqlite.Store, *pagination.Spools or
// *toolchain.Resolver. It must NOT import internal/app, internal/cli or
// internal/mcpserver -- internal/app composes it, never the reverse.
//
// LOCK ORDER -- stated once, here, and taken nowhere in this package.
//
// The caller holds the cross-process workspace lock (snapshot.WorkspaceLock)
// AND this process's indexing mutex (index.Coordinator.run) before Collect
// runs, exactly as index/retention.go:19-25 states for RetainByRef and for the
// same reason: collection ends in a sweep with no snapshot filter, which
// deletes every snapshot no generation, lease or session references, and a
// foreground indexing run has exactly such a window between writing its capture
// and opening the generation that first references it. The workspace lock keeps
// a second process out; the run mutex keeps this one out. A collector that
// takes either lock itself is drift: it would either deadlock against the
// caller that already holds it or collect outside it.
package retention

import (
	"context"
	"errors"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
)

// SessionStore is the session half of the collector's store surface. Both
// methods are existing *sqlite.Store methods with the same signature, and they
// run in this order: expiring a live session past its deadline is what makes it
// a closed session, and pruning removes closed sessions older than the
// retention window.
type SessionStore interface {
	ExpireSessions(ctx context.Context, now time.Time, limit int) (int64, error)
	PruneSessions(ctx context.Context, now time.Time, retention time.Duration, limit int) (int64, error)
}

// SpoolSweeper reclaims pagination spool files whose lease has expired or whose
// continuation was consumed. It is (*pagination.Spools).Sweep.
type SpoolSweeper interface {
	Sweep(ctx context.Context, now time.Time) (int64, error)
}

// SnapshotSweeper removes abandoned staging directories and unreferenced CAS
// temporaries under the data directory. It is snapshot.Sweep, restated as an
// interface so the collector can be driven without a data directory on disk.
type SnapshotSweeper interface {
	SweepSnapshots(dataDir string) error
}

// ToolCollector reclaims the tool store: staging directories no install owns
// and installed versions the lock no longer names. It is
// (*toolchain.Resolver).GC, which today has one caller, the manual
// `codectx tools gc` command, and no scheduled pass.
type ToolCollector interface {
	GC(ctx context.Context) (int, error)
}

// Options are the collector's dependencies and bounds. Every interface is
// required; a nil one is a composition defect.
type Options struct {
	Config   RetentionConfig
	Sessions SessionStore
	Spools   SpoolSweeper
	Snapshot SnapshotSweeper
	Tools    ToolCollector
	// Blobs and Objects are the two halves of the Section 10.4 grace protocol:
	// the store phases that demote, restore and delete a blob row, and the CAS
	// that holds the published object the last phase removes. Both are declared
	// in grace.go beside the implementation that drives them.
	Blobs   BlobStore
	Objects ObjectStore
	// Now is the clock. A test supplies a fixed one so the grace window and
	// every expiry boundary are deterministic.
	Now func() time.Time
}

// RetentionConfig is the configuration the collector reads, restated so this
// package takes no dependency on the shape of the whole config tree. DataDir
// is the absolute data directory; ClosedSessionRetention is the today-dead
// `storage.closed_session_retention` key L3b gives its first reader; GraceWindow
// is how long a trashed blob waits before L3a's second reachability check.
type RetentionConfig struct {
	DataDir                string
	ClosedSessionRetention time.Duration
	GraceWindow            time.Duration
	// BatchLimit bounds every delete pass. Section 6 requires a finite bound
	// on every traversal; a collection pass that cannot finish in one batch
	// finishes on the next one. A non-positive value is the collector's own
	// defaultBatchLimit, defaulted in New: the blob phases rescue a zero on
	// their own side, but sweep passes this straight to ExpireSessions and
	// PruneSessions, where a zero limit expires and prunes nothing while the
	// pass reports success.
	BatchLimit int
}

// defaultBatchLimit is the fallback bound New applies to a non-positive
// BatchLimit, the same role defaultGraceWindow (grace.go) plays for a zero
// grace window. It is a bound, not a setting: the composition root states the
// limit it wants, and this constant only keeps a collector built without one
// from silently reclaiming nothing.
const defaultBatchLimit = 200

// Report is what one collection pass reclaimed. Every count is what this pass
// actually removed, so a caller logs progress rather than intent.
type Report struct {
	SessionsExpired int64
	SessionsPruned  int64
	// SpoolBytesSwept is BYTES, not a count of files: (*pagination.Spools).Sweep
	// returns the live spool byte total it reconciled its budget against, and an
	// operator-facing field named for a count while carrying a byte total is a
	// misreport. The name states the unit so the number reads as what it is.
	SpoolBytesSwept  int64
	ToolsCollected   int
	BlobsQuarantined int64
	BlobsTrashed     int64
	BlobsDeleted     int64
	BlobsRestored    int64
	// OrphanObjectsSwept is CAS files no blobs row named -- content whose
	// naming commit never landed. It is counted apart from BlobsDeleted
	// because the two reclaim different states: BlobsDeleted is a row and its
	// object going together under the grace protocol, this is a file that
	// never had a row.
	OrphanObjectsSwept int64
	// OrphanSweepRan says whether the cadence gate let the orphan sweep walk
	// the store at all this pass. Without it a pass that SKIPPED the walk
	// because it was inside the grace window and a pass that walked every
	// bucket and found nothing both report `orphan_objects_swept=0`, and an
	// operator reading a log cannot tell "nothing to reclaim" from "not looked
	// at yet". It is set from the gate's own verdict rather than inferred from
	// the count, because the gate fails OPEN -- a missing, unreadable,
	// malformed or future-dated stamp sweeps -- and each of those is a pass
	// that ran.
	OrphanSweepRan bool
}

// OrphanSweepPhrase renders OrphanSweepRan for a log line, so the two passes
// that report one -- the startup collection pass and the post-activation one --
// say the same words for the same state rather than each inventing its own.
func (r Report) OrphanSweepPhrase() string {
	if r.OrphanSweepRan {
		return "ran"
	}
	return "skipped: within grace window"
}

// Collector runs one collection pass. It holds no mutable state; the lock order
// above is the caller's to establish, and this type takes no lock.
type Collector struct {
	opts Options
}

// New validates the dependency set and builds the collector.
func New(opts Options) (*Collector, error) {
	switch {
	case opts.Sessions == nil:
		return nil, missingDependency("session store")
	case opts.Spools == nil:
		return nil, missingDependency("spool sweeper")
	case opts.Snapshot == nil:
		return nil, missingDependency("snapshot sweeper")
	case opts.Tools == nil:
		return nil, missingDependency("tool collector")
	case opts.Blobs == nil:
		return nil, missingDependency("blob store")
	case opts.Objects == nil:
		return nil, missingDependency("object store")
	case opts.Config.DataDir == "":
		return nil, missingDependency("data directory")
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Config.BatchLimit <= 0 {
		opts.Config.BatchLimit = defaultBatchLimit
	}
	return &Collector{opts: opts}, nil
}

// Collect runs one pass: the sweeps first (L3b), then the blob grace protocol
// (L3a), because a session or spool released by a sweep is what makes a blob
// unreferenced in the same pass rather than the next one.
//
// The grace protocol runs even when a sweep step failed, and the two errors are
// joined -- the same rule sweep states for its own five steps, "so no step
// starves another". A failing tool-store collection or an unreadable <data>/lsp
// root must not be what stops trashed source from ever being reclaimed: the
// sweeps free references, they do not gate the protocol that acts on them, and
// each grace phase rechecks reachability inside its own transaction anyway.
//
// The caller holds both locks named in the package comment. Collect takes none.
func (c *Collector) Collect(ctx context.Context) (Report, error) {
	report, sweepErr := c.sweep(ctx)
	report, graceErr := c.grace(ctx, report)
	return report, errors.Join(sweepErr, graceErr)
}

// missingDependency is the composition refusal New returns.
func missingDependency(what string) error {
	return &model.Error{Code: model.CodeInternal,
		Message:     "retention: " + what + " is required",
		Remediation: "this is a composition defect; report it with the command you ran"}
}
