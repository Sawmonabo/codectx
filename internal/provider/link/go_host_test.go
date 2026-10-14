package link

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestTheStandardLibraryIsFoundThroughTheUsersGoLink protects the linker's
// view of Go outside the repository on a machine like the one Go indexing
// failed on: no GOROOT, a service manager's PATH, and go a symbolic link in
// ~/.local/bin to a toolchain kept elsewhere. The standard library is the
// linked toolchain's, found through the link without running go.
//
// Mutation: look go up on PATH only and no standard library is found, so every
// external Go type the project names is unavailable.
func TestTheStandardLibraryIsFoundThroughTheUsersGoLink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in toolchain is a unix executable behind a symbolic link")
	}
	home := t.TempDir()
	root := filepath.Join(home, ".local", "share", "go-versions", "1.27.2")
	for _, dir := range []string{filepath.Join(root, "bin"), filepath.Join(root, "src", "runtime")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "bin", "go"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "VERSION"), []byte("go1.27.2\ntime 2026-09-01T00:00:00Z\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "bin", "go"), filepath.Join(bin, "go")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", t.TempDir())
	t.Setenv("HOME", home)
	t.Setenv("GOROOT", "")
	t.Setenv("GOENV", "off")

	dir, release := goRoot("", "1.27", "")
	if want, _ := filepath.EvalSymlinks(root); dir != want || release != "go1.27.2" {
		t.Fatalf("goRoot = %q, %q; want the linked toolchain %q, go1.27.2", dir, release, want)
	}
}
