# 03 — Deleted documents in scoring statistics, and consistent scoring across segments and shards

**Question.** How do Lucene, Elasticsearch and tantivy treat deleted documents in docFreq, maxDoc/N and avgdl? Is the effect on ranking quality measured anywhere? How do they get consistent scoring across shards and segments (DFS query-then-fetch, global statistics)?

**Credibility scale.**
- A = peer-reviewed paper, or primary engine source, javadoc or issue tracker.
- B = official docs, or an engineering write-up by the team that runs the system.
- C = secondary, used only to locate an A or B source.

**Short answer.** All three engines count deleted documents in every BM25 statistic until a merge physically removes them.
- They say this is deliberate, "for practical reasons".
- Scores therefore move when a merge runs.
- Two copies of the same data can rank differently.

Consistency across segments is solved inside one index: statistics are summed over all segments. Across shards it is solved by an extra statistics round trip (Elasticsearch DFS; Solr ExactStatsCache). Those global statistics still include the deletes.

No source measures what counting deleted documents does to ranking quality. The closest measured evidence comes from distributed IR: BM25-style ranking is robust to approximate global statistics. That makes the case for exact statistics one of reproducibility, not of ranking quality.

---

## 1. What each engine counts

| # | Claim | Source | Cred. | Why this rating |
|---|---|---|---|---|
| L1 | Lucene `docFreq(term)` "does not take into account deleted documents that have not yet been merged away". `totalTermFreq`, `getSumDocFreq`, `getDocCount` and `getSumTotalTermFreq` all "just like other term measures … [do] not take deleted documents into account". Here "not take into account" means deletes are *not subtracted*. | `IndexReader.java:433-471`, Lucene 10.5.2 (latest release, 2026-10-04), https://github.com/apache/lucene/blob/releases/lucene/10.5.2/lucene/core/src/java/org/apache/lucene/index/IndexReader.java | A | Primary source and javadoc |
| L2 | `CollectionStatistics`: "Values may include statistics on deleted documents that have not yet been merged away." | `CollectionStatistics.java:41` (10.5.2) | A | Source |
| L3 | Lucene BM25 uses `N = docCount` ("total number of documents with field"), not maxDoc. (LUCENE-6711, 2015, made the same maxDoc-to-docCount change for `SimilarityBase`.) avgdl is `sumTotalTermFreq / docCount`. Both include deleted documents (L1). `maxDoc()` is "one greater than the largest possible document number"; `numDocs()` excludes deletes. | `BM25Similarity.java:102-109, 140-148`; `IndexReader.java:327-346` (10.5.2); LUCENE-6711, https://github.com/apache/lucene/issues/7769 | A | Source and tracker |
| L4 | `IndexSearcher.collectionStatistics(field)` sums `docCount`, `sumTotalTermFreq` and `sumDocFreq` over **every leaf (segment)**. Statistics are therefore global within one reader: every segment scores with the same N, df and avgdl. The javadoc says it "can be overridden … to return a field's statistics across a distributed collection". | `IndexSearcher.java:1119-1167` (10.5.2) | A | Source |
| L5 | Deletes are bounded by the merge policy, not by the statistics. The control is `TieredMergePolicy.deletesPctAllowed`, "the maximum percentage of doc id space taken by deleted docs … Default value is 20". It was introduced by LUCENE-8263 (2018) with a default of 33. The default was lowered to 20, with a floor of 5, by GITHUB-11761 (commit d966adcb62, 2022-10-05). LUCENE-8263's author reported that a 20% target raised I/O by about 10%, and a 10% target by about 50%. | `TieredMergePolicy.java:95, 159-177` (10.5.2); LUCENE-8263, https://github.com/apache/lucene/issues/9310; apache/lucene commits 8093c450c1 and d966adcb62 | A | Source, tracker and commit history |
| E1 | Elasticsearch: "for practical reasons, those deleted documents are taken into account for index statistics". Index statistics "may be different across copies of the same shard due to deleted documents". So the same request can return documents in a different order depending on which copy answers. The docs recommend a `preference` string per user, a single shard for small data, or `dfs_query_then_fetch`. | Elastic, "Getting consistent scoring", current docs, https://www.elastic.co/docs/solutions/search/full-text/search-relevance/consistent-scoring | B | Vendor docs |
| E2 | "When a merge completes, the term statistics will suddenly jump closer to their true values, changing hit scores." In a worst-case update test, deletes "hover between 35% and 60%". Relevance was not measured; only QPS was (−18% to −46% at 50% deletes). | Michael McCandless, "Lucene's Handling of Deleted Documents", Elastic blog, 2015-01-30, https://www.elastic.co/blog/lucenes-handling-of-deleted-documents | B | Written by a Lucene committer; the numbers predate the merge-policy change in L5 |
| T1 | tantivy's default `Bm25StatisticsProvider` (the `Searcher`) sums `segment_reader.max_doc()` for N. `max_doc` counts deleted documents. | `tantivy-0.26.2/src/query/bm25.rs:38-45` | A | Crate source |
| T2 | `InvertedIndexReader::total_num_tokens()` returns "the total number of tokens recorded for all documents (including deleted documents)". `doc_freq` returns the `term_info.doc_freq` written at indexing time. A live df exists only as `SegmentPostings::doc_freq_given_deletes(&AliveBitSet)`, which "will clone and scan through the posting lists. (this is a rather expensive operation)". | `tantivy-0.26.2/src/index/inverted_index_reader.rs:249-253`; `src/postings/segment_postings.rs:32-36` | A | Crate source |
| T3 | Maintainer, on a report that `doc_freq` returns 1 after the only matching document was deleted: "Yes we ignore delete in `doc_freq`. We should update the documentation accordingly." The issue has been open since 2020. In the report, the stale count also depended on the number of indexing threads (100, 99, 98 …), because it depends on how documents fall into segments. | quickwit-oss/tantivy issue #798, https://github.com/quickwit-oss/tantivy/issues/798 | A | Tracker |
| T4 | The `Bm25StatisticsProvider` hook was added so that users can "adjust the statistics to account for deletes". The PR author: "Currently the numbers of documents and tokens include deleted ones which is somewhat surprising." | tantivy PR #1855 (merged 2023-02-09), https://github.com/quickwit-oss/tantivy/pull/1855 | A | Merged PR |
| T5 | After a merge, tantivy *estimates* the token total of a segment that had deletes, from the quantized fieldnorms of the live documents. The code itself notes that even the no-delete value "is not necessarily exact as it could have been the result of a merge between segments themselves containing deletes". So avgdl after delete-and-merge differs from a fresh build's, and the difference persists. | `tantivy-0.26.2/src/indexer/merger.rs:29-58` | A | Crate source |
| T6 | tantivy has no soft-delete feature. Issue #2838, "Soft deletes" (2026-02), is an open feature request. A delete is only an alive bitset until a size-triggered merge purges it. `del_docs_ratio_before_merge` defaults to 1.0, meaning "deletes are not taken in account when identifying merge candidates". | tantivy issue #2838, https://github.com/quickwit-oss/tantivy/issues/2838; `tantivy-0.26.2/src/indexer/log_merge_policy.rs:12-15` | A | Tracker and source |

