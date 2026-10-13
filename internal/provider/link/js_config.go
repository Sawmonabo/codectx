package link

import (
	"context"
	"io"
	"path"
	"slices"
	"strings"

	"github.com/Sawmonabo/codectx/internal/model"
)

// The configuration an ECMAScript unit's module resolution reads, all of it
// from the pinned snapshot (ADR-0014 decision 5): every tsconfig*.json and
// jsconfig*.json, every package.json, and the workspace declarations
// (package.json `workspaces`, pnpm-workspace.yaml, lerna.json) of the unit.
// Each file is parsed once per build into a small record, one per config or
// package; nothing per source file is held. A config's `extends` is
// flattened in TypeScript's order and a base outside the scanned set is read
// by its path when a config names it.

// esConfigFile reports a file name the ECMAScript resolution reads: a
// TypeScript or JavaScript project config (any tsconfig*.json or
// jsconfig*.json, the names `extends` and `references` conventionally use)
// or a workspace declaration.
func esConfigFile(base string) bool {
	switch base {
	case "package.json", "pnpm-workspace.yaml", "lerna.json":
		return true
	}
	return strings.HasSuffix(base, ".json") && (strings.HasPrefix(base, "tsconfig") || strings.HasPrefix(base, "jsconfig"))
}

// esPackage is one package.json: its directory and the fields module
// resolution reads.
type esPackage struct {
	dir, name string
	// exports and imports are the fields as written, nil when absent.
	exports, imports *jval
	// types is `types`, else `typings`; module and main as written.
	types, module, main string
	// tsconfig is the `tsconfig` field a package-named `extends` reads.
	tsconfig string
	// esm reports `"type": "module"`.
	esm bool
	// workspaces are the `workspaces` globs, the array form or the
	// `packages` of the object form.
	workspaces    []string
	hasWorkspaces bool
	// raw is the canonical spelling of the fields above, a manifest field's
	// value.
	raw string
}

// rawConfig is one config file as written, before `extends` is applied.
type rawConfig struct {
	path string
	v    jval
}

// tsConfig is one config with its `extends` chain applied: every option
// module resolution and ownership read, each path already joined to the
// directory of the config that wrote it.
type tsConfig struct {
	path, dir string
	jsconfig  bool
	// opaque reports an `extends` base that could not be read (a package
	// outside the repository, a missing file): the options it may set are
	// unknown.
	opaque bool

	baseURL string
	// paths is the `paths` object and pathsDir the directory its targets
	// are relative to when no baseUrl is set: that of the config that wrote
	// `paths`.
	paths    *jval
	pathsDir string
	rootDirs []string
	rootDir  string
	// outDir and declDir are outDir and declarationDir, where set.
	outDir, declDir        string
	moduleResolution       string
	module                 string
	customConditions       []string
	exportsFlag, importsFl int8
	allowJs                int8

	files, include, exclude          []string
	hasFiles, hasInclude, hasExclude bool
	// references are the referenced configs' paths, which are not
	// inherited.
	references []string

	// own are the include, files and exclude globs, compiled on first use.
	own *ownership
	// digest is the canonical spelling of everything above, a manifest
	// field's value.
	digest string
}

// ownership is a config's compiled file patterns.
type ownership struct {
	files            map[string]bool
	include, exclude []glob
}

// readFile reads one snapshot file's bytes.
func readFile(ctx context.Context, view model.SnapshotView, fv model.FileVersion) ([]byte, error) {
	rc, fv, err := view.Open(ctx, fv.ID)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(io.LimitReader(rc, fv.Size))
}

// esInputs is every configuration file the unit's resolution reads, parsed.
type esInputs struct {
	view model.SnapshotView
	root string
	raws map[string]*rawConfig
	// missing are the config paths looked for and absent, so a second
	// lookup reads nothing.
	missing map[string]bool
	flat    map[string]*tsConfig
	pkgs    map[string]*esPackage
	// names maps a package name to the directories of the workspace
	// packages declaring it, sorted; two directories are the repository's
	// own ambiguity.
	names map[string][]string
	// declared reports that the unit declares its workspaces, and
	// undeclared then counts the packages left out of names.
	declared bool
	// workspaceFiles are pnpm-workspace.yaml and lerna.json `packages`
	// globs by file, for the manifest fields.
	workspaceFiles map[string][]string
	// dirConfigs are the paths of the configs the scan read, by directory,
	// sorted.
	dirConfigs map[string][]string
}

