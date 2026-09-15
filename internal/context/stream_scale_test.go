package context

import (
	"fmt"
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/Sawmonabo/codectx/internal/config"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
)

// stream_scale_test.go holds the ONE at-scale proof of the streamed compile:
// that a compile's memory is a function of the run budget and its paging is a
// function of the page, not of how large the repository is.
//
// It is the C-P4 items 2 and 3 proof. The generated fixture of
// generated_fixture_test.go proves the pipeline's SHAPE on a topology small
// enough to reason about; this one proves the SCALE claim, which needs two
// sizes and is only evidence if the two answers differ in size while the peak
// does not.

// scaleProofEnv opts this file's at-scale proof in.
const scaleProofEnv = "CODECTX_SCALE_PROOF"

// scaledLeaves are the two fan-out sizes the proof compiles. They must differ
// by a factor, so a peak that tracked the input could not possibly land on the
// same number twice.
var scaledLeaves = []int{20000, 40000}

// scaledSpecs is generatedSpecs at an arbitrary fan-out: the same four
// structural files and the same leaf shape, with the leaf count as a parameter.
// The leaves are documents for the reason generatedSpecs gives.
func scaledSpecs(leaves int) []fixtureFileSpec {
	out := []fixtureFileSpec{
		{genRootA, "RootA", model.NodeFunction, "package gen\n\nfunc RootA() {}\n", false},
		{genRootB, "RootB", model.NodeFunction, "package gen\n\nfunc RootB() {}\n", false},
		{genMid, "Mid", model.NodeFunction, "package gen\n\nfunc Mid() {}\n", false},
		{genShared, "Shared", model.NodeInterface, "package gen\n\ntype Shared interface{ S() }\n", false},
	}
	for i := range leaves {
		sym := fmt.Sprintf("Leaf%06d", i)
		out = append(out, fixtureFileSpec{genLeaf(i), sym, model.NodeDocument,
			fmt.Sprintf("# %s\n\nleaf %d of the scaled fan-out.\n", sym, i), i < generatedChanged})
	}
	return out
}

// scaledScope hangs every leaf off RootA by ONE relation kind, not by the
// seventeen generatedScope repeats: the scale this proof is about is the number
// of CANDIDATES the pipeline carries, and multiplying the edge count by
// seventeen would multiply the fixture's publication cost without changing what
// is being asserted. The two routes to Shared are kept so the scaled fixture
// still exercises the min-sequence fold.
func scaledScope(leaves int) func(fx *contextFixture) []model.Relation {
	return func(fx *contextFixture) []model.Relation {
		out := make([]model.Relation, 0, leaves+4)
		for i := range leaves {
			out = append(out, fx.edge(genRootA, model.RelCalls, genLeaf(i)))
		}
		return append(out,
			fx.edge(genRootA, model.RelCalls, genShared),
			fx.edge(genRootB, model.RelCalls, genMid),
			fx.edge(genMid, model.RelCalls, genShared),
			fx.edge(genRootB, model.RelCalls, genRootA))
	}
}

// scaleObservation is one size's measurement.
type scaleObservation struct {
	leaves   int
	entries  int
	peak     map[string]int
	spilled  map[string]int
	compile  time.Duration
	pages    []time.Duration
	pageRows []int
}

