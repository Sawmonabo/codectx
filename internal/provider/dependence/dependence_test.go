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
	parses  int
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
	if b.parse.Class == dependence.FailureNone && !b.noGraph {
		if err := os.WriteFile(req.OutputPath, []byte("graph"), 0o600); err != nil {
			return dependence.Outcome{}, err
		}
	}
	return b.parse, nil
}

func (b *fakeBackend) Export(_ context.Context, req dependence.ExportRequest) (dependence.ExportOutcome, error) {
	out := b.export
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
		// permits for that class, plus one per subdivided part.
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
			parses: 3,
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
			// The zero-exit helper crash: the engine reports success and
			// writes a graph with nothing in it.
			name:    "an export with no methods for a unit that has source is an engine failure",
			backend: &fakeBackend{deadExport: true},
			code:    model.CodeProviderOutputInvalid,
			detail:  map[string]string{"failure_class": "engine"},
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

	// The same closure, on the marker half, under a repository that owns more
	// lock files than any fixed bound would allow. Failure mode: a lock file
	// silently dropped from the closure, so the unit is reused across a
	// dependency change with nothing recording that it was.
	vendored := map[string]string{"go.mod": repo["go.mod"], "app.go": repo["app.go"]}
	for i := 0; i < 70; i++ {
		vendored[fmt.Sprintf("vendored/m%02d/go.sum", i)] = fmt.Sprintf("example.com/m%02d v1.0.0 h1:x=\n", i)
	}
	vh := providertest.New(t, vendored)
	// A lock file the previous generation's unit declared and this snapshot
	// holds as a tombstone. Failure mode, and the reason membership is a
	// predicate rather than a list built from the live manifest: a deleted
	// member the unit does not recognise leaves the scope eligible for carry
	// (Section 13.3), so the planner emits a carry that AttachCarried refuses
	// because one of the unit's inputs no longer exists in the snapshot.
	const tombstone = "vendored/m70/go.sum"
	view := withTombstone{SnapshotView: vh.View,
		row: model.FileVersion{ID: model.NewFileID(vh.Repo, tombstone), Path: tombstone, Status: model.FileDeleted}}
	vp, err := dependence.PlanUnits(context.Background(), view)
	if err != nil {
		t.Fatal(err)
	}
	if len(vp.Units) != 1 || vp.Units[0].ScopeKey != rootScope {
		t.Fatalf("plan = %v, want one %s unit", vp.Units, rootScope)
	}
	if !vp.Units[0].OwnsInput("vendored/m69/go.sum") {
		t.Error("the unit does not own one of the 71 manifest and lock files under its root; a lock file outside the closure leaves the cache key")
	}
	if !vp.Units[0].OwnsInput(tombstone) {
		t.Errorf("the unit does not own %s, a lock file the snapshot holds as a tombstone; the scope stays carry-eligible after a member is deleted", tombstone)
	}
}

// withTombstone is a snapshot view with one deleted manifest row appended.
// providertest retains live files only, and a tombstone is precisely the row
// the planner must still resolve to a member.
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
	for _, u := range p.Units {
		if u.ScopeKey != rootScope {
			continue
		}
		spec, err := u.Spec(cfg.AnalysisConfigHash())
		if err != nil {
			t.Fatal(err)
		}
		return spec.ID
	}
	t.Fatalf("the planner planned no %s unit", rootScope)
	return ""
}

// edited is files with one path's content replaced. It copies, because the
// fixture map is shared with every other row in this file.
func edited(files map[string]string, path, content string) map[string]string {
	out := maps.Clone(files)
	out[path] = content
	return out
}

// TestGovernorRetriesOnceAndOnlyHigher protects the memory ruling: there is no
// default ceiling, a retry happens only when it could succeed, and only an
// explicit user ceiling rejects work. Failure mode: a retry at the cap that
// just failed costs a full parse and cannot succeed; a bound invented when the
// machine is unreadable is a default memory ceiling by another name.
func TestGovernorRetriesOnceAndOnlyHigher(t *testing.T) {
	g := dependence.NewGovernor(0, 0)
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
	if g.Reject(unknown, rootScope) != nil {
		t.Error("a machine-derived allocation rejected a unit; only an explicit user ceiling may")
	}

	capped := dependence.NewGovernor(0, 1<<30)
	if capped.Reject(capped.Reserve(dependence.FamilyC, 1<<30, plenty), rootScope) == nil {
		t.Error("an explicit ceiling did not reject a unit that does not fit it")
	}

	// Admission is the other half of the same ruling: a reservation orders and
	// serializes heavy work, and two heavy analyzers run together only when
	// their reservations sum inside the machine-derived allocation. Failure
	// mode: admission grants two heavy analyzers whose summed reservations
	// exceed the allocation, so an indexing run OOM-kills the machine.
	//
	// maxHeavy is 2 here on purpose: at the default of 1 the count alone would
	// serialize the second unit and the memory gate would never be exercised.
	sched := plan.NewScheduler(2, plenty)
	heavy := g.Reserve(dependence.FamilyPython, 1<<30, plenty)
	if 2*heavy.Bytes() <= heavy.AllocationBytes {
		t.Fatalf("two %d-byte reservations fit the %d-byte allocation; this row no longer proves the gate",
			heavy.Bytes(), heavy.AllocationBytes)
	}
	release, err := sched.Admit(context.Background(), heavy)
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	blocked, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	if _, err := sched.Admit(blocked, heavy); err == nil {
		t.Error("admission granted a second heavy analyzer whose reservation does not fit beside the first")
	}
	// The first slot back is what lets the queue move: a reservation never
	// refuses work, it only orders it.
	release()
	release() // releasing twice must not free a slot that was never taken
	second, err := sched.Admit(context.Background(), heavy)
	if err != nil {
		t.Fatalf("Admit after release: %v", err)
	}
	stillBlocked, cancel2 := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel2()
	if _, err := sched.Admit(stillBlocked, heavy); err == nil {
		t.Error("releasing one admission twice freed a slot that was never taken")
	}
	second()
}
