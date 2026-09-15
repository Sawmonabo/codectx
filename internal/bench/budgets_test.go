package bench

import (
	"fmt"
	"testing"
)

// This file is the Section 23.2 release gate and the published benchmark set
// (lane T21-L2). It has no TestMain of its own: plateau_test.go's makes this
// test binary the parser worker, which is what lets a measurement here spawn
// parsers the way the shipped binary does. The budget constants and the corpus
// generator it measures against are corpus_test.go's; nothing is redeclared.
//
// L0 skeleton: every row below skips with the reason it cannot yet measure, so
// `go test -run TestResourceBudgets` and `go test -bench .` already SELECT the
// functions Step 4 names instead of silently matching nothing. L2 replaces each
// skip with a measurement; a row that keeps its skip after L2 is an unmet
// obligation, not a pass.

// --- the corpus scales beyond corpusTiny ------------------------------------
//
// corpusSpec, generateCorpus and corpusTiny are corpus_test.go's. These are the
// two further scales Section 23.1 asks for plus the shape obligation 7 needs.
// Seeds are fixed, so a spec is a workload and not a coin flip.
//
// generateCorpus writes 3 source files per package (Go, Python, TypeScript)
// plus 4 fixed files (the high-fanout Go file, go.mod, package.json,
// README.md), so a spec of P packages produces 3P+4 files. That arithmetic is
// what the counts below are chosen against.

// corpusSmallReal is the scale a 30-minute lane can measure on: 124 files,
// every required language, one high-fanout case. It is "small but real" in the
// Section 23.1 sense -- a workload with the same shape as the reference
// fixture, not a toy with one file per language.
var corpusSmallReal = corpusSpec{Packages: 40, Seed: 2101}

// corpusOver200Files is obligation 7's shape and nothing else: 214 files, which
// is the smallest round number over the 200-file statusLimit clamp. This
// repository cannot prove that clamp -- a session over it selects far fewer
// files -- which is why the shape is generated rather than borrowed.
var corpusOver200Files = corpusSpec{Packages: 70, Seed: 2102}

// corpusReference is the Section 23.1 reference fixture: 10,000 files. It is
// the VERIFY lane's workload, never a lane's -- the cold-index row alone has a
// 3-minute budget and -count=5 multiplies it -- and it is declared here so the
// reference scale has exactly one definition.
var corpusReference = corpusSpec{Packages: 3332, Seed: 2103}

// --- the release gate -------------------------------------------------------

// budgetRow is one Section 23.2 ceiling: what is measured, the frozen target,
// the corpus scale it is measured at, and -- until L2 lands -- why it is not
// measured yet.
type budgetRow struct {
	name   string
	target string
	spec   corpusSpec
	skip   string
}

// budgetRows is the whole Section 23.2 table. Every target is rendered from the
// frozen constant rather than retyped, so a constant and a gate can never drift
// apart: there is one number and both read it.
var budgetRows = []budgetRow{
	{"version-startup", budgetVersionP95.String(), corpusSmallReal,
		"L2: p95 of `codectx version --json` over a release build (-trimpath, no race, no coverage)"},
	{"exact-query", budgetExactQueryP95.String(), corpusSmallReal,
		"L2: p95 of an exact symbol/path query"},
	{"lexical-search", budgetLexicalSearchP95.String(), corpusSmallReal,
		"L2: p95 of a generation-local FTS search"},
	{"one-hop", budgetOneHopP95.String(), corpusSmallReal,
		"L2: p95 of a one-hop caller/callee traversal"},
	{"context-plan", budgetContextPlanP95.String(), corpusSmallReal,
		"L2: p95 of a context plan visiting at most the node budget"},
	{"context-plan-visited", fmt.Sprintf("%d nodes", budgetContextPlanVisitedNodes), corpusSmallReal,
		"L2: visited-node count of the planned traversal, read from the plan's own accounting"},
	{"ten-file-refresh", budgetTenFileRefreshP95.String(), corpusSmallReal,
		"L2: p95 of a ten-file base refresh measured after the debounce window"},
	{"no-change-refresh", budgetNoChangeRefreshP95.String(), corpusSmallReal,
		"L2: p95 of a no-change refresh, asserted on the reuse/parse counters and the absence of an " +
			"FTS body rewrite -- not on wall time, which passes on a fast host either way"},
	{"cold-index-reference", budgetColdIndexReference.String(), corpusReference,
		"VERIFY: the reference-fixture cold index is the one full-scale pass of the wave, not a lane's"},
	{"idle-mcp-rss", fmt.Sprintf("%d MiB", budgetIdleMCPRSSBytes>>20), corpusSmallReal,
		"L2: resident set of an idle `codectx mcp serve` process"},
	{"interactive-peak", fmt.Sprintf("%d MiB", budgetInteractivePeakBytes>>20), corpusSmallReal,
		"L2: peak resident set of the whole process tree during interactive queries, parser workers included"},
	{"indexing-peak", fmt.Sprintf("%d MiB", budgetIndexingPeakBytes>>20), corpusSmallReal,
		"L2: peak resident set of the whole process tree during indexing, parser workers included"},
	{"storage-ratio", fmt.Sprintf("%.1fx eligible source bytes", budgetStorageRatio), corpusSmallReal,
		"L2: on-disk base storage over eligible source bytes"},
}

