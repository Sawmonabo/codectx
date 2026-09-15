# ADR-0008 — Ingestion commits once per run: the ingestion group

**Status:** Accepted, 2026-09-15 · **Informs:** ADR-0001 §1.4 (store ratio gate), ADR-0002 (surrogate
identities), ADR-0004 (WAL synchronous mode) · **Inputs:** the measurements below, taken on this
repository's reference stores on 2026-09-15.

## Context

An index run of the reference repository (20 038 units, a 3.2 GB store) sent 118 GB to disk in
thirty minutes at a sustained 100 MB/s, and every earlier index run of every reference repository
shows the same shape:

| run | wall | database | bytes written | ratio |
|---|---|---|---|---|
| redglass, cert run | 2:11 | 473 MB | 25.5 GB | 54× |
| ai-sidekicks, cert run | 2:54 | 465 MB | 35.9 GB | 77× |
| r3, cert run | 6:18 | 1.8 GB | 76.7 GB | 43× |
| r3, packed-lexical baseline | 12:44 | 2.5 GB | 96.7 GB | 39× |
| r3, packed-lexical delta | 22:15 | 3.2 GB | 136.0 GB | 43× |
| redglass, wave-i tip, sampled | 6:45 | 624 MB | 46.6 GB | 75× |

The sampled redglass run locates the writes: 47.2 GB of the 46.6 GB total (the remainder is the
snapshot's content store) were written between "snapshot captured" and "packed adjacency build
started", the phase in which providers seal units, while the database grew from 6 MB to 624 MB.
Activation itself, the packed adjacency and term-statistics builds, wrote 0.1 GB. The host's disk
path saturated: 98 % full IO stall, load 22 on 16 cores, and the machine froze for the length of
the run.

The store's ingestion test reproduces the shape without a repository. It seals file-scoped units
the way a run does, one batch of nodes, relations, evidence, aliases and search documents per
unit, and reads the bytes the process sent to the block layer from `/proc/self/io`:

| units × facts | commits | stored | written | ratio | wall |
|---|---|---|---|---|---|
| 600 × 40 | 3 600 | 41.3 MiB | 1 854 MiB | 44.9× | 48 s |
| 1 000 × 40 | 6 000 | 65.6 MiB | 3 466 MiB | 52.8× | 106 s |
| 100 × 400 | 600 | 68.2 MiB | 1 544 MiB | 22.6× | 50 s |

The two shapes with the same fact count separate the cost of a commit from the cost of a fact:
6 000 commits × (F + 40 r) = 443 000 page writes and 600 commits × (F + 400 r) = 197 000 page
writes give F ≈ 46 pages per commit and r ≈ 0.71 pages per fact. The fixed term is the root and
interior pages of every b-tree a batch touches, the database header and the full-text index's
segment tables; the per-fact term is one leaf per hash-keyed index per row, because a
content-derived identity lands at a uniformly random point of its index. SQLite writes each dirtied
page to the write-ahead log in full and the checkpoint copies it again, so a commit costs
8 KiB per dirtied page however few bytes changed on it. Committing every batch pays both terms per
batch; the reference repository seals 20 000 units in six batches each.

## Decision

Every write an index run makes joins one write transaction per run, the ingestion group, and the
group commits at the points below; nothing commits per batch.

1. **Joining.** The store's ingestion calls (repository, blob and snapshot rows; unit begin, inputs,
   facts, evidence, aliases, search documents, delta state, seal, membership and carry-over;
   supplied indexes; capability rows; activation; abort) run inside the open group, each in a
   savepoint so a refused batch rolls back alone. The group's transaction is begun under a context
   that outlives any one caller.
2. **Commit points.** The group commits when its write-ahead log reaches 1 GiB; when an exclusive
   writer is waiting; at activation and at abort, whether they succeed or not; and when the store
   closes. 1 GiB of log is about 262 000 pages, the size of the hash-keyed index set of a reference
   store of 100 000 files, so a group that large lands many rows on each such page before the page
   is paid for; the bound is disk, never memory, because the writer's page cache spills to the log
   as it fills.
