package delta

import (
	"bufio"
	"context"
	"errors"
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
// the digests decide is what the rebuild costs. Whenever the predecessor's
// resolution state is readable, the applier hands it to Link with the stored
// digests: Link classifies the members against it, finds the facts again
// only when a table digest, the membership or a manifest field the linker
// reads moved, and republishes only the members whose facts changed. Those
// members and the predecessor's members that are gone are named replaced,
// and so is the predecessor's index-level bucket (the external nodes, which
// Link publishes again on every run); the rest of the predecessor is carried
// over. A body edit that keeps its table's digest republishes its own file
// and reads no other table; one that moves it republishes its own file and
// the files whose facts or targeted nodes moved. Without a readable
// predecessor state the unit is resolved and published in full and nothing
// is carried.
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
// that are new, and an input with no digest on either side (a manifest) whose
// bytes moved; Unchanged the others; Removed the predecessor's inputs this
// unit no longer has. The counts describe the inputs, not the work: a
// go.mod whose version line moved is Changed and moves no binding, since
// Link compares the manifest fields it reads. Result.Carried is what the
// inheriting path carried over, and zero on the other.
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
	// replacedPath holds, one per line in ascending order, the FileIDs Link
	// named replaced (LinkOptions.Replaced): the members it republished and
	// the predecessor's members that are gone.
	replacedPath := filepath.Join(dir, "replaced")

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
		if req.Previous != "" {
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
				opts.Previous, opts.Digests = f, a.digests
			}
		}
		b.res.Filtered = opts.Previous != nil
		var replacedOut *os.File
		var replacedW *bufio.Writer
		if b.res.Filtered {
			f, err := os.OpenFile(replacedPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
			if err != nil {
				return model.ProviderResult{}, internal("link replaced files: " + err.Error())
			}
			replacedOut = f
			replacedW = bufio.NewWriter(paced.NewWriter(replacedOut))
			opts.Replaced = func(id model.FileID) error {
				_, err := replacedW.WriteString(string(id) + "\n")
				return err
			}
		}
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
		if replacedOut != nil {
			if flushErr := replacedW.Flush(); err == nil && flushErr != nil {
				err = internal("link replaced files: " + flushErr.Error())
			}
			if closeErr := replacedOut.Close(); err == nil && closeErr != nil {
				err = internal("link replaced files: " + closeErr.Error())
			}
		}
		return res, err
	}

	b.complete = func(ctx context.Context, w *sqlite.UnitWriter) error {
		b.res.Delta = j.stats
		if b.res.Filtered {
			// The files Link named replaced were published again or are
			// gone; everything else of the predecessor located in a member
			// is carried. The index-level bucket is replaced: it holds only
			// the external nodes, which no member locates and Link
			// publishes on every run for every fact of the unit, carried or
			// fresh, that targets one, so a node no fact targets any more is
			// not carried without evidence.
			var filesErr error
			replaced := sqlite.Replaced{IndexLevel: true, Files: func(yield func(model.FileID) bool) {
				filesErr = replacedFiles(replacedPath, yield)
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
// predecessor stored none (FullNoState), stored one another version of the
// linker wrote (FullStateVersion), or stored one this build cannot read
// (FullStateUnreadable), which is warned about and resolved in full rather
// than failing the unit on every build.
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
	if err := link.ValidateState(f); errors.Is(err, link.ErrStateVersion) {
		return "", FullStateVersion, nil
	} else if err != nil {
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
// predecessor's stored ones, both in ascending FileID order (see inputs.go),
// which counts Result.Delta. With no predecessor next is nil and every input
// counts as changed.
type digestJoin struct {
	next  func() (model.UnitInput, error, bool)
	last  model.FileID
	old   model.UnitInput
	has   bool
	stats Stats
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
// an equal FileID compares digests, and anything else is an addition. An
// input with no digest on either side carries no table (a manifest such as
// go.mod or package.json, or a member whose link pass failed) and is counted
// by its bytes.
func (j *digestJoin) match(in model.UnitInput) error {
	for j.has && j.old.FileID < in.FileID {
		j.stats.Removed++
		if err := j.pull(); err != nil {
			return err
		}
	}
	if j.has && j.old.FileID == in.FileID {
		untabled := j.old.Digest == "" && in.Digest == ""
		if j.old.Digest == in.Digest && (!untabled || j.old.ContentHash == in.ContentHash) {
			j.stats.Unchanged++
		} else {
			j.stats.Changed++
		}
		return j.pull()
	}
	j.stats.Changed++
	return nil
}

// finish counts the predecessor's members past the last fresh input.
func (j *digestJoin) finish() error {
	for j.has {
		j.stats.Removed++
		if err := j.pull(); err != nil {
			return err
		}
	}
	return nil
}

// replacedFiles yields the FileIDs the file at path lists, one per line, until
// yield stops.
func replacedFiles(path string, yield func(model.FileID) bool) error {
	f, err := os.Open(path)
	if err != nil {
		return internal("link replaced files: " + err.Error())
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if !yield(model.FileID(sc.Text())) {
			return nil
		}
	}
	if err := sc.Err(); err != nil {
		return internal("link replaced files: " + err.Error())
	}
	return nil
}
