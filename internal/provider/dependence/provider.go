package dependence

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/Sawmonabo/codectx/internal/lang"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider"
	"github.com/Sawmonabo/codectx/internal/provider/dependence/neo4jcsv"
	"github.com/Sawmonabo/codectx/internal/snapshot"
	"github.com/Sawmonabo/codectx/internal/workspace"
)

const (
	// ProviderID is the product-facing identity. The engine behind it is never
	// named here, in a configuration key, in an evidence detail or in a query
	// result (Section 11.6).
	ProviderID = "dependence"
	// adapterVersion changes whenever this package's planning, governance,
	// classification or pinned argument arrays change. The descriptor version
	// is this plus the engine payload digest, so a unit built by one engine
	// release is never reused for another.
	adapterVersion = "3"
	// ScopeWorkspace is the scope of the C/C++ unit, the one unit that is the
	// whole repository.
	ScopeWorkspace = provider.ScopeWorkspace
	// component is the slog component every entry of this package carries.
	component = "provider.dependence"
)

// dependsOn are the providers whose units must be complete before a dependence
// unit runs: the file identities and the syntactic layer its call facts are
// reconciled against.
var dependsOn = []string{"filesystem", "treesitter"}

// Product-owned bounds on what one unit may leave on disk. Neither is a
// configuration knob: they are the shapes Section 6 requires a finite bound
// for, sized from the largest repositories measured in
// docs/research/10-round3-empirical.md (a 1.8M-line C repository is about 54 MB
// of source and 2 GB of export; a 1.05M-line Python tree exports 4.95 GB).
const (
	maxMaterializationBytes int64 = 4 * giB
	maxExportBytes          int64 = 16 * giB
)

// minStepTimeout is the least time an analysis step is started with. A unit
// whose deadline has all but expired fails as a timeout rather than starting a
// process that cannot finish.
const minStepTimeout = 5 * time.Second

// Options are the provider's configuration, mirroring `[providers.dependence]`
// field for field so the controller's wiring from config.Providers.Dependence
// is mechanical. This package never imports the configuration package: it is a
// provider, not a configuration consumer, and the coordinator owns the
// translation.
//
// `enabled` is deliberately absent. It is the coordinator's decision, not the
// provider's: `auto` (the default) enqueues every dependence unit as
// low-priority background work once the base generation is active, `true`
// blocks the index on those units and `false` never runs the provider at all.
// docs/providers-dependence.md carries the contract, including the typed
// `pending` payload a query gets while a unit it needs has not sealed.
type Options struct {
	// DataDir is the absolute private data directory the graph cache and the
	// run directories live under.
	DataDir string
	// Timeout bounds one whole unit: materialize, parse, export and import.
	// Zero is no wall-clock bound; StallTimeout catches a wedged unit instead.
	Timeout time.Duration
	// StallTimeout is the progress-based hang detector applied to every child
	// this provider starts (providers.dependence.stall_timeout). Zero disables
	// it.
	StallTimeout time.Duration
	// CacheBytes is the parsed-graph cache budget; 0 disables the cache.
	CacheBytes int64
	// UnitMemoryFloorBytes is the smallest heap cap a unit is given, and
	// UnitMemoryCeilingBytes the explicit user limit that may reject a unit
	// before it runs; 0 means the allocation is derived from the machine and
	// nothing is rejected up front.
	UnitMemoryFloorBytes   int64
	UnitMemoryCeilingBytes int64
	// Limits are the sink bounds the importer enforces before it allocates.
	Limits provider.Limits
	// MaxUnitsPerFamily, MaxStagedRows and MaxDerivedRows are the three
	// user-set reporting thresholds of `[providers.dependence]`, 0 (the
	// default) meaning no threshold at all. None refuses anything: a
	// repository's project count, an export's row count and the occurrences
	// that project from it are properties of the source, so exceeding one is
	// published on the unit's capability rows -- never a plan refusal, never a
	// silently truncated list, never a failed unit.
	//
	// These are the resolved values of config.Limit keys; the composition root
	// reads the Limit and hands the int64 over, because this package does not
	// import internal/config.
	MaxUnitsPerFamily int64
	MaxStagedRows     int64
	MaxDerivedRows    int64
}

// Provider is the dependence provider.Provider. One instance serves a process
// and is safe for concurrent use; every run works in its own private
// directory.
type Provider struct {
	backend  Backend
	importer Importer
	opts     Options
	gov      Governor
	cache    *Cache
	engine   Engine
}

// New binds the engine backend to the production export importer. The engine
// is resolved by the backend before this point, so the descriptor version is
// fixed for the process: a graph produced under one payload digest is never
// confused with one produced under another, and an engine replaced under a
// running process is not silently adopted.
func New(backend Backend, opts Options) (*Provider, error) {
	return NewWithImporter(backend, defaultImporter{}, opts)
}

