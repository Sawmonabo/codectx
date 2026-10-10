package context

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
)

// TestPlainWordsAreNotResolvedAsDeclarations pins the identifier-shape rule of
// exact resolution: in "fix the parseConfig timeout", parseConfig is an exact
// seed and the plain word fix is not, even though the snapshot declares both.
//
// Mutation caught: sending every free word of two or more characters to exact
// resolution makes the declaration named fix an originExactResolve seed.
func TestPlainWordsAreNotResolvedAsDeclarations(t *testing.T) {
	specs := []fixtureFileSpec{
		{"internal/cmd/fix.go", "fix", model.NodeFunction, "package cmd\n\nfunc fix() {}\n", false},
		{"internal/config/parse.go", "parseConfig", model.NodeFunction, "package config\n\nfunc parseConfig() error { return nil }\n", false},
	}
	fx := newFixtureFrom(t, specs, nil)
	svc := searchService(t, fx)
	reader, err := fx.Store.PinGeneration(fx.ctx, fx.Repo, fx.Gen, time.Minute)
	if err != nil {
		t.Fatalf("PinGeneration: %v", err)
	}
	defer reader.Close()
	c := &Compiler{store: fx.Store, repo: fx.Repo, search: svc, cfg: fx.Cfg,
		now: fx.Now, log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	sink := &collectSeeds{}
	if _, err := c.extractSeeds(fx.ctx, reader, fx.Gen, model.ContextRequest{
		Task:  "fix the parseConfig timeout",
		Phase: model.PhaseVerify,
	}, resolvedBudget{MaxBytes: fx.Cfg.Context.DefaultMaxBytes, MaxTokens: fx.Cfg.Context.DefaultEstimatedTokens,
		MaxFiles: fx.Cfg.Context.DefaultMaxFiles, MaxSlices: fx.Cfg.Context.MaxSlices}, roundInputs{}, sink); err != nil {
		t.Fatalf("extractSeeds: %v", err)
	}

	fixID := model.NewNodeID(fx.Repo, model.NodeFunction, model.CanonicalNodeKey("internal/cmd/fix.go", "fix"))
	parseID := model.NewNodeID(fx.Repo, model.NodeFunction, model.CanonicalNodeKey("internal/config/parse.go", "parseConfig"))
	exact := map[model.NodeID]bool{}
	for _, cnd := range sink.cands {
		if cnd.Origin == originExactResolve {
			exact[cnd.NodeID] = true
		}
	}
	if !exact[parseID] {
		t.Fatalf("the identifier parseConfig is not an exact seed; exact seeds were %v", exact)
	}
	if exact[fixID] {
		t.Fatalf("the plain word fix was resolved as a declaration; exact seeds were %v", exact)
	}
}
