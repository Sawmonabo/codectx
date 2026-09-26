# 17 — Graph traversal throughput: external-memory BFS, adjacency layout, visited sets, ranking

- **Lane:** GPERF-R (research, read-only) · **Date:** 2026-09-15 · **Scope:** impact / callers /
  callees / package-deps / shortest-path; search out of scope.
- **Evaluates:** ADR-0001 §2 §3.3; `internal/graph/{traverse,visited,visitedstore,walkrun,impactrank,path,pathscratch}.go`;
  `internal/storage/sqlite/adjacency.go`; ADR-0002 (integer surrogates), ADR-0003.

---

## 0. Headline

The measured ≈3 800 visited nodes/s is **not** an external-memory limit. It is a query-plan defect:
the frozen keyset order on `Adjacency.Edges` is the relation's 32-byte **canonical hash**, which no
index on the edge table can serve, so every keyset page sorts the whole chunk adjacency in a temp
B-tree and discards the prefix — **O(E_chunk²/P)** per chunk, not O(E_chunk). Removing it needs **no
schema change and no new structure**, only a keyset key that agrees with an index that exists. Proven below.

---

## 1. Empirical baseline (this lane's own measurements)

Two real stores, `immutable=1`, warm page cache, WSL2 host. R-mid is a different, smaller corpus,
cited only where the two agree in shape; every number marked R-large is from the 1.36 GiB store the
brief's 3 800 nodes/s refers to.

| Store | bytes | nodes | edges | max out-deg | avg out-deg |
|---|---|---|---|---|---|
| **R-large** (`codectx-gperf/store`, the 1.36 GiB reference) | 1 464 328 192 | 477 602 | 739 529 | **8 017** | 5.91 |
| R-mid (`codectx-cert/.../fe42192f…`) | 700 219 392 | 201 850 | 344 703 | 320 | 4.08 |

### 1.1 The plan defect, as the engine reports it

That read is served today by the store's packed adjacency lists, streamed by
`graphReader.Neighbours` (ADR-0002, ADR-0007). What follows measured the per-batch edge statement —
canonical → surrogate translation inside, one row per edge out.

`EXPLAIN QUERY PLAN` on that statement's exact shape, R-large and R-mid alike:

