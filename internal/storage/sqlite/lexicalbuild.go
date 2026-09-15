package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"fmt"
	"log/slog"
	"runtime"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
)

// The packed lexical structure (ADR-0007 Decision 1 as amended). A unit's term
// list and document attributes are folded ONCE, in the unit's seal
// transaction, out of a temporary index over that unit's documents alone; an
// activation merges the visible units' lists into the generation structure the
// reader reads. Nothing here scans the store-wide vocabulary: that scan cost
// 0.5 µs per posting instance on the reference repository and was paid again
// at every activation, including a one-file delta.
//
// Without the structure a query pays three b-tree descents per posting
// instance -- the vocabulary row, the document row and the generation
// membership probe -- on every term of every request, a temporary b-tree for
// each term's document frequency, and one document read per candidate.

// Lexical stream names. They are the `stream` column of
// generation_lexical_parts and unit_lexical_parts and are duplicated in those
// tables' CHECK constraints; change both together.
const (
	streamTermDir  = "term.dir"
	streamTermText = "term.text"
	streamPostList = "post.list"
	streamDocDir   = "doc.dir"
	streamDocAttr  = "doc.attr"
)

// lexPartBytes is one part of every lexical stream: an INTERNAL layout
// constant, not a user limit. No count of terms, documents or instances is
// ever refused because of it -- a stream simply has more parts. It also bounds
// the merge, whose heap is one part per open stream per source in the batch,
// which is why it is well under the 1 MiB an adjacency edge part uses. It is a
// var only so a test can shrink it (export_test.go) and prove that a value
// straddling a part boundary is stitched; production never writes to it.
var lexPartBytes = 1 << 18 // 256 KiB per part

// lexicalMergeBatch is how many sources one merge pass reads at once. Heap is
// proportional to it -- one buffered part per open stream per source -- and
// never to the number of units, so a repository with more units than this
// merges in runs and then merges the runs.
const lexicalMergeBatch = 64

// termEntryBytes is the width of one term-directory entry. The directory is
// fixed-width so a term lookup is a binary search by arithmetic rather than a
// scan of the vocabulary.
const termEntryBytes = 32

// Field offsets inside one term-directory entry, little-endian throughout.
const (
	termEntryTextOff = 0  // uint64 offset into term.text
	termEntryListOff = 8  // uint64 offset into post.list
	termEntryListLen = 16 // uint32 byte length of the posting list
	termEntryDF      = 20 // uint32 document frequency
	termEntryTextLen = 24 // uint16 byte length of the term
)

// docEntryBytes is the width of one document-directory entry, and the offsets
// inside it. The directory is fixed-width and ordered by document id, so a
// candidate's attributes are found by binary search by arithmetic and a page
// of ascending candidates walks it forwards.
const (
	docEntryBytes   = 20
	docEntryID      = 0  // uint64 document id (search_fts rowid)
	docEntryAttrOff = 8  // uint64 offset into doc.attr
	docEntryAttrLen = 16 // uint32 byte length of the attribute record
)

// lexicalColumns is the column order a packed posting list encodes a
// document's groups in: the column-NAME order flushDocument imposes on the
// live path. Callers sum float weights across the groups in the order they
// receive them, so fixing the order here is what keeps the packed scores
// bit-identical to the live ones. A column's code is its index plus one; zero
// is never a valid code.
var lexicalColumns = []SearchColumn{ColumnBody, ColumnName, ColumnPath, ColumnQualifiedName, ColumnSignature}

// lexicalColumnCode answers a column's code, or zero for a column search_fts
// does not declare.
func lexicalColumnCode(c SearchColumn) byte {
	for i, known := range lexicalColumns {
		if known == c {
			return byte(i + 1)
		}
	}
	return 0
}

// unitInstanceQuery is the bare scan the per-unit fold folds: every posting
// instance of ONE unit's documents, from the temporary index that holds only
// them. There is deliberately no ORDER BY: the instance vocabulary emits its
// rows in term order, and asking SQLite for that order materializes the whole
// vocabulary in a temporary b-tree before the first row. The order the scan
// actually delivers is verified as it arrives rather than trusted.
func unitInstanceQuery(vocab string) string {
	return `SELECT v.term, v.doc, v.col FROM ` + vocab + ` v`
}

// unitDocumentQuery reads one unit's documents, and every field the search
// package reads from a document row, in document-id order (ADR-0007
// Decision 2). The sort is over one unit's documents and no more, which is
// what keeps it bounded; the directory it feeds must ascend so a reader finds
// a candidate by binary search.
const unitDocumentQuery = `SELECT ` + searchDocumentColumns + ` FROM search_units su
	LEFT JOIN node_ids ni ON ni.id = su.node_id
	WHERE su.unit_id = ? ORDER BY su.doc_id`

