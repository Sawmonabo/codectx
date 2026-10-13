package manifest

import (
	"bytes"
	"context"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
)

// The JSON lock files are read as a token stream (encoding/json Decoder):
// the reader walks the top-level members with Token and decodes one package
// entry at a time with Decode, so the largest value ever decoded is one
// entry, never the document. Entry spans come from the decoder's input
// offsets, as jsonSpans takes them for package.json.

// npmEntry is one package-lock.json `packages` entry.
type npmEntry struct {
	Name                 string            `json:"name"`
	Version              string            `json:"version"`
	Resolved             string            `json:"resolved"`
	Integrity            string            `json:"integrity"`
	Link                 bool              `json:"link"`
	Dependencies         map[string]string `json:"dependencies"`
	OptionalDependencies map[string]string `json:"optionalDependencies"`
	PeerDependencies     map[string]string `json:"peerDependencies"`
	DevDependencies      map[string]string `json:"devDependencies"`
}

// packageLock reads package-lock.json. Lock versions 2 and 3 key every
// installed package by its node_modules path (`packages`); version 1 nests
// them (`dependencies`, each with `requires` and its own nested
// `dependencies`). A version 2 file carries both; `packages` is read and
// the version 1 tree skipped. A dependency resolves as Node resolves it:
// the nearest node_modules folder up the requiring package's path that
// holds the name.
func (u *unit) packageLock(ctx context.Context) error {
	dec := json.NewDecoder(bytes.NewReader(u.data))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		u.malformed()
		return nil
	}
	var (
		rootName, lockVersion string
		pkgs, tree            npmTree
		havePkgs              bool
	)
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			u.malformed()
			return nil
		}
		key, _ := tok.(string)
		switch key {
		case "name":
			err = dec.Decode(&rootName)
		case "lockfileVersion":
			var v json.Number
			err = dec.Decode(&v)
			lockVersion = v.String()
		case "packages":
			havePkgs = true
			err = u.npmPackages(dec, &pkgs)
		case "dependencies":
			if havePkgs || lockVersion != "" && lockVersion != "1" {
				err = skipValue(dec, 1)
			} else {
				err = u.npmTreeLevel(dec, &tree, "")
			}
		default:
			err = skipValue(dec, 1)
		}
		if err != nil {
			u.malformed()
			return nil
		}
	}
	t := tree
	if havePkgs {
		t = pkgs
	}
	for i := range t.entries {
		if t.keys[i] == "" && t.entries[i].name == "" {
			t.entries[i].name = rootName
		}
	}
	return u.lock(ctx, lockFile{ecosystem: ecosystemNPM, language: languageJS, version: lockVersion, entries: t.entries,
		resolve: t.resolve})
}

// npmTree is the packages of a package-lock.json with the node_modules path
// each is installed at (keys, parallel to entries) and the entry installed
// at each path.
type npmTree struct {
	entries []lockEntry
	keys    []string
	at      map[string]int
}

func (t *npmTree) add(key string, e lockEntry) int {
	if t.at == nil {
		t.at = map[string]int{}
	}
	t.entries = append(t.entries, e)
	t.keys = append(t.keys, key)
	if strings.Contains(key, "node_modules/") {
		t.at[key] = len(t.entries) - 1
	}
	return len(t.entries) - 1
}

// installed is the package and version of the entry at install path key.
func (t *npmTree) installed(key string) (name, version string, ok bool) {
	i, ok := t.at[key]
	if !ok {
		return "", "", false
	}
	return t.entries[i].name, t.entries[i].version, true
}

// npmAlias splits an npm alias specifier, `npm:<name>@<version>`, into the
// package's own name and version; ok is false for any other value, among
// them `npm:<range>`, which names no package (a later yarn's spelling of a
// plain registry range).
func npmAlias(v string) (name, version string, ok bool) {
	rest, ok := strings.CutPrefix(v, "npm:")
	if !ok || strings.IndexByte(rest[min(1, len(rest)):], '@') < 0 {
		return "", "", false
	}
	name, version = yarnSplit(rest)
	return name, version, name != ""
}

