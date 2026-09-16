# ADR-0007 — Lexical first page: packed term statistics at activation, rank before hydrate, heap-served first page

**Status:** Accepted, 2026-09-15 · **Informs:** ADR-0001 §2 (the page is the unit of memory), ADR-0003 §2.1
(the lexical tier holds no second copy of the source), ADR-0005 (the packed-at-activation shape this
record reuses) · **Inputs:** three search profiling rounds on the reference repository, whose
measurements are reproduced in full in the measurement record at the end of this document.

## Context

The lexical search answers its first page from a scorer that reads, per query term, every posting
instance of that term, the term's document frequency and the generation's document statistics, ranks
every candidate document, sorts the whole candidate set with the external merge sort, hydrates the
candidates and serves the first page. Two earlier rounds removed the consumer-side costs (a per-page
re-open of the posting scan, a by-rowid pending batch, a second sort pass) and left the first page at
1.24 s for `function` and 1.57 s for `Meteor` on the 13 223-file reference repository, against a 1 s
target, with every other measured term already under it. The rounds also rejected, by identity
argument, the two engine-side shortcuts: pushing ranking into the index engine's own ranking function
(a different scoring model) and a persisted index that reproduces the ranking (an approximation of
the served order). The product's rules bind this record: the served set and order are byte-identical
before and after; no candidate cap, no top-k approximation, no default limit; the first page comes
back complete under defaults with nothing to tune.

**Where the time goes (reference repository, 279 164 visible documents, 91 868 008 posting
instances, warm, three runs):**

| phase | measured | scales with |
|---|---|---|
| bare scan of every posting instance | 2.00 s (0.022 µs per instance) | instances |
| the same scan joined per instance to the document row and the generation-visibility row | 38.82 s (0.42 µs per instance) | instances × 3 b-tree descents |
| postings for one term, per instance: vocabulary row, document index, visibility probe | 3.04 page lookups and 23 virtual-machine steps per instance | instances of the term |
| document frequency per term | the same walk plus a temporary b-tree for `count(DISTINCT)` | instances of the term |
| document statistics | one probe per visible document | documents |
| `function`: 180 428 instances, 31 256 documents | 1.24 s first page | |
| `Meteor`: 67 385 instances, 46 487 documents | 1.57 s first page | |

Two facts decide the design. First, the posting path is linear in token instances and each instance
pays visibility resolution three times over; resolving visibility once is a 19× difference on the
same scan. Second, `Meteor` is slow for a different reason than `function`: it has a third of the
instances but half again as many candidate documents, so its cost sits after the postings, in
candidate hydration, ranking and the full sort, none of which the posting path touches.

## Decision 1 — packed per-generation term statistics, built at activation

Activation builds, after the packed adjacency and before the generation pointer flips, a packed
lexical side structure for the generation: for every single-token term, its document frequency and
its posting list as the exact per-document, column-ascending `(column, count)` sequence the scorer
reads today, chunked into parts on a document boundary exactly as ADR-0005 chunks adjacency lists,
behind a last-written commit row that also carries the generation's document statistics. The build
is one bare instance scan folded against a visibility bitmap resolved once, streamed to parts as the
scan advances; heap is one part plus one term's list. The scorer reads the packed form for
single-token terms and never steps the index tables for statistics; phrase terms keep the live path,
because a phrase needs offsets the packed form does not store.

**Identity.** The packed values are the scorer's inputs, not its outputs: the prototype compared the
ordered `(column, count)` sequence per document and the document frequency against the live path for
five terms on two stores and found zero mismatched documents. The ranking is unchanged because every
floating-point operation stays in the search package and consumes the same numbers in the same order.

**Alternatives.** *Batched or covering reads on the current tables* — rejected by measurement: the
cost is three descents per instance, not statement count; the posting scan is already one range scan
per term, and the vocabulary table is a virtual table with no covering index to offer.
*Persisting only the statistics* — rejected as dominated: it needs the same full instance pass this
decision needs and recovers 0.27 s of the 0.60 s the packed form recovers on `function`.
*A per-unit packed form sealed with the unit and merged at activation* — the shape that would make a
delta activation independent of repository size. Not chosen now: it is unmeasured, it needs a second
tokenisation per unit (through a temporary index table with the same tokenizer, so that the terms are
identical to the store-wide index), and the generation-level build already sits at the packed
adjacency's cost class. It becomes the decision if a delta activation on the reference repository
exceeds the bound in "What the change must show".

