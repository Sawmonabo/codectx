package index

// The single incremental scenario of Task 12 Step 1, carried across the eight
// cases the brief names: no-op reuse, ten changed files, a new symbol and
// dependency, a delete and a rename, an optional provider's failure, a
// required provider's failure, a concurrent refresh, and a watcher overflow.
// A ninth test covers the carry-distance paging the coordinator feeds the
// planner.
//
// Failure modes these protect, all of which are silent:
//   - a refresh that rebuilds units nothing changed, rewriting lexical bodies
//     and destroying the reuse Section 13.1 promises (asserted by comparing
//     sealed unit ids and the generation's FTS document count across runs);
//   - a refresh that reuses a unit whose file did change, so a query answers
//     from source that is gone;
//   - an optional provider's failure taking a healthy base generation down,
//     or a required provider's failure publishing a generation anyway;
//   - two concurrent refreshes publishing over one another's unseen pointer;
//   - a watch overflow dropping the paths it collapsed, so the index reports
//     coverage it does not have;
//   - a carry fold that reads one page of the previous generation's stale
//     members, reporting a unit stale for many generations as stale for one.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Sawmonabo/codectx/internal/config"
	"github.com/Sawmonabo/codectx/internal/index/plan"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/process"
	"github.com/Sawmonabo/codectx/internal/provider"
	"github.com/Sawmonabo/codectx/internal/provider/filesystem"
	"github.com/Sawmonabo/codectx/internal/provider/manifest"
	"github.com/Sawmonabo/codectx/internal/provider/scip"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter"
	tslang "github.com/Sawmonabo/codectx/internal/provider/treesitter/lang"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/wire"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/worker"
	"github.com/Sawmonabo/codectx/internal/snapshot"
	"github.com/Sawmonabo/codectx/internal/storage/sqlite"
	"github.com/Sawmonabo/codectx/internal/workspace"
)

// TestMain makes this test binary the parser worker, as the codectx binary is
// in production.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == wire.Subcommand {
		os.Exit(worker.Main(context.Background(), os.Stdin, os.Stdout, os.Stderr))
	}
	os.Exit(m.Run())
}

// fixture is one workspace, one store and one coordinator over real
// providers: the filesystem and manifest providers are required, the
// tree-sitter provider parses, and a supplied SCIP index is the optional
// provider the failure case needs.
type fixture struct {
	t       *testing.T
	ctx     context.Context
	repoDir string
	dataDir string
	store   *sqlite.Store
	cas     *snapshot.CAS
	lock    *snapshot.WorkspaceLock
	cfg     config.Config
	c       *Coordinator
}

