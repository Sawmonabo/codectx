# ADR-0009: The dependence import stages its export the way a bulk load writes

## Status

Accepted.

## Context

The dependence provider imports one unit's analysis export -- a directory of
node and edge CSV files -- by staging it in a private SQLite database and
deriving the unit's facts by ordered query ([providers-dependence](../providers-dependence.md#the-staging-database)).
Measured with the phase-by-phase harness in `internal/provider/dependence/neo4jcsv/scale_test.go`,
the staging wrote 34.5 times the export's bytes for a 9 MB synthetic export
and 42 times for a 47 MB one, and an uncapped index of the reference
repository left a 12.7 GB staging file behind a 1 GB store, writing at the
disk's full rate for the length of every import. The store's ingestion
group ([ADR-0008](ADR-0008-ingestion-group.md)) had already been made
write-proportional; the import was the remaining writer that saturated the
host.

Three mechanisms, each measured with the harness and the engine's `dbstat`
table:

1. **Overflow pages.** The occurrence and identity tables were `WITHOUT
   ROWID` tables carrying a JSON evidence or fact payload of one to two
   kilobytes. An index b-tree page holds at most about a kilobyte of a row
   locally ([SQLite file format, "B-tree Pages"](https://sqlite.org/fileformat2.html#b_tree_pages)),
   so every row bought a 4 KiB overflow page: 36 thousand occurrences grew
   the file by 171 MiB.
2. **Random-keyed inserts past the cache.** Rows were inserted into
   hash-keyed b-trees (relation identity, evidence identity, fact key) in
   the order they were derived, each a random leaf. Once a b-tree outgrows
   the page cache, each insert dirties a page the cache must spill before
   the next one, and each spill is a page write for one row: the
   repository-scale regime, at a page per row per index.
3. **Whole-table updates.** Node attribution ran five `UPDATE nodes` passes
   over the widest table; each rewrote every row's page.

A fourth defect found by the same harness: the alias emission sorted the
whole identity table once per 512-row page, quadratic in the unit's
entities (11 s at 36 thousand, hours at repository scale).

## Decision

The staging is written the way a bulk load writes, and nothing else:

1. **Append, then order once.** Every table is appended in the order its rows
   arrive, with no secondary index during the load. Every structure a later
   phase reads in another order -- the node table keyed by id, the edge
   tables in (type, source, target) and (type, target, source) order, the
   per-node attributes, the location and occurrence tables -- is built
   afterwards in one `INSERT ... SELECT ... ORDER BY` or `CREATE INDEX`,
   which the engine executes through its external merge sort
   ([vdbesort.c](https://sqlite.org/src/file/src/vdbesort.c)) and writes
   front to back. No row is updated after it is written; no row is inserted
   into the middle of a b-tree larger than the cache. Node attribution is a
   narrow attribute table derived from the node table, not columns updated
   on it.
2. **Integer node ids, rowid tables.** The export's node ids are the
   engine's integers and are staged as such; every derived table is keyed by
   them, so a lookup is an integer seek and a sorted copy is an integer
   sort. Payload-carrying tables are rowid tables, whose leaves hold about
   four kilobytes of a row locally, so no staged row overflows.
3. **Occurrences in projection order.** The projection is de-duplicated
   and sorted once by (kind, from entity, to entity, site), so the
   occurrences of one canonical edge arrive together. The relation staging
   appends them in that order and the emission reads them front to back,
   grouping where the entity pair changes, with no sort of the occurrences
   at all. The two cases that cannot be grouped that way -- an edge whose
   endpoint is an identity that several entities resolved to, and
   `may_refer_to`, whose target is named directly -- go to a second stream
   that is sorted once by relation identity; on a real export it is a
   small fraction of the occurrences.
4. **Compact rows.** An occurrence is staged as the fields its evidence row
   is minted from (entity pair, kind, file row, six range coordinates,
   native key, detail, fact key), about 190 bytes, and the evidence is
   minted again from the same fields at emission; the id it is minted with
   is checked against the one it was staged under. A node fact is the node
   plus the same evidence fields, about 420 bytes. Ranges are six decimal
   coordinates, a fifth of their JSON.
5. **A bounded cache that is also the sort buffer.** The staging database's
   page cache is `providers.dependence.staging_cache_kib` (256 MiB by
   default); it bounds the memory one import holds for its staging and is
   the buffer the engine sorts in ([PRAGMA cache_size](https://sqlite.org/pragma.html#pragma_cache_size)),
   so a table smaller than it is ordered without a spill file and a larger
   one spills once, sequentially, to a temporary file under the data
   directory.
6. **Paced writeback.** The staging file is paced to disk the way the
   store's log is (ADR-0008): every hundred milliseconds the kernel is
   asked to begin writing its dirty pages, through the shared
   `internal/writeback` package, so an import hands the disk its pages
   steadily rather than in bursts at each commit.
7. **Engine temporary files under the data directory.** The process sets
   the engine's temporary directory to `<data_dir>/tmp` at start-up
   ([PRAGMA temp_store_directory](https://sqlite.org/pragma.html#pragma_temp_store_directory)),
   so sort spills and statement journals live on the disk the user gave the
   data, never on a memory-backed system temp directory.
8. **Streamed, not paged, where the loop touches nothing else.** The alias
   list and the key set are each one ordered pass of the engine's sort read
   to the end, since their loops touch only the sink or the key file.

## Measurements

Synthetic export of 60 files × 40 methods × 5 calls (9.4 MiB, 36 thousand
occurrences), one process, staging cache 2 MiB so that every sort spills
and every table outgrows the cache -- the repository-scale regime:

| Design | Engine writes | Ratio | Wall |
|---|---|---|---|
| Before (32 MiB cache, the design's own) | 413 MiB | 44.0× | 5.1 s |
| Append and order once, JSON payloads¹ | 149 MiB | 16.0× | 3.4 s |
| plus occurrences in projection order, compact facts¹ | 105 MiB | 11.3× | 3.4 s |
| plus compact ranges (this decision) | 62 MiB | 6.6× | 3.0 s |

¹ Measured on the same generator before it gave blocks their containment
edge, a shape on which the previous design wrote 321 MiB (34.5×).

In every phase of the final design the bytes the engine writes equal the
bytes the staging file grows by, plus the sort spills: nothing is written
twice. The facts published are identical to the previous design's on the
same export (run-independent digest `66e6341f4d1c8911` from both). Of the
62 MiB, the export's own rows and their ordered copies are 26 MiB (2.8× the
CSV, which encodes an edge in 28 bytes); the rest is derived data with no
CSV counterpart: locations, identities, node facts and occurrences.

## Alternatives considered

- **Keep the schema, raise the cache.** A cache the size of the staging
  hides the spill regime and the overflow pages cost the same; on a
  monorepo unit no cache is that size.
- **Sort in Go with the external sorter of `internal/pagination`.** The
  same passes with the same disk traffic, and every derivation written as
  Go loops instead of one ordered statement each. The engine's sorter is
  already bounded by the cache and spills to the same directory.
- **A log-structured store instead of SQLite.** The staging is one import's
  scratch, deleted with it; what a log-structured store adds -- concurrent
  writers, compaction, point reads during the load -- the import does not
  use. The bulk-load discipline gives the sequential writes without a second
  engine.
- **Sort every occurrence by relation identity** (the first rewrite). Costs
  a second copy of every occurrence and its sort; the projection order
  already groups all but the merged-identity and `may_refer_to` cases.
- **Store node facts and evidence as JSON** (the first rewrite). Twice the
  bytes of the fields they are minted from, and the evidence is re-minted
  from those fields anyway.

## Consequences

- An import's staging writes are a small constant times its export and are
  sequential and paced; `TestImportWritesAreProportionalToTheExport` holds
  the bound at 8× under a 2 MiB cache and requires the published digest to
  match a reference import when one is named.
- Node ids that are not integers are refused as a malformed export, with a
  diagnostic naming the file. The engine writes integers; a producer that
  does not is not an export this import reads.
- The evidence order within one relation is the projection's order (by
  site) rather than evidence-identity order; an edge with more occurrences
  than the evidence clip keeps the first by site position, which is
  deterministic in the export.
- The staging cache is memory the import holds while it runs (256 MiB by
  default, released with the import); the alias and key passes hold one
  sort's buffer within it.

## Sources

- SQLite, "Database File Format", B-tree pages and overflow —
  https://sqlite.org/fileformat2.html#b_tree_pages
- SQLite, `vdbesort.c`, the external merge sort behind ORDER BY and CREATE
  INDEX — https://sqlite.org/src/file/src/vdbesort.c
- SQLite, "PRAGMA cache_size" and "PRAGMA temp_store_directory" —
  https://sqlite.org/pragma.html
- SQLite, "Temporary Files Used By SQLite" — https://sqlite.org/tempfiles.html
- PostgreSQL, "Populating a Database" (remove indexes during a bulk load,
  create them afterwards) — https://www.postgresql.org/docs/current/populate.html
- RocksDB, "Creating and Ingesting SST files" (bulk loading as sorted runs) —
  https://github.com/facebook/rocksdb/wiki/Creating-and-Ingesting-SST-files
- Goetz Graefe, "Implementing Sorting in Database Systems", ACM Computing
  Surveys 38(3), 2006 — https://dl.acm.org/doi/10.1145/1132960.1132964
- Patrick O'Neil, Edward Cheng, Dieter Gawlick, Elizabeth O'Neil, "The
  Log-Structured Merge-Tree (LSM-Tree)", Acta Informatica 33, 1996 —
  https://www.cs.umb.edu/~poneil/lsmtree.pdf
- Linux, `sync_file_range(2)` — https://man7.org/linux/man-pages/man2/sync_file_range.2.html
