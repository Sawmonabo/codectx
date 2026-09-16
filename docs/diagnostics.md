# Diagnostics

`codectx doctor`, `codectx status --resources` and the structured logs answer
most questions about a run. This page covers the one hook that is deliberately
not a flag: process profiling.

## Profiling a run

Set `CODECTX_PPROF_DIR` to a directory and every `codectx` process in the run
writes Go profiles into it:

```
CODECTX_PPROF_DIR=/tmp/ctxprof codectx index
go tool pprof -top /tmp/ctxprof/main-*.cpu.pprof
```

Three profiles are written per process, named `<role>-<pid>.<kind>.pprof`:

| File | What it holds |
|---|---|
| `main-<pid>.cpu.pprof` | CPU samples for the whole command |
| `main-<pid>.allocs.pprof` | every allocation the run made |
| `main-<pid>.heap.pprof` | live heap after a final collection |

It is an environment variable rather than a flag for two reasons. A run that is
already misbehaving must be profileable without changing the command line the
operator or their wrapper issues. And the tree-sitter parser workers take no
arguments of their own -- they are re-executions of this binary under a hidden
subcommand -- so a variable is the only channel that reaches them. A worker
writes the same three files under the role `worker`, which is what separates
parent cost from parse cost; `CODECTX_PPROF_DIR` is the **only** variable a
parser worker inherits, and it inherits nothing at all when the variable is
unset.

Profiling never fails a run: an unwritable directory is reported on stderr and
the command proceeds unprofiled.

## The run ledger

