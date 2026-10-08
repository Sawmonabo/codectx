package delta

import (
	"bufio"
	"context"
	"iter"
	"os"
	"path/filepath"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/paced"
	"github.com/Sawmonabo/codectx/internal/provider"
	"github.com/Sawmonabo/codectx/internal/provider/link"
	"github.com/Sawmonabo/codectx/internal/storage/sqlite"
)

// linkApplier builds one linking unit. A linking unit reads its members' link
// tables, not their bytes, so its delta is measured in table digests: each
// member input records the digest of the table it linked
// (model.UnitInput.Digest), and a build whose members and every digest equal
// its predecessor's hands Link the predecessor's resolution instead of
// resolving. That is the path an edit inside a function body takes: the
// file's bytes moved, so the unit is rebuilt, and its table did not, so
// nothing is relinked.
//
// Nothing is carried over: Link publishes every `calls` fact on both paths,
// from the fresh tables' ranges and identities, so Result.Carried stays zero.
type linkApplier struct {
	store  *sqlite.Store
	p      *link.Provider
	tables link.Tables
	limits provider.Limits
	pool   *provider.Pool
}

// NewLink binds the linking provider to the store its units are built in and
// to the tables its members' structural units stored.
func NewLink(store *sqlite.Store, p *link.Provider, tables link.Tables, limits provider.Limits, pool *provider.Pool) Applier {
	return &linkApplier{store: store, p: p, tables: tables, limits: limits, pool: pool}
}

func (a *linkApplier) Kind() string { return link.ResolutionKind }

