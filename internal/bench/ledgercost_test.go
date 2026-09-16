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
	"testing"
	"time"

	"github.com/Sawmonabo/codectx/internal/config"
	"github.com/Sawmonabo/codectx/internal/index"
	"github.com/Sawmonabo/codectx/internal/ledger"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/process"
	"github.com/Sawmonabo/codectx/internal/provider"
	"github.com/Sawmonabo/codectx/internal/provider/filesystem"
	"github.com/Sawmonabo/codectx/internal/provider/manifest"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/wire"
	"github.com/Sawmonabo/codectx/internal/snapshot"
	"github.com/Sawmonabo/codectx/internal/storage/sqlite"
	"github.com/Sawmonabo/codectx/internal/testenv"
	"github.com/Sawmonabo/codectx/internal/workspace"
)

// What a run costs when it is recorded.
//
// The ADR requires this figure: a run with the run ledger present must be
// within run-to-run noise of one without it. There is deliberately no setting
// that turns recording off, so the "off" arm is composed rather than
// configured -- index.Options.Ledger left nil. That is honest precisely
// because it is the same code path: *ledger.Ledger is nil-safe at every entry
// point a run reaches (NewRun, DeleteRuns, Stop) and ledger.Start on a context
// with no run returns a nil *Span whose methods do nothing, so neither arm
// takes a branch the other does not.
//
// Both arms compose the coordinator directly, the way TestIncrementalReuse
// does, because app.OpenWorkspace always opens a ledger -- which is the
// product's intent and not a gap. The only difference between the two
// compositions is the one field.
//
// Each repetition runs in a CHILD process for two reasons: the measured
// quantity is in-process memory (the bounded event bus, the live span structs,
// ledger.db's page cache and WAL), and treePeak -- the instrument the memory
// rows already use -- sums this process's DESCENDANTS, so an in-process arm
// would be invisible to it; and a fresh process gives every repetition the
// same cold heap, which one long-lived process cannot.
const (
	// ledgerArmSubcommand marks the child invocation of this test binary that
	// runs one arm. TestMain dispatches it before it touches benchRoot.
	ledgerArmSubcommand = "ledger-cost-arm"
	ledgerArmOn         = "on"
	ledgerArmOff        = "off"
	// ledgerCostReps is the number of measured repetitions per arm. One
	// further repetition per arm runs first and is discarded: it pays for the
	// page cache over a corpus nothing has read yet. It is even so that each
	// arm runs first in exactly half the repetitions.
	ledgerCostReps = 6
)

// ledgerArmResult is the one stdout line a child arm prints, parsed by the
// parent. spans is the number of rows the ledger recorded, which is what makes
// the "on" arm an arm and not a second copy of "off".
type ledgerArmResult struct {
	indexWall time.Duration
	stopWall  time.Duration
	spans     int64
}

