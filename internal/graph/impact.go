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
// resolved, the generation's capability rows and this page of the globally
// ranked package rollup over every edge the walk admitted.
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
	deadline time.Time) (answer impactAnswer, _ []model.ImpactEntry, _ *budget, _ string, err error) {
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
		// Released only on a TERMINAL outcome. A RETRYABLE failure inside the
		// walk -- CTX_WORKSPACE_BUSY from a contended store, a transient read
		// error -- tells the caller to present this same cursor again, and
		// releasing here destroyed the state that retry needs: the next
		// request answered CTX_CURSOR_INVALID and every page behind it was
		// lost. The state stays adoptable and expires with its own lease TTL.
		defer func() {
			if terminalOutcome(err) {
				resume.Release()
			}
		}()
		// The resumed budget carries the earlier pages' cumulative spend by
		// ASSIGNMENT, so replaying one cursor twice neither resets nor doubles it.
		b = resume.Budget
	}
	maxVisited, visitedNotice := resolveLimit("max_visited", req.MaxVisited, e.limits.Visited())
	maxEdges, edgeNotice := resolveLimit("max_edges", req.MaxEdges, e.limits.Edges())
	answer.Notices = appendNotice(appendNotice(answer.Notices, visitedNotice), edgeNotice)

	// The pass-1 INPUT of this answer is RETAINED across requests: ruling P3
	// lets the deadline split one walk over several of them, and a sort built
	// inside one request can only rank that request's leg -- the carried
	// visited set guarantees the earlier legs are never admitted again, so
	// their entities would be lost with no disclosure at all (walkretain.go).
	// Every admitted record is appended to it, and both ranking passes run over
	// the WHOLE retained input once the walk is exhausted.
	retain := resumeRetained(resume)
	if retain == nil {
		if retain, err = openRetainedWalk(e.walkScratchDir(), e.limits.FrontierBytes/visitedFilterBudgetShare); err != nil {
			return answer, nil, nil, "", err
		}
	}
	// Discarded AFTER the continuation below has taken it: a leg that mints a
	// walk cursor detaches the directory, and this then finds nothing to remove.
	defer retain.discard()

	var (
		acc   *impactAccumulator
		state walkState
	)
	if resume == nil || !resume.Cursor.WalkDone {
		acc = newImpactAccumulator(req.Start, b, maxVisited, maxEdges, retain.addEntry)
		// ONE streamed pass, TEED: the rollup's batching drives the walk, so
		// every admitted edge reaches the entity record and the package pair as
		// it is read and nothing is held between the edge that produced it and
		// the retained input it lands in. The rollup is nested inside the walk
		// rather than run again afterwards because the walk may be replayed only
		// by re-reading the whole graph.
		walkErr := e.rollupInto(ctx, &meta, func(sink edgeSink) error {
			var werr error
			// The walk is bounded by the frontier byte ceiling and the per-page
			// work budgets, which runWalkToCompletion returns at every internal
			// boundary, and it ends only when the frontier is empty, the depth
			// bound is reached or the deadline passes.
			state, werr = e.runWalkToCompletion(ctx, acc.Seeds(), expandOptions{
				Direction:     req.Direction,
				Kinds:         kinds,
				MaxDepth:      maxDepth,
				Budget:        b,
				BatchSize:     adjacencyBatch,
				FrontierBytes: e.limits.FrontierBytes,
				// Ruling P3, both halves: the deadline ends this page, and it
				// does so even before the page admitted an edge, because the
				// walk's frontier and every record it has admitted are
				// retained across the request.
				DeadlineStops:            true,
				DeadlineResumesEmptyPage: true,
				Resume:                   resume,
				// The walk's cumulative admitted-node set, append-only and
				// persistent: every internal link adds its own admissions to it
				// directly, so the continuation carries them and the next page
				// never re-admits a node an earlier link reported.
				Visited: retain.visited,
			}, func(fs frontierState, rel model.Relation) error {
				// The accumulator FIRST: it is what refuses an edge the work
				// budgets have no room for, and an edge it refused was never
				// admitted, so the rollup must not count it. Every edge it
				// accepts reaches the rollup, including one that reaches a seed
				// -- a seed is not an affected entity but the edge to it is
				// still a package-level dependency the walk read.
				if err := acc.Visit(fs, rel); err != nil {
					return err
				}
				return sink.Visit(fs, rel)
			})
			return werr
		}, retain.addPair, e.rollupProbe())
		if err := impactPhaseError(ctx, walkErr, &meta); err != nil {
			return answer, nil, nil, "", err
		}
	}
	if b.deadlineHit && len(state.Frontier) > 0 {
		// Ruling P3: the deadline ended this PAGE, not the answer. Nothing is
		// ranked and nothing is served -- ranking a walk that is still running
		// would publish an order the next page contradicts -- and the walk
		// continuation carries the frontier AND the records this leg admitted
		// forward, so the request that finishes the walk ranks all of them.
		markTruncated(&meta, reasonDeadline)
		next, err := e.continueWalk(ctx, b, impactEndpoint, queryHash, state,
			acc.lastOwner, acc.lastKey, resume, retain)
		if err != nil {
			return answer, nil, nil, "", err
		}
		answer = impactAnswer{Truncated: true, Reason: meta.TruncationReason,
			Notices: answer.Notices, Completeness: meta.Completeness}
		return answer, nil, b, next, nil
	}
	switch {
	case acc != nil && acc.reason != "":
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

	// The walk is exhausted. Both passes now run over every record EVERY leg of
	// it appended, which is what makes the served order the single unbounded
	// walk's order however many requests the walk was spread over.
	ranked, rankErr := e.rankImpact(ctx, retain, e.rankProbe())
	if ranked != nil {
		defer ranked.Close()
	}
	if err := impactPhaseError(ctx, rankErr, &meta); err != nil {
		return answer, nil, nil, "", err
	}
	var pairs *pagination.SortedRun[pairRecord]
	if ranked != nil {
		var pairErr error
		pairs, pairErr = e.rankPairs(ctx, retain.eachPair, e.pairProbe())
		if pairs != nil {
			defer pairs.Close()
		}
		if err := impactPhaseError(ctx, pairErr, &meta); err != nil {
			return answer, nil, nil, "", err
		}
	}
	if ranked == nil || pairs == nil {
		// Ruling P7: the deadline landed mid-RANK, after the walk had finished.
		// Nothing extra is persisted -- the input both passes read is already
		// retained -- so the continuation names it with the walk marked
		// complete and the next request re-sorts from it and serves page 1.
		markTruncated(&meta, reasonDeadline)
		next, err := e.continueRank(ctx, b, impactEndpoint, queryHash, retain)
		if err != nil {
			return answer, nil, nil, "", err
		}
		answer = impactAnswer{Truncated: true, Reason: meta.TruncationReason,
			Notices: answer.Notices, Completeness: meta.Completeness}
		return answer, nil, b, next, nil
	}
	entries, nextCursor, err := e.serveRankedRun(ctx, ranked, pairs, queryHash, b, limit, &answer, &meta)
	if err != nil {
		return answer, nil, nil, "", err
	}
	if err := impactPhaseError(ctx, e.attachImpactEvidence(ctx, entries), &meta); err != nil {
		return answer, nil, nil, "", err
	}
	answer.Truncated, answer.Reason = meta.Truncated, meta.TruncationReason
	answer.Completeness = meta.Completeness
	return answer, entries, b, nextCursor, nil
}

