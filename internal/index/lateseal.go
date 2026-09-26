package index

import (
	"context"
	"errors"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/Sawmonabo/codectx/internal/index/plan"
	"github.com/Sawmonabo/codectx/internal/ledger"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider"
	"github.com/Sawmonabo/codectx/internal/snapshot"
	"github.com/Sawmonabo/codectx/internal/storage/sqlite"
)

// Late-sealing dependence units (Section 11.6).
//
// Under `providers.dependence.enabled = "auto"` a dependence unit never blocks
// base readiness: the plan marks it deferred, the base generation carries its
// previous sealed unit as stale, and this sealer runs it afterwards as
// low-priority background work -- regardless of whether any query ever asks,
// because `auto` that only runs on demand is `false` for every
// non-interactive user.
//
// A sealed unit is published, never merged into the active generation. Storage
// attaches a unit to the generation it was begun for (SealUnit calls attach),
// so each tick runs its units in a short-lived WORK generation and then opens a
// PUBLICATION generation holding every member of the active generation plus the
// newly sealed units, validates it and activates it with the same
// compare-and-swap a refresh uses. The work generation is aborted only after
// that attach, because collectUnreachableUnits deletes any non-building unit no
// generation selects and would otherwise collect the unit between the two
// steps. Every unit that seals inside one tick publishes through one
// generation, which is the coalescing Section 11.6 requires.

// deferred is one queued background unit and the predecessor it imports its
// delta from.
type deferredUnit struct {
	unit     plan.Unit
	previous model.UnitID
}

// queueState is the generation one queue of deferred units was planned over,
// and the epoch that queue was published under.
//
// The epoch is what a tick runs its units by: it is bumped by every enqueue,
// so a tick that popped its first unit from one queue never takes a unit a
// later base activation put there. That is what keeps the units of two
// snapshots out of one work generation, and what lets the tick write every
// unit's predecessor into the work plan before its first worker starts rather
// than beside them.
type queueState struct {
	snap  model.SnapshotID
	sel   provider.Selection
	ref   string
	epoch int64
}

// lateSealer owns the background queue. One goroutine drains it, and the units
// of one tick overlap exactly as far as the machine's one allocation allows:
// the tick offers the next unit as soon as the previous one is past
// plan.Scheduler.Admit, so what runs at once is decided by the summed
// reservations of ADR-0010 decision 5 and by nothing else -- no count of its
// own, and no wait for a unit to finish.
type lateSealer struct {
	c *Coordinator

	// tick1 admits one tick at a time. The background loop and a caller's
	// Drain both run them, and two ticks at once would each open a work
	// generation and each publish, so one would lose the activation
	// compare-and-swap and throw its batch away.
	//
	// It is a one-slot channel rather than a mutex because the admission has
	// to be abandonable: a Drain whose context is cancelled while the
	// background loop owns a batch must return then, not when the batch ends,
	// and sync.Mutex.Lock has no way to hear the cancellation. A batch is
	// minutes of engine work, so the difference is an interruptible command
	// and an uninterruptible one.
	tick1 chan struct{}

	mu    sync.Mutex
	queue []deferredUnit
	// inflight holds the plan key of every unit a tick has popped and not yet
	// settled: building, or built and waiting for the rest of its batch.
	// Several units of one tick run at once, so it is a set and not one unit:
	// a queue that reports fewer units than are actually in flight answers for
	// coverage it does not have, exactly as one that reports only what has not
	// started yet answers "nothing is pending" for the whole minutes a unit
	// takes.
	inflight map[string]bool
	// state describes the generation the queued units were planned over. A
	// later base activation replaces the whole of it together with its own
	// queue: work planned over a superseded snapshot is not worth running.
	state queueState
	// foreground is the failure aggregate of the generation these units were
	// planned over. It is replaced with the queue, because failures of a
	// superseded generation say nothing about the one a later batch extends.
	foreground map[string]*providerFailures
	// background is the typed reason, per plan key, that a DEFERRED unit of
	// THIS queue did not seal, accumulated across every tick the queue is
	// drained by rather than held for the tick that produced it.
	//
	// It has to outlive its tick. A unit that fails is popped off the queue
	// and never retried, and UnitWriter.Fail deletes its unit row, so nothing
	// durable is left for a later publication to find: the scope is re-planned
	// deferred by every later generation and, with only that tick's failures
	// in hand, published as still running for ever -- an over-claim of work
	// nothing will finish, which Section 13.3 forbids outright.
	//
	// It is bounded by the queue it describes: one entry per queued scope at
	// worst, and it is replaced with the queue for the same reason foreground
	// is.
	background map[string]unitFailure
	// estimate is the mean duration of the deferred units this process has
	// completed, and samples how many it is over. Zero samples means the
	// estimate is unknown and Pending reports no estimate at all.
	estimate time.Duration
	samples  int64
	// published holds the results of the publications no Drain has delivered
	// yet. A tick records its publication here and never calls the caller's
	// callback itself: the tick may be the background loop's goroutine, and
	// Drain's contract is that progress is invoked from the draining
	// goroutine alone and never after Drain has returned -- which is what
	// lets the caller read what the callback wrote without a mutex.
	//
	// It is appended to whether or not a Drain is registered, so a batch that
	// publishes in the window between an activation and the Drain that
	// follows it is still reported rather than silently superseding the
	// result the caller ends up announcing. Only the most recent
	// maxBufferedPublications are kept, because a long-lived watch publishes
	// batch after batch with no drain to consume them and an unbounded
	// recollection of results nobody reads is a leak; the bound cannot lose a
	// publication a Drain wanted, since it delivers after every tick and the
	// pre-registration window holds at most one.
	published []model.IndexResult
	// draining is set for the life of a Drain. It is the re-entrancy guard,
	// and it is a field of its own rather than "a progress callback is
	// registered" because the nil callback Drain documents as legal would
	// otherwise be neither refused nor protected.
	draining bool
	// publishing is set while a tick is inside its publication. Between the
	// last unit finishing and the publication returning nothing is queued and
	// nothing is running, and a caller that reads that as "nothing is
	// pending" would close the coordinator on top of a completed batch and
	// cancel it away. Only the answers about what closing would abandon
	// (Coordinator.Pending, close) count it: the drain loop and the
	// background loop use it to decide whether to run a tick, and a tick is
	// not what an in-flight publication needs.
	publishing bool

	wake    chan struct{}
	ctx     context.Context
	cancel  context.CancelFunc
	done    chan struct{}
	started bool
}

