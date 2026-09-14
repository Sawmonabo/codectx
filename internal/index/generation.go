package index

import (
	"context"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Sawmonabo/codectx/internal/index/delta"
	"github.com/Sawmonabo/codectx/internal/index/plan"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider"
	"github.com/Sawmonabo/codectx/internal/reconcile"
	"github.com/Sawmonabo/codectx/internal/snapshot"
	"github.com/Sawmonabo/codectx/internal/storage/sqlite"
)

// index runs one indexing pass, retrying exactly once from the capture when
// another process published between this run's capture and its activation.
// One retry and no more: a caller that loses twice is contending with a writer
// that is winning, and a retry loop would be the busy loop Section 13.1
// forbids.
func (c *Coordinator) index(ctx context.Context, req model.IndexRequest) (model.IndexResult, error) {
	for attempt := 0; ; attempt++ {
		res, err := c.attempt(ctx, req)
		if err == nil {
			return res, nil
		}
		var typed *model.Error
		if attempt == 0 && errors.As(err, &typed) && typed.Code == model.CodeVersionConflict {
			c.log.Info("another activation intervened; re-capturing and publishing again",
				"component", component, "repository_id", string(c.repo))
			continue
		}
		return model.IndexResult{}, err
	}
}

// generation is one indexing pass over one capture.
type generation struct {
	c   *Coordinator
	req model.IndexRequest

	snap model.Snapshot
	view *snapshot.View
	sel  provider.Selection
	plan plan.Plan
	// prev is the generation that was active when this run captured. It is the
	// pointer Activate compares against, so a concurrent publication loses
	// with CTX_VERSION_CONFLICT instead of being overwritten unseen.
	prev model.GenerationID
	gen  model.GenerationID
	// builtRef is the ref this generation was opened under; the deferred work
	// it enqueues publishes under the same one.
	builtRef string

	started time.Time
	caps    *capabilityReport
	// sealed holds the plan keys of the deferred units a publication
	// generation attaches (Section 11.6). They are members of this generation
	// even though the plan still marks their scopes deferred, because the plan
	// only reuses units the previous generation selected and these sealed into
	// a staging generation instead. It is nil on the indexing path.
	sealed map[string]bool

	// mu guards everything the unit workers accumulate.
	mu          sync.Mutex
	runs        []model.ProviderResult
	reused      int64
	built       int64
	carried     int64
	invalidated int64
	parsed      int64
}

// attempt captures, plans, builds and publishes exactly once.
func (c *Coordinator) attempt(ctx context.Context, req model.IndexRequest) (model.IndexResult, error) {
	g := &generation{c: c, req: req, started: c.now(), caps: c.newCapabilityReport()}
	if err := c.opts.Store.EnsureRepository(ctx, c.repo, c.opts.Root.Path); err != nil {
		return model.IndexResult{}, err
	}
	if err := g.capture(ctx); err != nil {
		return model.IndexResult{}, err
	}
	if err := g.planUnits(ctx); err != nil {
		return model.IndexResult{}, err
	}
	ref, err := c.ref(ctx)
	if err != nil {
		return model.IndexResult{}, err
	}
	gen, err := c.opts.Store.BeginGeneration(ctx, c.repo, g.snap.ID, c.cfgHash, ref)
	if err != nil {
		return model.IndexResult{}, err
	}
	g.gen, g.builtRef = gen, ref
	res, err := g.publish(ctx)
	if err != nil {
		// The staging generation is aborted under a context that survives the
		// cancellation that may have caused the failure: a generation left
		// staging holds units no later run can complete, and startup recovery
		// would have to clean it up instead.
		if abortErr := c.opts.Store.Abort(context.WithoutCancel(ctx), gen); abortErr != nil {
			logTyped(c.log, "the failed staging generation could not be aborted", abortErr,
				"component", component, "generation_id", int64(gen))
		}
		return model.IndexResult{}, err
	}
	return res, nil
}

