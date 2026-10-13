package context

import (
	"container/heap"
	"context"
	"fmt"
	"slices"
	"sort"

	"github.com/Sawmonabo/codectx/internal/config"
	"github.com/Sawmonabo/codectx/internal/graph"
	"github.com/Sawmonabo/codectx/internal/model"
)

// referenceWalks is the reference's scope walk: for each root-weight group on
// its own, every node's largest (requirement, score) pairs over every route
// from that group's roots (ADR-0016 decision 1), each kept route admitted as
// its own record. It shares nothing with graph.WeightedWalk but the reader and
// the route arithmetic the ranking scores a route by.
//
// Per group it is a best-first label search held in heap, which the small
// parity fixtures allow. A route's pair is decided by its depth (the
// requirement, through the node's kind) and its route factor (the score, the
// group's weight being one number). Past depth refDepthClasses every kind is
// optional, so those depths are one class. A route is kept unless a kept route
// to the same node is no deeper in class and has a factor at least as large:
// that one's pair is at least as large for every kind and every boost, and so
// are its extensions. Routes are taken in factor order, which never rises along
// a route, so a kept route is final; ties go to the shallower route and then to
// the smaller last relation id.
//
// Every bound is the streamed walk's: the group's derived depth bound ends a
// route silently, context.max_graph_depth ends it and narrows the scope when an
// edge is left unwalked, and max_visited_nodes and max_graph_edges bound the
// whole walk, every group's routes counted together. The records are emitted
// in (node, depth class, class, weight) order across the groups, the order the
// walk serves its labels in, and each one's reasons are the relations a
// breadth-first walk from its group would deliver into the node.
//
// It returns the capability rows the walk discloses, which are the graph
// family's own (read here through a one-entry impact probe), or none when no
// group is walked.
func referenceWalks(ctx context.Context, eng *graph.Engine, gen model.GenerationID, cfg config.Context,
	groups []walkGroup, admitted map[string]bool, res *refScope) ([]model.CapabilityState, error) {
	if len(groups) == 0 {
		return nil, nil
	}
	r, err := eng.Reader()
	if err != nil {
		return nil, contextErr(ctx, err)
	}
	rule, err := scopeLabelRule()
	if err != nil {
		return nil, err
	}
	probe, err := eng.Impact(ctx, model.ImpactRequest{GenerationID: gen, Start: groups[0].Start[:1],
		Relations: scopeRelations, Direction: model.DirectionBoth, MaxDepth: 1,
		Page: model.PageRequest{Limit: 1}})
	if err != nil {
		return nil, contextErr(ctx, err)
	}
	w := &refWalker{ctx: ctx, r: r, rule: rule, cfg: cfg, adj: map[model.NodeID][]refEdge{}}
	if err := w.codes(); err != nil {
		return nil, err
	}
	var records []refRecord
	for _, g := range groups {
		out, err := w.group(g)
		if err != nil {
			return nil, err
		}
		records = append(records, out...)
		if w.stopped {
			break
		}
	}
	if w.cut {
		res.ScopeComplete = false
	}
	sort.SliceStable(records, func(i, j int) bool {
		a, b := records[i], records[j]
		if a.label.node != b.label.node {
			return a.label.node < b.label.node
		}
		if ca, cb := refClass(a.label.depth), refClass(b.label.depth); ca != cb {
			return ca < cb
		}
		if a.group.Named != b.group.Named {
			return !a.group.Named
		}
		return a.group.Weight < b.group.Weight
	})
	var reached []model.ImpactEntry
	for _, rec := range records {
		c, ok, err := w.record(rec)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		var dropped, truncated int64
		c.Reasons, dropped, truncated = boundReasons(c.Reasons)
		res.ReasonsDropped += dropped
		res.ReasonsTruncated += truncated
		c.Paths, c.MorePaths = boundPaths(c.Paths, cfg.MaxReasonPathsPerEntry)
		res.Candidates = append(res.Candidates, c)
		if n := len(reached); n == 0 || reached[n-1].NodeID != c.NodeID {
			reached = append(reached, model.ImpactEntry{NodeID: c.NodeID})
		}
	}
	if err := referenceCandidates(ctx, eng, gen, reached, admitted, res); err != nil {
		return nil, err
	}
	return probe.Meta.Completeness, nil
}

// refDepthClasses is the depth from which every node kind is optional.
var refDepthClasses = requirementRank(model.RequirementOptional) - requirementRank(model.RequirementFull) + 1

func refClass(depth int) int { return min(depth, refDepthClasses) }

// refEdge is one adjacency entry of a node, from that node's side.
type refEdge struct {
	other model.NodeID
	rel   model.RelationID
	kind  model.RelationKind
	// from is true when the node is the relation's from-node.
	from bool
}

