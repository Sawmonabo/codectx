# 16 — Directories, on-disk structures, file lifecycle, and space usage

Version: tantivy 0.26.2 and sub-crates `tantivy-sstable-0.7.0`, `tantivy-columnar-0.7.0`. Paths are inside `tantivy-0.26.2/` unless named. Docs: https://docs.rs/tantivy/0.26.2/tantivy/directory/index.html, https://docs.rs/tantivy/0.26.2/tantivy/space_usage/index.html

## Directories
- `MmapDirectory::open(path)` (`src/directory/mmap_directory/mod.rs:236`) and `MmapDirectory::open_with_madvice(path, Advice)` — "Opens a MmapDirectory in a directory, with a given access pattern. This is only supported on unix platforms." (`:218-230`). `Advice` is `memmap2::Advice`, re-exported (`:15`). The advice is applied to every file as it is mapped, and errors are ignored (`:126-128`). Use: `Index::open(MmapDirectory::open_with_madvice(dir, Advice::Random)?)` (`src/index/index.rs:510`). Added in 0.20 (CHANGELOG.md:353). `get_cache_info()` lists mapped files (`:284`).
- `RamDirectory::create()`, `deep_clone()`, `total_mem_usage()`, `persist(&dyn Directory)` (`src/directory/ram_directory.rs:128-158`): build fully in memory and write once. Index time only; the result is then read as an mmap index.
- The `Directory` trait (`src/directory/directory.rs:112-231`: `get_file_handle`, `open_read`, `delete`, `exists`, `open_write`, `atomic_read`, `atomic_write`, `sync_directory`, `acquire_lock`, `watch`) can be implemented to wrap another directory, e.g. to make `delete` a no-op or to log reads.
- README: "SIMD integer compression when the platform/CPU includes the SSE2 instruction set" (README.md:40).

## File lifecycle (relevant to retained snapshots)
- Every index file is "managed": `ManagedDirectory` keeps the list of files tantivy created, and `garbage_collect(get_living_files)` deletes every managed file not in the living set (`src/directory/managed_directory.rs:20-30, 109-152`). Files whose names start with "." (locks) are not managed (`:10-14`).
- GC runs after every commit and after every merge (`src/indexer/segment_updater.rs:452, 672`), and on demand via `IndexWriter::garbage_collect_files()` (`src/indexer/index_writer.rs:482`). The living set is `meta.json` plus the files of every segment the segment manager still lists (`src/indexer/segment_updater.rs:431-440`), so the files of segments that earlier commits or merges replaced are deleted, along with superseded `.del` files (a segment lists only the `.del` of its current delete opstamp, `src/index/index_meta.rs:101-121`).
- On Unix, a deleted file that is already mapped stays readable, so a live `Searcher` keeps working; on Windows the delete is expected to fail while the file is mapped (`managed_directory.rs:168-175`).
- `meta.json` holds exactly one commit (segments, schema, settings, opstamp, payload) (`src/index/index_meta.rs`; `src/indexer/single_segment_index_writer.rs:41-50` shows its fields). There is no list of past commits and no API to open an older one.

## On-disk structures (one set per segment, `SegmentComponent`, `src/index/segment_component.rs:11-28`)
- Postings (`.idx`): blocks of 128 doc ids, delta-encoded and bitpacked, followed by a block of term frequencies; the last partial block is vint-encoded (ARCHITECTURE.md, "postings/"). A skip list stores, per block, the (fieldnorm_id, tf) pair for block-max scoring (`src/postings/skip.rs:77-81`; `src/postings/serializer.rs:404-428`).
- Positions (`.pos`) per term occurrence; only with `WithFreqsAndPositions`.
- Term dictionary (`.term`): an FST from term to ordinal plus a term-info store (ARCHITECTURE.md, "termdict/"). With the `quickwit` feature the dictionary is an sstable instead (`src/termdict/mod.rs:21-28`).
- Fieldnorms (`.fieldnorm`): one byte per document per field with norms.
- Fast fields (`.fast`): columnar, codecs chosen automatically (see 10).
- Store (`.store`): row-oriented, compressed blocks of `docstore_blocksize` (see 10).
- Deletes (`.del`): one alive bitset per (segment, commit opstamp).

## Space usage reporting
`Searcher::space_usage()` -> `SearcherSpaceUsage { segments(), total() }` (`src/core/searcher.rs:240`; `src/space_usage/mod.rs:31-62`). Per segment: `termdict()`, `postings()`, `positions()`, `fast_fields()`, `fieldnorms()` (each a `PerFieldSpaceUsage`), `store()` (data and offsets), `deletes()`, `total()` (`src/space_usage/mod.rs:117-172`). Per field: `field_name`, `total`, sub-sizes, and for columns the column and dictionary bytes (`:214-322`). Per-field detail is new in 0.25 (CHANGELOG.md:78) and JSON/columnar detail in 0.26 (CHANGELOG.md:47).

## What it means for codectx
- Disk attribution: the bench's Rust arm reports total disk bytes only. Variant: print `space_usage()` per component and per field for every build, to show where bytes go (body positions vs fast fields vs store) and which of the other variants (no STORED, fieldnorms off, Zstd) pays.
- madvise variant: `Advice::Random` vs `Normal` vs `WillNeed` on cold queries (fresh process; drop the page cache only where the host rules allow it). Expected effect: lower read-ahead waste on random posting access (Random), or a faster first query at the cost of reading more (WillNeed).
- RamDirectory + persist variant for the `end` cadence: compare index wall time and peak RSS with the mmap build.
- Retained snapshots across processes need either one index directory per generation, or a custom `Directory` that never deletes plus saved copies of each `meta.json`; see 17.
