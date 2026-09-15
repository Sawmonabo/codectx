# Performance: method, measurements and what is still unproven

This document records how codectx's Section 23 budgets are measured, the numbers
measured so far, and — in its own section — every budget that is **not** met and
every platform and profile that was **not** built or exercised here. Nothing on
this page is rounded up. A budget that was skipped is written as skipped, a
budget that was missed is written with its number, and a platform that could not
be built on the measuring host is named as an unproven platform rather than
being reported as a pass.

The rule that governs every figure below is Section 23.5's: **no target may be
met by excluding the failing feature or by rewriting the benchmark after seeing
the results.**

## 1. Environment (Section 23.1)

Every measurement in Section 3 was taken on one host, in one configuration:

| Property | Value |
|---|---|
| CPU | Intel Core Ultra 9 285HX |
| OS / kernel | Linux on WSL2 (`6.18.x-microsoft-standard-WSL2`) |
| Platform | `linux/amd64` |
| Filesystem holding the workspace and the data directory | ext4 |
| Go toolchain | `go1.27.1` (`GOTOOLCHAIN` pinned; `internal/model.SchemaVersion = "1"`) |
| Build | release build, `go build -trimpath`; **uninstrumented** — no `-race`, no coverage instrumentation |
| Cache state | cold workspace per row: a freshly generated corpus and a fresh data directory; the module cache and the managed tool store are warm and are never written during a row |
| Managed toolchain | `[tools] offline = true` — every fetch is a typed refusal without opening a socket |
| Providers enabled | Tree-sitter structural baseline only. The SCIP, LSP and dependence providers are **off** for every row |
| Grammar versions | the Tree-sitter grammar module set pinned in `THIRD_PARTY_LICENSES.md` and `docs/providers-treesitter.md` |

**Uninstrumented release builds are the only valid source of latency and RSS
figures.** Race and coverage builds are correctness runs; no number in Section 3
comes from one.

### 1.1 Fixtures

Two fixture families are used, and they are not interchangeable.

**The generated corpus is primary** (Section 23.1). It is produced by
`internal/bench/corpus_test.go`'s deterministic generator from a
`corpusSpec{Packages, Seed}`; `P` packages yield `3P+4` files. Fixed seeds make
a run reproducible on any host.

| Scale | Spec | Files | Used by |
|---|---|---|---|
| `corpusTiny` | (L0/L3 default) | small | the end-to-end scenario's sizing |
| `corpusSmallReal` | `{Packages: 40, Seed: 2101}` | 124 | **every measured row in Section 3** |
| `corpusOver200Files` | `{Packages: 70, Seed: 2102}` | 214 | the session-status clamp row (Section 4, obligation 7) |
| `corpusReference` | `{Packages: 3332, Seed: 2103}` | 10 000 | the Section 23.1 reference workload — **VERIFY's pass, not measured here** |

Corpus naming is load-bearing rather than cosmetic: the workspace-symbol prefix
tier matches on the *qualified* name, so Go package names carry the corpus
prefix and each language uses its own infix. Without both properties the Go or
the non-Go half of the corpus is invisible to resolution and the fingerprint
parity row of Section 5 would pass vacuously.

**Real repositories are an additional signal, not a moving substitute** —
Section 23.1 says so in those words. Pinned real corpora are recorded in
`internal/bench/corpora.json` at a full 40-hex commit, with counts admitted only
when they were actually measured. One entry is recorded today:

| Name | Commit | Files | Lines | Bytes | Fixture hash |
|---|---|---|---|---|---|
| `codectx` | `acfe80e9ddd1b3e677af8082bacc06d02f8ffc7e` | 377 | 113 269 | 4 978 911 | `sha256:d8cab2e5…79eb1cd` |

### 1.2 Corpus shape actually exercised by Section 3

The `corpusSmallReal` workspace measured below: **124 files**, 40 packages,
seed 2101, 66 032 eligible source bytes, 248 indexing units. The context-plan
walk over it visits **103 nodes across 40 edges and selects 39 entries**, and is
not truncated.

## 2. Method

Each row builds its own workspace from the generator, opens it through
`app.Services` — the same frozen facade the CLI and the MCP adapters drive, so a
measurement describes the product and not a private helper — and samples the
operation `n` times. Latency rows report **p95 over the stated sample count**;
memory rows report the sampled peak of the **whole process tree** (parent plus
parser workers), read by the Task 20 host sampler.

