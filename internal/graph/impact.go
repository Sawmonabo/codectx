package graph

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
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

	answer, served, b, nextCursor, err := e.walkImpact(ctx, req, kinds, maxDepth, limit, queryHash, deadline)
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
		Entries:  served.Entries,
		Packages: answer.Packages,
		Reasons:  served.Reasons,
		Routes:   served.Routes,
		Answer:   served.Answer,
		// Cumulative spend, the same accounting the traversal operations
		// report: a resumed page carries what the earlier pages already spent
		// and adds its own, so the counters GROW across the pages of one walk
		// rather than repeating a single-shot total.
		VisitedCount: b.visited,
		EdgeCount:    b.edges,
		// Whole-walk counts, carried by every continuation.
		OpenSitesByDepth: b.openSites,
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
// walk: the request that MINTS the answer runs the expansion to
// completion, streams every admitted edge into a disk-backed sort, ranks the
// whole affected set once, serves the first page and spools the globally
// ranked remainder behind a cursor. Later pages read that spool and walk
// nothing.
//
// What that buys, and what the page-bounded predecessor could not give: the
// union of the pages is exactly the single-shot answer, in exactly the
// single-shot ORDER, with every node reported once: a node reached again from
// a later frontier is one record that pass 1 folds, not a second listing
// carrying that page's reasons.
//
// Peak heap is bounded by three things and none of them is the answer:
// one internal page's frontier (Limits.FrontierBytes), the sort's run buffer
// (a quarter of the query memory admission) plus its merge fan-in, and one
// served page. None of the three is a function of the reachable set.
//
// The two ways this still ends early are both pages, never answers: the query
// deadline mints the `f` walk continuation with no entries at all
// and the next request carries the walk on, and the user-set depth bound is
// reported as truncation with the ranked answer it did reach.
func (e *Engine) walkImpact(ctx context.Context, req model.ImpactRequest, kinds []model.RelationKind,
	maxDepth config.Limit, limit int, queryHash string,
	deadline time.Time) (answer impactAnswer, _ impactServed, _ *budget, _ string, err error) {
	meta := model.QueryMeta{}
	caps, err := e.completeness(ctx)
	if err != nil {
		return answer, impactServed{}, nil, "", err
	}
	meta.Completeness = caps

	b := &budget{deadline: deadline, now: e.now}
	var resume *resumeState
	// stalled is set only by the walkStalled path below. That page ends the
	// answer TERMINALLY with no cursor of its own, so the cursor the caller is
	// still holding is the whole remedy the reason offers -- and a fresh
	// request gets a fresh deadline (beginImpactQuery), so a warm read really
	// can advance where this one could not. Releasing the state on that path
	// would destroy it and make the documented remedy cost the walk from its
	// seed.
	// It is an explicit flag rather than "no cursor was minted", because the
	// exhausted, fully served answer mints no cursor either and that state is
	// genuinely finished; same shape as serveRankedImpact's complete.
	stalled := false
	if req.Page.Cursor != "" {
		c, rb, err := e.verifyContinuation(req.Page.Cursor, impactEndpoint, queryHash, deadline)
		if err != nil {
			return answer, impactServed{}, nil, "", err
		}
		if c.Ranked {
			// The walk is over and the order is settled: this page is a read of
			// the ranked spool and expands nothing.
			return e.serveRankedImpact(ctx, c, rb, limit, req.View.Normalized(), answer, meta)
		}
		resume, err = e.resumeTraversal(ctx, req.Page.Cursor, impactEndpoint, queryHash, deadline)
		if err != nil {
			return answer, impactServed{}, nil, "", err
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
			if terminalOutcome(err) && !stalled {
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

	// The pass-1 INPUT of this answer is RETAINED across requests: the
	// deadline may split one walk over several of them, and a sort built
	// inside one request can only rank that request's leg -- the carried
	// visited set guarantees the earlier legs are never admitted again, so
	// their entities would be lost with no disclosure at all (walkretain.go).
	// Every admitted record is appended to it, and both ranking passes run over
	// the WHOLE retained input once the walk is exhausted.
	retain := resumeRetained(resume)
	if retain == nil {
		if retain, err = e.openWalkState(); err != nil {
			return answer, impactServed{}, nil, "", err
		}
	}
	// Every state the walk admits names its route by reference into the
	// walk's route table, which the answer serves its routes from.
	retain.withRoutes()
	// Discarded AFTER the continuation below has taken it: a leg that mints a
	// walk cursor detaches the directory, and this then finds nothing to remove.
	defer retain.discard()

	var (
		acc   *impactAccumulator
		state walkState
	)
	if resume == nil || !resume.Cursor.WalkDone {
		reader, err := e.Reader()
		if err != nil {
			return answer, impactServed{}, nil, "", err
		}
		names := &levelNames{}
		// Every affected node is also counted, per depth, by the open call
		// sites it is a candidate of: possibly affected through a call no
		// analysis bound, counted and not walked.
		sites := newOpenSiteCounter(declarationReader(reader), b, e.scratchDir())
		defer sites.close()
		emit := func(rec impactRecord) error {
			if err := sites.note(ctx, rec); err != nil {
				return err
			}
			return retain.addEntry(rec)
		}
		acc = newImpactAccumulator(req.Start, b, maxVisited, maxEdges, emit,
			names, kindNames(reader))
		// Impact takes a call walk's dispatch step: a change to a method
		// affects the callers of every method it overrides whose call may
		// run it, and forward impact descends to the overrides a virtual
		// call may run. The base a callers step climbs through is a route,
		// not an affected entity.
		mode := dispatchFor(req.Direction, kinds)
		dispatches := e.dispatchReader()
		if dispatches == nil {
			mode = dispatchNone
		}
		acc.climbs = mode == dispatchCallers
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
				FrontierBytes: e.limits.FrontierBytes,
				// Both halves of the page rule: the deadline ends this page, and it
				// does so even before the page admitted an edge, because the
				// walk's frontier and every record it has admitted are
				// retained across the request.
				DeadlineStops:            true,
				DeadlineResumesEmptyPage: true,
				Resume:                   resume,
				// The walk's retained state: the frontier it commits level by
				// level, the cumulative admitted-node bitset and this answer's
				// pass-1 input, all in one directory the continuation carries
				// forward by rename.
				Retain: retain,
				// The per-level canonical resolution the accumulator reads. One
				// pointer, refilled by the walk before each level's edges are
				// delivered, so a record is named without a round trip per edge.
				Names:      names,
				Dispatch:   mode,
				Dispatches: dispatches,
				// The forward slice's orientation and its ranking rule
				// (impactdependence.go).
				Dependence: true,
				Flows:      e.flowReader(),
			}, func(fs frontierState, edge Edge) error {
				// The accumulator FIRST: it is what refuses an edge the work
				// budgets have no room for, and an edge it refused was never
				// admitted, so the rollup must not count it. Every edge it
				// accepts reaches the rollup, including one that reaches a seed
				// -- a seed is not an affected entity but the edge to it is
				// still a package-level dependency the walk read.
				if err := acc.Visit(fs, edge); err != nil {
					return err
				}
				return sink.Visit(fs, edge)
			})
			return werr
		}, retain.addPair, e.rollupProbe())
		if err := impactPhaseError(ctx, walkErr, &meta); err != nil {
			return answer, impactServed{}, nil, "", err
		}
		// Counted before any continuation is minted, so the token carries
		// this leg's last level too. The count reads the store and is
		// delivery, not walking, so it runs past the deadline.
		if err := sites.flush(context.WithoutCancel(ctx)); err != nil {
			return answer, impactServed{}, nil, "", err
		}
	}
	if b.deadlineHit && state.More {
		// The deadline ended this PAGE, not the answer. Nothing is
		// ranked and nothing is served -- ranking a walk that is still running
		// would publish an order the next page contradicts -- and the walk
		// continuation carries the frontier AND the records this leg admitted
		// forward, so the request that finishes the walk ranks all of them.
		if walkStalled(b, state, resume) {
			// No continuation: it would be the one this request was given.
			// That cursor stays ADOPTABLE for the rest of its lease, which is
			// what makes the reason's remedy -- present it again under a
			// longer resources.query_timeout -- resume the walk instead of
			// restarting it from the seed.
			stalled = true
			markTruncated(&meta, reasonDeadlineStalled)
			answer = impactAnswer{Truncated: true, Reason: meta.TruncationReason,
				Notices: answer.Notices, Completeness: meta.Completeness}
			return answer, impactServed{}, b, "", nil
		}
		markTruncated(&meta, reasonDeadline)
		next, err := e.continueWalk(ctx, b, impactEndpoint, queryHash, state, retain)
		if err != nil {
			return answer, impactServed{}, nil, "", err
		}
		answer = impactAnswer{Truncated: true, Reason: meta.TruncationReason,
			Notices: answer.Notices, Completeness: meta.Completeness}
		return answer, impactServed{}, b, next, nil
	}
	switch {
	case acc != nil && acc.reason != "":
		// The LAST link's stop, and so the one that ended the answer: an
		// internal boundary's reason is cleared by the first edge the next link
		// admits (impactAccumulator.Visit).
		markTruncated(&meta, acc.reason)
	case state.DepthLimited:
		// Nodes at the depth bound were admitted but never expanded; an impact
		// answer that stopped there is not the whole blast radius.
		markTruncated(&meta, reasonDepth)
	}

	// A request that names `may_call` walks on through the open call sites:
	// once the precise walk is exhausted, the possible callers (or callees)
	// of every affected node join the retained input before it is ranked,
	// and so on from them up to the depth bound. It runs once, in the request
	// that finished the walk, so a mid-rank continuation ranks it with the
	// rest.
	if acc != nil && slices.Contains(kinds, model.RelMayCall) {
		reader, err := e.Reader()
		if err != nil {
			return answer, impactServed{}, nil, "", err
		}
		if err := e.expandOpenSites(ctx, reader, req, maxDepth, retain); err != nil {
			if err := impactPhaseError(ctx, err, &meta); err != nil {
				return answer, impactServed{}, nil, "", err
			}
		}
	}

	// The walk is exhausted. Both passes now run over every record EVERY leg of
	// it appended, which is what makes the served order the single unbounded
	// walk's order however many requests the walk was spread over.
	ranked, rankErr := e.rankImpact(ctx, retain, e.rankProbe())
	if ranked != nil {
		defer ranked.Close()
	}
	if err := impactPhaseError(ctx, rankErr, &meta); err != nil {
		return answer, impactServed{}, nil, "", err
	}
	var pairs *pagination.SortedRun[pairRecord]
	if ranked != nil {
		var pairErr error
		pairs, pairErr = e.rankPairs(ctx, retain.eachPair, e.pairProbe())
		if pairs != nil {
			defer pairs.Close()
		}
		if err := impactPhaseError(ctx, pairErr, &meta); err != nil {
			return answer, impactServed{}, nil, "", err
		}
	}
	if ranked == nil || pairs == nil {
		// The deadline landed mid-RANK, after the walk had finished. The input
		// both passes read is already retained, and rankImpact has detached an
		// interrupted pass's runs into the same directory, so the continuation
		// names it with the walk marked complete and the next request resumes
		// the ranking from it and serves page 1.
		markTruncated(&meta, reasonDeadline)
		next, err := e.continueRank(ctx, b, impactEndpoint, queryHash, retain)
		if err != nil {
			return answer, impactServed{}, nil, "", err
		}
		answer = impactAnswer{Truncated: true, Reason: meta.TruncationReason,
			Notices: answer.Notices, Completeness: meta.Completeness}
		return answer, impactServed{}, b, next, nil
	}
	served, nextCursor, err := e.serveRankedRun(ctx, ranked, pairs, retain, req,
		queryHash, b, limit, &answer, &meta)
	if err != nil {
		return answer, impactServed{}, nil, "", err
	}
	answer.Truncated, answer.Reason = meta.Truncated, meta.TruncationReason
	answer.Completeness = meta.Completeness
	return answer, served, b, nextCursor, nil
}

