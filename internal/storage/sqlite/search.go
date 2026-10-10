package sqlite

import (
	"context"
	"database/sql"

	"github.com/Sawmonabo/codectx/internal/model"
)

// FileByPath resolves a normalized path to its file identity; unknown is
// CTX_ARGUMENT_INVALID, not an empty result.
func (r *PinnedReader) FileByPath(ctx context.Context, path string) (model.FileID, error) {
	if path == "" {
		return "", invalid("path must not be empty")
	}
	if len(path) > model.MaxPathBytes {
		return "", invalid("path is %d bytes, limit %d", len(path), model.MaxPathBytes)
	}
	var raw []byte
	err := r.s.read(ctx, func(tx *sql.Tx) error {
		// A file belongs to the pinned generation when some visible unit was
		// built over it, which is what makes an unknown path and a path
		// outside this generation the same answer.
		err := tx.QueryRowContext(ctx, `SELECT f.id FROM files f WHERE f.repository_id = ?2 AND f.path = ?3
			AND EXISTS (SELECT 1 FROM unit_inputs ui JOIN generation_units gu ON gu.unit_id = ui.unit_id
				WHERE ui.file_id = f.id AND gu.generation_id = ?1) LIMIT 1`, r.gen, r.repo, path).Scan(&raw)
		if isNoRows(err) {
			return invalid("path %q is not a file of generation %d", path, r.gen)
		}
		return wrap("files", err)
	})
	if err != nil {
		return "", err
	}
	return model.FileID(idHex(raw)), nil
}

// NodesInFile pages visible node facts declared in file, keyset on
// (start_byte, node_id), through idx_nodes_file. Every declared offset of a
// node is returned; DistinctNodesInFile is the one-row-per-node reading.
func (r *PinnedReader) NodesInFile(ctx context.Context, file model.FileID, afterStart int64, after model.NodeID, limit int) ([]StoredNode, error) {
	return r.nodesInFile(ctx, file, afterStart, after, limit, false)
}

// DistinctNodesInFile is NodesInFile restricted to ONE row per node: the row
// of the unit the Section 9.4 precedence order picks for that node, at that
// unit's offset. It exists because a retrieval tier that emits candidates by
// identity must not emit one node twice, while document-symbols keeps every
// declared offset; the two readings are separate methods so neither narrows
// the other.
//
// node_facts is keyed (unit_id, node_id), so a node at two offsets in one file
// is always two UNITS disagreeing about where it is declared, never two
// declarations of one unit. Taking the precedence winner's row -- offset and
// attributes from the same unit -- is what Nodes and Containers already do for
// the same disagreement (query.go:252, query.go:297), so an identity read of a
// node reports the same byte range whichever of them serves it.
//
// The order and the (start_byte, node_id) keyset are NodesInFile's, unchanged:
// each node now appears at exactly one offset, so the key stays total and
// exact across pages.
func (r *PinnedReader) DistinctNodesInFile(ctx context.Context, file model.FileID, afterStart int64, after model.NodeID, limit int) ([]StoredNode, error) {
	return r.nodesInFile(ctx, file, afterStart, after, limit, true)
}

func (r *PinnedReader) nodesInFile(ctx context.Context, file model.FileID, afterStart int64, after model.NodeID, limit int, distinct bool) ([]StoredNode, error) {
	limit = pageLimit(ctx, limit)
	fileRaw, err := idBlob("file_id", string(file))
	if err != nil {
		return nil, err
	}
	if afterStart < 0 {
		return nil, invalid("after start byte must not be negative")
	}
	afterRaw, err := optionalBlob("after", string(after))
	if err != nil {
		return nil, err
	}
	// Keyset on the declared order, exclusive of (afterStart, after). An empty
	// after is the first page and carries no key, so afterStart is not a
	// predicate of its own -- a node declared at byte 0 must not be skipped.
	//
	// node_facts permits a file-scoped fact with no byte range (schema.sql:178:
	// a null start_byte is legal beside a non-null file_id), and the proof
	// store contains such nodes. Dropping them would silently lose declarations
	// from a file's symbol list, so they sort with the offset they are paged
	// by: zero. node_id then totally orders the group, so paging stays exact.
	const startKey = "coalesce(nf.start_byte, 0)"
	keyset := ""
	args := []any{r.gen, fileRaw}
	if afterRaw != nil {
		// ?4 is the canonical NodeID the cursor carries, matched against the
		// dictionary column, never against node_facts.node_id -- that column is
		// a rebuild-local surrogate.
		keyset = " AND (" + startKey + " > ?3 OR (" + startKey + " = ?3 AND ni.canonical > ?4))"
		args = append(args, afterStart, afterRaw)
	}
	// Duplicate node ids at one offset are collapsed by the Section 9.4
	// precedence order -- verified source binding, then provider id, then unit
	// key -- which is the read-time mechanism; it is not
	// reimplemented here. The grouping key is the keyset key itself, so the
	// survivor of a group never straddles a page boundary. The same node at
	// two different offsets is two units disagreeing about the declaration:
	// both rows are returned here, and DistinctNodesInFile keeps only the one
	// the same precedence order picks.
	//
	// The distinct predicate names its own gu2/u2 aliases rather than reusing
	// visible(), whose u would shadow the outer unit row the ORDER BY reads.
	// It correlates on the outer node and file and reuses ?1 and ?2, so it
	// binds no argument of its own, and it resolves through
	// idx_node_facts_id(node_id, unit_id) -- the handful of units publishing
	// that one node, not a scan.
	distinctOnly := ""
	if distinct {
		distinctOnly = ` AND ` + startKey + ` = (SELECT coalesce(nf2.start_byte, 0) FROM node_facts nf2
			JOIN generation_units gu2 ON gu2.unit_id = nf2.unit_id AND gu2.generation_id = ?1
			JOIN units u2 ON u2.id = gu2.unit_id
			WHERE nf2.file_id = nf.file_id AND nf2.node_id = nf.node_id
			ORDER BY CASE u2.source_binding WHEN 'verified' THEN 0 ELSE 1 END, u2.provider_id, u2.unit_key LIMIT 1)`
	}
	query := `SELECT ` + nodeColumns + ` FROM node_facts nf` + r.visible("nf") +
		`JOIN node_ids ni ON ni.id = nf.node_id LEFT JOIN unit_inputs ui ON ui.unit_id = nf.unit_id AND ui.file_id = nf.file_id
		WHERE nf.file_id = ?2` + keyset + distinctOnly + `
		ORDER BY ` + startKey + `, ni.canonical, CASE u.source_binding WHEN 'verified' THEN 0 ELSE 1 END, u.provider_id, u.unit_key`
	var out []StoredNode
	err = r.s.read(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, query, args...)
		if err != nil {
			return wrap("node_facts", err)
		}
		defer rows.Close()
		var lastID model.NodeID
		var lastStart uint64
		for rows.Next() {
			n, err := scanNode(rows)
			if err != nil {
				return err
			}
			var start uint64
			if n.Bytes != nil {
				start = n.Bytes.Start
			}
			if len(out) > 0 && n.Node.ID == lastID && start == lastStart {
				continue
			}
			lastID, lastStart = n.Node.ID, start
			out = append(out, n)
			if len(out) == limit {
				break
			}
		}
		return wrap("node_facts", rows.Err())
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
