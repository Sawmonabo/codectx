package index

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Sawmonabo/codectx/internal/admission"
	"github.com/Sawmonabo/codectx/internal/config"
	"github.com/Sawmonabo/codectx/internal/index/plan"
	"github.com/Sawmonabo/codectx/internal/ledger"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider"
	"github.com/Sawmonabo/codectx/internal/provider/dependence"
	"github.com/Sawmonabo/codectx/internal/workspace"
)

// TestDeferredUnitsOverlapWithinTheAllocation protects the one invariant the
// background sealer exists to honour under ADR-0010 decision 5: its units
// overlap exactly as far as the machine's one allocation allows, and never one
// at a time. Silent breakage is invisible in every surface the product
// publishes -- the units all seal, the generation publishes, the capability
// rows are identical -- while the deferred work takes as many multiples of the
// wall clock as the allocation would have admitted units, with the rest of the
// machine idle.
//
// It is driven through tickHeld with a queue seeded by hand: plan.Build marks
// a unit heavy, and therefore deferrable, for the dependence provider alone,
// and that provider needs the real analysis engine. The overlap is taken from
// a counter the provider bumps on entry and drops on exit, never from span
// timestamps: on a fixture that runs in milliseconds, timestamps prove nothing
// deterministic.
func TestDeferredUnitsOverlapWithinTheAllocation(t *testing.T) {
	f := newFixture(t, map[string]string{"main.go": "package main\n"})
	ctx := f.ctx
	p := &countingHeavyProvider{}
	c := f.coordinator(append(f.providers(false), p))
	res, err := c.Index(ctx, model.IndexRequest{})
	if err != nil {
		t.Fatalf("Index: %v", err)
	}
	sel, err := c.opts.Registry.Select(ctx, c.opts.Root, c.policy, c.enablement)
	if err != nil {
		t.Fatal(err)
	}
	snap := res.Binding.SnapshotID
	// A machine whose allocation is known, so the reservations below are two
	// fifths and three fifths of it rather than byte counts that assume a host.
	// The base footprint is the one this process derives from the machine and
	// the configuration, exactly as the composition does: what the units are
	// admitted against here is the figure the product would use.
	allocation := dependence.Machine{AvailableBytes: 32 << 30, Observed: true}.
		SchedulingAllocation(config.BaseFootprint(c.opts.Config))
	admissionLedger, err := admission.NewLedger(allocation, 64<<30)
	if err != nil {
		t.Fatalf("the admission ledger was refused: %v", err)
	}
	c.sched = plan.NewScheduler(admissionLedger)

	t.Run("two units that fit the allocation run at once", func(t *testing.T) {
		p.reset(2)
		if err := f.tick(c, deferredScopes("fits-two", 3, allocation*2/5), snap, sel); err != nil {
			t.Fatalf("tickHeld: %v", err)
		}
		if got := p.max.Load(); got != 2 {
			t.Fatalf("at most %d of 3 deferred units ran at once; the allocation admits 2", got)
		}
	})

	t.Run("a unit that fills the allocation runs alone", func(t *testing.T) {
		p.reset(1)
		if err := f.tick(c, deferredScopes("fills-one", 3, allocation*3/5), snap, sel); err != nil {
			t.Fatalf("tickHeld: %v", err)
		}
		if got := p.max.Load(); got != 1 {
			t.Fatalf("%d deferred units ran at once; the allocation admits 1", got)
		}
	})

	t.Run("one unit's failure leaves the others sealed", func(t *testing.T) {
		p.reset(1)
		p.fail = plan.Key(heavyProviderID, "one-fails-1")
		units := deferredScopes("one-fails", 3, allocation*2/5)
		if err := f.tick(c, units, snap, sel); err != nil {
			t.Fatalf("tickHeld: %v", err)
		}
		// The batch publishes the two that sealed: a failure that took its
		// siblings down with it would publish nothing at all.
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
	})
}

// tick seeds the background queue with these units and runs one batch, which
// is what the loop and Drain both call once they have the tick slot.
func (f *fixture) tick(c *Coordinator, units []deferredUnit, snap model.SnapshotID, sel provider.Selection) error {
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
	return l.tickHeld(f.ctx, state)
}

// deferredScopes is n queued units of the counting provider, each reserving the
// same bytes.
func deferredScopes(prefix string, n int, reserve int64) []deferredUnit {
	out := make([]deferredUnit, 0, n)
	for i := range n {
		out = append(out, deferredUnit{unit: plan.Unit{ProviderID: heavyProviderID, ProviderVersion: "1",
			ScopeKey: prefix + "-" + string(rune('0'+i)), Heavy: true,
			Inputs:      func(func(model.UnitInput) error) error { return nil },
			Reservation: dependence.Reservation{HeapCapBytes: reserve}}})
	}
	return out
}

const heavyProviderID = "heavy-fixture"

// rawAnalyzerOutput is the standard-error tail a failed unit of the counting
// provider carries, which the run row keeps and no status answer may.
const rawAnalyzerOutput = "a stack trace naming source paths"

// countingHeavyProvider is a provider that emits nothing and reports how many
// of its units were inside IndexUnit at once. A unit waits there until the
// barrier the case expects is reached, so a correct sealer passes at once and
// a sealer that runs its units one at a time cannot reach it and stops at one.
//
// It is unavailable to detection: no plan derives a unit for it, so the
// foreground run neither builds nor seals the scopes the deferred queue below
// is seeded with.
type countingHeavyProvider struct {
	live    atomic.Int64
	max     atomic.Int64
	barrier atomic.Int64
	// fail is the plan key of the one unit that reports a failure, if any. It
	// is written before the tick and read by the units it runs.
	fail string
}

