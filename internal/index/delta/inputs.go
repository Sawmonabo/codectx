package delta

import "github.com/Sawmonabo/codectx/internal/model"

// A merge join of a predecessor's declared inputs against a unit's own is
// streamed from both ends and never held: a unit declares every source file
// of its project and Section 6 forbids retaining that list in the heap.
// Store.UnitInputs pages the predecessor's unit_inputs in ascending FileID
// order -- the order BeginUnit required when they were written -- and the
// unit's own inputs arrive in the same order, so the join is one pass with
// two cursors.
//
// The two orders agree because FileID is a lowercase hex digest and
// unit_inputs.file_id is its BLOB decoding: hex ordering and memcmp ordering
// of the same bytes are the same ordering. nextInput asserts it anyway -- a
// mis-sorted stream turns the join into a silent mismatch.

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
		return model.UnitInput{}, false, invalid("%s inputs are not in ascending file id order", side)
	}
	*last = in.FileID
	return in, true, nil
}