// resolve is Node's lookup: from the requiring package's folder, the first
// `node_modules/<name>` up the chain of enclosing package folders, ending at
// the root's. The package found there is named as its entry names it, its
// own name when dep is an alias.
func (t *npmTree) resolve(i int, dep string) (string, string) {
	k := t.keys[i]
	for {
		c := "node_modules/" + dep
		if k != "" {
			c = k + "/node_modules/" + dep
		}
		if name, v, ok := t.installed(c); ok {
			return name, v
		}
		if k == "" {
			return "", ""
		}
		j := strings.LastIndex(k, "node_modules/")
		if j < 0 {
			k = ""
			continue
		}
		k = strings.TrimSuffix(k[:j], "/")
	}
}

// npmPackages reads the version 2 and 3 `packages` object one entry at a
// time. The root ("") and a workspace folder (a path outside node_modules)
// are the repository's own packages; a link entry only points
// node_modules at such a folder, which is listed itself, so it adds nothing.
// An entry under node_modules is the package its folder names unless its
// `name` says otherwise: an aliased package keeps its own name there, and
// the folder's name is the alias.
func (u *unit) npmPackages(dec *json.Decoder, t *npmTree) error {
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return malformedJSON(err)
	}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return malformedJSON(err)
		}
		key, _ := tok.(string)
		start := keyStart(u.data, int(dec.InputOffset()))
		var p npmEntry
		if err := dec.Decode(&p); err != nil {
			return malformedJSON(err)
		}
		if p.Link {
			continue
		}
		e := lockEntry{version: p.Version, integrity: p.Integrity, source: p.Resolved, start: start, end: int(dec.InputOffset())}
		if j := strings.LastIndex(key, "node_modules/"); j >= 0 {
			e.name = key[j+len("node_modules/"):]
			if p.Name != "" && p.Name != e.name {
				e.name, e.alias = p.Name, e.name
			}
		} else {
			e.name, e.local = p.Name, true
			if e.name == "" && key != "" {
				e.name = key[strings.LastIndexByte(key, '/')+1:]
			}
		}
		e.deps = npmDeps(p.Dependencies, KindRuntime, p.OptionalDependencies, KindOptional, p.PeerDependencies, KindPeer,
			p.DevDependencies, KindDev)
		t.add(key, e)
	}
	_, err := dec.Token()
	return err
}

// npmTreeLevel reads one level of the version 1 `dependencies` tree under
// the package installed at parent. An entry's span ends where its nested
// tree begins, so spans stay in file order.
func (u *unit) npmTreeLevel(dec *json.Decoder, t *npmTree, parent string) error {
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return malformedJSON(err)
	}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return malformedJSON(err)
		}
		name, _ := tok.(string)
		key := "node_modules/" + name
		if parent != "" {
			key = parent + "/node_modules/" + name
		}
		start := keyStart(u.data, int(dec.InputOffset()))
		if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
			return malformedJSON(err)
		}
		i := t.add(key, lockEntry{name: name, start: start})
		nested := false
		var requires map[string]string
		for dec.More() {
			tok, err := dec.Token()
			if err != nil {
				return malformedJSON(err)
			}
			field, _ := tok.(string)
			e := &t.entries[i]
			switch field {
			case "version":
				// An aliased package's version is written
				// `npm:<name>@<version>`.
				if err = dec.Decode(&e.version); err == nil {
					if n, v, ok := npmAlias(e.version); ok {
						e.name, e.alias, e.version = n, e.name, v
					}
				}
			case "resolved":
				err = dec.Decode(&e.source)
			case "integrity":
				err = dec.Decode(&e.integrity)
			case "requires":
				err = dec.Decode(&requires)
			case "dependencies":
				nested = true
				t.entries[i].end = int(dec.InputOffset())
				err = u.npmTreeLevel(dec, t, key)
			default:
				err = skipValue(dec, 1+2*strings.Count(key, "node_modules/"))
			}
			if err != nil {
				return err
			}
			if !nested {
				t.entries[i].end = int(dec.InputOffset())
			}
		}
		if _, err := dec.Token(); err != nil {
			return malformedJSON(err)
		}
		t.entries[i].deps = npmDeps(requires, KindRuntime)
	}
	_, err := dec.Token()
	return err
}

// npmDeps lists dependency tables, given as alternating table and kind, in
// name order within each table.
func npmDeps(tables ...any) []lockDep {
	var out []lockDep
	for i := 0; i+1 < len(tables); i += 2 {
		m, _ := tables[i].(map[string]string)
		kind, _ := tables[i+1].(string)
		names := make([]string, 0, len(m))
		for n := range m {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			out = append(out, lockDep{name: n, requirement: m[n], kind: kind})
		}
	}
	return out
}

