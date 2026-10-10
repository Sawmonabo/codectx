# 04 — Directories and storage: MmapDirectory and madvise, RamDirectory, ManagedDirectory GC, retained commits, Warmer, space_usage, and on-disk sizes (tantivy 0.26.2)

Sources:
- The 0.26.2 crate source (`tantivy-0.26.2/src/...`, `tantivy-sstable-0.7.0/src/...`).
- `ARCHITECTURE.md` and `CHANGELOG.md`.
- https://docs.rs/tantivy/0.26.2/tantivy/directory/struct.MmapDirectory.html (fetched; the page shows version 0.26.2 and lists `create_from_tempdir`, `open`, `open_with_madvice` and `get_cache_info`).
- The sizes in §8 were measured on the bench arm's full-export indexes on the development host.
- Web sources are listed with a credibility note in §9.

**codectx context.**
- Each document has five text columns (name, qualified_name, signature, path, body) plus kind, path, id and token_count.
- Product rules, which are past decisions to weigh against the library, not fixed truths: complete answers with no top-k cap, deterministic ranking, and **retained snapshots with soft-deleted documents**. A generation that is no longer current must stay queryable, and a document deleted in a later generation must still be visible in the older one.

**Harness vocabulary.** `tindex <docs> <dir> <threads> [default|large|MB] [unit|every:K|end] [--tokenizer unicode61|code]`. `tquery <dir> <queries> <runs> <mode> …`. A knob without a flag is written as an exact setting followed by "needs a harness flag".

---

## 1. The Directory trait and how an Index sits on it

**What it is.** `Directory` is the storage abstraction tantivy reads and writes through. It is public and can be implemented by the user.

**Exact API.** `trait Directory` (`tantivy-0.26.2/src/directory/directory.rs:112-231`):
- `get_file_handle`, `open_read`, `delete`, `exists`, `open_write`
- `atomic_read`, `atomic_write`
- `sync_directory`
- `acquire_lock`
- `watch`
- `box_clone`

`Index::open(impl Into<Box<dyn Directory>>)` (`src/index/index.rs:510`) and `Index::create(dir, schema, settings)` (`index.rs:368`) wrap any `Directory`.

**Behaviour.**
- **Every index is wrapped in a `ManagedDirectory`** (`src/directory/managed_directory.rs:30-43`). It keeps `.managed.json`, the persisted list of every file tantivy created. Files whose names start with `.`, such as locks and `.managed.json` itself, are not managed (`managed_directory.rs:19-28`).
- **`Index::open` reads only `meta.json`.** `load_metas` calls `directory.atomic_read("meta.json")` (`index.rs:29-41,510-516`). **There is no API that opens an older commit by opstamp.** A commit is whatever `meta.json` names at that moment.
- **Readers notice new commits by watching `meta.json`.** `ReloadPolicy::OnCommitWithDelay` is the default (`src/reader/mod.rs:21-31,53`). The `MmapDirectory` watcher polls every 500 ms (`src/directory/mmap_directory/file_watcher.rs:12`). `ReloadPolicy::Manual` reloads only on `IndexReader::reload()` (`reader/mod.rs:287`).

**Applies to codectx?** Yes. A custom `Directory` that wraps `MmapDirectory` is the main lever for the snapshot rule (§4).

---

## 2. MmapDirectory and madvise

**Exact API.**
- `MmapDirectory::open(path)` (`src/directory/mmap_directory/mod.rs:236`)
- `MmapDirectory::open_with_madvice(path, Advice)`, unix only (`mmap_directory/mod.rs:219-230`). `Advice` is `memmap2::Advice`, re-exported (`mmap_directory/mod.rs:15`).
- `MmapDirectory::get_cache_info() -> CacheInfo { counters: { hit, miss }, mmapped: Vec<PathBuf> }` (`mmap_directory/mod.rs:66-79,284-295`)
- `MmapDirectory::create_from_tempdir()` (`mmap_directory/mod.rs:210`)

