package context

import (
	"math/rand"
	"os"
	"reflect"
	"slices"
	"sort"
	"testing"

	"github.com/Sawmonabo/codectx/internal/config"
	"github.com/Sawmonabo/codectx/internal/model"
)

// TestStreamRecordRoundTrip is the codec invariant every streamed pass rests
// on: a record written to a run file and read back is the record that was
// written. A codec that lost one field would not fail a build -- it would
// silently drop a path, a flag or an index and change the plan.
func TestStreamRecordRoundTrip(t *testing.T) {
	t.Parallel()
	roundTrip(t, "candRec populated", candRec{
		Seq: 7, Index: 3, NodeID: "node-1", FileID: "file-1",
		PathAtRank: "pkg/old.go", PathFinal: "pkg/new.go",
		Requirement: model.RequirementSymbol, Origin: originLexical, Depth: 2,
		StartByte: 512, ScoreMicros: 640_000, SizeBytes: 4096,
		Status: model.FileStatus("modified"), Reasons: []string{"first", "second"},
		MorePaths: 9, Excluded: "no route", FileMissing: true,
	})
	roundTrip(t, "candRec zero", candRec{})
	roundTrip(t, "pathRec populated", pathRec{
		Seq: 4, PathIdx: 2, CostUnits: 31, Evidence: []model.EvidenceID{"ev-1", "ev-2"}, HopCount: 2,
	})
	roundTrip(t, "pathRec zero", pathRec{})
	roundTrip(t, "hopRec populated", hopRec{
		RelationID: "rel-9", Seq: 4, PathIdx: 2, HopIdx: 1,
		Kind: model.RelCalls, Multiplier: 900_000,
	})
	roundTrip(t, "hopRec zero", hopRec{})
	roundTrip(t, "relAttrRec populated", relAttrRec{RelationID: "rel-9", Kind: model.RelTests, Multiplier: 950_000})
	roundTrip(t, "relAttrRec zero", relAttrRec{})
	roundTrip(t, "pkgEdgeRec populated", pkgEdgeRec{Pkg: "internal/context", RelationID: "rel-3"})
	roundTrip(t, "pkgEdgeRec zero", pkgEdgeRec{})
	roundTrip(t, "pkgCountRec populated", pkgCountRec{Pkg: "internal/graph", Edges: 12})
	roundTrip(t, "pkgCountRec zero", pkgCountRec{})
	roundTrip(t, "groupRec populated", groupRec{
		FileID: "file-2", Path: "cmd/main.go", Required: true, Bytes: 900, Tokens: 300, MinIndex: 5, Count: 4,
	})
	roundTrip(t, "groupRec zero", groupRec{})
	roundTrip(t, "decisionRec populated", decisionRec{FileID: "file-2", Keep: true, SliceIndex: 3})
	roundTrip(t, "decisionRec zero", decisionRec{})
}

