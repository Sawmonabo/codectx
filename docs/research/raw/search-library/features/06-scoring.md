# 06 — Scoring in tantivy 0.26.2

**Scope.** How tantivy computes a score, which statistics feed it, how scores from several fields combine, when block-max WAND prunes, and what `tweak_score` and `explain()` give.

**Sources.** The crate source of `tantivy = "=0.26.2"` is the source of truth. Citations read `tantivy-0.26.2/src/<path>:<line>`. Two pages were cross-checked on docs.rs: [TopDocs](https://docs.rs/tantivy/0.26.2/tantivy/collector/struct.TopDocs.html) and [Bm25Weight](https://docs.rs/tantivy/0.26.2/tantivy/query/struct.Bm25Weight.html). Version notes come from the crate's own `CHANGELOG.md`.

**What codectx scores today (the target).** codectx uses BM25F:
- Per term, it sums term frequency across the five columns with weights name 5, qualified_name 5, signature 2, path 2 and body 1, *before* saturation.
- k1 = 1.2 and b = 0.75.
- idf = ln(1 + (N − df + 0.5)/(df + 0.5)). df counts documents that have the term in *any* of the five columns, live documents only. N counts live documents only.
- dl is the document's `token_count`. avgdl is the mean `token_count` over live documents.
- The score is f64, quantized to round(score·1e6) as an int64. A total order on integers and exact strings sorts the results.

**Benchmark harness referred to below.**
- `tquery <idx> <queries> <runs> library|library-all|product [--kind K]… [--path-prefix P]… [--search-threads N] [--phase NAME]`
- `tcompare <a.jsonl> <b.jsonl>` reports both hit counts, plus top-200 and top-10 overlap.
- `teval <qrels> <queries> <results.jsonl>…` reports MRR@10, S@1, S@10, R@200, mean hits, zero-hit queries and missing queries.
- `tdelta <docs> <idx> [unit]` writes phases `delta-put`, `delta-commit`, `delta` and `delta-merge`.
- `tqrels <docs> <outdir> <seed>` writes the qrels.
- Every run goes under `systemd-run --user --scope -p MemoryMax=4G`.
- `library` is a nested `BooleanQuery` (term SHOULD → field SHOULD → `BoostQuery(TermQuery|PhraseQuery, 5/5/2/2/1)`) collected by `TopDocs::with_limit(200).order_by_score()`. `library-all` runs the same query but a custom collector keeps every `(doc, score)`. `product` is codectx's exact BM25F, computed in a custom collector.

## Verdict summary

| Capability | Does it meet a codectx rule? | Why |
|---|---|---|
| `Bm25Weight` formula | Same formula shape, **not the same numbers** | Statistics are per field, dl is quantized, scores are f32, and each field saturates separately |
| Fieldnorm encoding | No | Lengths are exact only from 0 to 40. Above that, rounding down loses up to 1/9 of the value |
| Per-field statistics | No | df, avgdl and tf are per (field, term). There is no union df and no shared dl |
| `Bm25StatisticsProvider` | Partly | It can supply live N and live df, but only per field, as f32. It does not change how the per-field scores combine |
| Deleted documents in statistics | No by default | `total_num_docs` uses `max_doc`. `doc_freq` and token totals include deleted documents until a merge |
| f32 `Score` | No | A `Weight`/`Scorer` cannot carry an f64 value. Summation order can change the last bit |
| Multi-field combination | No BM25F | `BooleanQuery` sums per-field BM25 values. `DisjunctionMaxQuery` takes the max plus a tie_breaker |
| Block-max WAND | Speed only, and only for flat pure-term unions in `TopDocs::order_by_score` | Turned off by MUST, MUST_NOT, phrases, nesting, filters and every other collector. It conflicts with the no-top-k rule anyway |
| `tweak_score` | Re-ranking hook only | It runs per document with an f32 input. It turns pruning off |
| `custom_score` | **Does not exist in 0.26.2** | It is absent from source and from docs.rs |
| `explain()` | Diagnostic only | f32 values, and always the searcher's own statistics |

---

## 1. `Bm25Weight` and the exact formula

**What it is.** The per-term (or per-phrase) BM25 weight. It holds idf·(1+k1), a 256-entry table of the length-normalisation term, and the average fieldnorm.

**API.** These match the docs.rs Bm25Weight page:
- `Bm25Weight::for_terms(statistics: &dyn Bm25StatisticsProvider, terms: &[Term]) -> Result<Bm25Weight>`
- `for_one_term(term_doc_freq: u64, total_num_docs: u64, avg_fieldnorm: Score)` and `for_one_term_without_explain(..)`
- `boost_by(&self, boost: Score)`
- `score(&self, fieldnorm_id: u8, term_freq: u32) -> Score`
- `max_score(&self)`
- `explain(&self, fieldnorm_id: u8, term_freq: u32) -> Explanation`

**Behaviour.**
- The constants are f32: `const K1: Score = 1.2;` and `const B: Score = 0.75;` (`tantivy-0.26.2/src/query/bm25.rs:8-9`).
- idf (`src/query/bm25.rs:52-56`):
  ```rust
  assert!(doc_count >= doc_freq, "{doc_count} >= {doc_freq}");
  let x = ((doc_count - doc_freq) as Score + 0.5) / (doc_freq as Score + 0.5);
  (1.0 + x).ln()
  ```
  This is the same formula as codectx's `bm25IDF`, computed in f32. The `assert!` is a hard panic, not a `debug_assert!`.
- Average fieldnorm: `total_num_tokens(field) as Score / total_num_docs() as Score` (`src/query/bm25.rs:109-111`).
- The weight factor is `weight = idf * (1.0 + K1)` (`src/query/bm25.rs:159`, `:168`). A boost multiplies it: `weight: self.weight * boost` (`src/query/bm25.rs:88`).
- The tf part is looked up in a table (`src/query/bm25.rs:58-69`, `:189-193`):
  ```rust
  fn cached_tf_component(fieldnorm: u32, average_fieldnorm: Score) -> Score {
      K1 * (1.0 - B + B * fieldnorm as Score / average_fieldnorm)
  }
  // compute_tf_cache: for each fieldnorm_id in 0..256, cache[id] = cached_tf_component(id_to_fieldnorm(id), avg)
  let norm = self.cache[fieldnorm_id as usize];
  term_freq / (term_freq + norm)
  ```
- The full score is `score = idf·(1+k1)·tf/(tf + k1·(1 − b + b·dl/avgdl))·boost`, where dl is the *decoded quantized* fieldnorm (`src/query/bm25.rs:179-181`). The 256-entry table is rebuilt once per `Bm25Weight`, so per-document scoring is one table lookup and one division.
- **Phrases.** For more than one term, idf is the **sum** of the member terms' idfs (`src/query/bm25.rs:121-127`), and tf is the phrase match count (`src/query/phrase_query/phrase_scorer.rs:578-586`). codectx scores a phrase from its own adjacency count, with a phrase-level df.
- `max_score()` is `self.score(255u8, 2_013_265_944)` (`src/query/bm25.rs:184-186`). It is the upper bound WAND uses.

**Applies to codectx?** Not as the ranking. The shape matches one *field* of BM25, but codectx saturates the *weighted sum* of tfs across columns once. The constants and the idf formula match, so a reference implementation can reuse them as a check. The exact tantivy score cannot equal codectx's for four reasons: per-field df and avgdl, quantized dl, f32, and per-field saturation.

**Metric moved.** Quality: MRR@10, S@1, S@10 (and R@200 indirectly) when `library` or `library-all` is compared with `product`.

**Benchmark variant.** This already exists:
```
tquery <idx> queries.txt 5 library-all  --phase lib-all
tquery <idx> queries.txt 5 product      --phase product
tcompare results/<lib-all>.jsonl results/<product>.jsonl     # top-10 and top-200 overlap
teval qrels-name.tsv queries-name.txt results/<lib-all>.jsonl results/<product>.jsonl
```
Repeat with qrels-doc and qrels-subword.

## 2. Fieldnorm encoding

**What it is.** A one-byte, per-(document, field) code for the field's token count. The module docs describe it as "on one byte with some precision loss, using the exact same scheme as Lucene" (`tantivy-0.26.2/src/fieldnorm/mod.rs:9-11`).

**API.**
- `FieldNormReader::fieldnorm_id(doc) -> u8` and `fieldnorm(doc) -> u32` (`src/fieldnorm/reader.rs:116`, `:128`).
- `FieldNormReader::fieldnorm_to_id(u32) -> u8` (`src/fieldnorm/reader.rs:147`).
- Turned off per field with `TextFieldIndexing::set_fieldnorms(false)`.

**Behaviour.**
- The table `FIELD_NORMS_TABLE: [u32; 256]` is at `src/fieldnorm/code.rs:13-270`.
- **It is exact for lengths 0 through 40.** id 40 → 40 (`src/fieldnorm/code.rs:54`), then id 41 → 42 (`:55`). The test asserts `fieldnorm_to_id(41) == 40` (`src/fieldnorm/code.rs:284`).
- After that the scale is log: id 63–66 decode to 144, 152, 168 and 184, and the largest value is 2 013 265 944 (`src/fieldnorm/code.rs:269`). The decoding rule is spelled out in the test at `src/fieldnorm/code.rs:308-327`.
- **Encoding rounds down**: `binary_search(&fieldnorm).unwrap_or_else(|idx| idx - 1)` (`src/fieldnorm/code.rs:7-11`).
- I computed over the table: a value can lose up to 1/9 of itself (worst step ≈ 11.1%). Near 1 000 tokens the step is 64 (984 → 1 048).
- **What is recorded** is the field's token count for the document, `indexing_position.num_tokens` (`src/indexer/segment_writer.rs:220`). With several values in one field, the counts add up.
- **With fieldnorms off**, the scorer uses `FieldNormReader::constant(max_doc, 1)` (`src/query/term_query/term_weight.rs:209-219`). avgdl still comes from `total_num_tokens`, so every document then looks one token long against the real average.
- **Merges with deletes** *estimate* the merged segment's token total from the quantized fieldnorms of live documents (`src/indexer/merger.rs:29-69`, used at `:333`). After such a merge, avgdl is approximate as well.

**Applies to codectx?**
- name, qualified_name, signature and path are almost always 40 tokens or fewer, so their fieldnorms are exact.
- body is often longer, so its dl is rounded down by up to about 11%.
- codectx's dl is one per-document `token_count`, not five per-field lengths. Fieldnorms cannot express it. `product` mode reads a `token_count` fast field instead.
- Turning fieldnorms off saves one byte per document per field, but breaks tantivy's own length normalisation.

**Metric moved.** Quality (MRR@10, S@10) for `library`/`library-all`. Disk bytes: 5 bytes per document across the five text fields.

**Benchmark variant.** Needs a new index option: `tindex … --no-fieldnorms body`, then `tquery <idx> queries.txt 5 library-all` and `tcompare` against the default index. This shows how much of the library-to-product gap comes from body length normalisation. Disk bytes come from the index phase's size output.

## 3. Per-field statistics

**What it is.** Every statistic BM25 uses is per field. df is per `Term`, which includes its field. Token totals are per field. N is global.

**API.**
- `Searcher::doc_freq(&Term) -> Result<u64>` (`tantivy-0.26.2/src/core/searcher.rs:133-141`)
- `InvertedIndexReader::total_num_tokens()` (`src/index/inverted_index_reader.rs:249-253`)
- `InvertedIndexReader::doc_freq(&Term)` (`src/index/inverted_index_reader.rs:276-281`)
- `Searcher::num_docs()` (`src/core/searcher.rs:123-129`)

**Behaviour.**
- `Bm25Weight::for_terms` requires every term to be in one field: `assert_eq!(term.field(), field, "All terms must belong to the same field.")` (`src/query/bm25.rs:100-107`). It then reads `total_num_tokens(field)` and `doc_freq(term)` for that field only (`:109-124`).
- Nothing in the crate computes a df over a union of fields.

**Applies to codectx?** It conflicts with codectx's rule that df is counted over the union of the five columns. In tantivy, a term common in body but rare in name gets a high idf in the name field and a low idf in the body field. codectx gives it one idf.

**Metric moved.** Quality: top-10 overlap between `library-all` and `product` in `tcompare`, and MRR@10/S@1 in `teval`.

**Benchmark variant.** Same as §1. To isolate the df effect, use the new `library-livestats` mode in §4 with a union-df provider.

## 4. `Bm25StatisticsProvider` and `EnableScoring`

**What it is.** The hook for replacing the statistics. It was added in 0.20 as "Make BM25 scoring more flexible" (`tantivy-0.26.2/CHANGELOG.md:339`).

**API.**
```rust
pub trait Bm25StatisticsProvider {                       // src/query/bm25.rs:15-25
    fn total_num_tokens(&self, field: Field) -> crate::Result<u64>;
    fn total_num_docs(&self) -> crate::Result<u64>;
    fn doc_freq(&self, term: &Term) -> crate::Result<u64>;
}
pub enum EnableScoring<'a> {                             // src/query/query.rs:14-34
    Enabled { searcher: &'a Searcher, statistics_provider: &'a dyn Bm25StatisticsProvider },
    Disabled { schema: &'a Schema, searcher_opt: Option<&'a Searcher> },
}
```
- Constructors: `enabled_from_searcher`, `enabled_from_statistics_provider(provider, searcher)`, `disabled_from_searcher` and `disabled_from_schema` (`src/query/query.rs:38-70`).
- Entry points: `Searcher::search_with_statistics_provider(query, collector, provider)` (`src/core/searcher.rs:193`) and `search_with_executor(query, collector, executor, enabled_scoring)` (`:220`).

**Behaviour.**
- `Searcher::search` chooses `Enabled` only if `collector.requires_scoring()`, and `Disabled` otherwise (`src/core/searcher.rs:198-203`).
- With scoring disabled:
  - a `TermQuery` builds `Bm25Weight::new(Explanation::new("<no score>", 1.0), 1.0)`;
  - it reads postings as `IndexRecordOption::Basic` (`src/query/term_query/term_query.rs:100-114`);
  - a posting list that covers every document becomes an `AllScorer` (`src/query/term_query/term_weight.rs:193-197`).
- **Which queries use the provider.** `TermQuery` (`src/query/term_query/term_query.rs:100-104`), `PhraseQuery` (`src/query/phrase_query/phrase_query.rs:122-124`) and `RegexPhraseQuery` (`src/query/phrase_query/regex_phrase_query.rs:147-149`) do.
- **Which ignore it.** `PhrasePrefixQuery` matches `EnableScoring::Enabled { searcher, .. }` and calls `Bm25Weight::for_terms(searcher, …)` (`src/query/phrase_prefix_query/phrase_prefix_query.rs:113-117`). MoreLikeThis also takes `searcher` (`src/query/more_like_this/query.rs:49`).
- `Query::explain` always uses `EnableScoring::enabled_from_searcher(searcher)` (`src/query/query.rs:137-142`), so it never sees a custom provider.
- **Mixing live N with stock df panics.** A provider that returns live N while letting `doc_freq` fall through to the stock count, which includes deleted documents, can trip `assert!(doc_count >= doc_freq)` (`src/query/bm25.rs:53`). Both must be live, or neither.

**Applies to codectx?** Partly.
- A provider can return live N (`Searcher::num_docs`, which sums `segment_reader.num_docs()`).
- It can return a live df by walking postings against the alive bitset.
- For a *union* df, `doc_freq(term)` can ignore the field and count the union of the same text in all five fields.
- It cannot change dl (still the per-field fieldnorm), per-field saturation or f32. So it narrows the gap without closing it.
- The provider's cost is real. A live df means reading the term's postings in every segment, on every query, before scoring starts.

**Metric moved.**
- Quality after a delta: top-10 overlap with `product` in `tcompare`, and MRR@10.
- Latency: the warm median rises by the cost of the df walk.

**Benchmark variant.** Needs a new mode, `library-livestats`. It is `library-all`, but searched through `search_with_statistics_provider` with:
- `total_num_docs` = Σ `num_docs()`;
- `doc_freq` = the number of alive documents in the union of the five fields' postings for the term text;
- `total_num_tokens(field)` = the sum over alive documents of that field's exact token count, which needs a per-field count fast field.

Run it on the index after `tdelta <docs> <idx> unit`, at phases `delta` and `delta-merge`:
```
tquery <idx> queries.txt 5 library-livestats --phase delta-livestats
tcompare results/<delta-livestats>.jsonl results/<delta-product>.jsonl
```

## 5. Do doc_freq and num_docs count deleted documents?

**Answer: yes, through the default provider, until a merge removes them.**
- `total_num_docs` sums `segment_reader.max_doc()` (`tantivy-0.26.2/src/query/bm25.rs:38-45`, the sum at line 42). `max_doc` includes deleted documents.
- `doc_freq` sums `term_info.doc_freq` per segment (`src/core/searcher.rs:133-141`, `src/index/inverted_index_reader.rs:276-281`). The term dictionary is written once per segment and never edited on delete.
- `total_num_tokens` is documented as "Returns the total number of tokens recorded for all documents (including deleted documents)." (`src/index/inverted_index_reader.rs:249-251`).
- By contrast, `Searcher::num_docs()` sums `segment_reader.num_docs()`, which counts live documents only (`src/core/searcher.rs:123-129`). It is *not* what BM25 uses.
- Deleted documents never *match*:
  - The default collect path filters on `alive_bitset.is_alive(doc)` (`src/collector/mod.rs:186-221`).
  - The top-k path skips `alive_bitset.is_deleted(doc)` (`src/collector/sort_key/sort_by_score.rs:38-48`).
  - `Weight::count` uses the alive bitset (`src/query/weight.rs:78-85`, `src/query/term_query/term_weight.rs:56-65`).
- A merge writes only live documents. After a merge, df and max_doc are live again, but the merged token total is an estimate from quantized fieldnorms (§2, `src/indexer/merger.rs:29-69`).

**Applies to codectx?** It directly conflicts with the rule "exact df excluding hidden documents". Soft-deleted documents that codectx keeps would also stay in tantivy's statistics until a merge. With the stock provider, scores after a delta depend on the merge history, so a delta index and a fresh index of the same tree score differently.

**Metric moved.** Quality drift after a delta: `tcompare` top-10/top-200 overlap between the `delta` and `delta-merge` phases, and against `product`.

**Benchmark variant.** This exists:
```
tdelta <docs> <idx> unit
tquery <idx> queries.txt 5 library-all --phase delta-lib-all
tquery <idx> queries.txt 5 product     --phase delta-product
tcompare results/<delta-lib-all>.jsonl results/<delta-product>.jsonl
```
Repeat at `delta-merge`. Then compare `library-all` on the delta index with `library-all` on a fresh index of the same final corpus. Any change in top-10 is statistics drift.

## 6. f32 scores

**What it is.** `pub type Score = f32;`, documented as "modelled internally as a `f32`" (`tantivy-0.26.2/src/lib.rs:314`). Every `Scorer::score`, `Explanation` value and collector callback carries f32.

**Behaviour that affects ordering.**
- Sums are f32. `SumCombiner` does `self.score += scorer.score()` (`src/query/score_combiner.rs:44-57`). An intersection adds `left + right + others` (`src/query/intersection.rs:235-239`).
- **The order of additions differs by path.**
  - `BufferedUnionScorer::refill` updates the combiners in the order of the scorer vector, which `unordered_drain_filter` reorders as postings run out (`src/query/union/buffered_union.rs:58-81`).
  - block-WAND sums the pivot scorers in doc-sorted order (`src/query/boolean_query/block_wand.rs:202-204`).
  - Float addition is not associative, so one document can get scores that differ in the last bit between `TopDocs::order_by_score` (pruned path) and a full collector (`library-all`). This is an inference from the code, not a measurement.
  - Scorer exhaustion order depends on doc ids, so a delta index and a fresh index of the same content can round differently too.
- Ties in `TopDocs` fall to the lower `DocAddress`. `compare_for_top_k` does `.then_with(|| lhs.doc.cmp(&rhs.doc))` (`src/collector/top_score_collector.rs:589-597`). A doc address is a segment ordinal plus a doc id, so it is a fact about the index layout, not the content.
- `NaturalComparator` maps an incomparable pair (NaN) to `Ordering::Equal` (`src/collector/sort_key/order.rs:84-89`).

**Applies to codectx?** It conflicts with "f64 BM25F, quantized to 1e-6, ordered by integers". f32 has about 7 significant digits. Rounding the f32 score to 1e-6 does not reproduce codectx's integers once scores go above about 10, and two values that are close in f64 can collapse into one f32. The only way to keep f64 is to compute the score outside `Scorer`, in a custom `Collector` whose `Fruit` carries f64 or i64. This is what `product` mode does.

**Metric moved.** Determinism: identical top-200 between delta and fresh indexes (`tcompare` overlap must be 200/200 *and* in order). Also top-10 overlap with `product`.

**Benchmark variant.** This exists. Run `tcompare` on `library` against `library-all` for the same index. Any difference beyond the documented tie rule is f32 path divergence or pruning (§8). Then run `tcompare` on `library-all` for a delta index against a fresh index.

## 7. How multi-field scores combine, and whether BM25F is expressible

**API and behaviour.**
- `BooleanQuery` builds a `BooleanWeight` with `SumCombiner` (`tantivy-0.26.2/src/query/boolean_query/boolean_query.rs:157-169`). A document's score is the sum of the per-field BM25 values, each saturated on its own field.
- `DisjunctionMaxQuery::with_tie_breaker(disjuncts, tie_breaker)` builds a `BooleanWeight` with `DisjunctionMaxCombiner` (`src/query/disjunction_max_query.rs:93-105`). Its score is `self.max + (self.sum - self.max) * self.tie_breaker` (`src/query/score_combiner.rs:91-94`).
- `BoostQuery::new(query, boost)` multiplies through: `self.weight.scorer(reader, boost * self.boost)`, which reaches `Bm25Weight::boost_by` (`src/query/boost_query.rs:70-72`, `src/query/bm25.rs:82-92`).
- `ConstScoreQuery::new(query, score)` gives `ConstScorer::new(inner, boost * self.score)` (`src/query/const_score_query.rs:68`).
- Fuzzy and regex queries (`AutomatonWeight`) are constant-score: `ConstScorer::new(doc_bitset, boost)` (`src/query/automaton_weight.rs:109`). So are `RangeQuery` (`src/query/range_query/range_query.rs:245`), `ExistsQuery` (`src/query/exist_query.rs:165`) and `AllQuery`.
- **Finding, from reading the code; not yet run.** A `DisjunctionMaxQuery` whose disjuncts are all `TermQuery`s with frequencies (≥2 of them) becomes a `SpecializedScorer::TermUnion` (`src/query/boolean_query/boolean_weight.rs:58-69`). Under `TopDocs::order_by_score`, `BooleanWeight::for_each_pruning` hands such a union to `block_wand`, which **sums** the term scores (`src/query/boolean_query/boolean_weight.rs:516-531`, `src/query/boolean_query/block_wand.rs:202-204`). It never consults `DisjunctionMaxCombiner`. The doc example only avoids this because it uses `IndexRecordOption::Basic`, which is not `ReadFreq` (`src/query/disjunction_max_query.rs:44-51`). Every other collector goes through `for_each`, which uses the combiner. So the same DisMax query may score differently under `order_by_score` than under `tweak_score(|_, s| s)`.

**Can BM25F be expressed?** Not with stock queries. There are three ways to get close:
1. **One combined field** in which each column's tokens are repeated once per unit of weight. tf becomes Σ w·tf and df becomes a union df. But dl becomes Σ w·len instead of `token_count`, it is quantized above 40, statistics still include deleted documents, the score is f32, phrase positions break across copies, and the index grows.
2. **A custom `Query`/`Weight`/`Scorer`.** The traits are public (`src/query/weight.rs:66-133`). It reads five postings per term, sums w·tf, takes dl from a `token_count` fast field, and gets idf from a live union df. This is exact BM25F, but it still returns f32 and can never use block-WAND, which requires `TermScorer`.
3. **A custom `Collector` that overrides `collect_segment`** and scores outside `Score` (this is `product` mode). This is exact f64 BM25F under the total order. The harness has verified it against a reference implementation.

**Applies to codectx?** Only option 3 meets the scoring rule. `BooleanQuery`-sum is what `library` measures, as "what a typical tantivy application ranks".

**Metric moved.** Quality: MRR@10, S@1 and S@10 for each combination rule against qrels, and top-10 overlap with `product`.

**Benchmark variants.**
- Exists: `library-all` (sum) against `product` (BM25F).
- Needs a new mode, `library-dismax`: per term, `DisjunctionMaxQuery::with_tie_breaker(five BoostQuery(TermQuery), t)` for t ∈ {0.0, 0.3, 0.7}, collected the `library-all` way. Score each with `teval` on all three qrels.
- Correctness probe for the finding above: on one index, run `library-dismax` with `TopDocs::with_limit(200).order_by_score()` and again with `TopDocs::with_limit(200).tweak_score(|_| |_, s| s)`. Then `tcompare` them. If the scores differ, the summing finding is confirmed.

## 8. Block-max WAND: when it applies and what turns it off

**What it is.** Dynamic pruning that skips 128-doc posting blocks whose stored maximum score cannot beat the current top-k threshold. The doc comment cites "Faster Top-k Document Retrieval Using Block-Max Indexes" (`tantivy-0.26.2/src/query/boolean_query/block_wand.rs:145-148`).

**API.** Internal. It is reached only through `Weight::for_each_pruning(threshold, reader, callback)` (`src/query/weight.rs:123-132`). In 0.26.2 the only caller is `SortBySimilarityScore::collect_segment_top_k`, which is what `TopDocs::…order_by_score()` uses (`src/collector/sort_key/sort_by_score.rs:27-61`, `src/collector/top_score_collector.rs:226-228`).

**When it applies. All of these must hold:**
1. The collector is `TopDocs::with_limit(n)[.and_offset(o)].order_by_score()`.
   - Every other sort key, including `tweak_score`, `order_by_fast_field`, `order_by_string_fast_field` and `order_by`, uses the default `collect_segment_top_k` → `default_collect_segment_impl` → `weight.for_each` with no threshold (`src/collector/sort_key/sort_key_computer.rs:117-135`, `src/collector/mod.rs:186-221`).
   - `Count` has `requires_scoring() == false` (`src/collector/count_collector.rs:51-53`).
   - Tuple collectors such as `(TopDocs, Count)`, `MultiCollector` and any custom collector do not override `collect_segment` (only `src/collector/mod.rs:173` defines it), so they also take the full path.
2. The query is either:
   - a single `TermQuery` whose postings have frequencies, which takes `block_wand_single_scorer` (`src/query/term_query/term_weight.rs:118-141`); or
   - a `BooleanWeight` whose `complex_scorer` returns `SpecializedScorer::TermUnion`.
3. `TermUnion` requires all of the following (`src/query/boolean_query/boolean_weight.rs:43-83`, `:222-296`):
   - no MUST clause;
   - no MUST_NOT clause, because any exclusion wraps the union in `Exclude` → `Other`;
   - `minimum_number_should_match ≤ 1`, because ≥2 builds a `Disjunction` (`:266-296`);
   - at least two SHOULD scorers after empty and all-match scorers are removed;
   - **every** SHOULD scorer is a `TermScorer` (`scorer.is::<TermScorer>()`, `:58`);
   - every one has `FreqReadingOption::ReadFreq`: "Block wand is only available if we read frequencies." (`:62-68`).

   `BoostQuery` keeps eligibility, because `BoostWeight::scorer` returns the inner `TermScorer` with the boost folded in (`src/query/boost_query.rs:70-72`).

**What turns it off.**
- Any MUST clause, including a MUST `ConstScoreQuery` filter for kind or folder.
- Any MUST_NOT clause.
- `minimum_number_should_match ≥ 2`.
- A `PhraseQuery`, `PhrasePrefixQuery`, regex, fuzzy, range or const-score clause.
- **Nesting.** A `BooleanQuery` clause gives a `BufferedUnionScorer`, not a `TermScorer`. The harness's `library` query is SHOULD(term) → SHOULD(field), so the outer union sees `BufferedUnionScorer`s and falls back to `for_each_pruning_scorer`. That fallback scores every document and compares each with the threshold (`src/query/weight.rs:47-60`). In practice `library` almost never block-WANDs.
- Fields indexed without frequencies (`IndexRecordOption::Basic`).
- Any collector other than `order_by_score`.
- The offset does **not** turn it off. `k = self.doc_range.end`, so the heap keeps offset+limit (`src/collector/sort_key_top_collector.rs:62-73`, `:76-95`).

**Exactness caveats.**
- The comparisons are strict:
  - `if score > threshold { threshold = callback(..) }` (`src/query/boolean_query/block_wand.rs:206`, `src/query/weight.rs:55`);
  - a block is skipped when `block_max_score_upperbound <= threshold` (`block_wand.rs:179`);
  - `TopNComputer::push` drops any key that is not `Greater` than the threshold (`src/collector/top_score_collector.rs:623-630`).

  A document that ties the k-th score loses to the lower doc id. That matches the final tie rule, so it is consistent.
- **Block max scores are computed at index time with segment-local statistics.** `Bm25Weight::for_one_term_without_explain(term_doc_freq, num_docs_in_segment, self.avg_fieldnorm)` (`src/postings/serializer.rs:359-376`) stores the `(fieldnorm_id, tf)` pair that maximises the score for each full block (`:404-428`). The source states that the bound "may not be exact. In this case a lower value is returned, (and may lead us to return a lesser document)… guaranteed to be correct if there is only one segment" (`src/query/term_query/term_scorer.rs:58-75`). **On a multi-segment index, `TopDocs::order_by_score` can in rare cases miss a true top-k document.**
- The tf in the block max is clamped to 255, and 255 decodes as `u32::MAX`, so it stays an upper bound (`src/postings/skip.rs:32-42`).
- Partial last blocks are computed when loaded (`src/postings/block_segment_postings.rs:147-170`).

**Applies to codectx?**
- It conflicts with the no-top-k rule: pruning is a top-k optimisation by definition.
- It conflicts with determinism and exactness: it can return a lesser document on multi-segment indexes, and its f32 summation order differs from the full path.
- codectx filters by kind and folder, so filtered queries could never use it anyway.
- It is useful only as a speed reference: how fast tantivy's best case is.

**Metric moved.** Warm and cold median latency. Top-200 overlap between `library` (or `library-flat`) and `library-all` should be 200/200. Any shortfall measures the multi-segment inexactness.

**Benchmark variants.**
- Needs a new mode, `library-flat`: one `BooleanQuery` with SHOULD over every (term × field) `BoostQuery(TermQuery(.., WithFreqs), w)`, collected with `TopDocs::with_limit(200).order_by_score()`. Single-token terms only; phrases would turn pruning off. The scores equal `library` up to f32 summation order, but the query is eligible for block-WAND.
  ```
  tquery <idx> queries-name.txt 20 library      --phase lib-nested
  tquery <idx> queries-name.txt 20 library-flat --phase lib-flat
  tquery <idx> queries-name.txt 20 library-all  --phase lib-all
  tcompare results/<lib-flat>.jsonl results/<lib-all>.jsonl
  ```
  Run it on a one-segment index (`tindex … end`) and on a many-segment index (`tindex … unit`). The second run measures whether the inexactness ever shows up.
- Turning it off: `library-flat --kind function` (adds a MUST filter) against unfiltered `library-flat`. The latency gap is the value of pruning.
- Threads: `library-flat --search-threads 4` against 1 thread. Each segment prunes on its own threshold (`src/core/searcher.rs:220-236`).

## 9. `tweak_score` and `custom_score`

**What it is.** A per-document hook that turns `(DocId, Score)` into any `PartialOrd` sort key, set up once per segment.

**API.** As on the docs.rs TopDocs page:
```rust
pub fn tweak_score<F, TSortKey>(self, sort_key_fn: F)
    -> impl Collector<Fruit = Vec<(TSortKey, DocAddress)>>
// F: Fn(&SegmentReader) -> (Fn(DocId, Score) -> TSortKey)
```
It is defined at `tantivy-0.26.2/src/collector/top_score_collector.rs:426-438` and implemented by `TweakScoreFn` (`:441-470`).

**`custom_score` does not exist in 0.26.2.** `rg custom_score` over `src/` finds only a test name (`src/collector/top_score_collector.rs:1539`). The docs.rs TopDocs page lists `for_doc_range`, `with_limit`, `and_offset`, `doc_range`, `order_by`, `order_by_fast_field`, `order_by_score`, `order_by_string_fast_field`, `order_by_u64_field` and `tweak_score`, and no `custom_score`. The only changelog mention is an old entry (`CHANGELOG.md:631`). The general replacement is `TopDocs::order_by(impl SortKeyComputer)` (`src/collector/top_score_collector.rs:322-331`).

**Behaviour.**
- `requires_scoring()` is `true` (`src/collector/top_score_collector.rs:455-457`), so BM25 is computed.
- It goes through the default `collect_segment_top_k`, so **block-WAND never runs** (§8).
- Its input is the f32 `Score`. The key it returns can be an integer or a tuple, for example `(i64, Reverse<u64>)` read from fast fields. That makes ties content-derived within one `TopDocs`.
- It still caps at `limit`, and each call sees only one document's f32.

**Applies to codectx?** Only as a re-ranking hook. It cannot rebuild BM25F, because it sees the sum of per-field scores and not the per-field tfs. It could replace doc-address ties with a fast-field key such as `id`, which is what `library-all` does in its own collector. It is still a top-k collector, against the no-top-k rule.

**Metric moved.** Determinism of ties: `tcompare` order identity across delta and fresh indexes. Latency rises compared with `order_by_score`, because there is no pruning.

**Benchmark variant.** Needs a new mode, `library-tweak`: the `library` query with `TopDocs::with_limit(200).tweak_score(|seg| { let id = seg.fast_fields().str("id"); move |doc, s| (s, Reverse(id_ord(doc))) })`. Compare its warm median with `library`. Compare its order with `library-all` using `tcompare`; ties should then agree.

## 10. `explain()`

**What it is.** A tree of `Explanation { value: Score, description, details, context }`, serialisable with `to_pretty_json()` (`tantivy-0.26.2/src/query/explanation.rs:18-80`).

**API.**
- `Query::explain(&self, searcher: &Searcher, doc_address: DocAddress) -> Result<Explanation>` (`src/query/query.rs:137-142`)
- `Weight::explain(&self, reader: &SegmentReader, doc: DocId)` (`src/query/weight.rs:75`)
- `Bm25Weight::explain(fieldnorm_id, term_freq)` (`src/query/bm25.rs:196-226`)

**Behaviour.**
- `Query::explain` builds a fresh weight with `EnableScoring::enabled_from_searcher(searcher)` (`src/query/query.rs:139`). A custom `Bm25StatisticsProvider` is never reflected.
- The BM25 node copies Lucene's format: "TermQuery, product of..." with children `(K1+1)`, the idf with `n` and `N`, and `freq / (freq + k1 * (1 - b + b * dl / avgdl))`, where `dl` is the decoded *quantized* fieldnorm (`src/query/bm25.rs:205-224`).
- `BooleanWeight::explain` labels its node "BooleanClause. sum of ..." but takes the value from `scorer.score()` (`src/query/boolean_query/boolean_weight.rs:440-458`). For a DisMax, the label says "sum" while the value is the max-based combination.
- `BoostWeight::explain` multiplies the boost into the value (`src/query/boost_query.rs:74-81`).
- A document that does not match returns an error (`does_not_match`).
- Each call re-creates the weight and re-seeks the scorer, so it costs about one query per explained document.

**Applies to codectx?** As a diagnostic only, for example to show why a hit ranked where it did. It reports tantivy's f32, per-field and quantized-dl numbers, not codectx's BM25F. A codectx explanation would come from the `product` collector's own f64 terms.

**Metric moved.** None for ranking. Latency, if exposed per hit.

**Benchmark variant.** Needs a new probe: `texplain <idx> <query> <k>`. It runs `library`, calls `query.explain` for the top k, and records the time per explanation and whether `explanation.value()` equals the collected score bit for bit. A mismatch reveals path-dependent f32 (§6) or the DisMax finding (§7).

## Version notes since 0.20 (from `CHANGELOG.md`)
- 0.20: "Make BM25 scoring more flexible" introduced `Bm25StatisticsProvider` (`CHANGELOG.md:339`). Block-based collection for non-scoring cases (`:336`).
- 0.22–0.24: `collect_block` was exposed and added to boxed collectors (`:237-238`).
- 0.26:
  - "Add lazy scorers to stop score evaluation early when a doc won't reach the top-K threshold" (`:55`). The mechanism in the 0.26.2 source is the threshold compare in `for_each_pruning_scorer` and block-WAND; no type named "lazy" exists in `src/query/`.
  - `collect_block` for collector wrappers (`:57`).
  - Saturated posting lists replaced by `AllScorer` in boolean queries (`:58`), visible as `remove_and_count_all_and_empty_scorers` in `boolean_weight.rs`.
  - `seek_danger` (`:59`).
- 0.26.2: "Disable the buffered union `seek_danger` override to avoid consuming unaligned scorers" (`:6`). This is a correctness fix in the union that BM25 sums run through.

## Summary of new harness modes this area calls for
| Mode / probe | Query and collector | Purpose | Metric |
|---|---|---|---|
| `library-flat` | flat SHOULD of term×field `BoostQuery(TermQuery)`, `TopDocs(200).order_by_score()` | best-case block-WAND; inexactness on multi-segment indexes | warm/cold median; top-200 overlap with `library-all` |
| `library-livestats` | `library-all` through `search_with_statistics_provider` (live N, live union df, exact per-field tokens) | how much of the gap is statistics | top-10 overlap with `product` after a delta; MRR@10; latency |
| `library-dismax` | per term, `DisjunctionMaxQuery` over fields with tie_breaker 0/0.3/0.7 | an alternative combination; check the DisMax-under-WAND finding | MRR@10, S@1, S@10; `order_by_score` against `tweak_score` scores |
| `library-tweak` | `tweak_score` key (score, id ordinal) | content-derived ties in a stock collector | order identity across delta and fresh; latency |
| `texplain` | `Query::explain` on the top k | diagnostic cost and f32 consistency | time per explanation; value equals collected score |
| `tindex --no-fieldnorms body` | index option | effect of body length normalisation; disk | MRR@10; disk bytes |
