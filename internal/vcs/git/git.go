// Package git runs the Git plumbing the snapshot builder needs and interprets
// its NUL-delimited output. It is the only place Git is executed.
//
// Every command goes through the shared process runner with a literal argv, an
// absolute executable resolved once, and a fixed environment allowlist: the
// child never inherits the parent's environment. The commands used here read
// the index and the worktree; none of them runs hooks, writes the index
// (GIT_OPTIONAL_LOCKS=0) or asks a terminal for credentials. The fsmonitor
// hook is disabled explicitly because `git status` would otherwise run a
// repository-configured executable. Git object IDs this package reports are
// provenance only: the bytes a snapshot retains are always read from the
// worktree, because attributes can make a checkout differ from its blob.
package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/process"
)

const (
	// DefaultTimeout bounds one plumbing run. Listing a quarter-million-file
	// index is seconds; a run that takes longer than this is stuck.
	DefaultTimeout = 10 * time.Minute
	grace          = 5 * time.Second
	// maxStderrBytes bounds diagnostics; Git's error text is short.
	maxStderrBytes = 64 << 10
	// maxRecordBytes bounds one NUL-delimited record: the longest legal path
	// plus the fixed-width status fields that precede it.
	maxRecordBytes = model.MaxPathBytes + 256
	// stderrExcerptBytes bounds the sanitized Git diagnostic carried in an
	// error message.
	stderrExcerptBytes = 200
)

// envAllowlist names the parent variables a Git child may see. HOME and
// XDG_CONFIG_HOME locate the operator's own (trusted, user-level) Git
// configuration; USERPROFILE and SYSTEMROOT are what Git for Windows needs to
// start at all; the temporary-directory variables let Git create its scratch
// files where the operator expects. Nothing else -- no credentials, proxies or
// PATH -- crosses the boundary.
var envAllowlist = []string{"HOME", "XDG_CONFIG_HOME", "USERPROFILE", "SYSTEMROOT", "TMPDIR", "TEMP", "TMP"}

// fixedEnv is the safe process environment of Section 21, set on every run.
var fixedEnv = []string{
	"GIT_CONFIG_NOSYSTEM=1",
	"GIT_OPTIONAL_LOCKS=0",
	"GIT_TERMINAL_PROMPT=0",
	"LC_ALL=C",
}

// safeConfig disables the one repository-configurable executable the commands
// used here would otherwise start.
var safeConfig = []string{"-c", "core.fsmonitor=false"}

// Git executes plumbing against one absolute Git executable.
type Git struct {
	runner  *process.Runner
	path    string
	env     []string
	timeout time.Duration
}

// Locate resolves the git executable on PATH once and returns its absolute
// path. The result is recorded in Git at construction; nothing resolves PATH
// again per command.
func Locate() (string, error) {
	found, err := exec.LookPath("git")
	if err != nil {
		return "", unavailable("git is not installed or not on PATH: %v", err)
	}
	abs, err := filepath.Abs(found)
	if err != nil {
		return "", unavailable("git path %q cannot be resolved: %v", found, err)
	}
	return abs, nil
}

// New records the absolute executable and captures the allowlisted environment
// once. timeout <= 0 selects DefaultTimeout.
func New(runner *process.Runner, executable string, timeout time.Duration) (*Git, error) {
	if runner == nil {
		return nil, internal("git requires the shared process runner")
	}
	if !filepath.IsAbs(executable) {
		return nil, &model.Error{Code: model.CodeTrustRequired,
			Message: fmt.Sprintf("git executable %q is not an absolute path", executable)}
	}
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	env := append([]string(nil), fixedEnv...)
	for _, name := range envAllowlist {
		if value, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+value)
		}
	}
	return &Git{runner: runner, path: executable, env: env, timeout: timeout}, nil
}

// Executable is the recorded absolute path.
func (g *Git) Executable() string { return g.path }

// IndexEntry is one `git ls-files -s` row: what the index tracks at a path.
type IndexEntry struct {
	Path string
	// Mode is the octal index mode: 100644 or 100755 for a file, 120000 for a
	// symbolic link and 160000 for a submodule (gitlink).
	Mode     string
	ObjectID string
	Stage    int
}

// Index modes with capture consequences.
const (
	ModeSymlink    = "120000"
	ModeGitlink    = "160000"
	ModeExecutable = "100755"
)

// ChangeKind classifies one `git status --porcelain=v2` row against the
// snapshot_files vocabulary.
type ChangeKind string

const (
	ChangeUntracked ChangeKind = "untracked"
	ChangeAdded     ChangeKind = "added"
	ChangeModified  ChangeKind = "modified"
	ChangeDeleted   ChangeKind = "deleted"
)

// Change is one path Git reports as differing from HEAD or as untracked.
// Paths Git considers clean are not reported at all.
type Change struct {
	Path string
	Kind ChangeKind
}

