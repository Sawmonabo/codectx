package graph

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/paced"
	"github.com/Sawmonabo/codectx/internal/pagination"
	"github.com/Sawmonabo/codectx/internal/scratch"
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

// reader is the packed adjacency the search reads structure through. The
// Adjacency methods above stay because the ANSWER is still hydrated through
// them (nodes and evidence); only the adjacency reads moved.
func (g *pathGraph) reader() *MemoryGraph {
	nodes := make([]model.Node, 0, len(g.nodes))
	for _, n := range g.nodes {
		nodes = append(nodes, n)
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].ID < nodes[j].ID })
	return NewMemoryGraph(g.Binding(), nodes, append([]model.Relation(nil), g.rels...))
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
		e, err := New(Options{Adjacency: g, Reader: g.reader(), Limits: limits})
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
		// NOT sorted: pathWalk.routes promises a served ORDER, and sorting
		// here would assert only the set. referenceRoutes returns its routes in
		// that same canonical order, so the comparison below is an order
		// assertion as well as a set one.
		got := routeSequences(res)
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
		reader := g.reader()
		kinds := []model.RelationKind{model.RelCalls, model.RelImports}
		codes, absent := kindCodesFor(reader, kinds)
		w := &pathWalk{reader: reader, kinds: kinds, kindCodes: codes, kindsAbsent: absent,
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
// the one walk whose state is a DATABASE. Were every statement it issues to
// carry the request context, a deadline landing between two of them -- which on
// a large graph is the common case, not the edge case -- would surface as
// CTX_INTERNAL ("path scratch read: context deadline exceeded") instead of the
// truncated answer, and whether the contract held would be a race on where the
// clock landed. The scratch is driven on an uncancellable context and
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
	e, err := New(Options{Adjacency: g, Reader: g.reader(), Limits: limits})
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

// pathPagingEngine builds an engine that can mint and honour path
// continuations: a signer, a lease store and a spool store whose directory is
// also where the retained search state lands, which is what the leak check
// below reads.
func pathPagingEngine(t *testing.T, g *pathGraph, reader GraphReader, dir string, store *fixtureLeases, maxVisited int) *Engine {
	t.Helper()
	signer, err := pagination.OpenSigner(t.TempDir())
	if err != nil {
		t.Fatalf("open signer: %v", err)
	}
	spools, err := pagination.NewSpools(dir, 8<<20, store)
	if err != nil {
		t.Fatalf("new spools: %v", err)
	}
	limits := pathProofLimits()
	limits.MaxVisited = maxVisited
	e, err := New(Options{Adjacency: g, Reader: reader, Signer: signer, Spools: spools,
		Leases: pagination.NewLeases(store, limits.CursorTTL), Limits: limits})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	return e
}

// retainedDirs counts the state directories the spool store is holding. Zero is
// the only acceptable number once a search has ended.
//
// The scratch pool is not one of them: a search's working database is a
// surface the store keeps and the next search writes over, so it is there
// before the first query and after the last one by design.
func retainedDirs(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read spool dir: %v", err)
	}
	pool := filepath.Base(scratch.Dir(dir))
	n := 0
	for _, e := range entries {
		if e.IsDir() && e.Name() != pool {
			n++
		}
	}
	return n
}

