# 13 — Scoring: BM25, statistics providers, fieldnorms, block-max upper bounds, tweaks

Version: tantivy 0.26.2. Paths are inside `tantivy-0.26.2/`. Docs: https://docs.rs/tantivy/0.26.2/tantivy/query/struct.Bm25Weight.html, https://docs.rs/tantivy/0.26.2/tantivy/query/trait.Bm25StatisticsProvider.html

## The formula is fixed
- "The relevance score implemented in the tantivy is not configurable" (ARCHITECTURE.md:10). `K1 = 1.2`, `B = 0.75` are private constants (`src/query/bm25.rs:8-9`). These equal codectx's `bm25K1, bm25B` (codectx `internal/search/bm25.go:24`, commit 16dfef6).
- `idf = ln(1 + (N - n + 0.5) / (n + 0.5))` (`src/query/bm25.rs:52-56`), the same formula as codectx `bm25IDF` (`bm25.go:36-47`).
- Score = `idf x (1 + K1) x tf / (tf + K1 x (1 - B + B x dl/avgdl))` with `dl` decoded from the one-byte fieldnorm (`src/query/bm25.rs:58-69, 179-190`). `Score` is `f32` (`src/lib.rs:314`). codectx scores in f64 and quantizes to 1e-6.
- `Bm25Weight` is per field: `for_terms` asserts "All terms must belong to the same field" (`src/query/bm25.rs:100-106`). There is no BM25F (cross-field weighted tf in one saturation). Multi-field queries are sums of per-field BM25 with per-field avgdl. `Bm25Weight::for_one_term(df, N, avgdl)`, `boost_by`, `score(fieldnorm_id, tf)`, `max_score`, `explain` are public (`:80-200`).

## Statistics include deleted documents
- `Bm25StatisticsProvider { total_num_tokens(field), total_num_docs(), doc_freq(term) }` (`src/query/bm25.rs:15-25`). The `Searcher` implementation uses `max_doc` for N (`:40-44`), "including deleted documents" token totals (`src/index/inverted_index_reader.rs:249-253`), and the dictionary's `TermInfo.doc_freq` for df (`src/core/searcher.rs:133-141`, `src/index/inverted_index_reader.rs:276-281`). All three count deleted documents until a merge rewrites the segment (`src/indexer/merger.rs:372-375`). `Searcher::num_docs()` does subtract deletes (`src/core/searcher.rs:123-129`), so the two disagree.
- Verified with the probe from 12 (`../features/probes/src/main.rs`), after deleting one of the two documents containing `title:alpha` and committing (one segment, no merge):
```
before delete: doc_freq=2 num_docs=23
after delete: doc_freq=2 num_docs=22 segments=1
```
and `explain` for the surviving document still reports `"n, number of docs containing this term": 2.0` and `"N, total number of docs": 23.0`.
- Mechanism to fix: implement `Bm25StatisticsProvider` with alive counts and pass it to `Searcher::search_with_statistics_provider(query, collector, &provider)` (`src/core/searcher.rs:193-208`) or `EnableScoring::enabled_from_statistics_provider` (`src/query/query.rs:46`). The alive df per segment is available as `SegmentPostings::doc_freq_given_deletes(&AliveBitSet)`, which is `pub` but "will clone and scan through the posting lists" (`src/postings/segment_postings.rs:32-36`). The provider still has no hook for alive token totals except a scan of a per-document length column (the bench's Rust arm scans `token_count`).

## Fieldnorms
- One byte per (document, field), "using the exact same scheme as Lucene", log-scale ids 0-255 (`src/fieldnorm/mod.rs:9-11`). Lengths 0-40 are exact ("Values up to 40 are encoded unchanged", ARCHITECTURE.md:260); above that, a length is rounded DOWN to the table entry: 41 -> 40, 49 -> 48, 100 -> 96, 1 000 -> 984 (`src/fieldnorm/code.rs:6-11, 13+`). Body lengths are routinely above 40, so library BM25 length normalization is approximate.
- With fieldnorms off, scoring uses a constant norm of 1 for every document (`src/query/term_query/term_weight.rs:209-217`).

## Block-max (block-WAND) upper bounds are not guaranteed exact
`TermScorer::block_max_score` (`src/query/term_query/term_scorer.rs:58-75`): "In some rare case, the result may not be exact. In this case a lower value is returned, (and may lead us to return a lesser document). At index time, we store the (fieldnorm_id, term frequency) pair that maximizes the score assuming the average fieldnorm computed on this segment ... (The result is on the other hand guaranteed to be correct if there is only one segment)." The pair is chosen at serialization (`src/postings/serializer.rs:404-428`). So a TopDocs answer from a multi-segment index can, rarely, miss a document that belongs in the top k. 0.26 also added lazy scorers that stop evaluation once a document cannot reach the threshold (CHANGELOG.md:55).

## Custom scoring hooks
- `TopDocs::tweak_score(|segment_reader| |doc, score| new_score)` (`src/collector/top_score_collector.rs:426`; `examples/faceted_search_with_tweaked_score.rs`). It runs on the BM25 score after it is computed, per document, and disables block-WAND because the sort key is no longer the raw score.
- `TopDocs::order_by(|segment_reader| |doc| key)` or any `SortKeyComputer` (`:322`), including tuples, see 14.
- Full control: a custom `Collector` that ignores the score (`requires_scoring() == false`) and reads postings itself through `SegmentReader::inverted_index(field)?.read_postings(term, option)` (`src/index/inverted_index_reader.rs:265`), as the bench's Rust arm's product mode does, or a custom `Query`/`Weight`/`Scorer` (ARCHITECTURE.md, "query/").
- `EnableScoring::Disabled` skips scoring and lets a term matching every document become an `AllScorer` (`src/query/query.rs:28-33`; `term_weight.rs:191-197`).
- `explain()` returns an `Explanation` tree with `to_pretty_json()` (`src/query/explanation.rs:53-79`).

## What it means for codectx
- Library BM25 differs from codectx in four ways: per-field sum, not BM25F; f32; fieldnorm quantization above 40 tokens; deleted documents in N, df and avgdl. Only the last is fixable through the library API (a statistics provider); the others require product mode's own scoring.
- Variant "library + alive statistics": the library-mode query with a `Bm25StatisticsProvider` returning alive N, alive df (sum of `doc_freq_given_deletes`) and alive token totals; run after the delta; compare the page with library mode and with product mode, and time the df scan.
- Variant "fieldnorms off": see 10.
- Variant "single segment": force one segment (NoMergePolicy + final merge) and compare library TopDocs pages with the all-docs page; the block-max note says they must then agree.
