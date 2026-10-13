package sqlite_test

import (
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
)

// TestManifestRoutesArePagedByEntryAndIndex pins how a manifest stores and
// serves its evidence routes: an entry reads back with its route count and no
// routes, and the routes view pages every route of every entry, in (entry,
// index) order, through pages that split one entry's routes.
//
// Mutations caught: a keyset on the entry ordinal alone (the page after a
// split resumes past the rest of that entry's routes, so routes are lost); the
// entries read still carrying the routes (an entry is then served with what
// its charge leaves out); the count not stored (it reads back zero).
func TestManifestRoutesArePagedByEntryAndIndex(t *testing.T) {
	f := newFixture(t, filepath.Join(t.TempDir(), "codectx.db"))
	a := f.file("pkg/a.go", "package pkg\nfunc F() {}\n")
	snap := f.snapshot("manifest-routes", a)
	gen, err := f.s.BeginGeneration(f.ctx, f.repo, snap.ID, model.H("semantic"), "main")
	if err != nil {
		t.Fatal(err)
	}
	run := f.run(gen)
	w := f.beginScope(gen, run, configHash, a)
	f.fillFile(w, run, a)
	if err := f.s.SealUnit(f.ctx, w); err != nil {
		t.Fatalf("SealUnit: %v", err)
	}
	bind := f.activate(gen, 0)
	rel := func(n string) model.RelationID { return model.RelationID(model.H("relation", n)) }
	routes := [][][]model.RelationID{
		{{rel("a")}, {rel("b"), rel("c")}, {rel("d")}},
		{},
		{{rel("e")}},
	}
	manifest := model.ContextManifest{ID: model.ManifestID(model.H("manifest", "routes")), Binding: bind,
		Phase: model.PhaseSweep, RequestHash: model.H("req"), PolicyVersion: "p1", CanonicalHash: model.H("canon"),
		EntryCount: 3, SliceCount: 1,
		Budget:         model.Budget{MaxEstimatedTokens: 1000, MaxBytes: 4096, MaxFiles: 10, MaxSlices: 2},
		Completeness:   fixtureCaps,
		ScopeComplete:  true,
		EstimateMethod: "test-estimator", CreatedAt: time.Now().UTC().Truncate(time.Microsecond)}
	var entries []model.ContextEntry
	for i, r := range routes {
		entries = append(entries, model.ContextEntry{Ordinal: i, FileID: a.id, Requirement: model.RequirementFull,
			EstimatedBytes: 1, EstimatedTokens: 1, Reasons: []string{"seed"}, EvidencePaths: r,
			RouteCount: int64(len(r))})
	}
	sl := []model.ContextSlice{{Index: 0, EntryOrdinals: []int{0, 1, 2}, EstimatedBytes: 3, EstimatedTokens: 3}}
	if err := f.s.PutManifest(f.ctx, manifest, []byte(`{"task":"t"}`), entries, sl, nil); err != nil {
		t.Fatalf("PutManifest: %v", err)
	}
	got, err := f.s.ManifestEntries(f.ctx, manifest.ID, -1, 10)
	if err != nil {
		t.Fatalf("ManifestEntries: %v", err)
	}
	for i, e := range got {
		if e.RouteCount != int64(len(routes[i])) || len(e.EvidencePaths) != 0 {
			t.Errorf("entry %d reads back %d routes counted and %d carried, want %d counted and none carried",
				i, e.RouteCount, len(e.EvidencePaths), len(routes[i]))
		}
	}
	var read []model.ContextRoute
	afterEntry, afterIndex := -1, -1
	for pages := 0; ; pages++ {
		if pages > 10 {
			t.Fatal("the routes keyset does not advance")
		}
		page, err := f.s.ManifestRoutes(f.ctx, manifest.ID, afterEntry, afterIndex, 2)
		if err != nil {
			t.Fatalf("ManifestRoutes: %v", err)
		}
		if len(page) == 0 {
			break
		}
		read = append(read, page...)
		afterEntry, afterIndex = page[len(page)-1].Entry, page[len(page)-1].Index
	}
	var want []model.ContextRoute
	for i, r := range routes {
		for j, p := range r {
			want = append(want, model.ContextRoute{Entry: i, Index: j, Relations: p})
		}
	}
	if !slices.EqualFunc(read, want, func(x, y model.ContextRoute) bool {
		return x.Entry == y.Entry && x.Index == y.Index && slices.Equal(x.Relations, y.Relations)
	}) {
		t.Fatalf("the routes view read %+v, want %+v", read, want)
	}
}
