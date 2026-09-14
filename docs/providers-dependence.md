# The `dependence` provider

`internal/provider/dependence` supplies control dependence, data dependence
through captures and globals, reads, writes and fallback call facts for the
nine supported languages. It is the Section 11.6 provider: extraction-lazy,
cached, governed per unit, and honest about every way an analysis can come
back incomplete.

This is the only product document that names the engine behind it. Everywhere
else — the provider id, the configuration table, capability names, evidence
details, error details, log fields, query results — it is "the engine".

## What it publishes

| Capability | Relation | Evidence detail |
|---|---|---|
| `control_depends_on` | `control_depends_on` | `cdg` |
| `data_flows_to` | `data_flows_to` | `reaching_def`, `reaching_def capture`, `global` |
| `reads` | `reads` | `assignment` |
| `writes` | `writes` | `assignment` |
| `calls` | `calls` | `call` |

Every fact is `static_analysis` precision with exact byte ranges. A dependence
edge does not claim a proven end-to-end source-to-sink flow; data dependence
that crosses a method boundary through a closure or a global says so in its
own evidence detail rather than borrowing the intraprocedural label.

`reads` and `writes` come from this provider alone: no SCIP indexer sets a
write role, and syntax cannot resolve the target of an assignment.

## The engine

The backend is [Joern](https://github.com/joernio/joern), pinned by the
Section 11.7 tool lock as the `joern` entry (`kind: cpg`, runtime `jdk`,
Apache-2.0; recorded in `THIRD_PARTY_LICENSES.md`). The version verified for
this implementation is **4.0.627**.

Two noninteractive commands, one exact argv each, no product-owned analysis
script and no interpreter server:

```text
joern-parse  --language <frontend> --max-num-def 40000 <private materialization> --output <private graph>
joern-export <private graph> --repr=all --format=neo4jcsv --out <private export>
```

* `--max-num-def 40000` replaces the engine default of 4000. Measured on a
  1.05M-line Python tree: 23% more parse time, 3% more memory, every skipped
  method removed, every other fact count identical
  (`docs/research/10-round3-empirical.md` §9a). It is part of the cache key and
  there is no second parse at a higher limit.
* The single `all` export carries every edge family the importer reads
  (`CALL`, `CDG`, `REACHING_DEF`, `CONTAINS`). `--repr=pdg|cdg|ddg` is not
  implemented for CSV or GraphML in this release; there is no GraphML path.
* The engine's argument parser rejects a repeated option, so the
  semantics-neutral option allowlist must never restate a pinned one.

**No version probe.** `joern-parse --version` is rejected as an unknown option
and `joern --version` drops into the interactive console, so the engine's
name, version and payload digest come from the lock entry that installed it
and travel on `Detection.ObservedVersion` and the descriptor version.

### Digest-to-release map

| Payload digest | Release |
|---|---|
| `964655bd…` (`joern-cli.zip`, SHA-256 verified locally) | 4.0.627 |

The lock is the authority; this table is the human-readable index that
`status`, `doctor` and the ledger's `ObservedVersion` strings resolve against.

## Units

One parse handles exactly one language, so one unit is one frontend-native
project. Scope keys are `pkg:<family>:<root-relative project directory>`,
with `workspace` for the one family whose unit is the repository.

| Family | Languages | Project marker | Rule |
|---|---|---|---|
| `c` | C, C++ | — | the whole repository, one unit: header resolution spans the tree |
| `go` | Go | `go.mod` | one unit per module, nested modules are their own units; `go.work` is ignored |
| `java` | Java | `pom.xml`, `build.gradle`, `build.gradle.kts` | one unit per module, nested modules are their own units |
| `javascript` | JavaScript, TypeScript, TSX | `tsconfig.json`, `jsconfig.json`, `package.json` | the outermost project; **never split** |
| `python` | Python | `pyproject.toml`, `setup.py`, `setup.cfg` | the outermost package |
| `rust` | Rust | `Cargo.toml` | the outermost Cargo workspace root |

Source of a family that lies outside every project of that family forms one
extra unit at the repository root — except Rust, where a directory without a
`Cargo.toml` produces an empty graph that is indistinguishable from a crashed
helper, so loose `.rs` files are left unanalysed rather than published as an
empty unit.

These rules are parity measurements, not preferences
(`docs/research/10-round3-empirical.md` §5–§8): splitting one TypeScript
project four ways kept 99.7% of control-dependence edges but only 46% of the
calls that resolved to internal methods; Python packages keep 100% of methods
and dependence edges and alias their cross-package calls by full name; a
whole-repository parse of a multi-module Go workspace covered 36 files because
the frontend reads `go.work`'s root module only.

A unit's files are materialized privately, and the nested projects it does not
own are pruned from that private copy before the engine reads it, so a module
inside another module is analysed once.

## The cache

The parsed graph is kept under `<data_dir>/dependence/graphs/` and reused
across generations, keyed on the complete semantic closure:

* every source file path and content hash of the unit,
* every manifest and lock file it owns (`go.mod`/`go.sum`,
  `package.json`/lockfile/`tsconfig.json`, `pyproject.toml`/`requirements*`,
  `Cargo.toml`/`Cargo.lock`, `compile_commands.json`, `CMakeLists.txt`),
* the pinned frontend argv including the definition cap,
* the unit's root and the nested projects it excludes,
* the engine payload digest and the runtime payload digest.

Any of these changing invalidates the entry. `providers.dependence.cache_bytes`
bounds the directory; retention evicts least recently used first, and `0`
disables caching rather than making it unbounded.

A graph whose parse skipped methods at the definition cap is **not** cached.
What was skipped exists only on that parse's standard error, and a reused graph
carries no trace of it, so a cache hit would republish `data_flows_to` as
`fresh` for a unit whose data dependence is missing whole method bodies. A
reused graph never publishes a capability fresher than the run that produced
it; the cost is that such a unit reparses every generation, which the pinned
definition cap makes rare.

## Memory

There is **no default memory ceiling**. A reservation orders and serializes
work; it never refuses it.

```text
reservation = heap cap + per-family resident allowance + helper allowance
heap cap    = clamp(unit_memory_floor_bytes,
                    unit source bytes x per-family estimate,
                    machine-derived allocation, unit_memory_ceiling_bytes)
allocation  = MemAvailable - base footprint - safety margin
```

* The cap is placed on the engine's frontend heap. It is lossless everywhere
  it succeeds: on five large repositories a capped run produced the same facts
  as the default run, or no graph at all (§4).
* A heap cap is not a memory cap. The per-family allowance is the resident
  memory the frontend keeps outside the heap, measured in §10: C/C++ 2.6 GB,
  Python 1.9 GB, Go 0.3–0.5 GB, Java 0.1–0.4 GB, TypeScript/JavaScript 0.3 GB,
  Rust 0.25 GB plus a fixed ~0.8 GB helper outside the heap.
* Export gets its own, smaller cap and its own reservation: it scales with the
  graph, not the source, and the export is deleted after import.
* `unit_memory_ceiling_bytes = 0` means machine-derived. Only an explicit
  non-zero value rejects a unit before it runs.
* On a host that does not publish available memory, the allocation is reported
  as unavailable, not as zero: the unit's own estimate stands and only an
  explicit ceiling bounds it. Inventing a bound there would be a default
  memory ceiling by another name.
* Out of memory is retried **exactly once**, at the machine-derived
  allocation, and only when that allocation exceeds the cap that failed. A
  retry at the same cap cost 100 s on a 1.05M-line Python tree and could not
  have succeeded.
* Units are never split for memory and no analysis limit is ever lowered to
  make one fit. `max_concurrent_heavy_analyzers` (default 1) and the summed
  reservations are the coordinator's scheduling inputs; the provider exposes
  the reservation and runs what it is given.

## Failure classes

Every one of these was reproduced against the real engine.

| Class | Signal | Outcome |
|---|---|---|
| `memory` | `OutOfMemoryError` on stderr, non-zero exit, no graph | `CTX_RESOURCE_LIMIT` with `heap_cap_bytes`, `allocation_bytes`, `estimated_bytes` and, when the tree was sampled, `observed_peak_bytes`. One retry, then fail closed. |
| `engine` (pass crash) | `Pass <name> failed in <n> ms` at WARN with the throwable | `CTX_PROVIDER_OUTPUT_INVALID` with `pass` and `exception`. No retry: it reproduces. Siblings are unaffected. |
| `engine` (zero-exit helper crash) | `Process exited with code <n>` on stderr, **or** an export with no methods for a unit that has source, **or** a clean exit that left no graph | same code. The exit status is a lie in this mode; the empty result is the only honest signal. |
| `timeout` | the step exceeded the unit deadline | `CTX_PROVIDER_TIMEOUT`. |
| definition-cap skip | paired `<method> has more than <n> definitions` and `Skipping.` WARN lines | **not** a failure: the unit seals and `data_flows_to` is published `partial` with the exact count and the method names. |

The out-of-memory test is applied before the helper-crash test, because an
out-of-memory stderr carries the `Process exited with code` line too — but
only for a run that actually failed. The marker is a substring match over the
bounded stderr, so a run that exited 0 and left a good graph can carry it from
an out-of-memory the engine caught and logged, or from a source path or method
name containing the word. Such a run is not `memory`: it falls through to the
engine/none decision, which the graph-presence check then resolves. Reading it
as heap exhaustion would spend the unit's single retry on a full reparse of a
unit that had already succeeded.

Classification depends on the engine logging at WARN, so the child environment
pins its log level rather than inheriting whatever the host set.

Every result is validated for non-emptiness before admission, and a failed
unit leaves no facts: `provider.RunUnit` deletes everything the run wrote and
the base generation is untouched.

## Subdivision

Subdivision is the last-resort recovery from a **reproducible** engine crash
and is never used for memory.

1. The full frontend-native unit always runs first.
2. On a crash the same unit is rerun once with the frontend's fixed
   semantics-neutral option allowlist. That list is **empty for all six
   frontends today**: every option this release offers changes results, so
   nothing can be added without changing what a success would mean. The rerun
   therefore proves only that the crash is deterministic rather than transient,
   which is exactly what it is for.
3. Only then is the unit split along the next frontend-native boundary, and
   each part that produces a live export is imported into the same unit.
   Two parts legitimately describe the same entity — above all the external
   stub of a callee both of them reference — and storage keys a node fact by
   (unit, node), so a repeated identity would fail the whole unit and lose
   every fact of every part. The parts therefore share one dedupe sink for the
   unit's lifetime: the first part to publish an identity writes it and later
   repeats are dropped. The seen set is on disk, inside the run directory, so
   a large unit does not hold hundreds of thousands of identities in memory,
   and dropping a repeat never breaks the put-order rule, because the identity
   it names was already written.
4. Every capability the subdivided unit publishes is `partial`, with the
   failed unit id and the backend failure (`<pass>/<exception>`). Control and
   data dependence survive splitting almost intact; engine `calls` keep under
   half of their resolved targets, so consumers that need calls should use the
   syntax-plus-SCIP path, which never depended on the engine.
5. If no part produces an honest result, the unit is `failed: engine`.

## `providers.dependence.enabled` and the coordinator

`enabled` is the coordinator's decision, not the provider's. The provider has
no notion of it.

| Value | Behaviour |
|---|---|
| `auto` (default) | nothing runs before the base generation is active; every dependence unit is then enqueued as low-priority background work under the resource governor. A query that needs a dependence fact promotes its units to the front of that queue and is answered with a typed `pending` capability until they seal. |
| `true` | the index blocks on the dependence units. |
| `false` | the provider never runs; its capabilities are `unavailable` with `CTX_PROVIDER_UNAVAILABLE` while the base generation stays `fresh`. |

The `pending` payload the coordinator publishes while a unit has not sealed
carries the capability and scope, the number of units the answer waits on, the
promoted unit's position in the queue, and an estimate. It is a capability
state, never a fact: a `pending` answer never returns a partial graph.

A sealed dependence unit never mutates the active generation. The coordinator
builds a new generation from the active generation's units plus the sealed
unit, validates it, and activates it atomically through the same path a
refresh uses (Section 13.1). Seals landing in one scheduler tick coalesce into
one generation, and a `pending` answer becomes a real answer exactly at
activation. A unit that is being refreshed after an edit answers `stale` with
a provenance distance, not `pending`, and its previously sealed build is
carried into the new generation until the fresh one replaces it (Section 13.3).

## Refresh and delta

The engine has no incremental mode, no merge and no per-file export
(joern#5757), so a refreshed unit is a whole parse and export — unless the
cache key still matches, in which case nothing runs at all.

What is wired today, exactly. Every import derives an id-independent semantic
key per fact (the fact label, its owning method's full name, its file, the
operator it was lowered from, its target name and its ordered byte ranges;
`<clinit>`-owned facts use a digest of their endpoints' source text instead of
their coordinates, because the Go frontend shuffles those between two parses
of identical source). The importer streams that key set to a sorted file,
diffs a supplied previous set against it in one merge pass, and can publish
only the relations whose key changed. Deriving the keys measured 1–5 s per
unit against 17–65 s engine runs, and a one-line edit changes about one fact
row in ten thousand.

What is **not** wired today. The key set is derived on every import but not
kept: the provider names no path for it, so it is written beside the staging
database and removed with it. It is not kept because there is nowhere to keep
it — a key set belongs to the *stored* unit, and storage has no fact-key
column. The provider likewise passes no previous key set, so every import is
a full one and the whole export is published. This is
deliberate, not an omission: the delta's other half is a storage operation —
the fresh export is diffed against the **stored** unit and the unchanged rows
are carried into the new unit — and `UnitWriter` has no carry-over or
delete-by-key path yet. Suppressing the unchanged relations before storage can
carry them over would publish a unit holding only the rows that changed, which
is a unit missing most of its facts. The provider therefore stays honest and
full until storage can hold up its end; the step is the coordinator's
(Task 12), and the key algebra is versioned so a stored set built by an older
algebra can never be mis-diffed against a fresh one.

## Privacy and cleanup

Materializations, graphs not selected for the cache, exports and the
importer's staging database are removed on every termination path. All of them
live under the provider's own private roots — `<data_dir>/dependence/runs/` for
the per-run directories and `<data_dir>/dependence/scratch/` for the staging
databases — never under the system temp directory: the staging database holds
source-derived graph content and was measured at 649 MB for one 64 MB export.

`defer` covers every return and every panic but not a `SIGKILL` or a power
loss, so the provider sweeps both private roots when it is constructed,
before any unit runs. A workspace has one cross-process owner, so nothing else
can hold a run of this provider at that moment and everything found there is
stale. The scan is bounded, and an entry that cannot be removed is logged and
skipped rather than failing construction. The child's stdout is discarded and its stderr is
read only for classification: no raw analyzer output, no source body and no
inherited environment reaches an ordinary log. Process-tree metrics are
recorded as their own log fields, separate from the base index's accounting.