// unitFTSName and unitVocabName are the per-unit temporary index and its
// instance vocabulary. The name carries the unit's row id because seal runs in
// parallel over one writer connection: a shared temporary table could not be
// folded per unit (an instance vocabulary cannot be constrained by document)
// and clearing it would discard another unit's rows.
func unitFTSName(unitRow int64) string { return fmt.Sprintf("temp.unit_fts_%d", unitRow) }

func unitVocabName(unitRow int64) string { return fmt.Sprintf("temp.unit_vocab_%d", unitRow) }

// createUnitIndex declares the unit's temporary index with EXACTLY search_fts's
// column set and tokenizer, so the terms it produces are identical to the
// store-wide index's by construction. It is contentless for the same reason
// search_fts is: the fold reads the vocabulary, never the text, and a second
// copy of the source in the temporary database is a copy Section 12.2 forbids.
func createUnitIndex(ctx context.Context, tx *sql.Tx, unitRow int64) error {
	for _, ddl := range []string{
		`CREATE VIRTUAL TABLE IF NOT EXISTS ` + unitFTSName(unitRow) +
			` USING fts5(name, qualified_name, signature, path, body, content='', tokenize='unicode61', detail='full')`,
		`CREATE VIRTUAL TABLE IF NOT EXISTS ` + unitVocabName(unitRow) +
			fmt.Sprintf(` USING fts5vocab(unit_fts_%d, 'instance')`, unitRow),
	} {
		if _, err := tx.ExecContext(ctx, ddl); err != nil {
			return wrap("unit index", err)
		}
	}
	return nil
}

// dropUnitIndex removes the unit's temporary index. It runs when the unit is
// sealed, abandoned or failed: the temporary database lives as long as the
// writer connection, so a table nobody drops leaks for the life of the process.
func dropUnitIndex(ctx context.Context, tx *sql.Tx, unitRow int64) error {
	for _, ddl := range []string{
		`DROP TABLE IF EXISTS ` + unitVocabName(unitRow),
		`DROP TABLE IF EXISTS ` + unitFTSName(unitRow),
	} {
		if _, err := tx.ExecContext(ctx, ddl); err != nil {
			return wrap("unit index", err)
		}
	}
	return nil
}

// lexSource is one input of a merge: the packed streams of a sealed unit or of
// an earlier merge run, with the counts that say how long its directories are.
type lexSource struct {
	key       partKey
	termCount int64
	docCount  int64
}

// buildUnitLexical folds the unit's packed lexical list inside its seal
// transaction. Two inputs can carry a document's term instances and they are
// disjoint by construction: the unit's temporary index holds the documents
// this unit indexed itself, and a delta's carried documents keep the
// predecessor's document ids, so the predecessor's own packed list is the only
// place their instances can be read from -- the index is contentless and no
// row of this database can reproduce the text they were indexed with.
func buildUnitLexical(ctx context.Context, tx *sql.Tx, unitRow, carriedFrom int64) error {
	docs, tokens, err := writeUnitDocuments(ctx, tx, unitRow)
	if err != nil {
		return err
	}
	// A unit that published no lexical document never created its index, and a
	// unit that did created it in its first document batch; either way the
	// fold below needs it to exist, and creating it is idempotent.
	if err := createUnitIndex(ctx, tx, unitRow); err != nil {
		return err
	}
	if carriedFrom == 0 {
		// Nothing is carried, so the fold IS the unit's list and is written
		// straight into it; there is nothing to merge it with.
		terms, err := foldUnitVocabulary(ctx, tx, unitRow, unitLexicalKey(unitRow))
		if err != nil {
			return err
		}
		return commitUnitLexical(ctx, tx, unitRow, docs, tokens, terms)
	}
	if err := ensureRunTable(ctx, tx); err != nil {
		return err
	}
	fresh := lexSource{key: runLexicalKey(unitRow)}
	if fresh.termCount, err = foldUnitVocabulary(ctx, tx, unitRow, fresh.key); err != nil {
		return err
	}
	prev, err := unitSource(ctx, tx, carriedFrom)
	if err != nil {
		return err
	}
	// Only the documents this unit actually carries: the predecessor's list
	// still names the documents the delta replaced.
	keep, err := unitDocuments(ctx, tx, unitRow)
	if err != nil {
		return err
	}
	terms, err := mergeTerms(ctx, tx, []lexSource{fresh, prev}, unitLexicalKey(unitRow), keep)
	if err != nil {
		return err
	}
	if err := deleteRun(ctx, tx, fresh.key); err != nil {
		return err
	}
	return commitUnitLexical(ctx, tx, unitRow, docs, tokens, terms)
}

