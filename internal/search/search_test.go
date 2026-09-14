package search

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"testing"
	"time"

	"github.com/Sawmonabo/codectx/internal/config"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
	"github.com/Sawmonabo/codectx/internal/storage/sqlite"
	_ "modernc.org/sqlite"
)

// The Section 14 ranking fixture. Three small lexical documents in one
// activated generation are the smallest corpus that can separate the exact
// tiers from lexical BM25, exercise an indexed prefix range, carry a Unicode
// identifier and a literal-punctuation body through the FTS encoder, and fold
// two occurrences of one node into one hit. Every Task 13 lane asserts against
// this one corpus so a ranking change cannot be hidden behind a private
// fixture.
const (
	fixtureProviderID      = "treesitter"
	fixtureProviderVersion = "1.0.0"
	fixtureConfigHash      = "cfg"
)

// doc describes one fixture document: its file, its node identity and the
// indexed text BM25 scores. Occurrences > 1 publishes that many search
// documents for the same node, which is what the deduplication leg folds.
type doc struct {
	path          string
	name          string
	qualifiedName string
	signature     string
	body          string
	occurrences   int
}

// fixtureDocs is the corpus. "Handle" and "HandleRequest" share the "pkg.Beta"
// qualified-name prefix; the third document carries a Unicode identifier and a
// literal-punctuation call in its body.
var fixtureDocs = []doc{
	{path: "pkg/alpha.go", name: "Handle", qualifiedName: "pkg.Alpha.Handle",
		signature: "func Handle(ctx context.Context) error",
		body:      "func Handle(ctx context.Context) error { return dispatch(ctx) }", occurrences: 2},
	{path: "pkg/beta.go", name: "HandleRequest", qualifiedName: "pkg.Beta.HandleRequest",
		signature: "func HandleRequest(r *Request) error",
		body:      "func HandleRequest(r *Request) error { return handle(r) }", occurrences: 1},
	{path: "pkg/café.go", name: "café", qualifiedName: "pkg.Uni.café",
		signature: "func café() string",
		body:      "func café() string { return foo(bar) }", occurrences: 1},
}

// fixture is one activated generation over fixtureDocs, plus the pagination
// machinery a Service needs.
type fixture struct {
	t       *testing.T
	ctx     context.Context
	store   *sqlite.Store
	repo    model.RepositoryID
	gen     model.GenerationID
	binding model.Binding
	opts    Options
	nodes   map[string]model.NodeID
	files   map[string]model.FileID
}

// newFixture builds and activates the corpus. It runs at every lane's entry so
// a corpus that no longer publishes is a failure here, not five lanes later.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	st, err := sqlite.Open(ctx, filepath.Join(dir, "codectx.db"), sqlite.Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	f := &fixture{t: t, ctx: ctx, store: st, repo: model.RepositoryID(model.H("search-fixture", "1")),
		nodes: map[string]model.NodeID{}, files: map[string]model.FileID{}}
	if err := st.EnsureRepository(ctx, f.repo, "/repo"); err != nil {
		t.Fatalf("EnsureRepository: %v", err)
	}

	hashes := make(map[string]string, len(fixtureDocs))
	files := make([]model.FileVersion, 0, len(fixtureDocs))
	for _, d := range fixtureDocs {
		sum := sha256.Sum256([]byte(d.body))
		hash := hex.EncodeToString(sum[:])
		hashes[d.path] = hash
		id := model.NewFileID(f.repo, d.path)
		f.files[d.path] = id
		f.nodes[d.path] = model.NewNodeID(f.repo, model.NodeFunction, model.CanonicalNodeKey(d.path, d.name))
		rec := model.BlobRecord{Hash: hash, Size: int64(len(d.body)),
			BlockDigests:    []string{hash},
			LineCheckpoints: []model.LineCheckpoint{{ByteOffset: 0, LineNumber: 1, LineStartByte: 0}}}
		if err := st.PutBlob(ctx, rec); err != nil {
			t.Fatalf("PutBlob(%s): %v", d.path, err)
		}
		files = append(files, model.FileVersion{ID: id, Path: d.path, Status: model.FileTracked,
			Size: int64(len(d.body)), ContentHash: hash, Language: "go"})
	}

	manifest := model.H("search-fixture-manifest")
	snap := model.Snapshot{
		ID:                 model.NewSnapshotID(f.repo, "", model.H("policy"), manifest),
		RepositoryID:       f.repo,
		CaptureConsistency: model.CaptureValidated,
		SourcePolicyHash:   model.H("policy"),
		FileCount:          uint64(len(files)),
		ManifestHash:       manifest,
		CreatedAt:          time.Now().UTC(),
	}
	for _, fv := range files {
		snap.SourceBytes += uint64(fv.Size)
	}
	err = st.PutSnapshot(ctx, snap, func(yield func(model.FileVersion) error) error {
		for _, fv := range files {
			if err := yield(fv); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("PutSnapshot: %v", err)
	}

	f.gen, err = st.BeginGeneration(ctx, f.repo, snap.ID, model.H("semantic"), "main")
	if err != nil {
		t.Fatalf("BeginGeneration: %v", err)
	}
	run, err := st.BeginProviderRun(ctx, f.gen, fixtureProviderID, fixtureProviderVersion)
	if err != nil {
		t.Fatalf("BeginProviderRun: %v", err)
	}
	for _, d := range fixtureDocs {
		f.publish(run, d, hashes[d.path])
	}
	f.binding, err = st.Activate(ctx, f.gen, 0, model.HealthFresh,
		[]model.CapabilityState{{ProviderID: fixtureProviderID, Capability: "structure", Scope: "workspace", State: model.CapabilityFresh}},
		"norm-v1")
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}

	signer, err := pagination.OpenSigner(dir)
	if err != nil {
		t.Fatalf("OpenSigner: %v", err)
	}
	spools, err := pagination.NewSpools(filepath.Join(dir, "spools"), 1<<20, st)
	if err != nil {
		t.Fatalf("NewSpools: %v", err)
	}
	f.opts = Options{Store: st, Repo: f.repo, Signer: signer, Spools: spools,
		Resources: config.Defaults().Resources, CursorTTL: pagination.DefaultCursorTTL,
		Now: func() time.Time { return time.Now().UTC() }}
	return f
}

