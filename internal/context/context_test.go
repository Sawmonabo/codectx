package context

import (
	stdcontext "context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Sawmonabo/codectx/internal/config"
	"github.com/Sawmonabo/codectx/internal/graph"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
	"github.com/Sawmonabo/codectx/internal/search"
	"github.com/Sawmonabo/codectx/internal/snapshot"
	"github.com/Sawmonabo/codectx/internal/source"
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
	t     *testing.T
	ctx   stdcontext.Context
	Store *store.Store
	// DBPath is the database file the store opened, so a row can close the
	// handle and reopen the SAME facts rather than a second copy of them.
	DBPath  string
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
	// Real lines, not one 600 KB line: sparse line checkpoints can only be
	// placed where a line begins, so a single-line artifact would leave the
	// file with one checkpoint at byte 0 however it was indexed. 15 + 600*1000
	// + 4 = 600_019 bytes either way.
	{"internal/order/generated.go", "Generated", model.NodeFunction, "package order\n\n" +
		strings.Repeat("// "+strings.Repeat("x", 996)+"\n", 600) + "//x\n"},
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
	dbPath := filepath.Join(t.TempDir(), "codectx.db")
	s, err := store.Open(ctx, dbPath, store.Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	fx := &contextFixture{t: t, ctx: ctx, Store: s, DBPath: dbPath, Cfg: config.Defaults(),
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
	// The block digests, the whole-file hash and the SPARSE LINE CHECKPOINTS
	// all come from the one streaming index the product computes at CAS
	// insertion time (source.BuildIndex, internal/snapshot/cas.go:185), rather
	// than from a hand-rolled loop here. A fixture that wrote a single
	// checkpoint at byte 0 would make any range deep inside a large file a
	// forward scan from byte zero, which serving refuses with
	// CTX_RESOURCE_LIMIT -- a defect of the fixture that would read as a defect
	// of the compiler.
	idx, err := source.BuildIndex(strings.NewReader(content))
	if err != nil {
		f.t.Fatalf("BuildIndex(%s): %v", path, err)
	}
	hash := idx.ContentHash
	rec := model.BlobRecord{Hash: hash, Size: int64(idx.Size), BlockDigests: idx.Blocks}
	for _, cp := range idx.Checkpoints {
		rec.LineCheckpoints = append(rec.LineCheckpoints,
			model.LineCheckpoint{ByteOffset: cp.Byte, LineNumber: cp.Line, LineStartByte: cp.Byte})
	}
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
		{
			// Guards Section 15.2: the seed steps run in their stated order, so
			// an identity the caller named can never be outranked by prose or by
			// a captured change, and an ambiguous name keeps every declaration.
			// A silent first pick here would hand the caller a confident plan
			// built on the wrong declaration, with nothing in the manifest
			// saying a choice was made.
			name: "seeds resolve in Section 15.2 step order and keep every declaration of an ambiguous name",
			// The published fixture names every symbol once, so ambiguity has to
			// be built: this republishes the same snapshot as a second activated
			// generation in which the caller file also declares Place.
			setup: func(t *testing.T, fx *contextFixture) {
				// Units are content-addressed and shared across generations, so a
				// second declaration of one name needs a second file: this
				// republishes the snapshot with two files that both declare Place
				// and activates a generation over them.
				prior := fx.Gen
				ambiguous := []string{"internal/order/place_read.go", "internal/order/place_write.go"}
				versions := make([]model.FileVersion, 0, len(fixtureFiles)+len(ambiguous))
				var sourceBytes uint64
				// The oversized artifact is left out: it carries one line
				// checkpoint for 600 KB, so hydrating a lexical hit inside it is a
				// typed resource limit that would stop the lexical step before it
				// contributes. Section 15.4 budgeting is the L4 row's subject, not
				// this one's.
				for _, spec := range fixtureFiles {
					if spec.path == "internal/order/generated.go" {
						continue
					}
					versions = append(versions, fx.File(spec.path))
					sourceBytes += uint64(len(spec.content))
				}
				for _, path := range ambiguous {
					// Reusing the implementation file's content keeps the blob the
					// row's CAS already holds while giving each path its own file
					// identity, so the two declarations are genuinely distinct.
					body := fixtureFiles[1].content
					versions = append(versions, fx.putBlob(path, body))
					sourceBytes += uint64(len(body))
				}
				manifest := model.H("codectx.test.manifest", "task-15-ambiguous")
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
				err := fx.Store.PutSnapshot(fx.ctx, snap, func(yield func(model.FileVersion) error) error {
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
				if fx.Gen, err = fx.Store.BeginGeneration(fx.ctx, fx.Repo, snap.ID,
					model.H("codectx.test.semantic", "ambiguous"), "main"); err != nil {
					t.Fatalf("BeginGeneration: %v", err)
				}
				run, err := fx.Store.BeginProviderRun(fx.ctx, fx.Gen, fixtureProviderID, fixtureProviderVersion)
				if err != nil {
					t.Fatalf("BeginProviderRun: %v", err)
				}
				// Units are content-addressed and already sealed, so the snapshot's
				// original facts are attached rather than rebuilt: without them the
				// generation would answer neither the exact-resolution step nor the
				// lexical one, and the row would silently prove four steps of seven.
				for _, spec := range fixtureFiles {
					if spec.path == "internal/order/generated.go" {
						continue
					}
					fv := fx.File(spec.path)
					in := model.UnitInput{FileID: fv.ID, ContentHash: fv.ContentHash}
					h := model.NewUnitInputHasher()
					if err := h.Add(in); err != nil {
						t.Fatalf("unit input hash(%s): %v", fv.Path, err)
					}
					id := model.NewUnitID(model.UnitSpec{ProviderID: fixtureProviderID,
						ProviderVersion: fixtureProviderVersion, ScopeKey: fv.Path,
						InputHash: h.Sum(), DependencyHash: model.DependencyHash(nil)}, fixtureConfigHash)
					if err := fx.Store.AttachUnit(fx.ctx, fx.Gen, id); err != nil {
						t.Fatalf("AttachUnit(%s): %v", fv.Path, err)
					}
				}
				for _, path := range ambiguous {
					fx.sealUnit(run, fx.File(path), "Place", model.NodeFunction)
				}
				if fx.Binding, err = fx.Store.Activate(fx.ctx, fx.Gen, prior, model.HealthFresh, fixtureCapabilities, "norm-v1"); err != nil {
					t.Fatalf("Activate: %v", err)
				}
			},
			run: func(t *testing.T, fx *contextFixture) {
				// The shared fixture composes no discovery service, so the row
				// builds the one it needs: seed resolution is defined in terms of
				// Search.Resolve and Search.Search, and a fake would be proving a
				// fake.
				svc := searchService(t, fx)
				reader, err := fx.Store.PinGeneration(fx.ctx, fx.Repo, fx.Gen, time.Minute)
				if err != nil {
					t.Fatalf("PinGeneration: %v", err)
				}
				defer reader.Close()
				c := &Compiler{store: fx.Store, repo: fx.Repo, search: svc, cfg: fx.Cfg,
					now: fx.Now, log: slog.New(slog.NewTextHandler(io.Discard, nil))}

				// One task mixing an explicit seed, a backticked identifier, a
				// path token, a free term and a path that names nothing.
				seeds, err := c.extractSeeds(fx.ctx, reader, fx.Gen, model.ContextRequest{
					Task:  "Update `Place` so internal/order/handler.go keeps working; check Repository and docs/missing.md",
					Seeds: []string{"internal/order/ports.go"},
					Phase: model.PhaseVerify,
				})
				if err != nil {
					t.Fatalf("extractSeeds: %v", err)
				}

				// Every Section 15.2 step contributes, so the order assertion below
				// covers the whole chain instead of a subset of it.
				contributed := map[originKind]bool{}
				for _, cnd := range seeds.Candidates {
					contributed[cnd.Origin] = true
				}
				for _, step := range []originKind{originExplicitSeed, originBacktick, originPathToken,
					originExactResolve, originLexical, originChangedFile} {
					if !contributed[step] {
						t.Fatalf("Section 15.2 step %d produced no seed; the row would prove only part of the order", step)
					}
				}
				// The steps append in order, so the origins a compile produces are
				// non-decreasing. Reordering any two steps breaks this.
				for i := 1; i < len(seeds.Candidates); i++ {
					if seeds.Candidates[i].Origin < seeds.Candidates[i-1].Origin {
						t.Fatalf("seed %d has origin %d after origin %d: extraction ran out of Section 15.2 order",
							i, seeds.Candidates[i].Origin, seeds.Candidates[i-1].Origin)
					}
				}
				byPath := map[string]candidate{}
				for _, cnd := range seeds.Candidates {
					if cnd.NodeID == "" {
						byPath[cnd.Path] = cnd
					}
				}
				if got := byPath["internal/order/ports.go"].Origin; got != originExplicitSeed {
					t.Fatalf("the explicit seed carries origin %d, want originExplicitSeed", got)
				}
				if got := byPath["internal/order/handler.go"].Origin; got != originPathToken {
					t.Fatalf("the path token carries origin %d, want originPathToken", got)
				}

				// Ambiguity is preserved: every declaration the resolver returns
				// for the backticked name is its own candidate, none is dropped.
				page, err := svc.Resolve(fx.ctx, model.SymbolRequest{GenerationID: fx.Gen, Query: "Place",
					Operation: model.SymbolResolve, SemanticSource: model.SemanticCanonical,
					Page: model.PageRequest{Limit: model.MaxPageItems}})
				if err != nil {
					t.Fatalf("Resolve: %v", err)
				}
				if len(page.Items) < 2 {
					t.Fatalf("the fixture resolves %q to %d declarations; the row needs an ambiguous name", "Place", len(page.Items))
				}
				declared := map[model.NodeID]bool{}
				for _, cnd := range seeds.Candidates {
					if cnd.Origin == originBacktick && cnd.NodeID != "" {
						declared[cnd.NodeID] = true
					}
				}
				if len(declared) != len(page.Items) {
					t.Fatalf("the backticked name produced %d seeds for %d declarations: a candidate was silently chosen or dropped",
						len(declared), len(page.Items))
				}
				for _, n := range page.Items {
					if !declared[n.ID] {
						t.Fatalf("declaration %s of the ambiguous name is missing from the seeds", n.ID)
					}
				}

				// An identity that resolves to nothing is reported with a reason
				// and leaves the scope incomplete; it is never dropped in silence.
				if !seeds.Unresolved {
					t.Fatal("a task naming an absent path left Unresolved false")
				}
				var reason string
				for _, ex := range seeds.Excluded {
					if ex.Path == "docs/missing.md" {
						reason = ex.Excluded
					}
				}
				if reason == "" {
					t.Fatalf("the absent path is not an exclusion with a reason; exclusions were %+v", seeds.Excluded)
				}
			},
		},
		// L2 SCOPE rows
		{
			name: "required scope pulls every decision boundary of the seed",
			// Guards Section 15.2: a plan that omits the caller, the contract,
			// the test or the configuration of a file it asks an actor to
			// change is not implementation ready, however well the omitted
			// entries would have scored.
			run: func(t *testing.T, fx *contextFixture) {
				impl := "internal/order/service.go"
				eng := fx.scopeEngine([]model.Relation{
					fx.edge("internal/order/handler.go", model.RelCalls, impl),
					fx.edge(impl, model.RelImplements, "internal/order/ports.go"),
					fx.edge("internal/order/service_test.go", model.RelTests, impl),
					fx.edge("config/order.toml", model.RelConfigures, impl),
					fx.edge("docs/order.md", model.RelDocuments, impl),
				}, fixtureCapabilities)
				// The same file reaches scope from two Section 15.2 steps: an
				// explicit seed that is also a captured change. One entity must
				// yield one entry -- context_entries is keyed by ordinal, so a
				// duplicate would persist as a second entry for one node.
				changed := fx.seedOf(impl)
				changed.Origin = originChangedFile
				res, err := expandScope(fx.ctx, eng, fx.Gen, fx.Cfg.Context,
					[]candidate{fx.seedOf(impl), changed}, fixtureCapabilities)
				if err != nil {
					t.Fatalf("expandScope: %v", err)
				}
				seen := 0
				for _, c := range res.Candidates {
					if c.Path == impl {
						seen++
					}
				}
				if seen != 1 {
					t.Fatalf("one entity produced %d entries; a manifest may hold only one", seen)
				}
				if !res.ScopeComplete {
					t.Fatalf("a fresh, untruncated expansion reported incomplete scope: %+v", res.Completeness)
				}
				for _, want := range []struct {
					path string
					req  model.Requirement
				}{
					{impl, model.RequirementFull},                             // the seed itself
					{"internal/order/ports.go", model.RequirementFull},        // contract
					{"internal/order/service_test.go", model.RequirementFull}, // test
					{"config/order.toml", model.RequirementFull},              // configuration
					{"internal/order/handler.go", model.RequirementSymbol},    // caller
					{"docs/order.md", model.RequirementRecommended},           // documentation
				} {
					got, ok := requirementFor(t, fx, res, want.path)
					if !ok {
						t.Fatalf("scope dropped the boundary %q", want.path)
					}
					if got.Requirement != want.req {
						t.Fatalf("boundary %q requirement = %q, want %q", want.path, got.Requirement, want.req)
					}
				}
				// candidate.Path is a FILE PATH. An expansion entry has none of
				// its own -- ImpactEntry.Name is a qualified name -- so it must
				// arrive empty for hydrateFiles to fill from the FileID.
				// A name written here is never corrected (hydration fills Path
				// only when empty) and reaches the stored manifest reference,
				// and so its canonical hash, as a path naming no file.
				for _, c := range res.Candidates {
					if c.Origin == originExpansion && c.Path != "" {
						t.Fatalf("expansion entry carries path %q of its own; hydration can no longer fill the real one", c.Path)
					}
				}
			},
		},
		{
			name: "an unresolved token is discovery but an unresolved explicit seed is an error",
			// Guards ruling Q7 from both sides: a task token nothing matched
			// must NOT fail the compile (it yields a discovery answer whose
			// omission is visible as a reasoned exclusion), while an explicit
			// seed the caller named and the snapshot does not hold MUST fail
			// rather than compile a plan quietly built around it.
			run: func(t *testing.T, fx *contextFixture) {
				impl := "internal/order/service.go"
				eng := fx.scopeEngine([]model.Relation{
					fx.edge("internal/order/handler.go", model.RelCalls, impl),
				}, fixtureCapabilities)
				unresolved := candidate{Path: "Ledger", Origin: originLexical,
					Excluded: "no symbol or path in the pinned snapshot matched"}

				res, err := expandScope(fx.ctx, eng, fx.Gen, fx.Cfg.Context,
					[]candidate{fx.seedOf(impl), unresolved}, fixtureCapabilities)
				if err != nil {
					t.Fatalf("an unresolved extracted token must not fail the compile: %v", err)
				}
				if res.ScopeComplete {
					t.Fatal("an unresolved token left the scope reported as complete")
				}
				kept, ok := requirementFor(t, fx, res, "Ledger")
				if !ok || kept.Excluded == "" {
					t.Fatalf("the unresolved token lost its exclusion: %+v", kept)
				}
				if kept.Requirement != "" {
					t.Fatalf("an unresolved token was marked %q; a discovery answer has no required entry for it", kept.Requirement)
				}

				unresolved.Origin = originExplicitSeed
				_, err = expandScope(fx.ctx, eng, fx.Gen, fx.Cfg.Context,
					[]candidate{fx.seedOf(impl), unresolved}, fixtureCapabilities)
				var typed *model.Error
				if !errors.As(err, &typed) || typed.Code != model.CodeScopeIncomplete {
					t.Fatalf("an unresolvable explicit seed returned %v, want %s", err, model.CodeScopeIncomplete)
				}
			},
		},
		{
			name: "a truncated or degraded expansion can never report complete scope",
			// Guards Section 15.2's one-way rule: truncation and a non-fresh
			// capability are what make an answer partial, and no later pass may
			// set ScopeComplete back to true. A plan that claims completeness
			// over a walk that stopped early grants false readiness.
			run: func(t *testing.T, fx *contextFixture) {
				impl := "internal/order/service.go"
				rels := []model.Relation{
					fx.edge("internal/order/handler.go", model.RelCalls, impl),
					fx.edge(impl, model.RelImplements, "internal/order/ports.go"),
					fx.edge("internal/order/service_test.go", model.RelTests, impl),
				}
				cfg := fx.Cfg.Context
				cfg.MaxGraphEdges = 1 // the walk must stop before the last edge
				res, err := expandScope(fx.ctx, fx.scopeEngine(rels, fixtureCapabilities), fx.Gen, cfg,
					[]candidate{fx.seedOf(impl)}, fixtureCapabilities)
				if err != nil {
					t.Fatalf("expandScope: %v", err)
				}
				if res.ScopeComplete {
					t.Fatal("an edge-budget truncation reported complete scope")
				}

				stale := []model.CapabilityState{{ProviderID: "treesitter", Capability: "structure",
					Scope: "workspace", State: model.CapabilityStale}}
				res, err = expandScope(fx.ctx, fx.scopeEngine(rels, stale), fx.Gen, fx.Cfg.Context,
					[]candidate{fx.seedOf(impl)}, stale)
				if err != nil {
					t.Fatalf("expandScope: %v", err)
				}
				if res.ScopeComplete {
					t.Fatal("a stale capability reported complete scope")
				}
				if len(res.Completeness) != 1 || res.Completeness[0].State != model.CapabilityStale {
					t.Fatalf("the manifest header lost the capability row behind the verdict: %+v", res.Completeness)
				}
			},
		},
		{
			name: "a lexical or changed-file seed never enters the required_full prefix",
			// Guards Section 15.2 and ruling Q7: only a named identity is
			// required in full. Promoting a merely lexical hit or a captured
			// change makes it mandatory full reading for Task 16's coverage
			// gate -- a required entry is never demoted or dropped later -- and
			// reorders the manifest, because Requirement is the first Section
			// 15.3 tie-break key. Nothing fails when this breaks; the actor is
			// simply told to read whole files the task never named.
			run: func(t *testing.T, fx *contextFixture) {
				lexical := fx.seedOf("internal/order/handler.go")
				lexical.NodeID, lexical.Origin = "", originLexical
				lexical.Requirement = model.RequirementRecommended
				changed := fx.seedOf("internal/order/service.go")
				changed.NodeID, changed.Origin = "", originChangedFile
				changed.Requirement = model.RequirementOptional

				// No seed resolves a symbol, so no boundary can be walked: this
				// is the discovery answer of ruling Q7, not an implementation
				// plan, and it must hold no required entry at all.
				res, err := expandScope(fx.ctx, fx.scopeEngine(nil, fixtureCapabilities), fx.Gen,
					fx.Cfg.Context, []candidate{lexical, changed}, fixtureCapabilities)
				if err != nil {
					t.Fatalf("expandScope: %v", err)
				}
				if res.ScopeComplete {
					t.Fatal("a scope that resolved no boundary reported complete scope")
				}
				for _, want := range []struct {
					path string
					req  model.Requirement
				}{
					{"internal/order/handler.go", model.RequirementRecommended},
					{"internal/order/service.go", model.RequirementOptional},
				} {
					got, ok := requirementFor(t, fx, res, want.path)
					if !ok {
						t.Fatalf("scope dropped the seed %q", want.path)
					}
					if got.Requirement != want.req {
						t.Fatalf("seed %q requirement = %q, want %q", want.path, got.Requirement, want.req)
					}
				}
				for _, c := range res.Candidates {
					if c.Requirement == model.RequirementFull {
						t.Fatalf("%q entered the required_full prefix on a discovery answer", c.Path)
					}
				}
			},
		},
		// L3 RANK rows
		{
			// Guards the Section 15.3 path contribution: the product of
			// weight(kind) and the per-edge precision multiplier, decayed once
			// per hop AFTER the first. Silent breakage here does not fail any
			// compile -- it just ranks a distant entity as if it were adjacent,
			// so the manifest quietly leads with the wrong files.
			name: "path contribution multiplies per-edge precision and decays once per hop after the first",
			run: func(t *testing.T, fx *contextFixture) {
				rels := map[model.RelationID]model.Relation{
					"r1": {ID: "r1", Kind: model.RelCalls},
					"r2": {ID: "r2", Kind: model.RelImplements},
				}
				compiler := map[model.RelationID]int64{"r1": precisionMultiplier[model.PrecisionCompiler]}
				heuristic := map[model.RelationID]int64{"r1": precisionMultiplier[model.PrecisionHeuristic]}
				twoHop := map[model.RelationID]int64{
					"r1": precisionMultiplier[model.PrecisionCompiler],
					"r2": precisionMultiplier[model.PrecisionSyntax],
				}
				cases := []struct {
					name string
					path model.RelationPath
					prec map[model.RelationID]int64
					want int64
				}{
					// calls 900_000 * compiler 1_000_000.
					{"compiler call hop", model.RelationPath{Relations: []model.RelationID{"r1"}}, compiler, 900_000},
					// calls 900_000 * heuristic 450_000: an unevidenced edge is
					// ranked as the weakest precision, never as a free one.
					{"heuristic call hop", model.RelationPath{Relations: []model.RelationID{"r1"}}, heuristic, 405_000},
					// (900_000) * (implements 940_000 * syntax 720_000) * decay 650_000.
					{"decayed second hop", model.RelationPath{Relations: []model.RelationID{"r1", "r2"}}, twoHop, 395_928},
				}
				for _, tc := range cases {
					got, kinds, ok, err := scorePath(tc.path, rels, tc.prec)
					if err != nil || !ok {
						t.Fatalf("%s: scorePath ok=%v err=%v", tc.name, ok, err)
					}
					if got != tc.want {
						t.Fatalf("%s: contribution = %d micros, want %d", tc.name, got, tc.want)
					}
					if len(kinds) != len(tc.path.Relations) {
						t.Fatalf("%s: reported %d hop kinds, want %d", tc.name, len(kinds), len(tc.path.Relations))
					}
				}
				// A hop this compile did not walk, or one whose kind carries no
				// Section 15.3 contribution, makes the route inadmissible: scored
				// as 1.0 it would rank an unexplained route above an explained one.
				if _, _, ok, err := scorePath(model.RelationPath{Relations: []model.RelationID{"unwalked"}}, rels, compiler); ok || err != nil {
					t.Fatalf("unwalked edge admitted: ok=%v err=%v", ok, err)
				}
				_ = fx
			},
		},
		{
			// Guards the rest of Section 15.3 through the pinned reader: an edge
			// with no visible evidence falls back to heuristic precision, each
			// bounded boost is added at most once, and the tie-break chain is
			// total. A broken order is invisible here but breaks Task 16, whose
			// `context next` is an ordinal walk over this order.
			name: "ranking falls back to heuristic precision and orders by the full tie-break chain",
			run: func(t *testing.T, fx *contextFixture) {
				reader, err := fx.Store.PinGeneration(fx.ctx, fx.Repo, fx.Gen, time.Minute)
				if err != nil {
					t.Fatalf("PinGeneration: %v", err)
				}
				defer reader.Close()

				impl, caller := fx.File("internal/order/service.go"), fx.File("internal/order/handler.go")
				edge := model.NewRelationID(fx.Repo, fx.Node("internal/order/handler.go"), model.RelCalls,
					fx.Node("internal/order/service.go"))
				// The fixture seals node evidence only, so this edge has no
				// evidence row and must take the heuristic multiplier.
				second := model.NewRelationID(fx.Repo, fx.Node("internal/order/handler.go"), model.RelImports,
					fx.Node("internal/order/service.go"))
				rels := map[model.RelationID]model.Relation{
					edge: {ID: edge, From: fx.Node("internal/order/handler.go"), Kind: model.RelCalls,
						To: fx.Node("internal/order/service.go")},
					// A weaker second route to the same entity: it cannot raise
					// the maximum, and with one retained path it is the route
					// MorePaths must disclose.
					second: {ID: second, From: fx.Node("internal/order/handler.go"), Kind: model.RelImports,
						To: fx.Node("internal/order/service.go")},
				}

				cands := []candidate{
					{FileID: impl.ID, Path: impl.Path, Requirement: model.RequirementFull,
						Origin: originExplicitSeed, Status: impl.Status},
					{NodeID: fx.Node("internal/order/handler.go"), FileID: caller.ID, Path: caller.Path,
						Requirement: model.RequirementRecommended, Origin: originExpansion, Depth: 1,
						Status: caller.Status, Paths: []model.RelationPath{
							{Relations: []model.RelationID{edge}},
							{Relations: []model.RelationID{second}},
							// A third admissible route, too long to store. It
							// reuses the same two relation ids, so it adds no
							// distinct edge to the package centrality and
							// changes no score -- only the count MorePaths owes.
							{Relations: longRoute(edge, second)},
						}},
					{NodeID: "n2", Path: "a.go", StartByte: 5, Requirement: model.RequirementOptional, Origin: originLexical},
					{NodeID: "n3", Path: "a.go", StartByte: 0, Requirement: model.RequirementOptional, Origin: originLexical},
					{NodeID: "n1", Path: "b.go", StartByte: 0, Requirement: model.RequirementOptional, Origin: originLexical},
					{NodeID: "n1", Path: "a.go", StartByte: 5, Requirement: model.RequirementOptional, Origin: originLexical},
				}
				// One retained path, so the second admitted route can only be
				// disclosed as a count.
				cfg := fx.Cfg
				cfg.Context.MaxReasonPathsPerEntry = 1
				ranked, err := (&Compiler{cfg: cfg}).rank(fx.ctx, reader, cands, rels)
				if err != nil {
					t.Fatalf("rank: %v", err)
				}
				// rank scores but does not order: buildPlan rewrites Path and
				// sorts, so the tie-break chain is asserted over the same total
				// order the plan applies.
				sort.SliceStable(ranked, func(i, j int) bool { return ranked[i].less(ranked[j]) })

				// Seed 1_000_000 + captured change 120_000 + two walked edges of
				// package centrality 10_000; caller 405_000 (the stronger call
				// route, never a sum over both) + that same 10_000.
				// The optional candidates sit in the repository root, which this
				// compile walked no edge in, so they score nothing at all and the
				// tie-break chain alone decides their order.
				type want struct {
					path  string
					id    model.NodeID
					score int64
				}
				expect := []want{
					{impl.Path, "", 1_130_000},
					{caller.Path, fx.Node("internal/order/handler.go"), 415_000},
					{"a.go", "n3", 0},
					{"a.go", "n1", 0},
					{"a.go", "n2", 0},
					{"b.go", "n1", 0},
				}
				if len(ranked) != len(expect) {
					t.Fatalf("rank returned %d candidates, want %d", len(ranked), len(expect))
				}
				for i, w := range expect {
					got := ranked[i]
					if got.Path != w.path || got.NodeID != w.id || got.ScoreMicros != w.score {
						t.Fatalf("rank[%d] = {path %q node %q score %d}, want {path %q node %q score %d}",
							i, got.Path, got.NodeID, got.ScoreMicros, w.path, w.id, w.score)
					}
					if len(got.Reasons) > model.MaxReasonsPerEntry {
						t.Fatalf("rank[%d] carries %d reasons, over the bound %d", i, len(got.Reasons), model.MaxReasonsPerEntry)
					}
				}
				// Three admitted routes, one retained: the two the manifest
				// cannot enumerate must BOTH be disclosed -- the weaker short
				// route AND the route dropped for exceeding
				// model.MaxRelationsPerPath. Counting only the retainable tail
				// would understate the routes that exist, which is the one
				// channel model.ContextEntry has for them, and a bounded
				// explanation would then read as an unexplained selection.
				if ranked[1].MorePaths != 2 || len(ranked[1].Paths) != 1 {
					t.Fatalf("caller retained %d paths with MorePaths %d, want 1 and 2",
						len(ranked[1].Paths), ranked[1].MorePaths)
				}
				if !slices.Contains(ranked[1].Reasons, "2 further route(s) reach this entity and are not enumerated") {
					t.Fatalf("the unenumerated routes were never disclosed: %q", ranked[1].Reasons)
				}
			},
		},
		// L4 BUDGET rows
		{
			// Guards the Section 15.4 rule that entry overhead is MEASURED, not
			// assumed: a budget that counted only the wire-encoded source would
			// under-count every header, delimiter and escape and overfill a
			// slice the caller was promised would fit.
			name: "entry sizes count measured metadata on top of worst-case wire bytes",
			run: func(t *testing.T, fx *contextFixture) {
				paths := []string{"internal/order/ports.go", "internal/order/service.go", "internal/order/handler.go"}
				cands, files := budgetRowInput(fx, paths, model.RequirementFull)
				b, err := resolveBudget(model.Budget{}, fx.Cfg.Context)
				if err != nil {
					t.Fatalf("resolveBudget: %v", err)
				}
				// A zero budget resolves to the configured default, never to
				// unlimited (Section 20.2).
				if b.MaxBytes != fx.Cfg.Context.DefaultMaxBytes || b.MaxSlices != fx.Cfg.Context.MaxSlices {
					t.Fatalf("zero budget resolved to %+v, want the configured context defaults", b)
				}
				p, err := buildPlan(cands, files, b)
				if err != nil {
					t.Fatalf("buildPlan: %v", err)
				}
				if len(p.Entries) != len(paths) {
					t.Fatalf("plan holds %d entries, want %d", len(p.Entries), len(paths))
				}
				for _, e := range p.Entries {
					wire, err := wireEncodedBytes(fx.Files[pathOfEntry(t, fx, e)].Size)
					if err != nil {
						t.Fatalf("wireEncodedBytes: %v", err)
					}
					raw, err := json.Marshal(e)
					if err != nil {
						t.Fatalf("Marshal: %v", err)
					}
					// The measured size is a fixed point: the stored entry,
					// serialized exactly as persisted, plus its worst-case
					// source encoding, is the number the entry reports.
					if want := wire + int64(len(raw)); e.EstimatedBytes != want {
						t.Errorf("entry %d estimated_bytes = %d, want %d (wire %d + measured metadata %d)",
							e.Ordinal, e.EstimatedBytes, want, wire, len(raw))
					}
					tokens, err := model.EstimateTokensUTF8Bytes(e.EstimatedBytes)
					if err != nil {
						t.Fatalf("EstimateTokensUTF8Bytes: %v", err)
					}
					if e.EstimatedTokens != tokens {
						t.Errorf("entry %d estimated_tokens = %d, want %d", e.Ordinal, e.EstimatedTokens, tokens)
					}
				}
			},
		},
		{
			// Guards Section 15.4's "necessary floor": a file is transported
			// ONCE however many symbols selected it, so charging its source to
			// every entry over it budgets, floors and persists the file at a
			// multiple of its real cost. The damage is not cosmetic — a plan
			// that fits is refused with CTX_MINIMUM_BUDGET naming a budget
			// larger than it needs, and a caller raising the budget to that
			// floor is following a number that was never the requirement.
			name: "a file selected through several symbols is charged its source once",
			run: func(t *testing.T, fx *contextFixture) {
				const path = "internal/order/ports.go"
				cands, files := budgetRowInput(fx, []string{path}, model.RequirementFull)
				// A second symbol of the SAME file: the port's method. Two
				// entries, one transported file.
				second := cands[0]
				second.NodeID = model.NewNodeID(fx.Repo, model.NodeFunction, model.CanonicalNodeKey(path, "Save"))
				second.ScoreMicros = cands[0].ScoreMicros - 1
				cands = append(cands, second)
				wire, err := wireEncodedBytes(fx.File(path).Size)
				if err != nil {
					t.Fatalf("wireEncodedBytes: %v", err)
				}
				// The file costs 676 bytes charged once and 760 charged per
				// entry, so this cap admits exactly one of the two rules. The
				// guard below fails the row rather than letting it go vacuous
				// if the entry metadata ever changes size.
				const sliceCap = 700
				b, err := resolveBudget(model.Budget{MaxBytes: sliceCap}, fx.Cfg.Context)
				if err != nil {
					t.Fatalf("resolveBudget: %v", err)
				}
				p, err := buildPlan(cands, files, b)
				if err != nil {
					t.Fatalf("a required file reached through two symbols did not fit a budget that holds it: %v", err)
				}
				if len(p.Entries) != 2 || len(p.Slices) != 1 {
					t.Fatalf("plan holds %d entries in %d slices, want both symbols in one slice", len(p.Entries), len(p.Slices))
				}
				// The first entry of the file carries the source; its sibling
				// carries its own measured metadata and nothing else.
				raw0, err := json.Marshal(p.Entries[0])
				if err != nil {
					t.Fatalf("Marshal: %v", err)
				}
				if want := wire + int64(len(raw0)); p.Entries[0].EstimatedBytes != want {
					t.Errorf("entry 0 estimated_bytes = %d, want %d (wire %d + measured metadata %d)",
						p.Entries[0].EstimatedBytes, want, wire, len(raw0))
				}
				raw1, err := json.Marshal(p.Entries[1])
				if err != nil {
					t.Fatalf("Marshal: %v", err)
				}
				if want := int64(len(raw1)); p.Entries[1].EstimatedBytes != want {
					t.Errorf("entry 1 estimated_bytes = %d, want %d (metadata alone: the file's %d wire bytes are already charged to entry 0)",
						p.Entries[1].EstimatedBytes, want, wire)
				}
				total := p.Entries[0].EstimatedBytes + p.Entries[1].EstimatedBytes
				if p.Slices[0].EstimatedBytes != total {
					t.Errorf("slice 0 holds %d bytes, want the %d its entries report; the packer and the store must agree",
						p.Slices[0].EstimatedBytes, total)
				}
				if total+wire <= b.MaxBytes {
					t.Fatalf("the cap %d no longer discriminates between per-file and per-entry charging (per-file %d, per-entry %d)",
						b.MaxBytes, total, total+wire)
				}
			},
		},
		{
			// Guards the Section 15.4 stored-manifest cap, which the per-slice
			// bounds do not imply: MaxSlices slices of MaxBytes each can be a
			// manifest far larger than context.max_manifest_bytes. Without the
			// check a compile silently persists a manifest above the
			// deployment's own bound, and required scope is exempt from being
			// dropped, never from the cap.
			name: "a plan over the stored-manifest byte cap is refused rather than persisted",
			run: func(t *testing.T, fx *contextFixture) {
				paths := []string{"internal/order/ports.go", "internal/order/service.go"}
				cands, files := budgetRowInput(fx, paths, model.RequirementFull)
				cfg := fx.Cfg.Context
				// Small enough that the two required files exceed it together,
				// while every per-slice bound stays at its default.
				cfg.MaxManifestBytes = 400
				b, err := resolveBudget(model.Budget{}, cfg)
				if err != nil {
					t.Fatalf("resolveBudget: %v", err)
				}
				p, err := buildPlan(cands, files, b)
				if err == nil {
					var total int64
					for _, s := range p.Slices {
						total += s.EstimatedBytes
					}
					t.Fatalf("a %d-byte manifest was accepted under a %s-byte cap", total, b.MaxManifestBytes)
				}
				var got *model.Error
				if !errors.As(err, &got) || got.Code != model.CodeResourceLimit {
					t.Fatalf("buildPlan error = %v, want code %s", err, model.CodeResourceLimit)
				}
				if got.Details["cap"] != "context.max_manifest_bytes" {
					t.Errorf("error names cap %q, want the configuration key that caused it", got.Details["cap"])
				}
				bytes, err := strconv.ParseInt(got.Details["manifest_bytes"], 10, 64)
				if err != nil {
					t.Fatalf("manifest_bytes = %q, want the measured total: %v", got.Details["manifest_bytes"], err)
				}
				if !b.MaxManifestBytes.Exceeded(bytes) {
					t.Errorf("manifest_bytes = %d, which does not exceed the cap %s the refusal cites", bytes, b.MaxManifestBytes)
				}
				// The refusal must be the manifest cap, not a per-slice bound
				// reached first, or the row would prove the wrong check.
				if bytes > b.MaxBytes {
					t.Errorf("the plan's %d bytes also exceed the per-slice budget %d; the row no longer isolates the manifest cap", bytes, b.MaxBytes)
				}
			},
		},
		{
			// THE Task 15 invariant: required scope never silently shrinks to
			// fit a budget. A required file too large for one slice must raise
			// the typed floor, never be demoted, truncated, or relabelled.
			name: "a required file larger than the byte budget raises the minimum-budget floor",
			run: func(t *testing.T, fx *contextFixture) {
				oversized := "internal/order/generated.go"
				cands, files := budgetRowInput(fx, []string{oversized}, model.RequirementFull)
				b, err := resolveBudget(model.Budget{}, fx.Cfg.Context)
				if err != nil {
					t.Fatalf("resolveBudget: %v", err)
				}
				wire, err := wireEncodedBytes(fx.File(oversized).Size)
				if err != nil {
					t.Fatalf("wireEncodedBytes: %v", err)
				}
				if wire <= b.MaxBytes {
					t.Fatalf("fixture file is not oversized: wire %d <= budget %d", wire, b.MaxBytes)
				}
				p, err := buildPlan(cands, files, b)
				if err == nil {
					t.Fatalf("an oversized required file produced a plan with %d entries and %d slices instead of the minimum-budget floor",
						len(p.Entries), len(p.Slices))
				}
				var got *model.Error
				if !errors.As(err, &got) || got.Code != model.CodeMinimumBudget {
					t.Fatalf("buildPlan error = %v, want code %s", err, model.CodeMinimumBudget)
				}
				floor, ok := got.Details["min_bytes"]
				if !ok {
					t.Fatalf("minimum-budget error carries no min_bytes; details %v", got.Details)
				}
				if n, err := strconv.ParseInt(floor, 10, 64); err != nil || n < wire {
					t.Errorf("min_bytes = %q, want a floor of at least the file's wire size %d", floor, wire)
				}
				for _, key := range []string{"min_estimated_tokens", "min_slices", "min_files", "missing"} {
					if v, ok := got.Details[key]; !ok || v == "" {
						t.Errorf("minimum-budget error is missing the frozen detail %q", key)
					}
				}
				if !strings.Contains(got.Details["missing"], oversized) {
					t.Errorf("missing = %q, want it to name %q", got.Details["missing"], oversized)
				}
			},
		},
		{
			// Guards against a silent omission: when a complete scope needs
			// more than one slice, every entry must still be reachable through
			// exactly one slice, and anything dropped must leave a reason.
			name: "a scope packed into two slices accounts for every ordinal and omits nothing silently",
			run: func(t *testing.T, fx *contextFixture) {
				paths := []string{"internal/order/ports.go", "internal/order/service.go",
					"internal/order/handler.go", "internal/order/service_test.go"}
				cands, files := budgetRowInput(fx, paths, model.RequirementFull)
				// Two optional candidates the file budget has no room for.
				// They are what makes the accounting assertion below real: a
				// plan that quietly forgot them would still satisfy a count of
				// its own entries.
				spare, spareFiles := budgetRowInput(fx,
					[]string{"docs/order.md", "config/order.toml"}, model.RequirementOptional)
				cands = append(cands, spare...)
				files = append(files, spareFiles...)
				// A byte bound that holds some but not all of the required
				// files forces a split at a file boundary; the file bound
				// admits the four required files and nothing more.
				b, err := resolveBudget(model.Budget{MaxBytes: 800, MaxFiles: len(paths)}, fx.Cfg.Context)
				if err != nil {
					t.Fatalf("resolveBudget: %v", err)
				}
				p, err := buildPlan(cands, files, b)
				if err != nil {
					t.Fatalf("buildPlan: %v", err)
				}
				if len(p.Slices) != 2 {
					t.Fatalf("plan holds %d slices, want 2 (entries %d, budget %+v)", len(p.Slices), len(p.Entries), b)
				}
				if len(p.Entries) != len(paths) {
					t.Fatalf("plan selected %d entries, want the %d required files", len(p.Entries), len(paths))
				}
				// The candidates the budget had no room for are persisted as
				// exclusions, not dropped: every candidate is accounted for in
				// exactly one of the two lists.
				if len(p.Excluded) != len(spare) {
					t.Fatalf("plan persisted %d exclusions, want %d; an unselected candidate vanished silently",
						len(p.Excluded), len(spare))
				}
				if len(p.Entries)+len(p.Excluded) != len(cands) {
					t.Fatalf("plan accounts for %d entries + %d exclusions, want all %d candidates",
						len(p.Entries), len(p.Excluded), len(cands))
				}
				seen := map[int]int{}
				for _, s := range p.Slices {
					if err := s.Validate(); err != nil {
						t.Fatalf("slice %d is invalid: %v", s.Index, err)
					}
					if s.EstimatedBytes > b.MaxBytes {
						t.Errorf("slice %d holds %d bytes, over the per-slice budget %d", s.Index, s.EstimatedBytes, b.MaxBytes)
					}
					for _, o := range s.EntryOrdinals {
						seen[o]++
					}
				}
				for i, e := range p.Entries {
					if e.Ordinal != i {
						t.Fatalf("entry %d carries ordinal %d; ordinals must be 0..n-1 in stored order", i, e.Ordinal)
					}
					if seen[e.Ordinal] != 1 {
						t.Errorf("ordinal %d appears in %d slices, want exactly 1", e.Ordinal, seen[e.Ordinal])
					}
				}
				for _, x := range p.Excluded {
					if strings.TrimSpace(x.Reason) == "" {
						t.Errorf("exclusion %d carries no reason, which is a silent omission", x.Ordinal)
					}
				}
			},
		},
		// L5 MANIFEST rows
		{
			// Task 16's `context next` walks manifest ordinals without
			// re-sorting, so a plan persisted in any order other than the
			// Section 15.3 reading order hands the actor required files after
			// optional ones with nothing in Task 15 failing. The row feeds the
			// manifest pass candidates in deliberately wrong order and asserts
			// what comes back out of the four Store.Manifest* read methods.
			name: "manifest ordinals are the actor's reading order and required_full is a prefix",
			run: func(t *testing.T, fx *contextFixture) {
				// The row drives the budget and manifest passes directly:
				// Compile's orchestration is the integration lane's, and the
				// ordering contract lives in these two. Ordinals are assigned
				// exactly ONCE, by the budget pass's total sort, and the
				// manifest pass persists them; a second ordering here would
				// agree with the first only by accident.
				c := &Compiler{store: fx.Store, repo: fx.Repo, cfg: fx.Cfg, now: fx.Now}
				req := model.ContextRequest{Task: "make Place idempotent", Phase: model.PhaseVerify}
				cand := func(path string, want model.Requirement, score int64) candidate {
					fv := fx.File(path)
					return candidate{NodeID: fx.Node(path), FileID: fv.ID, Path: path, Requirement: want,
						ScoreMicros: score, SizeBytes: fv.Size, Status: fv.Status, Reasons: []string{"fixture " + path}}
				}
				budget := model.Budget{MaxEstimatedTokens: 100_000, MaxBytes: 1 << 20, MaxFiles: 8, MaxSlices: 4}
				// Deliberately NOT in reading order, and docs/order.md
				// deliberately outscores both required_full entries:
				// requirement rank is the FIRST key of the chain, so a plan
				// ordered by score alone would put it first and hand the actor
				// an optional file before the code it must read.
				dropped := cand("config/order.toml", model.RequirementOptional, 10_000)
				dropped.Excluded = "below the optional cut for this budget"
				cands := []candidate{
					cand("docs/order.md", model.RequirementOptional, 990_000),
					cand("internal/order/service.go", model.RequirementFull, 900_000),
					cand("internal/order/handler.go", model.RequirementRecommended, 700_000),
					cand("internal/order/ports.go", model.RequirementFull, 940_000),
					dropped,
				}
				files := make([]model.FileVersion, 0, len(cands))
				for _, cd := range cands {
					files = append(files, fx.File(cd.Path))
				}
				b, err := resolveBudget(budget, fx.Cfg.Context)
				if err != nil {
					t.Fatalf("resolveBudget: %v", err)
				}
				p, err := buildPlan(cands, files, b)
				if err != nil {
					t.Fatalf("buildPlan: %v", err)
				}
				m, err := c.persistPlan(fx.ctx, fx.Binding, req, budget, p, fixtureCapabilities, true)
				if err != nil {
					t.Fatalf("persistManifest: %v", err)
				}

				entries, err := fx.Store.ManifestEntries(fx.ctx, m.ID, -1, 0)
				if err != nil {
					t.Fatalf("ManifestEntries: %v", err)
				}
				if len(entries) != 4 || m.EntryCount != 4 {
					t.Fatalf("read back %d entries (header says %d), want the 4 packed candidates", len(entries), m.EntryCount)
				}
				var lastRank = -1
				for i, e := range entries {
					if e.Ordinal != i {
						t.Fatalf("entry %d has ordinal %d; ordinals are the reading order and must be dense 0..n-1", i, e.Ordinal)
					}
					rank := map[model.Requirement]int{model.RequirementFull: 0, model.RequirementSymbol: 1,
						model.RequirementRecommended: 2, model.RequirementOptional: 3}[e.Requirement]
					if rank < lastRank {
						t.Fatalf("entry %d is %q after a stronger requirement; required_full must be a prefix", i, e.Requirement)
					}
					lastRank = rank
				}
				if entries[0].Requirement != model.RequirementFull || entries[1].Requirement != model.RequirementFull {
					t.Fatalf("the two required_full entries are not the prefix: got %q, %q", entries[0].Requirement, entries[1].Requirement)
				}
				// Within required_full the chain is score descending, so
				// ports.go (940_000) precedes service.go (900_000).
				if entries[0].FileID != fx.File("internal/order/ports.go").ID {
					t.Fatalf("entry 0 is %s, want the higher-scoring required_full file", entries[0].FileID)
				}

				slices, err := fx.Store.ManifestSlices(fx.ctx, m.ID, -1, 0)
				if err != nil {
					t.Fatalf("ManifestSlices: %v", err)
				}
				if len(slices) != m.SliceCount || len(slices) == 0 {
					t.Fatalf("read back %d slices, header says %d, want a nonzero agreeing count", len(slices), m.SliceCount)
				}
				// Every selected ordinal lands in exactly one slice, and each
				// slice lists its ordinals ascending: `context next` walks the
				// ordinals, so an ordinal in two slices would serve the same
				// bytes twice and one in none would be a silent omission.
				placed := map[int]int{}
				for _, sl := range slices {
					for i, o := range sl.EntryOrdinals {
						if o < 0 || o >= len(entries) {
							t.Fatalf("slice %d references ordinal %d, which is not an entry", sl.Index, o)
						}
						if i > 0 && o <= sl.EntryOrdinals[i-1] {
							t.Fatalf("slice %d lists ordinals %v out of reading order", sl.Index, sl.EntryOrdinals)
						}
						placed[o]++
					}
				}
				for o := range entries {
					if placed[o] != 1 {
						t.Fatalf("ordinal %d appears in %d slices, want exactly one", o, placed[o])
					}
				}

				excluded, err := fx.Store.ManifestExcluded(fx.ctx, m.ID, -1, 0)
				if err != nil {
					t.Fatalf("ManifestExcluded: %v", err)
				}
				if len(excluded) != 1 || strings.TrimSpace(excluded[0].Reason) == "" {
					t.Fatalf("exclusions = %+v, want exactly one carrying a nonempty reason", excluded)
				}

				// Recompiling the same request is a no-op, not a second plan.
				if again, err := c.persistPlan(fx.ctx, fx.Binding, req, budget, p,
					fixtureCapabilities, true); err != nil || again.CanonicalHash != m.CanonicalHash {
					t.Fatalf("re-persisting the identical plan = %v (hash %s), want the immutable manifest unchanged", err, again.CanonicalHash)
				}
				// A different plan under the same identity is the determinism
				// alarm, never a silent overwrite of what the actor is reading.
				_, err = c.persistPlan(fx.ctx, fx.Binding, req, budget, p, fixtureCapabilities, false)
				var typed *model.Error
				if !errors.As(err, &typed) || typed.Code != model.CodeVersionConflict {
					t.Fatalf("persisting a different canonical plan under the same id = %v, want %s", err, model.CodeVersionConflict)
				}
			},
		},
		{
			// The budget pass sizes a whole selected set at once. If FilesByID
			// leaked rows from outside the pinned snapshot, or errored on an id
			// the snapshot does not publish, budgeting would either charge for
			// invisible files or fail on a legitimate traversal frontier.
			name: "FilesByID hydrates a batch from the pinned snapshot and omits ids it does not publish",
			run: func(t *testing.T, fx *contextFixture) {
				reader, err := fx.Store.PinGeneration(fx.ctx, fx.Repo, fx.Gen, time.Minute)
				if err != nil {
					t.Fatalf("PinGeneration: %v", err)
				}
				defer reader.Close()
				absent := model.NewFileID(fx.Repo, "internal/order/never_captured.go")
				want := []string{"internal/order/ports.go", "internal/order/service.go", "docs/order.md"}
				ids := []model.FileID{absent}
				for _, p := range want {
					ids = append(ids, fx.File(p).ID)
				}
				got, err := reader.FilesByID(fx.ctx, ids)
				if err != nil {
					t.Fatalf("FilesByID: %v", err)
				}
				if len(got) != len(want) {
					t.Fatalf("FilesByID returned %d rows for %d ids, want the %d published ones with the absent id omitted",
						len(got), len(ids), len(want))
				}
				byID := map[model.FileID]model.FileVersion{}
				for _, fv := range got {
					byID[fv.ID] = fv
				}
				for _, p := range want {
					fv, ok := byID[fx.File(p).ID]
					if !ok {
						t.Fatalf("FilesByID omitted %s, which the pinned snapshot publishes", p)
					}
					if fv.Path != p || fv.Size != fx.File(p).Size || fv.Status != fx.File(p).Status {
						t.Fatalf("FilesByID(%s) = %+v, want the snapshot's path, size and status %+v", p, fv, fx.File(p))
					}
				}
			},
		},
		// INT rows
		{
			// A manifest identity that depended on the process, the clock or
			// the database file would make "repeated compile requests reuse the
			// immutable manifest" (Section 15.1) a lookup that never hits, and
			// two actors reading the same generation for the same task would
			// get two different plans with nothing reporting a difference. The
			// row compiles end to end, reopens the same database and recompiles
			// -- which must reuse rather than recompile -- and then compiles the
			// SAME facts in a second database, where the reuse lookup cannot
			// hit, so identity has to be re-derived from the pinned facts and
			// the request alone.
			name: "a manifest identity is a function of the pinned facts and the request, not of the process, the clock or the database file",
			run: func(t *testing.T, fx *contextFixture) {
				req := model.ContextRequest{Task: "make `Place` idempotent", Phase: model.PhaseVerify}
				// One advancing clock for every compile in the row: CreatedAt
				// is the only field a recompile may change, so it must be able
				// to change.
				tick := 0
				now := func() time.Time {
					tick++
					return fixtureNow().Add(time.Duration(tick) * time.Second)
				}
				first, err := intCompiler(t, fx, now).Compile(fx.ctx, req)
				if err != nil {
					t.Fatalf("Compile: %v", err)
				}
				if first.ID == "" || first.CanonicalHash == "" || first.EntryCount == 0 {
					t.Fatalf("the first compile produced %+v, want a persisted manifest with entries", first)
				}
				if first.Binding.GenerationID != fx.Gen || first.PolicyVersion != compilerPolicyVersion {
					t.Fatalf("manifest header = %+v, want the pinned generation and the frozen policy version", first)
				}

				// Close the store and reopen the SAME file: the stored manifest
				// is immutable, so the second compile must return it unchanged
				// rather than compile a second plan over it.
				if err := fx.Store.Close(); err != nil {
					t.Fatalf("Close: %v", err)
				}
				reopened, err := store.Open(fx.ctx, fx.DBPath, store.Options{})
				if err != nil {
					t.Fatalf("reopen: %v", err)
				}
				t.Cleanup(func() { reopened.Close() })
				fx.Store = reopened
				second, err := intCompiler(t, fx, now).Compile(fx.ctx, req)
				if err != nil {
					t.Fatalf("recompile after reopen: %v", err)
				}
				if second.ID != first.ID || second.CanonicalHash != first.CanonicalHash ||
					!second.CreatedAt.Equal(first.CreatedAt) {
					t.Fatalf("recompiling after a reopen returned %s/%s at %s, want the immutable manifest %s/%s at %s",
						second.ID, second.CanonicalHash, second.CreatedAt,
						first.ID, first.CanonicalHash, first.CreatedAt)
				}

				// The same facts in a second database, where nothing is stored
				// to reuse: the plan is recompiled from scratch and must land
				// on the same identity and the same canonical projection.
				other := newContextFixture(t)
				third, err := intCompiler(t, other, now).Compile(other.ctx, req)
				if err != nil {
					t.Fatalf("compile against a second store: %v", err)
				}
				if third.ID != first.ID {
					t.Fatalf("ManifestID = %s in a second store, want %s: identity must not depend on the database file", third.ID, first.ID)
				}
				if third.CanonicalHash != first.CanonicalHash {
					t.Fatalf("CanonicalHash = %s in a second store, want %s: the canonical projection must exclude CreatedAt and the local generation",
						third.CanonicalHash, first.CanonicalHash)
				}
				if third.EntryCount != first.EntryCount || third.SliceCount != first.SliceCount {
					t.Fatalf("second store compiled %d entries / %d slices, want %d / %d",
						third.EntryCount, third.SliceCount, first.EntryCount, first.SliceCount)
				}
				// CreatedAt is the one field a recompile is allowed to change,
				// and it is excluded from the canonical hash precisely so that
				// asserting both at once is possible.
				if !third.CreatedAt.After(first.CreatedAt) {
					t.Fatalf("second compile CreatedAt = %s, want a later instant than %s", third.CreatedAt, first.CreatedAt)
				}
			},
		},
		{
			// Guards row 41 / F15: the manifest notices are INSTALLED on the
			// compile's result, not merely computable. Both the page-clamp
			// disclosure and the pointer at the excluded-candidate projection
			// reach the caller only through Compile's own two assignments, and
			// a compile whose bounds cut silently cannot be told from one that
			// cut nothing.
			name: "a compile installs its page-clamp and excluded-candidate disclosures on the manifest it returns",
			setup: func(t *testing.T, fx *contextFixture) {
				// Above the wire ceiling is the only configuration that clamps:
				// a page size at or under it is served as asked.
				fx.Cfg.Resources.MaxPageItems = model.MaxPageItems + 500
			},
			run: func(t *testing.T, fx *contextFixture) {
				// The exclusion this row points at is an identity the task
				// named that the pinned snapshot answers with nothing -- a
				// reason a candidate is absent that exists on every fixture and
				// on every real repository. It is deliberately NOT a page-end
				// disclosure: the seed steps page to exhaustion, so a page
				// boundary no longer manufactures an exclusion, and a row that
				// relied on one was asserting an artifact of the bound rather
				// than the notice install.
				req := model.ContextRequest{Task: "make `Place` and `NoSuchSymbol` idempotent",
					Phase: model.PhaseVerify, Budget: model.Budget{MaxFiles: 1}}
				m, err := intCompiler(t, fx, fx.Now).Compile(fx.ctx, req)
				if err != nil {
					t.Fatalf("Compile: %v", err)
				}
				joined := strings.Join(m.Notices, "\n")
				for _, want := range []string{
					fmt.Sprintf("requested %d, effective %d", model.MaxPageItems+500, model.MaxPageItems),
					excludedViewPointer,
				} {
					if !strings.Contains(joined, want) {
						t.Fatalf("the compiled manifest disclosed %q, which does not carry %q; the notices never reached the caller",
							m.Notices, want)
					}
				}
			},
		},
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

// budgetRowInput builds the candidate set and the hydrated snapshot metadata a
// `// L4 BUDGET rows` row compiles, standing in for the seed, scope and ranking
// passes the fill-in lanes own. Scores descend with the given order so the
// Section 15.3 tie-break order is the order the paths are named in, which is
// what makes an ordinal assertion readable.
func budgetRowInput(fx *contextFixture, paths []string, req model.Requirement) ([]candidate, []model.FileVersion) {
	fx.t.Helper()
	cands := make([]candidate, 0, len(paths))
	files := make([]model.FileVersion, 0, len(paths))
	for i, path := range paths {
		fv := fx.File(path)
		files = append(files, fv)
		cands = append(cands, candidate{
			NodeID:      fx.Node(path),
			FileID:      fv.ID,
			Path:        path,
			Requirement: req,
			Origin:      originExplicitSeed,
			ScoreMicros: seedContribution - int64(i),
			Reasons:     []string{"named by the task"},
		})
	}
	return cands, files
}

// pathOfEntry maps a persisted entry back to the fixture path it covers, so a
// size assertion can read the file's own metadata without the row tracking
// ordinals by hand.
func pathOfEntry(t *testing.T, fx *contextFixture, e model.ContextEntry) string {
	t.Helper()
	for path, fv := range fx.Files {
		if fv.ID == e.FileID {
			return path
		}
	}
	t.Fatalf("entry %d names file %q, which the fixture does not publish", e.Ordinal, e.FileID)
	return ""
}

// --- Task 15 lane L2 (SCOPE) test support -------------------------------
//
// The fixture publishes no relations (the digest fakes the GraphFactory), and
// the only sqlite-to-graph.Adjacency adapter is unexported in internal/app, so
// scope rows drive a real graph.Engine over this in-memory adjacency. It is
// deliberately literal: it answers exactly the edges a row declares, so a row
// asserting a requirement is asserting scope's rule and not the store's.
type scopeAdjacency struct {
	binding   model.Binding
	nodes     map[model.NodeID]model.Node
	relations []model.Relation
	caps      []model.CapabilityState
}

func (a *scopeAdjacency) Binding() model.Binding { return a.binding }

func (a *scopeAdjacency) Capabilities(stdcontext.Context) ([]model.CapabilityState, error) {
	return a.caps, nil
}

// Edges is keyset-ordered by relation id after `after`, exactly as the port
// documents; an empty kinds slice means every kind.
func (a *scopeAdjacency) Edges(_ stdcontext.Context, nodes []model.NodeID, dir model.Direction,
	kinds []model.RelationKind, after model.RelationID, limit int) ([]model.Relation, error) {
	want := make(map[model.NodeID]bool, len(nodes))
	for _, n := range nodes {
		want[n] = true
	}
	allowed := make(map[model.RelationKind]bool, len(kinds))
	for _, k := range kinds {
		allowed[k] = true
	}
	out := make([]model.Relation, 0, limit)
	for _, r := range a.relations {
		if r.ID <= after || (len(kinds) > 0 && !allowed[r.Kind]) {
			continue
		}
		touches := (dir == model.DirectionOutgoing && want[r.From]) ||
			(dir == model.DirectionIncoming && want[r.To]) ||
			(dir == model.DirectionBoth && (want[r.From] || want[r.To]))
		if !touches {
			continue
		}
		if out = append(out, r); len(out) == limit {
			break
		}
	}
	return out, nil
}

func (a *scopeAdjacency) NodesByID(_ stdcontext.Context, ids []model.NodeID) ([]model.Node, error) {
	out := make([]model.Node, 0, len(ids))
	for _, id := range ids {
		if n, ok := a.nodes[id]; ok {
			out = append(out, n)
		}
	}
	return out, nil
}

// EvidenceFor returns no rows: scope asserts requirements and completeness, and
// per-edge evidence is lane L3's ranking input, not scope's.
func (a *scopeAdjacency) EvidenceFor(stdcontext.Context, []model.RelationID, int) (map[model.RelationID][]model.EvidenceID, error) {
	return map[model.RelationID][]model.EvidenceID{}, nil
}

// scopeEngine builds an engine over the fixture's nodes and the edges a row
// declares. The limits mirror internal/app's graphLimits, so a row exercises
// the same bounds production resolves.
func (f *contextFixture) scopeEngine(rels []model.Relation, caps []model.CapabilityState) *graph.Engine {
	f.t.Helper()
	adj := &scopeAdjacency{binding: f.Binding, nodes: map[model.NodeID]model.Node{}, relations: rels, caps: caps}
	for _, spec := range fixtureFiles {
		fv := f.File(spec.path)
		id := f.Node(spec.path)
		adj.nodes[id] = model.Node{ID: id, Kind: spec.kind, Language: "go", Name: spec.symbol,
			QualifiedName: spec.path + "." + spec.symbol, FileID: fv.ID, ContentHash: fv.ContentHash}
	}
	sort.Slice(adj.relations, func(i, j int) bool { return adj.relations[i].ID < adj.relations[j].ID })
	eng, err := graph.New(graph.Options{Adjacency: adj, Limits: graph.Limits{
		MaxDepth:       f.Cfg.Context.MaxGraphDepth.Int(),
		MaxVisited:     f.Cfg.Context.MaxVisitedNodes.Int(),
		MaxEdges:       f.Cfg.Context.MaxGraphEdges.Int(),
		MaxPageItems:   f.Cfg.Resources.MaxPageItems,
		MaxReasonPaths: f.Cfg.Context.MaxReasonPathsPerEntry.Int(),
		QueryTimeout:   f.Cfg.Resources.QueryTimeout.Std(),
		CursorTTL:      f.Cfg.Storage.QueryCursorTTL.Std(),
		FrontierBytes:  f.Cfg.Resources.QueryMemoryBytes,
		// Deliberately the real clock, not the fixture's frozen one: the engine
		// derives a context deadline from it, and a frozen instant in the past
		// would expire every walk before its first edge.
	}})
	if err != nil {
		f.t.Fatalf("graph.New: %v", err)
	}
	return eng
}

// edge declares one fixture relation by the paths it connects.
func (f *contextFixture) edge(from string, kind model.RelationKind, to string) model.Relation {
	f.t.Helper()
	a, b := f.Node(from), f.Node(to)
	return model.Relation{ID: model.NewRelationID(f.Repo, a, kind, b), From: a, Kind: kind, To: b}
}

// seedOf is the resolved seed a scope row starts from.
func (f *contextFixture) seedOf(path string) candidate {
	f.t.Helper()
	fv := f.File(path)
	return candidate{NodeID: f.Node(path), FileID: fv.ID, Path: path,
		Requirement: model.RequirementFull, Origin: originExplicitSeed, Status: fv.Status}
}

// requirementFor finds the candidate for the artifact at path. Expansion
// entries carry no path of their own -- scope.go leaves candidate.Path for the
// compiler's file hydration to fill -- so the match is by FileID, and falls back
// to the path only for a discovery candidate that resolved to no file at all.
func requirementFor(t *testing.T, fx *contextFixture, res scopeResult, path string) (candidate, bool) {
	t.Helper()
	id := fx.Files[path].ID
	for _, c := range res.Candidates {
		if (id != "" && c.FileID == id) || (c.Path != "" && c.Path == path) {
			return c, true
		}
	}
	return candidate{}, false
}

// searchService composes the real search.Service over fx's snapshot: its CAS
// content, signer and spools. Seed resolution is defined in terms of
// Search.Resolve and Search.Search, so a row that faked them would be proving
// the fake; every row that needs discovery shares this composition.
func searchService(t *testing.T, fx *contextFixture) *search.Service {
	t.Helper()
	dir := t.TempDir()
	cas, err := snapshot.OpenCAS(filepath.Join(dir, "cas"))
	if err != nil {
		t.Fatalf("OpenCAS: %v", err)
	}
	for _, spec := range fixtureFiles {
		if _, err := cas.Put(fx.ctx, strings.NewReader(spec.content)); err != nil {
			t.Fatalf("CAS.Put(%s): %v", spec.path, err)
		}
	}
	signer, err := pagination.OpenSigner(dir)
	if err != nil {
		t.Fatalf("OpenSigner: %v", err)
	}
	spools, err := pagination.NewSpools(filepath.Join(dir, "spools"), 1<<20, fx.Store)
	if err != nil {
		t.Fatalf("NewSpools: %v", err)
	}
	svc, err := search.New(search.Options{Store: fx.Store, Repo: fx.Repo, Signer: signer,
		Spools: spools, Leases: pagination.NewLeases(fx.Store, pagination.DefaultCursorTTL),
		Content: cas, Resources: fx.Cfg.Resources,
		CursorTTL: pagination.DefaultCursorTTL, Now: fx.Now})
	if err != nil {
		t.Fatalf("search.New: %v", err)
	}
	t.Cleanup(func() { svc.Close() })
	return svc
}

// intCompiler composes a Compiler over fx the way internal/app does: the real
// search service for seed resolution and a GraphFactory over the fixture's
// engine, which the shared builder composes neither of.
//
// Two clocks, deliberately. now is the caller's, and the row passes ONE
// advancing clock to every compiler it builds, because a frozen instant would
// make two compiles agree on CreatedAt by construction and the row asserts they
// differ. The engine keeps the real clock (scopeEngine), because it derives a
// context deadline and the fixture's frozen instant lies in the past, which
// would expire every walk before its first edge.
func intCompiler(t *testing.T, fx *contextFixture, now func() time.Time) *Compiler {
	t.Helper()
	svc := searchService(t, fx)
	c, err := New(Options{
		Store:  fx.Store,
		Repo:   fx.Repo,
		Search: svc,
		Graph: func(ctx stdcontext.Context, gen model.GenerationID) (*graph.Engine, func() error, error) {
			if gen != fx.Gen {
				t.Fatalf("the graph factory was asked for generation %d, want the pinned %d", gen, fx.Gen)
			}
			return fx.scopeEngine(nil, fixtureCapabilities), func() error { return nil }, nil
		},
		Config: fx.Cfg,
		Now:    now,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		// Every compile of this fixture writes its sort runs here, as the
		// workspace writes them under its spool area.
		SortDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// longRoute builds a route one hop past model.MaxRelationsPerPath out of the
// ids it is given, so a row can exercise the "admissible but too long to store"
// branch of scoreRoutes without adding a distinct edge to package centrality.
func longRoute(ids ...model.RelationID) []model.RelationID {
	out := make([]model.RelationID, 0, model.MaxRelationsPerPath+1)
	for i := 0; i <= model.MaxRelationsPerPath; i++ {
		out = append(out, ids[i%len(ids)])
	}
	return out
}
