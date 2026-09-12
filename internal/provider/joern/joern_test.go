package joern

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/process"
	"github.com/Sawmonabo/codectx/internal/provider"
	"github.com/Sawmonabo/codectx/internal/provider/providertest"
)

// The fake tools are this test binary, reached through symlinks named
// joern-parse and joern-export, so the provider runs exactly the production
// path: the shared runner, an absolute approved executable, the allowlisted
// environment and nothing else. The two variables below are the allowlist.
const (
	envFixture = "CODECTX_JOERN_FIXTURE"
	envFault   = "CODECTX_JOERN_FAULT"
)

func TestMain(m *testing.M) {
	switch filepath.Base(os.Args[0]) {
	case "joern-parse":
		os.Exit(fakeParse(os.Args[1:]))
	case "joern-export":
		os.Exit(fakeExport(os.Args[1:]))
	}
	os.Exit(m.Run())
}

// fakeParse behaves like joern-parse under the pinned argv: it prints a
// version for the probe, and otherwise requires the materialized source it
// was pointed at and writes the CPG file. A materialization that does not
// hold the pinned file is exit 3: the provider would then be analyzing
// something other than the snapshot.
func fakeParse(args []string) int {
	if len(args) == 1 && args[0] == "--version" {
		fmt.Println("joern-parse 2.0.0-test")
		return 0
	}
	if len(args) != 3 || args[1] != "--output" {
		fmt.Fprintln(os.Stderr, "unexpected joern-parse argv:", args)
		return 2
	}
	if _, err := os.Stat(filepath.Join(args[0], "src", "app.go")); err != nil {
		fmt.Fprintln(os.Stderr, "materialization lacks src/app.go:", err)
		return 3
	}
	if err := os.WriteFile(args[2], []byte("fake cpg"), 0o600); err != nil {
		return 4
	}
	return 0
}

// fakeExport copies the checked-in export for the requested representation,
// or injects the configured fault.
func fakeExport(args []string) int {
	if len(args) == 1 && args[0] == "--version" {
		fmt.Println("joern-export 2.0.0-test")
		return 0
	}
	if len(args) != 5 || args[3] != "--out" {
		fmt.Fprintln(os.Stderr, "unexpected joern-export argv:", args)
		return 2
	}
	if _, err := os.Stat(args[0]); err != nil {
		return 3
	}
	fixture, out := os.Getenv(envFixture), args[4]
	var src string
	switch {
	case args[1] == "--repr=all" && args[2] == "--format=neo4jcsv":
		src = filepath.Join(fixture, "neo4jcsv")
	case args[1] == "--repr=pdg" && args[2] == "--format=graphml":
		src = filepath.Join(fixture, "graphml")
	default:
		return 2
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return 4
	}
	for i, e := range entries {
		data, err := os.ReadFile(filepath.Join(src, e.Name()))
		if err != nil {
			return 4
		}
		if os.Getenv(envFault) == "export-midstream" && i == len(entries)/2 {
			// Half a file, then a crash.
			os.WriteFile(filepath.Join(out, e.Name()), data[:len(data)/2], 0o600)
			fmt.Fprintln(os.Stderr, "simulated export crash")
			return 2
		}
		if err := os.WriteFile(filepath.Join(out, e.Name()), data, 0o600); err != nil {
			return 4
		}
	}
	if os.Getenv(envFault) == "oversized" && src == filepath.Join(fixture, "neo4jcsv") {
		// One record wider than the cap, as a quoted field so it is one CSV
		// record whatever newlines it holds.
		os.WriteFile(filepath.Join(out, "edges_HUGE_header.csv"), []byte(":START_ID,:END_ID,:TYPE,BLOB:string\n"), 0o600)
		os.WriteFile(filepath.Join(out, "edges_HUGE_data.csv"), []byte("1,2,HUGE,\""+strings.Repeat("x\n", maxRecordBytes)+"\"\n"), 0o600)
	}
	return 0
}

// fixtureSource is the repository the export describes. Line numbers and
// columns in the CSV point into it; the test checks that the persisted
// evidence ranges select exactly those bytes.
const fixtureSource = `package main

import "fmt"

func helper() int {
	return 41
}

func check(v int) bool {
	return v > 40
}

func sink(v int) {
	fmt.Println(v)
}

func main() {
	x := helper()
	if check(x) {
		sink(x)
	}
}
`

var fixtureFiles = map[string]string{"src/app.go": fixtureSource}