// publish seals one file-scoped unit carrying d's node fact and its lexical
// documents into the staging generation.
func (f *fixture) publish(run model.ProviderRunID, d doc, hash string) {
	f.t.Helper()
	fileID := f.files[d.path]
	nodeID := f.nodes[d.path]
	input := model.UnitInput{FileID: fileID, ContentHash: hash}
	h := model.NewUnitInputHasher()
	if err := h.Add(input); err != nil {
		f.t.Fatalf("UnitInputHasher.Add(%s): %v", d.path, err)
	}
	spec := model.UnitSpec{ProviderID: fixtureProviderID, ProviderVersion: fixtureProviderVersion,
		ScopeKey: d.path, InputHash: h.Sum(), DependencyHash: model.DependencyHash(nil)}
	spec.ID = model.NewUnitID(spec, fixtureConfigHash)
	build := model.UnitBuild{Spec: spec, AnalysisConfigHash: fixtureConfigHash, OriginRunID: run,
		SourceBinding: model.SourceBindingVerified}
	w, err := f.store.BeginUnit(f.ctx, f.gen, build,
		func(yield func(model.UnitInput) error) error { return yield(input) })
	if err != nil {
		f.t.Fatalf("BeginUnit(%s): %v", d.path, err)
	}

	end := uint64(len(d.body))
	rng := &model.SourceRange{Start: model.Position{Byte: 0, Line: 1},
		End: model.Position{Byte: end, Line: 1, Column: uint32(end)}}
	ev := model.Evidence{UnitID: w.UnitID(), ProviderID: fixtureProviderID, ProviderVersion: fixtureProviderVersion,
		OriginRunID: run, NodeID: nodeID, Precision: model.PrecisionSyntax, FileID: fileID,
		ContentHash: hash, Range: rng}
	ev.ID = model.NewEvidenceID(ev)
	node := model.Node{ID: nodeID, Kind: model.NodeFunction, Language: "go", Name: d.name,
		QualifiedName: d.qualifiedName, Signature: d.signature, FileID: fileID, ContentHash: hash, Range: rng}
	if err := w.PutNodes(f.ctx, []model.NodeFact{{Node: node,
		CanonicalKey: model.CanonicalNodeKey(d.path, d.name), Evidence: []model.Evidence{ev}}}); err != nil {
		f.t.Fatalf("PutNodes(%s): %v", d.path, err)
	}

	units := make([]model.SearchUnit, 0, d.occurrences)
	for i := range d.occurrences {
		units = append(units, model.SearchUnit{
			ID: model.H("search-fixture-doc", d.path, hash, string(rune('a'+i))), NodeID: nodeID,
			FileID: fileID, Path: d.path, Kind: model.NodeFunction, Name: d.name,
			QualifiedName: d.qualifiedName, Signature: d.signature,
			Bytes: model.ByteRange{Start: 0, End: end}, Body: d.body, TokenCount: 1,
		})
	}
	if err := w.PutSearchUnits(f.ctx, units); err != nil {
		f.t.Fatalf("PutSearchUnits(%s): %v", d.path, err)
	}
	if err := f.store.SealUnit(f.ctx, w); err != nil {
		f.t.Fatalf("SealUnit(%s): %v", d.path, err)
	}
}

// newService builds the service under test over the fixture. The fill-in lanes
// call it; at the skeleton commit New is not implemented yet.
func newService(t *testing.T, o Options) *Service {
	t.Helper()
	s, err := New(o)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// leg is one scenario leg. Each Task 13 lane appends its legs under its own
// marker below, so the inserts never touch the same lines.
type leg struct {
	name string
	run  func(t *testing.T, f *fixture)
}

// TestSearchRankingScenario is the single Task 13 scenario: one corpus, one
// activated generation, every leg a lane contributes. It guards determinism of
// the Section 14.2 ranking -- a silent reordering or score drift would serve
// different context for identical inputs.
func TestSearchRankingScenario(t *testing.T) {
	legs := []leg{

		// L1 STORAGE rows

		// L2 EXACT rows

		// L3 LEXICAL rows

		// L4 RANK rows

	}
	f := newFixture(t)
	for _, l := range legs {
		t.Run(l.name, func(t *testing.T) { l.run(t, f) })
	}
}
