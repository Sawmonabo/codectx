# 18 — Visited-set structures and global ranking without materialising in RAM

- **Date:** 2026-09-15 · **Supplied by:** the project owner, from a literature pass run alongside
  research note 17 · **Used by:** [ADR-0005](../adr/ADR-0005-graph-traversal-layout.md).
- Every number below was extracted from the primary paper's own text (`curl | pdftotext`), not from
  a search summary. Three premises the evidence contradicts are flagged inline.

## A. Membership structures for a visited set

### A1. Plain bitset over dense integer ids

One bit per vertex, `bytes = n/8`:

| n | bytes | |
|---|---|---|
| 10⁶ | 125 000 | 122 KiB |
| 10⁷ | 1 250 000 | 1.19 MiB |
| 10⁸ | 12 500 000 | 11.9 MiB |
| 10⁹ | 125 000 000 | 119 MiB |

A hash set of 8-byte ids at n = 10⁹ is 8 GB of payload alone, 64× the bitset, and ≥ 16 GB at a 50 %
load factor. A 32 MiB last-level cache holds 2.68 × 10⁸ bits, so a plain bitset stays cache-resident
to that many vertices. The direction-optimising BFS paper treats the visited bitmap as the already
optimised baseline: "An effective optimization for shared-memory machines with large last-level
caches is to use a bitmap to mark nodes that have already been visited … These optimizations speed
up the edge checks but do not reduce the number of checks required" [R1]. The reference BFS of the
standard supercomputer graph benchmark carries one visited bit per vertex [R3][R4]; in the
distributed setting bit vectors "are more compact than sparse lists when the frontier is large and
also eliminate duplicates" [R5].

### A2. Container-partitioned compressed bitmaps

