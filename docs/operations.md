# Operations

How to check that a codectx workspace is healthy, what to do when it is not, and
what the tool reclaims on its own. Configuration keys named here are documented
in [configuration](configuration.md); the security reasoning behind the controls
is in [the threat model](threat-model.md).

## The first command to run

```
codectx gc              # give the workspace's pooled scratch space back
codectx doctor          # human summary
codectx doctor --json   # one machine-stable envelope
codectx doctor --deep   # runs the whole-database checks an ordinary run reports unverified
codectx doctor --offline
```

`doctor` returns a **bounded list of checks**. Each check carries a name, a
state, a human detail, a machine reason code and a remediation string, and the
report carries the build identity, the flags it ran under and an overall state.

| State | Meaning |
|---|---|
| `pass` | Checked, and healthy. |
| `warn` | Checked, degraded, still serving. Act before it becomes `fail`. |
| `fail` | Checked, and this workspace cannot serve correctly until it is fixed. |
| `unavailable` | **Not checked here.** The check or metric could not be measured on this platform or on this build. It is never reported as a passing check and never as a zero measurement. |
| `unverified` | **Not checked here either, but only because you did not ask.** The check is available; it walks the whole database, so an ordinary run skips it and says so. Its detail ends `not verified in shallow mode; run doctor --deep`. Like `unavailable` it does not degrade the overall state. |

`doctor` is cheap by default, and that is enforced by which checks run. An
ordinary run reads the database header, page count, schema fingerprint,
write-ahead-log mode and the two file sizes -- all constant cost, all still able
to fail the report. Two checks walk the whole database and are therefore
reported `unverified` until you pass `--deep`:

| Check | What `--deep` verifies |
|---|---|
| `storage_integrity` | `quick_check`, the foreign key check and the full-text index walk |
| `storage_accounting` | the generation, unit, blob, lease and session row counts. The database and write-ahead-log **sizes are still reported without `--deep`**, and so is the warning that the log is past its high-water mark -- that is a file stat, and it is the cheapest real finding this command has. |

A third, `source_retention`, is a bounded sample either way -- four objects
ordinarily, sixty-four under `--deep` -- and it reports `pass` on what it
sampled. It is `unverified` in one case only: the sample came back empty, and
telling "nothing is retained here" from "every retained object is unreadable"
needs the retained-object count, which is one of the row counts an ordinary run
does not read. Neither is claimed.

Nothing is dropped from the report: every skipped check still appears as a row,
in state `unverified`, naming `--deep` as what verifies it. On a 55 MB index the
walk costs 296 ms against 0.24 ms for the header reads, and it grows with the
database; on a multi-gigabyte index it is tens of seconds.

The parser smoke checks are also `--deep` only.

### What is checked

- **Build and toolchain pins** — the build identity and the pinned versions this
  binary was built against.
- **Workspace and data-directory permissions** — that the data directory exists,
  is user-private and is writable.
- **Free space** — measured against `resources.min_free_disk_bytes`.
- **Database schema, full-text index and write-ahead log** — including a
  write-ahead log larger than the store's ingestion group bound, which means it
  has not been checkpointed since the last run.
- **The active generation pointer** — whether this repository has one at all.
- **Recent capture and freshness** — how stale the active generation is.
- **Sampled content-store blocks** — a bounded sample of stored source blocks is
  re-verified against its recorded hash. `--deep` widens this.
- **Bundled grammar availability.**
- **Managed toolchain status, one check per lock entry** — identified by the
  entry name the lock records. The dependence backend is reported as
  `engine <version> <digest>`; no third-party product name is printed.
- **Orphan temporary state** — staging directories, materializations and spools
  that no live owner references.
- **Session and lease retention** — live and expired sessions, and the retention
  leases that keep a generation collectable or not.

`--offline` reports core policy and whether an OS-level analyzer restriction is
actually active on this host, in addition to refusing every fetch.

> The exact check names are produced by the diagnostics service and are stable
> per build; treat the reason code, not the name, as the thing to match on.

