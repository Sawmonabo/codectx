package reconcile_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/reconcile"
	"github.com/Sawmonabo/codectx/internal/storage/sqlite"
)

// aliasesFor returns n distinct stored aliases in canonical-key order.
type fixedAliases []sqlite.StoredAlias

func (f fixedAliases) LookupAliases(_ context.Context, _ []model.UnitID, _, _ string, limit int) ([]sqlite.StoredAlias, error) {
	if limit < len(f) {
		return f[:limit], nil
	}
	return f, nil
}

// TestManyEquallySupportedIdentitiesResolveInsteadOfFailingTheUnit protects the
// scale-posture rule that a repository property never fails an analysis unit.
//
// Mutation that fails it: return CTX_PROVIDER_OUTPUT_INVALID for a native key
// aliased to more identities than model.MaxAmbiguousCandidates. One heavily
// overloaded symbol name then refuses the whole dependency output — the unit
// produces nothing, not even the facts that had no ambiguity at all. The
// alias lookup's own page size is the only bound; the resolver keeps every
// alternative it returned so no may_refer_to edge is silently lost.
func TestManyEquallySupportedIdentitiesResolveInsteadOfFailingTheUnit(t *testing.T) {
	const n = sqlite.MaxAliasLookup // more than MaxAmbiguousCandidates+1
	if n <= model.MaxAmbiguousCandidates+1 {
		t.Fatalf("fixture is not over the ambiguity threshold: %d", n)
	}
	var store fixedAliases
	for i := 0; i < n; i++ {
		id := model.NodeID(fmt.Sprintf("%064x", i+1))
		store = append(store, sqlite.StoredAlias{NodeID: id, Kind: model.NodeFunction,
			CanonicalKey: fmt.Sprintf("go|pkg|Sym|%03d", i)})
	}
	repo := model.RepositoryID(fmt.Sprintf("%064x", 0xabc))
	unit := model.UnitID(fmt.Sprintf("%064x", 0xdef))
	r, err := reconcile.New(store, repo, []model.UnitID{unit})
	if err != nil {
		t.Fatalf("build the resolver: %v", err)
	}
	res, err := r.Resolve(context.Background(), model.NodeCandidate{
		ProviderID: "decl", ScopeKey: "pkg", NativeKey: "Sym", Kind: model.NodeFunction,
		Name: "Sym", QualifiedName: "pkg.Sym", FileID: model.FileID(fmt.Sprintf("%064x", 7)),
		ContentHash: fmt.Sprintf("%064x", 8),
		Range:       &model.SourceRange{Start: model.Position{Byte: 0, Line: 1}, End: model.Position{Byte: 1, Line: 1}},
	})
	if err != nil {
		t.Fatalf("%d equally supported identities failed the unit: %v", n, err)
	}
	if got, want := len(res.Ambiguous), n-1; got != want {
		t.Fatalf("resolution retained %d alternatives, want all %d: an alternative was dropped silently", got, want)
	}
	if res.Basis != model.MatchNativeKey {
		t.Fatalf("basis %q, want %q", res.Basis, model.MatchNativeKey)
	}
}
