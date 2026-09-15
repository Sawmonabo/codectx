package graph

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"

	"github.com/Sawmonabo/codectx/internal/config"
	"github.com/Sawmonabo/codectx/internal/model"
)

// Truncation reasons. Each names the exact bound that stopped the walk, so a
// caller can tell "you asked for fewer items" from "this workspace is bigger
// than your budget" from "the facts are not built yet". A truncated answer
// always carries one of these; an answer that silently stopped is a defect.
const (
	reasonEdgeBudget    = "edge budget exhausted"
	reasonVisitedBudget = "visited node budget exhausted"
	reasonPageFull      = "page item limit reached"
	reasonDependence    = "dependence units are still building"
	reasonFrontierBytes = "frontier memory budget exhausted"
	// reasonDepth is the depth bound. Before this it was the ONE stop that
	// reported nothing at all: the loop simply fell out with a live frontier
	// and Truncated=false, so a depth-limited answer read as a complete one.
	reasonDepth = "graph depth budget exhausted"
)

// edgeRowOverheadBytes is the fixed per-row cost of holding one edge of a
// frontier level in memory: the edgeRow struct, its model.Relation, the string
// headers inside both, and the map entry that records the relation as
// collected. It is a deliberately conservative ESTIMATE rather than a
// measurement -- Limits.FrontierBytes is a ceiling on memory, and an estimate
// that reads low would let the ceiling be crossed before the walk noticed.
const edgeRowOverheadBytes = 256

// edgeRowBytes estimates what one edgeRow costs, adding the identifiers it
// actually carries to the fixed overhead.
func edgeRowBytes(r edgeRow) int64 {
	return edgeRowOverheadBytes + int64(len(r.owner.Node)+len(r.owner.Via)+
		len(r.rel.ID)+len(r.rel.From)+len(r.rel.To)+len(r.rel.Kind)+len(r.neighbor))
}