// TestPathResumesAcrossPagesWithTheSameAnswer is the P5 continuation proof: a
// per-page visited budget small enough to force several pages must change only
// how many requests the answer takes, never the answer. The final page's routes
// and cost are compared against the same search run in one page, which the
// exactness proof above has already tied to the unbounded in-memory reference.
//
// Mutation (drop the resume position -- resume every page from the start of the
// cost bucket instead of from the cursor's last settled node, or drop the
// `pending` work queue so a node settled by one page is never expanded by the
// next) makes this FAIL: the search either loops without converging or serves a
// route list that is short of the single-page one.
func TestPathResumesAcrossPagesWithTheSameAnswer(t *testing.T) {
	g := pathProofGraph(64)
	from, to := g.node("s"), g.node("t")
	kinds := []model.RelationKind{model.RelCalls, model.RelImports}

	whole := pathPagingEngine(t, g, g.reader(), t.TempDir(), newFixtureLeases(), 0)
	want, err := whole.ShortestPath(context.Background(), model.PathRequest{GenerationID: 1,
		From: from, To: to, Relations: kinds})
	if err != nil {
		t.Fatalf("single-page search: %v", err)
	}
	if want.Meta.Truncated || len(want.Paths) == 0 {
		t.Fatalf("the single-page search must be a complete answer: truncated=%v paths=%d",
			want.Meta.Truncated, len(want.Paths))
	}

	dir := t.TempDir()
	// Three settled nodes per page: the proof graph needs far more than that
	// before the target settles, so the answer can only arrive in pages.
	e := pathPagingEngine(t, g, g.reader(), dir, newFixtureLeases(), 3)
	var got model.PathResult
	cursor := ""
	pages := 0
	for {
		pages++
		if pages > 200 {
			t.Fatalf("the search did not converge in %d pages", pages)
		}
		req := model.PathRequest{From: from, To: to, Relations: kinds}
		if cursor == "" {
			req.GenerationID = 1
		} else {
			req.Page = model.PageRequest{Cursor: cursor}
		}
		got, err = e.ShortestPath(context.Background(), req)
		if err != nil {
			t.Fatalf("page %d: %v", pages, err)
		}
		if got.Meta.NextCursor == "" {
			break
		}
		if !got.Meta.Truncated {
			t.Fatalf("page %d hands back a cursor but is not marked truncated", pages)
		}
		cursor = got.Meta.NextCursor
	}
	if pages < 3 {
		t.Fatalf("the per-page budget must force at least 3 pages; it took %d", pages)
	}
	if got.Meta.Truncated {
		t.Fatalf("the final page must be a complete answer, got reason %q", got.Meta.TruncationReason)
	}
	if gotSeq, wantSeq := routeSequences(got), routeSequences(want); !reflect.DeepEqual(gotSeq, wantSeq) {
		t.Fatalf("the paged search answered %v over %d pages;\nthe single-page search says %v",
			gotSeq, pages, wantSeq)
	}
	if got.VisitedCount < want.VisitedCount {
		t.Fatalf("the paged search reports %d visited cumulatively; the single-page search spent %d",
			got.VisitedCount, want.VisitedCount)
	}
	// The retained state belongs to the pages, not to the answer: once the
	// final page has arrived nothing of the search is left on disk.
	if n := retainedDirs(t, dir); n != 0 {
		t.Fatalf("the completed search left %d retained state directories behind", n)
	}
}

// TestPathDeadlineMintsAResumableCursor is ruling P3 for the path search: a
// deadline ends the PAGE, not the answer. The first page is given a deadline it
// cannot finish under, and the continuation it mints must complete the search.
//
// Mutation (end the answer on the deadline instead of retaining the state --
// the behaviour before this change) makes this FAIL at the missing cursor.
func TestPathDeadlineMintsAResumableCursor(t *testing.T) {
	g := pathProofGraph(64)
	from, to := g.node("s"), g.node("t")
	kinds := []model.RelationKind{model.RelCalls, model.RelImports}
	dir := t.TempDir()
	store := newFixtureLeases()
	// The deadline is landed by the graph itself, not by racing the clock: the
	// adjacency stalls past the whole query timeout on its second edge read, so
	// the next checkDeadline ALWAYS fires and this proof can never be skipped.
	// (`path` builds its deadline with context.WithDeadline from e.now(), which
	// the runtime enforces against the real clock, so the traversal's
	// clock-jumping slowAdjacency does not transfer here.) Landing the stop
	// EARLIER than the stall would satisfy every assertion below just as well
	// -- deadlineHit makes the walk resumable, so the page is truncated on the
	// deadline and mints a cursor either way -- so there is no flake window in
	// the other direction.
	stalled := false
	slow := slowPathGraph{MemoryGraph: g.reader(), calls: new(int), trigger: 2,
		stall: 4 * pathDeadlineProofTimeout, fired: &stalled}
	e := pathPagingEngine(t, g, slow, dir, store, 0)
	e.limits.QueryTimeout = pathDeadlineProofTimeout

	first, err := e.ShortestPath(context.Background(), model.PathRequest{GenerationID: 1,
		From: from, To: to, Relations: kinds})
	if err != nil {
		t.Fatalf("an expired deadline must end the page, not fail the request: %v", err)
	}
	// Which mechanism fired is a breadcrumb, not the contract: the reason check
	// below is what makes this non-vacuous.
	if !stalled {
		t.Logf("the deadline landed before the stall; the page's own reason is the proof")
	}
	if first.Meta.TruncationReason != pathReasonDeadline {
		t.Fatalf("a page whose search ran past the query timeout must be truncated on the deadline, got reason %q",
			first.Meta.TruncationReason)
	}
	if first.Meta.NextCursor == "" {
		t.Fatal("a deadline must end the page with a continuation, not the answer without one")
	}
	// The continuation runs under a deadline it can finish in.
	e.limits.QueryTimeout = time.Minute
	cursor := first.Meta.NextCursor
	for i := 0; i < 200; i++ {
		res, err := e.ShortestPath(context.Background(),
			model.PathRequest{From: from, To: to, Relations: kinds,
				Page: model.PageRequest{Cursor: cursor}})
		if err != nil {
			t.Fatalf("continuation %d: %v", i, err)
		}
		if res.Meta.NextCursor == "" {
			if len(res.Paths) == 0 {
				t.Fatal("the continuation completed the search but found no route")
			}
			if n := retainedDirs(t, dir); n != 0 {
				t.Fatalf("the completed search left %d retained state directories behind", n)
			}
			return
		}
		cursor = res.Meta.NextCursor
	}
	t.Fatal("the continuation never completed the search")
}

