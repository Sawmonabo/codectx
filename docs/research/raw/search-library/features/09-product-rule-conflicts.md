# 09 — Product rules against tantivy 0.26.2: what satisfies each, and what cannot

**Subject.** tantivy 0.26.2, the newest release on crates.io. The crate source is the source of truth. Citations read `tantivy-0.26.2/src/<path>:<line>`, `tantivy-0.26.2/<file>:<line>` for files at the crate root, or a docs.rs URL pinned to 0.26.2.

**codectx's lexical model, restated so this file stands alone.**
- Five text columns: `name`, `qualified_name`, `signature`, `path`, `body`. Each document also has `kind`, `path`, `id` and `token_count`.
- Scoring is BM25F:
  - one term frequency per term, Σ weight·tf over the columns, with weights 5/5/2/2/1, summed before saturation;
  - k1 = 1.2 and b = 0.75;
  - idf = ln(1 + (N − df + 0.5)/(df + 0.5)), with df counted over the union of the five columns, on visible documents only;
  - the document length is `token_count`, and avgdl is the mean over visible documents;
  - the score is computed in f64, quantized to round(score·1e6) as an int64, and ordered by an integer and string total order.
- That order is tier, then score descending, path, start byte, end byte, kind, name, qualified name, signature, node id, search key. Every key after the score comes from the content.
- Filters: `kind` (one of a set) and folder, which is a whole folder: `P` itself or under `P/`.
- Answers are complete (no top-k cap). A delta-updated index must answer exactly as a fresh index of the same content. Older snapshots are retained, and soft-deleted documents stay visible to the snapshots that held them.

**Benchmark harness referred to below.**
- `tindex <docs> <dir> <threads> [default|large|MB] [unit|every:K|end]`
- `tquery <dir> <queries> <runs> <library|library-all|product> [--kind K]… [--path-prefix P]… [--search-threads N] [--phase NAME]`
- `tdelta <docs> <dir> [unit]` deletes one unit's documents by term, re-adds them with one body changed, commits, then waits for merges.
- `tcompare <a.jsonl> <b.jsonl>` reports both hit counts and the top-200 and top-10 overlap.
- `teval <qrels> <queries> <results.jsonl>…` reports MRR@10, S@1, S@10, R@200, mean hits and zero-hit queries.
- `tcheck <docs> <dir> <queries> [filters]` compares product mode with a reference that uses no index.
- The modes:
  - `library` is stock BM25 with `TopDocs(200)`.
  - `library-all` is stock BM25 with a keep-every-hit collector, ordered by score and then `id`.
  - `product` computes our rules in a custom `collect_segment`, from postings, the alive bitset and fast fields.

---

## Rule 1 — exact df, excluding hidden documents

**What the rule needs.** df(term) is the number of *visible* documents that contain the term in *any* of the five columns. N and the token total are counted over visible documents too.

**What the stock library gives.**
- `Bm25StatisticsProvider` has three methods (`tantivy-0.26.2/src/query/bm25.rs:15-24`): `total_num_tokens(&self, field: Field)`, `total_num_docs(&self)` and `doc_freq(&self, term: &Term)`.
- A `Term` carries one field, so df is per (field, term). There is no method for a df over several columns.
- The `Searcher` implementation counts deleted documents in all three statistics:
  - `total_num_docs` sums `segment_reader.max_doc()` (`src/query/bm25.rs:38-44`), and max_doc includes deleted documents.
  - `Searcher::doc_freq` (`src/core/searcher.rs:133-141`) sums `InvertedIndexReader::doc_freq`, which returns the `term_info.doc_freq` written at indexing time (`src/index/inverted_index_reader.rs:276-281`).
  - `InvertedIndexReader::total_num_tokens` is documented as "(including deleted documents)" (`src/index/inverted_index_reader.rs:249-251`).
