package context

import (
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
)

// The override hierarchy: `Dog.sound` and `Cat.sound` both override
// `Animal.sound`. The fixture names each method by a distinct symbol so the
// seed resolves to `Dog.sound` alone.
const (
	hierDog    = "internal/zoo/dog.go"
	hierAnimal = "internal/zoo/animal.go"
	hierCat    = "internal/zoo/cat.go"
)

func hierarchySpecs() []fixtureFileSpec {
	return []fixtureFileSpec{
		{hierDog, "DogSound", model.NodeMethod, "package zoo\n\nfunc (Dog) sound() string { return \"woof\" }\n", false},
		{hierAnimal, "AnimalSound", model.NodeMethod, "package zoo\n\nfunc (Animal) sound() string { return \"\" }\n", false},
		{hierCat, "CatSound", model.NodeMethod, "package zoo\n\nfunc (Cat) sound() string { return \"meow\" }\n", false},
	}
}

func hierarchyScope(fx *contextFixture) []model.Relation {
	return []model.Relation{
		fx.edge(hierDog, model.RelOverrides, hierAnimal),
		fx.edge(hierCat, model.RelOverrides, hierAnimal),
	}
}

// TestAnOverrideSeedRequiresItsBaseAndNotItsSibling pins how far a method's
// hierarchy is required scope: a `Dog.sound` seed requires the `Animal.sound`
// it overrides (one hop) and only recommends `Cat.sound`, the sibling override
// reached over `overrides` up and then down (two hops).
//
// Mutation it catches: walk `overrides` up then down as one hop, so a sibling
// override is at depth 1 and becomes required scope.
func TestAnOverrideSeedRequiresItsBaseAndNotItsSibling(t *testing.T) {
	fx := newFixtureFrom(t, hierarchySpecs(), hierarchyScope)
	m, err := intCompiler(t, fx, fx.Now).Compile(fx.ctx, model.ContextRequest{
		Task: "make `DogSound` quieter", Seeds: []string{"DogSound"}, Phase: model.PhaseVerify})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	entries, _, _ := readBackManifest(t, fx, m.ID)
	byNode := map[model.NodeID]model.ContextEntry{}
	for _, e := range entries {
		byNode[e.NodeID] = e
	}
	base, ok := byNode[fx.Node(hierAnimal)]
	if !ok {
		t.Fatalf("the overridden base method is not in the plan; entries: %+v", entries)
	}
	if base.Requirement != model.RequirementSymbol {
		t.Errorf("the method the seed overrides is %s, want %s: one hop over overrides is required at its symbol",
			base.Requirement, model.RequirementSymbol)
	}
	sibling, ok := byNode[fx.Node(hierCat)]
	if !ok {
		t.Fatalf("the sibling override is not in the plan; the walk did not go up and back down; entries: %+v", entries)
	}
	if isRequired(sibling.Requirement) {
		t.Errorf("the sibling override is %s: it is two hops from the seed and must not be required scope",
			sibling.Requirement)
	}
	if sibling.Requirement != model.RequirementRecommended {
		t.Errorf("the sibling override is %s, want %s at two hops", sibling.Requirement, model.RequirementRecommended)
	}
}