func newFixture(t *testing.T, files map[string]string) *fixture {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	f := &fixture{t: t, ctx: ctx, repoDir: filepath.Join(dir, "repo"), dataDir: filepath.Join(dir, "data")}
	if err := os.MkdirAll(f.repoDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for path, content := range files {
		f.write(path, content)
	}
	cfg := config.Defaults()
	cfg.Storage.DataDir = f.dataDir
	cfg.Providers.SCIP.Enabled = config.Disabled
	cfg.Providers.LSP.Enabled = config.Disabled
	cfg.Providers.Dependence.Enabled = config.Disabled
	f.cfg = cfg

	cas, err := snapshot.OpenCAS(snapshot.CASDir(f.dataDir))
	if err != nil {
		t.Fatalf("OpenCAS: %v", err)
	}
	f.cas = cas
	// Carried lexical documents are re-indexed through the content store's
	// range reader: the database keeps no body (ADR-0003 §2.1).
	store, err := sqlite.Open(ctx, filepath.Join(f.dataDir, "codectx.db"), sqlite.Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	f.store = store
	if err := store.Recover(ctx, time.Now()); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	// One lock per workspace, taken once and handed to every coordinator the
	// scenario builds: it is the cross-process owner, and a second acquisition
	// in this process is refused exactly as another process would be.
	if f.lock, err = snapshot.LockWorkspace(ctx, f.dataDir, 0); err != nil {
		t.Fatalf("LockWorkspace: %v", err)
	}
	t.Cleanup(func() { f.lock.Close() })
	f.c = f.coordinator(f.providers(false))
	return f
}

// providers builds the registry. withSCIP adds the optional provider over a
// supplied index; withoutFilesystem is the required-failure case and is built
// by the caller.
func (f *fixture) providers(withSCIP bool) []provider.Provider {
	f.t.Helper()
	fs, err := filesystem.New(filesystem.Options{MaxSearchFileBytes: f.cfg.Workspace.MaxSearchFileBytes})
	if err != nil {
		f.t.Fatal(err)
	}
	mf, err := manifest.New(manifest.Options{MaxParseFileBytes: f.cfg.Workspace.MaxParseFileBytes})
	if err != nil {
		f.t.Fatal(err)
	}
	out := []provider.Provider{fs, mf, f.treesitter()}
	if withSCIP {
		p, err := scip.New(f.ctx, scip.Options{Import: "index.scip", WorkDir: filepath.Join(f.dataDir, "scip")})
		if err != nil {
			f.t.Fatal(err)
		}
		out = append(out, p)
	}
	return out
}

func (f *fixture) treesitter() provider.Provider {
	f.t.Helper()
	exe, err := os.Executable()
	if err != nil {
		f.t.Fatal(err)
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		f.t.Fatal(err)
	}
	runner, err := process.NewRunner(process.Limits{MaxConcurrent: 2, MemoryBudgetBytes: 2 << 30, DiskBudgetBytes: 1 << 30})
	if err != nil {
		f.t.Fatal(err)
	}
	workDir := filepath.Join(f.dataDir, "parsers")
	if err := os.MkdirAll(workDir, 0o700); err != nil {
		f.t.Fatal(err)
	}
	p, err := treesitter.New(treesitter.Options{MaxWorkers: 2, MaxParseFileBytes: f.cfg.Workspace.MaxParseFileBytes,
		WorkerIdleTTL: time.Minute, ParseTimeout: time.Minute, WorkerMemoryBytes: 256 << 20,
		Worker: treesitter.WorkerCommand{Path: exe, Args: []string{wire.Subcommand}}, Runner: runner, WorkDir: workDir})
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { p.Close() })
	return p
}

// coordinator opens a workspace root, takes the workspace lock and builds the
// coordinator, exactly as the composition root does.
func (f *fixture) coordinator(providers []provider.Provider) *Coordinator {
	f.t.Helper()
	root, err := workspace.Discover(f.repoDir)
	if err != nil {
		f.t.Fatalf("Discover: %v", err)
	}
	f.t.Cleanup(func() { root.Close() })
	registry, err := provider.NewRegistry(providers...)
	if err != nil {
		f.t.Fatalf("NewRegistry: %v", err)
	}
	pool, err := provider.NewPool(f.cfg.Index.QueueBytes)
	if err != nil {
		f.t.Fatal(err)
	}
	c, err := New(Options{Root: root, Config: f.cfg, Store: f.store, Registry: registry, CAS: f.cas,
		Lock: f.lock, Pool: pool})
	if err != nil {
		f.t.Fatalf("New: %v", err)
	}
	f.t.Cleanup(func() { c.Close() })
	return c
}

func (f *fixture) write(path, content string) {
	f.t.Helper()
	abs := filepath.Join(f.repoDir, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(abs, []byte(content), 0o600); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) remove(path string) {
	f.t.Helper()
	if err := os.Remove(filepath.Join(f.repoDir, filepath.FromSlash(path))); err != nil {
		f.t.Fatal(err)
	}
}

// units is the sealed unit every file provider selected for every path, keyed
// "providerID\x00scopeKey". It is the comparison that proves a unit was not
// rebuilt: an immutable unit's identity is its inputs, so an unchanged unit
// id is an unchanged unit.
func (f *fixture) units(gen model.GenerationID, paths []string) map[string]model.UnitID {
	f.t.Helper()
	out := map[string]model.UnitID{}
	for _, id := range []string{filesystem.ID, manifest.ID, tslang.ProviderID} {
		for _, p := range paths {
			unit, err := f.store.SelectedUnit(f.ctx, gen, id, filesystem.ScopeKey(p))
			if err != nil {
				continue
			}
			out[id+"\x00"+p] = unit
		}
	}
	return out
}

// documents is the generation's FTS document count: the lexical bodies a
// refresh must not rewrite.
func (f *fixture) documents(gen model.GenerationID) int64 {
	f.t.Helper()
	pinned, err := f.store.PinGeneration(f.ctx, f.c.repo, gen, time.Minute)
	if err != nil {
		f.t.Fatalf("PinGeneration: %v", err)
	}
	defer pinned.Close()
	docs, _, err := pinned.SearchStats(f.ctx)
	if err != nil {
		f.t.Fatalf("SearchStats: %v", err)
	}
	return docs
}

// goFile is one fixture source file with an optional extra declaration.
func goFile(pkg, name, extra string) string {
	return "package " + pkg + "\n\nfunc " + name + "() int { return 1 }\n" + extra
}

func scenarioFiles() map[string]string {
	files := map[string]string{
		"go.mod":    "module example.com/fixture\n\ngo 1.27\n",
		"README.md": "# fixture\n",
	}
	for i := 0; i < 12; i++ {
		files[fmt.Sprintf("pkg/f%02d.go", i)] = goFile("pkg", fmt.Sprintf("F%02d", i), "")
	}
	return files
}

func paths(files map[string]string) []string {
	out := make([]string, 0, len(files))
	for p := range files {
		out = append(out, p)
	}
	return out
}

func TestIncrementalScenario(t *testing.T) {
	files := scenarioFiles()
	f := newFixture(t, files)
	all := paths(files)
	ctx := f.ctx

	cold, err := f.c.Index(ctx, model.IndexRequest{})
	if err != nil {
		t.Fatalf("cold index: %v", err)
	}
	if cold.UnitsBuilt == 0 || cold.UnitsReused != 0 {
		t.Fatalf("cold index built %d and reused %d units; a first index builds everything",
			cold.UnitsBuilt, cold.UnitsReused)
	}
	if cold.Health != model.HealthFresh {
		t.Fatalf("cold index health %q, want fresh", cold.Health)
	}
	// The result is a contract value: a capability list or a reason the
	// coordinator assembles past its own bounds is a defect here, not in the
	// caller that later fails to serialize it.
	if err := cold.Validate(); err != nil {
		t.Fatalf("the cold index result does not satisfy its own contract: %v", err)
	}
	if int(cold.FilesCaptured) != len(files) {
		t.Fatalf("captured %d files, want %d", cold.FilesCaptured, len(files))
	}
	base := f.units(cold.Binding.GenerationID, all)
	baseDocs := f.documents(cold.Binding.GenerationID)
	if len(base) == 0 {
		t.Fatal("the cold generation selected no units")
	}

	t.Run("no-op refresh reuses everything and parses nothing", func(t *testing.T) {
		res, err := f.c.Refresh(ctx, nil)
		if err != nil {
			t.Fatalf("refresh: %v", err)
		}
		if res.UnitsBuilt != 0 || res.FilesParsed != 0 {
			t.Fatalf("a no-op refresh built %d units and parsed %d files; it must do neither",
				res.UnitsBuilt, res.FilesParsed)
		}
		if res.UnitsReused != cold.UnitsBuilt {
			t.Fatalf("reused %d of %d units", res.UnitsReused, cold.UnitsBuilt)
		}
		after := f.units(res.Binding.GenerationID, all)
		assertSameUnits(t, base, after, nil)
		if docs := f.documents(res.Binding.GenerationID); docs != baseDocs {
			t.Fatalf("the FTS corpus moved from %d to %d documents on a no-op refresh", baseDocs, docs)
		}
	})

	changed := make([]string, 0, 10)
	t.Run("ten changed files rebuild only their own units", func(t *testing.T) {
		for i := 0; i < 10; i++ {
			p := fmt.Sprintf("pkg/f%02d.go", i)
			changed = append(changed, p)
			f.write(p, goFile("pkg", fmt.Sprintf("F%02d", i), "\n// edited\n"))
		}
		res, err := f.c.Refresh(ctx, changed)
		if err != nil {
			t.Fatalf("refresh: %v", err)
		}
		if res.UnitsBuilt == 0 {
			t.Fatal("ten changed files rebuilt nothing")
		}
		if res.UnitsInvalidated == 0 {
			t.Fatal("ten changed files invalidated no previously reusable unit")
		}
		after := f.units(res.Binding.GenerationID, all)
		assertSameUnits(t, base, after, changed)
		assertRebuilt(t, base, after, changed)
		base, baseDocs = after, f.documents(res.Binding.GenerationID)
	})

	t.Run("a new symbol and dependency rebuild one file's units", func(t *testing.T) {
		p := "pkg/f11.go"
		f.write(p, "package pkg\n\nimport \"strings\"\n\nfunc F11() int { return len(strings.TrimSpace(\" x \")) }\n\nfunc Added() int { return 2 }\n")
		res, err := f.c.Refresh(ctx, []string{p})
		if err != nil {
			t.Fatalf("refresh: %v", err)
		}
		after := f.units(res.Binding.GenerationID, all)
		assertSameUnits(t, base, after, []string{p})
		assertRebuilt(t, base, after, []string{p})
		if docs := f.documents(res.Binding.GenerationID); docs <= baseDocs {
			t.Fatalf("a new declaration did not reach the lexical corpus: %d documents, was %d", docs, baseDocs)
		}
		base, baseDocs = after, f.documents(res.Binding.GenerationID)
	})

	t.Run("a delete and a rename move exactly their own units", func(t *testing.T) {
		f.remove("pkg/f00.go")
		f.remove("pkg/f01.go")
		renamed := "pkg/renamed.go"
		f.write(renamed, goFile("pkg", "F01", ""))
		res, err := f.c.Refresh(ctx, []string{"pkg/f00.go", "pkg/f01.go", renamed})
		if err != nil {
			t.Fatalf("refresh: %v", err)
		}
		gen := res.Binding.GenerationID
		for _, gone := range []string{"pkg/f00.go", "pkg/f01.go"} {
			if _, err := f.store.SelectedUnit(ctx, gen, filesystem.ID, filesystem.ScopeKey(gone)); err == nil {
				t.Fatalf("the generation still selects a unit for the deleted %s", gone)
			}
		}
		if _, err := f.store.SelectedUnit(ctx, gen, filesystem.ID, filesystem.ScopeKey(renamed)); err != nil {
			t.Fatalf("the renamed file has no unit: %v", err)
		}
		all = append(all, renamed)
		after := f.units(gen, all)
		assertSameUnits(t, base, after, []string{"pkg/f00.go", "pkg/f01.go", renamed})
		base, baseDocs = after, f.documents(gen)
	})

	t.Run("a concurrent refresh publishes one generation at a time", func(t *testing.T) {
		var wg sync.WaitGroup
		results := make([]model.IndexResult, 2)
		errs := make([]error, 2)
		for i := range results {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				results[i], errs[i] = f.c.Refresh(ctx, nil)
			}(i)
		}
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Fatalf("concurrent refresh %d: %v", i, err)
			}
		}
		if results[0].Binding.GenerationID == results[1].Binding.GenerationID {
			t.Fatal("two concurrent refreshes published the same generation")
		}
		active, err := f.store.ActiveGeneration(ctx, f.c.repo)
		if err != nil {
			t.Fatalf("ActiveGeneration: %v", err)
		}
		if active != max(results[0].Binding.GenerationID, results[1].Binding.GenerationID) {
			t.Fatalf("the active generation is %d, not the last published one", active)
		}
		base = f.units(active, all)
	})

	t.Run("a watch overflow collapses to a full reconciliation that drops nothing", func(t *testing.T) {
		p := "pkg/f05.go"
		f.write(p, goFile("pkg", "F05", "\n// overflowed\n"))
		// An overflow batch names no paths at all: it means "reconcile
		// everything". The change must still be found, because the capture and
		// the content hashes are the truth and the hint never was.
		//
		// Refresh ignores the hint by construction today, so this leg executes
		// the same code as the leg above: it is a forward guard, not live
		// coverage. It fails the day someone narrows the capture to the hint,
		// which is exactly when an overflow would start dropping the changes it
		// collapsed.
		res, err := f.c.Refresh(ctx, nil)
		if err != nil {
			t.Fatalf("refresh: %v", err)
		}
		after := f.units(res.Binding.GenerationID, all)
		assertRebuilt(t, base, after, []string{p})
		assertSameUnits(t, base, after, []string{p})
	})

	t.Run("status validates while a watch is running", func(t *testing.T) {
		// The watch projection adds a warning no other path produces, so this
		// is the only configuration in which Status can exceed its bounds.
		watchCtx, stop := context.WithCancel(ctx)
		defer stop()
		done := make(chan error, 1)
		go func() { done <- f.c.Watch(watchCtx, nil) }()
		var st model.IndexStatus
		for deadline := time.Now().Add(10 * time.Second); ; {
			var err error
			if st, err = f.c.Status(ctx); err != nil {
				t.Fatalf("status: %v", err)
			}
			if st.WatchActive || time.Now().After(deadline) {
				break
			}
			time.Sleep(2 * time.Millisecond)
		}
		if !st.WatchActive {
			t.Fatal("the watch loop never reported itself active")
		}
		if st.WatchComplete {
			t.Fatal("periodic reconciliation reported complete notification coverage")
		}
		if err := st.Validate(); err != nil {
			t.Fatalf("the status does not satisfy its own contract: %v", err)
		}
		stop()
		if err := <-done; err != nil {
			t.Fatalf("watch: %v", err)
		}
	})

	t.Run("a deferred publication outlives the activation that intervened while its units built", func(t *testing.T) {
		// Failure mode: the background tick reads the active generation when
		// it starts and the publication pins that id minutes later. An
		// ordinary reconciliation in between supersedes it and retention
		// deletes it, so the pin fails and the whole batch of sealed units is
		// thrown away and collected -- under the shipped
		// `providers.dependence.enabled = "auto"` default, every time.
		gen, err := f.store.ActiveGeneration(ctx, f.c.repo)
		if err != nil {
			t.Fatalf("ActiveGeneration: %v", err)
		}
		pinned, err := f.store.PinGeneration(ctx, f.c.repo, gen, statusLeaseTTL)
		if err != nil {
			t.Fatalf("PinGeneration: %v", err)
		}
		snap := pinned.Binding().SnapshotID
		pinned.Close()
		sel, err := f.c.opts.Registry.Select(ctx, f.c.opts.Root, f.c.policy, f.c.enablement)
		if err != nil {
			t.Fatalf("Select: %v", err)
		}
		// The reconciliation the tick does not see. It activates over gen and
		// retention collects it, exactly as the 30 s default interval does
		// while a dependence unit runs.
		if _, err := f.c.Refresh(ctx, nil); err != nil {
			t.Fatalf("refresh: %v", err)
		}
		if _, err := f.store.PinGeneration(ctx, f.c.repo, gen, statusLeaseTTL); err == nil {
			t.Fatal("the superseded generation still exists; this leg is not exercising the race")
		}
		// A publication with nothing to replace still pins and plans, which is
		// the whole of the path that failed: it must reach "nothing to
		// publish" over the generation that superseded gen, not
		// CTX_ARGUMENT_INVALID over the one that is gone.
		if _, _, err = f.c.late.publish(ctx, snap, sel, refNone, nil); err != nil {
			t.Fatalf("the deferred publication pinned a stale generation: %v", err)
		}
	})

	t.Run("an optional provider's failure degrades its capability only", func(t *testing.T) {
		// A supplied index that is not a SCIP index at all: the optional
		// provider plans its unit and cannot produce it.
		f.write("index.scip", "this is not a scip index\n")
		opt := f.coordinator(f.providers(true))
		res, err := opt.Index(ctx, model.IndexRequest{})
		if err != nil {
			t.Fatalf("index with a broken optional provider: %v", err)
		}
		if res.Health == model.HealthFailed {
			t.Fatal("an optional provider's failure failed the whole generation")
		}
		var sawScip bool
		for _, s := range res.Completeness {
			if s.ProviderID != scip.ID {
				continue
			}
			sawScip = true
			if s.State == model.CapabilityFresh {
				t.Fatalf("the broken optional provider reported %q", s.State)
			}
		}
		if !sawScip {
			t.Fatal("the broken optional provider published no capability row at all")
		}
		for _, s := range res.Completeness {
			if s.ProviderID == filesystem.ID && s.State != model.CapabilityFresh {
				t.Fatalf("the base provider was reported %q because an optional one failed", s.State)
			}
		}
		f.remove("index.scip")
	})

	t.Run("an optional provider's failure rows key on the exemplar scope, not on arrival order", func(t *testing.T) {
		// The units of one provider capability are built concurrently, and
		// the published row's scope_key AND diagnostic_code both fold into
		// details_json/diagnostic_code, which fold into the AnalysisKey. Two
		// identical runs whose failures merely arrived in a different order
		// must therefore publish the identical row; an arrival-ordered
		// exemplar keys them apart and breaks reuse for every consumer.
		failures := []struct{ scope, code string }{
			{"pkg:go:", model.CodeProviderTimeout},
			{"pkg:java:", model.CodeProviderOutputInvalid},
		}
		report := func(order []int) model.CapabilityState {
			r := newCapabilityReport()
			for _, i := range order {
				r.addFailure(scip.ID, "references", failures[i].scope, failures[i].code)
			}
			states := r.finish(f.c.log)
			for _, st := range states {
				if st.ProviderID == scip.ID {
					return st
				}
			}
			t.Fatal("the failure fold published no row")
			return model.CapabilityState{}
		}
		forward, reverse := report([]int{0, 1}), report([]int{1, 0})
		if forward.DiagnosticCode != reverse.DiagnosticCode {
			t.Fatalf("the published diagnostic code depends on arrival order: %q then %q",
				forward.DiagnosticCode, reverse.DiagnosticCode)
		}
		if forward.Details["scope_key"] != reverse.Details["scope_key"] {
			t.Fatalf("the published scope key depends on arrival order: %q then %q",
				forward.Details["scope_key"], reverse.Details["scope_key"])
		}
		// The code must be the chosen scope's own: a row naming one scope and
		// another scope's reason is arrival-ordered again, and misreports.
		if forward.Details["scope_key"] != failures[0].scope || forward.DiagnosticCode != failures[0].code {
			t.Fatalf("the exemplar row is scope %q code %q, want the lexicographically first scope %q and its code %q",
				forward.Details["scope_key"], forward.DiagnosticCode, failures[0].scope, failures[0].code)
		}
	})

	t.Run("one provider capability publishes one row per scope key", func(t *testing.T) {
		// Failure mode: generation_capabilities is keyed (generation,
		// provider, capability, scope_key) with no state column, while the
		// report keys its rows by state as well. A provider capability that
		// reaches the publication with a sibling unit's fresh row folded to
		// the workspace scope, a workspace-scoped partial degradation, a
		// failed unit and a carried workspace scope publishes four rows under
		// one primary key: the insert is rejected and `codectx index` exits on
		// a constraint error instead of publishing the degraded generation.
		r := newCapabilityReport()
		r.add(model.CapabilityState{ProviderID: scip.ID, Capability: "references",
			Scope: "pkg:go:", State: model.CapabilityFresh})
		r.add(model.CapabilityState{ProviderID: scip.ID, Capability: "references",
			Scope: provider.ScopeWorkspace, State: model.CapabilityPartial,
			DiagnosticCode: model.CodeProviderOutputInvalid})
		r.addCarried(scip.ID, "references", provider.ScopeWorkspace, 1, 2)
		r.addFailure(scip.ID, "references", "pkg:java:", model.CodeProviderTimeout)
		states := r.finish(f.c.log)
		var rows []model.CapabilityState
		for _, s := range states {
			if s.ProviderID == scip.ID && s.Capability == "references" {
				rows = append(rows, s)
			}
		}
		if len(rows) != 1 {
			t.Fatalf("one provider capability published %d rows for one scope key: %+v", len(rows), rows)
		}
		// Under-claim, never over-claim: the most severe row is the survivor.
		if rows[0].State != model.CapabilityFailed || rows[0].Scope != provider.ScopeWorkspace {
			t.Fatalf("the surviving row is %q at scope %q, want failed at the workspace scope",
				rows[0].State, rows[0].Scope)
		}

	})

	t.Run("a required provider's unit failure publishes nothing", func(t *testing.T) {
		before, err := f.store.ActiveGeneration(ctx, f.c.repo)
		if err != nil {
			t.Fatal(err)
		}
		// A new file the required filesystem provider cannot index. Its unit
		// has no sealed predecessor to attach, so the run must build it, and
		// its failure must take the whole generation with it.
		f.write("pkg/broken.go", goFile("pkg", "Broken", ""))
		providers := f.providers(false)
		providers[0] = failingProvider{Provider: providers[0], scope: filesystem.ScopeKey("pkg/broken.go")}
		broken := f.coordinator(providers)
		if _, err := broken.Index(ctx, model.IndexRequest{}); err == nil {
			t.Fatal("a required provider's unit failed and the generation was published anyway")
		}
		after, err := f.store.ActiveGeneration(ctx, f.c.repo)
		if err != nil {
			t.Fatal(err)
		}
		if after != before {
			t.Fatalf("the active generation moved from %d to %d although the run failed", before, after)
		}
		f.remove("pkg/broken.go")
	})
}

