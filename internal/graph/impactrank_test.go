package graph

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
)

// The freeze protects three invariants and nothing else: a record survives the
// disk round trip unchanged, every fold is independent of the order its
// records arrive in, and a ranked spool cannot be read as a walk spool or the
// reverse. Everything else in this file is machinery the fill-in lanes test
// against their own behaviour.

func TestRankRecordsRoundTripThroughTheSortCodec(t *testing.T) {
	impacts := []impactRecord{
		{},
		{NodeID: "n1"},
		{NodeID: "n2", Depth: 3, Cost: 7, Direction: model.DirectionIncoming, Via: "r9", Parent: "n1"},
		{NodeID: "n3", Depth: 1, Cost: 0, Direction: model.DirectionOutgoing,
			Route: []model.RelationID{"r1", "r2"}, Reasons: []string{"a", "b"}},
		{NodeID: "n4", Depth: 64, Cost: 1 << 40, Direction: model.DirectionBoth,
			Route: []model.RelationID{"\x00unicode-é"}, Reasons: []string{"quote\"and\\slash"}},
	}
	for _, want := range impacts {
		b, err := encodeImpactRecord(want)
		if err != nil {
			t.Fatalf("encoding %+v: %v", want, err)
		}
		got, err := decodeImpactRecord(b)
		if err != nil {
			t.Fatalf("decoding %s: %v", b, err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("impact record round trip: got %+v, want %+v", got, want)
		}
	}

	pairs := []pairRecord{
		{},
		{FromNodeID: "p1", ToNodeID: "p2", PairCount: 1},
		{FromNodeID: "p1", ToNodeID: "p2", FromPath: "a/b", ToPath: "c/d",
			PairCount: 1 << 40, EvidenceCount: 9},
	}
	for _, want := range pairs {
		b, err := encodePairRecord(want)
		if err != nil {
			t.Fatalf("encoding %+v: %v", want, err)
		}
		got, err := decodePairRecord(b)
		if err != nil {
			t.Fatalf("decoding %s: %v", b, err)
		}
		if got != want {
			t.Fatalf("pair record round trip: got %+v, want %+v", got, want)
		}
	}
}

func TestFoldsDoNotDependOnArrivalOrder(t *testing.T) {
	cheap := impactRecord{NodeID: "n", Depth: 1, Cost: 2, Direction: model.DirectionOutgoing,
		Via: "r1", Parent: "seed", Route: []model.RelationID{"r1"}, Reasons: []string{"b", "a"}}
	dear := impactRecord{NodeID: "n", Depth: 4, Cost: 9, Direction: model.DirectionIncoming,
		Via: "r2", Parent: "mid", Route: []model.RelationID{"r7", "r2"}, Reasons: []string{"c"}}
	// Same cost and depth, different admitting edge: the tie-break must still
	// pick one side, or the fold is not a function of its inputs.
	twinA := impactRecord{NodeID: "n", Depth: 1, Cost: 2, Via: "r3", Parent: "s1"}
	twinB := impactRecord{NodeID: "n", Depth: 1, Cost: 2, Via: "r4", Parent: "s2"}

	for _, tc := range []struct {
		name string
		a, b impactRecord
	}{
		{"cheaper_route_wins", cheap, dear},
		{"identical", cheap, cheap},
		{"tie_on_cost_and_depth", twinA, twinB},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ab, err := foldImpact(tc.a, tc.b)
			if err != nil {
				t.Fatalf("folding: %v", err)
			}
			ba, err := foldImpact(tc.b, tc.a)
			if err != nil {
				t.Fatalf("folding: %v", err)
			}
			if !reflect.DeepEqual(ab, ba) {
				t.Fatalf("fold is order dependent: %+v vs %+v", ab, ba)
			}
		})
	}

	// The cheapest route decides cost, depth, via, parent and route together:
	// a served path must never describe a route whose cost contradicts the
	// score the entry reports.
	got, err := foldImpact(dear, cheap)
	if err != nil {
		t.Fatalf("folding: %v", err)
	}
	if got.Cost != cheap.Cost || got.Depth != cheap.Depth || got.Via != cheap.Via ||
		got.Parent != cheap.Parent || !reflect.DeepEqual(got.Route, cheap.Route) {
		t.Fatalf("the fold did not take the whole route from the cheapest side: %+v", got)
	}
	// The incoming upgrade is sticky whichever side carried it.
	if got.Direction != model.DirectionIncoming {
		t.Fatalf("the incoming upgrade did not stick: %q", got.Direction)
	}
	// Reasons are the ordered union of both sides, not an arrival-ordered one.
	if want := []string{"a", "b", "c"}; !reflect.DeepEqual(got.Reasons, want) {
		t.Fatalf("reasons: got %v, want %v", got.Reasons, want)
	}

	x := pairRecord{FromNodeID: "f", ToNodeID: "t", FromPath: "z", ToPath: "b", PairCount: 2, EvidenceCount: 5}
	y := pairRecord{FromNodeID: "f", ToNodeID: "t", FromPath: "a", ToPath: "c", PairCount: 3, EvidenceCount: 1}
	xy, err := foldPair(x, y)
	if err != nil {
		t.Fatalf("folding a pair: %v", err)
	}
	yx, err := foldPair(y, x)
	if err != nil {
		t.Fatalf("folding a pair: %v", err)
	}
	if xy != yx {
		t.Fatalf("the pair fold is order dependent: %+v vs %+v", xy, yx)
	}
	if xy.PairCount != 5 || xy.EvidenceCount != 6 {
		t.Fatalf("the pair counts are not exact sums: %+v", xy)
	}
	// An unresolved label must never erase a resolved one, whichever side it
	// arrives on.
	blank := pairRecord{FromNodeID: "f", ToNodeID: "t", PairCount: 1}
	for _, got := range []pairRecord{mustFoldPair(t, blank, y), mustFoldPair(t, y, blank)} {
		if got.FromPath != y.FromPath || got.ToPath != y.ToPath {
			t.Fatalf("an empty label won the fold: %+v", got)
		}
	}
}

