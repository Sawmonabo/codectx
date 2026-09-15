package graph

import (
	"context"
	"unicode/utf8"

	"github.com/Sawmonabo/codectx/internal/config"
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
	var resume *resumeState
	if req.Page.Cursor != "" {
		resume, err = e.resumeTraversal(ctx, req.Page.Cursor, packageDepsEndpoint, queryHash, deadline)
		if err != nil {
			return model.Page[model.PackageEdge]{}, err
		}
		// The consumed spool outlives the walk -- the membership probes and the
		// spill that copies the cumulative set forward both read it -- so the
		// release is deferred rather than run on the happy path alone.
		defer resume.Release()
		b = resume.Budget
	}
	acc := // The rollup reads acc.Relations() and no impact record, so it emits
		// none. Lane P-c replaces this with the pair sort of ruling P4.
		newImpactAccumulator(req.Start, b, maxVisited, maxEdges, limit, nil)
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
		return model.Page[model.PackageEdge]{}, err
	}
	switch {
	case acc.reason != "":
		markTruncated(&meta, acc.reason)
	case b.frontierHit:
		// The rollup summarises the edges the walk read; a level the frontier
		// budget cut short must not read as the whole neighbourhood.
		markTruncated(&meta, reasonFrontierBytes)
	case state.DepthLimited:
		markTruncated(&meta, reasonDepth)
	}

	items, rollupErr := e.rollupPackages(ctx, acc.Relations(), &meta)
	if err := impactPhaseError(ctx, rollupErr, &meta); err != nil {
		return model.Page[model.PackageEdge]{}, err
	}
	// No page cut here, and no "further pairs were aggregated" notice: the walk
	// itself stops at `limit` admitted edges, so the distinct pairs it rolls up
	// can never exceed the page. The notice used to stand in for the
	// continuation this endpoint refused to offer; the continuation below is
	// now the honest channel, and each pair's counts are the counts of THIS
	// page's edges -- summing a pair across pages reproduces the whole-walk
	// total, because every edge is admitted on exactly one page.
	if meta.NextCursor, err = e.continueWalk(ctx, b, packageDepsEndpoint, queryHash,
		state, acc.lastOwner, acc.lastKey, resume); err != nil {
		return model.Page[model.PackageEdge]{}, err
	}
	page = model.Page[model.PackageEdge]{Meta: meta, Items: items}
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
	// cut is the containment-cut state of the WHOLE rollup, not of one batch:
	// a user-set edge allowance that stops a containment read must be
	// disclosed once however many batches it stops, and a node it dropped must
	// stay dropped rather than resolve from the candidates a later batch
	// happens to add for it. Both are what keep the answer independent of
	// where the batch boundaries fell.
	cut containmentCut
	// peak is the largest batch the sink ever held, the memory high-water mark
	// rollupStats reports.
	peak int
}

