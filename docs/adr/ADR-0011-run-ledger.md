# ADR-0011: The run ledger — a run records what each stage cost, in its own database, read live

## Status

Accepted, 2026-09-16.

## Context

`codectx status --resources` answers memory questions and only memory questions: parent and worker
resident set size, the three reservations, the store and write-ahead-log sizes, what the run has
freed, and each heavy unit's reservation, cap and observed peak
([diagnostics](../diagnostics.md), [ADR-0010](ADR-0010-engine-memory.md)). Nothing in the product
answers the question that actually decides where a run is slow or heavy: what each stage cost.

The cost of that gap is on record. The time breakdown of the reference-repository run -- eleven
seconds of startup, five minutes nine seconds of structural parse, thirteen minutes forty-one
seconds in a single analysis unit out of a twenty-six minute fifty-seven second run -- was
reconstructed by hand from log timestamps. A person can do that with a terminal and patience. An
agent driving the product over MCP cannot, and neither can a script, and neither can the product
itself when asked why a run is taking a long time *while it is still taking it*.

Six stages already log a duration, each in its own shape and none of them aggregated:
`internal/storage/sqlite/lexicalbuild.go:209`, `internal/storage/sqlite/graphbuild.go:253`,
`internal/storage/sqlite/lexicalmerge.go:140`, `internal/snapshot/builder.go:289` and
`internal/provider/dependence/provider.go:672` and `:763`. Three of the measurements the ledger
needs are already taken and thrown away: the child process tree's peak resident memory is sampled
every 250 ms by `internal/process/treesample_linux.go:36` and reaches only the dependence
provider's memory governor; the reaped child's `rusage` is reachable at
`internal/process/runner.go:539` and `:739` and is read by nothing at all; the tree's summed CPU
ticks are kept by the same sampler and consumed only as a liveness signal by the stall watchdog.

## Decision

### 1. A run is a tree of spans, not a list of timers

