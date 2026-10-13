package link

import (
	"context"
	"path"
	"slices"
	"strings"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
)

// ECMAScript module resolution (ADR-0014 decision 5). An import specifier
// written in a member file resolves the way TypeScript resolves it under
// the file's governing config, reading only the repository's own
// configuration (js_config.go) and the unit's member files:
//
//  1. a relative or absolute path, with TypeScript's extension
//     substitution, a directory's index file where the module mode allows
//     it, and then the config's rootDirs;
//  2. a `#` specifier through the `imports` of the file's package scope;
//  3. the config's `paths` (an exact key, else the longest prefix; each
//     fallback in order, the first that names a member wins);
//  4. the config's baseUrl;
//  5. the package's own name, when it has `exports`;
//  6. a workspace package by name, through its `exports` (conditions in
//     object order) where the mode reads them, else `types`/`typings`,
//     `module`, `main` and `index`;
//  7. otherwise a package outside the repository.
//
// A target that is not a member file (an `exports` entry into a build
// output the snapshot ignores) is mapped back to its source through the
// target package's config, outDir or declarationDir to rootDir; failing
// that, every branch of the entry naming a member is the answer, as a set;
// failing that, the import is an in-repository import that resolves to no
// member, counted and never called external.
//
// Several governing configs (a file two referenced configs include), two
// workspace packages of one name, a mode the configs leave open: each is
// resolved and the union of the targets is the answer, with its size.
//
// Each import's answer is computed once per build, while the hierarchy is
// built, and written to the decls index under keyESSpec, so every reader
// (the import facts, the export resolution, the inference) looks one key
// up, and a refresh re-runs exactly the nodes whose specifiers' answers
// moved (flowGroups).

// keyESSpec + importing file's path + NUL + specifier: one target member's
// module path without extension (Aux), with the import forms whose
// resolution reached it in Flags; an entry with an empty Aux is the answer
// of the forms in Flags that reached no member: esUnresolved, esExternal,
// or neither for a path that names no member.
const keyESSpec = "es\x00"

// The forms of an import and the outcomes a keyESSpec entry carries.
const (
	esStatic  byte = 1 << iota // an import declaration or `export … from`
	esDynamic                  // `import("…")`
	esRequire                  // `require("…")`, `import x = require("…")`
	// esUnresolved: a specifier of the repository (a workspace package,
	// its own package, a `#` import) whose target is no member file.
	esUnresolved
	// esExternal: a package outside the repository.
	esExternal
)

// esForm is the form flag of an import's Syntax.
func esForm(syntax string) byte {
	switch syntax {
	case ImportRequire:
		return esRequire
	case ImportDynamic:
		return esDynamic
	}
	return esStatic
}

// esMode is one TypeScript moduleResolution family.
type esMode uint8

const (
	modeNode10 esMode = iota
	modeNode16
	modeBundler
	// modeClassic is TypeScript's `classic` (js_classic.go).
	modeClassic
)

// esResult is what one specifier resolves to.
type esResult struct {
	targets          []string
	unresolved, extn bool
}

func (r *esResult) merge(o esResult) {
	for _, t := range o.targets {
		if !slices.Contains(r.targets, t) {
			r.targets = append(r.targets, t)
		}
	}
	r.unresolved = r.unresolved || o.unresolved
	r.extn = r.extn || o.extn
}

// esResolver resolves the specifiers of one ECMAScript unit while its
// hierarchy is built: the parsed configuration and an index of the member
// files' module paths, through which every existence probe is one point
// lookup. cur holds the answers of the member being added, which its
// re-exports read.
type esResolver struct {
	in    *esInputs
	bases *index
	cur   map[string]esResult
	curAt string
}

// keyESBase + a member's module path without extension: the member's path
// (Aux), in the resolver's own index.
const keyESBase = "b\x00"

// newESResolver reads the unit's configuration and indexes its members'
// module paths; nil for a unit of another family.
func newESResolver(ctx context.Context, u *unit) (*esResolver, error) {
	if u.family != FamilyJavaScript {
		return nil, nil
	}
	in, err := esInputsOf(ctx, u)
	if err != nil {
		return nil, err
	}
	// The resolver holds the configuration from here; the unit lets it go,
	// so it is released with the hierarchy.
	u.es = nil
	s, err := newEntrySort(u.dir)
	if err != nil {
		return nil, err
	}
	defer s.Close()
	err = u.members(func(fv model.FileVersion) error {
		return s.Add(entry{Key: keyESBase + stripExtension(fv.Path), Aux: fv.Path})
	})
	if err != nil {
		return nil, err
	}
	bases, err := writeIndex(u.dir, s)
	if err != nil {
		return nil, err
	}
	return &esResolver{in: in, bases: bases}, nil
}