- `Searcher::num_docs()` does exclude deletes. It sums `segment_reader.num_docs()` (`src/core/searcher.rs:123-128`), but BM25 does not use it.
- One stock call gives a live count: `SegmentPostings::doc_freq_given_deletes(&self, alive_bitset: &AliveBitSet) -> u32` (`src/postings/segment_postings.rs:36`). Its comment says it "will clone and scan through the posting lists. (this is a rather expensive operation)".

**Mechanism that satisfies the rule.**
1. Pass a custom `Bm25StatisticsProvider` through `Searcher::search_with_statistics_provider(query, collector, provider)` (`src/core/searcher.rs:193`) or `EnableScoring::enabled_from_statistics_provider` (`src/query/query.rs:46`). This can make per-field df and N exact over live documents. It cannot express a df over the union of the columns, because `Bm25Weight::for_terms` asserts "All terms must belong to the same field." (`src/query/bm25.rs:98-107`).
2. **Only complete mechanism:** a custom `Collector` that overrides `collect_segment` (`src/collector/mod.rs:173`). It reads `InvertedIndexReader::read_postings` for each of the five fields and unions the document ids. It counts a document only when `SegmentReader::alive_bitset()` (`src/index/segment_reader.rs:433`) shows it alive. It computes the global df in `merge_fruits` (`src/collector/mod.rs:167`). The harness's `product` mode does this.
3. Hidden documents that are not tantivy deletes, such as the soft deletes of a retained snapshot (Rule 6), are handled the same way. Open each segment with `SegmentReader::open_with_custom_alive_set(segment, Some(bitset))` (`src/index/segment_reader.rs:148-151`). The custom set is intersected with the segment's own deletes (`src/index/segment_reader.rs:176-184, 564-576`), so `alive_bitset()` then means "visible in this snapshot".

**Applies to codectx?** Yes, and it is required. After any delta, stock statistics drift away from the visible set. The df differs from ours even with no deletes at all, because ours is the union over the columns.

**Metric it moves.** Score identity against our engine (product mode vs `tcheck`); top-10 and top-200 overlap in `tcompare`; MRR@10 and S@1 in `teval`. Cost: warm and cold median latency, because each query term needs one posting read per field.

**Benchmark variant.**
```
tindex docs.jsonl idx 1 default unit
tdelta docs.jsonl idx            # deletes one unit and re-adds it changed
tquery idx queries.txt 5 product
tquery idx queries.txt 5 library-all
tcompare results/…product….jsonl results/…library-all….jsonl
tcheck docs.jsonl idx queries.txt
```
Expected result: product equals `tcheck` after the delta (smoke: 19/19). Measuring library-all's drift against a fresh index of the post-delta content **needs a new mode**: `tdelta` must emit the post-delta docs file, so that it can be indexed fresh and compared with `tcompare`.

---

## Rule 2 — a delta equals a fresh index of the same content

**What the rule needs.** After any sequence of deletes, re-adds and merges, every query serves the same hits, scores and order as a fresh index of the resulting content.

**Stock behaviour that breaks it.** Four things break it.
1. **Deleted documents stay in the statistics until a merge purges them.** See Rule 1. A delete only flips the alive bitset. It is written as a new file, `<segment>.<opstamp>.del` (`src/index/index_meta.rs:120`), and the postings and term infos keep the document.
2. **A merge estimates the token total; it does not recompute it.** `estimate_total_num_tokens_in_single_segment` (`src/indexer/merger.rs:29-58`) works like this:
   - With deletes present, it rebuilds the total from the *quantized* fieldnorms of the live documents: `count as u64 * u64::from(FieldNormReader::id_to_fieldnorm(fieldnorm_ord as u8))`.
   - With no fieldnorms, it falls back to a pro-rata: `(segment_num_tokens as f64 * ratio) as u64`.
   - The code comments that even the no-delete value "is not necessarily exact as it could have been the result of a merge between segments themselves containing deletes" (`src/indexer/merger.rs:33-35`).
   - So avgdl after delta-plus-merge differs from a fresh build's exact total, and the difference lasts.
