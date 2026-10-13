package link

import (
	"context"
	"slices"
	"testing"

	"golang.org/x/mod/module"

	"github.com/Sawmonabo/codectx/internal/provider/providertest"
)

// TestGoSourcesFollowTheGoCommand protects where a package outside the unit
// is read from, by the go command's own rules: a workspace module the
// governing go.work uses, from the snapshot; a local replacement, from the
// snapshot when inside the repository and nowhere when outside it; a
// versioned replacement at its own version; a requirement by the longest
// module path that prefixes the import; the standard library by a first
// path element without a dot; anything the go.mod does not require,
// nowhere.
//
// Mutation: read go.work replacements after the go.mod's -> example.com/r
// reads the go.mod's target. Mutation: match a requirement by any prefix
// -> example.com/a/b/c reads example.com/a. Mutation: follow a local
// replacement out of the repository -> example.com/out is read. Mutation:
// let a go.work govern a module it does not use -> the tools unit reads
// the workspace's modules.
func TestGoSourcesFollowTheGoCommand(t *testing.T) {
	h := providertest.New(t, map[string]string{
		"go.work": "go 1.27\n\nuse (\n\t.\n\t./staging/api\n)\n\nreplace example.com/r => ./work/r\n",
		"go.mod": `module example.com/m

go 1.27

toolchain go1.27.1

require (
	example.com/a v1.0.0
	example.com/a/b v1.1.0
	example.com/old v1.0.0
	example.com/r v1.0.0
	example.com/local v1.0.0
	example.com/out v1.0.0
)

replace (
	example.com/old v1.0.0 => example.com/new v1.3.0
	example.com/r => ./mod/r
	example.com/local => ./third/local
	example.com/out => ../outside
)
`,
		"staging/api/go.mod": "module example.com/api\n\ngo 1.27\n",
		"staging/api/api.go": "package api\n",
		"tools/go.mod":       "module example.com/tools\n\ngo 1.27\n",
		"tools/main.go":      "package main\n",
		"third/local/go.mod": "module example.com/local\n",
		"third/local/l.go":   "package local\n",
		"work/r/go.mod":      "module example.com/r\n",
		"mod/r/go.mod":       "module example.com/r\n",
		"vendor/modules.txt": "# example.com/a v1.0.0\n",
	})
	ctx := context.Background()
	info, err := readGoModuleInfo(ctx, &unit{content: h.View, family: FamilyGo})
	if err != nil {
		t.Fatal(err)
	}
	if info.module != "example.com/m" || info.goVersion != "1.27" || info.toolchain != "go1.27.1" || info.work != "go.work" {
		t.Fatalf("module %q go %q toolchain %q work %q", info.module, info.goVersion, info.toolchain, info.work)
	}
	cases := []struct {
		p    string
		want goSource
	}{
		{"net/http", goSource{std: true, sub: "net/http"}},
		{"example.com/api/v1", goSource{dir: "staging/api/v1"}},
		{"example.com/r/x", goSource{dir: "work/r/x"}},
		{"example.com/local/y", goSource{dir: "third/local/y"}},
		{"example.com/out/z", goSource{missing: true}},
		{"example.com/old/q", goSource{module: module.Version{Path: "example.com/new", Version: "v1.3.0"}, sub: "q"}},
		{"example.com/a/b/c", goSource{module: module.Version{Path: "example.com/a/b", Version: "v1.1.0"}, sub: "c"}},
		{"example.com/a", goSource{module: module.Version{Path: "example.com/a", Version: "v1.0.0"}}},
		{"example.com/unrequired", goSource{missing: true}},
	}
	for _, c := range cases {
		if got := info.source(c.p); got != c.want {
			t.Errorf("source(%q) = %+v, want %+v", c.p, got, c.want)
		}
	}

	tools, err := readGoModuleInfo(ctx, &unit{root: "tools", content: h.View, family: FamilyGo})
	if err != nil {
		t.Fatal(err)
	}
	if tools.work != "" || len(tools.uses) != 0 {
		t.Fatalf("a go.work that does not use tools governs it: work %q uses %v", tools.work, tools.uses)
	}
}

// TestGoManifestFieldsMoveWithWhatResolutionReads protects the refresh of
// a Go unit: a requirement's version, a replacement, the go line and the
// workspace each decide where a package is read from, so each moves a
// manifest field and the unit's hierarchy is built again.
//
// Mutation: keep only the module path in the fields -> every edit below
// leaves the fields equal.
func TestGoManifestFieldsMoveWithWhatResolutionReads(t *testing.T) {
	base := map[string]string{
		"go.mod":  "module example.com/m\n\ngo 1.27\n\nrequire example.com/a v1.0.0\n",
		"main.go": "package main\n",
	}
	fields := func(files map[string]string) []manifestField {
		h := providertest.New(t, files)
		fs, err := goManifestFields(context.Background(), &unit{content: h.View, family: FamilyGo})
		if err != nil {
			t.Fatal(err)
		}
		return fs
	}
	before := fields(base)
	for name, edit := range map[string]map[string]string{
		"version bump": {"go.mod": "module example.com/m\n\ngo 1.27\n\nrequire example.com/a v1.0.1\n"},
		"replacement":  {"go.mod": "module example.com/m\n\ngo 1.27\n\nrequire example.com/a v1.0.0\n\nreplace example.com/a => ./a\n"},
		"go line":      {"go.mod": "module example.com/m\n\ngo 1.28\n\nrequire example.com/a v1.0.0\n"},
		"workspace":    {"go.work": "go 1.27\n\nuse .\n"},
	} {
		files := map[string]string{}
		for k, v := range base {
			files[k] = v
		}
		for k, v := range edit {
			files[k] = v
		}
		if after := fields(files); slices.Equal(before, after) {
			t.Errorf("%s: the manifest fields did not move: %v", name, after)
		}
	}
}
