package sqlite

import (
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"io"
	"slices"
	"strings"

	"github.com/Sawmonabo/codectx/internal/graph"
	"github.com/Sawmonabo/codectx/internal/model"
)

// graphReader reads the packed per-generation adjacency (ADR-0005 Decision 1).
// It is the store's implementation of graph.GraphReader and is held to the same
// behaviour as the in-heap reference by graphtest.RunConformance, so a
// disagreement between the two is a defect in one of them rather than an
// accident of a fixture.
//
// It is per request, like the PinnedReader it reads through: the part cache
// below is a request-scoped window, not a process cache, so two concurrent
// requests never share a buffer and a request's footprint is bounded by the
// window and not by the repository.
type graphReader struct {
	r *PinnedReader

	maxNode     graph.NodeRef
	maxRelation graph.RelRef
	kinds       *graphKindTable
	nodeKinds   []model.NodeKind // index code-1

	cache map[string]*partWindow
}

var _ graph.GraphReader = (*graphReader)(nil)

// partsPerStream bounds the parts one stream keeps resident for one request.
// It is an INTERNAL working-set constant, not a user limit: a stream with more
// parts is read in full, one window at a time, and nothing is ever refused or
// truncated because of it. Sixteen parts is ~16 MiB for an edge stream and
// ~8 MiB for an offset directory in the worst case, and a walk that visits
// owners in ascending order touches parts sequentially, so the window is
// typically one or two parts deep.
const partsPerStream = 16

// partWindow is a bounded most-recently-used window over one stream's parts.
// The eviction order is a plain slice because the window is sixteen entries
// deep: a map plus a list would cost more than the scan it saves.
type partWindow struct {
	size  int // bytes in a full part of this stream
	order []int
	parts map[int][]byte
}

// graphKindTable is one generation's relation-kind dictionary, both ways.
type graphKindTable struct {
	byCode []model.RelationKind
	byKind map[model.RelationKind]graph.KindCode
}

func (t *graphKindTable) Code(kind model.RelationKind) (graph.KindCode, bool) {
	c, ok := t.byKind[kind]
	return c, ok
}

func (t *graphKindTable) Kind(code graph.KindCode) (model.RelationKind, bool) {
	if code == 0 || int(code) > len(t.byCode) {
		return "", false
	}
	return t.byCode[code-1], true
}

func (t *graphKindTable) Len() int { return len(t.byCode) }

// NewGraphReader opens the packed adjacency of the reader's pinned generation.
// A generation with no generation_graph row was never published with one, which
// is a corrupt store rather than an empty graph: the row is written last, so
// its absence means the build did not finish.
func NewGraphReader(ctx context.Context, r *PinnedReader) (graph.GraphReader, error) {
	g := &graphReader{r: r, cache: map[string]*partWindow{}}
	var maxNode, maxRelation, nodeCount, edgeCount, format int64
	var relKinds, nodeKinds []byte
	err := r.s.read(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, `SELECT max_node, max_relation, node_count, edge_count, kinds, node_kinds, format
			FROM generation_graph WHERE generation_id = ?`, r.gen).
			Scan(&maxNode, &maxRelation, &nodeCount, &edgeCount, &relKinds, &nodeKinds, &format)
		if isNoRows(err) {
			return corrupt("generation %d has no packed adjacency; it was published without one", r.gen)
		}
		return wrap("generation_graph", err)
	})
	if err != nil {
		return nil, err
	}
	if format != graphFormat {
		return nil, corrupt("generation %d carries packed adjacency format %d, not %d", r.gen, format, graphFormat)
	}
	g.maxNode, g.maxRelation = graph.NodeRef(maxNode), graph.RelRef(maxRelation)
	g.kinds = &graphKindTable{byKind: map[model.RelationKind]graph.KindCode{}}
	for i, k := range splitDictionary(relKinds) {
		g.kinds.byCode = append(g.kinds.byCode, model.RelationKind(k))
		g.kinds.byKind[model.RelationKind(k)] = graph.KindCode(i + 1)
	}
	for _, k := range splitDictionary(nodeKinds) {
		g.nodeKinds = append(g.nodeKinds, model.NodeKind(k))
	}
	return g, nil
}

// splitDictionary decodes a NUL-separated dictionary blob. An empty blob is an
// empty dictionary, not one entry of the empty string.
func splitDictionary(raw []byte) []string {
	if len(raw) == 0 {
		return nil
	}
	return strings.Split(string(raw), "\x00")
}

