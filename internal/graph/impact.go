package graph

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/Sawmonabo/codectx/internal/config"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
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

// walkImpact answers ONE page of an impact query, but never with a partial
// walk: ruling P2 says the request that MINTS the answer runs the expansion to
// completion, streams every admitted edge into a disk-backed sort, ranks the
// whole affected set once, serves the first page and spools the globally
// ranked remainder behind a cursor. Later pages read that spool and walk
// nothing.
//
// What that buys, and what the page-bounded predecessor could not give: the
// union of the pages is exactly the single-shot answer, in exactly the
// single-shot ORDER, with every node reported once. A node reached again from
// a later frontier used to be listed a second time with that page's reasons,
// because the accumulator was page-scoped; it is now one record that pass 1
// folds.
//
// Peak heap is unchanged in KIND and bounded by the same things it always was:
// one internal page's frontier (Limits.FrontierBytes), the sort's run buffer
// (a quarter of the query memory admission) plus its merge fan-in, and one
// served page. None of the three is a function of the reachable set.
//
// The two ways this still ends early are both pages, never answers: the query
// deadline (ruling P3) mints the `f` walk continuation with no entries at all
// and the next request carries the walk on, and the user-set depth bound is
// reported as truncation with the ranked answer it did reach.
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
		c, rb, err := e.verifyContinuation(req.Page.Cursor, impactEndpoint, queryHash, deadline)
		if err != nil {
			return answer, nil, nil, "", err
		}
		if c.Ranked {
			// The walk is over and the order is settled: this page is a read of
			// the ranked spool and expands nothing.
			return e.serveRankedImpact(ctx, c, rb, limit, answer, meta)
		}
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

	var (
		acc   *impactAccumulator
		state walkState
	)
	// ONE streamed pass: the sort's emit callback drives the walk, so no record
	// is held between the edge that produced it and the run buffer it lands in.
	ranked, rankErr := e.rankImpact(ctx, func(add func(impactRecord) error) error {
		acc = newImpactAccumulator(req.Start, b, maxVisited, maxEdges, 0, add)
		var walkErr error
		// pageItems 0: see impactAccumulator.pageItems. The walk is bounded by
		// the frontier byte ceiling and the per-page work budgets, which
		// runWalkToCompletion returns at every internal boundary, and it ends
		// only when the frontier is empty, the depth bound is reached or the
		// deadline passes.
		state, walkErr = e.runWalkToCompletion(ctx, acc.Seeds(), expandOptions{
			Direction:     req.Direction,
			Kinds:         kinds,
			MaxDepth:      maxDepth,
			Budget:        b,
			BatchSize:     adjacencyBatch,
			FrontierBytes: e.limits.FrontierBytes,
			Resume:        resume,
		}, acc.Visit)
		return walkErr
	})
	if state.ReleaseCarried != nil {
		// After the continuation below has been spilled: the spill is what
		// reads the carried stream.
		defer state.ReleaseCarried()
	}
	if err := impactPhaseError(ctx, rankErr, &meta); err != nil {
		return answer, nil, nil, "", err
	}
	if ranked != nil {
		defer ranked.Close()
	}
	if b.deadlineHit && len(state.Frontier) > 0 {
		// Ruling P3: the deadline ended this PAGE, not the answer. Nothing is
		// ranked and nothing is served -- ranking a walk that is still running
		// would publish an order the next page contradicts -- and the walk
		// continuation carries the frontier forward.
		markTruncated(&meta, reasonDeadline)
		next, err := e.continueWalk(ctx, b, impactEndpoint, queryHash, state, acc.lastOwner, acc.lastKey, resume)
		if err != nil {
			return answer, nil, nil, "", err
		}
		answer = impactAnswer{Truncated: true, Reason: meta.TruncationReason,
			Notices: answer.Notices, Completeness: meta.Completeness}
		return answer, nil, b, next, nil
	}
	switch {
	case acc.reason != "":
		// The LAST link's stop, and so the one that ended the answer: an
		// internal boundary's reason is cleared by the first edge the next link
		// admits (impactAccumulator.Visit).
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

	entries, nextCursor, err := e.serveRankedRun(ctx, ranked, queryHash, b, limit, &answer, &meta)
	if err != nil {
		return answer, nil, nil, "", err
	}
	if err := impactPhaseError(ctx, e.attachImpactEvidence(ctx, entries), &meta); err != nil {
		return answer, nil, nil, "", err
	}
	packages, rollupErr := e.rollupPackages(ctx, acc.Relations(), &meta)
	if err := impactPhaseError(ctx, rollupErr, &meta); err != nil {
		return answer, nil, nil, "", err
	}
	answer = impactAnswer{Truncated: meta.Truncated, Reason: meta.TruncationReason,
		Notices: answer.Notices, Completeness: meta.Completeness, Packages: packages}
	return answer, entries, b, nextCursor, nil
}

