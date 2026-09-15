package graph

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
)

// newLevelWalk is a retained walk over a throwaway parent directory, with the
// surrogate bounds the fixtures below use. Nothing is created on disk until a
// test writes into it, which is what the lazy-directory case relies on.
func newLevelWalk(t *testing.T, chunk int) *retainedWalk {
	t.Helper()
	w, err := openRetainedWalk(filepath.Join(t.TempDir(), "scratch"), walkBounds{Node: 1 << 20, Relation: 1 << 20}, nil)
	if err != nil {
		t.Fatalf("openRetainedWalk: %v", err)
	}
	w.chunkSize = chunk
	t.Cleanup(w.discard)
	return w
}

// levelFixture builds one record. owner and neighbour are surrogates; the
// canonical ids are derived from them so the canonical order is predictable.
func levelFixture(owner, neighbour NodeRef, rel RelRef, cost int64, kind KindCode) levelRecord {
	return levelRecord{
		Owner: frontierState{Depth: 1, Cost: cost, Node: owner, Via: RelRef(owner),
			Route: []RelRef{RelRef(owner)}},
		Edge:    Edge{Owner: owner, Neighbour: neighbour, Rel: rel, Kind: kind, Outgoing: true},
		OwnerID: model.NodeID(fmt.Sprintf("%064d", owner)),
		NodeID:  model.NodeID(fmt.Sprintf("%064d", neighbour)),
		RelID:   model.RelationID(fmt.Sprintf("%064d", rel)),
	}
}

// TestLevelRecordCodecRoundTripsEveryField is what pays for a packed codec
// being able to drift from the struct. A level record is the ONLY carrier of an
// admitted edge between the scan that delivered it and the page that serves it:
// a field added here and forgotten in encodeLevelRecord would serve an entry
// with a blank route, a lost owner or the wrong relation, and nothing else in
// the walk would notice.
func TestLevelRecordCodecRoundTripsEveryField(t *testing.T) {
	full := levelRecord{
		Owner:   frontierState{Depth: 3, Cost: 4200, Node: 77, Via: 88, Route: []RelRef{5, 88}},
		Edge:    Edge{Owner: 77, Neighbour: 910, Rel: 1011, Kind: 7, Outgoing: true},
		OwnerID: model.NodeID("00000000000000000000000000000000000000000000000000000000000000aa"),
		NodeID:  model.NodeID("00000000000000000000000000000000000000000000000000000000000000bb"),
		RelID:   model.RelationID("00000000000000000000000000000000000000000000000000000000000000cc"),
	}
	assertNoZeroLevelField(t, "levelRecord", reflect.ValueOf(full))
	assertNoZeroLevelField(t, "levelRecord.Owner", reflect.ValueOf(full.Owner))
	assertNoZeroLevelField(t, "levelRecord.Edge", reflect.ValueOf(full.Edge))

	for _, tc := range []struct {
		name string
		in   levelRecord
	}{
		{"every field set", full},
		// An incoming entry with no route is the seed level's shape, and it
		// must come back with a nil route rather than an empty-but-non-nil one.
		{"zero", levelRecord{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := decodeLevelRecord(encodeLevelRecord(tc.in))
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if tc.in.Owner.Route == nil && len(got.Owner.Route) == 0 {
				got.Owner.Route = nil
			}
			if !reflect.DeepEqual(got, tc.in) {
				t.Fatalf("round trip changed the record:\n got %+v\nwant %+v", got, tc.in)
			}
		})
	}

	// Damage is a typed corruption, never a short record: a level decoded short
	// serves fewer entries than the walk admitted and reads as the whole answer.
	enc := encodeLevelRecord(full)
	for _, tc := range []struct {
		name string
		in   []byte
	}{
		{"truncated", enc[:len(enc)-4]},
		{"trailing bytes", append(append([]byte(nil), enc...), 0)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := decodeLevelRecord(tc.in)
			var me *model.Error
			if err == nil {
				t.Fatal("a damaged record decoded without an error")
			}
			if !errors.As(err, &me) || me.Code != model.CodeStorageCorrupt {
				t.Fatalf("error %v, want %s", err, model.CodeStorageCorrupt)
			}
		})
	}

	// The canonical order is (neighbour, owner, relation) and nothing else: a
	// level keyed on surrogates would serve a fresh index and a delta-built one
	// in different orders.
	a := levelFixture(9, 1, 5, 0, 1)
	b := levelFixture(2, 1, 6, 0, 1)
	c := levelFixture(1, 3, 1, 0, 1)
	if !lessLevelRecord(b, a) {
		t.Fatal("same neighbour: the smaller canonical OWNER must sort first")
	}
	if !lessLevelRecord(a, c) {
		t.Fatal("the smaller canonical NEIGHBOUR must sort first whatever its owner is")
	}
	d := levelFixture(9, 1, 4, 0, 1)
	if !lessLevelRecord(d, a) {
		t.Fatal("same neighbour and owner: the smaller canonical RELATION must sort first")
	}
}