3. **The document address used as the tie-break is not the same.** `TopDocs` breaks ties by ascending `DocAddress` (`src/collector/top_score_collector.rs:26-28`). DocAddress is (segment ordinal, doc id).
   - Segment order is `committed_segment_metas` sorted by `Reverse(max_doc)` (`src/indexer/segment_updater.rs:406-407`).
   - Those metas come from a `HashMap<SegmentId, SegmentEntry>` (`src/indexer/segment_register.rs:18, 66-70`), keyed by a random `Uuid::new_v4()` (`src/index/segment_id.rs:42-44`). So segments with equal `max_doc` fall in an arbitrary order.
   - A merge stacks the alive documents in segment order (`src/indexer/merger.rs:255-262`).
   - Re-added documents land in new segments. A delta therefore puts the same content at different addresses.
4. **Index sorting no longer exists.** The 0.24 changelog lists "remove index sorting [#2434]" under breaking changes (`tantivy-0.26.2/CHANGELOG.md:105`). No setting can make doc ids follow the content.

**Mechanism that satisfies the rule.**
- Do not use stock statistics or the address tie-break.
- The product-mode collector reads N, df and the token total from the live documents (Rule 1). It takes the document length from an exact `token_count` fast field, not from fieldnorms, so merge estimates never enter the score.
- It orders by the content-derived total order (Rule 4).
- Merges are then invisible to the answer. Smoke evidence: product mode equals the index-free reference on 19/19 queries after a delta.

**Applies to codectx?** Yes. The ranking code states the rule: a delta re-index and a fresh index of a byte-identical tree must serve the same pages.

**Metric it moves.** Identity, measured as `tcompare` top-200 and top-10 overlap of delta against fresh. The goal is 200/200 and 10/10 with equal hit counts. Also MRR@10 stability across deltas.

**Benchmark variant.**
- `tindex docs.jsonl A 1 default unit; tdelta docs.jsonl A`, then a fresh build of the post-delta content into `B` (**needs a new mode**: `tdelta` emitting the post-delta docs).
- Then `tquery A queries.txt 3 product --phase delta-product` and `tquery B queries.txt 3 product --phase fresh-product`, and `tcompare` on the two.
- Repeat with `library-all` to measure stock drift.

---

## Rule 3 — no top-k cap: complete answers

**What the rule needs.** Every matching entity is counted and orderable; the client pages through the whole set.

**Stock behaviour.**
- `TopDocs::with_limit(limit)` panics on 0 (`src/collector/top_score_collector.rs:93-95`). Every `TopDocs` is a top-k: `doc_range()` is `offset..offset+limit` (`:85-87`), and segments collect only `doc_range.end` (`src/collector/sort_key_top_collector.rs:68-71`).
- Ordering by score turns on pruning: `weight.for_each_pruning(Score::MIN, …)` (`src/collector/sort_key/sort_by_score.rs:41, 50`). A complete answer through `TopDocs` needs `limit` ≥ the match count, so a counting pass comes first.
- `Count` counts and `DocSetCollector` returns an unordered set. Neither ranks.

**Mechanism that satisfies the rule.**
- A custom `Collector` with `requires_scoring() = true` (`src/collector/mod.rs:163`).
- Its `SegmentCollector::collect(doc, score)` keeps every hit.
- The default `collect_segment` path calls `weight.for_each`, not `for_each_pruning` (`src/collector/mod.rs:192-216`). Block-max WAND is therefore never used, and nothing is pruned.
- `merge_fruits` sorts the union. The harness's `library-all` does this, and so does `product` (with our own scoring).

**Applies to codectx?** Yes, and it is required.

**Metric it moves.** Mean hits and R@200 (complete), against warm and cold median latency and peak RSS. Expect a keep-all collector to be slower than `TopDocs(200)`. Smoke warm medians: library 0.15–0.32 ms, library-all 1.5–2.9 ms.

