package index

import (
	"context"
	"maps"
	"path/filepath"
	"slices"
	"testing"

	"github.com/Sawmonabo/codectx/internal/index/plan"
	"github.com/Sawmonabo/codectx/internal/ledger"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider"
	"github.com/Sawmonabo/codectx/internal/provider/dependence"
	"github.com/Sawmonabo/codectx/internal/storage/sqlite"
	"github.com/Sawmonabo/codectx/internal/workspace"
)

// TestOneDeferredUnitsFailureLeavesTheOthersSealed protects the isolation of a
// deferred batch's units: one unit's failure must not take its siblings down,
// and the batch publishes the units that sealed.
//
// It is driven through tickHeld with a queue seeded by hand: plan.Build marks
// a unit heavy, and therefore deferrable, for the dependence provider alone,
// and that provider needs the real analysis engine.
//
// Mutation: return the first unit's error from tickHeld instead of recording
// it, and nothing is published and the two siblings are not sealed.
func TestOneDeferredUnitsFailureLeavesTheOthersSealed(t *testing.T) {
	f := newFixture(t, map[string]string{"main.go": "package main\n"})
	ctx := f.ctx
	p := &heavyFixtureProvider{}
	c := f.coordinator(append(f.providers(false), p))
	res, err := c.Index(ctx, model.IndexRequest{})
	if err != nil {
		t.Fatalf("Index: %v", err)
	}
	sel, err := c.opts.Registry.Select(ctx, c.opts.Root, c.policy, c.enablement)
	if err != nil {
		t.Fatal(err)
	}
	p.fail = plan.Key(heavyProviderID, "one-fails-1")
	units := deferredScopes("one-fails", 3)
	if err := f.tick(ctx, c, units, res.Binding.SnapshotID, sel); err != nil {
		t.Fatalf("tickHeld: %v", err)
	}
	published := c.late.take()
	built := int64(-1)
	if len(published) == 1 {
		built = published[0].UnitsBuilt
	}
	if built != 2 {
		t.Fatalf("the batch published %d result(s) carrying %d built units, want one carrying 2",
			len(published), built)
	}
	for i, d := range units {
		spec, err := d.unit.Spec(c.cfgHash)
		if err != nil {
			t.Fatal(err)
		}
		state, exists, err := f.store.UnitState(ctx, spec.ID)
		if err != nil {
			t.Fatal(err)
		}
		sealed := exists && state == model.UnitSealed
		if want := i != 1; sealed != want {
			t.Fatalf("%s sealed=%v, want %v", d.unit.ScopeKey, sealed, want)
		}
	}
}

// TestACancelledDeferredBatchKeepsItsUnitsQueued protects what a cancelled
// deferred batch leaves outstanding.
//
// Failure mode: an interrupted Drain or a closing coordinator cancels a batch
// whose units are running; the units it had popped are neither re-queued nor
// counted, so Pending reads zero, the one-shot command reports a clean run
// with no "deferred units are not built" warning, and the work is lost in
// silence. Every unit must be back on the queue, in the order it was queued.
//
// That includes a unit that failed before the cancellation: its run row is the
// aborted work generation's, so no publication can list its reason, and a
// failure counted for it would be one status never names, while dropping it
// would leave its scope reported running with nothing queued to finish it.
//
// Mutation: pass requeue=false to settle on the cancelled path of tickHeld,
// and Pending reports no unit. Mutation: have settle skip the units that
// failed, and the queue is missing the failed scope.
func TestACancelledDeferredBatchKeepsItsUnitsQueued(t *testing.T) {
	f := newFixture(t, map[string]string{"main.go": "package main\n"})
	p := &heavyFixtureProvider{}
	c := f.coordinator(append(f.providers(false), p))
	res, err := c.Index(f.ctx, model.IndexRequest{})
	if err != nil {
		t.Fatalf("Index: %v", err)
	}
	sel, err := c.opts.Registry.Select(f.ctx, c.opts.Root, c.policy, c.enablement)
	if err != nil {
		t.Fatal(err)
	}
	units := deferredScopes("cancelled", 3)
	p.fail = plan.Key(heavyProviderID, units[1].unit.ScopeKey)
	p.entered = make(chan struct{}, len(units))
	ctx, cancel := context.WithCancel(f.ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- f.tick(ctx, c, units, res.Binding.SnapshotID, sel) }()
	// Every unit is inside its provider before the cancellation, so each one
	// is cancelled mid-build rather than refused at a storage call, and the
	// failing one has already returned its failure.
	for range units {
		<-p.entered
	}
	cancel()
	if err := <-done; err == nil {
		t.Fatal("a cancelled batch reported success")
	}
	if got := c.Pending().Units; got != len(units) {
		t.Fatalf("Pending reports %d units after a cancelled batch, want the %d it had popped", got, len(units))
	}
	c.late.mu.Lock()
	queued := make([]string, 0, len(c.late.queue))
	for _, d := range c.late.queue {
		queued = append(queued, d.unit.ScopeKey)
	}
	c.late.mu.Unlock()
	want := make([]string, 0, len(units))
	for _, d := range units {
		want = append(want, d.unit.ScopeKey)
	}
	if !slices.Equal(queued, want) {
		t.Fatalf("the queue holds %v after a cancelled batch, want %v in their queued order", queued, want)
	}
}

