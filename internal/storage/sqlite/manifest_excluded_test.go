package sqlite_test

import (
	"fmt"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
)

// The exclusion projection is the one repository-sized list a compiled manifest
// carries: a plan that keeps 40 entries can exclude every other candidate in
// the repository. PutManifest therefore takes it as a SEQUENCE and inserts row
// by row inside the manifest transaction, and this is the assertion that says
// so structurally rather than by reading the code.
//
// The measurement is taken from INSIDE the sequence, at its first and last row,
// because that is the only window in which a collecting writer would be holding
// the rows. Sampling before or after the call would see the same heap either
// way. The proof is that the growth across the window is the SAME CONSTANT at
// 20k exclusions and at 40k -- a writer that collected first would grow with
// the row count, because 40k references and reasons are megabytes.
//
// Mutation that must fail it: collect the sequence into a slice in PutManifest
// and insert from the slice. The 40k growth then exceeds the 20k growth by the
// size of 20k more rows, and the equality below breaks.
func TestPutManifestStreamsExclusionsRowAtATime(t *testing.T) {
	growth := map[int]int64{}
	for _, n := range []int{20_000, 40_000} {
		f := newFixture(t, filepath.Join(t.TempDir(), fmt.Sprintf("excl-%d.db", n)))
		ff := f.file("a.go", "package a\n")
		snap := f.snapshot("s1", ff)
		gen, err := f.s.BeginGeneration(f.ctx, f.repo, snap.ID, model.H("semantic"), "main")
		if err != nil {
			t.Fatalf("BeginGeneration: %v", err)
		}
		f.unit(gen, f.run(gen), ff)
		bind := f.activate(gen, 0)
		m := model.ContextManifest{
			ID: model.ManifestID(model.H("manifest", fmt.Sprint(n))), Binding: bind, Phase: model.PhaseSweep,
			RequestHash: model.H("req"), PolicyVersion: "p1", CanonicalHash: model.H("canon", fmt.Sprint(n)),
			EntryCount: 0, SliceCount: 0, Budget: model.Budget{MaxEstimatedTokens: 1000, MaxBytes: 4096, MaxFiles: 10, MaxSlices: 2},
			Completeness: fixtureCaps, ScopeComplete: false, EstimateMethod: "test-estimator",
			CreatedAt: time.Now().UTC().Truncate(time.Microsecond),
		}
		var first, last int64
		// The sequence GENERATES its rows; the test never holds them either, so
		// what the window measures is the writer's retention and nothing else.
		seq := func(yield func(model.ExcludedContextEntry) error) error {
			for i := 0; i < n; i++ {
				if i == 0 {
					first = liveHeap()
				}
				if i == n-1 {
					last = liveHeap()
				}
				if err := yield(model.ExcludedContextEntry{Ordinal: i,
					Reference: model.ContextReference{Path: fmt.Sprintf("src/pkg%04d/file%06d.go", i%1000, i)},
					Reason:    "the resolved budget could not hold this candidate"}); err != nil {
					return err
				}
			}
			return nil
		}
		if err := f.s.PutManifest(f.ctx, m, []byte(`{"task":"t"}`), nil, nil, seq); err != nil {
			t.Fatalf("PutManifest(%d exclusions): %v", n, err)
		}
		growth[n] = last - first
		t.Logf("%d exclusions: live heap across the write grew %d bytes", n, growth[n])
		// Every excluded row must be readable in ordinal order through the
		// unchanged `--view excluded` keyset reader: streaming the write may
		// not cost a row or reorder one.
		count, after := 0, -1
		for {
			page, err := f.s.ManifestExcluded(f.ctx, m.ID, after, 1000)
			if err != nil {
				t.Fatalf("ManifestExcluded: %v", err)
			}
			if len(page) == 0 {
				break
			}
			for _, x := range page {
				if x.Ordinal != count {
					t.Fatalf("excluded row %d read back with ordinal %d", count, x.Ordinal)
				}
				count++
			}
			after = page[len(page)-1].Ordinal
		}
		if count != n {
			t.Fatalf("read back %d excluded rows, wrote %d", count, n)
		}
	}
	// One row of this fixture is ~120 bytes of Go strings plus the struct, so
	// 20k more rows held at once is >= 2 MiB. The allowance below is an order
	// of magnitude under that and an order of magnitude over the per-row and
	// page-cache noise a streamed write actually shows.
	const allowance = 512 << 10
	if delta := growth[40_000] - growth[20_000]; delta > allowance {
		t.Fatalf("live heap across the exclusion write grew by %d bytes at 20k and %d at 40k; "+
			"a difference of %d bytes means the writer is holding the rows, not streaming them",
			growth[20_000], growth[40_000], delta)
	}
}

// liveHeap is the heap in use after a collection, so what it reports is what is
// still reachable rather than what has been allocated since the last sample.
func liveHeap() int64 {
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return int64(ms.HeapAlloc)
}