func assertNoZeroLevelField(t *testing.T, path string, v reflect.Value) {
	t.Helper()
	for i := 0; i < v.NumField(); i++ {
		f := v.Type().Field(i)
		if v.Field(i).Kind() == reflect.Struct {
			continue
		}
		if v.Field(i).IsZero() {
			t.Fatalf("%s.%s is zero in the codec fixture: fill it, or the round trip does not test it", path, f.Name)
		}
	}
}

// TestLevelCollectorSpillsAndFinishesComplete protects the level pipeline's
// central claim: a level too large for the frontier ceiling is spilled, sorted
// and served WHOLE. A finish that dropped the spilled tail would serve a
// truncated level as a complete one -- entries silently lost from the answer,
// which no other assertion in this package would catch.
//
// It also pins the COLLECTING resume: a collector reopened at the byte count
// its cursor carried holds exactly the prefix that was persisted, so the
// resumed scan neither replays a committed record nor loses one.
func TestLevelCollectorSpillsAndFinishesComplete(t *testing.T) {
	w := newLevelWalk(t, 0)
	// A ceiling of 200 bytes is a handful of records, so this level spills.
	c := newLevelCollector(w, 1, 200)
	want := map[string]bool{}
	for i := 1; i <= 400; i++ {
		// Fed in an order unrelated to the canonical one, so the sort has work.
		r := levelFixture(NodeRef(i%7+1), NodeRef(1000-i), RelRef(i), int64(i), 1)
		if err := c.add(r); err != nil {
			t.Fatalf("add: %v", err)
		}
		want[string(r.NodeID)+string(r.RelID)] = true
	}
	if c.rawBytes() == 0 {
		t.Fatal("a level over the ceiling stayed resident: nothing spilled")
	}
	if c.records() != 400 {
		t.Fatalf("records = %d, want 400", c.records())
	}

	s, err := c.finish(context.Background())
	if err != nil {
		t.Fatalf("finish: %v", err)
	}
	if _, err := os.Stat(filepath.Join(w.home.path, levelFileName(rawLevelPrefix, 1))); !os.IsNotExist(err) {
		t.Fatalf("the raw run survived finish: %v", err)
	}
	got := map[string]bool{}
	var prev levelRecord
	var n int64
	if err := s.each(0, func(r levelRecord, _ int64) error {
		if n > 0 && lessLevelRecord(r, prev) {
			t.Fatalf("record %d is out of canonical order", n)
		}
		prev, n = r, n+1
		got[string(r.NodeID)+string(r.RelID)] = true
		return nil
	}); err != nil {
		t.Fatalf("each: %v", err)
	}
	if n != 400 || s.records() != 400 {
		t.Fatalf("the sorted level holds %d records (reports %d), want 400", n, s.records())
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatal("the sorted level is not the set that was fed in")
	}

	// A collector reopened at a committed byte count holds exactly that prefix.
	spilled := newLevelCollector(w, 2, 0)
	for i := 1; i <= 10; i++ {
		if err := spilled.add(levelFixture(1, NodeRef(i), RelRef(i), 0, 1)); err != nil {
			t.Fatalf("add: %v", err)
		}
	}
	cut := spilled.rawBytes()
	for i := 11; i <= 20; i++ {
		if err := spilled.add(levelFixture(1, NodeRef(i), RelRef(i), 0, 1)); err != nil {
			t.Fatalf("add: %v", err)
		}
	}
	if err := spilled.raw.close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	resumed, err := reopenLevelCollector(w, 2, 0, cut)
	if err != nil {
		t.Fatalf("reopenLevelCollector: %v", err)
	}
	if resumed.records() != 10 || resumed.rawBytes() != cut {
		t.Fatalf("a collector reopened at %d bytes holds %d records at %d bytes, want 10 at %d",
			cut, resumed.records(), resumed.rawBytes(), cut)
	}
}

