# Performance: method, measurements and what is still unproven

This document records how codectx's Section 23 budgets are measured, the numbers
measured so far, and — in its own section — every budget that is **not** met and
every platform and profile that was **not** built or exercised here. Nothing on
this page is rounded up. A budget that was skipped is written as skipped, a
budget that was missed is written with its number, and a platform that could not
be built on the measuring host is named as an unproven platform rather than
being reported as a pass.

The scale posture these budgets are measured against — unlimited by default,
bounded by page — and the reasoning behind the two open misses on this page
(the indexing process-tree peak and the storage ratio) are recorded in
[ADR-0001 — Scale posture](adr/ADR-0001-scale-posture.md).

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
| `corpusOver200Files` | `{Packages: 120, Seed: 2102}` | 364 | the session-status clamp row (Section 3, row 15) |
| `corpusReference` | `{Packages: 3332, Seed: 2103}` | 10 000 | the Section 23.1 reference workload — measured by the verification pass, not on a bench row (Section 3, rows 9 and 13) |

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

### 1.2 Corpus shapes actually exercised by Section 3

Most of Section 3 runs on `corpusSmallReal`: **124 files**, 40 packages, seed
2101, 66 032 eligible source bytes, 248 indexing units. The context-plan walk
over it visits **103 nodes across 40 edges and selects 39 entries**, and is not
truncated.

Four rows do not. Rows 9 and 13 are defined over the Section 23.1 reference
workload and run on `corpusReference` — 10 000 files, but 262 551 lines and
5.70 MiB, which is short of the size Section 23.1 specifies (Section 4). Row 15
runs on `corpusOver200Files`, because no plan over a small corpus reaches a
200-file session. Row 16 is not a generated corpus at all: it is an 810-file
real workspace, which is the only shape that can answer whether a depth bound
is reached in practice.

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

Measured at small-real scale (124 files) on the environment of Section 1,
except rows 9 and 13, which are defined over the Section 23.1 reference
workload and were taken by the verification pass on the reference corpus, and
row 16, which was taken on a real 810-file workspace. `p95 / n` is the 95th
percentile over `n` samples.

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
| 9 | Cold base index of the reference fixture | 3 min | **1 min 14.8 s** over 10 000 files, process-tree peak **85.2 MiB** | PASS at 10 000 files; the corpus was short on lines and bytes — Section 4 |
| 10 | Indexing process-tree peak | 768 MiB | **110.8 MiB** | PASS |
| 11 | Idle MCP RSS | 128 MiB | **61.5 MiB** | PASS |
| 12 | Interactive process-tree peak | 256 MiB | **60.5 MiB** | PASS |
| 13 | Base storage vs eligible source bytes | 3.5× | **75.00×** on the 10 000-file generated corpus: 427 282 345 stored over 5 697 273 eligible source bytes (db 421 560 320 + wal 24 752 + CAS 5 697 273; **CAS alone = 1.00×**); re-measured **73.86×**. 149.0× at small-real scale | **MISS by 21×** — Section 4 |
| 14 | Low-memory profile (Section 23.3): one worker, 2 GiB | same results as the full profile | **fingerprint identical across 10 fact families**; index tree peak **47.9 MiB** | PASS |
| 15 | Session-status `statusLimit` >200-file clamp | clamp applied **and** reported | a **242-file** session with **216 required** files answered a first page of **199 records** and issued a cursor | PASS |
| 16 | Default context-graph budget re-pin | measurement-driven | **`max_graph_depth` stays 3**: on a real 810-file workspace the walk saturates at depth 2 — **1 030 visited / 1 295 edges**, unchanged at depth 3, 4 and 5 | PASS |

Row 15's two counts are the recorded run's, not a fixed point: plan selection
order among equally scored candidates is not bit-stable at this scale, and a
repeat on the same host reached 245 session files and 214 required. What the
row asserts is the invariant, not the counts — both counts clear 200, the first
page carries fewer than the 200-record ceiling, and a cursor is issued — so a
different growth curve cannot make it pass vacuously.

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
- **`max_graph_depth` stays 3.** The same sweep was repeated on a real
  810-file workspace with 30 seeds at the configured bounds, and it saturates
  at depth 2 there too — 1 030 visited nodes across 1 295 edges in 0.22 s,
  unchanged at depth 3, 4 and 5. That is **2.1% of `max_visited_nodes`** and
  **1.3% of `max_graph_edges`**. `Meta.Truncated` is true at every depth ≥ 2 on
  that workspace, but `truncation_reason` is *"affected entity record limit
  reached"* — the 200-record answer page, not a graph bound. Reading that flag
  as a depth truncation is what motivated the re-pin in the first place, so the
  premise is withdrawn: narrowing 3 → 2 would change nothing measured, and
  would drop the one hop that still adds nodes on a deeper graph. Whether a
  work limit like this one should default to unlimited at all is a separate
  question and is not answered by this measurement.

## 4. Misses, skips and unproven platforms

Nothing in this section clears the obligation as written. A row here may carry
a measurement — some do — but the measurement does not answer the obligation,
and the difference is stated rather than rounded away.