// pipfileEntry is one Pipfile.lock package.
type pipfileEntry struct {
	Version string   `json:"version"`
	Hashes  []string `json:"hashes"`
	Git     string   `json:"git"`
	Hg      string   `json:"hg"`
	Svn     string   `json:"svn"`
	Bzr     string   `json:"bzr"`
	Ref     string   `json:"ref"`
	Path    string   `json:"path"`
	File    string   `json:"file"`
	Index   string   `json:"index"`
	Edit    bool     `json:"editable"`
}

// pipfileLock reads Pipfile.lock: the `default` and `develop` sections map
// each package to its pinned `==` version and the hashes of its
// distribution files, or to where it comes from: a VCS checkout (a `git`,
// `hg`, `svn` or `bzr` URL and its `ref`), a path or a file. The format
// records no dependency graph.
func (u *unit) pipfileLock(ctx context.Context) error {
	dec := json.NewDecoder(bytes.NewReader(u.data))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		u.malformed()
		return nil
	}
	var entries []lockEntry
	var version string
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			u.malformed()
			return nil
		}
		section, _ := tok.(string)
		switch section {
		case "default", "develop":
			err = u.pipfileSection(dec, section, &entries)
		case "_meta":
			var meta struct {
				Spec int `json:"pipfile-spec"`
			}
			err = dec.Decode(&meta)
			if meta.Spec > 0 {
				version = strconv.Itoa(meta.Spec)
			}
		default:
			err = skipValue(dec, 1)
		}
		if err != nil {
			u.malformed()
			return nil
		}
	}
	return u.lock(ctx, lockFile{ecosystem: ecosystemPyPI, language: languagePython, version: version, entries: entries})
}

func (u *unit) pipfileSection(dec *json.Decoder, section string, entries *[]lockEntry) error {
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return malformedJSON(err)
	}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return malformedJSON(err)
		}
		name, _ := tok.(string)
		start := keyStart(u.data, int(dec.InputOffset()))
		var p pipfileEntry
		if err := dec.Decode(&p); err != nil {
			return malformedJSON(err)
		}
		e := lockEntry{name: normalizePyPI(name), version: strings.TrimLeft(p.Version, "="), hashes: len(p.Hashes), source: p.Index,
			group: section, start: start, end: int(dec.InputOffset())}
		switch vcs, url := vcsSource("git", p.Git, "hg", p.Hg, "svn", p.Svn, "bzr", p.Bzr); {
		case url != "":
			e.source, e.version = vcs+"+"+url, p.Ref
		case p.Path != "":
			e.source, e.local = p.Path, true
		case p.File != "":
			e.source = p.File
		}
		*entries = append(*entries, e)
	}
	_, err := dec.Token()
	return err
}

// bunLock reads bun.lock, a JSON document with comments and trailing commas
// allowed. `packages` keys each installed package by its install path (a
// name, or `parent/name` for a copy nested under a parent) to a tuple: the
// resolved `name@version`, the registry, an object with its dependency
// tables, and its integrity. A workspace or linked package resolves to
// `name@workspace:path` (or `link:`, `file:`, `root:`). A dependency
// resolves as the install paths nest: the requiring package's own nested
// copy first, then each enclosing path's, then the top-level one.
func (u *unit) bunLock(ctx context.Context) error {
	clean := jsoncBlank(u.data)
	dec := json.NewDecoder(bytes.NewReader(clean))
	dec.UseNumber()
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		u.malformed()
		return nil
	}
	var (
		version string
		t       npmTree
		keys    []string
	)
	t.at = map[string]int{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			u.malformed()
			return nil
		}
		key, _ := tok.(string)
		switch key {
		case "lockfileVersion":
			var v json.Number
			err = dec.Decode(&v)
			version = v.String()
		case "packages":
			err = u.bunPackages(dec, clean, &t, &keys)
		default:
			err = skipValue(dec, 1)
		}
		if err != nil {
			u.malformed()
			return nil
		}
	}
	resolve := func(i int, dep string) (string, string) {
		k := keys[i]
		for {
			c := dep
			if k != "" {
				c = k + "/" + dep
			}
			if name, v, ok := t.installed(c); ok {
				return name, v
			}
			if k == "" {
				return "", ""
			}
			k = bunParent(k)
		}
	}
	return u.lock(ctx, lockFile{ecosystem: ecosystemNPM, language: languageJS, version: version, entries: t.entries, resolve: resolve})
}

