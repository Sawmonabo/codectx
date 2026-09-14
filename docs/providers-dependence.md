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

Each published capability carries its own state at the unit's scope, and a
state that is not `fresh` carries the machine-readable particulars of why in
bounded `details` pairs: `skipped_methods` (the exact count) and
`skipped_method_names` (a sample, whole names only) on a `partial`
`data_flows_to`, `subdivided` and `backend_failure` on every capability of a
subdivided unit. Export rows the importer recognised but does not map are
reported once, under the `unsupported_labels` pseudo-capability, `unavailable`,
with a label-to-count map ordered by count and an `untracked_labels` count for
whatever did not fit. The particulars are never encoded into extra capability
rows whose *name* is the detail text: that grows a bounded list with the size
of the repository and claims a capability identity for something that is not a
capability.

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
  method removed, and no change to any other fact count beyond the run-to-run
  variance below — the measured CDG and CALL counts were equal
  (`docs/research/10-round3-empirical.md` §9a). It is part of the cache key and
  there is no second parse at a higher limit.
* The single `all` export carries every edge family the importer reads
  (`CALL`, `CDG`, `REACHING_DEF`, `CONTAINS`). `--repr=pdg|cdg|ddg` is not
  implemented for CSV or GraphML in this release; there is no GraphML path.
* The engine's argument parser rejects a repeated option, so the
  semantics-neutral option allowlist must never restate a pinned one.

**The engine is not run-to-run deterministic.** Two runs of the same pinned
argv over the same unmodified 161-file tree (this repository, Go frontend)
produced `nodes=13675 relations=52310 aliases=16922` and
`nodes=13677 relations=52311 aliases=16926` — a band of about 0.01%, also seen
as CDG −4 / REACHING_DEF −6 on a 1.5M-line Java repository
(`docs/research/10-round3-empirical.md` §4). Nothing in the provider assumes
two runs are equal: the graph cache replays a stored graph rather than
reparsing, fact keys are derived from source-side identity rather than from
engine node ids, and every parity claim in this document and in the code is
bounded by this band. A claim of *equality* between two engine runs anywhere
in the repository is a defect.

**No version probe.** `joern-parse --version` is rejected as an unknown option
and `joern --version` drops into the interactive console, so the engine's
version and payload digest come from the lock entry that installed it and
travel on `Detection.ObservedVersion` and the descriptor version. The lock
entry's *name* stays in the lock: `ObservedVersion` renders
`engine <version> <digest>`, because detection is a product surface.

### How the payload is resolved

The engine is one entry of the embedded tool lock, installed and verified by
`internal/toolchain`. The backend never looks on `PATH`, never probes and never
fetches anything itself: it is handed an `Engine` by a locator, and the
production locator is the toolchain resolver behind that interface.

* **Parse argv** is the resolved payload's own launcher prefix. The pinned
  entry is a launcher script that finds its JVM through `JAVA_HOME`, which the
  resolved payload carries in its environment and which points at the **managed
  JDK the lock pins** — no host Java is used or needed.
* **Export argv** is the same prefix with the entry's base name substituted,
  so it keeps the platform's extension (`.bat` on Windows). The export tool is
  not the lock's pinned entry, so the resolver does not re-hash it at every
  resolution: it is covered by the payload digest checked at install. The
  locator therefore checks it for presence, regularity and the executable bit
  and refuses with `CTX_TOOL_CORRUPT` otherwise, so a payload damaged after
  installation is a typed refusal rather than an exec failure in the middle of
  a unit.
* **Digest** is the payload's `Tool.Fingerprint()` — the tool name, version,
  pinned payload digest and the executables observed at this resolution. It is
  used rather than the bare payload digest because it is never empty: a
  `[tools.override.<name>]` has no pinned payload digest at all, and an empty
  digest makes the backend refuse the engine as incomplete. **RuntimeDigest**
  is the managed JDK's fingerprint, which Section 11.6 folds into the unit's
  semantic closure — the same engine on a different runtime is not the same
  analysis.
* A resolution that fails surfaces the toolchain's own code
  (`CTX_TOOL_OFFLINE`, `CTX_TOOL_UNSUPPORTED_PLATFORM`, `CTX_TOOL_CORRUPT`,
  `CTX_TOOL_OVERRIDE_INVALID`, `CTX_TOOL_FETCH_FAILED`,
  `CTX_TOOL_DIGEST_MISMATCH`), so "not installed" and "not available on this
  platform" are different answers.

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

A project whose scope key would exceed the identity bound is refused **a unit
of its own** by the planner, with a warning naming the family and the path.
The key is never truncated to fit: two deep directories sharing a long prefix
would cut to the same key, and every capability row, alias scope and cached
graph of one project would then be attributed to the other. Refusing in the
planner keeps the decision where the units are chosen rather than failing the
unit once it has already begun.

