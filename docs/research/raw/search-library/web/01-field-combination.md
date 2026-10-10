# 01: Field combination: BM25F against summed per-field BM25

**Question.** Is BM25F, where weighted term frequencies are summed across fields and saturated once, better than adding up a separate BM25 score for each field? This file checks Robertson, Zaragoza and Taylor (CIKM 2004), later evaluations, and how Lucene, Elasticsearch and tantivy combine fields. It also asks what the evidence says for code search specifically.

**Credibility key.**
- **A:** a peer-reviewed paper, or the engine's own source, javadoc or issue tracker.
- **B:** official documentation, or an engineering write-up by the team that runs the system.
- **C:** a secondary source, used only to locate an A or B source.

---

## Findings

### 1. The theory: why saturate once, after summing the fields

**1.1 Per-field scoring treats each field as separate evidence.**
- Scoring each field with BM25 and adding the results assumes "a separate eliteness property for each term/stream pair", and also that these are independent given relevance.
- The authors call this "a little unreasonable". Their model is that eliteness belongs to the term and the document, and is shared across the fields.
- Hence: "for each term, we should accumulate evidence for eliteness across all the streams. The saturation function should be applied at this stage, to the total evidence for each term."
- Source: S. Robertson, H. Zaragoza, *The Probabilistic Relevance Framework: BM25 and Beyond*, Foundations and Trends in IR 3(4), 2009, §3.6.1, pp. 362–363. https://staff.city.ac.uk/~sbrp622/papers/foundations_bm25_review.pdf
- **A.** A peer-reviewed survey by two of the three authors of the CIKM 2004 paper; it restates that paper's model as [50].

**1.2 "Simple BM25F" weights the document length as well as the term frequency.**
- Equations 3.16–3.18 define it:
  - weighted term frequency: tf_i = Σ_s v_s·tf_si;
  - weighted document length: dl = Σ_s v_s·sl_s;
  - avdl = the average of that weighted dl;
  - one saturation, k1·((1−b)+b·dl/avdl) + tf_i.
- With integer weights, this is ordinary BM25 run on a document whose title has been "replicated five times and the abstract twice" (§3.6.4).
- The paper names this simple form as the CIKM 2004 version [50].
- Source: same monograph, §3.6.3–3.6.4, pp. 363–365. **A.**

**1.3 The later variant uses a separate b per field.**
- Equations 3.19–3.21: each field's frequency is normalised by its own B_s = (1−b_s)+b_s·sl_s/avsl_s. The normalised frequencies are then weighted, summed and saturated once with k1.
- "We have in particular found it useful to allow b to be stream-specific."
- Tuned TREC-2003 values were k1 27.5 for topic distillation and 4.9 for named page, with per-field b from 0.5 to 0.95 (Table 3.1).
- Source: same monograph, §3.6.3, Table 3.1. **A.**

**1.4 The idf comes from the whole collection, ignoring fields.**
- "In [50, 62] we computed IDFs on the entire collection disregarding streams. This worked well in practice, but it can lead to degenerate cases (e.g., when a stream is extremely verbose and contains most terms for most documents). The proper definition of IDF in this context requires further research."
- Source: same monograph, §3.6.3, p. 364. **A.**

**1.5 The weights rescale term frequency, so k1 should be rescaled with them.**
- Suggested starting value: k1_BM25F ≈ k1_BM25 · Σ_s v_s·avsl_s / Σ_s avsl_s.
- Source: same monograph, §5.2, eq. 5.1, pp. 381–382. **A.**

**1.6 The CIKM 2004 paper itself could not be read.**
- The ACM DL, ResearchGate and Semantic Scholar all return HTTP 403, and Semantic Scholar's open-access status is "CLOSED".
- Its abstract is known only through secondary listings: per-field scoring "then combining linearly" can break BM25's saturation, and the paper proposes weighting term frequencies before saturation.
- Its result tables were **not verified** here.
- Source: S. E. Robertson, H. Zaragoza, M. Taylor, *Simple BM25 extension to multiple weighted fields*, CIKM 2004, pp. 42–49, doi:10.1145/1031171.1031181. **A** for the claim, but read only through the monograph.

