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
// Mutation proof: delete either `g.runsTotal++` in generation.go and
// RunsOmitted reports 0 (or an under-count) against the expected 5.
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
	if got := g.runsTotal - int64(len(g.runs)); got != extra {
		t.Fatalf("RunsOmitted would be %d, want %d: runs past the ceiling are dropped in silence", got, extra)
	}
}
