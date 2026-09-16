package dependence_test

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Sawmonabo/codectx/internal/config"
	"github.com/Sawmonabo/codectx/internal/index/plan"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider"
	"github.com/Sawmonabo/codectx/internal/provider/dependence"
	"github.com/Sawmonabo/codectx/internal/provider/dependence/neo4jcsv"
	"github.com/Sawmonabo/codectx/internal/provider/providertest"
	"github.com/Sawmonabo/codectx/internal/workspace"
)

// repo is one Go module with a nested module. It exercises the two planning
// rules that decide what an analysis even sees: a module is a unit, and a
// module nested inside another belongs to itself, not to its parent.
var repo = map[string]string{
	"go.mod":           "module example.com/app\n\ngo 1.27\n",
	"go.sum":           "",
	"app.go":           "package app\n\nfunc Run(x int) int {\n\ty := x + 1\n\treturn y\n}\n",
	"tool/go.mod":      "module example.com/app/tool\n\ngo 1.27\n",
	"tool/tool.go":     "package tool\n\nfunc Help() string { return \"help\" }\n",
	"docs/overview.md": "# overview\n",
}

const rootScope = "pkg:go:"

// fakeBackend stands in for the engine. It produces the artifacts the provider
// validates — a graph file and an export directory with a method-row file —
// and lets a test choose the outcome of each step, which is how the four
// failure paths below are driven without an engine and without a machine that
// can be made to run out of memory on demand.
type fakeBackend struct {
	parse  dependence.Outcome
	export dependence.ExportOutcome
	// deadExport writes an export with no method rows, which is what a crashed
	// frontend helper leaves behind.
	deadExport bool
	// noGraph makes a nominally clean parse leave no graph.
	noGraph bool
	// wholeExportCrash is the engine crash the WHOLE unit's export dies on
	// while every subdivided part's export succeeds -- the measured shape of a
	// 4,984-file project whose export died at every heap cap.
	wholeExportCrash dependence.Outcome
	// wholeParseCrash is the engine crash the WHOLE unit's parse dies on while
	// every subdivided part parses cleanly, which is the shape of the
	// reproducible linker fault a real monorepo unit crashed on twice.
	wholeParseCrash dependence.Outcome
	parses          int
	exports         int
}

func (b *fakeBackend) Engine() dependence.Engine {
	return dependence.Engine{ParseArgv: []string{"/opt/engine/parse"}, ExportArgv: []string{"/opt/engine/export"},
		// A payload name no rendered product string may contain. The real
		// locator fills this from the tool lock entry.
		Version: "1.0.0", Digest: strings.Repeat("a", 64), RuntimeDigest: strings.Repeat("b", 64)}
}

func (b *fakeBackend) Argv(dependence.Family) []string { return []string{"--pinned"} }

func (b *fakeBackend) NeutralOptions(dependence.Family) []string { return nil }

func (b *fakeBackend) Parse(_ context.Context, req dependence.ParseRequest) (dependence.Outcome, error) {
	b.parses++
	// A subdivided part's graph is "graph-<n>"; anything else is the whole
	// unit's, exactly as in Export below.
	if b.wholeParseCrash.Class != dependence.FailureNone && !strings.HasPrefix(filepath.Base(req.OutputPath), "graph-") {
		return b.wholeParseCrash, nil
	}
	if b.parse.Class == dependence.FailureNone && !b.noGraph {
		if err := os.WriteFile(req.OutputPath, []byte("graph"), 0o600); err != nil {
			return dependence.Outcome{}, err
		}
	}
	return b.parse, nil
}

func (b *fakeBackend) Export(_ context.Context, req dependence.ExportRequest) (dependence.ExportOutcome, error) {
	b.exports++
	out := b.export
	// A subdivided part's graph is "graph-<n>"; anything else is the whole
	// unit's graph, which after a clean parse may be the cache's own entry.
	if b.wholeExportCrash.Class != dependence.FailureNone && !strings.HasPrefix(filepath.Base(req.GraphPath), "graph-") {
		return dependence.ExportOutcome{Outcome: b.wholeExportCrash}, nil
	}
	if out.Class != dependence.FailureNone {
		return out, nil
	}
	if err := os.MkdirAll(req.OutputDir, 0o700); err != nil {
		return dependence.ExportOutcome{}, err
	}
	rows := "1,METHOD,Run\n"
	if b.deadExport {
		rows = ""
	}
	if err := os.WriteFile(filepath.Join(req.OutputDir, "nodes_METHOD_data.csv"), []byte(rows), 0o600); err != nil {
		return dependence.ExportOutcome{}, err
	}
	out.Live, out.Bytes = !b.deadExport, int64(len(rows))
	return out, nil
}

