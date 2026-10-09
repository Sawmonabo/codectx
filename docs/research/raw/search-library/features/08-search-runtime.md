# 08 — Search runtime: readers, searchers, executors, warmers, the doc store, snippets, caches

**Subject:** tantivy 0.26.2. The crate source is the source of truth; citations are `tantivy-0.26.2/src/<path>:<line>` (sub-crates by their own name and version). docs.rs pages for 0.26.2 were fetched to cross-check `ReloadPolicy` and `IndexReaderBuilder`; they match the source word for word.

**codectx context used throughout.** Documents have five text columns (name, qualified_name, signature, path, body) plus kind, path, id and token_count. Product rules: complete answers with no top-k cap, deterministic ranking, retained snapshots with soft-deleted documents, and filters by kind and by folder. Our engine pins one generation per query and answers each query on one thread. Our store keeps no body text; hits are hydrated from byte ranges.

**Benchmark harness referred to below** (already built):
- `tindex <docs> <dir> <threads> [default|large|MB] [unit|every:K|end] [--tokenizer unicode61|code]`
- `tquery <dir> <queries> <runs> <library|library-all|product> [--kind K]… [--path-prefix P]… [--search-threads N] [--phase NAME]`. Run 0 is cold (a fresh process; index open untimed); later runs are warm.
- `tdelta <docs> <dir> [unit]` (phases `delta-put`, `delta-commit`, `delta`, `delta-merge`).
- The harness opens its reader with `ReloadPolicy::Manual` and default cache settings. `library` mode fetches the 200 served ids with `Searcher::doc` (the doc store); `library-all` reads `id` from the fast field; `product` reads everything from fast fields.

Summary table:

| Capability | Applies to codectx? | Metric it moves | Variant |
|---|---|---|---|
| IndexReader / builder | Yes, one reader per open index | open time, cold latency | needs a timed `reader-open` phase |
| ReloadPolicy | Yes: `Manual` | delta visibility, idle CPU | existing (`Manual`), no new run required |
| Searcher, generation | Yes: the per-query pin | correctness after delta | `tdelta` then `tquery … product` |
| Retaining old generations | Partly, needs our own manifest | disk bytes, correctness | needs a new mode (see 3.3) |
| Executor | Library modes only | warm/cold median latency | `--search-threads 1/2/4/8` |
| Warmer | Yes: live N and token totals | product cold latency, open time | needs `--warm-stats` |
| Doc store + block cache | Weak: we fetch ids from fast fields | library latency | needs `--doc-cache N`, `--id-source` |
| Snippets | No for parity; optional for display | none on ranking | optional new mode |
| Query cache | None exists | — | — |
| mmap advice | Possibly | cold latency | needs `--madvise` |

---

## 1. IndexReader and IndexReaderBuilder

**What it is.** The entry point for searching. It owns the current `Searcher` and decides when a new commit becomes visible.

**API.**
- `Index::reader(&self) -> Result<IndexReader>` and `Index::reader_builder(&self) -> IndexReaderBuilder` (`tantivy-0.26.2/src/index/index.rs:450,458`).
- `IndexReaderBuilder::reload_policy(ReloadPolicy)`, `::doc_store_cache_num_blocks(usize)`, `::warmers(Vec<Weak<dyn Warmer>>)`, `::num_warming_threads(usize)`, `::try_into() -> Result<IndexReader>` (`src/reader/mod.rs:109,118,128,138,66`).
- `IndexReader::reload(&self) -> Result<()>` and `IndexReader::searcher(&self) -> Searcher` (`src/reader/mod.rs:287,298`).

**Behaviour.**
- Defaults: `ReloadPolicy::OnCommitWithDelay`, one warming thread, `DOCSTORE_CACHE_CAPACITY` = 100 blocks (`src/reader/mod.rs:53-57`; `src/store/reader.rs:25`).
- Building opens every searchable segment. The doc comment says it "may take hundreds of milliseconds of time and it may return an error" (`src/reader/mod.rs:63-65`).
- Opening a segment opens all of its files up front: terms, store, postings, positions, fast fields, fieldnorms, and the delete bitset, which it reads in full (`src/index/segment_reader.rs:148-185`). With the default `MmapDirectory` each file is an mmap, shared through a weak-reference map so a second open of the same path reuses it (`src/directory/mmap_directory/mod.rs:81-85,134-150`).
- Opening holds `META_LOCK` so that garbage collection cannot delete files while they are being opened (`src/reader/mod.rs:192-195`; GC takes the same lock, `src/directory/managed_directory.rs:132-140`).
- `reload()` builds a complete new searcher, warms it, and only then swaps it in through an `ArcSwap` (`src/reader/mod.rs:241-253`). Queries already running keep the searcher they hold.
- `searcher()` is an `ArcSwap` load plus an `Arc` clone (`src/reader/mod.rs:255-257`). It is lock-free and cheap. Its doc comment: "This method should be called every single time a search query is performed. The same searcher must be used for a given query, as it ensures the use of a consistent segment set." (`src/reader/mod.rs:291-297`).

