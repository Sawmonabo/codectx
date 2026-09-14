# Graph queries — `refs`, `callers`, `callees`, `path`, `impact`

These five commands answer bounded structural questions about one pinned
generation. They read; none of them takes the workspace lock, so they answer
while another process is indexing or watching. Every one of them produces an
answer that either is exhaustive or says, in the same breath, that it is not.

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
| `--generation` | all | Answer from this generation instead of the active one. Not combinable with `--cursor`. |
| `--timeout` | all | Deadline for this invocation. Zero leaves the configured query deadline in charge. |
| `--limit` | all but `path` | Items in one page. |
| `--cursor` | all but `path` | Continue a previous page. A cursor is bound to its endpoint, generation, analysis key and query; presenting it to a different query is `CTX_CURSOR_INVALID`. |
| `--depth` | `callers`, `callees`, `path`, `impact` | Maximum hops from the nearest start node. |
| `--visited` | `callers`, `callees`, `path`, `impact` | Maximum distinct nodes the walk may admit. |
| `--edges` | `callers`, `callees`, `impact` | Maximum distinct relations the walk may admit. On `callers`, `callees` and `impact` it is cumulative across the pages of one answer. |

**`path` issues no continuation.** `search`, `symbol`, `refs`, `callers`,
`callees` and `impact` print a `next` token when more remains, and resuming one
carries the budget the earlier pages already spent rather than refilling it.
`impact` resumes differently from the traversal commands: it ranks the whole
walk on the first page, serves the first `--limit` entries and keeps the ranked
tail, so a continuation replays that tail in rank order without walking again
and spends no further budget. Its `walked` counts and its package rollup are
therefore the same on every page — they describe the one walk behind the whole
answer. `path` is not paged at all — it declares neither flag, and its
`--visited` budget is spent by the one search it runs.

**Zero is not "unlimited".** A zero budget takes the configured default, and a
positive value may narrow that default but never widen it. The visited and edge
budgets are cumulative across the pages of one traversal rather than refilled
per page, so a continuation cannot spend the allowance twice.

Exhausting any budget, or the deadline, marks the answer truncated with the
reason that stopped it, and `path` reports truncation together with whatever
routes it found — never as "no path exists", which is reserved for a target that
is genuinely unreachable. A capability that is still building is reported as an
unavailable row plus a warning, so an incomplete answer never reads as a
complete one.

## Configuration these commands read

The engine reads no configuration itself; the values below are resolved once,
where the workspace is composed, and handed to it.

| Key | Effect here |
|---|---|
| `context.max_graph_depth` | Default and ceiling for `--depth`. |
| `context.max_visited_nodes` | Default and ceiling for `--visited`. |
| `context.max_graph_edges` | Default and ceiling for `--edges`. |
| `context.max_reason_paths_per_entry` | Equal-cost routes `path` returns. An `impact` entry carries the one route that admitted it, so a positive value admits that route and zero suppresses it. |
| `resources.max_page_items` | Default and ceiling for `--limit`. |
| `resources.query_timeout` | The per-request deadline the walk runs under. |
| `resources.max_concurrent_graph_queries` | Process-wide limit on concurrent graph queries. Waiting past the request deadline is `CTX_RESOURCE_LIMIT`. |
| `resources.query_memory_bytes` | Ceiling on the edges one frontier level of a traversal may hold at once. A level that reaches it stops reading, and the answer is truncated with `frontier memory budget exhausted` rather than accumulating a hub without a bound. |
| `storage.query_cursor_ttl` | Lifetime of a `--cursor` token and of the retention lease it names. |

See [Configuration](configuration.md) for the full tables.
