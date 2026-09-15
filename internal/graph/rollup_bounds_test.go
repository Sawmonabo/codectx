package graph

import (
	"context"
	"strings"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
)

// TestRollupContainmentIsUnboundedByDefaultAndOmittedWhenBounded protects the
// one invariant the deleted `containers_per_node` refusal broke: a rollup whose
// containment read outruns a hard-coded per-node ceiling used to fail the whole
// query on a DEFAULT configuration. The bound is the configured edge allowance
// now -- unlimited by default -- and a user-set allowance omits the nodes it
// stopped on and says so, instead of refusing.
//
// Mutation proof: restore `config.Limit(int64(len(batch))*16)` as the bound in
// containerPackages and the unlimited leg still passes (16 per node is enough
// for this fixture), but restore the `CTX_RESOURCE_LIMIT` return in place of
// cut(batch) and the bounded leg fails with that error instead of an answer.
func TestRollupContainmentIsUnboundedByDefaultAndOmittedWhenBounded(t *testing.T) {
	f := newGraphFixture(t)
	engine := func(t *testing.T, maxEdges int) *Engine {
		t.Helper()
		signer, err := pagination.OpenSigner(t.TempDir())
		if err != nil {
			t.Fatalf("open signer: %v", err)
		}
		store := newFixtureLeases()
		spools, err := pagination.NewSpools(t.TempDir(), 8<<20, store)
		if err != nil {
			t.Fatalf("new spools: %v", err)
		}
		limits := fixtureLimits()
		limits.MaxDepth, limits.MaxVisited = 0, 0
		limits.MaxEdges = maxEdges
		e, err := New(Options{Adjacency: f, Signer: signer, Spools: spools,
			Leases: pagination.NewLeases(store, limits.CursorTTL), Limits: limits})
		if err != nil {
			t.Fatalf("new engine: %v", err)
		}
		return e
	}
	req := model.GraphRequest{GenerationID: 1, Start: []model.NodeID{fixtureNodeID("n-wide")},
		Direction: model.DirectionOutgoing, Relations: []model.RelationKind{model.RelCalls}}

	t.Run("unlimited edges roll up every pair", func(t *testing.T) {
		page, err := engine(t, 0).PackageDependencies(context.Background(), req)
		if err != nil {
			t.Fatalf("a default (unlimited) edge allowance refused the rollup: %v", err)
		}
		if len(page.Items) == 0 {
			t.Fatalf("the unlimited rollup produced no pairs; the fixture cannot prove anything")
		}
		for _, n := range page.Meta.Notices {
			if strings.Contains(n, "max_graph_edges") {
				t.Fatalf("an unlimited edge allowance disclosed a cut containment read: %q", n)
			}
		}
	})

	t.Run("a user-set edge bound omits and discloses", func(t *testing.T) {
		// One edge: the walk admits it, and the containment read of its
		// endpoints has no allowance left, so every candidate is dropped.
		page, err := engine(t, 1).PackageDependencies(context.Background(), req)
		if err != nil {
			t.Fatalf("a user-set edge allowance refused the rollup instead of omitting: %v", err)
		}
		if !page.Meta.Truncated {
			t.Fatalf("a cut containment read left the answer marked complete")
		}
		var disclosed bool
		for _, n := range page.Meta.Notices {
			disclosed = disclosed || strings.Contains(n, "max_graph_edges")
		}
		if !disclosed {
			t.Fatalf("a cut containment read named no bound; notices = %v", page.Meta.Notices)
		}
	})
}
