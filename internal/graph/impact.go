package graph

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/Sawmonabo/codectx/internal/model"
)

// Impact answers Section 14.3: which entities may be affected by a change to
// the seeds, each one carrying the direction that made it affected and at least
// one evidence-backed reason. It is a bounded expansion over the impact
// allowlist plus a deterministic integer ranking pass -- there is no float
// anywhere in the score, so the same facts always rank the same way.
//
// Nothing here resolves names, reads config or touches storage: seeds arrive
// already resolved, every bound arrives in Limits, and every fact arrives
// through Adjacency.
func (e *Engine) Impact(ctx context.Context, req model.ImpactRequest) (model.ImpactResult, error) {
	// The request shape -- id format, known kinds, known direction, bounded seed
	// count -- is validated by model.ImpactRequest.Validate at the boundary that
	// built it. The engine re-checks only what it alone owns: it must have
	// something to expand from, and every bound it applies comes from Limits.
	if len(req.Start) == 0 {
		return model.ImpactResult{}, &model.Error{Code: model.CodeArgumentInvalid,
			Message: "impact requires at least one start node"}
	}
	// Continuations are not offered for impact in this wave: a cursor this
	// endpoint never issued is rejected rather than silently ignored, which
	// would answer page 1 while the caller believes it asked for page 2.
	if req.Page.Cursor != "" {
		return model.ImpactResult{}, (&model.Error{Code: model.CodeCursorInvalid,
			Message: "impact does not offer continuations"}).WithDetail("endpoint", "impact")
	}
	ctx, done, err := beginImpactQuery(ctx, e)
	if err != nil {
		return model.ImpactResult{}, err
	}
	defer done()

	kinds := impactAllowlist(req.Relations)
	meta := model.QueryMeta{Binding: e.adjacency.Binding()}
	pending, err := e.impactPendingRows(ctx, kinds)
	if err != nil {
		return model.ImpactResult{}, err
	}
	if len(pending) > 0 {
		// Copied unchanged in State/DiagnosticCode: a deferred dependence build
		// makes this answer non-exhaustive, and claiming otherwise would present
		// missing facts as an absence of impact.
		meta.Completeness = pending
		meta.Truncated = true
		meta.TruncationReason = pendingTruncationReason
	}

	b := e.impactBudget(ctx, req.MaxVisited, req.MaxEdges)
	acc := newImpactAccumulator(req.Start)
	walkErr := expand(ctx, e.adjacency, acc.Seeds(), expandOptions{
		Direction: req.Direction,
		Kinds:     kinds,
		MaxDepth:  resolveBound(req.MaxDepth, e.limits.MaxDepth),
		Budget:    b,
		BatchSize: adjacencyBatch,
	}, acc.Visit)
	if err := impactPhaseError(ctx, walkErr, &meta); err != nil {
		return model.ImpactResult{}, err
	}
	if b.visited <= 0 || b.edges <= 0 {
		markTruncated(&meta, "the traversal budget was exhausted before impact was complete")
	}

	entries := acc.Entries(e.limits.MaxReasonPaths)
	if len(entries) > model.MaxRecordsPerResult {
		entries = entries[:model.MaxRecordsPerResult]
		markTruncated(&meta, "more affected entities than one result can carry")
	}
	entries, hydrateErr := e.hydrateImpactEntries(ctx, entries)
	if err := impactPhaseError(ctx, hydrateErr, &meta); err != nil {
		return model.ImpactResult{}, err
	}
	limit := resolveBound(req.Page.Limit, e.limits.MaxPageItems)
	if len(entries) > limit {
		entries = entries[:limit]
		// No NextCursor: this endpoint issues none, so a truncated page says so
		// instead of handing back a continuation that cannot be resumed.
		markTruncated(&meta, "more affected entities than one page can carry")
	}
	if err := impactPhaseError(ctx, e.attachImpactEvidence(ctx, entries), &meta); err != nil {
		return model.ImpactResult{}, err
	}

	packages, rollupErr := e.rollupPackages(ctx, acc.Relations())
	if err := impactPhaseError(ctx, rollupErr, &meta); err != nil {
		return model.ImpactResult{}, err
	}
	return model.ImpactResult{
		Meta:         meta,
		Entries:      entries,
		Packages:     packages,
		VisitedCount: acc.VisitedCount(),
		EdgeCount:    acc.EdgeCount(),
	}, nil
}

