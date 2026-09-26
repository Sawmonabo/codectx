# 01 — Need-derived memory per file and per worker

Date 2026-09-26. Question: replace every fixed memory figure on the structural-parse (and, later, native dependence)
path with need learned from observation, for any repository and any host. Every `path:line` is at the head of this
branch. "The benchmark rows" means the finished parse/file rows of the public TypeScript compiler checkout at commit
`cf8cf4f6c17ada5e920949ad984791ceb9ce06df` (31,400 parse rows, 18,250 file rows; the only corpus finished when this
was written; §0 adds the all-corpora aggregate that finished later). It is **one repository**: every quantile quoted from it is evidence for the *shape* of a design, never a
constant in one. Mechanism probes are cited from `00-mechanism-probes.md` (P1–P4) and were not re-run.

## 0. What the code does today (the figures to replace, and two gaps the ADRs describe as closed)

| figure | where | what it is |
|---|---|---|
| 256 MiB per parser worker | `internal/app/compose.go:89`, reserved at `internal/provider/treesitter/pool.go:289` | one constant for every worker and every file, held from spawn to reap |
| `B_process` = 1.00 GiB | ADR-0012 §5 line 228 | stale: the code's base footprint is `config.BaseFootprint(cfg)` (`internal/config/machine.go:45-55,100-134`), whose idle term is 32 MiB (`machine.go:27`) from the 26.4 MiB measurement of ADR-0010. The "1 GiB" is the unmeasured idle constant ADR-0010 retired ("38× the measurement"), or the 1 GiB safety margin (`internal/provider/dependence/govern.go:41`) mistaken for it |
| ½ of available | `govern.go:65`, applied at `govern.go:230` | coexistence share |
| 1 GiB safety margin | `govern.go:41` | binds only where available < 2 × (base + margin) |
| 8 GiB unobserved stand-in | `govern.go:49`; every non-Linux host gets it because `ObserveMachine` returns nothing there (`internal/provider/dependence/machine_other.go:15`) | macOS and Windows are always "unobserved" |
| 768 MiB smallest child | `compose.go:108,181` | divisor that turns a byte budget into `process.Limits.MaxConcurrent` |
| per-family heap/resident constants | `govern.go:87-132` | fitted to one repository each (the comments name the repository per row) — the anti-pattern; frozen with the hosted engine |

**Gap 1 — the parser loop ADR-0012 §5 describes does not exist.** The worker's span is opened with an empty scope key
(`pool.go:573`), and the collector folds a span's peak into `scope_peaks` only when the scope key is non-empty
(`internal/ledger/collector.go:499-507`). Parser peaks never reach the learned table, and the span is per worker
*lifetime*, not per file. The learning that exists (`internal/index/plan/plan.go:862-867`,
`internal/ledger/read.go:452-485`) serves heavy units only and is **max-only, never decaying**
(`collector.go:505`: `max(peak_rss_bytes, excluded.peak_rss_bytes)`; `govern.go:207`): one pathological run raises a
scope or a whole language forever.

**Gap 2 — the allocation is not re-derived between files.** "The machine is read once" (`compose.go:711-731`); the
ledger's allocation is fixed at construction (`internal/admission/admission.go:95-105`). ADR-0012 §5's "re-derived
from the kernel's available-memory figure between files" is not implemented.

**Observed need is far below the constant.** Benchmark rows: the largest single-file native peak is 52.2 MB (a
3,151,774-byte TypeScript file); the per-source-byte native peak is p50 24.2 / p99 61.7 / max 109.2 (JavaScript),
27.1 / 67.8 / 278.1 (TypeScript), 19.3 / 35.7 / 54.7 (Go), 21.9 / 42.6 / 48.6 (TSX). For JavaScript and TypeScript,
files ≥ 64 KiB have *lower* ratios (p50 17.7 / 17.9) than files < 4 KiB (p50 24.1 / 27.3, max up to 278.1); Go is flat
(p50 21.0 vs 19.0). The need is affine where it is not flat (an intercept plus a slope), not a bare ratio. **All seven public corpora** (the corpus benchmark finished over llvm-project, kubernetes, elasticsearch,
home-assistant core, vscode, the TypeScript checkout and rust-lang/rust, all at the commits of the corpus matrix, binary
`36a529f`; figures from its per-corpus aggregate, files ≥ 4 KiB): native bytes per source byte p50 9.6 (C,
llvm-project) to ≈ 17–26 for every other language; p99 up to 45.0 (C++, llvm-project) and 72.6 (Rust, rust-lang/rust);
max 300.8 (C++, llvm-project). The largest single-file native need is 303.9 MiB for a 15.4 MiB C file (llvm-project) —
**above** the 256 MiB constant — then 135.1 MiB for an 8.0 MiB TypeScript file (vscode) and 131.9 MiB for a 2.6 MiB
Rust file (rust-lang/rust), while the median file needs well under 1 MiB. The constant is therefore wrong in both
directions at once: hundreds of times too large for the median file and too small for the largest. Scanner bytes are
uncounted for Python, C++ and Rust in every corpus. The same bytes vary by content by three orders of magnitude: a 4,029,319-byte
Go file peaked at 0.92 MB and a 1,144,573-byte one at 17 KB (data literals), against 39.2 MB for a 1,501,771-byte one.
Transient above the final tree is small: (peak − tree) / source p50 0.004, max 6.4 B/B on files ≥ 64 KiB; the input
copies the binding makes are 1.00–2.00× the source on the TypeScript checkout's 159 files ≥ 64 KiB (p50 1.23), and
1.00–2.29× on the matrix's 3,827 (p50 1.01; §3 gives every population).

