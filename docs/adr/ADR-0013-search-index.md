# ADR-0013: The lexical tier is an embedded search index, scored by our own BM25F

## Status

Accepted, 2026-10-08, by the owner after the measurement below. Amended 2026-10-09 by the owner's ruling: the
code-aware tokenizer form is adopted (decision 6), and the visibility route is the alive set (decision 3). Amended
2026-10-10 to the built design: visibility is per document (decision 3), the statistics come from the library over
the alive set (decision 2), the writer's lifecycle and the reconcile steps (decision 4), the order inside the exact
tiers (decision 5), the document length under the code tokenizer (decision 6) and the crate's place and build
(decision 1). One choice waits on the quality benchmark over real task questions, and names the result that decides
it: whether document length is weighted per column (decision 2).

This record replaces [ADR-0007](ADR-0007-lexical-first-page.md) in full. It replaces the lexical half of
[ADR-0003](ADR-0003-storage-tier2.md) and amends [ADR-0001](ADR-0001-scale-posture.md) §2.4: a ranked search serves its
best results, not every match (decision 5).

## Context

The lexical tier answers `codectx search` and the lexical part of every context answer. It ranks search documents (one
per declaration, plus file and manifest documents) with BM25F over five columns, `name`, `qualified_name`, `signature`,
`path` and `body`. The weights are 5, 5, 2, 2 and 1, with k1 1.2 and b 0.75. Scores are f64, quantized to 1e-6, and
ties break on content only. Statistics count visible documents only.

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
identical on 22 of 30. All 8 differences come from folding, because today's arm records its unfolded, document-level
page: on 3 rare queries (4 to 7 documents folding to 2 to 5 results) the pages diverge at position 3 or 5, and on the
other 5 at positions 117 to 172.

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
- The crate lives in `internal/search/textindex/`, beside the cgo package of the same name that calls it: its
  `Cargo.toml`, `Cargo.lock`, `rust-toolchain.toml`, `.cargo/config.toml` and `src/`. It builds as a static library,
  `libcodectx_textindex.a`, into the crate's own target directory `_build/`, which Go's `./...` patterns skip.
- The build is two steps: `go generate ./internal/search/textindex` runs `cargo build --release --locked`, and then
  `go build` links the archive. The toolchain is pinned by `rust-toolchain.toml`, and release and CI legs install
  exactly that version.
- The crate tree is locked (`Cargo.lock`); `--locked` refuses a build whose lock would change, Cargo verifies each
  crate against the checksum the lock records, and the release profile and the generate step's
  `--remap-path-prefix` of the home directory are set so that one commit builds the same archive bytes on any host. Every crate the archive links is in the license inventory.
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
| A one-byte length code, exact to 40 and then up to about 11% low | The exact token counts, one per column | Rounded lengths add ties, and a merge re-estimates the average from the rounded values. |

The collector runs inside the library's search loop. For each matching document it reads:
- the per-column term frequencies, from the postings;
- the five per-column token counts (`len_name`, `len_qualified_name`, `len_signature`, `len_path`, `len_body`),
  `kind`, the path and the content tie keys, from fast fields.

The score is BM25F: weighted term frequency wtf = Σ_c weight(c)·tf(c), one saturation
wtf·(k1+1)/(wtf + k1·(1 − b + b·dl/avgdl)), times idf = ln(1 + (N − df + 0.5)/(df + 0.5)), summed over the query's
terms in query order. A document is a candidate when some term has wtf > 0. The score is quantized as
round(score·10⁶), and a non-finite score is 0.

The statistics are exact for the generation being searched, and the library computes them over the generation's
alive set (decision 3):
- N, the sum of total lengths and the sum of weighted lengths are counted once per (generation, searcher) and cached
  with it; SQLite keeps no per-unit totals;
- df is counted per query term over alive postings. A segment that hides and deletes nothing answers df from its
  block summary (decision 5) instead.

The arithmetic is in the crate, with the logarithm implementation pinned: the argument reduction and polynomial of
Go's pure-Go `math.Log`. Go's `math.Log` and Rust's `f64::ln` differ in the last bit on 1 891 of 200 127 inputs, so
neither is used. Go scores nothing in production; a Go copy of the formula and the pinned logarithm lives in the
tests, written with an explicit conversion on every product so the compiler cannot fuse a multiply-add, and a shared
test vector proves the two agree to the quantum.

