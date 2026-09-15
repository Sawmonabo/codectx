package graph

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"slices"

	"github.com/Sawmonabo/codectx/internal/model"
)

// MemoryGraph is an in-heap GraphReader built from node and relation fixtures.
// It is the reference implementation of the port (ADR-0005): the conformance
// suite runs against it and against the store's packed reader, so a behaviour
// the two disagree on is a defect in one of them rather than an accident of the
// fixture.
//
// It is NOT a scale structure. It holds the whole graph in the Go heap, so it
// belongs to tests and to fixtures, never to a served request; the store's
// packed per-generation adjacency is the production read path.
//
// Visibility: a relation whose endpoints are not both in nodes is dropped, and
// a node appears exactly once however many times the fixture lists it (the last
// entry for an id wins, so a fixture can override an earlier node). This
// mirrors the generation semi-join the store's build performs.
type MemoryGraph struct {
	binding model.Binding
	// nodes is indexed by surrogate-1, in the order NewMemoryGraphOrdered was
	// given -- ascending canonical id unless a MemoryGraphOrder says otherwise:
	// deterministically from the content-derived ids alone, so the same fixture
	// always yields the same refs. The store assigns its own surrogates
	// (node_ids.id, no renumbering); nothing may assume the two agree.
	nodes   []model.Node
	refByID map[model.NodeID]NodeRef

	relIDs  []model.RelationID // indexed by relation surrogate-1
	relByID map[model.RelationID]RelRef
	// evidence is the per-relation evidence count side array, keyed by
	// canonical id because that is what a fixture declares it with. Nil means
	// the fixture declared none.
	evidence map[model.RelationID]int64

	// out and in are indexed by surrogate, so index 0 is the unused zero
	// surrogate and stays empty. Each list is sorted by (Neighbour, Rel),
	// which is the order the port promises and the tie-break that keeps two
	// relations between the same pair in a stable order.
	out, in [][]Edge

	kinds *memKindTable

	// Side arrays, indexed by surrogate; index 0 is the unused zero surrogate.
	nodeKinds  []model.NodeKind
	containers []NodeRef
	bytes      []int64
}

var _ GraphReader = (*MemoryGraph)(nil)

// memKindTable is one fixture's relation-kind dictionary. Codes are dense from
// 1 in ascending kind-spelling order, so they are a function of the fixture and
// not of the order the relations happened to be listed in.
type memKindTable struct {
	byCode []model.RelationKind // index code-1
	byKind map[model.RelationKind]KindCode
}

func (t *memKindTable) Code(kind model.RelationKind) (KindCode, bool) {
	c, ok := t.byKind[kind]
	return c, ok
}

func (t *memKindTable) Kind(code KindCode) (model.RelationKind, bool) {
	if code == 0 || int(code) > len(t.byCode) {
		return "", false
	}
	return t.byCode[code-1], true
}

func (t *memKindTable) Len() int { return len(t.byCode) }

// MemoryGraphOrder chooses the sequence surrogates are handed out in. Both
// fields are optional and default to ascending canonical order, which is what
// NewMemoryGraph uses.
//
// It exists because the store assigns its own surrogates (node_ids.id,
// relation_ids.id, no renumbering) and NOTHING may assume they ascend with the
// canonical id. A fixture whose two orders always agree cannot tell an answer
// keyed by the canonical id from one keyed by the surrogate, so every ordering
// and tie-break test written against the default fixture passes whichever key
// the code actually used. A test that supplies a reversed order here makes the
// two orders diverge and the confusion observable.
type MemoryGraphOrder struct {
	// Nodes orders the canonical node ids; surrogate 1 goes to the first.
	Nodes func(a, b model.NodeID) int
	// Relations does the same for relation ids.
	Relations func(a, b model.RelationID) int
}

// NewMemoryGraph builds a reader over the fixture, assigning surrogates in
// ascending canonical-id order. It never fails: a relation naming a node the
// fixture does not carry is invisible and is dropped, which is exactly what the
// generation semi-join does to a relation whose endpoints are not in the
// generation.
func NewMemoryGraph(binding model.Binding, nodes []model.Node, relations []model.Relation) *MemoryGraph {
	return NewMemoryGraphOrdered(binding, nodes, relations, MemoryGraphOrder{})
}

