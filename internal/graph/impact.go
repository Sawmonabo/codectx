package graph

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/Sawmonabo/codectx/internal/config"
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
func (e *Engine) Impact(ctx context.Context, req model.ImpactRequest) (res model.ImpactResult, err error) {
	defer func() { err = typedContextError(ctx, err) }()
	if err := req.Validate(); err != nil {
		return model.ImpactResult{}, err
	}
	ctx, deadline, done, err := beginImpactQuery(ctx, e)
	if err != nil {
		return model.ImpactResult{}, err
	}
	defer done()

	kinds := impactAllowlist(req.Relations)
	maxDepth, depthNotice := resolveLimit("max_depth", req.MaxDepth, e.limits.Depth())
	limit := resolveBound(req.Page.Limit, e.limits.MaxPageItems)
	// The page limit is part of the normalized query a continuation is bound
	// to, exactly as it is for a traversal: a resumed page that asked for a
	// different limit would cut the ranked list somewhere the issuing page
	// never stopped.
	queryHash := traversalQueryHash(req.Direction, kinds, req.Start, maxDepth.Int(), limit)

	answer, entries, b, nextCursor, err := e.walkImpact(ctx, req, kinds, maxDepth, limit, queryHash, deadline)
	if err != nil {
		return model.ImpactResult{}, err
	}

	meta := model.QueryMeta{Binding: e.adjacency.Binding(), Completeness: answer.Completeness,
		Notices:    appendNotice(append([]string(nil), answer.Notices...), depthNotice),
		NextCursor: nextCursor}
	if answer.Truncated {
		markTruncated(&meta, answer.Reason)
	}
	result := model.ImpactResult{
		Meta:     meta,
		Entries:  entries,
		Packages: answer.Packages,
		// Cumulative spend, the same accounting the traversal operations
		// report: a resumed page carries what the earlier pages already spent
		// and adds its own, so the counters GROW across the pages of one walk
		// rather than repeating a single-shot total.
		VisitedCount: b.visited,
		EdgeCount:    b.edges,
	}
	if err := result.Validate(); err != nil {
		return model.ImpactResult{}, err
	}
	return result, nil
}

// impactEndpoint binds an impact continuation to the operation that issued it.
// A cursor minted by impact means nothing to a traversal even at the same
// generation and seeds, and verifyContinuation rejects it.
const impactEndpoint = "graph.impact"

// impactAnswer is the answer-level half of one impact answer: the facts that
// describe the chunk of the walk this page read rather than the entries
// themselves -- whether it was truncated and why, the bound notices the request
// resolved, the generation's capability rows and the package rollup over this
// page's edges.
type impactAnswer struct {
	Truncated bool
	Reason    string
	// Notices travel with the answer so every page repeats the same
	// disclosure: page 2 was produced under the same bounds page 1 was, and
	// must say so.
	Notices      []string
	Completeness []model.CapabilityState
	Packages     []model.PackageEdge
}

