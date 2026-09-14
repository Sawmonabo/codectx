package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/Sawmonabo/codectx/internal/config"
	"github.com/Sawmonabo/codectx/internal/model"
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
	"github.com/Sawmonabo/codectx/internal/snapshot"
	"github.com/Sawmonabo/codectx/internal/storage/sqlite"
	"github.com/Sawmonabo/codectx/internal/toolchain"
	"github.com/Sawmonabo/codectx/internal/vcs/git"
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
)

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

// openMode selects what the composition may install while it is built.
type openMode uint8

const (
	// modeIndex composes for a run that will index: a payload a unit needs is
	// installed on demand (Section 11.7).
	modeIndex openMode = iota
	// modeReport composes for a read-only report. Nothing is fetched: a
	// `status` that downloads an analyzer to say whether it is present has
	// already made the answer untrue, and on a slow link it turns a status
	// call into a multi-gigabyte transfer (ledger 159).
	modeReport
)

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
	logger   *slog.Logger

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
func openStack(ctx context.Context, repo string, wait time.Duration, mode openMode) (s *stack, err error) {
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
	s = &stack{root: root, cfg: cfg, dataDir: cfg.Storage.DataDir,
		logger: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))}
	// Every later step opens something; from here a failure must release what
	// has been opened so far, in reverse.
	defer func() {
		if err != nil {
			s.Close()
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

	if s.lock, err = snapshot.LockWorkspace(ctx, s.dataDir, wait); err != nil {
		return nil, err
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
	// rather than inherited.
	if err = s.store.Recover(ctx, time.Now()); err != nil {
		return nil, err
	}
	if s.cas, err = snapshot.OpenCAS(snapshot.CASDir(s.dataDir)); err != nil {
		return nil, err
	}

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

	if s.resolver, s.toolDir, err = openResolver(cfg, os.Stderr, false); err != nil {
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
	if dp := s.openDependence(ctx, shared, mode); dp != nil {
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
	}); err != nil {
		return nil, err
	}
	return s, nil
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
func (s *stack) openDependence(ctx context.Context, runner *process.Runner, mode openMode) provider.Provider {
	if s.cfg.Providers.Dependence.Enabled == config.Disabled {
		s.dependenceAbsent(model.CapabilityUnavailable, "", nil)
		return nil
	}
	// This is the one construction in the whole composition that can fetch: the
	// locator resolves the analysis payload here rather than at unit time, so a
	// read-only report -- and only the locator inside it -- resolves through a
	// resolver with fetching refused. Everything else already installs nothing
	// at construction, and handing them an offline resolver would change what
	// they report about payloads they merely inspect.
	resolver := s.resolver
	if mode == modeReport {
		var rerr error
		if resolver, _, rerr = openResolver(s.cfg, os.Stderr, true); rerr != nil {
			s.dependenceAbsent(model.CapabilityUnavailable, model.CodeProviderUnavailable, rerr)
			return nil
		}
	}
	locator, err := joern.NewLocator(resolver)
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

// Close releases everything the stack opened, in reverse. Every step runs even
// when an earlier one failed: a lock left held or a worker left running is a
// workspace nobody else can index.
func (s *stack) Close() error {
	if s == nil {
		return nil
	}
	var errs []error
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
// configuration. forceOffline refuses every fetch without opening a socket,
// which is what a read-only report composes with.
func openResolver(cfg config.Config, stderr io.Writer, forceOffline bool) (*toolchain.Resolver, string, error) {
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
		Offline:       cfg.Tools.Offline || forceOffline,
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
