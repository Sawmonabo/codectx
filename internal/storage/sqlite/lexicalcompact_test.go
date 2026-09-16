package sqlite_test

import (
	"database/sql"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
	store "github.com/Sawmonabo/codectx/internal/storage/sqlite"
)

// tierFile names and fills the i-th file of a fixture that needs several
// segments in one size tier; the bodies differ so each unit folds a segment of
// its own vocabulary.
func tierFile(f *fixture, i int) fileFixture {
	return f.file(fmt.Sprintf("pkg/f%d.go", i),
		fmt.Sprintf("package pkg\nfunc Fn%d() { Fn%d() }\n", i, i))
}

// replacedFiles is the Replaced set of a delta that re-emits the named files.
func replacedFiles(files ...fileFixture) store.Replaced {
	ids := make([]model.FileID, 0, len(files))
	scopes := make([]string, 0, len(files))
	keys := make([]string, 0, len(files))
	for _, ff := range files {
		ids = append(ids, ff.id)
		scopes = append(scopes, "file:"+ff.path)
		keys = append(keys, "key:node:"+ff.path)
	}
	return store.Replaced{
		Files:  slices.Values(ids),
		Scopes: slices.Values(scopes),
		Keys:   slices.Values(keyList(keys...)),
	}
}

// documentsOfUnit answers the document rowids the named unit published or
// carried.
func documentsOfUnit(t *testing.T, db *sql.DB, unit model.UnitID) []int64 {
	t.Helper()
	raw, err := model.DecodeID(string(unit))
	if err != nil {
		t.Fatal(err)
	}
	rows, err := db.Query(`SELECT su.doc_id FROM search_units su JOIN units u ON u.id = su.unit_id
		WHERE u.unit_key = ?`, raw)
	if err != nil {
		t.Fatalf("documents of %s: %v", unit, err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var doc int64
		if err := rows.Scan(&doc); err != nil {
			t.Fatalf("documents of %s: %v", unit, err)
		}
		out = append(out, doc)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("documents of %s: %v", unit, err)
	}
	slices.Sort(out)
	return out
}

// Compaction of the packed lexical segments. A seal folds one segment per
// unit, so a store that never merged them would binary-search one directory
// per sealed unit on every term of every query, and every collected unit's
// documents would stay in the postings forever. These tests hold the two
// triggers and the one rule that makes merging safe: a merge keeps the
// documents this generation merely HIDES -- their unit can be attached again --
// and re-points every row it absorbs, so no document is ever offered by two
// segments of one generation and no reattached unit loses its postings.

// segmentOf answers the segment a document's row points at.
func segmentOf(t *testing.T, db *sql.DB, doc int64) int64 {
	t.Helper()
	var seg int64
	if err := db.QueryRow(`SELECT segment_id FROM search_units WHERE doc_id = ? LIMIT 1`, doc).Scan(&seg); err != nil {
		t.Fatalf("segment of document %d: %v", doc, err)
	}
	return seg
}

// A tier holding more than the ratio's worth of segments is merged into one.
// Without it a store's segment count is the count of units it has ever sealed:
// every term of every query pays one binary search per segment, and the bound
// ADR-0007 states for a lexical read stops holding. The merge must also leave
// NO row naming an input -- search_units.segment_id is what a later generation
// derives its set from, so a row still naming an absorbed segment would name it
// beside the segment holding the very same documents.
func TestActivationMergesATierIntoOneSegment(t *testing.T) {
	dbPath := t.TempDir() + "/codectx.db"
	f := newFixture(t, dbPath)
	ratio := store.LexicalMergeRatio()
	files := make([]fileFixture, 0, ratio+1)
	for i := 0; i <= ratio; i++ {
		files = append(files, tierFile(f, i))
	}
	snap := f.snapshot("one", files...)
	gen, err := f.s.BeginGeneration(f.ctx, f.repo, snap.ID, model.H("semantic"), "main")
	if err != nil {
		t.Fatal(err)
	}
	run := f.run(gen)
	// One unit per file, each its own scope key, so the generation seals
	// ratio+1 segments into the one tier a fixture's kilobytes fall in.
	for i, ff := range files {
		w := f.beginScopeKey(gen, run, configHash, fmt.Sprintf("scope-%d", i), ff)
		f.fillFile(w, run, ff)
		if err := f.s.SealUnit(f.ctx, w); err != nil {
			t.Fatalf("SealUnit(%s): %v", ff.path, err)
		}
	}
	f.activate(gen, 0)
	flushed(t, f.s)
	db := openRawDB(t, dbPath)

	set := segmentSet(t, db, int64(gen))
	if len(set) != 1 {
		t.Fatalf("a generation of %d sealed segments names %d of them; the tier was not merged", ratio+1, len(set))
	}
	merged := set[0]
	var docs, hidden int64
	if err := db.QueryRow(`SELECT doc_count FROM lexical_segments WHERE id = ?`, merged).Scan(&docs); err != nil {
		t.Fatal(err)
	}
	if docs != int64(ratio+1) {
		t.Fatalf("the merged segment packs %d documents, want the %d its inputs held", docs, ratio+1)
	}
	if err := db.QueryRow(`SELECT hidden FROM generation_segments WHERE generation_id = ? AND segment_id = ?`,
		int64(gen), merged).Scan(&hidden); err != nil {
		t.Fatal(err)
	}
	if hidden != 0 {
		t.Fatalf("the merged segment reports %d hidden documents; every one of them is this generation's", hidden)
	}
	// Every input row re-pointed. A row left naming an absorbed segment is what
	// lets a later generation name that segment beside the merged one.
	var stale int64
	if err := db.QueryRow(`SELECT count(*) FROM search_units WHERE segment_id IS NOT NULL AND segment_id <> ?`,
		merged).Scan(&stale); err != nil {
		t.Fatal(err)
	}
	if stale != 0 {
		t.Fatalf("%d documents still name a segment the merge absorbed; the merged segment holds them too", stale)
	}
	r, err := f.s.PinGeneration(f.ctx, f.repo, gen, time.Minute)
	if err != nil {
		t.Fatalf("PinGeneration: %v", err)
	}
	comparePackedToLive(t, f, r, gen, db)
}

// A unit that leaves a generation is not gone: it can be attached to a later
// one at any time, and it must then find its postings where its rows point. A
// merge that dropped the documents the activating generation merely HIDES would
// delete them from the only copy that exists -- the index is contentless, so no
// row of the store can rebuild a document's postings -- and the reattached unit
// would answer nothing for text it indexed. Only DEAD documents, whose unit was
// collected, are dropped.
func TestAMergeKeepsHiddenDocumentsSoAReattachedUnitStillAnswers(t *testing.T) {
	defer store.SetLexicalMergeRatio(1)()
	dbPath := t.TempDir() + "/codectx.db"
	f := newFixture(t, dbPath)
	a := f.file("pkg/a.go", "package pkg\nfunc Alpha() { Alpha() }\n")
	b := f.file("pkg/b.go", "package pkg\nfunc Beta() { Beta() }\n")
	c := f.file("pkg/c.go", "package pkg\nfunc Gamma() { Gamma() }\n")
	snap := f.snapshot("one", a, b, c)

	// The first generation seals two units into one tier, which the shrunk
	// ratio merges at once.
	gen1, err := f.s.BeginGeneration(f.ctx, f.repo, snap.ID, model.H("semantic"), "main")
	if err != nil {
		t.Fatal(err)
	}
	run1 := f.run(gen1)
	wa := f.beginScopeKey(gen1, run1, configHash, "scope-a", a)
	f.fillFile(wa, run1, a)
	if err := f.s.SealUnit(f.ctx, wa); err != nil {
		t.Fatalf("SealUnit(a): %v", err)
	}
	wb := f.beginScopeKey(gen1, run1, configHash, "scope-b", b)
	f.fillFile(wb, run1, b)
	if err := f.s.SealUnit(f.ctx, wb); err != nil {
		t.Fatalf("SealUnit(b): %v", err)
	}
	f.activate(gen1, 0)

	// The second generation drops the first unit and adds a third, so the
	// merge it triggers holds documents this generation hides.
	gen2, err := f.s.BeginGeneration(f.ctx, f.repo, snap.ID, model.H("semantic"), "main")
	if err != nil {
		t.Fatal(err)
	}
	run2 := f.run(gen2)
	if err := f.s.AttachUnit(f.ctx, gen2, wb.UnitID()); err != nil {
		t.Fatalf("AttachUnit(b): %v", err)
	}
	wc := f.beginScopeKey(gen2, run2, configHash, "scope-c", c)
	f.fillFile(wc, run2, c)
	if err := f.s.SealUnit(f.ctx, wc); err != nil {
		t.Fatalf("SealUnit(c): %v", err)
	}
	f.activate(gen2, gen1)
	flushed(t, f.s)
	db := openRawDB(t, dbPath)
	if set := segmentSet(t, db, int64(gen2)); len(set) != 1 {
		t.Fatalf("the second generation names %d segments; the tier holding both was not merged", len(set))
	}
	hiddenDoc := documentsOfUnit(t, db, wa.UnitID())
	if len(hiddenDoc) != 1 {
		t.Fatalf("the retired unit holds %d documents, want the one it published", len(hiddenDoc))
	}
	packed, err := store.SegmentPostingDocuments(db, segmentSet(t, db, int64(gen2))[0])
	if err != nil {
		t.Fatalf("SegmentPostingDocuments: %v", err)
	}
	if !contains(packed, hiddenDoc[0]) {
		t.Fatalf("the merged segment carries documents %v; the hidden document %d was dropped and no row of the store can rebuild it",
			packed, hiddenDoc[0])
	}

	// The third generation attaches the retired unit again. Its documents must
	// come back exactly once each, beside the units that stayed.
	gen3, err := f.s.BeginGeneration(f.ctx, f.repo, snap.ID, model.H("semantic"), "main")
	if err != nil {
		t.Fatal(err)
	}
	f.run(gen3)
	for _, w := range []*store.UnitWriter{wa, wb, wc} {
		if err := f.s.AttachUnit(f.ctx, gen3, w.UnitID()); err != nil {
			t.Fatalf("AttachUnit(%s): %v", w.UnitID(), err)
		}
	}
	f.activate(gen3, gen2)
	flushed(t, f.s)
	r, err := f.s.PinGeneration(f.ctx, f.repo, gen3, time.Minute)
	if err != nil {
		t.Fatalf("PinGeneration: %v", err)
	}
	// comparePackedToLive walks every indexed term and compares the ordered
	// per-document (column, count) sequence with the live path's, so a document
	// delivered twice, missing, or offered by two segments fails here.
	comparePackedToLive(t, f, r, gen3, openRawDB(t, dbPath))
}

// A segment survives the generation that named it only while some document
// still points at it, and survives its last document only while a retained
// generation still reads it. Collecting the generations before the last must
// therefore leave exactly the segments the last one holds: a collector that
// over-approximates ownership keeps every segment the store ever folded, on the
// operator's disk, forever.
func TestCollectingEarlierGenerationsLeavesOnlyTheLastOnesSegments(t *testing.T) {
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

	// Two deltas, each re-emitting one file and carrying the other, so each
	// generation folds a segment of its own and inherits one.
	prev, prevGen := w1, gen1
	for i, changed := range []fileFixture{
		f.file("pkg/a.go", "package pkg\nfunc Alpha() { /* two */ Alpha() }\n"),
		f.file("pkg/a.go", "package pkg\nfunc Alpha() { /* three */ Alpha() }\n"),
	} {
		snap := f.snapshot(fmt.Sprintf("gen%d", i+2), changed, b)
		gen, err := f.s.BeginGeneration(f.ctx, f.repo, snap.ID, model.H("semantic"), "main")
		if err != nil {
			t.Fatal(err)
		}
		run := f.run(gen)
		w := f.beginScope(gen, run, fmt.Sprintf("cfg-delta-%d", i), changed, b)
		f.fillFile(w, run, changed)
		f.fillIndexLevel(w, run, changed)
		if _, err := w.CarryOver(f.ctx, prev.UnitID(), replacedFiles(changed)); err != nil {
			t.Fatalf("CarryOver: %v", err)
		}
		if err := f.s.SealUnit(f.ctx, w); err != nil {
			t.Fatalf("SealUnit(delta): %v", err)
		}
		f.activate(gen, prevGen)
		prev, prevGen = w, gen
	}
	for _, gen := range []model.GenerationID{gen1} {
		if err := f.s.DeleteGeneration(f.ctx, gen); err != nil {
			t.Fatalf("DeleteGeneration(%d): %v", gen, err)
		}
	}
	flushed(t, f.s)
	db := openRawDB(t, dbPath)
	want := len(segmentSet(t, db, int64(prevGen)))
	var got int64
	if err := db.QueryRow(`SELECT count(*) FROM lexical_segments`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != int64(want) {
		t.Fatalf("the store holds %d segments after the earlier generations were collected; the live one names %d",
			got, want)
	}
	r, err := f.s.PinGeneration(f.ctx, f.repo, prevGen, time.Minute)
	if err != nil {
		t.Fatalf("PinGeneration: %v", err)
	}
	comparePackedToLive(t, f, r, prevGen, db)
}

// A segment whose unit was collected still carries that unit's documents in its
// postings: nothing rewrote it to remove them, and a term they appear in walks
// past every one of them on every query that asks for it. Once more than half
// of a segment's documents are dead it is rewritten alone, and the dead rowids
// must be GONE from the packed bytes -- not merely hidden by a bitmap, which
// would leave the cost in place forever.
func TestASegmentMostlyDeadIsRewrittenWithoutItsDeadDocuments(t *testing.T) {
	defer store.SetLexicalMergeRatio(1 << 20)()
	dbPath := t.TempDir() + "/codectx.db"
	f := newFixture(t, dbPath)
	files := []fileFixture{
		f.file("pkg/a.go", "package pkg\nfunc Alpha() { Alpha() }\n"),
		f.file("pkg/b.go", "package pkg\nfunc Beta() { Beta() }\n"),
		f.file("pkg/c.go", "package pkg\nfunc Gamma() { Gamma() }\n"),
		f.file("pkg/d.go", "package pkg\nfunc Delta() { Delta() }\n"),
	}
	snap1 := f.snapshot("one", files...)
	gen1, err := f.s.BeginGeneration(f.ctx, f.repo, snap1.ID, model.H("semantic"), "main")
	if err != nil {
		t.Fatal(err)
	}
	run1 := f.run(gen1)
	w1 := f.beginScope(gen1, run1, configHash, files...)
	for _, ff := range files {
		f.fillFile(w1, run1, ff)
	}
	if err := f.s.SealUnit(f.ctx, w1); err != nil {
		t.Fatalf("SealUnit: %v", err)
	}
	f.activate(gen1, 0)

	// A delta that re-emits three of the four files, carrying only the fourth
	// out of the first segment.
	changed := []fileFixture{
		f.file("pkg/a.go", "package pkg\nfunc Alpha() { /* two */ Alpha() }\n"),
		f.file("pkg/b.go", "package pkg\nfunc Beta() { /* two */ Beta() }\n"),
		f.file("pkg/c.go", "package pkg\nfunc Gamma() { /* two */ Gamma() }\n"),
	}
	snap2 := f.snapshot("two", changed[0], changed[1], changed[2], files[3])
	gen2, err := f.s.BeginGeneration(f.ctx, f.repo, snap2.ID, model.H("semantic"), "main")
	if err != nil {
		t.Fatal(err)
	}
	run2 := f.run(gen2)
	w2 := f.beginScope(gen2, run2, "cfg-delta", changed[0], changed[1], changed[2], files[3])
	for _, ff := range changed {
		f.fillFile(w2, run2, ff)
	}
	f.fillIndexLevel(w2, run2, changed[0])
	if _, err := w2.CarryOver(f.ctx, w1.UnitID(), replacedFiles(changed...)); err != nil {
		t.Fatalf("CarryOver: %v", err)
	}
	if err := f.s.SealUnit(f.ctx, w2); err != nil {
		t.Fatalf("SealUnit(delta): %v", err)
	}
	f.activate(gen2, gen1)
	flushed(t, f.s)
	db := openRawDB(t, dbPath)
	carried := documentsOfUnit(t, db, w2.UnitID())
	first := segmentSet(t, db, int64(gen1))[0]
	dead, err := store.SegmentPostingDocuments(db, first)
	if err != nil {
		t.Fatalf("SegmentPostingDocuments: %v", err)
	}
	if len(dead) != 4 {
		t.Fatalf("the first segment packs documents %v, want the four its seal folded", dead)
	}

	// Collecting the first generation collects its unit, and with it three of
	// the four documents the first segment packed: they are DEAD, not hidden.
	if err := f.s.DeleteGeneration(f.ctx, gen1); err != nil {
		t.Fatalf("DeleteGeneration: %v", err)
	}
	gen3, err := f.s.BeginGeneration(f.ctx, f.repo, snap2.ID, model.H("semantic"), "main")
	if err != nil {
		t.Fatal(err)
	}
	f.run(gen3)
	if err := f.s.AttachUnit(f.ctx, gen3, w2.UnitID()); err != nil {
		t.Fatalf("AttachUnit: %v", err)
	}
	f.activate(gen3, gen2)
	flushed(t, f.s)
	db = openRawDB(t, dbPath)

	survivor := int64(0)
	for _, doc := range carried {
		if contains(dead, doc) {
			survivor = doc
		}
	}
	if survivor == 0 {
		t.Fatalf("the delta carried none of the first segment's documents %v; nothing would be rewritten", dead)
	}
	rewritten := segmentOf(t, db, survivor)
	var docs int64
	if err := db.QueryRow(`SELECT doc_count FROM lexical_segments WHERE id = ?`, rewritten).Scan(&docs); err != nil {
		t.Fatal(err)
	}
	if docs != 1 {
		t.Fatalf("the segment holding the one surviving document packs %d documents; three quarters of it is dead and was not rewritten", docs)
	}
	got, err := store.SegmentPostingDocuments(db, rewritten)
	if err != nil {
		t.Fatalf("SegmentPostingDocuments: %v", err)
	}
	for _, doc := range dead {
		if doc != survivor && contains(got, doc) {
			t.Fatalf("the rewritten segment still carries the collected document %d in its postings: %v", doc, got)
		}
	}
	if !contains(got, survivor) {
		t.Fatalf("the rewritten segment carries %v, not the surviving document %d", got, survivor)
	}
	r, err := f.s.PinGeneration(f.ctx, f.repo, gen3, time.Minute)
	if err != nil {
		t.Fatalf("PinGeneration: %v", err)
	}
	comparePackedToLive(t, f, r, gen3, db)
}

func contains(in []int64, want int64) bool {
	for _, v := range in {
		if v == want {
			return true
		}
	}
	return false
}