func (r *esResolver) close() {
	if r != nil {
		r.bases.close()
	}
}

// add resolves every specifier t's imports and re-exports write and adds
// the answers to s under keyESSpec.
func (r *esResolver) add(ctx context.Context, s *pagination.ExternalSort[entry], t *Table) error {
	if r == nil {
		return nil
	}
	r.cur, r.curAt = map[string]esResult{}, t.Path
	forms := map[string]byte{}
	var order []string
	note := func(spec string, form byte) {
		if _, ok := forms[spec]; !ok {
			order = append(order, spec)
		}
		forms[spec] |= form
	}
	for _, imp := range t.Imports {
		note(imp.Path, esForm(imp.Syntax))
	}
	for _, x := range t.Reexports {
		if x.From != "" {
			note(x.From, esStatic)
		}
	}
	owners, err := r.owners(ctx, t.Path)
	if err != nil {
		return err
	}
	for _, spec := range order {
		var all esResult
		for form := esStatic; form <= esRequire; form <<= 1 {
			if forms[spec]&form == 0 {
				continue
			}
			res, err := r.resolve(ctx, owners, t.Path, spec, form)
			if err != nil {
				return err
			}
			all.merge(res)
			key := keyESSpec + t.Path + "\x00" + spec
			for _, target := range res.targets {
				if err := s.Add(entry{Key: key, Aux: target, Flags: form}); err != nil {
					return err
				}
			}
			if len(res.targets) == 0 {
				switch {
				case res.unresolved:
					err = s.Add(entry{Key: key, Flags: form | esUnresolved})
				case res.extn:
					err = s.Add(entry{Key: key, Flags: form | esExternal})
				default:
					// Answered, by nothing: a path no member is.
					err = s.Add(entry{Key: key, Flags: form})
				}
				if err != nil {
					return err
				}
			}
		}
		r.cur[spec] = all
	}
	return nil
}

// targets is the member module paths spec, written in the member being
// added (add), resolves to; a specifier no import of it writes resolves as
// a relative path alone.
func (r *esResolver) targets(from, spec string) []string {
	if r != nil && r.curAt == from {
		if res, ok := r.cur[spec]; ok {
			return res.targets
		}
	}
	if base, ok := moduleBase(from, spec, false); ok {
		return []string{base}
	}
	return nil
}

// resolve resolves spec, written in the member at from in the given form,
// under each of the file's governing configs (owners) and each module mode
// the config leaves possible, and unions the answers.
func (r *esResolver) resolve(ctx context.Context, owners []*tsConfig, from, spec string, form byte) (esResult, error) {
	var all esResult
	for _, c := range owners {
		for _, mode := range modesOf(c) {
			res, err := r.resolveUnder(ctx, from, spec, form, c, mode)
			if err != nil {
				return esResult{}, err
			}
			all.merge(res)
		}
	}
	if len(all.targets) > 0 {
		all.unresolved, all.extn = false, false
	}
	if all.unresolved {
		all.extn = false
	}
	slices.Sort(all.targets)
	return all, nil
}

// owners are the configs governing the member at from (TypeScript's editor rule):
// walking up from its directory to the unit root, the first tsconfig.json or
// jsconfig.json that includes it, or failing that one its references reach
// (a solution-style config), every such referenced config when several do;
// with no config including it, the nearest config by directory (a
// bundler's rule); with none at all, no config (a nil element).
func (r *esResolver) owners(ctx context.Context, from string) ([]*tsConfig, error) {
	var nearest *tsConfig
	for d := path.Dir(from); ; d = path.Dir(d) {
		if d == "." {
			d = ""
		}
		if within(d, r.in.root) {
			for _, name := range []string{"tsconfig.json", "jsconfig.json"} {
				c, err := r.in.config(ctx, path.Join(d, name))
				if err != nil {
					return nil, err
				}
				if c == nil {
					continue
				}
				if nearest == nil {
					nearest = c
				}
				owners, err := r.owning(ctx, c, from, map[string]bool{})
				if err != nil {
					return nil, err
				}
				if len(owners) > 0 {
					return owners, nil
				}
			}
		}
		if d == "" || d == r.in.root {
			break
		}
	}
	return []*tsConfig{nearest}, nil
}

