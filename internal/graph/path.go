package graph

import (
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
	pathReasonDeferred = "dependence units are still building"
)

// ShortestPath runs an EXTERNAL-MEMORY nonnegative integer-cost Dijkstra over
// the request's relation allowlist. An exhausted depth, visited budget or
// deadline is reported as truncation together with the paths found so far --
// never as "no path exists", which is reserved for a genuinely unreachable
// target, and never because the search ran out of heap: the settled set, the
// parent edges and the tentative cost buckets live on disk, so peak heap is a
// function of one bucket chunk and not of the reachable set.
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

	// The search state is a private scratch file for the life of this request
	// and is removed with it, whatever happened.
	// Uncancellable: a scratch write that stops halfway because the deadline
	// landed between two statements would fail the request instead of
	// truncating the answer. w.checkDeadline is the only stop.
	scCtx := context.WithoutCancel(ctx)
	sc, err := openPathScratch(scCtx, e.scratchDir())
	if err != nil {
		return model.PathResult{}, err
	}
	defer sc.close()

	w := &pathWalk{
		adjacency:     e.adjacency,
		kinds:         kinds,
		maxDepth:      pathBound(req.MaxDepth, e.limits.Depth()),
		maxVisited:    pathBound(req.MaxVisited, e.limits.Visited()),
		maxEdges:      e.limits.Edges(),
		frontierBytes: e.limits.FrontierBytes,
		sc:            sc,
		scCtx:         scCtx,
	}
	if err := w.run(ctx, req.From, req.To); err != nil {
		return model.PathResult{}, err
	}
	res.VisitedCount = w.visited

	var routes [][]model.Relation
	if w.targetSettled {
		// model.MaxReasonPathsPerEntry is still the wire bound PathResult's
		// Validate enforces, so it floors the configured setting rather than
		// being floored by it; an unlimited setting yields the wire bound, not
		// zero routes. A cut is reported as pathReasonRoutes below.
		want := e.limits.ReasonPaths().Min(config.Limit(model.MaxReasonPathsPerEntry))
		var capped bool
		routes, capped, err = w.routes(req.From, req.To, want.Int())
		if err != nil {
			return model.PathResult{}, err
		}
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

// pathGroup is one node's whole row group out of a cost bucket: every
// relaxation that reached it at this bucket's cost. The group is the unit of
// settling, and it is ALWAYS complete -- see pathWalk.readChunk.
type pathGroup struct {
	node    model.NodeID
	depth   int
	parents []pathParent
}

// pathParent is one tied incoming edge on a shortest route into a node.
type pathParent struct {
	rel  model.RelationID
	from model.NodeID
	kind model.RelationKind
}

// pathWalk is one EXTERNAL-MEMORY Dijkstra over the adjacency port.
//
// Every relation cost is an integer >= 1 (Cost never returns zero), so the
// search runs as cost-bucket phases in the Dial/Munagala-Ranade shape: bucket
// c is swept in one pass, a node is FINAL the moment its bucket is settled,
// and every relaxation a phase emits lands in a strictly later bucket, so a
// bucket is never reopened. That is what makes the state externalizable: the
// settled set, the parent edges of the shortest-path DAG and the tentative
// buckets all live in pathScratch, and nothing here is sized by the reachable
// set.
//
// Peak heap is the CHUNK: one node-aligned slice of the current bucket,
// bounded by frontierBytes, plus the scratch file's fixed page cache. There is
// no memory truncation reason, because there is no in-heap structure left for
// a memory ceiling to protect: exceeding frontierBytes costs another chunk,
// never a shortened answer.
type pathWalk struct {
	adjacency Adjacency
	kinds     []model.RelationKind
	maxDepth  config.Limit
	// maxVisited and maxEdges are the walk's work budgets, resolved the way
	// every other bound here is: the request may narrow the configured limit,
	// never widen it, and an unlimited configured bound leaves the walk
	// unbounded. Spending one is REPORTED as truncation; it is never silent.
	maxVisited config.Limit
	maxEdges   config.Limit
	// frontierBytes is Limits.FrontierBytes and bounds ONE CHUNK of the
	// current bucket. It is strictly positive (graph.New enforces it) and it
	// is not a scale limit: it decides how much of a bucket is read at a time,
	// not how much of the graph the search may cover.
	frontierBytes int64
	sc            *pathScratch
	// scCtx drives every scratch statement. It is the request context with its
	// cancellation removed, so the deadline ends the search at a checkDeadline
	// and never in the middle of a write to the state file.
	scCtx context.Context

	visited int64
	spent   int64
	// peakChunkBytes is the high-water mark of one chunk, the structural
	// memory assertion a test reads: it must stay inside the frontier budget
	// (plus the one group that budget is allowed to overshoot for) however
	// large the graph is.
	peakChunkBytes int64

	depthPruned   bool
	visitedHit    bool
	edgesHit      bool
	deadlineHit   bool
	targetSettled bool
}

// run settles cost buckets in ascending order until the target settles or a
// budget runs out. It stops the moment the target's group is settled: every
// predecessor on a shortest route into it is strictly cheaper, so it settled
// and expanded in an earlier bucket, and the target's tied-parent set is
// therefore already complete.
func (w *pathWalk) run(ctx context.Context, from, to model.NodeID) error {
	// The source is seeded as a parentless bucket-0 row, so the phase loop
	// below has exactly one shape: every node, source included, is settled out
	// of a bucket.
	if err := w.sc.exec(w.scCtx, `INSERT INTO bucket(cost, node, rel, frm, kind, depth) VALUES(0, ?, '', '', '', 0)`,
		string(from)); err != nil {
		return err
	}
	for {
		if stop, err := w.checkDeadline(ctx); stop || err != nil {
			return err
		}
		var cost int64
		found, err := w.sc.row(w.scCtx, `SELECT cost FROM bucket ORDER BY cost LIMIT 1`, nil, &cost)
		if err != nil {
			return err
		}
		if !found {
			return nil // the reachable set is exhausted: the target is unreachable
		}
		last := model.NodeID("")
		for {
			if stop, err := w.checkDeadline(ctx); stop || err != nil {
				return err
			}
			chunk, more, err := w.readChunk(cost, last)
			if err != nil {
				return err
			}
			if len(chunk) == 0 {
				break
			}
			last = chunk[len(chunk)-1].node
			settled, err := w.settle(ctx, cost, to, chunk)
			if err != nil {
				return err
			}
			if w.visitedHit || w.targetSettled {
				return nil
			}
			if err := w.expand(ctx, cost, settled); err != nil {
				return err
			}
			if w.edgesHit {
				return nil
			}
			if !more {
				break
			}
		}
		// The phase is complete, so its rows can go: every relaxation it
		// emitted landed in a strictly later bucket, and a resumed sweep of
		// this bucket would settle nothing.
		if err := w.sc.exec(w.scCtx, `DELETE FROM bucket WHERE cost = ?`, cost); err != nil {
			return err
		}
	}
}

// checkDeadline reports that the walk must stop. A caller who cancelled the
// query gets the cancellation; an expired deadline is truncation with whatever
// the search has settled so far.
func (w *pathWalk) checkDeadline(ctx context.Context) (bool, error) {
	err := ctx.Err()
	if err == nil {
		return false, nil
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		return true, model.Canceled(err)
	}
	w.deadlineHit = true
	return true, nil
}

// readChunk reads the next slice of bucket cost, starting after node last, in
// (node, rel) order. The slice is NODE-ALIGNED: a node's row group is never
// split across two chunks, because a split group would settle the node on its
// first half and drop the tied parents in its second -- equal-cost routes would
// vanish and the depth tie-break would be taken over a partial group. The byte
// budget is therefore checked only AT a group boundary, and one group may
// overshoot it; that group is a single node's tied in-edges, not the graph.
func (w *pathWalk) readChunk(cost int64, last model.NodeID) ([]pathGroup, bool, error) {
	var (
		out   []pathGroup
		bytes int64
		more  bool
	)
	err := w.sc.each(w.scCtx, `SELECT node, rel, frm, kind, depth FROM bucket
		WHERE cost = ? AND node > ? ORDER BY node, rel`, []any{cost, string(last)},
		func(scan func(...any) error) error {
			var node, rel, frm, kind string
			var depth int
			if err := scan(&node, &rel, &frm, &kind, &depth); err != nil {
				return internalErr("path scratch read: " + err.Error())
			}
			if len(out) > 0 && out[len(out)-1].node == model.NodeID(node) {
				g := &out[len(out)-1]
				if depth < g.depth {
					// The cost is already tied; the shorter hop count is the
					// depth the depth bound is measured against.
					g.depth = depth
				}
				if rel != "" {
					g.parents = append(g.parents, pathParent{model.RelationID(rel), model.NodeID(frm), model.RelationKind(kind)})
					bytes += pathRowBytes(node, rel, frm)
				}
				return nil
			}
			// A new group: this is the only place the chunk may be cut.
			if bytes >= w.frontierBytes {
				more = true
				return errPathChunkFull
			}
			g := pathGroup{node: model.NodeID(node), depth: depth}
			if rel != "" {
				g.parents = append(g.parents, pathParent{model.RelationID(rel), model.NodeID(frm), model.RelationKind(kind)})
			}
			out = append(out, g)
			bytes += pathRowBytes(node, rel, frm)
			return nil
		})
	if err != nil && !errors.Is(err, errPathChunkFull) {
		return nil, false, err
	}
	if bytes > w.peakChunkBytes {
		w.peakChunkBytes = bytes
	}
	return out, more, nil
}

// errPathChunkFull ends a chunk read at a group boundary. It never leaves
// readChunk.
var errPathChunkFull = errors.New("path chunk is full")

// pathRowBytes charges one bucket row the same conservative per-row overhead
// the frontier estimator uses, so the chunk budget reads in the same units the
// operator set.
func pathRowBytes(node, rel, frm string) int64 {
	return edgeRowOverheadBytes + int64(len(node)+len(rel)+len(frm))
}

// settle makes every group in chunk final at cost, recording its tied parents,
// and returns the nodes that were actually settled here.
//
// The staleness check is the exactness invariant: a node already in `settled`
// was settled in this bucket or an earlier one, so its recorded distance is
// already the cheapest and this group is a superseded relaxation. Re-settling
// it would overwrite a cheaper distance and a correct parent set with a worse
// one, and the enumerated route's cost would be wrong.
func (w *pathWalk) settle(ctx context.Context, cost int64, to model.NodeID, chunk []pathGroup) ([]model.NodeID, error) {
	var out []model.NodeID
	for _, g := range chunk {
		// Checked per GROUP, not per phase: cancellation and deadline latency
		// is then one group's work, not one whole cost bucket's.
		if stop, err := w.checkDeadline(ctx); stop || err != nil {
			return out, err
		}
		var known int64
		stale, err := w.sc.row(w.scCtx, `SELECT dist FROM settled WHERE node = ?`, []any{string(g.node)}, &known)
		if err != nil {
			return nil, err
		}
		if stale {
			continue
		}
		if atBound(w.maxVisited, int(w.visited)) {
			w.visitedHit = true
			return out, nil
		}
		if err := w.sc.exec(w.scCtx, `INSERT INTO settled(node, dist, depth) VALUES(?, ?, ?)`,
			string(g.node), cost, g.depth); err != nil {
			return nil, err
		}
		w.visited++
		for _, p := range g.parents {
			if err := w.sc.exec(w.scCtx, `INSERT OR IGNORE INTO parent(node, rel, frm, kind) VALUES(?, ?, ?, ?)`,
				string(g.node), string(p.rel), string(p.from), string(p.kind)); err != nil {
				return nil, err
			}
		}
		if g.node == to {
			w.targetSettled = true
			return out, nil
		}
		if atBound(w.maxDepth, g.depth) {
			// Cheapest-by-cost may still need more hops than the depth bound
			// allows; the answer is then the cheapest route WITHIN the depth
			// bound, and that is reported as truncation.
			w.depthPruned = true
			continue
		}
		out = append(out, g.node)
	}
	return out, nil
}

// expand reads every outgoing edge of the settled nodes in keyset-paged round
// trips and writes each relaxation into its cost bucket. Nothing is kept in
// heap between batches: the bucket table is the priority queue.
func (w *pathWalk) expand(ctx context.Context, cost int64, nodes []model.NodeID) error {
	for start := 0; start < len(nodes); start += adjacencyBatch {
		if stop, err := w.checkDeadline(ctx); stop || err != nil {
			return err
		}
		end := min(start+adjacencyBatch, len(nodes))
		batch := nodes[start:end]
		depths := make(map[model.NodeID]int, len(batch))
		for _, n := range batch {
			var d int
			if _, err := w.sc.row(w.scCtx, `SELECT depth FROM settled WHERE node = ?`, []any{string(n)}, &d); err != nil {
				return err
			}
			depths[n] = d
		}
		var after model.RelationID
		for {
			rels, err := w.adjacency.Edges(ctx, batch, model.DirectionOutgoing, w.kinds, after, adjacencyBatch)
			if err != nil {
				return err
			}
			// Only an empty page ends the keyset walk: the reader clamps the
			// requested limit down to model.MaxPageItems, so a short page is
			// the normal case rather than the end of the edges. It ends THIS
			// batch's walk, never the expansion: the batches after it still
			// have their own edges to read.
			if len(rels) == 0 {
				break
			}
			for _, r := range rels {
				after = r.ID
				w.spent++
				if w.maxEdges.Exceeded(w.spent) {
					w.edgesHit = true
					return nil
				}
				if err := w.sc.exec(w.scCtx,
					`INSERT OR IGNORE INTO bucket(cost, node, rel, frm, kind, depth) VALUES(?, ?, ?, ?, ?, ?)`,
					cost+Cost(r.Kind), string(r.To), string(r.ID), string(r.From), string(r.Kind),
					depths[r.From]+1); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// markReach records every node from which some shortest route still continues
// to the target, by walking the parent edges BACKWARDS from it. Enumeration
// consults it to prune the dead-end prefixes that would otherwise make the
// walk exponential in a dense DAG. The traversal is SQLite's own recursive
// query, whose working set is a transient index -- with temp_store=FILE that
// index spills to disk, so the marking is bounded like the rest of the state.
func (w *pathWalk) markReach(to model.NodeID) error {
	return w.sc.exec(w.scCtx, `INSERT OR IGNORE INTO reach(node)
		WITH RECURSIVE back(n) AS (
			SELECT ?
			UNION
			SELECT p.frm FROM parent p JOIN back ON p.node = back.n
		) SELECT n FROM back`, string(to))
}

// admitted returns the edges out of n that lie on a shortest route to the
// target, in RelationID order. The (frm, rel) index answers it directly, so
// enumeration never scans the DAG. The slice is materialized before the caller
// recurses: the scratch holds ONE connection, and a nested query under an open
// row set would wait on it forever.
func (w *pathWalk) admitted(n model.NodeID) ([]model.Relation, error) {
	var out []model.Relation
	err := w.sc.each(w.scCtx, `SELECT p.node, p.rel, p.kind FROM parent p
		WHERE p.frm = ? AND EXISTS(SELECT 1 FROM reach r WHERE r.node = p.node)
		ORDER BY p.rel`, []any{string(n)},
		func(scan func(...any) error) error {
			var node, rel, kind string
			if err := scan(&node, &rel, &kind); err != nil {
				return internalErr("path scratch read: " + err.Error())
			}
			out = append(out, model.Relation{ID: model.RelationID(rel), From: n,
				To: model.NodeID(node), Kind: model.RelationKind(kind)})
			return nil
		})
	return out, err
}

// routes enumerates up to want equal-cost shortest routes out of the on-disk
// shortest-path DAG, in the frozen order: cost ascending (every route here
// shares the settled cost of the target) then the RelationID sequence compared
// lexicographically. A preorder walk that visits each node's edges in
// RelationID order emits the sequences in exactly that order. The second
// return reports that the enumeration budget ran out before the DAG was
// exhausted.
func (w *pathWalk) routes(from, to model.NodeID, want int) ([][]model.Relation, bool, error) {
	if want <= 0 {
		return nil, false, nil
	}
	if err := w.markReach(to); err != nil {
		return nil, false, err
	}
	var out [][]model.Relation
	var cur []model.Relation
	steps := 0
	capped := false
	var walk func(n model.NodeID) error
	walk = func(n model.NodeID) error {
		if len(out) == want || capped {
			return nil
		}
		if n == to {
			out = append(out, append([]model.Relation(nil), cur...))
			return nil
		}
		if atBound(w.maxDepth, len(cur)) || len(cur) >= model.MaxRelationsPerPath {
			return nil
		}
		edges, err := w.admitted(n)
		if err != nil {
			return err
		}
		for _, r := range edges {
			if steps++; steps > pathEnumerationSteps {
				capped = true
				return nil
			}
			cur = append(cur, r)
			err := walk(r.To)
			cur = cur[:len(cur)-1]
			if err != nil {
				return err
			}
			if len(out) == want || capped {
				return nil
			}
		}
		return nil
	}
	if err := walk(from); err != nil {
		return nil, false, err
	}
	return out, capped, nil
}

// scratchDir is where a path search puts its state file: beside the
// continuation spools of the same store, so an operator has one place to look
// for a query's temporary bytes. An engine with no spool store falls back to
// the system temporary directory, which openPathScratch creates privately.
func (e *Engine) scratchDir() string {
	if e.spools == nil {
		return ""
	}
	return e.spools.SortDir()
}