// TestLedgerCost measures one fixture index run with the ledger recording and
// with it absent, several times each, alternating, and reports wall time and
// peak resident memory with their spread.
//
// It asserts no performance number. A test that failed on a ratio of two wall
// clocks would be flaky by construction, and the measurement -- not a gate --
// is what the ADR asks for. The only thing it fails on is an unmeasurable
// figure: a zero tree peak is "this host has no process accounting", which
// must never be recorded as a measurement of an empty process tree.
//
// On demand: go test -run TestLedgerCost ./internal/bench
func TestLedgerCost(t *testing.T) {
	if testing.Short() {
		t.Skip("run ledger cost measurement; run without -short")
	}
	testenv.SkipIfLoaded(t)
	t.Logf("host load average at start: %s", loadAverage())

	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		t.Fatal(err)
	}
	// One corpus for every repetition of both arms: the arms must differ in
	// the ledger and in nothing else, and generating per repetition would
	// measure the generator's own writes through the page cache.
	root := filepath.Join(benchRoot, "ledger-cost")
	repo := filepath.Join(root, "repo")
	if err := os.MkdirAll(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	generateCorpus(t, repo, corpusSmallReal)

	type sample struct {
		wall, outer time.Duration
		peak        uint64
	}
	samples := map[string][]sample{}
	// Repetition 0 is the discarded warm-up. The order of the two arms swaps
	// every repetition so that neither arm is systematically the one that runs
	// first, which is the position that pays for whatever the other warmed.
	for rep := 0; rep <= ledgerCostReps; rep++ {
		arms := []string{ledgerArmOff, ledgerArmOn}
		if rep%2 == 1 {
			arms[0], arms[1] = arms[1], arms[0]
		}
		for _, arm := range arms {
			// A fresh data directory per repetition: reusing one would make
			// every repetition after the first an incremental refresh that
			// reuses every unit, and both arms would then measure a no-op.
			dataDir := filepath.Join(root, fmt.Sprintf("data-%s-%d", arm, rep))
			var res ledgerArmResult
			var outer time.Duration
			var runErr error
			peak := treePeak(t, func() {
				started := time.Now()
				res, runErr = runLedgerArm(exe, repo, dataDir, arm)
				outer = time.Since(started)
			})
			if runErr != nil {
				t.Fatalf("rep %d arm %s: %v", rep, arm, runErr)
			}
			if arm == ledgerArmOn && res.spans == 0 {
				t.Fatalf("rep %d: the recording arm recorded no spans; the two arms are the same run", rep)
			}
			if arm == ledgerArmOff && res.spans != 0 {
				t.Fatalf("rep %d: the arm composed with no ledger wrote %d spans", rep, res.spans)
			}
			if peak == 0 {
				t.Errorf("NOT MEASURED: this host reported no resident set for the arm's process tree")
			}
			label := "measured"
			if rep == 0 {
				label = "warm-up (discarded)"
			}
			t.Logf("rep %d %-3s %-19s index %8s  stop %7s  peak %7.1f MiB  child %8s  spans %d",
				rep, arm, label, res.indexWall.Round(time.Millisecond), res.stopWall.Round(time.Millisecond),
				float64(peak)/(1<<20), outer.Round(time.Millisecond), res.spans)
			if rep == 0 {
				continue
			}
			samples[arm] = append(samples[arm], sample{wall: res.indexWall, outer: outer, peak: peak})
		}
	}

	walls := func(arm string) []float64 {
		var out []float64
		for _, s := range samples[arm] {
			out = append(out, s.wall.Seconds()*1000)
		}
		return out
	}
	peaks := func(arm string) []float64 {
		var out []float64
		for _, s := range samples[arm] {
			out = append(out, float64(s.peak)/(1<<20))
		}
		return out
	}
	outers := func(arm string) []float64 {
		var out []float64
		for _, s := range samples[arm] {
			out = append(out, s.outer.Seconds()*1000)
		}
		return out
	}
	for _, m := range []struct {
		what  string
		unit  string
		pick  func(string) []float64
		scale string
	}{
		{"index wall", "ms", walls, "%.0f"},
		{"whole child wall", "ms", outers, "%.0f"},
		{"tree peak", "MiB", peaks, "%.1f"},
	} {
		on, off := m.pick(ledgerArmOn), m.pick(ledgerArmOff)
		t.Logf("%s (%s, n=%d per arm): off median %s range %s..%s | on median %s range %s..%s | %s",
			m.what, m.unit, ledgerCostReps,
			fmtf(m.scale, median(off)), fmtf(m.scale, minOf(off)), fmtf(m.scale, maxOf(off)),
			fmtf(m.scale, median(on)), fmtf(m.scale, minOf(on)), fmtf(m.scale, maxOf(on)),
			verdict(off, on))
	}
	t.Logf("host load average at end: %s", loadAverage())
}

// verdict calls the difference against the spread, and states the rule rather
// than assuming it.
//
// The comparison is PAIRED: the two arms of one repetition run next to each
// other, so a repetition's difference is taken under whatever the machine was
// doing at that moment, and the drift that dominates the unpaired ranges
// cancels. Comparing a difference of medians against the wider arm's range
// instead would let one slow repetition in either arm widen the range enough
// to absorb a real and repeatable difference -- which is exactly what was
// observed here before this rule replaced it.
//
// The verdict is WITHIN NOISE when the per-repetition differences straddle
// zero: the sign of the difference is then not stable from repetition to
// repetition, which is what "indistinguishable from run-to-run noise" means.
// When every repetition falls the same way the difference is real, and the
// median difference and its range say how large it is.
func verdict(off, on []float64) string {
	if len(off) == 0 || len(on) != len(off) {
		return "NOT MEASURED"
	}
	diffs := make([]float64, len(off))
	for i := range off {
		diffs[i] = on[i] - off[i]
	}
	medOff, medDiff := median(off), median(diffs)
	share := "n/a"
	if medOff != 0 {
		share = fmt.Sprintf("%+.1f%%", 100*medDiff/medOff)
	}
	body := fmt.Sprintf("paired difference: median %+.1f (%s of the unrecorded arm), per-repetition %+.1f..%+.1f",
		medDiff, share, minOf(diffs), maxOf(diffs))
	if minOf(diffs) <= 0 && maxOf(diffs) >= 0 {
		return body + ": WITHIN NOISE (the sign is not stable across repetitions)"
	}
	return body + ": ABOVE NOISE (every repetition fell the same way)"
}

