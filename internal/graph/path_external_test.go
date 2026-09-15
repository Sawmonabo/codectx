package graph

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
)

// pathGraph is a standalone adjacency built from an explicit edge list, so a
// path invariant is proved on the smallest graph that exercises it rather than
// on the shared fixture's incidental shape.
type pathGraph struct {
	rels  []model.Relation
	nodes map[model.NodeID]model.Node
}

func newPathGraph(edges [][3]string) *pathGraph {
	g := &pathGraph{nodes: map[model.NodeID]model.Node{}}
	add := func(n string) model.NodeID {
		id := model.NodeID(fixtureID("node-" + n))
		if _, ok := g.nodes[id]; !ok {
			g.nodes[id] = model.Node{ID: id, Kind: model.NodeFunction, Name: n, QualifiedName: n,
				Language: "go", SemanticSource: model.SemanticCanonical}
		}
		return id
	}
	for i, e := range edges {
		g.rels = append(g.rels, model.Relation{
			ID:   model.RelationID(fixtureID(fmt.Sprintf("rel-%04d", i))),
			From: add(e[0]), To: add(e[1]), Kind: model.RelationKind(e[2]),
		})
	}
	sort.Slice(g.rels, func(i, j int) bool { return g.rels[i].ID < g.rels[j].ID })
	return g
}

func (g *pathGraph) node(n string) model.NodeID { return model.NodeID(fixtureID("node-" + n)) }

