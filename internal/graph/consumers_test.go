package graph

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"testing"

	"github.com/Sawmonabo/codectx/internal/config"
	"github.com/Sawmonabo/codectx/internal/model"
)

// This file is lane GP-L3's own test budget: the identity proofs that the
// consumers moved onto graph.GraphReader (ADR-0005) answer exactly what the
// containment-walking implementations answered, and the direction proof for the
// shortest-path search.
//
// The identity proofs are GOLDEN: the answers were captured from the
// pre-reader implementation, committed as testdata, and are re-checked against
// the reader path. A test that only compared the new implementation with
// itself would pass over any wholesale change of what the answer MEANS, which
// is the shortcut the scale posture forbids.

// memGraphFor builds the reference GraphReader over the shared fixture's own
// nodes and relations, so the two read paths see one graph. The walk still
// reads through Adjacency (it is another lane's), so an engine under test
// carries both ports over the same facts.
func memGraphFor(f *graphFixture) *MemoryGraph {
	nodes := make([]model.Node, 0, len(f.nodes))
	for _, n := range f.nodes {
		nodes = append(nodes, n)
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].ID < nodes[j].ID })
	counts := make(map[model.RelationID]int64, len(f.evidence))
	for id, rows := range f.evidence {
		counts[id] = int64(len(rows))
	}
	return NewMemoryGraph(f.binding, nodes, append([]model.Relation(nil), f.relations...)).
		WithEvidenceCounts(counts)
}

// golden compares got against the committed capture, and writes it when
// CODECTX_UPDATE_GOLDEN is set. The capture is the pre-reader answer; the
// update path exists to MAKE that capture once, never to bless a drift.
func golden(t *testing.T, name string, got any) {
	t.Helper()
	path := filepath.Join("testdata", name)
	blob, err := json.MarshalIndent(got, "", "  ")
	if err != nil {
		t.Fatalf("marshal %s: %v", name, err)
	}
	blob = append(blob, '\n')
	if os.Getenv("CODECTX_UPDATE_GOLDEN") != "" {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatalf("mkdir testdata: %v", err)
		}
		if err := os.WriteFile(path, blob, 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v (capture it with CODECTX_UPDATE_GOLDEN=1)", path, err)
	}
	if string(want) != string(blob) {
		t.Fatalf("%s differs from the captured pre-reader answer\n got: %s\nwant: %s", name, blob, want)
	}
}

// TestRollupAnswerIsUnchangedByTheReader is the rollup identity proof: the same
// pairs, in the same order, with the same edge and evidence counts the
// containment-walking rollup produced.
func TestRollupAnswerIsUnchangedByTheReader(t *testing.T) {
	f := newGraphFixture(t)
	limits := fixtureLimits()
	limits.MaxDepth, limits.MaxVisited, limits.MaxEdges = 0, 0, 0
	limits.MaxPageItems = 2 * fixtureWideCount
	e, err := New(Options{Adjacency: f, Reader: memGraphFor(f), Limits: limits})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	page, err := e.PackageDependencies(context.Background(), model.GraphRequest{
		GenerationID: f.binding.GenerationID,
		Start:        []model.NodeID{fixtureNodeID("n-a"), fixtureNodeID("n-wide")},
		Direction:    model.DirectionOutgoing,
		Relations:    []model.RelationKind{model.RelCalls},
	})
	if err != nil {
		t.Fatalf("PackageDependencies: %v", err)
	}
	if page.Meta.Truncated || page.Meta.NextCursor != "" {
		t.Fatalf("the identity answer must be one complete page: truncated=%v reason=%q",
			page.Meta.Truncated, page.Meta.TruncationReason)
	}
	if len(page.Items) == 0 {
		t.Fatal("the rollup produced no pairs; the fixture proves nothing")
	}
	golden(t, "rollup_pairs.json", page.Items)
}

// TestOverviewCountsAreUnchangedByTheReader is the repository-map identity
// proof: every container's direct file, symbol and byte counts, its parent and
// its depth, exactly as the containment-walking map reported them.
func TestOverviewCountsAreUnchangedByTheReader(t *testing.T) {
	f := newGraphFixture(t)
	limits := fixtureLimits()
	limits.MaxPageItems = 200
	e, err := New(Options{Adjacency: f, Reader: memGraphFor(f), Limits: limits})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	page, err := e.Overview(context.Background(), model.OverviewRequest{
		GenerationID: f.binding.GenerationID})
	if err != nil {
		t.Fatalf("Overview: %v", err)
	}
	if page.Meta.Truncated {
		t.Fatalf("a map inside every bound reports truncated: %q", page.Meta.TruncationReason)
	}
	if len(page.Items) == 0 {
		t.Fatal("the map listed no container; the fixture proves nothing")
	}
	golden(t, "overview_items.json", page.Items)
}

