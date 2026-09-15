// This file is owned by Task 15 lane L4. It holds
// Section 15.4 budgeting: measured entry sizes, per-slice byte and token bounds, and the CTX_MINIMUM_BUDGET floor.
//
// The shared contract it builds on (candidate, the ranking constants and the
// typed error constructors) is frozen in compiler.go and is not edited here.
package context

import (
	"fmt"
	"sort"

	"github.com/Sawmonabo/codectx/internal/config"
	"github.com/Sawmonabo/codectx/internal/model"
)

// resolvedBudget is one request's budget with every zero field resolved to its
// config.Context default. Section 20.2 is explicit that a zero never means
// unlimited, so nothing downstream sees a zero bound and skips a check.
// MaxBytes and MaxTokens apply PER SLICE; MaxFiles counts distinct selected
// files across the whole plan; MaxSlices caps total slices.
//
// MaxManifestBytes caps the whole stored manifest and is a CALLER budget the
// request may raise: the deployment value is the default window, not a ceiling
// on what a caller with a larger window may ask for. It carries the
// config.Limit convention -- zero is unlimited, and an unlimited manifest bound
// is safe here because the per-slice bounds and MaxSlices still bound the plan.
type resolvedBudget struct {
	MaxBytes         int64
	MaxTokens        int64
	MaxFiles         int
	MaxSlices        int
	MaxManifestBytes config.Limit
}

// resolveBudget applies the Section 20.1 [context] defaults to req's zero
// fields. A configured default of zero or less is a wiring defect rather than
// "unlimited": it would silently disable the bound this task exists to enforce.
func resolveBudget(b model.Budget, cfg config.Context) (resolvedBudget, error) {
	if err := b.Validate(); err != nil {
		return resolvedBudget{}, err
	}
	out := resolvedBudget{
		MaxBytes:  pick64(b.MaxBytes, cfg.DefaultMaxBytes),
		MaxTokens: pick64(b.MaxEstimatedTokens, cfg.DefaultEstimatedTokens),
		MaxFiles:  pick(b.MaxFiles, cfg.DefaultMaxFiles),
		// context.max_slices carries no Default prefix but is the same kind of
		// setting: the value a zero budget.max_slices resolves to.
		MaxSlices:        pick(b.MaxSlices, cfg.MaxSlices),
		MaxManifestBytes: config.Limit(pick64(b.MaxManifestBytes, cfg.MaxManifestBytes.Value())),
	}
	for _, f := range []struct {
		key   string
		value int64
	}{
		{"context.default_max_bytes", out.MaxBytes},
		{"context.default_estimated_tokens", out.MaxTokens},
		{"context.default_max_files", int64(out.MaxFiles)},
		{"context.max_slices", int64(out.MaxSlices)},
	} {
		if f.value <= 0 {
			return resolvedBudget{}, argumentInvalid("%s resolved to %d; a budget bound must be positive, and zero never means unlimited", f.key, f.value)
		}
	}
	return out, nil
}

func pick64(requested, fallback int64) int64 {
	if requested > 0 {
		return requested
	}
	return fallback
}

func pick(requested, fallback int) int {
	if requested > 0 {
		return requested
	}
	return fallback
}

// isRequired reports whether r may never be demoted, truncated or dropped to
// fit a budget (Section 15.4). required_full and required_symbol are required;
// recommended and optional are not.
func isRequired(r model.Requirement) bool { return requirementRank(r) <= 1 }

// sizeIterations bounds the measured-size fixed point below. model.ContextEntry
// carries estimated_bytes and estimated_tokens as JSON fields, so the entry's
// own serialized length depends on the numbers being measured. The iteration
// only ever grows those numbers by a digit at a time, so it settles in two
// rounds; more than this many means the arithmetic is not converging and the
// size would be a guess, which Section 15.4 forbids.
const sizeIterations = 8