// expand is the ONE batched BFS. visit is called once per admitted edge in the
// frozen (depth asc, NodeID asc) order and may return errStopExpansion to end
// the walk, which expand reports as a clean finish: the visitor stopped
// deliberately and owns whatever truncation flag it set.
//
// expand does one Adjacency.Edges round trip per frontier batch per keyset
// page, never one per node: a hub with 40 out-edges costs one call, not 40.
//
// It enforces only the bounds it is given -- MaxDepth, the deadline and
// cancellation. MaxVisited and MaxEdges are deliberately absent from
// expandOptions: budget carries the CUMULATIVE, cursor-carried counts already
// spent, and the caller resolves the caps from Limits and the request, so the
// visitor is the one place that compares the two. That keeps a resumed page
// from spending a fresh budget.
func expand(ctx context.Context, a Adjacency, seeds []model.NodeID, o expandOptions,
	visit func(frontierState, model.Relation) error) (walkState, error) {
	if a == nil {
		return walkState{}, (&model.Error{Code: model.CodeInternal,
			Message: "graph expansion requires an adjacency reader"}).WithDetail("operation", "expand")
	}
	if o.Budget == nil {
		return walkState{}, (&model.Error{Code: model.CodeInternal,
			Message: "graph expansion requires a budget"}).WithDetail("operation", "expand")
	}
	if visit == nil {
		return walkState{}, (&model.Error{Code: model.CodeInternal,
			Message: "graph expansion requires a visitor"}).WithDetail("operation", "expand")
	}
	batch := o.BatchSize
	if batch <= 0 {
		batch = adjacencyBatch
	}

	admittedNode := make(map[model.NodeID]bool, len(seeds))
	// A relation is admitted at most once for the whole walk: a cycle, an
	// overlapping batch or a DirectionBoth edge whose two endpoints are both on
	// the frontier must not be counted or emitted twice.
	admittedRel := map[model.RelationID]bool{}
	var (
		frontier []frontierState
		// carry is the NEXT level as the page that issued the cursor had
		// already built it, held aside until the re-read level completes.
		carry     []frontierState
		depth     int
		skipOwner model.NodeID
		skipKey   model.RelationID
	)
	if o.Resume == nil {
		// Seeds enter at depth 0 in request order, de-duplicated, then sorted by
		// NodeID: the frozen (depth asc, NodeID asc) order starts here.
		for _, s := range seeds {
			if s == "" || admittedNode[s] {
				continue
			}
			admittedNode[s] = true
			frontier = append(frontier, frontierState{Node: s})
		}
		o.Budget.visited += int64(len(frontier))
		o.Budget.pageVisited += int64(len(frontier))
	} else {
		// A resume never re-enters the seeds: they are already in the visited
		// set the issuing page spooled, and re-admitting them would spend the
		// cumulative visited budget a second time for the same nodes.
		for n := range o.Resume.Visited {
			admittedNode[n] = true
		}
		depth = o.Resume.Cursor.Depth
		skipOwner, skipKey = o.Resume.Cursor.LastOwner, o.Resume.Cursor.LastKey
		for _, fs := range o.Resume.Frontier {
			if fs.Depth == depth {
				frontier = append(frontier, fs)
			} else {
				carry = append(carry, fs)
			}
			// Seeding admittedRel with the relation each frontier node was
			// DISCOVERED by is what keeps a DirectionBoth walk from emitting one
			// edge on two pages. Re-reading a frontier node returns the edge that
			// reached it, which the issuing page already admitted; every node
			// this page expands is either in this spooled frontier (Via-seeded
			// here) or was discovered by this page itself (covered by the
			// in-page admittedRel below).
			if fs.Via != "" {
				admittedRel[fs.Via] = true
			}
		}
		sort.Slice(carry, func(i, j int) bool { return carry[i].Node < carry[j].Node })
	}
	sort.Slice(frontier, func(i, j int) bool { return frontier[i].Node < frontier[j].Node })

	// Expanding level `depth` produces nodes at depth+1, so the bound is
	// crossed when depth+1 would exceed it. config.Limit.Exceeded is the whole
	// test: an unlimited bound is never exceeded, so the walk is bounded by the
	// graph, by the page budgets below and by the deadline instead.
	for ; len(frontier) > 0 && !o.MaxDepth.Exceeded(int64(depth+1)); depth++ {
		if err := checkWalk(ctx, o.Budget); err != nil {
			return walkState{}, err
		}
		level := make(map[model.NodeID]frontierState, len(frontier))
		nodes := make([]model.NodeID, 0, len(frontier))
		for _, st := range frontier {
			level[st.Node] = st
			nodes = append(nodes, st.Node)
		}

		rows, err := levelEdges(ctx, a, nodes, level, o, batch, admittedRel, skipOwner, skipKey)
		if err != nil {
			return walkState{}, err
		}
		// Only the level a cursor stopped inside is skipped; every level after
		// it is read whole.
		skipOwner, skipKey = "", ""
		// A level that spent the frontier budget is still emitted -- the edges
		// it did read are facts -- but the walk stops after it rather than
		// expanding a level it knows is incomplete.

		next := carry
		carry = nil
		for _, row := range rows {
			if err := visit(row.owner, row.rel); err != nil {
				if errors.Is(err, errStopExpansion) {
					// A deliberate mid-level stop. The continuation re-reads
					// THIS level from the row just admitted and keeps the next
					// level as far as it was built, so no edge is read twice and
					// none is skipped.
					stopped := make([]frontierState, 0, len(frontier)+len(next))
					stopped = append(stopped, frontier...)
					stopped = append(stopped, next...)
					return walkState{Depth: depth, Frontier: stopped, Admitted: admittedNode}, nil
				}
				return walkState{}, err
			}
			admittedRel[row.rel.ID] = true
			o.Budget.edges++
			o.Budget.pageEdges++
			if admittedNode[row.neighbor] {
				continue
			}
			admittedNode[row.neighbor] = true
			o.Budget.visited++
			o.Budget.pageVisited++
			next = append(next, frontierState{
				Depth: depth + 1,
				Cost:  row.owner.Cost + Cost(row.rel.Kind),
				Node:  row.neighbor,
				Via:   row.rel.ID,
			})
		}
		if o.Budget.frontierHit {
			// The frontier byte ceiling SPILLS rather than stopping: the level
			// as far as it was read, plus the next level as far as it was
			// built, become the continuation the caller spools. The resumed
			// page re-reads this level from (lastOwner, lastKey), so no edge is
			// read twice and none is skipped -- exactly the deliberate
			// mid-level stop above, reached by a different trigger.
			stopped := make([]frontierState, 0, len(frontier)+len(next))
			stopped = append(stopped, frontier...)
			stopped = append(stopped, next...)
			return walkState{Depth: depth, Frontier: stopped, Admitted: admittedNode}, nil
		}
		sort.Slice(next, func(i, j int) bool { return next[i].Node < next[j].Node })
		frontier = next
	}
	// A walk that falls out of the loop with an EMPTY frontier ran to
	// completion. One that still holds a frontier ran out of depth: those nodes
	// are admitted but their edges were never read, which is a truncation the
	// caller must be told about. Reporting it is the whole of row 14 -- see
	// DepthLimited for why no continuation is minted for it.
	if len(frontier) > 0 {
		return walkState{Depth: depth, Frontier: frontier, Admitted: admittedNode, DepthLimited: true}, nil
	}
	return walkState{Depth: depth, Admitted: admittedNode}, nil
}