// NewWithImporter is New with the importer supplied. It exists for the tests
// that drive the provider's failure paths without an engine: the fake backend
// they need writes an export no real reader can import, so the two have to be
// replaced together.
func NewWithImporter(backend Backend, importer Importer, opts Options) (*Provider, error) {
	if backend == nil || importer == nil {
		return nil, invalid("the dependence provider needs an engine backend and an export importer")
	}
	if opts.Timeout < 0 || opts.StallTimeout < 0 {
		return nil, invalid("the dependence provider's unit timeout and stall timeout may not be negative")
	}
	if opts.MaxUnitsPerFamily < 0 || opts.MaxStagedRows < 0 || opts.MaxDerivedRows < 0 {
		return nil, invalid("the dependence provider's max_units_per_family, max_staged_rows and max_derived_rows may not be negative; 0 is no threshold")
	}
	if err := opts.Limits.Validate(); err != nil {
		return nil, err
	}
	e := backend.Engine()
	// The argv is the backend's concern: a lazily resolved backend has none
	// until its first unit runs and re-checks both before it executes anything.
	if e.Digest == "" {
		return nil, &model.Error{Code: model.CodeProviderUnavailable,
			Message: "the dependence backend resolved no analysis payload"}
	}
	cacheBytes := opts.CacheBytes
	if cacheBytes < 0 {
		cacheBytes = 0
	}
	cache, err := OpenCache(opts.DataDir, cacheBytes)
	if err != nil {
		return nil, err
	}
	sweepPrivate(opts.DataDir)
	return &Provider{backend: backend, importer: importer, opts: opts,
		gov: NewGovernor(opts.UnitMemoryFloorBytes, opts.UnitMemoryCeilingBytes), cache: cache, engine: e}, nil
}

// Descriptor is the static contract. Version is the adapter version plus the
// engine payload digest (Section 11.6): every evidence row's provider_version
// then carries both, so a fact can always be traced to the exact analysis that
// produced it without the engine's name appearing anywhere in the row.
//
// InvalidationScope is package: all but one unit is a frontend-native project.
// The C/C++ unit is the whole repository and says so through its scope key,
// which is what the coordinator schedules on.
func (p *Provider) Descriptor() model.ProviderDescriptor {
	return model.ProviderDescriptor{ID: ProviderID, Version: truncate(adapterVersion+"/"+p.engine.Digest, model.MaxIdentifierBytes),
		Capabilities: Capabilities, DependsOn: dependsOn, InvalidationScope: model.InvalidationPackage}
}

// Detect reports whether this workspace has anything for the provider to
// analyse and which payload would analyse it. There is no version probe: the
// pinned engine's command line has no version flag that can be run
// noninteractively, so name, version and digest come from the resolved
// payload, which is where the lock recorded them.
//
// Detection reads only the repository's project markers, stops at the bound,
// and never walks the tree into memory: it accumulates at most
// provider.MaxDetectionInputs paths and one boolean, whatever the repository's
// size. It traverses where the filesystem and treesitter providers answer from
// a constant because neither question it must answer — which projects exist,
// and whether any analysable source exists at all — can be answered without
// looking.
func (p *Provider) Detect(ctx context.Context, root workspace.Root, policy workspace.Policy) (provider.Detection, error) {
	inputs, languages, err := detectInputs(ctx, root, policy)
	if err != nil {
		return provider.Detection{}, err
	}
	if !languages {
		return provider.Detection{Available: false, DiagnosticCode: model.CodeProviderUnavailable}, nil
	}
	// The payload's name is deliberately absent. Detection is a product
	// surface — `status`, `doctor` and the ledger render this string — and the
	// engine is never named on one (Section 11.6, wave-A ruling). The version
	// and the payload digest are the whole of the provenance a reader needs:
	// docs/providers-dependence.md maps a digest to its release.
	return provider.Detection{Available: true, Capabilities: Capabilities, InputPaths: inputs,
		ObservedVersion: truncate("engine "+p.engine.Version+" "+p.engine.Digest, model.MaxIdentifierBytes)}, nil
}

// stopWalk ends a bounded detection walk without making an early stop look
// like a failure.
var stopWalk = errors.New("detection input bound reached")

// detectInputs collects the project markers detection recognized, bounded by
// provider.MaxDetectionInputs, and reports whether any analysable source
// exists at all.
func detectInputs(ctx context.Context, root workspace.Root, policy workspace.Policy) ([]string, bool, error) {
	var inputs []string
	var languages bool
	err := workspace.Walk(ctx, root, policy, func(f workspace.File) error {
		if !languages && FamilyOf(lang.Of(f.Path)) != "" {
			languages = true
		}
		base := filepath.Base(f.Path)
		for _, fam := range Families {
			if slices.Contains(projectMarkers[fam], base) {
				inputs = append(inputs, f.Path)
				break
			}
		}
		if len(inputs) >= provider.MaxDetectionInputs && languages {
			return stopWalk
		}
		return nil
	})
	if err != nil && !errors.Is(err, stopWalk) {
		return nil, false, err
	}
	slices.Sort(inputs)
	if len(inputs) > provider.MaxDetectionInputs {
		inputs = inputs[:provider.MaxDetectionInputs]
	}
	return inputs, languages, nil
}