// newProvider builds a provider over the fake tools with a private work
// directory and returns it with that directory.
func newProvider(t *testing.T) (*Provider, string) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"joern-parse", "joern-export"} {
		if err := os.Symlink(self, filepath.Join(bin, name)); err != nil {
			t.Fatal(err)
		}
	}
	fixture, err := filepath.Abs("testdata")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(envFixture, fixture)
	t.Setenv(envFault, "")
	tool := func(name string) Tool {
		return Tool{Path: filepath.Join(bin, name), VersionConstraint: "2.0.*", Timeout: 30 * time.Second, MemoryBudgetBytes: 1 << 30, DiskBudgetBytes: 1 << 30}
	}
	p := PinnedDefault()
	p.Parse, p.Export = tool("joern-parse"), tool("joern-export")
	p.WorkDir = filepath.Join(dir, "work")
	p.EnvAllowlist = []string{envFixture, envFault}
	p.Timeout = 2 * time.Minute
	runner, err := process.NewRunner(process.Limits{MaxConcurrent: 2, MemoryBudgetBytes: 4 << 30, DiskBudgetBytes: 4 << 30})
	if err != nil {
		t.Fatal(err)
	}
	prov, err := New(p, runner)
	if err != nil {
		t.Fatal(err)
	}
	return prov, p.WorkDir
}

// capture records every fact the provider handed to the real unit output.
type capture struct {
	provider.UnitOutput
	nodes     []model.NodeFact
	relations []model.RelationFact
	aliases   []model.NativeAlias
}

func (c *capture) PutNodes(ctx context.Context, f []model.NodeFact) error {
	c.nodes = append(c.nodes, f...)
	return c.UnitOutput.PutNodes(ctx, f)
}

func (c *capture) PutRelations(ctx context.Context, f []model.RelationFact) error {
	c.relations = append(c.relations, f...)
	return c.UnitOutput.PutRelations(ctx, f)
}

func (c *capture) PutAliases(ctx context.Context, a []model.NativeAlias) error {
	c.aliases = append(c.aliases, a...)
	return c.UnitOutput.PutAliases(ctx, a)
}

// run drives one unit through the production RunUnit path with a capturing
// output and returns what was captured, the result and RunUnit's error.
func run(t *testing.T, p provider.Provider, h *providertest.Harness) (*capture, providertest.Unit, model.ProviderResult, error) {
	t.Helper()
	inputs := []string{"src/app.go"}
	u := h.Plan(t, p, provider.ScopeWorkspace, inputs)
	cap := &capture{UnitOutput: h.Begin(t, u, inputs)}
	result, err := provider.RunUnit(context.Background(), p, u.Request, cap, providertest.Limits, h.Pool)
	return cap, u, result, err
}

// requireCleanWorkDir fails when a run left anything behind under the
// profile's work directory: a leaked materialization, CPG or export is
// retained source outside the CAS and disk the budget no longer accounts.
func requireCleanWorkDir(t *testing.T, workDir string) {
	t.Helper()
	var left []string
	filepath.WalkDir(workDir, func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			left = append(left, path)
		}
		return nil
	})
	if len(left) > 0 {
		t.Fatalf("the run left files under the work directory: %v", left)
	}
}

