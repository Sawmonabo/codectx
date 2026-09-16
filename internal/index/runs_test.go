package index

import (
	"context"
	"log/slog"
	"strconv"
	"testing"
	"time"

	"github.com/Sawmonabo/codectx/internal/config"
	"github.com/Sawmonabo/codectx/internal/index/plan"
	"github.com/Sawmonabo/codectx/internal/ledger"
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
	b := g.captureBuilder()
	if b.MaxRetries != 3 {
		t.Errorf("the capture builder's MaxRetries is %d, want the configured 3", b.MaxRetries)
	}
	if b.RetryDeadline != 90*time.Second {
		t.Errorf("the capture builder's RetryDeadline is %s, want the configured 90s", b.RetryDeadline)
	}
}

// TestLedgerRecordsEveryStageAndAgreesWithTheResult protects the one invariant
// the run ledger exists for: the ledger and the published result cannot
// disagree about what the run did. Both are meant to read the counters the
// stages already keep, so a stage that counts into the ledger separately from
// the result would let `status --resources` report a run the result contradicts
// -- and an operator has no way to tell which of the two is lying.
//
// It also asserts that every top-level stage this fixture exercises finished
// with a measured wall: a stage whose span is never ended reads as running for
// ever and its cost is simply missing from the breakdown.
//
// The three activation stages are asserted for a second reason: each of them
// used to log its own duration, and those lines were removed once the span
// carried the same wall. Removing a log line must not remove the fact it
// carried, so a missing span here is the information going silently missing.
//
// Mutation proof: delete the `walk.End(...)` call in generation.capture and the
// walk stage reads as unfinished; or drop the lexical_build span's End in
// buildLexical and that stage reads unfinished too.
func TestLedgerRecordsEveryStageAndAgreesWithTheResult(t *testing.T) {
	f := newFixture(t, map[string]string{"main.go": "package main\n\nfunc main() {}\n"})
	res, err := f.c.Index(f.ctx, model.IndexRequest{})
	if err != nil {
		t.Fatalf("Index: %v", err)
	}
	view := f.latestRun(res.Binding.GenerationID)
	if view.Run.Outcome != ledger.OutcomeOK || view.Run.GenerationID == nil ||
		*view.Run.GenerationID != int64(res.Binding.GenerationID) {
		t.Fatalf("the run row is %+v, want an ok run attached to generation %d",
			view.Run, res.Binding.GenerationID)
	}
	if view.Run.FileCount != res.FilesCaptured ||
		view.Run.UnitsSucceeded != res.UnitsReused+res.UnitsBuilt+res.UnitsCarried {
		t.Fatalf("the run row counts %d files and %d succeeded units; the result publishes %d files and %d",
			view.Run.FileCount, view.Run.UnitsSucceeded, res.FilesCaptured,
			res.UnitsReused+res.UnitsBuilt+res.UnitsCarried)
	}
	// The stages activation opens for itself, spelled where they are opened:
	// internal/storage/sqlite opens them under the activation span, so this
	// package cannot name their constants.
	inner := map[string]bool{"lexical_compaction": true, "adjacency": true, "lexical_build": true}
	stages := map[string]ledger.SpanRow{}
	for _, span := range view.Spans {
		if span.ParentSeq == nil || span.Stage == stageWalk || inner[span.Stage] {
			stages[span.Stage] = span
		}
	}
	for _, want := range []string{stageCapture, stageWalk, stagePlan, stageAttachReused,
		stageAttachCarried, stageBuild, stageCoverage, stageActivation, stageRetention,
		"lexical_compaction", "adjacency", "lexical_build"} {
		span, ok := stages[want]
		if !ok {
			t.Errorf("the run recorded no %q stage", want)
			continue
		}
		if span.FinishedAt == nil || span.Running {
			t.Errorf("the %q stage reads unfinished (%+v): its cost is missing from the breakdown", want, span)
		}
	}
	if got := stages[stageCapture]; got.ItemsOut != res.FilesCaptured {
		t.Errorf("the capture stage counted %d files out; the result publishes %d captured",
			got.ItemsOut, res.FilesCaptured)
	}
	if got := stages[stageBuild]; got.ItemsOut != res.UnitsBuilt {
		t.Errorf("the build stage counted %d units out; the result publishes %d built",
			got.ItemsOut, res.UnitsBuilt)
	}
}

// TestRunThatNeverReachedAGenerationIsStillRecorded protects a run that failed
// before publication from being an invisible run. The generation id does not
// exist until BeginGeneration returns, so a ledger keyed on it would record
// nothing at all for a failure in the capture, the plan or the ref -- which is
// exactly the run an operator is investigating. The run id is allocated at the
// start of the attempt instead, and the generation is attached later or never.
//
// Mutation proof: move the newRun/Context/Finish block in generation.attempt to
// after BeginGeneration and this fails with no run recorded at all.
func TestRunThatNeverReachedAGenerationIsStillRecorded(t *testing.T) {
	f := newFixture(t, map[string]string{"main.go": "package main\n"})
	ctx, cancel := context.WithCancel(f.ctx)
	cancel()
	if _, err := f.c.Index(ctx, model.IndexRequest{}); err == nil {
		t.Fatal("a cancelled index reported success")
	}
	view := f.latestRun(0)
	if view.Run.GenerationID != nil {
		t.Errorf("the failed run carries generation %d; it never opened one", *view.Run.GenerationID)
	}
	if view.Run.Outcome != ledger.OutcomeFailed {
		t.Errorf("the failed run's outcome is %q, want %q", view.Run.Outcome, ledger.OutcomeFailed)
	}
}
