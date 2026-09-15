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