// ImportOptions carry what the provider.Provider interface has no room for:
// the delta state of the unit being refreshed, and where this run's own state
// is to be written. The coordinator's delta applier owns both; the provider
// fills every other field of the export reader's options itself.
type ImportOptions struct {
	// PreviousKeys is the fact key set the sealed unit this run refreshes
	// published. The zero value is the absent set and means a full import:
	// every relation is published and nothing of a predecessor is carried.
	PreviousKeys neo4jcsv.KeySet
	// KeysPath is the absolute path this run's fresh key set is written to,
	// so the caller can store it with the unit it describes. Empty writes it
	// beside the import's staging database, which is deleted with it — which
	// is what IndexUnit, the full-import form, wants.
	KeysPath string
}

// Report is one import: the provider result the coordinator records, the fresh
// fact key set it stores as this unit's delta state, and the delta that key
// set has against ImportOptions.PreviousKeys.
//
// Keys is the absent set when this run cannot describe the whole unit with one
// set — a subdivided unit, whose parts each ran in full — and the next refresh
// of such a unit is a full import.
type Report struct {
	Result model.ProviderResult
	Keys   neo4jcsv.KeySet
	Delta  neo4jcsv.Delta
}

// IndexUnit is the full-import form of Import, which is what the
// provider.Provider interface can express. A coordinator that holds the unit's
// previous fact key set calls Import instead.
func (p *Provider) IndexUnit(ctx context.Context, req provider.UnitRequest, sink provider.Sink) (model.ProviderResult, error) {
	rep, err := p.Import(ctx, req, sink, ImportOptions{})
	return rep.Result, err
}

// Import produces exactly the assigned unit: plan the snapshot, find the
// unit the coordinator named, reserve, reuse or build the graph, export,
// validate, import, and remove every private artifact on every path. A failure
// returns a typed error and no facts; provider.RunUnit then deletes whatever
// reached storage, so an engine crash can never leave half a graph queryable.
func (p *Provider) Import(ctx context.Context, req provider.UnitRequest, sink provider.Sink,
	opts ImportOptions) (Report, error) {

	// A zero timeout is no wall clock: a monorepo unit is large, not wedged,
	// and the stall detector on every child is what catches a wedged one. The
	// cancel is still installed so the unit's children are torn down when the
	// caller gives up.
	var cancel context.CancelFunc
	if p.opts.Timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, p.opts.Timeout)
	} else {
		ctx, cancel = context.WithCancel(ctx)
	}
	defer cancel()

	unit, plan, err := p.unitFor(ctx, req)
	if err != nil {
		return Report{}, err
	}
	res := p.gov.Reserve(unit.Family, unit.Bytes, ObserveMachine())
	if err := p.gov.Reject(res, unit.ScopeKey); err != nil {
		return Report{}, err
	}

	run, err := p.openRun(req)
	if err != nil {
		return Report{}, err
	}
	defer run.close(req)

	pub, report, err := p.build(ctx, req, unit, res, run, sink, opts)
	if err != nil {
		return Report{}, err
	}
	pub.UnknownLabels = report.UnknownLabels
	// A project of this family the planner had to refuse has no unit of its
	// own: its files were analysed by whichever unit encloses them, under a
	// scope key that names a different project. Publishing this family fresh
	// while that is true is false readiness, and the unit that ran is the only
	// place with a capability row to say so.
	pub.UnplannedProjects = plan.Unplanned[unit.Family]
	pub.Family = unit.Family
	// The plan holds every project of the family, whatever the count: a
	// monorepo is not refused. A user who set the threshold is told which
	// family crossed it and by how much, on the rows of the unit that ran.
	// pub.OverUnitsPerFamily may already carry a subdivided unit's part count
	// against the same threshold; the plan's count is the larger claim about
	// the family and takes it.
	if n := plan.Projects[unit.Family]; p.opts.MaxUnitsPerFamily > 0 && int64(n) > p.opts.MaxUnitsPerFamily && n > pub.OverUnitsPerFamily {
		pub.OverUnitsPerFamily, pub.UnitsPerFamilyBound = n, p.opts.MaxUnitsPerFamily
	}
	// The unit's staged rows are the sum over its parts when it was subdivided,
	// so the threshold is compared here rather than read off the flag: a unit
	// can cross it in total without any one part crossing it alone.
	if p.opts.MaxStagedRows > 0 && report.StagedRows > p.opts.MaxStagedRows {
		pub.StagedRows, pub.StagedRowsBound = report.StagedRows, p.opts.MaxStagedRows
	}
	// The projected occurrences are summed over a subdivided unit's parts for
	// the same reason, and compared here for the same one.
	if p.opts.MaxDerivedRows > 0 && report.DerivedRows > p.opts.MaxDerivedRows {
		pub.DerivedRows, pub.DerivedRowsBound = report.DerivedRows, p.opts.MaxDerivedRows
	}
	// BytesProcessed is the export bytes the import actually read, which is
	// what this run processed and what the importer measured. The unit's
	// source bytes are a different figure and are logged as source_bytes
	// below; reporting them here would be a number no step of this run read.
	result := model.ProviderResult{RunID: req.Run, State: model.RunSucceeded,
		RecordsEmitted: uint64(report.Nodes + report.Relations + report.Aliases),
		BytesProcessed: report.BytesRead,
		Capabilities:   pub.capabilities(unit.ScopeKey)}
	slog.Info("dependence unit imported", "component", component, "unit", string(req.Unit.ID), "run", string(req.Run),
		"scope", unit.ScopeKey, "family", string(unit.Family), "source_files", unit.Files, "source_bytes", unit.Bytes,
		"nodes", report.Nodes, "relations", report.Relations, "aliases", report.Aliases,
		"export_bytes_read", report.BytesRead, "dropped_methods", report.DroppedMethods,
		"unlocated_facts", report.UnlocatedFacts, "unresolved_writes", report.UnresolvedWrites,
		"clipped_evidence", report.ClippedEvidence, "ignored_export_files", report.IgnoredFiles,
		"external_methods", report.ExternalMethods, "unknown_labels", len(report.UnknownLabels),
		"skipped_methods", pub.SkippedCount, "subdivided", pub.Subdivided != "",
		"unplanned_projects", pub.UnplannedProjects,
		"projects_in_family", plan.Projects[unit.Family], "over_max_units_per_family", pub.OverUnitsPerFamily > 0,
		"staged_rows", report.StagedRows, "over_max_staged_rows", report.OverStagedRows,
		"derived_rows", report.DerivedRows, "over_max_derived_rows", report.OverDerivedRows,
		"heap_cap_bytes", res.HeapCapBytes, "reservation_bytes", res.Bytes(), "allocation_bytes", res.AllocationBytes,
		"keys", report.Keys.Count(), "keys_changed", report.Changed, "keys_unchanged", report.Unchanged,
		"keys_removed", report.Removed)
	return Report{Result: result, Keys: report.Keys,
		Delta: neo4jcsv.Delta{Changed: report.Changed, Unchanged: report.Unchanged, Removed: report.Removed}}, nil
}

