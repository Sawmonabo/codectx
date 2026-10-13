package graph

import (
	"context"
	"slices"

	"github.com/Sawmonabo/codectx/internal/model"
)

// The dispatch step of a call walk (ADR-0015 decision 2).
//
// A call through a base class is stored once, against the method the
// receiver's declared type declares (`speak calls Animal.sound`), and each of
// its occurrences says whether it can run an override (`dispatch: virtual`)
// or cannot (`exact`: a `super` call, a qualified call, a constructed object).
// The overrides it may run are found at query time, by walking `overrides`:
//
//   - a CALLERS walk climbs from every method it lists callers of to each
//     method that method overrides, at any depth, and lists that base's
//     callers whose call is virtual -- "through" the base. It never descends:
//     a direct call into an override never runs its base, and a call into a
//     sibling override never runs this one.
//   - a CALLEES walk descends from the target of every virtual call to each
//     override of it, at any depth, as a method the call "may dispatch to".
//
// Both steps are taken inside the level walk, as edges of the walk: the
// `overrides` relation that climbed or descended is served as a row of the
// answer, so the route `speak -calls-> Animal.sound <-overrides- Dog.sound` is
// visible, and the walk goes on from the method it reached exactly as from any
// other. One adjacency read per level per visited node, as every walk.
//
// The step belongs to the operation, not to the relation list: a request for
// `calls` alone still takes it, and the `overrides` edges it climbs are added
// to the walk's own scan.

// dispatchMode is which dispatch step a walk takes.
type dispatchMode uint8

const (
	dispatchNone dispatchMode = iota
	// dispatchCallers climbs overrides and walks the bases' virtual callers.
	dispatchCallers
	// dispatchCallees descends from virtual calls' targets to their overrides.
	dispatchCallees
)

// dispatchFor is the step a walk in direction over kinds takes: a walk over
// `calls` in one direction is a call walk, and nothing else is.
func dispatchFor(direction model.Direction, kinds []model.RelationKind) dispatchMode {
	if !slices.Contains(kinds, model.RelCalls) {
		return dispatchNone
	}
	switch direction {
	case model.DirectionIncoming:
		return dispatchCallers
	case model.DirectionOutgoing:
		return dispatchCallees
	}
	return dispatchNone
}

// dispatchStep is how a call walk reached a node.
type dispatchStep uint8

const (
	// stepDirect is every node a walk reached by one of its own relations.
	stepDirect dispatchStep = iota
	// stepBase is a base method a callers walk climbed to: only its virtual
	// callers are walked, and they are callers "through" it.
	stepBase
	// stepVirtual is a callee reached by a call with a virtual occurrence:
	// its overrides may run instead.
	stepVirtual
	// stepOverride is an override a callees walk descended to.
	stepOverride
	// stepRest is a base method a callers walk admitted again because it
	// also reached it as a caller: the calls into it its base step skipped,
	// those with no virtual occurrence, are walked now, and it climbs no
	// further, having climbed when it was first admitted.
	stepRest
	// stepDescend is a callee a callees walk admitted again because a call
	// that can dispatch, or a descent, also reached it after it was first
	// admitted as an exact call's target: its overrides are walked now, and
	// its own calls are not, having been walked when it was first admitted.
	stepDescend
)

// dispatches reports whether a callees walk descends from a node admitted
// with step s to the overrides that may run in its place.
func (s dispatchStep) dispatches() bool {
	return s == stepVirtual || s == stepOverride || s == stepDescend
}

// DispatchReader is the OPTIONAL seam a GraphReader may also implement: for
// each relation, whether any of its occurrences is a virtual call, and the
// largest set size any occurrence carries. A relation counts as through-base
// or may-dispatch when ANY occurrence is virtual (ADR-0015); a walk whose
// reader lacks the seam takes no dispatch step.
type DispatchReader interface {
	// RelationDispatch reads the dispatch summary of each relation; results
	// align with rels, and a relation with no evidence reads as the zero
	// summary.
	RelationDispatch(ctx context.Context, rels []model.RelationID) ([]RelationDispatch, error)
}

// RelationDispatch summarizes how one relation's occurrences dispatch.
type RelationDispatch struct {
	// Virtual is true when any occurrence can run an override of the target.
	Virtual bool
	// SetSize is the largest number of targets any occurrence's site was
	// bound to, 0 for a site bound to one.
	SetSize int64
	// ValueBound is true when any occurrence was bound by a technique whose
	// targets are the values the project's own code gives the site
	// (valueBound): one of them is the target, and a value from outside
	// the project may be another.
	ValueBound bool
}

// The call techniques (evidence details) whose targets are the values the
// project's own code gives a site: the functions of its type whose address
// the project takes, the function values that reach it, and the functions
// written to the field it calls through. The linker publishes them under
// these names.
const (
	CallDetailAddressTaken  = "address_taken"
	CallDetailFunctionValue = "function_value"
	CallDetailField         = "field"
)

// ValueDetails are the techniques valueBound reads, for a reader that
// summarizes them in its own query.
var ValueDetails = []string{CallDetailAddressTaken, CallDetailFunctionValue, CallDetailField}

// valueBound reports whether detail is one of ValueDetails.
func valueBound(detail string) bool { return slices.Contains(ValueDetails, detail) }

// dispatchReader is the seam the engine hands a call walk, nil when the
// packed reader does not report dispatch.
func (e *Engine) dispatchReader() DispatchReader {
	d, _ := e.reader.(DispatchReader)
	return d
}