### 2. Evaluations

**2.1 TREC 2004 (same group).**
- "In previous work we showed that combining BM25 scores across fields can lead to a dangerous over-estimation of the importance of the term [7]."
- The per-field-b BM25F was tested with 4-fold cross-validation on the TREC-2003 named-page and topic-distillation topic sets, 50 topics each, against standard BM25 and the CIKM 2004 form. "The resulting performances were significantly better for a range of measures."
- No per-measure numbers for that comparison are printed. The submitted runs reach TD MAP 0.159–0.165 and NP MRR 0.719–0.731 (Table 5).
- Source: H. Zaragoza, N. Craswell, M. Taylor, S. Saria, S. Robertson, *Microsoft Cambridge at TREC-13: Web and HARD tracks*, TREC 2004. https://trec.nist.gov/pubs/trec13/papers/microsoft-cambridge.web.hard.pdf
- **A−.** A TREC notebook paper, not fully peer-reviewed. Its authors are the method's authors, on web data.

**2.2 DBpedia-Entity v2, a reproducible public benchmark.**
- Setup: 467 queries, entity search. The official runs give NDCG@10 and NDCG@100 for "Total".

| Run | NDCG@10 | NDCG@100 | What it is |
|---|---|---|---|
| BM25F-CA | 0.4605 | 0.5505 | Fielded BM25F, field weights tuned by coordinate ascent; best run |
| BM25-CA | 0.4399 | 0.5329 | Tuned single-field BM25 |
| BM25 | 0.2558 | 0.3582 | Untuned single-field BM25 |
| FSDM | 0.4524 | — | Fielded sequential dependence model |
| PRMS | 0.3905 | — | Probabilistic retrieval model for semistructured data |

- **Caveat.** This compares BM25F with *single-field* BM25. No summed per-field BM25 run is in the table. The gain from fields once both are tuned is +0.021 NDCG@10, about 5% relative.
- Sources:
  - F. Hasibi et al., *DBpedia-Entity v2: A Test Collection for Entity Search*, SIGIR 2017, doi:10.1145/3077136.3080751.
  - The runs table at https://github.com/iai-group/DBpedia-Entity/blob/master/README.md
- **A** (peer-reviewed collection with public runs). The table was read from the official repository, not the paper PDF, which was not reachable.

### 3. How the engines combine fields (read from source)

**3.1 Lucene `CombinedFieldQuery`, which implements simple BM25F.**
- **Its stated basis.**
  - The javadoc says: "The scoring is based on BM25F's simple formula described in" the monograph above.
  - Norms must be additive, "the norm of the combined field is the sum of norms for each individual field".
  - Norms must be encoded with `SmallFloat#intToByte4`. "Per-field similarities are not supported."
- **How it computes the statistics.**
  - Term frequency is weighted: `weight * postingsEnum.freq()`, then summed.
  - Document length is weighted: `MultiNormsLeafSimScorer` adds `weightArr[i] * LENGTH_TABLE[norm]` per field, then re-encodes the sum with `SmallFloat.intToByte4(Math.round(normValue))`. The combined length is therefore quantized again.
  - Collection statistics: `sumTotalTermFreq += weight * fieldStats.sumTotalTermFreq()`, and `maxDoc` and `docCount` take the maximum over the fields.
  - **Document frequency is `Math.max(termStats.docFreq(), docFreq)`**: the largest per-field df, not the union.
  - The builder rejects a weight below 1 ("weight must be greater or equal to 1").
- Sources: `lucene/core/src/java/org/apache/lucene/search/CombinedFieldQuery.java` and `MultiNormsLeafSimScorer.java`, apache/lucene main (fetched 2026-10-08). https://github.com/apache/lucene/blob/main/lucene/core/src/java/org/apache/lucene/search/CombinedFieldQuery.java
- **A.** Primary source. The sandbox copy of the same class is now marked deprecated in favour of the core one.

