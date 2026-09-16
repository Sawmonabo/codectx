package bench

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Sawmonabo/codectx/internal/app"
	"github.com/Sawmonabo/codectx/internal/config"
	"github.com/Sawmonabo/codectx/internal/diagnostics"
	"github.com/Sawmonabo/codectx/internal/model"
	_ "modernc.org/sqlite"
)

// This file is the Section 23.2 release gate and the published benchmark set.
// It has no TestMain of its own: plateau_test.go's makes this
// test binary the parser worker, which is what lets a measurement here spawn
// parsers the way the shipped binary does. The budget constants and the corpus
// generator it measures against are corpus_test.go's; nothing is redeclared.
//
// How each row is measured, and why the two instruments differ:
//
//   - LATENCY rows are measured in process against app.Services -- the same
//     facade the CLI and the MCP server drive. They cannot be measured by
//     timing a CLI process: Section 23.2 budgets `version --json` startup at
//     50 ms and an exact query at 50 ms, so a whole process per query would
//     have to answer in zero.
//   - MEMORY rows are measured against the exec'd release binary (-trimpath,
//     no race, no coverage), polled with the Task 20 host sampler
//     (internal/diagnostics). The sampler sums THIS process's descendants, and
//     the product under measurement is a descendant, so its tree -- the
//     codectx process plus its parser workers -- is exactly what is summed and
//     this test binary's own footprint is excluded. No second sampler is
//     written here. One honest caveat: the sampler's base/native split means
//     "this executable re-executed as a worker" vs "everything else", and the
//     executable here is the test binary, so both halves of the product tree
//     land in the native half. The rows therefore report the SUM, which is the
//     figure Section 23.2 names.
//   - The STORAGE row is read from the product's own accounting
//     (`IndexStatus` with Resources set, the Task 20 block), not re-derived by
//     walking directories a second time.

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

// corpusOver200Files is obligation 7's shape and nothing else: 364 files. The
// count that has to clear the 200-record page is the SESSION's file count, not
// the repository's, and neither one plan nor one repository size reaches it --
// a plan over this generator saturates at ~161 files however large the tree is
// (the figures are in TestSessionStatusClamp), so the session is grown through
// `context include` instead. The
// repository still has to be large enough to hold a 200-file session with room
// above it, which this repository and the 214-file shape are not: 3*120+4 files
// leave the gate clear of its own fixture's ceiling.
var corpusOver200Files = corpusSpec{Packages: 120, Seed: 2102}

// corpusReference is the Section 23.1 reference fixture, and Section 23.1
// states it in three numbers, not one: ~10,000 files, ~1M lines, ~80 MiB of
// source. The package count reaches the file count (3P+4), and the wide filler
// shape reaches the other two -- the original narrow shape gives 10,000 files
// of ~0.5 KiB, which is a quarter of the lines and a fourteenth of the bytes,
// and a cold index or a storage ratio measured over that describes a corpus
// the budgets were not written for.
//
// Generated and counted at this shape (no index): 10,000 files, 1,052,933
// lines, 86,064,203 bytes = 82.08 MiB. corpora.json records CLONED corpora --
// it demands an https clone url and a pinned commit per entry -- so a
// generated fixture is not one of its rows and it is unchanged. This is the
// VERIFY lane's workload, never a lane's -- the cold-index row alone
// has a 3-minute budget and -count=5 multiplies it -- and it is declared here
// so the reference scale has exactly one definition.
var corpusReference = corpusSpec{Packages: 3332, Seed: 2103, Helpers: 4, HelperLines: 17}

// --- the shared fixture -----------------------------------------------------

// budgetFixture is the one indexed workspace every latency row, every memory
// row and every benchmark measures against. It is built once per test binary:
// Section 23.2 has thirteen rows and `-bench . -count=5` re-enters every
// benchmark five times, so an index per row would turn a minutes-long run into
// an hours-long one and would measure the generator rather than the product.
type budgetFixture struct {
	repo string
	// home is the isolated XDG root of the in-process workspace, and dataDir
	// its storage. execHome is a SECOND root over the same repository: the
	// in-process workspace holds the Section 13.2 workspace lock for its
	// lifetime, so an exec'd `codectx index` needs its own data directory
	// rather than a second claim on this one.
	home, dataDir, execHome string
	binary                  string
	svc                     *app.Services
	// seeds are the workspace symbols the traversal rows start from, and
	// seedName one exact symbol name for the exact-query row.
	seeds    []model.NodeID
	seedName string
	// sourceBytes and resources are the product's own accounting, captured
	// once after the cold index for the storage-ratio row.
	sourceBytes uint64
	resources   model.ResourceReport
}

var (
	fixtureOnce  sync.Once
	fixtureValue *budgetFixture
)

// sharedRoot is the fixture's directory under benchRoot rather than a
// t.TempDir because the fixture outlives the test or benchmark that happens to
// build it first: a TempDir would be removed when that one function returned,
// taking the corpus out from under every later row. Creating it is all this
// does -- plateau_test.go's TestMain owns the root's lifetime, which is what
// makes it outlive every row and be released at process exit.
func sharedRoot(tb testing.TB) string {
	root := filepath.Join(benchRoot, "fixture")
	if err := os.MkdirAll(root, 0o700); err != nil {
		tb.Fatalf("create shared fixture root: %v", err)
	}
	return root
}

// fixture returns the shared indexed workspace, building it on first use.
func fixture(tb testing.TB) *budgetFixture {
	tb.Helper()
	fixtureOnce.Do(func() { fixtureValue = buildFixture(tb) })
	if fixtureValue == nil {
		tb.Skip("the shared budget fixture failed to build; the first row to use it reported why")
	}
	return fixtureValue
}

// benchStartNodes is the seed width the traversal rows walk from. It is the
// bench's OWN fixture constant and not a model or configuration bound: the row
// has to measure the same walk from build to build, so widening the wire
// ceiling (model.MaxStartNodes) or the operative context.max_start_nodes must
// not move the baseline the budget is re-pinned against.
const benchStartNodes = 64