// failingProvider is one real provider whose unit for a named scope fails. It
// is the only way to exercise a required provider's unit failure: every
// required provider in the product succeeds on a readable file, and a registry
// without one is refused at construction rather than at index time.
type failingProvider struct {
	provider.Provider
	scope string
}

func (p failingProvider) IndexUnit(ctx context.Context, req provider.UnitRequest, sink provider.Sink) (model.ProviderResult, error) {
	if req.Unit.ScopeKey == p.scope {
		return model.ProviderResult{RunID: req.Run, State: model.RunFailed},
			&model.Error{Code: model.CodeProviderOutputInvalid, Message: "the fixture made this required unit fail"}
	}
	return p.Provider.IndexUnit(ctx, req, sink)
}

// assertSameUnits requires every scope outside changed to hold the same unit
// it held before: the reuse Section 13.1 promises, proven by identity.
func assertSameUnits(t *testing.T, before, after map[string]model.UnitID, changed []string) {
	t.Helper()
	for key, unit := range before {
		if changedKey(key, changed) {
			continue
		}
		if got, ok := after[key]; !ok {
			t.Fatalf("%s lost its unit", key)
		} else if got != unit {
			t.Fatalf("%s was rebuilt: unit %s became %s", key, unit, got)
		}
	}
}

// assertRebuilt requires every changed path's units to be different ones.
func assertRebuilt(t *testing.T, before, after map[string]model.UnitID, changed []string) {
	t.Helper()
	for key, unit := range before {
		if !changedKey(key, changed) {
			continue
		}
		if got, ok := after[key]; ok && got == unit {
			t.Fatalf("%s kept unit %s although its file changed", key, unit)
		}
	}
}

