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
	admissionLedger, err := admission.NewLedger(allocation)
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
			&model.Error{Code: model.CodeProviderOutputInvalid, Message: "the fixture made this deferred unit fail"}
	}
	return model.ProviderResult{RunID: req.Run, State: model.RunSucceeded}, nil
}

// TestAnAllFailedDeferredBatchKeepsItsReason protects the record of the one
// deferred outcome that has nowhere else to go. When every unit of a batch
// fails, nothing is published: no capability row is written, because there is
// no publication generation, and the provider-run rows that hold each unit's
// reason are written against the work generation the tick aborts. The run
// ledger is what is left, and a tick that reported itself as an ok run with no
// units, or whose rows were swept because it reached no generation, would leave
// an operator with a log line and nothing durable at all.
//
// Mutation: report the totals only on the publishing path (as the index path
// did) and the run reads `0 planned, 0 succeeded` for a batch it ran.
//
// Mutation: finish the run with endOutcome(err) alone and a batch whose every
// unit failed reads as an ok run.
//
// Mutation: key the ledger sweep on generation_id again -- the run reaches no
// generation, so the collection pass deletes it and its spans.
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
	// Nothing published, so nothing is delivered: the reason is in the ledger
	// or it is nowhere.
	if published := c.late.take(); len(published) != 0 {
		t.Fatalf("a batch whose every unit failed delivered %d publication(s)", len(published))
	}
	// The collection pass is what would sweep a run that reached no generation,
	// so it runs before the read rather than after it.
	c.collect(ctx)
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
		t.Fatalf("no span of the failed batch names the scope %q with its typed reason; the %d "+
			"recorded spans are the only durable account of why the batch published nothing",
			units[0].unit.ScopeKey, len(view.Spans))
	}
}
