package app

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/Sawmonabo/codectx/internal/config"
	contextpkg "github.com/Sawmonabo/codectx/internal/context"
	"github.com/Sawmonabo/codectx/internal/coverage"
	"github.com/Sawmonabo/codectx/internal/index/watch"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
	"github.com/Sawmonabo/codectx/internal/process"
	"github.com/Sawmonabo/codectx/internal/provider"
	"github.com/Sawmonabo/codectx/internal/provider/dependence"
	"github.com/Sawmonabo/codectx/internal/provider/dependence/joern"
	"github.com/Sawmonabo/codectx/internal/provider/filesystem"
	"github.com/Sawmonabo/codectx/internal/provider/lsp"
	"github.com/Sawmonabo/codectx/internal/provider/manifest"
	"github.com/Sawmonabo/codectx/internal/provider/scip"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/wire"
	"github.com/Sawmonabo/codectx/internal/search"
	"github.com/Sawmonabo/codectx/internal/snapshot"
	"github.com/Sawmonabo/codectx/internal/storage/sqlite"
	"github.com/Sawmonabo/codectx/internal/toolchain"
	"github.com/Sawmonabo/codectx/internal/vcs/git"
	"github.com/Sawmonabo/codectx/internal/workflow"
	"github.com/Sawmonabo/codectx/internal/workspace"
)

// databaseName is the workspace database under the data directory.
const databaseName = "codectx.db"

// Private working directories under the data directory. Each is created 0700
// before the component that owns it is constructed, because a provider that
// creates its own scratch directory on first use creates it with whatever
// umask the operator happens to have (Section 21: user-private directories).
const (
	workersDirName = "workers"
	workDirName    = "work"
	spoolsDirName  = "spools"
)

// spoolBudgetDivisor is what the shared temporary-file budget is divided by to
// size the query spools. resources.max_temp_bytes is not a per-consumer budget:
// the same key is already the whole disk budget of both process runners
// (compose.go, the shared and parser runners below), so handing the spools the
// undivided key would let three independent consumers each believe they own it.
// A query's spools are the smallest of the three claims -- one bounded page of
// ranked records per live cursor -- so they take the smallest share.
const spoolBudgetDivisor = 8

// parserWorkerReservationBytes is what one tree-sitter worker is admitted
// against, and therefore what the parser runner's budget is sized from. It is
// the provider's own default restated here so one number sizes both sides
// rather than a budget guessing at a default it cannot see.
//
// It is known to over-reserve by roughly seven times against measured worker
// resident memory (ledger 113); re-pinning it is Task 20's measurement, and
// over-reserving is the safe direction for an admission bound.
const parserWorkerReservationBytes int64 = 256 << 20

// sharedRunnerHeadroom is how many children beyond the heavy-analyzer budget
// the shared runner admits: Git plumbing and a SCIP indexer run alongside a
// heavy analyzer, and at `max_concurrent_heavy_analyzers = 1` a single shared
// slot would let one graph engine run starve every Git command for the length
// of a whole unit. The tree-sitter workers and the language servers do not
// count here at all: each has its own runner for the same reason (ledger 111,
// 126).
const sharedRunnerHeadroom = 2

// unobservedChildMemoryBudget is the shared runner's memory budget on a host
// that does not report available memory. It is not a ceiling on a child: the
// runner's budget is admission accounting, and a reservation larger than it is
// refused before the process starts. It is sized above the largest fixed
// reservation the pinned analyzer profiles issue, so on an unmeasurable host
// every profile can still start; where the host does report memory, the
// machine-derived allocation replaces it and a run larger than the machine is
// refused, which is the Section 23.3 admission discipline.
const unobservedChildMemoryBudget int64 = 8 << 30

// openMode selects what the composition takes and what it may write.
type openMode uint8

const (
	// modeIndex composes for a run that will index: it takes the cross-process
	// workspace lock for the whole session and runs startup recovery under it.
	modeIndex openMode = iota
	// modeReport composes for a read-only report. It takes no lock, mutates no
	// generation and installs nothing -- it still creates the cache's own
	// directories, which is what opening a store means -- so `codectx status`
	// answers while a `watch` session holds the workspace instead of being
	// refused for a lock it does not need: Section 12.3 makes the active
	// generation immutable once published, and a report reads only that
	// (ledger 159 keeps the same path from installing anything, which the
	// providers now honour by construction).
	modeReport
)

// openOptions are the composition's variable inputs. They are one struct
// because two of them -- the report mode and the rebuild cache -- both change
// what openStack opens, and two positional parameters that must agree read
// worse than one value that carries the agreement.
type openOptions struct {
	mode openMode
	// wait is how long modeIndex waits for the workspace lock. modeReport
	// takes no lock and ignores it.
	wait time.Duration
	// rebuild opens a sibling cache instead of the configured one, which is
	// what `index --rebuild` means in Section 12.2: an explicitly requested new
	// cache, with the existing database left exactly as it was.
	rebuild bool
}

