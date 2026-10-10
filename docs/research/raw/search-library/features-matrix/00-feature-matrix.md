# tantivy 0.26.2: every lever for codectx, and how to benchmark it

**Version.** `tantivy = "=0.26.2"` in `../harness/source/Cargo.toml` (confirmed). Read from the published crate source (`tantivy-0.26.2/` plus `tantivy-columnar-0.7.0`, `tantivy-sstable-0.7.0`, `tantivy-tokenizer-api-0.7.0`), its README (features), ARCHITECTURE.md, CHANGELOG.md (0.20 to 0.26.2) and the 21 `examples/` (7 read in full, 14 by their commentary). docs.rs was not fetched: the crate source, which generates it, was used instead. Citations are `src/...:line` inside `tantivy-0.26.2/` unless a sub-crate is named. Docs: https://docs.rs/tantivy/0.26.2/tantivy/. Full notes with quotes: this folder and `../features/` (see the end).
**Probes** (a crate on =0.26.2, source in `../features/probes/`, outputs quoted in the raw notes) back the three claims marked "verified": the DisjunctionMax defect (Q14), statistics that count deleted documents (S2), and the tuple tie-break (C4).
**Key:** Applies Y/N/M (maybe). Metric: IT index time, IM index memory, D disk, QL query latency, QM query memory, Q quality. "library arm" = the library arm of the measurement uses it (`../harness/source/src/...`).

## 1. Feature matrix

