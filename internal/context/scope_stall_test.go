package context

import (
	stdcontext "context"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/Sawmonabo/codectx/internal/graph"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
)

// stallingAdjacency is the fixture's edge source with a clock the walk reads,
// stepped past the engine's query deadline on the FIRST round trip. That is the
// one shape no fixture in this package could produce before: a page the
// deadline cuts before the walk can advance at all, which graph.Impact answers
// truncated with NO continuation and NO entry (its cursor would be the one the
// request already carried, so it deliberately withholds it).
type stallingAdjacency struct {
	graph.Adjacency
	clock *time.Time
	calls *int
	jump  time.Duration
	// after is how many round trips run at the frozen clock before every one
	// after them steps past the deadline. The opening page must reach a real
	// keyset position -- a page that never advanced at all is exhaustion, not
	// a stall -- and no page after it may advance.
	after int
}

func (a stallingAdjacency) Edges(ctx stdcontext.Context, nodes []model.NodeID, dir model.Direction,
	kinds []model.RelationKind, after model.RelationID, limit int) ([]model.Relation, error) {
	*a.calls++
	if *a.calls > a.after {
		*a.clock = a.clock.Add(a.jump)
	}
	return a.Adjacency.Edges(ctx, nodes, dir, kinds, after, limit)
}

// TestAStalledWalkPageEndsThePassAndIsReIssuedOnce pins the stalled-page branch
// of P-A's halt (scope.go, `if stalled { next = cursor }`), which FX-H-X6 shipped
// reasoned rather than tested and REV-H4a carried as unpinned.
//
// Two things must hold for a page that came back truncated with an empty cursor
// and no entry. It must NOT read as exhaustion: breaking out of the walk loop
// there serves a fraction of the scope as a whole one. And it must not spin:
// the pass ends with the position it already had, so the NEXT request re-issues
// that one page rather than this call re-issuing it in a loop.
//
// Mutation (delete the `stalled` term, so halt is `stop() && next != ""`): the
// empty cursor falls through to the exhaustion break, passAIngest returns a
// FINISHED scope with halted=false, and the first assertion below fails.
func TestAStalledWalkPageEndsThePassAndIsReIssuedOnce(t *testing.T) {
	fx := newContextFixture(t)
	seed := fx.seedOf(fx.Specs[0].path)
	rels := []model.Relation{fx.edge(fx.Specs[0].path, model.RelCalls, fx.Specs[1].path)}

	clock := time.Now()
	calls := 0
	// The opening page runs at the frozen clock and stops at a real keyset
	// position; every round trip after it steps two minutes past a one-minute
	// deadline, so the page that resumes that position cannot advance.
	eng := stalledScopeEngine(t, fx, rels, &clock, &calls, 1)

	// Non-vacuity: the premise is that this fixture really does produce a
	// STALLED page -- truncated, no continuation, no entry. graph.Impact
	// withholds the cursor there deliberately: it would be the one the request
	// already carried.
	first, err := eng.Impact(fx.ctx, model.ImpactRequest{GenerationID: fx.Gen,
		Start: []model.NodeID{seed.NodeID}, Relations: scopeRelations,
		Direction: model.DirectionBoth, Page: model.PageRequest{Limit: 200}})
	if err != nil {
		t.Fatalf("probe page 1: %v", err)
	}
	if first.Meta.NextCursor == "" {
		t.Fatal("the opening page answered the whole walk, so no later page can stall")
	}
	probe, err := eng.Impact(fx.ctx, model.ImpactRequest{
		Start: []model.NodeID{seed.NodeID}, Relations: scopeRelations, Direction: model.DirectionBoth,
		Page: model.PageRequest{Limit: 200, Cursor: first.Meta.NextCursor}})
	if err != nil {
		t.Fatalf("probe page 2: %v", err)
	}
	if !probe.Meta.Truncated || probe.Meta.NextCursor != "" || len(probe.Entries) != 0 {
		t.Fatalf("the fixture's second page is not a stalled one (truncated=%v, cursor=%q, entries=%d), "+
			"so this row would pin the ordinary halt instead of the stall",
			probe.Meta.Truncated, probe.Meta.NextCursor, len(probe.Entries))
	}

	sorts := openSorts(t, fx.Cfg)
	c := &Compiler{cfg: fx.Cfg}
	ingest, err := c.newSeedIngest(sorts)
	if err != nil {
		t.Fatalf("newSeedIngest: %v", err)
	}
	if err := ingest.Admit(seed); err != nil {
		t.Fatalf("Admit: %v", err)
	}
	// The stop predicate is false for the opening page and true afterwards, so
	// the halt lands on the STALLED page and not on the ordinary one behind it
	// (two shipped rows already cover that one).
	calls, asked := 0, 0
	got, halted, err := c.passAIngest(fx.ctx, ingest, eng, fx.Gen, fixtureCapabilities,
		func() bool { asked++; return asked > 1 })
	if err != nil {
		t.Fatalf("passAIngest: %v", err)
	}
	if !halted {
		t.Fatalf("a walk page that stalled -- truncated, no cursor, no entry -- ended the pass as if the "+
			"walk were exhausted (halted=%v, ingested=%v). The scope it would publish is the fraction "+
			"the deadline reached, reported as the whole", halted, got != nil)
	}
	if got != nil {
		t.Fatal("the halted pass folded its sorts; a checkpoint must carry them unfolded")
	}
	// The pass re-issues nothing itself: it ends holding the position it
	// already had, and the NEXT request asks for that same page again.
	if ingest.cursor == "" {
		t.Fatal("the halted pass carries no position at all, so the request that resumes it would restart " +
			"the walk from its roots instead of re-issuing the page the deadline stalled")
	}
	// No re-issue loop: the halt question is asked once per PAGE, so two asks
	// are the opening page and the stalled one. A pass that re-issued the
	// stalled page in place would ask again for each attempt. (Round trips are
	// not the measure -- one page makes several, by design.)
	if asked != 2 {
		t.Fatalf("the pass read %d walk pages; the stalled page is the second, and a pass that read more "+
			"had re-issued it in a loop instead of ending", asked)
	}
}