**3.2 Why Lucene uses the largest per-field df, from the issue that added the query.**
- LUCENE-8216, "Better cross-field scoring", 2018. The query was committed on 2018-11-21 as `BM25FQuery`, in the sandbox.
- **Why weights must be at least 1.** Adrien Grand asked whether "weights are greater than or equal to 1 so that ttf is guaranteed to be greater than or equal to df". Jim Ferenczi replied: "Yes this is required because we keep the max of the df and the sum of the ttf."
- **Why the df is the largest per-field value.** In 2021 Alessandro Benedetti noted the df "should be … the cardinality of the set that is the Union of all the posting lists", and that the max "is actually the lower bound". Jim Ferenczi replied: "We need an approximation because computing the 'real' frequency is too costly. We also need the approximation to be bounded by max_doc so the max frequency does the job."
- **Two follow-ups are still unresolved:**
  - LUCENE-8710, a separate b per field: unresolved, moved to GitHub apache/lucene#9756.
  - LUCENE-8711, moving the multi-field logic into Similarity: unresolved.
- **Renamed in 8.9.** LUCENE-9725 (Lucene 8.9) generalised the query to other similarities and renamed it `CombinedFieldQuery`.
- Source: https://issues.apache.org/jira/browse/LUCENE-8216 (via the JIRA REST API), and LUCENE-8710, LUCENE-9725. **A.** Primary issue thread written by the implementers.

**3.3 Lucene `DisjunctionMaxQuery`, and per-field sums under `BooleanQuery`.**
- The javadoc frames DisMax as the fix for `BooleanQuery`'s "sum of the field scores". It scores the maximum plus `tieBreakerMultiplier` (0 to 1, "on the order of 0.1") times each other matching clause.
- Its stated goal is that "albino" matching one field and "elephant" another beats "albino" matching both.
- Each disjunct is still separately saturated BM25 with per-field statistics.
- Source: `lucene/core/src/java/org/apache/lucene/search/DisjunctionMaxQuery.java`, main. **A.**

**3.4 Elasticsearch.**
- **The three query types.**
  - `combined_fields` is "based on the simple BM25F formula". It requires the same search analyzer on every field and text fields only, and boosts must be at least 1.0. Elastic says it "makes some approximations, so scores will not obey the BM25F model perfectly".
  - `multi_match` `best_fields` takes the best field's score "plus `tie_breaker * _score` for all other matching fields". `most_fields` adds the per-field scores "just like a `bool` query".
  - `cross_fields` blends the per-field document frequencies. Elastic warns: "The score combination can even be incorrect", and says it is "usually only useful on short string fields that all have a boost of 1".
  - Source: Elastic docs, *combined_fields query* and *multi_match query* (current), https://www.elastic.co/docs/reference/query-languages/query-dsl/query-dsl-combined-fields-query and https://www.elastic.co/docs/reference/query-languages/query-dsl/query-dsl-multi-match-query. **B.**
- **Why combined_fields is a separate query.**
  - Elastic engineers planned it to avoid "the broken `cross_fields` scoring".
  - It became its own query because "BM25F is designed to combine scores across text fields. The field boosts have a very specific meaning, they must be at least 1.0f and represent a multipler on term frequency in the combined synthetic field."
  - Source: elastic/elasticsearch issue #41106, *Better cross-field support* (J. Tibshirani, J. Ferenczi, M. Harwood, A. Grand), 2019–2021. https://github.com/elastic/elasticsearch/issues/41106. **A.** Implementers' design record.
  - Related: #44700, "Cross-field type creates broken scores when not all fields have the same docCount"; and #89016 (2022), "Avoid negative scores with cross_fields type". **A.**

**3.5 tantivy 0.26.2: no BM25F.**
- **Each BM25 weight belongs to one field.**
  - `Bm25Weight::for_terms` asserts "All terms must belong to the same field."
  - The average length is that field's `total_num_tokens / total_num_docs`.
  - The length comes from the one-byte fieldnorm, through a 256-entry table: `src/query/bm25.rs:95-127, 58-69`.
