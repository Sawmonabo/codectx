package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"slices"

	"github.com/Sawmonabo/codectx/internal/model"
)

// The read side of the packed term statistics (ADR-0007 Decision 1). A
// single-token term's posting list, its document frequency and the
// generation's document statistics are answered from the packed streams, so a
// query steps no vocabulary row, no document row and no membership probe. A
// phrase keeps the live path: it needs the offsets the packed form does not
// store.

// lexicalIndex is one request's bounded view of a generation's packed term
// statistics: the commit row plus a small window of parts. It holds no
// vocabulary -- the window is at most partsPerStream parts of one stream.
type lexicalIndex struct {
	r          *PinnedReader
	docCount   int64
	tokenTotal int64
	termCount  int64
	cache      map[string]*partWindow
}

// openLexical opens the packed term statistics of the reader's pinned
// generation. A generation with no generation_lexical row was never published
// with one, which is a corrupt store rather than an empty vocabulary: the row
// is written last, so its absence means the build did not finish.
func (r *PinnedReader) openLexical(ctx context.Context) (*lexicalIndex, error) {
	x := &lexicalIndex{r: r, cache: map[string]*partWindow{}}
	err := r.s.read(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, `SELECT doc_count, token_total, term_count
			FROM generation_lexical WHERE generation_id = ?`, r.gen).Scan(&x.docCount, &x.tokenTotal, &x.termCount)
		if isNoRows(err) {
			return corrupt("generation %d has no packed term statistics; it was published without them", r.gen)
		}
		return wrap("generation_lexical", err)
	})
	if err != nil {
		return nil, err
	}
	return x, nil
}