// serveRankedRun serves the FIRST page off the freshly ranked run and spools
// everything after it behind a ranked continuation.
func (e *Engine) serveRankedRun(ctx context.Context, run *pagination.SortedRun[impactRecord], queryHash string,
	b *budget, limit int, answer *impactAnswer, meta *model.QueryMeta) ([]model.ImpactEntry, string, error) {
	page := make([]impactRecord, 0, limit)
	if err := run.Each(func(r impactRecord) error {
		if err := ctx.Err(); err != nil {
			return typedContextError(ctx, err)
		}
		page = append(page, r)
		if len(page) == limit {
			return errStopExpansion
		}
		return nil
	}); err != nil && !errors.Is(err, errStopExpansion) {
		return nil, "", err
	}
	entries, err := e.impactPage(ctx, page, answer, meta)
	if err != nil {
		return nil, "", err
	}
	if int64(len(page)) >= run.Len() {
		return entries, "", nil
	}
	next, err := e.nextRankedCursor(ctx, b, queryHash, run.Len(), int64(len(page)),
		rankedRunTail(ctx, run, len(page), encodeImpactRecord))
	return entries, next, err
}

// serveRankedImpact serves a LATER page: it reads the ranked spool the previous
// page left, copies what follows this page into a fresh one, and reports the
// answer-level facts the first page settled.
func (e *Engine) serveRankedImpact(ctx context.Context, c traversalCursor, b *budget, limit int,
	answer impactAnswer, meta model.QueryMeta) (impactAnswer, []model.ImpactEntry, *budget, string, error) {
	// The consumed spool is released only after the page is built: the copy
	// below reads it.
	defer e.releaseConsumed(context.WithoutCancel(ctx), c.SpoolID, c.LeaseID)
	tail := rankedTail{SpoolID: c.SpoolID, LeaseID: c.LeaseID,
		Offset: c.RankOffset, Served: c.RankServed, Total: c.RankTotal}
	page, rest, err := servePage(ctx, e.spools, c.spoolCursor(), e.now(), tail, limit, decodeImpactRecord)
	if err != nil {
		return answer, nil, nil, "", err
	}
	entries, err := e.impactPage(ctx, page, &answer, &meta)
	if err != nil {
		return answer, nil, nil, "", err
	}
	if err := impactPhaseError(ctx, e.attachImpactEvidence(ctx, entries), &meta); err != nil {
		return answer, nil, nil, "", err
	}
	var next string
	if !rest.done() {
		next, err = e.nextRankedCursor(ctx, b, c.QueryHash, rest.Total, rest.Served,
			rankedSpoolTail(ctx, e.spools, c.spoolCursor(), e.now(), rest.Offset))
		if err != nil {
			return answer, nil, nil, "", err
		}
	}
	answer.Completeness = meta.Completeness
	answer.Truncated, answer.Reason = meta.Truncated, meta.TruncationReason
	return answer, entries, b, next, nil
}