// maxBufferedPublications bounds the undelivered publication results the
// sealer remembers. It is a product-owned structural bound: a drain delivers
// after every tick, so only a watch with no drain at all can reach it, and
// what such a process would do with a hundred stale results is nothing.
const maxBufferedPublications = 32

func newLateSealer(c *Coordinator) *lateSealer {
	ctx, cancel := context.WithCancel(context.Background())
	return &lateSealer{c: c, tick1: make(chan struct{}, 1), wake: make(chan struct{}, 1),
		inflight: map[string]bool{}, ctx: ctx, cancel: cancel, done: make(chan struct{})}
}

// acquire takes the tick slot, giving up when ctx is cancelled. release
// returns it; every acquire that returned nil must be released.
func (l *lateSealer) acquire(ctx context.Context) error {
	// The context is examined first, so a cancelled caller never wins a free
	// slot on the select's coin toss and starts a batch it is about to abandon.
	if err := ctx.Err(); err != nil {
		return model.Canceled(err)
	}
	select {
	case l.tick1 <- struct{}{}:
		return nil
	case <-ctx.Done():
		return model.Canceled(ctx.Err())
	}
}

func (l *lateSealer) release() { <-l.tick1 }

// record remembers one publication for the drain goroutine to deliver.
func (l *lateSealer) record(res model.IndexResult) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.published = append(l.published, res)
	if n := len(l.published); n > maxBufferedPublications {
		l.published = append(l.published[:0], l.published[n-maxBufferedPublications:]...)
	}
}

// take removes everything recorded so far, for the drain goroutine to deliver.
func (l *lateSealer) take() []model.IndexResult {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := l.published
	l.published = nil
	return out
}

// enqueue replaces the background queue with this generation's deferred units.
// It is called after activation only.
func (l *lateSealer) enqueue(g *generation, units []plan.Unit) {
	l.mu.Lock()
	l.queue = l.queue[:0]
	for _, u := range units {
		l.queue = append(l.queue, deferredUnit{unit: u, previous: g.plan.Previous[plan.Key(u.ProviderID, u.ScopeKey)]})
	}
	// The epoch is bumped by every enqueue, including the one that queues
	// nothing: a tick in flight must stop taking units from a queue this call
	// has replaced whether the replacement holds work or not.
	l.state = queueState{snap: g.snap.ID, sel: g.sel, ref: g.ref(), epoch: l.state.epoch + 1}
	// The foreground generation's failures travel with the queue. The
	// publication below re-plans the same scopes and still holds nothing for
	// the ones that failed, so a report built without them calls the provider
	// fresh over a hole the foreground already found.
	l.foreground = g.failures
	// The scopes this queue's own background work failed belong to the queue
	// it was drained from, so they go with it.
	l.background = map[string]unitFailure{}
	pending := len(l.queue)
	if pending > 0 && !l.started {
		l.started = true
		go l.loop()
	}
	l.mu.Unlock()
	if pending == 0 {
		return
	}
	select {
	case l.wake <- struct{}{}:
	default:
	}
}

// close stops the background worker and waits for the tick in flight. The unit
// it was running is canceled, which fails that unit and leaves the active
// generation exactly as it was.
//
// Work that is still queued is abandoned, and saying so is the point of the
// warning: under `providers.dependence.enabled = "auto"` a one-shot command
// that closes the coordinator the moment its base generation activates would
// otherwise drop every deferred unit in silence. Coordinator.Drain is how a
// caller runs them to completion first; Coordinator.Pending is how it decides.
func (l *lateSealer) close() {
	l.mu.Lock()
	started, pending := l.started, len(l.queue)+len(l.inflight)
	if l.publishing {
		// A batch that has sealed its units and is publishing them is about
		// to be cancelled by l.cancel below, and its units are then members
		// of nothing but the aborted work generation. That is exactly the
		// loss this warning exists to name, so it is counted here too.
		pending++
	}
	l.mu.Unlock()
	if pending > 0 {
		l.c.log.Warn("deferred units were abandoned when the coordinator closed", "component", component,
			"repository_id", string(l.c.repo), "units", pending)
	}
	l.cancel()
	if started {
		<-l.done
	}
}