// stack is everything a workspace owns below the coordinator. It exists apart
// from Workspace so the composition -- configuration, directories, the lock,
// the store, the toolchain, the runners, the providers and the registry -- is
// one reviewable unit that depends on no coordinator.
type stack struct {
	root     workspace.Root
	cfg      config.Config
	dataDir  string
	store    *sqlite.Store
	cas      *snapshot.CAS
	registry *provider.Registry
	pool     *provider.Pool
	git      *git.Git
	lock     *snapshot.WorkspaceLock
	resolver *toolchain.Resolver
	toolDir  string
	lsp      *lsp.Manager
	ts       *treesitter.Provider
	watcher  *watch.Watcher
	logger   *slog.Logger

	// signer signs the pagination cursors every paged query hands back, and
	// spools hold the ranked pages a keyset cursor cannot re-derive. Both are
	// process-wide and are built once here because the search service and every
	// graph engine share them.
	signer *pagination.Signer
	spools *pagination.Spools
	// leases mints the cursor-scoped retention leases graph continuations
	// name. It is built once per stack rather than per request so every cursor
	// in the process retains its generation for the same configured window.
	leases *pagination.Leases
	// gate is the process-scoped max_concurrent_graph_queries semaphore. One
	// graph engine is built per request, so the bound cannot live on the engine.
	gate *graphGate
	// repo, search and coverage are set by openQueries once the coordinator has
	// resolved the repository identity, which is the coordinator's to derive.
	repo     model.RepositoryID
	search   *search.Service
	coverage *coverage.Service
	// workflow is the Section 17 guard, review and capsule service the facade
	// routes every session mutation through. INT wires it.
	workflow *workflow.Service

	// views memoises the per-snapshot read view the coverage service opens
	// through its SourceOpener. A view is pinned to one immutable snapshot, so
	// one per snapshot is correct for the life of the workspace; the mutex is
	// what makes the opener safe to call from concurrent requests, which
	// coverage.Service promises its callers.
	viewsMu sync.Mutex
	views   map[model.SnapshotID]*snapshot.View
	// snapshots memoises the immutable generation-to-snapshot binding the
	// workflow validator resolves. It shares viewsMu because it is the same
	// kind of fact -- a published binding that cannot change -- and is filled
	// on the same path.
	snapshots map[model.GenerationID]model.SnapshotID
	// compiler is the Section 15 context compiler. It is set by openCompiler
	// rather than openQueries because it needs the graph factory, which is a
	// Workspace method and therefore does not exist until the workspace does.
	compiler *contextpkg.Compiler

	// states are the capability rows detection can never publish because the
	// provider could not be constructed at all. A provider missing from the
	// registry is a capability that vanishes silently; these rows are what the
	// coordinator folds in so it does not (Section 13.3).
	states []model.CapabilityState
}