// owning is c when it includes from, else every config its references reach
// transitively that does.
func (r *esResolver) owning(ctx context.Context, c *tsConfig, from string, seen map[string]bool) ([]*tsConfig, error) {
	if seen[c.path] {
		return nil, nil
	}
	seen[c.path] = true
	if c.owns(from) {
		return []*tsConfig{c}, nil
	}
	var out []*tsConfig
	for _, ref := range c.references {
		rc, err := r.in.config(ctx, ref)
		if err != nil {
			return nil, err
		}
		if rc == nil {
			continue
		}
		got, err := r.owning(ctx, rc, from, seen)
		if err != nil {
			return nil, err
		}
		out = append(out, got...)
	}
	return out, nil
}

// modesOf is the moduleResolution c selects (TypeScript's defaults: Classic
// for none, AMD, UMD and System output, Node10 for CommonJS output, Node16
// for node16/nodenext, Bundler for preserve); when c sets none it can be
// read from, or there is no config, both module modes a current project may
// compile under, Bundler and Node16.
func modesOf(c *tsConfig) []esMode {
	if c == nil {
		return []esMode{modeBundler, modeNode16}
	}
	switch c.moduleResolution {
	case "classic":
		return []esMode{modeClassic}
	case "node", "node10":
		return []esMode{modeNode10}
	case "node16", "nodenext":
		return []esMode{modeNode16}
	case "bundler":
		return []esMode{modeBundler}
	}
	switch c.module {
	case "none", "amd", "umd", "system":
		// TypeScript 5.9 and 6.0 both default these to classic; 7.0
		// accepts none of them.
		return []esMode{modeClassic}
	case "commonjs":
		return []esMode{modeNode10}
	case "node16", "node18", "node20", "nodenext":
		return []esMode{modeNode16}
	case "preserve":
		return []esMode{modeBundler}
	}
	return []esMode{modeBundler, modeNode16}
}

// readsExports reports whether mode reads package.json `exports` (and the
// package's own name), unless c says otherwise.
func readsExports(c *tsConfig, mode esMode) bool {
	if c != nil && c.exportsFlag >= 0 {
		return c.exportsFlag == 1
	}
	return mode != modeNode10
}

// readsImports is readsExports for `imports`.
func readsImports(c *tsConfig, mode esMode) bool {
	if c != nil && c.importsFl >= 0 {
		return c.importsFl == 1
	}
	return mode != modeNode10
}

// importCondition is "import" or "require": the condition an import of the
// given form, in the member at from, matches under mode. Node16 follows the
// file's module format (an import declaration in a CommonJS file is a
// require); Bundler the syntax.
func (r *esResolver) importCondition(from string, form byte, mode esMode) string {
	switch {
	case form == esRequire:
		return "require"
	case form == esDynamic || mode != modeNode16:
		return "import"
	case r.esmFile(from):
		return "import"
	}
	return "require"
}

// esmFile reports an ES module under Node's rules: .mts/.mjs always,
// .cts/.cjs never, any other file when its package scope says
// `"type": "module"`.
func (r *esResolver) esmFile(from string) bool {
	switch {
	case strings.HasSuffix(from, ".mts"), strings.HasSuffix(from, ".mjs"):
		return true
	case strings.HasSuffix(from, ".cts"), strings.HasSuffix(from, ".cjs"):
		return false
	}
	p := r.scope(from)
	return p != nil && p.esm
}

// scope is the package scope of the member at from: the nearest
// package.json of the unit at or above its directory.
func (r *esResolver) scope(from string) *esPackage {
	for d := path.Dir(from); ; d = path.Dir(d) {
		if d == "." {
			d = ""
		}
		if p, ok := r.in.pkgs[d]; ok {
			return p
		}
		if d == "" {
			return nil
		}
	}
}

// conditions is the condition set of an `exports` or `imports` lookup.
func (r *esResolver) conditions(from string, form byte, c *tsConfig, mode esMode) map[string]bool {
	conds := map[string]bool{"types": true, "default": true, r.importCondition(from, form, mode): true}
	if mode == modeNode16 {
		conds["node"] = true
	}
	if c != nil && mode != modeNode10 {
		for _, k := range c.customConditions {
			conds[k] = true
		}
	}
	return conds
}