// walkImpact is ONE page of the impact walk: a page-bounded expansion over the
// allowlist, the ranking pass, hydration and the rollup, followed by the
// continuation the next page resumes from.
//
// It is the same spooled, cursor-resumable continuation a traversal has, and
// for the same reason: the accumulator below holds at most `limit` admitted
// edges -- and therefore at most `limit` affected nodes -- so its heap is a
// function of the page, never of the reachable subgraph. What outlives the page
// is the frontier and the cumulative admitted-node set, and both live in the
// continuation spool rather than in memory (see cursor.go and visited.go).
//
// The trade this makes is explicit: each page ranks its OWN chunk, so the pages
// together enumerate exactly the affected-node set the former single-shot
// answer did, but the rank order is per page and a node reached again from a
// later page's frontier is reported again with that page's reasons. Ranking the
// whole reachable set in one order needs the whole set in one place, which is
// the heap this fix exists to remove.
func (e *Engine) walkImpact(ctx context.Context, req model.ImpactRequest, kinds []model.RelationKind,
	maxDepth config.Limit, limit int, queryHash string,
	deadline time.Time) (impactAnswer, []model.ImpactEntry, *budget, string, error) {
	var answer impactAnswer
	meta := model.QueryMeta{}
	// The capability disclosure happens before the walk: a missing dependence
	// edge must not read as a genuine absence of impact.
	caps, deferred, err := e.completeness(ctx, kinds)
	if err != nil {
		return answer, nil, nil, "", err
	}
	meta.Completeness = caps
	if deferred {
		markTruncated(&meta, reasonDependence)
	}

	b := &budget{deadline: deadline, now: e.now}
	var resume *resumeState
	if req.Page.Cursor != "" {
		resume, err = e.resumeTraversal(ctx, req.Page.Cursor, impactEndpoint, queryHash, deadline)
		if err != nil {
			return answer, nil, nil, "", err
		}
		// The consumed continuation's spool outlives the walk: the membership
		// probes and the spill that copies the cumulative set forward both read
		// from it. Deferring the release here keeps an error return from leaking
		// a spool and a lease for the whole cursor TTL.
		defer resume.Release()
		// The resumed budget carries the earlier pages' cumulative spend by
		// ASSIGNMENT, so replaying one cursor twice neither resets nor doubles it.
		b = resume.Budget
	}
	maxVisited, visitedNotice := resolveLimit("max_visited", req.MaxVisited, e.limits.Visited())
	maxEdges, edgeNotice := resolveLimit("max_edges", req.MaxEdges, e.limits.Edges())
	answer.Notices = appendNotice(appendNotice(answer.Notices, visitedNotice), edgeNotice)
	acc := newImpactAccumulator(req.Start, b, maxVisited, maxEdges, limit)
	state, walkErr := expand(ctx, e.adjacency, acc.Seeds(), expandOptions{
		Direction:     req.Direction,
		Kinds:         kinds,
		MaxDepth:      maxDepth,
		Budget:        b,
		BatchSize:     adjacencyBatch,
		FrontierBytes: e.limits.FrontierBytes,
		Resume:        resume,
	}, acc.Visit)
	if err := impactPhaseError(ctx, walkErr, &meta); err != nil {
		return answer, nil, nil, "", err
	}
	switch {
	case acc.reason != "":
		markTruncated(&meta, acc.reason)
	case b.frontierHit:
		// A level the frontier budget cut short is truncation the caller must
		// see: the ranking below is over the edges that were read, not all of them.
		markTruncated(&meta, reasonFrontierBytes)
	case state.DepthLimited:
		// Nodes at the depth bound were admitted but never expanded; an impact
		// answer that stopped there is not the whole blast radius.
		markTruncated(&meta, reasonDepth)
	}

	entries := acc.Entries(e.limits.ReasonPaths())
	entries, unhydratable, hydrateErr := e.hydrateImpactEntries(ctx, entries)
	if err := impactPhaseError(ctx, hydrateErr, &meta); err != nil {
		return answer, nil, nil, "", err
	}
	if unhydratable > 0 {
		// An entry the pinned generation cannot hydrate is still a node the
		// walk admitted. Dropping it silently made the answer read as the
		// complete impact set; the count is the honest channel, since the
		// entry itself cannot be listed without inventing its kind.
		answer.Notices = appendNotice(answer.Notices,
			fmt.Sprintf("impact: %d affected node(s) were reached but could not be hydrated from the pinned generation and are not listed",
				unhydratable))
	}
	if err := impactPhaseError(ctx, e.attachImpactEvidence(ctx, entries), &meta); err != nil {
		return answer, nil, nil, "", err
	}
	packages, rollupErr := e.rollupPackages(ctx, acc.Relations(), &meta)
	if err := impactPhaseError(ctx, rollupErr, &meta); err != nil {
		return answer, nil, nil, "", err
	}
	nextCursor, err := e.continueWalk(ctx, b, impactEndpoint, queryHash, state, acc.lastOwner, acc.lastKey, resume)
	if err != nil {
		return answer, nil, nil, "", err
	}
	notices := answer.Notices
	answer = impactAnswer{Truncated: meta.Truncated, Reason: meta.TruncationReason,
		Notices: notices, Completeness: meta.Completeness, Packages: packages}
	return answer, entries, b, nextCursor, nil
}

// continueWalk mints the continuation for a page-bounded impact or
// package-dependency walk, or returns an empty token when the walk is done.
//
// It is the same rule the traversal applies (traverse.go): every stop that left
// a frontier standing is resumable, because the visited, edge, page-item and
// frontier-byte bounds are all PER-PAGE work budgets now. The depth bound is the
// single exception -- it is part of the query hash a cursor is bound to, so a
// token minted for it would resume a walk already past it, stop at once and
// mint another.
func (e *Engine) continueWalk(ctx context.Context, b *budget, endpoint, queryHash string,
	state walkState, lastOwner model.NodeID, lastKey model.RelationID, resume *resumeState) (string, error) {
	if len(state.Frontier) == 0 || state.DepthLimited {
		return "", nil
	}
	// The cumulative admitted set is NOT materialized here: only the nodes THIS
	// page admitted are, and the rest is copied spool to spool.
	var carried visitedStream
	if resume != nil {
		carried = resume.Visited
	}
	return e.nextTraversalCursor(ctx, b, continuation{
		Endpoint:  endpoint,
		QueryHash: queryHash,
		Depth:     state.Depth,
		LastOwner: lastOwner,
		LastKey:   lastKey,
		Frontier:  state.Frontier,
		Visited:   state.Admitted.newlyAdmitted(),
		Carried:   carried,
	})
}

