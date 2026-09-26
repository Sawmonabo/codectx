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
// The inputs are the real engine's own bytes, captured from runs of the
// pinned payload; only absolute paths were rewritten so the fixtures carry no
// developer's home directory. The timed pass-crash line is built from the
// `Pass %s failed in %.0f ms` format string read out of the pass base class in
// that payload, with the throwable the release logs alongside it.
// linker-pass-crash.stderr is the head of a real crash's standard error,
// recorded from a parse of the two source files that reproduce it; its pass
// line is the untimed `Pass <name> failed` form.
//
// Mutations that fail it: in classify (stderr.go), return FailureEngine for an
// out-of-memory stderr instead of FailureMemory -> the memory cases fail with
// the class they were misread as; in failedPass, match only the timed form ->
// the untimed crash records an empty pass name and a crash recognisable as
// reproducible on sight is parsed a second time; in classify's outcome switch,
// put the `out.Pass != "", helperCrash, res.ExitCode != 0` case ahead of the
// `oom && res.ExitCode != 0` case -> a heap-exhausted run whose stderr also
// carries a failed pass is classed as an engine crash, and the unit loses the
// one retry a larger cap could have won.
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
				ReadBytes: 11, WriteBytes: 13, TreeUnsampled: true}, nil, nil)
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
// leaves behind. Failure mode: a crashed unit's standard error is counted and
// thrown away, so nothing outside a rerun can say what the child reported.
// The tail must survive, bounded to what one error detail carries, and it
// must not carry the private directories this run made for the child: a
// diagnostic an operator reads is not the place to publish where the
// repository was materialized. The budget must be spent after the reduction,
// in whole lines: a cut taken before it can fall inside a path and leave the
// path's leading directories behind as unrooted text, and a final line longer
// than the budget must keep its end, which is where the exception is.
//
// Mutation: in stderrTail, take the FIRST lines instead of the last -> the
// tail carries the child's startup banner and the exception that ended the
// run is gone; or drop the private-path replacement -> the detail publishes
// the run's materialization directory; or cut the raw buffer to the budget
// before reducing it -> the long-home case publishes the tail of the home
// directory's name; or return "" when the last line alone exceeds the budget
// -> the over-long line case loses its exception.
func TestClassifyKeepsTheChildsLastWords(t *testing.T) {
	const private = "/var/data/codectx/abc123/work/run-7"
	stderr := "the first line, far enough back to be cut\n" +
		strings.Repeat("filler that pushes the interesting lines past the bound\n", 64) +
		"java.nio.file.InvalidPathException: Malformed input or input contains unmappable characters: " +
		private + "/out/createXngagement.js.json\n" +
		"\tat java.base/sun.nio.fs.UnixPath.encode(UnixPath.java:145)\n"

	got := classify(process.Result{Stderr: []byte(stderr), ExitCode: 1, StderrBytes: int64(len(stderr))},
		[]string{private}, nil)
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

	// A home directory long enough that the budget boundary falls inside it,
	// with the exception after it on the same line.
	home := "/home/" + strings.Repeat("longusername", model.MaxDetailBytes/12)
	cut := "refused " + home + "/src/demo-repo/pkg/handler.go\n" +
		"java.lang.IllegalStateException: " + home + "/src/demo-repo/pkg/handler.go\n"
	got = classify(process.Result{Stderr: []byte(cut), ExitCode: 1, StderrBytes: int64(len(cut))}, nil, nil)
	if strings.Contains(got.StderrTail, "longusername") || strings.Contains(got.StderrTail, "demo-repo") {
		t.Errorf("a path the budget split survives in part: %q", got.StderrTail)
	}
	if !strings.Contains(got.StderrTail, "IllegalStateException: handler.go") {
		t.Errorf("the tail lost the line the path sat on: %q", got.StderrTail)
	}

	long := strings.Repeat("frame ", model.MaxDetailBytes) + "java.lang.StackOverflowError\n"
	got = classify(process.Result{Stderr: []byte(long), ExitCode: 1, StderrBytes: int64(len(long))}, nil, nil)
	if !strings.HasSuffix(got.StderrTail, "java.lang.StackOverflowError") || len(got.StderrTail) > model.MaxDetailBytes {
		t.Errorf("a final line over the budget did not keep its bounded end: %d bytes ending %q",
			len(got.StderrTail), got.StderrTail[max(0, len(got.StderrTail)-40):])
	}
	if !strings.HasPrefix(got.StderrTail, "frame ") {
		t.Errorf("the over-long line starts inside a field: %q", got.StderrTail[:min(40, len(got.StderrTail))])
	}
}

