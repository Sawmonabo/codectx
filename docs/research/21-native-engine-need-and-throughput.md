# The native dependence engine: need-derived memory, least wall time, least resident memory, publication, and the corpora that judge it

Research note for the codectx product owner, extending [20-native-engine-post-mvp](20-native-engine-post-mvp.md) and
[ADR-0012](../adr/ADR-0012-native-dependence-engine.md). Research date 2026-09-26, at commit `b6dd3f6`.

**The direction it answers.** The product serves any enterprise codebase, superlarge included. It must size itself from
what the repository and the machine in front of it need. It must produce the same outputs in the least wall time with
the least resident memory. No constant, threshold or reservation may be fitted to one repository: a figure measured on a
corpus is evidence for a design and never a constant in code.

**What this note rests on.** Seven raw files under `raw/native-engine-need/`, cited by number:

- `00` — host probes of the peak-memory mechanisms.
- `01` — need-derived memory.
- `02` — wall time.
- `03` — resident memory.
- `04` — publication.
- `05` — corpora and goldens.
- `06` — the dependence-core benchmark aggregate over the public corpus matrix.

Every figure in the raw files cites `path:line` at `b6dd3f6` or a fetched URL. `06` was produced by the benchmark task
from a test binary built at `b6dd3f6`, run memory-capped, one corpus at a time. This research ran no benchmark and never
ran the product. Its only executions were the four KB-scale probes of `00`.

## 0. Bottom line

1. **The structural "parse wait" is the store's lock, not the parse.**
   - A worker's span is its process lifetime, so 6.2 s of wall against 0.13 s of processor time means a worker busy
     about 2% of the time (`02` §a).
   - The stage's wall is the per-unit work around the parse. Every per-unit store call takes one mutex: ingestion calls
     at `internal/storage/sqlite/open.go:710` and reads at `:830`. A unit makes about ten of those calls, plus one or
     two reads per extracted record.
   - The disk is not the cause: under the ingestion group a file unit costs zero commits and zero fsyncs.
2. **In the native engine's own code, the per-function lowering walk dominates, not the algorithms.**
   - kubernetes Go: 228,765 functions, 11.6 s in total, of which the three dependence passes are 0.54 s (`06`).
   - The Go lowering resolves fields by name 49 times. The binding allocates a C string on every one of those calls
     (`go-tree-sitter@v0.25.0/node.go:189-192`).
   - The JavaScript lowering already resolves field ids once per process.
3. **The 256 MiB per parser worker is wrong in both directions.**
   - The median file needs under 1 MiB of native memory in every language.
   - The largest file measured needs 303.9 MiB: a 15.4 MiB C file in llvm-project (`06`).
   - A per-file need is observable exactly by the worker itself (`00` P1), but only if the worker returns freed pages
     between files. Otherwise the C allocator reuses what it holds and the reading is censored (`01` §1, `03` §a).
4. **Two loops the ADRs describe as closed are open in code** (`01` §0).
   - Parser observations are never learned: worker spans have an empty scope key (`pool.go:573`), and the peak
     collector skips those (`internal/ledger/collector.go:500`).
   - The allocation is read once at composition, not re-derived between files.
5. **The native families are best published by the structural provider**, from the same worker walk that parses the
   file. A new package-scoped linking provider takes `calls` later. The `dependence` id is never reused, because the
   phase-3 gate counts dependence-provider rows (`04` §a).
6. **Found while measuring: a C++ header is parsed as C.** `.h` belongs to the C grammar
   (`internal/provider/treesitter/lang/lang.go:79`). On llvm-project, 74.7% of `.h` files parse with errors, against
   22.8% of `.c` files (`06`). A header's language is a property of the repository, so an extension map cannot decide
   it.

## 1. Need-derived memory, per file and per worker (`01`, `00`, `06`)

### What the code does today

