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
// The cross-process workspace lock is held by the caller (internal/app) for
// the whole life of the coordinator and handed in through Options: capture,
// indexing, publication and retention are all one owner's work, and
// reacquiring it per stage would let a collector run between two of them.
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
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider"
	"github.com/Sawmonabo/codectx/internal/provider/dependence"
	"github.com/Sawmonabo/codectx/internal/provider/scip"
	tslang "github.com/Sawmonabo/codectx/internal/provider/treesitter/lang"
	"github.com/Sawmonabo/codectx/internal/snapshot"
	"github.com/Sawmonabo/codectx/internal/storage/sqlite"
	"github.com/Sawmonabo/codectx/internal/toolchain"
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

// Options are the coordinator's dependencies. Every field except Resolver,
// Logger and Now is required; the workspace lock is the caller's and is never
// closed here.
type Options struct {
	Root     workspace.Root
	Config   config.Config
	Store    *sqlite.Store
	Registry *provider.Registry
	CAS      *snapshot.CAS
	Git      *git.Git
	Lock     *snapshot.WorkspaceLock
	Pool     *provider.Pool
	// Resolver is carried for the status path's non-fetching installed-tool
	// projection, which is composed above this package; nothing on the
	// indexing path resolves tools, because providers are handed their own
	// resolved binaries at construction.
	Resolver *toolchain.Resolver
	Logger   *slog.Logger
	Now      func() time.Time
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
}

// New validates the dependencies and builds the coordinator. It creates the
// private work directory 0700 before anything can write to it.
func New(o Options) (*Coordinator, error) {
	switch {
	case o.Root.Path == "":
		return nil, invalid("the coordinator needs an opened workspace root")
	case o.Store == nil || o.CAS == nil || o.Registry == nil || o.Pool == nil:
		return nil, invalid("the coordinator needs a store, a CAS, a provider registry and a sink pool")
	case o.Lock == nil:
		return nil, invalid("the coordinator needs the workspace lock its caller holds")
	case !filepath.IsAbs(o.Config.Storage.DataDir):
		return nil, invalid("the coordinator needs an absolute data directory")
	case o.Root.HasGit && o.Git == nil:
		return nil, invalid("the workspace is a Git repository but no git executable is available")
	}
	limits := provider.Limits{BatchRecords: o.Config.Index.BatchRecords, BatchBytes: o.Config.Index.BatchBytes,
		MaxRecordBytes: o.Config.Resources.MaxProviderRecordBytes}
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
		sched:   plan.NewScheduler(o.Config.Resources.MaxConcurrentHeavy, dependence.ObserveMachine()),
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

// Watch runs the coordinator's own bounded reconciliation loop: every
// reconcile_interval it refreshes and hands the result to emit, until ctx
// ends. It is the periodic half of Section 13.2 and needs no notification
// support from the operating system, which is exactly what makes it the
// fallback when watch coverage is incomplete.
//
// The filesystem-notification half is composed by the caller: the watcher
// (internal/index/watch) hands its debounced, overflow-collapsing batches to
// Refresh, whose path list is a hint. Keeping the two apart is what lets the
// notification source be absent, degraded or replaced without the coordinator
// knowing: source and hash truth beat a notification either way.
func (c *Coordinator) Watch(ctx context.Context, emit func(model.IndexResult)) error {
	interval := c.opts.Config.Index.ReconcileInterval.Std()
	if interval <= 0 {
		return invalid("watch needs a positive reconcile interval")
	}
	c.watch.enter()
	defer c.watch.leave()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
		res, err := c.Refresh(ctx, nil)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			// A failed reconciliation is not the end of the watch: the prior
			// generation is still published and the next tick tries again.
			// Nothing here carries source or native keys.
			c.log.Warn("periodic reconciliation failed", "component", component,
				"repository_id", string(c.repo), "error", provider.CodeOf(err))
			continue
		}
		c.watch.reconciledAt(c.now())
		if emit != nil {
			emit(res)
		}
	}
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

func invalid(msg string) error { return &model.Error{Code: model.CodeArgumentInvalid, Message: msg} }

func internalErr(msg string) error { return &model.Error{Code: model.CodeInternal, Message: msg} }
