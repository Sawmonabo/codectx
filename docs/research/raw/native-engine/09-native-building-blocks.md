# Native-engine research, raw evidence — native building blocks

Method: every row fetched over the network with WebFetch/WebSearch on 2026-09-16. Nothing was
cloned or installed — the Joern clone was this lane's one permitted download. Licence and
last-activity are as observed at fetch time.

| project | URL | language | licence (observed) | last activity (observed) | what it gives | usable from Go |
|---|---|---|---|---|---|---|
| github/stack-graphs | https://github.com/github/stack-graphs | Rust | `MIT OR Apache-2.0` (crate); repo API `Apache-2.0` | **ARCHIVED 2025-09-09**; crate 0.14.1 @ 2024-12-13 | name-resolution rules only; README has no CFG/dataflow/control-dependence | SUBPROCESS (CLI) / REFERENCE ONLY |
| tree-sitter/tree-sitter-graph | https://github.com/tree-sitter/tree-sitter-graph | Rust | `MIT OR Apache-2.0` | not archived, last push **2024-12-11**; crate 0.12.0 same day (dormant ~21 mo) | graph-construction DSL over a parse tree; zero CFG/dominator terms | REFERENCE ONLY |
| semgrep/semgrep (CE) | https://github.com/semgrep/semgrep | OCaml | `LGPL-2.1`; registry rules are "Semgrep Rules License v.1.0 … internal business use" | v1.177.0 @ 2026-09-10 | generic `CFG_build.ml` + IL; **CE taint is intraprocedural by design** | REFERENCE ONLY |
| x/tools `go/ssa` + `go/callgraph` | https://pkg.go.dev/golang.org/x/tools/go/ssa | Go | `BSD-3-Clause` | v0.50.0 @ 2026-09-08 | SSA + CFG (`Preds`/`Succs`) + **forward** dom tree (`Idom`/`Dominees`/`Dominates`); callgraph static/cha/rta/vta | **LIBRARY** (Go family only) |
| x/tools `go/cfg` | https://pkg.go.dev/golang.org/x/tools/go/cfg | Go | `BSD-3-Clause` | v0.50.0 @ 2026-09-08 | public (not internal) syntactic per-func CFG; 20 `BlockKind`s incl. `KindIfThen`; no branch conditions, no panic edges, no dominators | **LIBRARY** (Go only) |
| github/codeql (QL libs) | https://github.com/github/codeql | QL | `MIT` | push 2026-09-16 | per-language CFG algorithms, readable | REFERENCE ONLY |
| CodeQL CLI + extractors | https://github.com/github/codeql-cli-binaries/blob/main/LICENSE.md | — | "GitHub CodeQL Terms and Conditions" (proprietary) | n/a | DB generation | **NOT USABLE** |
| Fraunhofer-AISEC/cpg | https://github.com/Fraunhofer-AISEC/cpg | Kotlin/JVM | `Apache-2.0` | push 2026-09-16 | full CPG incl. EOG/CFG; 1,540-line generic pass, 125-line Go delta | REFERENCE ONLY |
| joernio/flatgraph | https://github.com/joernio/flatgraph | Scala/JVM | `Apache-2.0` | push 2026-09-15 | Joern's **storage** layer, not an analysis | REFERENCE ONLY |
| mozilla/rust-code-analysis | https://github.com/mozilla/rust-code-analysis | Rust | `MPL-2.0` (grammars MIT) | push 2026-04-06 | maintainability metrics; no CFG/dataflow/dominator in README | REFERENCE ONLY |
| ast-grep/ast-grep | https://github.com/ast-grep/ast-grep | Rust | `MIT` | push 2026-09-16 | structural search/rewrite; syntax only | REFERENCE ONLY |
| rust-analyzer (`ra_ap_*`) | https://crates.io/crates/ra_ap_hir | Rust | `MIT OR Apache-2.0` | ra_ap_hir 0.0.352 @ 2026-09-14 | Rust name resolution + types; no published CFG/CDG API | REFERENCE ONLY |
| facebookincubator/Glean | https://github.com/facebookincubator/Glean | Hack/Haskell | `BSD-3-Clause` (repo LICENSE; API says NOASSERTION) | push 2026-09-16 | fact store + query over facts others produce | REFERENCE ONLY |
| obi1kenobi/trustfall | https://github.com/obi1kenobi/trustfall | Rust | `Apache-2.0` | push 2026-09-14 | query engine over adapters; no CFG of its own | REFERENCE ONLY |
| tree-sitter/go-tree-sitter | https://github.com/tree-sitter/go-tree-sitter | Go + cgo | `MIT` | push 2025-11-16; **codectx already pins v0.25.0** | official Go bindings, the current successor | **LIBRARY (in use)** |
| smacker/go-tree-sitter | https://github.com/smacker/go-tree-sitter | Go + cgo | `MIT` | push 2024-08-27 (dormant ~2 yr) | older community bindings | SUPERSEDED |
| honnef.co/go/tools/go/ir | https://pkg.go.dev/honnef.co/go/tools/go/ir | Go | `MIT` | v0.8.1 @ 2026-08-21 | staticcheck's SSA IR: CFG + dom tree; **no post-dominator, no dominance frontier** in the index | **LIBRARY** (Go only) |
| gonum.org/v1/gonum/graph/flow | https://pkg.go.dev/gonum.org/v1/gonum/graph/flow | Go | `BSD-3-Clause` | v0.17.0 @ 2025-12-29 | `Dominators` / `DominatorsSLT` (Lengauer-Tarjan) on any `graph.Directed`; tree exposes only Root/DominatorOf/DominatedBy — no frontier | **LIBRARY** (language-agnostic) |
| alon.kr/x/graph | https://pkg.go.dev/alon.kr/x/graph | Go | **`GPL-3.0`** | v0.0.0-20250319… @ 2025-03-19 | the only Go `DominatorFrontier` found | **NOT USABLE** (licence) |

