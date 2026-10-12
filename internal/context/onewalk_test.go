package context

import (
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
)

// The weights fixture: `head` matches the task at the top level and reaches
// `settle` in two hops through `relay`; `tail` matches it at about a tenth of
// that and calls `settle` directly. `relay` and `settle` match nothing, so they
// are reached only by the walk.
const (
	weightsHead   = "internal/pay/head.go"
	weightsRelay  = "internal/pay/relay.go"
	weightsSettle = "internal/pay/settle.go"
	weightsTail   = "internal/pay/tail.go"
)

func weightsSpecs() []fixtureFileSpec {
	return []fixtureFileSpec{
		seedFile(weightsHead, seedHead), seedFile(weightsRelay, "relay"),
		seedFile(weightsSettle, "settle"), seedFile(weightsTail, seedTail),
	}
}

// The triangle fixture: the named seed `Start` documents `target` directly and
// reaches it in two calls through `middle`. The one-hop route's factor (0.55)
// is above the two-hop one's (0.9 x 0.9 x 0.65 = 0.53), and its depth makes a
// function required_symbol where the two-hop route makes it recommended; the
// two-hop route is the cheaper one by traversal cost (2 against 5).
const (
	triangleStart  = "internal/tri/start.go"
	triangleMiddle = "internal/tri/middle.go"
	triangleTarget = "internal/tri/target.go"
)

func triangleSpecs() []fixtureFileSpec {
	return []fixtureFileSpec{
		seedFile(triangleStart, "Start"), seedFile(triangleMiddle, "middle"),
		seedFile(triangleTarget, "target"),
	}
}

func triangleScope(fx *contextFixture) []model.Relation {
	return []model.Relation{
		fx.edge(triangleStart, model.RelDocuments, triangleTarget),
		fx.edge(triangleStart, model.RelCalls, triangleMiddle),
		fx.edge(triangleMiddle, model.RelCalls, triangleTarget),
	}
}

func weightsScope(fx *contextFixture) []model.Relation {
	return []model.Relation{
		fx.edge(weightsHead, model.RelCalls, weightsRelay),
		fx.edge(weightsRelay, model.RelCalls, weightsSettle),
		fx.edge(weightsTail, model.RelCalls, weightsSettle),
	}
}

// TestOneWalkEqualsTheMergeOfThePerWeightWalks protects ADR-0016 decision 1
// as the compiler computes it: ONE weighted walk from every root, each carrying
// its weight, must give every entity the same best (requirement, score) pair
// -- and so the same plan -- as taking, for each root weight on its own, every
// node's largest pair over every route from that weight's roots, and then each
// entity's best record across the weights.
//
// The reference is referenceCompile, whose scope walk (referenceWalks) does
// exactly that per group, apart from the walk. Both read every lexical match
// (fullRead), so both run one round over the same roots: the explicit seed's
// weight, and one weight per lexical score level.
//
//   - pairs: `finish` is three hops from the named seed and one from the tail
//     match L, so its best pair is L's; `forward` and `relay` are reached by
//     the named walk and by L's.
//   - closure: the tail matches Y and U each reach a node no other root does.
//   - weights: `settle` is one hop from the tail match and two from the head
//     match; the head's heavier, farther route lends it more.
//   - triangle: `target` is one `documents` hop and two `calls` hops from the
//     named seed; its larger pair is the one-hop route's, required_symbol at
//     depth 1, though the two-hop route is the cheaper by traversal cost.
//
// Mutations it catches: walk only one weight's roots (an entity reached only,
// or best, from another weight then takes another pair or leaves the plan);
// keep each node's cheapest-cost route, or its first-reached one, instead of
// its largest pair (`target` becomes recommended at depth 2).
func TestOneWalkEqualsTheMergeOfThePerWeightWalks(t *testing.T) {
	rows := []struct {
		name  string
		specs func() []fixtureFileSpec
		rels  func(*contextFixture) []model.Relation
		req   model.ContextRequest
		// reached is a node only the walk admits, so a plan without it would
		// make the comparison vacuous. Each row's file budget holds every
		// file, so no walk entry is packed out of the comparison.
		reached func(*contextFixture) model.NodeID
	}{
		{"pairs", pairsSpecs, pairsScope,
			model.ContextRequest{Task: seedTask, Seeds: []string{"Origin"}, Phase: model.PhaseVerify,
				Budget: model.Budget{MaxFiles: len(pairsSpecs())}},
			func(fx *contextFixture) model.NodeID { return fx.NodeOf(pairsNamed, "finish") }},
		{"closure", closureSpecs, closureScope, seedRequest(len(closureSpecs())),
			func(fx *contextFixture) model.NodeID { return fx.Node(closureV) }},
		{"weights", weightsSpecs, weightsScope, seedRequest(len(weightsSpecs())),
			func(fx *contextFixture) model.NodeID { return fx.Node(weightsSettle) }},
		{"triangle", triangleSpecs, triangleScope,
			model.ContextRequest{Task: "tidy", Seeds: []string{"Start"}, Phase: model.PhaseVerify,
				Budget: model.Budget{MaxFiles: len(triangleSpecs())}},
			func(fx *contextFixture) model.NodeID { return fx.Node(triangleTarget) }},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			fx := newFixtureFrom(t, row.specs(), row.rels)
			ref := intCompiler(t, fx, fx.Now)
			ref.fullRead = true
			want, wantComplete, wantNotices, _, err := ref.referenceCompile(fx.ctx, row.req)
			if err != nil {
				t.Fatalf("referenceCompile: %v", err)
			}
			c := intCompiler(t, fx, fx.Now)
			c.fullRead = true
			m, err := c.Compile(fx.ctx, row.req)
			if err != nil {
				t.Fatalf("Compile: %v", err)
			}
			entries, slices, excluded := readBackManifest(t, fx, m.ID)
			reached, ok := entryOf(entries, row.reached(fx))
			if !ok {
				t.Fatalf("the node only a walk reaches is not in the plan; the comparison would prove nothing")
			}
			if row.name == "triangle" && reached.Requirement != model.RequirementSymbol {
				t.Errorf("`target` is %s; its larger pair is the one-hop documents route's, %s at depth 1",
					reached.Requirement, model.RequirementSymbol)
			}
			sameJSON(t, "entries", want.Entries, entries)
			sameJSON(t, "slices", want.Slices, slices)
			sameJSON(t, "exclusions", want.Excluded, excluded)
			sameJSON(t, "notices", wantNotices, m.Notices)
			if m.ScopeComplete != wantComplete {
				t.Errorf("scope_complete is %v, the per-weight walks say %v", m.ScopeComplete, wantComplete)
			}
		})
	}
}
