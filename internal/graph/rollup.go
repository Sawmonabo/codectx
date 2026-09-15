package graph

import (
	"context"
	"errors"
	"slices"
	"unicode/utf8"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
)

// PackageDependencies rolls a bounded symbol-level walk up to the package or
// module containers of its endpoints and reports the distinct
// (from-package, to-package) pairs it found, with the number of symbol edges and
// the number of evidence records behind each pair.
//
// Section 14.3 forbids presenting a rollup as a precise symbol call, so the
// counts ride on every pair and the endpoints are always container nodes: a
// consumer can see that "app depends on lib" rests on nine edges without ever
// being told which function called which.
func (e *Engine) PackageDependencies(ctx context.Context, req model.GraphRequest) (page model.Page[model.PackageEdge], err error) {
	defer func() { err = typedContextError(ctx, err) }()
	if err := req.Validate(); err != nil {
		return model.Page[model.PackageEdge]{}, err
	}
	ctx, deadline, done, err := beginImpactQuery(ctx, e)
	if err != nil {
		return model.Page[model.PackageEdge]{}, err
	}
	defer done()

	kinds := req.Relations
	if len(kinds) == 0 {
		kinds = DefaultRelations()
	}
	meta := model.QueryMeta{Binding: e.adjacency.Binding()}
	caps, deferred, err := e.completeness(ctx, kinds)
	if err != nil {
		return model.Page[model.PackageEdge]{}, err
	}
	meta.Completeness = caps
	if deferred {
		markTruncated(&meta, reasonDependence)
	}

	b := &budget{deadline: deadline, now: e.now}
	maxVisited, visitedNotice := resolveLimit("max_visited", req.MaxVisited, e.limits.Visited())
	maxEdges, edgeNotice := resolveLimit("max_edges", req.MaxEdges, e.limits.Edges())
	maxDepth, depthNotice := resolveLimit("max_depth", req.MaxDepth, e.limits.Depth())
	limit, limitNotice := resolvePageItems(req.Page.Limit, e.limits.MaxPageItems)
	meta.Notices = appendNotice(appendNotice(appendNotice(appendNotice(meta.Notices,
		visitedNotice), edgeNotice), depthNotice), limitNotice)
	// The page limit is part of the normalized query a continuation is bound to:
	// a resumed page asking for a different one would stop the walk somewhere
	// the issuing page never did.
	queryHash := traversalQueryHash(req.Direction, kinds, req.Start, maxDepth.Int(), limit)
	// stalled marks the walkStalled page: it ends the answer with no cursor,
	// so the one the caller holds must stay adoptable (impact.go).
	stalled := false
	var resume *resumeState
	if req.Page.Cursor != "" {
		c, rb, err := e.verifyContinuation(req.Page.Cursor, packageDepsEndpoint, queryHash, deadline)
		if err != nil {
			return model.Page[model.PackageEdge]{}, err
		}
		if c.Ranked {
			// The walk is over and the order is settled: this page is a read of
			// the ranked pair spool and expands nothing.
			return e.serveRankedPairs(ctx, c, rb, limit, meta)
		}
		resume, err = e.resumeTraversal(ctx, req.Page.Cursor, packageDepsEndpoint, queryHash, deadline)
		if err != nil {
			return model.Page[model.PackageEdge]{}, err
		}
		// The consumed spool outlives the walk -- the membership probes and the
		// spill that copies the cumulative set forward both read it -- so the
		// release is deferred rather than run on the happy path alone. The one
		// exception is the stalled page below, which ends the answer with no
		// cursor of its own: the cursor the caller holds is the remedy, so the
		// state it names has to survive this request (impact.go says why).
		defer func() {
			if !stalled {
				resume.Release()
			}
		}()
		b = resume.Budget
	}
	// The pass-1 INPUT is retained across requests for the reason walkretain.go
	// states: this answer is globally ranked, and a walk the deadline splits
	// over several requests can only be ranked as one walk if every leg's
	// records survive the request that produced them.
	retain := resumeRetained(resume)
	if retain == nil {
		if retain, err = openRetainedWalk(e.walkScratchDir(), e.visitedFilterBytes(), e.probe); err != nil {
			return model.Page[model.PackageEdge]{}, err
		}
	}
	// Discarded AFTER the continuation below has taken it.
	defer retain.discard()

	// The accumulator enforces the ANSWER-level visited and edge bounds
	// (impactAccumulator.Visit states why they are the cumulative counters and
	// not the per-page ones) and records the keyset
	// position a deadline continuation resumes from. It emits no impact record:
	// this endpoint ranks pairs, not entities.
	var (
		acc   *impactAccumulator
		state walkState
	)
	if resume == nil || !resume.Cursor.WalkDone {
		acc = newImpactAccumulator(req.Start, b, maxVisited, maxEdges, nil, resume)
		walkErr := e.rollupInto(ctx, &meta, func(sink edgeSink) error {
			var werr error
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
				// admitted, so the rollup must not count it.
				if err := acc.Visit(fs, rel); err != nil {
					return err
				}
				return sink.Visit(fs, rel)
			})
			return werr
		}, retain.addPair, e.rollupProbe())
		if err := impactPhaseError(ctx, walkErr, &meta); err != nil {
			return model.Page[model.PackageEdge]{}, err
		}
	}
	if b.deadlineHit && len(state.Frontier) > 0 {
		// Ruling P3: the deadline ended this PAGE, not the answer. Nothing is
		// ranked and nothing is served -- ranking a walk that is still running
		// would publish an order the next page contradicts -- and the walk
		// continuation carries the frontier AND this leg's pair records forward.
		if walkStalled(b, state, acc.lastOwner, acc.lastKey, resume) {
			// No continuation: it would be the one this request was given, and
			// it stays adoptable so presenting it again under a longer timeout
			// resumes this walk.
			stalled = true
			markTruncated(&meta, reasonDeadlineStalled)
			return validatedPairPage(meta, nil)
		}
		markTruncated(&meta, reasonDeadline)
		if meta.NextCursor, err = e.continueWalk(ctx, b, packageDepsEndpoint, queryHash,
			state, acc.lastOwner, acc.lastKey, resume, retain); err != nil {
			return model.Page[model.PackageEdge]{}, err
		}
		return validatedPairPage(meta, nil)
	}
	switch {
	case acc != nil && acc.reason != "":
		markTruncated(&meta, acc.reason)
	case b.frontierHit:
		// The rollup summarises the edges the walk read; a level the frontier
		// budget cut short must not read as the whole neighbourhood.
		markTruncated(&meta, reasonFrontierBytes)
	case state.DepthLimited:
		markTruncated(&meta, reasonDepth)
	}
	// The walk is exhausted: the ranking runs over every pair record EVERY leg
	// appended, so the counts are exact sums over the whole walk and the order
	// is the single unbounded walk's order.
	run, rollupErr := e.rankPairs(ctx, retain.eachPair, e.pairProbe())
	if run != nil {
		defer run.Close()
	}
	if err := impactPhaseError(ctx, rollupErr, &meta); err != nil {
		return model.Page[model.PackageEdge]{}, err
	}
	if run == nil {
		// Ruling P7: the deadline landed mid-RANK, after the walk had finished.
		// walkImpact (impact.go) states why the retained input is named rather
		// than re-walked.
		markTruncated(&meta, reasonDeadline)
		if meta.NextCursor, err = e.continueRank(ctx, b, packageDepsEndpoint, queryHash, retain); err != nil {
			return model.Page[model.PackageEdge]{}, err
		}
		return validatedPairPage(meta, nil)
	}
	items := make([]model.PackageEdge, 0, limit)
	if err := run.Each(func(r pairRecord) error {
		if err := ctx.Err(); err != nil {
			return typedContextError(ctx, err)
		}
		items = append(items, r.edge())
		if len(items) == limit {
			return errStopExpansion
		}
		return nil
	}); err != nil && !errors.Is(err, errStopExpansion) {
		return model.Page[model.PackageEdge]{}, err
	}
	// The page is cut from the globally ranked pairs and the remainder spooled
	// behind the `r` cursor. The walk ran to COMPLETION, so each pair's counts
	// are exact sums over every edge the walk admitted -- not over one page's
	// -- and the union of the pages is the single-shot answer, in the
	// single-shot order, with no pair listed twice.
	if int64(len(items)) < run.Len() {
		header := rankedHeader{Total: run.Len(), Count: int(run.Len()) - len(items)}
		if meta.NextCursor, err = e.spillRankedCursor(ctx, b, packageDepsEndpoint, queryHash, header,
			int64(len(items)), 0, &meta, rankedRunTail(ctx, run, len(items), encodePairRecord)); err != nil {
			return model.Page[model.PackageEdge]{}, err
		}
	}
	return validatedPairPage(meta, items)
}

