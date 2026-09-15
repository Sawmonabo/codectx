package context

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Sawmonabo/codectx/internal/config"
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
	const limit = config.Limit(2)
	var s seedSet
	s.noteCut(originChangedFile, "the captured working-tree changes", limit)
	s.noteCut(originChangedFile, "the captured working-tree changes", limit)
	if !s.Unresolved {
		t.Fatal("a seed cut left the scope reported as complete")
	}
	if len(s.Excluded) != 1 {
		t.Fatalf("one step's cut produced %d exclusion rows, want exactly one", len(s.Excluded))
	}
	// Two DIFFERENT steps that share one originKind are two cuts, and the
	// dedupe must not fold them: keyed on the origin, the second step's cut
	// vanished and the manifest said the step ran whole.
	s.noteCut(originChangedFile, "a second step under the same origin", limit)
	if len(s.Excluded) != 2 {
		t.Fatalf("two steps sharing an origin produced %d exclusion rows, want one per step", len(s.Excluded))
	}

	// Row 20's other half: a step that stopped at a PAGE boundary rather than
	// at context.max_seeds is a different bound and must be disclosed separately, with
	// the continuation cursor, so the caller knows the read is resumable.
	s.notePageEnd(originLexical, "the lexical matches of the task text", "cursor-1")
	s.notePageEnd(originLexical, "the lexical matches of the task text", "cursor-1")
	if len(s.Excluded) != 3 {
		t.Fatalf("a page end produced %d rows in total, want one more than the two cut rows", len(s.Excluded))
	}
	if !strings.Contains(s.Excluded[2].Excluded, "cursor-1") {
		t.Fatalf("the page-end reason %q does not carry the continuation cursor", s.Excluded[2].Excluded)
	}

	p, err := buildPlan(s.Excluded, nil, resolvedBudget{MaxBytes: 1 << 20, MaxTokens: 1 << 20, MaxFiles: 8, MaxSlices: 8})
	if err != nil {
		t.Fatalf("budgeting the cut row failed: %v", err)
	}
	if len(p.Excluded) != len(s.Excluded) {
		t.Fatalf("the plan carries %d exclusions, want the %d cut rows", len(p.Excluded), len(s.Excluded))
	}
	if err := p.Excluded[0].Validate(); err != nil {
		t.Fatalf("the cut exclusion is not a valid manifest row, so reporting the cut would fail the compile: %v", err)
	}
	// VF6: the bound that cuts discovery is the operator's configuration key,
	// so the disclosure must name the key AND the value they set. A reason that
	// named a build constant told the reader nothing they could act on.
	if !strings.Contains(p.Excluded[0].Reason, "context.max_seeds") ||
		!strings.Contains(p.Excluded[0].Reason, fmt.Sprintf("%d", int64(limit))) {
		t.Fatalf("the exclusion reason %q does not name context.max_seeds and the configured value", p.Excluded[0].Reason)
	}
}

// TestUnlimitedSeedDiscoveryExaminesEveryIdentityTheTaskNames is VF6's other
// half: context.max_seeds defaults to unlimited, and unlimited must mean the
// discovery steps keep going rather than stopping at a build constant.
//
// Failure mode it guards: restoring ANY hard-coded seed ceiling. Every step in
// seeds.go asks seedsFull, so a ceiling reintroduced there stops the unlimited
// walk below at that number and stops the configured walk at the wrong one.
func TestUnlimitedSeedDiscoveryExaminesEveryIdentityTheTaskNames(t *testing.T) {
	// Well past the 64 the build used to stop at, so a restored constant is a
	// failure and not a coincidence.
	const named = 200
	var b strings.Builder
	for i := 0; i < named; i++ {
		fmt.Fprintf(&b, "`pkg/sub%d/file%d.go` ", i, i)
	}
	task := b.String()

	if got := len(taskTokens(task, config.Unlimited)); got != named {
		t.Fatalf("unlimited discovery examined %d of the %d identities the task names", got, named)
	}
	if got := len(freeTerms(task, config.Unlimited)); got != named {
		t.Fatalf("unlimited free-term discovery examined %d of the %d terms the task names", got, named)
	}
	// A user-set bound is the ONLY thing that stops it, and it stops it exactly
	// where the operator asked.
	if got := len(taskTokens(task, config.Limit(10))); got != 10 {
		t.Fatalf("context.max_seeds = 10 admitted %d identities", got)
	}
	if seedsFull(config.Unlimited, named) {
		t.Fatal("an unlimited bound reported the seed set full")
	}
	if !seedsFull(config.Limit(10), 10) {
		t.Fatal("a bound of 10 did not report the seed set full at 10 candidates")
	}
}

// TestChangedFileSeedsExhaustTheWorkingTreeAtAPageBoundary is the exact-boundary
// half of Section 15.2 step 6.
//
// Failure mode it protects, in two halves. A working tree whose changed-file
// count is an exact multiple of the read's page size is COMPLETE, and a step
// that infers "changes remain" from a full page reports such a scope incomplete
// with a page-end exclusion naming files that do not exist -- an actor then
// widens a plan that was already whole. The other half is the one that made the
// bound worth removing: the step must keep reading past the first page, so a
// branch with more changed files than one page contributes all of them and not
// the arbitrary prefix a page boundary happened to cut.
//
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

	var out seedSet
	if err := c.changedFileSeeds(fx.ctx, reader, map[string]bool{}, &out); err != nil {
		t.Fatalf("changedFileSeeds: %v", err)
	}
	var changed []string
	for _, cand := range out.Candidates {
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
