# 11 — Per-unit visibility in one index (`--hide-units`)

Version: tantivy 0.26.2. Our engine keeps retained snapshots in which some units are not visible. This variant prices that inside one tantivy index that holds every document.

## Mechanism
- `tquery … --hide-units PCT --hide-seed S` hides a unit when a seeded FNV-1a hash of its `unit` string, with a final avalanche, lands in the lowest PCT percent (`library.rs` `unit_hidden`). `source/smoke/visible.py` reproduces the hash.
- `HideSet::build` reads each segment's `unit` fast column once per searcher. It resolves the hidden decision once per term ordinal (`sorted_ords_to_term_cb`), then fills a per-segment `Vec<bool>` by doc id. This build is timed apart from the query and recorded as `hide_build_ms`.
- `--hide FILE` hides explicit `(segment id, doc)` addresses instead (`tdelta --soft`).
- Result lines carry `hidden_pct` and `stats_include_hidden`, and `variant` repeats the options.

## Three routes, all modes
- **weight** (default): `HideQuery` wraps the query. Its Weight intersects the scorer with the set, and it forwards `for_each_pruning` with hidden documents filtered out of the callback, so TopDocs keeps block-WAND (`src/query/weight.rs:123`).
- **collector**: `HideCollector` drops hidden documents before the inner segment collector sees them. It cannot forward TopDocs' own `collect_segment` (`src/collector/mod.rs:173`), so pruning is lost.
- **alive-set**: each segment is reopened with `SegmentReader::open_with_custom_alive_set` (`src/index/segment_reader.rs:148`). The bitset is live AND visible, written with `write_alive_bitset` and read back with `AliveBitSet::open` (`src/fastfield/alive_bitset.rs:13,52`). The searcher's loop runs over those readers on one thread. Every collector then honours visibility natively, TopDocs pruning included.
- **product** modes always hide inside their own collector, or through the alive set: hidden documents are never candidates and never counted in N, the token total or df.

## Smoke (6-segment fixture index, seed 7, 18 queries; reference = an index built from only the visible documents)
| PCT | units / docs hidden | product (any route) | library hits | library pages | hide build |
|---|---|---|---|---|---|
| 0 | 0 / 0 | 18/18 = unhidden | = unhidden | 18/18 = unhidden on all routes | 0.08–0.14 ms |
| 10 | 43 / 234 | 18/18 = visible-only index | 31 592 = visible-only | 3/18 same (7/18 as sets) | 0.08–0.19 ms |
| 50 | 234 / 2 356 | 18/18 = visible-only index | 15 568 = visible-only | 3/18 same (6/18 as sets) | 0.09–0.20 ms |
- Library modes match the visible-only index exactly in hits; their pages differ because the library's statistics still count hidden documents (`stats_include_hidden: true`).
- `--stats live` (visible-only N and df, fieldnorm-decoded token totals) moves 10% to 10/18 page sets. The remaining gap is the fieldnorm approximation and f32 layout ulps (10-variants.md, findings 3 and 4).
- `product-block --hide-units 10` also matches 18/18.
- `tdelta --soft` with `--hide FILE` equals the hard delete 19/19 for product and for library-all.
- **Which library route is faster:** on the fixture all three run within noise (library warm 0.14–0.16 ms, library-all 1.29–1.63 ms), so the fixture cannot rank them. The weight route is the default because it is the only one of the two asked-for routes that keeps TopDocs' pruning path. The alive-set route keeps it as well, but searches on one thread and reopens segments per searcher (0.13–0.20 ms build vs 0.08–0.15 ms). The kubernetes run should time `--hide-via weight|collector|alive-set` at 10 and 50 percent to settle it.
