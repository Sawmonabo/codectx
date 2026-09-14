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

- **Unknown keys are rejected** with `CTX_CONFIG_INVALID`. There is no extension
  namespace, so an unrecognized key is always a typo or a setting from a
  different version that this build would otherwise silently ignore.
- **No zero or negative value means "unlimited."** Every limit is a positive
  number. Exactly three keys give `0` a documented meaning of its own, and each
  is still a finite bound: `index.workers = 0` chooses from the available CPUs
  and memory reservations, `index.max_retained_bytes = 0` leaves retention
  governed by `index.retain_refs` alone rather than by a byte budget, and
  `providers.dependence.unit_memory_ceiling_bytes = 0` derives a unit's
  allocation from the machine. A negative value is rejected everywhere; it is
  never a third meaning.
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
| `max_files` | `250000` | project (lower only) | Maximum files in one workspace. Exceeding it is `CTX_RESOURCE_LIMIT`, never a truncated index. It is a budget, not a response ceiling: it may be set as large as the workspace needs. |
| `max_parse_file_bytes` | `5242880` | project (lower only) | Largest file admitted to structural parsing. |
| `max_search_file_bytes` | `26214400` | project (lower only) | Largest file admitted to search indexing. |

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
| `retain_refs` | `8` | Distinct refs (branches or commits) whose results stay on disk. Retention is by ref, not by snapshot count: switching A → B → C → A finds A's units still there and reuses them without a run. Every unit any retained generation references is retained with it. The minimum is `1`, which retains the active ref alone. |
| `max_retained_bytes` | `0` | Byte budget for the retained store. `0` means retention is governed by `retain_refs` alone. When set, least-recently-used refs are evicted first and never the active one. |

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
| `max_temp_bytes` | `4294967296` | Temporary bytes across materializations; must exceed `min_free_disk_bytes`. |
| `min_free_disk_bytes` | `1073741824` | Free-space reserve. Disk pressure returns a typed error or pauses indexing; it never evicts open-session source. |
| `max_metadata_response_bytes` | `262144` | Ceiling for generic tool responses, which never carry source bodies. Must be smaller than the source budget. |
| `max_source_response_bytes` | `7340032` | Ceiling for a source response, including encoding and envelope expansion. The 7 MiB hard ceiling cannot be raised. |
| `query_timeout` | `"10s"` | Deadline for one query. |
| `max_query_text_bytes` | `8192` | Largest query text. |
| `max_query_terms` | `32` | Most terms in one query. |
| `max_page_items` | `200` | Largest page. |
| `max_provider_record_bytes` | `4194304` | Largest single provider record; must fit `index.batch_bytes`. |

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
| `query_cursor_ttl` | `"15m"` | Lifetime of a signed query cursor and its retention lease. |

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
| `executable` | yes | Absolute path of a directly executable launcher. A bare command name is rejected: what `PATH` resolves to is not what was named. It must be a file the operating system can start on its own — not a `.jar` and not a `.js` entry point, even where the pinned tool is one: an override replaces the binary and never the invocation, so no managed runtime is composed around it and nothing supplies a `java -jar` or a `node` prefix. Overriding `scip-java`, `jdtls`, `scip-typescript`, `scip-python`, `typescript-language-server` or `pyright` therefore means naming a wrapper script, not the jar or the script the lock names. |
| `version` | yes | The exact version this binary is, not a constraint. It is recorded as the identity of the tool behind the units it produces. |
| `checksum` | yes | Lowercase hex SHA-256 of `executable`, verified at the start of every run. It is required, not optional: an override names a binary the lock does not describe, so without it the entry would admit whatever happens to sit at that path on the next run. |

## `[providers.*]` — analysis providers

| Key | Default | Trust | Meaning |
|---|---|---|---|
| `tree_sitter.enabled` | `true` | user | Structural parsing with the grammars bundled in this binary. It runs as a private subcommand of this same binary, so there is no external executable to approve. |
| `tree_sitter.languages` | `["go", "javascript", "typescript", "tsx", "python", "java", "rust", "c", "cpp"]` | project | Languages to parse. |
| `tree_sitter.worker_idle_ttl` | `"60s"` | user | Idle time before a parser worker is stopped. |
| `scip.enabled` | `"auto"` | user | `true`, `false` or `"auto"`. |
| `scip.timeout` | `"20m"` | user | Deadline for one SCIP indexer run. |
| `lsp.enabled` | `"auto"` | user | `true`, `false` or `"auto"`. |
| `lsp.request_timeout` | `"15s"` | user | Deadline for one language-server request. |
| `lsp.max_servers` | `1` | user | Concurrent language servers. |
| `lsp.max_outstanding_requests` | `8` | user | In-flight requests per server. |
| `lsp.idle_ttl` | `"60s"` | user | Idle time before a server is stopped. |
| `dependence.enabled` | `"auto"` | user | `true`, `false` or `"auto"`. `"auto"` runs dependence units as low-priority background work once the base generation is active; a query that asks for a dependence fact promotes its units and is answered `pending` until they seal. `true` blocks the index on them. The provider never delays base readiness. |
| `dependence.timeout` | `"45m"` | user | Deadline for one dependence unit. |
| `dependence.cache_bytes` | `4294967296` | user | Budget for the per-unit parsed-graph cache, which is what makes an unchanged unit cost nothing on refresh. |
| `dependence.unit_memory_floor_bytes` | `805306368` | user | Smallest allocation a unit may be sized to. Sizing a unit near its live set costs time rather than memory, so the floor keeps a small unit from being starved into a much slower run. |
| `dependence.unit_memory_ceiling_bytes` | `0` | user | Largest allocation a unit may be sized to. `0` derives it from the machine: free memory minus the base index footprint minus a safety margin. There is no default memory ceiling, and only a non-zero value here may reject a unit before it runs. |