**Applies to codectx.** Yes. Use one reader per open index, and take one `Searcher` per query: that is our "pin a generation per query". The open cost grows with the segment count. That is a reason to merge before activating a generation.

**Metric.** Index open time, which the harness does not time today, and the cold-query latency of a fresh process.

**Variant.** Needs a new phase: `tquery … --phase reader-open` timing only `index.reader_builder().reload_policy(ReloadPolicy::Manual).try_into()` once per cold process. Run it on indexes built with `tindex <docs> <dir> 1 default unit` (many segments) and `tindex <docs> <dir> 1 default end` (few segments), to show how open time scales with the segment count.

## 2. ReloadPolicy

**What it is.** It decides when a commit becomes visible to the reader.

**API.** `enum ReloadPolicy { Manual, OnCommitWithDelay }` (`src/reader/mod.rs:20-31`).

**Behaviour.**
- `Manual`: "No change is reflected automatically. You are required to call `IndexReader::reload()` manually." (`src/reader/mod.rs:22-27`).
- `OnCommitWithDelay` registers a watch callback that calls `reload()` (`src/reader/mod.rs:85-97`). On `MmapDirectory`, the watcher is one thread, `thread-tantivy-meta-file-watcher`. It polls `meta.json` every `POLLING_INTERVAL` = 500 ms, computes a checksum of the file, and runs the callbacks when the checksum changes (`src/directory/mmap_directory/file_watcher.rs:12,43-63`). The delay before a commit becomes visible is therefore up to 500 ms plus a full reader rebuild. The docs' "within milliseconds" is optimistic for the mmap directory. `RamDirectory` routes watch events directly (`src/directory/ram_directory.rs:102-104`).
- The reload always loads the latest `meta.json` (`src/index/index.rs:676-678`). Neither policy can load an older commit.
- A failed automatic reload is only logged (`src/reader/mod.rs:88-90`).

**Applies to codectx.** Yes, as `Manual`. codectx activates a generation explicitly, and a background reload would change what "current" means without that step. `Manual` also avoids a polling thread per open index. The harness already uses `Manual`.

**Metric.** Delta visibility latency (commit → searchable) and idle CPU. It does not affect query latency.

**Variant.** None is required. If measured: `tdelta <docs> <dir> unit`, then time `reader.reload()` (Manual) against the wall time until the first search sees the delta (OnCommitWithDelay). That needs a new phase, `reload`.

## 3. Searcher reuse, generations, and how long files live

### 3.1 Searcher and SearcherGeneration

**What it is.** A `Searcher` is a fixed set of `SegmentReader`s with their alive bitsets. `SearcherGeneration` identifies that set.

**API.**
- `Searcher::generation(&self) -> &SearcherGeneration` (`src/core/searcher.rs:80`).
- `SearcherGeneration::generation_id() -> u64` and `::segments() -> &BTreeMap<SegmentId, Option<Opstamp>>` (`src/core/searcher.rs:54,59`). The map goes from segment id to delete opstamp.
- Also `Searcher::segment_readers()`, `num_docs()`, `doc_freq(&Term)`, `search(&dyn Query, &C)`, `search_with_statistics_provider(...)` and `search_with_executor(...)` (`src/core/searcher.rs:157,123,133,180,193,220`).

