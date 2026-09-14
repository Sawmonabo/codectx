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
		MaxDepth:   cfg.MaxGraphDepth,
		MaxVisited: cfg.MaxVisitedNodes,
		MaxEdges:   cfg.MaxGraphEdges,
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
			// ImpactEntry.Name is the entry's qualified name, which this
			// candidate carries as its label until hydrateFiles replaces it
			// with the file path FileID resolves to.
			Path:        e.Name,
			Requirement: boundaryRequirement(e.Kind, e.Depth),
			Origin:      originExpansion,
			Depth:       e.Depth,
			Reasons:     boundReasons(e.Reasons),
			Paths:       boundPaths(e.Paths, cfg.MaxReasonPathsPerEntry),
		}
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

// boundReasons clamps an entry's explanation to the Section 15.3 caps. The
// engine already bounds them, so this guards a future engine change from
// producing a manifest that model.ContextEntry.Validate would reject at
// persistence time, one pass too late to explain itself.
func boundReasons(reasons []string) []string {
	if len(reasons) > model.MaxReasonsPerEntry {
		reasons = reasons[:model.MaxReasonsPerEntry]
	}
	out := make([]string, 0, len(reasons))
	for _, r := range reasons {
		if len(r) > model.MaxReasonBytes {
			r = r[:model.MaxReasonBytes]
		}
		out = append(out, r)
	}
	return out
}

// boundPaths clamps the stored evidence paths to context.max_reason_paths_per_entry.
func boundPaths(paths []model.RelationPath, max int) []model.RelationPath {
	if max <= 0 || max > model.MaxReasonPathsPerEntry {
		max = model.MaxReasonPathsPerEntry
	}
	if len(paths) > max {
		paths = paths[:max]
	}
	return append([]model.RelationPath(nil), paths...)
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
