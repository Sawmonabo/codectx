package delta

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider"
	"github.com/Sawmonabo/codectx/internal/provider/scip"
	"github.com/Sawmonabo/codectx/internal/storage/sqlite"
)

// KindSCIP is the delta-state kind of the SCIP document manifest.
const KindSCIP = "scip.document_manifest"

// component is the slog component of every entry this package emits.
const component = "index.delta"

// scipApplier builds one SCIP unit as a per-document delta. SCIP documents are
// independent and its global symbol strings are position-free, so a refreshed
// unit replaces only the documents whose canonical hash changed and inherits
// the rest.
type scipApplier struct {
	store  *sqlite.Store
	p      *scip.Provider
	limits provider.Limits
	pool   *provider.Pool
}

// NewSCIP binds the SCIP provider to the store its units are built in.
func NewSCIP(store *sqlite.Store, p *scip.Provider, limits provider.Limits, pool *provider.Pool) Applier {
	return &scipApplier{store: store, p: p, limits: limits, pool: pool}
}

func (a *scipApplier) Kind() string { return KindSCIP }

func (a *scipApplier) Apply(ctx context.Context, req Request) (Result, error) {
	if a.store == nil || a.p == nil {
		return Result{}, invalid("the scip delta applier needs a store and a provider")
	}
	if err := req.validate(); err != nil {
		return Result{}, err
	}
	dir, err := workDir(req)
	if err != nil {
		return Result{}, err
	}
	defer os.RemoveAll(dir)

	previous, err := a.previous(ctx, req, dir)
	if err != nil {
		return Result{}, err
	}

	b := &build{store: a.store, p: a.p, limits: a.limits, pool: a.pool, req: req}
	b.res.Full = previous == nil

	var rep scip.Report
	// The fresh manifest is a private temporary file this build owns on every
	// path, including the paths where the unit never reaches a seal.
	defer func() { _ = rep.Manifest.Close() }()

	b.index = func(ctx context.Context, ureq provider.UnitRequest, sink provider.Sink) (model.ProviderResult, error) {
		var err error
		rep, err = a.p.Import(ctx, ureq, sink, scip.ImportOptions{Previous: previous})
		return rep.Result, err
	}
	b.complete = func(ctx context.Context, w *sqlite.UnitWriter) error {
		if previous != nil {
			// Every non-unchanged path's bucket is replaced: its stored facts
			// are the ones this run re-imported, or the ones whose document
			// the fresh index no longer describes. The alias scope goes with
			// it, because a SCIP alias is scoped per document.
			//
			// Replaced.IndexLevel is never set. A delta run republishes the
			// index's external symbols only for the documents it reparsed, so
			// dropping the unit's index-level bucket would leave edges
			// carried from untouched documents without endpoints and the unit
			// could not seal.
			var replaced sqlite.Replaced
			repo := req.Unit.Binding.RepositoryID
			d, err := rep.Manifest.Diff(previous, func(c scip.Change) error {
				if c.Class == scip.ClassUnchanged {
					return nil
				}
				replaced.Files = append(replaced.Files, model.NewFileID(repo, c.Path))
				replaced.Scopes = append(replaced.Scopes, "file:"+c.Path)
				return nil
			})
			if err != nil {
				return err
			}
			b.res.Delta = Stats{Changed: d.Changed, Unchanged: d.Unchanged, Removed: d.Removed}
			if b.res.Carried, err = w.CarryOver(ctx, req.Previous, replaced); err != nil {
				return err
			}
		} else {
			b.res.Delta = Stats{Changed: rep.Delta.Changed, Unchanged: rep.Delta.Unchanged, Removed: rep.Delta.Removed}
		}
		fresh := filepath.Join(dir, "manifest")
		if err := rep.Manifest.Save(fresh); err != nil {
			return err
		}
		payload, err := os.ReadFile(fresh)
		if err != nil {
			return internal("scip document manifest: " + err.Error())
		}
		return w.PutDeltaState(ctx, KindSCIP, payload)
	}
	return b.run(ctx)
}

// previous loads the predecessor's stored document manifest. A unit that was
// never selected, one that stored no manifest, and one whose stored manifest
// this build cannot read all mean the same thing to the caller — build in full
// — because a full build is always correct and a refusal here would turn a
// recoverable state into a failed capability. The unreadable case is the only
// one that is not ordinary, so it is the only one that logs.
func (a *scipApplier) previous(ctx context.Context, req Request, dir string) (*scip.DocumentManifest, error) {
	if req.Previous == "" {
		return nil, nil
	}
	raw, err := a.store.DeltaState(ctx, req.Previous, KindSCIP)
	if err != nil {
		if isNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	name, err := writeState(dir, "previous-manifest", raw)
	if err != nil {
		return nil, err
	}
	m, err := scip.LoadDocumentManifest(name)
	if err != nil {
		slog.Warn("a stored scip document manifest could not be read; the unit is built in full",
			"component", component, "unit", string(req.Previous), "kind", KindSCIP, "error", err)
		return nil, nil
	}
	return m, nil
}

// isNotFound is the not-found contract with storage: a lookup that found no
// row is CTX_ARGUMENT_INVALID carrying sqlite.ReasonNotFound, which is what
// separates "there is no such state" from a malformed argument.
func isNotFound(err error) bool {
	var typed *model.Error
	return errors.As(err, &typed) && typed.Code == model.CodeArgumentInvalid &&
		typed.Details["reason"] == sqlite.ReasonNotFound
}
