# 10 — Every variant from the feature research, built and smoked

Version: tantivy 0.26.2. Proposals come from `../features-matrix/00-feature-matrix.md` (ids I/F/T/Q/S/C/RT), `../features-matrix/18-quality-design.md` (QD) and `../features-matrix/17-product-rule-mechanisms.md` (PR). Every row was smoked on the generated fixture (`<bench>/smoke/`, 4 800 docs, 510 units, 18 contract queries, niced, `MemoryMax=4G`) with the `target-next` build.
- Helper scripts: `source/smoke/{build.sh,q.sh,sum.py,diag.py,visible.py}`.
- "warm" is the median over queries of each query's median of 3 warm runs.
- "same" counts queries whose hits and page equal the stated reference.
- Fixture timings are sub-millisecond and mostly noise; they show that each variant runs, not which is fast. Speed is decided on the full kubernetes runs.

## Index variants (`tindex`; disk from `tspace`; reference = `end` cadence, 1 segment, 1 414 938 B)
| id | flag | what it tests | smoke |
|---|---|---|---|
| I1 | `<threads>` 2/4/8/16 | thread scaling | 2/4/1/2 segments; 1.52/1.65/1.47/1.44 MB; product 18/18 |
| I2 | budget 15 / 256 MB | budget vs segment count | 2 / 1 segments |
| I3 | `--merge-threads 1` | merge threads | unit cadence, 6 segments, as default |
| I4 | `--merge-policy log:min=4` / `log:min=16` | merge knobs | 3 / 15 segments after 510 commits |
| I5 | `--merge-policy none+final` | NoMerge then one merge | 1 segment; CPU 2.6 s vs 4.4 s (default) |
| I6 | cadence `every:100` | commit cadence | 6 commits, 225 ms vs 8.6 s per-unit |
| I8 | `--batch unit` | `IndexWriter::run` per unit | 9.5 s vs 8.6 s; same 6 segments |
| I11 | `--single-segment` | `SingleSegmentIndexWriter` | 121 ms, peak RSS 19.7 MB vs 30.6 MB |
| I12 | `--ram-then-persist` | RamDirectory + `persist` | 111 ms; bytes identical to mmap build |
| F1 | `--docstore none / zstd:3 / zstd:9` | compressor | store 665 / 140 / 134 KB vs lz4 233 KB |
| F2 | `--blocksize 65536 / 262144` (zstd:3) | block size | store 132 / 142 KB |
| F3 | `tquery --doc-cache 0/100/1000` | store cache | 0: warm 0.56 ms, 9 618 misses; 100: 0.13 ms, 42 misses |
| F4 | `--store none` | drop STORED | 1.20 MB; ids from fast field: library warm 1.2 ms vs 0.12 ms |
| F5 | `--fieldnorms text-only / none` | fieldnorm cost | 1.38 / 1.35 MB; `none` changes 18/18 library pages |
| F6 | `--body-record freqs` | no body positions | 1.17 MB; library errors on quoted phrases; product 13/18 |
| F7 | `--kind-fast-only` | kind as fast only | TermQuery errors under scoring (see findings); range path 18/18 |
| F9 | `tspace <dir>` | bytes per component/field | written for every build (`smoke/v/space-*.json`) |
| F10 | `cargo build --features sstable` | sstable termdict | termdict 180 KB vs 394 KB (FST); tcheck 19/19 |
| — | `--schema lean` | §8 lean schema | 0.96 MB; product 18/18; library ids via fast field |
| T2 | `--tokenizer default / simple` | stock tokenizers | library same 18/18 on this fixture |
| T4 | `--name-ngram` | 3-gram side field | ident R@200 0.64 vs 0.29 |
| T7 | `--tokenizer en_stem` | stemming | same 18/18 (synthetic vocabulary) |
| T9 | `--tokenizer code` | camelCase parts stacked | ident R@200 0.71, subword 0.97 |
| T10 | `--tokenizer pretokenized` | PreTokenizedString | add 90 ms vs 24 ms; product 18/18 |
| QD | `--parts-fields` | parts-only side fields | ident MRR 0.52, R@200 0.93 |
| C5 | `--kind-code` | u64 kind for FilterCollector | 18/18 |
| C8 | `--path-facet` | facet field | facets mode, facet path filter 18/18 |
| S2 | `--field-counts` | exact per-column token counts | lets `--stats live` count tokens exactly |
| new | `--combined` | one `all` field = five columns | 1.87 MB (+0.46 MB) |

