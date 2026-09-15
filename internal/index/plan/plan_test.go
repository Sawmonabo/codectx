package plan

import (
	"os"
	"runtime"
	"slices"
	"testing"

	"github.com/Sawmonabo/codectx/internal/config"
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

// unitAt is one file-invalidated provider's planned unit, as the manifest walk
// spills it: a deep scope key, because MaxScopeKeyBytes and not a fixed width
// is what the record's byte budget is charged against.
func unitAt(i int) fileUnitRecord {
	in := inputAt(i)
	return fileUnitRecord{Order: 0, Seq: int64(i), ProviderID: "filesystem", ProviderVersion: "1",
		ScopeKey:  "file:src/very/deeply/nested/package/path/pkg" + itoa(i%26) + "/file" + itoa(i) + ".go",
		FileID:    in.FileID,
		DependsOn: model.UnitID("unit" + itoa(i)), ContentHash: in.ContentHash}
}

// The plan's units must never be materialised whole. A file-invalidated
// provider plans one unit per snapshot file, so an in-heap []Unit made peak
// RSS a function of repository size -- the one property the product may not
// have. Plan.Units is now a sequence over the planner's merged run, and this
// holds a 200 000-unit plan against a 30 000-unit one: the live set the sort
// ever held must stay inside ONE run batch, and the heap held midway through
// the executor's walk must not carry the unit count with it.
//
// The heap delta alone would not be evidence -- a sort that never spilled
// would also hold a flat heap, for the wrong reason -- so each measurement
// also counts the sort's files: it must have spilled more than the merged
// output before the merge, and the merged run must be the only file left
// after it.
func TestPlannedUnitsAreStreamedNotHeldInHeap(t *testing.T) {
	// The run buffer is shrunk to 4096 records so that a plan a test can
	// afford to build still spills and merges for real; production's is
	// pagination.RunBufferRecords under the same byte budget, and is bounded
	// by this same statement. One batch is that buffer plus the merge's own
	// one record per run at the fan-in cap -- read from the constant, not
	// restated.
	const runBuffer = 4096
	const batch = runBuffer + pagination.MaxSortFanIn
	measure := func(n int) (peak int, held uint64) {
		dir := t.TempDir()
		files := func() int {
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatalf("ReadDir: %v", err)
			}
			return len(entries)
		}
		sorter, err := pagination.NewExternalSort(dir, "plan-units-", runBuffer,
			encodeUnit, decodeUnit, compareUnit)
		if err != nil {
			t.Fatalf("NewExternalSort: %v", err)
		}
		defer func() { _ = sorter.Close() }()
		sorter = sorter.WithRunBytes(
			pagination.SortRunBytes(config.Defaults().Resources.QueryMemoryBytes), sizeOfUnit)
		for i := range n {
			if err := sorter.Add(unitAt(i)); err != nil {
				t.Fatalf("Add: %v", err)
			}
		}
		if spilled := files(); spilled < 2 {
			t.Fatalf("a %d-unit plan left %d run files before the merge; it never spilled, so a "+
				"flat heap here would prove nothing", n, spilled)
		}
		run, err := sorter.Sorted()
		if err != nil {
			t.Fatalf("Sorted: %v", err)
		}
		defer func() { _ = run.Close() }()
		if left := files(); left != 1 {
			t.Fatalf("%d files remain after the merge of %d units, want only the merged run", left, n)
		}

		// The executor's own walk, through the production sequence. Peak heap
		// is read at its midpoint, with everything the plan owns alive.
		seen, mid := 0, n/2
		err = unitSequence(run, make([][]Unit, 1))(func(u Unit) error {
			if want := unitAt(seen).ScopeKey; u.ScopeKey != want {
				return errOrder(seen, u.ScopeKey, want)
			}
			seen++
			if seen == mid {
				var m runtime.MemStats
				runtime.GC()
				runtime.ReadMemStats(&m)
				held = m.HeapAlloc
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walking the plan's units: %v", err)
		}
		if seen != n {
			t.Fatalf("the plan streamed %d units, want %d", seen, n)
		}
		runtime.KeepAlive(run)
		return sorter.PeakLiveRecords(), held
	}
	smallPeak, small := measure(30_000)
	largePeak, large := measure(200_000)
	if largePeak > batch {
		t.Fatalf("planning 200 000 units held %d unit records live at once, want at most one batch "+
			"of %d: the plan is materialising its units", largePeak, batch)
	}
	if large > small*2 {
		t.Fatalf("heap held midway through a 200 000-unit walk is %d bytes against %d midway through "+
			"a 30 000-unit one: the executor is holding the plan, not streaming it", large, small)
	}
	t.Logf("peak live unit records: 30 000 units %d, 200 000 units %d (one batch = %d)",
		smallPeak, largePeak, batch)
	t.Logf("heap held mid-walk: 30 000 units %d bytes, 200 000 units %d bytes", small, large)
}

// errOrder names a unit the plan streamed out of the order the in-heap
// concatenation answered in. The executor runs a provider's units as one
// group and storage refuses a unit whose dependency is not sealed, so an order
// change is a build failure, not a cosmetic one.
func errOrder(at int, got, want string) error {
	return &model.Error{Code: model.CodeInternal,
		Message: "the plan streamed unit " + itoa(at) + " as " + got + ", want " + want}
}

// A provider's units must reach the executor as ONE contiguous stretch, in
// Selection.Active order. build opens a group when a provider's first unit
// arrives and drains it on the provider change, so a sequence that splits one
// provider across two stretches makes it open a second group for the same
// provider -- and storage refuses a unit whose declared dependency is not yet
// sealed. The streamed file units and the in-heap semantic units come from two
// different places, so their interleave is where that can go wrong.
func TestTheUnitSequenceInterleavesSemanticProvidersInSelectionOrder(t *testing.T) {
	sorter, err := pagination.NewExternalSort(t.TempDir(), "plan-units-", 8,
		encodeUnit, decodeUnit, compareUnit)
	if err != nil {
		t.Fatalf("NewExternalSort: %v", err)
	}
	t.Cleanup(func() { _ = sorter.Close() })
	// Positions 0 and 2 are file-invalidated providers, 1 and 3 semantic. The
	// records are added out of position order, so only the sort key can put
	// them back.
	for _, order := range []int{2, 0, 2, 0, 2} {
		rec := unitAt(len(t.Name()) + order)
		rec.Order, rec.Seq = order, int64(sorter.Len())
		rec.ScopeKey = "file:p" + itoa(order) + "/n" + itoa(int(rec.Seq)) + ".go"
		if err := sorter.Add(rec); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}
	run, err := sorter.Sorted()
	if err != nil {
		t.Fatalf("Sorted: %v", err)
	}
	t.Cleanup(func() { _ = run.Close() })
	sem := func(id, scope string) Unit { return Unit{ProviderID: id, ScopeKey: scope} }
	slots := [][]Unit{nil, {sem("scip", "pkg:a"), sem("scip", "pkg:b")}, nil, {sem("dep", "proj:r")}}

	var got []string
	if err := unitSequence(run, slots)(func(u Unit) error {
		got = append(got, u.ScopeKey)
		return nil
	}); err != nil {
		t.Fatalf("walking the sequence: %v", err)
	}
	want := []string{"file:p0/n1.go", "file:p0/n3.go", "pkg:a", "pkg:b",
		"file:p2/n0.go", "file:p2/n2.go", "file:p2/n4.go", "proj:r"}
	if !slices.Equal(got, want) {
		t.Fatalf("the plan streamed %v, want %v: each provider's units must be one contiguous "+
			"stretch, in Selection.Active order", got, want)
	}
}