| figure | where | status |
|---|---|---|
| 256 MiB per parser worker | `internal/app/compose.go:89` | one figure for every file and every worker |
| 1 GiB process base in ADR-0012 §5 | ADR text only | stale; the code already derives it (`config.BaseFootprint`, `internal/config/machine.go:45-55`) |
| 32 MiB idle term | `machine.go:27` | measured once, on one fixture |
| half of available | `hostShareDenominator`; ADR-0010 decision 2 | a coexistence share |
| allocation | `compose.go:711-731`, `internal/admission/admission.go:95-105` | read once, never re-derived |
| learning | `scope_peaks`, `collector.go:499-507` | max-only, never decays; parser spans skipped |

### Observation mechanisms, measured

| mechanism | cost | availability | per-file accuracy | verdict |
|---|---|---|---|---|
| worker's own `clear_refs=5` then `VmHWM` | 6.9 µs per `status` read (`00` P2) | Linux; macOS and Windows get a footprint at `Done` | exact once freed pages are returned | **chosen** |
| parent's 250 ms tree sampler | scans `/proc` per child per sweep (`treesample_linux.go:141-169`) | Linux only | 31,398 of 31,400 parses finish in under 250 ms (`01` §b) | worker-envelope cross-check only |
| `smaps_rollup` | 183 µs per read (`00` P2) | Linux | separates shared from private memory | envelope accounting only |
| cgroup v2 `memory.peak` | cheap | needs a delegated subtree: none from `init.scope` here, and the reset is per file descriptor (`00` P3, P4) | exact | rejected as a dependency |
| counting tree allocator | ≥ 18% of parse time (`01` §e) | the benchmark only | misses the scanners, the input copies, the Go heap and the arena | stays in the benchmark |

### The recommended design

- **Measure per file.** After each file the worker closes the tree, releases freed C pages, reads its base and resets
  its peak. After the next file it reads the peak and reports need, base and anonymous resident set in `Done`.
- **Hold envelopes.** A worker holds its observed base for its lifetime and reserves each file's predicted increment
  before dispatch. `Done` adjusts the holding to what was actually used, upward without waiting.
- **Grant forward progress.** A per-file increment is granted whenever no parse is in flight. This restates the
  runs-alone rule; Buck2's scheduler states the same rule ("the first running scene is never suspended").
- **Learn a model.** Keep a decaying 5%-bucket histogram of need per source byte, per repository, language, grammar
  fingerprint and size class. Reserve its weighted p99, which is the sample maximum below 100 observations. The
  half-life is measured in that repository's own file count for the language. The model is persisted per generation.
- **The first file of a never-seen language** is reserved at a structural prior:

  > worker base + source bytes × (80 + 8 + allocator overhead, per heap node, from `subtree.h:111-154,248-250`; + 2
  > for the binding's input copies; + 1 for the worker's buffer) ≈ base + 107 × bytes.

  - Nothing in the prior is fitted to a repository.
  - It holds on all 3,827 files of 64 KiB or more across the seven corpora.
  - It is exceeded by 9 of 74,059 files of 4 KiB or more (the worst is 301 B/B). That is acceptable because an overrun
    runs; it is never refused.
  - Longest-first dispatch pays the prior on the largest file of each language: about 1.7 GB reserved against
    303.9 MiB observed. The cost is concurrency for one file, never a failure.
- **Re-derive the allocation between files.** Take the smaller of available memory plus the product's own observed
  residency (less base and margin) and half of that figure. Counting the product's own residency means its own growth
  never throttles it. `ObserveMachine` (`internal/provider/dependence/machine_linux.go:31-43`) already takes the smaller
  of `MemAvailable` and cgroup headroom.
- **`B_process` is `config.BaseFootprint`,** with its idle term read from the parent's own resident set at composition.

Two figures stay constants, because nothing can be derived from need for them. **Half of available** is a
coexistence share: need says what the product would use, never what the person's editor will want next. **The margin**
is the same kind of figure.

