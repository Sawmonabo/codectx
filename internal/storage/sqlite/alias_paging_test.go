package sqlite_test

import (
	"path/filepath"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
	store "github.com/Sawmonabo/codectx/internal/storage/sqlite"
)

// A native key aliased to more identities than one read batch must yield every
// one of them. Before LookupAliases paged, a plain SQL `LIMIT MaxAliasLookup`
// cut the tail and returned a list that looked complete, so the resolver could
// neither report the cut nor page past it -- the class-E defect row 20 names.
//
// The batch is deliberately smaller than the alias count so the keyset loop has
// to run several times, and the identities are checked as a set: a keyset on
// canonical_key alone would skip every identity that ties on it.
func TestLookupAliasesReturnsEveryIdentityPastOneBatch(t *testing.T) {
	f := newFixture(t, filepath.Join(t.TempDir(), "codectx.db"))
	ctx := f.ctx

	a := f.file("pkg/a.go", "package pkg\nfunc A() {}\n")
	snap := f.snapshot("one", a)
	gen, err := f.s.BeginGeneration(ctx, f.repo, snap.ID, model.H("semantic"), "main")
	if err != nil {
		t.Fatalf("BeginGeneration: %v", err)
	}
	run := f.run(gen)
	w := f.begin(gen, run, a)

	const aliasCount = store.MaxAliasLookup * 3
	want := make(map[model.NodeID]bool, aliasCount)
	aliases := make([]model.NativeAlias, 0, aliasCount)
	for i := 0; i < aliasCount; i++ {
		name := "F" + string(rune('a'+i%26)) + string(rune('a'+i/26))
		id := f.putNode(w, run, a.path, name, &a, "key:node:"+name)
		want[id] = true
		aliases = append(aliases, model.NativeAlias{ScopeKey: a.path, NativeKey: "F", NodeID: id})
	}
	if err := w.PutAliases(ctx, aliases); err != nil {
		t.Fatalf("PutAliases: %v", err)
	}
	if err := f.s.SealUnit(ctx, w); err != nil {
		t.Fatalf("SealUnit: %v", err)
	}

	got, err := f.s.LookupAliases(ctx, []model.UnitID{w.UnitID()}, a.path, "F", 4)
	if err != nil {
		t.Fatalf("LookupAliases: %v", err)
	}
	if len(got) != aliasCount {
		t.Fatalf("LookupAliases returned %d identities, want all %d: the lookup still truncates", len(got), aliasCount)
	}
	for _, alias := range got {
		if !want[alias.NodeID] {
			t.Fatalf("LookupAliases returned %s, which is not an alias of this key", alias.NodeID)
		}
		delete(want, alias.NodeID)
	}
	for i := 1; i < len(got); i++ {
		if got[i-1].CanonicalKey > got[i].CanonicalKey {
			t.Fatalf("merged pages are out of canonical order at %d: %q then %q",
				i, got[i-1].CanonicalKey, got[i].CanonicalKey)
		}
	}
}
