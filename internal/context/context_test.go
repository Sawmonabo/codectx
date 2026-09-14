package context

import (
	stdcontext "context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path/filepath"
	"strconv"
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
				rels := map[model.RelationID]model.Relation{edge: {ID: edge,
					From: fx.Node("internal/order/handler.go"), Kind: model.RelCalls,
					To: fx.Node("internal/order/service.go")}}

				cands := []candidate{
					{FileID: impl.ID, Path: impl.Path, Requirement: model.RequirementFull,
						Origin: originExplicitSeed, Status: impl.Status},
					{NodeID: fx.Node("internal/order/handler.go"), FileID: caller.ID, Path: caller.Path,
						Requirement: model.RequirementRecommended, Origin: originExpansion, Depth: 1,
						Status: caller.Status, Paths: []model.RelationPath{{Relations: []model.RelationID{edge}}}},
					{NodeID: "n2", Path: "a.go", StartByte: 5, Requirement: model.RequirementOptional, Origin: originLexical},
					{NodeID: "n3", Path: "a.go", StartByte: 0, Requirement: model.RequirementOptional, Origin: originLexical},
					{NodeID: "n1", Path: "b.go", StartByte: 0, Requirement: model.RequirementOptional, Origin: originLexical},
					{NodeID: "n1", Path: "a.go", StartByte: 5, Requirement: model.RequirementOptional, Origin: originLexical},
				}
				ranked, err := (&Compiler{cfg: fx.Cfg}).rank(fx.ctx, reader, cands, rels)
				if err != nil {
					t.Fatalf("rank: %v", err)
				}

				// Seed 1_000_000 + captured change 120_000 + one walked edge of
				// package centrality 5_000; caller 405_000 + that same 5_000.
				// The optional candidates sit in the repository root, which this
				// compile walked no edge in, so they score nothing at all and the
				// tie-break chain alone decides their order.
				type want struct {
					path  string
					id    model.NodeID
					score int64
				}
				expect := []want{
					{impl.Path, "", 1_125_000},
					{caller.Path, fx.Node("internal/order/handler.go"), 410_000},
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
				// One admitted route, one route retained: nothing is hidden in
				// MorePaths that the entry could have explained.
				if ranked[1].MorePaths != 0 || len(ranked[1].Paths) != 1 {
					t.Fatalf("caller retained %d paths with MorePaths %d, want 1 and 0",
						len(ranked[1].Paths), ranked[1].MorePaths)
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
					if e.EstimatedBytes <= wire {
						t.Errorf("entry %d counted no metadata: estimated_bytes %d <= wire %d", e.Ordinal, e.EstimatedBytes, wire)
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
				// The row drives the manifest pass directly: Compile's
				// orchestration is the integration lane's, and the ordering
				// contract lives entirely in this pass.
				c := &Compiler{store: fx.Store, repo: fx.Repo, cfg: fx.Cfg, now: fx.Now}
				req := model.ContextRequest{Task: "make Place idempotent", Phase: model.PhaseVerify}
				cand := func(path string, want model.Requirement, score int64) candidate {
					fv := fx.File(path)
					return candidate{NodeID: fx.Node(path), FileID: fv.ID, Path: path, Requirement: want,
						ScoreMicros: score, SizeBytes: fv.Size, Status: fv.Status, Reasons: []string{"fixture " + path}}
				}
				budget := model.Budget{MaxEstimatedTokens: 100_000, MaxBytes: 1 << 20, MaxFiles: 8, MaxSlices: 4}
				// Packed in the budget pass's packing order, and within each
				// group deliberately not in reading order.
				// docs/order.md deliberately outscores both required_full
				// entries: requirement rank is the FIRST key of the chain, so
				// a plan ordered by score alone would put it first and hand the
				// actor an optional file before the code it must read.
				packed := [][]candidate{
					{cand("docs/order.md", model.RequirementOptional, 990_000),
						cand("internal/order/service.go", model.RequirementFull, 900_000)},
					{cand("internal/order/handler.go", model.RequirementRecommended, 700_000),
						cand("internal/order/ports.go", model.RequirementFull, 940_000)},
				}
				dropped := cand("config/order.toml", model.RequirementOptional, 10_000)
				dropped.Excluded = "below the optional cut for this budget"
				m, err := c.persistManifest(fx.ctx, fx.Binding, req, budget, packed,
					[]candidate{dropped}, fixtureCapabilities, true)
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
				if len(slices) != 2 || m.SliceCount != 2 {
					t.Fatalf("read back %d slices (header says %d), want 2", len(slices), m.SliceCount)
				}
				for _, sl := range slices {
					for i, o := range sl.EntryOrdinals {
						if o < 0 || o >= len(entries) {
							t.Fatalf("slice %d references ordinal %d, which is not an entry", sl.Index, o)
						}
						if i > 0 && o <= sl.EntryOrdinals[i-1] {
							t.Fatalf("slice %d lists ordinals %v out of reading order", sl.Index, sl.EntryOrdinals)
						}
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
				if again, err := c.persistManifest(fx.ctx, fx.Binding, req, budget, packed,
					[]candidate{dropped}, fixtureCapabilities, true); err != nil || again.CanonicalHash != m.CanonicalHash {
					t.Fatalf("re-persisting the identical plan = %v (hash %s), want the immutable manifest unchanged", err, again.CanonicalHash)
				}
				// A different plan under the same identity is the determinism
				// alarm, never a silent overwrite of what the actor is reading.
				_, err = c.persistManifest(fx.ctx, fx.Binding, req, budget, packed,
					[]candidate{dropped}, fixtureCapabilities, false)
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