// part returns one part of a stream through the request's bounded window,
// reading it from the store on a miss and evicting the least recently used
// part when the window is full.
func (x *lexicalIndex) part(ctx context.Context, stream string, index int) ([]byte, error) {
	w := x.cache[stream]
	if w == nil {
		w = &partWindow{size: partSizeFor(stream), parts: map[int][]byte{}}
		x.cache[stream] = w
	}
	if b, ok := w.parts[index]; ok {
		if i := slices.Index(w.order, index); i >= 0 {
			w.order = append(slices.Delete(w.order, i, i+1), index)
		}
		return b, nil
	}
	var raw []byte
	err := x.r.s.read(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, `SELECT bytes FROM generation_lexical_parts
			WHERE generation_id = ? AND stream = ? AND part = ?`, x.r.gen, stream, index).Scan(&raw)
		if isNoRows(err) {
			return corrupt("packed lexical stream %q of generation %d is missing part %d", stream, x.r.gen, index)
		}
		return wrap("generation_lexical_parts", err)
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
// part boundary a value or a list may straddle. Parts are fixed-size except
// the last, which is what makes the part index pure arithmetic.
func (x *lexicalIndex) readAt(ctx context.Context, stream string, off int64, n int) ([]byte, error) {
	size := int64(partSizeFor(stream))
	out := make([]byte, 0, n)
	for len(out) < n {
		index := int(off / size)
		part, err := x.part(ctx, stream, index)
		if err != nil {
			return nil, err
		}
		begin := int(off % size)
		if begin >= len(part) {
			return nil, corrupt("packed lexical stream %q of generation %d ends before offset %d", stream, x.r.gen, off)
		}
		take := min(len(part)-begin, n-len(out))
		out = append(out, part[begin:begin+take]...)
		off += int64(take)
	}
	return out, nil
}

// termEntry is one decoded term-directory entry.
type termEntry struct {
	textOff int64
	textLen int
	listOff int64
	listLen int
	df      int64
}

func (x *lexicalIndex) entryAt(ctx context.Context, index int64) (termEntry, error) {
	raw, err := x.readAt(ctx, streamTermDir, index*termEntryBytes, termEntryBytes)
	if err != nil {
		return termEntry{}, err
	}
	return termEntry{
		textOff: int64(binary.LittleEndian.Uint64(raw[termEntryTextOff:])),
		listOff: int64(binary.LittleEndian.Uint64(raw[termEntryListOff:])),
		listLen: int(binary.LittleEndian.Uint32(raw[termEntryListLen:])),
		df:      int64(binary.LittleEndian.Uint32(raw[termEntryDF:])),
		textLen: int(binary.LittleEndian.Uint16(raw[termEntryTextLen:])),
	}, nil
}

// lookup finds a term's directory entry by binary search over the fixed-width
// directory, which the build wrote in term order. A term the generation does
// not carry answers ok=false, not an error: a query for a word nobody indexed
// is an empty result.
func (x *lexicalIndex) lookup(ctx context.Context, term string) (termEntry, bool, error) {
	lo, hi := int64(0), x.termCount
	want := []byte(term)
	for lo < hi {
		mid := lo + (hi-lo)/2
		e, err := x.entryAt(ctx, mid)
		if err != nil {
			return termEntry{}, false, err
		}
		text, err := x.readAt(ctx, streamTermText, e.textOff, e.textLen)
		if err != nil {
			return termEntry{}, false, err
		}
		switch bytes.Compare(text, want) {
		case 0:
			return e, true, nil
		case -1:
			lo = mid + 1
		default:
			hi = mid
		}
	}
	return termEntry{}, false, nil
}

// SearchStats returns the visible document count and total token length from
// the generation's packed term statistics, where activation recorded them.
func (r *PinnedReader) SearchStats(ctx context.Context) (documents, tokens int64, err error) {
	x, err := r.openLexical(ctx)
	if err != nil {
		return 0, 0, err
	}
	return x.docCount, x.tokenTotal, nil
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
	x, err := r.openLexical(ctx)
	if err != nil {
		return nil, err
	}
	// Input order, zero for a term no visible document carries: the caller
	// indexes this slice by the position of its own term.
	out := make([]int64, len(terms))
	for i, t := range terms {
		if t == "" {
			return nil, invalid("document frequency term %d is empty", i)
		}
		e, ok, err := x.lookup(ctx, t)
		if err != nil {
			return nil, err
		}
		if ok {
			out[i] = e.df
		}
	}
	return out, nil
}

// TermCounts opens a stream over one single-token term's packed posting list:
// one TermOccurrence per (document, column), documents ascending and a
// document's columns in column-name order, carrying the counts the scorer sums
// and no offsets. A phrase needs offsets and uses TermOccurrences instead.
func (p *PostingSession) TermCounts(ctx context.Context, term string) (*PackedStream, error) {
	if term == "" {
		return nil, invalid("term must not be empty")
	}
	if p.tx == nil {
		return nil, internal("posting session is already closed")
	}
	if p.lex == nil {
		x, err := p.r.openLexical(ctx)
		if err != nil {
			return nil, err
		}
		p.lex = x
	}
	e, ok, err := p.lex.lookup(ctx, term)
	if err != nil {
		return nil, err
	}
	if !ok {
		return &PackedStream{}, nil
	}
	// One term's list is the whole heap cost of the stream, which is the bound
	// ADR-0007 states: a part plus a list, never the vocabulary.
	raw, err := p.lex.readAt(ctx, streamPostList, e.listOff, e.listLen)
	if err != nil {
		return nil, err
	}
	return &PackedStream{term: term, gen: p.r.gen, raw: raw}, nil
}

// PackedStream decodes one term's packed posting list in page-sized refills. A
// refill always ends on a document boundary, so a document's (column, count)
// groups are never split across two refills.
type PackedStream struct {
	term string
	gen  int64
	raw  []byte
	doc  int64
}

// Close releases the stream. It holds no statement; the method exists so a
// packed stream and a live one are the same contract to the caller.
func (s *PackedStream) Close() error {
	s.raw = nil
	return nil
}

// Next returns the next refill: the grouped occurrences of whole documents, at
// least limit rows unless the list is exhausted. A nil result with a nil error
// means the list is exhausted.
func (s *PackedStream) Next(ctx context.Context, limit int) ([]TermOccurrence, error) {
	limit = pageLimit(ctx, limit)
	if err := ctx.Err(); err != nil {
		return nil, model.Canceled(err)
	}
	var out []TermOccurrence
	for len(s.raw) > 0 && len(out) < limit {
		delta, n := binary.Uvarint(s.raw)
		if n <= 0 {
			return nil, s.malformed()
		}
		s.raw = s.raw[n:]
		s.doc += int64(delta)
		groups, n := binary.Uvarint(s.raw)
		if n <= 0 || groups == 0 {
			return nil, s.malformed()
		}
		s.raw = s.raw[n:]
		for i := uint64(0); i < groups; i++ {
			if len(s.raw) == 0 {
				return nil, s.malformed()
			}
			code := s.raw[0]
			s.raw = s.raw[1:]
			if code == 0 || int(code) > len(lexicalColumns) {
				return nil, s.malformed()
			}
			count, n := binary.Uvarint(s.raw)
			if n <= 0 {
				return nil, s.malformed()
			}
			s.raw = s.raw[n:]
			out = append(out, TermOccurrence{RowID: s.doc, Column: lexicalColumns[code-1], Count: int64(count)})
		}
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

func (s *PackedStream) malformed() error {
	return corrupt("packed lexical posting list of term %q in generation %d is malformed", s.term, s.gen)
}

// The per-document attribute record (ADR-0007 Decision 2). One record holds
// EVERY field the search package reads from a document row -- the ranker's
// tie-break and the deduplication key read the identity, path, kind, name,
// qualified name, signature and byte range, and the filters read two of them
// -- so a candidate is hydrated from the packed stream and no document row is
// read per candidate. The layout, in order: search key, node id (a presence
// byte and, when present, the canonical id), file id, start byte, end byte as
// a delta from the start, token count, then kind, path, name, qualified name
// and signature, each preceded by its length. Ids are the raw 32 bytes rather
// than their hex spelling: the stream is written once per document and read on
// every page that names it.
const attrIDBytes = 32

// encodeSearchDocument appends d's attribute record to dst.
func encodeSearchDocument(dst []byte, d SearchDocument) []byte {
	key, _ := model.DecodeID(d.ID)
	file, _ := model.DecodeID(string(d.FileID))
	dst = append(dst, key...)
	if d.NodeID == "" {
		dst = append(dst, 0)
	} else {
		node, _ := model.DecodeID(string(d.NodeID))
		dst = append(dst, 1)
		dst = append(dst, node...)
	}
	dst = append(dst, file...)
	dst = binary.AppendUvarint(dst, d.Bytes.Start)
	dst = binary.AppendUvarint(dst, d.Bytes.End-d.Bytes.Start)
	dst = binary.AppendUvarint(dst, uint64(d.TokenCount))
	for _, s := range [...]string{string(d.Kind), d.Path, d.Name, d.QualifiedName, d.Signature} {
		dst = binary.AppendUvarint(dst, uint64(len(s)))
		dst = append(dst, s...)
	}
	return dst
}

// decodeSearchDocument reads back one attribute record. A record that does not
// decode is a corrupt store, not an empty document: the stream is written in
// one transaction with the commit row that publishes it.
func decodeSearchDocument(raw []byte, doc int64) (SearchDocument, error) {
	d := SearchDocument{RowID: doc}
	r := attrReader{raw: raw}
	d.ID = idHex(r.fixed(attrIDBytes))
	if flag := r.byteAt(); flag == 1 {
		d.NodeID = model.NodeID(idHex(r.fixed(attrIDBytes)))
	} else if flag != 0 {
		r.bad = true
	}
	d.FileID = model.FileID(idHex(r.fixed(attrIDBytes)))
	start := r.uvarint()
	d.Bytes = model.ByteRange{Start: start, End: start + r.uvarint()}
	d.TokenCount = int64(r.uvarint())
	d.Kind = model.NodeKind(r.text())
	d.Path, d.Name, d.QualifiedName, d.Signature = r.text(), r.text(), r.text(), r.text()
	if r.bad || len(r.raw) != 0 {
		return SearchDocument{}, corrupt("the packed attribute record of document %d is malformed", doc)
	}
	return d, nil
}

// attrReader decodes one record field by field, latching the first malformed
// field instead of returning an error from every step: a record is either
// whole or the store is corrupt, and one check at the end says which.
type attrReader struct {
	raw []byte
	bad bool
}

func (r *attrReader) fixed(n int) []byte {
	if r.bad || len(r.raw) < n {
		r.bad = true
		return nil
	}
	out := r.raw[:n]
	r.raw = r.raw[n:]
	return out
}

func (r *attrReader) byteAt() byte {
	b := r.fixed(1)
	if b == nil {
		return 0
	}
	return b[0]
}

func (r *attrReader) uvarint() uint64 {
	if r.bad {
		return 0
	}
	v, n := binary.Uvarint(r.raw)
	if n <= 0 {
		r.bad = true
		return 0
	}
	r.raw = r.raw[n:]
	return v
}

func (r *attrReader) text() string {
	n := r.uvarint()
	if r.bad || uint64(len(r.raw)) < n {
		r.bad = true
		return ""
	}
	s := string(r.raw[:n])
	r.raw = r.raw[n:]
	return s
}

// docEntry is one decoded document-directory entry.
type docEntry struct {
	doc     int64
	attrOff int64
	attrLen int
}

func (x *lexicalIndex) docEntryAt(ctx context.Context, ordinal int64) (docEntry, error) {
	raw, err := x.readAt(ctx, streamDocDir, ordinal*docEntryBytes, docEntryBytes)
	if err != nil {
		return docEntry{}, err
	}
	return docEntry{
		doc:     int64(binary.LittleEndian.Uint64(raw[docEntryID:])),
		attrOff: int64(binary.LittleEndian.Uint64(raw[docEntryAttrOff:])),
		attrLen: int(binary.LittleEndian.Uint32(raw[docEntryAttrLen:])),
	}, nil
}

// PackedDocuments hydrates visible documents by rowid from the generation's
// packed attribute stream (len(rowids) <= model.MaxPageItems). Rowids the
// generation does not carry are omitted, and the answer is in the requested
// order, so a ranked page hydrates into the order it was ranked in.
//
// A page of ascending rowids walks the document directory once: each lookup
// resumes where the previous one stopped, so a page costs a bounded window of
// parts and no statement per document. A rowid that goes backwards restarts
// the walk, which keeps the answer right for a caller that does not sort.
func (r *PinnedReader) PackedDocuments(ctx context.Context, rowids []int64) ([]SearchDocument, error) {
	if len(rowids) == 0 {
		return nil, nil
	}
	if len(rowids) > model.MaxPageItems {
		return nil, invalid("search document hydration asked for %d rowids, limit %d", len(rowids), model.MaxPageItems)
	}
	x, err := r.openLexical(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]SearchDocument, 0, len(rowids))
	lo, prev := int64(0), int64(0)
	for _, id := range rowids {
		if id <= 0 {
			return nil, invalid("search document rowid %d is not positive", id)
		}
		if id < prev {
			lo = 0
		}
		prev = id
		ordinal, e, ok, err := x.findDocument(ctx, lo, id)
		if err != nil {
			return nil, err
		}
		lo = ordinal
		if !ok {
			continue
		}
		raw, err := x.readAt(ctx, streamDocAttr, e.attrOff, e.attrLen)
		if err != nil {
			return nil, err
		}
		d, err := decodeSearchDocument(raw, id)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
		lo = ordinal + 1
	}
	return out, nil
}

// findDocument binary-searches the document directory from ordinal lo for doc,
// answering the ordinal the search settled on so the next lookup of a larger
// rowid starts there instead of at the beginning.
func (x *lexicalIndex) findDocument(ctx context.Context, lo, doc int64) (int64, docEntry, bool, error) {
	hi := x.docCount
	for lo < hi {
		mid := lo + (hi-lo)/2
		e, err := x.docEntryAt(ctx, mid)
		if err != nil {
			return lo, docEntry{}, false, err
		}
		switch {
		case e.doc == doc:
			return mid, e, true, nil
		case e.doc < doc:
			lo = mid + 1
		default:
			hi = mid
		}
	}
	return lo, docEntry{}, false, nil
}
