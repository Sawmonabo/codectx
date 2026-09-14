// Package delta builds one unit as "the previous unit plus a delta"
// (Section 11.4): it runs a provider that can tell what changed since its last
// run, names everything of the predecessor that did not survive, and lets
// storage carry the rest into the new unit.
//
// The two wave-A importers produce deltas at two granularities — one SCIP
// document, one dependence fact — and Applier is the one interface behind
// both. What each applier does is the same sequence, and only the middle of it
// differs:
//
//  1. read the predecessor's delta state (Store.DeltaState under Kind);
//  2. open the new unit and run the provider's delta-aware import;
//  3. with the writer solely owned, diff the fresh state against the stored
//     one, hand storage the replaced set and carry the rest over;
//  4. store the fresh delta state and seal.
//
// A predecessor that is absent, stored no state, or stored state this build
// cannot read is not an error: the unit is built in full and Result.Full says
// so. Everything else is a typed failure of the whole unit, which leaves the
// predecessor sealed, published and untouched — the new unit's rows, carried
// ones included, are its own copies and Fail deletes them.
package delta

import (
	"context"
	"os"
	"path/filepath"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider"
	"github.com/Sawmonabo/codectx/internal/storage/sqlite"
)

// Stats counts one delta in the applier's own granularity: SCIP documents for
// the SCIP applier, fact keys for the dependence applier.
type Stats struct{ Changed, Unchanged, Removed int64 }

// Request is one unit to build. It is what the coordinator has already
// computed: the staging generation, the predecessor to diff against, the unit
// identity and its inputs, the provider request, and a private directory this
// build may write to.
type Request struct {
	Generation model.GenerationID
	Previous   model.UnitID // zero: full build
	Build      model.UnitBuild
	Inputs     func(yield func(model.UnitInput) error) error
	Unit       provider.UnitRequest
	WorkDir    string
}

func (r Request) validate() error {
	if r.Inputs == nil {
		return invalid("a delta build needs the unit's inputs")
	}
	if !filepath.IsAbs(r.WorkDir) {
		return invalid("a delta build needs an absolute private work directory")
	}
	if r.Build.Spec.ID != r.Unit.Unit.ID {
		return invalid("the unit build and the provider request name different units")
	}
	// A refresh whose inputs did not move is the unit that already exists, not
	// a delta against it. Storage refuses it too, but only once the unit is
	// half built; the planner's mistake belongs at this boundary.
	if r.Previous != "" && r.Previous == r.Build.Spec.ID {
		return invalid("a delta build cannot carry over from the unit it is building")
	}
	return nil
}

// Result is one built unit: what the provider reported, what was inherited
// rather than re-imported, and the delta that decided it.
type Result struct {
	Unit    model.UnitID
	Result  model.ProviderResult
	Carried sqlite.CarryOverStats
	Delta   Stats
	Full    bool // predecessor absent or unusable
}

// Applier builds one unit of one provider as a delta against its predecessor.
type Applier interface {
	// Kind is the delta-state kind this applier reads and writes, which is
	// also how a stored artifact is bound to the code that can read it.
	Kind() string
	Apply(ctx context.Context, req Request) (Result, error)
}

// workDir is one build's private scratch directory: the predecessor's delta
// state is written back to disk here for the provider to read, and the fresh
// state is staged here before it is stored. It is per build rather than per
// applier because two units of one provider run concurrently.
func workDir(req Request) (string, error) {
	if err := os.MkdirAll(req.WorkDir, 0o700); err != nil {
		return "", internal("delta work directory: " + err.Error())
	}
	dir, err := os.MkdirTemp(req.WorkDir, "delta-")
	if err != nil {
		return "", internal("delta work directory: " + err.Error())
	}
	return dir, nil
}

// writeState spills a stored delta-state payload to a file the provider's own
// loader can open. Both loaders validate what they read, so a corrupt payload
// is refused there rather than trusted here.
func writeState(dir, name string, payload []byte) (string, error) {
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, payload, 0o600); err != nil {
		return "", internal("delta state: " + err.Error())
	}
	return p, nil
}

func invalid(msg string) *model.Error {
	return &model.Error{Code: model.CodeArgumentInvalid, Message: msg}
}

func internal(msg string) *model.Error {
	return &model.Error{Code: model.CodeInternal, Message: msg}
}
