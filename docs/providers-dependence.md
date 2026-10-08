# The `dependence` provider

`internal/provider/dependence` supplies fallback call facts for the nine
supported languages. It is the Section 11.6 provider: extraction-lazy, cached,
governed per unit, and honest about every way an analysis can come back
incomplete. It publishes `calls` only, and only until the package-scoped
linking provider of ADR-0012 decision 9 publishes them; the provider and its
engine are then deleted.

The four file-local dependence families, `control_depends_on`,
`data_flows_to`, `reads` and `writes`, are not published here. The structural
provider computes them in process for every language and publishes them per
file, at `static_analysis` (docs/providers-treesitter.md, Dependence facts).
The importer drops them for every language: it derives no control dependence,
no def-use walk and no assignment reads or writes from the export, and so
publishes no `may_refer_to` for an assignment target it could not resolve.

The analysis engine behind it is identified by its tool lock entry and its
licence record. Everywhere the provider speaks — the provider id, capability
names, evidence details, error details, log fields, query results, and this
document — it is "the engine".

## What it publishes

| Capability | Relation | Evidence detail |
|---|---|---|
| `calls` | `calls` | `call`, `call speculated` |

Every fact is `static_analysis` precision with exact byte ranges. The
descriptor, detection and every unit's capability rows name `calls` alone
(`dependence.Capabilities` in `internal/provider/dependence/failure.go`). An
entity whose resolution is ambiguous still publishes its equally supported
alternatives as `may_refer_to` edges from the chosen identity.

Where a call site names a callee the engine could not find, the engine invents
one: a method with no definition anywhere in the graph, emitted so the site has
a target. The import keeps it — it is how a call into another unit binds by
full name — and says what it is. Its call edge carries the `call speculated`
detail instead of `call`, and the callee node carries
`{"resolution":"speculated","candidates":1}` where a declaration from outside
the unit carries `{"resolution":"import","candidates":1}`.

An invented callee is recognised by the export's own stub signal, over the
whole graph, and never by the one namespace the engine parks some of them
under. It takes two shapes: the engine either parks the callee in its
speculated namespace, or — when the site is inside code the engine did parse —
parks it under the enclosing program or type, named after the value that was
called. The second is marked by what the graph itself holds: the method has no
coordinates and no definition, while the scope that declares it is defined in
this graph, with coordinates, so a definition would have been there to find. A
declaration from another unit is not marked, because its own scope is a stub
with no coordinates either — which is what makes its name one from outside this
graph rather than a guess inside it. Marking by the namespace alone would
publish the second shape as a real import: a dependency of the source that the
source does not state.

A graph answer
(`codectx_callees`, `codectx_callers`) returns nodes and relations and never
the evidence behind them, which is why the node itself has to say it: without
that, an invented callee reads as a real dependency of the source.

A call through a method taken as a value — assigned to a variable, passed as
an argument — is **not** a `calls` edge to the referenced method. The engine
binds such a call site to an invented callee named after the variable
(JavaScript) or to no callee at all (Python), never to the method the value
holds, so a `calls` edge naming it would be this product's inference rather
than a fact the analysis produced.

The `calls` capability carries its state at the unit's scope, and a state that
is not `fresh` carries the machine-readable particulars of why in bounded
`details` pairs: `subdivided` and `backend_failure` on a subdivided unit, and
`unanalysed_files` on a unit with a source file the frontend was handed and did
not read (see the default path exclusions below). Export rows the importer
recognised but does not map are reported once, under the `unsupported_labels`
pseudo-capability, `unavailable`, with a label-to-count map ordered by count
and an `untracked_labels` count for whatever did not fit. The particulars are
never encoded into extra capability rows whose *name* is the detail text: that
grows a bounded list with the size of the repository and claims a capability
identity for something that is not a capability.

## The engine

The backend is a code-property-graph engine pinned by the Section 11.7 tool
lock as its `kind: cpg` entry (runtime `jdk`, Apache-2.0; recorded in
`THIRD_PARTY_LICENSES.md`). The version verified for
this implementation is **4.0.627**.

