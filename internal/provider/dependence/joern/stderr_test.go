package joern

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/process"
	"github.com/Sawmonabo/codectx/internal/provider/dependence"
)

// TestClassify protects the one thing this backend exists to get right: which
// failure class a run belongs to. Silent breakage here is a correctness
// failure, not a cosmetic one — an out-of-memory run misread as a crash loses
// the single retry the plan allows, a crash misread as out-of-memory burns a
// full parse on a retry that cannot succeed, a definition-cap skip misread as
// a clean run seals a unit as complete when its data dependence is missing
// whole method bodies, and a zero-exit dead graph misread as success admits an
// empty analysis as a fresh one.
//
// The inputs are the real engine's own bytes, captured from Joern 4.0.627 runs
// recorded in the lane report; only absolute paths were rewritten so the
// fixtures carry no developer's home directory. The timed pass-crash line is
// built from the `Pass %s failed in %.0f ms` format string read out of
// io.shiftleft.passes.CpgPassBase in the pinned payload, with the throwable
// the release logs alongside it. linker-pass-crash.stderr is the head of a
// real crash's standard error, recorded from a parse of the two source files
// that reproduce it; its pass line is the untimed `Pass <name> failed` form,
// which an earlier parser did not match — so a crash the product could have
// recognised as reproducible on sight recorded an empty pass name and was
// parsed a second time for nothing.
func TestClassify(t *testing.T) {
	const passCrash = "2026-09-13 23:10:01.001 WARN  CfgCreationPass           Pass CfgCreationPass failed in 3410 ms\n" +
		"java.util.NoSuchElementException: next on empty iterator\n" +
		"\tat scala.collection.Iterator$$anon$19.next(Iterator.scala:973)\n"

	cases := []struct {
		name      string
		stderr    string
		exit      int
		timedOut  bool
		want      dependence.FailureClass
		pass      string
		exception string
		skips     int
		firstSkip string
	}{
		{name: "a clean run is not a failure", want: dependence.FailureNone},
		{name: "frontend warnings that mean nothing are not a failure",
			stderr: fixture(t, "benign-warnings.stderr"), want: dependence.FailureNone},
		{name: "heap exhaustion is memory even though the same stderr also reports a helper exit",
			stderr: fixture(t, "out-of-memory.stderr"), exit: 1, want: dependence.FailureMemory,
			exception: "java.lang.OutOfMemoryError"},
		{name: "a definition-cap skip degrades the unit without failing it",
			stderr: fixture(t, "definition-cap-skip.stderr"), want: dependence.FailureNone,
			skips: 3, firstSkip: "<operator>.tupleLiteral"},
		{name: "a pass crash names the pass and the exception",
			stderr: passCrash, exit: 1, want: dependence.FailureEngine,
			pass: "CfgCreationPass", exception: "java.util.NoSuchElementException"},
		{name: "an untimed pass crash names the pass too",
			stderr: fixture(t, "linker-pass-crash.stderr"), exit: 1, want: dependence.FailureEngine,
			pass:      "io.joern.x2cpg.frontendspecific.jssrc2cpg.ObjectPropertyCallLinker",
			exception: "java.lang.RuntimeException"},
		{name: "a helper crash hidden behind a zero exit is still an engine failure",
			stderr: "2026-09-13 23:11:00.000 ERROR ExternalCommand$          Process exited with code 101.\n",
			want:   dependence.FailureEngine},
		{name: "a terminated run is a timeout whatever its stderr says",
			stderr: fixture(t, "out-of-memory.stderr"), exit: -1, timedOut: true, want: dependence.FailureTimeout},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// The child's measured cost travels out of the backend whatever
			// the step became: the outcome is the only channel between the
			// runner and the span that ran the child, so a class decision
			// that dropped it would leave every analyzer stage costless. The
			// tree peak is marked unsampled here to assert the other half:
			// a figure nobody took stays flagged absent rather than becoming
			// an observed zero.
			got := classify(process.Result{Stderr: []byte(c.stderr), ExitCode: c.exit, TimedOut: c.timedOut,
				StderrBytes: int64(len(c.stderr)), CPUUserMillis: 7, CPUSysMillis: 3,
				ReadBytes: 11, WriteBytes: 13, TreeUnsampled: true}, nil)
			if got.CPUUserMS != 7 || got.CPUSysMS != 3 || got.CPUUnsampled {
				t.Errorf("processor time = %d/%d ms (unsampled %t), want the child's 7/3 ms measured",
					got.CPUUserMS, got.CPUSysMS, got.CPUUnsampled)
			}
			if got.ReadBytes != 11 || got.WriteBytes != 13 || got.IOUnsampled {
				t.Errorf("transferred bytes = %d/%d (unsampled %t), want the child's 11/13 measured",
					got.ReadBytes, got.WriteBytes, got.IOUnsampled)
			}
			if !got.PeakUnsampled {
				t.Errorf("the tree peak reads measured (%d) for a result that sampled none", got.PeakBytes)
			}
			if got.Class != c.want {
				t.Errorf("class = %q, want %q", got.Class, c.want)
			}
			if got.Pass != c.pass {
				t.Errorf("pass = %q, want %q", got.Pass, c.pass)
			}
			if got.Exception != c.exception {
				t.Errorf("exception = %q, want %q", got.Exception, c.exception)
			}
			if got.SkippedCount != c.skips {
				t.Errorf("skipped = %d, want %d", got.SkippedCount, c.skips)
			}
			if c.firstSkip != "" && (len(got.SkippedMethods) == 0 || got.SkippedMethods[0] != c.firstSkip) {
				t.Errorf("skipped methods = %v, want the first to be %q", got.SkippedMethods, c.firstSkip)
			}
		})
	}
}

