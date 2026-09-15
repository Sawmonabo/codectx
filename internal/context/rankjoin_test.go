package context

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/Sawmonabo/codectx/internal/config"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
	"github.com/Sawmonabo/codectx/internal/storage/sqlite"
)

// recordingReader serves a synthetic pinned graph and records every call, so
// the streamed pass and the whole-set functions it replaces can be compared on
// what they ASKED the store, not only on what they returned. The package's own
// fixtures never reach the edge scan -- every compile in context_test.go has an
// empty `wanted` set -- so the read log has no coverage without this.
type recordingReader struct {
	// edges are the visible edges of each node, and evidence the rows of each
	// relation, exactly as the pinned reader would join them.
	edges    map[model.NodeID][]model.Relation
	evidence map[model.RelationID][]sqlite.StoredEvidence

	log []string
}

func (r *recordingReader) EdgesBatch(_ context.Context, nodes []model.NodeID, dir model.Direction,
	kinds []model.RelationKind, after model.RelationID, limit int) ([]model.Relation, error) {
	names := make([]string, len(nodes))
	for i, n := range nodes {
		names[i] = string(n)
	}
	r.log = append(r.log, fmt.Sprintf("edges nodes=%v dir=%s kinds=%d after=%q limit=%d",
		names, dir, len(kinds), after, limit))

	seen := map[model.RelationID]model.Relation{}
	for _, n := range nodes {
		for _, rel := range r.edges[n] {
			seen[rel.ID] = rel
		}
	}
	out := make([]model.Relation, 0, len(seen))
	for _, rel := range seen {
		if rel.ID > after {
			out = append(out, rel)
		}
	}
	// Keyset order by relation id, which is EdgesBatch's documented contract
	// and what makes the merge-matching in the streamed scan legal.
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (r *recordingReader) EvidenceBatch(_ context.Context, relations []model.RelationID,
	perRelation int) (map[model.RelationID][]sqlite.StoredEvidence, error) {
	ids := make([]string, len(relations))
	for i, id := range relations {
		ids[i] = string(id)
	}
	r.log = append(r.log, fmt.Sprintf("evidence ids=%v per=%d", ids, perRelation))
	out := map[model.RelationID][]sqlite.StoredEvidence{}
	for _, id := range relations {
		if rows, ok := r.evidence[id]; ok {
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

func relationFixture(t *testing.T, nodes, edgesPerNode int) *recordingReader {
	t.Helper()
	r := &recordingReader{edges: map[model.NodeID][]model.Relation{},
		evidence: map[model.RelationID][]sqlite.StoredEvidence{}}
	for n := 0; n < nodes; n++ {
		node := model.NodeID(fmt.Sprintf("n%03d", n))
		for e := 0; e < edgesPerNode; e++ {
			id := model.RelationID(fmt.Sprintf("r%04d", n*edgesPerNode+e))
			r.edges[node] = append(r.edges[node], model.Relation{ID: id,
				From: node, To: model.NodeID(fmt.Sprintf("n%03d", (n+1)%nodes)),
				Kind: scopeRelations[e%len(scopeRelations)]})
			if e%2 == 0 {
				r.evidence[id] = []sqlite.StoredEvidence{
					{Evidence: model.Evidence{Precision: model.PrecisionCompiler}}}
			}
		}
	}
	return r
}

// candidatesOver builds candidates whose routes name the given relations of the
// given nodes, including one excluded candidate: relationsOnPaths deliberately
// does not filter on Excluded, so an excluded candidate's node and routes are
// part of the read log and diverting them would change it.
func candidatesOver(r *recordingReader, nodes []model.NodeID, hopsPerPath int) []candidate {
	out := make([]candidate, 0, len(nodes))
	for i, n := range nodes {
		rels := r.edges[n]
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

func openSorts(t *testing.T) *compileSorts {
	t.Helper()
	s, err := newCompileSorts(config.Config{}, t.TempDir())
	if err != nil {
		t.Fatalf("sort area: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Fatalf("release sort area: %v", err)
		}
	})
	return s
}

// TestPassCReadLogMatchesWholeSet is the parity proof of P-C: on every shape
// that terminates the edge scan differently -- nothing wanted, an early exit
// once every relation is typed, an unmatchable relation that runs the node list
// out, an edge budget that cuts the scan short, and more nodes than one batch --
// the streamed pass must ask the store exactly what the whole-set functions ask
// it, in the same order.
func TestPassCReadLogMatchesWholeSet(t *testing.T) {
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
		{name: "an unmatchable relation", nodes: 8, edges: 3, hops: 2, pageItems: 4, budget: config.Unlimited, unmatched: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			fixture := relationFixture(t, tc.nodes, tc.edges)
			nodes := make([]model.NodeID, 0, tc.nodes)
			for n := 0; n < tc.nodes; n++ {
				nodes = append(nodes, model.NodeID(fmt.Sprintf("n%03d", n)))
			}
			cands := candidatesOver(fixture, nodes, tc.hops)
			if tc.unmatched {
				// A relation no node carries: len(out) can never reach
				// len(wanted), so the scan runs the node list out and the
				// compile reports an incomplete scope.
				cands[0].Paths[0].Relations = append(cands[0].Paths[0].Relations, "rZZZZ")
			}
			// One candidate names the same node as another and one names no
			// node: both are what the dedupe and the "" skip must survive.
			cands = append(cands, candidate{NodeID: nodes[0], FileID: "fdup", Paths: cands[0].Paths},
				candidate{FileID: "fnode", Paths: []model.RelationPath{{Relations: []model.RelationID{""}}}})

			c := testCompiler(tc.pageItems, tc.budget)

			refReader := &recordingReader{edges: fixture.edges, evidence: fixture.evidence}
			wantRelations, wantComplete, err := c.relationsOnPathsWholeSet(ctx, refReader, cands)
			if err != nil {
				t.Fatalf("whole-set edge scan: %v", err)
			}
			wantPrecision, err := c.resolvePrecisionWholeSet(ctx, refReader, cands)
			if err != nil {
				t.Fatalf("whole-set precision: %v", err)
			}

			streamReader := &recordingReader{edges: fixture.edges, evidence: fixture.evidence}
			s := openSorts(t)
			candRun, hopRun := spoolCandidates(t, s, cands)
			got, err := c.passCRelationAttributes(ctx, s, streamReader, hopRun, candRun)
			if err != nil {
				t.Fatalf("streamed P-C: %v", err)
			}

			// A parity proof over an empty log proves nothing, and that is
			// exactly what the package's existing fixtures give: guard it.
			if edgeReads, evidenceReads := countReads(refReader.log); tc.hops > 0 && (edgeReads == 0 || evidenceReads == 0) {
				t.Fatalf("fixture never reached the reads it proves: %d edge, %d evidence calls", edgeReads, evidenceReads)
			} else if tc.hops == 0 && evidenceReads != 0 {
				// The "" asymmetry: relationsOnPaths counts the empty
				// relation id in `wanted` so the scan still runs and can
				// never complete, while resolvePrecision skips it so no
				// evidence is read at all. Both sides must keep it.
				t.Fatalf("the empty relation id must not be read for evidence, got %d calls", evidenceReads)
			}
			if !reflect.DeepEqual(streamReader.log, refReader.log) {
				t.Fatalf("read log diverged\nstreamed: %#v\nwhole-set: %#v", streamReader.log, refReader.log)
			}
			if got.Complete != wantComplete {
				t.Fatalf("completeness: streamed %v, whole-set %v", got.Complete, wantComplete)
			}
			assertHopAttributes(t, got.Hops, hopRun, wantRelations, wantPrecision)
		})
	}
}

// countReads splits a read log into its edge and evidence calls.
func countReads(log []string) (edges, evidence int) {
	for _, line := range log {
		if strings.HasPrefix(line, "edges ") {
			edges++
			continue
		}
		evidence++
	}
	return edges, evidence
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
	s := openSorts(t)
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