// tick seeds the background queue with these units and runs one batch, which
// is what the loop and Drain both call once they have the tick slot.
func (f *fixture) tick(ctx context.Context, c *Coordinator, units []deferredUnit, snap model.SnapshotID,
	sel provider.Selection) error {
	f.t.Helper()
	l := c.late
	l.mu.Lock()
	l.queue = append(l.queue[:0], units...)
	// Undelivered publications of an earlier batch are dropped, so what take
	// answers afterwards is this batch's own.
	l.published = nil
	l.state = queueState{snap: snap, sel: sel, ref: refNone, epoch: l.state.epoch + 1}
	state := l.state
	l.mu.Unlock()
	return l.tickHeld(ctx, state)
}

// deferredScopes is n queued units of the fixture provider, each reserving
// nothing, so admission never holds one back.
func deferredScopes(prefix string, n int) []deferredUnit {
	out := make([]deferredUnit, 0, n)
	for i := range n {
		out = append(out, deferredUnit{unit: plan.Unit{ProviderID: heavyProviderID, ProviderVersion: "1",
			ScopeKey: prefix + "-" + string(rune('0'+i)), Heavy: true,
			Inputs:      func(func(model.UnitInput) error) error { return nil },
			Reservation: dependence.Reservation{}}})
	}
	return out
}

const heavyProviderID = "heavy-fixture"

// rawAnalyzerOutput is the standard-error tail a failed unit of the fixture
// provider carries, which the run row keeps and no status answer may.
const rawAnalyzerOutput = "a stack trace naming source paths"

// heavyFixtureProvider is a provider that emits nothing. The unit whose plan
// key is fail reports a failure at once; with entered set, every unit
// signals it first, and every unit but that one then holds until its context
// ends.
//
// It is unavailable to detection: no plan derives a unit for it, so the
// foreground run neither builds nor seals the scopes the deferred queue is
// seeded with.
type heavyFixtureProvider struct {
	// fail and entered are written before the tick and read by the units it
	// runs.
	fail    string
	entered chan struct{}
}

func (p *heavyFixtureProvider) Descriptor() model.ProviderDescriptor {
	return model.ProviderDescriptor{ID: heavyProviderID, Version: "1", Capabilities: []string{"structure"},
		InvalidationScope: model.InvalidationPackage}
}

func (p *heavyFixtureProvider) Detect(context.Context, workspace.Root, workspace.Policy) (provider.Detection, error) {
	return provider.Detection{DiagnosticCode: model.CodeProviderUnavailable}, nil
}

func (p *heavyFixtureProvider) IndexUnit(ctx context.Context, req provider.UnitRequest,
	_ provider.Sink) (model.ProviderResult, error) {

	if p.entered != nil {
		p.entered <- struct{}{}
	}
	if p.fail == plan.Key(req.Unit.ProviderID, req.Unit.ScopeKey) {
		return model.ProviderResult{RunID: req.Run, State: model.RunFailed},
			&model.Error{Code: model.CodeProviderOutputInvalid, Message: "the fixture made this deferred unit fail",
				Details: map[string]string{model.DetailStderrTail: rawAnalyzerOutput}}
	}
	if p.entered != nil {
		<-ctx.Done()
		return model.ProviderResult{RunID: req.Run, State: model.RunCanceled}, model.Canceled(ctx.Err())
	}
	return model.ProviderResult{RunID: req.Run, State: model.RunSucceeded}, nil
}