// capture builds the immutable snapshot this generation is about and reads the
// active pointer it will publish over.
func (g *generation) capture(ctx context.Context) error {
	c := g.c
	b := &snapshot.Builder{Root: c.opts.Root, Policy: c.policy, Repository: c.repo,
		SourcePolicyHash: c.opts.Config.SourcePolicyHash(), Store: c.opts.Store, CAS: c.opts.CAS,
		Git: c.opts.Git, Lock: c.opts.Lock, Logger: c.log}
	snap, err := b.Build(ctx)
	if err != nil {
		return err
	}
	g.snap = snap
	if g.prev, err = c.activeGeneration(ctx); err != nil {
		return err
	}
	view, err := snapshot.OpenView(ctx, c.opts.Store, c.opts.CAS, snap.ID)
	if err != nil {
		return err
	}
	g.view = view
	return nil
}

// planUnits selects the providers and derives the plan. A full or rebuilding
// run plans against no previous generation, so nothing is carried and no unit
// imports a delta; a unit whose identity is nevertheless already sealed is
// still attached rather than rebuilt, because an immutable unit with the same
// key is the same bytes by construction (see build).
func (g *generation) planUnits(ctx context.Context) error {
	c := g.c
	// Detection walks the workspace itself, so it is given the same Git
	// exclusion the capture applies. The configured policy alone has no ignore
	// hook at all, which had detection proposing scopes over every ignored
	// tree in the checkout -- source the generation can never contain.
	policy, err := snapshot.TraversalPolicy(ctx, c.policy, c.opts.Root, c.opts.Git)
	if err != nil {
		return err
	}
	sel, err := c.opts.Registry.Select(ctx, c.opts.Root, policy, c.enablement)
	if err != nil {
		return err
	}
	g.sel = sel
	prev := g.prev
	if g.req.Full || g.req.Rebuild {
		prev = 0
	}
	in := plan.Inputs{View: g.view, Selection: sel, Store: c.opts.Store, PrevGen: prev, Config: c.opts.Config}
	if prev != 0 {
		in.CarriedPage = c.carriedPage(prev)
	}
	p, err := plan.Build(ctx, in)
	if err != nil {
		return err
	}
	g.plan = p
	for _, s := range p.States {
		g.caps.add(s)
	}
	return nil
}

// publish attaches, builds, validates and activates.
func (g *generation) publish(ctx context.Context) (model.IndexResult, error) {
	if err := g.attachReused(ctx); err != nil {
		return model.IndexResult{}, err
	}
	if err := g.attachCarried(ctx); err != nil {
		return model.IndexResult{}, err
	}
	deferred, err := g.build(ctx)
	if err != nil {
		return model.IndexResult{}, err
	}
	g.coverage()
	states, _ := g.caps.finish(g.c.log)
	health := healthOf(states)
	binding, err := g.c.opts.Store.Activate(ctx, g.gen, g.prev, health, states, NormalizationVersion)
	if err != nil {
		return model.IndexResult{}, err
	}
	g.c.retain(ctx)
	// Deferred work is enqueued only after the base generation is published:
	// Section 11.6 is explicit that nothing dependence-shaped runs before base
	// readiness, and ruling Q9 makes the background tick run it even when no
	// query ever asks.
	g.c.late.enqueue(g, deferred)
	return model.IndexResult{Binding: binding, Health: health, Status: model.GenerationActive,
		Completeness: states, UnitsReused: g.reused, UnitsBuilt: g.built, UnitsCarried: g.carried,
		UnitsInvalidated: g.invalidated, FilesParsed: g.parsed, FilesCaptured: int64(g.snap.FileCount),
		Runs: g.runs, StartedAt: g.started, CompletedAt: g.c.now()}, nil
}

// attachReused makes every unit the plan proved identical a member of this
// generation. Storage re-checks each one's inputs against the snapshot, so a
// plan that was wrong about reuse is refused here rather than published.
func (g *generation) attachReused(ctx context.Context) error {
	for _, unit := range g.plan.Reuse {
		if err := g.c.opts.Store.AttachUnit(ctx, g.gen, unit); err != nil {
			return err
		}
		g.reused++
	}
	return nil
}