Two noninteractive commands, one exact argv each, no product-owned analysis
script and no interpreter server: a **parse** that is given the unit's
frontend, the definition cap, the unit's private materialization and a private
output graph, and an **export** that writes every representation of that graph
as CSV into a private export directory.

* The Java parse alone is also given the option that empties its frontend's
  default path exclusions; see the default path exclusions below for why no
  other family is given it.
* The definition cap is 40000, replacing the engine default of 4000. Measured
  on a 1.05M-line Python tree: 23% more parse time, 3% more memory, every
  skipped method removed, and no change to any other fact count beyond the
  run-to-run variance below — the measured control-dependence and call counts
  were equal (`docs/research/10-engine-empirical.md` §9a). It is part of the
  cache key and there is no second parse at a higher limit.
* The single export of every representation carries every edge family the
  importer reads (calls and containment). The narrower dependence-only representations are not
  implemented for CSV or GraphML in this release; there is no GraphML path.
* The engine's argument parser rejects a repeated option, so the
  semantics-neutral option allowlist must never restate a pinned one.

**The engine is not run-to-run deterministic.** Two runs of the same pinned
argv over the same unmodified 161-file tree (this repository, Go frontend)
produced `nodes=13675 relations=52310 aliases=16922` and
`nodes=13677 relations=52311 aliases=16926` — a band of about 0.01%, also seen
as control dependence −4 / reaching definitions −6 on a 1.5M-line Java
repository
(`docs/research/10-engine-empirical.md` §4). Nothing in the provider assumes
two runs are equal: the graph cache replays a stored graph rather than
reparsing, fact keys are derived from source-side identity rather than from
engine node ids, and every parity claim in this document and in the code is
bounded by this band. A claim of *equality* between two engine runs anywhere
in the repository is a defect.

**No version probe.** The parse command rejects a version option as unknown
and the engine's console launcher drops into its interactive console, so the
engine's version and payload digest come from the lock entry that installed it and
travel on `Detection.ObservedVersion` and the descriptor version. The lock
entry's *name* stays in the lock: `ObservedVersion` renders
`engine <version> <digest>`, because detection is a product surface.

### How the payload is resolved

The engine is one entry of the embedded tool lock, installed and verified by
`internal/toolchain`. The backend never looks on `PATH`, never probes and never
fetches anything itself: it is handed an `Engine` by the locator, which is the
toolchain resolver in production.

