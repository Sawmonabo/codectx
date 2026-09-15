package context

// stream_parity_test.go holds the whole-set pipeline the streamed passes
// replaced, kept verbatim as the REFERENCE the parity proofs compare against.
//
// A function arrives here when Compile stops calling it. Keeping the reference
// in a _test.go file rather than deleting it is deliberate: the streamed passes
// are only correct insofar as they reproduce this code's answer, and a
// reference that lives in production would be a second pipeline a caller could
// reach by accident. Nothing here may be called from a non-test file.
//
// Lane C-INT2 moved the two functions below, whose production callers Compile's
// wiring removed. `expandScope`, `rank` and `buildPlan` are still referenced by
// tests in four files of this package and have NOT been moved yet; that
// relocation, together with the helper sweep it needs, is the open half of
// C-STREAM item 5 and is recorded in C-INT2-report.md.

import (
	"context"
	"encoding/json"
	"sort"
	"testing"

	"github.com/Sawmonabo/codectx/internal/config"
	"github.com/Sawmonabo/codectx/internal/graph"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/storage/sqlite"
)

// hydrateFiles reads Size, Path and Status for every candidate's file in
// bounded batches and writes them onto the candidates in place.
//
// It runs ONCE, before ranking: the Section 15.3 active-change boost reads
// Status, and the Section 15.4 budget sizes an entry from Size, so a compile
// that hydrated per pass would issue the same read twice and could observe two
// different answers. The rows are returned as well as applied, so the budget
// pass consumes exactly what ranking saw.
//
// FilesByID omits an id the pinned snapshot does not hold and returns file_id
// order rather than input order, so the result is indexed by id here and a
// candidate whose file is invisible keeps a zero size, which buildPlan excludes
// with that reason rather than sizing as empty.
func (c *Compiler) hydrateFiles(ctx context.Context, reader *sqlite.PinnedReader,
	cands []candidate) ([]model.FileVersion, error) {
	seen := map[model.FileID]struct{}{}
	ids := make([]model.FileID, 0, len(cands))
	for _, cand := range cands {
		if cand.FileID == "" || cand.Excluded != "" {
			continue
		}
		if _, dup := seen[cand.FileID]; dup {
			continue
		}
		seen[cand.FileID] = struct{}{}
		ids = append(ids, cand.FileID)
	}
	limit := c.pageLimit()
	out := make([]model.FileVersion, 0, len(ids))
	for start := 0; start < len(ids); start += limit {
		batch := ids[start:min(start+limit, len(ids))]
		page, err := reader.FilesByID(ctx, batch)
		if err != nil {
			return nil, err
		}
		out = append(out, page...)
	}
	byID := make(map[model.FileID]model.FileVersion, len(out))
	for _, fv := range out {
		byID[fv.ID] = fv
	}
	for i := range cands {
		fv, ok := byID[cands[i].FileID]
		if !ok {
			continue
		}
		cands[i].SizeBytes, cands[i].Status = fv.Size, fv.Status
		if cands[i].Path == "" {
			cands[i].Path = fv.Path
		}
	}
	return out, nil
}

// relationsOnPaths reads the relation kind of every edge the expansion admitted
// onto a retained route, and reports whether it found all of them.
//
// A model.RelationPath stores relation ids only, graph.ImpactResult never
// returns the relations it walked, and the pinned reader exposes no by-id
// relation read -- adding one would be a second spelling of EdgesBatch. So the
// edges are re-read here from the candidate node set, keyset-paged by relation
// id and bounded by the same context.max_graph_edges the walk ran under, and
// filtered to the ids the routes actually name.
//
// The completeness flag matters: ranking treats an edge it cannot type as
// inadmissible, so stopping at the edge bound with ids still unfound changes
// scores. The caller discloses that as an incomplete scope rather than letting
// two compiles of one generation disagree in silence.
// It is a thin wrapper: the body moved to rankjoin.go, beside the streamed
// pass that must reproduce its read log, and reads through the narrow
// relationReader so that log is observable. Lane L5 removes this wrapper when
// Compile stops calling it.
func (c *Compiler) relationsOnPaths(ctx context.Context, reader *sqlite.PinnedReader,
	cands []candidate) (map[model.RelationID]model.Relation, bool, error) {
	return c.relationsOnPathsWholeSet(ctx, reader, cands)
}