func changedKey(key string, changed []string) bool {
	_, path, _ := strings.Cut(key, "\x00")
	for _, c := range changed {
		if c == path {
			return true
		}
	}
	return false
}

// TestCarryDistancesPageToTheEnd proves the coordinator feeds the planner the
// whole stale membership of the previous generation, not its first page.
//
// Failure mode: a carry fold that stops at one page reports every scope past
// it as never carried, so a unit stale for ten generations answers `stale` with
// a provenance distance of one -- a freshness claim the source does not
// support. The store caps a page at model.MaxPageItems and the planner ends its
// fold on a short page, so a fixture with one more carried unit than that cap
// is what makes the loop run at all.
func TestCarryDistancesPageToTheEnd(t *testing.T) {
	const carried = model.MaxPageItems + 1
	f := newFixture(t, map[string]string{
		"go.mod":     "module example.com/carry\n\ngo 1.27\n",
		"pkg/one.go": goFile("pkg", "One", ""),
	})
	ctx := f.ctx
	cold, err := f.c.Index(ctx, model.IndexRequest{})
	if err != nil {
		t.Fatalf("cold index: %v", err)
	}
	snap := cold.Binding.SnapshotID
	fv, err := f.store.SnapshotFile(ctx, snap, model.NewFileID(f.c.repo, "pkg/one.go"))
	if err != nil {
		t.Fatalf("SnapshotFile: %v", err)
	}
	caps := []model.CapabilityState{{ProviderID: "dependence", Capability: "calls",
		Scope: provider.ScopeWorkspace, State: model.CapabilityFresh}}

	// A generation of sealed units, so there is something to carry.
	sealed := make([]model.UnitID, 0, carried)
	gen, err := f.store.BeginGeneration(ctx, f.c.repo, snap, f.c.cfgHash, "refs/heads/carry")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < carried; i++ {
		u := plan.Unit{ProviderID: "dependence", ProviderVersion: "v1", ScopeKey: fmt.Sprintf("project:%04d", i),
			InputCount: 1, Inputs: func(yield func(model.UnitInput) error) error {
				return yield(model.UnitInput{FileID: fv.ID, ContentHash: fv.ContentHash, Executable: fv.Executable})
			}}
		spec, err := u.Spec(f.c.cfgHash)
		if err != nil {
			t.Fatal(err)
		}
		run, err := f.store.BeginProviderRun(ctx, gen, u.ProviderID, u.ProviderVersion)
		if err != nil {
			t.Fatal(err)
		}
		build := model.UnitBuild{Spec: spec, AnalysisConfigHash: f.c.cfgHash, OriginRunID: run,
			SourceBinding: model.SourceBindingVerified}
		w, err := f.store.BeginUnit(ctx, gen, build, u.Inputs)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.store.SealUnit(ctx, w); err != nil {
			t.Fatal(err)
		}
		sealed = append(sealed, spec.ID)
	}
	if _, err := f.store.Activate(ctx, gen, cold.Binding.GenerationID, model.HealthFresh, caps, NormalizationVersion); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	// The generation that carries every one of them as stale.
	stale, err := f.store.BeginGeneration(ctx, f.c.repo, snap, f.c.cfgHash, "refs/heads/carry")
	if err != nil {
		t.Fatal(err)
	}
	for _, unit := range sealed {
		if err := f.store.AttachCarried(ctx, stale, unit, sqlite.Carry{DistanceGenerations: 3, DistanceFiles: 7}); err != nil {
			t.Fatalf("AttachCarried: %v", err)
		}
	}
	if _, err := f.store.Activate(ctx, stale, gen, model.HealthDegraded, caps, NormalizationVersion); err != nil {
		t.Fatalf("Activate: %v", err)
	}

	view, err := snapshot.OpenView(ctx, f.store, f.cas, snap)
	if err != nil {
		t.Fatal(err)
	}
	sel, err := f.c.opts.Registry.Select(ctx, f.c.opts.Root, f.c.policy, f.c.enablement)
	if err != nil {
		t.Fatal(err)
	}
	page := f.c.carriedPage(stale)
	var pages, rows int
	counted := func(ctx context.Context, afterProviderID, afterScopeKey string, limit int) ([]plan.Carried, error) {
		got, err := page(ctx, afterProviderID, afterScopeKey, limit)
		pages++
		rows += len(got)
		for _, c := range got {
			if c.DistanceGenerations != 3 || c.DistanceFiles != 7 {
				t.Errorf("%s carried distance %d/%d, want 3/7", c.ScopeKey, c.DistanceGenerations, c.DistanceFiles)
			}
		}
		return got, err
	}
	if _, err := plan.Build(ctx, plan.Inputs{View: view, Selection: sel, Store: f.store, PrevGen: stale,
		CarriedPage: counted, Config: f.cfg}); err != nil {
		t.Fatalf("plan.Build: %v", err)
	}
	if pages < 2 {
		t.Fatalf("the carry fold read %d page(s); %d carried units cannot fit one page of %d",
			pages, carried, model.MaxPageItems)
	}
	if rows != carried {
		t.Fatalf("the carry fold saw %d of %d carried units", rows, carried)
	}
}