**Pending, document length weighted per column.** The BM25F paper uses Σ weight(c)·length(c) as the document length,
and so does the major engine that implements it. Ours uses the unweighted total: the token count over all five
columns. This is a departure, not a measured loss. Both forms are built: every column's token count is a fast field,
and each block summary stores the minimum document length under both forms, so the form is chosen per query and early
stopping stays exact under either. The quality benchmark over real task questions decides it, and the chosen form
then becomes a constant. Until then the choice is a fixed internal switch of the benchmark harness, never a user
setting.

## Decision 3: one index holds every retained unit; a generation is a visibility set

Retention stays: branch switching, pinned reads and cursors, and reattached hidden units all depend on it.

The library keeps only its latest commit. So a snapshot is not an index directory. A snapshot is a set of visible
documents over one index:
- A unit's documents are added once, when the unit is first built. Each carries a `doc_id`, a 64-bit number the store
  allocates from a monotonic sequence and never reuses, and the adding unit's content-keyed digest in `unit`. Both are
  indexed and fast fields.
- A generation's visible set is a bitmap over doc ids (bit id mod 8 of byte id/8, least significant bit first) of
  every `search_units.doc_id` row of the generation's member units. It is computed in the activation transaction and
  stored in SQLite (`generation_search.visible`).
- A view of the generation turns the bitmap into each segment's alive set, read from the `doc_id` column: a document is
  alive when it is not deleted and its bit is set. Hidden documents are never candidates. They never count in N, in
  either length sum or in df.
- A delta carries a unit by listing it in the new generation and copying the identity rows it keeps, never by writing
  its documents again. Only units the delta rebuilds add documents.
- A document is deleted when collection removes the last `search_units` row that names its doc id. The `unit` column
  counts a unit's documents for the reconcile (decision 4) and deletes a unit's documents by term.
- The index is one directory beside the store's database, `<database>.search`, owned by the store: it is created,
  recovered (decision 4) and removed with it.

**Visibility is per document, not per unit.** A delta carry-over copies only part of its predecessor's documents: it
drops the documents of replaced files and of nodes that no longer exist. A carried unit's documents were added by its
predecessor, so a set of visible units would show every document the predecessor added, including those the carrying
unit dropped. A set of doc ids shows exactly the rows the generation holds.

Measured on the fixture, 18 queries: our scoring with 10% and with 50% of units hidden returned pages identical to an
index built from only the visible documents, 18 of 18 both times. The library's own scoring did not, because its
statistics still count hidden documents.

**Alternatives.**
- *One index directory per generation.* It copies every unchanged segment, and the library offers no supported way to
  share segments across indexes.
- *A directory that never deletes, plus saved commit metadata.* It works against the library's design.

**The visibility route is the alive set.** On kubernetes (546 613 documents, 30 queries, 0%, 10% and 50% of units
hidden) the alive set, a query wrapper and the collector gave identical pages and times within noise of each other.
The alive set is the library's own mechanism for documents a searcher must not see, so the collector and the block
bounds never meet a hidden document. Building one generation's set costs 10–22 ms per searcher, more than a warm
query, so it is built once per (generation, segment) and cached with the searcher, the cache bounded by the query
memory budget (`resources.query_memory_bytes`).

## Decision 4: writing, committing and recovering

- **Writer.** The library's multi-threaded writer. Its thread count, its merge threads and the search threads are
  the machine's CPU count; its memory is the library's per-thread floor times its threads, admitted through the
  admission ledger before the writer opens and given back when it closes. The number of searches running at once is
  the machine's query slots; a search past them waits for one. There is no fixed constant.
- **Writer lifecycle.** The writer is opened by the first call that changes the index and stays open across a run.
  It is released after each activation's commit, and after the commit that ends a collection sweep run outside an
  activation, so another process can write; a release waits for running merges. A store that closes while its
  writer still holds documents of units a run sealed but never activated commits them first, with the active
  generation as the payload: they stay invisible until a generation that selects their units activates, and the
  next run reuses those units instead of rebuilding them. A store opened read-only never takes the writer.
