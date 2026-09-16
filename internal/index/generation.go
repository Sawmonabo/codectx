package index

import (
	"context"
	"errors"
	"maps"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Sawmonabo/codectx/internal/index/delta"
	"github.com/Sawmonabo/codectx/internal/index/plan"
	"github.com/Sawmonabo/codectx/internal/ledger"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/paced"
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
	// ledgerRun is this pass's ledger run. It is opened before the capture, so
	// a pass that fails before a generation exists is still a recorded run.
	ledgerRun *ledger.Run
	caps      *capabilityReport
	// sealed holds the plan keys of the deferred units a publication
	// generation attaches (Section 11.6). They are members of this generation
	// even though the plan still marks their scopes deferred, because the plan
	// only reuses units the previous generation selected and these sealed into
	// a staging generation instead. It is nil on the indexing path.
	sealed map[string]bool
	// failedScopes holds, per plan key, the typed reason a DEFERRED unit of
	// this publication's batch did not seal. A deferred scope that failed is
	// neither covered nor still running, and telling the three apart is what
	// keeps a provider whose other scopes published from being reported as
	// though none of them had. It is nil on the indexing path.
	failedScopes map[string]unitFailure

	// failures aggregates the units that failed, per provider. It is the
	// indexing path's record of what did not seal, and a publication
	// generation is seeded with the foreground generation's copy of it:
	// those scopes are re-planned by the publication and still have nothing
	// behind them, so losing them here republishes them as coverage -- which
	// is how a provider whose every precise unit failed came back `fresh`.
	//
	// It is an aggregate and not a per-unit map because it outlives the run:
	// a repository can fail thousands of scopes, and what the report needs is
	// a count, a planned total and a bounded sample, all of which are one
	// small entry per provider whatever failed.
	failures map[string]*providerFailures

	// mu guards everything the unit workers accumulate.
	mu sync.Mutex
	// published holds the providers a unit of this generation actually
	// succeeded for -- ran or attached. It is not the same question as "the
	// plan gave this provider a unit": a provider whose every unit failed was
	// planned and holds nothing, and softening its failure row on that basis
	// would report a capability with no facts as partial.
	published map[string]bool
	runs      []model.ProviderResult
	// runsTotal counts every run the generation produced, including the ones
	// past the per-result wire ceiling that runs does not carry. The ceiling is
	// a page-sized bound on one response (class B); the omitted count is what
	// keeps it from being a silent drop.
	runsTotal   int64
	reused      int64
	built       int64
	carried     int64
	invalidated int64
	parsed      int64
	// planned, failed and subdivided are the run row's remaining totals.
	// Nothing else counted them: the result publishes what this generation
	// holds, and these are what it attempted and what it lost on the way.
	planned    int64
	failed     int64
	subdivided int64
}

// unitFailure is the typed reason one unit did not seal: the diagnostic
// family, the safe message and the bounded particulars the provider attached.
// The code alone cannot tell an analyzer that could not be started from one
// that exited nonzero, which is the distinction an operator acts on.
type unitFailure struct {
	code    string
	message string
	details map[string]string
}

// maxFailedScopesNamed bounds the failed scope keys one capability row names.
// A repository can fail thousands of scopes and the row must fit the same
// detail bound either way, so past this many the row publishes the count in
// units_failed and flags the list as cut rather than growing with the failure.
const maxFailedScopesNamed = 8

// providerFailures aggregates one provider's failed scopes for the capability
// fold: how many failed, how many were planned, and a bounded, sorted sample
// of the scope keys with the reason each carried.
type providerFailures struct {
	units   int
	planned int
	named   []failedScope
}

// add folds one failed scope in, keeping the lexicographically first
// maxFailedScopesNamed of them. The sample is ordered by scope key and never
// by arrival: the units of one provider are built concurrently, and both
// details_json and diagnostic_code fold into the AnalysisKey, so an
// arrival-ordered sample would key two identical runs differently.
func (f *providerFailures) add(scopeKey string, failure unitFailure) {
	f.units++
	at := len(f.named)
	for at > 0 && f.named[at-1].scopeKey > scopeKey {
		at--
	}
	if at == maxFailedScopesNamed {
		return
	}
	f.named = slices.Insert(f.named, at, failedScope{scopeKey: scopeKey, failure: failure})
	if len(f.named) > maxFailedScopesNamed {
		f.named = f.named[:maxFailedScopesNamed]
	}
}