func mustFoldPair(t *testing.T, a, b pairRecord) pairRecord {
	t.Helper()
	got, err := foldPair(a, b)
	if err != nil {
		t.Fatalf("folding a pair: %v", err)
	}
	return got
}

func TestRankedSpoolHeaderIsNotAWalkRecord(t *testing.T) {
	b, err := encodeRankedHeader(42)
	if err != nil {
		t.Fatalf("encoding a ranked header: %v", err)
	}
	h, err := decodeRankedHeader(b)
	if err != nil {
		t.Fatalf("decoding a ranked header: %v", err)
	}
	if h.Kind != spoolRecordRanked || h.Total != 42 {
		t.Fatalf("ranked header round trip: %+v", h)
	}
	// A frontier record must not read as a ranked header: the two spool shapes
	// hold different things, and replaying one as the other serves a wrong
	// page rather than an error.
	walk, err := json.Marshal(spoolRecord{Kind: spoolRecordFrontier, Node: "n1", Depth: 2})
	if err != nil {
		t.Fatalf("encoding a frontier record: %v", err)
	}
	if _, err := decodeRankedHeader(walk); err == nil {
		t.Fatal("a frontier record was accepted as a ranked spool header")
	}
}

func TestRankedCursorFieldsAreRefusedOnAWalkContinuation(t *testing.T) {
	hex := strings.Repeat("a", model.IDHexLen)
	base := func() traversalCursor {
		return traversalCursor{
			Version: traversalCursorVersion, Endpoint: impactEndpoint, GenerationID: 1,
			AnalysisKey: "k", QueryHash: hex, LeaseID: hex, SpoolID: hex,
			Ranked: true, RankOffset: 10, RankServed: 10, RankTotal: 25,
			ExpiresAt: time.Unix(1, 0),
		}
	}
	if err := base().validate(); err != nil {
		t.Fatalf("a well-formed ranked cursor was refused: %v", err)
	}
	for _, tc := range []struct {
		name   string
		break_ func(*traversalCursor)
	}{
		{"walk cursor carrying a ranked position", func(c *traversalCursor) { c.Ranked = false; c.SpoolID = "" }},
		{"ranked cursor naming no spool", func(c *traversalCursor) { c.SpoolID = "" }},
		{"ranked cursor carrying a keyset position", func(c *traversalCursor) { c.LastOwner = "n1"; c.LastKey = "r1" }},
		{"ranked cursor continuing past the answer", func(c *traversalCursor) { c.RankServed = c.RankTotal + 1 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := base()
			tc.break_(&c)
			if err := c.validate(); err == nil {
				t.Fatalf("validate accepted %+v", c)
			}
		})
	}
}