```
|--SEARCH sn USING COVERING INDEX sqlite_autoindex_node_ids_1 (canonical=?)
|--SEARCH ri USING INDEX sqlite_autoindex_relation_ids_2 (from_node_id=?)
|--CORRELATED SCALAR SUBQUERY 2        <- visibility EXISTS, per candidate edge
|  |--SEARCH rf USING COVERING INDEX idx_relation_facts_id (relation_id=?)
|  `--CORRELATED SCALAR SUBQUERY 1     <- nested generation EXISTS, per candidate edge
|     `--SEARCH gu USING COVERING INDEX sqlite_autoindex_generation_units_2 (...)
`--USE TEMP B-TREE FOR ORDER BY        <- the defect
```

The keyset is `AND ri.canonical > ?` with `ORDER BY ri.canonical` (`adjacency.go` ~line 185).
`relation_ids.canonical` is a content hash, uncorrelated with `UNIQUE(from_node_id, kind, to_node_id)`,
so the `after` predicate **cannot seek** — it filters after the range scan. Every page re-scans the
chunk's whole adjacency, re-runs both correlated `EXISTS` per candidate edge and rebuilds a temp
B-tree; only the sort's input shrinks.

### 1.2 What that costs, measured (R-large)

A frontier chunk of `adjacencyBatch = 256` nodes taken from the top of the degree distribution
holds **194 393 edges** — 26 % of the whole graph in one chunk. `rowLimit = min(batch, MaxPageItems)`
= 256, so draining it takes **≈760 keyset pages**, each rescanning that chunk.

| Keyset position | ORDER BY canonical (current) | ORDER BY (from_node_id, kind, to_node_id) |
|---|---|---|
| page 1 of ~760 | **123 ms** | **5 ms** |
| ~50 % through | 91 ms | — |
| ~95 % through | 53 ms | — |
| mid-walk seek | — | **11 ms** |

Per-page cost decays as the filter removes rows, mean ≈88 ms: **≈67 s of query time to drain one
256-node chunk**, warm. The index-ordered form is O(P) per page (~8 ms) — **≈11× in wall clock here
and asymptotically E_chunk/P better**. On an average chunk (5.91 degree → ~1 513 edges → ~6 pages)
the amplification is ~6×, not 760×; the hub case is what makes a complete walk take minutes.

### 1.3 The fix is reachable — proven

Two EQP runs on R-large isolate the cause (full query otherwise identical):

- surrogate `from_node_id IN (…)` **+ `ORDER BY ri.canonical`** → temp B-tree **still present**, so
  the IN-list form is not the cause;
- surrogate `from_node_id IN (…)` **+ `ORDER BY ri.from_node_id, ri.kind, ri.to_node_id`** →
  **`USE TEMP B-TREE FOR ORDER BY` is gone**; the engine serves the order from
  `sqlite_autoindex_relation_ids_2` [S24].

The chunk must be sent as **ascending surrogate ids** for the planner to fuse the IN-list with the
ORDER BY prefix [S24]. Surrogates exist (ADR-0002); the per-batch edge statement translated
canonical → surrogate inside and never let a surrogate out. The read now travels over packed
adjacency lists whose entries are surrogate deltas, so the ordering the plan had to be coaxed into
is the storage order itself (ADR-0007).

---

## 2. External-memory and semi-external BFS: what applies here

Model: Aggarwal–Vitter, `scan(N)=Θ(N/(D·B))`, `sort(N)=Θ((N/(D·B)) log_{M/B}(N/B))` [S1]; merge fan-in
is Θ(M/B), so one merge pass covers any repository index. Two caveats this store forces:
**(i)** a B-tree point lookup is `O(log_B N)`, not `O(1)`, so every "+V unstructured accesses" term
carries a log factor (mitigated, not removed, by mmap-resident interior pages); **(ii)** the effective
`B` is the engine's 4 KiB page, so level dedup and ranking must run on **our own spool files with large
blocks**, never through the B-tree. *Premise correction: there is no Meyer–Zeh external-memory BFS —
the sublinear-I/O BFS line is Munagala–Ranade → Mehlhorn–Meyer; Meyer–Zeh is SSSP [S4].*

| Technique | Bound | Applies to a B-tree/mmap store? |
|---|---|---|
| Munagala–Ranade EM-BFS [S2] | `O(V + sort(V+E))`; per level sort `A(t)`, then `L(t) := A'(t) \ {L(t−1) ∪ L(t−2)}` by parallel scan of the two previous level files — **no random visited probes at all** | **Yes, essentially unchanged.** Needs only keyed set-at-a-time range scans, which a composite-PK B-tree does well; dedup is external sort over frontier-sized files we own. Its `O(V)` term becomes `O(V·log_B N)` descents. Its 2-level invariant needs an **undirected** graph — ours is directed, so a real visited set is still required (§4). |
| Mehlhorn–Meyer MM-BFS [S3] | `O(sqrt(V(V+E)/(D·B)) + sort(V+E))` — sublinear; Euler-tour/spanning-tree clustering + a scanned hot pool so each adjacency is fetched once | **Only if we pay for a second physical ordering.** The whole win comes from *physically rewriting* adjacency into cluster-ordered files. A `(src,…)` B-tree gives contiguity by src, not by cluster; MM-BFS would need a maintained `(cluster,src,dst)` table plus the spanning-tree/Euler-tour/list-ranking build, kept valid under incremental re-indexing. Without it we sit at MR's bound. |
| Meyer–Zeh undirected SSSP [S4] | `O(sqrt(VE/B)·log(W/w) + sort(V+E)·log log(VB/E))`; ESA'06 removes the `W/w` term at `O(sqrt(nm/B) log n + MST)` | Same clustering caveat. |
| **Semi-external** [S5][S37][S33] | formalised as `α·V ≤ M < E` — vertex state resident, edges on disk; `O(V + E/(D·B))` I/Os, or `L·scan(E)` with **zero random reads** for the streamed-bitmap variant | **Yes — the regime codectx is actually in**, and `V` bits is negligible (§4). A production semi-external engine reaches **up to 80 % of its in-memory performance** with vertex state in RAM and edge lists on SSD [S33]. Semi-external BFS for **directed** graphs is still open research [S34] — worth watching, not adopting. |
| Direction-optimising BFS [S7] | switch top-down→bottom-up at `m_f > m_u/α`, back at `n_f < V/β`, tuned **α=14, β=24**; **1.4–3.8×** on real social graphs (3.3–7.8× synthetic) | **Rejected — §2.1.** Also needs a reverse in-edge index. |
| Sparse/dense frontier switch [S8] | array-of-ids vs bool-array over `V`, switching at `|U| + Σdeg(U) > |E|/20`; sparse pushes out-edges, dense pulls in-edges | **Partly, and cheaply.** The sparse side is free; the dense side is the §4 bitset. The dense/pull path needs the reverse index. |
| Δ-stepping [S9] | light/heavy bucket relaxation; `O(V+E+d·L)` average-case sequential time for random weights; `Δ=1`→Dial, `Δ=∞`→Bellman–Ford | **Yes, for `path.go`.** Buckets are disk-backed spool queues, relaxations are keyed lookups, and it is explicitly tolerant of re-relaxation — a better fit than external Dijkstra with a tournament tree. It loses when no good Δ exists (long weighted chains). |
| Sharded out-of-core engines [S10][S11][S12] | trade random access for sequential shard scans; sequential/random bandwidth ratio measured at **500× on disk, 30× on SSD** [S11] | **Only if a flat blob exists.** They are the §3 design argument, not a drop-in; bespoke tiled formats [S35] are a whole storage layer (30 h preprocessing at trillion scale) and are irrelevant unless the engine is replaced. |

