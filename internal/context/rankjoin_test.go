package context

import (
	"cmp"
	"context"
	"fmt"
	"reflect"
	"sort"
	"testing"

	"github.com/Sawmonabo/codectx/internal/graph"

	"github.com/Sawmonabo/codectx/internal/config"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
	"github.com/Sawmonabo/codectx/internal/storage/sqlite"
)

// evidenceFixture serves the evidence rows of each relation exactly as the
// pinned reader would join them, so P-C's precision read runs with no store
// behind it. calls counts the batches it was asked for: a parity proof that
// never reached the read proves nothing, and the package's own compile fixtures
// never reach it.
type evidenceFixture struct {
	rows  map[model.RelationID][]sqlite.StoredEvidence
	calls int
}

func (e *evidenceFixture) EvidenceBatch(_ context.Context, relations []model.RelationID,
	_ int) (map[model.RelationID][]sqlite.StoredEvidence, error) {
	e.calls++
	out := map[model.RelationID][]sqlite.StoredEvidence{}
	for _, id := range relations {
		if rows, ok := e.rows[id]; ok {
			out[id] = rows
		}
	}
	return out, nil
}

// spoolCandidates is the L1 output these passes consume, built here so P-C can
// be proved before P-A and P-B land: the candidate run in ingest order and the
// hop run in route order.
func spoolCandidates(t *testing.T, s *compileSorts, cands []candidate) (*pagination.SortedRun[candRec], *pagination.SortedRun[hopRec]) {
	t.Helper()
	candSort, err := newSort[candRec](s, "test-cand", func(a, b candRec) int { return cmpInt(a.Seq, b.Seq) }, sizeOfCand)
	if err != nil {
		t.Fatalf("candidate sort: %v", err)
	}
	hopSort, err := newSort[hopRec](s, "test-hop", lessHopSeq, sizeOfHop)
	if err != nil {
		t.Fatalf("hop sort: %v", err)
	}
	for i, c := range cands {
		seq := int64(i)
		if err := candSort.Add(candRecOf(c, seq)); err != nil {
			t.Fatalf("add candidate: %v", err)
		}
		for pi, p := range c.Paths {
			for hi, rel := range p.Relations {
				if err := hopSort.Add(hopRec{RelationID: rel, Seq: seq,
					PathIdx: int32(pi), HopIdx: int32(hi)}); err != nil {
					t.Fatalf("add hop: %v", err)
				}
			}
		}
	}
	candRun, err := candSort.Sorted()
	if err != nil {
		t.Fatalf("candidate run: %v", err)
	}
	hopRun, err := hopSort.Sorted()
	if err != nil {
		t.Fatalf("hop run: %v", err)
	}
	return trackRun(s, candRun), trackRun(s, hopRun)
}

func testCompiler(pageItems int, edgeBudget config.Limit) *Compiler {
	cfg := config.Config{}
	cfg.Resources.MaxPageItems = pageItems
	cfg.Context.MaxGraphEdges = edgeBudget
	return &Compiler{cfg: cfg}
}

// relationFixture builds a fixture generation: `nodes` nodes, each publishing
// `edgesPerNode` outgoing relations, with the evidence rows of every second
// relation. It returns the packed-adjacency reader over it, the evidence
// source, and each node's own relations in canonical id order.
//
// The surrogate order is REVERSED against the canonical one. The port delivers
// an owner's list in surrogate order, and P-C joins it against a stream ordered
// by canonical relation id, so a fixture whose two orders agreed could not tell
// a correct join from one that skipped the ordering step entirely.
func relationFixture(t *testing.T, nodes, edgesPerNode int) (graph.GraphReader, *evidenceFixture,
	map[model.NodeID][]model.Relation) {
	t.Helper()
	ev := &evidenceFixture{rows: map[model.RelationID][]sqlite.StoredEvidence{}}
	byNode := map[model.NodeID][]model.Relation{}
	all := make([]model.Relation, 0, nodes*edgesPerNode)
	nodeFacts := make([]model.Node, 0, nodes)
	for n := 0; n < nodes; n++ {
		node := model.NodeID(fmt.Sprintf("n%03d", n))
		nodeFacts = append(nodeFacts, model.Node{ID: node, Kind: model.NodeFunction})
		for e := 0; e < edgesPerNode; e++ {
			rel := model.Relation{ID: model.RelationID(fmt.Sprintf("r%04d", n*edgesPerNode+e)),
				From: node, To: model.NodeID(fmt.Sprintf("n%03d", (n+1)%nodes)),
				Kind: scopeRelations[e%len(scopeRelations)]}
			byNode[node] = append(byNode[node], rel)
			all = append(all, rel)
			if e%2 == 0 {
				ev.rows[rel.ID] = []sqlite.StoredEvidence{
					{Evidence: model.Evidence{Precision: model.PrecisionCompiler}}}
			}
		}
	}
	g := graph.NewMemoryGraphOrdered(model.Binding{}, nodeFacts, all, graph.MemoryGraphOrder{
		Relations: func(a, b model.RelationID) int { return cmp.Compare(b, a) },
	})
	return g, ev, byNode
}