**Nothing is installed when the workspace is opened.** Construction resolves the
payload only if the store already holds it (`Resolver.ResolveInstalled`). A
payload the lock pins but the store does not hold is reported as absent, and the
backend keeps the payload's *pinned* identity — `Resolver.PinnedFingerprint`,
computed from the lock alone — so the descriptor version and the Section 11.6
cache key are the same string whether the payload landed before the process
started or during it. The first `Parse` or `Export` resolves the command lines,
and that is what installs the payload: the first unit that actually needs the
engine pays the fetch, at unit time, under the scheduler's reservation gate.
Resolving at construction would make every `codectx index`, `refresh` and
`watch` download roughly two gigabytes before the snapshot is even captured,
in every repository, whether or not the planner emits a dependence unit —
which is exactly the delay to base readiness Section 11.6 forbids. The
resolution is memoized with its error, so a payload that cannot be installed is
attempted once rather than once per language family, and the resolved digest is
re-checked against the identity construction already published.

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
| `964655bd…` (the engine's release archive, SHA-256 verified locally) | 4.0.627 |

The lock is the authority; this table is the human-readable index that
`status`, `doctor` and each provider's reported `ObservedVersion` resolve against.

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
(`docs/research/10-engine-empirical.md` §5–§8): splitting one TypeScript
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
* the engine payload digest and the runtime payload digest,
* the provider version and the analysis configuration hash — the two
  components of the unit's own identity that nothing above carries, so a
  provider upgrade or a configuration edit that rebuilds the unit can never
  reuse a graph produced under the version or the configuration it replaced.

Any of these changing invalidates the entry. `providers.dependence.cache_bytes`
bounds the directory; retention evicts least recently used first, and `0`
disables caching rather than making it unbounded.

A graph is stored only once its **export has proved it alive**, never when the
parse ends. Storing it at the end of the parse would cache a graph the engine
cannot export, under exactly the key a clean run produces, so every refresh
would find the entry, skip the parse and re-pay the dead export. For the same
reason the entry is **dropped** when an export proves the graph produces
nothing — the engine died
writing it out twice on its own exception, or both steps exited cleanly over a
unit with source and the export carried no method. Without that, every lookup would touch the dead entry
again and hold it at the most-recently-used end of the budget for as long as
the unit is refreshed.

A graph whose parse skipped methods at the definition cap is cached like any
other: the skip removes only a method's data dependence, which this provider
does not publish, so a reused graph publishes exactly what the run that
produced it did.

## Memory

There is **no memory ceiling**. A reservation orders work; it never refuses
it.

```text
reservation = heap cap + per-family resident allowance + helper allowance
heap cap    = clamp(unit_memory_floor_bytes,
                    unit source bytes x per-family estimate,
                    machine-derived allocation)
allocation  = min(MemAvailable - base footprint - safety margin,
                  MemAvailable / 2)
```

The base footprint is derived from the machine and the configuration -- this
process's own resident set, read once at load, plus the query, cache and queue
reservations and every page cache the process opens
([configuration](configuration.md)) -- so a host with more cores keeps more for
itself and offers its children less.

* The cap is sized to what the unit needs, never to what the machine has. A
  frontend grows toward whatever cap it is given and does not need it: a
  157 MB JavaScript project of 4,984 files parsed in 3 m 38 s under a 4 GiB
  cap and in 3 m 41 s with no cap at all, while the process tree's peak
  resident memory rose from 5.4 GB to 9.7 GB at 8 GiB and to 14.3 GB at
  16 GiB — for exports whose method, call, control-dependence and
  data-dependence counts were identical at every cap. A 2 GiB cap on the same
  project failed closed. Sizing a cap from the machine therefore buys nothing
  and serializes every other unit behind a reservation nothing uses. A soft
  heap ceiling was measured as an alternative and is not one: under a 16 GiB
  cap with a 1 GiB soft ceiling the same tree still peaked at 3.8 GB against
  the 1.2 GB a real 1 GiB cap produced, and adding periodic collection and
  aggressive free ratios moved it to 3.3 GB while costing time. The hard cap
  is the only thing the runtime honours.
* The allocation leaves the host at least **half** of what was available when
  the run began. The product runs beside the editor, the agents and the
  browser of the person indexing their repository; an allocation of
  "everything but the safety margin" handed one analyzer a 42 GB heap cap on
  a 47 GB machine. It is a design constant, not a setting, for the same
  reason the product has no default ceiling: it decides how the product
  shares a machine, not how much work it will do. A unit whose estimate
  exceeds even the allocation still runs, whole, at the allocation.
* The cap is placed on the engine's frontend heap. It is lossless everywhere
  it succeeds: on five large repositories a capped run produced the same facts
  as the default run within the engine's run-to-run variance — no systematic
  loss and no fact class missing — or no graph at all (§4).
* A heap cap is not a memory cap. The per-family allowance is the resident
  memory the frontend keeps outside the heap, measured in §10: C/C++ 2.6 GB,
  Python 1.9 GB, Go 0.3–0.5 GB, Java 0.1–0.4 GB, TypeScript/JavaScript 0.3 GB,
  Rust 0.25 GB plus a fixed ~0.8 GB helper outside the heap. The
  TypeScript/JavaScript figure is the one that scales with the project rather
  than sitting flat: the syntax helper holds the whole project's trees outside
  the heap, and on the 4,984-file project above the tree ran 1.7 GB above its
  cap, so that is the allowance the family reserves.
* Export gets its own, smaller heap cap and its own reservation: it scales with
  the graph, not the source, and the export is deleted after import. Its size
  is not capped: an export the disk cannot hold is a disk that is full, which
  the store reports as one. While the child writes it, the runner hands the
  export to the disk one window at a time, so it reaches the disk as it is
  written rather than as one burst when the kernel's flusher wakes.
* Nothing rejects a unit for the memory it asks for. There is no setting that
  can, and the machine-derived allocation only sizes the cap.
* On a host that does not publish available memory, the allocation is reported
  as unavailable, not as zero: the unit's own estimate stands as its cap.
  Inventing a bound there would be a default memory ceiling by another name.
  Admission still has a finite bound on such a host — the scheduler stands in a
  conservative allocation for the observation the platform withheld — because a
  gate with no bound is not a gate.
* Out of memory is retried **exactly once**, at the machine-derived
  allocation, and only when more memory is actually available: the allocation
  must exceed the cap that failed, **and** the analyzer tree's observed peak on
  the failed attempt must be below the allocation. A retry with no more memory
  behind it cost 100 s on a 1.05M-line Python tree and could not have
  succeeded. Where the platform does not sample the tree peak, only the first
  condition applies; an unsampled peak is absent, never zero. The retry is
  re-admitted before it runs: the unit returns its grant and is admitted again
  at the larger reservation, behind whatever queued while it ran, because a
  reservation above the allocation is admitted only when nothing else holds
  one. That larger reservation is the unit's from then on; the export runs,
  and the unit is compared and recorded, against it.
* A unit has exactly one reservation. The planner sizes it from the unit's
  source bytes and the one machine reading the admission allocation was derived
  from, raised to the largest peak this workspace has recorded for the scope;
  the coordinator admits the unit at it and hands it to the provider, which
  runs every child under its caps and compares every step's peak against it.
  The provider never sizes a second one, except in the standalone
  provider-interface form that nothing admitted, which sizes it the same way.
* Units are never split for memory and no analysis limit is ever lowered to
  make one fit. The summed reservations against the machine-derived allocation
  are the whole of the coordinator's scheduling input — there is no count of
  analyzers — and the provider exposes the reservation and runs what it is
  given.
* What each unit was reserved, capped and observed to peak at is disclosed
  per unit in the `status --resources` accounting block. It is process
  accounting rather than a capability detail: an observed peak differs on
  every run, and a capability row's details fold into the analysis key, where
  two identical runs must key identically. The rows are as many as a bounded
  response carries; a unit past them is counted as omitted, and the count of
  units over their reservation includes the omitted ones, so it never
  under-reports. A step that crashed or timed out keeps its own failure class
  on its stage row; only a step that succeeded is marked over reservation.

## Failure classes

Every one of these was reproduced against the real engine.

| Class | Signal | Outcome |
|---|---|---|
| `memory` | `OutOfMemoryError` on stderr, non-zero exit, no graph | `CTX_RESOURCE_LIMIT` with `heap_cap_bytes`, `allocation_bytes`, `estimated_bytes` and `observed_peak_bytes`. One retry, then fail closed. |
| `engine` (pass crash) | `Pass <name> failed in <n> ms` at WARN with the throwable, **or** the untimed `Pass <name> failed` at ERROR that a pass which dies before it is timed leaves | `CTX_PROVIDER_OUTPUT_INVALID` with `pass` and `exception`. A parse crash that names **both** is taken as reproducible on first sight and is not re-parsed: it goes straight to subdivision. Siblings are unaffected. |
| `engine` (crash that names no pass) | `Process exited with code <n>` on stderr, a clean exit that left no graph, a signal death, or any non-zero exit with nothing said about a pass | same code. For the zero-exit helper crash the exit status is a lie and the empty result is the only honest signal. Nothing here identifies the defect, so the one confirmation below is kept before anything is split. |
| `empty_export` | both steps exited 0 and the export carries no method for a unit that has source | `CTX_PROVIDER_OUTPUT_INVALID` with `family` and `source_files`, the count of the unit's own source files the materialization handed the frontend. Not worded as a crash, because none happened, and not worded as itself either: the frontend is given every file of the unit, so three causes remain and the failure names all three — source holding no definition this family's frontend parses, a frontend whose own fixed rules drop every file of the unit (see the default path exclusions below), and a frontend that failed without reporting it. A unit whose every file is dropped fails here, before any import, so it carries no `unanalysed_files`; a unit whose files are only partly dropped seals and discloses them under that detail. |
| `engine` (no part survived subdivision) | a subdivided unit no part of which produced a method | same code, with `source_files`, `parts`, `parts_failed` and `parts_without_method`. The reason is that tally — never "no part produced an honest result", which restates the class. |
| `timeout` | the step exceeded the unit deadline | `CTX_PROVIDER_TIMEOUT`. |
| definition-cap skip | paired `<method> has more than <n> definitions` and `Skipping.` WARN lines | **not** a failure: the unit seals. The skip removes only a method's data dependence, which this provider does not publish, so no capability row carries it. |

### The frontends' default path exclusions, and how they are disclosed

A frontend can drop files it is pointed at before anything is parsed, by
fixed rules of its own. Measured on the pinned payload, with a file under a
`test` directory in each family's fixture: both steps exit 0 on all six
families, and whether that file reaches the exported graph is

| Family | Given the option | Not given it | What the frontend drops by its own rules |
|---|---|---|---|
| Java | read | dropped | the folders `.git`, `.mvn`, `.gradle`, `build`, `target`, `out`, `node_modules`, `.idea` and `test`, wherever they sit in the path — cleared by the option |
| JavaScript, TypeScript, TSX | rejected; dropped | dropped | the folders `node_modules`, `venv`, `docs`, `test`, `tests`, `e2e`, `e2e-beta`, `examples`, `cypress`, `jest-cache`, `eslint-rules`, `codemods`, `flow-typed`, `i18n`, `vendor`, `www`, `dist` and `build`; spec, mock, end-to-end and test script files; `test*.json`; build-tool configuration files; and minified or bundled files. No option reaches these rules. |
| C, C++ | accepted, not honoured; dropped | dropped | dot-folders, `test` and `tests` folders, and `CMakeFiles`. No option reaches these rules. |
| Go | read | read | nothing observed |
| Rust | read | read | nothing observed |
| Python | rejected; read | read | nothing observed |

**The Java option.** The option that empties the Java frontend's default set
is passed to the Java parse alone, as the last argument of its command line,
after the delimiter that hands everything following it to the frontend rather
than to the parse tool. It changes nothing for the other families: C/C++, Go
and Rust accept it and read the same files either way, and the
JavaScript/TypeScript and Python frontends reject it with an unknown-option
warning on standard error — the stream the failure classifier reads. It is
therefore kept out of their command lines, and out of their cache keys, which
fold each family's own argument array. Clearing the Java set also un-ignores
`build`, `target`, `out`, `node_modules` and the dot-directories. That is
correct here and not a widening: the frontend is never pointed at a checkout.
It is pointed at a private materialization holding exactly the files the unit
owns, so which files exist for it to read is the product's decision, taken
once, upstream.

**What no option reaches is disclosed.** The JavaScript/TypeScript and C/C++
frontends' own rules have no switch, so a unit of either family can hand the
frontend files that never reach the graph. The import does not copy those rules
to predict the drop. It compares the unit's own source files — the files of the
unit's family the materialization handed the frontend — with the export's file
nodes, as one join over the on-disk staging database, and every file with no
file node is counted. A unit with any such file publishes **every** capability
`partial` with `CTX_PROVIDER_OUTPUT_INVALID` and
`unanalysed_files = "<count> <first path>"`, the path being the first such file
in path order: no fact of any pass comes from those files. The count and the
path share one value because a busy row fills most of the bounded detail map,
and either half alone would not say what was missed. Because the comparison
reads the export's own record, it also catches a file a frontend skipped for a
reason nobody has measured, on any family.

A subdivided unit is counted over the union of its parts: each imported part
compares its own files with its own export, and one streaming pass over the
manifest counts the unit's files that lie under no imported part — files in the
unit's root directory, in a dot-directory the split does not descend into, or
in a part that failed or exported no method.

**What is run a second time, and what is not.** Heap exhaustion keeps the one
retry described above, and only when more memory is actually available. A
parse crash whose standard error names **both** the failing pass and the
exception class is a deterministic fault in that pass: re-running the same argv
over the same source only re-proves what the stderr already says, at the price
of a second full parse of a unit large enough to be worth splitting — three
minutes and twenty-two seconds of one measured run — so the unit is subdivided
on first sight of it. Every other engine crash — a signal death, a step that
left no graph, an exit that named no pass — may be the machine rather than the
source and keeps the one confirmation of Subdivision step 2. An export crash is
always confirmed, whatever it named: the first-sight rule is the parse's, and
re-exporting a graph that is already on disk costs an export rather than a
parse. A timeout is never re-run.

Every failure carries `observed_peak_bytes`: the peak of the summed resident
memory over the whole analyzer tree, sampled every 250 ms while it ran. It is
the tree sum at one instant, never a sum of per-process high-water marks
reached at different instants, and it is therefore higher than
`/usr/bin/time %M`, which reports the largest single process — by tens to
hundreds of megabytes on this engine, whose orchestrator JVM, frontend and
helpers are separate processes (`docs/research/10-engine-empirical.md` §1).
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

Every failure also carries `stderr_tail`: the last of what the child wrote to
its standard error, in whole lines taken from the end, bounded to what one
error detail holds. Each line is reduced before it is counted, so the bound can
never cut a path in two and leave its leading directories behind as text no
rule recognises. A final line longer than the whole bound keeps its last
bytes, from the first field boundary in them, because the exception is at the
end of the line. A failure row is durable storage, so what it may contain is
narrower than what a log may:

- A path starts at every separator that follows a byte which cannot continue a
  relative path — a space, a quote, a bracket, a backtick, `=`, `>`, `@`, `|`,
  `!`, `+`, a comma — so ``cmd: `<abs>/analyzer-parse` ``, `2><abs>/child.log`
  and `@<abs>/args` are all reduced, and the punctuation around the path
  stays. A path ends at whitespace, a closing bracket, a quote, `|`, a comma or
  a semicolon.
- The run's private directories (the unit's materialization and its work
  directories) are replaced by `(private)` wherever such a path starts, and
  the rest of the path is kept: it is the part that says which step of the run
  wrote the file, and it names something inside a directory the reader is not
  being told.