// loop drains the queue one tick at a time.
func (l *lateSealer) loop() {
	defer close(l.done)
	for {
		select {
		case <-l.ctx.Done():
			return
		case <-l.wake:
		}
		for {
			if l.ctx.Err() != nil {
				return
			}
			n, state := l.pending()
			if n == 0 {
				break
			}
			if err := l.tick(l.ctx, state); err != nil {
				if l.ctx.Err() != nil {
					return
				}
				var typed *model.Error
				if errors.As(err, &typed) && typed.Code == model.CodeWorkspaceBusy {
					// Another process owns the workspace. The queue keeps its
					// units and stops asking until something wakes it again;
					// retrying here would be a hot loop against a lock that is
					// held for as long as that process runs.
					l.c.log.Info("deferred unit publication is waiting for the workspace another process holds",
						"component", component, "repository_id", string(l.c.repo), "units", n)
					break
				}
				// Background work that failed leaves the active generation
				// untouched and the scope answering stale; the next index
				// plans it again. Nothing here carries source or native keys.
				logTyped(l.c.log, "deferred unit publication failed", err,
					"component", component, "repository_id", string(l.c.repo))
			}
		}
	}
}

// next pops the head of the queue and adds it to the set in flight. It answers
// false when the queue is empty or when a later base activation replaced the
// queue: the units of that queue belong to another epoch, and this tick's work
// generation was opened over the snapshot its own epoch named.
func (l *lateSealer) next(epoch int64) (deferredUnit, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.queue) == 0 || l.state.epoch != epoch {
		return deferredUnit{}, false
	}
	d := l.queue[0]
	l.queue[0] = deferredUnit{}
	l.queue = l.queue[1:]
	l.inflight[plan.Key(d.unit.ProviderID, d.unit.ScopeKey)] = true
	return d, true
}

// pending is what the queue holds now: the units in flight plus everything
// behind them, and the state the tick works over.
//
// It is the question "is there a tick to run", so the publication window is
// deliberately not counted: a publication in flight needs no tick, and adding
// it here would have the background loop open and abort an empty work
// generation for every batch a Drain publishes. What closing the coordinator
// would abandon is the other question, and Coordinator.Pending answers it.
func (l *lateSealer) pending() (int, queueState) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.queue) + len(l.inflight), l.state
}

// pendingAt builds the typed answer, attaching the estimate only once this
// process has actually completed a deferred unit to average.
func (l *lateSealer) pendingAt(units, position int) Pending {
	p := Pending{Units: units, Position: position}
	if l.samples > 0 {
		p.Estimate = l.estimate
	}
	return p
}

// sealed is one background unit that completed.
type sealedUnit struct {
	providerID, scopeKey string
	unit                 model.UnitID
}

// batch is what one tick produced: the units that sealed and how many did not.
// The reasons of the ones that did not are kept on the sealer, in
// `background`, and not here: the publication has to tell a scope that failed
// from a scope still running, and a scope that failed in an EARLIER tick is
// just as far from running as one that failed in this one.
type batch struct {
	sealed []sealedUnit
	// failed counts this tick's units that did not seal, for the run row's
	// totals and the every-unit-failed path.
	failed int
	// work is the generation this tick's units ran in. Their provider runs,
	// and so the reasons the failed ones recorded, are rows of it, and the
	// publication copies those rows onto the generation it activates before
	// the work generation is aborted.
	work model.GenerationID
}

// tick takes the tick slot and runs one batch, abandoning the attempt if ctx
// is cancelled before the slot is free.
func (l *lateSealer) tick(ctx context.Context, state queueState) error {
	// The cross-process workspace lock first, then the tick slot: this batch
	// publishes a generation, and the background loop is the one publisher
	// that does not run inside a caller's hold. The lock order is
	// retention/retention.go's -- workspace lock, then this process's indexing
	// state -- and it is given back the moment the batch is done, so a
	// deferred unit never keeps the workspace from the person's own index.
	_, release, err := l.c.hold(ctx, HoldNow)
	if err != nil {
		return err
	}
	defer release()
	if err := l.acquire(ctx); err != nil {
		return err
	}
	defer l.release()
	return l.tickHeld(ctx, state)
}

