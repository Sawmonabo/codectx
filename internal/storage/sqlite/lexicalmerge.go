package sqlite

import (
	"bytes"
	"cmp"
	"context"
	"database/sql"
	"encoding/binary"
	"log/slog"
	"maps"
	"slices"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
)

// Compaction of the packed lexical structure (ADR-0007 Decision 1). A seal
// folds one segment per unit, so without compaction the segment count grows
// with every sealed unit and every query binary-searches all of them. Merging
// them all at each activation is the write amplification the segmented shape
// exists to remove, so segments are merged by GEOMETRIC PARTITIONING instead:
// they are grouped into size tiers by packed bytes, and while a tier holds more
// than the ratio's worth of segments its segments are merged. A document is
// then rewritten O(log_r N) times over its whole life instead of once per
// activation, and a one-file delta pays for its own small segment plus,
// occasionally, one tier merge.
//
// A merge KEEPS a document that the activating generation hides. Hidden means
// only that the unit holding it is not a member of this generation; that unit
// can be attached to a later one at any time, and it must find its postings
// where its rows point. Only DEAD documents are dropped: rowids no
// search_units row names any more, because the unit that held them was
// collected. The merge re-points every row of every input -- the members' and
// the retired units' alike -- at the merged segment, so after it no row names
// an input and a segment can never be named beside the one that absorbed it.

// The compaction layout. These are INTERNAL layout constants, not user limits:
// no count of terms, documents, units or segments is ever refused because of
// them, and no activation is deferred by them. They decide how often
// already-packed bytes are rewritten and how much of a merge is resident at
// once, which is the only thing they bound.
const (
	// lexTierBytes is the top of the smallest size tier. A segment smaller
	// than it is in tier 0; each further tier is lexMergeRatioDefault times as wide,
	// so a tier's segments are within one ratio of each other in size and a
	// merge never folds a large segment into a small one.
	lexTierBytes = 4 << 20

	// lexMergeFanIn is how many segments one merge reads at once. It is what
	// bounds the merge's heap: one part of each of a segment's five streams per
	// input, plus the writer's, so (lexMergeFanIn + 1) * 5 * lexPartBytes
	// whatever the size of the tier. A tier holding more than this is merged by
	// repeated merges of that many segments, not by one merge of all of them:
	// a first index of a large repository seals tens of thousands of segments
	// into one tier, and reading them all at once would be gigabytes of buffers
	// at the moment that publishes facts to every reader.
	lexMergeFanIn = lexMergeRatioDefault + 1

	// lexMergeRatioDefault is the ratio r the partitioning ships with.
	lexMergeRatioDefault = 4
)

// lexMergeRatio is the ratio r of the geometric partitioning: a tier holding
// more than r segments is merged. It is a var only so a test can shrink it
// (export_test.go) and drive a merge on a fixture whose segments are kilobytes;
// production never writes to it.
var lexMergeRatio = lexMergeRatioDefault

// lexicalTier is the size tier a segment of the given packed size belongs to:
// tier 0 below lexTierBytes, and each tier above it lexMergeRatioDefault times
// as wide. The tier boundaries do not move with the test ratio -- a tier is a
// property of the stored bytes, not of how eagerly they are merged.
func lexicalTier(size int64) int {
	tier := 0
	for limit := int64(lexTierBytes); size >= limit; limit *= lexMergeRatioDefault {
		tier++
	}
	return tier
}

// segmentStat is what compaction needs to know about one segment: how many
// documents it packed, how many bytes it occupies, and how many of its
// documents still have a row pointing at it. docs minus alive is the segment's
// DEAD documents -- the ones whose unit was collected -- which a merge drops.
type segmentStat struct {
	docs  int64
	bytes int64
	alive int64
}

func (s segmentStat) dead() int64 { return s.docs - s.alive }