- **Commit cadence.** One commit per generation activation, never one per unit. A commit costs one fsync, so a commit
  per unit cost 954 s on kubernetes. Documents added between commits are invisible until their generation activates.
- **Merging.** The library's background merges replace the lexical merge at activation. Activation no longer touches
  postings.
- **Crash order.** The library commits first, with the generation's id in decimal as its payload; queued document
  deletes are applied in that commit. The SQLite activation transaction follows: it stores the generation's visible
  bitmap, clears its member units' pending marks and removes the applied delete rows. So a generation SQLite has
  activated is always committed in the library. Each unit carries a pending mark, holding the count of documents it
  added, from the write of its rows until the activation that commits them.
- **Reconcile.** On start, a store that writes:
  1. applies the queued document deletes;
  2. deletes every library document whose doc id is at or past SQLite's next unallocated doc id, which a lost SQLite
     transaction allocated;
  3. for every unit still marked pending, compares the library's count of the unit's documents with the unit's own
     count; on a mismatch it fails the unit, with every unit whose rows name one of its doc ids, and those units are
     re-planned like any other unsealed unit;
  4. commits, with the active generation as the payload, and releases the writer.

  A payload naming a generation older than the active one means the index directory was lost or replaced. The store
  still opens; every search then answers `CTX_STORAGE_CORRUPT` with a re-index remediation. A read-only open of a
  database whose index directory is missing answers the same way.

## Decision 5: a ranked search serves the best results, not every match

A ranked search answers with the best-scoring results, a page at a time. It does not enumerate every matching
document. The product's no-default-limit rule (ADR-0001 §1.1) governs the data an index produces: files, units, facts,
rows and exact lookups are never dropped or truncated. A ranked list is a different thing. Its value is its head, and
no search a person or an AI client runs needs the 253 589th-best match for `vendor`.

- **Top-k collection.** A request asks for a depth of folded results, and the library returns the best `depth` by
  (quantized score, content tie key) and whether a further result exists, with a heap of exactly depth + 1 entries.
  A page after `served` results asks for depth served + limit + 1, so page n of 200 holds the results ranked
  200·(n − 1) + 1 to 200·n, whatever the match count.
- **Pruning.** Block-max WAND over blocks of 128 documents, with BM25F upper bounds per block of the union postings,
  skips every document that cannot enter the top k. A block is skipped only when its bound, widened by a relative and
  an absolute margin of 10⁻⁹ against rounding, is strictly below the heap's (depth + 1)-th score. A phrase term takes
  its bound from the block's exact phrase frequencies, never from the stored summary. A minimum score floors the
  results and skips every block whose bound is below it. It changes the work, never the results: the top k equal exhaustive scoring's top k. The
  library's own pruning returned pages equal to exhaustive scoring, with 0 misses, on 2- and 11-segment indexes, at
  about 0.5 ms a page whatever the page number. Our formula's pruned collector returned pages 1 to 3 identical to
  exhaustive scoring on 2 436 of 2 436 query-page pairs on a fixture, at 1 and 4 search threads, and on kubernetes
  3 030 of 3 030 pages over four query sets, 15.7 times faster summed on the 30 speed queries (4 653 ms to 296 ms). The library's pruned disjunction-max query is wrong in this release
  (the fix is merged upstream but not released), so pruning is built for our formula and never borrowed from the
  library's queries.
- **Match count.** The count is exact when no block was skipped, and otherwise a lower bound marked as one. That is
  how the major engines report it.
- **Folding.** Documents that share a result key (the node, or the path and start byte of a document with no node)
  fold into one result whose score is the group's highest; its representative is the group's least document under
  the content comparator. The heap holds one entry per key and keeps that maximum, so a page is 200 distinct results.
  Pruning can skip a group member only when it cannot raise its group's score or enter the page, so the folded score
  is exact. A folded result's occurrence count is exact when no block was skipped, and otherwise a lower bound, like
  the match count.
