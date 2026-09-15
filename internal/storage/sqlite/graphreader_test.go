package sqlite_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"math"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Sawmonabo/codectx/internal/graph"
	"github.com/Sawmonabo/codectx/internal/graph/graphtest"
	"github.com/Sawmonabo/codectx/internal/model"
	store "github.com/Sawmonabo/codectx/internal/storage/sqlite"
	_ "modernc.org/sqlite"
)

// graphFixtureKeys is the canonical key behind every fixture node id. The suite
// derives its ids from these keys, so publishing them is what makes the store
// compute exactly the ids the suite resolves.
var graphFixtureKeys = map[model.NodeID]string{
	graphtest.PkgA:   graphtest.NodeKey("pkg/a"),
	graphtest.PkgB:   graphtest.NodeKey("pkg/b"),
	graphtest.DirD:   graphtest.NodeKey("dir/d"),
	graphtest.Hub:    graphtest.NodeKey("pkg/a#hub"),
	graphtest.Leaf:   graphtest.NodeKey("pkg/b#leaf"),
	graphtest.Orphan: graphtest.NodeKey("pkg/a#orphan"),
	graphtest.FileF:  graphtest.NodeKey("pkg/a/f.go"),
}

// graphFixtureStore builds the conformance fixture through the normal store
// API -- snapshot, provider run, units, seal, Activate -- so the packed
// adjacency under test is the one a real activation writes.
//
// PkgB is published BEFORE PkgA so the store's surrogates ascend in an order
// that is NOT the canonical-id order. Without that, a container tie-break on
// the surrogate and one on the canonical id agree by accident and the suite
// cannot tell a correct build from the likeliest defect.
//
// Two units are written: a member of the generation, and a non-member carrying
// one extra relation between two visible nodes. The non-member's relation must
// be absent from the packed form, which is what proves visibility is resolved
// at build time.
func graphFixtureStore(t *testing.T) (*store.Store, model.RepositoryID, string) {
	s, repo, dbPath, _, _ := graphFixtureStoreFull(t)
	return s, repo, dbPath
}