func buildFixture(tb testing.TB) *budgetFixture {
	tb.Helper()
	ctx := context.Background()
	root := sharedRoot(tb)
	f := &budgetFixture{
		repo:     filepath.Join(root, "repo"),
		home:     filepath.Join(root, "home"),
		execHome: filepath.Join(root, "exec-home"),
	}
	if err := os.MkdirAll(f.repo, 0o700); err != nil {
		tb.Fatalf("create corpus directory: %v", err)
	}
	generateCorpus(tb, f.repo, corpusSmallReal)
	f.binary = releaseBinary(tb)
	// The exec'd rows get their own configuration as well as their own data
	// directory: without it the binary would read this host's real user
	// configuration, index with the optional providers on and fetch managed
	// payloads over the network, and the memory rows would describe a
	// different product than the latency rows do.
	writeConfig(tb, f.execHome, filepath.Join(f.execHome, "data"), "")
	// Deliberately not closed: the fixture is process-scoped (see
	// openWorkspace), and the test binary's exit releases the workspace lock
	// and the database handle.
	f.svc, f.dataDir, _ = openWorkspace(tb, ctx, f.repo, f.home, "")

	status, err := f.svc.IndexStatus(ctx, model.StatusRequest{Resources: true})
	if err != nil {
		tb.Fatalf("index status: %v", err)
	}
	f.sourceBytes = status.SourceBytes
	if status.Resources == nil {
		tb.Fatal("status --resources answered without the Section 23 accounting block")
	}
	f.resources = *status.Resources

	symbols, err := f.svc.Symbol(ctx, model.SymbolRequest{
		Query: corpusPrefix, Operation: model.SymbolWorkspaceSymbols, SemanticSource: model.SemanticCanonical})
	if err != nil {
		tb.Fatalf("workspace symbols: %v", err)
	}
	// The traversal rows start from benchStartNodes seeds, led by the
	// high-fanout symbol Section 23.1 asks for. A walk from one leaf visits two nodes and would measure nothing,
	// and the visited-node row has to state an upper bound the default budget
	// is re-pinned against, not a best case. The exact-query row wants the
	// opposite: one ordinary symbol, resolved by name.
	for _, n := range symbols.Items {
		if n.Kind == model.NodeFunction && strings.HasPrefix(n.Name, corpusPrefix+"GoRun") {
			f.seedName = n.Name
			break
		}
	}
	// The fan-out symbol is asked for by name: the prefix query above answers
	// one bounded page, and at this scale the page fills long before it.
	fanout, err := f.svc.Symbol(ctx, model.SymbolRequest{Query: corpusPrefix + "GoFanOut",
		Operation: model.SymbolWorkspaceSymbols, SemanticSource: model.SemanticCanonical})
	if err != nil {
		tb.Fatalf("fan-out symbol: %v", err)
	}
	for _, n := range fanout.Items {
		if n.Kind == model.NodeFunction && n.Name == corpusPrefix+"GoFanOut" {
			f.seeds = append(f.seeds, n.ID)
			break
		}
	}
	for _, n := range symbols.Items {
		if len(f.seeds) >= benchStartNodes {
			break
		}
		if n.Kind == model.NodeFunction || n.Kind == model.NodeMethod {
			f.seeds = append(f.seeds, n.ID)
		}
	}
	if len(f.seeds) == 0 || f.seedName == "" {
		tb.Fatalf("the corpus produced no %sGoFanOut/%sGoRun* functions to measure against",
			corpusPrefix, corpusPrefix)
	}
	return f
}

// openWorkspace writes an isolated configuration, opens the workspace over it
// and indexes the corpus once, returning the services and the directory the
// database was written to.
//
// It is budgets_test.go's own opener rather than corpus_test.go's openCorpus
// because the two have different jobs: this one takes a testing.TB (every
// benchmark below needs it), hands back the data directory the no-change row
// reads the lexical tables from, and takes the extra configuration the
// low-memory row varies worker counts and reservations with. openCorpus does
// none of the three and is the parity harness's.
func openWorkspace(tb testing.TB, ctx context.Context, repo, home, extra string) (*app.Services, string, func()) {
	tb.Helper()
	dataDir := filepath.Join(home, "data")
	writeConfig(tb, home, dataDir, extra)
	restore := useConfigHome(tb, home)
	w, err := app.OpenWorkspace(ctx, repo, app.OpenOptions{Operation: "index"})
	// The environment is restored as soon as the workspace has read it: a
	// shared fixture outlives the function that built it, and leaving HOME
	// pointing into the fixture would silently reconfigure every later row.
	restore()
	if err != nil {
		tb.Fatalf("open workspace: %v", err)
	}
	svc := w.Services()
	if _, err := svc.Index(ctx, model.IndexRequest{}); err != nil {
		_ = w.Close()
		tb.Fatalf("index: %v", err)
	}
	// The close is returned rather than registered on tb: the shared fixture
	// outlives whichever test or benchmark happened to build it, and a
	// tb.Cleanup would close the workspace the moment that one function
	// returned -- every later row then answers "sql: database is closed".
	return svc, dataDir, func() { _ = w.Close() }
}

// writeConfig writes the user-level configuration the measurements run under.
// The three optional providers are off: an "auto" provider that is unavailable
// on this host would make a budget a statement about the host. extra is
// appended verbatim so a row can vary one block.
func writeConfig(tb testing.TB, home, dataDir, extra string) {
	tb.Helper()
	dir := filepath.Join(home, "config", "codectx")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		tb.Fatalf("create config directory: %v", err)
	}
	body := "[storage]\ndata_dir = \"" + dataDir + "\"\n" +
		"[tools]\noffline = true\n" +
		"[providers.scip]\nenabled = false\n[providers.lsp]\nenabled = false\n" +
		"[providers.dependence]\nenabled = false\n" + extra
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(body), 0o600); err != nil {
		tb.Fatalf("write config: %v", err)
	}
}

// useConfigHome points the process at home for the duration of one workspace
// open and returns the restore. testing.T.Setenv is not usable here: it is
// refused in a benchmark, and every benchmark below opens a workspace.
func useConfigHome(tb testing.TB, home string) func() {
	tb.Helper()
	saved := map[string]string{}
	for k, v := range configEnv(home) {
		saved[k] = os.Getenv(k)
		if err := os.Setenv(k, v); err != nil {
			tb.Fatalf("set %s: %v", k, err)
		}
	}
	return func() {
		for k, v := range saved {
			_ = os.Setenv(k, v)
		}
	}
}

func configEnv(home string) map[string]string {
	return map[string]string{
		"HOME":            home,
		"XDG_CONFIG_HOME": filepath.Join(home, "config"),
		"XDG_CACHE_HOME":  filepath.Join(home, "cache"),
		"XDG_DATA_HOME":   filepath.Join(home, "share"),
	}
}

// execEnv is the isolated environment an exec'd release binary runs under. PATH
// is carried through because the process re-execs itself as its parser worker.
func execEnv(home string) []string {
	env := []string{"PATH=" + os.Getenv("PATH")}
	for k, v := range configEnv(home) {
		env = append(env, k+"="+v)
	}
	sort.Strings(env)
	return env
}

var (
	binaryOnce sync.Once
	binaryPath string
	binaryErr  error
)

// releaseBinary builds the product the way Section 23.5 requires a measurement
// to be taken: -trimpath, no race detector, no coverage instrumentation. It is
// built once per test binary, into this process's own root: two concurrent
// bench binaries writing one output path would race the linker against a
// running measurement.
func releaseBinary(tb testing.TB) string {
	tb.Helper()
	binaryOnce.Do(func() {
		dir := filepath.Join(benchRoot, "bin")
		if binaryErr = os.MkdirAll(dir, 0o700); binaryErr != nil {
			return
		}
		binaryPath = filepath.Join(dir, "codectx")
		cmd := exec.Command("go", "build", "-trimpath", "-o", binaryPath, "github.com/Sawmonabo/codectx/cmd/codectx")
		if out, err := cmd.CombinedOutput(); err != nil {
			binaryErr = fmt.Errorf("go build: %v: %s", err, out)
		}
	})
	if binaryErr != nil {
		tb.Fatalf("build the release binary: %v", binaryErr)
	}
	return binaryPath
}

