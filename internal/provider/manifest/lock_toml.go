package manifest

import (
	"bytes"
	"context"
	"sort"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
)

// The TOML lock files are arrays of tables, one table per package
// (`[[package]]`, PEP 751's `[[packages]]`), each followed by its own
// sub-tables. tomlSegments cuts the document at its top-level table headers
// with a lexer that knows strings, comments and brackets, so a header is
// only ever a header; each package's segment is then decoded alone with the
// pinned decoder. The largest value decoded is one package, never the file.

// tomlSegment is one top-level part of a TOML document: the lines before the
// first header (name ""), a `[table]`, or one element of an array of tables
// together with the sub-tables that follow it.
type tomlSegment struct {
	name       string
	array      bool
	start, end int
}

// tomlSegments yields each segment of data in order. A header continues the
// current segment only when that segment is an array element and the header
// names a table inside it (`[package.dependencies]` after `[[package]]`),
// which is what TOML binds it to.
func tomlSegments(data []byte, each func(tomlSegment) error) error {
	cur := tomlSegment{}
	var lx tomlLexer
	for off := 0; off < len(data); {
		next := bytes.IndexByte(data[off:], '\n')
		end := len(data)
		if next >= 0 {
			end = off + next + 1
		}
		line := data[off:end]
		if lx.idle() {
			if text := bytes.TrimLeft(line, " \t"); len(text) > 0 && text[0] == '[' {
				array := len(text) > 1 && text[1] == '['
				name := tomlHeader(strings.TrimSpace(string(text)))
				if !(cur.array && strings.HasPrefix(name, cur.name+".")) {
					cur.end = off
					if err := each(cur); err != nil {
						return err
					}
					cur = tomlSegment{name: name, array: array, start: off}
				}
				off = end
				continue
			}
		}
		lx.scan(line)
		off = end
	}
	cur.end = len(data)
	return each(cur)
}

// tomlLexer tracks what spans lines in a TOML document: a multi-line
// string and an open array or inline table.
type tomlLexer struct {
	str   string // the open multi-line string's delimiter, `"""` or `'''`
	depth int
}

func (l *tomlLexer) idle() bool { return l.str == "" && l.depth == 0 }

// scan advances the lexer over one line.
func (l *tomlLexer) scan(line []byte) {
	for i := 0; i < len(line); i++ {
		if l.str != "" {
			if bytes.HasPrefix(line[i:], []byte(l.str)) {
				i += len(l.str) - 1
				l.str = ""
			} else if line[i] == '\\' && l.str == `"""` {
				i++
			}
			continue
		}
		switch c := line[i]; c {
		case '#':
			return
		case '"', '\'':
			delim := strings.Repeat(string(c), 3)
			if bytes.HasPrefix(line[i:], []byte(delim)) {
				l.str = delim
				i += 2
				continue
			}
			for i++; i < len(line) && line[i] != c; i++ {
				if c == '"' && line[i] == '\\' {
					i++
				}
			}
		case '[', '{':
			l.depth++
		case ']', '}':
			if l.depth > 0 {
				l.depth--
			}
		}
	}
}

// tomlLockVersion reads key from the segment before the first header (table
// "") or from the top-level table named table, which is where lock files
// keep their format version; any other segment yields "".
func tomlLockVersion(data []byte, seg tomlSegment, table, key string) string {
	if seg.name != table || seg.array {
		return ""
	}
	var top map[string]any
	if err := toml.Unmarshal(data[seg.start:seg.end], &top); err != nil {
		return ""
	}
	if table != "" {
		top, _ = top[table].(map[string]any)
	}
	switch v := top[key].(type) {
	case string:
		return v
	case int64:
		return strconv.FormatInt(v, 10)
	}
	return ""
}

