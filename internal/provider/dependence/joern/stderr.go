package joern

// Diagnostic classification. Every pattern below was taken from Joern 4.0.627
// itself — from runs against real fixtures, or from the release's own payload
// — never from prose:
//
//   - The release logs everything to standard error through
//     `conf/log4j2.xml`, whose only appender targets SYSTEM_ERR at root level
//     WARN. A successful run's standard error is empty; the banner and the
//     "Successfully wrote graph" line go to standard output.
//   - Heap exhaustion: an induced 32 MB cap on a 1.36 MB Go module produced
//     exit 1, no graph, and `Output: java.lang.OutOfMemoryError: Java heap
//     space` with `Caused by: ... OutOfMemoryError` in the trace. The same
//     stderr also carries `Process exited with code 1.`, which is why the
//     out-of-memory test is applied first: the helper-crash signal is present
//     in an out-of-memory failure too and would otherwise mask it.
//   - Definition-cap skips: an induced `--max-num-def 1` on a Python fixture
//     produced paired WARN lines, `<method> has more than <n> definitions`
//     and `Skipping.`, 13 of each, from `ReachingDefPass`.
//   - Pass crash: the pass runner logs `Pass %s failed in %.0f ms` with the
//     throwable attached, at WARN, and a pass that throws before it is timed
//     is logged as `Pass %s failed` at ERROR instead. The format string and
//     the log level were read out of the pinned payload's bytecode; the two
//     reproductions are recorded in docs/research/10-round3-empirical.md
//     Section 6, and the untimed form is recorded verbatim in
//     testdata/linker-pass-crash.stderr.
//   - Zero-exit helper crash: the orchestrator prints `Process exited with
//     code <n>.` and still exits 0, having written a near-empty graph.

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"strings"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/process"
	"github.com/Sawmonabo/codectx/internal/provider/dependence"
)

// Diagnostic markers, verbatim.
const (
	markerOutOfMemory  = "OutOfMemoryError"
	markerPassFailed   = "Pass"
	markerFailed       = " failed"
	markerFailedIn     = markerFailed + " in "
	markerHelperExited = "Process exited with code"
	markerSkipping     = "Skipping."
	markerOverDefs     = " has more than "
)

// maxStderrLines bounds how much of a bounded stderr buffer the classifier
// walks. A run that emits more warnings than this is already reporting the
// same thing over and over; the counts stay exact for what was read.
const maxStderrLines = 200000

// maxScanTokenBytes bounds one stderr line before the scanner allocates. A
// stack trace line is short; a child that emits a megabyte without a newline
// is not going to be parsed into a diagnostic.
const maxScanTokenBytes = 1 << 20

// classify turns one child's outcome into the neutral vocabulary the provider
// acts on. A timeout wins over everything, because a terminated tree's stderr
// says whatever it happened to have flushed.
//
// private are the directories this run made for the child. They are reduced to
// their names in the tail the outcome carries: a diagnostic an operator reads
// must not publish where a private materialization or a work directory lives.
func classify(res process.Result, private []string) dependence.Outcome {
	out := dependence.Outcome{ExitCode: res.ExitCode, Duration: res.Duration, StderrBytes: res.StderrBytes,
		PeakBytes: res.PeakTreeBytes, PeakUnsampled: res.TreeUnsampled,
		StderrTail: stderrTail(res.Stderr, private)}
	if res.TimedOut {
		out.Class = dependence.FailureTimeout
		return out
	}
	var oom, helperCrash bool
	scanner := bufio.NewScanner(bytes.NewReader(res.Stderr))
	scanner.Buffer(make([]byte, 0, 64<<10), maxScanTokenBytes)
	for lines := 0; lines < maxStderrLines && scanner.Scan(); lines++ {
		line := scanner.Text()
		switch {
		case strings.Contains(line, markerOutOfMemory):
			oom = true
		case strings.Contains(line, markerHelperExited):
			helperCrash = true
		case strings.Contains(line, markerSkipping):
			out.SkippedCount++
		}
		if name, ok := skippedMethod(line); ok {
			if len(out.SkippedMethods) < dependence.MaxReportedSkips {
				out.SkippedMethods = append(out.SkippedMethods, name)
			}
		}
		if out.Pass == "" {
			if pass, ok := failedPass(line); ok {
				out.Pass = truncate(pass)
			}
		}
		if out.Exception == "" {
			if class, ok := throwable(line); ok {
				out.Exception = truncate(class)
			}
		}
	}
	switch {
	case oom && res.ExitCode != 0:
		// Heap exhaustion is the one class that may be retried, so it is
		// decided before any other signal the same stderr carries — but only
		// for a run that actually failed. The marker is a substring of the
		// bounded stderr, so a run that exited 0 and left a good graph can
		// carry it from an out-of-memory the engine caught and logged, or from
		// a source path or method name that contains the word. Classifying
		// that as memory would spend the unit's single retry on a full parse
		// of a unit that already succeeded. A zero-exit run falls through to
		// the engine/none decision below, which parse()'s graph-presence check
		// then resolves.
		out.Class = dependence.FailureMemory
	case out.Pass != "", helperCrash, res.ExitCode != 0:
		out.Class = dependence.FailureEngine
	}
	return out
}

