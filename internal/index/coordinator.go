// Package index is the indexing coordinator of Section 13: the one component
// that captures a snapshot, decides which units may be reused, runs the rest
// through the provider runtime, and publishes a generation atomically.
//
// The lifecycle of one run is fixed (Sections 12.3, 13.1):
//
//  1. capture the workspace into an immutable snapshot;
//  2. select the providers that can run and plan every unit over that
//     snapshot (internal/index/plan);
//  3. open a staging generation over the snapshot and the ref it was built
//     from;
//  4. attach the units the plan proved reusable, carry the stale predecessor
//     of every refreshing deferred scope, and build the rest -- SCIP and
//     dependence through their delta appliers (internal/index/delta), every
//     other provider through provider.RunUnit;
//  5. validate and activate with a compare-and-swap on the active pointer;
//  6. retain by ref.
//
// The active generation is never mutated. A failure aborts the staging
// generation and leaves the previously published one exactly as it was; a
// cancellation does the same and releases every reservation it took.
//
// The cross-process workspace lock comes from the caller (internal/app)
// through Options.Lock and is taken for the whole of one building operation:
// capture, indexing, publication and retention are all one owner's work, and
// reacquiring it per stage would let a collector run between two of them. It
// is given back when that operation ends -- a watch gives it back when it
// stops watching -- so a process that is idle between operations owns nothing
// and another process may index.
package index

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/Sawmonabo/codectx/internal/config"
	"github.com/Sawmonabo/codectx/internal/index/delta"
	"github.com/Sawmonabo/codectx/internal/index/plan"
	"github.com/Sawmonabo/codectx/internal/index/watch"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider"
	"github.com/Sawmonabo/codectx/internal/provider/dependence"
	"github.com/Sawmonabo/codectx/internal/provider/scip"
	tslang "github.com/Sawmonabo/codectx/internal/provider/treesitter/lang"
	"github.com/Sawmonabo/codectx/internal/snapshot"
	"github.com/Sawmonabo/codectx/internal/storage/sqlite"
	"github.com/Sawmonabo/codectx/internal/vcs/git"
	"github.com/Sawmonabo/codectx/internal/workspace"
)

// NormalizationVersion is the identity normalization contract this build
// publishes facts under. It folds into every AnalysisKey (Section 12.3), so it
// changes only when the normalization rules themselves change: a generation
// built under different rules must not share a key with one built under these.
// It is deliberately a compile-time constant and carries nothing operational.
const NormalizationVersion = "identity-normalization-v1"

// component is the slog component of every entry this package emits.
const component = "index"

// refNone is the generation ref of a workspace that is not a Git repository
// (ruling Q3). Storage refuses an empty ref rather than grouping every such
// generation under one nameless retention bucket, so the sentinel is explicit.
const refNone = "(none)"

// domainRepository separates the repository identity digest from every other
// digest in the product, so one can never be presented as another.
const domainRepository = "repository-identity-v1"

// maxWorkers bounds the units one generation builds concurrently when the
// configuration asks for the machine default. Each concurrent unit holds a
// live sink and an open writer, so the bound is a resource decision and not a
// throughput guess; provider.MaxLiveSinks is the structural ceiling above it.
const maxWorkers = 8

// Locker is where a coordinator gets the cross-process workspace lock of
// Sections 12.3 and 13.2.
//
// Hold takes the lock for the operation that is about to build and returns the
// release that gives it back; release runs exactly once, on every path,
// including the one where the operation failed partway. Holds NEST: the
// composition root hands out one lock for the process and releases it when the
// last holder has, so a capture inside a refresh, or a refresh inside a watch,
// takes no second lock and cannot release one another still needs.
//
// A workspace another process is indexing is reported as the typed, retryable
// CTX_WORKSPACE_BUSY snapshot.LockWorkspace produces, and nothing is held.
type Locker interface {
	Hold(ctx context.Context) (*snapshot.WorkspaceLock, func() error, error)
}