## 2. Consistency across segments and shards

| # | Mechanism | Source | Cred. |
|---|---|---|---|
| C1 | **Segments, inside one index.** Lucene's `IndexSearcher` (L4) and tantivy's `Searcher` (T1) sum statistics over all segments. Segment layout therefore does not change N or df, except through the deleted documents each segment still holds. | as L4, T1 | A |
| C2 | **Shards, Elasticsearch.** `DFS_QUERY_THEN_FETCH` is "Same as QUERY_THEN_FETCH, except for an initial scatter phase which goes and computes the distributed term frequencies for more accurate scoring". The default is `QUERY_THEN_FETCH`, which uses shard-local statistics. The DFS phase records each shard's Lucene `termStatistics` and `collectionStatistics`. `aggregateDfs` then **sums** docFreq, totalTermFreq, maxDoc, docCount, sumTotalTermFreq and sumDocFreq across shards. So DFS statistics are global but still include deletes (L1). | `SearchType.java:17-36`, `DfsPhase.java:78-120`, `DfsQueryPhase.java:189-243`, elastic/elasticsearch main (latest release 9.5.5, 2026-10-06), https://github.com/elastic/elasticsearch/tree/main/server/src/main/java/org/elasticsearch | A |
| C3 | DFS "provides the same as having set `number_of_shards=1`", at the cost of an extra round trip. It "is mostly needed only when the data between shards continues to be unevenly distributed". | Shane Connelly (Elastic), "Practical BM25 – Part 1: How Shards Affect Relevance Scoring", 2018-04-19, https://www.elastic.co/blog/practical-bm25-part-1-how-shards-affect-relevance-scoring-in-elasticsearch | B |
| C4 | **Shards, Solr.** `statsCache` options: `LocalStatsCache` (the default) "uses only local term and document statistics"; `ExactStatsCache` "uses global values (across the collection) for document frequency"; `ExactSharedStatsCache` reuses them across requests; `LRUStatsCache` caches them. | Apache Solr Reference Guide, "SolrCloud Distributed Requests", https://solr.apache.org/guide/solr/latest/deployment-guide/solrcloud-distributed-requests.html | B |
| C5 | **Distributed tantivy.** The engine has no protocol for this. The hook is a custom `Bm25StatisticsProvider` passed to `Searcher::search_with_statistics_provider` (T4). Its `doc_freq(&Term)` is per field, so it cannot express a df over several fields. | `tantivy-0.26.2/src/query/bm25.rs:15-25, 98-107`; `src/core/searcher.rs:193-208` | A |