// TestSortedLevelResumesAtTheReportedOffset protects the SERVING cursor. A page
// carries the offset each reports for the last record it delivered; if the
// resumed read did not start exactly there the answer would either repeat an
// entry across pages or skip one, and both read as a correct page.
func TestSortedLevelResumesAtTheReportedOffset(t *testing.T) {
	w := newLevelWalk(t, 0)
	c := newLevelCollector(w, 3, 1<<20)
	for i := 1; i <= 60; i++ {
		if err := c.add(levelFixture(NodeRef(i%5+1), NodeRef(i), RelRef(i), 0, 1)); err != nil {
			t.Fatalf("add: %v", err)
		}
	}
	s, err := c.finish(context.Background())
	if err != nil {
		t.Fatalf("finish: %v", err)
	}

	var whole []levelRecord
	var offsets []int64
	if err := s.each(0, func(r levelRecord, next int64) error {
		whole, offsets = append(whole, r), append(offsets, next)
		return nil
	}); err != nil {
		t.Fatalf("each: %v", err)
	}
	if len(whole) != 60 {
		t.Fatalf("the level holds %d records, want 60", len(whole))
	}

	// Cut at three offsets, including the last, and check the concatenation.
	for _, cut := range []int{1, 30, len(whole) - 1} {
		head := append([]levelRecord(nil), whole[:cut+1]...)
		if err := s.each(offsets[cut], func(r levelRecord, _ int64) error {
			head = append(head, r)
			return nil
		}); err != nil {
			t.Fatalf("each(%d): %v", offsets[cut], err)
		}
		if !reflect.DeepEqual(head, whole) {
			t.Fatalf("cut after record %d: the two halves do not reassemble the level", cut)
		}
	}

	if err := s.release(); err != nil {
		t.Fatalf("release: %v", err)
	}
	if _, err := os.Stat(filepath.Join(w.home.path, levelFileName(sortedLevelPrefix, 3))); !os.IsNotExist(err) {
		t.Fatalf("the sorted level survived release: %v", err)
	}
}