// Options are the coordinator's dependencies. Every field except Lock,
// Watcher, States, Logger and Now is required; the workspace lock is the
// caller's and is never closed here.
type Options struct {
	Root     workspace.Root
	Config   config.Config
	Store    *sqlite.Store
	Registry *provider.Registry
	CAS      *snapshot.CAS
	Git      *git.Git
	// Lock is where the cross-process workspace lock comes from. It may be nil
	// for a report-only coordinator: Status is legal without it, because
	// Section 12.3 makes the active generation immutable once published and a
	// report only reads it. Index, Refresh, Watch, Promote and Drain refuse
	// with a typed CTX_ARGUMENT_INVALID: they capture, build and publish, and
	// Section 13.2 gives that to exactly one cross-process owner.
	//
	// It is a source and not the lock itself because a composition may hold
	// the lock from its open (an indexing command, whose whole life is the
	// run) or take it at the first build (the server, which must come up and
	// answer beside an index another process is already running). The
	// coordinator asks for it where it is about to build and never closes it:
	// it is the composition root's.
	Lock Locker
	// Collector is the process-level reclaim pass, scheduled from the same
	// post-activation points as retention-by-ref because that is the one moment
	// this process holds both locks the pass requires. It may be nil; see the
	// Collector interface in retention.go.
	Collector Collector
	Pool      *provider.Pool
	// Watcher, when non-nil, is the notification source Watch drives: its
	// debounced batches become refreshes and its Coverage() is what status
	// reports. nil keeps the periodic-only behaviour, whose coverage is
	// reported incomplete because that is what it is.
	Watcher *watch.Watcher
	// States are the composition-time capability rows of providers that could
	// not be constructed at all -- an absent analyzer payload, a profile that
	// did not resolve. They are folded into every capability report
	// (IndexResult.Completeness, IndexStatus.Completeness) before the
	// MaxCapabilityStates bound is applied, so a degradation the operator must
	// see is never the row that truncation drops and never pushes the
	// published list past its own contract.
	States []model.CapabilityState
	// SuppliedIndexes are the already-built indexes this run was handed --
	// today the `--scip-index` path. The coordinator records each one against
	// every generation it publishes, whether or not the path resolved, because
	// an unresolved path plans no unit and leaves nothing else behind: without
	// the record, a typo and a run that supplied no index at all are the same
	// observation, and the first ships a repository with no imported symbols.
	SuppliedIndexes []SuppliedIndex
	Logger          *slog.Logger
	Now             func() time.Time
}

// SuppliedIndex is one supplied index as the composition root resolved it: the
// root-relative path the user named, plus the provider and scope key that path
// becomes if it is importable.
//
// The provider and scope key are supplied rather than derived here because the
// scope-key spelling belongs to the provider that reads it, and internal/index
// must not import one provider to spell another's scope.
type SuppliedIndex struct {
	Path       string
	ProviderID string
	ScopeKey   string
}

// Pending is the typed answer a query gets for a capability whose dependence
// units have not sealed yet (Section 11.6): how many units it waits on, where
// the promoted scope now sits in the background queue, and how long the
// remaining units are expected to take. Estimate is zero while no deferred
// unit of this process has completed: an unmeasured duration is reported as
// unmeasured, never as an invented number.
type Pending struct {
	Units    int
	Position int
	Estimate time.Duration
}

// Coordinator owns one workspace's indexing. Index, Refresh and Status are
// safe for concurrent use; the indexing runs themselves are serialized, which
// is what makes a concurrent refresh a queued second pass rather than a second
// writer (Section 13.2).
type Coordinator struct {
	opts   Options
	repo   model.RepositoryID
	policy workspace.Policy
	limits provider.Limits
	log    *slog.Logger
	now    func() time.Time

	// cfgHash is config.AnalysisConfigHash, which completes every unit
	// identity and is also the generation's semantic config hash.
	cfgHash string
	// workDir is the private scratch directory the delta appliers build in.
	workDir string
	// workers is how many units of one provider are built at once.
	workers int

	sched    *plan.Scheduler
	appliers map[string]delta.Applier

	// run serializes indexing runs in this process. The cross-process half is
	// the workspace lock the caller holds.
	run sync.Mutex

	// late owns the deferred dependence units of Section 11.6.
	late *lateSealer
	// watch is what this coordinator's own reconciliation loop knows about
	// watch coverage; Status projects it.
	watch watchState
	// retention is what the last retention sweep did; Status projects it, so a
	// sweep that never finishes is a reported degradation and not only a log
	// line (retentionState in retention.go).
	retention retentionState
}

