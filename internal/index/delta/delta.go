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
// cannot read is not an error: the unit is built in full and Result.Full and
// Result.FullReason say so. That degradation covers provider-written state
// only — the SCIP document manifest and the dependence key set. A failure to
// read what storage itself holds (the predecessor's declared inputs) is not a
// recoverable state but a broken store, and fails the unit like any other
// storage error. Everything else is likewise a typed failure of the whole
// unit, which leaves the predecessor sealed, published and untouched — the new
// unit's rows, carried ones included, are its own copies and Fail deletes
// them.
package delta

import (
	"context"
	"fmt"
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
	// Inputs yields the unit's declared inputs in ascending FileID order. It
	// must be re-iterable — every walk must yield the same inputs: storage
	// streams it to open the unit, and the dependence applier walks it again
	// (twice) to merge-join it against the predecessor's stored inputs rather
	// than keeping the file list.
	//
	// The contract as a whole is not enforceable — the fresh stream is never
	// fully drained on the later walks, so no fold over it could be compared
	// — but a drifting stream is not simply trusted either. Storage folds
	// model.NewUnitInputHasher over walk 1 and refuses a mismatch against
	// Build.Spec.InputHash, and the dependence applier refuses the one
	// disagreement between the later walks that would corrupt the unit: a
	// filtered emit chosen because walk 2 saw nothing replaced, while walk 3
	// names a replaced file whose bucket that emit never republishes (see
	// dependence.go). What is left unchecked is drift that costs import work
	// rather than rows.
	Inputs  func(yield func(model.UnitInput) error) error
	Unit    provider.UnitRequest
	WorkDir string
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

// Reasons a unit was built in full. Only FullNoPredecessor, FullNoState and
// FullStateUnreadable mean the previous unit could not be inherited from;
// FullSubdivided is a healthy predecessor this run cannot describe itself
// against, so a coordinator must not count it as an invalidation.
const (
	// FullNoPredecessor: the request named none (a first build, or a scope
	// the previous generation did not select).
	FullNoPredecessor = "no_predecessor"
	// FullNoState: the predecessor stored no delta state of this kind, or
	// stored one with nothing to diff against.
	FullNoState = "no_previous_state"
	// FullStateUnreadable: the predecessor's stored state exists and this
	// build cannot read it.
	FullStateUnreadable = "unreadable_previous_state"
	// FullSubdivided: the provider subdivided the unit for memory, so this
	// run publishes no whole-unit state a later refresh could diff against.
	FullSubdivided = "subdivided"
)

// Result is one built unit: what the provider reported, what was inherited
// rather than re-imported, and the delta that decided it.
type Result struct {
	Unit    model.UnitID
	Result  model.ProviderResult
	Carried sqlite.CarryOverStats
	Delta   Stats
	// Full says the unit was built without inheriting anything, and
	// FullReason says which of the four cases above it was. A coordinator
	// projecting invalidation counts must read the reason: only a missing or
	// unusable predecessor invalidated a previous unit.
	Full       bool
	FullReason string
	// Filtered says the emit this build requested was a filtered one AND that
	// the filter held: the predecessor's own state was supplied to the import,
	// and no unit of that state was retired, so the import emitted only what
	// changed. Both conjuncts matter. The dependence applier sets it as
	// `supplied && Delta.Removed == 0` because only then did the import leave
	// the unchanged relations, and the facts that name no file, unpublished —
	// a supplied filter over a state that lost a key re-emits enough that the
	// unit is not a filtered one in the sense the replaced set is built on.
	//
	// It is a property of what the build passed and measured, never of what
	// the provider did with it: an importer that accepted the previous state
	// and re-emitted everything anyway would still land here, and nothing
	// checks it. The SCIP applier sets it from the request alone, before its
	// import runs. That is deliberate — the replaced-set decisions keyed on
	// it (Replaced.IndexLevel, and the dependence applier's re-iterability
	// refusal) must be sound against what this build chose, not against a
	// report from the producer they are guarding. A delta whose
	// Filtered is false re-imported everything and inherited nothing, even
	// though the predecessor was present and usable — for the dependence
	// applier that is every refresh that changed or removed a declared file.
	// Result.Carried being all zero does not distinguish that from a
	// predecessor with nothing to give.
	Filtered bool
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
// previousState streams the predecessor's artifact of kind into a file
// named name under dir and returns its path. A predecessor that stored none
// returns an empty path and no error.
func previousState(ctx context.Context, store *sqlite.Store, dir, name string, unit model.UnitID, kind string) (string, error) {
	p := filepath.Join(dir, name)
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return "", internal("delta state: " + err.Error())
	}
	err = store.DeltaState(ctx, unit, kind, f)
	if closeErr := f.Close(); err == nil && closeErr != nil {
		err = internal("delta state: " + closeErr.Error())
	}
	if err != nil {
		if isNotFound(err) {
			return "", nil
		}
		return "", err
	}
	return p, nil
}

// putState stores one of this unit's delta-state artifacts from the file it
// was produced in, streaming it so the artifact is never held whole.
func putState(ctx context.Context, w *sqlite.UnitWriter, kind, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return internal("delta state " + kind + ": " + err.Error())
	}
	defer f.Close()
	return w.PutDeltaState(ctx, kind, f)
}

func invalid(format string, args ...any) *model.Error {
	return &model.Error{Code: model.CodeArgumentInvalid, Message: fmt.Sprintf(format, args...)}
}

func internal(msg string) *model.Error {
	return &model.Error{Code: model.CodeInternal, Message: msg}
}