// fakeImporter emits one method node per unit so a succeeded run actually puts
// a fact through the sink, which is what makes "a failed export admits no
// facts" a statement about storage rather than about a no-op. It stands in for
// the production importer only here, where the export is the fake backend's
// stub rather than a real one; dependence.New binds the real reader.
type fakeImporter struct{ report dependence.ImportReport }

func (f fakeImporter) Import(ctx context.Context, dir string, res provider.Resolver,
	sink provider.Sink, opts neo4jcsv.Options) (dependence.ImportReport, error) {

	cand := model.NodeCandidate{ProviderID: dependence.ProviderID, ScopeKey: opts.UnitScopeKey,
		NativeKey: "method:Run", Kind: model.NodeFunction, Language: opts.Language, Name: "Run"}
	resolution, err := res.Resolve(ctx, cand)
	if err != nil {
		return dependence.ImportReport{}, err
	}
	report := f.report
	report.Nodes = 1
	if err := sink.PutNodes(ctx, []model.NodeFact{{Node: resolution.Node, CanonicalKey: resolution.CanonicalKey,
		Evidence: []model.Evidence{evidenceFor(ctx, resolution.Node)}}}); err != nil {
		return dependence.ImportReport{}, err
	}
	return report, nil
}

// requestKey carries the unit request into the fake importer, which the real
// importer receives through its own options; a test only needs it to build a
// valid evidence row.
type requestKey struct{}

func evidenceFor(ctx context.Context, node model.Node) model.Evidence {
	req, _ := ctx.Value(requestKey{}).(provider.UnitRequest)
	e := model.Evidence{UnitID: req.Unit.ID, ProviderID: req.Unit.ProviderID, ProviderVersion: req.Unit.ProviderVersion,
		OriginRunID: req.Run, NodeID: node.ID, Precision: model.PrecisionStaticAnalysis}
	e.ID = model.NewEvidenceID(e)
	return e
}

// carrier passes the unit request to the fake importer through the context, so
// the fake stays a drop-in for the real importer's signature.
type carrier struct{ *dependence.Provider }

func (c carrier) IndexUnit(ctx context.Context, req provider.UnitRequest, sink provider.Sink) (model.ProviderResult, error) {
	return c.Provider.IndexUnit(context.WithValue(ctx, requestKey{}, req), req, sink)
}

func newProvider(t *testing.T, b *fakeBackend) provider.Provider {
	t.Helper()
	return newProviderIn(t, b, t.TempDir())
}

// newProviderIn builds a provider over a caller-chosen data directory, so a
// test can run two units against the same graph cache.
func newProviderIn(t *testing.T, b *fakeBackend, dataDir string) provider.Provider {
	t.Helper()
	// NewWithImporter, not New: the fake backend writes an export no real
	// reader can import, so the fault injection has to replace both.
	p, err := dependence.NewWithImporter(b, fakeImporter{}, dependence.Options{DataDir: dataDir,
		Timeout: 2 * time.Minute, CacheBytes: 1 << 20, Limits: providertest.Limits})
	if err != nil {
		t.Fatal(err)
	}
	return carrier{p}
}

// TestSucceededUnitConforms is the shared conformance check plus the planning
// rule it depends on. Failure mode: identities that depend on map iteration or
// discovery order would make the same bytes produce different canonical IDs,
// so unchanged facts could never be reused across snapshots.
func TestSucceededUnitConforms(t *testing.T) {
	providertest.Conform(t, newProvider(t, &fakeBackend{}), repo, rootScope, []string{"app.go", "go.mod"})

	// Detection is a product surface: `status`, `doctor` and the ledger render
	// ObservedVersion verbatim. Failure mode: the engine's payload name is
	// composed into it from the resolved payload with no string literal at the
	// site, so a name leak ships past every source-level check while the
	// provenance a reader actually needs — version and payload digest — is
	// unchanged. Only the rendered string can catch it.
	b := &fakeBackend{}
	h := providertest.New(t, repo)
	det, err := newProvider(t, b).Detect(context.Background(), h.Root, h.Policy)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if want := "engine " + b.Engine().Version + " " + b.Engine().Digest; det.ObservedVersion != want {
		t.Errorf("ObservedVersion = %q, want %q: the payload name is never rendered on a product surface",
			det.ObservedVersion, want)
	}
}

