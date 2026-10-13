package link_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
)

const nestedCallSrc = `def f():
    v = 1
    return v


def g(p):
    return p


def h():
    x = g(f())
    print(x)


def k():
    return g(f())


class C:
    def m(self):
        return self


def mk():
    c = C()
    return c


def u():
    y = mk().m()
    print(y)
`

// TestNestedCallResults pins where a call's value goes when another call
// consumes it directly: from the callee's return node into the parameter
// the argument binds (one relation carrying the argument and the return
// role), into the receiver parameter for a receiver, and on, through the
// consuming callee's summary, to the consuming call's result or the return
// node it reaches.
//
// Mutations: no nested flows (siteResults records nothing) -> no f.return
// edge into g.p; the nested flow carrying one role -> its roles are not
// both; the summary leg left out -> f.return does not reach h.x; the
// summary read for a result only -> f.return does not reach k.return; the
// receiver not followed -> no mk.return edge into C.m.self.
func TestNestedCallResults(t *testing.T) {
	l := linkSources(t, newStructural(t), map[string]string{"n.py": nestedCallSrc})
	names := map[model.NodeID]string{}
	for _, c := range []*capture{l.structural, l.out} {
		for id, n := range c.nodes {
			names[id] = n.QualifiedName
		}
	}
	edges := map[string][]string{}
	for _, c := range []*capture{l.structural, l.out} {
		for _, r := range c.rels {
			if r.Relation.Kind != model.RelDataFlowsTo {
				continue
			}
			key := names[r.Relation.From] + " -> " + names[r.Relation.To]
			for _, e := range r.Evidence {
				d := e.Detail
				if strings.HasPrefix(d, "summary:") {
					d = "summary"
				}
				if !slices.Contains(edges[key], d) {
					edges[key] = append(edges[key], d)
				}
			}
		}
	}
	for key := range edges {
		slices.Sort(edges[key])
	}
	for edge, want := range map[string][]string{
		"f.return -> g.p":       {"argument", "return"},
		"f.return -> h.x":       {"return"},
		"f.return -> k.return":  {"return"},
		"mk.return -> C.m.self": {"argument", "return"},
		"mk.return -> u.y":      {"return"},
		"g.return -> h.x":       {"return"},
		"C.m.return -> u.y":     {"return"},
	} {
		if !slices.Equal(edges[edge], want) {
			t.Errorf("%s carries %q, want %q; the flows are %v", edge, edges[edge], want, edges)
		}
	}
}
