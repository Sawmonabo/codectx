package graph

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/Sawmonabo/codectx/internal/model"
)

// This file is the repository map behind `codectx repo-map` and
// `codectx_repo_overview`: one bounded, generation-pinned page of CONTAINER
// nodes, each carrying what it directly holds.
//
// Three rules shape it, and every decision below keeps one of them:
//
//  1. No whole-repository materialization (Section 23.4). The answer is one
//     keyset page of containers; their children are read through batched,
//     indexed adjacency under a cumulative edge budget, never hydrated one
//     node at a time and never accumulated for the whole repository.
//  2. Aggregates are DIRECT containment, not transitive. A directory's
//     transitive file count is the whole subtree, which is the repository
//     again; the direct counts compose into the same total on the consumer's
//     side without any single answer holding the tree.
//  3. An answer that could not finish says so. Where the budget bounds which
//     containers were REACHED, that is reported Truncated; where it bounds the
//     per-container counts, the map refuses outright, because a short count is
//     a wrong measurement and a repo-map that under-reports a package reads to
//     a model as the repository's shape rather than as an incomplete answer.

// overviewEndpoint is the continuation endpoint tag. A cursor minted here is
// refused by every other paging endpoint and vice versa.
const overviewEndpoint = "repo_overview"

// ContainerReader is the OPTIONAL enumeration seam an Adjacency may also
// implement, in the same shape and for the same reason as EvidenceRowReader:
// the frozen Adjacency port reads facts that HANG OFF node ids the caller
// already holds, and the repository map has no such seed -- it is the listing
// of the containers themselves.
//
// It is a separate interface rather than a widening of Adjacency so the frozen
// port stays frozen and every existing fake keeps compiling. Unlike
// EvidenceRowReader there is no degraded fallback: without it there is no
// container to report, so Overview refuses with CTX_INTERNAL naming the seam
// rather than answering "this repository contains nothing".
type ContainerReader interface {
	// Containers lists the visible container nodes of the pinned generation
	// whose kind is one of kinds, keyset-ordered by NodeID after `after`, at
	// most limit rows. One round trip per call; limit is positive.
	Containers(ctx context.Context, kinds []model.NodeKind,
		after model.NodeID, limit int) ([]model.Node, error)
}

// overviewContainerKinds is the vocabulary of the repository map: the kinds
// Section 19.2's "repository/package/module/language map" is built from.
//
// It is deliberately WIDER than isContainerKind: a rollup asks "which package
// does this symbol belong to", where a directory would answer a different
// question, while the map is the structure itself and a repository whose
// directories were omitted would not be a map of it.
func overviewContainerKinds() []model.NodeKind {
	return []model.NodeKind{model.NodeRepository, model.NodeDirectory,
		model.NodePackage, model.NodeModule, model.NodeNamespace}
}

// overviewRelationKinds is the containment vocabulary the map is counted over.
//
// `contains` alone is not it. A provider attaches a TOP-LEVEL declaration to
// the module that holds it with `defines` and reserves `contains` for a NESTED
// one (internal/provider/treesitter/facts.go), so a map counted over `contains`
// alone reports a confident SymbolCount 0 for every ordinary package -- a wrong
// measurement, which is the one thing this answer may not ship.
//
// Ancestry deliberately does not use it: `defines` never reaches a container,
// so reading it while climbing a containment chain could only add non-container
// candidates to a parent lookup that then discards them.
func overviewRelationKinds() []model.RelationKind {
	return []model.RelationKind{model.RelContains, model.RelDefines}
}