// bunPackages reads the `packages` object one tuple at a time.
func (u *unit) bunPackages(dec *json.Decoder, data []byte, t *npmTree, keys *[]string) error {
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return malformedJSON(err)
	}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return malformedJSON(err)
		}
		key, _ := tok.(string)
		start := keyStart(data, int(dec.InputOffset()))
		var tuple []json.RawMessage
		if err := dec.Decode(&tuple); err != nil {
			return malformedJSON(err)
		}
		e := lockEntry{name: bunName(key), start: start, end: int(dec.InputOffset())}
		var info npmEntry
		for j, raw := range tuple {
			var s string
			switch {
			case json.Unmarshal(raw, &s) == nil:
				switch {
				case j == 0:
					// The package's own name and version: another name
					// than the install path's is an alias.
					var name string
					if name, e.version = yarnSplit(s); name != "" && name != e.name {
						e.name, e.alias = name, e.name
					}
				case j == 1 && s != "":
					e.source = s
				case j >= 2 && strings.HasPrefix(s, "sha") && strings.Contains(s, "-"):
					e.integrity = s
				case j >= 2:
					// A git package's resolved commit.
					e.source = strings.TrimPrefix(e.source+"#"+s, "#")
				}
			case len(raw) > 0 && raw[0] == '{':
				if err := json.Unmarshal(raw, &info); err != nil {
					return malformedJSON(err)
				}
			}
		}
		for _, p := range []string{"workspace:", "link:", "file:", "root:"} {
			if strings.HasPrefix(e.version, p) {
				e.source, e.version, e.local = e.version, "", true
				break
			}
		}
		e.deps = npmDeps(info.Dependencies, KindRuntime, info.OptionalDependencies, KindOptional, info.PeerDependencies, KindPeer)
		t.entries = append(t.entries, e)
		*keys = append(*keys, key)
		t.at[key] = len(t.entries) - 1
	}
	_, err := dec.Token()
	return err
}

// bunName is the package name an install path ends with: its last
// component, or its last two when the second-to-last is a scope.
func bunName(key string) string {
	i := strings.LastIndexByte(key, '/')
	if i < 0 {
		return key
	}
	if j := strings.LastIndexByte(key[:i], '/'); key[j+1] == '@' {
		return key[j+1:]
	}
	return key[i+1:]
}

// bunParent is the install path a nested path is nested under, "" for a
// top-level one.
func bunParent(key string) string {
	return strings.TrimSuffix(strings.TrimSuffix(key, bunName(key)), "/")
}

// jsoncBlank returns a copy of data with every comment and every trailing
// comma replaced by spaces (a comment's newlines kept), so the result is
// JSON whose every byte offset is the original's.
func jsoncBlank(data []byte) []byte {
	out := bytes.Clone(data)
	for i := 0; i < len(out); i++ {
		switch {
		case out[i] == '"':
			for i++; i < len(out) && out[i] != '"'; i++ {
				if out[i] == '\\' {
					i++
				}
			}
		case out[i] == '/' && i+1 < len(out) && out[i+1] == '/':
			for ; i < len(out) && out[i] != '\n'; i++ {
				out[i] = ' '
			}
		case out[i] == '/' && i+1 < len(out) && out[i+1] == '*':
			j := bytes.Index(out[i+2:], []byte("*/"))
			end := len(out)
			if j >= 0 {
				end = i + 2 + j + 2
			}
			for ; i < end; i++ {
				if out[i] != '\n' {
					out[i] = ' '
				}
			}
			i--
		}
	}
	for i := 0; i < len(out); i++ {
		switch out[i] {
		case '"':
			for i++; i < len(out) && out[i] != '"'; i++ {
				if out[i] == '\\' {
					i++
				}
			}
		case ',':
			j := i + 1
			for j < len(out) && (out[j] == ' ' || out[j] == '\t' || out[j] == '\n' || out[j] == '\r') {
				j++
			}
			if j < len(out) && (out[j] == '}' || out[j] == ']') {
				out[i] = ' '
			}
		}
	}
	return out
}

// vcsSource is the first of the alternating VCS names and URLs whose URL is
// set, as pip spells a VCS requirement's scheme (`hg+<url>`) with its URL.
func vcsSource(kv ...string) (vcs, url string) {
	for i := 0; i+1 < len(kv); i += 2 {
		if kv[i+1] != "" {
			return kv[i], kv[i+1]
		}
	}
	return "", ""
}
