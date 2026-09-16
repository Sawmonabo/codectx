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

The sampled redglass run locates the writes: all but a fraction of a gigabyte of the 47 GB (the
remainder is the snapshot's content store) were written between "snapshot captured" and "packed
adjacency build started", the phase in which providers seal units and the dependence import stages
its export, while the database grew from 6 MB to 624 MB.
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

Every write an index run makes joins one write transaction, the ingestion group, and the group is
bounded by the writer's page cache; nothing commits per batch.

1. **Joining.** The store's ingestion calls (repository, blob and snapshot rows; unit begin, inputs,
   facts, evidence, aliases, search documents, delta state, seal, membership and carry-over;
   supplied indexes; capability rows; activation; abort) run inside the open group, each in a
   savepoint so a refused batch rolls back alone. The group's transaction is begun under a context
   that outlives any one caller.
2. **Commit points.** The group commits the moment the writer's page cache would spill a dirty
   page to the log; when an exclusive writer is waiting; at activation and at abort, whether they
   succeed or not; and when the store closes. The cache (`storage.writer_cache_kib`, 1 GiB) is
   about 262 000 pages, the size of the hash-keyed index set of a reference store of 100 000
   files, so a group that large lands many rows on each such page before the page is paid for. A
   group that never spills writes nothing to the log until its commit, and the commit appends each
   dirtied page exactly once. The bound is memory the engine allocates as the cache fills, so a
   small run never takes it all, and the run releases it at activation and abort.