// walkState is where a walk stopped. A page that stopped on its item limit
// turns it into the continuation the next page resumes from; a walk that ran to
// completion leaves Frontier empty and mints nothing.
type walkState struct {
	// Depth is the level that was being expanded when the walk stopped.
	Depth int
	// Frontier holds that level together with the next level as far as it was
	// built. Each record carries its own depth, so a resumed walk measures
	// MaxDepth from the original seeds rather than from its own frontier.
	Frontier []frontierState
	// Admitted is every node the walk has admitted, cumulative across pages.
	Admitted map[model.NodeID]bool
	// DepthLimited records that the walk stopped because the user-set depth
	// bound was reached, with those nodes' edges still unread.
	//
	// A depth stop is REPORTED but not resumable, and it is the only stop of
	// which that is true. Every other bound here is a per-page work budget, so
	// the next page makes progress; the depth bound is a property of the walk
	// and is part of the query hash a cursor is bound to, so a continuation
	// minted for it would resume a walk that is already past the bound, stop at
	// once and mint another -- a cursor chain that never terminates and never
	// returns a row. The honest answer is Truncated + reasonDepth, and the
	// caller's remedy is to raise max_depth.
	DepthLimited bool
}

// edgeRow is one edge of a level attributed to the frontier node it left from,
// so visit receives that node's own state rather than a reconstructed one.
type edgeRow struct {
	owner    frontierState
	rel      model.Relation
	neighbor model.NodeID
}

// levelEdges reads every edge leaving one frontier level and returns them in
// the frozen (NodeID asc, RelationID asc) order. Nodes are sent in batches of
// at most batch ids, and each batch is keyset-paged by relation id, so the
// number of round trips grows with the frontier divided by the batch size --
// never with the frontier itself.
// skipOwner/skipKey, when set, are a resumed page's keyset position: every row
// at or before (skipOwner, skipKey) in the frozen emission order belongs to an
// earlier page and is dropped before it costs a frontier byte.
func levelEdges(ctx context.Context, a Adjacency, nodes []model.NodeID, level map[model.NodeID]frontierState,
	o expandOptions, batch int, admittedRel map[model.RelationID]bool,
	skipOwner model.NodeID, skipKey model.RelationID) ([]edgeRow, error) {
	var (
		rows  []edgeRow
		spent int64
	)
	collected := map[model.RelationID]bool{}
chunks:
	for start := 0; start < len(nodes); start += batch {
		end := start + batch
		if end > len(nodes) {
			end = len(nodes)
		}
		chunk := nodes[start:end]
		after := model.RelationID("")
		for {
			if err := checkWalk(ctx, o.Budget); err != nil {
				return nil, err
			}
			page, err := a.Edges(ctx, chunk, o.Direction, o.Kinds, after, batch)
			if err != nil {
				return nil, err
			}
			for _, rel := range page {
				if admittedRel[rel.ID] || collected[rel.ID] {
					continue
				}
				owner, neighbor, ok := attribute(rel, o.Direction, level)
				if !ok {
					// The reader returned an edge touching no frontier node.
					// Dropping it is the only honest answer -- attributing it
					// to an arbitrary node would invent a path.
					continue
				}
				row := edgeRow{owner: owner, rel: rel, neighbor: neighbor}
				if skipKey != "" && (row.owner.Node < skipOwner ||
					(row.owner.Node == skipOwner && row.rel.ID <= skipKey)) {
					continue
				}
				if spent > 0 && o.FrontierBytes > 0 && spent+edgeRowBytes(row) > o.FrontierBytes {
					// The level does not fit in the configured frontier budget:
					// spill what was read and let the continuation carry on,
					// rather than accumulating an unbounded hub in memory under
					// a bound the configuration says exists.
					//
					// `spent > 0` is what makes that terminate. A budget smaller
					// than ONE edge row would otherwise trip here before any row
					// was admitted, and since the resumed page re-reads the level
					// from the same keyset position it would trip at the same row
					// again: a cursor chain that returns no edge and never ends.
					// Every level-read therefore admits at least one edge --
					// exceeding the byte bound by at most one row -- which is the
					// same trade every other per-page budget here makes.
					o.Budget.frontierHit = true
					break chunks
				}
				spent += edgeRowBytes(row)
				collected[rel.ID] = true
				rows = append(rows, row)
			}
			if len(page) == 0 {
				break
			}
			// A short page is NOT exhaustion: Adjacency.Edges promises "at
			// most limit rows", and the shipped reader clamps any limit above
			// model.MaxPageItems down to it, so a full frontier chunk returns
			// fewer rows than asked for on every page. Only an EMPTY page ends
			// the keyset walk; the advance below is what makes the next one
			// reachable.
			after = page[len(page)-1].ID
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].owner.Node != rows[j].owner.Node {
			return rows[i].owner.Node < rows[j].owner.Node
		}
		return rows[i].rel.ID < rows[j].rel.ID
	})
	return rows, nil
}