// serveRankedPairs serves a LATER page of a package-dependency answer: it seeks
// to this cursor's byte offset in the spool the FIRST page wrote, reads at most
// one page out of it, and walks nothing.
func (e *Engine) serveRankedPairs(ctx context.Context, c traversalCursor, b *budget, limit int,
	meta model.QueryMeta) (_ model.Page[model.PackageEdge], err error) {
	// The spool and its lease are released when the ANSWER ends, not when a
	// page does: every later page reads the same spool. complete is explicit
	// rather than "no cursor was minted", because a renewal or signing failure
	// also mints no cursor and must leave this cursor adoptable.
	complete := false
	// Delivery, for serveRankedImpact's reason.
	ctx = deliverCtx(ctx)
	defer func() {
		if terminalOutcome(err) && complete {
			e.releaseConsumed(ctx, c.SpoolID, c.LeaseID)
		}
	}()
	tail := rankedTail{SpoolID: c.SpoolID, LeaseID: c.LeaseID,
		Offset: c.RankOffset, Served: c.RankServed, Total: c.RankTotal}
	records, rest, read, err := servePage(ctx, e.spools, c.spoolCursor(), e.now(), tail, limit, decodePairRecord)
	if err != nil {
		return model.Page[model.PackageEdge]{}, err
	}
	e.recordSpoolRead(read)
	items := make([]model.PackageEdge, 0, len(records))
	for _, r := range records {
		items = append(items, r.edge())
	}
	complete = rest.done()
	if !complete {
		if meta.NextCursor, err = e.continueRankedCursor(ctx, b, c,
			rest.Served, 0, rest.Offset, c.PairOffset, &meta); err != nil {
			return model.Page[model.PackageEdge]{}, err
		}
	}
	return validatedPairPage(meta, items)
}

