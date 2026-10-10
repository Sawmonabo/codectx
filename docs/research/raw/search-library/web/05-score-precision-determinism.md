# 05: Score precision and determinism: f32 against f64, and tie-breaking

**Question.**
- What score precision do Lucene and tantivy use?
- How is a deterministic order obtained when scores tie?
- Is there evidence that either choice affects quality or repeatability?

**Credibility key.**
- **A:** a peer-reviewed paper, or the engine's own source, javadoc or issue tracker.
- **B:** official documentation, or an engineering write-up by the team that runs the system.
- **C:** a secondary source, used only to locate an A or B source.

---

## Findings

### 1. Precision in the engines

**1.1 Lucene scores are 32-bit floats; sums go through doubles.**
- `Scorable.score()` returns `float`.
- `DisjunctionSumScorer.score` accumulates the clause scores in a `double`, then casts the total: `double score = 0; … score += w.scorable.score(); return (float) score;`.
- `BM25Similarity` holds `float k1, b`. Its idf is `(float) Math.log(1 + (docCount - docFreq + 0.5D)/(docFreq + 0.5D))`.
- Source: `lucene/core/src/java/org/apache/lucene/search/Scorable.java`, `DisjunctionSumScorer.java` and `similarities/BM25Similarity.java`, main (fetched 2026-10-08; 10.5.2 is the newest release, 2026-10-04). **A.**

**1.2 Lucene's own developers hit f32 rounding problems.**
- **Scores not increasing with term frequency.**
  - Robert Muir: "it seems like our bm25 may 'go backwards' for largish term freqs, looks like floating point issues to me."
  - For the DFR models: "I changed SimilarityBase to use double everywhere internally, then cast to 32-bit float at the end. This fixed all the numerical errors."
  - Source: LUCENE-7997, *More sanity testing of similarities*, 2017 (fixed in 8.0). https://issues.apache.org/jira/browse/LUCENE-7997 **A.**
- **The order of a float sum changes its value, and pruning has to allow for it.**
  - `MathUtil.sumUpperBound` exists because "The error of sums depends on the order in which values are summed up". It bounds the error with Higham (1993), formula 3.5.
  - `ScorerUtil.minRequiredScore` loops on `Math.ulp(minCompetitiveScore)` so that rounding cannot prune a competitive document.
  - Source: `lucene/core/src/java/org/apache/lucene/util/MathUtil.java` and `search/ScorerUtil.java`, main. **A.**
- **An f32 boundary case looped forever in production.** Elasticsearch searches were "stuck for long time" at high CPU. A float/double ulp mismatch in `MaxScoreSumPropagator.getMinCompetitiveScore` failed to converge. Source: LUCENE-10428, 2022 (fixed in 9.1). https://issues.apache.org/jira/browse/LUCENE-10428 **A.**

**1.3 tantivy 0.26.2 is f32 throughout, and sums in f32.**
- `pub type Score = f32;` (`src/lib.rs:314`).
- `K1` and `B` are `Score`.
- The idf is computed as `Score` (`src/query/bm25.rs:8-9, 52-56`).
- `SumCombiner` adds in f32 (`self.score += scorer.score()`, `src/query/score_combiner.rs:38-47`). There is no double accumulator.
- Source: https://github.com/quickwit-oss/tantivy/blob/0.26.2/src/query/score_combiner.rs **A.**

**1.4 What f32 can resolve.** The arithmetic below is computed here, with numpy `spacing`. **A.** IEEE-754 arithmetic.

| Score | f32 step (ulp) | f64 step (ulp) |
|---|---|---|
| 1 | 1.2e-7 | 2.2e-16 |
| 8 to 16 | 9.5e-7 | 1.8e-15 |
| 16 to 32 | **1.9e-6** | 3.6e-15 |
| 32 to 64 | **3.8e-6** | 7.1e-15 |
| 100 | **7.6e-6** | 1.4e-14 |

From a score of 16 up, f32 cannot represent a 1e-6 grid: neighbouring f32 values are already 1.9e-6 apart.

