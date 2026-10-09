# ADR-0013: The lexical tier is an embedded search index, scored by our own BM25F

## Status

Accepted, 2026-10-08, by the owner after the measurement below. Two choices inside the decisions wait on
measurements that are running on the same export, and each names the result that decides it:
- whether the code-aware tokenizer replaces the plain one (decision 6);
- whether document length is weighted per column (decision 2).

This record replaces [ADR-0007](ADR-0007-lexical-first-page.md) in full. It replaces the lexical half of
[ADR-0003](ADR-0003-storage-tier2.md) and amends [ADR-0001](ADR-0001-scale-posture.md) §2.4: a ranked search serves its
best results, not every match (decision 5).

## Context

The lexical tier answers `codectx search` and the lexical part of every context answer. It ranks search documents (one
per declaration, plus file and manifest documents) with BM25F over five columns, `name`, `qualified_name`, `signature`,
`path` and `body`. The weights are 5, 5, 2, 2 and 1, with k1 1.2 and b 0.75. Scores are f64, quantized to 1e-6, and
ties break on content only. Statistics count visible documents only. The scorer is `internal/search/bm25.go`.

Today the tier stores everything in SQLite:
- a full-text table for phrase matching;
- packed per-unit posting segments, sealed with each unit and merged into a generation at activation;
- the `search_units` document rows.

A query reads every posting of every query term, ranks every candidate, sorts the full candidate set on disk and then
serves page 1.

The owner asked for this tier to be measured against an embedded search library, and for every one of its features to
be considered. The question was not whether the library obeys our rules. Each rule was audited on its own merits
(research 22).

**What was measured.** The input was kubernetes: 546 613 search documents in 47 995 units, exported by the production
filesystem and tree-sitter providers and checked against a stored run of the same commit. The query set has 30
queries, in bands by document frequency, plus two-term queries and phrases. Quality was measured on three known-item
sets of 1 000 queries each, derived from the export with a fixed seed:

| Set | The query | Measures |
|---|---|---|
| name | a declaration's exact name | exact-identifier lookup |
| doc | the declaration's 4 rarest body words that are absent from its name, path and qualified name | concept search over documentation |
| subword | one camelCase or snake_case part | partial-identifier search |

The full record, with every command and raw result line, is
[`docs/research/22-search-library-evaluation.md`](../research/22-search-library-evaluation.md).

| Measure | Today | The library |
|---|---|---|
| Index build, all documents | 400.7 s wall, 593.7 s CPU, 2.45 GB peak | 9.0 s wall, 48.8 s CPU, 0.85 GB (16 threads, one commit); 18.1 s and 0.49 GB on 1 thread |
| Index on disk | about 641 MB live, plus 2.12 GB of superseded per-unit segments until collection | 337–387 MB |
| Activation | 165.4 s, mostly the lexical merge | none: segments merge in the background |
| 30 queries, first page of 200, warm | 47.4 s | 4.04 s under our exact scoring in a custom collector; 0.13 s scoring every match under the library's own model; 0.016 s for the library's pruned top 200 |
| The 5 most frequent terms | 4.15–7.11 s each | 413–709 ms each, under our scoring |
| Kind filter / folder filter | 730 ms / 748 ms | 18.6 ms / 5.5 ms, under our scoring |
| A commit per unit | — | 954 s for 47 995 commits (about 20 ms of fsync each) |

Under our exact scoring, the library returned the same folded hits as today on 30 of 30 queries. The first page was
identical on 22 of 30. The other 8 diverge at position 116 or later, because today's arm records an unfolded,
document-level page.

| Quality (MRR@10 / Success@10) | name | doc | subword |
|---|---|---|---|
| Today, and our scoring inside the library | 0.83 / 95% | 0.68 / 87% | 0.38 / 65% (Recall@200 0.21) |
| The library's own per-column scoring | 0.90 / 99% | 0.02 / 9% | 0.60 / 76% |
| The same, with the code-aware tokenizer | 0.91 / 99% | 0.05 / 12% | 0.70 / 93% (Recall@200 0.66) |

The slowness today is not single-threading. It comes from four things:
- two indexes over the same text;
- SQLite page round trips for every posting;
- a full sort spooled to disk before page 1;
- statistics recomputed per query.

The library's index removes the first three, and its fast columns remove the fourth.

**A caveat on the doc set.** The doc set is built so that the target matches in its body only. That is the case where
per-column scoring fails. The library gives each column its own document frequency, so a word that is rare in `name`
gets an IDF of about 5.3 there, against about 1.04 in the body. With the 5× boost, one stray name match outranks the
target's whole body match. The set measures a real search, concept words that appear only in a declaration's
documentation. It is not a neutral average, and it favours a model with one saturation over all columns.

## Decision 1: the postings live in an embedded search library, linked into the binary