// TestFailedUnitAdmitsNoFacts is Task 11 Step 1's process-fault case. Failure
// mode: a crashed or empty analysis whose partial output still reached a
// generation would let a query answer from facts no analysis ever finished
// producing, and would report the capability as fresh while doing it.
func TestFailedUnitAdmitsNoFacts(t *testing.T) {
	cases := []struct {
		name    string
		backend *fakeBackend
		code    string
		detail  map[string]string
		// parses is how many parse steps the classified failure is allowed to
		// cost: one attempt, plus the single confirmation or retry the plan
		// permits for that class -- which a crash that named its failing pass
		// and its exception does not get, because it reproduces on sight --
		// plus one per subdivided part.
		parses int
	}{
		{
			// A pass crash with no boundary to split along: the fixture module
			// has one subdirectory, which is the nested module, so no honest
			// subdivided result exists and the unit fails.
			name: "a reproducible pass crash fails the unit and names the pass",
			backend: &fakeBackend{parse: dependence.Outcome{Class: dependence.FailureEngine,
				Pass: "CfgCreationPass", Exception: "java.util.NoSuchElementException", ExitCode: 1}},
			code:   model.CodeProviderOutputInvalid,
			detail: map[string]string{"failure_class": "engine", "pass": "CfgCreationPass"},
			parses: 2,
		},
		{
			name: "heap exhaustion is retried exactly once and then fails closed with its figures",
			backend: &fakeBackend{parse: dependence.Outcome{Class: dependence.FailureMemory,
				Exception: "java.lang.OutOfMemoryError", ExitCode: 1}},
			code:   model.CodeResourceLimit,
			detail: map[string]string{"failure_class": "memory", "exception": "java.lang.OutOfMemoryError"},
			parses: 2,
		},
		{
			// Both steps exited cleanly and the export holds no method. It is
			// classified as what it is rather than as a crash, and it names
			// the family and the file count so a reader can compare what the
			// unit declared against what the frontend admitted.
			name:    "an export with no methods for a unit that has source is not worded as a crash",
			backend: &fakeBackend{deadExport: true},
			code:    model.CodeProviderOutputInvalid,
			detail:  map[string]string{"failure_class": "empty_export", "family": "go", "source_files": "1"},
			parses:  1,
		},
		{
			name:    "a clean exit that left no graph is an engine failure, not a success",
			backend: &fakeBackend{noGraph: true},
			code:    model.CodeProviderOutputInvalid,
			detail:  map[string]string{"failure_class": "engine"},
			parses:  3,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := providertest.New(t, repo)
			result, unit, err := h.Run(t, newProvider(t, c.backend), rootScope, []string{"app.go", "go.mod"})
			if err == nil {
				t.Fatal("the unit succeeded; a classified analysis failure must never seal")
			}
			if got := provider.CodeOf(err); got != c.code {
				t.Errorf("error code = %q, want %q (%v)", got, c.code, err)
			}
			var typed *model.Error
			if !errors.As(err, &typed) {
				t.Fatalf("the failure is untyped: %v", err)
			}
			for k, want := range c.detail {
				if typed.Details[k] != want {
					t.Errorf("detail %q = %q, want %q", k, typed.Details[k], want)
				}
			}
			if result.State == model.RunSucceeded {
				t.Errorf("run state = %s, want a failed state", result.State)
			}
			if state, ok := h.UnitState(t, unit); ok && state == model.UnitSealed {
				t.Error("the unit sealed; a failed analysis must leave no queryable facts")
			}
			if c.backend.parses != c.parses {
				t.Errorf("the unit cost %d parse steps, want %d: a doomed retry is a full parse spent for nothing",
					c.backend.parses, c.parses)
			}
		})
	}
}

