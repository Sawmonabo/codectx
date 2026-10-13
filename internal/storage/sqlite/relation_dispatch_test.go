package sqlite_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/Sawmonabo/codectx/internal/graph"
	"github.com/Sawmonabo/codectx/internal/graph/graphtest"
	"github.com/Sawmonabo/codectx/internal/model"
	store "github.com/Sawmonabo/codectx/internal/storage/sqlite"
)

// TestRelationDispatchReadsTheValueTechniques asserts that the store's
// dispatch summary says which `calls` relations a technique bound through
// the values the project gives the site (graph.ValueDetails): a call through
// a function pointer bound to the one function whose address the project
// takes is one of the project's own candidates, a direct call is not.
//
// Mutation caught: the summary query not reading the evidence detail (both
// read alike, and the one-target pointer call loses its value_set mark).
func TestRelationDispatchReadsTheValueTechniques(t *testing.T) {
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
	const src = "void f(int x) {}\nvoid call(void) { on_exit(1); f(2); }\n"
	ff := putFixtureFile(t, s, repo, graphtest.FixtureFilePath, src)
	snap := putFixtureSnapshot(t, s, repo, ff, len(src))
	gen, err := s.BeginGeneration(ctx, repo, snap.ID, model.H("value-techniques"), "main")
	if err != nil {
		t.Fatalf("BeginGeneration: %v", err)
	}
	run, err := s.BeginProviderRun(ctx, gen, providerID, providerVersion)
	if err != nil {
		t.Fatalf("BeginProviderRun: %v", err)
	}
	w := beginFixtureUnit(t, s, gen, run, ff, "value")
	at := func(start, end int) *model.SourceRange {
		return &model.SourceRange{Start: model.Position{Byte: uint64(start), Line: 1}, End: model.Position{Byte: uint64(end), Line: 1}}
	}
	fKey, callKey, extKey := graphtest.NodeKey("c#f"), graphtest.NodeKey("c#call"), graphtest.NodeKey("c#ext")
	f := model.NewNodeID(repo, model.NodeFunction, fKey)
	call := model.NewNodeID(repo, model.NodeFunction, callKey)
	ext := model.NewNodeID(repo, model.NodeFunction, extKey)
	var facts []model.NodeFact
	for _, n := range []struct {
		n   model.Node
		key string
	}{
		{model.Node{ID: f, Kind: model.NodeFunction, Name: "f", FileID: ff.id, ContentHash: ff.hash, Range: at(0, 16)}, fKey},
		{model.Node{ID: call, Kind: model.NodeFunction, Name: "call", FileID: ff.id, ContentHash: ff.hash, Range: at(17, 54)}, callKey},
		{model.Node{ID: ext, Kind: model.NodeFunction, Name: "g", FileID: ff.id, ContentHash: ff.hash, Range: at(17, 54)}, extKey},
	} {
		ev := model.Evidence{UnitID: w.UnitID(), ProviderID: providerID, ProviderVersion: providerVersion,
			OriginRunID: run, NodeID: n.n.ID, Precision: model.PrecisionSyntax}
		ev.ID = model.NewEvidenceID(ev)
		facts = append(facts, model.NodeFact{Node: n.n, CanonicalKey: n.key, Evidence: []model.Evidence{ev}})
	}
	if err := w.PutNodes(ctx, facts); err != nil {
		t.Fatalf("PutNodes: %v", err)
	}
	rel := func(to model.NodeID, rng *model.SourceRange, detail string) model.RelationFact {
		r := model.Relation{ID: model.NewRelationID(repo, call, model.RelCalls, to), From: call, Kind: model.RelCalls, To: to}
		ev := model.Evidence{UnitID: w.UnitID(), ProviderID: providerID, ProviderVersion: providerVersion, OriginRunID: run,
			RelationID: r.ID, Precision: model.PrecisionStaticAnalysis, FileID: ff.id, ContentHash: ff.hash, Range: rng,
			Detail: detail, Dispatch: model.DispatchExact}
		ev.ID = model.NewEvidenceID(ev)
		return model.RelationFact{Relation: r, Evidence: []model.Evidence{ev}}
	}
	pointer, direct := rel(f, at(35, 42), graph.CallDetailAddressTaken), rel(ext, at(46, 47), "direct")
	if err := w.PutRelations(ctx, []model.RelationFact{pointer, direct}); err != nil {
		t.Fatalf("PutRelations: %v", err)
	}
	if err := s.SealUnit(ctx, w); err != nil {
		t.Fatalf("SealUnit: %v", err)
	}
	if _, err := s.Activate(ctx, gen, 0, model.HealthFresh, fixtureCaps, "norm-v1"); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	dr, ok := openGraphReader(t, s, repo).(graph.DispatchReader)
	if !ok {
		t.Fatal("the store's graph reader does not summarize dispatch")
	}
	sums, err := dr.RelationDispatch(ctx, []model.RelationID{pointer.Relation.ID, direct.Relation.ID})
	if err != nil {
		t.Fatalf("RelationDispatch: %v", err)
	}
	if len(sums) != 2 || !sums[0].ValueBound || sums[1].ValueBound {
		t.Fatalf("dispatch summaries = %+v, want the address-taken call value-bound and the direct call not", sums)
	}
}