// --- measurement helpers ----------------------------------------------------

// p95 is the Section 23.5 percentile. It is the nearest-rank value, so a
// sample set of n reports a real observation and never an interpolation
// between two.
func p95(samples []time.Duration) time.Duration {
	sorted := append([]time.Duration(nil), samples...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	rank := (len(sorted)*95 + 99) / 100
	if rank < 1 {
		rank = 1
	}
	return sorted[rank-1]
}

// measure runs op n times and returns the p95, failing the row with the
// measured figure beside the target when it is over. Section 23.5 forbids
// meeting a target by rewriting the benchmark after seeing the result, so the
// comparison is made here and the number is reported either way.
func measure(t *testing.T, target time.Duration, n int, op func()) string {
	t.Helper()
	samples := make([]time.Duration, 0, n)
	for range n {
		started := time.Now()
		op()
		samples = append(samples, time.Since(started))
	}
	got := p95(samples)
	report := fmt.Sprintf("p95 %s over %d samples (target %s)", got.Round(time.Microsecond), n, target)
	if raceDetector {
		// Only a row that also asserts on work reaches this under the race
		// detector; the ceiling-only rows skip before they are measured. The
		// clock is still reported, and reported as ungated, so a log line can
		// never read as a pass the gate did not give.
		return report + " -- NOT GATED: " + raceSkipReason
	}
	if got > target {
		t.Errorf("BUDGET MISSED: %s", report)
	}
	return report
}

// treePeak polls the Task 20 host sampler while op runs and returns the highest
// process-tree resident set it observed, in bytes. The parent is excluded: the
// product runs as a descendant of this test binary (see the file comment), so
// the descendant sum is the product's whole tree.
// subjectPeak is treePeak for a row whose subject is ONE spawned process: it
// samples that pid and its descendants and nothing else.
//
// treePeak roots the tree at the test process, which is right for a row that
// measures work this binary does and wrong for a row that measures a child:
// every other descendant the harness happens to be holding -- the shared
// fixture's own workspace, a toolchain process, another row's leftovers -- is
// summed into the subject's figure. The idle-mcp row missed its budget by
// 60 MiB of parser workers belonging to the fixture, not to the server it
// names, which is a measurement that charges one process's memory to another.
func subjectPeak(tb testing.TB, pid int, op func()) uint64 {
	tb.Helper()
	var peak uint64
	sample := func() {
		if sum, ok := subjectTreeRSSBytes(pid); ok && sum > peak {
			peak = sum
		}
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		op()
	}()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-done:
			sample()
			return peak
		case <-tick.C:
			sample()
		}
	}
}

// subjectTreeRSSBytes sums the resident set of root and every process below it
// in one sweep of /proc, so the figures describe one instant. It reports false
// where /proc is unreadable rather than a smaller number, exactly as the
// product's own sampler does: a confidently wrong measurement is worse than a
// missing one.
func subjectTreeRSSBytes(root int) (uint64, bool) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0, false
	}
	type rec struct {
		parent int
		bytes  uint64
	}
	page := uint64(os.Getpagesize())
	all := make(map[int]rec, len(entries))
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 {
			continue
		}
		raw, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue
		}
		// Field 2 is the executable name in parentheses and may itself contain
		// spaces, so the split starts after the last ')'.
		cut := strings.LastIndexByte(string(raw), ')')
		if cut < 0 {
			continue
		}
		fields := strings.Fields(string(raw)[cut+1:])
		const ppidIndex, rssIndex = 1, 21
		if len(fields) <= rssIndex {
			continue
		}
		parent, err := strconv.Atoi(fields[ppidIndex])
		if err != nil {
			continue
		}
		pages, err := strconv.ParseUint(fields[rssIndex], 10, 64)
		if err != nil {
			continue
		}
		all[pid] = rec{parent: parent, bytes: pages * page}
	}
	self, ok := all[root]
	if !ok {
		return 0, false
	}
	total := self.bytes
	for pid, r := range all {
		if pid == root {
			continue
		}
		for hops := 0; hops < 16; hops++ {
			if r.parent == root {
				total += all[pid].bytes
				break
			}
			next, ok := all[r.parent]
			if !ok {
				break
			}
			r = next
		}
	}
	return total, true
}

// workersHeldByThisProcess counts the parser workers this test binary itself
// has running: a worker is this executable re-executed, so it is a child whose
// own executable is this one. A budget row that measures a CHILD must open its
// window with none of these alive, or the harness's own workspace is part of
// the subject's figure whatever the row does.
func workersHeldByThisProcess(tb testing.TB) int {
	tb.Helper()
	self, err := os.Executable()
	if err != nil {
		tb.Fatalf("resolve this executable: %v", err)
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0
	}
	me := os.Getpid()
	var n int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 || pid == me {
			continue
		}
		raw, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue
		}
		cut := strings.LastIndexByte(string(raw), ')')
		if cut < 0 {
			continue
		}
		fields := strings.Fields(string(raw)[cut+1:])
		if len(fields) < 2 {
			continue
		}
		if parent, err := strconv.Atoi(fields[1]); err != nil || parent != me {
			continue
		}
		if target, err := os.Readlink("/proc/" + e.Name() + "/exe"); err == nil && target == self {
			n++
		}
	}
	return n
}

func treePeak(tb testing.TB, op func()) uint64 {
	tb.Helper()
	sampler := diagnostics.NewHostSampler(diagnostics.HostSamplerOptions{})
	var peak uint64
	sample := func() {
		report, err := sampler.Sample(context.Background())
		if err != nil {
			return
		}
		var sum uint64
		if report.BaseWorkerRSSBytes != nil {
			sum += *report.BaseWorkerRSSBytes
		}
		if report.NativeWorkerBytes != nil {
			sum += *report.NativeWorkerBytes
		}
		if sum > peak {
			peak = sum
		}
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		op()
	}()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-done:
			sample()
			return peak
		case <-tick.C:
			sample()
		}
	}
}

// reportPeak formats a peak against its ceiling and fails the row when it is
// over. A host with no process accounting reports zero, which is not a
// measurement of an empty process tree, so it fails the row rather than
// passing it: Section 23 requires an unavailable figure be reported as
// unavailable, and a gate that passes on a blind host is not a gate.
func reportPeak(t *testing.T, what string, peak uint64, ceiling uint64) string {
	t.Helper()
	report := fmt.Sprintf("%s peak %.1f MiB (target %d MiB)", what, float64(peak)/(1<<20), ceiling>>20)
	switch {
	case peak == 0:
		t.Errorf("NOT MEASURED: this host reported no resident set for the product process tree; %s", report)
	case peak > ceiling:
		t.Errorf("BUDGET MISSED: %s", report)
	}
	return report
}

