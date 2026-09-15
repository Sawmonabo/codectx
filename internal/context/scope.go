// This file is owned by Task 15 lane L2. It holds
// Section 15.2 required-scope expansion over graph.Engine.Impact, the boundary-to-relation allowlist and the ScopeComplete rules.
//
// The shared contract it builds on (candidate, the ranking constants and the
// typed error constructors) is frozen in compiler.go and is not edited here.
package context

import (
	"context"
	"sort"

	"github.com/Sawmonabo/codectx/internal/config"
	"github.com/Sawmonabo/codectx/internal/graph"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
)

// scopeRelations is the Section 15.2 boundary allowlist, expressed as the
// relation kinds that reach each boundary: callers and callees (calls),
// contracts and types (implements, extends, overrides, defines), state
// ownership (reads, writes, data_flows_to, control_depends_on), dependencies
// (depends_on, imports), configuration (configures), tests (tests),
// documentation (documents) and integration or registration boundaries
// (references, exports).
//
// Containment kinds are deliberately absent: `contains` and `owns` express
// "same package or module", which Section 15.3 scores as a weak ranking
// contribution, not a boundary a decision depends on. Expanding over them would
// pull a whole package into required scope through one seed.
var scopeRelations = []model.RelationKind{
	model.RelCalls,
	model.RelImplements, model.RelExtends, model.RelOverrides, model.RelDefines,
	model.RelReads, model.RelWrites, model.RelDataFlowsTo, model.RelControlDependsOn,
	model.RelDependsOn, model.RelImports,
	model.RelConfigures,
	model.RelTests,
	model.RelDocuments,
	model.RelReferences, model.RelExports,
}