// commitUnitLexical writes the unit's commit row. It is written LAST, after
// every part, so its presence is what tells a merge that the parts behind it
// are complete.
func commitUnitLexical(ctx context.Context, tx *sql.Tx, unitRow, docs, tokens, terms int64) error {
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO unit_lexical(unit_id, doc_count, token_total, term_count) VALUES(?, ?, ?, ?)`,
		unitRow, docs, tokens, terms); err != nil {
		return wrap("unit_lexical", err)
	}
	return dropUnitIndex(ctx, tx, unitRow)
}

// writeUnitDocuments writes the unit's document directory and attribute
// stream, and returns the document count and token total the generation's
// statistics sum. Both carried and freshly indexed documents are read from
// search_units, which is the one place every field of a document lives.
func writeUnitDocuments(ctx context.Context, tx *sql.Tx, unitRow int64) (docs, tokens int64, err error) {
	w := newDocWriter(ctx, tx, unitLexicalKey(unitRow))
	rows, err := tx.QueryContext(ctx, unitDocumentQuery, unitRow)
	if err != nil {
		return 0, 0, wrap("search_units", err)
	}
	defer rows.Close()
	var last int64
	for rows.Next() {
		d, err := scanSearchDocument(rows)
		if err != nil {
			return 0, 0, err
		}
		if docs > 0 && d.RowID <= last {
			return 0, 0, corrupt("unit %d emitted document %d after document %d", unitRow, d.RowID, last)
		}
		if err := w.add(d); err != nil {
			return 0, 0, err
		}
		last, docs, tokens = d.RowID, docs+1, tokens+d.TokenCount
	}
	if err := rows.Err(); err != nil {
		return 0, 0, wrap("search_units", err)
	}
	return docs, tokens, w.close()
}

// unitDocuments resolves the unit's documents into a bitmap. A document id is
// the contentless index's own key, dense in practice, so a bit per id costs a
// byte per eight documents.
func unitDocuments(ctx context.Context, tx *sql.Tx, unitRow int64) (*docBitmap, error) {
	var maxDoc int64
	if err := tx.QueryRowContext(ctx, `SELECT coalesce(max(doc_id), 0) FROM search_units WHERE unit_id = ?`, unitRow).
		Scan(&maxDoc); err != nil {
		return nil, wrap("search_units", err)
	}
	b := &docBitmap{bits: make([]byte, maxDoc/8+1), max: maxDoc}
	rows, err := tx.QueryContext(ctx, `SELECT doc_id FROM search_units WHERE unit_id = ?`, unitRow)
	if err != nil {
		return nil, wrap("search_units", err)
	}
	defer rows.Close()
	for rows.Next() {
		var doc int64
		if err := rows.Scan(&doc); err != nil {
			return nil, wrap("search_units", err)
		}
		if doc >= 0 && doc <= maxDoc {
			b.set(doc)
		}
	}
	return b, wrap("search_units", rows.Err())
}

// docBitmap is a document set resolved once.
type docBitmap struct {
	bits []byte
	max  int64
}

func (b *docBitmap) has(doc int64) bool {
	if b == nil {
		return true
	}
	if doc < 0 || doc > b.max {
		return false
	}
	return b.bits[doc>>3]&(1<<uint(doc&7)) != 0
}

func (b *docBitmap) set(doc int64) {
	b.bits[doc>>3] |= 1 << uint(doc&7)
}

// foldUnitVocabulary folds the unit's temporary instance vocabulary into a
// packed term list and answers how many terms it wrote. The scan is over that
// unit's instances alone, so its cost is a unit's, paid once per unit version
// in the parallel seal phase, and never a repository's.
func foldUnitVocabulary(ctx context.Context, tx *sql.Tx, unitRow int64, out partKey) (int64, error) {
	w := newTermWriter(ctx, tx, out)
	rows, err := tx.QueryContext(ctx, unitInstanceQuery(unitVocabName(unitRow)))
	if err != nil {
		return 0, wrap("unit index", err)
	}
	defer rows.Close()
	// RawBytes aliases the driver's own buffer, so the scan allocates nothing
	// per row; a term is copied only when it changes.
	var term, col sql.RawBytes
	var doc int64
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return 0, model.Canceled(err)
		}
		if err := rows.Scan(&term, &doc, &col); err != nil {
			return 0, wrap("unit index", err)
		}
		column, ok := searchColumns[string(col)]
		if !ok {
			return 0, corrupt("the unit index names column %q, which search_fts does not declare", string(col))
		}
		if err := w.add(term, doc, lexicalColumnCode(column), 1); err != nil {
			return 0, err
		}
	}
	if err := rows.Err(); err != nil {
		return 0, wrap("unit index", err)
	}
	if err := w.close(); err != nil {
		return 0, err
	}
	return w.terms, nil
}

// buildLexical merges the generation's visible units into the packed structure
// every lexical query reads. It runs inside Activate's transaction, after the
// packed adjacency and before the active pointer flips, so a generation is
// published only with the structure and a failed merge fails the activation.
func buildLexical(ctx context.Context, tx *sql.Tx, gen int64) error {
	// ADR-0007 holds this pass to 5 % of the index wall clock, and a delta
	// activation to three times the packed adjacency's build. That bound is
	// only checkable if the operator can see the pass on its own, so its start
	// and end are logged rather than hidden inside the activation's total, and
	// its heap share is reported beside its wall clock because a cost stated
	// only in milliseconds cannot answer whether the merge is what pushed a
	// run into its memory ceiling.
	started := time.Now()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	slog.Default().Info("packed lexical build started", "generation", gen)
	var terms, docs, passes int64
	defer func() {
		runtime.ReadMemStats(&after)
		slog.Default().Info("packed lexical build finished",
			"generation", gen, "duration_ms", time.Since(started).Milliseconds(),
			"terms", terms, "documents", docs, "merge_passes", passes,
			// Signed: a build that ends after a collection leaves less live
			// heap than it found, and an unsigned subtraction would report that
			// as eighteen exabytes.
			"heap_delta_bytes", int64(after.HeapInuse)-int64(before.HeapInuse))
	}()

	sources, tokenTotal, docCount, err := visibleUnits(ctx, tx, gen)
	if err != nil {
		return err
	}
	if err := ensureRunTable(ctx, tx); err != nil {
		return err
	}
	var run int64
	// More sources than one pass can hold: merge them into runs of the same
	// format and merge the runs, so heap stays proportional to the batch.
	for len(sources) > lexicalMergeBatch {
		passes++
		next := make([]lexSource, 0, (len(sources)+lexicalMergeBatch-1)/lexicalMergeBatch)
		for start := 0; start < len(sources); start += lexicalMergeBatch {
			batch := sources[start:min(start+lexicalMergeBatch, len(sources))]
			run++
			out := runLexicalKey(run)
			merged, err := mergeInto(ctx, tx, batch, out)
			if err != nil {
				return err
			}
			next = append(next, merged)
		}
		for _, s := range sources {
			if s.key.table == runLexicalPartsTable {
				if err := deleteRun(ctx, tx, s.key); err != nil {
					return err
				}
			}
		}
		sources = next
	}
	passes++
	merged, err := mergeInto(ctx, tx, sources, lexicalKey(gen))
	if err != nil {
		return err
	}
	for _, s := range sources {
		if s.key.table == runLexicalPartsTable {
			if err := deleteRun(ctx, tx, s.key); err != nil {
				return err
			}
		}
	}
	terms, docs = merged.termCount, merged.docCount
	if merged.docCount != docCount {
		return corrupt("generation %d merged %d documents from unit lists but carries %d", gen, merged.docCount, docCount)
	}
	// The commit row is written last: its presence is what tells a reader every
	// part behind it is durable.
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO generation_lexical(generation_id, doc_count, token_total, term_count) VALUES(?, ?, ?, ?)`,
		gen, docCount, tokenTotal, merged.termCount); err != nil {
		return wrap("generation_lexical", err)
	}
	return nil
}