// attribute decides which end of an edge is the frontier node that reached it.
// Under DirectionBoth an edge may have both ends on the frontier; the lower
// NodeID owns it, so the emission order is a function of the facts alone.
func attribute(rel model.Relation, dir model.Direction,
	level map[model.NodeID]frontierState) (frontierState, model.NodeID, bool) {
	from, fromOK := level[rel.From]
	to, toOK := level[rel.To]
	switch dir {
	case model.DirectionOutgoing:
		return from, rel.To, fromOK
	case model.DirectionIncoming:
		return to, rel.From, toOK
	}
	switch {
	case fromOK && toOK:
		if rel.From <= rel.To {
			return from, rel.To, true
		}
		return to, rel.From, true
	case fromOK:
		return from, rel.To, true
	case toOK:
		return to, rel.From, true
	}
	return frontierState{}, "", false
}

// checkWalk fails the walk on the two conditions that are not budgets of
// admitted work: the caller went away, and the wall-clock query deadline
// passed. They are distinct and each has exactly one source -- cancellation
// comes from ctx, the timeout from budget.deadline, which the caller sets from
// the engine clock -- so neither check duplicates the other.
func checkWalk(ctx context.Context, b *budget) error {
	if err := ctx.Err(); err != nil {
		if errors.Is(err, context.Canceled) {
			return &model.Error{Code: model.CodeCanceled, Message: "graph traversal was canceled"}
		}
		return &model.Error{Code: model.CodeQueryDeadline, Message: "graph traversal exceeded its deadline"}
	}
	if !b.deadline.IsZero() && !b.clock().Before(b.deadline) {
		return &model.Error{Code: model.CodeQueryDeadline, Message: "graph traversal exceeded its deadline"}
	}
	return nil
}

// Neighbors expands the request's seeds in the request's own direction over its
// relation allowlist, reporting the direction it walked and the visited and
// edge counts it spent.
func (e *Engine) Neighbors(ctx context.Context, req model.GraphRequest) (model.GraphResult, error) {
	return e.traverse(ctx, req, neighborsEndpoint, req.Direction, req.Relations)
}

// neighborsEndpoint binds a continuation to the operation that issued it, the
// way referenceEndpoint does: a cursor minted by a traversal means nothing to
// another endpoint even at the same generation and seeds, and resumeTraversal
// rejects it.
const neighborsEndpoint = "graph.neighbors"

