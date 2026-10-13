package graph

import (
	"fmt"
	"slices"

	"github.com/Sawmonabo/codectx/internal/model"
)

// Impact over the program dependence graph (ADR-0012 decision 3).
//
// An impact walk is a forward slice over data and control dependence as well
// as a walk over calls: the impact allowlist holds the program-dependence
// relation set, and a walk with expandOptions.Dependence set takes two rules
// no other walk takes.
//
//   - Orientation. `x data_flows_to y` is stored from the dependency to the
//     dependent, every other kind the other way round, so the walk reads it
//     against its stored direction: the incoming half of an impact walk takes
//     a `data_flows_to` edge OUT of the node it is at, and reports the node it
//     reaches as incoming (it may need modification).
//   - Cost. A program-dependence hop costs impactCost, which ranks every node
//     a call, override or structural route reaches above the nodes only a
//     dependence route reaches (dependenceHorizon states the rule).
//
// Interprocedural realizability is a property of the facts, not of the walk.
// Across a call, the linking provider publishes three flows: each variable an
// argument reads to the parameter it binds; a summary edge from that variable
// to the call's result when the parameter reaches the callee's return; and a
// return edge into the call's result from each callee variable no parameter
// reaches. A walk that entered a callee through an argument can then reach no
// return edge's source, so it never leaves the callee through another call
// site, and the forward slice follows every `data_flows_to` edge with no
// per-path call stack: the two phases of Horwitz, Reps and Binkley's forward
// slice (TOPLAS 12(1), 1990, section 4.5) exist to stop exactly that ascent,
// and the summary edges carry the effect the second phase would otherwise
// lose.

// dependenceWalk is one walk's resolved dependence rules: the kind codes it
// walks against their stored direction, nil when the walk takes none.
type dependenceWalk struct {
	reversed map[KindCode]bool
}

// newDependenceWalk resolves the reversed kinds against the generation's
// dictionary. A walk without the Dependence option, or over none of the
// reversed kinds, takes no rule.
func newDependenceWalk(r GraphReader, o expandOptions) dependenceWalk {
	if !o.Dependence {
		return dependenceWalk{}
	}
	table := r.Kinds()
	if table == nil {
		return dependenceWalk{}
	}
	d := dependenceWalk{}
	for _, k := range reversedKinds() {
		if len(o.Kinds) > 0 && !slices.Contains(o.Kinds, k) {
			continue
		}
		code, ok := table.Code(k)
		if !ok {
			continue
		}
		if d.reversed == nil {
			d.reversed = map[KindCode]bool{}
		}
		d.reversed[code] = true
	}
	return d
}

// active reports whether the walk reads any kind against its stored direction.
func (d dependenceWalk) active() bool { return len(d.reversed) > 0 }

// scanDirection is the direction the walk's scan reads: both lists when a
// one-direction walk reads a reversed kind, whose entries lie in the other
// list, and otherwise what the dispatch rule reads.
func (d dependenceWalk) scanDirection(walk model.Direction, rule dispatchRule) model.Direction {
	if d.active() && walk != model.DirectionBoth {
		return model.DirectionBoth
	}
	return rule.direction(walk)
}

// orient decides one scanned entry of a one-direction walk that reads both
// lists for a reversed kind. A reversed kind's entry is taken from the list
// opposite the walk's direction; when the dispatch rule is inactive, so both
// lists are read only for the reversed kinds, every other entry is taken from
// the walk's own list. decided is false when the walk reads as stored (a
// `both` walk, or no reversed kind) or when the dispatch rule decides the
// entry.
//
// Under the dispatch rule, an owner admitted as a climb's base method is a
// route to its virtual callers, not an affected entity, and an owner admitted
// again only to descend to its overrides had its own edges walked when it was
// first admitted: neither takes a reversed kind's entry, which would walk the
// base's dependents or serve the descending node's edges a second time.
func (d dependenceWalk) orient(walk model.Direction, rule dispatchRule, owner frontierState, e Edge) (take, decided bool) {
	if !d.active() || walk == model.DirectionBoth {
		return false, false
	}
	outgoing := walk == model.DirectionOutgoing
	if d.reversed[e.Kind] {
		if rule.active() && (owner.Step == stepBase || owner.Step == stepDescend) {
			return false, true
		}
		return e.Outgoing != outgoing, true
	}
	if rule.active() {
		return false, false
	}
	return e.Outgoing == outgoing, true
}

// impactDirection is the direction an impact entry reports for an edge of
// kind reached with the reader's outgoing flag: incoming (may need
// modification) when the entry depends on the node the walk came from, and
// outgoing (may need reading) when that node depends on it.
func impactDirection(kind model.RelationKind, outgoing bool) model.Direction {
	if slices.Contains(reversedKinds(), kind) {
		outgoing = !outgoing
	}
	if outgoing {
		return model.DirectionOutgoing
	}
	return model.DirectionIncoming
}

// impactEdgeReason is impactReason for an edge whose stored direction and
// reported direction may differ: stored is whether the node the walk came
// from is the relation's from-node, so `this symbol data_flows_to it` is said
// of the entry its value flows to, which reports incoming.
func impactEdgeReason(kind model.RelationKind, stored bool, dir model.Direction, depth int) string {
	if stored {
		return fmt.Sprintf("this symbol %s it (%s, depth %d)", kind, dir, depth)
	}
	return fmt.Sprintf("%s this symbol (%s, depth %d)", kind, dir, depth)
}

// dependenceCosts is the walk's cost table with each program-dependence kind
// at its impactCost, so a frontier state's cost and the cost of the record
// impactAccumulator.Visit emits from it are one sum over one table.
func dependenceCosts(r GraphReader, costs kindCosts) kindCosts {
	table := r.Kinds()
	if table == nil {
		return costs
	}
	out := slices.Clone(costs)
	for _, k := range ProgramDependenceRelations() {
		if code, ok := table.Code(k); ok && int(code) < len(out) {
			out[code] = impactCost(k)
		}
	}
	return out
}