// beginImpactQuery applies the per-request deadline and the process-scoped
// concurrency gate to one impact-family operation. The gate is acquired under
// the deadline, so waiting past it surfaces as CTX_RESOURCE_LIMIT from the gate
// rather than as an unbounded wait.
//
// It is a free function taking the engine rather than a method so the
// impact-family files can share it without claiming a name a sibling lane may
// want on Engine.
func beginImpactQuery(ctx context.Context, e *Engine) (context.Context, time.Time, func(), error) {
	// One clock: the deadline is measured on the ENGINE clock, the same one the
	// walk budget compares against, and it is returned so the caller reuses this
	// instant instead of recomputing a later one after the gate wait.
	deadline := e.now().Add(e.limits.QueryTimeout)
	ctx, cancel := context.WithDeadline(ctx, deadline)
	if e.gate == nil {
		return ctx, deadline, cancel, nil
	}
	if err := e.gate.Acquire(ctx); err != nil {
		cancel()
		return nil, time.Time{}, nil, err
	}
	return ctx, deadline, func() {
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

// impactPhaseError separates the three ways any phase of an impact answer can
// end. A deadline is truncation with whatever is visible, never an error and
// never "nothing is affected" -- which is why it wraps hydration, evidence and
// the rollup too, not just the walk: a walk that spends the whole deadline
// leaves those phases facing an expired context, and failing there would throw
// away the truncated answer the walk just produced. A cancellation is the
// caller's own stop; anything else is a real failure. errStopExpansion is the
// visitor's own bounded stop and is not a failure at all.
//
// Both spellings of each condition are handled: the raw context error from a
// phase that only checks ctx, and the typed CTX_QUERY_DEADLINE / CTX_CANCELED
// the walk raises through checkWalk.
func impactPhaseError(ctx context.Context, err error, meta *model.QueryMeta) error {
	if err == nil || errors.Is(err, errStopExpansion) {
		return nil
	}
	var typed *model.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded),
		errors.As(err, &typed) && typed.Code == model.CodeQueryDeadline:
		markTruncated(meta, reasonDeadline)
		return nil
	case errors.Is(err, context.Canceled):
		return model.Canceled(ctx.Err())
	default:
		return err
	}
}

// reasonDeadline is the truncation reason for an answer the query deadline cut
// short. It names the bound, like every other truncation reason, so a caller
// can tell a slow query from a budget that was too small.
const reasonDeadline = "query deadline reached"

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
//
// It is also where MaxVisited and MaxEdges are enforced: expand deliberately
// does not know those caps, because only the caller can compare the cumulative,
// cursor-carried spend in budget against the bounds the request resolved.
type impactAccumulator struct {
	seeds      []model.NodeID
	isSeed     map[model.NodeID]bool
	byNode     map[model.NodeID]*impactNode
	order      []model.NodeID
	edges      []model.Relation
	budget     *budget
	maxVisited config.Limit
	maxEdges   config.Limit
	// pageItems is the page's item bound. It is what makes byNode, order and
	// edges page-sized rather than walk-sized: the accumulator stops the
	// expansion once it holds this many admitted edges, and since a node is
	// admitted only by an edge, order and byNode can never exceed it either.
	pageItems int
	reason    string
	// lastOwner and lastKey are the keyset position the continuation resumes
	// from: the frontier node whose chunk the last admitted row came from, and
	// that row's relation id.
	lastOwner model.NodeID
	lastKey   model.RelationID
}

