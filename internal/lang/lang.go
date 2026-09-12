// Package lang is the single path-to-language table of the codebase (ruling
// R7-4 and the Task 7 fix-round ruling that unified it with the snapshot
// manifest's table). A path becomes a language tag here and nowhere else, so
// the tag a snapshot records in its manifest and the tag a provider selects
// files by can never drift apart.
//
// The table is the union of the two tables that had diverged: it returns the
// same tag for every path the snapshot table recognized, and recognizes the
// documentation, build and configuration extensions the filesystem provider
// added. Classification is by extension and basename only: no file is opened
// to guess, and a path with no entry honestly has no language.
package lang

import (
	"path"
	"strings"
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
// rather than text.
func Of(rel string) string {
	base := path.Base(rel)
	if l, ok := byBasename[base]; ok {
		return l
	}
	return byExtension[strings.ToLower(path.Ext(base))]
}
