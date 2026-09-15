// This file is owned by Task 15 lane L4. It holds
// Section 15.4 budgeting: measured entry sizes, per-slice byte and token bounds, and the CTX_MINIMUM_BUDGET floor.
//
// The shared contract it builds on (candidate, the ranking constants and the
// typed error constructors) is frozen in compiler.go and is not edited here.
package context

import (
	"context"
	"fmt"
	"sort"

	"github.com/Sawmonabo/codectx/internal/config"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
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
		MaxManifestBytes: resolveManifestBytes(b.MaxManifestBytes, cfg.MaxManifestBytes),
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

// resolveManifestBytes resolves the one budget field a request may raise to
// unlimited. Zero inherits the deployment value (which is itself unlimited by
// default); model.BudgetUnlimited is the caller saying it can hold any manifest,
// and it wins over a finite deployment default because this is a caller budget,
// not a ceiling on what a caller with a larger window may ask for.
func resolveManifestBytes(requested int64, configured config.Limit) config.Limit {
	if requested == model.BudgetUnlimited {
		return config.Unlimited
	}
	if requested > 0 {
		return config.Limit(requested)
	}
	return configured
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
func measureEntry(c candidate, ordinal int, chargeSource bool) (model.ContextEntry, int64, error) {
	var wire int64
	if chargeSource {
		var err error
		if wire, err = wireEncodedBytes(c.SizeBytes); err != nil {
			return model.ContextEntry{}, 0, err
		}
	}
	paths, clipped := evidencePaths(c.Paths)
	e := model.ContextEntry{
		Ordinal:       ordinal,
		NodeID:        c.NodeID,
		FileID:        c.FileID,
		Requirement:   c.Requirement,
		ScoreMicros:   c.ScoreMicros,
		Reasons:       c.Reasons,
		EvidencePaths: paths,
	}
	for i := 0; i < sizeIterations; i++ {
		meta, err := serializedBytes(e)
		if err != nil {
			return model.ContextEntry{}, 0, err
		}
		bytes := wire + meta
		tokens, err := estimateTokens(bytes)
		if err != nil {
			return model.ContextEntry{}, 0, err
		}
		if e.EstimatedBytes == bytes && e.EstimatedTokens == tokens {
			return e, clipped, nil
		}
		e.EstimatedBytes, e.EstimatedTokens = bytes, tokens
	}
	return model.ContextEntry{}, 0, &model.Error{Code: model.CodeInternal,
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
// list keeps model.MaxRelationsPerPath, which Validate does still enforce -- and
// the cut is now counted and returned, so a route the manifest stores shorter
// than the route the walk found is disclosed as a manifest notice instead of
// being a silent edit of the evidence.
func evidencePaths(paths []model.RelationPath) ([][]model.RelationID, int64) {
	if len(paths) == 0 {
		return nil, 0
	}
	var clipped int64
	out := make([][]model.RelationID, 0, len(paths))
	for _, p := range paths {
		if len(p.Relations) == 0 {
			continue
		}
		rel := p.Relations
		if len(rel) > model.MaxRelationsPerPath {
			rel = rel[:model.MaxRelationsPerPath]
			clipped++
		}
		out = append(out, append([]model.RelationID(nil), rel...))
	}
	if len(out) == 0 {
		return nil, clipped
	}
	return out, clipped
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

// ---------------------------------------------------------------------------
// Streamed budgeting — C-STREAM passes P-G, P-H and P-I
// ---------------------------------------------------------------------------
//
// buildPlan above holds five candidate-sized structures at once (`sized`,
// `charged`, `provisional`, the groups' index lists and `plan.Excluded`). The
// passes below produce the SAME plan from sorted streams: every list becomes a
// sorted run, every map a merge join, and the only heap that grows with the
// answer is the one ruling C4 keeps there -- the slice table and its entry
// ordinals, which are a function of the resolved budget and not of the
// repository.
//
// Two invariants make the streamed plan equal to the whole-set one and are
// worth stating where a reader meets them:
//
//   - `Index` counts SURVIVORS of the three `sized` filters, never records of
//     the ranked stream. Today's `i` is a position in `sized` (budget.go:282),
//     so counting a filtered record would shift every later ordinal and with it
//     every stored slice membership.
//   - Exclusions keep today's sequence (ruling C1): the pre-sort exclusions in
//     expansion order first, then the packer's drops in group order. The first
//     group is replayed from a run keyed by `Seq` -- the ingest order the
//     ranked stream destroys -- and the second from a run keyed by
//     (group rank, entry position), which is the order appendDrops emits in.

// The three reasons the pre-sort filters exclude a candidate with. They are
// constants so the streamed pass and buildPlan cannot drift apart in the text a
// stored exclusion carries; `c.Excluded` itself is an earlier pass's own reason
// and has no constant here.
const (
	excludeNoFile    = "the candidate names no file, so its size cannot be measured without loading source"
	excludeInvisible = "the file is not visible in the pinned snapshot"
)

// planSink receives the streamed plan as P-I produces it: one entry per
// selected candidate in ascending ordinal, then one reasoned exclusion per
// candidate that was not selected, also in ascending ordinal. It is what keeps
// the two unbounded lists of `plan` out of heap -- entries go to the streamed
// Plan.Units and exclusions to the excluded-entries projection -- and it is an
// interface so the parity test can collect them into today's `plan` value and
// compare the two pipelines field by field.
type planSink interface {
	Entry(model.ContextEntry) error
	Exclude(model.ExcludedContextEntry) error
}

// planParts is what a streamed plan keeps in heap once P-I has run: the slice
// table (ruling C4: a function of the resolved budget, which is the caller's
// own declared window) and the three totals the manifest lane reports. The
// entries and exclusions themselves went to the sink.
type planParts struct {
	Slices           []model.ContextSlice
	RelationsClipped int64
	Entries          int64
	Excluded         int64
}

// rankedStreams is what the ranking passes hand the budget passes: the ranked
// candidate run (P-F, ordered by lessRank) and the two route runs (P-D, ordered
// by lessPathSeq and lessHopSeq, both keyed on the candidate's `Seq`).
//
// The routes travel separately because candRec deliberately does not carry them
// (C-L0 report, deviation 2: context.max_reason_paths_per_entry is unlimited by
// default, so an inline route list has no bound a sort record may assume). Both
// budget phases measure entries, and measureEntry reads `candidate.Paths`, so
// both must re-join the routes to their candidate; P-G re-keys them once from
// `Seq` to `Index` so that the measuring walks, which run in rank order, read
// them in one forward pass.
type rankedStreams struct {
	Ranked *pagination.SortedRun[candRec]
	Paths  *pagination.SortedRun[pathRec]
	Hops   *pagination.SortedRun[hopRec]
}

// measuredPlan is P-G's output and P-H's and P-I's input.
type measuredPlan struct {
	// ByFile is every survivor, ordered by lessFileIndex. P-I walks it to join
	// the packer's per-file verdict onto each member, and the first record of
	// each file run is that file's charged entry.
	ByFile *pagination.SortedRun[candRec]
	// Groups is one groupRec per file, ordered by lessGroupIndex: the order
	// groupByFile produces and the order packPlan must walk.
	Groups *pagination.SortedRun[groupRec]
	// Excluded is the pre-sort exclusions, ordered by Seq, each carrying its
	// reason in `Excluded` and its reference path in `PathAtRank` -- the value
	// today's exclude() reads, since buildPlan's unconditional path overwrite
	// happens only on the surviving branch (budget.go:254).
	Excluded *pagination.SortedRun[candRec]
	// Paths and Hops are the route streams re-keyed from Seq to Index.
	Paths *pagination.SortedRun[pathRec]
	Hops  *pagination.SortedRun[hopRec]
}

// packedPlan is P-H's output: one verdict per file group, ordered by file so
// P-I merge-joins it against ByFile without holding either side.
type packedPlan struct {
	Verdicts *pagination.SortedRun[packVerdict]
}

// sizedRec is one survivor on its way to the phase-one measurement, carrying
// the one fact candRec has no field for: whether this is the entry its file's
// source bytes are charged to. That fact is decided in (FileID, Index) order
// and consumed in Index order, so it rides the record between the two.
type sizedRec struct {
	Cand    candRec `json:"c"`
	Charged bool    `json:"g,omitempty"`
}

// keepRec is one member of a group the packer kept: the candidate, its charge
// flag, the slice its group went to and that group's rank. MinIndex is what
// orders a slice's entry ordinals, which follow group order and not ordinal
// order (sliceOrdinals walks groups, not entries).
type keepRec struct {
	Cand     candRec `json:"c"`
	Charged  bool    `json:"g,omitempty"`
	Slice    int32   `json:"s,omitempty"`
	MinIndex int64   `json:"m"`
}

// dropRec is one member of a group the packer dropped, keyed so that replaying
// the run reproduces appendDrops' order exactly: groups in rank order, and
// within a group its entries in rank order (slice.go's `indexes` are ascending).
type dropRec struct {
	Cand     candRec `json:"c"`
	MinIndex int64   `json:"m"`
	Reason   string  `json:"r,omitempty"`
}

// sliceMember is one selected entry's contribution to its slice, held only
// until the slice table is assembled (ruling C4).
type sliceMember struct {
	minIndex int64
	index    int64
	ordinal  int
	bytes    int64
	tokens   int64
}

// lessGroupFile groups per-candidate group contributions by file so foldGroup
// reduces them to one groupRec per file. A join comparator: adding a tie-break
// would make every record distinct and fold nothing.
func lessGroupFile(a, b groupRec) int { return cmpString(string(a.FileID), string(b.FileID)) }

// lessSizedIndex and lessKeepIndex order a measurement walk by rank position,
// which is the order the routes are keyed on and the order ordinals are
// assigned in. Total.
func lessSizedIndex(a, b sizedRec) int { return cmpInt(a.Cand.Index, b.Cand.Index) }
func lessKeepIndex(a, b keepRec) int   { return cmpInt(a.Cand.Index, b.Cand.Index) }

// lessDropRank orders drops as appendDrops emits them. Total.
func lessDropRank(a, b dropRec) int {
	if c := cmpInt(a.MinIndex, b.MinIndex); c != 0 {
		return c
	}
	return cmpInt(a.Cand.Index, b.Cand.Index)
}

func sizeOfSized(r sizedRec) int64 { return sizeOfCand(r.Cand) + recordOverheadBytes }
func sizeOfKeep(r keepRec) int64   { return sizeOfCand(r.Cand) + recordOverheadBytes }
func sizeOfDrop(r dropRec) int64 {
	return sizeOfCand(r.Cand) + int64(len(r.Reason)) + recordOverheadBytes
}

// sortedRun ends a sort and registers its output for release, so the run file
// is removed on every exit path and not only the one that reads it to the end.
func sortedRun[T any](s *compileSorts, sorter *pagination.ExternalSort[T]) (*pagination.SortedRun[T], error) {
	run, err := sorter.Sorted()
	if err != nil {
		return nil, err
	}
	return trackRun(s, run), nil
}

// routeCursors rebuilds one candidate's model.RelationPath values from the two
// Index-keyed route streams. Both are ascending in the same key as the walk
// that drives them, so a candidate's routes are read in one forward step and
// the only routes in heap are that candidate's -- which is what buildPlan holds
// per candidate today anyway.
type routeCursors struct {
	paths *runCursor[pathRec]
	hops  *runCursor[hopRec]
}

func newRouteCursors(paths *pagination.SortedRun[pathRec], hops *pagination.SortedRun[hopRec]) *routeCursors {
	return &routeCursors{paths: newRunCursor(paths), hops: newRunCursor(hops)}
}

// close releases both pull cursors. A cursor left open holds a coroutine and,
// with it, the file the run is read from.
func (r *routeCursors) close() {
	r.paths.stop()
	r.hops.stop()
}

// take answers the routes of the candidate at index. Routes of candidates the
// caller skipped (a dropped group, in phase two) are stepped over: the walks
// that drive this are subsequences of the key order, never reorderings of it.
func (r *routeCursors) take(index int64) ([]model.RelationPath, error) {
	for r.paths.ok && r.paths.cur.Seq < index {
		r.paths.advance()
	}
	for r.hops.ok && r.hops.cur.Seq < index {
		r.hops.advance()
	}
	var out []model.RelationPath
	for r.paths.ok && r.paths.cur.Seq == index {
		p := r.paths.cur
		r.paths.advance()
		var rels []model.RelationID
		for r.hops.ok && r.hops.cur.Seq == index && r.hops.cur.PathIdx == p.PathIdx {
			rels = append(rels, r.hops.cur.RelationID)
			r.hops.advance()
		}
		if int32(len(rels)) != p.HopCount {
			return nil, &model.Error{Code: model.CodeInternal,
				Message: "a reason route reached budgeting with fewer hops than it declared"}
		}
		out = append(out, model.RelationPath{Relations: rels, Evidence: p.Evidence, CostUnits: p.CostUnits})
	}
	if err := r.paths.err(); err != nil {
		return nil, err
	}
	return out, r.hops.err()
}

// checkRequiredFitsStream is checkRequiredFits over a sorted run. It walks the
// run up to three times -- SortedRun.Each re-opens its backing file per call
// and only refuses after Close -- for the floor, the slice count at that floor,
// and, on the refusing path only, the required path list the error reports.
//
// `missing` is the one list that still accumulates, and only on the error path:
// it is the detail minimumBudget joins, and model.MaxDetailBytes bounds it at
// the error boundary exactly as it bounds today's.
func checkRequiredFitsStream(ctx context.Context, groups *pagination.SortedRun[groupRec], b resolvedBudget) error {
	var floorBytes, floorTokens int64
	var requiredFiles int
	var missing []string
	if err := groups.Each(func(g groupRec) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !g.Required {
			return nil
		}
		requiredFiles++
		floorBytes = max(floorBytes, g.Bytes)
		floorTokens = max(floorTokens, g.Tokens)
		if g.Bytes > b.MaxBytes || g.Tokens > b.MaxTokens {
			missing = append(missing, g.Path)
		}
		return nil
	}); err != nil {
		return err
	}
	if requiredFiles == 0 {
		return nil
	}
	floorSlices, err := packedSliceCountStream(ctx, groups, floorBytes, floorTokens)
	if err != nil {
		return err
	}
	tooManyFiles := requiredFiles > b.MaxFiles
	tooManySlices := floorSlices > b.MaxSlices
	if len(missing) == 0 && !tooManyFiles && !tooManySlices {
		return nil
	}
	if tooManyFiles || tooManySlices {
		// The whole required set is what does not fit, not one file of it.
		missing = missing[:0]
		if err := groups.Each(func(g groupRec) error {
			if g.Required {
				missing = append(missing, g.Path)
			}
			return nil
		}); err != nil {
			return err
		}
	}
	return minimumBudget(floorBytes, floorTokens, max(floorSlices, 1), requiredFiles, missing)
}

// packedSliceCountStream is packedSliceCount over a sorted run: the same walk
// under the same rule, so the reported min_slices is the count the real packing
// would produce rather than an estimate of it.
func packedSliceCountStream(ctx context.Context, groups *pagination.SortedRun[groupRec],
	maxBytes, maxTokens int64) (int, error) {
	var slices int
	var bytes, tokens int64
	err := groups.Each(func(g groupRec) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !g.Required {
			return nil
		}
		if slices == 0 || bytes+g.Bytes > maxBytes || tokens+g.Tokens > maxTokens {
			slices++
			bytes, tokens = 0, 0
		}
		bytes += g.Bytes
		tokens += g.Tokens
		return nil
	})
	return slices, err
}

// assembleSlices is sliceOrdinals' second half: the slice table, built from the
// members P-I collected. A slice's ordinals follow GROUP order and then entry
// order within the group -- not ordinal order, because one group's members can
// straddle another's -- so the members are ordered by (group rank, entry rank)
// here exactly as sliceOrdinals' nested walk orders them.
func assembleSlices(members [][]sliceMember) ([]model.ContextSlice, error) {
	out := make([]model.ContextSlice, 0, len(members))
	for si := range members {
		ms := members[si]
		if len(ms) == 0 {
			return nil, &model.Error{Code: model.CodeInternal,
				Message: "the packer opened a slice it put no entry in"}
		}
		sort.Slice(ms, func(i, j int) bool {
			if ms[i].minIndex != ms[j].minIndex {
				return ms[i].minIndex < ms[j].minIndex
			}
			return ms[i].index < ms[j].index
		})
		s := model.ContextSlice{Index: si}
		for _, m := range ms {
			s.EntryOrdinals = append(s.EntryOrdinals, m.ordinal)
			s.EstimatedBytes += m.bytes
			s.EstimatedTokens += m.tokens
		}
		out = append(out, s)
	}
	return out, nil
}
