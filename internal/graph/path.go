package graph

import (
	"container/heap"
	"context"
	"errors"
	"sort"

	"github.com/Sawmonabo/codectx/internal/model"
)

// pathEnumerationSteps bounds the walk that enumerates equal-cost routes out of
// the shortest-path DAG. The DAG is pruned to shortest edges and to nodes that
// can still reach the target, but the number of distinct routes through it is
// still combinatorial in a dense graph, so the enumeration carries its own
// explicit finite bound like every other traversal here. Hitting it is reported
// as truncation, never as a shorter list of routes presented as complete.
const pathEnumerationSteps = 4096

// Truncation reasons for a path search. Each names the budget that ran out, so
// an operator reading a truncated answer knows which bound to raise.
const (
	pathReasonDeadline = "the query deadline was reached before the path search completed"
	pathReasonVisited  = "the visited-node budget was exhausted before the path search completed"
	pathReasonEdges    = "the edge budget was exhausted before the path search completed"
	pathReasonDepth    = "the max-depth bound stopped the path search; the routes returned are the cheapest within that depth"
	pathReasonRoutes   = "the route-enumeration budget was exhausted before every equal-cost route was collected"
	pathReasonDeferred = "dependence units are still building"
)

// ShortestPath runs a nonnegative integer-cost Dijkstra over the request's
// relation allowlist. An exhausted depth, visited budget or deadline is
// reported as truncation together with the paths found so far -- never as "no
// path exists", which is reserved for a genuinely unreachable target.
func (e *Engine) ShortestPath(ctx context.Context, req model.PathRequest) (res model.PathResult, err error) {
	defer func() { err = typedContextError(ctx, err) }()
	// The landed model validator is the request contract: it screens the
	// generation, the resolved id spelling of both endpoints, the relation
	// vocabulary and the bound signs. A second hand-written copy here would
	// drift from it, which is exactly how an unvalidated id reached the walk.
	if err := req.Validate(); err != nil {
		return model.PathResult{}, err
	}

	// The deadline wraps the gate as well as the walk: waiting for a slot past
	// the request deadline is a resource limit the caller must see, not a silent
	// queue.
	ctx, cancel := context.WithDeadline(ctx, e.now().Add(e.limits.QueryTimeout))
	defer cancel()
	if e.gate != nil {
		if err := e.gate.Acquire(ctx); err != nil {
			return model.PathResult{}, err
		}
		defer e.gate.Release()
	}

	kinds := req.Relations
	if len(kinds) == 0 {
		kinds = DefaultRelations()
	}

	res = model.PathResult{Meta: model.QueryMeta{Binding: e.adjacency.Binding()}}
	// The same disclosure the traversal entries make, from the same place: a
	// second implementation here would be a second shape for one contract --
	// pendingDependence returns the deferred rows alone, copied unchanged in
	// State and DiagnosticCode, with a promotion's queue position folded in.
	completeness, err := e.pendingDependence(ctx, kinds)
	if err != nil {
		return model.PathResult{}, err
	}
	res.Meta.Completeness = completeness
	if len(completeness) > 0 {
		pathTruncate(&res.Meta, pathReasonDeferred)
	}

	// A zero-edge route cannot be expressed: RelationPath requires at least one
	// relation. Reporting no route is honest; emitting a path that fails its own
	// validator is not.
	if req.From == req.To {
		return res, nil
	}

	w := &pathWalk{
		adjacency:  e.adjacency,
		kinds:      kinds,
		maxDepth:   pathBound(req.MaxDepth, e.limits.MaxDepth),
		maxVisited: int64(pathBound(req.MaxVisited, e.limits.MaxVisited)),
		maxEdges:   int64(e.limits.MaxEdges),
		dist:       map[model.NodeID]int64{},
		depth:      map[model.NodeID]int{},
		settled:    map[model.NodeID]bool{},
		edges:      map[model.NodeID][]model.Relation{},
	}
	if err := w.run(ctx, req.From, req.To); err != nil {
		return model.PathResult{}, err
	}
	res.VisitedCount = w.visited

	var routes [][]model.Relation
	if w.settled[req.To] {
		want := min(e.limits.MaxReasonPaths, model.MaxReasonPathsPerEntry)
		var capped bool
		routes, capped = w.routes(req.From, req.To, want)
		if capped {
			pathTruncate(&res.Meta, pathReasonRoutes)
		}
	}
	// Budget exhaustion is reported whether or not a route was found: a route
	// found under an exhausted budget is the cheapest one seen, not provably the
	// cheapest one there is.
	switch {
	case w.deadlineHit:
		pathTruncate(&res.Meta, pathReasonDeadline)
	case w.visitedHit:
		pathTruncate(&res.Meta, pathReasonVisited)
	case w.edgesHit:
		pathTruncate(&res.Meta, pathReasonEdges)
	case w.depthPruned:
		pathTruncate(&res.Meta, pathReasonDepth)
	}
	if len(routes) == 0 {
		return res, nil
	}

	paths, err := e.hydratePaths(ctx, routes)
	if err != nil {
		return model.PathResult{}, err
	}
	res.Paths = paths

	ids := []model.NodeID{req.From}
	seen := map[model.NodeID]bool{req.From: true}
	for _, route := range routes {
		for _, r := range route {
			if !seen[r.To] {
				seen[r.To] = true
				ids = append(ids, r.To)
			}
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	nodes, err := e.adjacency.NodesByID(ctx, ids)
	if err != nil {
		return model.PathResult{}, err
	}
	res.Nodes = nodes
	return res, nil
}

// pathBound resolves a request bound: zero means "the configured default", and
// a request may only narrow the engine's limit, never widen it.
func pathBound(requested, limit int) int {
	if requested <= 0 || requested > limit {
		return limit
	}
	return requested
}

// pathTruncate marks the answer incomplete. The first reason wins, so the
// budget that actually stopped the search is the one reported.
func pathTruncate(m *model.QueryMeta, reason string) {
	if m.Truncated {
		return
	}
	m.Truncated = true
	m.TruncationReason = reason
}

// hydratePaths attaches the evidence backing each route. Evidence is fetched for
// every relation on every route in ONE batched call, not one call per edge, and
// each route's evidence list carries the same bound as its relation list.
func (e *Engine) hydratePaths(ctx context.Context, routes [][]model.Relation) ([]model.RelationPath, error) {
	var ids []model.RelationID
	seen := map[model.RelationID]bool{}
	for _, route := range routes {
		for _, r := range route {
			if !seen[r.ID] {
				seen[r.ID] = true
				ids = append(ids, r.ID)
			}
		}
	}
	byRelation, err := e.adjacency.EvidenceFor(ctx, ids, model.MaxRelationsPerPath)
	if err != nil {
		return nil, err
	}
	paths := make([]model.RelationPath, 0, len(routes))
	for _, route := range routes {
		p := model.RelationPath{Relations: make([]model.RelationID, 0, len(route))}
		for _, r := range route {
			p.Relations = append(p.Relations, r.ID)
			p.CostUnits += Cost(r.Kind)
			for _, ev := range byRelation[r.ID] {
				if len(p.Evidence) == model.MaxRelationsPerPath {
					break
				}
				p.Evidence = append(p.Evidence, ev)
			}
		}
		paths = append(paths, p)
	}
	return paths, nil
}

// pathHeapItem is one frontier entry. The heap orders by the frozen composite
// key (cost asc, depth asc, NodeID asc) and never by insertion order, so two
// runs over the same facts settle nodes in the same sequence.
type pathHeapItem struct {
	cost  int64
	depth int
	node  model.NodeID
}

type pathHeap []pathHeapItem

func (h pathHeap) Len() int { return len(h) }
func (h pathHeap) Less(i, j int) bool {
	if h[i].cost != h[j].cost {
		return h[i].cost < h[j].cost
	}
	if h[i].depth != h[j].depth {
		return h[i].depth < h[j].depth
	}
	return h[i].node < h[j].node
}
func (h pathHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *pathHeap) Push(x any)   { *h = append(*h, x.(pathHeapItem)) }
func (h *pathHeap) Pop() any     { old := *h; it := old[len(old)-1]; *h = old[:len(old)-1]; return it }

// pathWalk is one Dijkstra over the adjacency port. It expands a whole cost
// bucket per round trip rather than one node at a time: every relation cost is
// at least 1 (Cost never returns zero), so two nodes at the same distance can
// never improve each other and the whole bucket is settled simultaneously. That
// is what lets the walk batch its frontier instead of issuing one query per node.
type pathWalk struct {
	adjacency  Adjacency
	kinds      []model.RelationKind
	maxDepth   int
	maxVisited int64
	maxEdges   int64

	dist    map[model.NodeID]int64
	depth   map[model.NodeID]int
	settled map[model.NodeID]bool
	// edges caches the outgoing edges of every node the walk expanded, each
	// slice in RelationID order, so route enumeration reads no map iteration
	// order and issues no extra query.
	edges map[model.NodeID][]model.Relation

	pq      pathHeap
	visited int64
	spent   int64

	depthPruned bool
	visitedHit  bool
	edgesHit    bool
	deadlineHit bool
}

// run settles nodes in cost order until the target is settled or a budget runs
// out. It stops the moment the target settles: every predecessor on a shortest
// route to it is strictly cheaper, so it was settled and expanded in an earlier
// bucket and the cached edge set into the target is already complete.
func (w *pathWalk) run(ctx context.Context, from, to model.NodeID) error {
	w.dist[from] = 0
	w.depth[from] = 0
	heap.Push(&w.pq, pathHeapItem{node: from})

	for w.pq.Len() > 0 {
		if err := ctx.Err(); err != nil {
			if !errors.Is(err, context.DeadlineExceeded) {
				// A caller who stopped the query gets the cancellation, not a
				// half-answer dressed up as a budget truncation.
				return model.Canceled(err)
			}
			w.deadlineHit = true
			return nil
		}
		bucket := w.popBucket()
		if w.settled[to] {
			return nil
		}
		// The budget check precedes the empty-bucket skip: popBucket trips the
		// budget before it appends, so an exhausted walk yields an empty bucket
		// and skipping to the next iteration here would drain the whole heap
		// one pop at a time after the budget was already declared spent.
		if w.visitedHit {
			return nil
		}
		if len(bucket) == 0 {
			continue
		}
		var expand []model.NodeID
		for _, it := range bucket {
			if it.depth >= w.maxDepth {
				// Cheapest-by-cost may still need more hops than the depth
				// bound allows; the answer is then the cheapest route WITHIN
				// the depth bound, and that is reported as truncation.
				w.depthPruned = true
				continue
			}
			expand = append(expand, it.node)
		}
		if len(expand) == 0 {
			continue
		}
		if err := w.expandBatch(ctx, expand); err != nil {
			return err
		}
		if w.edgesHit {
			return nil
		}
	}
	return nil
}

// popBucket settles every queued node sharing the current minimum cost, up to
// one adjacency batch, and returns them.
func (w *pathWalk) popBucket() []pathHeapItem {
	cost := w.pq[0].cost
	var bucket []pathHeapItem
	for w.pq.Len() > 0 && w.pq[0].cost == cost && len(bucket) < adjacencyBatch {
		it := heap.Pop(&w.pq).(pathHeapItem)
		if w.settled[it.node] || it.cost != w.dist[it.node] {
			continue // a stale entry superseded by a cheaper relaxation
		}
		if w.visited >= w.maxVisited {
			w.visitedHit = true
			break
		}
		w.settled[it.node] = true
		w.visited++
		bucket = append(bucket, it)
	}
	return bucket
}

// expandBatch reads every outgoing edge of the batch in keyset-paged round
// trips and relaxes them.
func (w *pathWalk) expandBatch(ctx context.Context, nodes []model.NodeID) error {
	// Sorting is deferred so it also runs on the truncated path: the edges of a
	// node the budget cut short are still read by route enumeration, and their
	// order is the determinism invariant.
	defer func() {
		for _, n := range nodes {
			e := w.edges[n]
			sort.Slice(e, func(i, j int) bool { return e[i].ID < e[j].ID })
		}
	}()
	var after model.RelationID
	for {
		rels, err := w.adjacency.Edges(ctx, nodes, model.DirectionOutgoing, w.kinds, after, adjacencyBatch)
		if err != nil {
			return err
		}
		for _, r := range rels {
			after = r.ID
			w.spent++
			if w.spent > w.maxEdges {
				w.edgesHit = true
				return nil
			}
			w.edges[r.From] = append(w.edges[r.From], r)
			w.relax(r)
		}
		// Only an empty page ends the keyset walk: the reader clamps the
		// requested limit down to model.MaxPageItems, so a short page is the
		// normal case rather than the end of the edges. `after` advanced on
		// every row above, so the next page starts past the last one read.
		if len(rels) == 0 {
			break
		}
	}
	return nil
}

// relax admits the cheaper of two routes into a node, breaking a cost tie on
// the shorter hop count so the recorded depth is the one the depth bound is
// measured against.
func (w *pathWalk) relax(r model.Relation) {
	if w.settled[r.To] {
		return
	}
	cost := w.dist[r.From] + Cost(r.Kind)
	depth := w.depth[r.From] + 1
	known, seen := w.dist[r.To]
	switch {
	case !seen || cost < known:
		w.dist[r.To] = cost
		w.depth[r.To] = depth
		heap.Push(&w.pq, pathHeapItem{cost: cost, depth: depth, node: r.To})
	case cost == known && depth < w.depth[r.To]:
		w.depth[r.To] = depth
		heap.Push(&w.pq, pathHeapItem{cost: cost, depth: depth, node: r.To})
	}
}

// routes enumerates up to want equal-cost shortest routes from the shortest-path
// DAG, in the frozen order: cost ascending (every route here shares the settled
// cost of the target) then the RelationID sequence compared lexicographically.
// A preorder walk that visits each node's edges in RelationID order emits the
// sequences in exactly that order. The second return reports that the
// enumeration budget ran out before the DAG was exhausted.
func (w *pathWalk) routes(from, to model.NodeID, want int) ([][]model.Relation, bool) {
	if want <= 0 {
		return nil, false
	}
	// reaches memoises "some shortest route continues from here to the target",
	// which prunes the dead-end prefixes that would otherwise make enumeration
	// exponential in a dense DAG.
	reaches := map[model.NodeID]bool{}
	var canReach func(n model.NodeID) bool
	canReach = func(n model.NodeID) bool {
		if n == to {
			return true
		}
		if ok, done := reaches[n]; done {
			return ok
		}
		reaches[n] = false // the DAG is acyclic (cost strictly increases per hop)
		for _, r := range w.admitted(n) {
			if canReach(r.To) {
				reaches[n] = true
				break
			}
		}
		return reaches[n]
	}

	var out [][]model.Relation
	var cur []model.Relation
	steps := 0
	capped := false
	var walk func(n model.NodeID)
	walk = func(n model.NodeID) {
		if len(out) == want || capped {
			return
		}
		if n == to {
			out = append(out, append([]model.Relation(nil), cur...))
			return
		}
		if len(cur) >= w.maxDepth || len(cur) >= model.MaxRelationsPerPath {
			return
		}
		for _, r := range w.admitted(n) {
			if steps++; steps > pathEnumerationSteps {
				capped = true
				return
			}
			if !canReach(r.To) {
				continue
			}
			cur = append(cur, r)
			walk(r.To)
			cur = cur[:len(cur)-1]
			if len(out) == want || capped {
				return
			}
		}
	}
	walk(from)
	return out, capped
}

// admitted returns the edges out of n that lie on a shortest route, in
// RelationID order.
func (w *pathWalk) admitted(n model.NodeID) []model.Relation {
	var out []model.Relation
	for _, r := range w.edges[n] {
		if !w.settled[r.To] {
			continue
		}
		if w.dist[n]+Cost(r.Kind) == w.dist[r.To] {
			out = append(out, r)
		}
	}
	return out
}
