// Package providertest is the shared provider conformance harness (Section
// 30.2). It stands up the real store, CAS, snapshot view and workspace root
// over a small in-memory file set, so a provider test exercises the same
// BeginUnit, sink, resolver, seal and fail paths production uses. Tasks 7–11
// build their fixtures on it instead of each inventing a storage stub.
package providertest

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider"
	"github.com/Sawmonabo/codectx/internal/reconcile"
	"github.com/Sawmonabo/codectx/internal/snapshot"
	"github.com/Sawmonabo/codectx/internal/storage/sqlite"
	"github.com/Sawmonabo/codectx/internal/workspace"
)

// ConfigHash is the analysis configuration digest every harness unit is keyed
// under.
const ConfigHash = "providertest-config"

// Limits are the harness sink bounds: small enough that a test can reach a
// cap with a handful of records, large enough that an ordinary fixture never
// does by accident.
var Limits = provider.Limits{BatchRecords: 64, BatchBytes: 256 << 10, MaxRecordBytes: 64 << 10}

// Harness is one repository, one snapshot and one staging generation over a
// real store. Fields are exported so a test can reach the store and view
// directly when it must assert on persisted rows.
type Harness struct {
	Store    *sqlite.Store
	CAS      *snapshot.CAS
	Repo     model.RepositoryID
	Snapshot model.Snapshot
	View     model.SnapshotView
	Gen      model.GenerationID
	Root     workspace.Root
	Policy   workspace.Policy
	Pool     *provider.Pool

	ctx   context.Context
	files map[string]model.FileVersion
}

