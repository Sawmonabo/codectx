// This file is owned by Task 15 lane L4. It holds
// Section 15.4 slicing: deterministic packing order, component grouping and file-boundary splits that preserve full-file requirements.
//
// The shared contract it builds on (candidate, the ranking constants and the
// typed error constructors) is frozen in compiler.go and is not edited here.
package context

import (
	"context"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
)

// ---------------------------------------------------------------------------
// Streamed packing — C-STREAM pass P-H
// ---------------------------------------------------------------------------

// The four reasons packPlan drops a group with, as constants so the streamed
// packer and the whole-set one cannot drift apart in the text a stored
// exclusion carries.
const (
	dropSliceLimit = "the budget's slice limit was reached before this entry"
	dropFileLimit  = "the budget's file limit was reached before this entry"
	dropOversized  = "the file does not fit in one slice of this budget"
)

// packVerdict is one group's outcome on the wire between P-H and P-I. It is
// decisionRec -- the frozen record of the packer's keep/slice decision -- plus
// the two facts a STREAMED drop needs and an in-heap `packing` never did: the
// reason, which today lives in the `dropped` slice packPlan returns, and the
// group's rank, which orders both the drops (appendDrops walks groups in rank
// order) and each slice's entry ordinals (sliceOrdinals does too).
type packVerdict struct {
	Decision decisionRec `json:"d"`
	MinIndex int64       `json:"i"`
	Reason   string      `json:"r,omitempty"`
}

// lessVerdict groups verdicts by file, the key P-I merge-joins them on against
// the measured stream lessFileIndex already ordered by file. A join comparator;
// one file has exactly one verdict.
func lessVerdict(a, b packVerdict) int { return lessDecision(a.Decision, b.Decision) }

func sizeOfVerdict(r packVerdict) int64 {
	return sizeOfDecision(r.Decision) + int64(len(r.Reason)) + recordOverheadBytes
}

// packPlanStream is packPlan over a sorted run of groups: the same forward walk
// under the same rule, in the same order, emitting one verdict per group
// instead of building the two candidate-sized maps `packing` holds.
//
// The walk is already forward-only and its state is already O(1) -- files,
// slices, bytes, tokens and `full` -- so the streamed form differs from the
// in-heap one in exactly one way: a kept group's members are not marked here.
// P-I applies the verdict to them by merge-joining it back onto the measured
// stream, which is what removes `keep map[int]bool` from the heap.
func packPlanStream(ctx context.Context, groups *pagination.SortedRun[groupRec],
	b resolvedBudget, emit func(packVerdict) error) error {
	files := 0
	slices := 0
	var bytes, tokens int64
	// full records whether the packer has stopped opening slices, so every
	// later group is excluded for the same stated reason instead of being
	// silently skipped.
	full := false

	return groups.Each(func(g groupRec) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		drop := func(reason string) error {
			return emit(packVerdict{Decision: decisionRec{FileID: g.FileID}, MinIndex: g.MinIndex, Reason: reason})
		}
		if !g.Required {
			switch {
			case full:
				return drop(dropSliceLimit)
			case files >= b.MaxFiles:
				return drop(dropFileLimit)
			case g.Bytes > b.MaxBytes || g.Tokens > b.MaxTokens:
				return drop(dropOversized)
			}
		}
		if slices == 0 || bytes+g.Bytes > b.MaxBytes || tokens+g.Tokens > b.MaxTokens {
			if slices == b.MaxSlices {
				if g.Required {
					// checkRequiredFitsStream has already proved the required
					// set fits, so reaching a required group that does not is a
					// defect in that check, not a budget outcome to absorb.
					return &model.Error{Code: model.CodeInternal,
						Message: "a required file did not fit the slice budget the minimum-budget check accepted"}
				}
				full = true
				return drop(dropSliceLimit)
			}
			slices++
			bytes, tokens = 0, 0
		}
		bytes += g.Bytes
		tokens += g.Tokens
		files++
		return emit(packVerdict{
			Decision: decisionRec{FileID: g.FileID, Keep: true, SliceIndex: int32(slices - 1)},
			MinIndex: g.MinIndex,
		})
	})
}