// expandScope turns resolved seeds into the Section 15.2 required scope. The
// engine and the generation are passed in already open and already pinned: the
// reader lease and the engine release belong to Compile, which defers them in
// reverse open order on every path, so opening either here would put one
// lifetime in two places.
//
// caps is the pinned generation's capability report (PinnedReader.Capabilities,
// query.go:498). It is the authoritative freshness source rather than
// ImpactResult.Meta.Completeness, which the engine populates only for deferred
// dependence (impact.go:41) and therefore under-reports stale, partial,
// unavailable and failed providers; both are merged so neither is lost.
//
// An unresolvable EXPLICIT seed is the one scope failure that is an error: the
// caller named a boundary and it does not exist, so CTX_SCOPE_INCOMPLETE says
// so instead of quietly compiling a plan around it. An empty or wholly
// ambiguous extracted scope is NOT an error: it yields a discovery answer with
// ScopeComplete false, no required entry and every unresolved token kept as its
// own reasoned exclusion (Section 15.2, ruling Q7).
func expandScope(ctx context.Context, eng *graph.Engine, gen model.GenerationID, cfg config.Context,
	seeds []candidate, caps []model.CapabilityState) (scopeResult, error) {
	if eng == nil {
		return scopeResult{}, argumentInvalid("scope expansion requires a graph engine")
	}
	if gen == 0 {
		return scopeResult{}, argumentInvalid("scope expansion requires an explicit pinned generation")
	}

	res := scopeResult{ScopeComplete: true, Candidates: make([]candidate, 0, len(seeds))}
	admitted := make(map[string]bool, len(seeds))
	start := make([]model.NodeID, 0, len(seeds))
	resolved := 0

	for _, s := range seeds {
		if s.Excluded != "" {
			if s.Origin == originExplicitSeed {
				return scopeResult{}, scopeIncomplete(s.Path)
			}
			// An unresolved extracted token stays visible as an exclusion and
			// carries the answer down to discovery rather than out of it.
			res.ScopeComplete = false
			res.Candidates = append(res.Candidates, s)
			continue
		}
		resolved++
		if id := s.entityID(); id != "" {
			if admitted[id] {
				// One file surfaces from several Section 15.2 steps -- an
				// explicit seed is also a changed file, a backtick token also
				// resolves exactly. Ambiguity that must be preserved is several
				// DISTINCT entities for one name, not one entity found twice:
				// context_entries is keyed by ordinal, not by node, so a second
				// copy would persist as a duplicate entry and an EntryCount
				// nothing rejects. The earliest step wins, which is the
				// strongest origin.
				continue
			}
			admitted[id] = true
		}
		s.Depth = 0
		if s.Requirement == "" {
			// Every Section 15.2 seed producer assigns its own requirement:
			// full for a named identity, recommended for a lexical hit,
			// optional for a captured change. Promoting all of them here would
			// put a merely modified or lexically matched file into the
			// required_full prefix Task 16 reads as mandatory, which Section
			// 15.2 and ruling Q7 both forbid. The default stands only for a
			// seed that carries none, which would otherwise rank below optional
			// and be droppable.
			s.Requirement = model.RequirementFull
		}
		res.Candidates = append(res.Candidates, s)
		if s.NodeID != "" && len(start) < model.MaxStartNodes {
			start = append(start, s.NodeID)
		} else if s.NodeID != "" {
			// More seeds than one bounded walk may start from: the boundaries
			// of the seeds that did not start are unexplored, and the answer
			// says so rather than reading as an exhaustive scope.
			res.ScopeComplete = false
		}
	}

	switch {
	case resolved == 0:
		// Discovery: nothing resolved, so nothing is required and no walk is
		// run. Seeds already carry their own exclusion reasons.
		res.ScopeComplete = false
		return res, nil
	case len(start) == 0:
		// Files resolved but no symbol did, so no boundary can be walked from
		// them. Each seed keeps the requirement its Section 15.2 step assigned;
		// the scope is not complete.
		res.ScopeComplete = false
		res.Completeness = degradedCapabilities(caps, nil)
		return res, nil
	}

	impact, err := eng.Impact(ctx, model.ImpactRequest{
		GenerationID: gen,
		Start:        start,
		Relations:    scopeRelations,
		// Both directions: a caller reaches the seed through an incoming edge
		// and a callee through an outgoing one, and Section 15.2 requires both.
		Direction:  model.DirectionBoth,
		MaxDepth:   cfg.MaxGraphDepth.Int(),
		MaxVisited: cfg.MaxVisitedNodes.Int(),
		MaxEdges:   cfg.MaxGraphEdges.Int(),
	})
	if err != nil {
		return scopeResult{}, contextErr(ctx, err)
	}
	if impact.Meta.Truncated {
		res.ScopeComplete = false
	}
	res.Completeness = degradedCapabilities(caps, impact.Meta.Completeness)
	if len(res.Completeness) > 0 {
		res.ScopeComplete = false
	}

	for _, e := range impact.Entries {
		c := candidate{
			NodeID: e.NodeID,
			FileID: e.FileID,
			// Path is deliberately left empty: candidate.Path is a FILE PATH
			// everywhere else, and ImpactEntry.Name is a qualified name.
			// Writing the name here made hydrateFiles (compiler.go) skip the
			// entry -- it fills Path only when it is empty -- so the file path
			// the FileID resolves to never reached the candidate, and
			// rank.go's packageOf() bucketed qualified names instead of
			// directories.
			Requirement: boundaryRequirement(e.Kind, e.Depth),
			Origin:      originExpansion,
			Depth:       e.Depth,
		}
		var dropped, truncated int64
		c.Reasons, dropped, truncated = boundReasons(e.Reasons)
		res.ReasonsDropped += dropped
		res.ReasonsTruncated += truncated
		c.Paths, c.MorePaths = boundPaths(e.Paths, cfg.MaxReasonPathsPerEntry)
		if admitted[c.entityID()] {
			// A seed the walk reached again keeps its seed requirement, which
			// is the strongest one; re-adding it would double an entry.
			continue
		}
		admitted[c.entityID()] = true
		res.Candidates = append(res.Candidates, c)
	}
	return res, nil
}

