# ADR-0005 — Graph traversal layout: packed per-generation adjacency, surrogate walk, bitset visited set

**Status:** Accepted, 2026-09-15 · **Informs:** ADR-0001 §2.2 and §3.3, ADR-0002 · **Inputs:**
research notes [17](../research/17-graph-traversal-throughput.md) and
[18](../research/18-visited-sets-and-external-ranking.md), and the profiling run recorded below.

## Context

The impact walk on the 13 223-file reference repository ran at ≈3 800 visited nodes/s at ten
thousand nodes and fell to ≈300 nodes/s beyond fifty thousand; the hub walk (102 247 nodes,
131 099 edges) completed only after 243.9 s. Two independent measurements on the same 1.36 GiB
store located the cost, and neither is the breadth-first search itself.

**The profile** (`impact <hub> --depth 0 --edges 0 --visited N`, CPU and heap profiles, N from
2 000 to 200 000; every run memory-capped):

| N | wall | marginal nodes/s | adjacency reads | package rollup |
|---|---|---|---|---|
| 10 000 | 2.66 s | 12 698 | 47 % | 18 % |
| 50 000 | 66.35 s | 628 | 11 % | 84 % |
| 200 000 (complete at 102 247) | 243.86 s | 294 | 4.3 % | **93 %** |

The package rollup resolves the containers of every admitted edge's endpoints in batches of 128
edges, re-reading containment and re-hydrating the same container nodes thousands of times over
1 025 batches; the process read 37.3 GB through 15.2 million `pread` calls to visit 50 000 nodes,
about 750 KB per node against a store of 1.46 GB, a read amplification near 3 700×. Once the
touched b-tree pages no longer fit the page cache (≈10 000 nodes on this store) every probe misses.
Adjacency reads themselves stayed linear at ≈104 µs per node, which is still two orders of
magnitude above the ≈1 µs per node a sequential read of the 8–12 MB of relation rows would cost.
The profile also falsified a documented invariant: the in-heap set of nodes "this page admitted"
grew to 102 247 entries on one page, because a page ends only when a level yields rows or the
deadline fires, so it is bounded by the reachable set, not by the page [P].

**The query plan** (`EXPLAIN QUERY PLAN` on the adjacency statement, same store): the keyset over a
frontier chunk orders by the relation's 32-byte canonical hash, which no index on the edge table
serves, so the planner reports `USE TEMP B-TREE FOR ORDER BY`, and each keyset page re-scans the
chunk's whole adjacency, re-runs two correlated visibility sub-queries per candidate edge and
rebuilds the sort: O(E_chunk²/P) per chunk. One 256-node hub chunk holds 194 393 edges (26 % of
the graph) and drains in ≈760 pages of 123 → 53 ms, ≈67 s; the same statement ordered by the
composite index `(from_node_id, kind, to_node_id)` runs in 5–11 ms per page with the temporary
b-tree gone [17 §1]. Direction-optimising breadth-first search was evaluated against this
repository's own degree data (average out-degree 5.91, near-acyclic call graph) and its
top-down/bottom-up switch would not fire [17 §2.1][S7].

**Rulings that bind this decision.** codectx is greenfield: no backward compatibility and no
migration obligation, so schemas, cursor formats and on-disk layouts change in place and existing
indexes are rebuilt; the design chosen is the one that meets the requirements, not the smallest
diff. Every answer stays lossless and globally ordered (ADR-0001 §2.2, §2.3); peak memory is a
function of the page, not of the repository (ADR-0001 §3.1); no default bound may truncate an
answer (ADR-0001 §2.1).

## Decision 1 — a packed per-generation adjacency is the only walk read path

At generation activation the store builds, from the existing relation tables, a packed adjacency
for that generation in both directions, stored as chunked blobs keyed by the generation and dropped
with it. Every traversal endpoint (impact, package rollup, neighbours, references, shortest path,
repository map) reads adjacency from it through one frozen port; the keyset statement over the
relation b-tree is deleted, not kept as a fallback.