3. **Why the cache and not the log.** A group that outgrows its cache spills dirty pages to the
   log one at a time and, when a spilled page is dirtied again, rewrites its frame in place; at
   commit it rewrites the checksum of every frame from the first such rewrite on. The kernel's
   page cache hides those rewrites on a short run, and the disk pays for them on a long one or
   under paced writeback: under an 8 MiB cache the store's ingestion test wrote 65 MiB of log
   frames and dirtied 555 MiB of them. The store detects a group's first frame from the log's
   size and header (the writer's journal size limit is zero, so the engine truncates the log at
   each reset, and a reset rewrites the header's salt and sequence), and commits.
4. **Statement journal in memory.** Each savepoint records the prior image of every page its batch
   touches; content-addressed rows touch a fresh page each. The engine's default moves that
   journal to a temporary file past 64 KiB, rewritten from offset zero at every batch: fifteen
   times the bytes stored, measured, in write calls the kernel absorbs on a short run and flushes
   on a long one. The store sets the engine's spill threshold to 64 MiB before the first
   connection, one batch's pages, released with the savepoint.
5. **Bounded writes in flight.** Every file the engine writes -- the log, the database at
   checkpoint, journals, sort spills -- reaches the disk through a file-system shim
   (`internal/storage/paced`) registered as the process default: after every window of
   8 MiB written to a file, the writer waits for the window before it to reach the disk and
   submits the new one. At most one window is in flight per file and at most two are dirty,
   whatever the transaction's size, so a commit's traffic is a stream at the disk's own rate
   with a bounded queue, never a burst. It changes nothing about what is durable when: the
   engine's own syncs still decide that, and find the file already written. The window is a
   layout constant, not a limit: it bounds what is outstanding, never what is written or how
   fast the disk may run. (Amended; see below.)
6. **Exclusive writers.** State that must be visible to every connection the moment the call
   returns (sessions, leases, heartbeats, retention, the planner's statistics, generation pins)
   keeps its own transaction. It announces itself, the next ingestion call commits the group, and
   it runs; it waits at most one batch.
7. **Own-writes reads.** The ingestion side's reads (unit states, aliases of dependency units,
   selected and carried units, delta state, unit inputs, the snapshot and blobs a capture recorded,
   generation status, provider runs) run on the group's connection while a group is open, so a run
   sees what it has stored. Query paths read the reader pool and see a run's units at its commits.
8. **Requirements, as tests.** The store's ingestion test asserts that the bytes the engine writes
   and the bytes sent to disk are each at most four times the bytes stored; a second test gives
   the writer a 2 MiB cache and asserts the log never exceeds twice the cache; a third writes
   64 MiB through the engine in one transaction and asserts that at most two windows of it are
   still dirty when the file is truncated, and that a wait was issued.

### Decision 5, amended 2026-09-16: the writer waits on the disk

The decision as first taken asked the kernel every hundred milliseconds to begin writing the
log's and the database's dirty pages and never waited. Two uncapped indexes of a 6 270-file
repository on a virtual machine that bounces every disk request through a 64 MB pool
(`swiotlb=force`) showed what that leaves: the group's commit appended 897 MB of log and the
checkpoint copied 892 MB into the database inside four seconds, the kernel logged the pool
exhausted twenty-two times in each run, and in the second run the whole machine stalled for
sixty seconds (I/O stall 94 %, memory stall 45 %) with the product's own write counter flat --
the host was draining a backlog the interval pacer had merely started. Bytes were write-once
as measured; the rate and the depth were not bounded by anything.

An interval pacer cannot bound them: the commit writes at memory speed, so by the time the
next tick fires the whole group is dirty, and the sync that ends the checkpoint submits all of
it as deep as the device queue allows. Alternative 4 below was rejected on the ground that a
bound "would need the writer to wait on the disk, which is a rate limit and a knob". The first
half is right and is now the decision; the second is wrong: a writer that waits for the
previous window before submitting the next runs at exactly the disk's rate with a fixed window
outstanding -- the way TCP's sender is clocked by acknowledgements and the way a database's
strict bytes-per-sync and checkpoint-flush settings are implemented -- and has no rate and no
setting. The window is a layout constant of the shim.

The shim wraps the engine's own file system rather than pacing named files from outside,
because the writes that burst are the engine's -- the log's frames, the checkpoint's pages,
a spilled statement journal, a sort's runs -- and only the file system sees every one of
them. On a platform without range writeback the wait is the wrapped file's own sync, which
with at most two windows dirty is a bounded wait.

## What the numbers must show

The ingestion test after the change, the group flushed and the log folded into the database before
measuring. "Engine" is what the engine passed to write calls; "disk" is what the process caused to
be sent to the block layer:

| units × facts | stored | engine | disk | wall |
|---|---|---|---|---|
| 600 × 40, per-batch commits (before) | 41.3 MiB | — | 1 854 MiB, 44.9× | 48 s |
| 600 × 40, 1 GiB log bound, 8 MiB cache, file statement journal | 73.1 MiB | 2 798 MiB, 23× | 74 MiB, 1.0× | 5.5 s |
| the same, paced | 73.1 MiB | 2 798 MiB | 161 MiB, 2.2× | 3.8 s |
| 600 × 40, cache-bound group, statement journal in memory, paced | 73.1 MiB | 73.1 MiB, 1.0× | 73.3 MiB, 1.0× | 3.5 s |
| 300 × 40 under a 2 MiB cache | 18.3 MiB | — | peak log 2.6 MiB | 3.1 s |

The probe that separated the two costs: 60 000 random 16-byte keys into a table and one index
under a 1 GiB cache wrote 10.3 MB for a 5.1 MB database (the log and the checkpoint, exactly), and
546 MB with a savepoint every forty rows. Stored counts the database and its log; the log is
truncated at the next reset, so the durable cost is the database plus the checkpoint copy. A
reference repository's index must write no more than four times its database and must not stall
its host's disk path.

## Alternatives considered

1. *Commit per batch* (the state this record replaces). Rejected by the measurements above: the
   cost is paid per commit, and a monorepo's run makes hundreds of thousands of them.
2. *Commit on a clock: one group per second or per N units.* Rejected: a cadence is a tuning knob
   with no right value (a second is 20 units on one host and 2 000 on another), and an idle group
   would hold the writer connection until its timer fired. Committing on demand, when another
   writer needs the database or the cache is full, has no knob and never holds a writer longer
   than one batch.
3. *Bound the group by the log's size (1 GiB) and let the kernel's page cache absorb the
   writer's spills.* Steel-man: the kernel's cache is larger than any setting and costs nothing to
   allocate. Rejected by measurement (decision 3): the spills are in-place rewrites of the log,
   which the kernel absorbs only while the pages are still dirty; on a run longer than the
   kernel's expiry, or under paced writeback, each rewrite reaches the disk, and the cost grows
   with the run.
4. *A pacer that bounds the kernel's dirty set to a constant.* Steel-man: it would make a group's
   commit invisible to the host. First rejected as a promise, on the ground that the writer would
   have to wait on the disk and that this is a rate limit and a knob; adopted by the amendment
   of decision 5 after measurement, since a writer clocked by the disk has no rate and no
   setting, and the interval pacer that was kept instead left the burst it was meant to remove.
5. *A staging database per run, attached and merged at activation.* Steel-man: the run's writes
   would be sequential appends into an empty file with no index maintenance, and the main
   database would stay untouched until the merge. Rejected: the merge inserts every row into the
   same hash-keyed indexes and pays the per-fact term once more; every read of a generation
   built over several runs would union several files; and sessions, leases and retention are
   keyed across generations in one file.
6. *Sorted, per-generation packed fact structures built at activation* (the shape ADR-0005 and
   ADR-0007 give the adjacency and the term statistics). This removes the per-fact term
   altogether, because a sorted build writes each page once with tens of rows on it, and it is
   the design for the residual cost recorded below. Not adopted here: the group removes the
   dominant terms with the storage layout unchanged, and the residual cost is bounded and
   measured, so the packed fact store is a decision for a record of its own when its trigger fires.

## Consequences and residual cost

A run's units become visible to query paths at its commits. A process that dies with a group
open loses the group: its units are rebuilt by the next run, which the store is built to do
cheaply, and nothing partly published is visible. An index run holds up to the writer's page cache
in memory for its writes, allocated as the groups fill it and released at activation and abort.
The write-ahead log holds at most one group plus the batch that spilled before the checkpoint
folds it and the next reset truncates it; a reader that keeps old frames open defers the reset
and the log grows by a group per commit until the reader is done, which the doctor's log check
reports past twice the cache. The callerless WAL maintenance routine and its high-water option are
deleted with this record; the planner's statistics refresh runs in an exclusive transaction of its
own. The store gains `Flush`, which commits the open group for a test or a tool that opens a
connection of its own.

The residual cost is the per-fact term across groups: a group touches at most one leaf per new
row per hash-keyed index, so once an index's leaf set is larger than the cache, every group of a
full run rewrites the leaves it touched, about the whole leaf set per group, and a delta run of K
new rows writes up to 8 KiB × K per such index. A store whose hash-keyed leaf sets are several
times the cache pays several times its size per full run. That is the trigger for the sorted-run
fact store of alternative 6: a measured run above the 4× bound, which a repository several times
the reference repository's size is expected to reach.

## Sources

- Write-Ahead Logging — https://sqlite.org/wal.html (what a commit writes to the log, what a
  checkpoint copies, why a reader never blocks the writer, when the log restarts).