- **Multi-field queries sum per-field scores.**
  - With several default fields, `QueryParser` builds `Occur::Should` over a boosted query per field (`src/query/query_parser/query_parser.rs:777-793`).
  - That sums separately saturated per-field BM25, each with its own df and average length: the BooleanQuery behaviour Lucene's DisMax javadoc warns against.
- **DisMax is available, but has a defect under top-k.**
  - `DisjunctionMaxQuery::with_tie_breaker` is available.
  - The companion feature research verified a defect when it runs under `TopDocs`: the block-WAND path sums the scores instead of taking the max (`src/query/boolean_query/block_wand.rs:200-204`). See file 02.
- Source: tantivy 0.26.2 source, https://github.com/quickwit-oss/tantivy/blob/0.26.2/src/query/bm25.rs. **A.**
- A search of the tantivy issue tracker for "BM25F" returned no issue or plan (`gh search issues --repo quickwit-oss/tantivy BM25F`, 2026-10-08).

**3.6 SQLite FTS5 `bm25()`, the function codectx's lexical tier grew from.**
- **Term frequency:** column-weighted. For each phrase instance, `aFreq[ip] += w`, where `w` is the weight of the instance's column.
- **Length: not weighted.** `D` comes from `xColumnSize(pFts, -1, &nTok)`, the total tokens in the row over all columns.
- **avgdl:** `nToken / nRow`.
- **idf:** `log((N − nHit + 0.5)/(nHit + 0.5))`, with a floor of 1e-6 when that is ≤ 0. nHit is the number of rows matching the phrase in any column, so the df is the union.
- **Saturation:** applied once per phrase, with k1 1.2 and b 0.75.
- **Same shape as codectx.** Weighted tf, unweighted length, union df: this is codectx's formula except for the idf, where codectx uses Lucene's `ln(1 + …)`.
- **It differs from the paper's simple BM25F only in the length term:** dl is not Σ v_s·sl_s.
- Source: `ext/fts5/fts5_aux.c`, `fts5Bm25Function` and `fts5Bm25GetData`, sqlite/sqlite master (fetched 2026-10-08). https://github.com/sqlite/sqlite/blob/master/ext/fts5/fts5_aux.c **A.**

### 4. Code search specifically

**4.1 Sourcegraph adopted BM25F in production.**
- How Zoekt scores:
  - Each content match adds 1 to a term's frequency, and each filename or symbol-definition match adds 5.
  - The result is saturated once with k1 1.2 and b 0.75.
  - The length is the file length divided by the average file length.
- Why not per-field sums: summing them "gives too big an advantage to files with a match on multiple fields", because it does not respect saturation.
- Result: "BM25 showed roughly 20% improvement across all key metrics compared to our baseline ranking". The baseline was Sourcegraph's previous ranking, not per-field BM25.
- The boost of 5 came from "a tiny grid of options", and "the final ranking is not too sensitive to the exact choice of boost".
- Source: J. Tibshirani, *Keeping it boring (and relevant) with BM25F*, Sourcegraph blog, 2025-04-04. https://sourcegraph.com/blog/keeping-it-boring-and-relevant-with-bm25f
- **B.** The team runs this in production. The evaluation set, its size and the metric names are not published, so the 20% cannot be checked.

**4.2 Zoekt's code matches the blog post.**
- `importantTermBoost = 5`, added for `m.fileName || p.matchesSymbol(m)`.
- `lowPriorityFilePenalty = 5` divides the term frequency of test and generated files. The comment reads: "The BM25F interpretation is that this data lives in a separate 'field'".
- `tfScore` is `((k+1)·f)/(k·(1−b+b·L)+f)`.
- Line scoring "skips inverse document frequency (idf) to keep the implementation simple".
- Source: `index/score.go`, sourcegraph/zoekt main (fetched 2026-10-08). https://github.com/sourcegraph/zoekt/blob/main/index/score.go **A.**

