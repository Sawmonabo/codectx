package index

import (
	"context"
	"errors"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Sawmonabo/codectx/internal/admission"
	"github.com/Sawmonabo/codectx/internal/index/delta"
	"github.com/Sawmonabo/codectx/internal/index/plan"
	"github.com/Sawmonabo/codectx/internal/ledger"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/paced"
	"github.com/Sawmonabo/codectx/internal/provider"
	"github.com/Sawmonabo/codectx/internal/reconcile"
	"github.com/Sawmonabo/codectx/internal/residency"
	"github.com/Sawmonabo/codectx/internal/snapshot"
	"github.com/Sawmonabo/codectx/internal/storage/sqlite"
	"github.com/Sawmonabo/codectx/internal/workspace"
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

	started time.Time
	// ledgerRun is this pass's ledger run. It is opened before the capture, so
	// a pass that fails before a generation exists is still a recorded run.
	ledgerRun *ledger.Run
	caps      *capabilityReport

	// failures aggregates the units that failed, per provider: the run's
	// record of what did not seal.
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
	invalidated int64
	parsed      int64
	// planned and failed are the run row's remaining totals. Nothing else
	// counted them: the result publishes what this generation holds, and
	// these are what it attempted and what it lost on the way.
	planned int64
	failed  int64

	// walked is how many of the plan's units the build walk reached, and
	// walkDone says it reached all of them. Together they are where a run that
	// ended early resumes the plan to account for what it never got to: the
	// unit sequence is re-iterable and answers in one fixed order, so the
	// units past walked are exactly the ones nothing ever started.
	walked   int64
	walkDone bool

	// passes is one measurement per provider pass, in the order the passes
	// ran, and passesTotal counts every pass, as runs and runsTotal do.
	// Written under mu.
	passes      []model.ProviderPass
	passesTotal int64
}

// unitFailure is the typed reason one unit did not seal: the diagnostic
// family, the safe message and the bounded particulars the provider attached.
// The code alone cannot tell an analyzer that could not be started from one
// that exited nonzero, which is the distinction an operator acts on.
type unitFailure struct {
	code    string
	message string
	// remediation is what the provider said an operator can do about this
	// failure. It travels with the message rather than inside details: the
	// detail map is the provider's own bounded budget, and a remediation
	// charged to it could be the entry a busy row drops.
	remediation string
	details     map[string]string
}

// maxFailedScopesNamed bounds the failed scope keys one capability row names.
// A repository can fail thousands of scopes and the row must fit the same
// detail bound either way, so past this many the row publishes the count in
// units_failed and flags the list as cut rather than growing with the failure.
const maxFailedScopesNamed = 8