// TestContainmentScanEndsOnTheLastEntry is F7, carried onto the packed reader:
// the containment scan used to report an incomplete read the moment its edge
// allowance was reached, so a containment set whose entries fill the allowance
// EXACTLY reported complete=false -- and both callers refuse an incomplete
// containment read, turning a whole answer into an omitted container. Only an
// allowance with an entry still standing in front of it is incomplete.
//
// Mutation: make the allowance test `maxEdges.Exceeded(read)` in
// scanNeighbours and the exact-fill leg below fails.
func TestContainmentScanEndsOnTheLastEntry(t *testing.T) {
	f := newGraphFixture(t)
	reader := memGraphFor(f)
	contains, ok := reader.Kinds().Code(model.RelContains)
	if !ok {
		t.Fatal("the fixture seals no containment; the scan cannot be exercised")
	}
	// pkg-app's outgoing containment: four `contains` children, a fixed count
	// the allowances below are set against.
	owner := sortedRefs(context.Background(), reader, []model.NodeID{fixtureNodeID("pkg-app")})
	if len(owner) != 1 {
		t.Fatalf("pkg-app resolved to %d surrogate(s), want 1", len(owner))
	}
	const rows = 4
	for _, tc := range []struct {
		name     string
		budget   int64
		wantRead int
		wantDone bool
	}{
		{name: "the allowance is exactly filled", budget: rows, wantRead: rows, wantDone: true},
		{name: "an entry is left past the allowance", budget: rows - 1, wantRead: rows - 1, wantDone: false},
		{name: "the allowance is unlimited", budget: 0, wantRead: rows, wantDone: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			read := 0
			b := &budget{}
			done, err := scanNeighbours(context.Background(), reader, owner,
				model.DirectionOutgoing, []KindCode{contains}, config.Limit(tc.budget), b,
				func(owners, _ []NodeRef) error { read += len(owners); return nil })
			if err != nil {
				t.Fatalf("scanNeighbours: %v", err)
			}
			if read != tc.wantRead || done != tc.wantDone {
				t.Fatalf("read %d entr(ies), complete=%v; want %d, complete=%v",
					read, done, tc.wantRead, tc.wantDone)
			}
			if b.edges != int64(tc.wantRead) {
				t.Fatalf("the scan charged %d edge(s) to the page budget, want %d", b.edges, tc.wantRead)
			}
		})
	}
}

// TestShortestPathFollowsTheRequestedDirection is the certification's "0
// paths" defect: the search only ever followed edges OUTGOING, so a pair
// connected against the edge direction -- a hub reached only by its callers,
// the ordinary case -- was reported as having no path at all. "No path exists"
// is the one answer a path search may not get wrong.
//
// Mutation: pin w.direction back to model.DirectionOutgoing in
// pathWalk.drain's Edges call and the incoming and both legs find no route.
// Mutation: drop the direction from pathQueryHash and the cursor leg accepts a
// token minted for the other direction.
func TestShortestPathFollowsTheRequestedDirection(t *testing.T) {
	f := newGraphFixture(t)
	limits := fixtureLimits()
	limits.MaxDepth, limits.MaxVisited = 0, 0
	e, err := New(Options{Adjacency: f, Reader: memGraphFor(f), Limits: limits})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	// n-hub is reachable from n-a by an outgoing `calls` edge and by nothing
	// else, so the pair (n-hub -> n-a) is connected ONLY against the edge
	// direction: exactly the shape the certification hit.
	req := model.PathRequest{GenerationID: f.binding.GenerationID,
		From: fixtureNodeID("n-hub"), To: fixtureNodeID("n-a"),
		Relations: []model.RelationKind{model.RelCalls}}
	for _, tc := range []struct {
		direction model.Direction
		want      int
	}{
		{direction: "", want: 0},
		{direction: model.DirectionOutgoing, want: 0},
		{direction: model.DirectionIncoming, want: 1},
		{direction: model.DirectionBoth, want: 1},
	} {
		t.Run(string(tc.direction)+"/", func(t *testing.T) {
			r := req
			r.Direction = tc.direction
			res, err := e.ShortestPath(context.Background(), r)
			if err != nil {
				t.Fatalf("ShortestPath: %v", err)
			}
			if len(res.Paths) != tc.want {
				t.Fatalf("direction %q found %d route(s), want %d: a pair connected only against "+
					"the edge direction is not unreachable", tc.direction, len(res.Paths), tc.want)
			}
			for _, p := range res.Paths {
				if len(p.Relations) == 0 {
					t.Fatal("a served route carries no relation")
				}
			}
		})
	}
	// The direction is part of the query a continuation is bound to: a token
	// minted for one orientation resumes a settled set that means nothing in
	// the other.
	out := req
	out.Direction = model.DirectionOutgoing
	in := req
	in.Direction = model.DirectionIncoming
	if pathQueryHash(DefaultRelations(), out.Direction, out.From, out.To, 0) ==
		pathQueryHash(DefaultRelations(), in.Direction, in.From, in.To, 0) {
		t.Fatal("two directions of one pair share a query hash: a cursor would resume the other search")
	}
}