// impactPage projects one page of ranked records into served entries and
// hydrates them. The records arrive in ruling P1's order and stay in it.
func (e *Engine) impactPage(ctx context.Context, page []impactRecord, answer *impactAnswer,
	meta *model.QueryMeta) ([]model.ImpactEntry, error) {
	reasonPaths := e.limits.ReasonPaths()
	entries := make([]model.ImpactEntry, 0, len(page))
	for _, r := range page {
		entry := model.ImpactEntry{NodeID: r.NodeID, Direction: r.Direction, Depth: r.Depth,
			ScoreMicros: r.scoreMicros(), Reasons: r.Reasons}
		if len(r.Route) > 0 && len(r.Route) <= model.MaxRelationsPerPath && !reasonPaths.Exceeded(1) {
			entry.Paths = []model.RelationPath{{Relations: r.Route, CostUnits: r.Cost}}
		}
		entries = append(entries, entry)
	}
	entries, unhydratable, hydrateErr := e.hydrateImpactEntries(ctx, entries)
	if err := impactPhaseError(ctx, hydrateErr, meta); err != nil {
		return nil, err
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
	return entries, nil
}

// nextRankedCursor writes the ranked remainder to a FRESH spool -- one spool per
// page, the rule cursor.go states -- and signs the cursor that names it.
//
// served is how many records of the whole ranked answer the pages up to and
// including this one have handed back, so the continuation's RankOffset is
// always zero: the new spool BEGINS at the next record.
func (e *Engine) nextRankedCursor(ctx context.Context, b *budget, queryHash string,
	total, served int64, tail func(func([]byte) error) error) (string, error) {
	if e.signer == nil || e.leases == nil || e.spools == nil {
		// No continuation machinery: the answer stops with this page and says
		// so, exactly as a walk that cannot spill its frontier does.
		return "", nil
	}
	binding := e.adjacency.Binding()
	lease, err := e.leases.Acquire(ctx, binding.GenerationID, binding.SnapshotID, model.LeaseCursor)
	if err != nil {
		return "", err
	}
	next := traversalCursor{
		Version: traversalCursorVersion, Endpoint: impactEndpoint,
		GenerationID: binding.GenerationID, AnalysisKey: binding.AnalysisKey,
		QueryHash: queryHash, LeaseID: lease.ID, Ranked: true,
		RankServed: served, RankTotal: total,
		Visited: b.visited, Edges: b.edges,
		ExpiresAt: e.now().Add(e.limits.CursorTTL).UTC().Truncate(time.Second),
	}
	id, err := e.spillRanked(next, total, tail)
	if err != nil {
		return "", e.releaseLease(ctx, lease.ID, err)
	}
	next.SpoolID = id
	if err := next.validate(); err != nil {
		e.releaseConsumed(ctx, id, "")
		return "", e.releaseLease(ctx, lease.ID, err)
	}
	payload, err := json.Marshal(next)
	if err != nil {
		e.releaseConsumed(ctx, id, "")
		return "", e.releaseLease(ctx, lease.ID,
			&model.Error{Code: model.CodeInternal, Message: "cursor encoding: " + err.Error()})
	}
	token, err := e.signer.Sign(pagination.PurposeCursor, payload, next.ExpiresAt)
	if err != nil {
		e.releaseConsumed(ctx, id, "")
		return "", e.releaseLease(ctx, lease.ID, err)
	}
	return token, nil
}

// spillRanked writes the ranked header and then every record tail streams, one
// at a time, so the remainder of the answer is never in heap.
func (e *Engine) spillRanked(next traversalCursor, total int64, tail func(func([]byte) error) error) (string, error) {
	sp, err := e.spools.Create(next.spoolCursor())
	if err != nil {
		return "", err
	}
	header, err := encodeRankedHeader(total)
	if err != nil {
		return "", e.releaseSpool(sp, err)
	}
	if err := sp.Append(header); err != nil {
		return "", e.releaseSpool(sp, err)
	}
	if err := tail(sp.Append); err != nil {
		return "", e.releaseSpool(sp, err)
	}
	if err := sp.Close(); err != nil {
		return "", e.releaseSpool(sp, err)
	}
	return sp.ID(), nil
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

// impactAccumulator collects one walk. Each node is admitted once -- which is
// what keeps a cycle (a -> b -> c -> a) from producing two entries for the same
// symbol -- while every later edge that reaches an already-admitted node still
// contributes its reason.
//
// It is also where MaxVisited and MaxEdges are enforced: expand deliberately
// does not know those caps, because only the caller can compare the cumulative,
// cursor-carried spend in budget against the bounds the request resolved.
type impactAccumulator struct {
	seeds  []model.NodeID
	isSeed map[model.NodeID]bool
	// emit streams one record per ADMITTED EDGE straight into the ranking
	// sort. There is no byNode map and no order slice any more: ruling P2 runs
	// the walk to completion, so both would have been sized by the reachable
	// set rather than by the page, and the per-node merge they used to do is
	// foldImpact's job inside pass 1 of the sort.
	emit       func(impactRecord) error
	edges      []model.Relation
	budget     *budget
	maxVisited config.Limit
	maxEdges   config.Limit
	// pageItems is the page's item bound, and ZERO turns it off. It is off for
	// an impact walk under ruling P2: that walk runs to completion and the
	// SERVED page is cut from the ranked answer afterwards, so stopping the
	// expansion at the page's item count would end the walk at the first page
	// and rank a fraction of the blast radius as though it were all of it. The
	// package rollup still sets it, and for it the bound still makes edges
	// page-sized.
	pageItems int
	reason    string
	// lastOwner and lastKey are the keyset position the continuation resumes
	// from: the frontier node whose chunk the last admitted row came from, and
	// that row's relation id.
	lastOwner model.NodeID
	lastKey   model.RelationID
}

func newImpactAccumulator(start []model.NodeID, b *budget, maxVisited, maxEdges config.Limit,
	pageItems int, emit func(impactRecord) error) *impactAccumulator {
	a := &impactAccumulator{
		isSeed:     make(map[model.NodeID]bool, len(start)),
		emit:       emit,
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
	case a.pageItems > 0 && len(a.edges) >= a.pageItems:
		// The page item bound, exactly as a traversal applies it
		// (traverse.go's `len(relations) >= maxItems`): it ends THIS page, and
		// the caller mints the continuation the next one resumes from. It is
		// also the bound that keeps edges, order and byNode page-sized.
		a.reason = reasonPageFull
		return errStopExpansion
	}
	// An admitted edge clears the last stop reason. Under ruling P2 one impact
	// answer chains several expand calls, and each internal boundary that ends
	// on a per-page work budget sets a reason the NEXT link then works past; a
	// reason that survived the link that overtook it would report a truncation
	// that did not happen. What is left at the end is the reason of the last
	// link, which is the only one that stopped the answer.
	a.reason = ""
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
	// One record per admitted edge, not per node. Two edges reaching the same
	// node produce two records with the same NodeID, and pass 1 of the sort
	// folds them with foldImpact -- which reproduces exactly what the old
	// per-node merge did (cheapest route wins, an incoming direction is sticky,
	// the reasons are the deduplicated union), without a map whose size was the
	// reachable set.
	if a.emit == nil {
		// The package rollup rides on the same walk but reads only the edges;
		// it has no record sort to stream into.
		return nil
	}
	return a.emit(impactRecord{
		NodeID:    reached,
		Depth:     depth,
		Cost:      state.Cost + Cost(rel.Kind),
		Direction: direction,
		Via:       rel.ID,
		Parent:    state.Node,
		Route:     appendRoute(state.Route, rel.ID),
		Reasons:   []string{impactReason(rel.Kind, direction, depth)},
	})
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
	// The page arrives ALREADY ordered, by ruling P1's (ScoreMicros desc,
	// Depth asc, NodeID asc), which the ranking pass applied over the whole
	// answer. The Name tie-break that used to be re-applied here is gone with
	// that ruling: Name is known only after hydration, so re-sorting a page by
	// it would reorder one page of a globally ordered answer and no two pages
	// would agree on where a record belongs.
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