func (p *countingHeavyProvider) reset(barrier int64) {
	p.live.Store(0)
	p.max.Store(0)
	p.barrier.Store(barrier)
	p.fail = ""
}

func (p *countingHeavyProvider) Descriptor() model.ProviderDescriptor {
	return model.ProviderDescriptor{ID: heavyProviderID, Version: "1", Capabilities: []string{"structure"},
		InvalidationScope: model.InvalidationPackage}
}

func (p *countingHeavyProvider) Detect(context.Context, workspace.Root, workspace.Policy) (provider.Detection, error) {
	return provider.Detection{DiagnosticCode: model.CodeProviderUnavailable}, nil
}

func (p *countingHeavyProvider) IndexUnit(ctx context.Context, req provider.UnitRequest,
	_ provider.Sink) (model.ProviderResult, error) {

	live := p.live.Add(1)
	defer p.live.Add(-1)
	for {
		got := p.max.Load()
		if got >= live || p.max.CompareAndSwap(got, live) {
			break
		}
	}
	// The wait is on the high-water mark and not on the live count, so a unit
	// that runs after the barrier has been reached passes straight through
	// rather than waiting for company that has already been and gone. Bounded:
	// a sealer that admits one unit at a time never reaches the barrier, and
	// the case that expects it then fails on the count rather than hanging
	// until the package timeout.
	deadline := time.Now().Add(5 * time.Second)
	for p.max.Load() < p.barrier.Load() && time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return model.ProviderResult{RunID: req.Run, State: model.RunCanceled}, model.Canceled(ctx.Err())
		case <-time.After(time.Millisecond):
		}
	}
	if p.fail == plan.Key(req.Unit.ProviderID, req.Unit.ScopeKey) {
		return model.ProviderResult{RunID: req.Run, State: model.RunFailed},
			&model.Error{Code: model.CodeProviderOutputInvalid, Message: "the fixture made this deferred unit fail",
				Details: map[string]string{model.DetailStderrTail: rawAnalyzerOutput}}
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
// running. The batch must publish, its reasons must be the published
// generation's, and the run row must say the batch failed.
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
// Mutation: finish the run with endOutcome(err) alone, and a batch whose every
// unit failed reads as an ok run.
func TestAnAllFailedDeferredBatchKeepsItsReason(t *testing.T) {
	f := newFixture(t, map[string]string{"main.go": "package main\n"})
	ctx := f.ctx
	p := &countingHeavyProvider{}
	c := f.coordinator(append(f.providers(false), p))
	res, err := c.Index(ctx, model.IndexRequest{})
	if err != nil {
		t.Fatalf("Index: %v", err)
	}
	sel, err := c.opts.Registry.Select(ctx, c.opts.Root, c.policy, c.enablement)
	if err != nil {
		t.Fatal(err)
	}
	p.reset(1)
	units := deferredScopes("all-fail", 1, 0)
	p.fail = plan.Key(heavyProviderID, units[0].unit.ScopeKey)
	if err := f.tick(c, units, res.Binding.SnapshotID, sel); err != nil {
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
	if err := f.ledger.Flush(ctx); err != nil {
		t.Fatalf("flush the ledger: %v", err)
	}
	reader, ok, err := ledger.OpenReader(ctx, f.dataDir)
	if err != nil || !ok {
		t.Fatalf("OpenReader: %v, present=%v", err, ok)
	}
	t.Cleanup(func() { reader.Close() })
	view, present, err := reader.LatestRun(ctx, string(c.repo), 0)
	if err != nil || !present {
		t.Fatalf("LatestRun: %v, present=%v", err, present)
	}
	if view.Run.Kind != ledger.KindDeferred {
		t.Fatalf("the latest run is a %s run, want the deferred tick's own", view.Run.Kind)
	}
	if view.Run.Outcome != ledger.OutcomeFailed || view.Run.UnitsPlanned != 1 ||
		view.Run.UnitsSucceeded != 0 || view.Run.UnitsFailed != 1 {
		t.Fatalf("the run reads %s with %d planned, %d succeeded, %d failed; want failed with "+
			"1 planned and 1 failed: the batch got nowhere and the row must say so",
			view.Run.Outcome, view.Run.UnitsPlanned, view.Run.UnitsSucceeded, view.Run.UnitsFailed)
	}
	reason := false
	for _, span := range view.Spans {
		if span.ScopeKey == units[0].unit.ScopeKey &&
			span.DiagnosticCode == model.CodeProviderOutputInvalid && span.Failure != "" {
			reason = true
		}
	}
	if !reason {
		t.Fatalf("no span of the failed batch names the scope %q with its typed reason among the %d "+
			"recorded spans",
			units[0].unit.ScopeKey, len(view.Spans))
	}

	// A later batch seals the scope that failed. The publication extends the
	// generation that carries the old reason, and must not carry it on: the
	// scope would be listed failed beside a capability row that reports it
	// covered, in every generation after.
	//
	// Mutation: pass no sealed scopes to CarryRunFailures in publishOnce, and
	// the answered failure is still listed.
	p.reset(1)
	if err := f.tick(c, units, res.Binding.SnapshotID, sel); err != nil {
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