## Recovery: what each code means and what to do

| Code | What happened | What to do |
|---|---|---|
| `CTX_WORKSPACE_NOT_FOUND` | No workspace was discovered from this directory upward. | Run `codectx init` at the repository root, or run from inside the repository. |
| `CTX_CONFIG_INVALID` | A configuration key is unknown, malformed or violates a cross-key rule. | The detail names the key. Fix it in the layer that set it; there is no extension namespace, so an unknown key is always a typo or a setting from another version. |
| `CTX_TRUST_REQUIRED` | A project file set a key only the user configuration may set, or raised a limit it may only lower. | Move the setting to your user configuration, or lower it. |
| `CTX_SCHEMA_MISMATCH` | The database on disk was written by a different schema. **This fails closed on purpose**; there is no migration layer. | Remove the workspace's data directory and re-index. Nothing in it is a source of truth: the repository is. |
| `CTX_STORAGE_CORRUPT` | An integrity check failed. | Run `codectx doctor --deep` for the detail, then remove the data directory and re-index. |
| `CTX_SOURCE_INTEGRITY` | A stored source block did not match its recorded hash. | Same as above: re-index. Do not keep serving from the store — retained bytes are what every answer cites. |
| `CTX_NO_ACTIVE_GENERATION` | The workspace is initialized but nothing has been published yet, or the last run failed before publication. | Run `codectx index`. A failed or unsealed unit is invisible by design, so a partial run leaves no half-visible state to clean up. |
| `CTX_WORKSPACE_BUSY` | Another process holds the workspace lock, or a live lease or retained session still references what was asked for. | **Retryable.** Wait and retry. The workspace lock is an advisory OS file lock, so it is released by the kernel if its holder dies — there is no stale lock file to remove by hand. |
| `CTX_DISK_FULL` | Free space is below `resources.min_free_disk_bytes`, a temporary budget was exhausted, or the filesystem refused a database write with less than one ingestion group free under the data directory. The message carries the engine's own result and the free space measured at the failure. | See "Disk pressure" below. |
| `CTX_RESOURCE_LIMIT` / `CTX_MINIMUM_BUDGET` | A bounded operation hit its ceiling, or the configured budgets cannot satisfy the minimum this build needs. | Narrow the request, or raise the relevant `[resources]` key. `CTX_MINIMUM_BUDGET` means the configuration itself does not hold together. |
| `CTX_PROVIDER_UNAVAILABLE` | A provider is not usable — not installed, unsupported platform, or refused as too old. | The detail names the provider and the reason. Either install or enable it, or disable it; the index is still published without it, with that capability reported degraded. |
| `CTX_BINARY_CONTENT` | A file is not text, so it has no lexical index. | Nothing to fix. The file is still retained and still served byte for byte; only searching inside it is meaningless. |
| `CTX_PROVIDER_TIMEOUT` / `CTX_PROVIDER_OUTPUT_INVALID` | An analyzer exceeded its bound, or produced output that failed validation. | Retry once; if it repeats, disable that provider and report it. Raw analyzer output is not in the ordinary log by design — request the private debug artifact if you need it. |
| `CTX_TOOL_OFFLINE` | A managed tool is missing and `tools.offline` is set. | Run `codectx tools prefetch` with fetching enabled, or install the tool and point an override at it. |
| `CTX_TOOL_DIGEST_MISMATCH` / `CTX_TOOL_CORRUPT` | A payload did not match the lock, or an installed tool failed verification. | Run `codectx tools gc` then `codectx tools verify`. A digest mismatch is never retried as if it were a network fault. |
| `CTX_TOOL_FETCH_FAILED` | The fetch itself failed. | **Retryable.** Check connectivity and proxy settings; note that proxy variables are honored but are not a security control. |
| `CTX_TOOL_UNSUPPORTED_PLATFORM` / `CTX_TOOL_OVERRIDE_INVALID` | The lock names no payload for this platform, or an override does not verify. | Supply a verified override, or accept the capability as unavailable. |
| `CTX_SESSION_EXPIRED` / `CTX_SESSION_SUPERSEDED` / `CTX_ACTOR_MISMATCH` | A context session is past its deadline, was replaced, or is being used by a different actor. | Open a new session. Receipts are never shared between sessions. |
| `CTX_CURSOR_INVALID` | A continuation token or a source receipt was malformed, tampered with, or older than `storage.query_cursor_ttl`. | Re-run the query from the first page. |
| `CTX_INTERNAL` | A composition or producer defect. | Report it with the command you ran. It is not an operator-fixable state. |

