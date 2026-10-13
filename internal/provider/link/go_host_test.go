package link_test

import (
	"os"
	"path/filepath"
)

// goTestHost points the Go host sources the linker reads (GOROOT, the
// module cache) at this package's fixtures, so a test reads the same
// standard-library and dependency declarations on every machine, no user go
// env file is read and no declaration is cached outside the test binary's
// own directory, which done removes.
func goTestHost() (done func()) {
	for k, dir := range map[string]string{"GOROOT": "testdata/goroot", "GOMODCACHE": "testdata/gomodcache"} {
		abs, err := filepath.Abs(dir)
		if err != nil {
			panic(err)
		}
		os.Setenv(k, abs)
	}
	os.Setenv("GOENV", "off")
	// The declarations cache is the test binary's own, so a fixture edited
	// between runs is read afresh and no test writes the user's cache.
	cache, err := os.MkdirTemp("", "codectx-link-cache-")
	if err != nil {
		panic(err)
	}
	os.Setenv("XDG_CACHE_HOME", cache)
	return func() { os.RemoveAll(cache) }
}