// tickHeld runs one batch in a work generation and publishes what sealed. The
// caller holds the tick slot. The publication is recorded for a Drain to
// deliver, never announced from here: this may be the background loop's
// goroutine.
func (l *lateSealer) tickHeld(ctx context.Context, state queueState) (err error) {
	c := l.c
	snap := state.snap
	// A deferred publication opens generations of its own, so it is a run in
	// its own right and not spans of the index run that queued it -- which had
	// ended long before this tick started. Its spans are keyed by the run id
	// and belong to no generation, so they outlive the work generation this
	// tick aborts on every path and the publication generation a later
	// retention sweeps: the reason a deferred unit failed is still there after
	// the tick, where a row written against the aborted generation is not.
	run := c.newRun(ledger.KindDeferred)
	ctx = run.Context(ctx)
	// The publication is remembered for a Drain to deliver only once this run
	// has ended and has read itself back: the result a Drain hands out states
	// what THIS deferred run did, never the index run's account, and a run
	// still going is not what a publication that has activated looks like.
	var res model.IndexResult
	published := false
	// What this tick popped and what became of it. They are declared here
	// because the run row's totals are reported on every exit path, including
	// the one where nothing sealed: a run row with no totals would read
	// `0 planned, 0 succeeded` for a batch that ran, a measurement nobody made.
	planned := 0
	var b batch
	defer func() {
		run.Report(ledger.Totals{UnitsPlanned: int64(planned),
			UnitsSucceeded: int64(len(b.sealed)), UnitsFailed: int64(b.failed)})
		// A batch whose every unit failed is not an ok run. It returns no
		// error -- one background unit's failure never stops the others -- and
		// its publication states failures only, so the run row's outcome is
		// what says the batch sealed nothing.
		outcome := endOutcome(err)
		if err == nil && len(b.sealed) == 0 && b.failed > 0 {
			outcome = ledger.OutcomeFailed
		}
		run.Finish(outcome)
		if err != nil || !published {
			return
		}
		c.attachRunLedger(ctx, &res, run, err)
		l.record(res)
	}()
	view, err := snapshot.OpenView(ctx, c.opts.Store, c.opts.CAS, snap)
	if err != nil {
		return err
	}
	// The pointer read here is the cost hint the work generation plans
	// against. The publication re-reads it for itself: this one is minutes
	// stale by the time the batch is ready.
	active, err := c.activeGeneration(ctx)
	if err != nil {
		return err
	}
	workGen, err := c.opts.Store.BeginGeneration(ctx, c.repo, snap, c.cfgHash, state.ref)
	if err != nil {
		return err
	}
	b.work = workGen
	// The work generation is aborted on every path, and only after the
	// publication has attached its sealed units and copied its failure rows.
	abort := func() {
		if err := c.opts.Store.Abort(context.WithoutCancel(ctx), workGen); err != nil {
			logTyped(c.log, "the deferred work generation could not be aborted", err,
				"component", component, "generation_id", int64(workGen))
		}
	}
	work := &generation{c: c, view: view, gen: workGen, prev: active, caps: c.newCapabilityReport(),
		snap: model.Snapshot{ID: snap, RepositoryID: c.repo},
		plan: plan.Plan{Previous: map[string]model.UnitID{}}}
	// The predecessor of every unit this tick may run is written into the work
	// plan here, before the first worker starts: generation.run reads that map
	// from each worker's own goroutine, and writing it beside those reads would
	// be two goroutines on one map.
	l.carryPrevious(work, state.epoch)
	// bmu guards what the workers accumulate. The batch is this tick's own
	// record of what sealed and what did not, which is neither of the
	// accumulators generation.mu guards.
	var (
		bmu sync.Mutex
		wg  sync.WaitGroup
		// popped is every unit this tick took off the queue, in the order it
		// took them. They stay in the set in flight until settle hands them
		// back or the batch is published, so no reader of Pending sees a unit
		// that is neither queued, running nor being published.
		popped []*poppedUnit
	)
	// One unit is popped at a time, so Promote can still see everything that
	// has not started yet; every unit that seals before the queue empties
	// publishes through the one generation below, which is the coalescing
	// Section 11.6 requires.
	for ctx.Err() == nil {
		d, ok := l.next(state.epoch)
		if !ok {
			break
		}
		pu := &poppedUnit{d: d}
		popped = append(popped, pu)
		if ctx.Err() != nil {
			break
		}
		planned++
		ready := make(chan struct{})
		admitted := sync.OnceFunc(func() { close(ready) })
		wg.Add(1)
		go func() {
			defer wg.Done()
			unit, out, runErr := l.runOne(ctx, work, d, admitted)
			if runErr != nil {
				if ctx.Err() != nil && errors.Is(runErr, ctx.Err()) {
					// The tick was cancelled, so this unit has no failure of
					// its own: recording one would write a run-failure row and
					// warn the operator about work only they stopped. It goes
					// back on the queue with the batch below.
					return
				}
				// One background unit's failure does not stop the others, and
				// it does not touch what is published: the scope keeps
				// answering stale from its carried predecessor. The reason is
				// kept on the run row and logged by the same path the
				// foreground uses, so a deferred failure is as diagnosable as
				// a foreground one. The row is written before the lock is
				// taken: the mutex guards this tick's batch and nothing else.
				failure := work.recordFailure(ctx, d.unit, out, runErr)
				l.recordBackgroundFailure(state.epoch, plan.Key(d.unit.ProviderID, d.unit.ScopeKey), failure)
				bmu.Lock()
				defer bmu.Unlock()
				pu.failed = true
				b.failed++
				return
			}
			bmu.Lock()
			defer bmu.Unlock()
			b.sealed = append(b.sealed, sealedUnit{providerID: d.unit.ProviderID, scopeKey: d.unit.ScopeKey, unit: unit})
		}()
		// The next unit is offered as soon as this one is past the admission
		// gate, and the tick waits for nothing else: what runs at once is the
		// summed reservations of ADR-0010 decision 5 and never a count of this
		// loop's own. An empty queue therefore ends the popping and not the
		// tick -- the publication below is after every worker has finished.
		<-ready
	}
	wg.Wait()
	if ctx.Err() != nil {
		// A cancelled tick settled nothing but its failures: every other unit
		// it popped is still outstanding, and dropping it would have Pending,
		// and the warning a closing coordinator gives, count none of it.
		l.settle(state.epoch, popped, true, false)
		abort()
		return model.Canceled(ctx.Err())
	}
	if len(b.sealed) == 0 && b.failed == 0 {
		l.settle(state.epoch, popped, false, false)
		abort()
		return nil
	}
	// A batch whose every unit failed publishes too. Its scopes are no longer
	// running, and the active generation's capability rows say they are: left
	// in place, every later status would report work in flight that nothing
	// will finish. The publication re-derives those rows with the failures
	// folded in and carries their reasons, while every scope keeps answering
	// from its carried predecessor.
	l.settle(state.epoch, popped, false, true)
	res, published, err = l.publish(ctx, snap, state.sel, state.ref, b)
	// A publication the cancellation stopped activated nothing, so its sealed
	// units go back on the queue exactly as a cancelled build's do; the next
	// tick publishes each one it still finds sealed and rebuilds any that
	// retention collected with the aborted work generation.
	l.settle(state.epoch, popped, err != nil && ctx.Err() != nil, false)
	abort()
	if err == nil && len(b.sealed) == 0 {
		c.log.Warn("every deferred unit of this batch failed",
			"component", component, "repository_id", string(c.repo),
			"units", b.failed, "published", published, "run_id", run.ID())
	}
	if err != nil {
		// The sealed units are members of nothing but the work generation this
		// tick just aborted, so the next retention pass collects them and the
		// engine work is lost. Saying how much is lost is the difference
		// between a diagnosable failure and minutes of analysis vanishing.
		c.log.Warn("a deferred publication failed and its sealed units are abandoned",
			"component", component, "repository_id", string(c.repo), "units", len(b.sealed))
		return err
	}
	return nil
}

