# 07 — Collectors and aggregations in tantivy 0.26.2

**Scope.** This file covers how tantivy hands matched documents to user code: `TopDocs` and its sort keys, `Count`, `DocSetCollector`, `MultiCollector`, `FilterCollector`, `FacetCollector`, `HistogramCollector`, the custom `Collector`/`SegmentCollector` API (with `collect_block` and `requires_scoring`), and the aggregation framework (terms, histogram, range, stats, top_hits, cardinality) with its memory limits.

**Sources.** The crate source `tantivy-0.26.2` is the source of truth. Citations are `tantivy-0.26.2/src/<path>:<line>` and were checked against the file. Release notes are from the crate's `CHANGELOG.md`. The public `TopDocs` method list was cross-checked against https://docs.rs/tantivy/0.26.2/tantivy/collector/struct.TopDocs.html, which lists the same nine methods and no `custom_score`.

**codectx rules this file is judged against.**
- Complete answers: no top-k cap.
- Deterministic ranking: ties break on content, never on where a document happens to sit in the index.
- Retained snapshots with soft-deleted documents.
- Filters by `kind` and by whole folder.
- Documents have five text columns (`name`, `qualified_name`, `signature`, `path`, `body`), plus `kind`, `path`, `id` and `token_count`.

**Benchmark vocabulary** (the existing harness):
- `tquery <dir> <queries> <runs> library|library-all|product [--kind K]… [--path-prefix P]… [--search-threads N] [--phase NAME]`.
  - `library` runs `TopDocs::with_limit(200).order_by_score()` plus an untimed `Count`.
  - `library-all` runs a custom collector that keeps every `(doc, score)`.
  - `product` runs our own scoring through a custom `collect_segment`.
- `teval` reports MRR@10, S@1, S@10, R@200, mean hits and zero-hit queries.
- `tcompare` reports hit counts and the top-200 and top-10 overlap.
- `tdelta` runs the delta phases.
- Latency is the cold or warm median from `tquery`. Peak memory means peak RSS, measured by wrapping `tquery` in GNU `time -v`.

A variant that no current mode covers is marked **"needs a new mode"**, with its exact settings.

---

## 0. How a search drives a collector (applies to everything below)

**What it is.** `Searcher::search(query, collector)` is the only entry point. The collector chooses whether scores are computed and how each segment is walked.

**API and behaviour.**
- **Scoring on or off.** `Searcher::search_with_statistics_provider` turns scoring on only when `collector.requires_scoring()` is true (`tantivy-0.26.2/src/core/searcher.rs:199-203`). Otherwise the weight is built with `EnableScoring::disabled_from_searcher`.
- **Segment walk.** `search_with_executor` builds the weight once. It calls `collector.collect_segment(weight, segment_ord, reader)` per segment through `executor.map`, then `collector.merge_fruits(fruits)` (`src/core/searcher.rs:220-237`).
- **Fruit order is fixed.** Fruits reach `merge_fruits` in segment-ordinal order even on the thread pool. Results are written to `result_placeholders[pos]` by task index (`src/core/executor.rs:87-93`). Merge input is therefore deterministic whatever the thread count.
- **Default segment loop** (`default_collect_segment_impl`, `src/collector/mod.rs:186-218`) has four cases:
  - alive bitset and scoring: `weight.for_each`, skipping deleted documents (`:193-198`);
  - alive bitset, no scoring: `for_each_no_score` in blocks, then a per-document `is_alive` check and `collect(doc, 0.0)` (`:200-206`);
  - no deletes, scoring: `for_each` (`:207-211`);
  - no deletes, no scoring: `collect_block(docs)` (`:212-216`).
- **`collect_block` is narrow.** It is reached only when the segment has **no deletes and scoring is off**. A segment with any soft delete falls back to per-document `collect`. The block length is `COLLECT_BLOCK_BUFFER_LEN = 64` (`src/docset.rs:15`).
- **Deleted documents never reach a collector** on the default path or on the `TopDocs` score path (§1.2). A collector that overrides `collect_segment` must check `reader.alive_bitset()` itself. Our product mode does.

**Applies to codectx?** Yes, as the foundation of every point below. Two facts matter most:
1. Soft-deleted documents are already excluded from collection. They are not excluded from BM25 statistics; see 06-scoring.md.
2. The deterministic merge order lets a custom collector produce a reproducible result.

**Metric.** None directly.

**Variant.** None. Every variant below rests on this path.

