package graph

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
)

// openWorld is the two-subclass program (`speak(a: Animal): a.sound()`, Dog
// and Cat override `sound`) with Animal.sound's openness given, plus
// `run(h: Handler): h.serve()` into a method nothing in the project
// overrides, as open, and `show(o)` calling an external method that Dog's
// `toString` overrides.
func openWorld(open bool) *callFixture {
	var meta json.RawMessage
	if open {
		meta = json.RawMessage(`{"extensible":true}`)
	}
	virtual := func(at uint64) []fixtureOccurrence {
		return []fixtureOccurrence{{Bytes: [2]uint64{at, at + 5}, Dispatch: model.DispatchVirtual}}
	}
	plain := func(at uint64) []fixtureOccurrence { return []fixtureOccurrence{{Bytes: [2]uint64{at, at + 5}}} }
	return newCallFixture("open_world", fixtureScenario{
		Nodes: []fixtureDecl{
			{Name: "Animal.sound", Kind: model.NodeMethod, File: "a.py", Bytes: [2]uint64{0, 10}, Metadata: meta},
			{Name: "Dog.sound", Kind: model.NodeMethod, File: "a.py", Bytes: [2]uint64{20, 30}},
			{Name: "Cat.sound", Kind: model.NodeMethod, File: "a.py", Bytes: [2]uint64{40, 50}},
			{Name: "speak", Kind: model.NodeFunction, File: "a.py", Bytes: [2]uint64{60, 80}},
			{Name: "Handler.serve", Kind: model.NodeMethod, File: "h.py", Bytes: [2]uint64{0, 10}, Metadata: meta},
			{Name: "run", Kind: model.NodeFunction, File: "h.py", Bytes: [2]uint64{20, 40}},
			{Name: "Object.toString", Kind: model.NodeMethod, File: "", Metadata: json.RawMessage(`{"resolution":"external"}`)},
			{Name: "Dog.toString", Kind: model.NodeMethod, File: "a.py", Bytes: [2]uint64{90, 100}},
			{Name: "show", Kind: model.NodeFunction, File: "a.py", Bytes: [2]uint64{110, 130}},
		},
		Relations: []fixtureFact{
			{From: "Dog.sound", Kind: model.RelOverrides, To: "Animal.sound", Occurrences: plain(20)},
			{From: "Cat.sound", Kind: model.RelOverrides, To: "Animal.sound", Occurrences: plain(40)},
			{From: "speak", Kind: model.RelCalls, To: "Animal.sound", Occurrences: virtual(70)},
			{From: "run", Kind: model.RelCalls, To: "Handler.serve", Occurrences: virtual(30)},
			{From: "Dog.toString", Kind: model.RelOverrides, To: "Object.toString", Occurrences: plain(90)},
			{From: "show", Kind: model.RelCalls, To: "Object.toString", Occurrences: virtual(120)},
		},
	})
}

func noticed(res model.GraphResult, method string) bool {
	return slices.ContainsFunc(res.Meta.Notices, func(n string) bool {
		return strings.Contains(n, method) && strings.Contains(n, "outside the indexed project")
	})
}