Steel-man of the alternative: Buck2's pressure-learned cap. It loses because it learns the limit by causing pressure,
because it needs a cgroup per action, and because PSI is Linux-only. Per-unit learning elsewhere comes from Kubernetes
VPA (decaying histogram, percentile) and SQL Server memory-grant feedback (percentile over persisted history, after its
oscillation problem). Bazel, Pants, PostgreSQL and Spark all use static figures (`01` §4, with URLs).

**Trade-off.** The ledger gains an adjust operation, the worker trims between files, one table is persisted, p99 is a
design constant, and about 1% of files overrun by design, each one disclosed.

**Measurement to confirm** (per repository class):
- Σ reserved against Σ observed.
- Peak tree residency against today's 256 MiB design at the same worker count.
- Overruns at most 2% after the first generation.
- Trimmed parse wall within noise of the untrimmed.
- The prior at or above the observed need on the largest file of every language.

If the prior falls below observed need on some class, the fallback is that the first file of a language runs alone.

## 2. Least wall time (`02`, `06`)

### (a) The parse wait, by file and line

**The instrument.** The worker's span wall runs from `pool.go:484` (`openWorkerSpan`, `ledger.Start` at `:573`) to the
reap at `:501`. Its processor time is the reaped child's rusage (`internal/process/runner.go:635-636`), not the
sampler's reading, which feeds only the hang detector (`pool.go:634`). The instrument therefore has no artifact: the
ratio is lifetime against work.

**The root cause.** `Store.ingestGroup` holds `groupMu` for the whole call (`open.go:706-710`), and `readOwn` holds it
too while a group is open (`open.go:829-834`), which during a cold build is always. Per file unit the lock is taken by:

- `UnitState`, `BeginProviderRun`, `BeginUnit`, the input flush and the manifest point lookup (`snapshots.go:67`);
- one or two alias reads per extracted candidate. The structural unit depends on its filesystem unit
  (`internal/index/plan/plan.go:626`), so every keyed candidate reads the store: `facts.go:702` →
  `internal/reconcile/resolver.go:69-90`;
- each flushed batch;
- `SealUnit`, with its eight closure queries;
- `CompleteProviderRun`.

Every call is a savepoint of one open group. With a 1 GiB writer cache and `synchronous=NORMAL` (ADR-0004), a unit
commits nothing and syncs nothing.

**Contributing causes.**
- **Drain and re-exec.** The stage can drain mid-provider: `leaveStage` runs when `IndexUnit` returns
  (`provider.go:244`), before flush, seal and run completion, so all in-flight units can be outside the stage at once.
  The pool then drains (`pool.go:411`) and the next unit re-executes a worker.
- **Worker start.** Each worker re-executes the whole binary (`cmd/codectx/main.go:22`) and verifies nine grammars.
- **The wire.** It makes one JSON message and four synchronous `io.Pipe` hand-offs per record.
- **The provider barrier.** `generation.go:771-778` holds the next provider until the previous one has sealed.

**Shares.** Parse plus start is at most 2% of the stage. The other 98% is unit work outside the exchange. How that 98%
splits between lock wait, SQL, re-exec and the barrier is unavailable from reading; the measurement below closes it.

**Recommendation.**
- Resolve a file's candidates in one batched read.
- Run the read-only steps on the reader pool against the last commit, adding one commit at each provider boundary.
- Hand writes to a writer goroutine that owns the connection and a statement cache for the whole group.
- Keep the stage open across the whole unit.

**Alternative.** Raise concurrency. It loses: WAL admits one writer, so more units add lock holders, not capacity.

**Measurement.** A mutex profile or a sub-span on `groupMu`; per-unit phase times; `WorkersStarted`; the runner queue
time. Pass: stage wall ≤ 2 × Σ worker CPU ÷ `ParserWorkers`, and `WorkersStarted ≤ MaxWorkers`.

### (b) One pass per file

Lower and analyse in `serve`, after extraction, over the same tree. Stream each function's facts as binary frames of ids
and byte ranges, then close the tree. Nothing outside the benchmark calls lowering today (`worker/lower.go:125,148`).