// New validates the dependencies and builds the coordinator. It creates the
// private work directory 0700 before anything can write to it.
func New(o Options) (*Coordinator, error) {
	switch {
	case o.Root.Path == "":
		return nil, invalid("the coordinator needs an opened workspace root")
	case o.Store == nil || o.CAS == nil || o.Registry == nil || o.Pool == nil:
		return nil, invalid("the coordinator needs a store, a CAS, a provider registry and a sink pool")
	case !filepath.IsAbs(o.Config.Storage.DataDir):
		return nil, invalid("the coordinator needs an absolute data directory")
	case o.Root.HasGit && o.Git == nil:
		return nil, invalid("the workspace is a Git repository but no git executable is available")
	}
	limits := provider.Limits{BatchRecords: o.Config.Index.BatchRecords, BatchBytes: o.Config.Index.BatchBytes,
		MaxRecordBytes: o.Config.Resources.MaxProviderRecordBytes.Value()}
	if err := limits.Validate(); err != nil {
		return nil, err
	}
	workDir := filepath.Join(o.Config.Storage.DataDir, "work", "index")
	if err := os.MkdirAll(workDir, 0o700); err != nil {
		return nil, internalErr("index work directory: " + err.Error())
	}
	c := &Coordinator{opts: o, repo: model.RepositoryID(model.H(domainRepository, filepath.ToSlash(o.Root.Path))),
		policy: o.Config.TraversalPolicy(), limits: limits, log: o.Logger, now: o.Now,
		cfgHash: o.Config.AnalysisConfigHash(), workDir: workDir,
		workers: workerCount(o.Config.Index.Workers),
		sched:   plan.NewScheduler(dependence.ObserveMachine()),
	}
	if c.log == nil {
		c.log = slog.Default()
	}
	if c.now == nil {
		c.now = time.Now
	}
	appliers, err := c.buildAppliers()
	if err != nil {
		return nil, err
	}
	c.appliers = appliers
	c.late = newLateSealer(c)
	return c, nil
}

// buildAppliers binds the delta appliers of the two providers that can
// describe what changed since their last run. A provider the registry does not
// hold simply has no applier: its units are then never planned either.
//
// A registered provider of an unexpected concrete type is a composition error
// and fails construction. Falling back to a full run would be a silent
// capability reduction: the unit would seal without delta state, and every
// later refresh would rebuild it whole with no diagnostic saying why.
func (c *Coordinator) buildAppliers() (map[string]delta.Applier, error) {
	out := map[string]delta.Applier{}
	if p, ok := c.opts.Registry.Lookup(scip.ID); ok {
		sp, ok := p.(*scip.Provider)
		if !ok {
			return nil, internalErr("the registered " + scip.ID + " provider cannot apply its own deltas")
		}
		out[scip.ID] = delta.NewSCIP(c.opts.Store, sp, c.limits, c.opts.Pool)
	}
	if p, ok := c.opts.Registry.Lookup(dependence.ProviderID); ok {
		dp, ok := p.(*dependence.Provider)
		if !ok {
			return nil, internalErr("the registered " + dependence.ProviderID + " provider cannot apply its own deltas")
		}
		out[dependence.ProviderID] = delta.NewDependence(c.opts.Store, dp, c.limits, c.opts.Pool)
	}
	return out, nil
}

// workerCount resolves index.workers. Zero selects from the available CPUs
// under a fixed ceiling; it is never unlimited (Section 20.1).
func workerCount(configured int) int {
	if configured > 0 {
		return min(configured, provider.MaxLiveSinks)
	}
	return max(1, min(runtime.NumCPU(), maxWorkers))
}

