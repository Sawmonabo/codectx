# Graph queries — `refs`, `callers`, `callees`, `path`, `impact`, and the `repo-map`

The five graph commands answer bounded structural questions about one pinned
generation; `repo-map`, documented at the end, reads the same pinned generation
through the same paging machinery. They read; none of them takes the workspace
lock, so they answer while another process is indexing or watching. Every one of
them produces an answer that either is exhaustive or says, in the same breath,
that it is not.

Each command accepts `--json`, which emits exactly one envelope on stdout and
nothing else. The human rendering carries the same binding, the same capability
rows and the same warnings, so neither consumer learns less than the other.

## Arguments: `<name-or-id>`

Every positional argument is either a **resolved node id** — 64 lowercase hex
characters, as printed by any answer that returned the node — or a **symbol
name**.

A name is resolved against the pinned generation: the exact `name` first, the
exact `qualified_name` second. A name that no node carries, and a name that
several nodes carry, are both `CTX_ARGUMENT_INVALID`; the ambiguous case names
up to sixteen candidate ids in its remediation, printed under the error on the
text path and carried in the `--json` envelope, so the caller can pick one. Nothing is ever resolved to the first
candidate silently — the candidates are different symbols, and answering about
the wrong one is a wrong answer the caller cannot detect.

## The commands

| Command | Answers | Walks |
|---|---|---|
| `refs <name-or-id>` | the sealed reference occurrences of one node | incoming `references` and `calls` |
| `callers <name-or-id>...` | who reaches these symbols | incoming `calls` |
| `callees <name-or-id>...` | what these symbols reach | outgoing `calls` |
| `path <from> <to>` | the cheapest dependency routes between two nodes | the default relation allowlist |
| `impact <name-or-id>...` | what a change to these symbols may affect, ranked | both directions over the impact allowlist |

`callers` and `callees` pin their direction and their relation; no flag can
contradict them. `path` uses fixed integer edge costs, so the same two nodes in
the same generation always produce the same routes in the same order. `impact`
reports a per-entry direction — **incoming** is *may need modification*,
**outgoing** is *may need reading* — and every entry carries at least one reason
naming the relation kind and direction that made it affected. `impact` also
carries the package rollup that summarises it: distinct package-to-package pairs
with their evidence counts, which is a summary and never a precise symbol call.

`refs` separates the two counts Section 9.2 requires: a symbol referenced twice
inside one canonical relation is **one relation and two occurrences**. Each item
is one occurrence and carries the precision class, file and byte range of the
evidence row behind it.

## Flags

| Flag | Commands | Meaning |
|---|---|---|
| `--repo` | all | Workspace root to answer from. |
| `--generation` | all | Answer from this generation instead of the active one. On every command but `path`, which issues no continuation, it is not combinable with `--cursor`. |
| `--timeout` | all | Deadline for this invocation. Zero leaves the configured query deadline in charge. |
| `--limit` | all but `path` | Items in one page. |
| `--cursor` | all but `path` | Continue a previous page. A cursor is bound to its endpoint, generation, analysis key and query; presenting it to a different query is `CTX_CURSOR_INVALID`. |
| `--depth` | `callers`, `callees`, `path`, `impact` | Maximum hops from the nearest start node. |
| `--visited` | `callers`, `callees`, `path`, `impact` | Nodes one page may admit. On `callers` and `callees` it is a **per-page work budget**: a page that spends it ends there and hands back a cursor. On `impact` and `path`, which walk once, spending it truncates that walk. |
| `--edges` | `callers`, `callees`, `impact` | Relations one page may admit. On `callers` and `callees` this is a per-page budget on the same terms as `--visited`; on `impact`, which walks once, spending it truncates that walk. |

