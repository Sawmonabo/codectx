# 02 — How the index arm reproduces the production write path

All file:line references are to commit 16dfef6. The harness is `source/cmd/tbindex/main.go` and `source/internal/tbench/build.go`.

## Store
The store is opened with exactly the options the indexing composition passes (`internal/app/compose.go:669`), derived from the same `config.Load` result. The cursor key is minted beside the database (`pagination.OpenSigner`), as every writing composition does.

## Sink limits, pool and concurrency
- Limits: `provider.Limits{BatchRecords, BatchBytes, MaxProviderRecordBytes}` from the configuration, as `internal/index/coordinator.go:284` builds them. The defaults are 1000 records and 4 MiB (`internal/config/config.go:621`).
- One shared `provider.NewPool(cfg.Index.QueueBytes)`, as `internal/app/compose.go:939` builds it.
- Concurrency: `min(config.BuildWorkers(cfg), provider.MaxLiveSinks)`, which is `workerCount` (`internal/index/coordinator.go:385`). It can be overridden with `-workers N`.
- Units are fed in seal order. One provider's units run at a time, and the store is flushed (committed) at every provider boundary and before the first unit, as `generation.build` does (`internal/index/generation.go:774`, `:800`).

## Per unit (`Builder.Build`)
1. `BeginProviderRun`.
2. `BeginUnit` with the real `UnitBuild` and its inputs (`internal/storage/sqlite/units.go:425`). The store re-hashes the streamed inputs against `Spec.InputHash` (`:564`) and re-derives the unit identity, so the identity is the real one. The run id is the new run's.
3. The referenced node facts go through their own `provider.BatchSink` and are flushed. This is a required row, not search work (see 03), and it is timed as its own phase, `nodes`.
4. The search documents go through `provider.NewBatchSink` over `provider.StoreUnit(store, w)` (`internal/provider/sink.go:349`, `internal/provider/unit.go:34`). `PutSearchUnits` (`sink.go:558`) flushes on the same record and byte bounds (`sink.go:617`) and sorts each batch by search key (`sink.go:745`) before `UnitWriter.PutSearchUnits` (`units.go:882`). That call is the one tokenizer pass, the FTS insert, the `search_units` row and the lexical staging. Phase: `put`.
5. `SealUnit` (`units.go:1384`): the seal checks, then the unit's lexical segment is folded (`:1429`). Phase: `seal`.
6. `CompleteProviderRun` with a succeeded result.
7. After every unit: `Activate(gen, 0, fresh, nil, NormalizationVersion)` (`units.go:1525`; the constant is at `internal/index/coordinator.go:65`). The lexical compaction runs inside it. Then `Store.Flush`. Phase: `activation`.

Every exported unit is built, including one that published no documents. Treesitter units declare their filesystem unit as a dependency, and `BeginUnit` refuses an unsealed dependency (`units.go:484`).

## What is measured
- Per phase: wall time, process CPU (getrusage), peak RSS (maxrss).
- When units run concurrently, begin, nodes, put and seal are sums over units. `build` is the wall time of all of them; for true per-phase walls, run with `-workers 1`.
- Disk: the apparent size of the store directory.
- Search bytes per table: SQLite `dbstat`, which is available in the embedded engine.

## Fidelity on the smoke (see 04)
On the smoke repository, the resulting store is identical to the real run's store in document count, token_count sum, distinct search keys, and summed name+path length.