// candidatesOver builds candidates whose routes name the given relations of the
// given nodes, including one excluded candidate: P-C deliberately does not
// filter on Excluded, so an excluded candidate's node and routes are part of
// the scan and diverting them would change what it types.
func candidatesOver(edges map[model.NodeID][]model.Relation, nodes []model.NodeID, hopsPerPath int) []candidate {
	out := make([]candidate, 0, len(nodes))
	for i, n := range nodes {
		rels := edges[n]
		var path model.RelationPath
		for h := 0; h < hopsPerPath && h < len(rels); h++ {
			path.Relations = append(path.Relations, rels[h].ID)
		}
		c := candidate{NodeID: n, FileID: model.FileID(fmt.Sprintf("f%03d", i)),
			Path: fmt.Sprintf("pkg%d/a.go", i%3), Paths: []model.RelationPath{path}}
		if i%7 == 6 {
			c.Excluded = "scope_boundary"
		}
		out = append(out, c)
	}
	return out
}

// TestPassCMatchesWholeSet is the parity proof of P-C: on every shape that
// terminates the edge scan differently -- nothing wanted, an early exit once
// every relation is typed, an unmatchable relation that runs the node list out,
// an edge budget that cuts the scan short, and more nodes than one batch -- the
// streamed pass must reach the same completeness verdict and put the same kind
// and multiplier on every hop as the whole-set form it replaced.
//
// The fixture's surrogate order is the reverse of its canonical order, so the
// per-batch sort in scanEdgeKinds is load-bearing: without it the forward
// cursor over the wanted stream walks past the entries a batch delivered and
// the scan leaves wanted relations untyped.
func TestPassCMatchesWholeSet(t *testing.T) {
	for _, tc := range []struct {
		name      string
		nodes     int
		edges     int
		hops      int
		pageItems int
		budget    config.Limit
		unmatched bool
	}{
		{name: "no routes at all", nodes: 4, edges: 3, hops: 0, pageItems: 8, budget: config.Unlimited},
		{name: "every wanted edge typed early", nodes: 6, edges: 3, hops: 2, pageItems: 8, budget: config.Unlimited},
		{name: "several node batches", nodes: 20, edges: 3, hops: 2, pageItems: 3, budget: config.Unlimited},
		{name: "paged within a node batch", nodes: 12, edges: 6, hops: 4, pageItems: 2, budget: config.Unlimited},
		{name: "edge budget cuts the scan", nodes: 20, edges: 6, hops: 4, pageItems: 3, budget: config.Limit(7)},
		// The budget spent at a NODE BATCH boundary rather than inside one: the
		// streamed scan latches `stopped` after the batch it cut and the
		// whole-set form breaks at the bottom of its loop, and the two must stop
		// on the same batch or a bounded compile types a different set.
		{name: "edge budget cuts at a node batch boundary", nodes: 20, edges: 6, hops: 4,
			pageItems: 3, budget: config.Limit(36)},
		{name: "an unmatchable relation", nodes: 8, edges: 3, hops: 2, pageItems: 4, budget: config.Unlimited, unmatched: true},
		// Ruling C8: more wanted relations than one keyset page carries. Both
		// sides must page to exhaustion under an unlimited bound.
		{name: "more wanted relations than one page", nodes: 60, edges: 6, hops: 4,
			pageItems: model.MaxPageItems, budget: config.Unlimited},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			g, evidence, edges := relationFixture(t, tc.nodes, tc.edges)
			nodes := make([]model.NodeID, 0, tc.nodes)
			for n := 0; n < tc.nodes; n++ {
				nodes = append(nodes, model.NodeID(fmt.Sprintf("n%03d", n)))
			}
			cands := candidatesOver(edges, nodes, tc.hops)
			if tc.unmatched {
				// A relation no node carries: the typed set can never reach
				// the wanted set, so the scan runs the node list out and the
				// compile reports an incomplete scope.
				cands[0].Paths[0].Relations = append(cands[0].Paths[0].Relations, "rZZZZ")
			}
			// One candidate names the same node as another and one names no
			// node: both are what the dedupe and the "" skip must survive.
			cands = append(cands, candidate{NodeID: nodes[0], FileID: "fdup", Paths: cands[0].Paths},
				candidate{FileID: "fnode", Paths: []model.RelationPath{{Relations: []model.RelationID{""}}}})

			c := testCompiler(tc.pageItems, tc.budget)

			wantRelations, wantComplete, err := c.relationsOnPathsWholeSet(ctx, g, cands)
			if err != nil {
				t.Fatalf("whole-set edge scan: %v", err)
			}
			wantPrecision, err := c.resolvePrecisionWholeSet(ctx, evidence, cands)
			if err != nil {
				t.Fatalf("whole-set precision: %v", err)
			}

			streamEvidence := &evidenceFixture{rows: evidence.rows}
			s := openSorts(t, config.Config{})
			candRun, hopRun := spoolCandidates(t, s, cands)
			got, err := c.passCRelationAttributes(ctx, s, streamEvidence, g, hopRun, candRun)
			if err != nil {
				t.Fatalf("streamed P-C: %v", err)
			}

			// A parity proof that never reached the reads it proves proves
			// nothing, and that is exactly what the package's own compile
			// fixtures give: guard it.
			switch {
			case tc.hops > 0 && (len(wantRelations) == 0 || streamEvidence.calls == 0):
				t.Fatalf("fixture never reached the reads it proves: %d typed relations, %d evidence calls",
					len(wantRelations), streamEvidence.calls)
			case tc.hops == 0 && streamEvidence.calls != 0:
				// The "" asymmetry: the empty relation id enters the wanted
				// set so the scan still runs and can never complete, while the
				// precision read skips it so no evidence is read at all. Both
				// sides must keep it.
				t.Fatalf("the empty relation id must not be read for evidence, got %d calls", streamEvidence.calls)
			}
			if got.Complete != wantComplete {
				t.Fatalf("completeness: streamed %v, whole-set %v", got.Complete, wantComplete)
			}
			assertHopAttributes(t, got.Hops, hopRun, wantRelations, wantPrecision)
		})
	}
}

