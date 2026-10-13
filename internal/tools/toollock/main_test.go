package main

import (
	"path/filepath"
	"testing"

	"github.com/Sawmonabo/codectx/internal/paced"
)

// TestWorkDirDefaultsUnderUserCache pins the scratch default to the user
// cache directory. Mutation: a default that names a directory relative to the
// working tree (a repository path) instead of the user cache directory.
func TestWorkDirDefaultsUnderUserCache(t *testing.T) {
	cache := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cache)
	t.Setenv("HOME", cache)
	t.Setenv("LocalAppData", cache)
	saved := *flagWork
	t.Cleanup(func() { *flagWork = saved })

	*flagWork = ""
	got, err := workDir()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(paced.ProductDirName, "toollock"); filepath.Join(filepath.Base(filepath.Dir(got)), filepath.Base(got)) != want {
		t.Fatalf("default scratch %q does not end in %q", got, want)
	}
	if rel, err := filepath.Rel(cache, got); err != nil || !filepath.IsLocal(rel) {
		t.Fatalf("default scratch %q is not under the user cache directory %q", got, cache)
	}
}
