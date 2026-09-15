package sqlite_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
	store "github.com/Sawmonabo/codectx/internal/storage/sqlite"
)

// TestSealClipsEvidenceToTheConfiguredClip protects the one place a user-set
// index.max_evidence_per_fact could be silently ignored. Seal is what enforces
// the clip over the union of fresh and carried occurrences; if it kept reading
// the record ceiling instead of the number the providers emitted under, a unit
// assembled by a delta would hold more occurrences than the same unit built
// fresh, and the stored row set would depend on how the unit was assembled.
func TestSealClipsEvidenceToTheConfiguredClip(t *testing.T) {
	const clip = 3
	dbPath := filepath.Join(t.TempDir(), "codectx.db")
	ctx := context.Background()
	s, err := store.Open(ctx, dbPath, store.Options{MaxEvidencePerFact: clip})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	f := &fixture{t: t, ctx: ctx, s: s, repo: model.RepositoryID(model.H("test-repo", "1")), files: map[string]fileFixture{}}
	if err := s.EnsureRepository(ctx, f.repo, "/repo"); err != nil {
		t.Fatalf("EnsureRepository: %v", err)
	}

	a := f.file("pkg/a.go", "package pkg\nfunc F() {}\n")
	snap := f.snapshot("clip", a)
	gen, err := s.BeginGeneration(ctx, f.repo, snap.ID, model.H("semantic"), "main")
	if err != nil {
		t.Fatal(err)
	}
	run := f.run(gen)
	w := f.beginScope(gen, run, configHash, a)

	fact := f.nodeFact(w, run, "pkg/a.go", "F", &a)
	// Five distinct occurrences of the same fact: distinct ranges give
	// distinct evidence ids, so none of them is a duplicate.
	fact.Evidence = nil
	for i := range 5 {
		ev := model.Evidence{UnitID: w.UnitID(), ProviderID: providerID, ProviderVersion: providerVersion,
			OriginRunID: run, NodeID: fact.Node.ID, Precision: model.PrecisionSyntax, NativeKey: "F",
			FileID: a.id, ContentHash: a.hash,
			Range: &model.SourceRange{Start: model.Position{Byte: uint64(i), Line: 1}, End: model.Position{Byte: uint64(i) + 1, Line: 1, Column: 1}}}
		ev.ID = model.NewEvidenceID(ev)
		fact.Evidence = append(fact.Evidence, ev)
	}
	if err := w.PutKeyedNodes(ctx, []model.NodeFact{fact}, [][]string{{model.H("fixture-fact-key", "key:clip")}}); err != nil {
		t.Fatalf("PutKeyedNodes: %v", err)
	}
	if err := s.SealUnit(ctx, w); err != nil {
		t.Fatalf("SealUnit: %v", err)
	}

	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	var n int
	if err := raw.QueryRow(`SELECT count(*) FROM evidence WHERE unit_id = ?`, unitRowID(t, raw, w.UnitID())).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != clip {
		t.Errorf("sealed unit holds %d evidence rows, want the configured clip of %d", n, clip)
	}
}
