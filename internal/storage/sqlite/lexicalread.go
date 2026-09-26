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
// generation's document statistics are answered from the generation's
// segments, so a query steps no vocabulary row, no document row and no
// membership probe. A phrase keeps the live path: it needs the offsets the
// packed form does not store.
//
// A generation names several segments and each holds its own documents, so
// every read here is a merge across them by document rowid: the term directory
// is binary-searched once per segment, and the per-segment posting streams are
// merged so the caller still receives one ascending sequence of rowids.

// segmentRef is one segment of the pinned generation's set.
type segmentRef struct {
	id        int64
	termCount int64
	// docCount is how many documents the segment packed, which is also how
	// many entries its document directory carries -- the range a hydration
	// binary-searches.
	docCount int64
	// hidden is how many of the segment's documents this generation does not
	// carry. Zero -- the common case, and the only case for a segment the
	// generation's own units folded -- is what lets a document frequency
	// answer from the term directory instead of walking the posting list.
	hidden int64
}

// lexicalMeta is the pinned generation's lexical shape, resolved once per
// reader: what segments it reads, how many documents and tokens it carries,
// and which documents are visible in it. It is immutable once built and shared
// by every read of that reader.
type lexicalMeta struct {
	docCount   int64
	tokenTotal int64
	segments   []segmentRef
	// visible is the generation's visible documents, as activation stored it.
	// A document an inherited segment still holds but whose unit left the
	// generation is hidden here; nothing is rewritten to hide it.
	visible *docBitmap
}

// lexPartsLive bounds the parts the WHOLE lexical part cache holds at once. It
// is an INTERNAL working-set constant, not a user limit: a read whose working
// set is wider re-reads parts from the store, and nothing is ever refused,
// truncated or answered differently because of it.
//
// Derivation: the deepest simultaneous need of one read is one part of each of
// the five lexical streams for the three segments a term lookup and the
// hydration that follows it alternate between, plus the one part a value
// straddling a part boundary continues into. The bound is over the cache and
// the victim is a whole slot, so a generation naming eighty segments holds
// what one naming four holds -- lexPartsLive * lexPartBytes, 16 MiB at the
// shipped part size. A bound applied per slot instead held
// segments * streams * lexPartsLive parts and so grew without limit with the
// segment count, which is the one thing ADR-0007's stated cost -- one part
// plus one term's list -- does not allow.
const lexPartsLive = 5*3 + 1

// lexicalIndex is one posting session's bounded view of that shape: the shared
// metadata plus a part cache of its own. It holds no vocabulary -- the cache is
// at most lexPartsLive parts across every stream of every segment -- and the
// cache belongs to the caller that opened it, so two concurrent reads never
// share an entry.
type lexicalIndex struct {
	r *PinnedReader
	*lexicalMeta
	cache map[partSlot]*partWindow
	// slots is the cache's eviction order, least recently used first. A victim
	// is a whole slot: a read that has moved on from a segment's stream has
	// moved on from every part of it, and freeing one part of a slot nobody
	// will touch again only defers the next eviction.
	slots []partSlot
	// live is how many parts the cache holds and peak the most it ever held,
	// which is what says the bound is over the cache rather than the slot.
	live, peak int
	// docLo is where the next hydration starts its search in each segment's
	// document directory. A candidate walk delivers rowids in ascending order
	// for the whole query, so the searched range shrinks as the walk advances
	// instead of covering the segment again for every page.
	docLo map[int64]int64
	// docLast is the rowid the last hydration answered, so a caller that goes
	// backwards is served from the whole directory again rather than from a
	// range that no longer contains its document.
	docLast int64
}

// partSlot names one stream of one segment. The window MUST be keyed by the
// segment as well as the stream: two segments' `post.list` streams are
// different bytes at the same offsets, and a window keyed by the stream alone
// would serve one segment's parts against another's directory.
type partSlot struct {
	segment int64
	stream  string
}

// generationLexicalQuery reads the pinned generation's segment set. There is
// deliberately no ORDER BY: generation_segments is a WITHOUT ROWID table keyed
// by (generation_id, ord), so a scan of its primary key already delivers the
// generation's segments in read order.
const generationLexicalQuery = `SELECT gs.segment_id, s.term_count, s.doc_count, gs.hidden
	FROM generation_segments gs JOIN lexical_segments s ON s.id = gs.segment_id
	WHERE gs.generation_id = ?1`

