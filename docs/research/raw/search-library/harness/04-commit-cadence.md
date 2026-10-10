# 04 — Commit cadence: the cost of committing per unit

**Context.** Production seals each unit (one provider scope, for example one directory or one file) as its own SQLite transaction and activates once at the end. The closest tantivy equivalent is one `IndexWriter::commit()` per unit. A tantivy commit, however, makes every indexing thread flush its in-memory segment, persist the segment and the meta file with fsyncs, and may schedule merges.

**Measured** (smoke fixture of 4 800 documents in 510 units, on the development host, a WSL2 filesystem; `tindex … unit` vs `… end`):

| threads | budget | cadence | commits | segments at end | wall | CPU | peak RSS |
|---|---|---|---|---|---|---|---|
| 1 | 50 MB | per unit | 510 | 6 | 9 246 ms | 4 846 ms | 46 MB |
| 16 | 16 × 50 MB | per unit | 510 | 2 | 33 888 ms | 20 742 ms | 486 MB |
| 1 | 2 048 MB | once | 1 | 1 | 106 ms | 106 ms | 30 MB |
| 16 | 2 048 MB | once | 1 | 2 | 248 ms | 228 ms | 83 MB |

**Finding.** A per-unit commit costs about **18 ms at 1 thread and 66 ms at 16 threads**. That is roughly 90× (1 thread) to 140× (16 threads) the cost of the same build committed once. It is fixed overhead per commit, not indexing work, so an export with tens of thousands of units will spend minutes on commits alone. More threads make each commit slower, because every thread's arena is flushed.

**What the harness offers.** The cadence is `unit` (faithful to per-unit sealing, the default), `every:K` units, or `end`, so the measurement can report the cadence's cost separately from indexing throughput. The memory budget is `default` (50 MB per thread; tantivy's minimum is 15 MB per thread, `tantivy-0.26.2/src/indexer/index_writer.rs:32`) or `large` (2 048 MB in total), passed to `Index::writer_with_num_threads` (`src/index/index.rs:580`). Builds also report the `-final` phase separately: the last commit plus `wait_merging_threads`.

**Forced merge after a delta.** `tdelta … --merge` first waits for the merges the commit started. It then opens a fresh writer, merges every searchable segment into one with `IndexWriter::merge` (`src/indexer/index_writer.rs:535`), waits, and records `delta-forcemerge`. A merge rewrites segments without their deleted documents, so library statistics stop counting them; this is what audit rule 3's before/after comparison needs. Smoke (6-segment index, 267-document unit): 7 segments holding 267 deleted documents became 1 segment holding 0, in 80 ms. Product mode on the merged index still matched the reference on 19 of 19 queries.
