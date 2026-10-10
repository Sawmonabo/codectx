package graph

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

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
	var cancel context.CancelFunc
	if deadline, bounded := e.queryDeadline(ctx); bounded {
		ctx, cancel = context.WithDeadline(ctx, deadline)
	} else {
		ctx, cancel = context.WithCancel(ctx)
	}
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
	// The empty direction is the outgoing default: every caller that predates
	// the field asked for exactly that, and normalizing here -- rather than at
	// each of the two places the direction is read -- is what keeps the query
	// hash and the walk agreeing on one spelling.
	direction := req.Direction
	if direction == "" {
		direction = model.DirectionOutgoing
	}

	res = model.PathResult{Meta: model.QueryMeta{Binding: e.adjacency.Binding()}}
	// The same disclosure the traversal entries make, from the same place: a
	// second implementation here would be a second shape for one contract.
	caps, err := e.completeness(ctx)
	if err != nil {
		return model.PathResult{}, err
	}
	res.Meta.Completeness = caps

	// A zero-edge route cannot be expressed: RelationPath requires at least one
	// relation. Reporting no route is honest; emitting a path that fails its own
	// validator is not.
	if req.From == req.To {
		return res, nil
	}

	maxDepth := pathBound(req.MaxDepth, e.limits.Depth())
	// The query hash binds a continuation to this exact normalized search, so a
	// cursor presented to a differently filtered or differently depth-bounded
	// one is CTX_CURSOR_INVALID rather than a silently repinned answer.
	queryHash := pathQueryHash(kinds, direction, req.From, req.To, maxDepth.Int())
	var resume *pathResume
	if req.Page.Cursor != "" {
		resume, err = e.resumePath(ctx, req.Page.Cursor, queryHash)
		if err != nil {
			return model.PathResult{}, err
		}
		// The consumed continuation's retention outlives the search: the state
		// directory IS the search, and when this page also ends early it is
		// adopted under a fresh lease before this release runs. Deferring it
		// here -- not at the end of the happy path -- is what keeps an error
		// return from pinning a generation for the whole cursor TTL.
		// Terminal outcomes only: a retryable failure (a busy store, a
		// transient read error) must leave the state adoptable, because the
		// caller's retry presents this same cursor. Releasing unconditionally
		// made an hours-long search unrecoverable on one contended page.
		defer func() {
			if terminalOutcome(err) {
				resume.Release()
			}
		}()
	}

	// The search state is a private scratch file. A fresh search owns it for
	// the life of this request and it is removed with the request, whatever
	// happened; a page that ends early hands it to the spool store instead
	// (nextPathCursor), and detach is what makes the close below a no-op.
	// Uncancellable: a scratch write that stops halfway because the deadline
	// landed between two statements would fail the request instead of
	// truncating the answer. w.checkDeadline is the only stop.
	scCtx := context.WithoutCancel(ctx)
	var sc *pathScratch
	if resume != nil {
		sc, err = reopenPathScratch(scCtx, resume.Dir)
	} else {
		sc, err = openPathScratch(scCtx, e.scratchDir())
	}
	if err != nil {
		return model.PathResult{}, err
	}
	// Terminal outcomes only, exactly as the lease release above and for the
	// same reason: on a retryable failure the caller presents THIS cursor
	// again, and the cursor names this directory. Keeping the lease while
	// removing the state it leases turned a busy page into
	// CTX_CURSOR_INVALID -- A15. retain() rolls the half-written page back and
	// leaves the directory as the previous page committed it, so the retry
	// resumes from the pre-page state rather than from a page torn in half.
	//
	// A FRESH search has no such obligation: no cursor names its surface yet
	// and nothing can adopt it, so close() gives it straight back to the pool
	// for the next query to write over.
	defer func() {
		if resume != nil && !terminalOutcome(err) {
			// A rollback that itself failed leaves retain() holding the
			// directory back, and close() below then removes it: a retry
			// refused as CTX_CURSOR_INVALID is recoverable by re-running the
			// search, a retry resumed from a half-undone one is not. The
			// caller is told so -- the failure is folded into the answer as a
			// TERMINAL one, which also lets the release above end the lease
			// over state that no longer exists.
			rerr := sc.retain()
			if rerr == nil {
				return
			}
			err = terminalRetention(errors.Join(err, rerr))
		}
		sc.close()
	}()

	codes, absent := kindCodesFor(e.reader, kinds)
	descent := pathDescentFor(e.reader, direction, kinds)
	w := &pathWalk{
		descent:     descent,
		reader:      e.reader,
		kinds:       kinds,
		kindCodes:   codes,
		kindsAbsent: absent,
		direction:   direction,
		maxDepth:    maxDepth,
		// Both work budgets are PER-PAGE, the traversal precedent: a page that
		// spends one ends there and returns a continuation, and the next page
		// carries on with a refilled allowance. The cumulative spend is carried
		// by the cursor and reported on the answer, so no page resets it.
		maxVisited:    pathBound(req.MaxVisited, e.limits.Visited()),
		maxEdges:      e.limits.Edges(),
		frontierBytes: e.limits.FrontierBytes,
		resumed:       resume != nil,
		sc:            sc,
		scCtx:         scCtx,
	}
	if resume != nil {
		// By ASSIGNMENT, so presenting one page's cursor twice neither resets
		// the counters nor doubles them.
		w.visited, w.spent = resume.Cursor.Visited, resume.Cursor.Edges
		w.bucketCost, w.lastSettled = resume.Cursor.BucketCost, resume.Cursor.LastNode
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
	// A page that ended on a budget rather than on the answer owes a
	// continuation: the deadline and the two work budgets end the PAGE, never
	// the search. It is minted AFTER the routes are enumerated, because
	// detaching the state directory closes the database they are read from.
	if w.resumable() {
		res.Meta.NextCursor, err = e.nextPathCursor(ctx, sc, queryHash, w)
		if err != nil {
			return model.PathResult{}, err
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
		// A name-only hop is offered only when the search proved there is no
		// precise route: an exhausted search, not one a budget or the clock
		// stopped, which owes a continuation instead.
		if res.Meta.Truncated || res.Meta.NextCursor != "" || !slices.Contains(kinds, model.RelCalls) {
			return res, nil
		}
		return e.possiblePath(ctx, res, req.From, req.To, direction, w.kindCodes, w.descent, maxDepth, w.maxVisited)
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
	var onRoutes []model.Relation
	for _, route := range routes {
		onRoutes = append(onRoutes, route...)
	}
	where, err := e.locate(ctx, ids, onRoutes)
	if err != nil {
		return model.PathResult{}, err
	}
	res.Locations, res.Occurrences = where.locations, where.occurrences
	res.Meta.Notices = append(res.Meta.Notices, where.notices...)
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
	// reader is the packed per-generation adjacency (ADR-0005 Decision 1): the
	// ONE read path for this search's structure. The external Dijkstra scratch
	// below is unchanged -- Decision 2 moves the path endpoint's adjacency
	// reads and nothing else.
	reader GraphReader
	kinds  []model.RelationKind
	// kindCodes is walk.kinds in the pinned generation's dictionary. A kind the
	// generation never sealed has no code and is simply absent, so an empty
	// kindCodes against a non-empty kinds means "this generation holds no edge
	// of any requested kind" -- NOT "every kind", which is what a nil slice
	// means to the port. kindsAbsent is that distinction, made explicit rather
	// than inferred, because inferring it wrong widens the search silently.
	kindCodes   []KindCode
	kindsAbsent bool
	// direction is the orientation the search relaxes edges in, already
	// normalized by ShortestPath, so it is never the empty value here. It is
	// part of the continuation's query hash: a cursor presented to a search
	// walking the other way would resume a settled set that means nothing in
	// the new orientation.
	direction model.Direction
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
	// resumed marks a search continuing from state a previous page retained.
	resumed bool
	sc      *pathScratch
	// scCtx drives every scratch statement. It is the request context with its
	// cancellation removed, so the deadline ends the search at a checkDeadline
	// and never in the middle of a write to the state file.
	scCtx context.Context

	// visited and spent are CUMULATIVE across every page of this search: the
	// cursor carries them and the answer reports them. pageVisited and
	// pageEdges are THIS page's own spend, and they are what maxVisited and
	// maxEdges are compared against -- the traversal precedent (traverse.go's
	// visit): the two bounds are per-page work budgets that END A PAGE with a
	// continuation, not ceilings that end the search.
	visited int64
	spent   int64

	pageVisited int64
	pageEdges   int64
	// bucketCost and lastSettled are the resume position: the cost bucket the
	// page stopped in and the last node it finished with inside it. A resumed
	// page re-reads that bucket from after lastSettled, so the groups it never
	// reached are still there to settle.
	bucketCost  int64
	lastSettled model.NodeID
	// expandAfter is the scan position inside the batch of pending nodes whose
	// edges are being read -- the (owner, list index) of the next entry the
	// packed adjacency must deliver -- mirrored into the scratch so a resumed
	// page does not re-read, and re-charge, edges an earlier page already
	// spent. It is a position in the CURRENT batch's scan and is cleared when
	// that batch is fully expanded.
	expandAfter EdgePos
	// peakChunkBytes is the high-water mark of one chunk, the structural
	// memory assertion a test reads: it must stay inside the frontier budget
	// (plus the one group that budget is allowed to overshoot for) however
	// large the graph is.
	peakChunkBytes int64

	// descent is the forward call walk's downward step: an outgoing search
	// over `calls` also descends from a method to each method overriding
	// it, at the cost of an `overrides` hop, because a virtual call into the
	// method may run any of them (ADR-0015 decision 2).
	descent pathDescent

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
	if !w.resumed {
		// The source is seeded as a parentless bucket-0 row, so the phase loop
		// below has exactly one shape: every node, source included, is settled
		// out of a bucket.
		if err := w.sc.exec(w.scCtx, `INSERT INTO bucket(cost, node, rel, frm, kind, depth) VALUES(0, ?, '', '', '', 0)`,
			string(from)); err != nil {
			return err
		}
	} else if err := w.loadProgress(); err != nil {
		return err
	}
	// A resumed page drains what the page before it settled but did not finish
	// expanding BEFORE it reads a bucket. Those nodes are already in `settled`,
	// so re-reading their bucket rows would skip them as stale and their
	// outgoing edges would never be read at all.
	if err := w.drain(ctx); err != nil {
		return err
	}
	if w.stopped() {
		return nil
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
		// A new phase restarts the resume position: lastSettled names a node
		// inside bucketCost and means nothing in any other bucket.
		if cost != w.bucketCost {
			w.bucketCost, w.lastSettled = cost, ""
		}
		for {
			if stop, err := w.checkDeadline(ctx); stop || err != nil {
				return err
			}
			chunk, more, err := w.readChunk(cost, w.lastSettled)
			if err != nil {
				return err
			}
			if len(chunk) == 0 {
				break
			}
			settled, err := w.settle(ctx, cost, to, chunk)
			if err != nil {
				return err
			}
			if err := w.markPending(settled); err != nil {
				return err
			}
			if w.stopped() {
				return nil
			}
			if err := w.drain(ctx); err != nil {
				return err
			}
			if w.stopped() {
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

// stopped reports that the page is over: the answer is found, or one of the
// three page budgets ran out. Each of the latter mints a continuation.
func (w *pathWalk) stopped() bool {
	return w.targetSettled || w.deadlineHit || w.visitedHit || w.edgesHit
}

// resumable reports that the search has work left and the page ended on a
// budget rather than on the answer, which is exactly when a continuation is
// owed. A depth-pruned search is NOT resumable: the depth bound is part of the
// query the cursor is bound to, so a page minted for it could only resume a
// search already past it.
func (w *pathWalk) resumable() bool {
	return !w.targetSettled && (w.deadlineHit || w.visitedHit || w.edgesHit)
}

// loadProgress restores what the previous page left in the state file: the
// expansion keyset position and the depth-pruned disclosure.
func (w *pathWalk) loadProgress() error {
	var after string
	var pruned int
	if _, err := w.sc.row(w.scCtx, `SELECT expand_after, depth_pruned FROM progress WHERE k = 0`,
		nil, &after, &pruned); err != nil {
		return err
	}
	pos, err := decodeEdgePos(after)
	if err != nil {
		return err
	}
	w.expandAfter = pos
	w.depthPruned = pruned != 0
	return nil
}

// saveProgress writes it back. It is called on every edge page and at every
// stop, so no page ever loses more than the batch page it was inside.
func (w *pathWalk) saveProgress() error {
	pruned := 0
	if w.depthPruned {
		pruned = 1
	}
	return w.sc.exec(w.scCtx, `UPDATE progress SET expand_after = ?, depth_pruned = ? WHERE k = 0`,
		encodeEdgePos(w.expandAfter), pruned)
}

// markPending records the nodes settled here as owing an expansion.
func (w *pathWalk) markPending(nodes []model.NodeID) error {
	for _, n := range nodes {
		if err := w.sc.exec(w.scCtx, `INSERT OR IGNORE INTO pending(node) VALUES(?)`, string(n)); err != nil {
			return err
		}
	}
	return nil
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
			// A superseded relaxation is finished with, so the resume position
			// advances past it: a continuation must not re-read it forever.
			w.lastSettled = g.node
			continue
		}
		// Compared against THIS page's spend, not the search's: the bound is a
		// per-page work budget that ends the page with a continuation. The test
		// is on the spend this group would take the page to, and it runs BEFORE
		// the group is settled, so the resume position still names the group
		// before it and nothing is skipped.
		if w.maxVisited.Exceeded(w.pageVisited + 1) {
			w.visitedHit = true
			return out, nil
		}
		if err := w.sc.exec(w.scCtx, `INSERT INTO settled(node, dist, depth) VALUES(?, ?, ?)`,
			string(g.node), cost, g.depth); err != nil {
			return nil, err
		}
		w.visited++
		w.pageVisited++
		w.lastSettled = g.node
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
			if err := w.saveProgress(); err != nil {
				return nil, err
			}
			continue
		}
		out = append(out, g.node)
	}
	return out, nil
}

// drain expands every node that has been settled but not yet expanded, in
// batched scans of the packed adjacency, writing each relaxation into its cost
// bucket. Nothing is kept in heap between batches: the pending table is the
// work queue and the bucket table is the priority queue, and the only heap the
// scan itself holds is one relaxation batch of at most adjacencyBatch entries.
//
// A node leaves `pending` only once its whole edge scan is done, and the scan's
// own position is saved after every batch, so a page that stops here resumes at
// the edge after the last one it charged -- it neither loses edges nor pays for
// them twice.
func (w *pathWalk) drain(ctx context.Context) error {
	for {
		if stop, err := w.checkDeadline(ctx); stop || err != nil {
			return err
		}
		batch, dists, depths, err := w.takePending()
		if err != nil {
			return err
		}
		if len(batch) == 0 {
			return nil
		}
		stopped, err := w.expandBatch(ctx, batch, dists, depths)
		if err != nil {
			return err
		}
		if stopped {
			// A budget or the deadline ended the page mid-batch. The position
			// saved by expandBatch names the entry that was NOT charged, and
			// the batch keeps its `pending` rows, so the next page resumes
			// exactly there.
			return nil
		}
		// The batch is fully expanded: its nodes owe nothing more, and the next
		// batch starts its own scan from the beginning.
		for _, n := range batch {
			if err := w.sc.exec(w.scCtx, `DELETE FROM pending WHERE node = ?`, string(n)); err != nil {
				return err
			}
		}
		w.expandAfter = EdgePos{}
		if err := w.saveProgress(); err != nil {
			return err
		}
	}
}

// expandBatch reads the packed lists of one pending batch in ONE resumable
// scan and relaxes every entry it delivers. It reports whether the page ended
// before the batch did.
//
// The scan is resumable rather than re-startable: the port delivers entries in
// (owner, list index) order and expandAfter names the next one it owes, so a
// page that stops on a budget, a deadline or the relaxation of a batch charges
// each entry exactly once across every page of the search.
func (w *pathWalk) expandBatch(ctx context.Context, batch []model.NodeID,
	dists map[model.NodeID]int64, depths map[model.NodeID]int) (bool, error) {
	if w.kindsAbsent {
		// The query names kinds and this generation seals none of them: there
		// is no edge to read. Reporting it as a finished scan is right -- the
		// batch owes nothing -- and passing an empty kind list to the port
		// would instead mean every kind.
		return false, nil
	}
	var plain, dispatched []model.NodeID
	for _, n := range batch {
		if _, virtual := pathState(n); virtual {
			dispatched = append(dispatched, n)
		} else {
			plain = append(plain, n)
		}
	}
	if err := w.expandDispatched(ctx, dispatched, dists, depths); err != nil {
		return false, err
	}
	refs, byRef, err := w.resolveBatch(ctx, plain)
	if err != nil {
		return false, err
	}
	if len(refs) == 0 {
		return false, nil
	}
	pend := make([]Edge, 0, adjacencyBatch)
	stopped := false
	var ferr error
	flush := func() error {
		if len(pend) == 0 {
			return nil
		}
		if err := w.relax(ctx, pend, byRef, dists, depths); err != nil {
			return err
		}
		pend = pend[:0]
		return nil
	}
	pos, err := w.reader.Neighbours(ctx, refs, w.direction, w.kindCodes, w.expandAfter,
		func(e Edge) error {
			// Both stops are checked BEFORE the entry is consumed, so the
			// position the port reports for a stopped scan -- the entry it
			// just delivered -- still names the first entry this page did NOT
			// charge, and the page that resumes there charges it once.
			if w.maxEdges.Exceeded(w.pageEdges + 1) {
				w.edgesHit, stopped = true, true
				return ErrStopScan
			}
			// The deadline is checked at a batch boundary -- an empty pend is
			// exactly one -- which is the only place the saved position is
			// consistent with what the buckets hold. A cancellation is
			// classified by checkDeadline exactly as it was when each batch
			// was its own round trip.
			if len(pend) == 0 {
				var derr error
				if stopped, derr = w.checkDeadline(ctx); derr != nil {
					ferr = derr
					return ErrStopScan
				}
				if stopped {
					return ErrStopScan
				}
			}
			w.spent++
			w.pageEdges++
			pend = append(pend, e)
			if len(pend) < adjacencyBatch {
				return nil
			}
			if ferr = flush(); ferr != nil {
				return ErrStopScan
			}
			return nil
		})
	if err != nil {
		if ctx.Err() == nil {
			return false, err
		}
		// The port surfaces context errors RAW (graphreader.go); classifying
		// them is the walk's job, and a deadline that lands inside the scan
		// ends the page exactly as one that lands between two of them does.
		// The position the port reports is still the last entry it delivered,
		// so the tail buffered below is relaxed rather than charged and lost.
		stop, derr := w.checkDeadline(ctx)
		if derr != nil {
			return false, derr
		}
		if !stop {
			return false, err
		}
		stopped = true
	}
	// The position is adopted before the tail is relaxed and saved after it:
	// both writes land in the same scratch transaction, so a page never
	// publishes a position ahead of the relaxations it stands for.
	w.expandAfter = pos
	if ferr != nil {
		return false, ferr
	}
	if err := flush(); err != nil {
		return false, err
	}
	return stopped, w.saveProgress()
}

// resolveBatch maps the batch's canonical node ids onto the surrogates the
// packed adjacency is keyed by, ASCENDING and duplicate-free as the port
// requires, and returns the reverse map the relaxation reads owners through.
//
// A node the pinned generation does not publish resolves to zero and is
// dropped: it has no packed list, and the search has nothing to expand for it.
func (w *pathWalk) resolveBatch(ctx context.Context, batch []model.NodeID) ([]NodeRef,
	map[NodeRef]model.NodeID, error) {
	resolved, err := w.reader.Resolve(ctx, batch)
	if err != nil {
		return nil, nil, err
	}
	byRef := make(map[NodeRef]model.NodeID, len(resolved))
	refs := make([]NodeRef, 0, len(resolved))
	for i, ref := range resolved {
		if ref == 0 {
			continue
		}
		if _, dup := byRef[ref]; dup {
			continue
		}
		byRef[ref] = batch[i]
		refs = append(refs, ref)
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i] < refs[j] })
	return refs, byRef, nil
}

// relax turns one batch of packed entries into bucket rows. The canonical ids
// the scratch is keyed by are resolved for the WHOLE batch in two batched
// primary-key reads before any row is inserted, never one read per entry, and
// the settle order therefore still ties on the content-derived canonical node
// id rather than on the rebuild-local surrogate the scan carries.
//
// Which endpoint the search came from is the entry's OWNER: the owner is the
// node this batch settled, whichever direction its list was read in. It is
// exact where guessing from the two endpoints is not: a self-loop, and an edge
// whose endpoints are both settled in this batch, relax from the node whose
// expansion actually delivered them.
func (w *pathWalk) relax(ctx context.Context, pend []Edge, byRef map[NodeRef]model.NodeID,
	dists map[model.NodeID]int64, depths map[model.NodeID]int) error {
	rels := make([]RelRef, len(pend))
	tos := make([]NodeRef, len(pend))
	for i, e := range pend {
		rels[i], tos[i] = e.Rel, e.Neighbour
	}
	relIDs, err := w.reader.RelationIDs(ctx, rels)
	if err != nil {
		return err
	}
	toIDs, err := w.reader.NodeIDs(ctx, tos)
	if err != nil {
		return err
	}
	virtual, err := w.virtualCalls(ctx, pend, relIDs)
	if err != nil {
		return err
	}
	for i, e := range pend {
		from, ok := byRef[e.Owner]
		if !ok {
			// The port delivered an entry for a node this batch is not
			// expanding, which is nothing this search can relax.
			continue
		}
		to, rel := toIDs[i], relIDs[i]
		if to == "" || rel == "" {
			// An endpoint or a relation the pinned generation does not
			// publish. It is not an answerable edge, and inserting a bucket
			// row keyed by an empty id would settle a node that does not
			// exist.
			continue
		}
		kind, ok := w.reader.Kinds().Kind(e.Kind)
		if !ok {
			continue
		}
		if err := w.sc.exec(w.scCtx,
			`INSERT OR IGNORE INTO bucket(cost, node, rel, frm, kind, depth) VALUES(?, ?, ?, ?, ?, ?)`,
			dists[from]+Cost(kind), string(to), string(rel), string(from), string(kind),
			depths[from]+1); err != nil {
			return err
		}
		if _, descending := pathState(from); virtual[rel] || descending {
			// A call that can dispatch reaches the callee a second way: as
			// the method whose overrides it may run, at this call's own
			// cost, whatever cheaper way the callee itself is reached. An
			// override a descent reached is reached that way too, and may
			// itself be overridden.
			if err := w.sc.exec(w.scCtx,
				`INSERT OR IGNORE INTO bucket(cost, node, rel, frm, kind, depth) VALUES(?, ?, ?, ?, ?, ?)`,
				dists[from]+Cost(kind), string(dispatchState(to)), string(rel), string(from), string(kind),
				depths[from]+1); err != nil {
				return err
			}
		}
	}
	return nil
}

// dispatchSuffix marks a path search's second state of a method: reached by
// a call that can dispatch, from which the search descends to the method's
// overrides. A canonical id is hex, so the suffix never names a node.
const dispatchSuffix = "~dispatch"

// dispatchState is the dispatch state of method n.
func dispatchState(n model.NodeID) model.NodeID { return n + dispatchSuffix }

// pathState reads a search state back to its node and whether it is the
// dispatch state.
func pathState(n model.NodeID) (model.NodeID, bool) {
	if node, ok := strings.CutSuffix(string(n), dispatchSuffix); ok {
		return model.NodeID(node), true
	}
	return n, false
}

// virtualCalls is which of a relaxation batch's outgoing `calls` relations
// have a virtual occurrence, in one batched read; nothing when the search
// takes no downward step.
func (w *pathWalk) virtualCalls(ctx context.Context, pend []Edge, relIDs []model.RelationID) (map[model.RelationID]bool, error) {
	if !w.descent.active {
		return map[model.RelationID]bool{}, nil
	}
	return virtualCalls(ctx, w.reader, pend, relIDs)
}

// virtualCalls is which of edges' outgoing `calls` relations, named by relIDs
// in the same order, have a virtual occurrence, read in batches; nothing from
// a reader that publishes no dispatch.
func virtualCalls(ctx context.Context, r GraphReader, edges []Edge, relIDs []model.RelationID) (map[model.RelationID]bool, error) {
	out := map[model.RelationID]bool{}
	dr, _ := r.(DispatchReader)
	calls, ok := r.Kinds().Code(model.RelCalls)
	if dr == nil || !ok {
		return out, nil
	}
	var ids []model.RelationID
	for i, e := range edges {
		if e.Outgoing && e.Kind == calls && relIDs[i] != "" {
			ids = append(ids, relIDs[i])
		}
	}
	for _, chunk := range impactChunkRelations(dedupeRelations(ids)) {
		sums, err := dr.RelationDispatch(ctx, chunk)
		if err != nil {
			return nil, err
		}
		if len(sums) != len(chunk) {
			return nil, internalErr("graph: the reader summarized a different number of relations than it was given")
		}
		for i, id := range chunk {
			if sums[i].Virtual {
				out[id] = true
			}
		}
	}
	return out, nil
}

// expandDispatched expands the dispatch states of a batch: each relaxes the
// overrides of its method, one hop down, and nothing else -- the method's own
// calls are its plain state's. The scan is read whole: an overrides list is
// short, and its relaxations are idempotent rows, so a page that stops later
// in the batch and repeats this costs nothing twice.
func (w *pathWalk) expandDispatched(ctx context.Context, states []model.NodeID,
	dists map[model.NodeID]int64, depths map[model.NodeID]int) error {
	if len(states) == 0 || !w.descent.active {
		return nil
	}
	methods := make([]model.NodeID, len(states))
	for i, n := range states {
		methods[i], _ = pathState(n)
	}
	resolved, err := w.reader.Resolve(ctx, methods)
	if err != nil {
		return err
	}
	byRef := map[NodeRef]model.NodeID{}
	var refs []NodeRef
	for i, ref := range resolved {
		if ref != 0 {
			byRef[ref] = states[i]
			refs = append(refs, ref)
		}
	}
	if len(refs) == 0 {
		return nil
	}
	var pend []Edge
	if _, err := w.reader.Neighbours(ctx, ascending(refs), model.DirectionIncoming, []KindCode{w.descent.overrides},
		EdgePos{}, func(e Edge) error { pend = append(pend, e); return nil }); err != nil {
		return err
	}
	// Charged to the edge budget like every entry the search reads. The scan
	// is not cut on it -- a repeat would charge it again -- so the page's
	// next scan, which is resumable, is the one the budget stops.
	w.spent += int64(len(pend))
	w.pageEdges += int64(len(pend))
	for start := 0; start < len(pend); start += adjacencyBatch {
		if err := w.relax(ctx, pend[start:min(start+adjacencyBatch, len(pend))], byRef, dists, depths); err != nil {
			return err
		}
	}
	return nil
}

// takePending reads the next batch of unexpanded nodes with the settled
// distance and depth each relaxation is measured from. The batch is read in
// node order and bounded by adjacencyBatch, so peak heap here is one batch.
func (w *pathWalk) takePending() ([]model.NodeID, map[model.NodeID]int64, map[model.NodeID]int, error) {
	var batch []model.NodeID
	dists := map[model.NodeID]int64{}
	depths := map[model.NodeID]int{}
	err := w.sc.each(w.scCtx, `SELECT p.node, s.dist, s.depth FROM pending p JOIN settled s ON s.node = p.node
		ORDER BY p.node LIMIT ?`, []any{adjacencyBatch},
		func(scan func(...any) error) error {
			var node string
			var dist int64
			var depth int
			if err := scan(&node, &dist, &depth); err != nil {
				return internalErr("path scratch read: " + err.Error())
			}
			batch = append(batch, model.NodeID(node))
			dists[model.NodeID(node)] = dist
			depths[model.NodeID(node)] = depth
			return nil
		})
	if err != nil {
		return nil, nil, nil, err
	}
	return batch, dists, depths, nil
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
			route := make([]model.Relation, len(cur))
			for i, r := range cur {
				r.From, _ = pathState(r.From)
				r.To, _ = pathState(r.To)
				route[i] = r
			}
			out = append(out, route)
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

// scratchDir is the directory whose scratch pool a path search takes its state
// file from: the one holding the continuation spools of the same store, so an
// operator has one place to look for a query's working bytes. An engine with
// no spool store has no pool and falls back to the system temporary
// directory, which openPathScratch creates privately.
func (e *Engine) scratchDir() string {
	if e.spools == nil {
		return ""
	}
	return e.spools.SortDir()
}

// pathDescent is an outgoing call search's downward step. A call that can
// dispatch reaches its callee's dispatch state too, and a dispatch state
// relaxes only the method's incoming `overrides` entries, each to the
// override itself and to the override's own dispatch state, so a descent
// goes on as deep as the overrides do. A node's own scan reads only its
// outgoing entries of the requested kinds. It is inactive for every other
// search.
type pathDescent struct {
	active    bool
	overrides KindCode
}

func pathDescentFor(r GraphReader, direction model.Direction, kinds []model.RelationKind) pathDescent {
	if direction != model.DirectionOutgoing || !slices.Contains(kinds, model.RelCalls) {
		return pathDescent{}
	}
	code, ok := r.Kinds().Code(model.RelOverrides)
	if !ok {
		return pathDescent{}
	}
	return pathDescent{active: true, overrides: code}
}

// possiblePath answers a pair with no precise route (ADR-0015 decision 3) by
// the cheapest route once name-only calls are admitted: one cost-ordered
// search from `from` over the request's relation kinds -- with the forward
// call walk's descent to overrides, as the precise search has -- that also
// steps along `may_call`, from a node holding an open call site to each
// candidate of it (against the edges for an incoming search, either way for
// both). A `may_call` step costs the dearest relation cost there is, so the
// search takes as many of them as the cheapest route needs and no more. The
// route is marked Possible, carries every hop's call site occurrences, and the
// answer counts its hops and says it may not run.
//
// The search is bounded by the request's depth bound and its visited budget;
// one either bound stops is reported truncated, never as "no route".
func (e *Engine) possiblePath(ctx context.Context, res model.PathResult, from, to model.NodeID,
	direction model.Direction, codes []KindCode, descent pathDescent,
	maxDepth, maxVisited config.Limit) (model.PathResult, error) {
	dr := declarationReader(e.reader)
	if dr == nil {
		return res, nil
	}
	s := &possibleSearch{e: e, dr: dr, to: to, direction: direction, codes: codes, descent: descent,
		maxDepth: maxDepth, maxVisited: maxVisited, tree: possibleTree{from: {}}}
	found, err := s.run(ctx, from)
	if err != nil {
		return model.PathResult{}, err
	}
	switch {
	case s.cut:
		pathTruncate(&res.Meta, pathReasonVisited)
	case s.pruned:
		pathTruncate(&res.Meta, pathReasonDepth)
	}
	if !found {
		return res, nil
	}
	route := s.tree.chain(to)
	if len(route) > model.MaxRelationsPerPath {
		pathTruncate(&res.Meta, pathReasonDepth)
		return res, nil
	}
	var ids []model.RelationID
	var legs []model.Relation
	hops := 0
	for _, step := range route {
		if step.rel.Kind == model.RelMayCall {
			hops++
			ids = append(ids, step.via...)
			continue
		}
		ids = append(ids, step.rel.ID)
		legs = append(legs, step.rel)
	}
	evidence, err := e.adjacency.EvidenceFor(ctx, ids, model.MaxRelationsPerPath)
	if err != nil {
		return model.PathResult{}, err
	}
	p := model.RelationPath{CostUnits: s.tree[to].cost, Possible: true}
	for _, step := range route {
		p.Relations = append(p.Relations, step.rel.ID)
		of := []model.RelationID{step.rel.ID}
		if step.rel.Kind == model.RelMayCall {
			of = step.via
		}
		for _, id := range of {
			for _, ev := range evidence[id] {
				if len(p.Evidence) < model.MaxRelationsPerPath {
					p.Evidence = append(p.Evidence, ev)
				}
			}
		}
	}
	res.Paths = []model.RelationPath{p}
	nodes := []model.NodeID{from}
	for _, step := range route {
		nodes = append(nodes, step.rel.From, step.rel.To)
	}
	nodes = sortedNodes(nodes)
	if res.Nodes, err = e.adjacency.NodesByID(ctx, nodes); err != nil {
		return model.PathResult{}, err
	}
	where, err := e.locate(ctx, nodes, legs)
	if err != nil {
		return model.PathResult{}, err
	}
	res.Locations, res.Occurrences = where.locations, where.occurrences
	res.Meta.Notices = append(res.Meta.Notices, where.notices...)
	if loc, ok := e.reader.(SourceLocator); ok {
		for _, step := range route {
			if step.rel.Kind != model.RelMayCall {
				continue
			}
			for _, v := range step.via {
				listed, total, err := e.relationSites(ctx, loc, v, occurrenceBudget-len(res.Occurrences))
				if err != nil {
					return model.PathResult{}, err
				}
				for _, o := range listed {
					o.RelationID = step.rel.ID
					res.Occurrences = append(res.Occurrences, o)
				}
				if int64(len(listed)) < total {
					res.Meta.Notices = append(res.Meta.Notices, fmt.Sprintf(
						"the possible route's call site relation %s has %d located occurrences; %d are listed", v, total, len(listed)))
				}
			}
		}
	}
	calls := "one call"
	if hops > 1 {
		calls = fmt.Sprintf("%d calls", hops)
	}
	res.Meta.Notices = append(res.Meta.Notices, fmt.Sprintf(
		"no precise route connects the two nodes; the route given takes %s no analysis bound, matched by "+
			"name only (`may_call`), and it may not run", calls))
	return res, nil
}

// possibleStep is one state of the possible-route search: its cost and depth
// from the start, the relation and state it was reached by, and for a
// `may_call` step the call site relations that name it.
type possibleStep struct {
	cost  int64
	depth int
	rel   model.Relation
	via   []model.RelationID
	prev  model.NodeID
}

// possibleTree is the search's states, keyed as the precise search keys them:
// a node, or a method's dispatch state (dispatchState).
type possibleTree map[model.NodeID]possibleStep

// chain is the steps from the start to n, in order, with every state read
// back to its node.
func (t possibleTree) chain(n model.NodeID) []possibleStep {
	var out []possibleStep
	for cur := t[n]; cur.prev != ""; cur = t[cur.prev] {
		step := cur
		step.rel.From, _ = pathState(step.rel.From)
		step.rel.To, _ = pathState(step.rel.To)
		out = append(out, step)
	}
	slices.Reverse(out)
	return out
}

// possibleSearch is one possible-route search.
type possibleSearch struct {
	e          *Engine
	dr         DeclarationReader
	to         model.NodeID
	direction  model.Direction
	codes      []KindCode
	descent    pathDescent
	maxDepth   config.Limit
	maxVisited config.Limit
	tree       possibleTree
	buckets    map[int64][]model.NodeID
	settled    map[model.NodeID]bool
	// cut is the visited budget stopping the search, pruned the depth bound
	// leaving a state unexpanded.
	cut, pruned bool
}

// run settles states cheapest first until the target settles, the frontier
// empties, or the budget stops it; it reports whether the target settled.
func (s *possibleSearch) run(ctx context.Context, from model.NodeID) (bool, error) {
	s.buckets = map[int64][]model.NodeID{0: {from}}
	s.settled = map[model.NodeID]bool{}
	for len(s.buckets) > 0 {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		var cost int64 = -1
		for c := range s.buckets {
			if cost < 0 || c < cost {
				cost = c
			}
		}
		var batch []model.NodeID
		for _, n := range sortedNodes(s.buckets[cost]) {
			if !s.settled[n] && s.tree[n].cost == cost {
				batch = append(batch, n)
			}
		}
		delete(s.buckets, cost)
		var expand []model.NodeID
		for _, n := range batch {
			if s.maxVisited.Exceeded(int64(len(s.settled) + 1)) {
				s.cut = true
				return false, nil
			}
			s.settled[n] = true
			if n == s.to {
				return true, nil
			}
			if atBound(s.maxDepth, s.tree[n].depth) {
				s.pruned = true
				continue
			}
			expand = append(expand, n)
		}
		if err := s.expand(ctx, expand); err != nil {
			return false, err
		}
	}
	return false, nil
}

// reach records state next reached from state at by rel, unless it is
// settled or already reached at no greater cost.
func (s *possibleSearch) reach(at, next model.NodeID, rel model.Relation, via []model.RelationID) {
	c := s.tree[at].cost + Cost(rel.Kind)
	if cur, seen := s.tree[next]; s.settled[next] || (seen && cur.cost <= c) {
		return
	}
	s.tree[next] = possibleStep{cost: c, depth: s.tree[at].depth + 1, rel: rel, via: via, prev: at}
	s.buckets[c] = append(s.buckets[c], next)
}

// expand steps out of one batch of states: a node along the request's
// relations, the descent and the name-only calls, a dispatch state to its
// method's overrides only.
func (s *possibleSearch) expand(ctx context.Context, batch []model.NodeID) error {
	var plain, dispatched []model.NodeID
	for _, n := range batch {
		if _, virtual := pathState(n); virtual {
			dispatched = append(dispatched, n)
		} else {
			plain = append(plain, n)
		}
	}
	if err := s.adjacent(ctx, plain, s.direction, s.codes, false); err != nil {
		return err
	}
	if s.descent.active {
		if err := s.adjacent(ctx, dispatched, model.DirectionIncoming, []KindCode{s.descent.overrides}, true); err != nil {
			return err
		}
	}
	return s.nameOnly(ctx, plain)
}

// adjacent relaxes the stored relations out of states as the precise search
// does: a call that can dispatch also reaches its callee's dispatch state, and
// an override a dispatch state reached also reaches its own.
func (s *possibleSearch) adjacent(ctx context.Context, states []model.NodeID, direction model.Direction,
	kinds []KindCode, fromDispatch bool) error {
	if len(states) == 0 || len(kinds) == 0 {
		return nil
	}
	r := s.e.reader
	nodes := make([]model.NodeID, len(states))
	for i, n := range states {
		nodes[i], _ = pathState(n)
	}
	refs, err := r.Resolve(ctx, nodes)
	if err != nil {
		return err
	}
	owner := map[NodeRef]model.NodeID{}
	var live []NodeRef
	for i, ref := range refs {
		if ref == 0 {
			continue
		}
		owner[ref] = states[i]
		live = append(live, ref)
	}
	if len(live) == 0 {
		return nil
	}
	var edges []Edge
	if _, err := r.Neighbours(ctx, ascending(live), direction, kinds, EdgePos{}, func(ed Edge) error {
		edges = append(edges, ed)
		return nil
	}); err != nil {
		return err
	}
	for start := 0; start < len(edges); start += model.MaxPageItems {
		chunk := edges[start:min(start+model.MaxPageItems, len(edges))]
		rels := make([]RelRef, len(chunk))
		others := make([]NodeRef, len(chunk))
		for i, ed := range chunk {
			rels[i], others[i] = ed.Rel, ed.Neighbour
		}
		relIDs, err := r.RelationIDs(ctx, rels)
		if err != nil {
			return err
		}
		otherIDs, err := r.NodeIDs(ctx, others)
		if err != nil {
			return err
		}
		virtual := map[model.RelationID]bool{}
		if s.descent.active {
			if virtual, err = virtualCalls(ctx, r, chunk, relIDs); err != nil {
				return err
			}
		}
		for i, ed := range chunk {
			kind, ok := r.Kinds().Kind(ed.Kind)
			if !ok || relIDs[i] == "" || otherIDs[i] == "" {
				continue
			}
			at, next := owner[ed.Owner], otherIDs[i]
			here, _ := pathState(at)
			rel := model.Relation{ID: relIDs[i], From: here, Kind: kind, To: next}
			if !ed.Outgoing {
				rel.From, rel.To = next, here
			}
			s.reach(at, next, rel, nil)
			if virtual[relIDs[i]] || fromDispatch {
				s.reach(at, dispatchState(next), rel, nil)
			}
		}
	}
	return nil
}

// nameOnly relaxes the `may_call` steps out of nodes: forward to each
// candidate of an open call site a node holds, backward to each holder of an
// open call site a node is a candidate of. A step names every call site
// relation that makes it.
func (s *possibleSearch) nameOnly(ctx context.Context, nodes []model.NodeID) error {
	if len(nodes) == 0 {
		return nil
	}
	nodes = sortedNodes(nodes)
	type hop struct{ at, next, caller, callee model.NodeID }
	vias := map[hop][]model.RelationID{}
	var order []hop
	note := func(h hop, via model.RelationID) {
		if _, ok := vias[h]; !ok {
			order = append(order, h)
		}
		if !slices.Contains(vias[h], via) {
			vias[h] = append(vias[h], via)
		}
	}
	if s.direction != model.DirectionIncoming {
		if err := s.e.candidatesOfSites(ctx, s.e.reader, s.dr, nodes, map[model.NodeID]bool{},
			func(c, k model.NodeID, via model.RelationID) {
				note(hop{at: c, next: k, caller: c, callee: k}, via)
			}); err != nil {
			return err
		}
	}
	if s.direction != model.DirectionOutgoing {
		if err := s.e.callersOfSites(ctx, s.e.reader, s.dr, nodes, map[model.NodeID]bool{},
			func(c, k model.NodeID, via model.RelationID) {
				note(hop{at: k, next: c, caller: c, callee: k}, via)
			}); err != nil {
			return err
		}
	}
	repo := s.e.reader.Binding().RepositoryID
	for _, h := range order {
		via := vias[h]
		slices.Sort(via)
		s.reach(h.at, h.next, model.Relation{ID: model.NewRelationID(repo, h.caller, model.RelMayCall, h.callee),
			From: h.caller, Kind: model.RelMayCall, To: h.callee}, via)
	}
	return nil
}