// TestTheStreamedCompilePeaksOnTheRunBufferAtEitherScale is C-P4 items 2 and 3.
//
// Two compiles over the same topology at two sizes. What is ASSERTED is the
// structural invariant: every sort of the larger compile peaks at exactly the
// record count the smaller one peaked at, so the compile's live working set is
// the run buffer and nothing else, and the answer still doubles. What is
// RECORDED, and reported in docs/performance.md rather than asserted, is wall
// clock: a latency ratio asserted inside a package that also runs under -race
// is a flake, and the claim the numbers support -- no per-page cost
// proportional to the total -- is a measurement, not an invariant of the code.
//
// The page latencies come from the persisted manifest's own keyset reader,
// which is how every consumer reads a plan back.
func TestTheStreamedCompilePeaksOnTheRunBufferAtEitherScale(t *testing.T) {
	// Publishing two fixtures of scaledLeaves files costs more than a minute
	// EACH before a compile starts, so this proof is opt-in rather than part of
	// every run of this package: leaving it in the default gate would put
	// minutes on every lane's `go test ./internal/context`, and several times
	// that under -race. The numbers it produces are recorded in
	// docs/performance.md; run it with CODECTX_SCALE_PROOF=1 to reproduce them.
	if os.Getenv(scaleProofEnv) == "" {
		t.Skipf("set %s=1 to run the at-scale compile proof (it publishes %v files)", scaleProofEnv, scaledLeaves)
	}
	obs := make([]scaleObservation, 0, len(scaledLeaves))
	for _, leaves := range scaledLeaves {
		obs = append(obs, measureCompileAtScale(t, leaves))
	}
	for _, o := range obs {
		t.Logf("leaves=%d entries=%d compile=%s pages=%v rows=%v",
			o.leaves, o.entries, o.compile.Round(time.Millisecond), o.pages, o.pageRows)
		for name, p := range o.peak {
			t.Logf("  sort %-16s peak=%d records spilledRuns=%d", name, p, o.spilled[name])
		}
	}
	small, large := obs[0], obs[len(obs)-1]
	if large.entries <= small.entries {
		t.Fatalf("the %d-leaf compile answered %d entries and the %d-leaf one %d: the two sizes must "+
			"differ in the ANSWER, or an unchanged peak proves nothing",
			small.leaves, small.entries, large.leaves, large.entries)
	}
	for name, want := range small.peak {
		got, ok := large.peak[name]
		if !ok {
			t.Errorf("the %d-leaf compile opened sort %q and the %d-leaf one did not", small.leaves, name, large.leaves)
			continue
		}
		if got != want {
			t.Errorf("sort %q peaked at %d records over %d leaves and %d records over %d leaves: the "+
				"compile's live working set tracks the repository, not the run budget",
				name, want, small.leaves, got, large.leaves)
		}
	}
}

// measureCompileAtScale publishes one scaled fixture, compiles it with the sort
// observer attached, and pages the persisted plan back.
func measureCompileAtScale(t *testing.T, leaves int) scaleObservation {
	t.Helper()
	fx := newFixtureFrom(t, scaledSpecs(leaves), scaledScope(leaves))
	c := intCompiler(t, fx, fx.Now)
	out := scaleObservation{leaves: leaves, peak: map[string]int{}, spilled: map[string]int{}}
	c.observeSorts = func(o []sortObservation) {
		for _, row := range o {
			// A pass may open a sort of the same name once per compile; the
			// last observation of a name is the one Close recorded for it.
			out.peak[row.Name] = row.Peak
			out.spilled[row.Name] = row.Spilled
		}
	}
	budget := model.Budget{MaxBytes: 1 << 30, MaxSlices: 4 * leaves, MaxFiles: 4 * leaves}
	start := time.Now()
	m, err := c.Compile(fx.ctx, generatedRequest(budget))
	if err != nil {
		t.Fatalf("Compile(%d leaves): %v", leaves, err)
	}
	out.compile = time.Since(start)

	after := -1
	for {
		pageStart := time.Now()
		page, err := fx.Store.ManifestEntries(fx.ctx, m.ID, after, 0)
		if err != nil {
			t.Fatalf("ManifestEntries(%d leaves): %v", leaves, err)
		}
		out.pages = append(out.pages, time.Since(pageStart).Round(time.Microsecond))
		out.pageRows = append(out.pageRows, len(page))
		if len(page) == 0 {
			break
		}
		out.entries += len(page)
		after = page[len(page)-1].Ordinal
	}
	return out
}

// seedSinkSizes are the two seed counts the push-sink proof pushes. They differ
// by an order of magnitude so a peak that tracked the seed count could not land
// on the same number twice.
var seedSinkSizes = []int{5000, 50000}