func (g *graphReader) Binding() model.Binding { return g.r.Binding() }
func (g *graphReader) MaxNode() graph.NodeRef { return g.maxNode }
func (g *graphReader) Kinds() graph.KindTable { return g.kinds }

// part returns one part of a stream through the request's bounded window,
// reading it from the store on a miss and evicting the least recently used part
// when the window is full.
func (g *graphReader) part(ctx context.Context, stream string, index int) ([]byte, error) {
	w := g.cache[stream]
	if w == nil {
		w = &partWindow{size: partSizeFor(stream), parts: map[int][]byte{}}
		g.cache[stream] = w
	}
	if b, ok := w.parts[index]; ok {
		if i := slices.Index(w.order, index); i >= 0 {
			w.order = append(slices.Delete(w.order, i, i+1), index)
		}
		return b, nil
	}
	var raw []byte
	err := g.r.s.read(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, `SELECT bytes FROM generation_graph_parts
			WHERE generation_id = ? AND stream = ? AND part = ?`, g.r.gen, stream, index).Scan(&raw)
		if isNoRows(err) {
			return corrupt("packed adjacency stream %q of generation %d is missing part %d", stream, g.r.gen, index)
		}
		return wrap("generation_graph_parts", err)
	})
	if err != nil {
		return nil, err
	}
	for len(w.order) >= partsPerStream {
		delete(w.parts, w.order[0])
		w.order = w.order[1:]
	}
	w.parts[index] = raw
	w.order = append(w.order, index)
	return raw, nil
}

// readAt copies n bytes from a stream starting at off, stitching across the
// part boundary a value or a list may straddle. Parts are fixed-size except the
// last, which is what makes the part index pure arithmetic.
func (g *graphReader) readAt(ctx context.Context, stream string, off int64, n int) ([]byte, error) {
	size := int64(partSizeFor(stream))
	out := make([]byte, 0, n)
	for len(out) < n {
		index := int(off / size)
		part, err := g.part(ctx, stream, index)
		if err != nil {
			return nil, err
		}
		begin := int(off % size)
		if begin >= len(part) {
			return nil, corrupt("packed adjacency stream %q of generation %d ends before offset %d", stream, g.r.gen, off)
		}
		take := min(len(part)-begin, n-len(out))
		out = append(out, part[begin:begin+take]...)
		off += int64(take)
	}
	return out, nil
}

// offsetAt reads one entry of an offsets directory.
func (g *graphReader) offsetAt(ctx context.Context, stream string, index uint64) (int64, error) {
	raw, err := g.readAt(ctx, stream, int64(index)*offsetEntryBytes, offsetEntryBytes)
	if err != nil {
		return 0, err
	}
	return int64(binary.LittleEndian.Uint64(raw)), nil
}

// arrayValue reads one fixed-width entry of a side array, answering zero for an
// index past the array's end: zero is the absent value of every side array, so
// a surrogate outside the generation reads as absent rather than as an error.
func (g *graphReader) arrayValue(ctx context.Context, stream string, index uint64, width int) ([]byte, error) {
	size := int64(partSizeFor(stream))
	off := int64(index) * int64(width)
	part, err := g.part(ctx, stream, int(off/size))
	if err != nil {
		return nil, err
	}
	begin := int(off % size)
	if begin+width > len(part) {
		return make([]byte, width), nil
	}
	return part[begin : begin+width], nil
}

// byteCursor streams a byte range of an edge stream through the part window, so
// decoding a list never materialises it: heap stays one part, whatever the
// degree of the node.
type byteCursor struct {
	g      *graphReader
	ctx    context.Context
	stream string
	off    int64
	end    int64
}

func (c *byteCursor) ReadByte() (byte, error) {
	if c.off >= c.end {
		return 0, io.EOF
	}
	b, err := c.g.readAt(c.ctx, c.stream, c.off, 1)
	if err != nil {
		return 0, err
	}
	c.off++
	return b[0], nil
}

// listSegment is one stored list of one owner: a byte range of one direction's
// edge stream, and which side of the relation the owner is on.
type listSegment struct {
	stream   string
	begin    int64
	end      int64
	outgoing bool
}