// TestClassifyReducesPunctuatedPaths protects the privacy of a durable failure
// row. Failure mode: the child prints absolute paths inside punctuation -- a
// backticked command line, an argument list, a quoted value, a redirection, an
// argument file -- and a reduction that missed any opener let the whole
// directory chain, including the operator's home directory and the repository
// path, travel through the failure detail into provider_runs.failure_json,
// which outlives every log. A known directory whose name holds a space must
// be reduced whole, not up to the space. The other half is asserted with it: a
// tail whose base names are gone diagnoses nothing, and a path under a
// directory this run made keeps the part inside that directory, which says
// which step wrote the file.
//
// The paths are synthetic. A fixture built from this machine's own home
// directory would put a host-local path in a tracked file.
//
// Mutation: in isPathByte, return true for '>' or '@' ->
// `2>/home/example-user/...` or `@/home/example-user/...` survives whole; or
// drop the known-root match in reduce -> the spaced home directory survives as
// `Doe/...`; or reduce a private path to its base name ->
// `(private)/out/export.json` loses the step that wrote it.
func TestClassifyReducesPunctuatedPaths(t *testing.T) {
	const home = "/home/example-user"
	const repo = home + "/src/demo-repo"
	const spaced = "/srv/Jane Doe"
	const work = "/var/data/run-3/work"
	stderr := "2026-09-14 08:00:00.000 ERROR Runner cmd: `" + home + "/.local/bin/analyzer-parse --language go`\n" +
		"java.lang.RuntimeException: refused List(" + repo + "/units/unit-4, --output)\n" +
		"\tat Importer.read(Importer.scala:88) file=\"" + repo + "/pkg/handler.go\"\n" +
		"child 2>" + home + "/logs/child.log @" + home + "/args/parse.args|" + home + "/bin/next\n" +
		"runtime " + spaced + "/runtime/bin/java exited\n" +
		"java.nio.file.NoSuchFileException: " + work + "/out/export.json\n"

	got := classify(process.Result{Stderr: []byte(stderr), ExitCode: 1, StderrBytes: int64(len(stderr))},
		[]string{work}, []string{spaced})
	for _, outside := range []string{"example-user", "demo-repo", "/home", ".local", "/src", "/pkg", "/units",
		"/logs", "/args", "Jane", "Doe", "/runtime", "run-3"} {
		if strings.Contains(got.StderrTail, outside) {
			t.Errorf("the tail publishes %q, a directory outside the workspace: %q", outside, got.StderrTail)
		}
	}
	for _, kept := range []string{"`analyzer-parse", "List(unit-4,", "file=\"handler.go\"", "(private)/out/export.json",
		"2>child.log", "@parse.args|next", "runtime java exited"} {
		if !strings.Contains(got.StderrTail, kept) {
			t.Errorf("the redaction destroyed the diagnostic: %q is not in %q", kept, got.StderrTail)
		}
	}
}

// TestParseArgsEndsWithTheFrontendDelimiter protects the one ordering the
// parse command line cannot survive losing. Everything after the delimiter is
// read by the frontend, not by the parse tool, so a source directory or an
// output path placed after it would leave the parse tool with no input at all
// -- a whole language family that analyses nothing, with no compile error and
// no diagnostic from the engine to say so.
//
// The second half protects the families the delimiter must not reach: a
// frontend that rejects the option writes a warning into the stderr the
// classifier reads, and the option would enter the cache key of a family whose
// graph it cannot change.
//
// Mutation: in parseArgs, append frontendArgs before the source directory ->
// the first half fails; in frontendArgs, return the Java suffix for every
// family -> the second half fails.
func TestParseArgsEndsWithTheFrontendDelimiter(t *testing.T) {
	java := frontendArgs(dependence.FamilyJava)
	args := parseArgs([]string{"--base"}, dependence.ParseRequest{
		Family: dependence.FamilyJava, SourceDir: "/src", OutputPath: "/out/graph"})
	tail := args[len(args)-len(java):]
	if len(java) == 0 || strings.Join(tail, " ") != strings.Join(java, " ") {
		t.Fatalf("the parse argv ends with %q, not with the frontend delimiter %q", tail, java)
	}
	head := args[:len(args)-len(java)]
	if got := strings.Join(head, " "); !strings.HasSuffix(got, "/src --output /out/graph") {
		t.Errorf("the paths do not precede the frontend delimiter: %q", got)
	}
	for _, f := range dependence.Families {
		if f == dependence.FamilyJava {
			continue
		}
		args := parseArgs([]string{"--base"}, dependence.ParseRequest{Family: f, SourceDir: "/src", OutputPath: "/out/graph"})
		if got := strings.Join(args, " "); !strings.HasSuffix(got, "/src --output /out/graph") {
			t.Errorf("family %s is handed frontend arguments it does not honour: %q", f, got)
		}
	}
}
