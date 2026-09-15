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

**One unreadable blob costs one hit, not the answer.** A `search` hit's `range`
is resolved by reading the file's bytes out of the content store. When the store
no longer holds them — the blob is missing, a read comes back short, a block
digest does not verify, or the snapshot no longer retains the row — that hit
comes back with no `range` and an `unresolved_fields` object naming the field
that could not be resolved against the typed reason it failed with:

```json
{ "path": "pkg/gone.go", "tier": "lexical_fts",
  "unresolved_fields": { "range": "CTX_SOURCE_INTEGRITY: blob is missing from the content-addressed store" } }
```

Every other hit on the page keeps its real range. The field is absent from a hit
that resolved normally, so a missing `range` is never ambiguous: either the hit
says why it is missing, or the hit carries one. This applies to the content
store alone. A document that claims bytes past the file it names is a corrupt
index, not a clamped range, and still fails the query with
`CTX_ARGUMENT_INVALID`; a cancelled query or an exhausted deadline still fails
as itself rather than arriving as a complete answer with per-hit footnotes. Run
`codectx doctor --deep` to verify the content store when hits carry this flag.

## Flags

| Flag | Commands | Meaning |
|---|---|---|
| `--repo` | all | Workspace root to answer from. |
| `--generation` | all | Answer from this generation instead of the active one. It is not combinable with `--cursor`: a cursor already pins its generation. |
| `--timeout` | all | Deadline for this invocation. Zero leaves the configured query deadline in charge. |
| `--limit` | all but `path` | Items in one page. A route set is bounded by the reason-path cap rather than paged, so `path` declares no `--limit`. |
| `--cursor` | all | Continue a previous page — on `path`, continue the same search. A cursor is bound to its endpoint, generation, analysis key and query; presenting it to a different query is `CTX_CURSOR_INVALID`. |
| `--depth` | `callers`, `callees`, `path`, `impact` | Maximum hops from the nearest start node. |
| `--visited` | `callers`, `callees`, `path`, `impact` | Nodes one page may admit. On `callers` and `callees` it is a **per-page work budget**: a page that spends it ends there and hands back a cursor. On `path` it is a per-page work budget too: a page that spends it ends there with a cursor, and the search carries on from the state it kept (its memory budget never truncates anything); on `impact` it ends the page and mints a continuation. |
| `--edges` | `callers`, `callees`, `impact` | Relations one page may admit. On `callers` and `callees` this is a per-page budget on the same terms as `--visited`; on `impact` it is a per-page budget too. |

**Every graph command issues a continuation.** `search`, `symbol`, `refs`, `callers`,
`callees`, `path` and `impact` print a `next` token when more remains. On `callers` and
`callees` a continuation resumes the walk itself, from the frontier the previous
page persisted, with a fresh per-page work allowance; the `walked` counts it
reports stay cumulative across the pages of the one walk, so replaying a cursor
neither resets nor doubles them. `impact` resumes the walk itself as well, but its pages carry a different
contract. Impact answers one page of the walk at a time. Each page ranks its own
chunk of the affected set and carries the package rollup over that chunk's
edges; `next_cursor` resumes the walk from the frontier the page stopped at. The
pages together enumerate exactly the affected-entity set a single unbounded walk
would, and a package pair's `pair_count` and `evidence_count` sum across pages to
the whole-walk totals, but the ranking is per page and an entity reached again
from a later page's frontier is listed again with that page's reasons.
`visited_count` and `edge_count` are cumulative and grow across the pages of one
answer. The same paragraph applies to the package-dependency rollup, which pages
on identical terms; it has no CLI, app-facade or MCP surface today, so its
continuation is reachable only from the engine API.

**Known defect, under remediation — the per-page ranking above is not the
intended contract.** Ranking each page over its own chunk, and listing an entity
again when a later page's frontier reaches it, are consequences of taking the
whole-walk accumulator off the heap, not a behaviour chosen for callers. The
intended contract is the one a single unbounded walk gives: one globally ranked,
deduplicated sequence paged without reordering or repetition. Until that lands,
treat the paragraph above as a description of current behaviour rather than as a
guarantee to build on.