**Behaviour.**
- **Mapping.** Each file is mmapped once and shared through a cache of weak references, so re-opening an already mapped file is a cache hit (`mmap_directory/mod.rs:81-140`).
- **The advice** is applied to every newly mapped file, and **madvise errors are ignored** (`mmap_directory/mod.rs:123-131`). madvise support was added in 0.20: "Added support for madvise when opening an mmapped Index" (CHANGELOG, 0.20).
- **Writes.** They go through `SafeFileWriter`, which calls `sync_data` when the file is finalized (`mmap_directory/mod.rs:314-336`).
- **`atomic_write`** = write a temp file in the same directory, `sync_data` it, then rename it over the target (`mmap_directory/mod.rs:354-369`).
- **`sync_directory`** opens the directory and calls `sync_data` on it. On Windows it does nothing (`mmap_directory/mod.rs:504-523`).
- **Locks** are `flock`-style exclusive locks on `.tantivy-writer.lock` and `.tantivy-meta.lock` (`mmap_directory/mod.rs:478-497`). The lock is released when the handle drops.
- **Readers use almost no anonymous memory.** "The data is read straight from an mmapped file, and loading an index is as fast as mmapping its files" (`ARCHITECTURE.md:136`). Query memory is therefore mostly page cache, not RSS.

**Applies to codectx?** Yes.
- **Query memory and cold latency depend on page-cache behaviour.**
  - `Advice::Random` should lower read-ahead waste on cold, scattered postings reads.
  - `Advice::Sequential` or `WillNeed` suit a warm-up scan of a whole column.
- **Product mode reads full fast columns per segment** (`../harness/06-extension-points.md`), which is close to sequential. Its postings reads are random. A single advice for every file is a compromise; a wrapping `Directory` could choose advice per file extension.

**Metric.** Query latency (cold), query memory (resident page-cache pages attributed to the process), and host friendliness (read-ahead I/O).

