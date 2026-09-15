package sqlite_test

import (
	"context"
	"testing"
	"time"

	"github.com/Sawmonabo/codectx/internal/graph"
	"github.com/Sawmonabo/codectx/internal/model"
	store "github.com/Sawmonabo/codectx/internal/storage/sqlite"
)

// The container slot must be answerable on the facts a language provider really
// publishes. A provider attaches a TOP-LEVEL declaration to the module that
// holds it with `defines` and reserves `contains` for a NESTED one, so a build
// that reads the slot from a container-kind `contains` parent alone finds
// nothing: the rollup, the repository map's package rollup and impact's
// container attribution then all present a repository as having no packages,
// which is a wrong measurement rather than a missing one.
//
// The fixture is that shape -- one file, its module, a top-level declaration
// reached by `defines`, a declaration nested inside it by `contains`, and the
// file node itself -- and every one of them must land in the module. Dropping
// the file claim from the build returns the slot to zero and fails this test.
func TestPackedContainerSlotFollowsTheFilesContainer(t *testing.T) {
	ctx := context.Background()
	repo := model.RepositoryID("00000000000000000000000000000000000000000000000000000000000000c1")
	s, err := store.Open(ctx, t.TempDir()+"/codectx.db", store.Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.EnsureRepository(ctx, repo, "/repo-container"); err != nil {
		t.Fatalf("EnsureRepository: %v", err)
	}
	src := "package a\n"
	ff := putFixtureFile(t, s, repo, "pkg/a/f.go", src)
	snap := putFixtureSnapshot(t, s, repo, ff, len(src))

	key := func(k string) string { return model.CanonicalNodeKey(k, "") }
	id := func(kind model.NodeKind, k string) model.NodeID {
		return model.NewNodeID(repo, kind, key(k))
	}
	var (
		module  = id(model.NodeModule, "pkg/a/f.go#mod")
		top     = id(model.NodeFunction, "pkg/a/f.go#top")
		nested  = id(model.NodeVariable, "pkg/a/f.go#top.v")
		fileNod = id(model.NodeFile, "pkg/a/f.go")
		dir     = id(model.NodeDirectory, "pkg/a")
	)
	nodes := []struct {
		id   model.NodeID
		kind model.NodeKind
		name string
		key  string
		file model.FileID
	}{
		{module, model.NodeModule, "f.go", "pkg/a/f.go#mod", ff.id},
		{top, model.NodeFunction, "top", "pkg/a/f.go#top", ff.id},
		{nested, model.NodeVariable, "v", "pkg/a/f.go#top.v", ff.id},
		{fileNod, model.NodeFile, "f.go", "pkg/a/f.go", ff.id},
		// The directory carries no file and claims the file node by
		// containment: it must never take the slot, from either claim.
		{dir, model.NodeDirectory, "a", "pkg/a", ""},
	}

	gen, err := s.BeginGeneration(ctx, repo, snap.ID, model.H("semantic"), "main")
	if err != nil {
		t.Fatalf("BeginGeneration: %v", err)
	}
	run, err := s.BeginProviderRun(ctx, gen, providerID, providerVersion)
	if err != nil {
		t.Fatalf("BeginProviderRun: %v", err)
	}
	w := beginFixtureUnit(t, s, gen, run, ff, "container")
	facts := make([]model.NodeFact, 0, len(nodes))
	for _, n := range nodes {
		ev := model.Evidence{UnitID: w.UnitID(), ProviderID: providerID, ProviderVersion: providerVersion,
			OriginRunID: run, NodeID: n.id, Precision: model.PrecisionSyntax}
		ev.ID = model.NewEvidenceID(ev)
		facts = append(facts, model.NodeFact{
			Node:         model.Node{ID: n.id, Kind: n.kind, Name: n.name, QualifiedName: n.key, FileID: n.file},
			CanonicalKey: key(n.key), Evidence: []model.Evidence{ev}})
	}
	if err := w.PutNodes(ctx, facts); err != nil {
		t.Fatalf("PutNodes: %v", err)
	}
	rel := func(from model.NodeID, kind model.RelationKind, to model.NodeID) model.Relation {
		return model.Relation{ID: model.NewRelationID(repo, from, kind, to), From: from, Kind: kind, To: to}
	}
	if err := w.PutRelations(ctx, fixtureRelationFacts(t, w.UnitID(), run, []model.Relation{
		rel(module, model.RelDefines, top),
		rel(top, model.RelContains, nested),
		rel(dir, model.RelContains, fileNod),
	})); err != nil {
		t.Fatalf("PutRelations: %v", err)
	}
	if err := s.SealUnit(ctx, w); err != nil {
		t.Fatalf("SealUnit: %v", err)
	}
	if _, err := s.Activate(ctx, gen, 0, model.HealthFresh, fixtureCaps, "norm-v1"); err != nil {
		t.Fatalf("Activate: %v", err)
	}

	r, err := s.PinGeneration(ctx, repo, 0, time.Minute)
	if err != nil {
		t.Fatalf("PinGeneration: %v", err)
	}
	defer r.Close()
	g, err := store.NewGraphReader(ctx, r)
	if err != nil {
		t.Fatalf("NewGraphReader: %v", err)
	}
	ask := make([]model.NodeID, 0, len(nodes))
	for _, n := range nodes {
		ask = append(ask, n.id)
	}
	refs, err := g.Resolve(ctx, ask)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	ordered := append([]graph.NodeRef(nil), refs...)
	sortRefs(ordered)
	containers, err := g.Containers(ctx, ordered)
	if err != nil {
		t.Fatalf("Containers: %v", err)
	}
	got := map[model.NodeID]model.NodeID{}
	ids, err := g.NodeIDs(ctx, containers)
	if err != nil {
		t.Fatalf("NodeIDs: %v", err)
	}
	owners, err := g.NodeIDs(ctx, ordered)
	if err != nil {
		t.Fatalf("NodeIDs(owners): %v", err)
	}
	for i, owner := range owners {
		got[owner] = ids[i]
	}
	want := map[model.NodeID]model.NodeID{
		module:  module, // a container is its own container
		top:     module, // reached by `defines`, never by `contains`
		nested:  module, // nested under a function, which is no container
		fileNod: module, // the file's own container, not the directory that holds it
		dir:     "",     // no file, no container-kind claimant
	}
	for id, wantContainer := range want {
		if got[id] != wantContainer {
			t.Fatalf("container of %s = %q, want %q (all containers: %v)", id, got[id], wantContainer, got)
		}
	}
}

func sortRefs(refs []graph.NodeRef) {
	for i := 1; i < len(refs); i++ {
		for j := i; j > 0 && refs[j] < refs[j-1]; j-- {
			refs[j], refs[j-1] = refs[j-1], refs[j]
		}
	}
}
