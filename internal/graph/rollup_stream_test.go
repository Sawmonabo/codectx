package graph

import (
	"context"
	"fmt"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
)

// newPairFixture builds a graph of `pkgs` packages in which every symbol calls
// one symbol of the NEXT package, so the rollup of `edges` call relations is
// exactly `pkgs` distinct pairs however the edges are batched. Every pair is
// therefore fed from many batches, which is what the fold has to survive.
//
// It is built directly rather than on newGraphFixture so the relation set is
// only what these two tests read: a rollup over twenty thousand edges is a
// scan per adjacency page, and the shared fixture's own edges would be scanned
// with it for nothing.
func newPairFixture(t *testing.T, edges, pkgs int) (*graphFixture, []model.Relation) {
	t.Helper()
	f := &graphFixture{
		nodes:    map[model.NodeID]model.Node{},
		evidence: map[model.RelationID][]model.Evidence{},
		binding: model.Binding{
			RepositoryID: model.RepositoryID(fixtureID("repo-1")),
			SnapshotID:   model.SnapshotID(fixtureID("snap-1")),
			GenerationID: 1,
			AnalysisKey:  model.AnalysisKey(fixtureID("akey-1")),
		},
	}
	pkgID := func(i int) model.NodeID { return fixtureNodeID(fmt.Sprintf("pkg-%03d", i)) }
	for i := 0; i < pkgs; i++ {
		name := fmt.Sprintf("pkg%03d", i)
		f.nodes[pkgID(i)] = model.Node{ID: pkgID(i), Kind: model.NodePackage, Name: name,
			QualifiedName: name, Language: "go", SemanticSource: model.SemanticCanonical}
	}
	symbol := func(name string, pkg int) model.NodeID {
		id := fixtureNodeID(name)
		f.nodes[id] = model.Node{ID: id, Kind: model.NodeFunction, Name: name,
			QualifiedName: name, Language: "go", SemanticSource: model.SemanticCanonical}
		f.relations = append(f.relations, model.Relation{ID: fixtureRelationID(len(f.relations)),
			From: pkgID(pkg), Kind: model.RelContains, To: id})
		return id
	}
	calls := make([]model.Relation, 0, edges)
	for i := 0; i < edges; i++ {
		from := symbol(fmt.Sprintf("s-from-%06d", i), i%pkgs)
		to := symbol(fmt.Sprintf("s-to-%06d", i), (i%pkgs+1)%pkgs)
		rel := model.Relation{ID: fixtureRelationID(len(f.relations)), From: from,
			Kind: model.RelCalls, To: to}
		f.relations = append(f.relations, rel)
		calls = append(calls, rel)
		// A varying number of occurrences per edge, so EvidenceCount is a real
		// sum rather than a copy of PairCount.
		for n := 0; n <= i%3; n++ {
			f.evidence[rel.ID] = append(f.evidence[rel.ID], model.Evidence{
				ID:              model.EvidenceID(fixtureID(fmt.Sprintf("ev-%06d-%d", i, n))),
				UnitID:          model.UnitID(fixtureID("unit-1")),
				ProviderID:      "treesitter",
				ProviderVersion: "0.0.1",
				OriginRunID:     model.ProviderRunID(fixtureID("run-1")),
				RelationID:      rel.ID,
				Precision:       model.PrecisionSyntax,
				FileID:          model.FileID(fixtureID("file-1")),
				Bytes:           &model.ByteRange{Start: uint64(i) * 16, End: uint64(i)*16 + 8},
			})
		}
	}
	return f, calls
}

// pairEngine is an engine over f with no spools: the pair sort spills to the
// operating system's temporary directory and removes its own files, which is
// the path an engine built without a store takes.
func pairEngine(t *testing.T, f *graphFixture) *Engine {
	t.Helper()
	limits := fixtureLimits()
	limits.MaxDepth, limits.MaxVisited, limits.MaxEdges = 0, 0, 0
	// The smallest run budget the sort accepts, so a fixture of this size
	// spills its runs to disk instead of sorting in one buffer: the invariant
	// below is about the disk-backed path, and a budget that held everything
	// would prove nothing about it.
	limits.FrontierBytes = 1
	e, err := New(Options{Adjacency: f, Limits: limits})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	return e
}

