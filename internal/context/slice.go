// This file is owned by Task 15 lane L4. It holds
// Section 15.4 slicing: deterministic packing order, component grouping and file-boundary splits that preserve full-file requirements.
//
// The shared contract it builds on (candidate, the ranking constants and the
// typed error constructors) is frozen in compiler.go and is not edited here.
package context

import "github.com/Sawmonabo/codectx/internal/model"

// fileGroup is the atomic unit of packing: every selected entry over one file,
// kept together.
//
// Section 15.4 groups required entries by strong dependency component and
// splits an oversized component "at file boundaries with the bounded
// task/contract header repeated, preserving full-file requirements". The
// frozen candidate record carries no component identity, and deriving strong
// components from explanation paths is the ranking lane's data, not
// budgeting's. The file boundary is therefore the grouping this rule actually
// needs and the only one this lane can honour without inventing an input: a
// required file is never split across slices, and a component larger than one
// slice is split between its files. A component that fits in one slice is
// unaffected either way, because the entries of a component are contiguous in
// the Section 15.3 order this packing walks.
type fileGroup struct {
	path     string
	required bool
	// indexes are positions in the sorted candidate slice, ascending, so the
	// first one is the group's rank and its ordinals stay in tie-break order.
	indexes []int
	bytes   int64
	tokens  int64
}

// groupByFile collects the sorted candidates into file-atomic groups, in the
// order their highest-ranked member appears. Group sizes sum the measured entry
// sizes, which over-counts a file selected through several symbols rather than
// under-counting it: a budget check must never be optimistic.
func groupByFile(sorted []candidate, entries []model.ContextEntry) ([]fileGroup, error) {
	if len(sorted) != len(entries) {
		return nil, &model.Error{Code: model.CodeInternal,
			Message: "the sized candidate and measured entry counts disagree"}
	}
	byFile := make(map[model.FileID]int, len(sorted))
	groups := make([]fileGroup, 0, len(sorted))
	for i, c := range sorted {
		at, ok := byFile[c.FileID]
		if !ok {
			byFile[c.FileID] = len(groups)
			groups = append(groups, fileGroup{path: c.Path})
			at = len(groups) - 1
		}
		g := &groups[at]
		g.indexes = append(g.indexes, i)
		g.bytes += entries[i].EstimatedBytes
		g.tokens += entries[i].EstimatedTokens
		// A file holding one required entry is required as a whole: dropping
		// its other entries to save budget would be the silent shrink of
		// required scope that Section 15.4 forbids.
		g.required = g.required || isRequired(c.Requirement)
	}
	return groups, nil
}

// drop records one candidate the packer could not fit, with the reason it is
// persisted as an exclusion. Every unselected candidate leaves one, so an
// omission is visible rather than silent (Section 15.4).
type drop struct {
	index  int
	reason string
}

// packing is what packPlan decided: which candidate indexes are kept, which
// slice each kept group belongs to, and why every other candidate was dropped.
// The packer owns the slice boundaries outright; nothing downstream re-derives
// them from sizes, so the stored slices cannot drift from the ones the budget
// was actually checked against.
type packing struct {
	keep    map[int]bool
	sliceOf map[int]int // group index -> slice index
	dropped []drop
}

// packPlan assigns groups to slices in Section 15.3 order: required files
// first (they are a prefix of that order), then recommended, then optional.
//
// A required group is never dropped here. checkRequiredFits has already proved
// the required set fits, so reaching a required group that does not is a defect
// in that check, not a budget outcome to absorb quietly.
func packPlan(groups []fileGroup, b resolvedBudget) (packing, error) {
	p := packing{keep: map[int]bool{}, sliceOf: map[int]int{}}
	files := 0
	slices := 0
	var bytes, tokens int64
	// full records whether the packer has stopped opening slices, so every
	// later group is excluded for the same stated reason instead of being
	// silently skipped.
	full := false

	for gi, g := range groups {
		if !g.required {
			switch {
			case full:
				p.dropped = appendDrops(p.dropped, g, "the budget's slice limit was reached before this entry")
				continue
			case files >= b.MaxFiles:
				p.dropped = appendDrops(p.dropped, g, "the budget's file limit was reached before this entry")
				continue
			case g.bytes > b.MaxBytes || g.tokens > b.MaxTokens:
				p.dropped = appendDrops(p.dropped, g, "the file does not fit in one slice of this budget")
				continue
			}
		}
		if slices == 0 || bytes+g.bytes > b.MaxBytes || tokens+g.tokens > b.MaxTokens {
			if slices == b.MaxSlices {
				if g.required {
					return packing{}, &model.Error{Code: model.CodeInternal,
						Message: "a required file did not fit the slice budget the minimum-budget check accepted"}
				}
				full = true
				p.dropped = appendDrops(p.dropped, g, "the budget's slice limit was reached before this entry")
				continue
			}
			slices++
			bytes, tokens = 0, 0
		}
		bytes += g.bytes
		tokens += g.tokens
		files++
		p.sliceOf[gi] = slices - 1
		for _, i := range g.indexes {
			p.keep[i] = true
		}
	}
	return p, nil
}

// appendDrops records every entry of an unselected group, so a file excluded
// through several symbols leaves one reasoned exclusion per symbol rather than
// one for the file and silence for the rest.
func appendDrops(dst []drop, g fileGroup, reason string) []drop {
	for _, i := range g.indexes {
		dst = append(dst, drop{index: i, reason: reason})
	}
	return dst
}

// sliceOrdinals materializes the packer's slice assignment over the FINAL entry
// ordinals and sizes. It re-decides no boundary: each kept group goes to the
// slice packPlan chose for it, so the stored totals are exact while the
// membership is exactly what the budget was checked against. Every selected
// ordinal therefore lands in exactly one slice, and the totals are never larger
// than the provisional ones (an entry's ordinal only shrinks when a
// lower-ranked candidate is dropped, and a shorter ordinal cannot serialize
// longer).
func sliceOrdinals(groups []fileGroup, p packing, final map[int]int, entries []model.ContextEntry) ([]model.ContextSlice, error) {
	out := make([]model.ContextSlice, 0, len(p.sliceOf))
	for gi, g := range groups {
		si, packed := p.sliceOf[gi]
		if !packed {
			continue
		}
		for si >= len(out) {
			out = append(out, model.ContextSlice{Index: len(out)})
		}
		cur := &out[si]
		for _, i := range g.indexes {
			if !p.keep[i] {
				continue
			}
			o, ok := final[i]
			if !ok {
				return nil, &model.Error{Code: model.CodeInternal,
					Message: "a selected entry was not assigned an ordinal"}
			}
			cur.EntryOrdinals = append(cur.EntryOrdinals, o)
			cur.EstimatedBytes += entries[o].EstimatedBytes
			cur.EstimatedTokens += entries[o].EstimatedTokens
		}
	}
	for i := range out {
		if len(out[i].EntryOrdinals) == 0 {
			return nil, &model.Error{Code: model.CodeInternal,
				Message: "the packer opened a slice it put no entry in"}
		}
	}
	return out, nil
}