// tomlLock reads a TOML lock file whose packages are the array named array:
// each package segment is decoded alone by decode, which returns its entry,
// and the format version is read from table.key.
func (u *unit) tomlLock(ctx context.Context, array, table, key, ecosystem, language string, decode func(seg []byte) (lockEntry, bool)) error {
	var entries []lockEntry
	version := ""
	err := tomlSegments(u.data, func(seg tomlSegment) error {
		if v := tomlLockVersion(u.data, seg, table, key); v != "" {
			version = v
		}
		if !seg.array || seg.name != array {
			return nil
		}
		e, ok := decode(u.data[seg.start:seg.end])
		if !ok {
			u.malformedEntry()
			return nil
		}
		e.start, e.end = seg.start, trimEnd(u.data, seg.start, seg.end)
		entries = append(entries, e)
		return nil
	})
	if err != nil {
		return err
	}
	return u.lock(ctx, lockFile{ecosystem: ecosystem, language: language, version: version, entries: entries})
}

// trimEnd drops the trailing blank lines of a segment from its span.
func trimEnd(data []byte, start, end int) int {
	for end > start && (data[end-1] == '\n' || data[end-1] == '\r' || data[end-1] == ' ' || data[end-1] == '\t') {
		end--
	}
	return end
}

// cargoLock reads Cargo.lock: `[[package]]` with name, version, source,
// checksum and dependencies written "name", "name version" or
// "name version (source)" -- the version is written only when the lock
// holds several versions of the name. A package without a source is a
// member of the workspace or a path dependency.
func (u *unit) cargoLock(ctx context.Context) error {
	return u.tomlLock(ctx, "package", "", "version", ecosystemCargo, languageRust, func(seg []byte) (lockEntry, bool) {
		var doc struct {
			Package []struct {
				Name         string   `toml:"name"`
				Version      string   `toml:"version"`
				Source       string   `toml:"source"`
				Checksum     string   `toml:"checksum"`
				Dependencies []string `toml:"dependencies"`
			} `toml:"package"`
		}
		if err := toml.Unmarshal(seg, &doc); err != nil || len(doc.Package) != 1 {
			return lockEntry{}, false
		}
		p := doc.Package[0]
		e := lockEntry{name: p.Name, version: p.Version, integrity: p.Checksum, source: p.Source, local: p.Source == ""}
		for _, d := range p.Dependencies {
			fields := strings.Fields(d)
			if len(fields) == 0 {
				continue
			}
			dep := lockDep{name: fields[0]}
			if len(fields) > 1 {
				dep.version = fields[1]
			}
			e.deps = append(e.deps, dep)
		}
		return e, true
	})
}

// pyFile is one hashed distribution file of a Python lock.
type pyFile struct {
	File string `toml:"file"`
	Hash string `toml:"hash"`
}

// sdistHash is the hash of a package's one source distribution among its
// files, or "" when it has none or several.
func sdistHash(files []pyFile) string {
	hash := ""
	for _, f := range files {
		if strings.HasSuffix(f.File, ".tar.gz") || strings.HasSuffix(f.File, ".zip") {
			if hash != "" {
				return ""
			}
			hash = f.Hash
		}
	}
	return hash
}

// poetryLock reads poetry.lock: `[[package]]` with name, version, its files
// and their hashes, its source and `[package.dependencies]`, whose values
// are a requirement, a table with a `version`, or a list of such tables for
// requirements that differ by marker.
func (u *unit) poetryLock(ctx context.Context) error {
	return u.tomlLock(ctx, "package", "metadata", "lock-version", ecosystemPyPI, languagePython, func(seg []byte) (lockEntry, bool) {
		var doc struct {
			Package []struct {
				Name     string   `toml:"name"`
				Version  string   `toml:"version"`
				Category string   `toml:"category"`
				Groups   []string `toml:"groups"`
				Files    []pyFile `toml:"files"`
				Source   *struct {
					Type      string `toml:"type"`
					URL       string `toml:"url"`
					Reference string `toml:"resolved_reference"`
				} `toml:"source"`
				Dependencies map[string]any `toml:"dependencies"`
			} `toml:"package"`
		}
		if err := toml.Unmarshal(seg, &doc); err != nil || len(doc.Package) != 1 {
			return lockEntry{}, false
		}
		p := doc.Package[0]
		e := lockEntry{name: normalizePyPI(p.Name), version: p.Version, integrity: sdistHash(p.Files), hashes: len(p.Files),
			group: strings.Join(p.Groups, ",")}
		if e.group == "" {
			e.group = p.Category
		}
		if s := p.Source; s != nil {
			e.source = s.URL
			if s.Reference != "" {
				e.source += "@" + s.Reference
			}
			e.local = s.Type == "directory" || s.Type == "file"
		}
		for _, n := range sortedKeys(p.Dependencies) {
			e.deps = append(e.deps, lockDep{name: normalizePyPI(n), requirement: poetryRequirement(p.Dependencies[n])})
		}
		return e, true
	})
}