**`path` returns the provably cheapest route, whatever the graph's size.** Its
search is an external-memory Dijkstra: every relation cost is an integer of at
least 1, so the search runs as cost-bucket phases, and a node is final once its
bucket is settled. The settled distances, the parent edges of the shortest-path
DAG and the tentative buckets live in a private scratch database for the life of
the request, not on the heap, so peak heap is one node-aligned slice of the
current bucket — `resources.query_memory_bytes` — plus that database's fixed
page cache, and never a function of the reachable set. Crossing that number
costs another slice and nothing else: there is no memory truncation reason, and
a `path` answer is never shortened because the search ran out of heap. A `path` page ends on the
deadline or on the per-page `--visited` budget with a cursor: the search's whole
state — settled set, parent DAG, cost buckets and the nodes it has settled but
not yet expanded — is retained under the cursor's own lease and reopened by the
next page, which carries on settling buckets rather than starting again. The
answer is exact once the final page arrives; the depth bound is the one stop
that is reported rather than resumed, because it is part of the query the cursor
is bound to. A retained search is reclaimed when its lease expires, like any
other continuation state.

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
with `graph depth budget exhausted` and no continuation. `impact` no longer walks once: a per-page
budget it exhausts ends that page and mints a continuation, on the same terms as
`callers` and `callees`. `path` is not
paged at all and its visited, edge and depth budgets truncate the one search it
runs; it reports truncation together with whatever routes it found — never as
"no path exists", which is reserved for a target that is genuinely unreachable,
and never because the search exceeded a memory ceiling.

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
| `resources.query_memory_bytes` | The one query memory admission, and two structures draw on it. For a traversal it is the ceiling on the edges one frontier level may hold at once: a level that reaches it spills to the continuation spool, the page stops there, reports `frontier memory budget exhausted` and hands back a cursor the next page resumes the walk from. For `path` it is the ceiling on one slice of the cost bucket being settled — the rest of that search's state is on disk — so reaching it costs another slice and never truncates the route list. For `search` it also sets the external sort's in-memory run budget — a quarter of this number, floored so a small setting slows the sort rather than failing the query — which is what keeps the deduplicated and ranked sets off the heap: runs spill to disk and merge. It is a share of the one admission rather than a key of its own, so the parts can never oversubscribe the whole. Peak heap on both paths is a function of this number rather than of the graph or the match count. |
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

### A compile is a stream, not a whole-set pass

A compile runs as a pipeline of sorted streams joined by merge: seeds and the
required scope, file hydration, relation attributes, route scoring, per-package
centrality, boosts, measurement, packing and emission. No stage holds a
structure sized by the candidate count. Every stage sorts through the same
external sort `search` uses, so its in-memory run budget is the quarter share of
`resources.query_memory_bytes` described above and peak heap is a function of
that number and of the resolved budget, never of how wide the task's scope is.

Run files are written under the workspace's spool directory and removed on every
exit path, including the error paths. Like the sort runs `search` writes, they
are not charged against `resources.max_temp_bytes`: a run set is sized by the
candidate count, and charging it would let that budget refuse a wide task
outright. Size the directory for the widest compile you expect in addition to
the live continuation spools the budget does cover.

A compile under the run budget never touches disk: the sort spills only once its
run buffer fills, so a small task is still two in-memory sorts.

The plan a compile persists follows the same rule. Its entries and slices are
bounded by the budget you asked for -- your own declared window -- and are
written from it. Its **exclusion list is not**: a plan that keeps forty entries
can exclude every other candidate in the repository, so the exclusions are
spooled as the emission pass produces them and streamed into the manifest
transaction row by row, in the order `--view excluded` reads them back. Writing
a manifest therefore costs one exclusion row of memory, not all of them, however
many candidates the budget turned away.

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
