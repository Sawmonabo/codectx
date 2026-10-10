# 08 — Does the library's pruned top-k miss a true top hit on a multi-segment index?

**The claim checked.** "The top-k shortcut can miss a true top hit on a multi-segment index." tantivy's `TopDocs` with a BM25 OR query may prune with block-max WAND (`tantivy-0.26.2/src/query/boolean_query/boolean_weight.rs:516-525`): documents whose upper bound cannot beat the current threshold are skipped. That pruning is meant to be exact.

**The tool.** `ttopk <index dir> <queries> [--search-threads N]`. For each query it runs:
- the library-mode query with `TopDocs::with_limit(200)` (pruned);
- the same query with the exhaustive collector, which scores every match (`library-all`'s collector).

The exhaustive list is ordered two ways:
- by (score desc, DocAddress asc), which is TopDocs' documented tie rule (`src/collector/top_score_collector.rs:26-28`); the pruned page must equal this exactly;
- by (score desc, id asc), which is what library-all serves.

Misses are judged on the exhaustive scores alone:
- **MISS**: a document scores above the lowest-scoring page document, yet is absent from the page;
- **WRONG**: a page document scores below the true 200th score.

`ttopk` also prints each page score that differs between the two scorer paths, and classifies every difference from library-all's page: same set with tie order only; sets differing only among documents tied at the 200th score; or different beyond ties. Its first line names the index: segments, live documents, deleted documents, tokenizer and search threads.

**Smoke result.** 812 queries: the fixture's 19, plus the name, subword and doc known-item sets.

| index | segments | deleted | pruned == exhaustive (score, address) | true misses | scores differing between paths |
|---|---|---|---|---|---|
| per-unit commits, 1 thread | 6 | 0 | 812 / 812 | 0 | 11 (max 1 ulp) |
| per-unit commits, 16 threads | 2 | 0 | 812 / 812 | 0 | 0 |
| one commit | 1 | 0 | 812 / 812 | 0 | 0 |
| after a delta | 3 | 267 | 812 / 812 | 0 | 80 (max 2 ulp) |
| after a delta, `--search-threads 4` | 3 | 267 | 812 / 812 | 0 | 80 (max 2 ulp) |
| after a delta and a forced merge (19 queries) | 1 | 0 | 19 / 19 | 0 | 0 |

**Reading.**
- On these indexes the pruned top-200 was the exact top-200 every time, with no misses, across segment counts, deletes and the multi-threaded executor.
- It differed from library-all's page only through ties (the address vs id tie rule): 495 queries by order alone and 302 by which tied documents filled the last slots, on the 6-segment index.
- The synthetic vocabulary is tiny, so ties are far more common than in real code.
- The scorer-path differences are f32 summation order: the same document gets a score 1–2 ulp apart on the two paths. A pruned page could therefore, in principle, order two near-equal documents differently from an exhaustive ranking.
- The check has to be repeated on the real export (the kubernetes run); this smoke does not settle it.
