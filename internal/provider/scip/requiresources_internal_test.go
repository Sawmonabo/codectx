package scip

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
)

// TestRequireSourcesRefusesAManifestWithoutSource protects the refusal that
// stands between a source-less project directory and an opaque tool failure.
//
// Failure mode, measured on a real repository: a package directory holding only
// a manifest and a lock file — no TypeScript or JavaScript at all — made the
// indexer exit 1, and the unit was published as CTX_PROVIDER_UNAVAILABLE "node
// exited with status 1", with no stderr and no remediation, so an operator
// could not tell a missing tool from a project with nothing to index. The same
// shape refuses a Java aggregator POM before a JVM starts.
//
// node_modules is excluded deliberately: a dependency's own sources are not
// this project's, and counting them would restore the opaque failure with an
// extra step. Mutation that must fail this test: drop the skipDirs walk arm, or
// add ".json" to the TypeScript extensions.
func TestRequireSourcesRefusesAManifestWithoutSource(t *testing.T) {
	tsExt := []string{".ts", ".tsx", ".mts", ".cts", ".js", ".jsx", ".mjs", ".cjs"}
	cases := []struct {
		name    string
		files   []string
		what    string
		exts    []string
		skip    []string
		refused bool
	}{
		// The measured project: a manifest, a lock file and documentation.
		{name: "typescript manifest only", what: "TypeScript", exts: tsExt, skip: []string{"node_modules"}, refused: true,
			files: []string{"README.md", "metadata.json", "npm-shrinkwrap.json", "package.json"}},
		{name: "typescript with one source", what: "TypeScript", exts: tsExt, skip: []string{"node_modules"},
			files: []string{"package.json", "src/index.ts"}},
		// A dependency tree cannot answer for the project's own source.
		{name: "typescript sources only under node_modules", what: "TypeScript", exts: tsExt, skip: []string{"node_modules"}, refused: true,
			files: []string{"package.json", "node_modules/dep/index.js"}},
		{name: "java aggregator pom", what: "Java", exts: []string{".java"}, refused: true,
			files: []string{"pom.xml"}},
		{name: "java with one source", what: "Java", exts: []string{".java"},
			files: []string{"pom.xml", "src/main/java/A.java"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			for _, f := range tc.files {
				p := filepath.Join(root, filepath.FromSlash(f))
				if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			err := requireSources(root, tc.what, tc.exts, tc.skip)
			if !tc.refused {
				if err != nil {
					t.Fatalf("a project holding source was refused: %v", err)
				}
				return
			}
			var typed *model.Error
			if !errors.As(err, &typed) || typed.Code != model.CodeProviderOutputInvalid || typed.Remediation == "" {
				t.Fatalf("err = %v, want CTX_PROVIDER_OUTPUT_INVALID with remediation, raised before the tool starts", err)
			}
		})
	}
}