// TestAdmitLevelCommitsTheCheapestRouteAndRedoesIdempotently protects the level
// transition's order of effects. Two things must hold. The route a neighbour is
// admitted with must be the cheapest of its group and derived only from
// content, or a fresh index and a delta-built index of one tree would serve
// different reasons for the same entry. And a redo after a crash between the
// bits and the manifest must admit nothing a second time while rebuilding the
// identical frontier -- the state the admitted file is written FIRST to make
// recoverable.
//
// Mutation (set the bits before admitted.<level> is written): the redo below
// finds no admitted file, rebuilds it against bits that are already set, admits
// nothing and leaves an EMPTY frontier, so the frontier assertion fails.
func TestAdmitLevelCommitsTheCheapestRouteAndRedoesIdempotently(t *testing.T) {
	w := newLevelWalk(t, 0)
	costs := kindCosts{0, 10, 1}
	c := newLevelCollector(w, 1, 1<<20)
	// Node 500 is reachable three ways. The cheapest edge is the kind-2 one
	// from owner 3; the two kind-1 edges cost more whatever their relation id.
	for _, r := range []levelRecord{
		levelFixture(1, 500, 7, 0, 1),
		levelFixture(3, 500, 9, 0, 2),
		levelFixture(2, 500, 8, 0, 1),
		// Node 600 is reached twice at the same cost; the smaller canonical
		// relation id breaks the tie.
		levelFixture(1, 600, 4, 0, 1),
		levelFixture(2, 600, 2, 0, 1),
	} {
		if err := c.add(r); err != nil {
			t.Fatalf("add: %v", err)
		}
	}
	s, err := c.finish(context.Background())
	if err != nil {
		t.Fatalf("finish: %v", err)
	}
	n, err := w.admitLevel(context.Background(), s, 1, costs)
	if err != nil {
		t.Fatalf("admitLevel: %v", err)
	}
	if n != 2 {
		t.Fatalf("admitted %d nodes, want 2", n)
	}
	admitted := map[NodeRef]frontierState{}
	if err := w.eachFrontier(1, 0, func(chunk []frontierState) error {
		for _, fs := range chunk {
			admitted[fs.Node] = fs
		}
		return nil
	}); err != nil {
		t.Fatalf("eachFrontier: %v", err)
	}
	if got := admitted[500]; got.Via != 9 || got.Cost != 1 {
		t.Fatalf("node 500 admitted via %d at cost %d, want the cheapest route (rel 9, cost 1)", got.Via, got.Cost)
	}
	if got := admitted[600]; got.Via != 2 {
		t.Fatalf("node 600 admitted via %d, want the smaller canonical relation id", got.Via)
	}
	for _, ref := range []NodeRef{500, 600} {
		on, err := w.testFrontier(ref)
		if err != nil || !on {
			t.Fatalf("testFrontier(%d) = %v, %v: the frontier was not rebuilt", ref, on, err)
		}
	}

	// A transition cut between choosing what to admit and writing
	// admitted.<level> must leave NOTHING behind: the redo below has to make
	// the same two admissions. Setting the bits before the file is written
	// loses them -- the redo finds no file, tests bits that are already set,
	// admits nothing and rebuilds an empty frontier.
	crashed := newLevelWalk(t, 0)
	crashed.crashInAdmit = true
	if _, err := crashed.admitLevel(context.Background(), s, 1, costs); !errors.Is(err, errAdmitCrash) {
		t.Fatalf("the injected crash returned %v, want the transition to be cut short", err)
	}
	crashed.crashInAdmit = false
	if got, err := crashed.admitLevel(context.Background(), s, 1, costs); err != nil || got != 2 {
		t.Fatalf("the redo after a cut transition admitted %d nodes (%v), want 2", got, err)
	}
	for _, ref := range []NodeRef{500, 600} {
		on, err := crashed.testFrontier(ref)
		if err != nil || !on {
			t.Fatalf("after the cut transition testFrontier(%d) = %v, %v: the level was lost", ref, on, err)
		}
	}

	// The redo: the admitted file is already there, and re-running the
	// transition must admit nothing twice and rebuild the same frontier.
	redo, err := w.admitLevel(context.Background(), s, 1, costs)
	if err != nil {
		t.Fatalf("redo admitLevel: %v", err)
	}
	if redo != n {
		t.Fatalf("the redo admitted %d nodes, want the same %d", redo, n)
	}
	if got := w.bits.count(); got != 2 {
		t.Fatalf("the visited set holds %d nodes after the redo, want 2", got)
	}
	again := map[NodeRef]frontierState{}
	if err := w.eachFrontier(1, 0, func(chunk []frontierState) error {
		for _, fs := range chunk {
			again[fs.Node] = fs
		}
		return nil
	}); err != nil {
		t.Fatalf("eachFrontier: %v", err)
	}
	if !reflect.DeepEqual(again, admitted) {
		t.Fatalf("the redo rebuilt a different frontier:\n got %+v\nwant %+v", again, admitted)
	}
}

