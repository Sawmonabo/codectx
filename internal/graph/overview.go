package graph

import (
	"context"
	"slices"
	"strings"

	"github.com/Sawmonabo/codectx/internal/config"
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
// It is deliberately WIDER than the rollup's container vocabulary (packages
// and modules): a rollup asks "which package does this symbol belong to",
// where a directory would answer a different question, while the map is the
// structure itself and a repository whose directories were omitted would not
// be a map of it.
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

	limit, limitNotice := resolvePageItems(req.Page.Limit, e.limits.MaxPageItems)
	// Depth goes through resolveLimit, not resolveBound: an unlimited
	// configured depth read as the integer 0 would climb exactly one level and
	// say nothing about it, and a request narrowed by the configuration is
	// disclosed rather than silently clamped.
	maxDepth, depthNotice := resolveLimit("max_depth", req.Depth, e.limits.Depth())
	// The cursor is bound to the normalized query the way every traversal
	// cursor is: a token issued for one depth or page size is refused by
	// another rather than silently answering a different question. This
	// endpoint reads one fixed containment vocabulary and has no seeds, so
	// those two components are the constant and the empty list.
	queryHash := traversalQueryHash(model.DirectionOutgoing,
		overviewRelationKinds(), nil, maxDepth.Int(), limit)

	b := &budget{deadline: deadline, now: e.now}
	var after model.NodeID
	if req.Page.Cursor != "" {
		c, resumed, err := e.verifyContinuation(req.Page.Cursor, overviewEndpoint, queryHash, deadline)
		if err != nil {
			return model.Page[model.OverviewItem]{}, err
		}
		// The enumeration position is the last container the issuing page
		// listed, carried as the reader's own surrogate; the cumulative edge
		// budget rides along, so paging the whole map cannot cost more
		// adjacency reads than one walk of it.
		b = resumed
		if after, err = e.containerAfter(ctx, c.LastNode); err != nil {
			return model.Page[model.OverviewItem]{}, err
		}
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
	meta.Notices = appendNotice(appendNotice(meta.Notices, depthNotice), limitNotice)
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
	// This endpoint reports COUNTS, and a container whose children were only
	// half read renders as a smaller container -- a wrong measurement rather
	// than a missing one, which is the one thing Section 23 forbids above all.
	// A user-set edge bound that cuts a containment read therefore OMITS the
	// containers it could not measure and says so, rather than publishing a
	// half count. It does not refuse the map: refusing turned one exhausted
	// bound into no answer at all, with "narrow the request scope" as the only
	// remedy, which is exactly the shape the scale posture forbids.
	held, unmeasured, err := e.containerContents(ctx, ids, b, &meta)
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
		if unmeasured[c.ID] {
			// Its containment read was cut by the configured edge bound. The
			// map omits it rather than under-counting it; meta says so.
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
		last, err := e.containerPosition(ctx, ids[len(ids)-1])
		if err != nil {
			return model.Page[model.OverviewItem]{}, err
		}
		token, err := e.nextTraversalCursor(ctx, b, continuation{
			Endpoint:  overviewEndpoint,
			QueryHash: queryHash,
			LastNode:  last,
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

// containerPosition is the reader position a continuation carries for the last
// container a page listed. The enumeration itself is ordered by canonical id --
// surrogates are assigned in intern order and say nothing about it -- so the
// position is resolved at MINT time and named back at resume time; what travels
// in the token is the generation-local surrogate every other traversal position
// is written in, and the cursor's generation fence covers it.
//
// It runs on a context DETACHED from the request deadline for the reason
// nextTraversalCursor does (cursor.go): minting is the LAST thing a page that
// ran out of time does, so charged against the expired context this read would
// fail with the context's own error and a page the engine had decided to
// continue would come back with nothing to resume from. It is one batched
// primary-key read of a single id, not another enumeration.
func (e *Engine) containerPosition(ctx context.Context, id model.NodeID) (NodeRef, error) {
	ctx = context.WithoutCancel(ctx)
	reader, err := e.consumerReader()
	if err != nil {
		return 0, err
	}
	refs, err := reader.Resolve(ctx, []model.NodeID{id})
	if err != nil {
		return 0, err
	}
	if len(refs) != 1 || refs[0] == 0 {
		// The enumeration listed it from this same pinned generation, so a
		// reader that cannot resolve it disagrees with itself.
		return 0, internalErr("graph: the packed reader does not carry a container the map just listed")
	}
	return refs[0], nil
}

// containerAfter names the container a continuation resumes after. A surrogate
// the pinned generation does not carry is a token from another generation that
// the fence let through, which is a refusal and never an enumeration from the
// beginning: restarting would serve the first page again as the second.
func (e *Engine) containerAfter(ctx context.Context, ref NodeRef) (model.NodeID, error) {
	if ref == 0 {
		return "", nil
	}
	reader, err := e.consumerReader()
	if err != nil {
		return "", err
	}
	ids, err := reader.NodeIDs(ctx, []NodeRef{ref})
	if err != nil {
		return "", err
	}
	if len(ids) != 1 || ids[0] == "" {
		return "", cursorInvalid("cursor names a container this generation does not carry")
	}
	return ids[0], nil
}

// containerAncestry resolves the immediate container parent and the containment
// depth of every container on the page, one LEVEL at a time: each round reads
// the incoming containment of a whole level in batched round trips, so a page
// of 200 containers costs one read per level and not one per container.
//
// A container whose chain is still climbing after maxDepth levels is absent
// from the returned depths, which is how the caller drops it: it sits deeper
// than the request asked for.
//
// A containment CYCLE is ended by the per-chain ancestor set, not by the level
// bound, because the level bound is now unlimited by default and a cycle under
// an unlimited bound is a hang rather than a short answer. A chain that climbs
// back to an ancestor it has already passed can never reach a root, so it is
// dropped exactly as an over-deep one is. The set holds one node identity per
// level actually climbed, per container on THIS page, so its peak is a
// function of the page size and the real chain depth.
func (e *Engine) containerAncestry(ctx context.Context, ids []model.NodeID, maxDepth config.Limit,
	b *budget, meta *model.QueryMeta) (map[model.NodeID]model.NodeID, map[model.NodeID]int, error) {
	parents := make(map[model.NodeID]model.NodeID, len(ids))
	depths := make(map[model.NodeID]int, len(ids))
	// climbing maps each page container to the ancestor its chain has reached.
	climbing := make(map[model.NodeID]model.NodeID, len(ids))
	for _, id := range ids {
		depths[id] = 0
		climbing[id] = id
	}
	seen := make(map[model.NodeID]map[model.NodeID]struct{}, len(ids))
	for _, id := range ids {
		seen[id] = map[model.NodeID]struct{}{id: {}}
	}
	cyclic := make(map[model.NodeID]struct{})
	for level := 0; !maxDepth.Exceeded(int64(level)) && len(climbing) > 0; level++ {
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
			if _, repeated := seen[id][parent]; repeated {
				cyclic[id] = struct{}{}
				continue
			}
			seen[id][parent] = struct{}{}
			if level == 0 {
				parents[id] = parent
			}
			depths[id]++
			next[id] = parent
		}
		climbing = next
	}
	// Whatever is still climbing is deeper than the request asked for, and a
	// chain that closed a cycle has no root to reach at any depth.
	for id := range climbing {
		delete(depths, id)
		delete(parents, id)
	}
	for id := range cyclic {
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
	reader, err := e.consumerReader()
	if err != nil {
		return nil, err
	}
	contains, ok := reader.Kinds().Code(model.RelContains)
	if !ok {
		// The generation seals no containment at all, so nothing has a parent.
		// That is an answer, not a failure: the map still lists its containers,
		// each at depth 0.
		return map[model.NodeID]model.NodeID{}, nil
	}
	// best is the lowest-canonical-id container parent found so far, per child
	// surrogate. Its bound is the batch, not the fan-out: a candidate is folded
	// into it as the scan delivers it and no candidate list is kept.
	best := map[NodeRef]model.NodeID{}
	// The child of a containment edge is the node the scan OWNS, so the
	// candidate is the neighbour; both its kind and its canonical id are needed
	// to apply the tie-break, and both come from one batched side-array read
	// per chunk of candidates rather than one hydration per candidate.
	for _, chunk := range chunkRefs(sortedRefs(ctx, reader, ids)) {
		remaining, exhausted := containmentBudget(b, e.limits)
		if exhausted {
			markTruncated(meta, reasonEdgeBudget)
			break
		}
		complete, err := scanNeighbours(ctx, reader, chunk, model.DirectionIncoming,
			[]KindCode{contains}, remaining, b, func(owners, candidates []NodeRef) error {
				kinds, err := reader.NodeKinds(ctx, candidates)
				if err != nil {
					return err
				}
				cids, err := reader.NodeIDs(ctx, candidates)
				if err != nil {
					return err
				}
				for i := range candidates {
					if !isOverviewContainer(kinds[i]) || cids[i] == "" {
						continue
					}
					// Several containers can claim one node -- a file sits in a
					// directory and in a package -- so the LOWEST canonical id
					// wins, the tie-break the rollup's container side array
					// applies, so the map is reproducible from the facts rather
					// than from the order the adjacency returned them in.
					if cur, seen := best[owners[i]]; !seen || cids[i] < cur {
						best[owners[i]] = cids[i]
					}
				}
				return nil
			})
		if err != nil {
			return nil, err
		}
		if !complete {
			markTruncated(meta, reasonEdgeBudget)
		}
	}
	out := make(map[model.NodeID]model.NodeID, len(ids))
	refs, err := reader.Resolve(ctx, ids)
	if err != nil {
		return nil, err
	}
	for i, id := range ids {
		if parent, ok := best[refs[i]]; ok {
			out[id] = parent
		}
	}
	return out, nil
}

// sortedRefs resolves ids to surrogates and returns them ascending and
// duplicate-free, dropping any id the generation does not publish. That is
// GraphReader.Neighbours' precondition, and it is enforced here rather than
// assumed: an unsorted or repeated frontier yields a silently wrong page --
// an edge delivered twice, or one skipped by the resume position -- instead of
// an error.
//
// A resolution failure yields no refs; the caller's own read of the reader
// surfaces the error, and every caller here resolves again for its own output
// map, so a swallowed failure cannot reach an answer as "this container holds
// nothing".
func sortedRefs(ctx context.Context, reader GraphReader, ids []model.NodeID) []NodeRef {
	refs, err := reader.Resolve(ctx, ids)
	if err != nil {
		return nil
	}
	out := make([]NodeRef, 0, len(refs))
	for _, ref := range refs {
		if ref != 0 {
			out = append(out, ref)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// chunkRefs splits an ascending ref list into scan-sized chunks, so the
// working set of one containment scan is a function of adjacencyBatch and
// never of the page.
func chunkRefs(refs []NodeRef) [][]NodeRef {
	var out [][]NodeRef
	for len(refs) > adjacencyBatch {
		out = append(out, refs[:adjacencyBatch])
		refs = refs[adjacencyBatch:]
	}
	if len(refs) > 0 {
		out = append(out, refs)
	}
	return out
}

// scanNeighbours streams one chunk's packed adjacency and hands it to fn in
// batches of at most adjacencyBatch entries, as two aligned slices: the owner
// of each entry and its neighbour. Batching is what lets a consumer read the
// side arrays of a whole batch of neighbours in one indexed read instead of
// one per entry, which is the whole point of the packed layout.
//
// maxEdges is the allowance LEFT of the page's cumulative edge budget; every
// delivered entry is charged to b. It reports whether the scan COMPLETED: a
// scan whose allowance is spent exactly as the entries run out is complete,
// and only an allowance that still has an entry standing in front of it is
// not. A caller turns an incomplete scan into the truncation its own answer
// requires -- the map omits the containers it could not measure -- rather than
// publishing a half-read count.
func scanNeighbours(ctx context.Context, reader GraphReader, chunk []NodeRef,
	direction model.Direction, kinds []KindCode, maxEdges config.Limit, b *budget,
	fn func(owners, neighbours []NodeRef) error) (bool, error) {
	owners := make([]NodeRef, 0, adjacencyBatch)
	neighbours := make([]NodeRef, 0, adjacencyBatch)
	flush := func() error {
		if len(owners) == 0 {
			return nil
		}
		if err := fn(owners, neighbours); err != nil {
			return err
		}
		owners, neighbours = owners[:0], neighbours[:0]
		return nil
	}
	var read int64
	complete := true
	var ferr error
	if _, err := reader.Neighbours(ctx, chunk, direction, kinds, EdgePos{}, func(e Edge) error {
		if maxEdges.Exceeded(read + 1) {
			// The allowance is spent and an entry is still standing: that, and
			// only that, is an incomplete read.
			complete = false
			return ErrStopScan
		}
		read++
		b.edges++
		owners = append(owners, e.Owner)
		neighbours = append(neighbours, e.Neighbour)
		if len(owners) < adjacencyBatch {
			return nil
		}
		if ferr = flush(); ferr != nil {
			return ErrStopScan
		}
		return nil
	}); err != nil {
		return false, err
	}
	if ferr != nil {
		return false, ferr
	}
	if err := flush(); err != nil {
		return false, err
	}
	return complete, nil
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
	b *budget, meta *model.QueryMeta) (map[model.NodeID]containerCounts,
	map[model.NodeID]bool, error) {
	reader, err := e.consumerReader()
	if err != nil {
		return nil, nil, err
	}
	out := make(map[model.NodeID]containerCounts, len(ids))
	unmeasured := map[model.NodeID]bool{}
	refs, err := reader.Resolve(ctx, ids)
	if err != nil {
		return nil, nil, err
	}
	// byRef is how a scan's owner surrogate becomes the canonical id the
	// answer is keyed by. A container the generation does not publish resolves
	// to the zero surrogate and simply owns no entries.
	byRef := make(map[NodeRef]model.NodeID, len(ids))
	for i, id := range ids {
		if refs[i] != 0 {
			byRef[refs[i]] = id
		}
	}
	kinds := make([]KindCode, 0, len(overviewRelationKinds()))
	for _, k := range overviewRelationKinds() {
		if code, ok := reader.Kinds().Code(k); ok {
			kinds = append(kinds, code)
		}
	}
	if len(kinds) == 0 {
		// The generation seals none of the containment vocabulary, so every
		// container on the page directly holds nothing. That is a measured
		// zero, not an unmeasured container.
		return out, unmeasured, nil
	}
	// cut marks every container of a chunk whose containment read the edge
	// bound stopped, and discloses the bound once. Those containers are
	// omitted from the map: an omitted container is a missing measurement,
	// which the caller can see and page past, while a half-counted one is a
	// wrong measurement it cannot tell from a small container.
	disclosed := false
	cut := func(chunk []NodeRef) {
		for _, ref := range chunk {
			id, ok := byRef[ref]
			if !ok {
				continue
			}
			unmeasured[id] = true
			// Retract whatever entries of this chunk were already counted: a
			// half-counted container is a wrong measurement, and the caller
			// cannot tell one from a small container.
			delete(out, id)
		}
		markTruncated(meta, reasonEdgeBudget)
		if disclosed {
			// One bound, one disclosure: a map whose every chunk was cut must
			// not repeat the same notice once per chunk.
			return
		}
		disclosed = true
		meta.Notices = appendNotice(meta.Notices,
			"max_graph_edges: the configured edge bound stopped a containment read, so the "+
				"containers it covered are omitted from this page rather than counted in part")
	}
	for _, chunk := range chunkRefs(sortedRefs(ctx, reader, ids)) {
		remaining, exhausted := containmentBudget(b, e.limits)
		if exhausted {
			cut(chunk)
			continue
		}
		// The counts come from the generation's node-kind and source-byte side
		// arrays, read once per batch of children. Nothing is hydrated: the
		// old map read a whole model.Node per child to look at two fields of
		// it, which is what made its second page refuse at the deadline
		// (ADR-0005).
		complete, err := scanNeighbours(ctx, reader, chunk, model.DirectionOutgoing,
			kinds, remaining, b, func(owners, children []NodeRef) error {
				childKinds, err := reader.NodeKinds(ctx, children)
				if err != nil {
					return err
				}
				bytes, err := reader.SourceBytes(ctx, children)
				if err != nil {
					return err
				}
				for i, owner := range owners {
					id, ok := byRef[owner]
					if !ok {
						continue
					}
					counts := out[id]
					switch {
					case childKinds[i] == model.NodeFile:
						counts.files++
						counts.bytes += bytes[i]
					case isOverviewContainer(childKinds[i]):
						// A nested container is structure, not a symbol; it
						// appears in the map as its own item with its own
						// counts.
					case childKinds[i] == "":
						// A child the generation publishes no kind for is not
						// counted: an aggregate must rest on facts this answer
						// could actually read.
					default:
						counts.symbols++
					}
					out[id] = counts
				}
				return nil
			})
		if err != nil {
			return nil, nil, err
		}
		if !complete {
			// The entries already counted for this chunk are a partial count
			// of its containers, which is the one thing this map may not
			// publish, so cut retracts them along with marking the chunk
			// unmeasured.
			cut(chunk)
		}
	}
	return out, unmeasured, nil
}

// containmentBudget is what is LEFT of the page's cumulative edge allowance.
// The whole page shares one allowance, so a single pathological container
// cannot make one answer read more adjacency than a walk of the same size
// would have been allowed to.
// An unlimited edge allowance has no remainder to compute: the zero Limit means
// "no bound", so subtracting the edges already spent from it and handing the
// result on as a numeric budget would refuse the very first batch.
//
// exhausted is reported separately from the remainder because a remainder of
// zero and an unlimited bound are the same integer and the opposite instruction.
func containmentBudget(b *budget, limits Limits) (remaining config.Limit, exhausted bool) {
	if limits.Edges().IsUnlimited() {
		return config.Unlimited, false
	}
	left := int64(limits.MaxEdges) - b.edges
	if left <= 0 {
		return config.Unlimited, true
	}
	return config.Limit(left), false
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
// map. See overviewContainerKinds for why it is wider than the rollup's.
func isOverviewContainer(k model.NodeKind) bool {
	switch k {
	case model.NodeRepository, model.NodeDirectory, model.NodePackage,
		model.NodeModule, model.NodeNamespace:
		return true
	}
	return false
}