// TestJoernExportFixture is the one export fixture: it conforms (descriptor,
// detection through the version probe, a sealed unit, identical identities
// across two repositories with the sink flushed after every Put), then checks
// the facts one run persists. Failure modes it protects:
//
//   - a CALL, CDG or REACHING_DEF edge that does not become the relation it
//     describes, or one that does when its endpoints are not distinct call
//     targets (wrong facts);
//   - an edge that names a node defined later in the export being dropped
//     (data loss: every edge file sorts before every node file);
//   - a method whose FILENAME is not in the snapshot producing a fact bound
//     to bytes that are not its source (wrong source binding);
//   - an evidence range that does not select the call's exact bytes (wrong
//     source served);
//   - an unknown label passing silently instead of being reported;
//   - a record wider than the cap reaching the CSV decoder's buffer
//     (unbounded allocation).
func TestJoernExportFixture(t *testing.T) {
	p, workDir := newProvider(t)
	providertest.Conform(t, p, fixtureFiles, provider.ScopeWorkspace, []string{"src/app.go"})
	requireCleanWorkDir(t, workDir)

	h := providertest.New(t, fixtureFiles)
	cap, u, result, err := run(t, p, h)
	if err != nil {
		t.Fatalf("RunUnit: %v", err)
	}
	if state, ok := h.UnitState(t, u.Build.Spec.ID); !ok || state != model.UnitSealed {
		t.Fatalf("unit state = %q (exists %v), want sealed", state, ok)
	}
	requireCleanWorkDir(t, workDir)

	names := map[string]model.NodeID{}
	for _, f := range cap.nodes {
		names[f.Node.Name] = f.Node.ID
	}
	for _, want := range []string{"main", "helper", "check", "sink", "Println"} {
		if _, ok := names[want]; !ok {
			t.Errorf("no node fact for method %q; got %v", want, names)
		}
	}
	if _, ok := names["ghost"]; ok || len(cap.nodes) != 5 {
		t.Errorf("a method whose path is not in the snapshot was published: %v", names)
	}
	kinds := map[string][]model.RelationFact{}
	for _, f := range cap.relations {
		kinds[string(f.Relation.Kind)] = append(kinds[string(f.Relation.Kind)], f)
	}
	edge := func(kind string, from, to string) *model.RelationFact {
		for i := range kinds[kind] {
			f := &kinds[kind][i]
			if f.Relation.From == names[from] && f.Relation.To == names[to] {
				return f
			}
		}
		t.Errorf("missing %s %s -> %s", kind, from, to)
		return nil
	}
	if len(kinds["calls"]) != 4 || len(kinds["control_depends_on"]) != 1 || len(kinds["data_flows_to"]) != 1 || len(cap.relations) != 6 {
		t.Fatalf("relation kinds = calls %d, control %d, data %d (total %d); want 4, 1, 1 (6)",
			len(kinds["calls"]), len(kinds["control_depends_on"]), len(kinds["data_flows_to"]), len(cap.relations))
	}
	edge("calls", "main", "helper")
	edge("calls", "main", "check")
	edge("calls", "sink", "Println")
	// sink(x) is guarded by check(x): sink control-depends on check.
	edge("control_depends_on", "sink", "check")
	// The REACHING_DEF chain helper() -> x := helper() (CSV) -> sink(x)
	// (GraphML only) is the forward, cross-export data dependence.
	if f := edge("data_flows_to", "helper", "sink"); f != nil && !strings.Contains(f.Evidence[0].Detail, "var=x") {
		t.Errorf("data dependence evidence detail = %q, want the variable", f.Evidence[0].Detail)
	}
	if f := edge("calls", "main", "sink"); f != nil {
		r := f.Evidence[0].Range
		if r == nil || fixtureSource[r.Start.Byte:r.End.Byte] != "sink(x)" || r.Start.Line != 20 {
			t.Errorf("call-site evidence range %+v does not select the call's bytes", r)
		}
	}
	var unknown, partial bool
	for _, c := range result.Capabilities {
		if c.Capability == "unsupported_label:FUTURE_EDGE" && c.State == model.CapabilityUnavailable {
			unknown = true
		}
		// The dropped ghost method is missing coverage and must be reported
		// as such, never as a fresh complete capability.
		if c.Capability == CapabilityCalls && c.State == model.CapabilityPartial && c.DiagnosticCode == model.CodeSourceBindingUnverified {
			partial = true
		}
	}
	if !unknown || !partial {
		t.Errorf("capabilities do not report the unknown label and the unbound method: %+v", result.Capabilities)
	}

	// An export record wider than the cap is refused by the bounded reader
	// (the limit it names), never handed to encoding/csv to buffer.
	t.Setenv(envFault, "oversized")
	h2 := providertest.New(t, fixtureFiles)
	_, u2, result2, err := run(t, p, h2)
	var typed *model.Error
	if !errors.As(err, &typed) || typed.Code != model.CodeResourceLimit || typed.Details["limit"] != "max_export_record_bytes" {
		t.Fatalf("oversized record: err = %v, want %s naming max_export_record_bytes", err, model.CodeResourceLimit)
	}
	if result2.State != model.RunFailed {
		t.Errorf("oversized record: run state = %s, want failed", result2.State)
	}
	if _, ok := h2.UnitState(t, u2.Build.Spec.ID); ok {
		t.Error("oversized record: the failed unit still exists")
	}
	requireCleanWorkDir(t, workDir)
}

// TestJoernExportFaultAdmitsNoFacts is the process-fault case: joern-export
// exits non-zero after writing half its files. Failure modes: partial export
// output admitted as facts (leaked unsealed facts), and the run directory,
// materialization or CPG surviving the failure (resource leak).
func TestJoernExportFaultAdmitsNoFacts(t *testing.T) {
	p, workDir := newProvider(t)
	t.Setenv(envFault, "export-midstream")
	h := providertest.New(t, fixtureFiles)
	cap, u, result, err := run(t, p, h)
	var typed *model.Error
	if !errors.As(err, &typed) || typed.Code != model.CodeProviderUnavailable {
		t.Fatalf("RunUnit err = %v, want the runner's %s for a non-zero exit", err, model.CodeProviderUnavailable)
	}
	if result.State != model.RunFailed || result.RecordsEmitted != 0 {
		t.Errorf("result = %+v, want a failed run with no records", result)
	}
	if len(cap.nodes)+len(cap.relations)+len(cap.aliases) != 0 {
		t.Errorf("facts reached storage from a failed export: %d nodes, %d relations, %d aliases", len(cap.nodes), len(cap.relations), len(cap.aliases))
	}
	if _, ok := h.UnitState(t, u.Build.Spec.ID); ok {
		t.Error("the failed unit still exists in storage")
	}
	requireCleanWorkDir(t, workDir)
}