## Which commands write, and which never do

Four kinds of command, by what they may change:

| | Workspace lock | Database writes | Commands |
|---|---|---|---|
| **Indexing** | Held for the session | The run's own | `index`, `watch`, `init`, `tools prefetch`, `tools gc` |
| **Recording** | None | Session, receipt and manifest rows of its own | `context ...`, the coverage and workflow mutations, `gc`, `doctor --deep` |
| **Answering** | None | **None at all** | `status`, `search`, `symbol`, `refs`, `callers`, `callees`, `path`, `impact`, `repomap`, `doctor` |
| **Both** | Taken at the first refresh, then held | The refresh's own; its exploration tools write nothing | `mcp serve` |

`tools status` is in neither row: it opens the managed-tool store and no
database at all.

A command in the answering row opens the store without a writer connection. It
answers throughout another process's `index` or `watch` -- that is the promise
`status --help` makes -- and it delays that run by nothing, because a
write-ahead log reader never waits on a writer. It is also why such a command
never reports `CTX_WORKSPACE_BUSY`: it takes no lock and begins no write that
could queue behind one.

Two consequences an operator sees:

* A `search` answer from such a process is a **single page**. A continuation
  needs a cursor lease and a spool, which are writes, so the answer is served
  and marked truncated, with a reason naming the hits beyond it, rather than
  carrying a token the process could not honour.
* `symbol` pages in full from such a process. Its continuation carries its whole
  position in the token and retains nothing on disk, so it needs no lease: the
  generation it names is held by the read snapshot of whichever call presents
  it, and a generation collected in between is answered `CTX_CURSOR_INVALID`,
  re-run from the first page.
* A **graph walk** -- `refs`, `callers`, `callees`, `path`, `impact` -- answers
  one page and mints no continuation. Its continuation would retain a spool or a
  search-state directory, and what reclaims such a directory is the expiry of the
  cursor lease that names it: a lease is a write, and a directory adopted without
  one would sit on disk with nothing to reclaim it. So nothing is adopted at all,
  the page is served and the answer is marked truncated with a reason naming what
  lies beyond it. Narrow the walk with `--depth`, `--visited` or `--edges` to
  bring the answer inside one page.
* `doctor --deep` is in the recording row, not the answering one: the search
  index's own integrity check is spelled as an insert into the index, so a deep
  report needs the writer. It still takes no workspace lock.

### The one process that is both

The MCP server is the only process in both rows at once. `codectx mcp serve`
keeps the writer, because `codectx_refresh_index` publishes generations; and it
answers an agent's questions from the same process while that refresh is
writing.

Its startup takes no lock and performs no write, so a server starts and answers
beside an index that is already running in another terminal. The workspace lock
is taken at the first refresh and held for the rest of the session. A refresh
asked for while another process holds the workspace is refused
`CTX_WORKSPACE_BUSY` -- that one tool call, not the session: every
question-answering tool keeps answering throughout, and the watch loop, when it
is on, takes the lock on a later pass instead of ending the session.

It therefore opens **two handles on one database**: the writer, and a read-only
handle beside it, opened after the writer so there is a schema to verify by
reading. Every tool that only asks a question -- search, the repository
overview, symbol lookup, references, the graph walks, and the index status
report -- is served through the read-only handle. They pin no generation on the
writer, so a tool call no longer commits the session's own refresh mid-group nor
queues behind it; the run commits the same ingestion groups it would have
committed with nobody reading.