The tier's index is the library's index:
- compressed postings with frequencies and positions;
- immutable segments, read through memory maps;
- a dictionary per column;
- fast (columnar) fields.

The library is a Rust crate, built as a static library in this repository and linked through cgo into the one codectx
binary. The binary already needs cgo for its parser grammars, and every release leg already builds natively.

The boundary is coarse:
- add one unit's documents;
- delete units;
- commit with a payload;
- open a generation's view;
- answer one page.

A page comes back as one packed buffer, so the cost of crossing (about 30–40 ns a call) is paid a handful of times per
query, never once per hit. Every export catches a panic, and an error returns as a code and a message.

**Alternatives.**
- *Keep the SQLite tier and tune it.* Every measured cost is structural to it: two indexes, a page round trip per
  posting, the external sort before page 1. Rejected.
- *A sidecar process over a socket.* It contains crashes, but adds a second binary to ship, serialisation on every
  page and its own lifecycle. The coarse batch boundary makes in-process calls cheap, and two production Go programs
  ship this library through cgo. Rejected.
- *A pure-Go search library.* It scores per column, as the library does, and has no published performance evidence. No
  arm was built.
- *A JVM search library.* It would bring back a runtime that ADR-0012 retires.
- *An engine that keeps two revisions or caps hits.* It breaks retention or completeness.
- No engine surveyed beats this library for an exact BM25 tier (research 22, §engines).

**Consequences.**
- Release and CI legs need a pinned Rust toolchain.
- The crate tree is locked (`Cargo.lock`), vendored for reproducible builds and added to the license inventory.
- Windows links against Go's MinGW toolchain, so the crate builds for the GNU targets: `x86_64-pc-windows-gnu`, and
  `aarch64-pc-windows-gnullvm` on arm64.
- A Rust panic that escapes an export aborts the whole process. A catch at every export is therefore part of the
  boundary's contract, and a test asserts it.
- An allocation failure inside the library aborts and cannot be caught. Decision 4 bounds the writer's memory, which
  is what makes such an abort a host failure and not a design one.

## Decision 2: ranking stays ours, computed in a custom collector

The library's own scoring was audited and not adopted.

| What the library does by default | What we keep | Why |
|---|---|---|
| One BM25 per column, summed | BM25F: weighted term frequency across columns, one saturation | Doc-set MRR@10 is 0.68 against 0.02. The BM25F paper, and the combined-field queries of two major engines, exist for this reason. |
| f32 scores | f64, quantized to 1e-6 | An f32 step is 1.9e-6 once a score reaches 16, too coarse for a 1e-6 grid. |
| Ties broken by internal document address, which changes with threads and merges | Ties broken on content only | Pages and cursors must repeat across builds. |
| Statistics that count deleted documents until a merge | Exact statistics over visible documents | A delta must equal a fresh index (decision 3). |
| A one-byte length code, exact to 40 and then up to about 11% low | The exact `token_count` | Rounded lengths add ties, and a merge re-estimates the average from the rounded values. |

The collector runs inside the library's search loop. For each matching document it reads:
- the per-column term frequencies, from the postings;
- `token_count`, `kind`, the path ordinal and the search key, from fast fields.

The statistics are exact for the generation being searched:
- N and the total token count are sums of per-unit totals held in SQLite;
- df is counted over visible postings.

Score arithmetic is the formula in `bm25.go`, carried into Rust, with the logarithm implementation pinned. Go's
`math.Log` and Rust's `f64::ln` differ in the last bit on 1 891 of 200 127 inputs, so one implementation is used on
both sides. A shared test vector proves equality.

**Pending, document length weighted per column.** The BM25F paper uses Σ weight(c)·length(c) as the document length,
and so does the major engine that implements it. Ours uses the unweighted total. This is a departure, not a measured
loss. The weighted form is adopted if it raises MRR@10 on two of the three sets and lowers it on none. Otherwise the
unweighted total stays.

## Decision 3: one index holds every retained unit; a generation is a visibility set

Retention stays: branch switching, pinned reads and cursors, and reattached hidden units all depend on it.

The library keeps only its latest commit. So a snapshot is not an index directory. A snapshot is a set of visible
units over one index:
- A unit's documents are added once, when the unit is first built. The `unit` field identifies them, and its digest is
  content-keyed.
- A generation's visibility set comes from SQLite's list of the generation's units. It is resolved once per term
  ordinal of the `unit` fast field and expanded per segment into a bitset over document ids (the fixture: 0.08–0.2 ms
  per searcher).
- It is applied as each segment's alive set, through the library's custom-alive-set reader, or inside the collector.
  Hidden documents are never candidates. They never count in N, in the total token count or in df.
- A unit's documents are deleted only when collection frees the unit. Deletion is by term on the `unit` field.

