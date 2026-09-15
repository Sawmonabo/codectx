package context

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/Sawmonabo/codectx/internal/config"
	"github.com/Sawmonabo/codectx/internal/model"
)

// The streamed budget passes (P-G, P-H, P-I) exist to produce byte-for-byte the
// plan buildPlan produces while holding none of the five candidate-sized
// structures it holds. The only proof that is worth anything is therefore a
// comparison against buildPlan itself over the same inputs: entries with their
// ordinals and measured sizes, slices with their membership and totals,
// exclusions with their ordinals AND their reasons, the clipped-route count,
// and the typed error when the budget cannot hold the required scope.
//
// The fixtures below are candidate-level rather than compiler-level on purpose:
// the wiring that would let a fixture reach here through Compile is L5's, and a
// parity test must be able to state the three pre-sort exclusion classes, the
// three drop reasons and the minimum-budget refusal directly.

// planCollector is the planSink the parity test measures against: it rebuilds
// today's `plan` value from the streamed entries and exclusions.
type planCollector struct{ p plan }

func (c *planCollector) Entry(e model.ContextEntry) error {
	c.p.Entries = append(c.p.Entries, e)
	return nil
}

func (c *planCollector) Exclude(e model.ExcludedContextEntry) error {
	c.p.Excluded = append(c.p.Excluded, e)
	return nil
}

// streamPlan runs P-G, P-H and P-I over the same inputs buildPlan takes. It
// stands in for the passes that produce their inputs: P-A's ingest sequence,
// P-B's hydration (both path fields and the snapshot-absent flag, exactly as
// hydrateFiles and buildPlan write them today), P-D's route streams and P-F's
// ranked sort.
func streamPlan(t *testing.T, cands []candidate, files []model.FileVersion, b resolvedBudget) (plan, error) {
	t.Helper()
	sorts, err := newCompileSorts(config.Config{}, t.TempDir())
	if err != nil {
		t.Fatalf("newCompileSorts: %v", err)
	}
	defer func() {
		if err := sorts.Close(); err != nil {
			t.Fatalf("releasing the compile sorts: %v", err)
		}
	}()

	ranked, err := newSort(sorts, "ranked", lessRank, sizeOfCand)
	if err != nil {
		t.Fatalf("ranked sort: %v", err)
	}
	pathSort, err := newSort(sorts, "paths", lessPathSeq, sizeOfPath)
	if err != nil {
		t.Fatalf("path sort: %v", err)
	}
	hopSort, err := newSort(sorts, "hops", lessHopSeq, sizeOfHop)
	if err != nil {
		t.Fatalf("hop sort: %v", err)
	}
	byID := map[model.FileID]model.FileVersion{}
	for _, fv := range files {
		byID[fv.ID] = fv
	}
	for seq, c := range cands {
		r := candRecOf(c, int64(seq))
		// hydrateFiles skips an excluded or fileless candidate and writes the
		// snapshot path only when the candidate carries none; buildPlan then
		// overwrites the path unconditionally on the surviving branch. That is
		// the split ruling C3 keeps as PathAtRank and PathFinal.
		if c.Excluded == "" && c.FileID != "" {
			fv, ok := byID[c.FileID]
			switch {
			case !ok:
				r.FileMissing = true
			default:
				r.SizeBytes, r.Status, r.PathFinal = fv.Size, fv.Status, fv.Path
				if r.PathAtRank == "" {
					r.PathAtRank = fv.Path
				}
			}
		}
		for pi, p := range c.Paths {
			if err := pathSort.Add(pathRec{Seq: int64(seq), PathIdx: int32(pi), CostUnits: p.CostUnits,
				Evidence: p.Evidence, HopCount: int32(len(p.Relations))}); err != nil {
				t.Fatalf("path add: %v", err)
			}
			for hi, rel := range p.Relations {
				if err := hopSort.Add(hopRec{RelationID: rel, Seq: int64(seq),
					PathIdx: int32(pi), HopIdx: int32(hi)}); err != nil {
					t.Fatalf("hop add: %v", err)
				}
			}
		}
		if err := ranked.Add(r); err != nil {
			t.Fatalf("ranked add: %v", err)
		}
	}
	rankedRun, err := sortedRun(sorts, ranked)
	if err != nil {
		t.Fatalf("ranked sorted: %v", err)
	}
	pathRun, err := sortedRun(sorts, pathSort)
	if err != nil {
		t.Fatalf("paths sorted: %v", err)
	}
	hopRun, err := sortedRun(sorts, hopSort)
	if err != nil {
		t.Fatalf("hops sorted: %v", err)
	}

	ctx := context.Background()
	c := &Compiler{}
	measured, err := c.passGMeasure(ctx, sorts, rankedStreams{Ranked: rankedRun, Paths: pathRun, Hops: hopRun})
	if err != nil {
		return plan{}, err
	}
	packed, err := c.passHPack(ctx, sorts, measured, b)
	if err != nil {
		return plan{}, err
	}
	var sink planCollector
	parts, err := c.passIEmit(ctx, sorts, measured, packed, b, &sink)
	if err != nil {
		return plan{}, err
	}
	out := sink.p
	out.Slices, out.RelationsClipped = parts.Slices, parts.RelationsClipped
	if parts.Entries != int64(len(out.Entries)) || parts.Excluded != int64(len(out.Excluded)) {
		t.Fatalf("P-I counted %d entries and %d exclusions, emitted %d and %d",
			parts.Entries, parts.Excluded, len(out.Entries), len(out.Excluded))
	}
	return out, nil
}

