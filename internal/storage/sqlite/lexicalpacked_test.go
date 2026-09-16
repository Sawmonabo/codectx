package sqlite_test

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
	store "github.com/Sawmonabo/codectx/internal/storage/sqlite"
)

// The packed term statistics ARE the scorer's inputs, so a value that drifts
// from the live posting path silently changes every ranked answer without
// changing an ordering rule. This compares, per term, the document frequency,
// the visible-document statistics and the ORDERED per-document
// (column, count) sequence between the packed form and the same statements the
// live path issues over the index tables. One mismatched document is a wrong
// score on a page nobody can tell apart from a right one.
// packedLexicalFixture publishes one generation over two files and returns the
// pinned reader, the fixture and the database path.
func packedLexicalFixture(t *testing.T) (*fixture, *store.PinnedReader, model.GenerationID, string) {
	t.Helper()
	dbPath := t.TempDir() + "/codectx.db"
	f := newFixture(t, dbPath)
	a := f.file("pkg/a.go", "package pkg\nfunc Alpha() { Alpha() }\n")
	b := f.file("pkg/b.go", "package pkg\nfunc Beta() { Alpha() }\n")
	snap := f.snapshot("one", a, b)
	gen, err := f.s.BeginGeneration(f.ctx, f.repo, snap.ID, model.H("semantic"), "main")
	if err != nil {
		t.Fatal(err)
	}
	run := f.run(gen)
	// TWO units, so the generation publishes TWO segments: the structure is
	// segmented per sealed unit, and a one-unit generation would exercise
	// neither the merge that keeps rowids ascending across segments nor the
	// part window that must be keyed by segment as well as by stream.
	first := f.beginScopeKey(gen, run, configHash, "scope-a", a)
	f.fillFile(first, run, a)
	f.fillIndexLevel(first, run, a)
	if err := f.s.SealUnit(f.ctx, first); err != nil {
		t.Fatalf("SealUnit(first): %v", err)
	}
	second := f.beginScopeKey(gen, run, configHash, "scope-b", b)
	f.fillFile(second, run, b)
	if err := f.s.SealUnit(f.ctx, second); err != nil {
		t.Fatalf("SealUnit(second): %v", err)
	}
	f.activate(gen, 0)
	r, err := f.s.PinGeneration(f.ctx, f.repo, gen, time.Minute)
	if err != nil {
		t.Fatalf("PinGeneration: %v", err)
	}
	return f, r, gen, dbPath
}

func TestPackedLexicalMatchesTheLivePath(t *testing.T) {
	f, r, gen, dbPath := packedLexicalFixture(t)
	flushed(t, f.s)
	comparePackedToLive(t, f, r, gen, openRawDB(t, dbPath))
}

// comparePackedToLive is the comparison itself, so a generation assembled any
// other way -- a delta that inherits its predecessor's segments, say -- is held
// to the same equality without a second copy of it.
func comparePackedToLive(t *testing.T, f *fixture, r *store.PinnedReader, gen model.GenerationID, db *sql.DB) {
	t.Helper()
	genID := int64(gen)

	var liveDocs, liveTokens int64
	if err := db.QueryRow(`SELECT count(*), coalesce(sum(su.token_count), 0) FROM search_units su
		JOIN units u ON u.id = su.unit_id
		JOIN generation_units gu ON gu.unit_id = su.unit_id AND gu.generation_id = ?`, genID).
		Scan(&liveDocs, &liveTokens); err != nil {
		t.Fatalf("live statistics: %v", err)
	}
	packedDocs, packedTokens, err := r.SearchStats(f.ctx)
	if err != nil {
		t.Fatalf("SearchStats: %v", err)
	}
	if packedDocs != liveDocs || packedTokens != liveTokens {
		t.Fatalf("packed statistics %d/%d, live %d/%d", packedDocs, packedTokens, liveDocs, liveTokens)
	}
	if liveDocs == 0 {
		t.Fatal("the fixture published no visible document; the comparison below would prove nothing")
	}

	terms := vocabularyTerms(t, db)
	if len(terms) < 4 {
		t.Fatalf("the fixture indexed %d terms; too few to compare", len(terms))
	}
	packedDF, err := r.DocumentFrequency(f.ctx, terms)
	if err != nil {
		t.Fatalf("DocumentFrequency: %v", err)
	}
	session, err := r.OpenPostings(f.ctx)
	if err != nil {
		t.Fatalf("OpenPostings: %v", err)
	}
	defer session.Close()
	for i, term := range terms {
		var liveDF int64
		if err := db.QueryRow(`SELECT count(DISTINCT v.doc) FROM search_vocab v
			JOIN search_units su ON su.doc_id = v.doc
			WHERE v.term = ? AND EXISTS (SELECT 1 FROM generation_units gu
				WHERE gu.unit_id = su.unit_id AND gu.generation_id = ?)`, term, genID).Scan(&liveDF); err != nil {
			t.Fatalf("live df(%q): %v", term, err)
		}
		if packedDF[i] != liveDF {
			t.Fatalf("term %q packed df %d, live df %d", term, packedDF[i], liveDF)
		}
		live, err := session.TermOccurrences(f.ctx, term)
		if err != nil {
			t.Fatalf("TermOccurrences(%q): %v", term, err)
		}
		liveSeq := drainCounts(t, f, live, term)
		packed, err := session.TermCounts(f.ctx, term)
		if err != nil {
			t.Fatalf("TermCounts(%q): %v", term, err)
		}
		packedSeq := drainCounts(t, f, packed, term)
		if packedSeq != liveSeq {
			t.Fatalf("term %q packed occurrences\n%s\nlive occurrences\n%s", term, packedSeq, liveSeq)
		}
	}
}