**4.3 Summed per-field scores in software engineering research.**
- BLUiR runs one search per (query part, code field) pair: two query parts × four fields (class, method, variable, comments), eight searches in all. It then sums the scores, using Indri's BM25-style term weighting.
- That beat the same model without fields on all four systems for MAP and MRR. Examples: SWT MAP 0.47 → 0.56, Eclipse 0.26 → 0.32.
- The authors name BM25F as the better untested option: "or better yet, weighting term frequencies rather than document fields to better control for term frequency saturation".
- Source: R. K. Saha, M. Lease, S. Khurshid, D. E. Perry, *Improving Bug Localization using Structured Information Retrieval*, ASE 2013, §IV-C and Table V. https://ischool.utexas.edu/ml/papers/saha-ase13.pdf
- **A.** Peer-reviewed. The task is bug localization: bug-report text against source files, not identifier queries.

---

**Evidence is thin where:**
- No peer-reviewed study compares BM25F with summed per-field BM25 on **code search**.
- The only code-search evidence for BM25F is Sourcegraph's unpublished internal evaluation, and its baseline was not per-field BM25.
- The CIKM 2004 result tables were not verified, because the paper is closed access. The comparison rests on the authors' own survey and their TREC report ("significantly better", without per-measure numbers).
- No study measures union df against Lucene's largest per-field df.
- No study measures weighted against unweighted document length.

## What this means for codectx

**Rule 7: f64 BM25F with one saturation and weights 5/5/2/2/1.**
- **Summing before saturating: supported.**
  - The theory supports it (§1.1).
  - Both large engines' implementers reached the same structure (§3.1, §3.4).
  - Sourcegraph reports it beat their earlier ranking in production code search (§4.1). Their choice of 5 for name and symbol matches against 1 for content matches codectx's name and qualified-name weights.
  - tantivy's stock multi-field query is a **sum of per-field BM25**. That is the model this evidence argues against, so a drop-in tantivy query would be a regression on the evidence (§3.5).
- **"dl = exact token_count" and an unweighted avgdl: contradicted.**
  - Simple BM25F (eq. 3.17) and Lucene's `CombinedFieldQuery` both use a *weighted* length: dl = Σ weight·field_length, and avgdl is the mean of that.
  - codectx uses `token_count`, the unweighted token total of the five columns. codectx therefore weights the term frequency but not the length.
  - This is FTS5's `bm25()` shape (§3.6), not the paper's.
  - Effect: the weighted term frequency can be up to 5× the raw count while the length term stays unweighted, so name and qualified-name matches saturate sooner than the model intends. Rescaling k1 (eq. 5.1) would compensate in part.
  - This is a departure from the cited formula, not a measured loss. A side-by-side run would settle it.
- **A separate b per field: not implemented, and neutral.**
  - The authors found it useful (§1.3).
  - Lucene's issue for it has been open since 2019 (§3.2).
  - Nothing measured on code.

**Rule 1: exact df over the union of the columns, visible documents only.**
- **Supported on definition.**
  - The model's authors compute idf over the whole collection "disregarding streams" (§1.4).
  - Lucene's implementer agrees the union df is the right one, and uses the largest per-field df only because the union is "too costly" (§3.2).
  - codectx computes the exact union, which is more faithful than Lucene.
- **Not measured.** No study measures the quality difference, and the authors warn that the idf definition for fields is unsettled.

**Rule 11 (1e-6 quantization to int64), rule 4 (content-only deterministic ties), rule 8 (tokenizer), rule 9 (tier first) and rule 10 (keyset paging): neutral.** This question does not bear on them.

**What the decision turns on.**
- Adopting tantivy keeps BM25F only if codectx scores outside tantivy's `Bm25Weight`.
- The library offers no BM25F and no union df.
- `DisjunctionMaxQuery` is a different model, and it has the pruning defect described in file 02.