// runCLI runs one release-build command against home and fails on a non-zero
// exit. Output is discarded: these rows measure time and memory, and a
// command's answer is what the latency rows and internal/e2e assert on.
func runCLI(tb testing.TB, f *budgetFixture, home string, args ...string) {
	tb.Helper()
	cmd := exec.Command(f.binary, append(args, "--repo", f.repo, "--json")...)
	cmd.Env = execEnv(home)
	if out, err := cmd.CombinedOutput(); err != nil {
		tb.Fatalf("codectx %s: %v: %s", strings.Join(args, " "), err, out)
	}
}

// lexicalShape is the identity of the stored lexical bodies: how many search
// units exist, the highest rowid ever allocated, and the token and byte totals.
//
// It is deliberately not a row count. internal/index's incremental scenario
// already asserts the FTS document count, and a delete-and-reinsert rewrite
// preserves that count exactly while allocating fresh rowids -- which is the
// rewrite the no-change row has to be able to see.
func lexicalShape(tb testing.TB, dataDir string) string {
	tb.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dataDir, "codectx.db")+"?mode=ro")
	if err != nil {
		tb.Fatalf("open the store read-only: %v", err)
	}
	defer db.Close()
	var units, maxRowID, tokens, bytes int64
	// The documents keep no stored body (ADR-0003 §2.1), so the source extent
	// they cover stands in for its bytes: a delete-and-reinsert rewrite moves
	// the rowids, which is what this shape exists to see, and a reparse that
	// re-cut the documents would move the extent.
	row := db.QueryRow(`SELECT count(*), coalesce(max(rowid), 0), coalesce(sum(token_count), 0),
		coalesce(sum(end_byte - start_byte), 0) FROM search_units`)
	if err := row.Scan(&units, &maxRowID, &tokens, &bytes); err != nil {
		tb.Fatalf("read the lexical tables: %v", err)
	}
	return fmt.Sprintf("units=%d max_rowid=%d tokens=%d body_bytes=%d", units, maxRowID, tokens, bytes)
}

// touchFiles rewrites n generated Go files with a changed body, so a refresh
// has real parser work to do.
func touchFiles(tb testing.TB, repo string, n, round int) {
	tb.Helper()
	for i := range n {
		path := filepath.Join(repo, "src", "go", fmt.Sprintf("%sGo%02d", corpusPrefix, i), "store.go")
		body, err := os.ReadFile(path)
		if err != nil {
			tb.Fatalf("read %s: %v", filepath.Base(path), err)
		}
		edited := fmt.Sprintf("%s\n// edit round %d\nfunc %sEdit%02d_%d() int { return %d }\n",
			body, round, corpusPrefix+"Go", i, round, round)
		if err := os.WriteFile(path, []byte(edited), 0o600); err != nil {
			tb.Fatalf("write %s: %v", filepath.Base(path), err)
		}
	}
}

// --- the release gate -------------------------------------------------------

// raceSkipReason is what a skipped budget row says out loud. Section 23.5
// forbids meeting a target by excluding the failing feature, so a row that is
// not measured must say it was not measured and why.
const raceSkipReason = "budgets are measured on release builds: under the race detector the " +
	"instrumented build, and the parser workers this test binary spawns as itself, are not the " +
	"product Section 23.2 states its latency and memory ceilings over"

// budgetRow is one Section 23.2 ceiling: what is measured, the frozen target,
// the corpus scale it is measured at, and the measurement itself. skip is set
// on the one row this lane does not own.
type budgetRow struct {
	name   string
	target string
	spec   corpusSpec
	skip   string
	// raceSkip marks a row whose whole assertion is a Section 23.2 latency or
	// memory ceiling. Section 23.5 takes those measurements on release builds,
	// so under the race detector the row skips with raceSkipReason rather than
	// reporting the detector's overhead as the product's. Rows that also
	// assert on WORK -- no-change-refresh's no-parse/no-FTS-rewrite pair, the
	// visited-node count, the storage decomposition -- are not marked: they
	// run under -race, and measure() reports their clock without gating it.
	raceSkip bool
	// measure takes the measurement, fails the row when it is over the
	// ceiling, and returns the figure to log either way.
	measure func(t *testing.T, f *budgetFixture) string
}

// planBudget is the manifest budget the context-plan rows compile against. The
// configured default (context.default_max_bytes) is smaller than the scope this
// corpus's high-fanout seed requires, and a plan that is refused for
// CTX_MINIMUM_BUDGET measures nothing, so the budget is stated explicitly and
// the traversal it drives is the one the visited-node row bounds.
//
// The floor this corpus reports is min_files=40, min_slices=39: one seed pulls
// the whole high-fanout scope, which is what makes the row a real plan rather
// than a one-file selection.
var planBudget = model.Budget{MaxFiles: 200, MaxSlices: 64, MaxBytes: 4 << 20, MaxEstimatedTokens: 400_000}

