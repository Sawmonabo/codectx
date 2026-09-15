package graph

import (
	"context"
	"fmt"
	"runtime"
	"runtime/debug"
	"testing"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
)

// TestAMillionNodeWalkHoldsAPageNotTheGraph is ADR-0005 Decision 2's scale
// claim, measured: peak heap over a walk is NEVER the reachable set. It is
// what the surrogate walk and the paged bitset were built for, and the number
// it logs is the lane's reported figure.
//
// MEASURED, and the test says so rather than claiming more: at a 32 MiB
// frontier ceiling this walk peaks about 250 MiB above the fixture, and
// quartering the ceiling does NOT quarter that. Two structures still scale
// with the WIDEST LEVEL rather than with the page, and both are named here so
// the next lane does not have to rediscover them:
//
//   - the frontier records themselves. Limits.FrontierBytes bounds the EDGE
//     rows one scan collects; the frontier those rows admit is a separate
//     slice with no ceiling of its own, and a 900 000-node level is 900 000
//     records held at once.
//   - levelNames over those records' ROUTES. A record carries its whole path
//     in canonical form (ADR-0005 Decision 2's content-derived ranking), so
//     each level resolves the deduplicated union of its frontier's route
//     relations -- at depth d that is up to d ids per record, and the ids are
//     64 hex characters each.
//
// The bound asserted below is therefore the one this build actually keeps, and
// the lane reports the gap to the 64 MiB target as an open item rather than
// asserting a number it does not meet.
//
// The shape is one seed over a hub of 100 000 children, each with nine of its
// own -- 1 000 001 nodes and 1 000 000 edges over three levels. A level of
// 100 000 is what makes the measurement mean anything: the old walk held that
// level's edges as model.Relation rows with four 64-hex ids each, and its
// cumulative set as sorted runs it merge-joined the whole of per level.
//
// The baseline is taken AFTER the fixture is built, so what is reported is the
// walk's own high-water mark and not the generation it reads.
//
// Mutation proof: give levelNames.fill a map that is not rebuilt per level
// (`if n.nodes == nil` around the two makes) and the walk's peak rises with
// every level instead of staying flat.
func TestAMillionNodeWalkHoldsAPageNotTheGraph(t *testing.T) {
	if testing.Short() {
		t.Skip("the million-node fixture is built in memory")
	}
	const (
		hub      = 100_000
		perChild = 9
	)
	nodes, rels := fanFixture(hub, perChild)
	t.Logf("fixture: %d nodes, %d edges", len(nodes), len(rels))

	binding := model.Binding{RepositoryID: model.RepositoryID(hexID(1)),
		SnapshotID: model.SnapshotID(hexID(2)), GenerationID: 1,
		AnalysisKey: model.AnalysisKey(hexID(3))}
	reader := NewMemoryGraph(binding, nodes, rels)
	e := scaleEngine(t, reader)

	// The heap is sampled, and a sample of HeapAlloc counts garbage the
	// collector has not reached yet. Against a 700 MiB fixture the default
	// pacing lets that reach the same order as the live set, which would report
	// the collector's headroom as the walk's footprint. A low GOGC makes the
	// heap track what is LIVE, which is what this measures.
	restore := debug.SetGCPercent(20)
	defer debug.SetGCPercent(restore)
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	stop := make(chan struct{})
	peak := make(chan uint64, 1)
	go func() {
		var high uint64
		var m runtime.MemStats
		for {
			select {
			case <-stop:
				peak <- high
				return
			default:
			}
			runtime.ReadMemStats(&m)
			if m.HeapAlloc > high {
				high = m.HeapAlloc
			}
			time.Sleep(time.Millisecond)
		}
	}()

	start := time.Now()
	res, err := e.Impact(context.Background(), model.ImpactRequest{
		GenerationID: 1, Start: []model.NodeID{nodes[0].ID},
		Direction: model.DirectionOutgoing,
		Relations: []model.RelationKind{model.RelCalls},
		Page:      model.PageRequest{Limit: 50},
	})
	wall := time.Since(start)
	close(stop)
	high := <-peak
	if err != nil {
		t.Fatalf("impact over a million-node generation: %v", err)
	}

	above := int64(high) - int64(before.HeapAlloc)
	t.Logf("walk wall %s; visited %d, edges %d; heap baseline %d MiB, peak %d MiB, above baseline %d MiB",
		wall.Round(time.Millisecond), res.VisitedCount, res.EdgeCount,
		before.HeapAlloc>>20, high>>20, above>>20)

	if res.VisitedCount != int64(len(nodes)) {
		t.Fatalf("the walk visited %d nodes, want every one of the %d the generation carries",
			res.VisitedCount, len(nodes))
	}
	if res.EdgeCount != int64(len(rels)) {
		t.Fatalf("the walk read %d edges, want the %d the generation carries",
			res.EdgeCount, len(rels))
	}
	const budget = 512 << 20
	if above > budget {
		t.Fatalf("the walk's peak heap was %d MiB above the fixture; the level bound and the "+
			"page cache are the only structures it may grow with, so it must stay under %d MiB",
			above>>20, budget>>20)
	}
}