- A path under any other directory the backend knows is reduced to its base
  name, with that directory matched as a whole prefix first, so a known
  directory whose name holds a space is reduced whole. The known directories
  are the user's home directory, the directories of the engine's launchers and
  every absolute directory the child's environment names. Every other rooted
  path is reduced to its base name the same way.
- The backend is not told the data directory. A path under it that is under
  none of the directories above is reduced by the generic rule, and a space in
  a directory name the backend does not know ends the path there: what follows
  the space is left as the unrooted text it then is.

Classification depends on the engine logging at WARN, so the child environment
pins its log level rather than inheriting whatever the host set. The child's
environment is built by the product and inherits nothing, which also means it
inherits no locale: every child is given a UTF-8 one, because a C locale makes
the platform's path encoding ASCII and a runtime that encodes a file name
through it cannot open a source file whose name holds a letter outside ASCII
at all. One such file failed a 4,984-file project with the
runtime's invalid-path exception; the same project parsed and exported with
the locale set and nothing else changed. Setting the encoding as a runtime
property instead does not work and was measured not to: the runtime derives its
path encoding from the locale and ignores the property.

Every result is validated for non-emptiness before admission, and a failed
unit leaves no facts: `provider.RunUnit` deletes everything the run wrote and
the base generation is untouched.