## 1. The finding that reframes the design: two quantities, not one

A parser worker is long-lived and its C allocator keeps freed pages. The benchmark rows show it: of the 38 files whose
native peak is ≥ 4 MiB, **31 moved the process's resident set by less than a quarter of that peak** (a 2,348,669-byte
file built a 19.98 MB tree with a resident-set change of 0). Consequently a per-file peak read inside a warm process —
`clear_refs=5` then `VmHWM` (P1) — measures *marginal new residency*, not the file's need: it reads about zero for any
file smaller than something the worker already held. P1 proved the reset in a process that returned its memory; a
parser worker does not.

So there are two quantities, and each has its own right instrument:

- **E_w, the worker's envelope**: what the machine has actually given worker *w*, which admission must hold while the
  worker lives. A lifetime peak (or current resident set) is exactly the right reading, and one exists on every
  platform.
- **need(f), a file's need**: what a worker grows by, above its idle base, to parse *f*. This is what must be predicted
  before dispatch and learned after. It is observable per file only if the worker's residency returns to its base
  between files, or through the counting allocator.

## 2. Sub-answers

### (a) The worker measures its own per-file peak (`clear_refs=5` + `VmHWM`) and ships it in `Done`

**Recommendation.** Adopt, with the release step that makes it a measurement of need rather than of marginal growth:
after each file the worker closes the tree, returns freed C pages to the OS (glibc `malloc_trim(0)`), reads its base
from `/proc/self/status` (`VmRSS`, `RssAnon`, `RssFile`), writes `5` to `/proc/self/clear_refs`, and after the next
file reads `VmHWM`. `need(f) = (VmHWM − base) − ΔRssFile`, where `ΔRssFile` (from the same `status` read, before and after) removes
the file-backed grammar tables and query rodata a worker faults on its first parse of a language — shared, untouched by
the trim, and otherwise one inflated observation per worker per language; `Done` carries `need`, `base` and the post-file `RssAnon`. Cost: one
status read is 6.9 µs (P2); the reset branch does no mapping walk (kernel `fs/proc/task_mmu.c`, the
`CLEAR_REFS_MM_HIWATER_RSS` branch calls `reset_mm_hiwater_rss(mm)` and jumps to unlock —
https://raw.githubusercontent.com/torvalds/linux/master/fs/proc/task_mmu.c, lines 1895-1901 at fetch time); the write's
own cost was not measured (unavailable). The trim's cost is the refaults of the next file's pages; it is unmeasured
(unavailable) and is the thing the benchmark must price. The trim is also a memory win in itself: without it every
worker keeps, forever, the residency of the largest file it ever parsed, and 16 workers hold 16 such envelopes.
Today's `Done.RSSBytes` is the current resident set from `statm` with the tree still open (`worker.go:156` defers
`tree.Close` past the write at `worker.go:163-167`; `wire.go:280-294`), so it already sits at peak minus transient;
the change is small.

**Strongest alternative, steel-manned.** Keep the warm worker and treat the reading as *censored*: when `VmHWM` rose
above the base the file set a new maximum and the reading is exact; when it did not, the need is known only to be
≤ the worker's current residency. Nothing is refaulted, no glibc call is made, and the ledger holds what the worker
really holds.

**Why the recommendation wins.** The censored scheme learns only from new maxima; with longest-first dispatch
(ADR-0012 §5) the first file a worker parses is its largest, so after it almost every later observation is censored
and the model never sees the ordinary file. It also leaves every worker's envelope at its lifetime maximum, which is
exactly the resident memory the owner wants minimal. The trimmed scheme observes every file exactly and returns the
envelope to base between files.