// TestExpiredPathLeaseSweepsTheRetainedState is the other half of the leak
// check: a continuation nobody follows must not pin its search state forever.
// The spool store sweeps a retained state DIRECTORY on lease expiry exactly as
// it sweeps a spool file, which is what pagination.Spools.AdoptDir buys.
//
// Mutation (restore `e.IsDir()` to Sweep's skip list) makes this FAIL: the
// directory survives the sweep.
func TestExpiredPathLeaseSweepsTheRetainedState(t *testing.T) {
	g := pathProofGraph(64)
	dir := t.TempDir()
	store := newFixtureLeases()
	e := pathPagingEngine(t, g, g.reader(), dir, store, 3)
	res, err := e.ShortestPath(context.Background(), model.PathRequest{GenerationID: 1,
		From: g.node("s"), To: g.node("t"),
		Relations: []model.RelationKind{model.RelCalls, model.RelImports}})
	if err != nil {
		t.Fatalf("first page: %v", err)
	}
	if res.Meta.NextCursor == "" {
		t.Fatal("the per-page budget must end this page with a continuation")
	}
	if n := retainedDirs(t, dir); n != 1 {
		t.Fatalf("a page that ended early must retain exactly one state directory, got %d", n)
	}
	spools, err := pagination.NewSpools(dir, 8<<20, store)
	if err != nil {
		t.Fatalf("new spools: %v", err)
	}
	// Every lease this search took is long past.
	if _, err := spools.Sweep(context.Background(), time.Now().Add(365*24*time.Hour)); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n := retainedDirs(t, dir); n != 0 {
		t.Fatalf("the sweep left %d retained state directories behind an expired lease", n)
	}
}

// pathDeadlineProofTimeout is the query timeout the deadline proofs run under.
// It is short enough that stalling past it costs the test well under a second
// and long enough that an ordinary scheduling hiccup cannot expire it before
// the stall does.
const pathDeadlineProofTimeout = 150 * time.Millisecond

