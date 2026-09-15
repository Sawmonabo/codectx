# ADR-0004 — WAL synchronous mode

**Status:** Accepted, 2026-09-15

## Context

A throughput investigation of a cold index profiled the run against a 3 584-file repository and
found it was neither CPU-bound nor parse-bound: the parent process sat at 23 % of one core out of
sixteen, the two parser workers at ~1.7 % combined, and parent threads were parked in
`jbd2_log_wait_commit` — the ext4 journal commit an `fsync()` waits on. The run wrote 3.7 GB across
1.44 M write syscalls to produce a 218 MiB store. The index is **fsync-bound**.

Two commit streams account for it. One is the content-addressed blob store, which syncs each staged
file and its bucket directory individually; that is a separate decision. The other is this one: the
store opened its single writer connection with `synchronous = FULL`, which in WAL mode adds an
`fsync()` of the write-ahead log after **every** transaction commit [S1]. The index commits once per
record batch, so the setting multiplies directly into the indexing commit stream.

Isolated measurement of that commit stream alone, on ext4 (400 single-row transactions of a 4 KiB
payload through the store's own `write` helper, the whole store otherwise at its defaults):

| `synchronous` | 400 commits | per commit |
|---|---|---|
| `FULL` | 4.10 s, 4.01 s | **10.2 ms**, 10.0 ms |
| `NORMAL` | 43 ms, 42 ms | **0.11 ms** |

A ~95x difference on the commit path. (Measuring this on `tmpfs`, where `/tmp` lives on the
development host, shows no difference at all: tmpfs has no `fsync()` to wait on. Every figure above
is from an ext4 temporary directory. The end-to-end fixture store is far too small — a handful of
commits inside a ~2.4 s process — for the difference to leave the noise; the fixture measured
2.32–2.59 s under NORMAL against 2.17–2.72 s under FULL, i.e. no signal.)

What FULL buys, precisely, is durability of the last commits across a **power loss or hard reset**.
It buys nothing against an application crash: transactions are durable across a process crash
regardless of the synchronous setting [S1]. And it buys nothing against corruption, because WAL mode
is not at risk of corruption at NORMAL — SQLite's own matrix rates WAL + NORMAL as atomic,
consistent and isolated, losing only durability, and states that a transaction committed in WAL mode
with `synchronous=NORMAL` "might roll back following a power loss or system crash" [S1].

The question is therefore whether codectx needs the last few commits of an index to survive a power
cut, at a price of ~10 ms of fsync per batch.

## Decision 1 — the WAL writer defaults to `synchronous = NORMAL`

It does not. The store is **rebuildable derived data**: everything in it is a function of the
repository's own bytes, and losing it entirely costs a re-index, not information. Three properties
make a rolled-back tail specifically harmless rather than merely cheap:

- **Consistency is guaranteed, not hoped for.** WAL + NORMAL never corrupts the database and is
  always consistent [S1]. A power loss leaves a database that opens and reads correctly, at some
  slightly earlier commit.
- **Generation activation is atomic.** A generation becomes visible in one commit. A rolled-back
  tail therefore leaves the previous generation active — a complete, coherent, older answer — never
  a half-published one.
- **Content blobs are ordered before the commit that names them.** Blob content is synced as a group
  before the commit that publishes a manifest referencing it, so the durable ordering "content
  first, then the row that names it" holds. A rolled-back generation commit can therefore only leave
  **orphan blobs** — content on disk that no row names — which the retention collector's CAS sweep
  reclaims: each pass walks every bucket of the store in bounded chunks, asks the index which of the
  hashes it holds a row for, and removes those it does not, once they are older than the
  `retention.blob_grace` window (the same window the row-side grace protocol uses, so an object
  published seconds before a commit that has not landed yet is never taken). It cannot leave a
  manifest pointing at content that is not there.

The worst outcome of a power loss under NORMAL is: the workspace is one generation behind, plus some
orphan blobs a later sweep removes. That is indistinguishable from the power loss having happened a
few seconds earlier, which no setting can prevent.

A new configuration key `storage.synchronous` (`"normal"` | `"full"`, default `"normal"`) lets an
operator who disagrees — or who runs on hardware where a re-index is genuinely expensive and power
loss genuinely likely — buy the old behaviour back with one line. An unrecognized spelling is
refused at load time with `storage.synchronous is "…"; use "normal" or "full"`, and the store fails
closed on one too: it never picks a durability mode the operator did not ask for.

The pragma applies to the **writer connection only**. Readers are opened `query_only = ON` and never
commit, so the mode is behaviourally inert for them; they stay pinned at `FULL` so that every pooled
connection still has its full pragma set read back and verified against a known value at open.

### Alternatives considered

**Keep `FULL` as the default and let operators opt into `NORMAL`.** The strongest case: a durability
promise should be weakened deliberately by the person who bears the risk, not by a default; defaults
are what almost everyone runs; and "your index is one generation stale after a power cut" is a
surprise, however cheap. This is a real argument and it is why the key exists at all. It loses on
what the promise is actually worth *here*. FULL's guarantee is exclusively about power loss, and its
payoff on a rebuildable cache is to save a re-index of the last batch — while its cost is paid on
every commit of every index on every machine, forever. A default should price the common case, and
the common case is a laptop or CI worker that is not losing power mid-index. Charging 10 ms per
batch to everyone, permanently, to avoid re-deriving derived data is the wrong trade; the operator
who genuinely needs it is the minority and is served by one key.

**Choose the mode per transaction — `NORMAL` for bulk index batches, `FULL` for the generation
activation commit.** Superficially the best of both: pay the fsync exactly once per generation, on
the commit that matters, and never on the thousands that do not. It was rejected on two grounds.
First, it does not deliver what it appears to: `synchronous` is a connection-level setting and all
writes go through a single writer connection, so it would have to be toggled around the activation
commit, and the fsync it then performs makes the *WAL* durable — it does not retroactively make the
batch commits ahead of it durable in any sense NORMAL does not already provide, because they are
already in the same WAL that is being synced. The apparent gain is mostly an illusion; what it
actually buys is one sync at the one point a crash window matters, which is real but small. Second,
it buys that small gain with a genuine cost: a durability mode that varies within a run is a
property no readback can verify at open, it defeats the one-pragma-set-per-connection invariant the
verified connector enforces, and it makes "what mode is this database running in?" unanswerable from
the outside. A single, verified, operator-visible setting is worth more than the fraction of a
promise the toggle recovers. Not ruled out forever: if a future measurement shows the activation
crash window matters, this is where to revisit.

**Leave `synchronous` alone and tune checkpointing instead.** The case: under NORMAL the checkpoint
becomes the only operation issuing a sync [S2], so checkpoint frequency — not commit frequency — is
what remains of the commit stream, and it is tunable without touching a durability promise at all.
It is rejected as an *alternative* because it is not one: checkpoint tuning changes nothing under
FULL, where the per-commit WAL fsync dominates and would still be paid. It is a complement, and it
is addressed below.

### Consequences and trade-offs accepted

Accepted: after a power loss or hard reset, an index may be missing its most recent commits, and the
store may hold orphan blobs until a retention pass at least `retention.blob_grace` after they were
written sweeps them. Not accepted, and not affected:
corruption (impossible in WAL mode at NORMAL), a half-published generation (activation is atomic),
a manifest naming absent content (content is ordered first), or any loss following an application
crash or a normal process kill (durable regardless of this setting [S1]).

The setting is operational, not semantic: it changes how a result is written, never what the result
is. It is therefore deliberately excluded from the source-policy, analysis-config and context-policy
fingerprints — changing it must not invalidate a single stored unit.

## Decision 1a — session and receipt state is covered by the same default, and the window is stated

Decision 1's justification — "the store is rebuildable derived data" — is true of the index and is
**not** true of everything the store holds. Sessions, source receipts and read confirmations
(`internal/storage/sqlite/state.go`) are a record of what an actor did, not a function of the
repository's bytes; nothing can recompute them. There is one writer connection
(`internal/storage/sqlite/open.go`), so `storage.synchronous` applies store-wide and NORMAL covers
these rows too. That was not argued for in Decision 1, so it is decided here.

**Decision: session and receipt writes run under the same setting, and the guarantee is stated
rather than implied.** The exact window: a receipt or confirmation commit is durable against an
application crash or a normal process kill immediately (that never depended on this setting), and
durable against a **power loss or hard reset** only once the write-ahead log has been synced — which
under NORMAL happens at the next checkpoint (`storage.wal_high_water_bytes`, 64 MiB by default, or
the writer's passive checkpoint) or at a clean close, not at the commit. Between the commit and that
point, a power loss can roll the commit back.

**Why that is acceptable.** The failure direction is fail-closed: a rolled-back receipt row **loses**
an attestation, it never fabricates one. An acknowledgement echoing a receipt whose row is gone is
refused as `CTX_CURSOR_INVALID` — the same refusal an expired receipt already gets — and the actor
re-reads and acknowledges again. A protocol that could silently *gain* a confirmation nobody made
would not be acceptable on these terms; losing one that the actor can redo is. The alternative,
syncing around the confirmation commit (a pinned writer connection raising `PRAGMA synchronous` for
that transaction, or an explicit `wal_checkpoint` after it), buys a narrower window at the cost of a
second durability mode inside one connection and an fsync on a query-path write; it is not taken
here, and an operator who needs it has the one setting that already covers these rows:
`storage.synchronous = "full"`, which makes every commit — index and receipt alike — durable at
commit.

## Decision 2 — checkpoint settings are reported, not changed

The brief asked whether checkpoint frequency contributes to the commit stream and whether a one-line
safe default exists. Both pragmas were read back from the embedded engine as the store actually
opens it, rather than quoted from documentation:

```
PRAGMA wal_autocheckpoint  = 1000        (pages)
PRAGMA page_size           = 4096
PRAGMA journal_size_limit  = -1          (no limit)
```

So the engine auto-checkpoints, PASSIVE, every **1000 pages ≈ 4 MiB** of WAL [S1], and never
truncates the WAL file afterwards — it reuses it from the beginning [S2][S1], which is the desired
behaviour and the reason `journal_size_limit` should stay at -1.

Two findings follow, both reportable and neither acted on here:

1. **Checkpoint frequency does contribute, and more so under NORMAL than under FULL.** NORMAL removes
   the per-commit WAL fsync; it does not remove the syncs a checkpoint performs — the WAL is synced
   before each checkpoint and the database file after each completed one [S1]. Under NORMAL the
   checkpoint is in fact the *only* thing issuing a sync [S2]. At ~4 MiB of WAL per checkpoint, a
   cold index that writes hundreds of megabytes still performs a checkpoint every few megabytes.
   Raising `wal_autocheckpoint` would trade a larger peak WAL file for proportionally fewer syncs.

2. **`storage.wal_high_water_bytes` is a backpressure trigger, not a second checkpoint interval, and
   the 16x gap is what makes it one.** `MaintainWAL` returns `backpressure` when the WAL still
   exceeds the 64 MiB mark *after* a PASSIVE checkpoint, or when that checkpoint reported busy. At
   defaults the engine's own 4 MiB auto-checkpoint keeps the WAL far below 64 MiB, so that branch
   fires only when checkpointing cannot advance — a long-lived reader pinning the WAL while the
   indexer keeps committing — which is precisely the condition it exists to detect. It is not dead
   code duplicating `wal_autocheckpoint`; the same key also serves as a diagnostic threshold in
   `doctor`, which stays meaningful regardless.

**Open question, deliberately not resolved here.** Raising `wal_autocheckpoint` would trade a larger
peak WAL for proportionally fewer syncs, and `journal_size_limit` should stay at -1 either way so the
WAL is reused rather than repeatedly truncated and re-grown. But deriving `wal_autocheckpoint` from
`storage.wal_high_water_bytes` — the obvious one-line coupling — would convert a backpressure
trigger into a routine checkpoint interval, which is plausibly the opposite of what the mechanism is
for, and would raise peak WAL and peak dirty pages up to 16x at current defaults. Whether the two
thresholds should be coupled needs measurement **under reader pressure**, not reasoning. Nothing is
changed here.

## Sources

[S1] *Pragma statements supported by SQLite* — https://www.sqlite.org/pragma.html — the
`synchronous` matrix (WAL + NORMAL is atomic, consistent and isolated and loses only durability;
WAL mode is safe from corruption at NORMAL; a WAL transaction at NORMAL "might roll back following a
power loss or system crash"; transactions are durable across application crashes regardless of the
setting; EXTRA is no different from FULL in WAL mode; FULL adds one WAL sync after each commit,
while at NORMAL the WAL is synced before each checkpoint and the database after each completed one),
the `wal_autocheckpoint` default of 1000 pages, and `journal_size_limit`'s default of -1 meaning no
limit. Used for Decision 1's durability analysis and both findings in Decision 2.

[S2] *Write-Ahead Logging* — https://www.sqlite.org/wal.html — writers sync the WAL on every commit
at FULL and omit that sync at NORMAL; with NORMAL "the checkpoint is the only operation to issue an
I/O barrier or sync operation", at the stated cost that transactions may roll back after a power
failure; the default automatic checkpoint at 1000 pages; and that a checkpoint does not normally
truncate the WAL but restarts it from the beginning. Used for the cost model in Context and for
finding 1 in Decision 2.
