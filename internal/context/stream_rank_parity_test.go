package context

import (
	stdcontext "context"
	"fmt"
	"path"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/Sawmonabo/codectx/internal/config"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
	"github.com/Sawmonabo/codectx/internal/storage/sqlite"
)

// TestStreamedRankingMatchesInMemoryRank is the C-STREAM P-D/P-F parity proof.
//
// The streamed route pass and boost pass must produce, for every candidate, the
// SAME score, the same bounded reasons IN THE SAME ORDER, the same MorePaths
// count and the same retained routes as the whole-set `rank` this wave replaces.
// It is a parity table over the package's existing fixtures rather than a new
// golden file on purpose: a golden would freeze today's numbers, while this
// fails the moment the two pipelines disagree, whatever Section 15.3 says.
//
// The score and the reason set are the whole product of this pass: nothing
// downstream re-derives them, `context next` is an ordinal walk over the order
// they produce, and a silent divergence would not fail any compile -- it would
// quietly lead the caller with the wrong files.
func TestStreamedRankingMatchesInMemoryRank(t *testing.T) {
	fx := newContextFixture(t)
	reader, err := fx.Store.PinGeneration(fx.ctx, fx.Repo, fx.Gen, time.Minute)
	if err != nil {
		t.Fatalf("PinGeneration: %v", err)
	}
	defer reader.Close()

	impl, caller := fx.File("internal/order/service.go"), fx.File("internal/order/handler.go")
	callerNode := fx.Node("internal/order/handler.go")
	edge := model.NewRelationID(fx.Repo, callerNode, model.RelCalls, fx.Node("internal/order/service.go"))
	second := model.NewRelationID(fx.Repo, callerNode, model.RelImports, fx.Node("internal/order/service.go"))
	tested := model.NewRelationID(fx.Repo, callerNode, model.RelTests, fx.Node("internal/order/service.go"))
	// A well-formed relation id this compile never walked: it is absent from
	// rels, so its route must be inadmissible on both pipelines.
	unwalked := model.NewRelationID(fx.Repo, callerNode, model.RelExtends, fx.Node("internal/order/service.go"))
	rels := map[model.RelationID]model.Relation{
		edge:   {ID: edge, From: callerNode, Kind: model.RelCalls, To: fx.Node("internal/order/service.go")},
		second: {ID: second, From: callerNode, Kind: model.RelImports, To: fx.Node("internal/order/service.go")},
		tested: {ID: tested, From: callerNode, Kind: model.RelTests, To: fx.Node("internal/order/service.go")},
	}

	// The scenario row's own candidate set: a seed with a captured change, an
	// expansion reached over three routes of which one is too long to store,
	// and four root-package candidates that walk no edge at all.
	scenario := []candidate{
		{FileID: impl.ID, Path: impl.Path, Requirement: model.RequirementFull,
			Origin: originExplicitSeed, Status: impl.Status},
		{NodeID: callerNode, FileID: caller.ID, Path: caller.Path,
			Requirement: model.RequirementRecommended, Origin: originExpansion, Depth: 1,
			Status: caller.Status, Paths: []model.RelationPath{
				{Relations: []model.RelationID{edge}, CostUnits: 3},
				{Relations: []model.RelationID{second}, CostUnits: 5},
				{Relations: longRoute(edge, second)},
			}},
		{NodeID: "n2", Path: "a.go", StartByte: 5, Requirement: model.RequirementOptional, Origin: originLexical},
		{NodeID: "n3", Path: "a.go", Requirement: model.RequirementOptional, Origin: originLexical},
		{NodeID: "n1", Path: "b.go", Requirement: model.RequirementOptional, Origin: originLexical},
		{NodeID: "n1", Path: "a.go", StartByte: 5, Requirement: model.RequirementOptional, Origin: originLexical},
	}
	// A route over a tests edge, so the associated-test boost is exercised on
	// both sides and an unwalked hop keeps its route inadmissible.
	association := []candidate{
		{NodeID: callerNode, FileID: caller.ID, Path: caller.Path, Requirement: model.RequirementSymbol,
			Origin: originBacktick, Depth: 2, Status: caller.Status, Paths: []model.RelationPath{
				{Relations: []model.RelationID{tested, edge}, Evidence: []model.EvidenceID{"e1"}},
				{Relations: []model.RelationID{unwalked}},
			}},
		{FileID: impl.ID, Path: impl.Path, Requirement: model.RequirementOptional,
			Origin: originChangedFile, Status: impl.Status},
	}

	for _, tc := range []struct {
		name    string
		cands   []candidate
		pathLim int64
		// splitPath moves each candidate's PathFinal into a different
		// directory from its PathAtRank. No existing fixture distinguishes the
		// two C3 path values -- every candidate in the package's tests is built
		// with one Path -- so this is the smallest fixture that does: bucketing
		// centrality by PathFinal regroups the walked edges and changes the
		// boost, the score, and the reason that names the package.
		splitPath bool
	}{
		{name: "one retained route", cands: scenario, pathLim: 1},
		{name: "unlimited retained routes", cands: scenario, pathLim: 0},
		{name: "associated test route and an unwalked hop", cands: association, pathLim: 2},
		{name: "rank path differs from final path", cands: scenario, pathLim: 1, splitPath: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := fx.Cfg
			cfg.Context.MaxReasonPathsPerEntry = config.Limit(tc.pathLim)
			c := &Compiler{cfg: cfg}

			want, err := c.rank(fx.ctx, reader, copyCandidates(tc.cands), rels)
			if err != nil {
				t.Fatalf("rank: %v", err)
			}
			got, order := streamRank(t, fx.ctx, c, reader, copyCandidates(tc.cands), rels, tc.splitPath)

			if len(got) != len(want) {
				t.Fatalf("streamed ranking produced %d candidates, want %d", len(got), len(want))
			}
			for i, w := range want {
				g, ok := got[int64(i)]
				if !ok {
					t.Fatalf("streamed ranking lost candidate %d (%s)", i, w.entityID())
				}
				if g.ScoreMicros != w.ScoreMicros {
					t.Fatalf("candidate %d (%s): streamed score %d micros, in-memory %d",
						i, w.entityID(), g.ScoreMicros, w.ScoreMicros)
				}
				if !reflect.DeepEqual(g.Reasons, w.Reasons) {
					t.Fatalf("candidate %d (%s): streamed reasons\n %q\nin-memory\n %q",
						i, w.entityID(), g.Reasons, w.Reasons)
				}
				if g.MorePaths != w.MorePaths {
					t.Fatalf("candidate %d (%s): streamed MorePaths %d, in-memory %d",
						i, w.entityID(), g.MorePaths, w.MorePaths)
				}
				if !reflect.DeepEqual(g.Paths, w.Paths) {
					t.Fatalf("candidate %d (%s): streamed routes\n %+v\nin-memory\n %+v",
						i, w.entityID(), g.Paths, w.Paths)
				}
			}
			// The ranked sort's ORDER is the deliverable, not just its
			// contents: Task 16's `context next` is an ordinal walk over it and
			// P-G assigns each survivor's Index from it, so one record added
			// before its score was written shifts every later ordinal. On these
			// rows PathFinal equals the path `rank` sorts on, so the streamed
			// order must be exactly the in-memory result under candidate.less.
			// The splitPath row is excluded: its PathFinal is synthetic, so the
			// two orders legitimately differ.
			if tc.splitPath {
				return
			}
			ordered := copyCandidates(want)
			sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].less(ordered[j]) })
			if len(order) != len(ordered) {
				t.Fatalf("the ranked sort answered %d records, want %d", len(order), len(ordered))
			}
			for i, seq := range order {
				if got[seq].entityID() != ordered[i].entityID() || got[seq].Path != ordered[i].Path {
					t.Fatalf("ranked[%d] = {%s %s}, want {%s %s}", i,
						got[seq].Path, got[seq].entityID(), ordered[i].Path, ordered[i].entityID())
				}
			}
		})
	}
}

