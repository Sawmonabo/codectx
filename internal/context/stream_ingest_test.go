package context

import (
	"reflect"
	"testing"

	"github.com/Sawmonabo/codectx/internal/config"
	"github.com/Sawmonabo/codectx/internal/graph"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
)

// TestStreamedIngestMatchesTheMapDedupe is the P-A invariant: replacing
// expandScope's `admitted map[string]bool` with a min-seq fold over an entityID
// sort (ruling C2) yields the SAME survivors, in the SAME admission order, with
// the same routes and the same scope verdict.
//
// It is one test over three scopes because one survivor set is one invariant:
// the seed-versus-seed dedupe, the seed-versus-boundary dedupe (scope.go:204,
// "the earliest step wins") and the boundary-versus-boundary one are the three
// shapes the map decided and the fold now decides.
//
// It also asserts the excluded records are on the ONE spool. Diverting them is
// what plan Section 2 P-A forbids, because P-I derives the exclusion projection
// by replaying this spool in seq order. Note for the reviewer: the brief's
// stated consequence -- "P-C's node set shrinks" -- does not hold for today's
// producers, because every site that sets Excluded (seeds.go:60,100,191) sets
// neither NodeID nor FileID and attaches no routes, so a diverted record
// removes nothing from P-C's node or relation sets. What it removes is the
// exclusion itself, which is what this asserts.
func TestStreamedIngestMatchesTheMapDedupe(t *testing.T) {
	t.Parallel()
	fx := newContextFixture(t)
	impl := "internal/order/service.go"
	rels := []model.Relation{
		fx.edge("internal/order/handler.go", model.RelCalls, impl),
		fx.edge(impl, model.RelImplements, "internal/order/ports.go"),
		fx.edge("internal/order/service_test.go", model.RelTests, impl),
		fx.edge("config/order.toml", model.RelConfigures, impl),
		fx.edge("docs/order.md", model.RelDocuments, impl),
	}
	changed := fx.seedOf(impl)
	changed.Origin = originChangedFile
	lexical := fx.seedOf("internal/order/handler.go")
	lexical.NodeID, lexical.Origin = "", originLexical
	lexical.Requirement = model.RequirementRecommended

	for _, row := range []struct {
		name  string
		eng   *graph.Engine
		seeds []candidate
	}{
		{
			// One entity from two Section 15.2 steps, and every boundary of it:
			// seed-vs-seed and seed-vs-boundary dedupe in one walk.
			name: "a duplicated seed and its boundaries",
			eng:  fx.scopeEngine(rels, fixtureCapabilities),
			seeds: []candidate{fx.seedOf(impl), changed,
				fx.seedOf("internal/order/ports.go")},
		},
		{
			// An unresolved token bypasses the dedupe entirely today and must
			// still reach the spool, in place, with its reason.
			name: "an unresolved token beside a resolved seed",
			eng:  fx.scopeEngine(rels, fixtureCapabilities),
			seeds: []candidate{
				{Path: "Ledger", Origin: originLexical,
					Excluded: "no symbol or path in the pinned snapshot matched"},
				fx.seedOf(impl),
			},
		},
		{
			// No symbol resolves, so the walk never runs: the early return must
			// finalize the spool exactly as the happy path does.
			name:  "a scope that resolves no symbol",
			eng:   fx.scopeEngine(nil, fixtureCapabilities),
			seeds: []candidate{lexical},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			want, err := expandScope(fx.ctx, row.eng, fx.Gen, fx.Cfg.Context, row.seeds, fixtureCapabilities)
			if err != nil {
				t.Fatalf("expandScope: %v", err)
			}
			got, sorts := streamScope(t, fx, row.eng, row.seeds)
			defer func() {
				if err := sorts.Close(); err != nil {
					t.Fatalf("releasing the sort area: %v", err)
				}
			}()

			cands := drainCandidates(t, got)
			if !reflect.DeepEqual(cands, want.Candidates) {
				t.Fatalf("the streamed survivors differ from the map's\n got %+v\nwant %+v", cands, want.Candidates)
			}
			got.Scope.Candidates = want.Candidates
			if !reflect.DeepEqual(got.Scope, want) {
				t.Fatalf("the streamed scope verdict = %+v, want %+v", got.Scope, want)
			}
			var excluded int
			for _, c := range cands {
				if c.Excluded != "" {
					excluded++
				}
			}
			var wantExcluded int
			for _, c := range want.Candidates {
				if c.Excluded != "" {
					wantExcluded++
				}
			}
			if excluded != wantExcluded {
				t.Fatalf("the spool carries %d excluded records, want %d: P-I derives the "+
					"exclusion projection by replaying this spool", excluded, wantExcluded)
			}
		})
	}
}