The model is the span model as tracing has settled on it -- name, parent, start, end, attributes,
status ([OpenTelemetry tracing specification](https://opentelemetry.io/docs/specs/otel/trace/api/)).
One `run` root; under it one span per stage; under a stage one span per unit; under a subdivided
unit one span per part. A share of the run's wall time is arithmetic over that tree, so it is
computed when the ledger is read and never stored.

Every span carries the same measured fields, and a field the platform cannot give is null, never
zero -- the rule the resources block already keeps, that an unavailable metric is recorded as
unavailable:

| field | source |
|---|---|
| `stage`, `scope_key`, `provider`, `parent` | the caller |
| `started_at`, `finished_at`, `wall_ms` | the monotonic clock at each end |
| `cpu_user_ms`, `cpu_sys_ms` | a child: its `rusage` at exit, which the runner already holds; an in-process stage that runs alone in the process: a `RUSAGE_SELF` delta; an in-process stage overlapping other goroutine work: null, with `cpu_unattributed = overlapped` |
| `peak_rss_bytes` | a child tree: the sampler the runner already starts per child; the process as a whole: on the run row only |
| `read_bytes`, `write_bytes` | the per-process I/O counters the platform exposes, summed over the group by the same sampler; for a child, the last sample before exit |
| `items_in`, `items_out` | counters the stage already keeps -- files walked, files parsed, records emitted, rows staged |
| `outcome`, `diagnostic_code`, `failure` | the typed error's message and the details the failing provider retained |

Go has no per-goroutine CPU accounting. A stage that runs concurrently with other work therefore
reports no CPU rather than a number derived from process-wide counters it does not own: a
plausible wrong attribution is worse than an honest absence, because only the first one gets
believed.

**Per-file work is never a span.** A parser worker's span aggregates its files. A span is a thing
that costs seconds; a span per file would make the ledger larger than the facts it describes.

### 2. The run records through a bus and one collector, never on its own goroutine

A stage records with two calls -- open a span, end it with an outcome and counts -- and the parent
is found in the context, so a provider's inner stages nest under the unit span the coordinator
opened without any provider knowing the ledger's shape. Each call does a **non-blocking send** on
a bounded channel. A full channel drops the event and counts it on the run row. The run never
waits on its own accounting: an accounting system that can stall the thing it measures is a defect
disguised as a feature.

A running span exposes atomic counters the stage increments as it goes, so progress costs one
atomic add per item and no channel traffic at all.

A run's writer states on the run row the deadline it undertakes to renew while it lives, and
renews it on the flush the collector already performs. That is the only way a second process can
tell a run still being written from one whose process died: a row that says it is running says
nothing about whether anyone is still writing it, and probing a process id is neither portable nor
free of races. The same question is already answered this way for the watch heartbeat, and a second
mechanism for it would be one too many.

**One collector goroutine** owns the store side, batching and flushing on a short interval or a
batch bound, whichever comes first, and snapshotting the counters of every running span so that a
reader in another process sees progress. A crash loses at most one flush interval of rows, and a
span still unfinished after its run ended reads as `interrupted` rather than as a span that never
happened.

The collector is also the one source of the log lines and the MCP notifications, so every
subscriber sees the same event. The six existing per-stage duration log lines become spans and
their bespoke lines are removed.

### 3. The ledger is its own database file beside the store

Two facts about the main store force this, and both are properties of decisions already taken.

The ingestion group holds one transaction open for most of a run and commits when its page cache
would spill or an exclusive writer waits ([ADR-0008](ADR-0008-ingestion-group.md),
`internal/storage/sqlite/open.go:488`). Rows written through it are invisible to a second process
until that commit, so a ledger inside it could not answer a question about a run in progress --
which is half of the requirement.

`Store.Activate` (`internal/storage/sqlite/units.go:1331`) holds an exclusive transaction around
lexical compaction, the adjacency build and the lexical build (`:1386`, `:1492`, `:1498`). Any
other writer on that file waits or fails busy for its duration, which is exactly the window an
operator most wants the ledger to be answering in.

There is a third reason that is not about contention. The main schema's text is hashed into a
fingerprint (`internal/storage/sqlite/schema.go:18`) that is folded into every analysis key
(`units.go:1503`), so adding a table there re-keys every analysis unit in the product and
invalidates every existing cache. A diagnostic table is not worth that, and never will be.

So the ledger is its own file beside the store, with its own schema and fingerprint, its own
write-ahead log, one writer (the collector) and any number of read-only readers at any moment. It
is found where the store is found, lives under the same directory lock, and its rows follow the
generation they describe out of existence when retention deletes it.

### 4. The same rows answer on four surfaces

1. **`codectx index`**: one progressive line per finished top-level span through the existing
   output path (`internal/cli/index.go:469`), and on completion the run row and the top stages by
   wall time with their share. Under `--json`, the same rows in the one envelope.
2. **`codectx status --resources`**: the ledger of the latest run for this repository -- the live
   one if a run is live, live meaning its writer has renewed the deadline it publishes on the run
   row and not merely that the row still says running, otherwise the one that produced the active
   generation. A run whose writer died without stopping its ledger reads as interrupted, with its
   still-open spans interrupted and no wall nobody measured. The rows are shown as a table sorted
   by wall time with the run row first. **`--follow`** re-renders on an interval until
   interrupted, which is the live view from a second terminal, and costs the run nothing because it
   only reads.
3. **MCP**: `codectx_index_status` with `resources=true` returns the same rows, because
   `model.IndexStatus` is the one model and a second shape would be a second thing to keep true.
   `codectx_refresh_index` sends progress notifications when the client passed a progress token,
   rate-limited and always increasing as the protocol requires and stopped at completion, and a
   log notification per finished span when the client set a logging level. Both come from a
   collector subscriber, never from the handler's goroutine.
4. **The structured log**: one line per finished span, which is what a log shipper or a proof
   recorder reads.

### 5. Nothing here is a setting

There is no flag to turn the ledger on, no sampling rate and no verbosity level. A run that cannot
say what it cost is not a diagnosable run, and a diagnostic that must be enabled before the problem
happens is a diagnostic that is off when the problem happens. The cost is therefore measured rather
than assumed, and a cost above the noise of a run is a defect to fix, not a knob to add.

## Alternatives considered

- **A table in the main store.** Steel-manned: one database, one retention path, no second file to
  find. Rejected on the two facts in decision 3 -- it is invisible while a run is in progress and
  blocked during activation, which are precisely the two moments the ledger exists for -- and on
  the analysis-key re-keying that any main-schema edit causes.
- **Writing rows from the stage's own goroutine.** Simpler: no channel, no collector, no dropped
  events. Rejected because it couples every stage to store write latency, so one slow flush stalls
  a parser, and the measurement changes what it measures.
- **A metrics file per run, written as lines beside the store.** Rejected: a second artefact to
  find, retain and collect, with none of the generation lifecycle the store already has.
- **Sampling the process periodically instead of bracketing stages.** Rejected: a sampled timeline
  says what the process was doing, not what a stage cost, and attribution is the whole question.
  The sampler is kept for peaks, where sampling is the right instrument.
- **A tracing or metrics library.** Steel-manned hardest: the span model here is borrowed from one,
  and the libraries are mature. Rejected because the product exports nothing over a network, the
  span is a twenty-field struct, and the dependency would be larger than the feature it serves --
  the same reasoning [ADR-0001](ADR-0001-scale-posture.md) applies to bounded-by-page scale.
- **A progress bar during `codectx index`.** Rejected: the total is unknown until planning has run
  and the stages are heterogeneous, so a bar would be a lie for the first stage and a poor summary
  afterwards. A line per span with its counts says more and pipes cleanly.

## Consequences

An operator and an agent can both ask what a run cost, and can ask while it is still running. The
reconstruction-from-timestamps that produced the reference breakdown becomes a query. The retained
failure on a span row is the one place a failed stage's reason survives the run, which is what a
later diagnosis reads.

The accepted trade-offs: a second database file exists in the workspace directory and must be
retained and collected with the generations it describes; a run that produces more events than the
channel bound drops some and says so rather than slowing down; a stage whose CPU cannot be
attributed reports none, so the CPU column has holes exactly where concurrency is; and the span
tree is a public shape that the CLI, the MCP tool and the log all depend on, so changing it changes
all four at once.

## Sources

- [OpenTelemetry tracing API specification](https://opentelemetry.io/docs/specs/otel/trace/api/)
  (the span model: name, parent, start, end, attributes, status).
- [Model Context Protocol specification](https://modelcontextprotocol.io/specification)
  (progress notifications must increase and stop at completion; logging notifications follow a set
  level).
- [ADR-0008](ADR-0008-ingestion-group.md) (the ingestion group's single long transaction, the fact
  that makes a table in the main store invisible while a run is in progress).
- [ADR-0010](ADR-0010-engine-memory.md) (the per-unit memory accounting the ledger's peak column
  sits beside, and the disclosure principle it established).
- [ADR-0001](ADR-0001-scale-posture.md) (bounded by page, unlimited by default: the posture the
  bounded event channel and the bounded read page follow).
- [diagnostics](../diagnostics.md) (the surfaces that answered memory alone before this record).
- Measurement record: the reference-repository run of 2026-09-16, whose hand-reconstructed time
  breakdown is quoted in the context above.
