# Configuration

codectx resolves one configuration from three layers, in this order:

1. **Built-in defaults** — the values in this document.
2. **User configuration** — `<os-config-dir>/codectx/config.toml`
   (`$XDG_CONFIG_HOME/codectx/config.toml` or `~/.config/codectx/config.toml` on
   Linux, `~/Library/Application Support/codectx/config.toml` on macOS,
   `%AppData%\codectx\config.toml` on Windows).
3. **Project configuration** — `.codectx.toml` in the workspace root.

Only the keys a file actually contains are merged; an absent key keeps the value
the previous layer resolved. The complete resolved configuration is then
validated once, and `codectx` refuses to start if it does not hold together.

Three rules apply to every key:

Why the count and size keys below default to unlimited, why `0` and
`"unlimited"` are one value, and which two families deliberately keep a finite
default are recorded in [ADR-0001 — Scale posture](adr/ADR-0001-scale-posture.md).

- **Unknown keys are rejected** with `CTX_CONFIG_INVALID`. There is no extension
  namespace, so an unrecognized key is always a typo or a setting from a
  different version that this build would otherwise silently ignore.
- **`0` — or the string `"unlimited"` — means unlimited, and it is the default
  for every count and size bound.** This build indexes a repository of any size:
  no shipped value refuses a repository, skips a file, fails an analysis unit,
  drops a row or truncates an answer. Set a bound and it is honoured, and
  exceeding it is always **reported** — a named file, a reason, a count — never
  a silent clamp and never a silent drop. A negative value is rejected
  everywhere; it is not a third meaning.

  Three groups of keys are **not** bounds and keep positive values:

  - **Reservations** — worker counts, batch and queue sizes, memory budgets,
    connection counts. These size the machine the work is given, not the
    repository. A zero-sized batch or a zero-connection reader is a broken
    reservation, not an unbounded one, so `0` is rejected. They never refuse a
    repository either: they serialise and defer work instead.
  - **Caller budgets** — `context.default_max_bytes`, `default_estimated_tokens`,
    `default_max_files` and `max_slices`. This is the **one family of non-zero
    defaults deliberately kept**. They are not scale refusals: a context plan
    must fit a model's window, and a plan that does not fit one is not an
    answer. They stay honest on two conditions, both of which hold. The
    manifest names **every** excluded file with a reason, paginated, never
    summarised — an exclusion is a row in the manifest, not a count. And a
    request may ask for **more** than the default: a positive `budget.*` field
    on a request wins over the configured default in either direction, and
    `budget.max_manifest_bytes` may additionally be set to unlimited by a
    caller that can hold any manifest.
  - **Wire and pagination ceilings** — `resources.max_page_items`,
    `max_query_text_bytes`, `max_metadata_response_bytes`,
    `max_source_response_bytes`, `coverage.chunk_bytes`, `max_chunk_bytes`,
    `max_receipts_per_confirmation`, `tools.max_fetch_bytes`. These bound one
    page or one message, which is lossless: the next page carries the rest.

  Two keys give `0` a meaning of its own that is not "unlimited":
  `index.workers = 0` chooses the count from available CPUs and reservations,
  and `providers.dependence.unit_memory_ceiling_bytes = 0` derives a unit's
  allocation from the machine.

  **Every bound that defaults to unlimited**, grouped by the table it lives in.
  Each key is described in full in that table below.

  | Table | Keys defaulting to `0` / `"unlimited"` |
  |---|---|
  | `[workspace]` | `max_files`, `max_parse_file_bytes`, `max_search_file_bytes` |
  | `[index]` | `max_retained_bytes` (no byte budget; retention is then governed by `retain_refs` alone), `watch_max_directories` |
  | `[resources]` | `max_query_terms`, `max_provider_record_bytes` |
  | `[providers.lsp]` | `max_overlay_bytes` |
  | `[providers.dependence]` | `max_units_per_family`, `max_staged_rows`, `max_derived_rows`, `max_export_files` |
  | `[context]` | `max_graph_depth`, `max_visited_nodes`, `max_graph_edges`, `max_reason_paths_per_entry`, `max_manifest_bytes`, `max_capsule_bytes` |
  | `[coverage]` | `max_unconfirmed_chunks_per_session` |
  | `[workflow]` | `max_observation_references` |

  `index.retain_refs` is the one key of this shape that keeps a finite default
  (`8`), because evicting a generation is lifecycle retention of something
  re-indexing reconstructs, not a dropped row; it still accepts `0` for "retain
  every ref".

- **A limit may be lowered, never raised past its contract ceiling.**
  Configuration narrows what this build does; it cannot widen what a stored
  column or a bounded response can hold.

## Trust model

| Trust level | Meaning |
|---|---|
| **user** | Only the user configuration may set it. A project file that sets it is rejected with `CTX_TRUST_REQUIRED`. |
| **project (lower only)** | A project file may set it, but only to a value that narrows the effective limit. Raising it is `CTX_TRUST_REQUIRED`. |
| **project** | A project file may set it freely. |

