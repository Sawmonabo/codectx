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
| github.com/modelcontextprotocol/go-sdk | v1.7.0 | Apache-2.0 (`LICENSE`) | The MCP server of Section 19: framing, dispatch and tool registration. The license records an in-progress MIT-to-Apache-2.0 transition, so contributions whose authors have not consented to relicensing stay MIT; both are permissive and neither restricts commercial use. |
| github.com/google/jsonschema-go | v0.4.3 | MIT (`LICENSE`) | JSON Schema inference and validation for the tool schemas; required by the MCP SDK. Copyright 2025 JSON Schema Go Project Authors. |
| github.com/segmentio/encoding | v0.5.4 | MIT (`LICENSE`) | JSON codec used by the MCP SDK. Copyright 2019 Segment.io, Inc. |
| github.com/segmentio/asm | v1.1.3 | MIT (`LICENSE`) | SIMD helpers; required by segmentio/encoding. Copyright 2021 Segment. |
| github.com/yosida95/uritemplate/v3 | v3.0.2 | BSD-3-Clause (`LICENSE`) | RFC 6570 URI templates; required by the MCP SDK's resource templates. Copyright 2016 Kohei Yoshida. |
| golang.org/x/oauth2 | v0.35.0 | BSD-3-Clause (`LICENSE`) | Reached through the MCP SDK's module graph. codectx registers no HTTP, auth or remote transport (Section 19.3) and imports only the SDK's `mcp` package, so no OAuth code path is reachable from this binary. Copyright 2009 The Go Authors. |
| golang.org/x/sync | v0.22.0 | BSD-3-Clause (`LICENSE`) | Concurrency primitives used by the MCP SDK. Copyright 2009 The Go Authors. |
| golang.org/x/time | v0.15.0 | BSD-3-Clause (`LICENSE`) | Rate limiting used by the MCP SDK. Copyright 2009 The Go Authors. |

The Go standard library and toolchain are covered by the Go project's
BSD-3-Clause license and are not redistributed as source by this repository.

The Tree-sitter grammars above are compiled into the parser worker from their
Go modules (each module carries the grammar's `parser.c`); the query packs under
`internal/provider/treesitter/lang/queries/` are codectx's own and are covered
by this repository's license. See `docs/providers-treesitter.md` for the pinned
ABI and grammar metadata that the unit fingerprint folds in.

## Pinned analyzer payloads

The managed analyzer toolchain of Section 11.7 pins one external analyzer or
runtime per lock entry (`docs/toolchain.md`). Most are pinned at the upstream
publisher's own asset URL; `gopls`, the four Node-hosted analyzers, and the
three platforms of `scip-go` upstream does not build have no upstream binary and
are built at release time and redistributed as assets of the `tools-v<n>`
release. Either way the payload is **not linked into the
binary**: the product downloads the one its lock names, verifies its SHA-256
and size, and runs it as a separate process. Every payload keeps its upstream
license file inside the archive; the table below is the inventory the lock also
records in each entry's `license` field, and the `Hosting` column says which of
the two the bytes come from.

| Payload | Version | License | Hosting | Upstream |
|---|---|---|---|---|
| `node` | 22.23.2 | MIT (Node.js core) with the bundled-component licenses in `LICENSE` (ICU under Unicode-DFS-2016, OpenSSL under Apache-2.0, zlib, libuv and others) | upstream | https://nodejs.org/dist/v22.23.2/ |
| `jdk` (Eclipse Temurin) | 21.0.12.1+1 | GPL-2.0-only WITH Classpath-exception-2.0 | upstream | https://adoptium.net/temurin/releases/?version=21 |
| `scip-go` | 0.2.7 | Apache-2.0 | upstream (linux amd64/arm64, darwin arm64); redistributed (darwin amd64, windows amd64/arm64) | https://github.com/sourcegraph/scip-go |
| `scip-typescript` | 0.4.0 | Apache-2.0 | redistributed | https://www.npmjs.com/package/@sourcegraph/scip-typescript |
| `scip-python` | 0.6.6 | MIT (the package vendors pyright) | redistributed | https://www.npmjs.com/package/@sourcegraph/scip-python |
| `scip-java` | 0.13.1 | Apache-2.0 | upstream | https://github.com/sourcegraph/scip-java |
| `rust-analyzer` | 2026-08-17.4 | MIT OR Apache-2.0 | upstream | https://github.com/rust-lang/rust-analyzer |
| `scip-clang` | 0.4.0 | Apache-2.0 | upstream | https://github.com/sourcegraph/scip-clang |
| `gopls` | 0.23.0 | BSD-3-Clause (the Go project) | redistributed | https://pkg.go.dev/golang.org/x/tools/gopls |
| `typescript-language-server` | 6.0.0 | Apache-2.0 | redistributed | https://www.npmjs.com/package/typescript-language-server |
| `typescript` (shipped inside the `typescript-language-server` payload) | 5.9.3 | Apache-2.0 | redistributed | https://www.npmjs.com/package/typescript |
| `ty` | 0.0.81 | MIT | upstream | https://github.com/astral-sh/ty |
| `clangd` | 22.1.6 | Apache-2.0 WITH LLVM-exception | upstream | https://github.com/clangd/clangd |
| `jdtls` (Eclipse JDT Language Server) | 1.61.0 | EPL-2.0 | upstream | https://download.eclipse.org/jdtls/milestones/1.61.0/ |
| `joern` (backend of the `dependence` provider) | 4.0.627 | Apache-2.0 | upstream | https://github.com/joernio/joern |

The npm payloads are produced with `npm ci --omit=dev --omit=optional
--ignore-scripts`, so each carries its resolved production dependency tree and
those packages' own licenses under `node_modules/`. None is copyleft beyond the
weak-copyleft entries named above (`jdk` under the Classpath Exception and
`jdtls` under EPL-2.0), and neither imposes an obligation on a program that
merely executes it as a separate process.

"Redistributed" payloads are the ones this project publishes; "upstream" ones
are fetched from the publisher's own release and are redistributed by nobody
here. Regenerate the inventory alongside the lock with:

```bash
go run ./internal/tools/toollock -licenses /dev/stdout -no-upload
```