func fmtf(format string, v float64) string { return fmt.Sprintf(format, v) }

// median is the nearest-rank middle: for an even sample count it is the upper
// of the two middle values. Either way it is a real observation and never an
// average of two, which would report a figure no repetition produced.
func median(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	return s[len(s)/2]
}

func minOf(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	m := v[0]
	for _, x := range v {
		if x < m {
			m = x
		}
	}
	return m
}

func maxOf(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	m := v[0]
	for _, x := range v {
		if x > m {
			m = x
		}
	}
	return m
}

// loadAverage is the host's one-minute figure, logged at both ends of the
// measurement so the report can state what the machine was doing rather than
// assert that it was quiet. A host without /proc has no figure, which is
// recorded as such.
func loadAverage() string {
	raw, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return "unavailable"
	}
	return strings.TrimSpace(string(raw))
}

// runLedgerArm runs one arm in a child process and parses its one result line.
// The child's environment carries only PATH: the arm composes from
// config.Defaults and reads no user configuration, so nothing on this host can
// change what is measured.
func runLedgerArm(exe, repo, dataDir, arm string) (ledgerArmResult, error) {
	cmd := exec.Command(exe, ledgerArmSubcommand, repo, dataDir, arm)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
	out, err := cmd.Output()
	if err != nil {
		var ex *exec.ExitError
		if errors.As(err, &ex) {
			return ledgerArmResult{}, fmt.Errorf("arm %s: %v: %s", arm, err, ex.Stderr)
		}
		return ledgerArmResult{}, fmt.Errorf("arm %s: %w", arm, err)
	}
	return parseLedgerArmResult(string(out))
}

const ledgerArmResultPrefix = "LEDGER-COST-ARM "

func parseLedgerArmResult(out string) (ledgerArmResult, error) {
	for _, line := range strings.Split(out, "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), ledgerArmResultPrefix)
		if !ok {
			continue
		}
		var res ledgerArmResult
		for _, field := range strings.Fields(rest) {
			key, value, ok := strings.Cut(field, "=")
			if !ok {
				return res, fmt.Errorf("arm result field %q is not key=value", field)
			}
			n, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				return res, fmt.Errorf("arm result field %q: %w", field, err)
			}
			switch key {
			case "index_ns":
				res.indexWall = time.Duration(n)
			case "stop_ns":
				res.stopWall = time.Duration(n)
			case "spans":
				res.spans = n
			default:
				return res, fmt.Errorf("arm result has an unknown field %q", key)
			}
		}
		return res, nil
	}
	return ledgerArmResult{}, fmt.Errorf("the arm printed no result line:\n%s", out)
}

// ledgerArmMain is the child. It composes the coordinator over the corpus the
// parent generated, runs one cold index, and prints what it cost. It returns an
// exit code rather than using testing.TB: it is not running a test.
func ledgerArmMain(args []string) int {
	if len(args) != 3 {
		fmt.Fprintf(os.Stderr, "usage: %s <repo> <data-dir> <on|off>\n", ledgerArmSubcommand)
		return 2
	}
	res, err := indexOnce(args[0], args[1], args[2] == ledgerArmOn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ledger cost arm: %v\n", err)
		return 1
	}
	fmt.Printf("%sindex_ns=%d stop_ns=%d spans=%d\n",
		ledgerArmResultPrefix, res.indexWall.Nanoseconds(), res.stopWall.Nanoseconds(), res.spans)
	return 0
}