// validatedPairPage is the one place a package-dependency page checks its own
// contract -- the page's bounds and every item's -- so no return path can serve
// a page that has not been validated.
func validatedPairPage(meta model.QueryMeta, items []model.PackageEdge) (model.Page[model.PackageEdge], error) {
	page := model.Page[model.PackageEdge]{Meta: meta, Items: items}
	if err := page.Validate(); err != nil {
		return model.Page[model.PackageEdge]{}, err
	}
	for _, item := range items {
		if err := item.Validate(); err != nil {
			return model.Page[model.PackageEdge]{}, err
		}
	}
	return page, nil
}

// packageDepsEndpoint binds a package-dependency continuation to the operation
// that issued it. It is deliberately NOT impactEndpoint: verifyContinuation's
// endpoint check is the only thing that stops a deps cursor resuming an impact
// walk, and the two produce different answers from the same frontier.
const packageDepsEndpoint = "graph.package_dependencies"

// edgeSink accepts the edges a walk admits, ONE AT A TIME, in the shape
// expand's visit callback delivers them. It is the seam ruling P2's completed
// walk (lane P-b) hands its edges to: the walk streams, the sink folds each
// batch into the pair sort as it fills, and neither side ever holds the
// admitted set. A walk that runs to exhaustion can therefore feed a rollup
// whose peak heap is one batch of edges rather than one of every edge the
// walk admitted.
//
// The frontier state is part of the signature so a sink can be passed straight
// to expand (or runWalkToCompletion) as its visit function. The package rollup
// ignores it: a pair is a fact about the edge's endpoints alone.
type edgeSink interface {
	Visit(state frontierState, rel model.Relation) error
}

// pairRollupBatch is how many admitted edges one resolution batch holds. An
// edge contributes at most two endpoints, so a batch's endpoint set is at most
// adjacencyBatch -- one containment page -- which is what keeps the rollup's
// live set a function of the batch and never of the walk.
const pairRollupBatch = adjacencyBatch / 2

