# 02 — Least wall time end to end

Date 2026-09-26. Read at the branch head; no product, benchmark or test was run for this note. Benchmark evidence is
the finished row file of one public corpus, the TypeScript compiler checkout at
`cf8cf4f6c17ada5e920949ad984791ceb9ce06df` (the only corpus the benchmark summary lists as finished), summarised as
quantiles and ratios over its `origin = tree` rows. Every figure from it is evidence for a design, never a constant: the
design derives each quantity from the repository and machine in front of it.

## (a) The observed parse wait: about 6.2 s of worker wall against about 0.13 s of worker CPU

### The observation, and the run behind it

The figure comes from the run ledger's `stage finished` lines (`internal/ledger`), printed by the product while the
`internal/e2e` package's tests ran during a capped test pass on 2026-09-25, 23:33–23:38 local time, over the tree of
commit `b7b8815` (`go test -count=1 ./internal/e2e` under `ulimit -v 6000000`, `GOMEMLIMIT=3GiB`, `GOMAXPROCS=4`). Two index runs in
that log carry `structural_parse` worker spans; each has four worker spans and one stage span. The lines are
reproduced here with the run id cut to 12 hex digits, the timestamp dropped, and nothing else changed:

```
run=335e980fb643 stage=structural_parse seq=20 items_in=1 items_out=12006 outcome=ok wall_ms=7210 cpu_user_ms=88 cpu_sys_ms=15 peak_rss_bytes=40677376 read_bytes=322741 write_bytes=1853709
run=335e980fb643 stage=structural_parse seq=22 items_in=1 items_out=6005 outcome=ok wall_ms=7209 cpu_user_ms=132 cpu_sys_ms=12 peak_rss_bytes=43089920 read_bytes=388746 write_bytes=1102512
run=335e980fb643 stage=structural_parse seq=21 items_in=1 items_out=6005 outcome=ok wall_ms=7210 cpu_user_ms=81 cpu_sys_ms=18 peak_rss_bytes=40738816 read_bytes=262712 write_bytes=1003729
run=335e980fb643 stage=structural_parse seq=19 items_in=1 items_out=12005 outcome=ok wall_ms=7211 cpu_user_ms=97 cpu_sys_ms=13 peak_rss_bytes=40943616 read_bytes=334779 write_bytes=1890037
run=335e980fb643 stage=structural_parse seq=18 items_in=4 items_out=36021 outcome=ok wall_ms=7212 cpu_unattributed=overlapped
run=d0e733064b1b stage=structural_parse seq=21 items_in=1 items_out=12006 outcome=ok wall_ms=6174 cpu_user_ms=92 cpu_sys_ms=28 peak_rss_bytes=40783872 read_bytes=322746 write_bytes=1853709
run=d0e733064b1b stage=structural_parse seq=19 items_in=1 items_out=12005 outcome=ok wall_ms=6176 cpu_user_ms=116 cpu_sys_ms=10 peak_rss_bytes=40542208 read_bytes=334779 write_bytes=1890037
run=d0e733064b1b stage=structural_parse seq=20 items_in=1 items_out=6005 outcome=ok wall_ms=6175 cpu_user_ms=142 cpu_sys_ms=20 peak_rss_bytes=43409408 read_bytes=388746 write_bytes=1102512
run=d0e733064b1b stage=structural_parse seq=22 items_in=1 items_out=6005 outcome=ok wall_ms=6174 cpu_user_ms=106 cpu_sys_ms=12 peak_rss_bytes=41021440 read_bytes=262712 write_bytes=1003729
run=d0e733064b1b stage=structural_parse seq=18 items_in=4 items_out=36021 outcome=ok wall_ms=6177 cpu_unattributed=overlapped
```