**`path` issues no continuation.** `search`, `symbol`, `refs`, `callers`,
`callees` and `impact` print a `next` token when more remains. On `callers` and
`callees` a continuation resumes the walk itself, from the frontier the previous
page persisted, with a fresh per-page work allowance; the `walked` counts it
reports stay cumulative across the pages of the one walk, so replaying a cursor
neither resets nor doubles them. `impact` resumes differently from the traversal commands: it ranks the whole
walk on the first page, serves the first `--limit` entries and keeps the ranked
tail, so a continuation replays that tail in rank order without walking again
and spends no further budget. Its `walked` counts and its package rollup are
therefore the same on every page — they describe the one walk behind the whole
answer. `path` is not paged at all — it declares neither flag, and its
`--visited` budget is spent by the one search it runs.

**Zero on a flag is not "unlimited"; zero in the configuration is.** A zero
`--depth`, `--visited` or `--edges` takes the configured bound, and a positive
value may narrow that bound but never widen it — a request that asks for more
than the configuration allows is answered under the configured value and told
so, in a notice reading `requested N, effective M`, rather than silently
tightened. The configured bounds themselves default to unlimited.

**Bounded per page, unlimited in total, user-set limits reported.**
`--visited` and `--edges` are per-page work budgets. On `callers` and `callees`,
a page that exhausts one stops there, reports the reason (`visited node budget
exhausted`, `edge budget exhausted`) and mints a continuation cursor; following
that cursor reaches the same nodes an unbounded walk would, so a budget you set
shapes the size of a page and never the completeness of the answer. The
frontier memory budget behaves the same way: a level that reaches it spills to
the continuation spool, reports `frontier memory budget exhausted`, and the
next page carries on from where it stopped.

Two stops end an answer rather than a page, and both say so. `--depth` is part
of the query a cursor is bound to, so a walk that ran out of depth is truncated
with `graph depth budget exhausted` and no continuation. And `impact` walks once
on its first page, so a per-page budget it exhausts truncates that walk; the
reason travels onto every later page of its spooled ranked tail. `path` is not
paged at all and its budgets truncate the one search it runs; it reports
truncation together with whatever routes it found — never as "no path exists",
which is reserved for a target that is genuinely unreachable.

The query deadline is not a work budget and is not resumable: a walk that
exceeds it fails the request with `CTX_QUERY_DEADLINE` rather than returning a
short page, so a slow answer is never served as a complete one. A capability that is still building is reported as an unavailable row plus a
warning, so an incomplete answer never reads as a complete one.

## Configuration these commands read

The engine reads no configuration itself; the values below are resolved once,
where the workspace is composed, and handed to it.

| Key | Effect here |
|---|---|
| `context.max_graph_depth` | Default and ceiling for `--depth`. Unlimited by default. |
| `context.max_visited_nodes` | Default and ceiling for `--visited`, per page. Unlimited by default. |
| `context.max_graph_edges` | Default and ceiling for `--edges`, per page. Unlimited by default. |
| `context.max_reason_paths_per_entry` | Equal-cost routes `path` returns. An `impact` entry carries the one route that admitted it, so a positive value admits that route and zero suppresses it. |
| `resources.max_page_items` | Default and ceiling for `--limit`. |
| `resources.query_timeout` | The per-request deadline the walk runs under. |
| `resources.max_concurrent_graph_queries` | Process-wide limit on concurrent graph queries. Waiting past the request deadline is `CTX_RESOURCE_LIMIT`. |
| `resources.query_memory_bytes` | The one query memory admission, and two structures draw on it. For a traversal it is the ceiling on the edges one frontier level may hold at once: a level that reaches it spills to the continuation spool, the page stops there, reports `frontier memory budget exhausted` and hands back a cursor the next page resumes the walk from. For `search` it also sets the external sort's in-memory run budget — a quarter of this number, floored so a small setting slows the sort rather than failing the query — which is what keeps the deduplicated and ranked sets off the heap: runs spill to disk and merge. It is a share of the one admission rather than a key of its own, so the parts can never oversubscribe the whole. Peak heap on both paths is a function of this number rather than of the graph or the match count. |
| `storage.query_cursor_ttl` | Lifetime of a `--cursor` token and of the retention lease it names. It is also the lifetime of a source receipt: a receipt `codectx context read` issued is refused with `CTX_CURSOR_INVALID` once this has elapsed. |