- **Ties.** The content comparator is score descending, then path, start byte, end byte, kind, name, qualified name,
  signature, node id and search key. Ties at the k-th entry are settled by it inside the heap of depth + 1 entries,
  never collected in a list.
- **Paging.** A cursor carries its position by content, as a keyset: in the exact phase, the tier and content key of
  the last exact result served; in the lexical phase, the number of lexical results served. The query hash it is
  bound to covers the query, its filters, the minimum score and the length form, so a cursor made under one minimum
  score never resumes under another. Filters the library does not apply (the language filter, and the drop of a
  lexical result already served by an exact tier) run after the page returns; while the filtered page is short and
  more results exist, the depth doubles and the page is asked again. Nothing is held between pages.
- **No spool.** The ranked set is never sorted to disk. ADR-0001 §2.4 is amended to this rule.
- **Block summaries.** Each segment has one summary file, `<segment id>.cxsum`, in the index directory. For every token
  of the five columns it holds the token's df over the segment's documents and, per block holding the token, the
  maximum weighted term frequency and the minimum document length under both length forms. Deleted documents stay in
  it; they only loosen a bound. A header names the segment and its document count; a mismatched or unreadable file
  is no summary, and that segment is scored exhaustively, so a stale summary is never read. A summary is built for
  every searchable segment that lacks one at each commit, after each merge (a writer release waits for merges), and
  by a forced merge; the summaries of segments no longer present are removed.
- **Bounds.** Ties are settled by the heap and the content comparator. A summary is read through a memory map with a
  binary-searched term table, so nothing per segment stays in the heap, and the scoring scratch is per block (128
  documents by the query's terms), never per segment. The writer's memory, its merge and search threads and the
  number of searches running at once come from the machine and the admission ledger (decision 4).
- **Deadline.** A query deadline that ends the library's search serves the best results found by then, truncated with
  the deadline as its reason and with no continuation. A caller's cancel is an error.

The exact tiers (path and symbol lookup) come first and are not ranked: each returns every match, ordered by the
content key (path, start byte, end byte, kind, name, qualified name, signature, node id), with score 0. A lexical
result whose node an exact tier served for the same query is dropped from the lexical sequence.

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

**The code-aware form is adopted.** This form emits the whole identifier, and also its camelCase and snake_case parts at
the same position. For example, `NewController` is indexed as `newcontroller`, `new` and `controller`. Under the
library's scoring it lifted the subword set from 0.60 to 0.70 MRR@10 (Recall@200 0.22 to 0.66), and it held the
other two sets. Under our scoring on kubernetes it lifted subword from 0.379 to 0.500 MRR@10 (Recall@200 0.214 to 0.630, zero-hit
queries 13 to 0), and cost name 0.023 and doc 0.009. The owner adopted it on that measurement: partial-name search is
the gap the product most needs closed, and whole identifiers keep their exact and prefix tiers ahead of it. The literature supports the whole-plus-parts form. On Go, it raised NDCG@10 from 0.309
to 0.563 in one 2026 preprint, while parts alone scored 0.469.

**As built.** Parts are emitted for a token whose spelling has at least two camelCase parts; snake_case needs nothing
more, because `_` already separates. On the query side an unquoted word yields the whole token and each of its parts
as separate terms, any of which may match; a quoted run is one phrase of whole tokens. A repeated term counts once.
A phrase's frequency in a column is the number of positions where its tokens stand in order, with no offset cap.

A document's length is counted by the same tokenizer when the document is added: per column, its token count with
the parts included, stored as the five fast fields of decision 2. Under the unweighted form the length is the total
over the five columns; under the weighted form it is Σ weight(c)·count(c).

## Decision 7: filters and new query features come from fast fields

`kind`, the path, the unit and the doc id are fast fields:
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
| The Go posting and statistics readers, the document-frequency cache | yes | the library counts the statistics over the alive set |
| The ranked-set sort and spool to disk | yes | decision 5 |
| `search_units` text columns and token count | yes | the library stores what it serves |

What stays in SQLite:
- each search document's identity row, from search key to node, file, byte range and `doc_id`;
- the doc-id sequence, each unit's pending mark until its documents are committed, and the queue of document deletes
  awaiting the next library commit;
- each generation's visible bitmap;
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
