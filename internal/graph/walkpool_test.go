package graph

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/Sawmonabo/codectx/internal/paced"
	"github.com/Sawmonabo/codectx/internal/scratch"
)

// openDescriptors is how many files this process has open. It is the count a
// leak shows up in first: a scratch pool is claimed with a lock file held for
// the process's life, so a pool per request is a descriptor per request.
func openDescriptors(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Skipf("this platform does not report the process's open files: %v", err)
	}
	return len(entries)
}

// The requirement: a request must cost what the store holds, not what the
// request invented. A walk mints a retained directory of its own per request,
// and a scratch pool taken under that directory claims an instance with a
// lock file held for the life of the process and registers that directory
// with the space reclaimer for the life of the process -- neither is ever
// released, because a pool is the store's and outlives every request. Pooling
// under a per-request directory therefore leaks a descriptor and a permanent
// registration per walk, which in a server that never restarts ends at the
// process's descriptor limit, and makes every later removal and every
// `status --resources` scan what the run's history invented.
//
// So a walk's sorts take their runs from the STORE's pool, the one every walk
// shares, and two hundred walks leave the process holding what one walk left.
//
// Mutation: pool under the walk's own directory again (pass
// filepath.Join(dir, "levelsort") to NewExternalSort in finishSpilled) and the
// count climbs by one pool and its descriptor per walk.
func TestManyWalksLeaveThePoolWhereOneWalkLeftIt(t *testing.T) {
	store := filepath.Join(t.TempDir(), "spools")
	spill := func() {
		w, err := openRetainedWalk(store, walkBounds{Node: 1 << 20}, nil)
		if err != nil {
			t.Fatalf("openRetainedWalk: %v", err)
		}
		defer w.discard()
		// A ceiling of 200 bytes is a handful of records, so the level spills
		// and the sort reaches the pool.
		c := newLevelCollector(w, 1, 200)
		for i := 1; i <= 40; i++ {
			if err := c.add(levelFixture(NodeRef(i%7+1), NodeRef(1000-i), RelRef(i), int64(i), 1)); err != nil {
				t.Fatalf("add: %v", err)
			}
		}
		if c.rawBytes() == 0 {
			t.Fatal("the level stayed resident: nothing spilled, so no sort reached a pool")
		}
		if _, err := c.finish(context.Background()); err != nil {
			t.Fatalf("finish: %v", err)
		}
	}

	// The first walk is what brings the store's own pool into being; the
	// question is what every walk after it costs.
	spill()
	paced.Drain()
	pools, descriptors := len(scratch.All()), openDescriptors(t)

	const walks = 200
	for range walks {
		spill()
	}

	paced.Drain()
	if got := len(scratch.All()); got != pools {
		t.Fatalf("%d walks left the process holding %d scratch pools, having held %d: "+
			"a pool per request is a claim and a registration nothing ever releases", walks, got, pools)
	}
	if got := openDescriptors(t); got > descriptors {
		t.Fatalf("%d walks left %d files open, having had %d: each pool holds its claim for the life of the process",
			walks, got, descriptors)
	}
}