### 2.1 Direction-optimising BFS does not apply — the degree data says why

Bottom-up wins when a huge frontier meets high-degree hubs, so most unvisited vertices find a parent
after a few in-edge probes and exit early [S7] — it needs a low-diameter, scale-free graph. The
paper's own worst results are its **highest-effective-diameter** inputs, for exactly this reason.
R-large has **avg out-degree 5.91** and a near-DAG call graph with long chains, so the frontier is
rarely a large fraction of `V`: `m_f > m_u/14` would rarely fire, and when it did the bottom-up sweep
over all unvisited vertices would be a net loss. **Do not implement it.**

---

## 3. Adjacency layout

### 3.1 Cost of a per-generation packed adjacency blob (CSR; R-large N=477 602, E=739 529)

| Component | Formula | Bytes |
|---|---|---|
| offsets, uint32 | (N+1)×4 | 1 910 412 (1.82 MiB) |
| targets, uint32 | E×4 | 2 958 116 (2.82 MiB) |
| kind, uint8 | E×1 | 739 529 (0.71 MiB) |
| **forward CSR** | | **≈5.35 MiB** |
| **+ reverse CSR** (callers) | same shape | **≈10.7 MiB total** |

**≈0.8 % of the 1.36 GiB store**; ≈107 MiB at 10× this graph — still mmap-able. Building it is one
ordered scan of `relation_ids` in the `(from_node_id, kind, to_node_id)` index order that already
exists — histogram degrees, prefix-sum, one sequential write — so no sort is needed and peak extra
memory is the offsets array alone. At the ~2.5–3 M edges/s an out-of-core engine sustains end-to-end
on a 2-core/8 GiB box [S10], 739 529 edges build in **well under a second**. Gap-coded/quasi-succinct
[S14][S15] (Elias–Fano: `2 + ⌈log₂(u/n)⌉` bits/element) or k²-tree [S16] forms reach 1.7–5.3 bits per
link on web/social graphs [S13][S16] but are **not worth it at 10 MiB**, and the k²-tree's measured
**2–15 µs per neighbour delivered** [S16] is far too slow for a walk — the win here is locality, not space.

### 3.2 Incremental maintenance

The literature's answer for a packed array that must absorb updates is a **base snapshot plus a
delta merged at read time**: multi-versioned snapshot arrays [S18], a log-structured edge store
compacted into the array [S17], transactional dynamic structures [S19][S20]. Mapped here: one
immutable blob per generation plus the existing `relation_ids` B-tree as the delta for units
re-indexed since; the reader merges blob order with delta order. **Fingerprint impact: none** — the
blob is derived state keyed by generation id, and a missing or stale blob falls back to the B-tree.
The measured ceiling for "dynamic but packed-fast" is **≈2× CSR memory and ≈1.2× CSR analytics time**
[S20]. The cautionary case is per-snapshot delta tables with fragment chaining, which grew **12 GB →
187 GB over 201 snapshots** [S18]: **compaction cadence is the design variable that matters most**,
and a per-vertex read must not degenerate into a pointer-chase one link long per generation.

### 3.3 Steel-man: keep the fully-external design, just batch better

