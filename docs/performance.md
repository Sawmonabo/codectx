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
| Providers enabled | Tree-sitter structural baseline only. The SCIP and LSP providers are **off** for every row |
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
| `corpusTiny` | `{Packages: 4, Seed: 21}` | small | the end-to-end scenario's sizing |
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
2101, 66 032 eligible source bytes, 248 indexing units. The context-plan scope
walk over it from 64 seeds visits **769 nodes across 1 596 edges** at the
default, unlimited bounds (Section 3.2).

Three rows do not. Rows 9 and 13 are defined over the Section 23.1 reference
workload and run on `corpusReference` — 10 000 files, 1 052 933 lines and
82.08 MiB, the size Section 23.1 specifies (an earlier generator produced
262 551 lines / 5.70 MiB; Section 4 records both measurements). Row 15
runs on `corpusOver200Files`, because no plan over a small corpus reaches a
200-file session.

## 2. Method

Each row builds its own workspace from the generator, opens it through
`app.Services` — the same frozen facade the CLI and the MCP adapters drive, so a
measurement describes the product and not a private helper — and samples the
operation `n` times. Latency rows report **p95 over the stated sample count**;
memory rows report the sampled peak of the **whole process tree** (parent plus
parser workers), read by the host sampler.

### What one parser worker costs

The parser worker count is a CONCURRENCY figure, not a resident cost: the
pool starts no process until a unit demands one and drains every worker when the
last unit returns, so a process that has stopped parsing holds no worker at all
(`TestPoolLazyAndDrainedWhenTheStageEnds`), and the number alive is the concurrent
parse demand rather than the ceiling. What one live worker costs was measured on
the measuring host from `/proc/<pid>/smaps_rollup`, on a worker that had sent its
hello and parsed nothing: **18.2 MiB RSS, 9.4 MiB PSS**.

A parse adds nothing lasting **for the file's size** — the worker closes each
tree as the file is finished — so the figure does not grow with the largest file
in the repository. It is not flat in the number of LANGUAGES a worker has served:
`state.parsers` and `state.queries` hold one parser and one compiled query per
language until the worker exits, and the figure above is a worker holding none of
them. The rest is the binary's mapped pages and the Go runtime.

The resident bound is therefore **live workers x ~18 MiB RSS and up**. RSS is
what the tree-peak rows sum, and it double counts: every worker is a
re-execution of the same binary, so the MARGINAL cost of one more worker is the
~9.4 MiB PSS figure and a summed-RSS bound overstates a large ceiling by roughly
a factor of two.

## 3. Section 23.2 — measured against target

Measured at small-real scale (124 files) on the environment of Section 1,
except rows 9 and 13, which are defined over the Section 23.1 reference
workload and were taken by the verification pass on the reference corpus.
`p95 / n` is the 95th percentile over `n` samples.