func newImpactAccumulator(start []model.NodeID, b *budget, maxVisited, maxEdges config.Limit,
	pageItems int) *impactAccumulator {
	a := &impactAccumulator{
		isSeed:     make(map[model.NodeID]bool, len(start)),
		byNode:     map[model.NodeID]*impactNode{},
		budget:     b,
		maxVisited: maxVisited,
		maxEdges:   maxEdges,
		pageItems:  pageItems,
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
// state is the FRONTIER node the edge left from, so the node this edge affects
// is the other end of it, one hop deeper and one edge cost further away.
func (a *impactAccumulator) Visit(state frontierState, rel model.Relation) error {
	switch {
	case a.maxEdges.Exceeded(a.budget.pageEdges + 1):
		a.reason = reasonEdgeBudget
		return errStopExpansion
	case a.maxVisited.Exceeded(a.budget.pageVisited + 1):
		a.reason = reasonVisitedBudget
		return errStopExpansion
	case len(a.edges) >= a.pageItems:
		// The page item bound, exactly as a traversal applies it
		// (traverse.go's `len(relations) >= maxItems`): it ends THIS page, and
		// the caller mints the continuation the next one resumes from. It is
		// also the bound that keeps edges, order and byNode page-sized.
		a.reason = reasonPageFull
		return errStopExpansion
	}
	a.edges = append(a.edges, rel)
	// The keyset position a continuation resumes from.
	a.lastOwner, a.lastKey = state.Node, rel.ID
	reached := otherEndpoint(state.Node, rel)
	if a.isSeed[reached] {
		// A seed is the thing being changed, not something the change affects.
		return nil
	}
	direction := impactEdgeDirection(state.Node, rel)
	depth := state.Depth + 1
	reason := impactReason(rel.Kind, direction, depth)
	entry, ok := a.byNode[reached]
	if !ok {
		entry = &impactNode{
			id: reached, depth: depth, cost: state.Cost + Cost(rel.Kind),
			direction: direction, via: rel.ID, parent: state.Node,
			seen: map[string]bool{},
		}
		a.byNode[reached] = entry
		a.order = append(a.order, reached)
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

// impactEdgeDirection is the Section 14.3 discriminator, read off the frontier
// node the edge left from: an edge leaving that node lands on something the
// seed depends on, which may need reading (outgoing); an edge arriving at it
// comes from something that depends on the seed and may need modification
// (incoming). A self-edge leaves and arrives at once and is reported as
// outgoing, then upgraded to incoming by the visitor.
func impactEdgeDirection(owner model.NodeID, rel model.Relation) model.Direction {
	if rel.From == owner {
		return model.DirectionOutgoing
	}
	return model.DirectionIncoming
}

// otherEndpoint is the node an edge reaches, given the frontier node it left
// from.
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

// Relations returns every admitted edge, for the package rollup that rides on
// the same walk.
func (a *impactAccumulator) Relations() []model.Relation { return a.edges }

// Entries ranks the affected entities. ScoreMicros is integer arithmetic over
// the frozen cost table -- 1_000_000 / (1 + cost) -- so a cheaper, more direct
// route always outranks a longer one and no float ever enters the ordering.
// The order is (ScoreMicros desc, Depth asc, Name asc, NodeID asc); Name is
// filled during hydration, so ties settle on NodeID here and stay stable.
//
// reasonPaths is context.max_reason_paths_per_entry. The accumulator records
// ONE parent per admitted node -- the edge that first reached it, which is the
// edge the entry's reason names -- so there is exactly one route to
// reconstruct and a positive bound admits it while a zero bound suppresses it.
// Enumerating alternate routes would mean keeping every parent of every node
// for the whole walk, which is the frontier blow-up MaxVisited exists to
// prevent; `path` is the command that answers "the equal-cost routes", and it
// is where this key bounds a count greater than one. docs/queries.md states
// the split.
func (a *impactAccumulator) Entries(reasonPaths config.Limit) []model.ImpactEntry {
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
		if p, ok := a.path(n); ok && !reasonPaths.Exceeded(1) {
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
// generation cannot hydrate cannot be listed: reporting it without its kind
// would fail the entry contract, and inventing one would publish a fact
// nothing backs. It is COUNTED and returned so the caller discloses the
// omission instead of letting the answer read as complete.
func (e *Engine) hydrateImpactEntries(ctx context.Context, entries []model.ImpactEntry) ([]model.ImpactEntry, int, error) {
	ids := make([]model.NodeID, 0, len(entries))
	for _, entry := range entries {
		ids = append(ids, entry.NodeID)
	}
	nodes, err := e.nodesByID(ctx, ids)
	if err != nil {
		return nil, 0, err
	}
	dropped := 0
	out := entries[:0]
	for _, entry := range entries {
		n, ok := nodes[entry.NodeID]
		if !ok {
			dropped++
			continue
		}
		entry.Kind = n.Kind
		entry.FileID = n.FileID
		entry.Name = clipTo(n.QualifiedName, model.MaxQualifiedNameBytes)
		out = append(out, entry)
	}
	// Name participates in the tie-break, so it is applied once it is known.
	sort.SliceStable(out, func(i, j int) bool {
		x, y := out[i], out[j]
		if x.ScoreMicros != y.ScoreMicros {
			return x.ScoreMicros > y.ScoreMicros
		}
		if x.Depth != y.Depth {
			return x.Depth < y.Depth
		}
		if x.Name != y.Name {
			return x.Name < y.Name
		}
		return x.NodeID < y.NodeID
	})
	return out, dropped, nil
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