// unitFor re-derives the plan from the pinned snapshot and returns the unit
// the coordinator assigned. Planning from the snapshot rather than the live
// checkout is what makes the unit's inputs the ones storage keyed it on; a
// scope key that is not in the plan is a coordinator/provider disagreement and
// fails rather than being guessed at.
// The whole plan is returned with the unit because what the plan refused is
// part of what this unit must publish: a family with an unplannable project is
// not fresh anywhere.
func (p *Provider) unitFor(ctx context.Context, req provider.UnitRequest) (Unit, Plan, error) {
	plan, err := PlanUnits(ctx, req.Content)
	if err != nil {
		return Unit{}, Plan{}, err
	}
	for _, u := range plan.Units {
		if u.ScopeKey == req.Unit.ScopeKey {
			return u, plan, nil
		}
	}
	return Unit{}, Plan{}, invalid("the dependence provider has no unit for scope " + truncate(req.Unit.ScopeKey, 128) + " in this snapshot")
}

// runDir is one unit's private working directory. Everything the analyzer
// reads or writes lives under it and it is removed on every termination path,
// so no materialization, graph or export outlives the unit.
type runDir struct{ root string }

func (p *Provider) openRun(req provider.UnitRequest) (*runDir, error) {
	id, err := model.NewRandomID()
	if err != nil {
		return nil, err
	}
	root := filepath.Join(runsRoot(p.opts.DataDir), "run-"+id[:16])
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, internalErr("the dependence run directory could not be created: " + err.Error())
	}
	return &runDir{root: root}, nil
}

// runsRoot and scratchRoot are the provider's two private working roots under
// the data directory: one directory per run, and the staging databases the
// importer creates. Both are swept at construction (sweepPrivate).
func runsRoot(dataDir string) string    { return filepath.Join(dataDir, "dependence", "runs") }
func scratchRoot(dataDir string) string { return filepath.Join(dataDir, "dependence", "scratch") }

// maxSweptEntries bounds the startup sweep's directory scan, so a corrupted or
// hand-filled private root can never turn provider construction into an
// unbounded walk (Section 6: an explicit finite bound on every traversal).
const maxSweptEntries = 4096

// sweepPrivate removes everything left under the provider's private working
// roots. defer covers every return and every panic but not SIGKILL or power
// loss, and a killed unit leaves a materialization, a graph, an export and a
// staging database behind — hundreds of megabytes for one unit. Nothing else
// in the product knows these paths, so the sweep belongs here.
//
// Sweeping unconditionally at construction is safe because a workspace has one
// cross-process owner (Section 6): no other process holds a run of this
// provider while this one is being built, and no unit of this process has
// started. A failure to remove an entry is logged and skipped rather than
// failing construction: leftover disk is a defect, but refusing to index
// because of it is worse.
func sweepPrivate(dataDir string) {
	if dataDir == "" {
		return
	}
	for _, root := range []string{runsRoot(dataDir), scratchRoot(dataDir)} {
		entries, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		if len(entries) > maxSweptEntries {
			entries = entries[:maxSweptEntries]
		}
		for _, e := range entries {
			path := filepath.Join(root, e.Name())
			if err := os.RemoveAll(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
				slog.Error("a stale dependence working directory was not removed", "component", component, "error", err)
				continue
			}
			slog.Info("a stale dependence working directory was swept", "component", component, "name", e.Name())
		}
	}
}

func (r *runDir) path(name string) string { return filepath.Join(r.root, name) }

func (r *runDir) close(req provider.UnitRequest) {
	if err := os.RemoveAll(r.root); err != nil && !errors.Is(err, fs.ErrNotExist) {
		slog.Error("a dependence run directory was not removed", "component", component,
			"run", string(req.Run), "error", err)
	}
}

