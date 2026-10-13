package link

import (
	"context"
	"slices"
	"testing"
)

// mustJSONC parses s or fails the test.
func mustJSONC(t *testing.T, s string) jval {
	t.Helper()
	v, err := parseJSONC([]byte(s))
	if err != nil {
		t.Fatalf("parseJSONC(%q): %v", s, err)
	}
	return v
}

// TestParseJSONC pins TypeScript's tolerance of comments and trailing
// commas in a config, and that bytes inside strings are left alone.
func TestParseJSONC(t *testing.T) {
	v := mustJSONC(t, "\xef\xbb\xbf{\n  // a comment, with a comma,\n  \"a\": \"x // not a comment, }\", /* block\n */\n  \"b\": [1, 2,],\n}\n")
	if got := v.text("a"); got != "x // not a comment, }" {
		// Mutation: strip `//` inside a string -> a is "x ".
		t.Fatalf("a = %q", got)
	}
	if b, _ := v.get("b"); len(b.items) != 2 {
		// Mutation: keep the trailing comma -> the decode fails.
		t.Fatalf("b = %+v", b)
	}
	o := mustJSONC(t, `{"z": 1, "a": 2, "m": 3}`)
	if !slices.Equal(o.keys, []string{"z", "a", "m"}) {
		// Mutation: decode objects into a map -> the condition order is lost.
		t.Fatalf("keys = %q", o.keys)
	}
}

// TestGlob pins TypeScript's include semantics and the package managers'
// workspace globs.
func TestGlob(t *testing.T) {
	cases := []struct {
		dir, pattern string
		files        bool
		implicit     bool
		path         string
		want         bool
		mutation     string
	}{
		{"", "src/*.ts", true, false, "src/a/b.ts", false, "`*` crossing a slash matches src/a/b.ts"},
		{"", "src/**/*.ts", true, false, "src/a/b.ts", true, "`**` matching one component only misses src/a/b.ts"},
		{"", "src/**/*.ts", true, false, "src/b.ts", true, "`**` requiring a component misses src/b.ts"},
		{"", "src/**/*", true, false, "src/.hidden/b.ts", false, "a wildcard entering a dot directory includes src/.hidden"},
		{"", "**/*", true, false, "node_modules/p/i.ts", false, "a wildcard entering node_modules includes a dependency"},
		{"pkg", "src", true, true, "pkg/src/x/y.ts", true, "a pattern with no extension not read as a directory misses pkg/src/x/y.ts"},
		{"pkg", "src/a?.ts", true, false, "pkg/src/ab.ts", true, "`?` not matching one character misses ab.ts"},
		{"", "packages/*", false, false, "packages/a", true, "a workspace glob not matched against the directory misses packages/a"},
		{"", "packages/*", false, false, "packages/a/b", false, "a workspace `*` crossing a slash admits packages/a/b"},
	}
	for _, c := range cases {
		g := compileGlob(c.dir, c.pattern, c.files, c.implicit)
		if got := g.match(c.path); got != c.want {
			t.Errorf("%s/%s matching %s = %v (mutation: %s)", c.dir, c.pattern, c.path, got, c.mutation)
		}
	}
	var s globSet
	s.add("", "packages/*")
	s.add("", "!packages/legacy")
	if s.match("packages/legacy") || !s.match("packages/a") {
		// Mutation: ignore a negated pattern -> packages/legacy is a member.
		t.Fatal("a negated workspace glob does not exclude its directory")
	}
}

// TestPnpmPackages pins the block and flow forms of pnpm-workspace.yaml's
// packages list.
func TestPnpmPackages(t *testing.T) {
	block := "packages:\n  # comment\n  - 'packages/*'\n  - \"apps/**\"\n  - '!**/test/**'\ncatalog:\n  - nope\n"
	if got := pnpmPackages([]byte(block)); !slices.Equal(got, []string{"packages/*", "apps/**", "!**/test/**"}) {
		// Mutation: keep reading items after the next top-level key -> "nope" is a glob.
		t.Fatalf("block = %q", got)
	}
	if got := pnpmPackages([]byte("packages: ['a/*', b]\n")); !slices.Equal(got, []string{"a/*", "b"}) {
		// Mutation: read only the block form -> the flow sequence yields nothing.
		t.Fatalf("flow = %q", got)
	}
}