// deltaChain publishes a generation over two files, then a delta generation
// whose one unit re-emits the changed file and carries the untouched one, and
// returns both. It is the smallest shape that exercises inheritance: the delta
// unit owns the segment its predecessor folded -- through the document it
// carried out of it -- AND the segment its own seal folded, so an activation
// that does not suppress a member's segment it has already inherited names one
// segment twice and delivers every document in it twice.
func deltaChain(t *testing.T) (*fixture, model.GenerationID, model.GenerationID, string) {
	t.Helper()
	dbPath := t.TempDir() + "/codectx.db"
	f := newFixture(t, dbPath)
	a1 := f.file("pkg/a.go", "package pkg\nfunc Alpha() { Alpha() }\n")
	b := f.file("pkg/b.go", "package pkg\nfunc Beta() { Alpha() }\n")
	snap1 := f.snapshot("one", a1, b)
	gen1, err := f.s.BeginGeneration(f.ctx, f.repo, snap1.ID, model.H("semantic"), "main")
	if err != nil {
		t.Fatal(err)
	}
	run1 := f.run(gen1)
	w1 := f.beginScope(gen1, run1, configHash, a1, b)
	f.fillScope(w1, run1, a1, b)
	if err := f.s.SealUnit(f.ctx, w1); err != nil {
		t.Fatalf("SealUnit: %v", err)
	}
	f.activate(gen1, 0)

	a2 := f.file("pkg/a.go", "package pkg\nfunc Alpha() { /* changed */ Alpha() }\n")
	snap2 := f.snapshot("two", a2, b)
	gen2, err := f.s.BeginGeneration(f.ctx, f.repo, snap2.ID, model.H("semantic"), "main")
	if err != nil {
		t.Fatal(err)
	}
	run2 := f.run(gen2)
	w2 := f.beginScope(gen2, run2, "cfg-delta", a2, b)
	f.fillFile(w2, run2, a2)
	f.fillIndexLevel(w2, run2, a2)
	stats, err := w2.CarryOver(f.ctx, w1.UnitID(), store.Replaced{
		Files:  slices.Values([]model.FileID{a2.id}),
		Scopes: slices.Values([]string{"file:" + a2.path}),
		Keys:   slices.Values(keyList("key:node:" + a2.path)),
	})
	if err != nil {
		t.Fatalf("CarryOver: %v", err)
	}
	if stats.SearchUnits != 1 {
		t.Fatalf("the delta carried %d documents, want b.go's one", stats.SearchUnits)
	}
	if err := f.s.SealUnit(f.ctx, w2); err != nil {
		t.Fatalf("SealUnit(delta): %v", err)
	}
	f.activate(gen2, gen1)
	return f, gen1, gen2, dbPath
}

