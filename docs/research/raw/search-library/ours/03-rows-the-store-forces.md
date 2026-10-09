# 03 — Rows the store forces before a search document can be written

All file:line references are to commit 16dfef6. Feeding only search documents into an empty store is refused at several points. Each row below is the minimum the store demands. All of them are copied from the real run; none is fabricated, except in the delta (see 05).

| Row | Why it is required | What the index arm writes |
|---|---|---|
| repository | the generation references it (`internal/storage/sqlite/source.go:19`, `EnsureRepository`) | the real repository id and root |
| blobs | `snapshot_files.content_hash REFERENCES blobs(hash)` (`internal/storage/sqlite/schema.sql:59`) | every blob record the real snapshot names (`source.go:38`, `PutBlob`) |
| snapshot + snapshot_files | `BeginUnit` streams the inputs and checks each one against the generation's snapshot (`internal/storage/sqlite/units.go:509`) | the real snapshot header and every file version (`source.go:115`, `PutSnapshot`) |
| generation, provider run | `BeginUnit` requires a staging generation and a running run of the same provider (`units.go:425`) | `BeginGeneration` with the real ref and analysis config hash; one `BeginProviderRun` per unit |
| unit + unit_inputs | `search_units` has `FOREIGN KEY(unit_id, file_id) REFERENCES unit_inputs` (`schema.sql:373`); the input hash and the unit identity are recomputed (`units.go:564`) | the real `UnitBuild` and its inputs |
| node_facts, node_ids | `search_units` has `FOREIGN KEY(unit_id, node_id) REFERENCES node_facts` (`schema.sql:372`), and `nodeRef` refuses an unregistered node (`units.go:1089`) | only the node facts the unit's documents reference; see below |
| evidence (one per node) | the seal check "node facts without evidence" (`units.go:1359`) | the first evidence row of each referenced node fact |

## Node facts
The node facts are the real ones. The store re-derives each node id from repository, kind and canonical key (`units.go:648`), so a fabricated key would be refused. Each fact keeps its first evidence row only. The evidence's `OriginRunID` is pointed at the new run, because `checkEvidence` refuses evidence naming another run (`units.go:591`). The evidence `UnitID` stays as it is, because the unit identity is unchanged.

Dropping the documents' node id was rejected as a shortcut. It would skip the `nodeRef` lookup, flattering the write, and it would break the node deduplication the query path performs (see 06).

## What is not written
The real run's relations, aliases and unreferenced nodes, and every other provider's units (manifest, dependence) are not written. So the store's one writer is less contended than in a real index. Writing them would be work the other arm does not do either.

The forced rows are timed apart from search work: `seed` covers open, repository, blobs, snapshot and generation; `begin` covers run and BeginUnit; `nodes` covers the node facts.
