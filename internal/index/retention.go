package index

import (
	"context"

	"github.com/Sawmonabo/codectx/internal/provider"
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

// retain sweeps the store to the configured policy. A retention failure never
// fails the indexing run that just published: the generation is active, its
// facts are correct, and the only consequence is disk that will be reclaimed
// on the next pass. It is logged with its diagnostic code so it cannot be
// silent.
func (c *Coordinator) retain(ctx context.Context) {
	policy := sqlite.RetentionPolicy{RetainRefs: c.opts.Config.Index.RetainRefs,
		MaxRetainedBytes: c.opts.Config.Index.MaxRetainedBytes}
	// The sweep must finish even when the caller's context is already ending:
	// a half-swept store is the one state retention must not leave behind.
	report, err := c.opts.Store.RetainByRef(context.WithoutCancel(ctx), c.repo, policy, c.now())
	if err != nil {
		c.log.Warn("retention could not sweep the store", "component", component,
			"repository_id", string(c.repo), "diagnostic_code", provider.CodeOf(err))
		return
	}
	c.log.Info("retention swept the store", "component", component, "repository_id", string(c.repo),
		"refs_retained", report.RefsRetained, "generations_swept", report.GenerationsSwept,
		"units_deleted", report.UnitsDeleted, "bytes_reclaimed", report.BytesReclaimed,
		"bytes_reclaimable", report.BytesReclaimable)
}
