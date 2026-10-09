# 09 — Why library mode collapses on the `doc` band

Version: tantivy 0.26.2. Run on the generated fixture (`<bench>/smoke/`, 4 800 documents), because the kubernetes indexes were not available to this probe. The collapse reproduces there: on the 520 `doc` queries, library MRR@10 is 0.000 and product's is 0.135, while R@200 is 0.85 against 0.99 (`teval`, 10-variants.md).

## Method
`tquery smoke/idx… q-doc5.txt 3 library --explain 10 --explain-qrels qrels/qrels-doc.tsv` on the first 5 `doc` queries. It writes the explanation of each of the top 10 hits and of each relevant document, along with that document's exhaustive rank (1 + the documents scoring strictly above it). The explanation is the Weight's own `explain` (`src/query/weight.rs`, the `Weight` trait), and its value equals the collected score on 360 of 360 hits checked. `source/smoke/diag.py` then sums the contributions per field.

## Result (per query: rank-1 hit, rank-10 hit, target)
| query | top-1 score (fields) | rank-10 score | target rank, score (fields) |
|---|---|---|---|
| `xylophone of zebra true` | 59.1 (qn 23.2, name 22.6, sig 10.5) | 52.0 | 208, 6.2 (body only) |
| `naive true ctx deltachange0` | 63.4 (qn 27.6, name 22.6, sig 13.2) | 52.9 | 188, 6.4 (body only) |
| `quokka proxy kube resume` | 78.0 (qn 42.0, name 26.8, sig 9.2) | 72.4 | 206, 6.3 (body only) |
| `struct 日本語 of int64` | 60.6 (qn 26.7, name 21.7, sig 10.6) | 52.3 | 179, 6.1 (body only) |
| `queue quokka proxy 日本語` | 56.5 (qn 26.4, name 21.4, sig 5.4) | 52.1 | 182, 6.2 (body only) |

## Cause: per-field IDF and the near-empty name field
- A `doc` query is made of the target's body words that are not in its name, path or qualified name (`tqrels`). The target therefore scores in `body` only: four terms at about 1.5 each, because each word sits in about 1 700 bodies (idf ≈ 1.04).
- The same words are rare in `name` and `qualified_name`: df 22–35 of 4 800, so idf ≈ 5.1–5.4 in those fields. tantivy keeps df per field (`src/query/bm25.rs:95`, `Bm25Weight::for_terms` from the provider's per-term `doc_freq`).
- The name field's average length is 0.68 tokens (many documents have an empty name), so a one-token name scores a saturation of 0.38 at dl 1. Each such hit is worth 4.5 × boost 5 ≈ 22.6 in `name`, plus about 23 in `qualified_name`.
- So one query word that happens to be a name earns about 45 points, against 6 points for the whole body match. Every top-10 document matches one or two query words in name and qualified name; none matches the body the way the target does.
- In product mode (BM25F), df is the union across the five columns (about 1 700 for these words), and the length is the whole document's. A name match is weighted 5× inside one saturation, not handed its own large IDF, so the target stays in reach.

## What the variants say (fixture, 520 queries)
- `library-combined --shape combined` (BM25 over one `all` field) lifts doc MRR@10 from 0.000 to 0.113 (product 0.135). One field means one df and one length, which is the BM25F property that matters here.
- `combined+fields` stays at 0.000, because the per-field terms are still added and still dominate.
- `library-all --stats live-union` (df as the union across the five fields, `library.rs` LiveStats) keeps MRR@10 at 0.000 but lifts R@200 to 1.000. The union df removes the IDF gap. What remains follows from the formula, though no explanation was run for this variant. With a union idf of about 1.04, one name word is worth about 2.2 × 1.04 × 0.38 (saturation at dl 1, avgdl 0.68) × boost 5 ≈ 4.3. That is added again in qualified_name (× 5) and in signature (× 2), so one word matching across the three fields is worth about 9–10. The target's four body words total about 6 (saturation 0.66 at dl 10, avgdl 52). Per-field sums with large boosts still favour a one-word identifier match; BM25F's single saturation over the weighted frequency does not.

## To reproduce on the kubernetes indexes
`TB_RESULTS=<dir> tquery <kubernetes idx> <first 5 lines of queries-doc.txt> 1 library --explain 10 --explain-qrels qrels-doc.tsv`, then `python3 source/smoke/diag.py <dir>/tantivy-library-query-explain.jsonl`.
