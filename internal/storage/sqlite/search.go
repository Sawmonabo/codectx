package sqlite

import (
	"context"
	"database/sql"
	"strconv"
	"strings"

	"github.com/Sawmonabo/codectx/internal/model"
)

// SearchColumn names one indexed column of search_fts, in its declared order.
type SearchColumn string

const (
	ColumnName          SearchColumn = "name"
	ColumnQualifiedName SearchColumn = "qualified_name"
	ColumnSignature     SearchColumn = "signature"
	ColumnPath          SearchColumn = "path"
	ColumnBody          SearchColumn = "body"
)

// MaxTermOffsets bounds the offsets one TermOccurrence carries.
const MaxTermOffsets = 512

// TermOccurrence is one term's instances in one column of one visible
// document. Offsets ascend, truncated at MaxTermOffsets; a phrase count from
// truncated offsets is a lower bound, which Truncated says.
type TermOccurrence struct {
	RowID     int64
	Column    SearchColumn
	Count     int64
	Offsets   []int64
	Truncated bool
}

// SearchDocument is a visible lexical document WITHOUT its body: Section 14.2
// forbids source bodies in generic results. Ranking and hydration use it.
type SearchDocument struct {
	RowID         int64
	ID            string
	NodeID        model.NodeID
	FileID        model.FileID
	Path          string
	Kind          model.NodeKind
	Name          string
	QualifiedName string
	Signature     string
	Bytes         model.ByteRange
	TokenCount    int64
}

// searchColumns is the declared column order of search_fts. A vocabulary row
// names its column as text, so this is also the validation set: an unknown
// name means the index and this binary disagree about the schema.
var searchColumns = map[string]SearchColumn{
	string(ColumnName):          ColumnName,
	string(ColumnQualifiedName): ColumnQualifiedName,
	string(ColumnSignature):     ColumnSignature,
	string(ColumnPath):          ColumnPath,
	string(ColumnBody):          ColumnBody,
}

// searchDocumentColumns is every search_units column except body: Section 14.2
// forbids source bodies in generic results, and a body would also dominate the
// memory one hydrated page costs.
const searchDocumentColumns = `su.rowid, su.search_key, ni.canonical, su.file_id, su.path, su.kind, su.name,
	su.qualified_name, su.signature, su.start_byte, su.end_byte, su.token_count`

