package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"log/slog"
	"runtime"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
)

// The packed per-generation term statistics, built once at activation
// (ADR-0007 Decision 1). Visibility is resolved HERE, once, into a document
// bitmap; one bare instance scan of the vocabulary is then folded against it
// and streamed into three chunked streams. Nothing here holds the vocabulary:
// heap is one part plus one term's posting list plus the bitmap.
//
// Without it a query pays three b-tree descents per posting instance -- the
// vocabulary row, the document row and the generation-membership probe -- on
// every term of every request, and a temporary b-tree for each term's document
// frequency.

// Lexical stream names. They are the `stream` column of
// generation_lexical_parts and are duplicated in that table's CHECK
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

// buildLexical writes the packed term statistics for gen. It runs inside
// Activate's transaction, after the packed adjacency and before the active
// pointer flips, so a generation is published only with the structure every
// lexical query reads and a failed build fails the activation.
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
	slog.Default().Info("packed lexical build started", "generation", gen)
	var terms, instances int64
	defer func() {
		runtime.ReadMemStats(&after)
		slog.Default().Info("packed lexical build finished",
			"generation", gen, "duration_ms", time.Since(started).Milliseconds(),
			"terms", terms, "instances", instances,
			// Signed: a build that ends after a collection leaves less live
			// heap than it found, and an unsigned subtraction would report that
			// as eighteen exabytes.
			"heap_delta_bytes", int64(after.HeapInuse)-int64(before.HeapInuse))
	}()

	visible, docCount, tokenTotal, err := visibleDocuments(ctx, tx, gen)
	if err != nil {
		return err
	}
	w := &lexicalWriter{
		dir:  newPartWriter(ctx, tx, gen, lexicalPartsTable, streamTermDir),
		text: newPartWriter(ctx, tx, gen, lexicalPartsTable, streamTermText),
		list: newPartWriter(ctx, tx, gen, lexicalPartsTable, streamPostList),
	}
	// No ORDER BY: the instance vocabulary emits its rows in term order, and
	// asking for that order makes the whole vocabulary materialize in a
	// temporary b-tree before the first row -- unbounded memory for a structure
	// whose point is that it is streamed. The order the scan actually delivers
	// is verified below rather than trusted.
	rows, err := tx.QueryContext(ctx, `SELECT v.term, v.doc, v.col FROM search_vocab v`)
	if err != nil {
		return wrap("search_vocab", err)
	}
	defer rows.Close()
	// RawBytes aliases the driver's own buffer, so a 90-million-row scan
	// allocates nothing per row; a term is copied only when it changes.
	var term, col sql.RawBytes
	var doc int64
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return model.Canceled(err)
		}
		if err := rows.Scan(&term, &doc, &col); err != nil {
			return wrap("search_vocab", err)
		}
		instances++
		if !visible.has(doc) {
			continue
		}
		column, ok := searchColumns[string(col)]
		if !ok {
			return corrupt("search_vocab names column %q, which search_fts does not declare", string(col))
		}
		if err := w.add(term, doc, lexicalColumnCode(column)); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return wrap("search_vocab", err)
	}
	if err := w.close(); err != nil {
		return err
	}
	terms = w.terms
	// The commit row is written last: its presence is what tells a reader every
	// part behind it is durable.
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO generation_lexical(generation_id, doc_count, token_total, term_count) VALUES(?, ?, ?, ?)`,
		gen, docCount, tokenTotal, w.terms); err != nil {
		return wrap("generation_lexical", err)
	}
	return nil
}

// docBitmap is the generation's visible document set, resolved once. A
// document id is the contentless index's own key, dense in practice, so a bit
// per id costs a byte per eight documents -- the whole reason the build can
// fold the instance scan without a membership probe per instance.
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

// visibleDocuments resolves the generation's visible documents into a bitmap
// and returns the document statistics the scorer needs, in one pass each.
func visibleDocuments(ctx context.Context, tx *sql.Tx, gen int64) (*docBitmap, int64, int64, error) {
	var docCount, tokenTotal, maxDoc int64
	if err := tx.QueryRowContext(ctx, `SELECT count(*), coalesce(sum(su.token_count), 0), coalesce(max(su.doc_id), 0)
		FROM search_units su WHERE EXISTS (SELECT 1 FROM generation_units gu
			WHERE gu.unit_id = su.unit_id AND gu.generation_id = ?1)`, gen).
		Scan(&docCount, &tokenTotal, &maxDoc); err != nil {
		return nil, 0, 0, wrap("search_units", err)
	}
	b := &docBitmap{bits: make([]byte, maxDoc/8+1), max: maxDoc}
	rows, err := tx.QueryContext(ctx, `SELECT su.doc_id FROM search_units su
		WHERE EXISTS (SELECT 1 FROM generation_units gu
			WHERE gu.unit_id = su.unit_id AND gu.generation_id = ?1)`, gen)
	if err != nil {
		return nil, 0, 0, wrap("search_units", err)
	}
	defer rows.Close()
	for rows.Next() {
		var doc int64
		if err := rows.Scan(&doc); err != nil {
			return nil, 0, 0, wrap("search_units", err)
		}
		if doc >= 0 && doc <= maxDoc {
			b.set(doc)
		}
	}
	return b, docCount, tokenTotal, wrap("search_units", rows.Err())
}

// lexicalWriter folds the instance scan into the three streams. It holds one
// term's posting list and one document's groups at a time: a term is committed
// to the directory only when the scan leaves it, which is what makes the fold
// a single streaming pass.
type lexicalWriter struct {
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

// add folds one instance row. Terms and, within a term, documents must not go
// backwards: the streamed directory is ordered by construction and a reader
// binary-searches it, so an out-of-order scan would silently produce a
// directory no lookup can trust.
func (w *lexicalWriter) add(term []byte, doc int64, code byte) error {
	if code == 0 {
		return internal("packed lexical build received an unknown column code")
	}
	switch {
	case !w.haveTrm || !bytes.Equal(w.term, term):
		if w.haveTrm && bytes.Compare(term, w.term) < 0 {
			return corrupt("search_vocab emitted term %q after term %q", string(term), string(w.term))
		}
		if err := w.flushTerm(); err != nil {
			return err
		}
		w.term = append(w.term[:0], term...)
		w.haveTrm = true
	case doc < w.doc:
		return corrupt("search_vocab emitted term %q at document %d after document %d", string(term), doc, w.doc)
	}
	if !w.haveDoc || doc != w.doc {
		if err := w.flushDoc(); err != nil {
			return err
		}
		w.doc, w.haveDoc = doc, true
	}
	w.counts[code]++
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

// flushTerm commits the term the scan just left: its bytes to term.text, its
// posting list to post.list and the entry that points at both to term.dir.
func (w *lexicalWriter) flushTerm() error {
	if !w.haveTrm {
		return nil
	}
	if err := w.flushDoc(); err != nil {
		return err
	}
	if w.df == 0 {
		// Every instance of this term is in a document the generation does not
		// carry. It is not a term of this generation and takes no entry.
		w.resetTerm()
		return nil
	}
	if len(w.term) > int(^uint16(0)) {
		return corrupt("search_vocab carries a term of %d bytes", len(w.term))
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