The 32-bit universe is split into 2¹⁶-value chunks keyed by the high 16 bits; each chunk is an
array container (sorted 16-bit values, ≤ 4 096, 2 B/value), a bitmap container ("1024 64-bit words
(using 8 kB) representing an uncompressed bitmap") or a run container (2 + 4r bytes for r runs); no
container "ever uses much more than 8 kB of memory" [R7]. Measured: consistently ≥ 3.5× faster
intersections and 1.7–43× faster unions than the word-aligned RLE formats; never slower than the
run-less form within 5 % and up to 2× faster on sorted data [R7][R8].

**Premise corrected.** Such a bitmap does not lose to an uncompressed bitset on dense random data:
its dense container *is* an uncompressed bitset plus a few bytes of chunk index (< 1 % overhead).
It loses only to pure run-length formats on long-run sorted data (7.4×: 3.2 vs 0.43 bits/value on
one weather dataset), which run containers close [R7]. An independent SIGMOD 2017 verdict quoted by
the project: use this format "whenever possible".

### A3. Bloom, blocked Bloom, cuckoo filters

False-positive rate `ε ≈ (1 − e^(−kn/m))^k`; optimal `k = (m/n)·ln 2`; inverting,
`m/n = 1.44·log₂(1/ε)`: 9.6 bits/element for 1 %, 14.4 for 0.1 %, and every decade of ε costs
4.79 bits/element [R10][R11]. The information-theoretic minimum is `log₂(1/ε)`, so a Bloom filter
carries 44 % overhead [R12]. Cuckoo filters use `(log₂(1/ε) + 3)/α` bits at α = 95.5 % with two
memory references per lookup, and beat a space-optimised Bloom filter below ε = 3 % [R12]. Blocked
Bloom filters put all k bits in one cache line (one miss per probe) at the cost of more space at
equal ε [R13][R14]. **Decision rule:** a visited set is monotone, so a filter's lack of deletion
costs nothing; but whenever ids are dense the plain bitset is 1 bit/element with zero false
positives and beats every filter.

### A4. Succinct rank/select

Measured overheads beyond the raw bits: 3.2 % (rank) and 0.39 % (select) [R15]; 3.51 % / 3.58 %
with faster queries [R16]; ~25 % / ~12 % for the older word-popcount layout [R17]; 0.78 % for a
constant-worst-case variant [R18]. **Premise corrected.** Enumerating a visited set in id order
needs no rank/select: a `ctz`/`blsr` word scan emits set bits in ascending order at 0 % overhead.
`select(i)` earns its 3 % only to resume at the i-th set element; a cursor that stores a bit
position instead of an ordinal makes it unnecessary.

### A5. Persisting and resuming a visited set across paged requests

The literature is thin. Superstep checkpointing in bulk-synchronous graph systems is crash
recovery at superstep granularity, not paged resumption, and is not precedent for a cursor API
[R19][R20][R21]. The on-point shape is a persisted bitmap snapshot plus an append-only log of
newly set ids, compacted periodically: a portable, memory-mappable serialisation format read by
every implementation of the container bitmap [R22], its production use as per-object durable
deletion vectors [R23], and a proposed delta log of type-length-value operations over a snapshot
because "it's costly to rewrite the entire snapshot on each change" [R24]. **Design implication:**
the visited set is monotone and append-only, so snapshot-plus-delta writes O(new ids) per page; the
frontier mutates and must be spooled in full.

### A6. External-memory duplicate elimination in BFS

`scan(x) = Θ(x/(D·B))`, `sort(x) = Θ((x/(D·B))·log_{M/B}(x/B))` [R25]. The level-batched external
BFS builds level `L(t)` by sorting the neighbour multiset of `L(t−1)` and subtracting `L(t−1) ∪ L(t−2)`
by scan, in `O(n + sort(n+m))` I/Os; the `Θ(n)` term is one unstructured adjacency access per node,
and the two-level subtraction is correct only for undirected graphs [R26][R25]. The sublinear
variant removes the `Θ(n)` term by physically preclustering adjacency and scanning a hot pool
[R25]; a computational study ran a 130 M-node / 1.4 B-edge crawl in under four hours on one disk
[R27]. Delayed duplicate detection appends children unchecked, then sorts or hash-partitions
before merging; hash-based detection was 41 % faster on the largest instance, and each logical file
is "broken up into a sequence of smaller file segments" so a segment can be deleted as it is read,
holding disk footprint below 2× during the merge [R28]. **Decided:** when the output must also be
globally ordered, the sort needed for deduplication is the sort already owed.

## B. Global ranking without materialising in RAM

### B7. External merge sort

Lower bound `Ω((N/B)·log_{M/B}(N/B))` I/Os, matched by `M/B`-way merge sort [R29]. Merge fan-in is
`Θ(M/B)`; with M = 4 GiB and B = 1 MiB the fan-in is ~4 096 and one merge pass covers ~16 TiB, so
any repository index sorts in two passes: run generation plus one merge. Replacement selection
doubles average run length but pays cache misses on every record [R30][R31][R32]; with fan-in
already ~4 000, prefer load-sort-store runs.

### B8. Top-k and threshold algorithms — rejected for a paged answer

A bounded min-heap gives the k best in `O(n log k)` and `O(k)` memory, but page 2 needs ranks
k+1..2k, which the heap discarded; re-running with 2k re-scans everything, and if the data changed
between runs, entries duplicate or vanish across pages. Its order among equal scores is an artefact
of insertion order. Threshold algorithms are instance-optimal for monotone aggregation with a
constant-size buffer [R33], and share the same failure: they terminate as soon as the threshold is
met, discarding exactly the state a continuation needs. For a paginated, globally ranked,
resumable result the external merge sort with a keyset cursor is the correct structure.

### B9. MSD radix-partitioned ranking

Partition-then-sort on the most-significant radix bits yields range-disjoint partitions whose
concatenation is the global order: up to 680 M tuples/s with flat scaling, where sort-then-merge
degrades past 256 M tuples as merge fan-in grows; partitioning alone reaches 4 G tuples/s; fan-out
is bounded by TLB entries (throughput collapses past 32 with 2 MiB pages) unless software-managed
buffers are used [R34][R35]. LSD radix offers nothing servable before the last pass. Cost: skew on
non-uniform keys. A candidate accelerator only if the sort ever becomes the measured bottleneck.

### B10. Determinism — byte-identical order across runs and resumptions

Two requirements compose: a **total** comparison key (append an intrinsic unique tie-breaker so no
two records compare equal, after which stability is moot and any correct sort yields the same
sequence), and a deterministic merge tie rule where equal keys remain [R36]. The trap the sources
do not state: a **positional** tie-break (input index, run number, insertion order) holds only when
input order is reproducible; across a resumption the input is re-enumerated from a possibly
different spool layout, so equal-scoring entries silently reorder and an entity lands on two pages
or none. A resumable external sort tie-breaks on a content-derived key, never a positional one.

## Sources

[R1] Direction-Optimizing Breadth-First Search — https://www.semanticscholar.org/paper/6bc45efd5b27f661b1cbbab8998b87a34da5967e
[R2] The GAP Benchmark Suite — https://arxiv.org/abs/1508.03619
[R3] Graph500 benchmark specification — https://graph500.org/
[R4] Graph500 reference implementation — https://github.com/graph500/graph500
[R5] Parallel Breadth-First Search on Distributed Memory Systems — https://arxiv.org/abs/1104.4518
[R6] Better Bitmap Performance with Roaring Bitmaps — https://arxiv.org/abs/1402.6407
[R7] Consistently Faster and Smaller Compressed Bitmaps with Roaring — https://arxiv.org/abs/1603.06549
[R8] Roaring Bitmaps: Implementation of an Optimized Software Library — https://arxiv.org/abs/1709.07821
[R9] Roaring bitmap project site — https://roaringbitmap.org/
[R10] Bloom Filter's False Positive Rate (University of Thessaly note) — https://www.inf.uth.gr/
[R11] A New Analysis of the False-Positive Rate of a Bloom Filter (NIST) — https://www.nist.gov/
[R12] Cuckoo Filter: Practically Better Than Bloom — https://doi.org/10.1145/2674005.2674994
[R13] Cache-, Hash- and Space-Efficient Bloom Filters — https://doi.org/10.1145/1498698.1594230
[R14] Blocked Bloom Filters with Choices — https://arxiv.org/abs/2501.18977
[R15] Space-Efficient, High-Performance Rank and Select Structures on Uncompressed Bit Sequences — https://link.springer.com/chapter/10.1007/978-3-642-38527-8_15
[R16] Engineering Compact Data Structures for Rank and Select Queries on Bit Vectors — https://arxiv.org/abs/2206.01149
[R17] Rank and Select: Another Lesson Learned — https://arxiv.org/abs/1605.01539
[R18] SPIDER: Improved Succinct Rank and Select Performance — https://arxiv.org/abs/2405.05214
[R19] Pregel: A System for Large-Scale Graph Processing — https://doi.org/10.1145/1807167.1807184
[R20] Fast Failure Recovery in Distributed Graph Processing Systems — https://doi.org/10.14778/2735496.2735506
[R21] Lightweight Fault Tolerance in Large-Scale Distributed Graph Processing — https://arxiv.org/abs/1601.06496
[R22] Portable Roaring serialization format — https://github.com/RoaringBitmap/RoaringFormatSpec/
[R23] Delta Lake protocol: deletion vectors — https://github.com/delta-io/delta/blob/master/PROTOCOL.md
[R24] Delta-roaring bitmap storage proposal (FeatureBase issue 24) — https://github.com/FeatureBaseDB/featurebase/issues/24
[R25] External-Memory Breadth-First Search with Sublinear I/O — https://people.mpi-inf.mpg.de/~mehlhorn/ftp/ExternalBFS.pdf
[R26] I/O-Complexity of Graph Algorithms — https://dl.acm.org/doi/10.5555/314500.314891
[R27] A Computational Study of External-Memory BFS Algorithms — https://resources.mpi-inf.mpg.de/departments/d1/teaching/ws10/models_of_computation/Ajwani.pdf
[R28] Best-First Frontier Search with Delayed Duplicate Detection — https://cdn.aaai.org/AAAI/2004/AAAI04-103.pdf
[R29] The Input/Output Complexity of Sorting and Related Problems — https://doi.org/10.1145/48529.48535
[R30] OpenDSA: External Sorting — https://opendsa-server.cs.vt.edu/ODSA/Books/Everything/html/ExternalSort.html
[R31] Two-way Replacement Selection — http://www.vldb.org/pvldb/vldb2010/pvldb_vol3/R76.pdf
[R32] Memory Management During Run Generation in External Sorting — https://doi.org/10.1145/276304.276352
[R33] Optimal Aggregation Algorithms for Middleware — https://www.wisdom.weizmann.ac.il/~naor/PAPERS/middle_agg.pdf
[R34] Multi-Core, Main-Memory Joins: Sort vs. Hash Revisited — http://www.vldb.org/pvldb/vol7/p85-balkesen.pdf
[R35] A Comprehensive Study of Main-Memory Partitioning and its Application to Large-Scale Comparison- and Radix-Sort — https://www.cs.columbia.edu/~orestis/sigmod14I.pdf
[R36] XiSort: Deterministic Sorting via IEEE-754 Total Ordering — https://arxiv.org/abs/2505.11927
