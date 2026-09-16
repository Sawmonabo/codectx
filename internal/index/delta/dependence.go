package delta

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/paced"
	"github.com/Sawmonabo/codectx/internal/provider"
	"github.com/Sawmonabo/codectx/internal/provider/dependence"
	"github.com/Sawmonabo/codectx/internal/provider/dependence/neo4jcsv"
	"github.com/Sawmonabo/codectx/internal/storage/sqlite"
)

// KindDependenceKeys is the delta-state kind of the fact key set one sealed
// dependence unit published. It is the only state this applier stores: the
// replaced-file set is a merge join of Store.UnitInputs against this unit's
// own inputs (see inputs.go), so the predecessor's declared files need no
// second copy.
const KindDependenceKeys = "dependence.fact_keys"

// dependenceApplier builds one dependence unit as a per-fact delta. The
// engine renumbers its node ids on every run, so the unit of replacement is
// the id-independent fact key rather than a file or a document.
//
// What it can save is asymmetric, and Result.Filtered is how a caller sees
// which side it landed on: a refresh that changes or removes any file the
// predecessor declared emits in full and inherits nothing, because naming an
// edited path in Replaced.Files drops rows a filtered emit would never
// republish (see Apply). Only an addition-only refresh keeps the filter and
// inherits rows.
type dependenceApplier struct {
	store  *sqlite.Store
	p      *dependence.Provider
	limits provider.Limits
	pool   *provider.Pool
}

// NewDependence binds the dependence provider to the store its units are built
// in.
func NewDependence(store *sqlite.Store, p *dependence.Provider, limits provider.Limits, pool *provider.Pool) Applier {
	return &dependenceApplier{store: store, p: p, limits: limits, pool: pool}
}

func (a *dependenceApplier) Kind() string { return KindDependenceKeys }

// errStopKeys unwinds the replaced-key stream when storage stops consuming it.
var errStopKeys = errors.New("delta: replaced key stream stopped")