// providerFailures aggregates one provider's failed scopes for the capability
// fold: how many failed, how many were planned, and a bounded, sorted sample
// of the scope keys with the reason each carried.
//
// THE COUNTING RULE, stated once for every surface that reports these figures:
// a capability's planned total is every scope the plan assigned to its
// provider for this generation -- the units it runs and the sealed units it
// reuses -- and its failed total is those of
// them that did not seal. How a scope is counted is plan.Plan.CountReused's
// rule, which the run row's planned total follows too; the per-provider
// figures are counted in coveredProviders and published in exactly one place,
// failureRow.publish, as the units_planned and units_failed details of the
// capability row. The completion block of `codectx index`, `codectx status`
// and the index-status tool all render that row and none of them recounts, so
// one generation cannot read differently on two of them.
//
// A total of the walked units alone would leave the reused scopes out, and a
// provider that sealed nine scopes and failed two would read "2 planned, 2
// failed": a row saying every unit failed over a capability that answers most
// queries.
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
	g := &generation{c: c, req: req, started: c.now(), caps: newCapabilityReport()}
	g.ledgerRun = c.newRun(ledger.KindIndex)
	ctx = g.ledgerRun.Context(ctx)
	// The reclaimer's own total when this run opened. What it gave back while
	// the run was open is the difference, read at the finish.
	freedBefore := paced.FreedBytes()
	defer func() {
		// Reported again at the finish so a pass that failed still states what
		// it got through, not zeros. The publish path reports at activation as
		// well, which is what a live reader sees while retention still runs.
		// The peak is read HERE and not at the activation report: retention,
		// collection and the reclaim below all run after activation, and a
		// high-water mark read before them would silently leave them out. It
		// is the kernel's mark for the whole process, so a freed byte cannot
		// lower it and reading it before the reclaim loses nothing.
		g.report(residency.Read().Peak)
		recordReclaim(ctx, freedBefore)
		g.ledgerRun.Finish(endOutcome(err))
		c.attachRunLedger(ctx, &res, g.ledgerRun, err)
	}()
	// The plan's whole-snapshot input run is a file under the work directory
	// for as long as the units that stream from it are running, and no longer.
	defer func() { _ = g.plan.Close() }()
	// Registered after the close above and so running before it: the account
	// below walks the plan's units, which must not be called once the plan is
	// closed. It is what makes a run that died between the plan and the build
	// still say what it had planned.
	defer func() { g.accountUnwalkedUnits(ctx) }()
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
	g.gen = gen
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
//
// The workspace lock is passed in rather than read from the options: this
// composition may have taken it at its first build rather than at its open,
// and the builder must be handed the lock this run actually holds. A nil one
// would make the builder take a second lock of its own and release it under
// the run.
func (g *generation) captureBuilder(lock *snapshot.WorkspaceLock) *snapshot.Builder {
	c := g.c
	return &snapshot.Builder{Root: c.opts.Root, Policy: c.policy, Repository: c.repo,
		SourcePolicyHash: c.opts.Config.SourcePolicyHash(), Store: c.opts.Store, CAS: c.opts.CAS,
		Git: c.opts.Git, Lock: lock, Logger: c.log,
		MaxRetries:    c.opts.Config.Index.CaptureMaxRetries.Value(),
		RetryDeadline: c.opts.Config.Index.CaptureRetryDeadline.Std()}
}

// capture builds the immutable snapshot this generation is about and reads the
// active pointer it will publish over.
func (g *generation) capture(ctx context.Context) (err error) {
	c := g.c
	ctx, span := ledger.Start(ctx, stageCapture, "")
	defer func() { span.End(endOutcome(err), ledger.Measured{CPUUnattributed: ledger.CPUOverlapped}, err) }()
	// The caller took the lock before this generation began; asking for it
	// again is how the builder names the one this process holds, and it
	// acquires nothing a second time. It sits inside the span so a capture
	// that was refused the workspace is recorded as the failed capture it is,
	// and the release runs before the span closes.
	lock, release, err := c.hold(ctx, HoldNow)
	if err != nil {
		return err
	}
	defer release()
	b := g.captureBuilder(lock)
	walkCtx, walk := ledger.Start(ctx, stageWalk, "")
	snap, err := b.Build(walkCtx)
	walk.AddOut(int64(snap.FileCount))
	walk.End(endOutcome(err), ledger.Measured{CPUUnattributed: ledger.CPUOverlapped}, err)
	if err != nil {
		return err
	}
	g.snap = snap
	span.AddOut(int64(snap.FileCount))
	if g.prev, err = c.activeGeneration(ctx); err != nil {
		return err
	}
	// The view reads the manifest and blobs on the reader pool against the
	// last commit, so the snapshot is committed before planning and every
	// unit read through it: no source read of the pass takes a job on the
	// writer.
	if err := c.opts.Store.Flush(ctx); err != nil {
		return err
	}
	view, err := snapshot.OpenView(ctx, c.opts.Store, c.opts.CAS, snap.ID)
	if err != nil {
		return err
	}
	g.view = view
	return nil
}