// pendingTruncationReason is the wording Section 11.6 requires when a
// dependence-backed capability is still building. It names the product concept,
// never the engine behind it.
const pendingTruncationReason = "dependence units are still building"

// beginImpactQuery applies the per-request deadline and the process-scoped
// concurrency gate to one impact-family operation. The gate is acquired under
// the deadline, so waiting past it surfaces as CTX_RESOURCE_LIMIT from the gate
// rather than as an unbounded wait.
//
// It is a free function taking the engine rather than a method so the
// impact-family files can share it without claiming a name a sibling lane may
// want on Engine.
func beginImpactQuery(ctx context.Context, e *Engine) (context.Context, func(), error) {
	ctx, cancel := context.WithTimeout(ctx, e.limits.QueryTimeout)
	if e.gate == nil {
		return ctx, cancel, nil
	}
	if err := e.gate.Acquire(ctx); err != nil {
		cancel()
		return nil, nil, err
	}
	return ctx, func() {
		e.gate.Release()
		cancel()
	}, nil
}

// impactAllowlist resolves the relation allowlist an impact walk expands over.
// A request that names none gets ImpactRelations in its frozen priority order;
// a request that names some is honoured exactly, because a caller asking about
// one kind must not silently receive nine others.
func impactAllowlist(requested []model.RelationKind) []model.RelationKind {
	if len(requested) == 0 {
		return ImpactRelations()
	}
	return append([]model.RelationKind(nil), requested...)
}

// resolveBound applies the "zero means the configured default" rule and clamps
// a caller-supplied bound to the configured one: a request can ask for less
// work than the deployment allows, never for more.
func resolveBound(requested, configured int) int {
	if requested <= 0 || requested > configured {
		return configured
	}
	return requested
}

// impactBudget builds the cumulative work allowance for one walk. budget's
// visited and edges fields are the REMAINING allowance, not a running count:
// the caps have no other home (expandOptions carries only MaxDepth), so the
// walk spends them down and a zero remainder is the signal that a hard bound
// stopped the answer short.
func (e *Engine) impactBudget(ctx context.Context, maxVisited, maxEdges int) *budget {
	b := &budget{
		visited: int64(resolveBound(maxVisited, e.limits.MaxVisited)),
		edges:   int64(resolveBound(maxEdges, e.limits.MaxEdges)),
	}
	if deadline, ok := ctx.Deadline(); ok {
		b.deadline = deadline
	} else {
		b.deadline = e.now().Add(e.limits.QueryTimeout)
	}
	return b
}

// impactPendingRows discloses a still-building dependence capability. It runs
// only when the allowlist actually touches a dependence-only kind: `calls` has a
// non-engine path, so a deferred dependence build does not make a calls answer
// incomplete and must not be reported as if it did.
//
// A promoter raises the priority of the deferred work and folds its queue
// position into the row's details. A failed promotion never fails the query --
// the answer is still correct, merely still incomplete -- and report-mode
// workspaces have no promoter at all.
func (e *Engine) impactPendingRows(ctx context.Context, kinds []model.RelationKind) ([]model.CapabilityState, error) {
	if !touchesDependenceOnly(kinds) {
		return nil, nil
	}
	caps, err := e.adjacency.Capabilities(ctx)
	if err != nil {
		return nil, err
	}
	var rows []model.CapabilityState
	for _, c := range caps {
		if c.ProviderID != dependenceProviderID || c.Details["reason"] != deferredReason {
			continue
		}
		if e.promoter != nil {
			if p, perr := e.promoter.Promote(ctx, c.ProviderID, c.Scope); perr == nil {
				c = c.WithDetail("units", fmt.Sprintf("%d", p.Units)).
					WithDetail("position", fmt.Sprintf("%d", p.Position))
				if p.Estimate > 0 {
					// An unmeasured estimate is reported as unmeasured rather
					// than as zero, which would read as "ready now".
					c = c.WithDetail("estimate_ms", fmt.Sprintf("%d", p.Estimate.Milliseconds()))
				}
			}
		}
		rows = append(rows, c)
	}
	return rows, nil
}

// dependenceProviderID and deferredReason are the landed spellings of the
// deferred capability row (internal/index/status.go addDeferred). They are
// matched, never re-invented, so the graph answer discloses exactly the row the
// indexer published. Neither names the analysis engine.
const (
	dependenceProviderID = "dependence"
	deferredReason       = "units_deferred"
)

