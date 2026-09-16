package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"log/slog"
	"maps"
	"runtime"
	"slices"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
)

// The packed lexical structure (ADR-0007 Decision 1). A SEGMENT is an
// immutable packed structure over one set of documents: a term directory in
// term order, the term text it points into, and the posting lists, whose
// documents ascend by rowid. A segment is folded once, by the seal of the unit
// whose documents it holds, from the token instances that unit's own tokenizer
// pass already produced. An activation names segments; it never rebuilds them,
// so publishing a one-file delta costs the delta's own segment and not a
// rewrite of the whole structure.
//
// Without the packed form a query pays three b-tree descents per posting
// instance -- the vocabulary row, the document row and the generation-membership
// probe -- on every term of every request, and a temporary b-tree for each
// term's document frequency.

// Lexical stream names. They are the `stream` column of
// lexical_segment_parts and are duplicated in that table's CHECK
// constraint; change both together.
const (
	streamTermDir  = "term.dir"
	streamTermText = "term.text"
	streamPostList = "post.list"
)

// lexPartBytes is one part of every lexical stream: an INTERNAL layout
// constant, not a user limit. No count of terms, documents or instances is
// ever refused because of it -- a stream simply has more parts. It is a var
// only so a test can shrink it (export_test.go) and prove that a value
// straddling a part boundary is stitched; production never writes to it.
var lexPartBytes = 1 << 20 // 1 MiB per part

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