**Behaviour.**
- `Searcher` is `Clone` and wraps an `Arc<SearcherInner>` (`src/core/searcher.rs:68-71`). The searcher pool was removed in 0.19 ("Remove Searcher pool and make `Searcher` cloneable", CHANGELOG.md:380, under 0.19).
- Generation ids come from a per-reader `AtomicU64` counter and increase by one on every reload (`src/reader/mod.rs:203-212`). The ids are not commit opstamps, and they are not stable across processes.
- A generation's identity is (segment id, delete opstamp) per segment (`src/core/searcher.rs:15-35`). A delete-only commit therefore produces a new generation even when no segment changed.
- `num_docs()` counts live documents (`src/core/searcher.rs:123-129`, from `SegmentReader::num_docs`, which is the alive count, `src/index/segment_reader.rs:186-190`).
- `doc_freq()` sums the raw term-info doc frequencies, which include deleted documents (`src/core/searcher.rs:133-141`).

**Applies to codectx.** Yes. One `Searcher` per query gives a consistent segment set and a consistent alive bitset for the whole query, including `merge_fruits`. That is the per-query pin. Soft deletes are visible exactly as of the generation's delete opstamps.

### 3.2 Does a held Searcher keep its files?

**Inside one process, with the mmap directory on Linux and macOS: the data, yes; the files on disk, no.**
- Garbage collection deletes every managed file that is not in the "living" set (`src/directory/managed_directory.rs:109-152`). The living set is the files of every `SegmentMeta` still alive in the index's in-process inventory, plus `meta.json` (`src/indexer/segment_updater.rs:431-439`; `src/index/index.rs:474-476`).
- GC runs automatically after every commit (`src/indexer/segment_updater.rs:452`) and after every merge (`src/indexer/segment_updater.rs:672`). It can also be called with `IndexWriter::garbage_collect_files()` (`src/indexer/index_writer.rs:482`).
- A `SegmentReader` holds open file slices only. It holds no `SegmentMeta` (`src/index/segment_reader.rs:32-50`), and `SearcherInner` holds readers plus a generation of ids (`src/core/searcher.rs:259-265`). A held `Searcher` therefore does not put its segments in the living set. After a merge, its files are unlinked.
- The `Searcher` keeps working because its mmaps remain valid after an unlink. This is what the doc comment "It guarantees that the `Segment` will not be removed before the destruction of the `Searcher`" (`src/core/searcher.rs:64-67`) relies on in practice.
- On Windows the delete fails while a file is mapped, and the file is retried at the next GC (`src/directory/managed_directory.rs:168-176`).
- A `SegmentMeta` *is* tracked in the inventory (`src/index/index_meta.rs:19-42`). So holding the `IndexMeta` from `Index::load_metas()` (`src/index/index.rs:520`) or a `Segment` from `Index::searchable_segments()` (`src/index/index.rs:643`), on the same `Index` (or a clone of it) that the writer uses, keeps those exact files, including the `.{opstamp}.del` file (`src/index/index_meta.rs:101-123`), out of GC.

**Across processes, or after a restart:** nothing pins an old generation. The reader opens only the latest `meta.json`. `IndexMeta::deserialize` is crate-private (`src/index/index_meta.rs:334`). The first commit or merge after a restart deletes every managed file not in the then-current metas.

### 3.3 Retained snapshots

**Mechanism.** No stock API serves an older commit. The public building blocks are:
- `Index::new_segment_meta(segment_id, max_doc)` with `SegmentMeta::with_delete_meta(num_deleted, opstamp)` (`src/index/index.rs:505`; `src/index/index_meta.rs:168`), which rebuild tracked metas from a manifest that codectx keeps;
- `Index::segment(meta)` plus `SegmentReader::open(&segment)` (`src/index/index.rs:652`; `src/index/segment_reader.rs:143`), which give readers over exactly those segments;
- `Collector::collect_segment` and `merge_fruits` (`src/collector/mod.rs:173`), which can be called directly over those readers without a `Searcher`.

Holding the rebuilt `SegmentMeta`s in the writer's process keeps their files out of GC.

**Applies to codectx.** Yes, and it is required for the retained-snapshot rule. The cost is a codectx-side snapshot manifest (segment ids, max_doc, delete opstamps), because tantivy keeps only the latest commit.

**Metric.** Disk bytes, which grow with every retained generation because merged-away segments stay on disk; and correctness (a pinned query after a merge returns the same page).

**Variant.** Needs a new mode: `tquery <dir> <queries> 5 product --phase pinned-after-merge`. It (a) records generation G's metas, (b) runs `tdelta <docs> <dir> unit` including `delta-merge`, and (c) answers from G's segments and requires the page to equal the pre-delta answer. Report the disk bytes before and after.

