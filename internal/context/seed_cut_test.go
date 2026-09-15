package context

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Sawmonabo/codectx/internal/config"
	"github.com/Sawmonabo/codectx/internal/model"
)

// TestTheSeedCutSurvivesIntoAValidManifestExclusion protects row 20's seed cut
// end to end, through the one place it can go wrong silently.
//
// Failure mode it guards: seed discovery used to stop at model.MaxSeeds without
// saying so. Reporting the cut is only an improvement if the row it produces is
// a VALID exclusion — model.ContextReference refuses a reference that names
// neither a node, a file nor a path, so a cut row carrying only a reason would
// convert a silent truncation into a failed context compile every time a task
// named more than MaxSeeds identities. No compiler fixture exceeds 64 seeds, so
// nothing else in this package would catch that.
func TestTheSeedCutSurvivesIntoAValidManifestExclusion(t *testing.T) {
	const limit = config.Limit(1)
	cfg := config.Defaults()
	cfg.Context.MaxSeeds = limit
	sorts := openSorts(t, cfg)
	defer sorts.Close()
	c := &Compiler{cfg: cfg}
	in, err := c.newSeedIngest(sorts)
	if err != nil {
		t.Fatalf("newSeedIngest: %v", err)
	}
	// Two DIFFERENT steps that share one originKind. Keyed on the origin, the
	// second step's cut vanished and the manifest said the step ran whole, so
	// the disclosure is per STEP and the two rows below are the assertion.
	for _, step := range []string{"the captured working-tree changes", "a second step under the same origin"} {
		in.BeginStep(originChangedFile, step)
		for i := 0; i < 2; i++ {
			if err := in.Admit(candidate{NodeID: model.NodeID(fmt.Sprintf("n-%s-%d", step, i)),
				Origin: originChangedFile}); err != nil {
				t.Fatalf("Admit: %v", err)
			}
		}
	}
	if _, err := in.foldSeeds(); err != nil {
		t.Fatalf("foldSeeds: %v", err)
	}
	if in.scope.ScopeComplete {
		t.Fatal("a seed cut left the scope reported as complete")
	}
	var cuts []candidate
	run, err := in.candSort.Sorted()
	if err != nil {
		t.Fatalf("Sorted: %v", err)
	}
	defer run.Close()
	if err := run.Each(func(r candRec) error {
		if strings.Contains(r.Excluded, "context.max_seeds") {
			cuts = append(cuts, r.finalCandidate())
		}
		return nil
	}); err != nil {
		t.Fatalf("Each: %v", err)
	}
	if len(cuts) != 2 {
		t.Fatalf("a bound of %s over two steps produced %d cut rows, want one per stopped step", limit, len(cuts))
	}

	// Row 20's other half: a step that stopped at a PAGE boundary rather than
	// at context.max_seeds is a different bound and must be disclosed separately, with
	// the continuation cursor, so the caller knows the read is resumable.
	out := seedSet{sink: &collectSeeds{}}
	out.notePageEnd(originLexical, "the lexical matches of the task text", "cursor-1")
	out.notePageEnd(originLexical, "the lexical matches of the task text", "cursor-1")
	if len(out.Excluded) != 1 {
		t.Fatalf("a repeated page end produced %d rows, want exactly one", len(out.Excluded))
	}
	if !strings.Contains(out.Excluded[0].Excluded, "cursor-1") {
		t.Fatalf("the page-end reason %q does not carry the continuation cursor", out.Excluded[0].Excluded)
	}

	// Reporting the cut is only an improvement if the row it produces is a
	// VALID exclusion: model.ContextReference refuses a reference that names
	// neither a node, a file nor a path, so a cut row carrying only a reason
	// would convert a silent truncation into a failed context compile every
	// time a task named more than the bound allows.
	rows := append(append([]candidate(nil), cuts...), out.Excluded...)
	p, err := buildPlan(rows, nil, resolvedBudget{MaxBytes: 1 << 20, MaxTokens: 1 << 20, MaxFiles: 8, MaxSlices: 8})
	if err != nil {
		t.Fatalf("budgeting the cut row failed: %v", err)
	}
	if len(p.Excluded) != len(rows) {
		t.Fatalf("the plan carries %d exclusions, want the %d cut rows", len(p.Excluded), len(rows))
	}
	for i := range p.Excluded {
		if err := p.Excluded[i].Validate(); err != nil {
			t.Fatalf("the disclosure row %d is not a valid manifest exclusion: %v", i, err)
		}
	}
}