func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestClassifyKeepsTheChildsLastWords protects the evidence a failed unit
// leaves behind. Failure mode: a unit crashed on a real repository, its 7,584
// bytes of standard error were counted and thrown away, and the only record
// left was the count -- so nothing outside a rerun could say what the child
// had reported. The tail must survive, bounded to what one error detail
// carries, and it must not carry the private directories this run made for the
// child: a diagnostic an operator reads is not the place to publish where the
// repository was materialized.
func TestClassifyKeepsTheChildsLastWords(t *testing.T) {
	const private = "/var/data/codectx/abc123/work/run-7"
	stderr := "the first line, far enough back to be cut\n" +
		strings.Repeat("filler that pushes the interesting lines past the bound\n", 64) +
		"java.nio.file.InvalidPathException: Malformed input or input contains unmappable characters: " +
		private + "/out/createXngagement.js.json\n" +
		"\tat java.base/sun.nio.fs.UnixPath.encode(UnixPath.java:145)\n"

	got := classify(process.Result{Stderr: []byte(stderr), ExitCode: 1, StderrBytes: int64(len(stderr))},
		[]string{private})
	if got.Class != dependence.FailureEngine {
		t.Fatalf("class = %q, want an engine failure", got.Class)
	}
	if !strings.Contains(got.StderrTail, "InvalidPathException") {
		t.Errorf("the tail lost the child's last words: %q", got.StderrTail)
	}
	if !strings.Contains(got.StderrTail, "UnixPath.encode") {
		t.Errorf("the tail lost the last line of the trace: %q", got.StderrTail)
	}
	if strings.Contains(got.StderrTail, private) {
		t.Errorf("the tail publishes a private run directory: %q", got.StderrTail)
	}
	if strings.Contains(got.StderrTail, "the first line") {
		t.Errorf("the tail is not bounded to the last of the stream: %q", got.StderrTail)
	}
	if len(got.StderrTail) > model.MaxDetailBytes {
		t.Errorf("the tail is %d bytes, more than one error detail carries", len(got.StderrTail))
	}
}