// visibleDocument restricts a search_units alias to the pinned generation
// without joining units. It is the EXISTS form Match already uses: the planner
// then drives the query from its own most selective index (the FTS index, the
// files unique index, the vocabulary) and probes membership per candidate,
// instead of walking every unit of the generation first.
func (r *PinnedReader) visibleDocument(alias string) string {
	return ` AND EXISTS (SELECT 1 FROM generation_units gu WHERE gu.unit_id = ` + alias + `.unit_id AND gu.generation_id = ?1) `
}

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
// (start_byte, node_id), through idx_nodes_file.
func (r *PinnedReader) NodesInFile(ctx context.Context, file model.FileID, afterStart int64, after model.NodeID, limit int) ([]StoredNode, error) {
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
		// now a rebuild-local surrogate (scale-posture-plan.md 3d).
		keyset = " AND (" + startKey + " > ?3 OR (" + startKey + " = ?3 AND ni.canonical > ?4))"
		args = append(args, afterStart, afterRaw)
	}
	// Duplicate node ids at one offset are collapsed by the Section 9.4
	// precedence order -- verified source binding, then provider id, then unit
	// key -- which is the read-time mechanism ruling Q8 names; it is not
	// reimplemented here. The grouping key is the keyset key itself, so the
	// survivor of a group never straddles a page boundary. The same node at
	// two different offsets is two distinct declarations and both are returned.
	query := `SELECT ` + nodeColumns + ` FROM node_facts nf` + r.visible("nf") +
		`JOIN node_ids ni ON ni.id = nf.node_id LEFT JOIN unit_inputs ui ON ui.unit_id = nf.unit_id AND ui.file_id = nf.file_id
		WHERE nf.file_id = ?2` + keyset + `
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

// DocumentFrequency returns, per term in order, how many visible documents
// contain it; the caller caps len(terms) at resources.max_query_terms.
func (r *PinnedReader) DocumentFrequency(ctx context.Context, terms []string) ([]int64, error) {
	if len(terms) == 0 {
		return nil, invalid("document frequency needs at least one term")
	}
	if len(terms) > model.MaxFilterValues {
		return nil, invalid("document frequency asked for %d terms, limit %d", len(terms), model.MaxFilterValues)
	}
	marks := make([]string, len(terms))
	args := []any{r.gen}
	for i, t := range terms {
		if t == "" {
			return nil, invalid("document frequency term %d is empty", i)
		}
		// Numbered, because the visibility clause that follows binds ?1 and
		// SQLite would otherwise number these anonymous marks from 1 too.
		marks[i] = "?" + strconv.Itoa(i+2)
		args = append(args, t)
	}
	// One statement for every term: a per-term round trip would be N+1 against
	// the vocabulary, which is the largest table a query touches.
	query := `SELECT v.term, count(DISTINCT v.doc) FROM search_vocab v
		JOIN search_units su ON su.rowid = v.doc
		WHERE v.term IN (` + strings.Join(marks, ",") + `)` + r.visibleDocument("su") + `GROUP BY v.term`
	counts := make(map[string]int64, len(terms))
	err := r.s.read(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, query, args...)
		if err != nil {
			return wrap("search_vocab", err)
		}
		defer rows.Close()
		for rows.Next() {
			var term string
			var n int64
			if err := rows.Scan(&term, &n); err != nil {
				return wrap("search_vocab", err)
			}
			counts[term] = n
		}
		return wrap("search_vocab", rows.Err())
	})
	if err != nil {
		return nil, err
	}
	// Input order, zero for a term no visible document carries: the caller
	// indexes this slice by the position of its own term.
	out := make([]int64, len(terms))
	for i, t := range terms {
		out[i] = counts[t]
	}
	return out, nil
}

// TermOccurrences pages visible documents containing term, keyset on rowid,
// one row per (document, column), ordered by (rowid, column).
func (r *PinnedReader) TermOccurrences(ctx context.Context, term string, after int64, limit int) ([]TermOccurrence, error) {
	limit = pageLimit(ctx, limit)
	if term == "" {
		return nil, invalid("term must not be empty")
	}
	if after < 0 {
		return nil, invalid("after rowid must not be negative")
	}
	// Every instance of the term in a document is scanned so Count is exact
	// even where Offsets is truncated; a page ends only on a document
	// boundary, because the keyset is the rowid and a half-read document's
	// remaining columns could never be paged back. SQLite sorts the whole
	// term's instance list per page, so a full walk of the corpus's most
	// frequent term costs one sort per page: measured at 41 pages / 105 ms for
	// df 7796 on the proof store, against a 10 s query timeout.
	query := `SELECT v.doc, v.col, v.offset FROM search_vocab v
		JOIN search_units su ON su.rowid = v.doc
		WHERE v.term = ?2 AND v.doc > ?3` + r.visibleDocument("su") + `ORDER BY v.doc, v.col, v.offset`
	var out []TermOccurrence
	err := r.s.read(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, query, r.gen, term, after)
		if err != nil {
			return wrap("search_vocab", err)
		}
		defer rows.Close()
		var doc int64
		for rows.Next() {
			var rowid, offset int64
			var col string
			if err := rows.Scan(&rowid, &col, &offset); err != nil {
				return wrap("search_vocab", err)
			}
			column, ok := searchColumns[col]
			if !ok {
				return corrupt("search_vocab names column %q, which search_fts does not declare", col)
			}
			if rowid != doc {
				if len(out) >= limit {
					break
				}
				doc = rowid
			}
			if n := len(out); n > 0 && out[n-1].RowID == rowid && out[n-1].Column == column {
				o := &out[n-1]
				o.Count++
				if len(o.Offsets) < MaxTermOffsets {
					o.Offsets = append(o.Offsets, offset)
				} else {
					o.Truncated = true
				}
				continue
			}
			out = append(out, TermOccurrence{RowID: rowid, Column: column, Count: 1, Offsets: []int64{offset}})
		}
		return wrap("search_vocab", rows.Err())
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// SearchDocuments hydrates visible documents by rowid in one bounded query
// (len(rowids) <= model.MaxPageItems). Missing rowids are omitted.
func (r *PinnedReader) SearchDocuments(ctx context.Context, rowids []int64) ([]SearchDocument, error) {
	if len(rowids) == 0 {
		return nil, nil
	}
	if len(rowids) > model.MaxPageItems {
		return nil, invalid("search document hydration asked for %d rowids, limit %d", len(rowids), model.MaxPageItems)
	}
	marks := make([]string, len(rowids))
	args := []any{r.gen}
	for i, id := range rowids {
		if id <= 0 {
			return nil, invalid("search document rowid %d is not positive", id)
		}
		marks[i] = "?" + strconv.Itoa(i+2)
		args = append(args, id)
	}
	query := `SELECT ` + searchDocumentColumns + ` FROM search_units su
		LEFT JOIN node_ids ni ON ni.id = su.node_id
		WHERE su.rowid IN (` + strings.Join(marks, ",") + `)` + r.visibleDocument("su")
	byRowID := make(map[int64]SearchDocument, len(rowids))
	err := r.s.read(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, query, args...)
		if err != nil {
			return wrap("search_units", err)
		}
		defer rows.Close()
		for rows.Next() {
			var d SearchDocument
			var key, node, file []byte
			var start, end int64
			if err := rows.Scan(&d.RowID, &key, &node, &file, &d.Path, &d.Kind, &d.Name, &d.QualifiedName,
				&d.Signature, &start, &end, &d.TokenCount); err != nil {
				return wrap("search_units", err)
			}
			d.ID, d.FileID = idHex(key), model.FileID(idHex(file))
			if node != nil {
				d.NodeID = model.NodeID(idHex(node))
			}
			d.Bytes = model.ByteRange{Start: uint64(start), End: uint64(end)}
			byRowID[d.RowID] = d
		}
		return wrap("search_units", rows.Err())
	})
	if err != nil {
		return nil, err
	}
	// Requested order, so a ranked page hydrates into the order it was ranked
	// in; a rowid the generation does not contain is omitted, not zeroed.
	out := make([]SearchDocument, 0, len(rowids))
	for _, id := range rowids {
		if d, ok := byRowID[id]; ok {
			out = append(out, d)
		}
	}
	return out, nil
}