// TestPackagePairCountsAreExactAndOrderIndependent protects ruling P4's
// exactness: the pairs a streamed rollup reports -- their set, their global
// order and their two counts -- are a function of the edges alone, never of
// the batch boundaries the stream happened to fall on. The fixture's 700 edges
// cross six resolution batches, so every pair is folded from records emitted
// by several of them.
//
// Mutation proof: drop `.WithFold(foldPair)` from rankPairs' first pass and the
// answer becomes 700 one-count records instead of eight summed pairs -- both
// legs fail on the very first pair.
func TestPackagePairCountsAreExactAndOrderIndependent(t *testing.T) {
	const edges, pkgs = 700, 8
	f, calls := newPairFixture(t, edges, pkgs)
	if got := (edges + pairRollupBatch - 1) / pairRollupBatch; got < 5 {
		t.Fatalf("the fixture spans %d resolution batches; it cannot prove batch independence", got)
	}
	e := pairEngine(t, f)

	// The single-shot baseline: every edge counted once against the pair of its
	// endpoints' packages, computed here with no batching at all.
	type key struct{ from, to model.NodeID }
	want := map[key]model.PackageEdge{}
	for i, rel := range calls {
		k := key{from: fixtureNodeID(fmt.Sprintf("pkg-%03d", i%pkgs)),
			to: fixtureNodeID(fmt.Sprintf("pkg-%03d", (i%pkgs+1)%pkgs))}
		edge := want[k]
		edge.FromNodeID, edge.ToNodeID = k.from, k.to
		edge.FromPath = fmt.Sprintf("pkg%03d", i%pkgs)
		edge.ToPath = fmt.Sprintf("pkg%03d", (i%pkgs+1)%pkgs)
		edge.PairCount++
		edge.EvidenceCount += int64(len(f.evidence[rel.ID]))
		want[k] = edge
	}

	forward := make([]model.Relation, len(calls))
	copy(forward, calls)
	reversed := make([]model.Relation, 0, len(calls))
	for i := len(calls) - 1; i >= 0; i-- {
		reversed = append(reversed, calls[i])
	}
	var first []model.PackageEdge
	for _, leg := range []struct {
		name string
		in   []model.Relation
	}{{"declaration order", forward}, {"reversed order", reversed}} {
		meta := model.QueryMeta{}
		got, err := rollupSlice(e, &meta, leg.in)
		if err != nil {
			t.Fatalf("%s: rollup: %v", leg.name, err)
		}
		if len(got) != pkgs {
			t.Fatalf("%s: rolled up %d pairs, want the %d the fixture has", leg.name, len(got), pkgs)
		}
		var pairs, evidence int64
		for _, edge := range got {
			exp, ok := want[key{from: edge.FromNodeID, to: edge.ToNodeID}]
			if !ok {
				t.Fatalf("%s: rollup reported a pair the edges do not have: %s -> %s",
					leg.name, edge.FromPath, edge.ToPath)
			}
			if edge != exp {
				t.Fatalf("%s: pair %s -> %s rolled up to %+v, want the single-shot %+v",
					leg.name, edge.FromPath, edge.ToPath, edge, exp)
			}
			pairs += edge.PairCount
			evidence += edge.EvidenceCount
		}
		if pairs != edges {
			t.Fatalf("%s: the pair counts sum to %d, want one per admitted edge (%d)",
				leg.name, pairs, edges)
		}
		if evidence == 0 {
			t.Fatalf("%s: every evidence count is zero; the fold cannot be checked", leg.name)
		}
		if first == nil {
			first = got
			continue
		}
		// The SERVED ORDER too, not just the set: a global rank that depended
		// on arrival order is the defect this programme exists to remove.
		for i := range got {
			if got[i] != first[i] {
				t.Fatalf("%s: pair %d is %+v, but the other order served %+v",
					leg.name, i, got[i], first[i])
			}
		}
	}
}

// TestPackageRollupHoldsOneBatchOfEdges protects the memory invariant ruling P4
// is for: the rollup of an unbounded walk holds ONE resolution batch of edges
// at a time, never the edges the walk admitted. Twenty thousand edges stream
// through it and the live set stays at pairRollupBatch.
//
// The sort's own envelope -- one run buffer plus the merge fan-in -- is
// pagination.ExternalSort's invariant and is asserted there; what this test
// owns is the half above it, the batch the rollup itself buffers.
//
// Mutation proof: collect the endpoints whole (buffer every edge in
// pairRollup.Visit and flush once) and the peak becomes 20000.
func TestPackageRollupHoldsOneBatchOfEdges(t *testing.T) {
	const edges, pkgs = 20000, 8
	f, calls := newPairFixture(t, edges, pkgs)
	e := pairEngine(t, f)

	meta := model.QueryMeta{}
	var stats rollupStats
	run, err := e.rollupRanked(context.Background(), &meta, func(sink edgeSink) error {
		for _, rel := range calls {
			if err := sink.Visit(frontierState{}, rel); err != nil {
				return err
			}
		}
		return nil
	}, &stats)
	if err != nil {
		t.Fatalf("streamed rollup: %v", err)
	}
	defer run.Close()

	if stats.PeakLiveEdges > pairRollupBatch {
		t.Fatalf("the rollup held %d edges at once over %d admitted; the batch bound is %d",
			stats.PeakLiveEdges, edges, pairRollupBatch)
	}
	var pairs, count int64
	if err := run.Each(func(v pairRecord) error {
		pairs++
		count += v.PairCount
		return nil
	}); err != nil {
		t.Fatalf("read the ranked pairs: %v", err)
	}
	if pairs != pkgs || count != edges {
		t.Fatalf("the ranked run holds %d pairs summing to %d edges, want %d pairs summing to %d",
			pairs, count, pkgs, edges)
	}
}

// rollupSlice drains the whole ranked pair run into a slice. It is a TEST-only
// adapter: production serves the run one page at a time and never materializes
// it, so the assertions below compare a whole-answer baseline the served pages
// are then checked against elsewhere.
func rollupSlice(e *Engine, meta *model.QueryMeta, relations []model.Relation) ([]model.PackageEdge, error) {
	run, err := e.rollupRanked(context.Background(), meta, func(sink edgeSink) error {
		for _, r := range relations {
			if err := sink.Visit(frontierState{}, r); err != nil {
				return err
			}
		}
		return nil
	}, nil)
	if err != nil {
		return nil, err
	}
	defer run.Close()
	var out []model.PackageEdge
	if err := run.Each(func(v pairRecord) error {
		out = append(out, v.edge())
		return nil
	}); err != nil {
		return nil, err
	}
	return out, nil
}