// lexicalMeta resolves the reader's lexical shape, once. A generation with no
// generation_lexical row was never published with one, which is a corrupt store
// rather than an empty vocabulary: the row is written last, so its absence
// means the activation did not finish.
func (r *PinnedReader) lexical(ctx context.Context) (*lexicalMeta, error) {
	r.lexMu.Lock()
	defer r.lexMu.Unlock()
	if r.lexMeta != nil {
		return r.lexMeta, nil
	}
	m := &lexicalMeta{}
	err := r.s.read(ctx, func(tx *sql.Tx) error {
		var bits []byte
		err := tx.QueryRowContext(ctx, `SELECT doc_count, token_total, visible
			FROM generation_lexical WHERE generation_id = ?`, r.gen).Scan(&m.docCount, &m.tokenTotal, &bits)
		if isNoRows(err) {
			return corrupt("generation %d has no packed term statistics; it was published without them", r.gen)
		}
		if err != nil {
			return wrap("generation_lexical", err)
		}
		m.visible = &docBitmap{bits: bits, max: int64(len(bits))*8 - 1}
		rows, err := tx.QueryContext(ctx, generationLexicalQuery, r.gen)
		if err != nil {
			return wrap("generation_segments", err)
		}
		defer rows.Close()
		for rows.Next() {
			var s segmentRef
			if err := rows.Scan(&s.id, &s.termCount, &s.docCount, &s.hidden); err != nil {
				return wrap("generation_segments", err)
			}
			m.segments = append(m.segments, s)
		}
		return wrap("generation_segments", rows.Err())
	})
	if err != nil {
		return nil, err
	}
	r.lexMeta = m
	return m, nil
}

// openLexical opens the packed term statistics of the reader's pinned
// generation for one call: the reader's resolved shape and a part window of
// this call's own.
func (r *PinnedReader) openLexical(ctx context.Context) (*lexicalIndex, error) {
	m, err := r.lexical(ctx)
	if err != nil {
		return nil, err
	}
	return &lexicalIndex{r: r, lexicalMeta: m, cache: map[partSlot]*partWindow{}, docLo: map[int64]int64{}}, nil
}

// part returns one part of a segment's stream through the request's bounded
// window, reading it from the store on a miss and evicting the least recently
// used part when the window is full.
func (x *lexicalIndex) part(ctx context.Context, segment int64, stream string, index int) ([]byte, error) {
	slot := partSlot{segment: segment, stream: stream}
	w := x.cache[slot]
	if w == nil {
		w = &partWindow{size: partSizeFor(stream), limit: lexPartsLive, parts: map[int][]byte{}}
		x.cache[slot] = w
	}
	// Touched before anything can be evicted, so the slot being read is the
	// most recent one and is never its own victim.
	x.touch(slot)
	if b, ok := w.parts[index]; ok {
		if i := slices.Index(w.order, index); i >= 0 {
			w.order = append(slices.Delete(w.order, i, i+1), index)
		}
		return b, nil
	}
	var raw []byte
	err := x.r.s.read(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, `SELECT bytes FROM lexical_segment_parts
			WHERE segment_id = ? AND stream = ? AND part = ?`, segment, stream, index).Scan(&raw)
		if isNoRows(err) {
			return corrupt("packed lexical stream %q of segment %d is missing part %d", stream, segment, index)
		}
		return wrap("lexical_segment_parts", err)
	})
	if err != nil {
		return nil, err
	}
	for len(w.order) >= w.limit {
		delete(w.parts, w.order[0])
		w.order = w.order[1:]
		x.live--
	}
	w.parts[index] = raw
	w.order = append(w.order, index)
	x.live++
	x.evict()
	if x.live > x.peak {
		x.peak = x.live
	}
	return raw, nil
}

// touch makes slot the most recently used one.
func (x *lexicalIndex) touch(slot partSlot) {
	if i := slices.Index(x.slots, slot); i >= 0 {
		x.slots = append(slices.Delete(x.slots, i, i+1), slot)
		return
	}
	x.slots = append(x.slots, slot)
}

