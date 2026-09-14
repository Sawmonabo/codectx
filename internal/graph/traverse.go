package graph

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"time"

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
)

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
	visit func(frontierState, model.Relation) error) error {
	if a == nil {
		return (&model.Error{Code: model.CodeInternal,
			Message: "graph expansion requires an adjacency reader"}).WithDetail("operation", "expand")
	}
	if o.Budget == nil {
		return (&model.Error{Code: model.CodeInternal,
			Message: "graph expansion requires a budget"}).WithDetail("operation", "expand")
	}
	if visit == nil {
		return (&model.Error{Code: model.CodeInternal,
			Message: "graph expansion requires a visitor"}).WithDetail("operation", "expand")
	}
	batch := o.BatchSize
	if batch <= 0 {
		batch = adjacencyBatch
	}

	// Seeds enter at depth 0 in request order, de-duplicated, then sorted by
	// NodeID: the frozen (depth asc, NodeID asc) order starts here.
	admittedNode := make(map[model.NodeID]bool, len(seeds))
	var frontier []frontierState
	for _, s := range seeds {
		if s == "" || admittedNode[s] {
			continue
		}
		admittedNode[s] = true
		frontier = append(frontier, frontierState{Node: s})
	}
	sort.Slice(frontier, func(i, j int) bool { return frontier[i].Node < frontier[j].Node })
	o.Budget.visited += int64(len(frontier))

	// A relation is admitted at most once for the whole walk: a cycle, an
	// overlapping batch or a DirectionBoth edge whose two endpoints are both on
	// the frontier must not be counted or emitted twice.
	admittedRel := map[model.RelationID]bool{}

	for depth := 0; depth < o.MaxDepth && len(frontier) > 0; depth++ {
		if err := checkWalk(ctx, o.Budget); err != nil {
			return err
		}
		level := make(map[model.NodeID]frontierState, len(frontier))
		nodes := make([]model.NodeID, 0, len(frontier))
		for _, st := range frontier {
			level[st.Node] = st
			nodes = append(nodes, st.Node)
		}

		rows, err := levelEdges(ctx, a, nodes, level, o, batch, admittedRel)
		if err != nil {
			return err
		}

		var next []frontierState
		for _, row := range rows {
			if err := visit(row.owner, row.rel); err != nil {
				if errors.Is(err, errStopExpansion) {
					return nil
				}
				return err
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
		sort.Slice(next, func(i, j int) bool { return next[i].Node < next[j].Node })
		frontier = next
	}
	return nil
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
func levelEdges(ctx context.Context, a Adjacency, nodes []model.NodeID, level map[model.NodeID]frontierState,
	o expandOptions, batch int, admittedRel map[model.RelationID]bool) ([]edgeRow, error) {
	var rows []edgeRow
	collected := map[model.RelationID]bool{}
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
				collected[rel.ID] = true
				rows = append(rows, edgeRow{owner: owner, rel: rel, neighbor: neighbor})
			}
			if len(page) < batch {
				break
			}
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
	if !b.deadline.IsZero() && !time.Now().Before(b.deadline) {
		return &model.Error{Code: model.CodeQueryDeadline, Message: "graph traversal exceeded its deadline"}
	}
	return nil
}

// Neighbors expands the request's seeds in the request's own direction over its
// relation allowlist, reporting the direction it walked and the visited and
// edge counts it spent.
func (e *Engine) Neighbors(ctx context.Context, req model.GraphRequest) (model.GraphResult, error) {
	return e.traverse(ctx, req, req.Direction, req.Relations, nil)
}

// Callers expands incoming `calls` edges. It pins both the direction and the
// relation itself; a request that contradicts either is rejected with
// CTX_ARGUMENT_INVALID rather than having the field silently ignored.
func (e *Engine) Callers(ctx context.Context, req model.GraphRequest) (model.GraphResult, error) {
	return e.traverse(ctx, req, model.DirectionIncoming, []model.RelationKind{model.RelCalls},
		pinnedCalls(model.DirectionIncoming, "callers"))
}

// Callees expands outgoing `calls` edges, pinning direction and relation the
// same way Callers does.
func (e *Engine) Callees(ctx context.Context, req model.GraphRequest) (model.GraphResult, error) {
	return e.traverse(ctx, req, model.DirectionOutgoing, []model.RelationKind{model.RelCalls},
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

// continuationUnavailable rejects a request carrying a traversal cursor.
// Silently ignoring a cursor would restart the walk from the seeds while the
// caller believed it was resuming, which would double-spend the cumulative
// budget the cursor exists to carry, so a typed refusal is the honest answer.
//
// The traversal cursor CODEC exists (cursor.go: traversalCursor,
// resumeTraversal, nextTraversalCursor, and the spool spill it writes), but no
// operation issues or consumes one yet: the walks below expand from their seeds
// in a single page. Offering the continuation is the work of threading
// resumeTraversal into this function and nextTraversalCursor into the result,
// and is deliberately not done here.
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
func (e *Engine) traverse(ctx context.Context, req model.GraphRequest, dir model.Direction,
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
	if err := continuationUnavailable(req.Page.Cursor); err != nil {
		return model.GraphResult{}, err
	}
	// The deadline wraps the gate as well as the walk, so waiting for a slot
	// past the request deadline is the resource limit the caller must see, and
	// every adjacency round trip below runs under Section 3's per-request
	// deadline rather than only being checked between expansion steps.
	ctx, cancel := context.WithDeadline(ctx, e.now().Add(e.limits.QueryTimeout))
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

	b := &budget{deadline: e.now().Add(e.limits.QueryTimeout)}
	var (
		relations  []model.Relation
		walkReason string
	)
	endpoints := map[model.NodeID]bool{}
	for _, s := range req.Start {
		endpoints[s] = true
	}
	visit := func(_ frontierState, rel model.Relation) error {
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
		endpoints[rel.From] = true
		endpoints[rel.To] = true
		return nil
	}
	if err := expand(ctx, e.adjacency, req.Start, expandOptions{
		Direction: dir,
		Kinds:     kinds,
		MaxDepth:  maxDepth,
		Budget:    b,
		BatchSize: adjacencyBatch,
	}, visit); err != nil {
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
	if reason == "" && len(pending) > 0 {
		reason = reasonDependence
	}
	result := model.GraphResult{
		Meta: model.QueryMeta{
			Binding:          e.adjacency.Binding(),
			Completeness:     pending,
			Truncated:        reason != "",
			TruncationReason: reason,
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
