package scip

import (
	"context"

	"github.com/Sawmonabo/codectx/internal/admission"
	"github.com/Sawmonabo/codectx/internal/model"
)

// ProfileAdmission is a profile scope's admission step over the snapshot it
// indexes, and false for a key that is not a profile scope or names a kind
// this build does not know: that key is a supplied index, which runs no
// indexer and reserves nothing.
//
// The step plans the run's memory and temporary disk from its need (ADR-0012
// decision 5, "External indexers and language servers"): the bytes of the
// snapshot files under the project directory the indexer is pointed at, times
// what this repository's runs of the same indexer payload and work-size class
// were measured to use and to write per byte, within the allocations the
// ledger holds for this admission; the disk also holds the snapshot copy the
// run stages. The first run of a class is reserved at the allocations and runs
// alone; the runs of its class wait for it and are planned from it. The run
// itself raises the holding to the resident set its tree is read at
// (runProfile).
func (p *Provider) ProfileAdmission(view model.SnapshotView, scopeKey string) (admission.Admit, bool) {
	name, root, isProfile := splitProfileScope(scopeKey)
	if !isProfile {
		return nil, false
	}
	k := Kind(name)
	spec, known := kindSpecs[k]
	if !known {
		return nil, false
	}
	key := admission.ChildKey{Kind: admission.ChildIndexer, Tool: name, Fingerprint: p.identities[k]}
	return func(ctx context.Context, l *admission.Ledger) (*admission.Holding, error) {
		// The run copies the whole snapshot, within the operator's
		// materialization bound, before the indexer starts.
		work, err := admission.ProjectWork(ctx, view, root, spec.triggers, p.limits.MaxMaterializeBytes.Value())
		if err != nil {
			return nil, err
		}
		return l.HoldChild(ctx, &p.needs, key, work, nil)
	}, true
}
