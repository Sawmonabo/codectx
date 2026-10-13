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
`

// TestReturnFlowWalkIsRealizable walks the published dependence edges of
// both providers the way an impact walk does, with no call stack: along
// data_flows_to, and from a controlling variable to the one that
// control-depends on it. A callee f(p) called at two sites must give each
// site its argument back only through the summary at that site: a walk from
// a1 reaches r1 and never r2, while f's created k reaches both results
// through f's return node.
//
// Mutation caught: a return edge published from a variable a parameter
// reaches (y, which p reaches, flowing into f's return node) makes the walk
// from a1 reach r2.
func TestReturnFlowWalkIsRealizable(t *testing.T) {
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
	next := map[model.NodeID][]model.NodeID{}
	summary := map[[2]string]bool{}
	var leaks []string
	for _, r := range rels {
		from, to := r.Relation.From, r.Relation.To
		switch r.Relation.Kind {
		case model.RelDataFlowsTo:
			next[from] = append(next[from], to)
			for _, e := range r.Evidence {
				if strings.HasPrefix(e.Detail, "summary:") {
					summary[[2]string{names[from], names[to]}] = true
				}
			}
			if dst := names[to]; (dst == "s1.r1" || dst == "s2.r2" || dst == "f.return") && (names[from] == "f.y" || names[from] == "f.p") {
				leaks = append(leaks, names[from]+" -> "+dst)
			}
		case model.RelControlDependsOn:
			next[to] = append(next[to], from)
		}
	}
	for _, s := range [][2]string{{"s1.a1", "s1.r1"}, {"s2.a2", "s2.r2"}} {
		if !summary[s] {
			t.Fatalf("no summary edge %s -> %s among %v", s[0], s[1], summary)
		}
	}
	walk := func(start string) []string {
		id, ok := ids[start]
		if !ok {
			t.Fatalf("no node %s", start)
		}
		seen := map[model.NodeID]bool{id: true}
		stack := []model.NodeID{id}
		var reached []string
		for len(stack) > 0 {
			n := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			for _, m := range next[n] {
				if !seen[m] {
					seen[m] = true
					reached = append(reached, names[m])
					stack = append(stack, m)
				}
			}
		}
		return reached
	}
	if got := walk("s1.a1"); !slices.Contains(got, "s1.r1") || slices.Contains(got, "s2.r2") {
		t.Fatalf("a walk from a1 reaches %q; want r1 and never r2", got)
	}
	if got := walk("f.k"); !slices.Contains(got, "s1.r1") || !slices.Contains(got, "s2.r2") {
		t.Fatalf("a walk from f's created k reaches %q; want both results", got)
	}
	if len(leaks) > 0 {
		t.Fatalf("parameter-reached values flow into a result or the return node: %q", leaks)
	}
}