// serveRankedRun serves the FIRST page off the two freshly ranked runs -- the
// affected entities and the package pairs the same walk produced -- and spools
// everything after it behind one ranked continuation.
//
// Both lists are cut to the SAME page bound and paged together: an impact
// result has room for one cursor, and a page that served the whole rollup
// beside a cut entity list would overrun the record bound the result validates
// itself against.
func (e *Engine) serveRankedRun(ctx context.Context, run *pagination.SortedRun[impactRecord],
	pairRun *pagination.SortedRun[pairRecord], queryHash string, b *budget, limit int,
	answer *impactAnswer, meta *model.QueryMeta) ([]model.ImpactEntry, string, error) {
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
	pairPage := make([]model.PackageEdge, 0, limit)
	if err := pairRun.Each(func(r pairRecord) error {
		if err := ctx.Err(); err != nil {
			return typedContextError(ctx, err)
		}
		pairPage = append(pairPage, r.edge())
		if len(pairPage) == limit {
			return errStopExpansion
		}
		return nil
	}); err != nil && !errors.Is(err, errStopExpansion) {
		return nil, "", err
	}
	answer.Packages = pairPage
	entries, err := e.impactPage(ctx, page, answer, meta)
	if err != nil {
		return nil, "", err
	}
	if int64(len(page)) >= run.Len() && int64(len(pairPage)) >= pairRun.Len() {
		return entries, "", nil
	}
	header := rankedHeader{
		Total: run.Len(), Count: int(run.Len()) - len(page),
		PairTotal: pairRun.Len(), PairCount: int(pairRun.Len()) - len(pairPage),
	}
	next, err := e.nextRankedCursor(ctx, b, impactEndpoint, queryHash, header,
		int64(len(page)), int64(len(pairPage)), meta,
		chainTails(rankedRunTail(ctx, run, len(page), encodeImpactRecord),
			rankedRunTail(ctx, pairRun, len(pairPage), encodePairRecord)))
	return entries, next, err
}