The split exists because `.codectx.toml` lives inside the repository being
analyzed. A repository may tell codectx to look at less of itself; it may never
tell codectx to run something, to look outside itself, to relax a safety check,
or to raise a resource ceiling. Concretely, a project file can never set an
executable path, an argument array, an environment variable, a shell, a network
posture, a path root, any `[tools]` key, `workspace.follow_symlinks`,
`storage.data_dir`, `context.strict_read_gate`, or
`context.allow_exploratory_waiver_consolidation`.

`enabled = "auto"` for an external analyzer means "run every managed profile
whose trigger evidence the snapshot holds." It never means "run whatever is on
`PATH`": a managed profile names an entry in the embedded tool lock, so the
analyzer it starts is a payload the shipped binary pinned and verified, never a
command found on the host. Detecting a tool by running it with `--version` or
`--help` is still execution, and it is execution of that pinned payload.

## `[workspace]` — source eligibility

| Key | Default | Trust | Meaning |
|---|---|---|---|
| `follow_symlinks` | `false` | user | Traverse symbolic links inside the workspace. Even when enabled, a link that resolves outside the workspace fails the walk with `CTX_PATH_ESCAPE`, and a link that re-enters a directory already on the path is reported as a cycle. |
| `include_untracked` | `true` | project | Index files Git does not track. Ignore rules apply to untracked discovery; tracked files are indexed even when an ignore pattern matches them. |
| `index_generated` | `false` | project | Index paths classified as generated (see below). |
| `index_vendor` | `false` | project | Index vendored dependency directories (see below). |
| `max_files` | `0` (unlimited) | project (lower only) | Maximum files in one workspace. Unlimited by default: a repository is never refused for its size. A value you set is reported when exceeded (files seen against the limit), never a truncated index. |
| `max_parse_file_bytes` | `0` (unlimited) | project (lower only) | Largest file admitted to structural parsing. Unlimited by default; a value you set reports each skipped file with its reason. |
| `max_search_file_bytes` | `0` (unlimited) | project (lower only) | Largest file admitted to search indexing. Unlimited by default; a value you set reports each skipped file with its reason. |
| `max_dir_entries` | `0` (unlimited) | project (lower only) | How many children one directory may hold. Unlimited by default; a value you set is reported once per directory that passes it and the traversal continues — it is never a clamp and never a refusal. |
| `max_depth` | `0` (unlimited) | project (lower only) | How deeply directories may nest. Unlimited by default; a value you set is reported once per level that passes it and the traversal continues into the deeper tree. |
| `max_ignored_roots` | `0` (unlimited) | project (lower only) | How many outermost ignored paths the shared traversal policy holds. Unlimited by default; a value you set that is exceeded degrades the policy to the base one — the ignored trees are walked rather than excluded, and the degradation is logged — never a refusal. |

`max_parse_file_bytes` and `max_search_file_bytes` are **analysis admission
limits, not retention limits**. A file over the limit is still captured, still
stored in the content-addressed store and still readable as source; it is
reported as skipped for that capability with an explicit reason.

### What "vendor" and "generated" mean

These two toggles are defined by fixed built-in lists, not by a configurable
pattern language. The lists are part of the meaning of the settings:

- **Vendor directories:** `vendor`, `node_modules`, `third_party`,
  `bower_components`, `Godeps`.
- **Generated directories:** `__pycache__`, `.next`.
- **Generated file suffixes:** `.pb.go`, `.pb.cc`, `.pb.h`, `_pb2.py`,
  `_pb2_grpc.py`, `.generated.go`, `_generated.go`, `.g.dart`, `.freezed.dart`,
  `.min.js`, `.min.css`.

Classification is by path only. No file is opened to guess at its provenance,
because sniffing every candidate for a generated-code banner would mean reading
the whole repository before capture.

### Unconditional exclusions

Two paths are excluded regardless of every setting and every hook: any `.git`
entry, and the resolved data directory when it lies inside the workspace.
Neither is repository source, and indexing the store while writing it would be a
feedback loop.

A path that Git tracks overrides an ignore match and the vendor and generated
toggles. It does not override the unconditional exclusions, the symlink policy,
path validation, or the file budget.

An excluded directory is not read at all unless some tracked path is known to
lie inside it. Disabling `index_vendor` therefore costs nothing on a repository
with a large `node_modules`: the directory is never listed, so it cannot be
walked and cannot fail the per-directory entry limit either.

## `[index]` — scheduling

None of these are semantic inputs: they change how work is scheduled, never what
it concludes. All are **user** trust.

