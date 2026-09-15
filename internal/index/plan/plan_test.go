package plan

import (
	"os"
	"runtime"
	"slices"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
)

// inputAt is a distinct, deterministic snapshot input. File identities are
// hashes of (repository, path), so discovery order -- which is path order -- is
// not identity order, which is what makes the sort load-bearing.
func inputAt(i int) model.UnitInput {
	id := model.NewFileID("repo", "src/pkg"+string(rune('a'+i%26))+"/file"+itoa(i)+".go")
	return model.UnitInput{FileID: id, ContentHash: string(model.NewFileID("content", itoa(i)))}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

// digestOf folds a sorted sequence exactly as Unit.Spec does, so an order
// difference of one pair is a different unit identity.
func digestOf(t *testing.T, each func(yield func(model.UnitInput) error) error) string {
	t.Helper()
	h := model.NewUnitInputHasher()
	if err := each(h.Add); err != nil {
		t.Fatalf("folding the input digest: %v", err)
	}
	return h.Sum()
}

// The planner's external merge must reproduce the in-heap sort BYTE FOR BYTE,
// because the folded input digest it feeds is UnitSpec identity: an order that
// differs by one pair invalidates every stored reuse and carry decision of
// every prior generation. This holds the two digests against each other over
// 50 000 inputs with a run buffer small enough to force ~50 spilled runs and a
// real merge, which is the only way the merge path is exercised at all.
func TestExternalMergeReproducesTheInHeapInputOrder(t *testing.T) {
	const n = 50_000
	want := make([]model.UnitInput, 0, n)
	sorter, err := pagination.NewExternalSort(t.TempDir(), "plan-inputs-", 1024,
		encodeInput, decodeInput, compareInput)
	if err != nil {
		t.Fatalf("NewExternalSort: %v", err)
	}
	t.Cleanup(func() { _ = sorter.Close() })
	for i := range n {
		in := inputAt(i)
		want = append(want, in)
		if err := sorter.Add(in); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}
	slices.SortFunc(want, func(a, c model.UnitInput) int { return compareID(a.FileID, c.FileID) })

	run, err := sorter.Sorted()
	if err != nil {
		t.Fatalf("Sorted: %v", err)
	}
	t.Cleanup(func() { _ = run.Close() })
	if run.Len() != n {
		t.Fatalf("merged run holds %d inputs, want %d", run.Len(), n)
	}
	if got, expect := digestOf(t, run.Each), digestOf(t, staticInputs(want)); got != expect {
		t.Fatalf("merged input digest %s, want the in-heap sort's %s: unit identity moved", got, expect)
	}

	// Re-iterable: the planner folds the digest and the coordinator streams the
	// same sequence again into storage.
	if got, expect := digestOf(t, run.Each), digestOf(t, staticInputs(want)); got != expect {
		t.Fatalf("second pass digest %s, want %s", got, expect)
	}
}

// Peak heap must not grow with the number of inputs. Two sorts an order of
// magnitude apart are held against each other: the in-heap slice they replaced
// grew linearly, so a regression to one shows up as a ~10x ratio here.
//
// The heap delta alone is not sufficient evidence, because a sort that never
// spilled would also hold a flat heap for the wrong reason. Each measurement
// therefore also counts the files the sort left behind: it must have spilled
// (more than the merged output alone would leave) while the sort is alive, and
// the merged run must be the ONLY file left once the merge is done -- the run
// files are removed by Sorted, not at Close.
func TestSortedInputsDoNotGrowTheHeapWithInputCount(t *testing.T) {
	measure := func(n int) uint64 {
		dir := t.TempDir()
		files := func() int {
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatalf("ReadDir: %v", err)
			}
			return len(entries)
		}
		sorter, err := pagination.NewExternalSort(dir, "plan-inputs-", 4096,
			encodeInput, decodeInput, compareInput)
		if err != nil {
			t.Fatalf("NewExternalSort: %v", err)
		}
		defer func() { _ = sorter.Close() }()
		for i := range n {
			if err := sorter.Add(inputAt(i)); err != nil {
				t.Fatalf("Add: %v", err)
			}
		}
		if spilled := files(); spilled < 2 {
			t.Fatalf("a %d-input sort left %d run files before the merge; it never spilled, "+
				"so a flat heap here would prove nothing", n, spilled)
		}
		run, err := sorter.Sorted()
		if err != nil {
			t.Fatalf("Sorted: %v", err)
		}
		defer func() { _ = run.Close() }()
		if left := files(); left != 1 {
			t.Fatalf("%d files remain after the merge of %d inputs, want only the merged run: "+
				"a run file that survives Sorted is disk held for the life of the plan", left, n)
		}
		var m runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&m)
		held := m.HeapAlloc
		runtime.KeepAlive(run)
		runtime.KeepAlive(sorter)
		return held
	}
	small, large := measure(30_000), measure(300_000)
	if large > small*2 {
		t.Fatalf("heap held after 300 000 inputs is %d bytes against %d after 30 000: "+
			"a 10x input count must not carry the heap with it", large, small)
	}
	t.Logf("heap held: 30 000 inputs %d bytes, 300 000 inputs %d bytes", small, large)
}

// Reading a closed run must fail, never read as empty. The empty input digest
// is the same digest whatever the snapshot holds, so a unit folded from a
// closed run would claim an identity that reuses forever no matter what
// changed -- the exact degradation emit refuses by hand for a scope with no
// members. A lifetime mistake has to be loud.
func TestClosedInputRunRefusesToReadAsEmpty(t *testing.T) {
	sorter, err := pagination.NewExternalSort(t.TempDir(), "plan-inputs-", 8,
		encodeInput, decodeInput, compareInput)
	if err != nil {
		t.Fatalf("NewExternalSort: %v", err)
	}
	for i := range 64 { // more than the run buffer, so the run is file-backed
		if err := sorter.Add(inputAt(i)); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}
	run, err := sorter.Sorted()
	if err != nil {
		t.Fatalf("Sorted: %v", err)
	}
	if err := run.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	n := 0
	err = run.Each(func(model.UnitInput) error { n++; return nil })
	if err == nil {
		t.Fatalf("reading a closed run yielded %d inputs and no error; an empty walk here folds the "+
			"empty input digest, which is a unit identity that reuses forever", n)
	}
}
