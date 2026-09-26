package sqlite

import (
	"context"
	"database/sql"
	"encoding/binary"

	"github.com/Sawmonabo/codectx/internal/model"
)

// The per-document attribute stream of the packed lexical structure (ADR-0007
// Decision 2). A segment carries, once per document, every field the search
// tier reads from a document row -- its identity, its file, its path, kind,
// name, qualified name and signature, its byte range and its token count -- so
// a candidate is hydrated from the packed bytes the query is already reading
// instead of from a row of its own. Without it every candidate costs an index
// descent into the document table: a query serving one page of 200 hits reads
// tens of thousands of rows, one per candidate, for values that never change
// once a unit is sealed.
//
// It is TWO streams for the same reason the term vocabulary is: `doc.dir` is a
// fixed-width directory in ascending document rowid, so a rowid is found by
// arithmetic rather than by a scan, and `doc.attr` holds the variable-length
// records it points into. Keeping the directory in the same stream would mean
// holding every entry of a segment in memory until its records were written,
// which is the whole-segment residency the packed form exists to avoid.
const (
	streamDocDir  = "doc.dir"
	streamDocAttr = "doc.attr"
)

// docEntryBytes is the width of one document-directory entry. Fixed width is
// what makes a lookup a binary search by arithmetic.
const docEntryBytes = 20

// Field offsets inside one document-directory entry, little-endian throughout.
const (
	docEntryRowID   = 0  // uint64 document rowid
	docEntryAttrOff = 8  // uint64 offset into doc.attr
	docEntryAttrLen = 16 // uint32 byte length of the record
)

// segmentAttributeQuery reads the attributes of the documents a segment holds,
// in ascending document rowid. idx_search_segment(segment_id, doc_id) both
// selects the rows and delivers that order, so no temporary b-tree is built at
// the moment a seal publishes a unit. A document may be named by several rows
// -- a carry chain shares one document across its units -- so the walk folds
// consecutive duplicates, which the ordering makes adjacent.
const segmentAttributeQuery = `SELECT su.doc_id, su.search_key, ni.canonical, su.file_id, su.path, su.kind,
		su.name, su.qualified_name, su.signature, su.start_byte, su.end_byte, su.token_count
	FROM search_units su LEFT JOIN node_ids ni ON ni.id = su.node_id
	WHERE su.segment_id = ?1 ORDER BY su.doc_id`

// docWriter appends the two document streams. It holds one record at a time:
// the directory entry is written with the record it points at, so the pass is
// streaming and a segment's documents are never resident together.
type docWriter struct {
	dir, attr *partWriter
	off       int64
	docs      int64
	last      int64
	started   bool
	entry     [docEntryBytes]byte
}

func newDocWriter(ctx context.Context, tx *sql.Tx, segment int64) *docWriter {
	return &docWriter{
		dir:  newPartWriter(ctx, tx, segmentKey(segment), streamDocDir),
		attr: newPartWriter(ctx, tx, segmentKey(segment), streamDocAttr),
	}
}

// add appends one document's record. Documents must strictly ascend: the
// directory is binary-searched, so an entry out of order would silently answer
// the wrong document's attributes for a candidate -- a hit serving another
// document's path and byte range.
func (w *docWriter) add(doc int64, record []byte) error {
	if w.started && doc <= w.last {
		return corrupt("packed lexical document stream wrote document %d after document %d", doc, w.last)
	}
	if len(record) > int(^uint32(0)) {
		return corrupt("packed lexical document %d carries a record of %d bytes", doc, len(record))
	}
	binary.LittleEndian.PutUint64(w.entry[docEntryRowID:], uint64(doc))
	binary.LittleEndian.PutUint64(w.entry[docEntryAttrOff:], uint64(w.off))
	binary.LittleEndian.PutUint32(w.entry[docEntryAttrLen:], uint32(len(record)))
	if err := w.attr.write(record); err != nil {
		return err
	}
	if err := w.dir.write(w.entry[:]); err != nil {
		return err
	}
	w.off += int64(len(record))
	w.docs++
	w.last, w.started = doc, true
	return nil
}

// bytes is how much the two streams added to the segment's packed size.
func (w *docWriter) bytes() int64 { return w.dir.total + w.attr.total }

func (w *docWriter) close() error {
	for _, p := range []*partWriter{w.dir, w.attr} {
		if err := p.close(); err != nil {
			return err
		}
	}
	return nil
}