### 2. How the engines break ties

**2.1 Lucene.**
- Within an index, `HitQueue.lessThan` compares `Float.compare(score)`. On a tie, the lower `doc` wins (`hitA.doc > hitB.doc`).
- Across shards, `TopDocs.merge` breaks ties by `shardIndex`, then `doc` (`DEFAULT_TIE_BREAKER = SHARD_INDEX_TIE_BREAKER.thenComparing(DOC_ID_TIE_BREAKER)`). Within one shard it keeps that shard's order.
- Source: `HitQueue.java` and `TopDocs.java`, main. **A.**

**2.2 Elasticsearch: internal ids differ between copies of a shard.**
- "when two documents have the same score, they will be sorted by their internal Lucene doc id", and these ids can differ between copies of one shard. The scores can differ too, because "index statistics may be different across copies of the same shard due to deleted documents".
- The fix offered is a sticky `preference` (a user or session id). That makes results consistent per user, not true for all users.
- Source: Elastic, *Getting consistent scoring*. https://www.elastic.co/guide/en/elasticsearch/reference/master/consistent-scoring.html **B.**

**2.3 Elasticsearch: deep paging needs a unique tie-break key.**
- "This tiebreaker field should contain a unique value for each document. If you don't include a tiebreaker field, your paged results could miss or duplicate hits."
- "All PIT search requests add an implicit sort tiebreaker field called `_shard_doc`."
- "If a refresh occurs between these requests, the order of your results may change".
- "We no longer recommend using the scroll API for deep pagination."
- Source: Elastic, *Paginate search results*. https://www.elastic.co/docs/reference/elasticsearch/rest-apis/paginate-search-results **B.**

**2.4 tantivy.**
- The `TopDocs` doc says: "In case of a tie on the sort key, documents are always sorted by ascending `DocAddress`" (`src/collector/top_score_collector.rs:26-28`). The comparator is `.then_with(|| lhs.doc.cmp(&rhs.doc))` (`:596`).
- Multi-threaded search places each segment's results back by position (`result_placeholders[pos] = Some(fruit)`, `src/core/executor.rs:87-93`), so threads add no nondeterminism.
- `DocAddress` is (segment ordinal, doc id). The companion feature research found segment order follows `max_doc` and then a HashMap keyed by a random UUID. A delta or rebuild therefore re-addresses the same content.
- **A.**

**2.5 tantivy had a tie determinism defect, and its fix cost speed.**
- The old `TopNComputer` threshold did not include the doc id. Stu Hood explains this "was a determinism hazard … if all of the matches had a single feature value, then some might be eliminated non-deterministically based on the order that segments were collected".
- The fix kept equal-valued items in the buffer, which is deterministic. ParadeDB then measured tie-heavy top-N as **60% slower**.
- Sources:
  - quickwit-oss/tantivy#2775, 2025-12. https://github.com/quickwit-oss/tantivy/issues/2775
  - The fix, quickwit-oss/tantivy#2672, merged 2025-07-16.
- **A.** Maintainers' and an embedder's issue thread with a benchmark.

### 3. What ties do to measured quality and repeatability

**3.1 Tie order by internal id makes runs unrepeatable. The measured differences are small but real.**
- **Setup.** In Lucene, ties are broken by internal id, and "internal document ids are not assigned consistently between different index instances of the same collection" because of multi-threaded indexing. Five index builds per collection were compared.
- **Newswire, bag-of-words queries.** Variation is usually in the 4th decimal: BM25 AP Δ 0.0003 and P30 Δ 0.0004 on Robust04, and AP Δ 0.0006 and P30 Δ 0.0013 on Robust05. The largest newswire Δ is 0.0027 (P30, QL, Robust05).
- **Tweets, which have more ties.** Up to 0.0026 AP and 0.0058 P30.
- **Repeatability cost.** Breaking ties by the *external* id made queries **~18–26% slower** for bag-of-words, and up to +90% for QL+RM3 on tweets.
- **Their recommendation.** "score ties should be broken by external collection ids".
- Source: J. Lin, P. Yang, *The Impact of Score Ties on Repeatability in Document Ranking*, SIGIR 2019, pp. 1125–1128, doi:10.1145/3331184.3331339; arXiv:1807.05798v2, Tables 1–4. https://arxiv.org/abs/1807.05798 **A.**

