package lsp

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestServersFindTheirToolchainOutsideAShortPATH protects the reason gopls and
// rust-analyzer could not load a project when an editor or a service started
// codectx: the parent's PATH lacked the directory the user's toolchain is in.
// A server that drives a host toolchain gets the parent's PATH with that
// directory appended; a server that drives none keeps the parent's PATH.
//
// Mutation: drop Host from the gopls or rust-analyzer definition, or the PATH
// branch of env, and that server's PATH has no toolchain on it.
func TestServersFindTheirToolchainOutsideAShortPATH(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in toolchain is a set of unix executables")
	}
	tool := func(dir, name string) {
		t.Helper()
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	home := t.TempDir()
	goBin := filepath.Join(home, ".local", "bin")
	tool(goBin, "go")
	cargoHome := t.TempDir()
	cargoBin := filepath.Join(cargoHome, "bin")
	tool(cargoBin, "cargo")
	tool(cargoBin, "rustc")
	short := t.TempDir()
	t.Setenv("PATH", short)
	t.Setenv("HOME", home)
	t.Setenv("CARGO_HOME", cargoHome)
	t.Setenv("GOROOT", "")
	t.Setenv("GOENV", "off")

	pathOf := func(server string) string {
		t.Helper()
		def, ok := definitions[server]
		if !ok {
			t.Fatalf("no server definition named %s", server)
		}
		for _, kv := range (Profile{Definition: def}).env() {
			if v, ok := strings.CutPrefix(kv, "PATH="); ok {
				return v
			}
		}
		return ""
	}
	sep := string(os.PathListSeparator)
	for server, want := range map[string]string{
		"gopls":         short + sep + goBin,
		"rust-analyzer": short + sep + cargoBin,
		"ty":            short,
	} {
		if got := pathOf(server); got != want {
			t.Errorf("%s PATH = %q, want %q", server, got, want)
		}
	}
}