// TestADeadlineInterruptedPathPageChargesEachEntryOnce holds the edge charge to
// the entries a page actually took. The port reports a scan stopped from the
// callback as the entry it last DELIVERED, and the resumed scan starts there,
// so an entry charged before the stop decision is charged again by the page
// that resumes -- a search with a finite edge budget then spends it on entries
// it already paid for and stops converging. Both stops are therefore decided
// before the charge: the page charges every entry it delivered except the one
// it stopped on, which is the next page's first.
//
// The deadline is landed inside one node's list by the reader itself, after
// exactly one relaxation batch, so the stop falls on a batch boundary -- where
// the saved position is consistent with the buckets -- and never on a race.
//
// Mutation (charge `w.spent++`/`w.pageEdges++` before the deadline check, as
// the code did when the check sat after the batch flush): the stopped entry is
// charged by this page as well as the next and the cursor's edge count is one
// too many.
func TestADeadlineInterruptedPathPageChargesEachEntryOnce(t *testing.T) {
	// `s` owns more than two relaxation batches of entries in this fixture, so
	// the stall below lands inside its list rather than between two scans.
	g := pathProofGraph(512)
	delivered, fired := 0, false
	reader := stallingPathGraph{MemoryGraph: g.reader(), delivered: &delivered,
		after: adjacencyBatch, stall: 4 * pathDeadlineProofTimeout, fired: &fired}
	e := pathPagingEngine(t, g, reader, t.TempDir(), newFixtureLeases(), 0)
	e.limits.QueryTimeout = pathDeadlineProofTimeout

	first, err := e.ShortestPath(context.Background(), model.PathRequest{GenerationID: 1,
		From: g.node("s"), To: g.node("t"),
		Relations: []model.RelationKind{model.RelCalls, model.RelImports}})
	if err != nil {
		t.Fatalf("an expired deadline must end the page, not fail the request: %v", err)
	}
	if !fired {
		t.Fatal("the reader never stalled; the page was not interrupted inside a scan")
	}
	if first.Meta.TruncationReason != pathReasonDeadline || first.Meta.NextCursor == "" {
		t.Fatalf("want a deadline-truncated page with a continuation, got reason %q cursor %q",
			first.Meta.TruncationReason, first.Meta.NextCursor)
	}
	payload, err := e.signer.Verify(first.Meta.NextCursor, pagination.PurposeCursor, e.now())
	if err != nil {
		t.Fatalf("verify cursor: %v", err)
	}
	var c pathCursor
	if err := json.Unmarshal(payload, &c); err != nil {
		t.Fatalf("decode cursor: %v", err)
	}
	if want := int64(delivered - 1); c.Edges != want {
		t.Fatalf("the page delivered %d entries and charged %d; it must charge %d -- every entry "+
			"but the one it stopped on, which the next page charges", delivered, c.Edges, want)
	}
}

// stallingPathGraph expires the search's deadline INSIDE one adjacency scan,
// after a fixed number of delivered entries, and counts everything the port
// hands the walk. slowPathGraph below stalls between scans, which is a
// different stop: the port checks the context per owner and delivers nothing.
type stallingPathGraph struct {
	*MemoryGraph
	delivered *int
	after     int
	stall     time.Duration
	fired     *bool
}

func (s stallingPathGraph) Neighbours(ctx context.Context, refs []NodeRef, dir model.Direction,
	kinds []KindCode, from EdgePos, fn func(Edge) error) (EdgePos, error) {
	return s.MemoryGraph.Neighbours(ctx, refs, dir, kinds, from, func(e Edge) error {
		*s.delivered++
		if !*s.fired && *s.delivered > s.after {
			*s.fired = true
			time.Sleep(s.stall)
		}
		return fn(e)
	})
}

// slowPathGraph is the deterministic deadline hook for the path search. The
// traversal has one already (slowAdjacency, frontier_test.go), but it works by
// jumping the engine's injected clock, and the path search's stop is a real
// context deadline -- so here the stall is real time, fired once at a fixed
// adjacency scan. It hooks the READER, which is where the search's structure
// reads now live, and embeds *MemoryGraph so every other port method is the
// fixture's own.
type slowPathGraph struct {
	*MemoryGraph
	calls   *int
	trigger int
	stall   time.Duration
	fired   *bool
}

func (s slowPathGraph) Neighbours(ctx context.Context, refs []NodeRef, dir model.Direction,
	kinds []KindCode, from EdgePos, fn func(Edge) error) (EdgePos, error) {
	*s.calls++
	if !*s.fired && *s.calls >= s.trigger {
		*s.fired = true
		time.Sleep(s.stall)
	}
	return s.MemoryGraph.Neighbours(ctx, refs, dir, kinds, from, fn)
}

// busyPathGraph fails one adjacency scan of the RESUMED page with a retryable
// CTX_WORKSPACE_BUSY, the way a contended store does.
type busyPathGraph struct {
	*MemoryGraph
	armed *bool
}

func (b busyPathGraph) Neighbours(ctx context.Context, refs []NodeRef, dir model.Direction,
	kinds []KindCode, from EdgePos, fn func(Edge) error) (EdgePos, error) {
	if *b.armed {
		*b.armed = false
		return from, &model.Error{Code: model.CodeWorkspaceBusy, Retryable: true,
			Message: "storage: another writer holds the workspace"}
	}
	return b.MemoryGraph.Neighbours(ctx, refs, dir, kinds, from, fn)
}