// build runs the whole analysis for one unit and returns what it must publish.
func (p *Provider) build(ctx context.Context, req provider.UnitRequest, unit Unit, res Reservation,
	run *runDir, sink provider.Sink, opts ImportOptions) (publication, ImportReport, error) {

	// Membership is decided file by file, by the unit itself: only the files
	// this unit owns are ever written. Copying the whole snapshot and deleting
	// the nested projects afterwards spent the time and the disk of every
	// sibling project, and left another unit's source inside this unit's
	// private tree for as long as the pruning took.
	mat, err := snapshot.Materialize(ctx, req.Content, model.FileSelection{},
		snapshot.MaterializeOptions{Dir: run.path("src"), MaxBytes: maxMaterializationBytes,
			Include: func(fv model.FileVersion) bool { return unit.Contains(fv.Path) }})
	if err != nil {
		return publication{}, ImportReport{}, err
	}
	defer mat.Close()
	source, err := unitSource(mat.Root(), unit)
	if err != nil {
		return publication{}, ImportReport{}, err
	}

	graph, outcome, err := p.graphFor(ctx, req, unit, res, run, source)
	if err != nil {
		return publication{}, ImportReport{}, err
	}
	if outcome.Class == FailureEngine {
		// A reproducible crash is the only thing subdivision is for. Every
		// capability of the result then says so.
		report, overParts, err := p.subdivide(ctx, req, unit, res, run, source, sink, outcome)
		if err != nil {
			return publication{}, ImportReport{}, err
		}
		return publication{Subdivided: unit.ScopeKey, BackendFailed: backendFailure(outcome),
			OverUnitsPerFamily: overParts, UnitsPerFamilyBound: p.opts.MaxUnitsPerFamily}, report, nil
	}

	exp, err := p.export(ctx, req, unit, res, run, graph)
	if err != nil {
		return publication{}, ImportReport{}, err
	}
	report, err := p.importExport(ctx, req, unit, source, unit.Root, run.path("export"), sink, opts)
	if err != nil {
		return publication{}, ImportReport{}, err
	}
	return publication{Skipped: outcome.SkippedMethods, SkippedCount: outcome.SkippedCount + exp.SkippedCount}, report, nil
}

// graphFor reuses a cached graph whose semantic closure matches, or parses a
// new one. The returned outcome is FailureEngine when the unit crashed
// reproducibly and the caller must subdivide; every other failure class is
// already an error by then.
func (p *Provider) graphFor(ctx context.Context, req provider.UnitRequest, unit Unit, res Reservation,
	run *runDir, source string) (string, Outcome, error) {

	key, err := CacheKey(ctx, req.Content, unit, p.backend.Argv(unit.Family), p.engine)
	if err != nil {
		return "", Outcome{}, err
	}
	if cached, ok := p.cache.Lookup(key); ok {
		slog.Info("dependence graph reused", "component", component, "unit", string(req.Unit.ID), "scope", unit.ScopeKey)
		return cached, Outcome{}, nil
	}
	graph := run.path("graph")
	outcome, err := p.parse(ctx, req, unit, res, source, graph, nil)
	if err != nil {
		return "", Outcome{}, err
	}
	switch outcome.Class {
	case FailureMemory:
		retry := p.gov.RetryCap(res, observedPeak(outcome))
		if retry == 0 {
			// Either the first attempt already had the whole allocation, or
			// the tree it ran in peaked at what the machine can allocate. A
			// retry with no more memory behind it cannot succeed and costs a
			// full parse.
			return "", Outcome{}, failure(FailureMemory, unit.ScopeKey, outcome, res)
		}
		retried := res
		retried.HeapCapBytes = retry
		if outcome, err = p.parse(ctx, req, unit, retried, source, graph, nil); err != nil {
			return "", Outcome{}, err
		}
		if outcome.Class == FailureMemory {
			return "", Outcome{}, failure(FailureMemory, unit.ScopeKey, outcome, retried)
		}
		res = retried
	case FailureTimeout:
		return "", Outcome{}, failure(FailureTimeout, unit.ScopeKey, outcome, res)
	}
	if outcome.Class == FailureEngine {
		// Confirm the crash before anything is split. The neutral option
		// allowlist is empty for every frontend today, so the confirmation
		// runs the same argv over the same source; the engine is not
		// run-to-run deterministic, so what it yields is a second observation
		// of the same failure class, which raises the odds that the crash is
		// deterministic without proving it. That is what subdivision is
		// allowed to rest on: a class seen twice, against the cost of
		// splitting a project, which loses more than half of its resolved
		// calls. A crash seen once is never split on.
		confirm, err := p.parse(ctx, req, unit, res, source, graph, p.backend.NeutralOptions(unit.Family))
		if err != nil {
			return "", Outcome{}, err
		}
		if confirm.Class == FailureEngine {
			return "", confirm, nil
		}
		if confirm.Class != FailureNone {
			return "", Outcome{}, failure(confirm.Class, unit.ScopeKey, confirm, res)
		}
		outcome = confirm
	}
	if outcome.SkippedCount == 0 && p.cache.Put(key, graph) {
		// A graph whose parse skipped methods is deliberately not cached. What
		// was skipped is only on the parse's stderr, and a cache hit replays
		// the graph without it — so a reused entry would publish data_flows_to
		// as fresh for a unit whose data dependence is missing whole method
		// bodies. Not caching it costs a reparse for units that skip at all,
		// which the pinned definition cap makes rare (docs/research/
		// 10-round3-empirical.md Section 9a); caching it would cost the truth.
		return p.cache.path(key), outcome, nil
	}
	return graph, outcome, nil
}