// visibleUnits lists the generation's members as merge sources, ordered by
// unit id so the merge is deterministic, and sums the document statistics the
// scorer needs. A member without a packed list was sealed by a binary that did
// not write one, which is a corrupt store rather than an empty unit.
func visibleUnits(ctx context.Context, tx *sql.Tx, gen int64) ([]lexSource, int64, int64, error) {
	rows, err := tx.QueryContext(ctx, `SELECT gu.unit_id, ul.doc_count, ul.token_total, ul.term_count
		FROM generation_units gu LEFT JOIN unit_lexical ul ON ul.unit_id = gu.unit_id
		WHERE gu.generation_id = ? ORDER BY gu.unit_id`, gen)
	if err != nil {
		return nil, 0, 0, wrap("unit_lexical", err)
	}
	defer rows.Close()
	var out []lexSource
	var tokens, docs int64
	for rows.Next() {
		var unit int64
		var docCount, tokenTotal, termCount sql.NullInt64
		if err := rows.Scan(&unit, &docCount, &tokenTotal, &termCount); err != nil {
			return nil, 0, 0, wrap("unit_lexical", err)
		}
		if !docCount.Valid {
			return nil, 0, 0, corrupt("unit %d has no packed lexical list; it was sealed without one", unit)
		}
		out = append(out, lexSource{key: unitLexicalKey(unit), termCount: termCount.Int64, docCount: docCount.Int64})
		docs += docCount.Int64
		tokens += tokenTotal.Int64
	}
	return out, tokens, docs, wrap("unit_lexical", rows.Err())
}