// forceSnapshotPaths makes detection see every path the pinned snapshot
// holds, as the capture's own hooks made the capture admit them. Section 10.2
// forces a tracked path past ignore rules and past the vendor and generated
// exclusions, so a tracked `third_party/` project is in the manifest and is
// planned from it; a detection walk that pruned it would propose no scope for
// that project, and its unit would be planned with no detection to say the
// provider sees it. The answers are point and range lookups on the stored
// manifest, so nothing the size of the repository is held.
//
// A hook can only answer yes or no, so the first lookup that fails is kept and
// every later hook answers no; the returned function reports it, and the
// caller returns it once detection has finished. A path longer than any manifest path cannot be
// in the snapshot and is simply not held.
func (g *generation) forceSnapshotPaths(ctx context.Context, policy *workspace.Policy) func() error {
	var hookErr error
	store, id := g.c.opts.Store, g.snap.ID
	policy.ForceInclude = func(rel string) bool {
		if hookErr != nil || len(rel) > model.MaxPathBytes {
			return false
		}
		held := false
		hookErr = g.view.EachFile(ctx, model.FileSelection{Paths: []string{rel}}, func(fv model.FileVersion) error {
			// A tombstone names a path this snapshot records as gone.
			held = fv.Status != model.FileDeleted
			return nil
		})
		return hookErr == nil && held
	}
	policy.ForceIncludeDir = func(rel string) bool {
		prefix := rel + "/"
		if hookErr != nil || len(prefix) > model.MaxPathBytes {
			return false
		}
		// The manifest is ordered bytewise, so the first path after
		// `rel/` lies beneath rel exactly when anything does.
		page, err := store.SnapshotFiles(ctx, id, prefix, 1)
		if err != nil {
			hookErr = err
			return false
		}
		return len(page) > 0 && strings.HasPrefix(page[0].Path, prefix)
	}
	return func() error { return hookErr }
}

