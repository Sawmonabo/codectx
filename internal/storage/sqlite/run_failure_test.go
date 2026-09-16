package sqlite_test

import (
	"path/filepath"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
)

// TestRunFailureOutlivesTheFailedRunAndDiesWithItsGeneration protects the one
// durable home a failed unit's reason has.
//
// Failure mode: a unit that never sealed leaves no row of its own, and the
// capability report publishes one exemplar scope per provider capability and
// excludes the tool output entirely. If the reason does not survive the run's
// completion, an operator asking why a scope has no facts has only a log line
// that has already scrolled away -- and the readiness ledger has nothing at
// all to read. The reason must also not outlive the generation it is about:
// the run row is swept once that generation is gone, and a reason kept for a
// generation nobody can name is a row that grows without bound.
//
// Mutation proof: in CompleteProviderRun, add `failure_json = ”` to the
// UPDATE, and the completed run reports no reason.
func TestRunFailureOutlivesTheFailedRunAndDiesWithItsGeneration(t *testing.T) {
	f := newFixture(t, filepath.Join(t.TempDir(), "store.db"))
	snap := f.snapshot("head")
	gen, err := f.s.BeginGeneration(f.ctx, f.repo, snap.ID, model.H("semantic"), "main")
	if err != nil {
		t.Fatalf("BeginGeneration: %v", err)
	}
	run := f.run(gen)
	want := model.RunFailure{ScopeKey: "profile:b:vendor", Code: model.CodeProviderUnavailable,
		Message: "the indexer exited with status 1",
		Details: map[string]string{"tool": "an-indexer", model.DetailStderrTail: "a stack trace"}}
	if err := f.s.CompleteProviderRun(f.ctx, model.ProviderResult{RunID: run, State: model.RunFailed}, want.Code); err != nil {
		t.Fatalf("CompleteProviderRun: %v", err)
	}
	if err := f.s.RecordRunFailure(f.ctx, run, want); err != nil {
		t.Fatalf("RecordRunFailure: %v", err)
	}
	got, ok, err := f.s.RunFailure(f.ctx, run)
	if err != nil || !ok {
		t.Fatalf("RunFailure: %v, kept=%v; want the reason of the run that failed", err, ok)
	}
	if got.ScopeKey != want.ScopeKey || got.Code != want.Code || got.Message != want.Message ||
		got.Details["tool"] != want.Details["tool"] || got.Details[model.DetailStderrTail] != want.Details[model.DetailStderrTail] {
		t.Fatalf("the kept reason is %+v, want %+v", got, want)
	}
	if err := f.s.Abort(f.ctx, gen); err != nil {
		t.Fatalf("Abort: %v", err)
	}
	if err := f.s.DeleteGeneration(f.ctx, gen); err != nil {
		t.Fatalf("DeleteGeneration: %v", err)
	}
	if _, ok, err := f.s.RunFailure(f.ctx, run); err != nil || ok {
		t.Fatalf("RunFailure after the generation was deleted: kept=%v err=%v; want the row swept with it", ok, err)
	}
}