// segmentStats reads what compaction needs to know about each of the
// generation's segments: one point lookup per segment on the segments' primary
// key -- the set is a function of the generation's documents, never of its
// size -- and one index-only walk of the documents that still point at it. The
// packed count is what turns a per-segment live count into the hidden count a
// document frequency uses to skip the posting walk; the alive count is what
// decides a rewrite.
func segmentStats(ctx context.Context, tx *sql.Tx, ids []int64) (map[int64]segmentStat, error) {
	out := make(map[int64]segmentStat, len(ids))
	for _, id := range ids {
		var st segmentStat
		err := tx.QueryRowContext(ctx, `SELECT doc_count, bytes FROM lexical_segments WHERE id = ?`, id).
			Scan(&st.docs, &st.bytes)
		if isNoRows(err) {
			return nil, corrupt("generation names lexical segment %d, which does not exist", id)
		}
		if err != nil {
			return nil, wrap("lexical_segments", err)
		}
		if st.alive, err = aliveDocuments(ctx, tx, []int64{id}, nil); err != nil {
			return nil, err
		}
		out[id] = st
	}
	return out, nil
}

// compactBeforeActivation runs the staging generation's due merges before the
// call that publishes it, and reports what they cost. The pass is logged on
// its own, as the activation's own build is, because a cost hidden inside an
// activation's total cannot be told from the publication itself.
func (s *Store) compactBeforeActivation(ctx context.Context, gen model.GenerationID) error {
	var row int64
	if err := s.ingest(ctx, func(tx *sql.Tx) error {
		g, err := s.generationRow(ctx, tx, gen, model.GenerationStaging)
		if err != nil {
			return err
		}
		row = g.id
		return nil
	}); err != nil {
		return err
	}
	started := time.Now()
	merges, err := s.compactGeneration(ctx, row)
	if merges > 0 || err != nil {
		slog.Default().Info("packed lexical compaction finished", "generation", gen,
			"duration_ms", time.Since(started).Milliseconds(), "merges", merges)
	}
	return err
}

// compactGeneration merges the generation's segment set down to what the
// geometric partitioning allows, each merge its own ingestion call, before the
// call that activates the generation.
//
// Each merge is its own call for one reason: the group's commit decision fires
// only at the end of an ingestion call, so a cascade run inside the activation
// would have no commit point and nothing in the group's accounting would bound
// its log. A first index seals one segment per unit, so its cascade is
// (N-4)/lexMergeRatio merges -- thousands of them for a large repository -- and
// WALBoundBytes, which is stated as the largest log an ingestion group leaves
// behind, would not be a bound on it.
//
// Committing a merge before the swap is safe because a merged segment is data
// nothing published references yet. The merge writes a new immutable segment
// and re-points the rows that named its inputs; the generations already
// published still name the inputs in generation_segments, and the collector
// drops a segment only when NO generation names it and NO row points at it, so
// neither the inputs a live generation still reads nor the merged segment a row
// now points at can be collected. If the activation never happens, the merged
// segment is simply the shape the next one will publish.
//
// The activation itself stays atomic: it is one call, which resolves the
// segment set from the rows the merges left and flips the pointer.
func (s *Store) compactGeneration(ctx context.Context, gen int64) (int64, error) {
	var (
		ids   []int64
		live  map[int64]int64
		stats map[int64]segmentStat
	)
	if err := s.ingest(ctx, func(tx *sql.Tx) error {
		var err error
		if _, _, _, live, err = visibleDocuments(ctx, tx, gen); err != nil {
			return err
		}
		ids = slices.Sorted(maps.Keys(live))
		stats, err = segmentStats(ctx, tx, ids)
		return err
	}); err != nil {
		return 0, err
	}
	var merges int64
	for {
		if err := ctx.Err(); err != nil {
			return merges, model.Canceled(err)
		}
		inputs := dueMerge(ids, stats)
		if inputs == nil {
			return merges, nil
		}
		if err := s.ingest(ctx, func(tx *sql.Tx) error {
			var err error
			ids, err = applyMerge(ctx, tx, inputs, ids, live, stats)
			return err
		}); err != nil {
			return merges, err
		}
		merges++
	}
}

