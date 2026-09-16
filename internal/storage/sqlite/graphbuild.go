package sqlite

import (
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"log/slog"
	"runtime"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
)

// The packed per-generation adjacency, built once at activation (ADR-0005
// Decision 1). Two ordered index scans of relation_ids, one per direction, are
// streamed into an offsets directory and an edge stream; three per-node side
// arrays and one per-relation array come from one ordered pass each. Nothing
// here holds the graph: heap is bounded by one part plus one node's list.
//
// Visibility is resolved HERE, once, instead of once per candidate edge per
// page: a relation with no relation_facts row in a member unit is absent from
// the packed form, so a walk never sees it and never pays for a semi-join.

// Stream names. They are the `stream` column of generation_graph_parts and are
// duplicated in that table's CHECK constraint; change both together.
const (
	streamOutOffsets    = "out.off"
	streamOutEdges      = "out.edges"
	streamInOffsets     = "in.off"
	streamInEdges       = "in.edges"
	streamNodeKind      = "node.kind"
	streamNodeContainer = "node.container"
	streamNodeBytes     = "node.bytes"
	streamRelEvidence   = "rel.evidence"
)

// The chunked-stream tables one partWriter serves. They have the same shape --
// key, stream, part, bytes -- and differ only in which row the parts hang off,
// so a partKey names the table and that key column together.
const (
	graphPartsTable   = "generation_graph_parts"
	segmentPartsTable = "lexical_segment_parts"
)

// partKey is the row a chunked stream belongs to.
type partKey struct {
	table  string
	column string
	id     int64
}

func graphKey(gen int64) partKey {
	return partKey{table: graphPartsTable, column: "generation_id", id: gen}
}

// segmentKey names one immutable lexical segment's streams.
func segmentKey(segment int64) partKey {
	return partKey{table: segmentPartsTable, column: "segment_id", id: segment}
}

// Part sizes. These are INTERNAL layout constants, not user limits: they bound
// the working set of a build or a read, and no count of nodes, edges or bytes
// is ever refused because of them -- a stream simply has more parts. They are
// chosen so one part is a comfortable read (1 MiB of edges, 512 KiB of
// offsets) and the reader's bounded window stays a few megabytes.
// edgePartBytes is a var only so a test can shrink it (export_test.go) and
// prove that a list straddling a part boundary is stitched; production never
// writes to it.
var edgePartBytes = 1 << 20 // 1 MiB of encoded edge entries per part

const arrayPartEntries = 65536 // entries per part of an offset or side array

// entryBytes is the width of one entry in each fixed-width stream.
const (
	offsetEntryBytes    = 8 // uint64 byte offset into the edge stream
	kindEntryBytes      = 1 // one node-kind code
	containerEntryBytes = 8 // container surrogate
	nodeBytesEntryBytes = 8 // source size of a file node
	evidenceEntryBytes  = 4 // evidence count of a relation
)

// partSizeFor is the byte size of one full part of a stream. It is the single
// place the reader and the writer agree on chunking, so a part index is always
// offset/partSize.
func partSizeFor(stream string) int {
	switch stream {
	case streamOutEdges, streamInEdges:
		return edgePartBytes
	case streamOutOffsets, streamInOffsets:
		return arrayPartEntries * offsetEntryBytes
	case streamNodeKind:
		return arrayPartEntries * kindEntryBytes
	case streamNodeContainer:
		return arrayPartEntries * containerEntryBytes
	case streamNodeBytes:
		return arrayPartEntries * nodeBytesEntryBytes
	case streamRelEvidence:
		return arrayPartEntries * evidenceEntryBytes
	case streamTermDir, streamTermText, streamPostList:
		return lexPartBytes
	default:
		return 0
	}
}

// containerKinds is the rollup's container vocabulary: only a package or a
// module can hold the container slot, so a `contains` parent that is a
// directory or a file never wins it. It mirrors the engine's own rule.
var containerKinds = []string{string(model.NodePackage), string(model.NodeModule)}