**Benchmark variant.**
- `tquery idx queries.txt 5 library` against `tquery idx queries.txt 5 library-all`: the same scoring, top-k against complete. Compare latency, `hits` and R@200 in `teval`.
- `--search-threads 4` shows whether parallel segments recover the cost.

---

## Rule 4 — deterministic ties

**What the rule needs.** A total order that depends only on content. Ties on the quantized score break on path, start byte, end byte, kind, name, qualified name and signature, then the identity keys.

**Stock behaviour.**
- Ties fall to `DocAddress` (`src/collector/top_score_collector.rs:26-28`; comparator `.then_with(|| lhs.doc.cmp(&rhs.doc))` at `:596`). That is stable within one index and not across a delta or a rebuild (Rule 2, item 3).
- `Score` is f32 (`src/lib.rs:314`). Two documents whose f64 scores differ by less than f32 resolution tie in tantivy and not in our engine, and the reverse can happen too.
- Multi-threaded search keeps fruits in segment order: `result_placeholders[pos] = Some(fruit)` (`src/core/executor.rs:87-93`). Threads therefore do not add nondeterminism; the segment order is the problem.

**Partial stock mechanism.**
- `TopDocs::order_by` takes tuple sort keys (`src/collector/sort_key/sort_key_computer.rs:142-148`). For example: `TopDocs::with_limit(n).order_by(((SortBySimilarityScore, Order::Desc), (SortByString::for_field("path_s"), Order::Asc)))`.
- `SortByString` compares term ordinals inside a segment and converts them to `String` before the merge across segments (`src/collector/sort_key/sort_by_string.rs:51-71`). So the merged order depends on content.
- It still ties on the f32 score, it is still a top-k (Rule 3), and the code notes that each conversion is an individual dictionary lookup (`:62-63`).

**Mechanism that satisfies the rule.**
- A custom collector computes the f64 score itself (Rule 7) and quantizes it.
- It compares on dictionary ordinals within a segment and on full strings across segments, in `merge_fruits`. The harness's product mode does this, resolving only the survivors with `Dictionary::sorted_ords_to_term_cb` in tantivy-sstable 0.7.0.

**Applies to codectx?** Yes.

**Metric it moves.** Top-10 and top-200 overlap between runs, between thread counts, and between delta and fresh. Run-to-run differences must be zero.

**Benchmark variant.**
- `tquery idx q.txt 5 product` against `tquery idx q.txt 5 product --phase rerun`, then `tcompare`: identity is expected.
- `tquery idx q.txt 5 library-all --search-threads 1` against `--search-threads 8`, then `tcompare`. Expect identity, because of the executor's ordered merge.
- Delta against fresh as in Rule 2.

---

## Rule 5 — retained snapshots

**What the rule needs.** A query pins one generation and answers from it while later commits and merges proceed. Older generations stay readable while they are retained.

**Stock behaviour.**
- **In-process, a held `Searcher` is a snapshot.** ARCHITECTURE.md says: "as long as the user holds and reuse the same Searcher, search will happen on an immutable snapshot of the index" (`tantivy-0.26.2/ARCHITECTURE.md:89`). `SearcherGeneration` records `(SegmentId → delete opstamp)` and a generation id (`src/core/searcher.rs:32-35`, `generation_id()` at `:54`).
- **A held `Searcher` does not stop file GC; on Unix it survives GC only because of open mappings.**
  - `SegmentReader` keeps opened `FileSlice`s and composites, not the `SegmentMeta` (`src/index/segment_reader.rs:32-50`). Every component file is opened when the reader is built (`:152-181`).
  - GC deletes every managed file that is not in `list_files()` (`src/directory/managed_directory.rs:109-150`). That list is built from the index's live `SegmentMeta` inventory (`src/indexer/segment_updater.rs:431-440`, `src/index/index.rs:474-476`).
  - GC runs after every commit and every merge (`src/indexer/segment_updater.rs:449-452`, `:672`).
  - `MmapDirectory::delete` is `fs::remove_file` (`src/directory/mmap_directory/mod.rs:396-409`). On Unix the unlinked, mapped files stay readable until the Searcher drops, and their disk space is reclaimed only then.
  - On Windows the delete fails and is logged; the code expects this: "On windows, delete is expected to fail if the file is mmapped" (`src/directory/managed_directory.rs:168-172`).