`internal/bench` contains no production code. It holds `doc.go`,
`plateau_test.go` (whose `TestMain` makes the test binary the parser worker, so
`app.OpenWorkspace` can spawn parsers through `os.Executable`),
`corpus_test.go` (generator, the frozen Section 23.2 budget constants, the
fingerprint-parity rows and the corpora manifest rows) and `budgets_test.go`
(`TestResourceBudgets` and the published `Benchmark*` functions). Selectors are
by function name, not by file:

```bash
go test ./internal/bench -run TestResourceBudgets -count=1
go test ./internal/bench -run '^$' -bench . -benchmem -count=5
```

Every row names the failure mode it protects, and a row is accepted only when a
one-line mutation makes it fail. The mutation proof for the budget table is that
tightening a row's target makes the row fail **with the measured number**:
`budgetExactQueryP95` set to one microsecond produced
`BUDGET MISSED: p95 790µs over 50 samples (target 1µs)`, and reverting restored
the pass below.

## 3. Section 23.2 — measured against target

Measured at small-real scale (124 files) on the environment of Section 1.
`p95 / n` is the 95th percentile over `n` samples.

| # | Row | Target | Measured | Status |
|---|---|---|---|---|
| 1 | `version --json` startup | 50 ms | p95 **2.96 ms** / 20 | PASS |
| 2 | Exact symbol/path query | 50 ms | p95 **1.09 ms** / 50 | PASS |
| 3 | Lexical (FTS) search | 150 ms | p95 **28.7 ms** / 50 | PASS |
| 4 | One-hop caller/callee | 100 ms | p95 **5.72 ms** / 50 | PASS |
| 5 | Context plan, ≤50k visited nodes | 1 s | p95 **74.8 ms** / 10 | PASS |
| 6 | Context plan visited nodes | ≤ 50 000 | **103 visited / 40 edges / 39 entries**, not truncated | PASS |
| 7 | Ten-file base refresh after debounce | 1.5 s | p95 **144 ms** / 5 | PASS |
| 8 | No-change refresh: no parser work, no FTS body rewrite | 250 ms | p95 **52.6 ms**; **248 units reused, 0 files parsed**, lexical shape identical | PASS |
| 9 | Cold base index of the reference fixture | 3 min | **SKIP — routed to VERIFY** (Section 4) | not measured here |
| 10 | Indexing process-tree peak | 768 MiB | **110.8 MiB** | PASS |
| 11 | Idle MCP RSS | 128 MiB | **61.5 MiB** | PASS |
| 12 | Interactive process-tree peak | 256 MiB | **60.5 MiB** | PASS |
| 13 | Base storage vs eligible source bytes | 3.5× | **149.0×** at small-real scale (db 5 349 376 + wal 4 424 912 over 66 032 source bytes; **CAS alone 66 032 = 1.00×**) | **measured, not gated here** — Section 4 |
| 14 | Low-memory profile (Section 23.3): one worker, 2 GiB | same results as the full profile | **fingerprint identical across 10 fact families**; index tree peak **47.9 MiB** | PASS |
| 15 | Session-status `statusLimit` >200-file clamp | clamp applied **and** reported | **UNMET** — Section 4 | blocker |
| 16 | Default context-graph budget re-pin | measurement-driven | **measured; `max_graph_depth` pin not decided here** — Section 4 | open |

Retained unchanged generations adding no duplicate blobs, graph facts or FTS
bodies is covered by row 8's instrument (248 units reused, 0 files parsed, the
lexical shape byte-identical) and by the end-to-end retained-read row in
`internal/e2e`.

### 3.1 Published benchmarks

`go test ./internal/bench -run '^$' -bench . -benchmem -count=5`, same host and
build, five runs each:

```
BenchmarkVersionStartup-16      520    2308981 ns/op    13542 B/op      73 allocs/op
BenchmarkExactQuery-16         1886     614143 ns/op    44715 B/op     947 allocs/op
BenchmarkLexicalSearch-16        44   25071726 ns/op  7338075 B/op  152235 allocs/op
BenchmarkOneHopTraversal-16     265    4559743 ns/op   779765 B/op   13342 allocs/op
BenchmarkContextPlan-16         405    2732277 ns/op   313500 B/op    7395 allocs/op
BenchmarkTenFileRefresh-16        9  997187150 ns/op 28662835 B/op  281603 allocs/op
BenchmarkNoChangeRefresh-16      18   63185963 ns/op 14675458 B/op  117058 allocs/op
```