**Trade-off accepted.** Activation pays a full instance pass per generation. The prototype's fold
cost 47.9 s on the reference repository because it reused the read path's per-group allocation; the
production fold is a streaming writer over a term-sorted scan and is bounded below by the 2.0 s bare
scan. The build budget is 5 % of the index wall, the same budget the packed adjacency met at 1.4 %.

### Decision 1, amended the same day: the packed form is sealed per unit and merged at activation

The generation-level build was implemented and measured, and its trigger fired on the first
measurement. On the reference repository the build took 46.15 s, 6.0 % of the 763.8 s index wall,
and the cost is not the fold: it is the embedded engine delivering the vocabulary rows.

| what is stepped, reference repository, 91 868 008 instances | wall | per instance |
|---|---|---|
| `count(*)` over the vocabulary, engine only, no rows delivered | 6.56 s | 0.071 µs |
| one integer column per row through the database layer | 19.85 s | 0.216 µs |
| term, document and column per row through the database layer | 45.51 s | 0.495 µs |
| the same rows through the raw driver with one reused value slice | 37.94 s | 0.413 µs |
| the production build (fold, bitmap, part writes) | 46.15 s | 0.502 µs |

The build sits 0.6 s above the cost of merely receiving its rows, folding in SQL plans an unbounded
sorter (`USE TEMP B-TREE FOR GROUP BY`), and the raw driver saves a sixth. Nothing on the pinned
engine brings a whole-store row scan under the budget, and the scan would be paid again at every
activation, including a one-file delta. So the shape changes and the format stays:

- **At seal, per unit.** The unit's lexical documents are tokenised a second time into a temporary
  index table with the same tokenizer and column set, so the terms are identical to the store-wide
  index by construction; the temporary vocabulary, which holds only that unit's instances, is folded
  into the unit's packed term list (term directory, term text, per-document column-ascending
  `(column, count)` sequences, and the per-document attributes of Decision 2) and stored with the
  unit. A unit's list is built once, in the parallel seal phase, and is reused by every generation
  that carries the unit, exactly as its facts are.
- **At activation, per generation.** The visible units' term lists are merged term by term (a batched
  k-way merge whose memory is proportional to the batch, not to the unit count) into the generation
  structure of the original Decision 1: the same three streams, the same parts, the same reader.
  Visibility is resolved by construction, because only the generation's units are merged, and the
  document frequency of a term is the sum of its per-unit document counts, since a document belongs
  to exactly one unit of a generation.

**Consequences.** Sealing tokenises each document twice; the second pass touches only that unit's
rows, so its cost is spread across the seal workers and paid once per unit version. Activation
becomes a streaming copy bounded by the packed bytes of the generation rather than by row delivery,
and a delta activation does the same work as a full one on the packed streams only, with no row scan.
The per-unit lists add their packed bytes to the store, reported in the change's measurement. The
build budget stands at 5 % of index wall, and a delta activation must build the lexical structure in
at most three times the packed adjacency's build on the same store.

### Decision 1, amended a second time: the packed form is a set of immutable segments; activation names them

The first amendment was implemented and measured, and it failed the requirement it was chosen for:
an activation's cost must be proportional to what changed. On the reference repository the seal
folds cost 58.9 s and wrote 118 GB, because "tokenised a second time into a temporary index table"
meant a second full text index per unit, whose own internal merges rewrote the same postings
repeatedly; the activation then merged all 20 038 units' lists in three passes, 86.2 s — worse than
the 46.15 s whole-store build it replaced. Merging every unit at every activation is the same
whole-store rewrite in a different shape: on a monorepo, one saved file would rewrite gigabytes,
which is exactly the write amplification ADR-0008 and ADR-0009 removed from the ingestion path.

So the unit of the packed form becomes a **segment**, and an activation stops producing one.

- **A segment is an immutable packed structure over a set of documents**: the three streams of the
  original Decision 1 (`term.dir`, `term.text`, `post.list`) plus the per-document attribute stream
  of Decision 2, chunked into parts exactly as before, with postings inside a segment ascending by
  document rowid. `lexical_segments` carries a segment's term and document counts and its packed
  bytes; `lexical_segment_parts` holds the chunks; `segment_units` names the units whose documents
  it holds; `generation_segments` is one generation's set, in read order. There is no
  generation-level parts table any more.