// strictESM reports a lookup that neither drops an extension nor reads a
// directory's index file: an ES module's import under Node16.
func (r *esResolver) strictESM(from string, form byte, mode esMode) bool {
	return mode == modeNode16 && r.importCondition(from, form, mode) == "import"
}

// resolveUnder is one resolution of spec under config c and mode (the order
// above).
func (r *esResolver) resolveUnder(ctx context.Context, from, spec string, form byte, c *tsConfig, mode esMode) (esResult, error) {
	if mode == modeClassic {
		return r.resolveClassic(from, spec, c)
	}
	strict := r.strictESM(from, form, mode)
	if esRelative(spec) {
		p := esJoin(from, spec)
		got, err := r.probe(p, strict)
		if err != nil || got != "" {
			return one(got), err
		}
		for _, alt := range rootDirCandidates(c, p) {
			got, err := r.probe(alt, strict)
			if err != nil || got != "" {
				return one(got), err
			}
		}
		return esResult{}, nil
	}
	if strings.HasPrefix(spec, "#") {
		p := r.scope(from)
		if p == nil || p.imports == nil || !readsImports(c, mode) {
			return esResult{}, nil
		}
		return r.packageMap(ctx, p, *p.imports, spec, from, form, c, mode, true)
	}
	matched := false
	if c != nil && c.paths != nil {
		var targets []string
		var star string
		if targets, star, matched = matchPaths(*c.paths, spec); matched {
			for _, t := range targets {
				got, err := r.probe(path.Join(c.pathsRoot(), strings.Replace(t, "*", star, 1)), strict)
				if err != nil || got != "" {
					return one(got), err
				}
			}
		}
	}
	// A `paths` key that matches decides the settings' answer, found or
	// not: the baseUrl is read only when none does
	// (tryLoadModuleUsingOptionalResolutionSettings).
	if c != nil && c.baseURL != "" && !matched {
		got, err := r.probe(path.Join(c.baseURL, spec), strict)
		if err != nil || got != "" {
			return one(got), err
		}
	}
	name, sub := splitPackage(spec)
	if self := r.scope(from); self != nil && self.name == name && self.exports != nil && readsExports(c, mode) {
		return r.packageMap(ctx, self, *self.exports, exportKey(sub), from, form, c, mode, false)
	}
	dirs := r.in.names[name]
	if len(dirs) == 0 {
		return esResult{extn: true}, nil
	}
	var all esResult
	for _, d := range dirs {
		res, err := r.inPackage(ctx, r.in.pkgs[d], sub, from, form, c, mode, strict)
		if err != nil {
			return esResult{}, err
		}
		all.merge(res)
	}
	if len(all.targets) == 0 {
		all.unresolved = true
	}
	return all, nil
}

// esRelative reports a specifier TypeScript reads as a path
// (isExternalModuleNameRelative): relative, or rooted.
func esRelative(spec string) bool {
	return strings.HasPrefix(spec, "./") || strings.HasPrefix(spec, "../") || strings.HasPrefix(spec, "/") || spec == "." || spec == ".."
}

// esJoin is the repository path a relative or rooted specifier written in
// the member at from names.
func esJoin(from, spec string) string {
	if strings.HasPrefix(spec, "/") {
		return strings.TrimPrefix(path.Clean(spec), "/")
	}
	return path.Join(path.Dir(from), spec)
}

// rootDirCandidates is where c's rootDirs send the relative path p besides
// p itself (tryLoadModuleUsingRootDirs): the rootDir that is p's longest
// matching prefix decides the suffix, which is tried under every other
// rootDir in order.
func rootDirCandidates(c *tsConfig, p string) []string {
	if c == nil {
		return nil
	}
	best := -1
	for i, rd := range c.rootDirs {
		if within(p, rd) && p != rd && (best < 0 || len(rd) > len(c.rootDirs[best])) {
			best = i
		}
	}
	if best < 0 {
		return nil
	}
	rel := strings.TrimPrefix(p, c.rootDirs[best])
	rel = strings.TrimPrefix(rel, "/")
	var out []string
	for i, rd := range c.rootDirs {
		if i != best && rd != c.rootDirs[best] {
			out = append(out, path.Join(rd, rel))
		}
	}
	return out
}