// attachCarried carries the stale predecessor of every refreshing deferred
// scope into this generation (Section 13.3, ruling Q4): the capability keeps
// answering as `stale` with its provenance distance while the fresh unit is
// built in the background, and is replaced at the next activation.
func (g *generation) attachCarried(ctx context.Context) error {
	for _, carried := range g.plan.Carry {
		err := g.c.opts.Store.AttachCarried(ctx, g.gen, carried.Unit,
			sqlite.Carry{DistanceGenerations: carried.DistanceGenerations, DistanceFiles: carried.DistanceFiles})
		if err != nil {
			var typed *model.Error
			if errors.As(err, &typed) && typed.Code == model.CodeSnapshotChanged {
				// A predecessor whose inputs no longer exist is not carried.
				// The planner refuses that case from the manifest; storage
				// refuses it from the snapshot rows, which is the authority.
				// The scope then simply has no member here.
				continue
			}
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

// build runs every unit the plan did not reuse, provider by provider in
// dependency order, and returns the deferred ones for the background tick. A
// provider's units run concurrently; the next provider starts only when the
// previous one's units are sealed, because storage refuses to open a unit
// whose declared dependency is not yet sealed.
func (g *generation) build(ctx context.Context) ([]plan.Unit, error) {
	var deferred []plan.Unit
	for i := 0; i < len(g.plan.Units); {
		id := g.plan.Units[i].ProviderID
		j := i
		for j < len(g.plan.Units) && g.plan.Units[j].ProviderID == id {
			j++
		}
		group := g.plan.Units[i:j]
		i = j
		var run []plan.Unit
		for _, u := range group {
			if u.Deferred {
				deferred = append(deferred, u)
				continue
			}
			run = append(run, u)
		}
		if err := g.buildGroup(ctx, run); err != nil {
			return nil, err
		}
	}
	return deferred, nil
}

// buildGroup builds one provider's units with bounded concurrency. The first
// failure that must abort the generation cancels the rest; an optional
// provider's failure is recorded and the group continues, because Section 13.3
// forbids a disabled or failing optional tool from taking a healthy base
// generation down with it.
func (g *generation) buildGroup(ctx context.Context, units []plan.Unit) error {
	if len(units) == 0 {
		return nil
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	sem := make(chan struct{}, g.c.workers)
	var wg sync.WaitGroup
	var once sync.Once
	var fatal error
	for _, u := range units {
		select {
		case sem <- struct{}{}:
		case <-runCtx.Done():
			wg.Wait()
			if fatal != nil {
				return fatal
			}
			return model.Canceled(runCtx.Err())
		}
		wg.Add(1)
		go func(u plan.Unit) {
			defer wg.Done()
			defer func() { <-sem }()
			if err := g.unit(runCtx, u); err != nil {
				once.Do(func() { fatal = err; cancel() })
			}
		}(u)
	}
	wg.Wait()
	return fatal
}

// unit builds one unit. The returned error is nonnil only when the failure
// must abort the whole generation: a required provider's unit, a storage
// failure, or a cancellation.
func (g *generation) unit(ctx context.Context, u plan.Unit) error {
	spec, err := u.Spec(g.c.cfgHash)
	if err != nil {
		return err
	}
	if u.Heavy {
		release, err := g.c.sched.Admit(ctx, u.Reservation)
		if err != nil {
			return err
		}
		defer release()
	}
	// A unit whose key is already sealed is the same bytes under the same
	// configuration by construction, so it is attached rather than rebuilt.
	// This is what makes Section 12.4's A -> B -> C -> A promise real: the
	// planner only consults the previous generation, and A's units are
	// selected by a retained generation the plan never looked at.
	state, exists, err := g.c.opts.Store.UnitState(ctx, spec.ID)
	if err != nil {
		return err
	}
	if exists && state == model.UnitSealed {
		if err := g.c.opts.Store.AttachUnit(ctx, g.gen, spec.ID); err != nil {
			return err
		}
		g.mu.Lock()
		g.reused++
		g.mu.Unlock()
		return nil
	}
	out, err := g.run(ctx, u, spec)
	if err == nil {
		g.record(out)
		return nil
	}
	return g.failure(ctx, u, out.result, err)
}

// outcome is one unit's run: what the provider reported, how many files it
// actually read, and whether building it discarded a predecessor that was
// usable until now.
type outcome struct {
	result      model.ProviderResult
	parsed      int64
	invalidated bool
}

// run executes one unit through its delta applier when the provider has one,
// and through the provider runtime otherwise.
func (g *generation) run(ctx context.Context, u plan.Unit, spec model.UnitSpec) (outcome, error) {
	c := g.c
	p, ok := c.opts.Registry.Lookup(u.ProviderID)
	if !ok {
		return outcome{}, internalErr("the plan names provider " + u.ProviderID + ", which is not registered")
	}
	binding, err := g.sourceBinding(ctx, p, u.ScopeKey)
	if err != nil {
		return outcome{}, err
	}
	runID, err := c.opts.Store.BeginProviderRun(ctx, g.gen, u.ProviderID, u.ProviderVersion)
	if err != nil {
		return outcome{}, err
	}
	resolver, err := reconcile.New(c.opts.Store, c.repo, u.DependsOn)
	if err != nil {
		return outcome{}, err
	}
	build := model.UnitBuild{Spec: spec, AnalysisConfigHash: c.cfgHash, OriginRunID: runID,
		SourceBinding: binding, Dependencies: u.DependsOn}
	ureq := provider.UnitRequest{
		Binding: model.Binding{RepositoryID: c.repo, SnapshotID: g.snap.ID, GenerationID: g.gen},
		Unit:    spec, Run: runID, Content: g.view, Resolver: resolver,
	}
	if applier, ok := c.appliers[u.ProviderID]; ok {
		previous := g.plan.Previous[plan.Key(u.ProviderID, u.ScopeKey)]
		req := delta.Request{Generation: g.gen, Previous: previous,
			Build: build, Inputs: inputsOf(u.Inputs), Unit: ureq, WorkDir: filepath.Join(c.workDir, u.ProviderID)}
		// The applier calls CompleteProviderRun itself, on both paths and
		// under a context that survives cancellation; calling it again here
		// would fail as "run is not running".
		out, err := applier.Apply(ctx, req)
		if err != nil {
			// out.Result carries the run the applier actually opened, so the
			// failure still publishes a provider-run row; without it the
			// operator sees that a capability failed and nothing about the run
			// that failed.
			return outcome{result: out.Result}, err
		}
		return outcome{result: annotate(out), parsed: parsedBy(applier, u, out),
			invalidated: previous != "" && !subdivided(out)}, nil
	}
	// A file-invalidated unit has no delta applier and no recorded
	// predecessor, so whether it replaced one is asked of the previous
	// generation directly. Only a rebuilt unit asks, so the cost is bounded by
	// the change set and a cold index asks nothing at all.
	invalidated, err := g.replacesPredecessor(ctx, u, spec)
	if err != nil {
		return outcome{}, err
	}
	w, err := c.opts.Store.BeginUnit(ctx, g.gen, build, inputsOf(u.Inputs))
	if err != nil {
		return outcome{}, err
	}
	result, runErr := provider.RunUnit(ctx, p, ureq, provider.StoreUnit(c.opts.Store, w), c.limits, c.opts.Pool)
	// The run is recorded on both paths, under a context that survives the
	// cancellation a sink failure raises: a run left `running` is a row no
	// later generation can complete.
	if err := c.opts.Store.CompleteProviderRun(context.WithoutCancel(ctx), result, provider.CodeOf(runErr)); err != nil {
		runErr = errors.Join(runErr, err)
	}
	return outcome{result: result, parsed: int64(len(u.Inputs)), invalidated: invalidated && runErr == nil}, runErr
}

// parsedBy is how many of a delta-built unit's files were actually read. A
// SCIP delta re-imports exactly the documents whose hash moved, so counting its
// declared inputs would report a whole-repository parse for a one-file edit --
// the very claim Section 13.1's no-op refresh evidence rests on. Every other
// applier reparses its whole unit, and the engine has no incremental mode at
// all, so there the declared inputs are the truth.
func parsedBy(a delta.Applier, u plan.Unit, out delta.Result) int64 {
	if a.Kind() == delta.KindSCIP && !out.Full {
		return out.Delta.Changed
	}
	return int64(len(u.Inputs))
}

// replacesPredecessor reports whether the previous generation selected a
// different sealed unit for this scope, which is what makes rebuilding this
// one an invalidation rather than a first build.
func (g *generation) replacesPredecessor(ctx context.Context, u plan.Unit, spec model.UnitSpec) (bool, error) {
	if g.prev == 0 {
		return false, nil
	}
	prev, err := g.c.opts.Store.SelectedUnit(ctx, g.prev, u.ProviderID, u.ScopeKey)
	if err != nil {
		var typed *model.Error
		if errors.As(err, &typed) && typed.Details["reason"] == sqlite.ReasonNotFound {
			return false, nil
		}
		return false, err
	}
	return prev != "" && prev != spec.ID, nil
}

// annotate surfaces the delta decision on the unit's own result: which of the
// four full-build reasons applied, and whether the emit that produced it was a
// filtered one. Only the reasons that name an unusable predecessor count as an
// invalidation, which is why the reason and not the Full flag is published.
func annotate(out delta.Result) model.ProviderResult {
	res := out.Result
	if len(res.Capabilities) == 0 {
		return res
	}
	rows := make([]model.CapabilityState, 0, len(res.Capabilities))
	for _, c := range res.Capabilities {
		if out.Full {
			c = c.WithDetail("delta_full_reason", out.FullReason)
		}
		rows = append(rows, c.WithDetail("delta_filtered", strconv.FormatBool(out.Filtered)))
	}
	res.Capabilities = rows
	return res
}

// subdivided reports the one full-build reason that is not an invalidation:
// the provider subdivided the unit for a reproducible engine crash, so this
// run publishes no whole-unit state to diff against although the predecessor
// itself is healthy. The delta package states that a coordinator projecting
// invalidation counts must read the reason rather than the Full flag.
func subdivided(out delta.Result) bool {
	return out.Full && out.FullReason == delta.FullSubdivided
}

// sourceBinding is how the unit is opened (residual 112): a provider that can
// prove its output is about the captured bytes answers for itself, and every
// other provider reads only through the pinned view, which is verified by
// construction.
func (g *generation) sourceBinding(ctx context.Context, p provider.Provider, scopeKey string) (model.SourceBinding, error) {
	verifier, ok := p.(interface {
		Verify(context.Context, model.SnapshotView, string) (model.SourceBinding, error)
	})
	if !ok {
		return model.SourceBindingVerified, nil
	}
	return verifier.Verify(ctx, g.view, scopeKey)
}

// record folds one successful unit into the run's metrics and capability rows.
func (g *generation) record(out outcome) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.built++
	g.parsed += out.parsed
	if out.invalidated {
		g.invalidated++
	}
	if len(g.runs) < model.MaxRecordsPerResult {
		g.runs = append(g.runs, out.result)
	}
	for _, s := range out.result.Capabilities {
		g.caps.add(s)
	}
}

// failure decides what one unit's failure does to the generation. A required
// provider takes the generation down; an optional one publishes a failed
// capability and the generation continues degraded (Section 13.3).
func (g *generation) failure(ctx context.Context, u plan.Unit, res model.ProviderResult, cause error) error {
	if ctx.Err() != nil && errors.Is(cause, ctx.Err()) {
		return cause
	}
	p, ok := g.c.opts.Registry.Lookup(u.ProviderID)
	if !ok || p.Descriptor().Required {
		return cause
	}
	code := provider.CodeOf(cause)
	g.mu.Lock()
	defer g.mu.Unlock()
	if res.RunID != "" && len(g.runs) < model.MaxRecordsPerResult {
		g.runs = append(g.runs, res)
	}
	for _, capability := range p.Descriptor().Capabilities {
		g.caps.addFailure(u.ProviderID, capability, u.ScopeKey, code)
	}
	// The scope key is the only identifier logged; a provider's own output,
	// source bytes and native keys never reach an ordinary log line.
	g.c.log.Warn("an optional provider unit failed; its capability is published failed",
		"component", component, "provider_id", u.ProviderID, "scope_key", u.ScopeKey,
		"generation_id", int64(g.gen), "diagnostic_code", code)
	return nil
}

// coverage publishes the fresh rows and the one degradation that is otherwise
// invisible: a provider that is available and produced no unit at all
// (residual 113). "Available, zero units" is not fresh coverage, and without a
// row for it status would report a capability nothing answers as healthy.
func (g *generation) coverage() {
	covered, deferred := g.coveredProviders()
	for _, p := range g.sel.Active {
		d := p.Descriptor()
		for _, capability := range d.Capabilities {
			switch {
			// Deferred wins over covered: a provider with one scope still
			// running is not fresh coverage, whatever its other scopes did.
			case deferred[d.ID]:
				g.caps.addDeferred(d.ID, capability)
			case covered[d.ID]:
				g.caps.addFresh(d.ID, capability)
			default:
				g.caps.addUnavailable(d.ID, capability)
			}
		}
	}
}

// coveredProviders is the set of providers this generation actually holds a
// member for -- a unit it ran, a unit it reused, or a stale unit it carried --
// and, separately, the providers whose only work was deferred. A deferred unit
// is not a member of this generation: it seals into a later publication, so
// counting it as coverage would report a capability fresh while nothing had
// been written for it, which is the false readiness Section 11.6 forbids. The
// Reuse key is the provider id and the scope key joined by NUL, which is the
// frozen shape of plan.Key.
func (g *generation) coveredProviders() (covered, deferred map[string]bool) {
	out := make(map[string]bool, len(g.sel.Active))
	deferred = make(map[string]bool, len(g.sel.Active))
	for _, u := range g.plan.Units {
		if u.Deferred {
			// A publication generation holds the deferred units that have
			// already sealed; the rest are still background work.
			if g.sealed[plan.Key(u.ProviderID, u.ScopeKey)] {
				out[u.ProviderID] = true
			} else {
				deferred[u.ProviderID] = true
			}
			continue
		}
		out[u.ProviderID] = true
	}
	for _, c := range g.plan.Carry {
		out[c.ProviderID] = true
	}
	for key := range g.plan.Reuse {
		if id, _, ok := strings.Cut(key, "\x00"); ok {
			out[id] = true
		}
	}
	return out, deferred
}

// capabilitiesOf is one provider's declared capability list, empty when the
// registry does not hold it.
func (g *generation) capabilitiesOf(providerID string) []string {
	p, ok := g.c.opts.Registry.Lookup(providerID)
	if !ok {
		return nil
	}
	return p.Descriptor().Capabilities
}

// inputsOf is the re-iterable input stream storage and the dependence applier
// require: every walk yields the same inputs, in the ascending FileID order
// the planner already sorted them into. The slice is the plan's own and is
// never mutated here.
func inputsOf(inputs []model.UnitInput) func(yield func(model.UnitInput) error) error {
	return func(yield func(model.UnitInput) error) error {
		for _, in := range inputs {
			if err := yield(in); err != nil {
				return err
			}
		}
		return nil
	}
}

// healthOf is Section 13.3's whole-generation health. An unavailable optional
// capability leaves a base generation fresh; anything partial, stale or failed
// makes it degraded. A generation with no coverage at all is not published:
// storage refuses a generation with no members.
func healthOf(states []model.CapabilityState) model.GenerationHealth {
	health := model.HealthFresh
	for _, s := range states {
		switch s.State {
		case model.CapabilityPartial, model.CapabilityStale, model.CapabilityFailed:
			health = model.HealthDegraded
		}
	}
	return health
}
