package graph

import (
	"container/heap"
	"context"
	"errors"
	"sort"

	"github.com/Sawmonabo/codectx/internal/config"
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
	pathReasonMemory   = "the query memory budget was exhausted before the path search completed"
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
	// completeness returns the generation's capability rows with the deferred
	// dependence ones enriched in place, and the bound flag, never the row
	// count, is what truncates the answer.
	caps, deferred, err := e.completeness(ctx, kinds)
	if err != nil {
		return model.PathResult{}, err
	}
	res.Meta.Completeness = caps
	if deferred {
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
		maxDepth:   pathBound(req.MaxDepth, e.limits.Depth()),
		maxVisited: pathBound(req.MaxVisited, e.limits.Visited()),
		maxEdges:   e.limits.Edges(),
		maxBytes:   e.limits.FrontierBytes,
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
		// model.MaxReasonPathsPerEntry is still the wire bound PathResult's
		// Validate enforces, so it floors the configured setting rather than
		// being floored by it; an unlimited setting yields the wire bound, not
		// zero routes. A cut is reported as pathReasonRoutes below.
		want := e.limits.ReasonPaths().Min(config.Limit(model.MaxReasonPathsPerEntry))
		var capped bool
		routes, capped = w.routes(req.From, req.To, want.Int())
		if capped {
			pathTruncate(&res.Meta, pathReasonRoutes)
		}
	}
	// Budget exhaustion is reported whether or not a route was found: a route
	// found under an exhausted budget is the cheapest one seen, not provably the
	// cheapest one there is.
	switch {
	case w.memoryHit:
		pathTruncate(&res.Meta, pathReasonMemory)
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
// A request may only narrow the configured bound, never widen it, and an
// unlimited configured bound is the top of the lattice: any finite request
// lowers it, and an absent request leaves the walk unbounded.
func pathBound(requested int, limit config.Limit) config.Limit {
	if requested <= 0 {
		return limit
	}
	return config.Limit(requested).Min(limit)
}

// atBound reports whether a walk that has already taken n steps has reached
// bound, so the step after it would cross. An unlimited bound is never reached:
// reading the zero Limit as a numeric ceiling would stop every walk at once.
func atBound(bound config.Limit, n int) bool { return bound.Exceeded(int64(n) + 1) }

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
	maxDepth   config.Limit
	maxVisited config.Limit
	maxEdges   config.Limit
	// maxBytes is Limits.FrontierBytes: the ceiling on what the walk's own
	// state -- dist, depth, settled, the cached edges and the priority queue --
	// may hold at once. Before it, those four maps grew with the REACHABLE SET
	// and the finite default budgets were the only thing bounding them; with
	// every count bound unlimited by default there was nothing left. Unlike the
	// BFS frontier this state cannot spill: a Dijkstra resumed from a persisted
	// frontier also needs its settled distances, and that is a continuation
	// this endpoint does not have. So the ceiling is REPORTED as truncation
	// (pathReasonMemory) with the cheapest routes found so far, and the
	// operator's remedy is resources.query_memory_bytes.
	maxBytes int64

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
	// bytes is the running estimate of what the four maps and the heap hold,
	// charged with the same conservative per-row overhead the BFS frontier uses.
	bytes int64

	depthPruned bool
	memoryHit   bool
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
	w.charge(pathNodeBytes(from) + pathQueueBytes(from))
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
			if atBound(w.maxDepth, it.depth) {
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
		if w.edgesHit || w.memoryHit {
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
		if atBound(w.maxVisited, int(w.visited)) {
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
			if w.maxEdges.Exceeded(w.spent) {
				w.edgesHit = true
				return nil
			}
			w.charge(pathEdgeBytes(r))
			w.edges[r.From] = append(w.edges[r.From], r)
			w.relax(r)
			if w.memoryHit {
				return nil
			}
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
		if !seen {
			w.charge(pathNodeBytes(r.To))
		}
		w.dist[r.To] = cost
		w.depth[r.To] = depth
		w.charge(pathQueueBytes(r.To))
		heap.Push(&w.pq, pathHeapItem{cost: cost, depth: depth, node: r.To})
	case cost == known && depth < w.depth[r.To]:
		w.depth[r.To] = depth
		w.charge(pathQueueBytes(r.To))
		heap.Push(&w.pq, pathHeapItem{cost: cost, depth: depth, node: r.To})
	}
}

// charge adds n bytes to the walk's estimate and trips memoryHit once the
// ceiling is crossed. An unlimited ceiling (0) never trips, which is what keeps
// `query_memory_bytes = 0` meaning "bounded by the graph and the count budgets
// alone" rather than "stop immediately".
func (w *pathWalk) charge(n int64) {
	w.bytes += n
	if w.maxBytes > 0 && w.bytes > w.maxBytes {
		w.memoryHit = true
	}
}

// pathNodeBytes, pathQueueBytes and pathEdgeBytes estimate what one settled
// node, one queue entry and one cached edge cost. Like edgeRowOverheadBytes
// they are deliberate over-estimates: the ceiling is a memory ceiling, and an
// estimate that read low would let it be crossed before the walk noticed.
//
// A node is charged once for its three map entries (dist, depth, settled) and
// again for every queue entry it takes, because a node relaxed twice occupies
// the heap twice.
func pathNodeBytes(n model.NodeID) int64 { return 3 * (edgeRowOverheadBytes + int64(len(n))) }

func pathQueueBytes(n model.NodeID) int64 { return edgeRowOverheadBytes + int64(len(n)) }

func pathEdgeBytes(r model.Relation) int64 {
	return edgeRowOverheadBytes + int64(len(r.ID)+len(r.From)+len(r.To)+len(r.Kind))
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
		if atBound(w.maxDepth, len(cur)) || len(cur) >= model.MaxRelationsPerPath {
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
