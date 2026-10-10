package graph

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
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
	pathReasonRouteEnd = "more equal-cost routes remain and this workspace offers no continuation to list them"
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
		w.routing = resume.Cursor.Routes
		w.routeAfter, w.routesListed = resume.Cursor.After, resume.Cursor.Listed
		w.routeCount, w.routeCountAtLeast = resume.Cursor.Count, resume.Cursor.AtLeast
	}
	if w.routing {
		// A continuation of the route list: the search is finished and its
		// shortest-path graph is in the state; only the list goes on.
		if err := w.loadProgress(); err != nil {
			return model.PathResult{}, err
		}
		w.targetSettled = true
	} else if err := w.run(ctx, req.From, req.To); err != nil {
		return model.PathResult{}, err
	}
	// A name-only call is admitted only when the search proved there is no
	// precise route: an exhausted search, not one a budget, the clock or the
	// depth bound stopped, which owes a continuation or a truncation instead.
	if !w.routing && !w.possible && !w.targetSettled && !w.resumable() && !w.depthPruned &&
		slices.Contains(kinds, model.RelCalls) && declarationReader(e.reader) != nil {
		if err := w.startPossible(); err != nil {
			return model.PathResult{}, err
		}
		if err := w.run(ctx, req.From, req.To); err != nil {
			return model.PathResult{}, err
		}
	}
	res.VisitedCount = w.visited

	var routes [][]model.Relation
	var vias map[model.RelationID][]model.RelationID
	if w.targetSettled {
		if !w.routing {
			// The page that settles the target counts its equal-cost routes
			// once, over the shortest-path graph, without listing them.
			if err := w.markReach(req.To); err != nil {
				return model.PathResult{}, err
			}
			if w.routeCount, w.routeCountAtLeast, err = w.countRoutes(ctx, req.From, req.To); err != nil {
				return model.PathResult{}, err
			}
			w.routing = true
		}
		// Every equal-cost route is listed, a page at a time: the request's
		// page limit (the endpoint default when zero), and never more nodes
		// than a page carries. The list resumes after the last route a page
		// finished, so no route is held past the page that lists it.
		limit := req.Page.Limit
		if limit <= 0 {
			limit = model.MaxPageItems
		}
		page, err := w.routePage(req.From, req.To, limit)
		if err != nil {
			return model.PathResult{}, err
		}
		routes = page.routes
		w.routesListed += int64(len(routes))
		if page.capped {
			pathTruncate(&res.Meta, pathReasonRoutes)
		}
		if !page.more && w.routeCountAtLeast {
			// The last page: every route is listed, so the count is exact.
			w.routeCount, w.routeCountAtLeast = w.routesListed, false
		}
		if w.routeCountAtLeast {
			w.routeCount = max(w.routeCount, w.routesListed)
		}
		res.RouteCount, res.RouteCountAtLeast = w.routeCount, w.routeCountAtLeast
		if vias, err = w.hopVias(routes); err != nil {
			return model.PathResult{}, err
		}
		if page.more {
			w.routeAfter = page.after
			if res.Meta.NextCursor, err = e.nextPathCursor(ctx, sc, queryHash, w); err != nil {
				return model.PathResult{}, err
			}
			if res.Meta.NextCursor == "" {
				pathTruncate(&res.Meta, pathReasonRouteEnd)
			}
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
		return res, nil
	}

	paths, err := e.hydratePaths(ctx, routes, vias)
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
		for _, r := range route {
			if r.Kind != model.RelMayCall {
				onRoutes = append(onRoutes, r)
			}
		}
	}
	where, err := e.locate(ctx, ids, onRoutes)
	if err != nil {
		return model.PathResult{}, err
	}
	res.Locations, res.Occurrences = where.locations, where.occurrences
	res.Meta.Notices = append(res.Meta.Notices, where.notices...)
	if w.possible {
		if err := e.locateHops(ctx, &res, vias); err != nil {
			return model.PathResult{}, err
		}
		res.Meta.Notices = append(res.Meta.Notices, possibleNotices(routes)...)
	}
	return res, nil
}