// applyMerge folds inputs into one segment and returns the set that replaces
// them, in segment-id order. live and stats are updated in place so the
// caller's next decision -- and the activation's coverage check and hidden
// counts -- are about the set that is actually recorded.
//
// The cascade terminates: a tier merge reads more than one segment and writes
// one, so it strictly reduces the set's size; a dead-fraction rewrite reads one
// segment and writes one whose dead count is zero, so it strictly reduces the
// set's total dead documents and never grows the set. Neither can undo the
// other's progress, and both quantities are finite.
func applyMerge(ctx context.Context, tx *sql.Tx, inputs, ids []int64, live map[int64]int64,
	stats map[int64]segmentStat) ([]int64, error) {
	merged, kept, size, err := mergeSegments(ctx, tx, inputs, stats)
	if err != nil {
		return nil, err
	}
	var carried int64
	for _, in := range inputs {
		carried += live[in]
		delete(live, in)
		delete(stats, in)
	}
	live[merged] = carried
	// The merged segment holds exactly the documents the merge kept, and
	// every one of them has a row pointing at it: the re-point ran over the
	// same rows the keep set was read from, in the same transaction.
	stats[merged] = segmentStat{docs: kept, bytes: size, alive: kept}
	ids = slices.DeleteFunc(ids, func(id int64) bool { return slices.Contains(inputs, id) })
	ids = append(ids, merged)
	slices.Sort(ids)
	return ids, nil
}

// dueMerge picks the segments of one merge, or nil when the set is already
// what the partitioning allows. A segment more than half of whose documents
// are dead is rewritten alone first: its postings still carry every collected
// document's entry, and a query walks past them on every term they appear in.
// Otherwise the lowest tier holding more than the ratio's worth of segments
// gives up its smallest lexMergeFanIn segments -- the smallest, so the bytes a
// merge rewrites are the cheapest ones available at that tier.
func dueMerge(ids []int64, stats map[int64]segmentStat) []int64 {
	for _, id := range ids {
		if s := stats[id]; s.dead()*2 > s.docs {
			return []int64{id}
		}
	}
	tiers := map[int][]int64{}
	for _, id := range ids {
		t := lexicalTier(stats[id].bytes)
		tiers[t] = append(tiers[t], id)
	}
	for _, t := range slices.Sorted(maps.Keys(tiers)) {
		tier := tiers[t]
		if len(tier) <= lexMergeRatio {
			continue
		}
		slices.SortFunc(tier, func(a, b int64) int {
			if c := cmp.Compare(stats[a].bytes, stats[b].bytes); c != 0 {
				return c
			}
			return cmp.Compare(a, b)
		})
		return tier[:min(len(tier), lexMergeFanIn)]
	}
	return nil
}