## `[analyzers.<name>]` — approved analyzer profiles

**User trust only.** A project file that declares a profile is rejected with
`CTX_TRUST_REQUIRED`. A profile is a description of a tool that has been
approved; nothing in the configuration layer runs anything.

| Key | Required | Meaning |
|---|---|---|
| `executable` | yes | Absolute path. A bare command name is rejected: what `PATH` resolves to is not what was approved. |
| `version_constraint` | yes | The version the caller must observe before admitting output. |
| `checksum` | no | Expected lowercase SHA-256 hex of the executable. This is how a writable-repository executable substitution is detected. |
| `args` | no | Fixed argument array. Only the substitutions `${input_dir}`, `${output_file}`, `${work_dir}` and `${manifest}` may appear; any other `${...}` is rejected rather than passed through literally. |
| `env_allowlist` | no | Names of environment variables the child may receive. Anything not named is absent from the child environment entirely. It is a fingerprint input: changing it invalidates the units the profile produced. |
| `work_dir` | yes | Absolute private working directory. |
| `memory_budget_bytes` | yes | Memory reservation for one run. |
| `disk_budget_bytes` | yes | Temporary disk reservation for one run. |
| `network` | yes | `"denied"` or `"allowed"`. See the limitation below. |
| `timeout` | yes | Deadline for one run. |

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
| `max_graph_depth` | `3` | user | Graph expansion depth. |
| `max_visited_nodes` | `50000` | user | Nodes visited in one expansion. |
| `max_graph_edges` | `100000` | user | Edges traversed in one expansion. |
| `max_reason_paths_per_entry` | `3` | user | Explanation paths per entry. |
| `max_manifest_bytes` | `8388608` | user | Largest manifest. |
| `max_capsule_bytes` | `8388608` | user | Largest capsule. |
| `strict_read_gate` | `true` | user | Require confirmed source coverage before implementation readiness. |
| `allow_exploratory_waiver_consolidation` | `false` | user | Exploratory waiver consolidation. It never weakens strict read readiness. |

## `[coverage]` — source reads and receipts

All **user** trust.

| Key | Default | Meaning |
|---|---|---|
| `chunk_bytes` | `65536` | Default raw source chunk. |
| `max_chunk_bytes` | `1048576` | Largest raw source chunk. The chunk plus its worst-case base64 expansion and envelope must fit `resources.max_source_response_bytes`. |
| `session_ttl` | `"24h"` | Lifetime of a coverage session. |
| `max_receipts_per_confirmation` | `16` | Receipts per confirmation batch. |
| `max_unconfirmed_chunks_per_session` | `64` | Issued but unconfirmed chunks per session. |

## `[mcp]` — server transport

All **user** trust.

| Key | Default | Meaning |
|---|---|---|
| `transport` | `"stdio"` | The only supported transport in this build. |
| `watch` | `true` | Keep the index fresh while the MCP server runs. |

## Cross-field validation

The resolved configuration is checked as a whole, not key by key. These are the
relationships that must hold:

- `coverage.chunk_bytes` ≤ `coverage.max_chunk_bytes` ≤ 1 MiB.
- `coverage.max_chunk_bytes` base64-encoded, plus the response envelope, fits
  `resources.max_source_response_bytes`, which itself fits the 7 MiB ceiling.
- `resources.max_metadata_response_bytes` < `resources.max_source_response_bytes`.
- `resources.max_provider_record_bytes` ≤ `index.batch_bytes` ≤ `index.queue_bytes`.
  One record must fit one batch, and one batch must fit the queue reservation.
  There is no separate ceiling on a record beyond that: `index.batch_bytes` is
  the real constraint, and inventing a second one would refuse a configuration
  that is internally consistent.
- `resources.max_concurrent_graph_queries` ≤ `resources.max_concurrent_queries`.
- `max_concurrent_queries × query_memory_bytes` + `cache_bytes` + `queue_bytes`
  ≤ `resources.base_memory_budget_bytes`.
- `resources.max_temp_bytes` > `resources.min_free_disk_bytes`.
- `context.default_max_files` ≤ `workspace.max_files`, and
  `context.default_max_bytes` ≤ `context.max_manifest_bytes`.
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
| **Analysis config** | `max_parse_file_bytes`, `max_search_file_bytes`, every `providers.*` selection, and every approved profile's executable, version constraint, checksum, arguments, environment allowlist and network posture | unit identity and the analysis key |
| **Context policy** | the whole `[context]` table | manifest identity |

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

Every child process runs with exactly the environment its approved profile
allows and nothing else: the parent's environment is never inherited, so
credentials held by the codectx process cannot leak into an analyzer. That is a
disclosure control, and it is all it is.

It does **not** prevent the child from opening a network connection. Neither
does `network = "denied"` in a profile, which is a declared posture that
diagnostics report, not an enforced restriction. Proxy variables and
environment flags are not a security boundary. Actually denying an analyzer
network access requires an OS sandbox, container or namespace — a deployment
control outside this tool. `codectx doctor --offline` reports both the core
policy and whether an OS-level restriction is actually active, and says so when
only monitoring is available.

Similarly, an argument array is not a sandbox. Compilers, indexers and language
servers legitimately load plugins, build macros and project configuration, which
means running one on a repository can execute code from that repository. That is
why every external analyzer requires an explicitly approved profile.

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