**For it.** No new artefact, build step, staleness window or second read path to keep consistent
with the delta model, and no extra bytes in a store already over its ratio budget (ADR-0001 §3.3).
§1.3 shows the 11× is available *inside* the current design; the `EXISTS` pair is two covering-index
descents [S24]. Batch size is a tunable, not a rewrite: `rowLimit` is clamped to
`min(adjacencyBatch=256, MaxPageItems)`, which is what manufactures 760 pages from one chunk.

**When it loses.** When per-edge random descent dominates *even in index order* — the working set
exceeds the page cache and each descent faults. Index order gives locality *within one node's
adjacency*, but the frontier is scattered, so at scale the walk degenerates to one random descent per
frontier node. The blob makes a level's neighbour reads sequential in an artefact ~130× smaller than
the store, and the measured gap is large: benchmarked head-to-head against an embedded B+-tree graph
backend, a packed array scans **≈14× faster per edge** (and ≈47× vs an LSM store), with the B+-tree
triggering **7.1× more last-level-cache misses** [S36]. That ratio is the size of the prize, and also
the reason not to claim it before phase 1 is measured. **Batching first, blob only if phase 1 misses.**

---

## 4. Visited set

Plain bitset over dense surrogate ids, 1 bit/node, `ceil(N/8)`:

| N | bytes | |
|---|---|---|
| 477 602 (R-large) | 59 701 | **58 KiB** |
| 10⁶ | 125 000 | 122 KiB |
| 10⁷ | 1 250 000 | 1.19 MiB |
| 10⁸ | 12 500 000 | **11.9 MiB** |
| 10⁹ | 125 000 000 | 119 MiB |

A bitset is formally `f(repo)`, which ADR-0001 forbids for *heap* — but 11.9 MiB for a
hundred-million-node monorepo is smaller than one page of hydrated nodes, and **mmap/sparse-file
backing** makes resident pages `f(touched region)`: the ruling's intent (peak not proportional to
repository size) holds because untouched pages never fault in. That is the semi-external model [S5][S6].

Versus sorted-runs + Bloom: the filter's geometry is frozen at creation and **saturates past ~m/16
admitted nodes** (ADR-0001 §3.3, "Residual, by design"), after which every level falls back to a
full merge-join over all runs — per-level cost becomes `O(total_visited)`, the §1.2 quadratic in a
different place. A bitset has no saturation and no false positives. A container-based compressed bitmap
[S21][S22] is the safer default: its dense container **is** an uncompressed bitset (1 bit/value over
a 2¹⁶ chunk, ~8 kB) plus a few bytes of chunk index, so it never loses meaningfully to a plain
bitset, while sparse chunks cost 16 bits/value regardless of universe size — it only loses to
run-length formats on long-run sorted data (measured 7.4×: 3.2 vs 0.43 bits/value) [S22]. Blocked
Bloom / cuckoo filters [S23][S27] stay right only where ids are **not** dense, as do frontier-sort
dedup [S2] and delayed duplicate detection [S28]; note a visited set is monotone, so a filter's
lack of deletion costs nothing.

**Resumable pagination.** Persist the bitset as the mmap file; carry its **generation + a bit
position** in the cursor. Enumerating set bits in id order is a `ctz`/`blsr` word scan at **0 %**
index overhead — succinct rank/select (3.2–3.6 % overhead [S29]) is needed only to resume at the
*i-th* set element, which a bit-position cursor makes unnecessary. A page then appends and copies
nothing: O(page), not O(visited) — the property ADR-0001 §3.3 already won with append-only runs. Do
**not** snapshot per page (the defect already fixed). The literature on cursor-resumable traversal
state is thin: superstep checkpointing is crash recovery, not paged resumption, and is **not**
precedent here; the on-point shape is a persisted bitmap snapshot plus an append-only op log
compacted periodically [S30].

---

## 5. Global ranking

`impactrank.go`'s external merge sort is **already the right primitive** and should not change.

- External merge sort costs `sort(N)` I/Os [S1]; peak memory is the run buffer plus fan-in blocks,
  independent of result size, and it yields a **total order**, so page *k* is defined for any *k*.
- **Tournament/heap top-k** is `O(n log k)` in `O(k)` memory and cheaper — but correct only if the
  consumer never paginates past *k*. codectx promises a lossless cursor over the whole ranked set,
  so top-k reintroduces truncation under another name. **Reject.** Threshold algorithms [S25] assume
  sorted per-attribute access the store lacks and also answer top-k. **Reject, same reason.**
