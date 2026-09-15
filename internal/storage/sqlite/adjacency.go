package sqlite

import (
	"context"
	"database/sql"
	"strconv"
	"strings"

	"github.com/Sawmonabo/codectx/internal/model"
)

// Batched adjacency for the graph traversal (Section 14). A breadth-first walk
// expands a whole frontier at once, so every read here takes a list of ids and
// answers in one round trip: looping the single-subject readers in query.go
// would issue one statement per frontier node, which is the N+1 the traversal
// budget cannot afford.
//
// Two rules shape the SQL. First, PinnedReader.visible binds the generation as
// the positional parameter ?1, so every other parameter is numbered explicitly
// rather than left to the "?" auto-numbering, which depends on where a marker
// lands in the final statement text. Second, the covering indexes are
// idx_relations_from(from_node_id, ...) and idx_relations_to(to_node_id, ...):
// a "both" direction expressed as an OR across the two columns can use neither,
// so it is a UNION of two indexed scans with the keyset ORDER BY applied
// outside the compound select.

// memberOf is visible() rewritten as a semi-join on one fact alias. The join
// form drives from generation_units, which for an id-keyed batch makes the
// planner walk every unit in the generation; as an EXISTS the batch drives from
// its own id index (idx_node_facts_id, idx_evidence_relation) and probes
// membership per row. It restricts exactly what visible() restricts, and both
// spellings must change together.
func memberOf(alias string) string {
	return ` AND EXISTS (SELECT 1 FROM generation_units gu WHERE gu.unit_id = ` + alias + `.unit_id AND gu.generation_id = ?1)`
}

// maxAdjacencyBatch bounds the ids one batched read accepts. It reuses the
// landed record ceiling rather than introducing a new limit: the traversal's
// frontier batch (256) sits well inside it, and a caller that hands over more
// has lost track of its own bounds.
const maxAdjacencyBatch = model.MaxRecordsPerResult

// binder numbers parameters positionally. ?1 is always the pinned generation,
// because that is the marker PinnedReader.visible embeds.
type binder struct{ args []any }

func newBinder(gen int64) *binder { return &binder{args: []any{gen}} }

// mark appends v and returns the positional marker that reads it. The same
// marker can be repeated in the statement, which is what lets the two branches
// of the "both" UNION share one node list.
func (b *binder) mark(v any) string {
	b.args = append(b.args, v)
	return "?" + strconv.Itoa(len(b.args))
}

// markList appends every value and returns the parenthesised marker list for an
// IN clause.
func (b *binder) markList(values []any) string {
	marks := make([]string, len(values))
	for i, v := range values {
		marks[i] = b.mark(v)
	}
	return "(" + strings.Join(marks, ",") + ")"
}

// batchBlobs decodes and de-duplicates a batch of public ids, preserving the
// first occurrence so the caller's list length bounds the statement.
func batchBlobs(field string, ids []string) ([]any, error) {
	if len(ids) == 0 {
		return nil, invalid("%s batch is empty", field)
	}
	if len(ids) > maxAdjacencyBatch {
		return nil, invalid("%s batch has %d ids, limit %d", field, len(ids), maxAdjacencyBatch)
	}
	out := make([]any, 0, len(ids))
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		raw, err := idBlob(field, id)
		if err != nil {
			return nil, err
		}
		out = append(out, raw)
	}
	return out, nil
}

// kindMarks binds the relation kind filter, rejecting an unknown kind rather
// than silently matching nothing.
func kindMarks(b *binder, kinds []model.RelationKind) (string, error) {
	if len(kinds) == 0 {
		return "", nil
	}
	if len(kinds) > model.MaxFilterValues {
		return "", invalid("relation filter has %d kinds, limit %d", len(kinds), model.MaxFilterValues)
	}
	values := make([]any, len(kinds))
	for i, k := range kinds {
		if !k.Valid() {
			return "", invalid("relation kind %q is not a known relation kind", k)
		}
		values[i] = string(k)
	}
	return " AND ri.kind IN " + b.markList(values), nil
}

