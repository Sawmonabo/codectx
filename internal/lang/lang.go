// Package lang is the single path-to-language table of the codebase, shared by
// the filesystem provider and the snapshot manifest. A path becomes a language tag here and nowhere else, so
// the tag a snapshot records in its manifest and the tag a provider selects
// files by can never drift apart.
//
// The table covers source languages and the documentation, build and
// configuration extensions the filesystem provider classifies. Classification is by extension and basename only: no file is opened
// to guess, and a path with no entry honestly has no language.
//
// One extension, ".h", names a header the C and C++ grammars both declare,
// whose language is the repository's and not the file name's. A reader that
// holds the snapshot tags through For, which answers the grammar the
// snapshot's census parses its headers with first; Of is the answer without a
// census, which only a reader indifferent to C against C++ may use.
package lang

import (
	"path"
	"strings"

	"github.com/Sawmonabo/codectx/internal/model"
	tslang "github.com/Sawmonabo/codectx/internal/provider/treesitter/lang"
)

// byExtension maps a lowercased extension to its language tag. The tags for
// the bundled tree-sitter grammars match the tree_sitter.languages spellings
// of docs/configuration.md so a provider can select files by the manifest's
// own column.
var byExtension = map[string]string{
	".go": "go",
	".js": "javascript", ".mjs": "javascript", ".cjs": "javascript", ".jsx": "javascript",
	".ts": "typescript", ".mts": "typescript", ".cts": "typescript",
	".tsx":        "tsx",
	".py":         "python",
	".pyi":        "python",
	".java":       "java",
	".rs":         "rust",
	".c":          "c",
	".h":          "c",
	".cc":         "cpp",
	".cpp":        "cpp",
	".cxx":        "cpp",
	".hpp":        "cpp",
	".hh":         "cpp",
	".hxx":        "cpp",
	".cs":         "csharp",
	".rb":         "ruby",
	".php":        "php",
	".kt":         "kotlin",
	".kts":        "kotlin",
	".swift":      "swift",
	".scala":      "scala",
	".sh":         "shell",
	".bash":       "shell",
	".sql":        "sql",
	".proto":      "protobuf",
	".json":       "json",
	".yaml":       "yaml",
	".yml":        "yaml",
	".toml":       "toml",
	".xml":        "xml",
	".md":         "markdown",
	".markdown":   "markdown",
	".rst":        "rst",
	".adoc":       "asciidoc",
	".asciidoc":   "asciidoc",
	".txt":        "text",
	".html":       "html",
	".css":        "css",
	".graphql":    "graphql",
	".gql":        "graphql",
	".graphqls":   "graphql",
	".tf":         "terraform",
	".tfvars":     "terraform",
	".cmake":      "cmake",
	".bzl":        "starlark",
	".gradle":     "groovy",
	".mk":         "make",
	".dockerfile": "dockerfile",
}

// byBasename covers the build and CI files that carry no extension, plus the
// few whose extension says less than their name.
var byBasename = map[string]string{
	"Makefile":        "make",
	"makefile":        "make",
	"GNUmakefile":     "make",
	"Dockerfile":      "dockerfile",
	"Jenkinsfile":     "groovy",
	"BUILD":           "starlark",
	"BUILD.bazel":     "starlark",
	"WORKSPACE":       "starlark",
	"WORKSPACE.bazel": "starlark",
	"MODULE.bazel":    "starlark",
	"CMakeLists.txt":  "cmake",
}

// Of returns the language tag for a root-relative path, or "" when the path
// says nothing. The basename is consulted first, so CMakeLists.txt is cmake
// rather than text. It knows no census, so a ".h" header answers the table's
// "c" whatever the repository is written in; Tagger.Of is the answer within a
// snapshot.
func Of(rel string) string {
	base := path.Base(rel)
	if l, ok := byBasename[base]; ok {
		return l
	}
	return byExtension[strings.ToLower(path.Ext(base))]
}

// Tagger tags the paths of one snapshot.
type Tagger struct {
	header string
}

// For is the tagger of one snapshot: a header two grammars declare takes the
// grammar the snapshot's census parses headers with first
// (tslang.Census.Header), and every other path its Of tag. It is the grammar
// a header's parse starts from, not the one its kept parse used: the manifest
// row is written at capture, before any parse.
func For(s model.Snapshot) Tagger {
	return Tagger{header: tslang.Census{C: s.CUnits, CPP: s.CPPUnits}.Header().First}
}

// Of returns the language tag of a root-relative path in the tagger's
// snapshot.
func (t Tagger) Of(rel string) string {
	if len(tslang.Candidates(rel)) > 1 {
		return t.header
	}
	return Of(rel)
}