// Head returns the full object ID HEAD resolves to, or "" for a repository
// with no commits yet.
func (g *Git) Head(ctx context.Context, root string) (string, error) {
	var out bytes.Buffer
	result, err := g.run(ctx, root, &out, 128, "rev-parse", "--verify", "--quiet", "HEAD")
	if err != nil {
		if result.ExitCode == 1 && !result.TimedOut && !result.Canceled {
			// --quiet: exit 1 is exactly "HEAD does not resolve".
			return "", nil
		}
		return "", err
	}
	return strings.TrimSpace(out.String()), nil
}

// ListIndex streams every index entry (`git ls-files -z -s`) to visit. The
// output is parsed as it arrives; no repository-sized list is built here.
func (g *Git) ListIndex(ctx context.Context, root string, maxEntries int64, visit func(IndexEntry) error) error {
	p := newParser(func(record []byte) error {
		// "<mode> <oid> <stage>\t<path>"
		meta, path, ok := bytes.Cut(record, []byte{'\t'})
		fields := bytes.Fields(meta)
		if !ok || len(fields) != 3 || len(path) == 0 {
			return corruptOutput("ls-files", record)
		}
		stage, err := strconv.Atoi(string(fields[2]))
		if err != nil {
			return corruptOutput("ls-files", record)
		}
		return visit(IndexEntry{Path: string(path), Mode: string(fields[0]), ObjectID: string(fields[1]), Stage: stage})
	})
	return g.stream(ctx, root, p, maxEntries, "ls-files", "-z", "-s")
}

// Status streams every path that differs from HEAD, and every untracked
// non-ignored path when untracked is set, to visit. Rename detection is off so
// a rename arrives as a deletion and an addition, matching path identity.
// Ignore rules are applied by Git itself to untracked discovery.
func (g *Git) Status(ctx context.Context, root string, untracked bool, maxEntries int64, visit func(Change) error) error {
	skipNext := false
	p := newParser(func(record []byte) error {
		if skipNext {
			// The original path of a rename/copy entry; the entry itself was
			// already classified.
			skipNext = false
			return nil
		}
		if len(record) < 2 {
			return corruptOutput("status", record)
		}
		switch record[0] {
		case '?':
			return visit(Change{Path: string(record[2:]), Kind: ChangeUntracked})
		case '!', '#':
			return nil
		case '1', '2', 'u':
			// "1 <XY> <sub> <mH> <mI> <mW> <hH> <hI> <path>"
			// "2 ... <X><score> <path>" followed by the original path record
			// "u <XY> <sub> <m1> <m2> <m3> <mW> <h1> <h2> <h3> <path>"
			fieldCount := map[byte]int{'1': 8, '2': 9, 'u': 10}[record[0]]
			path, xy, ok := splitFields(record, fieldCount)
			if !ok {
				return corruptOutput("status", record)
			}
			skipNext = record[0] == '2'
			return visit(Change{Path: path, Kind: classify(xy)})
		}
		return corruptOutput("status", record)
	})
	args := []string{"status", "-z", "--porcelain=v2", "--no-renames"}
	if untracked {
		args = append(args, "--untracked-files=all")
	} else {
		args = append(args, "--untracked-files=no")
	}
	return g.stream(ctx, root, p, maxEntries, args...)
}

// splitFields returns the path after n space-separated fields and the second
// field (XY).
func splitFields(record []byte, n int) (path, xy string, ok bool) {
	rest := record
	for i := 0; i < n; i++ {
		field, tail, found := bytes.Cut(rest, []byte{' '})
		if !found {
			return "", "", false
		}
		if i == 1 {
			xy = string(field)
		}
		rest = tail
	}
	if len(rest) == 0 || len(xy) != 2 {
		return "", "", false
	}
	return string(rest), xy, true
}

// classify maps a porcelain XY pair to a change kind. A deletion on either
// side wins because the worktree bytes are gone; an addition on either side
// means HEAD has no version; anything else differs in content or mode.
func classify(xy string) ChangeKind {
	switch {
	case strings.ContainsRune(xy, 'D'):
		return ChangeDeleted
	case strings.ContainsRune(xy, 'A'):
		return ChangeAdded
	default:
		return ChangeModified
	}
}

// SparseCheckout reports whether the worktree is a sparse checkout, in which
// case absent tracked paths are not deletions of the operator's making.
func (g *Git) SparseCheckout(ctx context.Context, root string) (bool, error) {
	var out bytes.Buffer
	if _, err := g.run(ctx, root, &out, 64, "config", "--type=bool", "--default=false", "core.sparseCheckout"); err != nil {
		return false, err
	}
	return strings.TrimSpace(out.String()) == "true", nil
}

// CatBlob streams the raw object oid (no filters applied) into sink, refusing
// more than maxBytes. It is the explicit repair source of Section 10.4; the
// caller verifies the complete SHA-256 before publishing anything.
func (g *Git) CatBlob(ctx context.Context, root, oid string, sink io.Writer, maxBytes int64) error {
	if !isHexObjectID(oid) {
		return &model.Error{Code: model.CodeArgumentInvalid, Message: "git object id is not hexadecimal"}
	}
	_, err := g.run(ctx, root, sink, maxBytes, "cat-file", "blob", oid)
	return err
}