| Key | Default | Meaning |
|---|---|---|
| `workers` | `0` | Indexing workers; `0` chooses from available CPUs and memory reservations. |
| `max_parser_workers` | `2` | Ceiling on concurrent native parser worker processes. |
| `batch_records` | `1000` | Records per provider batch. |
| `batch_bytes` | `4194304` | Bytes per provider batch. Must fit `index.queue_bytes`. |
| `queue_bytes` | `16777216` | Reserved bytes for the staging queue. |
| `watch_pending_paths` | `10000` | Pending watch events before coalescing. |
| `watch_pending_bytes` | `2097152` | Reserved bytes for pending watch events. |
| `watch_debounce` | `"250ms"` | Quiet period before a watch batch is scheduled. |
| `reconcile_interval` | `"30s"` | Period of full reconciliation, which catches missed and timestamp-preserving changes. |
| `retain_refs` | `8` | Distinct refs (branches or commits) whose results stay on disk. Retention is by ref, not by snapshot count: switching A → B → C → A finds A's units still there and reuses them without a run. Every unit any retained generation references is retained with it. `0` retains every ref, matching `max_retained_bytes`. This keeps a finite default because a generation is reconstructible by re-indexing, so evicting one is lifecycle retention rather than a dropped row. |
| `max_retained_bytes` | `0` | Byte budget for the retained store. `0` means retention is governed by `retain_refs` alone. When set, least-recently-used refs are evicted first and never the active one. |
| `watch_max_directories` | `0` (unlimited) | How many directories one watcher may watch. Unlimited by default: a repository's directory count is a property of the repository, and the host's own notification limit is the real ceiling — reaching that is refused by the host and reported as incomplete watch coverage with a reason. A set value stops the watch set at that many directories and reports coverage incomplete, never silently. |

## `[resources]` — memory, concurrency, disk and response budgets

All **user** trust.

| Key | Default | Meaning |
|---|---|---|
| `base_memory_budget_bytes` | `805306368` | Aggregate baseline memory reservation. |
| `query_memory_bytes` | `33554432` | Reservation per concurrent query. |
| `cache_bytes` | `33554432` | Total cache reservation. |
| `max_concurrent_queries` | `4` | Concurrent queries. |
| `max_concurrent_graph_queries` | `2` | Concurrent graph queries; must not exceed `max_concurrent_queries`. |
| `max_concurrent_heavy_analyzers` | `1` | Concurrent heavy analyzer runs. |
| `max_temp_bytes` | `4294967296` | Temporary bytes across materializations; must exceed `min_free_disk_bytes`. One eighth of it is the budget for the paging spools that hold the ranked remainder of a `codectx search` answer between pages; the rest stays the materialization budget it already was. Temporary sort runs written while a query is being ranked share the spool directory but are deliberately **not** charged against this budget, because a run set is sized by the match count and charging it would let this budget refuse a wide query outright; size the directory for the ranked working set of the largest query you expect in addition to the live continuation spools the budget does cover. |
| `min_free_disk_bytes` | `1073741824` | Free-space reserve. Disk pressure returns a typed error or pauses indexing; it never evicts open-session source. |
| `max_metadata_response_bytes` | `262144` | Ceiling for generic tool responses, which never carry source bodies. Must be smaller than the source budget. |
| `max_source_response_bytes` | `7340032` | Ceiling for a source response, including encoding and envelope expansion. The 7 MiB hard ceiling cannot be raised. |
| `query_timeout` | `"10s"` | Deadline for one query. `codectx search` and `codectx symbol` apply it to the whole request, from pinning the generation to hydrating the page; exceeding it is `CTX_QUERY_DEADLINE`, an explicit incomplete answer, never a persisted complete one. `--timeout` on those commands narrows it further and never widens it. |
| `max_query_text_bytes` | `8192` | Largest query text. |
| `max_query_terms` | `0` (unlimited) | Most terms in one query. Unlimited by default; a value you set refuses the query with the term count, never silently drops terms. Query text is tokenized with the index's own tokenizer, and a quoted phrase counts as one term. |
| `max_page_items` | `200` | Largest page, and the bound `--limit` is clamped to, for `codectx search` and `codectx symbol` as well as the graph commands: lowering it lowers the pages they serve. The candidate bound each retrieval tier of `codectx search` is read to stays the `200` ceiling, so narrowing the page never narrows what was ranked; a tier that fills that bound makes the answer report `truncated` with a reason rather than silently serving a short page, and every continuation of that answer repeats the same `truncated` flag and reason. |
| `max_provider_record_bytes` | `0` (unlimited) | Largest single provider record. Unlimited by default, so one oversized record never fails a unit; when you set one it must fit `index.batch_bytes`. |

## `[storage]` — SQLite and the data directory

All **user** trust.

| Key | Default | Meaning |
|---|---|---|
| `data_dir` | `""` | Absolute path for all cache and state. Empty resolves to a user-private per-workspace directory (below). |
| `busy_timeout` | `"5s"` | SQLite busy timeout. |
| `read_connections` | `2` | Reader connections in the bounded pool. |
| `writer_cache_kib` | `8192` | Writer page cache. |
| `reader_cache_kib` | `4096` | Per-reader page cache. |
| `wal_high_water_bytes` | `67108864` | WAL size that triggers a checkpoint. |
| `closed_session_retention` | `"7d"` | How long closed sessions are retained before pruning. |
| `query_cursor_ttl` | `"15m"` | Lifetime of a signed query cursor and its retention lease, **and of a source receipt**. A `codectx search` or `codectx symbol` continuation takes its own retention lease for this long, so the generation the first page was read from stays collectable only once the token it printed has expired. The same value bounds how long a receipt `codectx context read` issued may be echoed back to `codectx context acknowledge`: lowering it to shorten cursor retention shortens that window too, and a receipt echoed after it has expired is rejected as `CTX_CURSOR_INVALID`. |