// TestResourceBudgets is the Section 23.2 release gate.
//
// Failure mode it protects: a measurement over an absolute ceiling is reported
// as a pass. Section 23.5 is explicit that no target may be met by excluding
// the failing feature or by rewriting the benchmark after seeing the result, so
// a row that misses must FAIL with the measured number beside the target -- a
// gate that cannot fail is not a gate.
func TestResourceBudgets(t *testing.T) {
	if testing.Short() {
		t.Skip("resource budgets; run without -short")
	}
	for _, row := range budgetRows {
		t.Run(row.name, func(t *testing.T) {
			// L2 replaces this skip with the measurement. The target and the
			// corpus scale are logged either way, so a skipped run still says
			// what it would have measured and against what.
			t.Logf("target %s at %d packages (seed %d)", row.target, row.spec.Packages, row.spec.Seed)
			t.Skip(row.skip)
		})
	}
}

// TestSessionStatusClamp is obligation 7: a session over more than 200 files
// clamps at statusLimit and REPORTS the clamp.
//
// Failure mode it protects: a large session's status reads as complete when it
// was truncated, so an actor consolidates on a coverage picture that is missing
// files nobody told it about. It is separate from the budget table because it
// is a correctness row, not a ceiling, and it is the only consumer of
// corpusOver200Files.
func TestSessionStatusClamp(t *testing.T) {
	if testing.Short() {
		t.Skip("synthetic >200-file session; run without -short")
	}
	t.Logf("shape: %d packages -> %d files (seed %d)",
		corpusOver200Files.Packages, 3*corpusOver200Files.Packages+4, corpusOver200Files.Seed)
	t.Skip("L2: plan a session over the >200-file corpus and assert the clamp is applied AND reported")
}

// --- the published benchmarks ----------------------------------------------
//
// Step 4 runs `go test ./internal/bench -run '^$' -bench . -benchmem -count=5`,
// which selects by FUNCTION name, not by file. Each stub skips with the reason
// it is not measured yet rather than running an empty body: an empty benchmark
// reports a meaningless 0 ns/op that reads as a result.

// BenchmarkVersionStartup measures process startup: `codectx version --json`
// opens no database, loads no grammar and detects no analyzer (Section 7.2).
func BenchmarkVersionStartup(b *testing.B) {
	b.Skip("L2: release-build startup benchmark")
}

// BenchmarkExactQuery measures exact symbol and path resolution.
func BenchmarkExactQuery(b *testing.B) {
	b.Skip("L2: exact symbol/path query benchmark")
}

// BenchmarkLexicalSearch measures generation-local lexical retrieval.
func BenchmarkLexicalSearch(b *testing.B) {
	b.Skip("L2: FTS search benchmark")
}

// BenchmarkOneHopTraversal measures a one-hop caller/callee neighborhood.
func BenchmarkOneHopTraversal(b *testing.B) {
	b.Skip("L2: one-hop traversal benchmark")
}

// BenchmarkContextPlan measures manifest compilation over a bounded traversal.
func BenchmarkContextPlan(b *testing.B) {
	b.Skip("L2: context plan benchmark")
}

// BenchmarkTenFileRefresh measures an incremental refresh of ten changed files.
func BenchmarkTenFileRefresh(b *testing.B) {
	b.Skip("L2: ten-file incremental refresh benchmark")
}

// BenchmarkNoChangeRefresh measures the reuse path: a refresh with nothing
// changed, which must do no parser work and rewrite no FTS body.
func BenchmarkNoChangeRefresh(b *testing.B) {
	b.Skip("L2: no-change refresh benchmark")
}