// buildable refuses an entry point that captures, builds or publishes when the
// coordinator was composed without a source for the cross-process workspace
// lock. A report-only coordinator is a legal composition (Section 13.2 gives
// indexing to one owner, and a report is not indexing), so the refusal belongs
// at each building method rather than in New, where it would also forbid
// Status.
func (c *Coordinator) buildable() error {
	if c.opts.Lock == nil {
		return invalid("this coordinator was opened without the workspace indexing lock")
	}
	return nil
}

// hold is buildable plus the lock itself, for the duration of ONE operation:
// the caller releases it when that operation ends, so an idle session owns
// nothing and the person's own `codectx index` is not refused for as long as
// a server happens to be running. A workspace another process is building in
// answers the typed, retryable CTX_WORKSPACE_BUSY of snapshot.LockWorkspace.
//
// Every entry point that builds calls this, and Watch deliberately does not at
// its entry: a session that watches must survive a workspace that is busy
// right now, so its patience is the reconcile interval it already has -- each
// pass asks again, and a pass that cannot have the lock is logged and retried
// rather than ending the session. A watch that HAS taken it keeps it for as
// long as it watches, which is the one operation whose duration is the
// session's (watchHold below).
func (c *Coordinator) hold(ctx context.Context) (*snapshot.WorkspaceLock, func() error, error) {
	if err := c.buildable(); err != nil {
		return nil, nil, err
	}
	return c.opts.Lock.Hold(ctx)
}

// watchHold makes this watch the workspace's owner for as long as it watches.
// It is taken at the first pass rather than at Watch's entry, and a pass that
// cannot have it leaves the watch running: the workspace is busy now, and the
// next pass asks again.
//
// A watch holds across passes rather than per pass because it IS the session
// that is keeping the index fresh -- the one case where another process's
// index would be doing the same work -- and because releasing between passes
// would hand the workspace away in every gap it has.
func (c *Coordinator) watchHold(ctx context.Context) error {
	c.watch.mu.Lock()
	held := c.watch.release != nil
	c.watch.mu.Unlock()
	if held {
		return nil
	}
	_, release, err := c.hold(ctx)
	if err != nil {
		return err
	}
	c.watch.mu.Lock()
	if c.watch.release != nil {
		// A concurrent pass got there first. Give this one back at once:
		// one watch session holds one reference, never two.
		c.watch.mu.Unlock()
		return release()
	}
	c.watch.release = release
	c.watch.mu.Unlock()
	return nil
}

// watchReleased gives back whatever watchHold took. It runs when the watch
// loop returns, by whichever path, and is a no-op for a watch that never got
// the lock at all.
func (c *Coordinator) watchReleased() error {
	c.watch.mu.Lock()
	release := c.watch.release
	c.watch.release = nil
	c.watch.mu.Unlock()
	if release == nil {
		return nil
	}
	return release()
}

// Repository is the identity this coordinator derived for the workspace root.
// It is the key every generation, lease and pinned read is scoped by, and it is
// exposed rather than re-derived by callers so the derivation has exactly one
// spelling in the process.
func (c *Coordinator) Repository() model.RepositoryID { return c.repo }

// Close stops the background deferred work. It does not release the workspace
// lock, the store or the providers: those belong to the composition root,
// which closes them in reverse after this returns.
func (c *Coordinator) Close() error {
	c.late.close()
	return nil
}

// Index builds and publishes one generation over a fresh capture.
//
// Full plans every unit from scratch: no previous generation is consulted, so
// nothing is reused or carried. Rebuild implies Full here; replacing the cache
// directory itself is the composition root's, because a coordinator that
// deleted the store it was handed would be destroying state its caller still
// owns.
func (c *Coordinator) Index(ctx context.Context, req model.IndexRequest) (model.IndexResult, error) {
	_, release, err := c.hold(ctx)
	if err != nil {
		return model.IndexResult{}, err
	}
	defer release()
	if err := req.Validate(); err != nil {
		return model.IndexResult{}, err
	}
	c.run.Lock()
	defer c.run.Unlock()
	return c.index(ctx, req)
}

