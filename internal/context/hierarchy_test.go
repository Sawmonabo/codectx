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

// TestAClassSeedRequiresItsSupertypeAndNotItsSibling: a `Dog` seed requires
// the `Animal` it extends whole (one hop) and only recommends `Cat`, the
// sibling class reached up `extends` and straight back down it. By distance
// alone the sibling is two hops from the seed, which demotes a class one rank
// to required_symbol; the route's turn is what makes it a sibling.
//
// Mutation it catches: the walk not recording the turn (no peer kinds in the
// label rule), or admitEntry ignoring it: Cat is required_symbol.
func TestAClassSeedRequiresItsSupertypeAndNotItsSibling(t *testing.T) {
	specs := []fixtureFileSpec{
		{hierDog, "Dog", model.NodeClass, "package zoo\n\ntype Dog struct{ Animal }\n", false},
		{hierAnimal, "Animal", model.NodeClass, "package zoo\n\ntype Animal struct{}\n", false},
		{hierCat, "Cat", model.NodeClass, "package zoo\n\ntype Cat struct{ Animal }\n", false},
	}
	fx := newFixtureFrom(t, specs, func(fx *contextFixture) []model.Relation {
		return []model.Relation{
			fx.edge(hierDog, model.RelExtends, hierAnimal),
			fx.edge(hierCat, model.RelExtends, hierAnimal),
		}
	})
	m, err := intCompiler(t, fx, fx.Now).Compile(fx.ctx, model.ContextRequest{
		Task: "make `Dog` quieter", Seeds: []string{"Dog"}, Phase: model.PhaseVerify})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	entries, _, _ := readBackManifest(t, fx, m.ID)
	byNode := map[model.NodeID]model.ContextEntry{}
	for _, e := range entries {
		byNode[e.NodeID] = e
	}
	if base := byNode[fx.Node(hierAnimal)]; base.Requirement != model.RequirementFull {
		t.Errorf("the supertype is %q, want %s: one hop over extends is a contract read whole",
			base.Requirement, model.RequirementFull)
	}
	sibling, ok := byNode[fx.Node(hierCat)]
	if !ok {
		t.Fatalf("the sibling class is not in the plan; the walk did not go up and back down; entries: %+v", entries)
	}
	if sibling.Requirement != model.RequirementRecommended {
		t.Errorf("the sibling class is %s, want %s: it shares a supertype with the seed and is not one it depends on",
			sibling.Requirement, model.RequirementRecommended)
	}
}