// impactServed is one page of one view of an impact answer, and the token that
// names the whole answer.
type impactServed struct {
	Entries []model.ImpactEntry
	Reasons []model.ImpactReason
	Routes  []model.ImpactRoute
	Answer  string
}

// viewPage is where one page of a view stands in the ranked records: how many
// records it used up, and how many reasons of the next one it already served
// (the reasons view alone splits a record across pages).
type viewPage struct {
	consumed int
	skip     int64
}

// servePageOf cuts one page of view out of records, the ranked records from
// ordinal base on, the first of them with skip of its reasons already served.
// Entries and routes take one item per record; reasons take each record's
// reasons in order, up to limit.
func (e *Engine) servePageOf(ctx context.Context, view model.ImpactView, records []impactRecord,
	base, skip int64, limit int, routes *routeReader, answer *impactAnswer, meta *model.QueryMeta,
) (impactServed, viewPage, error) {
	var out impactServed
	switch view {
	case model.ImpactViewReasons:
		reasons, at := cutReasons(records, base, skip, limit)
		out.Reasons = reasons
		return out, at, nil
	case model.ImpactViewRoutes:
		var err error
		if out.Routes, err = routesOf(records, base, routes); err != nil {
			return impactServed{}, viewPage{}, err
		}
		if err := impactPhaseError(ctx, e.attachRouteEvidence(deliverCtx(ctx), out.Routes), meta); err != nil {
			return impactServed{}, viewPage{}, err
		}
		return out, viewPage{consumed: len(records)}, nil
	default:
		var err error
		if out.Entries, err = e.impactPage(ctx, records, base, answer, meta); err != nil {
			return impactServed{}, viewPage{}, err
		}
		return out, viewPage{consumed: len(records)}, nil
	}
}