// Apply builds the unit. Result.Filtered reports the fast path: it is set from
// what this build handed Link, never from what Link reported. Result.Delta
// counts member inputs: Changed are those whose digest moved or that are new,
// Unchanged those whose digest held, Removed the predecessor's members this
// unit no longer has.
func (a *linkApplier) Apply(ctx context.Context, req Request) (Result, error) {
	if a.store == nil || a.p == nil || a.tables == nil {
		return Result{}, invalid("the link delta applier needs a store, a provider and the stored tables")
	}
	if err := req.validate(); err != nil {
		return Result{}, err
	}
	dir, err := workDir(req)
	if err != nil {
		return Result{}, err
	}
	defer paced.RemoveAll(dir)

	j := &digestJoin{}
	req.Inputs = a.digested(ctx, req, j)
	b := &build{store: a.store, p: a.p, limits: a.limits, pool: a.pool, req: req}
	if req.Previous == "" {
		b.res.Full, b.res.FullReason = true, FullNoPredecessor
	}
	statePath := filepath.Join(dir, "resolution")

	b.index = func(ctx context.Context, ureq provider.UnitRequest, sink provider.Sink) (model.ProviderResult, error) {
		opts := link.LinkOptions{Dir: dir}
		if req.Previous != "" && j.same() {
			prev, err := previousState(ctx, a.store, dir, "previous-resolution", req.Previous, link.ResolutionKind)
			if err != nil {
				return model.ProviderResult{}, err
			}
			if prev == "" {
				b.res.Full, b.res.FullReason = true, FullNoState
			} else {
				f, err := os.Open(prev)
				if err != nil {
					return model.ProviderResult{}, internal("link resolution state: " + err.Error())
				}
				defer f.Close()
				opts.Previous = f
			}
		}
		b.res.Filtered = opts.Previous != nil
		out, err := os.OpenFile(statePath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
		if err != nil {
			return model.ProviderResult{}, internal("link resolution state: " + err.Error())
		}
		w := bufio.NewWriter(paced.NewWriter(out))
		opts.State = w
		res, err := a.p.Link(ctx, ureq, sink, opts)
		if flushErr := w.Flush(); err == nil && flushErr != nil {
			err = internal("link resolution state: " + flushErr.Error())
		}
		if closeErr := out.Close(); err == nil && closeErr != nil {
			err = internal("link resolution state: " + closeErr.Error())
		}
		return res, err
	}

	b.complete = func(ctx context.Context, w *sqlite.UnitWriter) error {
		b.res.Delta = j.stats
		info, err := os.Stat(statePath)
		if err != nil {
			return internal("link resolution state: " + err.Error())
		}
		// A unit with nothing resolved wrote no state, and storage refuses an
		// empty artifact. Its successor then finds no state and resolves in
		// full, which for a unit with nothing to resolve costs nothing.
		if info.Size() == 0 {
			return nil
		}
		return putState(ctx, w, link.ResolutionKind, statePath)
	}
	return b.run(ctx)
}

// digested is the unit's input stream with each input's Digest set: the
// digest of the table the structural unit the staging generation selects for
// the member stored, or empty for a member with none (a manifest, a language
// with no lowering, a file whose link pass failed). It is the stream BeginUnit
// stores, and the same walk merge-joins it against the predecessor's stored
// inputs into j, so every table is read once for its digest here and once
// more by Link, and no list of members is held.
//
// It stays re-iterable: each walk yields the same inputs with the same
// digests, and each restarts the join.
func (a *linkApplier) digested(ctx context.Context, req Request, j *digestJoin) func(yield func(model.UnitInput) error) error {
	return func(yield func(model.UnitInput) error) error {
		*j = digestJoin{}
		if req.Previous != "" {
			next, stop := iter.Pull2(a.store.UnitInputs(ctx, req.Previous))
			defer stop()
			j.next = next
			if err := j.pull(); err != nil {
				return err
			}
		}
		err := req.Inputs(func(in model.UnitInput) error {
			fv, err := a.store.SnapshotFile(ctx, req.Unit.Binding.SnapshotID, in.FileID)
			if err != nil {
				return err
			}
			t, ok, err := a.tables.Table(ctx, req.Generation, fv)
			if err != nil {
				return err
			}
			if ok {
				if in.Digest, err = t.Digest(); err != nil {
					return internal("link table digest of " + fv.Path + ": " + err.Error())
				}
			}
			if err := j.match(in); err != nil {
				return err
			}
			return yield(in)
		})
		if err != nil {
			return err
		}
		return j.finish()
	}
}

// digestJoin is the merge join of a unit's digested inputs against its
// predecessor's stored ones, both in ascending FileID order (see inputs.go).
// With no predecessor next is nil and every input counts as changed.
type digestJoin struct {
	next     func() (model.UnitInput, error, bool)
	last     model.FileID
	old      model.UnitInput
	has      bool
	stats    Stats
	complete bool
}

func (j *digestJoin) pull() error {
	if j.next == nil {
		j.has = false
		return nil
	}
	in, ok, err := nextInput(j.next, &j.last, "the previous unit's")
	j.old, j.has = in, ok
	return err
}

// match accounts one fresh input: every predecessor member before it is gone,
// an equal FileID compares digests, and anything else is an addition.
func (j *digestJoin) match(in model.UnitInput) error {
	for j.has && j.old.FileID < in.FileID {
		j.stats.Removed++
		if err := j.pull(); err != nil {
			return err
		}
	}
	if j.has && j.old.FileID == in.FileID {
		if j.old.Digest == in.Digest {
			j.stats.Unchanged++
		} else {
			j.stats.Changed++
		}
		return j.pull()
	}
	j.stats.Changed++
	return nil
}

// finish counts the predecessor's members past the last fresh input and
// marks the join whole, which same requires: a walk that stopped early has
// compared only part of the unit.
func (j *digestJoin) finish() error {
	for j.has {
		j.stats.Removed++
		if err := j.pull(); err != nil {
			return err
		}
	}
	j.complete = true
	return nil
}

// same reports that the unit has its predecessor's members, each with its
// predecessor's digest.
func (j *digestJoin) same() bool {
	return j.complete && j.stats.Changed == 0 && j.stats.Removed == 0
}
