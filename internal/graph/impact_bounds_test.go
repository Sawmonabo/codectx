package graph

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
)

// TestImpactHoldsOneEnvelopeWhateverTheWalkAdmits is the structural memory
// proof rulings P2/P4 exist for: an impact answer over a walk of twenty
// thousand and then forty thousand reachable nodes holds the SAME constant
// working set, and that constant is a function of the sort's run budget, the
// merge fan-in, the served page and the rollup's resolution batch -- never of
// how many nodes the walk admitted.
//
// The envelope is `(run budget + fan-in) + one page + pairRollupBatch`. The
// three terms are separate because the structures are: the two ranking sorts
// run SEQUENTIALLY (rankImpact closes pass 1 before pass 2 opens, and finishes
// before rankPairs starts), so the sorts contribute their MAXIMUM; the rollup's
// batch is held above them while the walk streams; and the page is the
// `make([]T, 0, limit)` servePage allocates. The run-budget term is derived
// from pagination.SortRunBytes and the production charge (sizeOfImpactRecord /
// sizeOfPairRecord) of the SMALLEST record this fixture produces, so it is read
// from the design rather than restated as a literal -- and it is asserted to be
// well under the smaller fixture, without which "the same constant at both
// sizes" would be true of any number large enough to hold everything.
//
// Mutation proof (d), whole-slice restored: delete the `if len(p.batch) <
// pairRollupBatch { return nil }` early return from pairRollup.Visit so the
// sink buffers every admitted edge and flushes once. The rollup's live set
// becomes the walk (20000, then 40000) and both sizes fail the envelope.
func TestImpactHoldsOneEnvelopeWhateverTheWalkAdmits(t *testing.T) {
	// The mid fan-out is the frontier's widest INTERNAL level; the leaves are
	// admitted and expanded but have no edges of their own.
	const fanOut = 200
	sizes := []int{20000, 40000}

	var envelope int
	for _, reachable := range sizes {
		t.Run(fmt.Sprintf("%d reachable nodes", reachable), func(t *testing.T) {
			f := newReachableFixture(t, reachable/fanOut, fanOut)
			e, probe := probedImpactEngine(t, f)

			// The envelope, derived once from the design constants and the
			// engine's own limits. It is identical at both sizes because every
			// term is: SortRunBytes reads the query memory admission, the
			// fan-in and the rollup batch are package constants, and the page
			// bound is the configured one.
			// The run buffer is charged in BYTES, so the most records it can
			// hold is its budget over the cheapest record either ranking puts
			// in it -- the entity sort and the pair sort share the budget and
			// the pair record is the smaller of the two.
			charge := min(sizeOfImpactRecord(smallestFixtureImpactRecord()),
				sizeOfPairRecord(smallestFixturePairRecord()))
			runRecords := int(pagination.SortRunBytes(e.limits.FrontierBytes)/charge) + 1
			bound := runRecords + pagination.MaxSortFanIn + e.limits.MaxPageItems + pairRollupBatch
			if bound >= sizes[0] {
				t.Fatalf("the envelope is %d records over a %d-node walk: a bound that can hold the "+
					"whole answer proves nothing about boundedness", bound, sizes[0])
			}
			if envelope == 0 {
				envelope = bound
			} else if bound != envelope {
				t.Fatalf("the envelope moved with the fixture size: %d here, %d at %d nodes",
					bound, envelope, sizes[0])
			}

			res, err := e.Impact(context.Background(), model.ImpactRequest{
				GenerationID: 1, Start: []model.NodeID{fixtureNodeID("bound-seed")},
				Direction: model.DirectionOutgoing,
				Relations: []model.RelationKind{model.RelCalls}})
			if err != nil {
				t.Fatalf("impact over %d reachable nodes: %v", reachable, err)
			}
			// Non-vacuity: the walk really did admit the whole fixture. A
			// frontier or budget stop that quietly shrank it would leave the
			// peak assertion below asserting boundedness over a walk of a few
			// hundred nodes.
			if res.Meta.Truncated {
				t.Fatalf("the walk over %d reachable nodes was cut short (%q); the bound below would "+
					"be asserted over a shrunken answer", reachable, res.Meta.TruncationReason)
			}
			if res.VisitedCount < int64(reachable) {
				t.Fatalf("the walk admitted %d nodes, want the fixture's %d",
					res.VisitedCount, reachable)
			}
			if len(res.Entries) != e.limits.MaxPageItems {
				t.Fatalf("the first page served %d entries, want the page bound %d",
					len(res.Entries), e.limits.MaxPageItems)
			}

			// The sorts are sequential, so the run-budget term covers their
			// maximum; the rollup batch and the page are held alongside.
			sorts := max(probe.Rank.PeakLiveRecords, probe.Pairs.PeakLiveRecords)
			held := sorts + probe.Rollup.PeakLiveEdges + len(res.Entries)
			if held > envelope {
				t.Fatalf("an impact answer over %d reachable nodes held %d records at once "+
					"(sorts %d, rollup %d, page %d); the envelope is %d",
					reachable, held, sorts, probe.Rollup.PeakLiveEdges, len(res.Entries), envelope)
			}
			// The peak must also not be the answer itself: a sort that never
			// spilled would pass the bound only because the bound is generous.
			if probe.Rank.PeakLiveRecords >= reachable {
				t.Fatalf("the entity ranking held %d of %d records: it never spilled a run",
					probe.Rank.PeakLiveRecords, reachable)
			}
		})
	}
}