A separate pass would re-parse every file. In the benchmark's per-file rows, parse is 31% of per-file cost for Go and 68%
for JavaScript on the TypeScript checkout (`02` §b). Version independence is kept anyway, by two capability rows on one
unit.

**Within the pass, the lowering is the cost** (`06`). On kubernetes Go, 95% of the per-function time is lowering, about
48 µs per function. The cheap first step is to switch the Go lowering to field ids: the id path already exists
(`worker/lower.go:96-102`) and needs no new dependency. How much of the 48 µs the per-call C-string allocation explains
is unavailable until a CPU profile of the lowering walk is taken.

**Measurement.** One-pass against parse-then-reparse wall, per language and class. Wire bytes per edge, binary frames
against JSON. A lowering profile.

### (c) Scheduling

- Today's order is path order (`plan.go:1163`).
- Parse time ranks with source bytes at Spearman ρ 0.82–0.86 per language.
- A list-scheduling replay of the rows (`02` §c, not a run) gives these makespans:

  | workers | path order | largest-first by bytes |
  |---|---|---|
  | 16 | 1.136 s | 1.030 s (the lower bound) |
  | 64 | 0.456 s | 0.258 s |

**Recommendation.** Order by bytes, largest first, from one shared queue. Work stealing solves a contention that one
central queue does not have. The worker count is `min(CPUs, allocation ÷ observed need)`.

The gain is zero while (a) keeps the stage store-bound, so (a) comes first. Pass: makespan within 5% of
max(Σ cost ÷ workers, largest file).

### (d) Store write throughput

- Statements are prepared once per batch, which for a one-file unit means once per file (`units.go:993`).
- About a dozen secondary b-trees are maintained on every insert (`schema.sql:556-623`).
- Seal runs eight closure queries per unit.

**Recommendation.** A writer goroutine with a per-group statement cache and per-group seal validation.

**Alternative.** Build indexes at activation. It is rejected for the query-serving indexes: activation would scale with
the store, and the resolver reads `idx_alias_lookup` during the run. Deferring the query-only name indexes is left to the
measurement.

**Measurement.** Writer busy time ÷ stage wall. At 80% or more the writer saturates and index order is the next lever.

## 3. Least resident memory (`03`, `06`)

**Native peak bytes per source byte**, per file, over the public corpus matrix (`06`; the counted native peak of the
parse ÷ the file's source bytes, one row per language from the corpus where it is the majority):

| language (corpus) | p50 | p90 | p99 | max |
|---|---|---|---|---|
| C (llvm-project) | 9.59 | 21.94 | 31.60 | 260.09 |
| C++ (llvm-project) | 18.73 | 27.40 | 44.98 | 300.80 |
| Go (kubernetes) | 20.24 | 27.61 | 36.49 | 80.24 |
| Java (elasticsearch) | 16.85 | 21.48 | 26.86 | 49.30 |
| Python (home-assistant core) | 22.77 | 27.84 | 32.99 | 77.92 |
| Rust (rust-lang/rust) | 22.47 | 37.74 | 72.60 | 147.84 |
| TypeScript (vscode) | 22.40 | 28.17 | 38.14 | 54.81 |
| JavaScript (TypeScript checkout; compiler test data, 81.65% with errors) | 25.98 | 42.62 | 70.93 | 109.21 |

`03` reports the tree's own bytes (`tree_bytes`, a different instrument) at p50 26.0 / 15.8 / 22.9 for the TypeScript
checkout's TypeScript, kubernetes Go and home-assistant Python. Within one language the tree ratio runs from 0.015× (a file that is one long literal) to about 80×, so no per-grammar
constant is possible. The runtime's structure explains the spread (`subtree.h`): a heap node costs about 88 bytes, an
inline leaf costs its 8-byte slot, and external-scanner tokens and multi-line tokens are always on the heap. The
structure supplies no number. Scanner bytes are uncounted for Python, C++ and Rust.

**What the counter misses.**
- **A new high.** On a file that sets the worker's high, resident-set growth is about the counted peak plus the
  binding's input copies, up to 2× the source (`parser.go:272-278`).
- **Every other file.** The resident set does not move on about 95% of files, because glibc reuses freed memory. This is
  why per-file measurement needs the trim of §1.

**Recommendations.**
- **Return memory at the file boundary.** Close the tree, drop the arena's backing (removing the 1 MiB threshold, which
  fired for 3 of about 351,000 functions while the arena kept 0.4–1.25 MB), and call `malloc_trim(0)`. Then reset the
  peak.