### 3.2 Context-graph depth sweep (behind rows 6 and 16)

64 seeds, both traversal directions, configured bounds
`max_graph_depth = 3` / `max_visited_nodes = 50 000` / `max_graph_edges = 100 000`:

```
depth 1: visited=91  edges=28  entries=27  truncated=false
depth 2: visited=103 edges=40  entries=39  truncated=false
depth 3: visited=103 edges=40  entries=39  truncated=false
depth 4: visited=103 edges=40  entries=39  truncated=false
```

The walk saturates at depth 2 on this corpus. Two conclusions are firm from it
and one is not:

- **No raise to `max_visited_nodes`.** 50 000 *is* Section 23.2's context-plan
  ceiling, so raising it would break a release gate by construction.
- **No raise to `max_graph_edges`.** Measured edges-per-visited-node is 0.39
  against a pinned ratio of 2:1 — five times the headroom the walk uses.
- **`max_graph_depth` cannot be decided from this evidence.** The generated
  corpus saturates at depth 2, while the truncation that motivated the re-pin
  came from real-repository fan-in the generator does not produce. Narrowing
  3 → 2 here would pin a budget against the wrong workload, which is the thing
  the ledger item exists to prevent. See Section 4.

## 4. Misses, skips and unproven platforms

Nothing in this section is a pass.

| Item | State | What it would take |
|---|---|---|
| **Row 9 — cold index of the Section 23.1 reference fixture (1M lines / 10k files / ~80 MiB) within 3 min** | **Not measured.** Skipped at bench scale and routed to the wave's single verification pass, which is the only actor licensed to run a full-scale cold index. | One reference-fixture pass at `corpusReference` (10 000 files), reported with the elapsed time. |
| **Row 13 — base storage ≤ 3.5× eligible source bytes** | **Not asserted at the scale the budget is defined over.** Measured 149.0× at small-real scale; the CAS itself is exactly source-sized (1.00×), and the excess is the database's and the WAL's fixed cost, which ~0.5 KiB files cannot amortise while the reference fixture's ~8 KiB files can. Gating the absolute 3.5× at this scale would enforce Section 23.2 against a workload it is not defined over. | The verification pass asserts 3.5× at reference scale. Until it does, **this budget has no evidence either way.** |
| **Row 15 — `statusLimit` >200-file clamp applied and reported** | **UNMET.** The count that must clear the 200-record page is the *session's required* files, and the scope walk reaches one symbol per package and then saturates: a plan required 131 files at 214 repository files, 152 at 364, 161 at 724 and 158 at 1204, so no corpus size alone reaches the clamp. Adding 63 disjoint path seeds moved it 161 → 163, because a path seed resolves a file but no symbol and so never becomes required. | **In progress in lane T21-L2b**, which grows the session's required set through `context include` over further manifests until it exceeds 200 files, then asserts the clamp is both applied and reported. |
| **Row 16 — `max_graph_depth` default re-pin** | **Open.** Measured but not decided (Section 3.2). | **Decided by the verification pass on the real store**: one impact query at the configured depth over a ~745-file real workspace, reporting visited, edges and `Meta.Truncated`. The recommendation is applied in the fix round; no value is changed on this page's evidence. |
| **`darwin/amd64` and `darwin/arm64`** | **UNPROVEN PLATFORM.** No macOS host was available to the measuring environment, and the release rule forbids a cross-built binary or bundle. Nothing on this page was measured on darwin, and no darwin archive or bundle was produced or exercised here. | The release workflow's native macOS legs. Until one runs, darwin is declared, not proved. |
| **`windows/amd64`, `windows/arm64`, `linux/arm64`** | **Not measured here.** The Section 3 figures are `linux/amd64` only. The six-target build proof is the verification lane's and the release workflow's. | Native legs per target; no cross-compiled measurement is accepted. |
| **Managed analyzer payloads under network isolation** | **Not exercised.** The every-push isolated job pairs an *empty* tool store with `[tools] offline = true`, so it covers the structural baseline only; no managed analyzer payload runs under isolation. | A prefetched store — the tools-matrix leg and the verification bundle leg, not the per-push job. |
| **Race-build confirmation of the budget rows** | **Not run.** `-race` was not run over the Section 3 rows. The latency headroom is 10–50×, so a race build breaching a ceiling is unlikely — but that is a residual, not a measurement. | One `-race` pass over `TestResourceBudgets`, owned by the verification lane. |
| **Sensitivity of row 8's no-change instrument** | **Partially unproved.** Three product mutations were attempted against the reuse decision and all were absorbed: the refresh still reused 248 units and parsed 0 files, because the reuse decision is content-addressed at the unit level. That is evidence the fast path is real, but it leaves the row's mutation sensitivity unproved. | Mutate unit *identity* (a provider fingerprint) rather than the reuse decision. Untried. |