// assertHopAttributes checks the joined hop stream against what scorePath would
// read from the two whole-set maps, hop for hop and in route order: the join is
// only parity-preserving if every hop carries the kind and multiplier the map
// lookups would have produced, including the untyped hops that make a route
// inadmissible.
func assertHopAttributes(t *testing.T, got, source *pagination.SortedRun[hopRec],
	relations map[model.RelationID]model.Relation, precision map[model.RelationID]int64) {
	t.Helper()
	var want []hopRec
	if err := source.Each(func(h hopRec) error {
		h.Kind = relations[h.RelationID].Kind
		if m, ok := precision[h.RelationID]; ok {
			h.Multiplier = m
		}
		want = append(want, h)
		return nil
	}); err != nil {
		t.Fatalf("walk source hops: %v", err)
	}
	var have []hopRec
	if err := got.Each(func(h hopRec) error { have = append(have, h); return nil }); err != nil {
		t.Fatalf("walk joined hops: %v", err)
	}
	if !reflect.DeepEqual(have, want) {
		t.Fatalf("joined hops diverged\nstreamed:  %+v\nwhole-set: %+v", have, want)
	}
}

// TestPassEMatchesCentralityMap proves P-E against the nested map it replaces:
// the streamed count of a package's distinct admitted edges must equal
// len(centrality[pkg]) for every package, including a package whose edges
// repeat across candidates.
func TestPassEMatchesCentralityMap(t *testing.T) {
	ctx := context.Background()
	s := openSorts(t, config.Config{})
	edges, err := newSort[pkgEdgeRec](s, "test-pkg-edge", lessPkgEdge, sizeOfPkgEdge)
	if err != nil {
		t.Fatalf("edge sort: %v", err)
	}
	edges = edges.WithFold(foldPkgEdgeDistinct)

	reference := map[string]map[model.RelationID]struct{}{}
	for i := 0; i < 500; i++ {
		// Repeats across packages and within one, in an order no package's
		// edges are contiguous in, so the sort is what makes them so.
		pkg := fmt.Sprintf("pkg%d", i%7)
		id := model.RelationID(fmt.Sprintf("r%03d", i%53))
		if err := edges.Add(pkgEdgeRec{Pkg: pkg, RelationID: id}); err != nil {
			t.Fatalf("add edge: %v", err)
		}
		if reference[pkg] == nil {
			reference[pkg] = map[model.RelationID]struct{}{}
		}
		reference[pkg][id] = struct{}{}
	}

	run, err := (&Compiler{}).passECentrality(ctx, s, edges)
	if err != nil {
		t.Fatalf("streamed P-E: %v", err)
	}
	got := map[string]int64{}
	var order []string
	if err := run.Each(func(r pkgCountRec) error {
		if _, dup := got[r.Pkg]; dup {
			return fmt.Errorf("package %q counted twice", r.Pkg)
		}
		got[r.Pkg], order = r.Edges, append(order, r.Pkg)
		return nil
	}); err != nil {
		t.Fatalf("walk counts: %v", err)
	}
	if len(got) != len(reference) {
		t.Fatalf("packages: streamed %d, map %d", len(got), len(reference))
	}
	for pkg, edgeSet := range reference {
		if got[pkg] != int64(len(edgeSet)) {
			t.Errorf("package %q: streamed %d distinct edges, map %d", pkg, got[pkg], len(edgeSet))
		}
	}
	if !sort.StringsAreSorted(order) {
		t.Errorf("counts must be emitted in package order for P-F's merge join, got %v", order)
	}
}