// TestMatchPaths pins TypeScript's paths matching: an exact key first, then
// the longest prefix.
func TestMatchPaths(t *testing.T) {
	paths := mustJSONC(t, `{"*": ["any/*"], "@/*": ["src/*"], "@/lib/*": ["lib/*", "fallback/*"], "@/exact": ["e.ts"]}`)
	targets, star, ok := matchPaths(paths, "@/lib/x")
	if !ok || star != "x" || !slices.Equal(targets, []string{"lib/*", "fallback/*"}) {
		// Mutation: take the first matching key -> "*" wins with star "@/lib/x".
		t.Fatalf("@/lib/x -> %q %q %v", targets, star, ok)
	}
	if targets, _, _ := matchPaths(paths, "@/exact"); !slices.Equal(targets, []string{"e.ts"}) {
		// Mutation: no exact-key preference -> "@/*" wins.
		t.Fatalf("@/exact -> %q", targets)
	}
}

// TestMatchExports pins Node's exports entry selection.
func TestMatchExports(t *testing.T) {
	m := mustJSONC(t, `{".": "./i.js", "./*": "./all/*.js", "./feat/*": "./feat/*.js", "./feat/x": "./x.js"}`)
	for _, c := range []struct {
		key, want, star, mutation string
	}{
		{"./feat/x", "./x.js", "", "patterns before the exact key -> ./feat/* wins"},
		{"./feat/y", "./feat/*.js", "y", "PATTERN_KEY_COMPARE ignored -> ./* wins"},
		{"./z", "./all/*.js", "z", "a single-star key not matched -> ./z unexported"},
	} {
		e, star, ok := matchExports(m, c.key, false)
		if !ok || e.str != c.want || star != c.star {
			t.Errorf("%s -> %q %q %v (mutation: %s)", c.key, e.str, star, ok, c.mutation)
		}
	}
	sugar := mustJSONC(t, `{"import": "./e.js", "require": "./c.js"}`)
	if e, _, ok := matchExports(sugar, ".", false); !ok || e.kind != jObject {
		// Mutation: read a conditions object as a subpath map -> "." is unexported.
		t.Fatalf("sugar . -> %+v %v", e, ok)
	}
}

// TestPickTargets pins conditional exports: the first key, in object order,
// the conditions hold, and a null target blocking the subpath.
func TestPickTargets(t *testing.T) {
	v := mustJSONC(t, `{"browser": "./b.js", "require": {"node": "./n.cjs", "default": "./d.cjs"}, "import": "./e.js", "default": "./x.js"}`)
	got, blocked := pickTargets(v, map[string]bool{"import": true, "default": true, "types": true})
	if blocked || !slices.Equal(got, []string{"./e.js"}) {
		// Mutation: match conditions in the condition set's order, or take an unenabled one -> ./b.js or ./x.js.
		t.Fatalf("import -> %q %v", got, blocked)
	}
	got, _ = pickTargets(v, map[string]bool{"require": true, "default": true})
	if !slices.Equal(got, []string{"./d.cjs"}) {
		// Mutation: not descending into nested conditions -> no target or ./n.cjs.
		t.Fatalf("require -> %q", got)
	}
	if _, blocked := pickTargets(mustJSONC(t, `{"default": null}`), map[string]bool{"default": true}); !blocked {
		// Mutation: a null target read as no target -> the subpath falls through to other branches.
		t.Fatal("a null target does not block the subpath")
	}
}

// testInputs is an esInputs over the given config files, with no view.
func testInputs(t *testing.T, files map[string]string) *esInputs {
	t.Helper()
	in := &esInputs{raws: map[string]*rawConfig{}, missing: map[string]bool{}, flat: map[string]*tsConfig{},
		pkgs: map[string]*esPackage{}, names: map[string][]string{}, dirConfigs: map[string][]string{}}
	for p, s := range files {
		in.raws[p] = &rawConfig{path: p, v: mustJSONC(t, s)}
	}
	return in
}

