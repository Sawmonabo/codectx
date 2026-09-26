package sqlite_test

import (
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
	store "github.com/Sawmonabo/codectx/internal/storage/sqlite"
)

// lexicalAnswer is everything a generation's lexical tier answers a query set
// with, as text: the ordered document tuples a term matches and every input
// BM25 scores them from. Scoring itself is a pure function of these (corpus
// size, total length, per-term document frequency, per-document length and
// per-term occurrence counts and offsets), and internal/search's rank order
// breaks ties on path/start byte/node id/search key and never on the rowid, so
// two generations with an identical answer here rank and score identically.
func lexicalAnswer(t *testing.T, f *fixture, gen model.GenerationID, terms []string) string {
	t.Helper()
	r, err := f.s.PinGeneration(f.ctx, f.repo, gen, time.Minute)
	if err != nil {
		t.Fatalf("PinGeneration(%d): %v", gen, err)
	}
	var b strings.Builder
	docs, tokens, err := r.SearchStats(f.ctx)
	if err != nil {
		t.Fatalf("SearchStats: %v", err)
	}
	fmt.Fprintf(&b, "corpus documents=%d tokens=%d\n", docs, tokens)
	dfs, err := r.DocumentFrequency(f.ctx, terms)
	if err != nil {
		t.Fatalf("DocumentFrequency: %v", err)
	}
	for i, term := range terms {
		fmt.Fprintf(&b, "term %q df=%d\n", term, dfs[i])
		ids, err := r.Match(f.ctx, `"`+term+`"`, 0, 100)
		if err != nil {
			t.Fatalf("Match(%q): %v", term, err)
		}
		// Match pages by the document id, which a carried document keeps and a
		// freshly indexed one mints; the ANSWER must not depend on either, so
		// the tuples are compared in the rank tie-break order, not in id order.
		// One posting session per term: the reader serves a term's whole
		// instance list from a single held statement now, so the stream is
		// drained once here and the per-document rows are picked out of it.
		sess, err := r.OpenPostings(f.ctx)
		if err != nil {
			t.Fatalf("OpenPostings: %v", err)
		}
		stream, err := sess.TermOccurrences(f.ctx, term)
		if err != nil {
			t.Fatalf("TermOccurrences(%q): %v", term, err)
		}
		var occ []store.TermOccurrence
		for {
			page, err := stream.Next(f.ctx, 100)
			if err != nil {
				t.Fatalf("TermOccurrences(%q) next: %v", term, err)
			}
			if page == nil {
				break
			}
			occ = append(occ, page...)
		}
		if err := stream.Close(); err != nil {
			t.Fatalf("stream close: %v", err)
		}
		if err := sess.Close(); err != nil {
			t.Fatalf("session close: %v", err)
		}
		var lines []string
		for _, id := range ids {
			d, err := r.SearchUnit(f.ctx, id)
			if err != nil {
				t.Fatalf("SearchUnit(%d): %v", id, err)
			}
			var cols []string
			for _, o := range occ {
				if o.RowID != id {
					continue
				}
				cols = append(cols, fmt.Sprintf("%s:%d@%v", o.Column, o.Count, o.Offsets))
			}
			lines = append(lines, fmt.Sprintf("  %s %s %s %q [%d,%d) len=%d key=%s %s",
				d.Path, d.Name, d.QualifiedName, d.Signature, d.Bytes.Start, d.Bytes.End, d.TokenCount, d.ID, strings.Join(cols, " ")))
		}
		slices.Sort(lines)
		b.WriteString(strings.Join(lines, "\n"))
		b.WriteString("\n")
	}
	return b.String()
}

// TestDeltaCarriesTheTextItIndexed pins the one invariant a contentless index
// cannot check for itself: a carried lexical document keeps the text it was
// indexed with.
//
// search_fts stores no body, and the body a producer published is NOT the
// source over the document's byte range -- treesitter publishes names,
// signature and documentation over the whole declaration's extent, and the
// filesystem provider publishes no body at all. So a carry-over that rebuilds
// the index from the content store indexes text that was never any document's
// body: the same tree answers differently depending on whether it was indexed
// in full or refreshed, with no failing check anywhere, because every
// integrity check a contentless index offers is internal to the index.
//
// The fixture is what makes this provable: searchDoc's Body (docBody) shares
// no token with the file's source, so re-derivation shows up as a changed hit
// set rather than as an identical one.
func TestDeltaCarriesTheTextItIndexed(t *testing.T) {
	dbPath := t.TempDir() + "/codectx.db"
	f := newFixture(t, dbPath)
	a1 := f.file("pkg/a.go", "package pkg\nfunc F() {}\n")
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
		Keys:   slices.Values(keyList("key:node:" + a2.path)),
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
	terms := []string{"zqxdocstring", "zqxsignature", "f", "package", "pkg"}
	full := lexicalAnswer(t, f, genFull, terms)
	delta := lexicalAnswer(t, f, genDelta, terms)
	if full != delta {
		t.Errorf("a delta answers differently from a full index of the same tree.\nfull:\n%s\ndelta:\n%s", full, delta)
	}
	// Every read above resolves a document by doc_id, and the posting stream does
	// not deduplicate, so a generation that saw one doc_id twice would
	// double-count offsets and hydrate an arbitrary one of the rows. Two facts
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
		t.Errorf("%d document ids are visible twice in one generation; a shared posting must never reach two member units", shared)
	}
	if err := f.s.Check(f.ctx, true); err != nil {
		t.Fatalf("deep integrity check after a delta: %v", err)
	}

	// Retiring the predecessor is where a shared posting is easiest to lose or
	// to mistake for garbage: the unit whose insert minted it is gone, while
	// the delta unit still names it. deleteUnit must keep it (it is released
	// only when NO surviving unit names it), and the deep check's vocabulary
	// scan must not read it as stale -- that scan walks doc_id, which the
	// surviving row carries, and not the rowid the retired row had.
	if err := f.s.DeleteGeneration(f.ctx, gen1); err != nil {
		t.Fatalf("DeleteGeneration(gen1): %v", err)
	}
	if err := f.s.Check(f.ctx, true); err != nil {
		t.Fatalf("deep integrity check after retiring the unit that minted the shared posting: %v", err)
	}
	if after := lexicalAnswer(t, f, genDelta, terms); after != delta {
		t.Errorf("retiring the predecessor changed the delta generation's answer.\nbefore:\n%s\nafter:\n%s", delta, after)
	}

	if !strings.Contains(full, "zqxdocstring") {
		t.Fatalf("fixture no longer exercises a body that differs from its byte range:\n%s", full)
	}
}