Measured on the fixture, 18 queries: our scoring with 10% and with 50% of units hidden returned pages identical to an
index built from only the visible documents, 18 of 18 both times. The library's own scoring did not, because its
statistics still count hidden documents.

**Alternatives.**
- *One index directory per generation.* It copies every unchanged segment, and the library offers no supported way to
  share segments across indexes.
- *A directory that never deletes, plus saved commit metadata.* It works against the library's design.

**Pending: the visibility route** (alive set, query wrapper or collector). It is chosen by its kubernetes time at 10%
and 50% hidden. All three routes give identical pages.

## Decision 4: writing, committing and recovering

- **Writer.** The library's multi-threaded writer, with its thread count and memory budget derived from the admission's
  measured need, like every other worker. Merge threads and the search executor are governed the same way. There is no
  fixed constant.
- **Commit cadence.** One commit per generation activation, never one per unit. A commit costs one fsync, so a commit
  per unit cost 954 s on kubernetes. Units added between commits are invisible until their generation activates.
- **Merging.** The library's background merges replace the lexical merge at activation. Activation no longer touches
  postings.
- **Crash order.** The library commits first, with the generation's id as its payload. SQLite activates the generation
  after that. So a generation SQLite has activated is always committed in the library. On start, reconciliation:
  1. deletes documents of units that no SQLite generation references;
  2. re-plans any unit whose documents never reached a commit, like any other unsealed unit.

## Decision 5: a ranked search serves the best results, not every match

A ranked search answers with the best-scoring results, a page at a time. It does not enumerate every matching
document. The product's no-default-limit rule (ADR-0001 §1.1) governs the data an index produces: files, units, facts,
rows and exact lookups are never dropped or truncated. A ranked list is a different thing. Its value is its head, and
no search a person or an AI client runs needs the 253 589th-best match for `vendor`.

- **Top-k collection.** Page n is collected as the top 200·n by (quantized score, content tie key), with a heap of
  that size. Page 1 holds 200 entries, whatever the match count.
- **Pruning.** Block-max WAND, with BM25F upper bounds per block of the union postings, skips every document that
  cannot enter the top k. It changes the work, never the results: the top k equal exhaustive scoring's top k. The
  library's own pruning returned pages equal to exhaustive scoring, with 0 misses, on 2- and 11-segment indexes, at
  about 0.5 ms a page whatever the page number. Our formula's pruned collector returned pages 1 to 3 identical to
  exhaustive scoring on 2 436 of 2 436 query-page pairs on a fixture, at 1 and 4 search threads; its kubernetes run is
  next. The library's pruned disjunction-max query is wrong in this release
  (the fix is merged upstream but not released), so pruning is built for our formula and never borrowed from the
  library's queries.
- **Match count.** The count is exact when every match was visited, and otherwise a lower bound marked as one. That is
  how the major engines report it.
- **No spool.** The ranked set is never sorted to disk. ADR-0001 §2.4 is amended to this rule.

The exact tier (path and symbol lookup) is not ranked and keeps returning every match.

**Alternatives.** *Score every match and keep a heap after a cursor, so every page down to the last match is
reachable.* It costs a full scoring of every match for every page (4.04 s for the 30 queries, against milliseconds
pruned). That is a price paid for depth no search uses. Rejected.

## Decision 6: tokenization

The tokenizer reproduces today's rules:
- letters, numbers and private-use characters form tokens;
- everything else separates;
- case folds;
- diacritics are removed.

Every library tokenizer with a silent length cut is avoided. The library's default analyzer drops tokens of 40 bytes or
more.

**Pending: the code-aware form.** This form emits the whole identifier, and also its camelCase and snake_case parts at
the same position. For example, `NewController` is indexed as `newcontroller`, `new` and `controller`. Under the
library's scoring it lifted the subword set from 0.60 to 0.70 MRR@10 (Recall@200 0.22 to 0.66), and it held the
other two sets. Under our scoring it is being measured now. It is adopted if it lifts subword, holds name within 0.01
MRR@10 and does not lower doc. The literature supports the whole-plus-parts form. On Go, it raised NDCG@10 from 0.309
to 0.563 in one 2026 preprint, while parts alone scored 0.469.

## Decision 7: filters and new query features come from fast fields

`kind`, the path and the unit are fast fields:
- **Kind filter:** a set of term ordinals.
- **Folder filter:** a contiguous ordinal range of the sorted path dictionary.