// appendDocumentRecord encodes one document's attributes. Every field is
// length-prefixed or a varint, so the record is self-describing and a merge
// copies it verbatim rather than decoding and re-encoding it -- which is what
// makes a merged segment's attributes the same bytes the seal packed.
func appendDocumentRecord(dst []byte, key, node, file []byte, path, kind, name, qualified, signature string,
	start, end, tokens int64) []byte {
	dst = appendBlob(dst, key)
	// A document with no node is a file-level document; the record says so
	// with a zero rather than an empty blob, because an empty canonical id and
	// no node at all are different answers to the caller.
	if node == nil {
		dst = binary.AppendUvarint(dst, 0)
	} else {
		dst = binary.AppendUvarint(dst, uint64(len(node))+1)
		dst = append(dst, node...)
	}
	dst = appendBlob(dst, file)
	dst = binary.AppendUvarint(dst, uint64(tokens))
	dst = binary.AppendUvarint(dst, uint64(start))
	// The end is a delta: the schema holds end_byte >= start_byte, so the
	// difference is a span rather than a second absolute offset.
	dst = binary.AppendUvarint(dst, uint64(end-start))
	for _, s := range []string{path, kind, name, qualified, signature} {
		dst = binary.AppendUvarint(dst, uint64(len(s)))
		dst = append(dst, s...)
	}
	return dst
}

func appendBlob(dst, b []byte) []byte {
	dst = binary.AppendUvarint(dst, uint64(len(b)))
	return append(dst, b...)
}

// decodeDocument reads one packed record back into the document the search
// tier serves. The values are the ones the seal read from the document's row,
// so a hydrated candidate is byte-for-byte what a row read answered.
func decodeDocument(doc int64, record []byte) (SearchDocument, error) {
	d := SearchDocument{RowID: doc}
	c := recordCursor{raw: record, doc: doc}
	key, err := c.blob()
	if err != nil {
		return SearchDocument{}, err
	}
	d.ID = idHex(key)
	node, err := c.uvarint()
	if err != nil {
		return SearchDocument{}, err
	}
	if node > 0 {
		raw, err := c.take(int(node - 1))
		if err != nil {
			return SearchDocument{}, err
		}
		d.NodeID = model.NodeID(idHex(raw))
	}
	file, err := c.blob()
	if err != nil {
		return SearchDocument{}, err
	}
	d.FileID = model.FileID(idHex(file))
	tokens, err := c.uvarint()
	if err != nil {
		return SearchDocument{}, err
	}
	d.TokenCount = int64(tokens)
	start, err := c.uvarint()
	if err != nil {
		return SearchDocument{}, err
	}
	span, err := c.uvarint()
	if err != nil {
		return SearchDocument{}, err
	}
	d.Bytes = model.ByteRange{Start: start, End: start + span}
	fields := [...]*string{&d.Path, nil, &d.Name, &d.QualifiedName, &d.Signature}
	for i := range fields {
		s, err := c.blob()
		if err != nil {
			return SearchDocument{}, err
		}
		if fields[i] == nil {
			d.Kind = model.NodeKind(s)
			continue
		}
		*fields[i] = string(s)
	}
	if len(c.raw) != 0 {
		return SearchDocument{}, corrupt("packed lexical attributes of document %d carry %d bytes past its fields",
			doc, len(c.raw))
	}
	return d, nil
}

// recordCursor decodes one attribute record field by field, refusing a record
// that ends early instead of reading past it.
type recordCursor struct {
	raw []byte
	doc int64
}

func (c *recordCursor) uvarint() (uint64, error) {
	v, n := binary.Uvarint(c.raw)
	if n <= 0 {
		return 0, corrupt("packed lexical attributes of document %d are malformed", c.doc)
	}
	c.raw = c.raw[n:]
	return v, nil
}

func (c *recordCursor) take(n int) ([]byte, error) {
	if n < 0 || n > len(c.raw) {
		return nil, corrupt("packed lexical attributes of document %d are malformed", c.doc)
	}
	out := c.raw[:n]
	c.raw = c.raw[n:]
	return out, nil
}

func (c *recordCursor) blob() ([]byte, error) {
	n, err := c.uvarint()
	if err != nil {
		return nil, err
	}
	return c.take(int(n))
}

