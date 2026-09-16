package index

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/Sawmonabo/codectx/internal/ledger"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/paced"
	"github.com/Sawmonabo/codectx/internal/provider"
	"github.com/Sawmonabo/codectx/internal/retention"
	"github.com/Sawmonabo/codectx/internal/storage/sqlite"
)

// Retention by distinct ref (Section 12.4, ruling Q10). Every activation
// records the ref it was built from, and retention keeps the last
// `retain_refs` refs the user actually indexed rather than the last N
// snapshots, which is what makes switching A -> B -> C -> A find A's units
// still on disk and reuse them without a run.
//
// It runs after activation and never before: collecting while a generation is
// staging would race the units it is about to attach, and the workspace lock
// the caller holds is what keeps a second process out of both.
//
// Within this process retention must also be taken under Coordinator.run, not
// only under the cross-process workspace lock. RetainByRef ends in a sweep with
// no snapshot filter, which deletes every snapshot no generation, lease or
// session references -- and a foreground indexing run has exactly such a
// window, between writing its capture and opening the generation that first
// references it. The indexing path is already inside that lock; the background
// publication takes it explicitly.

// retain sweeps the store to the configured policy. A retention failure never
// fails the indexing run that just published: the generation is active, its
// facts are correct, and the only consequence is disk that will be reclaimed
// on the next pass. It is logged with its diagnostic code AND recorded on the
// coordinator, so `status` reports it: a log line alone is not a reader, and a
// policy the store refuses would otherwise leave retention never sweeping with
// nothing in the product saying so while the store grows.
func (c *Coordinator) retain(ctx context.Context) {
	policy := sqlite.RetentionPolicy{RetainRefs: c.opts.Config.Index.RetainRefs.Int(),
		MaxRetainedBytes: c.opts.Config.Index.MaxRetainedBytes.Value()}
	// The sweep must finish even when the caller's context is already ending:
	// a half-swept store is the one state retention must not leave behind. The
	// span is opened on that same uncancellable context, or a run cancelled
	// while retention is sweeping would record no retention at all.
	ctx = context.WithoutCancel(ctx)
	ctx, span := ledger.Start(ctx, stageRetention, "")
	report, err := c.opts.Store.RetainByRef(ctx, c.repo, policy, c.now())
	span.AddOut(int64(report.GenerationsSwept))
	// A deleted generation's ledger rows go with it. The ledger keeps its own
	// file, so nothing else would ever collect them and the file would grow
	// for as long as the workspace is indexed.
	if delErr := c.opts.Ledger.DeleteRuns(ctx, report.GenerationsDeleted); delErr != nil {
		logTyped(c.log, "the swept generations' ledger rows could not be deleted", delErr,
			"component", component, "repository_id", string(c.repo))
	}
	span.End(endOutcome(err), ledger.Measured{}, err)
	if err != nil {
		logTyped(c.log, "retention could not sweep the store", err,
			"component", component, "repository_id", string(c.repo))
		c.retention.record(err)
		return
	}
	c.retention.clear()
	c.log.Info("retention swept the store", "component", component, "repository_id", string(c.repo),
		"refs_retained", report.RefsRetained, "generations_swept", report.GenerationsSwept,
		"units_deleted", report.UnitsDeleted, "bytes_reclaimed", report.BytesReclaimed,
		"bytes_reclaimable", report.BytesReclaimable)
}

// Collector is the process-level reclaim pass this coordinator schedules. It is
// the narrow shape of *retention.Collector: the collector owns the Section 10.4
// blob grace protocol and is the one caller of the five sweep helpers that had
// none, and the coordinator owns the only moment in the process at which both
// locks the pass requires are already held.
//
// It may be nil. A coordinator built without one indexes exactly as before and
// reclaims nothing on its own -- the composition root always supplies one, and
// only a coordinator assembled in a test goes without.
type Collector interface {
	Collect(ctx context.Context) (retention.Report, error)
}

// collect runs one process-level collection pass. It is called from the same
// post-activation points as retain and under the same two locks, for the same
// reason: the pass ends in sweeps with no snapshot filter, and the window
// between a capture being written and its generation referencing it is exactly
// what those locks close.
//
// Like retain it never fails the run that just published -- the generation is
// active and its facts are correct, and unreclaimed disk is reclaimed by the
// next pass -- and like retain it finishes even when the caller's context is
// already ending, because a half-collected store is the state this must not
// leave behind. Its failure is logged with its diagnostic code so it cannot be
// silent.
func (c *Coordinator) collect(ctx context.Context) {
	// The sweep runs before the store's own pass and outside the collection
	// span, on the uncancellable context: it is the ledger's half of the same
	// collection, it is what keeps the two classes of run no generation will
	// ever reach from accumulating, and a coordinator assembled without a
	// store-side collector still has a ledger to sweep.
	ctx = context.WithoutCancel(ctx)
	c.sweepLedger(ctx)
	if c.opts.Collector == nil {
		return
	}
	ctx, span := ledger.Start(ctx, stageCollection, "")
	report, err := c.opts.Collector.Collect(ctx)
	span.End(endOutcome(err), ledger.Measured{}, err)
	if err != nil {
		logTyped(c.log, "the collection pass did not finish", err,
			"component", component, "repository_id", string(c.repo))
		return
	}
	c.log.Info("collection pass finished", "component", component, "repository_id", string(c.repo),
		"sessions_expired", report.SessionsExpired, "sessions_pruned", report.SessionsPruned,
		"spool_bytes_swept", report.SpoolBytesSwept, "tools_collected", report.ToolsCollected,
		"blobs_quarantined", report.BlobsQuarantined, "blobs_trashed", report.BlobsTrashed,
		"blobs_deleted", report.BlobsDeleted, "blobs_restored", report.BlobsRestored,
		"orphan_objects_swept", report.OrphanObjectsSwept,
		"orphan_sweep", report.OrphanSweepPhrase())
}