// locateHops lists each name-only step's call sites as the step's own
// occurrences, in step order, within the answer's occurrence budget.
func (e *Engine) locateHops(ctx context.Context, res *model.PathResult, vias map[model.RelationID][]model.RelationID) error {
	loc, ok := e.reader.(SourceLocator)
	if !ok {
		return nil
	}
	hops := make([]model.RelationID, 0, len(vias))
	for hop := range vias {
		hops = append(hops, hop)
	}
	slices.Sort(hops)
	for _, hop := range hops {
		for _, v := range vias[hop] {
			listed, total, err := e.relationSites(ctx, loc, v, occurrenceBudget-len(res.Occurrences))
			if err != nil {
				return err
			}
			for _, o := range listed {
				o.RelationID = hop
				res.Occurrences = append(res.Occurrences, o)
			}
			if int64(len(listed)) < total {
				res.Meta.Notices = append(res.Meta.Notices, fmt.Sprintf(
					"the possible route's call site relation %s has %d located occurrences; %d are listed", v, total, len(listed)))
			}
		}
	}
	return nil
}

// possibleNotices says, for each route the name-only search returned, how
// many calls it takes that no analysis bound.
func possibleNotices(routes [][]model.Relation) []string {
	out := make([]string, 0, len(routes))
	for i, route := range routes {
		hops := 0
		for _, r := range route {
			if r.Kind == model.RelMayCall {
				hops++
			}
		}
		calls := "one call"
		if hops > 1 {
			calls = fmt.Sprintf("%d calls", hops)
		}
		which := "the route given"
		if len(routes) > 1 {
			which = fmt.Sprintf("route %d of %d", i+1, len(routes))
		}
		out = append(out, fmt.Sprintf("no precise route connects the two nodes; %s takes %s no analysis bound, "+
			"matched by name only (`may_call`), and it may not run", which, calls))
	}
	return out
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
//
// A name-only step's evidence is that of the call site relations behind it
// (vias), and a route that takes one is marked Possible.
func (e *Engine) hydratePaths(ctx context.Context, routes [][]model.Relation,
	vias map[model.RelationID][]model.RelationID) ([]model.RelationPath, error) {
	var ids []model.RelationID
	seen := map[model.RelationID]bool{}
	backing := func(r model.Relation) []model.RelationID {
		if r.Kind == model.RelMayCall {
			return vias[r.ID]
		}
		return []model.RelationID{r.ID}
	}
	for _, route := range routes {
		for _, r := range route {
			for _, id := range backing(r) {
				if !seen[id] {
					seen[id] = true
					ids = append(ids, id)
				}
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
			if r.Kind == model.RelMayCall {
				p.Possible = true
			}
			for _, id := range backing(r) {
				for _, ev := range byRelation[id] {
					if len(p.Evidence) < model.MaxRelationsPerPath {
						p.Evidence = append(p.Evidence, ev)
					}
				}
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
	// scanDone and hopPos extend that position past the scan: the batch's
	// stored lists are all read, and where its name-only steps are (nameOnly).
	scanDone bool
	hopPos   hopPos
	// possible is the search that also takes name-only calls, run when the
	// precise search proved no route exists (startPossible).
	possible bool
	// routing is a search that settled its target and is listing its
	// equal-cost routes: routeAfter is the position after the last route
	// subtree a page finished (routePage), routesListed the routes every page
	// so far listed, and routeCount the count of them all (countRoutes), a
	// lower bound while routeCountAtLeast.
	routing           bool
	routeAfter        []string
	routesListed      int64
	routeCount        int64
	routeCountAtLeast bool
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
// expansion position, the depth-pruned disclosure and the search's phase.
func (w *pathWalk) loadProgress() error {
	var after, hops string
	var pruned, scanDone, possible int
	if _, err := w.sc.row(w.scCtx, `SELECT expand_after, depth_pruned, scan_done, hop_pos, possible
		FROM progress WHERE k = 0`, nil, &after, &pruned, &scanDone, &hops, &possible); err != nil {
		return err
	}
	w.hopPos = hopPos{}
	if hops != "" {
		if err := json.Unmarshal([]byte(hops), &w.hopPos); err != nil {
			return internalErr("path scratch: the name-only position: " + err.Error())
		}
	}
	w.scanDone, w.possible = scanDone != 0, possible != 0
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
	flag := func(b bool) int {
		if b {
			return 1
		}
		return 0
	}
	hops, err := json.Marshal(w.hopPos)
	if err != nil {
		return internalErr("path scratch: the name-only position: " + err.Error())
	}
	return w.sc.exec(w.scCtx, `UPDATE progress SET expand_after = ?, depth_pruned = ?, scan_done = ?,
		hop_pos = ?, possible = ? WHERE k = 0`,
		encodeEdgePos(w.expandAfter), flag(w.depthPruned), flag(w.scanDone), string(hops), flag(w.possible))
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
		w.expandAfter, w.scanDone, w.hopPos = EdgePos{}, false, hopPos{}
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
	if !w.scanDone {
		stopped, err := w.scanBatch(ctx, batch, dists, depths)
		if err != nil || stopped {
			return stopped, err
		}
		// The batch's stored lists are read; the page that resumes it goes
		// straight to its name-only steps.
		w.scanDone = true
		if err := w.saveProgress(); err != nil {
			return false, err
		}
	}
	if !w.possible {
		return false, nil
	}
	return w.nameOnly(ctx, batch, dists, depths)
}

// scanBatch reads the stored lists of one pending batch in ONE resumable scan
// and relaxes every entry it keeps, reporting whether the page ended first.
// Under the descent the scan reads both lists: an outgoing entry of a
// requested kind relaxes from the node's own state, an incoming `overrides`
// entry from its dispatch state, and an entry neither state in the batch
// owns is skipped uncharged. A long overrides list is therefore split across
// pages by the same scan position as any other list.
func (w *pathWalk) scanBatch(ctx context.Context, batch []model.NodeID,
	dists map[model.NodeID]int64, depths map[model.NodeID]int) (bool, error) {
	refs, plain, dispatch, err := w.resolveBatch(ctx, batch)
	if err != nil {
		return false, err
	}
	if len(refs) == 0 {
		return false, nil
	}
	direction, kinds := w.direction, w.kindCodes
	if w.descent.active {
		direction = model.DirectionBoth
		if !slices.Contains(kinds, w.descent.overrides) {
			kinds = append(slices.Clone(kinds), w.descent.overrides)
		}
	}
	owner := func(e Edge) (model.NodeID, bool) {
		if !w.descent.active {
			n, ok := plain[e.Owner]
			return n, ok
		}
		if e.Outgoing {
			if !slices.Contains(w.kindCodes, e.Kind) {
				return "", false
			}
			n, ok := plain[e.Owner]
			return n, ok
		}
		if e.Kind != w.descent.overrides {
			return "", false
		}
		n, ok := dispatch[e.Owner]
		return n, ok
	}
	pend := make([]pathEntry, 0, adjacencyBatch)
	stopped := false
	var ferr error
	flush := func() error {
		if len(pend) == 0 {
			return nil
		}
		if err := w.relax(ctx, pend, dists, depths); err != nil {
			return err
		}
		pend = pend[:0]
		return nil
	}
	pos, err := w.reader.Neighbours(ctx, refs, direction, kinds, w.expandAfter,
		func(e Edge) error {
			from, ok := owner(e)
			if !ok {
				return nil
			}
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
			pend = append(pend, pathEntry{edge: e, from: from})
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

// pathEntry is one kept entry of a batch's scan and the search state it
// relaxes from.
type pathEntry struct {
	edge Edge
	from model.NodeID
}

// resolveBatch maps the batch's states onto the surrogates the packed
// adjacency is keyed by, ASCENDING and duplicate-free as the port requires,
// and returns the reverse maps the scan reads owners through: a node's own
// state, and its dispatch state when the batch holds that.
//
// A node the pinned generation does not publish resolves to zero and is
// dropped: it has no packed list, and the search has nothing to expand for it.
func (w *pathWalk) resolveBatch(ctx context.Context, batch []model.NodeID) ([]NodeRef,
	map[NodeRef]model.NodeID, map[NodeRef]model.NodeID, error) {
	nodes := make([]model.NodeID, len(batch))
	for i, n := range batch {
		nodes[i], _ = pathState(n)
	}
	resolved, err := w.reader.Resolve(ctx, nodes)
	if err != nil {
		return nil, nil, nil, err
	}
	plain := map[NodeRef]model.NodeID{}
	dispatch := map[NodeRef]model.NodeID{}
	refs := make([]NodeRef, 0, len(resolved))
	for i, ref := range resolved {
		if ref == 0 {
			continue
		}
		_, p := plain[ref]
		_, d := dispatch[ref]
		if !p && !d {
			refs = append(refs, ref)
		}
		if _, isDispatch := pathState(batch[i]); isDispatch {
			dispatch[ref] = batch[i]
		} else {
			plain[ref] = batch[i]
		}
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i] < refs[j] })
	return refs, plain, dispatch, nil
}

// relax turns one batch of packed entries into bucket rows. The canonical ids
// the scratch is keyed by are resolved for the WHOLE batch in two batched
// primary-key reads before any row is inserted, never one read per entry, and
// the settle order therefore still ties on the content-derived canonical node
// id rather than on the rebuild-local surrogate the scan carries.
//
// Which state the search came from is the entry's owner state: the state of
// the node this batch settled that kept the entry, whichever direction its
// list was read in. It is exact where guessing from the two endpoints is not:
// a self-loop, and an edge whose endpoints are both settled in this batch,
// relax from the state whose expansion actually delivered them.
func (w *pathWalk) relax(ctx context.Context, pend []pathEntry,
	dists map[model.NodeID]int64, depths map[model.NodeID]int) error {
	rels := make([]RelRef, len(pend))
	tos := make([]NodeRef, len(pend))
	edges := make([]Edge, len(pend))
	for i, p := range pend {
		rels[i], tos[i], edges[i] = p.edge.Rel, p.edge.Neighbour, p.edge
	}
	relIDs, err := w.reader.RelationIDs(ctx, rels)
	if err != nil {
		return err
	}
	toIDs, err := w.reader.NodeIDs(ctx, tos)
	if err != nil {
		return err
	}
	virtual, err := w.virtualCalls(ctx, edges, relIDs)
	if err != nil {
		return err
	}
	for i, e := range edges {
		from := pend[i].from
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

// virtualCalls is which of a relaxation batch's outgoing `calls` relations,
// named by relIDs in the same order, have a virtual occurrence, read in
// batches; nothing when the search takes no downward step.
func (w *pathWalk) virtualCalls(ctx context.Context, edges []Edge, relIDs []model.RelationID) (map[model.RelationID]bool, error) {
	out := map[model.RelationID]bool{}
	dr, _ := w.reader.(DispatchReader)
	if !w.descent.active || dr == nil {
		return out, nil
	}
	calls, ok := w.reader.Kinds().Code(model.RelCalls)
	if !ok {
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

// nameOnly relaxes the batch's `may_call` steps once its stored lists are
// read: from a node holding an open call site to each candidate of it, and,
// against the edges for an incoming search, from a candidate to each caller
// of a site naming it; both for both. Each step costs the dearest relation
// cost, is charged to the edge budget as one entry per call site relation
// that makes it, and records that relation (`hopvia`) for the answer.
//
// Every list the step reads is streamed, never held: a node's calls and a
// site's callers through resumable adjacency scans, a name's declarations a
// page at a time by node id. hopPos is the position inside them -- the node,
// the site, the declaration last finished and the scan position -- so a page
// the edge budget stops resumes at the step it did not charge, and a batch
// holds one declaration page and one scan entry whatever a name's count. A
// dispatch state takes no step: its method's steps are its own state's.
func (w *pathWalk) nameOnly(ctx context.Context, batch []model.NodeID,
	dists map[model.NodeID]int64, depths map[model.NodeID]int) (bool, error) {
	dr := declarationReader(w.reader)
	calls, ok := w.reader.Kinds().Code(model.RelCalls)
	if dr == nil || !ok {
		return false, nil
	}
	var nodes []model.NodeID
	for _, n := range batch {
		if _, isDispatch := pathState(n); !isDispatch {
			nodes = append(nodes, n)
		}
	}
	nodes = sortedNodes(nodes)
	s := &hopStep{w: w, dr: dr, calls: calls, repo: w.reader.Binding().RepositoryID, dists: dists, depths: depths}
	for phase := w.hopPos.Phase; phase <= hopBackward; phase++ {
		if (phase == hopForward && w.direction == model.DirectionIncoming) ||
			(phase == hopBackward && w.direction == model.DirectionOutgoing) {
			continue
		}
		for _, n := range nodes {
			resuming := w.hopPos.Node != "" && w.hopPos.Phase == phase
			if resuming && n < w.hopPos.Node {
				continue
			}
			if !resuming || n != w.hopPos.Node {
				w.hopPos = hopPos{Phase: phase, Node: n}
			}
			var stopped bool
			var err error
			if phase == hopForward {
				stopped, err = s.forward(ctx, n)
			} else {
				stopped, err = s.backward(ctx, n)
			}
			if err != nil {
				return false, err
			}
			if stopped {
				return true, w.saveProgress()
			}
		}
		w.hopPos = hopPos{Phase: phase + 1}
	}
	return false, w.saveProgress()
}

// The two passes of a batch's name-only steps, in order.
const (
	hopForward  = 0
	hopBackward = 1
)

// hopPos is the resume position inside a batch's name-only steps: the pass
// and the node it is at, the site whose callers are being read (backward),
// the last declaration finished in the name's list, and the position in the
// adjacency scan being read. The zero value is the start.
type hopPos struct {
	Phase int          `json:"p"`
	Node  model.NodeID `json:"n,omitempty"`
	Site  model.NodeID `json:"s,omitempty"`
	After model.NodeID `json:"a,omitempty"`
	Edge  string       `json:"e,omitempty"`
}

// hopStep is one batch's name-only steps.
type hopStep struct {
	w      *pathWalk
	dr     DeclarationReader
	calls  KindCode
	repo   model.RepositoryID
	dists  map[model.NodeID]int64
	depths map[model.NodeID]int
}

// take relaxes one step, or reports that the page's edge budget is spent.
func (s *hopStep) take(at, next, caller, callee model.NodeID, via model.RelationID) (bool, error) {
	w := s.w
	if w.maxEdges.Exceeded(w.pageEdges + 1) {
		w.edgesHit = true
		return false, nil
	}
	w.spent++
	w.pageEdges++
	hop := model.NewRelationID(s.repo, caller, model.RelMayCall, callee)
	if err := w.sc.exec(w.scCtx,
		`INSERT OR IGNORE INTO bucket(cost, node, rel, frm, kind, depth) VALUES(?, ?, ?, ?, ?, ?)`,
		s.dists[at]+Cost(model.RelMayCall), string(next), string(hop), string(at), string(model.RelMayCall),
		s.depths[at]+1); err != nil {
		return false, err
	}
	return true, w.sc.exec(w.scCtx, `INSERT OR IGNORE INTO hopvia(rel, via) VALUES(?, ?)`, string(hop), string(via))
}

// one resolves a single scan entry's relation and other endpoint.
func (s *hopStep) one(ctx context.Context, e Edge) (model.RelationID, model.NodeID, error) {
	rels, err := s.w.reader.RelationIDs(ctx, []RelRef{e.Rel})
	if err != nil {
		return "", "", err
	}
	others, err := s.w.reader.NodeIDs(ctx, []NodeRef{e.Neighbour})
	if err != nil {
		return "", "", err
	}
	return rels[0], others[0], nil
}

// scan streams one node's list of `calls` entries in direction from the
// saved position, stopping where visit asks to.
func (s *hopStep) scan(ctx context.Context, n model.NodeID, direction model.Direction,
	visit func(e Edge, first bool) (bool, error)) (bool, error) {
	refs, err := s.w.reader.Resolve(ctx, []model.NodeID{n})
	if err != nil || len(refs) == 0 || refs[0] == 0 {
		return false, err
	}
	from, err := decodeEdgePos(s.w.hopPos.Edge)
	if err != nil {
		return false, err
	}
	first, stopped := true, false
	var verr error
	pos, err := s.w.reader.Neighbours(ctx, refs, direction, []KindCode{s.calls}, from, func(e Edge) error {
		ok, err := visit(e, first)
		first = false
		if err != nil {
			verr = err
			return ErrStopScan
		}
		if !ok {
			stopped = true
			return ErrStopScan
		}
		return nil
	})
	if verr != nil {
		return false, verr
	}
	if err != nil {
		return false, err
	}
	if stopped {
		s.w.hopPos.Edge = encodeEdgePos(pos)
	}
	return stopped, nil
}

// forward takes the steps from node c: for each of its calls into an open
// site, to each declaration that fits the site, a page of the name at a time.
func (s *hopStep) forward(ctx context.Context, c model.NodeID) (bool, error) {
	return s.scan(ctx, c, model.DirectionOutgoing, func(e Edge, first bool) (bool, error) {
		via, target, err := s.one(ctx, e)
		if err != nil || via == "" || target == "" {
			return err == nil, err
		}
		decls, err := s.dr.Declared(ctx, []model.NodeID{target})
		if err != nil || len(decls) == 0 {
			return err == nil, err
		}
		site, ok := openCallSite(decls[0])
		if !ok || site.Reason == model.ReasonComputed || site.Name == "" {
			return true, nil
		}
		after := model.NodeID("")
		if first {
			after = s.w.hopPos.After
		}
		for {
			page, next, err := s.dr.Declarations(ctx, site.Name, shapeKinds(site.Shape), after, model.MaxPageItems)
			if err != nil {
				return false, err
			}
			for _, d := range page {
				if site.fits(d.Node) {
					ok, err := s.take(c, d.Node.ID, c, d.Node.ID, via)
					if err != nil || !ok {
						s.w.hopPos.After = after
						return false, err
					}
				}
				after = d.Node.ID
			}
			if next == "" {
				break
			}
			after = next
		}
		s.w.hopPos.After = ""
		return true, nil
	})
}

// backward takes the steps into node k: for each open site whose candidates
// include it, a page of k's name at a time, to each caller of the site.
func (s *hopStep) backward(ctx context.Context, k model.NodeID) (bool, error) {
	decls, err := s.dr.Declared(ctx, []model.NodeID{k})
	if err != nil || len(decls) == 0 {
		return false, err
	}
	n := decls[0].Node
	if !isDeclared(n) || n.Name == "" {
		return false, nil
	}
	after := s.w.hopPos.After
	for {
		page, next, err := s.dr.Declarations(ctx, n.Name, nil, after, model.MaxPageItems)
		if err != nil {
			return false, err
		}
		for _, d := range page {
			site, ok := openCallSite(d)
			if ok && site.Reason != model.ReasonComputed && site.fits(n) {
				if s.w.hopPos.Site != d.Node.ID {
					s.w.hopPos.Site, s.w.hopPos.Edge = d.Node.ID, ""
				}
				stopped, err := s.scan(ctx, d.Node.ID, model.DirectionIncoming, func(e Edge, _ bool) (bool, error) {
					via, caller, err := s.one(ctx, e)
					if err != nil || via == "" || caller == "" {
						return err == nil, err
					}
					return s.take(k, caller, caller, k, via)
				})
				if err != nil || stopped {
					s.w.hopPos.After = after
					return stopped, err
				}
				s.w.hopPos.Site, s.w.hopPos.Edge = "", ""
			}
			after = d.Node.ID
		}
		if next == "" {
			return false, nil
		}
		after = next
	}
}

// hopVias is the call site relations behind each `may_call` step on routes.
func (w *pathWalk) hopVias(routes [][]model.Relation) (map[model.RelationID][]model.RelationID, error) {
	out := map[model.RelationID][]model.RelationID{}
	for _, route := range routes {
		for _, r := range route {
			if r.Kind != model.RelMayCall {
				continue
			}
			if _, done := out[r.ID]; done {
				continue
			}
			var vias []model.RelationID
			if err := w.sc.each(w.scCtx, `SELECT via FROM hopvia WHERE rel = ? ORDER BY via`, []any{string(r.ID)},
				func(scan func(...any) error) error {
					var via string
					if err := scan(&via); err != nil {
						return internalErr("path scratch read: " + err.Error())
					}
					vias = append(vias, model.RelationID(via))
					return nil
				}); err != nil {
				return nil, err
			}
			out[r.ID] = vias
		}
	}
	return out, nil
}

// startPossible turns an exhausted precise search into the search that also
// takes name-only calls (ADR-0015 decision 3): the state is emptied and the
// same walk runs again from the start with `may_call` steps admitted. The
// phase is recorded in the state, so a continuation resumes it. The page's
// work budgets and the search's cumulative counters carry on.
func (w *pathWalk) startPossible() error {
	for _, table := range []string{"settled", "parent", "bucket", "reach", "pending", "hopvia"} {
		if err := w.sc.exec(w.scCtx, `DELETE FROM `+table); err != nil {
			return err
		}
	}
	w.possible, w.resumed = true, false
	w.bucketCost, w.lastSettled = 0, ""
	w.expandAfter, w.scanDone, w.hopPos = EdgePos{}, false, hopPos{}
	w.depthPruned, w.targetSettled = false, false
	return w.saveProgress()
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

// routeStep is one edge of the shortest-path graph out of a state, keyed for
// the enumeration by (relation, state): one relation reaches a method and its
// dispatch state both.
type routeStep struct {
	rel  model.Relation
	node model.NodeID
}

func (r routeStep) key() string { return string(r.rel.ID) + "\x00" + string(r.node) }

// routeEdges is the page of edges out of state n that lie on a shortest route
// to the target, in (relation, state) order from key at (inclusive), at most
// limit of them. The page is materialized before the caller recurses: the
// scratch holds ONE connection, and a nested query under an open row set
// would wait on it forever.
func (w *pathWalk) routeEdges(n model.NodeID, at string, limit int) ([]routeStep, error) {
	rel, node, _ := strings.Cut(at, "\x00")
	var out []routeStep
	err := w.sc.each(w.scCtx, `SELECT p.node, p.rel, p.kind FROM parent p
		WHERE p.frm = ? AND (p.rel, p.node) >= (?, ?) AND EXISTS(SELECT 1 FROM reach r WHERE r.node = p.node)
		ORDER BY p.rel, p.node LIMIT ?`, []any{string(n), rel, node, limit},
		func(scan func(...any) error) error {
			var to, id, kind string
			if err := scan(&to, &id, &kind); err != nil {
				return internalErr("path scratch read: " + err.Error())
			}
			out = append(out, routeStep{node: model.NodeID(to), rel: model.Relation{ID: model.RelationID(id),
				From: n, To: model.NodeID(to), Kind: model.RelationKind(kind)}})
			return nil
		})
	return out, err
}

// routeLen is the longest route the enumeration lists: the depth bound and
// the relations one path carries.
func (w *pathWalk) routeLen() int {
	if w.maxDepth.Exceeded(int64(model.MaxRelationsPerPath)) {
		return w.maxDepth.Int()
	}
	return model.MaxRelationsPerPath
}

// routePageResult is one page of the route list.
type routePageResult struct {
	routes [][]model.Relation
	// more is that routes remain after this page; after is the position the
	// next page starts after.
	more  bool
	after []string
	// capped is the enumeration's per-page step budget ending the page.
	capped bool
}

// routePage lists the next page of equal-cost shortest routes out of the
// on-disk shortest-path graph, in the frozen order: the (relation, state)
// sequence compared lexicographically, which a preorder walk visiting each
// state's edges in that order emits. It starts after w.routeAfter: the
// prefix of the last route subtree a page finished, so a subtree is never
// listed twice and a resumed page descends straight to where the last one
// stopped. A page takes at most limit routes and at most a page of nodes, and
// at most pathEnumerationSteps edges of the graph: a dense graph's dead-end
// prefixes (the depth bound) cost steps, and a page that spends them ends
// with a continuation rather than a shorter list.
func (w *pathWalk) routePage(from, to model.NodeID, limit int) (routePageResult, error) {
	var out routePageResult
	after := w.routeAfter
	maxLen := w.routeLen()
	nodes := map[model.NodeID]bool{from: true}
	var cur []routeStep
	done := slices.Clone(after) // the prefix of the last finished subtree
	steps := 0
	stop := false
	take := func() bool {
		if len(out.routes) == limit {
			return false
		}
		route := make([]model.Relation, len(cur))
		added := 0
		for i, st := range cur {
			r := st.rel
			r.From, _ = pathState(r.From)
			r.To, _ = pathState(r.To)
			route[i] = r
			if !nodes[r.To] {
				added++
			}
		}
		if len(out.routes) > 0 && len(nodes)+added > model.MaxPageItems {
			return false
		}
		for _, r := range route {
			nodes[r.To] = true
		}
		out.routes = append(out.routes, route)
		return true
	}
	var walk func(n model.NodeID, constrained bool) error
	walk = func(n model.NodeID, constrained bool) error {
		if n == to {
			if !take() {
				stop, out.more = true, true
			}
			return nil
		}
		if len(cur) >= maxLen {
			return nil
		}
		depth := len(cur)
		at := ""
		if constrained {
			at = after[depth]
		}
		for {
			edges, err := w.routeEdges(n, at, adjacencyBatch)
			if err != nil {
				return err
			}
			for _, st := range edges {
				key := st.key()
				within := constrained && key == after[depth]
				if within && depth == len(after)-1 {
					continue // that subtree was finished by an earlier page
				}
				if steps++; steps > pathEnumerationSteps {
					stop, out.more, out.capped = true, true, true
					return nil
				}
				cur = append(cur, st)
				err := walk(st.node, within)
				cur = cur[:len(cur)-1]
				if err != nil {
					return err
				}
				if stop {
					return nil
				}
				done = append(slices.Clone(keysOf(cur)), key)
			}
			if len(edges) < adjacencyBatch {
				return nil
			}
			at = edges[len(edges)-1].key() + "\x00"
			constrained = false
		}
	}
	if err := walk(from, len(after) > 0); err != nil {
		return routePageResult{}, err
	}
	out.after = done
	return out, nil
}

// keysOf is the enumeration keys of a route prefix.
func keysOf(steps []routeStep) []string {
	out := make([]string, len(steps))
	for i, st := range steps {
		out[i] = st.key()
	}
	return out
}

// countRoutes counts the equal-cost shortest routes the enumeration lists,
// without listing them: dynamic programming over the shortest-path graph in
// settle order, where every parent is strictly cheaper than its child, the
// routes reaching a state at each length being the sum over its parents'
// routes one shorter, up to the longest route listed. The partial counts live
// in the state (pathcount), a page of states at a time. A count past the
// largest integer, or a deadline that lands first, is reported as a lower
// bound (atLeast), which the last page of the list makes exact.
func (w *pathWalk) countRoutes(ctx context.Context, from, to model.NodeID) (int64, bool, error) {
	if err := w.sc.exec(w.scCtx, `DELETE FROM pathcount`); err != nil {
		return 0, false, err
	}
	if err := w.sc.exec(w.scCtx, `INSERT INTO pathcount(node, len, n) VALUES(?, 0, 1)`, string(from)); err != nil {
		return 0, false, err
	}
	maxLen := w.routeLen()
	saturated := false
	var lastDist int64 = -1
	lastNode := ""
	for {
		if stop, err := w.checkDeadline(ctx); stop || err != nil {
			return 0, true, err
		}
		type state struct {
			node string
			dist int64
		}
		var page []state
		if err := w.sc.each(w.scCtx, `SELECT s.node, s.dist FROM settled s JOIN reach r ON r.node = s.node
			WHERE (s.dist, s.node) > (?, ?) AND s.node != ? ORDER BY s.dist, s.node LIMIT ?`,
			[]any{lastDist, lastNode, string(from), adjacencyBatch},
			func(scan func(...any) error) error {
				var st state
				if err := scan(&st.node, &st.dist); err != nil {
					return internalErr("path scratch read: " + err.Error())
				}
				page = append(page, st)
				return nil
			}); err != nil {
			return 0, false, err
		}
		for _, st := range page {
			sums := map[int64]int64{}
			if err := w.sc.each(w.scCtx, `SELECT c.len + 1, c.n FROM parent p JOIN pathcount c ON c.node = p.frm
				WHERE p.node = ? AND c.len + 1 <= ?`, []any{st.node, maxLen},
				func(scan func(...any) error) error {
					var l, n int64
					if err := scan(&l, &n); err != nil {
						return internalErr("path scratch read: " + err.Error())
					}
					if sums[l] > math.MaxInt64-n {
						sums[l], saturated = math.MaxInt64, true
					} else {
						sums[l] += n
					}
					return nil
				}); err != nil {
				return 0, false, err
			}
			for l, n := range sums {
				if err := w.sc.exec(w.scCtx, `INSERT INTO pathcount(node, len, n) VALUES(?, ?, ?)`, st.node, l, n); err != nil {
					return 0, false, err
				}
			}
		}
		if len(page) < adjacencyBatch {
			break
		}
		lastDist, lastNode = page[len(page)-1].dist, page[len(page)-1].node
	}
	var total int64
	if err := w.sc.each(w.scCtx, `SELECT n FROM pathcount WHERE node = ?`, []any{string(to)},
		func(scan func(...any) error) error {
			var n int64
			if err := scan(&n); err != nil {
				return internalErr("path scratch read: " + err.Error())
			}
			if total > math.MaxInt64-n {
				total, saturated = math.MaxInt64, true
			} else {
				total += n
			}
			return nil
		}); err != nil {
		return 0, false, err
	}
	return total, saturated, nil
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
// goes on as deep as the overrides do. A node's own state relaxes only its
// outgoing entries of the requested kinds (scanBatch). It is inactive for
// every other search.
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
