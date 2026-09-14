package graph

import (
	"context"
	"errors"
	"sort"
	"strconv"

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

	for ; depth < o.MaxDepth && len(frontier) > 0; depth++ {
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
			if admittedNode[row.neighbor] {
				continue
			}
			admittedNode[row.neighbor] = true
			o.Budget.visited++
			next = append(next, frontierState{
				Depth: depth + 1,
				Cost:  row.owner.Cost + Cost(row.rel.Kind),
				Node:  row.neighbor,
				Via:   row.rel.ID,
			})
		}
		if o.Budget.frontierHit {
			return walkState{Admitted: admittedNode}, nil
		}
		sort.Slice(next, func(i, j int) bool { return next[i].Node < next[j].Node })
		frontier = next
	}
	// A walk that falls out of the loop ran to completion: an empty Frontier is
	// what tells the caller there is nothing to continue from.
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
				if o.FrontierBytes > 0 && spent+edgeRowBytes(row) > o.FrontierBytes {
					// The level does not fit in the configured frontier budget.
					// Stopping here and disclosing it is the honest answer; the
					// alternative is accumulating an unbounded hub in memory
					// under a bound the configuration says exists.
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
	return e.traverse(ctx, req, neighborsEndpoint, req.Direction, req.Relations, nil)
}

// Callers expands incoming `calls` edges. It pins both the direction and the
// relation itself; a request that contradicts either is rejected with
// CTX_ARGUMENT_INVALID rather than having the field silently ignored.
func (e *Engine) Callers(ctx context.Context, req model.GraphRequest) (model.GraphResult, error) {
	return e.traverse(ctx, req, callersEndpoint, model.DirectionIncoming, []model.RelationKind{model.RelCalls},
		pinnedCalls(model.DirectionIncoming, "callers"))
}

// Callees expands outgoing `calls` edges, pinning direction and relation the
// same way Callers does.
func (e *Engine) Callees(ctx context.Context, req model.GraphRequest) (model.GraphResult, error) {
	return e.traverse(ctx, req, calleesEndpoint, model.DirectionOutgoing, []model.RelationKind{model.RelCalls},
		pinnedCalls(model.DirectionOutgoing, "callees"))
}

// pinnedCalls builds the request check for an operation whose direction and
// relation are fixed by its name. GraphRequest.Validate already rejects an
// unknown direction, so the only remaining case is a known direction that
// disagrees with the operation. An empty relation list means "the operation's
// own relation"; any other list is a caller asking for something the operation
// cannot answer, and is refused for the same reason the direction is.
func pinnedCalls(dir model.Direction, op string) func(model.GraphRequest) error {
	return func(req model.GraphRequest) error {
		if req.Direction != dir {
			return (&model.Error{Code: model.CodeArgumentInvalid,
				Message: "this operation walks one fixed direction"}).
				WithDetail("operation", op).
				WithDetail("direction", string(dir))
		}
		if len(req.Relations) == 0 {
			return nil
		}
		if len(req.Relations) != 1 || req.Relations[0] != model.RelCalls {
			return (&model.Error{Code: model.CodeArgumentInvalid,
				Message: "this operation walks one fixed relation kind"}).
				WithDetail("operation", op).
				WithDetail("relation", string(model.RelCalls))
		}
		return nil
	}
}

// Endpoint names bind a continuation to the operation that issued it, the way
// referenceEndpoint does: a cursor minted by callees means nothing to callers
// even at the same generation and seeds, and resumeTraversal rejects it.
const (
	neighborsEndpoint = "graph.neighbors"
	callersEndpoint   = "graph.callers"
	calleesEndpoint   = "graph.callees"
)

// continuationUnavailable rejects a request carrying a traversal cursor.
// Silently ignoring a cursor would restart the walk from the seeds while the
// caller believed it was resuming, which would double-spend the cumulative
// budget the cursor exists to carry, so a typed refusal is the honest answer.
//
// The traversal operations above DO page: Neighbors, Callers and Callees mint
// and resume a traversalCursor. This refusal is what remains for the two
// impact-family operations, whose page boundary is not a keyset position at
// all: Impact ranks the whole walk and cuts the ranked list, and
// PackageDependencies aggregates it, so neither has a (owner, relation) stop to
// resume from. Their CLI commands declare no --cursor flag; the refusal covers
// the API path, where a caller can still set Page.Cursor.
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
func resolveBound(requested, configured int) int {
	if requested <= 0 || requested > configured {
		return configured
	}
	return requested
}

