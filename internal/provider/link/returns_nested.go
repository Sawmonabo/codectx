package link

import (
	"github.com/Sawmonabo/codectx/internal/model"
)

// Nested call results (docs/providers-link.md, Return flows). A call whose
// value another call consumes directly (`g(f())`, `f().m()`) has no
// variable to write it into: its value flows from its callee's return node
// into the parameter of the consuming call's callee that the argument it is
// part of binds (the receiver parameter for a receiver), and, when that
// parameter reaches its callable's return, on to the consuming call's own
// result. The first edge leaves one callee and enters another, so it
// carries both roles, `return` and `argument`: an impact walk that entered
// f through another call's argument never takes it, and one that takes it
// has entered g and never leaves g through a return edge. The second edge
// leaves f and stays in the caller, the summary of g at this site, so it is
// a `return` edge. Both are published at the nested call.

// receiverArg is the argument index a site's facts give its receiver: one
// past its arguments, which no argument has.
func (s *Site) receiverArg() int32 { return int32(len(s.Arguments)) }

// nestedArg is the argument index of the facts about n's argument of s.
func (s *Site) nestedArg(n Nested) int32 {
	if n.Arg == NestedReceiver {
		return s.receiverArg()
	}
	return n.Arg
}

// nests reports whether a call nested in s is part of argument arg.
func (s *Site) nests(arg int32) bool {
	for _, n := range s.Nested {
		if s.nestedArg(n) == arg {
			return true
		}
	}
	return false
}

// receiverFlows emits the FactFlow, with its FactSummary, of the call that
// is site s's receiver into target's receiver parameter (Index 0), when a
// call is s's receiver and target declares one.
func receiverFlows(s *Site, site, target Ref, params []paramInfo, emit func(Binding) error) error {
	if !s.nests(s.receiverArg()) {
		return nil
	}
	for i := range params {
		if params[i].Index == 0 {
			return flowFacts(s, site, target, params, s.receiverArg(), params[i].Pos, emit)
		}
	}
	return nil
}

// nestedOut is what a consuming site publishes into: per argument index,
// the parameter nodes its flows reach and whether a summary carries it to
// the site's result.
type nestedOut struct {
	params  map[int32][]model.NodeID
	summary map[int32]bool
}

// noteNested keeps, for site i (s) and its published facts group, what the
// nested flows of the file need: the return nodes of a call another one
// consumes, and the parameters and summaries of a consuming call.
func (f *filePublication) noteNested(i int32, s *Site, group []idBinding) {
	if len(s.Nested) == 0 && s.Feeds == 0 {
		return
	}
	if f.nestRets == nil {
		f.nestRets, f.nestOuts = map[int32][]model.NodeID{}, map[int32]nestedOut{}
	}
	out := nestedOut{params: map[int32][]model.NodeID{}, summary: map[int32]bool{}}
	for _, b := range group {
		switch {
		case b.Kind == FactReturn && s.Feeds > 0:
			f.nestRets[i] = append(f.nestRets[i], b.Target)
		case b.Kind == FactFlow && len(s.Nested) > 0 && s.nests(b.Arg):
			out.params[b.Arg] = append(out.params[b.Arg], b.Target)
		case b.Kind == FactSummary && len(s.Nested) > 0 && s.nests(b.Arg):
			out.summary[b.Arg] = true
		}
	}
	if len(s.Nested) > 0 {
		f.nestOuts[i] = out
	}
}

// nestedFlows publishes the file's nested flows, once every site's facts
// are noted: for each call a site consumes, from each of its callee's return
// nodes to each parameter the argument binds (detail `argument` and detail
// `return`), and, where the summary carries the argument, to each variable
// of the consuming site's result and the return node it reaches (detail
// `return`), at the nested call.
func (f *filePublication) nestedFlows() error {
	for i := range f.t.Sites {
		out, ok := f.nestOuts[int32(i)]
		if !ok {
			continue
		}
		s := &f.t.Sites[i]
		key := callsiteKey(f.fv.Path, s.NameStart, s.NameEnd)
		for _, n := range s.Nested {
			inner := &f.t.Sites[n.Site]
			arg := s.nestedArg(n)
			rng := located(inner.Range)
			for _, ret := range f.nestRets[n.Site] {
				for _, p := range out.params[arg] {
					f.relation(ret, model.RelDataFlowsTo, p, rng, key, DetailArgument, "", 0)
					f.relation(ret, model.RelDataFlowsTo, p, rng, key, DetailReturn, "", 0)
				}
				if !out.summary[arg] {
					continue
				}
				for _, to := range s.Results {
					f.node(occurrenceOf(f.fv, f.t, to))
					f.relation(ret, model.RelDataFlowsTo, to.Node, rng, key, DetailReturn, "", 0)
				}
				if s.Into > 0 {
					if int(s.Into) > len(f.t.Decls) || f.t.Decls[s.Into-1].Return == nil {
						return internalErr("a link table site of " + f.fv.Path + " returns into a declaration with no return node")
					}
					into := *f.t.Decls[s.Into-1].Return
					f.node(occurrenceOf(f.fv, f.t, into))
					if into.Node != ret {
						f.relation(ret, model.RelDataFlowsTo, into.Node, rng, key, DetailReturn, "", 0)
					}
				}
			}
		}
	}
	return nil
}