// fanFixture builds the seed, its hub of children and their children, with one
// `calls` relation per parent-child pair.
func fanFixture(hub, perChild int) ([]model.Node, []model.Relation) {
	total := 1 + hub + hub*perChild
	nodes := make([]model.Node, 0, total)
	rels := make([]model.Relation, 0, total-1)
	id := func(i int) model.NodeID { return model.NodeID(hexID(i)) }
	for i := 0; i < total; i++ {
		name := fmt.Sprintf("s%07d", i)
		nodes = append(nodes, model.Node{ID: id(i), Kind: model.NodeFunction, Name: name,
			QualifiedName: name, Language: "go", SemanticSource: model.SemanticCanonical})
	}
	edge := func(from, to int) {
		rels = append(rels, model.Relation{ID: model.RelationID(hexID(len(rels))),
			From: id(from), Kind: model.RelCalls, To: id(to)})
	}
	for c := 1; c <= hub; c++ {
		edge(0, c)
		for g := 0; g < perChild; g++ {
			edge(c, 1+hub+(c-1)*perChild+g)
		}
	}
	return nodes, rels
}

// hexID renders i as a canonical 64-hex identifier. The fixture needs a million
// distinct well-formed ids and nothing about their CONTENT, so they are
// counted rather than hashed.
func hexID(i int) string { return fmt.Sprintf("%064x", i) }

// scaleEngine wires the continuation machinery the walk needs -- a signer, a
// spool area and a lease table -- exactly as internal/app does, because a walk
// that cannot retain its state cannot keep a bitset either.
func scaleEngine(t *testing.T, reader GraphReader) *Engine {
	t.Helper()
	dir := t.TempDir()
	signer, err := pagination.OpenSigner(dir)
	if err != nil {
		t.Fatalf("open signer: %v", err)
	}
	store := newFixtureLeases()
	spools, err := pagination.NewSpools(dir, 4<<30, store)
	if err != nil {
		t.Fatalf("new spools: %v", err)
	}
	e, err := New(Options{
		Adjacency: memAdjacency{MemoryGraph: reader.(*MemoryGraph)},
		Reader:    reader,
		Signer:    signer,
		Spools:    spools,
		Leases:    pagination.NewLeases(store, time.Hour),
		Limits: Limits{
			MaxPageItems:   model.MaxPageItems,
			MaxReasonPaths: 1,
			QueryTimeout:   0,
			CursorTTL:      time.Hour,
			FrontierBytes:  32 << 20,
		},
	})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	return e
}

// memAdjacency is the DELIVERY half of the port, which the walk no longer
// reads structure through: New still requires an Adjacency, and hydration is
// all this fixture asks of it.
type memAdjacency struct{ *MemoryGraph }

func (memAdjacency) Edges(context.Context, []model.NodeID, model.Direction,
	[]model.RelationKind, model.RelationID, int) ([]model.Relation, error) {
	return nil, nil
}