// poetryRequirement renders a poetry.lock dependency value: the string, a
// table's version and markers, or each element of a list of tables,
// separated by " || " as alternatives that differ by marker.
func poetryRequirement(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case map[string]any:
		req := asString(t["version"])
		if m := asString(t["markers"]); m != "" {
			req += " ; " + m
		}
		return req
	case []map[string]any:
		parts := make([]string, len(t))
		for i, m := range t {
			parts[i] = poetryRequirement(m)
		}
		return strings.Join(parts, " || ")
	case []any:
		parts := make([]string, len(t))
		for i, m := range t {
			parts[i] = poetryRequirement(m)
		}
		return strings.Join(parts, " || ")
	}
	return ""
}

// uvDep is one dependency of a uv.lock package: the name, the version only
// when the lock holds several of the name, and the marker it applies under.
type uvDep struct {
	Name    string `toml:"name"`
	Version string `toml:"version"`
	Marker  string `toml:"marker"`
}

// uvLock reads uv.lock: `[[package]]` with name, version, a source table
// (registry, git, url, path, directory, editable or virtual), dependencies,
// optional and development dependencies by group, and the hashes of its
// source distribution and wheels.
func (u *unit) uvLock(ctx context.Context) error {
	return u.tomlLock(ctx, "package", "", "version", ecosystemPyPI, languagePython, func(seg []byte) (lockEntry, bool) {
		var doc struct {
			Package []struct {
				Name     string             `toml:"name"`
				Version  string             `toml:"version"`
				Source   map[string]any     `toml:"source"`
				Deps     []uvDep            `toml:"dependencies"`
				Optional map[string][]uvDep `toml:"optional-dependencies"`
				Dev      map[string][]uvDep `toml:"dev-dependencies"`
				Sdist    *struct {
					Hash string `toml:"hash"`
				} `toml:"sdist"`
				Wheels []struct {
					Hash string `toml:"hash"`
				} `toml:"wheels"`
			} `toml:"package"`
		}
		if err := toml.Unmarshal(seg, &doc); err != nil || len(doc.Package) != 1 {
			return lockEntry{}, false
		}
		p := doc.Package[0]
		e := lockEntry{name: normalizePyPI(p.Name), version: p.Version, hashes: len(p.Wheels)}
		if p.Sdist != nil {
			e.integrity = p.Sdist.Hash
			e.hashes++
		}
		for _, k := range sortedKeys(p.Source) {
			e.source = k + "=" + asString(p.Source[k])
			e.local = k == "editable" || k == "virtual" || k == "path" || k == "directory"
		}
		add := func(deps []uvDep, kind, group string) {
			for _, d := range deps {
				req := ""
				if d.Marker != "" {
					req = "; " + d.Marker
				}
				if group != "" {
					req = strings.TrimSpace("[" + group + "] " + req)
				}
				e.deps = append(e.deps, lockDep{name: normalizePyPI(d.Name), version: d.Version, requirement: req, kind: kind})
			}
		}
		add(p.Deps, KindRuntime, "")
		for _, g := range sortedKeys(p.Optional) {
			add(p.Optional[g], KindOptional, g)
		}
		for _, g := range sortedKeys(p.Dev) {
			add(p.Dev[g], KindDev, g)
		}
		return e, true
	})
}

// pylockHashes renders a PEP 751 hashes table, algorithm to digest, as
// "algorithm:digest" pairs in algorithm order.
func pylockHashes(h map[string]string) string {
	algs := make([]string, 0, len(h))
	for a := range h {
		algs = append(algs, a)
	}
	sort.Strings(algs)
	parts := make([]string, len(algs))
	for i, a := range algs {
		parts[i] = a + ":" + h[a]
	}
	return strings.Join(parts, " ")
}