// TestAnAllFailedDeferredBatchKeepsItsReason protects the reasons of a
// deferred batch whose every unit failed.
//
// Failure mode: the provider-run rows holding each unit's reason are written
// against the work generation the tick aborts, and status reads the reasons of
// the ACTIVE generation, so a deferred failure reached no failed_units list
// while the capability row named it; a batch whose every unit failed published
// nothing at all and left the active generation claiming its scopes were still
// running. The batch must publish, and its reasons must be the published
// generation's.
//
// It also protects the privacy rule on the status answer: the analyzer's
// standard-error tail stays on the run row and never reaches failed_units,
// which `status --json` and the index-status tool hand to any client.
//
// A reason is carried only while its scope stays failed: once a later batch
// seals the scope, its old reason must leave failed_units.
//
// Mutation: in tickHeld, abort and return when nothing sealed, and no
// publication is delivered. Mutation: drop the CarryRunFailures call from
// publishOnce, and status lists no failed unit. Mutation: drop the
// withoutRawOutput loop from StatusReader.Status, and the tail is served.
func TestAnAllFailedDeferredBatchKeepsItsReason(t *testing.T) {
	f := newFixture(t, map[string]string{"main.go": "package main\n"})
	ctx := f.ctx
	p := &heavyFixtureProvider{}
	c := f.coordinator(append(f.providers(false), p))
	res, err := c.Index(ctx, model.IndexRequest{})
	if err != nil {
		t.Fatalf("Index: %v", err)
	}
	sel, err := c.opts.Registry.Select(ctx, c.opts.Root, c.policy, c.enablement)
	if err != nil {
		t.Fatal(err)
	}
	units := deferredScopes("all-fail", 1)
	p.fail = plan.Key(heavyProviderID, units[0].unit.ScopeKey)
	if err := f.tick(ctx, c, units, res.Binding.SnapshotID, sel); err != nil {
		t.Fatalf("tickHeld: %v", err)
	}
	published := c.late.take()
	if len(published) != 1 {
		t.Fatalf("a batch whose every unit failed delivered %d publication(s), want one", len(published))
	}
	st, err := f.status(c)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if err := st.Validate(); err != nil {
		t.Fatalf("the status answer does not validate: %v", err)
	}
	if st.Binding.GenerationID != published[0].Binding.GenerationID {
		t.Fatalf("status answers generation %d, want the publication %d",
			st.Binding.GenerationID, published[0].Binding.GenerationID)
	}
	if len(st.FailedUnits) != 1 || st.FailedUnits[0].ScopeKey != units[0].unit.ScopeKey {
		t.Fatalf("status lists failed units %+v, want the one deferred scope that failed", st.FailedUnits)
	}
	if _, ok := st.FailedUnits[0].Details[model.DetailStderrTail]; ok {
		t.Fatal("the status answer carries the analyzer's standard-error tail")
	}
	kept, _, err := f.store.FailedRuns(ctx, st.Binding.GenerationID)
	if err != nil || len(kept) != 1 || kept[0].Details[model.DetailStderrTail] != rawAnalyzerOutput {
		t.Fatalf("the run row keeps %+v (err=%v), want the reason with its standard-error tail", kept, err)
	}
	// A later batch seals the scope that failed. The publication extends the
	// generation that carries the old reason, and must not carry it on: the
	// scope would be listed failed beside a capability row that reports it
	// covered, in every generation after.
	//
	// Mutation: pass no sealed scopes to CarryRunFailures in publishOnce, and
	// the answered failure is still listed.
	p.fail = ""
	if err := f.tick(ctx, c, units, res.Binding.SnapshotID, sel); err != nil {
		t.Fatalf("tickHeld (the scope seals): %v", err)
	}
	if n := len(c.late.take()); n != 1 {
		t.Fatalf("the batch that sealed the scope delivered %d publication(s), want one", n)
	}
	if st, err = f.status(c); err != nil {
		t.Fatalf("status: %v", err)
	}
	if len(st.FailedUnits) != 0 {
		t.Fatalf("status still lists %+v as failed after a later batch sealed it", st.FailedUnits)
	}
}

