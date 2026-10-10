// This file holds Section 15.4 slicing: deterministic packing order, component grouping and file-boundary splits that preserve full-file requirements.
//
// The shared contract it builds on (candidate, the ranking constants and the
// typed error constructors) is defined in compiler.go.
package context

import (
	"context"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
)

// ---------------------------------------------------------------------------
// Streamed packing — pass P-H
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

// packOutcome is what the round decision reads from one round's pack
// (ADR-0016 decisions 4 and 5): the files it kept, the rank key of the first
// group it dropped for the file limit or the slice limit, and how many of the
// read's witness files it dropped as oversized.
//
// It is read from P-H's verdict stream and P-G's group stream rather than
// recorded while packing, so every boundary that carries those two streams
// -- the one behind P-H included -- resumes into the same decision with no
// carry of its own.
type packOutcome struct {
	// Kept is the kept files in file-id order, at most the plan's file budget
	// beside the required files the budget check admitted.
	Kept []model.FileID
	// LimitDrop is set when a group was dropped for the file or slice limit;
	// LimitPossible, LimitRequirement and LimitScore are then the rank key of
	// the first such group in group order.
	LimitDrop        bool
	LimitPossible    bool
	LimitRequirement model.Requirement
	LimitScore       int64
	// OversizedWitnesses counts the witness files dropped because their group
	// does not fit one slice.
	OversizedWitnesses int64
}

// readPackOutcome walks the verdicts once, in file order, for the kept files,
// the oversized witnesses and the lowest group rank dropped for a limit, then
// the groups in rank order up to that rank for its key. Both walks hold one
// record of each stream; the witness set is the read's, at most F+1+Extra
// files.
func readPackOutcome(ctx context.Context, m *measuredPlan, p *packedPlan, witnesses []model.FileID) (packOutcome, error) {
	if m == nil || m.Groups == nil || p == nil || p.Verdicts == nil {
		return packOutcome{}, &model.Error{Code: model.CodeInternal,
			Message: "the round decision was reached with no packed plan to read"}
	}
	witness := make(map[model.FileID]bool, len(witnesses))
	for _, id := range witnesses {
		witness[id] = true
	}
	var out packOutcome
	limitAt := int64(-1)
	if err := p.Verdicts.Each(func(v packVerdict) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		switch {
		case v.Decision.Keep:
			out.Kept = append(out.Kept, v.Decision.FileID)
		case v.Reason == dropOversized:
			if witness[v.Decision.FileID] {
				out.OversizedWitnesses++
			}
		case v.Reason == dropFileLimit || v.Reason == dropSliceLimit:
			if limitAt < 0 || v.MinIndex < limitAt {
				limitAt = v.MinIndex
			}
		}
		return nil
	}); err != nil {
		return packOutcome{}, err
	}
	if limitAt < 0 {
		return out, nil
	}
	next, stop := pullRun(m.Groups)
	defer stop()
	for {
		g, ok, err := next()
		if err != nil {
			return packOutcome{}, err
		}
		if !ok {
			return packOutcome{}, &model.Error{Code: model.CodeInternal,
				Message: "the packer dropped a group the measured plan does not hold"}
		}
		if g.MinIndex == limitAt {
			out.LimitDrop = true
			out.LimitPossible, out.LimitRequirement, out.LimitScore = g.KeyPossible, g.KeyRequirement, g.KeyScore
			return out, nil
		}
	}
}

// certified reports whether the pack's certificate holds against the stop
// (ADR-0016 decision 4): the first group dropped for a limit ranks above
// (recommended, tau) -- a precise group whose requirement ranks above
// recommended, or a recommended one scoring above tau. A pack that dropped
// nothing for a limit is not bound by the file budget, so it certifies
// nothing.
func (o packOutcome) certified(tau int64) bool {
	if !o.LimitDrop || o.LimitPossible {
		return false
	}
	rank, rec := requirementRank(o.LimitRequirement), requirementRank(model.RequirementRecommended)
	return rank < rec || (rank == rec && o.LimitScore > tau)
}
