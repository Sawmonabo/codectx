package index

import (
	"log/slog"
	"strconv"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
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
		g.record(outcome{result: model.ProviderResult{RunID: model.ProviderRunID("run-" + strconv.Itoa(i)),
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