// TestAnAbandonedDeferredUnitReadsFailedNotRunning protects the status of the
// deferred units a failed publication lost. Nothing requeues them, so a row
// that still counts them running reports work in flight that nothing will
// finish, the over-claim Section 13.3 forbids: the row must read failed, with
// reason publication_failed, the publication error's code and the deferred
// run's id. Once a later publication has folded them into its own rows they
// must not be subtracted from its count a second time.
//
// Mutation: answer nil from abandonedFailures, and the row still
// reports two units running; drop the clear from keepFailures, and the
// published generation's row is rewritten again.
func TestAnAbandonedDeferredUnitReadsFailedNotRunning(t *testing.T) {
	l := newLateSealer(nil)
	l.state = queueState{snap: "snap", epoch: 1}
	units := deferredScopes("lost", 2)
	l.abandon(1, units, &model.Error{Code: model.CodeDiskFull, Message: "the store is full"}, "run-1")
	rows := []model.CapabilityState{{ProviderID: heavyProviderID, Capability: "dependence",
		Scope: provider.ScopeWorkspace, State: model.CapabilityUnavailable,
		DiagnosticCode: model.CodeProviderUnavailable, UnitsRunning: 2,
		Details: map[string]string{"reason": reasonUnitsDeferred}}}
	row := projectAbandoned(rows, l.abandonedFailures("snap"))[0]
	if row.State != model.CapabilityFailed || row.UnitsRunning != 0 || row.DiagnosticCode != model.CodeDiskFull ||
		row.Details["reason"] != reasonPublicationFailed || row.Details[detailRunID] != "run-1" ||
		row.Details[model.DetailUnitsFailed] != "2" {
		t.Fatalf("the abandoned units read %+v, want failed, none running, %s, %s and run-1",
			row, model.CodeDiskFull, reasonPublicationFailed)
	}
	if err := row.Validate(); err != nil {
		t.Fatalf("the projected row does not validate: %v", err)
	}
	if other := projectAbandoned(rows, l.abandonedFailures("another-snap"))[0]; other.State != model.CapabilityUnavailable {
		t.Fatalf("a row over another snapshot was rewritten to %+v", other)
	}
	if kept := l.backgroundFailures(nil); len(kept) != 2 ||
		kept[plan.Key(heavyProviderID, units[0].unit.ScopeKey)].code != model.CodeDiskFull {
		t.Fatalf("the next publication's failed scopes are %+v, want both abandoned units", kept)
	}
	l.keepFailures(1, nil)
	if again := projectAbandoned(rows, l.abandonedFailures("snap"))[0]; again.State != model.CapabilityUnavailable {
		t.Fatalf("units a publication already stated were projected again: %+v", again)
	}
}