// parse runs one parse step and validates that it left a graph behind.
func (p *Provider) parse(ctx context.Context, req provider.UnitRequest, unit Unit, res Reservation,
	source, graph string, extra []string) (Outcome, error) {

	_ = os.Remove(graph)
	timeout, err := remaining(ctx)
	if err != nil {
		return Outcome{}, err
	}
	out, err := p.backend.Parse(ctx, ParseRequest{SourceDir: source, OutputPath: graph, Family: unit.Family,
		HeapCapBytes: res.HeapCapBytes, ExtraArgs: extra, ReservationBytes: res.ParseBytes(), Timeout: timeout, StallTimeout: p.opts.StallTimeout})
	if err != nil {
		return Outcome{}, err
	}
	slog.Info("dependence parse finished", "component", component, "unit", string(req.Unit.ID), "scope", unit.ScopeKey,
		"family", string(unit.Family), "exit_code", out.ExitCode, "duration", out.Duration, "failure_class", string(out.Class),
		"pass", out.Pass, "skipped_methods", out.SkippedCount, "heap_cap_bytes", res.HeapCapBytes,
		"reservation_bytes", res.ParseBytes(), "stderr_bytes", out.StderrBytes,
		"tree_peak_bytes", peakForLog(out))
	if out.Class == FailureNone {
		if info, err := os.Stat(graph); err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
			// A zero exit with no graph is the helper crash the orchestrator
			// hides (research Section 6). It is an engine failure, not a
			// success with nothing in it.
			out.Class = FailureEngine
		}
	}
	return out, nil
}

// export writes the graph out and proves the result is a live graph. A unit
// with source files whose export carries no methods is the zero-exit helper
// crash of research Section 6: the exit code is a lie there, and the only
// honest signal is the empty result.
func (p *Provider) export(ctx context.Context, req provider.UnitRequest, unit Unit, res Reservation,
	run *runDir, graph string) (ExportOutcome, error) {

	dir := run.path("export")
	timeout, err := remaining(ctx)
	if err != nil {
		return ExportOutcome{}, err
	}
	out, err := p.backend.Export(ctx, ExportRequest{GraphPath: graph, OutputDir: dir,
		HeapCapBytes: res.ExportHeapCapBytes, ReservationBytes: res.ExportBytes(), Timeout: timeout, StallTimeout: p.opts.StallTimeout})
	if err != nil {
		return ExportOutcome{}, err
	}
	slog.Info("dependence export finished", "component", component, "unit", string(req.Unit.ID), "scope", unit.ScopeKey,
		"exit_code", out.ExitCode, "duration", out.Duration, "failure_class", string(out.Class),
		"export_live", out.Live, "export_bytes", out.Bytes, "heap_cap_bytes", res.ExportHeapCapBytes,
		"reservation_bytes", res.ExportBytes(), "stderr_bytes", out.StderrBytes)
	if out.Bytes > maxExportBytes {
		return ExportOutcome{}, resourceLimit("the dependence export exceeds the bound one unit may write").
			WithDetail("scope_key", truncate(unit.ScopeKey, model.MaxIdentifierBytes)).
			WithDetail("export_bytes", itoa(out.Bytes)).WithDetail("limit", "max_export_bytes").
			WithDetail("bound", itoa(maxExportBytes))
	}
	if out.Class != FailureNone {
		return ExportOutcome{}, failure(out.Class, unit.ScopeKey, out.Outcome, res)
	}
	if unit.Files > 0 && !out.Live {
		out.Outcome.Class = FailureEngine
		return ExportOutcome{}, failure(FailureEngine, unit.ScopeKey, out.Outcome, res).
			WithDetail("reason", "the analysis produced no methods for a unit that has source")
	}
	return out, nil
}

