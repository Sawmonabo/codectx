package delta_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Sawmonabo/codectx/internal/index/delta"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider"
	"github.com/Sawmonabo/codectx/internal/reconcile"
	"github.com/Sawmonabo/codectx/internal/snapshot"
	"github.com/Sawmonabo/codectx/internal/storage/sqlite"

	_ "modernc.org/sqlite"
)

// The appliers are exercised against a real store on a temporary path, and the
// facts a refresh sealed are read back out of the database file itself: what a
// delta must produce is what a full import of the same tree produces, and only
// the rows can say whether it did.
//
// providertest.Harness stands up one snapshot and one generation, which a
// refresh cannot use — it needs a predecessor tree, an edited tree, a
// generation per build and the database path to read back. This fixture is
// that, over the same real store, CAS and snapshot view.

// fixture is one repository over one store.
type fixture struct {
	t      *testing.T
	ctx    context.Context
	dbPath string
	dir    string
	store  *sqlite.Store
	cas    *snapshot.CAS
	repo   model.RepositoryID
	pool   *provider.Pool
}

// limits are the sink bounds every unit here runs under: small enough that a
// test could reach one deliberately, large enough that a handful of facts
// never does by accident.
var limits = provider.Limits{BatchRecords: 64, BatchBytes: 256 << 10, MaxRecordBytes: 64 << 10}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	dataDir := filepath.Join(dir, "data")
	cas, err := snapshot.OpenCAS(snapshot.CASDir(dataDir))
	if err != nil {
		t.Fatalf("OpenCAS: %v", err)
	}
	dbPath := filepath.Join(dataDir, "codectx.db")
	store, err := sqlite.Open(ctx, dbPath, sqlite.Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	pool, err := provider.NewPool(4 * limits.BatchBytes)
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, ctx: ctx, dbPath: dbPath, dir: dir, store: store, cas: cas, pool: pool,
		repo: model.RepositoryID(model.H("delta-test-repo"))}
	if err := store.EnsureRepository(ctx, f.repo, filepath.Join(dir, "repo")); err != nil {
		t.Fatalf("EnsureRepository: %v", err)
	}
	return f
}

// tree is one recorded snapshot of a file set, with the manifest rows a unit's
// inputs are taken from.
type tree struct {
	snap  model.Snapshot
	view  model.SnapshotView
	files map[string]model.FileVersion
	paths []string
}