// TestAnOpenBaseSaysItsOverridesMayNotBeAll pins the open-world disclosure:
// a base method code outside the indexed project can override (one its
// language leaves open, or one outside the project) marks every through-base
// and may-dispatch row through it open, counts them, and says in a notice
// that the overrides listed or counted for it are the project's own, also
// for a virtual call into an open method no override of the project
// implements; a closed base says none of it.
//
// Mutations caught: no disclosure (the open marks, the count and the notices
// are missing); reading openness from the language mark alone (the external
// Object.toString is not open); disclosing a closed base (the closed program
// carries the notice).
func TestAnOpenBaseSaysItsOverridesMayNotBeAll(t *testing.T) {
	f := openWorld(true)
	res := callAnswer(t, f, "Dog.sound", model.DirectionIncoming)
	ms := marksOf(res, f.rel(t, "speak", model.RelCalls, "Animal.sound"))
	if len(ms) != 1 || ms[0].Kind != model.MarkThroughBase || !ms[0].Open {
		t.Errorf("speak's call through Animal.sound is marked %+v, want through_base, open", ms)
	}
	if res.Counts.OpenBase != 1 || !noticed(res, "Animal.sound") {
		t.Errorf("callers(Dog.sound) counts %+v and notices %q, want 1 open base and a notice naming Animal.sound",
			res.Counts, res.Meta.Notices)
	}

	res = callAnswer(t, f, "speak", model.DirectionOutgoing)
	for _, sub := range []string{"Dog.sound", "Cat.sound"} {
		ms := marksOf(res, f.rel(t, sub, model.RelOverrides, "Animal.sound"))
		if len(ms) != 1 || ms[0].Kind != model.MarkMayDispatch || !ms[0].Open {
			t.Errorf("callees(speak) marks %s %+v, want may_dispatch, open", sub, ms)
		}
	}
	if res.Counts.OpenBase != 2 || !noticed(res, "Animal.sound") {
		t.Errorf("callees(speak) counts %+v and notices %q, want 2 open rows and a notice naming Animal.sound",
			res.Counts, res.Meta.Notices)
	}

	if res := callAnswer(t, f, "run", model.DirectionOutgoing); !noticed(res, "Handler.serve") {
		t.Errorf("callees(run) notices %q, want Handler.serve, which nothing in the project overrides, disclosed open",
			res.Meta.Notices)
	}
	res = callAnswer(t, f, "Dog.toString", model.DirectionIncoming)
	ms = marksOf(res, f.rel(t, "show", model.RelCalls, "Object.toString"))
	if len(ms) != 1 || !ms[0].Open {
		t.Errorf("show's call through the external Object.toString is marked %+v, want open", ms)
	}

	closed := openWorld(false)
	res = callAnswer(t, closed, "Dog.sound", model.DirectionIncoming)
	ms = marksOf(res, closed.rel(t, "speak", model.RelCalls, "Animal.sound"))
	if len(ms) != 1 || ms[0].Open || res.Counts.OpenBase != 0 || noticed(res, "Animal.sound") {
		t.Errorf("callers(Dog.sound) of a closed base marks %+v, counts %+v, notices %q; want nothing open",
			ms, res.Counts, res.Meta.Notices)
	}
}

// TestAnOpenBaseOfALongNameKeepsTheAnswerValid pins the notice's bound: a
// base whose qualified name alone is longer than a notice may be is named by
// its id, so the answer stays valid.
//
// Mutation caught: naming the base by its qualified name whatever its length
// (callAnswer's Validate refuses the answer).
func TestAnOpenBaseOfALongNameKeepsTheAnswerValid(t *testing.T) {
	long := "pkg." + strings.Repeat("Deep", 150) + ".sound"
	f := newCallFixture("open_long", fixtureScenario{
		Nodes: []fixtureDecl{
			{Key: long, Name: "sound", Kind: model.NodeMethod, File: "a.py", Bytes: [2]uint64{0, 10},
				Metadata: json.RawMessage(`{"extensible":true}`)},
			{Name: "speak", Kind: model.NodeFunction, File: "a.py", Bytes: [2]uint64{20, 40}},
		},
		Relations: []fixtureFact{{From: "speak", Kind: model.RelCalls, To: long,
			Occurrences: []fixtureOccurrence{{Bytes: [2]uint64{30, 35}, Dispatch: model.DispatchVirtual}}}},
	})
	res := callAnswer(t, f, "speak", model.DirectionOutgoing)
	if !noticed(res, string(f.id(t, long))) {
		t.Errorf("callees(speak) notices %q, want the open base named by its id", res.Meta.Notices)
	}
}

// TestImpactOfAnOpenMethodSaysOutsideOverridesAreAffected pins the impact
// disclosure: changing a method code outside the indexed project can
// override also breaks the overrides written there, which no index of the
// project lists, so the answer says so; a closed method says nothing of it.
//
// Mutation caught: no seed disclosure in Impact (the open program's answer
// lists Dog.sound and Cat.sound as if they were every override).
func TestImpactOfAnOpenMethodSaysOutsideOverridesAreAffected(t *testing.T) {
	for _, open := range []bool{true, false} {
		f := openWorld(open)
		res, err := f.engine(t).Impact(context.Background(), model.ImpactRequest{
			Start: []model.NodeID{f.id(t, "Animal.sound")}, Direction: model.DirectionIncoming})
		if err != nil {
			t.Fatalf("impact(Animal.sound): %v", err)
		}
		got := slices.ContainsFunc(res.Meta.Notices, func(n string) bool {
			return strings.Contains(n, "Animal.sound") && strings.Contains(n, "outside the indexed project")
		})
		if got != open {
			t.Errorf("impact(Animal.sound) with the base open=%v notices %q", open, res.Meta.Notices)
		}
	}
}