**Trade-off accepted.** Per-file refault cost; a glibc-specific call (musl and non-Linux need their own release call or
fall back to the censored reading); on a kernel without `CONFIG_PROC_PAGE_MONITOR` the `clear_refs` write fails and the
worker falls back to `Done`-time current residency with the tree open (≈ need, since transient is ≈ 0 in the rows).

**Platforms.** Linux: exact, unprivileged (P1 ran as the user). macOS: the interval peak exists
(`ri_interval_max_phys_footprint`, xnu `bsd/sys/resource.h:330`) and a reset exists (`proc_reset_footprint_interval`,
xnu `libsyscall/wrappers/libproc/libproc.c:755`) but is declared only in `libproc_internal.h:154` — private API, not
shippable; the public reading is the monotone `ri_lifetime_max_phys_footprint` (`resource.h:325`). Windows:
`PeakWorkingSetSize` is lifetime only, no reset
(https://learn.microsoft.com/en-us/windows/win32/api/psapi/ns-psapi-process_memory_counters). On both, per-file need is
the current footprint at `Done` with the tree open, taken after the platform's release call; whether that release call
returns the parser library's pages on each platform is unavailable (not verified here). A fork-per-file worker would
make "lifetime" equal "per file" everywhere, but pays a process spawn and a grammar/query rebuild per file across
10⁴–10⁶ files — rejected.

**The measurement the benchmark task must take to confirm it.** Per repository class (one large repository per language family plus a mixed monorepo): for every file,
`need` from the trimmed worker vs the counting allocator's native peak + input copies on the same file; pass when
`need ≥ native_peak + input_copy` on ≥ 99% of files and the parse wall with trim is within the benchmark's stated
noise of the wall without; report the trim's added wall per class.

### (b) The parent's 250 ms tree sampler

**Recommendation.** Keep it as the *envelope* cross-check and for non-parser children; do not use it for per-file need.
31,398 of the 31,400 benchmark parses finished in under 250 ms (max 337 ms), so the sampler (`runner.go:228`) almost
never takes a sample *during* a file; it sees worker drift. Its cost scales with the host: each sweep reads every
`/proc` entry (`internal/process/treesample_linux.go:141-169`), one sampler per running child (`runner.go:577`), so 16
workers are 16 full `/proc` scans every 250 ms. It is Linux-only (`treesample_other.go:17` returns nil).

**Strongest alternative, steel-manned.** Shorten the interval until it sees files: one mechanism, in the parent, that
trusts nothing the child reports. **Why the recommendation wins:** a 1 ms interval would be 250× the `/proc` scans and
still miss sub-millisecond parses, which are most files; the worker observes itself for free. **Trade-off accepted:**
the parent no longer independently verifies per-file figures; it verifies the envelope. **The measurement the
benchmark task must take to confirm it:**
per class, the sampler's worker peak vs `max(base + need)` reported through `Done`; pass when the sampler never exceeds
the worker-reported maximum by more than one file's need.

### (c) `smaps_rollup`

**Recommendation.** Do not use it per file (183 µs, 27× `status`, P2); the per-file *delta* already cancels shared
text. Use `status` fields for the envelope: charge each worker its `RssAnon` and charge the file-backed text
(`RssFile`, shared by all workers of one binary) once. `smaps_rollup`'s `Pss` is the exact apportionment and earns its
cost only once, at worker start, if the benchmark shows `RssFile` differs materially from the text's `Pss`.
**Strongest alternative, steel-manned:** `Pss` for every reading, the only exact per-process share of shared pages.
**Why the recommendation wins:** 183 µs per file across 10⁶ files is minutes of CPU for a correction the
`RssAnon`/`RssFile` split makes at 6.9 µs. **Trade-off accepted:** shared anonymous memory (none expected in a worker)
would be double-counted. **The measurement the benchmark task must take to confirm it:** per class, Σ workers' `RssAnon` + one `RssFile`
vs Σ `Pss` at worker start; pass within 5%.

### (d) cgroup v2 `memory.peak`

**Recommendation.** Reject as a primary signal. It measures what P1 measures (P4), but needs a delegated subtree per
worker and one held descriptor per worker (the reset is per descriptor, P4); a process started from an ordinary shell
here sits in root-owned `init.scope` and cannot create a child (P3); macOS, Windows, most containers and CI runners have
no delegated user manager. Buck2, the one build system found that uses cgroups for actions, states the precondition in
its source: the daemon must be started "with something like systemd-run" and hold `Delegate=yes`
(https://raw.githubusercontent.com/facebook/buck2/main/app/buck2_resource_control/src/buck_cgroup_tree.rs, the
start-up doc comment). **Strongest alternative, steel-manned:** use it where delegated and fall back elsewhere — it is
kernel-attributed, covers the whole worker tree and counts charged page cache. **Why the recommendation wins:** two code
paths for the same quantity, one reachable only on a configured Linux host. **Trade-off accepted:** no kernel-enforced
per-worker limit (none is wanted: ADR-0010 forbids refusing for size) and no page-cache attribution. **The measurement
the benchmark task must take to confirm it:** none is needed to keep it rejected; what would revive it is a survey of
the fraction of target hosts (developer machines, CI runners, containers) on which the product's process can obtain a
delegated memory subtree unprivileged — if that were near all of them, re-open.

### (e) The counting tree allocator as a live signal

**Recommendation.** Keep it in the benchmark (attribution, owned by the per-grammar research); do not install it in the
production worker. Its measured overhead is ≥ 18% of parse time against a *paused* counter (Σ counted/Σ paused 1.18
over 31,400 files, 1.22 on files ≥ 64 KiB), and the paused run still takes the lock on every free and realloc
(`internal/bench/allocator.go:79-84`), so the overhead against no hook is larger and unavailable. It cannot see the
external scanners of cpp, rust and python (`allocator.go:38-50`; their rows carry `scanner_bytes: null`), the binding's
input copies (`allocator.go:51-61`), the Go heap, or the dependence arena. A cheaper header-prefix counter is unsafe:
the binding frees libc-allocated C strings through the hook (`allocator.go:51-58`), and a header read on a foreign
pointer is a crash. **Strongest alternative, steel-manned:** it is portable to every platform and the only signal that sees the
20 MB need behind a zero resident-set change. **Why the recommendation wins:** the trimmed worker (a) sees the same
need on Linux without the 18%+, and it also sees what the counter cannot (scanners, copies, Go heap, arena — the file
rows show arena peaks up to 9.85 B/B on files ≥ 64 KiB). **Trade-off accepted:** macOS and Windows per-file need is the
coarser `Done`-time footprint. **The measurement the benchmark task must take to confirm it:** as (a): `need − (native_peak + input_copy)` per file per class is the
invisible remainder; report its quantiles per grammar.

### (f) A per-language need model learned online

**Recommendation.** For each (repository, language, grammar fingerprint `lang.Fingerprint()`), keep a decaying
histogram of `need / source_bytes` per log₂ size class, with exponential buckets (VPA's 5% growth,
https://raw.githubusercontent.com/kubernetes/autoscaler/master/vertical-pod-autoscaler/pkg/recommender/model/aggregations_config.go).
- **Statistic:** the weighted p99 of the class. A class with fewer than 100 observations has a p99 equal to its sample
  maximum, so no separate "minimum history" constant is needed. p99 is a design constant with its reason — an
  under-reservation is never a failure (the file runs; ADR-0010), it spends some of the host's kept half for one file's
  duration, and the product then expects about 1% of files to overrun and discloses them — in the same standing as
  `hostShareDenominator` (`govern.go:51-65`); it is not fitted to any repository. VPA uses p90 for the target and p95
  for the upper bound with a 15% margin
  (https://raw.githubusercontent.com/kubernetes/autoscaler/master/vertical-pod-autoscaler/pkg/recommender/config/config.go);
  SQL Server uses "a high percentile of past memory grant sizing requirements"
  (https://learn.microsoft.com/en-us/sql/relational-databases/performance/intelligent-query-processing-memory-grant-feedback).
- **Decay:** by observations, not by clock or generation: older weight halves every time the repository's own count of
  files of that language has been observed again, so a full re-index replaces half the history and an incremental
  generation of ten files barely moves it. The half-life is derived from the repository in front of the product.
- **Persistence:** a ledger table beside `scope_peaks`, keyed (repository, language, grammar fingerprint, size class),
  bucket weights as its payload, written at the end of each generation. Bounded by construction: 9 grammars × at most
  one class per power of two up to `wire.MaxSourceOffset` × about 190 buckets (5% steps over four decades of B/B).
- **Unseen class:** borrow the ratio of the nearest populated class. Where ratios fall with size (JavaScript and
  TypeScript in the rows), borrowing from a smaller class over-estimates a larger file (safe) and borrowing from a
  larger class under-estimates a smaller one by about the p50 gap (24 vs 18 B/B) — small absolute bytes, corrected at
  once because small files are numerous; where ratios are flat (Go), borrowing either way is close. No direction is
  assumed in the design: an under-estimate is an overrun, which runs and is observed.
- **Weights:** "sample maximum below 100 observations" is stated in total weight: under decay a faded extreme drops
  out of the p99 tail, which is the intended forgetting.
- **File larger than any seen:** the largest populated class's ratio × its bytes. It is observed exactly when it runs
  and enters the model before the next admission; no bump constant is needed because, unlike VPA's OOM-truncated
  observation (VPA bumps by `max(+100 MB, ×1.2)`, `aggregations_config.go` `DefaultOOMBumpUpRatio`/`DefaultOOMMinBumpUp`),
  the worker completes and reports its true peak. A worker the kernel kills mid-file is the censored case: the file is
  retried once alone (runs-alone rule) on a fresh worker, whose reading is exact.

**Strongest alternative, steel-manned.** Max-only, as `scope_peaks` does today: never under-reserves for any size seen,
has no quantile constant, and is one row. **Why the recommendation wins:** max-only never forgets, so one generated
file poisons a language on that repository forever (SQL Server's lesson: feedback that follows one extreme execution
oscillates and "disables itself"; the percentile over history is its fix, same URL); and a max has no per-size
structure, so a 3 MB file's peak would be reserved for every 1 KB file. **Trade-off accepted:** ~1% of files overrun
their reservation by design, disclosed; one design constant (p99) and one derived half-life. **The measurement the benchmark task must take to confirm it:** per class,
the fraction of files whose `need` exceeds the reservation after the first generation (pass: ≤ 2%) and the summed
reservation vs summed need (report the over-reservation ratio); across two consecutive generations of the same
snapshot, the model's p99 per language changes by less than the bucket width.

## 3. The explicit answers

**The first file of a never-seen language** ("never seen" = no model row for this repository, language and grammar
fingerprint; all nine grammars are pinned, so nothing is unknown at build time except the repository's content). It is
reserved at a **structural prior**, with no constant fitted to a repository:

`reserve = base_w + source_bytes × (s_node + c_copy + c_src)`

- `base_w`: the worker's observed idle base (the `Hello` frame gains the worker's `RssAnon`, `wire.go:88-92`); before
  the first worker exists, the parent's own resident set at composition (same binary, same runtime).
- `s_node`: the worst case of one heap subtree per source byte, from the vendored structs:
  `ts_subtree_alloc_size(child_count) = child_count × sizeof(Subtree) + sizeof(SubtreeHeapData)` (go-tree-sitter
  v0.25.0 `src/subtree.h:248-250`). On LP64, hand-computed from `subtree.h:111-154`: header fields 44 bytes, 11 flag
  bits 2 bytes, union aligned to 8 at offset 48, union 32 bytes (the `ExternalScannerState` arm: 24 + 4 → 32) —
  `sizeof(SubtreeHeapData) = 80`; `sizeof(Subtree) = 8` (a union of a pointer and 8 bytes of inline data). Each heap
  node costs 80 + its 8-byte slot in its parent's allocation + allocator chunk overhead (≤ 16 bytes, an
  assumption about glibc): `s_node = 80 + 8 + 16 = 104`. The benchmark must assert both sizes through cgo.
- `c_copy = 2` (the binding's input copies), `c_src = 1` (the worker's Go-side source buffer).
- So the prior is `base_w + source_bytes × (104 + 2 + 1) = base_w + 107 × source_bytes`.

`c_copy = 2` is a prior term, not a bound. The adapter copies every chunk the read callback returns into a C string
held until the parse ends (`internal/bench/allocator.go:51-56`), so a chunk the parser reads again is copied again.
Over the finished tree parse rows of the seven corpora (`input_copy_bytes ÷ source_bytes`, per file): 230,366
non-empty files, p50 1.002, p99 1.235, max 51.6, 212 above 2; the 74,059 files ≥ 4 KiB, p99 1.387, max 51.6, 20
above 2; the 3,827 files ≥ 64 KiB, p50 1.013, p99 1.849, max 2.29, 12 above 2.

**The count, at the prior as derived.** The prior covers three terms, so each file is compared on the same three:
its counted native peak (`native_peak_bytes`) + its measured input copies (`input_copy_bytes`) + its source bytes
(the buffer), against `107 × source_bytes`; the worker base is on both sides and drops out. Counted per file over
every finished `origin = tree` parse row of the seven corpora (230,566 rows; the row files behind `06`), not from the
aggregate:

| population | files | above 107 B/B | largest (B/B) |
|---|---|---|---|
| ≥ 64 KiB | 3,827 | 0 | 89.25 (a 78,294-byte Rust file, rust-lang/rust) |
| ≥ 4 KiB | 74,059 | 9 | 302.8 (a 10,479-byte C++ file, llvm-project) |

The nine, by the same quantity: 302.8 C++ and 262.1 C (llvm-project), 152.4 JavaScript (vscode), 149.8, 144.1 and
125.5 Rust (rust-lang/rust), 113.9 C (llvm-project), 111.2 JavaScript (the TypeScript checkout, with a syntax error),
108.0 Rust (rust-lang/rust). The counted native peak alone exceeds 107 B/B on 8 of the 74,059 (the 106.0 B/B Rust file
drops out) and on none of the 3,827, whose largest is 87.25 B/B (the same Rust file; the TypeScript checkout's largest
is 76.4).

Verdict on the hint: **confirmed as a prior, refuted as a bound.** The prior holds on every file ≥ 64 KiB, and the
GLR transient (max 6.4 B/B) fits inside it; it is exceeded by 9 of 74,059 files ≥ 4 KiB. But "≤ one heap node per
source byte" is an
assumption, not a theorem: unary chains (`expression → identifier`) and zero-width tokens (missing nodes, scanner
indent/dedent) can exceed it, and files < 4 KiB reach 278 B/B (the per-parse intercept; absolute bytes are tiny). The
design does not need a bound — an overrun runs — so a prior is sufficient. With longest-first dispatch the prior is
paid on each language's *largest* file — exactly the size range where it held on every corpus: on the TypeScript
checkout's 3,151,774-byte file it reserves ≈ 337 MB against an observed 52 MB for 239 ms, and on llvm-project's
15.4 MiB C file ≈ 1.7 GB against 303.9 MiB, costing concurrency for one file, never a failure; a file whose prior exceeds the allocation runs alone
(`admission.go:113-116,226`).

**How it converges.** After one file the class has a sample maximum; after 100, a p99; the prior is never used again for
that (repository, language, grammar). Each generation's observations enter the persisted histogram, so the second run
of a repository starts converged.

**A file whose observed peak exceeds its reservation.** It has already run and is never refused (ADR-0010 decisions 2 and 5;
`admission.go:113-116`). The worker's `Done` reports it; the pool raises the worker's ledger holding to the observed
figure at once (a ledger *adjust* that records, without waiting, memory already taken — the ledger needs this one
operation, since today a reservation is fixed from grant to release, `admission.go:147-188,270-279`), the observation
enters the model before the next admission, and the drift (reserved, observed) is disclosed per ADR-0010 decision 4.

**Admission shape.** A worker holds `base_w` for its lifetime (spawn reservation, `pool.go:289`); before each file the
pool reserves `max(0, base_w + predicted_need − held_w)` and after `Done` adjusts the holding back to the post-trim
`RssAnon`. The ledger is FIFO across every reserver (`admission.go:20-25`), so a large file at the head blocks smaller
reservations behind it; with longest-first dispatch that is the intended order. The parser runner beneath must never
refuse what the ledger admitted (`runner.go:426-427` refuses a spawn reservation above its budget): its budget becomes
`max(allocation, largest base_w prior)` in place of `max(childMemory, 256 MiB)` at `compose.go:780`; per-file
increments are ledger-only and never reach the runner.

**Forward progress (required, or the shape deadlocks).** The ledger's runs-alone rule fires only when nothing is
admitted (`admission.go:226`). With W workers admitted holding their bases and no parse in flight, a head increment that
does not fit would wait for a release nothing will produce; the same happens when a re-derived allocation falls below
Σ bases while every worker is idle. `stopIdle` (the make-room step passed at `pool.go:289`) frees *other* idle workers'
bases, never the requester's. The rule the design states: **a per-file increment is granted whenever no parse is in
flight** — the runs-alone rule restated for the increment class, since held bases are memory already taken, not work
that will release. This is Buck2's guarantee that the first running scene is never suspended: "Without this kind of a
guarantee, we risk creating a deadlock where no scene ever finishes before it's killed and retried" (`scheduler.rs`,
the `running_scenes` comment).

**"Half of available."** It is a coexistence share, not a need, and **no derivation from need exists**: need says how
much the product would use, never how much the person's editor, browser and agents will want in the next minute. Keep
it, as `hostShareDenominator` with its reason. Steel-man of the alternative — Buck2's: its scheduler keeps
`estimated_memory_cap`, "the total memory in use by buck the last time we saw significant memory pressure", which "also
implicitly incorporates information about what processes other than buck are doing", and freezes or kills-and-retries
actions under pressure
(https://raw.githubusercontent.com/facebook/buck2/main/app/buck2_resource_control/src/scheduler.rs, the
`estimated_memory_cap` field comment and the `CgroupFreeze`/`KillAndRetry` variants). That is an observed coexistence
limit rather than a fixed share. It loses here because (1) it learns the limit by *causing* pressure — the person's
editor stalls first, which is the failure ADR-0010 was written to prevent; (2) its attribution needs a cgroup per action
(d); (3) PSI (`/proc/pressure/memory`, readable unprivileged on this host) is Linux-only, so the share is still needed
everywhere else. PSI is worth recording as a later Linux-only brake (stop admitting new increments while memory stall is
rising), not as the allocation. What must change is Gap 2: re-derive the allocation between files as
`min(A − base − margin, A / 2)` with `A = ObserveMachine(t) + own(t)` — `ObserveMachine` already takes the smaller of
`MemAvailable` and the control-group headroom (`internal/provider/dependence/machine_linux.go:31-43`), which is what
binds in a container — where `own(t)` is the product's own observed
resident set (Σ workers' `RssAnon` from their last `Done` plus the parent's), so the product's own growth is not read as
a competitor and does not throttle itself.

**`B_process`.** It becomes `config.BaseFootprint(cfg)` — already derived from the machine and the configuration in
code (`machine.go:45-55,100-134`) — with its one measured term, `IdleFootprintBytes = 32 MiB` (`machine.go:27`),
replaced by the parent's own resident set read at composition (Linux `status`, macOS `task_info`, Windows
`GetProcessMemoryInfo`), keeping 32 MiB only where the platform returns nothing. ADR-0012 §5's "1.00 GiB" is stale and
should read as the derived function. One term the formula lacks: the parent buffers every fact of an in-flight file
before returning it (`pool.go:757-789` appends decls, imports and refs), so `W` in-flight files cost parent memory in
proportion to their facts; that belongs either in the per-file reservation or to streaming (the least-memory question).

**The other fixed figures.** Safety margin 1 GiB: not a need figure; it binds only on hosts with < ≈ 2 GiB available,
where it is the floor that keeps them usable — keep as a stated coexistence constant (no derivation from need exists),
or fold into the p99 headroom once the model covers every child. 8 GiB unobserved stand-in: shrink its reach by
observing available memory on macOS (Mach `host_statistics64`) and Windows (`GlobalMemoryStatusEx`) instead of
declaring both unobserved (`machine_other.go:15`); those two calls are named from platform knowledge and were not
fetched here (unavailable as citations). 768 MiB smallest child: a count divisor, not a reservation; the parser runner's
count is already the core count, so it leaves this path.

## 4. How others solve it (fetched)

- **Bazel:** a spawn's cost is subtracted from a tracker and returned at completion; the default action cost is
  "cpu: 1, memory: 250 MB" and `resource_set` is a static callback of (os, number of inputs)
  (https://bazel.build/rules/lib/builtins/actions); the team called its memory estimates "basically just random
  numbers" (https://groups.google.com/g/bazel-discuss/c/f7ffwBja7iQ); it does not measure actual use
  (https://jmmv.dev/2019/12/bazel-local-resources.html). `--experimental_collect_resource_estimation` is profiler-only
  (https://bazel.build/reference/command-line-reference). Nothing is learned.
- **Pants:** `process_per_child_memory_usage` default 512 MiB, pool sized from `process_total_child_memory_usage`
  (https://www.pantsbuild.org/stable/reference/global-options). Static.
- **Buck2:** pressure-driven, cgroup-attributed, freeze or kill-and-retry; the first running scene is never suspended,
  guaranteeing progress (`scheduler.rs`, the `running_scenes` comment). Needs delegation (d).
- **Kubernetes VPA:** decaying histogram (24 h half-life, 5% buckets, one peak per 24 h window), p90 target / p50 lower /
  p95 upper, 15% margin, OOM bump `max(used + 100 MB, used × 1.2)` (URLs in (f); `container.go` RecordOOM at
  https://raw.githubusercontent.com/kubernetes/autoscaler/master/vertical-pod-autoscaler/pkg/recommender/model/container.go).
  The model this design adapts, with decay by observations instead of clock.
- **SQL Server memory-grant feedback:** increase on spill, decrease when > 2× over-granted (batch) or < 50% used (row),
  disables itself on oscillation, and the fix is a high percentile over recent history persisted in the Query Store
  (URL in (f)).
- **PostgreSQL:** `work_mem` 4 MB per operation, `hash_mem_multiplier` 2.0, spill to disk beyond it; concurrent
  operations multiply it (https://www.postgresql.org/docs/current/runtime-config-resource.html). Static; the analogue of
  a per-worker constant.
- **Spark:** `spark.memory.fraction` 0.6 and `memoryOverheadFactor` 0.10 are static fractions
  (https://spark.apache.org/docs/latest/configuration.html); its "adaptive" memory is execution/storage borrowing inside
  one heap, not a learned reservation.

Only VPA and SQL Server learn a per-unit reservation from observation (Buck2 learns a whole-daemon cap); both converge by a high percentile over decaying,
persisted history. Neither has the product's advantage: the worker reports its exact per-file peak, so no bump for a
censored reading is needed except after a kernel kill.

## 5. Recommended design (one paragraph per mechanism)

1. **Worker self-measurement.** After each file: close tree, release freed C pages, read base from `status`, reset
   `VmHWM` (`clear_refs=5`); after the next file read `VmHWM`; `Done` carries `need`, `base`, `RssAnon`. `Hello`
   carries the idle base. macOS/Windows: `Done`-time footprint with the tree open after the platform's release call.
2. **Envelope accounting.** A worker holds its observed base; each file adds its predicted increment before dispatch;
   `Done` adjusts the holding to what the worker actually holds (up for an overrun, without waiting; down after the
   trim). Shared text counted once (`RssFile`), private per worker (`RssAnon`).
3. **Model.** Per (repository, language, grammar fingerprint, log₂ size class) decaying 5%-bucket histogram of
   need/byte; weighted p99 (sample max below 100 observations); half-life = the repository's own file count for that
   language; persisted per generation in a bounded ledger table; structural prior only for an empty model.
4. **Allocation.** Keep the ½ share and the margin as coexistence constants with reasons; re-derive the allocation
   between files from `MemAvailable` plus the product's own observed residency; observe available memory on macOS and
   Windows so the 8 GiB stand-in applies only where observation truly fails.
5. **Learning loop fix.** Parser observations go to the new table (not `scope_peaks`, whose empty-key skip at
   `collector.go:500` stays correct for scope-less spans); `scope_peaks` for heavy units moves from max-only to the same
   decaying quantile.
6. **Disclosure.** Per generation and per language: reservation, observed need quantiles, overrun count and drift.
7. **Forward progress.** A per-file increment is granted whenever no parse is in flight (§3).

**Strongest alternative, steel-manned.** Keep the constants and make them honest: a per-worker reservation sized by
`workspace.max_parse_file_bytes` × a structural slope, max-only learning in `scope_peaks`, and a cgroup where one is
delegated. Nothing is predicted, nothing can under-reserve for a size already seen, and the ledger needs no adjust
operation. **Why the recommendation wins:** a per-worker constant must cover the largest admissible file on every worker
at once — the corpora's largest need is 303.9 MiB against a median well under 1 MiB — so it reserves orders of magnitude
more than a run uses, and it is a figure the policy forbids; max-only never forgets; the cgroup is unavailable on most
hosts (d). **Trade-off accepted:** a ledger adjust operation, a trimmed worker, one persisted table, one design constant
(p99), and about 1% of files running over their reservation by design, disclosed.

**The measurement the benchmark task must take to confirm it (the whole design)** (per repository class: one large repository per language
family plus a mixed monorepo): Σ reserved vs Σ observed need and peak whole-tree residency against the current 256 MiB
design at the same worker count; overrun fraction ≤ 2% after the first generation; the trimmed worker's wall within
noise of the untrimmed; the structural prior ≥ observed need on the first (largest) file of every language in every
class. A class where the prior is below observed need refutes the struct-size prior and the design falls back to
"first file of a language runs alone".

## Unavailable (with the reason)

- The counting allocator's overhead against no hook at all (the benchmark only times against a paused counter).
- The per-file cost of `malloc_trim(0)` and the refaults it causes; the cost of the `clear_refs` write (P2 timed reads).
- Whether each platform's release call returns the parser library's pages on macOS and Windows; the macOS and Windows
  available-memory calls were named, not fetched.
- Scanner allocations for Python, C++ and Rust (uncounted in every corpus); they are inside the trimmed worker's
  `VmHWM` reading and outside every benchmark figure quoted here.
- `sizeof(SubtreeHeapData)` measured rather than hand-computed (needs a cgo build, which this research may not run).