// pathsRoot is the directory c's `paths` targets are relative to: the
// baseUrl, else the directory of the config that wrote `paths`.
func (c *tsConfig) pathsRoot() string {
	if c.baseURL != "" {
		return c.baseURL
	}
	return c.pathsDir
}

func one(target string) esResult {
	if target == "" {
		return esResult{}
	}
	return esResult{targets: []string{target}}
}

// exportKey is the `exports` key a package subpath is looked up under.
func exportKey(sub string) string {
	if sub == "" {
		return "."
	}
	return "./" + sub
}

// inPackage resolves the subpath sub of the workspace package p: through
// its `exports` when the mode reads them and it has some, else its entry
// fields (types, module, main, then index) for the package itself and the
// subpath as a path inside the package.
func (r *esResolver) inPackage(ctx context.Context, p *esPackage, sub, from string, form byte, c *tsConfig, mode esMode, strict bool) (esResult, error) {
	if p.exports != nil && readsExports(c, mode) {
		return r.packageMap(ctx, p, *p.exports, exportKey(sub), from, form, c, mode, false)
	}
	if sub != "" {
		got, err := r.target(ctx, p, sub, strict)
		return esResult{targets: got}, err
	}
	for _, f := range []string{p.types, p.module, p.main} {
		if f == "" {
			continue
		}
		got, err := r.target(ctx, p, f, false)
		if err != nil || len(got) > 0 {
			return esResult{targets: got}, err
		}
	}
	got, err := r.probe(path.Join(p.dir, "index"), false)
	return one(got), err
}

// packageMap resolves key through the package's `exports` (or, for
// imports, its `imports`) map m: the entry the key matches, its target under
// the lookup's conditions (fallbacks in order, the first naming a member
// wins), and when no selected target names a member, every branch of the
// entry that does, as a set. An `imports` target that is a bare specifier
// resolves as a package.
func (r *esResolver) packageMap(ctx context.Context, p *esPackage, m jval, key, from string, form byte, c *tsConfig, mode esMode, imports bool) (esResult, error) {
	entry, star, ok := matchExports(m, key, imports)
	if !ok {
		return esResult{unresolved: !imports}, nil
	}
	conds := r.conditions(from, form, c, mode)
	selected, blocked := pickTargets(entry, conds)
	if blocked {
		return esResult{unresolved: !imports}, nil
	}
	resolveTarget := func(t string) (esResult, error) {
		t = strings.ReplaceAll(t, "*", star)
		switch {
		case imports && strings.HasPrefix(t, "#"):
			// A target may not be another `#` specifier.
			return esResult{}, nil
		case imports && !strings.HasPrefix(t, "./"):
			return r.resolveUnder(ctx, from, t, form, c, mode)
		}
		got, err := r.target(ctx, p, t, false)
		return esResult{targets: got}, err
	}
	for _, t := range selected {
		res, err := resolveTarget(t)
		if err != nil || len(res.targets) > 0 || res.extn {
			return res, err
		}
	}
	var all esResult
	for _, t := range allTargets(entry) {
		res, err := resolveTarget(t)
		if err != nil {
			return esResult{}, err
		}
		all.merge(esResult{targets: res.targets})
	}
	if len(all.targets) == 0 {
		all.unresolved = true
	}
	return all, nil
}

// target is the member a path written in package p's manifest names (an
// `exports` target, `main`): the member itself, else, when it lies in an
// output directory of a config of the nearest directory at or above it
// whose configs emit there, the source file TypeScript maps it back to
// (outDir or declarationDir replaced by rootDir, the output extension by
// the source one); several such configs mapping it to different members
// are a set.
func (r *esResolver) target(ctx context.Context, p *esPackage, t string, strict bool) ([]string, error) {
	full := path.Join(p.dir, t)
	got, err := r.probe(full, strict)
	if err != nil || got != "" {
		return nonEmpty(got), err
	}
	for d := path.Dir(full); ; d = path.Dir(d) {
		if d == "." {
			d = ""
		}
		if !within(d, r.in.root) {
			return nil, nil
		}
		var out []string
		emits := false
		for _, cp := range r.in.dirConfigs[d] {
			c, err := r.in.config(ctx, cp)
			if err != nil {
				return nil, err
			}
			if c == nil {
				continue
			}
			for _, o := range c.outputs() {
				rel, ok := strings.CutPrefix(full, o+"/")
				if !ok {
					continue
				}
				emits = true
				for _, root := range c.sourceRoots() {
					got, err := r.probe(path.Join(root, rel), false)
					if err != nil {
						return nil, err
					}
					if got != "" {
						if !slices.Contains(out, got) {
							out = append(out, got)
						}
						break
					}
				}
			}
		}
		if emits {
			slices.Sort(out)
			return out, nil
		}
		if d == "" {
			return nil, nil
		}
	}
}