**Available to import rather than write (3) — superseded by the decision in
`18-algorithms-dominance-and-dataflow.md` §1, which writes the dominator core in the repository:** `gonum.org/v1/gonum/graph/flow` — BSD-3, Lengauer-Tarjan over any
directed graph, so it is language-agnostic and serves all six families;
`github.com/tree-sitter/go-tree-sitter` — already pinned at v0.25.0 in this worktree's `go.mod`, so
the smacker-vs-official question is settled for us; and, for the Go family alone, `x/tools`
`go/ssa`+`go/callgraph` (BSD-3) or `honnef.co/go/tools/go/ir` (MIT).

**The verified negative, correctly qualified:** no permissively-licensed Go package in this set
exposes post-dominators or a dominance frontier **in its public API**. That is not the same as "no
source exists": `go/ssa`'s `lift.go:99-106` computes a Cytron dominance frontier over the recurrence
at `:79-97`, unexported, in about 28 lines of BSD-3 source that can be ported rather than imported
(`18-algorithms-dominance-and-dataflow.md`). The build is sized against porting, not against
inventing. `go/ssa` and honnef `ir` both stop at the
forward dominator tree; gonum stops at `DominatorOf`/`DominatedBy`. The one Go package that does
compute a frontier is GPL-3.0 and single-author v0.0.0. So the must-write list is exactly:
post-dominator tree, dominance frontier, Ferrante control dependence — on top of gonum's dominator
core. (Running gonum on reversed CFG edges to obtain post-dominators was this note's inference from the
generic `graph.Directed` signature. It is now **verified from the module cache**: both entry points
consult exactly one `graph.Directed` method, `From`, so a reversed view is sound
— `18-algorithms-dominance-and-dataflow.md` §2 carries the proof and the exit-node requirement.
**The decision taken there is nonetheless to write Cooper-Harvey-Kennedy in the repository rather than
import gonum**, so the shared core adds **no module at all**; the rows below record what was available,
not what is taken.)

**Looks usable, isn't:** stack-graphs is archived *and* its `stack-graphs/Cargo.toml` declares no
`cdylib`/`staticlib` — there is no C ABI to cgo against, so the only route is shelling out to the
`tree-sitter-stack-graphs` CLI, i.e. a subprocess against an archived binary; it also gives name
resolution only (the `calls` family), never control dependence. tree-sitter-graph is a DSL, not an
analysis, and dormant 21 months. Semgrep is ruled out by language (OCaml) before licence, and its CE
taint is intraprocedural-only — the same scope codectx already imports, so a subprocess buys no
capability. CodeQL's MIT QL libraries are readable but the CLI/extractors bar redistribution,
hosting, and automated analysis of private code without GHAS. Fraunhofer `cpg` and flatgraph are
Apache-2.0 but JVM — either one reinstates the runtime profile the lane is leaving; `cpg` stays the
best per-language CFG-delta anchor.

**UNVERIFIED:** `syn` and `rustc_mir`/rustc internals were not fetched (budget); the "no stable
out-of-rustc MIR API" claim is therefore unchecked. GitHub's API reports `github/stack-graphs` as
Apache-2.0 while the crate manifest says `MIT OR Apache-2.0` — both Apache-2.0-compatible, so the
discrepancy is immaterial.

**Correction this supersedes:** `docs/research/05-cfg-cdg-from-treesitter.md` §3 treats
`stack-graphs` as a live-but-inapplicable project. It was **archived on 2025-09-09**. The verdict
("not applicable") is unchanged; the reason is now stronger.

## What a native engine would add to `go.mod` (measured against the current file)

Current direct dependencies (`go.mod`, 45 lines):
BurntSushi/toml, fsnotify, google/jsonschema-go, modelcontextprotocol/go-sdk, spf13/cobra,
tree-sitter/go-tree-sitter + eight grammars, golang.org/x/mod, golang.org/x/sys, modernc.org/libc,
modernc.org/sqlite. **Neither gonum, nor golang.org/x/tools, nor honnef.co/go/tools is present.**

| addition | version to pin | licence | why |
|---|---|---|---|
| `gonum.org/v1/gonum/graph/flow` | v0.17.0 | BSD-3-Clause | Lengauer-Tarjan over any `graph.Directed`, one dependency for all six families |
| `golang.org/x/tools` (`go/ssa`, `go/callgraph`) | v0.50.0 | BSD-3-Clause | **optional**, Go family only, for a compiler-precision Go call graph |

Nothing else. The tree-sitter CST layer, cgo and all nine grammars are **already pinned**, so the
native engine adds no parser, no runtime, no helper binary and no per-language toolchain — which is
precisely the opposite of the engine, whose six frontends drive Eclipse CDT, javaparser's symbol
solver, @joernio/astgen, goastgen and a Rust helper, plus a JDK.

No-paid-dependency rule (policy.md): both additions are free, offline, permissively licensed and
pinnable to an exact version. Neither reaches the network at runtime.