| # | Row | Target | Measured | Status |
|---|---|---|---|---|
| 1 | `version --json` startup | 50 ms | p95 **2.96 ms** / 20 | PASS |
| 2 | Exact symbol/path query | 50 ms | p95 **1.09 ms** / 50 | PASS |
| 3 | Lexical search | 150 ms | not yet measured on the search index | UNMEASURED — recorded from the integrated binary ([ADR-0013](adr/ADR-0013-search-index.md), decision 9); the search index at scale is below |
| 4 | One-hop caller/callee | 100 ms | p95 **5.72 ms** / 50 | PASS |
| 5 | Context plan, ≤50k visited nodes | 1 s | p95 **74.8 ms** / 10 | PASS |
| 6 | Context plan visited nodes | ≤ 50 000 | **769 visited / 1 596 edges** from 64 seeds at the default bounds (Section 3.2) | PASS |
| 7 | Ten-file base refresh after debounce | 1.5 s | p95 **144 ms** / 5 | PASS |
| 8 | No-change refresh: no parser work, no search-index rewrite | 250 ms | p95 **52.6 ms**; **248 units reused, 0 files parsed**, lexical shape identical | PASS |
| 9 | Cold base index of the reference fixture | 3 min | **1 min 32.1 s** over 10 000 files / 1 052 933 lines / 86 064 203 B (the corrected Section 23.1 size); process-tree peak **145.3 MiB** | PASS — re-measured after the external-merge planner and batched provider sinks landed (earlier: 2 min 56.3 s / 912.6 MiB) |
| 10 | Indexing process-tree peak | 768 MiB | **110.8 MiB** at small-real scale; **145.3 MiB** on the corrected reference corpus (sampled at 250 ms and 50 ms, two cold runs within 0.1 MiB) | PASS — the earlier 912.6 MiB at reference scale was measured before the planner's external merge sort and the batched provider sinks |
| 11 | Idle MCP RSS | 128 MiB | **64.0 MiB** | PASS — re-measured under the corrected semantics (see below); the earlier 61.5 MiB was a startup-window sample |
| 12 | Interactive process-tree peak | 256 MiB | **60.5 MiB** | PASS |
| 13 | Base storage vs eligible source bytes | 3.5× | **16.10×** on the corrected reference corpus: 1 385 729 099 stored over 86 064 203 eligible source bytes (db 1 299 664 896 + wal 0 + CAS 86 064 203; **CAS alone = 1.00×**). Earlier, on the undersized ~570 B/file corpus: 75.00× / 73.86×; 149.0× at small-real scale | **MISS by 4.6×** — Section 4 |
| 14 | Low-memory profile (Section 23.3): one worker, 2 GiB | same results as the full profile | **fingerprint identical across 10 fact families**; index tree peak **47.9 MiB** | PASS |
| 15 | Session-status `statusLimit` >200-file clamp | clamp applied **and** reported | a **242-file** session with **216 required** files answered a first page of **199 records** and issued a cursor | PASS |
| 16 | Default context-graph bounds | measurement-driven | `max_graph_depth`, `max_visited_nodes` and `max_graph_edges` default to **unlimited**; the walk ends on its own at depth 4, **769 nodes / 1 596 edges**, 1.5% of row 6's ceiling (Section 3.2) | PASS |

Row 11 measures a RESTING session, which is what "idle" claims. A window opened
at process start with `mcp.watch` at its default would cover the initial
refresh and its parser workers — a startup peak under an idle label. The row
therefore starts the session with `--watch=false` and samples a 2 s window after a 2 s settle, so the figure is
the server at rest: workspace open, no refresh running. The refresh peak is row
10's subject and is measured there under `index --full`. The 128 MiB target is
unchanged.

Row 15's two counts are the recorded run's, not a fixed point: plan selection
order among equally scored candidates is not bit-stable at this scale, and a
repeat on the same host reached 245 session files and 214 required. What the
row asserts is the invariant, not the counts — both counts clear 200, the first
page carries fewer than the 200-record ceiling, and a cursor is issued — so a
different growth curve cannot make it pass vacuously.

Retained unchanged generations adding no duplicate blobs, graph facts or search
documents is covered by row 8's instrument (248 units reused, 0 files parsed, the
lexical shape byte-identical) and by the end-to-end retained-read row in
`internal/e2e`.

### 3.1 Published benchmarks

`go test ./internal/bench -run '^$' -bench . -benchmem -count=5`, same host and
build, five runs each. `BenchmarkLexicalSearch` is recorded from the integrated
binary with row 3.

```
BenchmarkVersionStartup-16      520    2308981 ns/op    13542 B/op      73 allocs/op
BenchmarkExactQuery-16         1886     614143 ns/op    44715 B/op     947 allocs/op
BenchmarkOneHopTraversal-16     265    4559743 ns/op   779765 B/op   13342 allocs/op
BenchmarkContextPlan-16         405    2732277 ns/op   313500 B/op    7395 allocs/op
BenchmarkTenFileRefresh-16        9  997187150 ns/op 28662835 B/op  281603 allocs/op
BenchmarkNoChangeRefresh-16      18   63185963 ns/op 14675458 B/op  117058 allocs/op
```

### 3.2 Context-graph depth sweep (behind rows 6 and 16)

