package index

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/Sawmonabo/codectx/internal/model"
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
	// a half-swept store is the one state retention must not leave behind.
	report, err := c.opts.Store.RetainByRef(context.WithoutCancel(ctx), c.repo, policy, c.now())
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
	if c.opts.Collector == nil {
		return
	}
	report, err := c.opts.Collector.Collect(context.WithoutCancel(ctx))
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
		"orphan_objects_swept", report.OrphanObjectsSwept)
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