// budgetRows is the whole Section 23.2 table. Every target is rendered from the
// frozen constant rather than retyped, so a constant and a gate can never drift
// apart: there is one number and both read it.
var budgetRows = []budgetRow{
	{name: "version-startup", raceSkip: true, target: budgetVersionP95.String(), spec: corpusSmallReal,
		measure: func(t *testing.T, f *budgetFixture) string {
			return measure(t, budgetVersionP95, 20, func() {
				cmd := exec.Command(f.binary, "version", "--json")
				cmd.Env = execEnv(f.execHome)
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("codectx version: %v: %s", err, out)
				}
			})
		}},
	{name: "exact-query", raceSkip: true, target: budgetExactQueryP95.String(), spec: corpusSmallReal,
		measure: func(t *testing.T, f *budgetFixture) string {
			ctx := context.Background()
			return measure(t, budgetExactQueryP95, 50, func() {
				if _, err := f.svc.Symbol(ctx, model.SymbolRequest{Query: f.seedName,
					Operation: model.SymbolWorkspaceSymbols, SemanticSource: model.SemanticCanonical}); err != nil {
					t.Fatalf("exact symbol query: %v", err)
				}
			})
		}},
	{name: "lexical-search", raceSkip: true, target: budgetLexicalSearchP95.String(), spec: corpusSmallReal,
		measure: func(t *testing.T, f *budgetFixture) string {
			ctx := context.Background()
			return measure(t, budgetLexicalSearchP95, 50, func() {
				if _, err := f.svc.Search(ctx, model.SearchRequest{Query: corpusPrefix}); err != nil {
					t.Fatalf("search: %v", err)
				}
			})
		}},
	{name: "one-hop", raceSkip: true, target: budgetOneHopP95.String(), spec: corpusSmallReal,
		measure: func(t *testing.T, f *budgetFixture) string {
			ctx := context.Background()
			return measure(t, budgetOneHopP95, 50, func() {
				if _, err := f.svc.Graph(ctx, model.GraphRequest{Start: f.seeds,
					Direction: model.DirectionBoth, MaxDepth: 1}); err != nil {
					t.Fatalf("one-hop traversal: %v", err)
				}
			})
		}},
	{name: "context-plan", raceSkip: true, target: budgetContextPlanP95.String(), spec: corpusSmallReal,
		measure: func(t *testing.T, f *budgetFixture) string {
			ctx := context.Background()
			round := 0
			return measure(t, budgetContextPlanP95, 10, func() {
				round++
				// A distinct actor per iteration: an idempotent replan would
				// return the first manifest and time a cache lookup.
				if _, _, err := f.svc.Plan(ctx, model.PlanRequest{
					ActorID: fmt.Sprintf("bench-plan-%02d", round),
					Context: model.ContextRequest{Task: "measure the context plan budget",
						Seeds: []string{f.seedName}, Phase: model.PhaseSweep, Budget: planBudget}}); err != nil {
					var typed *model.Error
					if errors.As(err, &typed) {
						t.Fatalf("context plan: %v %v", err, typed.Details)
					}
					t.Fatalf("context plan: %v", err)
				}
			})
		}},
	{name: "context-plan-visited", target: fmt.Sprintf("%d nodes", budgetContextPlanVisitedNodes),
		spec:    corpusSmallReal,
		measure: measureScopeWalk},
	{name: "ten-file-refresh", raceSkip: true, target: budgetTenFileRefreshP95.String(), spec: corpusSmallReal,
		measure: func(t *testing.T, f *budgetFixture) string {
			ctx := context.Background()
			round := 0
			return measure(t, budgetTenFileRefreshP95, 5, func() {
				round++
				touchFiles(t, f.repo, 10, round)
				res, err := f.svc.Refresh(ctx, model.IndexRequest{})
				if err != nil {
					t.Fatalf("ten-file refresh: %v", err)
				}
				if res.FilesParsed == 0 {
					t.Fatalf("round %d parsed nothing: the row would be timing a no-op", round)
				}
			})
		}},
	{name: "no-change-refresh", target: budgetNoChangeRefreshP95.String(), spec: corpusSmallReal,
		measure: measureNoChangeRefresh},
	{name: "cold-index-reference", target: budgetColdIndexReference.String(), spec: corpusReference,
		skip: "ruling 2: the reference-fixture cold index is the wave's ONE full-scale pass and belongs to " +
			"the VERIFY lane; a lane that ran it would contradict the never-cold-index proof tier"},
	{name: "indexing-peak", raceSkip: true, target: fmt.Sprintf("%d MiB", budgetIndexingPeakBytes>>20), spec: corpusSmallReal,
		measure: func(t *testing.T, f *budgetFixture) string {
			peak := treePeak(t, func() { runCLI(t, f, f.execHome, "index", "--full") })
			return reportPeak(t, "indexing", peak, budgetIndexingPeakBytes)
		}},
	{name: "idle-mcp-rss", raceSkip: true, target: fmt.Sprintf("%d MiB", budgetIdleMCPRSSBytes>>20), spec: corpusSmallReal,
		measure: func(t *testing.T, f *budgetFixture) string {
			// IDLE means idle. `mcp.watch` is on by default, so a server
			// started plainly spends its first seconds running the initial
			// refresh -- parser workers and all -- and a window opened at
			// process start sampled that STARTUP peak, not the resting
			// session. The row is the resting one, so the watcher is off for
			// it (`--watch=false`) and the window opens only after the session
			// has settled. The refresh peak is row 10's subject, measured
			// there under `index --full`.
			cmd := exec.Command(f.binary, "mcp", "serve", "--repo", f.repo, "--watch=false")
			cmd.Env = execEnv(f.execHome)
			// A server with no stdin producer would see EOF and exit before it
			// could be sampled, so the pipe is held open and closed to stop it.
			stdin, err := cmd.StdinPipe()
			if err != nil {
				t.Fatalf("stdin pipe: %v", err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatalf("start mcp serve: %v", err)
			}
			// Settle: opening the workspace and standing the server up is
			// startup, not idle, so it happens outside the sampled window.
			time.Sleep(idleMCPSettle)
			// The subject is the server, so the harness must be holding no
			// parser workers of its own when the window opens: the pool drains
			// when the parse stage ends, so the shared fixture's workspace has
			// none left after its index.
			if held := workersHeldByThisProcess(t); held != 0 {
				t.Fatalf("the harness holds %d parser worker(s) of its own as the window opens; they would be measured as the server's", held)
			}
			peak := subjectPeak(t, cmd.Process.Pid, func() { time.Sleep(idleMCPSample) })
			_ = stdin.Close()
			if err := cmd.Wait(); err != nil {
				t.Fatalf("mcp serve exited non-zero (%v)", err)
			}
			return reportPeak(t, "idle mcp", peak, budgetIdleMCPRSSBytes) +
				fmt.Sprintf(" (resting session: watcher off, sampled over %s after a %s settle)",
					idleMCPSample, idleMCPSettle)
		}},
	{name: "interactive-peak", raceSkip: true, target: fmt.Sprintf("%d MiB", budgetInteractivePeakBytes>>20), spec: corpusSmallReal,
		measure: func(t *testing.T, f *budgetFixture) string {
			peak := treePeak(t, func() {
				runCLI(t, f, f.execHome, "status")
				runCLI(t, f, f.execHome, "search", corpusPrefix)
				runCLI(t, f, f.execHome, "symbol", f.seedName)
				runCLI(t, f, f.execHome, "repo-map")
			})
			return reportPeak(t, "interactive", peak, budgetInteractivePeakBytes)
		}},
	{name: "storage-ratio", target: fmt.Sprintf("%.1fx eligible source bytes", budgetStorageRatio),
		spec: corpusSmallReal,
		// The absolute 3.5x ceiling is asserted at REFERENCE scale by the
		// VERIFY lane, not here, and the reason is in the numbers this row
		// prints: the store's fixed cost -- schema, page granularity, the
		// write-ahead log's high-water mark -- is a constant that a corpus of
		// ~0.5 KiB files cannot amortise, while Section 23.1's reference
		// fixture is 1M lines over 10k files (~8 KiB each). Measuring the
		// ratio at small-real scale and failing the gate on it would enforce a
		// ceiling against a workload Section 23.2 does not define it over, and
		// a permanently red gate is not a gate either. What this row owes the
		// wave is the measured decomposition, and that is what it reports.
		measure: func(t *testing.T, f *budgetFixture) string {
			var stored uint64
			for _, part := range []*uint64{f.resources.DatabaseBytes, f.resources.WALBytes, f.resources.CASBytes} {
				if part == nil {
					t.Fatal("the resource block left a storage figure absent; the ratio cannot be stated")
				}
				stored += *part
			}
			if f.sourceBytes == 0 {
				t.Fatal("the corpus reported no eligible source bytes")
			}
			return fmt.Sprintf("%.2fx at small-real scale (%d stored over %d eligible source bytes; "+
				"db=%d wal=%d cas=%d; cas/source=%.2fx). The %.1fx ceiling is VERIFY's, at reference scale.",
				float64(stored)/float64(f.sourceBytes), stored, f.sourceBytes,
				*f.resources.DatabaseBytes, *f.resources.WALBytes, *f.resources.CASBytes,
				float64(*f.resources.CASBytes)/float64(f.sourceBytes), budgetStorageRatio)
		}},
}

