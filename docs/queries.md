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
| `callers <name-or-id>...` | who reaches these symbols | incoming `calls`, and the virtual calls into every method they override |
| `callees <name-or-id>...` | what these symbols reach | outgoing `calls`, the overrides a virtual call may run, and an endpoint's `routes_to` |
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
evidence row behind it. The order is the **canonical relation id ascending**,
and a page ends on a relation boundary, so the pages of one `refs` answer
concatenate to the single-shot answer: the same occurrences, in the same order,
each listed exactly once. That order is a property of the facts alone — it does
not move when a reindex renumbers the store's internal identifiers — so a
`refs` cursor kept across pages resumes the list a caller already saw. The
first page reads the symbol's whole list once and retains everything it did not
serve; a list that fits one page retains nothing at all. A page served by a
process that holds the writer renews that retention and issues a cursor with a
fresh `resources.cursor_ttl`, so a long reference list is not bounded by the TTL
its first page was minted under. A process that writes nothing to the database
records no retention lease and renews nothing: its pages carry forward the
expiry the first one stamped on the retained list, which is reachable for that
one TTL and `CTX_CURSOR_INVALID` after it. A
`refs` cursor carries a versioned payload: one minted by a build that spelled it
differently answers `CTX_CURSOR_INVALID` rather than being read with today's
field meanings.

**One unreadable blob costs one hit, not the answer.** A `search` hit's `range`
is resolved by reading the file's bytes out of the content store. When the store
no longer holds them — the blob is missing, a read comes back short, a block
digest does not verify, or the snapshot no longer retains the row — that hit
comes back with no `range` and an `unresolved_fields` object naming the field
that could not be resolved against the typed reason it failed with:

```json
{ "path": "pkg/gone.go", "tier": "lexical",
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

## What a call answer carries

**Where everything is.** A `callers`, `callees` or `path` answer carries a
`locations` entry for every node it lists — the file, its path in the pinned
snapshot and the node's byte range — and an `occurrences` entry for every call
site behind every `calls` relation it lists, each with its file, byte range,
`dispatch` (`virtual` when the call can run an override of its target, `exact`
when it cannot) and, for a site bound to several targets, its `set_size`. One
answer is enough to open a call hierarchy: no follow-up `symbol` or `refs` call
is needed to place a caller or a call. A relation with more call sites than one
answer holds lists its first sites and a notice gives the exact count. The
MCP `codectx_callers` and `codectx_callees` tools walk `calls` when `relations`
is empty, exactly as these commands do; a request that names relations walks
those.

**Calls through a base class are found at query time.** A call is stored once,
against the method its receiver's declared type declares: in

```python
class Animal:
    def sound(self): ...
class Dog(Animal):
    def sound(self): ...