What an agent sees for it:

* The **paged** ones among them answer **one page** during a session, for the
  same reason the answering row does: a continuation needs a cursor lease and a
  spool, and both are writes.
* The status report is not paged: it is a single bounded answer, so
  `codectx_index_status` answers during the session's own refresh -- including
  what that session's watch covers and a retention sweep that did not finish,
  which the serving process knows in memory and reads from no handle at all.
* The session tools (`codectx_context_*`, `codectx_read_source`) still go
  through the writer: they record rows, which is what they are for. Called
  during the session's own refresh, they wait for the group the run has open.

## When a capability is not `fresh`

`status` publishes one row per provider capability, and a row that is not
`fresh` says in machine-readable `details` why. Read the state first:

| State | What it means |
|---|---|
| `fresh` | This generation holds the capability's facts and nothing degraded it. |
| `partial` | Some scope of this capability published facts into this generation and at least one other did not. The facts that are there are complete for the scopes that sealed. |
| `failed` | The capability was attempted and no scope of it published facts into this generation. It is never reported `unavailable`: that would leave the generation healthy over a provider that answers nothing. |
| `unavailable` | Nobody attempted it — no unit was planned (the tool is not installed, the platform has no payload, the provider is disabled), or its units are still deferred to background work. |

A `partial` or `failed` row carries the shape of the failure, not just its
code:

- `units_planned` — how many units the plan gave this provider behind this
  row. It is what makes the next figure readable: "two failed" is a different
  report depending on whether two or two hundred were tried.
- `units_failed` — how many of them failed.
- `failed_scopes` — the scope keys that failed, **at most eight**, the
  lexicographically first ones rather than the first to arrive, so two
  identical runs publish an identical row. When more than eight failed, this
  list is a sample and `details_truncated` names `failed_scopes` to say so;
  `units_failed` is still the full count.
- `scope_key` — the one exemplar scope whose reason the row's
  `diagnostic_code` and `failure_message` belong to, which is the first of
  `failed_scopes`.
- `failure_message` — that scope's safe message, so the row says what happened
  and not only which family it belongs to. The provider's own bounded
  particulars travel beside it — which profile, which tool, which project,
  what the run was declared under.
- `subdivided`, on a dependence capability, names a unit that crashed and was
  recovered by splitting; `backend_failure` beside it says what the crash was
  and how it was established to reproduce ([dependence](providers-dependence.md)).

Raw analyzer output is never in a capability row and never in an ordinary log
line. What an operator can read is, in order of how long it lasts:

1. The warning logged when the unit failed: the provider, the scope key, the
   generation, the diagnostic code, the message and the particulars — all of
   it except the tool's standard error.
2. The capability row above, for as long as the generation is active.
3. The failed provider run row in the workspace database, which keeps the
   whole typed reason including the bounded tail of what the tool wrote to its
   standard error, for exactly as long as the generation that failed is
   retained. **No command in this build prints that row**: the log line and the
   capability row are what you read a failure from.

A provider may contribute only a bounded number of details to one row. The
figures above take several of those slots, so a busy row can now carry
`details_omitted` — a count of the provider particulars that did not fit —
where the same row previously carried them all. The count is published rather
than the drop being silent.

## Retention, collection and the grace window

Nothing is deleted implicitly by a query. Reclamation happens in one **collection
pass**, run on the startup-recovery path and after a generation is activated.
The caller holds both the cross-process workspace lock and this process's
indexing mutex for the whole pass, so a second process cannot publish into the
window a sweep is examining. Every pass is batch-bounded: what does not finish
in one pass finishes in the next.

A pass does these things, in this order:

1. **Sessions.** Live sessions past their deadline are expired; closed sessions
   older than `storage.closed_session_retention` are pruned.
2. **Pagination spools.** Spool files whose lease has expired or whose
   continuation was consumed are removed. What the pass reports for this phase
   is a **byte** total, not a file count: the sweep reconciles the spool
   directory against its byte budget, so the figure is the live spool bytes it
   ended the pass with.