- **MSD radix-partitioned ranking** is the one credible accelerator: partitioning on the rank key's
  *leading* bits yields range-disjoint partitions whose **concatenation is already the global order**,
  so each is independently sortable, persistable and servable, and a cursor is just
  `(partition, offset)`. Measured, partitioning sustains ~4 G tuples/s against multi-way merge's
  0.5–1.5 G, and partition-then-sort reaches 680 M tuples/s with flat scaling where sort-then-merge
  degrades past 256 M on merge fan-in [S31]. LSD offers none of this — nothing is servable until the
  last pass. Caveats: fan-out is TLB-bounded (measured throughput collapse past 32 with 2 MiB pages)
  unless software-managed write buffers are used, and partitions skew on non-uniform keys. Phase 3.
- **Byte-identical order across runs and resumptions** needs a *total* tie-break key. Make it
  intrinsic — the canonical node id — and stability becomes moot, because no two records compare
  equal and any correct sort (parallel, SIMD, non-stable) yields the same sequence. A **positional**
  tie-break (input index, run number, insertion order) is the trap: across a resumption the input is
  re-enumerated from a possibly different spool layout, so equal-scoring entries silently reorder and
  an entity appears on two pages or none. Both properties hold today; any change must preserve them.

---

## 6. Recommendation

**Per endpoint.** impact / callers / callees / package-deps: level-synchronous BFS stays, in the
**semi-external** regime — visited state resident, edges streamed. Shortest path: keep external
Dijkstra in the per-request scratch; revisit bucketed Δ-relaxation [S9] only if profiling shows the
priority queue, not adjacency, is the cost. **No direction-optimising BFS** (§2.1).

**Phase 1 — no schema change, no new artefact. Expected ≈8–11× on hub walks.**
1. Change the keyset key on `Adjacency.Edges` from the relation's canonical hash to the composite
   `(from_node_id, kind, to_node_id)`, and send frontier chunks as **ascending surrogate ids** so the
   planner fuses the IN-list with the ORDER BY prefix (§1.3, proven). Schema-free but **not
   contract-free**: a frozen port signature and the cursor payload change (version bump), and the
   surrogate-space position is rebuild-local, so it must be fenced by the cursor's generation id.
2. Stop clamping `rowLimit` to `adjacencyBatch` — 256 rows per page over a 194 393-edge chunk is what
   manufactures 760 round trips; the row page should be the wire page.
3. Replace the per-edge nested correlated `EXISTS` with one semi-join against the generation's
   visible-unit set (8 961 rows on R-large — it fits in a transient in-memory table).

**Sketch of the rate.** Phase-1 per-page work is 256 index entries + 256 visibility probes = 5–11 ms
measured, i.e. ~25–50 k delivered edges/s per connection against ~2.9 k today on a hub chunk. At avg
degree 5.91 that is **≈4 000–8 500 visited nodes/s** in hub-heavy regions where the current code
manages a few hundred; average chunks improve ~6×.

**Phase 2 — derived per-generation packed adjacency blob** (§3.1, ≈10.7 MiB, ≈0.8 % of the store),
built at seal time from the existing index order, merged at read time with a B-tree delta (§3.2).
Purely a cache: no fact identity, no fingerprint change, degrades to the B-tree path when absent.

**Phase 3 — mmap-backed bitset visited set** over surrogate ids (§4), closing the Bloom saturation
residual; optionally radix-partitioned ranking (§5).

Phases 2 and 3 are justified only **after phase 1 is measured**: if phase 1 alone reaches target on
R-large, the §3.3 steel-man wins and the blob is not built.

---

## Bibliography