// TestEdgeScanPagesToExhaustionUnderAnUnlimitedBound is ruling C8's proof.
//
// context.max_graph_edges is Unlimited by default. Reading that absent bound as
// model.MaxPageItems would stop a scope naming more than one page of relations
// after 200 entries, leave the rest untyped and report an incomplete scope -- a
// default that refuses work on exactly the repositories this compiler serves.
//
// Restoring `ValueOr(model.MaxPageItems)` at either scan site fails this.
func TestEdgeScanPagesToExhaustionUnderAnUnlimitedBound(t *testing.T) {
	ctx := context.Background()
	const nodes, edgesPerNode, hops = 60, 6, 4
	g, evidence, edges := relationFixture(t, nodes, edgesPerNode)
	ids := make([]model.NodeID, 0, nodes)
	for n := 0; n < nodes; n++ {
		ids = append(ids, model.NodeID(fmt.Sprintf("n%03d", n)))
	}
	cands := candidatesOver(edges, ids, hops)
	wanted := map[model.RelationID]struct{}{}
	for _, cand := range cands {
		for _, p := range cand.Paths {
			for _, rel := range p.Relations {
				wanted[rel] = struct{}{}
			}
		}
	}
	if len(wanted) <= model.MaxPageItems {
		t.Fatalf("the fixture must want more than one page of relations, got %d", len(wanted))
	}

	c := testCompiler(model.MaxPageItems, config.Unlimited)
	relations, complete, err := c.relationsOnPathsWholeSet(ctx, g, cands)
	if err != nil {
		t.Fatalf("whole-set edge scan: %v", err)
	}
	if !complete || len(relations) != len(wanted) {
		t.Fatalf("whole-set scan typed %d of %d wanted relations (complete=%v)", len(relations), len(wanted), complete)
	}

	s := openSorts(t, config.Config{})
	candRun, hopRun := spoolCandidates(t, s, cands)
	got, err := c.passCRelationAttributes(ctx, s, evidence, g, hopRun, candRun)
	if err != nil {
		t.Fatalf("streamed P-C: %v", err)
	}
	if !got.Complete {
		t.Fatalf("the streamed scan reported an incomplete scope under an unlimited edge bound")
	}
	typed := map[model.RelationID]struct{}{}
	if err := got.Hops.Each(func(h hopRec) error {
		if h.Kind != "" {
			typed[h.RelationID] = struct{}{}
		}
		return nil
	}); err != nil {
		t.Fatalf("walk hops: %v", err)
	}
	if len(typed) != len(wanted) {
		t.Fatalf("the streamed scan typed %d of %d wanted relations", len(typed), len(wanted))
	}

	// A user-set bound is still obeyed: only an explicit limit cuts the scan.
	bounded := testCompiler(model.MaxPageItems, config.Limit(model.MaxPageItems))
	if _, boundedComplete, err := bounded.relationsOnPathsWholeSet(ctx, g, cands); err != nil {
		t.Fatalf("bounded edge scan: %v", err)
	} else if boundedComplete {
		t.Fatalf("a user-set max_graph_edges of %d must cut this scan", model.MaxPageItems)
	}
}