3. **Snapshot staging and content-store temporaries.** Crash leftovers under the
   data directory are removed. A published content object is never touched here.
4. **Tool store.** Staging directories no install owns, and installed versions
   the lock no longer names, are collected. A tool whose lock is held by a
   running install is skipped, not waited on.
5. **Generations and units.** Retention is **by ref**, governed by
   `index.retain_refs` and `index.max_retained_bytes`. A generation is refused
   collection while a live lease or a retained session references it — that
   refusal is the retryable `CTX_WORKSPACE_BUSY`, not a silent skip.
6. **Source blobs — the grace protocol.** See below.

### The blob grace protocol

Source bytes are the one thing this tool must never lose while something can
still cite them, so they are not deleted in the pass that finds them
unreferenced. A blob moves through states instead:

`ready` → `quarantined` → `trash` → *(grace window elapses)* → **rechecked** →
deleted.

A blob found unreferenced is quarantined, then trashed with the time it was
trashed. Only after the grace window has elapsed is reachability checked **a
second time**, and only a blob still unreferenced at that second check is
deleted. Anything that references the blob again in the meantime restores it to
`ready` in place, and the grace timestamp is cleared with it. Deletion order is
fixed: the block and line-checkpoint rows are removed only after the blob row
itself, never before, so a crash mid-delete can never leave a blob that claims
bytes it no longer has.

The grace window is `retention.blob_grace`, a bounded duration defaulting to
`24h`. It trades disk against the cost of losing bytes an in-flight answer still
cites; shortening it reclaims sooner and narrows that safety margin. A
non-positive value is refused by configuration validation, so the window can be
tuned but never switched off. It doubles as the cadence of the collector's CAS
orphan sweep: that sweep walks every bucket of the content-addressed store, so
it runs at most once per window instead of on every published generation. The
cadence delays the walk, it never bounds it -- each run reclaims every orphan
past the window, and a data directory whose last-sweep stamp is missing or
unreadable sweeps on the next pass.

### What is *not* collected automatically

- A language server's per-profile working directory under `<data_dir>/lsp/`
  survives a profile change. Remove it by hand if a profile's footprint matters.
- Files an analyzer child leaves outside its own run directory. On Windows a
  non-console child never receives the graceful stop signal (see
  [the threat model](threat-model.md)), so it never gets the chance to clean up
  after itself; the pass above reclaims the run directory regardless.

## Disk pressure

Free space is checked against `resources.min_free_disk_bytes` (default
`1073741824`). Below it, indexing pauses and writes return a typed
`CTX_DISK_FULL` rather than a partial store. A database write the
filesystem refuses is settled against the disk at that moment: the engine
reports a full disk and a failing device with the same result codes, so the
store measures the free space under the data directory and reports
`CTX_DISK_FULL` only when less than one ingestion group is free; with more
free the disk is not full, and the error is `CTX_INTERNAL` carrying the
engine's message and code and the measured figure, with the kernel log as the
place to look for the device's refusal. **Disk pressure never evicts the
source an open session is reading** — degrading an answer is not an acceptable
way to free space.

Temporary bytes across materializations and spools are bounded by
`resources.max_temp_bytes`, which is **unlimited by default** (`0`): nothing
refuses a walk, a materialization or a continuation until you set it. A value
you DO set must exceed `resources.min_free_disk_bytes`, and it refuses a run up
front with `CTX_RESOURCE_LIMIT` naming the key. `min_free_disk_bytes` is
enforced against actual free space either way -- it protects the host's space
rather than capping work.