**Its source is not dropped.** A refused directory is excluded from nothing, so
its files fall back to the unit that encloses them — the enclosing project, or
the family's repository-root unit. No file of a family is ever orphaned, and
the family always has a unit that runs. It is still a degradation: that source
is analysed at a coarser project boundary than it owns, with the neighbouring
projects' files around it, and resolution depends on a project's extent. So
every capability the family publishes is `partial` with
`CTX_PROVIDER_OUTPUT_INVALID` and an `unplanned_projects` count in its
`details`. The family is never reported fresh while that is true.

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

A unit's files are materialized privately by membership: the copy is made file
by file and only the files the unit owns are ever written, so a module inside
another module is analysed once, by its own unit. Nothing is copied and then
deleted — the nested project's source never enters this unit's private tree at
all, and the sibling projects cost neither the copy time nor the disk.

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
  as the default run within the engine's run-to-run variance — no systematic
  loss and no fact class missing — or no graph at all (§4).
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
  allocation, and only when more memory is actually available: the allocation
  must exceed the cap that failed, **and** the analyzer tree's observed peak on
  the failed attempt must be below the allocation. A retry with no more memory
  behind it cost 100 s on a 1.05M-line Python tree and could not have
  succeeded. Where the platform does not sample the tree peak, only the first
  condition applies; an unsampled peak is absent, never zero.
* Units are never split for memory and no analysis limit is ever lowered to
  make one fit. `max_concurrent_heavy_analyzers` (default 1) and the summed
  reservations are the coordinator's scheduling inputs; the provider exposes
  the reservation and runs what it is given.

## Failure classes

Every one of these was reproduced against the real engine.

| Class | Signal | Outcome |
|---|---|---|
| `memory` | `OutOfMemoryError` on stderr, non-zero exit, no graph | `CTX_RESOURCE_LIMIT` with `heap_cap_bytes`, `allocation_bytes`, `estimated_bytes` and `observed_peak_bytes`. One retry, then fail closed. |
| `engine` (pass crash) | `Pass <name> failed in <n> ms` at WARN with the throwable | `CTX_PROVIDER_OUTPUT_INVALID` with `pass` and `exception`. No retry: it reproduces. Siblings are unaffected. |
| `engine` (zero-exit helper crash) | `Process exited with code <n>` on stderr, **or** an export with no methods for a unit that has source, **or** a clean exit that left no graph | same code. The exit status is a lie in this mode; the empty result is the only honest signal. |
| `timeout` | the step exceeded the unit deadline | `CTX_PROVIDER_TIMEOUT`. |
| definition-cap skip | paired `<method> has more than <n> definitions` and `Skipping.` WARN lines | **not** a failure: the unit seals and `data_flows_to` is published `partial` with the exact count and a sample of the method names in its `details`. |

Every failure carries `observed_peak_bytes`: the peak of the summed resident
memory over the whole analyzer tree, sampled every 250 ms while it ran. It is
the tree sum at one instant, never a sum of per-process high-water marks
reached at different instants, and it is therefore higher than
`/usr/bin/time %M`, which reports the largest single process — by tens to
hundreds of megabytes on this engine, whose orchestrator JVM, frontend and
helpers are separate processes (`docs/research/10-round3-empirical.md` §1).
The figure is what the memory governor's estimate is checked against. Where
the platform cannot observe a running tree — anything but Linux today — the
detail is absent and a memory failure says so under
`observed_peak_bytes_unavailable`; it is never published as a zero, which
would read as an analysis that used no memory.

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
   is therefore the same argv over the same source, and since the engine is not
   run-to-run deterministic it yields a *second observation of the same failure
   class*: that raises the odds the crash is deterministic rather than
   transient without proving it. A crash seen once is never split on.
3. Only then is the unit split along the next frontend-native boundary, and
   each part that produces a live export is imported into the same unit.
   Two parts legitimately describe the same entity — above all the external
   stub of a callee both of them reference. Storage keys a node fact by
   (unit, node); it admits a repeat of that key whose stored columns are
   identical and refuses one whose columns diverge with
   `CTX_PROVIDER_OUTPUT_INVALID`. Nothing in the provider drops or rewrites a
   repeat: what the import publishes for one identity is a function of that
   identity alone, so the parts' repeats are identical and cost nothing, and a
   divergence — the parts disagreeing about one entity — fails the unit where
   it can be seen instead of being absorbed into a silently truncated one.
4. Every capability the subdivided unit publishes is `partial`, carrying the
   failed unit id under `subdivided` and the backend failure under
   `backend_failure` (`<pass>/<exception>`) in its `details`. Control and
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

