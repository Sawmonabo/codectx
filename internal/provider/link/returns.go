package link

import (
	"strconv"

	"github.com/Sawmonabo/codectx/internal/model"
)

// Return-value flow by summary edges (ADR-0017 decision 7). A bound call
// site's result is its Results, the variables the call's value is written
// into, and its Into, the callable whose return the value reaches directly.
// The callee gives back two things, neither a plain edge from inside the
// callee to the site:
//   - its return summary: a parameter whose value reaches its return
//     (Parameter.Returned) makes each argument bound to it flow into the
//     site's Results (FactSummary), straight from the caller's variables,
//     so a walk never enters the callee to come back;
//   - its return node (Decl.Return), into which only the values created
//     inside it flow, never one a parameter reaches: it flows into the
//     site's Results and Into (FactReturn), so a walk that leaves through
//     any site's return started inside the callee and never entered it
//     through another site's argument.

// hasResult reports whether site s's value reaches a variable or a return.
func hasResult(s *Site) bool { return len(s.Results) > 0 || s.Into > 0 }

// flowFacts emits, for argument arg of site s at site bound to parameter pos
// of target among params (target's declared parameters), the argument's
// FactFlow into the parameter and, when that parameter's value reaches
// target's return and the site's value is written into a variable, its
// FactSummary. A summary never targets Into: the callable that returns the
// call's value has the argument's own summary for that.
func flowFacts(s *Site, site, target Ref, params []paramInfo, arg, pos int32, emit func(Binding) error) error {
	if err := emit(Binding{Kind: FactFlow, Site: site, Target: target, Detail: DetailArgument, Arg: arg, Param: pos}); err != nil {
		return err
	}
	if len(s.Results) == 0 {
		return nil
	}
	for i := range params {
		if p := &params[i]; p.Pos == pos && p.Returned {
			return emit(Binding{Kind: FactSummary, Site: site, Target: target, Detail: DetailSummary, Arg: arg, Param: pos,
				Position: int32(p.Index)})
		}
	}
	return nil
}

// returnFact emits the FactReturn of site s at site bound to the
// declaration target, when the site's value reaches a variable or a return.
// A target with no return node makes it no fact (idTable.identify).
func returnFact(s *Site, site, target Ref, emit func(Binding) error) error {
	if !hasResult(s) {
		return nil
	}
	return emit(Binding{Kind: FactReturn, Site: site, Target: target, Detail: DetailReturn})
}

// returnFlows publishes the return flow of site s, whose facts are group and
// whose call-site key is key: for each FactSummary, one `data_flows_to` from
// each variable the argument reads to each variable of s.Results, evidenced
// at the argument with the detail "summary:" and the parameter's position;
// for each FactReturn, one from the callee's return node to each variable of
// s.Results and to the return node of the callable s.Into names, evidenced
// at the site with the detail "return". Each endpoint of the site's own file
// is published as an occurrence beside it.
func (f *filePublication) returnFlows(s *Site, group []idBinding, key string) error {
	results := func() {
		for _, r := range s.Results {
			f.node(occurrenceOf(f.fv, f.t, r))
		}
	}
	for _, b := range group {
		switch b.Kind {
		case FactSummary:
			if b.Arg >= int32(len(s.Arguments)) {
				return internalErr("a summary names argument " + strconv.Itoa(int(b.Arg)) + " past the arguments of a site of " + f.fv.Path)
			}
			a := &s.Arguments[b.Arg]
			detail := DetailSummary + ":" + strconv.Itoa(int(b.Position))
			results()
			for _, r := range a.Reads {
				f.node(occurrenceOf(f.fv, f.t, r))
				for _, to := range s.Results {
					if r.Node != to.Node {
						f.relation(r.Node, model.RelDataFlowsTo, to.Node, located(a.Range), key, detail, "", 0)
					}
				}
			}
		case FactReturn:
			results()
			for _, to := range s.Results {
				f.relation(b.Target, model.RelDataFlowsTo, to.Node, located(s.Range), key, DetailReturn, "", 0)
			}
			if s.Into > 0 {
				if int(s.Into) > len(f.t.Decls) || f.t.Decls[s.Into-1].Return == nil {
					return internalErr("a link table site of " + f.fv.Path + " returns into a declaration with no return node")
				}
				into := *f.t.Decls[s.Into-1].Return
				f.node(occurrenceOf(f.fv, f.t, into))
				if into.Node != b.Target {
					f.relation(b.Target, model.RelDataFlowsTo, into.Node, located(s.Range), key, DetailReturn, "", 0)
				}
			}
		}
	}
	return nil
}
