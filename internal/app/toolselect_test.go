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
	got, err := SelectedTools(root)
	if err != nil {
		t.Fatalf("SelectedTools: %v", err)
	}

	// The Go and Python indexers and language servers, the runtime the
	// Node-hosted indexer needs, and the dependence engine both languages'
	// families run with its own runtime. The engine and its runtime are read
	// from the lock rather than named, so this file names no analyzer backend.
	lock := toolchain.Embedded()
	want := []string{"gopls", "node", "pyright", "scip-go", "scip-python"}
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