// continuationUnavailable rejects a request carrying a traversal cursor.
// Silently ignoring a cursor would restart the walk from the seeds while the
// caller believed it was resuming, which would double-spend the cumulative
// budget the cursor exists to carry, so a typed refusal is the honest answer.
//
// Every other operation DOES page: Neighbors mints and resumes
// a traversalCursor from a keyset position, and Impact spills its ranked
// tail into a spool and replays it. PackageDependencies is the one that remains:
// it aggregates a whole walk into pairs, so it has neither a (owner, relation)
// stop to resume from nor a ranked list to cut. Its CLI command declares no
// --cursor flag; this refusal covers the API path, where a caller can still set
// Page.Cursor.
func continuationUnavailable(cursor string) error {
	if cursor == "" {
		return nil
	}
	return (&model.Error{Code: model.CodeCursorInvalid,
		Message: "graph traversal continuations are not offered"}).
		WithDetail("reason", "continuation_unavailable")
}

// resolveBound applies the Section 20.1 zero-value convention: zero takes the
// configured default and a positive request value is honoured only as far as
// that default, so a request can tighten a bound but never raise it.
//
// It is the FINITE form, for the one bound that is never unlimited: the page
// item ceiling. Use resolveLimit for every scale bound -- a silent clamp there
// is the class-G defect this wave removes.
func resolveBound(requested, configured int) int {
	if requested <= 0 || requested > configured {
		return configured
	}
	return requested
}

// resolveLimit resolves one unlimited-capable count bound and, when the request
// asked for more than the configuration allows, returns the notice that says so.
// A request can still only tighten a bound -- raising it is an operator
// decision, not a caller's -- but it is never SILENTLY tightened: the answer
// carries "requested N, effective M" so the caller can tell a small answer
// caused by its own request from one caused by the configuration.
//
// config.Limit.Min owns the comparison, with unlimited as the top of the
// lattice, so an unlimited configuration honours any finite request.
func resolveLimit(key string, requested int, configured config.Limit) (config.Limit, string) {
	if requested <= 0 {
		return configured, ""
	}
	effective := config.Limit(requested).Min(configured)
	if effective.Value() == int64(requested) {
		return effective, ""
	}
	return effective, fmt.Sprintf("%s: requested %d, effective %s (the configured bound)",
		key, requested, effective)
}

// resolvePageItems is resolveBound with the same disclosure obligation.
func resolvePageItems(requested, configured int) (int, string) {
	effective := resolveBound(requested, configured)
	if requested <= 0 || effective == requested {
		return effective, ""
	}
	return effective, fmt.Sprintf("page.limit: requested %d, effective %d (the configured bound)",
		requested, effective)
}

// appendNotice collects the non-empty disclosures a walk accumulated.
func appendNotice(into []string, notice string) []string {
	if notice == "" {
		return into
	}
	return append(into, notice)
}

