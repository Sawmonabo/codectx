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
// whose language is the repository's and not the file name's. The capture
// tags through For, which answers the grammar the snapshot's census parses its
// headers with first: the manifest row is written before any parse. A reader
// that holds a generation tags through Tagger.In, which answers the grammar
// that generation's parse of the header kept. Of is the answer without a
// census, which only a reader indifferent to C against C++ may use.
package lang

import (
	"context"
	"path"
	"strings"
	"sync"

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

// Tagger tags the paths of one snapshot, or of one generation when bound by
// In. Copies share one binding, so a copy reports the error any copy met.
type Tagger struct {
	header string
	kept   *keptParses
}

// For is the tagger of one snapshot: a header two grammars declare takes the
// grammar the snapshot's census parses headers with first
// (tslang.Census.Header), and every other path its Of tag. It is the grammar
// a header's parse starts from: the manifest row is written at capture, before
// any parse, so the capture tags with it and a generation's reader binds the
// tagger to its parses with In.
func For(s model.Snapshot) Tagger {
	return Tagger{header: tslang.Census{C: s.CUnits, CPP: s.CPPUnits}.Header().First}
}

// Parses answers the language a provider's parse of one file kept in one
// generation: the language of the module node the provider's unit at scope
// published for path. ok is false when the generation holds no such parse.
type Parses interface {
	ParsedLanguage(ctx context.Context, provider, scope, path string) (language string, ok bool, err error)
}

// keptParses is a tagger's binding to one generation. It remembers the last
// header it answered, since a search reads a file's hits together, and the
// first error a lookup met; it holds nothing that grows with the repository.
type keptParses struct {
	ctx    context.Context
	parses Parses

	mu       sync.Mutex
	lastPath string
	lastTag  string
	err      error
}

// In binds the tagger to one generation's parses: a header two grammars
// declare is then tagged with the grammar that generation's structural parse
// of it kept (the fallback grammar when the first parse had more error bytes,
// the only enabled grammar when tree_sitter.languages enables one), and a
// header the generation holds no parse of keeps the census's first grammar.
// Every other path is tagged as before. A lookup that fails tags its header
// "" and is reported by Err, which the reader checks before it answers.
func (t Tagger) In(ctx context.Context, parses Parses) Tagger {
	t.kept = &keptParses{ctx: ctx, parses: parses}
	return t
}

// Err is the first error a bound tagger's lookup met, or nil.
func (t Tagger) Err() error {
	if t.kept == nil {
		return nil
	}
	t.kept.mu.Lock()
	defer t.kept.mu.Unlock()
	return t.kept.err
}

// Of returns the language tag of a root-relative path in the tagger's
// snapshot, or in its generation when bound by In.
func (t Tagger) Of(rel string) string {
	if !tslang.SharedHeader(rel) {
		return Of(rel)
	}
	if t.kept == nil {
		return t.header
	}
	return t.kept.of(rel, t.header)
}

// of is a bound tagger's answer for one header.
func (k *keptParses) of(rel, first string) string {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.err != nil {
		return ""
	}
	if k.lastPath == rel {
		return k.lastTag
	}
	got, ok, err := k.parses.ParsedLanguage(k.ctx, tslang.ProviderID, tslang.ScopePrefix+rel, rel)
	if err != nil {
		k.err = err
		return ""
	}
	if !ok {
		got = first
	}
	k.lastPath, k.lastTag = rel, got
	return got
}