**Layout.** The node universe is the integer surrogate `node_ids.id` (ADR-0002); no renumbering.
Per direction: an offsets directory (one 64-bit offset per surrogate, in parts of 65 536 entries)
and an edge stream in parts of about 1 MiB, each node's list sorted by neighbour surrogate and
encoded as varint deltas of the neighbour, a varint relation surrogate and a one-byte relation-kind
code from a per-generation kind dictionary. Per node, the generation also carries three side
arrays in the same chunked form: node-kind code, container surrogate (the deterministic
lowest-canonical container, or zero), and source bytes for file nodes; per relation, an evidence
count. Only relations and nodes visible in the generation are present, so visibility is resolved
once at build time instead of once per candidate edge per page.

**Build.** Two ordered index scans of `relation_ids`, one per direction, semi-joined against the
generation's unit set, streamed: offsets are emitted as the scan passes each surrogate, parts are
flushed as they fill, and heap stays O(part), never O(nodes). The side arrays come from one ordered
scan of the generation's node facts and one aggregate over evidence. On the reference store
(739 529 relations) the whole build is one sequential pass of ≈30 MB of index rows and writes
≈10 MB; the acceptance bound is 5 % of the cold-index wall clock. Because the blob is derived from
facts already sealed, it is neither a fact nor an identity: no provider version, no analysis
fingerprint. The schema fingerprint changes, which rebuilds existing stores; the greenfield ruling
makes that a rebuild, not a migration.

**Alternatives considered.**