func fileVersion(id, path string, size int64) model.FileVersion {
	return model.FileVersion{ID: model.FileID(id), Path: path, Size: size, Status: model.FileTracked}
}

func TestStreamedBudgetMatchesTheWholeSetPlan(t *testing.T) {
	files := []model.FileVersion{
		// The file IDs deliberately sort AGAINST the rank order, so a walk in
		// file order and a walk in group order are distinguishable: the drops
		// and each slice's entry ordinals follow group order (ruling C1), and
		// a fixture whose two orders agreed would prove neither.
		fileVersion("zzz", "internal/order/service.go", 4096),
		fileVersion("mmm", "internal/order/handler.go", 2048),
		fileVersion("aaa", "internal/order/ports.go", 1024),
		fileVersion("bbb", "internal/order/huge.go", 1<<20),
	}
	routed := []model.RelationPath{
		{Relations: []model.RelationID{"r1", "r2"}, Evidence: []model.EvidenceID{"e1"}, CostUnits: 3},
		{Relations: []model.RelationID{"r3"}, CostUnits: 1},
	}
	// A route longer than model.MaxRelationsPerPath, so RelationsClipped is
	// exercised on a SELECTED entry and counted in phase two only.
	long := make([]model.RelationID, model.MaxRelationsPerPath+2)
	for i := range long {
		long[i] = model.RelationID("L" + string(rune('a'+i)))
	}

	base := []candidate{
		// An already-excluded candidate that OUTRANKS every survivor: `Index`
		// counts survivors, so a pass that numbered this record too would shift
		// every ordinal and every slice membership below it.
		{NodeID: "n-cut", FileID: "aaa", Path: "internal/order/ports.go",
			Requirement: model.RequirementFull, ScoreMicros: 2_000_000,
			Excluded: "the seed cut dropped this identity"},
		{FileID: "zzz", Path: "internal/order/service.go", Requirement: model.RequirementFull,
			Origin: originExplicitSeed, ScoreMicros: 1_130_000, Reasons: []string{"named by the task"}},
		{NodeID: "n-service-place", FileID: "zzz", Path: "internal/order/service.go",
			Requirement: model.RequirementSymbol, Origin: originQualified, StartByte: 120,
			ScoreMicros: 900_000, Paths: routed},
		{NodeID: "n-handler", FileID: "mmm", Path: "internal/order/handler.go",
			Requirement: model.RequirementRecommended, Origin: originExpansion, Depth: 1,
			ScoreMicros: 415_000, MorePaths: 2,
			Paths: []model.RelationPath{{Relations: long, CostUnits: 9}}},
		{NodeID: "n-ports", FileID: "aaa", Path: "internal/order/ports.go",
			Requirement: model.RequirementOptional, Origin: originLexical, ScoreMicros: 10_000},
		// The other two pre-sort exclusion classes, in ingest order.
		{NodeID: "n-nofile", Path: "unresolved.go", Requirement: model.RequirementOptional},
		{NodeID: "n-gone", FileID: "f9", Path: "internal/order/gone.go", Requirement: model.RequirementOptional},
	}
	withHuge := append(append([]candidate(nil), base...), candidate{
		NodeID: "n-huge", FileID: "bbb", Path: "internal/order/huge.go",
		Requirement: model.RequirementOptional, Origin: originLexical})

	cases := []struct {
		name   string
		cands  []candidate
		budget resolvedBudget
	}{
		{"every file fits one slice", base,
			resolvedBudget{MaxBytes: 1 << 20, MaxTokens: 1 << 20, MaxFiles: 8, MaxSlices: 8}},
		{"the file limit drops the lowest-ranked groups", base,
			resolvedBudget{MaxBytes: 1 << 20, MaxTokens: 1 << 20, MaxFiles: 2, MaxSlices: 8}},
		{"a small byte budget opens several slices", base,
			resolvedBudget{MaxBytes: 6000, MaxTokens: 1 << 20, MaxFiles: 8, MaxSlices: 8}},
		{"the slice limit drops every later group", base,
			resolvedBudget{MaxBytes: 6000, MaxTokens: 1 << 20, MaxFiles: 8, MaxSlices: 1}},
		{"a file too large for one slice is dropped alone", withHuge,
			resolvedBudget{MaxBytes: 1 << 19, MaxTokens: 1 << 20, MaxFiles: 8, MaxSlices: 8}},
		{"the token budget bounds a slice", base,
			resolvedBudget{MaxBytes: 1 << 20, MaxTokens: 1500, MaxFiles: 8, MaxSlices: 8}},
		{"a manifest cap the packed plan exceeds", base,
			resolvedBudget{MaxBytes: 1 << 20, MaxTokens: 1 << 20, MaxFiles: 8, MaxSlices: 8,
				MaxManifestBytes: config.Limit(64)}},
		{"the required scope does not fit one slice", base,
			resolvedBudget{MaxBytes: 512, MaxTokens: 1 << 20, MaxFiles: 8, MaxSlices: 8}},
		{"the required files outnumber the file budget", base,
			resolvedBudget{MaxBytes: 1 << 20, MaxTokens: 1 << 20, MaxFiles: 1, MaxSlices: 8}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			want, wantErr := buildPlan(append([]candidate(nil), tc.cands...), files, tc.budget)
			got, gotErr := streamPlan(t, tc.cands, files, tc.budget)
			if (wantErr == nil) != (gotErr == nil) {
				t.Fatalf("buildPlan error %v, streamed error %v", wantErr, gotErr)
			}
			if wantErr != nil {
				var a, b *model.Error
				if !errors.As(wantErr, &a) || !errors.As(gotErr, &b) {
					t.Fatalf("untyped errors: %v / %v", wantErr, gotErr)
				}
				if a.Code != b.Code || a.Message != b.Message || !reflect.DeepEqual(a.Details, b.Details) {
					t.Fatalf("streamed error %+v, want %+v", b, a)
				}
				return
			}
			if !reflect.DeepEqual(got.Entries, want.Entries) {
				t.Fatalf("entries differ:\n streamed %+v\n whole-set %+v", got.Entries, want.Entries)
			}
			if !reflect.DeepEqual(got.Slices, want.Slices) {
				t.Fatalf("slices differ:\n streamed %+v\n whole-set %+v", got.Slices, want.Slices)
			}
			if !reflect.DeepEqual(got.Excluded, want.Excluded) {
				t.Fatalf("exclusions differ:\n streamed %+v\n whole-set %+v", got.Excluded, want.Excluded)
			}
			if got.RelationsClipped != want.RelationsClipped {
				t.Fatalf("streamed RelationsClipped %d, want %d", got.RelationsClipped, want.RelationsClipped)
			}
			if len(want.Entries) == 0 || len(want.Excluded) == 0 {
				t.Fatalf("the fixture proves nothing: %d entries, %d exclusions",
					len(want.Entries), len(want.Excluded))
			}
		})
	}
}