// dispatchKinds are the kinds a dispatch walk scans beside the requested
// ones: `overrides` for the step itself, and, for a callees walk, `routes_to`,
// so a walk from an endpoint reaches the handler it routes to before its
// calls.
func dispatchKinds(mode dispatchMode, kinds []model.RelationKind) []model.RelationKind {
	extra := []model.RelationKind{model.RelOverrides}
	if mode == dispatchCallees {
		extra = append(extra, model.RelRoutesTo)
	}
	out := slices.Clone(kinds)
	for _, k := range extra {
		if !slices.Contains(out, k) {
			out = append(out, k)
		}
	}
	return out
}

// dispatchRule is one walk's resolved dispatch step: the kind codes it
// treats specially and the codes the request itself named.
type dispatchRule struct {
	mode      dispatchMode
	calls     KindCode
	overrides KindCode
	routes    KindCode
	requested map[KindCode]bool
}

// newDispatchRule resolves the step against the generation's dictionary. A
// generation that seals no `overrides` and no `routes_to` relation takes no
// step at all.
func newDispatchRule(r GraphReader, mode dispatchMode, requested []model.RelationKind) dispatchRule {
	if mode == dispatchNone {
		return dispatchRule{}
	}
	table := r.Kinds()
	if table == nil {
		return dispatchRule{}
	}
	rule := dispatchRule{mode: mode, requested: map[KindCode]bool{}}
	rule.calls, _ = table.Code(model.RelCalls)
	rule.overrides, _ = table.Code(model.RelOverrides)
	if mode == dispatchCallees {
		rule.routes, _ = table.Code(model.RelRoutesTo)
	}
	for _, k := range requested {
		if code, ok := table.Code(k); ok {
			rule.requested[code] = true
		}
	}
	if rule.overrides == 0 && rule.routes == 0 {
		return dispatchRule{}
	}
	return rule
}

// active reports whether the walk takes a dispatch step.
func (d dispatchRule) active() bool { return d.mode != dispatchNone }

// direction is the direction the walk's scan reads: both lists, because each
// step reads the list a plain walk would not.
func (d dispatchRule) direction(walk model.Direction) model.Direction {
	if d.active() {
		return model.DirectionBoth
	}
	return walk
}

// entry decides one scanned entry of a dispatch walk: whether it is kept, the
// step the node it reaches is admitted with, and whether that depends on the
// relation having a virtual occurrence (settled in one batched read per
// collected batch, flush in traverse.go).
//
// A callers walk keeps the owner's incoming entries of the requested kinds --
// only `calls`, and only virtual ones, from a base it climbed to, and only the
// others from a base admitted again as a caller -- and every outgoing
// `overrides` entry, which climbs, but the one that admitted the owner. A callees walk keeps the owner's
// outgoing entries of the requested kinds and every outgoing `routes_to`
// entry -- but none from a node admitted again only to descend -- and an
// incoming `overrides` entry only from a node a virtual call or an earlier
// descent reached. Nothing else is kept: the scan reads both lists
// only to find the step's edges.
func (d dispatchRule) entry(owner frontierState, e Edge) (keep bool, step dispatchStep, virtual bool) {
	switch d.mode {
	case dispatchCallers:
		if !e.Outgoing {
			if owner.Step == stepBase || owner.Step == stepRest {
				return e.Kind == d.calls, stepDirect, true
			}
			return d.requested[e.Kind], stepDirect, false
		}
		// The relation that admitted the owner is not climbed back along: an
		// override reached through a requested `overrides` edge would
		// otherwise serve that same edge a second time.
		if e.Kind == d.overrides && owner.Step != stepRest && e.Rel != owner.Via {
			return true, stepBase, false
		}
		return false, stepDirect, false
	case dispatchCallees:
		if e.Outgoing {
			if owner.Step == stepDescend {
				return false, stepDirect, false
			}
			if e.Kind == d.routes && d.routes != 0 {
				return true, stepDirect, false
			}
			if !d.requested[e.Kind] {
				return false, stepDirect, false
			}
			return true, stepDirect, e.Kind == d.calls
		}
		if e.Kind == d.overrides && owner.Step.dispatches() {
			return true, stepOverride, false
		}
		return false, stepDirect, false
	}
	return false, stepDirect, false
}

// settle applies the batched virtual read to the records entry flagged: in a
// callers walk a base's caller is kept only when its call is virtual; in a
// callees walk a virtual call admits its target as one whose overrides may
// run. It returns the records that stay, in order.
func (d dispatchRule) settle(ctx context.Context, dr DispatchReader, recs []levelRecord) ([]levelRecord, error) {
	var ids []model.RelationID
	for _, r := range recs {
		if r.check {
			ids = append(ids, r.RelID)
		}
	}
	if len(ids) == 0 {
		return recs, nil
	}
	got := map[model.RelationID]bool{}
	for _, chunk := range impactChunkRelations(dedupeRelations(ids)) {
		sums, err := dr.RelationDispatch(ctx, chunk)
		if err != nil {
			return nil, err
		}
		if len(sums) != len(chunk) {
			return nil, internalErr("graph: the reader summarized a different number of relations than it was given")
		}
		for i, id := range chunk {
			got[id] = sums[i].Virtual
		}
	}
	out := recs[:0]
	for _, r := range recs {
		if r.check {
			r.check = false
			switch d.mode {
			case dispatchCallers:
				// A base's callers are its virtual ones; the rest of them,
				// walked when it is admitted again, the others.
				if got[r.RelID] == (r.Owner.Step == stepRest) {
					continue
				}
			case dispatchCallees:
				if got[r.RelID] {
					r.Step = stepVirtual
				}
			}
		}
		out = append(out, r)
	}
	return out, nil
}