func nonEmpty(s string) []string {
	if s == "" {
		return nil
	}
	return []string{s}
}

// sourceRoots are the directories c's sources are emitted from: rootDir
// when set, else both the config's own directory (TypeScript 6's default)
// and the longest common directory of its include and files entries (the
// common root of the inputs, earlier versions' default).
func (c *tsConfig) sourceRoots() []string {
	if c.rootDir != "" {
		return []string{c.rootDir}
	}
	out := []string{c.dir}
	var common []string
	first := true
	for _, p := range append(slices.Clone(c.include), c.files...) {
		var lit []string
		for _, seg := range strings.Split(p, "/") {
			if strings.ContainsAny(seg, "*?") {
				break
			}
			lit = append(lit, seg)
		}
		if slices.Contains(c.files, p) || len(lit) > 0 && strings.Contains(lit[len(lit)-1], ".") {
			lit = lit[:max(len(lit)-1, 0)]
		}
		if first {
			common, first = lit, false
			continue
		}
		n := 0
		for n < len(common) && n < len(lit) && common[n] == lit[n] {
			n++
		}
		common = common[:n]
	}
	if root := strings.Join(common, "/"); !first && root != c.dir {
		out = append(out, root)
	}
	return out
}

// probe is the member module path the path p names, "" when none does: p
// itself, with any extension TypeScript substitutes; and, unless strict,
// the index file of the directory p.
func (r *esResolver) probe(p string, strict bool) (string, error) {
	base := stripExtension(p)
	if strict && base == p {
		// An ES module's import names its file with its extension.
		return "", nil
	}
	candidates := []string{base}
	if !strict {
		candidates = append(candidates, path.Join(p, "index"))
	}
	for _, b := range candidates {
		found := false
		if err := r.bases.lookup(keyESBase+b, func(entry) error {
			found = true
			return nil
		}); err != nil {
			return "", err
		}
		if found {
			return b, nil
		}
	}
	return "", nil
}

// matchPaths is the targets of the `paths` key spec matches, an exact key
// first, else the pattern with the longest prefix (the first written on a
// tie), with the text its `*` matched (TypeScript's matchPatternOrExact).
func matchPaths(paths jval, spec string) (targets []string, star string, ok bool) {
	best := -1
	for i, k := range paths.keys {
		if k == spec && !strings.Contains(k, "*") {
			best, star = i, ""
			break
		}
		pre, suf, wild := strings.Cut(k, "*")
		if !wild || strings.Contains(suf, "*") || len(spec) < len(pre)+len(suf) ||
			!strings.HasPrefix(spec, pre) || !strings.HasSuffix(spec, suf) {
			continue
		}
		if best < 0 || len(pre) > len(strings.SplitN(paths.keys[best], "*", 2)[0]) {
			best, star = i, spec[len(pre):len(spec)-len(suf)]
		}
	}
	if best < 0 {
		return nil, "", false
	}
	v := paths.items[best]
	switch v.kind {
	case jString:
		return []string{v.str}, star, true
	case jArray:
		for _, x := range v.items {
			if x.kind == jString {
				targets = append(targets, x.str)
			}
		}
	}
	return targets, star, true
}

// matchExports is the entry of an `exports` or `imports` map key selects
// (Node's PACKAGE_EXPORTS_RESOLVE and PACKAGE_IMPORTS_RESOLVE): exports
// written as a string, an array or a conditions object are the entry of
// ".", an exact key wins, else the single-`*` pattern key with the longest
// prefix, then the longest key (PATTERN_KEY_COMPARE), with what its `*`
// matched.
func matchExports(m jval, key string, imports bool) (entry jval, star string, ok bool) {
	if !imports {
		sugar := m.kind != jObject
		if m.kind == jObject {
			sugar = !slices.ContainsFunc(m.keys, func(k string) bool { return strings.HasPrefix(k, ".") })
		}
		if sugar {
			if key == "." {
				return m, "", true
			}
			return jval{}, "", false
		}
	}
	if m.kind != jObject {
		return jval{}, "", false
	}
	if v, found := m.get(key); found && !strings.Contains(key, "*") {
		return v, "", true
	}
	bestKey := ""
	for i, k := range m.keys {
		pre, suf, wild := strings.Cut(k, "*")
		if !wild || strings.Contains(suf, "*") || !strings.HasPrefix(key, pre) || key == pre ||
			!strings.HasSuffix(key, suf) || len(key) < len(pre)+len(suf) {
			continue
		}
		if !ok || patternKeyLess(k, bestKey) {
			entry, star, ok, bestKey = m.items[i], key[len(pre):len(key)-len(suf)], true, k
		}
	}
	return entry, star, ok
}

