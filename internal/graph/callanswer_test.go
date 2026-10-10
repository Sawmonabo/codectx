package graph

import (
	"context"
	"slices"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
)

// TestCallersAnswerLocatesEveryNodeAndCallSite pins a self-locating call
// hierarchy: one callers call returns where the callee and each caller are
// (their stored byte ranges, file and path) and where every call is (each
// call site's range, with its dispatch), so a client needs no follow-up symbol
// or references lookup to learn where anything is.
//
// Mutation caught: hydrating the answer's nodes without their stored interval
// (the delivery read that drops StoredNode.Bytes), or dropping the call
// relation's occurrences.
func TestCallersAnswerLocatesEveryNodeAndCallSite(t *testing.T) {
	f := loadCallFixture(t, "two_subclasses")
	e := f.engine(t)
	base := f.id(t, "Animal.sound")
	res, err := e.Neighbors(context.Background(), model.GraphRequest{
		Start: []model.NodeID{base}, Direction: model.DirectionIncoming,
		Relations: []model.RelationKind{model.RelCalls},
	})
	if err != nil {
		t.Fatalf("callers: %v", err)
	}
	locs := map[model.NodeID]model.NodeLocation{}
	for _, l := range res.Locations {
		locs[l.NodeID] = l
	}
	for _, name := range []string{"Animal.sound", "speak"} {
		id := f.id(t, name)
		got, ok := locs[id]
		if !ok {
			t.Errorf("the answer carries no location for %s; a caller would need a symbol lookup to find it", name)
			continue
		}
		if want := f.located[id]; got != want {
			t.Errorf("%s located at %+v, want the stored %+v", name, got, want)
		}
	}
	call := f.rel(t, "speak", model.RelCalls, "Animal.sound")
	var sites []model.RelationOccurrence
	for _, o := range res.Occurrences {
		if o.RelationID == call {
			sites = append(sites, o)
		}
	}
	want := f.evidence[call]
	if len(sites) != len(want) {
		t.Fatalf("speak -> Animal.sound carries %d call sites, want %d", len(sites), len(want))
	}
	for i, o := range sites {
		if o.EvidenceID != want[i].ID || o.Bytes != *want[i].Bytes || o.FileID != want[i].FileID ||
			o.Dispatch != want[i].Dispatch {
			t.Errorf("call site %d = %+v, want the stored occurrence %+v", i, o, want[i])
		}
	}
}

// callAnswer runs one call walk over a scenario.
func callAnswer(t *testing.T, f *callFixture, start string, dir model.Direction) model.GraphResult {
	t.Helper()
	res, err := f.engine(t).Neighbors(context.Background(), model.GraphRequest{
		Start: []model.NodeID{f.id(t, start)}, Direction: dir,
		Relations: []model.RelationKind{model.RelCalls},
	})
	if err != nil {
		t.Fatalf("%s of %s: %v", dir, start, err)
	}
	if err := res.Validate(); err != nil {
		t.Fatalf("%s of %s answered an invalid result: %v", dir, start, err)
	}
	return res
}

// hasRelation reports whether res serves rel as a row.
func hasRelation(res model.GraphResult, rel model.RelationID) bool {
	for _, r := range res.Relations {
		if r.ID == rel {
			return true
		}
	}
	return false
}

// marksOf is every mark res carries for rel.
func marksOf(res model.GraphResult, rel model.RelationID) []model.GraphRowMark {
	var out []model.GraphRowMark
	for _, m := range res.Marks {
		if m.RelationID == rel {
			out = append(out, m)
		}
	}
	return out
}

// callersOf is every node res lists as calling through a `calls` row.
func callersOf(f *callFixture, res model.GraphResult) map[string]bool {
	out := map[string]bool{}
	for _, r := range res.Relations {
		if r.Kind == model.RelCalls {
			out[f.name(r.From)] = true
		}
	}
	return out
}