// measureScopeWalk is the Section 23.2 visited-node row and the measurement the
// default context graph budgets are re-pinned from (obligation 8).
//
// The plan's own scope walk is exactly this call: internal/context runs
// eng.Impact with both directions and the three configured context bounds
// (scope.go:154-163). model.PlanResult publishes no visited count, so the
// traversal is driven directly with those bounds rather than inferred from the
// manifest, from the widest seed set a request may legally carry.
//
// It sweeps the depth as well as reporting the configured depth, because VF2's
// evidence was a walk that TRUNCATED at the default depth on a 745-file
// workspace and completed only when the depth was narrowed: the number that
// decides the pin is how the visited and edge counts grow per hop, and one
// depth cannot show that.
func measureScopeWalk(t *testing.T, f *budgetFixture) string {
	bounds := config.Defaults().Context
	// The default depth is unlimited now, so the row probes a fixed ladder
	// instead of the default value: what it measures is how visited and edge
	// counts GROW per hop, and the ceiling assertion below is what pins the
	// plan size. scopeWalkProbeDepth is the depth that assertion is made at.
	const scopeWalkProbeDepth = 3
	var out []string
	var atConfigured string
	for depth := 1; depth <= scopeWalkProbeDepth+1; depth++ {
		res, err := f.svc.Impact(context.Background(), model.ImpactRequest{Start: f.seeds,
			Direction: model.DirectionBoth, MaxDepth: depth,
			MaxVisited: bounds.MaxVisitedNodes.Int(), MaxEdges: bounds.MaxGraphEdges.Int()})
		if err != nil {
			t.Fatalf("scope traversal at depth %d: %v", depth, err)
		}
		line := fmt.Sprintf("depth %d: visited=%d edges=%d entries=%d truncated=%v edges/visited=%.2f",
			depth, res.VisitedCount, res.EdgeCount, len(res.Entries), res.Meta.Truncated,
			float64(res.EdgeCount)/float64(max(res.VisitedCount, 1)))
		out = append(out, line)
		if depth == scopeWalkProbeDepth {
			atConfigured = line
			if res.VisitedCount > budgetContextPlanVisitedNodes {
				t.Errorf("BUDGET MISSED: the scope walk visited %d nodes against a %d-node ceiling",
					res.VisitedCount, budgetContextPlanVisitedNodes)
			}
		}
	}
	return fmt.Sprintf("%d seeds, ceiling %d nodes; at probe depth %s\n\t%s",
		len(f.seeds), budgetContextPlanVisitedNodes, atConfigured, strings.Join(out, "\n\t"))
}

// measureNoChangeRefresh is the Section 23.2 reuse row.
//
// Failure mode it protects: the fast path silently reparses or rewrites the
// lexical bodies, and the 250 ms ceiling passes on a fast host anyway. So the
// assertion is on the work, not the clock -- no file parsed, every unit reused,
// and the stored lexical bodies byte-for-byte the same objects, including their
// rowids. A delete-and-reinsert rewrite keeps the document COUNT (which
// internal/index already asserts) and moves the rowids, which is precisely the
// difference this row exists to see.
func measureNoChangeRefresh(t *testing.T, f *budgetFixture) string {
	ctx := context.Background()
	before := lexicalShape(t, f.dataDir)
	var reused, parsed int64
	report := measure(t, budgetNoChangeRefreshP95, 5, func() {
		res, err := f.svc.Refresh(ctx, model.IndexRequest{})
		if err != nil {
			t.Fatalf("no-change refresh: %v", err)
		}
		reused, parsed = res.UnitsReused, res.FilesParsed
		if parsed != 0 {
			t.Errorf("a refresh with nothing changed parsed %d files", parsed)
		}
		if reused == 0 {
			t.Error("a refresh with nothing changed reused no unit")
		}
	})
	after := lexicalShape(t, f.dataDir)
	if before != after {
		t.Errorf("a refresh with nothing changed rewrote the lexical bodies: %s -> %s", before, after)
	}
	return fmt.Sprintf("%s; %d units reused, %d files parsed, lexical shape unchanged (%s)",
		report, reused, parsed, after)
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
	f := fixture(t)
	for _, row := range budgetRows {
		t.Run(row.name, func(t *testing.T) {
			t.Logf("target %s at %d packages (seed %d)", row.target, row.spec.Packages, row.spec.Seed)
			if row.skip != "" {
				t.Skip(row.skip)
			}
			if raceDetector && row.raceSkip {
				t.Skip(raceSkipReason)
			}
			// Rows share one workspace and several mutate it (the refresh
			// rows), so they run in sequence rather than in parallel: a
			// measurement taken while another row was indexing would describe
			// neither.
			t.Log(row.measure(t, f))
		})
	}
}

// TestLowMemoryProfile is Section 23.3: the full base feature set runs on a
// small host with fewer workers, producing the same semantic results more
// slowly.
//
// Failure mode it protects: a reduced-worker profile quietly produces a
// different index -- fewer facts, a narrower search -- and the reduction reads
// as the cost of a small host rather than as missing data. The comparison is
// corpus_test.go's fingerprint, which is the one comparator in this tree that
// names a dropped fact family out loud.
//
// What it does NOT prove: the host is not confined to 2 GiB. The profile is
// configured -- a 2 GiB base memory budget with one worker -- not enforced by a
// cgroup, so the row states the peak it measured against that budget.
func TestLowMemoryProfile(t *testing.T) {
	if testing.Short() {
		t.Skip("low-memory profile; run without -short")
	}
	ctx := context.Background()
	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	// corpusTiny, not corpusSmallReal: the fingerprint compares whole pages and
	// fails loudly rather than comparing two prefixes, so the corpus must stay
	// inside one page (corpus_test.go's requireComplete).
	generateCorpus(t, repo, corpusTiny)

	baseline, _, closeBaseline := openWorkspace(t, ctx, repo, filepath.Join(t.TempDir(), "default"), "")
	t.Cleanup(closeBaseline)
	want := capture(t, ctx, baseline)

	const lowMemory = "[index]\nworkers = 1\n"
	var peak uint64
	var constrained *app.Services
	home := filepath.Join(t.TempDir(), "low")
	var closeConstrained func()
	peak = treePeak(t, func() { constrained, _, closeConstrained = openWorkspace(t, ctx, repo, home, lowMemory) })
	t.Cleanup(closeConstrained)
	got := capture(t, ctx, constrained)

	if diffs := compareFingerprints(want, got); len(diffs) > 0 {
		t.Errorf("the one-worker profile produced different results:\n\t%s", strings.Join(diffs, "\n\t"))
	}
	t.Logf("one worker, 2 GiB budget: index tree peak %.1f MiB; fingerprint identical across %d fact families",
		float64(peak)/(1<<20), len(want.families()))
}

