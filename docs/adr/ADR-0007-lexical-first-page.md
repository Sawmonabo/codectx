# ADR-0007 — Lexical first page: packed term statistics at activation, rank before hydrate, heap-served first page

**Status:** Accepted, 2026-09-15 · **Informs:** ADR-0001 §2 (the page is the unit of memory), ADR-0003 §2.1
(the lexical tier holds no second copy of the source), ADR-0005 (the packed-at-activation shape this
record reuses) · **Inputs:** the four search profiling rounds recorded in the implementation ledger
(QPERF-4, QPERF-5, QPERF-6) and the measurements reproduced below.

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

## Decision 2 — rank on packed inputs, hydrate only the served page

Candidate documents are ranked from the packed postings and the per-document attributes the filters
need (kind code, path, name and token count, stored once per document in the packed structure),
and the document rows are hydrated only for the page being served. Today every candidate is hydrated
before ranking (`Meteor` hydrates 46 487 rows to serve 200). Filters that today run on hydrated rows
run on the packed attributes; the served page is unchanged because filters and ranking consume the
same values.

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

- QPERF-4, QPERF-5, QPERF-6 lane reports — `.superpowers/sdd/implementation-plan/QPERF-{4,5,6}-report.md` (the phase profiles, the identity proof, the rejected engine-side shortcuts).
- ADR-0003 — Storage tier 2 — `docs/adr/ADR-0003-storage-tier2.md` (contentless lexical tier; why the postings are the only copy of the indexed text).
- ADR-0005 — Graph traversal layout — `docs/adr/ADR-0005-graph-traversal-layout.md` (packed-at-activation parts, ranked pages served by byte offset).
- SQLite FTS5 Extension, the fts5vocab virtual table module — https://www.sqlite.org/fts5.html#the_fts5vocab_virtual_table_module (the instance table the scan reads; why it offers no covering index).
- SQLite, EXPLAIN QUERY PLAN — https://www.sqlite.org/eqp.html (the plan lines reproduced above).
- SQLite, The Query Optimizer Overview — https://www.sqlite.org/optoverview.html (correlated scalar subqueries and temporary b-trees for `DISTINCT`).
- Vitter, External Memory Algorithms and Data Structures: Dealing with Massive Data — https://www.ittc.ku.edu/~jsv/Papers/Vit.IO_survey.pdf (external merge sort; why a single sorted run served by offset is the right shape for later pages).
- Knuth, The Art of Computer Programming vol. 3, §5.2.3 (heapsort and selection with a bounded heap) — https://www-cs-faculty.stanford.edu/~knuth/taocp.html (the bounded heap serves exactly the first page of the same total order).
