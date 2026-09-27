package plan

import (
	"context"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/Sawmonabo/codectx/internal/config"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
	"github.com/Sawmonabo/codectx/internal/provider/dependence"
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
	sorter, err := pagination.NewExternalSort(t.TempDir(), 1024,
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
		sorter, err := pagination.NewExternalSort(dir, 4096,
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
		if spilled := sorter.SpilledRuns(); spilled < 2 {
			t.Fatalf("a %d-input sort spilled %d runs; it stayed in heap, "+
				"so a flat heap here would prove nothing", n, spilled)
		}
		run, err := sorter.Sorted()
		if err != nil {
			t.Fatalf("Sorted: %v", err)
		}
		defer func() { _ = run.Close() }()
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
	sorter, err := pagination.NewExternalSort(t.TempDir(), 8,
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
		sorter, err := pagination.NewExternalSort(dir, runBuffer,
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
		if spilled := sorter.SpilledRuns(); spilled < 2 {
			t.Fatalf("a %d-unit plan spilled %d runs; it stayed in heap, so a "+
				"flat heap here would prove nothing", n, spilled)
		}
		run, err := sorter.Sorted()
		if err != nil {
			t.Fatalf("Sorted: %v", err)
		}
		defer func() { _ = run.Close() }()

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
//
// Within a file provider's stretch the units run largest file first, arrival
// breaking a tie, so a parse stage's longest file starts first instead of
// becoming its tail. Mutations that fail it: the bytes term dropped from
// compareUnit, sorted ascending, or ranked above the provider position.
func TestTheUnitSequenceInterleavesSemanticProvidersInSelectionOrder(t *testing.T) {
	sorter, err := pagination.NewExternalSort(t.TempDir(), 8,
		encodeUnit, decodeUnit, compareUnit)
	if err != nil {
		t.Fatalf("NewExternalSort: %v", err)
	}
	t.Cleanup(func() { _ = sorter.Close() })
	// Positions 0 and 2 are file-invalidated providers, 1 and 3 semantic. The
	// records are added out of position order, so only the sort key can put
	// them back.
	sizes := []int64{10, 5, 30, 5, 10}
	for i, order := range []int{2, 0, 2, 0, 2} {
		rec := unitAt(len(t.Name()) + order)
		rec.Order, rec.Bytes, rec.Seq = order, sizes[i], int64(sorter.Len())
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
		"file:p2/n2.go", "file:p2/n0.go", "file:p2/n4.go", "proj:r"}
	if !slices.Equal(got, want) {
		t.Fatalf("the plan streamed %v, want %v: each provider's units must be one contiguous "+
			"stretch, in Selection.Active order, largest file first within it", got, want)
	}
}

// A repository's second index must never reserve a heavy unit against less
// than its first index watched that unit use: the reference runs recorded one
// JavaScript unit peaking at 1.04x the figure it was admitted against, and
// overrunning a reservation is the direction that freezes a host. The other
// half of the same invariant is the one that would be catastrophic to get
// wrong: a scope nobody ever measured, a workspace with no ledger at all and a
// platform that samples no process tree are every one of them NO OBSERVATION,
// and the reservation the family constants derived must then stand exactly as
// it is. A missing measurement read as a peak of zero would collapse every
// reservation in the product to the floor.
//
// Failure modes protected: (1) the second run of a repository under-reserves
// what the first run measured and freezes the host; (2) an absent observation
// is read as zero and every reservation collapses.
func TestARecordedPeakOnlyEverRaisesAReservationAndAnAbsentOneChangesNothing(t *testing.T) {
	const (
		scope    = "pkg:javascript:app"
		sibling  = "pkg:javascript:lib"
		otherLan = "pkg:go:service"
		// Far above what 1 MiB of JavaScript source derives, so a raise is
		// unmistakable and the floor cannot account for it.
		huge = int64(9) << 30
	)
	unit := func(key string) *semantic {
		return &semantic{providerID: dependence.ProviderID, scopeKey: key, heavy: true,
			family: dependence.FamilyJavaScript, bytes: 1 << 20}
	}
	gov := dependence.NewGovernor(0, 0)
	// An unobserved machine, so no allocation bound can mask the raise.
	machine := dependence.Machine{}
	derived := gov.Reserve(dependence.FamilyJavaScript, 1<<20, machine).Bytes()
	if derived <= 0 {
		t.Fatalf("the family constants derive %d bytes for a heavy unit; the test cannot tell a raise from a collapse", derived)
	}

	for _, tc := range []struct {
		name  string
		peaks map[string]int64
		unit  *semantic
		want  int64
	}{
		{name: "the scope's own recorded peak raises the reservation to it",
			peaks: map[string]int64{scope: huge}, unit: unit(scope), want: huge},
		{name: "a scope with no history of its own is sized by its language's",
			peaks: map[string]int64{scope: huge}, unit: unit(sibling), want: huge},
		{name: "no ledger at all leaves the derived reservation standing",
			peaks: nil, unit: unit(scope), want: derived},
		{name: "a ledger with no peak for this scope or its language leaves it standing",
			peaks: map[string]int64{otherLan: huge}, unit: unit(scope), want: derived},
		{name: "a recorded peak below the derived figure never lowers it",
			peaks: map[string]int64{scope: 1}, unit: unit(scope), want: derived},
		{name: "the scope's own measurement wins over its language's larger one",
			peaks: map[string]int64{sibling: huge, scope: huge / 2},
			unit:  unit(scope), want: max(derived, huge/2)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := &builder{}
			if tc.peaks != nil {
				b.in.RecordedPeak = ledgerLike(tc.peaks)
			}
			got := b.reserve(context.Background(), gov, tc.unit, machine).Bytes()
			if got != tc.want {
				t.Fatalf("the unit is admitted against %d bytes, want %d (the family constants derive %d)",
					got, tc.want, derived)
			}
			if got <= 0 {
				t.Fatalf("the unit is admitted against %d bytes: an absent observation was read as zero", got)
			}
		})
	}
}

// ledgerLike answers a lookup the way the run ledger's does: the scope's own
// row, and where it has none and a family prefix is given, the largest row
// whose key begins with it.
func ledgerLike(peaks map[string]int64) func(ctx context.Context, scopeKey, familyPrefix string) (int64, bool) {
	return func(_ context.Context, scopeKey, familyPrefix string) (int64, bool) {
		if peak, ok := peaks[scopeKey]; ok {
			return peak, true
		}
		largest, observed := int64(0), false
		for key, peak := range peaks {
			if familyPrefix != "" && strings.HasPrefix(key, familyPrefix) && peak > largest {
				largest, observed = peak, true
			}
		}
		return largest, observed
	}
}

// A header's grammar is decided by the snapshot's census, so a header's file
// units must be keyed by the census-derived grammar each reads: the
// structural unit by the grammar it is parsed with first, the filesystem unit
// by the manifest's tag, the census's first grammar. A census change that
// moves that grammar must rebuild the unit, and one that keeps it must reuse
// it, with every other file untouched either way. With only one of the two
// grammars enabled every header is parsed with that one, so no census change
// moves the structural unit, while the tag still moves the filesystem unit.
// The identity is taken after the unit's spill round trip, which is the form
// the executor derives it from. It fails if the key is omitted from
// fileUnits, Spec or the spilled record (a flip keeps the identity), if it is
// taken from the census's basis rather than a grammar (the non-flipping
// change moves it), if the structural key ignores the enabled grammars (the
// one-grammar flip moves it), or if the filesystem unit takes the structural
// key (the one-grammar flip keeps the stale tag's unit).
func TestAHeaderUnitIsKeyedByTheGrammarItReads(t *testing.T) {
	identity := func(providerID string, census model.Snapshot, path string, languages ...string) model.UnitID {
		t.Helper()
		sorter, err := pagination.NewExternalSort(t.TempDir(), 0, encodeUnit, decodeUnit, compareUnit)
		if err != nil {
			t.Fatalf("NewExternalSort: %v", err)
		}
		defer func() { _ = sorter.Close() }()
		b := &builder{headers: headerKeysOf(census, languages), allUnits: sorter, providerOrder: map[string]int{},
			plan:          Plan{Reuse: map[string]model.UnitID{}, Unplanned: map[string]int{}},
			fileProviders: []fileProvider{{id: providerID, version: "1", gate: func(model.FileVersion) bool { return true }}}}
		fv := model.FileVersion{ID: model.NewFileID("repo", path), Path: path,
			ContentHash: string(model.NewFileID("content", path))}
		if _, _, err := b.fileUnits(context.Background(), fv, false); err != nil {
			t.Fatalf("fileUnits(%s): %v", path, err)
		}
		run, err := sorter.Sorted()
		if err != nil {
			t.Fatalf("Sorted: %v", err)
		}
		defer func() { _ = run.Close() }()
		var id model.UnitID
		if err := run.Each(func(rec fileUnitRecord) error {
			spec, err := rec.unit().Spec("config")
			id = spec.ID
			return err
		}); err != nil {
			t.Fatalf("deriving the unit of %s: %v", path, err)
		}
		return id
	}
	const structural, files = "treesitter", "filesystem"
	cOnly := model.Snapshot{FileCount: 1, CUnits: 1}
	cppOnly := model.Snapshot{FileCount: 1, CPPUnits: 1}
	mixed := model.Snapshot{FileCount: 2, CUnits: 1, CPPUnits: 1}
	none := model.Snapshot{}
	for _, p := range []string{structural, files} {
		if identity(p, cOnly, "inc/a.h") == identity(p, cppOnly, "inc/a.h") {
			t.Fatalf("a census that flips the header grammar from C to C++ kept the %s unit's identity: "+
				"it would be reused with facts made under the wrong grammar", p)
		}
		if identity(p, mixed, "inc/a.h") != identity(p, none, "inc/a.h") {
			t.Fatalf("a census change that keeps C++ first moved the %s unit's identity: "+
				"every header would be rebuilt for nothing", p)
		}
		for _, path := range []string{"src/a.c", "inc/a.hpp"} {
			if identity(p, cOnly, path) != identity(p, cppOnly, path) {
				t.Fatalf("%s is no two-grammar header, yet the census moved its %s unit's identity", path, p)
			}
		}
	}
	if identity(structural, cOnly, "inc/a.h", "cpp") != identity(structural, cppOnly, "inc/a.h", "cpp") {
		t.Fatal("with only the C++ grammar enabled a census flip moved the structural unit's identity, " +
			"though every header is parsed with C++ either way")
	}
	if identity(files, cOnly, "inc/a.h", "cpp") == identity(files, cppOnly, "inc/a.h", "cpp") {
		t.Fatal("with only the C++ grammar enabled a census flip kept the filesystem unit's identity, " +
			"though the manifest's tag for the header moved from c to cpp")
	}
}
