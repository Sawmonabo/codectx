package sqlite_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Sawmonabo/codectx/internal/graph"
	"github.com/Sawmonabo/codectx/internal/graph/graphtest"
	"github.com/Sawmonabo/codectx/internal/model"
	store "github.com/Sawmonabo/codectx/internal/storage/sqlite"
)

// TestDependencyAnswerCarriesTheLockDetail asserts that a graph answer over
// `depends_on` lists the relation's occurrence with its evidence detail, so
// a lock entry's resolved version, integrity and set size reach the answer
// that names the dependency.
//
// Mutations it catches: occurrenceKind not listing `depends_on` (the answer
// carries no occurrence); the store's RelationOccurrences dropping the
// evidence row's detail (the occurrence carries none).
func TestDependencyAnswerCarriesTheLockDetail(t *testing.T) {
	ctx := context.Background()
	repo := graphtest.FixtureRepository
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "codectx.db"), store.WithDerivedReaders(store.Options{}))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.EnsureRepository(ctx, repo, "/repo"); err != nil {
		t.Fatalf("EnsureRepository: %v", err)
	}
	const src = `{"packages": {"node_modules/left-pad": {"version": "1.3.0"}}}`
	ff := putFixtureFile(t, s, repo, graphtest.FixtureFilePath, src)
	snap := putFixtureSnapshot(t, s, repo, ff, len(src))
	gen, err := s.BeginGeneration(ctx, repo, snap.ID, model.H("semantic-detail"), "main")
	if err != nil {
		t.Fatalf("BeginGeneration: %v", err)
	}
	run, err := s.BeginProviderRun(ctx, gen, providerID, providerVersion)
	if err != nil {
		t.Fatalf("BeginProviderRun: %v", err)
	}
	w := beginFixtureUnit(t, s, gen, run, ff, "detail")

	cfgKey, depKey := graphtest.NodeKey("pkg/a#lock"), graphtest.NodeKey("npm:left-pad")
	cfg := model.NewNodeID(repo, model.NodeConfiguration, cfgKey)
	dep := model.NewNodeID(repo, model.NodeDependency, depKey)
	whole := &model.SourceRange{Start: model.Position{Byte: 0, Line: 1}, End: model.Position{Byte: uint64(len(src)), Line: 1}}
	i := strings.Index(src, `"node_modules/left-pad"`)
	entry := &model.SourceRange{Start: model.Position{Byte: uint64(i), Line: 1}, End: model.Position{Byte: uint64(len(src) - 1), Line: 1}}
	var facts []model.NodeFact
	for _, n := range []struct {
		n   model.Node
		key string
	}{
		{model.Node{ID: cfg, Kind: model.NodeConfiguration, Name: "package-lock.json", FileID: ff.id, ContentHash: ff.hash, Range: whole}, cfgKey},
		{model.Node{ID: dep, Kind: model.NodeDependency, Name: "left-pad"}, depKey},
	} {
		ev := model.Evidence{UnitID: w.UnitID(), ProviderID: providerID, ProviderVersion: providerVersion,
			OriginRunID: run, NodeID: n.n.ID, Precision: model.PrecisionSyntax}
		ev.ID = model.NewEvidenceID(ev)
		facts = append(facts, model.NodeFact{Node: n.n, CanonicalKey: n.key, Evidence: []model.Evidence{ev}})
	}
	if err := w.PutNodes(ctx, facts); err != nil {
		t.Fatalf("PutNodes: %v", err)
	}
	const detail = `{"integrity":"sha512-abc","kind":"locked","resolved_version":"1.3.0","set_size":"1"}`
	rel := model.Relation{ID: model.NewRelationID(repo, cfg, model.RelDependsOn, dep), From: cfg, Kind: model.RelDependsOn, To: dep}
	ev := model.Evidence{UnitID: w.UnitID(), ProviderID: providerID, ProviderVersion: providerVersion, OriginRunID: run,
		RelationID: rel.ID, Precision: model.PrecisionSyntax, FileID: ff.id, ContentHash: ff.hash, Range: entry, Detail: detail}
	ev.ID = model.NewEvidenceID(ev)
	if err := w.PutRelations(ctx, []model.RelationFact{{Relation: rel, Evidence: []model.Evidence{ev}}}); err != nil {
		t.Fatalf("PutRelations: %v", err)
	}
	if err := s.SealUnit(ctx, w); err != nil {
		t.Fatalf("SealUnit: %v", err)
	}
	if _, err := s.Activate(ctx, gen, 0, model.HealthFresh, fixtureCaps, "norm-v1"); err != nil {
		t.Fatalf("Activate: %v", err)
	}

	g := openGraphReader(t, s, repo)
	adj, ok := g.(graph.Adjacency)
	if !ok {
		t.Fatal("the store's graph reader does not hydrate nodes")
	}
	e, err := graph.New(graph.Options{Adjacency: adj, Reader: g, Limits: graph.Limits{
		MaxDepth: 3, MaxVisited: 1000, MaxEdges: 1000, MaxPageItems: 200,
		QueryTimeout: 10 * time.Second, CursorTTL: time.Minute, FrontierBytes: 1 << 20,
	}})
	if err != nil {
		t.Fatalf("graph.New: %v", err)
	}
	res, err := e.Neighbors(ctx, model.GraphRequest{Start: []model.NodeID{cfg},
		Direction: model.DirectionOutgoing, Relations: []model.RelationKind{model.RelDependsOn}})
	if err != nil {
		t.Fatalf("dependencies: %v", err)
	}
	if err := res.Validate(); err != nil {
		t.Fatalf("the answer is invalid: %v", err)
	}
	if len(res.Occurrences) != 1 || res.Occurrences[0].RelationID != rel.ID || res.Occurrences[0].Detail != detail {
		t.Fatalf("the depends_on answer lists occurrences %+v, want one of %s with detail %s", res.Occurrences, rel.ID, detail)
	}
}