// cutReasons takes up to limit reasons from records, in order, starting skip
// reasons into the first record, and reports where it stopped.
func cutReasons(records []impactRecord, base, skip int64, limit int) ([]model.ImpactReason, viewPage) {
	out := make([]model.ImpactReason, 0, min(limit, 64))
	for i, r := range records {
		from := int64(0)
		if i == 0 {
			from = skip
		}
		for j := from; j < int64(len(r.Reasons)); j++ {
			if len(out) == limit {
				return out, viewPage{consumed: i, skip: j}
			}
			out = append(out, model.ImpactReason{Entry: base + int64(i), Index: j, Reason: r.Reasons[j]})
		}
	}
	return out, viewPage{consumed: len(records)}
}

// routesOf rebuilds the route of every record from the route table, whole.
func routesOf(records []impactRecord, base int64, routes *routeReader) ([]model.ImpactRoute, error) {
	out := make([]model.ImpactRoute, 0, len(records))
	for i, r := range records {
		if r.Via == "" {
			continue
		}
		rels, err := routes.route(r.Adm, r.Via)
		if err != nil {
			return nil, err
		}
		out = append(out, model.ImpactRoute{Entry: base + int64(i), Relations: rels,
			CostUnits: plainCost(r.Cost), Possible: r.Possible})
	}
	return out, nil
}

// serveRankedRun serves the FIRST page of the requested view off the two
// freshly ranked runs -- the affected entities and the package pairs the same
// walk produced. An answer one page of each view it is read through holds is
// served whole in this response and nothing is retained (serveInline).
// Otherwise it spools the WHOLE ranked answer behind the page: every view
// reads the same spool, so the answer token this page mints reads any view
// from its start and the continuation reads on in this one.
//
// The entries view cuts both lists to the SAME page bound and pages them
// together: an impact result has room for one cursor, and a page that served
// the whole rollup beside a cut entity list would overrun the record bound the
// result validates itself against. The reasons and routes views serve no
// pairs.
//
// The page's routes are read from the walk's own route table; the table then
// moves beside the spool, where every later page reads them.
func (e *Engine) serveRankedRun(ctx context.Context, run *pagination.SortedRun[impactRecord],
	pairRun *pagination.SortedRun[pairRecord], retain *retainedWalk, req model.ImpactRequest, queryHash string,
	b *budget, limit int, answer *impactAnswer, meta *model.QueryMeta) (impactServed, string, error) {
	view := req.View.Normalized()
	// Delivery, for serveRankedImpact's reason: both runs are ranked and this
	// reads one page off them and spills the answer.
	ctx = deliverCtx(ctx)
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
		return impactServed{}, "", err
	}
	// An answer one page of every view it is read through holds is served
	// whole, here, and retains nothing.
	if served, inline, err := e.serveInline(ctx, run, pairRun, page, retain, req, limit, answer, meta); err != nil || inline {
		return served, "", err
	}
	pairPage := make([]model.PackageEdge, 0, limit)
	if view == model.ImpactViewEntries {
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
			return impactServed{}, "", err
		}
	}
	answer.Packages = pairPage
	routes, err := retain.routeReader()
	if err != nil {
		return impactServed{}, "", err
	}
	served, at, err := e.servePageOf(ctx, view, page, 0, 0, limit, routes, answer, meta)
	if cerr := routes.close(); err == nil && cerr != nil {
		err = internalErr("graph: the route table: " + cerr.Error())
	}
	if err != nil {
		return impactServed{}, "", err
	}
	pos := rankedPos{view: view, served: int64(at.consumed), skip: at.skip, pairServed: pairRun.Len()}
	if view == model.ImpactViewEntries {
		pos.pairServed = int64(len(pairPage))
	}
	done := pos.complete(run.Len(), pairRun.Len())
	// finished is every view no later request reads: the ones the client said
	// it will not read, and this one when this page served it whole.
	var finished []model.ImpactView
	for _, v := range model.ImpactViews {
		if !req.Reads(v) || (v == view && done) {
			finished = append(finished, v)
		}
	}
	if done && (run.Len() == 0 || len(finished) == len(model.ImpactViews)) {
		// Nothing left that any view will read: the page is the answer.
		return served, "", nil
	}
	header := rankedHeader{Total: run.Len(), Count: int(run.Len()), PairTotal: pairRun.Len(), PairCount: int(pairRun.Len())}
	next, answerToken, err := e.spillImpactAnswer(ctx, b, queryHash, header, pos, done, finished, retain, meta,
		rankedSection{tail: rankedRunTail(ctx, run, 0, encodeImpactRecord), mark: at.consumed},
		rankedSection{tail: rankedRunTail(ctx, pairRun, 0, encodePairRecord), mark: int(pos.pairServed)})
	served.Answer = answerToken
	return served, next, err
}

