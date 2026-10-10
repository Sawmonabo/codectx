# 12 — product-topk: an exact block-max top-k for our BM25F, and threads for product modes

Version: tantivy 0.26.2. Code: `topk.rs`, `tblock`, `tquery product-topk`, `tcheck --topk`, `ttopk --shape product-topk`.

## What is stored (`tblock <dir>`, after the build; one file per segment)
- File: `tb-blockmax-<segment uuid>-<dl>.bin` beside the index, where `dl` is `token_count`, or `token_count_code` on a code index.
- Contents: for every single token in any of the five text columns, its union document frequency in the segment (deleted documents included), and one entry per 128-document block of doc ids that holds it: `(block, wtf_max, dl_min)`.
  - `wtf_max` is the block's maximum of Σ_column weight × tf (weights 5/5/2/2/1).
  - `dl_min` is the block's minimum document length.
- How it is built: one pass that merges the five columns' sorted term dictionaries, so each token's postings are read once (`InvertedIndexReader::terms`, `read_postings_from_terminfo`, `src/index/inverted_index_reader.rs:101,224`).
- Segments are immutable, so a file is valid until a merge replaces its segment; `tblock` must run again after any commit or merge. Phrases are not stored.
- Fixture: 899 tokens, 4 877 blocks, 106 KB, built in 4.8 ms (1 segment).

## The bound
- Our per-term contribution is idf_t × sat(wtf, dl, avgdl), with sat = wtf (k1+1) / (wtf + k1 (1 − b + b·dl/avgdl)) (`bm25_saturation`; idf > 0 always).
- sat rises with wtf and falls with dl. So ub_t(b) = idf_t × sat(wtf_max, dl_min, avgdl) bounds t's contribution to every document of block b; this is the formula given in the brief.
- A block's bound is Σ_t ub_t(b). The floating operations are the same correctly rounded, monotone ones, summed in the same term order. A document's exact sum therefore cannot exceed the block's sum, and the bound adds a 1e-9 relative plus 1e-9 absolute margin before `quantize`.
- A phrase term has no stored entry. Its exact (doc, wtf) list is computed (positions are needed, and its df needs every match), and its block bound is taken from that list.

## The search (two collectors, each through the searcher's executor)
1. **DfCollector** works out df per term:
   - from the side file when the segment has no deleted and no hidden documents;
   - otherwise from a doc-id walk of the five postings lists, counting live, visible documents;
   - for a phrase, from product's own exact list.
2. **TopkCollector**, per segment:
   - Orders blocks by bound, highest first, and scores each block exactly: postings are re-sought from the block's first doc, and the arithmetic is product's, in the same order.
   - Folds survivors by entity on segment ranks.
   - Once the segment holds `depth` = page × 200 entities, θ is the micros of its depth-th entity. A block whose quantized bound is below θ ends the segment, documents below θ are not folded, and entities below θ are dropped.
   - The survivors are resolved to strings and folded globally under cmpScored. Page N is the slice N of the top N × 200.
3. **hits** is product's own count, run apart and untimed, as library mode counts with a separate Count.

## Exactness proof
- **Per segment:** nothing skipped can reach θ, because its micros ≤ quantized bound < θ. Ties at θ are kept and settled by the comparator.
- **Across segments:** a global top-`depth` entity E is in its segment's local top-`depth`. Its best document lies in that segment, and any entity that beats it locally also beats it globally, because an entity's global representative is at least as good as its local one. So the union of local survivors holds the exact global top `depth`.
- **Checked on the fixture:**
  - `tcheck --topk`: 19/19 on 1 and 6 segments, 19/19 with kind and path filters, 19/19 post-delta (3 segments, 267 deleted), and `--code` 19/19 on 1 and 6 segments.
  - `ttopk --shape product-topk` over 812 queries × pages 1–3 = 2 436 pairs per index, on 1, 6 and 3 (post-delta) segments and on code indexes, each with 1 and 4 threads: identical 2 436/2 436 every time.
  - `tquery product-topk`: 18/18 equal to product for page 2 and page 5, filtered, with `--hide-units 10` via the collector route and via the alive set, and with `--search-threads 4`.

## Smoke latency (warm median over queries, ms; 6-segment fixture unless noted)
| run | product | product-topk |
|---|---|---|
| page 1 | 2.05 | 1.07 |
| page 1, `--search-threads 4` | 2.44 | 1.21 |
| page 2 / page 5 | 1.62 / 2.03 | 1.34 / 2.22 |
| kind + path filter | 0.43 | 0.53 |
| `--hide-units 10` | 1.82 | 1.21 |
| code index (product-code vs topk) | 1.79 | 1.03 |
| ttopk page 1, best of 3, 812 queries | 2.18 | 1.09 |
- On the fixture the bound skips only about 2% of blocks (88 605 of 90 312 scored on 6 segments). The synthetic vocabulary puts most query words in about a third of all documents, so block bounds are flat.
- The 2× gain comes from θ keeping most documents out of the fold: no rank-column reads and no map entries for them.
- Deep pages lose the advantage (page 5 is slower). A filter keeps θ low, because only kept documents raise it.
- The real skip rate needs the kubernetes index.

## Threads for product modes
- `--search-threads N` now applies to product, product-block, product-code and product-topk. It calls `Index::set_multithread_executor`, so `Searcher::search` maps segments over the executor (`src/core/searcher.rs:220-236`) and `merge_fruits` folds them.
- That fold uses a global map keyed by entity and the total cmpScored order, then a sort, so the answer does not depend on segment order or timing.
- Checked: 18/18 identical with 4 threads in each product mode, `tcheck --search-threads 4` 19/19, and ttopk with 4 threads identical on 2 436 pairs.
- On the fixture, 4 threads are slower (2.44 vs 2.05 ms), because thread overhead outweighs 6 tiny segments.