1. *Fix the query plan only: index-order keyset, unclamped row page, hoisted visibility semi-join*
   (research note 17's recommendation, "traverse in index order before changing the structure").
   Steel-man: no new artefact, no build step, no second read path, and the 11× on hub chunks is
   proven reachable inside the current design. It loses on the constraint that decided this record:
   even in index order every frontier node is one random b-tree descent, so past the page cache the
   walk degenerates to a random read per node; a packed array scanned sequentially measures
   ≈14× faster per edge than an embedded b-tree graph backend, with the b-tree taking 7.1× more
   last-level-cache misses [S36], and the whole packed structure is ≈0.8 % of the store [17 §3.1].
   The research note deferred the blob behind a re-measurement on compatibility and diff-size
   grounds; the greenfield ruling removes those grounds, and two phases touching the same walk code
   cost more than one correct design.
2. *A packed adjacency merged at read time with the relation b-tree as a delta for units re-indexed
   since* [S17][S18][S20]. Steel-man: no rebuild per refresh. Rejected: a generation is immutable
   once active (ADR-0001 §2.2 pins cursors to it), so there is nothing to merge; the delta belongs
   to the next generation, whose build is a sequential pass. The cautionary result for chained
   deltas (12 GB → 187 GB over 201 snapshots) [S18] is the reason not to introduce them.
3. *Compressed encodings* (quasi-succinct or k²-tree) [S13][S14][S16]. Rejected at this size: 1.7–5.3
   bits per link is not worth a 2–15 µs per-neighbour decode on a 10 MB artefact whose win is
   locality, not space [17 §3.1]. Varint deltas keep decoding at a few nanoseconds per edge.
4. *Cluster-ordered adjacency for the sublinear external BFS bound* [S3]. Rejected: its win is a
   second physical ordering that must be maintained under re-indexing, for a graph whose whole
   packed form fits the page cache many times over [17 §2].

**Consequences.** Store size grows by ≈0.6–0.8 % on the reference store, inside the 3.5× ratio
gate (ADR-0001 §1.4). Activation gains a sequential build measured on real repositories. The port
signature changes and every walk caller moves together. The repository map's counts come from the
containment lists and the side arrays instead of hydrating every child, which is what made its
second page refuse at the deadline.

## Decision 2 — the walk carries surrogates and keeps its visited set as a paged bitset

The level-synchronous breadth-first walk stays (semi-external regime: vertex state resident, edges
streamed [S5][S33]); what changes is what it carries. Frontier records, spooled levels and internal
maps hold 64-bit surrogates, not 64-hex canonical ids; canonical ids are resolved in one batched
primary-key read per committed level and at delivery. The visited set is a bitset over the
generation's surrogate range, persisted in the retained walk directory as a sparse file and read
and written through a bounded cache of 4 KiB pages; level candidates are sorted before membership
tests so page touches cluster. The Bloom filter, the sorted runs and the merge-join fallback are
deleted with the in-heap "added" set.

**Atomicity of a page.** A level's bits are applied only after that level's frontier records are
spooled, and the continuation is minted after both; adoption re-applies the last spooled level's
refs (idempotent), so a page cut between the two writes loses no node and admits none twice.

**Alternatives considered.**

1. *Keep the append-only runs plus a frozen Bloom filter* (ADR-0001 §3.3). Rejected: the filter's
   geometry is frozen at creation and saturates past ≈m/16 admitted nodes, after which every level
   falls back to a full merge-join — the same quadratic in another place [17 §4]; a bitset has no
   saturation and no false positives, and at 10⁸ nodes it is 11.9 MiB on disk with only touched
   pages resident [18 A1].
2. *A container-partitioned compressed bitmap* [S21][S22]. Steel-man: its dense container is an
   uncompressed bitset, so it never loses meaningfully, and it wins on sparse sets [18 A2]. Not
   adopted now because a walk's visited set is dense within the surrogate ranges it touches and the
   plain paged bitset needs no container index; the on-disk format is a sparse file, so untouched
   regions cost nothing either way. Revisit only if a measured walk shows sparse touched ranges.
3. *Cuckoo or blocked Bloom filters* [S23][S27][18 A3]. Rejected: right only where ids are not
   dense; surrogates are dense, and the bitset is 1 bit per node with zero false positives.
4. *Rank/select over the bitset for cursor resumption* [18 A4]. Rejected: the cursor stores a bit
   position, and enumeration in id order is a `ctz` word scan at zero overhead.
5. *Direction-optimising or bottom-up frontier* [S7][S8]. Rejected on the measured degree data
   (Context). *Δ-stepping for the shortest path* [S9]: deferred; the path endpoint keeps its
   external Dijkstra scratch and only its adjacency reads move to the packed layout.

**Consequences.** Peak heap for a walk is the frontier level bound plus the bitset page cache plus
the sort buffers already in place, independent of the reachable set — the invariant the profile
falsified is restored. The cursor payload version increases; its surrogate positions are fenced by
the generation id the cursor already carries, so a cursor never re-enters another id space. Ranking
records keep the content-derived canonical node id as their total tie-break key (research note 18
B10; the search identity defect of the same day was exactly a root-dependent tie-break), so a fresh
index and a delta-built index of the same tree serve byte-identical pages.

## Decision 3 — ranking stays an external merge sort with a total, content-derived key

The global ranking of impact entries and package pairs keeps the two-pass external merge sort
(ADR-0001 §2.3). Top-k heaps and threshold algorithms are rejected because a lossless cursor must
page past any k, which is the state they discard [18 B8][S25]; MSD radix partitioning is recorded
as the one credible accelerator if the sort is ever the measured bottleneck [S31][S32] — today
serving costs ≈0.4 s of a 244 s walk. The tie-break stays intrinsic (canonical node id), never
positional, so parallel or resumed sorts yield the same sequence [18 B10].

## What the numbers must show

On the reference repository's hub walk, unbounded: complete in one default-deadline page, wall
≤ 3 s end to end (walk, rollup, rank, first page), resident set ≤ 150 MB, `truncated:false` on the
final page after following cursors; visited-count and entries identical to the pre-change answer.
Arithmetic: 131 099 edges × ≈7 B decode ≈ 1 MB read and ≈3 ms; membership 131 099 tests at
≈50 ns ≈ 7 ms; frontier spool ≈4 MB; rollup lookups 262 198 array reads; two external sorts over
≈130 000 records ≈ 0.4 s; hydration of one page ≈ 20 ms. The walk-only phase is expected above
10⁵ nodes/s. Activation on the same repository must add ≤ 5 % to the cold index wall.

## Sources

Research note 17 (this repository) carries the full bibliography [S1]–[S37]; research note 18
carries [R1]–[R36]. The entries below are the ones this record's text relies on, each once.

- [P] GPERF-P profile of the impact walk on the reference store, 2026-09-15 (per-phase split,
  rate versus N, `/proc/<pid>/io` bytes, level structure) — recorded in research note 17 §1;
  harness `scripts/perf/gperf-p-impact.sh`.
- [17 §1–§4] Graph traversal throughput: external-memory BFS, adjacency layout, visited sets,
  ranking — `docs/research/17-graph-traversal-throughput.md` (query-plan defect, blob sizing,
  Bloom saturation, direction-optimising rejection).
- [18 A1–A4, B8, B10] Visited-set structures and global ranking —
  `docs/research/18-visited-sets-and-external-ranking.md` (bitset arithmetic, compressed-bitmap
  premise correction, filter decision rule, rank/select, top-k rejection, determinism).
- [S3] External-Memory Breadth-First Search with Sublinear I/O — https://people.mpi-inf.mpg.de/~mehlhorn/ftp/ExternalBFS.pdf (why cluster-ordered adjacency is rejected).
- [S5] A Computational Study of External-Memory BFS Algorithms — https://resources.mpi-inf.mpg.de/departments/d1/teaching/ws10/models_of_computation/Ajwani.pdf (semi-external regime).
- [S7] Direction-Optimizing Breadth-First Search — https://www.semanticscholar.org/paper/6bc45efd5b27f661b1cbbab8998b87a34da5967e (switch heuristic, visited bitmap as baseline).
- [S8] Ligra: A Lightweight Graph Processing Framework for Shared Memory — https://doi.org/10.1145/2442516.2442530 (sparse/dense frontier switch, rejected).
- [S9] Δ-stepping: A Parallelizable Shortest Path Algorithm — https://doi.org/10.1016/S0196-6774(03)00076-2 (deferred for the path endpoint).
- [S13] The WebGraph Framework I: Compression Techniques — https://doi.org/10.1145/988672.988752 (compressed encodings, rejected at this size).
- [S14] Quasi-Succinct Indices — https://doi.org/10.1145/2433396.2433409 (same).
- [S16] k2-Trees for Compact Web Graph Representation — https://doi.org/10.1007/978-3-642-03784-9_3 (per-neighbour decode cost).
- [S17] GraphOne: A Data Store for Real-time Analytics on Evolving Graphs — https://www.usenix.org/conference/fast19/presentation/kumar (read-time delta merge, rejected).
- [S18] LLAMA: Efficient Graph Analytics Using Large Multiversioned Arrays — https://doi.org/10.1109/ICDE.2015.7113298 (chained-delta growth, why deltas are not introduced).
- [S20] Sortledton: A Universal, Transactional Graph Data Structure — https://doi.org/10.14778/3538598.3538613 (dynamic packed ceiling).
- [S21] Better Bitmap Performance with Roaring Bitmaps — https://arxiv.org/abs/1402.6407 (compressed bitmap alternative).
- [S22] Roaring Bitmaps: Implementation of an Optimized Software Library — https://arxiv.org/abs/1709.07821 (same).
- [S23] Cuckoo Filter: Practically Better Than Bloom — https://doi.org/10.1145/2674005.2674994 (filters rejected for dense ids).
- [S25] Optimal Aggregation Algorithms for Middleware — https://www.wisdom.weizmann.ac.il/~naor/PAPERS/middle_agg.pdf (threshold algorithms rejected).
- [S27] Cache-, Hash- and Space-Efficient Bloom Filters — https://doi.org/10.1145/1498698.1594230 (blocked Bloom rejected).
- [S31] Multi-Core, Main-Memory Joins: Sort vs. Hash Revisited — http://www.vldb.org/pvldb/vol7/p85-balkesen.pdf (MSD partitioning as a future accelerator).
- [S32] A Comprehensive Study of Main-Memory Partitioning — https://www.cs.columbia.edu/~orestis/sigmod14I.pdf (same).
- [S33] FlashGraph: semi-external graph engine with vertex state in memory — https://www.usenix.org/conference/fast15/technical-sessions/presentation/zheng (semi-external regime).
- [S36] LiveGraph: purely sequential adjacency-list scans, measured against b-tree, LSM and CSR backends — https://pacman.cs.tsinghua.edu.cn/~cwg/publication/livegraph-2020/livegraph-2020.pdf (packed array versus b-tree per-edge cost, the decisive measurement for Decision 1).
