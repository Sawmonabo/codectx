package context

import (
	"strings"
	"testing"
	"time"

	"github.com/Sawmonabo/codectx/internal/config"
)

// TestLexicalReadEndedByADeadlineIsIncomplete proves a lexical read that a
// query deadline ended leaves the scope incomplete and says so: matches the
// read never reached could rank above the packed files. A one-nanosecond
// resources.query_timeout is spent before the read's first storage read, so
// the read ends on its deadline with nothing read.
//
// Mutation: ignore ReadEnd.Truncated in lexicalSeeds, and the step reports a
// complete scope with no exclusion.
func TestLexicalReadEndedByADeadlineIsIncomplete(t *testing.T) {
	fx := newFixtureFrom(t, closureSpecs(), closureScope)
	fx.Cfg.Resources.QueryTimeout = config.Duration(time.Nanosecond)
	c := intCompiler(t, fx, fx.Now)
	reader, err := c.store.PinGeneration(fx.ctx, c.repo, fx.Binding.GenerationID, time.Minute)
	if err != nil {
		t.Fatalf("PinGeneration: %v", err)
	}
	defer reader.Close()
	b, err := resolveBudget(seedRequest(2).Budget, c.cfg.Context)
	if err != nil {
		t.Fatalf("resolveBudget: %v", err)
	}
	out := seedSet{sink: &collectSeeds{}}
	if err := c.lexicalSeeds(fx.ctx, reader, seedTask, b, roundInputs{}, &out); err != nil {
		t.Fatalf("lexicalSeeds under an expired budget: %v: a deadline ends the read, not the compile", err)
	}
	if !out.Unresolved {
		t.Fatal("a lexical read the deadline ended reported a complete scope")
	}
	if len(out.Excluded) != 1 || !strings.Contains(out.Excluded[0].Excluded, "resources.query_timeout") {
		t.Fatalf("the exclusions are %+v, want one row naming the query time budget", out.Excluded)
	}
}
