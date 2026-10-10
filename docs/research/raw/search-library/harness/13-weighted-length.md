# 13 — `--dl weighted`: a column-weighted document length for product modes

Version: tantivy 0.26.2. Build: `CARGO_TARGET_DIR=target-next3` (`rust/target-next3/release`).

## Definition
- `--dl total` (the default) keeps today's length: the `token_count` column, or `token_count_code` in product-code.
- `--dl weighted` uses dl = Σ_c weight(c) × len(c) over name, qualified_name, signature, path and body, with weights 5/5/2/2/1, the same weights as the BM25F frequency. avgdl is the mean of that sum over live, visible documents (`corpus_stats_of`).
- Applies to product, product-code, product-block and product-topk. product-topk needs `tblock <dir> --dl weighted`, whose block files hold the weighted minimum length.

## What is stored (`tindex --column-lengths`)
- Five u64 fast fields, `len_name`, `len_qualified_name`, `len_signature`, `len_path` and `len_body`, each a column's EXACT token count computed at index time, not decoded fieldnorms.
- The count uses the index's own tokenizer: unicode61 normally, or on a `--tokenizer code` index the code tokenizer with its stacked camelCase parts, as its postings and `token_count_code` count them.
- Product reads them through `product::DlCol` (one column, or the weighted sum of five).
- Fixture cost: +5 fast fields; plain build 108 ms vs 114 ms without (noise).

## Proofs on the fixture (`<bench>/smoke/`, capped, niced)
- **`--dl total` is today's output.** The script `source/smoke/same-dl.sh` runs today's product (`target-next2`) and `target-next3`'s product, with no flag and with `--dl total`. Indexes: plain 6 segments, post-delta, code, and both column-length indexes; each plain and filtered.
  - Result: 570 result lines per build, identical in every field except `wall_ms`.
  - `--dl total` is not recorded in `variant` or the phase, so the lines and file names are the same.
- **`--dl weighted` is exact.** `tcheck --dl weighted` computes the reference's weighted length from its own per-column token counts. It gives 19/19 on 1 and 6 segments, 19/19 filtered, and 19/19 on the code index (`--code --dl weighted`). `--dl total` on the same indexes also gives 19/19.
- **Other routes:**
  - `product-topk --dl weighted` matches `product --dl weighted` 18/18.
  - `--search-threads 4` matches 18/18.
  - Weighted changes the ranking: 0/18 pages equal to total, Jaccard 0.73.

## Fixture quality (MRR@10 / R@200; synthetic vocabulary, so only the direction is a hint)
| arm | name | doc | subword | ident |
|---|---|---|---|---|
| product total | 0.959 / 1.000 | 0.135 / 0.992 | 0.756 / 0.563 | 0.273 / 0.281 |
| product weighted | 0.967 / 1.000 | 0.003 / 0.239 | 0.701 / 0.557 | 0.273 / 0.273 |
| product-code total | 0.936 / 0.994 | 0.114 / 0.985 | 0.756 / 0.965 | 0.578 / 0.707 |
| product-code weighted | 0.956 / 0.998 | 0.002 / 0.214 | 0.736 / 0.960 | 0.602 / 0.706 |
- Weighting the length by 5 for names makes a document with a long name or qualified name look long. That penalises a body match against a large dl, which the fixture's `doc` band shows sharply. The kubernetes sets decide.

## Kubernetes commands
They ran as owner run 3 (`../runs/05-variants.md`, script `../runs/r2-logs/f-weighted.sh`). From `<bench>/`, with `T=rust/target-next3/release`:
```
systemd-run --user --scope -q -p MemoryMax=12G $T/tindex docs.jsonl idx/collen 16 large end --column-lengths
systemd-run --user --scope -q -p MemoryMax=12G $T/tindex docs.jsonl idx/collen-code 16 large end --column-lengths --tokenizer code
for s in name doc subword; do
  for dl in total weighted; do
    systemd-run --user --scope -q -p MemoryMax=8G $T/tquery idx/collen qual/queries-$s.txt 1 product --dl $dl --phase q-$s-$dl
    systemd-run --user --scope -q -p MemoryMax=8G $T/tquery idx/collen-code qual/queries-$s.txt 1 product-code --dl $dl --phase q-$s-$dl
  done
  $T/teval qual/qrels-$s.tsv qual/queries-$s.txt results/tantivy-product-q-$s-*.jsonl results/tantivy-product-code-q-$s-*.jsonl
done
```
Set `TB_RESULTS=<dir>` on each command to keep these lines apart from earlier ones. The `--dl total` runs on the same indexes are the controls; they must equal the earlier product and product-code quality numbers.