// rankedPos is where one view of a ranked answer stands: the records it has
// used up, the reasons of the next record it already served, and the pairs it
// has served (all of them, for a view that serves none).
type rankedPos struct {
	view       model.ImpactView
	served     int64
	skip       int64
	pairServed int64
}

// complete reports that the view has served the whole answer.
func (p rankedPos) complete(total, pairTotal int64) bool {
	return p.served >= total && p.skip == 0 && p.pairServed >= pairTotal
}

// rankedSection is one section of a ranked spool: the records it streams, and
// the record count after which the writer records its byte offset -- where the
// view that minted the spool resumes.
type rankedSection struct {
	tail func(func([]byte) error) error
	mark int
}

// spillImpactAnswer writes the WHOLE ranked answer into one spool, moves the
// route table beside it under the same lease, and signs the two tokens a page
// hands back: the answer token, which reads any view from its start, and --
// when this view has more to serve -- the continuation of this view.
func (e *Engine) spillImpactAnswer(ctx context.Context, b *budget, queryHash string, header rankedHeader,
	pos rankedPos, done bool, finished []model.ImpactView, routes *retainedWalk, meta *model.QueryMeta,
	sections ...rankedSection) (string, string, error) {
	if e.signer == nil || e.spools == nil {
		if !done {
			markTruncated(meta, reasonNoContinuation)
		}
		return "", "", nil
	}
	binding := e.adjacency.Binding()
	var leaseID string
	if e.leases.Retains() {
		lease, err := e.leases.Acquire(ctx, binding.GenerationID, binding.SnapshotID, model.LeaseCursor)
		if err != nil {
			return "", "", err
		}
		leaseID = lease.ID
	}
	c := traversalCursor{
		Version: traversalCursorVersion, Endpoint: impactEndpoint,
		GenerationID: binding.GenerationID, AnalysisKey: binding.AnalysisKey,
		QueryHash: queryHash, LeaseID: leaseID, Ranked: true,
		RankTotal: header.Total, PairTotal: header.PairTotal,
		Visited: b.visited, Edges: b.edges,
		ExpiresAt: e.now().Add(e.limits.CursorTTL).UTC().Truncate(time.Second),
	}
	id, starts, marks, err := e.spillRankedMarked(c, header, sections...)
	if err != nil {
		return "", "", e.releaseLease(ctx, leaseID, err)
	}
	c.SpoolID, c.RankStart, c.PairStart = id, starts[0], starts[1]
	if header.Total > 0 {
		if c.RouteID, err = e.adoptRoutes(c, routes, finished); err != nil {
			e.releaseConsumed(ctx, id, "")
			return "", "", e.releaseLease(ctx, leaseID, err)
		}
	}
	fail := func(err error) (string, string, error) {
		e.releaseConsumed(ctx, c.RouteID, "")
		e.releaseConsumed(ctx, id, "")
		return "", "", e.releaseLease(ctx, leaseID, err)
	}
	var answerToken string
	if header.Total > 0 {
		if answerToken, err = e.signRanked(c.answer()); err != nil {
			return fail(err)
		}
	}
	if done {
		if header.Total == 0 {
			// Only the rollup was spilled, and this page served all of it:
			// no other view has anything to read.
			e.releaseConsumed(ctx, id, leaseID)
		}
		return "", answerToken, nil
	}
	next := c.at(pos, marks[0], marks[1])
	token, err := e.signRanked(next)
	if err != nil {
		return fail(err)
	}
	return token, answerToken, nil
}

// spillRankedMarked writes the ranked header and then every record each
// section streams, one at a time, so the answer is never in heap. It reports
// the spool id, the byte offset each section begins at, and the byte offset
// after each section's mark records.
func (e *Engine) spillRankedMarked(next traversalCursor, h rankedHeader,
	sections ...rankedSection) (string, []int64, []int64, error) {
	sp, err := e.spools.Create(next.spoolCursor())
	if err != nil {
		return "", nil, nil, err
	}
	header, err := encodeRankedHeader(h)
	if err != nil {
		return "", nil, nil, e.releaseSpool(sp, err)
	}
	if err := sp.Append(header); err != nil {
		return "", nil, nil, e.releaseSpool(sp, err)
	}
	starts := make([]int64, len(sections))
	marks := make([]int64, len(sections))
	for i, s := range sections {
		starts[i], marks[i] = sp.Written(), -1
		count := 0
		if err := s.tail(func(rec []byte) error {
			if count == s.mark {
				marks[i] = sp.Written()
			}
			count++
			return sp.Append(rec)
		}); err != nil {
			return "", nil, nil, e.releaseSpool(sp, err)
		}
		if marks[i] < 0 {
			marks[i] = sp.Written()
		}
	}
	if err := sp.Close(); err != nil {
		return "", nil, nil, e.releaseSpool(sp, err)
	}
	e.recordSpoolWritten(sp.Written())
	return sp.ID(), starts, marks, nil
}