## Query variants (`tquery`; reference = baseline library or library-all on the same index)
| id | flag | what it tests | smoke |
|---|---|---|---|
| Q1 | `--shape parser` | QueryParser with boosts | 12/18 library pages, hits 32 249 vs 32 270 |
| Q4/Q15 | `--shape flat` | block-WAND eligible shape | ttopk: pruned ≠ exhaustive scores on 590/812 queries (≤2 ulp), 0 misses |
| Q5 | `--slop N` | phrase slop | slop 2: hits 34 388 vs 32 270 |
| Q6 | `--shape phrase-prefix:N` | PhrasePrefix | hits 24 295 |
| Q8 | `--shape fuzzy` | fuzzy d=1 at 0.1 | hits 32 793 |
| Q11/Q14 | `--shape dismax:0.1/0.3` (+ttopk `dismax:0.0`) | DisMax and its pruning defect | pruned == exhaustive 812/812 (nested under per-term Boolean) |
| — | `--shape msm` | all terms required | hits 26 948 |
| — | `--weights a/b/c/d/e` | field boosts | 1/1/1/1/1: 3/18 same pages |
| Q3 | `--kind-filter termset` | TermSet filter | 18/18 |
| C5 | `--kind-filter bytes / collector / code` | BytesFilterCollector / own wrapper / FilterCollector u64 | bytes: 0 hits (finding); collector, code 18/18 |
| Q7/Q9 | `--path-filter range/ffrange/invrange/facet/collector` | path filter routes | all 18/18; regex warm 0.29 ms, others 0.11–0.14 |
| C4 | `--collect tuple-id` | (score desc, id asc) sort key | equals library-all page 18/18; warm 1.3 ms vs 0.12 |
| C6 | `--collect tuple-count` | (TopDocs, Count) in one search | 18/18 |
| S5 | `--collect tweak` | identity tweak_score | 18/18 |
| S2 | `--stats live / live-union` | alive-only BM25 statistics | see findings |
| S8 | `--explain K` | explain cost and value | 360/360 values equal the score; 0.02–0.03 ms each |
| RT1 | `--search-threads 4` | segment parallelism | 6 segments: warm 0.24 vs 0.14 ms |
| RT2 | `--warm-stats` | Warmer caching corpus stats | product 18/18 |
| RT5 | `--madvise normal/random/sequential/willneed` + `--time-open` | mmap advice, open time | 18/18; open 0.14–0.20 ms |
| — | `--id-source fast` | id from fast field, not store | 1.2 ms vs 0.12 ms warm |
| C7 | mode `count --count-via query/collector` | Weight::count vs Count | 18/18 |
| C7 | mode `docset --docset-via docset/vec` | DocSetCollector vs Vec | 0.10 vs 0.02 ms |
| C9 | mode `agg-kind --agg tuple/separate/custom` | terms aggregation vs own counter | identical counts on all 18 |
| C8 | mode `facets --facets tuple/separate` | FacetCollector | 18/18 |
| S7 | mode `product-block` | product with Weight candidates | 18/18 (filters, hide included) |
| new | mode `product-code` | our BM25F on a code index | `tcheck --code` 19/19, 19/19 filtered, 1 and 6 segments |
| new | mode `library-combined [--shape combined+fields[:W]]` | BM25 over `all` alone, or plus fields | see quality |
| PR | `--hide-units PCT --hide-seed S --hide-via …`, `--hide FILE` | per-unit visibility | see 11-hide-units.md |

