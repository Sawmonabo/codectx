# 01 — How the exporter reproduces the production extraction path

All file:line references are to commit 16dfef6. The harness sources are under `source/`, at their repository-relative paths.

## What it runs
`tbexport <repo> <out>` (`source/cmd/tbexport/main.go`) does not call the providers itself. It runs the real `codectx index` command in-process, through `cli.NewRoot` and `cli.Execute`, with the arguments `index --repo <repo>`. Planning, admission, the provider registry, the batch sinks and the store are therefore exactly what a user's run gets:
- per-unit build: `internal/index/generation.go:1256` (`Store.BeginUnit`) and `internal/index/generation.go:1260` (`provider.RunUnit` over `provider.StoreUnit`);
- providers grouped one at a time: `internal/index/generation.go:774` and `:800`.

The tree-sitter parser runs in worker subprocesses started by re-executing the current binary with a hidden subcommand (`internal/app/compose.go:894`). The exporter dispatches that subcommand to `worker.Main` exactly as `cmd/codectx/main.go:22` does, so the parse is the real one.

The configuration is the real one: `config.Load` merges the built-in defaults, the user file and the project file. The only change is that `XDG_CACHE_HOME` points the default data directory at a directory inside `<out>`. The operator's own cache is never touched.

## Where the documents are captured
Documents are captured inside the store's own write path, after the store accepted them. The capture is a nil-guarded observer, `sqlite.TBObserve` (`source/internal/storage/sqlite/tbcapture.go`), with four call sites added to `units.go` (`source/internal/storage/sqlite/units.go.diff`):
- `BeginUnit` (`internal/storage/sqlite/units.go:425`): records the unit's real `UnitBuild` and its streamed inputs.
- `PutKeyedNodes` (`:633`): records the node facts the unit put.
- `PutSearchUnits` (`:882`): records each document with `token_count` set to the value the store computes. That value comes from the store's one tokenizer pass, `countTokens` (`internal/storage/sqlite/query.go:909`, called at `units.go:907`); the store ignores the provider's own informational count. The same pass also yields the distinct terms of each document, which feed the per-term document frequency in `terms.tsv`.
- `SealUnit` (`:1384`): on success, the unit's spilled documents are appended to `docs.jsonl` and its record to `prereq/units.jsonl`.

Only the `filesystem` and `treesitter` providers are exported, as the brief requires. Every other provider's documents are counted and printed as "not exported".

## Order
Documents are grouped by unit and written in seal order, under one lock. The coordinator builds one provider's units at a time (`generation.go:800`), so every filesystem unit precedes every treesitter unit. A filesystem unit always precedes the treesitter unit that depends on it.

## The tokenizer behind terms.tsv
`terms.tsv` uses the store's tokenizer: unicode61 over the five `search_fts` columns (`internal/storage/sqlite/schema.sql:388`). This is the same tokenizer `Store.Tokenize` (`internal/storage/sqlite/query.go:873`) declares. The terms are read from the instance vocabulary that `countTokens` already produces, so there is no second tokenization pass.

## Side files the index arm needs (rows the store forces; see 03)
- `prereq/source.json` holds the repository id and root, the ref, the snapshot header, every file version and every blob record. It is read back from the real run's store through `Store.Snapshot`, `SnapshotFiles` and `Blob`, for the active generation's snapshot. The exporter checks that this is the snapshot the exported units were built over; the real run's base generation may have been superseded by a later publication of deferred work over the same snapshot.
- `prereq/units.jsonl` holds, per unit: the build, the inputs, the referenced node facts, and the document count.
