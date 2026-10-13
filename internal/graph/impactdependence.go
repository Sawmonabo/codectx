package graph

import (
	"context"
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
//   - Rank. A route that takes a program-dependence hop carries
//     dependenceTier in its cost, so every node a call, override or
//     structural route reaches ranks above every node only a dependence route
//     reaches, at any depth.
//
//   - Realizability. Across a call the linking provider publishes an
//     argument flow (a variable an argument reads to the parameter it binds),
//     a summary flow (that variable to the call's result, when the parameter
//     reaches the callee's return) and a return flow (the callee's return node
//     to the call's result). A route that entered a callee through an argument
//     never leaves it through a return flow: it would come out at every call
//     site's result, the unrealizable path, while the summary flow at its own
//     site already carries what it returns. This is the two-phase forward
//     slice of Horwitz, Reps and Binkley (TOPLAS 12(1), 1990, section 4.5)
//     kept per route rather than as two passes: a route carries
//     sliceDescended once it descends, and a node first admitted descended is
//     admitted once more when a route without it reaches it, so every node the
//     first phase would mark keeps its right to leave (retainedWalk.lifts).
//     Taken against its stored direction the rule is the same with the roles
//     swapped: walking a return flow backwards enters the callee, walking an
//     argument flow backwards leaves it. The descent describes a chain of
//     dependence: an edge of any other kind starts a new one.

// dependenceWalk is one walk's resolved dependence rules: the kind codes it
// walks against their stored direction, nil when the walk takes none; the
// program-dependence kind codes; data_flows_to's code and the reader of its
// relations' call roles, nil when the walk keeps no call context.
type dependenceWalk struct {
	reversed map[KindCode]bool
	pdg      map[KindCode]bool
	flow     KindCode
	roles    FlowRoleReader
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
	d := dependenceWalk{pdg: map[KindCode]bool{}}
	for _, k := range ProgramDependenceRelations() {
		if code, ok := table.Code(k); ok {
			d.pdg[code] = true
		}
	}
	if code, ok := table.Code(model.RelDataFlowsTo); ok && o.Flows != nil {
		d.flow, d.roles = code, o.Flows
	}
	d.reversed = reversedCodes(table, o.Kinds)
	return d
}

// reversedCodes resolves the reversed kinds among kinds -- every kind when
// kinds is empty -- against the generation's dictionary, nil when none is
// present.
func reversedCodes(table KindTable, kinds []model.RelationKind) map[KindCode]bool {
	var out map[KindCode]bool
	if table == nil {
		return nil
	}
	for _, k := range reversedKinds() {
		if len(kinds) > 0 && !slices.Contains(kinds, k) {
			continue
		}
		code, ok := table.Code(k)
		if !ok {
			continue
		}
		if out == nil {
			out = map[KindCode]bool{}
		}
		out[code] = true
	}
	return out
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
// marked by dependenceTier, so kindCosts.add sets the tier on a frontier
// state's cost exactly as impactAdd sets it on the record
// impactAccumulator.Visit emits from that state.
func dependenceCosts(r GraphReader, costs kindCosts) kindCosts {
	table := r.Kinds()
	if table == nil {
		return costs
	}
	out := slices.Clone(costs)
	for _, k := range ProgramDependenceRelations() {
		if code, ok := table.Code(k); ok && int(code) < len(out) {
			out[code] = Cost(k) | dependenceTier
		}
	}
	return out
}

// add is a route's cost extended by one edge of kind code: the kind's plain
// cost added and, for a kind marked with dependenceTier, the tier set rather
// than added, so it is carried once however many dependence hops the route
// takes. A table no kind of which is marked adds the plain cost.
func (c kindCosts) add(total int64, code KindCode) int64 {
	v := c.of(code)
	return (total | v&dependenceTier) + v&^dependenceTier
}

// sliceFlags is how a dependence walk's route reached a node (frontierState
// and levelRecord Slice).
type sliceFlags uint8

const (
	// sliceDescended: the route entered a callee through a call's argument
	// and may not leave it through a call site.
	sliceDescended sliceFlags = 1 << iota
	// sliceSunk marks an admitted state that is its node's FIRST admission
	// and is descended (retainedWalk.sunk).
	sliceSunk
	// sliceUpgrade marks the second admission of a sunk node, by a route
	// that is not descended. Its dependence edges were walked when it was
	// first admitted; they are walked again only to carry the right to leave,
	// so they reach the walk as silent records, and only the edges that leave
	// through a call site are new.
	sliceUpgrade
	// sliceSilent marks a level record that admits but is never served: it
	// lifts a node already reached, and its edge was served once already.
	sliceSilent
)

func (f sliceFlags) descended() bool { return f&sliceDescended != 0 }

// The evidence details that name a data flow's call role, as the linking
// provider publishes them (docs/providers-link.md).
const (
	// FlowDetailArgument is an argument flow: a variable an argument reads to
	// the parameter it binds.
	FlowDetailArgument = "argument"
	// FlowDetailReturn is a return flow: the callee's return node to the call
	// site's result.
	FlowDetailReturn = "return"
)

// FlowRole is one data_flows_to relation's call role: whether any of its
// occurrences is an argument flow, and whether any is a return flow.
type FlowRole struct {
	Argument bool
	Return   bool
}

// FlowRoleReader is the OPTIONAL seam a GraphReader may also implement: the
// call role of each relation, read in one grouped read per batch, as
// DispatchReader reads dispatch. A dependence walk whose reader lacks it keeps
// no call context.
type FlowRoleReader interface {
	// RelationFlowRoles reads each relation's role; results align with rels,
	// and a relation with no evidence reads as the zero role.
	RelationFlowRoles(ctx context.Context, rels []model.RelationID) ([]FlowRole, error)
}

// flowReader is the seam an impact walk hands its expansion, nil when the
// packed reader does not report flow roles.
func (e *Engine) flowReader() FlowRoleReader {
	f, _ := e.reader.(FlowRoleReader)
	return f
}

// inDirection reports whether a one-direction or `both` walk takes entry e by
// orientation alone: a reversed kind from the list opposite the walk's
// direction, every other kind from the walk's own list.
func (d dependenceWalk) inDirection(walk model.Direction, e Edge) bool {
	if walk == model.DirectionBoth {
		return true
	}
	outgoing := walk == model.DirectionOutgoing
	if d.reversed[e.Kind] {
		return e.Outgoing != outgoing
	}
	return e.Outgoing == outgoing
}

// decideEntry is the scan's decision for one delivered entry: whether it is
// collected, and the dispatch step, dispatch check and slice flags of the
// record it becomes. It is the direction dedup rule, then the dependence
// orientation, then the dispatch step, and, on a dependence walk, the slice
// flags; an entry whose call role decides it is settled with its batch
// (settleSlice).
//
// An owner admitted to lift its right to leave (sliceUpgrade) walks only its
// dependence edges, all of them, whatever the dedup rule says: each is either
// one it served at its first admission, which now only lifts the node it
// reaches, or one leaving through a call site, which is new. Its other edges
// were walked at its first admission, unless the dispatch step admitted it
// again at the same time, and then that step decides them.
func (w *levelWalk) decideEntry(owner frontierState, e Edge, rec *levelRecord) (bool, error) {
	d := w.dependence
	pdg := d.pdg[e.Kind]
	kept, err := w.keepEntry(e)
	if err != nil {
		return false, err
	}
	if owner.Slice&sliceUpgrade != 0 {
		if !pdg {
			if !kept || (owner.Step != stepRest && owner.Step != stepDescend) || !w.rule.active() {
				return false, nil
			}
			var take bool
			take, rec.Step, rec.check = w.rule.entry(owner, e)
			return take, nil
		}
		if !d.inDirection(w.o.Direction, e) {
			return false, nil
		}
		rec.lift, rec.kept = true, kept
		rec.role = e.Kind == d.flow && d.roles != nil
		return true, nil
	}
	if !kept {
		return false, nil
	}
	if take, decided := d.orient(w.o.Direction, w.rule, owner, e); decided {
		if !take {
			return false, nil
		}
	} else if w.rule.active() {
		var take bool
		if take, rec.Step, rec.check = w.rule.entry(owner, e); !take {
			return false, nil
		}
	}
	if pdg {
		rec.Slice = owner.Slice & sliceDescended
		rec.role = e.Kind == d.flow && d.roles != nil
	}
	return true, nil
}

// settleSlice applies one batch's call roles, read in one grouped read per
// chunk, to the records decideEntry flagged, and returns the records that
// stay, in order. A descended record's route never leaves through a call
// site; a record that enters a callee is descended. A lifting owner's record
// that leaves through a call site is new and served; one that enters a callee
// reaches nothing its first admission did not; any other reaches a node
// already admitted and is silent, or, if its node was never reached, is served.
func (d dependenceWalk) settleSlice(ctx context.Context, w *levelWalk, recs []levelRecord) ([]levelRecord, error) {
	var ids []model.RelationID
	for _, r := range recs {
		if r.role {
			ids = append(ids, r.RelID)
		}
	}
	roles := map[model.RelationID]FlowRole{}
	for _, chunk := range impactChunkRelations(dedupeRelations(ids)) {
		got, err := d.roles.RelationFlowRoles(ctx, chunk)
		if err != nil {
			return nil, err
		}
		if len(got) != len(chunk) {
			return nil, internalErr("graph: the reader named a different number of flow roles than it was given")
		}
		for i, id := range chunk {
			roles[id] = got[i]
		}
	}
	out := recs[:0]
	for _, r := range recs {
		var role FlowRole
		if r.role {
			role = roles[r.RelID]
		}
		forward := r.Edge.Outgoing
		enters := (role.Argument && forward) || (role.Return && !forward)
		leaves := (role.Return && forward) || (role.Argument && !forward)
		lift, kept := r.lift, r.kept
		r.role, r.lift, r.kept = false, false, false
		if lift {
			switch {
			case leaves:
				if !kept {
					continue
				}
				r.Slice = 0
			case enters:
				continue
			default:
				reached, err := w.o.Retain.bits.test(uint64(r.Edge.Neighbour))
				if err != nil {
					return nil, err
				}
				r.Slice = 0
				if reached {
					r.Slice = sliceSilent
				}
			}
			out = append(out, r)
			continue
		}
		if leaves && r.Owner.Slice.descended() {
			continue
		}
		if enters {
			r.Slice |= sliceDescended
		}
		out = append(out, r)
	}
	return out, nil
}

// slicing reports whether the walk keeps call context.
func (d dependenceWalk) slicing() bool { return d.roles != nil && d.flow != 0 }

// The bit files of a dependence walk's call context (retainedWalk.sunk and
// lifted), beside the walk's other node sets.
const (
	bitsetSunkFile   = "sunk.bits"
	bitsetLiftedFile = "lifted.bits"
)

// lifts reports whether a node the walk already admitted is admitted once
// more by a record of slice flags reached: the node was first admitted
// descended, has not been lifted, and the record's route is not descended.
func (w *retainedWalk) lifts(node NodeRef, reached sliceFlags) (bool, error) {
	if reached.descended() {
		return false, nil
	}
	sunk, err := w.sunk.test(uint64(node))
	if err != nil || !sunk {
		return false, err
	}
	lifted, err := w.lifted.test(uint64(node))
	return !lifted, err
}

// applySlice records one admitted level's sunk and lifted nodes. Like
// applySteps it is idempotent.
func (w *retainedWalk) applySlice(sunk, lifted []NodeRef) error {
	if len(sunk) > 0 {
		if _, err := bitsetSet(w.sunk, sunk); err != nil {
			return err
		}
	}
	if len(lifted) > 0 {
		if _, err := bitsetSet(w.lifted, lifted); err != nil {
			return err
		}
	}
	return nil
}
