package retention

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
)

// The Section 10.4 blob grace protocol, the one thing this package owns rather
// than schedules. An unreferenced blob is quarantined, then trashed under a
// recheck of reachability, and deleted -- row, blocks, line checkpoints and
// the CAS object on disk -- only after the grace window and a further
// reachability check made inside the deleting transaction. A blob that becomes
// referenced again at any point is restored, not deleted.
//
// Binding invariant from storage/sqlite/source.go: blob_blocks and
// line_checkpoints must not be dropped before the blobs row, because PutBlob's
// restore path flips a demoted row's state and keeps them. The store honours it
// by deleting nothing but the blobs row and letting the foreign keys cascade,
// and this package never asks for a block delete of its own.

// defaultGraceWindow is how long a trashed blob waits before its final
// reachability check: the protocol's safety margin against a reader that
// pinned a generation just as the manifest naming its blob went away.
//
// It is the fallback, not the setting. `retention.blob_grace` is the operator
// key (default 24h, validated positive) and the composition root passes it as
// RetentionConfig.GraceWindow; this constant answers a zero window, which is
// what a collector built without configuration -- a test driving the boundary
// without sleeping -- hands in.
const defaultGraceWindow = 24 * time.Hour

// BlobStore is the grace protocol's store surface, declared here beside the
// implementation that drives it because these methods were designed with it
// (storage/sqlite/gc.go). Each is one bounded phase, and each restores rather
// than advances a blob whose reference reappeared.
type BlobStore interface {
	// QuarantineBlobs demotes up to limit 'ready' blobs that nothing
	// references, stamping each with now, and reports how many.
	QuarantineBlobs(ctx context.Context, now time.Time, limit int) (int64, error)
	// TrashBlobs rechecks quarantined blobs: still unreferenced moves to
	// 'trash', referenced again is restored to 'ready'. It takes no clock,
	// because the grace window is measured from the quarantine stamp and
	// rewriting that stamp would restart the window on every pass.
	TrashBlobs(ctx context.Context, limit int) (trashed, restored int64, err error)
	// KnownBlobs reports which of hashes the store still holds a blobs row
	// for, in any state, as a set: absence from the result means no row. It
	// is not a grace phase but the oracle the orphan sweep below asks, and it
	// lives on this interface because *sqlite.Store answers it.
	KnownBlobs(ctx context.Context, hashes []string) (map[string]struct{}, error)
	// CollectBlobs deletes up to limit blobs trashed at or before deadline
	// that a recheck in the deleting transaction still finds unreferenced,
	// returning their hashes so their CAS objects can go; a trashed blob
	// referenced again is restored instead.
	CollectBlobs(ctx context.Context, deadline time.Time, limit int) (deleted []string, restored int64, err error)
}

// ObjectStore removes a published CAS object by content hash. internal/snapshot
// owns the only derivation of an object's path (CAS.path, unexported), so this
// package must not rebuild <data>/cas/<hh>/<hash> itself -- that is the
// duplicate implementation the charter above forbids.
//
// Named INT seam: internal/snapshot needs `func (c *CAS) Remove(hash string)
// error` reusing c.path, and internal/app hands the CAS in as Options.Objects.
type ObjectStore interface {
	Remove(hash string) error
	// SweepOrphans removes published objects no blobs row names -- content a
	// rolled-back or crashed capture left behind, which Remove's grace
	// protocol never sees because it only ever had a file and never a row.
	// known is BlobStore.KnownBlobs, passed as a bare func so this package
	// states the seam without importing internal/snapshot; grace is the
	// resolved blob grace window, and batch bounds the sweep's working set,
	// not how much it reclaims.
	SweepOrphans(ctx context.Context, known func(context.Context, []string) (map[string]struct{}, error),
		now time.Time, grace time.Duration, batch int) (int64, error)
}

