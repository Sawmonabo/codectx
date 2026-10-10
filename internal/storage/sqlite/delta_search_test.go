package sqlite_test

import (
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/search/textindex"
	store "github.com/Sawmonabo/codectx/internal/storage/sqlite"
)

// lexicalAnswer is everything a generation's lexical tier answers a query set
// with, as text: each term's page in rank order, every result with its score,
// occurrence count and identity, and the page's match count. Scores are exact
// over the generation's visible documents and ties break on content, never on
// a doc id, so two generations that hold the same visible documents answer
// identically here, and a generation that sees one document more -- a hidden
// document counted in N, df or the length average -- does not.
func lexicalAnswer(t *testing.T, f *fixture, gen model.GenerationID, terms []string) string {
	t.Helper()
	r, err := f.s.PinGeneration(f.ctx, f.repo, gen, time.Minute)
	if err != nil {
		t.Fatalf("PinGeneration(%d): %v", gen, err)
	}
	defer r.Close()
	view, err := r.SearchView(f.ctx)
	if err != nil {
		t.Fatalf("SearchView(%d): %v", gen, err)
	}
	defer view.Close()
	var b strings.Builder
	for _, term := range terms {
		page, err := view.Search(f.ctx, textindex.Request{Query: term, Depth: 100})
		if err != nil {
			t.Fatalf("Search(%q): %v", term, err)
		}
		fmt.Fprintf(&b, "term %q matches=%d exact=%v more=%v\n", term, page.Matches, page.MatchesExact, page.More)
		for _, h := range page.Hits {
			fmt.Fprintf(&b, "  %s %s %s %q [%d,%d) score=%d occurrences=%d key=%s\n",
				h.Path, h.Name, h.QualifiedName, h.Signature, h.Bytes.Start, h.Bytes.End, h.ScoreMicros, h.Occurrences, h.SearchKey)
		}
	}
	return b.String()
}