// Refresh re-indexes after a change. paths is a hint and nothing more: the
// capture re-reads the workspace and the plan compares content hashes, so a
// notification that named the wrong file, or named none at all, changes what
// this run costs and never what it concludes (Section 13.2).
func (c *Coordinator) Refresh(ctx context.Context, paths []string) (model.IndexResult, error) {
	_, release, err := c.hold(ctx)
	if err != nil {
		return model.IndexResult{}, err
	}
	defer release()
	// The hint is recorded and deliberately not acted on: narrowing the
	// capture to it is exactly the mistake Section 13.2 names, because a
	// notification can miss a timestamp-preserving write and Git status alone
	// is not the truth for an untracked file.
	c.log.Debug("refresh requested", "component", component, "repository_id", string(c.repo),
		"hinted_paths", len(paths))
	c.run.Lock()
	defer c.run.Unlock()
	return c.index(ctx, model.IndexRequest{})
}

// Watch reconciles this workspace until ctx ends, handing every published
// result to emit (Section 13.2).
//
// With Options.Watcher set it is the notification half: the watcher's
// debounced, overflow-collapsing batches become refreshes, the watcher's own
// reconcile interval is the periodic fallback, and Coverage() is what status
// reports. A batch's path list is a hint and nothing more -- Refresh
// re-captures and compares content hashes -- so an overflow batch, which
// carries no paths, costs a full capture and never a dropped change.
//
// Without a watcher it is the periodic half alone: every reconcile_interval it
// refreshes. That needs no notification support from the operating system,
// which is exactly what makes it the fallback, and its coverage is reported
// incomplete because periodic reconciliation is not notification coverage.
func (c *Coordinator) Watch(ctx context.Context, emit func(model.IndexResult)) error {
	if err := c.buildable(); err != nil {
		return err
	}
	interval := c.opts.Config.Index.ReconcileInterval.Std()
	if interval <= 0 {
		return invalid("watch needs a positive reconcile interval")
	}
	c.watch.enter(c.opts.Watcher)
	defer c.watch.leave()
	// The workspace this watch owns while it watches, taken at its first pass
	// and given back here however the loop ends.
	defer func() {
		if err := c.watchReleased(); err != nil {
			logTyped(c.log, "the workspace lock this watch held was not released cleanly", err,
				"component", component, "repository_id", string(c.repo))
		}
	}()
	// The heartbeat row names this repository, and on a workspace nothing has
	// ever indexed there is no repository row for it to name: every beat would
	// be refused by the foreign key and logged, and a second process asking
	// whether a watch covers this workspace would be told nothing does. The
	// identity is recorded here, before the first beat, by the same call the
	// build path makes -- a watch IS the owner of this workspace for as long as
	// it runs, and it has the lock to say so.
	if err := c.opts.Store.EnsureRepository(ctx, c.repo, c.opts.Root.Path); err != nil {
		return err
	}
	// Evaluated now and deferred as its result: the beat starts here and the
	// withdrawal it returns runs when this watch ends.
	defer c.beatHeartbeat(ctx)()
	if c.opts.Watcher != nil {
		return c.watchNotified(ctx, emit)
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
		if _, ok := c.reconcile(ctx, nil, emit); !ok && ctx.Err() != nil {
			return nil
		}
	}
}

// watchNotified drives the notification watcher. Batches are delivered
// synchronously, so events that arrive during a refresh accumulate into the
// next batch rather than starting a second one.
func (c *Coordinator) watchNotified(ctx context.Context, emit func(model.IndexResult)) error {
	err := c.opts.Watcher.Run(ctx, func(b watch.Batch) error {
		// An overflow batch is a full-reconciliation request: its Paths are
		// empty by construction and the hint is dropped entirely.
		var paths []string
		if !b.Overflow {
			paths = b.Paths
		}
		if _, ok := c.reconcile(ctx, paths, emit); !ok && ctx.Err() != nil {
			return ctx.Err()
		}
		return nil
	})
	if ctx.Err() != nil {
		return nil
	}
	return err
}

// watchHeartbeatInterval is how often a running watch republishes its row, and
// watchHeartbeatTTL is the deadline it writes into that row. The TTL is three
// intervals, so two lost or slow writes do not make a live watch read as dead.
//
// Both are fixed here rather than derived from reconcile_interval, because the
// row must stay refreshed on a workspace that reconciles hourly, and because
// deriving the window in the reader would make liveness depend on the reader's
// configuration matching the writer's. They are deliberately short: the window
// in which a watch that died without clearing its row still reads as live is
// exactly the TTL, and nothing else shortens it.
const (
	watchHeartbeatInterval = 10 * time.Second
	watchHeartbeatTTL      = 30 * time.Second
)

