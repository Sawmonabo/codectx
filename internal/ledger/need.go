package ledger

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
)

// NeedKey names one learned need model within a repository: the language, the
// grammar fingerprint the files were parsed under, the build identity of the
// worker that parsed them, and the file-size class. A worker of a new build
// learns afresh: what it holds of a file's tree beside the parse -- the
// flattened array, the lowering's structures -- changes with the build, and a
// model learned under another would reserve for work the file no longer does.
type NeedKey struct {
	Language    string
	Fingerprint string
	Build       string
	SizeClass   int
}

// NeedObservation is one parsed file's measured need, with the model state
// after it was folded in. State is the encoded model, opaque to this package.
type NeedObservation struct {
	Key NeedKey
	// ReservedBytes is the per-file increment the file was admitted with, and
	// NeedBytes what the worker measured it to use above its base.
	ReservedBytes int64
	NeedBytes     int64
	State         []byte
}

// RepositoryID is the repository this run records, as the hex the ledger's
// readers take, and empty for a run that records nothing.
func (r *Run) RepositoryID() string {
	if r == nil {
		return ""
	}
	return hex.EncodeToString(r.repo)
}

// ObserveNeed records one file's observation. The model state it carries is
// written to the observation store in the same transaction as the
// observation, whatever becomes of the generation it was taken under.
//
// It crosses the bus like every other event, so it never waits: a full bus
// drops it and counts the drop on the run row, and the next observation's
// state, which folds in everything before it, supersedes what was lost. The
// state is copied, so the caller may reuse its buffer at once.
//
// An observation the store would refuse -- no language, no fingerprint, no
// build, no state, a negative class, or a negative figure, which no measurement is -- is
// counted as dropped too rather than published: written, it would fail the
// transaction it shares with every other event of the batch and lose them all.
// A nil or stopped run records nothing.
func (r *Run) ObserveNeed(o NeedObservation) {
	if r == nil {
		return
	}
	if o.Key.Language == "" || o.Key.Fingerprint == "" || o.Key.Build == "" || o.Key.SizeClass < 0 || len(o.State) == 0 ||
		o.NeedBytes < 0 || o.ReservedBytes < 0 {
		if !r.stopped.Load() {
			r.dropped.Add(1)
			r.dirty.Store(true)
		}
		return
	}
	o.State = bytes.Clone(o.State)
	r.c.publish(event{kind: eventNeed, run: r, need: &o})
}

// NeedModel is the persisted model state for key in this run's repository,
// and false when none is recorded. A nil run and a run whose collector has
// detached read nothing and answer false, not an error, exactly as they record
// nothing: the store is read through the run's own attachment.
//
// It first waits for the collector to write everything published before the
// call, so an observation still on the bus -- one a model dropped at the end
// of a stage published, or one a failed run left behind -- is in the answer,
// and a model loaded again never forgets it. It waits on the collector and
// reads through its one connection, so it is never called from a subscriber,
// which runs while a flush holds that connection.
func (r *Run) NeedModel(ctx context.Context, key NeedKey) ([]byte, bool, error) {
	if r == nil || r.detached() {
		return nil, false, nil
	}
	if err := r.c.flushed(ctx); err != nil {
		return nil, false, err
	}
	var state []byte
	err := r.c.db.QueryRowContext(ctx, `SELECT state FROM need_models
		WHERE repository_id = ? AND language = ? AND fingerprint = ? AND build = ? AND size_class = ?`,
		r.repo, key.Language, key.Fingerprint, key.Build, key.SizeClass).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		// A detach leaves the handle before it closes the database, so a read
		// that lost that race is a detached read.
		if r.detached() {
			return nil, false, nil
		}
		return nil, false, wrap("read the need model", err)
	}
	return state, true, nil
}

// detached reports whether the attachment this run belongs to is no longer
// the handle's current one: it has detached, or is detaching, and its
// database is closed or about to be.
func (r *Run) detached() bool { return r.c.l.current() != r.c }
