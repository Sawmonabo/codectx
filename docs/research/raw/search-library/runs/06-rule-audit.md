# Rule audit on the kubernetes export (Oct 8 2026) — partial: item 3 and part of item 5 are unmeasured (owner ruling, Oct 8)
Each product rule the library departs from, judged on what it buys a user and what it costs. Evidence is from round 1
(`02`-`04` here) and round 2 (`05-variants.md`, logs in `r2-logs/`). Quality: teval, MRR@10 / R@200, known-item
sets of 1 000 queries (seed 20261008). Speed: the 30-query set, sum of warm medians.

## 1. BM25F (one saturation over weighted fields) vs the library's per-field sum
- Buys: the doc band. Our ranking: doc 0.684 / 0.982; the library's per-field sum (`library-all`): 0.023 / 0.516.
  The cause (harness 09): per-field IDF on near-empty name fields hands a one-word name match about 45 points
  against about 6 for the whole body match. Union document frequencies alone (`--stats live-union`) give 0.034:
  the single saturation is what matters.
- Costs: the name and subword bands. library-all name 0.907 vs ours 0.833; subword 0.602 vs 0.379; ident 0.242 vs
  0.221. First-page overlap library-all vs product: top-10 0.60 (speed), 0.75 (name), 0.19 (doc), 0.58 (subword).
- In the library, BM25 over one combined field reproduces BM25F's numbers (name 0.831, doc 0.683, subword 0.381)
  for +104 MB of disk; equal boosts 1/1/1/1/1 lift the per-field sum's doc band only to 0.265.
- Verdict: **keep** the single saturation; the doc band collapse (0.68 -> 0.02) is far larger than the name gain
  (0.07). The name and subword gains say the name columns deserve more weight than 5/5 inside BM25F; that is a
  tuning question to measure next, not a reason to drop the rule.

## 2. No top-k vs exact top-k pruning
- The library's pruned top-200 equals its exhaustive top-200 by (score, address) on 30/30 and 1 000/1 000 queries
  for the nested shape on 2 and 11 segments (round 1), and for dismax, msm and fuzzy on both (round 2). The flat and
  parser shapes report "misses" on 2 of 30 queries (11 segments) and 45 of 1 000 name queries: every one is a document
  1-2 ulp above the page's lowest pruned score, a tie broken by f32 summation order, not a lost document.
- Cost of no top-k: library top-200 16.0 ms; exhaustive library-all 129.3 ms; product 4 039 ms (round 1).
  Paging by an increasing k: pages 1, 2, 3, 5, 10 cost 15.9, 16.2, 16.5, 16.4, 18.3 ms summed (round 1).
- Under our own scoring (owner run 2, `product-topk`: block-max skipping that keeps ties at the threshold and settles
  them by our comparator): pages identical to exhaustive product on 3 030 of 3 030 queries (30 speed + 3 × 1 000
  quality). Warm sum on the 30 queries 4 653 -> 296 ms (15.7x; "vendor" 876.6 -> 8.7 ms); doc set 85 488 -> 14 682 ms;
  67.7% of blocks skipped on the 30 queries. With 4 search threads: 185 ms. Cost: a 75.5 MB block-max side file per
  index (2 segments), 1.46 s to build, rebuilt after every commit or merge.
- Verdict: **change**. "Score every match" buys nothing the exact pruned top-k does not return: the pages are
  identical. Keep the exactness requirement (the complete ordered page and our tie-break), drop the exhaustive scan.

## 3. Exact statistics vs statistics that count deleted documents
- Not measured: the drift comparison (post-delta pages vs a fresh index, before and after a forced merge) was cut.
  What exists: a 9 460-document delta leaves 3 segments with 9 460 deleted documents counted in the library's
  statistics; a forced merge removes them in 12.4 s (07-delta.md). The fresh post-delta index was built (tindex
  validated every token count: 0 mismatches) but not queried.
- Verdict: **open**.

## 4. f64 with quantization vs f32
- f32 is not stable across builds of the same documents: library-all, which breaks ties by id, serves different
  pages on 2 vs 11 segments for 376 of 1 000 name queries, 799 of 1 000 doc and 572 of 1 000 subword (sets differ on
  53, 114 and 26). The library's own TopDocs order (ties by address) differs on 680, 991 and 963 pages.
- Our product mode (f64, quantized, canonical tie-break) gives 30/30 identical pages on 2 vs 11 segments.
- Ranking quality barely moves (library MRR@10 within 0.0015 across layouts), so the cost of f32 is determinism:
  the same documents give a different page after a rebuild or a merge.
- Verdict: **keep** f64 with quantization, as the rule that makes pages reproducible; quality alone would not
  require it.

## 5. Exact document length vs the one-byte fieldnorm
- Measured in the library: fieldnorms on vs off changes quality (name 0.901 -> 0.867, subword 0.602 -> 0.499 with
  them off), so length normalization matters; `text-only` fieldnorms change nothing.
- Not measured: our scoring with one-byte quantized lengths in place of exact ones (no harness route). The exact
  length costs one fast column (token_count, 0.6 MB on 546 613 documents). Merges in the library estimate token
  totals from fieldnorms when a segment has deletes (harness 10, finding 3), so a library index is not an exact
  reference after deletes.
- Verdict: **keep** (the exact length is nearly free); the one-byte alternative is unmeasured.

## 6. The tokenizer (unicode61 vs the code-aware variant)
- In the library, the code index lifts subword R@200 0.218 -> 0.661 and ident MRR@10 0.242 -> 0.535, at +33 MB
  (+9.4%) disk; name +0.007, doc +0.022.
- Under our scoring (owner run 1, `product-code` vs `product`): subword MRR@10 0.379 -> 0.500, R@200 0.214 -> 0.630,
  zero-hit queries 13 -> 0; name 0.833 -> 0.810 (R@200 0.946 -> 0.920); doc 0.684 -> 0.676. Mean hits grow 5.6x on
  name (9 096 -> 50 583), because camelCase parts match far more documents.
- Verdict: **change**, adopt the code-aware tokenizer: the subword gain (+0.12 MRR@10, +0.42 R@200) is about five times
  the name loss (-0.023), and weighted length (rule 7) recovers the name loss and more.

## 7. Unweighted document length (token_count) vs a column-weighted length (owner run 3)
- dl = Σ weight(c) · len(c) with the BM25F weights (5/5/2/2/1). Under product: name +0.024 (0.857), subword +0.055
  (0.435), doc -0.014 (0.670). Under product-code: name 0.856, subword 0.559 / R@200 0.644, doc 0.648 (-0.037 vs
  product). The `--dl total` controls reproduce the earlier pages exactly (1 000/1 000 per set and arm).
- Cost: five exact per-column count fast fields (index 355 MB vs 353.5 MB).
- Verdict: **change**, with care: it is the best measured arm on name and subword, but it costs the doc band 0.014
  (unicode61) to 0.037 (code). A weighting of the length less steep than the frequency weights is the next thing
  to measure; the decision belongs with the tokenizer change.
