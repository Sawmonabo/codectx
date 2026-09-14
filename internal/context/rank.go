// This file is owned by Task 15 lane L3. It holds
// Section 15.3 integer ranking: path contributions, per-edge precision, depth decay, bounded boosts and the total tie-break order.
//
// The shared contract it builds on (candidate, the ranking constants and the
// typed error constructors) is frozen in compiler.go and is not edited here.
//
// Section 15.3's score is NOT Section 14.3's traversal ranking. graph.Cost and
// the impact score 1_000_000/(1+cost) answer "how far did the walk travel";
// this pass answers "how much does this entity inform the task". Recomputing
// here is what the spec asks for, not a duplicate implementation, so this file
// never calls into internal/graph.
package context

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/storage/sqlite"
)

// centralityPerEdgeMicros is the package-centrality boost contributed by one
// distinct admitted edge touching a candidate's package in THIS compile. The
// input is deliberately walk-local rather than whole-graph centrality, which
// would make a manifest depend on parts of the repository the request never
// reached. It is an internal constant and not a configuration key: it belongs
// to the frozen code-side policy that compilerPolicyVersion labels, so changing
// it must change that label rather than one user's config. Ten distinct edges
// saturate boostCentralityMax.
const centralityPerEdgeMicros int64 = boostCentralityMax / 10

// evidencePerRelation bounds the evidence rows resolved per admitted edge.
// Precision is picked as the most precise row in that bounded, evidence-id
// ordered prefix, so the multiplier is a deterministic function of the pinned
// generation rather than of how many occurrences a provider happened to seal.
const evidencePerRelation = 8

// maxScoreMicros clamps the Section 15.3 sum. The largest honest score is an
// explicit seed carrying every bounded boost; anything above it would be an
// arithmetic defect presented as a ranking.
const maxScoreMicros = seedContribution + maxBoostMicros

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

// morePathsReason is the bounded disclosure of routes the manifest admitted but
// could not enumerate (Section 15.3: "report additional-path counts rather than
// growing an exponential path list"). model.ContextEntry carries reasons and
// evidence paths and nothing else, so a reason is the only channel the count
// has; without it the retained paths would read as the whole story.
func morePathsReason(more int64) []string {
	if more <= 0 {
		return nil
	}
	return []string{fmt.Sprintf("%d further route(s) reach this entity and are not enumerated", more)}
}