// TestConfigExtends pins TypeScript's extends flattening: bases in order,
// the later winning key by key, include replaced whole, paths relative to
// the config that wrote them, references never inherited.
func TestConfigExtends(t *testing.T) {
	in := testInputs(t, map[string]string{
		"base/one.json": `{"compilerOptions": {"moduleResolution": "node10", "paths": {"@/*": ["src/*"]}, "outDir": "out"},
			"include": ["src"], "references": [{"path": "../other"}]}`,
		"base/two.json":     `{"compilerOptions": {"moduleResolution": "bundler"}}`,
		"app/tsconfig.json": `{"extends": ["../base/one.json", "../base/two.json"], "include": ["lib"]}`,
	})
	c, err := in.config(context.Background(), "app/tsconfig.json")
	if err != nil || c == nil {
		t.Fatalf("config: %v %v", c, err)
	}
	if c.moduleResolution != "bundler" {
		// Mutation: apply the extends array in reverse -> node10 wins.
		t.Errorf("moduleResolution = %q", c.moduleResolution)
	}
	if c.pathsDir != "base" || c.outDir != "base/out" {
		// Mutation: rebase an inherited path to the inheriting config -> app, app/out.
		t.Errorf("pathsDir = %q, outDir = %q", c.pathsDir, c.outDir)
	}
	if !slices.Equal(c.include, []string{"app/lib"}) {
		// Mutation: merge include with the base's -> base/src is included too.
		t.Errorf("include = %q", c.include)
	}
	if len(c.references) != 0 {
		// Mutation: inherit references -> the base's reference is the app's.
		t.Errorf("references = %q", c.references)
	}
}

// TestOwners pins TypeScript's editor governing-config rule for a solution-style
// config: the referenced configs that include the file, all of them.
func TestOwners(t *testing.T) {
	in := testInputs(t, map[string]string{
		"tsconfig.json":      `{"files": [], "references": [{"path": "./tsconfig.app.json"}, {"path": "./tsconfig.spec.json"}]}`,
		"tsconfig.app.json":  `{"include": ["src"]}`,
		"tsconfig.spec.json": `{"include": ["src/**/*.ts"]}`,
	})
	r := &esResolver{in: in}
	owners, err := r.owners(context.Background(), "src/a.ts")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, c := range owners {
		got = append(got, c.path)
	}
	if !slices.Equal(got, []string{"tsconfig.app.json", "tsconfig.spec.json"}) {
		// Mutation: keep the first owner, or stop at the solution config -> one owner or the root config.
		t.Fatalf("owners = %q", got)
	}
}

// TestModuleBasesFileOnly pins that a member is indexed under its own path
// only: a directory's index file is reached through the directory only
// where the importing file's mode allows it (esResolver.probe).
func TestModuleBasesFileOnly(t *testing.T) {
	if got := moduleBases("src/lib/index.ts", false); !slices.Equal(got, []string{"src/lib/index"}) {
		// Mutation: index every index file under its directory -> `import "./lib"` binds under nodenext ESM.
		t.Fatalf("moduleBases = %q", got)
	}
	if got := stripExtension("dist/u.d.mts"); got != "dist/u" {
		// Mutation: no .d.mts extension -> an ESM declaration file maps to dist/u.d.
		t.Fatalf("stripExtension = %q", got)
	}
}

// TestOwnsESConfigs pins that every config an ECMAScript resolution reads is
// an input of its unit, so an edit to it alone rebuilds the unit.
func TestOwnsESConfigs(t *testing.T) {
	u := Unit{Family: FamilyJavaScript}
	for _, p := range []string{"tsconfig.base.json", "apps/a/tsconfig.app.json", "pnpm-workspace.yaml", "lerna.json"} {
		if !u.OwnsInput(p) {
			// Mutation: closure by the project marker names alone -> tsconfig.base.json is not an input.
			t.Errorf("%s is not an input", p)
		}
	}
	if u.OwnsInput("data/strings.json") {
		// Mutation: every .json an input -> a data file rebuilds the unit.
		t.Error("data/strings.json is an input")
	}
}