// carryPrevious writes the predecessor every queued unit imports its delta
// from into the work generation's plan. It is called once, before the tick
// starts its first worker, and covers exactly the queue of this tick's epoch:
// a queue a later activation replaced is another tick's, and next stops at it.
func (l *lateSealer) carryPrevious(work *generation, epoch int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.state.epoch != epoch {
		return
	}
	for _, d := range l.queue {
		work.plan.Previous[plan.Key(d.unit.ProviderID, d.unit.ScopeKey)] = d.previous
	}
}

// poppedUnit is one unit a tick took off the queue, and whether it settled
// with a failure of its own. The flag is written under the tick's batch mutex
// and read once every worker has returned.
type poppedUnit struct {
	d      deferredUnit
	failed bool
}

// settle takes the tick's popped units out of the set in flight and sets the
// publication window in one step, so no reader of Pending sees a unit in
// neither place. With requeue, every popped unit that did not fail goes back
// to the head of the queue in the order it was popped -- unless a later base
// activation replaced the queue, whose epoch this work no longer belongs to.
func (l *lateSealer) settle(epoch int64, popped []*poppedUnit, requeue, publishing bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	back := make([]deferredUnit, 0, len(popped))
	for _, pu := range popped {
		delete(l.inflight, plan.Key(pu.d.unit.ProviderID, pu.d.unit.ScopeKey))
		if requeue && !pu.failed {
			back = append(back, pu.d)
		}
	}
	if len(back) > 0 && l.state.epoch == epoch {
		l.queue = append(back, l.queue...)
	}
	l.publishing = publishing
}

// runOne builds one deferred unit in the work generation, under the heavy
// analyzer admission gate.
// The provider result is answered on both paths: a failure's result carries
// the run the provider actually opened, which is where the reason is kept.
//
// admitted is called once this unit is past the admission gate, and on every
// path that never reaches it: it is how the tick knows it may offer the next
// unit, so a unit that returns before the gate must not hold the queue behind
// it.
func (l *lateSealer) runOne(ctx context.Context, work *generation, d deferredUnit,
	admitted func()) (id model.UnitID, res outcome, err error) {

	defer admitted()
	// The row exists before the unit is admitted, and the deferred close below
	// gives it a terminal state on every path this call can take; run closes
	// it first for a unit that reached its provider.
	span := ledger.Plan(ctx, d.unit.ProviderID, d.unit.ScopeKey, d.unit.ProviderID)
	defer func() { span.End(unitEnding(err), ledger.Measured{CPUUnattributed: ledger.CPUOverlapped}, err) }()
	spec, err := d.unit.Spec(l.c.cfgHash)
	if err != nil {
		return "", outcome{}, err
	}
	state, exists, err := l.c.opts.Store.UnitState(ctx, spec.ID)
	if err != nil {
		return "", outcome{}, err
	}
	if exists && state == model.UnitSealed {
		// An earlier tick sealed it and could not publish; the unit is
		// immutable, so it is published now rather than rebuilt.
		return spec.ID, outcome{}, nil
	}
	var grant *unitGrant
	if d.unit.Heavy {
		var admitErr error
		if grant, admitErr = admitUnit(ctx, l.c.sched, d.unit.Reservation); admitErr != nil {
			// The gate refused or was cancelled, so the unit never reached its
			// work: unavailable with that reason, not a failure of a provider
			// that was never asked. Ending the span here wins over the deferred
			// close, which takes the first terminal state a span is given.
			span.End(ledger.OutcomeUnavailable, notAdmitted(provider.CodeOf(admitErr)), nil)
			return "", outcome{}, admitErr
		}
		defer grant.release()
	}
	// Past the gate: this reservation is part of the sum the next unit is
	// admitted against, so the tick may offer that one now. The duration the
	// estimate averages starts here too: what Pending estimates is a unit's
	// own build, and the wait for admission is the queue's, not the unit's.
	admitted()
	started := l.c.now()
	out, err := work.run(ctx, d.unit, spec, span, grant)
	if err != nil {
		return "", out, err
	}
	l.observe(l.c.now().Sub(started))
	return spec.ID, out, nil
}

// observe folds one completed unit's duration into the running mean Pending
// reports its estimate from.
func (l *lateSealer) observe(d time.Duration) {
	if d <= 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.samples++
	l.estimate += (d - l.estimate) / time.Duration(l.samples)
}

