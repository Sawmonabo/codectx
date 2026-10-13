package testenv

import (
	"os"
	"path/filepath"
	"runtime"

	"github.com/Sawmonabo/codectx/internal/paced"
)

// GoHost points the Go host sources the linker reads at the repository's own
// fixtures: GOROOT and GOMODCACHE name the small standard library and module
// cache under internal/provider/link/testdata, GOENV is off so no user go env
// file is read, and XDG_CACHE_HOME is a fresh directory so the linker's
// declarations cache is the test binary's own. A test binary whose tests link
// Go sources -- in process, or in a child that inherits the environment --
// calls it once from TestMain, so its results are the same on every machine
// and it never reads the host's Go installation or writes the user's cache.
// done removes the cache directory, at the pace every free in the product
// keeps.
func GoHost() (done func()) {
	_, self, _, ok := runtime.Caller(0)
	if !ok {
		panic("testenv: the caller's source path is not known")
	}
	hostEnv = os.Environ()
	fixtures := filepath.Join(filepath.Dir(self), "..", "provider", "link", "testdata")
	os.Setenv("GOROOT", filepath.Join(fixtures, "goroot"))
	os.Setenv("GOMODCACHE", filepath.Join(fixtures, "gomodcache"))
	os.Setenv("GOENV", "off")
	cache, err := os.MkdirTemp("", "codectx-go-decls-")
	if err != nil {
		panic(err)
	}
	os.Setenv("XDG_CACHE_HOME", cache)
	return func() { _ = paced.RemoveAll(cache) }
}

// hostEnv is the environment as GoHost found it.
var hostEnv []string

// HostEnviron is the environment for a go command a test runs after GoHost:
// the process's environment as it was before GoHost replaced the Go host, so
// the go command builds with the host's own toolchain. Before GoHost it is
// the current environment.
func HostEnviron() []string {
	if hostEnv == nil {
		return os.Environ()
	}
	return hostEnv
}