// TestDeltaCarriesTheTextItIndexed pins two invariants of a delta carry-over.
//
// Visibility is per document: the delta unit carries b.go's document from its
// predecessor and replaces a.go's, so its generation shows the carried document
// and not the predecessor's a.go document, although the library holds both
// under the predecessor's unit. Mutation: build the visible set from the units
// that ADDED the documents instead of from the member rows' doc ids -- the
// predecessor's a.go document (the only one holding zqxoriginal) then shows
// in the delta generation and its statistics, and the delta answer differs from
// the full index's.
//
// A carried document keeps the text it was indexed with. The database stores
// no body, and the body a producer published is NOT the
// source over the document's byte range -- treesitter publishes names,
// signature and documentation over the whole declaration's extent, and the
// filesystem provider publishes no body at all. So a carry-over that rebuilds
// the index from the content store indexes text that was never any document's
// body: the same tree answers differently depending on whether it was indexed
// in full or refreshed, with no failing check anywhere.
//
// The fixture is what makes this provable: searchDoc's Body (docBody) shares
// no token with the file's source, so re-derivation shows up as a changed hit
// set rather than as an identical one.
func TestDeltaCarriesTheTextItIndexed(t *testing.T) {
	dbPath := t.TempDir() + "/codectx.db"
	f := newFixture(t, dbPath)
	a1 := f.file("pkg/a.go", "package pkg\nfunc F() { zqxoriginal() }\n")
	b := f.file("pkg/b.go", "package pkg\nfunc F() { F() }\n")
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

	// a.go is edited; b.go is untouched and is the document that gets carried.
	a2 := f.file("pkg/a.go", "package pkg\nfunc F() { /* edited */ }\n")
	snap2 := f.snapshot("two", a2, b)

	genFull, err := f.s.BeginGeneration(f.ctx, f.repo, snap2.ID, model.H("semantic"), "main")
	if err != nil {
		t.Fatal(err)
	}
	runFull := f.run(genFull)
	wFull := f.beginScope(genFull, runFull, "cfg-full", a2, b)
	f.fillScope(wFull, runFull, a2, b)
	if err := f.s.SealUnit(f.ctx, wFull); err != nil {
		t.Fatalf("SealUnit(full): %v", err)
	}
	f.activate(genFull, gen1)

	genDelta, err := f.s.BeginGeneration(f.ctx, f.repo, snap2.ID, model.H("semantic"), "main")
	if err != nil {
		t.Fatal(err)
	}
	runDelta := f.run(genDelta)
	wDelta := f.beginScope(genDelta, runDelta, "cfg-delta", a2, b)
	f.fillFile(wDelta, runDelta, a2)
	f.fillIndexLevel(wDelta, runDelta, a2)
	stats, err := wDelta.CarryOver(f.ctx, w1.UnitID(), store.Replaced{
		Files:  slices.Values([]model.FileID{a2.id}),
		Scopes: slices.Values([]string{"file:" + a2.path}),
	})
	if err != nil {
		t.Fatalf("CarryOver: %v", err)
	}
	if stats.SearchUnits != 1 {
		t.Fatalf("carried %d documents, want b.go's one", stats.SearchUnits)
	}
	if err := f.s.SealUnit(f.ctx, wDelta); err != nil {
		t.Fatalf("SealUnit(delta): %v", err)
	}
	f.activate(genDelta, genFull)

	// zqxdocstring and zqxsignature appear only in a published body; package
	// and pkg appear only in the source the byte range covers. A carry-over
	// that re-derives the body loses the first pair and gains the second.
	terms := []string{"zqxdocstring", "zqxsignature", "f", "package", "pkg", "zqxoriginal", "edited"}
	full := lexicalAnswer(t, f, genFull, terms)
	delta := lexicalAnswer(t, f, genDelta, terms)
	if full != delta {
		t.Errorf("a delta answers differently from a full index of the same tree.\nfull:\n%s\ndelta:\n%s", full, delta)
	}
	if !strings.Contains(delta, "zqxoriginal\" matches=0") {
		t.Errorf("the delta generation sees the replaced a.go document of its predecessor:\n%s", delta)
	}
	if first := lexicalAnswer(t, f, gen1, []string{"zqxoriginal"}); !strings.Contains(first, "matches=1") {
		t.Fatalf("fixture no longer indexes the predecessor's a.go document under zqxoriginal:\n%s", first)
	}
	// A generation's visible set is a set of doc ids, so a generation that saw
	// one doc_id through two member rows would serve whichever row's identity
	// the library stored for it. Two facts
	// in two files keep that from happening -- CarryOver refuses a predecessor
	// of another provider or scope key, and generation_units is keyed by
	// (generation_id, provider_id, scope_key) -- and it has to hold by
	// induction over repeated deltas, so it is asserted rather than argued.
	flushed(t, f.s)
	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	var shared int64
	if err := raw.QueryRow(`SELECT count(*) FROM (SELECT su.doc_id FROM search_units su
		JOIN generation_units gu ON gu.unit_id = su.unit_id
		GROUP BY gu.generation_id, su.doc_id HAVING count(*) > 1)`).Scan(&shared); err != nil {
		t.Fatal(err)
	}
	if shared != 0 {
		t.Errorf("%d document ids are visible twice in one generation; a shared document must never reach two member units", shared)
	}
	if err := f.s.Check(f.ctx, true); err != nil {
		t.Fatalf("deep integrity check after a delta: %v", err)
	}

	// Retiring the predecessor is where a shared document is easiest to lose or
	// to mistake for garbage: the unit whose insert added it is gone, while
	// the delta unit still names it. deleteUnit must keep it (it is released
	// only when NO surviving unit names it), while the predecessor's a.go
	// document, which nothing names any more, is deleted from the index.
	if err := f.s.DeleteGeneration(f.ctx, gen1); err != nil {
		t.Fatalf("DeleteGeneration(gen1): %v", err)
	}
	if err := f.s.Check(f.ctx, true); err != nil {
		t.Fatalf("deep integrity check after retiring the unit that added the shared document: %v", err)
	}
	if after := lexicalAnswer(t, f, genDelta, terms); after != delta {
		t.Errorf("retiring the predecessor changed the delta generation's answer.\nbefore:\n%s\nafter:\n%s", delta, after)
	}

	if !strings.Contains(full, "zqxdocstring") {
		t.Fatalf("fixture no longer exercises a body that differs from its byte range:\n%s", full)
	}
}