Most of the disk a workspace holds is not leftovers: it is the working files
the store keeps and writes over instead of freeing, because freeing is the
expensive act on this class of host
([storage](storage.md#the-scratch-pool)). `codectx status --resources` reports
it as `scratch_bytes`, and it only ever grows towards the workspace's working
set.

In order, when you are short on space:

1. `codectx doctor` — the free-space and orphan-temporary checks say whether the
   space is being consumed by the store or by leftovers.
2. `codectx gc` — empties the workspace's scratch pools. It prints what each
   pool holds, by what its surfaces were taken for, before it frees anything,
   so you can judge the space before giving it back. There is no timer and no
   threshold: this is the only thing that shrinks `scratch_bytes`.

   What it leaves it names. A pool instance a running process owns is skipped
   whole -- emptying it would take that run's working files out from under it
   -- and appears as `left_alone` with what it holds and why; a queued removal
   the filesystem refused appears as `stuck_frees` with the reason. Together
   they are why freed can fall short of held without the request having
   failed.

   The space is given back a window at a time, with a sync and a wait between
   windows, which is slow on purpose. Freeing a large amount at once leaves a
   virtualized host owing work it does not report, and about a minute later
   every disk request on the machine waits for it. Even paced, a very large
   collection can stall such a host for about a minute; the command says so.
3. `codectx tools gc` — reclaims tool-store staging and versions the lock no
   longer names.
4. Lower `index.retain_refs`, or set `index.max_retained_bytes`, and let the
   next collection pass evict least-recently-used refs. The active ref is never
   evicted.
5. Wait out the grace window, or re-run after it elapses: trashed blobs are not
   reclaimed before their second reachability check.

## Rebuilding a workspace

There is no migration layer and no repair tool, because there is nothing in the
data directory that the repository cannot produce again. A rebuild is always
safe and is the correct answer to `CTX_SCHEMA_MISMATCH`, `CTX_STORAGE_CORRUPT`
and `CTX_SOURCE_INTEGRITY`:

1. Stop anything holding the workspace — a `codectx watch` or an MCP server.
2. Remove the workspace's data directory — `storage.data_dir`, whose empty
   default resolves to a user-private per-workspace directory documented in
   [configuration](configuration.md).
3. `codectx init`, then `codectx index`.
4. `codectx doctor --deep` to confirm.

Open sessions, cursors and receipts do not survive a rebuild. That is correct:
they cite a generation that no longer exists.

## Unavailable metrics and unsupported platforms

`codectx status --resources` reports the resource accounting block. **A metric
that cannot be measured is absent, never zero.** The two surfaces express that
differently, and both are load-bearing:

- In the resource block, an unmeasured field is a **null pointer, omitted from
  the JSON entirely**. A field that *is* present with the value `0` is a real
  measurement of zero.
- In `doctor`, an unmeasured check has `state: "unavailable"`. It is not `pass`
  and it is not `fail`. A check that was skipped because you did not pass
  `--deep` has `state: "unverified"` -- also not `pass`.

`status` also reports a completeness table: the capability state of each
provider that produced the sealed units of the active generation. The
query-time working-tree overlay has **no row in it, by design** -- it seals
nothing and reports its state per query instead; [the overlay
page](providers-lsp.md) says what to read ahead of a query.

If you see `0` where you expected a figure, it is a measurement of zero — file
it as a bug rather than assuming the platform does not support it.

The columns below describe the **platform capability each metric family depends
on**, not a per-build inventory. A family whose reader is absent — because the
platform has none, or because the measurement is not wired in this build —
reports `unavailable`. It never reports `0`.

| Metric family | linux | darwin | windows | Why |
|---|---|---|---|---|
| Parent process RSS, current and peak | measured | measured | measured | Read from the OS process interface. |
| Go-managed bytes | measured | measured | measured | Reported by the Go runtime. |
| **Process-tree peak, sampled concurrently** | **measured** | **`unavailable`** | **`unavailable`** | Sampling walks the live process group's per-process memory while the tree runs. Only the Linux build has that reader; on every other platform the runner sets its tree-unsampled flag and the figure is omitted rather than under-reported as the parent's alone. |
| **Native worker and per-engine memory** | **measured** | **`unavailable`** | **`unavailable`** | Same reader as the tree peak: a worker's memory is a member of the sampled tree. |
| Live subprocesses | platform-independent | platform-independent | platform-independent | Counted in-process by the runner as children start and are reaped, never sampled from the OS, so no platform lacks a reader for it. |
| Database, WAL, temporary and content-store bytes | measured | measured | measured | Filesystem sizes. |
| Free disk bytes | measured, else `unavailable` | measured, else `unavailable` | measured, else `unavailable` | Reported by the OS for the data directory's filesystem. A filesystem that refuses to answer yields `unavailable`, not `0`. |
| Unit reuse and parse counts | `unavailable` | `unavailable` | `unavailable` | Counted per indexing run and reported on that run's result, where `codectx index` prints them. They are **not** process-wide totals, and the resource block reports no figure rather than reporting `0` for a process that has indexed nothing this run. No platform differs. |
| Pending watch events | measured while a watch is live, else `unavailable` | measured while a watch is live, else `unavailable` | measured while a watch is live, else `unavailable` | A running watch (`codectx watch`, or `codectx index --watch`) publishes a heartbeat row carrying its pending-event count and **its own** expiry, refreshed while it runs and withdrawn when it stops. A second process reports the figure while that row is live. Once it expires — the watch was killed, or the machine went down — the figure is `unavailable` again rather than the dead writer's last count, which would read as "the watch is caught up"; `doctor`'s `watch_heartbeat` check is what says a watch stopped. A watch running without filesystem notifications has no queue to count and publishes no figure. |
| Freed, pending-free and scratch bytes | platform-independent | platform-independent | platform-independent | Counted in the process, not read from the host. `freed_bytes` is one byte-exact counter of everything this process has actually given back -- its own removals, the reclaimer's, and the file-system shim's shortening of a file the engine owns -- and `freed_by_purpose` is the labelled part of that same counter, so the two cannot disagree. `pending_free_bytes` is what a removal has renamed aside and the reclaimer has not released yet; it is neither of the other two while it waits. `scratch_bytes` is the disk the pools hold, summed over every pool this process has opened, and it is absent rather than short if a pool cannot be read. Only `scratch_bytes` reads the disk, and only it can be `unavailable`. `stuck_frees` names the queued removals the filesystem refused, with the reason, so `pending_free_bytes` that never falls is never left unexplained. |
| Query, cache and queue reservations | platform-independent | platform-independent | platform-independent | Derived from the resolved `[resources]` configuration, so they are what was reserved, not what was touched, and no platform differs. |
| Per-unit analyzer memory (`analyzer_units`) | measured | reservation and caps only | reservation and caps only | One row per heavy analysis unit this process ran: the reservation it was admitted against, the heap caps its two steps ran under, the machine-derived allocation those caps were bounded by, and the peak its process tree reached. The first three are the governor's own arithmetic and are always present. `allocation_bytes` is absent where the host does not publish available memory, and `observed_peak_bytes` is absent wherever the tree sampler is — the same reader as the process-tree peak above. Reading the row's figures together is the point: a peak far under the cap says the unit was serialized behind memory it never used. It is process accounting and not per generation, so it covers what **this** process has run. |
| Hard OS memory enforcement | partial | none | partial | Enforcement uses cgroup and job controls where they exist. Where they do not, codectx **admits and monitors only**, and says so rather than implying a limit it cannot enforce. |

Two limitations of the Linux sampler itself, which the figures above do not
otherwise disclose. Membership in a sample is decided by **parent chain**, not by
process group: a descendant that is re-parented away after its own parent exits
leaves the sampled set, so a peak taken across such an exit can under-report. And
a sweep that reaches its bounded process ceiling returns the base-worker and
native figures **both absent** rather than a smaller sum — a truncated sweep can
drop an intermediate ancestor and hide every descendant below it, and a partial
sum presented as a total is the misreport this rule exists to prevent.

The practical consequence on macOS and Windows: an analyzer's memory is bounded
by admission control and by the runner's own limits, and its *observed* peak is
reported as unavailable. Use the Linux build when you need the measurement.