// newPairRollup opens a sink that emits into add.
func newPairRollup(ctx context.Context, e *Engine, meta *model.QueryMeta,
	add func(pairRecord) error) *pairRollup {
	return &pairRollup{ctx: ctx, e: e, meta: meta, add: add,
		batch: make([]model.Relation, 0, pairRollupBatch),
		cut:   containmentCut{unattributed: map[model.NodeID]bool{}}}
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
	containers, err := p.e.containerPackages(p.ctx, endpoints, p.meta, &p.cut)
	if err != nil {
		return err
	}
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
		if !okFrom || !okTo || from.ID == to.ID {
			continue
		}
		// The labels are clipped HERE, where the record is built: pairRecord's
		// projection onto model.PackageEdge does no clipping, and an over-long
		// path would fail the served item's own validation.
		if err := p.add(pairRecord{
			FromNodeID: from.ID, ToNodeID: to.ID,
			FromPath: clipPath(packageLabel(from)), ToPath: clipPath(packageLabel(to)),
			PairCount: 1, EvidenceCount: evidence[r.ID],
		}); err != nil {
			return err
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
// This is the seam lane P-INT connects the completed walk's pages to: a served
// page is read off this run and its remainder spooled behind the `r` cursor,
// instead of being drained into a slice as rollupPackages does for today's
// page-bounded callers.
func (e *Engine) rollupRanked(ctx context.Context, meta *model.QueryMeta,
	feed func(edgeSink) error, stats *rollupStats) (*pagination.SortedRun[pairRecord], error) {
	return e.rankPairs(ctx, func(add func(pairRecord) error) error {
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
	})
}

// rollupPackages aggregates symbol-level relations into distinct package pairs.
// It is the shared body behind ImpactResult.Packages and the standalone
// PackageDependencies, so the two can never disagree about what a pair means.
//
// It is the SLICE ADAPTER over rollupRanked, for the callers whose walk is
// still page-bounded: the relations it takes are one page's worth, so draining
// the ranked run into a slice holds a page and not an answer. The streaming
// path above is the one that survives the unbounded walk.
func (e *Engine) rollupPackages(ctx context.Context, relations []model.Relation,
	meta *model.QueryMeta) ([]model.PackageEdge, error) {
	if len(relations) == 0 {
		return nil, nil
	}
	run, err := e.rollupRanked(ctx, meta, func(sink edgeSink) error {
		for _, r := range relations {
			if err := sink.Visit(frontierState{}, r); err != nil {
				return err
			}
		}
		return nil
	}, nil)
	if err != nil {
		return nil, err
	}
	defer run.Close()
	var out []model.PackageEdge
	if err := run.Each(func(v pairRecord) error {
		out = append(out, v.edge())
		return nil
	}); err != nil {
		return nil, err
	}
	// No MaxRecordsPerResult cut here, and no page slice either. The walk that
	// produced these relations stops at the page's item bound, so the pairs it
	// aggregates are already page-sized: `limit` edges can yield at most
	// `limit` distinct pairs. Nothing is discarded, so there is nothing to
	// disclose by count.
	return out, nil
}

// containmentCut is the containment-read cut state of ONE rollup: the nodes a
// user-set edge allowance dropped, and whether the bound has been disclosed.
// It is the caller's rather than containerPackages' own, because the rollup
// resolves its endpoints in batches and both facts are facts of the whole
// answer: disclosed once, and a dropped node dropped for good.
type containmentCut struct {
	unattributed map[model.NodeID]bool
	disclosed    bool
}

// containerPackages maps each node to the package or module node that contains
// it, in bounded batched round trips. A node that is itself a container maps to
// itself, so a package-level edge rolls up to the pair it already names.
//
// The containment read is bounded by the CONFIGURED edge allowance, which is
// unlimited by default, so the shipped configuration reads every containment
// edge a batch has. It used to be bounded by a hard-coded sixteen containers
// per node: a graph that legitimately claimed a node from more containers than
// that refused the whole rollup on a default configuration, which is a scale
// refusal rather than a policy the operator chose.
//
// A user-set allowance that stops a batch's read OMITS that batch's nodes from
// the map and discloses the bound once, the shape containerContents already
// uses. Omission is what the old refusal was protecting: a node whose candidate
// list is half-read would be attributed to the wrong package, not merely to
// fewer, and an unattributed endpoint drops its edge from the rollup -- the
// same treatment rollupPackages already gives an endpoint with no container at
// all. The pairs that remain are therefore measured pairs, and the answer says
// it is not the whole rollup.
func (e *Engine) containerPackages(ctx context.Context, ids []model.NodeID,
	meta *model.QueryMeta, state *containmentCut) (map[model.NodeID]model.Node, error) {
	ids = dedupeNodes(append([]model.NodeID(nil), ids...))
	candidates := map[model.NodeID][]model.NodeID{}
	var lookup []model.NodeID
	lookup = append(lookup, ids...)
	if state.unattributed == nil {
		state.unattributed = map[model.NodeID]bool{}
	}
	unattributed := state.unattributed
	// cut drops every candidate a stopped batch collected -- a half-read
	// candidate list is a wrong attribution, not a short one -- and discloses
	// the bound once however many batches it stops, ACROSS the whole rollup:
	// state outlives this call so a resolution split into batches discloses
	// the same once and drops the same nodes as a single-shot one.
	cut := func(batch []model.NodeID) {
		for _, id := range batch {
			delete(candidates, id)
			unattributed[id] = true
		}
		markTruncated(meta, reasonEdgeBudget)
		if state.disclosed {
			return
		}
		state.disclosed = true
		meta.Notices = appendNotice(meta.Notices,
			"max_graph_edges: the configured edge bound stopped a containment read, so the "+
				"relations of the nodes it covered are omitted from this rollup rather than "+
				"attributed to a partly-read container")
	}
	for _, batch := range impactChunkNodes(ids) {
		// Streaming, not a slice: each containment row is folded into the
		// candidate list of the node it contains as it arrives, so the heap
		// here is one adjacency page plus the candidates themselves, never the
		// whole containment fan-out of the batch.
		complete, err := e.streamContainsEdges(ctx, batch, model.DirectionIncoming,
			[]model.RelationKind{model.RelContains}, e.limits.Edges(),
			func(r model.Relation) error {
				candidates[r.To] = append(candidates[r.To], r.From)
				lookup = append(lookup, r.From)
				return nil
			})
		if err != nil {
			return nil, err
		}
		if !complete {
			cut(batch)
		}
	}
	nodes, err := e.nodesByID(ctx, lookup)
	if err != nil {
		return nil, err
	}
	out := make(map[model.NodeID]model.Node, len(ids))
	for _, id := range ids {
		if unattributed[id] {
			continue
		}
		if n, ok := nodes[id]; ok && isContainerKind(n.Kind) {
			out[id] = n
			continue
		}
		// Several containers can claim one node; the lowest NodeID wins so the
		// rollup is reproducible from the facts alone rather than from the order
		// the adjacency happened to return.
		var best *model.Node
		for _, cand := range candidates[id] {
			n, ok := nodes[cand]
			if !ok || !isContainerKind(n.Kind) {
				continue
			}
			if best == nil || n.ID < best.ID {
				chosen := n
				best = &chosen
			}
		}
		if best != nil {
			out[id] = *best
		}
	}
	return out, nil
}

// streamContainsEdges reads the containment edges of one batch of nodes in
// direction, keyset-paged by RelationID and delivered one ROW at a time, up to
// maxEdges rows. It holds one adjacency page -- adjacencyBatch rows -- and never
// the whole containment fan-out of a batch of containers, so a caller that only
// needs to fold each row (a rollup keeps one candidate list per node; the
// repository map keeps one counter per container) pays a page of heap rather
// than a level of it.
//
// It reports whether the read COMPLETED, and that flag is F7's fix: a read
// whose budget is spent EXACTLY as the rows run out is complete, not
// incomplete. The old loop tested the budget before each PAGE, so a containment
// set that exactly filled it reported incomplete and both callers refused a
// legitimately whole answer -- and, in the other direction, a page could
// deliver up to adjacencyBatch rows past the budget before the test ran. The
// budget is compared per ROW now, and only the empty page ends the walk.
func (e *Engine) streamContainsEdges(ctx context.Context, batch []model.NodeID,
	direction model.Direction, kinds []model.RelationKind, maxEdges config.Limit,
	fn func(model.Relation) error) (bool, error) {
	var (
		read  int
		after model.RelationID
	)
	for {
		rels, err := e.adjacency.Edges(ctx, batch, direction, kinds, after, adjacencyBatch)
		if err != nil {
			return false, err
		}
		// Only an empty page ends the walk: the reader clamps the requested
		// limit down to model.MaxPageItems, so testing for a short page would
		// stop after the first one and under-roll every container.
		if len(rels) == 0 {
			return true, nil
		}
		for _, r := range rels {
			if atBound(maxEdges, read) {
				// The budget is spent and a row is still standing: that, and
				// only that, is an incomplete containment read.
				return false, nil
			}
			if err := fn(r); err != nil {
				return false, err
			}
			read++
			after = r.ID
		}
	}
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
