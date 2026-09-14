package index

import (
	"context"
	"sync"
	"time"

	"github.com/Sawmonabo/codectx/internal/index/plan"
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
	// estimate is the mean duration of the deferred units this process has
	// completed, and samples how many it is over. Zero samples means the
	// estimate is unknown and Pending reports no estimate at all.
	estimate time.Duration
	samples  int64

	wake    chan struct{}
	ctx     context.Context
	cancel  context.CancelFunc
	done    chan struct{}
	started bool
}

func newLateSealer(c *Coordinator) *lateSealer {
	ctx, cancel := context.WithCancel(context.Background())
	return &lateSealer{c: c, wake: make(chan struct{}, 1), ctx: ctx, cancel: cancel, done: make(chan struct{})}
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
func (l *lateSealer) close() {
	l.mu.Lock()
	started := l.started
	l.mu.Unlock()
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
				l.c.log.Warn("deferred unit publication failed", "component", component,
					"repository_id", string(l.c.repo), "diagnostic_code", provider.CodeOf(err))
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

// tick runs one batch in a work generation and publishes what sealed.
func (l *lateSealer) tick(ctx context.Context, snap model.SnapshotID,
	sel provider.Selection, ref string) error {
	c := l.c
	view, err := snapshot.OpenView(ctx, c.opts.Store, c.opts.CAS, snap)
	if err != nil {
		return err
	}
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
			c.log.Warn("the deferred work generation could not be aborted", "component", component,
				"generation_id", int64(workGen), "diagnostic_code", provider.CodeOf(err))
		}
	}
	work := &generation{c: c, view: view, gen: workGen, prev: active, caps: newCapabilityReport(),
		snap: model.Snapshot{ID: snap, RepositoryID: c.repo},
		plan: plan.Plan{Previous: map[string]model.UnitID{}}}
	var sealed []sealedUnit
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
		unit, err := l.runOne(ctx, work, d)
		l.finished()
		if err != nil {
			// One background unit's failure does not stop the others, and it
			// does not touch what is published: the scope keeps answering
			// stale from its carried predecessor.
			c.log.Warn("a deferred unit failed", "component", component, "provider_id", d.unit.ProviderID,
				"scope_key", d.unit.ScopeKey, "diagnostic_code", provider.CodeOf(err))
			continue
		}
		sealed = append(sealed, sealedUnit{providerID: d.unit.ProviderID, scopeKey: d.unit.ScopeKey, unit: unit})
	}
	if len(sealed) == 0 {
		abort()
		return nil
	}
	err = l.publish(ctx, snap, sel, ref, active, sealed)
	abort()
	return err
}

// runOne builds one deferred unit in the work generation, under the heavy
// analyzer admission gate.
func (l *lateSealer) runOne(ctx context.Context, work *generation, d deferredUnit) (model.UnitID, error) {
	started := l.c.now()
	spec, err := d.unit.Spec(l.c.cfgHash)
	if err != nil {
		return "", err
	}
	state, exists, err := l.c.opts.Store.UnitState(ctx, spec.ID)
	if err != nil {
		return "", err
	}
	if exists && state == model.UnitSealed {
		// An earlier tick sealed it and could not publish; the unit is
		// immutable, so it is published now rather than rebuilt.
		return spec.ID, nil
	}
	if d.unit.Heavy {
		release, err := l.c.sched.Admit(ctx, d.unit.Reservation)
		if err != nil {
			return "", err
		}
		defer release()
	}
	if _, err := work.run(ctx, d.unit, spec); err != nil {
		return "", err
	}
	l.observe(l.c.now().Sub(started))
	return spec.ID, nil
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
// units in place of the stale predecessors they replace.
//
// The membership is re-derived by planning over the same snapshot rather than
// copied: the plan answers, per scope, with the unit whose identity that
// snapshot justifies, which for every unchanged member is the member itself.
// A publication whose active generation has moved to another snapshot is
// dropped instead: publishing over it would replace a newer generation with an
// older one's source. The sealed units stay sealed and the next index reuses
// them without running anything.
func (l *lateSealer) publish(ctx context.Context, snap model.SnapshotID, sel provider.Selection,
	ref string, active model.GenerationID, sealed []sealedUnit) error {
	c := l.c
	if active == 0 {
		return invalid("a deferred publication needs an active generation to extend")
	}
	pinned, err := c.opts.Store.PinGeneration(ctx, c.repo, active, statusLeaseTTL)
	if err != nil {
		return err
	}
	current := pinned.Binding().SnapshotID
	pinned.Close()
	if current != snap {
		c.log.Info("deferred units were sealed over a superseded snapshot; they will be reused by the next index",
			"component", component, "units", len(sealed))
		return nil
	}
	view, err := snapshot.OpenView(ctx, c.opts.Store, c.opts.CAS, snap)
	if err != nil {
		return err
	}
	in := plan.Inputs{View: view, Selection: sel, Store: c.opts.Store, PrevGen: active,
		CarriedPage: c.carriedPage(active), Config: c.opts.Config}
	p, err := plan.Build(ctx, in)
	if err != nil {
		return err
	}
	replacing := make(map[string]model.UnitID, len(sealed))
	for _, s := range sealed {
		replacing[plan.Key(s.providerID, s.scopeKey)] = s.unit
	}
	// Every sealed unit must be the unit this plan derives for its scope. If
	// it is not, the snapshot or the configuration moved under the background
	// work and the unit is not this generation's answer; nothing is published.
	for _, u := range p.Units {
		key := plan.Key(u.ProviderID, u.ScopeKey)
		want, ok := replacing[key]
		if !ok {
			continue
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
	}
	if len(replacing) == 0 {
		return nil
	}

	pubGen, err := c.opts.Store.BeginGeneration(ctx, c.repo, snap, c.cfgHash, ref)
	if err != nil {
		return err
	}
	g := &generation{c: c, view: view, sel: sel, plan: p, gen: pubGen, prev: active,
		snap: model.Snapshot{ID: snap, RepositoryID: c.repo}, started: c.now(), caps: newCapabilityReport()}
	for _, s := range p.States {
		g.caps.add(s)
	}
	err = l.attach(ctx, g, replacing)
	if err == nil {
		g.coverage()
		states := g.caps.finish(c.log)
		_, err = c.opts.Store.Activate(ctx, pubGen, active, healthOf(states), states, NormalizationVersion)
	}
	if err != nil {
		if abortErr := c.opts.Store.Abort(context.WithoutCancel(ctx), pubGen); abortErr != nil {
			c.log.Warn("the failed publication generation could not be aborted", "component", component,
				"generation_id", int64(pubGen), "diagnostic_code", provider.CodeOf(abortErr))
		}
		return err
	}
	c.log.Info("deferred units published", "component", component, "repository_id", string(c.repo),
		"generation_id", int64(pubGen), "units", len(replacing))
	c.retain(ctx)
	return nil
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

// Promote moves one scope's deferred unit to the head of the background queue
// and answers what the caller is waiting for (Section 11.6). A scope that is
// not queued answers zero units: it is not pending, and the active generation
// already holds whatever it has.
func (c *Coordinator) Promote(ctx context.Context, providerID, scopeKey string) (Pending, error) {
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