`TestResourceBudgets/context-plan-visited`: the plan's scope walk -- its
boundary relations (`internal/context/scope.go`'s `scopeRelations`), both
directions, the default unlimited depth, visited and edge bounds -- from 64
seeds, then the same walk bounded at depths 1 to 4. The seeds are the fan-out
function and 63 functions and methods spread evenly over the corpus's
declarations in name order, so every run walks from the same declarations.
`entries` is the answer's first page (200 records); `truncated` on a bounded
line is that line's depth bound:

```
default:  visited=769 edges=1596 truncated=false
depth 1:  visited=265 edges=313  truncated=true (depth)
depth 2:  visited=605 edges=1217 truncated=true (depth)
depth 3:  visited=730 edges=1544 truncated=true (depth)
depth 4:  visited=769 edges=1583 truncated=true (depth)
```

The same walk without the two program-dependence relations
(`data_flows_to`, `control_depends_on`) visits 727 nodes across 1 493 edges:
dependence reach adds 42 nodes (5.8%) and 103 edges (6.9%) at this scale.
Identical across runs.

- **The 50 000-node ceiling holds with room**: the whole walk is 1.5% of it.
  50 000 is Section 23.2's context-plan ceiling and is not moved by this
  measurement.
- **Edges per visited node is 2.08** over the whole walk.
- **The walk ends on its own**: +340, +125 and +39 nodes for hops 2, 3 and 4,
  and nothing past depth 4, so no default bound is needed to keep it small
  here; the bounds stay unlimited and a request that wants one names it.

### Row 3 at scale — the search index on a 546 613-document export

Row 3's 150 ms target is measured at small-real scale. At scale, the search
index was measured on kubernetes: 546 613 search documents in 47 995 units,
exported by the production filesystem and tree-sitter providers, with a 30-query
set in bands by document frequency plus two-term queries and phrases
([ADR-0013](adr/ADR-0013-search-index.md);
[research 22](research/22-search-library-evaluation.md) holds every command
and raw result line).

| Measure | The search index |
|---|---|
| Index build, all documents | 9.0 s wall, 48.8 s CPU, 0.85 GB peak (16 threads, one commit); 18.1 s and 0.49 GB on 1 thread |
| Index on disk | 337–387 MB |
| Activation | no postings work: segments merge in the background |
| 30 queries, first page of 200, warm | 4.04 s when every match is scored under our exact formula; 0.016 s for the library's own pruned top 200 under its own scoring |
| The 5 most frequent terms, every match scored under our formula | 413–709 ms each |
| Kind filter / folder filter, under our formula | 18.6 ms / 5.5 ms |

A ranked page is the pruned top k (ADR-0013 decision 5), so the figure row 3
answers to is our formula's pruned page. That pruned collector returned pages 1
to 3 identical to exhaustive scoring on 2 436 of 2 436 query-page pairs on a
fixture, at 1 and 4 search threads, and on kubernetes on 3 030 of 3 030 pages
over four query sets, 15.7 times faster summed on the 30 speed queries (4 653 ms
to 296 ms). Row 3 is recorded from the integrated binary.

## 4. Misses, skips and unproven platforms

Nothing in this section clears the obligation as written. A row here may carry
a measurement — some do — but the measurement does not answer the obligation,
and the difference is stated rather than rounded away.

| Item | State | What it would take |
|---|---|---|
| **Row 9 — cold index of the Section 23.1 reference fixture (1M lines / 10k files / ~80 MiB) within 3 min** | **Measured at the file count, not at the line and byte count.** The verification pass cold-indexed `corpusReference` in 1 min 14.8 s with an 85.2 MiB process-tree peak — but that corpus yields **262 551 lines / 5.70 MiB**, where Section 23.1 specifies ≈1M lines and ≈80 MiB. The corpus is ~3.8× short on lines and ~14× short on bytes, so row 9's PASS stands for the 10 000-file shape only. | Done: the generator now yields 10 000 files / 1 052 933 lines / 82.08 MiB. The first cold index at that size measured 2 min 56.3 s with a 912.6 MiB process-tree peak (a row 10 MISS); after the planner's external merge sort and the batched provider sinks landed it measures **1 min 32.1 s** with a **145.3 MiB** peak, and both rows PASS. |
| **Row 13 — base storage ≤ 3.5× eligible source bytes** | **MISS, by 21×.** On the 10 000-file generated corpus the store held 427 282 345 bytes over 5 697 273 eligible source bytes — **75.00×**, re-measured at **73.86×**: a 421 MB database over 5.70 MiB of source. The content store itself is exactly source-sized (1.00×). Two facts bound how this number should be read. The corpus averages ~570 bytes per file where Section 23.1's reference is ~8 KiB per file, so per-file fixed cost dominates it more than it would at the specified size (see row 9). And the amplifier is identified, not guessed: **per-row identity width** — 32-byte BLOB identities and wide TEXT keys replicated into every secondary index, with `WITHOUT ROWID` secondary indexes re-storing the full primary key. Per-table accounting puts relations at ~22% of the file, evidence ~22%, native_aliases ~20% and nodes ~20%. Search-text duplication, JSON payloads, WAL size and page fill were each measured and ruled out. The earlier explanation on this page — that small generated files simply cannot amortise a fixed cost — is withdrawn; it does not survive a 421 MB database. | Re-measured on the corrected reference corpus: **16.10×** (1.30 GB database over 82.08 MiB of source) — still a MISS by 4.6×, with the same amplifier. The storage-layout redesign (integer surrogate identities and key interning) has **landed**; the decision, the alternatives refused on the way and its sources are recorded in [ADR-0002 — Storage identities](adr/ADR-0002-storage-identities.md). Measured on a real 4 019-file repository indexed twice, once by each binary, with identical row counts in every identity table: 1 005 887 488 → 632 266 752 stored bytes and **5 732.8 → 3 603.5 bytes per indexed symbol, −37.1 %**. That is the per-symbol regression metric, not this row: the ratio has **not** been re-measured since, so row 13 stays a MISS and is re-recorded only from a re-run on the reference corpus **and** on real repositories. |
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
outgrows `model.MaxPageItems`. Parity at reference scale is outside what this
fingerprint establishes.

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

### 3.3 The context compile's memory: what the peak is a function of

The streamed context compile claims that its live working set is the sort run
buffer and not the repository. Two measurements on the host of Section 1, both
from `internal/context`:

| What was pushed | Records | Peak live records per sort | Heap in use, live, after the push | Spilled runs |
|---|---|---|---|---|
| Seed push sink, 5 000 entities offered twice | 10 000 | scope-seed 401 · scope-seedseq 395 · scope-entity 395 | +72 KiB | seed sorts spill |
| Seed push sink, 50 000 entities offered twice | 100 000 | scope-seed 401 · scope-seedseq 395 · scope-entity 395 | +144 KiB | seed sorts spill |
| One compile sort at the primitive's floor run budget | 20 000 | 401 (~2× the run budget in bytes, the record that triggers the spill included) | — | 50 |

Ten times the seeds, the same peak per sort and the same order of live bytes:
the sink holds one run buffer whatever discovery finds. Both rows come from
`TestTheSeedSinkPeaksOnTheRunBufferAtEitherSeedCount`; the third is
`TestACompileSortHoldsItsRunBudgetAndSpills`. Each figure is asserted by the
test that produced it, so a regression fails rather than being noticed here.

**The whole-compile scale curve.** `TestTheStreamedCompilePeaksOnTheRunBufferAtEitherScale`
publishes a fan-out fixture of N leaf entities, compiles it, and pages the
persisted manifest back. Publication is timed separately from the compile: it is
the fixture's cost and never the product's. The compile column below was taken
under the DEFAULT admission (`resources.query_memory_bytes`, 32 MiB), where a
sort of a few tens of thousands of these records never fills its 8 MiB run
buffer and nothing spills; the peak/spill column comes from the committed proof,
which sets a 1 MiB admission so both sizes are past the spill threshold, since
"the peak is the run buffer" is only a claim once the buffer binds.

| N (leaves) | Fixture publish | Compile | Page latency, persisted manifest (200 rows/page) |
|---|---|---|---|
| 500 | — | 105 ms | 0.27–0.56 ms |
| 1 000 | — | 152 ms | 0.50–0.62 ms |
| 2 000 | — | 247 ms | 0.52–0.70 ms |
| 4 000 | — | 536 ms | 0.57–1.22 ms |
| 8 000 | — | 1.272 s | 0.51–1.61 ms |
| 16 000 | — | 4.116 s | 0.48–1.64 ms |
| 2 000 (1 MiB admission) | 2.8 s | 469 ms | 0.48–0.88 ms |
| 4 000 (1 MiB admission) | 6.8 s | 849 ms | 0.45–1.07 ms |

Page latency is FLAT across the whole range — a page of the persisted plan costs
about half a millisecond whether the plan holds 509 rows or 16 009 — which is the
paging claim. The peak, at the 1 MiB admission, is likewise flat while the
spilled-run count is the thing that grows:

| N (leaves) | `scope-cand` / `scope-entity` peak | Spilled runs |
|---|---|---|
| 2 000 | 774 records | 3 |
| 4 000 | 774 records | 6 |

**The compile is superlinear, and where.** The per-doubling wall ratios are
1.45, 1.62, 2.17, 2.37, 3.24 — increasing, not constant: the local exponent at
the top of the measured range (8 000 → 16 000) is log₂ 3.24 ≈ **1.70**. The
cause WAS the ranked-impact continuation in `internal/graph`, not the context
compile's own sorts. `Engine.serveRankedImpact` served each later page by
opening the ranked spool FROM THE START and then copying every record after the
page into a fresh spool, so each page cost O(records remaining) and a walk of N
entries read to exhaustion at a 200-row page cost Θ(N²/400) record reads and
writes. A CPU profile of the 500/1 000/2 000 and 4 000/8 000/16 000 runs
attributed 0.69 s of the 1.13 s spent in `Engine.walkImpact` to
`serveRankedImpact`, of which 0.55 s was `pagination.Spools.Open` — the re-read
and the re-copy — and that share rose with N. Nothing in `internal/context` is
quadratic.

That continuation is O(page) per page: the spool is written once, by the
page that settles the order, and every later page seeks to the byte offsets its
cursor carries and reads only its own page (`pagination.Spools.OpenAt`,
`RankOffset`/`PairOffset`, cursor payload version 6). Measured on the
20 101-node deadline-split fixture at 200 rows a page
(`graph.TestADeadlineSplitWalkAlwaysAdvances`), spool bytes read per page are
flat at 88 046 on pages 3, 50 and 100 and nothing is written after the spill,
and the per-page latency is ~0.39 ms throughout, where the copying continuation
measured above fell with the shrinking remainder — 18.9 ms at page 3, 10.7 ms
at page 50, 1.9 ms at page 100. The exponent above is the "before" figure and has not been
re-measured.

**What the fixture costs, and why it is not the product.** Publishing the
fixture dominates the test's wall clock: at 16 000 leaves it is 74.3 s of the
82.7 s of CPU the test spends, almost all of it in
`storage/sqlite.(*Store).SealUnit` under `contextFixture.sealRelations` (44.2 s)
and `sealUnit` (26.9 s), with `_sqlite3BtreeIndexMoveto` alone at 39.0 s. This
is what C-FIX2 recorded as "the compile had not finished after twelve minutes":
`t.Logf` output is buffered until the test ends, so the publication of two
fixtures was indistinguishable from a hung compile. The compile at 16 000 leaves
is 4.1 s.

Reproduce with:

```bash
CODECTX_SCALE_PROOF=1 go test ./internal/context -run TestTheStreamedCompilePeaks -count=1 -v -timeout 30m
CODECTX_SCALE_PROOF=1 CODECTX_SCALE_LEAVES=2000,4000,8000 \
  go test ./internal/context -run TestTheStreamedCompilePeaks -count=1 -v -timeout 30m
```

## 7. Reproducing this page

```bash
go generate ./internal/search/textindex
go build -trimpath -o ./bin/codectx ./cmd/codectx
go test ./internal/bench -run TestResourceBudgets -count=1
go test ./internal/bench -run TestSessionStatusClamp -count=1 -v
go test ./internal/bench -run '^$' -bench . -benchmem -count=5
go test ./internal/bench -run 'TestFingerprintParity|TestCorporaManifest' -count=1 -v
go test ./internal/context -run 'TestTheSeedSinkPeaks|TestACompileSortHolds' -count=1 -v
CODECTX_SCALE_PROOF=1 go test ./internal/context \
  -run TestTheStreamedCompilePeaksOnTheRunBufferAtEitherScale -count=1 -v -timeout 30m
```

The two `internal/context` lines produce Section 3.3: the first its measured
rows, the second the compile-level rows of the same table.

That block produces every row of Section 3 except two. Rows 9 and 13 need a
cold index of the reference corpus; both are run by the verification pass and
neither is reproduced by a bench selector. Row
15's two counts are reproduced as an invariant, not as numbers — see the note
under Section 3's table.

Under `-short` the whole package skips: these rows build real workspaces and are
not unit tests. Re-record Section 1 whenever the host, the filesystem, the
toolchain, the grammar set or the enabled provider set changes — a number
without its environment is not a measurement.