// NewMemoryGraphOrdered is NewMemoryGraph with the surrogate assignment order
// chosen by the caller. Everything else -- visibility, the kind dictionary, the
// per-list edge order, the side arrays and the container attribution -- is
// unchanged, so the same fixture answers the same facts whatever order it is
// built in, and the conformance suite runs against both.
func NewMemoryGraphOrdered(binding model.Binding, nodes []model.Node, relations []model.Relation,
	order MemoryGraphOrder) *MemoryGraph {
	g := &MemoryGraph{binding: binding, refByID: map[model.NodeID]NodeRef{}, relByID: map[model.RelationID]RelRef{}}

	byID := make(map[model.NodeID]model.Node, len(nodes))
	for _, n := range nodes {
		byID[n.ID] = n
	}
	ids := make([]model.NodeID, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	slices.SortFunc(ids, orderOr(order.Nodes, cmp.Compare[model.NodeID]))
	g.nodes = make([]model.Node, 0, len(ids))
	for i, id := range ids {
		g.refByID[id] = NodeRef(i + 1)
		g.nodes = append(g.nodes, byID[id])
	}

	rels := make(map[model.RelationID]model.Relation, len(relations))
	kindSeen := map[model.RelationKind]bool{}
	for _, r := range relations {
		if _, ok := g.refByID[r.From]; !ok {
			continue
		}
		if _, ok := g.refByID[r.To]; !ok {
			continue
		}
		rels[r.ID] = r
		kindSeen[r.Kind] = true
	}
	relIDs := make([]model.RelationID, 0, len(rels))
	for id := range rels {
		relIDs = append(relIDs, id)
	}
	slices.SortFunc(relIDs, orderOr(order.Relations, cmp.Compare[model.RelationID]))
	g.relIDs = relIDs
	for i, id := range relIDs {
		g.relByID[id] = RelRef(i + 1)
	}

	kindList := make([]model.RelationKind, 0, len(kindSeen))
	for k := range kindSeen {
		kindList = append(kindList, k)
	}
	slices.Sort(kindList)
	g.kinds = &memKindTable{byCode: kindList, byKind: make(map[model.RelationKind]KindCode, len(kindList))}
	for i, k := range kindList {
		g.kinds.byKind[k] = KindCode(i + 1)
	}

	n := len(g.nodes)
	g.out = make([][]Edge, n+1)
	g.in = make([][]Edge, n+1)
	for _, id := range relIDs {
		r := rels[id]
		from, to := g.refByID[r.From], g.refByID[r.To]
		rel := g.relByID[id]
		code, _ := g.kinds.Code(r.Kind)
		g.out[from] = append(g.out[from], Edge{Owner: from, Neighbour: to, Rel: rel, Kind: code, Outgoing: true})
		g.in[to] = append(g.in[to], Edge{Owner: to, Neighbour: from, Rel: rel, Kind: code, Outgoing: false})
	}
	byNeighbour := func(a, b Edge) int {
		if c := cmp.Compare(a.Neighbour, b.Neighbour); c != 0 {
			return c
		}
		return cmp.Compare(a.Rel, b.Rel)
	}
	for i := range g.out {
		slices.SortFunc(g.out[i], byNeighbour)
		slices.SortFunc(g.in[i], byNeighbour)
	}

	g.nodeKinds = make([]model.NodeKind, n+1)
	g.bytes = make([]int64, n+1)
	g.containers = make([]NodeRef, n+1)
	for i, node := range g.nodes {
		ref := NodeRef(i + 1)
		g.nodeKinds[ref] = node.Kind
		if node.Kind == model.NodeFile {
			g.bytes[ref] = memSourceBytes(node)
		}
	}
	containsCode, hasContains := g.kinds.Code(model.RelContains)
	for i := range g.nodes {
		ref := NodeRef(i + 1)
		if memIsContainerKind(g.nodeKinds[ref]) {
			// A container is its own container, so a container-level edge
			// rolls up to the pair it already names.
			g.containers[ref] = ref
			continue
		}
		if !hasContains {
			continue
		}
		// Several containers can claim one node; the LOWEST CANONICAL ID wins,
		// which is the tie-break the package rollup uses today, so the
		// attribution is reproducible from the facts and not from the order the
		// adjacency returned. The CANONICAL ids are compared, never the
		// surrogates: MemoryGraphOrder lets the two orders diverge, and the
		// store's build cannot take that shortcut either.
		var best NodeRef
		for _, e := range g.in[ref] {
			if e.Kind != containsCode || !memIsContainerKind(g.nodeKinds[e.Neighbour]) {
				continue
			}
			if best == 0 || g.nodes[e.Neighbour-1].ID < g.nodes[best-1].ID {
				best = e.Neighbour
			}
		}
		g.containers[ref] = best
	}
	return g
}

// memIsContainerKind is the rollup's container vocabulary: only a package or a
// module can be the target of a rollup, so a `contains` parent that is a
// directory or a file never wins the container slot.
func memIsContainerKind(k model.NodeKind) bool {
	return k == model.NodePackage || k == model.NodeModule
}

// memSourceBytes reads the size a file node carries in its typed metadata,
// which is the only byte fact the port exposes. A missing, unreadable or
// negative size contributes nothing rather than a guess.
func memSourceBytes(n model.Node) int64 {
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

func (g *MemoryGraph) Binding() model.Binding { return g.binding }

func (g *MemoryGraph) MaxNode() NodeRef { return NodeRef(len(g.nodes)) }

// MaxRelation is the largest relation surrogate this fixture generation holds.
func (g *MemoryGraph) MaxRelation() RelRef { return RelRef(len(g.relIDs)) }

func (g *MemoryGraph) Kinds() KindTable { return g.kinds }

func (g *MemoryGraph) Resolve(ctx context.Context, ids []model.NodeID) ([]NodeRef, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out := make([]NodeRef, len(ids))
	for i, id := range ids {
		out[i] = g.refByID[id]
	}
	return out, nil
}

func (g *MemoryGraph) NodeIDs(ctx context.Context, refs []NodeRef) ([]model.NodeID, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out := make([]model.NodeID, len(refs))
	for i, ref := range refs {
		if ref == 0 || int(ref) > len(g.nodes) {
			continue
		}
		out[i] = g.nodes[ref-1].ID
	}
	return out, nil
}

func (g *MemoryGraph) RelationIDs(ctx context.Context, refs []RelRef) ([]model.RelationID, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out := make([]model.RelationID, len(refs))
	for i, ref := range refs {
		if ref == 0 || int(ref) > len(g.relIDs) {
			continue
		}
		out[i] = g.relIDs[ref-1]
	}
	return out, nil
}

// Neighbours streams the fixture's packed lists exactly as the port specifies.
//
// EdgePos.Index is a BOUNDARY, not the identity of a delivered entry: it counts
// the owner's stored entries -- before any kind filtering -- that the scan has
// already consumed, so resuming means "skip the first Index entries of owner
// Node". A scan that runs to the end therefore reports the last delivered
// entry's index PLUS ONE, and a scan stopped by ErrStopScan reports the
// undelivered entry's own index; resuming from either delivers exactly the
// entries that were not delivered, which is what keeps a walk from dropping or
// repeating an edge across pages.
//
// For DirectionBoth an owner's list is its outgoing entries followed by its
// incoming entries, and Index counts across that concatenation. Neighbour
// surrogates ascend within each of the two lists, not across the join, because
// the port specifies the two lists in that order.
func (g *MemoryGraph) Neighbours(ctx context.Context, refs []NodeRef, direction model.Direction,
	kinds []KindCode, from EdgePos, fn func(Edge) error) (EdgePos, error) {
	if err := ascendingRefs(refs); err != nil {
		return from, err
	}
	var filter map[KindCode]bool
	if len(kinds) > 0 {
		filter = make(map[KindCode]bool, len(kinds))
		for _, k := range kinds {
			filter[k] = true
		}
	}
	pos := from
	for _, ref := range refs {
		if err := ctx.Err(); err != nil {
			return pos, err
		}
		if ref == 0 || int(ref) > len(g.nodes) || ref < from.Node {
			continue
		}
		list := g.ownerList(ref, direction)
		start := 0
		if ref == from.Node {
			start = int(from.Index)
		}
		for i := start; i < len(list); i++ {
			e := list[i]
			if filter != nil && !filter[e.Kind] {
				continue
			}
			if err := fn(e); err != nil {
				if errors.Is(err, ErrStopScan) {
					return EdgePos{Node: ref, Index: uint32(i)}, nil
				}
				return EdgePos{Node: ref, Index: uint32(i)}, err
			}
			pos = EdgePos{Node: ref, Index: uint32(i + 1)}
		}
	}
	return pos, nil
}

// ownerList is the owner's stored list in direction: for DirectionBoth the
// outgoing entries followed by the incoming ones, which is the order the port
// fixes and the order EdgePos.Index counts in.
func (g *MemoryGraph) ownerList(ref NodeRef, direction model.Direction) []Edge {
	switch direction {
	case model.DirectionIncoming:
		return g.in[ref]
	case model.DirectionBoth:
		joined := make([]Edge, 0, len(g.out[ref])+len(g.in[ref]))
		joined = append(joined, g.out[ref]...)
		return append(joined, g.in[ref]...)
	default:
		return g.out[ref]
	}
}

// ascendingRefs enforces the port's precondition on refs. It is checked rather
// than assumed because a caller that passes an unsorted or repeated frontier
// gets a silently wrong page -- an edge delivered twice, or one skipped by the
// resume position -- instead of an error.
func ascendingRefs[T ~uint64](refs []T) error {
	for i := 1; i < len(refs); i++ {
		if refs[i] <= refs[i-1] {
			return &model.Error{Code: model.CodeArgumentInvalid,
				Message: "graph: Neighbours requires ascending, duplicate-free node refs"}
		}
	}
	return nil
}

func (g *MemoryGraph) NodeKinds(ctx context.Context, refs []NodeRef) ([]model.NodeKind, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out := make([]model.NodeKind, len(refs))
	for i, ref := range refs {
		if ref == 0 || int(ref) > len(g.nodes) {
			continue
		}
		out[i] = g.nodeKinds[ref]
	}
	return out, nil
}

func (g *MemoryGraph) Containers(ctx context.Context, refs []NodeRef) ([]NodeRef, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out := make([]NodeRef, len(refs))
	for i, ref := range refs {
		if ref == 0 || int(ref) > len(g.nodes) {
			continue
		}
		out[i] = g.containers[ref]
	}
	return out, nil
}

func (g *MemoryGraph) SourceBytes(ctx context.Context, refs []NodeRef) ([]int64, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out := make([]int64, len(refs))
	for i, ref := range refs {
		if ref == 0 || int(ref) > len(g.nodes) {
			continue
		}
		out[i] = g.bytes[ref]
	}
	return out, nil
}

// EvidenceCounts reports one occurrence for every relation the fixture carries
// and zero for a relation that is not in it. A fixture states relations, not
// the occurrences that produced them, and a relation exists because at least
// one occurrence produced it; inventing a larger count would make a ranking
// that weighs evidence disagree with the store for no reason.
func (g *MemoryGraph) EvidenceCounts(ctx context.Context, rels []RelRef) ([]int64, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out := make([]int64, len(rels))
	for i, ref := range rels {
		if ref == 0 || int(ref) > len(g.relIDs) {
			continue
		}
		if n, ok := g.evidence[g.relIDs[ref-1]]; ok {
			out[i] = n
			continue
		}
		// A fixture that declared no evidence at all still describes relations
		// the analysis derived, so one is the honest floor; a fixture that DID
		// declare evidence gets its own counts above.
		out[i] = 1
	}
	return out, nil
}

// WithEvidenceCounts records the evidence backing each relation, so a fixture
// that declares evidence reports it through the side array the rollup reads.
//
// It is a separate call rather than a constructor parameter because the port's
// evidence count is a per-generation SIDE ARRAY, not part of the graph
// structure, and most fixtures have no evidence to declare at all.
func (g *MemoryGraph) WithEvidenceCounts(counts map[model.RelationID]int64) *MemoryGraph {
	g.evidence = counts
	return g
}

// NodesByID hydrates the fixture's nodes in the order asked, skipping an id the
// fixture does not carry.
func (g *MemoryGraph) NodesByID(ctx context.Context, ids []model.NodeID) ([]model.Node, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out := make([]model.Node, 0, len(ids))
	for _, id := range ids {
		if ref, ok := g.refByID[id]; ok {
			out = append(out, g.nodes[ref-1])
		}
	}
	return out, nil
}

// EvidenceFor returns no evidence identities: a fixture of nodes and relations
// carries none, and a synthesised EvidenceID would name a record that does not
// exist. A caller that needs evidence-backed paths reads them from a store.
func (g *MemoryGraph) EvidenceFor(ctx context.Context, relations []model.RelationID,
	limit int) (map[model.RelationID][]model.EvidenceID, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return map[model.RelationID][]model.EvidenceID{}, nil
}

// Capabilities reports none: a fixture has no provider run behind it.
func (g *MemoryGraph) Capabilities(ctx context.Context) ([]model.CapabilityState, error) {
	return nil, ctx.Err()
}

// orderOr is the caller's comparator, or the canonical ascending one when it
// supplied none.
func orderOr[T any](order, fallback func(a, b T) int) func(a, b T) int {
	if order != nil {
		return order
	}
	return fallback
}
