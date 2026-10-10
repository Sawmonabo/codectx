package manifest

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path"
	"strconv"
	"strings"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider/filesystem"
)

// requirements reads a pip requirements file and every file it includes.
//
// The supported subset of pip's requirements file format is:
//
//   - A line ending in `\` continues on the next line; a comment line ends a
//     continuation.
//   - `#` at the start of a line, or after whitespace, starts a comment that
//     runs to the end of the line.
//   - A requirement line is a PEP 508 requirement: the name (PEP 503
//     normalized for identity) and, verbatim on the evidence as
//     `requirement`, its extras, version specifiers, direct reference and
//     environment markers. Markers are carried, never evaluated.
//   - Per-requirement options after the requirement (`--hash`,
//     `--config-settings`) are carried verbatim as `options`.
//   - `-r`/`--requirement` includes another requirements file and
//     `-c`/`--constraint` a constraints file, each resolved relative to the
//     directory of the file that names it and read from the same snapshot.
//     Every file is read once per mode, so an include cycle ends.
//   - A constraint line pins a version without installing anything, so it is
//     `configures` with `constraint: true`, never `depends_on`.
//
// Everything else is recorded in the node's `unresolved` metadata as written
// and never interpreted: every other option line (`-e`, `--index-url`,
// `--find-links`, ...), a requirement that is a bare URL, path or archive,
// and an include that is a URL, absolute, outside the workspace, empty, or
// names an environment variable. An include the snapshot does not hold is
// still a `configures` fact and leaves the capability partial. A requirement
// that names an environment variable (`${TOKEN}`) after its name is published
// with `unresolved: environment`.
func (u *unit) requirements(ctx context.Context) error {
	rel := u.e.File().Path
	w := &reqWalk{u: u, root: rel, visited: map[reqVisit]bool{{rel, false}: true}}
	queue := []reqVisit{{rel, false}}
	for len(queue) > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		next := queue[0]
		queue = queue[1:]
		data := u.data
		if next.path != rel {
			read, ok, err := u.readIncluded(ctx, next.path)
			if err != nil {
				return err
			}
			if !ok {
				continue
			}
			data = read
		}
		for _, l := range logicalLines(data) {
			if target, ok := w.line(next.path, l, next.constraint); ok && !w.visited[target] {
				w.visited[target] = true
				queue = append(queue, target)
			}
		}
	}
	meta := map[string]any{}
	if len(w.unresolved) > 0 {
		meta["unresolved"] = w.unresolved
	}
	cfg, err := u.defines(ctx, model.NodeConfiguration, ecosystemPyPI+":requirements:"+rel, path.Base(rel), languagePython, nil, meta)
	if err != nil {
		return err
	}
	for i, inc := range w.includes {
		if u.cut(BoundEntries, u.entries, int64(i+1)) {
			break
		}
		target, err := u.referencedFile(ctx, inc.target, inc.rng)
		if err != nil {
			return err
		}
		if err := u.e.Relation(cfg.ID, model.RelConfigures, target.ID, filesystem.Attrs{Precision: model.PrecisionSyntax, Range: inc.rng,
			Detail: filesystem.Detail("include", inc.mode, "via", inc.via)}); err != nil {
			return err
		}
	}
	for i, d := range w.deps {
		if u.cut(BoundDependencies, u.deps, int64(i+1)) {
			return nil
		}
		relKind, extra := model.RelDependsOn, []string{"options", d.options, "via", d.via}
		if d.constraint {
			relKind, extra = model.RelConfigures, append(extra, "constraint", "true")
		}
		if d.environment {
			extra = append(extra, "unresolved", "environment")
		}
		if err := u.edge(ctx, cfg, relKind, ecosystemPyPI, normalizePyPI(d.name), languagePython, KindRuntime, d.spec, d.rng, extra...); err != nil {
			return err
		}
	}
	return nil
}

// reqVisit is one file read in one mode. A file named both by `-r` and by
// `-c` is read twice, once as requirements and once as constraints, as pip
// reads it; any further mention of the same pair is the cycle guard's stop.
type reqVisit struct {
	path       string
	constraint bool
}

// reqWalk accumulates what the unit's file and its includes declare, in the
// order the breadth-first walk reads them.
type reqWalk struct {
	u          *unit
	root       string
	visited    map[reqVisit]bool
	deps       []reqDep
	includes   []reqInclude
	unresolved []map[string]string
}

// reqDep is one requirement or constraint line. via is the declaring file
// when it is not the unit's own, and rng is set only for the unit's own
// file: evidence ranges are in the unit's file.
type reqDep struct {
	name, spec, options, via string
	constraint, environment  bool
	rng                      *model.SourceRange
}

// reqInclude is one resolved `-r` or `-c` line.
type reqInclude struct {
	target, mode, via string
	rng               *model.SourceRange
}

// reqLogical is one logical line: the continuation-joined text with its
// comment removed, and the byte span it occupies in its file.
type reqLogical struct {
	text       string
	start, end int
}

