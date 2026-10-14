package link

import (
	"go/version"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"golang.org/x/mod/module"

	"github.com/Sawmonabo/codectx/internal/hosttool"
)

// The read-only host sources of Go code outside the repository: the
// standard library of the Go toolchain that builds the project, and the
// module cache. Each is found by the go command's own rules without running
// it (running it may download a toolchain, "Go Toolchains"), and nothing
// here writes to either.

// goModCache is the module cache directory: GOMODCACHE, else the first
// GOPATH entry's pkg/mod, GOPATH defaulting to $HOME/go (cmd/go
// internal/cfg, go/build defaultGOPATH). Empty when none can be named.
func goModCache() string {
	if v := hosttool.GoEnv("GOMODCACHE", os.LookupEnv); v != "" {
		return filepath.Clean(v)
	}
	gopath := hosttool.GoEnv("GOPATH", os.LookupEnv)
	if gopath == "" {
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			return ""
		}
		gopath = filepath.Join(home, "go")
	}
	first, _, _ := strings.Cut(gopath, string(os.PathListSeparator))
	if first == "" {
		return ""
	}
	return filepath.Join(first, "pkg", "mod")
}

// goModuleDir is the module cache's directory of module m at its exact
// version ("Module cache": both path and version case-escaped), empty when
// m cannot be escaped or no cache is known.
func goModuleDir(cache string, m module.Version) string {
	if cache == "" {
		return ""
	}
	p, err := module.EscapePath(m.Path)
	if err != nil {
		return ""
	}
	v, err := module.EscapeVersion(m.Version)
	if err != nil {
		return ""
	}
	return filepath.Join(cache, filepath.FromSlash(p)+"@"+v)
}

// goRoot is the standard library the project builds with and its Go
// version ("go1.27.2", from GOROOT/VERSION), both empty when none is found.
//
// The go command's own GOROOT is GOROOT from the environment, else the
// directory two levels above its executable, its symbolic links resolved
// (cmd/go internal/cfg findGOROOT); here the executable is the host `go`
// (pathGoRoot), never run. That toolchain builds the project unless the go.mod's
// toolchain line, else its go line, asks for a newer one: the go command
// then switches to that release ("Go Toolchains", GOTOOLCHAIN=auto), kept
// in the module cache as golang.org/toolchain@v0.0.1-<release>.<GOOS>-<GOARCH>;
// that one is read when it is there, and the older one otherwise is not the
// project's, so none is.
func goRoot(cache, goLine, toolchain string) (dir, release string) {
	dir = hosttool.GoEnv("GOROOT", os.LookupEnv)
	if dir == "" {
		dir = pathGoRoot()
	}
	if dir != "" {
		release = goRootRelease(dir)
		if release == "" {
			dir = ""
		}
	}
	want := toolchain
	if want == "" || want == "default" || want == "local" {
		want = ""
		if goLine != "" {
			want = "go" + goLine
		}
	}
	if want == "" || !version.IsValid(want) || release != "" && version.Compare(want, release) <= 0 {
		return dir, release
	}
	tc := goModuleDir(cache, module.Version{Path: "golang.org/toolchain",
		Version: "v0.0.1-" + want + "." + runtime.GOOS + "-" + runtime.GOARCH})
	if tc != "" {
		if r := goRootRelease(tc); r != "" {
			return tc, r
		}
	}
	return "", ""
}

// pathGoRoot is the GOROOT of the host `go` executable (hosttool.Find: PATH,
// then GOROOT, then the installers' default locations): its directory's
// parent, or its symbolic links' target's, holding src/runtime.
func pathGoRoot() string {
	exe, ok := hosttool.Find("go", os.LookupEnv)
	if !ok {
		return ""
	}
	var err error
	for _, e := range []string{exe, ""} {
		if e == "" {
			if e, err = filepath.EvalSymlinks(exe); err != nil {
				return ""
			}
		}
		for _, up := range []string{"../..", "../../.."} {
			dir := filepath.Join(e, up)
			if fi, err := os.Stat(filepath.Join(dir, "src", "runtime")); err == nil && fi.IsDir() {
				return dir
			}
		}
	}
	return ""
}

// goRootRelease is the release a GOROOT's VERSION file names on its first
// line ("go1.27.2"), empty when it names none.
func goRootRelease(dir string) string {
	data, err := os.ReadFile(filepath.Join(dir, "VERSION"))
	if err != nil {
		return ""
	}
	line, _, _ := strings.Cut(string(data), "\n")
	line = strings.TrimSpace(line)
	if !version.IsValid(line) {
		return ""
	}
	return line
}
