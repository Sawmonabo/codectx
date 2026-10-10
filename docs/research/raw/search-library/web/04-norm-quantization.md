# 04: Norm quantization: one-byte document lengths

**Question.**
- What is the measured effect of Lucene's one-byte norms on ranking quality?
  - Before Lucene 7, the norm was the length's 1/sqrt, stored as a SmallFloat.
  - From Lucene 7, the length itself is stored, exact up to a bound.
- What are tantivy's fieldnorm design notes?

**Credibility key.**
- **A:** a peer-reviewed paper, or the engine's own source, javadoc or issue tracker.
- **B:** official documentation, or an engineering write-up by the team that runs the system.
- **C:** a secondary source, used only to locate an A or B source.

---

## Findings

### 1. Lucene before 7.0: 1/sqrt(length) in a 3-bit-mantissa byte

- **The encoding.** In 6.x, `BM25Similarity.encodeNormValue` returns `SmallFloat.floatToByte315(boost / (float) Math.sqrt(fieldLength))`. It decodes through `NORM_TABLE[i] = 1.0f / (f*f)`.
- Source: `BM25Similarity.java` and `SmallFloat.java`, apache/lucene-solr `branch_6_6`. https://github.com/apache/lucene-solr/blob/branch_6_6/lucene/core/src/java/org/apache/lucene/search/similarities/BM25Similarity.java **A.**

**Ported and run here.** The two methods (`floatToByte315` and `byte315ToFloat`) were ported to Python and run. **A.** The arithmetic of the primary source. The decoded lengths:

| Length | 1 | 2 | 3 | 4 | 5 | 6 | 7 | 8–10 | 12–15 | 20 | 30–40 | 100 |
|---|---|---|---|---|---|---|---|---|---|---|---|---|
| Decoded as | 1 | 2.56 | 4 | 4 | 5.22 | 7.11 | 7.11 | 10.24 | 16 | 20.9 | 40.96 | 113.78 |

- Lengths 1 to 40 collapse to **10 distinct values**.
- The worst relative error over lengths 1 to 100 000 is **56%**.

**Measured effect: −0.005 MAP on one collection.**
- Lucene 4.7.2's stock BM25 against an exact-length BM25, on the TREC 2004 Robust collection: MAP **0.2302** against **0.2352**.
- Source: E. Di Buccio, G. M. Di Nunzio, N. Ferro, D. Harman, M. Maistro, G. Silvello, *Unfolding Off-the-shelf IR Systems for Reproducibility*, SIGIR 2015 RIGOR workshop, §2. https://www.dei.unipd.it/~ferro/papers/2015/SIGIR-RIGOR_2015.pdf
- **A−.** Peer-reviewed workshop position paper; one collection; no significance test reported.

### 2. Lucene 7.0 onward (LUCENE-7730): the length itself, exact up to 40

**The change and its stated accuracy.**
- Adrien Grand, 2017: "SimilarityBase and BM25Similarity now encode directly the length (rather than 1/sqrt(length)) in a way that preserves 4 significant bits across the whole integer range and is accurate up to 40."
- Rounding: "Everything works as if the lengths were rounded to the value in this table that is immediately lesser".
- It shipped in 7.0, because the encoding is chosen by the index's creation major version.

**The only quality measurement in the issue: about 1%, on an unnamed corpus.**
- Robert Muir: "I ran some basic relevance tests and it all checks out … I see the typical 1% difference in this corpus that i would see vs using e.g. a 32 bit integer. But for e.g. very small docs users will be much happier and less likely to compalin about the quantization to a single byte."
- No corpus, metric or numbers are given.