## Subdivision

Subdivision is the last-resort recovery from a **reproducible** engine crash,
in the parse or in the export, and is never used for memory.

1. The full frontend-native unit always runs first.
2. A parse crash that named both its failing pass and its exception class is
   not rerun at all: the child's own diagnostics identify the defect, so the
   unit goes straight to step 3 on first sight of it. On any other crash the
   same unit is rerun once with the frontend's fixed
   semantics-neutral option allowlist. That list is **empty for all six
   frontends today**: every option this release offers changes results, so
   nothing can be added without changing what a success would mean. The rerun
   is therefore the same argv over the same source, and since the engine is not
   run-to-run deterministic it yields a *second observation of the same failure
   class*: that raises the odds the crash is deterministic rather than
   transient without proving it. A crash that said nothing about which pass
   died is never split on after a single observation.

   An **export** that dies on the engine's own exception is confirmed the same
   way — including one that named its failing pass, because the confirmation
   costs an export of a graph already on disk rather than a second parse — by exporting the graph already on disk a second time; this step has no
   neutral option to offer, so the confirmation is the same argv again. A
   project whose parse succeeds at every heap cap and whose whole-unit export
   dies at every one of them, while each of its subdivided parts exports
   cleanly, is a measured shape, not a hypothetical: reporting it as a failed
   unit would throw away every fact the engine could still produce for that project.
   Memory, the unit deadline and an export that exits cleanly holding no method
   keep their own paths and are never subdivided — the first two are properties
   of what the unit was given, the third a statement about the frontend's own
   exclusions.
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
   `backend_failure` in its `details`. That value is the failing pass and its
   exception class — `<pass>/<exception>`, or whichever of the two the crash
   named — followed in parentheses by how the provider established that the
   crash reproduces: `named pass and exception, taken on first sight` for a
   parse crash that identified itself, `failure class observed twice` for
   everything else, including every export crash. A crash that named neither
   publishes the decision alone. The decision travels inside this one value
   rather than under a key of its own because a subdivided row already fills
   most of the bounded detail map a provider may contribute, and a key dropped
   at the budget would tell an operator nothing exactly when the row is
   busiest. Control and
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

