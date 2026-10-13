package graph

import (
	"slices"

	"github.com/Sawmonabo/codectx/internal/model"
)

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
	model.RelCalls:      1,
	model.RelImplements: 1,
	model.RelExtends:    1,
	model.RelOverrides:  1,
	// An endpoint runs its handler: as tight a coupling as a call.
	model.RelRoutesTo:         1,
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

	// Name-only candidates, computed per query. A path admits them at the
	// highest cost there is, so any precise route is cheaper (ADR-0015).
	model.RelMayCall:   7,
	model.RelMayImport: 7,
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
// It also excludes may_call and may_import, which are computed per query and
// walked only when a request names them. `routes_to` is in it: an endpoint
// depends on the handler it runs, so a walk that names no relation crosses
// from an endpoint to its handler. The order is the model's vocabulary order
// and is stable.
func DefaultRelations() []model.RelationKind {
	return []model.RelationKind{
		model.RelContains, model.RelDefines, model.RelReferences, model.RelCalls,
		model.RelImports, model.RelExports, model.RelImplements, model.RelExtends,
		model.RelOverrides, model.RelReads, model.RelWrites, model.RelDataFlowsTo,
		model.RelControlDependsOn, model.RelTests, model.RelDependsOn,
		model.RelBuilds, model.RelGenerates, model.RelDocuments,
		model.RelConfigures, model.RelOwns, model.RelRoutesTo,
	}
}

// ImpactRelations is the allowlist an impact query expands over, in the
// priority order impact ranking assumes: the kinds that most strongly imply a
// consumer must change come first. `routes_to` is in it so a changed handler
// marks the endpoints that route to it affected, and the program-dependence
// relation set closes it, so impact is a forward slice over data and control
// dependence as well as a walk over calls (ADR-0012 decision 3).
func ImpactRelations() []model.RelationKind {
	return append([]model.RelationKind{
		model.RelImplements, model.RelOverrides, model.RelCalls, model.RelRoutesTo, model.RelReads,
		model.RelWrites, model.RelTests, model.RelConfigures, model.RelDocuments,
		model.RelDependsOn, model.RelImports,
	}, ProgramDependenceRelations()...)
}

// ProgramDependenceRelations is the program-dependence relation set: data
// dependence and control dependence, which the structural provider keys by
// the same variable entities and the linking provider extends across calls
// (argument flows, and the summary and return flows of a call's result). A
// walk over the two together is a walk over the program dependence graph
// (Ferrante, Ottenstein and Warren 1987) without the caller composing it by
// naming relation kinds.
func ProgramDependenceRelations() []model.RelationKind {
	return []model.RelationKind{model.RelDataFlowsTo, model.RelControlDependsOn}
}

// dependenceTier is the bit an impact route's cost carries once the route
// has taken a program-dependence hop. It makes the cost a lexicographic key,
// (reached through dependence, cost), in one integer: every comparison that
// orders routes by cost -- the walk's admission, the record fold and the
// served rank -- puts every route with no program-dependence hop before every
// route with one, at every depth, and orders each tier by its plain cost.
// Every route stays in the answer; the rule decides only the order.
//
// It is set, never added (kindCosts.add, impactAdd), so a route of any number
// of dependence hops carries it once. The plain cost beneath it is at most the
// dearest kind cost times the route's depth, and 2^48 / 7 hops is a walk
// deeper than any store's node count, so the bit never collides with a plain
// cost. A state's encoded cost grows from one or two varint bytes to seven
// only once its route has taken a dependence hop.
const dependenceTier int64 = 1 << 48

// isDependence reports a program-dependence kind.
func isDependence(k model.RelationKind) bool {
	return slices.Contains(ProgramDependenceRelations(), k)
}

// impactAdd is the cost of an impact route extended by one edge of kind k:
// the traversal cost (Cost) added, and dependenceTier set for a
// program-dependence kind. Every other operation adds the plain cost, so a
// path or a traversal over data flow costs what it always has.
func impactAdd(total int64, k model.RelationKind) int64 {
	if isDependence(k) {
		total |= dependenceTier
	}
	return total + Cost(k)
}

// plainCost is a route cost without its tier bit: the sum of its kind costs.
func plainCost(cost int64) int64 { return cost &^ dependenceTier }

// reversedKinds are the kinds whose stored direction runs from the dependency
// to the dependent. Every other relation kind is stored dependent first (`a
// calls b`, `x control_depends_on y`, `f reads v`: the from-node depends on the
// to-node), but `x data_flows_to y` says y depends on x. An impact walk walks
// them against their stored direction, so its incoming half still reaches the
// entities that may need modification and its outgoing half the ones that may
// need reading.
func reversedKinds() []model.RelationKind {
	return []model.RelationKind{model.RelDataFlowsTo}
}
