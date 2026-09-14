package graph

import "github.com/Sawmonabo/codectx/internal/model"

// relationCost is the compile-time traversal cost table. It is deliberately NOT
// config-derived: a configurable cost would have to enter the AnalysisKey to
// keep Section 15.1 canonical context identity honest, so two workspaces with
// the same facts would answer differently under the same key. Freezing it here
// makes every path answer reproducible from the facts alone.
//
// Costs are small positive integers so Dijkstra stays in integer arithmetic;
// no float appears anywhere in path or impact ranking. Cheaper means "a
// stronger, more direct dependency": a call is the tightest coupling, a
// containment edge the loosest.
var relationCost = map[model.RelationKind]int64{
	model.RelCalls:            1,
	model.RelImplements:       1,
	model.RelExtends:          1,
	model.RelOverrides:        1,
	model.RelImports:          2,
	model.RelExports:          2,
	model.RelDependsOn:        2,
	model.RelReferences:       2,
	model.RelDataFlowsTo:      2,
	model.RelControlDependsOn: 3,
	model.RelReads:            3,
	model.RelWrites:           3,
	model.RelTests:            4,
	model.RelBuilds:           4,
	model.RelGenerates:        4,
	model.RelDocuments:        5,
	model.RelConfigures:       5,
	model.RelContains:         6,
	model.RelDefines:          6,
	model.RelOwns:             6,

	// Identity-reconciliation edges. They are excluded from every default
	// allowlist (see DefaultRelations), but a caller may still name one
	// explicitly, so the table is total over the Section 9.2 vocabulary. They
	// are the most expensive hop because following a rename or an ambiguous
	// candidate is the weakest evidence of a dependency there is -- and a
	// missing entry defaulting to zero would make them free identity edges
	// that outrank every real dependency.
	model.RelRenamedFrom: 7,
	model.RelMovedFrom:   7,
	model.RelMayReferTo:  7,
}

// unknownRelationCost is charged for a relation kind absent from the table. The
// table is total over the current vocabulary, so this is reached only if a new
// kind is added to model without a cost; charging the maximum keeps such an
// edge from silently winning a shortest-path race.
const unknownRelationCost int64 = 7

// Cost returns the deterministic integer traversal cost of one edge of kind k.
// It never returns zero, so no edge is free and Dijkstra always makes progress.
func Cost(k model.RelationKind) int64 {
	if c, ok := relationCost[k]; ok {
		return c
	}
	return unknownRelationCost
}

// DefaultRelations is the relation allowlist a traversal uses when the request
// names none. It excludes renamed_from, moved_from and may_refer_to: those are
// identity-reconciliation edges, not dependency edges, and walking them by
// default would report a symbol's own former name as something it depends on.
// The order is the Section 9.2 vocabulary order and is stable.
func DefaultRelations() []model.RelationKind {
	return []model.RelationKind{
		model.RelContains, model.RelDefines, model.RelReferences, model.RelCalls,
		model.RelImports, model.RelExports, model.RelImplements, model.RelExtends,
		model.RelOverrides, model.RelReads, model.RelWrites, model.RelDataFlowsTo,
		model.RelControlDependsOn, model.RelTests, model.RelDependsOn,
		model.RelBuilds, model.RelGenerates, model.RelDocuments,
		model.RelConfigures, model.RelOwns,
	}
}

// ImpactRelations is the allowlist an impact query expands over, in the
// priority order impact ranking assumes: the kinds that most strongly imply a
// consumer must change come first.
func ImpactRelations() []model.RelationKind {
	return []model.RelationKind{
		model.RelImplements, model.RelOverrides, model.RelCalls, model.RelReads,
		model.RelWrites, model.RelTests, model.RelConfigures, model.RelDocuments,
		model.RelDependsOn, model.RelImports,
	}
}

// DependenceOnly lists the relation kinds only the dependence provider can
// produce. A request touching any of them must report a deferred dependence
// capability before traversing, because their absence would otherwise look like
// a genuine absence of edges. `calls` is deliberately not in this list: it also
// has a non-engine path, so a deferred dependence build does not make a calls
// answer incomplete.
func DependenceOnly() []model.RelationKind {
	return []model.RelationKind{
		model.RelControlDependsOn, model.RelDataFlowsTo, model.RelReads, model.RelWrites,
	}
}