func isHexObjectID(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// stream runs one listing command, feeding its stdout through p as it arrives.
// The stdout bound is derived from the entry budget: a listing longer than the
// configured file budget is a typed limit, never a truncated manifest.
func (g *Git) stream(ctx context.Context, root string, p *parser, maxEntries int64, args ...string) error {
	if maxEntries <= 0 {
		return &model.Error{Code: model.CodeResourceLimit, Message: "git listing needs a positive entry budget"}
	}
	p.maxRecords = maxEntries
	_, err := g.run(ctx, root, p, maxEntries*maxRecordBytes, args...)
	if p.err != nil {
		// The parser's own typed rejection is the cause; the runner only saw
		// its sink refuse bytes.
		return p.err
	}
	if err != nil {
		return err
	}
	return p.finish()
}

// run executes git with safeConfig and args in root, delivering stdout to
// sink. The Result is returned alongside the error so callers can interpret
// exit codes the command documents.
func (g *Git) run(ctx context.Context, root string, sink io.Writer, maxStdout int64, args ...string) (process.Result, error) {
	if !filepath.IsAbs(root) {
		return process.Result{}, internal("git working directory must be absolute")
	}
	var stderr bytes.Buffer
	argv := append(append([]string(nil), safeConfig...), args...)
	result, err := g.runner.Run(ctx, process.Spec{
		Path:           g.path,
		Args:           argv,
		Dir:            root,
		Env:            g.env,
		Stdout:         sink,
		Stderr:         &stderr,
		MaxStdoutBytes: maxStdout,
		MaxStderrBytes: maxStderrBytes,
		Timeout:        g.timeout,
		Grace:          grace,
	})
	if err != nil {
		var typed *model.Error
		if errors.As(err, &typed) && typed.Code == model.CodeProviderUnavailable {
			// The runner knows only that the child exited non-zero; Git's own
			// first diagnostic line, sanitized, says why.
			typed.Message = "git " + args[0] + " failed: " + excerpt(stderr.Bytes())
		}
		return result, err
	}
	return result, nil
}

// excerpt returns the first line of Git's diagnostic with control characters
// removed and a fixed byte ceiling, so a message is safe to display and can
// never grow with the output.
func excerpt(stderr []byte) string {
	line, _, _ := bytes.Cut(stderr, []byte{'\n'})
	clean := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, string(line))
	if len(clean) > stderrExcerptBytes {
		clean = clean[:stderrExcerptBytes]
		for len(clean) > 0 && !utf8RuneStart(clean[len(clean)-1]) {
			clean = clean[:len(clean)-1]
		}
	}
	if clean == "" {
		return "no diagnostic"
	}
	return clean
}

func utf8RuneStart(b byte) bool { return b&0xC0 != 0x80 }

// parser splits a NUL-delimited stream into records as bytes arrive. It is the
// runner's stdout sink, so it runs on the runner's drain goroutine; the
// caller's visit function runs there too and must not touch the caller's
// state concurrently with anything else, which the builder guarantees by
// waiting for Run to return.
type parser struct {
	visit      func(record []byte) error
	buf        []byte
	records    int64
	maxRecords int64
	err        error
}

func newParser(visit func(record []byte) error) *parser {
	return &parser{visit: visit}
}

func (p *parser) Write(b []byte) (int, error) {
	if p.err != nil {
		return 0, p.err
	}
	n := len(b)
	for len(b) > 0 {
		i := bytes.IndexByte(b, 0)
		if i < 0 {
			if len(p.buf)+len(b) > maxRecordBytes {
				p.err = corruptOutput("git", nil)
				return 0, p.err
			}
			p.buf = append(p.buf, b...)
			return n, nil
		}
		record := b[:i]
		if len(p.buf) > 0 {
			record = append(p.buf, record...)
		}
		if len(record) > maxRecordBytes {
			p.err = corruptOutput("git", nil)
			return 0, p.err
		}
		p.records++
		if p.records > p.maxRecords {
			p.err = &model.Error{Code: model.CodeResourceLimit,
				Message: fmt.Sprintf("git listed more than the configured %d paths", p.maxRecords)}
			return 0, p.err
		}
		if err := p.visit(record); err != nil {
			p.err = err
			return 0, err
		}
		p.buf = p.buf[:0]
		b = b[i+1:]
	}
	return n, nil
}

// finish rejects a stream that ended mid-record.
func (p *parser) finish() error {
	if len(p.buf) > 0 {
		return corruptOutput("git", nil)
	}
	return nil
}

func corruptOutput(command string, record []byte) *model.Error {
	msg := "git " + command + " produced a record this build cannot interpret"
	if record != nil {
		msg += fmt.Sprintf(" (%d bytes)", len(record))
	}
	return &model.Error{Code: model.CodeProviderOutputInvalid, Message: msg}
}

func unavailable(format string, args ...any) *model.Error {
	return &model.Error{Code: model.CodeProviderUnavailable, Message: fmt.Sprintf(format, args...),
		Remediation: "install git or configure the workspace without Git tracking"}
}

func internal(format string, args ...any) *model.Error {
	return &model.Error{Code: model.CodeInternal, Message: fmt.Sprintf(format, args...)}
}