// beatHeartbeat publishes this watch's heartbeat and keeps republishing it
// until the returned stop function runs, which withdraws the row.
//
// Withdrawing on the way out is what makes a deliberate `Ctrl-C` immediate: a
// row left behind is fresh for a further TTL and would report a watch that is
// no longer running as live coverage. Expiry remains the answer for a process
// that died without reaching here, which is why the writer states a deadline at
// all -- no pid probe is portable or race-free enough to be the death signal.
func (c *Coordinator) beatHeartbeat(ctx context.Context) func() {
	c.publishHeartbeat(ctx)
	done := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		t := time.NewTicker(watchHeartbeatInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-done:
				return
			case <-t.C:
				c.publishHeartbeat(ctx)
			}
		}
	}()
	return func() {
		close(done)
		<-stopped
		// ctx is already cancelled on the ordinary exit path, so the
		// withdrawal gets a fresh deadline of its own; without one the delete
		// would be refused by the very cancellation it is reacting to.
		clearCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), watchHeartbeatInterval)
		defer cancel()
		if err := c.opts.Store.ClearWatchHeartbeat(clearCtx, c.repo); err != nil {
			logTyped(c.log, "the watch heartbeat could not be withdrawn; another process will report this watch as running until it expires",
				err, "component", component, "repository_id", string(c.repo))
		}
	}
}

// publishHeartbeat writes one heartbeat row. A failure is logged and never ends
// the watch: the watch itself is still reconciling this workspace, and the row
// is how another process reports on it. The row then expires, so a reader is
// told the coverage is unknown rather than shown a figure that stopped moving.
//
// What the row claims is watchState.heartbeat's rule: a watch that has completed
// no pass -- one still waiting for the workspace another process holds --
// publishes its presence and no figures, so a reader tells it apart from a watch
// that is covering this workspace and from one that never ran.
func (c *Coordinator) publishHeartbeat(ctx context.Context) {
	lastPass, pending := c.watch.heartbeat()
	err := c.opts.Store.RecordWatchHeartbeat(ctx, c.repo, sqlite.WatchHeartbeat{
		WriterPID:     os.Getpid(),
		LastPassAt:    lastPass,
		PendingEvents: pending,
		ExpiresAt:     c.now().Add(watchHeartbeatTTL),
	})
	if err != nil && ctx.Err() == nil {
		logTyped(c.log, "the watch heartbeat could not be published; another process cannot report on this watch",
			err, "component", component, "repository_id", string(c.repo))
	}
}

// reconcile runs one watch-driven refresh. A failed reconciliation is not the
// end of the watch: the prior generation is still published and the next batch
// or tick tries again.
func (c *Coordinator) reconcile(ctx context.Context, paths []string, emit func(model.IndexResult)) (model.IndexResult, bool) {
	// The lock first, and kept for the rest of the watch. A workspace another
	// process is building in is not this pass's failure: the pass is skipped,
	// the next one asks again, and the session keeps running.
	if err := c.watchHold(ctx); err != nil {
		if ctx.Err() == nil {
			logTyped(c.log, "this pass could not take the workspace; another process holds it and the next pass will ask again", err,
				"component", component, "repository_id", string(c.repo))
		}
		return model.IndexResult{}, false
	}
	res, err := c.Refresh(ctx, paths)
	if err != nil {
		if ctx.Err() == nil {
			logTyped(c.log, "periodic reconciliation failed", err,
				"component", component, "repository_id", string(c.repo))
		}
		return model.IndexResult{}, false
	}
	c.watch.reconciledAt(c.now())
	// Republished on the pass rather than only on the timer: the pass is what
	// changes the two figures the row carries, and a reader that arrives right
	// after a burst of edits must see the pending count that burst produced,
	// not the one from up to a heartbeat interval ago.
	c.publishHeartbeat(ctx)
	if emit != nil {
		emit(res)
	}
	return res, true
}

