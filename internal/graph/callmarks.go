package graph

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/Sawmonabo/codectx/internal/model"
)

// callPage collects, while one page of a call walk is served, the rows its
// dispatch step produced, so the answer can mark them and count them apart.
type callPage struct {
	mode dispatchMode
	// calls is every `calls` relation the page served, in serve order.
	calls []model.RelationID
	// throughBase maps a callers walk's call into a base method to that base.
	throughBase map[model.RelationID]model.NodeID
	// mayDispatch is every `overrides` relation a callees walk descended.
	mayDispatch []model.RelationID
}

func newCallPage(mode dispatchMode) *callPage {
	return &callPage{mode: mode, throughBase: map[model.RelationID]model.NodeID{}}
}

// note records one served relation of the page: owner is the frontier state
// the edge left from and ownerID that node's canonical id.
func (p *callPage) note(owner frontierState, edge Edge, rel model.Relation, ownerID model.NodeID) {
	if p == nil || p.mode == dispatchNone {
		return
	}
	switch {
	case rel.Kind == model.RelCalls:
		p.calls = append(p.calls, rel.ID)
		if p.mode == dispatchCallers && owner.Step == stepBase {
			p.throughBase[rel.ID] = ownerID
		}
	case rel.Kind == model.RelOverrides && p.mode == dispatchCallees && !edge.Outgoing:
		p.mayDispatch = append(p.mayDispatch, rel.ID)
	}
}

// drop forgets relations the page no longer serves.
func (p *callPage) drop(ids map[model.RelationID]bool) {
	if p == nil {
		return
	}
	p.calls = slices.DeleteFunc(p.calls, func(id model.RelationID) bool { return ids[id] })
}

// marks turns the page's rows into the answer's marks and adds its counts to
// the walk's cumulative ones: a call through a base method is marked with the
// base and how many methods override it, and it lists only its virtual
// occurrences -- its exact ones (a `super` call, a qualified call) can never
// run the override, so they are dropped from the answer's occurrences too; a
// descended override is a method the call may dispatch to; a call whose site
// was bound to several targets, or bound through the values the project
// gives it (a function pointer, a function value, a field: RelationDispatch.
// ValueBound), is one of n, n >= 1. Every mark is a possibility
// (model.GraphRowMark). It returns the marks and the answer's occurrences.
func (e *Engine) marks(ctx context.Context, p *callPage, occurrences []model.RelationOccurrence,
	counts *model.GraphCounts) ([]model.GraphRowMark, []model.RelationOccurrence, error) {
	if p == nil || p.mode == dispatchNone {
		return nil, occurrences, nil
	}
	var out []model.GraphRowMark
	virtual := map[model.RelationID][]model.EvidenceID{}
	kept := occurrences[:0:0]
	for _, o := range occurrences {
		if o.Dispatch == model.DispatchVirtual {
			virtual[o.RelationID] = append(virtual[o.RelationID], o.EvidenceID)
		} else if _, through := p.throughBase[o.RelationID]; through {
			continue
		}
		kept = append(kept, o)
	}
	overrides := map[model.NodeID]int64{}
	for _, id := range p.calls {
		base, ok := p.throughBase[id]
		if !ok {
			counts.Direct++
			continue
		}
		n, seen := overrides[base]
		if !seen {
			var err error
			if n, err = e.overrideCount(ctx, base); err != nil {
				return nil, nil, err
			}
			overrides[base] = n
		}
		counts.ThroughBase++
		out = append(out, model.GraphRowMark{RelationID: id, Kind: model.MarkThroughBase, Base: base,
			BaseOverrides: max(n, 1), Possible: true, Occurrences: virtual[id]})
	}
	for _, id := range p.mayDispatch {
		counts.MayDispatch++
		out = append(out, model.GraphRowMark{RelationID: id, Kind: model.MarkMayDispatch, Possible: true})
	}
	sets, err := e.setSizes(ctx, p.calls)
	if err != nil {
		return nil, nil, err
	}
	for _, id := range p.calls {
		if s := sets[id]; s.SetSize >= 2 || s.ValueBound {
			out = append(out, model.GraphRowMark{RelationID: id, Kind: model.MarkValueSet, SetSize: max(s.SetSize, 1),
				Possible: true})
		}
	}
	return out, kept, nil
}

// setSizes reads the dispatch summary of each relation's occurrences: the
// largest set size, and whether a technique bound one through the values
// the project gives its site.
func (e *Engine) setSizes(ctx context.Context, rels []model.RelationID) (map[model.RelationID]RelationDispatch, error) {
	out := map[model.RelationID]RelationDispatch{}
	dr := e.dispatchReader()
	if dr == nil || len(rels) == 0 {
		return out, nil
	}
	for _, chunk := range impactChunkRelations(dedupeRelations(rels)) {
		sums, err := dr.RelationDispatch(ctx, chunk)
		if err != nil {
			return nil, err
		}
		if len(sums) != len(chunk) {
			return nil, internalErr("graph: the reader summarized a different number of relations than it was given")
		}
		for i, id := range chunk {
			out[id] = sums[i]
		}
	}
	return out, nil
}

