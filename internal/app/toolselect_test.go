package app

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/Sawmonabo/codectx/internal/process"
	"github.com/Sawmonabo/codectx/internal/provider/lsp"
	"github.com/Sawmonabo/codectx/internal/provider/scip"
	"github.com/Sawmonabo/codectx/internal/toolchain"
	"github.com/Sawmonabo/codectx/internal/vcs/git"
)

// A repository must select exactly the pinned tools its own markers and
// sources declare, with each selected entry's runtime. Under-selecting is the
// failure an offline runner discovers mid-index, when a payload
// `tools prefetch --for-repo` was supposed to have installed is fetched from a
// host with no network; over-selecting sends the doctor's operator to download
// gigabytes for a language the repository does not contain. Both are silent
// until the run that needs the answer, so the mapping is proved here.
//
// Mutation: drop the `runtimes` loop in SelectedTools -> `node`, which no
// marker names and which the Node-hosted indexer cannot run without, is absent
// from the selection.
func TestSelectedToolsMapsMarkersAndSourcesToLockEntries(t *testing.T) {
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
// This fixture is not a Git repository, so its `app/node_modules/dep/Cargo.toml`
// is untracked and the exclusion is final: the tracked half of that rule is the
// force-include the test below proves.
//
// Mutation: in signals.observe, record a marker only for a path with no "/"
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

// A tracked project under an excluded directory is a project. Section 10.2
// forces a tracked path past the vendor exclusion, so a capture holds
// `third_party/mycrate/Cargo.toml`, the planners root a project on it and an
// index run needs that project's payloads -- while a selection that read only
// the admitted tree pruned `third_party/` wholesale and installed nothing for
// it. That under-selection is silent until the offline runner fetches the
// missing payload mid-index, which is the one failure `--for-repo` exists to
// prevent.
//
// NOT RUN: committed unrun.
//
// Mutation: return nil from signals.tracked before the index listing -> the
// tracked Cargo.toml is invisible and the selection is empty.
func TestSelectedToolsForcesTrackedPathsPastExclusions(t *testing.T) {
	exe, err := git.Locate()
	if err != nil {
		t.Skipf("git is unavailable: %v", err)
	}
	root := t.TempDir()
	files := map[string]string{
		"third_party/mycrate/Cargo.toml": "[package]\nname = \"mycrate\"\n",
		"third_party/mycrate/src/lib.rs": "pub fn f() {}\n",
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
	runner, err := process.NewRunner(process.Limits{MaxConcurrent: 1, MemoryBudgetBytes: 1 << 30})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	gitCmd := func(args ...string) {
		t.Helper()
		result, err := runner.Run(context.Background(), process.Spec{
			Path: exe, Args: args, Dir: root,
			Env:            []string{"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull, "HOME=" + t.TempDir(), "LC_ALL=C"},
			MaxStdoutBytes: 1 << 20, MaxStderrBytes: 1 << 20, Timeout: time.Minute, Grace: time.Second,
		})
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, result.Stderr)
		}
	}
	gitCmd("init", "-q", "-b", "main")
	gitCmd("add", "third_party/mycrate/Cargo.toml", "third_party/mycrate/src/lib.rs")

	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	got, err := SelectedTools(root)
	if err != nil {
		t.Fatalf("SelectedTools: %v", err)
	}

	// What this project's own marker names, read from the mappings the
	// selection reads rather than spelled out here.
	var want []string
	for _, kind := range scip.Kinds {
		if slices.Contains(scip.Triggers(kind), "Cargo.toml") {
			want = append(want, string(kind))
		}
	}
	for _, def := range lsp.Definitions() {
		if slices.Contains(def.RootMarkers, "Cargo.toml") {
			want = append(want, def.Name)
		}
	}
	if len(want) == 0 {
		t.Fatal("no indexer and no language server declares Cargo.toml; this fixture would prove nothing")
	}
	for _, name := range want {
		if !slices.Contains(got, name) {
			t.Fatalf("selected %v, which is missing %s: a tracked project under an excluded directory selected nothing",
				got, name)
		}
	}
}