---

## 1. `TopDocs`

### 1.1 What exists in 0.26.2

| API | Signature (abridged) | Location |
|---|---|---|
| `TopDocs::with_limit(limit)` | panics if `limit == 0` | `src/collector/top_score_collector.rs:93-96` |
| `.and_offset(offset)` | skips the first `offset` documents | `:139` |
| `TopDocs::for_doc_range(start..end)` / `.doc_range()` | limit = end − start, offset = start | `:77-87` |
| `.order_by_score()` | `Collector<Fruit = Vec<(Score, DocAddress)>>` | `:226-228` |
| `.order_by_fast_field::<T>(field, Order)` | `Vec<(Option<T>, DocAddress)>` | `:299-310` |
| `.order_by_u64_field(field, Order)` | `Vec<(Option<u64>, DocAddress)>` | `:217-224` |
| `.order_by_string_fast_field(field, Order)` | `Vec<(Option<String>, DocAddress)>` | `:312-320` |
| `.order_by(sort_key_computer)` | any `SortKeyComputer` | `:322-330` |
| `.tweak_score(f)` | `f: Fn(&SegmentReader) -> Fn(DocId, Score) -> K` | `:426-438` |

**There is no `custom_score` in 0.26.2.** `rg custom_score src/` finds only a doc-comment variable name and a test name. docs.rs for 0.26.2 lists no such method. The same job, a sort key computed from fast fields without the score, is done by `order_by` with a closure. The blanket impl `impl<F, SegmentF, TSortKey> SortKeyComputer for F where F: Fn(&SegmentReader) -> SegmentF, SegmentF: FnMut(DocId) -> TSortKey` is at `src/collector/sort_key/sort_key_computer.rs:455-468`. Its `requires_scoring` keeps the trait default, `false` (`:113-115`), so no BM25 is computed.

