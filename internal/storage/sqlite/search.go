package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
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
	// ColumnBody is an indexed column of search_fts; it is not a stored
	// column of search_units, which keeps no copy of the source text.
	ColumnBody SearchColumn = "body"
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

// SearchDocument is a visible lexical document without its body: Section 14.2
// forbids source bodies in generic results, and the database stores no body to
// return (ADR-0003 §2.1). Ranking and hydration use it.
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

// searchDocumentColumns is every search_units column a hydrated page needs.
// No body appears because none is stored (ADR-0003 §2.1): Section 14.2 forbids
// source bodies in generic results, and a body would also dominate the memory
// one hydrated page costs.
const searchDocumentColumns = `su.doc_id, su.search_key, ni.canonical, su.file_id, su.path, su.kind, su.name,
	su.qualified_name, su.signature, su.start_byte, su.end_byte, su.token_count`

// visibleDocument restricts a search_units alias to the pinned generation
// without joining units. It is also what keeps a doc_id single-valued: a
// posting shared along a carry chain has one row per unit of that chain, and
// only one of them is a member of any generation. It is the EXISTS form Match already uses: the planner
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
		// now a rebuild-local surrogate (scale-posture-plan.md 3d).
		keyset = " AND (" + startKey + " > ?3 OR (" + startKey + " = ?3 AND ni.canonical > ?4))"
		args = append(args, afterStart, afterRaw)
	}
	// Duplicate node ids at one offset are collapsed by the Section 9.4
	// precedence order -- verified source binding, then provider id, then unit
	// key -- which is the read-time mechanism ruling Q8 names; it is not
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

// PostingSession is one read transaction dedicated to a query's term streams.
// Every stream it opens reads the same snapshot, and that snapshot is held for
// the whole candidate walk instead of being re-taken per page. It runs on the
// posting pool, never the reader pool, so a query that holds a session can
// still issue the short reads (Match, SearchDocuments) the same walk needs.
// Close rolls the transaction back and is safe to call more than once; closing
// it also closes every stream still open on it.
type PostingSession struct {
	r   *PinnedReader
	tx  *sql.Tx
	lex *lexicalIndex
}

// OpenPostings begins a posting session for this generation.
func (r *PinnedReader) OpenPostings(ctx context.Context) (*PostingSession, error) {
	tx, err := r.s.postings.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, wrap("begin postings", err)
	}
	return &PostingSession{r: r, tx: tx}, nil
}

// Close ends the session. Cancelling or timing out the context the streams
// were opened with makes their statements fail and the caller unwind to this
// Close, which is what releases the connection back to the posting pool.
func (p *PostingSession) Close() error {
	tx := p.tx
	if tx == nil {
		return nil
	}
	p.tx = nil
	if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
		return wrap("postings", err)
	}
	return nil
}

// occurrenceQuery reads one term's whole posting list as a single statement.
// There is deliberately no ORDER BY: fts5vocab('instance') emits a term's
// instances in doclist order, which is document ascending and, within a
// document, column ordinal then offset ascending. Asking SQLite for that order
// makes it materialize the term's entire instance list in a temp b-tree before
// the first row, which for a corpus-frequent term is both the dominant cost of
// a query and unbounded memory. CROSS JOIN pins search_vocab as the outer loop
// so the emission order is the scan order; OccurrenceStream verifies the order
// it actually receives rather than trusting it.
//
// search_units is joined on doc_id, not rowid: the lexical document a posting
// names is the contentless index's own key (ADR-0003 §2.1), and a delta
// carry-over shares it along a carry chain, so several rows may name one
// document. idx_search_doc resolves the inner side, and visibleDocument narrows
// it to this generation -- at most one surviving row per document, so the join
// neither drops nor duplicates an instance.
const occurrenceQuery = `SELECT v.doc, v.col, v.offset FROM search_vocab v
		CROSS JOIN search_units su ON su.doc_id = v.doc
		WHERE v.term = ?2`

// TermOccurrences opens a stream over every visible instance of term, one row
// per (document, column), documents ascending. The stream lives until Close or
// until the session ends.
func (p *PostingSession) TermOccurrences(ctx context.Context, term string) (*OccurrenceStream, error) {
	if term == "" {
		return nil, invalid("term must not be empty")
	}
	if p.tx == nil {
		return nil, internal("posting session is already closed")
	}
	rows, err := p.tx.QueryContext(ctx, occurrenceQuery+p.r.visibleDocument("su"), p.r.gen, term)
	if err != nil {
		return nil, wrap("search_vocab", err)
	}
	return &OccurrenceStream{term: term, rows: rows, engine: p.r.s.Version}, nil
}

// OccurrenceStream pulls one term's posting list in page-sized refills from a
// single statement, so peak memory is one page no matter how frequent the term
// is. A refill always ends on a document boundary, with the first row of the
// next document carried as lookahead: cutting a document in half would split
// its (column, offsets) groups and understate Count.
type OccurrenceStream struct {
	term string
	rows *sql.Rows
	// head is the row read past the end of the last refill, still unemitted.
	head    rawOccurrence
	have    bool
	drained bool
	// prev is the last row scanned, for the emission-order check.
	prev    rawOccurrence
	started bool
	// engine is sqlite_version() as observed at open, named in the order
	// violation so an operator can tell an engine change from a damaged index.
	engine string
}