- **Keep one C heap.** Pin the parse loop to one OS thread. On glibc, set `MALLOC_ARENA_MAX=1` in the worker's spawn
  environment.
- **Stop double-buffering in the parent.** Decode each frame straight into the builder, dropping the intermediate
  `extraction` copy (`pool.go:698-708`). Keep one file's facts per sink hand-off:
  - the evidence clip is file-wide (`facts.go:734-797`);
  - reference resolution needs every declaration first (`facts.go:667`);
  - retry-once would put duplicates into the sink.
- **Leave `GOGC` and `GOMEMLIMIT` unset,** for a different reason than before. Counting C allocation is possible (the
  binding routes it through Go, `allocator.go:22-36`), but in the worker the memory the limit would govern is small, and
  the coordinator's retained records are already bounded by the sink pool (`sink.go:50-70`). A limit would add only the
  thrashing risk the Go GC guide describes (https://go.dev/doc/gc-guide).

**Trade-offs.** The cost of the trim and of the re-faults is unmeasured, and the arena setting is glibc-only.

**Measurement.** Resident set after each file returns to the base. The trim costs less than the noise of a median file.
The parent's Go heap per file stays at or below the worker's tree peak.

## 4. Publication (`04`)

**Who publishes.** The structural provider (`treesitter`) publishes `control_depends_on`, `data_flows_to`, `reads` and
`writes` as per-file capabilities, with `static_analysis` precision on their evidence. The fixed `syntax` precision at
`treesitter/facts.go:712` becomes per family.

A failure of the pass degrades that file's four capabilities and never fails the unit. That matters because the
structural provider is Required, so a failed unit fails the generation (`generation.go:1257-1260`).

**Alternative: a native backend under the `dependence` id.** Its strength is that none of the nine consumers keyed on
that id would change. It loses on three counts:
- The phase-3 gate ("zero dependence-provider rows") and retirement condition 2 become unmeasurable.
- A provider has one invalidation scope, and `dependence` is package-scoped (`dependence/provider.go:214`).
- It re-parses every file.

**Coexistence with the oracle.** A per-language graduation set, one predicate in code shared by both producers, decides
who publishes. The engine importer drops the four families for graduated languages, and the set is deleted at phase 3.
The native shadow runs offline in the harness and is never attached to a generation.

**The differential harness.** Relocate `FactKey`, `KeySet`, `LoadKeySet`, `Diff`, `saveKeys` and `markDelta` (no `DedupeSink` exists in the
code).
- The comparison key normalises away the owner full name and the operator.
- Both endpoints become the existing cross-provider declaration key.
- Each line of the key file carries the digest and its pre-image, so every mismatch can be classified into a signed
  cause.
- `data_flows_to` causes: φ-depth, engine over-kill, substring over-connection, the depth-8 cutoff, call-site endpoints,
  the Go package initialiser, byte range only, and cross-file globals.
- Control-dependence causes: exit augmentation (29 of 228,765 kubernetes Go functions need it), try-block modelling, and
  operator conditions.

**Per-file reindexing.** The four families reuse structural file units on a fingerprint match. `calls` becomes a linking
unit keyed on per-file export and call-site table digests, so an edit to a function body does not trigger a relink. This
is a model addition, since unit inputs are files today (`delta/delta.go:56-72`).

**The program-dependence-graph surface.** No new tool: the tool set is fixed at 23 (`mcpserver/registry.go:94`).
- A `ProgramDependence()` relation set joins control and data dependence by entity pair.
- It is added to impact, which excludes both kinds today (`graph/cost.go:83-89`).
- Evidence ranges are paged per pair.

**Entities.** Parameters and locals become structural `variable` nodes carrying the declaration key (`emit.go:415-417`),
so both producers resolve to one identity. Anchor to every operand: the engine drops control dependences on operator
conditions.

**Measurement.** Time to the base generation and time to all capabilities fresh, per class. The signed per-cause
difference, with an unexplained residue within the band from two engine runs.

## 5. Corpora and goldens (`05`, `06`)

**Pins.** All seven clones are shallow at pins that match their HEADs, and the product reads one tree, never history.

| corpus | commit |
|---|---|
| kubernetes | `dfd7b93a` |
| home-assistant core | `5d010719` |
| elasticsearch | `3273b67c` |
| vscode | `529ee190` |
| rust-lang/rust | `5ceaf660` |
| llvm-project | `4257da8e` |
| TypeScript checkout | `cf8cf4f6` |

**The TypeScript checkout is not a TypeScript project at that pin.** It is the compiler's Go port with the TypeScript
test data: 12,508 of its 12,799 `.ts` files are test data. It instantiates configured Go, unconfigured TypeScript test
cases and generated JavaScript, and a TypeScript-majority production repository must come from elsewhere (vscode).
`internal/bench/corpora.json` should gain the seven entries as unmeasured, each naming its class.

**Oracle unit per language**, as a rule that applies to any repository of the class:

| language | unit |
|---|---|
| Go | the whole package (module for the engine) |
| C/C++ | a component directory holding its headers; header facts form a separate band |
| Java | the build module; control and data dependence are diffable, only `calls` is authored |
| Python | the package |
| JS/TS | the tsconfig project, never split |
| Rust | authored goldens only |

Choosing units from a class:
- A unit's edge count × the band must be at least one edge.
- The engine's priced peak must fit the allocation.
- Take the smallest eligible unit, one at least twice its size, the largest that fits, and one vendored and one
  generated unit.
- A unit the engine crashes on is excluded, never counted as agreement.

**Goldens.** Rust goldens are authored from the Rust Reference; `?` on both `Result` and `Option` is mandatory. Java
goldens are authored from JLS chapters 14 and 15. They use the existing golden format (`lower_fixture_test.go:15-31`).
Equality is a valid gate only when:
- each case cites its reference anchor;
- two people derive it independently;
- no case is derived from, or corrected against, a producer's output;
- every construct the lowering handles has a case.

**Gates by class.** A class is claimed only on at least two instances from different language families, and every
instance must pass, not the average. Two classes cannot yet be claimed:
- **Unconfigured, whole repository:** one instance, and it is private.
- **Typed, configured by a build step:** llvm-project alone.

## 6. What the benchmark task must add

Each is a column or row, measured per repository class:
- the worker's per-file need, base and anonymous resident set from `Done`;
- the trim's wall and yield;
- Go heap against C heap (`runtime/metrics` beside `VmHWM`);
- records and evidence per file;
- `groupMu` wait and per-unit phase times;
- `WorkersStarted`;
- writer busy fraction;
- a CPU profile of the lowering walk;
- scanner bytes (Python, C++, Rust);
- `sizeof(SubtreeHeapData)` asserted through cgo.

## Unavailable, with the reason

- **The split of the structural stage's 98% between lock, SQL, re-exec and barrier:** needs a mutex profile.
- **Worker start time and per-record wire cost:** nothing records them.
- **The trim's cost:** never measured.
- **Scanner allocations:** uncounted by design.
- **macOS and Windows release and available-memory calls:** named, not fetched.
- **Native cost for Python, Java, C/C++, Rust and TypeScript:** no lowering exists yet.
- **An unconfigured public whole-repository corpus:** none is pinned.
