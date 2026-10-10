package context

import (
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
)

// The two files of the weak-root fixture: `refund` is the task's only lexical
// match, and `settle` is what it calls.
const (
	pairsCaller = "internal/pay/money.go"
	pairsCallee = "internal/pay/ledger.go"
)

// TestALexicalRootNeverMakesAnEntryRequired protects ADR-0016 decision 1's
// cap on a lexical root's class: the requirement a walk from a lexical match
// lends is no stronger than recommended, so a task that names nothing exactly
// cannot pull a callee into required scope through a word that merely matched.
//
// The task names no explicit seed, and its only word that matches anything,
// "refund", is plain prose -- no underscore, digit or interior capital -- so
// no exact step resolves it and `refund` is a lexical root and nothing more.
// `settle` is a function one `calls` hop from it, which boundaryRequirement
// makes required_symbol from any root that may lend a requirement whole.
//
// Mutation it catches: drop the recommended cap on lexical walk groups in
// admitEntry. `settle` then reaches the plan as required_symbol.
func TestALexicalRootNeverMakesAnEntryRequired(t *testing.T) {
	specs := []fixtureFileSpec{
		{pairsCaller, "refund", model.NodeFunction, "package pay\n\nfunc refund() { settle() }\n", false},
		{pairsCallee, "settle", model.NodeFunction, "package pay\n\nfunc settle() {}\n", false},
	}
	fx := newFixtureFrom(t, specs, func(fx *contextFixture) []model.Relation {
		return []model.Relation{fx.edge(pairsCaller, model.RelCalls, pairsCallee)}
	})
	m, err := intCompiler(t, fx, fx.Now).Compile(fx.ctx, model.ContextRequest{
		Task:  "fix the refund rounding",
		Phase: model.PhaseVerify,
	})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	entries, _, _ := readBackManifest(t, fx, m.ID)
	byNode := map[model.NodeID]model.ContextEntry{}
	for _, e := range entries {
		byNode[e.NodeID] = e
	}
	// The premise: the match was read and walked. Without the caller in the
	// plan the assertion below would pass for want of a walk.
	if _, ok := byNode[fx.Node(pairsCaller)]; !ok {
		t.Fatalf("the task's only lexical match %s is not in the plan; the row has no lexical root to test", pairsCaller)
	}
	callee, ok := byNode[fx.Node(pairsCallee)]
	if !ok {
		t.Fatalf("%s, one calls hop from the lexical root, is not in the plan; the walk from a lexical root did not run",
			pairsCallee)
	}
	switch callee.Requirement {
	case model.RequirementFull, model.RequirementSymbol:
		t.Fatalf("%s reached only from a lexical match is %s; a lexical root lends at most %s",
			pairsCallee, callee.Requirement, model.RequirementRecommended)
	}
}
