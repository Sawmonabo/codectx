package hosttool

import (
	"os"
	"path/filepath"
	"runtime"
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

// homeVar is the variable this platform's home directory is read from.
func homeVar() string {
	if runtime.GOOS == "windows" {
		return "USERPROFILE"
	}
	return "HOME"
}

// Mutation: search the fallbacks before PATH and the GOROOT copy wins.
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

// Mutation: drop GOROOT/bin from the fallbacks, or the install locations, and
// the matching half fails.
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

// TestFindGoLinkedFromTheUserBinDirUnderAServicePATH is the machine the Go
// indexer failed on: no GOROOT, no go env file, a service manager's PATH, and
// go a symbolic link in ~/.local/bin to a toolchain kept elsewhere under the
// home directory, put on PATH only by the login shell's profile.
//
// Mutation: drop userBinDir from the go fallbacks and go is found nowhere.
func TestFindGoLinkedFromTheUserBinDirUnderAServicePATH(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no per-user executable directory")
	}
	noInstallDirs(t)
	home := t.TempDir()
	real := fakeTool(t, filepath.Join(home, ".local", "share", "go-versions", "1.27.2", "bin"), "go")
	bin := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, filepath.Join(bin, "go")); err != nil {
		t.Fatal(err)
	}
	service := t.TempDir() + string(os.PathListSeparator) + t.TempDir()
	vars := env(map[string]string{"PATH": service, "HOME": home, "GOENV": "off"})

	got, ok := Find("go", vars)
	if want := filepath.Join(bin, "go"); !ok || got != want {
		t.Fatalf("Find = %q, %v; want the user's link %q", got, ok, want)
	}
	child, ok := ChildPATH(vars, "go")
	if want := service + string(os.PathListSeparator) + bin; !ok || child != want {
		t.Fatalf("ChildPATH = %q, %v; want the service PATH with %q appended", child, ok, bin)
	}
}

// Mutation: drop the final fallback check, or let an empty lookup succeed,
// and Find reports a tool that is nowhere.
func TestFindReportsAbsence(t *testing.T) {
	noInstallDirs(t, t.TempDir())
	if got, ok := Find("go", env(map[string]string{"PATH": t.TempDir(), "HOME": t.TempDir(), "GOENV": "off"})); ok {
		t.Fatalf("Find = %q; want no go anywhere", got)
	}
}

// Mutation: drop the filepath.IsAbs check in inDirs and the working
// directory's go is found.
func TestFindSkipsRelativePATHEntries(t *testing.T) {
	noInstallDirs(t)
	dir := t.TempDir()
	fakeTool(t, dir, "go")
	t.Chdir(dir)
	if got, ok := Find("go", env(map[string]string{"PATH": ".", "GOENV": "off"})); ok {
		t.Fatalf("Find = %q; a relative PATH entry must not be searched", got)
	}
}

// Mutation: read GOROOT from the environment only and the file's GOROOT is
// never searched.
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

// TestGoEnvReadsTheFileAsTheGoCommandDoes pins the go command's own reading
// of the file (cmd/go internal/cfg readEnvFile): a later line for a key wins,
// a line not starting with an upper-case letter is skipped, and nothing is
// trimmed.
//
// Mutation: return the first match, or trim the line, and the matching case
// fails.
func TestGoEnvReadsTheFileAsTheGoCommandDoes(t *testing.T) {
	file := filepath.Join(t.TempDir(), "env")
	body := "GOROOT=/first\n# GOROOT=/comment\n GOROOT=/indented\nGOROOT=/last\nGOPATH=/p"
	if err := os.WriteFile(file, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	vars := env(map[string]string{"GOENV": file})
	if got := GoEnv("GOROOT", vars); got != "/last" {
		t.Errorf("GoEnv(GOROOT) = %q, want the last line's /last", got)
	}
	if got := GoEnv("GOPATH", vars); got != "/p" {
		t.Errorf("GoEnv(GOPATH) = %q, want /p from a last line with no newline", got)
	}
	if got := GoEnv("GOROOT", env(map[string]string{"GOENV": file, "GOROOT": "/env"})); got != "/env" {
		t.Errorf("GoEnv(GOROOT) = %q; the environment must win over the file", got)
	}
	if got := GoEnv("GOROOT", env(map[string]string{"GOENV": "off"})); got != "" {
		t.Errorf("GoEnv(GOROOT) with GOENV=off = %q, want none", got)
	}
}

// Mutation: drop CARGO_HOME from the fallbacks, or read the home directory
// from the other platform's variable, and the matching case fails.
func TestFindCargoInCargoHomeThenHome(t *testing.T) {
	cargoHome := t.TempDir()
	inCargoHome := fakeTool(t, filepath.Join(cargoHome, "bin"), "cargo")
	home := t.TempDir()
	inHome := fakeTool(t, filepath.Join(home, ".cargo", "bin"), "cargo")
	short := t.TempDir()

	got, ok := Find("cargo", env(map[string]string{"PATH": short, "CARGO_HOME": cargoHome, homeVar(): home}))
	if !ok || got != inCargoHome {
		t.Fatalf("with CARGO_HOME: Find = %q, %v; want %q", got, ok, inCargoHome)
	}
	got, ok = Find("cargo", env(map[string]string{"PATH": short, homeVar(): home}))
	if !ok || got != inHome {
		t.Fatalf("without CARGO_HOME: Find = %q, %v; want %q", got, ok, inHome)
	}
	other := "USERPROFILE"
	if homeVar() == other {
		other = "HOME"
	}
	if got, ok := Find("cargo", env(map[string]string{"PATH": short, other: home})); ok {
		t.Fatalf("with only %s: Find = %q; cargo installs under %s on this platform", other, got, homeVar())
	}
}

// TestFindCargoWhereCARGONamesIt follows rust-analyzer, which runs the cargo
// that CARGO names before any on PATH or in Cargo's home.
//
// Mutation: drop the CARGO lookup and cargo is found nowhere.
func TestFindCargoWhereCARGONamesIt(t *testing.T) {
	exe := fakeTool(t, filepath.Join(t.TempDir(), "toolchain", "bin"), "cargo")
	got, ok := Find("cargo", env(map[string]string{"PATH": t.TempDir(), "CARGO": exe}))
	if !ok || got != exe {
		t.Fatalf("Find = %q, %v; want the cargo CARGO names, %q", got, ok, exe)
	}
	if got, ok := Find("cargo", env(map[string]string{"PATH": t.TempDir(), "CARGO": "relative/cargo"})); ok {
		t.Fatalf("Find = %q; a relative CARGO names a different file in every directory", got)
	}
}

// Mutation: append a directory the PATH already holds, or put the found
// directory first, and the PATH is no longer the parent's with an addition.
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

// Mutation: return the empty parent PATH as present and the child gets an
// empty PATH it never had.
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

// Mutation: drop the execute-bit check, or the regular-file check, in
// isExecutable and a non-program is reported as the tool.
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
	// Every fallback is absent: no GOROOT, no home, no CARGO_HOME, no install
	// location. Only the two non-programs on PATH could be found.
	for _, name := range []string{"go", "cargo"} {
		if got, ok := Find(name, env(map[string]string{"PATH": dir, "GOENV": "off"})); ok {
			t.Errorf("Find(%q) = %q; a file without an execute bit or a directory is no tool", name, got)
		}
	}
}