// mergeSegments folds the inputs into one new segment and re-points every row
// that named an input at it. It returns the new segment and how many documents
// it holds.
//
// Nothing of a segment is held whole: the inputs' streams are read
// sequentially, one part of each resident at a time, and the output is written
// through the same part writer a seal uses, inside the activation's own
// transaction and therefore under the ingestion group's write discipline
// (ADR-0008).
func mergeSegments(ctx context.Context, tx *sql.Tx, inputs []int64,
	stats map[int64]segmentStat) (segment, docs, size int64, err error) {
	keep, err := newDocumentBitmap(ctx, tx)
	if err != nil {
		return 0, 0, 0, err
	}
	kept, err := aliveDocuments(ctx, tx, inputs, keep)
	if err != nil {
		return 0, 0, 0, err
	}
	if kept == 0 {
		return 0, 0, 0, internal("packed lexical merge was asked to fold segments no document points at")
	}
	// The row is inserted FIRST so the engine assigns its id and the parts can
	// be keyed by it as they stream, exactly as a seal's fold does.
	res, err := tx.ExecContext(ctx, `INSERT INTO lexical_segments(term_count, doc_count, bytes) VALUES(0, ?, 0)`, kept)
	if err != nil {
		return 0, 0, 0, wrap("lexical_segments", err)
	}
	merged, err := res.LastInsertId()
	if err != nil {
		return 0, 0, 0, wrap("lexical_segments", err)
	}

	scans := make([]*segmentScan, 0, len(inputs))
	for _, id := range inputs {
		sc, err := openSegmentScan(ctx, tx, id)
		if err != nil {
			return 0, 0, 0, err
		}
		if err := sc.next(); err != nil {
			return 0, 0, 0, err
		}
		scans = append(scans, sc)
	}
	w := &lexicalWriter{
		dir:  newPartWriter(ctx, tx, segmentKey(merged), streamTermDir),
		text: newPartWriter(ctx, tx, segmentKey(merged), streamTermText),
		list: newPartWriter(ctx, tx, segmentKey(merged), streamPostList),
	}
	var term []byte
	holders := make([]*segmentScan, 0, len(scans))
	for {
		if err := ctx.Err(); err != nil {
			return 0, 0, 0, model.Canceled(err)
		}
		// The smallest term any input still offers, copied out of its reader
		// before anything advances: the readers' buffers are one part each and
		// an advance overwrites them.
		var lowest []byte
		for _, sc := range scans {
			if sc.live && (lowest == nil || bytes.Compare(sc.term, lowest) < 0) {
				lowest = sc.term
			}
		}
		if lowest == nil {
			break
		}
		term = append(term[:0], lowest...)
		holders = holders[:0]
		for _, sc := range scans {
			if sc.live && bytes.Equal(sc.term, term) {
				holders = append(holders, sc)
			}
		}
		// A term only one input carries is copied VERBATIM -- its posting bytes
		// go to the merged segment unread -- but only when that input has no
		// dead document at all. The weaker test, "no dead document in THIS
		// list", is not used on purpose: answering it means decoding the list,
		// which is the work the verbatim path exists to avoid.
		if len(holders) == 1 && stats[holders[0].id].dead() == 0 {
			if err := w.copyTerm(term, holders[0].list, holders[0].df); err != nil {
				return 0, 0, 0, err
			}
		} else if err := mergeTerm(w, term, holders, keep); err != nil {
			return 0, 0, 0, err
		}
		for _, sc := range holders {
			if err := sc.next(); err != nil {
				return 0, 0, 0, err
			}
		}
	}
	if err := w.close(); err != nil {
		return 0, 0, 0, err
	}
	// The merged segment carries the attributes of the documents it kept, each
	// record copied verbatim from the input that packed it, so a candidate is
	// hydrated from the merged bytes exactly as it was from the input's.
	packedDocs, docBytes, err := mergeDocuments(ctx, tx, inputs, stats, keep, merged)
	if err != nil {
		return 0, 0, 0, err
	}
	// A row pointing at an input whose directory does not hold its document
	// would leave a candidate the merged segment cannot hydrate, and the
	// re-point below would make that permanent.
	if packedDocs != kept {
		return 0, 0, 0, corrupt("packed lexical merge kept %d documents but packed the attributes of %d",
			kept, packedDocs)
	}
	packed := w.dir.total + w.text.total + w.list.total + docBytes
	if _, err := tx.ExecContext(ctx, `UPDATE lexical_segments SET term_count = ?, bytes = ? WHERE id = ?`,
		w.terms, packed, merged); err != nil {
		return 0, 0, 0, wrap("lexical_segments", err)
	}
	// Every row of every input is re-pointed, the members' and the retired
	// units' alike. This is what makes the merged segment the only place its
	// documents are found: an input is named by no row afterwards, so a later
	// generation cannot name it beside the segment that absorbed it, and a unit
	// attached again later finds its postings where its own rows point.
	for _, id := range inputs {
		if _, err := tx.ExecContext(ctx, repointSegmentStatement, merged, id); err != nil {
			return 0, 0, 0, wrap("search_units", err)
		}
	}
	return merged, kept, packed, nil
}

// repointSegmentStatement moves every document of one absorbed segment to the
// segment that absorbed it. idx_search_segment makes it a search of the rows
// that name the input rather than a scan of every document in the store.
const repointSegmentStatement = `UPDATE search_units SET segment_id = ?1 WHERE segment_id = ?2`

// mergeTerm writes one term whose documents come from several inputs, or from
// one input that still carries dead documents. The inputs' posting lists are
// merged by document rowid so the merged list still ascends, and a document no
// row points at any more is dropped: it is DEAD, not hidden, and no unit will
// ever ask for it again. A term all of whose documents are dead is written
// nowhere -- the writer is never told about it, so it simply leaves the
// merged segment's vocabulary.
func mergeTerm(w *lexicalWriter, term []byte, holders []*segmentScan, keep *docBitmap) error {
	cursors := make([]*postingCursor, 0, len(holders))
	for _, sc := range holders {
		c := &postingCursor{raw: sc.list, term: string(term), segment: sc.id}
		live, err := c.next()
		if err != nil {
			return err
		}
		if live {
			cursors = append(cursors, c)
		}
	}
	for len(cursors) > 0 {
		pick := 0
		for i := 1; i < len(cursors); i++ {
			if cursors[i].doc < cursors[pick].doc {
				pick = i
			}
		}
		c := cursors[pick]
		if keep.has(c.doc) {
			for _, g := range c.groups {
				if err := w.add(term, c.doc, lexicalColumnCode(g.Column), g.Count); err != nil {
					return err
				}
			}
		}
		live, err := c.next()
		if err != nil {
			return err
		}
		if !live {
			cursors = slices.Delete(cursors, pick, pick+1)
		}
	}
	return nil
}