## 3. Is the quality effect measured?

| # | Evidence | Source | Cred. |
|---|---|---|---|
| Q1 | **Nothing measured for deletes.** I searched Lucene, ES and tantivy docs and trackers, and SIGIR, CIKM, ECIR and IP&M venues, for any effectiveness measurement of statistics that include deleted documents. I found none. Engine authors describe the effect qualitatively: "suddenly jump" after a merge (E2), and replicas ranking differently (E1). | — | — |
| Q2 | **Closest measured evidence, from distributed IR.** Collection-wide information must be shared across sites to match centralized effectiveness, but "complete dissemination is unnecessary". How much is needed depends on how documents are allocated: little for random allocation, more for allocation by content. | Viles, French, "Dissemination of collection wide information in a distributed IR system", SIGIR 1995 (abstract read; full text not fetched) | A (abstract only) |
| Q3 | Estimating global term weights (IDF) without a global view gives "very good retrieval performance … when just the most frequent terms of a collection are known". Mixing small samples with a reference corpus works well. Measured on standard IR test collections. | Witschel, "Global term weights in distributed environments", IP&M 44(3) 1049–1061, 2008, DOI 10.1016/j.ipm.2007.09.003 (abstract read via an index page; the publisher returned 403) | A (abstract only) |

**Evidence is thin where:**
- No study measures the quality cost of counting deleted documents in df, N or avgdl, in general or for code search.
- Q2 and Q3 concern approximate statistics in distributed web or news retrieval, not deletes. They suggest the quality effect of a few percent of stale documents is small, but that is an inference.
- The McCandless numbers (E2) predate the default change from 33% to 20% (L5).
- I found no published number on how far tantivy's merge-time token estimate (T5) drifts on real data.

## What this means for codectx

- **Rule 1 exact df over the column union, visible documents only — supports the mechanism, not a quality claim.** No engine provides it.
  - Lucene and ES count deletes (L1, E1). tantivy counts deletes, and its maintainer confirms that `doc_freq` ignores them (T2, T3).
  - All three keep df per field, so none has a union df (C5).
  - The evidence does **not** show that exact statistics rank better (Q1–Q3). Rule 1 should be justified by rule 2 (reproducibility and identity), not by ranking quality. If the owner ever trades identity for speed, the measured literature suggests approximate statistics would cost little quality. That is a judgment call, and these sources do not settle it.
- **Rule 2 delta = fresh — supports.** All three engines violate it by design: scores "jump" when a merge runs (E2), replicas disagree (E1), tantivy's stale df depends on the thread count (T3), and its post-merge avgdl is an estimate (T5). Only statistics computed from the visible set, which is what codectx does, give identical answers for identical content.
- **Rule 7 dl = exact token_count — supports.** Lucene's avgdl uses lengths that include deleted documents, and its per-document norms are quantized (see 04). tantivy's merged token total is rebuilt from quantized fieldnorms (T5). An exact token-count column with a visible-only mean is the only way to stay independent of merge history.
- **Rule 6 soft deletes survive merges — neutral to contradicting the engines' model.** In every engine a delete is soft only until a merge purges it, and merges are triggered by size or by the delete ratio (L5, T6). tantivy has no soft-delete feature (T6). Rule 6 needs visibility kept outside the engine's delete mechanism, for example a custom alive set per generation.
- **Rule 5 retained snapshots — neutral.** The engines give consistency within one reader or PIT (see 02, D2). Nothing here addresses snapshots that persist across restarts.
- **Rule 11 1e-6 quantization — neutral.** No source here.
- **Cross-shard consistency (C2–C4) — not needed by codectx.** codectx has one local index per generation, so the shard problem does not arise. The analogue is segments: C1 already makes N and df global across segments, and rule 1/rule 2 additionally remove the deleted documents. If the lexical tier ever splits its index, the ES pattern (gather the statistics, then score) is the precedent. Its statistics must be computed over visible documents, not summed from engine counters.
- **Rule 3, rule 4, rule 8, rule 9, rule 10 — neutral** for this question.