func (g *pathGraph) Edges(_ context.Context, nodes []model.NodeID, dir model.Direction,
	kinds []model.RelationKind, after model.RelationID, limit int) ([]model.Relation, error) {
	want := map[model.NodeID]bool{}
	for _, n := range nodes {
		want[n] = true
	}
	allowed := map[model.RelationKind]bool{}
	for _, k := range kinds {
		allowed[k] = true
	}
	var out []model.Relation
	for _, r := range g.rels {
		if r.ID <= after || len(out) >= limit {
			continue
		}
		if dir == model.DirectionOutgoing && !want[r.From] {
			continue
		}
		if len(allowed) > 0 && !allowed[r.Kind] {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

func (g *pathGraph) NodesByID(_ context.Context, ids []model.NodeID) ([]model.Node, error) {
	var out []model.Node
	for _, id := range ids {
		if n, ok := g.nodes[id]; ok {
			out = append(out, n)
		}
	}
	return out, nil
}

func (g *pathGraph) EvidenceFor(context.Context, []model.RelationID, int) (map[model.RelationID][]model.EvidenceID, error) {
	return map[model.RelationID][]model.EvidenceID{}, nil
}

func (g *pathGraph) Capabilities(context.Context) ([]model.CapabilityState, error) { return nil, nil }

func (g *pathGraph) Binding() model.Binding {
	return model.Binding{RepositoryID: model.RepositoryID(fixtureID("repo-1")),
		SnapshotID: model.SnapshotID(fixtureID("snap-1")), GenerationID: 1,
		AnalysisKey: model.AnalysisKey(fixtureID("akey-1"))}
}

// pathProofGraph is the fixture the exactness proof runs on. It holds the three
// shapes a shortest-path search can get wrong:
//
//   - TIED ROUTES: `mid` is reached at cost 2 both directly (s -> mid, one
//     `imports` hop) and through `hop` (two `calls` hops), so a search that
//     keeps one parent per node serves a visibly short route list;
//   - a STALE RELAXATION: `alt` reaches `mid` again at cost 3, in a bucket the
//     search still sweeps before the target settles at 4. That row must be
//     dropped as superseded; re-settling `mid` there rewrites its distance and
//     its parent set, and the served routes change;
//   - DEAD ENDS: `dead` and the g-nodes are settled but reach nothing, so
//     enumeration must prune them rather than walk them.
//
// `calls` (cost 1) and `imports` (cost 2) give it two integer costs; `width`
// fans the lattice out so the walk needs many bucket chunks under a small
// frontier budget.
func pathProofGraph(width int) *pathGraph {
	edges := [][3]string{
		{"s", "hop", string(model.RelCalls)},     // cost 1
		{"hop", "mid", string(model.RelCalls)},   // cost 2 at mid
		{"s", "mid", string(model.RelImports)},   // cost 2 at mid: TIED with the hop route
		{"s", "alt", string(model.RelCalls)},     // cost 1
		{"alt", "mid", string(model.RelImports)}, // cost 3 at mid: the superseded relaxation
		{"mid", "t", string(model.RelImports)},   // cost 4 at the target
		{"mid", "dead", string(model.RelCalls)},  // a settled node that reaches nothing
	}
	for i := 0; i < width; i++ {
		edges = append(edges,
			[3]string{"s", fmt.Sprintf("f%03d", i), string(model.RelCalls)},
			[3]string{fmt.Sprintf("f%03d", i), fmt.Sprintf("g%03d", i), string(model.RelCalls)})
	}
	return newPathGraph(edges)
}

func pathProofLimits() Limits {
	l := fixtureLimits()
	// Every scale bound unlimited: these rows isolate the walk's own behaviour.
	l.MaxDepth, l.MaxVisited, l.MaxEdges = 0, 0, 0
	l.MaxReasonPaths = 8
	l.QueryTimeout = time.Minute
	return l
}

// routeSequences renders an answer as the ordered list of its ordered relation
// id sequences -- the served output, not an internal structure.
func routeSequences(res model.PathResult) []string {
	out := make([]string, 0, len(res.Paths))
	for _, p := range res.Paths {
		ids := make([]string, 0, len(p.Relations))
		for _, r := range p.Relations {
			ids = append(ids, string(r))
		}
		out = append(out, fmt.Sprintf("%d:%s", p.CostUnits, strings.Join(ids, ",")))
	}
	return out
}

// referenceRoutes is the unbounded in-heap reference: an exhaustive walk of
// every simple route, keeping the cheapest cost and every route that achieves
// it, in the frozen (relation id sequence) order. It is deliberately naive --
// it shares no code with the engine, so it cannot share a bug with it.
func referenceRoutes(g *pathGraph, from, to model.NodeID, kinds []model.RelationKind) (int64, []string) {
	allowed := map[model.RelationKind]bool{}
	for _, k := range kinds {
		allowed[k] = true
	}
	best := int64(-1)
	var found []string
	seen := map[model.NodeID]bool{from: true}
	var cur []model.Relation
	var walk func(n model.NodeID, cost int64)
	walk = func(n model.NodeID, cost int64) {
		if n == to {
			if best < 0 || cost < best {
				best, found = cost, nil
			}
			if cost == best {
				ids := make([]string, 0, len(cur))
				for _, r := range cur {
					ids = append(ids, string(r.ID))
				}
				found = append(found, fmt.Sprintf("%d:%s", cost, strings.Join(ids, ",")))
			}
			return
		}
		for _, r := range g.rels {
			if r.From != n || seen[r.To] || (len(allowed) > 0 && !allowed[r.Kind]) {
				continue
			}
			seen[r.To] = true
			cur = append(cur, r)
			walk(r.To, cost+Cost(r.Kind))
			cur = cur[:len(cur)-1]
			seen[r.To] = false
		}
	}
	walk(from, 0)
	sort.Strings(found)
	return best, found
}

// TestShortestPathIsExactUnderATinyChunkBudget is the P5 exactness proof. The
// search state -- settled distances, the parent edges of the shortest-path DAG
// and the tentative cost buckets -- lives in the scratch database, so a chunk
// budget far smaller than the state changes how many chunks the walk reads and
// NOTHING about the answer: same cost, same routes, in the same order, and not
// truncated.
//
// Mutation (drop the staleness/settled check in pathWalk.settle -- let a group
// re-settle a node already in `settled`): the bucket-3 row for `mid` is swept
// before the target settles, so `mid` is re-settled at distance 3 with `alt` as
// its parent; the served route list becomes the single 5-cost route through
// `alt` instead of the two 4-cost routes, and the comparison below FAILS.
//
// Mutation (skip the spill: keep the settled/parent/bucket state in Go maps
// instead of the scratch): the heap assertion in
// TestPathWalkHeapIsBoundedByTheChunk fails, because the peak chunk then grows
// with the reachable set.
func TestShortestPathIsExactUnderATinyChunkBudget(t *testing.T) {
	g := pathProofGraph(64)
	kinds := []model.RelationKind{model.RelCalls, model.RelImports}
	req := model.PathRequest{GenerationID: 1, From: g.node("s"), To: g.node("t"), Relations: kinds}

	wantCost, wantRoutes := referenceRoutes(g, req.From, req.To, kinds)
	if len(wantRoutes) < 2 {
		t.Fatalf("the proof needs a fixture with tied routes; the reference found %d", len(wantRoutes))
	}

	answers := map[string][]string{}
	for _, frontier := range []int64{1, 1 << 20} {
		limits := pathProofLimits()
		limits.FrontierBytes = frontier
		e, err := New(Options{Adjacency: g, Limits: limits})
		if err != nil {
			t.Fatalf("new engine: %v", err)
		}
		res, err := e.ShortestPath(context.Background(), req)
		if err != nil {
			t.Fatalf("frontier %d: shortest path: %v", frontier, err)
		}
		if res.Meta.Truncated {
			t.Fatalf("frontier %d: the answer is truncated (%q); a chunk budget must never truncate",
				frontier, res.Meta.TruncationReason)
		}
		got := routeSequences(res)
		sort.Strings(got)
		if len(got) == 0 || !strings.HasPrefix(got[0], fmt.Sprintf("%d:", wantCost)) {
			t.Fatalf("frontier %d: got routes %v, want cost %d", frontier, got, wantCost)
		}
		answers[fmt.Sprint(frontier)] = got
	}
	if fmt.Sprint(answers["1"]) != fmt.Sprint(answers[fmt.Sprint(1<<20)]) {
		t.Fatalf("the bounded search answered %v and the unbounded one %v: the chunk budget changed the answer",
			answers["1"], answers[fmt.Sprint(1<<20)])
	}
	if fmt.Sprint(answers["1"]) != fmt.Sprint(wantRoutes) {
		t.Fatalf("the search answered %v; the unbounded in-memory reference says %v", answers["1"], wantRoutes)
	}
}

// TestPathWalkHeapIsBoundedByTheChunk is the memory statement. Before this the
// walk held dist, depth, settled, the cached edges and the priority queue in
// heap, all sized by the REACHABLE SET, and crossing a byte ceiling TRUNCATED
// the answer (pathReasonMemory, now deleted). They are on disk now, so the only
// heap structure left is one node-aligned chunk of the current cost bucket, and
// its high-water mark must not grow with the graph.
//
// Mutation: skip the spill -- read a whole bucket per chunk instead of cutting
// at the byte budget -- and peakChunkBytes rises with the fan-out, failing the
// comparison below.
func TestPathWalkHeapIsBoundedByTheChunk(t *testing.T) {
	const frontier = 512
	peaks := map[int]int64{}
	for _, width := range []int{16, 512} {
		g := pathProofGraph(width)
		sc, err := openPathScratch(context.Background(), t.TempDir())
		if err != nil {
			t.Fatalf("scratch: %v", err)
		}
		defer sc.close()
		w := &pathWalk{adjacency: g, kinds: []model.RelationKind{model.RelCalls, model.RelImports},
			frontierBytes: frontier, sc: sc, scCtx: context.Background()}
		if err := w.run(context.Background(), g.node("s"), g.node("unreachable")); err != nil {
			t.Fatalf("width %d: walk: %v", width, err)
		}
		if w.visited < int64(width) {
			t.Fatalf("width %d: the walk settled only %d node(s); the proof needs a walk larger than its chunk",
				width, w.visited)
		}
		peaks[width] = w.peakChunkBytes
	}
	// One group may overshoot the budget (a group is never split), so the
	// envelope is the budget plus one group, not the budget exactly.
	const oneGroup = edgeRowOverheadBytes + 4*model.MaxIdentifierBytes
	for width, peak := range peaks {
		if peak > frontier+oneGroup {
			t.Fatalf("width %d: peak chunk held %d bytes, over the %d-byte budget plus one group",
				width, peak, frontier+oneGroup)
		}
	}
	if peaks[512] > peaks[16] {
		t.Fatalf("the peak chunk grew from %d to %d bytes when the graph grew 32x: heap tracks the graph",
			peaks[16], peaks[512])
	}
}

// TestPathDeadlineTruncatesRatherThanFails is the contract docs/queries.md
// states: an expired query deadline is reported as truncation together with
// whatever the search found, never as a failed request.
//
// It is not a restatement of the traversal deadline test: the path search is
// the one walk whose state is a DATABASE, and every statement it issues used to
// carry the request context. A deadline landing between two of those statements
// -- which on a large graph is the common case, not the edge case -- surfaced
// as CTX_INTERNAL ("path scratch read: context deadline exceeded") instead of
// the truncated answer, so whether the contract held was a race on where the
// clock landed. The scratch is driven on an uncancellable context now and
// pathWalk.checkDeadline is the only stop; this row is what holds that.
//
// Mutation: pass ctx instead of w.scCtx to a scratch statement the walk reaches
// before its next checkDeadline -- the bucket seed in run, say -- and this test
// fails with CTX_INTERNAL rather than returning a truncated answer.
func TestPathDeadlineTruncatesRatherThanFails(t *testing.T) {
	g := pathProofGraph(512)
	limits := pathProofLimits()
	// Already past by the time the walk issues its first statement, wherever
	// in the search that lands.
	limits.QueryTimeout = time.Nanosecond
	e, err := New(Options{Adjacency: g, Limits: limits})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	res, err := e.ShortestPath(context.Background(), model.PathRequest{GenerationID: 1,
		From: g.node("s"), To: g.node("t"),
		Relations: []model.RelationKind{model.RelCalls, model.RelImports}})
	if err != nil {
		t.Fatalf("an expired deadline must truncate the answer, not fail the request: %v", err)
	}
	if !res.Meta.Truncated || res.Meta.TruncationReason != pathReasonDeadline {
		t.Fatalf("got truncated=%v reason=%q, want %q", res.Meta.Truncated, res.Meta.TruncationReason,
			pathReasonDeadline)
	}
}