- **The seal folds one segment per unit, from the tokenizer pass the seal already makes.** There is
  no second tokenisation and no second index. The pass that counts a batch's tokens reads its
  grouped instance vocabulary once and serves both answers from it: the per-document token counts,
  and one staged row per `(term, document, column)` group. The rows are appended, in arrival order,
  to a staging database of the unit's own under the data directory's temporary directory — never
  into the workspace database, whose write-ahead log would carry every staged row a second time —
  with durability off and no index during the load, paced to disk while the unit builds (ADR-0009).
  At seal, ONE ordered read of that staging, resolved against the document rowids the ingestion
  transaction assigned, is folded by the same writer that wrote the generation-wide form. That read
  is the only `ORDER BY` any plan of this store issues; the engine's external merge sort performs it,
  bounded by the staging's page cache and spilling under the same temporary directory. The staging is
  deleted at seal, at Abandon and at Fail. A unit that publishes no document folds no segment.
- **Activation adds segments; it never rebuilds.** A generation's set is its predecessor's set plus
  the segments of members whose documents no inherited segment already holds, recorded in
  `generation_segments`. A generation with no predecessor takes its members' segments alone. A
  delta's carried documents keep their document rowid and therefore the segment they were folded
  into, so the carrying unit is recorded as an owner of that segment and nothing is rewritten to
  carry a document forward. The invariant, tested: every visible document of a generation lies in
  exactly ONE of its segments.
- **Visibility is a per-generation document bitmap.** A document of an inherited segment whose unit
  left the generation is hidden by the bitmap; the segment is not rewritten to remove it.
- **Compaction by geometric partitioning, at activation, amortised.** While a size tier holds more
  than `r` segments, that tier's segments are merged into one — a streaming merge on
  (term, document rowid) that copies a term's posting bytes verbatim when only one input holds the
  term and drops hidden documents — as is any segment more than half of whose documents are hidden.
  The merged segment replaces its inputs in the activating generation's set ONLY; a retained
  generation keeps its own rows and is never touched. A document therefore pays O(log_r N) merges
  over its life, and a one-file delta pays for one small segment plus, occasionally, one tier merge.
- **Reads merge segments by document rowid.** A term is one binary search per segment over that
  segment's directory; the per-segment posting streams are merged so the candidate walk still
  receives one strictly ascending sequence of rowids, and a document two segments both claim is
  reported as a corrupt store rather than scored twice. Document frequency stays EXACT: a generation
  that hides nothing answers from the directory entries, and otherwise the posting is walked against
  the bitmap. Approximate frequencies that count hidden documents are rejected: a ranking would then
  depend on the store's history, and a delta-built store would not answer identically to a fresh one
  over the same tree.
- **Garbage.** A segment no retained generation names and no live unit owns is deleted in the same
  transaction as the generation or the unit that released it.

**Consequences.** Each document is tokenised exactly once, on the path that already tokenised it, so
the seal's added cost is one staged row per `(term, document, column)` group and no second index.
Activation writes the generation's segment list and, when a tier is full, one merged segment; it
never touches a byte of a segment it merely inherits, which is what makes a delta activation's cost
proportional to the delta. In exchange, a read pays one directory search per segment instead of one,
and a generation that has hidden documents pays a posting walk for a document frequency — both
bounded by the segment count, which compaction holds at O(log_r N). The build budget stands at 5 % of
index wall, and a delta activation must build the lexical structure in at most three times the packed
adjacency's build on the same store.

**Alternative considered.** *Keeping one structure per generation and rewriting it incrementally* —
the strongest competing shape, because a single sorted run is the cheapest possible read. Rejected:
any in-place rewrite of a sorted run is proportional to the run, not to the change, so it reproduces
exactly the cost this amendment exists to remove, and it cannot leave a retained generation's bytes
untouched. The trade-off accepted is the read-side merge across segments.

## Decision 2 — every candidate attribute comes from the packed per-document stream; no document row is read per candidate

Ranking needs, per candidate, more than a score: the deterministic tie-break and the deduplication key
read the document's identity, path, kind, name, qualified name, signature and byte range, and the
path and kind filters read two of those. Today those fields are hydrated with one document-row read
per candidate (`Meteor` reads 46 487 rows to serve 200). The packed structure therefore carries, once
per document and addressable by document ordinal, every field the search package reads from a
document row: row id, node id, search key, file id, path, kind, name, qualified name, signature,
start and end byte, token count. Candidates are hydrated from that stream, sequentially, in document
order, with no SQL statement per candidate; the document-row read path is deleted. The served page
is unchanged because the values are the same values, and the comparator and the deduplication key
are untouched. What Decision 2 removes is the per-candidate index descent, not the per-candidate
data.