// TestRetryableFailureLeavesAPathContinuationAdoptable is the SK3/A15 proof.
// ShortestPath releases the continuation it consumed on the way out. The
// failure mode: releasing it on EVERY exit, so one transient
// CTX_WORKSPACE_BUSY on one page turns the next presentation of that same
// cursor into CTX_CURSOR_INVALID and an hours-long search behind it is
// unrecoverable -- while the error itself tells the caller to retry.
//
// The retry must also see the state the failing page STARTED from, not a page
// torn off halfway through, so the routes it finishes with are compared against
// the same search run in a single page.
//
// Mutation (the terminalOutcome guard over the scratch in path.go removed): the
// retry fails with CTX_CURSOR_INVALID, which is the assertion this test leads
// with.
func TestRetryableFailureLeavesAPathContinuationAdoptable(t *testing.T) {
	g := pathProofGraph(64)
	from, to := g.node("s"), g.node("t")
	kinds := []model.RelationKind{model.RelCalls, model.RelImports}

	whole := pathPagingEngine(t, g, g.reader(), t.TempDir(), newFixtureLeases(), 0)
	want, err := whole.ShortestPath(context.Background(), model.PathRequest{GenerationID: 1,
		From: from, To: to, Relations: kinds})
	if err != nil {
		t.Fatalf("single-page search: %v", err)
	}

	dir := t.TempDir()
	armed := false
	busy := busyPathGraph{MemoryGraph: g.reader(), armed: &armed}
	// Three settled nodes per page, so the first page ends early and hands
	// back a continuation over retained search state.
	e := pathPagingEngine(t, g, busy, dir, newFixtureLeases(), 3)

	first, err := e.ShortestPath(context.Background(), model.PathRequest{GenerationID: 1,
		From: from, To: to, Relations: kinds})
	if err != nil {
		t.Fatalf("first page: %v", err)
	}
	cursor := first.Meta.NextCursor
	if cursor == "" {
		t.Fatal("the per-page budget must end this page with a continuation")
	}

	// The resumed page hits a contended store.
	armed = true
	resume := model.PathRequest{From: from, To: to, Relations: kinds,
		Page: model.PageRequest{Cursor: cursor}}
	_, err = e.ShortestPath(context.Background(), resume)
	var typed *model.Error
	if !errors.As(err, &typed) || typed.Code != model.CodeWorkspaceBusy {
		t.Fatalf("the resumed page must surface the store's retryable failure, got %v", err)
	}
	if armed {
		t.Fatal("the busy failure was never reached: the page did not read an edge")
	}
	if n := retainedDirs(t, dir); n != 1 {
		t.Fatalf("a retryable failure must leave the search's retained state in place, found %d directories", n)
	}

	// The SAME cursor, which is exactly what a retryable error tells the caller
	// to present, and the search carries on to its answer.
	for i := 0; i < 200; i++ {
		res, err := e.ShortestPath(context.Background(), resume)
		if err != nil {
			t.Fatalf("retry %d after a retryable failure: %v", i, err)
		}
		if res.Meta.NextCursor == "" {
			if len(res.Paths) == 0 {
				t.Fatal("the retried continuation completed the search but found no route")
			}
			// The abandoned page was rolled back, so the retry resumed from the
			// state the cursor names: the answer is the whole search's, not one
			// missing the routes through the half-written page.
			if gotSeq, wantSeq := routeSequences(res), routeSequences(want); !reflect.DeepEqual(gotSeq, wantSeq) {
				t.Fatalf("the search retried through a busy page answered %v;\nthe uninterrupted single-page search says %v",
					gotSeq, wantSeq)
			}
			if n := retainedDirs(t, dir); n != 0 {
				t.Fatalf("the completed search left %d retained state directories behind", n)
			}
			return
		}
		resume.Page = model.PageRequest{Cursor: res.Meta.NextCursor}
	}
	t.Fatal("the retried continuation never completed the search")
}