// openStack composes the workspace. The order is load-bearing: the executable
// identity is resolved first because the parser workers are this same binary
// and a composition that cannot name itself cannot parse anything; the private
// directories exist before anything opens a file inside them; the cross-process
// lock is taken before the database, so startup recovery runs as the single
// owner (Sections 12.3, 13.2).
func openStack(ctx context.Context, repo string, o openOptions) (s *stack, err error) {
	// os.Executable is resolved once, here, and a failure is fatal: every
	// later reader of this path would otherwise silently fall back to argv[0],
	// which a caller controls (ledger 113, 126).
	exe, err := os.Executable()
	if err != nil {
		return nil, &model.Error{Code: model.CodeInternal,
			Message: "this binary's own path cannot be determined: " + err.Error()}
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return nil, &model.Error{Code: model.CodeInternal,
			Message: "this binary's own path cannot be resolved: " + err.Error()}
	}

	cfg, err := config.Load(repo)
	if err != nil {
		return nil, err
	}
	root, err := workspace.Discover(repo)
	if err != nil {
		return nil, err
	}
	// The tool store is derived from the configured data directory and must
	// stay there whatever cache this run writes: a rebuild that moved it would
	// re-fetch every managed payload, which for the analysis engine alone is
	// roughly two gigabytes for a flag that is about the database.
	toolCfg := cfg
	if o.rebuild {
		if cfg.Storage.DataDir, err = makeRebuildDir(cfg.Storage.DataDir, time.Now()); err != nil {
			return nil, err
		}
	}
	// cfg.Storage.DataDir is the effective cache from here on, including in
	// Config.TraversalPolicy, which excludes it from the walk: a rebuild cache
	// inside the workspace must be excluded as the configured one is.
	st := &stack{root: root, cfg: cfg, dataDir: cfg.Storage.DataDir,
		logger: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))}
	// Components whose shared signatures carry no logger (the traversal
	// policy, for one) log through the package default; make it this one so
	// every line the process emits is formatted the same way.
	slog.SetDefault(st.logger)
	s = st
	// Every later step opens something; from here a failure must release what
	// has been opened so far, in reverse. The deferred close names the local
	// rather than the named result, because every error path below returns
	// `nil, err` and would otherwise disarm its own cleanup.
	defer func() {
		if err != nil {
			st.Close()
			s = nil
		}
	}()

	tsWorkDir := filepath.Join(s.dataDir, workersDirName, "treesitter")
	scipWorkDir := filepath.Join(s.dataDir, workDirName, "scip")
	for _, dir := range []string{s.dataDir, tsWorkDir, scipWorkDir} {
		if err = os.MkdirAll(dir, 0o700); err != nil {
			return nil, &model.Error{Code: model.CodeInternal,
				Message: "the private data directory cannot be created: " + err.Error()}
		}
	}

	// Section 13.2's single cross-process owner governs indexing. A report
	// publishes nothing, so it takes no lock: the writer lock on this path made
	// `codectx status` permanently refused while a `watch` session ran, with a
	// remediation ("wait for the running process to finish") that a watch never
	// satisfies. internal/index refuses its building entry points when Lock is
	// nil, so the absence is enforced there rather than trusted here.
	if o.mode == modeIndex {
		if s.lock, err = snapshot.LockWorkspace(ctx, s.dataDir, o.wait); err != nil {
			return nil, err
		}
	}
	if s.store, err = sqlite.Open(ctx, filepath.Join(s.dataDir, databaseName), sqlite.Options{
		BusyTimeout:       cfg.Storage.BusyTimeout.Std(),
		ReadConnections:   cfg.Storage.ReadConnections,
		WriterCacheKiB:    cfg.Storage.WriterCacheKiB,
		ReaderCacheKiB:    cfg.Storage.ReaderCacheKiB,
		WALHighWaterBytes: cfg.Storage.WALHighWaterBytes,
		BatchRecords:      cfg.Index.BatchRecords,
		BatchBytes:        cfg.Index.BatchBytes,
		MaxJSONBytes:      cfg.Context.MaxManifestBytes,
	}); err != nil {
		return nil, err
	}
	// Recovery runs under the lock and before anything reads a generation, so
	// a staging generation an earlier crash abandoned is failed and collected
	// rather than inherited. It is a write -- Abort on every staging generation
	// and then a sweep -- so a report, which holds no lock, must not run it:
	// doing so would abort the staging generation of a concurrently running
	// index. Skipping it costs a report nothing, because Store.Recover never
	// touches the active pointer and Coordinator.Status reads only the active
	// generation.
	if o.mode == modeIndex {
		if err = s.store.Recover(ctx, time.Now()); err != nil {
			return nil, err
		}
	}
	if s.cas, err = snapshot.OpenCAS(snapshot.CASDir(s.dataDir)); err != nil {
		return nil, err
	}

	// The cursor key and the query spools are workspace-private state under the
	// cache this run actually opened, so a rebuild cache signs with its own key
	// and a cursor issued against the old cache is refused rather than decoded
	// against a generation that is not there. The store is the lease store: a
	// spool lives exactly as long as the retention lease its cursor carries.
	if s.signer, err = pagination.OpenSigner(s.dataDir); err != nil {
		return nil, err
	}
	if s.spools, err = pagination.NewSpools(filepath.Join(s.dataDir, workDirName, spoolsDirName),
		cfg.Resources.MaxTempBytes/spoolBudgetDivisor, s.store); err != nil {
		return nil, err
	}
	s.leases = pagination.NewLeases(s.store, cfg.Storage.QueryCursorTTL.Std())
	s.gate = newGraphGate(cfg.Resources.MaxConcurrentGraphQueries)

	// The analyzer runners are budgeted from what their children reserve, not
	// from resources.base_memory_budget_bytes: that setting budgets this
	// process's own footprint. Using it here refused every language server and
	// every graph-engine run at admission, before the process existed, because
	// one server reserves several times the whole base budget (measured on this
	// repository; the lane report carries the output).
	childMemory := dependence.ObserveMachine().Allocation(
		dependence.DefaultBaseFootprintBytes, dependence.DefaultSafetyMarginBytes)
	if childMemory <= 0 {
		childMemory = unobservedChildMemoryBudget
	}
	shared, err := process.NewRunner(process.Limits{
		MaxConcurrent:     maxInt(1, cfg.Resources.MaxConcurrentHeavy) + sharedRunnerHeadroom,
		MemoryBudgetBytes: childMemory,
		DiskBudgetBytes:   cfg.Resources.MaxTempBytes,
	})
	if err != nil {
		return nil, err
	}
	parserWorkers := maxInt(1, cfg.Index.MaxParserWorkers)
	parsers, err := process.NewRunner(process.Limits{
		MaxConcurrent:     parserWorkers,
		MemoryBudgetBytes: int64(parserWorkers) * parserWorkerReservationBytes,
		DiskBudgetBytes:   cfg.Resources.MaxTempBytes,
	})
	if err != nil {
		return nil, err
	}
	// A language server's reservation is a property of the pinned definition,
	// so the server runner is budgeted from the definitions themselves rather
	// than from a number here that would drift the moment one is re-pinned.
	maxServers := maxInt(1, cfg.Providers.LSP.MaxServers)
	var serverMemory, serverDisk int64
	for _, def := range lsp.Definitions() {
		serverMemory = maxInt64(serverMemory, def.MemoryBudgetBytes)
		serverDisk = maxInt64(serverDisk, def.DiskBudgetBytes)
	}
	servers, err := process.NewRunner(process.Limits{
		MaxConcurrent:     maxServers,
		MemoryBudgetBytes: int64(maxServers) * serverMemory,
		DiskBudgetBytes:   int64(maxServers) * serverDisk,
	})
	if err != nil {
		return nil, err
	}

	if root.HasGit {
		// A Git workspace is never captured without its membership and ignore
		// rules, so an absent Git here is a workspace failure rather than a
		// quieter capture.
		exePath, gerr := git.Locate()
		if gerr != nil {
			return nil, gerr
		}
		if s.git, err = git.New(ctx, shared, exePath, 0); err != nil {
			return nil, err
		}
	}

	if s.resolver, s.toolDir, err = openResolver(toolCfg, os.Stderr); err != nil {
		return nil, err
	}

	fs, err := filesystem.New(filesystem.Options{MaxSearchFileBytes: cfg.Workspace.MaxSearchFileBytes})
	if err != nil {
		return nil, err
	}
	mf, err := manifest.New(manifest.Options{MaxParseFileBytes: cfg.Workspace.MaxParseFileBytes})
	if err != nil {
		return nil, err
	}
	if s.ts, err = treesitter.New(treesitter.Options{
		Languages:         cfg.Providers.TreeSitter.Languages,
		MaxWorkers:        parserWorkers,
		MaxParseFileBytes: cfg.Workspace.MaxParseFileBytes,
		WorkerIdleTTL:     cfg.Providers.TreeSitter.WorkerIdleTTL.Std(),
		WorkerMemoryBytes: parserWorkerReservationBytes,
		Worker:            treesitter.WorkerCommand{Path: exe, Args: []string{wire.Subcommand}},
		Runner:            parsers,
		WorkDir:           tsWorkDir,
	}); err != nil {
		return nil, err
	}
	// The SCIP provider is given the resolver so a kind whose payload the store
	// does not hold is still planned and fetched by the first unit that needs
	// it; construction installs nothing either way.
	sp, err := scip.New(ctx, scip.Options{
		Resolver: s.resolver,
		Runner:   shared,
		Timeout:  cfg.Providers.SCIP.Timeout.Std(),
		WorkDir:  scipWorkDir,
	})
	if err != nil {
		return nil, err
	}

	providers := []provider.Provider{fs, mf, s.ts, sp}
	if dp := s.openDependence(ctx, shared); dp != nil {
		providers = append(providers, dp)
	}
	if s.registry, err = provider.NewRegistry(providers...); err != nil {
		return nil, err
	}
	if s.pool, err = provider.NewPool(cfg.Index.QueueBytes); err != nil {
		return nil, err
	}
	if s.lsp, err = lsp.New(lsp.Options{
		Runner:                 servers,
		DataDir:                s.dataDir,
		MaxServers:             maxServers,
		MaxOutstandingRequests: cfg.Providers.LSP.MaxOutstandingRequests,
		RequestTimeout:         cfg.Providers.LSP.RequestTimeout.Std(),
		IdleTTL:                cfg.Providers.LSP.IdleTTL.Std(),
		MaxOverlayBytes:        cfg.Providers.LSP.MaxOverlayBytes,
	}); err != nil {
		return nil, err
	}
	// The filesystem watcher is the notification half of Section 13.2. It is
	// built only for a run that may index: a report never watches, and a
	// watcher a report constructed would place inotify watches over the whole
	// workspace to answer a question that reads one row.
	//
	// The policy is the capture's own, not bare Config.TraversalPolicy():
	// snapshot.TraversalPolicy installs the Git ignore predicate -- and only
	// that, because the force-include hooks a capture pass adds are backed by
	// that capture's staging database and have no standalone form, so they stay
	// nil here. Without the ignore predicate the watch set and the periodic
	// rescan cover every gitignored build tree the snapshot can never contain.
	//
	// The ignored set is read once, here: a .gitignore edited during a session
	// is honoured by the next generation's own traversal, which recomputes it,
	// but not by the watch set, which keeps this snapshot of it until the
	// workspace is reopened. Watcher.Coverage's doc names that window.
	if o.mode == modeIndex {
		policy, perr := snapshot.TraversalPolicy(ctx, cfg.TraversalPolicy(), root, s.git)
		if perr != nil {
			return nil, perr
		}
		if s.watcher, err = watch.New(watch.Options{
			Root:      root,
			Policy:    policy,
			Debounce:  cfg.Index.WatchDebounce.Std(),
			Reconcile: cfg.Index.ReconcileInterval.Std(),
			MaxPaths:  cfg.Index.WatchPendingPaths,
			MaxBytes:  cfg.Index.WatchPendingBytes,
			Logger:    s.logger,
		}); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// rebuildDir is the sibling cache `index --rebuild` opens. Section 12.2 makes
// the flag an explicitly requested new cache, not a migration and not a
// deletion: the configured directory and the database inside it are left
// exactly as they were, and the new cache is named beside it so an operator can
// see both and remove the one they no longer want.
//
// The instant is a parameter so the name is a function of the run rather than
// of a clock read somewhere inside the composition. The name it derives is a
// candidate, not the answer: makeRebuildDir owns which directory is actually
// created.
func rebuildDir(dataDir string, now time.Time) string {
	return dataDir + "-rebuild-" + now.UTC().Format("20060102T150405Z")
}

// maxRebuildAttempts bounds the disambiguating ladder below. The timestamp has
// one-second granularity, so the ladder exists for runs inside the same second;
// a workspace that has produced 64 rebuild caches in one second is a script in
// a loop, not an operator, and it deserves an error rather than a 65th cache.
const maxRebuildAttempts = 64

// makeRebuildDir creates the sibling cache and returns the directory it made.
//
// It is os.Mkdir and not os.MkdirAll because the difference is the whole
// contract: MkdirAll accepts a directory that already exists, so two
// `index --rebuild` runs inside the same second silently shared one cache while
// the second run told the operator a new one had been created. An existing
// directory is therefore visible here, and the name gains a `-2`, `-3`, ...
// suffix until one is free -- the timestamp still names the run, the suffix
// only distinguishes runs the timestamp cannot.
//
// The parent is created with MkdirAll: the configured data directory's own
// parent may not exist yet on a first run, and that directory is not the one
// whose prior existence means anything.
func makeRebuildDir(dataDir string, now time.Time) (string, error) {
	base := rebuildDir(dataDir, now)
	if err := os.MkdirAll(filepath.Dir(base), 0o700); err != nil {
		return "", &model.Error{Code: model.CodeInternal,
			Message: "the private data directory cannot be created: " + err.Error()}
	}
	for attempt := 1; attempt <= maxRebuildAttempts; attempt++ {
		candidate := base
		if attempt > 1 {
			candidate = base + "-" + strconv.Itoa(attempt)
		}
		err := os.Mkdir(candidate, 0o700)
		if err == nil {
			return candidate, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return "", &model.Error{Code: model.CodeInternal,
				Message: "the rebuild cache cannot be created: " + err.Error()}
		}
	}
	return "", &model.Error{Code: model.CodeArgumentInvalid,
		Message: "a new rebuild cache cannot be named beside " + dataDir +
			": every candidate from " + base + " onwards already exists. Remove the rebuild caches you no longer need, or wait a second and run the command again."}
}

// openDependence builds the dependence provider, or reports its absence.
//
// The provider is not constructed when the configuration disables it, and it
// cannot be constructed when the analysis payload does not resolve. Neither is
// an error: an optional tool that is off or absent must not fail a healthy base
// generation (Section 11.1). What it must not do is disappear -- so the reason
// becomes a capability row the coordinator publishes, which is the difference
// between "this capability is unavailable, here is why" and a capability the
// report never mentions.
func (s *stack) openDependence(ctx context.Context, runner *process.Runner) provider.Provider {
	if s.cfg.Providers.Dependence.Enabled == config.Disabled {
		s.dependenceAbsent(model.CapabilityUnavailable, "", nil)
		return nil
	}
	// Nothing here installs anything any more: the locator reports the pinned
	// identity of a payload the store does not hold and the first unit that
	// needs the engine resolves it. The report path therefore needs no second,
	// force-offline resolver -- which was itself a defect, because it reported
	// a merely-uninstalled payload as CTX_TOOL_OFFLINE and told an operator to
	// turn off an offline mode they had never enabled.
	locator, err := joern.NewLocator(s.resolver)
	if err == nil {
		var backend *joern.Backend
		if backend, err = joern.New(ctx, locator, runner); err == nil {
			var p *dependence.Provider
			p, err = dependence.New(backend, dependence.Options{
				DataDir:                s.dataDir,
				Timeout:                s.cfg.Providers.Dependence.Timeout.Std(),
				CacheBytes:             s.cfg.Providers.Dependence.CacheBytes,
				UnitMemoryFloorBytes:   s.cfg.Providers.Dependence.UnitMemoryFloorBytes,
				UnitMemoryCeilingBytes: s.cfg.Providers.Dependence.UnitMemoryCeilingBytes,
				Limits: provider.Limits{
					BatchRecords:   s.cfg.Index.BatchRecords,
					BatchBytes:     s.cfg.Index.BatchBytes,
					MaxRecordBytes: s.cfg.Resources.MaxProviderRecordBytes,
				},
			})
			if err == nil {
				return p
			}
		}
	}
	// `auto` treats an absent payload as honest absence; an explicitly
	// requested provider that cannot run is a failed capability.
	state := model.CapabilityUnavailable
	if s.cfg.Providers.Dependence.Enabled == config.Enabled {
		state = model.CapabilityFailed
	}
	var typed *model.Error
	code := model.CodeProviderUnavailable
	if errors.As(err, &typed) {
		code = typed.Code
	}
	s.dependenceAbsent(state, code, err)
	return nil
}

// dependenceAbsent records the capability rows of the dependence provider when
// it could not be built. The capability names come from the provider package
// rather than from a literal list here, so a provider that gains a capability
// does not quietly stop reporting it when it is absent. It is deliberately
// specific to this provider: a general helper taking a provider ID would
// publish this provider's capability names under another provider's identity
// the first time a second one failed to construct.
func (s *stack) dependenceAbsent(state model.CapabilityStateValue, code string, cause error) {
	for _, capability := range dependence.Capabilities {
		s.states = append(s.states, model.CapabilityState{ProviderID: dependence.ProviderID, Capability: capability,
			Scope: provider.ScopeWorkspace, State: state, DiagnosticCode: code})
	}
	if cause != nil {
		s.logger.Info("an optional provider is not available in this workspace",
			"component", "app", "provider_id", dependence.ProviderID, "state", string(state), "error_code", code)
	}
}

// openQueries builds the query services that need the repository identity,
// which only the coordinator derives. It is called by open once the coordinator
// exists rather than from openStack, because the alternative -- re-deriving the
// identity here -- would be a second spelling of it that silently drifts the
// day the first one changes.
func (s *stack) openQueries(repo model.RepositoryID) error {
	s.repo = repo
	svc, err := search.New(search.Options{
		Store:     s.store,
		Repo:      repo,
		Signer:    s.signer,
		Spools:    s.spools,
		Content:   s.cas,
		Resources: s.cfg.Resources,
		CursorTTL: s.cfg.Storage.QueryCursorTTL.Std(),
		Now:       time.Now,
		Logger:    s.logger,
	})
	if err != nil {
		return err
	}
	s.search = svc
	return s.openCoverage()
}

// openCoverage builds the Section 16 coverage service. It is called from
// openQueries rather than openStack for the same reason the search service is:
// nothing below the coordinator can name the repository, and the session store
// the service reads is scoped by it.
//
// The service is built in both compositions, because `codectx context ...`
// opens the workspace for a report (it reads pinned source and writes only
// session rows, so it needs no workspace lock); building it only for an
// indexing run would leave every one of those commands with no service.
//
// The store is passed as the narrow coverage.Sessions interface and the CAS
// never leaves this file: the service sees the frozen interfaces and a Limits
// resolved here, never *sqlite.Store's wider surface, *snapshot.CAS or
// config.Config.
func (s *stack) openCoverage() error {
	s.views = make(map[model.SnapshotID]*snapshot.View)
	s.snapshots = make(map[model.GenerationID]model.SnapshotID)
	svc, err := coverage.New(coverage.Options{
		Sessions:   s.store,
		OpenSource: s.openView,
		Signer:     s.signer,
		// The session lease must expire with the session it retains, so this is
		// its own Leases at coverage.session_ttl -- the search service's is at
		// storage.query_cursor_ttl and sharing it would release the pinned
		// snapshot out from under a session that is still open.
		Leases: pagination.NewLeases(s.store, s.cfg.Coverage.SessionTTL.Std()),
		Limits: coverageLimits(s.cfg),
		Now:    time.Now,
		Logger: s.logger,
	})
	if err != nil {
		return err
	}
	s.coverage = svc
	return nil
}

// openWorkflow builds the Section 17 workflow service. It is composed last,
// from open() rather than from openQueries, because workflow.New requires a
// Compiler and the compiler itself does not exist until openCompiler has run
// over the workspace's graph factory: wiring it inside openQueries would fail
// every composition, including the report composition `codectx context ...`
// uses.
//
// Like the coverage service it is built in both compositions -- the context
// commands open the workspace for a report -- and it is built eagerly, so a
// missing dependency or a non-positive bound fails the open rather than every
// request.
//
// The store is passed as the narrow workflow.Sessions interface and the bounds
// are resolved here, so the service never sees *sqlite.Store's wider surface or
// config.Config.
func (s *stack) openWorkflow() error {
	svc, err := workflow.New(workflow.Options{
		Sessions: s.store,
		Compile:  s.compiler,
		Validate: validatorFunc(s.currentSource),
		Limits:   workflowLimits(s.cfg),
		Now:      time.Now,
		Logger:   s.logger,
	})
	if err != nil {
		return err
	}
	s.workflow = svc
	return nil
}

// workflowLimits resolves the Section 20.1 bounds the workflow service
// enforces. It is the only place configuration is turned into those bounds, so
// the service itself never reads config.Config.
//
// MaxObservationReferences has no configuration key: it is the model's own
// ceiling on one observation's reference list (model.MaxObservationReferences),
// and an operator-settable second ceiling would be a bound the model already
// refuses to exceed.
func workflowLimits(cfg config.Config) workflow.Limits {
	return workflow.Limits{
		MaxPageItems:                        cfg.Resources.MaxPageItems,
		MaxObservationReferences:            model.MaxObservationReferences,
		MaxCapsuleBytes:                     cfg.Context.MaxCapsuleBytes,
		QueryTimeout:                        cfg.Resources.QueryTimeout.Std(),
		AllowExploratoryWaiverConsolidation: cfg.Context.AllowExploratoryWaiverConsolidation,
	}
}

// validatorFunc adapts the closure below to workflow.Validator. The interface
// has one method, so the adapter is the whole implementation; a named struct
// would add a type without adding a fact.
type validatorFunc func(ctx context.Context, file model.FileID, hash string) (bool, error)

func (f validatorFunc) Current(ctx context.Context, file model.FileID, hash string) (bool, error) {
	return f(ctx, file, hash)
}

// currentSource is the workflow service's Validator: does this pinned file
// still carry this content hash in the repository's CURRENT source?
//
// The question is deliberately asked against the ACTIVE generation's snapshot
// and not the session's own. A session's pinned hashes were copied out of its
// own snapshot and a snapshot is immutable, so validating against it would
// compare a row with itself and answer true for every file forever -- a
// readiness gate that can never detect stale source, which is the precise
// failure Section 16.3's precondition 7 exists to catch.
//
// It reads the catalog row directly rather than through s.view: *snapshot.View
// exposes content only by opening or reading bytes, and this needs one indexed
// metadata row. Store.SnapshotFile is the same Catalog method the view itself
// calls, so this is not a second path to the pinned manifest.
//
// Three answers are "not current" rather than failures, because each is a real
// state of a live workspace: nothing published yet, the file absent from the
// current snapshot, and a deletion tombstone.
//
// Note what this cannot decide today, so nobody reads more into it than it
// says. A generation's snapshot is fixed when the generation begins, so while
// the session's generation IS the active one the snapshot asked here is the
// session's own and every pinned hash matches by construction; and when it is
// not the active one, the gate is already shut by supersession. Precondition 7
// is therefore subsumed by Superseded at present. It is asked separately anyway
// because the two are different questions -- the readiness contract promises a
// per-file answer, and the source that would make it decisive is a per-file
// WORKTREE hash, which this repository does not have yet: coherence is
// snapshot-level (HEAD plus a dirty flag, internal/index/status.go:88).
// Ledgered for Task 20; until then the guarantee limit's "pair it with
// expected-content-hash validation at each write" is what covers the gap.
func (s *stack) currentSource(ctx context.Context, file model.FileID, hash string) (bool, error) {
	snap, err := s.activeSnapshot(ctx)
	if err != nil {
		var typed *model.Error
		if errors.As(err, &typed) && typed.Code == model.CodeNoActiveGeneration {
			return false, nil
		}
		return false, err
	}
	fv, err := s.store.SnapshotFile(ctx, snap, file)
	if err != nil {
		var typed *model.Error
		if errors.As(err, &typed) && typed.Details["reason"] == sqlite.ReasonNotFound {
			return false, nil
		}
		return false, err
	}
	if fv.Status == model.FileDeleted {
		return false, nil
	}
	return fv.ContentHash == hash, nil
}

// activeSnapshot is the snapshot of the repository's currently published
// generation.
//
// The active generation is re-read on every call -- it is one indexed row and
// it is exactly the fact that may have moved since the last request, so caching
// it would cache the answer the gate must not cache. The generation-to-snapshot
// binding is immutable once published, so that half is memoised beside the
// views, which is what keeps a per-file validator walk from taking a retention
// lease per file.
func (s *stack) activeSnapshot(ctx context.Context) (model.SnapshotID, error) {
	gen, err := s.store.ActiveGeneration(ctx, s.repo)
	if err != nil {
		return "", err
	}
	s.viewsMu.Lock()
	id, ok := s.snapshots[gen]
	s.viewsMu.Unlock()
	if ok {
		return id, nil
	}
	pinned, err := s.store.PinGeneration(ctx, s.repo, gen, s.cfg.Storage.QueryCursorTTL.Std())
	if err != nil {
		return "", err
	}
	defer pinned.Close()
	id = pinned.Binding().SnapshotID
	s.viewsMu.Lock()
	s.snapshots[gen] = id
	s.viewsMu.Unlock()
	return id, nil
}

// openCompiler builds the Section 15 context compiler over the services
// openQueries composed. graph is Workspace.Query, which opens a bounded engine
// over ONE explicit generation and returns the release that drops its lease: it
// satisfies contextpkg.GraphFactory exactly, so the sqlite adjacency adapter is
// reused rather than spelled a second time here.
//
// It is built eagerly at open, like search, so a missing dependency fails the
// composition instead of every compile, where it would read as a data problem.
func (s *stack) openCompiler(graph contextpkg.GraphFactory) error {
	c, err := contextpkg.New(contextpkg.Options{
		Store:  s.store,
		Repo:   s.repo,
		Search: s.search,
		Graph:  graph,
		Config: s.cfg,
		Now:    time.Now,
		Logger: s.logger,
	})
	if err != nil {
		return err
	}
	s.compiler = c
	return nil
}

// coverageLimits resolves the Section 20.1 bounds the coverage service enforces.
// It is the only place configuration is turned into those bounds, so the service
// itself never reads config.Config.
//
// ReceiptTTL has no key of its own: a receipt is a signed token handed back for
// one continuation exactly as a cursor is, so it expires on
// storage.query_cursor_ttl, the key that already bounds this workspace's signed
// tokens. Giving it a second key would let an operator set two lifetimes for one
// kind of token.
func coverageLimits(cfg config.Config) coverage.Limits {
	return coverage.Limits{
		ChunkBytes:                     cfg.Coverage.ChunkBytes,
		MaxChunkBytes:                  cfg.Coverage.MaxChunkBytes,
		MaxSourceResponseBytes:         cfg.Resources.MaxSourceResponseBytes,
		MaxMetadataResponseBytes:       cfg.Resources.MaxMetadataResponseBytes,
		MaxReceiptsPerConfirmation:     cfg.Coverage.MaxReceiptsPerConfirmation,
		MaxUnconfirmedChunksPerSession: cfg.Coverage.MaxUnconfirmedChunksPerSession,
		MaxPageItems:                   cfg.Resources.MaxPageItems,
		SessionTTL:                     cfg.Coverage.SessionTTL.Std(),
		QueryTimeout:                   cfg.Resources.QueryTimeout.Std(),
		ReceiptTTL:                     cfg.Storage.QueryCursorTTL.Std(),
	}
}

// openView is the coverage service's SourceOpener: the per-snapshot verified
// read surface, memoised so a session that reads a hundred chunks opens one
// view rather than a hundred. A *snapshot.View holds no handle -- it is the
// catalog, the CAS and an immutable header -- so there is nothing to release
// and the map is dropped with the stack.
func (s *stack) openView(ctx context.Context, id model.SnapshotID) (coverage.Source, error) {
	v, err := s.view(ctx, id)
	if err != nil {
		// The concrete pointer is dropped explicitly rather than returned into
		// the interface: a nil *snapshot.View in a non-nil coverage.Source is a
		// value every caller's `if src != nil` would wave through.
		return nil, err
	}
	return v, nil
}

// view is the memoised view itself, in its concrete type. openView narrows it
// to coverage.Source for the coverage service; the LSP overlay route needs the
// same view as a model.SnapshotView, which *snapshot.View satisfies and
// coverage.Source does not, so the memoisation lives here and is not spelled a
// second time beside it.
func (s *stack) view(ctx context.Context, id model.SnapshotID) (*snapshot.View, error) {
	s.viewsMu.Lock()
	defer s.viewsMu.Unlock()
	if v, ok := s.views[id]; ok {
		return v, nil
	}
	v, err := snapshot.OpenView(ctx, s.store, s.cas, id)
	if err != nil {
		return nil, err
	}
	s.views[id] = v
	return v, nil
}

// Close releases everything the stack opened, in reverse. Every step runs even
// when an earlier one failed: a lock left held or a worker left running is a
// workspace nobody else can index.
func (s *stack) Close() error {
	if s == nil {
		return nil
	}
	var errs []error
	// The context compiler is released first, then the search service: both
	// read through the store, and the compiler reads through the search
	// service, so a dependency closed under either would be a reader outliving
	// what it reads.
	if s.compiler != nil {
		errs = append(errs, s.compiler.Close())
	}
	if s.search != nil {
		errs = append(errs, s.search.Close())
	}
	if s.lsp != nil {
		errs = append(errs, s.lsp.Close())
	}
	if s.ts != nil {
		s.ts.Close()
	}
	if s.store != nil {
		errs = append(errs, s.store.Close())
	}
	if s.lock != nil {
		errs = append(errs, s.lock.Close())
	}
	errs = append(errs, s.root.Close())
	return errors.Join(errs...)
}

// openResolver builds one managed-toolchain resolver over a resolved
// configuration. There is exactly one resolver per composition: `tools.offline`
// is the only thing that refuses a fetch, because a second resolver that
// refused them regardless reported a payload that is merely not installed as a
// payload the operator's own offline setting withheld.
func openResolver(cfg config.Config, stderr io.Writer) (*toolchain.Resolver, string, error) {
	// The two directories are distinct and are passed as such: config's data
	// directory is per workspace and the store under it is <data_dir>/tools,
	// while tools.cache_dir names the store itself -- which is how one store is
	// shared by every checkout on the machine. Folding the second into the first
	// would append "tools" to a path the user already pointed at the store.
	overrides := make(map[string]toolchain.Override, len(cfg.Tools.Override))
	for name, ov := range cfg.Tools.Override {
		overrides[name] = toolchain.Override(ov)
	}
	res, err := toolchain.New(toolchain.Options{
		DataDir:       cfg.Storage.DataDir,
		StoreDir:      cfg.Tools.CacheDir,
		Offline:       cfg.Tools.Offline,
		Mirror:        cfg.Tools.Mirror,
		MaxFetchBytes: cfg.Tools.MaxFetchBytes,
		FetchTimeout:  cfg.Tools.FetchTimeout.Std(),
		Overrides:     overrides,
		// Section 11.7 requires one record per completed fetch in ordinary
		// operation; Section 18.2 puts logs on stderr, never on the result
		// stream a --json consumer reads.
		Log: slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: slog.LevelInfo})),
	})
	if err != nil {
		return nil, "", err
	}
	// The reported path is the resolver's own, so the report can never name a
	// store other than the one it read.
	return res, res.StoreDir(), nil
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
