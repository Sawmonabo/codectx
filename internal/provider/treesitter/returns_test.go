package treesitter_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider"
	"github.com/Sawmonabo/codectx/internal/provider/link"
	"github.com/Sawmonabo/codectx/internal/provider/providertest"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/lang"
)

const returnFlowSrc = `def mix(p):
    k = 1
    y = p + k
    return y


def make():
    v = Box()
    return v


def wrap(x):
    return make()


def use():
    a = make()
    print(make())
    return a


def gate(p):
    y = 0
    if p:
        y = 1
    return y


def choose(p):
    if p:
        return 1
    return 2
`

// TestReturnFlow pins the structural half of return-value flow on one
// Python file: which parameters reach their callable's return, which values
// created inside a callable flow into its return node, and where each call's
// value goes. The names come from the source; the walk rule they protect is
// that a value a parameter reaches never flows into the return node, so no
// walk enters through one call's argument and leaves through another's
// result.
func TestReturnFlow(t *testing.T) {
	p := newProvider(t)
	h := providertest.New(t, map[string]string{"r.py": returnFlowSrc})
	u := h.Plan(t, p, lang.ScopePrefix+"r.py", []string{"r.py"})
	c := &tableCapture{capture: &capture{UnitOutput: h.Begin(t, u, []string{"r.py"})}}
	result, err := provider.RunUnit(context.Background(), p, u.Request, c, providertest.Limits, h.Pool)
	if err != nil || result.State != model.RunSucceeded {
		t.Fatalf("RunUnit: %v, %s", err, result.State)
	}
	tbl, err := link.DecodeTable(c.table)
	if err != nil {
		t.Fatal(err)
	}
	names := map[model.NodeID]string{}
	for _, n := range c.nodes {
		names[n.Node.ID] = n.Node.QualifiedName
	}
	var flows []string
	for _, r := range c.rels {
		for _, e := range r.Evidence {
			if r.Relation.Kind == model.RelDataFlowsTo && e.Detail == "return" {
				flows = append(flows, names[r.Relation.From]+" -> "+names[r.Relation.To])
			}
		}
	}
	slices.Sort(flows)
	// mutation: drop the exclusion of values a parameter reaches (y, which p
	// reaches, would flow into mix's return node); mutation: drop the
	// boundary flow into a parameter-reached variable (k would not);
	// mutation: follow flows only, not control dependence (gate's y, which
	// p decides, would flow into gate's return node).
	want := []string{"make.v -> make.return", "mix.k -> mix.return", "use.a -> use.return"}
	if !slices.Equal(flows, want) {
		t.Fatalf("created values into return nodes %q, want %q", flows, want)
	}

	decl := func(name string) *link.Decl {
		i := slices.IndexFunc(tbl.Decls, func(d link.Decl) bool { return d.Name == name })
		if i < 0 {
			t.Fatalf("no declaration %s", name)
		}
		return &tbl.Decls[i]
	}
	// mutation: mark every parameter returned (wrap's x reaches no return);
	// mutation: follow flows only (gate's p, which decides y, and choose's
	// p, which decides which return runs, would not be returned).
	for name, want := range map[string]bool{"mix": true, "wrap": false, "gate": true, "choose": true} {
		if got := decl(name).Parameters[0].Returned; got != want {
			t.Fatalf("%s's parameter returned %v, want %v", name, got, want)
		}
	}
	for _, name := range []string{"mix", "make", "wrap", "use", "gate", "choose"} {
		if decl(name).Return == nil || decl(name).Return.Node == "" {
			t.Fatalf("%s has no return node", name)
		}
	}

	var sites []string
	for _, s := range tbl.Sites {
		if s.Callee != "make" {
			continue
		}
		var into string
		if s.Into > 0 {
			into = " into " + tbl.Decls[s.Into-1].Name
		}
		var results []string
		for _, r := range s.Results {
			results = append(results, r.Name)
		}
		sites = append(sites, strings.Join(results, ",")+into)
	}
	slices.Sort(sites)
	// mutation: take the innermost node's definitions even when another call
	// consumes the value first (`print(make())` would write into nothing,
	// but a node defining a variable around it would claim it); mutation:
	// never set Into (wrap's `return make()` would lose its way back).
	if want := []string{"", " into wrap", "a"}; !slices.Equal(sites, want) {
		t.Fatalf("make's call sites write into %q, want %q", sites, want)
	}
}