// copyCandidates gives each pipeline its own slice, because rank scores in
// place. The copy is shallow, which is safe only while no fixture candidate
// carries pre-set Reasons: appendReason appends, so a shared backing array
// would let one side's reasons appear on the other's.
func copyCandidates(cands []candidate) []candidate {
	out := make([]candidate, len(cands))
	copy(out, cands)
	return out
}

// streamRank runs P-D, P-E and P-F over the same inputs `rank` reads and answers
// the ranked candidates by their ingest pathRec/hopRec streams P-D emitted.
//
// It is a FIXTURE harness: hopsBySeq below accumulates every retained hop in a
// map, which is fine for a handful of candidates and is not the shape the
// wave's heap assertion may reuse.
//
// P-E is spelled out here rather than called: passECentrality belongs to lane
// L2 and is still a stub. It is the frozen folds and nothing else
// (foldPkgEdgeDistinct then foldPkgCount), and this harness must swap to
// passECentrality once L2 lands so the proof covers the shipped aggregation.
func streamRank(t *testing.T, ctx stdcontext.Context, c *Compiler, reader *sqlite.PinnedReader,
	cands []candidate, rels map[model.RelationID]model.Relation, splitPath bool) (map[int64]candidate, []int64) {
	t.Helper()
	precision, err := c.resolvePrecision(ctx, reader, cands)
	if err != nil {
		t.Fatalf("resolvePrecision: %v", err)
	}
	sorts, err := newCompileSorts(c.cfg, t.TempDir())
	if err != nil {
		t.Fatalf("newCompileSorts: %v", err)
	}
	defer func() {
		if err := sorts.Close(); err != nil {
			t.Fatalf("releasing the compile sort area: %v", err)
		}
	}()

	// P-A/P-B stand in: the candidate, route and hop streams as the ingest,
	// hydrate and attribute passes will hand them over.
	candSort := mustSort(t, sorts, "cand", func(a, b candRec) int { return cmpInt(a.Seq, b.Seq) }, sizeOfCand)
	routeSort := mustSort(t, sorts, "path", lessPathSeq, sizeOfPath)
	hopSort := mustSort(t, sorts, "hop", lessHopSeq, sizeOfHop)
	for seq, cand := range cands {
		rec := candRecOf(cand, int64(seq))
		if splitPath {
			// A path whose directory differs per candidate, so bucketing
			// centrality by PathFinal regroups every walked edge.
			rec.PathFinal = fmt.Sprintf("final%d/%s", seq, path.Base(cand.Path))
		}
		mustAdd(t, candSort.Add(rec))
		for pathIdx, p := range cand.Paths {
			mustAdd(t, routeSort.Add(pathRec{Seq: int64(seq), PathIdx: int32(pathIdx),
				CostUnits: p.CostUnits, Evidence: p.Evidence, HopCount: int32(len(p.Relations))}))
			for hopIdx, id := range p.Relations {
				mustAdd(t, hopSort.Add(hopRec{RelationID: id, Seq: int64(seq),
					PathIdx: int32(pathIdx), HopIdx: int32(hopIdx),
					Kind: rels[id].Kind, Multiplier: precision[id]}))
			}
		}
	}
	candRun, routeRun, hopRun := mustSorted(t, sorts, candSort), mustSorted(t, sorts, routeSort), mustSorted(t, sorts, hopSort)

	retainedPaths := mustSort(t, sorts, "kept-path", lessPathSeq, sizeOfPath)
	retainedHops := mustSort(t, sorts, "kept-hop", lessHopSeq, sizeOfHop)
	edgeSort := mustSort(t, sorts, "pkg-edge", lessPkgEdge, sizeOfPkgEdge).WithFold(foldPkgEdgeDistinct)

	scoredRun, err := c.passDRouteScoring(ctx, sorts, candRun, routeRun, hopRun,
		retainedPaths, retainedHops, edgeSort)
	if err != nil {
		t.Fatalf("passDRouteScoring: %v", err)
	}

	// P-E, spelled with the frozen folds until lane L2's passECentrality lands.
	edgeRun := mustSorted(t, sorts, edgeSort)
	countSort := mustSort(t, sorts, "pkg-count", lessPkg, sizeOfPkgCount).WithFold(foldPkgCount)
	if err := edgeRun.Each(func(e pkgEdgeRec) error {
		return countSort.Add(pkgCountRec{Pkg: e.Pkg, Edges: 1})
	}); err != nil {
		t.Fatalf("counting package edges: %v", err)
	}
	countRun := mustSorted(t, sorts, countSort)

	rankedSort := mustSort(t, sorts, "ranked", lessRank, sizeOfCand)
	if err := c.passFBoosts(ctx, scoredRun, countRun, rankedSort); err != nil {
		t.Fatalf("passFBoosts: %v", err)
	}
	rankedRun := mustSorted(t, sorts, rankedSort)

	routes := map[int64][]model.RelationPath{}
	keptRoutes, keptHops := mustSorted(t, sorts, retainedPaths), mustSorted(t, sorts, retainedHops)
	hopsBySeq := map[int64][]hopRec{}
	if err := keptHops.Each(func(h hopRec) error {
		hopsBySeq[h.Seq] = append(hopsBySeq[h.Seq], h)
		return nil
	}); err != nil {
		t.Fatalf("reading the retained hop stream: %v", err)
	}
	if err := keptRoutes.Each(func(r pathRec) error {
		ws, err := rebuildRoutes(r.Seq, []pathRec{r}, hopsBySeq[r.Seq])
		if err != nil {
			// rebuildRoutes checks the whole candidate at once; here one route
			// is rebuilt at a time, so its own hops are selected first.
			return err
		}
		routes[r.Seq] = append(routes[r.Seq], ws.paths...)
		return nil
	}); err != nil {
		t.Fatalf("reading the retained route stream: %v", err)
	}

	out := map[int64]candidate{}
	var order []int64
	if err := rankedRun.Each(func(rec candRec) error {
		cand := rec.rankCandidate()
		cand.Paths = routes[rec.Seq]
		out[rec.Seq] = cand
		order = append(order, rec.Seq)
		return nil
	}); err != nil {
		t.Fatalf("reading the ranked stream: %v", err)
	}
	return out, order
}

func mustSort[T any](t *testing.T, s *compileSorts, name string, compare func(a, b T) int,
	sizeOf func(T) int64) *pagination.ExternalSort[T] {
	t.Helper()
	sorter, err := newSort(s, name, compare, sizeOf)
	if err != nil {
		t.Fatalf("newSort(%s): %v", name, err)
	}
	return sorter
}

func mustSorted[T any](t *testing.T, s *compileSorts, sorter *pagination.ExternalSort[T]) *pagination.SortedRun[T] {
	t.Helper()
	run, err := sorter.Sorted()
	if err != nil {
		t.Fatalf("Sorted: %v", err)
	}
	return trackRun(s, run)
}

func mustAdd(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
}
