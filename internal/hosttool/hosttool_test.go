package hosttool

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// env is a lookup over a fixed map, so a test names the whole environment the
// code under test may read.
func env(m map[string]string) Lookup {
	return func(k string) (string, bool) {
		v, ok := m[k]
		return v, ok
	}
}

// fakeTool writes an executable named for this platform into dir and returns
// its path.
func fakeTool(t *testing.T, dir, name string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, executableName(name))
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// noInstallDirs stubs the default Go locations for the test, so the host's own
// Go installation cannot satisfy a lookup the test expects to fail.
func noInstallDirs(t *testing.T, dirs ...string) {
	t.Helper()
	saved := goInstallDirs
	goInstallDirs = func(Lookup) []string { return dirs }
	t.Cleanup(func() { goInstallDirs = saved })
}

func TestFindPrefersPATH(t *testing.T) {
	noInstallDirs(t)
	onPath := fakeTool(t, filepath.Join(t.TempDir(), "bin"), "go")
	root := t.TempDir()
	fakeTool(t, filepath.Join(root, "bin"), "go")
	got, ok := Find("go", env(map[string]string{"PATH": filepath.Dir(onPath), "GOROOT": root, "GOENV": "off"}))
	if !ok || got != onPath {
		t.Fatalf("Find = %q, %v; want the PATH copy %q", got, ok, onPath)
	}
}

func TestFindFallsBackToGOROOTThenInstallDirs(t *testing.T) {
	root := t.TempDir()
	inRoot := fakeTool(t, filepath.Join(root, "bin"), "go")
	installed := fakeTool(t, filepath.Join(t.TempDir(), "bin"), "go")
	noInstallDirs(t, filepath.Dir(installed))
	short := t.TempDir() // a service PATH: real directories, no go in them

	got, ok := Find("go", env(map[string]string{"PATH": short, "GOROOT": root, "GOENV": "off"}))
	if !ok || got != inRoot {
		t.Fatalf("with GOROOT: Find = %q, %v; want %q", got, ok, inRoot)
	}
	got, ok = Find("go", env(map[string]string{"PATH": short, "GOENV": "off"}))
	if !ok || got != installed {
		t.Fatalf("without GOROOT: Find = %q, %v; want the install location %q", got, ok, installed)
	}
}

func TestFindReportsAbsence(t *testing.T) {
	noInstallDirs(t, t.TempDir())
	if got, ok := Find("go", env(map[string]string{"PATH": t.TempDir(), "GOENV": "off"})); ok {
		t.Fatalf("Find = %q; want no go anywhere", got)
	}
}

func TestFindSkipsRelativePATHEntries(t *testing.T) {
	noInstallDirs(t)
	dir := t.TempDir()
	fakeTool(t, dir, "go")
	t.Chdir(dir)
	if got, ok := Find("go", env(map[string]string{"PATH": ".", "GOENV": "off"})); ok {
		t.Fatalf("Find = %q; a relative PATH entry must not be searched", got)
	}
}

func TestFindReadsGOROOTFromTheGoEnvFile(t *testing.T) {
	noInstallDirs(t)
	root := t.TempDir()
	inRoot := fakeTool(t, filepath.Join(root, "bin"), "go")
	file := filepath.Join(t.TempDir(), "env")
	if err := os.WriteFile(file, []byte("GOTOOLCHAIN=local\nGOROOT="+root+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, ok := Find("go", env(map[string]string{"PATH": t.TempDir(), "GOENV": file}))
	if !ok || got != inRoot {
		t.Fatalf("Find = %q, %v; want GOROOT from the go env file, %q", got, ok, inRoot)
	}
}

func TestFindCargoInCargoHomeThenHome(t *testing.T) {
	cargoHome := t.TempDir()
	inCargoHome := fakeTool(t, filepath.Join(cargoHome, "bin"), "cargo")
	home := t.TempDir()
	inHome := fakeTool(t, filepath.Join(home, ".cargo", "bin"), "cargo")
	short := t.TempDir()

	got, ok := Find("cargo", env(map[string]string{"PATH": short, "CARGO_HOME": cargoHome, "HOME": home}))
	if !ok || got != inCargoHome {
		t.Fatalf("with CARGO_HOME: Find = %q, %v; want %q", got, ok, inCargoHome)
	}
	got, ok = Find("cargo", env(map[string]string{"PATH": short, "HOME": home}))
	if !ok || got != inHome {
		t.Fatalf("without CARGO_HOME: Find = %q, %v; want %q", got, ok, inHome)
	}
	got, ok = Find("cargo", env(map[string]string{"PATH": short, "USERPROFILE": home}))
	if !ok || got != inHome {
		t.Fatalf("with USERPROFILE only: Find = %q, %v; want %q", got, ok, inHome)
	}
}

func TestChildPATHAppendsWhatThePATHLacks(t *testing.T) {
	noInstallDirs(t)
	root := t.TempDir()
	fakeTool(t, filepath.Join(root, "bin"), "go")
	short := t.TempDir()
	sep := string(os.PathListSeparator)

	got, ok := ChildPATH(env(map[string]string{"PATH": short, "GOROOT": root, "GOENV": "off"}), "go")
	if want := short + sep + filepath.Join(root, "bin"); !ok || got != want {
		t.Fatalf("ChildPATH = %q, %v; want %q", got, ok, want)
	}

	onPath := filepath.Dir(fakeTool(t, filepath.Join(t.TempDir(), "bin"), "go"))
	parent := short + sep + onPath
	got, ok = ChildPATH(env(map[string]string{"PATH": parent, "GOROOT": root, "GOENV": "off"}), "go")
	if !ok || got != parent {
		t.Fatalf("ChildPATH = %q, %v; a PATH that already holds go must pass unchanged, %q", got, ok, parent)
	}
}

func TestChildPATHWithNoParentPATH(t *testing.T) {
	noInstallDirs(t)
	root := t.TempDir()
	fakeTool(t, filepath.Join(root, "bin"), "go")
	got, ok := ChildPATH(env(map[string]string{"GOROOT": root, "GOENV": "off"}), "go")
	if want := filepath.Join(root, "bin"); !ok || got != want {
		t.Fatalf("ChildPATH = %q, %v; want just the found directory %q", got, ok, want)
	}
	if got, ok := ChildPATH(env(map[string]string{"GOENV": "off"}), "go"); ok {
		t.Fatalf("ChildPATH = %q; want no PATH when the parent has none and nothing is found", got)
	}
}

func TestFindRequiresAnExecutableFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no execute bit")
	}
	noInstallDirs(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go"), []byte("not a program"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "cargo"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"go", "cargo"} {
		if got, ok := Find(name, env(map[string]string{"PATH": dir, "GOENV": "off"})); ok && strings.HasPrefix(got, dir) {
			t.Errorf("Find(%q) = %q; a file without an execute bit or a directory is no tool", name, got)
		}
	}
}
