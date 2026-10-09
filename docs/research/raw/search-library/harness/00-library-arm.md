# 00 — The library arm (Rust): what the harness offers, its smoke, its departures and its variants
**Version:** `tantivy = "=0.26.2"`, the newest release on crates.io (Sept 8, 2026). No 0.27 release exists.
**Build:** `cd <bench>/rust && nice -n 19 systemd-run --user --scope -p MemoryMax=8G cargo build --release` from the sources in `source/` (pinned in `source/Cargo.toml`, locked in `source/Cargo.lock`). For the sstable term dictionary, add `--features sstable`.
**Run from `<bench>/`** (each under `systemd-run --user --scope -p MemoryMax=4G`). Results go to `./results/`, or to `$TB_RESULTS` if set.
**Evidence:** per-topic notes and a copy of the source are in this folder (01–12, plus `source/` with `source/smoke/` helpers). 10 has every variant's full result, 11 covers `--hide-units`, 09 covers the `doc` collapse, and 12 covers product-topk's bound and its exactness proof.
- `tindex <docs> <dir> <threads> [default|large|MB] [unit|every:K|end] [writer options] [schema options]` (option list in the file header). Phases are `index-t<T>-<budget>-<cadence>[-<tag per option>]{-add,-final,[-persist],}`. The schema options are saved to `<dir>/tb-config.json`.
- `tquery <dir> <queries> <runs> <library|library-all|library-combined|product|product-block|product-code|count|docset|agg-kind|facets> [options]` (option list in the file header). The default phase is `query`, plus one tag per option. Lines carry `variant`, plus `hidden_pct`, `stats_include_hidden` and `hide_build_ms` when hiding.
- `tdelta <docs> <dir> [unit] [--merge] [--delete unit|id|query] [--merge-policy SPEC] [--soft FILE]`: phases `delta-put`, `delta-commit`, `delta`, `delta-merge` (+ option tags); with `--merge`, also `delta-forcemerge`.
- `tblock <dir>` (product-topk's per-segment block-max files; rerun after any commit or merge), `tspace <dir>` (space_usage JSON), `tsnap <src> <dst>` (hard-link snapshot), `tpin <copy> <docs> <queries> [unit]` (held searcher across delta + merge; reload timings).
- `ttopk <dir> <queries> [--search-threads N] [--shape S|product-topk]` (product-topk: identity with product on pages 1–3 of every query, plus timings): library's pruned top-200 vs the exhaustive top-200, per query, as a set and in order; prints every miss and names the index (08).
- `tcompare <a.jsonl> <b.jsonl>`: per query, both hit counts and the |A∩B| overlap of the top 200 and top 10.
- `tqrels <docs> <outdir> <seed>` → `queries-{name,doc,subword,ident}.txt` and `qrels-*.tsv`.
- `teval <qrels> <queries> <results.jsonl>…` → per arm and phase: MRR@10, S@1, S@10, R@200, mean hits, zero-hit queries, missing queries.
- Smoke-only checks: `tgen`, `ttok` + `probes/tokgen`, `tlog` + `probes/golog`, `tcheck <docs> <dir> <queries> [filters] [--code] [--topk] [--search-threads N]` (`--code` checks product-code, `--topk` also checks product-topk). tcheck holds the whole corpus in memory, so it is not for the export.

**Modes** (detail in 03): the query is built once and shared by both library modes.
- `library`: tantivy BM25 with `TopDocs(200)` and stored-document id fetches; hits come from an untimed `Count`.
- `library-all`: the same BM25, but a custom collector keeps every (doc, score). It sorts by score desc then id and serves 200; hits = raw documents.
- `product`: our rules, on unicode61 indexes only. `product-code`: the same rules on a `--tokenizer code` index; terms gain camelCase parts and the length is `token_count_code`.
- `product-topk`: product's exact page through block-max skipping (12). It follows the index's tokenizer, needs `tblock`, and `--page N` serves the top N·200. Its hits are product's count, taken apart and untimed. `--search-threads N` now applies to every product mode, with identical output.
- `library-combined`: library-all's collection over `--combined`'s `all` field (`--shape combined`), or `all` SHOULD the five boosted fields (`combined+fields[:W]`).
- `--kind` and `--path-prefix` apply to all modes. The path rule is whole folders: `P` itself or under `P/`, so `doc` never matches `docs/`.
- `--search-threads` applies to the library modes only. Lines carry `filter` and `search_threads` only when those options are used.

**Product extension points** (06): a custom `Collector` with `collect_segment` overridden, `read_postings` with freqs and positions, the alive bitset, fast-field rank columns, and `sorted_ords_to_term_cb`. The stock `Bm25StatisticsProvider` keeps df per field and counts deleted documents, and `Score` is f32, so neither can reproduce our scores.

## Smoke (generated fixture `<bench>/smoke/`: 4 800 docs, 510 units)
- Tokenizer: tables probed from the product's own SQLite 3.53.4 (0 anomalies). Text diff over 303 391 tokens: identical except the mid-character 32 768-byte cut (01).
- `go_log` matches Go bit for bit on 200 127 values, where `f64::ln` differs on 1 891 (02).
- `tcheck`: product mode equals the reference on 18/18 queries (1- and 2-segment indexes), 18/18 with kind and path filters, and 19/19 after the delta.
- Commits per unit cost about 18 ms each at 1 thread (9.2 s total) and about 66 ms at 16 (33.9 s). Committing once: 106 ms and 248 ms (04).
- Warm median: library 0.15–0.32 ms; library-all 1.5–2.9 ms; product 0.62–3.3 ms.
- Filtered runs (`--kind function --kind method --path-prefix pkg/api`): same hits in all three modes ("the": 16).
- Path rule: `doc` → 0, `docs` → 534 raw / 436 folded, an exact file → 12 in every mode. `--search-threads 4` runs.
- `--page 1/2/5/10`: library warm 0.16–0.22 ms for every page; library-all 1.6 ms (p1) to 12.4 ms (p10). `tdelta --merge`: 7 segments with 267 deleted → 1 with 0, in 80 ms.
- `ttopk`, 812 queries, on 1-, 2-, 3- (post-delta) and 6-segment indexes: pruned page equals exhaustive (score, address) on 812/812, 0 true misses. Same-document scores differ by 1–2 ulp between the two paths (up to 80 page scores); library vs library-all differ only by ties.
- `tcompare` library-all vs product: "the" 1 799 vs 994 hits; top-200 overlap 165/200, top-10 overlap 7/10.
- `tqrels` seed 7: 232 name, 520 doc, 41 subword queries. Subword R@200: unicode61 library 0.56, code library 0.97 (the pipeline works; a synthetic vocabulary says nothing about quality).

## Departures (full list in 05)
1. Lexical tier only. 2. The over-long-token cut. 3. Extra fast fields counted in disk bytes. 4. Cold = fresh process, page cache warm. 5. Per-unit commit ≠ seal. 6. Product cold includes the `token_count` scan.
7. Library modes: per-field BM25 whose statistics include deleted documents, raw-document hits, phrases not capped at 512, different tie rules.
8. Subword: unicode61 already splits snake_case, so only camelCase parts need the code variant. The code variant puts parts at their token's position, which inflates fieldnorms and lets phrases span identifiers.

## Variants (one row each; full numbers in 10 and 11)
| variant | flag / tool | note | tests | metric it moves |
|---|---|---|---|---|
| threads, budgets | `tindex <T> <MB>` | I1, I2 | segment spread | IT, segments, RSS |
| merge threads / knobs | `--merge-threads`, `--merge-policy log:…` | I3, I4 | merge cost | IT, segments |
| NoMerge + final | `--merge-policy none+final` | I5 | one final merge | IT, CPU |
| cadence | `every:K` | I6 | commit cost | IT |
| batched ops | `--batch unit` | I8 | `run` per unit | IT |
| delete routes | `tdelta --delete unit\|id\|query` | I9, I10 | delete cost | delta ms |
| single-segment writer | `--single-segment` | I11 | one-shot build | IT, RSS |
| RAM then persist | `--ram-then-persist` | I12 | RAM build | IT, RSS |
| docstore | `--docstore none\|lz4\|zstd:L`, `--blocksize` | F1, F2 | store size | disk, fetch ms |
| store cache | `tquery --doc-cache N` | F3 | cache hits | warm ms |
| no STORED / lean | `--store none`, `--schema lean`, `--id-source fast` | F4, §8 | fast-field ids | disk, warm ms |
| fieldnorms | `--fieldnorms text-only\|none` | F5 | norm cost | disk, pages |
| body freqs only | `--body-record freqs` | F6 | positions cost | disk, phrase answers |
| fast-only kind | `--kind-fast-only` | F7 | fast-field filter | disk, filter ms |
| space report | `tspace` | F9, RT6 | bytes per component | disk |
| sstable termdict | `--features sstable` | F10 | termdict format | disk, ms |
| tokenizers | `--tokenizer default\|simple\|en_stem\|code\|pretokenized` | T2, T7, T9, T10 | analysis | hits, quality, IT |
| side fields | `--parts-fields`, `--name-ngram` | QD, T4 | partial names | quality, disk |
| query shapes | `--shape flat\|dismax:T\|msm\|fuzzy\|phrase-prefix:N\|parser` | Q1, Q4, Q6, Q8, Q11, Q15 | scoring/pruning | ms, pages, quality |
| slop / weights | `--slop N`, `--weights a/b/c/d/e` | Q5, QD | phrase/boosts | hits, pages |
| kind filter routes | `--kind-filter terms\|termset\|bytes\|collector\|code` (+`--kind-code`) | Q3, Q13, C5 | filter cost | ms, hits |
| path filter routes | `--path-filter regex\|range\|ffrange\|invrange\|facet\|collector` (+`--path-facet`) | Q7, Q9, C8 | filter cost | ms |
| collectors | `--collect tuple-count\|tuple-id\|tweak` | C4, C6, S5 | one pass, ties, hook | ms, determinism |
| statistics | `--stats live\|live-union` (+`--field-counts`) | S2 | alive-only BM25 | pages |
| explain | `--explain K [--explain-qrels F]` | S8, 09 | cost, value check | ms |
| block-max checks | `ttopk --shape S` | S4, Q14, Q15 | pruned vs exhaustive | misses, ulps |
| search threads | `--search-threads N` | RT1 | segment parallelism | warm ms |
| warmer | `--warm-stats`, `--time-open` | RT2 | stats at open | cold ms, open ms |
| madvise | `--madvise normal\|random\|sequential\|willneed` | RT5 | mmap advice | cold ms |
| count / docset | modes `count --count-via`, `docset --docset-via` | C6, C7 | counting routes | ms |
| aggregation / facets | modes `agg-kind --agg`, `facets --facets` | C8, C9 | per-kind / per-folder counts | ms |
| product-block | mode `product-block` | S7 | candidates from Weight | ms |
| product-code | mode `product-code` (+ `token_count_code`) | quality results | BM25F + code tokens | quality |
| library-combined | `--combined`, mode `library-combined` | quality results | one-field BM25 | quality |
| visibility | `--hide-units PCT --hide-seed S --hide-via weight\|collector\|alive-set`, `--hide F` | PR | retained snapshots in one index | ms, build ms |
| soft delta | `tdelta --soft F` | PR | delta by hiding | delta ms |
| snapshot / pin | `tsnap`, `tpin` | PR (a) | retained generations | ms, equality |
| ident band | `tqrels` | QD | identifier queries | quality |

**Not implemented, and why:** I7 commit payload (functional only; nothing to time). I13 index sorting (removed in 0.24). F8 Coerce, T8 SplitCompoundWords, Q10 Exists, Q16 MoreLikeThis, C2 order by fast field (marked "—", no bench question). T3 Regex tokenizer (its proposal is a later quality band only). T6 RemoveLong is a corpus count, not a variant: 2 of the fixture's 303 391 tokens (from `ttok`) are 40 bytes or longer, and the `default` and `en_stem` variants would drop them. RT7 snippets (no body stored; the bench measures no previews). PR design (b), a retaining `Directory`: it is covered by (a) `tsnap` plus `tpin`'s held searcher, which shows tantivy already keeps a held generation readable across a merge. C8 Histogram (no numeric question).

**Smoke of the variants (detail in 10 and 11):**
- Every variant runs.
- Baseline answers equal the previous binary (library, library-all, product, plain and filtered).
- tcheck passes 19/19 on every unicode61 variant except body-freqs (14/19, phrases).
- product-code passes 19/19 against its own reference.
- `--hide-units` 0/10/50 matches a visible-only index exactly in product mode.

**Findings:**
- `BytesFilterCollector` silently drops every document on a STRING fast field.
- `TermQuery`'s fast-field fallback works only with scoring off.
- Merges estimate token totals from fieldnorms.
- f32 scores move by ulps across segment layouts.
- The `doc` collapse comes from per-field IDF on a near-empty name field (09).

## Product-topk and product threads (note 12)
| variant | flag / tool | tests | smoke vs product (warm ms, 6 segments) |
|---|---|---|---|
| product-topk | `tblock <dir>`, mode `product-topk [--page N]` | exact block-max top-k for our BM25F | page 1: 1.07 vs 2.05; page 5: 2.22 vs 2.03; filtered 0.53 vs 0.43; code index 1.03 vs 1.79 |
| product threads | `--search-threads N` on product, product-block, product-code, product-topk | segments over the executor, merged under our comparator | 4 threads: 2.44 vs 2.05 (product); topk 1.21 |

Exactness:
- `tcheck --topk` 19/19 on 1 and 6 segments, filtered, post-delta, and on code indexes.
- `ttopk --shape product-topk`: 2 436/2 436 identical (query, page 1–3) pairs on every index, with 1 and 4 threads.
- Threads give identical output in every product mode.
- On the fixture the bound skips only about 2% of blocks; the gain is θ keeping documents out of the fold. The kubernetes run decides the real skip rate.