// TestSkippedMethodsPublishPartial is Task 11 Step 1's stderr case. Failure
// mode: a unit whose data dependence is missing whole method bodies, sealed
// and reported fresh, is a silent correctness loss — a consumer would read
// "no data flow here" as an analysed absence rather than an unanalysed one.
func TestSkippedMethodsPublishPartial(t *testing.T) {
	dataDir := t.TempDir()
	skips := dependence.Outcome{SkippedCount: 2,
		SkippedMethods: []string{"app.go:<module>.Run", "app.go:<module>.Helper"}}
	b := &fakeBackend{parse: skips}
	h := providertest.New(t, repo)
	result, unit, err := h.Run(t, newProviderIn(t, b, dataDir), rootScope, []string{"app.go", "go.mod"})
	if err != nil {
		t.Fatalf("the unit failed: %v", err)
	}
	if result.State != model.RunSucceeded {
		t.Fatalf("run state = %s, want succeeded: a definition-cap skip degrades a unit, it does not fail it", result.State)
	}
	if state, ok := h.UnitState(t, unit); !ok || state != model.UnitSealed {
		t.Fatalf("unit state = %q, want sealed", state)
	}
	if err := result.Validate(); err != nil {
		t.Fatalf("the published result is not valid: %v", err)
	}
	rows := byCapability(result.Capabilities)
	if rows[dependence.CapabilityDataFlowsTo].State != model.CapabilityPartial {
		t.Errorf("data_flows_to = %q, want partial", rows[dependence.CapabilityDataFlowsTo].State)
	}
	for _, c := range []string{dependence.CapabilityControlDependsOn, dependence.CapabilityReads,
		dependence.CapabilityWrites, dependence.CapabilityCalls} {
		if rows[c].State != model.CapabilityFresh {
			t.Errorf("%s = %q, want fresh: only data flow depends on the pass that skipped", c, rows[c].State)
		}
		// A fresh capability carries no detail. A detail on a fresh row would
		// describe a loss the row itself denies, and a consumer reading the
		// row would have to decide which of the two to believe.
		if len(rows[c].Details) != 0 {
			t.Errorf("%s is fresh but carries details %v", c, rows[c].Details)
		}
	}
	// The partial row carries its own particulars. Without them the result
	// says a capability is incomplete but nothing about what is missing, and
	// the only remaining way to publish it is a capability row whose name is
	// the detail text — which grows the bounded list with the repository.
	details := rows[dependence.CapabilityDataFlowsTo].Details
	if details["skipped_methods"] != "2" {
		t.Errorf("skipped_methods = %q, want the exact count \"2\"", details["skipped_methods"])
	}
	for _, name := range b.parse.SkippedMethods {
		if !strings.Contains(details["skipped_method_names"], name) {
			t.Errorf("the partial capability does not name the skipped method %q; names = %q",
				name, details["skipped_method_names"])
		}
	}

	// The same unit again, against the same graph cache. What was skipped
	// exists only on the parse's stderr, so a reused graph carries no trace of
	// it: reusing one here would republish data_flows_to as fresh and silently
	// upgrade an unanalysed absence into an analysed one. This is the path the
	// first run cannot reach, and the one a cache exists to take.
	again := &fakeBackend{parse: skips}
	h2 := providertest.New(t, repo)
	second, _, err := h2.Run(t, newProviderIn(t, again, dataDir), rootScope, []string{"app.go", "go.mod"})
	if err != nil {
		t.Fatalf("the second unit failed: %v", err)
	}
	if again.parses == 0 {
		t.Error("the second run reused a cached graph for a unit that skipped methods")
	}
	rows = byCapability(second.Capabilities)
	if rows[dependence.CapabilityDataFlowsTo].State != model.CapabilityPartial {
		t.Errorf("data_flows_to on reuse = %q, want partial", rows[dependence.CapabilityDataFlowsTo].State)
	}
	if got := rows[dependence.CapabilityDataFlowsTo].Details["skipped_methods"]; got != "2" {
		t.Errorf("skipped_methods on reuse = %q, want the exact count \"2\"", got)
	}
}

// byCapability indexes a result's capability rows by name.
func byCapability(states []model.CapabilityState) map[string]model.CapabilityState {
	out := make(map[string]model.CapabilityState, len(states))
	for _, c := range states {
		out[c.Capability] = c
	}
	return out
}

// TestCleanGraphIsReused is the other half of the cache contract, and the
// reason the reuse assertion above is not vacuous. Failure mode: a cache that
// never hits would make every "partial survives reuse" assertion pass while
// the expensive parse it exists to avoid runs every generation anyway.
func TestCleanGraphIsReused(t *testing.T) {
	dataDir := t.TempDir()
	first := &fakeBackend{}
	h := providertest.New(t, repo)
	if _, _, err := h.Run(t, newProviderIn(t, first, dataDir), rootScope, []string{"app.go", "go.mod"}); err != nil {
		t.Fatalf("the first unit failed: %v", err)
	}
	if first.parses != 1 {
		t.Fatalf("parses = %d, want 1 on a cold cache", first.parses)
	}
	again := &fakeBackend{}
	h2 := providertest.New(t, repo)
	if _, _, err := h2.Run(t, newProviderIn(t, again, dataDir), rootScope, []string{"app.go", "go.mod"}); err != nil {
		t.Fatalf("the second unit failed: %v", err)
	}
	if again.parses != 0 {
		t.Errorf("parses = %d, want 0: the same source and the same pinned argv must reuse the graph", again.parses)
	}
}