## Decision 3 — the first page from a bounded heap; the full order sorted on the first continuation

The first request ranks the candidate stream through a bounded heap of one page plus one entry under
the same total order the external sort uses (score, then the canonical tie-break), and serves the
page from the heap while the scored candidates are spooled raw in the request's retained directory.
The full external merge sort runs once, on the first continuation request, over the raw spool, and
later pages are served by byte offset from the sorted run as ADR-0005 serves ranked pages. The heap
and the sort implement one comparator, so the first page is the sorted run's first page by
construction, and a test proves it by comparing the two on a fixture whose ties span the page
boundary. A single-page answer therefore never pays the sort; a multi-page answer pays it exactly
once, on the request that needs it. This is not a top-k cap: every candidate is scored, spooled and
served.

**Alternative.** *Sort first, as today.* Rejected: the sort is paid on every first page for a
result set that is complete only across pages, and it is the largest surviving `Meteor` phase.

## What the change must show

On the reference repository, default configuration, every provider on, both binaries on stores built
from the same tree: every page of `function`, `Meteor`, `return`, `user` and `createAccount`
byte-identical between the two binaries; `function` and `Meteor` first page under 1 s warm and the
cold first page reported; the packed build at or under 5 % of index wall and a delta activation
(one file touched) at or under 3× the packed adjacency's build on the same store; store growth
reported; query peak RSS at or under today's; the heap-versus-sort identity test and the no-temp-b-tree
plan test mutation-proved.

## Sources

- ADR-0003 — Storage tier 2 — `docs/adr/ADR-0003-storage-tier2.md` (contentless lexical tier; why the postings are the only copy of the indexed text).
- ADR-0005 — Graph traversal layout — `docs/adr/ADR-0005-graph-traversal-layout.md` (packed-at-activation parts, ranked pages served by byte offset).
- SQLite FTS5 Extension, the fts5vocab virtual table module — https://www.sqlite.org/fts5.html#the_fts5vocab_virtual_table_module (the instance table the scan reads; why it offers no covering index).
- SQLite, EXPLAIN QUERY PLAN — https://www.sqlite.org/eqp.html (the plan lines reproduced above).
- SQLite, The Query Optimizer Overview — https://www.sqlite.org/optoverview.html (correlated scalar subqueries and temporary b-trees for `DISTINCT`).
- Vitter, External Memory Algorithms and Data Structures: Dealing with Massive Data — https://www.ittc.ku.edu/~jsv/Papers/Vit.IO_survey.pdf (external merge sort; why a single sorted run served by offset is the right shape for later pages).
- Knuth, The Art of Computer Programming vol. 3, §5.2.3 (heapsort and selection with a bounded heap) — https://www-cs-faculty.stanford.edu/~knuth/taocp.html (the bounded heap serves exactly the first page of the same total order).
- Nicholas Lester, Alistair Moffat, Justin Zobel, "Fast on-line index construction by geometric partitioning", CIKM 2005 — https://doi.org/10.1145/1099554.1099739 (the tiered merge schedule and its O(log_r N) amortised cost per document).
- Justin Zobel, Alistair Moffat, "Inverted files for text search engines", ACM Computing Surveys 38(2), 2006 — https://doi.org/10.1145/1132956.1132959 (index maintenance strategies; why rebuild-in-place is proportional to the index).
- Stefan Büttcher, Charles L. A. Clarke, Brad Lushman, "Hybrid index maintenance for growing text collections", SIGIR 2006 — https://doi.org/10.1145/1148170.1148233 (merging short and long posting lists under different schedules).
- Patrick O'Neil, Edward Cheng, Dieter Gawlick, Elizabeth O'Neil, "The Log-Structured Merge-Tree (LSM-Tree)", Acta Informatica 33, 1996 — https://www.cs.umb.edu/~poneil/lsmtree.pdf (immutable runs merged on a size schedule instead of updated in place).
- "TieredMergePolicy", search-engine library documentation, version 9.9 — https://lucene.apache.org/core/9_9_0/core/org/apache/lucene/index/TieredMergePolicy.html (a production tiered merge schedule).
- "Lucene90LiveDocsFormat", search-engine library documentation, version 9.9 — https://lucene.apache.org/core/9_9_0/core/org/apache/lucene/codecs/lucene90/Lucene90LiveDocsFormat.html (deletions as a live-document bitset, dropped at merge).
- Index file formats, segments, search-engine library documentation, version 9.9 — https://lucene.apache.org/core/9_9_0/core/org/apache/lucene/codecs/lucene99/package-summary.html (an index as a set of immutable segments).

