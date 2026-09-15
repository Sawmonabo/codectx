package graph

import "testing"

// TestALevelCutBetweenTheAdmittedFileAndItsBitsLosesNothingAndAdmitsNothingTwice
// is ADR-0005 Decision 2's page-atomicity rule, proved on the retained
// directory itself rather than through an engine: the machinery below is what
// every endpoint's recovery rests on, and no fixture clock is needed to cut a
// page in the one place that matters.
//
// The level transition's order of effects is admitted.<level> first, then the
// visited bits, then the frontier bits, then the sync that lets the level be
// named as served. A request killed between the first write and the last leaves
// a level whose admitted states are on disk and whose bits are not. The next
// request re-applies that file and must reach exactly the state an
// uninterrupted transition would have reached:
//
//   - nothing is LOST: every node the cut level named is admitted afterwards,
//     so the walk still expands it, and the frontier bits name that level and
//     only that level -- the question the direction dedup rule asks;
//   - nothing is admitted TWICE: the population count is the count of distinct
//     surrogates, because the bitset counts bit TRANSITIONS and not set calls,
//     which is what makes the re-application idempotent and what keeps
//     `visited_count` equal to the set's size across any number of cuts.
//
// Mutation proof (fails this test): make bitsetSet count each ref it is handed
// rather than each bit it flips -- the second application reports 7 admitted
// nodes where the set holds 4.
func TestALevelCutBetweenTheAdmittedFileAndItsBitsLosesNothingAndAdmitsNothingTwice(t *testing.T) {
	level := []frontierState{
		{Depth: 1, Cost: 1, Node: 4, Via: 9, Route: []RelRef{9}},
		{Depth: 1, Cost: 1, Node: 7, Via: 3, Route: []RelRef{3}},
		{Depth: 1, Cost: 2, Node: 12, Via: 5, Route: []RelRef{1, 5}},
	}
	dir := t.TempDir()
	w, err := openRetainedWalk(dir, walkBounds{Node: 64}, &heapProbe{})
	if err != nil {
		t.Fatalf("open retained walk: %v", err)
	}
	// The seed level commits whole, so the cut below is the SECOND level's and
	// the re-application has a non-empty prior population to be idempotent
	// against.
	if err := w.commitSeeds([]frontierState{{Node: 1}}); err != nil {
		t.Fatalf("commit the seed level: %v", err)
	}
	// The cut: the admitted states land, the process dies, the bits never do.
	file := openRetainFile(w.home, levelFileName(admittedLevelPrefix, 1))
	for _, fs := range level {
		if err := file.append(encodeFrontierState(fs)); err != nil {
			t.Fatalf("write the admitted level: %v", err)
		}
	}
	if err := file.close(); err != nil {
		t.Fatalf("close the admitted level: %v", err)
	}
	if cut := w.bits.count(); cut != 1 {
		t.Fatalf("the cut level marked %d node(s) before it was cut: this case only proves "+
			"anything when the states reach disk and the bits do not", cut-1)
	}
	if err := w.close(); err != nil {
		t.Fatalf("close the cut walk: %v", err)
	}

	resumed, err := reopenRetainedWalk(w.home.path, "", walkBounds{Node: 64}, &heapProbe{})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer resumed.close()
	// The redo of the transition: the file that is already there is the
	// committed decision of this level, and applying it is what the interrupted
	// request never reached.
	redo := openRetainFile(resumed.home, levelFileName(admittedLevelPrefix, 1))
	if err := resumed.applyFrontier(redo); err != nil {
		t.Fatalf("apply the cut level: %v", err)
	}
	// Nothing lost: every surrogate the cut level named answers the admission
	// test the walk asks before it expands a node, and the frontier the next
	// level is scanned against is exactly this level.
	for _, fs := range level {
		in, err := resumed.bits.test(uint64(fs.Node))
		if err != nil {
			t.Fatalf("test node %d: %v", fs.Node, err)
		}
		if !in {
			t.Errorf("node %d is not admitted after the redo: the cut lost it", fs.Node)
		}
		on, err := resumed.testFrontier(fs.Node)
		if err != nil {
			t.Fatalf("test the frontier at %d: %v", fs.Node, err)
		}
		if !on {
			t.Errorf("node %d is not on the frontier after the redo: the dedup rule would drop its edges", fs.Node)
		}
	}
	if on, err := resumed.testFrontier(1); err != nil || on {
		t.Errorf("the seed is still on the frontier after the redo (%v, %v): the frontier is one level, not a union", on, err)
	}
	var recovered int
	if err := redo.each(func([]byte) error {
		recovered++
		return nil
	}); err != nil {
		t.Fatalf("replay the admitted level: %v", err)
	}
	if recovered != len(level) {
		t.Fatalf("the redo recovered %d admitted state(s) of %d: the cut lost a node", recovered, len(level))
	}
	if got := resumed.bits.count(); got != uint64(len(level)+1) {
		t.Fatalf("the resumed walk reports %d admitted node(s), want %d", got, len(level)+1)
	}
	// Nothing twice: applying the same level again is the retry of a page that
	// was cut a second time, and it may not move the count.
	if err := resumed.applyFrontier(redo); err != nil {
		t.Fatalf("re-apply: %v", err)
	}
	if got := resumed.bits.count(); got != uint64(len(level)+1) {
		t.Fatalf("a second application of the same level reports %d admitted node(s), want %d: "+
			"visited_count counts set CALLS, not distinct nodes", got, len(level)+1)
	}
}