// Overview answers one page of the repository map: container nodes in NodeID
// keyset order, each with the files, symbols and source bytes it directly
// contains, its immediate container parent and its containment depth.
//
// Depth is inclusive and 0-based: a root container is depth 0, and a request
// asking for depth 2 receives depths 0, 1 and 2. A zero Depth means the
// engine's configured MaxDepth, never "unlimited", exactly as every other
// bound on this engine.
func (e *Engine) Overview(ctx context.Context, req model.OverviewRequest) (page model.Page[model.OverviewItem], err error) {
	defer func() { err = typedContextError(ctx, err) }()
	if err := req.Validate(); err != nil {
		return model.Page[model.OverviewItem]{}, err
	}
	reader, ok := e.adjacency.(ContainerReader)
	if !ok {
		return model.Page[model.OverviewItem]{}, &model.Error{Code: model.CodeInternal,
			Message:     "the pinned reader for this workspace does not enumerate container nodes",
			Remediation: "this is a wiring defect; report it with the command you ran"}
	}
	ctx, deadline, done, err := beginImpactQuery(ctx, e)
	if err != nil {
		return model.Page[model.OverviewItem]{}, err
	}
	defer done()

	limit := resolveBound(req.Page.Limit, e.limits.MaxPageItems)
	maxDepth := resolveBound(req.Depth, e.limits.MaxDepth)
	// The cursor is bound to the normalized query the way every traversal
	// cursor is: a token issued for one depth or page size is refused by
	// another rather than silently answering a different question. This
	// endpoint reads one fixed containment vocabulary and has no seeds, so
	// those two components are the constant and the empty list.
	queryHash := traversalQueryHash(model.DirectionOutgoing,
		overviewRelationKinds(), nil, maxDepth, limit)

	b := &budget{deadline: deadline, now: e.now}
	var after model.NodeID
	if req.Page.Cursor != "" {
		c, resumed, err := e.verifyContinuation(req.Page.Cursor, overviewEndpoint, queryHash, deadline)
		if err != nil {
			return model.Page[model.OverviewItem]{}, err
		}
		// The keyset position is the last container the issuing page listed;
		// the cumulative edge budget rides along, so paging the whole map
		// cannot cost more adjacency reads than one walk of it.
		after, b = c.LastOwner, resumed
		// The continuation is consumed here, at the point its state is in
		// memory, exactly as resumeTraversal consumes a pure keyset one: this
		// endpoint mints a lease per page and spills no spool, so holding the
		// replayed lease until the cursor TTL would pin a generation against
		// retention for state nothing will read again -- one lease per page of
		// every walk of the map. Presenting the same token twice is therefore
		// CTX_CURSOR_INVALID rather than a replayed page, the same deliberate
		// trade every other continuation makes.
		e.releaseConsumed(ctx, c.SpoolID, c.LeaseID)
	}

	meta := model.QueryMeta{Binding: e.adjacency.Binding()}
	// The deferred flag is discarded deliberately: it reports that a
	// DEPENDENCE-only kind was asked for while its units are still building,
	// and both kinds this map counts are canonical ones no provider defers. The
	// capability rows themselves still ride on every answer.
	caps, _, err := e.completeness(ctx, overviewRelationKinds())
	if err != nil {
		return model.Page[model.OverviewItem]{}, err
	}
	meta.Completeness = caps

	containers, err := reader.Containers(ctx, overviewContainerKinds(), after, limit)
	if err != nil {
		return model.Page[model.OverviewItem]{}, err
	}
	ids := make([]model.NodeID, 0, len(containers))
	for _, c := range containers {
		ids = append(ids, c.ID)
	}
	parents, depths, err := e.containerAncestry(ctx, ids, maxDepth, b, &meta)
	if err != nil {
		return model.Page[model.OverviewItem]{}, err
	}
	// Unlike Neighbors and PackageDependencies, a failed or exhausted
	// containment read is NOT turned into a truncated answer here
	// (impactPhaseError's deadline-to-reasonDeadline path). Those endpoints
	// report the edges they did read; this one reports COUNTS, and a container
	// whose children were only half read renders as a smaller container -- a
	// wrong measurement rather than a missing one, which is the one thing
	// Section 23 forbids above all. A map that could not be counted refuses
	// instead, which is what containerContents does on an exhausted budget.
	held, err := e.containerContents(ctx, ids, b)
	if err != nil {
		return model.Page[model.OverviewItem]{}, err
	}

	items := make([]model.OverviewItem, 0, len(containers))
	for _, c := range containers {
		depth, ok := depths[c.ID]
		if !ok {
			// Its containment chain is longer than the requested depth: the
			// page reports the levels that were asked for and nothing else.
			continue
		}
		counts := held[c.ID]
		path, name := containerLabels(c)
		items = append(items, model.OverviewItem{
			NodeID:       c.ID,
			Kind:         c.Kind,
			Path:         path,
			Name:         name,
			Language:     c.Language,
			Depth:        depth,
			FileCount:    counts.files,
			SymbolCount:  counts.symbols,
			SourceBytes:  counts.bytes,
			ParentNodeID: parents[c.ID],
		})
	}

	// A short enumeration page is the end of the map; a full one may not be,
	// so the answer offers the container after the last one it listed. Items
	// dropped by the depth filter do not change that: the keyset is the
	// enumeration, not the filtered page.
	if len(containers) == limit {
		token, err := e.nextTraversalCursor(ctx, b, continuation{
			Endpoint:  overviewEndpoint,
			QueryHash: queryHash,
			LastOwner: ids[len(ids)-1],
			// The frozen payload keeps a keyset position as an (owner, key)
			// PAIR and rejects a half-formed one. This endpoint's keyset is a
			// single container NodeID -- it pages over nodes, not over the
			// edges of one node -- so the same id rides in both halves and
			// only LastOwner is ever read back.
			LastKey: model.RelationID(ids[len(ids)-1]),
		})
		if err != nil {
			return model.Page[model.OverviewItem]{}, err
		}
		if token == "" {
			markTruncated(&meta, reasonPageFull)
		}
		meta.NextCursor = token
	}

	page = model.Page[model.OverviewItem]{Meta: meta, Items: items}
	if err := page.Validate(); err != nil {
		return model.Page[model.OverviewItem]{}, err
	}
	for _, item := range items {
		if err := item.Validate(); err != nil {
			return model.Page[model.OverviewItem]{}, err
		}
	}
	return page, nil
}