// indexOnce is the arm itself. Every line of it is shared by the two arms
// except the one that opens the ledger.
func indexOnce(repo, dataDir string, record bool) (ledgerArmResult, error) {
	ctx := context.Background()
	cfg := config.Defaults()
	cfg.Storage.DataDir = dataDir
	// The three optional providers are off for the same reason the budget rows
	// turn them off: an "auto" provider that is unavailable on this host would
	// make the figure a statement about the host.
	cfg.Providers.SCIP.Enabled = config.Disabled
	cfg.Providers.LSP.Enabled = config.Disabled
	cfg.Providers.Dependence.Enabled = config.Disabled

	cas, err := snapshot.OpenCAS(snapshot.CASDir(dataDir))
	if err != nil {
		return ledgerArmResult{}, err
	}
	store, err := sqlite.Open(ctx, filepath.Join(dataDir, "codectx.db"), sqlite.Options{})
	if err != nil {
		return ledgerArmResult{}, err
	}
	defer store.Close()
	lock, err := snapshot.LockWorkspace(ctx, dataDir, 0)
	if err != nil {
		return ledgerArmResult{}, err
	}
	defer lock.Close()
	root, err := workspace.Discover(repo)
	if err != nil {
		return ledgerArmResult{}, err
	}
	defer root.Close()

	exe, err := os.Executable()
	if err != nil {
		return ledgerArmResult{}, err
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return ledgerArmResult{}, err
	}
	runner, err := process.NewRunner(process.Limits{MaxConcurrent: 4, MemoryBudgetBytes: 4 << 30, DiskBudgetBytes: 1 << 30})
	if err != nil {
		return ledgerArmResult{}, err
	}
	parsers := filepath.Join(dataDir, "parsers")
	if err := os.MkdirAll(parsers, 0o700); err != nil {
		return ledgerArmResult{}, err
	}
	ts, err := treesitter.New(treesitter.Options{MaxWorkers: 2, MaxParseFileBytes: cfg.Workspace.MaxParseFileBytes,
		ParseTimeout: time.Minute, WorkerMemoryBytes: 256 << 20,
		Worker: treesitter.WorkerCommand{Path: exe, Args: []string{wire.Subcommand}}, Runner: runner, WorkDir: parsers})
	if err != nil {
		return ledgerArmResult{}, err
	}
	defer ts.Close()
	fs, err := filesystem.New(filesystem.Options{MaxSearchFileBytes: cfg.Workspace.MaxSearchFileBytes})
	if err != nil {
		return ledgerArmResult{}, err
	}
	mf, err := manifest.New(manifest.Options{MaxParseFileBytes: cfg.Workspace.MaxParseFileBytes})
	if err != nil {
		return ledgerArmResult{}, err
	}
	registry, err := provider.NewRegistry(fs, mf, ts)
	if err != nil {
		return ledgerArmResult{}, err
	}
	pool, err := provider.NewPool(cfg.Index.QueueBytes)
	if err != nil {
		return ledgerArmResult{}, err
	}

	// The one difference between the arms. No subscriber is registered: the
	// shipped composition adds a structured-log subscriber, whose cost is the
	// logger's and not the ledger's, and measuring it here would attribute one
	// to the other.
	var led *ledger.Ledger
	if record {
		if led, err = ledger.Open(ctx, dataDir); err != nil {
			return ledgerArmResult{}, err
		}
	}
	c, err := index.New(index.Options{Root: root, Config: cfg, Store: store, Registry: registry,
		CAS: cas, Lock: lock, Pool: pool, Ledger: led})
	if err != nil {
		return ledgerArmResult{}, err
	}
	defer c.Close()

	started := time.Now()
	if _, err := c.Index(ctx, model.IndexRequest{}); err != nil {
		return ledgerArmResult{}, fmt.Errorf("index: %w", err)
	}
	res := ledgerArmResult{indexWall: time.Since(started)}
	// Stop is timed apart from the run: it is what flushes the last rows, so
	// folding it into the run's wall would charge the run for work that
	// happens after it, and dropping it would hide a cost that is real.
	stopped := time.Now()
	if err := led.Stop(); err != nil {
		return res, fmt.Errorf("stop the ledger: %w", err)
	}
	res.stopWall = time.Since(stopped)
	if res.spans, err = recordedSpans(dataDir, record); err != nil {
		return res, err
	}
	return res, nil
}

// recordedSpans counts what the ledger wrote, which is what proves the two arms
// are not the same run twice. The arm that composed no ledger must have written
// no file at all.
func recordedSpans(dataDir string, record bool) (int64, error) {
	path := ledger.Path(dataDir)
	if !record {
		switch _, err := os.Stat(path); {
		case err == nil:
			return 0, fmt.Errorf("the arm composed with no ledger created %s", path)
		case os.IsNotExist(err):
			return 0, nil
		default:
			return 0, err
		}
	}
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		return 0, err
	}
	defer db.Close()
	var spans int64
	if err := db.QueryRow(`SELECT count(*) FROM spans`).Scan(&spans); err != nil {
		return 0, fmt.Errorf("count the recorded spans: %w", err)
	}
	return spans, nil
}