- **Across a restart, only the latest commit exists.**
  - `load_metas` reads the single `meta.json` (`src/index/index.rs:29-33`), and `searchable_segment_metas` returns its segments (`:676-678`).
  - No commit-retention or deletion-policy API exists. `rg -n -i "deletion.?policy|commit.?point|keep_commit" src` returns nothing.
  - Old `.del` files and merged-away segments are garbage-collected.
- No public constructor builds a `Searcher` from an arbitrary segment list: `SearcherInner::new` is `pub(crate)` (`src/core/searcher.rs:269`).

- **A held `SegmentMeta` does stop GC.** `Index::new_segment_meta` is documented: "As long as the `SegmentMeta` lives, the files associated with the `SegmentMeta` are guaranteed to not be garbage collected, regardless of whether the segment is recorded as part of the index or not." (`src/index/index.rs:500-506`).
  - The inventory that GC consults is the one these metas are tracked in (`src/index/index_meta.rs:19-42`).
  - Holding the `IndexMeta` from `Index::load_metas()` (`src/index/index.rs:520`) on the writer's `Index`, or a clone of it, therefore keeps that generation's segment and `.{opstamp}.del` files.
  - After a restart, the metas can be rebuilt from a codectx-side manifest:
    - `SegmentId::from_uuid_string` (`src/index/segment_id.rs:74`);
    - `Index::new_segment_meta(segment_id, max_doc)` (`src/index/index.rs:505`);
    - `SegmentMeta::with_delete_meta(num_deleted, opstamp)` (`src/index/index_meta.rs:166-168`), which is `#[doc(hidden)]`.
  - The rebuild must happen before the first commit or merge. After that, GC deletes every file outside the current metas.

**Mechanisms that satisfy it.**
1. **Hold `Searcher`s** for retained generations, in-process only. Disk use grows with unlinked files. This does not survive a restart and does not work on Windows.
1a. **Hold each retained generation's `SegmentMeta`s** on the writer's `Index`, and keep a manifest that records segment id, max_doc and delete opstamp per generation. Readers come from `Index::segment(meta)` (`src/index/index.rs:652`) plus `SegmentReader::open`. The collector is driven per reader, because a `Searcher` cannot be built from an arbitrary segment list. This keeps tantivy's own deletes as the visibility mechanism, and each delete commit's `.del` file stays on disk while a generation references it.
2. **Keep segments, and own visibility outside tantivy.** This route is durable and does not depend on old `.del` files:
   - Set `NoMergePolicy` (`src/indexer/merge_policy.rs:24`, via `IndexWriter::set_merge_policy`, `src/indexer/index_writer.rs:470`), or merge only at times the product chooses.
   - Do not call `delete_term`. Record each document's visibility per generation in the product's own store, keyed by `id`.
   - At query time, build one `AliveBitSet` per segment for the pinned generation. The public `write_alive_bitset` and `AliveBitSet::open` do this (`src/fastfield/alive_bitset.rs:13, 52`).
   - Open readers with `SegmentReader::open_with_custom_alive_set` (`src/index/segment_reader.rs:148`). The bitset must be sized to the segment's `max_doc`, which an assert checks (`:570`).
   - Drive `Collector::collect_segment` and `merge_fruits` directly over those readers, because a `Searcher` cannot be built from them.
   - The cost: segment files must outlive every generation that references them, so the product decides when to merge. Doc ids are reassigned by any merge (`src/indexer/merger.rs:255-262`), so visibility must be keyed by a stable field, not by DocAddress.