// TestPathResumesInsideOneNodeEdgeList is the invariant the packed adjacency
// introduced: a page whose EDGE budget runs out stops in the middle of one
// settled node's list, and the position it saves is an entry inside that list,
// not the node. Before the packed reader the position was a relation id and
// every batch re-ran its own keyset; now it is an (owner, list index) pair, and
// the index is the half that can be lost silently -- a resumed page that keeps
// only the owner re-reads and RE-CHARGES the entries the page before it already
// spent, which is how a search with a finite edge budget stops converging.
//
// The answer, the route list and the cumulative edge count are all compared
// against the same search run in a single page, so neither a lost entry nor a
// doubly charged one passes.
//
// Mutation (`w.expandAfter = pos` -> `w.expandAfter = EdgePos{Node: pos.Node}`
// in expandBatch, which keeps the owner and drops the list index): every
// resumed page re-reads its owner's list from the beginning, the edge budget
// is spent on the same entries forever, and the search never converges.
func TestPathResumesInsideOneNodeEdgeList(t *testing.T) {
	g := pathProofGraph(64)
	from, to := g.node("s"), g.node("t")
	kinds := []model.RelationKind{model.RelCalls, model.RelImports}

	whole := pathPagingEngine(t, g, g.reader(), t.TempDir(), newFixtureLeases(), 0)
	want, err := whole.ShortestPath(context.Background(), model.PathRequest{GenerationID: 1,
		From: from, To: to, Relations: kinds})
	if err != nil {
		t.Fatalf("single-page search: %v", err)
	}

	e := pathPagingEngine(t, g, g.reader(), t.TempDir(), newFixtureLeases(), 0)
	// `s` alone owns more than seven entries in this fixture, so every early
	// page ends INSIDE one node's list rather than between two nodes.
	e.limits.MaxEdges = 7
	var got model.PathResult
	cursor := ""
	pages := 0
	for {
		pages++
		if pages > 120 {
			t.Fatalf("the search did not converge in %d pages: the resumed scan is re-reading entries", pages)
		}
		req := model.PathRequest{From: from, To: to, Relations: kinds}
		if cursor == "" {
			req.GenerationID = 1
		} else {
			req.Page = model.PageRequest{Cursor: cursor}
		}
		got, err = e.ShortestPath(context.Background(), req)
		if err != nil {
			t.Fatalf("page %d: %v", pages, err)
		}
		if got.Meta.NextCursor == "" {
			break
		}
		cursor = got.Meta.NextCursor
	}
	if pages < 3 {
		t.Fatalf("the edge budget must force at least 3 pages; it took %d", pages)
	}
	if gotSeq, wantSeq := routeSequences(got), routeSequences(want); !reflect.DeepEqual(gotSeq, wantSeq) {
		t.Fatalf("the edge-budgeted search answered %v over %d pages;\nthe single-page search says %v",
			gotSeq, pages, wantSeq)
	}
	// Convergence is the double-charge detector, and it is asserted by the
	// page ceiling above rather than by a counter: the answer carries the
	// visited count but not the edge one, and a resumed scan that re-charges
	// the entries before its stop never gets past the first list at all.
}

// TestPathTieBreakSettlesOnCanonicalIdNotSurrogate is the equal-cost half of
// the scratch's ordering contract. The fixture offers two routes from n-a to
// n-z of exactly the same cost, so which one is reported is decided by the
// settle tie-break alone, and that tie-break is the CANONICAL node id.
//
// It needs a reader whose surrogates do not ascend with the canonical ids:
// while the two orders agree, a search that settled on the surrogate reports
// the same route as one that settled on the canonical id, and the invariant is
// unobservable. Reversing the surrogate assignment makes the two disagree on
// every node, so the route is the same one only if the canonical id decided it.
//
// Mutation (the relaxation inserts the settled endpoint's SURROGATE where its
// canonical node id belongs, so the bucket's (cost, node) order becomes a
// surrogate order): the reversed reader reports the other equal-cost route and
// this fails.
func TestPathTieBreakSettlesOnCanonicalIdNotSurrogate(t *testing.T) {
	f := newGraphFixture(t)
	nodes := make([]model.Node, 0, len(f.nodes))
	for _, n := range f.nodes {
		nodes = append(nodes, n)
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].ID < nodes[j].ID })
	relations := append([]model.Relation(nil), f.relations...)

	// ONE reported route, not three. The fixture's two routes are equal-cost
	// and the default reason-path bound reports BOTH, which breaks no tie at
	// all: the answer would be the same whatever the settle order did.
	limits := fixtureLimits()
	limits.MaxReasonPaths = 1

	route := func(order MemoryGraphOrder) []model.RelationID {
		t.Helper()
		e, err := New(Options{Adjacency: f, Limits: limits,
			Reader: NewMemoryGraphOrdered(f.binding, nodes, relations, order)})
		if err != nil {
			t.Fatalf("new engine: %v", err)
		}
		res, err := e.ShortestPath(context.Background(), model.PathRequest{GenerationID: 1,
			From: fixtureNodeID("n-a"), To: fixtureNodeID("n-z"),
			Relations: []model.RelationKind{model.RelCalls}})
		if err != nil {
			t.Fatalf("ShortestPath: %v", err)
		}
		if len(res.Paths) != 1 {
			t.Fatalf("got %d routes from n-a to n-z, want exactly one: two reported "+
				"equal-cost routes break no tie", len(res.Paths))
		}
		return res.Paths[0].Relations
	}

	canonical := route(MemoryGraphOrder{})
	reversed := route(MemoryGraphOrder{
		Nodes:     func(a, b model.NodeID) int { return -cmp.Compare(a, b) },
		Relations: func(a, b model.RelationID) int { return -cmp.Compare(a, b) },
	})
	if !slices.Equal(canonical, reversed) {
		t.Fatalf("the equal-cost tie settled on the surrogate order\n canonical: %v\n reversed:  %v",
			canonical, reversed)
	}
}