### The data directory

By default all cache and state lives **outside the repository**, in a
per-workspace subdirectory of the OS cache directory:

```
<os-cache-dir>/codectx/<workspace-key>
```

`<workspace-key>` is a 16-character prefix of the canonical hash of the absolute
workspace root, so two checkouts of the same project never share state and no
path component of the repository ever appears in the data path.

Loading configuration does not create the directory. The storage layer creates
it with user-private permissions when it first opens the database. When the data
directory does lie inside the workspace, it is excluded from traversal
unconditionally.

## `[retention]` — the process-level collector

All **user** trust.

| Key | Default | Meaning |
|---|---|---|
| `blob_grace` | `"24h"` | How long a blob that nothing references waits, once trashed, before the collector rechecks reachability and deletes its row and its content-addressed object. It is the safety margin that protects a reader which pinned a generation in the instant the manifest naming a blob went away, not a throughput knob: shortening it narrows that protection, lengthening it only delays reclaim. Must be positive — a zero window would delete an object in the same pass that trashed it. |

## `[tools]` — the managed analyzer toolchain

All **user** trust, and a project file that sets any key here is rejected with
`CTX_TRUST_REQUIRED`: this table decides which binaries this build executes.

codectx owns every external analyzer and every runtime one needs. Nothing is
looked up on `PATH`, nothing is installed by hand, and nothing runs that the
shipped binary did not pin: each tool's exact version, per-platform URL and
SHA-256 come from a lock embedded in the binary, and a payload whose digest
disagrees is never extracted and never executed. None of these keys is required
for ordinary use.

| Key | Default | Meaning |
|---|---|---|
| `offline` | `false` | Make every fetch a typed refusal without opening a socket. A tool already in the store still runs. |
| `cache_dir` | `""` | Absolute path of the tool store. Empty resolves to `tools/` inside the data directory, created user-private. |
| `mirror` | `""` | Absolute `https` URL prefix serving every lock asset. It replaces the scheme and host and keeps the original host as the first path segment — `https://nodejs.org/dist/v22.23.2/node.tar.gz` becomes `<mirror>/nodejs.org/dist/v22.23.2/node.tar.gz` — so one mirror serves every publisher the lock names without their paths colliding. The digests stay the lock's, so a mirror relocates bytes and never changes which bytes are accepted. Plaintext `http` is rejected. A mirror answers `200` directly or redirects only within the upstream host's own set; a redirect to a host of the mirror's own is refused. |
| `max_fetch_bytes` | `2147483648` | Ceiling on one payload download. |
| `fetch_timeout` | `"10m"` | Deadline for one payload download. |

### `[tools.override.<name>]`

An override is the only way to run a tool the lock did not ship for this
platform, or to replace one it did. It changes the **binary** and never the
invocation: argument arrays, environment allowlists, budgets, working
directories and network posture are product code, not configuration.

| Key | Required | Meaning |
|---|---|---|
| `executable` | yes | Absolute path of a directly executable launcher. A bare command name is rejected: what `PATH` resolves to is not what was named. It must be a file the operating system can start on its own — not a `.jar` and not a `.js` entry point, even where the pinned tool is one: an override replaces the binary and never the invocation, so no managed runtime is composed around it and nothing supplies a `java -jar` or a `node` prefix. Overriding `scip-java`, `jdtls`, `scip-typescript`, `scip-python`, `typescript-language-server` or `pyright` therefore means naming a wrapper script, not the jar or the script the lock names. An override of the graph engine (`joern`) carries a second constraint: it must be that payload's own `joern-parse` launcher, because the export tool the same unit runs is derived from this file's name and must sit beside it under the same directory; an override named anything else is refused with `CTX_TOOL_OVERRIDE_INVALID`. |
| `version` | yes | The exact version this binary is, not a constraint. It is recorded as the identity of the tool behind the units it produces. |
| `checksum` | yes | Lowercase hex SHA-256 of `executable`, verified at the start of every run. It is required, not optional: an override names a binary the lock does not describe, so without it the entry would admit whatever happens to sit at that path on the next run. |

## `[providers.*]` — analysis providers