// smallestFixtureImpactRecord is the CHEAPEST record newReachableFixture's walk
// produces: a depth-one node, whose route is one relation. Charging the run
// budget against the smallest record is what makes the derived record count an
// upper bound on how many the buffer can hold.
func smallestFixtureImpactRecord() impactRecord {
	return impactRecord{
		NodeID: fixtureNodeID("bound-mid-000000"),
		Depth:  1, Cost: 1, Direction: model.DirectionOutgoing,
		Via:     fixtureRelationID(1),
		Parent:  fixtureNodeID("bound-seed"),
		Route:   []model.RelationID{fixtureRelationID(1)},
		Reasons: []string{string(model.RelCalls)},
	}
}

// smallestFixturePairRecord is the cheapest pair record the same walk produces:
// the rollup's labels are the fixture's package names, and the two node ids are
// fixed width.
func smallestFixturePairRecord() pairRecord {
	return pairRecord{
		FromNodeID: fixtureNodeID("bound-pkg-00"), ToNodeID: fixtureNodeID("bound-pkg-01"),
		FromPath: "bound-pkg-00", ToPath: "bound-pkg-01", PairCount: 1, EvidenceCount: 1,
	}
}

// probedImpactEngine builds an engine over f with the memory probe attached and
// every scale bound unlimited, so nothing but the structures under test bounds
// what the answer holds.
func probedImpactEngine(t *testing.T, f *graphFixture) (*Engine, *heapProbe) {
	t.Helper()
	signer, err := pagination.OpenSigner(t.TempDir())
	if err != nil {
		t.Fatalf("open signer: %v", err)
	}
	store := newFixtureLeases()
	spools, err := pagination.NewSpools(t.TempDir(), 512<<20, store)
	if err != nil {
		t.Fatalf("new spools: %v", err)
	}
	limits := fixtureLimits()
	limits.MaxDepth, limits.MaxVisited, limits.MaxEdges = 0, 0, 0
	limits.MaxPageItems = 200
	limits.QueryTimeout = 10 * time.Minute
	// The query memory admission, and with it the sorts' run budget (a quarter
	// of it, pagination.SortRunBytes). It is set well ABOVE what the widest
	// frontier of either fixture needs -- a frontier the ceiling cut would
	// shrink the walk, and the answer this proof is asserted over would no
	// longer be the whole one -- and far below what would let either ranking
	// hold its records in one run buffer.
	limits.FrontierBytes = 8 << 20
	e, err := New(Options{Adjacency: f, Signer: signer, Spools: spools,
		Leases: pagination.NewLeases(store, limits.CursorTTL), Limits: limits})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	probe := &heapProbe{}
	e.probe = probe
	return e, probe
}

// newReachableFixture builds a two-level fan-out from one seed: mids nodes at
// depth one, each calling fanOut leaves of its own, so the walk admits
// mids*(1+fanOut) nodes and its widest INTERNAL frontier is mids. Every node is
// contained by one of eight packages, so the answer's rollup is real work
// rather than an empty pass.
//
// The containment edges are declared FIRST so that every call edge sorts after
// them: the fixture's keyset is the declaration counter, and a walk paging
// through the calls then skips the containment rows on the id comparison alone.
func newReachableFixture(t *testing.T, mids, fanOut int) *graphFixture {
	t.Helper()
	const pkgs = 8
	f := &graphFixture{
		nodes:    make(map[model.NodeID]model.Node, mids*(fanOut+1)+pkgs+1),
		evidence: map[model.RelationID][]model.Evidence{},
		binding: model.Binding{
			RepositoryID: model.RepositoryID(fixtureID("repo-1")),
			SnapshotID:   model.SnapshotID(fixtureID("snap-1")),
			GenerationID: 1,
			AnalysisKey:  model.AnalysisKey(fixtureID("akey-1")),
		},
	}
	add := func(name string, kind model.NodeKind) model.NodeID {
		id := fixtureNodeID(name)
		f.nodes[id] = model.Node{ID: id, Kind: kind, Name: name, QualifiedName: name,
			Language: "go", SemanticSource: model.SemanticCanonical}
		return id
	}
	pkgID := make([]model.NodeID, pkgs)
	for i := range pkgID {
		pkgID[i] = add(fmt.Sprintf("bound-pkg-%02d", i), model.NodePackage)
	}
	seed := add("bound-seed", model.NodeFunction)
	mid := make([]model.NodeID, mids)
	for i := range mid {
		mid[i] = add(fmt.Sprintf("bound-mid-%06d", i), model.NodeFunction)
	}
	leaf := make([]model.NodeID, 0, mids*fanOut)
	for i := 0; i < mids*fanOut; i++ {
		leaf = append(leaf, add(fmt.Sprintf("bound-leaf-%08d", i), model.NodeFunction))
	}
	contained := func(owner int, id model.NodeID) {
		f.relations = append(f.relations, model.Relation{ID: fixtureRelationID(len(f.relations)),
			From: pkgID[owner%pkgs], Kind: model.RelContains, To: id})
	}
	contained(0, seed)
	for i, id := range mid {
		contained(i+1, id)
	}
	for i, id := range leaf {
		contained(i+2, id)
	}
	calls := func(from, to model.NodeID) {
		f.relations = append(f.relations, model.Relation{ID: fixtureRelationID(len(f.relations)),
			From: from, Kind: model.RelCalls, To: to})
	}
	for _, id := range mid {
		calls(seed, id)
	}
	for i, id := range leaf {
		calls(mid[i/fanOut], id)
	}
	return f
}