// ref is the ref this generation is built from (ruling Q3): the branch when
// HEAD is symbolic, the HEAD object id when it is detached, and the fixed
// sentinel when the workspace is not a Git repository. Retention groups by it,
// so two detached commits are two refs.
func (c *Coordinator) ref(ctx context.Context) (string, error) {
	if !c.opts.Root.HasGit || c.opts.Git == nil {
		return refNone, nil
	}
	name, err := c.opts.Git.SymbolicRef(ctx, c.opts.Root.Path)
	if err != nil {
		return "", err
	}
	if name != "" {
		return name, nil
	}
	head, err := c.opts.Git.Head(ctx, c.opts.Root.Path)
	if err != nil {
		return "", err
	}
	if head == "" {
		return refNone, nil
	}
	return head, nil
}

// carriedPage adapts sqlite.Store.CarriedUnits to the planner's page fetcher.
// The limit is passed through verbatim: the planner ends its fold on a page
// shorter than the one it asked for, so capping it here would truncate the
// fold silently and every scope past the first page would read as never
// carried.
func (c *Coordinator) carriedPage(gen model.GenerationID) func(context.Context, string, string, int) ([]plan.Carried, error) {
	return func(ctx context.Context, afterProviderID, afterScopeKey string, limit int) ([]plan.Carried, error) {
		page, err := c.opts.Store.CarriedUnits(ctx, gen, afterProviderID, afterScopeKey, limit)
		if err != nil {
			return nil, err
		}
		out := make([]plan.Carried, 0, len(page))
		for _, cu := range page {
			out = append(out, plan.Carried{Unit: cu.Unit, ProviderID: cu.ProviderID, ScopeKey: cu.ScopeKey,
				DistanceGenerations: cu.Carry.DistanceGenerations, DistanceFiles: cu.Carry.DistanceFiles})
		}
		return out, nil
	}
}

// enablement is what Registry.Select asks about each provider. A provider the
// configuration does not name is enabled, which is what a required base
// provider is. The LSP overlay is not a registry provider -- it answers live
// queries rather than sealing units -- so it has no row here.
func (c *Coordinator) enablement(providerID string) config.Enablement {
	p := c.opts.Config.Providers
	switch providerID {
	case scip.ID:
		return p.SCIP.Enabled
	case dependence.ProviderID:
		return p.Dependence.Enabled
	case tslang.ProviderID:
		if !p.TreeSitter.Enabled {
			return config.Disabled
		}
	}
	return config.Enabled
}

// activeGeneration reads the published generation, answering zero when nothing
// has been published yet. A repository with no active generation is the
// ordinary first-run state, not an error.
func (c *Coordinator) activeGeneration(ctx context.Context) (model.GenerationID, error) {
	gen, err := c.opts.Store.ActiveGeneration(ctx, c.repo)
	if err != nil {
		var typed *model.Error
		if errors.As(err, &typed) && typed.Code == model.CodeNoActiveGeneration {
			return 0, nil
		}
		return 0, err
	}
	return gen, nil
}

// logTyped logs a failure with its typed diagnostic beside its code. The code
// alone does not distinguish, say, a pinned generation that is gone from a
// capability row that failed validation, and these lines are the only record
// of a background failure an operator ever sees. model.Error.Message and
// Remediation are product-authored text: they carry no source bytes, no
// secrets, no environment and no raw analyzer output, which is what Section
// 20.1 keeps out of ordinary logs.
func logTyped(log *slog.Logger, msg string, err error, args ...any) {
	var typed *model.Error
	if errors.As(err, &typed) {
		log.Warn(msg, append(args, "diagnostic_code", typed.Code, "diagnostic", typed.Message,
			"remediation", typed.Remediation)...)
		return
	}
	log.Warn(msg, append(args, "diagnostic_code", provider.CodeOf(err))...)
}

func invalid(msg string) error { return &model.Error{Code: model.CodeArgumentInvalid, Message: msg} }

func internalErr(msg string) error { return &model.Error{Code: model.CodeInternal, Message: msg} }