**Sort-key building blocks** (`src/collector/sort_key/mod.rs:9-15`):
- `SortBySimilarityScore`;
- `SortByStaticFastValue<T>` (u64, i64, f64, bool, date);
- `SortByString` (a string fast column, compared by term ordinal inside a segment and converted to `String` at harvest, `sort_by_string.rs:55-70`);
- `SortByBytes`;
- `SortByErasedType` (the type is chosen at runtime, at a cost; new in 0.26 per CHANGELOG #2770).

Each block pairs with an `Order` as `(computer, Order)` (`order.rs:443`).

**Tuples.** Tuples of 2, 3 or 4 sort keys compare lexicographically (`sort_key_computer.rs:141`, `:325`, `:383`). The crate's own test sorts by `((SortBySimilarityScore, score_order), (SortByString::for_field("city"), city_order))` (`src/collector/sort_key/mod.rs:318-321`).

**What `Order` means** (`order.rs:286-292`):
- `Order::Desc` maps to `NaturalComparator`, where `None` sorts lowest.
- `Order::Asc` maps to `ReverseNoneIsLowerComparator`: ascending, with `None` last.
- `NaturalComparator` uses `partial_cmp(...).unwrap_or(Equal)` (`order.rs:87`), so a NaN score compares equal to everything.

### 1.2 Behaviour

- **Tie rule.** "In case of a tie on the sort key, documents are always sorted by ascending `DocAddress`" (`top_score_collector.rs:26-28`). It is implemented in `compare_for_top_k`: `.reverse().then_with(|| lhs.doc.cmp(&rhs.doc))` (`:589-597`). `DocAddress` is (segment ordinal, doc id). It depends on segment layout, commit cadence and merges, not on content.
- **Top-k engine (`TopNComputer`).**
  - It allocates a buffer of `2 × top_n` up front: `let vec_cap = top_n.max(1) * 2` (`:609-611`).
  - When the buffer is full it keeps the best `top_n` with `select_nth_unstable_by`, and remembers the first one dropped as a threshold (`:636-660`).
  - Later pushes that are not strictly better are rejected: `if self.comparator.compare(&sort_key, last_median) != Ordering::Greater { return; }` (`:626`).
  - The documented precondition: "Items must be `push`ed to the TopNComputer in ascending `DocId|DocAddress` order" (`:507`).
  - Each segment computes its own top `doc_range.end`, so **offset + limit**, not limit (`sort_key_top_collector.rs:39-42`, `:66-73`).
  - The merge pushes every segment's `into_vec()` output, in segment order, into one more `TopNComputer` of size `doc_range.end`, then skips `offset` (`sort_key_top_collector.rs:76-94`).
  - Deep pages therefore cost O(offset + limit) per segment.
- **Block-WAND applies only to bare `order_by_score()`.**
  - `SortBySimilarityScore` overrides `collect_segment_top_k`. It calls `weight.for_each_pruning(Score::MIN, …)` and feeds the current threshold back (`sort_by_score.rs:27-61`). This is where Block-WAND pruning happens (see 06-scoring.md for when the boolean weight can prune, `src/query/boolean_query/boolean_weight.rs:516`).
  - The generic `for_each_pruning_scorer` only calls back when `score > threshold` (`src/query/weight.rs:47-60`).
  - Deleted documents are skipped inside that callback: `if alive_bitset.is_deleted(doc) { return threshold; }` (`sort_by_score.rs:38-46`).
  - Every other sort-key computer uses the default `collect_segment_top_k`, which is the full `default_collect_segment_impl` (`sort_key_computer.rs:117-133`). That includes `(SortBySimilarityScore, Order::Desc)` (the `(T, Order)` impl at `order.rs:443` does not override it), any tuple, `tweak_score` and `order_by_fast_field`. They score every match and get no pruning.
  - Tuples do evaluate lazily: the tail key is computed only when the head ties the threshold (`sort_key_computer.rs:216-257`).
- **Wrapping kills pruning too.** Only `TopBySortKeyCollector` overrides `Collector::collect_segment` (`sort_key_top_collector.rs:62`; `rg "fn collect_segment"` finds no other override). Putting `TopDocs` inside a tuple `(TopDocs…, Count)`, a `MultiCollector` or a `FilterCollector` sends it through the default loop with full scoring.
- **"No limit" is not a TopDocs mode.**
  - `with_limit(0)` panics (`:94`).
  - `with_limit(usize::MAX)` makes `top_n.max(1) * 2` overflow. That is a panic in debug and a wrapped, failing capacity request in release.
  - To get everything you must `Count` first and pass that count. You then pay for the extra counting pass, and each segment allocates `2 × total_hits` entries up front.
- **`tweak_score`** passes the f32 BM25 score through a user closure, and `requires_scoring() = true` (`top_score_collector.rs:455-457`). The closure sees an `f32` (`Score = f32`, `src/lib.rs`); it cannot recover an f64 score.

### 1.3 Applies to codectx?

- **As the answer collector: no.** The product forbids a top-k cap, and `TopDocs` is by construction a top-k with a mandatory limit. Its tie rule (`DocAddress`) breaks "a delta and a fresh index serve the same page": the same content in a different segment layout ties differently. Its score is f32 BM25, not our f64 BM25F (see 06).
- **As a fast first page: maybe.** `TopDocs` with a tuple sort key can make ties content-based. For example, `(score desc, path asc, start asc)` over the `path_s` string column and the `start` u64 column. That is much closer to our comparator (score, then path, then start byte; `cmpScored` continues with end, kind, name, qualified name, signature).
  - Two gaps remain. Score equality is on f32 rather than the f64 quantized to 1e-6, so near-ties split differently. And the tuple loses Block-WAND.
- **`order_by` with a closure** is the supported way to rank by a key built from fast fields. It does not help a BM25F score, which needs postings.

### 1.4 Metric moved

| Change | Metrics it should move |
|---|---|
| WAND (bare `order_by_score`) vs tuple / tweak (no WAND) | warm and cold median |
| Content-based tie keys | top-10 and top-200 overlap with `product` in `tcompare`; MRR@10 and S@1 only where ties straddle rank 1–10; run-to-run and delta-vs-fresh page identity |
| `with_limit(Count)` | peak memory; latency, because of the counting pass |

### 1.5 Benchmark variants

1. **Baseline, already run:** `tquery <dir> queries.txt 5 library` uses `TopDocs::with_limit(200).order_by_score()`.
2. **Needs a new mode `library-tie`:**
   - Collector: `TopDocs::with_limit(200).order_by(((SortBySimilarityScore, Order::Desc), (SortByString::for_field("path_s"), Order::Asc), (SortByStaticFastValue::<u64>::for_field("start"), Order::Asc)))`.
   - Same query as `library`, 5 runs.
   - Compare its warm median to `library`; the difference is the cost of losing WAND.
   - Run `tcompare` against `product`; the difference shows how much tie order explains.
   - Repeat after `tdelta` on both the delta index and a fresh index of the same tree. Pass: identical top 200.
3. **Needs a new mode `library-topall`:**
   - `n = searcher.search(&q, &Count)?`, then `TopDocs::with_limit(n.max(1)).order_by_score()`, timed together.
   - Compare peak memory and median with `library-all`.
   - This measures the "Count then TopDocs" route to a complete answer against the keep-all collector.
4. **Pruning-loss check:** `library` with `--search-threads 1` against the same collector wrapped as `(TopDocs::with_limit(200).order_by_score(), Count)` in one search. **Needs a new mode `library-tuple`.** It shows the cost of fusing the hit count into the timed search.

---

## 2. `Count`

**What it is.** It counts matching live documents: `pub struct Count` (`src/collector/count_collector.rs:36`).

**API.** `searcher.search(&query, &Count) -> usize`, or `query.count(&searcher)`.

**Behaviour.**
- `requires_scoring() = false` (`count_collector.rs:51-53`).
- The segment collector increments on `collect` (`:68-70`). It has no `collect_block` override, so it uses the default loop over the block (`src/collector/mod.rs:309-313`).
- Deleted documents are excluded by the default loop (§0).
- `Query::count` (`src/query/query.rs:145-152`) takes a different path. It calls `Weight::count` per segment. The default counts with the alive bitset, or `count_including_deleted()` when the segment has no deletes (`src/query/weight.rs:78-85`).
- `TermWeight::count` returns the term's `doc_freq` straight from the term dictionary when the segment has no deletes. With deletes, it walks the postings against the alive bitset (`src/query/term_query/term_weight.rs:56-65`).
- Both paths give live counts.

**Applies to codectx?** Partly.
- Our `hits` is the number of **folded entities**, not raw documents (library arm: "the" gives 1 799 raw documents against 994 folded). `Count` gives the raw figure.
- It is useful as a candidate-set size, for example to size a keep-all buffer up front.
- Use `Query::count` rather than `search(&q, &Count)` for a one-term query: with no deletes, it is a dictionary lookup.

**Metric.** Mean hits (raw) and latency of the counting pass.

**Variant.** It already exists as the untimed hit count in `library`. **Needs a new mode `count-only`:** time `query.count(&searcher)` against `searcher.search(&q, &Count)` for 5 runs, on a fresh index and after `tdelta`. This shows the dictionary shortcut and how much deletes cost.

---

## 3. `DocSetCollector`

**What it is.** It returns the set of matching documents.

**API.** `searcher.search(&q, &DocSetCollector) -> HashSet<DocAddress>` (`src/collector/docset_collector.rs`).

**Behaviour.**
- `requires_scoring() = false` (`:26-28`).
- Each segment fills a `HashSet<DocId>` (`:53-55`). The merge rebuilds one `HashSet<DocAddress>` sized to the total (`:30-41`).
- The set is unordered and carries no score.
- Deleted documents are excluded (§0).

**Applies to codectx?** Rarely.
- It could feed an exact-match tier (the name, qualified-name and prefix tiers) where membership matters and order comes from our own comparator.
- A `HashSet` costs more memory than a sorted `Vec<DocId>` per segment, and the order still has to be rebuilt.
- A custom collector that keeps a `Vec<u32>` per segment is strictly better for us.

**Metric.** Peak memory and latency against a `Vec` collector.

**Variant.** **Needs a new mode `docset`:** `searcher.search(&q, &DocSetCollector)`, reporting set size and the warm median, against a custom `Vec<DocId>` collector with `requires_scoring() = false` that overrides `collect_block`. Run on a fresh index (where `collect_block` is used) and after `tdelta` (where it is not, §0).

---

## 4. `MultiCollector` and tuple collectors

**What it is.** It runs several collectors in one pass over the matches.

**API.**
- Tuples `(A, B)` up to larger arities are typed; for example, the pair impl is at `src/collector/mod.rs:322-390`.
- `MultiCollector::new()` with `add_collector(c) -> FruitHandle` (`src/collector/multi_collector.rs:171`, `:184`) is type-erased through `Box<dyn Fruit>`. `handle.extract(&mut multi_fruit)` downcasts and "can panic" on a mismatch (`:113-120`).

**Behaviour.**
- `requires_scoring` is the OR of the children (`multi_collector.rs:222-227`; tuple `mod.rs:346-348`).
- `collect_block` forwards the block to each child (`multi_collector.rs:265`, `mod.rs:379`).
- Neither overrides `collect_segment`, so a `TopDocs` child loses Block-WAND (§1.2).
- Per the doc comment, `MultiCollector` "should only be used for use cases where the Collector types is unknown at compile time" (`multi_collector.rs:125-126`).

**Applies to codectx?** Yes, in its tuple form.
- One pass can produce the answer list plus facet or terms counts per kind or folder, if the product shows those counts.
- `MultiCollector` itself is not needed: our collector set is fixed at compile time.

**Metric.** Latency of one pass against two passes; peak memory.

**Variant.** **Needs a new mode `library-all+facets`:** `(KeepAll, FacetCollector::for_field("path_facet") with add_facet("/"))`, against `library-all` followed by a separate facet search. This needs the `path_facet` field from §6.

---

## 5. `FilterCollector` and `BytesFilterCollector`

**What it is.** It wraps another collector and drops documents whose fast-field value fails a predicate.

**API.**
- `FilterCollector::new(field: String, predicate: Fn(T) -> bool, collector)` (`src/collector/filter_collector_wrapper.rs:70`, `:86`).
- `BytesFilterCollector::new(field, Fn(&[u8]) -> bool, collector)` (`:251`, `:265`).

**Behaviour.**
- A document is accepted if **any** of its values passes (`accept_document`, `:160-169`). A document with no column or no value is rejected.
- `collect_block` filters into a 64-entry buffer before forwarding (`:187-199`; CHANGELOG 0.26 #2727).
- `requires_scoring` is the inner collector's (`:133-135`).
- `T` must implement `HasAssociatedColumnType`. In `tantivy-columnar-0.7.0` that is `u64`, `i64`, `f64`, `bool`, `DateTime` and `Ipv6Addr` only (`columnar/column_type.rs:97-145`), so **a `STRING | FAST` column such as `kind` or `path_s` cannot be the predicate field**.
- `BytesFilterCollector` opens a `Bytes` column (`src/fastfield/readers.rs:206-214`), not a `Str` column (`:195-203`), so it does not read a string field either.
- It does not override `collect_segment`, so a wrapped `TopDocs` loses Block-WAND.
- Filtering happens **after** the query has matched, and in scored mode after scoring. Non-matching documents are still scored.

**Applies to codectx?** Weakly.
- The kind filter would need `kind` stored as a `u64` code fast field, with `FilterCollector::<_, _, u64>` and a predicate over the allowed codes. The folder filter would need a bytes column with a `starts_with(P + "/") || == P` predicate.
- The harness already filters in the query: `MUST ConstScoreQuery(TermQuery(kind) | TermQuery/RegexQuery(path_s), 0.0)`. That lets the intersection skip non-matching documents before scoring.
- `FilterCollector` should only win when the filter is unselective.
- Our engine scores against unfiltered corpus statistics in every case. Neither method changes df, because df comes from the term dictionary.

**Metric.** Warm median on filtered runs; byte cost of the extra fast fields.

**Variant.** **Needs a new mode and an index option.**
- Add `kind_code: u64 FAST` at index time.
- `tquery … library-all --kind function --kind method --filter-mode collector`, wrapping the keep-all collector in `FilterCollector::new("kind_code".into(), |c: u64| c == F || c == M, keep_all)`.
- Compare against the existing query-side filter at the same settings. Same hits are required; then compare medians.

---

## 6. `FacetCollector` (and a facet `path` field)

**What it is.** It counts matching documents under the children of a hierarchical facet.

**API.**
- `let mut fc = FacetCollector::for_field("path_facet"); fc.add_facet("/pkg");` (`src/collector/facet_collector.rs:204`, `:220`).
- The result is `FacetCounts`, read with `.get("/pkg")` (`:433`) or `.top_k("/pkg", k)` (`:452`).

**Behaviour.**
- `requires_scoring() = false` (`:284-286`).
- `collect` counts each document once per collapsed child, even if it has several facet values (`:370-377`).
- The merge sums into a `BTreeMap<Facet, u64>` (`:288-296`), so `get()` iterates in facet byte order, which is deterministic.
- `top_k` breaks ties on count desc, then facet asc (`impl Ord for Hit`, `:30-37`). That is also deterministic.
- Adding two facets where one is a prefix of the other panics (`:220-232`).
- Deleted documents are excluded (§0).
- At index time the facet tokenizer emits **every ancestor** of a facet: `/top/a/b` is indexed as `/`, `/top`, `/top/a` and `/top/a/b` (`src/tokenizer/facet_tokenizer.rs:3-11`, test `:93-110`).
  - So a `TermQuery` on the facet `/pkg/api` matches every file under `pkg/api` and never `pkg/apix`.
  - That is exactly our whole-folder rule (`P` itself or under `P/`), and it needs no `RegexQuery`.

**Applies to codectx?** Yes, for folder filtering and for per-folder counts.
- The folder filter becomes one term lookup instead of an exact term plus a regex scan of the `path_s` dictionary.
- If the product ever shows per-folder hit counts, `FacetCollector` gives them exactly and in deterministic order.
- Cost: one more indexed field with depth-many terms per document. A file path of depth d adds d+1 terms.
- An exact file path is a leaf facet and matches itself, which matches the current rule.

**Metric.**
- Filtered warm and cold median (folder filter by term against regex).
- Disk bytes (the extra field).
- Same hits as the current `--path-prefix` (correctness gate).

**Variant.** **Needs a new index option and mode.**
- `tindex <docs> <dir> 1 default end --path-facet` adds `path_facet: FacetOptions` built from the cleaned path split on `/`.
- `tquery <dir> queries.txt 5 library-all --path-prefix pkg/api --path-filter facet` builds the MUST clause from `TermQuery(Term::from_facet(path_facet, &Facet::from("/pkg/api")))`.
- Gates against `--path-filter regex`:
  - equal hits on `doc`, `docs` and an exact file (the library arm's smoke cases: 0, 534 and 12 raw);
  - lower median;
  - the disk-bytes difference reported.

---

## 7. `HistogramCollector`

**What it is.** It produces fixed-width bucket counts over one numeric fast field.

**API.** `HistogramCollector::new::<T>(field: String, min_value: T, bucket_width: u64, num_buckets: usize)`, where `T` must be u64, i64 or date (it asserts; `src/collector/histogram_collector.rs:48-63`).

**Behaviour.**
- Values below `min_value` or beyond the last bucket are **silently dropped** (`:74-81`).
- It reads the first value per document (`first_or_default_col(0)`, `:119`).
- `requires_scoring() = false` (`:130`).
- The merge adds vectors (`:134-136`).

**Applies to codectx?** No answer path needs it. A `token_count` histogram could describe the corpus offline, but our N and average document length are exact sums, which a histogram does not give.

**Metric.** None.

**Variant.** None. It is not worth benchmarking for codectx.

---

## 8. Custom `Collector` / `SegmentCollector` API

**What it is.** The extension point every collector above is built on. Both of our non-library harness modes use it.

**API** (`src/collector/mod.rs`):
- `trait Collector: Sync + Send` (`:141`) with:
  - `type Fruit`, `type Child: SegmentCollector`;
  - `check_schema` (`:150`);
  - `for_segment(segment_ord, &SegmentReader) -> Result<Child>` (`:156-160`);
  - `requires_scoring() -> bool` (`:163`);
  - `merge_fruits(Vec<child fruit>) -> Result<Fruit>` (`:167-170`);
  - `collect_segment(&dyn Weight, segment_ord, &SegmentReader)`, which is overridable (`:173-183`).
- `trait SegmentCollector: 'static` (`:296`) with:
  - `collect(doc, score)` (`:302`);
  - `collect_block(&[DocId])`, whose default loops over `collect(doc, 0.0)` (`:309-313`);
  - `harvest()` (`:316`).
- "Segments are not guaranteed to be visited in any specific order" (`:140`). The fruits still arrive at `merge_fruits` in segment-ordinal order (§0).
- `Option<C>` is a collector too (`:223-290`).
- See `examples/custom_collector.rs` in the crate: a std-dev collector over a fast field with `requires_scoring() = false`.

**Behaviour that matters.**
- **`requires_scoring`.** `false` builds the weight with scoring disabled, so no BM25 statistics are fetched and postings are read without scores. `true` makes every match be scored. `requires_scoring() = true` together with a non-overridden `collect_segment` is exactly `library-all`: every `(doc, f32 score)`, nothing pruned.
- **`collect_block`** is used only without deletes and without scoring (§0). Its value is avoiding one virtual call per document in blocks of 64. Collectors that accumulate (counts, `Vec<DocId>`) should override it.
- **Overriding `collect_segment`** hands the collector the `Weight` and the `SegmentReader`. The collector may ignore the weight and read postings, fast fields and the alive bitset directly. Product mode does this, with `AllQuery`, `requires_scoring() = false` and its own BM25F. It must then apply `reader.alive_bitset()` itself.

**Applies to codectx?** Yes. This is the only collector surface that satisfies all the product rules together:
- complete answers (keep everything);
- content-defined order (sort in `merge_fruits` with our comparator on fast-field values);
- live-only statistics (alive bitset);
- f64 BM25F (computed in the collector, not in a tantivy `Scorer`, whose output is f32).

**Metric.** All of them. It is the product mode's MRR@10, S@1, S@10, R@200, mean hits, zero-hit queries, latency and peak memory.

**Variants (existing).**
- `tquery <dir> queries.txt 5 product`, and `library-all`.
- With `--kind function --kind method --path-prefix pkg/api`.
- After `tdelta`.

**Variant (needs a new mode `product-block`).** The product collector currently drives its own posting walk. A variant that keeps the custom fold and comparator but lets tantivy produce candidates measures whether the library's union and intersection beat our own posting merge on candidate generation. It uses a `BooleanQuery` of `TermQuery`s with `requires_scoring() = false`, so candidates arrive through `collect_block` on clean segments. Same hits are required.

---

## 9. Aggregations

**What it is.** An Elasticsearch-style aggregation tree (`src/aggregation/`), run as a collector.

**API.**
- `AggregationCollector::from_aggs(aggs: Aggregations, AggContextParams { limits, tokenizers })` (`src/aggregation/collector.rs:23-36`; `AggContextParams`, `src/aggregation/mod.rs:184-196`). The result is `AggregationResults`.
- `DistributedAggregationCollector` (`collector.rs:46-60`) returns `IntermediateAggregationResults`, for merging across indexes.
- Requests are usually written as JSON and deserialized into `Aggregations`; see `examples/aggregation.rs` and `examples/filter_aggregation.rs` in the crate.

**Common behaviour.**
- `requires_scoring() = false` for both collectors (`collector.rs:79-81`, `:109-111`). **No aggregation can use the BM25 score.**
- Both use `collect_block` when possible (`collector.rs:187`) and the default loop, so deleted documents are excluded (§0).
- **Limits** (`src/aggregation/agg_limits.rs`):
  - `AggregationLimitsGuard::new(memory_limit: Option<u64>, bucket_limit: Option<u32>)` (`:82-89`).
  - Defaults: `DEFAULT_MEMORY_LIMIT = 500_000_000` bytes and `DEFAULT_BUCKET_LIMIT = 65000` (`src/aggregation/collector.rs:15-18`).
  - Memory is an **estimate** shared by all segment collectors of one request, through an `Arc<AtomicU64>` (`agg_limits.rs:22-36`, `:91-98`). Going over fails the request with `AggregationError::MemoryExceeded` (`:105-118`).
  - The bucket limit counts **returned** buckets, "not counting intermediate buckets" (`:32-34`). It is checked once on the final result (`src/aggregation/intermediate_agg_result.rs:157-168`), so it fails the whole request rather than truncating it.

### 9.1 `terms`

- **Request** (`src/aggregation/bucket/term_agg.rs:202-247`): `field`, `size` (default **10**, `:316`), `segment_size` with aliases `shard_size` and `split_size` (default **`size × 10`**, never below `size`, `:318-321`), `min_doc_count` (default **1**, `:329`), `order` (default **`_count` desc**: `OrderTarget::Count`, `src/aggregation/bucket/mod.rs:58-63`; `Order::Desc`, `:45-52`), `show_term_doc_count_error`, `missing`, and include/exclude (CHANGELOG 0.26 #2717).
- Works "only on fast fields of type u64, f64, i64 and text" (`term_agg.rs:76-77`).
- **Approximate across segments:** "Each segment returns up to segment_size results" (`term_agg.rs:109`), and doc counts "may be approximate" (`:85-86`). The segment cut-off is at `:960`.
- **Ties are not deterministic by content.**
  - Segment-level and final sorts by count use `sort_unstable_by_key` on the count alone (`term_agg.rs:948-955`; `intermediate_agg_result.rs:758-763`).
  - The final input comes from an `FxHashMap` (`intermediate_agg_result.rs:694-695`).
  - Equal counts therefore come out in hash-map order, and the `size` cut-off (`:795`) can keep either of two tied terms.
  - Ordering by `_key` uses a stable `sort_by` on the key (`:747-755`), which is deterministic.

### 9.2 `histogram`, `range`, `stats`

- **`histogram`** (`src/aggregation/bucket/histogram/histogram.rs:92`): fixed-interval buckets with `min_doc_count`, bounded by the 65 000 returned-bucket limit.
- **`range`** (`src/aggregation/bucket/range.rs:80`): user-given ranges.
- **`stats`** (`src/aggregation/metric/stats.rs:29`): count, sum, min, max and avg over a numeric fast field, as `f64`.
- All three are exact over live documents.

### 9.3 `top_hits`

- **Request** (`src/aggregation/metric/top_hits.rs:117`): `sort` is a list of `{fast_field: asc|desc}` (`KeyOrder`, `:136`), plus `size`, `from` and `docvalue_fields`.
- "Should not be used as a top-level aggregation", only as a sub-aggregation (`:57-58`).
- It "does not return the actual document addresses", only fast-field values (`:61-62`).
- It cannot sort by score, because aggregations never score.

### 9.4 `cardinality`

- An approximate distinct count: "provides an approximate count" (`src/aggregation/metric/cardinality.rs:70`).
- Since 0.26 it uses Apache DataSketches HLL with `LG_K = 11`, `Hll4` (`:20`, `:313`; CHANGELOG #2837).

### 9.5 Others present in 0.26.2

- **`filter`** (new in 0.26, #2711; `src/aggregation/bucket/filter.rs:170`): a doc count plus sub-aggregations over a query-filtered subset.
- **`composite`** (new in 0.26, #2856; `src/aggregation/bucket/composite/mod.rs:189-207`): "a paginable aggregation", ordered by its composite key, with `size` and an `after` key. This is the only aggregation that can enumerate **every** bucket exactly, page by page, without the `segment_size` approximation.

### 9.6 Applies to codectx?

- **Not on the answer path.** No aggregation sees scores. `terms` counts are approximate and tie order is hash-dependent, and `top_hits` returns no document addresses. None meets "complete answer, deterministic ties, f64 BM25F".
- **For counts beside the answer:**
  - Per-kind counts: `terms` on `kind`. With fewer kinds than `segment_size` the counts are exact, but ties at equal count must be re-sorted by key on our side.
  - Per-folder counts: `FacetCollector` (§6) or `composite`.
  - Both count **raw documents**, not folded entities. A file split into several body chunks counts once per chunk.
- **Limits.** The 500 MB estimate and 65 000-bucket defaults are hard failures, not truncation. That is compatible with "complete or fail", but a large repository can fail a `terms` aggregation on `path_s`. Use `composite` paging or the facet route for folders.

### 9.7 Metric moved

Latency of a count side-query and peak memory. No quality metric moves.

### 9.8 Variant

**Needs a new mode `agg-kind`:**
- `AggregationCollector::from_aggs(json!({"k": {"terms": {"field": "kind", "size": 1000, "segment_size": 1000, "order": {"_key": "asc"}}}}), AggContextParams::new(AggregationLimitsGuard::new(None, None), Default::default()))`.
- Run it in one tuple search `(keep_all, agg)`, against `library-all` followed by a separate aggregation search. 5 runs, warm median and peak memory.
- Correctness gate: each bucket's `doc_count` equals a `Count` over `kind:K`.
- Repeat after `tdelta`: counts must equal the live set.

---

## 10. Summary for codectx

| Need | Library mechanism | Fit |
|---|---|---|
| Complete answer, no cap | custom `Collector` keeping every match (`library-all`, product mode) | yes |
| Complete answer, no cap | `TopDocs::with_limit(Count)` | possible but wasteful (counting pass, 2× buffer per segment) |
| Deterministic, content-based ties | custom `merge_fruits` sort; partly `TopDocs::order_by` with a tuple `(score, path_s, start)` | custom: yes; tuple: f32 ties, no WAND |
| Deleted documents excluded from results | default collect loop / `TopDocs` WAND path check the alive bitset | yes (statistics: see 06) |
| Kind filter | query-side `MUST ConstScoreQuery(TermQuery kind)`; `FilterCollector` only with a u64 kind code | query-side preferred |
| Folder filter | facet field + `TermQuery` (ancestors indexed) | yes, cleaner than regex |
| Per-kind / per-folder counts | `terms` agg (re-sort ties), `FacetCollector`, `composite` | side-query only; raw-document counts |
| f64 BM25F | none: collectors receive `Score = f32` | custom `collect_segment` only |
