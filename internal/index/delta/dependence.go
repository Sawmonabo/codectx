package delta

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider"
	"github.com/Sawmonabo/codectx/internal/provider/dependence"
	"github.com/Sawmonabo/codectx/internal/provider/dependence/neo4jcsv"
	"github.com/Sawmonabo/codectx/internal/storage/sqlite"
)

const (
	// KindDependenceKeys is the delta-state kind of the fact key set one
	// sealed dependence unit published.
	KindDependenceKeys = "dependence.fact_keys"
	// KindDependenceInputs is the delta-state kind of that same unit's input
	// manifest: the file identities and content hashes it declared.
	//
	// It exists because Replaced.Files is the complement storage checks a
	// carry-over against (checkCarriedInputs), and nothing else can produce
	// it. A dependence fact key is a digest of the fact, not of the file it
	// came from, so a changed key cannot be mapped back to a path; storage
	// holds the predecessor's unit_inputs rows but exports no reader for
	// them. Store.UnitInputs would make this kind unnecessary — see the lane
	// report.
	KindDependenceInputs = "dependence.unit_inputs"
)

// dependenceApplier builds one dependence unit as a per-fact delta. The
// engine renumbers its node ids on every run, so the unit of replacement is
// the id-independent fact key rather than a file or a document.
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

// prevState is a usable predecessor: both of its delta-state artifacts, read
// back and validated. Either one missing means a full build, because the key
// set alone cannot name a replaced bucket and the manifest alone cannot name a
// replaced fact.
type prevState struct {
	keys   neo4jcsv.KeySet
	inputs string // the predecessor's input manifest, spilled to this build's work dir
}

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
	defer os.RemoveAll(dir)

	prev, err := a.previous(ctx, req, dir)
	if err != nil {
		return Result{}, err
	}

	b := &build{store: a.store, p: a.p, limits: a.limits, pool: a.pool, req: req}
	b.res.Full = prev == nil

	// The fresh input manifest is written as storage streams the inputs it is
	// already reading, in the ascending file order BeginUnit requires, so the
	// merge join below never sorts and never holds the file list.
	freshInputs := filepath.Join(dir, "inputs")
	b.req.Inputs = teeInputs(freshInputs, req.Inputs)

	keysPath := filepath.Join(dir, "keys")
	var (
		rep           dependence.Report
		replacedFiles []model.FileID
		supplied      bool
	)

	b.index = func(ctx context.Context, ureq provider.UnitRequest, sink provider.Sink) (model.ProviderResult, error) {
		opts := dependence.ImportOptions{KeysPath: keysPath}
		if prev != nil {
			// The tee has run to completion inside BeginUnit, so both
			// manifests are closed and complete by the time this reads them.
			var err error
			if replacedFiles, err = diffInputs(prev.inputs, freshInputs); err != nil {
				return model.ProviderResult{}, err
			}
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
			// the predecessor, so replacedFiles is empty and the delta is
			// still worth having.
			if len(replacedFiles) == 0 {
				opts.PreviousKeys = prev.keys
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
			// nothing this one could have carried.
			b.res.Full = true
			return nil
		}
		if prev != nil {
			// The delta is measured here, against the predecessor's own key
			// set, so it is the real delta even on the runs where the import
			// was deliberately given no previous keys.
			d, err := rep.Keys.Diff(prev.keys, nil)
			if err != nil {
				return err
			}
			b.res.Delta = Stats{Changed: int64(d.Changed), Unchanged: int64(d.Unchanged), Removed: int64(d.Removed)}

			// filtered mirrors neo4jcsv.deltaFilter over what this build
			// actually passed: only then did the import leave the unchanged
			// relations, and the facts that name no file, unpublished.
			filtered := supplied && d.Removed == 0
			replaced := sqlite.Replaced{
				Files: replacedFiles,
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
				IndexLevel: !filtered,
			}
			// Replaced.Keys is the complement: every key the fresh set added
			// and every key the predecessor held and it does not. It is
			// streamed straight out of the same merge pass, consumed once,
			// and the error is checked after storage has finished with it.
			var diffErr error
			replaced.Keys = func(yield func(string) bool) {
				_, diffErr = rep.Keys.Diff(prev.keys, func(key string, removed bool) error {
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
			if b.res.Carried, err = w.CarryOver(ctx, req.Previous, replaced); err != nil {
				return err
			}
			if diffErr != nil {
				return diffErr
			}
		} else {
			b.res.Delta = Stats{Changed: int64(rep.Delta.Changed),
				Unchanged: int64(rep.Delta.Unchanged), Removed: int64(rep.Delta.Removed)}
		}
		if err := putState(ctx, w, KindDependenceKeys, rep.Keys.Path()); err != nil {
			return err
		}
		return putState(ctx, w, KindDependenceInputs, freshInputs)
	}
	return b.run(ctx)
}

// previous loads the predecessor's key set and input manifest. Anything short
// of both — no predecessor, no stored state, state this build cannot read —
// means a full build, because a full build is always correct and refusing here
// would turn a recoverable state into a failed capability.
func (a *dependenceApplier) previous(ctx context.Context, req Request, dir string) (*prevState, error) {
	if req.Previous == "" {
		return nil, nil
	}
	rawKeys, err := a.state(ctx, req.Previous, KindDependenceKeys)
	if err != nil || rawKeys == nil {
		return nil, err
	}
	rawInputs, err := a.state(ctx, req.Previous, KindDependenceInputs)
	if err != nil || rawInputs == nil {
		return nil, err
	}
	keysFile, err := writeState(dir, "previous-keys", rawKeys)
	if err != nil {
		return nil, err
	}
	inputsFile, err := writeState(dir, "previous-inputs", rawInputs)
	if err != nil {
		return nil, err
	}
	keys, err := neo4jcsv.LoadKeySet(keysFile)
	if err != nil {
		slog.Warn("a stored dependence fact key set could not be read; the unit is built in full",
			"component", component, "unit", string(req.Previous), "kind", KindDependenceKeys, "error", err)
		return nil, nil
	}
	// A predecessor that published no key stored no fact_keys row either — a
	// unit with no source seals honestly that way — and CarryOver refuses a
	// replaced-key set against a unit that stored none. There is also nothing
	// to inherit. Both make it an unusable predecessor rather than a failure.
	if keys.Count() == 0 {
		return nil, nil
	}
	// The manifest is opened now rather than at the diff, so an unreadable one
	// degrades to a full build like the key set does instead of failing a unit
	// that is already half built.
	rows, err := openInputRows(inputsFile)
	if err != nil {
		slog.Warn("a stored dependence unit input manifest could not be read; the unit is built in full",
			"component", component, "unit", string(req.Previous), "kind", KindDependenceInputs, "error", err)
		return nil, nil
	}
	rows.close()
	return &prevState{keys: keys, inputs: inputsFile}, nil
}

// state reads one delta-state payload, reporting a missing one as nil.
func (a *dependenceApplier) state(ctx context.Context, unit model.UnitID, kind string) ([]byte, error) {
	raw, err := a.store.DeltaState(ctx, unit, kind)
	if err != nil {
		if isNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return raw, nil
}

// putState stores one of this unit's delta-state artifacts from the file it
// was produced in.
func putState(ctx context.Context, w *sqlite.UnitWriter, kind, path string) error {
	payload, err := os.ReadFile(path)
	if err != nil {
		return internal("delta state " + kind + ": " + err.Error())
	}
	return w.PutDeltaState(ctx, kind, payload)
}