// ensureRunTable declares the temporary table intermediate merge runs live in.
// It is temporary on purpose: a run exists only between two passes of one
// merge, and an interrupted activation must not leave one in the database.
func ensureRunTable(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS temp.lexical_run_parts (
		run_id INTEGER NOT NULL, stream TEXT NOT NULL, part INTEGER NOT NULL, bytes BLOB NOT NULL,
		PRIMARY KEY(run_id, stream, part)) WITHOUT ROWID`)
	return wrap("lexical_run_parts", err)
}

func deleteRun(ctx context.Context, tx *sql.Tx, key partKey) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM temp.lexical_run_parts WHERE run_id = ?`, key.id)
	return wrap("lexical_run_parts", err)
}

// unitSource reads a sealed unit's list as a merge source.
func unitSource(ctx context.Context, tx *sql.Tx, unitRow int64) (lexSource, error) {
	s := lexSource{key: unitLexicalKey(unitRow)}
	var tokens int64
	err := tx.QueryRowContext(ctx, `SELECT doc_count, token_total, term_count FROM unit_lexical WHERE unit_id = ?`, unitRow).
		Scan(&s.docCount, &tokens, &s.termCount)
	if isNoRows(err) {
		return s, corrupt("unit %d has no packed lexical list; it was sealed without one", unitRow)
	}
	return s, wrap("unit_lexical", err)
}

// mergeInto merges the sources' document streams and term lists into out and
// returns the merged source, so a run is an input of the next pass exactly as
// a unit is.
func mergeInto(ctx context.Context, tx *sql.Tx, sources []lexSource, out partKey) (lexSource, error) {
	merged := lexSource{key: out}
	docs, err := mergeDocuments(ctx, tx, sources, out)
	if err != nil {
		return merged, err
	}
	terms, err := mergeTerms(ctx, tx, sources, out, nil)
	if err != nil {
		return merged, err
	}
	merged.docCount, merged.termCount = docs, terms
	return merged, nil
}

// mergeDocuments merges the sources' document directories into one ordered by
// document id, copying each document's attribute record across unchanged. The
// merge is by document id rather than by concatenating whole units because
// nothing orders one unit's document ids entirely before another's: the ids
// are index rowids handed out as the parallel seal phase inserts, so two units
// sealed at the same time interleave.
func mergeDocuments(ctx context.Context, tx *sql.Tx, sources []lexSource, out partKey) (docs int64, err error) {
	w := newDocWriter(ctx, tx, out)
	cursors := make([]*docCursor, 0, len(sources))
	for _, s := range sources {
		c := &docCursor{
			dir:  newPartReader(ctx, tx, s.key, streamDocDir),
			attr: newPartReader(ctx, tx, s.key, streamDocAttr),
			left: s.docCount,
		}
		ok, err := c.next()
		if err != nil {
			return 0, err
		}
		if ok {
			cursors = append(cursors, c)
		}
	}
	var last int64
	for len(cursors) > 0 {
		if err := ctx.Err(); err != nil {
			return 0, model.Canceled(err)
		}
		pick := 0
		for i, c := range cursors {
			if c.doc < cursors[pick].doc {
				pick = i
			}
		}
		c := cursors[pick]
		if docs > 0 && c.doc <= last {
			return 0, corrupt("packed lexical merge produced document %d after document %d", c.doc, last)
		}
		if err := w.addRaw(c.doc, c.attrBytes); err != nil {
			return 0, err
		}
		last, docs = c.doc, docs+1
		ok, err := c.next()
		if err != nil {
			return 0, err
		}
		if !ok {
			cursors = append(cursors[:pick], cursors[pick+1:]...)
		}
	}
	return docs, w.close()
}

// mergeTerms merges the sources' term lists into out. keep, when not nil,
// restricts the result to one unit's documents: a delta's new unit merges its
// predecessor's list, which still names the documents the delta replaced.
//
// A term's document frequency is the number of documents the merge actually
// emits for it, and a document belongs to exactly one source, so the merged
// frequency is the sum of the per-source frequencies over the kept documents.
func mergeTerms(ctx context.Context, tx *sql.Tx, sources []lexSource, out partKey, keep *docBitmap) (int64, error) {
	w := newTermWriter(ctx, tx, out)
	cursors := make([]*termCursor, 0, len(sources))
	for _, s := range sources {
		c := &termCursor{
			dir:  newPartReader(ctx, tx, s.key, streamTermDir),
			text: newPartReader(ctx, tx, s.key, streamTermText),
			list: newPartReader(ctx, tx, s.key, streamPostList),
			left: s.termCount,
		}
		ok, err := c.next()
		if err != nil {
			return 0, err
		}
		if ok {
			cursors = append(cursors, c)
		}
	}
	postings := make([]*postingCursor, 0, len(sources))
	// The smallest term is COPIED out of the cursor that carries it: advancing
	// that cursor rewrites its term buffer in place, and a term that aliased it
	// would change under the comparisons that decide which cursors to advance.
	var term []byte
	for len(cursors) > 0 {
		if err := ctx.Err(); err != nil {
			return 0, model.Canceled(err)
		}
		smallest := cursors[0].term
		for _, c := range cursors[1:] {
			if bytes.Compare(c.term, smallest) < 0 {
				smallest = c.term
			}
		}
		term = append(term[:0], smallest...)
		postings = postings[:0]
		for _, c := range cursors {
			if bytes.Equal(c.term, term) {
				postings = append(postings, &postingCursor{raw: c.list.buffered(), term: term})
			}
		}
		if err := mergePostings(w, term, postings, keep); err != nil {
			return 0, err
		}
		live := cursors[:0]
		for _, c := range cursors {
			if bytes.Equal(c.term, term) {
				ok, err := c.next()
				if err != nil {
					return 0, err
				}
				if !ok {
					continue
				}
			}
			live = append(live, c)
		}
		cursors = live
	}
	if err := w.close(); err != nil {
		return 0, err
	}
	return w.terms, nil
}

