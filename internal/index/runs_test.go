package index

import (
	"log/slog"
	"strconv"
	"testing"
	"time"

	"github.com/Sawmonabo/codectx/internal/config"
	"github.com/Sawmonabo/codectx/internal/index/plan"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/snapshot"
)

// TestRunsOmittedCountsEveryRunPastTheCeiling protects the honesty of the run
// list. Runs is a wire-sized page -- one response carries at most
// model.MaxRecordsPerResult runs -- but the generation used to stop appending
// there with no counter, no log line and no Warnings entry, so a repository
// with more provider runs than one response may carry published a list that
// silently claimed to be the whole of it.
//
// It asserts what the generation PUBLISHES -- model.IndexResult.RunsOmitted as
// generation.publish fills it, through the same runsPage the publish site uses
// -- and never recomputes the subtraction itself: a test that recomputed it
// would pass with the published field hard-wired to 0, which is exactly the
// silence it exists to catch.
//
// Mutation proof: delete either `g.runsTotal++` in generation.go, or return a
// constant 0 from runsPage, and RunsOmitted reports 0 against the expected 5.
func TestRunsOmittedCountsEveryRunPastTheCeiling(t *testing.T) {
	t.Parallel()
	const extra = 5
	g := &generation{caps: newCapabilityReport(), c: &Coordinator{log: slog.Default()}}
	for i := range model.MaxRecordsPerResult + extra {
		g.record(plan.Unit{ProviderID: "dependence"}, outcome{result: model.ProviderResult{RunID: model.ProviderRunID("run-" + strconv.Itoa(i)),
			State: model.RunSucceeded}})
	}
	if len(g.runs) != model.MaxRecordsPerResult {
		t.Fatalf("the run page holds %d runs, want the %d-run ceiling", len(g.runs), model.MaxRecordsPerResult)
	}
	runs, omitted := g.runsPage()
	res := model.IndexResult{Runs: runs, RunsOmitted: omitted}
	if len(res.Runs) != model.MaxRecordsPerResult {
		t.Fatalf("the published run list holds %d runs, want the %d-run page", len(res.Runs), model.MaxRecordsPerResult)
	}
	if res.RunsOmitted != extra {
		t.Fatalf("the published RunsOmitted is %d, want %d: runs past the ceiling are dropped in silence",
			res.RunsOmitted, extra)
	}
}

// TestCaptureBuilderCarriesTheConfiguredRetryBudget proves index.capture_max_retries
// and index.capture_retry_deadline are READ. Both are the only operator escape
// hatch from a validated capture of a worktree that never settles: without the
// wiring the builder's own zero values applied and the keys were dead config,
// which is the defect this asserts against.
//
// Mutation proof: drop either field from captureBuilder and this fails on the
// value the builder carries.
func TestCaptureBuilderCarriesTheConfiguredRetryBudget(t *testing.T) {
	t.Parallel()
	cfg := config.Defaults()
	if !cfg.Index.CaptureMaxRetries.IsUnlimited() {
		t.Fatalf("the default capture retry count is %s, want unlimited", cfg.Index.CaptureMaxRetries)
	}
	if cfg.Index.CaptureRetryDeadline.Std() != snapshot.DefaultRetryDeadline {
		t.Fatalf("the default capture retry deadline is %s, want %s",
			cfg.Index.CaptureRetryDeadline.Std(), snapshot.DefaultRetryDeadline)
	}
	cfg.Index.CaptureMaxRetries = config.Limit(3)
	cfg.Index.CaptureRetryDeadline = config.Duration(90 * time.Second)
	g := &generation{c: &Coordinator{opts: Options{Config: cfg}, log: slog.Default()}}
	b := g.captureBuilder(nil)
	if b.MaxRetries != 3 {
		t.Errorf("the capture builder's MaxRetries is %d, want the configured 3", b.MaxRetries)
	}
	if b.RetryDeadline != 90*time.Second {
		t.Errorf("the capture builder's RetryDeadline is %s, want the configured 90s", b.RetryDeadline)
	}
}