[S1] The Input/Output Complexity of Sorting and Related Problems — https://doi.org/10.1145/48529.48535
[S2] I/O-Complexity of Graph Algorithms (SODA 1999) — https://dl.acm.org/doi/10.5555/314500.314891
[S3] External-Memory Breadth-First Search with Sublinear I/O (ESA 2002) — https://people.mpi-inf.mpg.de/~mehlhorn/ftp/ExternalBFS.pdf
[S4] I/O-Efficient Undirected Shortest Paths (ESA 2003) — https://doi.org/10.1007/978-3-540-39658-1_40
[S5] A Computational Study of External-Memory BFS Algorithms (SODA 2006) — https://resources.mpi-inf.mpg.de/departments/d1/teaching/ws10/models_of_computation/Ajwani.pdf
[S6] Graph500 benchmark specification (semi-external / bitmap BFS practice) — https://graph500.org/
[S7] Direction-Optimizing Breadth-First Search (SC 2012) — https://www.semanticscholar.org/paper/6bc45efd5b27f661b1cbbab8998b87a34da5967e
[S8] Ligra: A Lightweight Graph Processing Framework for Shared Memory (PPoPP 2013) — https://doi.org/10.1145/2442516.2442530
[S9] Δ-stepping: A Parallelizable Shortest Path Algorithm (J. Algorithms 2003) — https://doi.org/10.1016/S0196-6774(03)00076-2
[S10] GraphChi: Large-Scale Graph Computation on Just a PC (OSDI 2012) — https://www.usenix.org/conference/osdi12/technical-sessions/presentation/kyrola
[S11] X-Stream: Edge-centric Graph Processing using Streaming Partitions (SOSP 2013) — https://doi.org/10.1145/2517349.2522740
[S12] GridGraph: Large-Scale Graph Processing on a Single Machine (USENIX ATC 2015) — https://www.usenix.org/conference/atc15/technical-session/presentation/zhu
[S13] The WebGraph Framework I: Compression Techniques (WWW 2004) — https://doi.org/10.1145/988672.988752
[S14] Quasi-Succinct Indices (WSDM 2013) — https://doi.org/10.1145/2433396.2433409
[S15] Layered Label Propagation: A MultiResolution Coordinate-Free Ordering (WWW 2011) — https://doi.org/10.1145/1963405.1963488
[S16] k2-Trees for Compact Web Graph Representation (SPIRE 2009) — https://doi.org/10.1007/978-3-642-03784-9_3
[S17] GraphOne: A Data Store for Real-time Analytics on Evolving Graphs (FAST 2019) — https://www.usenix.org/conference/fast19/presentation/kumar
[S18] LLAMA: Efficient Graph Analytics Using Large Multiversioned Arrays (ICDE 2015) — https://doi.org/10.1109/ICDE.2015.7113298
[S19] Teseo and the Analysis of Structural Dynamic Graphs (VLDB 2021) — https://doi.org/10.14778/3446095.3446090
[S20] Sortledton: A Universal, Transactional Graph Data Structure (VLDB 2022) — https://doi.org/10.14778/3538598.3538613
[S21] Better Bitmap Performance with Roaring Bitmaps (SPE 2016) — https://arxiv.org/abs/1402.6407
[S22] Roaring Bitmaps: Implementation of an Optimized Software Library (SPE 2018) — https://arxiv.org/abs/1709.07821
[S23] Cuckoo Filter: Practically Better Than Bloom (CoNEXT 2014) — https://doi.org/10.1145/2674005.2674994
[S24] SQLite Query Optimizer Overview — the ORDER BY optimisation and IN-list/index fusion — https://www.sqlite.org/optoverview.html
[S25] Optimal Aggregation Algorithms for Middleware (JCSS 2003) — https://www.wisdom.weizmann.ac.il/~naor/PAPERS/middle_agg.pdf
[S26] Space/Time Trade-offs in Hash Coding with Allowable Errors (CACM 1970) — https://doi.org/10.1145/362686.362692
[S27] Cache-, Hash- and Space-Efficient Bloom Filters (ACM JEA 2009) — https://doi.org/10.1145/1498698.1594230
[S28] Best-First Frontier Search with Delayed Duplicate Detection (AAAI 2004) — https://cdn.aaai.org/AAAI/2004/AAAI04-103.pdf
[S29] Space-Efficient, High-Performance Rank and Select Structures (SEA 2013) — https://link.springer.com/chapter/10.1007/978-3-642-38527-8_15
[S30] Portable Roaring serialization format (snapshot + append-only op log practice) — https://github.com/RoaringBitmap/RoaringFormatSpec/
[S31] Multi-Core, Main-Memory Joins: Sort vs. Hash Revisited (PVLDB 7) — http://www.vldb.org/pvldb/vol7/p85-balkesen.pdf
[S32] A Comprehensive Study of Main-Memory Partitioning (SIGMOD 2014) — https://www.cs.columbia.edu/~orestis/sigmod14I.pdf
[S33] FlashGraph: semi-external graph engine, vertex state in memory (FAST 2015) — https://www.usenix.org/conference/fast15/technical-sessions/presentation/zheng
[S34] Efficient Semi-External Breadth-First Search for directed graphs (2025) — https://arxiv.org/abs/2507.12925
[S35] Mosaic: out-of-core graph processing on a single machine (EuroSys 2017) — https://taesoo.kim/pubs/2017/maass:mosaic.pdf
[S36] LiveGraph: purely sequential adjacency-list scans — measured vs B+-tree/LSM/CSR (VLDB 2020) — https://pacman.cs.tsinghua.edu.cn/~cwg/publication/livegraph-2020/livegraph-2020.pdf
[S37] A Functional Approach to External Graph Algorithms — origin of the semi-external model (Algorithmica 2002) — https://link.springer.com/article/10.1007/s00453-001-0088-5

