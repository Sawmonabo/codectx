# Round 2: variants on the kubernetes export (Oct 8 2026)

Library 0.26.2, harness `<bench>/rust` built as `target-next` (and `target-sstable` for the sstable term dictionary; `target-next2`
for the owner's runs 1-2 and `target-next3` for run 3). Every run: a clean environment (`env -i` with HOME, PATH, USER, XDG_RUNTIME_DIR,
DBUS_SESSION_BUS_ADDRESS, TMPDIR), under `systemd-run --user --scope -p MemoryMax=12G`, one at a time, with no other
heavy workload on the host. `free -g` was checked before every index build (27-28 GB available
throughout; the 8 GB stop was never reached). Every command line, exit code and output line is in `r2-logs/`
(`a-index.log` builds, `b-query.log` query variants (not published: 20.8 MB; its figures are the tables below), `c-audit.log` delta and drift, `e-owner.log` the owner's runs);
the runner scripts are `r2-logs/*.sh`. Result files are `<bench>/results/tantivy-<mode>-<phase>.jsonl` with the phase
names below; index builds are `tantivy-index-<label>*.jsonl`, and `<bench>/space/<index>.json` holds each index's
`space_usage`. Host load every 30 s is in `r2-logs/host-load-r2.tsv` (1-minute load 2.3 to 11.3 on 16 cores during the runs).

## Never ran (owner ruling, Oct 8)
The owner limited the round to three further runs (code-aware tokenizer quality under our scoring, early stopping, weighted
length). Every index build (area A) and every query variant (area B) ran. These did not:
- the rule-audit drift comparison: tdelta on copies A-F ran, and delta-A's library speed, library qual sets, library-all
  speed and product speed ran; the library-all quality runs on delta-A, everything on delta-B (merged) and on the
  fresh post-delta index, the `--stats live` drift runs, and the soft-delta `--hide` pages never ran (06, 07);
- our engine's delta (`tbdelta`), so <bench>/store is unchanged, and the like-for-like delta table (07);
- the doc-collapse explain run (harness 09);
- the rebuild of the code index for `product-code` speed and the noise rerun of the baselines.
The owner's three runs are at the end of this file.

## How to read the tables
- Speed: the 30-query set, 1 cold run (fresh process) + 5 warm runs; "sum of warm medians" adds each query's warm
  median. "ours / variant" divides our engine's warm sum on the same queries (47.4 s unfiltered, from round 1) by the
  variant's. The baseline library run is round 1's (`tantivy-library-speed`, t16-large-end, 16.0 ms).
- Noise: sub-millisecond medians on a shared host. Indexes that hold the same data in the same 2-segment layout
  (`t16-large-end-ram` 15.6 ms, `t2-large-end` 14.1 ms, `fn-textonly` 14.6 ms) span about ±15% around the baseline,
  so a library speed delta under about 20% is noise. The 1-segment indexes are all slower than the 2-segment baseline
  (t8-large-end +57%, e1000-nonefinal +55%, t1-large-end-single +27%); single runs cannot tell whether that is a
  one-segment effect or larger noise.
- "same as baseline": hits equal; pages identical in order; pages equal as sets; mean top-10 overlap. The library's
  TopDocs breaks score ties by document address, so any index with a different document layout shows 5/30 identical
  pages and about 14/30 equal sets against the baseline with no change in ranking (t2-large-end, the RAM build and
  the merge variants all do). A ranking change shows as a lower top-10 overlap (fn-none 0.51, tok-enstem 0.73).
- Quality: teval on the known-item sets (seed 20261008; name, doc, subword 1 000 queries each; ident 56), 1 run.
  Cells are MRR@10 / R@200, then the MRR@10 delta against our engine and against the baseline library run.
  Our engine: name 0.8331 / 0.9460, doc 0.6842 / 0.9820, subword 0.3793 / 0.2130, ident 0.2208 / 0.4025.

## Index variants: build cost and bytes (one build each; `tspace` components in MB)
Merge-policy and cadence rows are against `t16-default-every1000` (48 commits); the rest against `t16-large-end`.
| index | args | wall s | CPU s | peak RSS GB | commits | segments | disk MB | termdict | postings | positions | fast | fieldnorms | store | vs baseline |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| t16-large-end | index-t16-large-end | 13.1 | 57.9 | 1.26 | 1 | 2 | 353.5 | 121.4 | 53.0 | 58.3 | 61.2 | 7.7 | 52.0 | baseline |
| t2-large-end | index-t2-large-end | 9.8 | 22.2 | 1.03 | 1 | 2 | 339.9 | 121.4 | 47.1 | 56.3 | 60.8 | 7.7 | 46.5 | wall -25%, disk -3.9% |
| t4-large-end | index-t4-large-end | 5.4 | 22.5 | 1.00 | 1 | 4 | 357.4 | 132.6 | 48.4 | 57.4 | 63.2 | 7.7 | 48.1 | wall -59%, disk +1.1% |
| t8-large-end | index-t8-large-end | 17.4 | 43.5 | 1.15 | 1 | 1 | 336.9 | 111.4 | 51.5 | 57.2 | 59.1 | 7.7 | 50.0 | wall +33%, disk -4.7% |
| t16-256-end | index-t16-256-end | 11.1 | 59.9 | 0.57 | 1 | 13 | 342.3 | 121.7 | 48.5 | 57.2 | 59.7 | 7.7 | 47.6 | wall -16%, disk -3.2% |
| t16-1024-end | index-t16-1024-end | 12.7 | 55.6 | 1.21 | 1 | 5 | 354.4 | 122.9 | 52.8 | 58.4 | 61.3 | 7.7 | 51.4 | wall -3%, disk +0.2% |
| t16-large-end-ram | index-t16-large-end-ram | 13.9 | 57.4 | 2.03 | 1 | 2 | 353.7 | 121.5 | 53.0 | 58.3 | 61.2 | 7.7 | 52.0 | wall +6%, disk +0.1% |
| ds-none | index-t16-large-end-none | 12.6 | 55.0 | 1.25 | 1 | 2 | 433.4 | 121.4 | 53.0 | 58.3 | 61.2 | 7.7 | 131.9 | wall -4%, disk +22.6% |
| ds-zstd3 | index-t16-large-end-zstd3 | 12.8 | 57.0 | 1.27 | 1 | 2 | 334.6 | 121.4 | 52.9 | 58.3 | 61.2 | 7.7 | 33.0 | wall -2%, disk -5.4% |
| ds-zstd9 | index-t16-large-end-zstd9 | 13.5 | 60.9 | 1.28 | 1 | 2 | 333.7 | 121.4 | 53.0 | 58.3 | 61.2 | 7.7 | 32.1 | wall +3%, disk -5.6% |
| ds-zstd3-bs64k | index-t16-large-end-zstd3-bs65536 | 13.3 | 57.5 | 1.27 | 1 | 2 | 332.2 | 121.4 | 52.9 | 58.3 | 61.2 | 7.7 | 30.7 | wall +2%, disk -6.0% |
| ds-zstd3-bs256k | index-t16-large-end-zstd3-bs262144 | 13.1 | 57.9 | 1.29 | 1 | 2 | 333.8 | 121.5 | 53.0 | 58.3 | 61.2 | 7.7 | 32.2 | wall -0%, disk -5.6% |
| store-none | index-t16-large-end-nostore | 12.8 | 55.3 | 1.26 | 1 | 2 | 303.7 | 121.4 | 53.0 | 58.3 | 61.2 | 7.7 | 2.2 | wall -2%, disk -14.1% |
| lean | index-t16-large-end-nostore-fntextonly-leannum-leanstr | 10.0 | 42.7 | 0.97 | 1 | 2 | 228.2 | 57.3 | 46.5 | 58.3 | 61.2 | 2.7 | 2.2 | wall -24%, disk -35.4% |
| fn-textonly | index-t16-large-end-fntextonly | 13.1 | 56.5 | 1.25 | 1 | 2 | 348.9 | 121.4 | 52.9 | 58.2 | 61.2 | 3.3 | 51.9 | wall +0%, disk -1.3% |
| fn-none | index-t16-large-end-fnnone | 12.7 | 56.2 | 1.25 | 1 | 2 | 346.3 | 121.4 | 52.9 | 58.3 | 61.2 | 0.5 | 51.9 | wall -3%, disk -2.0% |
| body-freqs | index-t16-large-end-bodyfreqs | 12.8 | 56.9 | 1.17 | 1 | 2 | 300.8 | 120.3 | 52.6 | 7.2 | 61.2 | 7.7 | 51.9 | wall -2%, disk -14.9% |
| kind-fast | index-t16-large-end-kindfast | 13.0 | 58.4 | 1.26 | 1 | 2 | 351.0 | 121.4 | 52.5 | 58.2 | 61.2 | 7.1 | 50.5 | wall -1%, disk -0.7% |
| kindcode-facet | index-t16-large-end-facet-kindcode | 14.9 | 63.4 | 1.33 | 1 | 2 | 364.2 | 122.1 | 57.3 | 58.3 | 66.9 | 7.7 | 52.0 | wall +13%, disk +3.0% |
| sstable | index-t16-large-end | 7.8 | 36.2 | 1.21 | 1 | 2 | 292.7 | 60.5 | 53.0 | 58.3 | 61.2 | 7.7 | 52.0 | wall -41%, disk -17.2% |
| tok-default | index-t16-large-end-default | 12.8 | 53.6 | 1.24 | 1 | 2 | 350.2 | 118.6 | 52.8 | 58.1 | 61.2 | 7.7 | 51.9 | wall -2%, disk -0.9% |
| tok-simple | index-t16-large-end-simple | 12.9 | 53.7 | 1.25 | 1 | 2 | 353.4 | 121.4 | 52.9 | 58.2 | 61.2 | 7.7 | 51.9 | wall -2%, disk -0.0% |
| tok-enstem | index-t16-large-end-enstem | 12.4 | 62.7 | 1.23 | 1 | 2 | 347.6 | 118.0 | 50.8 | 57.7 | 61.2 | 7.7 | 52.2 | wall -6%, disk -1.7% |
| tok-pretok | index-t16-large-end-pretokenized | 20.2 | 69.5 | 1.26 | 1 | 2 | 353.6 | 121.4 | 53.0 | 58.3 | 61.2 | 7.7 | 51.9 | wall +54%, disk +0.0% |
| parts | index-t16-large-end-parts | 14.5 | 59.8 | 1.29 | 1 | 2 | 360.8 | 121.8 | 57.4 | 59.6 | 61.2 | 8.7 | 52.0 | wall +11%, disk +2.1% |
| ngram | index-t16-large-end-ngram | 13.6 | 57.9 | 1.30 | 1 | 2 | 364.5 | 121.7 | 63.1 | 58.2 | 61.2 | 8.2 | 52.1 | wall +4%, disk +3.1% |
| counts | index-t16-large-end-counts | 15.6 | 60.3 | 1.27 | 1 | 2 | 355.4 | 121.5 | 53.0 | 58.3 | 63.1 | 7.7 | 51.9 | wall +19%, disk +0.5% |
| combined | index-t16-large-end-combined | 15.9 | 74.5 | 1.56 | 1 | 2 | 457.3 | 131.4 | 84.3 | 120.1 | 61.2 | 8.2 | 52.1 | wall +22%, disk +29.3% |
| code-combined | index-t16-large-end-code-combined | 21.9 | 93.0 | 1.68 | 1 | 2 | 518.3 | 133.8 | 101.1 | 161.3 | 61.9 | 8.2 | 52.0 | wall +67%, disk +46.6% |
| t16-large-end-code | index-t16-large-end-code | 13.3 | 67.0 | 1.36 | 1 | 2 | 386.8 | 123.0 | 64.5 | 78.2 | 61.3 | 7.7 | 52.2 | wall +2%, disk +9.4% |

| index | args | wall s | CPU s | peak RSS GB | commits | segments | disk MB | termdict | postings | positions | fast | fieldnorms | store | vs baseline |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| t16-default-every1000 | index-t16-default-every1000 | 20.4 | 70.0 | 0.86 | 48 | 12 | 343.1 | 121.3 | 48.2 | 57.0 | 59.9 | 7.7 | 49.0 | baseline |
| e1000-log-min4 | index-t16-default-every1000-mplogmin4 | 22.6 | 87.9 | 0.86 | 48 | 6 | 347.7 | 123.6 | 49.3 | 57.4 | 60.7 | 7.7 | 49.1 | wall +11%, disk +1.4% |
| e1000-log-min16 | index-t16-default-every1000-mplogmin16 | 13.2 | 57.9 | 0.91 | 48 | 18 | 345.5 | 123.8 | 48.0 | 57.4 | 59.5 | 7.7 | 49.1 | wall -35%, disk +0.7% |
| e1000-none | index-t16-default-every1000-mpnone | 12.3 | 38.5 | 0.79 | 48 | 768 | 405.4 | 171.1 | 48.3 | 61.0 | 67.5 | 7.7 | 48.7 | wall -40%, disk +18.2% |
| e1000-nonefinal | index-t16-default-every1000-mpnonefinal | 29.7 | 53.9 | 1.08 | 48 | 1 | 333.6 | 111.4 | 49.9 | 56.8 | 59.2 | 7.7 | 48.7 | wall +45%, disk -2.8% |
| e1000-mt1 | index-t16-default-every1000-mt1 | 29.2 | 60.0 | 0.85 | 48 | 19 | 350.6 | 127.7 | 48.2 | 57.7 | 60.6 | 7.7 | 48.7 | wall +43%, disk +2.2% |
| e1000-mt8 | index-t16-default-every1000-mt8 | 12.6 | 65.2 | 0.89 | 48 | 19 | 350.8 | 128.0 | 47.9 | 57.5 | 60.6 | 7.7 | 49.0 | wall -38%, disk +2.3% |
| e1000-batchunit | index-t16-default-every1000-batchunit | 12.9 | 54.2 | 0.88 | 48 | 17 | 340.8 | 126.3 | 45.1 | 56.7 | 59.5 | 7.7 | 45.5 | wall -37%, disk -0.7% |
| t16-default-every100 | index-t16-default-every100 | 77.2 | 130.6 | 0.75 | 480 | 15 | 342.5 | 124.5 | 47.3 | 57.0 | 59.7 | 7.7 | 46.4 | wall +278%, disk -0.2% |

| index | args | wall s | CPU s | peak RSS GB | commits | segments | disk MB | termdict | postings | positions | fast | fieldnorms | store | vs baseline |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| t1-default-end | index-t1-default-end | 18.1 | 26.9 | 0.49 | 1 | 13 | 337.2 | 123.4 | 45.3 | 56.2 | 59.0 | 7.7 | 45.5 | baseline |
| t1-large-end-single | index-t1-large-end-single | 19.6 | 19.7 | 0.83 | 1 | 1 | 323.9 | 111.4 | 45.7 | 55.2 | 58.5 | 7.7 | 45.5 | wall +8%, disk -3.9% |
| t1-default-unit | index-t1-default-unit | 954.4 | 869.1 | 0.23 | 47995 | 16 | 337.4 | 122.7 | 45.9 | 56.3 | 59.0 | 7.7 | 45.8 | wall +5174%, disk +0.1% |

- Merge policy is the build-time lever: NoMerge leaves 768 segments (wall -40%) but makes library search 7x slower
  (130.7 ms vs 18.0 ms summed on the every-1000 baseline); `log:min=16` and 8 merge threads cut the 48-commit build by about 35-38% with
  18-19 segments and no measurable search cost.
- A commit every 100 units costs 77.2 s vs 20.4 s at every 1 000 (480 vs 48 commits, about 0.12 s per commit).
- Disk: zstd:3 docstore saves 19 MB (store 52.0 -> 33.0 MB); an uncompressed docstore adds 80 MB; dropping STORED
  (`store-none`) saves 50 MB, but fetching ids from the fast field then doubles library latency (34.7 vs 16.0 ms); the lean schema is 228 MB (-35%); the sstable term dictionary halves termdict
  (121.4 -> 60.5 MB, -17% disk) and costs 2.5x library latency; body without positions saves 51 MB and breaks phrases.
- Build walls are single runs: same-layout builds vary 13-20 s, so wall deltas under about 30% are noise.

## Speed of every variant
### Writer, merge and layout variants: library mode, 30 queries
| variant | sum of warm medians, ms (vs baseline) | median warm ms | sum cold ms | ours / variant (warm sum) | same as baseline | note |
|---|---|---|---|---|---|---|
| baseline t16-large-end (2 segments) | 16.0 vs 16.0 (+0%) | 0.32 | 226.6 | 2956x | 30/30 hits, 30/30 pages, 30/30 sets, top-10 overlap 1.00 |  |
| t16-default-end (11 segments) | 17.9 vs 16.0 (+12%) | 0.50 | 260.0 | 2645x | 30/30 hits, 5/30 pages, 14/30 sets, top-10 overlap 0.90 |  |
| t2-large-end | 14.1 vs 16.0 (-12%) | 0.27 | 276.7 | 3364x | 30/30 hits, 5/30 pages, 13/30 sets, top-10 overlap 0.91 |  |
| t4-large-end | 15.2 vs 16.0 (-5%) | 0.30 | 227.0 | 3111x | 30/30 hits, 5/30 pages, 15/30 sets, top-10 overlap 0.92 |  |
| t8-large-end | 25.2 vs 16.0 (+57%) | 0.78 | 177.1 | 1884x | 30/30 hits, 5/30 pages, 13/30 sets, top-10 overlap 0.90 |  |
| t16-256-end | 17.9 vs 16.0 (+12%) | 0.36 | 194.2 | 2643x | 30/30 hits, 5/30 pages, 14/30 sets, top-10 overlap 0.91 |  |
| t16-1024-end | 15.5 vs 16.0 (-3%) | 0.35 | 204.3 | 3061x | 30/30 hits, 5/30 pages, 14/30 sets, top-10 overlap 0.91 |  |
| t16-large-end-ram | 15.6 vs 16.0 (-3%) | 0.32 | 253.3 | 3046x | 30/30 hits, 5/30 pages, 15/30 sets, top-10 overlap 0.92 |  |
| t1-large-end-single | 20.4 vs 16.0 (+27%) | 0.60 | 183.6 | 2319x | 30/30 hits, 5/30 pages, 14/30 sets, top-10 overlap 0.90 |  |
| t1-default-end | 17.7 vs 16.0 (+10%) | 0.35 | 1064.4 | 2680x | 30/30 hits, 5/30 pages, 14/30 sets, top-10 overlap 0.91 |  |
| t16-default-every1000 | 18.0 vs 16.0 (+12%) | 0.48 | 275.9 | 2633x | 30/30 hits, 5/30 pages, 14/30 sets, top-10 overlap 0.90 |  |
| e1000-log-min4 | 15.6 vs 16.0 (-3%) | 0.38 | 176.6 | 3046x | 30/30 hits, 5/30 pages, 14/30 sets, top-10 overlap 0.91 |  |
| e1000-log-min16 | 19.8 vs 16.0 (+23%) | 0.42 | 218.8 | 2398x | 30/30 hits, 5/30 pages, 14/30 sets, top-10 overlap 0.91 |  |
| e1000-none | 130.7 vs 16.0 (+715%) | 3.80 | 603.1 | 363x | 30/30 hits, 5/30 pages, 14/30 sets, top-10 overlap 0.91 |  |
| e1000-nonefinal | 24.8 vs 16.0 (+55%) | 0.66 | 186.4 | 1911x | 30/30 hits, 5/30 pages, 15/30 sets, top-10 overlap 0.91 |  |
| e1000-mt1 | 21.0 vs 16.0 (+31%) | 0.41 | 233.8 | 2259x | 30/30 hits, 5/30 pages, 13/30 sets, top-10 overlap 0.90 |  |
| e1000-mt8 | 21.0 vs 16.0 (+31%) | 0.47 | 262.1 | 2259x | 30/30 hits, 5/30 pages, 15/30 sets, top-10 overlap 0.91 |  |
| e1000-batchunit | 18.6 vs 16.0 (+16%) | 0.40 | 235.5 | 2550x | 30/30 hits, 5/30 pages, 14/30 sets, top-10 overlap 0.90 |  |
| t16-default-every100 | 22.0 vs 16.0 (+37%) | 0.49 | 269.9 | 2155x | 30/30 hits, 5/30 pages, 15/30 sets, top-10 overlap 0.91 |  |

### Schema, docstore, tokenizer and side-field variants: library mode, 30 queries
| variant | sum of warm medians, ms (vs baseline) | median warm ms | sum cold ms | ours / variant (warm sum) | same as baseline | note |
|---|---|---|---|---|---|---|
| baseline | 16.0 vs 16.0 (+0%) | 0.32 | 226.6 | 2956x | 30/30 hits, 30/30 pages, 30/30 sets, top-10 overlap 1.00 |  |
| ds-none | 16.1 vs 16.0 (+0%) | 0.34 | 315.2 | 2949x | 30/30 hits, 5/30 pages, 13/30 sets, top-10 overlap 0.89 |  |
| ds-zstd3 | 16.3 vs 16.0 (+2%) | 0.32 | 248.6 | 2902x | 30/30 hits, 5/30 pages, 13/30 sets, top-10 overlap 0.91 |  |
| ds-zstd9 | 17.5 vs 16.0 (+9%) | 0.29 | 263.3 | 2713x | 30/30 hits, 5/30 pages, 13/30 sets, top-10 overlap 0.91 |  |
| ds-zstd3-bs64k | 14.6 vs 16.0 (-9%) | 0.28 | 357.7 | 3254x | 30/30 hits, 5/30 pages, 14/30 sets, top-10 overlap 0.90 |  |
| ds-zstd3-bs256k | 14.8 vs 16.0 (-8%) | 0.29 | 945.0 | 3198x | 30/30 hits, 5/30 pages, 14/30 sets, top-10 overlap 0.92 |  |
| store-none | 34.7 vs 16.0 (+116%) | 1.09 | 171.2 | 1366x | 30/30 hits, 5/30 pages, 15/30 sets, top-10 overlap 0.91 |  |
| lean | 34.4 vs 16.0 (+115%) | 1.07 | 163.9 | 1377x | 30/30 hits, 5/30 pages, 14/30 sets, top-10 overlap 0.90 |  |
| fn-textonly | 14.6 vs 16.0 (-9%) | 0.30 | 201.6 | 3246x | 30/30 hits, 5/30 pages, 14/30 sets, top-10 overlap 0.91 |  |
| fn-none | 14.0 vs 16.0 (-13%) | 0.28 | 191.1 | 3384x | 30/30 hits, 2/30 pages, 5/30 sets, top-10 overlap 0.51 |  |
| body-freqs | 11.9 vs 16.0 (-26%) | 0.27 | 175.7 |  | 25/25 hits, 5/25 pages, 14/25 sets, top-10 overlap 0.89 |  |
| tok-default | 14.4 vs 16.0 (-10%) | 0.30 | 181.7 | 3292x | 29/30 hits, 5/30 pages, 14/30 sets, top-10 overlap 0.90 |  |
| tok-simple | 15.0 vs 16.0 (-7%) | 0.29 | 156.6 | 3164x | 29/30 hits, 5/30 pages, 15/30 sets, top-10 overlap 0.90 |  |
| tok-enstem | 16.8 vs 16.0 (+5%) | 0.32 | 194.7 | 2815x | 5/30 hits, 4/30 pages, 4/30 sets, top-10 overlap 0.73 |  |
| tok-pretok | 15.3 vs 16.0 (-5%) | 0.37 | 157.9 | 3103x | 30/30 hits, 5/30 pages, 15/30 sets, top-10 overlap 0.91 |  |
| parts | 16.1 vs 16.0 (+0%) | 0.35 | 197.1 | 2948x | 5/30 hits, 5/30 pages, 5/30 sets, top-10 overlap 0.78 |  |
| ngram | 21.1 vs 16.0 (+32%) | 0.44 | 152.0 | 2247x | 10/30 hits, 5/30 pages, 8/30 sets, top-10 overlap 0.83 |  |
| counts | 13.7 vs 16.0 (-14%) | 0.27 | 139.3 | 3447x | 30/30 hits, 5/30 pages, 13/30 sets, top-10 overlap 0.90 |  |
| sstable termdict | 39.7 vs 16.0 (+148%) | 1.09 | 331.7 | 1193x | 30/30 hits, 5/30 pages, 16/30 sets, top-10 overlap 0.90 |  |

### library-all and product on index variants
| variant | sum of warm medians, ms (vs baseline) | median warm ms | sum cold ms | ours / variant (warm sum) | same as baseline | note |
|---|---|---|---|---|---|---|
| library-all baseline | 129.3 vs 129.3 (+0%) | 1.51 | 268.7 | 366x | 30/30 hits, 30/30 pages, 30/30 sets, top-10 overlap 1.00 |  |
| fn-textonly | 152.7 vs 129.3 (+18%) | 2.01 | 316.0 | 310x | 30/30 hits, 15/30 pages, 28/30 sets, top-10 overlap 1.00 |  |
| fn-none | 171.7 vs 129.3 (+33%) | 2.28 | 274.6 | 276x | 30/30 hits, 1/30 pages, 5/30 sets, top-10 overlap 0.54 |  |
| sstable | 226.5 vs 129.3 (+75%) | 3.14 | 432.1 | 209x | 30/30 hits, 19/30 pages, 29/30 sets, top-10 overlap 1.00 |  |
| variant | sum of warm medians, ms (vs baseline) | median warm ms | sum cold ms | ours / variant (warm sum) | same as baseline | note |
|---|---|---|---|---|---|---|
| product baseline | 4038.9 vs 4038.9 (+0%) | 50.74 | 4735.8 | 12x | 30/30 hits, 30/30 pages, 30/30 sets, top-10 overlap 1.00 |  |
| lean | 4373.7 vs 4038.9 (+8%) | 52.87 | 5128.2 | 11x | 30/30 hits, 30/30 pages, 30/30 sets, top-10 overlap 1.00 |  |
| body-freqs | 4385.1 vs 4038.9 (+9%) | 53.05 | 5059.5 | 11x | 25/30 hits, 25/30 pages, 25/30 sets, top-10 overlap 0.97 |  |
| tok-pretok | 4803.1 vs 4038.9 (+19%) | 54.16 | 5453.5 | 10x | 30/30 hits, 30/30 pages, 30/30 sets, top-10 overlap 1.00 |  |
| fn-none | 5006.5 vs 4038.9 (+24%) | 60.32 | 5888.9 | 9x | 30/30 hits, 30/30 pages, 30/30 sets, top-10 overlap 1.00 |  |
| sstable | 5214.5 vs 4038.9 (+29%) | 64.01 | 6106.5 | 9x | 30/30 hits, 30/30 pages, 30/30 sets, top-10 overlap 1.00 |  |
| time-open | 4722.9 vs 4038.9 (+17%) | 56.35 | 5444.7 | 10x | 30/30 hits, 30/30 pages, 30/30 sets, top-10 overlap 1.00 |  |
| warm-stats | 5001.1 vs 4038.9 (+24%) | 60.75 | 5587.7 | 9x | 30/30 hits, 30/30 pages, 30/30 sets, top-10 overlap 1.00 |  |
| product-block | 4813.8 vs 4038.9 (+19%) | 52.56 | 5546.8 | 10x | 30/30 hits, 30/30 pages, 30/30 sets, top-10 overlap 1.00 |  |

### Query shapes, statistics, collectors, reader options: library mode on t16-large-end, 30 queries
| variant | sum of warm medians, ms (vs baseline) | median warm ms | sum cold ms | ours / variant (warm sum) | same as baseline | note |
|---|---|---|---|---|---|---|
| baseline nested | 16.0 vs 16.0 (+0%) | 0.32 | 226.6 | 2956x | 30/30 hits, 30/30 pages, 30/30 sets, top-10 overlap 1.00 |  |
| flat | 13.6 vs 16.0 (-15%) | 0.37 | 116.7 | 3490x | 30/30 hits, 15/30 pages, 30/30 sets, top-10 overlap 1.00 |  |
| dismax:0.1 | 19.2 vs 16.0 (+20%) | 0.32 | 77.7 | 2471x | 30/30 hits, 5/30 pages, 8/30 sets, top-10 overlap 0.82 |  |
| dismax:0.3 | 18.3 vs 16.0 (+14%) | 0.36 | 77.9 | 2583x | 30/30 hits, 5/30 pages, 8/30 sets, top-10 overlap 0.90 |  |
| msm | 14.9 vs 16.0 (-7%) | 0.34 | 67.6 | 3173x | 25/30 hits, 25/30 pages, 25/30 sets, top-10 overlap 0.90 |  |
| fuzzy | 71.3 vs 16.0 (+345%) | 1.69 | 156.5 | 664x | 8/30 hits, 10/30 pages, 25/30 sets, top-10 overlap 1.00 |  |
| phrase-prefix:50 | 35.9 vs 16.0 (+124%) | 0.85 | 88.6 | 1320x | 5/30 hits, 1/30 pages, 4/30 sets, top-10 overlap 0.31 |  |
| parser | 15.7 vs 16.0 (-2%) | 0.40 | 77.5 | 3023x | 30/30 hits, 19/30 pages, 30/30 sets, top-10 overlap 1.00 |  |
| slop 1 | 17.2 vs 16.0 (+7%) | 0.45 | 76.9 | 2753x | 26/30 hits, 27/30 pages, 27/30 sets, top-10 overlap 0.95 |  |
| slop 2 | 17.8 vs 16.0 (+11%) | 0.36 | 80.1 | 2662x | 26/30 hits, 27/30 pages, 27/30 sets, top-10 overlap 0.93 |  |
| weights 1/1/1/1/1 | 19.8 vs 16.0 (+24%) | 0.49 | 90.4 | 2391x | 30/30 hits, 5/30 pages, 10/30 sets, top-10 overlap 0.78 |  |
| stats live | 16.5 vs 16.0 (+3%) | 0.40 | 94.6 | 2872x | 30/30 hits, 13/30 pages, 30/30 sets, top-10 overlap 1.00 |  |
| stats live-union | 18.8 vs 16.0 (+17%) | 0.42 | 219.7 | 2526x | 30/30 hits, 5/30 pages, 9/30 sets, top-10 overlap 0.90 |  |
| counts index + stats live | 15.3 vs 16.0 (-5%) | 0.28 | 372.1 | 3097x | 30/30 hits, 5/30 pages, 13/30 sets, top-10 overlap 0.90 |  |
| collect tuple-count | 19.3 vs 16.0 (+20%) | 0.36 | 103.7 | 2458x | 30/30 hits, 30/30 pages, 30/30 sets, top-10 overlap 1.00 |  |
| collect tuple-id | 93.6 vs 16.0 (+484%) | 3.44 | 200.5 | 506x | 30/30 hits, 5/30 pages, 16/30 sets, top-10 overlap 0.91 |  |
| collect tweak | 24.6 vs 16.0 (+54%) | 0.57 | 123.9 | 1924x | 30/30 hits, 30/30 pages, 30/30 sets, top-10 overlap 1.00 |  |
| id-source fast | 46.3 vs 16.0 (+189%) | 1.50 | 78.3 | 1023x | 30/30 hits, 30/30 pages, 30/30 sets, top-10 overlap 1.00 |  |
| doc-cache 0 | 31.9 vs 16.0 (+99%) | 0.91 | 62.4 | 1487x | 30/30 hits, 30/30 pages, 30/30 sets, top-10 overlap 1.00 |  |
| doc-cache 100 | 16.8 vs 16.0 (+4%) | 0.36 | 76.3 | 2829x | 30/30 hits, 30/30 pages, 30/30 sets, top-10 overlap 1.00 |  |
| doc-cache 1000 | 15.9 vs 16.0 (-1%) | 0.32 | 78.5 | 2985x | 30/30 hits, 30/30 pages, 30/30 sets, top-10 overlap 1.00 |  |
| time-open | 17.8 vs 16.0 (+11%) | 0.39 | 90.1 | 2667x | 30/30 hits, 30/30 pages, 30/30 sets, top-10 overlap 1.00 |  |
| madvise normal | 17.9 vs 16.0 (+12%) | 0.43 | 81.9 | 2648x | 30/30 hits, 30/30 pages, 30/30 sets, top-10 overlap 1.00 |  |
| madvise random | 18.1 vs 16.0 (+13%) | 0.38 | 82.1 | 2624x | 30/30 hits, 30/30 pages, 30/30 sets, top-10 overlap 1.00 |  |
| madvise sequential | 17.2 vs 16.0 (+7%) | 0.37 | 85.6 | 2762x | 30/30 hits, 30/30 pages, 30/30 sets, top-10 overlap 1.00 |  |
| madvise willneed | 17.7 vs 16.0 (+11%) | 0.49 | 77.5 | 2674x | 30/30 hits, 30/30 pages, 30/30 sets, top-10 overlap 1.00 |  |
| variant | sum of warm medians, ms (vs baseline) | median warm ms | sum cold ms | ours / variant (warm sum) | same as baseline | note |
|---|---|---|---|---|---|---|
| library-all baseline | 129.3 vs 129.3 (+0%) | 1.51 | 268.7 | 366x | 30/30 hits, 30/30 pages, 30/30 sets, top-10 overlap 1.00 |  |
| stats live | 149.7 vs 129.3 (+16%) | 1.80 | 299.9 | 317x | 30/30 hits, 14/30 pages, 30/30 sets, top-10 overlap 1.00 |  |
| stats live-union | 149.5 vs 129.3 (+16%) | 1.80 | 376.9 | 317x | 30/30 hits, 5/30 pages, 9/30 sets, top-10 overlap 0.90 |  |

### Search threads on the 11-segment index
| variant | sum of warm medians, ms (vs baseline) | median warm ms | sum cold ms | ours / variant (warm sum) | same as baseline | note |
|---|---|---|---|---|---|---|
| library 1 thread | 17.9 vs 17.9 (+0%) | 0.50 | 260.0 | 2645x | 30/30 hits, 30/30 pages, 30/30 sets, top-10 overlap 1.00 |  |
| 2 | 40.8 vs 17.9 (+128%) | 0.95 | 223.2 | 1162x | 30/30 hits, 30/30 pages, 30/30 sets, top-10 overlap 1.00 |  |
| 4 | 21.8 vs 17.9 (+22%) | 0.55 | 104.9 | 2171x | 30/30 hits, 30/30 pages, 30/30 sets, top-10 overlap 1.00 |  |
| 8 | 15.4 vs 17.9 (-14%) | 0.46 | 87.2 | 3076x | 30/30 hits, 30/30 pages, 30/30 sets, top-10 overlap 1.00 |  |
| variant | sum of warm medians, ms (vs baseline) | median warm ms | sum cold ms | ours / variant (warm sum) | same as baseline | note |
|---|---|---|---|---|---|---|
| library-all 1 thread | 137.5 vs 137.5 (+0%) | 1.59 | 274.0 | 345x | 30/30 hits, 30/30 pages, 30/30 sets, top-10 overlap 1.00 |  |
| 2 | 173.6 vs 137.5 (+26%) | 2.91 | 439.9 | 273x | 30/30 hits, 30/30 pages, 30/30 sets, top-10 overlap 1.00 |  |
| 4 | 141.2 vs 137.5 (+3%) | 2.11 | 244.6 | 336x | 30/30 hits, 30/30 pages, 30/30 sets, top-10 overlap 1.00 |  |
| 8 | 128.8 vs 137.5 (-6%) | 1.93 | 230.9 | 368x | 30/30 hits, 30/30 pages, 30/30 sets, top-10 overlap 1.00 |  |

### Kind filter routes (--kind function --kind method), on the kind-code + path-facet index
| variant | sum of warm medians, ms (vs baseline) | median warm ms | sum cold ms | ours / variant (warm sum) | same as baseline | note |
|---|---|---|---|---|---|---|
| terms (default) | 38.0 vs 38.0 (+0%) | 0.91 | 261.1 | 1057x | 30/30 hits, 30/30 pages, 30/30 sets, top-10 overlap 1.00 |  |
| termset | 33.2 vs 38.0 (-13%) | 0.66 | 98.6 | 1210x | 30/30 hits, 30/30 pages, 30/30 sets, top-10 overlap 1.00 |  |
| bytes | 12.3 vs 38.0 (-68%) | 0.23 | 24.9 | 3265x | 5/30 hits, 5/30 pages, 5/30 sets, top-10 overlap 0.00 |  |
| collector | 31.3 vs 38.0 (-18%) | 0.49 | 92.7 | 1285x | 30/30 hits, 30/30 pages, 30/30 sets, top-10 overlap 1.00 |  |
| code (u64 FilterCollector) | 27.1 vs 38.0 (-29%) | 0.35 | 88.8 | 1479x | 30/30 hits, 30/30 pages, 30/30 sets, top-10 overlap 1.00 |  |
| kind-fast-only index (range) | 51.6 vs 38.0 (+36%) | 1.62 | 266.6 | 778x | 30/30 hits, 5/30 pages, 18/30 sets, top-10 overlap 0.93 |  |
| library-all terms | 119.3 vs 38.0 (+214%) | 1.52 | 204.2 | 337x | 30/30 hits, 5/30 pages, 19/30 sets, top-10 overlap 0.92 |  |
| product | 2603.2 vs 38.0 (+6754%) | 24.06 | 3164.2 | 15x | 30/30 hits, 5/30 pages, 12/30 sets, top-10 overlap 0.50 |  |
| product, kind-fast-only index | 2760.0 vs 38.0 (+7167%) | 28.10 | 3178.0 | 15x | 30/30 hits, 5/30 pages, 12/30 sets, top-10 overlap 0.50 |  |

### Path filter routes (--path-prefix pkg), same index
| variant | sum of warm medians, ms (vs baseline) | median warm ms | sum cold ms | ours / variant (warm sum) | same as baseline | note |
|---|---|---|---|---|---|---|
| regex (default) | 116.1 vs 116.1 (+0%) | 3.63 | 176.5 | 335x | 30/30 hits, 30/30 pages, 30/30 sets, top-10 overlap 1.00 |  |
| range | 29.5 vs 116.1 (-75%) | 0.61 | 90.9 | 1321x | 30/30 hits, 30/30 pages, 30/30 sets, top-10 overlap 1.00 |  |
| ffrange | 28.1 vs 116.1 (-76%) | 0.67 | 95.8 | 1384x | 30/30 hits, 30/30 pages, 30/30 sets, top-10 overlap 1.00 |  |
| invrange | 106.0 vs 116.1 (-9%) | 3.42 | 150.7 | 367x | 30/30 hits, 30/30 pages, 30/30 sets, top-10 overlap 1.00 |  |
| facet | 19.0 vs 116.1 (-84%) | 0.33 | 70.4 | 2048x | 30/30 hits, 30/30 pages, 30/30 sets, top-10 overlap 1.00 |  |
| collector | 32.3 vs 116.1 (-72%) | 0.65 | 78.7 | 1205x | 30/30 hits, 30/30 pages, 30/30 sets, top-10 overlap 1.00 |  |

### Counting, sets, aggregations, facets (vs the library top-200 baseline)
| variant | sum of warm medians, ms (vs baseline) | median warm ms | sum cold ms | ours / variant (warm sum) | same as baseline | note |
|---|---|---|---|---|---|---|
| library top-200 | 16.0 vs 16.0 (+0%) | 0.32 | 226.6 | 2956x | 30/30 hits, 30/30 pages, 30/30 sets, top-10 overlap 1.00 |  |
| count via query | 5.4 vs 16.0 (-67%) | 0.07 | 13.6 | 8843x | 30/30 hits, 0/30 pages, 0/30 sets, top-10 overlap 0.00 |  |
| count via collector | 7.3 vs 16.0 (-55%) | 0.07 | 14.9 | 6523x | 30/30 hits, 0/30 pages, 0/30 sets, top-10 overlap 0.00 |  |
| docset DocSetCollector | 49.1 vs 16.0 (+206%) | 0.37 | 67.7 | 965x | 30/30 hits, 0/30 pages, 0/30 sets, top-10 overlap 0.00 |  |
| docset Vec | 6.8 vs 16.0 (-57%) | 0.12 | 16.5 | 6919x | 30/30 hits, 0/30 pages, 0/30 sets, top-10 overlap 0.00 |  |
| agg-kind tuple | 24.1 vs 16.0 (+50%) | 0.50 | 101.5 | 1970x | 30/30 hits, 30/30 pages, 30/30 sets, top-10 overlap 1.00 |  |
| agg-kind separate | 26.5 vs 16.0 (+65%) | 0.52 | 82.6 | 1789x | 30/30 hits, 30/30 pages, 30/30 sets, top-10 overlap 1.00 |  |
| agg-kind custom | 21.1 vs 16.0 (+31%) | 0.36 | 78.9 | 2250x | 30/30 hits, 30/30 pages, 30/30 sets, top-10 overlap 1.00 |  |
| facets tuple | 111.5 vs 16.0 (+595%) | 3.47 | 193.7 | 425x | 30/30 hits, 5/30 pages, 14/30 sets, top-10 overlap 0.92 |  |
| facets separate | 115.3 vs 16.0 (+619%) | 3.53 | 182.9 | 411x | 30/30 hits, 5/30 pages, 14/30 sets, top-10 overlap 0.92 |  |


Findings from the speed tables:
- body-freqs: the library aborts on the first quoted phrase ("Applied phrase query on field body, which does not
  have positions indexed"); 25 of 30 queries ran. Product mode finds no phrase match instead (25/30 hits equal).
- `--kind-filter bytes` returns 0 hits for every kind-filtered query on kubernetes too (5/30 equal hits are the 5
  rare-term queries that have 0 function/method hits anyway): the BytesFilterCollector finding holds.
- Path filter: the facet route (19.0 ms) and the term-range routes (28-30 ms) beat the regex route (116.1 ms) by 4-6x
  with identical pages; kind filter: the u64 FilterCollector is the fastest correct route (27.1 vs 38.0 ms).
- Counting: `Weight::count` answers all 30 hit counts in 5.4 ms summed, a third of the top-200 query.
- Fuzzy (d=1) is 4.5x slower and changes hits on 22/30 queries; phrase-prefix:50 changes hits on 25/30.
- Search threads on 11 segments: 8 threads -14% (library), -6% (library-all); 2 threads is slower (+128%).
- Product: the Warmer moves statistics to open (open 2.10 ms vs 0.35 ms median) but the cold query is not faster
  (cold sum 5 588 vs 5 445 ms with `--time-open`); product-block equals product on 30/30 pages at +19% (noise range).

## Quality of the ranking and tokenization variants
Index variants (library mode unless named):
| variant | name MRR@10 / R@200 | doc MRR@10 / R@200 | subword MRR@10 / R@200 | ident MRR@10 / R@200 | zero-hit queries (name/doc/subword/ident) |
|---|---|---|---|---|---|
| ours | 0.8331 / 0.9460 | 0.6842 / 0.9820 | 0.3793 / 0.2130 | 0.2208 / 0.4025 | 0/0/13/11 |
| library baseline | 0.9012 / 0.9563 (ours +0.068) | 0.0236 / 0.5160 (ours -0.661) | 0.6023 / 0.2179 (ours +0.223) | 0.2417 / 0.3310 (ours +0.021) | 0/0/13/11 |
| library-all baseline | 0.9070 / 0.9563 (ours +0.074) (lib +0.006) | 0.0230 / 0.5160 (ours -0.661) (lib -0.001) | 0.6024 / 0.2179 (ours +0.223) (lib +0.000) | 0.2417 / 0.3310 (ours +0.021) (lib +0.000) | 0/0/13/11 |
| product (unicode61) | 0.8331 / 0.9460 (ours +0.000) (lib -0.068) | 0.6843 / 0.9820 (ours +0.000) (lib +0.661) | 0.3793 / 0.2139 (ours +0.000) (lib -0.223) | 0.2208 / 0.4025 (ours +0.000) (lib -0.021) | 0/0/13/11 |
| library, round-1 code index | 0.9085 / 0.9507 (ours +0.075) (lib +0.007) | 0.0457 / 0.4890 (ours -0.639) (lib +0.022) | 0.7030 / 0.6606 (ours +0.324) (lib +0.101) | 0.5352 / 0.7030 (ours +0.314) (lib +0.293) | 0/0/0/8 |
| library-all, round-1 code index | 0.9120 / 0.9508 (ours +0.079) (lib +0.011) | 0.0456 / 0.4870 (ours -0.639) (lib +0.022) | 0.7031 / 0.6606 (ours +0.324) (lib +0.101) | 0.5352 / 0.7030 (ours +0.314) (lib +0.293) | 0/0/0/8 |
| fn-textonly | 0.9036 / 0.9562 (ours +0.071) (lib +0.002) | 0.0230 / 0.5120 (ours -0.661) (lib -0.001) | 0.6036 / 0.2179 (ours +0.224) (lib +0.001) | 0.2417 / 0.3310 (ours +0.021) (lib +0.000) | 0/0/13/11 |
| fn-none | 0.8672 / 0.9442 (ours +0.034) (lib -0.034) | 0.0376 / 0.5440 (ours -0.647) (lib +0.014) | 0.4993 / 0.1888 (ours +0.120) (lib -0.103) | 0.2238 / 0.3036 (ours +0.003) (lib -0.018) | 0/0/13/11 |
| body-freqs | 0.9052 / 0.9562 (ours +0.072) (lib +0.004) | 0.0230 / 0.5170 (ours -0.661) (lib -0.001) | 0.6028 / 0.2179 (ours +0.223) (lib +0.001) | 0.0179 / 0.0179 (ours -0.203) (lib -0.224) | 0/0/13/53 |
| tok-default | 0.9017 / 0.9470 (ours +0.069) (lib +0.000) | 0.0234 / 0.5150 (ours -0.661) (lib -0.000) | 0.5954 / 0.2178 (ours +0.216) (lib -0.007) | 0.2417 / 0.3310 (ours +0.021) (lib +0.000) | 9/0/13/11 |
| tok-simple | 0.9042 / 0.9563 (ours +0.071) (lib +0.003) | 0.0237 / 0.5140 (ours -0.661) (lib +0.000) | 0.6028 / 0.2181 (ours +0.223) (lib +0.001) | 0.2417 / 0.3310 (ours +0.021) (lib +0.000) | 0/0/13/11 |
| tok-enstem | 0.8682 / 0.9436 (ours +0.035) (lib -0.033) | 0.0243 / 0.3610 (ours -0.660) (lib +0.001) | 0.4760 / 0.1864 (ours +0.097) (lib -0.126) | 0.2417 / 0.3355 (ours +0.021) (lib +0.000) | 9/0/11/11 |
| tok-pretok | 0.9039 / 0.9563 (ours +0.071) (lib +0.003) | 0.0232 / 0.5130 (ours -0.661) (lib -0.000) | 0.6020 / 0.2180 (ours +0.223) (lib -0.000) | 0.2417 / 0.3310 (ours +0.021) (lib +0.000) | 0/0/13/11 |
| parts | 0.9125 / 0.9560 (ours +0.079) (lib +0.011) | 0.0219 / 0.5370 (ours -0.662) (lib -0.002) | 0.5865 / 0.5661 (ours +0.207) (lib -0.016) | 0.4917 / 0.7545 (ours +0.271) (lib +0.250) | 0/0/0/0 |
| ngram | 0.9047 / 0.9563 (ours +0.072) (lib +0.003) | 0.0210 / 0.5420 (ours -0.663) (lib -0.003) | 0.6430 / 0.5143 (ours +0.264) (lib +0.041) | 0.2417 / 0.4382 (ours +0.021) (lib +0.000) | 0/0/0/11 |
| library-all fn-textonly | 0.9069 / 0.9563 (ours +0.074) (lib +0.006) | 0.0230 / 0.5160 (ours -0.661) (lib -0.001) | 0.6024 / 0.2179 (ours +0.223) (lib +0.000) | 0.2417 / 0.3310 (ours +0.021) (lib +0.000) | 0/0/13/11 |
| library-all fn-none | 0.8696 / 0.9439 (ours +0.037) (lib -0.032) | 0.0377 / 0.5430 (ours -0.647) (lib +0.014) | 0.4967 / 0.1899 (ours +0.117) (lib -0.106) | 0.2238 / 0.3036 (ours +0.003) (lib -0.018) | 0/0/13/11 |
| counts index + stats live | 0.9042 / 0.9563 (ours +0.071) (lib +0.003) | 0.0233 / 0.5160 (ours -0.661) (lib -0.000) | 0.6036 / 0.2180 (ours +0.224) (lib +0.001) | 0.2417 / 0.3310 (ours +0.021) (lib +0.000) | 0/0/13/11 |

Query variants on t16-large-end, the combined indexes, and the 11-segment index:
| variant | name MRR@10 / R@200 | doc MRR@10 / R@200 | subword MRR@10 / R@200 | ident MRR@10 / R@200 | zero-hit queries (name/doc/subword/ident) |
|---|---|---|---|---|---|
| library baseline (nested) | 0.9012 / 0.9563 (ours +0.068) | 0.0236 / 0.5160 (ours -0.661) | 0.6023 / 0.2179 (ours +0.223) | 0.2417 / 0.3310 (ours +0.021) | 0/0/13/11 |
| flat | 0.9042 / 0.9563 (ours +0.071) (lib +0.003) | 0.0237 / 0.5160 (ours -0.661) (lib +0.000) | 0.6024 / 0.2179 (ours +0.223) (lib +0.000) | 0.2417 / 0.3310 (ours +0.021) (lib +0.000) | 0/0/13/11 |
| dismax:0.1 | 0.8993 / 0.9561 (ours +0.066) (lib -0.002) | 0.0358 / 0.6320 (ours -0.648) (lib +0.012) | 0.5986 / 0.2124 (ours +0.219) (lib -0.004) | 0.2417 / 0.3489 (ours +0.021) (lib +0.000) | 0/0/13/11 |
| dismax:0.3 | 0.9006 / 0.9563 (ours +0.068) (lib -0.001) | 0.0304 / 0.5830 (ours -0.654) (lib +0.007) | 0.5993 / 0.2136 (ours +0.220) (lib -0.003) | 0.2417 / 0.3489 (ours +0.021) (lib +0.000) | 0/0/13/11 |
| msm | 0.9012 / 0.9563 (ours +0.068) (lib +0.000) | 0.6860 / 0.9840 (ours +0.002) (lib +0.662) | 0.6023 / 0.2179 (ours +0.223) (lib +0.000) | 0.2417 / 0.3146 (ours +0.021) (lib +0.000) | 0/0/13/12 |
| fuzzy | 0.9026 / 0.9563 (ours +0.070) (lib +0.001) | 0.0238 / 0.5170 (ours -0.660) (lib +0.000) | 0.6026 / 0.2204 (ours +0.223) (lib +0.000) | 0.2417 / 0.3310 (ours +0.021) (lib +0.000) | 0/0/5/11 |
| phrase-prefix:50 | 0.7985 / 0.9229 (ours -0.035) (lib -0.103) | 0.0000 / 0.0000 (ours -0.684) (lib -0.024) | 0.5761 / 0.3141 (ours +0.197) (lib -0.026) | 0.2173 / 0.3152 (ours -0.004) (lib -0.024) | 1/1000/6/22 |
| parser | 0.9156 / 0.9563 (ours +0.083) (lib +0.014) | 0.0236 / 0.5160 (ours -0.661) (lib +0.000) | 0.6024 / 0.2179 (ours +0.223) (lib +0.000) | 0.2417 / 0.3310 (ours +0.021) (lib +0.000) | 0/0/13/11 |
| weights 1/1/1/1/1 | 0.8985 / 0.9540 (ours +0.065) (lib -0.003) | 0.2654 / 0.9440 (ours -0.419) (lib +0.242) | 0.5618 / 0.2266 (ours +0.182) (lib -0.040) | 0.2455 / 0.3310 (ours +0.025) (lib +0.004) | 0/0/13/11 |
| stats live | 0.9007 / 0.9563 (ours +0.068) (lib -0.001) | 0.0236 / 0.5150 (ours -0.661) (lib +0.000) | 0.6022 / 0.2179 (ours +0.223) (lib -0.000) | 0.2417 / 0.3310 (ours +0.021) (lib +0.000) | 0/0/13/11 |
| stats live-union | 0.9024 / 0.9559 (ours +0.069) (lib +0.001) | 0.0338 / 0.6330 (ours -0.650) (lib +0.010) | 0.6005 / 0.2182 (ours +0.221) (lib -0.002) | 0.2417 / 0.3310 (ours +0.021) (lib +0.000) | 0/0/13/11 |
| library-all stats live | 0.9075 / 0.9563 (ours +0.074) (lib +0.006) | 0.0230 / 0.5160 (ours -0.661) (lib -0.001) | 0.6022 / 0.2179 (ours +0.223) (lib -0.000) | 0.2417 / 0.3310 (ours +0.021) (lib +0.000) | 0/0/13/11 |
| library-all stats live-union | 0.9087 / 0.9559 (ours +0.076) (lib +0.007) | 0.0335 / 0.6330 (ours -0.651) (lib +0.010) | 0.6004 / 0.2183 (ours +0.221) (lib -0.002) | 0.2417 / 0.3310 (ours +0.021) (lib +0.000) | 0/0/13/11 |
| slop 1 (ident only) | — | — | — | 0.2417 / 0.3310 (ours +0.021) (lib +0.000) | —/—/—/11 |
| slop 2 (ident only) | — | — | — | 0.2417 / 0.3283 (ours +0.021) (lib +0.000) | —/—/—/10 |
| combined: BM25 over all | 0.8308 / 0.9456 (ours -0.002) (lib -0.070) | 0.6830 / 0.9820 (ours -0.001) (lib +0.659) | 0.3814 / 0.2218 (ours +0.002) (lib -0.221) | 0.2112 / 0.3819 (ours -0.010) (lib -0.030) | 0/0/13/11 |
| combined+fields | 0.9053 / 0.9562 (ours +0.072) (lib +0.004) | 0.0425 / 0.6620 (ours -0.642) (lib +0.019) | 0.6022 / 0.2181 (ours +0.223) (lib -0.000) | 0.2417 / 0.3310 (ours +0.021) (lib +0.000) | 0/0/13/11 |
| code+combined: all | 0.8051 / 0.9159 (ours -0.028) (lib -0.096) | 0.6723 / 0.9820 (ours -0.012) (lib +0.649) | 0.5812 / 0.6425 (ours +0.202) (lib -0.021) | 0.5131 / 0.6803 (ours +0.292) (lib +0.271) | 0/0/0/8 |
| code+combined: all+fields | 0.9122 / 0.9505 (ours +0.079) (lib +0.011) | 0.0860 / 0.6270 (ours -0.598) (lib +0.062) | 0.7021 / 0.6603 (ours +0.323) (lib +0.100) | 0.5362 / 0.7030 (ours +0.315) (lib +0.294) | 0/0/0/8 |
| library 11-seg | 0.9027 / 0.9561 (ours +0.070) (lib +0.001) | 0.0233 / 0.5140 (ours -0.661) (lib -0.000) | 0.6037 / 0.2179 (ours +0.224) (lib +0.001) | — | 0/0/13/— |
| library-all 11-seg | 0.9067 / 0.9563 (ours +0.074) (lib +0.005) | 0.0230 / 0.5160 (ours -0.661) (lib -0.001) | 0.6024 / 0.2179 (ours +0.223) (lib +0.000) | — | 0/0/13/— |

Findings from the quality tables:
- The doc band: the library's per-field sum stays near 0.02-0.04 MRR@10 on every unicode61 variant with the stock
  boosts. Three things lift it: BM25 over one combined field (0.683, equal to ours), equal boosts 1/1/1/1/1 (0.265),
  and `msm` (0.686). `msm` requires every term, which our matching does not (mean hits 90 vs 14 694), so it changes
  the answer set, not the ranking. Union document frequencies (`live-union`) give only 0.034.
- One combined field reproduces our ranking on unicode61 (name 0.831, doc 0.683, subword 0.381 vs ours 0.833, 0.684,
  0.379) at +104 MB of disk (combined index 457 MB vs 353 MB).
- Code-aware tokens: the library on the code index lifts subword R@200 from 0.218 to 0.661 and ident MRR@10 from
  0.242 to 0.535; `--parts-fields` reaches ident 0.492 / R@200 0.755 and subword R@200 0.566 on a unicode61 index.
  code+combined (all field only) keeps doc at 0.672 and gets subword 0.581 / 0.643 and ident 0.513.
- Fieldnorms off (`fn-none`) costs the library 0.034 name and 0.103 subword MRR@10; `text-only` changes nothing.
- en_stem loses 0.126 subword MRR@10 and 0.155 doc R@200; the stock `default` tokenizer drops 9 name queries to zero
  hits (it removes tokens of 40 bytes or more; harness 10, T6).
- body-freqs ident: the run aborted at the first phrase query, so its ident row is not a quality number.
- 2 vs 11 segments, same documents: library MRR@10 moves by at most 0.0015; it is the tie order that moves (below).

## Tie and score stability across layouts (rule audit 4 evidence)
Same queries, t16-large-end (2 segments) vs t16-default-end (11 segments), pages compared:
| mode | set | hits equal | pages identical | pages equal as sets | mean top-200 overlap | mean top-10 overlap |
|---|---|---|---|---|---|---|
| library (ties by address) | speed 30 | 30 | 5 | 14 | 0.967 | 0.900 |
| library | name 1000 | 1000 | 320 | 740 | 0.963 | 0.899 |
| library | doc 1000 | 1000 | 9 | 362 | 0.940 | 0.940 |
| library | subword 1000 | 1000 | 37 | 446 | 0.960 | 0.917 |
| library-all (ties by id) | speed 30 | 30 | 13 | 28 | 0.999 | 0.990 |
| library-all | name 1000 | 1000 | 624 | 947 | 0.997 | 0.980 |
| library-all | doc 1000 | 1000 | 201 | 886 | 0.997 | 0.990 |
| library-all | subword 1000 | 1000 | 428 | 974 | 0.999 | 0.988 |
library-all breaks ties by id, so any difference there is an f32 score that moved with the layout: the (score, id)
order is not stable across builds of the same documents (376 of 1 000 name pages differ).

## Pruned top-k per query shape (`ttopk --shape`, 30 queries; the flat shape also on the name set)
| shape | index | pruned == exhaustive (score, address) | reported misses | queries with a miss | largest gap | scores differing between paths (max) |
|---|---|---|---|---|---|---|
| flat | 2 segments | 14/30 | 0 | 0 | — | 1 417 (2 ulp) |
| flat | 11 segments | 12/30 | 10 | 2 | 1 ulp | 1 396 (2 ulp) |
| flat | 11 segments, name 1000 | 611/1000 | 4 556 (3 907 above the page, 649 below the 200th) | 45 | 2 ulp | 22 616 (6 ulp) |
| dismax:0.0 / dismax:0.1 / msm / fuzzy | 2 and 11 | 30/30 each | 0 | 0 | — | 0 |
| parser | 2 segments | 19/30 | 0 | 0 | — | 1 040 (2 ulp) |
| parser | 11 segments | 17/30 | 10 | 2 | 1 ulp | 1 022 (2 ulp) |
Round 1: nested shape 30/30 and 1 000/1 000, 0 misses. Every "miss" here is a document whose exhaustive score is 1-2
ulp above the page's lowest pruned score: the block-WAND path sums the same terms in a different order, so a tie at
the 200th place resolves to different documents. No document beyond f32 rounding was lost.

## Visibility in one index (`--hide-units PCT --hide-seed 7`, 30 queries)
| mode | hidden | route | warm sum ms | median warm ms | hits / unhidden | pages identical to unhidden | sets equal | hide build median ms |
|---|---|---|---|---|---|---|---|---|
| library | 0% | weight | 15.8 | 0.34 | 1.000 | 30/30 | 30/30 | 11.05 |
| library | 0% | collector | 16.1 | 0.31 | 1.000 | 30/30 | 30/30 | 12.44 |
| library | 0% | alive-set | 15.1 | 0.35 | 1.000 | 30/30 | 30/30 | 12.10 |
| library | 10% | weight | 16.1 | 0.31 | 0.889 | 4/30 | 4/30 | 10.89 |
| library | 10% | collector | 18.6 | 0.41 | 0.889 | 4/30 | 4/30 | 10.49 |
| library | 10% | alive-set | 22.2 | 0.51 | 0.889 | 4/30 | 4/30 | 22.49 |
| library | 50% | weight | 18.8 | 0.32 | 0.506 | 1/30 | 1/30 | 14.26 |
| library | 50% | collector | 22.5 | 0.35 | 0.506 | 1/30 | 1/30 | 14.88 |
| library | 50% | alive-set | 18.6 | 0.37 | 0.506 | 1/30 | 1/30 | 15.32 |
| library-all | 0% | weight | 155.0 | 1.72 | 1.000 | 30/30 | 30/30 | 11.34 |
| library-all | 0% | collector | 145.2 | 1.51 | 1.000 | 30/30 | 30/30 | 9.87 |
| library-all | 0% | alive-set | 153.9 | 1.72 | 1.000 | 30/30 | 30/30 | 13.09 |
| library-all | 10% | weight | 146.1 | 1.88 | 0.889 | 4/30 | 4/30 | 10.77 |
| library-all | 10% | collector | 147.7 | 2.17 | 0.889 | 4/30 | 4/30 | 10.90 |
| library-all | 10% | alive-set | 151.3 | 1.87 | 0.889 | 4/30 | 4/30 | 12.94 |
| library-all | 50% | weight | 103.2 | 1.45 | 0.506 | 1/30 | 1/30 | 12.70 |
| library-all | 50% | collector | 103.6 | 1.50 | 0.506 | 1/30 | 1/30 | 14.25 |
| library-all | 50% | alive-set | 96.8 | 1.44 | 0.506 | 1/30 | 1/30 | 15.79 |
| product | 0% | collector | 4851.1 | 55.69 | 1.000 | 30/30 | 30/30 | 11.01 |
| product | 0% | alive-set | 4650.0 | 57.24 | 1.000 | 30/30 | 30/30 | 11.46 |
| product | 10% | collector | 4063.1 | 49.78 | 0.890 | 4/30 | 4/30 | 11.38 |
| product | 10% | alive-set | 4246.5 | 54.15 | 0.890 | 4/30 | 4/30 | 10.86 |
| product | 50% | collector | 2893.7 | 34.77 | 0.505 | 1/30 | 1/30 | 13.69 |
| product | 50% | alive-set | 2895.7 | 36.73 | 0.505 | 1/30 | 1/30 | 19.23 |
- 0% equals the unhidden pages 30/30 on every route and mode (the correctness check). The three library routes are
  within noise of each other; building the hidden set costs 10-22 ms per searcher on 546 613 documents, which is
  larger than a warm library query and must be cached per searcher generation.
- Product mode ignores `--hide-via weight` (it always hides in its own collector), so only collector and alive-set
  were run for it. A visible-only reference index was not built on kubernetes (smoke: exact match, harness 11).

## Owner run 1: the code-aware tokenizer under our scoring (`product-code` vs `product`)
`product-code` needs the `token_count_code` column, which the round-1 code index (older build) lacks, so the code
index was rebuilt with the same arguments (`tindex docs.jsonl idx/code-next2 16 large end --tokenizer code`,
`target-next2`: 21.4 s, 2 segments, 387 MB, 0 token-count mismatches). `product` is round 1's run on t16-large-end.
| set | arm | MRR@10 | S@1 | S@10 | R@200 | mean hits | Δ MRR@10 | Δ R@200 |
|---|---|---|---|---|---|---|---|---|
| name | product | 0.8331 | 0.7640 | 0.9500 | 0.9460 | 9 095.6 | | |
| name | product-code | 0.8101 | 0.7440 | 0.9320 | 0.9198 | 50 582.5 | -0.0230 | -0.0262 |
| doc | product | 0.6843 | 0.5870 | 0.8650 | 0.9820 | 13 689.9 | | |
| doc | product-code | 0.6756 | 0.5740 | 0.8590 | 0.9830 | 16 371.3 | -0.0087 | +0.0010 |
| subword | product | 0.3793 | 0.2650 | 0.6490 | 0.2139 | 4 181.1 | | |
| subword | product-code | 0.5003 | 0.3450 | 0.8670 | 0.6299 | 5 311.7 | +0.1210 | +0.4160 |
Zero-hit queries: subword 13 (product) vs 0 (product-code). Commands: `r2-logs/e-owner.log`; results
`tantivy-product-code-o1-code-qual-{name,doc,subword}.jsonl`.

## Owner run 2: early stopping (`product-topk`, `target-next2`)
`tblock <bench>/idx/t16-large-end` first: 2 segments, 4 814 741 blocks, 75.5 MB of side files, 1.46 s; no commit or merge
followed, so it was not rerun. `product` for the 30 queries ran back to back with the same binary
(`o2-prod-speed`; its pages equal round 1's product 30/30); for the quality sets the product reference is round 1's run.
| set | queries | pages identical to product | product warm: median / sum ms | product-topk warm: median / sum ms | median per-query ratio | blocks skipped (all queries) | median per-query skip |
|---|---|---|---|---|---|---|---|
| speed 30 (5 warm runs) | 30 | 30 | 53.46 / 4653 | 10.12 / 296 | 0.30x | 67.7% of 51466 | 45.1% |
| name 1000 (1 warm run) | 1000 | 1000 | 1.81 / 35203 | 1.78 / 4817 | 0.86x | 69.5% of 522272 | 0.0% |
| doc 1000 (1 warm run) | 1000 | 1000 | 74.05 / 85488 | 12.65 / 14682 | 0.19x | 58.7% of 1936315 | 57.8% |
| subword 1000 (1 warm run) | 1000 | 1000 | 20.72 / 36471 | 9.61 / 9018 | 0.49x | 67.6% of 844892 | 47.3% |

`--search-threads 4`, the 30 queries, latency only:
| arm | warm sum, 1 thread | warm sum, 4 threads | Δ | median warm, 1 → 4 threads | pages vs 1 thread |
|---|---|---|---|---|---|
| product | 4 653 ms | 4 443 ms | -4.5% | 53.46 → 51.77 ms | 30/30 |
| product-topk | 296 ms | 185 ms | -37.5% | 10.12 → 6.23 ms | 30/30 |
- Per-query latencies for the 30 queries (product vs product-topk, ratio, skip rate): `r2-logs/topk-per-query.md`.
- `tblock` left its 75.5 MB of `tb-blockmax-*.bin` files inside `idx/t16-large-end`: `du` of that directory now
  counts them; `tspace` does not.
- Every page of product-topk equals product's: 3 030 of 3 030. On the 30 queries it is 15.7x faster summed
  (4 653 → 296 ms), up to 100x on the most frequent terms ("vendor" 876.6 → 8.7 ms, 94.6% of blocks skipped).
- It is slower than product on one query only, the phrase "admission controller" (5.6 → 7.4 ms); the five rarest
  terms, where no block is skipped, still gain 23-36%. The phrase queries print no block counts.
- The name set's median ratio is 0.86 (most names are rare: median skip 0%), but its sum falls 35 203 → 4 817 ms,
  because the frequent names dominate the cost.

## Owner run 3: weighted document length under our scoring (`--dl weighted`, `target-next3`)
dl = Σ weight(c) · len(c) over the five columns (5/5/2/2/1), avgdl its mean, from five exact per-column token counts
stored by `tindex --column-lengths` (harness 13). Indexes: `idx/collen` (unicode61, 14.7 s, 355 MB) and
`idx/collen-code` (code tokenizer, 23.4 s, 389 MB), both deleted afterwards (rebuildable). Queries under
`MemoryMax=8G`. The `--dl total` controls equal the earlier runs exactly: 1 000/1 000 identical pages on every set,
for product and for product-code, and the same teval numbers.
| set | arm | dl | MRR@10 | S@1 | S@10 | R@200 | mean hits | Δ MRR@10 vs product | Δ R@200 vs product |
|---|---|---|---|---|---|---|---|---|---|
| name | product | total (control) | 0.8331 | 0.7640 | 0.9500 | 0.9460 | 9 095.6 | | |
| name | product | weighted | 0.8566 | 0.7880 | 0.9670 | 0.9505 | 9 095.6 | +0.0235 | +0.0045 |
| name | product-code | total | 0.8101 | 0.7440 | 0.9320 | 0.9198 | 50 582.5 | -0.0230 | -0.0262 |
| name | product-code | weighted | 0.8562 | 0.7940 | 0.9680 | 0.9308 | 50 582.5 | +0.0231 | -0.0152 |
| doc | product | total (control) | 0.6843 | 0.5870 | 0.8650 | 0.9820 | 13 689.9 | | |
| doc | product | weighted | 0.6703 | 0.5670 | 0.8630 | 0.9810 | 13 689.9 | -0.0140 | -0.0010 |
| doc | product-code | total | 0.6756 | 0.5740 | 0.8590 | 0.9830 | 16 371.3 | -0.0087 | +0.0010 |
| doc | product-code | weighted | 0.6475 | 0.5400 | 0.8490 | 0.9790 | 16 371.3 | -0.0368 | -0.0030 |
| subword | product | total (control) | 0.3793 | 0.2650 | 0.6490 | 0.2139 | 4 181.1 | | |
| subword | product | weighted | 0.4346 | 0.3130 | 0.7050 | 0.2139 | 4 181.1 | +0.0553 | +0.0000 |
| subword | product-code | total | 0.5003 | 0.3450 | 0.8670 | 0.6299 | 5 311.7 | +0.1210 | +0.4160 |
| subword | product-code | weighted | 0.5585 | 0.3940 | 0.9100 | 0.6444 | 5 311.7 | +0.1792 | +0.4305 |
- Weighted length helps name (+0.024) and subword (+0.055) and costs doc (-0.014); with the code tokenizer it is the
  best arm on name (0.856) and subword (0.559 / 0.644), at a doc cost of -0.037 against product.
- Weighted vs total pages (product): identical on 465 name, 19 doc and 45 subword queries of 1 000; mean top-10
  overlap 0.94, 0.91, 0.84. Hits never change (length does not change matching).
- Commands and outputs: `r2-logs/f-weighted-name.log` and `r2-logs/f-weighted-doc-subword.log` (one log, split at the doc set), teval output `r2-logs/f-weighted-teval.txt`; results
  `tantivy-product-q-<set>-<dl>.jsonl`, `tantivy-product-code-q-<set>-<dl>.jsonl`.