3. **A wrapping `Directory` that refuses `delete`**, plus saved copies of `meta.json` served through `atomic_read`. The trait methods are `src/directory/directory.rs:133, 172`. This works, but GC retries and logs on every commit, and the product must collect files itself.

**Applies to codectx?** Yes. Mechanism 1 covers queries in flight. Snapshots that outlive the process need mechanism 1a or 2. 1a is closer to stock behaviour. 2 also gives soft deletes that survive merges (Rule 6).

**Metric it moves.** Disk bytes held by retained generations; warm and cold median latency (each custom alive set costs one bitset build per segment per pinned generation); open time.

**Benchmark variant.** **Needs a new mode.** `tquery` pins the newest commit only. The new mode would open an index, run `tdelta` from a second process while a held `Searcher` answers `queries.txt`, and assert that the answers do not change. A second variant would reopen an older generation through custom alive sets. Report disk bytes before and after the Searcher drops.

---

## Rule 6 — soft deletes kept across merges

**What the rule needs.** A document hidden in the current generation stays in the index for the retained generations that still show it, whatever merges run.

**Stock behaviour.**
- tantivy's delete *is* soft until the next merge. `delete_term` only marks documents, and "the deletion itself will be visible only after calling `commit()`" (`src/indexer/index_writer.rs:672-686`). The same text appears on docs.rs: https://docs.rs/tantivy/0.26.2/tantivy/indexer/struct.IndexWriter.html (fetched; that page does not say when deleted documents are physically removed).
- **A merge drops deleted documents for good.**
  - The new doc-id map is built from `reader.doc_ids_alive()` (`src/indexer/merger.rs:255-262`).
  - Postings skip deleted documents: "deleted doc are skipped as they do not have a `remapped_doc_id`" (`:439`).
  - Terms whose documents were all deleted are removed (`:388-392`).
  - The store copies only alive documents (`:512`).
- Merges start on their own: `DefaultMergePolicy = LogMergePolicy` (`src/indexer/mod.rs:49`), which `consider_merge_options` runs after each commit (`src/indexer/segment_updater.rs:453`).
- `del_docs_ratio_before_merge` defaults to 1.0, which "means that deletes are not taken in account when identifying merge candidates" (`src/indexer/log_merge_policy.rs:12-15`). Deletes alone never trigger a merge, but any size-triggered merge still purges them.

**Mechanism that satisfies it.**
- Do not express soft deletes as tantivy deletes. Keep visibility outside the index and apply it through custom alive sets (Rule 5, mechanism 2). Merges then see no deletes and keep every document.
- Or keep using tantivy deletes and hold every retained generation's `SegmentMeta`s (Rule 5, mechanism 1a). A merge still writes a purged segment for new generations, but the pre-merge segment and its `.del` file stay on disk for the generations that reference them. The cost is double storage for merged ranges until those generations are released.
- The alternative, `NoMergePolicy` plus tantivy deletes, keeps them only until someone calls `IndexWriter::merge` (`src/indexer/index_writer.rs:535`). It also fixes each segment to one alive bitset per commit, and the old `.del` files are garbage-collected (Rule 5).
- `merge_filtered_segments` (`src/indexer/segment_updater.rs:188`, `#[doc(hidden)]`) can purge with a caller-chosen alive set into a *new* index. Use it for a deliberate compaction once no retained generation needs the hidden documents.

**Applies to codectx?** Yes.

**Metric it moves.** Disk bytes, because hidden documents are never reclaimed until compaction; segment count and the query latency that follows from it; correctness of retained-generation answers.

**Benchmark variant.** **Needs a new mode**: `tdelta --soft` records the deleted unit's ids instead of calling `delete_term`, and `tquery … product --hide <idsfile>` builds custom alive sets. Compare against the current `tdelta` with `tcompare` for answer identity. Compare disk bytes and the `delta-merge` wall time as well.

---

## Rule 7 — f64 BM25F

**What the rule needs.** The weighted tf is summed across columns before saturation. The arithmetic is f64, quantized to 1e-6.