// EdgesBatch pages the visible edges touching any node in nodes, keyset-ordered
// by relation id, in one round trip. It is the batched form of Relations: the
// traversal calls it once per frontier, never once per node.
//
// The two semantics graph.Adjacency.Edges freezes hold here: an empty kinds
// slice is "no kind filter", not "no rows", and a non-positive limit is a
// typed argument error rather than an unbounded read. limit above
// model.MaxPageItems is clamped to it.
func (r *PinnedReader) EdgesBatch(ctx context.Context, nodes []model.NodeID, direction model.Direction,
	kinds []model.RelationKind, after model.RelationID, limit int) ([]model.Relation, error) {
	query, args, err := r.edgesBatchQuery(ctx, nodes, direction, kinds, after, limit)
	if err != nil {
		return nil, err
	}
	var out []model.Relation
	err = r.s.read(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, query, args...)
		if err != nil {
			return wrap("relation_facts", err)
		}
		defer rows.Close()
		for rows.Next() {
			var id, from, to []byte
			var rel model.Relation
			if err := rows.Scan(&id, &from, &rel.Kind, &to); err != nil {
				return wrap("relation_facts", err)
			}
			rel.ID, rel.From, rel.To = model.RelationID(idHex(id)), model.NodeID(idHex(from)), model.NodeID(idHex(to))
			out = append(out, rel)
		}
		return wrap("relation_facts", rows.Err())
	})
	return out, err
}

// edgesBatchQuery builds the statement EdgesBatch runs. It is separate so the
// query plan can be asserted against exactly the SQL that ships.
func (r *PinnedReader) edgesBatchQuery(ctx context.Context, nodes []model.NodeID, direction model.Direction,
	kinds []model.RelationKind, after model.RelationID, limit int) (string, []any, error) {
	if limit < 0 {
		return "", nil, invalid("edge batch limit cannot be negative")
	}
	// A zero limit is "no caller-side bound", which pageLimit resolves to the
	// page size the storage layer serves anyway. Refusing it made 0 mean
	// "broken" in the one place the rest of the tree now reads as "unlimited".
	limit = pageLimit(ctx, limit)
	if !direction.Valid() {
		return "", nil, invalid("direction %q is not a known direction", direction)
	}
	raw := make([]string, len(nodes))
	for i, n := range nodes {
		raw[i] = string(n)
	}
	values, err := batchBlobs("node_id", raw)
	if err != nil {
		return "", nil, err
	}
	afterRaw, err := optionalBlob("after", string(after))
	if err != nil {
		return "", nil, err
	}
	b := newBinder(r.gen)
	nodeList := b.markList(values)
	kindClause, err := kindMarks(b, kinds)
	if err != nil {
		return "", nil, err
	}
	keyset := ""
	if afterRaw != nil {
		keyset = " AND ri.id > " + b.mark(afterRaw)
	}
	limitMark := b.mark(limit)

	// One indexed scan per direction column; an OR across the two columns would
	// use neither index. The EXISTS applies the same membership rule visible()
	// applies, as a semi-join so the scan stays on the relation index.
	branch := func(column string) string {
		return `SELECT ri.id AS id, ri.from_node_id AS from_node_id, ri.kind AS kind, ri.to_node_id AS to_node_id
			FROM relation_ids ri WHERE ri.` + column + ` IN ` + nodeList + kindClause + keyset + `
			AND EXISTS (SELECT 1 FROM relation_facts rf WHERE rf.relation_id = ri.id` + memberOf("rf") + `)
			ORDER BY ri.id LIMIT ` + limitMark
	}
	var query string
	switch direction {
	case model.DirectionOutgoing:
		query = branch("from_node_id")
	case model.DirectionIncoming:
		query = branch("to_node_id")
	default:
		// Each branch is already keyset-bounded, so the union of their first
		// `limit` rows contains the union's first `limit` rows; the outer sort
		// merges them back into relation-id order.
		query = `SELECT id, from_node_id, kind, to_node_id FROM (
			SELECT * FROM (` + branch("from_node_id") + `) UNION SELECT * FROM (` + branch("to_node_id") + `)
		) ORDER BY id LIMIT ` + limitMark
	}
	return query, b.args, nil
}