Both are tested per document inside the collector, never as a separate scan. The library's fuzzy, prefix, regular
expression and phrase-prefix queries, its aggregations and its snippets become available as new features. They are
adopted one at a time and never switch on by default. Each uses an explicit limit where the library caps silently
(the phrase-prefix expansion of 50 terms, the terms aggregation's 10 buckets).

## Decision 8: what is removed

| Component | Removed | Reason |
|---|---|---|
| The full-text table | yes, with its document-size table | the library's positions serve phrases |
| The throwaway tokenizer table | yes | each document is tokenized once |
| Per-unit lexical staging databases, packed lexical segments, their parts and generation tables | yes | the library's segments hold the postings |
| Lexical compaction at activation | yes | about 2.1 GB of extra writes and 3 minutes per kubernetes run |
| The Go posting and statistics readers, the document-frequency cache | yes | fast fields and per-unit totals answer them |
| The ranked-set sort and spool to disk | yes | decision 5 |
| `search_units` text columns | yes | the library stores what it serves |

What stays in SQLite:
- each search document's identity row, from search key to node;
- per-unit document and token totals;
- everything that is not search: files, symbols, relations, evidence, snapshots and units.

The exact tier (path and symbol lookup) and the order "tiers first, then lexical score" are unchanged.

## Decision 9: what proves the change

Tests that name their mutation:
1. On a multi-language fixture, the collector's top k equals exhaustive scoring by the formula it replaces, with and
   without pruning.
2. A generation with hidden units ranks exactly like an index of only its visible documents.
3. A crash between the library commit and the SQLite activation recovers to the last activated generation, with
   nothing visible that it does not hold.
4. A panic inside an export returns an error and does not end the process.
5. A delta's pages equal a fresh index's.

The kubernetes measurement is repeated on the integrated binary and recorded in research 22.

## Sources

- Robertson and Zaragoza, *The Probabilistic Relevance Framework: BM25 and Beyond*, Foundations and Trends in IR
  3(4), 2009, §3.6.1, https://staff.city.ac.uk/~sbrp622/papers/foundations_bm25_review.pdf: BM25F, one saturation
  over weighted frequencies, and the weighted document length (decision 2).
- Zaragoza, Craswell, Taylor, Saria and Robertson, *Microsoft Cambridge at TREC-13: Web and HARD tracks*, TREC 2004,
  https://trec.nist.gov/pubs/trec13/papers/microsoft-cambridge.web.hard.pdf: the field-weighted model in practice.
- Combined-field query source, https://github.com/apache/lucene/blob/main/lucene/core/src/java/org/apache/lucene/search/CombinedFieldQuery.java:
  a major engine's BM25F, with weighted length (decision 2).
- Broder, Carmel, Herscovici, Soffer and Zien, *Efficient query evaluation using a two-level retrieval process*, CIKM
  2003, https://doi.org/10.1145/956863.956944: WAND, which is safe up to k (decision 5).
- Ding and Suel, *Faster top-k document retrieval using block-max indexes*, SIGIR 2011,
  https://research.engineering.nyu.edu/~suel/papers/bmw.pdf: block-max WAND returns the exact top k (decision 5).
- Kamphuis, de Vries, Boytsov and Lin, *Which BM25 Do You Mean? A Large-Scale Reproducibility Study of Scoring
  Variants*, ECIR 2020, https://doi.org/10.1007/978-3-030-45442-5_4: exact against quantized lengths (decision 2).
- Lin and Yang, *The Impact of Score Ties on Repeatability in Document Ranking*, SIGIR 2019,
  https://arxiv.org/abs/1807.05798: tie order and repeatability (decision 2).
- Hill, Binkley, Lawrie, Pollock and Vijay-Shanker, *An Empirical Study of Identifier Splitting Techniques*, Empirical
  Software Engineering 19(6), 2014, https://doi.org/10.1007/s10664-013-9261-0: camelCase and underscore splitting
  (decision 6).
- Radha and Goktas, *Improving BM25 Code Retrieval Under Fixed Generic Tokenization*, arXiv:2605.18561, 2026,
  https://arxiv.org/abs/2605.18561: whole identifier plus parts against parts alone (decision 6).
- The search library's source at release 0.26.2, https://github.com/quickwit-oss/tantivy/tree/0.26.2:
  - the scorer and its per-column statistics (`src/query/bm25.rs`);
  - the one-byte length code (`src/fieldnorm/code.rs`);
  - the custom-alive-set reader (`src/index/segment_reader.rs`);
  - the collector trait (`src/collector/mod.rs`);
  - the garbage collection of superseded commits (`src/indexer/segment_updater.rs`);
  - the silent caps named in decisions 6 and 7.
- The search library's fix for its pruned disjunction-max scoring, https://github.com/quickwit-oss/tantivy/pull/3022
  (decision 5).
- The Rust 1.81 release notes, https://blog.rust-lang.org/2024/09/05/Rust-1.81.0/: an uncaught unwind across a C ABI
  aborts (decision 1).
- Production embeddings of the library from Go: https://github.com/anyproto/tantivy-go and
  https://github.com/milvus-io/milvus (decision 1).
- Measurement record: [`docs/research/22-search-library-evaluation.md`](../research/22-search-library-evaluation.md)
  and its raw evidence under `docs/research/raw/search-library/`.