// overrideCount is how many methods override base at any depth: the methods
// a virtual call into base may run instead of it. One adjacency read per
// level of the override tree.
func (e *Engine) overrideCount(ctx context.Context, base model.NodeID) (int64, error) {
	r := e.reader
	code, ok := r.Kinds().Code(model.RelOverrides)
	if !ok {
		return 0, nil
	}
	refs, err := r.Resolve(ctx, []model.NodeID{base})
	if err != nil {
		return 0, err
	}
	if len(refs) != 1 || refs[0] == 0 {
		return 0, nil
	}
	seen := map[NodeRef]bool{refs[0]: true}
	level := refs
	var n int64
	for len(level) > 0 {
		var next []NodeRef
		if _, err := r.Neighbours(ctx, level, model.DirectionIncoming, []KindCode{code}, EdgePos{}, func(e Edge) error {
			if !seen[e.Neighbour] {
				seen[e.Neighbour] = true
				next = append(next, e.Neighbour)
			}
			return nil
		}); err != nil {
			return 0, err
		}
		n += int64(len(next))
		level = ascending(next)
	}
	return n, nil
}

// externalNotices is ADR-0015's disclosure for a callers answer: a start
// method that marks no override, of a class whose supertype lies outside the
// indexed project, may still be called through that supertype, and such calls
// are found only for overrides the language marks.
func (e *Engine) externalNotices(ctx context.Context, start []model.NodeID) ([]string, error) {
	r := e.reader
	table := r.Kinds()
	if table == nil {
		return nil, nil
	}
	overrides, hasOverrides := table.Code(model.RelOverrides)
	var supers []KindCode
	for _, k := range []model.RelationKind{model.RelExtends, model.RelImplements} {
		if code, ok := table.Code(k); ok {
			supers = append(supers, code)
		}
	}
	if len(supers) == 0 {
		return nil, nil
	}
	ids := dedupeNodes(start)
	refs, err := r.Resolve(ctx, ids)
	if err != nil {
		return nil, err
	}
	var notices []string
	for i, ref := range refs {
		if ref == 0 {
			continue
		}
		kinds, err := r.NodeKinds(ctx, []NodeRef{ref})
		if err != nil {
			return nil, err
		}
		if len(kinds) != 1 || kinds[0] != model.NodeMethod {
			continue
		}
		if hasOverrides {
			marked := false
			if _, err := r.Neighbours(ctx, []NodeRef{ref}, model.DirectionOutgoing, []KindCode{overrides}, EdgePos{},
				func(Edge) error { marked = true; return ErrStopScan }); err != nil {
				return nil, err
			}
			if marked {
				continue
			}
		}
		owners, err := r.Containers(ctx, []NodeRef{ref})
		if err != nil {
			return nil, err
		}
		if len(owners) != 1 || owners[0] == 0 {
			continue
		}
		var supersOf []NodeRef
		if _, err := r.Neighbours(ctx, []NodeRef{owners[0]}, model.DirectionOutgoing, supers, EdgePos{},
			func(e Edge) error { supersOf = append(supersOf, e.Neighbour); return nil }); err != nil {
			return nil, err
		}
		if len(supersOf) == 0 {
			continue
		}
		superIDs, err := r.NodeIDs(ctx, ascending(supersOf))
		if err != nil {
			return nil, err
		}
		nodes, err := e.adjacency.NodesByID(ctx, superIDs)
		if err != nil {
			return nil, err
		}
		method := string(ids[i])
		self, err := e.adjacency.NodesByID(ctx, []model.NodeID{ids[i]})
		if err != nil {
			return nil, err
		}
		if len(self) == 1 {
			method = nodeLabel(self[0]) + " (" + method + ")"
		}
		for _, n := range nodes {
			if !isExternal(n) {
				continue
			}
			label := nodeLabel(n)
			notices = append(notices, fmt.Sprintf(
				"%s marks no override, and its class extends %s, which is outside the indexed project: "+
					"calls through %s are found only for overrides the language marks, so callers through it may be missing",
				method, label, label))
		}
	}
	return notices, nil
}

// nodeLabel is the name a notice gives a node: its qualified name, else its
// name.
func nodeLabel(n model.Node) string {
	if n.QualifiedName != "" {
		return n.QualifiedName
	}
	return n.Name
}

// isExternal reports whether n is a structural node standing for a type or
// method outside the indexed project.
func isExternal(n model.Node) bool {
	if len(n.Metadata) == 0 {
		return false
	}
	var meta struct {
		Resolution model.NameResolution `json:"resolution"`
	}
	if err := json.Unmarshal(n.Metadata, &meta); err != nil {
		return false
	}
	return meta.Resolution == model.ResolutionExternal
}