// measureEntry builds the persisted entry for c at ordinal and measures its
// size. EstimatedBytes is the ACTUALLY MEASURED canonical-JSON length of the
// entry's own metadata — Section 15.4 puts headers, metadata, delimiters and
// escaping in the budget, so the overhead is measured through serializedBytes,
// never assumed — plus, on the FIRST entry of each file only, the worst-case
// wire-encoded length of that file's source.
//
// chargeSource is what selects that first entry. A file is transported once
// however many symbols selected it, so charging its source to every entry
// would budget, floor and persist a file reached through N symbols at N times
// its size, and the Section 15.4 floor would name a budget larger than the one
// the plan actually needs. Summing the entries of a file therefore yields the
// file's real cost, which is what groupByFile and sliceOrdinals both do.
//
// Source bytes are never loaded; model.FileVersion.Size is the only input.
func measureEntry(c candidate, ordinal int, chargeSource bool) (model.ContextEntry, error) {
	var wire int64
	if chargeSource {
		var err error
		if wire, err = wireEncodedBytes(c.SizeBytes); err != nil {
			return model.ContextEntry{}, err
		}
	}
	e := model.ContextEntry{
		Ordinal:       ordinal,
		NodeID:        c.NodeID,
		FileID:        c.FileID,
		Requirement:   c.Requirement,
		ScoreMicros:   c.ScoreMicros,
		Reasons:       c.Reasons,
		EvidencePaths: evidencePaths(c.Paths),
	}
	for i := 0; i < sizeIterations; i++ {
		meta, err := serializedBytes(e)
		if err != nil {
			return model.ContextEntry{}, err
		}
		bytes := wire + meta
		tokens, err := estimateTokens(bytes)
		if err != nil {
			return model.ContextEntry{}, err
		}
		if e.EstimatedBytes == bytes && e.EstimatedTokens == tokens {
			return e, nil
		}
		e.EstimatedBytes, e.EstimatedTokens = bytes, tokens
	}
	return model.ContextEntry{}, &model.Error{Code: model.CodeInternal,
		Message: "the measured entry size did not settle; it would be a guess rather than a measurement"}
}