// pairRollup is the streaming half of the rollup: it buffers one batch of
// admitted edges, resolves THAT batch's containers and evidence counts, and
// emits one pairRecord per surviving edge into the pair sort, which folds the
// records of one pair into the exact sums ruling P4 requires.
//
// It holds the request context rather than taking one per edge because
// edgeSink's signature is expand's visit callback, which carries none; the
// sink is request-scoped and never outlives the call that built it.
type pairRollup struct {
	ctx  context.Context
	e    *Engine
	meta *model.QueryMeta
	add  func(pairRecord) error

	batch []model.Relation
	// labels caches the reported label of each container the rollup has
	// resolved, keyed by its surrogate. Its bound is the number of DISTINCT
	// CONTAINERS the walk touched -- packages and modules -- and never the
	// number of nodes or edges it visited: a container is hydrated once per
	// walk however many of its members the walk admits, which is what turned
	// the old rollup's per-batch re-hydration of the same few packages into 93
	// per cent of a large walk. A repository has orders of magnitude fewer
	// packages than symbols, so the cache is a page-sized structure by
	// construction.
	labels map[NodeRef]containerLabel
	// peak is the largest batch the sink ever held, the memory high-water mark
	// rollupStats reports.
	peak int
}

// containerLabel is what a rolled-up endpoint reports: the container's
// canonical id, which the pair is ordered and folded by, and its path-shaped
// label, already clipped to the served field bound.
type containerLabel struct {
	id   model.NodeID
	path string
}

// newPairRollup opens a sink that emits into add.
func newPairRollup(ctx context.Context, e *Engine, meta *model.QueryMeta,
	add func(pairRecord) error) *pairRollup {
	return &pairRollup{ctx: ctx, e: e, meta: meta, add: add,
		batch:  make([]model.Relation, 0, pairRollupBatch),
		labels: map[NodeRef]containerLabel{}}
}

// Visit buffers one admitted edge and resolves the batch once it is full.
func (p *pairRollup) Visit(_ frontierState, rel model.Relation) error {
	p.batch = append(p.batch, rel)
	if len(p.batch) > p.peak {
		p.peak = len(p.batch)
	}
	if len(p.batch) < pairRollupBatch {
		return nil
	}
	return p.flush()
}

// flush resolves the buffered batch and empties it. It is called for every
// full batch and once more for the remainder, so an edge is emitted exactly
// once however the batches fell.
//
// The resolution is three ARRAY reads on the pinned reader -- the endpoints'
// surrogates, their container surrogates and the batch's evidence counts --
// and never a containment traversal. ADR-0005 states why: the old rollup
// re-read the incoming `contains` edges of every endpoint of every batch and
// re-hydrated the same handful of container nodes over a thousand batches,
// which is where 93 per cent of a large walk's wall clock went. The container
// of a node is a per-generation side array now, so a batch costs one indexed
// read per array whatever the fan-out of its endpoints.
func (p *pairRollup) flush() error {
	if len(p.batch) == 0 {
		return nil
	}
	batch := p.batch
	// The buffer is emptied BEFORE the batch is resolved, not after: a feed
	// that swallows a Visit error the way expand swallows errStopExpansion
	// would otherwise let rollupRanked's trailing flush emit this batch a
	// second time. Nothing appends to p.batch while batch is being read, so
	// the two may alias.
	p.batch = p.batch[:0]
	endpoints := make([]model.NodeID, 0, 2*len(batch))
	relationIDs := make([]model.RelationID, 0, len(batch))
	for _, r := range batch {
		endpoints = append(endpoints, r.From, r.To)
		relationIDs = append(relationIDs, r.ID)
	}
	containers, err := p.containerLabels(endpoints)
	if err != nil {
		return err
	}
	// Evidence counts still come from the batched relation hydration rather
	// than from GraphReader.EvidenceCounts, for one reason: that array is keyed
	// by RelRef and the port offers no canonical-id-to-relation-surrogate
	// resolution, while the walk's visitor delivers a model.Relation. It is one
	// batched read per batch of relations either way -- it was never part of
	// the rollup's cost -- and it moves with the walk's visitor signature.
	evidence, err := p.e.evidenceCounts(p.ctx, relationIDs)
	if err != nil {
		return err
	}
	for _, r := range batch {
		from, okFrom := containers[r.From]
		to, okTo := containers[r.To]
		// An edge whose endpoints share a container contributes nothing: a
		// package depending on itself is not a dependency. An edge with no
		// resolvable container is dropped rather than attributed to a guessed
		// package.
		if !okFrom || !okTo || from.id == to.id {
			continue
		}
		if err := p.add(pairRecord{
			FromNodeID: from.id, ToNodeID: to.id,
			FromPath: from.path, ToPath: to.path,
			PairCount: 1, EvidenceCount: evidence[r.ID],
		}); err != nil {
			return err
		}
	}
	return nil
}