"6.2 s against 0.13 s" is the second run, `d0e733064b1b`: the four worker walls are 6,174–6,176 ms, and user plus
system CPU is 120, 126, 162 and 118 ms, a mean of 131.5 ms. The first run gives 7,209–7,211 ms against 103–144 ms. In
both runs every worker's wall equals the stage's wall to within 3 ms, which is what the instrument reading below
predicts: a worker span is the stage's lifetime, not a parse. The log itself was not kept; the lines above are the
whole of the evidence.

### What the instrument measures

- **Wall.** A worker's span is opened in `pool.start` before the runner is called
  (`internal/provider/treesitter/pool.go:484` → `openWorkerSpan` `:561-575`, `ledger.Start` at `:573`) and ended in the
  run goroutine after the runner has reaped the process (`pool.go:501`). The ledger's wall is
  `end.Sub(s.started)` (`internal/ledger/span.go:278`). So a worker's wall is its **process lifetime**: exec, hello,
  every parse it served, the idle time between them, and the drain that ends it.
- **CPU.** `measured(w.result)` (`internal/provider/treesitter/spans.go:37`) copies `CPUUserMillis` /
  `CPUSysMillis`, which the runner reads from the reaped child's rusage (`cmd.ProcessState.UserTime()` /
  `SystemTime()`, `internal/process/runner.go:635-636`). It is **not** the 250 ms sampler: the sampler's clock-tick sum
  (`internal/process/treesample_linux.go:80`) feeds only `CPUProgress`, i.e. the hang detector
  (`pool.go:634`). No instrument artifact on CPU: 0.13 s is the worker's real processor time, start-up included.
- **Runner queue inside the span.** The span also contains the parser runner's own admission wait
  (`runner.go:425-470`, called from `Run` before `run` starts its clock at `runner.go:501`). The parser runner's budget is
  `max(childMemory, 256 MiB)` (`internal/app/compose.go:780`) and its concurrency `ParserWorkers`
  (`compose.go:779`), the same allocation the pool's ledger reservation (`pool.go:289`) was already granted against, so
  this wait is expected to be zero. It is an artifact only if the two ever disagree; the measurement below separates it.
- **How long a worker lives.** `Provider.IndexUnit` brackets the whole unit with `enterStage`/`leaveStage`
  (`internal/provider/treesitter/provider.go:243-244`); the last caller to leave drains every idle worker
  (`provider.go:132-139` → `pool.drain` `pool.go:411`). A worker is held by a caller only for the exchange
  (`pool.go:757-808`) and released right after (`provider.go:357-358`). With `ParserWorkers = CPUs`
  (`internal/config/machine.go:400`) and `BuildWorkers = CPUs` (`machine.go:90-95`, used at
  `internal/index/generation.go:893`), every worker started in the stage stays idle-but-alive until the stage ends.

**Verdict on the reading this note was asked to test: confirmed, with one refinement.** The 6.2 s / 0.13 s ratio is by
construction — a worker span is the structural stage's wall, not a parse wait — so the ratio itself is not a defect.
What it exposes is the stage wall: the stage lasts as long as the **per-unit work around the parse**, and that work is
serialized. The refinement: the serialization is on **one mutex and one connection, not on the disk.** Under the
ingestion group a unit costs about zero commits and zero fsyncs (below), so "round trips through a single writer" is
right and "fsync per unit" is not.

### The root cause: every per-unit store call, reads included, takes `Store.groupMu`

`ingestGroup` holds `s.groupMu` for the whole of `fn` (`internal/storage/sqlite/open.go:706-710`), and `readOwn` holds
the same lock for the length of its `fn` whenever a group is open (`open.go:829-834`) — which, during a cold build, is
always. Per structural-parse file unit the calls that take it, in order:

| step | call | lock | cite |
|---|---|---|---|
| reuse check | `Store.UnitState` | `readOwn` | `generation.go:962`, `sqlite/reconcile.go:20-39` |
| run row | `BeginProviderRun` | `ingest` | `generation.go:1079`, `sqlite/units.go:83-104` |
| unit row + dependency check | `BeginUnit` | `ingest` | `generation.go:1122`, `units.go:425-500` |
| input rows | `streamInputs` flush (1 per `BatchRecords`=1000 inputs, so 1) | `ingest` | `units.go:509-560`, `internal/config/config.go:618` |
| manifest row of the file | `provider.lookup` → `SnapshotFile` | `readOwn` | `provider.go:298-315`, `internal/snapshot/view.go:59-83`, `sqlite/snapshots.go:57-67` |
| **identity of every declaration, import and reference candidate** | `Resolver.Resolve` → `LookupAliases`, up to two per candidate (strong key, native key) | `readOwn` | `treesitter/facts.go:702`, `internal/reconcile/resolver.go:69-90`, `sqlite/reconcile.go:71-91` |
| facts | `PutNodes`, `PutRelations`, `PutAliases`, `PutSearchUnits` — one `ingest` per flushed batch kind | `ingest` | `internal/provider/sink.go:694-733`, `units.go:673/782/840/915` |
| seal | `SealUnit`: evidence clip, eight closure queries, lexical fold, membership insert | `ingest` | `units.go:1362-1440` |
| run close | `CompleteProviderRun` | `ingest` | `generation.go:1130`, `units.go:113-134` |

The resolver row is the one that scales with the file: a structural unit carries `DependsOn = [its filesystem unit]`
(`internal/index/plan/plan.go:626`), so `len(r.deps) > 0` and every keyed candidate issues a store read under the
writer's lock. The parse is one exchange; the rest of the unit is roughly 10 lock acquisitions plus one or two per
extracted record, all queued on one `sync.Mutex` with every other unit's writes. That lock, `open.go:710` (and
`open.go:830` for the reads), is the root cause of the stage wall; the worker span is its symptom.

**Commits and fsyncs per unit.** Every call above runs inside a `SAVEPOINT` of the open group (`open.go:720-746`); the
group commits only when forced, when an exclusive `Store.write` is waiting, or when the writer's page cache spilled
(`open.go:752-759`). The cache is 1 GiB by default (`open.go:645`), no exclusive writer runs in a one-shot index (the
watch heartbeat is started only by the watch path, `internal/index/coordinator.go:538`, `:601-620`), and the writer runs
`synchronous = NORMAL` (ADR-0004 decision 1), under which "the checkpoint is the only operation to issue an I/O barrier
or sync operation" (https://www.sqlite.org/wal.html). So a file unit costs **0 commits and 0 fsyncs** in the common
case; each group commit costs one commit plus one `wal_checkpoint(PASSIVE)` (`open.go:777`) with its two syncs (WAL
before, database after, per the same page).

**Is the manifest lookup indexed?** Yes: `EachFile` with a `Paths` selection is one point lookup per path
(`snapshot/view.go:70-83`, `SnapshotFile` keyed by `NewFileID(repo, path)` at `:74`) against
`PRIMARY KEY(snapshot_id, file_id)` of a `WITHOUT ROWID` table (`sqlite/schema.sql:53-66`) — not a manifest scan. Its
cost is that it runs on the writer's transaction under `groupMu` while the group is open (`snapshots.go:67` →
`open.go:830`).

### Contributing causes

1. **The stage can reach zero mid-provider (drain and re-exec).** `leaveStage` runs when `IndexUnit` returns
   (`provider.go:244`), before the unit's `Flush`, `Seal` and `CompleteProviderRun` (`internal/provider/unit.go:67-86`,
   `generation.go:1130`), and a newly submitted unit (`generation.go:899-921`) does `Spec`, `UnitState`,
   `BeginProviderRun`, `reconcile.New`, `BeginUnit` (`generation.go:962-1122`) before it reaches `enterStage`. When
   every in-flight unit is in those pre/post phases at once — the likelier the more they queue on `groupMu` — the count
   is zero, the pool is drained synchronously in the leaving unit's goroutine (`pool.go:411-428`, each stop waits for the
   reap, `pool.go:670-681`), and the next unit re-executes a worker. `stopIdle` (`pool.go:331-341`) also retires an idle
   worker when the ledger needs room. Share: needs measurement — `Stats.WorkersStarted` against `MaxWorkers` for the run
   (`pool.go:823`); any excess is re-exec cost.