// evict drops whole slots, least recently used first, until the cache is
// within its bound. The slot being read is the most recent one and the last
// slot is never dropped, so a read whose own stream is wider than the bound
// keeps what it is reading and pays a re-read for what it is not.
func (x *lexicalIndex) evict() {
	for x.live > lexPartsLive && len(x.slots) > 1 {
		victim := x.slots[0]
		x.slots = x.slots[1:]
		x.live -= len(x.cache[victim].parts)
		delete(x.cache, victim)
	}
}

// readAt copies n bytes from a segment's stream starting at off, stitching
// across the part boundary a value or a list may straddle. Parts are
// fixed-size except the last, which is what makes the part index pure
// arithmetic.
func (x *lexicalIndex) readAt(ctx context.Context, segment int64, stream string, off int64, n int) ([]byte, error) {
	size := int64(partSizeFor(stream))
	out := make([]byte, 0, n)
	for len(out) < n {
		index := int(off / size)
		part, err := x.part(ctx, segment, stream, index)
		if err != nil {
			return nil, err
		}
		begin := int(off % size)
		if begin >= len(part) {
			return nil, corrupt("packed lexical stream %q of segment %d ends before offset %d", stream, segment, off)
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

func (x *lexicalIndex) entryAt(ctx context.Context, segment, index int64) (termEntry, error) {
	raw, err := x.readAt(ctx, segment, streamTermDir, index*termEntryBytes, termEntryBytes)
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

// lookup finds a term's entry in one segment's directory by binary search over
// the fixed-width directory, which the fold wrote in term order. A term the
// segment does not carry answers ok=false, not an error: a word nobody in that
// segment indexed is an empty list, and another segment may still hold it.
func (x *lexicalIndex) lookup(ctx context.Context, seg segmentRef, term string) (termEntry, bool, error) {
	lo, hi := int64(0), seg.termCount
	want := []byte(term)
	for lo < hi {
		mid := lo + (hi-lo)/2
		e, err := x.entryAt(ctx, seg.id, mid)
		if err != nil {
			return termEntry{}, false, err
		}
		text, err := x.readAt(ctx, seg.id, streamTermText, e.textOff, e.textLen)
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

// postingList reads one term's whole posting list out of one segment. One
// term's list per segment is the whole heap cost of a read, which is the bound
// ADR-0007 states: a part plus a list, never the vocabulary.
func (x *lexicalIndex) postingList(ctx context.Context, seg segmentRef, e termEntry) ([]byte, error) {
	return x.readAt(ctx, seg.id, streamPostList, e.listOff, e.listLen)
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
// contain it. It is EXACT: a segment every one of whose units the generation
// carries answers from its directory entry, and a segment holding documents
// the generation hides is counted by walking that term's posting list against
// the visible bitmap. An approximate frequency that counted hidden documents
// would make a score depend on the store's history, so two stores holding the
// same code would rank the same query differently.
//
// It bounds the term count itself at nothing: a term is one binary search per
// segment, which reads one fixed-width entry and one term text per probe and
// frees both. How many terms one request may carry is the search tier's
// decision, under resources.max_query_terms.
func (r *PinnedReader) DocumentFrequency(ctx context.Context, terms []string) ([]int64, error) {
	if len(terms) == 0 {
		return nil, invalid("document frequency needs at least one term")
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
		for _, seg := range x.segments {
			e, ok, err := x.lookup(ctx, seg, t)
			if err != nil {
				return nil, err
			}
			if !ok {
				continue
			}
			if seg.hidden == 0 {
				out[i] += e.df
				continue
			}
			raw, err := x.postingList(ctx, seg, e)
			if err != nil {
				return nil, err
			}
			n, err := countVisible(raw, x.visible, t, seg.id)
			if err != nil {
				return nil, err
			}
			out[i] += n
		}
	}
	return out, nil
}

// countVisible walks a posting list and counts the documents the generation
// still carries. It decodes the document deltas and skips each document's
// groups without materializing them.
func countVisible(raw []byte, visible *docBitmap, term string, segment int64) (int64, error) {
	c := postingCursor{raw: raw, term: term, segment: segment}
	var n int64
	for {
		ok, err := c.next()
		if err != nil {
			return 0, err
		}
		if !ok {
			return n, nil
		}
		if visible.has(c.doc) {
			n++
		}
	}
}

// TermCounts opens a stream over one single-token term's packed posting lists:
// one TermOccurrence per (document, column), documents ascending across the
// whole generation and a document's columns in column-name order, carrying the
// counts the scorer sums and no offsets. A phrase needs offsets and uses
// TermOccurrences instead.
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
	s := &PackedStream{term: term, gen: p.r.gen, visible: p.lex.visible}
	for _, seg := range p.lex.segments {
		e, ok, err := p.lex.lookup(ctx, seg, term)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		raw, err := p.lex.postingList(ctx, seg, e)
		if err != nil {
			return nil, err
		}
		c := &postingCursor{raw: raw, term: term, segment: seg.id}
		live, err := c.next()
		if err != nil {
			return nil, err
		}
		if live {
			s.cursors = append(s.cursors, c)
		}
	}
	return s, nil
}

// postingCursor decodes one segment's posting list for one term, one document
// at a time. It holds the current document's (column, count) groups and
// nothing else.
type postingCursor struct {
	raw     []byte
	term    string
	segment int64
	doc     int64
	groups  []TermOccurrence
}

// next advances to the next document of the list, decoding its groups. It
// answers false when the list is exhausted.
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
		c.groups = append(c.groups, TermOccurrence{RowID: c.doc, Column: lexicalColumns[code-1], Count: int64(count)})
	}
	return true, nil
}

func (c *postingCursor) malformed() error {
	return corrupt("packed lexical posting list of term %q in segment %d is malformed", c.term, c.segment)
}

// PackedStream merges one term's posting lists across the generation's
// segments by document rowid, in page-sized refills. A refill always ends on a
// document boundary, so a document's (column, count) groups are never split
// across two refills, and the rowids it delivers strictly ascend -- the order
// the candidate walk requires.
type PackedStream struct {
	term    string
	gen     int64
	visible *docBitmap
	cursors []*postingCursor
	last    int64
	started bool
}

// Close releases the stream. It holds no statement; the method exists so a
// packed stream and a live one are the same contract to the caller.
func (s *PackedStream) Close() error {
	s.cursors = nil
	return nil
}

// Next returns the next refill: the grouped occurrences of whole documents, at
// least limit rows unless the lists are exhausted. A nil result with a nil
// error means they are exhausted.
func (s *PackedStream) Next(ctx context.Context, limit int) ([]TermOccurrence, error) {
	limit = pageLimit(ctx, limit)
	var out []TermOccurrence
	for len(s.cursors) > 0 && len(out) < limit {
		// Checked every iteration, not once: a document the generation hides
		// advances a cursor and emits nothing, so one refill can walk a whole
		// posting list without ever filling its page.
		if err := ctx.Err(); err != nil {
			return nil, model.Canceled(err)
		}
		// The smallest rowid any segment still offers. Every visible document
		// of a generation lies in exactly ONE of its segments, so two cursors
		// offering the same document is a structure that would deliver that
		// document twice and double its contribution to the score.
		pick := 0
		for i := 1; i < len(s.cursors); i++ {
			if s.cursors[i].doc < s.cursors[pick].doc {
				pick = i
			}
		}
		for i := range s.cursors {
			if i != pick && s.cursors[i].doc == s.cursors[pick].doc {
				return nil, corrupt("generation %d holds document %d in segments %d and %d",
					s.gen, s.cursors[pick].doc, s.cursors[pick].segment, s.cursors[i].segment)
			}
		}
		c := s.cursors[pick]
		if s.started && c.doc <= s.last {
			return nil, corrupt("packed lexical posting list of term %q in segment %d leaves document %d after %d",
				s.term, c.segment, c.doc, s.last)
		}
		// A document an inherited segment still holds but whose unit left the
		// generation is skipped here; nothing was rewritten to hide it.
		if s.visible.has(c.doc) {
			out = append(out, c.groups...)
			s.last, s.started = c.doc, true
		}
		live, err := c.next()
		if err != nil {
			return nil, err
		}
		if !live {
			s.cursors = slices.Delete(s.cursors, pick, pick+1)
		}
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// documentEntryAt decodes one entry of a segment's document directory: the
// document it names and the slice of the attribute stream holding its record.
func (x *lexicalIndex) documentEntryAt(ctx context.Context, segment, index int64) (doc, off int64, length int, err error) {
	raw, err := x.readAt(ctx, segment, streamDocDir, index*docEntryBytes, docEntryBytes)
	if err != nil {
		return 0, 0, 0, err
	}
	return int64(binary.LittleEndian.Uint64(raw[docEntryRowID:])),
		int64(binary.LittleEndian.Uint64(raw[docEntryAttrOff:])),
		int(binary.LittleEndian.Uint32(raw[docEntryAttrLen:])), nil
}

// findDocument binary-searches one segment's document directory for a rowid,
// starting at lo -- where the walk left this segment -- and answers the entry's
// ordinal. A segment that does not hold the document answers ok=false: every
// visible document lies in exactly one segment of the generation, so the other
// segments are simply asked next.
func (x *lexicalIndex) findDocument(ctx context.Context, seg segmentRef, rowid, lo int64) (index, off int64, length int, ok bool, err error) {
	hi := seg.docCount
	for lo < hi {
		mid := lo + (hi-lo)/2
		doc, at, n, err := x.documentEntryAt(ctx, seg.id, mid)
		if err != nil {
			return 0, 0, 0, false, err
		}
		switch {
		case doc == rowid:
			return mid, at, n, true, nil
		case doc < rowid:
			lo = mid + 1
		default:
			hi = mid
		}
	}
	return lo, 0, 0, false, nil
}

// PackedDocuments hydrates a page of candidates from the segments' packed
// per-document attribute streams (ADR-0007 Decision 2). It issues NO statement
// per candidate: the fields are read out of the same parts the query is already
// reading, through the session's window, so a page of 200 hits costs the parts
// its documents lie in rather than 200 index descents into the document table.
//
// The answer is in the requested order, and a rowid the generation does not
// make visible is omitted rather than zeroed -- a document an inherited segment
// still holds but whose unit left the generation is not a document of this
// generation at all.
func (p *PostingSession) PackedDocuments(ctx context.Context, rowids []int64) ([]SearchDocument, error) {
	if len(rowids) == 0 {
		return nil, nil
	}
	if len(rowids) > model.MaxPageItems {
		return nil, invalid("search document hydration asked for %d rowids, limit %d", len(rowids), model.MaxPageItems)
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
	x := p.lex
	out := make([]SearchDocument, 0, len(rowids))
	for _, id := range rowids {
		if id <= 0 {
			return nil, invalid("search document rowid %d is not positive", id)
		}
		if id < x.docLast {
			clear(x.docLo)
		}
		x.docLast = id
		if !x.visible.has(id) {
			continue
		}
		d, ok, err := x.document(ctx, id)
		if err != nil {
			return nil, err
		}
		// A visible document the generation's segments do not hold is a
		// structure no query could serve: the candidate would be scored and
		// then vanish from the page it was ranked into.
		if !ok {
			return nil, corrupt("generation %d makes document %d visible but no segment of it packs the document's attributes",
				p.r.gen, id)
		}
		out = append(out, d)
	}
	return out, nil
}

// document reads one visible document's attributes out of whichever segment
// holds it, advancing that segment's search bound past the entry it found.
func (x *lexicalIndex) document(ctx context.Context, rowid int64) (SearchDocument, bool, error) {
	for _, seg := range x.segments {
		index, off, length, ok, err := x.findDocument(ctx, seg, rowid, x.docLo[seg.id])
		if err != nil {
			return SearchDocument{}, false, err
		}
		if !ok {
			// Where the search stopped is where the next, larger candidate
			// starts: the directory ascends, so nothing before it can hold one.
			x.docLo[seg.id] = index
			continue
		}
		record, err := x.readAt(ctx, seg.id, streamDocAttr, off, length)
		if err != nil {
			return SearchDocument{}, false, err
		}
		x.docLo[seg.id] = index + 1
		d, err := decodeDocument(rowid, record)
		if err != nil {
			return SearchDocument{}, false, err
		}
		return d, true, nil
	}
	return SearchDocument{}, false, nil
}