**3.2 Tie order can change evaluation results.**
- `trec_eval` re-sorts a run and breaks ties by `docno`, so "a relevant document can rise in rank only because of its name".
- Sources:
  - G. Cabanac, G. Hubert, M. Boughanem, C. Chrisment, *Tie-Breaking Bias*, CLEF 2010, LNCS 6360, pp. 112–123, doi:10.1007/978-3-642-15998-5_13. Read here through the authors' project page, http://www.irit.fr/~Guillaume.Cabanac/tie-breaking-bias/
  - A TREC-5 figure quoted by Di Buccio et al. (SIGIR RIGOR 2015, https://www.dei.unipd.it/~ferro/papers/2015/SIGIR-RIGOR_2015.pdf): ties and reordering "cause average changes in Average Precision (AP) around 1% but with a maximum of 139%".
- **A** for the claim. The effect sizes are second-hand here, because the Springer PDF was not fetched.

### 4. A finding from this project's own probe (not from the web)

Go's `math.Log` and Rust's `f64::ln` differ in the last bit on 1 891 of 200 127 inputs. That is why the benchmark prototype ported Go's log bit for bit. Implementations of f64 also differ, so a cross-language engine needs one pinned log implementation, not just f64.

---

**Evidence is thin where:**
- No study compares f32 with f64 scoring for ranking *quality*. The f32 evidence is only about engineering faults: non-monotonic scores, pruning bounds, and an infinite loop.
- No study measures tie frequency in code search. Code has many near-duplicate short units, such as overloads and generated stubs. Tweets, the closest analogue, showed the largest tie effects (§3.1).
- Quantizing to a fixed decimal grid, as codectx does, appears in the literature only as a speed technique (impact quantization, cited by Lin and Yang), not as a determinism one.

## What this means for codectx

**Rule 11 (f64, quantized to round(score·1e6) as int64): supported, and f32 is shown to be insufficient.**
- f32 steps are 1.9e-6 at scores of 16 and above (§1.4). tantivy's f32 `Score` cannot carry codectx's 1e-6 grid.
- Lucene's own developers moved internal arithmetic to double for correctness (§1.2).
- f64 gives about 1e-14 at these magnitudes.
- The quantization step makes the result independent of summation order, provided every arithmetic step is identical. That needs the pinned log (§4).

**Rule 4 (content-only ties): supported.**
- Engine-internal tie-breaks are not stable across index builds or copies:
  - Lucene and Elasticsearch use the doc id and shard index (§2.1–2.2);
  - tantivy uses `DocAddress` (§2.4).
- The literature recommends a key from outside the engine (§3.1).
- Elastic requires a unique tie-break key for paging (§2.3).
- The measured cost is about 20% latency in Lucene (§3.1) and 60% on tie-heavy top-N in tantivy (§2.5), so it should be budgeted.
- codectx breaks ties on content: path, byte range, kind, names. That is stricter than the external id recommended, and survives rebuilds.

**Rule 2 (delta = fresh): supported.** Elastic documents that statistics *and* tie order differ between shard copies that hold the same content (§2.2), which is the same failure in a different form.

**Rule 10 (keyset-cursor paging): supported.** Elastic's guidance for complete deep paging is a `search_after` keyset over a unique total order, preferably inside a point in time (§2.3). That is codectx's design. Elastic's own warning that "the order of your results may change" after a refresh is what codectx's pinned generation prevents (rule 5).

**Rule 7 (f64 BM25F): supported on precision.** tantivy's scoring path (`Bm25Weight`, `SumCombiner`, `Scorer::score`) is f32 end to end. codectx must compute its score outside it, as the prototype's custom collector does.

**Rule 1, rule 3, rule 6, rule 8 and rule 9: neutral.** This question does not bear on them.