// stalledScopeEngine is contextFixture.scopeEngine wired to a clock the
// adjacency steps, which scopeEngine deliberately does not offer: it passes the
// real clock so that its own rows are not cut by a frozen instant. The limits
// are the fixture's, so the walk under test is the walk production runs.
func stalledScopeEngine(t *testing.T, fx *contextFixture, rels []model.Relation,
	clock *time.Time, calls *int, after int) *graph.Engine {
	t.Helper()
	inner := &scopeAdjacency{binding: fx.Binding, nodes: map[model.NodeID]model.Node{},
		relations: rels, caps: fixtureCapabilities}
	for _, spec := range fx.Specs {
		fv := fx.File(spec.path)
		id := fx.Node(spec.path)
		inner.nodes[id] = model.Node{ID: id, Kind: spec.kind, Language: "go", Name: spec.symbol,
			QualifiedName: spec.path + "." + spec.symbol, FileID: fv.ID, ContentHash: fv.ContentHash}
	}
	sort.Slice(inner.relations, func(i, j int) bool { return inner.relations[i].ID < inner.relations[j].ID })
	dir := t.TempDir()
	signer, err := pagination.OpenSigner(dir)
	if err != nil {
		t.Fatalf("OpenSigner: %v", err)
	}
	spools, err := pagination.NewSpools(filepath.Join(dir, "spools"),
		fx.Cfg.Resources.MaxTempBytes/fixtureSpoolBudgetDivisor, fx.Store)
	if err != nil {
		t.Fatalf("NewSpools: %v", err)
	}
	eng, err := graph.New(graph.Options{
		Adjacency: stallingAdjacency{Adjacency: inner, clock: clock, calls: calls, jump: 2 * time.Minute, after: after},
		Signer:    signer, Spools: spools,
		Leases: pagination.NewLeases(fx.Store, pagination.DefaultCursorTTL),
		Limits: graph.Limits{
			MaxDepth:       fx.Cfg.Context.MaxGraphDepth.Int(),
			MaxVisited:     fx.Cfg.Context.MaxVisitedNodes.Int(),
			MaxEdges:       fx.Cfg.Context.MaxGraphEdges.Int(),
			MaxPageItems:   fx.Cfg.Resources.MaxPageItems,
			MaxReasonPaths: fx.Cfg.Context.MaxReasonPathsPerEntry.Int(),
			QueryTimeout:   time.Minute,
			CursorTTL:      fx.Cfg.Storage.QueryCursorTTL.Std(),
			FrontierBytes:  fx.Cfg.Resources.QueryMemoryBytes,
		},
		Now: func() time.Time { return *clock }})
	if err != nil {
		t.Fatalf("graph.New: %v", err)
	}
	return eng
}