// rank scores every candidate in place and returns the same slice. It does NOT
// order it: buildPlan rewrites each candidate's Path from the file version and
// then sorts with the same total Section 15.3 order, so an order established
// here would be sorted on a path key that budgeting replaces. It resolves
// per-edge precision for the whole candidate set in one batched pass, scores
// each admitted route in fixed point, keeps the maximum route (never a sum over
// routes, which is how a cycle inflates a score), discloses the routes it did
// not enumerate as a bounded reason, and adds each bounded boost at most once.
//
// relations carries every relation on an admitted path, keyed by id: a
// RelationPath stores relation ids only and the pinned reader exposes no
// by-id relation read, so the pass that walked the edges supplies their kinds.
// An id missing from it, or a kind absent from contributionWeight, makes that
// route inadmissible rather than free.
//
// Candidate Status must already be hydrated for the active-change boost to see
// a captured change; the compile hydrates file metadata once and the budget
// pass reuses it.
//
// It is not idempotent: it narrows each candidate's Paths to the routes it
// retained, so a second call over the same slice would resolve precision for
// only that subset and re-score against it. Rank once per compile.
func (c *Compiler) rank(ctx context.Context, reader *sqlite.PinnedReader, cands []candidate,
	relations map[model.RelationID]model.Relation) ([]candidate, error) {
	if reader == nil {
		return nil, argumentInvalid("ranking requires a pinned reader")
	}
	precision, err := c.resolvePrecision(ctx, reader, cands)
	if err != nil {
		return nil, err
	}

	// Pass one scores routes and records, per package, the distinct admitted
	// edges this compile actually touched -- the walk-local centrality input.
	routed := make([]routeScore, len(cands))
	centrality := map[string]map[model.RelationID]struct{}{}
	for i := range cands {
		r, err := scoreRoutes(cands[i], relations, precision, c.reasonPathLimit())
		if err != nil {
			return nil, err
		}
		routed[i] = r
		pkg := packageOf(cands[i].Path)
		edges := centrality[pkg]
		if edges == nil {
			edges = map[model.RelationID]struct{}{}
			centrality[pkg] = edges
		}
		for _, id := range r.admittedEdges {
			edges[id] = struct{}{}
		}
	}

	// Pass two needs the centrality map complete, so it cannot be folded into
	// the loop above: a candidate ranked first would otherwise see fewer edges
	// in its package than one ranked last.
	for i := range cands {
		cands[i].Paths = routed[i].paths
		cands[i].MorePaths = routed[i].morePaths
		base := originContribution(cands[i].Origin)
		if routed[i].best > base {
			base = routed[i].best
		}
		boosts, reasons := boostsFor(cands[i], routed[i], centrality)
		score := base + boosts
		if score > maxScoreMicros {
			score = maxScoreMicros
		}
		if score < 0 {
			score = 0
		}
		cands[i].ScoreMicros = score
		// The routes that were not enumerated are disclosed BEFORE the boost
		// reasons: appendReason drops silently at MaxReasonsPerEntry, and a
		// bounded explanation that lost this line reads as an unexplained
		// selection -- the one thing MorePaths exists to prevent.
		for _, reason := range append(morePathsReason(cands[i].MorePaths), routed[i].reasons...) {
			cands[i].Reasons = appendReason(cands[i].Reasons, reason)
		}
		for _, reason := range reasons {
			cands[i].Reasons = appendReason(cands[i].Reasons, reason)
		}
	}

	return cands, nil
}