// touchesDependenceOnly reports whether an allowlist contains a kind only the
// dependence provider can produce.
func touchesDependenceOnly(kinds []model.RelationKind) bool {
	only := make(map[model.RelationKind]bool, 4)
	for _, k := range DependenceOnly() {
		only[k] = true
	}
	for _, k := range kinds {
		if only[k] {
			return true
		}
	}
	return false
}

// impactPhaseError separates the three ways any phase of an impact answer can
// end. A deadline is truncation with whatever is visible, never an error and
// never "nothing is affected" -- which is why it wraps hydration, evidence and
// the rollup too, not just the walk: a walk that spends the whole deadline
// leaves those phases facing an expired context, and failing there would throw
// away the truncated answer the walk just produced. A cancellation is the
// caller's own stop; anything else is a real failure. errStopExpansion is the
// visitor's own bounded stop and is not a failure at all.
func impactPhaseError(ctx context.Context, err error, meta *model.QueryMeta) error {
	switch {
	case err == nil, errors.Is(err, errStopExpansion):
		return nil
	case errors.Is(err, context.DeadlineExceeded):
		markTruncated(meta, "the query deadline expired before impact was complete")
		return nil
	case errors.Is(err, context.Canceled):
		return model.Canceled(ctx.Err())
	default:
		return err
	}
}

// markTruncated records the first reason an answer fell short. The first reason
// wins because it is the bound that actually stopped the work; overwriting it
// with a later, broader one would misreport why the answer is incomplete.
func markTruncated(meta *model.QueryMeta, reason string) {
	if meta.Truncated {
		return
	}
	meta.Truncated = true
	meta.TruncationReason = reason
}

// impactNode is one affected entity as the walk found it: the cheapest route
// that reached it, the direction that route walked, and the reasons every edge
// that touched it contributes.
type impactNode struct {
	id        model.NodeID
	depth     int
	cost      int64
	direction model.Direction
	via       model.RelationID
	parent    model.NodeID
	reasons   []string
	seen      map[string]bool
}

// impactAccumulator collects one walk. Each node is admitted once -- which is
// what keeps a cycle (a -> b -> c -> a) from producing two entries for the same
// symbol -- while every later edge that reaches an already-admitted node still
// contributes its reason.
type impactAccumulator struct {
	seeds   []model.NodeID
	isSeed  map[model.NodeID]bool
	byNode  map[model.NodeID]*impactNode
	order   []model.NodeID
	edges   []model.Relation
	edgeCnt int64
}

func newImpactAccumulator(start []model.NodeID) *impactAccumulator {
	a := &impactAccumulator{
		isSeed: make(map[model.NodeID]bool, len(start)),
		byNode: map[model.NodeID]*impactNode{},
	}
	for _, s := range start {
		if a.isSeed[s] {
			continue
		}
		a.isSeed[s] = true
		a.seeds = append(a.seeds, s)
	}
	// The frozen frontier order is (depth asc, NodeID asc): seeds enter at
	// depth 0 de-duplicated and sorted, so two requests naming the same seeds in
	// different orders answer identically.
	sort.Slice(a.seeds, func(i, j int) bool { return a.seeds[i] < a.seeds[j] })
	return a
}

func (a *impactAccumulator) Seeds() []model.NodeID { return a.seeds }

// Visit is the expand visitor: one call per admitted edge, in frontier order.
func (a *impactAccumulator) Visit(state frontierState, rel model.Relation) error {
	a.edgeCnt++
	a.edges = append(a.edges, rel)
	if a.isSeed[state.Node] {
		// A seed is the thing being changed, not something the change affects.
		return nil
	}
	direction := impactEdgeDirection(state.Node, rel)
	reason := impactReason(rel.Kind, direction, state.Depth)
	entry, ok := a.byNode[state.Node]
	if !ok {
		entry = &impactNode{
			id: state.Node, depth: state.Depth, cost: state.Cost,
			direction: direction, via: rel.ID,
			parent: otherEndpoint(state.Node, rel),
			seen:   map[string]bool{},
		}
		a.byNode[state.Node] = entry
		a.order = append(a.order, state.Node)
	}
	if direction == model.DirectionIncoming {
		// A node reachable both ways under DirectionBoth is reported as
		// incoming: "may need modification" is the stronger claim, and leaving
		// it to whichever edge the walk saw first would make the Section 14.3
		// discriminator depend on frontier order.
		entry.direction = model.DirectionIncoming
	}
	if !entry.seen[reason] && len(entry.reasons) < model.MaxReasonsPerEntry {
		entry.seen[reason] = true
		entry.reasons = append(entry.reasons, reason)
	}
	return nil
}

