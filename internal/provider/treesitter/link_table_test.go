package treesitter_test

import (
	"context"
	"strings"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider"
	"github.com/Sawmonabo/codectx/internal/provider/link"
	"github.com/Sawmonabo/codectx/internal/provider/providertest"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/lang"
)

// tableCapture records the facts a unit persisted and the link table it
// stored.
type tableCapture struct {
	*capture
	table []byte
}

func (c *tableCapture) PutState(ctx context.Context, kind string, payload []byte) error {
	if kind == link.TableKind {
		c.table = append([]byte(nil), payload...)
	}
	return c.capture.PutState(ctx, kind, payload)
}

// indexedTable runs the structural provider over one file and returns the
// nodes it published and the link table it stored.
func indexedTable(t *testing.T, p *treesitter.Provider, file, src string) ([]model.NodeFact, *link.Table) {
	t.Helper()
	h := providertest.New(t, map[string]string{file: src})
	u := h.Plan(t, p, lang.ScopePrefix+file, []string{file})
	c := &tableCapture{capture: &capture{UnitOutput: h.Begin(t, u, []string{file})}}
	result, err := provider.RunUnit(context.Background(), p, u.Request, c, providertest.Limits, h.Pool)
	if err != nil {
		t.Fatalf("RunUnit(%s): %v", file, err)
	}
	if result.State != model.RunSucceeded {
		t.Fatalf("run state for %s = %s, want succeeded", file, result.State)
	}
	if c.table == nil {
		t.Fatalf("%s stored no link table", file)
	}
	tbl, err := link.DecodeTable(c.table)
	if err != nil {
		t.Fatal(err)
	}
	return c.nodes, tbl
}

// variableAt is the one variable node published at the identifier that
// starts at byte start, failing on none or several.
func variableAt(t *testing.T, nodes []model.NodeFact, start int) model.NodeID {
	t.Helper()
	var found []model.NodeID
	for _, n := range nodes {
		if n.Node.Kind == model.NodeVariable && n.Node.Range != nil && int(n.Node.Range.Start.Byte) == start {
			found = append(found, n.Node.ID)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%d variable nodes at byte %d, want one", len(found), start)
	}
	return found[0]
}

// declNamed is the table's declaration named name.
func declNamed(t *testing.T, tbl *link.Table, name string) *link.Decl {
	t.Helper()
	for i := range tbl.Decls {
		if tbl.Decls[i].Name == name {
			return &tbl.Decls[i]
		}
	}
	t.Fatalf("the table has no declaration %s", name)
	return nil
}

const linkTableSrc = `def callee(a, b):
    return a


def caller(x):
    y = x
    callee(x, b=y)
    callee(1, b=2)
`

// TestTableEndpointsAreThePublishedVariableNodes protects the join of the
// link table's parameters and argument reads to the variable nodes the
// dependence facts publish: an argument's flow into a parameter is an edge
// between those nodes, so a table endpoint that names any other node is an
// edge to an entity nothing else describes. callee's parameters are a and b;
// caller's first call passes x by position and y by keyword, each read
// naming the caller's variable; the second call passes literals and reads
// nothing.
//
// Mutation: mint a second node for a variable the table names (resolve the
// table's endpoints apart from functions) -> a parameter's or read's Node is
// not the one variable node published at its identifier. Mutation: keep a
// read whose variable has no node -> the literal call's arguments read.
func TestTableEndpointsAreThePublishedVariableNodes(t *testing.T) {
	p := newProvider(t)
	nodes, tbl := indexedTable(t, p, "m.py", linkTableSrc)
	callee := declNamed(t, tbl, "callee")
	if len(callee.Parameters) != 2 {
		t.Fatalf("callee has parameters %+v, want a and b", callee.Parameters)
	}
	for i, name := range []string{"a", "b"} {
		prm := callee.Parameters[i]
		at := strings.Index(linkTableSrc, name+",")
		if name == "b" {
			at = strings.Index(linkTableSrc, "b)")
		}
		if prm.Name != name || prm.Index != uint32(i+1) || prm.Node != variableAt(t, nodes, at) {
			t.Fatalf("parameter %d is %+v, want %s at index %d naming the variable node at byte %d", i, prm, name, i+1, at)
		}
	}
	var calls []*link.Site
	for i := range tbl.Sites {
		if tbl.Sites[i].Callee == "callee" {
			calls = append(calls, &tbl.Sites[i])
		}
	}
	if len(calls) != 2 {
		t.Fatalf("%d sites call callee, want 2", len(calls))
	}
	first := calls[0]
	if len(first.Arguments) != 2 || first.Arguments[1].Keyword != "b" {
		t.Fatalf("the first call's arguments are %+v, want x and b=y", first.Arguments)
	}
	for i, at := range []int{strings.Index(linkTableSrc, "x):"), strings.Index(linkTableSrc, "y = x")} {
		reads := first.Arguments[i].Reads
		if len(reads) != 1 || reads[0].Node != variableAt(t, nodes, at) || reads[0].Kind != model.NodeVariable {
			t.Fatalf("argument %d reads %+v, want the variable node at byte %d", i, reads, at)
		}
	}
	for i, a := range calls[1].Arguments {
		if len(a.Reads) != 0 {
			t.Fatalf("literal argument %d reads %+v", i, a.Reads)
		}
	}
}

// TestAnEditBeforeACallableKeepsItsParameterNodes protects the parameter
// endpoints' identity across a body edit before the callable: a parameter is
// keyed by its callable and its position, so another file's argument flow
// into it, carried from an earlier run, still names a node the new run
// publishes.
//
// Mutation: key the table's parameter endpoint by its range -> the edited
// file's callee parameters name new nodes.
func TestAnEditBeforeACallableKeepsItsParameterNodes(t *testing.T) {
	p := newProvider(t)
	_, before := indexedTable(t, p, "m.py", linkTableSrc)
	edited := strings.Replace(linkTableSrc, "def callee", "def other():\n    z = 1\n    return z\n\n\ndef callee", 1)
	_, after := indexedTable(t, p, "m.py", edited)
	was, now := declNamed(t, before, "callee").Parameters, declNamed(t, after, "callee").Parameters
	if len(was) != 2 || len(now) != 2 {
		t.Fatalf("callee's parameters %+v then %+v, want two each", was, now)
	}
	for i := range was {
		if was[i].Node == "" || was[i].Node != now[i].Node {
			t.Fatalf("parameter %s named %q, then %q after an edit before callee", was[i].Name, was[i].Node, now[i].Node)
		}
	}
}