func isContainerKind(kind string) bool { return slices.Contains(containerKinds, kind) }

// partWriter streams one chunked stream into generation_graph_parts. Bytes are
// appended and flushed at exactly one part size, so the reader can index a part
// by arithmetic; only the final part is short. The buffer is the whole heap
// cost of a stream.
type partWriter struct {
	ctx context.Context
	tx  *sql.Tx
	// key is the row the stream belongs to: a generation's adjacency, or one
	// immutable lexical segment. Both chunk the same way, so one writer
	// serves both.
	key    partKey
	stream string
	size   int
	buf    []byte
	part   int
	total  int64
}

func newPartWriter(ctx context.Context, tx *sql.Tx, key partKey, stream string) *partWriter {
	size := partSizeFor(stream)
	return &partWriter{ctx: ctx, tx: tx, key: key, stream: stream, size: size, buf: make([]byte, 0, size)}
}

func (w *partWriter) write(b []byte) error {
	w.total += int64(len(b))
	for len(b) > 0 {
		room := w.size - len(w.buf)
		n := min(room, len(b))
		w.buf = append(w.buf, b[:n]...)
		b = b[n:]
		if len(w.buf) == w.size {
			if err := w.flush(); err != nil {
				return err
			}
		}
	}
	return nil
}

func (w *partWriter) flush() error {
	if len(w.buf) == 0 {
		return nil
	}
	if _, err := w.tx.ExecContext(w.ctx,
		`INSERT INTO `+w.key.table+`(`+w.key.column+`, stream, part, bytes) VALUES(?, ?, ?, ?)`,
		w.key.id, w.stream, w.part, w.buf); err != nil {
		return wrap(w.key.table, err)
	}
	w.part++
	w.buf = w.buf[:0]
	return nil
}

func (w *partWriter) close() error { return w.flush() }

// arrayWriter writes a fixed-width side array in surrogate order, filling the
// gap before each written index with zeros. Zero is the "absent" value of every
// side array -- no kind, no container, no size, no evidence -- so a surrogate
// the generation does not carry needs no row and costs one zeroed entry.
type arrayWriter struct {
	w     *partWriter
	width int
	next  uint64 // the next index the writer will emit
	zero  []byte
	// fill is the value written for a SKIPPED index. It is the zero entry for
	// every side array, where zero means absent. An offsets directory sets it
	// to the CURRENT end of the edge stream before each write, because a node
	// with no list must satisfy offset[i] == offset[i+1]: a zero there would
	// hand the node the whole stream up to its successor's list.
	fill []byte
}

func newArrayWriter(ctx context.Context, tx *sql.Tx, gen int64, stream string, width int) *arrayWriter {
	zero := make([]byte, width)
	return &arrayWriter{w: newPartWriter(ctx, tx, graphKey(gen), stream), width: width, zero: zero, fill: zero}
}

// set writes value at index, padding every skipped index with zeros. Indexes
// must not go backwards; the callers all feed ordered scans.
func (a *arrayWriter) set(index uint64, value []byte) error {
	if err := a.pad(index); err != nil {
		return err
	}
	a.next = index + 1
	return a.w.write(value)
}

// pad zero-fills up to (not including) index.
func (a *arrayWriter) pad(index uint64) error {
	for a.next < index {
		if err := a.w.write(a.fill); err != nil {
			return err
		}
		a.next++
	}
	return nil
}

// closeAt fills through last and flushes, so the array always has exactly
// last+1 entries and the reader never has to special-case a short tail.
func (a *arrayWriter) closeAt(last uint64) error {
	if err := a.pad(last + 1); err != nil {
		return err
	}
	return a.w.close()
}

// packedEdge is one entry of a node's list before encoding.
type packedEdge struct {
	neighbour uint64
	rel       uint64
	kind      byte
}