// segmentDocumentQuery walks the documents that still point at one segment. It
// is an index-only walk of idx_search_segment, which delivers them in ascending
// document order within the segment, so the distinct count below needs neither
// DISTINCT nor an ORDER BY -- both would materialize the segment's documents in
// a temporary b-tree.
const segmentDocumentQuery = `SELECT doc_id FROM search_units WHERE segment_id = ?1`

// aliveDocuments counts the distinct documents of the given segments that still
// have a row pointing at them and, when bits is not nil, marks each of them
// there. A document with no row left is DEAD: the unit holding it was
// collected, nothing can ever attach it again, and a merge drops it. A document
// may be named by several rows -- a carry chain shares one document across its
// units -- which is why the count is of distinct documents, the thing a
// segment's doc_count means. It is the ONE place that count is derived, so the
// rewrite trigger and the merged segment's doc_count can never disagree.
func aliveDocuments(ctx context.Context, tx *sql.Tx, ids []int64, bits *docBitmap) (int64, error) {
	var n int64
	for _, id := range ids {
		rows, err := tx.QueryContext(ctx, segmentDocumentQuery, id)
		if err != nil {
			return 0, wrap("search_units", err)
		}
		last, have := int64(0), false
		for rows.Next() {
			var doc int64
			if err := rows.Scan(&doc); err != nil {
				rows.Close()
				return 0, wrap("search_units", err)
			}
			if have && doc < last {
				rows.Close()
				return 0, corrupt("search_units left document %d after %d in segment %d; the walk is not ordered",
					doc, last, id)
			}
			if !have || doc != last {
				n++
				if bits != nil {
					if doc < 0 || doc > bits.max {
						rows.Close()
						return 0, corrupt("search_units names document %d, past the largest document %d",
							doc, bits.max)
					}
					bits.set(doc)
				}
			}
			last, have = doc, true
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return 0, wrap("search_units", err)
		}
	}
	return n, nil
}

// newDocumentBitmap sizes a bitmap over every document the store holds, so a
// merge can mark the documents its inputs keep. It is a bit per document rowid
// -- a byte per eight documents -- which is the same bound the generation's
// visible bitmap already carries.
func newDocumentBitmap(ctx context.Context, tx *sql.Tx) (*docBitmap, error) {
	var maxDoc int64
	if err := tx.QueryRowContext(ctx, `SELECT coalesce(max(doc_id), 0) FROM search_units`).Scan(&maxDoc); err != nil {
		return nil, wrap("search_units", err)
	}
	return &docBitmap{bits: make([]byte, maxDoc/8+1), max: maxDoc}, nil
}

// segmentScan reads one segment's vocabulary in term order: the fixed-width
// directory and, with it, the term text and the posting bytes each entry
// points at. All three streams were written front to back in the same term
// order, so the scan is sequential and one part of each is resident at a time.
type segmentScan struct {
	id                 int64
	dir, text, postSrc *streamReader
	entries            int64
	index              int64
	textOff, listOff   int64

	// term and list are the current entry's bytes. They alias the readers'
	// resident parts and stay valid until the next call to next.
	term []byte
	list []byte
	df   int64
	live bool
}

func openSegmentScan(ctx context.Context, tx *sql.Tx, id int64) (*segmentScan, error) {
	var entries int64
	err := tx.QueryRowContext(ctx, `SELECT term_count FROM lexical_segments WHERE id = ?`, id).Scan(&entries)
	if isNoRows(err) {
		return nil, corrupt("packed lexical merge names segment %d, which does not exist", id)
	}
	if err != nil {
		return nil, wrap("lexical_segments", err)
	}
	return &segmentScan{
		id:      id,
		dir:     newStreamReader(ctx, tx, id, streamTermDir),
		text:    newStreamReader(ctx, tx, id, streamTermText),
		postSrc: newStreamReader(ctx, tx, id, streamPostList),
		entries: entries,
	}, nil
}