3. **Exclusive writers.** State that must be visible to every connection the moment the call
   returns (sessions, leases, heartbeats, retention, the planner's statistics, generation pins)
   keeps its own transaction. It announces itself, the next ingestion call commits the group, and
   it runs; it waits at most one batch.
4. **Own-writes reads.** The ingestion side's reads (unit states, aliases of dependency units,
   selected and carried units, delta state, unit inputs, the snapshot and blobs a capture recorded,
   generation status, provider runs) run on the group's connection while a group is open, so a run
   sees what it has stored. Query paths read the reader pool and see a run's units at its commits.
5. **Requirement, as a test.** The store's ingestion test asserts that the bytes sent to disk are at
   most four times the bytes stored, and fails at 44.9× under per-batch commits.

## What the numbers must show

The ingestion test after the change, the group flushed and the log folded into the database before
measuring:

| units × facts | stored | written | ratio | wall |
|---|---|---|---|---|
| 600 × 40 | 73.1 MiB | 74.1 MiB | 1.0× | 5.5 s |
| 1 000 × 40 | 122.0 MiB | 123.5 MiB | 1.0× | 9.7 s |
| 100 × 400 | 117.9 MiB | 128.8 MiB | 1.1× | 8.4 s |

Stored counts the database and its log, which holds the group's frames until the next checkpoint
folds them; against the database alone the durable cost is about 2×, the log copy plus the
checkpoint copy. A reference repository's index must write no more than four times its database
and must not stall its host's disk path.

## Alternatives considered

1. *Commit per batch* (the state this record replaces). Rejected by the measurements above: the
   cost is paid per commit, and a monorepo's run makes hundreds of thousands of them.
2. *Commit on a clock: one group per second or per N units.* Rejected: a cadence is a tuning knob
   with no right value (a second is 20 units on one host and 2 000 on another), and an idle group
   would hold the writer connection until its timer fired. Committing on demand, when another
   writer needs the database, has no knob and never holds a writer longer than one batch.
3. *A staging database per run, attached and merged at activation.* Steel-man: the run's writes
   would be sequential appends into an empty file with no index maintenance, and the main
   database would stay untouched until the merge. Rejected: the merge inserts every row into the
   same hash-keyed indexes and pays the per-fact term once more; every read of a generation
   built over several runs would union several files; and sessions, leases and retention are
   keyed across generations in one file.
4. *Sorted, per-generation packed fact structures built at activation* (the shape ADR-0005 and
   ADR-0007 give the adjacency and the term statistics). This removes the per-fact term
   altogether, because a sorted build writes each page once with tens of rows on it, and it is
   the design for the residual cost recorded below. Not adopted here: the group removes the
   dominant terms with the storage layout unchanged, and the residual cost is bounded and
   measured, so the packed fact store is a decision for a record of its own when its trigger fires.

## Consequences and residual cost

A run's units become visible to query paths at its commits. A process that dies with a group
open loses the group: its units are rebuilt by the next run, which the store is built to do
cheaply, and nothing partly published is visible. The write-ahead log grows to the group's dirtied
pages, up to 1 GiB, before the checkpoint folds it. The callerless WAL maintenance routine and its
high-water option are deleted with this record; the planner's statistics refresh runs in an
exclusive transaction of its own. The store gains `Flush`, which commits the open group for a test
or a tool that opens a connection of its own.

The residual cost is the per-fact term inside one group: a group touches at most one leaf per new
row per hash-keyed index, so a delta run of K new rows writes up to 8 KiB × K per such index, and a
full run writes each hash-keyed index about once. The trigger for the packed fact store of
alternative 4 is a measured delta run on a reference store above the 4× bound.

## Sources

- Write-Ahead Logging — https://sqlite.org/wal.html (what a commit writes to the log, what a
  checkpoint copies, why a reader never blocks the writer).
- Atomic Commit In SQLite — https://sqlite.org/atomiccommit.html (a dirtied page is written in
  full at commit).
- SAVEPOINT — https://sqlite.org/lang_savepoint.html (a nested rollback inside one transaction).
- Isolation In SQLite — https://sqlite.org/isolation.html (a connection sees its own uncommitted
  writes; other connections see the last commit).
- The /proc Filesystem, `/proc/[pid]/io` — https://www.kernel.org/doc/html/latest/filesystems/proc.html
  (`write_bytes`: bytes the process caused to be sent to the storage layer, the figure the test
  and the sampled runs read).
- PSI — Pressure Stall Information — https://docs.kernel.org/accounting/psi.html (the IO stall
  figures quoted in Context).