// TestEachFrontierChunksAndResumesAtTheNode protects the frontier's memory
// bound and its resume. The next level's scan takes the frontier a chunk at a
// time, so the chunking must not drop a state at a boundary, and a page cut
// mid-level resumes at the surrogate it stopped on -- a resume that ignored it
// would re-scan states already expanded and double their edges.
func TestEachFrontierChunksAndResumesAtTheNode(t *testing.T) {
	w := newLevelWalk(t, 2)
	c := newLevelCollector(w, 1, 1<<20)
	for i := 1; i <= 5; i++ {
		if err := c.add(levelFixture(1, NodeRef(100*i), RelRef(i), 0, 1)); err != nil {
			t.Fatalf("add: %v", err)
		}
	}
	s, err := c.finish(context.Background())
	if err != nil {
		t.Fatalf("finish: %v", err)
	}
	if _, err := w.admitLevel(context.Background(), s, 1, kindCosts{0, 1}); err != nil {
		t.Fatalf("admitLevel: %v", err)
	}

	var sizes []int
	var seen []NodeRef
	if err := w.eachFrontier(1, 0, func(chunk []frontierState) error {
		sizes = append(sizes, len(chunk))
		for _, fs := range chunk {
			seen = append(seen, fs.Node)
		}
		return nil
	}); err != nil {
		t.Fatalf("eachFrontier: %v", err)
	}
	if !reflect.DeepEqual(sizes, []int{2, 2, 1}) {
		t.Fatalf("chunk sizes %v, want 2, 2 and a short last one", sizes)
	}
	if !reflect.DeepEqual(seen, []NodeRef{100, 200, 300, 400, 500}) {
		t.Fatalf("the frontier streamed %v, want every admitted node ascending", seen)
	}

	var resumed []NodeRef
	if err := w.eachFrontier(1, 300, func(chunk []frontierState) error {
		for _, fs := range chunk {
			resumed = append(resumed, fs.Node)
		}
		return nil
	}); err != nil {
		t.Fatalf("eachFrontier(from 300): %v", err)
	}
	if !reflect.DeepEqual(resumed, []NodeRef{300, 400, 500}) {
		t.Fatalf("resuming at 300 streamed %v, want the tail from 300", resumed)
	}
}

// TestAWalkThatNeitherSpillsNorDetachesLeavesNothingOnDisk protects the lazy
// retained directory. A walk answered in one page has no state to carry
// forward, and an empty directory left behind is still adopted, leased, charged
// against the spool byte budget and swept -- state the request never needed and
// the next one has to pay for.
//
// Mutation (create the directory in openRetainedWalk): the first assertion
// below fails.
func TestAWalkThatNeitherSpillsNorDetachesLeavesNothingOnDisk(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "scratch")
	w, err := openRetainedWalk(parent, walkBounds{Node: 1 << 20, Relation: 1 << 20}, nil)
	if err != nil {
		t.Fatalf("openRetainedWalk: %v", err)
	}
	if _, err := bitsetSet(w.bits, []NodeRef{1, 2, 3}); err != nil {
		t.Fatalf("bitsetSet: %v", err)
	}
	if in, err := w.bits.test(2); err != nil || !in {
		t.Fatalf("test(2) = %v, %v: a resident set must still answer", in, err)
	}
	if _, err := os.Stat(w.home.path); !os.IsNotExist(err) {
		t.Fatalf("the retained directory was created before anything was written: %v", err)
	}

	// Detaching is a write: the next request opens the directory by name.
	dir, err := w.detach()
	if err != nil {
		t.Fatalf("detach: %v", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("the detached directory is not on disk: %v", err)
	}
	reopened, err := reopenRetainedWalk(dir, "", walkBounds{Node: 1 << 20, Relation: 1 << 20}, nil)
	if err != nil {
		t.Fatalf("reopenRetainedWalk: %v", err)
	}
	defer reopened.discard()
	if in, err := reopened.bits.test(2); err != nil || !in {
		t.Fatalf("the detached set lost its bits: test(2) = %v, %v", in, err)
	}
}