// pathSurfaces is every surface of the store's path-search pool, with its
// current length.
func pathSurfaces(t *testing.T, dir string) map[string]int64 {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(scratch.Dir(dir), "*", "path-search", "*"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	out := map[string]int64{}
	for _, m := range matches {
		st, err := os.Stat(m)
		if err != nil {
			t.Fatalf("stat %s: %v", m, err)
		}
		out[m] = st.Size()
	}
	return out
}

// TestAPathSearchReusesOneSurfaceAndFreesNothing is why the search's state is
// pooled.
//
// A shortest-path search holds its settled set, its parent edges and its cost
// buckets in a database on disk, and it used to create that database per query
// and remove it when the page ended. A store answering path queries therefore
// handed the filesystem one search's worth of deallocation per query, for
// ever. On a host that discards freed blocks under a sparse virtual disk that
// stalls every writer on the machine for about a minute, a minute later.
//
// The surface is taken from the store's pool and given back at its length.
// The second search must answer from the same surface, free nothing -- which
// the paced step count reports, a step being one window of disk handed back --
// and leave the surface no smaller than the first search left it.
//
// Mutation: empty the surface with CREATE TABLE IF NOT EXISTS instead of
// dropping the tables first and the second search fails outright, because the
// schema it re-applies collides with the one already in the surface. The
// silent form of that bug -- a surface whose settled set and parent edges
// survive into the next search, which then serves the previous query's routes
// -- is what the route comparison below catches.
func TestAPathSearchReusesOneSurfaceAndFreesNothing(t *testing.T) {
	g := pathProofGraph(64)
	from, to := g.node("s"), g.node("t")
	kinds := []model.RelationKind{model.RelCalls, model.RelImports}
	dir := t.TempDir()
	e := pathPagingEngine(t, g, g.reader(), dir, newFixtureLeases(), 0)
	req := model.PathRequest{GenerationID: 1, From: from, To: to, Relations: kinds}

	first, err := e.ShortestPath(context.Background(), req)
	if err != nil {
		t.Fatalf("first search: %v", err)
	}
	if first.Meta.Truncated || len(first.Paths) == 0 {
		t.Fatalf("the first search must be a complete answer: truncated=%v paths=%d",
			first.Meta.Truncated, len(first.Paths))
	}
	pool := pathSurfaces(t, e.scratchDir())
	if len(pool) != 1 {
		t.Fatalf("one search left %d surfaces in the pool, want the one it took: %v", len(pool), pool)
	}

	steps := paced.Steps()
	second, err := e.ShortestPath(context.Background(), req)
	if err != nil {
		t.Fatalf("second search: %v", err)
	}
	if freed := paced.Steps() - steps; freed != 0 {
		t.Fatalf("the second search freed %d windows of disk; a search that reuses its surface frees none", freed)
	}
	if gotSeq, wantSeq := routeSequences(second), routeSequences(first); !reflect.DeepEqual(gotSeq, wantSeq) {
		t.Fatalf("the second search answered %v from the reused surface; the first said %v", gotSeq, wantSeq)
	}
	after := pathSurfaces(t, e.scratchDir())
	if len(after) != 1 {
		t.Fatalf("the second search left %d surfaces, want the one it reused: %v", len(after), after)
	}
	for path, n := range after {
		was, ok := pool[path]
		if !ok {
			t.Fatalf("the second search took a new surface %s instead of the pooled %v", path, pool)
		}
		if n < was {
			t.Fatalf("pooled surface %s shrank from %d to %d bytes: it was emptied rather than reused", path, was, n)
		}
	}
}