// nodeBatchColumns is nodeColumns with explicit result names so the grouped
// subquery can be projected by name. It must stay column-for-column identical
// to nodeColumns, which is what scanNode reads.
const nodeBatchColumns = `nf.node_id AS node_id, ni.kind AS kind, nf.language AS language, nf.name AS name,
	nf.qualified_name AS qualified_name, nf.signature AS signature, nf.file_id AS file_id,
	ui.content_hash AS content_hash, nf.start_byte AS start_byte, nf.end_byte AS end_byte,
	nf.metadata_json AS metadata_json, u.unit_key AS unit_key, u.source_binding AS source_binding`

const nodeBatchOuterColumns = `node_id, kind, language, name, qualified_name, signature, file_id, content_hash,
	start_byte, end_byte, metadata_json, unit_key, source_binding`

// nodePrecedence is nodeOrder expressed as a single sortable key so one row per
// node can be chosen by aggregation instead of by a per-node statement: exact
// source binding first, then provider id, then the stable unit key. A query
// whose only aggregate is min() takes its bare columns from the matching row,
// so the projected fact is the preferred one.
const nodePrecedence = `min(CASE u.source_binding WHEN 'verified' THEN '0' ELSE '1' END || char(31) || u.provider_id
	|| char(31) || lower(hex(u.unit_key))) AS precedence`

// NodesByID hydrates a batch of node ids into their preferred visible facts,
// ordered by node id. Ids that are not visible in the pinned generation are
// absent from the result rather than an error: a traversal frontier legitimately
// contains edge endpoints whose node facts this generation does not publish.
func (r *PinnedReader) NodesByID(ctx context.Context, ids []model.NodeID) ([]StoredNode, error) {
	query, args, err := r.nodesByIDQuery(ids)
	if err != nil {
		return nil, err
	}
	var out []StoredNode
	err = r.s.read(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, query, args...)
		if err != nil {
			return wrap("node_facts", err)
		}
		defer rows.Close()
		for rows.Next() {
			n, err := scanNode(rows)
			if err != nil {
				return err
			}
			out = append(out, n)
		}
		return wrap("node_facts", rows.Err())
	})
	return out, err
}

// nodesByIDQuery builds the statement NodesByID runs, separately so the query
// plan can be asserted against exactly the SQL that ships.
func (r *PinnedReader) nodesByIDQuery(ids []model.NodeID) (string, []any, error) {
	raw := make([]string, len(ids))
	for i, id := range ids {
		raw[i] = string(id)
	}
	values, err := batchBlobs("node_id", raw)
	if err != nil {
		return "", nil, err
	}
	b := newBinder(r.gen)
	nodeList := b.markList(values)
	limitMark := b.mark(len(values))
	query := `SELECT ` + nodeBatchOuterColumns + ` FROM (
		SELECT ` + nodeBatchColumns + `, ` + nodePrecedence + ` FROM node_facts nf
		JOIN units u ON u.id = nf.unit_id
		JOIN node_ids ni ON ni.id = nf.node_id
		LEFT JOIN unit_inputs ui ON ui.unit_id = nf.unit_id AND ui.file_id = nf.file_id
		WHERE nf.node_id IN ` + nodeList + memberOf("nf") + ` GROUP BY nf.node_id
	) ORDER BY node_id LIMIT ` + limitMark
	return query, b.args, nil
}

// evidenceBatchColumns names every column StoredEvidence needs, aliased so the
// windowed subquery can be projected by name.
const evidenceBatchColumns = `e.id AS id, u.unit_key AS unit_key, u.provider_id AS provider_id,
	u.provider_version AS provider_version, u.origin_run_id AS origin_run_id, e.node_id AS node_id,
	e.relation_id AS relation_id, e.precision AS precision, e.file_id AS file_id,
	ui.content_hash AS content_hash, e.start_byte AS start_byte, e.end_byte AS end_byte,
	e.native_key AS native_key, e.detail AS detail`