// importExport streams one export into the sink.
//
// Every field the importer validates is filled from the request: an evidence
// row is invalid without the unit, its provider version and the origin run, a
// relation id cannot be derived without the repository, and a byte range
// cannot be verified without the pinned snapshot bytes. ProjectRoot is the
// absolute directory the engine parsed, because that is what the export's
// absolute FILENAME values are admitted against, and unitRoot is that same
// directory as a snapshot path: the export's relative paths are relative to
// what the engine was given, so a unit below the repository root — a nested
// module, or one part of a subdivided unit — needs its own root prefixed back
// on, or every fact it publishes binds to a path the snapshot does not have
// and the unit seals with nothing in it. ScratchDir keeps the staging database,
// which holds source-derived graph content and was measured at 649 MB for a
// 64 MB export, inside the provider's private data directory instead of the
// system temp directory.
//
// Language is the source language recorded on the export's fileless nodes
// only; a located node takes its language from the snapshot file. The family
// is coarser than the language there (C++ nodes are tagged c, TypeScript and
// TSX nodes javascript), which is accurate for every located fact and
// approximate for the fileless remainder.
//
// PreviousKeys and KeysPath come from the coordinator's delta applier
// (docs/providers-dependence.md §Refresh and delta). A run handed the previous
// unit's key set publishes only the relations whose key changed; a run handed
// none publishes every one.
func (p *Provider) importExport(ctx context.Context, req provider.UnitRequest, unit Unit,
	source, unitRoot, dir string, sink provider.Sink, opts ImportOptions) (ImportReport, error) {

	scratch := scratchRoot(p.opts.DataDir)
	if err := os.MkdirAll(scratch, 0o700); err != nil {
		return ImportReport{}, internalErr("the dependence import scratch directory could not be created: " + err.Error())
	}
	report, err := p.importer.Import(ctx, dir, req.Resolver, sink, neo4jcsv.Options{
		Language: string(unit.Family), UnitScopeKey: unit.ScopeKey, ProjectRoot: source, UnitRoot: unitRoot,
		Limits: p.opts.Limits, Repository: req.Binding.RepositoryID, Unit: req.Unit, Run: req.Run,
		Content: req.Content, ScratchDir: scratch, MaxStagedRows: p.opts.MaxStagedRows,
		MaxDerivedRows: p.opts.MaxDerivedRows,
		PreviousKeys:   opts.PreviousKeys, KeysPath: opts.KeysPath})
	if err != nil {
		return ImportReport{}, err
	}
	if err := os.RemoveAll(dir); err != nil && !errors.Is(err, fs.ErrNotExist) {
		slog.Error("a dependence export was not removed", "component", component, "run", string(req.Run), "error", err)
	}
	return report, nil
}

// subdivide is the last-resort recovery from a reproducible crash. The unit is
// split along the next frontend-native boundary — its immediate source-bearing
// subdirectories — and every child that succeeds is imported into the same
// unit. Nothing about the result is presented as equivalent to a whole-unit
// run: the caller publishes every capability as partial with the failed unit
// and the backend failure. If no child produces anything, the unit fails.
func (p *Provider) subdivide(ctx context.Context, req provider.UnitRequest, unit Unit, res Reservation,
	run *runDir, source string, sink provider.Sink, crash Outcome) (ImportReport, int, error) {

	children, err := childProjects(source, unit)
	if err != nil {
		return ImportReport{}, 0, err
	}
	// The parts are all parsed and imported whatever the count. A user-set
	// providers.dependence.max_units_per_family says how many parts of one
	// unit the caller wanted to hear about, so crossing it is reported on
	// every capability row this subdivided unit publishes.
	overParts := 0
	if p.opts.MaxUnitsPerFamily > 0 && int64(len(children)) > p.opts.MaxUnitsPerFamily {
		overParts = len(children)
		slog.Warn("a subdivided dependence unit has more parts than providers.dependence.max_units_per_family; every part is analysed",
			"component", component, "unit", string(req.Unit.ID), "scope", unit.ScopeKey,
			"parts", len(children), "max_units_per_family", p.opts.MaxUnitsPerFamily)
	}
	slog.Warn("dependence unit subdivided after a reproducible backend crash", "component", component,
		"unit", string(req.Unit.ID), "scope", unit.ScopeKey, "pass", crash.Pass, "exception", crash.Exception,
		"children", len(children))
	// Every part imports into the one sink storage opened for the unit. Two
	// parts legitimately describe the same entity — above all the external
	// stub of a callee both parts reference — and storage now admits a
	// repeated node identity whose stored columns are identical and refuses
	// one whose columns diverge (CTX_PROVIDER_OUTPUT_INVALID). The repeat is
	// therefore neither dropped here nor absorbed there: identical repeats
	// cost nothing and a divergence, which would mean the parts disagree
	// about one entity, fails the unit where it can be seen.
	var total ImportReport
	var admitted int
	for i, child := range children {
		graph := run.path("graph-" + itoa(int64(i)))
		out, err := p.parse(ctx, req, unit, res, filepath.Join(source, child), graph, nil)
		if err != nil {
			return ImportReport{}, 0, err
		}
		if out.Class != FailureNone {
			continue
		}
		dir := run.path("export-" + itoa(int64(i)))
		timeout, err := remaining(ctx)
		if err != nil {
			return ImportReport{}, 0, err
		}
		exp, err := p.backend.Export(ctx, ExportRequest{GraphPath: graph, OutputDir: dir,
			HeapCapBytes: res.ExportHeapCapBytes, ReservationBytes: res.ExportBytes(), Timeout: timeout, StallTimeout: p.opts.StallTimeout})
		if err != nil {
			return ImportReport{}, 0, err
		}
		if exp.Class != FailureNone || !exp.Live {
			continue
		}
		// No delta options: a part's key set is a subset of the unit's, so
		// diffing one against the whole unit's previous set would report
		// every other part's keys removed and carry nothing. Every part
		// imports in full and merge reports the absent set, which makes the
		// next refresh of a subdivided unit a full import.
		report, err := p.importExport(ctx, req, unit, filepath.Join(source, child),
			path.Join(unit.Root, child), dir, sink, ImportOptions{})
		if err != nil {
			return ImportReport{}, 0, err
		}
		total = merge(total, report)
		admitted++
		_ = os.Remove(graph)
	}
	if admitted == 0 {
		return ImportReport{}, 0, failure(FailureEngine, unit.ScopeKey, crash, res).
			WithDetail("reason", "no subdivided part of the unit produced an honest result")
	}
	return total, overParts, nil
}

