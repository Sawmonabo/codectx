package sqlite_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/Sawmonabo/codectx/internal/index"
	"github.com/Sawmonabo/codectx/internal/model"
	store "github.com/Sawmonabo/codectx/internal/storage/sqlite"
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
// Mutation proof: in RecordRunFailure, add `AND status = 'running'` to the
// UPDATE, and the reason of the run already completed is refused.
func TestRunFailureOutlivesTheFailedRunAndDiesWithItsGeneration(t *testing.T) {
	f := newFixture(t, filepath.Join(t.TempDir(), "store.db"))
	snap := f.snapshot("head")
	gen, err := f.s.BeginGeneration(f.ctx, f.repo, snap.ID, model.H("semantic"), "main")
	if err != nil {
		t.Fatalf("BeginGeneration: %v", err)
	}
	run := f.run(gen)
	want := model.RunFailure{ProviderID: providerID, ScopeKey: "profile:b:vendor", Code: model.CodeProviderUnavailable,
		Message: "the indexer exited with status 1", Remediation: "install the indexer, or disable the provider",
		Details: map[string]string{"tool": "an-indexer", model.DetailStderrTail: "a stack trace"}}
	if err := f.s.CompleteProviderRun(f.ctx, model.ProviderResult{RunID: run, State: model.RunFailed}, want.Code); err != nil {
		t.Fatalf("CompleteProviderRun: %v", err)
	}
	if err := f.s.RecordRunFailure(f.ctx, run, want); err != nil {
		t.Fatalf("RecordRunFailure: %v", err)
	}
	kept, omitted, err := f.s.FailedRuns(f.ctx, gen)
	if err != nil || len(kept) != 1 || omitted != 0 {
		t.Fatalf("FailedRuns: %v, %d reasons, %d omitted; want the one reason of the run that failed",
			err, len(kept), omitted)
	}
	got := kept[0]
	if got.ProviderID != want.ProviderID || got.ScopeKey != want.ScopeKey || got.Code != want.Code ||
		got.Message != want.Message || got.Remediation != want.Remediation ||
		got.Details["tool"] != want.Details["tool"] || got.Details[model.DetailStderrTail] != want.Details[model.DetailStderrTail] {
		t.Fatalf("the kept reason is %+v, want %+v", got, want)
	}
	if err := f.s.Abort(f.ctx, gen); err != nil {
		t.Fatalf("Abort: %v", err)
	}
	if err := f.s.DeleteGeneration(f.ctx, gen); err != nil {
		t.Fatalf("DeleteGeneration: %v", err)
	}
	if kept, _, err := f.s.FailedRuns(f.ctx, gen); err != nil || len(kept) != 0 {
		t.Fatalf("FailedRuns after the generation was deleted: %d kept, err=%v; want the row swept with it",
			len(kept), err)
	}
}