// buildGraph writes the packed adjacency for gen. It runs inside Activate's
// transaction, before the active pointer flips, so a generation is published
// only with its graph and a failed build fails the activation.
func buildGraph(ctx context.Context, tx *sql.Tx, gen int64) error {
	// The build is the one sequential pass activation adds, and ADR-0005 holds
	// it to 5 % of the index wall clock. That bound is only checkable if the
	// operator can see the pass on its own, so its start and end are logged
	// rather than hidden inside the activation's total.
	//
	// The pass's own memory share is reported beside its wall clock for the
	// same reason: ADR-0005 bounds what the build costs, and a cost stated only
	// in milliseconds cannot answer whether the build is what pushed a run into
	// its memory ceiling. The two ReadMemStats calls that measure it each stop
	// the world, once per activation rather than once per unit -- the price of
	// an operator-visible figure, paid at the rate of a published generation.
	started := time.Now()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	slog.Default().Info("packed adjacency build started", "generation", gen)
	defer func() {
		runtime.ReadMemStats(&after)
		slog.Default().Info("packed adjacency build finished",
			"generation", gen, "duration_ms", time.Since(started).Milliseconds(),
			// Signed: a build that ends after a collection leaves less live
			// heap than it found, and an unsigned subtraction would report that
			// as eighteen exabytes.
			"heap_delta_bytes", int64(after.HeapInuse)-int64(before.HeapInuse))
	}()
	if err := dropGraphTemp(ctx, tx); err != nil {
		return err
	}
	defer dropGraphTemp(ctx, tx) //nolint:errcheck // best-effort cleanup; the build's own error wins.

	// The visible relation ids are materialised FIRST, into a temp table keyed
	// by the relation surrogate. Activation runs before refreshStatistics, so
	// sqlite_stat1 still describes the previous generation (or, on a first
	// index, nothing): an inline EXISTS would leave the planner guessing on
	// every ordered scan below. One pass here makes both scans' plans a
	// covering-index scan plus a primary-key probe, whatever the statistics say.
	if _, err := tx.ExecContext(ctx, `CREATE TEMP TABLE tmp_graph_rel(id INTEGER PRIMARY KEY)`); err != nil {
		return wrap("tmp_graph_rel", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO tmp_graph_rel(id)
		SELECT DISTINCT rf.relation_id FROM relation_facts rf
		JOIN generation_units gu ON gu.unit_id = rf.unit_id AND gu.generation_id = ?`, gen); err != nil {
		return wrap("tmp_graph_rel", err)
	}
	// The generation's member units are materialised the same way, and for the
	// same reason: the evidence count below has to visit every evidence row of
	// every member unit, and asking the planner to reach generation_units per
	// row makes it drive the scan from that table -- 20 000 random index probes
	// into a multi-million-row table, then a temp b-tree to put the groups back
	// in relation order. Against the membership as a temp primary key the scan
	// is an ordered covering walk with two integer probes per row.
	if _, err := tx.ExecContext(ctx, `CREATE TEMP TABLE tmp_graph_unit(id INTEGER PRIMARY KEY)`); err != nil {
		return wrap("tmp_graph_unit", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO tmp_graph_unit(id)
		SELECT unit_id FROM generation_units WHERE generation_id = ?`, gen); err != nil {
		return wrap("tmp_graph_unit", err)
	}
	if _, err := tx.ExecContext(ctx, `CREATE TEMP TABLE tmp_graph_node(id INTEGER PRIMARY KEY)`); err != nil {
		return wrap("tmp_graph_node", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO tmp_graph_node(id)
		SELECT DISTINCT nf.node_id FROM node_facts nf
		JOIN generation_units gu ON gu.unit_id = nf.unit_id AND gu.generation_id = ?`, gen); err != nil {
		return wrap("tmp_graph_node", err)
	}
	// One row per file, naming the container-kind node published for it. It is
	// materialised for the reason the other temp tables are: containerQuery's
	// driving scan is the ordered walk of every visible node, and asking the
	// planner to find each node's container through node_ids on the fly makes
	// it re-choose that driving table once statistics exist. It is also what
	// keeps the pass off the Go heap -- the file-to-container directory is
	// repository-scale, so it lives in the database, not in a map.
	if _, err := tx.ExecContext(ctx, `CREATE TEMP TABLE tmp_graph_file_container(
		file_id BLOB PRIMARY KEY, node_id INTEGER NOT NULL, canonical BLOB NOT NULL)`); err != nil {
		return wrap("tmp_graph_file_container", err)
	}
	containerArgs := make([]any, 0, len(containerKinds))
	for _, k := range containerKinds {
		containerArgs = append(containerArgs, k)
	}
	if _, err := tx.ExecContext(ctx, fileContainerInsert(), containerArgs...); err != nil {
		return wrap("tmp_graph_file_container", err)
	}
	// The settled container of every claimed node. The two claims are folded
	// here, in node order, so buildNodeArrays reads one ordered integer-key
	// scan and the precedence between them is stated once.
	if _, err := tx.ExecContext(ctx, `CREATE TEMP TABLE tmp_graph_node_container(
		node_id INTEGER PRIMARY KEY, container INTEGER NOT NULL, canonical BLOB NOT NULL)`); err != nil {
		return wrap("tmp_graph_node_container", err)
	}
	if _, err := tx.ExecContext(ctx, fileClaimInsert); err != nil {
		return wrap("tmp_graph_node_container", err)
	}
	if _, err := tx.ExecContext(ctx, containsClaimInsert(),
		append([]any{string(model.RelContains)}, containerArgs...)...); err != nil {
		return wrap("tmp_graph_node_container", err)
	}

	var maxNode, maxRelation, nodeCount, edgeCount int64
	if err := tx.QueryRowContext(ctx, `SELECT count(*), coalesce(max(id), 0) FROM tmp_graph_node`).
		Scan(&nodeCount, &maxNode); err != nil {
		return wrap("tmp_graph_node", err)
	}
	if err := tx.QueryRowContext(ctx, `SELECT count(*), coalesce(max(id), 0) FROM tmp_graph_rel`).
		Scan(&edgeCount, &maxRelation); err != nil {
		return wrap("tmp_graph_rel", err)
	}

	relKinds, err := graphDictionary(ctx, tx, `SELECT DISTINCT ri.kind FROM relation_ids ri
		JOIN tmp_graph_rel t ON t.id = ri.id ORDER BY ri.kind`)
	if err != nil {
		return err
	}
	nodeKinds, err := graphDictionary(ctx, tx, `SELECT DISTINCT ni.kind FROM node_ids ni
		JOIN tmp_graph_node t ON t.id = ni.id ORDER BY ni.kind`)
	if err != nil {
		return err
	}
	relCode := codeIndex(relKinds)
	nodeCode := codeIndex(nodeKinds)

	if err := buildDirection(ctx, tx, gen, uint64(maxNode), relCode, true); err != nil {
		return err
	}
	if err := buildDirection(ctx, tx, gen, uint64(maxNode), relCode, false); err != nil {
		return err
	}
	if err := buildNodeArrays(ctx, tx, gen, uint64(maxNode), nodeCode); err != nil {
		return err
	}
	if err := buildEvidenceCounts(ctx, tx, gen, uint64(maxRelation)); err != nil {
		return err
	}

	// Written LAST: the row's presence is the commit marker for every part.
	if _, err := tx.ExecContext(ctx, `INSERT INTO generation_graph(generation_id, max_node, max_relation,
		node_count, edge_count, kinds, node_kinds) VALUES(?, ?, ?, ?, ?, ?, ?)`,
		gen, maxNode, maxRelation, nodeCount, edgeCount,
		[]byte(strings.Join(relKinds, "\x00")), []byte(strings.Join(nodeKinds, "\x00"))); err != nil {
		return wrap("generation_graph", err)
	}
	return nil
}

func dropGraphTemp(ctx context.Context, tx *sql.Tx) error {
	for _, name := range []string{"tmp_graph_rel", "tmp_graph_node", "tmp_graph_unit",
		"tmp_graph_file_container", "tmp_graph_node_container"} {
		if _, err := tx.ExecContext(ctx, `DROP TABLE IF EXISTS temp.`+name); err != nil {
			return wrap(name, err)
		}
	}
	return nil
}

// graphDictionary reads a dense dictionary in ascending spelling order, so a
// code is a function of the kinds the generation carries and not of the order
// rows happened to arrive. Code 1 is entry 0; code 0 is never a kind.
func graphDictionary(ctx context.Context, tx *sql.Tx, query string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, query)
	if err != nil {
		return nil, wrap("generation_graph", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var kind string
		if err := rows.Scan(&kind); err != nil {
			return nil, wrap("generation_graph", err)
		}
		out = append(out, kind)
	}
	return out, wrap("generation_graph", rows.Err())
}

func codeIndex(dict []string) map[string]byte {
	m := make(map[string]byte, len(dict))
	for i, k := range dict {
		m[k] = byte(i + 1)
	}
	return m
}

// outgoingEdgeQuery and incomingEdgeQuery are the two ordered scans the build
// streams. They are separate functions so the query-plan test asserts exactly
// the SQL that ships.
//
// The order is (from_node_id, kind, to_node_id), not (from_node_id,
// to_node_id): relation_ids carries UNIQUE(from_node_id, kind, to_node_id) and
// idx_relations_to(to_node_id, kind, from_node_id), and there is no index on
// the pair alone, so asking for the pair order makes the planner build a
// temporary b-tree over every visible relation. Each node's list is re-sorted
// by (neighbour, relation) in Go before it is encoded, which costs one sort of
// one list and keeps the scan itself a covering-index walk.
func outgoingEdgeQuery() string {
	return `SELECT ri.id, ri.from_node_id, ri.kind, ri.to_node_id FROM relation_ids ri
		JOIN tmp_graph_rel t ON t.id = ri.id
		ORDER BY ri.from_node_id, ri.kind, ri.to_node_id`
}

func incomingEdgeQuery() string {
	return `SELECT ri.id, ri.to_node_id, ri.kind, ri.from_node_id FROM relation_ids ri
		JOIN tmp_graph_rel t ON t.id = ri.id
		ORDER BY ri.to_node_id, ri.kind, ri.from_node_id`
}

// buildDirection streams one direction into its offsets directory and its edge
// stream. Offsets are emitted as the scan passes each owner, so nothing but the
// current owner's list and one part buffer is ever in heap.
func buildDirection(ctx context.Context, tx *sql.Tx, gen int64, maxNode uint64,
	relCode map[string]byte, outgoing bool) error {
	offStream, edgeStream, query := streamInOffsets, streamInEdges, incomingEdgeQuery()
	if outgoing {
		offStream, edgeStream, query = streamOutOffsets, streamOutEdges, outgoingEdgeQuery()
	}
	offsets := newArrayWriter(ctx, tx, gen, offStream, offsetEntryBytes)
	edges := newPartWriter(ctx, tx, graphKey(gen), edgeStream)

	rows, err := tx.QueryContext(ctx, query)
	if err != nil {
		return wrap("relation_ids", err)
	}
	defer rows.Close()

	var owner uint64
	var list []packedEdge
	var scratch [binary.MaxVarintLen64*2 + 1]byte

	// emit encodes one owner's list at the current end of the edge stream and
	// records that position as the owner's offset. Every surrogate the scan
	// skipped shares the same position, which is what makes an empty list
	// offset[i] == offset[i+1].
	emit := func() error {
		if owner == 0 {
			return nil
		}
		sort.Slice(list, func(i, j int) bool {
			if list[i].neighbour != list[j].neighbour {
				return list[i].neighbour < list[j].neighbour
			}
			return list[i].rel < list[j].rel
		})
		pos := make([]byte, offsetEntryBytes)
		binary.LittleEndian.PutUint64(pos, uint64(edges.total))
		offsets.fill = pos
		if err := offsets.set(owner, pos); err != nil {
			return err
		}
		var prev uint64
		for _, e := range list {
			n := binary.PutUvarint(scratch[:], e.neighbour-prev)
			n += binary.PutUvarint(scratch[n:], e.rel)
			scratch[n] = e.kind
			n++
			if err := edges.write(scratch[:n]); err != nil {
				return err
			}
			prev = e.neighbour
		}
		list = list[:0]
		return nil
	}

	for rows.Next() {
		var rel, own, other int64
		var kind string
		if err := rows.Scan(&rel, &own, &kind, &other); err != nil {
			return wrap("relation_ids", err)
		}
		if uint64(own) != owner {
			if err := emit(); err != nil {
				return err
			}
			owner = uint64(own)
		}
		list = append(list, packedEdge{neighbour: uint64(other), rel: uint64(rel), kind: relCode[kind]})
	}
	if err := rows.Err(); err != nil {
		return wrap("relation_ids", err)
	}
	if err := emit(); err != nil {
		return err
	}
	if err := edges.close(); err != nil {
		return err
	}
	// The directory carries one extra entry: offsets[maxNode+1] is the total
	// length, so the last node's list ends where the stream does and the reader
	// needs no special case.
	tail := make([]byte, offsetEntryBytes)
	binary.LittleEndian.PutUint64(tail, uint64(edges.total))
	offsets.fill = tail
	if err := offsets.set(maxNode+1, tail); err != nil {
		return err
	}
	return offsets.w.close()
}

// visibleNodeQuery reads one row per visible node in surrogate order: the
// identity kind and the metadata of the PREFERRED fact, chosen by the same
// precedence the single-node read applies (exact source binding, then provider,
// then the stable unit key), so the side arrays agree with what a hydrated node
// reports.
func visibleNodeQuery() string {
	return `SELECT node_id, kind, metadata_json FROM (
		SELECT nf.node_id AS node_id, ni.kind AS kind, nf.metadata_json AS metadata_json, ` + nodePrecedence + `
		FROM node_facts nf
		JOIN tmp_graph_node t ON t.id = nf.node_id
		JOIN units u ON u.id = nf.unit_id
		JOIN node_ids ni ON ni.id = nf.node_id
		JOIN generation_units gu ON gu.unit_id = nf.unit_id AND gu.generation_id = ?
		GROUP BY nf.node_id
	) ORDER BY node_id`
}

// containerMarks is the parameter list the two container claims bind their
// vocabulary to.
func containerMarks() string {
	marks := make([]string, len(containerKinds))
	for i := range containerKinds {
		marks[i] = "?"
	}
	return strings.Join(marks, ",")
}

// fileContainerInsert fills tmp_graph_file_container: one row per file, naming
// the CONTAINER-KIND node the generation publishes FOR that file.
//
// The tie-break is on the 32-byte canonical id, never on the surrogate --
// surrogates are assigned in the order ids were first interned, so the lowest
// surrogate is a different node from the lowest canonical id whenever a
// repository was indexed in any order but sorted. min() over canonical picks
// the row, and the bare node_id beside it is that row's own surrogate.
func fileContainerInsert() string {
	return `INSERT INTO tmp_graph_file_container(file_id, node_id, canonical)
		SELECT nf.file_id, nf.node_id, min(ni.canonical) FROM node_facts nf
		JOIN tmp_graph_unit u ON u.id = nf.unit_id
		JOIN node_ids ni ON ni.id = nf.node_id
		WHERE nf.file_id IS NOT NULL AND ni.kind IN (` + containerMarks() + `)
		GROUP BY nf.file_id`
}

// fileClaimInsert claims every visible node for the container-kind node that
// owns the node's OWN FILE.
//
// It is what makes the slot answerable at all. A provider attaches a TOP-LEVEL
// declaration to the module that holds it with `defines` and reserves
// `contains` for a NESTED one, so the containment claim below finds no
// container-kind parent anywhere on an ordinary repository -- zero rows, and a
// graph that reads as having no packages -- and even read over `defines` too it
// would still leave every nested declaration unattributed, because its parent
// is the enclosing function. The file is what both cases agree on: a container
// is minted per file and a declaration never nests across files, so the file's
// owner IS the container-kind node at the top of the node's containment chain,
// reached in one ordered scan rather than by climbing.
//
// A node whose file publishes no container-kind node -- and one with no file at
// all, which is every unresolved reference -- is not claimed here, and its slot
// stays zero rather than taking a directory or a file, neither of which answers
// "which package does this symbol belong to".
//
// A node the generation claims from several member units can name several
// files; the same canonical tie-break settles that, so the slot is a function
// of the facts and not of the order the scan met the units.
const fileClaimInsert = `INSERT INTO tmp_graph_node_container(node_id, container, canonical)
	SELECT nf.node_id, m.node_id, min(m.canonical) FROM node_facts nf
	JOIN tmp_graph_node t ON t.id = nf.node_id
	JOIN tmp_graph_unit u ON u.id = nf.unit_id
	JOIN tmp_graph_file_container m ON m.file_id = nf.file_id
	GROUP BY nf.node_id`

// containsClaimInsert overrides that claim wherever a container-kind node
// claims the node DIRECTLY, by a `contains` edge, with the lowest canonical id
// among such claimants. An explicit containment fact is the stronger statement
// of where a node belongs, and it is the rule the in-heap reference reader
// applies, so honouring it first is what keeps the two readers answering alike
// on a graph that states it.
func containsClaimInsert() string {
	return `INSERT INTO tmp_graph_node_container(node_id, container, canonical)
		SELECT ri.to_node_id, ri.from_node_id, min(ni.canonical) FROM relation_ids ri
		JOIN tmp_graph_rel t ON t.id = ri.id
		JOIN node_ids ni ON ni.id = ri.from_node_id
		WHERE ri.kind = ? AND ni.kind IN (` + containerMarks() + `)
		GROUP BY ri.to_node_id
		ON CONFLICT(node_id) DO UPDATE SET container = excluded.container, canonical = excluded.canonical`
}

// containerQuery reads the settled claims in node order. Both claims are folded
// into tmp_graph_node_container before the pass runs, so the build's container
// cursor is an ordered walk of an integer primary key and never a sort.
func containerQuery() string {
	return `SELECT node_id, container, canonical FROM tmp_graph_node_container ORDER BY node_id`
}

// buildNodeArrays streams node.kind, node.container and node.bytes. The two
// ordered cursors -- visible nodes and container claims -- are merge-joined, so
// the pass holds one row of each rather than a map over the node set.
func buildNodeArrays(ctx context.Context, tx *sql.Tx, gen int64, maxNode uint64, nodeCode map[string]byte) error {
	claims, err := tx.QueryContext(ctx, containerQuery())
	if err != nil {
		return wrap("tmp_graph_node_container", err)
	}
	defer claims.Close()
	var claimNode, claimContainer int64
	var claimOK bool
	nextClaim := func() error {
		claimOK = claims.Next()
		if !claimOK {
			return wrap("tmp_graph_node_container", claims.Err())
		}
		var canonical []byte
		return wrap("tmp_graph_node_container", claims.Scan(&claimNode, &claimContainer, &canonical))
	}
	if err := nextClaim(); err != nil {
		return err
	}

	nodes, err := tx.QueryContext(ctx, visibleNodeQuery(), gen)
	if err != nil {
		return wrap("node_facts", err)
	}
	defer nodes.Close()

	kinds := newArrayWriter(ctx, tx, gen, streamNodeKind, kindEntryBytes)
	containers := newArrayWriter(ctx, tx, gen, streamNodeContainer, containerEntryBytes)
	sizes := newArrayWriter(ctx, tx, gen, streamNodeBytes, nodeBytesEntryBytes)

	for nodes.Next() {
		var id int64
		var kind, metadata string
		if err := nodes.Scan(&id, &kind, &metadata); err != nil {
			return wrap("node_facts", err)
		}
		ref := uint64(id)
		if err := kinds.set(ref, []byte{nodeCode[kind]}); err != nil {
			return err
		}

		// A container-kind node IS its own container, so a container-level edge
		// rolls up to the pair it already names.
		var container uint64
		if isContainerKind(kind) {
			container = ref
		} else {
			for claimOK && uint64(claimNode) < ref {
				if err := nextClaim(); err != nil {
					return err
				}
			}
			if claimOK && uint64(claimNode) == ref {
				container = uint64(claimContainer)
			}
		}
		buf := make([]byte, containerEntryBytes)
		binary.LittleEndian.PutUint64(buf, container)
		if err := containers.set(ref, buf); err != nil {
			return err
		}

		if model.NodeKind(kind) == model.NodeFile {
			if size := nodeSourceSize(metadata); size > 0 {
				sz := make([]byte, nodeBytesEntryBytes)
				binary.LittleEndian.PutUint64(sz, uint64(size))
				if err := sizes.set(ref, sz); err != nil {
					return err
				}
			}
		}
	}
	if err := nodes.Err(); err != nil {
		return wrap("node_facts", err)
	}
	if err := kinds.closeAt(maxNode); err != nil {
		return err
	}
	if err := containers.closeAt(maxNode); err != nil {
		return err
	}
	return sizes.closeAt(maxNode)
}

// evidenceCountQuery reads the per-relation occurrence count as ONE ordered
// covering walk of idx_evidence_relation, which is both the group order and the
// output order, so the pass builds no temporary b-tree and reads the evidence
// index once end to end instead of probing it per member unit.
//
// The index is pinned rather than left to the planner. Statistics are rewritten
// after every publication, so by the second generation sqlite_stat1 describes
// evidence and the temp tables do not; without the pin the planner is free to
// re-choose the driving table between one generation and the next, and the
// alternative it reaches for -- membership first, then a per-unit probe -- is
// the plan whose cost grows with the evidence of the whole generation rather
// than with its size, and which cost 85 s of a 103 s build on the reference
// repository against 1.6 s here.
func evidenceCountQuery() string {
	return `SELECT e.relation_id, count(*) FROM evidence e INDEXED BY idx_evidence_relation
		JOIN tmp_graph_rel t ON t.id = e.relation_id
		JOIN tmp_graph_unit gu ON gu.id = e.unit_id
		GROUP BY e.relation_id ORDER BY e.relation_id`
}

// buildEvidenceCounts streams the per-relation occurrence count. A relation
// visible in the generation is backed by at least one occurrence in a member
// unit, so a zero here means "not a relation of this generation".
func buildEvidenceCounts(ctx context.Context, tx *sql.Tx, gen int64, maxRelation uint64) error {
	rows, err := tx.QueryContext(ctx, evidenceCountQuery())
	if err != nil {
		return wrap("evidence", err)
	}
	defer rows.Close()
	counts := newArrayWriter(ctx, tx, gen, streamRelEvidence, evidenceEntryBytes)
	for rows.Next() {
		var rel, n int64
		if err := rows.Scan(&rel, &n); err != nil {
			return wrap("evidence", err)
		}
		// The array is uint32 wide: a count beyond it is clamped rather than
		// wrapped, because a ranking that weighs evidence must never read a
		// huge count as a small one. It is not a limit on anything stored.
		if n > 0xFFFFFFFF {
			n = 0xFFFFFFFF
		}
		buf := make([]byte, evidenceEntryBytes)
		binary.LittleEndian.PutUint32(buf, uint32(n))
		if err := counts.set(uint64(rel), buf); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return wrap("evidence", err)
	}
	return counts.closeAt(maxRelation)
}

// nodeSourceSize reads the size a file node carries in its typed metadata,
// which is the only byte fact the packed form exposes. A missing, unreadable or
// negative size contributes nothing rather than a guess.
func nodeSourceSize(metadata string) int64 {
	if metadata == "" || metadata == "{}" {
		return 0
	}
	var meta struct {
		Size int64 `json:"size"`
	}
	if err := json.Unmarshal([]byte(metadata), &meta); err != nil || meta.Size < 0 {
		return 0
	}
	return meta.Size
}
