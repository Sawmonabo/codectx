package graph

import (
	"context"
	"fmt"
	"runtime"
	"runtime/debug"
	"testing"
	"time"

	"github.com/Sawmonabo/codectx/internal/config"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
)

// scaleFrontierBytes is the ceiling the measured walks run under: the shipped
// default of resources.query_memory_bytes, which app/query.go carries into
// Limits.FrontierBytes. The guard below is stated as a multiple of it, so a
// configuration change moves the bound with the ceiling instead of silently
// loosening it.
var scaleFrontierBytes = config.Defaults().Resources.QueryMemoryBytes

// scaleAllowance is everything a walk's heap holds that the 4 x FrontierBytes
// term does not, DERIVED from the 100 000-wide measurement below and fixed
// here.
//
// Derivation. The 100 000-wide walk peaks 112 MiB above its fixture (the
// t.Logf line this test prints). Its level is ~21 MiB of encoded records, so it
// never reaches the 32 MiB ceiling and the level pipeline is nowhere near the
// 4 x 32 MiB the ceiling term already allows: those 112 MiB are therefore the
// envelope of everything a walk holds that the level cannot move -- one
// frontier chunk of 65 536 states, the two paged bitsets' resident pages, the
// bounded route-name LRU, one levelResolveBatch of records being named, one
// delivered page, the external sorts' merge readers, AND the collector float
// the sampler cannot tell from live data (HeapAlloc is read without forcing a
// collection, so at GOGC 20 a fixture of F bytes floats up to F/5 of garbage
// into every sample; that term grows with the FIXTURE, not with the level, and
// is why the two measured figures are 112 MiB and 192 MiB rather than equal).
// 128 MiB is that 112 MiB rounded up to the next whole multiple of
// FrontierBytes, which is the unit every other term here is stated in.
const scaleAllowance = 128 << 20

// TestAWalkHoldsAPageNotTheLevel is ADR-0005 Decision 2's scale claim,
// measured: the heap a walk holds is a function of its BOUNDS and not of the
// widest level it crosses. It is what the level pipeline -- collect, spill,
// external sort, serve by offset -- and the paged bitsets were built for.
//
// The proof is the PAIR. One absolute number proves nothing about scaling, so
// the same walk is measured over a level of 100 000 and over a level of
// 1 000 000, ten times wider, and the two heap figures must be within 2x of
// each other. A structure that grew with the level would be an order of
// magnitude apart; the measured pair is 112 MiB and 192 MiB, a factor of 1.7
// for a factor of 10 in width, and the gap is the collector float scaleAllowance
// accounts for. The 4 x FrontierBytes + scaleAllowance ceiling on the 1 000 000
// figure is the absolute runaway guard beside it: four sorts and collectors may
// each hold their ceiling at once, and nothing else in the walk has a size the
// level can move.
//
// Both walks are also asserted COMPLETE -- every node visited, every edge read
// -- because a walk that stopped early would hold a small heap for the most
// uninteresting of reasons.
//
// The failure mode this protects: a level held whole in heap. That is the
// structure every earlier shape of this walk had (a level's edges as
// model.Relation rows, a cumulative sorted-run set merge-joined per level, a
// per-level ref -> canonical id table), and on a repository whose hub fans out
// to a million nodes it is the difference between a 32 MiB page and the
// generation itself.
//
// Mutation proof: remove the collector's spill (levelCollector.add keeps every
// record resident) and the 1 000 000-wide walk holds its level, breaking both
// the ratio and the ceiling.
func TestAWalkHoldsAPageNotTheLevel(t *testing.T) {
	if testing.Short() {
		t.Skip("the million-edge fixtures are built in memory")
	}
	small := measureWideLevel(t, 100_000)
	// The first fixture is unreachable here and the second is about to be
	// built beside it: without this the two live at once.
	runtime.GC()
	large := measureWideLevel(t, 1_000_000)

	if large > 2*small || small > 2*large {
		t.Fatalf("a level ten times wider moved the walk's heap from %d MiB to %d MiB: "+
			"the walk is holding something that scales with the level, not with its bounds",
			small>>20, large>>20)
	}
	ceiling := int64(4*scaleFrontierBytes + scaleAllowance)
	if large > ceiling {
		t.Fatalf("the 1 000 000-wide walk peaked %d MiB above its fixture, over the %d MiB "+
			"ceiling (4 x FrontierBytes %d MiB + allowance %d MiB)",
			large>>20, ceiling>>20, scaleFrontierBytes>>20, int64(scaleAllowance)>>20)
	}
}