// childProjects lists the immediate subdirectories of a unit's root that hold
// source, in a stable order. That is the next frontend-native boundary below a
// project: the project's own top-level packages or source directories.
//
// Every one of them is returned. A truncated list would drop whole parts of a
// crashed unit's source from the only run that can still analyse it, and would
// do it without a word: the unit would seal, partial for the subdivision, with
// no sign that the tail of its source was never parsed. The caller reports the
// count against the user's threshold instead.
func childProjects(source string, unit Unit) ([]string, error) {
	entries, err := os.ReadDir(source)
	if err != nil {
		return nil, internalErr("the dependence unit could not be subdivided: " + err.Error())
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		out = append(out, e.Name())
	}
	slices.Sort(out)
	if len(out) == 0 {
		return nil, failure(FailureEngine, unit.ScopeKey, Outcome{}, Reservation{}).
			WithDetail("reason", "the unit has no boundary below it to split along")
	}
	return out, nil
}

// merge accumulates the counts of one subdivided unit's parts. Every counter
// the importer reports is summed: a figure that carried only the last part's
// value would understate what the unit dropped, could not locate or refused,
// which is exactly what those counters exist to make non-silent.
//
// Keys is deliberately not merged. A key set is one file on disk and the
// unit's set is the union of its parts', which cannot be formed by addition;
// a subdivided unit therefore reports the absent set, and the next refresh of
// it is a full import. Inventing one part's set as the unit's would mis-diff
// the whole unit at the next refresh.
func merge(a, b ImportReport) ImportReport {
	a.Nodes += b.Nodes
	a.Relations += b.Relations
	a.Aliases += b.Aliases
	a.ExternalMethods += b.ExternalMethods
	a.Changed += b.Changed
	a.Unchanged += b.Unchanged
	a.Removed += b.Removed
	a.DroppedMethods += b.DroppedMethods
	a.UnlocatedFacts += b.UnlocatedFacts
	a.UnresolvedWrites += b.UnresolvedWrites
	a.ClippedEvidence += b.ClippedEvidence
	a.UnknownRows += b.UnknownRows
	a.IgnoredFiles += b.IgnoredFiles
	a.BytesRead += b.BytesRead
	// Each part stages into its own scratch and compares its own rows against
	// the threshold, so three parts of ten rows each cross a bound of fifteen
	// that none of them crossed alone. The unit's count is the sum, and the
	// caller compares that; the per-part flag is carried too, so a part that
	// crossed on its own is never lost behind a sum.
	a.StagedRows += b.StagedRows
	a.OverStagedRows = a.OverStagedRows || b.OverStagedRows
	a.DerivedRows += b.DerivedRows
	a.OverDerivedRows = a.OverDerivedRows || b.OverDerivedRows
	a.Keys = neo4jcsv.KeySet{}
	if b.UnknownLabels != nil {
		if a.UnknownLabels == nil {
			a.UnknownLabels = map[string]int{}
		}
		for l, n := range b.UnknownLabels {
			a.UnknownLabels[l] += n
		}
	}
	return a
}

// unitSource is the directory of a unit's materialization that the engine
// reads. The materialization already holds only this unit's files, so the only
// thing left to establish is that the project directory is actually there: an
// absent one would otherwise be handed to the engine as a missing path or, for
// the repository root, as an empty tree that parses to an empty graph
// indistinguishable from a crashed frontend helper.
func unitSource(root string, unit Unit) (string, error) {
	dir := root
	if unit.Root != "" {
		dir = filepath.Join(root, filepath.FromSlash(unit.Root))
	}
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return "", invalid("the dependence unit's project directory is not in the snapshot: " + truncate(unit.Root, 128))
	}
	return dir, nil
}

// backendFailure renders the crash a subdivided unit publishes: the failing
// pass and its exception class, which is what a maintainer needs to report the
// crash upstream.
func backendFailure(o Outcome) string {
	switch {
	case o.Pass != "" && o.Exception != "":
		return o.Pass + "/" + o.Exception
	case o.Pass != "":
		return o.Pass
	default:
		return o.Exception
	}
}

// remaining is the time left on the unit's deadline, or a typed timeout when
// too little is left to start a step. Starting an analyzer with a second to live
// wastes the second and reports a timeout anyway. A unit with no deadline is
// the configured unlimited case: every step is started with no wall clock and
// bounded by its stall detector instead.
func remaining(ctx context.Context) (time.Duration, error) {
	deadline, ok := ctx.Deadline()
	if !ok {
		return 0, nil
	}
	left := time.Until(deadline)
	if left < minStepTimeout {
		return 0, &model.Error{Code: model.CodeProviderTimeout,
			Message: "the dependence unit's deadline expired before the next analysis step could start"}
	}
	return left, nil
}