// readESInputs reads the unit's configuration: one pass over the snapshot's
// manifest selects the files of the unit (under its root and outside every
// nested project the plan gives a unit of its own) whose names
// esConfigFile admits, and each is read and parsed once.
func readESInputs(ctx context.Context, u *unit) (*esInputs, error) {
	in := &esInputs{view: u.content, root: u.root, raws: map[string]*rawConfig{}, missing: map[string]bool{},
		flat: map[string]*tsConfig{}, pkgs: map[string]*esPackage{}, names: map[string][]string{},
		workspaceFiles: map[string][]string{}, dirConfigs: map[string][]string{}}
	if u.content == nil {
		return in, nil
	}
	var found []model.FileVersion
	roots := map[string]bool{}
	err := u.content.EachFile(ctx, model.FileSelection{}, func(fv model.FileVersion) error {
		if fv.Status == model.FileDeleted {
			return nil
		}
		dir, base := path.Split(fv.Path)
		if slices.Contains(projectMarkers[FamilyJavaScript], base) {
			roots[strings.TrimSuffix(dir, "/")] = true
		}
		if within(fv.Path, u.root) && esConfigFile(base) {
			found = append(found, fv)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	own := Unit{Root: u.root}
	units, _ := planFamily(FamilyJavaScript, roots)
	for _, pu := range units {
		if pu.Root == u.root {
			own = pu
		}
	}
	var workspaceGlobs globSet
	for _, fv := range found {
		if !own.Contains(fv.Path) {
			continue
		}
		data, err := readFile(ctx, u.content, fv)
		if err != nil {
			return nil, err
		}
		dir, base := path.Split(fv.Path)
		dir = strings.TrimSuffix(dir, "/")
		switch base {
		case "package.json":
			v, err := parseJSONC(data)
			if err != nil || v.kind != jObject {
				// A package.json that does not parse declares nothing.
				continue
			}
			p := newESPackage(dir, v)
			in.pkgs[dir] = p
			if p.hasWorkspaces {
				in.declared = true
				for _, g := range p.workspaces {
					workspaceGlobs.add(dir, g)
				}
			}
		case "pnpm-workspace.yaml":
			globs := pnpmPackages(data)
			in.declared = true
			in.workspaceFiles[fv.Path] = globs
			for _, g := range globs {
				workspaceGlobs.add(dir, g)
			}
			// The workspace root is always a member.
			workspaceGlobs.add(dir, ".")
		case "lerna.json":
			v, err := parseJSONC(data)
			if err != nil {
				continue
			}
			if globs, ok := v.strings("packages"); ok {
				in.declared = true
				in.workspaceFiles[fv.Path] = globs
				for _, g := range globs {
					workspaceGlobs.add(dir, g)
				}
			}
		default:
			v, err := parseJSONC(data)
			if err != nil || v.kind != jObject {
				in.missing[fv.Path] = true
				continue
			}
			in.raws[fv.Path] = &rawConfig{path: fv.Path, v: v}
			in.dirConfigs[dir] = append(in.dirConfigs[dir], fv.Path)
		}
	}
	for dir, p := range in.pkgs {
		if p.name == "" {
			continue
		}
		if in.declared && !workspaceGlobs.match(dir) && !p.hasWorkspaces {
			continue
		}
		in.names[p.name] = append(in.names[p.name], dir)
	}
	for _, dirs := range in.names {
		slices.Sort(dirs)
	}
	for _, ps := range in.dirConfigs {
		slices.Sort(ps)
	}
	return in, nil
}

// newESPackage reads a package.json object.
func newESPackage(dir string, v jval) *esPackage {
	p := &esPackage{dir: dir, name: v.text("name"), types: cmpOrString(v.text("types"), v.text("typings")),
		module: v.text("module"), main: v.text("main"), tsconfig: v.text("tsconfig"), esm: v.text("type") == "module"}
	if x, ok := v.get("exports"); ok {
		p.exports = &x
	}
	if x, ok := v.get("imports"); ok {
		p.imports = &x
	}
	if w, ok := v.get("workspaces"); ok {
		p.hasWorkspaces = true
		switch w.kind {
		case jArray:
			p.workspaces, _ = v.strings("workspaces")
		case jObject:
			p.workspaces, _ = w.strings("packages")
		}
	}
	var fields jval
	fields.kind = jObject
	for _, k := range []string{"name", "type", "exports", "imports", "types", "typings", "module", "main", "tsconfig", "workspaces"} {
		if x, ok := v.get(k); ok {
			fields.keys = append(fields.keys, k)
			fields.items = append(fields.items, x)
		}
	}
	p.raw = canonicalJSON(fields)
	return p
}

func cmpOrString(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// pnpmPackages is the `packages` sequence of a pnpm-workspace.yaml: its
// scalars, written as a block sequence (`- 'packages/*'`) or a flow sequence
// (`packages: [a, b]`), quoted or plain, with comments. Only the top-level
// `packages` key is read; every other key is pnpm's own settings.
func pnpmPackages(data []byte) []string {
	var out []string
	scalar := func(s string) string {
		s = strings.TrimSpace(s)
		if len(s) >= 2 && (s[0] == '\'' || s[0] == '"') && s[len(s)-1] == s[0] {
			return s[1 : len(s)-1]
		}
		if i := strings.Index(s, " #"); i >= 0 {
			s = strings.TrimSpace(s[:i])
		}
		return s
	}
	in := false
	for line := range strings.SplitSeq(string(data), "\n") {
		line = strings.TrimRight(line, "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		top := line[0] != ' ' && line[0] != '\t' && line[0] != '-'
		if top {
			in = false
			key, rest, ok := strings.Cut(line, ":")
			if !ok || strings.TrimSpace(scalar(key)) != "packages" {
				continue
			}
			rest = strings.TrimSpace(rest)
			if i := strings.Index(rest, "#"); i == 0 {
				rest = ""
			}
			if strings.HasPrefix(rest, "[") {
				body := strings.TrimSuffix(strings.TrimSpace(strings.TrimPrefix(rest, "[")), "]")
				for item := range strings.SplitSeq(body, ",") {
					if s := scalar(item); s != "" {
						out = append(out, s)
					}
				}
				continue
			}
			in = true
			continue
		}
		if in && strings.HasPrefix(trimmed, "-") {
			if s := scalar(strings.TrimPrefix(trimmed, "-")); s != "" {
				out = append(out, s)
			}
		}
	}
	return out
}

// raw is the config file at p, read from the snapshot when the scan did not
// select it (an `extends` base under another name), nil when there is none.
func (in *esInputs) raw(ctx context.Context, p string) (*rawConfig, error) {
	if r, ok := in.raws[p]; ok {
		return r, nil
	}
	if in.missing[p] || in.view == nil || within(p, in.root) && esConfigFile(path.Base(p)) {
		// The scan read every config of the unit under these names.
		return nil, nil
	}
	in.missing[p] = true
	var r *rawConfig
	err := in.view.EachFile(ctx, model.FileSelection{Paths: []string{p}}, func(fv model.FileVersion) error {
		if fv.Status == model.FileDeleted || fv.Path != p {
			return nil
		}
		data, err := readFile(ctx, in.view, fv)
		if err != nil {
			return err
		}
		if v, err := parseJSONC(data); err == nil && v.kind == jObject {
			r = &rawConfig{path: p, v: v}
		}
		return nil
	})
	if err != nil || r == nil {
		return nil, err
	}
	delete(in.missing, p)
	in.raws[p] = r
	return r, nil
}

// config is the config at p with its `extends` chain applied, nil when
// there is none.
func (in *esInputs) config(ctx context.Context, p string) (*tsConfig, error) {
	if c, ok := in.flat[p]; ok {
		return c, nil
	}
	in.flat[p] = nil // a cycle reads the config as absent
	r, err := in.raw(ctx, p)
	if err != nil || r == nil {
		return nil, err
	}
	c := &tsConfig{path: p, dir: path.Dir(p), jsconfig: strings.HasPrefix(path.Base(p), "jsconfig"),
		exportsFlag: -1, importsFl: -1, allowJs: -1}
	if c.dir == "." {
		c.dir = ""
	}
	if err := in.apply(ctx, c, r, map[string]bool{p: true}); err != nil {
		return nil, err
	}
	if refs, ok := r.v.get("references"); ok && refs.kind == jArray {
		for _, ref := range refs.items {
			rp := ref.text("path")
			if rp == "" {
				continue
			}
			target := path.Join(c.dir, rp)
			if !strings.HasSuffix(target, ".json") {
				target = path.Join(target, "tsconfig.json")
			}
			c.references = append(c.references, target)
		}
	}
	c.digest = c.spell()
	in.flat[p] = c
	return c, nil
}

// apply layers r onto c: each `extends` base first, in order, so a later one
// and then r's own settings win, compilerOptions key by key and `files`,
// `include` and `exclude` whole (TypeScript's commandLineParser).
func (in *esInputs) apply(ctx context.Context, c *tsConfig, r *rawConfig, seen map[string]bool) error {
	dir := path.Dir(r.path)
	if dir == "." {
		dir = ""
	}
	bases, _ := r.v.strings("extends")
	for _, spec := range bases {
		bp, ok := in.extendsPath(dir, spec)
		if !ok || seen[bp] {
			c.opaque = c.opaque || !ok
			continue
		}
		br, err := in.raw(ctx, bp)
		if err != nil {
			return err
		}
		if br == nil {
			c.opaque = true
			continue
		}
		seen[bp] = true
		if err := in.apply(ctx, c, br, seen); err != nil {
			return err
		}
		delete(seen, bp)
	}
	join := func(p string) string {
		j := path.Join(dir, p)
		if j == "." {
			return ""
		}
		return j
	}
	if opts, ok := r.v.get("compilerOptions"); ok && opts.kind == jObject {
		if s, ok := opts.get("baseUrl"); ok && s.kind == jString {
			c.baseURL = join(s.str)
		}
		if p, ok := opts.get("paths"); ok && p.kind == jObject {
			c.paths, c.pathsDir = &p, dir
		}
		if dirs, ok := opts.strings("rootDirs"); ok {
			c.rootDirs = c.rootDirs[:0:0]
			for _, d := range dirs {
				c.rootDirs = append(c.rootDirs, join(d))
			}
		}
		if s := opts.text("rootDir"); s != "" {
			c.rootDir = join(s)
		}
		if s := opts.text("outDir"); s != "" {
			c.outDir = join(s)
		}
		if s := opts.text("declarationDir"); s != "" {
			c.declDir = join(s)
		}
		if s := opts.text("moduleResolution"); s != "" {
			c.moduleResolution = strings.ToLower(s)
		}
		if s := opts.text("module"); s != "" {
			c.module = strings.ToLower(s)
		}
		if conds, ok := opts.strings("customConditions"); ok {
			c.customConditions = conds
		}
		if f := opts.flag("resolvePackageJsonExports"); f >= 0 {
			c.exportsFlag = f
		}
		if f := opts.flag("resolvePackageJsonImports"); f >= 0 {
			c.importsFl = f
		}
		if f := opts.flag("allowJs"); f >= 0 {
			c.allowJs = f
		}
	}
	if fs, ok := r.v.strings("files"); ok {
		c.files, c.hasFiles = nil, true
		for _, f := range fs {
			c.files = append(c.files, join(f))
		}
	}
	if is, ok := r.v.strings("include"); ok {
		c.include, c.hasInclude = rebase(dir, is), true
	}
	if es, ok := r.v.strings("exclude"); ok {
		c.exclude, c.hasExclude = rebase(dir, es), true
	}
	return nil
}

// rebase writes each pattern relative to the repository root, keeping its
// wildcards.
func rebase(dir string, patterns []string) []string {
	out := make([]string, 0, len(patterns))
	for _, p := range patterns {
		j := path.Join(dir, strings.TrimPrefix(p, "./"))
		if j == "." {
			j = ""
		}
		out = append(out, j)
	}
	return out
}

// extendsPath is the config file an `extends` entry written in a config of
// directory dir names: a relative or absolute path (".json" added when the
// file as written is absent), or a package name or package subpath resolved
// through the workspace map (the package's `tsconfig` field, else its
// tsconfig.json, for a bare name). It reports false for a package the
// repository does not hold, which is a base the snapshot cannot read.
func (in *esInputs) extendsPath(dir, spec string) (string, bool) {
	relative := strings.HasPrefix(spec, "./") || strings.HasPrefix(spec, "../") || strings.HasPrefix(spec, "/")
	if relative {
		p := path.Join(dir, spec)
		if strings.HasPrefix(spec, "/") {
			p = strings.TrimPrefix(path.Clean(spec), "/")
		}
		if !strings.HasSuffix(p, ".json") {
			if _, ok := in.raws[p]; !ok {
				p += ".json"
			}
		}
		return p, true
	}
	name, sub := splitPackage(spec)
	dirs := in.names[name]
	if len(dirs) != 1 {
		// Absent, or two workspace packages under one name: the base
		// is not one file.
		return "", false
	}
	pkg := in.pkgs[dirs[0]]
	if sub == "" {
		if pkg.tsconfig != "" {
			return path.Join(pkg.dir, pkg.tsconfig), true
		}
		return path.Join(pkg.dir, "tsconfig.json"), true
	}
	p := path.Join(pkg.dir, sub)
	if !strings.HasSuffix(p, ".json") {
		p += ".json"
	}
	return p, true
}

// splitPackage splits a bare specifier into its package name (two
// components for a scoped name) and the subpath after it, without a
// leading slash.
func splitPackage(spec string) (name, sub string) {
	n := 1
	if strings.HasPrefix(spec, "@") {
		n = 2
	}
	parts := strings.SplitN(spec, "/", n+1)
	if len(parts) <= n {
		return spec, ""
	}
	return strings.Join(parts[:n], "/"), parts[n]
}

// spell is c's options in one canonical string.
func (c *tsConfig) spell() string {
	var b strings.Builder
	w := func(k string, vs ...string) {
		b.WriteString(k)
		for _, v := range vs {
			b.WriteByte('\x00')
			b.WriteString(v)
		}
		b.WriteByte('\n')
	}
	flag := func(f int8) string { return string('0' + rune(f+1)) }
	paths := ""
	if c.paths != nil {
		paths = canonicalJSON(*c.paths)
	}
	w("baseUrl", c.baseURL)
	w("paths", paths, c.pathsDir)
	w("rootDirs", c.rootDirs...)
	w("rootDir", c.rootDir)
	w("outDirs", c.outDir, c.declDir)
	w("moduleResolution", c.moduleResolution, c.module)
	w("customConditions", c.customConditions...)
	w("flags", flag(c.exportsFlag), flag(c.importsFl), flag(c.allowJs), flag(boolFlag(c.opaque)))
	w("files", append([]string{flag(boolFlag(c.hasFiles))}, c.files...)...)
	w("include", append([]string{flag(boolFlag(c.hasInclude))}, c.include...)...)
	w("exclude", append([]string{flag(boolFlag(c.hasExclude))}, c.exclude...)...)
	w("references", c.references...)
	return b.String()
}

func boolFlag(b bool) int8 {
	if b {
		return 1
	}
	return 0
}

// outputs are the directories c emits into: outDir and declarationDir.
func (c *tsConfig) outputs() []string {
	var out []string
	for _, d := range []string{c.outDir, c.declDir} {
		if d != "" && !slices.Contains(out, d) {
			out = append(out, d)
		}
	}
	return out
}

// jsFile reports a JavaScript file, which a TypeScript config includes only
// with allowJs (a jsconfig.json always).
func jsFile(p string) bool {
	switch path.Ext(p) {
	case ".js", ".jsx", ".mjs", ".cjs":
		return true
	}
	return false
}

// owns reports whether c's files, include and exclude cover the
// root-relative path p (TypeScript's matchFiles): a file named in `files`,
// or one an include pattern matches and no exclude pattern does, of an
// extension the config compiles. `include` defaults to everything under the
// config's directory unless `files` is set; `exclude` defaults to the
// package folders and the output directories.
func (c *tsConfig) owns(p string) bool {
	if jsFile(p) && !(c.allowJs == 1 || c.jsconfig && c.allowJs != 0) {
		return false
	}
	if c.own == nil {
		o := &ownership{files: map[string]bool{}}
		for _, f := range c.files {
			o.files[f] = true
		}
		include := c.include
		if !c.hasInclude {
			include = nil
			if !c.hasFiles {
				include = []string{path.Join(c.dir, "**/*")}
			}
		}
		for _, g := range include {
			o.include = append(o.include, compileGlob("", g, true, true))
		}
		exclude := c.exclude
		if !c.hasExclude {
			exclude = nil
			for _, d := range []string{"node_modules", "bower_components", "jspm_packages"} {
				exclude = append(exclude, path.Join(c.dir, d))
			}
			if c.outDir != "" {
				exclude = append(exclude, c.outDir)
			}
		}
		for _, g := range exclude {
			o.exclude = append(o.exclude, compileGlob("", g, false, true))
		}
		c.own = o
	}
	if c.own.files[p] {
		return true
	}
	for _, g := range c.own.include {
		if g.match(p) {
			for _, x := range c.own.exclude {
				if x.match(p) {
					return false
				}
			}
			return true
		}
	}
	return false
}