What is wired today, exactly. Every import derives an engine-id-independent
semantic key per fact (the fact label, its owning method's full name, its
file, the operator it was lowered from, its target name, its ordered byte
ranges and — for a relation — its two endpoints' published identities;
`<clinit>`-owned facts use a digest of their endpoints' source text instead of
their coordinates, because the Go frontend shuffles those between two parses
of identical source). The endpoints are part of the key because a relation's
published identity is derived from them and a located declaration's identity
is derived from its declaration range: edit a callee and every call edge into
it moves to a new identity while the site's own file, owner, target name and
byte range do not move at all. Without them one key would name two different
facts across a refresh, the previous row would be carried under a key the
fresh run still publishes, and the unit would hold an edge whose endpoint no
longer exists. A node fact takes no endpoints component — its identity
already tracks through its file and its coordinates, and an identity minted
from a declaration range would reintroduce the initializer nondeterminism the
coordinate rule exists to defeat. One measured pair of independent engine runs
over one unchanged package of this repository published the same 6842 keys,
none changed and none removed — one sample, not a determinism guarantee: the
engine is not run-to-run deterministic (see §The engine for its variance
band), and what this measures is that the variance did not reach the key
algebra on that pair. The importer streams that key set to a sorted file,
diffs a supplied previous set against it in one merge pass, and can publish
only the relations whose key changed. Deriving the keys measured 1–5 s per
unit against 17–65 s engine runs, and a one-line edit changes about one fact
row in ten thousand.

Every fact reaches storage with its keys. A published fact carries *every*
key behind it, not one: a node identity two entities resolved to carries both
entities' keys, and a canonical edge carries the key of each of its
occurrences, including the occurrences past the evidence bound. Storage drops
a carried fact when **any** of its keys is replaced, so a key left off would
strand the row it backs — the next refresh would classify that key as changed,
storage would keep the old row under a key the fresh fact never names, and the
unit would hold two descriptions of one identity. The list is sorted and
deduplicated because storage refuses an unsorted one, and there is no bound on
its length beyond the record-byte accounting every fact is already charged
under, which fails closed with `CTX_RESOURCE_LIMIT`. Measured on a real
engine export of one package of this repository (973 node facts, 3389
relations, 6865 keys): at most 5 keys behind one node fact and 51 behind one
relation.

A delta import filters whole relations, never occurrences: an edge is
republished if any of its occurrences carries a changed key, and it is then
republished entire, so a delta-built unit is row-identical to the same unit
built in full. And an import whose previous key set *lost* a key republishes
every relation. A removed key is a digest that cannot be mapped back to the
edge it backed, and that edge may still exist — delete one of two identical
calls and the edge keeps an unchanged key, gains no changed one, and has had
its previous row dropped by the carry-over — so the sound rule is to
over-emit. It costs import work and never correctness. Node facts are outside
this: they are published whole on every run.

**What ships.** The delta is wired end to end by the coordinator's
`internal/index/delta` applier, not by the provider: the provider exposes
`ImportOptions{PreviousKeys, KeysPath}` and reports `Report.Keys`, and the
applier decides where a stored unit's delta state lives, reads the previous
unit's set back and calls storage's carry-over.

- **Stored state.** One kind, `"dependence.fact_keys"`: the sorted key set the
  sealed unit published, written with the unit through
  `UnitWriter.PutDeltaState` and read back with `Store.DeltaState`. The unit's
  *declared inputs* are not stored a second time — the applier merge-joins
  `Store.UnitInputs(previous)` against this unit's own input stream.
- **The replaced set.** `Replaced.Keys` is the changed keys plus the removed
  ones, streamed straight out of `KeySet.Diff`. `Replaced.Files` is the merge
  join above: every file the predecessor declared that this unit does not
  declare with the same bytes. `Replaced.Scopes` is deliberately empty — nodes
  and their aliases are emitted whole on every dependence import, so naming the
  unit's scope would delete the aliases of every identity that survived.
  `Replaced.IndexLevel` is set exactly when the emit was unfiltered and
  therefore republished the facts that name no file.
- **When `PreviousKeys` is supplied, and when it is withheld.** Only when that
  replaced-file set is empty. Naming an edited path in `Replaced.Files` drops
  the predecessor's evidence in that path, and a filtered emit never rewrites
  the relations in it whose keys did not move, so the unit would be missing
  rows a full build holds; not naming it trips storage's carried-input check.
  An edit therefore has no sound filtered form, and the applier withholds the
  previous keys, which makes the import emit every relation — always correct,
  and it costs import work only.

The consequence is worth stating plainly: **only an addition-only refresh
inherits rows.** A refresh that changes or removes any declared file does a
full build's import work and carries nothing, even though its predecessor was
present and usable. `Result.Filtered` is how a caller tells the two apart —
`Result.Carried` being all zero is indistinguishable from a predecessor that
had nothing to give. Measured through the applier against the real engine: an
edited file inherited nothing and published 14 relations, exactly as the full
build did, while an added file inherited 14 relations and 29 evidence rows and
published no relation of its own where the full build published 14 — both
row-identical to the full build of the same unit identity.

The key algebra is versioned, so a stored set built by an older algebra can
never be mis-diffed against a fresh one.

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