// foldUnitSegment folds the sealing unit's staged token instances into one
// immutable segment and records the unit as its owner. It runs inside the
// seal's transaction, so a unit becomes sealed and gains its segment together
// or neither. A unit that published no document gets no segment and answers
// zero.
func foldUnitSegment(ctx context.Context, tx *sql.Tx, unitRow int64, stage *lexicalStage) (int64, error) {
	docs, err := stage.docCount(ctx)
	if err != nil {
		return 0, err
	}
	if docs == 0 {
		return 0, nil
	}
	if err := stage.commit(ctx); err != nil {
		return 0, err
	}
	// The segment row is inserted FIRST so the engine assigns its id and the
	// parts can be keyed by it as they stream. Seals run in parallel over one
	// writer, so deriving the id from max(id) + 1 would hand two units the
	// same segment.
	res, err := tx.ExecContext(ctx, `INSERT INTO lexical_segments(term_count, doc_count, bytes) VALUES(0, ?, 0)`, docs)
	if err != nil {
		return 0, wrap("lexical_segments", err)
	}
	segment, err := res.LastInsertId()
	if err != nil {
		return 0, wrap("lexical_segments", err)
	}
	w := &lexicalWriter{
		dir:  newPartWriter(ctx, tx, segmentKey(segment), streamTermDir),
		text: newPartWriter(ctx, tx, segmentKey(segment), streamTermText),
		list: newPartWriter(ctx, tx, segmentKey(segment), streamPostList),
	}
	rows, err := stage.db.QueryContext(ctx, stageOrderedRead)
	if err != nil {
		return 0, wrap("lexical staging", err)
	}
	defer rows.Close()
	// RawBytes aliases the driver's own buffer, so the fold allocates nothing
	// per row; a term is copied only when it changes.
	var term, col sql.RawBytes
	var doc, n int64
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return 0, model.Canceled(err)
		}
		if err := rows.Scan(&term, &doc, &col, &n); err != nil {
			return 0, wrap("lexical staging", err)
		}
		column, ok := searchColumns[string(col)]
		if !ok {
			return 0, corrupt("lexical staging names column %q, which search_fts does not declare", string(col))
		}
		if err := w.add(term, doc, lexicalColumnCode(column), n); err != nil {
			return 0, err
		}
	}
	if err := rows.Err(); err != nil {
		return 0, wrap("lexical staging", err)
	}
	if err := w.close(); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE lexical_segments SET term_count = ?, bytes = ? WHERE id = ?`,
		w.terms, w.dir.total+w.text.total+w.list.total, segment); err != nil {
		return 0, wrap("lexical_segments", err)
	}
	// The documents the fold just packed name their segment. They are exactly
	// the unit's rows that no earlier fold claimed: a carried document arrived
	// with its predecessor's segment already on it, and a batch whose ingestion
	// failed left no row here at all.
	if _, err := tx.ExecContext(ctx,
		`UPDATE search_units SET segment_id = ?1 WHERE unit_id = ?2 AND segment_id IS NULL`,
		segment, unitRow); err != nil {
		return 0, wrap("search_units", err)
	}
	return segment, nil
}

// buildLexical records the generation's segment set. It runs inside Activate's
// transaction, after the packed adjacency and before the active pointer flips,
// so a generation is published only with the structure every lexical query
// reads.
//
// The set is DERIVED, never copied: it is the distinct segments the live rows
// of the generation's member units point at, in segment-id order.
// search_units.segment_id is the source of truth -- a seal writes it over the
// documents it folded, a carry-over copies it forward with doc_id, and a
// compaction re-points it -- so a generation inherits its predecessor's
// segments by that rule rather than by copying its set, and no segment can be
// named beside the one that absorbed its documents.
//
// An activation therefore writes one row per segment, the visible-document
// bitmap and whatever compaction was due, never a rewrite of the whole packed
// structure: publishing a one-file delta costs the delta's own segment plus,
// occasionally, one tier merge.
func buildLexical(ctx context.Context, tx *sql.Tx, gen int64) error {
	// ADR-0007 holds this pass to 5 % of the index wall clock. That bound is
	// only checkable if the operator can see the pass on its own, so its start
	// and end are logged rather than hidden inside the activation's total, and
	// its heap share is reported beside its wall clock because a cost stated
	// only in milliseconds cannot answer whether the build is what pushed a run
	// into its memory ceiling.
	started := time.Now()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	slog.Default().Info("packed lexical activation started", "generation", gen)
	var segments, merges int64
	defer func() {
		runtime.ReadMemStats(&after)
		slog.Default().Info("packed lexical activation finished",
			"generation", gen, "duration_ms", time.Since(started).Milliseconds(),
			"segments", segments, "merges", merges,
			// Signed: a pass that ends after a collection leaves less live
			// heap than it found, and an unsigned subtraction would report that
			// as eighteen exabytes.
			"heap_delta_bytes", int64(after.HeapInuse)-int64(before.HeapInuse))
	}()

	visible, docCount, tokenTotal, live, err := visibleDocuments(ctx, tx, gen)
	if err != nil {
		return err
	}
	// Segment-id order. The set comes out of a map because the one pass that
	// resolves it is over DOCUMENTS, and asking the engine for a sorted
	// distinct segment instead would materialize the set in a temporary b-tree
	// at the moment that publishes facts to every reader.
	ids := slices.Sorted(maps.Keys(live))
	stats, err := segmentStats(ctx, tx, ids)
	if err != nil {
		return err
	}
	ids, merges, err = compactSegments(ctx, tx, ids, live, stats)
	if err != nil {
		return err
	}

	var covered int64
	for ord, id := range ids {
		hidden := stats[id].docs - live[id]
		if hidden < 0 {
			return corrupt("generation %d holds %d documents of segment %d, which packed %d",
				gen, live[id], id, stats[id].docs)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO generation_segments(generation_id, segment_id, ord, hidden) VALUES(?, ?, ?, ?)`,
			gen, id, ord, hidden); err != nil {
			return wrap("generation_segments", err)
		}
		covered += live[id]
	}
	segments = int64(len(ids))
	// Every visible document of a generation lies in exactly ONE of its
	// segments. The set is assembled from the segments its own documents point
	// at, so a document whose row points at a segment the set dropped would
	// simply never be read -- a term would silently miss it on every query,
	// with no failing check anywhere.
	if covered != docCount {
		return corrupt("generation %d carries %d documents but its %d segments hold %d of them",
			gen, docCount, segments, covered)
	}
	// The commit row is written last: its presence is what tells a reader the
	// whole segment set behind it is durable. The bitmap travels with it, so a
	// reader takes the generation's visible documents as activation resolved
	// them instead of scanning them again for every pinned generation.
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO generation_lexical(generation_id, doc_count, token_total, visible) VALUES(?, ?, ?, ?)`,
		gen, docCount, tokenTotal, visible.bits); err != nil {
		return wrap("generation_lexical", err)
	}
	return nil
}

// docBitmap is the generation's visible document set, resolved once. A
// document id is the contentless index's own key, dense in practice, so a bit
// per id costs a byte per eight documents -- which is what lets a read skip a
// document an inherited segment still holds without a membership probe per
// posting.
type docBitmap struct {
	bits []byte
	max  int64
}

func (b *docBitmap) has(doc int64) bool {
	if doc < 0 || doc > b.max {
		return false
	}
	return b.bits[doc>>3]&(1<<uint(doc&7)) != 0
}

func (b *docBitmap) set(doc int64) {
	b.bits[doc>>3] |= 1 << uint(doc&7)
}

// generationDocumentQuery is the one pass that resolves a generation's
// documents, and with them its segment set. There is deliberately no ORDER BY
// and no DISTINCT: the set is folded in the caller's map, and asking the engine
// for either would materialize every document of the generation in a temporary
// b-tree.
const generationDocumentQuery = `SELECT su.doc_id, su.segment_id FROM search_units su
	WHERE EXISTS (SELECT 1 FROM generation_units gu
		WHERE gu.unit_id = su.unit_id AND gu.generation_id = ?1)`

// visibleDocuments resolves the generation's visible documents into a bitmap,
// the document statistics the scorer needs, and how many documents of each
// segment the generation still carries -- all from ONE pass over the
// generation's documents, which is the only place the segment a document lies
// in is known per document. The live counts are what the hidden count on
// generation_segments is derived from, they are the generation's segment set
// itself, and activation stores the bitmap, so no reader repeats this pass.
func visibleDocuments(ctx context.Context, tx *sql.Tx, gen int64) (*docBitmap, int64, int64, map[int64]int64, error) {
	var docCount, tokenTotal, maxDoc int64
	if err := tx.QueryRowContext(ctx, `SELECT count(*), coalesce(sum(su.token_count), 0), coalesce(max(su.doc_id), 0)
		FROM search_units su WHERE EXISTS (SELECT 1 FROM generation_units gu
			WHERE gu.unit_id = su.unit_id AND gu.generation_id = ?1)`, gen).
		Scan(&docCount, &tokenTotal, &maxDoc); err != nil {
		return nil, 0, 0, nil, wrap("search_units", err)
	}
	b := &docBitmap{bits: make([]byte, maxDoc/8+1), max: maxDoc}
	live := map[int64]int64{}
	rows, err := tx.QueryContext(ctx, generationDocumentQuery, gen)
	if err != nil {
		return nil, 0, 0, nil, wrap("search_units", err)
	}
	defer rows.Close()
	for rows.Next() {
		var doc int64
		var segment sql.NullInt64
		if err := rows.Scan(&doc, &segment); err != nil {
			return nil, 0, 0, nil, wrap("search_units", err)
		}
		if doc >= 0 && doc <= maxDoc {
			b.set(doc)
		}
		// A sealed unit's documents always name a segment; a NULL is a document
		// of a unit that is still building, which no generation carries.
		if segment.Valid {
			live[segment.Int64]++
		}
	}
	return b, docCount, tokenTotal, live, wrap("search_units", rows.Err())
}

// lexicalWriter folds the instance scan into the three streams. It holds one
// term's posting list and one document's groups at a time: a term is committed
// to the directory only when the scan leaves it, which is what makes the fold
// a single streaming pass.
type lexicalWriter struct {
	dir, text, list *partWriter

	term    []byte
	haveTrm bool
	// prev is the last term the directory received, from either path, so the
	// order guard holds across a verbatim copy as well as a folded term.
	prev     []byte
	havePrev bool
	df       int64
	listBuf  []byte
	textOff  int64
	listOff  int64

	doc     int64
	last    int64
	haveDoc bool
	counts  [16]int64 // one slot per column code; codes start at one
	terms   int64

	scratch [binary.MaxVarintLen64]byte
}

// add folds one input row: how many times a term occurs in one column of one
// document. Within a term, documents must not go backwards; terms must not go
// backwards either, which writeEntry checks for both this path and the merge's
// verbatim one. The streamed directory is ordered by construction and a reader
// binary-searches it, so an out-of-order write would silently produce a
// directory no lookup can trust.
func (w *lexicalWriter) add(term []byte, doc int64, code byte, n int64) error {
	if code == 0 {
		return internal("packed lexical fold received an unknown column code")
	}
	if n <= 0 {
		return corrupt("lexical staging counts term %q in document %d %d times", string(term), doc, n)
	}
	switch {
	case !w.haveTrm || !bytes.Equal(w.term, term):
		if err := w.flushTerm(); err != nil {
			return err
		}
		w.term = append(w.term[:0], term...)
		w.haveTrm = true
	case doc < w.doc:
		return corrupt("packed lexical fold emitted term %q at document %d after document %d", string(term), doc, w.doc)
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
func (w *lexicalWriter) flushDoc() error {
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
	w.listBuf = w.appendUvarint(w.listBuf, uint64(w.doc-w.prevDoc()))
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

// last is the document the previous encoded entry named, so the next one can
// be a delta. It resets with each term.
func (w *lexicalWriter) prevDoc() int64 { return w.last }

func (w *lexicalWriter) appendUvarint(dst []byte, v uint64) []byte {
	n := binary.PutUvarint(w.scratch[:], v)
	return append(dst, w.scratch[:n]...)
}

// flushTerm commits the term the fold just left: its bytes to term.text, its
// posting list to post.list and the entry that points at both to term.dir.
func (w *lexicalWriter) flushTerm() error {
	if !w.haveTrm {
		return nil
	}
	if err := w.flushDoc(); err != nil {
		return err
	}
	if w.df == 0 {
		return internal("packed lexical fold left term " + string(w.term) + " with no document")
	}
	if err := w.writeEntry(w.term, w.listBuf, w.df); err != nil {
		return err
	}
	w.resetTerm()
	return nil
}

// copyTerm writes a term whose posting bytes are taken verbatim from one input
// segment: the merge copies them unread, so the term's documents, their groups
// and their deltas are exactly the bytes that were packed. The caller has
// established that no other input carries the term and that this one holds no
// dead document, which is what makes an unread copy equal to a decoded merge.
func (w *lexicalWriter) copyTerm(term, list []byte, df int64) error {
	if err := w.flushTerm(); err != nil {
		return err
	}
	if df == 0 || len(list) == 0 {
		return internal("packed lexical merge was asked to copy term " + string(term) + " with no document")
	}
	return w.writeEntry(term, list, df)
}

// writeEntry appends one term to the three streams and is the ONE place the
// directory's order is established: a term that does not follow the previous
// one would produce a directory a binary search cannot trust, whichever path
// wrote it.
func (w *lexicalWriter) writeEntry(term, list []byte, df int64) error {
	if w.havePrev && bytes.Compare(term, w.prev) <= 0 {
		return corrupt("packed lexical fold wrote term %q after term %q", string(term), string(w.prev))
	}
	if len(term) > int(^uint16(0)) {
		return corrupt("packed lexical fold carries a term of %d bytes", len(term))
	}
	var entry [termEntryBytes]byte
	binary.LittleEndian.PutUint64(entry[termEntryTextOff:], uint64(w.textOff))
	binary.LittleEndian.PutUint64(entry[termEntryListOff:], uint64(w.listOff))
	binary.LittleEndian.PutUint32(entry[termEntryListLen:], uint32(len(list)))
	binary.LittleEndian.PutUint32(entry[termEntryDF:], uint32(df))
	binary.LittleEndian.PutUint16(entry[termEntryTextLen:], uint16(len(term)))
	if err := w.text.write(term); err != nil {
		return err
	}
	if err := w.list.write(list); err != nil {
		return err
	}
	if err := w.dir.write(entry[:]); err != nil {
		return err
	}
	w.textOff += int64(len(term))
	w.listOff += int64(len(list))
	w.terms++
	w.prev = append(w.prev[:0], term...)
	w.havePrev = true
	return nil
}

func (w *lexicalWriter) resetTerm() {
	w.listBuf, w.df, w.last, w.haveTrm, w.haveDoc = w.listBuf[:0], 0, 0, false, false
	w.counts = [16]int64{}
}

func (w *lexicalWriter) close() error {
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