// recordReclaim records what the paced reclaimer gave back to the filesystem
// while this run was open, read from the counter the reclaimer already keeps
// rather than measured a second time: the freeing is the run's own cost --
// every analyzer output, every materialization and every scratch surface the
// run removed goes back through it, a window at a time -- and nothing else in
// the ledger accounts for it.
//
// It is one span at the end of the run and not a bracket around it. The
// reclaimer is a process-wide goroutine that frees at its own pace for the
// whole life of the run, so a span holding the run's own wall would top every
// wall-sorted surface while measuring nothing; the fact here is a quantity,
// and items_out is where the ledger carries quantities.
//
// The figure is what THIS PROCESS freed while the run was open, not what this
// run's own removals cost: one reclaimer serves the process, and a removal a
// run queues may be freed after it ends, by the next run or by the next
// process to claim the same set.
func recordReclaim(ctx context.Context, freedBefore int64) {
	freed := paced.FreedBytes() - freedBefore
	_, span := ledger.Start(ctx, stageReclaim, "")
	span.End(ledger.OutcomeOK, ledger.Measured{ItemsOut: &freed, CPUUnattributed: ledger.CPUOverlapped}, nil)
}

// sweepLedger deletes the ledger rows no generation will ever collect, one
// bounded page per pass. Two classes of run have no other way out of the file:
// the overlay run a process opens for its language-server starts, which belongs
// to no generation and outlives nothing but its own process; and a run that
// ended without publishing anything -- the periodic tick that finds nothing to
// publish never attaches a generation, and the ticks do not stop. DeleteRuns is
// keyed by generation, so without this pass both grow for as long as the
// workspace is indexed.
//
// It is called from the collection pass because that is where the process
// already reclaims what nothing references, and because both sweeps skip a run
// whose writer is still live: the run this pass is part of, another process's
// overlay, and a tick in flight are all live by the same judgement a reader
// uses, so nothing here can delete a run a status surface has just called live.
//
// Like the passes around it, a failure never fails the run that published: the
// rows stay and the next pass takes them, and the diagnostic is logged so a
// file that is never being swept cannot be silent.
func (c *Coordinator) sweepLedger(ctx context.Context) {
	overlays, err := c.opts.Ledger.OverlayRuns(ctx)
	if err == nil {
		err = c.opts.Ledger.DeleteOverlayRuns(ctx, overlays)
	}
	if err == nil {
		err = c.opts.Ledger.DeleteRunsWithoutGeneration(ctx)
	}
	if err != nil {
		logTyped(c.log, "the ledger runs no generation will collect were not swept", err,
			"component", component, "repository_id", string(c.repo))
	}
}

// retentionState is what this coordinator knows about its own last retention
// sweep, and Status projects it -- the same shape watchState has, and for the
// same reason: retain may not fail the run that published, so the only way a
// failed sweep reaches a user is a reported degradation. A refused policy is
// the case that matters: it fails every pass identically, so the store grows
// without bound while every generation publishes successfully.
//
// It is mutex-guarded because retain runs from two places (the publish path
// and the late-seal tick) while Status reads from a caller's goroutine.
type retentionState struct {
	mu sync.Mutex
	// failure is the rendered warning of the last sweep that did not finish,
	// empty once a later sweep succeeded.
	failure string
}

// record keeps the diagnostic of a sweep that did not finish.
func (r *retentionState) record(err error) {
	diagnostic := string(provider.CodeOf(err))
	var typed *model.Error
	if errors.As(err, &typed) && typed.Message != "" {
		diagnostic = string(typed.Code) + ": " + typed.Message
	}
	note, _ := model.TruncateField(fmt.Sprintf("the last retention sweep did not finish (%s): the store keeps "+
		"every generation retention would have reclaimed until a later pass succeeds", diagnostic), model.MaxReasonBytes)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failure = note
}

// clear forgets a recorded failure once a sweep has finished.
func (r *retentionState) clear() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failure = ""
}

// project publishes the recorded failure as a status warning.
func (r *retentionState) project(st *model.IndexStatus) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failure != "" {
		st.Warnings = append(st.Warnings, r.failure)
	}
}