// publish opens the publication generation: every member of the active
// generation that the same snapshot still reproduces, plus the newly sealed
// units in place of the stale predecessors they replace. It answers the result
// it published, and false when there was nothing to publish.
//
// The membership is re-derived by planning over the same snapshot rather than
// copied: the plan answers, per scope, with the unit whose identity that
// snapshot justifies, which for every unchanged member is the member itself,
// and the spec.ID check below then refuses any sealed unit the current plan
// does not derive. A storage listing of the active generation's members would
// save a manifest walk per publication but would not give that check for free.
//
// A publication whose active generation has moved to another snapshot is
// dropped: publishing over it would replace a newer generation with an older
// one's source. The sealed units are then members of nothing but the work
// generation the tick aborts, so retention collects them and the next index
// rebuilds them -- which is why the drop is logged with its unit count rather
// than noted as a reuse.
//
// A lost activation compare-and-swap is retried exactly once, from a fresh
// pointer read and a fresh plan. One retry and no more, for the same reason
// the indexing path stops there: a caller that loses twice is contending with
// a writer that is winning.
func (l *lateSealer) publish(ctx context.Context, snap model.SnapshotID, sel provider.Selection,
	ref string, b batch) (model.IndexResult, bool, error) {
	for attempt := 0; ; attempt++ {
		res, published, err := l.publishOnce(ctx, snap, sel, ref, b)
		if err == nil {
			return res, published, nil
		}
		var typed *model.Error
		if attempt == 0 && errors.As(err, &typed) && typed.Code == model.CodeVersionConflict {
			l.c.log.Info("another activation intervened; re-deriving the deferred publication",
				"component", component, "repository_id", string(l.c.repo))
			continue
		}
		return model.IndexResult{}, false, err
	}
}

// publishOnce is one attempt. The active generation is read here and nowhere
// earlier: the tick that produced these units may have been running for
// minutes, and an ordinary reconciliation in between both supersedes the
// generation the tick saw and has retention delete it, so pinning that id
// would fail and discard the whole batch.
func (l *lateSealer) publishOnce(ctx context.Context, snap model.SnapshotID, sel provider.Selection,
	ref string, b batch) (model.IndexResult, bool, error) {
	c := l.c
	started := c.now()
	active, err := c.activeGeneration(ctx)
	if err != nil {
		return model.IndexResult{}, false, err
	}
	if active == 0 {
		return model.IndexResult{}, false, invalid("a deferred publication needs an active generation to extend")
	}
	pinned, err := c.opts.Store.PinGeneration(ctx, c.repo, active, statusLeaseTTL)
	if err != nil {
		return model.IndexResult{}, false, err
	}
	current := pinned.Binding().SnapshotID
	pinned.Close()
	if current != snap {
		c.log.Warn("deferred units were sealed over a superseded snapshot; they are not published and the next index rebuilds them",
			"component", component, "repository_id", string(c.repo), "units", len(b.sealed))
		return model.IndexResult{}, false, nil
	}
	view, err := snapshot.OpenView(ctx, c.opts.Store, c.opts.CAS, snap)
	if err != nil {
		return model.IndexResult{}, false, err
	}
	// The published result reports the capture it is about, so the snapshot
	// row is read rather than synthesized: a zero file count would read as a
	// publication over an empty workspace.
	captured, err := c.opts.Store.Snapshot(ctx, snap)
	if err != nil {
		return model.IndexResult{}, false, err
	}
	in := plan.Inputs{View: view, Selection: sel, Store: c.opts.Store, PrevGen: active,
		CarriedPage: c.carriedPage(active), Config: c.opts.Config, TempDir: c.workDir, Machine: c.opts.Machine}
	p, err := plan.Build(ctx, in)
	if err != nil {
		return model.IndexResult{}, false, err
	}
	defer func() { _ = p.Close() }()
	replacing := make(map[string]model.UnitID, len(b.sealed))
	for _, s := range b.sealed {
		replacing[plan.Key(s.providerID, s.scopeKey)] = s.unit
	}
	// Every sealed unit must be the unit this plan derives for its scope. If
	// it is not, the snapshot or the configuration moved under the background
	// work and the unit is not this generation's answer; nothing is published.
	if err := p.Units(func(u plan.Unit) error {
		key := plan.Key(u.ProviderID, u.ScopeKey)
		want, ok := replacing[key]
		if !ok {
			return nil
		}
		spec, err := u.Spec(c.cfgHash)
		if err != nil {
			return err
		}
		if spec.ID != want {
			c.log.Info("a deferred unit no longer matches the plan for its scope; it is not published",
				"component", component, "provider_id", u.ProviderID, "scope_key", u.ScopeKey)
			delete(replacing, key)
		}
		return nil
	}); err != nil {
		return model.IndexResult{}, false, err
	}
	// Failures alone are worth a publication: they turn scopes the active
	// generation reports running into the failures they are.
	if len(replacing) == 0 && b.failed == 0 {
		return model.IndexResult{}, false, nil
	}

	pubGen, err := c.opts.Store.BeginGeneration(ctx, c.repo, snap, c.cfgHash, ref)
	if err != nil {
		return model.IndexResult{}, false, err
	}
	// The run is linked to the generation it publishes INTO, never to the work
	// generation, which is aborted on every path: the column states which
	// generation this run produced, and the work generation is one no reader
	// will ever find. A tick that publishes nothing leaves it null, which is
	// the truthful answer and costs the run nothing -- the ledger's own bound
	// keeps a run's account whether or not it reached a generation.
	ledger.RunFromContext(ctx).AttachGeneration(int64(pubGen))
	g := &generation{c: c, view: view, sel: sel, plan: p, gen: pubGen, prev: active,
		snap: captured, started: started, caps: c.newCapabilityReport(),
		failedScopes: l.backgroundFailures(), failures: l.foregroundFailures()}
	for _, s := range p.States {
		g.caps.add(s)
	}
	var binding model.Binding
	var states []model.CapabilityState
	var health model.GenerationHealth
	// The reasons this generation's capability rows state are copied onto it:
	// the active generation's -- the foreground pass's and every earlier
	// batch's, each carried forward by the publication before -- and this
	// batch's own, from the work generation that is aborted next. A scope this
	// generation seals takes no reason with it: its fresh unit is the answer.
	sealed := make([]sqlite.RunScope, 0, len(replacing))
	for _, s := range b.sealed {
		if _, ok := replacing[plan.Key(s.providerID, s.scopeKey)]; ok {
			sealed = append(sealed, sqlite.RunScope{ProviderID: s.providerID, ScopeKey: s.scopeKey})
		}
	}
	err = c.opts.Store.CarryRunFailures(ctx, pubGen, sealed, active, b.work)
	if err == nil {
		err = l.attach(ctx, g, replacing)
	}
	if err == nil {
		err = g.coverage(ctx)
	}
	if err == nil {
		states = g.caps.finish(c.log)
		health = healthOf(states)
		// The publication generation is a new row, so it carries none of the
		// working generation's supplied-index record: it is re-recorded here
		// or the sealed generation reports no supplied index at all.
		err = c.recordSuppliedIndexes(ctx, pubGen)
		if err == nil {
			binding, err = c.opts.Store.Activate(ctx, pubGen, active, health, states, NormalizationVersion)
		}
	}
	if err != nil {
		if abortErr := c.opts.Store.Abort(context.WithoutCancel(ctx), pubGen); abortErr != nil {
			logTyped(c.log, "the failed publication generation could not be aborted", abortErr,
				"component", component, "generation_id", int64(pubGen))
		}
		return model.IndexResult{}, false, err
	}
	c.log.Info("deferred units published", "component", component, "repository_id", string(c.repo),
		"generation_id", int64(pubGen), "units", len(replacing))
	// Retention sweeps snapshots nothing references, and a foreground capture
	// is unreferenced between the moment it is written and the moment its
	// generation begins. This runs on the sealer goroutine, so it takes the
	// same lock the indexing runs hold rather than collecting one of them.
	c.run.Lock()
	c.retain(ctx)
	c.collect(ctx)
	c.run.Unlock()
	// The run list is the same wire-sized page the foreground publish serves,
	// with the same count of what did not fit: a late-sealed generation with
	// more runs than one response carries must say so rather than serve a
	// short list as the whole of it.
	runs, omitted := g.runsPage()
	return model.IndexResult{Binding: binding, Health: health, Status: model.GenerationActive,
		Completeness: states, UnitsReused: g.reused, UnitsBuilt: g.built, UnitsCarried: g.carried,
		UnitsInvalidated: g.invalidated, FilesParsed: g.parsed, FilesCaptured: int64(g.snap.FileCount),
		Runs: runs, RunsOmitted: omitted, ProvidersDisabled: disabledProviders(c.opts.Config),
		StartedAt: started, CompletedAt: c.now()}, true, nil
}