// TestPlanUnits protects the unit shapes the parity measurements settled:
// splitting a project loses more than half of its resolved calls, and a
// module nested in another module analysed twice would double every fact it
// holds.
func TestPlanUnits(t *testing.T) {
	h := providertest.New(t, repo)
	p, err := dependence.PlanUnits(context.Background(), h.View)
	if err != nil {
		t.Fatal(err)
	}
	plan := p.Units
	var keys []string
	for _, u := range plan {
		keys = append(keys, u.ScopeKey)
	}
	want := []string{"pkg:go:", "pkg:go:tool"}
	if !slices.Equal(keys, want) {
		t.Fatalf("plan = %v, want %v", keys, want)
	}
	if plan[0].Contains("tool/tool.go") {
		t.Error("the outer module owns the nested module's file; it would be analysed twice")
	}
	if !plan[1].Contains("tool/tool.go") {
		t.Error("the nested module does not own its own file")
	}
	if plan[0].Files != 1 || plan[0].Bytes == 0 {
		t.Errorf("outer module folded %d files and %d bytes; the governor sizes the heap cap from these",
			plan[0].Files, plan[0].Bytes)
	}

	// A unit's cache key and its planned identity are its semantic closure and
	// nothing else (Section 11.6). Failure mode on the wide side: a
	// documentation edit invalidates the heaviest unit in the product, which is
	// a full engine parse and export. Failure mode on the narrow side, which is
	// the one that corrupts answers: a source file or a lock file leaves the
	// key, so the unit is reused across a change it never saw and serves stale
	// dependence facts as fresh.
	base := dependenceUnitID(t, repo)
	if got := dependenceUnitID(t, edited(repo, "docs/overview.md", "# overview, revised\n")); got != base {
		t.Error("editing a documentation file changed the dependence unit's identity; every README edit would reparse the module")
	}
	if got := dependenceUnitID(t, edited(repo, "app.go", "package app\n\nfunc Run(x int) int { return x }\n")); got == base {
		t.Error("editing the module's own source did not change its identity; the unit would be reused across a source change")
	}
	if got := dependenceUnitID(t, edited(repo, "go.sum", "example.com/dep v1.2.3 h1:abc=\n")); got == base {
		t.Error("editing the module's lock file did not change its identity; the unit would be reused across a dependency change")
	}

	// The same closure on the marker half, for a lock file the snapshot holds
	// as a tombstone.
	vendored := map[string]string{"go.mod": repo["go.mod"], "app.go": repo["app.go"]}
	for i := 0; i < 2; i++ {
		vendored[fmt.Sprintf("vendored/m%02d/go.sum", i)] = fmt.Sprintf("example.com/m%02d v1.0.0 h1:x=\n", i)
	}
	vh := providertest.New(t, vendored)
	// A lock file the previous generation's unit declared and this snapshot
	// holds as a tombstone. Failure mode, and the reason membership is a
	// predicate rather than a list built from the live manifest: a deleted
	// member the unit does not recognise leaves the scope eligible for carry
	// (Section 13.3), so the planner emits a carry that AttachCarried refuses
	// because one of the unit's inputs no longer exists in the snapshot.
	const tombstone = "vendored/m02/go.sum"
	view := withTombstone{SnapshotView: vh.View,
		row: model.FileVersion{ID: model.NewFileID(vh.Repo, tombstone), Path: tombstone, Status: model.FileDeleted}}
	vp, err := dependence.PlanUnits(context.Background(), view)
	if err != nil {
		t.Fatal(err)
	}
	if len(vp.Units) != 1 || vp.Units[0].ScopeKey != rootScope {
		t.Fatalf("plan = %v, want one %s unit", vp.Units, rootScope)
	}
	if !vp.Units[0].OwnsInput(tombstone) {
		t.Errorf("the unit does not own %s, a lock file the snapshot holds as a tombstone; the scope stays carry-eligible after a member is deleted", tombstone)
	}
}

// withTombstone is a snapshot view with one deleted manifest row appended.
// providertest retains live files only, and a tombstone is precisely the row
// the planner must still resolve to a member. The row is yielded whatever the
// selection says; both callers pass an empty FileSelection. It is appended
// last, and the path is chosen to sort after every fixture path, so the view's
// ascending-path order is preserved.
type withTombstone struct {
	model.SnapshotView
	row model.FileVersion
}

func (v withTombstone) EachFile(ctx context.Context, sel model.FileSelection, fn func(model.FileVersion) error) error {
	if err := v.SnapshotView.EachFile(ctx, sel, fn); err != nil {
		return err
	}
	return fn(v.row)
}

// planStub is the dependence provider as the planner sees it. The planner
// dispatches on descriptor identity and asks a dependence provider nothing
// else -- the scopes come from dependence.PlanUnits over the snapshot -- so
// the closure rows above need no engine.
type planStub struct{}

func (planStub) Descriptor() model.ProviderDescriptor {
	return model.ProviderDescriptor{ID: dependence.ProviderID, Version: "planstub",
		Capabilities: dependence.Capabilities, InvalidationScope: model.InvalidationPackage}
}