// serveRankedImpact serves a LATER page: it reads the ranked spool the previous
// page left, copies what follows this page into a fresh one, and reports the
// answer-level facts the first page settled.
func (e *Engine) serveRankedImpact(ctx context.Context, c traversalCursor, b *budget, limit int,
	answer impactAnswer, meta model.QueryMeta) (_ impactAnswer, _ []model.ImpactEntry, _ *budget, _ string, err error) {
	// The consumed spool is released only after the page is built (the copy
	// below reads it) and only on a TERMINAL outcome: a retryable failure --
	// a busy store, a transient read -- leaves this cursor adoptable so the
	// caller can present it again instead of losing the ranked remainder.
	defer func() {
		if terminalOutcome(err) {
			e.releaseConsumed(context.WithoutCancel(ctx), c.SpoolID, c.LeaseID)
		}
	}()
	page, pairPage, h, err := serveRankedSections(ctx, e.spools, c.spoolCursor(), e.now(), limit)
	if err != nil {
		return answer, nil, nil, "", err
	}
	answer.Packages = make([]model.PackageEdge, 0, len(pairPage))
	for _, r := range pairPage {
		answer.Packages = append(answer.Packages, r.edge())
	}
	entries, err := e.impactPage(ctx, page, &answer, &meta)
	if err != nil {
		return answer, nil, nil, "", err
	}
	if err := impactPhaseError(ctx, e.attachImpactEvidence(ctx, entries), &meta); err != nil {
		return answer, nil, nil, "", err
	}
	var next string
	// Served counts records CONSUMED FROM THE SPOOL rather than entries handed
	// back, so a hydration drop cannot end the answer a record early.
	served, pairServed := c.RankServed+int64(len(page)), c.PairServed+int64(len(pairPage))
	if served < h.Total || pairServed < h.PairTotal {
		header := rankedHeader{
			Total: h.Total, Count: h.Count - len(page),
			PairTotal: h.PairTotal, PairCount: h.PairCount - len(pairPage),
		}
		next, err = e.nextRankedCursor(ctx, b, c.Endpoint, c.QueryHash, header, served, pairServed, &meta,
			rankedSpoolSections(ctx, e.spools, c.spoolCursor(), e.now(), h, len(page), len(pairPage)))
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
func (e *Engine) nextRankedCursor(ctx context.Context, b *budget, endpoint, queryHash string,
	header rankedHeader, served, pairServed int64, meta *model.QueryMeta,
	tail func(func([]byte) error) error) (string, error) {
	if e.signer == nil || e.leases == nil || e.spools == nil {
		// No continuation machinery: the answer stops with this page and SAYS
		// so, exactly as a walk that cannot spill its frontier does. It is only
		// reached with a ranked remainder still unserved -- both callers check
		// that first -- so the stop always cuts the answer, and returning an
		// empty token alone left the caller reading a complete-looking page of
		// a longer ranking. Marking it is what makes the cut visible.
		markTruncated(meta, reasonNoContinuation)
		return "", nil
	}
	binding := e.adjacency.Binding()
	lease, err := e.leases.Acquire(ctx, binding.GenerationID, binding.SnapshotID, model.LeaseCursor)
	if err != nil {
		return "", err
	}
	next := traversalCursor{
		Version: traversalCursorVersion, Endpoint: endpoint,
		GenerationID: binding.GenerationID, AnalysisKey: binding.AnalysisKey,
		QueryHash: queryHash, LeaseID: lease.ID, Ranked: true,
		RankServed: served, RankTotal: header.Total,
		PairServed: pairServed, PairTotal: header.PairTotal,
		Visited: b.visited, Edges: b.edges,
		ExpiresAt: e.now().Add(e.limits.CursorTTL).UTC().Truncate(time.Second),
	}
	id, err := e.spillRanked(next, header, tail)
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
func (e *Engine) spillRanked(next traversalCursor, h rankedHeader, tail func(func([]byte) error) error) (string, error) {
	sp, err := e.spools.Create(next.spoolCursor())
	if err != nil {
		return "", err
	}
	header, err := encodeRankedHeader(h)
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
//
// A resumable frontier is NOT enough on these two endpoints, and that is what
// retain carries. Their answers are globally RANKED, so the request that
// finishes the walk must rank every record every leg admitted, not just its
// own: the cumulative visited set this cursor carries guarantees the earlier
// legs' nodes are never admitted a second time, so a continuation that named
// only a frontier would serve the last leg's ranking as the whole blast radius
// and disclose nothing. retain is the retained pass-1 input the store adopts
// alongside the frontier spool (walkretain.go); the token names both, and the
// leg that exhausts the walk ranks the whole of it.
func (e *Engine) continueWalk(ctx context.Context, b *budget, endpoint, queryHash string,
	state walkState, lastOwner model.NodeID, lastKey model.RelationID,
	resume *resumeState, retain *retainedWalk) (string, error) {
	if len(state.Frontier) == 0 || state.DepthLimited {
		return "", nil
	}
	// The cumulative admitted set is NOT materialized here and NOT copied
	// forward: only the nodes THIS page admitted are, appended to the retained
	// run store as one more ascending run (visitedstore.go). Everything earlier
	// stays exactly where the page that admitted it wrote it.
	return e.nextTraversalCursor(ctx, b, continuation{
		Endpoint:  endpoint,
		QueryHash: queryHash,
		Depth:     state.Depth,
		LastOwner: lastOwner,
		LastKey:   lastKey,
		Frontier:  state.Frontier,
		Visited:   state.Admitted.addedNodes(),
		Retain:    retain,
	})
}

// continueRank mints ruling P7's continuation: the walk is EXHAUSTED and the
// deadline cut the ranking short, so the token names the retained pass-1 input
// alone -- no frontier, no keyset position -- and the next request runs both
// passes over it and serves page 1. Nothing extra is persisted: the input the
// sort would re-read is what every leg has been appending to all along.
func (e *Engine) continueRank(ctx context.Context, b *budget, endpoint, queryHash string,
	retain *retainedWalk) (string, error) {
	return e.nextTraversalCursor(ctx, b, continuation{
		Endpoint:  endpoint,
		QueryHash: queryHash,
		Retain:    retain,
		WalkDone:  true,
	})
}

// resumeRetained is the pass-1 input a continuation arrived with, or nil when
// this is the request that mints the answer.
func resumeRetained(r *resumeState) *retainedWalk {
	if r == nil {
		return nil
	}
	return r.Retain
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

// reasonNoContinuation is the truncation reason for a ranked answer whose
// remainder cannot be paged because this engine was built without the
// continuation machinery -- no signer, no lease store or no spool store. The
// records exist and the ranking is complete; what is missing is the token that
// would hand the rest of them back, so the page that is served is a prefix and
// must say it is one.
const reasonNoContinuation = "continuation state is unavailable in this workspace"

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
	budget     *budget
	maxVisited config.Limit
	maxEdges   config.Limit
	reason     string
	// lastOwner and lastKey are the keyset position the continuation resumes
	// from: the frontier node whose chunk the last admitted row came from, and
	// that row's relation id.
	lastOwner model.NodeID
	lastKey   model.RelationID
}

func newImpactAccumulator(start []model.NodeID, b *budget, maxVisited, maxEdges config.Limit,
	emit func(impactRecord) error) *impactAccumulator {
	a := &impactAccumulator{
		isSeed:     make(map[model.NodeID]bool, len(start)),
		emit:       emit,
		budget:     b,
		maxVisited: maxVisited,
		maxEdges:   maxEdges,
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
	// The CUMULATIVE counters, not the per-page ones. Under ruling P2 the walk
	// runs to completion inside one request, so a bound compared against a
	// counter that runWalkToCompletion resets at every internal boundary could
	// never be reached: a user-set max_edges would be silently unenforceable
	// and the answer would exceed the allowance the caller asked for. These two
	// are ANSWER-level bounds -- the only stops here that are -- so exceeding
	// one truncates the answer and is reported, exactly as §20.1 requires of an
	// explicit user limit.
	case a.maxEdges.Exceeded(a.budget.edges + 1):
		a.reason = reasonEdgeBudget
		return errStopExpansion
	case a.maxVisited.Exceeded(a.budget.visited + 1):
		a.reason = reasonVisitedBudget
		return errStopExpansion
	}
	// An admitted edge clears the last stop reason. A stop set by one internal
	// link of the chain and then worked past by the next would report a
	// truncation that did not happen; what survives is the reason of the link
	// that actually ended the answer.
	a.reason = ""
	// The keyset position a continuation resumes from.
	a.lastOwner, a.lastKey = state.Node, rel.ID
	reached := otherEndpoint(state.Node, rel)
	if a.isSeed[reached] {
		// A seed is the thing being changed, not something the change affects.
		// The edge itself is still admitted -- the package rollup teed off this
		// visitor counts it -- so this returns nil rather than a stop.
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
