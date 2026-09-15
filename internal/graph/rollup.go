package graph

import (
	"context"
	"fmt"
	"sort"
	"unicode/utf8"

	"github.com/Sawmonabo/codectx/internal/config"
	"github.com/Sawmonabo/codectx/internal/model"
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
	if err := continuationUnavailable(req.Page.Cursor); err != nil {
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
	meta.Notices = appendNotice(appendNotice(appendNotice(meta.Notices, visitedNotice), edgeNotice), depthNotice)
	acc := newImpactAccumulator(req.Start, b, maxVisited, maxEdges)
	state, walkErr := expand(ctx, e.adjacency, acc.Seeds(), expandOptions{
		Direction:     req.Direction,
		Kinds:         kinds,
		MaxDepth:      maxDepth,
		Budget:        b,
		BatchSize:     adjacencyBatch,
		FrontierBytes: e.limits.FrontierBytes,
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

	items, rollupErr := e.rollupPackages(ctx, acc.Relations())
	if err := impactPhaseError(ctx, rollupErr, &meta); err != nil {
		return model.Page[model.PackageEdge]{}, err
	}
	limit, limitNotice := resolvePageItems(req.Page.Limit, e.limits.MaxPageItems)
	meta.Notices = appendNotice(meta.Notices, limitNotice)
	if len(items) > limit {
		// The pairs beyond this page are DISCLOSED, not dropped in silence:
		// PackageDependencies offers no continuation (continuationUnavailable),
		// so the count of what the page could not carry is the only honest
		// channel there is.
		meta.Notices = appendNotice(meta.Notices,
			fmt.Sprintf("package rollup: %d further package pair(s) were aggregated and are not listed on this page",
				len(items)-limit))
		items = items[:limit]
		markTruncated(&meta, reasonPageFull)
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

// rollupPackages aggregates symbol-level relations into distinct package pairs.
// It is the shared body behind ImpactResult.Packages and the standalone
// PackageDependencies, so the two can never disagree about what a pair means.
//
// An edge whose endpoints share a container contributes nothing: a package
// depending on itself is not a dependency, and reporting it would drown the
// real cross-package pairs. An edge with no resolvable container is dropped
// rather than attributed to a guessed package.
func (e *Engine) rollupPackages(ctx context.Context, relations []model.Relation) ([]model.PackageEdge, error) {
	if len(relations) == 0 {
		return nil, nil
	}
	endpoints := make([]model.NodeID, 0, 2*len(relations))
	relationIDs := make([]model.RelationID, 0, len(relations))
	for _, r := range relations {
		endpoints = append(endpoints, r.From, r.To)
		relationIDs = append(relationIDs, r.ID)
	}
	containers, err := e.containerPackages(ctx, endpoints)
	if err != nil {
		return nil, err
	}
	evidence, err := e.evidenceCounts(ctx, relationIDs)
	if err != nil {
		return nil, err
	}

	type pairKey struct{ from, to model.NodeID }
	pairs := map[pairKey]*model.PackageEdge{}
	var order []pairKey
	for _, r := range relations {
		from, okFrom := containers[r.From]
		to, okTo := containers[r.To]
		if !okFrom || !okTo || from.ID == to.ID {
			continue
		}
		key := pairKey{from: from.ID, to: to.ID}
		edge, ok := pairs[key]
		if !ok {
			edge = &model.PackageEdge{
				FromNodeID: from.ID, ToNodeID: to.ID,
				FromPath: clipPath(packageLabel(from)), ToPath: clipPath(packageLabel(to)),
			}
			pairs[key] = edge
			order = append(order, key)
		}
		edge.PairCount++
		edge.EvidenceCount += evidence[r.ID]
	}

	out := make([]model.PackageEdge, 0, len(order))
	for _, key := range order {
		out = append(out, *pairs[key])
	}
	sort.Slice(out, func(i, j int) bool {
		x, y := out[i], out[j]
		if x.FromPath != y.FromPath {
			return x.FromPath < y.FromPath
		}
		if x.ToPath != y.ToPath {
			return x.ToPath < y.ToPath
		}
		if x.FromNodeID != y.FromNodeID {
			return x.FromNodeID < y.FromNodeID
		}
		return x.ToNodeID < y.ToNodeID
	})
	// No MaxRecordsPerResult cut here. It used to discard the tail of the
	// aggregation with a comment saying it could not disclose the loss, which
	// made the page-overflow notice below understate what the answer held.
	// The aggregate is over ONE page's edges -- the per-page edge and visited
	// budgets bound what the walk read -- so the pair set is already
	// page-sized, and the page slice is the only cut, disclosed by count.
	return out, nil
}

// containerPackages maps each node to the package or module node that contains
// it, in bounded batched round trips. A node that is itself a container maps to
// itself, so a package-level edge rolls up to the pair it already names.
func (e *Engine) containerPackages(ctx context.Context, ids []model.NodeID) (map[model.NodeID]model.Node, error) {
	ids = dedupeNodes(append([]model.NodeID(nil), ids...))
	candidates := map[model.NodeID][]model.NodeID{}
	var lookup []model.NodeID
	lookup = append(lookup, ids...)
	for _, batch := range impactChunkNodes(ids) {
		rels, complete, err := e.containsEdges(ctx, batch, model.DirectionIncoming,
			[]model.RelationKind{model.RelContains}, config.Limit(int64(len(batch))*maxContainersPerNode))
		if err != nil {
			return nil, err
		}
		if !complete {
			// A rollup cannot disclose truncation: its callers hand it a
			// relation list and take a pair list back. Refusing loudly is the
			// only honest answer left -- a rollup built on half the containment
			// would attribute edges to the wrong packages, not merely to fewer.
			return nil, (&model.Error{Code: model.CodeResourceLimit,
				Message:     "this generation contains more containment edges for one batch of nodes than a rollup may read",
				Remediation: "narrow the request scope"}).WithDetail("limit", "containers_per_node")
		}
		for _, r := range rels {
			candidates[r.To] = append(candidates[r.To], r.From)
			lookup = append(lookup, r.From)
		}
	}
	nodes, err := e.nodesByID(ctx, lookup)
	if err != nil {
		return nil, err
	}
	out := make(map[model.NodeID]model.Node, len(ids))
	for _, id := range ids {
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

// maxContainersPerNode bounds how many containers one node may be read as
// belonging to. A node legitimately sits in several -- a file is in a directory
// and in a package -- but a graph in which one node is claimed by sixteen is
// pathological, and an unbounded "read until done" is exactly how such a graph
// becomes an unbounded query.
//
// It replaces a flat page ceiling that did not scale with the batch: sixteen
// clamped pages carry 3200 containment rows whatever the batch size, so a batch
// of 256 nodes silently stopped at twelve containers per node while a batch of
// four was allowed eight hundred. The budget below is per node, so a batch
// reads what its own size needs and the loop still has an explicit finite
// bound.
const maxContainersPerNode = 16

// containsEdges reads the containment edges of one batch of nodes in direction,
// keyset-paged by RelationID, up to maxEdges rows.
//
// kinds is the containment vocabulary the caller counts over, and it is a
// parameter rather than a constant because the two callers mean different
// things by "contained": a rollup and an ancestry climb ask which CONTAINER
// holds a node, which only `contains` answers, while the repository map counts
// what a container holds and a top-level declaration hangs off its module with
// `defines` (see overviewRelationKinds).
//
// It reports whether the read COMPLETED. The caller decides what an incomplete
// containment read means for its answer -- both a rollup and the repository
// map's counts refuse -- because the one thing neither may do is report a
// partial containment as the whole of it: a package that silently loses half
// its members reads as a smaller package, not as an incomplete answer.
func (e *Engine) containsEdges(ctx context.Context, batch []model.NodeID,
	direction model.Direction, kinds []model.RelationKind, maxEdges config.Limit) ([]model.Relation, bool, error) {
	var (
		out   []model.Relation
		after model.RelationID
	)
	for !atBound(maxEdges, len(out)) {
		rels, err := e.adjacency.Edges(ctx, batch, direction, kinds, after, adjacencyBatch)
		if err != nil {
			return nil, false, err
		}
		out = append(out, rels...)
		// Only an empty page ends the walk: the reader clamps the requested
		// limit down to model.MaxPageItems, so testing for a short page would
		// stop after the first one and under-roll every container.
		if len(rels) == 0 {
			return out, true, nil
		}
		after = rels[len(rels)-1].ID
	}
	return out, false, nil
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