// TestASecondProcessReadsWhyAUnitFailedAndWhatIsStillRunning protects the two
// disclosures a status report owes a process that did not do the indexing.
//
// Failure mode: the reason a unit failed and the fact that a scope is still
// being built both lived only in the indexing process -- the reason on a run
// row no command read, the running set in the coordinator's own deferred
// queue. A `codectx status` in another terminal, and the index-status tool in
// a server, therefore reported a capability as merely degraded while the work
// that completes it was still running, and could not say why any scope had no
// facts at all. Both must come off the stored generation, and the reason must
// arrive without the analyzer's standard-error tail, which stays on the run
// row: this answer is what `status --json` hands any client.
//
// Mutation proof: in StatusReader.Status, drop the FailedRuns read, and no
// failed unit is read; drop its withoutRawOutput loop, and the tail is served;
// in Store.Activate, insert 0 for units_running, and neither running scope is
// read.
func TestASecondProcessReadsWhyAUnitFailedAndWhatIsStillRunning(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.db")
	f := newFixture(t, path)
	// A generation with a sealed unit: the capability below is partial, which
	// means one of its scopes DID publish, and a generation with no member at
	// all is never activated.
	gen := sealRepositoryInto(t, f, 2, 4, "interleaved", nil)
	run := f.run(gen)
	reason := model.RunFailure{ProviderID: providerID, ScopeKey: "pkg:java:app", Code: model.CodeProviderUnavailable,
		Message:     "the analyzer produced no facts for this unit",
		Remediation: "check that this unit's source files hold definitions this language family parses",
		Details:     map[string]string{model.DetailStderrTail: "a stack trace"}}
	if err := f.s.CompleteProviderRun(f.ctx, model.ProviderResult{RunID: run, State: model.RunFailed}, reason.Code); err != nil {
		t.Fatalf("CompleteProviderRun: %v", err)
	}
	if err := f.s.RecordRunFailure(f.ctx, run, reason); err != nil {
		t.Fatalf("RecordRunFailure: %v", err)
	}
	var err error
	// The interleaving the fold used to lose: one scope failed, another
	// published, and a third is still building in the background. The failure
	// row outranks the deferred row, so the running scope is disclosed on the
	// surviving row or nowhere at all.
	interleaved := model.CapabilityState{ProviderID: providerID, Capability: "structure",
		Scope: "workspace", State: model.CapabilityPartial, DiagnosticCode: reason.Code,
		Remediation: reason.Remediation, UnitsRunning: 1,
		Details: map[string]string{model.DetailUnitsFailed: "1", model.DetailUnitsPlanned: "3"}}
	// A second provider with work of its own still in flight. units_running is
	// an assertion about the whole generation, so a publication that counted
	// only the scopes of whichever provider it was publishing for would answer
	// zero here, and the second process would be told this capability is
	// simply unavailable while its units are being built.
	//
	// This builds the rows rather than driving a late seal, which no test in
	// this package can reach, so it holds the store and the second handle to
	// the figure and not to the counting; what counts it is internal/index's
	// holdsFreshUnit, over generation_units.
	otherProvider := model.CapabilityState{ProviderID: "dependence", Capability: "calls",
		Scope: "workspace", State: model.CapabilityUnavailable, DiagnosticCode: model.CodeProviderUnavailable,
		UnitsRunning: 2, Details: map[string]string{"reason": "units_deferred"}}
	if _, err = f.s.Activate(f.ctx, gen, 0, model.HealthDegraded,
		[]model.CapabilityState{interleaved, otherProvider}, "norm-v1"); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if err := f.s.Flush(f.ctx); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	// A second handle on the same file, with no coordinator behind it: this is
	// the process that did none of the work and holds none of its state.
	reader, err := store.Open(f.ctx, path, store.WithDerivedReaders(store.Options{ReadOnly: true, BusyTimeout: 500 * time.Millisecond}))
	if err != nil {
		t.Fatalf("the second handle could not be opened: %v", err)
	}
	defer reader.Close()
	status, err := index.NewStatusReader(index.StatusOptions{Store: reader, Repo: f.repo})
	if err != nil {
		t.Fatalf("NewStatusReader: %v", err)
	}
	st, err := status.Status(f.ctx)
	if err != nil {
		t.Fatalf("Status through the second handle: %v", err)
	}
	if err := st.Validate(); err != nil {
		t.Fatalf("the status the second handle answered does not validate: %v", err)
	}
	if len(st.FailedUnits) != 1 || st.FailedUnitsOmitted != 0 {
		t.Fatalf("the second process read %d failed units (%d omitted), want the one that failed",
			len(st.FailedUnits), st.FailedUnitsOmitted)
	}
	got := st.FailedUnits[0]
	if got.ProviderID != reason.ProviderID || got.ScopeKey != reason.ScopeKey ||
		got.Message != reason.Message || got.Remediation != reason.Remediation {
		t.Errorf("the reason the second process read is %+v, want %+v", got, reason)
	}
	if _, ok := got.Details[model.DetailStderrTail]; ok {
		t.Errorf("the second process read the analyzer's standard-error tail on a failed unit")
	}
	running := map[string]int{}
	var row model.CapabilityState
	for _, c := range st.Completeness {
		running[c.ProviderID] = c.UnitsRunning
		if c.ProviderID == providerID {
			row = c
		}
	}
	if len(st.Completeness) != 2 {
		t.Fatalf("the second process read %d capability rows, want two: %+v", len(st.Completeness), st.Completeness)
	}
	if running[providerID] != 1 {
		t.Errorf("the row reports units_running=%d, want 1: the scope still being built was disclosed to nobody",
			running[providerID])
	}
	if running[otherProvider.ProviderID] != 2 {
		t.Errorf("the other provider reports units_running=%d, want 2: the scopes queued for a provider other "+
			"than the one being published for were counted by nobody", running[otherProvider.ProviderID])
	}
	if row.Remediation != reason.Remediation {
		t.Errorf("the row carries remediation %q, want %q", row.Remediation, reason.Remediation)
	}
}
