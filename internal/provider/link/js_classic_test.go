package link

import (
	"context"
	"slices"
	"testing"
)

// resolveIn resolves spec, written in the member at from as an import
// declaration, in a JavaScript unit over files.
func resolveIn(t *testing.T, files map[string]string, from, spec string) esResult {
	t.Helper()
	u := jsUnit(t, newMapView(files))
	ctx := context.Background()
	r, err := newESResolver(ctx, u)
	if err != nil {
		t.Fatal(err)
	}
	defer r.close()
	owners, err := r.owners(ctx, from)
	if err != nil {
		t.Fatal(err)
	}
	res, err := r.resolve(ctx, owners, from, spec, esStatic)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// TestClassicResolution pins TypeScript's classic resolution
// (classicNameResolver): a bare specifier climbs the importing file's
// directories looking for a file, TypeScript files before JavaScript files
// over the whole climb; no directory index, package.json or workspace
// package is read; a `paths` key that matches decides without the baseUrl.
func TestClassicResolution(t *testing.T) {
	classic := `{"compilerOptions": {"moduleResolution": "classic"}}`
	cases := []struct {
		name     string
		files    map[string]string
		from     string
		spec     string
		want     []string
		extn     bool
		mutation string
	}{
		{"a bare specifier found by the climb", map[string]string{
			"tsconfig.json": classic, "src/app/main.ts": "", "src/utils.ts": ""},
			"src/app/main.ts", "utils", []string{"src/utils"}, false,
			"no ancestor climb -> utils is external"},
		{"a TypeScript file above beats a JavaScript file nearer", map[string]string{
			"tsconfig.json": classic, "src/app/main.ts": "", "src/app/util.js": "", "util.ts": ""},
			"src/app/main.ts", "util", []string{"util"}, false,
			"one pass over every extension -> src/app/util.js wins"},
		{"a directory's index file is not read", map[string]string{
			"tsconfig.json": classic, "src/main.ts": "", "src/lib/index.ts": ""},
			"src/main.ts", "./lib", nil, false,
			"the node probe (index fallback) -> ./lib binds src/lib/index"},
		{"a workspace package is not read", map[string]string{
			"tsconfig.json": classic, "package.json": `{"workspaces": ["packages/*"]}`, "src/main.ts": "",
			"packages/p/package.json": `{"name": "p", "main": "index.ts"}`, "packages/p/index.ts": ""},
			"src/main.ts", "p", nil, true,
			"the workspace map read under classic -> p binds packages/p/index"},
		{"a .js specifier reads the .ts file", map[string]string{
			"tsconfig.json": classic, "src/main.ts": "", "src/a.ts": ""},
			"src/main.ts", "./a.js", []string{"src/a"}, false,
			"no extension replacement -> ./a.js names nothing"},
		{"a matching paths key decides without the baseUrl", map[string]string{
			"tsconfig.json": `{"compilerOptions": {"moduleResolution": "classic", "baseUrl": "lib", "paths": {"x/*": ["gen/*"]}}}`,
			"src/main.ts":   "", "lib/x/y.ts": ""},
			"src/main.ts", "x/y", nil, true,
			"the baseUrl read after a matching paths key misses -> x/y binds lib/x/y.ts"},
		{"AMD output defaults to classic", map[string]string{
			"tsconfig.json": `{"compilerOptions": {"module": "amd"}}`, "src/app/main.ts": "", "src/utils.ts": ""},
			"src/app/main.ts", "utils", []string{"src/utils"}, false,
			"module amd read as the unset default (Bundler and Node16) -> utils is external"},
	}
	for _, c := range cases {
		got := resolveIn(t, c.files, c.from, c.spec)
		if !slices.Equal(got.targets, c.want) || got.extn != c.extn {
			t.Errorf("%s: %s resolves to %q (external %v), want %q (external %v); mutation: %s",
				c.name, c.spec, got.targets, got.extn, c.want, c.extn, c.mutation)
		}
	}
}

// TestRootDirsLongestPrefix pins TypeScript's rootDirs rule for a relative
// import under any mode: the rootDir that is the longest prefix of the
// path decides the suffix tried under the others.
func TestRootDirsLongestPrefix(t *testing.T) {
	got := resolveIn(t, map[string]string{
		"tsconfig.json":     `{"compilerOptions": {"moduleResolution": "bundler", "rootDirs": ["src", "src/views", "gen"]}}`,
		"src/views/main.ts": "", "gen/t.ts": "", "gen/views/t.ts": "",
	}, "src/views/main.ts", "./t")
	if !slices.Equal(got.targets, []string{"gen/t"}) {
		// Mutation: every rootDir that prefixes the path is tried, in order
		// -> src's suffix views/t binds gen/views/t first.
		t.Fatalf("./t resolves to %q, want gen/t", got.targets)
	}
}

// TestPathsKeyDecidesBeforeBaseURL pins, under a node mode, that a `paths`
// key that matches decides the settings' answer: the baseUrl is read only
// when no key matches (tryLoadModuleUsingOptionalResolutionSettings).
func TestPathsKeyDecidesBeforeBaseURL(t *testing.T) {
	got := resolveIn(t, map[string]string{
		"tsconfig.json": `{"compilerOptions": {"moduleResolution": "bundler", "baseUrl": "lib", "paths": {"x/*": ["gen/*"]}}}`,
		"src/main.ts":   "", "lib/x/y.ts": "",
	}, "src/main.ts", "x/y")
	if len(got.targets) != 0 || !got.extn {
		// Mutation: the baseUrl read after a matching paths key misses ->
		// x/y binds lib/x/y.
		t.Fatalf("x/y resolves to %q (external %v), want an outside package", got.targets, got.extn)
	}
}
