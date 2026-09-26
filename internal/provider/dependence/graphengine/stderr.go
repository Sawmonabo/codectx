package graphengine

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
//     reproductions are recorded in docs/research/10-engine-empirical.md
//     Section 6, and the untimed form is recorded verbatim in
//     testdata/linker-pass-crash.stderr.
//   - Zero-exit helper crash: the orchestrator prints `Process exited with
//     code <n>.` and still exits 0, having written a near-empty graph.

import (
	"bufio"
	"bytes"
	"cmp"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf8"

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
// private are the directories this run made for the child and roots the other
// directories its paths are known to live under. The tail the outcome carries
// is reduced over both (stderrTail): a diagnostic an operator reads must not
// publish where a private materialization, a work directory or the
// repository lives.
func classify(res process.Result, private, roots []string) dependence.Outcome {
	out := dependence.Outcome{ExitCode: res.ExitCode, Duration: res.Duration, StderrBytes: res.StderrBytes,
		PeakBytes: res.PeakTreeBytes, PeakUnsampled: res.TreeUnsampled,
		CPUUserMS: res.CPUUserMillis, CPUSysMS: res.CPUSysMillis, CPUUnsampled: res.CPUUnsampled,
		ReadBytes: res.ReadBytes, WriteBytes: res.WriteBytes, IOUnsampled: res.IOUnsampled,
		StderrTail: stderrTail(res.Stderr, private, roots)}
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
// detail's worth, in whole lines, and with every absolute path in it reduced.
//
// Lines are taken from the end and each is reduced before it is counted, so a
// cut can never fall inside a path: a path the budget splits would leave its
// leading directories -- the operator's home directory among them -- as an
// unrooted remainder no rule recognises. A final line that is longer than the
// whole budget on its own keeps its last bytes, from the first field boundary
// in them, so the exception at the end of an over-long line is still what the
// reader sees.
//
// The reduction is deliberately blunt. What a reader needs from an engine
// stack trace is the exception, the pass and the file it choked on, and every
// one of those survives a base name; what nobody outside this process may be
// told is where the run materialized the repository, nor where the repository
// itself lives. A rooted path is a path whatever produced it, so the rule
// needs no knowledge of which tool wrote the line.
//
// private are the directories this run made for the child and roots the other
// directories this process knows the child's paths live under (knownRoots).
// Both are matched as prefixes wherever a path starts, before the generic
// rule, so a known directory whose name holds a space is reduced whole.
func stderrTail(b []byte, private, roots []string) string {
	r := newReducer(private, roots)
	b = bytes.TrimRight(b, " \t\r\n")
	var lines []string
	size := 0
	for len(b) > 0 {
		line := b
		if i := bytes.LastIndexByte(b, '\n'); i >= 0 {
			line, b = b[i+1:], b[:i]
		} else {
			b = nil
		}
		reduced := r.reduce(string(bytes.TrimRight(line, "\r")))
		need := len(reduced)
		if len(lines) > 0 {
			need++ // the newline that joins it to the line after it
		}
		if size+need > model.MaxDetailBytes {
			if len(lines) == 0 {
				return lastFields(reduced)
			}
			break
		}
		lines = append(lines, reduced)
		size += need
	}
	slices.Reverse(lines)
	return strings.Join(lines, "\n")
}

// lastFields is the last model.MaxDetailBytes of one over-long line, starting
// at a field boundary so the reader is never handed half a word. A line with
// no boundary in its last bytes keeps them from the first whole character.
func lastFields(line string) string {
	s := line[len(line)-model.MaxDetailBytes:]
	if i := strings.IndexAny(s, " \t"); i >= 0 && i+1 < len(s) {
		return s[i+1:]
	}
	for len(s) > 0 && !utf8.RuneStart(s[0]) {
		s = s[1:]
	}
	return s
}

// privatePathName is what a directory this run made is called in a
// diagnostic, and what a known root is called when nothing follows it.
const privatePathName = "(private)"

// knownRoots are the directories outside the run's own that this process
// knows the child's paths live under: the user's home directory, the
// directories of the engine's launchers, and every absolute directory the
// child's environment names (the runtime's home among them). The data
// directory is not among them -- the backend is not told it -- so a path
// under it is reduced by the generic rule, or by the home directory when it
// lies beneath that.
func knownRoots(e dependence.Engine) []string {
	var roots []string
	if home, err := os.UserHomeDir(); err == nil && filepath.IsAbs(home) {
		roots = append(roots, filepath.Clean(home))
	}
	for _, argv := range [][]string{e.ParseArgv, e.ExportArgv} {
		for _, a := range argv {
			if filepath.IsAbs(a) {
				roots = append(roots, filepath.Dir(a))
			}
		}
	}
	for _, kv := range e.Env {
		if _, v, ok := strings.Cut(kv, "="); ok && filepath.IsAbs(v) && !strings.ContainsRune(v, os.PathListSeparator) {
			roots = append(roots, filepath.Clean(v))
		}
	}
	return roots
}

// pathTerminators end a rooted path inside a line. A path separator is never
// one of them, which is what lets the scan resume at the end of a reduced path
// without re-examining it. A path that holds a space under no known root is
// therefore reduced up to the space, and what follows it is left as the
// unrooted text it then is.
const pathTerminators = " \t)]}>|\"'`,;"

// isPathByte reports whether c continues a relative path. A separator after
// any other byte -- punctuation, an operator, a quote, a redirection -- starts
// a rooted path, so `2>/a/b`, `@/a/args`, `x=/a/b` and `"/a/b"` are all
// reduced. The separator itself continues a path, so the scan never re-roots
// inside one.
func isPathByte(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c >= 0x80:
		return true
	}
	return c == os.PathSeparator || strings.IndexByte("._-~%", c) >= 0
}

// root is one known directory and how a path under it is reduced.
type root struct {
	dir string
	// private keeps what follows the directory: the part inside a directory
	// this run made says which step wrote the file and publishes nothing.
	private bool
}

// reducer replaces every rooted path in a line.
type reducer struct{ roots []root }

// newReducer orders the known directories longest first, so a directory this
// run made under the home directory is matched as itself and not as a path
// under home.
func newReducer(private, known []string) reducer {
	var rs []root
	for _, d := range private {
		if d != "" && d != string(os.PathSeparator) {
			rs = append(rs, root{dir: filepath.Clean(d), private: true})
		}
	}
	for _, d := range known {
		if d != "" && d != string(os.PathSeparator) {
			rs = append(rs, root{dir: filepath.Clean(d)})
		}
	}
	slices.SortStableFunc(rs, func(a, b root) int { return cmp.Compare(len(b.dir), len(a.dir)) })
	return reducer{roots: rs}
}

// reduce replaces every rooted path in one line, leaving the punctuation
// around it in place: an operator still has to be able to read the line, so
// `cmd: "/a/b/tool"` is reduced to `cmd: "tool"` rather than to a bare name
// with its quoting gone. A path under a directory this run made becomes
// `(private)` followed by the rest of the path; a path under any other known
// directory, or under none, becomes its base name.
//
// Relative paths are left alone. They name something inside a directory the
// reader is not being told, so they publish nothing on their own, and they
// carry the step a file belongs to.
func (r reducer) reduce(line string) string {
	var b strings.Builder
	for i := 0; i < len(line); i++ {
		if line[i] != os.PathSeparator || (i > 0 && isPathByte(line[i-1])) {
			b.WriteByte(line[i])
			continue
		}
		rest := i
		under := root{}
		for _, rt := range r.roots {
			if within(line[i:], rt.dir) {
				under, rest = rt, i+len(rt.dir)
				break
			}
		}
		end := len(line)
		if j := strings.IndexAny(line[rest:], pathTerminators); j >= 0 {
			end = rest + j
		}
		switch {
		case under.private:
			b.WriteString(privatePathName)
			b.WriteString(line[rest:end])
		case under.dir != "" && (rest == end || line[rest:end] == string(os.PathSeparator)):
			b.WriteString(privatePathName)
		default:
			b.WriteString(filepath.Base(line[rest:end]))
		}
		i = end - 1
	}
	return b.String()
}

// within reports whether s starts with the directory dir as a whole path
// component: `/srv/data/x` is within `/srv/data`, `/srv/database` is not.
func within(s, dir string) bool {
	if !strings.HasPrefix(s, dir) {
		return false
	}
	return len(s) == len(dir) || s[len(dir)] == os.PathSeparator || !isPathByte(s[len(dir)])
}

// failedPass extracts the analysis pass name from either of the two lines the
// engine logs when a pass dies: `Pass <name> failed in <n> ms`, timed, and
// `Pass <name> failed`, untimed, which is the one a pass that threw before it
// could be timed leaves. Both are matched from the right of whatever precedes
// `failed`, because the log's own logger column is usually the same pass name
// and a left-to-right search finds that instead.
//
// Both forms are recognised because an unnamed pass is the difference between
// a crash the product knows reproduces and one it has to re-run to find out.
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