// mergePostings folds one term's lists from every source that carries it into
// the writer, documents ascending.
func mergePostings(w *termWriter, term []byte, cursors []*postingCursor, keep *docBitmap) error {
	live := make([]*postingCursor, 0, len(cursors))
	for _, c := range cursors {
		ok, err := c.next()
		if err != nil {
			return err
		}
		if ok {
			live = append(live, c)
		}
	}
	var last int64
	var any bool
	for len(live) > 0 {
		pick := 0
		for i, c := range live {
			if c.doc < live[pick].doc {
				pick = i
			}
		}
		c := live[pick]
		if any && c.doc <= last {
			return corrupt("packed lexical merge produced term %q at document %d after document %d", string(term), c.doc, last)
		}
		if keep.has(c.doc) {
			for _, g := range c.groups {
				if err := w.add(term, c.doc, g.code, g.count); err != nil {
					return err
				}
			}
			last, any = c.doc, true
		}
		ok, err := c.next()
		if err != nil {
			return err
		}
		if !ok {
			live = append(live[:pick], live[pick+1:]...)
		}
	}
	return nil
}

// partReader is a sequential cursor over one chunked stream inside a write
// transaction: it holds exactly ONE part, which is what bounds a merge pass to
// its batch. The request-side reader (lexicalread.go) keeps a small
// least-recently-used window instead, because a query jumps around the term
// directory while a merge only ever walks forwards.
type partReader struct {
	ctx    context.Context
	tx     *sql.Tx
	key    partKey
	stream string
	size   int64
	index  int
	part   []byte
	loaded bool
	off    int64 // the stream offset the cursor has consumed to
	out    []byte
}

func newPartReader(ctx context.Context, tx *sql.Tx, key partKey, stream string) *partReader {
	return &partReader{ctx: ctx, tx: tx, key: key, stream: stream, size: int64(partSizeFor(stream))}
}

// read consumes the next n bytes of the stream, stitching across the part
// boundary a value may straddle. The returned slice is valid until the next
// read.
func (r *partReader) read(n int) ([]byte, error) {
	r.out = r.out[:0]
	for len(r.out) < n {
		index := int(r.off / r.size)
		if !r.loaded || index != r.index {
			if err := r.load(index); err != nil {
				return nil, err
			}
		}
		begin := int(r.off % r.size)
		if begin >= len(r.part) {
			return nil, corrupt("packed lexical stream %q of %s %d ends before offset %d", r.stream, r.key.column, r.key.id, r.off)
		}
		take := min(len(r.part)-begin, n-len(r.out))
		r.out = append(r.out, r.part[begin:begin+take]...)
		r.off += int64(take)
	}
	return r.out, nil
}

// buffered is the slice the last read returned, so a caller that read a term's
// whole posting list can hand it on without copying it again.
func (r *partReader) buffered() []byte { return r.out }

func (r *partReader) load(index int) error {
	var raw []byte
	err := r.tx.QueryRowContext(r.ctx, `SELECT bytes FROM `+r.key.table+` WHERE `+r.key.column+
		` = ? AND stream = ? AND part = ?`, r.key.id, r.stream, index).Scan(&raw)
	if isNoRows(err) {
		return corrupt("packed lexical stream %q of %s %d is missing part %d", r.stream, r.key.column, r.key.id, index)
	}
	if err != nil {
		return wrap(r.key.table, err)
	}
	r.part, r.index, r.loaded = raw, index, true
	return nil
}

// seek positions the cursor at an absolute stream offset. The merge reads each
// stream forwards, so this only ever moves it to where the directory says the
// next value starts.
func (r *partReader) seek(off int64) { r.off = off }

// termCursor walks one source's term directory in order, holding one term and
// its posting list at a time.
type termCursor struct {
	dir, text, list *partReader
	left            int64
	term            []byte
	df              int64
}