// TestDeferredUnitsAreNotCoverage guards the two false-readiness invariants of
// Section 11.6 that only the `auto` configuration reaches. Neither can be
// driven end to end here: plan.Build marks a unit heavy -- and therefore
// deferrable -- for the dependence provider alone, and that provider needs the
// real analysis engine. So the coverage projection and the background queue are
// exercised directly, which is where both defects lived.
func TestDeferredUnitsAreNotCoverage(t *testing.T) {
	f := newFixture(t, map[string]string{"pkg/a.go": goFile("pkg", "A", "")})
	ctx := f.ctx
	sel, err := f.c.opts.Registry.Select(ctx, f.c.opts.Root, f.c.policy, f.c.enablement)
	if err != nil {
		t.Fatal(err)
	}
	if len(sel.Active) == 0 {
		t.Fatal("detection selected no provider")
	}
	d := sel.Active[0].Descriptor()

	// A generation whose only planned unit for this provider is deferred holds
	// no member for it: the unit seals into a later publication.
	g := &generation{c: f.c, caps: newCapabilityReport(), sel: sel,
		plan: plan.Plan{Units: oneUnit(plan.Unit{ProviderID: d.ID, ScopeKey: "scope", Deferred: true})}}
	if err := g.coverage(); err != nil {
		t.Fatalf("coverage: %v", err)
	}
	published := g.caps.finish(f.c.log)
	for _, s := range published {
		if s.ProviderID != d.ID {
			continue
		}
		if s.State == model.CapabilityFresh {
			t.Fatalf("%s/%s reported fresh coverage while its only unit is still deferred", s.ProviderID, s.Capability)
		}
		if s.Details["reason"] != "units_deferred" {
			t.Fatalf("%s/%s deferred row has details %v", s.ProviderID, s.Capability, s.Details)
		}
	}

	// The same unit, once the publication generation attaches it, is coverage.
	g = &generation{c: f.c, caps: newCapabilityReport(), sel: sel,
		plan:   plan.Plan{Units: oneUnit(plan.Unit{ProviderID: d.ID, ScopeKey: "scope", Deferred: true})},
		sealed: map[string]bool{plan.Key(d.ID, "scope"): true}}
	if err := g.coverage(); err != nil {
		t.Fatalf("coverage: %v", err)
	}
	published = g.caps.finish(f.c.log)
	for _, s := range published {
		if s.ProviderID == d.ID && s.State != model.CapabilityFresh {
			t.Fatalf("%s/%s reported %q after its deferred unit sealed into this generation",
				s.ProviderID, s.Capability, s.State)
		}
	}

	// A unit the sealer has already started is still pending. Reporting only
	// what has not started answers "nothing pending" for as long as the unit
	// runs, which is the whole window a query needs the answer in.
	l := f.c.late
	l.mu.Lock()
	l.running = &deferredUnit{unit: plan.Unit{ProviderID: d.ID, ScopeKey: "running"}}
	l.queue = append(l.queue, deferredUnit{unit: plan.Unit{ProviderID: d.ID, ScopeKey: "queued"}})
	l.mu.Unlock()
	p, err := f.c.Promote(ctx, d.ID, "running")
	if err != nil {
		t.Fatalf("Promote: %v", err)
	}
	if p.Units != 2 || p.Position != 1 {
		t.Fatalf("the unit in flight is %d units at position %d, want 2 at 1", p.Units, p.Position)
	}
	if p, err = f.c.Promote(ctx, d.ID, "queued"); err != nil {
		t.Fatalf("Promote: %v", err)
	}
	if p.Units != 2 || p.Position != 2 {
		t.Fatalf("a promoted unit behind the one in flight is %d units at position %d, want 2 at 2", p.Units, p.Position)
	}
}