// TestContainerContentsCountsFilesSymbolsAndBytes covers the three branches the
// shared fixture cannot reach: it carries no file node, so the file count and
// the source-byte total are zero everywhere in it, and no container of it holds
// another container. All three are exactly where a wrong count would read as
// the repository's shape rather than as a missing measurement -- a package
// whose nested packages were counted as its own symbols, or whose bytes were
// silently zero.
//
// Mutations, each caught here: count a file as a symbol (drop the NodeFile
// branch); count a nested container as a symbol (drop the isOverviewContainer
// branch); read the byte total from anywhere but SourceBytes.
func TestContainerContentsCountsFilesSymbolsAndBytes(t *testing.T) {
	node := func(name string, kind model.NodeKind, size int64) model.Node {
		n := model.Node{ID: fixtureNodeID(name), Kind: kind, Name: name, QualifiedName: name,
			Language: "go", SemanticSource: model.SemanticCanonical}
		if kind == model.NodeFile {
			n.Metadata = json.RawMessage(`{"size":` + strconv.FormatInt(size, 10) + `}`)
		}
		return n
	}
	nodes := []model.Node{
		node("c-pkg", model.NodePackage, 0),
		node("c-nested", model.NodePackage, 0),
		node("c-file-a", model.NodeFile, 400),
		node("c-file-b", model.NodeFile, 26),
		node("c-fn", model.NodeFunction, 0),
		node("c-var", model.NodeVariable, 0),
	}
	var rels []model.Relation
	add := func(from string, kind model.RelationKind, to string) {
		rels = append(rels, model.Relation{ID: fixtureRelationID(len(rels)),
			From: fixtureNodeID(from), Kind: kind, To: fixtureNodeID(to)})
	}
	add("c-pkg", model.RelContains, "c-file-a")
	add("c-pkg", model.RelContains, "c-file-b")
	add("c-pkg", model.RelContains, "c-nested") // structure, not a symbol
	add("c-pkg", model.RelContains, "c-var")
	add("c-pkg", model.RelDefines, "c-fn") // a top-level declaration
	binding := model.Binding{RepositoryID: model.RepositoryID(fixtureID("repo-1")),
		SnapshotID: model.SnapshotID(fixtureID("snap-1")), GenerationID: 1,
		AnalysisKey: model.AnalysisKey(fixtureID("akey-1"))}
	e := &Engine{reader: NewMemoryGraph(binding, nodes, rels), limits: fixtureLimits()}

	pkg := fixtureNodeID("c-pkg")
	meta := &model.QueryMeta{}
	held, unmeasured, err := e.containerContents(context.Background(),
		[]model.NodeID{pkg}, &budget{}, meta)
	if err != nil {
		t.Fatalf("containerContents: %v", err)
	}
	if unmeasured[pkg] {
		t.Fatal("an unlimited edge allowance left the container unmeasured")
	}
	got := held[pkg]
	// Two files, two symbols (the variable it contains and the function it
	// defines), the nested package counted as neither, and the bytes of both
	// files summed from the source-byte side array.
	if got.files != 2 || got.symbols != 2 || got.bytes != 426 {
		t.Fatalf("c-pkg holds %d file(s), %d symbol(s), %d byte(s); want 2, 2, 426",
			got.files, got.symbols, got.bytes)
	}
	if meta.Truncated {
		t.Fatalf("a complete count reported truncated: %q", meta.TruncationReason)
	}
}