**Stock behaviour.**
- `pub type Score = f32;` (`src/lib.rs:314`). `K1: Score = 1.2`, `B: Score = 0.75` (`src/query/bm25.rs:8-9`). The idf is computed in f32: `((doc_count - doc_freq) as Score + 0.5) / (doc_freq as Score + 0.5)` (`src/query/bm25.rs:52-56`).
- **Each `Bm25Weight` belongs to one field and saturates on its own.** `for_terms` asserts one field (`src/query/bm25.rs:98-107`), and the score is `self.weight * self.tf_factor(fieldnorm_id, term_freq)` (`:179-181`).
- A per-field `BoostQuery(TermQuery)` under a `BooleanQuery` therefore sums five separately saturated BM25 scores. That is not BM25F.
- Document length is the field's quantized fieldnorm id (one byte), looked up in a 256-entry table (`src/query/bm25.rs:62-68`), not an exact count.
- For a phrase, the idf is the *sum* of its terms' idfs (`src/query/bm25.rs:121-126`).
- Every scoring callback carries f32: `&mut dyn FnMut(DocId, Score)` (`src/query/weight.rs:11, 92`), and `SegmentCollector::collect(doc, score: Score)` (`src/collector/mod.rs:302`).
- A custom `Weight` or `Scorer` could compute BM25F internally, but it would still hand the collector an f32.

**Mechanism that satisfies it.** Score outside tantivy's scoring path, in a custom `collect_segment`:
- Use `requires_scoring() = false`, and issue the search with `AllQuery` so the Weight is ignored.
- Read postings with freqs, and with positions for phrases (`InvertedIndexReader::read_postings`, `src/index/inverted_index_reader.rs:265`).
- Take the document length from a `token_count` fast field.
- Compute in f64 and quantize to int64 before any comparison.
- tantivy then provides storage, postings and columns only. This is the harness's product mode: smoke equality is 18/18 queries and 18/18 filtered, and Go's `math.Log` was ported bit for bit (200 127 values; `f64::ln` differs on 1 891).

**Applies to codectx?** Yes. f32 cannot carry our scores to an exact 1e-6 quantization, and summing per-field BM25 is a different ranking.

**Metric it moves.** Score identity (product against `tcheck`); MRR@10, S@1 and S@10 differences between `library-all` and `product` in `teval`. Product's own cost shows in warm and cold median latency.

**Benchmark variant.**
```
tqrels docs.jsonl qrels 7
tquery idx qrels/queries-name.txt 5 library-all
tquery idx qrels/queries-name.txt 5 product
teval qrels/qrels-name.tsv qrels/queries-name.txt results/…library-all….jsonl results/…product….jsonl
```
Repeat for `doc` and `subword`, and with `--kind function --path-prefix <folder>`. The quality gap between the arms isolates BM25F/f64 from per-field f32 BM25.

---

## Summary

| Rule | Stock tantivy | Mechanism that satisfies it |
|---|---|---|
| Exact df, hidden excluded | No: per-field df, and deleted documents are counted | Custom `collect_segment` with an alive or custom alive set; union df in `merge_fruits` |
| Delta = fresh | No: stats with deletes, estimated token totals after a merge, ties on DocAddress | The same collector, with an exact `token_count` and a content order |
| No top-k | `TopDocs` is top-k and prunes | Custom keep-all collector, using `for_each` (no WAND) |
| Deterministic ties | Ties on DocAddress, compared in f32 | Integer score plus string keys in `merge_fruits` |
| Retained snapshots | A held `Searcher` in-process (Unix); only the latest commit after a restart | Held `SegmentMeta`s plus a manifest (GC-safe by documented guarantee), or own visibility with `open_with_custom_alive_set` |
| Soft deletes across merges | Merges purge deletes | Hold pre-merge `SegmentMeta`s for retained generations, or never call `delete_term` and keep visibility outside the index |
| f64 BM25F | f32, per-field saturation, quantized lengths | Score in the collector from postings and fast fields |
