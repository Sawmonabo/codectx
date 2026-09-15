package graph

import (
	"errors"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
)

// A page is cut between spooling a level's frontier and applying that level's
// bits, so adoption RE-APPLIES the last spooled level. The count the answer
// discloses as visited_count therefore has to follow bit transitions and not
// set calls, and it has to survive the handle: the next request opens the same
// directory and continues the same walk.
func TestAVisitedBitsetCountsOnlyTheBitsItTurnsOn(t *testing.T) {
	dir := t.TempDir()
	level := []NodeRef{3, 9, 4096, 70_000}

	set, err := openBitset(dir, 100_000, nil)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := set.set(level); err != nil {
		t.Fatalf("set: %v", err)
	}
	// The re-application adoption performs, in the same handle and then across
	// one: neither may count a node the walk has already admitted.
	if _, err := set.set(level); err != nil {
		t.Fatalf("re-apply: %v", err)
	}
	if err := set.close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	resumed, err := openBitset(dir, 100_000, nil)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer resumed.close()
	if _, err := resumed.set(level); err != nil {
		t.Fatalf("adopt: %v", err)
	}
	if got := resumed.count(); got != uint64(len(level)) {
		t.Fatalf("count after two re-applications = %d, want %d", got, len(level))
	}
	for _, ref := range level {
		ok, err := resumed.test(ref)
		if err != nil {
			t.Fatalf("test(%d): %v", ref, err)
		}
		if !ok {
			t.Fatalf("test(%d) = false, want the admitted node to be present", ref)
		}
	}
	// A neighbouring bit in the same byte as ref 3 must be untouched, or the
	// walk would refuse to admit a node it never reached.
	if ok, err := resumed.test(2); err != nil || ok {
		t.Fatalf("test(2) = %v, %v; want false with no error", ok, err)
	}
}

// The page cache is a memory bound, never a bound on the work a walk may do: a
// level that reaches more pages than the cache holds still admits every node.
// That only holds if eviction WRITES the page it drops -- a dropped dirty page
// is a node admitted in heap and never on disk, which the next page re-admits
// and reports on two pages.
func TestAVisitedBitsetKeepsBitsThatOutliveItsPageCache(t *testing.T) {
	dir := t.TempDir()
	const perPage = bitsetPageBytes * 8
	const pages = bitsetCachePages + 44 // more pages than the cache can hold

	refs := make([]NodeRef, 0, pages)
	for i := range pages {
		refs = append(refs, NodeRef(i*perPage+1))
	}
	maxNode := refs[len(refs)-1]

	set, err := openBitset(dir, maxNode, nil)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := set.set(refs); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := set.close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	resumed, err := openBitset(dir, maxNode, nil)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer resumed.close()
	if got := resumed.count(); got != pages {
		t.Fatalf("count = %d, want %d", got, pages)
	}
	for _, ref := range refs {
		ok, err := resumed.test(ref)
		if err != nil {
			t.Fatalf("test(%d): %v", ref, err)
		}
		if !ok {
			t.Fatalf("test(%d) = false; the page holding it was evicted without being written", ref)
		}
	}
}

// Two preconditions are CHECKED rather than assumed, because breaking either
// one is silent. A surrogate above the pinned generation's MaxNode comes from
// another id space and the cursor fence exists to make it impossible, so
// accepting it would hide the breach behind a file the walk quietly extended.
// An unsorted level is merely slow -- it thrashes the cache instead of walking
// it forward once -- and nothing downstream would ever report it.
func TestAVisitedBitsetRefusesRefsItCannotHold(t *testing.T) {
	set, err := openBitset(t.TempDir(), 64, nil)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer set.close()

	if _, err := set.set([]NodeRef{65}); err == nil {
		t.Fatal("set accepted a surrogate above MaxNode, want an error")
	}
	if _, err := set.test(65); err == nil {
		t.Fatal("test accepted a surrogate above MaxNode, want an error")
	}

	_, err = set.set([]NodeRef{9, 4})
	var typed *model.Error
	if !errors.As(err, &typed) || typed.Code != model.CodeArgumentInvalid {
		t.Fatalf("set of a descending level = %v, want CTX_ARGUMENT_INVALID", err)
	}

	// Zero is never a node, so it has no bit and it is not counted.
	if _, err := set.set([]NodeRef{0, 7}); err != nil {
		t.Fatalf("set with the zero surrogate: %v", err)
	}
	if got := set.count(); got != 1 {
		t.Fatalf("count = %d, want 1: the zero surrogate must not take a bit", got)
	}
}
