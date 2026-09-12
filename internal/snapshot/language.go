package snapshot

import (
	"path"
	"strings"
)

// languageByExtension is the one place a path becomes a language tag. Tags
// match the tree_sitter.languages spellings of docs/configuration.md so a
// provider can select files by the manifest's own column. Classification is by
// extension only: no file is opened to guess, and a path with no entry has no
// language, which is honest rather than wrong.
var languageByExtension = map[string]string{
	".go":    "go",
	".js":    "javascript",
	".mjs":   "javascript",
	".cjs":   "javascript",
	".jsx":   "javascript",
	".ts":    "typescript",
	".mts":   "typescript",
	".cts":   "typescript",
	".tsx":   "tsx",
	".py":    "python",
	".pyi":   "python",
	".java":  "java",
	".rs":    "rust",
	".c":     "c",
	".h":     "c",
	".cc":    "cpp",
	".cpp":   "cpp",
	".cxx":   "cpp",
	".hpp":   "cpp",
	".hh":    "cpp",
	".hxx":   "cpp",
	".cs":    "csharp",
	".rb":    "ruby",
	".php":   "php",
	".kt":    "kotlin",
	".kts":   "kotlin",
	".swift": "swift",
	".scala": "scala",
	".sh":    "shell",
	".bash":  "shell",
	".sql":   "sql",
	".proto": "protobuf",
	".json":  "json",
	".yaml":  "yaml",
	".yml":   "yaml",
	".toml":  "toml",
	".md":    "markdown",
	".html":  "html",
	".css":   "css",
}

// languageByBasename covers the build files that carry no extension.
var languageByBasename = map[string]string{
	"Makefile":   "make",
	"Dockerfile": "dockerfile",
}

func languageOf(rel string) string {
	base := path.Base(rel)
	if lang, ok := languageByBasename[base]; ok {
		return lang
	}
	return languageByExtension[strings.ToLower(path.Ext(base))]
}
