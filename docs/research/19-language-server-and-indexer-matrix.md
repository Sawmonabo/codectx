# Language servers and SCIP indexers: was the pinned set researched, and is it still right?

Research lane TOOLS-R, 2026-09-15, linux/amd64. Measurements use the tools already in
`~/.local/share/codectx/tools/` plus three candidates installed into a scratch prefix and
deleted afterwards; nothing was written into the tool store.

## 0. The honest answer to "was it researched?"

**The indexers were; the servers were not.** Research 08 [1] compared all six pinned SCIP
indexers on one fixture shape (roles, determinism, range encoding) and research 12 [2] measured
their whole-unit wall/RSS and smallest sound re-index granularity — both real bake-offs.
Research 12 §4 also examined the six **already-pinned** servers, but only for two questions
(how each stays incremental, and whether it can export bulk facts) and concluded correctly that
LSP must stay a live overlay. **No document compares a pinned server against an alternative,
and no ADR records a server choice.** `docs/toolchain.md` justifies *versions* (`node` 22
because `typescript-language-server` 6.0.0 declares `engines.node >= 22.22.2`; `typescript`
5.9.3 because 7.x drops `tsserver.js`) but never the *candidate*. Python got pyright because
scip-python is a vendored pyright [3] — a pairing inherited from the indexer, not a measured
decision. The plan's §11.7 table is the same shape: it records `pyright | server | python |
node | npm pyright | all six` and never says why that row is pyright. This note is that
comparison.

## 1. What codectx actually asks a server for

From `docs/providers-lsp.md`: seven operations — `textDocument/definition`, `typeDefinition`,
`implementation`, `references` (with `includeDeclaration`), `documentSymbol`,
`workspace/symbol`, and `callHierarchy` prepare + incoming + outgoing. Nothing else: no
completion, no diagnostics, no formatting, no `workspace/executeCommand`, no `applyEdit`. Four
unusual properties of the session shape decide which server fits:

1. **Every session is cold.** The server starts lazily against a *private materialization of a
   pinned snapshot*, `max_servers` defaults to 1, and the idle TTL stops it after the last
   overlay closes — no warm editor process accumulates an index across a workday. A server
   needing a background-indexing window before `references` is complete is *wrong for a first
   question*, not merely slow.
2. **Documents never change.** `didChange` is never sent, so a server's incremental-edit
   machinery is dead weight; only cold-start cost matters.
3. **Ranges must validate against the pinned bytes**, and the client offers `utf-8`, `utf-32`,
   `utf-16` in that order; a server omitting `positionEncoding` is used at the utf-16 default.
4. **`serverInfo.version` is provenance.** A server answering `initialize` with no `serverInfo`
   forces the fallback to the lock's pinned version — pyright and typescript-language-server
   both do (re-confirmed below).

On the SCIP side (`docs/providers-scip.md`) the importer consumes only `Document.relative_path`
+ `text`, `Occurrence` (`range`/`typed_range`, `symbol`, `symbol_roles`) and
`SymbolInformation` + `Relationship`, streamed in three passes with a per-document canonical
hash driving the delta — so any tool emitting conformant SCIP at document granularity is
substitutable.

## 2. Local measurements

Method: a JSON-RPC driver does `initialize` → `initialized` → `didOpen` → **one**
`textDocument/references` (`includeDeclaration: true`) → `shutdown` → `exit`, under `(ulimit -v
6000000)`, with `/usr/bin/time -v` wrapping the **server process itself**. `+Ns` rows sleep N
seconds after `didOpen` before asking, giving a background indexer its window. Client
capabilities declare implementation, typeDefinition and callHierarchy (a thinner client makes
typescript-language-server withhold `callHierarchyProvider` — worth knowing).

**Python — `~/repos/m32rimm`, 11,267 `.py` outside `.venv`; one class, `AimProductRequests`,
textually present in 104 files.**

| Server | settle | `references` | results | server peak RSS | caps (D T I R Ds Ws Ch) |
|---|---|---|---|---|---|
| pyright 1.1.414 (pinned) | cold | 1.43 s | **2** | 267 MB | D T **–** R Ds Ws Ch |
| pyright 1.1.414 | +15 s | 0.23 s | 138 | 290 MB | same |
| ty 0.0.81 | **cold** | **0.21 s** | **136** | **168 MB** | all seven |
| ty 0.0.81 | +15 s | 0.05 s | 136 | 166 MB | all seven |
| pyrefly 1.3.1 | cold | 0.20 s | **2** | 487 MB | all seven |
| pyrefly 1.3.1 | +15 s | 0.00 s | 136 | 541 MB | all seven |

Replicated on a smaller corpus (`~/repos/ai-foundations`, 2,379 `.py`; RSS is the
driver-plus-server tree, overstating by ~15 MB): cold pyright 1.02 s / **90** / 212 MB, cold ty
0.21 s / **93** / 101 MB, cold pyrefly 0.38 s / **90** / 1,108 MB. The ordering holds; the
completeness gap does not appear because the queried class sits in the opened file's own import
closure — the condition that fails on m32rimm.

**MEASURED.** The decisive result of this lane. On a cold session — the only kind codectx has —
pyright and pyrefly answer **2 of ~136** references; ty answers the full set in 0.21 s at 168
MB. pyright needs a settle window (0 s → 2, 15 s → 138) because its `references` walks the
*program*, i.e. the opened file's import closure, until background workspace indexing widens
it. pyright also never advertises `implementationProvider`, which is why
`docs/providers-lsp.md` records python as "all but implementations". The 138 vs 136
disagreement is unexplained and needs adjudicating before a swap.

**TypeScript/JavaScript — `~/repos/r3/app`, 4,513 `.js`, Meteor, no `tsconfig.json`.**

| Server | settle | `references` | results | server peak RSS | caps |
|---|---|---|---|---|---|
| typescript-language-server 6.0.0 + TS 5.9.3 (pinned) | cold | 0.45 s | 2 | 68 MB | all seven |
| typescript-language-server 6.0.0 | +30 s | 0.01 s | 2 | 68 MB | all seven |
| tsgo 7.0.0-dev.20260707.2 (`--lsp`) | cold | 0.06 s | 2 | not captured¹ | all seven |

**MEASURED.** Identical answers; tsgo is ~7× faster on the first request and reports
`serverInfo` and `positionEncoding: utf-16`, which the pinned server does not. ¹ tsgo does not
exit on `exit` within the wait, so `rusage` was not collected — treat its RSS as unmeasured.
Note the repository shape: with no `tsconfig.json`, tsserver builds an *inferred project* and
both servers scope references to the open file's import closure — a real limitation on JS
monorepos, but not one a swap fixes.

## 3. Candidate matrix

Legend for "codectx's seven": Def / TypeDef / Impl / Refs / DocSym / WsSym / CallHier.

### Python

| Candidate | Version (2026-09-15) | Status | Impl. lang | Seven | Cold-session refs | Memory / latency | Incremental | License | Distribution | Known gaps |
|---|---|---|---|---|---|---|---|---|---|---|
| **pyright** (pinned) | 1.1.414, 2026-09-09 | stable, active | TypeScript/Node | 6 of 7 — no `implementation` (measured) | **incomplete (2 of 136)** | 267–290 MB, 0.23 s warm | in-memory only; `_markFileDirtyRecursive` over `importedBy`; no cache dir [2] | MIT [4] | npm; needs the pinned Node | no `serverInfo`; no implementations; refs need a settle window; untyped code is inferred but unannotated params fall back to `Unknown` |
| **ty** | 0.0.81, 2026-09-15 | **0.0.x, no stable API; 1.0 targeted for 2026** [5][6] | Rust | **7 of 7, implementations verified** | **complete (136) in 0.21 s** | **168 MB** | salsa-style; built around incrementality [6] | MIT | single static binary, 20 targets each with a `.sha256` sidecar (covers all six codectx platforms) + PyPI wheels; **no runtime** | vendor docs still list `textDocument/implementation` as unsupported (#3514) [7], but 0.0.81 advertises **and answers** it correctly — measured on a 3-class fixture, `class Base` returns Base + both subclasses and `Base.run` returns all three overrides, so the docs are stale. Real risk is churn: breaking changes between any two 0.0.x releases [5] |
| **basedpyright** | 1.40.1 (= pyright 1.1.414), 2026-09-10 | stable, active fork | TypeScript/Node | same as pyright | same as pyright (same engine) | same as pyright | same as pyright | MIT [4] | npm **and** PyPI (bundles Node) | superset of pyright's diagnostics + open-source re-implementations of Pylance-only LSP features [8]; **none of the added features are in codectx's seven**, and it inherits every pyright gap above |
| **pyrefly** | 1.3.1, 2026-09-15 | **stable (1.0 shipped)**, Instagram's default, PyTorch/JAX adopters [9] | Rust | 7 of 7 [9] | **incomplete (2 of 136)** | **487–541 MB** — the heaviest of the three | incremental; vendor claims 2–125× faster post-save diagnostics, 40–60 % less memory *than its own beta* [9] (marketing) | MIT | single static binary, 9 platform archives each with a `.sha256` sidecar; PyPI wheels; no runtime | cold-session refs as bad as pyright's, at 2–3× the RSS; highest typing-spec conformance of the four (97 %+, vendor) [9] |
| **jedi-language-server** | 0.47.0, 2026-05-31 | stable but low activity | Python | partial (no call hierarchy) | n/a | — | none | MIT | PyPI only | **needs a CPython runtime codectx does not pin** — a third managed runtime, ~100 MB/platform. Heuristic (non-type-checking) resolution. Disqualified on distribution alone |
| **python-lsp-server** | 1.14.0, 2025-12-06 | stable, slow cadence | Python | partial, plugin-dependent | n/a | — | none | MIT | PyPI only | same runtime disqualifier; references come from rope/jedi plugins, not a type checker |

### TypeScript / JavaScript

| Candidate | Version | Status | Impl. lang | Seven | Memory / latency | Incremental | License | Distribution | Known gaps |
|---|---|---|---|---|---|---|---|---|---|
| **typescript-language-server 6.0.0 + typescript 5.9.3** (pinned) | 6.0.0, 2026-08-20 | stable, active | TypeScript/Node | all seven (measured, when the client declares them) | 68 MB, 0.45 s cold | in-memory only; `.tsbuildinfo` is a `tsc` feature, absent from tsserver [2] | MIT + MS-licensed vendored parts [10] | npm ×2 (server + `typescript`); needs the pinned Node | no `serverInfo`, no `positionEncoding`; `findReferences` is O(all files in the program) [2]; on a repo with no `tsconfig.json`, references are import-closure-scoped |
| **tsgo / TypeScript 7 native LSP** | 7.0.2 stable (2026-08-20); `@typescript/native-preview` 7.0.0-dev.20260707.2 | **7.0 is the stable `typescript` package**, but it "does not ship with an API" — 7.1 is to ship a new one [11] | Go | all seven — handler + capability list read from `typescript-go` at `typescript/v7.0.2`, including `workspace/symbol` and call hierarchy [12] | 0.06 s cold (7× faster); RSS not captured | new engine, per-project | Apache-2.0 [12] | `typescript` 7.0.2 on npm is a thin shim over 20 per-platform optional deps; only `bin/tsc` is exposed, and `@typescript/native-preview` is the preview channel that exposes `tsgo` | the staging repo `microsoft/typescript-go` is **archived** as of 2026-08-31 (development moved in-tree); the no-API gap blocks Vue/Svelte/Astro/MDX toolchains [11]; the *preview* npm package is still `-dev.` tagged, so there is no stable, pinnable binary with a published digest yet |
| **vtsls** | `@vtsls/language-server` 0.3.0 (npm), last release 2025-12-24 | stale — 9 months with no release | TypeScript/Node | superset of tsls (it wraps VS Code's own TS extension) | not measured | same as tsserver | MIT [13] | npm; needs Node | a thicker wrapper over the *same* tsserver, so it fixes none of the measured gaps and adds a stale dependency. No reason to prefer it |

### Go, Rust, Java, C/C++

| Language | Pinned | Latest upstream | Alternatives | Verdict |
|---|---|---|---|---|
| **Go** | `gopls` 0.23.0 | **0.23.0** — pin is current | none credible; the reference implementation and the only Go server with a whole-workspace reference index. Also the only server in the set with a **persistent on-disk cache** (`$GOPLSCACHE`, 1 GB soft budget, 5-day max age, keyed by a digest of the value's recipe) [2] | keep, unconditionally |
| **Rust** | `rust-analyzer` 2026-08-17.4 | 2026-09-14 (weekly train) — pin is ~4 weeks behind on the same line | none; it is simultaneously the only Rust server and, via `rust-analyzer scip`, the only Rust SCIP emitter. "Keeps all input data in memory and never does any IO"; persistent caching is issue #4712, open since 2020 [2] | keep; consider advancing the pin |
| **Java** | `jdtls` 1.61.0 | **1.61.0** — newest milestone | `georgewfraser/java-language-server` (MIT, 814 stars, pushed 2026-09-13) is the only javac-based alternative: no Gradle project model, much smaller feature surface, no ecosystem. jdtls is the de-facto standard every other Java tool builds on | keep. The cost is real — a JVM plus an Equinox workspace jdtls *writes* into `work_dir` — and `docs/providers-lsp.md` already documents it |
| **C/C++** | `clangd` 22.1.6 | **22.1.6** (2026-05-28) — pin is current | `ccls` 0.20250815.1 (Apache-2.0, last release 2025-11-15): historically indexed the whole project up front where clangd parses on open, but clangd has overtaken it on LSP coverage and maintenance velocity [14] | keep. clangd's `.cache/clangd/index/` is content-hash incremental and the richest persisted index in the set, though its on-disk format is explicitly "experimental… don't rely on it" [2] |

## 4. The SCIP side: is there a better indexer per language?

Every lock entry is at the newest upstream release (scip-go 0.2.7, scip-typescript 0.4.0,
scip-python 0.6.6, scip-java 0.13.1, scip-clang 0.4.0, rust-analyzer's `scip`). The published
indexer ecosystem [15] adds only `scip-dart`, `scip-dotnet`, `scip-php` and `scip-ruby` —
languages codectx does not index.

- **Python.** Neither ty nor pyrefly mentions SCIP anywhere in its README, and neither ships a
  batch index subcommand — `ty` has exactly `check`, `server`, `version`, `explain` (verified
  locally). **No replacement for scip-python exists.** Its cost is the vendored pyright: 84.66
  s / 2,947 MB for 1,316 documents on `python/mypy` [2] — the slowest indexer in the set by
  13×, and the reason research 12 notes that "a one-line Python edit still costs 83 s of
  pyright before anything is written". That pyright was modified to *remove* its bail-out and
  memory-eviction heuristics so it analyses everything [2], which is why it is both complete
  and expensive. A ty-based emitter would be the highest-value tool change available to codectx
  and does not exist; building one means writing a SCIP backend against ty's semantic index,
  which is not a lane-sized task.
- **TypeScript.** scip-typescript 0.4.0 is still actively developed (symbol-kind emission
  merged 2026-09-11) and there is **no SCIP emitter on the native port**; a tsgo-based indexer
  would need the 7.1 API that does not exist yet [11].
- **Go / Java / C++ / Rust.** No alternative emitter. rust-analyzer's `scip` subcommand is the
  pattern research 12 identified as right ("same engine, separate batch entry point"); no
  Python or TypeScript engine offers an equivalent.

## 5. Recommendation per language

**Python — switch the *server* to ty and keep scip-python.**
This is the one language where the pinned choice is measurably wrong for codectx's session
shape. On an 11k-file repository a cold pyright answers 2 of 136 references and cannot answer
`implementation` at all; ty answers all 136 in 0.21 s at 168 MB, answers `implementation`
correctly (verified against a subclass fixture), reports `serverInfo`, negotiates `utf-8`, and
ships as a single static binary with a `.sha256` sidecar and no runtime — which would also let
the lock stop paying Node for Python. The strongest case for *keeping* pyright is that the gap
is a readiness bug, not a capability one: the overlay could wait for the server to finish
indexing before its first `references`, and pyright then returns 138 in 0.23 s. That is the
cheaper change and it should be made regardless — but it converts every cold python query into
a ≥15 s wall on an 11k-file repository, which no amount of tuning fixes, whereas ty is complete
at 0.21 s with no wait at all. The one real cost of the switch is churn: ty is 0.0.x with an
explicit "breaking changes between any two versions" warning [5], so the pin needs re-cutting
on a faster cadence than the rest of the lock. basedpyright is **not** the answer — same
engine, same cold-session gap, everything it adds is outside codectx's seven. pyrefly is the
most conformant checker of the four and the safest bet on stability (1.0, Instagram's default),
but it has pyright's cold-session gap at 2–3× the memory: worse than ty, barely better than the
status quo.

**TypeScript/JavaScript — keep typescript-language-server 6.0.0 for now, and re-evaluate at
TypeScript 7.1.** tsgo answered identically at 7× the speed and is the better long-term target:
a static Go binary with no Node runtime, all seven of codectx's operations implemented, and it
reports `serverInfo` and `positionEncoding` where the pinned server reports neither. But the
only channel exposing the `tsgo` binary today is `@typescript/native-preview`, still on a
`-dev.` tag; stable `typescript` 7.0.2 exposes only `bin/tsc` over 20 per-platform optional
deps and ships no API [11]; and the staging repo is archived. Pinning a `-dev.` build into a
lock whose premise is reproducibility is the wrong trade. vtsls is rejected outright: same
tsserver, more wrapper, nine months stale.

**Go — keep gopls, unconditionally.** There is no second Go language server worth naming, the
pin is already at the newest release, and gopls is the only server in the set with a supported
persistent cache — exactly the property that makes a cold codectx session cheap.

**Rust — keep rust-analyzer, and advance the pin.** It is the only Rust server and the only
Rust SCIP emitter, so the two lock roles collapse into one payload by necessity rather than by
choice. The pin (2026-08-17.4) is about four weeks behind a weekly release train; that is a
pin-refresh item, not a candidate question.

**Java — keep jdtls.** The only alternative, `java-language-server`, has no Gradle project
model and a fraction of the feature surface; nothing else targets LSP. jdtls's costs — a
managed JDK, Equinox runtime arguments, a per-payload configuration directory it writes into —
are already paid, measured and documented; a switch would trade them for a strictly smaller
answer.

**C/C++ — keep clangd.** ccls's one historical advantage, an up-front whole-project index, has
been eroded by clangd's background index, which is content-hash incremental and persisted;
ccls's release cadence has fallen behind while clangd tracks the LSP spec [14]. The pin is at
the newest release.

## 6. What a switch would cost, concretely

If Python moves to ty:

- **`internal/toolchain/tools.lock.json`** — replace the `pyright` entry with a `ty` entry:
  `kind: server`, `runtime: ""` (not `node`), `license: MIT`, `languages: ["python"]`, `entry:
  ty`. Upstream publishes `ty-{x86_64,aarch64}-unknown-linux-{gnu,musl}`, `-apple-darwin` and
  `-pc-windows-msvc` archives (20 targets in all), so **all six platform keys are pinned
  upstream and none need hosting** — the reverse of pyright, which must be prebuilt with `npm
  ci --omit=dev` at release time. Upstream also publishes a per-asset `.sha256` sidecar and a
  `sha256.sum`, so `upstream_digest` can be recorded — better provenance than clangd or
  rust-analyzer, which publish none. `node` stays pinned for the three other node-hosted
  payloads, so nothing else changes in the runtime graph.
- **`internal/provider/lsp`** — the `pyright` `Definition` becomes `ty` with arguments
  `["server"]` instead of `["--stdio"]`, same `PATH HOME` allowlist, same root markers. Because
  ty is not runtime-hosted, `Resolve` stops composing `<managed node> <entry>` for python and
  runs the payload directly — the same shape as gopls, clangd and rust-analyzer.
- **What changes in the product**: python gains `implementations`,
  `OverlayBinding.ProviderVersion` stops falling back to the pinned lock version because ty
  reports `serverInfo`, and the negotiated `positionEncoding` becomes `utf-8` rather than the
  utf-16 default — all three feed `InputDigest`, so every cached python overlay answer is
  correctly a different question after the swap. The verification table in
  `docs/providers-lsp.md` needs re-running.
- **What does *not* change**: `scip-python` stays exactly as pinned, so canonical python facts
  and their 83 s cost are untouched — this is an overlay-only change.

## Sources

1. codectx, `docs/research/08-scip-empirical-six-indexers.md` (in-repo).
2. codectx, `docs/research/12-incremental-scip-lsp.md` (in-repo), §3.1 baselines and §4 LSP server table.
3. Sourcegraph, "scip-python: a precise Python indexer" — https://sourcegraph.com/blog/scip-python
4. pyright LICENSE.txt (MIT) — https://raw.githubusercontent.com/microsoft/pyright/main/LICENSE.txt ; basedpyright LICENSE.txt (MIT, same text) — https://raw.githubusercontent.com/DetachHead/basedpyright/main/LICENSE.txt
5. astral-sh/ty README + releases (0.0.81, 2026-09-15; "does not yet have a stable API; breaking changes… may occur between any two versions") — https://github.com/astral-sh/ty , https://github.com/astral-sh/ty/releases
6. Astral, "ty: An extremely fast Python type checker and language server" — https://astral.sh/blog/ty
7. ty docs, "Language server" (supported/unsupported request list; `textDocument/implementation` unsupported, issue #3514) — https://docs.astral.sh/ty/features/language-server/
8. DetachHead/basedpyright README — https://github.com/DetachHead/basedpyright
9. Pyrefly, "Pyrefly v1.0 is here!" — https://pyrefly.org/blog/v1.0/ (performance and conformance figures are the vendor's own); "IDE features" — https://pyrefly.org/en/docs/IDE-features/
10. typescript-language-server LICENSE — https://github.com/typescript-language-server/typescript-language-server/blob/master/LICENSE
11. Microsoft, "Announcing TypeScript 7.0" — https://devblogs.microsoft.com/typescript/announcing-typescript-7-0/ ; npm registry metadata for `typescript@7.0.2` and `@typescript/native-preview` — https://registry.npmjs.org/typescript and https://registry.npmjs.org/@typescript/native-preview
12. microsoft/typescript-go `internal/lsp/server.go` at tag `typescript/v7.0.2` (handler registration and `ServerCapabilities`) — https://raw.githubusercontent.com/microsoft/typescript-go/typescript/v7.0.2/internal/lsp/server.go ; repository metadata showing `archived: true` — https://api.github.com/repos/microsoft/typescript-go
13. yioneko/vtsls LICENSE (MIT) and releases — https://github.com/yioneko/vtsls
14. MaskRay/ccls, "Comparison with clangd" (issue #880) — https://github.com/MaskRay/ccls/issues/880 ; clangd "Excessive memory consumption" discussion #2578 — https://github.com/clangd/clangd/discussions/2578
15. SCIP Code Intelligence Protocol, indexer list — https://scip-code.org/
16. Every version and release date above was read on 2026-09-15 from the GitHub releases API (`api.github.com/repos/<owner>/<repo>/releases/latest`), the npm registry (`registry.npmjs.org/<pkg>`), PyPI (`pypi.org/pypi/<pkg>/json`), the Go module proxy (https://proxy.golang.org/golang.org/x/tools/gopls/@v/list) and the Eclipse milestone listing (https://download.eclipse.org/jdtls/milestones/). `georgewfraser/java-language-server` metadata — https://github.com/georgewfraser/java-language-server