// impactEdgeDirection is the Section 14.3 discriminator. The walk admitted
// state.Node through rel, so which end of rel it landed on says which way we
// walked: landing on the source means something upstream points at the seed and
// may need modification (incoming); landing on the target means the seed points
// at it and it may need reading (outgoing). A self-edge lands on both and is
// reported as incoming, the stronger of the two claims.
func impactEdgeDirection(node model.NodeID, rel model.Relation) model.Direction {
	if rel.From == node {
		return model.DirectionIncoming
	}
	return model.DirectionOutgoing
}

// otherEndpoint is the node an edge was traversed from, given the node it
// reached.
func otherEndpoint(node model.NodeID, rel model.Relation) model.NodeID {
	if rel.From == node {
		return rel.To
	}
	return rel.From
}

// impactReason renders one reason in product vocabulary, naming the relation
// kind, the direction and the depth -- never the analysis engine that produced
// the edge. Section 14.3 rejects an entry with no reason outright.
func impactReason(kind model.RelationKind, dir model.Direction, depth int) string {
	if dir == model.DirectionIncoming {
		return fmt.Sprintf("%s this symbol (incoming, depth %d)", kind, depth)
	}
	return fmt.Sprintf("this symbol %s it (outgoing, depth %d)", kind, depth)
}

func (a *impactAccumulator) EdgeCount() int64 { return a.edgeCnt }

// VisitedCount counts the seeds plus every node the walk admitted.
func (a *impactAccumulator) VisitedCount() int64 { return int64(len(a.seeds) + len(a.byNode)) }

// Relations returns every admitted edge, for the package rollup that rides on
// the same walk.
func (a *impactAccumulator) Relations() []model.Relation { return a.edges }

// Entries ranks the affected entities. ScoreMicros is integer arithmetic over
// the frozen cost table -- 1_000_000 / (1 + cost) -- so a cheaper, more direct
// route always outranks a longer one and no float ever enters the ordering.
// The order is (ScoreMicros desc, Depth asc, Path asc, NodeID asc); Path is
// filled during hydration, so ties settle on NodeID here and stay stable.
func (a *impactAccumulator) Entries(maxPaths int) []model.ImpactEntry {
	entries := make([]model.ImpactEntry, 0, len(a.order))
	for _, id := range a.order {
		n := a.byNode[id]
		entry := model.ImpactEntry{
			NodeID:      n.id,
			Direction:   n.direction,
			Depth:       n.depth,
			ScoreMicros: 1_000_000 / (1 + n.cost),
			Reasons:     n.reasons,
		}
		if p, ok := a.path(n); ok && maxPaths > 0 {
			entry.Paths = []model.RelationPath{p}
		}
		entries = append(entries, entry)
	}
	sort.SliceStable(entries, func(i, j int) bool {
		x, y := entries[i], entries[j]
		if x.ScoreMicros != y.ScoreMicros {
			return x.ScoreMicros > y.ScoreMicros
		}
		if x.Depth != y.Depth {
			return x.Depth < y.Depth
		}
		return x.NodeID < y.NodeID
	})
	return entries
}

// path reconstructs the route that admitted n by walking the parent chain back
// to a seed. Each node records its parent once, on first admission, so the
// chain is a tree and a cycle in the graph cannot make it loop; the length cap
// bounds it even if a future walk changed that.
func (a *impactAccumulator) path(n *impactNode) (model.RelationPath, bool) {
	relations := make([]model.RelationID, 0, n.depth+1)
	for cur := n; cur != nil; {
		relations = append(relations, cur.via)
		if len(relations) > model.MaxRelationsPerPath {
			return model.RelationPath{}, false
		}
		if a.isSeed[cur.parent] {
			break
		}
		next, ok := a.byNode[cur.parent]
		if !ok {
			return model.RelationPath{}, false
		}
		cur = next
	}
	for i, j := 0, len(relations)-1; i < j; i, j = i+1, j-1 {
		relations[i], relations[j] = relations[j], relations[i]
	}
	return model.RelationPath{Relations: relations, CostUnits: n.cost}, true
}