// traverse is the body behind Neighbors: dir and kinds are what the request
// asked to walk, and endpoint is what a continuation minted here is bound to.
func (e *Engine) traverse(ctx context.Context, req model.GraphRequest, endpoint string, dir model.Direction,
	kinds []model.RelationKind) (res model.GraphResult, err error) {
	// One deferred mapping covers every traversal: a bare
	// context failure from the adjacency reader becomes the Section 8 code for
	// the state it is in, and anything already typed is left alone.
	defer func() { err = typedContextError(ctx, err) }()
	if err := req.Validate(); err != nil {
		return model.GraphResult{}, err
	}
	// The deadline wraps the gate as well as the walk, so waiting for a slot
	// past the request deadline is the resource limit the caller must see, and
	// every adjacency round trip below runs under Section 3's per-request
	// deadline rather than only being checked between expansion steps.
	deadline := e.now().Add(e.limits.QueryTimeout)
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	if e.gate != nil {
		if err := e.gate.Acquire(ctx); err != nil {
			return model.GraphResult{}, err
		}
		defer e.gate.Release()
	}
	if len(kinds) == 0 {
		kinds = DefaultRelations()
	}

	var notices []string
	maxDepth, notice := resolveLimit("max_depth", req.MaxDepth, e.limits.Depth())
	notices = appendNotice(notices, notice)
	maxVisited, notice := resolveLimit("max_visited", req.MaxVisited, e.limits.Visited())
	notices = appendNotice(notices, notice)
	maxEdges, notice := resolveLimit("max_edges", req.MaxEdges, e.limits.Edges())
	notices = appendNotice(notices, notice)
	maxItems, notice := resolvePageItems(req.Page.Limit, e.limits.MaxPageItems)
	notices = appendNotice(notices, notice)

	// The capability disclosure happens before the walk: a missing dependence
	// edge must not read as a genuine absence of edges.
	caps, deferred, err := e.completeness(ctx, kinds)
	if err != nil {
		return model.GraphResult{}, err
	}

	// The walk's deadline is the SAME instant the context carries, taken before
	// the gate wait: a deadline recomputed after it would outlive the request's
	// own by however long the wait took.
	b := &budget{deadline: deadline, now: e.now}
	// The query hash binds a continuation to this exact normalized walk, so a
	// cursor presented to a differently filtered or differently bounded query is
	// CTX_CURSOR_INVALID rather than a silently repinned answer.
	queryHash := traversalQueryHash(dir, kinds, req.Start, maxDepth.Int(), maxItems)
	var resume *resumeState
	if req.Page.Cursor != "" {
		resume, err = e.resumeTraversal(ctx, req.Page.Cursor, endpoint, queryHash, deadline)
		if err != nil {
			return model.GraphResult{}, err
		}
		// The resumed budget carries the earlier pages' cumulative spend by
		// ASSIGNMENT, so replaying one cursor twice neither resets nor doubles it.
		b = resume.Budget
	}
	var (
		relations  []model.Relation
		walkReason string
		lastOwner  model.NodeID
		lastKey    model.RelationID
	)
	endpoints := map[model.NodeID]bool{}
	for _, s := range req.Start {
		endpoints[s] = true
	}
	visit := func(owner frontierState, rel model.Relation) error {
		// The visited and edge budgets are compared against THIS page's spend:
		// they are work budgets that end a page, not ceilings that end a walk.
		// Exceeded is strictly greater, so the test is on the spend this row
		// would take the page to.
		switch {
		case maxEdges.Exceeded(b.pageEdges + 1):
			walkReason = reasonEdgeBudget
			return errStopExpansion
		case maxVisited.Exceeded(b.pageVisited + 1):
			walkReason = reasonVisitedBudget
			return errStopExpansion
		case len(relations) >= maxItems:
			walkReason = reasonPageFull
			return errStopExpansion
		}
		relations = append(relations, rel)
		// The keyset position a continuation resumes from: the owner names the
		// frontier node whose chunk the row came from, the relation id the row.
		lastOwner, lastKey = owner.Node, rel.ID
		endpoints[rel.From] = true
		endpoints[rel.To] = true
		return nil
	}
	state, err := expand(ctx, e.adjacency, req.Start, expandOptions{
		Direction:     dir,
		Kinds:         kinds,
		MaxDepth:      maxDepth,
		Budget:        b,
		BatchSize:     adjacencyBatch,
		FrontierBytes: e.limits.FrontierBytes,
		Resume:        resume,
	}, visit)
	if err != nil {
		return model.GraphResult{}, err
	}

	ids := make([]model.NodeID, 0, len(endpoints))
	for id := range endpoints {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	nodes, err := e.adjacency.NodesByID(ctx, ids)
	if err != nil {
		return model.GraphResult{}, err
	}

	reason := walkReason
	// A level cut short by the frontier budget is truncation the caller must
	// see; the visitor's own reason is more specific, so it wins when both hold.
	if reason == "" && b.frontierHit {
		reason = reasonFrontierBytes
	}
	// A walk that ran out of depth with nodes still unexpanded is truncated and
	// says so: before this it fell out of the loop reporting nothing.
	if reason == "" && state.DepthLimited {
		reason = reasonDepth
	}
	if reason == "" && deferred {
		reason = reasonDependence
	}
	// A continuation is offered for EVERY stop that left a frontier standing --
	// a full page, a spent per-page visited or edge budget, a level the frontier
	// byte ceiling spilled. All three are now per-page budgets, so the resumed
	// page makes progress rather than stopping at once.
	//
	// The depth bound is the single exception, and DepthLimited on walkState
	// carries the reason: it is part of the query hash the cursor is bound to,
	// so a continuation minted for it could only resume a walk already past it.
	var nextCursor string
	if len(state.Frontier) > 0 && !state.DepthLimited {
		visited := make([]model.NodeID, 0, len(state.Admitted))
		for id := range state.Admitted {
			visited = append(visited, id)
		}
		sort.Slice(visited, func(i, j int) bool { return visited[i] < visited[j] })
		nextCursor, err = e.nextTraversalCursor(ctx, b, continuation{
			Endpoint:  endpoint,
			QueryHash: queryHash,
			Depth:     state.Depth,
			LastOwner: lastOwner,
			LastKey:   lastKey,
			Frontier:  state.Frontier,
			Visited:   visited,
		})
		if err != nil {
			return model.GraphResult{}, err
		}
	}
	result := model.GraphResult{
		Meta: model.QueryMeta{
			Binding:          e.adjacency.Binding(),
			Completeness:     caps,
			Truncated:        reason != "",
			TruncationReason: reason,
			NextCursor:       nextCursor,
			Notices:          notices,
		},
		Direction: dir,
		Nodes:     nodes,
		Relations: relations,
		// The counts are cumulative spend, not per-page tallies, so a
		// continuation resumes from them rather than from zero.
		VisitedCount: b.visited,
		EdgeCount:    b.edges,
		// MaxDepth echoes the effective depth bound the walk ran under, so a
		// caller can tell a shallow answer caused by a tightened bound from one
		// caused by the graph simply ending.
		MaxDepth: maxDepth.Int(),
	}
	if err := result.Validate(); err != nil {
		return model.GraphResult{}, err
	}
	return result, nil
}

// completeness is the ONE capability derivation every graph answer uses. It
// reads the pinned generation's capability report through the same
// Adjacency.Capabilities port search reads it through (search.go:210), so the
// graph family discloses the same rows `search` and `symbol` do rather than
// reporting no capabilities at all.
//
// The deferred-dependence disclosure is folded INTO those rows, not appended to
// them: a deferred row is one of the generation's own capability rows, enriched
// here with the promotion's queue position. Appending would publish the row
// twice and push a full report past model.MaxCapabilityStates, which the
// answer's own Validate then rejects.
//
// deferred reports whether a request touching a dependence-only relation kind
// found deferred units; that, and never the length of rows, is what makes an
// answer truncated -- every generation carries capability rows, so a
// length test would report every answer as incomplete.
//
// In report mode there is no promoter and the rows are disclosed without
// promoting. A failed promotion never fails the query: the answer is still
// correct, just still incomplete.
func (e *Engine) completeness(ctx context.Context, kinds []model.RelationKind) ([]model.CapabilityState, bool, error) {
	rows, err := e.adjacency.Capabilities(ctx)
	if err != nil {
		return nil, false, err
	}
	wanted := map[model.RelationKind]bool{}
	for _, k := range kinds {
		wanted[k] = true
	}
	touches := false
	for _, k := range DependenceOnly() {
		if wanted[k] {
			touches = true
			break
		}
	}
	if !touches {
		return rows, false, nil
	}
	deferred := false
	for i, c := range rows {
		if c.ProviderID != dependenceProviderID || c.Details["reason"] != reasonUnitsDeferred {
			continue
		}
		deferred = true
		if e.promoter == nil {
			continue
		}
		p, err := e.promoter.Promote(ctx, dependenceProviderID, c.Scope)
		if err != nil {
			continue
		}
		// WithDetail clones the map, truncates the value and refuses to
		// grow past MaxCapabilityDetails, so folding a queue position in
		// can neither mutate the reader's row nor build a row that then
		// fails its own Validate and fails the query it was disclosing.
		row := c.WithDetail("units", strconv.Itoa(p.Units)).
			WithDetail("position", strconv.Itoa(p.Position))
		if p.Estimate > 0 {
			// An unmeasured duration is reported as unmeasured, never invented.
			row = row.WithDetail("estimate_ms", strconv.FormatInt(p.Estimate.Milliseconds(), 10))
		}
		rows[i] = row
	}
	return rows, deferred, nil
}

const (
	// dependenceProviderID is the provider whose deferred units make a
	// dependence-only answer incomplete.
	dependenceProviderID = "dependence"
	// reasonUnitsDeferred is the landed addDeferred spelling; no new capability
	// state or diagnostic code is introduced for pending work.
	reasonUnitsDeferred = "units_deferred"
)