// A supplied index whose path resolves to nothing must still be recorded
// against the published generation. An unresolved path plans no unit, so the
// row is the only surviving evidence that tells it apart from a run that
// supplied no index at all -- the distinction doctor's supplied_index check
// exists to report, and the one that was unobservable before this record.
func TestSuppliedIndexRecordedWhenUnresolved(t *testing.T) {
	f := newFixture(t, map[string]string{"a.go": "package a\n"})
	f.c.opts.SuppliedIndexes = []SuppliedIndex{{
		Path: "missing.scip", ProviderID: "scip", ScopeKey: "import:missing.scip"}}
	res, err := f.c.Index(f.ctx, model.IndexRequest{})
	if err != nil {
		t.Fatalf("Index: %v", err)
	}
	rows, err := f.store.SuppliedIndexes(f.ctx, res.Binding.GenerationID)
	if err != nil {
		t.Fatalf("SuppliedIndexes: %v", err)
	}
	if len(rows) != 1 || rows[0].Path != "missing.scip" || rows[0].Resolved {
		t.Fatalf("supplied index record = %+v, want one unresolved missing.scip", rows)
	}
}

// oneUnit is a hand-built plan.Plan's unit sequence. Plan.Units is a sequence
// and not a slice because plan.Build streams it from a spilled run; a test
// that assembles a plan by hand supplies the same shape over one unit.
func oneUnit(u plan.Unit) func(yield func(plan.Unit) error) error {
	return func(yield func(plan.Unit) error) error { return yield(u) }
}