## Context compilation reads the same facts

The context compiler (Section 15) is not a command — it ships as a library the
workspace exposes, and Task 16's coverage session is its consumer — but it
queries through this same engine and the same pinned generation, so the keys
above bind identically for it. It pins ONE generation for a whole compile and
passes that explicit generation into every seed lookup, expansion and batch, so
an activation part way through can never split one manifest across two
generations.

It reads these further keys:

| Key | Effect there |
|---|---|
| `context.default_estimated_tokens` | The per-slice token budget a zero `budget.max_estimated_tokens` resolves to. |
| `context.default_max_bytes` | The per-slice byte budget a zero `budget.max_bytes` resolves to. |
| `context.default_max_files` | The distinct selected files a zero `budget.max_files` resolves to, across the whole plan. |
| `context.max_slices` | The slice count a zero `budget.max_slices` resolves to. |

A zero budget field means "use the configured default", never "unlimited", and a
configured default that resolves to zero or less is rejected rather than
disabling the bound. A budget that cannot hold the required scope is
`CTX_MINIMUM_BUDGET` carrying the floor (`min_bytes`, `min_estimated_tokens`,
`min_slices`, `min_files`, `missing`): required files are never demoted, split
into misleading independently complete slices, or dropped to fit.

`resources.max_page_items` bounds every batch the compile issues — seed
resolution, file hydration, the edge read behind per-edge precision and the
evidence batch — and `resources.query_timeout` is the deadline around the whole
compile. A deadline or a cancellation returns an explicit incomplete answer
(`CTX_QUERY_DEADLINE` / `CTX_CANCELED`) and persists no manifest.

See [Configuration](configuration.md) for the full tables.

## The repository map — `repo-map`

`codectx repo-map [path]` is not a graph walk, but it reads the same pinned
generation through the same paging machinery, so it is documented here rather
than twice.

```sh
codectx repo-map .            --depth 2
codectx repo-map --repo . --limit 100 --json
```

It reports the **containers** of the pinned generation — repository, modules,
packages and directories — with the file, symbol and byte totals aggregated
under each one. Nothing is materialized whole: the answer is a bounded page of
container metadata, and a repository larger than one page is continued through
the token the answer prints rather than truncated.

`--depth` bounds how far down the container tree the map goes, counted from the
repository root; zero leaves the configured bound in charge, as it does on every
flag in the table above. `--repo`, `--generation`, `--limit`, `--cursor`,
`--timeout` and `--json` mean exactly what they mean for the five commands
above.

**Paging repeats the whole question.** A continuation token is bound to the
request that minted it, so the second page is `--cursor <token>` *plus every
other flag the first page carried* — `--depth` and `--limit` included:

```sh
codectx repo-map . --limit 100
codectx repo-map . --limit 100 --cursor <token>   # not `--cursor <token>` alone
```

Dropping one of them is refused with `CTX_CURSOR_INVALID` naming the inputs the
token is bound to, rather than silently answering a differently shaped question.

**Names are canonical, not provider-spelled.** The `path` and `name` of a
container are its canonical qualified name with any quoting the provider spelled
the scope with removed, and `name` also drops the trailing separator: a Go
package that is stored as a backquoted, slash-terminated scope is reported as
`github.com/owner/repo/internal/x`, not as the scope string. A container whose
children could not be counted inside the query's edge budget is **refused** with
`CTX_RESOURCE_LIMIT` rather than reported with short totals — an under-counted
package reads as the repository's shape, not as an incomplete answer.

The repository is named either as a positional path — the Section 18.1 spelling
— or with `--repo`. Naming it both ways is **refused** rather than resolved to
one of them: either precedence silently ignores something the operator typed.

A workspace that cannot be opened fails the command. It is not answered with an
empty page, because an empty map and an unopenable workspace are different
facts and rendering one as the other is the single thing a map must never say.
