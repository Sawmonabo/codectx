package link

import (
	"context"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/wire"
)

// TestAnUnknownSummaryIsCountedNotPublished pins the return summary of a
// callee whose analysis failed: its parameter is Unknown and has no node,
// so a bound site whose value is written into a variable finds a summary
// fact for the argument, which publishes no relation (no false flow from
// the argument to the result) and is counted on the data_flows_to row.
//
// Mutations: no summary for an Unknown parameter (flowFacts reads Returned
// alone) -> no summary fact, the loss is silent; publish the unnamed
// summary -> an argument-to-result relation the callee was never shown to
// carry.
func TestAnUnknownSummaryIsCountedNotPublished(t *testing.T) {
	param := Parameter{Kind: model.NodeVariable, Name: "p", Index: 1, Unknown: true}
	var facts []Binding
	s := &Site{Results: []Endpoint{node("r", model.NodeVariable, "r", 14)}}
	err := flowFacts(s, Ref{File: 0, Index: 0}, Ref{File: 1, Index: 0}, []paramInfo{{Pos: 0, Index: 1, Unknown: true}}, 0, 0,
		func(b Binding) error {
			facts = append(facts, b)
			return nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 2 || facts[1].Kind != FactSummary {
		t.Fatalf("facts = %+v, want the argument flow and its summary", facts)
	}

	callee := declared("f", model.NodeFunction, "f", 0, param)
	ret := node("ret:f", model.NodeReturn, "return", 0)
	callee.Return = &ret
	b := table("b.c", "c", []Decl{callee}, nil)
	arg := Argument{Range: span(12, 13), Reads: []Endpoint{node("a", model.NodeVariable, "a", 12)}}
	site := callAt(0, "f", wire.ShapeBare, 10, arg)
	site.Results = s.Results
	a := table("a.c", "c", []Decl{declared("main", model.NodeFunction, "main", 0)}, []Site{site})
	u := testUnit(t, a, b)
	got := publishFacts(t, u, "u", []idBinding{
		callFact("a.c", 0, "b.c", "f", model.DispatchExact),
		{Kind: FactFlow, SiteFile: "a.c", Site: 0, Arg: 0, TargetFile: "b.c", Detail: DetailArgument},
		{Kind: FactSummary, SiteFile: "a.c", Site: 0, Arg: 0, Position: 1, TargetFile: "b.c", Detail: DetailSummary},
	}, nil, nil)
	if n := len(got.edges(model.RelDataFlowsTo)); n != 0 {
		t.Errorf("the flows into a callee whose analysis failed publish %d relations, want none", n)
	}
	if row := got.row(t, model.RelDataFlowsTo); row.State != model.CapabilityPartial || row.Details["facts_without_node"] != "2" {
		t.Errorf("the data_flows_to row is %s with %v, want partial with facts_without_node 2 (the flow and its summary)",
			row.State, row.Details)
	}
}

// TestUnknownIsInTheParameterEntry pins that the Unknown mark reaches the
// readers of a callee's parameters through its hierarchy entry, which a
// refresh traces, so a callee whose analysis later succeeds re-runs the
// sites bound to it.
//
// Mutation: addParameters leaves paramUnknown out -> the inference reads
// the parameter as neither Returned nor Unknown and finds no summary.
func TestUnknownIsInTheParameterEntry(t *testing.T) {
	f := decl(model.NodeFunction, "f", -1)
	f.Parameters = []Parameter{{Kind: model.NodeVariable, Name: "p", Index: 1, Unknown: true}}
	u := goldenUnit(t, js("src/a.js", []Decl{f}, nil, nil))
	ctx := context.Background()
	h, err := buildHierarchy(ctx, u, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer h.close()
	var got []paramInfo
	if err := h.parameters(Ref{File: 0, Index: 0}, func(p paramInfo) error {
		got = append(got, p)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !got[0].Unknown || got[0].Returned {
		t.Fatalf("parameters = %+v, want one Unknown parameter", got)
	}
}