// rawOccurrence is one (document, column, offset) instance row.
type rawOccurrence struct {
	doc    int64
	column SearchColumn
	offset int64
}

// Close releases the statement. Safe to call more than once.
func (s *OccurrenceStream) Close() error {
	if s.rows == nil {
		return nil
	}
	rows := s.rows
	s.rows, s.drained, s.have = nil, true, false
	if err := rows.Close(); err != nil {
		return wrap("search_vocab", err)
	}
	return nil
}

// Next returns the next refill: the grouped occurrences of whole documents,
// at least limit rows unless the stream is exhausted, in the same
// (document, column, offset) order the former per-page statement produced. A
// nil result with a nil error means the stream is exhausted.
func (s *OccurrenceStream) Next(ctx context.Context, limit int) ([]TermOccurrence, error) {
	limit = pageLimit(ctx, limit)
	var out, group []TermOccurrence
	for {
		if !s.have {
			ok, err := s.scan(ctx)
			if err != nil {
				return nil, err
			}
			if !ok {
				break
			}
		}
		if len(group) > 0 && group[0].RowID != s.head.doc {
			out = append(out, flushDocument(group)...)
			group = nil
			// The lookahead row stays buffered for the next refill, which is
			// what lets a document's groups survive a page boundary intact.
			if len(out) >= limit {
				return out, nil
			}
		}
		group = appendInstance(group, s.head)
		s.have = false
	}
	out = append(out, flushDocument(group)...)
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// scan reads one row into head, verifying the emission order the stream relies
// on: documents never go backwards, and a document's offsets ascend within
// each column. A store that broke either would silently misgroup instances.
func (s *OccurrenceStream) scan(ctx context.Context) (bool, error) {
	if s.drained || s.rows == nil {
		return false, nil
	}
	if err := ctx.Err(); err != nil {
		return false, model.Canceled(err)
	}
	if !s.rows.Next() {
		s.drained = true
		return false, wrap("search_vocab", s.rows.Err())
	}
	var col string
	var row rawOccurrence
	if err := s.rows.Scan(&row.doc, &col, &row.offset); err != nil {
		return false, wrap("search_vocab", err)
	}
	column, ok := searchColumns[col]
	if !ok {
		return false, corrupt("search_vocab names column %q, which search_fts does not declare", col)
	}
	row.column = column
	if s.started {
		switch {
		case row.doc < s.prev.doc:
			return false, s.orderViolation("emitted term %q at document %d after document %d",
				s.term, row.doc, s.prev.doc)
		case row.doc == s.prev.doc && row.column == s.prev.column && row.offset <= s.prev.offset:
			return false, s.orderViolation("emitted term %q at offset %d after offset %d in document %d column %q",
				s.term, row.offset, s.prev.offset, row.doc, col)
		}
	}
	s.started, s.prev, s.head, s.have = true, row, row, true
	return true, nil
}

// orderViolation is the typed refusal the emission-order check raises. It
// carries a Remediation of its own rather than the bare corrupt() helper,
// because this guard has TWO causes and only one of them is a damaged index:
// fts5vocab does not contract doclist order, so an engine whose emission order
// changed would trip it on a perfectly good store, and the re-index the bare
// CTX_STORAGE_CORRUPT message invites would not fix that. The engine version
// observed at open is named so an operator can tell the two apart.
func (s *OccurrenceStream) orderViolation(format string, args ...any) *model.Error {
	engine := s.engine
	if engine == "" {
		engine = "unknown"
	}
	return &model.Error{Code: model.CodeStorageCorrupt,
		Message: "search_vocab " + fmt.Sprintf(format, args...),
		Remediation: "the search index may be damaged, or the embedded SQLite engine (" + engine +
			") may order this term's postings differently than the index was built under; " +
			"re-index the repository with `codectx index --repo <path>`, and if the same error " +
			"returns on the fresh index, report it with that engine version"}
}

// appendInstance folds one instance row into the current document's groups.
// The groups are searched rather than only the last one compared, so a store
// that interleaves a document's columns still counts each column once.
func appendInstance(group []TermOccurrence, row rawOccurrence) []TermOccurrence {
	for i := range group {
		if group[i].Column != row.column {
			continue
		}
		o := &group[i]
		o.Count++
		if len(o.Offsets) < MaxTermOffsets {
			o.Offsets = append(o.Offsets, row.offset)
		} else {
			o.Truncated = true
		}
		return group
	}
	return append(group, TermOccurrence{RowID: row.doc, Column: row.column, Count: 1, Offsets: []int64{row.offset}})
}

// flushDocument orders one document's groups by column name. fts5vocab emits a
// document's columns in declared (ordinal) order; callers sum float weights
// across them, so the order is fixed here to the column-name order the former
// ORDER BY produced, keeping scores bit-identical.
func flushDocument(group []TermOccurrence) []TermOccurrence {
	sort.Slice(group, func(i, j int) bool { return group[i].Column < group[j].Column })
	return group
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
		WHERE su.doc_id IN (` + strings.Join(marks, ",") + `)` + r.visibleDocument("su")
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
