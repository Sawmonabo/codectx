package filesystem_test

import (
	"strings"
	"testing"

	"github.com/Sawmonabo/codectx/internal/provider/filesystem"
	"github.com/Sawmonabo/codectx/internal/provider/providertest"
)

// TestFilesystemConform runs the shared conformance fixture over a polyglot
// repository: a nested source file, a README, a build file and a file made
// of one line longer than a chunk. Failure mode: a unit whose identities
// depended on discovery order, wall time or the sink's flush timing would
// publish different repository, directory or file IDs for the same bytes,
// so unchanged files could never reuse their units and a relation could
// reach storage before the node it references.
func TestFilesystemConform(t *testing.T) {
	files := map[string]string{
		"README.md":       "# App\n\nSee [main](cmd/app/main.go).\n",
		"cmd/app/main.go": "package main\n\nfunc main() {}\n",
		"Dockerfile":      "FROM scratch\n",
		"docs/long.txt":   strings.Repeat("é", 20000),
		"assets/logo.bin": "PNG\x00\x00binary",
		"empty.txt":       "",
	}
	p, err := filesystem.New(filesystem.Options{MaxSearchFileBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"README.md", "cmd/app/main.go", "Dockerfile", "docs/long.txt", "assets/logo.bin", "empty.txt"} {
		providertest.Conform(t, p, files, filesystem.ScopeKey(path), []string{path})
	}
}
