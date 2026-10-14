package graph

import (
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
)

// oneFunctionValue is `void call(void) { on_exit(1); }` with the one function
// f whose address the project takes, `run(cb) { cb(1) }` with the one
// function value g the project passes it, `speak(a) { a.sound() }` whose
// receiver holds only the Dog the project passes it, and `direct() { f(1)
// }`, a direct call: each bound site has one target.
func oneFunctionValue() *callFixture {
	occ := func(at uint64, detail string) []fixtureOccurrence {
		return []fixtureOccurrence{{Bytes: [2]uint64{at, at + 5}, Dispatch: model.DispatchExact, Detail: detail}}
	}
	return newCallFixture("one_function_value", fixtureScenario{
		Nodes: []fixtureDecl{
			{Name: "f", Kind: model.NodeFunction, File: "c.c", Bytes: [2]uint64{0, 20}, Language: "c"},
			{Name: "g", Kind: model.NodeFunction, File: "c.js", Bytes: [2]uint64{0, 20}, Language: "javascript"},
			{Name: "call", Kind: model.NodeFunction, File: "c.c", Bytes: [2]uint64{30, 60}, Language: "c"},
			{Name: "run", Kind: model.NodeFunction, File: "c.js", Bytes: [2]uint64{30, 60}, Language: "javascript"},
			{Name: "direct", Kind: model.NodeFunction, File: "c.c", Bytes: [2]uint64{70, 90}, Language: "c"},
			{Name: "sound", Kind: model.NodeMethod, File: "c.js", Bytes: [2]uint64{70, 90}, Language: "javascript"},
			{Name: "speak", Kind: model.NodeFunction, File: "c.js", Bytes: [2]uint64{100, 130}, Language: "javascript"},
		},
		Relations: []fixtureFact{
			{From: "call", Kind: model.RelCalls, To: "f", Occurrences: occ(40, "address_taken")},
			{From: "run", Kind: model.RelCalls, To: "g", Occurrences: occ(40, "function_value")},
			{From: "direct", Kind: model.RelCalls, To: "f", Occurrences: occ(80, "direct")},
			{From: "speak", Kind: model.RelCalls, To: "sound", Occurrences: occ(110, "receiver_value")},
		},
	})
}

// TestOneFunctionValueIsMarkedOneOfOne pins the value_set mark on a call
// through a function pointer or function value bound to one target, and on a
// method call whose receiver holds one class's instance the project passes
// it: the set is the project's own (the functions whose address it takes,
// the values it passes), and a value from outside it may hold another, so
// the row reads "1 of 1", never a direct call; a direct call carries no mark.
//
// Mutations caught: marking value sets only from two targets up (the
// address_taken and function_value rows carry no mark); marking by detail
// alone without the technique list (the direct row is marked); leaving
// receiver_value out of the technique list (the speak row is unmarked).
func TestOneFunctionValueIsMarkedOneOfOne(t *testing.T) {
	f := oneFunctionValue()
	for _, c := range []struct{ from, to string }{{"call", "f"}, {"run", "g"}, {"speak", "sound"}} {
		res := callAnswer(t, f, c.from, model.DirectionOutgoing)
		ms := marksOf(res, f.rel(t, c.from, model.RelCalls, c.to))
		if len(ms) != 1 || ms[0].Kind != model.MarkValueSet || ms[0].SetSize != 1 || !ms[0].Possible {
			t.Errorf("callees(%s) marks its one bound target %+v, want value_set 1 of 1", c.from, ms)
		}
	}
	res := callAnswer(t, f, "direct", model.DirectionOutgoing)
	if ms := marksOf(res, f.rel(t, "direct", model.RelCalls, "f")); len(ms) != 0 {
		t.Errorf("callees(direct) marks its direct call %+v, want no mark", ms)
	}
}
