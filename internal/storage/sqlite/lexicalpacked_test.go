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
// packedLexicalFixture publishes two generations through the normal seal and
// activation path: the first merges THREE units' packed lists, and the second
// is a delta over the same tree that rebuilds one of them and reuses the other
// two, which is the merge input shape a one-file edit produces.
func packedLexicalFixture(t *testing.T) (*fixture, []model.GenerationID, string) {
	t.Helper()
	dbPath := t.TempDir() + "/codectx.db"
	f := newFixture(t, dbPath)
	a := f.file("pkg/a.go", "package pkg\nfunc Alpha() { Alpha() }\n")
	b := f.file("pkg/b.go", "package pkg\nfunc Beta() { Alpha() }\n")
	c := f.file("pkg/c.go", "package pkg\nfunc Gamma() { Beta() }\n")
	snap := f.snapshot("one", a, b, c)
	gen1, err := f.s.BeginGeneration(f.ctx, f.repo, snap.ID, model.H("semantic"), "main")
	if err != nil {
		t.Fatal(err)
	}
	run1 := f.run(gen1)
	units := []model.UnitID{f.unit(gen1, run1, a), f.unit(gen1, run1, b), f.unit(gen1, run1, c)}
	f.activate(gen1, 0)

	// One file is edited: its unit is rebuilt and sealed, the two untouched
	// units are reused exactly as they were sealed.
	a2 := f.file("pkg/a.go", "package pkg\nfunc Alpha() { Alpha(); Alpha() }\n")
	snap2 := f.snapshot("two", a2, b, c)
	gen2, err := f.s.BeginGeneration(f.ctx, f.repo, snap2.ID, model.H("semantic"), "main")
	if err != nil {
		t.Fatal(err)
	}
	run2 := f.run(gen2)
	f.unit(gen2, run2, a2)
	for _, u := range units[1:] {
		if err := f.s.AttachUnit(f.ctx, gen2, u); err != nil {
			t.Fatalf("AttachUnit: %v", err)
		}
	}
	f.activate(gen2, gen1)
	return f, []model.GenerationID{gen1, gen2}, dbPath
}

func TestPackedLexicalMatchesTheLivePath(t *testing.T) {
	f, gens, dbPath := packedLexicalFixture(t)
	db := openRawDB(t, dbPath)
	for _, gen := range gens {
		r, err := f.s.PinGeneration(f.ctx, f.repo, gen, time.Minute)
		if err != nil {
			t.Fatalf("PinGeneration(%d): %v", gen, err)
		}
		comparePackedToLive(t, f, r, db, int64(gen))
		if err := r.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	}
}

func comparePackedToLive(t *testing.T, f *fixture, r *store.PinnedReader, db *sql.DB, genID int64) {
	t.Helper()
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
		t.Fatalf("generation %d packed statistics %d/%d, live %d/%d", genID, packedDocs, packedTokens, liveDocs, liveTokens)
	}
	if liveDocs < 3 {
		t.Fatalf("generation %d published %d documents; too few to prove a merge across units", genID, liveDocs)
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
			t.Fatalf("generation %d term %q packed df %d, live df %d", genID, term, packedDF[i], liveDF)
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
			t.Fatalf("generation %d term %q packed occurrences\n%s\nlive occurrences\n%s", genID, term, packedSeq, liveSeq)
		}
	}
	comparePackedDocuments(t, f, r, db, genID)
}

// comparePackedDocuments is the Decision 2 half: every field the ranker, the
// tie-break, the deduplication key and the filters read now comes from the
// packed attribute stream instead of a document row, so a field that drifts
// there serves a wrong path, a wrong byte range or a wrong identity on a page
// nobody can tell apart from a right one.
func comparePackedDocuments(t *testing.T, f *fixture, r *store.PinnedReader, db *sql.DB, genID int64) {
	t.Helper()
	rows, err := db.Query(`SELECT su.doc_id, lower(hex(su.search_key)), coalesce(lower(hex(ni.canonical)), ''),
		lower(hex(su.file_id)), su.path, su.kind, su.name, su.qualified_name, su.signature,
		su.start_byte, su.end_byte, su.token_count
		FROM search_units su LEFT JOIN node_ids ni ON ni.id = su.node_id
		JOIN generation_units gu ON gu.unit_id = su.unit_id AND gu.generation_id = ?
		ORDER BY su.doc_id`, genID)
	if err != nil {
		t.Fatalf("live documents: %v", err)
	}
	defer rows.Close()
	var rowids []int64
	var live []string
	for rows.Next() {
		var id, start, end, tokens int64
		var key, node, file, path, kind, name, qname, sig string
		if err := rows.Scan(&id, &key, &node, &file, &path, &kind, &name, &qname, &sig, &start, &end, &tokens); err != nil {
			t.Fatalf("scan live document: %v", err)
		}
		rowids = append(rowids, id)
		live = append(live, fmt.Sprintf("%d %s %s %s %s %s %s %s %q [%d,%d) len=%d",
			id, key, node, file, path, kind, name, qname, sig, start, end, tokens))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("live documents: %v", err)
	}
	packed, err := r.PackedDocuments(f.ctx, rowids)
	if err != nil {
		t.Fatalf("PackedDocuments: %v", err)
	}
	if len(packed) != len(live) {
		t.Fatalf("generation %d hydrated %d of %d documents from the packed stream", genID, len(packed), len(live))
	}
	for i, d := range packed {
		got := fmt.Sprintf("%d %s %s %s %s %s %s %s %q [%d,%d) len=%d",
			d.RowID, d.ID, string(d.NodeID), string(d.FileID), d.Path, string(d.Kind), d.Name, d.QualifiedName,
			d.Signature, d.Bytes.Start, d.Bytes.End, d.TokenCount)
		if got != live[i] {
			t.Fatalf("generation %d packed document\n%s\nlive document\n%s", genID, got, live[i])
		}
	}
	// A rowid of another generation must be omitted, not hydrated: the stream
	// carries the generation's documents and nothing else.
	if out, err := r.PackedDocuments(f.ctx, []int64{rowids[len(rowids)-1] + 1000}); err != nil || len(out) != 0 {
		t.Fatalf("PackedDocuments of an absent rowid = %v, %v; want no document and no error", out, err)
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

// The fold and the merge must not build a temporary b-tree. The fold steps
// every posting instance of a unit and the merge every part of every visible
// unit, and a sorter over either set is unbounded memory at index time -- the
// defect ADR-0007 Decision 1 exists to avoid.
func TestLexicalBuildScansUseNoTempBTree(t *testing.T) {
	_, _, dbPath := packedLexicalFixture(t)
	db := openRawDB(t, dbPath)
	if _, err := db.Exec(`CREATE VIRTUAL TABLE temp.unit_fts_1 USING fts5(name, qualified_name, signature, path, body, content='', tokenize='unicode61', detail='full')`); err != nil {
		t.Fatalf("unit index: %v", err)
	}
	if _, err := db.Exec(`CREATE VIRTUAL TABLE temp.unit_vocab_1 USING fts5vocab(unit_fts_1, 'instance')`); err != nil {
		t.Fatalf("unit vocabulary: %v", err)
	}
	for _, q := range []string{
		store.UnitInstanceQuery("temp.unit_vocab_1"),
		store.MergePartQuery(store.UnitLexicalPartsTable, "unit_id"),
	} {
		plan := explain(t, db, q)
		t.Logf("%s\n%s", q, plan)
		if strings.Contains(strings.ToUpper(plan), "TEMP B-TREE") {
			t.Fatalf("a lexical build scan builds a temporary b-tree:\n%s\n%s", q, plan)
		}
	}
}