2. **Worker start.** The worker is the whole product binary re-executed (`cmd/codectx/main.go:22`), so every package
   `init` runs (for example `internal/storage/sqlite/engine.go:35`, `internal/storage/pacedvfs/paced.go:109`), then
   `verify` over all nine grammars (`worker/worker.go:117-137`), the hello with the fingerprint (`worker.go:61`,
   checked at `pool.go:530`), and a lazy per-language query compile on first use (`worker.go:171-189`). Cost:
   unavailable — nothing records time from `Run` to the hello frame. It is inside the 0.13 s CPU and bounded above by it
   for the fixture.
3. **The wire.** `io.Pipe` has "no internal buffering"; "the data is copied directly from the Write to the corresponding
   Read" (https://pkg.go.dev/io#Pipe). The parent writes a request and the source as separate frames, each a header
   write and a payload write (`internal/provider/treesitter/wire/wire.go:191-222`) through the runner's stdin pump
   (`runner.go:957-981`); the worker answers one JSON message per record (`worker.go:236-238`), the runner's drain copies
   it (`internal/process/stream.go:80-90`) into a second `io.Pipe`, and the parent does two `ReadFull`s and one
   `json.Unmarshal` per record (`wire.go:252-272`, `pool.go:765-799`). Per record that is one marshal, one unmarshal and
   four synchronous hand-offs. Cost per record: unavailable (no benchmark row times the exchange); it is CPU on both
   sides, so it is already inside the 0.13 s for the worker's half.
4. **Provider barrier.** A provider's group is drained before the next provider's first unit is admitted
   (`generation.go:771-778`), so the structural stage starts only after every filesystem unit has sealed, and its tail
   is idle cores.

### Projected shares of the fixture's wall

Parse and worker start: at most 0.13 s per worker, **about 2% of 6.2 s** (the run `d0e733064b1b` above). The rest,
**about 98%, is unit work outside the exchange**; the split between waiting on `groupMu`, SQL execution inside it,
re-exec after a drain and the provider barrier is **unavailable from reading** and needs the measurement below. No
share is assigned to fsync, because the reading above shows none on the unit path.

**Recommendation.** Take the store off the per-record path and the lock off the per-unit path: resolve a file's
candidates in one batched read per file (all keys of the file in one `IN` probe against the dependency units), let
units run their read-only steps (`UnitState`, `SnapshotFile`, alias lookup) on the reader pool against the last
commit rather than through `readOwn`, and hand a unit's writes to the single writer as one queued batch that the writer
applies without the producer waiting. Keep the stage open across the whole unit, not just `IndexUnit`, so a drain
happens only when the provider's work is done.

**Strongest alternative, steel-manned.** Keep the design and raise concurrency: the lock is cheap when the store is
small, the savepoint-per-call shape is what makes a refused batch roll back alone, `readOwn` is what lets a unit see
the dependency facts this run has not committed yet, and a batch-resolve API is a second resolver path to keep
correct.

