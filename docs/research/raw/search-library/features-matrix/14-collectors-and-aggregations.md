# 14 — Collectors and aggregations

Version: tantivy 0.26.2. Paths are inside `tantivy-0.26.2/`. Docs: https://docs.rs/tantivy/0.26.2/tantivy/collector/index.html, https://docs.rs/tantivy/0.26.2/tantivy/aggregation/index.html

## The Collector contract
`Collector { for_segment, requires_scoring, merge_fruits, collect_segment }` and `SegmentCollector { collect(doc, score), collect_block(&[DocId]), harvest }` (`src/collector/mod.rs:156-235`). The default `collect_segment` skips deleted documents through the alive bitset (`:179-200`). Returning `false` from `requires_scoring` lets the engine skip BM25 and feed doc blocks (`collect_block`). Several collectors run in one pass as a tuple, e.g. `(Count, TopDocs::with_limit(2).order_by_score())` (`src/collector/mod.rs:24-60`), or through `MultiCollector` (`src/collector/multi_collector.rs`).

## TopDocs
- `TopDocs::with_limit(n)` panics on 0 (`src/collector/top_score_collector.rs:93-96`); `and_offset(k)` (`:139`); `for_doc_range(a..b)` (`:77`). There is no unlimited mode: the limit sizes a `TopNComputer` buffer of `2 x n` (`:608-616`).
- Orders: `order_by_score()` (`:226`), `order_by_fast_field::<T>(field, Order)` (`:299`), `order_by_string_fast_field(field, Order)` (`:312`), `order_by_u64_field` (`:217`), `order_by(sort_key_computer)` (`:322`), `tweak_score(f)` (`:426`).
- Ties: "The TopNComputer will tiebreak by using ascending `D` (DocId or DocAddress) ... regardless of the `Comparator`" (`:503-510`; `compare_for_top_k` `:589-597`). A `DocAddress` is (segment ordinal, doc id), which depends on thread count, flush points and merges, so equal scores are ordered deterministically for one index but not across two builds of the same corpus.
- Sort keys compose: 2-, 3- and 4-tuples of `SortKeyComputer` (`src/collector/sort_key/sort_key_computer.rs:141, 325, 383`), `(computer, Order)` (`src/collector/sort_key/order.rs:443`), with built-ins `SortBySimilarityScore`, `SortByStaticFastValue`, `SortByString`, `SortByBytes`, `SortByErasedType` (`src/collector/sort_key/mod.rs:9-15`). "When using a tuple as the sorting key, the sort key is evaluated in a lazy manner" (`sort_key_computer.rs:36`). So `TopDocs::with_limit(200).order_by(((SortBySimilarityScore, Order::Desc), (SortByString::for_field("id"), Order::Asc)))` gives a build-independent tie-break, at the cost of leaving the block-WAND path (only a bare score key overrides `collect_segment_top_k` with pruning, `src/collector/sort_key/sort_by_score.rs:27-58`).
  Verified with a probe on tantivy =0.26.2 (`../features/probes/src/bin/tiebreak.rs`; three documents with identical text, ids added in the order c, a, b):
  ```
  order_by_score: [0, 1, 2]
  tuple (score desc, id asc): [(1, Some("a")), (2, Some("b")), (0, Some("c"))]
  ```
  `order_by_score` breaks the tie on doc id (add order); the tuple key breaks it on `id`. The collector was
  `TopDocs::with_limit(3).order_by(((SortBySimilarityScore, Order::Desc), (SortByString::for_field("id"), Order::Asc)))`.
- Score-ordered TopDocs prunes with block-WAND and drops deleted documents inside the pruning callback (`sort_by_score.rs:38-48`).

## Other collectors
| Collector | Fruit | Source |
|---|---|---|
| `Count` | `usize` of live matches | `src/collector/count_collector.rs:39-45` |
| `DocSetCollector` | `HashSet<DocAddress>`: every match, unordered | `src/collector/docset_collector.rs:12` |
| `FilterCollector::new(field_name, predicate, collector)` / `BytesFilterCollector` | wraps a collector, keeps docs whose fast-field value passes | `src/collector/filter_collector_wrapper.rs:86, 265` |
| `FacetCollector` | facet counts under a root | `src/collector/facet_collector.rs` |
| `HistogramCollector` | counts per bucket of a fast field | `src/collector/histogram_collector.rs` |
| `MultiCollector` | dynamic set of collectors | `src/collector/multi_collector.rs` |

`Query::count(searcher)` uses `Weight::count`, which for a term on a segment without deletes reads the doc_freq without iterating (`src/query/query.rs:145`; `src/query/term_query/term_weight.rs:56-66`). The `Count` collector itself iterates.

## Aggregations
- `AggregationCollector::from_aggs(Aggregations, AggContextParams)` (`src/aggregation/collector.rs:23-56`), requests in Elasticsearch JSON (`examples/aggregation.rs`).
- Buckets: terms (with include/exclude, new in 0.26), range, histogram, date_histogram, filter (0.26), composite (0.26) (`src/aggregation/bucket/`; CHANGELOG.md:29-34). Metrics: avg, cardinality, count, extended_stats, max, min, percentiles, stats, sum, top_hits (`src/aggregation/metric/`).
- Limits: `DEFAULT_BUCKET_LIMIT = 65000`, `DEFAULT_MEMORY_LIMIT = 500_000_000` bytes (`src/aggregation/collector.rs:15, 18`), set through `AggregationLimitsGuard::new(memory, buckets)` (`src/aggregation/agg_limits.rs:82`). A terms aggregation returns the top 10 terms unless `size` is set, and fetches `10 x size` per segment (`src/aggregation/bucket/term_agg.rs:204-218, 316`).

## What it means for codectx
- "No top-k cap" means every match must be scored and ordered. In the library that is a custom collector that keeps every (doc, score) and sorts after merge (the bench's Rust arm's `library-all` and `product` modes), or `DocSetCollector` + a second pass. TopDocs with a huge limit is the same work with a 2n buffer.
- `(Count, TopDocs)` in one search halves the work the bench's Rust arm's library mode now does in two searches (it runs `Count` separately). Variant: tuple collector vs two searches; compare warm latency.
- Kind and path filters: `FilterCollector` on the `kind`/`path_s` fast field vs the current MUST `ConstScoreQuery(0)` clause; compare filtered latency. Both leave df and N unfiltered, as codectx requires.
- Tuple `order_by` with an id tie-break is the library-native way to get codectx-style deterministic ties; variant against `order_by_score` to price the lost pruning.
- Aggregations are "no" for ranking; "maybe" for a per-kind result count (a terms aggregation on `kind` with an explicit `size`), which codectx does not serve today.