// refLabel is one route: its end, depth and factor, its last hop and the
// route it extends.
type refLabel struct {
	node   model.NodeID
	depth  int
	factor int64
	rel    model.RelationID
	kind   model.RelationKind
	// leaves is true when the last hop leaves the parent.
	leaves bool
	parent *refLabel
}

// refRecord is a kept route and the group it belongs to, with the group's
// shallowest depth at every node, which its reasons are read against.
type refRecord struct {
	label *refLabel
	group walkGroup
	bound int
	dist  map[model.NodeID]int
}

type refWalker struct {
	ctx       context.Context
	r         graph.GraphReader
	rule      graph.LabelRule
	cfg       config.Context
	kindCodes []graph.KindCode
	adj       map[model.NodeID][]refEdge
	// edges and visited are the whole walk's spend; visitedSet the nodes it
	// counted.
	edges, visited int64
	visitedSet     map[model.NodeID]bool
	cut, stopped   bool
}

func (w *refWalker) codes() error {
	for _, k := range scopeRelations {
		if c, ok := w.r.Kinds().Code(k); ok {
			w.kindCodes = append(w.kindCodes, c)
		}
	}
	w.visitedSet = map[model.NodeID]bool{}
	return nil
}

// neighbours reads a node's adjacency once.
func (w *refWalker) neighbours(n model.NodeID) ([]refEdge, error) {
	if got, ok := w.adj[n]; ok {
		return got, nil
	}
	var out []refEdge
	if len(w.kindCodes) > 0 {
		refs, err := w.r.Resolve(w.ctx, []model.NodeID{n})
		if err != nil {
			return nil, err
		}
		if len(refs) == 1 && refs[0] != 0 {
			var edges []graph.Edge
			if _, err := w.r.Neighbours(w.ctx, refs, model.DirectionBoth, w.kindCodes, graph.EdgePos{},
				func(e graph.Edge) error { edges = append(edges, e); return nil }); err != nil {
				return nil, err
			}
			others := make([]graph.NodeRef, len(edges))
			rels := make([]graph.RelRef, len(edges))
			for i, e := range edges {
				others[i], rels[i] = e.Neighbour, e.Rel
			}
			otherIDs, err := w.r.NodeIDs(w.ctx, others)
			if err != nil {
				return nil, err
			}
			relIDs, err := w.r.RelationIDs(w.ctx, rels)
			if err != nil {
				return nil, err
			}
			for i, e := range edges {
				kind, ok := w.r.Kinds().Kind(e.Kind)
				if !ok || otherIDs[i] == "" || relIDs[i] == "" {
					continue
				}
				out = append(out, refEdge{other: otherIDs[i], rel: relIDs[i], kind: kind, from: e.Outgoing})
			}
		}
	}
	w.adj[n] = out
	return out, nil
}

// refQueue orders routes best first: the larger factor, then the shallower
// depth, then the node and the smaller last relation id.
type refQueue []*refLabel

func (q refQueue) Len() int { return len(q) }
func (q refQueue) Less(i, j int) bool {
	a, b := q[i], q[j]
	if a.factor != b.factor {
		return a.factor > b.factor
	}
	if a.depth != b.depth {
		return a.depth < b.depth
	}
	if a.node != b.node {
		return a.node < b.node
	}
	return a.rel < b.rel
}
func (q refQueue) Swap(i, j int) { q[i], q[j] = q[j], q[i] }
func (q *refQueue) Push(x any)   { *q = append(*q, x.(*refLabel)) }
func (q *refQueue) Pop() any {
	old := *q
	x := old[len(old)-1]
	*q = old[:len(old)-1]
	return x
}

// group runs one group's label search and answers its kept routes past depth
// zero.
func (w *refWalker) group(g walkGroup) ([]refRecord, error) {
	bound, derived := walkDepth(g.MaxDepth, w.cfg.MaxGraphDepth)
	kept := map[model.NodeID][]*refLabel{}
	q := &refQueue{}
	for _, n := range g.Start {
		heap.Push(q, &refLabel{node: n, factor: w.rule.Scale})
	}
	var out []*refLabel
	for q.Len() > 0 {
		l := heap.Pop(q).(*refLabel)
		dominated := slices.ContainsFunc(kept[l.node], func(k *refLabel) bool {
			return refClass(k.depth) <= refClass(l.depth) && k.factor >= l.factor
		})
		if dominated {
			continue
		}
		if l.depth > 0 && !w.visitedSet[l.node] {
			if w.cfg.MaxVisitedNodes.Exceeded(w.visited + 1) {
				w.cut, w.stopped = true, true
				break
			}
			w.visitedSet[l.node] = true
			w.visited++
		}
		kept[l.node] = append(kept[l.node], l)
		if l.depth > 0 {
			out = append(out, l)
		}
		edges, err := w.neighbours(l.node)
		if err != nil {
			return nil, err
		}
		if bound > 0 && l.depth >= bound {
			if !derived && len(edges) > 0 {
				w.cut = true
			}
			continue
		}
		for _, e := range edges {
			if w.cfg.MaxGraphEdges.Exceeded(w.edges + 1) {
				w.cut, w.stopped = true, true
				break
			}
			w.edges++
			factor, err := mulScaled(l.factor, w.rule.Hop[e.kind])
			if err != nil {
				return nil, err
			}
			if l.depth > 0 {
				if factor, err = mulScaled(factor, w.rule.Decay); err != nil {
					return nil, err
				}
			}
			heap.Push(q, &refLabel{node: e.other, depth: l.depth + 1, factor: factor, rel: e.rel,
				kind: e.kind, leaves: e.from, parent: l})
		}
		if w.stopped {
			break
		}
	}
	dist := map[model.NodeID]int{}
	for n, labels := range kept {
		for _, k := range labels {
			if d, ok := dist[n]; !ok || k.depth < d {
				dist[n] = k.depth
			}
		}
	}
	records := make([]refRecord, 0, len(out))
	for _, l := range out {
		records = append(records, refRecord{label: l, group: g, bound: bound, dist: dist})
	}
	return records, nil
}

