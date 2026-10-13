package codectx

import (
	"os/exec"
	"runtime/debug"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
)

// TestDigestFollowsTheCode digests a module of three packages, a importing b
// and an outside module, c reached by nothing: a change to the code a or b
// is built from, to the outside module's version or to the toolchain changes
// the digest, and a change to a test file, to c or to a module nothing
// imports does not, nor does digesting the same code again.
//
// Mutations: read only the roots' own files -> a change to b keeps the
// digest. Read test files -> a change to a_test.go moves it. Leave the
// module versions out -> the version bump keeps it.
func TestDigestFollowsTheCode(t *testing.T) {
	files := func() fstest.MapFS {
		return fstest.MapFS{
			"internal/a/a.go":      {Data: []byte("package a\n\nimport (\n\t\"fmt\"\n\t\"" + Module + "/internal/b\"\n\t\"example.com/ext/sub\"\n)\n")},
			"internal/a/a_test.go": {Data: []byte("package a\n")},
			"internal/b/b.go":      {Data: []byte("package b\n\nfunc B() int { return 1 }\n")},
			"internal/c/c.go":      {Data: []byte("package c\n")},
		}
	}
	mods := func(ext, other string) []*debug.Module {
		return []*debug.Module{{Path: "example.com/ext", Version: ext, Sum: "h1:" + ext},
			{Path: "example.com/other", Version: other, Sum: "h1:" + other}}
	}
	sum := func(fsys fstest.MapFS, deps []*debug.Module, toolchain string) string {
		t.Helper()
		s, pkgs, err := digest(fsys, deps, toolchain, "internal/a")
		if err != nil {
			t.Fatal(err)
		}
		if want := []string{"internal/a", "internal/b"}; !slices.Equal(pkgs, want) {
			t.Fatalf("the digest read packages %v, want %v", pkgs, want)
		}
		return s
	}
	base := sum(files(), mods("v1.0.0", "v1.0.0"), "go1")
	if again := sum(files(), mods("v1.0.0", "v1.0.0"), "go1"); again != base {
		t.Error("the same code digests differently")
	}
	edit := func(name, data string) fstest.MapFS {
		f := files()
		f[name] = &fstest.MapFile{Data: []byte(data)}
		return f
	}
	for _, c := range []struct {
		what  string
		got   string
		moves bool
	}{
		{"an edit of the root", sum(edit("internal/a/a.go", string(files()["internal/a/a.go"].Data)+"\nvar X = 1\n"), mods("v1.0.0", "v1.0.0"), "go1"), true},
		{"an edit of an imported package", sum(edit("internal/b/b.go", "package b\n\nfunc B() int { return 2 }\n"), mods("v1.0.0", "v1.0.0"), "go1"), true},
		{"a new file of an imported package", sum(edit("internal/b/b2.go", "package b\n"), mods("v1.0.0", "v1.0.0"), "go1"), true},
		{"a bump of an imported module", sum(files(), mods("v1.1.0", "v1.0.0"), "go1"), true},
		{"another toolchain", sum(files(), mods("v1.0.0", "v1.0.0"), "go2"), true},
		{"an edit of a test file", sum(edit("internal/a/a_test.go", "package a\n\nvar T = 1\n"), mods("v1.0.0", "v1.0.0"), "go1"), false},
		{"an edit of a package nothing imports", sum(edit("internal/c/c.go", "package c\n\nvar C = 1\n"), mods("v1.0.0", "v1.0.0"), "go1"), false},
		{"a bump of a module nothing imports", sum(files(), mods("v1.0.0", "v2.0.0"), "go1"), false},
	} {
		if moved := c.got != base; moved != c.moves {
			t.Errorf("%s: digest moved = %v, want %v", c.what, moved, c.moves)
		}
	}
	if _, _, err := digest(edit("internal/a/a.go", "package a\n\nimport \""+Module+"/internal/gone\"\n"), nil, "go1", "internal/a"); err == nil {
		t.Error("an import whose sources are not embedded digests without an error")
	}
}

// TestDigestCoversTheBuild checks, for each root set a provider's version is
// digested from, that the embedded sources the digest reads hold every
// package of the module the go command builds the roots from.
//
// Mutation: drop a depth from the embed patterns -> a package the build
// links is not read.
func TestDigestCoversTheBuild(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go command")
	}
	for _, roots := range [][]string{
		{"internal/provider/treesitter", "internal/provider/treesitter/worker"},
		{"internal/provider/link"},
	} {
		_, read, err := digest(sources, nil, "go", roots...)
		if err != nil {
			t.Fatal(err)
		}
		args := []string{"list", "-deps", "-f", "{{if .Module}}{{if eq .Module.Path \"" + Module + "\"}}{{.ImportPath}}{{end}}{{end}}"}
		for _, r := range roots {
			args = append(args, "./"+r)
		}
		out, err := exec.Command("go", args...).Output()
		if err != nil {
			t.Fatalf("go list: %v", err)
		}
		for _, p := range strings.Fields(string(out)) {
			dir := strings.TrimPrefix(p, Module+"/")
			if p == Module {
				dir = "."
			}
			if !slices.Contains(read, dir) {
				t.Errorf("%v: the build links %s, and the digest does not read it", roots, p)
			}
		}
	}
}

// BenchmarkDigest is the cost a process pays once for each version.
func BenchmarkDigest(b *testing.B) {
	for b.Loop() {
		if _, err := Digest("internal/provider/treesitter", "internal/provider/treesitter/worker"); err != nil {
			b.Fatal(err)
		}
		if _, err := Digest("internal/provider/link"); err != nil {
			b.Fatal(err)
		}
	}
}