// attempt captures, plans, builds and publishes exactly once.
//
// The ledger run is opened here and not at publication: the capture, the plan
// and the ref all run before any generation id exists, and a pass that fails in
// one of them would otherwise be a run nothing recorded. The generation is
// attached to the run row once BeginGeneration returns and stays null
// otherwise, and the deferred finish closes the run on every exit path --
// including the one that aborts the staging generation.
func (c *Coordinator) attempt(ctx context.Context, req model.IndexRequest) (res model.IndexResult, err error) {
	g := &generation{c: c, req: req, started: c.now(), caps: c.newCapabilityReport()}
	g.ledgerRun = c.newRun(ledger.KindIndex)
	ctx = g.ledgerRun.Context(ctx)
	// The reclaimer's own total when this run opened. What it gave back while
	// the run was open is the difference, read at the finish.
	freedBefore := paced.FreedBytes()
	runCtx := ctx
	defer func() {
		// Reported again at the finish so a pass that failed still states what
		// it got through, not zeros. The publish path reports at activation as
		// well, which is what a live reader sees while retention still runs.
		g.report()
		recordReclaim(runCtx, freedBefore)
		g.ledgerRun.Finish(endOutcome(err))
	}()
	// The plan's whole-snapshot input run is a file under the work directory
	// for as long as the units that stream from it are running, and no longer.
	defer func() { _ = g.plan.Close() }()
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
	g.ledgerRun.AttachGeneration(int64(gen))
	res, err = g.publish(ctx)
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

// captureBuilder is the snapshot builder this generation captures with. It is
// its own function so the operator settings it carries -- index.capture_max_retries
// and index.capture_retry_deadline, the only escape hatch from a worktree that
// never settles -- are reachable by a test without running a capture.
func (g *generation) captureBuilder() *snapshot.Builder {
	c := g.c
	return &snapshot.Builder{Root: c.opts.Root, Policy: c.policy, Repository: c.repo,
		SourcePolicyHash: c.opts.Config.SourcePolicyHash(), Store: c.opts.Store, CAS: c.opts.CAS,
		Git: c.opts.Git, Lock: c.opts.Lock, Logger: c.log,
		MaxRetries:    c.opts.Config.Index.CaptureMaxRetries.Value(),
		RetryDeadline: c.opts.Config.Index.CaptureRetryDeadline.Std()}
}

// capture builds the immutable snapshot this generation is about and reads the
// active pointer it will publish over.
func (g *generation) capture(ctx context.Context) (err error) {
	c := g.c
	ctx, span := ledger.Start(ctx, stageCapture, "")
	defer func() { span.End(endOutcome(err), ledger.Measured{}, err) }()
	b := g.captureBuilder()
	walkCtx, walk := ledger.Start(ctx, stageWalk, "")
	snap, err := b.Build(walkCtx)
	walk.AddOut(int64(snap.FileCount))
	walk.End(endOutcome(err), ledger.Measured{}, err)
	if err != nil {
		return err
	}
	g.snap = snap
	span.AddOut(int64(snap.FileCount))
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
func (g *generation) planUnits(ctx context.Context) (err error) {
	c := g.c
	ctx, span := ledger.Start(ctx, stagePlan, "")
	defer func() { span.End(endOutcome(err), ledger.Measured{}, err) }()
	// Detection walks the workspace itself, so it is given the same Git
	// exclusion the capture applies. The configured policy alone has no ignore
	// hook at all, which had detection proposing scopes over every ignored
	// tree in the checkout -- source the generation can never contain.
	policy, err := snapshot.TraversalPolicy(ctx, c.policy, c.opts.Root, c.opts.Git)
	if err != nil {
		return err
	}
	span.AddIn(int64(g.snap.FileCount))
	sel, err := c.opts.Registry.Select(ctx, c.opts.Root, policy, c.enablement)
	if err != nil {
		return err
	}
	g.sel = sel
	prev := g.prev
	if g.req.Full || g.req.Rebuild {
		prev = 0
	}
	in := plan.Inputs{View: g.view, Selection: sel, Store: c.opts.Store, PrevGen: prev,
		Config: c.opts.Config, TempDir: c.workDir}
	if prev != 0 {
		in.CarriedPage = c.carriedPage(prev)
	}
	p, err := plan.Build(ctx, in)
	if err != nil {
		return err
	}
	g.plan = p
	g.planned = int64(len(p.Reuse) + len(p.Carry))
	span.AddOut(g.planned)
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
	if err := g.coverage(ctx); err != nil {
		return model.IndexResult{}, err
	}
	if err := g.c.recordSuppliedIndexes(ctx, g.gen); err != nil {
		return model.IndexResult{}, err
	}
	states := g.caps.finish(g.c.log)
	health := healthOf(states)
	activateCtx, activation := ledger.Start(ctx, stageActivation, "")
	binding, err := g.c.opts.Store.Activate(activateCtx, g.gen, g.prev, health, states, NormalizationVersion)
	activation.End(endOutcome(err), ledger.Measured{}, err)
	if err != nil {
		return model.IndexResult{}, err
	}
	g.report()
	g.c.retain(ctx)
	g.c.collect(ctx)
	// Deferred work is enqueued only after the base generation is published:
	// Section 11.6 is explicit that nothing dependence-shaped runs before base
	// readiness, and ruling Q9 makes the background tick run it even when no
	// query ever asks.
	g.c.late.enqueue(g, deferred)
	runs, omitted := g.runsPage()
	return model.IndexResult{Binding: binding, Health: health, Status: model.GenerationActive,
		Completeness: states, UnitsReused: g.reused, UnitsBuilt: g.built, UnitsCarried: g.carried,
		UnitsInvalidated: g.invalidated, FilesParsed: g.parsed, FilesCaptured: int64(g.snap.FileCount),
		Runs: runs, RunsOmitted: omitted,
		StartedAt: g.started, CompletedAt: g.c.now()}, nil
}

// report hands the run row its totals. It is called once activation has
// succeeded, when every count the result publishes is final, so the ledger's
// figures and model.IndexResult's are the same counters and cannot disagree.
func (g *generation) report() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.ledgerRun.Report(ledger.Totals{
		FileCount:       int64(g.snap.FileCount),
		SourceBytes:     int64(g.snap.SourceBytes),
		UnitsPlanned:    g.planned,
		UnitsSucceeded:  g.reused + g.carried + g.built,
		UnitsFailed:     g.failed,
		UnitsSubdivided: g.subdivided,
	})
}

// endOutcome is how a run or a stage ended. A unit has a third ending and
// spells it in unitOutcome.
func endOutcome(err error) ledger.Outcome {
	if err != nil {
		return ledger.OutcomeFailed
	}
	return ledger.OutcomeOK
}

// unitOutcome tells a unit's three endings apart. A subdivided unit succeeded
// in parts and is neither a plain success nor a failure, and reporting it as
// either would hide the one full-build reason an operator acts on.
func unitOutcome(res outcome, err error) ledger.Outcome {
	switch {
	case err != nil && absent(provider.CodeOf(err)):
		return ledger.OutcomeUnavailable
	case err != nil:
		return ledger.OutcomeFailed
	case res.subdivided:
		return ledger.OutcomeSubdivided
	default:
		return ledger.OutcomeOK
	}
}

// unitEnding is how a unit ended on a path that never reached its provider: a
// unit already sealed under the same key is attached rather than rebuilt, and
// anything else here failed before the run.
//
// Success means attached and nothing else, because the two other ways a unit
// returns no error -- a run that succeeded, and an optional provider's failure
// that the generation absorbs -- have both already closed the span with their
// own outcome, and a span records one end.
func unitEnding(err error) ledger.Outcome {
	if err == nil {
		return ledger.OutcomeReused
	}
	return endingFor(provider.CodeOf(err))
}

// endingFor is the terminal state a unit that produced nothing takes, decided
// by its diagnostic code alone: a unit that reached no output because
// something it needed was not there is unavailable, and one that was asked to
// do its work and could not is failed. Keeping the two apart is the whole
// value of the row -- "unavailable" that cannot say which scope and why is the
// assertion this account exists to replace -- so every path that closes a
// unit's span decides it here rather than naming an outcome directly.
func endingFor(code string) ledger.Outcome {
	if absent(code) {
		return ledger.OutcomeUnavailable
	}
	return ledger.OutcomeFailed
}

// absent reports whether a diagnostic code says the unit reached no output
// because something it needed was not there -- a provider with nothing to run
// against, or a tool this machine cannot supply -- rather than because work
// that ran went wrong. The two are the difference between "this machine cannot
// do it" and "this repository broke it", and an operator acts on them
// differently.
func absent(code string) bool {
	switch code {
	case model.CodeProviderUnavailable, model.CodeToolOffline, model.CodeToolUnsupportedPlatform,
		model.CodeToolFetchFailed, model.CodeToolDigestMismatch, model.CodeToolCorrupt,
		model.CodeToolOverrideInvalid:
		return true
	}
	return false
}

// reasonDeferred is what a unit's row says when this run handed it to the
// background sealer instead of running it.
const reasonDeferred = "the unit is deferred to background work after activation"

// int64Ptr is the address of a count a span reports at its end. Measured's
// fields are pointers because an unavailable measurement is absent and never
// zero; a count the caller has is always available.
func int64Ptr(n int64) *int64 { return &n }

// runsPage is the run list the result publishes and the number of runs that
// did not fit it. Runs is a wire-sized page -- at most model.MaxRecordsPerResult
// summaries -- and a generation with more runs than that must say so rather
// than serve a short list as the whole of it. It is a method and not two
// expressions at the publish site so the honesty of the count is reachable by
// a test without rebuilding a generation's whole publish path.
func (g *generation) runsPage() ([]model.ProviderResult, int64) {
	return g.runs, g.runsTotal - int64(len(g.runs))
}

// attachReused makes every unit the plan proved identical a member of this
// generation. Storage re-checks each one's inputs against the snapshot, so a
// plan that was wrong about reuse is refused here rather than published.
func (g *generation) attachReused(ctx context.Context) (err error) {
	ctx, span := ledger.Start(ctx, stageAttachReused, "")
	defer func() {
		span.End(endOutcome(err), ledger.Measured{ItemsIn: int64Ptr(int64(len(g.plan.Reuse))), ItemsOut: &g.reused}, err)
	}()
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
func (g *generation) attachCarried(ctx context.Context) (err error) {
	ctx, span := ledger.Start(ctx, stageAttachCarried, "")
	defer func() {
		span.End(endOutcome(err), ledger.Measured{ItemsIn: int64Ptr(int64(len(g.plan.Carry))), ItemsOut: &g.carried}, err)
	}()
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
func (g *generation) build(ctx context.Context) (_ []plan.Unit, err error) {
	ctx, span := ledger.Start(ctx, stageBuild, "")
	defer func() { span.End(endOutcome(err), ledger.Measured{ItemsOut: &g.built}, err) }()
	var deferred []plan.Unit
	var group *unitGroup
	current := ""
	// The plan streams its units, so no provider's whole unit list is ever in
	// heap: a group is opened when its first unit arrives and drained when the
	// provider changes. The units arrive grouped by provider by construction --
	// Plan.Units answers in Selection.Active order -- which is what makes a
	// provider change the end of its group.
	walkErr := g.eachUnit(func(u plan.Unit) error {
		g.planned++
		span.AddIn(1)
		// The unit's row exists from the moment the plan names it, before
		// admission and before any provider is reached. A unit that never
		// runs is then a row that says so with its reason, instead of a
		// capability reported unavailable that nothing can substantiate.
		unitSpan := ledger.Plan(ctx, u.ProviderID, u.ScopeKey, u.ProviderID)
		if u.ProviderID != current {
			if group != nil {
				err := group.wait()
				group = nil
				if err != nil {
					return err
				}
			}
			current = u.ProviderID
		}
		if u.Deferred {
			// Not this run's work: it is queued for the background sealer,
			// which records it under its own run. Leaving the row planned
			// would have this run report it as never admitted.
			unitSpan.End(ledger.OutcomeSkipped, ledger.Measured{Failure: reasonDeferred}, nil)
			deferred = append(deferred, u)
			return nil
		}
		if group == nil {
			group = g.newUnitGroup(ctx)
		}
		return group.submit(u, unitSpan)
	})
	err = walkErr
	if walkErr != nil {
		// A group left running by the failing walk is drained before the error
		// is reported, so no unit outlives the call that started it. Its own
		// error is the one submit or wait already returned.
		if group != nil {
			_ = group.wait()
		}
		return nil, walkErr
	}
	if group != nil {
		if err = group.wait(); err != nil {
			return nil, err
		}
	}
	return deferred, nil
}

// eachUnit walks the plan's units. A plan assembled by hand rather than by
// plan.Build may carry no unit sequence at all, which is a plan that runs
// nothing -- the late-seal work generation is exactly that.
func (g *generation) eachUnit(yield func(plan.Unit) error) error {
	if g.plan.Units == nil {
		return nil
	}
	return g.plan.Units(yield)
}

// unitGroup runs one provider's units with bounded concurrency as the plan
// hands them over. It is the streaming shape of what was one []plan.Unit per
// provider: the same worker bound, the same first-fatal-failure-cancels-the
// -rest precedence, and a live set of at most workers units.
type unitGroup struct {
	g      *generation
	ctx    context.Context
	cancel context.CancelFunc
	sem    chan struct{}
	wg     sync.WaitGroup
	once   sync.Once
	fatal  error
}

func (g *generation) newUnitGroup(ctx context.Context) *unitGroup {
	runCtx, cancel := context.WithCancel(ctx)
	return &unitGroup{g: g, ctx: runCtx, cancel: cancel, sem: make(chan struct{}, g.c.workers)}
}

// submit admits one unit against the group's worker slots, blocking while they
// are all busy. A group whose first fatal failure has already cancelled it
// stops admitting and reports that failure, which ends the plan walk.
func (u *unitGroup) submit(unit plan.Unit, span *ledger.Span) error {
	select {
	case u.sem <- struct{}{}:
	case <-u.ctx.Done():
		u.wg.Wait()
		err := u.fatal
		if err == nil {
			err = model.Canceled(u.ctx.Err())
		}
		// The group stopped admitting before this unit had a worker, so it
		// never reached its work and says exactly that.
		span.End(ledger.OutcomeUnavailable, ledger.Measured{
			DiagnosticCode: model.CodeProviderUnavailable, Failure: ledger.ReasonNotAdmitted}, nil)
		return err
	}
	u.wg.Add(1)
	go func() {
		defer u.wg.Done()
		defer func() { <-u.sem }()
		if err := u.g.unit(u.ctx, unit, span); err != nil {
			u.once.Do(func() { u.fatal = err; u.cancel() })
		}
	}()
	return nil
}

// wait drains the group and reports its first fatal failure. It releases the
// group's context whatever the outcome, so an abandoned group leaks nothing.
func (u *unitGroup) wait() error {
	u.wg.Wait()
	u.cancel()
	return u.fatal
}

// unit builds one unit. The returned error is nonnil only when the failure
// must abort the whole generation: a required provider's unit, a storage
// failure, or a cancellation.
func (g *generation) unit(ctx context.Context, u plan.Unit, span *ledger.Span) (err error) {
	// Every path out of this call closes the unit's planned row. run closes it
	// first for a unit that reached a provider and End is idempotent, so this
	// states the outcome only for the paths that never do: the attach of a
	// unit already sealed, and the failures before the run.
	defer func() { span.End(unitEnding(err), ledger.Measured{}, err) }()
	spec, err := u.Spec(g.c.cfgHash)
	if err != nil {
		return err
	}
	if u.Heavy {
		release, admitErr := g.c.sched.Admit(ctx, u.Reservation)
		if admitErr != nil {
			// The admission gate refused or was cancelled, so the unit never
			// reached its work: unavailable with that reason, not a failure of
			// a provider that was never asked.
			span.End(ledger.OutcomeUnavailable, ledger.Measured{
				DiagnosticCode: provider.CodeOf(admitErr), Failure: ledger.ReasonNotAdmitted}, nil)
			return admitErr
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
		g.markPublished(u.ProviderID)
		g.mu.Unlock()
		return nil
	}
	out, runErr := g.run(ctx, u, spec, span)
	if runErr == nil {
		g.record(u, out)
		return nil
	}
	return g.failure(ctx, u, out, runErr)
}

// outcome is one unit's run: what the provider reported, how many files it
// actually read, and whether building it discarded a predecessor that was
// usable until now.
type outcome struct {
	result      model.ProviderResult
	parsed      int64
	invalidated bool
	// subdivided reports the one full-build reason that is not an
	// invalidation, which the run row counts separately from a failure.
	subdivided bool
	// span is the unit's own span. It is still open when the unit failed:
	// the failure path ends it with the one typed reason it also writes to
	// the provider-run row, so the two records of one failure cannot
	// disagree about it.
	span *ledger.Span
}

// run executes one unit through its delta applier when the provider has one,
// and through the provider runtime otherwise.
func (g *generation) run(ctx context.Context, u plan.Unit, spec model.UnitSpec, span *ledger.Span) (res outcome, err error) {
	c := g.c
	// The unit's span was opened when the plan named it and begins here: the
	// providers' own inner spans find it in the context they are handed and
	// nest under it without naming it, so a unit's cost is the sum of parts
	// recorded by code that knows nothing about this package.
	ctx = span.Begin(ctx)
	defer func() {
		// The span travels out on every path, because a unit that failed is
		// ended by the failure path instead: that is where the typed reason
		// is built, and typing the same error here as well would produce two
		// records of one failure that can disagree.
		res.span = span
		if err != nil {
			return
		}
		span.End(unitOutcome(res, nil),
			ledger.Measured{ItemsIn: int64Ptr(u.InputCount), ItemsOut: int64Ptr(res.parsed)}, nil)
	}()
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
			Build: build, Inputs: u.Inputs, Unit: ureq, WorkDir: filepath.Join(c.workDir, u.ProviderID)}
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
			invalidated: previous != "" && !subdivided(out), subdivided: subdivided(out)}, nil
	}
	// A file-invalidated unit has no delta applier and no recorded
	// predecessor, so whether it replaced one is asked of the previous
	// generation directly. Only a rebuilt unit asks, so the cost is bounded by
	// the change set and a cold index asks nothing at all.
	invalidated, err := g.replacesPredecessor(ctx, u, spec)
	if err != nil {
		return outcome{}, err
	}
	w, err := c.opts.Store.BeginUnit(ctx, g.gen, build, u.Inputs)
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
	return outcome{result: result, parsed: u.InputCount, invalidated: invalidated && runErr == nil}, runErr
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
	return u.InputCount
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

// markPublished records that this generation holds a member for a provider.
// The caller holds g.mu.
func (g *generation) markPublished(providerID string) {
	if g.published == nil {
		g.published = map[string]bool{}
	}
	g.published[providerID] = true
}

// record folds one successful unit into the run's metrics and capability rows.
func (g *generation) record(u plan.Unit, out outcome) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.markPublished(u.ProviderID)
	g.built++
	if out.subdivided {
		g.subdivided++
	}
	g.parsed += out.parsed
	if out.invalidated {
		g.invalidated++
	}
	g.runsTotal++
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
func (g *generation) failure(ctx context.Context, u plan.Unit, out outcome, cause error) error {
	res := out.result
	// The unit's span ends here whatever its failure does to the generation:
	// one left open reads as interrupted and the unit's cost simply vanishes
	// from the breakdown. recordFailure ends it first, with the reason it also
	// writes to the run row, and an end is once-only -- so this is the backstop
	// for the two exits below, neither of which records a row to agree with.
	m := ledger.Measured{ItemsIn: int64Ptr(u.InputCount), ItemsOut: int64Ptr(out.parsed)}
	defer func() { out.span.End(endingFor(provider.CodeOf(cause)), m, cause) }()
	if ctx.Err() != nil && errors.Is(cause, ctx.Err()) {
		return cause
	}
	p, ok := g.c.opts.Registry.Lookup(u.ProviderID)
	if !ok || p.Descriptor().Required {
		return cause
	}
	f := g.recordFailure(ctx, u, out, cause)
	g.mu.Lock()
	defer g.mu.Unlock()
	g.failed++
	if res.RunID != "" {
		g.runsTotal++
		if len(g.runs) < model.MaxRecordsPerResult {
			g.runs = append(g.runs, res)
		}
	}
	// The reason is aggregated per provider and folded into the capability
	// rows by coverage, which publishes a count, a planned total and a
	// bounded sample of the scopes instead of one row per failed unit.
	if g.failures == nil {
		g.failures = map[string]*providerFailures{}
	}
	agg, ok := g.failures[u.ProviderID]
	if !ok {
		agg = &providerFailures{}
		g.failures[u.ProviderID] = agg
	}
	agg.add(u.ScopeKey, f)
	return nil
}

// recordFailure types one unit's failure, keeps it on the run row and tells
// the operator. The durable row is what survives the log: a capability row
// carries one exemplar per provider capability and never the tool output, so
// without it the particulars of every other failed scope are lost the moment
// the process exits.
//
// The log line carries the message and the bounded particulars, but never the
// standard-error tail: that is raw analyzer output, which Section 6 keeps out
// of ordinary logs. The tail is on the run row alone. The scope key is the
// only identifier logged; a provider's own source bytes and native keys never
// reach a log line at all.
func (g *generation) recordFailure(ctx context.Context, u plan.Unit, out outcome, cause error) unitFailure {
	res := out.result
	f := typedFailure(cause)
	stored := model.RunFailure{ScopeKey: u.ScopeKey, Code: f.code, Message: f.message, Details: f.details}
	// One value, two records: the unit's span publishes the reason the run row
	// publishes, so the ledger and the store can never disagree about why a
	// unit failed. The error is not handed to End -- it would fill the span
	// from the error's own text, which is what typedFailure deliberately drops
	// for an untyped error, and the two records would then diverge on exactly
	// the failures nobody has a safe message for.
	out.span.End(endingFor(stored.Code), ledger.Measured{ItemsIn: int64Ptr(u.InputCount),
		ItemsOut: int64Ptr(out.parsed), DiagnosticCode: stored.Code, Failure: stored.Message}, nil)
	if res.RunID != "" {
		// Under a context that survives the cancellation the failure may have
		// arrived with: a reason dropped because the run was cancelled is the
		// case the operator most needs the row for.
		if err := g.c.opts.Store.RecordRunFailure(context.WithoutCancel(ctx), res.RunID, stored); err != nil {
			logTyped(g.c.log, "the reason a provider unit failed could not be recorded", err,
				"component", component, "provider_id", u.ProviderID, "scope_key", u.ScopeKey)
		}
	}
	args := []any{"component", component, "provider_id", u.ProviderID, "scope_key", u.ScopeKey,
		"generation_id", int64(g.gen), "diagnostic_code", f.code, "message", f.message}
	for _, k := range slices.Sorted(maps.Keys(f.details)) {
		if k != model.DetailStderrTail {
			args = append(args, k, f.details[k])
		}
	}
	g.c.log.Warn("an optional provider unit failed; its capability is published failed", args...)
	return f
}

// typedFailure projects an error onto the reason a capability row and a run
// row publish. An untyped error has no safe message to publish -- it is a
// defect's text, not a diagnostic -- so only its family is kept.
func typedFailure(cause error) unitFailure {
	f := unitFailure{code: provider.CodeOf(cause)}
	var typed *model.Error
	if errors.As(cause, &typed) {
		f.message, f.details = typed.Message, typed.Details
	}
	return f
}

// coverage publishes the fresh rows and the one degradation that is otherwise
// invisible: a provider that is available and produced no unit at all
// (residual 113). "Available, zero units" is not fresh coverage, and without a
// row for it status would report a capability nothing answers as healthy.
func (g *generation) coverage(ctx context.Context) (err error) {
	_, span := ledger.Start(ctx, stageCoverage, "")
	defer func() { span.End(endOutcome(err), ledger.Measured{}, err) }()
	covered, deferred, failed, published, err := g.coveredProviders()
	if err != nil {
		return err
	}
	for _, p := range g.sel.Active {
		d := p.Descriptor()
		for _, capability := range d.Capabilities {
			// A scope that failed is published as the failure it is, naming
			// that scope and its reason, before the three coverage answers
			// below: none of them may speak for a capability one of whose
			// scopes is a known failure. It is recorded first so `reported`
			// sees it.
			if f, ok := failed[d.ID]; ok {
				g.caps.addFailures(d.ID, capability, f)
			}
			// And a capability that did publish a scope is partial, not
			// failed: the facts of that scope are in this generation and
			// answer queries. The test is whether a member exists, not
			// whether a unit was planned -- a provider whose every unit
			// failed published nothing and stays failed.
			if published[d.ID] {
				g.caps.coveredElsewhere(d.ID, capability)
			}
			switch {
			// Deferred is a scope still running and nothing else: a provider
			// with work in flight is not fresh coverage, whatever its other
			// scopes did.
			case deferred[d.ID]:
				g.caps.addDeferred(d.ID, capability)
			case covered[d.ID]:
				g.caps.addFresh(d.ID, capability)
			default:
				g.caps.addUnavailable(d.ID, capability)
			}
		}
	}
	return nil
}

// failedScope is one scope of a provider that did not seal, with the reason.
type failedScope struct {
	scopeKey string
	failure  unitFailure
}

// coveredProviders is the set of providers this generation actually holds a
// member for -- a unit it ran, a unit it reused, or a stale unit it carried --
// and, separately, the providers whose only work was deferred. A deferred unit
// is not a member of this generation: it seals into a later publication, so
// counting it as coverage would report a capability fresh while nothing had
// been written for it, which is the false readiness Section 11.6 forbids. The
// Reuse key is the provider id and the scope key joined by NUL, which is the
// frozen shape of plan.Key.
//
// published is the narrower question the partial ruling needs: the providers a
// member was actually written or attached for. Coverage counts a planned unit,
// because "available and produced no unit at all" is the degradation it exists
// to catch; a failure row may only be softened by facts that exist.
func (g *generation) coveredProviders() (covered, deferred map[string]bool, failed map[string]*providerFailures, published map[string]bool, err error) {
	out := make(map[string]bool, len(g.sel.Active))
	deferred = make(map[string]bool, len(g.sel.Active))
	failed = make(map[string]*providerFailures, len(g.sel.Active))
	published = make(map[string]bool, len(g.sel.Active))
	g.mu.Lock()
	for id := range g.published {
		published[id] = true
	}
	g.mu.Unlock()
	// Streamed, not ranged: the plan's unit list is repository-sized, and the
	// three answers here are one bounded entry per provider whatever it holds.
	g.mu.Lock()
	for id, agg := range g.failures {
		clone := *agg
		clone.named = slices.Clone(agg.named)
		failed[id] = &clone
	}
	g.mu.Unlock()
	record := func(u plan.Unit, f unitFailure) {
		agg, ok := failed[u.ProviderID]
		if !ok {
			agg = &providerFailures{}
			failed[u.ProviderID] = agg
		}
		agg.add(u.ScopeKey, f)
	}
	planned := make(map[string]int, len(g.sel.Active))
	if err := g.eachUnit(func(u plan.Unit) error {
		planned[u.ProviderID]++
		if u.Deferred {
			key := plan.Key(u.ProviderID, u.ScopeKey)
			// A publication generation holds the deferred units that have
			// already sealed; of the rest, the ones this batch tried and
			// could not seal are failures, and only what is left is still
			// background work.
			switch {
			case g.sealed[key]:
				out[u.ProviderID] = true
				published[u.ProviderID] = true
			case g.failedScopes[key].code != "":
				record(u, g.failedScopes[key])
			default:
				deferred[u.ProviderID] = true
			}
			return nil
		}
		out[u.ProviderID] = true
		return nil
	}); err != nil {
		return nil, nil, nil, nil, err
	}
	for id, agg := range failed {
		agg.planned = planned[id]
	}
	for _, c := range g.plan.Carry {
		out[c.ProviderID] = true
		published[c.ProviderID] = true
	}
	for key := range g.plan.Reuse {
		if id, _, ok := strings.Cut(key, "\x00"); ok {
			out[id] = true
			published[id] = true
		}
	}
	return out, deferred, failed, published, nil
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

// recordSuppliedIndexes records every index this run was handed against the
// generation about to be published, resolved or not.
//
// It runs while the generation is still staging, so a run that never activates
// leaves no row behind, and its failure fails the publish: a generation
// activated without the record would answer "this generation was built with no
// supplied index" for a run that supplied one, which is the false report the
// record exists to prevent.
//
// Resolution is asked of the generation rather than of the filesystem: the
// question is not whether a file exists now but whether this generation holds
// the unit that path produced. A path that matched nothing selects no unit and
// is recorded unresolved, which is the case the doctor check reports.
func (c *Coordinator) recordSuppliedIndexes(ctx context.Context, gen model.GenerationID) error {
	for _, si := range c.opts.SuppliedIndexes {
		resolved := true
		if _, err := c.opts.Store.SelectedUnit(ctx, gen, si.ProviderID, si.ScopeKey); err != nil {
			var typed *model.Error
			if !errors.As(err, &typed) || typed.Details["reason"] != sqlite.ReasonNotFound {
				return err
			}
			resolved = false
		}
		if err := c.opts.Store.RecordSuppliedIndex(ctx, gen, si.Path, resolved); err != nil {
			return err
		}
	}
	return nil
}