The engine has no incremental mode, no merge and no per-file export, so a
refreshed unit is a whole parse and export — unless the cache key still
matches, in which case nothing runs at all. That is the upstream project's own
answer, not an inference from its documentation: asked on its issue tracker
whether the parse step supports incremental builds, a maintainer answered "No,
it doesn't right now", and the issue was closed on 2026-08-28 as answered
rather than as planned; the separate feature request for incremental updates
has been open since 2026-03-08. Both issues are cited with their links in
`docs/research/11-incremental-joern.md`, which also records what was measured
against them.

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
  and it costs import work only. The two questions are answered by two
  independent walks of the coordinator's `Request.Inputs`, so the applier holds
  them against each other: a filtered emit chosen because the first walk saw
  nothing replaced, while carry-over's walk names a replaced file, is a
  `Request.Inputs` that is not re-iterable, and the unit is refused rather than
  sealed missing the rows that file's bucket held.

The consequence is worth stating plainly: **only an addition-only refresh
inherits rows.** A refresh that changes or removes any declared file does a
full build's import work and carries nothing, even though its predecessor was
present and usable. `Result.Filtered` is how a caller tells the two apart —
`Result.Carried` being all zero is indistinguishable from a predecessor that
had nothing to give.

Measured through the applier against the real engine, over one named fixture:
the Go fixture of the export reader's test data (the `pkg:go:` unit,
`go.mod` + `app/app.go` + `helper/helper.go`, 38 fact keys) as the predecessor,
refreshed after adding one file, `extra/extra.go`, declaring `func Note() int {
return 7 }`. The refresh kept its filter and inherited **14 relations and 29
evidence rows**; its own import published 11 nodes, 17 aliases and **0
relations**, and the unit it sealed — 11 node facts, 14 relation facts, 40 fact
keys, 40 evidence rows, 17 aliases — is row-identical to a full build of the
same unit identity, whose import published those same 14 relations itself. The
zero is a property of *that added file*, which declares no call, not of
addition-only refreshes in general: an added file that called something would
publish its own relations and inherit the rest. What generalises is the
row-identity, and that nothing was re-imported for the untouched files.