// line classifies one logical line of file, read in constraint mode or not.
// It returns the include to follow, if the line is one.
func (w *reqWalk) line(file string, l reqLogical, constraint bool) (reqVisit, bool) {
	var rng *model.SourceRange
	via := ""
	if file == w.root {
		rng = w.u.rng(l.start, l.end)
	} else {
		via = file
	}
	if strings.HasPrefix(l.text, "-") {
		opt, value := splitOption(l.text)
		switch opt {
		case "-r", "--requirement":
			return w.include(file, value, false, l.text, via, rng)
		case "-c", "--constraint":
			return w.include(file, value, true, l.text, via, rng)
		}
		w.unresolve(l.text, via, "")
		return reqVisit{}, false
	}
	args, options := breakArgsOptions(l.text)
	name, spec := splitPEP508(args)
	if !pep508Name(name) || !pep508Rest(spec) || archiveName(name) {
		// A bare URL, path or archive installs what it names, which this
		// static reader cannot know.
		w.unresolve(l.text, via, "")
		return reqVisit{}, false
	}
	w.deps = append(w.deps, reqDep{name: name, spec: spec, options: options, via: via,
		constraint: constraint, environment: strings.Contains(l.text, "${"), rng: rng})
	return reqVisit{}, false
}

// include resolves an include named in file and records it, or records the
// line unresolved when it names nothing in the workspace.
func (w *reqWalk) include(file, value string, constraint bool, text, via string, rng *model.SourceRange) (reqVisit, bool) {
	mode := "requirement"
	if constraint {
		mode = "constraint"
	}
	switch {
	case value == "":
		w.unresolve(text, via, "malformed")
		w.u.malformedEntry()
		return reqVisit{}, false
	case strings.Contains(value, "://"):
		w.unresolve(text, via, "url")
		return reqVisit{}, false
	case strings.Contains(value, "${"):
		w.unresolve(text, via, "environment")
		return reqVisit{}, false
	case path.IsAbs(value) || (len(value) > 1 && value[1] == ':'):
		w.unresolve(text, via, "absolute")
		return reqVisit{}, false
	}
	target, ok := resolveRelative(path.Dir(file), value)
	if !ok {
		w.unresolve(text, via, "outside_workspace")
		return reqVisit{}, false
	}
	w.includes = append(w.includes, reqInclude{target: target, mode: mode, via: via, rng: rng})
	return reqVisit{target, constraint}, true
}

// unresolve records a line this reader does not interpret, with the file it
// came from when that is an include and the reason when the line itself is
// one pip would act on.
func (w *reqWalk) unresolve(text, via, reason string) {
	if w.u.cut(BoundEntries, w.u.entries, int64(len(w.unresolved)+1)) {
		return
	}
	entry := map[string]string{"line": text}
	if via != "" {
		entry["via"] = via
	}
	if reason != "" {
		entry["reason"] = reason
	}
	w.unresolved = append(w.unresolved, entry)
}

// readIncluded reads an included file from the unit's snapshot under the
// same parse admission as the unit's own file. ok is false for a file the
// snapshot does not hold (never retained, ignored, or deleted) and for one
// over the parse bound; both are recorded on the unit's capability.
func (u *unit) readIncluded(ctx context.Context, rel string) ([]byte, bool, error) {
	rc, fv, err := u.content.Open(ctx, model.NewFileID(u.repo, rel))
	if err != nil {
		var me *model.Error
		if errors.As(err, &me) && me.Code == model.CodeArgumentInvalid {
			// The included file is not in this snapshot: pip would fail
			// here, and the packages it declares are not visible.
			u.malformedEntry()
			return nil, false, nil
		}
		return nil, false, err
	}
	defer rc.Close()
	if u.maxParse.Exceeded(fv.Size) {
		u.degraded("max_parse_file_bytes", rel+" is "+strconv.FormatInt(fv.Size, 10)+" over "+u.maxParse.String())
		return nil, false, nil
	}
	data, err := io.ReadAll(io.LimitReader(rc, fv.Size+1))
	if err != nil {
		return nil, false, err
	}
	if int64(len(data)) != fv.Size {
		return nil, false, &model.Error{Code: model.CodeSourceIntegrity, Message: "retained bytes differ in length from the included requirements file"}
	}
	u.e.AddBytes(uint64(len(data)))
	return data, true, nil
}