// snapshot retains every file in the CAS and records the snapshot over them.
// Two trees with the same contents are the same snapshot, which is what lets a
// refresh and the full import it is compared against read the same source.
func (f *fixture) snapshot(files map[string]string) *tree {
	f.t.Helper()
	tr := &tree{files: map[string]model.FileVersion{}}
	for p := range files {
		tr.paths = append(tr.paths, p)
	}
	slices.Sort(tr.paths)
	manifest := model.NewHasher("delta-test-manifest")
	var total uint64
	for _, p := range tr.paths {
		rec, err := f.cas.Put(f.ctx, strings.NewReader(files[p]))
		if err != nil {
			f.t.Fatalf("CAS.Put(%s): %v", p, err)
		}
		if err := f.store.PutBlob(f.ctx, rec); err != nil {
			f.t.Fatalf("PutBlob(%s): %v", p, err)
		}
		tr.files[p] = model.FileVersion{ID: model.NewFileID(f.repo, p), Path: p, Status: model.FileTracked,
			Size: rec.Size, ContentHash: rec.Hash}
		manifest.AddString(p)
		manifest.AddString(rec.Hash)
		total += uint64(rec.Size)
	}
	policyHash := model.H("delta-test-policy")
	tr.snap = model.Snapshot{
		ID: model.NewSnapshotID(f.repo, "", policyHash, manifest.Sum()), RepositoryID: f.repo,
		CaptureConsistency: model.CaptureOperatorFrozen, SourcePolicyHash: policyHash,
		FileCount: uint64(len(tr.paths)), SourceBytes: total, ManifestHash: manifest.Sum(), CreatedAt: time.Now().UTC(),
	}
	err := f.store.PutSnapshot(f.ctx, tr.snap, func(yield func(model.FileVersion) error) error {
		for _, p := range tr.paths {
			if err := yield(tr.files[p]); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		f.t.Fatalf("PutSnapshot: %v", err)
	}
	if tr.view, err = snapshot.OpenView(f.ctx, f.store, f.cas, tr.snap.ID); err != nil {
		f.t.Fatalf("OpenView: %v", err)
	}
	return tr
}

// generation opens a staging generation over a tree.
func (f *fixture) generation(tr *tree) model.GenerationID {
	f.t.Helper()
	gen, err := f.store.BeginGeneration(f.ctx, f.repo, tr.snap.ID, model.H("delta-test-semantic"), "refs/heads/delta-test")
	if err != nil {
		f.t.Fatalf("BeginGeneration: %v", err)
	}
	return gen
}

// inputsOf maps paths to unit inputs in the ascending FileID order BeginUnit
// and the appliers' merge join both require.
func (tr *tree) inputsOf(t *testing.T, paths []string) []model.UnitInput {
	t.Helper()
	out := make([]model.UnitInput, 0, len(paths))
	for _, p := range paths {
		fv, ok := tr.files[p]
		if !ok {
			t.Fatalf("the tree has no file %q", p)
		}
		out = append(out, model.UnitInput{FileID: fv.ID, ContentHash: fv.ContentHash, Executable: fv.Executable})
	}
	slices.SortFunc(out, func(a, b model.UnitInput) int { return strings.Compare(string(a.FileID), string(b.FileID)) })
	return out
}

// build is one unit a test asks an applier for: the tree it is built over, the
// generation it belongs to, the analysis configuration that keys it apart from
// its sibling builds, the paths it declares and the predecessor it refreshes.
type build struct {
	tree     *tree
	gen      model.GenerationID
	cfgHash  string
	scopeKey string
	paths    []string
	previous model.UnitID
}

// request derives the unit identity exactly as the coordinator does — inputs
// folded in ascending FileID order, the unit key over the spec plus the
// analysis configuration — and returns the delta request over it.
func (f *fixture) request(p provider.Provider, b build) delta.Request {
	f.t.Helper()
	d := p.Descriptor()
	run, err := f.store.BeginProviderRun(f.ctx, b.gen, d.ID, d.Version)
	if err != nil {
		f.t.Fatalf("BeginProviderRun: %v", err)
	}
	ins := b.tree.inputsOf(f.t, b.paths)
	hasher := model.NewUnitInputHasher()
	for _, in := range ins {
		if err := hasher.Add(in); err != nil {
			f.t.Fatal(err)
		}
	}
	spec := model.UnitSpec{ProviderID: d.ID, ProviderVersion: d.Version, ScopeKey: b.scopeKey,
		InputHash: hasher.Sum(), DependencyHash: model.DependencyHash(nil)}
	spec.ID = model.NewUnitID(spec, b.cfgHash)
	resolver, err := reconcile.New(f.store, f.repo, nil)
	if err != nil {
		f.t.Fatalf("reconcile.New: %v", err)
	}
	return delta.Request{
		Generation: b.gen,
		Previous:   b.previous,
		Build: model.UnitBuild{Spec: spec, AnalysisConfigHash: b.cfgHash, OriginRunID: run,
			SourceBinding: model.SourceBindingVerified},
		Inputs: func(yield func(model.UnitInput) error) error {
			for _, in := range ins {
				if err := yield(in); err != nil {
					return err
				}
			}
			return nil
		},
		Unit: provider.UnitRequest{
			Binding:  model.Binding{RepositoryID: f.repo, SnapshotID: b.tree.snap.ID, GenerationID: b.gen},
			Unit:     spec,
			Run:      run,
			Content:  b.tree.view,
			Resolver: resolver,
		},
		WorkDir: filepath.Join(f.dir, "work", string(spec.ID)),
	}
}

// flush commits the ingestion group. A predecessor a refresh reads through the
// reader pool must be committed, as it is by the activation of the generation
// it belongs to, before the run that carries from it begins. It is load
// bearing and says so when it is missing: without it the pool read finds no
// unit at all and the refresh fails with CTX_ARGUMENT_INVALID rather than
// quietly finding an empty set of previous inputs.
func (f *fixture) flush() {
	f.t.Helper()
	if err := f.store.Flush(f.ctx); err != nil {
		f.t.Fatalf("Flush: %v", err)
	}
}

// raw opens the database file for the fact comparison.
func (f *fixture) raw() *sql.DB {
	f.t.Helper()
	db, err := sql.Open("sqlite", f.dbPath)
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { db.Close() })
	return db
}

// factColumns are the identifying columns of each fact table with every
// unit-scoped column excluded: the row's own unit id, the rowid search_units
// mints on insert, and the evidence id, which folds the unit id by
// construction and so can never match across two units describing the same
// occurrence. The surrogates are compared as stored — both sides are units of
// the same database, where one surrogate is one canonical identity, so this is
// also what proves the refreshed unit references the dictionary rows the full
// import did.
var factColumns = []struct{ table, cols string }{
	{"node_facts", "node_id, language, name, qualified_name, signature, lower(hex(coalesce(file_id, x''))), coalesce(start_byte,-1), coalesce(end_byte,-1), metadata_json"},
	{"relation_facts", "relation_id"},
	{"fact_keys", "coalesce(node_id, 0), coalesce(relation_id, 0), fact_key"},
	{"native_aliases", "scope_key_id, native_key_id, node_id"},
	{"search_units", "lower(hex(search_key)), coalesce(node_id, 0), lower(hex(file_id)), path, kind, name, qualified_name, signature, start_byte, end_byte, token_count"},
	{"evidence", "coalesce(node_id, 0), coalesce(relation_id, 0), precision, lower(hex(coalesce(file_id, x''))), coalesce(start_byte,-1), coalesce(end_byte,-1), native_key_id, detail, content_hash_bound"},
}

// sameFacts asserts that the refreshed unit holds exactly the facts the full
// import of the same tree holds.
func sameFacts(t *testing.T, db *sql.DB, full, refreshed model.UnitID) {
	t.Helper()
	want, got := unitRowID(t, db, full), unitRowID(t, db, refreshed)
	for _, tb := range factColumns {
		for _, dir := range []struct {
			what string
			a, b int64
		}{{"missing from the refreshed unit", want, got}, {"only in the refreshed unit", got, want}} {
			var n int64
			q := `SELECT count(*) FROM (SELECT ` + tb.cols + ` FROM ` + tb.table + ` WHERE unit_id = ?1
				EXCEPT SELECT ` + tb.cols + ` FROM ` + tb.table + ` WHERE unit_id = ?2)`
			if err := db.QueryRow(q, dir.a, dir.b).Scan(&n); err != nil {
				t.Fatalf("%s: %v", tb.table, err)
			}
			if n != 0 {
				t.Errorf("%s: %d rows %s", tb.table, n, dir.what)
			}
		}
	}
}

func unitRowID(t *testing.T, db *sql.DB, id model.UnitID) int64 {
	t.Helper()
	raw, _ := model.DecodeID(string(id))
	var row int64
	if err := db.QueryRow(`SELECT id FROM units WHERE unit_key = ?`, raw).Scan(&row); err != nil {
		t.Fatalf("unit row for %s: %v", id, err)
	}
	return row
}
