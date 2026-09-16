package app

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/Sawmonabo/codectx/internal/toolchain"
)

// A repository must select exactly the pinned tools its own root declares.
// Under-selecting is the failure an offline runner discovers mid-index, when a
// payload `tools prefetch --for-repo` was supposed to have installed is fetched
// from a host with no network; over-selecting sends the doctor's operator to
// download gigabytes for a language the repository does not contain. Both are
// silent until the run that needs the answer, so the mapping is proved here.
func TestSelectedToolsIsTheRootsOwnAnswer(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"go.mod", "main.go", "pyproject.toml", "app.py"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("x\n"), 0o600); err != nil {
			t.Fatalf("write fixture %s: %v", name, err)
		}
	}
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	got, err := SelectedTools(root)
	if err != nil {
		t.Fatalf("SelectedTools: %v", err)
	}

	// The Go and Python indexers and language servers, the runtime the
	// Node-hosted indexer needs, and the dependence engine both languages'
	// families run with its own runtime. The engine and its runtime are read
	// from the lock rather than named, so this file names no analyzer backend.
	lock := toolchain.Embedded()
	want := []string{"gopls", "node", "scip-go", "scip-python", "ty"}
	for _, name := range lock.Names() {
		if lock.Tools[name].Kind != cpgKind {
			continue
		}
		want = append(want, name)
		if rt := lock.Tools[name].Runtime; rt != "" {
			want = append(want, rt)
		}
	}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("selected %v, want exactly %v", got, want)
	}
}

// A monorepo keeps every project in a subdirectory. Reading markers at the
// repository root alone selected NO indexer payload and no language server for
// a repository with a project per subdirectory -- the measured shape -- so
// `tools prefetch --for-repo` installed nothing the index then needed and the
// offline runner discovered it mid-index, which is the one failure this
// mapping exists to prevent. The traversal is the workspace's own, so a
// manifest inside a dependency directory is not a project and selects nothing.
//
// Mutation: in repositorySignals, record a marker only for a path with no "/"
// in it (the repository root's own entries) -> every indexer and every
// language server disappears from the selection and only the graph engine and
// its runtime, which the subdirectory sources still select, remain.
func TestSelectedToolsFindsProjectsInSubdirectories(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		"README.md":                       "x\n",
		"app/package.json":                "{}\n",
		"app/index.ts":                    "x\n",
		"app/node_modules/dep/Cargo.toml": "x\n",
		"services/billing/go.mod":         "module x\n",
		"services/billing/main.go":        "package main\n",
	}
	for name, body := range files {
		full := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatalf("mkdir for %s: %v", name, err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatalf("write fixture %s: %v", name, err)
		}
	}
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	got, err := SelectedTools(root)
	if err != nil {
		t.Fatalf("SelectedTools: %v", err)
	}

	lock := toolchain.Embedded()
	want := []string{"gopls", "node", "scip-go", "scip-typescript", "typescript-language-server"}
	for _, name := range lock.Names() {
		if lock.Tools[name].Kind != cpgKind {
			continue
		}
		want = append(want, name)
		if rt := lock.Tools[name].Runtime; rt != "" {
			want = append(want, rt)
		}
	}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("selected %v, want exactly %v", got, want)
	}
}