// TestSessionStatusClamp is obligation 7: a session over more than 200 files
// clamps at statusLimit and REPORTS the clamp.
//
// Failure mode it protects: a large session's status reads as complete when it
// was truncated, so an actor consolidates on a coverage picture that is missing
// files nobody told it about. Store.Coverage runs its limit through pageLimit,
// which SILENTLY clamps anything above model.MaxPageItems instead of rejecting
// it, so a Status that asked for the ceiling could never see the probe record
// that proves there is another page -- every file past the first 200 would
// vanish with no cursor and no truncation notice.
//
// What the row counts, and why. The clamp governs the SESSION FILE count, not
// the required-full count: Store.Coverage pages session_files with no
// requirement predicate, while SessionStatus.RequiredFiles is
// coverageSummarySQL's required_full-only aggregate. Both are logged below;
// only the paged total is gated.
//
// How the session gets past 200. A single plan cannot do it: a plan over this
// generator was measured saturating at ~161 files (131 required at 214
// repository files, 152 at 364, 161 at 724, 158 at 1204), because the scope
// walk reaches one symbol per package and stops, and a path seed resolves a
// file but no symbol. The session itself is what grows: `context include`
// recompiles over further seeds under the session's own budget and unions the
// result into session_files (sessionFilesSQL is INSERT OR IGNORE), so disjoint
// batches of per-package symbols accumulate. That is the route this row takes.
func TestSessionStatusClamp(t *testing.T) {
	if testing.Short() {
		t.Skip("synthetic >200-file session; run without -short")
	}
	ctx := context.Background()
	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	generateCorpus(t, repo, corpusOver200Files)
	t.Logf("shape: %d packages -> %d files (seed %d)",
		corpusOver200Files.Packages, 3*corpusOver200Files.Packages+4, corpusOver200Files.Seed)

	// Its own workspace, not the shared fixture: that one is corpusSmallReal
	// (124 files) and cannot hold a 200-file session at all. Unlike the shared
	// fixture this workspace does not outlive the row, so its close IS
	// registered -- the lock and the handle are released here.
	svc, _, closeWorkspace := openWorkspace(t, ctx, repo, filepath.Join(t.TempDir(), "home"), "")
	t.Cleanup(closeWorkspace)

	// One symbol per package per language -- the widest disjoint seed set the
	// corpus offers, one per source file. The names are constructed from the
	// generator's own naming rule rather than discovered: a workspace-symbol
	// query cannot enumerate them, because the prefix tier matches the
	// PACKAGE-QUALIFIED name (search/exact.go) and one bounded page could not
	// carry 3*120 symbols in any case. The first name is resolved below so a
	// generator whose naming drifted fails here and not as a mystery shortfall.
	var seeds []string
	for p := range corpusOver200Files.Packages {
		for _, infix := range []string{"Go", "Py", "Ts"} {
			seeds = append(seeds, fmt.Sprintf("%s%sRun%02d", corpusPrefix, infix, p))
		}
	}
	probe, err := svc.Symbol(ctx, model.SymbolRequest{Query: seeds[0],
		Operation: model.SymbolWorkspaceSymbols, SemanticSource: model.SemanticCanonical})
	if err != nil {
		t.Fatalf("workspace symbols %q: %v", seeds[0], err)
	}
	if len(probe.Items) == 0 {
		t.Fatalf("the corpus produced no symbol named %q, so the seed set names nothing", seeds[0])
	}

	// The session's own budget, raised once at plan time because include.go
	// compiles every later manifest under the CURRENT manifest's budget: a
	// planBudget-sized ceiling (200 files) would refuse with CTX_MINIMUM_BUDGET
	// exactly as the session approached the page it exists to exercise.
	//
	// Re-pinned to the walk this row now measures. The walk's root width is
	// context.max_start_nodes and unlimited by default, so 24 task seeds resolve
	// to every distinct entity they name rather than the first 64, and the
	// required scope is legitimately wider than the constants above were sized
	// for: MaxSlices 128 refuses it outright with CTX_MINIMUM_BUDGET.
	//
	// The byte and token terms are the SLICE-CUT terms, not headroom:
	// packedSliceCountStream opens a new slice when the next group would take
	// the running total past MaxBytes or MaxEstimatedTokens, with no per-slice
	// divisor. planBudget's 4 MiB over a ~193 KB corpus never cuts, so the
	// packer emits every admitted entry into ONE slice and
	// model.ContextSlice.Validate refuses its 1508-record entry_ordinals against
	// the 1000-record page width. 64 KiB / 128k tokens cuts the same required
	// set into slices no page refuses; MaxSlices 512 is the ceiling that count
	// has to clear. Widening MaxFiles alone does not help -- it is not the
	// binding term at either setting.
	budget := model.Budget{MaxFiles: 500, MaxSlices: 512,
		MaxBytes: 64 << 10, MaxEstimatedTokens: 128_000}
	// Batches are disjoint and small: sessionFilesSQL is INSERT OR IGNORE, so a
	// repeated package adds nothing, and one batch's walk must stay inside the
	// budget above.
	const batch = 24
	actor := fmt.Sprintf("clamp-%d", os.Getpid())
	var plan model.PlanResult
	var status model.SessionStatus
	plan, status, err = svc.Plan(ctx, model.PlanRequest{ActorID: actor,
		Context: model.ContextRequest{Task: "exercise the >200-file session status page",
			Seeds: seeds[:batch], Phase: model.PhaseSweep, Budget: budget}})
	if err != nil {
		t.Fatalf("context plan: %v", err)
	}
	session := model.SessionRequest{SessionID: plan.SessionID, ActorID: actor}
	total := countCoverage(t, ctx, svc, session)
	t.Logf("plan over %d seeds: %d session files, %d required", batch, total, status.RequiredFiles)

	// Grown until BOTH counts clear the page: the gate is on the session file
	// count, which is what Store.Coverage pages and statusLimit governs, but
	// carrying the required-full count past 200 as well means the row exercises
	// the clamp over a session that is oversized by either reading of it.
	grown := func() bool { return total > model.MaxPageItems && int(status.RequiredFiles) > model.MaxPageItems }
	for from := batch; from < len(seeds) && !grown(); from += batch {
		to := min(from+batch, len(seeds))
		status, err = svc.Include(ctx, model.IncludeRequest{SessionID: plan.SessionID, ActorID: actor,
			Seeds: seeds[from:to], ExpectedVersion: status.StateVersion})
		if err != nil {
			t.Fatalf("context include [%d:%d]: %v", from, to, err)
		}
		total = countCoverage(t, ctx, svc, session)
		t.Logf("after include [%d:%d]: %d session files, %d required, scope version %d",
			from, to, total, status.RequiredFiles, status.ScopeVersion)
	}
	if !grown() {
		t.Fatalf("the session grew to %d files (%d required) over %d seeds, which never clears the %d-record page",
			total, status.RequiredFiles, len(seeds), model.MaxPageItems)
	}

	// The two halves of the invariant, on the first page of a session that is
	// larger than the page: the clamp is APPLIED (the page stays strictly under
	// the ceiling, which is what leaves room for the probe record) and it is
	// REPORTED (a cursor the caller can actually continue from).
	page, _, err := svc.SessionStatus(ctx, session, model.PageRequest{})
	if err != nil {
		t.Fatalf("context status: %v", err)
	}
	if len(page.Items) >= model.MaxPageItems {
		t.Errorf("the first page of a %d-file session carries %d records; the clamp must keep it under %d "+
			"so the probe record can prove there is another page", total, len(page.Items), model.MaxPageItems)
	}
	if page.Meta.NextCursor == "" {
		t.Fatalf("the first page of a %d-file session carries %d records and no cursor (truncated=%v, reason=%q): "+
			"the remaining files are unreachable and nothing said so",
			total, len(page.Items), page.Meta.Truncated, page.Meta.TruncationReason)
	}
	t.Logf("clamp: %d-file session, first page %d records, cursor issued; required %d",
		total, len(page.Items), status.RequiredFiles)
}