func (a *dependenceApplier) Apply(ctx context.Context, req Request) (Result, error) {
	if a.store == nil || a.p == nil {
		return Result{}, invalid("the dependence delta applier needs a store and a provider")
	}
	if err := req.validate(); err != nil {
		return Result{}, err
	}
	dir, err := workDir(req)
	if err != nil {
		return Result{}, err
	}
	defer paced.RemoveAll(dir)

	prev, reason, err := a.previous(ctx, req, dir)
	if err != nil {
		return Result{}, err
	}

	b := &build{store: a.store, p: a.p, limits: a.limits, pool: a.pool, req: req}
	b.res.Full, b.res.FullReason = prev == nil, reason

	keysPath := filepath.Join(dir, "keys")
	var (
		rep         dependence.Report
		anyReplaced bool
		supplied    bool
	)

	b.index = func(ctx context.Context, ureq provider.UnitRequest, sink provider.Sink) (model.ProviderResult, error) {
		opts := dependence.ImportOptions{KeysPath: keysPath}
		if prev != nil {
			// THE INVARIANT: a filtered emit may only be requested when no
			// file the predecessor declared changed or went away, because
			// Replaced.Files drops buckets a filtered emit does not
			// republish.
			//
			// A filtered import emits only the relations whose keys are new.
			// Naming an edited path in Replaced.Files deletes the
			// predecessor's evidence in that path, and the filtered emit
			// never rewrites the relations whose keys did not move — so the
			// unit would be missing rows a full build holds. Withholding the
			// previous keys makes the import emit every relation, which is
			// always correct and costs import work only. A refresh that only
			// added files keeps its filter: an added path names nothing of
			// the predecessor, so the replaced set is empty and the delta is
			// still worth having.
			//
			// The question here is only whether that set is empty, so the
			// walk stops at the first member; the set itself is streamed
			// into Replaced.Files at carry-over.
			err := replacedInputs(ctx, a.store, req.Previous, req.Inputs, func(model.FileID) error {
				anyReplaced = true
				return errStopInputs
			})
			if err != nil {
				return model.ProviderResult{}, err
			}
			if !anyReplaced {
				opts.PreviousKeys = *prev
			}
			supplied = !opts.PreviousKeys.Empty()
		}
		var err error
		rep, err = a.p.Import(ctx, ureq, sink, opts)
		return rep.Result, err
	}

	b.complete = func(ctx context.Context, w *sqlite.UnitWriter) error {
		if rep.Keys.Empty() {
			// A unit the provider subdivided publishes no whole-unit key set,
			// so there is nothing a later refresh could diff against and
			// nothing this one could have carried. The predecessor was
			// healthy, which is why the reason is recorded rather than folded
			// into "unusable".
			b.res.Full, b.res.FullReason = true, FullSubdivided
			return nil
		}
		if prev != nil {
			// The delta is measured here, against the predecessor's own key
			// set, so it is the real delta even on the runs where the import
			// was deliberately given no previous keys.
			d, err := rep.Keys.Diff(*prev, nil)
			if err != nil {
				return err
			}
			b.res.Delta = Stats{Changed: int64(d.Changed), Unchanged: int64(d.Unchanged), Removed: int64(d.Removed)}

			// filtered mirrors neo4jcsv.deltaFilter over what this build
			// actually passed: only then did the import leave the unchanged
			// relations, and the facts that name no file, unpublished.
			b.res.Filtered = supplied && d.Removed == 0
			var filesErr error
			// replacedSeen counts what the walk below actually produced, so
			// the two walks of req.Inputs can be held against each other: see
			// the refusal after CarryOver returns.
			var replacedSeen int
			replaced := sqlite.Replaced{
				// The replaced files are streamed from the same merge join
				// the emptiness check above ran, walked a second time rather
				// than remembered: a refresh over a moved branch names every
				// file of the project and Section 6 forbids holding that
				// list.
				Files: func(yield func(model.FileID) bool) {
					filesErr = replacedInputs(ctx, a.store, req.Previous, req.Inputs, func(f model.FileID) error {
						replacedSeen++
						if !yield(f) {
							return errStopInputs
						}
						return nil
					})
				},
				// Scopes stays empty: a dependence alias is carried only
				// while the node fact it names is, and nodes and aliases are
				// emitted whole on every dependence import, so the alias rows
				// this would drop are the ones the fresh run has just
				// rewritten anyway. Dropping the unit's scope would instead
				// delete the aliases of every identity that survived.
				Scopes: nil,
				// The index-level bucket — the facts that name no file — is
				// dropped exactly when the emit was unfiltered and therefore
				// republished them. Under a filtered emit it must be kept, or
				// an unchanged relation whose only evidence is unlocated
				// loses it and the unit cannot seal.
				IndexLevel: !b.res.Filtered,
			}
			// Replaced.Keys is the complement: every key the fresh set added
			// and every key the predecessor held and it does not. It is
			// streamed straight out of the same merge pass, consumed once,
			// and the error is checked after storage has finished with it.
			var diffErr error
			replaced.Keys = func(yield func(string) bool) {
				_, diffErr = rep.Keys.Diff(*prev, func(key string, removed bool) error {
					if !yield(key) {
						return errStopKeys
					}
					return nil
				})
				if errors.Is(diffErr, errStopKeys) {
					diffErr = nil
				}
			}
			// CarryOver runs even when no key can be inherited: an all-zero
			// CarryOverStats is then the evidence that the unfiltered emit
			// really did republish everything, instead of an assumption about
			// the emitter that nothing checks.
			b.res.Carried, err = w.CarryOver(ctx, req.Previous, replaced)
			// The two stream errors are checked before the call's own: a
			// stream that failed mid-walk staged fewer replaced entries than
			// the applier named, so CarryOver's success would be a unit that
			// kept rows it was told to drop.
			if filesErr != nil {
				return filesErr
			}
			if diffErr != nil {
				return diffErr
			}
			// The decisive disagreement between the two walks of req.Inputs.
			// The filtered emit above was chosen because walk 2 found no
			// declared input replaced; this walk named some anyway. That
			// orientation is the corrupting one: Replaced.Files drops the
			// predecessor's evidence in a path the filtered emit never
			// republishes, so the unit would seal missing rows a full build
			// holds, under a capability that says fresh. Nothing downstream
			// can see it — the changed file IS named, so checkCarriedInputs
			// is satisfied, and dependence node facts are republished whole,
			// so SealUnit finds no dangling endpoint. Refusing here fails the
			// unit, and RunUnit's failure path deletes its rows.
			//
			// The opposite disagreement needs no check: if walk 2 saw a
			// replacement and this walk did not, the emit was unfiltered and
			// republished everything.
			if b.res.Filtered && replacedSeen != 0 {
				return invalid("this unit's input stream is not re-iterable: the filtered "+
					"emit was chosen because no declared input was replaced, but carry-over "+
					"named %d replaced file(s); Request.Inputs must yield the same inputs "+
					"on every walk", replacedSeen)
			}
			if err != nil {
				return err
			}
		} else {
			b.res.Delta = Stats{Changed: int64(rep.Delta.Changed),
				Unchanged: int64(rep.Delta.Unchanged), Removed: int64(rep.Delta.Removed)}
		}
		return putState(ctx, w, KindDependenceKeys, rep.Keys.Path())
	}
	return b.run(ctx)
}

// previous loads the predecessor's fact key set. No predecessor, no stored
// state, state this build cannot read and a stored set with no key in it all
// mean a full build, because a full build is always correct and refusing here
// would turn a recoverable state into a failed capability. The returned reason
// says which it was, so a coordinator can tell an invalidated predecessor from
// one that never existed.
func (a *dependenceApplier) previous(ctx context.Context, req Request, dir string) (*neo4jcsv.KeySet, string, error) {
	if req.Previous == "" {
		return nil, FullNoPredecessor, nil
	}
	keysFile, err := previousState(ctx, a.store, dir, "previous-keys", req.Previous, KindDependenceKeys)
	if err != nil {
		return nil, "", err
	}
	if keysFile == "" {
		return nil, FullNoState, nil
	}
	keys, err := neo4jcsv.LoadKeySet(keysFile)
	if err != nil {
		slog.Warn("a stored dependence fact key set could not be read; the unit is built in full",
			"component", component, "unit", string(req.Previous), "kind", KindDependenceKeys, "error", err)
		return nil, FullStateUnreadable, nil
	}
	// A predecessor that published no key stored no fact_keys row either — a
	// unit with no source seals honestly that way — and CarryOver refuses a
	// replaced-key set against a unit that stored none. There is also nothing
	// to inherit. Both make it an unusable predecessor rather than a failure.
	if keys.Count() == 0 {
		return nil, FullNoState, nil
	}
	return &keys, "", nil
}