- Atomic Commit In SQLite — https://sqlite.org/atomiccommit.html (a dirtied page is written in
  full at commit).
- SAVEPOINT — https://sqlite.org/lang_savepoint.html (a nested rollback inside one transaction).
- Isolation In SQLite — https://sqlite.org/isolation.html (a connection sees its own uncommitted
  writes; other connections see the last commit).
- PRAGMA statements: cache_size, cache_spill, journal_size_limit, wal_checkpoint, shrink_memory —
  https://sqlite.org/pragma.html (when the cache spills, when the log is truncated, what a
  passive checkpoint does, releasing the cache).
- Configuration options, SQLITE_CONFIG_STMTJRNL_SPILL — https://sqlite.org/c3ref/c_config_covering_index_scan.html
  (the statement journal's in-memory threshold, settable only before the engine initialises).
- Temporary disk files used by SQLite, statement journals — https://sqlite.org/tempfiles.html
  (what a savepoint records and when the journal spills to a file).
- SQLite source, wal.c — https://sqlite.org/src/file?name=src/wal.c (a page already in the log for
  the current transaction is rewritten in place; the frame checksums after it are recomputed at
  commit).
- sync_file_range(2) — https://man7.org/linux/man-pages/man2/sync_file_range.2.html
  (SYNC_FILE_RANGE_WAIT_BEFORE then SYNC_FILE_RANGE_WRITE: wait for the pages already
  submitted, then submit the dirty ones -- one window in flight).
- SQLite, "The OS Backend (VFS) To SQLite" — https://sqlite.org/vfs.html (a shim VFS wraps the
  default one and intercepts its file methods).
- Linux kernel, "DMA and swiotlb" — https://docs.kernel.org/core-api/swiotlb.html (the bounce-buffer
  pool every disk request of a virtual machine may have to pass through).
- RocksDB Tuning Guide, bytes_per_sync and wal_bytes_per_sync —
  https://github.com/facebook/rocksdb/wiki/RocksDB-Tuning-Guide (paced writeback of files being
  written, for the same reason).
- PostgreSQL, checkpoint_flush_after — https://www.postgresql.org/docs/current/runtime-config-wal.html
  (the same pacing for checkpoint writes, and why a burst at the sync is what it prevents).
- The /proc Filesystem, `/proc/[pid]/io` — https://www.kernel.org/doc/html/latest/filesystems/proc.html
  (`write_bytes`, `wchar` and `cancelled_write_bytes`: the figures the tests read).
- PSI — Pressure Stall Information — https://docs.kernel.org/accounting/psi.html (the IO stall
  figures quoted in Context).