// segments resolves the owner's stored lists in the order EdgePos.Index counts
// them: for DirectionBoth the outgoing list first and then the incoming one,
// which is the order the port fixes.
func (g *graphReader) segments(ctx context.Context, ref graph.NodeRef, direction model.Direction) ([]listSegment, error) {
	var out []listSegment
	add := func(offStream, edgeStream string, outgoing bool) error {
		begin, err := g.offsetAt(ctx, offStream, uint64(ref))
		if err != nil {
			return err
		}
		end, err := g.offsetAt(ctx, offStream, uint64(ref)+1)
		if err != nil {
			return err
		}
		if end > begin {
			out = append(out, listSegment{stream: edgeStream, begin: begin, end: end, outgoing: outgoing})
		}
		return nil
	}
	if direction != model.DirectionIncoming {
		if err := add(streamOutOffsets, streamOutEdges, true); err != nil {
			return nil, err
		}
	}
	if direction != model.DirectionOutgoing {
		if err := add(streamInOffsets, streamInEdges, false); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// listIter decodes one owner's stored entries in order across its segments. The
// delta base resets at each segment, because each segment is a list start.
type listIter struct {
	g    *graphReader
	ctx  context.Context
	ref  graph.NodeRef
	segs []listSegment
	si   int
	cur  *byteCursor
	prev uint64
}

// next decodes the next stored entry, before any kind filtering.
func (it *listIter) next() (graph.Edge, bool, error) {
	for {
		if it.cur == nil {
			if it.si >= len(it.segs) {
				return graph.Edge{}, false, nil
			}
			s := it.segs[it.si]
			it.cur = &byteCursor{g: it.g, ctx: it.ctx, stream: s.stream, off: s.begin, end: s.end}
			it.prev = 0
		}
		if it.cur.off >= it.cur.end {
			it.cur = nil
			it.si++
			continue
		}
		delta, err := binary.ReadUvarint(it.cur)
		if err != nil {
			return graph.Edge{}, false, corrupt("packed adjacency of generation %d: %v", it.g.r.gen, err)
		}
		rel, err := binary.ReadUvarint(it.cur)
		if err != nil {
			return graph.Edge{}, false, corrupt("packed adjacency of generation %d: %v", it.g.r.gen, err)
		}
		code, err := it.cur.ReadByte()
		if err != nil {
			return graph.Edge{}, false, corrupt("packed adjacency of generation %d: %v", it.g.r.gen, err)
		}
		it.prev += delta
		return graph.Edge{Owner: it.ref, Neighbour: graph.NodeRef(it.prev), Rel: graph.RelRef(rel),
			Kind: graph.KindCode(code), Outgoing: it.segs[it.si].outgoing}, true, nil
	}
}

// Neighbours streams the packed lists exactly as the port specifies, and as the
// in-heap reference implements it.
//
// EdgePos.Index is a BOUNDARY, not the identity of a delivered entry: it counts
// the owner's stored entries -- before any kind filtering -- that the scan has
// already consumed, so resuming means "skip the first Index entries of owner
// Node". A completed scan reports the last delivered entry's index PLUS ONE; a
// scan stopped by ErrStopScan reports the undelivered entry's own index.
// Skipped entries are decoded rather than computed past: offsets are byte
// offsets, so an entry count is not arithmetic on them.
func (g *graphReader) Neighbours(ctx context.Context, refs []graph.NodeRef, direction model.Direction,
	kinds []graph.KindCode, from graph.EdgePos, fn func(graph.Edge) error) (graph.EdgePos, error) {
	if err := ascendingNodeRefs(refs); err != nil {
		return from, err
	}
	if !direction.Valid() {
		return from, invalid("direction %q is not a known direction", direction)
	}
	var filter map[graph.KindCode]bool
	if len(kinds) > 0 {
		filter = make(map[graph.KindCode]bool, len(kinds))
		for _, k := range kinds {
			filter[k] = true
		}
	}
	pos := from
	for _, ref := range refs {
		if err := ctx.Err(); err != nil {
			return pos, err
		}
		if ref == 0 || ref > g.maxNode || ref < from.Node {
			continue
		}
		segs, err := g.segments(ctx, ref, direction)
		if err != nil {
			return pos, err
		}
		start := 0
		if ref == from.Node {
			start = int(from.Index)
		}
		it := &listIter{g: g, ctx: ctx, ref: ref, segs: segs}
		for i := 0; ; i++ {
			e, ok, err := it.next()
			if err != nil {
				return pos, err
			}
			if !ok {
				break
			}
			if i < start {
				continue
			}
			if filter != nil && !filter[e.Kind] {
				continue
			}
			if err := fn(e); err != nil {
				if errors.Is(err, graph.ErrStopScan) {
					return graph.EdgePos{Node: ref, Index: uint32(i)}, nil
				}
				return graph.EdgePos{Node: ref, Index: uint32(i)}, err
			}
			pos = graph.EdgePos{Node: ref, Index: uint32(i + 1)}
		}
	}
	return pos, nil
}

// ascendingNodeRefs enforces the port's precondition on refs. It is checked
// rather than assumed because a caller that passes an unsorted or repeated
// frontier gets a silently wrong page -- an edge delivered twice, or one
// skipped by the resume position -- instead of an error.
func ascendingNodeRefs(refs []graph.NodeRef) error {
	for i := 1; i < len(refs); i++ {
		if refs[i] <= refs[i-1] {
			return invalid("graph: Neighbours requires ascending, duplicate-free node refs")
		}
	}
	return nil
}

// graphBatch is the number of ids one dictionary round trip carries. It is an
// internal batching constant: a longer list is read in more batches, never
// refused.
const graphBatch = 256

// Resolve maps canonical ids to surrogates, 0 for an id the generation does not
// carry. The dictionary is store-wide, so a surrogate it answers is checked
// against the generation's node-kind array: a node interned by another
// generation resolves to 0 here, which is what "not visible" means.
func (g *graphReader) Resolve(ctx context.Context, ids []model.NodeID) ([]graph.NodeRef, error) {
	out := make([]graph.NodeRef, len(ids))
	byID := make(map[string]graph.NodeRef, len(ids))
	raw := make([][]byte, 0, len(ids))
	for _, id := range ids {
		b, err := idBlob("node_id", string(id))
		if err != nil {
			return nil, err
		}
		raw = append(raw, b)
	}
	for begin := 0; begin < len(raw); begin += graphBatch {
		chunk := raw[begin:min(begin+graphBatch, len(raw))]
		args := make([]any, len(chunk))
		marks := make([]string, len(chunk))
		for i, b := range chunk {
			args[i], marks[i] = b, "?"
		}
		err := g.r.s.read(ctx, func(tx *sql.Tx) error {
			rows, err := tx.QueryContext(ctx, `SELECT canonical, id FROM node_ids WHERE canonical IN (`+
				strings.Join(marks, ",")+`)`, args...)
			if err != nil {
				return wrap("node_ids", err)
			}
			defer rows.Close()
			for rows.Next() {
				var canonical []byte
				var ref int64
				if err := rows.Scan(&canonical, &ref); err != nil {
					return wrap("node_ids", err)
				}
				byID[idHex(canonical)] = graph.NodeRef(ref)
			}
			return wrap("node_ids", rows.Err())
		})
		if err != nil {
			return nil, err
		}
	}
	for i, id := range ids {
		ref, ok := byID[string(id)]
		if !ok || ref > g.maxNode {
			continue
		}
		code, err := g.arrayValue(ctx, streamNodeKind, uint64(ref), kindEntryBytes)
		if err != nil {
			return nil, err
		}
		if code[0] == 0 {
			continue
		}
		out[i] = ref
	}
	return out, nil
}

// canonicalByRef reads a dictionary table's canonical ids for a batch of
// surrogates. The refs are sorted before each batch so the integer primary key
// is probed in ascending order and one b-tree walk serves the batch.
func (g *graphReader) canonicalByRef(ctx context.Context, table string, refs []uint64) (map[uint64]string, error) {
	out := make(map[uint64]string, len(refs))
	sorted := slices.Clone(refs)
	slices.Sort(sorted)
	sorted = slices.Compact(sorted)
	for begin := 0; begin < len(sorted); begin += graphBatch {
		chunk := sorted[begin:min(begin+graphBatch, len(sorted))]
		args := make([]any, len(chunk))
		marks := make([]string, len(chunk))
		for i, ref := range chunk {
			args[i], marks[i] = int64(ref), "?"
		}
		err := g.r.s.read(ctx, func(tx *sql.Tx) error {
			rows, err := tx.QueryContext(ctx, `SELECT id, canonical FROM `+table+` WHERE id IN (`+
				strings.Join(marks, ",")+`) ORDER BY id`, args...)
			if err != nil {
				return wrap(table, err)
			}
			defer rows.Close()
			for rows.Next() {
				var ref int64
				var canonical []byte
				if err := rows.Scan(&ref, &canonical); err != nil {
					return wrap(table, err)
				}
				out[uint64(ref)] = idHex(canonical)
			}
			return wrap(table, rows.Err())
		})
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (g *graphReader) NodeIDs(ctx context.Context, refs []graph.NodeRef) ([]model.NodeID, error) {
	want := make([]uint64, 0, len(refs))
	for _, ref := range refs {
		if ref != 0 && ref <= g.maxNode {
			want = append(want, uint64(ref))
		}
	}
	found, err := g.canonicalByRef(ctx, "node_ids", want)
	if err != nil {
		return nil, err
	}
	out := make([]model.NodeID, len(refs))
	for i, ref := range refs {
		out[i] = model.NodeID(found[uint64(ref)])
	}
	return out, nil
}

func (g *graphReader) RelationIDs(ctx context.Context, refs []graph.RelRef) ([]model.RelationID, error) {
	want := make([]uint64, 0, len(refs))
	for _, ref := range refs {
		if ref != 0 && ref <= g.maxRelation {
			want = append(want, uint64(ref))
		}
	}
	found, err := g.canonicalByRef(ctx, "relation_ids", want)
	if err != nil {
		return nil, err
	}
	out := make([]model.RelationID, len(refs))
	for i, ref := range refs {
		out[i] = model.RelationID(found[uint64(ref)])
	}
	return out, nil
}

func (g *graphReader) NodeKinds(ctx context.Context, refs []graph.NodeRef) ([]model.NodeKind, error) {
	out := make([]model.NodeKind, len(refs))
	for i, ref := range refs {
		if ref == 0 || ref > g.maxNode {
			continue
		}
		raw, err := g.arrayValue(ctx, streamNodeKind, uint64(ref), kindEntryBytes)
		if err != nil {
			return nil, err
		}
		if code := int(raw[0]); code > 0 && code <= len(g.nodeKinds) {
			out[i] = g.nodeKinds[code-1]
		}
	}
	return out, nil
}

func (g *graphReader) Containers(ctx context.Context, refs []graph.NodeRef) ([]graph.NodeRef, error) {
	out := make([]graph.NodeRef, len(refs))
	for i, ref := range refs {
		if ref == 0 || ref > g.maxNode {
			continue
		}
		raw, err := g.arrayValue(ctx, streamNodeContainer, uint64(ref), containerEntryBytes)
		if err != nil {
			return nil, err
		}
		out[i] = graph.NodeRef(binary.LittleEndian.Uint64(raw))
	}
	return out, nil
}

func (g *graphReader) SourceBytes(ctx context.Context, refs []graph.NodeRef) ([]int64, error) {
	out := make([]int64, len(refs))
	for i, ref := range refs {
		if ref == 0 || ref > g.maxNode {
			continue
		}
		raw, err := g.arrayValue(ctx, streamNodeBytes, uint64(ref), nodeBytesEntryBytes)
		if err != nil {
			return nil, err
		}
		out[i] = int64(binary.LittleEndian.Uint64(raw))
	}
	return out, nil
}

func (g *graphReader) EvidenceCounts(ctx context.Context, rels []graph.RelRef) ([]int64, error) {
	out := make([]int64, len(rels))
	for i, ref := range rels {
		if ref == 0 || ref > g.maxRelation {
			continue
		}
		raw, err := g.arrayValue(ctx, streamRelEvidence, uint64(ref), evidenceEntryBytes)
		if err != nil {
			return nil, err
		}
		out[i] = int64(binary.LittleEndian.Uint32(raw))
	}
	return out, nil
}

// NodesByID hydrates a page of nodes for delivery through the reader's own
// batched read, so the packed form never duplicates node attributes.
func (g *graphReader) NodesByID(ctx context.Context, ids []model.NodeID) ([]model.Node, error) {
	stored, err := g.r.NodesByID(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make([]model.Node, 0, len(stored))
	for _, s := range stored {
		out = append(out, s.Node)
	}
	return out, nil
}

// EvidenceFor hydrates the evidence identities backing a page of relations.
func (g *graphReader) EvidenceFor(ctx context.Context, relations []model.RelationID,
	limit int) (map[model.RelationID][]model.EvidenceID, error) {
	batched, err := g.r.EvidenceBatch(ctx, relations, limit)
	if err != nil {
		return nil, err
	}
	out := make(map[model.RelationID][]model.EvidenceID, len(batched))
	for rel, rows := range batched {
		ids := make([]model.EvidenceID, 0, len(rows))
		for _, row := range rows {
			ids = append(ids, row.Evidence.ID)
		}
		out[rel] = ids
	}
	return out, nil
}

func (g *graphReader) Capabilities(ctx context.Context) ([]model.CapabilityState, error) {
	return g.r.Capabilities(ctx)
}