| Item | State | What it would take |
|---|---|---|
| **Row 9 — cold index of the Section 23.1 reference fixture (1M lines / 10k files / ~80 MiB) within 3 min** | **Measured at the file count, not at the line and byte count.** The verification pass cold-indexed `corpusReference` in 1 min 14.8 s with an 85.2 MiB process-tree peak — but that corpus yields **262 551 lines / 5.70 MiB**, where Section 23.1 specifies ≈1M lines and ≈80 MiB. The corpus is ~3.8× short on lines and ~14× short on bytes, so row 9's PASS stands for the 10 000-file shape only. | The generator is being re-specified to Section 23.1's size; one cold index at that size, re-run and re-recorded here. |
| **Row 13 — base storage ≤ 3.5× eligible source bytes** | **MISS, by 21×.** On the 10 000-file generated corpus the store held 427 282 345 bytes over 5 697 273 eligible source bytes — **75.00×**, re-measured at **73.86×**: a 421 MB database over 5.70 MiB of source. The content store itself is exactly source-sized (1.00×). Two facts bound how this number should be read. The corpus averages ~570 bytes per file where Section 23.1's reference is ~8 KiB per file, so per-file fixed cost dominates it more than it would at the specified size (see row 9). And the amplifier is identified, not guessed: **per-row identity width** — 32-byte BLOB identities and wide TEXT keys replicated into every secondary index, with `WITHOUT ROWID` secondary indexes re-storing the full primary key. Per-table accounting puts relations at ~22% of the file, evidence ~22%, native_aliases ~20% and nodes ~20%. FTS content duplication, JSON payloads, WAL size and page fill were each measured and ruled out. The earlier explanation on this page — that small generated files simply cannot amortise a fixed cost — is withdrawn; it does not survive a 421 MB database. | A storage-layout redesign (integer surrogate identities and key interning), scheduled as its own task in the next wave. The decision, the alternatives refused on the way and its sources are recorded in [ADR-0002 — Storage identities](adr/ADR-0002-storage-identities.md). The ratio is then re-measured on the corrected reference corpus **and** on real repositories, and this row is re-recorded from those runs. |
| **`darwin/amd64` and `darwin/arm64`** | **UNPROVEN PLATFORM.** No macOS SDK or macOS host was available to the measuring environment, and the release rule forbids a cross-built binary or bundle. Nothing on this page was measured on darwin, and no darwin archive or bundle was produced or exercised here. These targets are **built only by the release workflow's native legs; unproven on the measuring host.** | A native macOS leg of the release workflow. Until one runs, darwin is declared, not proved. |
| **`windows/arm64`** | **UNPROVEN PLATFORM.** The structural parser is CGo, so every target needs a native C toolchain, and the measuring host's toolchain set carries no aarch64 Windows cross-compiler — only the i686 and x86_64 ones. The target is therefore **built only by the release workflow's native legs; unproven on the measuring host.** | A native `windows/arm64` leg of the release workflow. |
| **Section 3 figures on any platform but `linux/amd64`** | **Not measured.** Every latency and memory number in Section 3 is `linux/amd64`. Of the build proof: `linux/amd64` was built and run natively, `linux/arm64` was built and run under emulation, and `windows/amd64` was built but not executed. No Section 3 figure was taken on any of them, and none is accepted from a cross-compiled binary. | Native legs per target, each reporting its own figures. |
| **Managed analyzer payloads under network isolation** | **Not exercised.** The every-push isolated job pairs an *empty* tool store with `[tools] offline = true`, so it covers the structural baseline only; no managed analyzer payload runs under isolation. | A prefetched store — the tools-matrix leg and the release workflow's bundle legs, not the per-push job. |
| **`codectx tools prefetch --all` into a fresh store** | **Not performed here.** Everything *downstream* of the store was proved: an extracted bundle indexed, queried, planned and served MCP under network isolation with zero outbound connections and an unchanged store. Filling a fresh store is the one step that was not, because on this host it refuses offline as designed and completing it would mean downloading twelve payloads, including the largest at roughly 1.9 GB — which the offline rule for this measurement forbids. | The release workflow's bundle legs, which prefetch natively per target. |
| **Race-build confirmation of the budget rows** | **Run, and it does not confirm them — as expected.** Under `-race`, five Section 23.2 latency rows exceeded their targets (lexical search 641 ms, one-hop 156 ms, context plan 2.40 s, ten-file refresh 3.71 s, no-change refresh 1.74 s). Section 23.2 is defined over uninstrumented release builds, which is why Section 1 says so; a race build is a correctness run and its timings are not evidence about a budget either way. The budget rows are being changed to skip under `-race` with that reason stated, so the correctness rows still run. | Nothing further. The race build's job is data races, and it is run for that. |
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
go test ./internal/bench -run TestSessionStatusClamp -count=1 -v
go test ./internal/bench -run '^$' -bench . -benchmem -count=5
go test ./internal/bench -run 'TestFingerprintParity|TestCorporaManifest' -count=1 -v
```

That block produces every row of Section 3 except three. Rows 9 and 13 need a
cold index of the reference corpus, and row 16 needs a real workspace; both are
run by the verification pass and neither is reproduced by a bench selector. Row
15's two counts are reproduced as an invariant, not as numbers — see the note
under Section 3's table.

Under `-short` the whole package skips: these rows build real workspaces and are
not unit tests. Re-record Section 1 whenever the host, the filesystem, the
toolchain, the grammar set or the enabled provider set changes — a number
without its environment is not a measurement.