// grace runs one pass of the three phases in order and adds what it reclaimed
// to report. The phases are deliberately one pass each rather than a loop to
// exhaustion: a blob quarantined by this pass is trashed by the next one and
// deleted by the one after the grace window, so the protocol's delay is real
// and one collection can never run unbounded.
//
// The caller holds both locks named in the package comment; grace takes none.
func (c *Collector) grace(ctx context.Context, report Report) (Report, error) {
	now := c.opts.Now()
	limit := c.opts.Config.BatchLimit
	window := c.opts.Config.GraceWindow
	if window <= 0 {
		window = defaultGraceWindow
	}

	// Every count is added only after its phase reported success: a phase is
	// one transaction, and a commit that fails returns the number of rows the
	// statement matched for work that then rolled back. Report is what the
	// pass actually reclaimed, so a failed phase contributes nothing while the
	// phases that did commit keep their counts.
	quarantined, err := c.opts.Blobs.QuarantineBlobs(ctx, now, limit)
	if err != nil {
		return report, err
	}
	report.BlobsQuarantined += quarantined

	trashed, restored, err := c.opts.Blobs.TrashBlobs(ctx, limit)
	if err != nil {
		return report, err
	}
	report.BlobsTrashed += trashed
	report.BlobsRestored += restored

	deleted, restored, err := c.opts.Blobs.CollectBlobs(ctx, now.Add(-window), limit)
	if err != nil {
		return report, err
	}
	report.BlobsRestored += restored
	report.BlobsDeleted += int64(len(deleted))

	// The rows are already gone, so every object here is unreachable through
	// the store. An object that cannot be removed is reported with the rest
	// rather than retried or hidden: it is a leftover file, not lost source,
	// and the next capture of the same content republishes over it.
	var errs []error
	for _, hash := range deleted {
		if err := c.opts.Objects.Remove(hash); err != nil {
			errs = append(errs, err)
		}
	}

	// The objects the phases above never see: content published to the CAS by a
	// capture whose naming commit never landed. The grace protocol cannot reach
	// them because it walks rows, and these files have none. The same window
	// guards them -- an object younger than it may be a publication whose
	// commit is still in flight -- and the sweep re-walks every bucket each
	// pass, so the batch is a working-set size and not a cap on the reclaim.
	//
	// It runs on a CADENCE, not on every pass. The sweep is the one phase whose
	// cost is the size of the whole store rather than the size of the change:
	// it walks all 256 buckets and every object in them, under the workspace
	// lock and the indexing mutex, while a collection pass runs after EVERY
	// activation -- so an incremental refresh of one file paid for a full CAS
	// walk. Nothing is given up by waiting: an orphan only appears after a
	// rolled-back or crashed capture, and the grace window already holds every
	// orphan that long before this phase may touch it, so a sweep once per
	// window reclaims exactly the same objects a per-pass sweep did. The
	// cadence is a schedule, never a cap -- when the sweep runs it still walks
	// the whole store and reclaims everything it finds.
	due, stamp, err := c.orphanSweepDue(now, window)
	if err != nil {
		errs = append(errs, err)
	}
	if due {
		swept, err := c.opts.Objects.SweepOrphans(ctx, c.opts.Blobs.KnownBlobs, now, window, limit)
		report.OrphanObjectsSwept += swept
		if err != nil {
			errs = append(errs, err)
		}
		if err := writeStamp(stamp, now); err != nil {
			errs = append(errs, err)
		}
	}
	return report, errors.Join(errs...)
}

// orphanSweepPath is the cadence stamp's name under the data directory. It sits
// beside the CAS and the staging root rather than inside either: snapshot.Sweep
// reclaims <data>/staging and <data>/cas/tmp by prefix and would not recognize
// a file of this package's, and the CAS buckets are exactly what SweepOrphans
// walks.
const orphanSweepPath = "last-orphan-sweep"

// orphanSweepDue reports whether the CAS orphan sweep is due this pass, and the
// path of the stamp to rewrite when it has run.
//
// The direction of every uncertainty is "sweep": a missing stamp (the first
// pass ever, or a data directory an operator cleaned), an unreadable one, a
// malformed one, and a stamp dated in the FUTURE -- which a clock stepped
// backwards produces and which a plain "now - stamp < window" test would read
// as "not due" for an unbounded time -- all run the sweep and rewrite the
// stamp. The failure mode of sweeping too often is the cost this cadence
// exists to cut; the failure mode of never sweeping is disk that is never
// reclaimed, so the gate never fails closed.
func (c *Collector) orphanSweepDue(now time.Time, window time.Duration) (bool, string, error) {
	stamp := filepath.Join(c.opts.Config.DataDir, orphanSweepPath)
	raw, err := os.ReadFile(stamp)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return true, stamp, nil
		}
		// Reported, not swallowed: an operator whose data directory stopped
		// being readable should see it, and the pass still sweeps.
		return true, stamp, ioError("orphan sweep stamp", err)
	}
	last, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(string(raw)))
	if err != nil {
		return true, stamp, nil
	}
	elapsed := now.Sub(last)
	return elapsed < 0 || elapsed >= window, stamp, nil
}

// writeStamp records when the orphan sweep last ran. A torn or truncated write
// is self-healing: orphanSweepDue reads anything it cannot parse as "due", so
// the worst outcome of a failed write is the per-pass frequency this cadence
// replaced.
func writeStamp(path string, now time.Time) error {
	if err := os.WriteFile(path, []byte(now.UTC().Format(time.RFC3339Nano)), 0o644); err != nil {
		return ioError("orphan sweep stamp", err)
	}
	return nil
}

// ioError is this package's one filesystem-failure mapping: every error leaving
// the collector is a *model.Error carrying a diagnostic code, and the path text
// is never part of the message -- a data-directory path is not something an
// error may carry into a log.
func ioError(op string, err error) error {
	var typed *model.Error
	if errors.As(err, &typed) {
		return err
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return model.Canceled(err)
	}
	if errors.Is(err, syscall.ENOSPC) {
		return &model.Error{Code: model.CodeDiskFull, Message: op + ": the data directory's disk is full",
			Remediation: "free disk space or move storage.data_dir to a larger volume"}
	}
	return &model.Error{Code: model.CodeInternal, Message: op + " could not be recorded"}
}