// containerLabels maps each endpoint of one batch to the package or module
// that contains it, reading the generation's container side array rather than
// walking containment. A node that is itself a container maps to itself (the
// reader's own rule), so a package-level edge rolls up to the pair it already
// names, and a node with no container is absent from the map, which drops its
// edge rather than attributing it to a guessed package.
//
// Only containers this rollup has not seen before are hydrated, and the cache
// that decides that is keyed by the container surrogate: the hydration cost of
// a whole walk is therefore one batched node read per adjacencyBatch DISTINCT
// containers, not one per batch of edges.
func (p *pairRollup) containerLabels(ids []model.NodeID) (map[model.NodeID]containerLabel, error) {
	reader, err := p.e.consumerReader()
	if err != nil {
		return nil, err
	}
	ids = dedupeNodes(append([]model.NodeID(nil), ids...))
	out := make(map[model.NodeID]containerLabel, len(ids))
	// missing collects the containers this batch needs and the cache does not
	// hold, deduplicated by surrogate so one unseen package is hydrated once
	// however many of this batch's endpoints name it.
	missing := map[NodeRef]bool{}
	owners := make([]NodeRef, 0, len(ids))
	for _, chunk := range impactChunkNodes(ids) {
		// THE RESOLUTION SEAM. The walk's visitor still delivers canonical
		// endpoint ids, so this batch has to resolve them to surrogates before
		// it can read the side arrays. Once the walk carries surrogates the
		// endpoints arrive as refs already and this call -- and the id-keyed
		// map around it -- is the only thing that has to go; every read below
		// is already on refs.
		refs, err := reader.Resolve(p.ctx, chunk)
		if err != nil {
			return nil, err
		}
		containers, err := reader.Containers(p.ctx, refs)
		if err != nil {
			return nil, err
		}
		owners = append(owners, containers...)
		for _, c := range containers {
			if c == 0 {
				continue
			}
			if _, cached := p.labels[c]; !cached {
				missing[c] = true
			}
		}
	}
	if err := p.hydrate(reader, missing); err != nil {
		return nil, err
	}
	for i, id := range ids {
		label, ok := p.labels[owners[i]]
		if !ok {
			// No container in this generation, or one the generation does not
			// publish a node for: the endpoint is unattributed and its edge is
			// dropped, exactly as before.
			continue
		}
		out[id] = label
	}
	return out, nil
}

// hydrate reads the canonical id and the reported label of every container the
// cache is missing, in batched round trips, and records them. A container the
// generation publishes no node for is recorded as absent rather than retried
// on the next batch, so one unpublishable container cannot cost one read per
// batch for the rest of the walk.
func (p *pairRollup) hydrate(reader GraphReader, missing map[NodeRef]bool) error {
	if len(missing) == 0 {
		return nil
	}
	refs := make([]NodeRef, 0, len(missing))
	for ref := range missing {
		refs = append(refs, ref)
	}
	// Ascending, so the reads cluster the way the packed side arrays are laid
	// out rather than probing them in map-iteration order.
	slices.Sort(refs)
	for len(refs) > 0 {
		chunk := refs
		if len(chunk) > adjacencyBatch {
			chunk = chunk[:adjacencyBatch]
		}
		refs = refs[len(chunk):]
		ids, err := reader.NodeIDs(p.ctx, chunk)
		if err != nil {
			return err
		}
		want := make([]model.NodeID, 0, len(ids))
		for _, id := range ids {
			if id != "" {
				want = append(want, id)
			}
		}
		nodes, err := reader.NodesByID(p.ctx, want)
		if err != nil {
			return err
		}
		byID := make(map[model.NodeID]model.Node, len(nodes))
		for _, n := range nodes {
			byID[n.ID] = n
		}
		for i, ref := range chunk {
			n, ok := byID[ids[i]]
			if !ok {
				continue
			}
			// The label is clipped HERE, where it enters the cache:
			// pairRecord's projection onto model.PackageEdge does no clipping,
			// and an over-long path would fail the served item's own
			// validation.
			p.labels[ref] = containerLabel{id: n.ID, path: clipPath(packageLabel(n))}
		}
	}
	return nil
}

