package delta

import (
	"context"
	"errors"
	"iter"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/storage/sqlite"
)

// The replaced-file set of a dependence unit is a merge join of the
// predecessor's declared inputs against this unit's own, streamed from both
// ends and never held: a unit declares every source file of its project and
// Section 6 forbids retaining that list in the heap.
//
// Both sides are the rows storage already holds. Store.UnitInputs pages the
// predecessor's unit_inputs in ascending FileID order — the order BeginUnit
// required when they were written — and Request.Inputs yields this unit's in
// the same order, so the join is one pass with two cursors. A stored manifest
// of the same rows was the alternative and was deleted: it duplicated
// unit_inputs, doubled the unit's delta-state payload, and gave a
// row-corrupt copy of storage's own data the power to fail a half-built unit.
//
// The two orders agree because FileID is a lowercase hex digest and
// unit_inputs.file_id is its BLOB decoding: hex ordering and memcmp ordering
// of the same bytes are the same ordering. nextInput asserts it on both sides
// anyway — a mis-sorted stream turns this join into a silent mismatch, which
// names too few replaced buckets and carries a fact about source that moved.

// errStopInputs unwinds a merge join whose consumer stopped early.
var errStopInputs = errors.New("delta: replaced input stream stopped")

// replacedInputs calls fn with every file the predecessor declared that this
// unit does not declare with the same bytes: the changed ones and the ones
// that are gone. A file only the fresh stream holds is an addition and names
// nothing of the predecessor, so it is not in the set. fn may return
// errStopInputs to end the walk, which is not an error.
func replacedInputs(ctx context.Context, store *sqlite.Store, prev model.UnitID,
	fresh func(yield func(model.UnitInput) error) error, fn func(model.FileID) error) error {
	oldNext, oldStop := iter.Pull2(store.UnitInputs(ctx, prev))
	defer oldStop()
	freshNext, freshStop := iter.Pull2(inputSeq(fresh))
	defer freshStop()

	var lastOld, lastFresh model.FileID
	a, aok, err := nextInput(oldNext, &lastOld, "the previous unit's")
	if err != nil {
		return err
	}
	b, bok, err := nextInput(freshNext, &lastFresh, "this unit's")
	if err != nil {
		return err
	}
	for aok {
		switch {
		case !bok || a.FileID < b.FileID:
			// The predecessor declared it and this unit does not.
			if err = fn(a.FileID); err == nil {
				a, aok, err = nextInput(oldNext, &lastOld, "the previous unit's")
			}
		case b.FileID < a.FileID:
			b, bok, err = nextInput(freshNext, &lastFresh, "this unit's")
		default:
			if a.ContentHash != b.ContentHash || a.Executable != b.Executable {
				err = fn(a.FileID)
			}
			if err == nil {
				if a, aok, err = nextInput(oldNext, &lastOld, "the previous unit's"); err == nil {
					b, bok, err = nextInput(freshNext, &lastFresh, "this unit's")
				}
			}
		}
		if errors.Is(err, errStopInputs) {
			return nil
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// inputSeq adapts the coordinator's push-style input stream to the pull form
// the merge join needs. A failure of the stream itself is delivered as the
// sequence's error, so it cannot be mistaken for the end of the inputs.
func inputSeq(inputs func(yield func(model.UnitInput) error) error) iter.Seq2[model.UnitInput, error] {
	return func(yield func(model.UnitInput, error) bool) {
		err := inputs(func(in model.UnitInput) error {
			if !yield(in, nil) {
				return errStopInputs
			}
			return nil
		})
		if err != nil && !errors.Is(err, errStopInputs) {
			yield(model.UnitInput{}, err)
		}
	}
}

// nextInput pulls one input and enforces the ascending FileID order the join
// depends on.
func nextInput(next func() (model.UnitInput, error, bool), last *model.FileID, side string) (model.UnitInput, bool, error) {
	in, err, ok := next()
	if !ok {
		return model.UnitInput{}, false, nil
	}
	if err != nil {
		return model.UnitInput{}, false, err
	}
	if *last != "" && in.FileID <= *last {
		return model.UnitInput{}, false, invalid(side + " inputs are not in ascending file id order")
	}
	*last = in.FileID
	return in, true, nil
}