// recordBackgroundFailure keeps one deferred unit's typed reason for as long
// as the queue it came from does. The unit is popped and never retried and its
// unit row is deleted with it, so this is the only record left that the scope
// is a failure and not work still in flight.
//
// A reason from a queue a later base activation replaced is dropped, as next
// drops that queue's units: the replacing queue may hold the same scope again,
// and a stale failure recorded against it would publish that scope failed
// before its new unit has even run.
func (l *lateSealer) recordBackgroundFailure(epoch int64, key string, f unitFailure) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.state.epoch != epoch {
		return
	}
	if l.background == nil {
		l.background = map[string]unitFailure{}
	}
	l.background[key] = f
}

// backgroundFailures is a copy of every reason this queue's ticks have
// recorded, so the publication generation can fold them in without sharing
// state with the sealer. It is nil before the first failure, which is the same
// empty answer the indexing path gives.
func (l *lateSealer) backgroundFailures() map[string]unitFailure {
	l.mu.Lock()
	defer l.mu.Unlock()
	return maps.Clone(l.background)
}

// foregroundFailures is a copy of the failure aggregate the queued units were
// planned over, so the publication generation can fold it in without sharing
// state with the sealer.
func (l *lateSealer) foregroundFailures() map[string]*providerFailures {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make(map[string]*providerFailures, len(l.foreground))
	for id, agg := range l.foreground {
		clone := *agg
		clone.named = slices.Clone(agg.named)
		out[id] = &clone
	}
	return out
}

// attach fills the publication generation: the reused members, the sealed
// units in place of the scopes they complete, and the stale predecessors of
// every scope still refreshing.
func (l *lateSealer) attach(ctx context.Context, g *generation, replacing map[string]model.UnitID) error {
	if err := g.attachReused(ctx); err != nil {
		return err
	}
	// The attached rows are themselves what the coverage pass reads back to
	// decide which deferred scopes this generation still has outstanding, so
	// nothing mirrors them in memory: they must be attached before coverage
	// runs, which is the order publishOnce calls the two in.
	for _, unit := range replacing {
		if err := g.c.opts.Store.AttachUnit(ctx, g.gen, unit); err != nil {
			return err
		}
		g.built++
	}
	for _, carried := range g.plan.Carry {
		if _, replaced := replacing[plan.Key(carried.ProviderID, carried.ScopeKey)]; replaced {
			// The scope's fresh unit is a member of this generation, so its
			// predecessor is not: generation_units holds one row per scope,
			// and the stale answer is exactly what this publication ends.
			continue
		}
		err := g.c.opts.Store.AttachCarried(ctx, g.gen, carried.Unit,
			sqlite.Carry{DistanceGenerations: carried.DistanceGenerations, DistanceFiles: carried.DistanceFiles})
		if err != nil {
			return err
		}
		g.carried++
		for _, capability := range g.capabilitiesOf(carried.ProviderID) {
			g.caps.addCarried(carried.ProviderID, capability, carried.ScopeKey,
				carried.DistanceGenerations, carried.DistanceFiles)
		}
	}
	return nil
}