// TestTheSeedSinkPeaksOnTheRunBufferAtEitherSeedCount is ruling C10's proof.
//
// Before the ruling, discovery accumulated every admitted seed in a slice and a
// dedupe map beside it, and both lived until the expansion consumed them: the
// compile's peak was a function of how many seeds the repository answered. Now
// a producer pushes each seed into the sort area as it finds it.
//
// Two assertions, both structural and neither a measurement:
//
//   - the seed sorts peak at the SAME record count at 5 000 seeds and at
//     50 000, so the sink's live working set is the run buffer; and
//   - the fold still keeps the EARLIEST seed for each entity, so removing the
//     `seen` map did not turn the dedupe into a last-writer-wins.
//
// Heap-in-use is logged rather than asserted: this package also runs under
// -race, where ReadMemStats deltas wander, and the peak above is the invariant
// the heap number merely illustrates.
func TestTheSeedSinkPeaksOnTheRunBufferAtEitherSeedCount(t *testing.T) {
	cfg := config.Defaults()
	peaks := make([]map[string]int, 0, len(seedSinkSizes))
	for _, seeds := range seedSinkSizes {
		sorts := openSorts(t, cfg)
		// The smallest run budget the primitive honours, so a few thousand
		// records already outgrow it and the peak below is the budget's.
		sorts.runBytes = pagination.SortRunBytes(1)
		c := &Compiler{cfg: cfg}
		in, err := c.newSeedIngest(sorts)
		if err != nil {
			t.Fatalf("newSeedIngest: %v", err)
		}
		in.BeginStep(originExplicitSeed, "the identities the task names")
		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		for i := range seeds {
			// Every entity is offered TWICE, adjacent, so the fold has real
			// work at both sizes and the winner is checkable.
			for _, req := range []model.Requirement{model.RequirementFull, model.RequirementOptional} {
				if err := in.Admit(candidate{NodeID: model.NodeID(fmt.Sprintf("n%08d", i)),
					Requirement: req, Origin: originExplicitSeed}); err != nil {
					t.Fatalf("Admit: %v", err)
				}
			}
		}
		// Collected before the second reading: what is interesting is what the
		// push LEFT LIVE, not the garbage it produced on the way.
		runtime.GC()
		runtime.ReadMemStats(&after)
		start, err := in.foldSeeds()
		if err != nil {
			t.Fatalf("foldSeeds: %v", err)
		}
		if len(start) != min(seeds, model.MaxStartNodes) {
			t.Fatalf("%d seeds produced %d walk roots, want %d",
				seeds, len(start), min(seeds, model.MaxStartNodes))
		}
		// The fold keeps the earliest arrival per entity, which is the one
		// carrying RequirementFull: a last-writer-wins fold answers Optional.
		run, err := in.entitySort.Sorted()
		if err != nil {
			t.Fatalf("Sorted: %v", err)
		}
		kept := 0
		if err := run.Each(func(r candRec) error {
			kept++
			if r.Requirement != model.RequirementFull {
				return fmt.Errorf("entity %s survived the fold as %q, want the EARLIEST arrival (%q)",
					r.entityID(), r.Requirement, model.RequirementFull)
			}
			return nil
		}); err != nil {
			t.Fatalf("the seed fold: %v", err)
		}
		run.Close()
		if kept != seeds {
			t.Fatalf("%d distinct entities offered twice each folded to %d, want %d", seeds, kept, seeds)
		}
		if err := sorts.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		peak := map[string]int{}
		for _, o := range sorts.observations() {
			peak[o.Name] = o.Peak
		}
		t.Logf("seeds=%d heapInUse %d -> %d bytes (delta %d) peaks=%v",
			seeds, before.HeapInuse, after.HeapInuse, int64(after.HeapInuse)-int64(before.HeapInuse), peak)
		peaks = append(peaks, peak)
	}
	for name, want := range peaks[0] {
		got, ok := peaks[1][name]
		if !ok {
			t.Errorf("sort %q was opened at %d seeds and not at %d", name, seedSinkSizes[0], seedSinkSizes[1])
			continue
		}
		if got != want {
			t.Errorf("sort %q peaked at %d records over %d seeds and %d records over %d seeds: the seed "+
				"sink's live working set tracks the seed count, not the run budget",
				name, want, seedSinkSizes[0], got, seedSinkSizes[1])
		}
	}
}