func roundTrip[T any](t *testing.T, name string, want T) {
	t.Helper()
	b, err := encodeRecord(want)
	if err != nil {
		t.Fatalf("%s: encode: %v", name, err)
	}
	got, err := decodeRecord[T](b)
	if err != nil {
		t.Fatalf("%s: decode: %v", name, err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s: round trip changed the record\n got: %+v\nwant: %+v", name, got, want)
	}
	// Encoding is deterministic: a run file written twice is the same bytes,
	// which is what makes two compiles of one generation identical.
	again, err := encodeRecord(want)
	if err != nil {
		t.Fatalf("%s: re-encode: %v", name, err)
	}
	if string(again) != string(b) {
		t.Fatalf("%s: encoding is not deterministic: %q then %q", name, b, again)
	}
}

// TestLessRankMatchesCandidateLess is the parity linchpin of the whole wave: the
// streamed pipeline sorts candRecs with lessRank where today's pipeline sorts
// candidates with candidate.less, so any disagreement between the two is a
// different plan, different ordinals and different slice membership.
//
// The table draws PathAtRank INDEPENDENTLY of PathFinal (ruling C3 keeps both,
// and buildPlan sorts AFTER the unconditional overwrite that sets PathFinal), so
// a lessRank that read the wrong one disagrees here; scores collide often enough
// that the path, start byte and entity keys are all reached; and an unset
// Requirement exercises requirementRank's sorts-last branch on both sides.
func TestLessRankMatchesCandidateLess(t *testing.T) {
	t.Parallel()
	recs := randomCandRecs(t, 220)

	pathReached, reqReached := false, false
	for i, a := range recs {
		for j, b := range recs {
			if i == j {
				continue
			}
			ca, cb := a.finalCandidate(), b.finalCandidate()
			got := lessRank(a, b)
			if got == 0 {
				t.Fatalf("lessRank is not total: %+v and %+v compare equal", a, b)
			}
			switch {
			case ca.less(cb):
				if got >= 0 {
					t.Fatalf("candidate.less says %d < %d, lessRank says %d", i, j, got)
				}
			case cb.less(ca):
				if got <= 0 {
					t.Fatalf("candidate.less says %d > %d, lessRank says %d", i, j, got)
				}
			default:
				// A tie under candidate.less: the two records share every key
				// it compares, and only lessRank's seq separates them.
				if want := cmpInt(a.Seq, b.Seq); (got < 0) != (want < 0) {
					t.Fatalf("tie between %d and %d is not broken by seq: got %d, want sign of %d", i, j, got, want)
				}
			}
			if requirementRank(a.Requirement) == requirementRank(b.Requirement) &&
				a.ScoreMicros == b.ScoreMicros && a.PathFinal != b.PathFinal {
				pathReached = true
			}
			if requirementRank(a.Requirement) == 4 || requirementRank(b.Requirement) == 4 {
				reqReached = true
			}
		}
	}
	if !pathReached {
		t.Fatal("the table never reached the path key, so it cannot show which path field lessRank reads")
	}
	if !reqReached {
		t.Fatal("the table never reached an unknown requirement")
	}

	// End to end: the two sorts produce the same sequence, which is the form
	// the pipeline actually depends on.
	byLess := slices.Clone(recs)
	sort.SliceStable(byLess, func(i, j int) bool {
		return byLess[i].finalCandidate().less(byLess[j].finalCandidate())
	})
	byRank := slices.Clone(recs)
	slices.SortStableFunc(byRank, lessRank)
	for i := range byLess {
		if byLess[i].Seq != byRank[i].Seq {
			t.Fatalf("sorted sequences diverge at %d: less gave seq %d, lessRank gave seq %d",
				i, byLess[i].Seq, byRank[i].Seq)
		}
	}
}

// randomCandRecs builds the parity table. Small key domains are deliberate: a
// table of distinct values would only ever exercise the first tie-break.
func randomCandRecs(t *testing.T, n int) []candRec {
	t.Helper()
	rng := rand.New(rand.NewSource(20260915))
	reqs := []model.Requirement{model.RequirementFull, model.RequirementSymbol,
		model.RequirementRecommended, model.RequirementOptional, model.Requirement("")}
	paths := []string{"a/one.go", "a/two.go", "b/one.go", ""}
	out := make([]candRec, 0, n)
	for i := range n {
		r := candRec{
			Seq:         int64(i),
			Requirement: reqs[rng.Intn(len(reqs))],
			ScoreMicros: int64(rng.Intn(4)) * 100_000,
			PathFinal:   paths[rng.Intn(len(paths))],
			StartByte:   int64(rng.Intn(3)) * 64,
		}
		// PathAtRank is drawn INDEPENDENTLY, not derived from PathFinal: a
		// derived value with a constant prefix orders identically and would let
		// a lessRank that read the wrong field pass. Ruling C3 exists precisely
		// because the two can name different packages.
		r.PathAtRank = paths[rng.Intn(len(paths))]
		switch rng.Intn(3) {
		case 0:
			r.NodeID = model.NodeID(paths[rng.Intn(len(paths))] + "#node")
		case 1:
			r.FileID = model.FileID(paths[rng.Intn(len(paths))] + "#file")
		}
		out = append(out, r)
	}
	return out
}

// TestFoldsAreOrderIndependent proves every fold answers the same record
// whichever side it is given first. pagination.ExternalSort folds left to right
// over the ordered stream, but a fold that depended on that would break the
// moment a collapse pass or a different merge order was introduced, and the
// break would be a silently different score or count rather than a failure.
func TestFoldsAreOrderIndependent(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewSource(7))

	for range 200 {
		a := candRec{Seq: int64(rng.Intn(50)), NodeID: "n", Origin: originExplicitSeed}
		b := candRec{Seq: int64(rng.Intn(50)), NodeID: "n", Origin: originLexical}
		if a.Seq == b.Seq {
			continue
		}
		ab, err := foldMinSeq(a, b)
		if err != nil {
			t.Fatalf("foldMinSeq: %v", err)
		}
		ba, err := foldMinSeq(b, a)
		if err != nil {
			t.Fatalf("foldMinSeq: %v", err)
		}
		if !reflect.DeepEqual(ab, ba) {
			t.Fatalf("foldMinSeq depends on order: %+v vs %+v", ab, ba)
		}
		// The earliest step wins, which is scope.go:112's rule exactly.
		if ab.Seq != min(a.Seq, b.Seq) {
			t.Fatalf("foldMinSeq kept seq %d, want %d", ab.Seq, min(a.Seq, b.Seq))
		}
	}

	for range 200 {
		a := pkgCountRec{Pkg: "p", Edges: int64(rng.Intn(100))}
		b := pkgCountRec{Pkg: "p", Edges: int64(rng.Intn(100))}
		ab, _ := foldPkgCount(a, b)
		ba, _ := foldPkgCount(b, a)
		if !reflect.DeepEqual(ab, ba) || ab.Edges != a.Edges+b.Edges {
			t.Fatalf("foldPkgCount is not an order-independent sum: %+v vs %+v", ab, ba)
		}
	}

	for range 200 {
		a := groupRec{FileID: "f", Path: "a.go", Required: rng.Intn(2) == 0,
			Bytes: int64(rng.Intn(1000)), Tokens: int64(rng.Intn(100)), MinIndex: int64(rng.Intn(50)), Count: 1}
		b := groupRec{FileID: "f", Path: "b.go", Required: rng.Intn(2) == 0,
			Bytes: int64(rng.Intn(1000)), Tokens: int64(rng.Intn(100)), MinIndex: int64(rng.Intn(50)), Count: 1}
		if a.MinIndex == b.MinIndex {
			// Two members of one group never share a rank position: Index is
			// unique across the stream, so this pair is not reachable.
			continue
		}
		ab, _ := foldGroup(a, b)
		ba, _ := foldGroup(b, a)
		if !reflect.DeepEqual(ab, ba) {
			t.Fatalf("foldGroup depends on order: %+v vs %+v", ab, ba)
		}
		if ab.Required != (a.Required || b.Required) || ab.Bytes != a.Bytes+b.Bytes ||
			ab.Tokens != a.Tokens+b.Tokens || ab.Count != 2 || ab.MinIndex != min(a.MinIndex, b.MinIndex) {
			t.Fatalf("foldGroup is not the fileGroup aggregate: %+v from %+v and %+v", ab, a, b)
		}
		// The path follows the lowest-ranked member, which is the member
		// groupByFile creates the group from.
		want := a.Path
		if b.MinIndex < a.MinIndex {
			want = b.Path
		}
		if ab.Path != want {
			t.Fatalf("foldGroup kept path %q, want %q", ab.Path, want)
		}
	}

	for range 200 {
		// One relation has one kind, so the reachable pairs are "same kind" and
		// "one side carries no kind yet".
		kind := model.RelCalls
		a := relAttrRec{RelationID: "rel", Kind: kind, Multiplier: int64(rng.Intn(1_000_000))}
		b := relAttrRec{RelationID: "rel", Multiplier: int64(rng.Intn(1_000_000))}
		if rng.Intn(2) == 0 {
			b.Kind = kind
		}
		ab, _ := foldRelAttr(a, b)
		ba, _ := foldRelAttr(b, a)
		if !reflect.DeepEqual(ab, ba) {
			t.Fatalf("foldRelAttr depends on order: %+v vs %+v", ab, ba)
		}
		if ab.Kind != kind || ab.Multiplier != max(a.Multiplier, b.Multiplier) {
			t.Fatalf("foldRelAttr is not mostPrecise: %+v from %+v and %+v", ab, a, b)
		}
	}

	for range 50 {
		a := pkgEdgeRec{Pkg: "p", RelationID: "rel"}
		ab, _ := foldPkgEdgeDistinct(a, a)
		ba, _ := foldPkgEdgeDistinct(a, a)
		if ab != a || ba != a {
			t.Fatalf("foldPkgEdgeDistinct did not collapse a repeated edge: %+v", ab)
		}
	}

	// The heuristic floor lives in precision() and is applied there whatever the
	// fold produced, so an edge with no recognised evidence scores exactly as
	// mostPrecise scores it today.
	floor := precisionMultiplier[model.PrecisionHeuristic]
	if got := (relAttrRec{RelationID: "rel"}).precision(); got != floor {
		t.Fatalf("an unresolved edge takes multiplier %d, want the heuristic floor %d", got, floor)
	}
	if got := (relAttrRec{RelationID: "rel", Multiplier: 1_000_000}).precision(); got != 1_000_000 {
		t.Fatalf("a resolved edge takes multiplier %d, want 1000000", got)
	}
}