const evidenceBatchOuterColumns = `id, unit_key, provider_id, provider_version, origin_run_id, node_id,
	relation_id, precision, file_id, content_hash, start_byte, end_byte, native_key, detail`

// EvidenceBatch returns the visible evidence backing each relation in one round
// trip, at most perRelation rows per relation in evidence-id order. The per-
// relation cap is applied inside SQL, so one heavily evidenced relation cannot
// consume the whole batch and starve the relations after it. perRelation is an
// internal hydration cap rather than a frozen request field, so zero takes the
// model.MaxPageItems default the rest of the reader uses.
func (r *PinnedReader) EvidenceBatch(ctx context.Context, relations []model.RelationID, perRelation int) (map[model.RelationID][]StoredEvidence, error) {
	query, args, err := r.evidenceBatchQuery(ctx, relations, perRelation)
	if err != nil {
		return nil, err
	}
	out := make(map[model.RelationID][]StoredEvidence, len(relations))
	err = r.s.read(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, query, args...)
		if err != nil {
			return wrap("evidence", err)
		}
		defer rows.Close()
		for rows.Next() {
			se, relation, err := scanBatchedEvidence(rows)
			if err != nil {
				return err
			}
			out[relation] = append(out[relation], se)
		}
		return wrap("evidence", rows.Err())
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// evidenceBatchQuery builds the statement EvidenceBatch runs, separately so the
// query plan can be asserted against exactly the SQL that ships.
func (r *PinnedReader) evidenceBatchQuery(ctx context.Context, relations []model.RelationID, perRelation int) (string, []any, error) {
	perRelation = pageLimit(ctx, perRelation)
	raw := make([]string, len(relations))
	for i, id := range relations {
		raw[i] = string(id)
	}
	values, err := batchBlobs("relation_id", raw)
	if err != nil {
		return "", nil, err
	}
	b := newBinder(r.gen)
	relList := b.markList(values)
	perMark := b.mark(perRelation)
	query := `SELECT ` + evidenceBatchOuterColumns + ` FROM (
		SELECT ` + evidenceBatchColumns + `, row_number() OVER (PARTITION BY e.relation_id ORDER BY e.id) AS rn
		FROM evidence e
		JOIN units u ON u.id = e.unit_id
		LEFT JOIN unit_inputs ui ON ui.unit_id = e.unit_id AND ui.file_id = e.file_id
		WHERE e.relation_id IN ` + relList + memberOf("e") + `
	) WHERE rn <= ` + perMark + ` ORDER BY relation_id, id`
	return query, b.args, nil
}

// scanBatchedEvidence reads one row of evidenceBatchOuterColumns. It mirrors the
// scan in Evidence, which is not factored out there and which this file may not
// edit; the column lists are kept adjacent so a schema change breaks both.
func scanBatchedEvidence(rows *sql.Rows) (StoredEvidence, model.RelationID, error) {
	var se StoredEvidence
	var id, unitKey, run, nodeID, relID, fileID, contentHash []byte
	var start, end sql.NullInt64
	if err := rows.Scan(&id, &unitKey, &se.Evidence.ProviderID, &se.Evidence.ProviderVersion, &run, &nodeID, &relID,
		&se.Evidence.Precision, &fileID, &contentHash, &start, &end, &se.Evidence.NativeKey, &se.Evidence.Detail); err != nil {
		return se, "", wrap("evidence", err)
	}
	se.Evidence.ID, se.Evidence.UnitID, se.Evidence.OriginRunID = model.EvidenceID(idHex(id)), model.UnitID(idHex(unitKey)), model.ProviderRunID(idHex(run))
	if nodeID != nil {
		se.Evidence.NodeID = model.NodeID(idHex(nodeID))
	}
	se.Evidence.RelationID = model.RelationID(idHex(relID))
	if fileID != nil {
		se.Evidence.FileID, se.Evidence.ContentHash = model.FileID(idHex(fileID)), idHex(contentHash)
	}
	if start.Valid {
		se.Bytes = &model.ByteRange{Start: uint64(start.Int64), End: uint64(end.Int64)}
	}
	return se, se.Evidence.RelationID, nil
}
