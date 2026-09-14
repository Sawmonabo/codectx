package graph

import (
	"context"
	"sort"
	"unicode/utf8"

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
	ctx, done, err := beginImpactQuery(ctx, e)
	if err != nil {
		return model.Page[model.PackageEdge]{}, err
	}
	defer done()

	kinds := req.Relations
	if len(kinds) == 0 {
		kinds = DefaultRelations()
	}
	meta := model.QueryMeta{Binding: e.adjacency.Binding()}
	pending, err := e.pendingDependence(ctx, kinds)
	if err != nil {
		return model.Page[model.PackageEdge]{}, err
	}
	if len(pending) > 0 {
		meta.Completeness = pending
		markTruncated(&meta, reasonDependence)
	}

	b := &budget{deadline: e.now().Add(e.limits.QueryTimeout)}
	acc := newImpactAccumulator(req.Start, b,
		int64(resolveBound(req.MaxVisited, e.limits.MaxVisited)),
		int64(resolveBound(req.MaxEdges, e.limits.MaxEdges)))
	walkErr := expand(ctx, e.adjacency, acc.Seeds(), expandOptions{
		Direction: req.Direction,
		Kinds:     kinds,
		MaxDepth:  resolveBound(req.MaxDepth, e.limits.MaxDepth),
		Budget:    b,
		BatchSize: adjacencyBatch,
	}, acc.Visit)
	if err := impactPhaseError(ctx, walkErr, &meta); err != nil {
		return model.Page[model.PackageEdge]{}, err
	}
	if acc.reason != "" {
		markTruncated(&meta, acc.reason)
	}

	items, rollupErr := e.rollupPackages(ctx, acc.Relations())
	if err := impactPhaseError(ctx, rollupErr, &meta); err != nil {
		return model.Page[model.PackageEdge]{}, err
	}
	if limit := resolveBound(req.Page.Limit, e.limits.MaxPageItems); len(items) > limit {
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
	if len(out) > model.MaxRecordsPerResult {
		out = out[:model.MaxRecordsPerResult]
	}
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
		rels, err := e.containsEdges(ctx, batch)
		if err != nil {
			return nil, err
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

// maxContainerPages bounds the keyset walk over one batch's containment edges.
// A node normally has one container, so this is generous headroom rather than a
// working limit -- but the loop still has an explicit finite bound, because an
// unbounded "read until done" is exactly how a pathological graph becomes an
// unbounded query.
const maxContainerPages = 16

// containsEdges reads the incoming `contains` edges of one batch of nodes,
// keyset-paged by RelationID.
func (e *Engine) containsEdges(ctx context.Context, batch []model.NodeID) ([]model.Relation, error) {
	var (
		out   []model.Relation
		after model.RelationID
	)
	for page := 0; page < maxContainerPages; page++ {
		rels, err := e.adjacency.Edges(ctx, batch, model.DirectionIncoming,
			[]model.RelationKind{model.RelContains}, after, adjacencyBatch)
		if err != nil {
			return nil, err
		}
		out = append(out, rels...)
		if len(rels) < adjacencyBatch {
			break
		}
		after = rels[len(rels)-1].ID
	}
	return out, nil
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
func clipPath(s string) string {
	if len(s) <= model.MaxPathBytes {
		return s
	}
	cut := model.MaxPathBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}