// TestCompileSortsReleaseEveryRun proves ruling C5's structural half: every sort
// a compile opens is registered for release, so a compile that fails mid-pass
// leaves no run file behind in the store's shared sort area. It also exercises
// the sort area end to end -- the byte-budgeted run buffer, lessEntityID and
// foldMinSeq -- because a release path that never spilled would prove nothing.
func TestCompileSortsReleaseEveryRun(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg := config.Config{}
	cfg.Resources.QueryMemoryBytes = 1 << 20

	sorts, err := newCompileSorts(cfg, dir)
	if err != nil {
		t.Fatalf("newCompileSorts: %v", err)
	}
	sorter, err := newSort(sorts, "cand", lessEntityID, sizeOfCand)
	if err != nil {
		t.Fatalf("newSort: %v", err)
	}
	sorter = sorter.WithFold(foldMinSeq)
	// Enough records, and wide enough, to fill the run buffer several times over
	// so the sort really spills and really merges.
	const entities, copies = 400, 12
	for c := range copies {
		for e := range entities {
			rec := candRec{
				Seq:        int64(c*entities + e),
				NodeID:     model.NodeID("node-" + string(rune('a'+e%26)) + itoa(e)),
				PathFinal:  "internal/context/" + itoa(e) + ".go",
				PathAtRank: "internal/context/" + itoa(e) + ".go",
				Reasons:    []string{"a reason long enough to make the run buffer mean something at all"},
			}
			if err := sorter.Add(rec); err != nil {
				t.Fatalf("Add: %v", err)
			}
		}
	}
	run, err := sorter.Sorted()
	if err != nil {
		t.Fatalf("Sorted: %v", err)
	}
	run = trackRun(sorts, run)
	if run.Len() != entities {
		t.Fatalf("the dedupe fold kept %d records, want %d distinct entities", run.Len(), entities)
	}
	seen := 0
	err = run.Each(func(r candRec) error {
		// The earliest arrival of each entity survives: its seq is its position
		// in the FIRST copy, which is below the entity count.
		if r.Seq >= entities {
			return &model.Error{Code: model.CodeInternal, Message: "a later arrival survived the min-seq fold"}
		}
		seen++
		return nil
	})
	if err != nil {
		t.Fatalf("Each: %v", err)
	}
	if seen != entities {
		t.Fatalf("walked %d records, want %d", seen, entities)
	}

	if err := sorts.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	left, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(left) != 0 {
		t.Fatalf("the sort area still holds %d file(s) after release: %v", len(left), left)
	}

	if _, err := newCompileSorts(cfg, ""); err == nil {
		t.Fatal("newCompileSorts accepted an empty sort directory; ruling C5 requires the store's own")
	}
}

// itoa keeps the fixture readable without pulling strconv into the file for one
// call site.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