// The fixture publishes exactly one captured change (contextFixture.putBlob
// tracks every file but the implementation), so a page size of one is the exact
// boundary: page 1 is full, page 2 is empty, and only the second read can tell
// the difference.
func TestChangedFileSeedsExhaustTheWorkingTreeAtAPageBoundary(t *testing.T) {
	fx := newContextFixture(t)
	// One row per read, over one changed file: len(page) == pageSize on the
	// page that is also the last one.
	fx.Cfg.Resources.MaxPageItems = 1
	c := intCompiler(t, fx, fx.Now)
	reader, err := c.store.PinGeneration(fx.ctx, c.repo, fx.Binding.GenerationID, time.Minute)
	if err != nil {
		t.Fatalf("PinGeneration: %v", err)
	}
	defer reader.Close()

	sink := &collectSeeds{}
	out := seedSet{sink: sink}
	if err := c.changedFileSeeds(fx.ctx, reader, &out); err != nil {
		t.Fatalf("changedFileSeeds: %v", err)
	}
	var changed []string
	for _, cand := range sink.cands {
		if cand.Origin == originChangedFile {
			changed = append(changed, cand.Path)
		}
	}
	if len(changed) != 1 {
		t.Fatalf("the step admitted %v from a working tree with one captured change", changed)
	}
	if out.Unresolved {
		t.Errorf("a working tree whose changed-file count equals the page size was reported INCOMPLETE; "+
			"the exclusions say %v. A full page is a page boundary, not a remainder: only the next read answers that",
			out.Excluded)
	}
	if len(out.Excluded) != 0 {
		t.Errorf("the step disclosed %v over a complete working tree; a page end that did not happen must not be named",
			out.Excluded)
	}
}

// TestChangedFileSeedsKeepReadingPastTheFirstPage is the OTHER half of the
// bound removal, and the half wave-h review finding A5 records the test above
// as unable to prove.
//
// The test above publishes exactly one captured change, so page 1 is full and
// page 2 is empty: it proves that a full last page is not mistaken for a
// remainder, and nothing else. A step that read page 1 and stopped would pass
// it unchanged, because with one change page 1 IS the whole working tree. The
// failure mode that made the bound worth removing -- a branch with more changed
// files than one page contributing the arbitrary prefix a page boundary
// happened to cut -- needs a working tree whose changes outnumber one page, and
// an assertion that names a change only the SECOND read can reach.
//
// The generated fixture publishes generatedChanged captured changes; at a page
// size of one, the last of them is generatedChanged reads in.
func TestChangedFileSeedsKeepReadingPastTheFirstPage(t *testing.T) {
	if generatedChanged < 2 {
		t.Fatalf("the generated fixture publishes %d captured changes; one page cannot be exceeded",
			generatedChanged)
	}
	fx := newGeneratedFixture(t)
	// One changed file per read, over generatedChanged of them: every change
	// but the first lies beyond the first page.
	fx.Cfg.Resources.MaxPageItems = 1
	c := intCompiler(t, fx, fx.Now)
	reader, err := c.store.PinGeneration(fx.ctx, c.repo, fx.Binding.GenerationID, time.Minute)
	if err != nil {
		t.Fatalf("PinGeneration: %v", err)
	}
	defer reader.Close()

	sink := &collectSeeds{}
	out := seedSet{sink: sink}
	if err := c.changedFileSeeds(fx.ctx, reader, &out); err != nil {
		t.Fatalf("changedFileSeeds: %v", err)
	}
	seen := map[string]bool{}
	for _, cand := range sink.cands {
		if cand.Origin == originChangedFile {
			seen[cand.Path] = true
		}
	}
	var missing []string
	for i := 0; i < generatedChanged; i++ {
		if !seen[genLeaf(i)] {
			missing = append(missing, genLeaf(i))
		}
	}
	if len(missing) > 0 {
		t.Fatalf("the step admitted %d of the %d captured changes at a page size of 1 and missed %v; "+
			"it stopped at a page boundary instead of reading the working tree to the end",
			len(seen), generatedChanged, missing)
	}
	if out.Unresolved {
		t.Errorf("a working tree the step read to the end was reported INCOMPLETE: %v", out.Excluded)
	}
}