The key algebra is versioned, so a stored set built by an older algebra can
never be mis-diffed against a fresh one.

## The staging database

The importer stages the whole export in a private SQLite database under
`<data_dir>/dependence/scratch/`, a pooled surface per import, and derives the unit's
facts from it by
ordered query, so a fact is a function of the export's content and never of
the order its files were read. The staging is written the way a bulk load
writes: every table is appended in the order its rows arrive, with no
secondary index during the load, and every structure a later phase reads in
another order -- the node table keyed by the engine's integer node id, the
edge tables by source and by target, the per-node attributes, the locations,
the occurrences -- is built afterwards in one ordered pass of the engine's
external merge sort. No row is updated after it is written and nothing is
inserted into the middle of a b-tree larger than the cache. Occurrences are
derived in projection order, which already groups the occurrences of one
canonical edge, so the relation emission reads them front to back with no
sort; only the edges whose endpoint several entities resolved to, and
`may_refer_to`, take a second, sorted stream.

The result is that an import's disk traffic is a small constant times its
export -- 6.6× on the synthetic export of the importer's scale test, with a
staging cache small enough that every sort spills, against 34.5× for the
row-at-a-time staging [ADR-0009](adr/ADR-0009-import-staging.md) measured --
and that every byte is written once, sequentially, with a bounded window in
flight rather than in a burst at each commit. The page
cache of the staging database is `providers.dependence.staging_cache_kib`
(256 MiB by default): it bounds the memory one import holds for its staging
and is the buffer the engine sorts in, so a table smaller than it is ordered
in memory and a larger one spills once to a temporary file under the data
directory's `tmp/`. [ADR-0009](adr/ADR-0009-import-staging.md) records the
measurements and the alternatives.