// containerAncestry resolves the immediate container parent and the containment
// depth of every container on the page, one LEVEL at a time: each round reads
// the incoming containment of a whole level in batched round trips, so a page
// of 200 containers costs one read per level and not one per container.
//
// A container whose chain is still climbing after maxDepth levels is absent
// from the returned depths, which is how the caller drops it: it sits deeper
// than the request asked for. The level bound is also what makes a containment
// cycle finite rather than a hang.
func (e *Engine) containerAncestry(ctx context.Context, ids []model.NodeID, maxDepth int,
	b *budget, meta *model.QueryMeta) (map[model.NodeID]model.NodeID, map[model.NodeID]int, error) {
	parents := make(map[model.NodeID]model.NodeID, len(ids))
	depths := make(map[model.NodeID]int, len(ids))
	// climbing maps each page container to the ancestor its chain has reached.
	climbing := make(map[model.NodeID]model.NodeID, len(ids))
	for _, id := range ids {
		depths[id] = 0
		climbing[id] = id
	}
	for level := 0; level <= maxDepth && len(climbing) > 0; level++ {
		frontier := make([]model.NodeID, 0, len(climbing))
		for _, anc := range climbing {
			frontier = append(frontier, anc)
		}
		up, err := e.containerParents(ctx, dedupeNodes(frontier), b, meta)
		if err != nil {
			return nil, nil, err
		}
		next := make(map[model.NodeID]model.NodeID, len(climbing))
		for id, anc := range climbing {
			parent, ok := up[anc]
			if !ok {
				continue // anc is a root; this chain is complete
			}
			if level == 0 {
				parents[id] = parent
			}
			depths[id]++
			next[id] = parent
		}
		climbing = next
	}
	// Whatever is still climbing is deeper than the request asked for.
	for id := range climbing {
		delete(depths, id)
		delete(parents, id)
	}
	return parents, depths, nil
}

