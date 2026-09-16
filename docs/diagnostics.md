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

## The run ledger over MCP

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
  logging level at that severity or lower -- on the call itself, beside the
  progress token, or for a session that keeps one, session-wide. One message
  per finished stage, carrying the stage row itself as structured `data` in the
  shape above, so an agent watching the stream reads the figures rather than
  parsing a line.

Both are sent from the server's own goroutine for that call, never from the one
recording the run: a client that is slow to read, or gone, costs its own
notifications and never the run's speed.