// TestStreamedHydrationCarriesBothPathValues is the P-B invariant and ruling
// C3's proof. The two path writes today's pipeline performs differ, and this
// row is the smallest fixture that distinguishes them: a candidate whose
// pre-set Path is NOT its snapshot path keeps that path for ranking
// (hydrateFiles fills Path only when empty, compiler.go:341) while budgeting
// sees the snapshot path (budget.go:254). Collapsing the two fields makes one
// of the two assertions below fail.
//
// No existing fixture produces such a candidate -- expansion entries carry no
// path of their own by design (scope.go:188-194) and seed producers record the
// snapshot path -- so it is constructed here rather than found.
//
// The same row proves the other two P-B outputs: the snapshot-absent flag
// buildPlan learns from a map miss, and that a batch-local file map hydrates
// exactly what hydrateFiles' whole-set one does.
func TestStreamedHydrationCarriesBothPathValues(t *testing.T) {
	t.Parallel()
	fx := newContextFixture(t)
	reader, err := fx.Store.PinGeneration(fx.ctx, fx.Repo, fx.Gen, fx.Cfg.Storage.QueryCursorTTL.Std())
	if err != nil {
		t.Fatalf("PinGeneration: %v", err)
	}
	defer reader.Close()

	impl := fx.Files["internal/order/service.go"]
	ports := fx.Files["internal/order/ports.go"]
	in := []candidate{
		{NodeID: "n-empty", FileID: impl.ID},
		{NodeID: "n-stale", FileID: ports.ID, Path: "internal/order/moved.go"},
		{NodeID: "n-gone", FileID: model.FileID("00000000000000000000000000000000000000000000000000000000000000ff")},
		{Path: "seed discovery: exact", Excluded: "the step stopped at its bound"},
	}
	c := &Compiler{cfg: fx.Cfg}
	sorts := openSorts(t, fx.Cfg)
	defer func() {
		if err := sorts.Close(); err != nil {
			t.Fatalf("releasing the sort area: %v", err)
		}
	}()
	spool := spoolOf(t, sorts, in)
	out, err := c.passBHydrate(fx.ctx, sorts, reader, spool)
	if err != nil {
		t.Fatalf("passBHydrate: %v", err)
	}
	var got []candRec
	if err := out.Each(func(r candRec) error { got = append(got, r); return nil }); err != nil {
		t.Fatalf("draining the hydrated spool: %v", err)
	}
	if len(got) != len(in) {
		t.Fatalf("hydration returned %d records, want %d", len(got), len(in))
	}
	if got[0].PathAtRank != impl.Path || got[0].PathFinal != impl.Path {
		t.Fatalf("a candidate with no path of its own got (%q, %q), want both %q",
			got[0].PathAtRank, got[0].PathFinal, impl.Path)
	}
	if got[0].SizeBytes != impl.Size || got[0].Status != impl.Status {
		t.Fatalf("hydration lost size/status: %d/%q", got[0].SizeBytes, got[0].Status)
	}
	if got[1].PathAtRank != "internal/order/moved.go" {
		t.Fatalf("ranking's path was overwritten with the snapshot path %q; packageOf, "+
			"centrality and the boost reason read this field (C3)", got[1].PathAtRank)
	}
	if got[1].PathFinal != ports.Path {
		t.Fatalf("budgeting's path = %q, want the snapshot path %q (budget.go:254)",
			got[1].PathFinal, ports.Path)
	}
	if !got[2].FileMissing {
		t.Fatal("a file the pinned snapshot does not hold was not flagged; P-G excludes it from this flag")
	}
	if got[3].FileMissing || got[3].PathFinal != "seed discovery: exact" {
		t.Fatalf("an excluded record was altered by hydration: %+v", got[3])
	}
}

// streamScope runs P-A over its own sort area and hands back both, so the
// caller releases the area after it has read the runs.
func streamScope(t *testing.T, fx *contextFixture, eng *graph.Engine, seeds []candidate) (*ingested, *compileSorts) {
	t.Helper()
	sorts := openSorts(t, fx.Cfg)
	c := &Compiler{cfg: fx.Cfg}
	got, err := c.passAIngest(fx.ctx, sorts, eng, fx.Gen, seeds, fixtureCapabilities)
	if err != nil {
		t.Fatalf("passAIngest: %v", err)
	}
	return got, sorts
}

func openSorts(t *testing.T, cfg config.Config) *compileSorts {
	t.Helper()
	sorts, err := newCompileSorts(cfg, t.TempDir())
	if err != nil {
		t.Fatalf("newCompileSorts: %v", err)
	}
	return sorts
}

// spoolOf builds a seq-ordered candidate spool from candidates, the shape P-A
// hands P-B.
func spoolOf(t *testing.T, sorts *compileSorts, cands []candidate) *pagination.SortedRun[candRec] {
	t.Helper()
	sorter, err := newSort[candRec](sorts, "test-spool", lessCandSeq, sizeOfCand)
	if err != nil {
		t.Fatalf("newSort: %v", err)
	}
	for i, c := range cands {
		if err := sorter.Add(candRecOf(c, int64(i))); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}
	run, err := sorter.Sorted()
	if err != nil {
		t.Fatalf("Sorted: %v", err)
	}
	return trackRun(sorts, run)
}

// drainCandidates rebuilds the candidates P-A produced, routes included: the
// record stream carries them as pathRec/hopRec keyed by the same seq, so this
// is also the check that no route was lost on the way onto those streams.
func drainCandidates(t *testing.T, in *ingested) []candidate {
	t.Helper()
	routes := map[int64][]model.RelationPath{}
	if err := in.Paths.Each(func(p pathRec) error {
		routes[p.Seq] = append(routes[p.Seq], model.RelationPath{
			CostUnits: p.CostUnits, Evidence: p.Evidence,
			Relations: make([]model.RelationID, 0, p.HopCount),
		})
		return nil
	}); err != nil {
		t.Fatalf("draining the route stream: %v", err)
	}
	if err := in.Hops.Each(func(h hopRec) error {
		rs := routes[h.Seq]
		if int(h.PathIdx) >= len(rs) {
			t.Fatalf("hop %+v names a route the path stream does not carry", h)
		}
		rs[h.PathIdx].Relations = append(rs[h.PathIdx].Relations, h.RelationID)
		return nil
	}); err != nil {
		t.Fatalf("draining the hop stream: %v", err)
	}
	var out []candidate
	if err := in.Cands.Each(func(r candRec) error {
		c := r.rankCandidate()
		c.Paths = routes[r.Seq]
		out = append(out, c)
		return nil
	}); err != nil {
		t.Fatalf("draining the candidate spool: %v", err)
	}
	return out
}