// stderrTail is the last of a child's standard error: at most one error
// detail's worth, cut forward to a line boundary so a stack frame is never
// served half-written, and with every absolute path in it reduced to a name.
//
// The reduction is deliberately blunt. What a reader needs from an engine
// stack trace is the exception, the pass and the file it choked on, and every
// one of those survives a base name; what nobody outside this process may be
// told is where the run materialized the repository. A field that begins with
// a path separator is a path whatever produced it, so the rule needs no
// knowledge of which tool wrote the line -- and the private directories are
// replaced first, by name, so a path under one of them cannot survive as an
// unrelated-looking suffix.
func stderrTail(b []byte, private []string) string {
	if len(b) == 0 {
		return ""
	}
	if len(b) > model.MaxDetailBytes {
		b = b[len(b)-model.MaxDetailBytes:]
		if i := bytes.IndexByte(b, '\n'); i >= 0 {
			b = b[i+1:]
		}
	}
	s := string(b)
	for _, dir := range private {
		if dir != "" {
			s = strings.ReplaceAll(s, dir, privatePathName)
		}
	}
	fields := strings.Fields(s)
	for i, f := range fields {
		if strings.HasPrefix(f, string(os.PathSeparator)) {
			fields[i] = filepath.Base(f)
		}
	}
	tail := strings.Join(fields, " ")
	if len(tail) > model.MaxDetailBytes {
		tail = tail[len(tail)-model.MaxDetailBytes:]
	}
	return tail
}

// privatePathName is what a directory this run made is called in a
// diagnostic. It does not begin with a path separator, so what is left of a
// path under it -- the part inside the run's own directory, which is the part
// that says which step wrote the file -- survives the reduction below.
const privatePathName = "(private)"

// failedPass extracts the analysis pass name from either of the two lines the
// engine logs when a pass dies: `Pass <name> failed in <n> ms`, timed, and
// `Pass <name> failed`, untimed, which is the one a pass that threw before it
// could be timed leaves. Both are matched from the right of whatever precedes
// `failed`, because the log's own logger column is usually the same pass name
// and a left-to-right search finds that instead.
//
// Recognising only the timed form recorded an empty pass name for a real
// crash, and an unnamed pass is the difference between a crash the product
// knows reproduces and one it has to re-run to find out.
func failedPass(line string) (string, bool) {
	head := strings.TrimRight(line, " \t\r")
	switch j := strings.Index(head, markerFailedIn); {
	case j >= 0:
		head = head[:j]
	case strings.HasSuffix(head, markerFailed):
		head = head[:len(head)-len(markerFailed)]
	default:
		return "", false
	}
	fields := strings.Fields(head)
	if len(fields) < 2 || fields[len(fields)-2] != markerPassFailed {
		return "", false
	}
	return fields[len(fields)-1], true
}

// skippedMethod extracts the method the engine declined to analyse for data
// flow from its `<method> has more than <n> definitions` warning. The name is
// the last field before the marker, so the log's timestamp, level and logger
// columns fall away without the classifier having to model the layout.
func skippedMethod(line string) (string, bool) {
	i := strings.Index(line, markerOverDefs)
	if i < 0 || !strings.HasSuffix(strings.TrimSpace(line), "definitions") {
		return "", false
	}
	fields := strings.Fields(line[:i])
	if len(fields) == 0 {
		return "", false
	}
	return fields[len(fields)-1], true
}

// throwable extracts a Java throwable class name from a stack-trace line. Only
// the first one is kept: it is the outermost failure, and the `Caused by`
// chain below it is detail a log entry already carries.
func throwable(line string) (string, bool) {
	for _, field := range strings.Fields(strings.TrimPrefix(strings.TrimSpace(line), "Caused by:")) {
		name := strings.TrimSuffix(field, ":")
		if !strings.Contains(name, ".") {
			continue
		}
		if strings.HasSuffix(name, "Exception") || strings.HasSuffix(name, "Error") {
			return name, true
		}
	}
	return "", false
}

// truncate bounds a diagnostic string to what a bounded error detail holds.
func truncate(s string) string {
	if len(s) <= model.MaxIdentifierBytes {
		return s
	}
	return s[:model.MaxIdentifierBytes]
}