## 4. The multi-threaded Executor

**What it is.** It runs `collect_segment` for each segment of one query on a rayon pool.

**API.**
- `enum Executor { SingleThread, ThreadPool(Arc<rayon::ThreadPool>) }`, `Executor::single_thread()`, `Executor::multi_thread(num_threads, prefix)` (`src/core/executor.rs:11-38`).
- `Index::set_multithread_executor(n)`, `Index::set_default_multithread_executor()` (one thread per available CPU), `Index::set_executor(Executor)` to share one pool, and `Index::search_executor()` (`src/index/index.rs:301,313,307,295`).
- `Searcher::search_with_executor(query, collector, &executor, EnableScoring)` (`src/core/searcher.rs:220-237`).

**Behaviour.**
- The default is `Executor::single_thread()` (`src/index/index.rs:392`). `Searcher::search` uses the index's executor (`src/core/searcher.rs:204-205`).
- Parallelism is one task per segment only. The doc comment: "It is powerless at making search faster if your index consists in one large segment … multithreading a single query on several threads will not improve your throughput. It can actually hurt it." (`src/core/searcher.rs:208-219`).
- **Determinism:** fruits are written into a slot indexed by segment ordinal, so `merge_fruits` always receives them in segment order, whatever order the threads finish in (`src/core/executor.rs:66-93`). Any tie rule that depends on merge order stays stable across thread counts.
- A failed task fails the whole search; panics propagate (`src/core/executor.rs:40-43,89-98`).
- The shared-executor API arrived in 0.22 ("Add shared search executor", CHANGELOG.md:232), and the Executor API changed in 0.24 (CHANGELOG.md:164).

**Applies to codectx.** For the library modes only. Our engine answers each query on one thread, and product mode refuses `--search-threads` so that it stays comparable. The gain depends on the segment count, which depends on commit cadence and merges.

**Metric.** Warm and cold median latency, and p95. CPU time per query rises.

**Variant.** `tquery <dir> <queries> 5 library --search-threads N` and `… library-all --search-threads N` for N in 1, 2, 4, 8. Run each on two indexes: `tindex <docs> <dir-unit> 1 default unit` and `tindex <docs> <dir-end> 1 default end`. Phases come out as `-sN`. Expect a gain only on the many-segment index.

## 5. Warmers

**What it is.** A hook that runs on every new searcher generation before that generation is published, so that per-segment or per-generation state is ready when the first query runs.

**API.**
- `trait Warmer: Sync + Send { fn warm(&self, searcher: &Searcher) -> Result<()>; fn garbage_collect(&self, live_generations: &[&SearcherGeneration]); }` (`src/reader/warming.rs:14-20`).
- Registered as `Weak` references through `IndexReaderBuilder::warmers`, with `num_warming_threads` (`src/reader/mod.rs:128,138`).

**Behaviour.**
- `warm` runs inside `create_searcher` before the swap (`src/reader/mod.rs:237,250`), for the initial open as well as for every reload. With more than one warmer and more than one thread, the warmers run in parallel (`src/reader/warming.rs:88-89,165-171`).
- With no live warmers, no thread is created (`src/reader/warming.rs:80-84`).
- A background thread, `tantivy-warm-gc`, ticks every `GC_INTERVAL` = 1 s (`src/reader/warming.rs:9,132-162`). It calls `garbage_collect` with the generations still alive, but only when some warmed generation has been dropped (`src/reader/warming.rs:107-129`). Liveness comes from the `SearcherGeneration` inventory, so a generation stays live while any `Searcher` of it exists.
- The doc comment lists keys an implementer can choose: per `generation_id`, per `(segment_id, delete_opstamp)`, or per `segment_id` (`src/core/searcher.rs:20-30`).

**Applies to codectx.** Yes. Product mode now scans `token_count` over live documents on its cold run to get N and the token total (harness departure 6). A warmer keyed by `(segment_id, delete_opstamp)` can compute the live count and the live token sum once per segment state. Each query would then sum per-segment values. This is the equivalent of our aggregate stored at activation. The same warmer could hold codectx's df cache keyed by generation, because tantivy has no query or statistics cache of its own (section 8).

**Metric.** Product-mode cold latency goes down. Reader open and reload time go up by the same scan.