Every index run records what each stage of it cost, in its own database beside
the store ([storage](storage.md#the-run-ledger-beside-the-store),
[ADR-0011](adr/ADR-0011-run-ledger.md)). There is nothing to turn on: no flag,
no sampling rate, no verbosity level. A diagnostic that has to be enabled
before the problem happens is off when the problem happens.

### What a span is, and what it is not

A run is a tree of **spans**, not a list of timers. One run at the root; under
it one span per stage; under a stage one span per unit; under a subdivided
unit one span per part. Each span carries its own wall time, processor time,
peak resident memory, transferred bytes, what went in and what came out, and
how it ended.

**Per-file work is never a span.** A parser worker's span covers every file
that worker parsed; an analyzer unit's span covers the unit. A span is a thing
that costs seconds, and a span per file would make the account larger than
what it accounts for.

A run is one of three kinds. An `index` run is a `codectx index`, `refresh` or
`watch` pass. A `deferred` run is one tick of the deferred publication that
finishes work an earlier run left staged; each tick is its own run. An
`overlay` run belongs to the process rather than to any generation, and holds
the spans of things a process does outside a run -- today, starting a language
server.

### The stages a run records

An index run opens these at its top level: `capture` (with `walk` beneath it),
`plan`, `attach_reused`, `attach_carried`, `build`, `structural_parse` (the
run's total for the parser pool), `coverage`, `activation`,
`lexical_compaction`, `adjacency`, `lexical_build`, `retention`, `collection`
and `reclaim`.

Under `build`, one span per planned unit, whose stage is the **provider's own
id** -- `filesystem`, `treesitter`, `manifest` and the rest -- and whose scope
is the unit's scope key. Beneath a unit span sit the steps that provider
takes: `run` and `import` for a precisely indexed unit; `parse`, `export` and
`import` for an analyzed one, with a `part` span per piece of a unit that was
subdivided and that part's own steps beneath it; and `seal`, where the unit's
facts become visible. Beneath the `structural_parse` total sits one span per
parser worker, each carrying that worker's child process measurements.

`server_start`, one per language server, sits under the process's `overlay`
run rather than under an index run: a server is started lazily, by whatever
needed it, and no generation owns it.

Two figures are stages rather than brackets. `reclaim` is a single span at the
run's end whose `out` count is the bytes **this process** freed while the run
was open -- not what this run's own removals cost, because one paced reclaimer
serves the process and a removal a run queues may be freed after it ends. A
bracket around the whole run would have the run's own wall and would top every
wall-sorted table while measuring nothing.

### How each stage ends

`ok`, `failed`, `subdivided`, `reused` and `skipped` are endings the run
reported. Three are not:

- `running` is a span that had not finished when the row was read.
- `interrupted` is a span that was still open when its run ended -- a
  cancelled run, or a process that died. It has no finish time and no wall,
  because nobody measured one.
- `unavailable` is a unit the plan named that reached no output. Its
  `diagnostic_code` and reason say which of three things happened: no profile
  matched it and there was nothing to run; the tool it needs is absent
  (offline, unsupported platform, a failed or mismatched fetch); or it was
  planned and never reached admission, because the run ended or was refused
  first.

A unit the plan named and nothing has started yet is `planned`. A row still
`planned` when its run ends is swept to `unavailable` with the
never-admitted reason: a run accounts for every unit it planned, including the
ones it never got to.

### Reading the columns honestly

**The processor-time column has holes, and they are deliberate.** Go has no
per-goroutine processor accounting. A stage that ran beside other work in this
process therefore reports no processor time and says
`unavailable (overlapped)` rather than a share of process-wide counters it
does not own -- `seal`, `activation`, `adjacency`, `lexical_build`,
`lexical_compaction`, the in-process `import` steps and `reclaim` all read this
way. A plausible wrong attribution is worse than an honest absence, because
only the first one gets believed. `unavailable (unsampled)` is a different
answer: the platform does not expose the counters at all. A span that ran a
child process has real processor time, taken from the child when it was
reaped.

**A child's transferred bytes are a sample, not a total.** They come from the
per-process counters of every process in the child's group, swept every 250 ms,
and the figure kept is **the last sweep that still found the tree** -- an
exiting process takes its counters with it, so there is no exit-time total to
read. They count bytes the process asked the kernel for, so a pipe write and a
page-cache write both count and the figure is not disk volume. The same sweep
supplies a child span's peak resident memory. Both figures reach `--json`, the
MCP row and the structured log line; the table below carries the eight columns
shown there and not these two, because a table wide enough for them stops
fitting a terminal.

**The run row's peak is the process's peak, not the run's.** It is the kernel's
own high-water mark for this process, read at the run's end, so it covers every
run this process has already served: in a one-shot command the two are the same
figure, and in a long-lived server on its fourth refresh the mark may have been
set by the first. The stage rows carry no peak of their own for the reason
above, and the run row's `cpu` stays `unavailable` because nothing measures
processor time for the process as a whole.

**An interrupted run shows no wall.** A run states on its own row a deadline
its writer promises to renew while it lives. A reader that finds the deadline
elapsed knows the process writing that run stopped -- the only signal of
process death that works on every platform, since a process id can be reused
and probing one is neither portable nor race-free. Such a run reads
`interrupted`, and its wall, and every stage's share of it, render
`unavailable` rather than `0s` and `0%`: nobody measured when that run ended,
and inventing a finish time would present the run an operator is investigating
as one that took no time. Its still-open spans read `interrupted` for the same
reason.

**`unavailable` is never zero** anywhere in the account, on any column. A
figure nothing measured is absent.

Two lines qualify the rows above them where they appear. `incomplete` says how
many accounting events the bounded event bus refused because the collector was
behind: a run never waits on its own accounting, so the loss is counted rather
than prevented, and above zero the stages listed are not the whole run.
`omitted` says how many further stages the run recorded beyond the page being
shown.

### The surfaces

**`codectx index`** prints one line per finished **top-level** stage as the run
goes, and closes with the run's own row and its top-level stages ordered by
wall time with each one's share:

```
stage       capture 6ms, ok, in 0, out 3
run         ok in 21ms, 8 planned, 8 succeeded, 0 failed, 0 subdivided
stage       capture 6ms, 29% of the run, ok, in 0, out 3
stage       activation 5ms, 24% of the run, ok, in 0, out 0
```

The progressive lines are the run's own; a deferred tick and a `watch` pass
print none, and under `--json` they go to stderr so that stdout carries the one
envelope and nothing else. `--json` carries the same rows on the result
itself, at `data.run` and `data.stages`, with `data.stages_omitted`. Because only
top-level stages are printed, a unit that ended `unavailable` is not in this
block -- read it from `status --resources` or its JSON.

**`codectx status --resources`** prints the whole tree of the latest run for
this repository as a table, the run's own row first and the stages under it
ordered by wall time, with each stage's reason on its own line beneath it:

```
run
  stage             scope         wall        cpu          peak             in   out  outcome
  run               generation 7  12s         unavailable  431497216 bytes  120  2    ok
  walk              -             8s          unavailable  unavailable      120  118  ok
  structural_parse  -             running 3s  unavailable  unavailable      40   0    running
  seal              -             1s          unavailable  unavailable      0    0    failed (CTX_UNIT_FAILED)
    seal failed     the unit did not seal
```

"Latest" means the run that is live if one is -- live by its writer still
renewing the deadline on its row, not merely by the row saying `running` --
and otherwise the run that produced the active generation. The report opens
the ledger read-only, so it reads while a run writes, costs that run nothing
and never creates the file where no run has ever been recorded.

**`--follow`** re-renders the whole report every second until it is
interrupted, which is the live view from a second terminal. With `--json` it
emits one complete envelope per second; each is a whole snapshot, never a
delta, which is what a script tails.

**The structured log** carries one `level=INFO msg="stage finished"` line per
finished stage on stderr, with `component=ledger` and the row's fields as
attributes:

```
level=INFO msg="stage finished" component=ledger run_id=… stage=treesitter seq=7 wall_ms=31402 items_in=812 items_out=812 outcome=ok scope_key=go:root provider=treesitter cpu_user_ms=4100 peak_rss_bytes=268435456 read_bytes=1073741824 write_bytes=268435456
```

An attribute nothing measured is absent rather than zero, `wall_ms` included:
a stage with no `wall_ms` never finished. This is what a log shipper or a
proof recorder reads.

### The run ledger over MCP

`codectx_index_status` with `resources: true` answers with the same run the CLI
reads -- one model, not a projection of it. The rows arrive at
**`data.resources.run`** and **`data.resources.stages`**: the tool envelope
carries the status object directly in `data`, one level shallower than the
CLI's `data.index...`, so a client that assumes the command's path finds
nothing there.

`data.resources.run` is the latest run for this repository: the live one while a
run is going, otherwise the one that produced the active generation. It carries
`run_id`, `kind`, `generation_id` (absent on a run that never reached one),
`started_at`, `finished_at` (absent while it is going), `wall_ms`, `outcome`,
the file and unit counts, and `events_dropped` -- above zero, the stage rows are
known to be incomplete. `data.resources.stages` is that run's stages, each with
`seq` and `parent_seq` (absent at the top level), `stage`, `scope_key`,
`provider`, `wall_ms` with `running` beside it, `cpu_user_ms`/`cpu_sys_ms` or
`cpu_unattributed` saying why they are absent, `peak_rss_bytes`,
`read_bytes`/`write_bytes`, `items_in`/`items_out`, `outcome`,
`diagnostic_code`, `failure` and `share_of_wall`. Every measured field is absent
rather than zero where nothing measured it.

`codectx_refresh_index` reports the run it is making as it goes, from the same
finished-stage rows:

- **`notifications/progress`**, only when the call carried a `progressToken`.
  `progress` is the number of the run's top-level stages that have finished, so
  it strictly increases and never counts a unit's inner steps; `message` names
  the stage that just finished with its outcome, its wall and its counts. At
  most one notification a second: a stage that finishes inside that second is
  counted by the next notification's value, never dropped from it. `total` is
  absent -- how many stages a run will open is not known to a stage that has
  finished, and a total that is a guess is worse than none. Nothing is sent
  after the call answers, and a call that passed no token is sent nothing at
  all.
- **`notifications/message`** at `info`, only when the client has asked for a
  logging level at that severity or lower. A call carries its own level in the
  metadata beside the progress token, and that level **replaces** any the
  session set earlier, so a client that sets a level once for the session and
  then makes calls that carry none is sent nothing. One message
  per finished stage, carrying the stage row itself as structured `data` in the
  shape above, so an agent watching the stream reads the figures rather than
  parsing a line.

Both are sent from the server's own goroutine for that call, never from the one
recording the run: a client that is slow to read, or gone, costs its own
notifications and never the run's speed.