// TestCallersOfAnOverrideIncludeCallsThroughItsBase pins ADR-0015's callers
// step on the two-subclass program (`speak(a: Animal): a.sound()`, Dog and
// Cat override `sound`, `direct(d: Dog): d.sound()`): who calls Dog.sound is
// `direct`, and `speak` through Animal.sound, counted apart, with the
// `overrides` relation that makes the route visible.
//
// Mutations caught: dropping the override step (speak is missing); walking
// overrides downward for callers (Animal.sound lists direct, which never runs
// Animal.sound).
func TestCallersOfAnOverrideIncludeCallsThroughItsBase(t *testing.T) {
	f := loadCallFixture(t, "two_subclasses")
	res := callAnswer(t, f, "Dog.sound", model.DirectionIncoming)
	direct := f.rel(t, "direct", model.RelCalls, "Dog.sound")
	through := f.rel(t, "speak", model.RelCalls, "Animal.sound")
	if !hasRelation(res, direct) {
		t.Errorf("callers(Dog.sound) does not list direct, its direct caller")
	}
	if len(marksOf(res, direct)) != 0 {
		t.Errorf("the direct call into Dog.sound is marked %+v; an exact fact carries no mark", marksOf(res, direct))
	}
	if !hasRelation(res, through) {
		t.Fatalf("callers(Dog.sound) does not list speak, whose a.sound() on an Animal may run Dog.sound")
	}
	ms := marksOf(res, through)
	if len(ms) != 1 || ms[0].Kind != model.MarkThroughBase || ms[0].Base != f.id(t, "Animal.sound") ||
		ms[0].BaseOverrides != 2 || !ms[0].Possible {
		t.Errorf("speak's call is marked %+v, want one possible through_base mark naming Animal.sound with its 2 overrides", ms)
	}
	if !hasRelation(res, f.rel(t, "Dog.sound", model.RelOverrides, "Animal.sound")) {
		t.Errorf("the answer omits Dog.sound overrides Animal.sound, the relation the through-base route uses")
	}
	if res.Counts.Direct != 1 || res.Counts.ThroughBase != 1 {
		t.Errorf("counts = %+v, want 1 direct and 1 through a base", res.Counts)
	}

	base := callAnswer(t, f, "Animal.sound", model.DirectionIncoming)
	if got := callersOf(f, base); got["direct"] || !got["speak"] {
		t.Errorf("callers(Animal.sound) = %v, want speak and never direct: direct(d: Dog) never runs Animal.sound", got)
	}
}

// TestCallersThroughBasesAtEveryDepth pins the any-depth climb on the
// three-level programs (Animal -> Dog -> Puppy): Puppy.sound is reached by
// `walk(d: Dog): d.sound()` through Dog.sound and by `speak(a: Animal)`
// through Animal.sound. Neither `super().sound()` nor C++'s qualified
// `a.Animal::sound()` can run an override, so neither is a through-base row.
//
// Mutations caught: one level only (speak is missing from Puppy.sound's
// callers); expanding exact calls (the super and qualified calls appear);
// walking overrides downward (callers(Animal.sound) lists walk).
func TestCallersThroughBasesAtEveryDepth(t *testing.T) {
	py := loadCallFixture(t, "three_levels_py")
	got := callersOf(py, callAnswer(t, py, "Puppy.sound", model.DirectionIncoming))
	if !got["speak"] || !got["walk"] {
		t.Errorf("callers(Puppy.sound) = %v, want speak (through Animal.sound) and walk (through Dog.sound)", got)
	}
	if got["Puppy.sound"] {
		t.Errorf("callers(Puppy.sound) lists Puppy.sound's own super().sound(), an exact call that runs Dog.sound only")
	}
	if got := callersOf(py, callAnswer(t, py, "Animal.sound", model.DirectionIncoming)); got["walk"] {
		t.Errorf("callers(Animal.sound) = %v; walk(d: Dog) calls Dog.sound and never runs Animal.sound", got)
	}

	cpp := loadCallFixture(t, "three_levels_cpp")
	got = callersOf(cpp, callAnswer(t, cpp, "Dog.sound", model.DirectionIncoming))
	if !got["speak"] {
		t.Errorf("callers(Dog.sound) = %v, want speak through Animal.sound", got)
	}
	if got["exact"] {
		t.Errorf("callers(Dog.sound) lists exact, whose a.Animal::sound() is qualified and never dispatches")
	}
}

// TestCalleesListTheOverridesAVirtualCallMayRun pins the callees step: a
// virtual call's target is listed, then every override of it at any depth,
// each marked "may dispatch to" and counted apart.
//
// Mutation caught: no downward step in callees (Dog.sound, Cat.sound and, a
// level further, Puppy.sound are missing).
func TestCalleesListTheOverridesAVirtualCallMayRun(t *testing.T) {
	f := loadCallFixture(t, "two_subclasses")
	res := callAnswer(t, f, "speak", model.DirectionOutgoing)
	if !hasRelation(res, f.rel(t, "speak", model.RelCalls, "Animal.sound")) {
		t.Fatalf("callees(speak) does not list Animal.sound, its declared callee")
	}
	for _, sub := range []string{"Dog.sound", "Cat.sound"} {
		rel := f.rel(t, sub, model.RelOverrides, "Animal.sound")
		ms := marksOf(res, rel)
		if !hasRelation(res, rel) || len(ms) != 1 || ms[0].Kind != model.MarkMayDispatch || !ms[0].Possible {
			t.Errorf("callees(speak) lists %s with marks %+v, want it marked may_dispatch", sub, ms)
		}
	}
	if res.Counts.Direct != 1 || res.Counts.MayDispatch != 2 {
		t.Errorf("counts = %+v, want 1 direct callee and 2 overrides it may dispatch to", res.Counts)
	}

	deep := loadCallFixture(t, "three_levels_py")
	res = callAnswer(t, deep, "speak", model.DirectionOutgoing)
	if rel := deep.rel(t, "Puppy.sound", model.RelOverrides, "Dog.sound"); !hasRelation(res, rel) {
		t.Errorf("callees(speak) stops one level down; Puppy.sound overrides Dog.sound and a.sound() may run it")
	}
}