// TestAnAbandonedDeferredUnitReadsFailedInAnotherProcess protects the same
// status in every process but the one whose publication failed. That process
// alone holds the in-process record, so a `codectx status` or the index-status
// tool of another process would read the abandoned units as still running for
// as long as the active generation stands. It must read them from the run
// ledger -- the deferred run's abandonment marker over its unit spans -- as
// failed, with reason publication_failed, the publication error's code and
// that run's id, and only while the generation the marker names is active.
//
// The tick is real and its publication really fails: its units seal, and
// publishOnce's plan is built from the tick's own selection, which here
// carries an active provider with no detection, so plan.Build refuses it.
// Nothing in the tick before the publication reads that selection. The reader
// is built as another process builds one: a second, read-only store handle, a
// run-ledger reader over the data directory that was never attached, and no
// deferred sealer.
//
// Mutation: remove the markAbandoned call from tickHeld's publication-failure
// path, or drop the scope_key match from MarkedUnits, and the row still
// reports two units running, or a row over another generation is rewritten.
func TestAnAbandonedDeferredUnitReadsFailedInAnotherProcess(t *testing.T) {
	f := newFixture(t, map[string]string{"main.go": "package main\n"})
	ctx := f.ctx
	c := f.coordinator(append(f.providers(false), &heavyFixtureProvider{}))
	res, err := c.Index(ctx, model.IndexRequest{})
	if err != nil {
		t.Fatalf("Index: %v", err)
	}
	sel, err := c.opts.Registry.Select(ctx, c.opts.Root, c.policy, c.enablement)
	if err != nil {
		t.Fatal(err)
	}
	if len(sel.Active) == 0 {
		t.Fatal("the selection holds no active provider to strip a detection from")
	}
	broken := sel
	broken.Detections = maps.Clone(sel.Detections)
	delete(broken.Detections, sel.Active[0].Descriptor().ID)
	units := deferredScopes("lost", 2)
	if err := f.tick(ctx, c, units, res.Binding.SnapshotID, broken); err == nil {
		t.Fatal("the tick's publication succeeded over a selection plan.Build refuses")
	}
	c.late.mu.Lock()
	lost, ok := c.late.abandoned[plan.Key(heavyProviderID, units[0].unit.ScopeKey)]
	c.late.mu.Unlock()
	if !ok {
		t.Fatal("the failed publication abandoned nothing in the process that ran it")
	}
	runID, code := lost.failure.details[detailRunID], lost.failure.code
	if err := f.ledger.Flush(ctx); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	second, err := sqlite.Open(ctx, filepath.Join(f.dataDir, "codectx.db"), sqlite.Options{ReadOnly: true})
	if err != nil {
		t.Fatalf("a second, read-only store handle: %v", err)
	}
	t.Cleanup(func() { second.Close() })
	other, err := NewStatusReader(StatusOptions{Root: c.opts.Root, Config: c.opts.Config, Store: second,
		Git: c.opts.Git, Repo: c.repo, RunLedger: dirLedger{f.dataDir}})
	if err != nil {
		t.Fatal(err)
	}
	active, err := second.ActiveGeneration(ctx, c.repo)
	if err != nil {
		t.Fatal(err)
	}
	if active != res.Binding.GenerationID {
		t.Fatalf("the failed publication moved the active generation to %d from %d", active, res.Binding.GenerationID)
	}
	rows := []model.CapabilityState{{ProviderID: heavyProviderID, Capability: "structure",
		Scope: provider.ScopeWorkspace, State: model.CapabilityUnavailable,
		DiagnosticCode: model.CodeProviderUnavailable, UnitsRunning: 2,
		Details: map[string]string{"reason": reasonUnitsDeferred}}}
	abandoned, err := other.recordedAbandonment(ctx, active)
	if err != nil {
		t.Fatalf("recordedAbandonment: %v", err)
	}
	row := projectAbandoned(rows, abandoned)[0]
	if row.State != model.CapabilityFailed || row.UnitsRunning != 0 || row.DiagnosticCode != code ||
		row.Details["reason"] != reasonPublicationFailed || runID == "" || row.Details[detailRunID] != runID ||
		row.Details[model.DetailUnitsFailed] != "2" {
		t.Fatalf("another process reads the abandoned units as %+v, want failed, none running, %s, %s and run %q",
			row, code, reasonPublicationFailed, runID)
	}
	if err := row.Validate(); err != nil {
		t.Fatalf("the projected row does not validate: %v", err)
	}
	later, err := other.recordedAbandonment(ctx, active+1)
	if err != nil {
		t.Fatalf("recordedAbandonment: %v", err)
	}
	if again := projectAbandoned(rows, later)[0]; again.State != model.CapabilityUnavailable {
		t.Fatalf("a row over another generation was rewritten to %+v", again)
	}
}

// dirLedger is the run-ledger reader another process composes: it opens the
// file in the data directory per call and only reads. Status asks it for the
// marked units alone.
type dirLedger struct{ dir string }

func (dirLedger) Run(context.Context, string) (*model.RunRecord, []model.StageRecord, int64, error) {
	return nil, nil, 0, nil
}

func (dirLedger) OpenPeaks(context.Context) (PeakLookup, error) { return nil, nil }

func (d dirLedger) MarkedUnits(ctx context.Context, repositoryID string, generationID int64, stage string,
	named int) ([]ledger.MarkedUnits, error) {
	reader, recorded, err := ledger.OpenReader(ctx, d.dir)
	if err != nil || !recorded {
		return nil, err
	}
	defer reader.Close()
	return reader.MarkedUnits(ctx, repositoryID, generationID, stage, named)
}
