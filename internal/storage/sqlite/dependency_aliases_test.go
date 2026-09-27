package sqlite_test

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
)

// A unit's candidates resolve against a table of its dependencies' aliases,
// loaded once from the last commit. Three silent failures would each mint an
// identity other than the one the committed dependency names, with every run
// still reported successful:
//
//   - A dependency sealed in the open ingestion group but not committed is
//     answered as aliased to nothing. Mutation: read the table through the
//     group (readOwn), or drop the visibility check (requireSealed).
//   - A requested dependency that is not sealed (here: committed building) is
//     read like a sealed one. Mutation: drop the visibility check. With that
//     check present, dropping the `u.state = 'sealed'` join predicate alone is
//     masked, because no unsealed unit ever reaches the alias query.
//   - Two identities that tie on the canonical key come back in the order
//     their rows were written. Mutation: drop the node-id tie-break in
//     sortAliases. The identity with the larger node id is interned first, so
//     row order is the reverse of the canonical order and the mutation shows.
//
// The table must also answer every key with exactly the identities the sealed
// dependency aliased it to.
//
// The group is assumed to stay open across the seal: the fixture writes a few
// rows, far below what spills the writer's cache, and no exclusive writer
// waits.
func TestDependencyAliasesAnswersCommittedSealedDependenciesOnly(t *testing.T) {
	f := newFixture(t, filepath.Join(t.TempDir(), "codectx.db"))
	ctx := f.ctx

	a := f.file("pkg/a.go", "package pkg\nfunc F() {}\nfunc G() {}\n")
	b := f.file("pkg/b.go", "package pkg\nvar F = 1\n")
	snap := f.snapshot("dependency-aliases", a, b)
	gen, err := f.s.BeginGeneration(ctx, f.repo, snap.ID, model.H("semantic"), "main")
	if err != nil {
		t.Fatalf("BeginGeneration: %v", err)
	}
	run := f.run(gen)

	// The sealed dependency: F aliased to a function and a class that share one
	// canonical key, G aliased to one more identity.
	wa := f.begin(gen, run, a)
	fn := f.nodeFact(wa, run, a.path, "F", &a)
	class := withKind(f, fn, model.NodeClass)
	first, second := fn, class
	if first.Node.ID < second.Node.ID {
		first, second = second, first
	}
	g := f.nodeFact(wa, run, a.path, "G", &a)
	for _, fact := range []model.NodeFact{first, second, g} {
		if err := wa.PutKeyedNodes(ctx, []model.NodeFact{fact}, [][]string{keyList()}); err != nil {
			t.Fatalf("PutKeyedNodes(%s %s): %v", fact.Node.Kind, fact.Node.Name, err)
		}
	}
	if err := wa.PutAliases(ctx, []model.NativeAlias{
		{ScopeKey: a.path, NativeKey: "F", NodeID: first.Node.ID},
		{ScopeKey: a.path, NativeKey: "F", NodeID: second.Node.ID},
		{ScopeKey: a.path, NativeKey: "G", NodeID: g.Node.ID},
	}); err != nil {
		t.Fatalf("PutAliases: %v", err)
	}
	if err := f.s.SealUnit(ctx, wa); err != nil {
		t.Fatalf("SealUnit: %v", err)
	}
	sealed := wa.UnitID()

	if _, err := f.s.DependencyAliases(ctx, []model.UnitID{sealed}); err == nil {
		t.Fatal("DependencyAliases answered a dependency sealed only in the uncommitted group")
	} else {
		wantCode(t, err, model.CodeArgumentInvalid)
		if !strings.Contains(err.Error(), string(sealed)) {
			t.Fatalf("the refusal %q does not name the dependency %s", err, sealed)
		}
	}

	// A second unit aliases F to its own identity and is committed building.
	wb := f.begin(gen, run, b)
	other := f.nodeFact(wb, run, b.path, "F", &b)
	if err := wb.PutKeyedNodes(ctx, []model.NodeFact{other}, [][]string{keyList()}); err != nil {
		t.Fatalf("PutKeyedNodes(unsealed): %v", err)
	}
	if err := wb.PutAliases(ctx, []model.NativeAlias{{ScopeKey: a.path, NativeKey: "F", NodeID: other.Node.ID}}); err != nil {
		t.Fatalf("PutAliases(unsealed): %v", err)
	}
	flushed(t, f.s)
	building := wb.UnitID()

	if _, err := f.s.DependencyAliases(ctx, []model.UnitID{sealed, building}); err == nil {
		t.Fatal("DependencyAliases answered a dependency that is not sealed")
	} else {
		wantCode(t, err, model.CodeArgumentInvalid)
		if !strings.Contains(err.Error(), string(building)) {
			t.Fatalf("the refusal %q does not name the unsealed dependency %s", err, building)
		}
	}

	table, err := f.s.DependencyAliases(ctx, []model.UnitID{sealed})
	if err != nil {
		t.Fatalf("DependencyAliases after the commit: %v", err)
	}
	// Each key answers exactly the identities the sealed dependency aliased it
	// to, in canonical order: F's two tie on the canonical key and so come in
	// node-id order, and the building unit's alias of F is absent.
	low, high := min(first.Node.ID, second.Node.ID), max(first.Node.ID, second.Node.ID)
	for key, want := range map[string][]model.NodeID{"F": {low, high}, "G": {g.Node.ID}} {
		got, err := table.LookupAliases(ctx, []model.UnitID{sealed}, a.path, key)
		if err != nil {
			t.Fatalf("table LookupAliases(%s): %v", key, err)
		}
		ids := make([]model.NodeID, 0, len(got))
		for _, alias := range got {
			ids = append(ids, alias.NodeID)
		}
		if !slices.Equal(ids, want) {
			t.Fatalf("%s: the table answers %v, want %v (the building unit's alias is %s)", key, ids, want, other.Node.ID)
		}
	}
	tied, err := table.LookupAliases(ctx, []model.UnitID{sealed}, a.path, "F")
	if err != nil {
		t.Fatalf("table LookupAliases(F): %v", err)
	}
	if len(tied) != 2 || tied[0].CanonicalKey != tied[1].CanonicalKey || tied[0].NodeID >= tied[1].NodeID {
		t.Fatalf("F answers %v, want the two identities tied on one canonical key in node-id order", tied)
	}
}

// withKind is fact published under another kind: the same canonical key, and
// the node id and evidence that key and kind derive.
func withKind(f *fixture, fact model.NodeFact, kind model.NodeKind) model.NodeFact {
	fact.Node.Kind = kind
	fact.Node.ID = model.NewNodeID(f.repo, kind, fact.CanonicalKey)
	ev := fact.Evidence[0]
	ev.NodeID = fact.Node.ID
	ev.ID = model.NewEvidenceID(ev)
	fact.Evidence = []model.Evidence{ev}
	return fact
}