| Key | Default | Trust | Meaning |
|---|---|---|---|
| `tree_sitter.enabled` | `true` | user | Structural parsing with the grammars bundled in this binary. It runs as a private subcommand of this same binary, so there is no external executable to approve. |
| `tree_sitter.languages` | `["go", "javascript", "typescript", "tsx", "python", "java", "rust", "c", "cpp"]` | project | Languages to parse. |
| `tree_sitter.worker_idle_ttl` | `"60s"` | user | Idle time before a parser worker is stopped. |
| `scip.enabled` | `"auto"` | user | `true`, `false` or `"auto"`. |
| `scip.timeout` | `"0s"` (no limit) | user | Wall-clock deadline for one SCIP indexer run. `0` by default: a monorepo's import is slow, not broken, and a deadline that fails an analysis unit refuses a repository for its size. |
| `scip.stall_timeout` | `"5m"` | user | Hang detector, not a size limit. How long a subprocess may make **no progress at all** — no stdout, no stderr, no CPU, no growth of its output file — before the unit fails with reason `stalled` and is reported. Finite by default: a wedged process makes no progress however large the repository. |
| `lsp.enabled` | `"auto"` | user | `true`, `false` or `"auto"`. |
| `lsp.request_timeout` | `"15s"` | user | Deadline for one language-server request. |
| `lsp.max_servers` | `1` | user | Concurrent language servers. |
| `lsp.max_outstanding_requests` | `8` | user | In-flight requests per server. |
| `lsp.idle_ttl` | `"60s"` | user | Idle time before a server is stopped. |
| `lsp.max_overlay_bytes` | `0` (unlimited) | user | Unlimited by default. When set, it bounds, separately, the materialized snapshot (files that do not fit are named in the log and left out, never refused), admission of one file to the pinned coordinate cache (a file over the bound is reported and the query answers about the rest), and the bytes sent to a server in one rolling minute. Unlimited still keeps a finite ceiling on the pinned cache, whose eviction costs a re-read and no answer. A value you set must be at least `resources.max_source_response_bytes`. |
| `dependence.enabled` | `"auto"` | user | `true`, `false` or `"auto"`. `"auto"` runs dependence units as low-priority background work once the base generation is active; a query that asks for a dependence fact promotes its units and is answered `pending` until they seal. `true` blocks the index on them. The provider never delays base readiness: nothing is installed when the workspace is opened, and the analysis payload is fetched by the first unit that needs it. A one-shot building command -- `codectx index` or `codectx refresh` -- prints its own generation as soon as that generation is active and then stays until the deferred queue is empty, publishing each batch as a further generation; interrupting it keeps everything already published and reports how much is still queued. `false` is the opt-out: it plans no dependence unit at all. |
| `dependence.timeout` | `"0s"` (no limit) | user | Wall-clock deadline for one dependence unit. `0` by default, for the same reason as `scip.timeout`. |
| `dependence.stall_timeout` | `"5m"` | user | The same progress-based hang detector as `scip.stall_timeout`, applied to one dependence unit. |
| `dependence.cache_bytes` | `4294967296` | user | Budget for the per-unit parsed-graph cache, which is what makes an unchanged unit cost nothing on refresh. |
| `dependence.unit_memory_floor_bytes` | `805306368` | user | Smallest allocation a unit may be sized to. Sizing a unit near its live set costs time rather than memory, so the floor keeps a small unit from being starved into a much slower run. |
| `dependence.unit_memory_ceiling_bytes` | `0` | user | Largest allocation a unit may be sized to. `0` derives it from the machine: free memory minus the base index footprint minus a safety margin. There is no default memory ceiling, and only a non-zero value here may reject a unit before it runs. |
| `dependence.max_units_per_family` | `0` (unlimited) | user | How many frontend-native projects of one language family you want a plan to hold. Unlimited by default: a monorepo's project count belongs to the repository, so every project is planned as its own unit and a crashed unit is still split along every one of its parts. A set value refuses nothing and drops nothing — crossing it marks the family's capability rows partial with `CTX_RESOURCE_LIMIT`, naming the family, the project count and this value. |
| `dependence.max_staged_rows` | `0` (unlimited) | user | How many rows you want one unit's import to stage. Unlimited by default: staging is an on-disk database read back one keyset page at a time, so the row count bounds disk (roughly 10× the export's bytes), not memory. A set value never fails the unit and never stops the import — crossing it marks the unit's capability rows partial with `CTX_RESOURCE_LIMIT`, carrying the staged count and this value. |
| `dependence.max_derived_rows` | `0` (unlimited) | user | How many relation occurrences you want one unit's import to project from its staged rows. Unlimited by default: the projection is computed and paged inside the same on-disk staging database, so the occurrence count bounds disk rather than memory, and it belongs to the source. A set value never fails the unit and never truncates the projection — crossing it marks the unit's capability rows partial with `CTX_RESOURCE_LIMIT`, carrying the derived count and this value. |
| `dependence.max_export_files` | `0` (unlimited) | user | How many entries one analysis export directory may hold. Unlimited by default: the file count follows the export's label vocabulary rather than the repository, and the directory is read one entry at a time. A set value is the only thing that refuses an import here, with `CTX_RESOURCE_LIMIT` naming this key. |

