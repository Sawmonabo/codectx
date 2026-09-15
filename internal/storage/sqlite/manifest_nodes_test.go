package sqlite_test

import (
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
)

// TestManifestNodeIdsRoundTripCanonicalThroughSurrogates pins the one invariant
// context_entries.node_id acquired when it became an INTEGER surrogate into
// node_ids: everything this package hands back must still name the CANONICAL
// 32-byte node id.
//
// Nothing else in the tree exercises it. The manifest fixtures elsewhere carry
// a FileID only, and the capsule's scope collector is tested against a fake
// store, so both readers could return a rebuild-local surrogate -- or nothing
// at all -- with every suite green. Both failure modes are silent past this
// package: a surrogate rendered as a node id crosses the CLI/MCP boundary as a
// plausible-looking identifier, and an empty scope page seals a capsule whose
// scope list is empty and hashes that emptiness into the capsule identity.
//
// The paged drain repeats the exact loop workflow's scope collector runs
// (workflow/capsule.go: feed the last id back as `after`), so an ordering or a
// cursor that is not on the canonical column shows up here as a short or
// looping page rather than only in a sealed capsule.
func TestManifestNodeIdsRoundTripCanonicalThroughSurrogates(t *testing.T) {
	f := newFixture(t, filepath.Join(t.TempDir(), "codectx.db"))
	a := f.file("pkg/a.go", "package pkg\nfunc F() {}\n")
	b := f.file("pkg/b.go", "package pkg\nfunc G() {}\n")
	snap := f.snapshot("manifest-nodes", a, b)
	gen, err := f.s.BeginGeneration(f.ctx, f.repo, snap.ID, model.H("semantic"), "main")
	if err != nil {
		t.Fatal(err)
	}
	run := f.run(gen)
	w := f.beginScope(gen, run, configHash, a, b)
	f.fillFile(w, run, a)
	f.fillFile(w, run, b)
	first := f.putNode(w, run, "pkg/a.go", "Alpha", &a, "key:alpha")
	second := f.putNode(w, run, "pkg/b.go", "Beta", &b, "key:beta")
	if err := f.s.SealUnit(f.ctx, w); err != nil {
		t.Fatalf("SealUnit: %v", err)
	}
	bind := f.activate(gen, 0)

	published := []model.NodeID{first, second}
	slices.Sort(published)
	manifest := model.ContextManifest{ID: model.ManifestID(model.H("manifest", "nodes")), Binding: bind,
		Phase: model.PhaseSweep, RequestHash: model.H("req"), PolicyVersion: "p1", CanonicalHash: model.H("canon"),
		EntryCount: 3, SliceCount: 1,
		Budget:         model.Budget{MaxEstimatedTokens: 1000, MaxBytes: 4096, MaxFiles: 10, MaxSlices: 2},
		Completeness:   fixtureCaps,
		ScopeComplete:  true,
		EstimateMethod: "test-estimator", CreatedAt: time.Now().UTC().Truncate(time.Microsecond)}
	entries := []model.ContextEntry{
		{Ordinal: 0, NodeID: first, FileID: a.id, Requirement: model.RequirementFull, EstimatedBytes: 1, EstimatedTokens: 1, Reasons: []string{"seed"}},
		{Ordinal: 1, NodeID: second, FileID: b.id, Requirement: model.RequirementFull, EstimatedBytes: 1, EstimatedTokens: 1, Reasons: []string{"seed"}},
		// A whole-file entry names no node and must contribute nothing to the
		// scope list while still reading back with an empty NodeID.
		{Ordinal: 2, FileID: b.id, Requirement: model.RequirementFull, EstimatedBytes: 1, EstimatedTokens: 1, Reasons: []string{"seed"}},
	}
	sl := []model.ContextSlice{{Index: 0, EntryOrdinals: []int{0, 1, 2}, EstimatedBytes: 3, EstimatedTokens: 3}}
	if err := f.s.PutManifest(f.ctx, manifest, []byte(`{"task":"t"}`), entries, sl, nil); err != nil {
		t.Fatalf("PutManifest: %v", err)
	}

	got, err := f.s.ManifestEntries(f.ctx, manifest.ID, -1, 10)
	if err != nil {
		t.Fatalf("ManifestEntries: %v", err)
	}
	if len(got) != len(entries) {
		t.Fatalf("ManifestEntries returned %d entries, want the %d written", len(got), len(entries))
	}
	for i, e := range got {
		if e.NodeID != entries[i].NodeID {
			t.Errorf("entry %d reads back node id %q, want the canonical %q that was written", i, e.NodeID, entries[i].NodeID)
		}
		if e.FileID != entries[i].FileID {
			t.Errorf("entry %d reads back file id %q, want %q", i, e.FileID, entries[i].FileID)
		}
	}

	scope, err := f.s.ManifestScopeNodes(f.ctx, manifest.ID, "", 10)
	if err != nil {
		t.Fatalf("ManifestScopeNodes: %v", err)
	}
	if !slices.Equal(scope, published) {
		t.Fatalf("ManifestScopeNodes = %v, want the %d canonical ids the entries name, ascending: %v", scope, len(published), published)
	}

	// The capsule's scope collector drains this pager one page at a time and
	// feeds the last id it received back as the cursor. A seal whose scope
	// count is zero -- or which never terminates -- is the same defect.
	var walked []model.NodeID
	after := model.NodeID("")
	for range len(published) + 2 {
		page, err := f.s.ManifestScopeNodes(f.ctx, manifest.ID, after, 1)
		if err != nil {
			t.Fatalf("ManifestScopeNodes(after=%q): %v", after, err)
		}
		if len(page) == 0 {
			break
		}
		walked = append(walked, page...)
		after = page[len(page)-1]
	}
	if len(walked) == 0 {
		t.Fatal("the paged scope walk collected 0 nodes; every sealed capsule's scope list would be empty")
	}
	if !slices.Equal(walked, published) {
		t.Fatalf("the paged scope walk collected %v, want %v", walked, published)
	}
}