## Writer, snapshot and delta tools
| id | tool | smoke |
|---|---|---|
| I9/I10 | `tdelta --delete unit/id/query` | 267 deleted each way; tcheck 19/19 post-delta; commit 33/40/40 ms |
| I4 | `tdelta --merge-policy log:del=0.2` | no merge triggered (7 segments) |
| PR | `tdelta --soft FILE` + `tquery --hide FILE` | product and library-all equal the hard delete 19/19 |
| PR(a) | `tsnap <src> <dst>` | hard-link snapshot: 37 files, 1.45 MB, 0.34 ms; product 18/18 on the snapshot |
| PR | `tpin <copy> <docs> <queries>` | held searcher answers equal after delta + forced merge (19/19); OnCommitWithDelay visible after 414 ms; Manual reload 0.16 ms |
| QD | `tqrels` band `ident` | 55 queries (whole, inner part, words, phrase) |
| Q14/Q15 | `ttopk --shape S` | per-shape pruned vs exhaustive |

## Quality on the fixture (`teval`, MRR@10 / R@200)
| arm | name | doc | subword | ident |
|---|---|---|---|---|
| product (unicode61) | 0.959 / 1.000 | 0.135 / 0.992 | 0.756 / 0.563 | 0.273 / 0.281 |
| product-code | 0.936 / 0.994 | 0.114 / 0.985 | 0.756 / 0.965 | 0.578 / 0.707 |
| library | 0.964 / 1.000 | 0.000 / 0.852 | 0.693 / 0.564 | 0.273 / 0.289 |
| library-combined (combined) | 0.939 / 0.995 | 0.113 / 0.969 | 0.730 / 0.551 | 0.264 / 0.273 |
| library-combined (combined+fields) | 0.964 / 1.000 | 0.000 / 0.983 | 0.652 / 0.563 | 0.264 / 0.289 |
| library-combined on code index | 0.908 / 0.988 | 0.100 / 0.969 | 0.757 / 0.941 | 0.557 / 0.705 |
| library-all, code index | 0.954 / 1.000 | 0.000 / 0.089 | 0.733 / 0.968 | 0.468 / 0.708 |
The fixture's vocabulary is synthetic; the kubernetes runs decide quality.

## Findings the smoke established
1. `BytesFilterCollector` reads `fast_fields().bytes(field)` (`src/collector/filter_collector_wrapper.rs:287-293`), which opens only `ColumnType::Bytes` columns (`src/fastfield/readers.rs:206-214`). On a STRING fast field it finds no column and drops every document: 0 hits, no error. The bench's own ordinal wrapper (`KeepCollector`) and `FilterCollector` on a u64 code work.
2. `TermQuery` falls back to a fast-field range only when scoring is off (`src/query/term_query/term_query.rs:131-137`). As a filter under a scored query it fails with "Field "kind" is not indexed". The fast-only variant therefore builds the same `RangeQuery [k, k]` itself.
3. A merge estimates each field's token total from the one-byte fieldnorms when a segment has deletes (`src/indexer/merger.rs:29-62`). So the merged index is not an exact reference either. On the fixture, `--stats live` (fieldnorm-decoded totals) matches the merged index's page set 19/19; the exact-count variant matches 13/19.
4. Scores of the same document differ in the last f32 bits across segment layouts and query shapes: 14 of 200 ids for `café` between 1 and 4 segments. The flat shape on block-WAND differs on 590/812 queries, by up to 2 ulp. Pages compared across layouts must use a tie-insensitive measure (set or Jaccard; `sum.py` reports both).
5. A string fast column's ord-to-string lookup costs about 6 µs per id at 200 ids per page, ten times the LZ4 docstore with its block cache. Without `--doc-cache` (0 blocks), the store costs 0.56 ms.
6. `IndexBuilder::create` is private (`src/index/index.rs:247`); `single_segment_index_writer` is `#[doc(hidden)]` (`:185`). The bench uses `open_or_create` on an empty directory, then `SingleSegmentIndexWriter::new`.
7. `IndexWriterOptions` is a typestate builder (`src/indexer/index_writer.rs:50-62`): every option must be set in one chain.
8. A PhraseQuery on a field indexed without positions is a hard error ("does not have positions indexed", `src/query/phrase_query/phrase_query.rs:107`). Product mode instead finds no phrase match (13/18).