// planUnits selects the providers and derives the plan. A full or rebuilding
// run plans against no previous generation, so no unit imports a delta; a unit whose identity is nevertheless already sealed is
// still attached rather than rebuilt, because an immutable unit with the same
// key is the same bytes by construction (see build).
func (g *generation) planUnits(ctx context.Context) (err error) {
	c := g.c
	ctx, span := ledger.Start(ctx, stagePlan, "")
	defer func() { span.End(endOutcome(err), ledger.Measured{CPUUnattributed: ledger.CPUOverlapped}, err) }()
	// Detection walks the workspace itself, so it is given the same Git
	// exclusion the capture applies. The configured policy alone has no ignore
	// hook at all, and detection would propose scopes over every ignored tree
	// in the checkout -- source the generation can never contain.
	policy, err := snapshot.TraversalPolicy(ctx, c.policy, c.opts.Root, c.opts.Git)
	if err != nil {
		return err
	}
	hookErr := g.forceSnapshotPaths(ctx, &policy)
	span.AddIn(int64(g.snap.FileCount))
	sel, err := c.opts.Registry.Select(ctx, c.opts.Root, policy, c.enablement)
	if err == nil {
		err = hookErr()
	}
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
	p, err := plan.Build(ctx, in)
	if err != nil {
		return err
	}
	g.plan = p
	// The reused scopes are the planned scopes the build walk never yields;
	// the walk counts every other one as it reaches it, and the end-of-run
	// account the ones it never reached.
	p.CountReused(func(string) { g.planned++ })
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
	if err := g.build(ctx); err != nil {
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
	activation.End(endOutcome(err), ledger.Measured{CPUUnattributed: ledger.CPUOverlapped}, err)
	if err != nil {
		return model.IndexResult{}, err
	}
	// No peak yet: the run has not ended, and a mark taken here would be
	// reported as the run's own while retention and collection are still to
	// come. The finish above reports it.
	g.report(nil)
	g.c.retain(ctx)
	g.c.collect(ctx)
	runs, omitted := g.runsPage()
	return model.IndexResult{Binding: binding, Health: health, Status: model.GenerationActive,
		Completeness: states, UnitsReused: g.reused, UnitsBuilt: g.built,
		UnitsInvalidated: g.invalidated, FilesParsed: g.parsed, FilesCaptured: int64(g.snap.FileCount),
		Runs: runs, RunsOmitted: omitted, ProvidersDisabled: disabledProviders(g.c.opts.Config),
		Passes: g.passes, PassesOmitted: g.passesTotal - int64(len(g.passes)),
		StartedAt: g.started, CompletedAt: g.c.now()}, nil
}

// RunLedgerReader reads one recorded run back as the model carries it: the run
// row, one page of its stages, and how many stages that page left out. It is
// an interface here and satisfied in the composition, so the one conversion
// from a recorded span to a stage row stays where every other surface's
// conversion is and this package never grows a second.
type RunLedgerReader interface {
	Run(ctx context.Context, runID string) (*model.RunRecord, []model.StageRecord, int64, error)
}

// attachRunLedger states on a result what the run that produced it just did:
// the run row and its stages, read back from the ledger by that run's own id.
// A pass reports through it with its own run.
//
// It reads after the finish, and behind a flush, because finishing a run only
// moves in-memory state: the row turns terminal at the collector's next write,
// so a read without the barrier would render a finished run as still running
// and leave out the stages that ended last. And it reads BY ID, because the
// latest run of this repository may be another run, which would answer for the
// run that asked.
//
// A failed pass attaches nothing: its result is discarded by the caller, and
// the run it recorded is read through the status surfaces like any other. A
// failure to flush or to read leaves the result without a run rather than with
// half of one, and says so in the log: the block is an account of the run, and
// an account that is silently partial is worse than one that is absent.
func (c *Coordinator) attachRunLedger(ctx context.Context, res *model.IndexResult, run *ledger.Run, runErr error) {
	if runErr != nil || c.opts.RunLedgerReader == nil || run.ID() == "" {
		return
	}
	if err := c.opts.Ledger.Flush(ctx); err != nil {
		logTyped(c.log, "the run could not report on itself: its accounting was not written", err,
			"component", component, "run_id", run.ID())
		return
	}
	row, stages, omitted, err := c.opts.RunLedgerReader.Run(ctx, run.ID())
	if err != nil {
		logTyped(c.log, "the run could not report on itself: its accounting could not be read", err,
			"component", component, "run_id", run.ID())
		return
	}
	res.Run, res.Stages, res.StagesOmitted = row, stages, omitted
}

// report hands the run row its totals. It is called once activation has
// succeeded, when every count the result publishes is final, so the ledger's
// figures and model.IndexResult's are the same counters and cannot disagree.
//
// peak is the process's high-water resident size, and is nil everywhere but at
// the run's end: it is a measurement of the whole run, so only the caller that
// knows the run is over has one to give.
func (g *generation) report(peak *uint64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.ledgerRun.Report(ledger.Totals{
		FileCount:           int64(g.snap.FileCount),
		SourceBytes:         int64(g.snap.SourceBytes),
		UnitsPlanned:        g.planned,
		UnitsSucceeded:      g.reused + g.built,
		UnitsFailed:         g.failed,
		ProcessPeakRSSBytes: peak,
	})
}

// endOutcome is how a run or a stage ended. A unit that never reached its
// provider spells its ending in unitEnding.
func endOutcome(err error) ledger.Outcome {
	if err != nil {
		return ledger.OutcomeFailed
	}
	return ledger.OutcomeOK
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

// notAdmitted is how a unit that never reached its work ends: the diagnostic
// code that kept it out and the ledger's own reason. It waited in-process
// beside other goroutines, so its CPU is the overlapped kind the process-wide
// counters cannot attribute, and the span says so rather than leaving the
// columns null with no reason.
func notAdmitted(code string) ledger.Measured {
	return ledger.Measured{CPUUnattributed: ledger.CPUOverlapped, DiagnosticCode: code,
		Failure: ledger.ReasonNotAdmitted}
}

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
//
// It runs before the build, on the coordinator's goroutine alone, so the fold
// needs no lock here.
func (g *generation) attachReused(ctx context.Context) (err error) {
	ctx, span := ledger.Start(ctx, stageAttachReused, "")
	defer func() {
		span.End(endOutcome(err), ledger.Measured{CPUUnattributed: ledger.CPUOverlapped,
			ItemsIn: int64Ptr(int64(len(g.plan.Reuse))), ItemsOut: &g.reused}, err)
	}()
	for _, unit := range g.plan.Reuse {
		if err := g.c.opts.Store.AttachUnit(ctx, g.gen, unit); err != nil {
			return err
		}
		rows, err := g.c.opts.Store.UnitCapabilities(ctx, unit)
		if err != nil {
			return err
		}
		g.foldReused(rows)
		g.reused++
	}
	return nil
}

// foldReused adds a reused unit's stored capability rows to the report, in the
// same fold a built unit's rows go through. A unit the generation attaches
// instead of building reported these rows when it sealed; leaving them out
// would publish the generation's counts as covering only the units it rebuilt,
// and a reused unit that sealed degraded as though it were healthy. Each unit's
// rows are read and folded on their own, so no more of them is in heap at once
// than one unit's and the fold's own rows.
func (g *generation) foldReused(rows []model.CapabilityState) {
	for _, s := range rows {
		g.caps.add(s)
	}
}

// build runs every unit the plan did not reuse, provider by provider in
// dependency order. A provider's units run concurrently; the next provider starts only when the
// previous one's units are sealed and committed, because a unit resolves
// against its declared dependencies as of the last commit and storage refuses
// one whose dependency is not sealed there.
func (g *generation) build(ctx context.Context) (err error) {
	ctx, span := ledger.Start(ctx, stageBuild, "")
	defer func() {
		span.End(endOutcome(err), ledger.Measured{CPUUnattributed: ledger.CPUOverlapped, ItemsOut: &g.built}, err)
	}()
	// The units attached before the build are committed first: a unit of the
	// first provider that runs resolves its dependencies against the last
	// commit, and a dependency sealed only in the open group would be refused
	// there.
	if err := g.c.opts.Store.Flush(ctx); err != nil {
		return err
	}
	var group *unitGroup
	current := ""
	// The plan streams its units, so no provider's whole unit list is ever in
	// heap: a group is opened when its first unit arrives and drained when the
	// provider changes. The units arrive grouped by provider by construction --
	// Plan.Units answers in Selection.Active order -- which is what makes a
	// provider change the end of its group.
	walkErr := g.eachUnit(func(u plan.Unit) error {
		// The previous provider's group is drained before this unit is
		// counted or given a row: a group whose failure ends the walk here
		// leaves this unit one the walk never reached, which is what the
		// end-of-run account writes the row for. Counting it first would
		// leave a row nothing ever closed.
		if u.ProviderID != current {
			if group != nil {
				err := group.wait()
				group = nil
				if err != nil {
					return err
				}
				// The next provider's units read their dependencies from the
				// last commit, so what this provider sealed is committed before
				// the first of them runs: one commit per provider boundary.
				if err := g.c.opts.Store.Flush(ctx); err != nil {
					return err
				}
			}
			current = u.ProviderID
		}
		g.planned++
		g.walked++
		span.AddIn(1)
		// The unit's row exists from the moment the plan names it, before
		// admission and before any provider is reached. A unit that never
		// runs is then a row that says so with its reason, instead of a
		// capability reported unavailable that nothing can substantiate.
		unitSpan := ledger.Plan(ctx, u.ProviderID, u.ScopeKey, u.ProviderID)
		if group == nil {
			group = g.newUnitGroup(ctx, u.ProviderID)
		}
		return group.submit(u, unitSpan)
	})
	err = walkErr
	if walkErr == nil {
		// The walk reached every unit the plan named, so nothing is left for
		// the end-of-run account to pick up.
		g.walkDone = true
	}
	if walkErr != nil {
		// A group left running by the failing walk is drained before the error
		// is reported, so no unit outlives the call that started it. Its own
		// error is the one submit or wait already returned.
		if group != nil {
			_ = group.wait()
		}
		return walkErr
	}
	if group != nil {
		return group.wait()
	}
	return nil
}

// eachUnit walks the plan's units. A plan assembled by hand rather than by
// plan.Build may carry no unit sequence at all, which is a plan that runs
// nothing.
func (g *generation) eachUnit(yield func(plan.Unit) error) error {
	if g.plan.Units == nil {
		return nil
	}
	return g.plan.Units(yield)
}

// accountUnwalkedUnits gives every unit the plan named but the build walk
// never reached a row that says so, at the end of the run that planned them.
//
// A run that dies after the plan is derived and before the walk gets to a unit
// -- a ref that could not be read, a generation that could not be opened, a
// required provider's first unit failing and cancelling the rest -- would
// otherwise leave those units with no trace at all, which is exactly the
// invisibility the planned-unit row exists to remove, in the run an operator
// most wants to account for. Each row is written already terminal: nothing
// started the unit, so there is no wall to measure and no provider to name a
// reason, and the ledger's own reason for work that was never admitted is what
// it carries.
//
// The plan's unit sequence is re-iterable and answers in one fixed order, so
// resuming it past the units the walk reached names exactly the ones it did
// not. It must not be called after the plan is closed.
func (g *generation) accountUnwalkedUnits(ctx context.Context) {
	if g.walkDone {
		return
	}
	var seen int64
	err := g.eachUnit(func(u plan.Unit) error {
		seen++
		if seen <= g.walked {
			return nil
		}
		span := ledger.Plan(ctx, u.ProviderID, u.ScopeKey, u.ProviderID)
		span.End(ledger.OutcomeUnavailable, notAdmitted(model.CodeProviderUnavailable), nil)
		g.mu.Lock()
		g.planned++
		g.mu.Unlock()
		return nil
	})
	if err != nil {
		// The plan's own sequence could not be re-read, so this run cannot say
		// what it had left to do. The operator is told that much rather than
		// being left with a unit count that quietly stops short.
		logTyped(g.c.log, "the units this run never reached could not be accounted for", err,
			"component", component, "run_id", g.ledgerRun.ID())
	}
}

// unitGroup runs one provider's units with bounded concurrency as the plan
// hands them over: a live set of at most workers units, and the first fatal
// failure cancels the rest.
//
// The group is also the provider's whole pass over this run's units. A
// provider that keeps a stage has it open for exactly that pass, so what the
// stage holds warm -- the parser workers -- outlives every unit of the pass and
// is not given back, and started again, when two sequential units leave it
// empty between them.
type unitGroup struct {
	g      *generation
	ctx    context.Context
	cancel context.CancelFunc
	sem    chan struct{}
	wg     sync.WaitGroup
	once   sync.Once
	fatal  error

	provider   string
	closeStage func() model.StageFigures
	// started, writerBusy and units are the pass's measurement: its wall, the
	// store writer's busy time when it began, and the units it admitted.
	started    time.Time
	writerBusy time.Duration
	units      int64
}

// stageOpener is a provider that keeps a stage open across a pass over many
// units, and whose close answers what the stage measured. It is asserted
// structurally here, at the one call site, so the provider interface carries
// nothing only one provider needs; a provider without it runs each unit as it
// always has, and its pass has no stage figures.
type stageOpener interface {
	OpenStage(ctx context.Context) (closeStage func() model.StageFigures)
}

func (g *generation) newUnitGroup(ctx context.Context, providerID string) *unitGroup {
	runCtx, cancel := context.WithCancel(ctx)
	u := &unitGroup{g: g, ctx: runCtx, cancel: cancel, sem: make(chan struct{}, g.c.workers),
		provider: providerID, started: time.Now(), writerBusy: g.c.opts.Store.WriterBusy()}
	if p, ok := g.c.opts.Registry.Lookup(providerID); ok {
		if s, ok := p.(stageOpener); ok {
			u.closeStage = s.OpenStage(ctx)
		}
	}
	return u
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
		span.End(ledger.OutcomeUnavailable, notAdmitted(model.CodeProviderUnavailable), nil)
		return err
	}
	u.units++
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
// group's context whatever the outcome, so an abandoned group leaks nothing;
// closes the provider's stage after the last unit has left it, on every path,
// a cancelled pass included, since the stage leaves under the run it was
// opened in and consults no context of the close's; and records the pass's measurement on the generation
// after that close, so the wall covers the stage's drain and the stage's
// figures are final.
func (u *unitGroup) wait() error {
	u.wg.Wait()
	u.cancel()
	var stage *model.StageFigures
	if u.closeStage != nil {
		figures := u.closeStage()
		stage = &figures
		u.closeStage = nil
	}
	u.g.recordPass(providerPass(u.provider, u.units, time.Since(u.started),
		u.g.c.opts.Store.WriterBusy()-u.writerBusy, stage))
	return u.fatal
}

// recordPass keeps one pass's measurement for the result, within the same
// per-result page Runs is held to, and counts it either way.
func (g *generation) recordPass(pass model.ProviderPass) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.passesTotal++
	if len(g.passes) < model.MaxRecordsPerResult {
		g.passes = append(g.passes, pass)
	}
}

// providerPass assembles one pass's record and the three passing
// measurements of ADR-0012 decision 5 from its figures. A ratio is left
// absent when a figure it needs is unavailable or its denominator is zero:
// a pass that measured nothing neither passes nor fails, and reporting it as
// 0 would read as the best possible pass.
//
//   - WallPerCPU is wall × the most parses in flight ÷ Σ worker CPU, the
//     measurement that passes at 2 or less;
//   - StartsPerSlot is workers started ÷ the most parses in flight, which
//     passes at 1 or less;
//   - WriterBusyShare is writer busy ÷ wall, reported, with build order the
//     next lever at 0.8 or more.
func providerPass(providerID string, units int64, wall, writerBusy time.Duration,
	stage *model.StageFigures) model.ProviderPass {
	pass := model.ProviderPass{ProviderID: providerID, Units: units, WallMS: wall.Milliseconds(),
		WriterBusyMS: writerBusy.Milliseconds(), Stage: stage}
	pass.WriterBusyShare = passRatio(float64(pass.WriterBusyMS), pass.WallMS)
	if stage != nil {
		pass.StartsPerSlot = passRatio(float64(stage.WorkersStarted), stage.MaxInFlight)
		if stage.WorkerCPUMS != nil && stage.MaxInFlight > 0 {
			pass.WallPerCPU = passRatio(float64(pass.WallMS)*float64(stage.MaxInFlight), *stage.WorkerCPUMS)
		}
	}
	return pass
}

// passRatio is num ÷ den, or absent when den is not positive.
func passRatio(num float64, den int64) *float64 {
	if den <= 0 {
		return nil
	}
	r := num / float64(den)
	return &r
}

// unit builds one unit. The returned error is nonnil only when the failure
// must abort the whole generation: a required provider's unit, a storage
// failure, or a cancellation.
func (g *generation) unit(ctx context.Context, u plan.Unit, span *ledger.Span) (err error) {
	// Every path out of this call closes the unit's planned row. run closes it
	// first for a unit that reached a provider and End is idempotent, so this
	// states the outcome only for the paths that never do: the attach of a
	// unit already sealed, and the failures before the run.
	defer func() { span.End(unitEnding(err), ledger.Measured{CPUUnattributed: ledger.CPUOverlapped}, err) }()
	spec, err := u.Spec(g.c.cfgHash)
	if err != nil {
		return err
	}
	if u.Admit != nil {
		held, admitErr := g.c.sched.Admit(ctx, u.Admit)
		if admitErr != nil {
			// The admission gate refused or was cancelled, so the unit never
			// reached its work: unavailable with that reason, not a failure of
			// a provider that was never asked.
			span.End(ledger.OutcomeUnavailable, notAdmitted(provider.CodeOf(admitErr)), nil)
			return admitErr
		}
		defer held.Release()
		ctx = admission.WithHolding(ctx, held)
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
		// Read before the lock is taken: the read is a job on the store's
		// writer, which a unit worker holding the lock must never wait on.
		rows, err := g.c.opts.Store.UnitCapabilities(ctx, spec.ID)
		if err != nil {
			return err
		}
		g.mu.Lock()
		g.reused++
		g.markPublished(u.ProviderID)
		g.foldReused(rows)
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
		span.End(ledger.OutcomeOK,
			ledger.Measured{CPUUnattributed: ledger.CPUOverlapped,
				ItemsIn: int64Ptr(u.InputCount), ItemsOut: int64Ptr(res.parsed)}, nil)
	}()
	p, ok := c.opts.Registry.Lookup(u.ProviderID)
	if !ok {
		return outcome{}, internalErr("the plan names provider " + u.ProviderID + ", which is not registered")
	}
	binding, err := g.sourceBinding(ctx, p, u.ScopeKey)
	if err != nil {
		return outcome{}, err
	}
	// The unit's candidates resolve against its declared dependencies' aliases,
	// read once here from the last commit rather than once per candidate. The
	// read can refuse a dependency that is not committed sealed, so it runs
	// before the provider run is opened: a refusal after it would leave a run
	// row `running` that nothing completes.
	aliases, err := c.opts.Store.DependencyAliases(ctx, u.DependsOn)
	if err != nil {
		return outcome{}, err
	}
	resolver, err := reconcile.New(aliases, c.repo, u.DependsOn)
	if err != nil {
		return outcome{}, err
	}
	runID, err := c.opts.Store.BeginProviderRun(ctx, g.gen, u.ProviderID, u.ProviderVersion)
	if err != nil {
		return outcome{}, err
	}
	build := model.UnitBuild{Spec: spec, AnalysisConfigHash: c.cfgHash, OriginRunID: runID,
		SourceBinding: binding, Dependencies: u.DependsOn, DependencyKeys: u.DependencyKeys}
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
		return outcome{result: out.Result, parsed: parsedBy(applier, u, out),
			invalidated: previous != ""}, nil
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

// sourceBinding is how the unit is opened: a provider that can
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
	m := ledger.Measured{CPUUnattributed: ledger.CPUOverlapped,
		ItemsIn: int64Ptr(u.InputCount), ItemsOut: int64Ptr(out.parsed)}
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
	stored := model.RunFailure{ProviderID: u.ProviderID, ScopeKey: u.ScopeKey, Code: f.code,
		Message: f.message, Remediation: f.remediation, Details: f.details}
	// One value, two records: the unit's span publishes the reason the run row
	// publishes, so the ledger and the store can never disagree about why a
	// unit failed. The error is not handed to End -- it would fill the span
	// from the error's own text, which is what typedFailure deliberately drops
	// for an untyped error, and the two records would then diverge on exactly
	// the failures nobody has a safe message for.
	out.span.End(endingFor(stored.Code), ledger.Measured{CPUUnattributed: ledger.CPUOverlapped,
		ItemsIn: int64Ptr(u.InputCount), ItemsOut: int64Ptr(out.parsed),
		DiagnosticCode: stored.Code, Failure: stored.Message}, nil)
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
	if f.remediation != "" {
		args = append(args, "remediation", f.remediation)
	}
	details := withoutRawOutput(f.details)
	for _, k := range slices.Sorted(maps.Keys(details)) {
		args = append(args, k, details[k])
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
		f.message, f.details, f.remediation = typed.Message, typed.Details, typed.Remediation
	}
	return f
}

// coverage publishes the fresh rows and the one degradation that is otherwise
// invisible: a provider that is available and produced no unit at all.
// "Available, zero units" is not fresh coverage, and without a
// row for it status would report a capability nothing answers as healthy.
func (g *generation) coverage(ctx context.Context) (err error) {
	_, span := ledger.Start(ctx, stageCoverage, "")
	defer func() { span.End(endOutcome(err), ledger.Measured{CPUUnattributed: ledger.CPUOverlapped}, err) }()
	covered, failed, published, err := g.coveredProviders()
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
			if covered[d.ID] {
				g.caps.addFresh(d.ID, capability)
			} else {
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

// coveredProviders is the set of providers this generation was planned a
// member for -- a unit it ran or a unit it reused.
//
// published is the narrower question the partial state needs: the providers a
// member was actually written or attached for. Coverage counts a planned unit,
// because "available and produced no unit at all" is the degradation it exists
// to catch; a failure row may only be softened by facts that exist.
func (g *generation) coveredProviders() (covered map[string]bool, failed map[string]*providerFailures, published map[string]bool, err error) {
	out := make(map[string]bool, len(g.sel.Active))
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
	planned := make(map[string]int, len(g.sel.Active))
	if err := g.eachUnit(func(u plan.Unit) error {
		planned[u.ProviderID]++
		out[u.ProviderID] = true
		return nil
	}); err != nil {
		return nil, nil, nil, err
	}
	// A reused scope is a scope the plan assigned this provider that the walk
	// above never yields, because it is sealed already.
	g.plan.CountReused(func(id string) {
		out[id] = true
		published[id] = true
		planned[id]++
	})
	// The planned total is settled only here, once every source of a planned
	// scope has been folded in.
	for id, agg := range failed {
		agg.planned = planned[id]
	}
	return out, failed, published, nil
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