// containerParents maps each id to the container node that directly contains
// it, in bounded batched round trips.
//
// Several containers can claim one node -- a file sits in a directory and in a
// package -- so the lowest NodeID wins, the same tie-break containerPackages
// applies, so the map is reproducible from the facts rather than from the order
// the adjacency happened to return them in.
//
// An exhausted budget is reported as Truncated here rather than refused: what
// is lost is which containers the page REACHED and at what depth, and a page
// that says it is not the whole map is an honest answer. It is the per-
// container COUNTS that may not be short, and containerContents refuses for
// exactly that reason.
func (e *Engine) containerParents(ctx context.Context, ids []model.NodeID,
	b *budget, meta *model.QueryMeta) (map[model.NodeID]model.NodeID, error) {
	candidates := map[model.NodeID][]model.NodeID{}
	lookup := make([]model.NodeID, 0, len(ids))
	for _, batch := range impactChunkNodes(ids) {
		rels, complete, err := e.containsEdges(ctx, batch, model.DirectionIncoming,
			[]model.RelationKind{model.RelContains}, containmentBudget(b, e.limits))
		if err != nil {
			return nil, err
		}
		b.edges += int64(len(rels))
		if !complete {
			markTruncated(meta, reasonEdgeBudget)
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
	out := make(map[model.NodeID]model.NodeID, len(ids))
	for _, id := range ids {
		var best model.NodeID
		for _, cand := range candidates[id] {
			n, ok := nodes[cand]
			if !ok || !isOverviewContainer(n.Kind) {
				continue
			}
			if best == "" || n.ID < best {
				best = n.ID
			}
		}
		if best != "" {
			out[id] = best
		}
	}
	return out, nil
}

// containerCounts is what one container directly holds.
type containerCounts struct{ files, symbols, bytes int64 }

// containerContents aggregates the DIRECT children of every container on the
// page: the files it holds, the symbols declared in it, and the source bytes
// those files carry. Children are read in batched containment round trips and
// hydrated in batches, never one node at a time, and the whole page shares one
// cumulative edge budget.
//
// A container tree that outruns that budget REFUSES with CTX_RESOURCE_LIMIT.
// Nothing here can be reported instead: the counts already accumulated are
// short by an unknown amount, and a page flag naming no container would leave
// every number on it indistinguishable from a measured one.
func (e *Engine) containerContents(ctx context.Context, ids []model.NodeID,
	b *budget) (map[model.NodeID]containerCounts, error) {
	out := make(map[model.NodeID]containerCounts, len(ids))
	for _, batch := range impactChunkNodes(ids) {
		rels, complete, err := e.containsEdges(ctx, batch, model.DirectionOutgoing,
			overviewRelationKinds(), containmentBudget(b, e.limits))
		if err != nil {
			return nil, err
		}
		b.edges += int64(len(rels))
		if !complete {
			return nil, (&model.Error{Code: model.CodeResourceLimit,
				Message:     "this generation holds more containment edges under one page of containers than a repository map may read, so its per-container counts cannot be measured",
				Remediation: "narrow the request scope"}).WithDetail("limit", "edges")
		}
		children := make([]model.NodeID, 0, len(rels))
		for _, r := range rels {
			children = append(children, r.To)
		}
		nodes, err := e.nodesByID(ctx, children)
		if err != nil {
			return nil, err
		}
		for _, r := range rels {
			child, ok := nodes[r.To]
			if !ok {
				// A child that is not visible in this generation is not
				// counted: an aggregate must rest on facts this answer could
				// actually read.
				continue
			}
			counts := out[r.From]
			switch {
			case child.Kind == model.NodeFile:
				counts.files++
				counts.bytes += nodeSourceBytes(child)
			case isOverviewContainer(child.Kind):
				// A nested container is structure, not a symbol; it appears in
				// the map as its own item with its own counts.
			default:
				counts.symbols++
			}
			out[r.From] = counts
		}
	}
	return out, nil
}

// containmentBudget is what is LEFT of the page's cumulative edge allowance.
// The whole page shares one allowance, so a single pathological container
// cannot make one answer read more adjacency than a walk of the same size
// would have been allowed to.
func containmentBudget(b *budget, limits Limits) int64 {
	left := int64(limits.MaxEdges) - b.edges
	if left < 0 {
		return 0
	}
	return left
}

// containerLabels is the operator-facing path and name of one container.
//
// Both are the container's CANONICAL qualified name -- its own Name is a bare
// label a provider is free to spell the way its language quotes a scope, and a
// Go package scope reaches this map backquoted and slash-terminated, the way
// the scope itself is spelled, while a standard-library one reaches it as
// "fmt/". A repo-map row is read as
// the container's identity by a person and by a model, so the quoting is
// stripped HERE, at the producer: doing it in the CLI would leave the JSON and
// the MCP answer spelling the same container two other ways, and doing it
// nowhere publishes provider syntax as a repository's structure.
//
// The name also loses the trailing separator the scope carries -- a name is not
// a prefix -- while the path keeps it, because that is what a path is. A label
// that is nothing BUT quoting is kept as it stands: both fields are required,
// and an empty column reads as a name that failed to render.
func containerLabels(n model.Node) (path, name string) {
	label := packageLabel(n)
	clean := stripQuoting(label)
	if clean == "" {
		clean = label
	}
	name = strings.TrimRight(clean, "/")
	if name == "" {
		name = clean
	}
	return clipPath(clean), clipTo(name, model.MaxNameBytes)
}

// stripQuoting removes the quoting characters a provider may have spelled a
// scope with. It is deliberately a removal and not an escape: the quotes carry
// no information the reader of a map needs, and an escaped one would still read
// as part of the container's name.
func stripQuoting(s string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case '`', '\'', '"':
			return -1
		}
		return r
	}, s)
}

// isOverviewContainer reports whether a node kind is part of the repository
// map. See overviewContainerKinds for why it is wider than isContainerKind.
func isOverviewContainer(k model.NodeKind) bool {
	switch k {
	case model.NodeRepository, model.NodeDirectory, model.NodePackage,
		model.NodeModule, model.NodeNamespace:
		return true
	}
	return false
}

// nodeSourceBytes is the size a file node carries in its typed metadata, which
// is the only byte fact the graph port exposes: the filesystem provider records
// it there when it emits the node (internal/provider/filesystem, "size").
//
// A node whose metadata carries no size contributes nothing rather than a
// guess, and a negative or unreadable one is treated the same way: an
// over-reported byte total is a wrong measurement, not a missing one.
func nodeSourceBytes(n model.Node) int64 {
	if len(n.Metadata) == 0 {
		return 0
	}
	var meta struct {
		Size int64 `json:"size"`
	}
	if err := json.Unmarshal(n.Metadata, &meta); err != nil || meta.Size < 0 {
		return 0
	}
	return meta.Size
}