## 5. Fingerprint parity — the method, and why it is a gate

Section 23.5 forbids accepting an optimization without proving that the
capabilities, facts and query answers it produces are unchanged. The method
implemented in `internal/bench/corpus_test.go` is:

1. Index the fixed corpus twice — once under the full configuration, once under
   the candidate change.
2. `capture` reduces each run to a set of **fact families** (per-language node
   families, `overview`, `relation/*`, `search/*`) plus the capability rows,
   digesting each family's canonical lines.
3. `compareFingerprints` reports, family by family, a family that **disappeared**
   (naming it and its baseline line count) and a family whose digest **changed**
   (naming both digests and both line counts). A reduction that removes work is
   therefore named, not merely counted.

The row is mutation-proved: replacing the family loop with a comparator that
checks only the capability rows fails with *"a run that parsed only Go
fingerprinted identically to the full run"*.

**The finding that makes this a gate rather than a formality.** Over a corpus
indexed with `providers.tree_sitter.languages = ["go"]`, **every capability row
is byte-identical to the full run** — `treesitter|structure|workspace|fresh` —
while all Python and TypeScript node facts are gone:

```
fact family "node/python" disappeared (27 lines in the baseline)
fact family "node/typescript" disappeared (27 lines in the baseline)
fact family "overview" changed: 29 lines/5280c5fcd4b2f214 -> 21 lines/a47da8269c8ada99
fact family "relation/calls" changed: 20 lines/7fa4b68fc7fd48bc -> 4 lines/b7d435573569c3cd
fact family "search/Ctxbench" changed: 101 lines/1bfaa17ae55b6090 -> 47 lines/db9394abe74c012d
capability rows are identical across the reduction
```

Completeness and capability reporting alone therefore **cannot** detect this
class of reduction — a capability silently narrowed to fewer languages still
reports `fresh`. The fact-family comparison is the only instrument in the tree
that can, which is why it, and not the capability report, is what an
optimization must clear.

Residual: the fingerprint is captured at tiny scale, one page per query; the
comparison fails loudly rather than comparing two prefixes if a widened corpus
outgrows `model.MaxPageItems`. A reference-scale parity pass has not been run.

## 6. Section 23.5 — the metric set

Every performance investigation reports, and this page's rows draw from:

- CPU time; allocations and bytes allocated (`-benchmem`, above); peak heap.
- Parent RSS, native/worker RSS, and **whole-process-tree peak RSS** sampled
  concurrently. On platforms without a tree reader the figure is reported
  `unavailable`, never `0` — see `docs/operations.md`.
- Disk bytes read and written, and temporary bytes.
- Startup time; p50/p95/p99 latency; throughput.
- Unit reuse and parse counts per indexing run.
- Peak database, WAL and content-store sizes.

Two rules bind the interpretation:

- A change that puts a figure **over an absolute Section 23.2 budget fails the
  release gate.**
- A **regression greater than 10%** against the recorded figure above requires
  investigation and a written explanation, even when the absolute budget is
  still met.

## 7. Reproducing this page

```bash
go build -trimpath -o ./bin/codectx ./cmd/codectx
go test ./internal/bench -run TestResourceBudgets -count=1
go test ./internal/bench -run '^$' -bench . -benchmem -count=5
go test ./internal/bench -run 'TestFingerprintParity|TestCorporaManifest' -count=1 -v
```

Under `-short` the whole package skips: these rows build real workspaces and are
not unit tests. Re-record Section 1 whenever the host, the filesystem, the
toolchain, the grammar set or the enabled provider set changes — a number
without its environment is not a measurement.