func graphFixtureStoreFull(t *testing.T) (*store.Store, model.RepositoryID, string, model.Snapshot, fileFixture) {
	t.Helper()
	ctx := context.Background()
	repo := graphtest.FixtureRepository
	dbPath := filepath.Join(t.TempDir(), "codectx.db")
	s, err := store.Open(ctx, dbPath, store.Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.EnsureRepository(ctx, repo, "/repo"); err != nil {
		t.Fatalf("EnsureRepository: %v", err)
	}
	src := "package a\n"
	ff := putFixtureFile(t, s, repo, "pkg/a/f.go", src)
	snap := putFixtureSnapshot(t, s, repo, ff, len(src))

	gen, err := s.BeginGeneration(ctx, repo, snap.ID, model.H("semantic"), "main")
	if err != nil {
		t.Fatalf("BeginGeneration: %v", err)
	}
	run, err := s.BeginProviderRun(ctx, gen, providerID, providerVersion)
	if err != nil {
		t.Fatalf("BeginProviderRun: %v", err)
	}

	// Ordered so PkgB is interned before PkgA.
	ordered := []model.NodeID{graphtest.PkgB, graphtest.PkgA, graphtest.DirD, graphtest.Hub,
		graphtest.Leaf, graphtest.Orphan, graphtest.FileF}
	byID := map[model.NodeID]model.Node{}
	for _, n := range graphtest.Nodes() {
		byID[n.ID] = n
	}

	member := beginFixtureUnit(t, s, gen, run, ff, "member")
	var nodes []model.NodeFact
	for _, id := range ordered {
		n := byID[id]
		ev := model.Evidence{UnitID: member.UnitID(), ProviderID: providerID, ProviderVersion: providerVersion,
			OriginRunID: run, NodeID: id, Precision: model.PrecisionSyntax}
		ev.ID = model.NewEvidenceID(ev)
		nodes = append(nodes, model.NodeFact{Node: n, CanonicalKey: graphFixtureKeys[id], Evidence: []model.Evidence{ev}})
	}
	if err := member.PutNodes(ctx, nodes); err != nil {
		t.Fatalf("PutNodes: %v", err)
	}
	// VisibleRelations, not Relations: activation refuses a relation whose
	// endpoint has no visible node fact, so the suite's deliberately dangling
	// relation cannot be published at all. The build's own visibility rule is
	// proved by the non-member unit below instead.
	if err := member.PutRelations(ctx, fixtureRelationFacts(t, member.UnitID(), run, graphtest.VisibleRelations())); err != nil {
		t.Fatalf("PutRelations: %v", err)
	}
	if err := s.SealUnit(ctx, member); err != nil {
		t.Fatalf("SealUnit: %v", err)
	}

	// The non-member unit belongs to no generation. Its relation names two
	// nodes that ARE visible, so only the generation semi-join keeps it out.
	orphanGen, err := s.BeginGeneration(ctx, repo, snap.ID, model.H("semantic-other"), "other")
	if err != nil {
		t.Fatalf("BeginGeneration(non-member): %v", err)
	}
	otherRun, err := s.BeginProviderRun(ctx, orphanGen, providerID, providerVersion)
	if err != nil {
		t.Fatalf("BeginProviderRun(non-member): %v", err)
	}
	outsider := beginFixtureUnit(t, s, orphanGen, otherRun, ff, "outsider")
	// A unit may only seal with both endpoints of each relation published in
	// it, so the outsider republishes the two nodes it joins. They are already
	// visible through the member unit, so the only thing keeping the relation
	// below out of the packed form is the generation semi-join.
	var outsiderNodes []model.NodeFact
	for _, id := range []model.NodeID{graphtest.Orphan, graphtest.Leaf} {
		n := byID[id]
		ev := model.Evidence{UnitID: outsider.UnitID(), ProviderID: providerID, ProviderVersion: providerVersion,
			OriginRunID: otherRun, NodeID: id, Precision: model.PrecisionSyntax}
		ev.ID = model.NewEvidenceID(ev)
		outsiderNodes = append(outsiderNodes, model.NodeFact{Node: n, CanonicalKey: graphFixtureKeys[id],
			Evidence: []model.Evidence{ev}})
	}
	if err := outsider.PutNodes(ctx, outsiderNodes); err != nil {
		t.Fatalf("PutNodes(non-member): %v", err)
	}
	hidden := model.Relation{ID: model.NewRelationID(repo, graphtest.Orphan, model.RelReferences, graphtest.Leaf),
		From: graphtest.Orphan, Kind: model.RelReferences, To: graphtest.Leaf}
	if err := outsider.PutRelations(ctx, fixtureRelationFacts(t, outsider.UnitID(), otherRun, []model.Relation{hidden})); err != nil {
		t.Fatalf("PutRelations(non-member): %v", err)
	}
	if err := s.SealUnit(ctx, outsider); err != nil {
		t.Fatalf("SealUnit(non-member): %v", err)
	}

	if _, err := s.Activate(ctx, gen, 0, model.HealthFresh, fixtureCaps, "norm-v1"); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	return s, repo, dbPath, snap, ff
}

// publishEmptyGeneration activates a second generation over the same snapshot,
// which supersedes the first. It republishes only one node, because its purpose
// is to move the active pointer, not to be walked.
func publishEmptyGeneration(t *testing.T, s *store.Store, repo model.RepositoryID,
	snap model.Snapshot, ff fileFixture, previous model.GenerationID) {
	t.Helper()
	ctx := context.Background()
	gen, err := s.BeginGeneration(ctx, repo, snap.ID, model.H("semantic-next"), "main")
	if err != nil {
		t.Fatalf("BeginGeneration(next): %v", err)
	}
	run, err := s.BeginProviderRun(ctx, gen, providerID, providerVersion)
	if err != nil {
		t.Fatalf("BeginProviderRun(next): %v", err)
	}
	w := beginFixtureUnit(t, s, gen, run, ff, "next")
	ev := model.Evidence{UnitID: w.UnitID(), ProviderID: providerID, ProviderVersion: providerVersion,
		OriginRunID: run, NodeID: graphtest.Orphan, Precision: model.PrecisionSyntax}
	ev.ID = model.NewEvidenceID(ev)
	var orphan model.Node
	for _, n := range graphtest.Nodes() {
		if n.ID == graphtest.Orphan {
			orphan = n
		}
	}
	if err := w.PutNodes(ctx, []model.NodeFact{{Node: orphan,
		CanonicalKey: graphFixtureKeys[graphtest.Orphan], Evidence: []model.Evidence{ev}}}); err != nil {
		t.Fatalf("PutNodes(next): %v", err)
	}
	if err := s.SealUnit(ctx, w); err != nil {
		t.Fatalf("SealUnit(next): %v", err)
	}
	if _, err := s.Activate(ctx, gen, previous, model.HealthFresh, fixtureCaps, "norm-v1"); err != nil {
		t.Fatalf("Activate(next): %v", err)
	}
}

func fixtureRelationFacts(t *testing.T, unit model.UnitID, run model.ProviderRunID, rels []model.Relation) []model.RelationFact {
	t.Helper()
	out := make([]model.RelationFact, 0, len(rels))
	for _, r := range rels {
		ev := model.Evidence{UnitID: unit, ProviderID: providerID, ProviderVersion: providerVersion,
			OriginRunID: run, RelationID: r.ID, Precision: model.PrecisionSyntax}
		ev.ID = model.NewEvidenceID(ev)
		out = append(out, model.RelationFact{Relation: r, Evidence: []model.Evidence{ev}})
	}
	return out
}

func putFixtureFile(t *testing.T, s *store.Store, repo model.RepositoryID, path, content string) fileFixture {
	t.Helper()
	sum := sha256.Sum256([]byte(content))
	ff := fileFixture{path: path, id: model.NewFileID(repo, path), content: []byte(content),
		hash: hex.EncodeToString(sum[:])}
	rec := model.BlobRecord{Hash: ff.hash, Size: int64(len(content))}
	for off := 0; off < len(content); off += model.BlobBlockBytes {
		end := min(off+model.BlobBlockBytes, len(content))
		d := sha256.Sum256([]byte(content[off:end]))
		rec.BlockDigests = append(rec.BlockDigests, hex.EncodeToString(d[:]))
	}
	rec.LineCheckpoints = []model.LineCheckpoint{{ByteOffset: 0, LineNumber: 1, LineStartByte: 0}}
	if err := s.PutBlob(context.Background(), rec); err != nil {
		t.Fatalf("PutBlob: %v", err)
	}
	return ff
}

// openRawDB opens the fixture database beside the store, so a test can count
// rows the public surface does not report.
func openRawDB(t *testing.T, dbPath string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func putFixtureSnapshot(t *testing.T, s *store.Store, repo model.RepositoryID, ff fileFixture, size int) model.Snapshot {
	t.Helper()
	manifest := model.H("manifest-graph", ff.path)
	snap := model.Snapshot{ID: model.NewSnapshotID(repo, "", model.H("policy"), manifest), RepositoryID: repo,
		CaptureConsistency: model.CaptureValidated, SourcePolicyHash: model.H("policy"), FileCount: 1,
		SourceBytes: uint64(size), ManifestHash: manifest, CreatedAt: time.Now().UTC()}
	err := s.PutSnapshot(context.Background(), snap, func(yield func(model.FileVersion) error) error {
		return yield(model.FileVersion{ID: ff.id, Path: ff.path, Status: model.FileTracked,
			Size: int64(len(ff.content)), ContentHash: ff.hash, Language: "go"})
	})
	if err != nil {
		t.Fatalf("PutSnapshot: %v", err)
	}
	return snap
}

func beginFixtureUnit(t *testing.T, s *store.Store, gen model.GenerationID, run model.ProviderRunID,
	ff fileFixture, scope string) *store.UnitWriter {
	t.Helper()
	input := model.UnitInput{FileID: ff.id, ContentHash: ff.hash}
	h := model.NewUnitInputHasher()
	if err := h.Add(input); err != nil {
		t.Fatal(err)
	}
	spec := model.UnitSpec{ProviderID: providerID, ProviderVersion: providerVersion, ScopeKey: scope,
		InputHash: h.Sum(), DependencyHash: model.DependencyHash(nil)}
	spec.ID = model.NewUnitID(spec, configHash)
	build := model.UnitBuild{Spec: spec, AnalysisConfigHash: configHash, OriginRunID: run,
		SourceBinding: model.SourceBindingVerified}
	w, err := s.BeginUnit(context.Background(), gen, build, func(yield func(model.UnitInput) error) error { return yield(input) })
	if err != nil {
		t.Fatalf("BeginUnit(%s): %v", scope, err)
	}
	return w
}

// openGraphReader pins the fixture store's active generation and opens the
// packed reader over it.
func openGraphReader(t *testing.T, s *store.Store, repo model.RepositoryID) graph.GraphReader {
	t.Helper()
	ctx := context.Background()
	r, err := s.PinGeneration(ctx, repo, 0, time.Minute)
	if err != nil {
		t.Fatalf("PinGeneration: %v", err)
	}
	t.Cleanup(func() { r.Close() })
	g, err := store.NewGraphReader(ctx, r)
	if err != nil {
		t.Fatalf("NewGraphReader: %v", err)
	}
	return g
}

// The store's packed reader answers exactly as the in-heap reference does. A
// behaviour the two disagree on is a defect in one of them, not an accident of
// a fixture.
func TestPackedGraphReaderConformance(t *testing.T) {
	s, repo, _ := graphFixtureStore(t)
	graphtest.RunConformance(t, func(t *testing.T) graph.GraphReader { return openGraphReader(t, s, repo) })
}

// A list that straddles a part boundary is stitched. Production part sizes put
// this case beyond any fixture, so the part size is shrunk to a few bytes and
// the SAME conformance suite runs: every list of the fixture then spans several
// parts, and a reader that decodes one part at a time fails it.
func TestPackedGraphReaderStitchesSplitParts(t *testing.T) {
	restore := store.SetEdgePartBytes(3)
	defer restore()
	s, repo, dbPath := graphFixtureStore(t)
	parts := countGraphParts(t, openRawDB(t, dbPath), "out.edges")
	if parts < 3 {
		t.Fatalf("out.edges has %d parts; the fixture must span several for this to prove stitching", parts)
	}
	graphtest.RunConformance(t, func(t *testing.T) graph.GraphReader { return openGraphReader(t, s, repo) })
}

func countGraphParts(t *testing.T, db *sql.DB, stream string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM generation_graph_parts WHERE stream = ?`, stream).Scan(&n); err != nil {
		t.Fatalf("count parts: %v", err)
	}
	return n
}

// Neither ordered scan of the build may build a temporary b-tree: the sort is
// the whole visible relation set, and a temp b-tree over it is the defect
// ADR-0005 exists to remove.
func TestGraphBuildScansUseNoTempBTree(t *testing.T) {
	_, _, dbPath := graphFixtureStore(t)
	db := openRawDB(t, dbPath)
	if _, err := db.Exec(`CREATE TEMP TABLE tmp_graph_rel(id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatalf("tmp_graph_rel: %v", err)
	}
	for _, q := range []struct {
		name  string
		query string
	}{
		{"outgoing", store.OutgoingEdgeQuery()},
		{"incoming", store.IncomingEdgeQuery()},
	} {
		plan := explain(t, db, q.query)
		t.Logf("%s plan:\n%s", q.name, plan)
		if strings.Contains(strings.ToUpper(plan), "TEMP B-TREE") {
			t.Fatalf("%s scan builds a temporary b-tree over every visible relation:\n%s", q.name, plan)
		}
	}
}

func explain(t *testing.T, db *sql.DB, query string) string {
	t.Helper()
	rows, err := db.Query("EXPLAIN QUERY PLAN " + query)
	if err != nil {
		t.Fatalf("EXPLAIN QUERY PLAN: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatalf("scan plan: %v", err)
		}
		out = append(out, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("plan rows: %v", err)
	}
	return strings.Join(out, "\n")
}

// Collecting a generation drops its packed adjacency with it: the two tables
// cascade, so a graph never outlives the generation it describes.
func TestDeleteGenerationCascadesPackedGraph(t *testing.T) {
	s, repo, dbPath, snap, ff := graphFixtureStoreFull(t)
	ctx := context.Background()
	db := openRawDB(t, dbPath)
	before := graphRowCounts(t, db)
	if before.header != 1 || before.parts == 0 {
		t.Fatalf("activation wrote %+v, want one header row and at least one part", before)
	}
	gen, err := s.ActiveGeneration(ctx, repo)
	if err != nil {
		t.Fatalf("ActiveGeneration: %v", err)
	}
	// A generation is collectable only once it is superseded, so a second one
	// is published over the same snapshot first.
	publishEmptyGeneration(t, s, repo, snap, ff, gen)
	if err := s.DeleteGeneration(ctx, gen); err != nil {
		t.Fatalf("DeleteGeneration: %v", err)
	}
	after := graphRowCounts(t, db)
	if after.header != 1 || after.parts == 0 {
		t.Fatalf("after DeleteGeneration %+v; the surviving generation lost its packed adjacency", after)
	}
	var orphaned int
	if err := db.QueryRow(`SELECT count(*) FROM generation_graph_parts WHERE generation_id = ?`, int64(gen)).
		Scan(&orphaned); err != nil {
		t.Fatalf("count orphaned parts: %v", err)
	}
	if orphaned != 0 {
		t.Fatalf("%d parts of the collected generation remain; the packed adjacency outlived its generation", orphaned)
	}
}

type graphCounts struct{ header, parts int }

func graphRowCounts(t *testing.T, db *sql.DB) graphCounts {
	t.Helper()
	var c graphCounts
	if err := db.QueryRow(`SELECT (SELECT count(*) FROM generation_graph), (SELECT count(*) FROM generation_graph_parts)`).
		Scan(&c.header, &c.parts); err != nil {
		t.Fatalf("count graph rows: %v", err)
	}
	return c
}

// lockedBuffer is a writer safe to hand a slog handler while the store runs.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// The build's completion line must state the build's own memory share.
//
// The failure mode it protects: ADR-0005 bounds what the one sequential pass
// activation adds, and an operator holding a run to that bound can only
// attribute a memory ceiling to this pass if the pass reports its own heap
// delta. A build that reports duration alone leaves its share of a run's peak
// indistinguishable from the rest of the activation, so the bound stops being
// checkable from the logs.
//
// slog's default logger is process-global, so this case does not run parallel
// and restores what it replaced.
func TestPackedGraphBuildLogsItsOwnHeapShare(t *testing.T) {
	var out lockedBuffer
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })
	slog.SetDefault(slog.New(slog.NewJSONHandler(&out, &slog.HandlerOptions{Level: slog.LevelInfo})))

	// Publishing the graph fixture activates a generation, which is the only
	// thing that runs the packed adjacency build.
	graphFixtureStore(t)

	var finished map[string]any
	for line := range strings.SplitSeq(strings.TrimSpace(out.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line %q is not JSON: %v", line, err)
		}
		if rec["msg"] == "packed adjacency build finished" {
			finished = rec
		}
	}
	if finished == nil {
		t.Fatalf("no packed adjacency build finished line in:\n%s", out.String())
	}
	raw, ok := finished["heap_delta_bytes"]
	if !ok {
		t.Fatalf("the build's completion line carries %v and no heap_delta_bytes: the pass reports its "+
			"wall clock without its memory share", finished)
	}
	delta, ok := raw.(float64)
	if !ok || delta != math.Trunc(delta) {
		t.Fatalf("heap_delta_bytes is %#v, want a whole number of bytes", raw)
	}
	t.Logf("packed adjacency build: duration_ms=%v heap_delta_bytes=%v", finished["duration_ms"], raw)
}