// scopeResult is what the expansion pass hands to ranking: the seeds with their
// requirements assigned plus every boundary the walk admitted, the capability
// rows that made the answer less than complete, and the ScopeComplete verdict.
//
// Exclusions are not a separate list: a candidate whose Excluded reason is set
// is the excluded record (compiler.go), so an omission cannot be dropped on the
// way from one pass to the next.
type scopeResult struct {
	Candidates []candidate
	// Completeness holds the non-fresh capability rows behind ScopeComplete,
	// deduplicated and ordered, for the manifest header.
	Completeness []model.CapabilityState
	// ScopeComplete is write-once-false: every assignment below sets it false
	// and none sets it true, so no later pass can repair a truncated walk, a
	// degraded capability or an unresolved seed by scoring around it.
	ScopeComplete bool
	// ReasonsDropped and ReasonsTruncated count the Section 15.3 explanation
	// cuts boundReasons applied across the whole expansion. They are COUNTS and
	// not one notice per entry: an entry-sized disclosure list is
	// repository-sized in heap, which is the shape this wave removes, while the
	// two counts are what a caller deciding whether an explanation is whole
	// actually needs. The compiler turns them into manifest notices.
	ReasonsDropped   int64
	ReasonsTruncated int64
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

// boundaryRequirement is how much of an admitted boundary the actor must read.
// The relation allowlist decides WHICH boundaries are admitted; the node kind
// decides HOW MUCH of one, because that is a property of the artifact rather
// than of the edge that reached it: a contract, a test and a configuration file
// are only meaningful whole, while a calling function is meaningful at its own
// symbol.
//
// Distance weakens the claim: a boundary d hops out is demoted d-1 ranks, so
// the second hop of a dependency chain is recommended rather than required.
// Demotion is by DISTANCE only — never by score, which Section 15.4 forbids.
func boundaryRequirement(kind model.NodeKind, depth int) model.Requirement {
	rank := requirementRank(baseRequirement(kind))
	if depth > 1 {
		rank += depth - 1
	}
	if rank > requirementRank(model.RequirementOptional) {
		rank = requirementRank(model.RequirementOptional)
	}
	return requirementByRank[rank]
}

// requirementByRank inverts requirementRank (compiler.go) so a demotion is
// arithmetic on the single ordering the tie-break chain already uses, rather
// than a second requirement ladder that could drift from it.
var requirementByRank = [...]model.Requirement{
	model.RequirementFull, model.RequirementSymbol,
	model.RequirementRecommended, model.RequirementOptional,
}

// baseRequirement is the requirement of a boundary reached in one hop.
func baseRequirement(kind model.NodeKind) model.Requirement {
	switch kind {
	case model.NodeInterface, model.NodeClass, model.NodeStruct, model.NodeEnum,
		model.NodeTest, model.NodeConfiguration, model.NodeFile:
		// Contracts, types, tests and configuration are read whole: half a
		// contract is a misreading of it, and Section 15.4 forbids serving a
		// partial artifact as if it were complete.
		return model.RequirementFull
	case model.NodeFunction, model.NodeMethod, model.NodeField, model.NodeVariable,
		model.NodeConstant, model.NodeEndpoint, model.NodeDatabaseEntity:
		// A caller, a callee or a state owner is required, at its own symbol.
		return model.RequirementSymbol
	case model.NodeDocument, model.NodePackage, model.NodeModule, model.NodeNamespace,
		model.NodeDependency, model.NodeBuildTarget:
		return model.RequirementRecommended
	}
	// An unknown kind informs rather than binds: it is never silently required.
	return model.RequirementOptional
}

// boundReasons clamps an entry's explanation to the Section 15.3 caps and
// returns what it had to cut. The engine already bounds them, so this guards a
// future engine change from producing a manifest that
// model.ContextEntry.Validate would reject at persistence time, one pass too
// late to explain itself.
//
// The cut is no longer silent. Both halves are counted and surfaced as manifest
// notices by the compiler: an explanation shortened without saying so reads as
// the whole reason an entity was selected. The flag does NOT ride on the
// entry's own Reasons -- the caps this function enforces are exactly the ones
// ContextEntry.Validate checks, so appending a disclosure there would produce
// MaxReasonsPerEntry+1 reasons and turn a disclosed clamp into a failed
// compile. The per-reason cut goes through model.TruncateField, which never
// leaves a partial UTF-8 sequence behind as the raw slice did.
func boundReasons(reasons []string) (out []string, dropped, truncated int64) {
	if len(reasons) > model.MaxReasonsPerEntry {
		dropped = int64(len(reasons) - model.MaxReasonsPerEntry)
		reasons = reasons[:model.MaxReasonsPerEntry]
	}
	out = make([]string, 0, len(reasons))
	for _, r := range reasons {
		bounded, original := model.TruncateField(r, model.MaxReasonBytes)
		if original > len(bounded) {
			truncated++
		}
		out = append(out, bounded)
	}
	return out, dropped, truncated
}

// boundPaths applies context.max_reason_paths_per_entry to the routes an
// expansion entry arrives with, and returns the number it could not carry so
// the caller discloses them rather than losing them.
//
// It no longer floors the setting at model.MaxReasonPathsPerEntry. That
// constant is a report threshold, not a wire ceiling: rank.go honours a
// configured value above it, and re-clamping to 3 here would silently undo
// the operator's setting one lane later -- the class-G shape this wave removes.
// config.Limit owns the test, so unlimited keeps every route.
func boundPaths(paths []model.RelationPath, max config.Limit) ([]model.RelationPath, int64) {
	if !max.Exceeded(int64(len(paths))) {
		return append([]model.RelationPath(nil), paths...), 0
	}
	keep := max.Int()
	return append([]model.RelationPath(nil), paths[:keep]...), int64(len(paths) - keep)
}

// degradedCapabilities is every capability row that is not fresh, from the
// pinned report and from the walk's own disclosure, deduplicated on the
// (provider, capability, scope) identity and ordered so two compiles of the
// same generation produce byte-identical manifest headers.
func degradedCapabilities(reported, disclosed []model.CapabilityState) []model.CapabilityState {
	seen := make(map[[3]string]bool)
	out := make([]model.CapabilityState, 0, len(reported)+len(disclosed))
	for _, group := range [][]model.CapabilityState{reported, disclosed} {
		for _, c := range group {
			if c.State == model.CapabilityFresh {
				continue
			}
			key := [3]string{c.ProviderID, c.Capability, c.Scope}
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.ProviderID != b.ProviderID {
			return a.ProviderID < b.ProviderID
		}
		if a.Capability != b.Capability {
			return a.Capability < b.Capability
		}
		return a.Scope < b.Scope
	})
	if len(out) == 0 {
		return nil
	}
	return out
}

// ---------------------------------------------------------------------------
// C-STREAM pass P-A: the streamed expansion (lane L1)
// ---------------------------------------------------------------------------

// ingested is what pass P-A hands the rest of the streamed pipeline: the one
// candidate spool in admission order, the two route streams keyed by the same
// seq, and the scope verdict WITHOUT its Candidates slice -- the slice is the
// repo-sized structure this pass exists to remove.
//
// Excluded candidates are on Cands, not diverted: relationsOnPaths deliberately
// does not filter on Excluded while hydrateFiles does, and P-I derives the
// exclusion projection by replaying this spool in seq order (plan Section 2
// P-A, ruling C1). Every run is registered with the compile's sort area, so all
// three are released by compileSorts.Close on every exit path.
type ingested struct {
	Cands *pagination.SortedRun[candRec]
	Paths *pagination.SortedRun[pathRec]
	Hops  *pagination.SortedRun[hopRec]
	Scope scopeResult
}

// lessCandSeq orders a candidate stream by ingest sequence: the admission order
// expandScope appends in today, and the order every later pass replays the
// spool in (P-B's batches, P-I's exclusion projection). It is total because seq
// is assigned once per ingested candidate and never reused.
//
// L0 froze no seq-primary candRec comparator -- lessRank is score-primary and
// lessFileIndex file-primary -- so this is the shared one; P-C and P-I replay
// seq-ordered candidate streams and must use it rather than each defining its
// own.
func lessCandSeq(a, b candRec) int { return cmpInt(a.Seq, b.Seq) }

// expandScopeStream is expandScope as sorted streams. It produces the same
// candidates, in the same admission order, with the same scope verdict, holding
// one sort run buffer per sort instead of res.Candidates and
// `admitted map[string]bool` (ruling C2).
//
// The dedupe is two folds and no join. Seeds that participate in today's
// dedupe (resolved, and carrying an entity identity) go through a first
// lessEntityID/foldMinSeq sort so that `start` can be built from the survivors
// before the walk runs; those survivors and every impact entry then go through
// a second one. foldMinSeq over that union reproduces BOTH of today's dedupe
// sites at once: a seed survivor's seq is always smaller than any impact
// entry's, so a boundary the walk reaches again keeps its seed requirement
// (scope.go:204), and two impact entries fold to the earlier one (scope.go:209).
// Re-folding an already-folded seed survivor is idempotent.
//
// Seeds that do NOT participate today -- an unresolved token, and a seed
// carrying neither a node nor a file identity -- are added straight to the
// output sort, exactly as today's loop appends them without consulting the map.
//
// Seq is the seed's index for a seed and len(seeds)+j for the j-th impact
// entry, which is why the route emission below can index impact.Entries
// directly instead of joining against it.
func (c *Compiler) expandScopeStream(ctx context.Context, s *compileSorts, eng *graph.Engine,
	gen model.GenerationID, seeds []candidate, caps []model.CapabilityState) (*ingested, error) {
	if s == nil {
		return nil, argumentInvalid("a streamed scope expansion requires an open sort area")
	}
	if eng == nil {
		return nil, argumentInvalid("scope expansion requires a graph engine")
	}
	if gen == 0 {
		return nil, argumentInvalid("scope expansion requires an explicit pinned generation")
	}
	cfg := c.cfg.Context

	seedSort, err := newSort[candRec](s, "scope-seed", lessEntityID, sizeOfCand)
	if err != nil {
		return nil, err
	}
	seedSort = seedSort.WithFold(foldMinSeq)
	seedSeqSort, err := newSort[candRec](s, "scope-seedseq", lessCandSeq, sizeOfCand)
	if err != nil {
		return nil, err
	}
	entitySort, err := newSort[candRec](s, "scope-entity", lessEntityID, sizeOfCand)
	if err != nil {
		return nil, err
	}
	entitySort = entitySort.WithFold(foldMinSeq)
	candSort, err := newSort[candRec](s, "scope-cand", lessCandSeq, sizeOfCand)
	if err != nil {
		return nil, err
	}

	out := &ingested{Scope: scopeResult{ScopeComplete: true}}
	resolved := 0
	for i := range seeds {
		sd := seeds[i]
		seq := int64(i)
		if sd.Excluded != "" {
			if sd.Origin == originExplicitSeed {
				return nil, scopeIncomplete(sd.Path)
			}
			// An unresolved extracted token stays visible as an exclusion and
			// carries the answer down to discovery rather than out of it.
			out.Scope.ScopeComplete = false
			if err := candSort.Add(candRecOf(sd, seq)); err != nil {
				return nil, err
			}
			continue
		}
		resolved++
		sd.Depth = 0
		if sd.Requirement == "" {
			// Every Section 15.2 seed producer assigns its own requirement; the
			// default stands only for a seed that carries none, which would
			// otherwise rank below optional and be droppable. Today this runs
			// after the dedupe skip, which only ever discards the record it
			// would have applied to, so applying it before the fold is the same.
			sd.Requirement = model.RequirementFull
		}
		if sd.entityID() == "" {
			// Today's guard is `if id := s.entityID(); id != ""`: a seed with no
			// identity never enters the map and so is never a duplicate.
			if err := candSort.Add(candRecOf(sd, seq)); err != nil {
				return nil, err
			}
			continue
		}
		if err := seedSort.Add(candRecOf(sd, seq)); err != nil {
			return nil, err
		}
	}

	seedRun, err := seedSort.Sorted()
	if err != nil {
		return nil, err
	}
	trackRun(s, seedRun)
	if err := seedRun.Each(func(r candRec) error { return seedSeqSort.Add(r) }); err != nil {
		return nil, err
	}
	seedSeqRun, err := seedSeqSort.Sorted()
	if err != nil {
		return nil, err
	}
	trackRun(s, seedSeqRun)

	// start is built from the deduped survivors in seq order, never at ingest:
	// today a duplicate seed returns before the start logic, so counting it
	// here would both reorder the walk's roots and mis-trigger the overflow
	// disclosure below.
	start := make([]model.NodeID, 0, model.MaxStartNodes)
	if err := seedSeqRun.Each(func(r candRec) error {
		if err := entitySort.Add(r); err != nil {
			return err
		}
		if r.NodeID == "" {
			return nil
		}
		if len(start) < model.MaxStartNodes {
			start = append(start, r.NodeID)
			return nil
		}
		// More seeds than one bounded walk may start from: the boundaries of
		// the seeds that did not start are unexplored, and the answer says so
		// rather than reading as an exhaustive scope.
		out.Scope.ScopeComplete = false
		return nil
	}); err != nil {
		return nil, err
	}

	switch {
	case resolved == 0:
		// Discovery: nothing resolved, so nothing is required and no walk is
		// run. Seeds already carry their own exclusion reasons.
		out.Scope.ScopeComplete = false
		return finishIngest(s, out, entitySort, candSort, nil, 0, cfg.MaxReasonPathsPerEntry)
	case len(start) == 0:
		// Files resolved but no symbol did, so no boundary can be walked from
		// them. Each seed keeps the requirement its Section 15.2 step assigned;
		// the scope is not complete.
		out.Scope.ScopeComplete = false
		out.Scope.Completeness = degradedCapabilities(caps, nil)
		return finishIngest(s, out, entitySort, candSort, nil, 0, cfg.MaxReasonPathsPerEntry)
	}

	impact, err := eng.Impact(ctx, model.ImpactRequest{
		GenerationID: gen,
		Start:        start,
		Relations:    scopeRelations,
		Direction:    model.DirectionBoth,
		MaxDepth:     cfg.MaxGraphDepth.Int(),
		MaxVisited:   cfg.MaxVisitedNodes.Int(),
		MaxEdges:     cfg.MaxGraphEdges.Int(),
	})
	if err != nil {
		return nil, contextErr(ctx, err)
	}
	if impact.Meta.Truncated {
		out.Scope.ScopeComplete = false
	}
	out.Scope.Completeness = degradedCapabilities(caps, impact.Meta.Completeness)
	if len(out.Scope.Completeness) > 0 {
		out.Scope.ScopeComplete = false
	}

	impactBase := int64(len(seeds))
	for j := range impact.Entries {
		e := impact.Entries[j]
		cand := candidate{
			NodeID: e.NodeID,
			FileID: e.FileID,
			// Path is deliberately left empty: candidate.Path is a FILE PATH
			// everywhere else and ImpactEntry.Name is a qualified name.
			Requirement: boundaryRequirement(e.Kind, e.Depth),
			Origin:      originExpansion,
			Depth:       e.Depth,
		}
		var dropped, truncated int64
		cand.Reasons, dropped, truncated = boundReasons(e.Reasons)
		out.Scope.ReasonsDropped += dropped
		out.Scope.ReasonsTruncated += truncated
		// Only the disclosure count is kept here; the routes themselves are
		// emitted below, once the fold has said which entries survived.
		_, cand.MorePaths = boundPaths(e.Paths, cfg.MaxReasonPathsPerEntry)
		if err := entitySort.Add(candRecOf(cand, impactBase+int64(j))); err != nil {
			return nil, err
		}
	}
	return finishIngest(s, out, entitySort, candSort, impact.Entries, impactBase,
		cfg.MaxReasonPathsPerEntry)
}

// finishIngest drains the dedupe survivors into the output sort, materializes
// the candidate spool in seq order, and emits the two route streams for the
// impact entries that survived.
//
// It is the ONE tail of expandScopeStream: the two early returns above reach it
// as well, so no path hands the next pass an unfinalized sort.
//
// The route emission needs no join. Seq is the impact entry's own position
// offset by len(seeds), so a surviving record indexes impact.Entries directly;
// a record the fold dropped never reaches the walk and its routes are therefore
// never emitted -- which is what keeps P-C's `wanted` set the survivors' set,
// exactly as today's relationsOnPaths loop over res.Candidates does.
//
// Only an expansion entry carries routes: a model.RelationPath is produced by a
// graph walk, and every seed producer (seeds.go) resolves through search and
// attaches none. A seed producer that began attaching one would lose it here,
// and with it the edges it puts on P-C's wanted set, so it must emit its routes
// through this same stream rather than on the candidate.
func finishIngest(s *compileSorts, out *ingested,
	entitySort, candSort *pagination.ExternalSort[candRec],
	entries []model.ImpactEntry, impactBase int64, maxPaths config.Limit) (*ingested, error) {
	entityRun, err := entitySort.Sorted()
	if err != nil {
		return nil, err
	}
	trackRun(s, entityRun)
	if err := entityRun.Each(func(r candRec) error { return candSort.Add(r) }); err != nil {
		return nil, err
	}
	candRun, err := candSort.Sorted()
	if err != nil {
		return nil, err
	}
	trackRun(s, candRun)

	pathSort, err := newSort[pathRec](s, "scope-path", lessPathSeq, sizeOfPath)
	if err != nil {
		return nil, err
	}
	hopSort, err := newSort[hopRec](s, "scope-hop", lessHopSeq, sizeOfHop)
	if err != nil {
		return nil, err
	}
	if err := candRun.Each(func(r candRec) error {
		if r.Seq < impactBase || r.Seq-impactBase >= int64(len(entries)) {
			return nil
		}
		paths, _ := boundPaths(entries[r.Seq-impactBase].Paths, maxPaths)
		for pi := range paths {
			p := paths[pi]
			if err := pathSort.Add(pathRec{
				Seq: r.Seq, PathIdx: int32(pi), CostUnits: p.CostUnits,
				Evidence: p.Evidence, HopCount: int32(len(p.Relations)),
			}); err != nil {
				return err
			}
			for hi, rel := range p.Relations {
				if err := hopSort.Add(hopRec{
					RelationID: rel, Seq: r.Seq, PathIdx: int32(pi), HopIdx: int32(hi),
				}); err != nil {
					return err
				}
			}
		}
		return nil
	}); err != nil {
		return nil, err
	}
	if out.Paths, err = pathSort.Sorted(); err != nil {
		return nil, err
	}
	trackRun(s, out.Paths)
	if out.Hops, err = hopSort.Sorted(); err != nil {
		return nil, err
	}
	trackRun(s, out.Hops)
	out.Cands = candRun
	return out, nil
}
