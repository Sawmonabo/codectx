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

// nameFixture is a store holding one file with two functions, Foo with its
// name token's range and Bar without one, and Bar's call to Foo, activated.
type nameFixture struct {
	s        *store.Store
	repo     model.RepositoryID
	foo, bar model.NodeID
	// fooBytes and fooName are Foo's stored range and name token range.
	fooBytes, fooName model.ByteRange
	barBytes          model.ByteRange
}

func newNameFixture(t *testing.T) nameFixture {
	t.Helper()
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
	const src = "package a\n\nfunc Foo() {}\n\nfunc Bar() { Foo() }\n"
	ff := putFixtureFile(t, s, repo, graphtest.FixtureFilePath, src)
	snap := putFixtureSnapshot(t, s, repo, ff, len(src))
	gen, err := s.BeginGeneration(ctx, repo, snap.ID, model.H("semantic-names"), "main")
	if err != nil {
		t.Fatalf("BeginGeneration: %v", err)
	}
	run, err := s.BeginProviderRun(ctx, gen, providerID, providerVersion)
	if err != nil {
		t.Fatalf("BeginProviderRun: %v", err)
	}
	w := beginFixtureUnit(t, s, gen, run, ff, "names")

	span := func(text string) model.ByteRange {
		i := strings.Index(src, text)
		return model.ByteRange{Start: uint64(i), End: uint64(i + len(text))}
	}
	// Every fixture byte sits on one line as far as a stored fact is
	// concerned: storage keeps bytes only, so the line is not under test.
	located := func(r model.ByteRange) *model.SourceRange {
		return &model.SourceRange{Start: model.Position{Byte: r.Start, Line: 1}, End: model.Position{Byte: r.End, Line: 1}}
	}
	f := nameFixture{s: s, repo: repo, fooBytes: span("func Foo() {}"), barBytes: span("func Bar() { Foo() }")}
	f.fooName = model.ByteRange{Start: f.fooBytes.Start + uint64(len("func ")), End: f.fooBytes.Start + uint64(len("func Foo"))}
	fooKey, barKey := graphtest.NodeKey("pkg/a#Foo"), graphtest.NodeKey("pkg/a#Bar")
	f.foo = model.NewNodeID(repo, model.NodeFunction, fooKey)
	f.bar = model.NewNodeID(repo, model.NodeFunction, barKey)
	nodes := []struct {
		n   model.Node
		key string
	}{
		{model.Node{ID: f.foo, Kind: model.NodeFunction, Name: "Foo", FileID: ff.id, ContentHash: ff.hash,
			Range: located(f.fooBytes), NameRange: located(f.fooName)}, fooKey},
		{model.Node{ID: f.bar, Kind: model.NodeFunction, Name: "Bar", FileID: ff.id, ContentHash: ff.hash,
			Range: located(f.barBytes)}, barKey},
	}
	var facts []model.NodeFact
	for _, n := range nodes {
		ev := model.Evidence{UnitID: w.UnitID(), ProviderID: providerID, ProviderVersion: providerVersion,
			OriginRunID: run, NodeID: n.n.ID, Precision: model.PrecisionSyntax}
		ev.ID = model.NewEvidenceID(ev)
		facts = append(facts, model.NodeFact{Node: n.n, CanonicalKey: n.key, Evidence: []model.Evidence{ev}})
	}
	if err := w.PutNodes(ctx, facts); err != nil {
		t.Fatalf("PutNodes: %v", err)
	}
	call := model.Relation{ID: model.NewRelationID(repo, f.bar, model.RelCalls, f.foo), From: f.bar, Kind: model.RelCalls, To: f.foo}
	if err := w.PutRelations(ctx, fixtureRelationFacts(t, w.UnitID(), run, []model.Relation{call})); err != nil {
		t.Fatalf("PutRelations: %v", err)
	}
	if err := s.SealUnit(ctx, w); err != nil {
		t.Fatalf("SealUnit: %v", err)
	}
	if _, err := s.Activate(ctx, gen, 0, model.HealthFresh, fixtureCaps, "norm-v1"); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	return f
}

