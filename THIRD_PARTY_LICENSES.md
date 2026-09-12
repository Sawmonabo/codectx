# Third-party licenses

codectx is licensed under the Apache License 2.0 (see `LICENSE`). The binary
links the components below. The list is the module set reported by
`go list -deps ./cmd/codectx`, not the full `go list -m all` module graph, so it
excludes modules that only take part in dependency resolution or in a
dependency's own tooling. Each license was read from the module cache at the
pinned version; none is copyleft or carries a commercial-use restriction.

Regenerate the module set with:

```bash
go list -deps -f '{{with .Module}}{{.Path}} {{.Version}} {{.Dir}}{{end}}' ./cmd/codectx | sort -u
GOOS=windows go list -deps -f '{{with .Module}}{{.Path}} {{.Version}}{{end}}' ./cmd/codectx | sort -u
```

| Module | Version | License | Notes |
|---|---|---|---|
| github.com/spf13/cobra | v1.10.2 | Apache-2.0 (`LICENSE.txt`) | Command parsing. |
| github.com/spf13/pflag | v1.0.9 | BSD-3-Clause (`LICENSE`) | Flag parsing; required by cobra. Copyright 2012 Alex Ogier and The Go Authors. |
| github.com/inconshreveable/mousetrap | v1.1.0 | Apache-2.0 (`LICENSE`) | Windows-only; linked into the Windows targets only. Copyright 2022 Alan Shreve. |
| github.com/tree-sitter/go-tree-sitter | v0.25.0 | MIT (`LICENSE`) | Tree-sitter binding; bundles the tree-sitter runtime (MIT, Max Brunsfeld). Amaan Qureshi and tree-sitter contributors. Linked through the parser worker. |
| github.com/mattn/go-pointer | v0.0.1 | MIT (`LICENSE`) | Binding dependency. Yasuhiro Matsumoto. |
| github.com/tree-sitter/tree-sitter-c | v0.24.2 | MIT (`LICENSE`) | Grammar; native parser source compiled in. |
| github.com/tree-sitter/tree-sitter-cpp | v0.23.4 | MIT (`LICENSE`) | Grammar. |
| github.com/tree-sitter/tree-sitter-go | v0.25.0 | MIT (`LICENSE`) | Grammar. |
| github.com/tree-sitter/tree-sitter-java | v0.23.5 | MIT (`LICENSE`) | Grammar. |
| github.com/tree-sitter/tree-sitter-javascript | v0.25.0 | MIT (`LICENSE`) | Grammar. |
| github.com/tree-sitter/tree-sitter-python | v0.25.0 | MIT (`LICENSE`) | Grammar. |
| github.com/tree-sitter/tree-sitter-rust | v0.24.2 | MIT (`LICENSE`) | Grammar. |
| github.com/tree-sitter/tree-sitter-typescript | v0.23.2 | MIT (`LICENSE`) | Grammar (TypeScript and TSX). Grammar authors: Max Brunsfeld, Ayman Nadeem, Maxim Sokolov and tree-sitter contributors. |

The Go standard library and toolchain are covered by the Go project's
BSD-3-Clause license and are not redistributed as source by this repository.

The Tree-sitter grammars above are compiled into the parser worker from their
Go modules (each module carries the grammar's `parser.c`); the query packs under
`internal/provider/treesitter/lang/queries/` are codectx's own and are covered
by this repository's license. See `docs/providers-treesitter.md` for the pinned
ABI and grammar metadata that the unit fingerprint folds in.