A staging file is created once and reused. It is a surface of the shared
scratch pool (purpose `import-staging`, see
[the scratch pool](storage.md#the-scratch-pool)) rather than a pool of this
provider's own: it is the largest single file the product writes, so a pool
nobody else knew about was disk the run held and never disclosed, missing from
`scratch_bytes` and out of reach of `codectx gc`.

An import takes a surface, empties it by dropping its tables -- which returns
their pages to the file's own free list, with automatic vacuuming off, so the
file never shrinks -- and gives it back when it ends. Dropping is also what
makes the schema creation idempotent: each phase of an import creates the
structures it fills, and a reused file already holds them. A pooled surface
rather than one fixed path is what keeps this correct whether or not imports
through one provider overlap: a staging database is opened with an exclusive
lock, so two concurrent imports must have two files. An import whose staging
cannot be emptied -- an image a crash left mid-write -- retires that surface
instead of giving it back, because handing it to the next import would fail
every import of the workspace identically.

Nothing removes a staging file during a run, and the sweep below leaves the
pool alone: its instances are claimed with a lock, so the surface a killed run
held is taken over by the next run rather than swept. Creating and deleting
hundreds of megabytes per unit is what the run must not do: where the
filesystem discards freed blocks and the machine's disk is a sparse image, a
free of several gigabytes stalls every process on the machine about a minute
later, unobservably.

## Privacy and cleanup

Materializations, graphs not selected for the cache and exports are removed on
every termination path; the importer's staging files are emptied and reused
instead, and nothing removes them but `codectx gc`.

Those three are removed rather than pooled, and the reason is the content, not
the disk. An export and a graph are written by the engine, which chooses their
layout and their length, so nothing this product writes can be laid over them
and a stale one left in place would be read back as this run's output. A
materialization is this unit's own files: copying the whole snapshot and
pruning "spent the time and the disk of every sibling project, and left another
unit's source inside this unit's private tree", so one tree shared between units
reinstates exactly that, and an analyzer writes into the tree it was given. What
those removals cost is volume rather than a burst: each is renamed aside and
returns at once, and the process's one reclaimer gives the space back a window
at a time off the run's path, accounted in the resources block's
`freed_by_purpose` as `analyzer-output` or `materialization` as it goes. All of them
live under the provider's own private roots — `<data_dir>/dependence/runs/` for
the per-run directories and `<data_dir>/dependence/scratch/` for the staging
databases — never under the system temp directory: the staging database holds
source-derived graph content.

`defer` covers every return and every panic but not a `SIGKILL` or a power
loss, so the provider sweeps both private roots when it is constructed,
before any unit runs. A workspace has one cross-process owner, so nothing else
can hold a run of this provider at that moment and everything found there is
stale. The scan is bounded, and an entry that cannot be removed is logged and
skipped rather than failing construction. The child's stdout is discarded and its stderr is
read only for classification: no raw analyzer output, no source body and no
inherited environment reaches an ordinary log. Process-tree metrics are
recorded as their own log fields, separate from the base index's accounting.