// New writes files (root-relative path to content) into a temporary
// workspace, retains them in a CAS, records the snapshot and opens a staging
// generation over it.
func New(t *testing.T, files map[string]string) *Harness {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	repoDir := filepath.Join(dir, "repo")
	dataDir := filepath.Join(dir, "data")
	store, err := sqlite.Open(ctx, filepath.Join(dataDir, "codectx.db"), sqlite.Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	cas, err := snapshot.OpenCAS(snapshot.CASDir(dataDir))
	if err != nil {
		t.Fatalf("OpenCAS: %v", err)
	}
	pool, err := provider.NewPool(4 * Limits.BatchBytes)
	if err != nil {
		t.Fatal(err)
	}
	h := &Harness{Store: store, CAS: cas, Pool: pool, ctx: ctx, files: map[string]model.FileVersion{}}
	// The repository identity is fixed rather than derived from the temporary
	// directory, so two harnesses over the same files mint the same file, unit
	// and node identities and a test can compare them.
	h.Repo = model.RepositoryID(model.H("providertest-repo"))
	if err := store.EnsureRepository(ctx, h.Repo, repoDir); err != nil {
		t.Fatalf("EnsureRepository: %v", err)
	}

	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	slices.Sort(paths)
	manifest := model.NewHasher("providertest-manifest")
	var total uint64
	for _, p := range paths {
		content := files[p]
		abs := filepath.Join(repoDir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		rec, err := cas.Put(ctx, strings.NewReader(content))
		if err != nil {
			t.Fatalf("CAS.Put(%s): %v", p, err)
		}
		if err := store.PutBlob(ctx, rec); err != nil {
			t.Fatalf("PutBlob(%s): %v", p, err)
		}
		fv := model.FileVersion{ID: model.NewFileID(h.Repo, p), Path: p, Status: model.FileTracked, Size: rec.Size, ContentHash: rec.Hash}
		h.files[p] = fv
		manifest.AddString(p)
		manifest.AddString(rec.Hash)
		total += uint64(rec.Size)
	}
	policyHash := model.H("providertest-policy")
	h.Snapshot = model.Snapshot{
		ID: model.NewSnapshotID(h.Repo, "", policyHash, manifest.Sum()), RepositoryID: h.Repo,
		CaptureConsistency: model.CaptureOperatorFrozen, SourcePolicyHash: policyHash,
		FileCount: uint64(len(paths)), SourceBytes: total, ManifestHash: manifest.Sum(), CreatedAt: time.Now().UTC(),
	}
	err = store.PutSnapshot(ctx, h.Snapshot, func(yield func(model.FileVersion) error) error {
		for _, p := range paths {
			if err := yield(h.files[p]); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("PutSnapshot: %v", err)
	}
	view, err := snapshot.OpenView(ctx, store, cas, h.Snapshot.ID)
	if err != nil {
		t.Fatalf("OpenView: %v", err)
	}
	h.View = view
	if h.Gen, err = store.BeginGeneration(ctx, h.Repo, h.Snapshot.ID, model.H("providertest-semantic")); err != nil {
		t.Fatalf("BeginGeneration: %v", err)
	}
	if h.Root, err = workspace.Discover(repoDir); err != nil {
		t.Fatalf("Discover: %v", err)
	}
	t.Cleanup(func() { h.Root.Close() })
	h.Policy = workspace.Policy{MaxFiles: 10000, DataDir: dataDir}
	return h
}

// File returns the snapshot manifest row for a harness path.
func (h *Harness) File(t *testing.T, path string) model.FileVersion {
	t.Helper()
	fv, ok := h.files[path]
	if !ok {
		t.Fatalf("harness has no file %q", path)
	}
	return fv
}

// Unit is one assigned unit: the run it belongs to, the identity storage
// opened for it and the request the provider received. Deps are the sealed
// units the resolver was built over.
type Unit struct {
	Run     model.ProviderRunID
	Build   model.UnitBuild
	Request provider.UnitRequest
	Deps    []model.UnitID
}

// Plan derives the unit identity for provider p over scopeKey and the given
// input paths with the given sealed dependencies, exactly as the coordinator
// will: inputs folded in ascending FileID order, the dependency digest over
// the sorted dependency keys and the unit key over both plus ConfigHash.
func (h *Harness) Plan(t *testing.T, p provider.Provider, scopeKey string, inputs []string, deps ...model.UnitID) Unit {
	t.Helper()
	d := p.Descriptor()
	run, err := h.Store.BeginProviderRun(h.ctx, h.Gen, d.ID, d.Version)
	if err != nil {
		t.Fatalf("BeginProviderRun: %v", err)
	}
	hasher := model.NewUnitInputHasher()
	for _, in := range h.inputs(t, inputs) {
		if err := hasher.Add(in); err != nil {
			t.Fatal(err)
		}
	}
	spec := model.UnitSpec{ProviderID: d.ID, ProviderVersion: d.Version, ScopeKey: scopeKey, InputHash: hasher.Sum(), DependencyHash: model.DependencyHash(deps)}
	spec.ID = model.NewUnitID(spec, ConfigHash)
	build := model.UnitBuild{Spec: spec, AnalysisConfigHash: ConfigHash, OriginRunID: run, SourceBinding: model.SourceBindingVerified, Dependencies: deps}
	resolver, err := reconcile.New(h.Store, h.Repo, deps)
	if err != nil {
		t.Fatalf("reconcile.New: %v", err)
	}
	req := provider.UnitRequest{
		Binding:  model.Binding{RepositoryID: h.Repo, SnapshotID: h.Snapshot.ID, GenerationID: h.Gen},
		Unit:     spec,
		Run:      run,
		Content:  h.View,
		Resolver: resolver,
	}
	return Unit{Run: run, Build: build, Request: req, Deps: deps}
}

// inputs maps paths to unit inputs in ascending FileID order.
func (h *Harness) inputs(t *testing.T, paths []string) []model.UnitInput {
	t.Helper()
	out := make([]model.UnitInput, 0, len(paths))
	for _, p := range paths {
		fv := h.File(t, p)
		out = append(out, model.UnitInput{FileID: fv.ID, ContentHash: fv.ContentHash, Executable: fv.Executable})
	}
	slices.SortFunc(out, func(a, b model.UnitInput) int { return strings.Compare(string(a.FileID), string(b.FileID)) })
	return out
}

// Begin opens the unit in storage and returns its output. The caller runs the
// provider through provider.RunUnit or drives the sink directly.
func (h *Harness) Begin(t *testing.T, u Unit, inputs []string) provider.UnitOutput {
	t.Helper()
	ins := h.inputs(t, inputs)
	w, err := h.Store.BeginUnit(h.ctx, h.Gen, u.Build, func(yield func(model.UnitInput) error) error {
		for _, in := range ins {
			if err := yield(in); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("BeginUnit: %v", err)
	}
	return provider.StoreUnit(h.Store, w)
}

// Run plans, opens and executes one unit of p end to end. It returns the
// provider's result and the unit identity; err is RunUnit's error so a test
// can assert on a failure path.
func (h *Harness) Run(t *testing.T, p provider.Provider, scopeKey string, inputs []string, deps ...model.UnitID) (model.ProviderResult, model.UnitID, error) {
	t.Helper()
	u := h.Plan(t, p, scopeKey, inputs, deps...)
	out := h.Begin(t, u, inputs)
	result, err := provider.RunUnit(h.ctx, p, u.Request, out, Limits, h.Pool)
	if cerr := h.Store.CompleteProviderRun(h.ctx, result, provider.CodeOf(err)); cerr != nil {
		t.Fatalf("CompleteProviderRun: %v", cerr)
	}
	return result, u.Build.Spec.ID, err
}

// UnitState reports whether the unit exists and in what state; it is how a
// test proves failed output was discarded rather than left building.
func (h *Harness) UnitState(t *testing.T, id model.UnitID) (model.UnitState, bool) {
	t.Helper()
	state, exists, err := h.Store.UnitState(h.ctx, id)
	if err != nil {
		t.Fatalf("UnitState: %v", err)
	}
	return state, exists
}

// Evidence builds a node evidence row for the unit req describes with the
// identity derived, the shape every provider fact needs.
func Evidence(req provider.UnitRequest, node model.NodeID, precision model.Precision, fv model.FileVersion, rng *model.SourceRange) model.Evidence {
	e := model.Evidence{UnitID: req.Unit.ID, ProviderID: req.Unit.ProviderID, ProviderVersion: req.Unit.ProviderVersion,
		OriginRunID: req.Run, NodeID: node, Precision: precision, FileID: fv.ID, ContentHash: fv.ContentHash, Range: rng}
	e.ID = model.NewEvidenceID(e)
	return e
}
