package context

import (
	stdcontext "context"
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Sawmonabo/codectx/internal/config"
	"github.com/Sawmonabo/codectx/internal/model"
	store "github.com/Sawmonabo/codectx/internal/storage/sqlite"
)

// contextFixture is the ONE Task 15 fixture: a small deterministic snapshot
// holding an implementation file, its caller, the interface it satisfies, its
// test, its documentation, its configuration and one oversized file, published
// as a single active generation. The unresolved boundary of Section 15.2 is a
// task token that names none of them, so it is supplied per row rather than
// stored here.
//
// Every fill-in lane shares this builder; a lane that needs one more artifact
// adds it through its row's setup hook rather than editing the builder.
type contextFixture struct {
	t       *testing.T
	ctx     stdcontext.Context
	Store   *store.Store
	Repo    model.RepositoryID
	Cfg     config.Config
	Files   map[string]model.FileVersion
	Gen     model.GenerationID
	Binding model.Binding
	Now     func() time.Time
}

// fixtureFiles is the deterministic content of the fixture snapshot, in the
// order it is published. Sizes are content-derived, so an assertion on a byte
// floor can be written against them without loading source bytes.
var fixtureFiles = []struct {
	path    string
	symbol  string
	kind    model.NodeKind
	content string
}{
	{"internal/order/ports.go", "Repository", model.NodeInterface, "package order\n\ntype Repository interface{ Save(Order) error }\n"},
	{"internal/order/service.go", "Place", model.NodeFunction, "package order\n\nfunc Place(r Repository, o Order) error { return r.Save(o) }\n"},
	{"internal/order/handler.go", "Handle", model.NodeFunction, "package order\n\nfunc Handle(r Repository) error { return Place(r, Order{}) }\n"},
	{"internal/order/service_test.go", "TestPlace", model.NodeTest, "package order\n\nfunc TestPlace(t *testing.T) { _ = Place }\n"},
	{"docs/order.md", "Orders", model.NodeDocument, "# Orders\n\nPlace writes through the Repository port.\n"},
	{"config/order.toml", "order", model.NodeConfiguration, "[order]\nmax_items = 10\n"},
	// The oversized artifact of Section 15.4. Its worst-case wire size,
	// 4*ceil(600_019/3) = 800_028 bytes, exceeds context.default_max_bytes
	// (524_288),
	// so a required entry over it raises CTX_MINIMUM_BUDGET under the default
	// budget rather than being truncated, split or demoted.
	{"internal/order/generated.go", "Generated", model.NodeFunction, "package order\n\n// " + strings.Repeat("x", 600_000) + "\n"},
}

// The fixture's one provider identity. Every unit, run and evidence row in the
// snapshot carries it, so a capability report has a single subject.
const (
	fixtureProviderID      = "treesitter"
	fixtureProviderVersion = "1.0.0"
	fixtureConfigHash      = "codectx-test-analysis-config"
)

// fixtureCapabilities is the fresh capability report the fixture generation
// activates with. A row that needs a partial or stale capability publishes its
// own generation through its setup hook.
var fixtureCapabilities = []model.CapabilityState{
	{ProviderID: "treesitter", Capability: "structure", Scope: "workspace", State: model.CapabilityFresh},
}

// fixtureNow is the frozen clock every compile in this scenario reads, so that
// CreatedAt is the only field a recompile is allowed to change.
func fixtureNow() time.Time { return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC) }