// evidencePaths projects the ranking lane's explanation paths onto the stored
// relation-id lists.
//
// The path COUNT is not re-bounded here. The ranking lane already applied
// context.max_reason_paths_per_entry as written -- including a value above
// model.MaxReasonPathsPerEntry, and including unlimited -- and
// model.ContextEntry.Validate no longer refuses the entry on that count, so a
// second clip at 3 would discard routes the operator asked to keep after the
// lane that honoured the setting had produced them. Only the per-path relation
// list keeps model.MaxRelationsPerPath, which Validate does still enforce.
func evidencePaths(paths []model.RelationPath) [][]model.RelationID {
	if len(paths) == 0 {
		return nil
	}
	out := make([][]model.RelationID, 0, len(paths))
	for _, p := range paths {
		if len(p.Relations) == 0 {
			continue
		}
		rel := p.Relations
		if len(rel) > model.MaxRelationsPerPath {
			rel = rel[:model.MaxRelationsPerPath]
		}
		out = append(out, append([]model.RelationID(nil), rel...))
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// plan is the Section 15.4 result: entries in Section 15.3 tie-break order with
// ordinals 0..n-1 and required_full as a prefix, slices that reference those
// ordinals, and one reasoned exclusion for every candidate not selected. The
// manifest lane persists it unchanged; nothing here is a wire shape.
type plan struct {
	Entries  []model.ContextEntry
	Slices   []model.ContextSlice
	Excluded []model.ExcludedContextEntry
}

// buildPlan sizes, selects, orders and packs cands under b.
//
// files is the snapshot metadata the caller hydrated in bounded batches through
// PinnedReader.FilesByID; a candidate whose file is absent from the pinned
// snapshot is excluded with that reason rather than silently sized as empty.
//
// Required scope never silently shrinks to fit the budget: when a required file
// cannot fit a slice, the required files outnumber MaxFiles, or the required
// entries need more than MaxSlices, this returns CTX_MINIMUM_BUDGET carrying
// the floor and the missing paths. It never demotes a requirement, truncates a
// plan, or labels a snippet as a full file.
func buildPlan(cands []candidate, files []model.FileVersion, b resolvedBudget) (plan, error) {
	meta := make(map[model.FileID]model.FileVersion, len(files))
	for _, fv := range files {
		meta[fv.ID] = fv
	}

	var out plan
	exclude := func(c candidate, reason string) {
		out.Excluded = append(out.Excluded, model.ExcludedContextEntry{
			Ordinal:   len(out.Excluded),
			Reference: model.ContextReference{NodeID: c.NodeID, FileID: c.FileID, Path: c.Path},
			Reason:    reason,
		})
	}

	sized := make([]candidate, 0, len(cands))
	for _, c := range cands {
		switch {
		case c.Excluded != "":
			// An earlier pass already ruled this candidate out with a reason;
			// budgeting neither revives it nor drops its reason.
			exclude(c, c.Excluded)
		case c.FileID == "":
			exclude(c, "the candidate names no file, so its size cannot be measured without loading source")
		default:
			fv, ok := meta[c.FileID]
			if !ok {
				exclude(c, "the file is not visible in the pinned snapshot")
				continue
			}
			c.SizeBytes, c.Status, c.Path = fv.Size, fv.Status, fv.Path
			sized = append(sized, c)
		}
	}

	// One total order (Section 15.3) decides ordinals, packing order and the
	// required_full prefix at once; nothing downstream re-sorts.
	sort.SliceStable(sized, func(i, j int) bool { return sized[i].less(sized[j]) })

	// charged holds, for each selected file, the index of its highest-ranked
	// entry: the one entry that carries the file's source bytes. Selection
	// keeps or drops a file's entries together (packPlan works on file-atomic
	// groups), so this index is still the file's first entry in phase two and
	// both passes charge the source in the same place.
	charged := make(map[model.FileID]int, len(sized))
	for i, c := range sized {
		if _, seen := charged[c.FileID]; !seen {
			charged[c.FileID] = i
		}
	}

	// Phase one measures every candidate at its position in the FULL order.
	// Selection only ever removes lower-ranked candidates, so a selected
	// entry's final ordinal is never larger than this one and its final
	// measured size is never larger than the size checked here: the budget is
	// checked against an upper bound and can only be under-filled, never
	// overfilled.
	provisional := make([]model.ContextEntry, len(sized))
	for i, c := range sized {
		e, err := measureEntry(c, i, charged[c.FileID] == i)
		if err != nil {
			return plan{}, err
		}
		provisional[i] = e
	}

	groups, err := groupByFile(sized, provisional)
	if err != nil {
		return plan{}, err
	}
	if err := checkRequiredFits(groups, b); err != nil {
		return plan{}, err
	}

	packed, err := packPlan(groups, b)
	if err != nil {
		return plan{}, err
	}

	// Phase two re-measures the selected entries at their final ordinals, so
	// every persisted size is exact rather than the conservative bound.
	final := make(map[int]int, len(packed.keep))
	out.Entries = make([]model.ContextEntry, 0, len(packed.keep))
	for i := range sized {
		if !packed.keep[i] {
			continue
		}
		ordinal := len(out.Entries)
		e, err := measureEntry(sized[i], ordinal, charged[sized[i].FileID] == i)
		if err != nil {
			return plan{}, err
		}
		final[i] = ordinal
		out.Entries = append(out.Entries, e)
	}
	if out.Slices, err = sliceOrdinals(groups, packed, final, out.Entries); err != nil {
		return plan{}, err
	}
	if err := checkManifestFits(out.Slices, b); err != nil {
		return plan{}, err
	}
	// Exclusions are appended after the entries are ordered so their ordinals
	// follow the same walk, and every dropped candidate leaves exactly one.
	for _, d := range packed.dropped {
		exclude(sized[d.index], d.reason)
	}
	return out, nil
}

// checkRequiredFits raises the Section 15.4 minimum-budget floor for the three
// frozen triggers: a required file whose encoded size exceeds MaxBytes (or
// whose tokens exceed MaxEstimatedTokens), required files outnumbering
// MaxFiles, and required files needing more than MaxSlices. The reported floor
// is the budget under which the required scope WOULD fit, so raising the budget
// to it is a real remedy rather than an invitation to retry blindly.
func checkRequiredFits(groups []fileGroup, b resolvedBudget) error {
	var floorBytes, floorTokens int64
	var requiredFiles int
	var missing []string
	for _, g := range groups {
		if !g.required {
			continue
		}
		requiredFiles++
		floorBytes = max(floorBytes, g.bytes)
		floorTokens = max(floorTokens, g.tokens)
		if g.bytes > b.MaxBytes || g.tokens > b.MaxTokens {
			missing = append(missing, g.path)
		}
	}
	if requiredFiles == 0 {
		return nil
	}
	// A required file is atomic: Section 15.4 splits an oversized component at
	// FILE boundaries, so one file's required entries cannot be spread over two
	// slices and the per-slice floor is the largest required file.
	floorSlices := packedSliceCount(groups, floorBytes, floorTokens)
	tooManyFiles := requiredFiles > b.MaxFiles
	tooManySlices := floorSlices > b.MaxSlices
	if len(missing) == 0 && !tooManyFiles && !tooManySlices {
		return nil
	}
	if tooManyFiles || tooManySlices {
		// The whole required set is what does not fit, not one file of it.
		missing = requiredPaths(groups)
	}
	return minimumBudget(floorBytes, floorTokens, max(floorSlices, 1), requiredFiles, missing)
}

// requiredPaths lists the required files in stored order, for the minimum
// budget error's `missing` detail. model.MaxDetailBytes bounds the joined value
// at the error boundary, so a large scope reports a truncated list rather than
// an unbounded one.
func requiredPaths(groups []fileGroup) []string {
	out := make([]string, 0, len(groups))
	for _, g := range groups {
		if g.required {
			out = append(out, g.path)
		}
	}
	return out
}

// packedSliceCount is how many slices the required files need when each slice
// holds at most maxBytes and maxTokens. The real packing in slice.go walks the
// same groups in the same order under the same rule, so this is the count that
// budget would actually produce rather than an estimate of it.
//
// checkRequiredFits calls it at the reported min_bytes, not at the caller's
// MaxBytes, which is what makes the reported details one coherent budget: this
// packing is monotone in the cap, so the count at the smallest admissible slice
// size is an upper bound on the count at any larger one. Raising max_slices to
// min_slices therefore suffices for every max_bytes at or above min_bytes,
// including the caller's current value, and the two details never describe
// budgets that disagree.
func packedSliceCount(groups []fileGroup, maxBytes, maxTokens int64) int {
	var slices int
	var bytes, tokens int64
	for _, g := range groups {
		if !g.required {
			continue
		}
		if slices == 0 || bytes+g.bytes > maxBytes || tokens+g.tokens > maxTokens {
			slices++
			bytes, tokens = 0, 0
		}
		bytes += g.bytes
		tokens += g.tokens
	}
	return slices
}

// checkManifestFits enforces the Section 15.4 stored-manifest byte cap over the
// plan the packer actually produced. The per-slice bounds alone do not imply
// it: MaxSlices slices of MaxBytes each may exceed context.max_manifest_bytes,
// and only required scope is exempt from being dropped, never from the cap. The
// totals summed here are the exact ones that persist, so the refusal describes
// the manifest that would have been stored rather than an estimate of it.
func checkManifestFits(slices []model.ContextSlice, b resolvedBudget) error {
	var total int64
	for _, s := range slices {
		total += s.EstimatedBytes
	}
	// An unlimited manifest budget has nothing to exceed. config.Limit owns the
	// test; a bare `total <= value` would read unlimited as "refuse everything".
	if !b.MaxManifestBytes.Exceeded(total) {
		return nil
	}
	return (&model.Error{
		Code:    model.CodeResourceLimit,
		Message: "the selected context exceeds the stored-manifest byte cap",
	}).
		WithDetail("manifest_bytes", fmt.Sprint(total)).
		WithDetail("cap", "context.max_manifest_bytes").
		WithDetail("cap_bytes", b.MaxManifestBytes.String()).
		WithRemediation("narrow the task, or raise budget.max_manifest_bytes on the request (it is a caller budget, " +
			"defaulting to context.max_manifest_bytes, and 0 means unlimited)")
}
