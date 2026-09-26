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
	"errors"
	"fmt"
	"iter"
	"sort"
	"strings"

	"github.com/Sawmonabo/codectx/internal/config"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
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

// reasonPathLimit is the configured cap on stored explanation paths per entry.
//
// It does NOT clamp. model.MaxReasonPathsPerEntry is a report threshold, not a
// wire ceiling, so a configured value ABOVE it is honoured rather than silently
// replaced by the smaller one.
// The returned value keeps the config.Limit convention: zero is unlimited, and
// scoreRoutes stores every admissible route for it. What an entry still cannot
// enumerate is disclosed by MorePaths, so a bounded explanation never reads as
// an unexplained selection.
func (c *Compiler) reasonPathLimit() config.Limit {
	return c.cfg.Context.MaxReasonPathsPerEntry
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
	precision map[model.RelationID]int64, pathLimit config.Limit) (routeScore, error) {
	out := routeScore{kinds: map[model.RelationKind]struct{}{}}
	type scored struct {
		path         model.RelationPath
		contribution int64
	}
	admitted := make([]scored, 0, len(cand.Paths))
	dropped := int64(0)
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
		// route the manifest does not actually hold. It is counted as dropped
		// here, because MorePaths below is otherwise only the tail of what WAS
		// retainable and would understate the routes that exist.
		if len(p.Relations) > model.MaxRelationsPerPath {
			dropped++
			continue
		}
		admitted = append(admitted, scored{path: p, contribution: contribution})
	}
	if len(admitted) == 0 {
		// Every admissible route was too long to store. MorePaths still
		// discloses that routes exist, so a bounded explanation never reads as
		// an unexplained selection.
		out.morePaths = dropped
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
	// An unlimited path bound keeps every admissible route; config.Limit owns
	// the test, so there is no bare zero check here.
	keep := len(admitted)
	if pathLimit.Exceeded(int64(keep)) {
		keep = pathLimit.Int()
	}
	out.paths = make([]model.RelationPath, 0, keep)
	for _, s := range admitted[:keep] {
		out.paths = append(out.paths, s.path)
	}
	out.morePaths = dropped + int64(len(admitted)-keep)
	out.reasons = append(out.reasons, routeReason(cand, admitted[0].path, relations, out.best))
	return out, nil
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

// boostsForCounts is boostsFor over the two facts the route pass and the
// centrality aggregation reduce to: whether an admitted route arrived over a
// test or contract edge, and how many distinct edges this compile walked in the
// candidate's package. It is the ONE implementation of the boost set and of the
// order its reasons are appended in, so the whole-set pass and the streamed
// pass cannot drift: rank reaches it through boostsFor with the nested
// centrality map, and P-F reaches it with P-E's streamed count.
func boostsForCounts(cand candidate, associated bool, edges int64) (int64, []string) {
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
	if associated {
		total += boostAssociatedTest
		reasons = append(reasons, "an admitted route reaches it as a test or contract of the scope")
	}
	pkg := packageOf(cand.Path)
	if edges > 0 {
		boost := edges * centralityPerEdgeMicros
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

// isCapturedChange reports the Section 15.3 active-change statuses. They are
// the Section 15.2 step 6 admission set, read from the one changedStatuses
// definition rather than re-spelled here: a second list would let the boost and
// the seed step disagree about what a captured change is.
func isCapturedChange(s model.FileStatus) bool {
	for _, changed := range changedStatuses {
		if s == changed {
			return true
		}
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

// ---------------------------------------------------------------------------
// Streaming ranking (C-STREAM passes P-D and P-F)
// ---------------------------------------------------------------------------
//
// The streamed passes below hold plumbing only and no policy: they rebuild one
// candidate's working set from the sorted record streams and then call the very
// functions `rank` calls -- scoreRoutes, scorePath, originContribution,
// boostsForCounts, morePathsReason, appendReason. That is what makes the two
// paths equal by construction rather than by a second derivation of Section
// 15.3 that would have to be kept in step by hand.

// scoredRec is the P-D -> P-F pipe: one candidate after its routes are scored
// and before its boosts are known. It exists because P-F cannot run until P-E's
// per-package edge counts are complete (rank.go's "pass two needs the
// centrality map complete"), so the route result has to survive a sort.
//
// It carries only what P-F still needs from the route pass, which is why
// routeScore itself is not the record: `paths` already travel as their own
// retained pathRec/hopRec streams, `admittedEdges` are consumed into the
// centrality sort by P-D, and `reasons` are appended to Cand.Reasons by P-D --
// so nothing here is unbounded.
//
//   - Pkg is packageOf(PathAtRank), computed once by P-D. Ruling C3: the
//     centrality bucket and the boost reason read the AT-RANK path, while
//     lessRank reads PathFinal. Storing the key keeps the two apart.
//   - Base is max(originContribution(Origin), routed.best), the rank pass's
//     `base` before boosts.
//   - Associated is associatedTestOrContract(routed): the kind set exists only
//     to answer that one question, so the answer travels and the set does not.
type scoredRec struct {
	Cand candRec `json:"c"`
	Pkg  string  `json:"p,omitempty"`

	Base       int64 `json:"b,omitempty"`
	Associated bool  `json:"a,omitempty"`
}

// lessScoredPkg groups scored candidates by package so P-F merge-joins them
// against P-E's counts. A JOIN comparator in the sense of stream.go's note: the
// key is the package alone, and the merge's stability keeps a package's members
// in arrival order. No tie-break is needed or wanted, because P-F re-sorts every
// record into the ranked sort under lessRank, which is total.
func lessScoredPkg(a, b scoredRec) int { return cmpString(a.Pkg, b.Pkg) }

// sizeOfScored charges one buffered scored record: its candidate plus the
// package key it adds.
func sizeOfScored(r scoredRec) int64 {
	return sizeOfCand(r.Cand) + int64(len(r.Pkg)) + recordOverheadBytes
}

// pullRun turns a SortedRun into a pull iterator, so a pass can merge-join two
// or three sorted streams without buffering any of them. SortedRun only pushes
// (Each), and a merge-join needs to advance one side on demand.
//
// The walk error is reported by the final call, the one that reports the stream
// exhausted: iter.Pull runs the sequence only as far as each yield, so Each has
// returned exactly when ok is false. The caller must call stop.
func pullRun[T any](r *pagination.SortedRun[T]) (next func() (T, bool, error), stop func()) {
	var walkErr error
	pullNext, pullStop := iter.Pull(func(yield func(T) bool) {
		walkErr = r.Each(func(v T) error {
			if !yield(v) {
				return errStopRun
			}
			return nil
		})
		if errors.Is(walkErr, errStopRun) {
			walkErr = nil
		}
	})
	return func() (T, bool, error) {
		v, ok := pullNext()
		return v, ok, walkErr
	}, pullStop
}

// routeWorkingSet is one candidate's rebuilt routes plus the two lookup maps
// scoreRoutes and scorePath read. It is the per-candidate working set the plan
// bounds P-D's heap by: it holds one candidate's hops and nothing else.
type routeWorkingSet struct {
	paths     []model.RelationPath
	relations map[model.RelationID]model.Relation
	precision map[model.RelationID]int64
}

// rebuildRoutes reassembles one candidate's Paths from the contiguous
// (Seq, PathIdx) route run and (Seq, PathIdx, HopIdx) hop run, in exactly the
// order today's candidate.Paths carries them, and derives the relation and
// precision maps from the same hops.
//
// A hop run shorter than the route's HopCount is a defect and not a shorter
// route: silently scoring a truncated route would claim a contribution for a
// path the compile never walked.
func rebuildRoutes(seq int64, routes []pathRec, hops []hopRec) (routeWorkingSet, error) {
	ws := routeWorkingSet{
		relations: make(map[model.RelationID]model.Relation, len(hops)),
		precision: make(map[model.RelationID]int64, len(hops)),
	}
	byPath := make(map[int32][]hopRec, len(routes))
	for _, h := range hops {
		byPath[h.PathIdx] = append(byPath[h.PathIdx], h)
		// The heuristic floor is applied on READ and not by the producer, so
		// the value is today's precision whether the attribute join wrote the
		// raw evidence multiplier or the already-floored one: mostPrecise never
		// answers below the floor and an unresolved edge takes it
		// (rank.go:324-327), so the maximum is idempotent either way.
		if m := (relAttrRec{Multiplier: h.Multiplier}).precision(); m > ws.precision[h.RelationID] {
			ws.precision[h.RelationID] = m
		}
		// A hop whose kind the attribute join could not resolve still occupies
		// the map: scorePath's `known` lookup then succeeds and the missing
		// contribution weight reports the route inadmissible, which is the same
		// verdict today's missing-kind branch reaches.
		if rel, ok := ws.relations[h.RelationID]; !ok || (rel.Kind == "" && h.Kind != "") {
			ws.relations[h.RelationID] = model.Relation{ID: h.RelationID, Kind: h.Kind}
		}
	}
	ws.paths = make([]model.RelationPath, 0, len(routes))
	for _, r := range routes {
		run := byPath[r.PathIdx]
		if int32(len(run)) != r.HopCount {
			return routeWorkingSet{}, &model.Error{Code: model.CodeInternal,
				Message: fmt.Sprintf("a streamed context compile read %d hop(s) for route %d of candidate %d, which carries %d",
					len(run), r.PathIdx, seq, r.HopCount)}
		}
		ids := make([]model.RelationID, 0, len(run))
		for _, h := range run {
			ids = append(ids, h.RelationID)
		}
		ws.paths = append(ws.paths, model.RelationPath{
			Relations: ids, Evidence: r.Evidence, CostUnits: r.CostUnits,
		})
	}
	return ws, nil
}