// TestValueSetsAndMixedOccurrences pins the per-occurrence rules: a site
// bound by value flow to two classes' methods is shown as one of two, and a
// relation holding a qualified (exact) and a virtual occurrence into the base
// is through-base with only its virtual occurrence listed.
//
// Mutation caught: marking a whole relation from its first occurrence (the
// exact `Animal.sound(a)` decides `both` and the row is dropped, or both
// occurrences are listed).
func TestValueSetsAndMixedOccurrences(t *testing.T) {
	f := loadCallFixture(t, "value_set")
	res := callAnswer(t, f, "Dog.sound", model.DirectionIncoming)
	untyped := f.rel(t, "speak_u", model.RelCalls, "Dog.sound")
	ms := marksOf(res, untyped)
	if len(ms) != 1 || ms[0].Kind != model.MarkValueSet || ms[0].SetSize != 2 {
		t.Errorf("speak_u's call is marked %+v, want value_set 1 of 2", ms)
	}
	mixed := f.rel(t, "both", model.RelCalls, "Animal.sound")
	ms = marksOf(res, mixed)
	if len(ms) != 1 || ms[0].Kind != model.MarkThroughBase {
		t.Fatalf("both's call into Animal.sound is marked %+v, want through_base: one of its occurrences is virtual", ms)
	}
	var virtual []model.EvidenceID
	for _, ev := range f.evidence[mixed] {
		if ev.Dispatch == model.DispatchVirtual {
			virtual = append(virtual, ev.ID)
		}
	}
	if len(ms[0].Occurrences) != len(virtual) || len(virtual) != 1 || ms[0].Occurrences[0] != virtual[0] {
		t.Errorf("the through-base row lists occurrences %v, want only the virtual one %v", ms[0].Occurrences, virtual)
	}
	var listed []model.EvidenceID
	for _, o := range res.Occurrences {
		if o.RelationID == mixed {
			listed = append(listed, o.EvidenceID)
		}
	}
	if !slices.Equal(listed, virtual) {
		t.Errorf("the answer lists the through-base call's sites %v, want only the virtual one %v: "+
			"its super call cannot run the override", listed, virtual)
	}
}

// TestImpactReferencesAndPathsTakeTheOverrideStep pins the other operations:
// impact of a base lists its overrides; impact of an override lists the
// callers through its base and not the base; references walk the method
// family both ways; a forward path reaches an override through its base.
//
// Mutation caught: dropping the override step from any of the three.
func TestImpactReferencesAndPathsTakeTheOverrideStep(t *testing.T) {
	ctx := context.Background()
	f := loadCallFixture(t, "two_subclasses")
	e := f.engine(t)

	affected := func(start string) map[string]bool {
		res, err := e.Impact(ctx, model.ImpactRequest{Start: []model.NodeID{f.id(t, start)},
			Direction: model.DirectionIncoming})
		if err != nil {
			t.Fatalf("impact(%s): %v", start, err)
		}
		out := map[string]bool{}
		for _, en := range res.Entries {
			out[f.name(en.NodeID)] = true
		}
		return out
	}
	if got := affected("Animal.sound"); !got["Dog.sound"] || !got["Cat.sound"] {
		t.Errorf("impact(Animal.sound) = %v, want both overrides: a change to the base breaks them", got)
	}
	if got := affected("Dog.sound"); !got["speak"] || got["Animal.sound"] {
		t.Errorf("impact(Dog.sound) = %v, want speak (it may run Dog.sound) and never Animal.sound (a route, not affected)", got)
	}

	refsOf := func(node string) map[model.RelationID]bool {
		page, err := e.References(ctx, model.ReferenceRequest{NodeID: f.id(t, node),
			Operation: model.ReferenceReferences, SemanticSource: model.SemanticCanonical})
		if err != nil {
			t.Fatalf("references(%s): %v", node, err)
		}
		out := map[model.RelationID]bool{}
		for _, o := range page.Items {
			out[o.RelationID] = true
		}
		return out
	}
	if !refsOf("Dog.sound")[f.rel(t, "speak", model.RelCalls, "Animal.sound")] {
		t.Errorf("references(Dog.sound) omits Animal.sound's references; the family is walked upward")
	}
	if !refsOf("Animal.sound")[f.rel(t, "direct", model.RelCalls, "Dog.sound")] {
		t.Errorf("references(Animal.sound) omits Dog.sound's references; the family is walked downward")
	}

	path, err := e.ShortestPath(ctx, model.PathRequest{From: f.id(t, "speak"), To: f.id(t, "Dog.sound"),
		Relations: []model.RelationKind{model.RelCalls}})
	if err != nil {
		t.Fatalf("path(speak -> Dog.sound): %v", err)
	}
	want := []model.RelationID{f.rel(t, "speak", model.RelCalls, "Animal.sound"),
		f.rel(t, "Dog.sound", model.RelOverrides, "Animal.sound")}
	if len(path.Paths) == 0 || !slices.Equal(path.Paths[0].Relations, want) {
		t.Errorf("path(speak -> Dog.sound) = %+v, want the route through Animal.sound %v", path.Paths, want)
	}
}