// pylock reads a PEP 751 lock file (pylock.toml, pylock.<name>.toml):
// `[[packages]]` with name, version, the dependencies that identify other
// packages of the file, and exactly one source: an index's sdist and
// wheels, a VCS checkout, a directory or an archive, each with its hashes.
func (u *unit) pylock(ctx context.Context) error {
	type hashed struct {
		Hashes map[string]string `toml:"hashes"`
	}
	return u.tomlLock(ctx, "packages", "", "lock-version", ecosystemPyPI, languagePython, func(seg []byte) (lockEntry, bool) {
		var doc struct {
			Packages []struct {
				Name         string `toml:"name"`
				Version      string `toml:"version"`
				Index        string `toml:"index"`
				Dependencies []struct {
					Name    string `toml:"name"`
					Version string `toml:"version"`
				} `toml:"dependencies"`
				VCS *struct {
					URL    string `toml:"url"`
					Path   string `toml:"path"`
					Commit string `toml:"commit-id"`
				} `toml:"vcs"`
				Directory *struct {
					Path string `toml:"path"`
				} `toml:"directory"`
				Archive *struct {
					URL    string            `toml:"url"`
					Path   string            `toml:"path"`
					Hashes map[string]string `toml:"hashes"`
				} `toml:"archive"`
				Sdist  *hashed  `toml:"sdist"`
				Wheels []hashed `toml:"wheels"`
			} `toml:"packages"`
		}
		if err := toml.Unmarshal(seg, &doc); err != nil || len(doc.Packages) != 1 {
			return lockEntry{}, false
		}
		p := doc.Packages[0]
		e := lockEntry{name: normalizePyPI(p.Name), version: p.Version, source: p.Index, hashes: len(p.Wheels)}
		switch {
		case p.Sdist != nil:
			e.integrity = pylockHashes(p.Sdist.Hashes)
			e.hashes++
		case p.Archive != nil:
			e.integrity, e.source = pylockHashes(p.Archive.Hashes), p.Archive.URL+p.Archive.Path
			e.hashes++
		case p.VCS != nil:
			e.source = p.VCS.URL + p.VCS.Path + "@" + p.VCS.Commit
		case p.Directory != nil:
			e.source, e.local = p.Directory.Path, true
		}
		for _, d := range p.Dependencies {
			e.deps = append(e.deps, lockDep{name: normalizePyPI(d.Name), version: d.Version})
		}
		return e, true
	})
}

// pdmLock reads pdm.lock: `[[package]]` with name, version, the groups it
// belongs to, dependencies as PEP 508 requirement strings and its files'
// hashes; a path, editable or VCS package says where it comes from.
func (u *unit) pdmLock(ctx context.Context) error {
	return u.tomlLock(ctx, "package", "metadata", "lock_version", ecosystemPyPI, languagePython, func(seg []byte) (lockEntry, bool) {
		var doc struct {
			Package []struct {
				Name         string   `toml:"name"`
				Version      string   `toml:"version"`
				Groups       []string `toml:"groups"`
				Dependencies []string `toml:"dependencies"`
				Files        []pyFile `toml:"files"`
				Path         string   `toml:"path"`
				Git          string   `toml:"git"`
				Revision     string   `toml:"revision"`
				URL          string   `toml:"url"`
			} `toml:"package"`
		}
		if err := toml.Unmarshal(seg, &doc); err != nil || len(doc.Package) != 1 {
			return lockEntry{}, false
		}
		p := doc.Package[0]
		e := lockEntry{name: normalizePyPI(p.Name), version: p.Version, integrity: sdistHash(p.Files), hashes: len(p.Files),
			group: strings.Join(p.Groups, ",")}
		switch {
		case p.Path != "":
			e.source, e.local = p.Path, true
		case p.Git != "":
			e.source = p.Git + "@" + p.Revision
		case p.URL != "":
			e.source = p.URL
		}
		for _, d := range p.Dependencies {
			name, spec := splitPEP508(d)
			e.deps = append(e.deps, lockDep{name: normalizePyPI(name), requirement: spec})
		}
		return e, true
	})
}