**Variant.** Needs a new option: `tquery <dir> <queries> 5 product --warm-stats`, which registers the warmer and keeps a strong `Arc` to it (warmers are held weakly and are pruned once dropped, `src/reader/warming.rs:95-103`). Compare the cold median with and without the option, and report reader open time (section 1's `reader-open` phase) for both.

## 6. The doc store: fetch cost and its cache

**What it is.** The row store for `STORED` fields. It holds compressed blocks of serialized documents, with a skip index from doc id to block.

**API.**
- `Searcher::doc::<D>(DocAddress) -> Result<D>` (`src/core/searcher.rs:88-91`). `Searcher::doc_async` exists only behind the `quickwit` feature (`src/core/searcher.rs:106-115`).
- `Searcher::doc_store_cache_stats() -> CacheStats { num_entries, cache_hits, cache_misses }` (`src/core/searcher.rs:96-104`; `src/store/reader.rs:123-130`).
- `IndexReaderBuilder::doc_store_cache_num_blocks(n)` (`src/reader/mod.rs:118`).
- Index settings `docstore_compression` and `docstore_blocksize` (`src/index/index_meta.rs:217,226`).

**Behaviour.**
- Default block size is 16 384 bytes (`src/index/index_meta.rs:230-232`). The writer closes a block once its content plus offsets exceeds the size (`src/store/writer.rs:61-68`), so a block is about 16 KiB plus one document.
- Default compression is LZ4, because `lz4-compression` is a default feature (`src/store/compressors.rs:133-143`; `Cargo.toml` `[features] default`). Zstd needs the `zstd-compression` feature.
- One fetch does a skip-index seek, then an LRU lookup by block start offset under a `Mutex`. On a miss it reads and decompresses the whole block and inserts it. It then slices out the document and deserializes **every stored field** of it (`src/store/reader.rs:197-258`). The doc comment: "Calling `.get(doc)` is relatively costly as it requires decompressing a compressed block … It should not be called to score documents" (`src/store/reader.rs:230-238`).
- `cache_num_blocks = 0` disables the cache (`src/store/reader.rs:169-171`, `NonZeroUsize::new`).
- The cache belongs to one `StoreReader`. Each `Searcher` creates one store reader per segment (`src/core/searcher.rs:284-287`). So the cache is per segment, per generation: every reload starts cold, and the memory bound is about `segments × n × ~16 KiB` of decompressed blocks (100 blocks ≈ 1.6 MiB per segment).
- Fast-field alternative: `segment_reader.fast_fields().str("id")` (`src/fastfield/readers.rs:195`) gives a `StrColumn`. `ord_to_str` decodes one sstable block of the column's dictionary per lookup (`tantivy-columnar-0.7.0/src/column/dictionary_encoded.rs:96`; `tantivy-sstable-0.7.0/src/dictionary.rs:495-510`), with no cache. `sorted_ords_to_term_cb` resolves a sorted batch in one pass (`tantivy-sstable-0.7.0/src/dictionary.rs:515`).

**Applies to codectx.** Weakly. The harness schema stores id, kind, path_s, unit and token_count, but not body. Product and library-all modes read only fast fields. Only `library` mode pays 200 store fetches per query. codectx hydrates its own spans and needs no stored body. The store could be dropped from the schema altogether (no `STORED` flags), which saves disk bytes, if the library mode switches to fast-field ids.

**Metric.** `library` warm and cold median latency; disk bytes (store size versus fast-field size); peak RSS (the cache).

**Variant.** Needs new options on `tquery`: `--doc-cache N` (N = 0, 100, 1000) and `--id-source store|fast`. Run `tquery <dir> <queries> 5 library --doc-cache N` and `… library --id-source fast`, and print `doc_store_cache_stats()` once at the end of each run. For disk bytes, build a second index with the `STORED` flags removed (needs a `tindex` schema option, `--no-store`) and compare the `.store` sizes from `Searcher::space_usage()` (`src/core/searcher.rs:240`).

## 7. Snippets and highlighting

**What it is.** It picks the best text window containing query terms and marks the matches.

**API.**
- `SnippetGenerator::create(&Searcher, &dyn Query, Field) -> Result<SnippetGenerator>` and `SnippetGenerator::new(terms_text, tokenizer, field, max_num_chars)` (`src/snippet/mod.rs:400,386`).
- `set_max_num_chars(usize)`, `snippet_from_doc(&D)` and `snippet(&str)` (`src/snippet/mod.rs:435,448,466`).
- `Snippet::fragment()`, `highlighted() -> &[Range<usize>]`, `to_html()` and `set_snippet_prefix_postfix` (`src/snippet/mod.rs:168,173,150,178`).

**Behaviour.**
- `create` keeps only the query's terms on the given field (`src/snippet/mod.rs:406-410`). It weights each term `1 / (1 + doc_freq)` (`src/snippet/mod.rs:419-422`), where `doc_freq` includes deleted documents (section 3.1).
- It re-tokenizes the text with the field's tokenizer (`src/snippet/mod.rs:425`; `src/index/index.rs:418`). It needs neither positions nor stored text: `snippet(&str)` accepts any text, and `snippet_from_doc` is only a convenience that reads a stored document.
- Matching is per token, by `token.text.to_lowercase()` against the term set (`src/snippet/mod.rs:103-110`). Phrases are not matched as phrases: each phrase term highlights alone.
- The window limit defaults to `DEFAULT_MAX_NUM_CHARS = 150` (`src/snippet/mod.rs:70`). The check is `next.offset_to - fragment.start_offset > max_num_chars` (`src/snippet/mod.rs:215`), which compares byte offsets even though the docs say characters.
- The best fragment is the highest score; ties go to the earliest fragment (`src/snippet/mod.rs:234-245`), so the choice is deterministic. Default markup is `<b>`…`</b>` (`src/snippet/mod.rs:72-73`).
- The ranges algorithm improved in 0.24 (CHANGELOG.md:129).

**Applies to codectx.** Not for parity or ranking. codectx returns hits with byte spans, not highlighted fragments. If display highlighting is ever wanted, `snippet(&hydrated_text)` works without storing body text, but the phrase-blind matching and byte-window rule would have to be accepted or replaced.

**Metric.** None on ranking (MRR@10, S@1, S@10, R@200 unchanged). It adds latency per served hit.

**Variant.** Optional: a new mode, `--snippets body:150`, that hydrates each served hit's text and times `SnippetGenerator::snippet`. It is not needed for the adoption decision.

## 8. Query cache, and the caches that do exist

**Query cache: none.** `rg -n -i "query.?cache|filter.?cache|result.?cache" src` over tantivy-0.26.2 finds no match. Nothing caches query results, filter bitsets or statistics across searches.

**The caches that do exist:**
- **Doc-store block LRU**, per segment per searcher, 100 blocks by default (section 6; `src/store/reader.rs:25,72-77`).
- **mmap handle map**, `MmapCache`: a weak-reference map from path to mapping, so repeated opens reuse one mapping (`src/directory/mmap_directory/mod.rs:81-150`). It caches mappings, not pages; page residency is left to the OS page cache. `MmapDirectory::open_with_madvice(path, Advice)` applies an `madvise` hint to every mapping (`src/directory/mmap_directory/mod.rs:223`, applied at `:123-130`), and `get_cache_info()` reports hits and misses (`:284`).
- **Per-segment inverted-index reader map**: one `InvertedIndexReader` per field, created lazily and reused (`src/index/segment_reader.rs:33,222-230`).
- **BM25 tf-norm table**: 256 precomputed values per `Bm25Weight`, built per query (`src/query/bm25.rs:62-68`). It is per query, not a cache across queries.
- **Block-max score memo** inside one postings cursor (`src/postings/block_segment_postings.rs:30,152-171`). It is also per query.
- **Aggregation sub-aggregation buffers** (`src/aggregation/cached_sub_aggs.rs:26,99,173`). These are per request; see 07.

**Applies to codectx.** codectx's df cache keyed by generation has no tantivy counterpart. To keep it, codectx must hold it itself, keyed by `SearcherGeneration::generation_id()` (process-local) or by its own generation key, and could fill it from a warmer (section 5). The madvise hint may matter for cold latency on large indexes.

**Metric.** Cold median latency (madvise), and warm latency on repeated terms (an application-side df cache).

**Variant.** Needs a new option: `tquery <dir> <queries> 5 library|product --madvise random|sequential|willneed`, which opens the index with `Index::open(MmapDirectory::open_with_madvice(dir, advice)?)`. Compare cold medians against the default (no advice).