**Why the recommendation wins.** More units cannot help: every added unit adds lock holders, not lock capacity — one
WAL admits one writer (https://www.sqlite.org/wal.html). The dependency a structural unit reads is its own file's
filesystem unit, sealed before the structural stage starts (`generation.go:771-778`), so it is visible to a reader
once the group commits at the provider boundary — a commit the recommendation adds; today the group stays open
across providers, which is the only reason `readOwn` is needed there. Savepoint
atomicity survives unchanged inside a writer-owned batch.

**Trade-off accepted.** A commit at the provider boundary (one fsync pair per provider per run), and a resolver whose
batch API must return exactly what per-candidate `Resolve` does — the existing resolver tests become its oracle.

**The measurement the benchmark task must take to confirm it.** On the nine-language fixture and on each public corpus
class (a Go-majority monorepo, the TypeScript compiler, a Java-majority repository, a mixed monorepo), one cold index
with: a mutex profile of the parent (`runtime.SetMutexProfileFraction`) or a sub-span around `groupMu` acquisition in
`ingestGroup`/`readOwn`; per unit, time from `submit` to `enterStage`, inside `IndexUnit`, and from `leaveStage` to
seal; `WorkersStarted`; and the runner's queue time (span start to `runner.go:501`). Pass: after the change, the
structural stage's wall is within 2× of `Σ worker CPU / ParserWorkers`, and `WorkersStarted ≤ MaxWorkers`.

## (b) One pass per file for structural and dependence facts

Today the worker parses, runs the structural query and closes the tree (`worker.go:151-167`, `defer tree.Close()` at
`:156`). Lowering exists (`worker/lower.go:125` `Functions`, `:148` `Lower`; languages `go` and `javascript`,
`lower.go:49`) but nothing outside the benchmark calls it. The benchmark's per-file rows run parse + lower + the
three passes on one tree (`internal/bench/flowbench_test.go:556-590`). Over the corpus: Go parse 2.25 s of a 7.16 s
file total (31%), JavaScript 11.77 s of 17.30 s (68%); Spearman ρ between file wall and bytes 0.82 (Go), 0.90
(JavaScript). Dependence edges produced: 19.5 per KiB of Go source, 11.0 per KiB of JavaScript (control + def-use,
function rows).

**Recommendation.** In `serve`, after `ex.run`, walk `Functions` over the same root, lower and analyse each function
into the worker's one arena, stream that function's facts as frames before the next, then close the tree — one parse,
two cursor walks, tree freed at the end of the file. Cross the wire with ids and byte ranges only (function span, node
spans, edge pairs), never text, in a binary frame per function rather than a JSON message per edge.

**Strongest alternative, steel-manned.** A separate dependence pass (its own worker request, or its own provider) keeps
the structural provider's version and its units independent of the dependence algorithms, so a lowering change does not
invalidate every structural unit, and a crash in lowering cannot lose structural facts.

**Why the recommendation wins.** The separate pass re-parses: by the rows above, +31% (Go) to +68% (JavaScript) of the
per-file cost, on every file, forever. Version independence is kept by emitting the two fact families under two
capability rows of one unit, and crash isolation is already per file.

**Projected share.** The re-parse a separate pass would add: +31% (Go) to +68% (JavaScript) of per-file cost on this
corpus.

**Trade-off accepted.** A lowering change re-runs the parse of every file it touches; the worker holds one tree plus one
function's arena at a time instead of one tree.

**The measurement.** Per corpus class, per language: file wall of one-pass versus parse-then-reparse, and wire bytes
per source byte for the binary frame versus JSON. Pass: one-pass wall ≤ parse-then-reparse wall minus 0.9 × parse
wall; wire bytes per edge below the JSON frame's.

## (c) Scheduling: order, stealing, worker count

The build order is `(provider position, arrival)` (`plan.go:1163`), i.e. path order; the only queue is the group's
semaphore of `BuildWorkers` slots (`generation.go:893`, `:899-921`). Over the corpus, parse time against source bytes:
Spearman ρ = 0.836 (Go, n = 5,115), 0.824 (JavaScript, 13,131), 0.864 (TypeScript, 12,804), 0.862 (TSX, 350); ns per
byte p50/p99 = 83/246, 245/1,691, 109/724, 96/501. List-scheduling simulation over the same parse times (a replay of the
rows, not a run): at 16 workers path order 1.136 s, random 1.140 s, longest-first by **bytes** 1.030 s = the lower
bound; at 64 workers 0.456 s path order against 0.258 s by bytes (lower bound 0.257 s). For whole-file cost at 16
workers the largest single file (1.594 s) is the bound and longest-first by bytes meets it.

**Recommendation.** Order a provider's file units by source bytes descending, per language (the bytes are on the
manifest row the plan already reads), using the online ns-per-byte of that language in this run to interleave
languages; one shared queue, no stealing. Worker count stays `min(CPUs, allocation ÷ observed per-worker need)` with the
need learned per file (question 1), never a fixed figure.