// measureWideLevel walks a generation whose level 1 is exactly width nodes and
// reports the bytes of heap the walk itself held above that generation. It
// fails the test unless the walk completed over every node and every edge: a
// walk that stopped early would report a small heap for the most uninteresting
// of reasons.
//
// The fixture is built inside this call and the slices it was built from are
// dropped before the baseline is taken, so what is reported is the walk's own
// high-water mark and not the generation it reads.
func measureWideLevel(t *testing.T, width int) int64 {
	t.Helper()
	nodes, rels := fanFixture(width)
	nodeCount, edgeCount := int64(len(nodes)), int64(len(rels))
	seed := nodes[0].ID
	binding := model.Binding{RepositoryID: model.RepositoryID(hexID(1)),
		SnapshotID: model.SnapshotID(hexID(2)), GenerationID: 1,
		AnalysisKey: model.AnalysisKey(hexID(3))}
	reader := NewMemoryGraph(binding, nodes, rels)
	nodes, rels = nil, nil
	e := scaleEngine(t, reader)

	// A sample of HeapAlloc counts garbage the collector has not reached yet.
	// Against a fixture this size the default pacing lets that reach the same
	// order as the live set, which would report the collector's headroom as the
	// walk's footprint. A low GOGC makes the heap track what is LIVE, which is
	// what this measures.
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
		GenerationID: 1, Start: []model.NodeID{seed},
		Direction: model.DirectionOutgoing,
		Relations: []model.RelationKind{model.RelCalls},
		Page:      model.PageRequest{Limit: 50},
	})
	wall := time.Since(start)
	close(stop)
	high := <-peak
	if err != nil {
		t.Fatalf("impact over a %d-wide level: %v", width, err)
	}

	above := int64(high) - int64(before.HeapAlloc)
	t.Logf("level width %d: wall %s; visited %d, edges %d; heap baseline %d MiB, peak %d MiB, above baseline %d MiB",
		width, wall.Round(time.Millisecond), res.VisitedCount, res.EdgeCount,
		before.HeapAlloc>>20, high>>20, above>>20)

	if res.VisitedCount != nodeCount {
		t.Fatalf("the %d-wide walk visited %d nodes, want every one of the %d the generation carries",
			width, res.VisitedCount, nodeCount)
	}
	if res.EdgeCount != edgeCount {
		t.Fatalf("the %d-wide walk read %d edges, want the %d the generation carries",
			width, res.EdgeCount, edgeCount)
	}
	return above
}

// fanFixture builds a seed and the width children one `calls` relation each
// reaches, so the walk crosses exactly one level of exactly width nodes.
func fanFixture(width int) ([]model.Node, []model.Relation) {
	total := 1 + width
	nodes := make([]model.Node, 0, total)
	rels := make([]model.Relation, 0, width)
	id := func(i int) model.NodeID { return model.NodeID(hexID(i)) }
	for i := 0; i < total; i++ {
		name := fmt.Sprintf("s%07d", i)
		nodes = append(nodes, model.Node{ID: id(i), Kind: model.NodeFunction, Name: name,
			QualifiedName: name, Language: "go", SemanticSource: model.SemanticCanonical})
	}
	for c := 1; c <= width; c++ {
		rels = append(rels, model.Relation{ID: model.RelationID(hexID(len(rels))),
			From: id(0), Kind: model.RelCalls, To: id(c)})
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
		Adjacency: reader.(*MemoryGraph),
		Reader:    reader,
		Signer:    signer,
		Spools:    spools,
		Leases:    pagination.NewLeases(store, time.Hour),
		Limits: Limits{
			MaxPageItems:   model.MaxPageItems,
			MaxReasonPaths: 1,
			QueryTimeout:   0,
			CursorTTL:      time.Hour,
			FrontierBytes:  scaleFrontierBytes,
		},
	})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	return e
}
