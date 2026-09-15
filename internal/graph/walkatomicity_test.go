package graph

import "testing"

// TestALevelCutBetweenRecordsAndBitsLosesNothingAndAdmitsNothingTwice is
// ADR-0005 Decision 2's page-atomicity rule, proved on the retained directory
// itself rather than through an engine: the machinery below is what every
// endpoint's recovery rests on, and no fixture clock is needed to cut a page in
// the one place that matters.
//
// The commit order is records, then emitted relations, then admitted nodes. A
// request killed between the first write and the last leaves a level whose
// records are on disk and whose bits are not. The next request adopts that
// level, re-applies its surrogates, and must reach exactly the state an
// uninterrupted commit would have reached:
//
//   - nothing is LOST: every node the cut level named is admitted after the
//     adoption, so the walk still expands it;
//   - nothing is admitted TWICE: the population count is the count of distinct
//     surrogates, because the bitset counts bit TRANSITIONS and not set calls,
//     which is what makes the re-application idempotent and what keeps
//     `visited_count` equal to the set's size across any number of cuts.
//
// Mutation proof (fails this test): make bitsetSet count each ref it is handed
// rather than each bit it flips -- the second adoption reports 7 admitted nodes
// where the set holds 4.
func TestALevelCutBetweenRecordsAndBitsLosesNothingAndAdmitsNothingTwice(t *testing.T) {
	level := []frontierState{
		{Depth: 1, Cost: 1, Node: 7, Via: 3, Route: []RelRef{3}},
		{Depth: 1, Cost: 1, Node: 4, Via: 9, Route: []RelRef{9}},
		{Depth: 1, Cost: 2, Node: 12, Via: 5, Route: []RelRef{1, 5}},
	}
	dir := t.TempDir()
	w, err := openRetainedWalk(dir, walkBounds{Node: 64, Relation: 64}, &heapProbe{})
	if err != nil {
		t.Fatalf("open retained walk: %v", err)
	}
	// The seed level commits whole, so the cut below is the SECOND level's and
	// the adoption has a non-empty prior population to be idempotent against.
	if err := w.commitLevel([]frontierState{{Node: 1}}, []NodeRef{1}); err != nil {
		t.Fatalf("commit the seed level: %v", err)
	}
	// The cut: the records land, the process dies, the bits never do.
	if err := w.spoolLevel(level); err != nil {
		t.Fatalf("spool the cut level: %v", err)
	}
	cut := w.bits.count()
	if cut != 1 {
		t.Fatalf("the cut level marked %d node(s) before it was cut: this case only proves "+
			"anything when the records reach disk and the bits do not", cut-1)
	}
	if err := w.close(); err != nil {
		t.Fatalf("close the cut walk: %v", err)
	}

	resumed, err := reopenRetainedWalk(w.home.path, "", walkBounds{Node: 64, Relation: 64}, &heapProbe{})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer resumed.close()
	adopted, err := resumed.adopt()
	if err != nil {
		t.Fatalf("adopt the cut level: %v", err)
	}
	if len(adopted) != len(level) {
		t.Fatalf("the adoption recovered %d frontier record(s) of %d: the cut lost a node",
			len(adopted), len(level))
	}
	for i, want := range level {
		got := adopted[i]
		if got.Node != want.Node || got.Via != want.Via || got.Depth != want.Depth ||
			got.Cost != want.Cost || len(got.Route) != len(want.Route) {
			t.Fatalf("record %d came back as %+v, want %+v", i, got, want)
		}
	}
	// Nothing lost: every surrogate the cut level named answers the admission
	// test the walk asks before it expands a node.
	for _, fs := range level {
		in, err := resumed.bits.test(uint64(fs.Node))
		if err != nil {
			t.Fatalf("test node %d: %v", fs.Node, err)
		}
		if !in {
			t.Errorf("node %d is not admitted after the adoption: the cut lost it", fs.Node)
		}
	}
	if got := resumed.bits.count(); got != uint64(len(level)+1) {
		t.Fatalf("the adopted walk reports %d admitted node(s), want %d", got, len(level)+1)
	}
	// Nothing twice: adopting the same level again is the retry of a page that
	// was cut a second time, and it may not move the count.
	if _, err := resumed.adopt(); err != nil {
		t.Fatalf("re-adopt: %v", err)
	}
	if got := resumed.bits.count(); got != uint64(len(level)+1) {
		t.Fatalf("a second adoption of the same level reports %d admitted node(s), want %d: "+
			"visited_count counts set CALLS, not distinct nodes", got, len(level)+1)
	}
}