// next advances to the segment's next term, or leaves the scan not live when
// its directory is exhausted.
func (s *segmentScan) next() error {
	if s.index >= s.entries {
		s.live, s.term, s.list = false, nil, nil
		return nil
	}
	raw, err := s.dir.read(s.index*termEntryBytes, termEntryBytes)
	if err != nil {
		return err
	}
	textOff := int64(binary.LittleEndian.Uint64(raw[termEntryTextOff:]))
	listOff := int64(binary.LittleEndian.Uint64(raw[termEntryListOff:]))
	textLen := int(binary.LittleEndian.Uint16(raw[termEntryTextLen:]))
	listLen := int(binary.LittleEndian.Uint32(raw[termEntryListLen:]))
	// The three streams are written front to back in one pass, so a term's
	// bytes begin exactly where the previous term's ended. A directory that
	// points anywhere else is a structure this sequential scan would read
	// wrongly without noticing.
	if textOff != s.textOff || listOff != s.listOff {
		return corrupt("packed lexical segment %d term %d points at text %d and postings %d, not %d and %d",
			s.id, s.index, textOff, listOff, s.textOff, s.listOff)
	}
	if s.term, err = s.text.read(textOff, textLen); err != nil {
		return err
	}
	if s.list, err = s.postSrc.read(listOff, listLen); err != nil {
		return err
	}
	s.df = int64(binary.LittleEndian.Uint32(raw[termEntryDF:]))
	s.textOff, s.listOff = textOff+int64(textLen), listOff+int64(listLen)
	s.index++
	s.live = true
	return nil
}

// streamReader reads one segment stream sequentially, holding ONE part. A
// merge reads its inputs front to back, so a part is loaded once and left
// behind; the window a random read needs is the reader's (lexicalread.go), not
// this one's.
type streamReader struct {
	ctx     context.Context
	tx      *sql.Tx
	segment int64
	stream  string
	size    int64
	part    int
	loaded  bool
	buf     []byte
	stitch  []byte
}

func newStreamReader(ctx context.Context, tx *sql.Tx, segment int64, stream string) *streamReader {
	return &streamReader{ctx: ctx, tx: tx, segment: segment, stream: stream, size: int64(partSizeFor(stream))}
}

// read returns n bytes at off. The result aliases the reader's resident part
// unless the range straddles a part boundary, in which case it is stitched into
// a buffer of the reader's own; either way it stays valid until the next read.
func (r *streamReader) read(off int64, n int) ([]byte, error) {
	if n == 0 {
		return nil, nil
	}
	index := int(off / r.size)
	begin := int(off % r.size)
	if err := r.load(index); err != nil {
		return nil, err
	}
	if begin+n <= len(r.buf) {
		return r.buf[begin : begin+n], nil
	}
	r.stitch = append(r.stitch[:0], r.buf[min(begin, len(r.buf)):]...)
	for len(r.stitch) < n {
		index++
		if err := r.load(index); err != nil {
			return nil, err
		}
		take := min(len(r.buf), n-len(r.stitch))
		if take == 0 {
			return nil, corrupt("packed lexical stream %q of segment %d ends before offset %d",
				r.stream, r.segment, off)
		}
		r.stitch = append(r.stitch, r.buf[:take]...)
	}
	return r.stitch, nil
}

func (r *streamReader) load(index int) error {
	if r.loaded && r.part == index {
		return nil
	}
	var raw []byte
	err := r.tx.QueryRowContext(r.ctx, `SELECT bytes FROM lexical_segment_parts
		WHERE segment_id = ? AND stream = ? AND part = ?`, r.segment, r.stream, index).Scan(&raw)
	if isNoRows(err) {
		return corrupt("packed lexical stream %q of segment %d is missing part %d", r.stream, r.segment, index)
	}
	if err != nil {
		return wrap("lexical_segment_parts", err)
	}
	r.buf, r.part, r.loaded = raw, index, true
	return nil
}