// rollupStats reports what the rollup held at once. It exists so the memory
// invariant -- peak live edges is one batch, never the walk -- is asserted
// against the production path rather than restated by a test, the shape
// internal/search's collector uses for its own sort peak. Nothing in the
// served answer depends on it.
type rollupStats struct{ PeakLiveEdges int }

// observe records a live-set high-water mark.
func (s *rollupStats) observe(n int) {
	if n > s.PeakLiveEdges {
		s.PeakLiveEdges = n
	}
}

// rollupRanked is the whole rollup as ruling P4 specifies it: feed streams the
// walk's admitted edges into an edgeSink, each batch resolves its own
// containers and evidence, and rankPairs folds and orders the pairs on disk.
// The counts it reports are exact sums over every edge fed, not over one page
// of them, and the order is the global (FromPath, ToPath, FromNodeID,
// ToNodeID) one -- both facts of the whole feed and neither of where the
// batches fell.
//
// The caller closes the returned run. stats may be nil.
//
// It is what both ranked package answers are served from: PackageDependencies
// reads its page off this run directly, and an impact answer tees its walk into
// it alongside the entity ranking, so the two can never disagree about what a
// pair means.
func (e *Engine) rollupRanked(ctx context.Context, meta *model.QueryMeta,
	feed func(edgeSink) error, stats *rollupStats) (*pagination.SortedRun[pairRecord], error) {
	return e.rankPairs(ctx, func(add func(pairRecord) error) error {
		return e.rollupInto(ctx, meta, feed, add, stats)
	}, e.pairProbe())
}

// rollupInto is the streaming half alone: feed's admitted edges are batched,
// each batch resolves its own containers and evidence, and one pairRecord per
// surviving edge is appended to add. It is factored out of rollupRanked
// because the sink's destination is no longer always a sort -- a walk that is
// split across requests appends its pairs to the RETAINED pass-1 input
// instead (walkretain.go) and ranks them only once the walk is exhausted -- and
// the batching, the cut state and the exactness of the counts must be the same
// either way.
//
// stats may be nil.
func (e *Engine) rollupInto(ctx context.Context, meta *model.QueryMeta,
	feed func(edgeSink) error, add func(pairRecord) error, stats *rollupStats) error {
	sink := newPairRollup(ctx, e, meta, add)
	if err := feed(sink); err != nil {
		return err
	}
	if err := sink.flush(); err != nil {
		return err
	}
	if stats != nil {
		stats.observe(sink.peak)
	}
	return nil
}

// evidenceCounts counts the evidence records backing each relation, in bounded
// batches. A container relation legitimately carries none, which is why a zero
// evidence count is a valid pair and a zero pair count is not.
func (e *Engine) evidenceCounts(ctx context.Context, ids []model.RelationID) (map[model.RelationID]int64, error) {
	out := make(map[model.RelationID]int64, len(ids))
	for _, batch := range impactChunkRelations(dedupeRelations(append([]model.RelationID(nil), ids...))) {
		got, err := e.adjacency.EvidenceFor(ctx, batch, model.MaxRelationsPerPath)
		if err != nil {
			return nil, err
		}
		for id, ev := range got {
			out[id] = int64(len(ev))
		}
	}
	return out, nil
}

// isContainerKind reports whether a node kind can be the target of a rollup.
// Only packages and modules qualify: rolling up to a file or a directory would
// answer a different question than the one Section 14.3 asks.
func isContainerKind(k model.NodeKind) bool {
	return k == model.NodePackage || k == model.NodeModule
}

// packageLabel is the path-shaped name a rollup reports for a container. A
// package has no file of its own, so its qualified name is the most specific
// identifier there is; the plain name is the fallback when nothing qualified it.
func packageLabel(n model.Node) string {
	if n.QualifiedName != "" {
		return n.QualifiedName
	}
	return n.Name
}

// clipPath bounds a reported path to MaxPathBytes on a rune boundary. Over-long
// input would otherwise fail the result contract and turn a complete answer into
// an error; clipping mid-rune would emit invalid UTF-8 in JSON.
func clipPath(s string) string { return clipTo(s, model.MaxPathBytes) }

// clipTo bounds s to max bytes on a rune boundary. It is the one clipper behind
// clipPath and the qualified-name clip an impact entry reports, so a reported
// identifier can never fail its own field bound.
func clipTo(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}