**Strongest alternative, steel-manned.** Work stealing (per-worker deques) is the textbook answer to unknown cost and
needs no size proxy.

**Why the recommendation wins.** There is one central queue already, so stealing solves a contention that does not
exist; what the tail needs is the big files first, and bytes rank them with ρ 0.82-0.86 at no cost. The caveat that
decides the order of work: while (a) holds, a unit's wall is store-bound, so ordering by bytes orders the wrong
queue — (a) comes first.

**Projected share.** On this corpus's replay, ordering by bytes cuts the parse makespan about 10% at 16 workers and
about 44% at 64; zero while (a) keeps the stage store-bound.

**Trade-off accepted.** The plan's external sort gains a size key, and determinism of emission order must not depend on
build order (it does not: seal is per unit).

**The measurement.** Per corpus class: makespan of the structural stage under path order and bytes-descending order at
the machine's worker count. Pass: bytes-descending within 5% of `max(Σ cost ÷ workers, largest file)`.

## (d) Store write throughput

One writer connection, one WAL writer at a time (https://www.sqlite.org/wal.html); the product's group turns thousands
of statements into one transaction, which is what the engine's own guidance asks — "SQLite will easily do 50,000 or
more INSERT statements per second … But it will only do a few dozen transactions per second", and grouping amortises
the commit (https://www.sqlite.org/faq.html#q19). Commit count is already minimal (above). What remains per row:
statements are prepared once per **batch**, not per group (`sqlite/stmtcache.go` header; `units.go:993-1004`), and with
one-file units a batch is a file, so a dozen prepares per unit per fact kind; each fact row also maintains every
secondary index declared before the load — four on `node_facts`, one on `relation_facts` and one on `relation_ids`, four on `evidence`, three on
`native_aliases` (`sqlite/schema.sql:556-623`) — keyed by content-hash identities, so each insert touches random pages
in about a dozen b-trees; and `SealUnit` runs eight closure queries per unit (`units.go:1374-1430`).

**Recommendation.** Keep the group; move the writer to a dedicated goroutine that owns the connection and a statement
cache for the whole group, fed batches from a bounded channel, so producers never hold the lock and prepares are once
per group. Validate seal closure per group, not per unit, where the checks are unit-scoped joins (they are:
`?1 = unit_id`), in one statement over the group's units.

**Strongest alternative, steel-manned.** Drop secondary indexes for the build and create them at activation, which is
the classic bulk-load order and turns random index inserts into sorted builds.

**Why the recommendation wins.** Activation already has to be atomic and short; a whole-index rebuild there scales with
the store, not the change set, and an incremental refresh would pay it for one file. The indexes also serve this run's
own reads (`idx_alias_lookup`, `schema.sql:597`, is what the resolver probes). Deferring only the query-only indexes
(`idx_nodes_name`, `idx_nodes_qname`) is a candidate the measurement can decide.

**Projected share.** The writer's share is the unmeasured part of (a)'s 98%; writer-busy ÷ stage wall is the figure.

**Trade-off accepted.** A writer goroutine is one more owner of state; a seal failure is found at group end and must
fail exactly its unit, which the savepoint per unit preserves.

**The measurement.** Per corpus class, cold index: rows per second into `node_facts`/`evidence`, prepares per run
(`sqlite3_prepare` count via the statement cache), and writer utilisation (time the writer goroutine is busy ÷ stage
wall). Pass: writer busy ≥ 80% of the stage wall means the writer saturates and the index-order change is next; below
that, the producers are the limit.

## Unavailable, with the reason

- The split of the fixture's 98% between lock wait, SQL time, re-exec and barrier: needs the mutex profile of (a).
- Worker start time and per-record wire cost: no row records either.
- Rank correlation for the other corpora: their rows were not finished when this note was written.