// countCoverage walks every page of a session's coverage and returns the record
// count. The walk is bounded: a cursor that never terminates is itself the
// silent-loss failure this row exists to catch, so it is reported rather than
// looped on.
func countCoverage(t *testing.T, ctx context.Context, svc *app.Services, req model.SessionRequest) int {
	t.Helper()
	const maxPages = 16
	total, cursor := 0, ""
	for page := 0; ; page++ {
		if page == maxPages {
			t.Fatalf("session coverage did not end in %d pages (%d records so far)", maxPages, total)
		}
		got, _, err := svc.SessionStatus(ctx, req, model.PageRequest{Cursor: cursor})
		if err != nil {
			t.Fatalf("context status page %d: %v", page, err)
		}
		total += len(got.Items)
		if got.Meta.Truncated {
			t.Fatalf("session coverage page %d is truncated with no way to continue: %s",
				page, got.Meta.TruncationReason)
		}
		if got.Meta.NextCursor == "" {
			// A last page that fills the ceiling exactly is the silent loss
			// this row exists to catch: the store clamps a limit above
			// model.MaxPageItems instead of rejecting it, so a full page with
			// no cursor cannot be distinguished from a complete one and any
			// further file is unreachable with nothing said about it.
			if len(got.Items) >= model.MaxPageItems {
				t.Fatalf("coverage page %d carries the full %d-record ceiling and no cursor, so it cannot be "+
					"continued; %d records read", page, model.MaxPageItems, total)
			}
			return total
		}
		cursor = got.Meta.NextCursor
	}
}

// --- the published benchmarks ----------------------------------------------
//
// Step 4 runs `go test ./internal/bench -run '^$' -bench . -benchmem -count=5`,
// which selects by FUNCTION name, not by file. Every benchmark below drives the
// same shared fixture, so the set costs one index however many times -count
// re-enters them.

// BenchmarkVersionStartup measures process startup: `codectx version --json`
// opens no database, loads no grammar and detects no analyzer (Section 7.2).
func BenchmarkVersionStartup(b *testing.B) {
	if testing.Short() {
		b.Skip("startup benchmark; run without -short")
	}
	f := fixture(b)
	env := execEnv(f.execHome)
	for b.Loop() {
		cmd := exec.Command(f.binary, "version", "--json")
		cmd.Env = env
		if out, err := cmd.CombinedOutput(); err != nil {
			b.Fatalf("codectx version: %v: %s", err, out)
		}
	}
}

// BenchmarkExactQuery measures exact symbol and path resolution.
func BenchmarkExactQuery(b *testing.B) {
	f := benchFixture(b)
	ctx := context.Background()
	req := model.SymbolRequest{Query: f.seedName, Operation: model.SymbolWorkspaceSymbols,
		SemanticSource: model.SemanticCanonical}
	for b.Loop() {
		if _, err := f.svc.Symbol(ctx, req); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkLexicalSearch measures generation-local lexical retrieval.
func BenchmarkLexicalSearch(b *testing.B) {
	f := benchFixture(b)
	ctx := context.Background()
	req := model.SearchRequest{Query: corpusPrefix}
	for b.Loop() {
		if _, err := f.svc.Search(ctx, req); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkOneHopTraversal measures a one-hop caller/callee neighborhood.
func BenchmarkOneHopTraversal(b *testing.B) {
	f := benchFixture(b)
	ctx := context.Background()
	req := model.GraphRequest{Start: f.seeds, Direction: model.DirectionBoth, MaxDepth: 1}
	for b.Loop() {
		if _, err := f.svc.Graph(ctx, req); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkContextPlan measures manifest compilation over a bounded traversal.
func BenchmarkContextPlan(b *testing.B) {
	f := benchFixture(b)
	ctx := context.Background()
	round := 0
	for b.Loop() {
		round++
		if _, _, err := f.svc.Plan(ctx, model.PlanRequest{
			ActorID: fmt.Sprintf("bench-%d-%d", os.Getpid(), round),
			Context: model.ContextRequest{Task: "benchmark the context plan",
				Seeds: []string{f.seedName}, Phase: model.PhaseSweep, Budget: planBudget}}); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkTenFileRefresh measures an incremental refresh of ten changed files.
func BenchmarkTenFileRefresh(b *testing.B) {
	f := benchFixture(b)
	ctx := context.Background()
	round := 0
	for b.Loop() {
		round++
		b.StopTimer()
		touchFiles(b, f.repo, 10, round)
		b.StartTimer()
		if _, err := f.svc.Refresh(ctx, model.IndexRequest{}); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkNoChangeRefresh measures the reuse path: a refresh with nothing
// changed, which must do no parser work and rewrite no FTS body. That it does
// neither is asserted by TestResourceBudgets/no-change-refresh; this measures
// what the reuse path costs.
func BenchmarkNoChangeRefresh(b *testing.B) {
	f := benchFixture(b)
	ctx := context.Background()
	for b.Loop() {
		if _, err := f.svc.Refresh(ctx, model.IndexRequest{}); err != nil {
			b.Fatal(err)
		}
	}
}

// benchFixture is the shared fixture plus the one rule every in-process
// benchmark shares: `-short` skips, because doc.go forbids production code in
// this package and a short run must not index a corpus.
func benchFixture(b *testing.B) *budgetFixture {
	b.Helper()
	if testing.Short() {
		b.Skip("benchmark over a generated corpus; run without -short")
	}
	return fixture(b)
}