// patternKeyLess reports that pattern key a sorts before b under
// PATTERN_KEY_COMPARE: the longer prefix before `*`, then the longer key.
func patternKeyLess(a, b string) bool {
	pa, pb := strings.Index(a, "*"), strings.Index(b, "*")
	if pa != pb {
		return pa > pb
	}
	return len(a) > len(b)
}

// pickTargets is the target paths of an `exports` entry under conds, in
// fallback order: a string is itself, an array its elements' targets in
// order, a conditions object the targets of its first key, in object order,
// that conds holds and that yields any. blocked reports a null target,
// which excludes the subpath.
func pickTargets(v jval, conds map[string]bool) (out []string, blocked bool) {
	switch v.kind {
	case jNull:
		return nil, true
	case jString:
		return []string{v.str}, false
	case jArray:
		for _, x := range v.items {
			got, _ := pickTargets(x, conds)
			out = append(out, got...)
		}
		return out, false
	case jObject:
		for i, k := range v.keys {
			if !conds[k] {
				continue
			}
			got, blocked := pickTargets(v.items[i], conds)
			if blocked {
				return nil, true
			}
			if len(got) > 0 {
				return got, false
			}
		}
	}
	return nil, false
}

// allTargets is every target path an `exports` entry names under any
// condition, in the order written.
func allTargets(v jval) []string {
	switch v.kind {
	case jString:
		return []string{v.str}
	case jArray, jObject:
		var out []string
		for _, x := range v.items {
			out = append(out, allTargets(x)...)
		}
		return out
	}
	return nil
}

// esModules yields the member module paths spec, written in the member at
// from, resolves to (keyESSpec), under any import form; a specifier no
// import of the file writes resolves as a relative path alone.
func (h *hierarchy) esModules(from, spec string, yield func(base string) error) error {
	any := false
	var targets []string
	err := h.decls.lookup(keyESSpec+from+"\x00"+spec, func(e entry) error {
		any = true
		if e.Aux != "" && !slices.Contains(targets, e.Aux) {
			targets = append(targets, e.Aux)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if !any {
		if base, ok := moduleBase(from, spec, false); ok {
			targets = append(targets, base)
		}
	}
	for _, t := range targets {
		if err := yield(t); err != nil {
			return err
		}
	}
	return nil
}

// esOutside reports whether spec, written in the member at from, names a
// package outside the repository under every form that reached an answer:
// a call through its module is an external call. A relative specifier, or
// one the repository's configuration resolves, is never outside.
func (h *hierarchy) esOutside(from, spec string) (bool, error) {
	outside, inside := false, false
	err := h.decls.lookup(keyESSpec+from+"\x00"+spec, func(e entry) error {
		if e.Flags&esExternal != 0 {
			outside = true
		} else {
			inside = true
		}
		return nil
	})
	if err != nil || inside {
		return false, err
	}
	if !outside {
		// A specifier no import of the file writes: a package unless it
		// is a path.
		_, rel := moduleBase(from, spec, false)
		return !rel && !strings.HasPrefix(spec, "/"), nil
	}
	return true, nil
}

// modules yields the member module paths the import spec written in fc's
// file names (modulesAt).
func (h *hierarchy) modules(fc fileCtx, spec string, yield func(base string) error) error {
	return h.modulesAt(fc.path, fc.family == "python", spec, yield)
}

// modulesAt yields the module paths an import spec written in the member at
// from names: Python's dotted module path as a path (moduleBase), an
// ECMAScript specifier's resolution (esModules).
func (h *hierarchy) modulesAt(from string, python bool, spec string, yield func(base string) error) error {
	if python {
		if base, ok := moduleBase(from, spec, true); ok {
			return yield(base)
		}
		return nil
	}
	return h.esModules(from, spec, yield)
}