func (c *termCursor) next() (bool, error) {
	if c.left == 0 {
		return false, nil
	}
	c.left--
	raw, err := c.dir.read(termEntryBytes)
	if err != nil {
		return false, err
	}
	textOff := int64(binary.LittleEndian.Uint64(raw[termEntryTextOff:]))
	listOff := int64(binary.LittleEndian.Uint64(raw[termEntryListOff:]))
	listLen := int(binary.LittleEndian.Uint32(raw[termEntryListLen:]))
	c.df = int64(binary.LittleEndian.Uint32(raw[termEntryDF:]))
	textLen := int(binary.LittleEndian.Uint16(raw[termEntryTextLen:]))
	c.text.seek(textOff)
	text, err := c.text.read(textLen)
	if err != nil {
		return false, err
	}
	c.term = append(c.term[:0], text...)
	c.list.seek(listOff)
	if _, err := c.list.read(listLen); err != nil {
		return false, err
	}
	return true, nil
}

// docCursor walks one source's document directory in order.
type docCursor struct {
	dir, attr *partReader
	left      int64
	doc       int64
	attrBytes []byte
}

func (c *docCursor) next() (bool, error) {
	if c.left == 0 {
		return false, nil
	}
	c.left--
	raw, err := c.dir.read(docEntryBytes)
	if err != nil {
		return false, err
	}
	c.doc = int64(binary.LittleEndian.Uint64(raw[docEntryID:]))
	off := int64(binary.LittleEndian.Uint64(raw[docEntryAttrOff:]))
	n := int(binary.LittleEndian.Uint32(raw[docEntryAttrLen:]))
	c.attr.seek(off)
	attr, err := c.attr.read(n)
	if err != nil {
		return false, err
	}
	c.attrBytes = attr
	return true, nil
}

// postingGroup is one (column, count) pair of one document.
type postingGroup struct {
	code  byte
	count int64
}

// postingCursor decodes one term's packed posting list document by document.
type postingCursor struct {
	term   []byte
	raw    []byte
	doc    int64
	groups []postingGroup
}

func (c *postingCursor) next() (bool, error) {
	if len(c.raw) == 0 {
		return false, nil
	}
	delta, n := binary.Uvarint(c.raw)
	if n <= 0 {
		return false, c.malformed()
	}
	c.raw = c.raw[n:]
	c.doc += int64(delta)
	groups, n := binary.Uvarint(c.raw)
	if n <= 0 || groups == 0 {
		return false, c.malformed()
	}
	c.raw = c.raw[n:]
	c.groups = c.groups[:0]
	for i := uint64(0); i < groups; i++ {
		if len(c.raw) == 0 {
			return false, c.malformed()
		}
		code := c.raw[0]
		c.raw = c.raw[1:]
		if code == 0 || int(code) > len(lexicalColumns) {
			return false, c.malformed()
		}
		count, n := binary.Uvarint(c.raw)
		if n <= 0 {
			return false, c.malformed()
		}
		c.raw = c.raw[n:]
		c.groups = append(c.groups, postingGroup{code: code, count: int64(count)})
	}
	return true, nil
}

func (c *postingCursor) malformed() error {
	return corrupt("packed lexical posting list of term %q is malformed", string(c.term))
}

// docWriter streams the document directory and attribute stream. It holds one
// document's record at a time.
type docWriter struct {
	dir, attr *partWriter
	off       int64
	entry     [docEntryBytes]byte
	buf       []byte
}

func newDocWriter(ctx context.Context, tx *sql.Tx, key partKey) *docWriter {
	return &docWriter{dir: newPartWriter(ctx, tx, key, streamDocDir), attr: newPartWriter(ctx, tx, key, streamDocAttr)}
}

func (w *docWriter) add(d SearchDocument) error {
	w.buf = encodeSearchDocument(w.buf[:0], d)
	return w.addRaw(d.RowID, w.buf)
}

func (w *docWriter) addRaw(doc int64, attr []byte) error {
	binary.LittleEndian.PutUint64(w.entry[docEntryID:], uint64(doc))
	binary.LittleEndian.PutUint64(w.entry[docEntryAttrOff:], uint64(w.off))
	binary.LittleEndian.PutUint32(w.entry[docEntryAttrLen:], uint32(len(attr)))
	if err := w.attr.write(attr); err != nil {
		return err
	}
	if err := w.dir.write(w.entry[:]); err != nil {
		return err
	}
	w.off += int64(len(attr))
	return nil
}

func (w *docWriter) close() error {
	for _, p := range []*partWriter{w.dir, w.attr} {
		if err := p.close(); err != nil {
			return err
		}
	}
	return nil
}

// termWriter folds an ordered stream of (term, document, column, count) into
// the three term streams. It holds one term's posting list and one document's
// groups at a time: a term is committed to the directory only when the stream
// leaves it, which is what makes the fold a single streaming pass.
type termWriter struct {
	dir, text, list *partWriter

	term    []byte
	haveTrm bool
	df      int64
	listBuf []byte
	textOff int64
	listOff int64

	doc     int64
	last    int64
	haveDoc bool
	counts  [16]int64 // one slot per column code; codes start at one
	terms   int64

	scratch [binary.MaxVarintLen64]byte
}