// hydrateImpactEntries fills the kind, file and path an entry reports, in
// batched round trips rather than one lookup per entry. A node the pinned
// generation cannot hydrate is dropped: reporting it without its kind would
// fail the entry contract, and inventing one would publish a fact nothing
// backs.
func (e *Engine) hydrateImpactEntries(ctx context.Context, entries []model.ImpactEntry) ([]model.ImpactEntry, error) {
	ids := make([]model.NodeID, 0, len(entries))
	for _, entry := range entries {
		ids = append(ids, entry.NodeID)
	}
	nodes, err := e.nodesByID(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := entries[:0]
	for _, entry := range entries {
		n, ok := nodes[entry.NodeID]
		if !ok {
			continue
		}
		entry.Kind = n.Kind
		entry.FileID = n.FileID
		entry.Path = clipPath(n.QualifiedName)
		out = append(out, entry)
	}
	// Path participates in the tie-break, so it is applied once it is known.
	sort.SliceStable(out, func(i, j int) bool {
		x, y := out[i], out[j]
		if x.ScoreMicros != y.ScoreMicros {
			return x.ScoreMicros > y.ScoreMicros
		}
		if x.Depth != y.Depth {
			return x.Depth < y.Depth
		}
		if x.Path != y.Path {
			return x.Path < y.Path
		}
		return x.NodeID < y.NodeID
	})
	return out, nil
}

// nodesByID hydrates ids in bounded batches and returns them keyed by id.
func (e *Engine) nodesByID(ctx context.Context, ids []model.NodeID) (map[model.NodeID]model.Node, error) {
	out := make(map[model.NodeID]model.Node, len(ids))
	for _, batch := range impactChunkNodes(dedupeNodes(ids)) {
		nodes, err := e.adjacency.NodesByID(ctx, batch)
		if err != nil {
			return nil, err
		}
		for _, n := range nodes {
			out[n.ID] = n
		}
	}
	return out, nil
}

// attachImpactEvidence makes every returned reason evidence-backed: the
// evidence for a whole page of paths is hydrated in bounded batches, never one
// query per edge.
func (e *Engine) attachImpactEvidence(ctx context.Context, entries []model.ImpactEntry) error {
	var ids []model.RelationID
	for _, entry := range entries {
		for _, p := range entry.Paths {
			ids = append(ids, p.Relations...)
		}
	}
	evidence := make(map[model.RelationID][]model.EvidenceID, len(ids))
	for _, batch := range impactChunkRelations(dedupeRelations(ids)) {
		got, err := e.adjacency.EvidenceFor(ctx, batch, model.MaxRelationsPerPath)
		if err != nil {
			return err
		}
		for id, ev := range got {
			evidence[id] = ev
		}
	}
	for i := range entries {
		for j := range entries[i].Paths {
			p := &entries[i].Paths[j]
			var ev []model.EvidenceID
			for _, rel := range p.Relations {
				ev = append(ev, evidence[rel]...)
				if len(ev) >= model.MaxRelationsPerPath {
					ev = ev[:model.MaxRelationsPerPath]
					break
				}
			}
			p.Evidence = ev
		}
	}
	return nil
}

// dedupeNodes and dedupeRelations keep a batch from asking for the same row
// twice, which a cycle or a shared prefix path makes routine.
func dedupeNodes(ids []model.NodeID) []model.NodeID {
	seen := make(map[model.NodeID]bool, len(ids))
	out := ids[:0]
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

func dedupeRelations(ids []model.RelationID) []model.RelationID {
	seen := make(map[model.RelationID]bool, len(ids))
	out := ids[:0]
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

// impactChunkNodes and impactChunkRelations split a lookup into adjacencyBatch
// sized round trips, so a wide frontier never becomes one unbounded query.
func impactChunkNodes(ids []model.NodeID) [][]model.NodeID {
	var out [][]model.NodeID
	for len(ids) > adjacencyBatch {
		out = append(out, ids[:adjacencyBatch])
		ids = ids[adjacencyBatch:]
	}
	if len(ids) > 0 {
		out = append(out, ids)
	}
	return out
}

func impactChunkRelations(ids []model.RelationID) [][]model.RelationID {
	var out [][]model.RelationID
	for len(ids) > adjacencyBatch {
		out = append(out, ids[:adjacencyBatch])
		ids = ids[adjacencyBatch:]
	}
	if len(ids) > 0 {
		out = append(out, ids)
	}
	return out
}