// serveRankedImpact serves a LATER page of one view: it seeks to the cursor's
// byte offsets in the spool the FIRST page wrote, reads at most one page, and
// reports the answer-level facts the first page settled. Nothing is copied
// forward, so the page costs its own page.
//
// The cursor is either the continuation of one view -- presented again with
// that view -- or the answer token, which starts whichever view the request
// names.
func (e *Engine) serveRankedImpact(ctx context.Context, c traversalCursor, b *budget, limit int,
	view model.ImpactView, answer impactAnswer, meta model.QueryMeta) (_ impactAnswer, _ impactServed, _ *budget, _ string, err error) {
	// The answer is released when EVERY view of it has been served whole,
	// not when one view ends: the others read the same spool. done is
	// explicit rather than "no cursor was minted", because a renewal, signing
	// or marshal failure also mints no cursor and must leave this cursor
	// adoptable so the caller can present it again.
	done := false
	// Serving a ranked page is DELIVERY, so it runs deadline-detached like
	// every other delivery step: the walk is over, the order is settled, and
	// what is left is one bounded read off a local spool plus the tokens that
	// name the rest.
	ctx = deliverCtx(ctx)
	pos, rankOff, pairOff, err := c.position(view)
	if err != nil {
		return answer, impactServed{}, nil, "", err
	}
	defer func() {
		if terminalOutcome(err) && done {
			e.finishView(ctx, c, view)
		}
	}()
	if e.spools == nil {
		return answer, impactServed{}, nil, "", cursorInvalid("continuation state has expired or was released")
	}
	h, _, read, err := rankedSpoolHeader(ctx, e.spools, c.spoolCursor(), e.now())
	if err != nil {
		return answer, impactServed{}, nil, "", err
	}
	rankLeft, pairLeft := c.RankTotal-pos.served, c.PairTotal-pos.pairServed
	if err := h.checkAgainst(rankLeft, pairLeft); err != nil {
		return answer, impactServed{}, nil, "", err
	}
	routes, err := e.openRankedRoutes(ctx, c)
	if err != nil {
		return answer, impactServed{}, nil, "", err
	}
	defer routes.close()
	page, pairPage, endRank, endPair, sectionBytes, err := serveRankedSections(ctx, e.spools,
		c.spoolCursor(), e.now(), rankOff, pairOff, pageWant(limit, rankLeft), pageWant(limit, pairLeft))
	if err != nil {
		return answer, impactServed{}, nil, "", err
	}
	e.recordSpoolRead(read + sectionBytes)
	answer.Packages = make([]model.PackageEdge, 0, len(pairPage))
	for _, r := range pairPage {
		answer.Packages = append(answer.Packages, r.edge())
	}
	served, at, err := e.servePageOf(ctx, view, page, pos.served, pos.skip, limit, routes, &answer, &meta)
	if err != nil {
		return answer, impactServed{}, nil, "", err
	}
	if at.consumed < len(page) {
		// The reasons view stopped inside a record: the next page resumes at
		// that record's first byte, which is where the records it used up end.
		if endRank, _, err = readRankedSection(ctx, e.spools, c.spoolCursor(), e.now(), rankOff,
			at.consumed, func([]byte) error { return nil }); err != nil {
			return answer, impactServed{}, nil, "", err
		}
	}
	// Served counts records CONSUMED FROM THE SPOOL rather than entries handed
	// back, so a hydration drop cannot end the answer a record early.
	next := rankedPos{view: view, served: pos.served + int64(at.consumed), skip: at.skip,
		pairServed: pos.pairServed + int64(len(pairPage))}
	done = next.complete(c.RankTotal, c.PairTotal)
	token, answerToken, err := e.continueImpactCursor(ctx, b, c, next, done, endRank, endPair, &meta)
	if err != nil {
		return answer, impactServed{}, nil, "", err
	}
	served.Answer = answerToken
	answer.Completeness = meta.Completeness
	answer.Truncated, answer.Reason = meta.Truncated, meta.TruncationReason
	return answer, served, b, token, nil
}

// continueImpactCursor renews the answer's lease and signs the answer token
// and, when the view has more to serve, its continuation. It writes nothing to
// the database beyond the renewal: a ranked page is O(page).
//
// A LEASED spool is renewed on every page, because its liveness is its lease's
// (pagination.Spools.live) and the answer is not over. A leaseless spool -- and
// a leased one presented to a process that writes nothing -- has no renewal:
// the tokens carry forward the expiry its header was stamped with.
func (e *Engine) continueImpactCursor(ctx context.Context, b *budget, c traversalCursor, pos rankedPos,
	done bool, rankOff, pairOff int64, meta *model.QueryMeta) (string, string, error) {
	if e.signer == nil {
		if !done {
			markTruncated(meta, reasonNoContinuation)
		}
		return "", "", nil
	}
	c.Version = traversalCursorVersion
	c.Visited, c.Edges = b.visited, b.edges
	if c.LeaseID != "" && e.leases.Retains() {
		if _, err := e.leases.Renew(ctx, c.LeaseID); err != nil {
			return "", "", err
		}
		c.ExpiresAt = e.now().Add(e.limits.CursorTTL).UTC().Truncate(time.Second)
	}
	answerToken, err := e.signRanked(c.answer())
	if err != nil {
		return "", "", err
	}
	if done {
		return "", answerToken, nil
	}
	token, err := e.signRanked(c.at(pos, rankOff, pairOff))
	if err != nil {
		return "", "", err
	}
	return token, answerToken, nil
}

// pageWant is how many records of one ranked section a page takes: its limit,
// or everything that is left when that is less. A section with nothing left is
// not read at all.
func pageWant(limit int, left int64) int {
	if int64(limit) > left {
		if left < 0 {
			return 0
		}
		return int(left)
	}
	return limit
}