### Timeouts here are hang detectors, not size limits

`providers.scip.timeout` and `providers.dependence.timeout` are `0` by default,
which means **no wall-clock limit at all**. A wall clock cannot tell a large
analysis unit from a wedged one — on a monorepo the legitimate run is the long
one — so a deadline that fails a unit is a refusal of the repository for its
size.

What catches a wedged subprocess instead is `providers.*.stall_timeout`, which
is finite by default (`5m`) and measures **progress, not elapsed time**: bytes
read from the child's stdout or stderr (counted before any output bound, and
whether or not the bytes are kept) and, where the platform can sample a running
tree, the tree's consumed CPU time. A child that produces none of those signals
for the whole window is terminated, and the unit fails with reason `stalled`
and is reported. Setting either `stall_timeout` to `0` disables the detector for
that provider; a negative value is rejected.

## Analyzer profiles are not configurable

There is no `[analyzers.<name>]` table. An analyzer profile — which binary
starts, under which runtime, with which argument array, which environment
variables it may see, its budgets, its timeout and its declared network posture
— is product code, and the binary is the payload the embedded tool lock pinned
and the store verified (Section 20.2 — trust is the lock, not an approval).
There is no approved path to write, no `PATH` lookup and no way for a
configuration file to change what an analyzer is given.

What a user may still say about tools is in `[tools]` and
`[tools.override.<name>]` above: where the store lives, whether fetching is
allowed, which mirror to use, and — for one named lock entry at a time — an
exact replacement binary with its version and checksum. `docs/providers-scip.md`
and `docs/providers-lsp.md` list the argument arrays and environment allowlists
each profile actually uses.

There is no shell, so an argument is always a literal argument. There are no
cloud or AI credential fields in core.

## `[context]` — context compiler