### Indexing
| # | Capability | Exact API | Applies | Metric | library arm | How to benchmark (variant, settings, baseline) |
|---|---|---|---|---|---|---|
| I1 | Indexing threads | `Index::writer_with_num_threads(n, total)`; `IndexWriterOptions::builder().num_worker_threads(n)` | Y: one segment per thread per commit (`src/indexer/index_writer.rs:69`); `Index::writer()` caps at 8 (`src/index/index.rs:613-623`), these do not (`index_writer.rs:285-300`) | IT, IM, segments | yes (1, 16) | n ∈ {1,2,4,8,16} at fixed total budget; vs t1 |
| I2 | Memory budget per thread | `memory_budget_per_thread` (min 15 MB, flush at budget−1 MB, `src/indexer/index_writer.rs:29-33,195`) | Y | IT, IM, segments | yes (50 MB/t, 2 GB) | {15, 50, 256 MB}/t and 2 GB total; vs 50 MB/t; record segment count |
| I3 | Merge threads | `IndexWriterOptions::num_merge_threads` (default 4, `:60-62`) | Y: host load | IT, IM | no | {1, 4} with default policy; vs 4 |
| I4 | Log merge policy knobs | `LogMergePolicy::set_min_num_segments` / `set_max_docs_before_merge` / `set_min_layer_size` / `set_level_log_size` / `set_del_docs_ratio_before_merge` (defaults 8, 10M, 10 000, 0.75, 1.0, `src/indexer/log_merge_policy.rs:8-15`) | Y | IT, D, QL | no (default) | min_num_segments {4, 16}; del ratio 0.1 after delta; vs default |
| I5 | No merges + one final merge | `set_merge_policy(Box::new(NoMergePolicy))`, `IndexWriter::merge(&ids)`, `wait_merging_threads()` (`src/indexer/index_writer.rs:354,470,535`) | Y: one segment = exact block-max bounds (#S4) and add-order doc ids | IT, QL | no | NoMerge during build, merge all at end; vs default policy |
| I6 | Commit / prepare_commit | `commit()`, `prepare_commit()?.commit()` / `.abort()`, `commit_future()`; a commit flushes every worker (`:598-603,618,664`) | Y: dominant cost at unit cadence | IT, segments | yes (unit, every:K, end) | cadence {unit, every:100, every:1000, end} × threads {1,16}; vs unit |
| I7 | Commit payload | `PreparedCommit::set_payload(&str)`; read via `Index::load_metas()` (`src/indexer/prepared_commit.rs:27`, `src/index/index.rs:520`) | M: tag a generation id | — | no | functional only; no timing |
| I8 | Opstamps and batches | `add_document` → `Opstamp`; `run(Vec<UserOperation>)` keeps a batch in one segment (`src/indexer/index_writer.rs:727,762`) | Y: one unit = one batch | IT | no | `run` per unit vs `add_document` loop; compare IT and segment spread |
| I9 | Delete by term | `delete_term(Term)` (Basic TermQuery, `:680-686`) | Y: delta by `unit` | IT (delta) | yes (tdelta) | keep; vs delete_query |
| I10 | Delete by query | `delete_query(Box<dyn Query>)` (`#[doc(hidden)]`, `:697-708`) | M: delete a path prefix or several units at once | IT (delta) | no | RegexQuery on `path_s` delta vs N delete_terms |
| I11 | One-shot single segment writer | `Index::builder().single_segment_index_writer(dir, budget)` (`#[doc(hidden)]`, "Dragons ahead", `src/index/index.rs:175-192`) | M: from-scratch build | IT, IM | no | `end` cadence build; vs t1/t16 `end` |
| I12 | RAM build then persist | `RamDirectory::create()` … `persist(&dir)` (`src/directory/ram_directory.rs:128-158`) | M | IT, IM | no | `end` cadence; vs mmap build; watch peak RSS |
| I13 | Index sorting | removed in 0.24 (CHANGELOG.md:105); no field in `IndexSettings` (`src/index/index_meta.rs:214-227`) | N: gone | — | — | — |

### Index settings and field options
| # | Capability | Exact API | Applies | Metric | library arm | How to benchmark |
|---|---|---|---|---|---|---|
| F1 | Docstore compressor | `IndexSettings.docstore_compression = Compressor::{None, Lz4, Zstd(ZstdCompressor{compression_level})}` (`src/store/compressors.rs:10-19`); Zstd needs Cargo feature `zstd-compression`, not default (`Cargo.toml:47-72`) | Y: id/kind/path/unit/token_count are stored | D, IT, QL (200 fetches) | no (Lz4 default) | {None, Lz4, Zstd 3, Zstd 9}; vs Lz4 |
| F2 | Docstore block size | `IndexSettings.docstore_blocksize` (default 16 384, `src/index/index_meta.rs:229-232`) | Y | D, QL | no | {16 KiB, 64 KiB, 256 KiB} with Zstd; vs 16 KiB |
| F3 | Docstore reader cache | `IndexReaderBuilder::doc_store_cache_num_blocks(n)` (default 100, `src/store/reader.rs:25`) | Y: library page fetch | QL, QM | no | {0, 100, 1000}; vs 100; read `doc_store_cache_stats()` |
| F4 | Drop STORED entirely | read id/kind/path from FAST columns (`segment_reader.fast_fields()`) | Y: every stored field is also FAST | D, QL | partly (library-all reads `id` fast) | schema without STORED; vs current; space_usage `store()` |
| F5 | Fieldnorms on/off | `TextFieldIndexing::set_fieldnorms(false)` (`src/schema/text_options.rs:236`); off ⇒ constant norm 1 (`src/query/term_query/term_weight.rs:209-217`) | Y for product (never reads them); M for library (changes BM25) | D, Q | no | off on 5 text fields; vs on; compare disk + library pages |
| F6 | Record options | `IndexRecordOption::{Basic, WithFreqs, WithFreqsAndPositions}` (`src/schema/index_record_option.rs:19-28`) | Y: positions needed (phrases hit every column) | D, IT | yes (positions on 5, Basic on STRING) | Basic `unit`/`node_id` already; no cheaper safe variant |
| F7 | Indexed vs fast vs stored | `INDEXED`, `FAST`, `STORED`, `STRING`, `TEXT` (`src/schema/flags.rs:16-53`); FAST-only `TermQuery` fallback (0.26, CHANGELOG.md:37) | Y | D, QL | yes | `kind` FAST-only (no inverted index), filter via the fallback; vs STRING + FAST |
| F8 | Coerce | `set_coerce()` / `COERCE` (`src/schema/text_options.rs:143`, `numeric_options.rs:87`) | N: the exporter writes typed values | — | no | — |
| F9 | Fast-field codecs | automatic Bitpacked/Linear/BlockwiseLinear (`tantivy-columnar-0.7.0/src/column_values/u64_based/mod.rs:79-92,172`) | N: no knob | D | implicit | report bytes per column via space_usage |
| F10 | Term dictionary format | FST by default; sstable with feature `quickwit` (`src/termdict/mod.rs:21-28`) | M | D, QL | no | build with `features=["quickwit"]`; vs FST |

### Tokenizers and filters
| # | Capability | Exact API | Applies | Metric | library arm | How to benchmark |
|---|---|---|---|---|---|---|
| T1 | Custom tokenizer | `impl Tokenizer`/`TokenFilter`; `index.tokenizers().register(name, TextAnalyzer)` | Y: only way to match FTS5 unicode61 | Q, IT | yes (`Unicode61`) | keep; tokenizer throughput via `ttok` |
| T2 | Simple / Whitespace / Raw | `SimpleTokenizer` (splits on `!is_alphanumeric`, `src/tokenizer/simple_tokenizer.rs:34`), `WhitespaceTokenizer`, `RawTokenizer` | N for text (differ from unicode61); Raw = `STRING` | Q | Raw yes | Simple+LowerCaser as a "stock" variant; report hit diffs vs unicode61 |
| T3 | Regex tokenizer | `RegexTokenizer::new(pat)` (`src/tokenizer/regex_tokenizer.rs:57`) | M: identifier-shaped tokens | Q, IT | no | quality band only (#4) |
| T4 | N-grams | `NgramTokenizer::new(min, max, prefix_only)` (`src/tokenizer/ngram_tokenizer.rs:96-130`) | M: substring match on names | Q, D, IT | no | extra `name_ngram` (3,3) field; vs baseline; disk + partial-name recall |
| T5 | LowerCaser / AsciiFolding | `LowerCaser`, `AsciiFoldingFilter` (`src/tokenizer/lower_caser.rs:44-63`) | N: unicode61 already folds and strips diacritics | Q | (inside Unicode61) | — |
| T6 | RemoveLong | `RemoveLongFilter::limit(n)`; the `default` analyzer drops tokens ≥ 40 bytes (`src/tokenizer/tokenizer_manager.rs:60-63`, `remove_long.rs:35-36`) | N: trap for a drop-in | Q | no (own truncation at 32 768) | count tokens ≥ 40 bytes in the corpus to size the loss |
| T7 | Stemmer / StopWord | `Stemmer::new(Language)`, `StopWordFilter::new/remove` (`src/tokenizer/stemmer.rs:69`, `stop_word_filter/mod.rs:37,67`) | N for product (changes hits); M as an experiment | Q, D | no | `en_stem` side field in the quality run only |
| T8 | SplitCompoundWords | `SplitCompoundWords::from_dictionary(words)`: splits only full dictionary decompositions (`src/tokenizer/split_compound_words.rs:1-5,56`) | N: needs a dictionary; identifiers are open-vocabulary | Q | no | — |
| T9 | camelCase / snake_case | no built-in; custom filter with parts at the same `position` (`src/postings/postings_writer.rs:152-155`) | Y: main quality lever (#4) | Q, D, IT | yes (`--tokenizer code`) | see section 4 |
| T10 | Pre-tokenized text | `PreTokenizedString{text, tokens}` (`src/tokenizer/tokenized_string.rs:11`) | M: feed codectx's own tokens | IT | no | tokens from the export vs tokenizing in Rust; IT |
| T11 | Token length cap | `MAX_TOKEN_LEN` = 65 530 bytes, longer tokens DROPPED (`src/tokenizer/mod.rs:165-168`, `src/postings/postings_writer.rs:141-147`) | Y: FTS5 truncates instead | Q | handled (truncates first) | — |

### Queries
| # | Capability | Exact API | Applies | Metric | library arm | How to benchmark |
|---|---|---|---|---|---|---|
| Q1 | QueryParser | `QueryParser::for_index`, `set_conjunction_by_default`, `set_field_boost`, `set_field_fuzzy`, `allow_regexes`, `parse_query_lenient` (`src/query/query_parser/query_parser.rs:279-349`) | N: a multi-token literal becomes a PHRASE (`:958-995`); codectx ORs it | Q | no (hand-built) | parser variant with boosts 5/5/2/2/1; report hit diffs |
| Q2 | Term | `TermQuery::new(term, WithFreqs)` | Y | QL | yes | — |
| Q3 | TermSet | `TermSetQuery::new(terms)`, constant score (`src/query/set_query.rs:59`, `automaton_weight.rs:109`) | Y: kind filter | QL | no (SHOULD of Basic terms) | kind filter as TermSet; vs SHOULD |
| Q4 | Boolean | `BooleanQuery::new`, `union`, `with_minimum_required_clauses` (`boolean_query.rs:180-257`) | Y | QL | yes (nested) | flat SHOULD over (term, field) `Boost(Term)` vs nested (#Q15) |
| Q5 | Phrase + slop | `PhraseQuery::new_with_offset_and_slop`, `set_slop`; idf = sum of term idfs (`phrase_query.rs:48-80`, `bm25.rs:120-128`) | Y (slop 0) | QL, Q | yes | phrase band timing; slop N (codectx has none) |
| Q6 | PhrasePrefix | `PhrasePrefixQuery`, max 50 expansions (`phrase_prefix_query.rs:8,67`) | N: cap + new semantics | — | no | — |
| Q7 | Regex | `RegexQuery::from_pattern`, constant score (`regex_query.rs:64`) | Y: path-prefix filter | QL | yes | vs range on `path_s` [p/, p0) |
| Q8 | Fuzzy | `FuzzyTermQuery::new(term, d, transpose)`, constant score (`fuzzy_query.rs:93-103`) | N: new semantics | Q | no | typo band only, as a separate experiment |
| Q9 | Range | `RangeQuery::new(Bound, Bound)` (fast field auto), `FastFieldRangeQuery` (`range_query/range_query.rs:80`) | M: path prefix as a string range | QL | no | prefix range vs RegexQuery (#Q7) |
| Q10 | Exists | `ExistsQuery::new(field, false)` (`exist_query.rs:64`) | N | — | no | — |
| Q11 | DisjunctionMax | `DisjunctionMaxQuery::with_tie_breaker(qs, t)` (`disjunction_max_query.rs:92-105`) | M: closer to per-field max scoring; **verified defect** under TopDocs (#Q14) | Q | no | per term over 5 boosted fields, t ∈ {0.1, 0.3}, non-pruning collector; vs product page |
| Q12 | Boost | `BoostQuery::new(q, w)`; keeps a TermScorer a TermScorer (`boost_query.rs:70-71`) | Y: field weights | Q | yes | — |
| Q13 | ConstScore | `ConstScoreQuery::new(q, s)` (`const_score_query.rs:66-69`) | Y: filters add no score | QL | yes (MUST, 0) | vs FilterCollector (#C5) |
| Q14 | DisMax defect | TermUnion → `block_wand` SUMS scores, ignores the combiner (`boolean_query/block_wand.rs:200-204`, `boolean_weight.rs:57-68,516-531`). Verified: WithFreqs doc scores 4.44 under TopDocs vs 2.22 correct; Basic is correct | Y: avoid or collect unpruned | Q | — | regression check in every DisMax run |
| Q15 | Block-WAND eligibility | only a union of frequency-reading `TermScorer`s (`boolean_weight.rs:57-68`); nested Boolean, Phrase, ConstScore disable it | Y: library mode speed | QL | no (nested shape) | term-only bands: flat vs nested, TopDocs(200) |
| Q16 | MoreLikeThis | `MoreLikeThisQuery::builder()…with_document(addr)` (`more_like_this/query.rs:41-178`) | N: not a codectx query | — | no | — |

### Scoring
| # | Capability | Exact API | Applies | Metric | library arm | How to benchmark |
|---|---|---|---|---|---|---|
| S1 | BM25 | fixed `K1=1.2, B=0.75` (`src/query/bm25.rs:8-9`); same idf as codectx; per field, f32 (`src/lib.rs:314`); not BM25F (`for_terms` asserts one field, `bm25.rs:100-106`) | Y (library mode) | Q | yes | library vs product page overlap |
| S2 | Statistics provider | `Bm25StatisticsProvider`, `search_with_statistics_provider` (`bm25.rs:15-25`, `src/core/searcher.rs:193-208`). Default counts deleted docs in N, df, token totals (`bm25.rs:40-44`, `inverted_index_reader.rs:249-253`); **verified**: df 2 and N 23 after a delete, `num_docs` 22 | Y | Q, QL | no (product computes its own) | alive provider (df via `doc_freq_given_deletes`, `src/postings/segment_postings.rs:36`) after delta; vs default |
| S3 | Fieldnorm encoding | 1 byte, exact to 40, then floor to a log table (41→40, 100→96, 1 000→984) (`src/fieldnorm/code.rs:6-11`) | Y: library dl is approximate | Q | implicit | covered by F5 |
| S4 | Block-max exactness | "may lead us to return a lesser document … guaranteed to be correct if there is only one segment" (`src/query/term_query/term_scorer.rs:58-70`) | Y: exactness | Q | — | TopDocs page vs all-docs page on multi- and one-segment builds |
| S5 | tweak_score / order_by | `TopDocs::tweak_score(f)`, `order_by(f)` (`top_score_collector.rs:322,426`) | M: fast-field tie-breaks | Q, QL | no | see C4 |
| S6 | EnableScoring | `EnableScoring::Disabled` lets a full-match term become `AllScorer` (`term_weight.rs:191-197`) | Y: product mode bypasses scoring | QL | yes (product) | — |
| S7 | Custom Weight/Scorer or postings walk | `inverted_index(field)?.read_postings(term, opt)` (`src/index/inverted_index_reader.rs:265`) | Y | QL, Q | yes (product) | — |
| S8 | explain() | `Query::explain(&searcher, addr)` → `to_pretty_json()` (`src/query/query.rs:138`) | M: debugging | — | no | — |

### Collectors and aggregations
| # | Capability | Exact API | Applies | Metric | library arm | How to benchmark |
|---|---|---|---|---|---|---|
| C1 | TopDocs + offset | `TopDocs::with_limit(n).and_offset(k)`; limit required, panics on 0 (`top_score_collector.rs:93-139`) | N for product (cap) | QL | yes (library) | — |
| C2 | Order by fast field | `order_by_fast_field`, `order_by_string_fast_field`, `order_by_u64_field` (`:217-312`) | M | QL | no | — |
| C3 | Tie-break rule | ascending DocAddress (`:503-510,589-597`); layout-dependent across builds | Y: determinism | Q | handled (id) | same corpus at t1 vs t16: compare TopDocs pages |
| C4 | Tuple sort keys | `order_by(((SortBySimilarityScore, Desc), (SortByString::for_field("id"), Asc)))` (`sort_key/sort_key_computer.rs:141`); **verified** tie on `id` | Y | Q, QL | no | vs `order_by_score`; prices lost pruning |
| C5 | FilterCollector | `FilterCollector::new(field, pred, inner)` on a fast field (`filter_collector_wrapper.rs:86`) | Y: kind/path filters | QL | no (MUST ConstScore) | kind filter via FilterCollector; vs MUST clause |
| C6 | Count | `Count`; `Query::count` reads df when no deletes (`term_weight.rs:56-66`) | Y | QL | yes (separate search) | tuple `(Count, TopDocs)` in one search vs two |
| C7 | DocSet / Multi / custom | `DocSetCollector` (`HashSet<DocAddress>`), `MultiCollector`, custom `Collector` (`collector/mod.rs:156-235`) | Y: complete answers | QL, QM | yes (library-all, product) | — |
| C8 | Facet / Histogram | `FacetCollector`, `HistogramCollector` | N | — | no | — |
| C9 | Aggregations | `AggregationCollector::from_aggs`; terms size default 10; limits 65 000 buckets, 500 MB (`aggregation/collector.rs:15-18`, `bucket/term_agg.rs:316`) | M: per-kind counts | QL | no | terms agg on `kind` with explicit size; vs custom count |

### Search runtime, directories, reporting
| # | Capability | Exact API | Applies | Metric | library arm | How to benchmark |
|---|---|---|---|---|---|---|
| RT1 | Multi-thread executor | `Index::set_multithread_executor(n)`; one task per segment, "powerless … if … one large segment" (`src/core/searcher.rs:210-232`) | M: latency vs host load | QL, CPU | yes (`--search-threads`) | {1,4,8} on unit-cadence and one-segment indexes |
| RT2 | Warmers | `IndexReaderBuilder::warmers(Vec<Weak<dyn Warmer>>)`, `num_warming_threads` (`src/reader/warming.rs:14-20`) | M: cache alive N/token totals per generation | QL | no | product cold with a warmer vs per-query scan |
| RT3 | ReloadPolicy | `Manual` / `OnCommitWithDelay` (500 ms poll) (`src/reader/mod.rs:21-31`, `file_watcher.rs:12`) | N for a CLI | — | yes (Manual) | — |
| RT4 | Searcher reuse | `reader.searcher()` snapshot (ARCHITECTURE.md, "Searcher") | Y | QL | yes (warm runs) | — |
| RT5 | Mmap + madvise | `MmapDirectory::open_with_madvice(path, Advice)` (`src/directory/mmap_directory/mod.rs:218-230`) | M | QL (cold) | no | {Normal, Random, WillNeed} cold runs; vs `open` |
| RT6 | space_usage | `Searcher::space_usage()` per segment, component and field (`src/space_usage/mod.rs:31-322`) | Y: disk attribution | D | no | print for every build variant |
| RT7 | Snippets | `SnippetGenerator::create(&searcher, &q, field)`, `snippet(text)` (`src/snippet/mod.rs:400-466`) | N: bench does not measure previews | — | no | — |

## 2. Ranked levers (most likely to move a number codectx cares about)
1. **Commit cadence × threads (I6, I1, I2).** Every commit flushes every worker. The library arm's smoke: 9 246 ms at t1 per-unit commits vs 106 ms at t1 with a 2 GB budget and one commit (about 18 ms per commit at t1, 66 ms at t16), so cadence is the largest index-time lever.
2. **Merge policy, ending in one segment (I5, I4).** Removes segment-count overhead at query time, makes block-max exact (S4) and doc ids add-ordered (C3).
3. **Code-aware tokenization (T9).** The only change likely to improve result quality for identifier queries (section 4).
4. **Flat SHOULD for block-WAND (Q15).** Library-mode latency on term-only queries; irrelevant to product mode.
5. **Drop STORED, fieldnorms off (F4, F5).** Pure disk savings for product mode, which reads neither.
6. **Docstore Zstd + block size (F1, F2).** Disk, if anything stays stored.
7. **Kind filter as TermSet or FilterCollector (Q3, C5).** Filtered-query latency.
8. **Warmer for corpus statistics (RT2).** Product-mode cold latency.
9. **Executor (RT1).** Latency only with many segments; costs CPU.
10. **madvise (RT5).** Cold latency, small and host-dependent.

## 3. Product-rule conflicts and the mechanism for each
- **Complete answers, no top-k cap.** TopDocs needs a limit and prunes; block-max can miss a document across segments (S4); PhrasePrefix caps at 50; terms aggregation at 10; `default` analyzer drops ≥40-byte tokens. *Mechanism:* a custom collector that keeps every live (doc, score) (C7). Every pruning feature is then off by construction.
- **Deterministic ranking.** f32 scores; ties on layout-dependent doc addresses (C3). *Mechanism:* tuple sort key with an `id` tie-break (C4), or product mode's f64 score + codectx comparator; or one thread + NoMerge + final merge.
- **Statistics over visible documents.** N, df and token totals include deleted documents until a merge (S2, verified); merge policy ignores deletes by default (I4). *Mechanism:* a `Bm25StatisticsProvider` with alive counts (no BM25F possible); exact only in product mode.
- **Retained snapshots.** One commit in `meta.json`; GC after every commit and merge deletes replaced files (`src/indexer/segment_updater.rs:431-455`, `src/directory/managed_directory.rs:109-152`). *Mechanism:* in-process, a held `Searcher`; across processes, none supported: one directory per generation, or an unsupported no-delete `Directory` plus saved metas.
- **Soft-deleted documents.** Native tombstones (alive bitsets) fit, but only "as of the latest commit". *Mechanism:* `SegmentReader::open_with_custom_alive_set` (public, intersects masks, `src/index/segment_reader.rs:147-185`) with the product running weights and collectors per segment itself (`Searcher` construction is `pub(crate)`).
- **Never freeze the host.** Cap worker threads, merge threads (default 4), executor threads, aggregation memory.

## 4. Quality-evaluation design (its own variant)
- **Variants:** baseline unicode61; stacked camelCase parts (`--tokenizer code`, parts at the token's position; note they lengthen fieldnorms, `src/postings/postings_writer.rs:155`); side fields `name_parts`, `qualified_name_parts` (parts only, own boost 2) so baseline statistics stay intact.
- **Queries:** the 30 contract queries (must lose no hits), plus an identifier band from `docs.jsonl` names with ≥2 camel parts across df bands; per identifier: whole word, one inner part, parts as words, parts as a phrase (query-side offsets via `PhraseQuery::new_with_offset`).
- **Judgments without labels:** known-item targets = documents whose `name` equals the identifier (`name_s` fast field). Report success@1/@10/@200, reciprocal rank, hits, Jaccard and rank-biased overlap with baseline, hits lost; costs IT, D (space_usage), QL.
- **Same harness, other quality variants:** flat vs nested (Q15), DisMax t∈{0.1,0.3} unpruned (Q11), alive statistics (S2), fieldnorms off (F5); product mode's page is the reference. Every variant ranks with `(score desc, id asc)`.

## Raw notes
This folder holds the feature-matrix pass: 10 settings and fields · 11 tokenizers · 12 queries (DisMax probe) · 13 scoring (deleted-documents probe) · 14 collectors and aggregations (tie-break probe) · 15 search runtime (with snippets) · 16 directories and storage · 17 product-rule mechanisms · 18 quality design. A second pass over the same source, with longer quotes, is in `../features/`: 01 indexing · 02 settings and fields · 03 tokenizers · 04 directories and storage · 05 queries · 06 scoring · 07 collectors and aggregations · 08 search runtime · 09 product-rule conflicts. Rows I1–I13 above carry this pass's indexing findings; `../features/01-indexing.md` has the full note.