---

## Draft ADR-0005 — Traverse in index order before changing the structure

- **Status:** Proposed · **Date:** 2026-09-15 · **Informs:** ADR-0001 §2, ADR-0002

### Decision

Traversal reads adjacency in the order the edge index already stores it. The keyset key on the
`Adjacency.Edges` port changes from the relation's 32-byte canonical hash to the composite
`(from_node_id, kind, to_node_id)`, frontier chunks are sent as ascending surrogate ids, the row
page stops being clamped to the frontier-chunk size, and the per-edge nested visibility `EXISTS`
becomes one semi-join against the generation's visible-unit set. No schema change; a cursor payload
version bump, fenced by the generation id the cursor already carries. A derived per-generation
adjacency blob and a bitmap visited set are **deferred** behind a re-measurement.

### Alternatives

1. **Keep the current fully-external design and only tune batch sizes.** Smallest diff; keeps one
   code path; the port stays frozen.
2. **Build a packed per-generation adjacency blob now** (≈10.7 MiB for a 477 602-node / 739 529-edge
   store, ≈0.8 % of it) and traverse that.
3. **Adopt a bidirectional/bottom-up frontier strategy.**

### Why

The measured 3 800 nodes/s is a query-plan artefact, not an I/O bound. The canonical-hash keyset is
unservable by any index, so `EXPLAIN QUERY PLAN` reports `USE TEMP B-TREE FOR ORDER BY` and every
page re-scans the chunk's whole adjacency: O(E_chunk²/P). On the 1.36 GiB reference store one
256-node hub chunk holds 194 393 edges and needs ~760 pages at 123→53 ms each (≈67 s); the same
query ordered by the composite index takes 5–11 ms per page with the temp B-tree gone — verified by
two otherwise-identical query plans. Alternative 1 is the honest baseline and this decision *is*
alternative 1 done properly, so it is adopted rather than rejected; it loses only when per-edge
random descents dominate even in index order, which is what phase 2 would answer. Alternative 2 is
premature: recommending a new structure to escape a fixable plan defect would buy a build step, a
staleness window and a second read path for a win we have not yet shown is needed. Alternative 3 is
rejected on this repository's own shape — average out-degree 5.91 and a near-DAG call graph give
the bottom-up direction nothing to win, and its switch heuristic would never fire.

### Consequences

- The cursor payload version increases; the surrogate-space keyset position is rebuild-local and
  must stay fenced by the generation id, or a resumed page could re-enter a different id space.
- A frozen port signature changes; `path.go` and every walk caller must move together.
- Traversal order over a level changes (index order, not hash order). Any emitted ordering that
  callers depend on must be re-established by the ranking pass, which is unaffected.
- Ranking keeps the external merge sort and its total tie-break key, so page order stays
  byte-identical across runs and resumptions.
- The Bloom saturation residual recorded in ADR-0001 §3.3 is **not** closed by this decision; it is
  deferred to the bitmap phase.
- Re-measurement on the reference store is a gate: if phase 1 reaches target, the blob is not built.

### Sources

[S1] https://doi.org/10.1145/48529.48535 · [S5] https://dl.acm.org/doi/10.5555/1109557.1109619 ·
[S7] https://dl.acm.org/doi/10.5555/2388996.2389013 · [S8] https://doi.org/10.1145/2442516.2442530 ·
[S17] https://www.usenix.org/conference/fast19/presentation/kumar ·
[S18] https://doi.org/10.1109/ICDE.2015.7113298 · [S24] https://www.sqlite.org/optoverview.html
