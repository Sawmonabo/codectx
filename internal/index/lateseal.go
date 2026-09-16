package index

import (
	"context"
	"errors"
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

// Late-sealing dependence units (Section 11.6, rulings Q1 and Q9).
//
// Under `providers.dependence.enabled = "auto"` a dependence unit never blocks
// base readiness: the plan marks it deferred, the base generation carries its
// previous sealed unit as stale, and this sealer runs it afterwards as
// low-priority background work -- regardless of whether any query ever asks
// (Q9), because `auto` that only runs on demand is `false` for every
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

// lateSealer owns the background queue. One goroutine drains it, so heavy
// analyzers are additionally serialized by the scheduler's admission gate.
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
	// running is the unit the tick is building right now, or nil. It is part
	// of what is pending: a queue that reports only what has not started yet
	// answers "nothing is pending" for the whole minutes a unit takes, and a
	// query reads that as coverage it does not have.
	running *deferredUnit
	// snap, sel and ref describe the generation the queued units were planned
	// over. A later base activation replaces all three together with its own
	// queue: work planned over a superseded snapshot is not worth running.
	snap model.SnapshotID
	sel  provider.Selection
	ref  string
	// foreground is the failure aggregate of the generation these units were
	// planned over. It is replaced with the queue, because failures of a
	// superseded generation say nothing about the one a later batch extends.
	foreground map[string]*providerFailures
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
		ctx: ctx, cancel: cancel, done: make(chan struct{})}
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
	l.snap, l.sel, l.ref = g.snap.ID, g.sel, g.ref()
	// The foreground generation's failures travel with the queue. The
	// publication below re-plans the same scopes and still holds nothing for
	// the ones that failed, so a report built without them calls the provider
	// fresh over a hole the foreground already found.
	l.foreground = g.failures
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
	started, pending := l.started, len(l.queue)
	if l.running != nil {
		pending++
	}
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
			n, snap, sel, ref := l.pending()
			if n == 0 {
				break
			}
			if err := l.tick(l.ctx, snap, sel, ref); err != nil {
				if l.ctx.Err() != nil {
					return
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

// next pops the head of the queue and records it as the unit in flight. It
// answers false when the queue is empty or when a later base activation
// replaced the queue with work planned over another snapshot: units of two
// snapshots must never seal into one work generation.
func (l *lateSealer) next(snap model.SnapshotID) (deferredUnit, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.queue) == 0 || l.snap != snap {
		l.running = nil
		return deferredUnit{}, false
	}
	d := l.queue[0]
	l.queue[0] = deferredUnit{}
	l.queue = l.queue[1:]
	l.running = &d
	return d, true
}

// finished clears the unit in flight.
func (l *lateSealer) finished() {
	l.mu.Lock()
	l.running = nil
	l.mu.Unlock()
}

// pending is what the queue holds now: the unit in flight plus everything
// behind it, and the snapshot, selection and ref the tick works over.
//
// It is the question "is there a tick to run", so the publication window is
// deliberately not counted: a publication in flight needs no tick, and adding
// it here would have the background loop open and abort an empty work
// generation for every batch a Drain publishes. What closing the coordinator
// would abandon is the other question, and Coordinator.Pending answers it.
func (l *lateSealer) pending() (int, model.SnapshotID, provider.Selection, string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := len(l.queue)
	if l.running != nil {
		n++
	}
	return n, l.snap, l.sel, l.ref
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

// batch is what one tick produced: the units that sealed, and the reason, per
// plan key, for each one that did not. The failures travel with the seals
// because the publication has to tell a scope that failed from a scope still
// running -- a provider whose other scopes published is partial with the
// failed scope named, not a provider nobody has heard from.
type batch struct {
	sealed []sealedUnit
	failed map[string]unitFailure
}

// tick takes the tick slot and runs one batch, abandoning the attempt if ctx
// is cancelled before the slot is free.
func (l *lateSealer) tick(ctx context.Context, snap model.SnapshotID,
	sel provider.Selection, ref string) error {
	if err := l.acquire(ctx); err != nil {
		return err
	}
	defer l.release()
	return l.tickHeld(ctx, snap, sel, ref)
}

// tickHeld runs one batch in a work generation and publishes what sealed. The
// caller holds the tick slot. The publication is recorded for a Drain to
// deliver, never announced from here: this may be the background loop's
// goroutine.
func (l *lateSealer) tickHeld(ctx context.Context, snap model.SnapshotID,
	sel provider.Selection, ref string) (err error) {
	c := l.c
	// A deferred publication opens generations of its own, so it is a run in
	// its own right and not spans of the index run that queued it -- which had
	// ended long before this tick started. Its spans are keyed by the run id
	// and outlive the work generation, which is aborted on every path: the
	// reason a deferred unit failed is therefore still there after the tick,
	// where a row written against the aborted generation would not be.
	run := c.newRun(ledger.KindDeferred)
	ctx = run.Context(ctx)
	defer func() { run.Finish(endOutcome(err)) }()
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
	workGen, err := c.opts.Store.BeginGeneration(ctx, c.repo, snap, c.cfgHash, ref)
	if err != nil {
		return err
	}
	// The work generation is aborted on every path, and only after the
	// publication has attached what it holds (ruling Q1).
	abort := func() {
		if err := c.opts.Store.Abort(context.WithoutCancel(ctx), workGen); err != nil {
			logTyped(c.log, "the deferred work generation could not be aborted", err,
				"component", component, "generation_id", int64(workGen))
		}
	}
	work := &generation{c: c, view: view, gen: workGen, prev: active, caps: c.newCapabilityReport(),
		snap: model.Snapshot{ID: snap, RepositoryID: c.repo},
		plan: plan.Plan{Previous: map[string]model.UnitID{}}}
	b := batch{failed: map[string]unitFailure{}}
	// One unit is popped at a time, so Promote can still see everything that
	// has not sealed yet; every unit that seals before the queue empties
	// publishes through the one generation below, which is the coalescing
	// Section 11.6 requires.
	for {
		d, ok := l.next(snap)
		if !ok {
			break
		}
		if ctx.Err() != nil {
			l.finished()
			abort()
			return model.Canceled(ctx.Err())
		}
		work.plan.Previous[plan.Key(d.unit.ProviderID, d.unit.ScopeKey)] = d.previous
		unit, out, err := l.runOne(ctx, work, d)
		l.finished()
		if err != nil {
			// One background unit's failure does not stop the others, and it
			// does not touch what is published: the scope keeps answering
			// stale from its carried predecessor. The reason is kept on the
			// run row and logged by the same path the foreground uses, so a
			// deferred failure is as diagnosable as a foreground one.
			b.failed[plan.Key(d.unit.ProviderID, d.unit.ScopeKey)] = work.recordFailure(ctx, d.unit, out, err)
			continue
		}
		b.sealed = append(b.sealed, sealedUnit{providerID: d.unit.ProviderID, scopeKey: d.unit.ScopeKey, unit: unit})
	}
	if len(b.sealed) == 0 {
		abort()
		return nil
	}
	l.setPublishing(true)
	res, published, err := l.publish(ctx, snap, sel, ref, b)
	l.setPublishing(false)
	abort()
	if err != nil {
		// The sealed units are members of nothing but the work generation this
		// tick just aborted, so the next retention pass collects them and the
		// engine work is lost. Saying how much is lost is the difference
		// between a diagnosable failure and minutes of analysis vanishing.
		c.log.Warn("a deferred publication failed and its sealed units are abandoned",
			"component", component, "repository_id", string(c.repo), "units", len(b.sealed))
		return err
	}
	if published {
		l.record(res)
	}
	return nil
}

// setPublishing marks the publication window, which is pending work that is
// neither queued nor running.
func (l *lateSealer) setPublishing(on bool) {
	l.mu.Lock()
	l.publishing = on
	l.mu.Unlock()
}

// runOne builds one deferred unit in the work generation, under the heavy
// analyzer admission gate.
// The provider result is answered on both paths: a failure's result carries
// the run the provider actually opened, which is where the reason is kept.
func (l *lateSealer) runOne(ctx context.Context, work *generation, d deferredUnit) (model.UnitID, outcome, error) {
	started := l.c.now()
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
	if d.unit.Heavy {
		release, err := l.c.sched.Admit(ctx, d.unit.Reservation)
		if err != nil {
			return "", outcome{}, err
		}
		defer release()
	}
	out, err := work.run(ctx, d.unit, spec)
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
// does not derive. The named alternative is a storage listing of the active
// generation's members (a keyset-paged GenerationUnits), which would save a
// manifest walk per publication but would not give that check for free; it is
// recorded as a storage follow-up, not taken here.
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
		CarriedPage: c.carriedPage(active), Config: c.opts.Config, TempDir: c.workDir}
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
	if len(replacing) == 0 {
		return model.IndexResult{}, false, nil
	}

	pubGen, err := c.opts.Store.BeginGeneration(ctx, c.repo, snap, c.cfgHash, ref)
	if err != nil {
		return model.IndexResult{}, false, err
	}
	// The run is linked to the generation it publishes INTO, never to the work
	// generation, which is aborted on every path: a run linked to a generation
	// that no longer exists would be swept with it, taking the record of what
	// the tick did.
	ledger.RunFromContext(ctx).AttachGeneration(int64(pubGen))
	g := &generation{c: c, view: view, sel: sel, plan: p, gen: pubGen, prev: active,
		snap: captured, started: started, caps: c.newCapabilityReport(),
		failedScopes: b.failed, failures: l.foregroundFailures()}
	for _, s := range p.States {
		g.caps.add(s)
	}
	var binding model.Binding
	var states []model.CapabilityState
	var health model.GenerationHealth
	err = l.attach(ctx, g, replacing)
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
		Runs: runs, RunsOmitted: omitted, StartedAt: started, CompletedAt: c.now()}, true, nil
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
	g.sealed = make(map[string]bool, len(replacing))
	for key, unit := range replacing {
		if err := g.c.opts.Store.AttachUnit(ctx, g.gen, unit); err != nil {
			return err
		}
		g.sealed[key] = true
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
	units := len(l.queue)
	if l.running != nil {
		units++
	}
	if l.publishing {
		units++
	}
	if units == 0 {
		return Pending{}
	}
	return l.pendingAt(units, 0)
}

// Drain runs the deferred queue to empty in the caller's own goroutine,
// publishing each batch through the ruling-Q1 publication generation and
// calling progress after each publication. Nil progress is allowed.
//
// It is how a one-shot command honours ruling Q9 -- under the shipped
// `providers.dependence.enabled = "auto"` the deferred units must run whether
// or not a query ever asks, and a process that closed the coordinator the
// moment its base generation activated would run none of them.
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
	if err := c.writable(); err != nil {
		return err
	}
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
		units, snap, sel, ref := l.pending()
		if units == 0 {
			l.release()
			deliver()
			return nil
		}
		err := l.tickHeld(ctx, snap, sel, ref)
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
	if err := c.writable(); err != nil {
		return Pending{}, err
	}
	if err := ctx.Err(); err != nil {
		return Pending{}, model.Canceled(err)
	}
	if providerID == "" || scopeKey == "" {
		return Pending{}, invalid("promotion needs a provider id and a scope key")
	}
	l := c.late
	l.mu.Lock()
	defer l.mu.Unlock()
	pending := len(l.queue)
	if l.running != nil {
		pending++
		// The unit is already building: it cannot be promoted any further, and
		// reporting it as absent would let the caller read "nothing pending" for
		// the whole time it runs.
		if l.running.unit.ProviderID == providerID && l.running.unit.ScopeKey == scopeKey {
			return l.pendingAt(pending, 1), nil
		}
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
	// A unit in flight holds position 1, so a promoted unit is next after it.
	position := 1
	if l.running != nil {
		position = 2
	}
	return l.pendingAt(pending, position), nil
}

// ref is the ref of the generation this pass built, which the deferred work
// publishes under as well: background units belong to the ref their base
// generation came from, not to whatever HEAD points at when they finish.
func (g *generation) ref() string { return g.builtRef }