// foldUnitDocuments packs the attributes of the documents the fold just
// claimed for the segment, in ascending rowid, and answers how many it wrote.
// That count is the segment's doc_count: it is the number of entries the
// directory carries, so the reader's binary search and the compaction's
// statistics are talking about the same documents.
func foldUnitDocuments(ctx context.Context, tx *sql.Tx, segment int64) (docs, bytes int64, err error) {
	w := newDocWriter(ctx, tx, segment)
	rows, err := tx.QueryContext(ctx, segmentAttributeQuery, segment)
	if err != nil {
		return 0, 0, wrap("search_units", err)
	}
	defer rows.Close()
	var record []byte
	last, started := int64(0), false
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return 0, 0, model.Canceled(err)
		}
		var doc, start, end, tokens int64
		var key, node, file []byte
		var path, kind, name, qualified, signature string
		if err := rows.Scan(&doc, &key, &node, &file, &path, &kind, &name, &qualified, &signature,
			&start, &end, &tokens); err != nil {
			return 0, 0, wrap("search_units", err)
		}
		if started && doc == last {
			continue
		}
		last, started = doc, true
		record = appendDocumentRecord(record[:0], key, node, file, path, kind, name, qualified, signature,
			start, end, tokens)
		if err := w.add(doc, record); err != nil {
			return 0, 0, err
		}
	}
	if err := rows.Err(); err != nil {
		return 0, 0, wrap("search_units", err)
	}
	if err := w.close(); err != nil {
		return 0, 0, err
	}
	return w.docs, w.bytes(), nil
}

// docScan reads one segment's documents in rowid order: the fixed-width
// directory and, with it, the record each entry points at. Both streams were
// written front to back in the same order, so the scan is sequential and one
// part of each is resident at a time.
type docScan struct {
	id         int64
	dir, attr  *streamReader
	entries    int64
	index      int64
	attrOff    int64
	doc        int64
	record     []byte
	live       bool
	haveBefore bool
}

func openDocScan(ctx context.Context, tx *sql.Tx, id, entries int64) *docScan {
	return &docScan{
		id:      id,
		dir:     newStreamReader(ctx, tx, id, streamDocDir),
		attr:    newStreamReader(ctx, tx, id, streamDocAttr),
		entries: entries,
	}
}

// next advances to the segment's next document, or leaves the scan not live
// when its directory is exhausted. record aliases the reader's resident part
// and stays valid until the next call.
func (s *docScan) next() error {
	if s.index >= s.entries {
		s.live, s.record = false, nil
		return nil
	}
	raw, err := s.dir.read(s.index*docEntryBytes, docEntryBytes)
	if err != nil {
		return err
	}
	doc := int64(binary.LittleEndian.Uint64(raw[docEntryRowID:]))
	off := int64(binary.LittleEndian.Uint64(raw[docEntryAttrOff:]))
	length := int(binary.LittleEndian.Uint32(raw[docEntryAttrLen:]))
	// The two streams are written front to back in one pass, so a record
	// begins exactly where the previous one ended, and the rowids ascend. A
	// directory that says otherwise is a structure this sequential scan would
	// read wrongly without noticing.
	if off != s.attrOff {
		return corrupt("packed lexical segment %d document %d points at attributes %d, not %d",
			s.id, s.index, off, s.attrOff)
	}
	if s.haveBefore && doc <= s.doc {
		return corrupt("packed lexical segment %d leaves document %d after document %d", s.id, doc, s.doc)
	}
	if s.record, err = s.attr.read(off, length); err != nil {
		return err
	}
	s.doc, s.attrOff = doc, off+int64(length)
	s.index++
	s.live, s.haveBefore = true, true
	return nil
}

// mergeDocuments folds the inputs' document streams into the merged segment's,
// keeping every document a row still points at and dropping the dead ones. The
// inputs hold disjoint document sets -- a document lies in exactly one segment
// -- so the merge is one pass by ascending rowid, and each kept record is
// copied verbatim: the merged segment answers the same attribute bytes the
// seal packed.
func mergeDocuments(ctx context.Context, tx *sql.Tx, inputs []int64, stats map[int64]segmentStat,
	keep *docBitmap, merged int64) (docs, bytes int64, err error) {
	scans := make([]*docScan, 0, len(inputs))
	for _, id := range inputs {
		sc := openDocScan(ctx, tx, id, stats[id].docs)
		if err := sc.next(); err != nil {
			return 0, 0, err
		}
		scans = append(scans, sc)
	}
	w := newDocWriter(ctx, tx, merged)
	for {
		if err := ctx.Err(); err != nil {
			return 0, 0, model.Canceled(err)
		}
		pick := -1
		for i, sc := range scans {
			if sc.live && (pick < 0 || sc.doc < scans[pick].doc) {
				pick = i
			}
		}
		if pick < 0 {
			break
		}
		sc := scans[pick]
		if keep.has(sc.doc) {
			if err := w.add(sc.doc, sc.record); err != nil {
				return 0, 0, err
			}
		}
		if err := sc.next(); err != nil {
			return 0, 0, err
		}
	}
	if err := w.close(); err != nil {
		return 0, 0, err
	}
	return w.docs, w.bytes(), nil
}