// record builds one kept route's candidate: the node hydrated, the
// requirement of its kind at the route's depth (capped for a lexical group),
// its reasons and its one route with that route's evidence.
func (w *refWalker) record(rec refRecord) (candidate, bool, error) {
	l := rec.label
	nodes, err := w.r.NodesByID(w.ctx, []model.NodeID{l.node})
	if err != nil {
		return candidate{}, false, err
	}
	if len(nodes) == 0 {
		return candidate{}, false, nil
	}
	n := nodes[0]
	requirement := boundaryRequirement(n.Kind, l.depth)
	if !rec.group.Named && requirementRank(requirement) < requirementRank(model.RequirementRecommended) {
		requirement = model.RequirementRecommended
	}
	reasons, err := w.reasons(rec)
	if err != nil {
		return candidate{}, false, err
	}
	c := candidate{NodeID: n.ID, FileID: n.FileID, Requirement: requirement, Origin: originExpansion,
		Depth: l.depth, RootWeight: rec.group.Weight, Named: rec.group.Named, Reasons: reasons}
	var hops []*refLabel
	for h := l; h.parent != nil; h = h.parent {
		hops = append(hops, h)
	}
	slices.Reverse(hops)
	if len(hops) > 0 && len(hops) <= model.MaxRelationsPerPath {
		rels := make([]model.RelationID, len(hops))
		var cost int64
		for i, h := range hops {
			rels[i] = h.rel
			cost += graph.Cost(h.kind)
		}
		evidence, err := w.r.EvidenceFor(w.ctx, rels, model.MaxRelationsPerPath)
		if err != nil {
			return candidate{}, false, err
		}
		var ev []model.EvidenceID
		for _, rel := range rels {
			ev = append(ev, evidence[rel]...)
			if len(ev) >= model.MaxRelationsPerPath {
				ev = ev[:model.MaxRelationsPerPath]
				break
			}
		}
		c.Paths = []model.RelationPath{{Relations: rels, CostUnits: cost, Evidence: ev}}
	}
	return c, true, nil
}

// reasons are the relations a breadth-first walk from the route's group would
// deliver into its node: from a neighbour one level shallower, or from one on
// the same level when the relation leaves that neighbour, the neighbour being
// one the group expands. With none, the route's own last hop explains it.
func (w *refWalker) reasons(rec refRecord) ([]string, error) {
	l := rec.label
	edges, err := w.neighbours(l.node)
	if err != nil {
		return nil, err
	}
	near := rec.dist[l.node]
	var out []string
	for _, e := range edges {
		far, ok := rec.dist[e.other]
		if !ok || (rec.bound > 0 && far >= rec.bound) {
			continue
		}
		leaves := !e.from
		if near != far+1 && !(near == far && leaves) {
			continue
		}
		out = append(out, refReason(e.kind, leaves, far+1))
	}
	if len(out) == 0 {
		out = []string{refReason(l.kind, l.leaves, l.depth)}
	}
	slices.Sort(out)
	out = slices.Compact(out)
	if len(out) > model.MaxReasonsPerEntry {
		out = out[:model.MaxReasonsPerEntry]
	}
	return out, nil
}

// refReason is a walk reason's wording (docs/queries.md): "this symbol <kind>
// it" when the relation leaves the node the walk came from, and the reported
// direction outgoing for every kind but data_flows_to, which is stored
// dependency first and reports the other direction (an impact entry its value
// flows to may need modification).
func refReason(kind model.RelationKind, outgoing bool, depth int) string {
	dir := "incoming"
	if outgoing != (kind == model.RelDataFlowsTo) {
		dir = "outgoing"
	}
	if outgoing {
		return fmt.Sprintf("this symbol %s it (%s, depth %d)", kind, dir, depth)
	}
	return fmt.Sprintf("%s this symbol (%s, depth %d)", kind, dir, depth)
}
