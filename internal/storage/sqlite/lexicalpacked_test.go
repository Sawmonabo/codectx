package sqlite_test

import (
	"context"
	"database/sql"
	"fmt"
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
	w := f.beginScope(gen, run, configHash, a, b)
	f.fillScope(w, run, a, b)
	if err := f.s.SealUnit(f.ctx, w); err != nil {
		t.Fatalf("SealUnit: %v", err)
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
	db := openRawDB(t, dbPath)
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

// The build's instance scan must not build a temporary b-tree: it steps every
// posting instance of the repository, and a sorter over that set is unbounded
// memory at activation -- the defect ADR-0007 Decision 1 exists to avoid.
func TestLexicalBuildScanUsesNoTempBTree(t *testing.T) {
	f, _, _, dbPath := packedLexicalFixture(t)
	flushed(t, f.s)
	plan := explain(t, openRawDB(t, dbPath), store.LexicalInstanceQuery())
	t.Logf("instance scan plan:\n%s", plan)
	if strings.Contains(strings.ToUpper(plan), "TEMP B-TREE") {
		t.Fatalf("the build's instance scan builds a temporary b-tree over every posting instance:\n%s", plan)
	}
}