// Pending reports the deferred work outstanding right now (Section 11.6):
// what a caller that is about to close the coordinator would abandon, and what
// Drain would run. Position is zero because this is an answer about the whole
// queue rather than about one promoted scope.
//
// A batch that has finished building and is publishing counts as one unit:
// closing the coordinator cancels that publication and its units are collected
// with the work generation, so a caller that read zero here would report a
// clean one-shot run over work it had just thrown away. It is counted as one
// rather than as the batch's size because the batch is published as a whole,
// and what the operator is told to do about it -- run `codectx watch` -- is
// the same either way.
func (c *Coordinator) Pending() Pending {
	l := c.late
	l.mu.Lock()
	defer l.mu.Unlock()
	units := len(l.queue) + len(l.inflight)
	if l.publishing {
		units++
	}
	if units == 0 {
		return Pending{}
	}
	return l.pendingAt(units, 0)
}

// Drain runs the deferred queue to empty in the caller's own goroutine,
// publishing each batch through its publication generation and calling
// progress after each publication. Nil progress is allowed.
//
// It is how a one-shot command runs the deferred units at all: under the
// shipped `providers.dependence.enabled = "auto"` they must run whether or not
// a query ever asks, and a process that closed the coordinator the moment its
// base generation activated would run none of them.
//
// Cancelling returns at once, including while the background loop owns the
// batch: the wait for the tick slot is abandoned rather than served, because
// the batch it is waiting for is minutes of engine work and an operator's
// interrupt that is answered in minutes is an interrupt that was ignored. The
// base generation and every batch already published stay exactly as they are,
// and what is still outstanding is reported by Pending.
//
// progress is called from this goroutine only, and never after Drain has
// returned: the caller may therefore write from it and read what it wrote once
// Drain is done, with no lock. Every publication recorded since the last
// delivery is handed over after each tick and once more before Drain returns
// on any path, so a batch the background loop published outside a tick of this
// Drain's is reported too.
//
// The queue is tested for emptiness with the tick slot held, which is the only
// state in which "nothing is queued and nothing is running" is true of the
// whole sealer: outside it the answer can be the gap between a batch's last
// unit and its publication, and a caller that closed the coordinator there
// would cancel a completed batch away.
func (c *Coordinator) Drain(ctx context.Context, progress func(model.IndexResult)) error {
	_, release, err := c.hold(ctx, HoldNow)
	if err != nil {
		return err
	}
	defer release()
	l := c.late
	l.mu.Lock()
	if l.draining {
		l.mu.Unlock()
		return invalid("the deferred queue is already being drained")
	}
	l.draining = true
	l.mu.Unlock()
	defer func() {
		l.mu.Lock()
		l.draining = false
		l.mu.Unlock()
	}()
	deliver := func() {
		for _, res := range l.take() {
			if progress != nil {
				progress(res)
			}
		}
	}
	for {
		if err := l.acquire(ctx); err != nil {
			deliver()
			return err
		}
		units, state := l.pending()
		if units == 0 {
			l.release()
			deliver()
			return nil
		}
		err := l.tickHeld(ctx, state)
		l.release()
		deliver()
		if err != nil {
			return err
		}
	}
}

// Promote moves one scope's deferred unit to the head of the background queue
// and answers what the caller is waiting for (Section 11.6). A scope that is
// not queued answers zero units: it is not pending, and the active generation
// already holds whatever it has.
func (c *Coordinator) Promote(ctx context.Context, providerID, scopeKey string) (Pending, error) {
	_, release, err := c.hold(ctx, HoldNow)
	if err != nil {
		return Pending{}, err
	}
	defer release()
	if err := ctx.Err(); err != nil {
		return Pending{}, model.Canceled(err)
	}
	if providerID == "" || scopeKey == "" {
		return Pending{}, invalid("promotion needs a provider id and a scope key")
	}
	l := c.late
	l.mu.Lock()
	defer l.mu.Unlock()
	pending := len(l.queue) + len(l.inflight)
	// A unit already building cannot be promoted any further, and reporting it
	// as absent would let the caller read "nothing pending" for the whole time
	// it runs.
	if l.inflight[plan.Key(providerID, scopeKey)] {
		return l.pendingAt(pending, 1), nil
	}
	at := -1
	for i, d := range l.queue {
		if d.unit.ProviderID == providerID && d.unit.ScopeKey == scopeKey {
			at = i
			break
		}
	}
	if at < 0 {
		return Pending{}, nil
	}
	promoted := l.queue[at]
	copy(l.queue[1:at+1], l.queue[:at])
	l.queue[0] = promoted
	// The units in flight hold the positions in front, so a promoted unit is
	// next after them.
	return l.pendingAt(pending, len(l.inflight)+1), nil
}

// ref is the ref of the generation this pass built, which the deferred work
// publishes under as well: background units belong to the ref their base
// generation came from, not to whatever HEAD points at when they finish.
func (g *generation) ref() string { return g.builtRef }