// logicalLines splits a requirements file into logical lines the way pip
// does: continuations are joined first (a trailing `\` is dropped and the
// next line appended; a comment line ends the run), then comments are
// removed and the text trimmed. Empty results are skipped. The span runs
// from the first non-blank byte of the first physical line to the last
// non-blank, non-comment byte of the last one.
func logicalLines(data []byte) []reqLogical {
	var out []reqLogical
	var buf strings.Builder
	open, start := false, 0
	emit := func(text string, start, lastLine, end int) {
		if c := commentStart(string(data[lastLine:end])); c >= 0 {
			end = lastLine + c
		}
		text = strings.TrimSpace(stripComment(text))
		if text == "" {
			return
		}
		for start < end && spanTrim(data[start]) {
			start++
		}
		for end > start && spanTrim(data[end-1]) {
			end--
		}
		out = append(out, reqLogical{text: text, start: start, end: end})
	}
	for pos := 0; pos < len(data); {
		lineEnd, next := len(data), len(data)
		if nl := bytes.IndexByte(data[pos:], '\n'); nl >= 0 {
			lineEnd, next = pos+nl, pos+nl+1
		}
		if lineEnd > pos && data[lineEnd-1] == '\r' {
			lineEnd--
		}
		line := string(data[pos:lineEnd])
		comment := strings.HasPrefix(strings.TrimLeft(line, " \t"), "#")
		switch {
		case !strings.HasSuffix(line, `\`) || comment:
			if comment {
				// pip keeps the comment separated so it is still removed
				// once joined.
				line = " " + line
			}
			if open {
				buf.WriteString(line)
				emit(buf.String(), start, pos, lineEnd)
				buf.Reset()
				open = false
			} else {
				emit(line, pos, pos, lineEnd)
			}
		default:
			if !open {
				open, start = true, pos
			}
			buf.WriteString(strings.Trim(line, `\`))
		}
		pos = next
	}
	if open {
		// A continuation at the end of the file ends there.
		emit(buf.String(), start, len(data), len(data))
	}
	return out
}

// commentStart is the offset of the `#` that starts a comment in s: at the
// start of s or after whitespace. It is -1 when s has none.
func commentStart(s string) int {
	for i := 0; i < len(s); i++ {
		if s[i] == '#' && (i == 0 || isBlank(s[i-1])) {
			return i
		}
	}
	return -1
}

func stripComment(s string) string {
	if c := commentStart(s); c >= 0 {
		return s[:c]
	}
	return s
}

func isBlank(c byte) bool { return c == ' ' || c == '\t' || c == '\r' || c == '\f' || c == '\v' }

// spanTrim is what a logical line's span does not start or end on: blanks,
// and the newline and backslash of a continuation that a comment line ended.
func spanTrim(c byte) bool { return isBlank(c) || c == '\n' || c == '\\' }

// splitOption separates an option line into the option and its first value:
// `--opt=value`, `--opt value`, `-o value` and `-ovalue`. A quoted value
// loses its quotes.
func splitOption(text string) (opt, value string) {
	fields := strings.Fields(text)
	opt = fields[0]
	switch {
	case strings.HasPrefix(opt, "--"):
		if i := strings.IndexByte(opt, '='); i >= 0 {
			opt, value = opt[:i], opt[i+1:]
		} else if len(fields) > 1 {
			value = fields[1]
		}
	case len(opt) > 2:
		opt, value = opt[:2], opt[2:]
	case len(fields) > 1:
		value = fields[1]
	}
	if len(value) >= 2 && (value[0] == '"' || value[0] == '\'') && value[len(value)-1] == value[0] {
		value = value[1 : len(value)-1]
	}
	return opt, value
}

// breakArgsOptions splits a requirement line at its first space-separated
// token that starts with `-`, as pip does: the requirement before it, the
// per-requirement options from it on.
func breakArgsOptions(text string) (args, options string) {
	tokens := strings.Split(text, " ")
	for i, tok := range tokens {
		if strings.HasPrefix(tok, "-") {
			return strings.TrimSpace(strings.Join(tokens[:i], " ")), strings.TrimSpace(strings.Join(tokens[i:], " "))
		}
	}
	return text, ""
}

// pep508Name reports whether name is a valid PEP 508 project name: letters,
// digits, `-`, `_` and `.`, starting and ending with a letter or digit.
func pep508Name(name string) bool {
	if name == "" {
		return false
	}
	alnum := func(c byte) bool { return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') }
	return alnum(name[0]) && alnum(name[len(name)-1])
}

// pep508Rest reports whether what follows the name can continue a PEP 508
// requirement: nothing, extras, a version specifier, a direct reference or
// markers. A URL's `://` or a path's `/` cannot.
func pep508Rest(rest string) bool {
	if rest == "" {
		return true
	}
	return strings.ContainsRune("[(<>=!~;@", rune(rest[0]))
}

// archiveName reports whether a name-shaped requirement is a local archive
// (`pkg-1.0.tar.gz`, `pkg-1.0-py3-none-any.whl`), which pip installs as a
// path.
func archiveName(name string) bool {
	lower := strings.ToLower(name)
	for _, ext := range []string{".whl", ".zip", ".tar", ".tar.gz", ".tgz", ".tar.bz2", ".tbz", ".tar.xz", ".txz"} {
		if strings.HasSuffix(lower, ext) {
			return true
		}
	}
	return false
}