// newContextFixture opens an in-process store, publishes fixtureFiles as one
// snapshot and activates a generation over it.
func newContextFixture(t *testing.T) *contextFixture {
	t.Helper()
	ctx := stdcontext.Background()
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "codectx.db"), store.Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	fx := &contextFixture{t: t, ctx: ctx, Store: s, Cfg: config.Defaults(),
		Repo:  model.RepositoryID(model.H("codectx.test.repo", "task-15")),
		Files: map[string]model.FileVersion{}, Now: fixtureNow}
	if err := s.EnsureRepository(ctx, fx.Repo, "/repo"); err != nil {
		t.Fatalf("EnsureRepository: %v", err)
	}

	versions := make([]model.FileVersion, 0, len(fixtureFiles))
	var sourceBytes uint64
	for _, f := range fixtureFiles {
		versions = append(versions, fx.putBlob(f.path, f.content))
		sourceBytes += uint64(len(f.content))
	}
	manifest := model.H("codectx.test.manifest", "task-15")
	policy := model.H("codectx.test.source-policy")
	snap := model.Snapshot{
		ID:                 model.NewSnapshotID(fx.Repo, "", policy, manifest),
		RepositoryID:       fx.Repo,
		CaptureConsistency: model.CaptureValidated,
		SourcePolicyHash:   policy,
		FileCount:          uint64(len(versions)),
		SourceBytes:        sourceBytes,
		ManifestHash:       manifest,
		CreatedAt:          fixtureNow(),
	}
	err = s.PutSnapshot(ctx, snap, func(yield func(model.FileVersion) error) error {
		for _, fv := range versions {
			if err := yield(fv); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("PutSnapshot: %v", err)
	}
	if fx.Gen, err = s.BeginGeneration(ctx, fx.Repo, snap.ID, model.H("codectx.test.semantic"), "main"); err != nil {
		t.Fatalf("BeginGeneration: %v", err)
	}
	run, err := s.BeginProviderRun(ctx, fx.Gen, fixtureProviderID, fixtureProviderVersion)
	if err != nil {
		t.Fatalf("BeginProviderRun: %v", err)
	}
	for i, f := range fixtureFiles {
		fx.sealUnit(run, versions[i], f.symbol, f.kind)
	}
	if fx.Binding, err = s.Activate(ctx, fx.Gen, 0, model.HealthFresh, fixtureCapabilities, "norm-v1"); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	return fx
}

// putBlob stores one file's content and records its snapshot metadata. Status
// is tracked except for the implementation file, which is a captured change so
// the Section 15.3 active-change boost has an input.
func (f *contextFixture) putBlob(path, content string) model.FileVersion {
	f.t.Helper()
	sum := sha256.Sum256([]byte(content))
	hash := hex.EncodeToString(sum[:])
	rec := model.BlobRecord{Hash: hash, Size: int64(len(content))}
	for off := 0; off < len(content); off += model.BlobBlockBytes {
		end := min(off+model.BlobBlockBytes, len(content))
		d := sha256.Sum256([]byte(content[off:end]))
		rec.BlockDigests = append(rec.BlockDigests, hex.EncodeToString(d[:]))
	}
	rec.LineCheckpoints = []model.LineCheckpoint{{ByteOffset: 0, LineNumber: 1, LineStartByte: 0}}
	if err := f.Store.PutBlob(f.ctx, rec); err != nil {
		f.t.Fatalf("PutBlob(%s): %v", path, err)
	}
	status := model.FileTracked
	if path == "internal/order/service.go" {
		status = model.FileModified
	}
	fv := model.FileVersion{ID: model.NewFileID(f.Repo, path), Path: path, Status: status,
		Size: int64(len(content)), ContentHash: hash, Language: "go"}
	f.Files[path] = fv
	return fv
}

// sealUnit publishes one file-scoped unit holding the file's single node, its
// native alias and its lexical document, so Search.Resolve and Search.Search
// have something to resolve and the generation has units to activate.
func (f *contextFixture) sealUnit(run model.ProviderRunID, fv model.FileVersion, symbol string, kind model.NodeKind) {
	f.t.Helper()
	in := model.UnitInput{FileID: fv.ID, ContentHash: fv.ContentHash}
	h := model.NewUnitInputHasher()
	if err := h.Add(in); err != nil {
		f.t.Fatalf("unit input hash(%s): %v", fv.Path, err)
	}
	spec := model.UnitSpec{ProviderID: fixtureProviderID, ProviderVersion: fixtureProviderVersion,
		ScopeKey: fv.Path, InputHash: h.Sum(), DependencyHash: model.DependencyHash(nil)}
	spec.ID = model.NewUnitID(spec, fixtureConfigHash)
	build := model.UnitBuild{Spec: spec, AnalysisConfigHash: fixtureConfigHash, OriginRunID: run,
		SourceBinding: model.SourceBindingVerified}
	w, err := f.Store.BeginUnit(f.ctx, f.Gen, build, func(yield func(model.UnitInput) error) error { return yield(in) })
	if err != nil {
		f.t.Fatalf("BeginUnit(%s): %v", fv.Path, err)
	}
	key := model.CanonicalNodeKey(fv.Path, symbol)
	id := model.NewNodeID(f.Repo, kind, key)
	rng := &model.SourceRange{Start: model.Position{Byte: 0, Line: 1},
		End: model.Position{Byte: uint64(fv.Size), Line: 1}}
	ev := model.Evidence{UnitID: w.UnitID(), ProviderID: fixtureProviderID, ProviderVersion: fixtureProviderVersion,
		OriginRunID: run, NodeID: id, Precision: model.PrecisionSyntax, FileID: fv.ID,
		ContentHash: fv.ContentHash, Range: rng}
	ev.ID = model.NewEvidenceID(ev)
	node := model.Node{ID: id, Kind: kind, Language: "go", Name: symbol, QualifiedName: fv.Path + "." + symbol,
		FileID: fv.ID, ContentHash: fv.ContentHash, Range: rng}
	if err := w.PutNodes(f.ctx, []model.NodeFact{{Node: node, CanonicalKey: key, Evidence: []model.Evidence{ev}}}); err != nil {
		f.t.Fatalf("PutNodes(%s): %v", fv.Path, err)
	}
	if err := w.PutAliases(f.ctx, []model.NativeAlias{{ScopeKey: fv.Path, NativeKey: symbol, NodeID: id}}); err != nil {
		f.t.Fatalf("PutAliases(%s): %v", fv.Path, err)
	}
	doc := model.SearchUnit{ID: model.H("codectx.test.search", fv.Path, fv.ContentHash), NodeID: id, FileID: fv.ID,
		Path: fv.Path, Kind: kind, Name: symbol, QualifiedName: fv.Path + "." + symbol,
		Bytes: model.ByteRange{Start: 0, End: uint64(fv.Size)}, Body: symbol}
	if err := w.PutSearchUnits(f.ctx, []model.SearchUnit{doc}); err != nil {
		f.t.Fatalf("PutSearchUnits(%s): %v", fv.Path, err)
	}
	if err := f.Store.SealUnit(f.ctx, w); err != nil {
		f.t.Fatalf("SealUnit(%s): %v", fv.Path, err)
	}
}

// Node returns the fixture node identity for path, which is how a row names an
// expansion seed without recomputing the canonical key.
func (f *contextFixture) Node(path string) model.NodeID {
	f.t.Helper()
	for _, spec := range fixtureFiles {
		if spec.path == path {
			return model.NewNodeID(f.Repo, spec.kind, model.CanonicalNodeKey(path, spec.symbol))
		}
	}
	f.t.Fatalf("fixture has no node for %q", path)
	return ""
}

// File returns the fixture's snapshot metadata for path, failing the test if a
// row names a file the fixture does not publish.
func (f *contextFixture) File(path string) model.FileVersion {
	f.t.Helper()
	fv, ok := f.Files[path]
	if !ok {
		f.t.Fatalf("fixture has no file %q", path)
	}
	return fv
}

// contextScenarioRow is one row of the single Task 15 scenario table. Each row
// names the failure mode it guards, optionally extends the fixture through
// setup, and asserts in run. Lanes add rows under their own marker only.
type contextScenarioRow struct {
	// name states the invariant the row guards, not the mechanism it uses.
	name string
	// setup extends the shared fixture for this row alone; nil when the row
	// needs nothing beyond the published snapshot.
	setup func(t *testing.T, fx *contextFixture)
	// run compiles and asserts. A row that expects a typed failure asserts the
	// model.Error code, never a message substring.
	run func(t *testing.T, fx *contextFixture)
}

// TestContextCompilerScenario is the ONE Task 15 test. It builds the fixture
// once before the table so the builder is proved on every run, then gives each
// row its own fixture so no row can observe another's persisted manifest.
func TestContextCompilerScenario(t *testing.T) {
	base := newContextFixture(t)
	// The fixture itself is the scenario's precondition: a compile that pins a
	// generation cannot be written against a snapshot that never activated, and
	// with no rows yet this is the only thing proving the builder runs at all.
	if base.Binding.GenerationID != base.Gen || base.Binding.AnalysisKey == "" {
		t.Fatalf("fixture generation %d did not activate: binding %+v", base.Gen, base.Binding)
	}
	if got := len(base.Files); got != len(fixtureFiles) {
		t.Fatalf("fixture published %d files, want %d", got, len(fixtureFiles))
	}
	// The implementation file is the captured change the Section 15.3
	// active-change boost reads, and its node is what an expansion row seeds
	// from; a fixture that lost either would make those rows vacuous.
	if impl := base.File("internal/order/service.go"); impl.Status != model.FileModified {
		t.Fatalf("fixture implementation file status = %q, want %q", impl.Status, model.FileModified)
	}
	if base.Node("internal/order/service.go") == "" {
		t.Fatal("fixture published no node for the implementation file")
	}

	rows := []contextScenarioRow{
		// L1 SEEDS rows
		// L2 SCOPE rows
		// L3 RANK rows
		// L4 BUDGET rows
		// L5 MANIFEST rows
		// INT rows
	}

	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			fx := newContextFixture(t)
			if row.setup != nil {
				row.setup(t, fx)
			}
			row.run(t, fx)
		})
	}
}
