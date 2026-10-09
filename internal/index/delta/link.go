package delta

import (
	"bufio"
	"context"
	"iter"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/paced"
	"github.com/Sawmonabo/codectx/internal/provider"
	"github.com/Sawmonabo/codectx/internal/provider/link"
	"github.com/Sawmonabo/codectx/internal/storage/sqlite"
)

// linkApplier builds one linking unit. A linking unit reads its members' link
// tables, not their bytes, so its delta is measured in member digests: each
// member input records the link.MemberDigest its structural unit stored
// beside its table (model.UnitInput.Digest), read without decoding the table.
//
// The unit's identity is still its inputs' content: the facts it publishes
// bind their evidence to each file's bytes and ranges, so a unit keyed on the
// digests alone would be reused with evidence about bytes that are gone. What
// the digests decide is what the rebuild costs. When the unit has its
// predecessor's members, each with its predecessor's digest, it hands Link the
// predecessor's resolution and the stream of members whose content changed:
// Link resolves nothing and republishes only those members' facts, and the
// rest of the predecessor is carried over with those members named replaced.
// That is the path an edit inside a function body takes: it republishes the
// edited file's facts and reads only the tables that republication needs.
// Any other build resolves in full, publishes every fact and carries nothing.
type linkApplier struct {
	store   *sqlite.Store
	p       *link.Provider
	digests link.Digests
	limits  provider.Limits
	pool    *provider.Pool
}

// NewLink binds the linking provider to the store its units are built in and
// to the member digests its members' structural units stored.
func NewLink(store *sqlite.Store, p *link.Provider, digests link.Digests, limits provider.Limits, pool *provider.Pool) Applier {
	return &linkApplier{store: store, p: p, digests: digests, limits: limits, pool: pool}
}

func (a *linkApplier) Kind() string { return link.ResolutionKind }

// Apply builds the unit. Result.Filtered reports the inheriting path: it is
// set from what this build handed Link, never from what Link reported.
// Result.Delta counts member inputs: Changed are those whose digest moved or
// that are new, Unchanged those whose digest held, Removed the predecessor's
// members this unit no longer has. Result.Carried is what the inheriting path
// carried over, and zero on the other.
func (a *linkApplier) Apply(ctx context.Context, req Request) (Result, error) {
	if a.store == nil || a.p == nil || a.digests == nil {
		return Result{}, invalid("the link delta applier needs a store, a provider and the stored digests")
	}
	if err := req.validate(); err != nil {
		return Result{}, err
	}
	dir, err := workDir(req)
	if err != nil {
		return Result{}, err
	}
	defer paced.RemoveAll(dir)

	inputs := req.Inputs
	j := &digestJoin{}
	req.Inputs = a.digested(ctx, req, j)
	b := &build{store: a.store, p: a.p, limits: a.limits, pool: a.pool, req: req}
	if req.Previous == "" {
		b.res.Full, b.res.FullReason = true, FullNoPredecessor
	}
	statePath := filepath.Join(dir, "resolution")
	// edited streams the files the predecessor declared that this unit does
	// not declare with the same bytes, in ascending FileID order, from the
	// merge join of the two units' stored inputs, walked afresh each time so
	// no list of them is held. On the inheriting path the members and their
	// digests are the predecessor's, so every file it names is one whose
	// content changed. It walks the coordinator's inputs, not the digested
	// stream, whose walks restart the digest join.
	edited := func(yield func(model.FileID) error) error {
		return replacedInputs(ctx, a.store, req.Previous, inputs, yield)
	}

	b.index = func(ctx context.Context, ureq provider.UnitRequest, sink provider.Sink) (model.ProviderResult, error) {
		// The members are the unit's inputs, already in FileID order, read
		// as the snapshot's file versions: Link keeps its members of them
		// instead of sorting the manifest.
		opts := link.LinkOptions{Dir: dir, Members: func(yield func(model.FileVersion) error) error {
			return inputs(func(in model.UnitInput) error {
				fv, err := a.store.SnapshotFile(ctx, req.Unit.Binding.SnapshotID, in.FileID)
				if err != nil {
					return err
				}
				return yield(fv)
			})
		}}
		if req.Previous != "" && j.same() {
			prev, reason, err := a.previous(ctx, req, dir)
			if err != nil {
				return model.ProviderResult{}, err
			}
			if prev == "" {
				b.res.Full, b.res.FullReason = true, reason
			} else if f, err := os.Open(prev); err != nil {
				return model.ProviderResult{}, internal("link resolution state: " + err.Error())
			} else {
				defer f.Close()
				opts.Previous, opts.Edited = f, edited
			}
		}
		b.res.Filtered = opts.Previous != nil
		// The state file is new in this build's fresh directory, so the open
		// creates it and has nothing to free.
		out, err := os.OpenFile(statePath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
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
		if b.res.Filtered {
			// The edited members' facts were republished; everything else of
			// the predecessor is carried. Nothing is index-level: every fact
			// the linker publishes is located in a member.
			var filesErr error
			replaced := sqlite.Replaced{Files: func(yield func(model.FileID) bool) {
				filesErr = edited(func(f model.FileID) error {
					if !yield(f) {
						return errStopInputs
					}
					return nil
				})
			}}
			carried, err := w.CarryOver(ctx, req.Previous, replaced)
			if filesErr != nil {
				return filesErr
			}
			if err != nil {
				return err
			}
			b.res.Carried = carried
		}
		return putState(ctx, w, link.ResolutionKind, statePath)
	}
	return b.run(ctx)
}

// previous spills the predecessor's resolution state into dir and returns its
// path, or an empty path and the reason the unit is built in full: the
// predecessor stored none (FullNoState), or stored one this build cannot read
// (FullStateUnreadable), which is warned about and resolved in full rather
// than failing the unit on every build until a digest moves.
func (a *linkApplier) previous(ctx context.Context, req Request, dir string) (string, string, error) {
	path, err := previousState(ctx, a.store, dir, "previous-resolution", req.Previous, link.ResolutionKind)
	if err != nil {
		return "", "", err
	}
	if path == "" {
		return "", FullNoState, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return "", "", internal("link resolution state: " + err.Error())
	}
	defer f.Close()
	if err := link.ValidateState(f); err != nil {
		slog.Warn("a stored link resolution state could not be read; the unit is resolved in full",
			"component", component, "unit", string(req.Previous), "kind", link.ResolutionKind, "error", err)
		return "", FullStateUnreadable, nil
	}
	return path, "", nil
}

// digested is the unit's input stream with each input's Digest set: the
// member digest the structural unit the staging generation selects for the
// member stored, or empty for a member with none (a manifest, a language
// with no lowering, a file whose link pass failed). It is the stream BeginUnit
// stores, and the same walk merge-joins it against the predecessor's stored
// inputs into j, so each digest is one small read and no table is decoded
// here, and no list of members is held.
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
			if in.Digest, _, err = a.digests.Digest(ctx, req.Generation, fv); err != nil {
				return err
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
