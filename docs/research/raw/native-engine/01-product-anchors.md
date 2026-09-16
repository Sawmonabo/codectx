# Native-engine research, raw evidence — product-side anchors (product tree at 62976df, read-only)

## Command
```
$ find internal/provider/<pkg> -name "*.go" ! -name "*_test.go" -exec cat {} + | wc -l   # non-test
$ find internal/provider/<pkg> -name "*_test.go"            -exec cat {} + | wc -l   # test
```

## The dependence provider as it stands (the JVM-engine consumer a native engine would displace)

| package | non-test Go lines | test Go lines |
|---|---|---|
| `internal/provider/dependence` (this dir only) | 2754 | 793 |
| `internal/provider/dependence/neo4jcsv` (this dir only) | 3446 | 1428 |
| `internal/provider/dependence/joern` (this dir only) | 737 | 131 |
| `internal/provider/treesitter` (this dir only) | 1936 | 502 |
| `internal/provider/treesitter/worker` (this dir only) | 990 | 0 |
| `internal/provider/scip` (this dir only) | 5686 | 1066 |
| `internal/provider/lsp` (this dir only) | 3157 | 1221 |

Recursive totals:
  internal/provider/dependence: non-test 7036, test 2352
  internal/provider/treesitter: non-test 3353, test 502
  internal/provider/scip: non-test 5700, test 1066
  internal/provider/lsp: non-test 3157, test 1221

## The tree-sitter scaffolding a native pass would ride on (the existing extension point)

```
$ ls internal/provider/treesitter/lang/queries/
c.scm
cpp.scm
ecmascript.scm
go.scm
java.scm
javascript.scm
python.scm
rust.scm
typescript.scm

$ wc -l internal/provider/treesitter/lang/queries/*
  22 internal/provider/treesitter/lang/queries/c.scm
  11 internal/provider/treesitter/lang/queries/cpp.scm
  28 internal/provider/treesitter/lang/queries/ecmascript.scm
  22 internal/provider/treesitter/lang/queries/go.scm
  21 internal/provider/treesitter/lang/queries/java.scm
   3 internal/provider/treesitter/lang/queries/javascript.scm
  15 internal/provider/treesitter/lang/queries/python.scm
  27 internal/provider/treesitter/lang/queries/rust.scm
  16 internal/provider/treesitter/lang/queries/typescript.scm
 165 total

$ wc -l internal/provider/treesitter/lang/*.go internal/provider/treesitter/worker/*.go
  140 internal/provider/treesitter/lang/lang.go
  494 internal/provider/treesitter/worker/extract.go
  281 internal/provider/treesitter/worker/grammars.go
  215 internal/provider/treesitter/worker/worker.go
 1130 total
```

### Grammars wired (the `grammar` hook table)
```
64:				case "struct_type":
66:				case "interface_type":
204:		case "function_declarator":
207:		case "pointer_declarator", "array_declarator", "init_declarator", "attributed_declarator", "reference_declarator", "parenthesized_declarator":
213:		case "qualified_identifier":
218:		case "destructor_name", "operator_name":
```

### tree-sitter binding already in the product (cgo already paid for)
```
$ grep -i "tree-sitter\|treesitter" go.mod
	github.com/tree-sitter/go-tree-sitter v0.25.0
	github.com/tree-sitter/tree-sitter-c v0.24.2
	github.com/tree-sitter/tree-sitter-cpp v0.23.4
	github.com/tree-sitter/tree-sitter-go v0.25.0
	github.com/tree-sitter/tree-sitter-java v0.23.5
	github.com/tree-sitter/tree-sitter-javascript v0.25.0
	github.com/tree-sitter/tree-sitter-python v0.25.0
	github.com/tree-sitter/tree-sitter-rust v0.24.2
	github.com/tree-sitter/tree-sitter-typescript v0.23.2

$ grep -rn "CGO\|cgo" --include=*.go internal/provider/treesitter | head -5
internal/tools/toollock/build.go:83:		"CGO_ENABLED=0",
(no direct import "C" in internal/ means the binding vendors it)
```

### Languages the product advertises
```
```

### The nine pinned grammars (internal/provider/treesitter/lang/lang.go:23, :78-90)
```
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
```
Binding: `github.com/tree-sitter/go-tree-sitter@v0.25.0` (lang.go:23) — the CST layer, cgo included,
is already a shipped, pinned product dependency for all nine languages. A native engine adds no new
parser dependency; it adds passes over a tree the product already builds per file.