// traverse is the shared body of Neighbors, Callers and Callees. dir and kinds
// are what the operation actually walks; pin, when non-nil, is the operation's
// own request check, run after the shared validation so it can trust the
// request's shape.
func (e *Engine) traverse(ctx context.Context, req model.GraphRequest, endpoint string, dir model.Direction,
	kinds []model.RelationKind, pin func(model.GraphRequest) error) (res model.GraphResult, err error) {
	// One deferred mapping covers Neighbors, Callers and Callees: a bare
	// context failure from the adjacency reader becomes the Section 8 code for
	// the state it is in, and anything already typed is left alone.
	defer func() { err = typedContextError(ctx, err) }()
	if err := req.Validate(); err != nil {
		return model.GraphResult{}, err
	}
	if pin != nil {
		if err := pin(req); err != nil {
			return model.GraphResult{}, err
		}
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

	maxDepth := resolveBound(req.MaxDepth, e.limits.MaxDepth)
	maxVisited := int64(resolveBound(req.MaxVisited, e.limits.MaxVisited))
	maxEdges := int64(resolveBound(req.MaxEdges, e.limits.MaxEdges))
	maxItems := resolveBound(req.Page.Limit, e.limits.MaxPageItems)

	// The deferred-dependence disclosure happens before the walk: a missing
	// dependence edge must not read as a genuine absence of edges.
	pending, err := e.pendingDependence(ctx, kinds)
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
	queryHash := traversalQueryHash(dir, kinds, req.Start, maxDepth, maxItems)
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
		switch {
		case b.edges >= maxEdges:
			walkReason = reasonEdgeBudget
			return errStopExpansion
		case b.visited >= maxVisited:
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
	if reason == "" && len(pending) > 0 {
		reason = reasonDependence
	}
	// A continuation is offered for exactly one stop: the page filled up while
	// the walk still had a frontier. Every other truncation reason means the
	// walk cannot usefully go on -- a spent visited or edge budget is cumulative
	// and a resumed page would stop at once, and a level the frontier byte
	// ceiling cut short would be cut short again.
	var nextCursor string
	if reason == reasonPageFull && len(state.Frontier) > 0 {
		visited := make([]model.NodeID, 0, len(state.Admitted))
		for id := range state.Admitted {
			visited = append(visited, id)
		}
		sort.Slice(visited, func(i, j int) bool { return visited[i] < visited[j] })
		nextCursor, err = e.nextTraversalCursor(b, continuation{
			Endpoint:  endpoint,
			QueryHash: queryHash,
			LeaseID:   e.leaseID(),
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
			Completeness:     pending,
			Truncated:        reason != "",
			TruncationReason: reason,
			NextCursor:       nextCursor,
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
		MaxDepth: maxDepth,
	}
	if err := result.Validate(); err != nil {
		return model.GraphResult{}, err
	}
	return result, nil
}

// pendingDependence reports the deferred dependence capability rows a request
// touching a dependence-only relation kind must disclose. It returns the rows
// unchanged in State and DiagnosticCode, folding a promotion's queue position
// into Details when a promoter is available; in report mode there is no
// promoter and the rows are reported without promoting. A failed promotion
// never fails the query -- the answer is still correct, just still incomplete.
func (e *Engine) pendingDependence(ctx context.Context, kinds []model.RelationKind) ([]model.CapabilityState, error) {
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
		return nil, nil
	}
	caps, err := e.adjacency.Capabilities(ctx)
	if err != nil {
		return nil, err
	}
	var rows []model.CapabilityState
	for _, c := range caps {
		if c.ProviderID != dependenceProviderID || c.Details["reason"] != reasonUnitsDeferred {
			continue
		}
		row := c
		if e.promoter != nil {
			if p, err := e.promoter.Promote(ctx, dependenceProviderID, c.Scope); err == nil {
				// WithDetail clones the map, truncates the value and refuses to
				// grow past MaxCapabilityDetails, so folding a queue position in
				// can neither mutate the reader's row nor build a row that then
				// fails its own Validate and fails the query it was disclosing.
				row = row.WithDetail("units", strconv.Itoa(p.Units)).
					WithDetail("position", strconv.Itoa(p.Position))
				if p.Estimate > 0 {
					// An unmeasured duration is reported as unmeasured, never invented.
					row = row.WithDetail("estimate_ms", strconv.FormatInt(p.Estimate.Milliseconds(), 10))
				}
			}
		}
		rows = append(rows, row)
	}
	return rows, nil
}

const (
	// dependenceProviderID is the provider whose deferred units make a
	// dependence-only answer incomplete.
	dependenceProviderID = "dependence"
	// reasonUnitsDeferred is the landed addDeferred spelling; no new capability
	// state or diagnostic code is introduced for pending work.
	reasonUnitsDeferred = "units_deferred"
)