// reasonPathLimit is the configured cap on stored explanation paths per entry,
// floored at the model bound so a zero or oversized config value cannot make a
// manifest that PutManifest rejects.
func (c *Compiler) reasonPathLimit() int {
	limit := c.cfg.Context.MaxReasonPathsPerEntry
	if limit <= 0 || limit > model.MaxReasonPathsPerEntry {
		return model.MaxReasonPathsPerEntry
	}
	return limit
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
func (c *Compiler) resolvePrecision(ctx context.Context, reader *sqlite.PinnedReader,
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

// mostPrecise picks the multiplier of the most precise evidence row backing one
// edge. The most precise occurrence, not the first one read, decides: several
// providers may seal the same edge, and taking whichever the id order happened
// to put first would let an added heuristic occurrence lower an edge that a
// compiler already proved.
func mostPrecise(evidence []sqlite.StoredEvidence) int64 {
	best := precisionMultiplier[model.PrecisionHeuristic]
	for _, e := range evidence {
		if m, ok := precisionMultiplier[e.Evidence.Precision]; ok && m > best {
			best = m
		}
	}
	return best
}

// routeScore is the per-candidate result of the route pass: the maximum
// admitted contribution, the bounded routes retained to explain it, the routes
// admitted but not enumerated, and the distinct edges the candidate contributed
// to its package's walk-local centrality.
type routeScore struct {
	best          int64
	paths         []model.RelationPath
	morePaths     int64
	admittedEdges []model.RelationID
	kinds         map[model.RelationKind]struct{}
	reasons       []string
}

// scoreRoutes scores every route reaching cand and keeps the maximum. Section
// 15.3 aggregates by maximum and never by a sum over routes: graph.Impact
// admits one parent per node, so the maximum IS the admitted route, and summing
// would let a cycle inflate an entity above an entity that genuinely matters.
func scoreRoutes(cand candidate, relations map[model.RelationID]model.Relation,
	precision map[model.RelationID]int64, pathLimit int) (routeScore, error) {
	out := routeScore{kinds: map[model.RelationKind]struct{}{}}
	type scored struct {
		path         model.RelationPath
		contribution int64
	}
	admitted := make([]scored, 0, len(cand.Paths))
	for _, p := range cand.Paths {
		contribution, kinds, ok, err := scorePath(p, relations, precision)
		if err != nil {
			return routeScore{}, err
		}
		if !ok {
			continue
		}
		if contribution > out.best {
			out.best = contribution
		}
		for _, k := range kinds {
			out.kinds[k] = struct{}{}
		}
		out.admittedEdges = append(out.admittedEdges, p.Relations...)
		// A route longer than the stored bound still scores and still counts,
		// but it cannot be retained: a truncated explanation path would claim a
		// route the manifest does not actually hold.
		if len(p.Relations) <= model.MaxRelationsPerPath {
			admitted = append(admitted, scored{path: p, contribution: contribution})
		}
	}
	if len(admitted) == 0 {
		// Every admissible route was too long to store. MorePaths still
		// discloses that routes exist, so a bounded explanation never reads as
		// an unexplained selection.
		out.morePaths = int64(countAdmitted(cand, relations, precision))
		return out, nil
	}
	sort.SliceStable(admitted, func(i, j int) bool {
		if admitted[i].contribution != admitted[j].contribution {
			return admitted[i].contribution > admitted[j].contribution
		}
		if len(admitted[i].path.Relations) != len(admitted[j].path.Relations) {
			return len(admitted[i].path.Relations) < len(admitted[j].path.Relations)
		}
		return admitted[i].path.Relations[0] < admitted[j].path.Relations[0]
	})
	keep := min(pathLimit, len(admitted))
	out.paths = make([]model.RelationPath, 0, keep)
	for _, s := range admitted[:keep] {
		out.paths = append(out.paths, s.path)
	}
	out.morePaths = int64(len(admitted) - keep)
	out.reasons = append(out.reasons, routeReason(cand, admitted[0].path, relations, out.best))
	return out, nil
}

// countAdmitted reports how many routes were admissible when none could be
// retained, so MorePaths still discloses that routes exist.
func countAdmitted(cand candidate, relations map[model.RelationID]model.Relation,
	precision map[model.RelationID]int64) int {
	n := 0
	for _, p := range cand.Paths {
		if _, _, ok, err := scorePath(p, relations, precision); ok && err == nil {
			n++
		}
	}
	return n
}

// scorePath is the Section 15.3 path contribution: the product over the route's
// hops of weight(kind) * precision(edge), decayed once per hop after the first.
// It reports the route inadmissible rather than free when a hop names a
// relation this compile did not walk or a kind that carries no contribution --
// a missing kind scored as 1.0 would rank an unexplained route above an
// explained one.
func scorePath(p model.RelationPath, relations map[model.RelationID]model.Relation,
	precision map[model.RelationID]int64) (int64, []model.RelationKind, bool, error) {
	if len(p.Relations) == 0 {
		return 0, nil, false, nil
	}
	contribution := scale
	kinds := make([]model.RelationKind, 0, len(p.Relations))
	for i, id := range p.Relations {
		rel, known := relations[id]
		if !known {
			return 0, nil, false, nil
		}
		weight, carries := contributionWeight[rel.Kind]
		if !carries {
			return 0, nil, false, nil
		}
		edge, resolved := precision[id]
		if !resolved {
			edge = precisionMultiplier[model.PrecisionHeuristic]
		}
		hop, err := mulScaled(weight, edge)
		if err != nil {
			return 0, nil, false, err
		}
		if contribution, err = mulScaled(contribution, hop); err != nil {
			return 0, nil, false, err
		}
		if i > 0 {
			if contribution, err = mulScaled(contribution, depthDecay); err != nil {
				return 0, nil, false, err
			}
		}
		kinds = append(kinds, rel.Kind)
	}
	return contribution, kinds, true, nil
}

// originContribution is the non-relational contribution of the Section 15.2
// step that discovered a candidate. Section 15.3 names exactly two: the
// explicit seed and the exact symbol or path match. A candidate discovered
// lexically or as a captured change therefore contributes nothing on its own
// and is ranked by the routes that reach it plus its bounded boosts -- a
// captured change is already worth boostActiveChange, and inventing a weight
// for a lexical hit would put a number in the ranking that Section 15.3 does
// not state.
func originContribution(o originKind) int64 {
	switch o {
	case originExplicitSeed:
		return seedContribution
	case originBacktick, originPathToken, originQualified, originExactResolve:
		return exactContribution
	}
	return 0
}

// namedByTask reports whether the task text named this candidate verbatim: the
// Section 15.2 steps that read the task string itself, as opposed to the
// caller's explicit seeds, a lexical match or a captured change.
func namedByTask(o originKind) bool {
	switch o {
	case originBacktick, originPathToken, originQualified, originExactResolve:
		return true
	}
	return false
}

// boostsFor adds each Section 15.3 boost at most once and clamps their total at
// maxBoostMicros. Every boost is evidence-backed: the task identifier boost
// needs the task to have named the entity, the change boost needs a captured
// working-tree status, the association boost needs an admitted route that
// arrives over a test or contract edge, and centrality is bounded by the edges
// this compile actually walked.
func boostsFor(cand candidate, routed routeScore,
	centrality map[string]map[model.RelationID]struct{}) (int64, []string) {
	var total int64
	var reasons []string
	if namedByTask(cand.Origin) {
		total += boostTaskIdentifier
		reasons = append(reasons, "the task names this entity exactly")
	}
	if isCapturedChange(cand.Status) {
		total += boostActiveChange
		reasons = append(reasons, fmt.Sprintf("the working tree has a captured change (%s)", cand.Status))
	}
	if associatedTestOrContract(routed) {
		total += boostAssociatedTest
		reasons = append(reasons, "an admitted route reaches it as a test or contract of the scope")
	}
	pkg := packageOf(cand.Path)
	if edges := len(centrality[pkg]); edges > 0 {
		boost := int64(edges) * centralityPerEdgeMicros
		if boost > boostCentralityMax {
			boost = boostCentralityMax
		}
		total += boost
		reasons = append(reasons, fmt.Sprintf("this compile walked %d distinct edges in %s", edges, packageLabel(pkg)))
	}
	if total > maxBoostMicros {
		total = maxBoostMicros
	}
	return total, reasons
}

// isCapturedChange reports the Section 15.3 active-change statuses.
func isCapturedChange(s model.FileStatus) bool {
	switch s {
	case model.FileModified, model.FileAdded, model.FileUntracked:
		return true
	}
	return false
}

// associatedTestOrContract reports whether the candidate is an associated test
// or contract of the scope: an admitted route arrives over a tests, implements,
// overrides or extends edge. The trigger is the edge and never the node kind,
// so a file merely named like a test cannot claim the boost without a sealed
// relation saying it tests something in scope.
func associatedTestOrContract(routed routeScore) bool {
	for _, k := range []model.RelationKind{model.RelTests, model.RelImplements, model.RelOverrides, model.RelExtends} {
		if _, ok := routed.kinds[k]; ok {
			return true
		}
	}
	return false
}

// packageOf is the walk-local package key: the directory holding the
// candidate's normalized path. It is derived from the path rather than from a
// stored package node so that a file-level candidate, which names no node, is
// still attributed to the package its siblings score in.
func packageOf(path string) string {
	if i := strings.LastIndex(path, "/"); i >= 0 {
		return path[:i]
	}
	return ""
}

// packageLabel names the repository root readably in a reason.
func packageLabel(pkg string) string {
	if pkg == "" {
		return "the repository root"
	}
	return pkg
}

// routeReason explains the winning route in one bounded sentence: the boundary
// it entered over and the score it carried, never the name of the engine that
// walked it (Section 11.6).
func routeReason(cand candidate, best model.RelationPath,
	relations map[model.RelationID]model.Relation, contribution int64) string {
	kind := model.RelationKind("")
	if rel, ok := relations[best.Relations[0]]; ok {
		kind = rel.Kind
	}
	return fmt.Sprintf("reached over %d %s hop(s) at depth %d, contributing %d micros",
		len(best.Relations), kind, cand.Depth, contribution)
}

// appendReason keeps the explanation inside the Section 15.3 bounds that
// model.ContextEntry validates: at most MaxReasonsPerEntry reasons of at most
// MaxReasonBytes each, with no duplicate. A reason dropped here is dropped
// because the entry is already fully explained, never because it was silently
// truncated into something else.
func appendReason(reasons []string, reason string) []string {
	reason = strings.TrimSpace(reason)
	if reason == "" || len(reasons) >= model.MaxReasonsPerEntry {
		return reasons
	}
	if len(reason) > model.MaxReasonBytes {
		return reasons
	}
	for _, existing := range reasons {
		if existing == reason {
			return reasons
		}
	}
	return append(reasons, reason)
}
