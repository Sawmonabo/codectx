package link_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
)

const returnWalkSrc = `def f(p):
    k = 1
    y = p + k
    return y


def s1(a1):
    r1 = f(a1)
    print(r1)


def s2(a2):
    r2 = f(a2)
    print(r2)


def g(p):
    k = 1
    if p:
        return k
    return 0


def t1(b1):
    q1 = g(b1)
    print(q1)


def t2(b2):
    q2 = g(b2)
    print(q2)
`

// TestReturnFlowRoles pins the dependence edges both providers publish for a
// callee called at two sites, each data_flows_to with exactly one role an
// impact walk reads from its evidence: the argument flows into the callee's
// parameter, the summary from each argument to its own site's result, the
// callee's every returned value into its return node as a reaching
// definition, and a return edge from the return node to each site's result.
// A walk that starts inside the callee, from a value no parameter's flow
// creates, reaches every site's result.
//
// Mutation caught: leaving a value out of the return node because a
// parameter decides whether it is returned (g's k, gated by p) leaves the
// walk from k short of q1 and q2; mutation: leaving out a value a parameter
// reaches (f's y) loses f.y -> f.return; mutation: publishing the summary at
// every bound site of the callee gives a1 -> r2.
func TestReturnFlowRoles(t *testing.T) {
	l := linkSources(t, newStructural(t), map[string]string{"w.py": returnWalkSrc})
	names := map[model.NodeID]string{}
	ids := map[string]model.NodeID{}
	var rels []model.RelationFact
	for _, c := range []*capture{l.structural, l.out} {
		for id, n := range c.nodes {
			names[id] = n.QualifiedName
			ids[n.QualifiedName] = id
		}
		rels = append(rels, c.rels...)
	}
	role := func(d string) string {
		if strings.HasPrefix(d, "summary:") {
			return "summary"
		}
		return d
	}
	edges := map[string]bool{}
	next := map[model.NodeID][]model.NodeID{}
	for _, r := range rels {
		if r.Relation.Kind != model.RelDataFlowsTo {
			continue
		}
		from, to := names[r.Relation.From], names[r.Relation.To]
		var roles []string
		for _, e := range r.Evidence {
			roles = append(roles, role(e.Detail))
		}
		slices.Sort(roles)
		if roles = slices.Compact(roles); len(roles) != 1 {
			t.Fatalf("%s -> %s carries the roles %q, want one", from, to, roles)
		}
		edges[from+" "+roles[0]+" "+to] = true
		next[r.Relation.From] = append(next[r.Relation.From], r.Relation.To)
	}
	for _, want := range []string{
		"s1.a1 argument f.p", "s1.a1 summary s1.r1", "s2.a2 summary s2.r2",
		"f.y reaching_def f.return", "f.return return s1.r1", "f.return return s2.r2",
		"g.k reaching_def g.return", "g.return return t1.q1", "g.return return t2.q2",
		"t1.b1 summary t1.q1", "t2.b2 summary t2.q2",
	} {
		if !edges[want] {
			t.Fatalf("no edge %q among %v", want, edges)
		}
	}
	for _, wrong := range []string{"s1.a1 summary s2.r2", "s2.a2 summary s1.r1"} {
		if edges[wrong] {
			t.Fatalf("edge %q joins one site's argument to another site's result", wrong)
		}
	}
	reached := map[model.NodeID]bool{ids["g.k"]: true}
	stack := []model.NodeID{ids["g.k"]}
	for len(stack) > 0 {
		n := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		for _, m := range next[n] {
			if !reached[m] {
				reached[m] = true
				stack = append(stack, m)
			}
		}
	}
	if !reached[ids["t1.q1"]] || !reached[ids["t2.q2"]] {
		t.Fatalf("a walk from g's k reaches q1 %v, q2 %v; want both", reached[ids["t1.q1"]], reached[ids["t2.q2"]])
	}
}
