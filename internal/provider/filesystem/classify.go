package filesystem

import (
	"path"
	"strings"

	"github.com/Sawmonabo/codectx/internal/model"
)

// Classification is what a path alone says about a file: its language tag,
// the format it is recognized as, and the semantic node kind the filesystem
// provider emits for it in addition to the file node. Formats the manifest
// provider parses (go.mod, package.json, ...) carry no Node here because the
// node they define is only known after parsing; recognition-only formats
// (Dockerfile, CI, Terraform, ...) are represented as the classified node with
// no relations beyond `defines`, because inventing build or template
// evaluation is not allowed (Section 11.2).
type Classification struct {
	Language string
	Format   string
	Node     model.NodeKind
}

// Recognized formats. The manifest provider parses the first six and
// Markdown; everything else is recognition-only.
const (
	FormatGoMod       = "gomod"
	FormatGoWork      = "gowork"
	FormatPackageJSON = "packagejson"
	FormatCargo       = "cargo"
	FormatPyProject   = "pyproject"
	FormatPom         = "pom"
	FormatMarkdown    = "markdown"
)

// languageByExtension is the single path-to-language table of R7-4. The tags
// for the bundled grammars match tree_sitter.languages in
// docs/configuration.md so Task 8 selects files by the same spelling.
var languageByExtension = map[string]string{
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

// languageByBasename covers files that carry no extension.
var languageByBasename = map[string]string{
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

// Language returns the language tag for a root-relative path, or "" when the
// path says nothing. Classification is by extension and basename only: no
// file is opened to guess, and an unknown path honestly has no language.
func Language(rel string) string {
	base := path.Base(rel)
	if lang, ok := languageByBasename[base]; ok {
		return lang
	}
	return languageByExtension[strings.ToLower(path.Ext(base))]
}

// byBasename maps exact basenames to their format and node kind.
var byBasename = map[string]Classification{
	"go.mod":         {Format: FormatGoMod},
	"go.work":        {Format: FormatGoWork},
	"package.json":   {Format: FormatPackageJSON},
	"Cargo.toml":     {Format: FormatCargo},
	"pyproject.toml": {Format: FormatPyProject},
	"pom.xml":        {Format: FormatPom},

	"Dockerfile":              {Format: "dockerfile", Node: model.NodeBuildTarget},
	"Jenkinsfile":             {Format: "jenkins", Node: model.NodeConfiguration},
	".gitlab-ci.yml":          {Format: "gitlab-ci", Node: model.NodeConfiguration},
	".travis.yml":             {Format: "travis", Node: model.NodeConfiguration},
	"azure-pipelines.yml":     {Format: "azure-pipelines", Node: model.NodeConfiguration},
	"bitbucket-pipelines.yml": {Format: "bitbucket-pipelines", Node: model.NodeConfiguration},
	"kustomization.yaml":      {Format: "kustomize", Node: model.NodeConfiguration},
	"kustomization.yml":       {Format: "kustomize", Node: model.NodeConfiguration},
	"Chart.yaml":              {Format: "helm", Node: model.NodeConfiguration},
	"build.gradle":            {Format: "gradle", Node: model.NodeBuildTarget},
	"build.gradle.kts":        {Format: "gradle", Node: model.NodeBuildTarget},
	"settings.gradle":         {Format: "gradle", Node: model.NodeBuildTarget},
	"settings.gradle.kts":     {Format: "gradle", Node: model.NodeBuildTarget},
	"BUILD":                   {Format: "bazel", Node: model.NodeBuildTarget},
	"BUILD.bazel":             {Format: "bazel", Node: model.NodeBuildTarget},
	"WORKSPACE":               {Format: "bazel", Node: model.NodeBuildTarget},
	"WORKSPACE.bazel":         {Format: "bazel", Node: model.NodeBuildTarget},
	"MODULE.bazel":            {Format: "bazel", Node: model.NodeBuildTarget},
	"CMakeLists.txt":          {Format: "cmake", Node: model.NodeBuildTarget},
	"Makefile":                {Format: "make", Node: model.NodeBuildTarget},
	"makefile":                {Format: "make", Node: model.NodeBuildTarget},
	"GNUmakefile":             {Format: "make", Node: model.NodeBuildTarget},
}

// byExtension maps lowercase extensions to their format and node kind.
var byExtension = map[string]Classification{
	".md":         {Format: FormatMarkdown, Node: model.NodeDocument},
	".markdown":   {Format: FormatMarkdown, Node: model.NodeDocument},
	".rst":        {Format: "rst", Node: model.NodeDocument},
	".adoc":       {Format: "asciidoc", Node: model.NodeDocument},
	".asciidoc":   {Format: "asciidoc", Node: model.NodeDocument},
	".txt":        {Format: "text", Node: model.NodeDocument},
	".graphql":    {Format: "graphql", Node: model.NodeDocument},
	".gql":        {Format: "graphql", Node: model.NodeDocument},
	".graphqls":   {Format: "graphql", Node: model.NodeDocument},
	".proto":      {Format: "protobuf", Node: model.NodeDocument},
	".sql":        {Format: "sql", Node: model.NodeDocument},
	".tf":         {Format: "terraform", Node: model.NodeConfiguration},
	".tfvars":     {Format: "terraform", Node: model.NodeConfiguration},
	".bzl":        {Format: "bazel", Node: model.NodeBuildTarget},
	".cmake":      {Format: "cmake", Node: model.NodeBuildTarget},
	".mk":         {Format: "make", Node: model.NodeBuildTarget},
	".dockerfile": {Format: "dockerfile", Node: model.NodeBuildTarget},
}

// Classify reports what a root-relative path is recognized as. It is a pure
// function of the path.
func Classify(rel string) Classification {
	base := path.Base(rel)
	dir := path.Dir(rel)
	lower := strings.ToLower(base)
	ext := strings.ToLower(path.Ext(base))
	var c Classification
	switch {
	case byBasename[base].Format != "":
		c = byBasename[base]
	case strings.HasPrefix(base, "Dockerfile."):
		c = Classification{Format: "dockerfile", Node: model.NodeBuildTarget}
	case (dir == ".github/workflows") && (ext == ".yml" || ext == ".yaml"):
		c = Classification{Format: "github-actions", Node: model.NodeConfiguration}
	case dir == ".circleci" && base == "config.yml":
		c = Classification{Format: "circleci", Node: model.NodeConfiguration}
	case isCompose(lower):
		c = Classification{Format: "compose", Node: model.NodeConfiguration}
	case isOpenAPI(lower):
		c = Classification{Format: "openapi", Node: model.NodeDocument}
	case byExtension[ext].Format != "":
		c = byExtension[ext]
		if c.Format == FormatMarkdown && inADRDirectory(dir) {
			// An architecture decision record is Markdown that the manifest
			// provider still scans; the format records what it is.
			c.Format = "adr"
		}
	case strings.HasPrefix(lower, "readme") || strings.HasPrefix(lower, "changelog") ||
		strings.HasPrefix(lower, "contributing") || strings.HasPrefix(lower, "license") ||
		strings.HasPrefix(lower, "notice") || strings.HasPrefix(lower, "authors"):
		c = Classification{Format: "text", Node: model.NodeDocument}
	}
	c.Language = Language(rel)
	return c
}

func isCompose(lower string) bool {
	if lower != "compose.yml" && lower != "compose.yaml" && !strings.HasPrefix(lower, "docker-compose") {
		return false
	}
	return strings.HasSuffix(lower, ".yml") || strings.HasSuffix(lower, ".yaml")
}

func isOpenAPI(lower string) bool {
	stem, ext, _ := strings.Cut(lower, ".")
	if stem != "openapi" && stem != "swagger" {
		return false
	}
	return ext == "yaml" || ext == "yml" || ext == "json"
}

// inADRDirectory reports whether some component of dir is "adr" or "adrs".
func inADRDirectory(dir string) bool {
	for _, part := range strings.Split(dir, "/") {
		if strings.EqualFold(part, "adr") || strings.EqualFold(part, "adrs") {
			return true
		}
	}
	return false
}

// IsReadme reports whether a basename is a README-style file: the document
// whose convention is to describe its containing directory.
func IsReadme(rel string) bool {
	return strings.HasPrefix(strings.ToLower(path.Base(rel)), "readme")
}
