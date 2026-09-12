// Package lang is the pinned language registry of the tree-sitter provider:
// the nine mandatory grammars of Section 11.3, the exact module version,
// grammar ABI and grammar metadata each one was reviewed at, the file
// extensions each grammar declares, and the embedded structural query pack.
// It is pure Go so both the parent provider and the parser worker import it;
// the worker verifies every grammar it links against this table at startup
// and refuses to run on a mismatch, so a stale binary can never publish facts
// under the wrong fingerprint.
package lang

import (
	"embed"
	"path"
	"slices"
	"strconv"
	"strings"

	"github.com/Sawmonabo/codectx/internal/model"
)

// BindingModule is the pinned Go binding. Its lifecycle files were read in
// full before the worker's API choice (see docs/providers-treesitter.md).
const BindingModule = "github.com/tree-sitter/go-tree-sitter@v0.25.0"

// ProviderID is the descriptor identity of the structural provider.
const ProviderID = "treesitter"

// extractionVersion changes whenever the worker's extraction logic changes
// the facts it emits for the same source and query pack.
const extractionVersion = "1"

//go:embed queries/*.scm
var queryFS embed.FS

// Language is one pinned grammar registration.
type Language struct {
	// Name is the language tag used by configuration, the snapshot manifest
	// and Node.Language.
	Name string
	// Module is the pinned grammar module and version.
	Module string
	// ABI is the tree-sitter language ABI the pinned grammar was generated
	// with; the worker refuses a grammar whose runtime ABI differs.
	ABI uint32
	// Metadata is the grammar's own semantic version as compiled into the
	// parser (ts_language_metadata), empty for ABI 14 grammars that predate
	// it. Where present the worker checks it too.
	Metadata string
	// Extensions are the file extensions the grammar declares in its
	// tree-sitter.json; used only when the manifest row carries no language.
	Extensions []string
	// Query is the compiled-per-worker structural query source.
	Query string
	// Separator joins qualified-name components.
	Separator string
}

// All is every supported language in name order.
var All = build()

func build() []Language {
	read := func(names ...string) string {
		var b strings.Builder
		for _, n := range names {
			src, err := queryFS.ReadFile("queries/" + n + ".scm")
			if err != nil {
				panic("treesitter query pack " + n + " is missing: " + err.Error())
			}
			b.Write(src)
			b.WriteByte('\n')
		}
		return b.String()
	}
	js := read("ecmascript", "javascript")
	ts := read("ecmascript", "typescript")
	c := read("c")
	cpp := read("c", "cpp")
	all := []Language{
		{Name: "c", Module: "github.com/tree-sitter/tree-sitter-c@v0.24.2", ABI: 15, Metadata: "0.24.2", Extensions: []string{".c", ".h"}, Query: c, Separator: "::"},
		{Name: "cpp", Module: "github.com/tree-sitter/tree-sitter-cpp@v0.23.4", ABI: 14, Extensions: []string{".cc", ".cpp", ".cxx", ".hpp", ".hh", ".hxx"}, Query: cpp, Separator: "::"},
		{Name: "go", Module: "github.com/tree-sitter/tree-sitter-go@v0.25.0", ABI: 15, Metadata: "0.25.0", Extensions: []string{".go"}, Query: read("go"), Separator: "."},
		{Name: "java", Module: "github.com/tree-sitter/tree-sitter-java@v0.23.5", ABI: 14, Extensions: []string{".java"}, Query: read("java"), Separator: "."},
		{Name: "javascript", Module: "github.com/tree-sitter/tree-sitter-javascript@v0.25.0", ABI: 15, Metadata: "0.25.0", Extensions: []string{".js", ".mjs", ".cjs", ".jsx"}, Query: js, Separator: "."},
		{Name: "python", Module: "github.com/tree-sitter/tree-sitter-python@v0.25.0", ABI: 15, Metadata: "0.25.0", Extensions: []string{".py", ".pyi"}, Query: read("python"), Separator: "."},
		// The rust parser.c at v0.24.2 still embeds metadata 0.24.1; the pin
		// records what the compiled grammar reports.
		{Name: "rust", Module: "github.com/tree-sitter/tree-sitter-rust@v0.24.2", ABI: 15, Metadata: "0.24.1", Extensions: []string{".rs"}, Query: read("rust"), Separator: "::"},
		{Name: "tsx", Module: "github.com/tree-sitter/tree-sitter-typescript@v0.23.2", ABI: 14, Extensions: []string{".tsx"}, Query: ts, Separator: "."},
		{Name: "typescript", Module: "github.com/tree-sitter/tree-sitter-typescript@v0.23.2", ABI: 14, Extensions: []string{".ts", ".mts", ".cts"}, Query: ts, Separator: "."},
	}
	slices.SortFunc(all, func(a, b Language) int { return strings.Compare(a.Name, b.Name) })
	return all
}

// Lookup returns the registration for a language tag.
func Lookup(name string) (Language, bool) {
	i, ok := slices.BinarySearchFunc(All, name, func(l Language, n string) int { return strings.Compare(l.Name, n) })
	if !ok {
		return Language{}, false
	}
	return All[i], true
}

// ByExtension classifies a path by the extension its grammar declares. It is
// the fallback for a manifest row without a language tag; ".h" is C here
// because the C grammar owns it in both grammars' declarations.
func ByExtension(p string) (Language, bool) {
	ext := strings.ToLower(path.Ext(p))
	for _, l := range All {
		if slices.Contains(l.Extensions, ext) {
			return l, true
		}
	}
	return Language{}, false
}

// Fingerprint is the digest folded into the provider version: binding,
// extraction version and, per language, module version, ABI, metadata and
// the exact query source. Any change to a grammar pin or a query invalidates
// every unit the provider has sealed, which is what Section 9.4 requires of
// an analyzer/query version.
func Fingerprint() string {
	h := model.NewHasher("treesitter-fingerprint-v1")
	h.AddString(BindingModule)
	h.AddString(extractionVersion)
	for _, l := range All {
		h.AddString(l.Name)
		h.AddString(l.Module)
		h.AddString(strconv.FormatUint(uint64(l.ABI), 10))
		h.AddString(l.Metadata)
		h.AddString(l.Query)
	}
	return h.Sum()
}

// Version is the provider version string: a readable prefix plus the first
// 24 hex digits of the fingerprint, well inside model.MaxIdentifierBytes.
func Version() string {
	return "1." + extractionVersion + "-" + Fingerprint()[:24]
}