def speak(a: Animal): a.sound()
def direct(d: Dog): d.sound()
```

`speak` calls `Animal.sound`. So `callers Dog.sound` lists `direct`, then climbs
`overrides` to `Animal.sound` — at any depth, so a `Puppy.sound` under `Dog`
reaches both — and lists `speak`, whose call is virtual, marked
`through_base` with the base and how many methods override it. The
`overrides` row it climbed is in the answer, the row lists only the call's
virtual sites, and `counts` reports `direct` and `through_base` apart. A
through-base row is a call that **may** run the method, never one that must:
`super().sound()` and a qualified `Animal::sound()` are exact and never appear,
and `callers Animal.sound` never lists `direct`, whose call can only run
`Dog.sound`. `callees speak` lists `Animal.sound` and then every override of it,
at any depth, marked `may_dispatch` and counted as `may_dispatch`. A site bound
by value flow to several classes is marked `value_set` with its set size ("1
of 2"). `path` and a forward `impact` take the same downward step, a path
only from a call that can dispatch, at that call's cost even when a cheaper
exact call reaches the same method; `impact` of a method lists the methods
overriding it; `refs` of a method includes the references of every method in
its override family, up and down. The step belongs to the command, not to the
relation list: a walk over `calls` alone takes it. A base method the climb
reached that is also a caller of a symbol in the walk has the rest of its
callers walked too, a callee first reached by an exact call and later by one
that can dispatch has its overrides listed too, and an `overrides` row is listed once even when the
request names `overrides`. When a method marks no override and its class extends a type outside the
indexed project, the callers answer says that calls through that type are found
only for overrides the language marks.

**Calls no analysis bound are listed by name, after every precise row.** When
the linker cannot bind a call — `obj.handle()` with nothing known about `obj` —
it publishes one open-site node for the call, located at it, named by the
callee and carrying its shape (`bare`, `member`, `value`, `construct`) and
argument count; the caller's `calls` edge goes to that node, and it replaces the
structural `call:<name>` placeholder for the same site, so an open call is one
row, on whichever page the open site falls. Its candidates are computed per query, never stored: the declarations of
the same name, of a kind the shape can call (methods for a member call,
functions for a bare or value call, classes for a construction), in the same
language family (C with C++, JavaScript with TypeScript), and, in C, C++, Go,
Java and Rust, with parameters that take the call's arguments. Each is a
`may_call` relation, marked `name_only`: it may not be what runs.

- `callers` lists, after the precise callers, the callers of every open site
  the symbol is a candidate of, and counts them as `may_call` beside
  `open_sites`; with no caller at all, by analysis or by name, the notice
  quotes the linker's site counts (sites, bound, open, computed, external)
  rather than reporting a bare zero.
- `callees` lists the open-site node and then its candidates, nearest first:
  the same file, the same directory, a file the call's file imports or is
  imported by, the same unit, then anywhere.
- `refs` lists the callers of those open sites after every precise
  reference, at `heuristic` precision.
- `path`, only when the search proved no precise route exists, searches
  again with `may_call` admitted at the dearest relation cost, so the cheapest
  route takes as many name-only hops as it needs and no more. It is the same
  disk-backed search, paged the same way, and lists every equal-cost route as
  the precise search does; each route is marked `possible`, each hop carries
  its call sites, and a notice per route counts its hops.
- `impact` counts, per depth, the open sites naming an affected symbol
  (`open_sites_by_depth`) and does not list their callers as affected; an MCP
  request whose `relations` name `may_call` walks on through them, one depth
  deeper per hop, on routes marked `possible`.

The possible rows are one hop from the start symbols; an MCP request whose
`relations` name `may_call` walks on through them, to the callers of the
callers' open sites and so on, up to its depth bound. A name declared thousands of times has an exact count
and is paged, never cut. An import whose path does not fold to a constant
(`require("./plugins/" + name)`) is an open import site with its literal
fragments, and an outgoing `imports` answer lists, after the precise imports,
every file whose path matches them as `may_import`, also by name only and not
walked on unless a request's `relations` name `may_import`.

**Endpoints.** `routes_to` (endpoint to handler) is not a call and is not in the
`callers` or `callees` relation set, but a `callees` walk from an endpoint
follows it to the handler first, then the handler's calls. `impact` walks it,
so a changed handler marks the endpoints that route to it affected.

## Flags

| Flag | Commands | Meaning |
|---|---|---|
| `--repo` | all | Workspace root to answer from. |
| `--generation` | all | Answer from this generation instead of the active one. It is not combinable with `--cursor`: a cursor already pins its generation. |
| `--timeout` | all | Deadline for this invocation, and the only thing that bounds one: `resources.query_timeout` is a default and not a ceiling, and it is unlimited, so a command you do not bound answers in full. Every command honours that — `search`, `symbol`, the graph walks, `context plan`, the coverage and workflow commands, and the MCP tools alike. A value here **sets** the deadline for the call — above a configured `resources.query_timeout` as readily as below it. What a call does when it expires depends on whether there is anything to resume past. `search`, `symbol`, the graph walks and `context plan` accumulate work across pages or passes, so a bounded call ends with a continuation cursor and the next call carries on from it. The coverage commands (`coverage read`, `coverage next`, `coverage ack`, `coverage session`, `coverage status`) and the workflow commands (`workflow advance`, `workflow close`, `workflow include`, `workflow status`) answer one bounded read or apply one mutation, so there is no partial state for a cursor to name: an expiry there is reported as `CTX_QUERY_DEADLINE`, naming `resources.query_timeout` / `--timeout` as the setting to move. Zero asks for no deadline of its own and leaves the configured value in charge. |
| `--limit` | all | Items in one page; on `path`, the equal-cost routes one page lists. |
| `--cursor` | all | Continue a previous page — on `path`, continue the same search, or the list of its equal-cost routes. A cursor is bound to its endpoint, generation, analysis key and query; presenting it to a different query is `CTX_CURSOR_INVALID`. |
| `--depth` | `callers`, `callees`, `path`, `impact` | Maximum hops from the nearest start node. |
| `--visited` | `callers`, `callees`, `path`, `impact` | Nodes a walk may admit. On `callers` and `callees` it is a **per-page work budget**: a page that spends it ends there and hands back a cursor. On `path` it is a per-page work budget too: a page that spends it ends there with a cursor, and the search carries on from the state it kept, so the route it finally reports is the one an unbounded search would. On `impact`, which walks once for the whole answer, it is an **answer-level** bound: spending it truncates that answer and is reported. |
| `--edges` | `callers`, `callees`, `impact` | Relations a walk may admit, on exactly the terms `--visited` is bounded on for the same command. |
| `--direction` | `path` | Which way edges are followed: `outgoing` (the default), `incoming` or `both`. The other traversal commands pin their own direction — a `callers` query asked outgoing would not be a callers query — so only `path` takes it. |

**`path --direction` is the difference between "no route" and "no route that way".**
The default, `outgoing`, answers *what does the first node depend on, on the way to the
second*. A node that is only ever called has no outgoing route to its callers, so an
outgoing search between such a pair reports no path at all even though the two are
connected; `--direction incoming` follows edges backwards, and `--direction both`
ignores their orientation and returns the cheapest undirected route. The direction is
part of the query a cursor is bound to, so a continuation cannot be presented to a
search walking the other way.

**Every graph command issues a continuation.** `search`, `symbol`, `refs`, `callers`,
`callees`, `path` and `impact` print a `next` token when more remains. On `callers` and
`callees` a continuation resumes the walk itself, from the frontier the previous
page persisted, with a fresh per-page work allowance; the `walked` counts it
reports stay cumulative across the pages of the one walk, so replaying a cursor
neither resets nor doubles them. `impact` carries a different contract: **its first page pays for the whole
walk.** The request that mints an impact answer expands until the walk is
exhausted, streams every admitted edge into a disk-backed sort, ranks the whole
affected set once, serves the first page and spools the globally ranked
remainder behind the continuation. Later pages read that spool and walk nothing,
which is why page 1 is the slow one and every page after it is a file read.

What that buys is the contract a single unbounded walk gives, and it is what
`impact` now guarantees: the pages **concatenate** to the single-shot answer —
the same entities, in the same global order, each listed exactly once. An entity
reached again from a later frontier is one record, not two; the cheapest route
to it wins, and the reasons of every edge that touched it are merged into that
one entry. The order is `score_micros` descending, then `depth` ascending, then
`node_id` ascending. **`name` is no longer a tie-break** — it is known only
after hydration, so ranking on it would have reordered one page of a globally
ordered answer.

An impact answer carries two ranked lists, the affected entities and the package
rollup over every edge the walk admitted, and one `next_cursor` pages both: each
page takes up to `--limit` from each list, and the cursor is offered while
either list has records left. A pair's `pair_count` and `evidence_count` are
exact totals for the whole walk, not per-page fragments to be summed. The
package-dependency rollup answers on identical terms from its own endpoint.
`visited_count` and `edge_count` are cumulative and grow across the pages of one
answer.

**What a query deadline does to that, when you set one.** There is none by
default — `resources.query_timeout` is unlimited — so an unbounded `impact`
request answers over the whole walk. A deadline you do set — through `--timeout`, or by giving
that key a positive value — ends a page, never the answer, with one exception,
the third case below, which ends the answer and says so. There are three
outcomes.

*Mid-walk.* A deadline reached while the walk is still running returns a page
with **no entries at all**, `truncation_reason` `query deadline reached` and a
cursor that carries the walk's own frontier: the next request continues the walk
from where it stopped, and once the walk completes the pages come from the
ranked spool as above.

The state that cursor names is internal to the generation it pins, and this is
what that means for you. A walk carries the set of nodes it has already
admitted, the level it had reached, and its position inside that level. All
three are expressed in the generation's own internal numbering, so a cursor is
**bound to one generation**
and is refused with `CTX_CURSOR_INVALID` by any other, exactly as a cursor
issued for a different query or a different endpoint is. Re-indexing between two
pages of an answer therefore ends that answer: present the cursor and it is
refused, rather than silently continued against a numbering in which the same
values mean different code. Re-run the query. That admitted set is what makes
the pages of one answer disjoint: `visited_count` and `edge_count` count distinct
nodes and distinct relations, so a node reached again by a second route, a cycle
or a `both`-direction edge is counted once, however many pages the answer takes.

*Mid-ranking.* A deadline that lands after the walk finished but while the
ranking is still running is reported the same way, and its cursor resumes the
interrupted sort itself: the runs the ranking had already spilled are adopted by
the next request rather than re-sorted, so the answer is the one the
uninterrupted query would have given, in the same order.

*Stalled.* A resumed page whose every adjacency read already ran past the
deadline admits nothing, and the continuation it could mint is the one it was
handed. Rather than hand a client a chain that pages forever without reaching a
new edge, that page ends the answer: `truncation_reason` `query deadline reached
before the walk could advance`, and **no cursor**. The remedy is to present the
same cursor you already hold again — that state is left adoptable, so the walk
carries on from where it stopped rather than from the seed — under a larger
`resources.query_timeout`. Do it within `resources.cursor_ttl`, which is what
that state lives on; past it the cursor answers `CTX_CURSOR_INVALID` and the
query has to be re-run.

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
not yet expanded — is retained under the cursor's own deadline and reopened by the
next page, which carries on settling buckets rather than starting again. The
answer is exact once the final page arrives; the depth bound is the one stop
that is reported rather than resumed, because it is part of the query the cursor
is bound to. A retained search is reclaimed when its lease expires, like any
other continuation state; a search retained by a process that records no lease
is reclaimed on the expiry stamped into the state itself, which every page
re-stamps as it hands its directory over.

**Zero on a flag is not "unlimited"; zero in the configuration is.** A zero
`--depth`, `--visited` or `--edges` takes the configured bound, and a positive
value may narrow that bound but never widen it — a request that asks for more
than the configuration allows is answered under the configured value and told
so, in a notice reading `requested N, effective M`, rather than silently
tightened. The configured bounds themselves default to unlimited.

**Bounded per page, unlimited in total, user-set limits reported.**
On `callers` and `callees` `--visited` and `--edges` are per-page work budgets:
a page that exhausts one stops there, reports the reason (`visited node budget
exhausted`, `edge budget exhausted`) and mints a continuation cursor; following
that cursor reaches the same nodes an unbounded walk would, so a budget you set
shapes the size of a page and never the completeness of the answer. The
frontier memory budget is not one of them: it bounds how much of a level is
held in memory at once, and a level past it spills to the walk's own scratch and
carries on, so it changes where the level lives and never which edges the answer
holds.

A walk delivers one level at a time, through two states a cursor can name. While
**collecting**, it reads the adjacency of the level before it and keeps every
edge exactly once — under `--direction both` an edge whose two ends are on the
same level is kept from its source, and one that reaches back to an earlier
level was already delivered there. Nothing is served from a level being
collected; a deadline here hands back the position the read reached. The level
is then sorted **once**, and that sort IS the served order within a level: the
canonical id of the neighbour the entry reaches, then the canonical id of the
owner it was reached from, then the canonical relation id. All three are
identifiers of the facts themselves, so two indexes of the same tree serve one
order, and every page of that level is a read straight out of the sorted level
at the byte offset the cursor carries. A continuation therefore resumes either
the read that was collecting the level or the offset that was serving it, and in
both cases it neither repeats an edge nor loses one. `visited_count` counts the nodes each level
admits as the level closes, which is when the walk admits them.

Two stops end an answer rather than a page, and both say so. `--depth` is part
of the query a cursor is bound to, so a walk that ran out of depth is truncated
with `graph depth budget exhausted` and no continuation. `impact` walks once for
the whole answer, so a `--visited` or `--edges` allowance it spends truncates
that answer, reports the same reason and offers no continuation — the ranked
pages that follow are what is left of a walk that stopped, not a walk to be
resumed. Say that plainly: `--visited N` is **your** bound, and an `impact`
answer that spends it is the truncated answer for the nodes the walk had
admitted, disclosed as `truncated` with that reason rather than served as a
whole one. Once the bounded walk ends and ranking begins, the cursor an
`impact` page offers is the ranked **tail of that bounded set** — the rest of
what was admitted, in the one global order — and never a continuation of the
walk past `N`. The default, `0`, sets no bound: the walk runs to exhaustion and
its pages concatenate to the whole answer.

`path` is the third shape, and it is neither of those. Its answer is the list
of every equal-cost route, paged by `--limit` with `next` and counted whole in
`route_count` (counted over the shortest-path graph, not by listing; "at least"
when the deadline lands first, and exact on the last page). The routes are
listed in relation order from the search's kept state, a page at a time, never
held all at once. The **search** behind it is paged too: `--visited` is a per-page work
budget there, and a page that spends it ends with `next` rather than an answer.
Following that cursor resumes the same search from the state it kept, so the
routes it finally reports are the ones an unbounded search would report. The
query deadline behaves the same way on `path` — the page ends, the cursor
carries on — and so does the engine's edge bound, which `path` registers no
flag for: it reports `edge budget exhausted` and mints a cursor. Only `--depth`
truncates a `path` answer outright, because depth is part of the query a cursor
is bound to. Truncation is always reported together with whatever routes were
found — never as "no path exists", which is reserved for a target that is
genuinely unreachable in the direction asked for, and never because the search
exceeded a memory ceiling. A pair connected only against the edge direction is
not unreachable: it is reached with `--direction incoming` or `--direction both`.

**When a temp budget you set cannot hold a continuation.** The state the next
page resumes from — a walk's frontier spool, a `path` search's retained scratch
— is charged against `resources.max_temp_bytes`, which is unlimited by default.
If you set that budget and a page's continuation state does not fit it, the
request is REFUSED with `CTX_RESOURCE_LIMIT` naming `resources.max_temp_bytes`,
rather than returning a short answer under whichever work-budget reason happened
to be marked. Retrying frees no bytes: raise the budget, or narrow the query
with `--depth`, `--visited` or `--edges` so the walk finishes within one page.

On every command but `path`, the query deadline ends a page and not an answer:
a walk that runs out of time
returns what it has, reports `query deadline reached` and hands back a cursor the
next request carries on from, rather than failing with `CTX_QUERY_DEADLINE`, so a
slow answer is never served as a complete one and never lost either. This holds
for every deadline, including one that lands before the page has read a single
edge: an `impact` or package-rollup request keeps its walk — the frontier it had
reached and every entity it had already admitted — so the page is empty,
reported as truncated, and its cursor carries the walk on. A deadline that lands
after that walk finished but during the ranking keeps the same walk and mints
the same kind of cursor; the next request ranks it and serves page 1.

`path` keeps the same contract by its own mechanism: a deadline that lands
inside the search truncates the page with `the query deadline was reached before
the path search completed` and mints a cursor over the retained search state, so
the next request carries on settling buckets. A `path` request never fails with
`CTX_QUERY_DEADLINE`.

## What `search` reads for the lexical tier

The lexical tier reads the generation's search index
([ADR-0013](adr/ADR-0013-search-index.md); [storage.md](storage.md#the-search-index)
describes it). A custom collector runs inside the index's search loop and reads,
for each matching document, the per-column term frequencies from the postings
and `token_count`, `kind`, the path ordinal and the search key from fast fields.
The statistics are exact for the generation being searched: N and the total
token count are sums of per-unit totals, and a term's document frequency counts
visible documents only, so a document the generation hides never changes a
score.

The score is BM25F over five columns — `name`, `qualified_name`, `signature`,
`path` and `body`, weighted 5, 5, 2, 2 and 1, with k1 1.2 and b 0.75 — as one
saturation over the weighted term frequency. It is computed in f64 and quantized
to 1e-6, with one pinned logarithm implementation on both sides of the boundary.
Whether document length is weighted per column is **pending**: the weighted form
is adopted if it raises MRR@10 on two of the three quality sets and lowers it on
none; otherwise the unweighted total stays.

A **phrase** — two or more tokens that must be adjacent — is answered from the
positions the index stores. A **kind filter** is a set of term ordinals and a
**folder filter** a contiguous ordinal range of the sorted path dictionary;
both are tested per document inside the collector, never as a separate scan.

## How `search` serves a page

A ranked search answers with its best results, a page at a time
([ADR-0013](adr/ADR-0013-search-index.md), decision 5;
[ADR-0001](adr/ADR-0001-scale-posture.md) §2.4). Page n is collected as the top
200·n by (quantized score, content tie key) with a heap of that size, so page 1
holds 200 entries whatever the match count. Block-max WAND, with BM25F upper
bounds per block of the union postings, skips every document that cannot enter
the top k; it changes the work and never the results, so the page equals
exhaustive scoring's. The match count is exact when every match was visited,
and otherwise a lower bound marked as one. Nothing is spooled and nothing is
sorted to disk. The exact tiers (path and symbol lookup) are not ranked: they
walk their whole keyset and return every match.

**The order guarantee.** Results are served in the Section 14.2 order: tier,
then descending score, then the content tie key — path, then start byte, then
the keys derived from the file's bytes (end byte, kind, name, qualified name,
signature), and last the identity keys. Every key is an integer or an exact
string, and the order is a pure function of the repository's content — the same
tree indexed at another root, or re-indexed as a delta, answers in the same
order. A cursor carries the last served position by content, not by document
address, so it survives the index's background merges between pages.

A cursor pins the generation, the analysis key and the query with its filters.
Resubmitting a continuation with a different query or filter set, or against a
re-analysed generation, is `CTX_CURSOR_INVALID` rather than a quietly different
answer under a continued page number. The generation a cursor pins is retained
by the cursor's lease for `storage.query_cursor_ttl`, which each page renews
where the process holds the writer; a search keeps no other state between
pages.

## Configuration these commands read

The engine reads no configuration itself; the values below are resolved once,
where the workspace is composed, and handed to it.

| Key | Effect here |
|---|---|
| `context.max_graph_depth` | Default and ceiling for `--depth`. Unlimited by default. |
| `context.max_visited_nodes` | Default and ceiling for `--visited`. Unlimited by default. |
| `context.max_graph_edges` | Default and ceiling for `--edges`. Unlimited by default. |
| `context.max_reason_paths_per_entry` | Explanation routes a context entry stores. An `impact` entry carries the one route that admitted it, so a positive value admits that route and zero suppresses it. It does not bound `path`, which lists every equal-cost route. |
| `resources.max_page_items` | Default and ceiling for `--limit`. |
| `resources.query_timeout` | The default deadline a walk runs under, taken only by a call that carries none of its own. Unlimited by default, so an unbounded walk answers in full. |
| traversal slots (half this machine's cores, at least one) | Process-wide gate on concurrent graph queries; it is derived from the machine and is not a setting. Waiting past the request deadline is `CTX_RESOURCE_LIMIT`. |
| `resources.query_memory_bytes` | The one query memory admission, and two structures draw on it. For a traversal it is the ceiling on the edges one frontier level may hold at once: a level that reaches it spills the edges it has collected to the walk's own scratch and carries on reading, so it bounds the memory a level costs and never the edges the answer holds. For `path` it is the ceiling on one slice of the cost bucket being settled — the rest of that search's state is on disk — so reaching it costs another slice and never truncates the route list. For the context compiler it also sets the external sort's in-memory run budget — a quarter of this number, floored so a small setting slows the sort rather than failing the compile — a share of the one admission rather than a key of its own, so the parts can never oversubscribe the whole. Peak heap on these paths is a function of this number rather than of the graph or the candidate count. A ranked `search` holds one top-k heap of 200·n entries for page n, whatever the match count. |
| `storage.query_cursor_ttl` | Lifetime of a `--cursor` token and of the retention lease it names. It is also the lifetime of a source receipt: a receipt `codectx context read` issued is refused with `CTX_CURSOR_INVALID` once this has elapsed. |

## Context compilation reads the same facts

The context compiler (Section 15) is not a command — it ships as a library the
workspace exposes, and the coverage session is its consumer — but it
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
structure sized by the candidate count. Every stage sorts through the
external sort the index planner uses, so its in-memory run budget is the quarter share of
`resources.query_memory_bytes` described above and peak heap is a function of
that number and of the resolved budget, never of how wide the task's scope is.

Run files are written under the workspace's spool directory and removed on every
exit path, including the error paths. They
are not charged against `resources.max_temp_bytes`: a run set is sized by the
candidate count, and charging it would let that budget refuse a wide task
outright. Size the directory for the widest compile you expect in addition to
the live continuation spools the budget does cover.

A compile under the run budget never touches disk: the sort spills only once its
run buffer fills, so a small task is still two in-memory sorts.

### A deadline you set ends the pass, not the compile

`codectx context plan` compiles the whole plan when nothing bounds it: with
`resources.query_timeout` unlimited, a call you do not bound returns the
manifest and opens the session, with no cursor to follow.

If you do bound one — `--timeout`, or a positive `resources.query_timeout` — it
carries the same contract the graph commands do. A compile that reaches that
deadline stops at the boundary of the pass it is in, persists that boundary's
streams, and answers `truncated` with
`truncation_reason deadline` and a continuation token instead of failing with
`CTX_QUERY_DEADLINE`. No manifest is persisted and no session is opened for a
truncated plan, so there is nothing partial for a later reader to mistake for an
answer.

Present that token back as `--cursor`, with every other flag unchanged, and the
compile resumes at the first unfinished pass — inside the scope walk it resumes
from the walk's own continuation, not from the seeds. The plan the chain finally
returns is byte-for-byte the plan one uninterrupted compile would have returned:
the same manifest identity, the same entries and slices in the same order, the
same receipts. The token lives for `storage.query_cursor_ttl`; past that the
continuation state is reclaimed and the compile has to be re-run.

The same contract is what an MCP client gets, on the same terms: unbounded by
default, and a continuation only when the client's own request carries a
deadline. The continuation is a property of the plan request, not of the command
line.

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
into misleading independently complete slices, or dropped to fit. `missing`
names the required paths that do not fit and ends with `… and N more` when
there are more of them than the detail field carries, so the count is always
whole even where the list is not.

`context.max_start_nodes` bounds how many discovered seeds become roots of the
boundary walk and is unlimited by default, so a task naming hundreds of
resolvable identities is walked from all of them. A value you set leaves the
seeds past it unwalked, and the manifest carries an exclusion row naming the
limit and how many roots did not start. See
[Configuration](configuration.md#context--context-compiler).

`resources.max_page_items` bounds every batch the compile issues — seed
resolution, file hydration, the edge read behind per-edge precision and the
evidence batch — and `resources.query_timeout`, when you set one, is the
deadline around the whole compile; unset, which is the default, nothing bounds
it and the compile returns the whole plan. A deadline or a cancellation persists no manifest. A **cancellation**
is an explicit incomplete answer (`CTX_CANCELED`); a **deadline** ends the pass
the compile is in rather than the answer, and `context plan` returns
`truncated = deadline` with a `next_cursor` you present back as `--cursor` to
resume at the first unfinished pass. This includes a deadline that fires while
the required-scope graph walk is still running: the pass ends with the walk's
place in hand and the continuation carries it on from there, so no leg of the
walk is ever thrown away. The plan the final call returns is the one
an uninterrupted compile would have produced. See
[Context sessions](context-sessions.md#a-plan-that-runs-out-of-query-deadline-continues-it-does-not-fail).
`CTX_QUERY_DEADLINE` is still raised for the compile callers that have no
continuation to hand back, such as the workflow service's own consolidate
compile.

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
children could not be counted inside the query's edge budget is **omitted** from
the page, which is marked truncated and carries a notice naming
`max_graph_edges`, rather than reported with short totals — an under-counted
package reads as the repository's shape, while a missing one is visible as the
incomplete answer it is.

The repository is named either as a positional path — the Section 18.1 spelling
— or with `--repo`. Naming it both ways is **refused** rather than resolved to
one of them: either precedence silently ignores something the operator typed.

A workspace that cannot be opened fails the command. It is not answered with an
empty page, because an empty map and an unopenable workspace are different
facts and rendering one as the other is the single thing a map must never say.

## Reading source — `codectx source`

```sh
codectx source <node-id | file-id | path> [--offset N] [--max-bytes N] [--generation N] [--json]
```

`codectx source` prints the indexed bytes of one node or one file, outside any
context session. It is the command-line half of `codectx_read_source` without a
`session_id`, and it reads exactly what that tool reads.

- **A node id** prints the node's range as the pinned generation holds it. A
  node keeps its id across generations while its range moves, so the range read
  is always the asked generation's: pass the `--generation` of the answer that
  named the node, and the bytes match that answer's ranges exactly. A node the
  generation does not hold — deleted, or only a language-server overlay row,
  which is never a canonical fact — is refused as **not in this generation**
  (`CTX_ARGUMENT_INVALID`). A node whose stored range or content hash disagrees
  with the generation's manifest is `CTX_SOURCE_INTEGRITY`, a damaged index
  rather than a wrong argument.
- **A file id or a path** prints the file from `--offset` (default 0) to its
  end. A node is printed from its own start and takes no `--offset`.
- **An argument of 64 lowercase hex characters is an id**: it is read as a node
  id, and as a file id when the generation holds no such node. With `--offset`
  it can only be a file id.
- **A path** is the root-relative path the index stores. It is only turned into
  a file id and looked up in the generation's manifest; nothing is opened on it.
  A path the snapshot does not hold — one that climbs out of the tree with
  `../`, an ignored file, a symbolic link the capture skipped, anything under
  the data directory — is refused as not in this generation. The bytes come
  from the content store, never from the working tree, so an edit made since the
  index ran does not show.

Without `--generation` the active generation is read. The whole span is
printed: the command reads it in chunks of `--max-bytes` raw bytes (0, the
default, is the configured `coverage.chunk_bytes`; a value above
`coverage.max_chunk_bytes` is clamped to it) and holds one chunk at a time.
Every chunk after the first is asked for by the signed continuation the previous
chunk carried, so the span comes from the generation the first chunk was read
from, and that continuation holds a retention lease, so a retention pass between
two chunks cannot collect that generation. Because the lease is a write, this
command opens the workspace the way the context commands do: during another
process's indexing write it waits for that write to commit.

Text output is the span's bytes and nothing else — a chunk that is not valid
UTF-8 is decoded back to its bytes — so `codectx source <id> > out` reproduces
them exactly, CRLF line endings and a missing final newline included. `--json`
writes one envelope per chunk, in order. Each carries the generation `binding`,
`file_id`, `node_id` for a node read, `content_hash`, the `byte_range` and
`line_range` of that chunk, `encoding` (`utf8`, or `base64` where the bytes are
not valid UTF-8), `content`, `partial_line`, and `next_offset` and
`next_cursor` while the span continues. No envelope carries a `session` block:
a read outside a session issues no receipt and earns no coverage.

Through the tool, `codectx_read_source` without `session_id` takes exactly one
of `node_id`, `file_id` or `path`, plus `offset` for a file, `max_bytes` for the
chunk size and `generation_id`; or, to continue, `cursor` alone (with
`max_bytes` if wanted). A node that fits one chunk — nearly every symbol — comes
back whole in one call; a longer span pages with `next_cursor` to the node's or
the file's end. The source response bound, `resources.max_source_response_bytes`,
applies exactly as it does to a session read.