## Measurement record

Every number this record relies on, so that it stands alone. Reference repository: 13 223 files,
522 939 613 source bytes, 279 164 visible lexical documents, 91 868 008 posting instances; index
wall 763.8 s under the default configuration with every provider on; store 2.29 GB (database
1.97 GB). Fixture: a 210-file repository with 5 279 documents and 260 020 instances. Queries warm,
three runs after a discarded warm-up, page size 200.

**Round 1 (consumer side).** Removing the per-page re-open of the posting scan and the by-rowid
pending batch took `function` from 1.70 s to 1.27 s and `Meteor` from 2.11 s to 1.18 s; every page
of `return` (41 pages, 8 020 items), `user` (35 pages, 6 904 items) and `createAccount` (1 page,
6 items) was byte-identical before and after. Profile of `function` after the round: ranking 80 %
of the process, of which the lexical tier 0.93 s (posting walk 0.33 s, document frequency 0.18 s,
document hydration 0.16 s, match 0.09 s, statistics 0.09 s, emit 0.08 s) and the collector's
external sort 0.21 s. Two engine-side shortcuts were rejected: pushing ranking into the index
engine's own ranking function (a different scoring model: different inverse-document-frequency and
saturation constants, no per-column weights) and a persisted index that reproduces the ranking (an
approximation of the served order).

**Round 2 (storage side).** Query plans of every statement the lexical tier issues: the posting
scan is a virtual-table scan of the vocabulary joined per instance to the document row by
`idx_search_doc` and, through a correlated scalar subquery, to the generation-visibility row by its
covering unique index; document frequency is the same plan plus `USE TEMP B-TREE FOR count(DISTINCT)`;
document statistics probe the visibility row once per visible document. One statement issued twice
in one connection on the fixture: 2 163 rows, 6 539 page-cache hits, 56 misses cold and 0 warm,
49 765 virtual-machine steps both times: 3.04 page lookups and 23 steps per instance, unchanged by
warming. On the reference repository: bare instance scan 2.00 s (0.022 µs per instance); the same
scan joined per instance 38.82 s (0.42 µs per instance), a 19.4× difference; postings for
`function` / `meteor` / `return` / `user` step 180 428 / 67 385 / 131 472 / 41 408 rows in
0.077 / 0.042 / 0.058 / 0.028 s; document frequency over the same rows (31 256 / 46 487 / 9 221 /
7 650 documents) 0.085 / 0.046 / 0.067 / 0.026 s with a temporary b-tree each; statistics over
279 164 documents 0.084 s.

**Prototype of Decision 1.** Visibility resolved once into a document bitmap (fixture 5 279
documents in 672 bytes; reference 279 164 documents in 34 904 bytes), then one bare instance scan
folded into per-document column-ascending `(column, count)` sequences and per-term document
frequencies: fixture 5 922 terms in 164 ms; reference 619 941 terms in 47.93 s using the read path's
per-group fold. Identity against the live path, comparing the ordered sequence per document and the
frequency: `function` 31 256 / 31 256 / 31 256, `meteor` 46 487 / 46 487 / 46 487, `return`
9 221 / 9 221 / 9 221, `user` 7 650 / 7 650 / 7 650 (live documents / live frequency / packed
frequency), zero mismatched documents on both stores.

**Baseline first page, reference repository, default configuration:** `function` 1.240 / 1.272 /
1.245 s at 99.6 MB peak RSS; `Meteor` 1.553 / 1.573 / 1.616 s at 103.3 MB; `return` 0.762 s,
`user` 0.531 s, `createAccount` 0.270 s (medians) at 99.1 / 82.9 / 33.2 MB. Rescaling round 1's
phase split per term, Decision 1 alone removes about 0.60 s from `function` (to about 0.65 s) and
about 0.31 s from `Meteor` (to about 1.26 s), which is why Decisions 2 and 3 exist.