**Source and rating.**
- LUCENE-7730, *Better encode length normalization in similarities*, 2017 (now apache/lucene#8781). https://issues.apache.org/jira/browse/LUCENE-7730
- **A** for the design. **B** for the "1%", which is a committer's report without data.

**The code today (Lucene main; 10.5.2 is the newest release, 2026-10-04).**
- `Similarity.computeNorm` returns `SmallFloat.intToByte4(numTerms)`.
- `BM25Similarity` decodes through `LENGTH_TABLE[i] = SmallFloat.byte4ToInt((byte) i)`.
- `intToByte4` stores values below `NUM_FREE_VALUES` (= 255 − `longToInt4(Integer.MAX_VALUE)` = 24) directly. It stores larger values as 24 + a 4-significant-bit float, truncated.
- Sources: `lucene/core/src/java/org/apache/lucene/util/SmallFloat.java`, `.../search/similarities/Similarity.java` and `BM25Similarity.java`, main (fetched 2026-10-08). **A.**

**Ported and run here.** The decode was ported and evaluated. **A.** The arithmetic of the primary source.
- Exact for every length up to **40**.
- Then the decoded length steps through 42, 44, … 56, 60, 64, …
- Examples: 41 → 40, 63 → 60, 100 → 96, 500 → 472, 5 000 → 4 632.
- The worst relative error over lengths 41 to 10⁶ is **11.1%**. The decoded length is always ≤ the true one.

### 3. A controlled study: exact lengths make no significant difference (newswire)

**Setup.**
- "Lucene (accurate)" uses exact document lengths and is "otherwise identical to the Lucene default". The default uses the one-byte post-7.0 length.
- k1 0.9, b 0.4. The tokenization, stopwords and cleanup are fixed across variants.

**Results.**

| Collection | Measure | Lucene (default) | Lucene (accurate) |
|---|---|---|---|
| Robust04 | AP | .2531 | .2533 |
| Robust04 | P@30 | .3102 | .3104 |
| Core17 | AP | .2087 | .2094 |
| Core17 | P@30 | .4293 | .4327 |
| Core18 | AP | .2495 | .2495 |
| Core18 | P@30 | .3567 | .3593 |

- "Both an ANOVA and Tukey's HSD show no significant differences between any variant, on all test collections."
- "Comparing Lucene (default) and Lucene (accurate), we find negligible differences in effectiveness. However, the differences in retrieval time are also negligible": 52 against 55 ms per query on Robust04 in Anserini.
- "We therefore suggest that Lucene might benefit from storing exact document lengths."

**Source and rating.**
- C. Kamphuis, A. P. de Vries, L. Boytsov, J. Lin, *Which BM25 Do You Mean? A Large-Scale Reproducibility Study of Scoring Variants*, ECIR 2020, LNCS 12036, pp. 28–34, doi:10.1007/978-3-030-45442-5_4, Table 2, Table 3 and §3. https://cs.uwaterloo.ca/~jimmylin/publications/978-3-030-45442-5_4.pdf
- **A.** Peer-reviewed reproducibility study with public code (github.com/Chriskamphuis/olddog).

### 4. tantivy's fieldnorms

**The same scheme as Lucene: checked, and the tables are identical.**
- The module doc says it "encodes `fieldnorm` on one byte with some precision loss, using the exact same scheme as Lucene. Each value is placed on a log-scale that takes values from `0` to `255`." It adds that the scale lets BM25 "precompute computationally expensive functions of the fieldnorm in a very short array".
- `src/fieldnorm/code.rs` holds a 256-entry `FIELD_NORMS_TABLE`. Its test rebuilds the table with `IDENTITY_PART = 24` and the same mantissa decode.
- Encoding is `binary_search(...).unwrap_or_else(|idx| idx - 1)`: round down, as in Lucene.
- **Checked here:** the 0.26.2 table is element-for-element identical to Lucene's `byte4ToInt` table (256/256 entries).
- Source: tantivy 0.26.2, `src/fieldnorm/mod.rs:1-18` and `src/fieldnorm/code.rs`. https://github.com/quickwit-oss/tantivy/blob/0.26.2/src/fieldnorm/code.rs **A.**

**Consequences recorded in tantivy's own source and issues.**

- **BM25 reads the length from the byte.**
  - `compute_tf_cache` precomputes `K1·(1−B+B·fieldnorm/avg)` for the 256 ids.
  - `Bm25Weight::score(fieldnorm_id, term_freq)` reads the cache.
  - The average length is still the exact `total_num_tokens / total_num_docs` (`src/query/bm25.rs:58-69, 111`).
  - **A.**
- **k1 and B are fixed when the index is built.**
  - The maintainer writes: "Tantivy cheats in its blockmax wand computation: we do not store all partial minima for `(term_freq, fieldnorm)` but instead store the single best `(term_freq, fieldnorm)` for a given set of constants `(k1, B)` and a doc freq measure at the scale of the given segment. The `(k1, B)` therefore needs to be known at indexing. The doc_freq part actually makes the result possibly a tiny bit inaccurate."
  - The cause is the per-block maximum, which is stored as a `(freq, fieldnorm_id)` pair.
  - Source: P. Masurel, quickwit-oss/tantivy#2195, *Change const parameter of bm25*, 2023 (open). https://github.com/quickwit-oss/tantivy/issues/2195
  - **A.** Maintainer's statement. The pruning side is covered in file 02.
- **A merge estimates the token total from quantized lengths.**
  - With deletes, `estimate_total_num_tokens_in_single_segment` rebuilds the token total from quantized lengths: `count * id_to_fieldnorm(fieldnorm_ord)` over the live documents.
  - The comment says even the no-delete value "is not necessarily exact as it could have been the result of a merge between segments themselves containing deletes".
  - So after delete-plus-merge, avgdl also carries quantization error.
  - Source: `src/indexer/merger.rs:29-58`, 0.26.2. **A.**
- **No more-precise option exists.**
  - Issue #1438 (open, 2022) proposes only *smaller* fieldnorms, by bit-packing or a constant: "fieldnorms are stored only as an approximation".
  - Fieldnorms can be switched off per field (`set_fieldnorms(false)`), in which case the length is constant.
  - https://github.com/quickwit-oss/tantivy/issues/1438 **A.**

### 5. Exact lengths elsewhere

- SQLite FTS5's `bm25()` reads the exact row size from `xColumnSize(pFts, -1, &nTok)`.
- Source: `ext/fts5/fts5_aux.c`, `fts5Bm25Function`. **A.**

---

**Evidence is thin where:**
- No study measures one-byte lengths on **code**, or on documents as short as codectx's.
- The 41-to-~600 band is where the post-7 encoding loses up to about 11%. It is not isolated in any study.
- The studies use newswire articles, not short code units. Their length distributions were not checked here.
- The "1%" in LUCENE-7730 has no published data.
- The only significance-tested result is ECIR 2020, on three newswire collections.

## What this means for codectx

**Rule 7 (dl = exact token_count) and rule 11 (1e-6 quantization): partly supported.**
- **Exact lengths are not shown to improve quality.**
  - On newswire, exact lengths give no significant gain (ECIR 2020).
  - The pre-7 encoding cost about 0.005 MAP on one collection (RIGOR 2015).
  - The post-7 encoding is exact for lengths up to 40, which covers short fields such as names and signatures.
- **The identity case for exact lengths is strong. Three properties of one-byte lengths conflict with codectx's rules:**
  1. Two documents of different length can score identically. For example, lengths 41 and 40 are the same after encoding, and 64 to 67 all decode to 64. That moves ties onto the tie-break keys (rule 4).
  2. After a delete and a merge, tantivy's avgdl is re-estimated from quantized lengths (§4), so a delta index and a fresh one can differ (rule 2).
  3. The block-max bounds bake in k1, B and the per-segment df (§4).
- **The rule holds, on identity grounds, not quality grounds.**
  - Keep codectx's exact `token_count` as a fast field, and do not use tantivy fieldnorms for codectx's score.
  - Kamphuis et al. found exact lengths cost no measurable time.

**Rule 2 (delta = fresh): supported.** tantivy's merge-time token estimate (§4) is a concrete way a delta index and a fresh index of the same content diverge in avgdl when stock statistics are used.

**Rule 4 (content-only ties): neutral, with a note.** Quantized lengths create more exact score ties. That makes content-only tie-break keys matter more.

**Rule 1, rule 3, rule 5, rule 6, rule 8, rule 9 and rule 10: neutral.** This question does not bear on them.