// TestNodeLocationsServeTheStoredNameRange asserts that a node written with a
// name range reads back through NodeLocations with that exact range, and a
// node written without one reads back with none.
//
// Mutations it catches: the writer not binding the name columns (Foo reads
// back absent); the reader serving a NULL or zero name range as present (Bar
// reads back with one); NodeLocations dropping the stored name range.
func TestNodeLocationsServeTheStoredNameRange(t *testing.T) {
	f := newNameFixture(t)
	g := openGraphReader(t, f.s, f.repo)
	loc, ok := g.(graph.SourceLocator)
	if !ok {
		t.Fatal("the store's graph reader does not locate nodes")
	}
	got, err := loc.NodeLocations(context.Background(), []model.NodeID{f.foo, f.bar})
	if err != nil {
		t.Fatalf("NodeLocations: %v", err)
	}
	byID := map[model.NodeID]model.NodeLocation{}
	for _, l := range got {
		byID[l.NodeID] = l
	}
	foo, ok := byID[f.foo]
	if !ok {
		t.Fatal("Foo has no location")
	}
	if foo.Bytes != f.fooBytes || foo.NameBytes == nil || *foo.NameBytes != f.fooName {
		t.Errorf("Foo located at %+v name %v, want %+v name %+v", foo.Bytes, foo.NameBytes, f.fooBytes, f.fooName)
	}
	bar, ok := byID[f.bar]
	if !ok {
		t.Fatal("Bar has no location")
	}
	if bar.Bytes != f.barBytes || bar.NameBytes != nil {
		t.Errorf("Bar located at %+v name %v, want %+v and no name range", bar.Bytes, bar.NameBytes, f.barBytes)
	}
}

// TestCallersAnswerLocatesTheCalleeName asserts that a callers answer over
// the store carries, for the callee declaration, its name token's range
// inside its own range, so a call hierarchy item has its selection range
// from the one answer.
//
// Mutations it catches: the graph engine's location step rebuilding a
// location without NameBytes; the store's NodeLocations not serving it.
func TestCallersAnswerLocatesTheCalleeName(t *testing.T) {
	f := newNameFixture(t)
	g := openGraphReader(t, f.s, f.repo)
	adj, ok := g.(graph.Adjacency)
	if !ok {
		t.Fatal("the store's graph reader does not hydrate nodes")
	}
	e, err := graph.New(graph.Options{Adjacency: adj, Reader: g, Limits: graph.Limits{
		MaxDepth: 3, MaxVisited: 1000, MaxEdges: 1000, MaxPageItems: 200, MaxReasonPaths: 3,
		QueryTimeout: 10 * time.Second, CursorTTL: time.Minute, FrontierBytes: 1 << 20,
	}})
	if err != nil {
		t.Fatalf("graph.New: %v", err)
	}
	res, err := e.Neighbors(context.Background(), model.GraphRequest{Start: []model.NodeID{f.foo},
		Direction: model.DirectionIncoming, Relations: []model.RelationKind{model.RelCalls}})
	if err != nil {
		t.Fatalf("callers: %v", err)
	}
	if err := res.Validate(); err != nil {
		t.Fatalf("callers answered an invalid result: %v", err)
	}
	var found bool
	for _, l := range res.Locations {
		if l.NodeID != f.foo {
			continue
		}
		found = true
		if l.NameBytes == nil || *l.NameBytes != f.fooName {
			t.Errorf("Foo's location carries name %v, want %+v", l.NameBytes, f.fooName)
		} else if l.NameBytes.Start < l.Bytes.Start || l.NameBytes.End > l.Bytes.End {
			t.Errorf("Foo's name %+v lies outside its range %+v", *l.NameBytes, l.Bytes)
		}
	}
	if !found {
		t.Fatalf("the callers answer carries no location for Foo: %+v", res.Locations)
	}
}