// impactPage projects one page of ranked records, the first at ordinal base,
// into served entries and hydrates them. The records arrive in the served order
// and stay in it. An entry names how many reasons and routes it has; the
// reasons and routes views serve them.
func (e *Engine) impactPage(ctx context.Context, page []impactRecord, base int64,
	answer *impactAnswer, meta *model.QueryMeta) ([]model.ImpactEntry, error) {
	entries := make([]model.ImpactEntry, 0, len(page))
	for i, r := range page {
		entry := model.ImpactEntry{Ordinal: base + int64(i), NodeID: r.NodeID, Direction: r.Direction,
			Depth: r.Depth, ScoreMicros: r.scoreMicros(), ReasonCount: int64(len(r.Reasons))}
		if r.Via != "" {
			entry.RouteCount = 1
		}
		entries = append(entries, entry)
	}
	entries, unhydratable, hydrateErr := e.hydrateImpactEntries(deliverCtx(ctx), entries)
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

// deliverCtx detaches the request deadline from the work that DELIVERS a page
// the engine has already decided the contents of: hydrating the page's nodes
// and attaching its evidence.
//
// Those reads are bounded by the page, never by the reachable set -- at most
// Page.Limit nodes and their evidence, in batched round trips -- and the
// records they describe are already CONSUMED: serveRankedImpact advances
// RankServed by the records it took off the spool, so a hydration read that
// fails past the deadline does not truncate the answer, it deletes those
// entities from it with no notice and no cursor that would ever hand them back.
// Measured on a deadline-split fixture walk: 2 176 of 2 440 affected entities
// served, the chain reporting itself complete.
//
// Cancellation is dropped with the deadline, which costs nothing here: a
// canceled request is answered as CTX_CANCELED by impactPhaseError before any
// page is delivered, and what runs under this context is a bounded local read.
func deliverCtx(ctx context.Context) context.Context { return context.WithoutCancel(ctx) }

// spillRankedCursor writes the ranked remainder of the FIRST page into ONE
// spool -- the two sorted runs laid down in section order -- and signs the
// cursor that names it and the byte offset each section begins at.
//
// served is how many records of the whole ranked answer the pages up to and
// including this one have handed back. Every later page reuses this spool and
// this lease; only this one creates them.
func (e *Engine) spillRankedCursor(ctx context.Context, b *budget, endpoint, queryHash string,
	header rankedHeader, served, pairServed int64, meta *model.QueryMeta,
	tails ...func(func([]byte) error) error) (string, error) {
	if !e.canContinue(meta) {
		return "", nil
	}
	binding := e.adjacency.Binding()
	// The lease is conditional, the spool is not: a process that writes nothing
	// to the database still spills the remainder, and the spool it creates is
	// bound to this cursor's expiry rather than to a lease row.
	var leaseID string
	if e.leases.Retains() {
		lease, err := e.leases.Acquire(ctx, binding.GenerationID, binding.SnapshotID, model.LeaseCursor)
		if err != nil {
			return "", err
		}
		leaseID = lease.ID
	}
	next := traversalCursor{
		Version: traversalCursorVersion, Endpoint: endpoint,
		GenerationID: binding.GenerationID, AnalysisKey: binding.AnalysisKey,
		QueryHash: queryHash, LeaseID: leaseID, Ranked: true,
		RankServed: served, RankTotal: header.Total,
		PairServed: pairServed, PairTotal: header.PairTotal,
		Visited: b.visited, Edges: b.edges,
		ExpiresAt: e.now().Add(e.limits.CursorTTL).UTC().Truncate(time.Second),
	}
	id, rankOff, pairOff, err := e.spillRanked(next, header, tails...)
	if err != nil {
		return "", e.releaseLease(ctx, leaseID, err)
	}
	next.SpoolID, next.RankOffset, next.PairOffset = id, rankOff, pairOff
	token, err := e.signRanked(next)
	if err != nil {
		e.releaseConsumed(ctx, id, "")
		return "", e.releaseLease(ctx, leaseID, err)
	}
	return token, nil
}

// continueRankedCursor mints the cursor for the page after a LATER page. It
// names the SAME spool and the SAME lease and carries the byte offsets the page
// it follows stopped at. It writes nothing to the database: that is what makes
// a ranked page O(page) rather than O(remaining).
//
// A LEASED spool is renewed on every page, because its liveness is its lease's
// (pagination.Spools.live) and the answer is not over. A leaseless spool -- and
// a leased one presented to a process that writes nothing -- has no renewal:
// the token carries forward the expiry its header was stamped with, so the
// whole ranking is reachable for that one window and a page asked for after it
// is the typed CTX_CURSOR_INVALID.
func (e *Engine) continueRankedCursor(ctx context.Context, b *budget, c traversalCursor,
	served, pairServed, rankOff, pairOff int64, meta *model.QueryMeta) (string, error) {
	if !e.canContinue(meta) {
		return "", nil
	}
	next := c
	next.Version = traversalCursorVersion
	next.RankServed, next.PairServed = served, pairServed
	next.RankOffset, next.PairOffset = rankOff, pairOff
	next.Visited, next.Edges = b.visited, b.edges
	if c.LeaseID != "" && e.leases.Retains() {
		if _, err := e.leases.Renew(ctx, c.LeaseID); err != nil {
			return "", err
		}
		next.ExpiresAt = e.now().Add(e.limits.CursorTTL).UTC().Truncate(time.Second)
	}
	return e.signRanked(next)
}

// canContinue reports whether this engine can mint a ranked continuation at
// all. Without a signer or a spool store the answer stops with this page and
// SAYS so, exactly as a walk that cannot spill its frontier does. It is only
// consulted with a ranked remainder still unserved -- every caller checks that
// first -- so the stop always cuts the answer, and returning an empty token
// alone left the caller reading a complete-looking page of a longer ranking.
// Marking it is what makes the cut visible.
func (e *Engine) canContinue(meta *model.QueryMeta) bool {
	if e.signer == nil || e.spools == nil {
		markTruncated(meta, reasonNoContinuation)
		return false
	}
	return true
}

// signRanked validates and signs a ranked continuation payload. It does NOT
// release the spool on failure: the spool outlives the page now, and the caller
// that created it is the only one that may take it back.
func (e *Engine) signRanked(next traversalCursor) (string, error) {
	if err := next.validate(); err != nil {
		return "", err
	}
	payload, err := json.Marshal(next)
	if err != nil {
		return "", &model.Error{Code: model.CodeInternal, Message: "cursor encoding: " + err.Error()}
	}
	return e.signer.Sign(pagination.PurposeCursor, payload, next.ExpiresAt)
}

// spillRanked writes the ranked header and then every record each tail streams,
// one at a time, so the remainder of the answer is never in heap. It reports
// the spool id and the byte offset each SECTION begins at: the sections are
// laid down in order and the writer's own position between them is what places
// the second one, so no reader ever has to count records to find it.
func (e *Engine) spillRanked(next traversalCursor, h rankedHeader,
	tails ...func(func([]byte) error) error) (string, int64, int64, error) {
	sp, err := e.spools.Create(next.spoolCursor())
	if err != nil {
		return "", 0, 0, err
	}
	header, err := encodeRankedHeader(h)
	if err != nil {
		return "", 0, 0, e.releaseSpool(sp, err)
	}
	if err := sp.Append(header); err != nil {
		return "", 0, 0, e.releaseSpool(sp, err)
	}
	// offsets[0] is where the first section's records begin; each later entry
	// is where the next section does.
	offsets := make([]int64, 0, len(tails)+1)
	for _, tail := range tails {
		offsets = append(offsets, sp.Written())
		if err := tail(sp.Append); err != nil {
			return "", 0, 0, e.releaseSpool(sp, err)
		}
	}
	offsets = append(offsets, sp.Written())
	if err := sp.Close(); err != nil {
		return "", 0, 0, e.releaseSpool(sp, err)
	}
	e.recordSpoolWritten(sp.Written())
	pairOff := offsets[len(offsets)-1]
	if len(offsets) > 1 {
		pairOff = offsets[1]
	}
	return sp.ID(), offsets[0], pairOff, nil
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
	state walkState, retain *retainedWalk) (string, error) {
	if !state.More || state.DepthLimited {
		return "", nil
	}
	// Nothing of the walk is materialized or copied here. The frontier is the
	// admitted file of the level before the one the walk stopped in, the
	// admitted nodes are already bits beside it, and the token names the
	// directory rather than a copy of what is in it.
	return e.nextTraversalCursor(ctx, b, continuation{
		Endpoint:    endpoint,
		QueryHash:   queryHash,
		Level:       state.Level,
		LevelState:  state.LevelState,
		LevelPos:    state.LevelPos,
		RawBytes:    state.RawBytes,
		LevelOffset: state.LevelOffset,
		More:        state.More,
		Retain:      retain,
	})
}

// walkStalled reports that this page ended on the deadline having made NO
// progress: it admitted no edge and no node, and the continuation it is about
// to mint names the same level, the same level state and the same position in
// it as the one it was handed. Minting it would hand the caller back the cursor they arrived
// with, and a client that follows the chain pages forever without ever
// reaching a new edge -- observed as a visited_count frozen for thousands of
// pages when every page's first adjacency read already ran past the deadline.
//
// The walk cannot simply read one more batch first: the request context
// carries the SAME deadline (beginImpactQuery), so no adjacency read can
// succeed past it. So the honest answer is the other one the page rule
// allows -- the deadline is terminal for this answer, reported with its own
// truncation reason, and the caller's remedy is to raise
// resources.query_timeout. Nothing is lost that the caller did not already
// hold: this page served nothing.
//
// It is only ever true on a RESUMED page. The request that mints the answer
// has no earlier continuation to repeat, and a deadline there always leaves a
// frontier the caller has never seen.
func walkStalled(b *budget, state walkState, resume *resumeState) bool {
	if resume == nil || b.pageEdges != 0 || b.pageVisited != 0 {
		return false
	}
	c := resume.Cursor
	return state.Level == c.Level && state.LevelState == c.LevelState &&
		state.LevelPos == c.LevelPos && state.RawBytes == c.RawBytes &&
		state.LevelOffset == c.LevelOffset
}

// continueRank mints the mid-rank continuation: the walk is EXHAUSTED and the
// deadline cut the ranking short, so the token names the retained pass-1 input
// alone -- no frontier, no keyset position -- and the next request finishes
// both passes over it and serves page 1. The mint persists nothing itself: the
// input the sort reads is what every leg has been appending to all along, and
// an interrupted impact pass's runs were already detached beside it.
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
// It is a free function taking the engine rather than a method: it is the
// impact family's shared entry, not an Engine operation of its own.
func beginImpactQuery(ctx context.Context, e *Engine) (context.Context, time.Time, func(), error) {
	// One clock: the deadline is measured on the ENGINE clock, the same one the
	// walk budget compares against, and it is returned so the caller reuses this
	// instant instead of recomputing a later one after the gate wait.
	// A request with no deadline of any kind (query_timeout 0, no caller
	// deadline) runs unbounded: a zero instant here would be a deadline that
	// has already passed, which refuses every query instead of running it.
	deadline, bounded := e.queryDeadline(ctx)
	var cancel context.CancelFunc
	if bounded {
		ctx, cancel = context.WithDeadline(ctx, deadline)
	} else {
		ctx, cancel = context.WithCancel(ctx)
	}
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
// remainder cannot be paged: this engine was built without the continuation
// machinery, so it has neither a signer to bind a token to nor a spool store to
// spill the remainder into. The records exist and the ranking is complete; what
// is missing is the token that would hand the rest of them back, so the page
// that is served is a prefix and must say it is one.
const reasonNoContinuation = "this answer was served by an endpoint that retains no continuation state, " +
	"so the ranked records beyond this page are not reachable; narrow the walk's depth or its node bound " +
	"to bring the answer within one page"

// reasonDeadlineStalled is the truncation reason for a page that ran out of
// time WITHOUT moving the walk on: it admitted nothing and left the frontier
// and the keyset position exactly where it found them. See walkStalled.
const reasonDeadlineStalled = "query deadline reached before the walk could advance"

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
	// sort. There is no byNode map and no order slice: the request runs the walk
	// to completion, so either would be sized by the reachable set rather than
	// by the page, and merging a node's records is foldImpact's job inside
	// pass 1 of the sort.
	emit       func(impactRecord) error
	budget     *budget
	maxVisited config.Limit
	maxEdges   config.Limit
	reason     string
	// names is the per-level canonical resolution expand refills before each
	// level's edges are delivered. The walk carries surrogates; a ranking
	// record must carry content-derived canonical ids, and this is the one
	// batched read per level that turns the one into the other.
	names *levelNames
	// kinds names a relation kind from the generation's kind code, so a reason
	// and a cost are read off the edge without a dictionary lookup per edge.
	kinds []model.RelationKind
	// climbs is set on a walk that takes the callers dispatch step: an
	// `overrides` edge it climbs reaches a base method only as the route to
	// that base's virtual callers, so the base is not listed as affected.
	climbs bool
}

// newImpactAccumulator builds the visitor for one LEG of a walk. resume is the
// continuation this leg was handed, or nil on the request that mints the
// answer.
//
// The scan position is SEEDED from that continuation rather than left zero.
// Two things depend on it. A page that admits nothing -- every adjacency read
// ran past the deadline -- must mint the position it was given, not the zero
// EdgePos, which restarts the level from its beginning and re-reads every row
// the previous page already read.
// And walkStalled compares this position against the cursor's to decide that
// the page made no progress at all: with a zero start the two could only ever
// match on a resume taken at a LEVEL BOUNDARY, so a walk stalled MID-LEVEL --
// the common shape, since a deadline lands inside a multi-owner chunk -- could
// not report it until it had first thrown the position away.
func newImpactAccumulator(start []model.NodeID, b *budget, maxVisited, maxEdges config.Limit,
	emit func(impactRecord) error, names *levelNames, kinds []model.RelationKind) *impactAccumulator {
	a := &impactAccumulator{
		isSeed:     make(map[model.NodeID]bool, len(start)),
		emit:       emit,
		budget:     b,
		maxVisited: maxVisited,
		maxEdges:   maxEdges,
		names:      names,
		kinds:      kinds,
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
func (a *impactAccumulator) Visit(state frontierState, e Edge) error {
	switch {
	// The CUMULATIVE counters, not the per-page ones. The walk
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
	if a.emit == nil {
		// The package rollup rides on the same walk but reads only the edges;
		// it has no record sort to stream into.
		return nil
	}
	reached := a.names.node(e.Neighbour)
	if reached == "" {
		// The generation carries an adjacency entry for a node it publishes no
		// canonical id for. It cannot be named, so it cannot be listed; the
		// EDGE is still admitted, because the walk read it and the rollup
		// counts it.
		return nil
	}
	if a.isSeed[reached] {
		// A seed is the thing being changed, not something the change affects.
		// The edge itself is still admitted -- the package rollup teed off this
		// visitor counts it -- so this returns nil rather than a stop.
		return nil
	}
	kind := a.kindOf(e.Kind)
	if a.climbs && kind == model.RelOverrides && e.Outgoing {
		return nil
	}
	direction := impactDirection(kind, e.Outgoing)
	depth := state.Depth + 1
	// One record per admitted edge, not per node. Two edges reaching the same
	// node produce two records with the same NodeID, and pass 1 of the sort
	// folds them with foldImpact -- which reproduces exactly what the old
	// per-node merge did (cheapest route wins, an incoming direction is sticky,
	// the reasons are the deduplicated union), without a map whose size was the
	// reachable set.
	//
	// Every identifier on the record is CANONICAL, resolved from this level's
	// one batched read. That is ADR-0005 Decision 2's whole point: the walk is
	// free to carry build-dependent surrogates, and the order the answer is
	// served in is not -- foldImpact's route tie-break reads Via, the route
	// hash and Parent, so deriving any of the three from a surrogate would let
	// a fresh index and a delta-built index of the same tree serve different
	// pages. The route itself is the owner's admission plus Via: the rows
	// behind the admission hold canonical ids too.
	via := a.names.rel(e.Rel)
	return a.emit(impactRecord{
		NodeID:    reached,
		Depth:     depth,
		Cost:      impactAdd(state.Cost, kind),
		Direction: direction,
		Via:       via,
		Parent:    a.names.node(state.Node),
		Adm:       state.Adm,
		Hash:      state.Hash.extend(via),
		Reasons:   []string{impactEdgeReason(kind, e.Outgoing, direction, depth)},
	})
}

// kindOf names the relation kind behind one edge's generation kind code.
func (a *impactAccumulator) kindOf(code KindCode) model.RelationKind {
	if int(code) >= len(a.kinds) {
		return ""
	}
	return a.kinds[code]
}

// impactReason renders one reason in product vocabulary, naming the relation
// kind, the direction and the depth -- never the analysis engine that produced
// the edge. Section 14.3 rejects an entry with no reason outright.
func impactReason(kind model.RelationKind, dir model.Direction, depth int) string {
	return impactEdgeReason(kind, dir == model.DirectionOutgoing, dir, depth)
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
	// The page arrives ALREADY ordered, by the served key (ScoreMicros desc,
	// Depth asc, NodeID asc), which the ranking pass applied over the whole
	// answer. No tie-break is re-applied here: Name is known only after
	// hydration, so re-sorting a page by it would reorder one page of a
	// globally ordered answer and no two pages would agree on where a record
	// belongs.
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

// attachRouteEvidence makes every served route evidence-backed: the evidence
// for a whole page of routes is hydrated in bounded batches, never one query
// per edge, and a route carries the evidence of every relation it crosses.
func (e *Engine) attachRouteEvidence(ctx context.Context, routes []model.ImpactRoute) error {
	var ids []model.RelationID
	for _, r := range routes {
		ids = append(ids, r.Relations...)
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
	for i := range routes {
		var ev []model.EvidenceID
		for _, rel := range routes[i].Relations {
			ev = append(ev, evidence[rel]...)
		}
		routes[i].Evidence = ev
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