func (planStub) Detect(context.Context, workspace.Root, workspace.Policy) (provider.Detection, error) {
	return provider.Detection{}, errors.New("the planner never detects")
}

func (planStub) IndexUnit(context.Context, provider.UnitRequest, provider.Sink) (model.ProviderResult, error) {
	return model.ProviderResult{}, errors.New("the planner never runs a unit")
}

// dependenceUnitID plans files through the real planner and folds the identity
// of the root module's unit, which is what reuse compares byte for byte. Two
// harnesses over the same content mint the same identities (providertest fixes
// the repository id for exactly this comparison), so a difference here is a
// difference in the closure and nothing else.
func dependenceUnitID(t *testing.T, files map[string]string) model.UnitID {
	t.Helper()
	h := providertest.New(t, files)
	cfg := config.Defaults()
	p, err := plan.Build(context.Background(), plan.Inputs{View: h.View, Store: h.Store, Config: cfg,
		Selection: provider.Selection{Active: []provider.Provider{planStub{}},
			Detections: map[string]provider.Detection{dependence.ProviderID: {Available: true,
				Capabilities: dependence.Capabilities}}}})
	if err != nil {
		t.Fatal(err)
	}
	var found model.UnitID
	if err := p.Units(func(u plan.Unit) error {
		if u.ScopeKey != rootScope || found != "" {
			return nil
		}
		spec, err := u.Spec(cfg.AnalysisConfigHash())
		if err != nil {
			return err
		}
		found = spec.ID
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if found == "" {
		t.Fatalf("the planner planned no %s unit", rootScope)
	}
	return found
}

// edited is files with one path's content replaced. It copies, because the
// fixture map is shared with every other row in this file.
func edited(files map[string]string, path, content string) map[string]string {
	out := maps.Clone(files)
	out[path] = content
	return out
}

// TestGovernorRetriesOnceAndOnlyHigher protects the memory ruling: there is no
// ceiling at all, and a retry happens only when it could succeed. Failure
// mode: a retry at the cap that just failed costs a full parse and cannot
// succeed; a bound invented when the machine is unreadable is a default memory
// ceiling by another name.
func TestGovernorRetriesOnceAndOnlyHigher(t *testing.T) {
	g := dependence.NewGovernor(0)
	plenty := dependence.Machine{AvailableBytes: 32 << 30, Observed: true}

	small := g.Reserve(dependence.FamilyGo, 1<<20, plenty)
	if small.HeapCapBytes != dependence.DefaultUnitMemoryFloorBytes {
		t.Errorf("a tiny unit got a %d-byte cap, want the floor", small.HeapCapBytes)
	}
	if small.Bytes() <= small.HeapCapBytes {
		t.Error("the reservation is not above its heap cap; a cap is not a memory cap")
	}
	if g.RetryCap(small, 0) <= small.HeapCapBytes {
		t.Error("a unit far below the allocation has no retry available")
	}
	// The observed peak is the tree's, on the attempt that just failed. A tree
	// that already reached what the machine can allocate has nowhere to grow,
	// whatever its heap cap was, so the retry the plan allows would be a
	// second full parse for the same failure.
	if got := g.RetryCap(small, small.AllocationBytes); got != 0 {
		t.Errorf("retry cap = %d, want none: the failed attempt already peaked at the whole allocation", got)
	}

	// The host keeps at least half of what was available when the run began.
	// Failure mode: an allocation of "everything but two gigabytes" handed one
	// analyzer a 42 GB heap cap on a 47 GB machine, which then serialized
	// every other unit behind it and left nothing for the editor, the agents
	// and the browser the person is using while they index.
	if small.AllocationBytes > plenty.AvailableBytes/2 {
		t.Errorf("allocation %d of %d available leaves the host less than half",
			small.AllocationBytes, plenty.AvailableBytes)
	}
	// And the cap comes from what the unit needs, not from what the machine
	// has: the largest JavaScript project measured ran at full speed under
	// 4 GiB, so a 157 MB one must not be handed the whole allocation.
	big := g.Reserve(dependence.FamilyJavaScript, 157<<20, plenty)
	if big.HeapCapBytes >= big.AllocationBytes {
		t.Errorf("a 157 MB JavaScript unit was capped at the allocation (%d of %d): the cap is sized to the machine, not to the need",
			big.HeapCapBytes, big.AllocationBytes)
	}
	if big.HeapCapBytes < 4<<30 {
		t.Errorf("a 157 MB JavaScript unit was capped at %d, below the 4 GiB the measured project needed", big.HeapCapBytes)
	}

	huge := g.Reserve(dependence.FamilyPython, 1<<30, plenty)
	if huge.HeapCapBytes != huge.AllocationBytes {
		t.Errorf("cap %d is not bounded by the machine-derived allocation %d", huge.HeapCapBytes, huge.AllocationBytes)
	}
	if got := g.RetryCap(huge, 0); got != 0 {
		t.Errorf("retry cap = %d, want none: the first attempt already had the whole allocation", got)
	}

	unknown := g.Reserve(dependence.FamilyPython, 1<<30, dependence.Machine{})
	if unknown.AllocationBytes != 0 {
		t.Error("an unobserved machine reports an allocation; unavailable must stay unavailable")
	}
	if unknown.HeapCapBytes != unknown.EstimatedBytes {
		t.Error("an unobserved machine bounded the cap; that would be a default memory ceiling")
	}
}

// TestAdmissionIsTheAllocationAndNeverACount protects the rule that how much
// heavy work runs at once is decided by one observation of the machine: heavy
// children are admitted while the sum of their reservations fits the
// machine-derived allocation, and nothing counts them. Failure mode: a count
// serialises every analysis unit on a machine with room for four of them, and
// on a small machine a count admits work whose summed reservations the host
// cannot hold.
//
// Mutation: give Scheduler a maxHeavy of 1 again -- add `maxHeavy int` set to
// 1 by NewScheduler and `if s.admitted >= s.maxHeavy { return }` at the head of
// pump -> "a 32 GiB machine admitted 1 of four 4 GiB reservations at once; how
// much runs at once is being decided by a count, not by the allocation".
func TestAdmissionIsTheAllocationAndNeverACount(t *testing.T) {
	// The reservation is built directly so the figures in this test are the
	// ones being asserted about, not a family estimate that would move with a
	// re-measurement.
	const fourGiB = 4 << 30
	unit := dependence.Reservation{HeapCapBytes: fourGiB}
	if unit.Bytes() != fourGiB {
		t.Fatalf("reservation is %d bytes, want %d; this test's arithmetic no longer holds", unit.Bytes(), fourGiB)
	}

	// 32 GiB available: the allocation is half of it, 16 GiB, which is exactly
	// four of these reservations.
	big := plan.NewScheduler(dependence.Machine{AvailableBytes: 32 << 30, Observed: true})
	var releases []func()
	for i := 0; i < 4; i++ {
		admitted, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		release, err := big.Admit(admitted, unit)
		cancel()
		if err != nil {
			t.Fatalf("a 32 GiB machine admitted %d of four 4 GiB reservations at once; how much runs at once is being decided by a count, not by the allocation", i)
		}
		releases = append(releases, release)
	}
	blocked, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	if _, err := big.Admit(blocked, unit); err == nil {
		t.Error("a fifth 4 GiB reservation was admitted into a 16 GiB allocation")
	}
	// A waiter is never refused, only ordered: one release lets it through.
	releases[0]()
	fifth, err := big.Admit(context.Background(), unit)
	if err != nil {
		t.Fatalf("the waiting reservation was not admitted once room was given back: %v", err)
	}
	fifth()
	for _, release := range releases[1:] {
		release()
	}

	// The same reservation on an 8 GiB machine: the allocation is 4 GiB, so
	// exactly one runs, and it runs because an idle scheduler admits any single
	// unit whatever it reserves -- work is never refused for memory.
	small := plan.NewScheduler(dependence.Machine{AvailableBytes: 8 << 30, Observed: true})
	release, err := small.Admit(context.Background(), unit)
	if err != nil {
		t.Fatalf("an 8 GiB machine admitted nothing: %v", err)
	}
	second, cancel2 := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel2()
	if _, err := small.Admit(second, unit); err == nil {
		t.Error("an 8 GiB machine admitted two 4 GiB reservations at once")
	}
	release()
}

// splittable is one module whose own subdirectory holds source, so the unit
// has a frontend-native boundary to be split along.
var splittable = map[string]string{
	"go.mod":         "module example.com/app\n\ngo 1.27\n",
	"app.go":         "package app\n\nfunc Run(x int) int {\n\ty := x + 1\n\treturn y\n}\n",
	"inner/inner.go": "package inner\n\nfunc Helper() int { return 1 }\n",
}

// A unit the engine cannot export is recovered by subdivision, not reported as
// a crashed unit.
//
// Failure mode: a project whose parse succeeds at every heap cap and whose
// export then dies on the engine's own exception -- measured on a real
// 4,984-file project, whose every subdivided part exported cleanly -- was
// published as a failed unit. Every fact the engine could still produce for
// that project was thrown away, and five capabilities went unavailable for a
// repository the engine could analyse. The recovery is the one a reproducible
// parse crash already has, and the result must say what it is: partial, naming
// the subdivided scope and the failing pass and exception, never memory and
// never a crash of the unit.
//
// Mutation: in export, return failure(out.Class, ...) for the engine class
// instead of confirming and handing the crash back ->
// "the unit failed: CTX_PROVIDER_OUTPUT_INVALID: the dependence unit failed:
// the analysis backend crashed"
func TestADeadExportIsRecoveredBySubdivision(t *testing.T) {
	b := &fakeBackend{wholeExportCrash: dependence.Outcome{Class: dependence.FailureEngine,
		Pass: "Base", Exception: "java.util.NoSuchElementException", ExitCode: 1}}
	h := providertest.New(t, splittable)
	result, unit, err := h.Run(t, newProvider(t, b), rootScope, []string{"app.go", "go.mod", "inner/inner.go"})
	if err != nil {
		t.Fatalf("the unit failed: %v", err)
	}
	if result.State != model.RunSucceeded {
		t.Fatalf("run state = %s, want succeeded: subdivision is a recovery, not a failure", result.State)
	}
	if state, ok := h.UnitState(t, unit); !ok || state != model.UnitSealed {
		t.Fatalf("unit state = %q, want sealed: the parts the engine could export are facts", state)
	}
	// The dead export is confirmed before anything is split, exactly as a
	// parse crash is: one export of the whole unit, one confirmation of it,
	// and one per subdivided part.
	if b.exports != 3 {
		t.Errorf("the unit cost %d export steps, want 3 (the export, its confirmation, one part)", b.exports)
	}
	rows := byCapability(result.Capabilities)
	for _, c := range dependence.Capabilities {
		row := rows[c]
		if row.State != model.CapabilityPartial {
			t.Errorf("%s = %q, want partial: a subdivided unit is never fresh", c, row.State)
		}
		if row.DiagnosticCode == model.CodeResourceLimit {
			t.Errorf("%s reports %q: a dead export is not memory", c, row.DiagnosticCode)
		}
		if row.Details["subdivided"] != rootScope {
			t.Errorf("%s names subdivided=%q, want %q", c, row.Details["subdivided"], rootScope)
		}
		// The export step has no first-sight rule: its crash was observed twice
		// before the unit was split, and the row says which it was.
		if got := row.Details["backend_failure"]; got != "Base/java.util.NoSuchElementException (failure class observed twice)" {
			t.Errorf("%s reports backend_failure=%q, want the pass, its exception and how the crash was established", c, got)
		}
	}
}

// TestANamedCrashIsSubdividedWithoutASecondParse protects the most expensive
// decision this provider makes. Failure mode: a crash whose standard error had
// already named the failing pass and the exception class was parsed a second
// time before the unit was split — on a real monorepo that second parse cost
// three minutes and twenty-two seconds of machine time and only re-proved the
// same deterministic fault, and the unit was subdivided anyway. The report must
// also say which way the decision went, so nobody has to infer from a duration
// why one crash was re-parsed and another was not.
//
// Mutation: make the first-sight branch in graphFor unreachable
// (`if false && reproducibleOnSight(outcome)`) so every crash is confirmed ->
// "the unit cost 3 parse steps, want 2".
func TestANamedCrashIsSubdividedWithoutASecondParse(t *testing.T) {
	b := &fakeBackend{wholeParseCrash: dependence.Outcome{Class: dependence.FailureEngine,
		Pass: "ObjectPropertyCallLinker", Exception: "java.lang.RuntimeException", ExitCode: 1}}
	h := providertest.New(t, splittable)
	result, unit, err := h.Run(t, newProvider(t, b), rootScope, []string{"app.go", "go.mod", "inner/inner.go"})
	if err != nil {
		t.Fatalf("the unit failed: %v", err)
	}
	if state, ok := h.UnitState(t, unit); !ok || state != model.UnitSealed {
		t.Fatalf("unit state = %q, want sealed: the parts the engine could parse are facts", state)
	}
	if b.parses != 2 {
		t.Errorf("the unit cost %d parse steps, want 2 (the crash, one part): a crash that named its pass and its exception reproduces on sight",
			b.parses)
	}
	const want = "ObjectPropertyCallLinker/java.lang.RuntimeException (named pass and exception, taken on first sight)"
	for _, c := range dependence.Capabilities {
		row := byCapability(result.Capabilities)[c]
		if row.State != model.CapabilityPartial {
			t.Errorf("%s = %q, want partial: a subdivided unit is never fresh", c, row.State)
		}
		if got := row.Details["backend_failure"]; got != want {
			t.Errorf("%s reports backend_failure=%q, want %q", c, got, want)
		}
	}
}