// resolvePrecision maps every relation on every candidate route to its edge
// precision multiplier in ONE batched resolution pass. Precision lives on
// Evidence and not on Relation, so there is no per-edge read to loop over here;
// an edge with no visible evidence row takes the heuristic multiplier rather
// than dropping out of the ranking. Only sealed facts reach this map: it reads
// through the pinned reader, whose visibility join already excludes anything
// that is not a selected unit of the pinned generation.
//
// The id set is sorted and de-duplicated before the read, so the request the
// store sees is a function of the candidate set and not of discovery order. A
// set larger than one batch is chunked rather than truncated: a silently
// dropped edge would score a real route as heuristic.
// It is a thin wrapper: the body moved to rankjoin.go, beside the streamed pass
// that must reproduce its read log. Lane L5 removes this wrapper when rank
// stops calling it.
func (c *Compiler) resolvePrecision(ctx context.Context, reader *sqlite.PinnedReader,
	cands []candidate) (map[model.RelationID]int64, error) {
	return c.resolvePrecisionWholeSet(ctx, reader, cands)
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
			exclude(c, excludeNoFile)
		default:
			fv, ok := meta[c.FileID]
			if !ok {
				exclude(c, excludeInvisible)
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
		// The clip count is taken from phase two, not here: phase one measures
		// every candidate including the ones selection drops, so counting both
		// passes would double every selected entry and count entries the
		// manifest never carries.
		e, _, err := measureEntry(c, i, charged[c.FileID] == i)
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
		e, clipped, err := measureEntry(sized[i], ordinal, charged[sized[i].FileID] == i)
		if err != nil {
			return plan{}, err
		}
		out.RelationsClipped += clipped
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

func (c *Compiler) relationsOnPathsWholeSet(ctx context.Context, reader relationReader,
	cands []candidate) (map[model.RelationID]model.Relation, bool, error) {
	wanted := map[model.RelationID]struct{}{}
	nodes := make([]model.NodeID, 0, len(cands))
	seenNode := map[model.NodeID]struct{}{}
	for _, cand := range cands {
		for _, p := range cand.Paths {
			for _, rel := range p.Relations {
				wanted[rel] = struct{}{}
			}
		}
		if cand.NodeID == "" {
			continue
		}
		if _, dup := seenNode[cand.NodeID]; dup {
			continue
		}
		seenNode[cand.NodeID] = struct{}{}
		nodes = append(nodes, cand.NodeID)
	}
	out := make(map[model.RelationID]model.Relation, len(wanted))
	if len(wanted) == 0 || len(nodes) == 0 {
		return out, len(wanted) == 0, nil
	}

	limit := c.pageLimit()
	budget, bounded := c.edgeScanBudget()
	scanned := 0
	for start := 0; start < len(nodes) && len(out) < len(wanted); start += limit {
		batch := nodes[start:min(start+limit, len(nodes))]
		var after model.RelationID
		for len(out) < len(wanted) {
			page, err := reader.EdgesBatch(ctx, batch, model.DirectionBoth, scopeRelations, after, limit)
			if err != nil {
				return nil, false, err
			}
			for _, rel := range page {
				if _, want := wanted[rel.ID]; want {
					out[rel.ID] = rel
				}
				after = rel.ID
			}
			scanned += len(page)
			if len(page) < limit || (bounded && scanned >= budget) {
				break
			}
		}
		if bounded && scanned >= budget {
			break
		}
	}
	return out, len(out) == len(wanted), nil
}

// resolvePrecisionWholeSet is today's resolvePrecision (rank.go), moved here
// unchanged except for the reader type, for the same reason.
func (c *Compiler) resolvePrecisionWholeSet(ctx context.Context, reader relationReader,
	cands []candidate) (map[model.RelationID]int64, error) {
	seen := map[model.RelationID]struct{}{}
	ids := make([]model.RelationID, 0, len(cands))
	for _, cand := range cands {
		for _, p := range cand.Paths {
			for _, id := range p.Relations {
				if _, dup := seen[id]; dup || id == "" {
					continue
				}
				seen[id] = struct{}{}
				ids = append(ids, id)
			}
		}
	}
	out := make(map[model.RelationID]int64, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	batch := c.pageLimit()
	for start := 0; start < len(ids); start += batch {
		end := min(start+batch, len(ids))
		rows, err := reader.EvidenceBatch(ctx, ids[start:end], evidencePerRelation)
		if err != nil {
			return nil, contextErr(ctx, err)
		}
		for id, evidence := range rows {
			out[id] = mostPrecise(evidence)
		}
	}
	return out, nil
}

// boostsFor adds each Section 15.3 boost at most once and clamps their total at
// maxBoostMicros. Every boost is evidence-backed: the task identifier boost
// needs the task to have named the entity, the change boost needs a captured
// working-tree status, the association boost needs an admitted route that
// arrives over a test or contract edge, and centrality is bounded by the edges
// this compile actually walked.
func boostsFor(cand candidate, routed routeScore,
	centrality map[string]map[model.RelationID]struct{}) (int64, []string) {
	return boostsForCounts(cand, associatedTestOrContract(routed),
		int64(len(centrality[packageOf(cand.Path)])))
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

// groupByFile collects the sorted candidates into file-atomic groups, in the
// order their highest-ranked member appears. Group sizes sum the measured entry
// sizes, and measureEntry charges a file's source to its first entry alone, so
// a group's size is the file's real transport cost: its source once plus every
// selected entry's own metadata.
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
				p.dropped = appendDrops(p.dropped, g, dropSliceLimit)
				continue
			case files >= b.MaxFiles:
				p.dropped = appendDrops(p.dropped, g, dropFileLimit)
				continue
			case g.bytes > b.MaxBytes || g.tokens > b.MaxTokens:
				p.dropped = appendDrops(p.dropped, g, dropOversized)
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
				p.dropped = appendDrops(p.dropped, g, dropSliceLimit)
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

// appendDrops records every entry of an unselected group, so a file excluded
// through several symbols leaves one reasoned exclusion per symbol rather than
// one for the file and silence for the rest.
func appendDrops(dst []drop, g fileGroup, reason string) []drop {
	for _, i := range g.indexes {
		dst = append(dst, drop{index: i, reason: reason})
	}
	return dst
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

// less is the total Section 15.3 order: requirement rank, descending
// ScoreMicros, normalized path, start byte, entity id.
func (a candidate) less(b candidate) bool {
	if ra, rb := requirementRank(a.Requirement), requirementRank(b.Requirement); ra != rb {
		return ra < rb
	}
	if a.ScoreMicros != b.ScoreMicros {
		return a.ScoreMicros > b.ScoreMicros
	}
	if a.Path != b.Path {
		return a.Path < b.Path
	}
	if a.StartByte != b.StartByte {
		return a.StartByte < b.StartByte
	}
	// The last key is unique, so the order is total. A node identity wins over
	// the file identity: a file-level candidate carries no NodeID.
	return a.entityID() < b.entityID()
}

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

// plan is the Section 15.4 result: entries in Section 15.3 tie-break order with
// ordinals 0..n-1 and required_full as a prefix, slices that reference those
// ordinals, and one reasoned exclusion for every candidate not selected. The
// manifest lane persists it unchanged; nothing here is a wire shape.
type plan struct {
	Entries  []model.ContextEntry
	Slices   []model.ContextSlice
	Excluded []model.ExcludedContextEntry
	// RelationsClipped counts the routes whose relation list evidencePaths had
	// to cut to model.MaxRelationsPerPath across the SELECTED entries. It is a
	// count, not a per-entry list, for the reason scopeResult's two counters
	// are: the compiler turns it into one manifest notice, so a shortened route
	// never reads as the whole route.
	RelationsClipped int64
}

// drop records one candidate the packer could not fit, with the reason it is
// persisted as an exclusion. Every unselected candidate leaves one, so an
// omission is visible rather than silent (Section 15.4).
type drop struct {
	index  int
	reason string
}

// referenceCompile is Compile as this package ran it before the streamed passes
// replaced it: seeds, expandScope, hydrateFiles, relationsOnPaths, rank,
// resolveBudget, buildPlan, kept verbatim from the pre-stream compiler.go.
//
// It deliberately stops short of the two steps Compile does around the pipeline.
// The manifest-reuse lookup is skipped because the reference is compared against
// a Compile of the SAME store and a reference that reused would compare the
// streamed plan with itself; and persistence is skipped because the reference
// must not take the manifest identity the streamed compile is about to write.
// The caller persists nothing here and compares against what Compile stored.
func (c *Compiler) referenceCompile(ctx context.Context, req model.ContextRequest) (
	plan, bool, []string, model.Budget, error) {
	if err := req.Validate(); err != nil {
		return plan{}, false, nil, model.Budget{}, err
	}
	reader, err := c.store.PinGeneration(ctx, c.repo, req.GenerationID, c.cfg.Storage.QueryCursorTTL.Std())
	if err != nil {
		return plan{}, false, nil, model.Budget{}, err
	}
	defer reader.Close()
	gen := reader.Binding().GenerationID

	seeds, err := c.extractSeeds(ctx, reader, gen, req)
	if err != nil {
		return plan{}, false, nil, model.Budget{}, err
	}
	caps, err := reader.Capabilities(ctx)
	if err != nil {
		return plan{}, false, nil, model.Budget{}, err
	}
	engine, release, err := c.graph(ctx, gen)
	if err != nil {
		return plan{}, false, nil, model.Budget{}, err
	}
	defer release()

	scoped, err := expandScope(ctx, engine, gen, c.cfg.Context,
		append(append([]candidate(nil), seeds.Candidates...), seeds.Excluded...), caps)
	if err != nil {
		return plan{}, false, nil, model.Budget{}, err
	}
	scopeComplete := scoped.ScopeComplete && !seeds.Unresolved
	files, err := c.hydrateFiles(ctx, reader, scoped.Candidates)
	if err != nil {
		return plan{}, false, nil, model.Budget{}, err
	}
	relations, complete, err := c.relationsOnPaths(ctx, reader, scoped.Candidates)
	if err != nil {
		return plan{}, false, nil, model.Budget{}, err
	}
	if !complete {
		scopeComplete = false
	}
	ranked, err := c.rank(ctx, reader, scoped.Candidates, relations)
	if err != nil {
		return plan{}, false, nil, model.Budget{}, err
	}
	resolved, err := resolveBudget(req.Budget, c.cfg.Context)
	if err != nil {
		return plan{}, false, nil, model.Budget{}, err
	}
	packed, err := buildPlan(ranked, files, resolved)
	if err != nil {
		return plan{}, false, nil, model.Budget{}, err
	}
	stored := model.Budget{MaxEstimatedTokens: resolved.MaxTokens, MaxBytes: resolved.MaxBytes,
		MaxFiles: resolved.MaxFiles, MaxSlices: resolved.MaxSlices}
	return packed, scopeComplete, c.manifestNotices(scoped, partsOf(packed)), stored, nil
}

// parityRequests are the requests the parity proof compiles through both
// pipelines. They are chosen to reach every branch the streamed passes rewrote:
// an unbounded compile (every candidate survives to an entry), a compile whose
// byte budget drops files in the packer (the P-H drop stream), one whose file
// budget drops whole groups, one naming an identity the snapshot cannot resolve
// (the pre-sort exclusion stream and an incomplete scope), and a phase that
// scores a different requirement mix.
var parityRequests = []struct {
	name string
	req  model.ContextRequest
	// rels shapes the scope this row walks. Nil leaves the edgeless graph the
	// first six rows were written against; the rows below it exist because an
	// edgeless graph admits every candidate exactly once, folds nothing,
	// routes nothing and drops nothing, so four of the five plan-table
	// mutations produce the same plan through both pipelines and the proof
	// says nothing about them.
	rels func(*contextFixture) []model.Relation
}{
	{"unbounded", model.ContextRequest{Task: "make `Place` idempotent", Phase: model.PhaseVerify}, nil},
	{"sweep phase", model.ContextRequest{Task: "make `Place` idempotent", Phase: model.PhaseSweep}, nil},
	{"byte budget drops files", model.ContextRequest{Task: "make `Place` idempotent",
		Phase: model.PhaseVerify, Budget: model.Budget{MaxBytes: 4096, MaxSlices: 2}}, nil},
	{"file budget drops groups", model.ContextRequest{Task: "make `Place` idempotent",
		Phase: model.PhaseVerify, Budget: model.Budget{MaxFiles: 2}}, nil},
	{"unresolvable identity", model.ContextRequest{Task: "make `Place` and `NoSuchSymbol` idempotent",
		Phase: model.PhaseVerify, Budget: model.Budget{MaxFiles: 1}}, nil},
	{"sweep phase, a different seed", model.ContextRequest{Task: "order placement", Phase: model.PhaseSweep}, nil},

	// Two seeds whose expansions both reach `Repository`, at ONE hop from
	// `Place` and at TWO from `Handle`. The scope dedupe therefore sees the
	// same entity twice with DIFFERENT payloads, which is what makes the
	// min-sequence fold observable: a fold that kept the later arrival would
	// keep the other depth, and depth reaches the score, the ordinal and the
	// stored entry. An edgeless row cannot show this -- it admits nothing
	// twice -- and neither can two seeds at equal depth, whose two arrivals
	// fold to the same record whichever side survives.
	{"two seeds admit one entity at two depths", model.ContextRequest{
		Task: "make `Place` idempotent", Seeds: []string{"Place", "Handle"},
		Phase: model.PhaseVerify}, routedScope},
	// The same shaped scope under a file budget that drops whole groups, so
	// the packer emits a drop stream over more than one file and the order
	// those drops are persisted in is decided by rank rather than by file.
	{"routed scope, the file budget drops groups", model.ContextRequest{
		Task: "make `Place` idempotent", Seeds: []string{"Place", "Handle"},
		Phase: model.PhaseVerify, Budget: model.Budget{MaxFiles: 5}}, routedScope},
	// The same shaped scope under a byte budget, which drops inside a group
	// as well as between groups.
	{"routed scope, the byte budget drops files", model.ContextRequest{
		Task: "make `Place` idempotent", Seeds: []string{"Place", "Handle"},
		Phase: model.PhaseVerify, Budget: model.Budget{MaxBytes: 16384, MaxSlices: 4}}, routedScope},
	// Ruling C8: the relation-kind scan reads an UNLIMITED
	// context.max_graph_edges to exhaustion rather than as one page. Under a
	// scope of more than 200 wanted relations -- model.MaxPageItems is 200 --
	// a scan that stopped at one page would leave edges untyped, ranking
	// would score their routes as inadmissible, and the two pipelines would
	// disagree. The six rows above are all far under 200 relations, so none
	// of them can fail if the page bound comes back.
	{"a scope wanting more than one page of relations", model.ContextRequest{
		Task: "make `Place` idempotent", Seeds: []string{"Place", "Handle"},
		Phase: model.PhaseVerify}, denseScope},
}

// routedScope is the shaped scope of the rows above: `Handle` calls `Place`,
// `Place` calls `Repository` and is tested by `TestPlace`, documented by
// `docs/order.md` and configured by `config/order.toml`. Seeded from both
// `Place` and `Handle`, `Repository` is reached at two different depths and
// every other artifact is reached on a route, so routes, package centrality,
// multi-reason boosts and cross-file drops all have inputs.
func routedScope(fx *contextFixture) []model.Relation {
	return []model.Relation{
		fx.edge("internal/order/handler.go", model.RelCalls, "internal/order/service.go"),
		fx.edge("internal/order/service.go", model.RelCalls, "internal/order/ports.go"),
		fx.edge("internal/order/service.go", model.RelImplements, "internal/order/ports.go"),
		fx.edge("internal/order/service_test.go", model.RelTests, "internal/order/service.go"),
		fx.edge("docs/order.md", model.RelDocuments, "internal/order/service.go"),
		fx.edge("config/order.toml", model.RelConfigures, "internal/order/service.go"),
	}
}

// denseScope is routedScope plus enough further edges among the same seven
// nodes to take the wanted relation count past model.MaxPageItems. The kinds
// are drawn from model's own vocabulary rather than invented, and every edge
// is between two fixture nodes, so the walk stays over the published snapshot.
//
// The oversized artifact is left out of BOTH shaped scopes on purpose: an edge
// to it pulls it into the required scope, whose worst-case wire size alone
// exceeds the default byte budget, and every row would then report
// CTX_MINIMUM_BUDGET before a single pass compared anything. The floor itself
// already has its own coverage; these rows are about the plan.
// oversizedFixtureFile is the fixture's Section 15.4 artifact, named once here
// rather than repeated as a literal in the filter below.
const oversizedFixtureFile = "internal/order/generated.go"

func denseScope(fx *contextFixture) []model.Relation {
	kinds := []model.RelationKind{model.RelReferences, model.RelReads, model.RelWrites,
		model.RelDependsOn, model.RelDataFlowsTo, model.RelControlDependsOn, model.RelOwns,
		model.RelImports, model.RelExports, model.RelBuilds}
	out := routedScope(fx)
	for _, k := range kinds {
		for _, from := range fixtureFiles {
			for _, to := range fixtureFiles {
				if from.path == to.path || from.path == oversizedFixtureFile || to.path == oversizedFixtureFile {
					continue
				}
				out = append(out, fx.edge(from.path, k, to.path))
			}
		}
	}
	if len(out) <= model.MaxPageItems {
		panic("denseScope must declare more than one page of relations")
	}
	return out
}

// TestTheStreamedCompileIsByteForByteTheWholeSetPlan is C-STREAM proof (1).
//
// The streamed passes are only correct insofar as they reproduce the whole-set
// pipeline's answer, and every structure they replaced -- the admitted map, the
// hydration map, the relation map, the routed slice, the group index lists --
// was also the thing that decided an ordinal. A pass that drops a dedupe, folds
// on the wrong key or emits in the wrong order still produces a plausible plan,
// so the invariant is not "a plan came out" but "the SAME plan came out":
// entries with their ordinals, reasons and evidence routes, the slice table with
// its entry ordinals, every exclusion with its reason in its persisted order,
// the compile's notices, and the canonical hash the manifest identity is built
// on. Each is compared as canonical JSON, so a difference names the field.
func TestTheStreamedCompileIsByteForByteTheWholeSetPlan(t *testing.T) {
	for _, row := range parityRequests {
		t.Run(row.name, func(t *testing.T) {
			fx := newContextFixture(t)
			// Set before either compiler is built: intCompiler reads
			// fx.Rels when it composes the graph factory, and a row that
			// shaped the scope between the two would compare two scopes.
			if row.rels != nil {
				fx.Rels = row.rels(fx)
			}
			c := intCompiler(t, fx, fx.Now)
			ref, refScopeComplete, refNotices, refBudget, err := c.referenceCompile(fx.ctx, row.req)
			if err != nil {
				t.Fatalf("referenceCompile: %v", err)
			}
			m, err := intCompiler(t, fx, fx.Now).Compile(fx.ctx, row.req)
			if err != nil {
				t.Fatalf("Compile: %v", err)
			}
			entries, err := fx.Store.ManifestEntries(fx.ctx, m.ID, -1, 0)
			if err != nil {
				t.Fatalf("ManifestEntries: %v", err)
			}
			slices, err := fx.Store.ManifestSlices(fx.ctx, m.ID, -1, 0)
			if err != nil {
				t.Fatalf("ManifestSlices: %v", err)
			}
			excluded, err := fx.Store.ManifestExcluded(fx.ctx, m.ID, -1, 0)
			if err != nil {
				t.Fatalf("ManifestExcluded: %v", err)
			}
			// The plan must not be vacuously equal: a row that compiled to
			// nothing would pass every comparison below without exercising one
			// streamed pass.
			if len(entries) == 0 && len(excluded) == 0 {
				t.Fatalf("the compile produced neither an entry nor an exclusion, so this row proves nothing")
			}
			sameJSON(t, "entries", ref.Entries, entries)
			sameJSON(t, "slices", ref.Slices, slices)
			sameJSON(t, "exclusions", ref.Excluded, excluded)
			sameJSON(t, "notices", refNotices, m.Notices)
			if m.ScopeComplete != refScopeComplete {
				t.Errorf("scope_complete is %v, the whole-set pipeline says %v", m.ScopeComplete, refScopeComplete)
			}
			if m.EntryCount != len(ref.Entries) || m.SliceCount != len(ref.Slices) {
				t.Errorf("header counts %d entries / %d slices, the whole-set plan has %d / %d",
					m.EntryCount, m.SliceCount, len(ref.Entries), len(ref.Slices))
			}
			_, reqHash := manifestIdentity(fx.Binding, row.req, fx.Cfg)
			want := canonicalManifestHash(fx.Binding, reqHash, refBudget, refScopeComplete,
				ref.Entries, ref.Slices, ref.Excluded)
			if m.CanonicalHash != want {
				t.Errorf("canonical hash %s, the whole-set plan hashes to %s", m.CanonicalHash, want)
			}
		})
	}
}

// sameJSON compares two values by their canonical JSON encoding and reports the
// first difference as the encodings themselves, so a divergence names the field rather
// than the Go type.
func sameJSON(t *testing.T, what string, want, got any) {
	t.Helper()
	a, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal reference %s: %v", what, err)
	}
	b, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal streamed %s: %v", what, err)
	}
	if string(a) != string(b) {
		t.Errorf("%s differ\n whole-set: %s\n  streamed: %s", what, a, b)
	}
}
