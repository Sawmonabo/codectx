# Native-engine research, raw evidence — can SCIP + LSP replace the engine's call graph? The measured answer on r3 itself

Source: `docs/research/19-language-server-and-indexer-matrix.md` §2 **[MEASURED]**, lane TOOLS-R,
2026-09-15, linux/amd64. Method: JSON-RPC driver, `initialize` → `initialized` → `didOpen` → one
`textDocument/references` (`includeDeclaration: true`) → `shutdown` → `exit`, `/usr/bin/time -v`
wrapping the server process.

## The JavaScript result, measured on the r3 corpus, project `app` — the same project that costs 20:20
```
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

```

**This is decisive for the `calls` question on r3.** On the exact repository whose JavaScript unit
is 64% of the run, the *language-server* route returns **2 references**, cold and after a 30 s settle,
from both the pinned server and the faster candidate — because `app` has no `tsconfig.json`, so
tsserver builds an inferred project and scopes references to the opened file's import closure. The
note says plainly that this is "a real limitation on JS monorepos, but not one a swap fixes".

## And the Python result, for contrast (the same failure shape, a different cause)
```
textually present in 104 files.**

| Server | settle | `references` | results | server peak RSS | caps (D T I R Ds Ws Ch) |
|---|---|---|---|---|---|
| pyright 1.1.414 (pinned) | cold | 1.43 s | **2** | 267 MB | D T **–** R Ds Ws Ch |
| pyright 1.1.414 | +15 s | 0.23 s | 138 | 290 MB | same |
| ty 0.0.81 | **cold** | **0.21 s** | **136** | **168 MB** | all seven |
| ty 0.0.81 | +15 s | 0.05 s | 136 | 166 MB | all seven |
| pyrefly 1.3.1 | cold | 0.20 s | **2** | 487 MB | all seven |
| pyrefly 1.3.1 | +15 s | 0.00 s | 136 | 541 MB | all seven |

Replicated on a smaller corpus (a second, smaller Python corpus, 2,379 `.py`; RSS is the
driver-plus-server tree, overstating by ~15 MB): cold pyright 1.02 s / **90** / 212 MB, cold ty
0.21 s / **93** / 101 MB, cold pyrefly 0.38 s / **90** / 1,108 MB. The ordering holds; the
completeness gap does not appear because the queried class sits in the opened file's own import
closure — the condition that fails on m32rimm.
```
Amended 2026-09-15: the implementation lane could not reproduce the `+15 s` pyright row — 2
locations cold and still 2 after 15, 40 and 60 s — so the cold gap is **wider** than the table shows.

## Consequence for the post-MVP design

"Replace the engine's calls with SCIP + LSP" is not one claim, it is three, and they fail differently:

| route | what it gives on r3 | why it fails where it fails |
|---|---|---|
| SCIP, where an indexer runs and succeeds | compiler-precision call edges at zero extra analysis | r3-3: nine of ten TypeScript precise units failed `CTX_PROVIDER_UNAVAILABLE` with 0 records; the Java one failed `CTX_PROVIDER_OUTPUT_INVALID` (program ledger, 16:10) |
| LSP `callHierarchy` / `references` | **2 results** on `r3/app` | no `tsconfig.json` → inferred project → import-closure scope; not fixed by changing server (19 §2) |
| LSP as a bulk precomputed table at all | N round trips for N symbols | `docs/research/04` §3.3; every session is cold, `max_servers` defaults to 1 (19 §1) |
| tree-sitter name heuristics (shipped today) | `syntax` precision, `may_refer_to` + `candidates` | mis-wires on overloads, interfaces and same-named methods (`04` §3.4) |

So the engine's `calls` is **not** simply replaceable on r3 today. That is an argument for keeping
the engine's call graph, not against a native dependence engine — the two fact groups separate
cleanly, and it is exactly the separation the subdivision parity table already shows (`10-round3` §8:
CDG 99.7% / REACHING_DEF 99.9% survive a split, resolved calls 46% do not).