**Benchmark variants** (need a harness flag `--madvise none|random|sequential|willneed`, applied in `tquery`'s open path):
1. Drop the caches between runs where the host allows it. Otherwise use the report's "Cold = fresh process, page cache warm" definition and state which one was used.
2. Run `tquery idx/t16-large-end queries.txt 5 product --madvise random` against `--madvise none`, then the same for `library-all`.
3. Report the median and p95 for cold and warm, the peak RSS, and major page faults (`/usr/bin/time -v` or `getrusage`).

---

## 3. RamDirectory

**Exact API.** `RamDirectory::create()`, `deep_clone()`, `total_mem_usage()` and `persist(&dyn Directory)` (`src/directory/ram_directory.rs:128-158`). `Index::create_in_ram(schema)` (`index.rs:323`).

**Behaviour.**
- Everything lives in anonymous memory: "It is mainly meant for unit testing. Writes are only made visible upon flushing" (`ram_directory.rs:117-120`).
- `persist` writes every file to another directory through `open_write`, "even if they were written using the atomic_write() api", and "If an error is encountered, files may be persisted partially" (`ram_directory.rs:152-158`).

**Applies to codectx?** Only as an index-time variant. It builds without fsyncs and then persists once, which isolates the fsync share of commit cost (see `01-indexing.md` §3). The whole index must fit in RAM twice during `persist`, which is not friendly to the host on large repositories.

**Metric.** Index time (no fsync) and index memory (the whole index resident).

**Benchmark variant.** `tindex docs.jsonl ix-ram 8 large unit --ram-then-persist`: build in `RamDirectory`, then `persist` into an empty `MmapDirectory`. Needs a harness flag. Compare against `tindex docs.jsonl ix 8 large unit`. The difference is the per-commit durability cost. Run it under `MemoryMax=4G`, and report peak RSS against index bytes.

---

## 4. ManagedDirectory garbage collection, and how retained older commits behave

**Exact API.**
- `IndexWriter::garbage_collect_files() -> FutureResult<GarbageCollectionResult { deleted_files, failed_to_delete_files }>` (`src/indexer/index_writer.rs:482`)
- `ManagedDirectory::garbage_collect(get_living_files)` (`managed_directory.rs:109-200`)
- `ManagedDirectory::list_managed_files()` (`managed_directory.rs:260`)
- `Index::validate_checksum()` (`index.rs:690`)

**Behaviour.**
- **When GC runs.** Automatically after every commit (`src/indexer/segment_updater.rs:452`) and after every merge that ends (`segment_updater.rs:672`), as well as on demand.
- **What survives.** The living set is `meta.json` plus `SegmentMeta::list_files()` for every `SegmentMeta` that is **alive in this process's inventory** (`segment_updater.rs:431-440`, `src/index/index.rs:474-476`, `src/index/index_meta.rs:26-32`). `list_files` names the six component files plus the single `.del` of the segment's *current* delete opstamp (`index_meta.rs:101-123`).
  - Every other managed file is deleted (`managed_directory.rs:138-178`). That includes old segments replaced by a merge and superseded `.del` files.
  - A failed delete is kept on the list and retried at the next GC. On Windows a mapped file is expected to fail (`managed_directory.rs:163-176`).
- **The race guard.** GC takes `.tantivy-meta.lock` while it computes the living set (`managed_directory.rs:132-151`). A reader takes the same lock while it opens segment files (`src/reader/mod.rs:190-201`). This stops "gc removes a file that was useful for process B, before process B opened it" (`managed_directory.rs:132-137`).
- **Each new managed file rewrites `.managed.json`** through `atomic_write`, which on `MmapDirectory` is a temp-file write, `sync_data` and rename (`managed_directory.rs:213-240,52-60`). So every segment component file costs one extra fsync'd small write. This adds to the commit cost described in `01-indexing.md` §3.
- **What keeps an older commit's files alive** (the hinge for retained snapshots):
  1. **A live, tracked `SegmentMeta` in the same process and on the same `Index` (or a clone).** Examples: the `IndexMeta` from `index.load_metas()`, or `Segment`s from `index.searchable_segments()`. Both track their metas in the index's shared inventory (`index.rs:29-41,643-650`). Those exact files, including that moment's `.del`, survive every GC for as long as the object lives.
  2. **A `Searcher` alone does *not* pin files in the inventory.** `SegmentReader` keeps only `segment_id` and `delete_opstamp`, not a `SegmentMeta` (`src/index/segment_reader.rs:32-50`). The reader opens segments from `searchable_segments()` and then drops the `Segment`s (`reader/mod.rs:192-200`). A held `Searcher` keeps working after GC because its files are already mmapped (unix unlink semantics). That matches ARCHITECTURE.md: "as long as the user holds and reuse the same Searcher, search will happen on an immutable snapshot of the index" (`ARCHITECTURE.md:89`). But the files vanish from the directory, so no new process can open that commit, and their disk space returns only when the mapping is dropped.
  3. **Across processes and restarts, nothing is pinned.** A new process starts with an empty inventory and reads only the current `meta.json`. Its first commit or merge GC deletes every file that the current commit does not reference.
- **Retained commits therefore need one of these designs:**
  - **(a) A directory per generation.** Close the generation's writer and never write to it again. A new generation is a new directory, built in full or by copying (hard-linking) the immutable segment files of the previous generation and then applying deltas. Tantivy files are immutable once written (ARCHITECTURE.md "Deletes": "Like all segment files, this file is immutable").
  - **(b) One directory with a retaining `Directory` wrapper.**
    - `delete(path)` refuses, or postpones, any file named by a *retained* saved `meta.json`.
    - On each commit, the caller copies `meta.json` to its own snapshot manifest.
    - To open snapshot G, a read-only wrapper serves G's saved `meta.json` from `atomic_read("meta.json")`, and `Index::open` works unchanged.
    - The wrapper must also implement `Directory::watch`. The default `ReloadPolicy::OnCommitWithDelay` registers a watch when the reader is built (`src/reader/mod.rs:80-96`). Either return a handle that never fires, or build the reader with `ReloadPolicy::Manual`, which registers none.
    - If `delete` returns an I/O error, the file stays managed and GC logs an error each round. If it returns `Ok` without deleting, tantivy forgets the file, so the wrapper owns its cleanup from then on.
  - **(c) Snapshots in one long-lived process.** Hold `IndexMeta` objects (pin by inventory, design 1). Simplest, but lost on restart.
- **Soft deletes inside a snapshot work naturally.** A snapshot's `meta.json` names each segment's `delete_opstamp`, and so its own `.del` (`index_meta.rs:116-121`). A document deleted in a later generation is alive in the older `.del`, so the older snapshot still shows it, provided the old `.del` and the pre-merge segment files are retained.
  - **Merges are the risk.** They physically drop deleted documents into new files (`src/indexer/merger.rs:244-262`), so the old files must outlive the merge.
  - `NoMergePolicy` with explicit merges only at generation boundaries keeps that predictable (`01-indexing.md` §5).

**What each side buys and costs.**
- **Tantivy's model** (keep only the current commit, GC eagerly) buys bounded disk use and no bookkeeping.
- **codectx's retained snapshots** buy reproducible answers for older generations. On tantivy they cost one of the following:
  - more disk (design a);
  - a wrapper and a manifest (design b);
  - process-lifetime-only snapshots (design c).

**Applies to codectx?** Yes. This is the main storage decision for adoption.

**Metric.** Disk (retained files), index time (hard-link or copy cost per generation, under design a), and correctness: a snapshot query returns the same hits after later commits, merges and restarts.

**Benchmark variants** (all need harness support; there is no flag today):
1. **GC baseline.** After `tindex … 8 default unit`, record `list_managed_files().len()` and the directory bytes. Then run `tdelta docs.jsonl ix unit` and record the same again. This shows how much GC removes per delta.
2. **Design (b) proof.**
   1. Wrap `MmapDirectory` with a retaining `delete`.
   2. Save `meta.json` → `snap-1.json` after the build.
   3. Run `tdelta` (delete a unit, re-add it, commit), then force a merge with `IndexWriter::merge(&all_ids)` and `wait_merging_threads()`.
   4. Restart the process and open `snap-1` through the read-only wrapper.
   5. `tquery … product` on `snap-1` must equal the pre-delta run (`tcompare` 200/200 with identical order), including the deleted unit's documents.
   6. Report the retained bytes.
3. **Design (a) cost.** Time and bytes to hard-link the previous generation's files into a new directory plus apply a delta, against a full rebuild at `tindex … 8 large end`.

---

## 5. Warmer

**What it is.** A hook that runs when a reader opens a new searcher generation. It can build per-segment caches, and drops them when that generation dies.

**Exact API.**
- `trait Warmer: Sync + Send { fn warm(&self, searcher: &Searcher) -> Result<()>; fn garbage_collect(&self, live_generations: &[&SearcherGeneration]); }` (`src/reader/warming.rs:11-20`)
- Registered with `IndexReaderBuilder::warmers(Vec<Weak<dyn Warmer>>)` and `num_warming_threads(usize)` (default 1) (`reader/mod.rs:56,128,138`).
- `SearcherGeneration::{generation_id(), segments() -> &BTreeMap<SegmentId, Option<Opstamp>>}` (`src/core/searcher.rs:32-35,54-59`).
- Example: `examples/warmer.rs` ("load values from an external sources and tie their lifecycle to that of the index segments").

**Behaviour.**
- `warm` runs inside `create_searcher`, before the new searcher is published (`reader/mod.rs:237`). Its cost is therefore paid at reload, not at the first query.
- A background thread calls `garbage_collect` every `GC_INTERVAL = 1 s`, with the live generations, when warmers exist (`warming.rs:9,41-46,147-150`).
- **A generation is keyed by `(SegmentId, delete opstamp)`.** A cache keyed this way is invalidated exactly when deletes change.

**Applies to codectx?** Yes.
- **Product mode's per-segment, per-query setup** can move into a warmer, keyed by `SegmentId` + delete opstamp:
  - the `token_count` scan, which is departure 6 in `../harness/00-library-arm.md`;
  - the live-document df, N and avgdl over the alive bitset;
  - the string-column ordinal tables.
- That turns per-query work into per-generation work, and the result is still exact over live documents.

**Metric.** Query latency (removes the per-query `token_count` scan and the statistics passes), query memory (the cached arrays: about 4–8 bytes per document per cached column), and reload latency (paid once per commit).

**Benchmark variant.** `tquery idx/t16-large-end queries.txt 5 product --warmer stats`. A warmer precomputes, per segment, `token_count` as a `Vec<u32>`, the live N, and the token sum. Needs a harness flag. Report the cold-first-query and warm median/p95 against the current product mode, plus the warm-up time and the cache bytes. Ranking must be identical (`tcompare`).

---

## 6. space_usage reporting

**Exact API.**
- `Searcher::space_usage() -> io::Result<SearcherSpaceUsage>` (`src/core/searcher.rs:240`)
- `SearcherSpaceUsage::{segments(), total()}` (`src/space_usage/mod.rs:31-62`)
- `SegmentSpaceUsage::{num_docs(), termdict(), postings(), positions(), fast_fields(), fieldnorms(), store(), deletes(), total(), component(SegmentComponent)}` (`space_usage/mod.rs:65-180`)
- `PerFieldSpaceUsage::{fields(), total()}` and `FieldUsage::{field_name(), sub_num_bytes(), column_num_bytes(), dictionary_num_bytes(), column_space_usage(), total()}` (`space_usage/mod.rs:214-325`)
- `StoreSpaceUsage::{data_usage(), offsets_usage(), total()}` (`space_usage/mod.rs:183-210`)

**Behaviour.**
- It reports bytes **per segment, per component, per field**, and for fast fields per column, including the dictionary bytes of string columns.
- History: "Add per-field size details" (CHANGELOG, 0.25), and "Improve `space_usage` reporting for JSON fields and columnar data" (CHANGELOG, 0.26).

**Applies to codectx?** Yes. It is the instrument for every disk variant in `02-settings-fields.md` (lean schema, fieldnorms off, path facet, store none).

**Benchmark variant.** Add a `tspace <dir>` tool, or a `--space` option on `tquery`, that prints `space_usage()` as JSON per segment, component and field. Needs a harness flag. Run it on every index built by the variants in `01`–`03`.

---

## 7. On-disk components and what sizes them

| file | component | what is in it | what sizes it (source) |
|---|---|---|---|
| `<seg>.term` | Terms | Per-field term dictionary: an FST mapping term → ordinal, plus a `TermInfo` store pointing into `.idx` and `.pos` (`src/termdict/mod.rs:1-19`) | Number and length of distinct terms per field. Unique-per-document keys (`id`, `node_id`) add one term per document. With the `quickwit` feature it is an sstable instead (`termdict/mod.rs:21-29`). |
| `<seg>.idx` | Postings | Doc-id blocks, plus term frequencies when the record option has freqs | Total postings; the record option (`02` §3) |
| `<seg>.pos` | Positions | Positions per occurrence | Total tokens in fields with `WithFreqsAndPositions` |
| `<seg>.fast` | FastFields | Columnar values, plus sstable dictionaries for string and bytes columns (zstd-compressed blocks of more than 2 048 bytes, `tantivy-sstable-0.7.0/src/delta.rs:56-77`) | Number of fast fields; cardinality of string columns |
| `<seg>.fieldnorm` | FieldNorms | 1 byte per document per field with fieldnorms (`src/fieldnorm/mod.rs:9-11`) | documents × fields with fieldnorms |
| `<seg>.store` | Store | Row store compressed in `docstore_blocksize` blocks (`02` §2) | Stored fields |
| `<seg>.<opstamp>.del` | Delete | Alive bitset, about max_doc/8 bytes | Present only after deletes |
| `meta.json`, `.managed.json`, `.tantivy-*.lock` | — | Commit point, managed-file list, locks | — |

(Segment component list: `src/index/segment_component.rs:9-29`; file names: `src/index/index_meta.rs:111-123`.)

---

## 8. Measured on-disk sizes (bench arm, full export: 546 613 documents, LZ4 docstore)

These are per-extension totals over the segments named in each index's `meta.json`. They were summed from the files of the existing bench indexes by a read-only scan, on the development host on Oct 8.

| index (threads, budget, cadence[, tokenizer]) | segments | `.term` | `.fast` | `.pos` | `.idx` | `.store` | `.fieldnorm` | total |
|---|---|---|---|---|---|---|---|---|
| 1, 50 MB, end | 13 | 123.4 MB (36.6%) | 59.0 MB | 56.2 MB | 45.3 MB | 45.5 MB | 7.66 MB | 337.2 MB |
| 16, 2 048 MB, end | 2 | 121.4 MB (34.3%) | 61.2 MB | 58.3 MB | 53.0 MB | 52.0 MB | 7.65 MB | 353.5 MB |
| 16, 50 MB, end | 11 | 130.7 MB (36.1%) | 62.7 MB | 58.6 MB | 51.9 MB | 50.4 MB | 7.65 MB | 361.9 MB |
| 16, 50 MB, every 1000 units | 12 | 121.3 MB | 59.9 MB | 57.0 MB | 48.2 MB | 49.0 MB | 7.65 MB | 343.1 MB |
| 16, 2 048 MB, end, `code` | 2 | 123.0 MB | 61.3 MB | 78.2 MB | 64.5 MB | 52.2 MB | 7.65 MB | 386.8 MB |

(MB = 10^6 bytes. A sixth index, 1 thread per unit, was still being written during the scan and is left out.)

**Readings, each tied to the table.**
- **`.term` is the largest component, at 34–37%.** The schema indexes eight raw `STRING` keys besides the five text columns, and two of them (`id`, `node_id`) are unique per document. The per-field split needs `space_usage` (§6). This is a measurement to run, not a conclusion.
- **`.fieldnorm` = 7.65 MB = 14 fields × 546 613 documents** (7 652 582 bytes plus footers). The 14 fields are the 5 text columns, the 8 `STRING` fields (fieldnorms on by default, `src/schema/text_options.rs:264-273`) and `token_count` (`INDEXED` turns fieldnorms on, `src/schema/numeric_options.rs:176-186`). 9 of the 14 serve no scoring purpose in either mode. That is about 4.9 MB here, roughly 1.4% of the index. Real, but small.
- **The `code` tokenizer adds 33 MB (+9%).** It adds +20 MB `.pos` and +11.6 MB `.idx` over unicode61 at the same settings. This is the disk cost of stacked camelCase parts (`03-tokenizers.md` §6A).
- **Fewer, larger segments are not reliably smaller.** One thread with 13 segments came out smallest (337 MB), and 16 threads with 2 segments came to 354 MB. The difference is mostly `.idx` and `.store`. The segments hold different document mixes, so this does not isolate segment count; treat it as noise until it is repeated.
- **`.store` is 13–15% of disk**, holding only `id`, `kind`, `path_s`, `unit` and `token_count`, all of which also exist as fast columns. This is the `--store none` variant's target (`02` §2).

**Benchmark variant.** Re-run the scan above, or `tspace` (§6), on every variant index. Report the per-component deltas against `t16-large-end`.

---

## 9. Web sources consulted (with credibility)

**Primary sources: maintainers and official.**
- Paul Masurel, "Of tantivy's indexing", July 16 2017 (tantivy 0.4): https://fulmicoton.com/posts/behold-tantivy-part2/. The author is tantivy's creator. It describes per-thread segments and budgets, and says "committing too often will hurt your indexing throughput considerably". Its benchmark is 5 M Wikipedia documents (8 GB), 4 threads, 94 s, merges disabled, on 2017 hardware. The design is still accurate; the numbers are dated.
- Paul Masurel, "Of tantivy indexing datastructure, the stacker", April 16 2019: https://fulmicoton.com/posts/tantivy-stacker/. The creator again. It explains the arena with 32-bit addresses, buckets holding a hash plus an address, and posting blocks growing from 16 B to 32 KB. It matches `tantivy-stacker-0.7.0/src/memory_arena.rs:27-28,199-204`.
- tantivy wiki, "Life of a Segment", last edited June 22 2018 (tantivy 0.6): https://github.com/quickwit-oss/tantivy/wiki/Life-of-a-Segment. A maintainer page. It covers segment states, tombstone `.del` files tied to a commit opstamp, and that obsolete files are "guaranteed to be eventually deleted by a file garbage collector". It is old; the file is now `.managed.json`.
- Quickwit, "Tantivy 0.22", April 16 2024: https://quickwit.io/blog/tantivy-0.22. A release post by the maintainers. It reports about 40% higher indexing throughput (GitHub dataset), and 760 → 590 MB memory while indexing 1.1 GB of HDFS logs, without hardware details. It also notes the 15 MB minimum budget.
- Quickwit, "Tantivy 0.19 is out", January 18 2023: https://quickwit.io/blog/tantivy-0.19. A maintainer post: about 50% faster indexing from compressing the docstore on a separate thread, with configurable compression levels and block size.
- tantivy issues #1223, #1224 and #1225 (fsync count, a missing fsync on `atomic_write`, fewer `sync_directory` calls; fixed in 0.17) and #706 (max segment size and deletes in the merge policy, closed with PR #765). These are maintainer-filed issues; only their titles and summaries were read.

**Credible secondary sources: other projects' measured reports (workload-specific).**
- ivygrep PR #432, October 8 2026: https://github.com/bvolpato/ivygrep/pull/432.
  - On macOS (M5 Max, APFS), `sync_data` maps to `F_FULLFSYNC` at about 3.7 ms each, against `F_BARRIERFSYNC` at about 0.4 ms.
  - A wrapping `Directory` cut commit durability time from 134 ms to 46 ms on a 200-file workspace.
  - It confirms that "Tantivy ends each segment file and each atomic write (`meta.json` and `.managed.json`) with `File::sync_data`", which matches §2 and §4.
  - Measured, with the platform stated, but macOS only.
- HarperFast/fulltext PR #46, September 30 2026 (tantivy 0.26.2): https://github.com/HarperFast/fulltext/pull/46.
  - Limiting merge threads to 1 cut median ingest by about 25%, and to 2 by about 12%.
  - Its writers keep the default policy but with a 50% deleted-document threshold; 30% "reduced disk use further but added close-time merge drain".
  - Internal-harness numbers with no hardware stated.
- tantivy issue #2932, May 15 2026, open: https://github.com/quickwit-oss/tantivy/issues/2932. One writer per change with `delete_term` + `add_document` + commit gave about 2 000 segments of about 26 KB and several seconds to load from cold cache. A user report with no maintainer reply. It agrees with `01-indexing.md` §3 on commit cadence.
- paperless-ngx PR #13683, August 14 2026: https://github.com/paperless-ngx/paperless-ngx/pull/13683. It claims unreferenced segment files accumulate unless `garbage_collect_files()` is called after `commit()`.
  - The 0.26.2 source runs GC inside every commit (`segment_updater.rs:452`). Files are left only when something still pins their `SegmentMeta` at that moment, or a delete fails. The PR's estimate ("roughly one full active-index generation per write batch") is consistent with files pinned at commit time and freed at the next GC.
  - Third party, with no measurements; it disagrees with the source on the cause.
- tantivy issue #2316, February 13 2024, closed: https://github.com/quickwit-oss/tantivy/issues/2316. On a 172 M-document index, setting transparent huge pages to "always" took hot query latency from about 80 ms to about 35 ms, with no gain at 1 M documents. A single user's report; only relevant at a scale far above codectx's.
- tantivy issue #3079, September 7 2026, open: https://github.com/quickwit-oss/tantivy/issues/3079. A writer killed before its first commit leaves 0-byte component files that the next commit's GC should remove. A user report with no maintainer reply.