// segmentSet is a generation's segments in read order.
func segmentSet(t *testing.T, db *sql.DB, gen int64) []int64 {
	t.Helper()
	rows, err := db.Query(`SELECT segment_id FROM generation_segments WHERE generation_id = ? ORDER BY ord`, gen)
	if err != nil {
		t.Fatalf("generation_segments(%d): %v", gen, err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("generation_segments(%d): %v", gen, err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("generation_segments(%d): %v", gen, err)
	}
	return out
}

// A delta inherits its predecessor's segments, and its own members own some of
// those same segments through the documents they carried. Naming such a segment
// a second time is not a wasted row: every document in it is then offered by two
// cursors of the same merge, which doubles its contribution to every score it
// appears in. The packed answer must stay identical to the live path across the
// whole inherited set.
func TestADeltaInheritsEachOfItsPredecessorsSegmentsExactlyOnce(t *testing.T) {
	f, gen1, gen2, dbPath := deltaChain(t)
	flushed(t, f.s)
	db := openRawDB(t, dbPath)
	inherited, got := segmentSet(t, db, int64(gen1)), segmentSet(t, db, int64(gen2))
	if len(inherited) != 1 {
		t.Fatalf("the first generation named %d segments, want the one its single unit folded", len(inherited))
	}
	if len(got) != 2 {
		t.Fatalf("the delta named %d segments %v, want the inherited one and its own", len(got), got)
	}
	if got[0] != inherited[0] {
		t.Fatalf("the delta's set is %v; the inherited segment %d must keep its read position", got, inherited[0])
	}
	if got[0] == got[1] {
		t.Fatalf("the delta named segment %d twice; every document in it would be delivered twice", got[0])
	}
	r, err := f.s.PinGeneration(f.ctx, f.repo, gen2, time.Minute)
	if err != nil {
		t.Fatalf("PinGeneration: %v", err)
	}
	comparePackedToLive(t, f, r, gen2, db)
}

// An activation NAMES segments; it never folds one. The cost of publishing a
// delta is therefore the delta's own segment -- written at its seal, over the
// documents that unit itself published -- plus one row per named segment and
// one visible-document bitmap, and not a rewrite of everything the store has
// already packed. A segment that claimed documents it did not fold is the same
// defect in reverse: the generation's live count of it would exceed what it
// packed, and no posting exists for the difference.
func TestADeltaActivationPacksOnlyTheDocumentsItsOwnUnitPublished(t *testing.T) {
	f, gen1, gen2, dbPath := deltaChain(t)
	flushed(t, f.s)
	db := openRawDB(t, dbPath)

	var segments int64
	if err := db.QueryRow(`SELECT count(*) FROM lexical_segments`).Scan(&segments); err != nil {
		t.Fatal(err)
	}
	if segments != 2 {
		t.Fatalf("the store holds %d segments after two seals; an activation folded one of its own", segments)
	}
	set := segmentSet(t, db, int64(gen2))
	counts := map[int64]int64{}
	for _, id := range set {
		var docs int64
		if err := db.QueryRow(`SELECT doc_count FROM lexical_segments WHERE id = ?`, id).Scan(&docs); err != nil {
			t.Fatal(err)
		}
		counts[id] = docs
	}
	inherited := segmentSet(t, db, int64(gen1))[0]
	if counts[inherited] != 2 {
		t.Fatalf("the inherited segment packs %d documents, want the two its seal folded", counts[inherited])
	}
	fresh := set[len(set)-1]
	if counts[fresh] != 1 {
		t.Fatalf("the delta's segment packs %d documents; a one-file delta publishes ONE, and anything more is the whole structure folded again", counts[fresh])
	}
	// The only per-document thing an activation writes: one BIT per document
	// rowid, not a row per document.
	var bitmap, docs int64
	if err := db.QueryRow(`SELECT length(visible), doc_count FROM generation_lexical WHERE generation_id = ?`,
		int64(gen2)).Scan(&bitmap, &docs); err != nil {
		t.Fatal(err)
	}
	if docs != 2 {
		t.Fatalf("the delta carries %d documents, want a.go's and b.go's", docs)
	}
	var maxDoc int64
	if err := db.QueryRow(`SELECT coalesce(max(su.doc_id), 0) FROM search_units su
		JOIN generation_units gu ON gu.unit_id = su.unit_id AND gu.generation_id = ?`,
		int64(gen2)).Scan(&maxDoc); err != nil {
		t.Fatal(err)
	}
	if want := maxDoc/8 + 1; bitmap != want {
		t.Fatalf("the visible bitmap is %d bytes for document rowids up to %d, want %d", bitmap, maxDoc, want)
	}
}

// A build that dies between its first documents and its seal leaves a staging
// database nothing will ever read again. The directory it sits in is shared by
// every unit of every process using this data directory, so it is never swept
// blindly -- the file goes where the dead unit is already known to be dead. If
// it did not, a workspace that crashes repeatedly grows a second copy of its
// token instances, on the operator's data disk, forever.
func TestRecoveringACrashedBuildRemovesItsLexicalStaging(t *testing.T) {
	dbPath := t.TempDir() + "/codectx.db"
	f := newFixture(t, dbPath)
	a := f.file("pkg/a.go", "package pkg\nfunc Alpha() { Alpha() }\n")
	snap := f.snapshot("one", a)
	gen, err := f.s.BeginGeneration(f.ctx, f.repo, snap.ID, model.H("semantic"), "main")
	if err != nil {
		t.Fatal(err)
	}
	run := f.run(gen)
	// Deliberately neither sealed, abandoned nor failed: this is the writer a
	// killed process leaves behind.
	w := f.begin(gen, run, a)
	f.fill(w, run, a)
	staging := filepath.Join(filepath.Dir(dbPath), "tmp", "lexical-*.db")
	before, err := filepath.Glob(staging)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != 1 {
		t.Fatalf("a building unit left %d staging databases, want the one it stages its token instances in", len(before))
	}
	// Recovery looks for staging generations on the reader pool, which cannot
	// see an ingestion group the store still holds open.
	flushed(t, f.s)
	if err := f.s.Recover(f.ctx, time.Now()); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	after, err := filepath.Glob(staging)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 0 {
		t.Fatalf("recovery left %v behind; the unit is gone and nothing will ever read or remove them", after)
	}
}

// A query is bounded by resources.max_query_terms, which is unlimited by
// default, and the reader must answer however many terms one arrives with: a
// bound of its own here refuses an ordinary query outright, and a partial
// answer would score the terms it dropped as if the corpus did not carry them.
// This asks for more terms than any filter list may hold and requires the one
// call to agree, term for term, with the same terms asked for one at a time.
func TestDocumentFrequencyAnswersEveryTermOfAWideQuery(t *testing.T) {
	f, r, _, dbPath := packedLexicalFixture(t)
	flushed(t, f.s)
	indexed := vocabularyTerms(t, openRawDB(t, dbPath))
	if len(indexed) == 0 {
		t.Fatal("the fixture indexed no term; the comparison below would prove nothing")
	}
	terms := append([]string(nil), indexed...)
	for i := 0; len(terms) <= model.MaxFilterValues; i++ {
		terms = append(terms, fmt.Sprintf("wordnodocumentcarries%d", i))
	}

	got, err := r.DocumentFrequency(f.ctx, terms)
	if err != nil {
		t.Fatalf("DocumentFrequency(%d terms): %v", len(terms), err)
	}
	if len(got) != len(terms) {
		t.Fatalf("DocumentFrequency answered %d frequencies for %d terms", len(got), len(terms))
	}
	var carried int
	for i, term := range terms {
		one, err := r.DocumentFrequency(f.ctx, []string{term})
		if err != nil {
			t.Fatalf("DocumentFrequency(%q): %v", term, err)
		}
		if got[i] != one[0] {
			t.Fatalf("term %q is %d in the %d-term answer and %d on its own", term, got[i], len(terms), one[0])
		}
		if one[0] > 0 {
			carried++
		}
	}
	if carried == 0 {
		t.Fatalf("every one of the %d terms answered zero; the agreement above proves nothing", len(terms))
	}
}

// occurrenceStream is the contract both the packed and the live stream answer;
// the test drains either through it so one comparison covers both.
type occurrenceStream interface {
	Next(ctx context.Context, limit int) ([]store.TermOccurrence, error)
	Close() error
}

// drainCounts renders a term's whole stream as the ordered per-document
// (column, count) sequence, which is exactly what the scorer folds.
func drainCounts(t *testing.T, f *fixture, s occurrenceStream, term string) string {
	t.Helper()
	defer s.Close()
	var b strings.Builder
	for {
		page, err := s.Next(f.ctx, 8)
		if err != nil {
			t.Fatalf("stream(%q): %v", term, err)
		}
		if page == nil {
			return b.String()
		}
		for _, o := range page {
			fmt.Fprintf(&b, "doc=%d col=%s count=%d\n", o.RowID, o.Column, o.Count)
		}
	}
}

func vocabularyTerms(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query(`SELECT DISTINCT term FROM search_vocab ORDER BY term`)
	if err != nil {
		t.Fatalf("terms: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var term string
		if err := rows.Scan(&term); err != nil {
			t.Fatalf("scan term: %v", err)
		}
		out = append(out, term)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("terms: %v", err)
	}
	return out
}

// No scan of the packed lexical structure may build a temporary b-tree. Both
// of these run at activation, over a set that grows with the repository, and a
// sorter over either is unbounded memory at the one moment that publishes facts
// to every reader -- the defect ADR-0007 Decision 1 exists to avoid. The ONE
// ordered read of the design is the seal's, over one unit's own staging in a
// database of its own; nothing in the workspace database sorts.
func TestLexicalBuildScansUseNoTempBTree(t *testing.T) {
	f, _, gen, dbPath := packedLexicalFixture(t)
	flushed(t, f.s)
	db, genID := openRawDB(t, dbPath), int64(gen)
	for _, q := range []struct{ what, query string }{
		{"the activation's walk of its members' segments", store.GenerationSegmentQuery()},
		{"the activation's walk of its predecessor's set", store.PredecessorSegmentQuery()},
		{"a reader's walk of the generation's segment set", store.GenerationLexicalQuery()},
	} {
		plan := explain(t, db, q.query, genID)
		t.Logf("%s:\n%s", q.what, plan)
		if strings.Contains(strings.ToUpper(plan), "TEMP B-TREE") {
			t.Fatalf("%s builds a temporary b-tree:\n%s", q.what, plan)
		}
	}
}
