package graph

import (
	"context"
	"fmt"
	"runtime"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
)

// syntheticNodes is the size of the walk the cumulative set is measured
// against: 200 000 nodes already admitted by earlier pages of one continuation
// chain. A map of that many 64-hex NodeIDs is tens of megabytes of heap, which
// is the structure this set exists to keep off the heap.
const syntheticNodes = 200_000

// syntheticID is one node of the synthetic graph, spelled the way model.NodeID
// is: a 64-character hex identifier.
func syntheticID(i int) model.NodeID {
	return model.NodeID(fmt.Sprintf("%064x", i))
}

// spooledSet is a stand-in for the continuation spool: it replays every node an
// earlier page admitted, one at a time, holding none of them. The real stream
// is pagination.Spools.Open over a disk file (cursor.go), which has exactly
// this shape -- a forward-only replay of one record at a time.
func spooledSet(n int) visitedStream {
	return func(_ context.Context, fn func(model.NodeID) error) error {
		for i := 0; i < n; i++ {
			if err := fn(syntheticID(i)); err != nil {
				return err
			}
		}
		return nil
	}
}

// TestVisitedSetHeapIsBoundedByTheFrontNotTheWalk is the no-OOM invariant: a
// page resuming a walk that has already admitted 200 000 nodes answers
// membership for every one of them while holding only its own page's worth of
// them in heap.
//
// Before this set, the resume materialized the whole cumulative set into a
// map[NodeID]bool on EVERY page, so peak heap grew with the walk and a
// repository-sized traversal could not finish.
func TestVisitedSetHeapIsBoundedByTheFrontNotTheWalk(t *testing.T) {
	ctx := context.Background()
	set := newVisitedSet(spooledSet(syntheticNodes))

	var before runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)

	// A page's worth of work: levels of candidates probed against the spooled
	// set. Every candidate below IS in that set, so each level's answers are
	// correct only if the stream is consulted.
	const levels, perLevel = 40, 50
	hits := 0
	for l := 0; l < levels; l++ {
		candidates := make([]model.NodeID, 0, perLevel)
		for k := 0; k < perLevel; k++ {
			candidates = append(candidates, syntheticID((l*perLevel+k)*37%syntheticNodes))
		}
		if err := set.warm(ctx, candidates); err != nil {
			t.Fatalf("warm level %d: %v", l, err)
		}
		for _, id := range candidates {
			if set.has(id) {
				hits++
				continue
			}
			// Not yet admitted: this page admits it, and it must stay
			// answered from the front for the rest of the page.
			set.add(id)
		}
	}
	if hits != levels*perLevel {
		t.Fatalf("the spooled set answered %d of %d candidates; every one of them was admitted by an earlier page",
			hits, levels*perLevel)
	}

	var after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&after)
	grew := int64(after.HeapAlloc) - int64(before.HeapAlloc)
	t.Logf("cumulative set = %d nodes; heap held by the front after %d levels = %d bytes",
		syntheticNodes, levels, grew)

	// A map of 200 000 64-hex ids costs upward of 20 MB. The front here holds
	// at most levels*perLevel ids plus one level of probe answers, so one
	// megabyte is a bound that a walk-sized structure cannot meet and a
	// page-sized one clears by an order of magnitude.
	const bound = 1 << 20
	if grew > bound {
		t.Fatalf("the visited set held %d bytes of heap for a %d-node walk; the front must be bounded by the page (bound %d bytes)",
			grew, syntheticNodes, bound)
	}
	// The front is what the next spill writes, and it must be only this page's
	// own admissions -- never the cumulative set, which is copied spool to
	// spool instead.
	if got := len(set.newlyAdmitted()); got > levels*perLevel {
		t.Fatalf("the page offered %d newly admitted nodes; it admitted at most %d", got, levels*perLevel)
	}
}