func newTermWriter(ctx context.Context, tx *sql.Tx, key partKey) *termWriter {
	return &termWriter{
		dir:  newPartWriter(ctx, tx, key, streamTermDir),
		text: newPartWriter(ctx, tx, key, streamTermText),
		list: newPartWriter(ctx, tx, key, streamPostList),
	}
}

// add folds n instances of one term in one column of one document. Terms and,
// within a term, documents must not go backwards: the streamed directory is
// ordered by construction and a reader binary-searches it, so an out-of-order
// input would silently produce a directory no lookup can trust.
func (w *termWriter) add(term []byte, doc int64, code byte, n int64) error {
	if code == 0 {
		return internal("packed lexical build received an unknown column code")
	}
	switch {
	case !w.haveTrm || !bytes.Equal(w.term, term):
		if w.haveTrm && bytes.Compare(term, w.term) < 0 {
			return corrupt("packed lexical input emitted term %q after term %q", string(term), string(w.term))
		}
		if err := w.flushTerm(); err != nil {
			return err
		}
		w.term = append(w.term[:0], term...)
		w.haveTrm = true
	case doc < w.doc:
		return corrupt("packed lexical input emitted term %q at document %d after document %d", string(term), doc, w.doc)
	}
	if !w.haveDoc || doc != w.doc {
		if err := w.flushDoc(); err != nil {
			return err
		}
		w.doc, w.haveDoc = doc, true
	}
	w.counts[code] += n
	return nil
}

// flushDoc encodes the current document into the term's list: the document id
// as a delta from the previous one, then the group count, then one
// (column code, count) pair per column in column-name order.
func (w *termWriter) flushDoc() error {
	if !w.haveDoc {
		return nil
	}
	groups := 0
	for _, n := range w.counts {
		if n > 0 {
			groups++
		}
	}
	if groups == 0 {
		w.haveDoc = false
		return nil
	}
	w.listBuf = w.appendUvarint(w.listBuf, uint64(w.doc-w.last))
	w.listBuf = w.appendUvarint(w.listBuf, uint64(groups))
	for code, n := range w.counts {
		if n == 0 {
			continue
		}
		w.listBuf = append(w.listBuf, byte(code))
		w.listBuf = w.appendUvarint(w.listBuf, uint64(n))
		w.counts[code] = 0
	}
	w.df++
	w.last, w.haveDoc = w.doc, false
	return nil
}

func (w *termWriter) appendUvarint(dst []byte, v uint64) []byte {
	n := binary.PutUvarint(w.scratch[:], v)
	return append(dst, w.scratch[:n]...)
}

// flushTerm commits the term the stream just left: its bytes to term.text, its
// posting list to post.list and the entry that points at both to term.dir.
func (w *termWriter) flushTerm() error {
	if !w.haveTrm {
		return nil
	}
	if err := w.flushDoc(); err != nil {
		return err
	}
	if w.df == 0 {
		// Every instance of this term is in a document this list does not
		// carry. It is not a term of the list and takes no entry.
		w.resetTerm()
		return nil
	}
	if len(w.term) > int(^uint16(0)) {
		return corrupt("the lexical index carries a term of %d bytes", len(w.term))
	}
	var entry [termEntryBytes]byte
	binary.LittleEndian.PutUint64(entry[termEntryTextOff:], uint64(w.textOff))
	binary.LittleEndian.PutUint64(entry[termEntryListOff:], uint64(w.listOff))
	binary.LittleEndian.PutUint32(entry[termEntryListLen:], uint32(len(w.listBuf)))
	binary.LittleEndian.PutUint32(entry[termEntryDF:], uint32(w.df))
	binary.LittleEndian.PutUint16(entry[termEntryTextLen:], uint16(len(w.term)))
	if err := w.text.write(w.term); err != nil {
		return err
	}
	if err := w.list.write(w.listBuf); err != nil {
		return err
	}
	if err := w.dir.write(entry[:]); err != nil {
		return err
	}
	w.textOff += int64(len(w.term))
	w.listOff += int64(len(w.listBuf))
	w.terms++
	w.resetTerm()
	return nil
}

func (w *termWriter) resetTerm() {
	w.listBuf, w.df, w.last, w.haveTrm, w.haveDoc = w.listBuf[:0], 0, 0, false, false
	w.counts = [16]int64{}
}

func (w *termWriter) close() error {
	if err := w.flushTerm(); err != nil {
		return err
	}
	for _, p := range []*partWriter{w.dir, w.text, w.list} {
		if err := p.close(); err != nil {
			return err
		}
	}
	return nil
}
