package graph

import (
	"context"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
)

// nestedCallNodes are the nested-call fixture's nodes: f's parameter pf, its
// local k and its return node retf; g's parameter q, its local n and its
// return node retg; aA, f's argument at another site; a3 and res3, g's
// argument and result at another site; x, y, z a chain into retf from
// outside any call.
var nestedCallNodes = []string{"aA", "pf", "k", "retf", "q", "n", "retg", "a3", "res3", "x", "y", "z"}

// nestedCallImpact is the incoming impact of seeds over `g(f())`: f's return
// node flows into g's parameter by one relation carrying both roles (the
// linker's nested-call flow), each role of roles[2] as given.
func nestedCallImpact(t *testing.T, nested FlowRole, seeds ...string) map[string]bool {
	t.Helper()
	nodes := map[string]model.NodeKind{}
	for _, n := range nestedCallNodes {
		nodes[n] = model.NodeVariable
	}
	flow := string(model.RelDataFlowsTo)
	f := dependenceFixture(nodes, [][3]string{
		{"aA", flow, "pf"}, {"pf", flow, "retf"}, // 0 argument, 1
		{"retf", flow, "q"},                   // 2: the nested call's flow
		{"q", flow, "n"}, {"n", flow, "retg"}, // 3, 4
		{"a3", flow, "q"}, {"retg", flow, "res3"}, // 5 argument, 6 return
		{"k", flow, "retf"},                                     // 7
		{"x", flow, "y"}, {"y", flow, "z"}, {"z", flow, "retf"}, // 8, 9, 10
	})
	g := memGraphFor(f).WithFlowRoles(map[model.RelationID]FlowRole{
		fixtureRelationID(0): {Argument: true}, fixtureRelationID(2): nested,
		fixtureRelationID(5): {Argument: true}, fixtureRelationID(6): {Return: true},
	})
	signer, err := pagination.OpenSigner(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store := newFixtureLeases()
	spools, err := pagination.NewSpools(t.TempDir(), 64<<20, store)
	if err != nil {
		t.Fatal(err)
	}
	limits := fixtureLimits()
	limits.MaxDepth, limits.MaxVisited, limits.MaxEdges = 0, 0, 0
	e, err := New(Options{Adjacency: f, Reader: g, Signer: signer, Spools: spools,
		Leases: pagination.NewLeases(store, limits.CursorTTL), Limits: limits})
	if err != nil {
		t.Fatal(err)
	}
	start := make([]model.NodeID, 0, len(seeds))
	for _, s := range seeds {
		start = append(start, fixtureNodeID(s))
	}
	res, err := e.Impact(context.Background(), model.ImpactRequest{GenerationID: 1, Start: start,
		Direction: model.DirectionIncoming})
	if err != nil {
		t.Fatal(err)
	}
	if res.Meta.Truncated {
		t.Fatalf("the impact of %v is truncated (%q)", seeds, res.Meta.TruncationReason)
	}
	got := map[string]bool{}
	for _, en := range res.Entries {
		for _, name := range nestedCallNodes {
			if en.NodeID == fixtureNodeID(name) {
				got[name] = true
			}
		}
	}
	return got
}

// TestImpactThroughANestedCall pins the two-phase rule over a nested call's
// flow, which leaves f and enters g: a value f creates reaches g's body but
// never another site's result of g; a value that entered f through another
// call's argument never leaves f through it; and a node of f first reached
// inside a call and then lifted takes the flow into g still inside g.
//
// Mutations: the flow published as a return edge alone -> k reaches res3 through
// g's return; as an argument edge alone -> aA, inside f, leaves through it to
// q; a lifted route's record taking it reset to undescended
// (settleSlice's leaving case ignoring the entering role) -> aA and x reach res3.
func TestImpactThroughANestedCall(t *testing.T) {
	both := FlowRole{Argument: true, Return: true}
	cases := []struct {
		name      string
		role      FlowRole
		seeds     []string
		want, not []string
	}{
		{"a value f creates", both, []string{"k"}, []string{"q", "n", "retg"}, []string{"res3"}},
		{"a value that entered f elsewhere", both, []string{"aA"}, []string{"pf", "retf"}, []string{"q", "res3"}},
		{"a lifted node of f", both, []string{"aA", "x"}, []string{"retf", "q", "retg"}, []string{"res3"}},
	}
	for _, c := range cases {
		got := nestedCallImpact(t, c.role, c.seeds...)
		for _, n := range c.want {
			if !got[n] {
				t.Errorf("%s: impact of %v = %v, want %s", c.name, c.seeds, got, n)
			}
		}
		for _, n := range c.not {
			if got[n] {
				t.Errorf("%s: impact of %v = %v reaches %s, an unrealizable path", c.name, c.seeds, got, n)
			}
		}
	}
}