| Key | Default | Trust | Meaning |
|---|---|---|---|
| `default_phase` | `"sweep"` | project | Default workflow phase for a request that names none. |
| `default_estimated_tokens` | `80000` | project (lower only) | Default token budget. |
| `default_max_bytes` | `524288` | project (lower only) | Default byte budget; must fit `max_manifest_bytes`. |
| `default_max_files` | `200` | project (lower only) | Default file budget; must fit `workspace.max_files`. |
| `max_slices` | `16` | user | Most slices in one manifest. |
| `max_graph_depth` | `0` (unlimited) | user | Graph expansion depth. |
| `max_visited_nodes` | `0` (unlimited) | user | Nodes **one page** of an expansion may visit. A per-page work budget, not a ceiling on the walk. |
| `max_graph_edges` | `0` (unlimited) | user | Relations **one page** of an expansion may admit. A per-page work budget, not a ceiling on the walk. |
| `max_reason_paths_per_entry` | `0` (unlimited) | user | Explanation paths stored per entry. |
| `max_manifest_bytes` | `0` (unlimited) | user | Largest manifest. Unlimited by default; a compile is never refused for the size of its plan. |
| `max_capsule_bytes` | `0` (unlimited) | user | Largest capsule, by bytes. Unlimited by default. It is not the only thing that bounds a capsule today — see [The one remaining default cap](#the-one-remaining-default-cap) below. |
| `strict_read_gate` | `true` | user | Require confirmed source coverage before implementation readiness. |
| `allow_exploratory_waiver_consolidation` | `false` | user | Exploratory waiver consolidation. It never weakens strict read readiness. |

The context compiler binds them as follows. The three `default_*` budgets and
`max_slices` are what a **zero** field of a request's budget resolves to. They
are the one family where zero in a *request* is "use the default" rather than
"unlimited", because a context plan that does not fit a window is not an answer.
`default_max_bytes` and `default_estimated_tokens` apply **per slice**,
`default_max_files` counts distinct selected files across the whole plan, and
`max_slices` caps the total. Those four are caller budgets, not limits on the
repository, which is why they alone keep non-zero defaults — and every file a
budget excludes is named in the manifest with its reason.

`max_graph_depth`, `max_visited_nodes` and `max_graph_edges` bound graph
expansion, and all three are unlimited by default.

`max_visited_nodes` and `max_graph_edges` are **per-page work budgets, not
cumulative ceilings on a walk**. A page spends its own allowance; a page that
exhausts one stops there, reports the reason that stopped it (`visited node
budget exhausted` or `edge budget exhausted`) and mints a continuation cursor,
and the next page resumes the walk from the persisted frontier with a fresh
allowance. The answer is therefore bounded per page and unlimited in total:
following the cursor reaches the same nodes an unbounded walk would. The
`visited` and `edges` counts a result reports stay **cumulative** across the
pages of one walk, so a caller still sees the total the walk has spent, and a
replayed cursor neither resets nor doubles it.

Two stops are not resumable, and both say so rather than pretending otherwise.
`max_graph_depth` is part of the query a cursor is bound to, so a walk that ran
out of depth is reported truncated with no continuation. And `impact` performs
its whole walk on the first page and then serves a spooled ranked tail, so a
per-page budget it exhausts ends that one walk: the answer is truncated with
the reason, and every later page repeats the same flag and reason.

`max_reason_paths_per_entry` bounds the explanation routes stored per entry;
routes beyond it are reported as a count, never silently dropped.

The ranking weights themselves are **not** configuration. They are compile-time
constants labelled by the manifest's `policy_version`, so changing one changes
that label rather than one workspace's answers. The whole `[context]` table is
already part of manifest identity through the context policy fingerprint below,
so a manifest compiled under different bounds is a different manifest rather
than a silent reuse.

### The one remaining default cap

One built-in cap in this build can still refuse work, and it is stated here
rather than left to be discovered. Sealing a capsule is bounded by two
constants that are **not** configuration and do not default to unlimited:

- **250 000 coverage or waiver files per capsule.** A session whose coverage or
  waiver set is larger cannot be sealed.
- **1 000 records per capsule list** for the capsule's scope and its typed
  record lists.

Crossing either refuses the seal with `CTX_RESOURCE_LIMIT`, naming the list and
the bound; a capsule is never sealed with a truncated list, so nothing is
silently dropped. What a user hits today is therefore an explicit refusal on
`codectx context capsule` for a session of that size, with the remediation to
narrow the session's scope — every other surface of that session, including its
coverage and manifest pages, is unaffected.

This is the one known remaining default cap in the tree. Making the capsule a
paginated, resumable surface is a schema-level change across the workflow, MCP
and CLI layers, and it is scheduled as its own sub-track of the next wave rather
than half-landed here. The reasoning is recorded in
[ADR-0001 — Scale posture](adr/ADR-0001-scale-posture.md).

## `[coverage]` — source reads and receipts

All **user** trust.

| Key | Default | Meaning |
|---|---|---|
| `chunk_bytes` | `65536` | Default raw source chunk. |
| `max_chunk_bytes` | `1048576` | Largest raw source chunk. The chunk plus its worst-case base64 expansion and envelope must fit `resources.max_source_response_bytes`. |
| `session_ttl` | `"24h"` | Lifetime of a coverage session. |
| `max_receipts_per_confirmation` | `16` | Receipts per confirmation batch. |
| `max_unconfirmed_chunks_per_session` | `0` (unlimited) | Issued but unconfirmed chunks per session. Unlimited by default; a value you set pauses further chunks until receipts are confirmed, and says so. |

There is no receipt lifetime of its own: a source receipt lives as long as
`storage.query_cursor_ttl`, which the `[storage]` table above describes. A
session that reads for longer than that must acknowledge as it goes, because an
expired receipt cannot be confirmed and its bytes earn no coverage.

## `[workflow]` — observations and scope reviews

All **user** trust.

| Key | Default | Trust | Meaning |
|---|---|---|---|
| `max_observation_references` | `0` (unlimited) | user | How many references you want one recorded observation — or one scope review, counted across all eight of its categories — to carry. Unlimited by default: a review of a large scope cites what it read, and the product does not refuse an attestation for the size of the repository it attests to. A set value is the operator's own ceiling: crossing it is reported with `CTX_RESOURCE_LIMIT`, naming the reference count and this value, so raising it is a decision with both numbers in hand. |

A single observation's own wire contract still refuses more than 64 references,
so this key governs the aggregate scope-review path in full and the
single-observation path only up to that contract ceiling.

## `[mcp]` — server transport

All **user** trust.

| Key | Default | Meaning |
|---|---|---|
| `transport` | `"stdio"` | The only supported transport in this build. |
| `watch` | `true` | Keep the index fresh while the MCP server runs. `codectx mcp serve --watch=false` turns it off for one session, and `--watch` turns it on where this key is `false`. |

## Cross-field validation

The resolved configuration is checked as a whole, not key by key. These are the
relationships that must hold:

- `coverage.chunk_bytes` ≤ `coverage.max_chunk_bytes` ≤ 1 MiB.
- `coverage.max_chunk_bytes` base64-encoded, plus the response envelope, fits
  `resources.max_source_response_bytes`, which itself fits the 7 MiB ceiling.
- `resources.max_metadata_response_bytes` < `resources.max_source_response_bytes`.
- `resources.max_provider_record_bytes` ≤ `index.batch_bytes` ≤ `index.queue_bytes`,
  the first pairing only when you set a record bound at all.
  One record must fit one batch, and one batch must fit the queue reservation.
  There is no separate ceiling on a record beyond that: `index.batch_bytes` is
  the real constraint, and inventing a second one would refuse a configuration
  that is internally consistent.
- `resources.max_concurrent_graph_queries` ≤ `resources.max_concurrent_queries`.
- `max_concurrent_queries × query_memory_bytes` + `cache_bytes` + `queue_bytes`
  ≤ `resources.base_memory_budget_bytes`.
- `resources.max_temp_bytes` > `resources.min_free_disk_bytes`.
- `context.default_max_files` ≤ `workspace.max_files`, and
  `context.default_max_bytes` ≤ `context.max_manifest_bytes` — each only when
  the bound on the right is set. There is nothing to exceed in an unlimited one.
- `providers.dependence.unit_memory_ceiling_bytes`, when set, is at least
  `providers.dependence.unit_memory_floor_bytes`. A ceiling under the floor is
  not a narrow budget; it is a provider that rejects every unit before it runs.
- All byte arithmetic stays inside 64-bit signed range.

## Fingerprints

Configuration feeds three separate fingerprints, so that changing one kind of
setting does not invalidate work that did not depend on it:

| Fingerprint | Covers | Invalidates |
|---|---|---|
| **Source policy** | `follow_symlinks`, `include_untracked`, `index_generated`, `index_vendor`, `max_files`, and a digest of this build's built-in vendor and generated classification lists | the snapshot identity |
| **Analysis config** | `max_parse_file_bytes`, `max_search_file_bytes` and every `providers.*` selection | unit identity and the analysis key |
| **Context policy** | the whole `[context]` table | manifest identity |

`[tools]` is deliberately **not** in the analysis fingerprint: a store location
or a mirror is an operational setting, and the identity that matters is the
payload's own. Each provider folds the fingerprint of the payloads it resolved
into its own `Descriptor().Version` instead, so replacing an analyzer
invalidates the units it produced without a store path doing the same.

The exclusion lists are in the source fingerprint because the toggles alone do
not decide eligibility: `index_vendor = false` means something different in a
build that classifies a different set of directories as vendored, and the
snapshot identity has to say so rather than reuse a capture taken under the
other list. An analyzer's environment allowlist is in the analysis fingerprint
for the same reason: a profile that gains a variable can resolve different
dependencies from identical source.

Operational settings — worker counts, batch sizes, cache sizes, idle TTLs, the
data directory, retention, the tool store location and its fetch budgets,
transport and logging — are in none of them. They change how the work is
scheduled or where it is kept, never what it concludes.

Raising an admission limit does change the analysis fingerprint. That is
deliberate: a unit produced under a lower limit is not the same result as one
produced under a higher one, and reusing it would report a complete result that
was in fact produced under a different requirement.

## Capture and isolation limitations

These are properties of the design, stated plainly so that no setting is read as
a stronger guarantee than it is.

### A regular filesystem gives no global instant

An ordinary filesystem cannot deliver a transactionally simultaneous read of
every file while other processes are writing. What codectx guarantees is an
exact immutable captured manifest plus detected-change validation, and it
reports which of the two it had:

- `validated_capture` — the ordinary case: an exact manifest of the bytes that
  were captured, with changes detected and revalidated during reconciliation. A
  file changing during capture is retried at most twice, after which the capture
  fails with `CTX_SNAPSHOT_UNSTABLE` and the last active generation is retained.
- `operator_frozen` — the operator supplied a quiescent or locked source, or an
  OS-level snapshot.

`capture_consistency` is never upgraded by inference. Nothing codectx observes
can turn a `validated_capture` into an `operator_frozen` one; only the operator
supplying an actually frozen source can.

Watcher events and coarse timestamps are not proof that a file is unchanged, so
a previously computed hash is reused only through a validated unchanged-file
mechanism, and periodic full reconciliation catches timestamp-preserving edits.

### Clearing the environment is not a network boundary

Every child process runs with exactly the environment its profile's allowlist
names and nothing else: the parent's environment is never inherited, so
credentials held by the codectx process cannot leak into an analyzer. That is a
disclosure control, and it is all it is.

It does **not** prevent the child from opening a network connection. Neither
does a profile's `denied` network posture, which is a value diagnostics report,
not an enforced restriction. Proxy variables and
environment flags are not a security boundary. Actually denying an analyzer
network access requires an OS sandbox, container or namespace — a deployment
control outside this tool. `codectx doctor --offline` reports both the core
policy and whether an OS-level restriction is actually active, and says so when
only monitoring is available.

Similarly, an argument array is not a sandbox. Compilers, indexers and language
servers legitimately load plugins, build macros and project configuration, which
means running one on a repository can execute code from that repository. That is
why every external analyzer is a payload this build pinned by digest, with an
argument array and an environment this build fixed, rather than anything a
repository or a configuration file can influence.

### Resource budgets are admission, not enforcement

A memory or disk budget is counted before work starts. A native child can
temporarily exceed its reservation, and Go's own memory limit is a soft runtime
control that covers neither native allocations nor subprocess memory. Hard
enforcement is platform-dependent: codectx uses the process-group and Job Object
controls the platform provides, and reports when only admission and monitoring
are available. Under memory pressure it reduces concurrency and caches before it
refuses work, and it degrades concurrency before it degrades coverage.

Process-tree termination itself is enforced: on Unix every child runs in its own
process group and the whole group is signalled, and on Windows every child is
created suspended, assigned to a Job Object with kill-on-close, and only then
resumed, so no descendant can escape between creation and assignment.
