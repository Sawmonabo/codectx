# Incremental analysis with Joern 4.0.627: what the engine can do when one file changes

Date 2026-09-13. Machine: linux/amd64, 16 cores, 47 GiB, Joern 4.0.627, JDK 21.0.12
(`~/.local/opt/joern-cli`, `~/.local/opt/bompedia-jdk-21`). Every run used the pinned product argv
shape (`joern-parse --language <frontend> --max-num-def 40000`, then
`joern-export --repr=all --format=neo4jcsv`) at the default JVM heap unless stated.

Every claim below is tagged:
**[MEASURED]** = a run on this machine, command shown; **[SOURCED]** = an upstream artifact with a
URL; **[INFERRED]** = a calculation from measured numbers, labelled as such.

---

## 0. Short answer

**[SOURCED]** Joern has no incremental, partial, or differential mode. A maintainer answered the
question directly: "Does joern-parse support incremental builds?" → *"No, it doesn't right now."*
(<https://github.com/joernio/joern/issues/5757>, closed 2026-08-28). A feature request with an
unaudited prototype for one frontend is open and untriaged
(<https://github.com/joernio/joern/issues/5865>); a CPG-merging request has sat unanswered since 2023
(<https://github.com/joernio/joern/issues/2296>).

**[MEASURED]** Nothing in the shipped CLI merges or appends: `joern-parse -o <existing cpg.bin>`
overwrites the file, and the REPL's `importCpg` switches projects rather than merging. `joern-export`
has no per-file or per-method filter for the export we use.

**[MEASURED]** So today, editing one file costs a full unit re-run: for this repository (37.6k Go
lines) 4.8 s parse + 2.0 s export + a 49 MB CSV re-import; for a 116k-line Python package 8.2 s +
6.2 s + 246 MB.

**[MEASURED]** But that full re-run changes almost nothing: a one-line edit changed **52 of 398k**
fact rows for Go (0.013%), **139 of 1.86M** for Python (0.008%), **275 of 530k** for TypeScript
(0.05%). No other source file's facts changed, except for a handful of Go rows that a control run
shows are the engine's own run-to-run nondeterminism rather than a consequence of the edit (§4.2).

**[MEASURED]** And a per-file parse is far more faithful than expected for the fact family this
provider owns: for Python and TypeScript, control dependence and reaching-definition edges for the
methods in one file are **bit-for-bit the same** whether that file is parsed alone or inside the
whole unit. Go is the outlier (98.3% / 98.5%, plus 14% spurious dataflow edges and 14.9% wrong
types). What a file-only parse loses is **cross-file call resolution** — but less than the raw
identical-key rate suggests: once stubs that keep the correct `METHOD_FULL_NAME` are credited (the
reconciler aliases those), 96% of Go and 98% of Python outgoing calls survive. TypeScript is the real
casualty at 75%, and its failures are *wrong* names, not stubs, so they cannot be repaired.

The practical conclusion is at the end (§8): keep the frontend-native unit, make the *storage* update
a delta instead of a rewrite, and treat file-level parsing as an optional fast-preview tier for
Python/TypeScript/Java only — never as the authoritative unit.

---

## 1. What upstream offers (and does not)

### 1.1 Direct statements and open requests **[SOURCED]**

| Artifact | Status | What it says |
|---|---|---|
| [joern#5757](https://github.com/joernio/joern/issues/5757) | closed 2026-08-28 | "Does joern-parse support incremental builds?" → maintainer `max-leuthaeuser`: *"No, it doesn't right now."* Answered-no, not won't-do. |
| [joern#5865](https://github.com/joernio/joern/issues/5865) | open, untriaged | "[Feature] Incremental CPG update for javasrc2cpg". Proposes deleting `File` nodes + `NamespaceBlock` subtrees, re-parsing with `AstCreationPass(sourcesOverride=…)`, then re-running `OuterClassRefPass`, `TypeNodePass`, `TypeInferencePass`, `AstLinkerPass`, `ContainsEdgePass`, `StaticCallLinker`. Claims ~350 ms/file vs ~10 min full rebuild. No maintainer response; never became a PR; the only comment asks for the code. The pass list contains **no dataflow overlay**, so the claimed speed-up covers base-CPG reconstruction only. |
| [joern#2296](https://github.com/joernio/joern/issues/2296) | open since 2023-02-22 | CPG merging ("create a .jar cpg first, save it, and don't have to generate it again"). No maintainer reply in 3.5 years. |
| [joern#6283](https://github.com/joernio/joern/issues/6283) / [PR#6284](https://github.com/joernio/joern/pull/6284) | merged 2026-09-11 | Fixes an NPE in `joern-parse --overlaysonly`. The fix ships in **v4.0.627 and later only** — the release we pin. |
| GitHub Discussions on joernio/joern | none | The repository has zero discussions; targeted issue searches for "partial analysis", "reuse cpg", "incremental update", "add file to cpg", "watch mode" return nothing. |
| `joern-cpg2scpg` | removed | Not present in the v4.0.627 tree. |

**[SOURCED]** The CPG schema itself reserves a slot for this and nobody uses it. The `HASH` property
is documented as *"useful to determine whether code has already been analyzed in incremental analysis
pipelines"*
([Base.scala](https://github.com/ShiftLeftSecurity/codepropertygraph/blob/master/schema/src/main/scala/io/shiftleft/codepropertygraph/schema/Base.scala)),
and the CPG README calls the representation *"designed for incremental and distributed code
analysis"*
([README](https://github.com/ShiftLeftSecurity/codepropertygraph/blob/master/README.md)) — with no
consumers in either repository.

### 1.2 Which passes are file-local and which need the whole unit **[SOURCED]**

This is the load-bearing question: an incremental design can only be as fine as the coarsest pass it
must re-run.

**Method-local** (input is genuinely one method or one file):

- `CfgCreationPass`, `CfgDominatorPass`, `CdgPass` — `generateParts()` is `cpg.method.toArray` and
  `runOnPart` reads only that method.
- `ReachingDefPass` — the entire `dataflowOss` overlay is this one pass, a
  `ForkJoinParallelCpgPass[Method]` solving each method in isolation
  ([OssDataFlow.scala](https://github.com/joernio/joern/blob/master/dataflowengineoss/src/main/scala/io/joern/dataflowengineoss/layers/dataflows/OssDataFlow.scala),
  [ReachingDefPass.scala](https://github.com/joernio/joern/blob/master/dataflowengineoss/src/main/scala/io/joern/dataflowengineoss/passes/reachingdef/ReachingDefPass.scala)).
  Caveat: it initialises global semantics and its precision depends on `methodFullName` values
  resolved by earlier global passes.
- `ObjectPropertyCallLinker` (jssrc2cpg) — writes are gated to the same file; only the enumeration is
  global.

**Whole-unit** (cannot be narrowed to a file):

- `MethodStubCreator` — stubs a callee only where `cpg.method.fullNameExact(...)` is empty, so adding
  file B must *retract* a stub created for file A.
- `TypeNodePass` — scans every node's `TypeFullName`.
- `StaticCallLinker`, `DynamicCallLinker`, `XTypeHintCallLinker`, `AstLinkerPass`, `NaiveCallLinker`.
- `XTypeRecovery` (pysrc2cpg, jssrc2cpg) — parts are files but reads are global and it iterates twice
  by default.
- `ConstClosurePass` (jssrc2cpg) — groups every assignment target in the unit by bare name.
- `XInheritanceFullNamePass`, `XImportResolverPass` subclasses.

**Frontend-level blockers that prevent even building one file's AST in isolation:**

| Frontend | Blocker |
|---|---|
| gosrc2cpg | Four phases: `InitialMainSrcPass` → `PackageCtorCreationPass` → `DownloadDependenciesPass` → `AstCreationPass`. Phase 1 builds no AST; it fills the in-memory `GoGlobal` maps (package metadata, struct member types such as `pkg.Person.Age -> int`) that phase 4 resolves against. `GoGlobal` is never persisted and `PackageCtorCreationPass` destructively clears part of it. Local pass-class listing confirms `AstForPackageConstructorCreator` and `PackageCtorCreationPass` exist in the pinned jar **[MEASURED]**. |
| javasrc2cpg | One shared `JavaSymbolSolver` with an `EagerSourceTypeSolver` that resolves every `TypeDeclaration` in the source root at construction; `TypeInferencePass` builds a global method index in its constructor. |
| c2cpg | CDT parses each translation unit independently, but an `Accumulator` threads cross-TU state and `FullNameUniquenessPass` states: *"The AST is built on a per-file basis where we don't have knowledge of methods defined in other files - Name uniqueness must be established across the entire codebase."* (Note: `CGlobal`/`HeaderContentPass` do **not** exist at v4.0.627.) |
| rust2cpg | The bundled rust-analyzer-based generator loads the whole Cargo workspace. |
| kotlin2cpg | A whole-project `BindingContext` is passed into every part. |

**[SOURCED]** The closest thing upstream to an incremental primitive is `AstSummaryVisitor`
(csharpsrc2cpg, rubysrc2cpg): it parses a file at signature level into a throwaway `Cpg.empty`,
projects it to plain case classes, and `ProgramSummary.merge` is an associative merge. External
summaries can be persisted (C# `--external-summary-paths`, Ruby MessagePack stubs); **a project's own
summary is never written out**, and no such mechanism exists for Go, Python, TypeScript, C or Java.

### 1.3 What the storage layer can and cannot do **[SOURCED]**

- **Can splice.** `flatgraph.DiffGraphBuilder` exposes `addNode`, `addEdge`, `removeNode`,
  `removeEdge`, `setNodeProperty`, applied via `flatgraph.DiffGraphApplier.applyDiff`
  ([DiffGraphBuilder.scala](https://github.com/joernio/flatgraph/blob/master/core/src/main/scala/flatgraph/DiffGraphBuilder.scala)).
  Deletion is a tombstone, so node ids survive a save/load round trip.
- **Cannot append.** `Graph.close()` re-encodes every column of the file; the flatgraph README notes
  that concatenating two flatgraph files just makes the second one "trailing garbage"
  ([README](https://github.com/joernio/flatgraph/blob/master/README.md)). There is no subgraph-copy
  API between two graphs — which is exactly why `AstSummaryVisitor` projects to case classes instead.
- **Passes are add-only.** `CfgCreationPass`, `CdgPass` and `ReachingDefPass` have no removal path, so
  re-running them over a graph that already has their edges duplicates work (confirmed by measurement
  in §4.5).

### 1.4 What the commercial origin does **[SOURCED]**

Qwiet AI (formerly ShiftLeft), the vendor that built Joern, documents **no** incremental, delta or
diff scan: a sweep of all 567 pages of `docs.shiftleft.io` found zero hits for "incremental", "delta
scan", "differential" or "changed files". Their diffing is at the *findings* level on two completed
scans: *"When comparing two scans, Qwiet only considers findings present in the target that are not
present in the source"* (<https://docs.shiftleft.io/cli/reference/check-analysis-v2>), and the scan
command itself has no `--changed-files`/`--since`/`--incremental` flag but does carry
`--cpg-generation-timeout`, i.e. a CPG per scan
(<https://docs.shiftleft.io/cli/reference/analyze>). Their engineering blog argues *against*
incremental analysis explicitly: *"Incremental analysis is about only scanning a limited part of code
based on the changes… This usually has a high error rate"*
([archived](https://web.archive.org/web/20201208134315/https://blog.shiftleft.io/how-shiftleft-is-able-to-analyze-1-million-loc-under-15-minutes-d2655dfc0f92)).
Library handling is hand-written function summaries, not cached dependency CPGs
([archived](https://web.archive.org/web/20170615162337/https://blog.shiftleft.io/semantic-code-property-graphs-and-security-profiles-b3b5933517c1)).

### 1.5 Prior art elsewhere, briefly **[SOURCED]**

- **Fraunhofer AISEC cpg**: no incremental mode. Maintainer: incremental CPG creation *"is already
  something that we have planned, currently as a potential master's thesis topic, but it is not yet
  implemented"*, because *"the passes for variable resolving, call resolving etc. need the whole graph
  to operate on"* (<https://github.com/Fraunhofer-AISEC/cpg/issues/102>). The thesis was written —
  Hopstock, *Incremental Construction of Code Property Graphs*, TUM 2021, 50 commits over a 200-file
  Java project: ~60 min → ~13 min
  (<https://www.sec.in.tum.de/i20/student-work/incremental-construction-of-code-property-graphs-1>) —
  and never upstreamed. Its Neo4j support is write-only (no load path,
  <https://github.com/Fraunhofer-AISEC/cpg/tree/main/cpg-neo4j>).
- **Academia**: no incremental-CPG paper beyond that thesis. Incremental IFDS/IDE (Reviser, ICSE'14,
  <https://www.bodden.de/pubs/ab14reviser.pdf>) recomputes the call graph from scratch and in the
  worst case saves nothing (205 s vs 215 s); IncA (ASE'16, archived repo) needed up to 22 s to delete
  one assignment in a 6.5 KLOC program against a 35 s full analysis
  (<https://szabta89.github.io/publications/inca-pldi2021.pdf>). Every working implementation is
  *warm-process*: it holds solver state in RAM across edits, which a CLI that re-execs cannot do.
- **CodeQL** now ships overlay databases: `database init --overlay-base` plus
  `--overlay-changes=FILE` re-extracts only changed files, with per-predicate opt-in reuse via
  `overlay[local]` annotations, `build-mode: none` only
  (<https://docs.github.com/en/code-security/how-tos/find-and-fix-code-vulnerabilities/scan-from-the-command-line/incremental-analysis>,
  <https://codeql.github.com/docs/ql-language-reference/annotations/>).
- **Infer** reuses per-procedure summaries with reverse-dependency invalidation
  (`--mark-unchanged-procs`, `--incremental-analysis`, `--invalidate-only`,
  <https://github.com/facebook/infer/blob/main/infer/src/base/Config.ml>).
- **Glean** stacks incremental databases and hides superseded per-file fact units
  (<https://glean.software/docs/implementation/incrementality/>).
- **SCIP**: no incremental indexing; scip-clang's "Incremental builds support" is closed as not
  planned (<https://github.com/sourcegraph/scip-clang/issues/183>).
- Commercial "incremental" SAST is mostly reduced reach plus result carry-forward: Checkmarx scans
  changed files plus a "closure" and admits results outside it *"cannot be found"*
  (<https://docs.checkmarx.com/en/34965-324470-sast-scanner.html>); Fortify requires full translation
  and excludes dataflow from `-incremental`, then removed the feature
  (<https://www.microfocus.com/documentation/fortify-static-code-analyzer-and-tools/2010/SCA_Guide_20.1.2.pdf>);
  SonarQube skips unchanged files and filters to new code
  (<https://docs.sonarsource.com/sonarqube-server/10.8/analyzing-source-code/incremental-analysis/introduction.md>).

---

## 2. Experimental setup **[MEASURED]**

Scratchpad `…/scratchpad/incr/`. Corpora were copied so files could be edited:

| Corpus | Language | Source files | Lines | Unit shape |
|---|---|---|---|---|
| codectx (this repo, minus `docs/`) | Go | 126 | 37,590 | one `go.mod` module |
| `redglass/packages/pipeline` | Python | 474 | 116,057 | one package tree with `pyproject.toml` |
| `bompedia/desktop` (minus `node_modules`, `dist`) | TypeScript | 83 | 41,986 | one `tsconfig.json` project |
| synthetic `c-fix` | C | 3 (`main.c`, `util.c`, `util.h`) | 22 | one directory |
| synthetic `java-fix` | Java | 2 | 20 | one source root |

Exact commands (Go shown; the others differ only in `--language` and path):

```bash
export JAVA_HOME=~/.local/opt/bompedia-jdk-21
export PATH=$JAVA_HOME/bin:~/.local/opt/joern-cli:~/.cargo/bin:$PATH

# whole unit
/usr/bin/time -f "%e %M" joern-parse  corpus/go-codectx --language golang --max-num-def 40000 -o out/go-whole.bin
/usr/bin/time -f "%e %M" joern-export out/go-whole.bin --repr=all --format=neo4jcsv --out out/go-whole-csv

# one file only, keeping the real module layout (exclude every other source file by name)
LIST=$(find . -name '*.go' | sed 's|^\./||' | grep -v '^internal/provider/scip/importer.go$' | paste -sd,)
/usr/bin/time -f "%e %M" joern-parse corpus/go-codectx --language golang --max-num-def 40000 \
  -o out/go-file.bin --frontend-args --exclude "$LIST"
```

Facts were compared with an id-independent **semantic key**: every node is keyed by
(owning method's file, owning method full name, label, name, line, column, order, hash of code) and
every edge by the pair of node keys. Node ids are renumbered on every run, so a raw row diff is
meaningless. Two key modes were used: *structural* (no type attributes) and *full* (adds
`TYPE_FULL_NAME` and the callee's `METHOD_FULL_NAME`), which separates "the edge exists" from "the
edge is correctly typed". Script: `an.py` in the scratchpad; method→node attribution comes from the
export's `CONTAINS` edges.

**[MEASURED] A note on `--exclude-regex`.** Exclusion matching is substring-based (`find`-style), not
a full match, so a negative-lookahead regex such as `^(?!.*importer\.go).*` excludes *everything*,
including the file it was meant to keep (verified: 1 FILE node, 0 methods). Use `--exclude` with an
explicit comma-separated file list instead; a 4 KB list of 125 paths and a 20 KB list of 473 paths
both worked.

---

## 3. Baselines: what a unit costs today **[MEASURED]**

| Unit | FILE nodes | parse wall | parse peak RSS | cpg.bin | export wall | export RSS | CSV | CALL / CDG / REACHING_DEF / METHOD |
|---|---|---|---|---|---|---|---|---|
| Go, 37.6k lines | 173 | 4.77 s | 1.35 GB | 4.92 MB | 1.98 s | 0.71 GB | 49 MB | 35,207 / 60,847 / 228,481 / 2,672 |
| Python, 116k lines | 475 | 8.23 s | 3.30 GB | 20.5 MB | 6.19 s | 0.87 GB | 246 MB | 391,875 / 79,429 / 861,563 / 17,115 |
| TypeScript, 42k lines | 66 | 6.72 s | 2.29 GB | 6.25 MB | 2.64 s | 0.75 GB | 71 MB | 49,712 / 52,751 / 269,810 / 3,090 |

Re-running the same unit after a one-line edit costs the same: Go 4.96 s + 2.14 s, Python 7.56 s +
6.10 s, TypeScript 6.83 s + 2.58 s. **There is no engine-side saving from the fact that only one file
changed.**

**Per-invocation floor.** Parsing a unit with every file excluded (empty graph) still costs
**1.34 s / 179 MB** (Go), **1.35 s / 199 MB** (Python), **1.44 s / 198 MB** (TypeScript). That is JVM
start, frontend start and graph serialisation, and it is the hard floor under any per-file scheme.

---

## 4. The experiments

### 4.1 What a one-line edit actually changes in the export **[MEASURED]**

One line was modified in place (no line-count change) inside one function: Go
`internal/provider/scip/importer.go:559` (`if sym.local {` → `if sym.local && name != "" {`), Python
`knowledge/kg_projector.py:469`, TypeScript `bomography/workspace.ts:603`. The whole unit was
re-parsed and re-exported, and the exports compared on structural keys.

| Language | CDG | REACHING_DEF | CALL | CFG | REF | total rows changed | share of all facts | changed outside the edited file |
|---|---|---|---|---|---|---|---|---|
| Go | 10 | 27 | 4 | 10 | 1 | **52** | 0.013% (of 398,142) | 15 — see below |
| Python | 113 | 14 | 2 | 9 | 1 | **139** | 0.008% (of 1,856,199) | 0 |
| TypeScript | 171 | 60 | 12 | 31 | 1 | **275** | 0.052% (of 530,124) | 1 |

For Python and TypeScript, no file other than the edited one had any fact changed. Go's 15
"elsewhere" rows are of two kinds, and neither is caused by the edit: 13 sit on synthetic call-linker
stubs (`FILENAME=<empty>`, e.g. `…UnitRequest.Content.<FieldAccess>.<unknown>.EachFile`, plus their
`p0…p3` stub parameters), and 2 belong to a call site in `internal/reconcile/resolver_test.go:21`
whose resolved callee flipped between a stub and the correct
`internal/model.SnapshotView.EachFile`. The control run in §4.2 produces exactly the same class of
flip with **no** edit at all, so these rows are engine nondeterminism, not edit blast radius.

### 4.2 The determinism control — and the one place Joern is not reproducible **[MEASURED]**

The same unedited source was parsed twice and the exports compared:

| Language | CDG | REACHING_DEF | CALL | CFG | REF |
|---|---|---|---|---|---|
| Python | 0 | 0 | 0 | 0 | 0 |
| TypeScript | 0 | 0 | 0 | 0 | 0 |
| Go, all methods | 0 | **23,434 removed / 22,623 added (20% churn)** | 619 (3.5%) | 1,257 (2.5%) | 0 |
| Go, excluding `<clinit>` | 0 | 9 | 2 | 1 | 0 |

Two runs of identical Go source do not even produce the same cpg.bin (4,921,505 vs 4,922,524 bytes),
and the method count moved 2,672 → 2,673. **All of the churn is inside the synthetic per-package
`<clinit>` methods** that gosrc2cpg creates (`PackageCtorCreationPass`): the package's file-level
declarations are appended in a nondeterministic order, so their line/order coordinates shuffle between
runs. The pinned jar confirms this locally: `io/joern/gosrc2cpg/passes/PackageCtorCreationPass.class`
and `io/joern/gosrc2cpg/astcreation/AstForPackageConstructorCreator.class` are both present in
`io.joern.gosrc2cpg-4.0.627.jar`, alongside `InitialMainSrcPass` and the `GoGlobal` datastructure
**[MEASURED]** — MEASURED corroboration of the SOURCED pass architecture in §1.2, and the single
strongest piece of evidence in this report that Go's floor is the package, not the file. One package's `<clinit>` alone (`internal/model`) accounts for 19,343 of the changed rows.
Outside `<clinit>`, Go is effectively deterministic (9 rows of 183k).

Outside `<clinit>` a residual 12 rows still move between two identical Go runs, and they are
instructive: a call site in `internal/reconcile/resolver_test.go:29` resolved to
`internal/provider.Resolver.Resolve` in one run and to the stub
`internal/provider.UnitRequest.Resolver.<FieldAccess>.<unknown>.Resolve` in the other, dragging that
stub's own dataflow rows with it. gosrc2cpg's resolution of method calls through struct-field
receivers is order-sensitive.

This matters three times over: it is the noise floor for any row-level delta scheme (0.004% outside
`<clinit>`, 20% of dataflow rows with it); it means a Go `<clinit>` is a genuinely whole-package
construct that no per-file parse can reproduce; and it means a diff of two Joern exports cannot be
read as "what the edit did" without running this control first.

### 4.3 File-only and package-only parses: what survives **[MEASURED]**

For each language the unit was parsed with all but one file excluded (and, for Python/TypeScript, with
all but one package/directory excluded), keeping the real unit root so module paths and relative file
paths are unchanged. Facts were then restricted to the methods *declared in that file* and compared
with the same methods from the whole-unit run.

Cost:

| Run | files parsed | parse wall | parse RSS | export wall | export RSS |
|---|---|---|---|---|---|
| Go whole | 173 | 4.77 s | 1.35 GB | 1.98 s | 0.71 GB |
| Go one file | 4 | **2.53 s** | 0.34 GB | 0.60 s | 0.16 GB |
| Python whole | 475 | 8.23 s | 3.30 GB | 6.19 s | 0.87 GB |
| Python one file | 2 | **2.46 s** | 0.35 GB | 0.75 s | 0.23 GB |
| Python one package (27 files) | 28 | 2.94 s | 0.62 GB | 1.07 s | 0.48 GB |
| TypeScript whole | 66 | 6.72 s | 2.29 GB | 2.64 s | 0.75 GB |
| TypeScript one file | 2 | **4.44 s** | 0.78 GB | 1.00 s | 0.44 GB |
| TypeScript one directory (14 files) | 14 | 5.13 s | 1.29 GB | 1.40 s | 0.51 GB |
| C whole (3 files) | — | 1.72 s | 0.24 GB | — | — |
| C one file | — | 1.72 s | 0.24 GB | — | — |
| Java whole (2 files) | — | 4.98 s | 0.51 GB | — | — |
| Java one file | — | 4.66 s | 0.51 GB | — | — |

Fidelity, for the methods declared in the edited file:

| Language / scope | METHOD rows | CDG | REACHING_DEF | REF | non-operator CALL edges, identical key | node types wrong |
|---|---|---|---|---|---|---|
| Go, file only | 43/43 identical | 2,584/2,629 (98.3%), 15 spurious | 6,754/6,857 (98.5%), **960 spurious (+14%)** | 932/932 (100%) | 174/240 (**72%**) | 380 of 2,554 (**14.9%**) |
| Python, file only | 39/39 identical | 935/935 (**100%**) | 5,112/5,112 (**100%**) | 914/955 (96%) | 236/377 (63%) | 54 of 1,478 (3.7%) |
| Python, package only | 39/39 identical | **100%** | **100%** | 953/955 (99.8%) | 282/377 (75%) | 1 of 1,478 (0.1%) |
| TypeScript, file only | 205/205 identical | 8,750/8,750 (**100%**) | 31,571/31,571 (**100%**) | 3,972/3,972 (100%) | 1,170/1,556 (75%) | **0** |
| TypeScript, directory only | 205/205 identical | **100%** | **100%** | 100% | 1,272/1,556 (82%) | 0 |
| C, file only | identical | 22/22 (100%) | 95/95 (100%), 1 spurious | 17/17 (100%) | 2/4 (50%) | 0 |
| Java, file only — **exclusion had no effect, see below** | identical | 24/24 (100%) | 101/101 (100%) | 13/13 (100%) | 4/4 (100%) | 0 |

The call-edge column above counts only edges whose key is byte-identical. That is a lower bound on
what is usable, because it scores an edge as lost when the callee merely became an external stub
carrying the *same* `METHOD_FULL_NAME` — which the plan's reconciler aliases back to the real
declaration (report 07 §"Sharding experiment"). Splitting the same data by what actually happened,
and separating calls *out of* the re-parsed file from calls *into* it from files that were not
re-parsed **[MEASURED]**:

| Scope | Outbound edges | identical | stub, same FULL_NAME (recoverable) | wrong FULL_NAME | call site absent | usable by full-name alias |
|---|---|---|---|---|---|---|
| Go, file only | 234 | 174 (74.4%) | 51 (21.8%) | 9 (3.8%) | 0 | **225/234 (96.2%)** |
| Python, file only | 277 | 236 (85.2%) | 35 (12.6%) | 6 (2.2%) | 0 | **271/277 (97.8%)** |
| Python, package only | 277 | 275 (99.3%) | 2 (0.7%) | 0 | 0 | **277/277 (100%)** |
| TypeScript, file only | 1,555 | 1,170 (75.2%) | 0 | 297 (19.1%) | 88 (5.7%) | **1,170/1,555 (75.2%)** |
| TypeScript, directory only | 1,555 | 1,272 (81.8%) | 0 | 255 (16.4%) | 28 (1.8%) | **1,272/1,555 (81.8%)** |
| C, file only | 4 | 2 (50%) | 2 (50%) | 0 | 0 | **4/4 (100%)** |

The inbound half is a different thing and is not a fidelity loss of the reduced run: the whole-unit
run had 6 (Go), 100 (Python file-only), 1 (TypeScript) call edges whose *caller* lives in a file that
was not re-parsed. Those edges are simply not produced, because their owner file was not analysed;
in a delta-import model they stay in the store attached to the files that own them. Package-level
Python recovers 7 of its 100 inbound edges because the package contains some of the callers.

So the honest statement is: **for Go, Python and C, a file-only parse keeps essentially all of the
edited file's outgoing calls in a form the reconciler can bind (96–100%); for TypeScript it does
not.** TypeScript is the only frontend where the failure is *wrong* rather than *unresolved* — 19%
of its call edges point at a different `METHOD_FULL_NAME` (`__ecma.Map:<operator>.new` becomes
`__ecma.Set:<operator>.new`; a callee in `pinned-transport.ts` becomes one in `benchmark.ts`). A
wrong edge cannot be repaired by aliasing and must not be published.

Readings:

- **Control and data dependence are intraprocedural in practice.** For Python, TypeScript, C and Java
  the per-method CDG and REACHING_DEF edges are identical when the file is parsed alone. This is the
  engine's own structure showing through: the whole `dataflowOss` overlay is a single per-method pass
  (§1.2).
- **Go is the exception.** The Go frontend's lowering depends on knowing, for example, whether an
  identifier is a package name or a variable, so a lone file produces slightly different ASTs: 1.7% of
  CDG edges missing, and 14% *extra* reaching-def edges — an over-approximation caused by unresolved
  callees, not a safe subset. 14.9% of typed nodes get a different `TYPE_FULL_NAME`, and the wrong
  values are not just `ANY`: `error` became a concrete struct type, a concrete type became `ANY`, and
  call targets degraded to `ANY.scopeKey`.
- **Cross-file call resolution degrades, but mostly recoverably.** Raw identical-key rates are 75%
  (TypeScript), 72% (Go), 63% (Python), 50% (C); after crediting stubs that keep the correct
  `METHOD_FULL_NAME` and excluding inbound edges owned by other files, 96% (Go), 98% (Python), 100%
  (C) — and still only 75% for TypeScript, whose losses are wrong names rather than stubs. Package
  granularity takes Python to 100%, directory granularity takes TypeScript to 82%.
- **Java was never actually reduced, so it is not evidence of anything.** The two runs' fact counts
  are byte-identical (`node_FILE` 3, `node_METHOD` 19, `edge_REACHING_DEF` 254 in both) and the
  "file-only" export still contains `src/Util.java` as a `FILE` node with its methods
  `IS_EXTERNAL=false`. `--exclude` is ignored by javasrc2cpg: it produced the same graph in 6% less
  wall time. The correct reading is **not** "per-file Java is exact" but "per-file Java was never
  exercised, because exclusion does not reduce the work" — consistent with `EagerSourceTypeSolver`
  reading the whole source root (§1.2 [SOURCED]). A genuine single-file Java parse from a separate
  input directory was not tested.
- **The C and Java numbers come from 3-file and 2-file fixtures**, not from a real source tree, and
  the C fixture has a header declaring both functions — the friendliest possible case for a file-only
  parse. Treat them as fixture results, not as frontend properties. C's exclusion did take effect
  (`node_FILE` and every edge count dropped), unlike Java's.
- **Python's whole-tree run is sometimes *less* precise.** Several nodes typed `ANY` in the whole run
  are concretely typed in the file-only run, and in TypeScript 31 call sites that the whole-project run
  left as `<unknownFullName>` resolve in the file-only run. Whole-program type recovery fans out
  candidates; this matches the finding in report 10 §8 that the whole-tree Python run emits 2.41M
  external call edges against 1.35M for per-package units.

### 4.4 Can two graphs be merged, or one extended? **[MEASURED]**

| Probe | Command | Result |
|---|---|---|
| Parse a second input into an existing graph | `joern-parse corpus/go-onefile --language golang -o out/merge.bin` where `merge.bin` was a copy of the 4.92 MB whole-unit graph | **Truncates.** Resulting file 69,268 bytes, containing 3 FILE / 6 METHOD nodes — only the new input. No merge, no warning. |
| Load a second CPG in the REPL | script calling `importCpg(other)` | **Replaces the active project.** `cpg` went from 173 files / 2,672 methods to 2 files / 168 methods. Both projects are listed in the workspace, but `cpg` binds to one at a time. `WorkspaceManager` deletes an existing project of the same name first. |
| Mutate a cached graph in place | REPL script: `DiffGraphBuilder.removeNode` on the 9,509 AST nodes of one file, then `DiffGraphApplier.applyDiff` | **Works.** 87 ms to apply; methods 2,672 → 2,629; the file's methods are gone. The whole script (JVM start, graph load, mutation, save) took 5.06 s wall / 0.99 GB. |
| Recompute overlays after that deletion | clear the `OVERLAYS` metadata property, then `run.ossdataflow` and `run.base` | **Works.** Dataflow overlay over the whole 128k-node graph: **685 ms**. Base overlay: 389 ms (recreated 65 stub nodes). |
| Add a parsed file back into the graph | — | **No API.** flatgraph has no subgraph copy between graphs, and the frontend jars are not on the REPL classpath (`joern-cli/lib` ships `x2cpg` and `dataflowengineoss` but not `gosrc2cpg`/`pysrc2cpg`/`jssrc2cpg`). |

So the *delete* half of a splice is cheap and available; the *add* half does not exist in anything the
product may legitimately run. Also **[MEASURED]**: `joern-parse` does apply the dataflow overlay — the
metadata of a cpg.bin produced by our pinned argv reads
`OVERLAYS=IndexedSeq(base, controlflow, typerel, callgraph, dataflowOss)`.

### 4.5 Overlay reuse: `--nooverlays` / `--overlaysonly` **[MEASURED]**

| Unit | parse `--nooverlays` | cpg after | then `--overlaysonly` | cpg after | `--overlaysonly` a second time | effect of the second run |
|---|---|---|---|---|---|---|
| Go 37.6k | 3.06 s | 2.11 MB | 2.28 s | 4.92 MB | 0.70 s | none — byte size unchanged (no-op) |
| Python 116k | 2.56 s | 8.82 MB | 5.48 s | 20.46 MB | 5.67 s | **graph grew to 20.73 MB**: +441 TYPE_DECL, +871 REF, +6 CALL, +1 METHOD |
| TypeScript 42k | 3.48 s | 2.55 MB | 3.85 s | 6.24 MB | 1.86 s | **graph grew to 6.28 MB**: +17 METHOD, +1,013 REF, +31 CALL, +1 TYPE_DECL |

CDG and REACHING_DEF counts were unchanged in every case. The asymmetry matches the upstream code:
`LayerCreator.run` skips layers already recorded in `cpg.metaData.overlays` (hence Go's clean no-op),
but the frontend-specific `applyPostProcessingPasses` — type recovery and the linkers for
pysrc2cpg/jssrc2cpg — are not gated and run again, adding nodes and edges
([LayerCreator.scala](https://github.com/joernio/joern/blob/master/semanticcpg/src/main/scala/io/shiftleft/semanticcpg/layers/LayerCreator.scala)).
**Conclusion: `--overlaysonly` is safe only on a bare `--nooverlays` graph and only once. It is not a
reuse primitive for an already-overlaid cached graph**, and for Python/TypeScript re-applying it
corrupts fact counts. (It also only works at all from v4.0.627, the release we pin — earlier versions
NPE, PR#6284.)

Useful by-product: when parsing is split in two, the overlay stage is 43% (Go), 68% (Python) and 53%
(TypeScript) of the two-stage total, and re-running just the dataflow overlay in-process took 685 ms
for a 37.6k-line Go unit. Splitting also costs more overall than parsing in one shot (Go 5.34 s vs
4.77 s, TypeScript 7.33 s vs 6.72 s; Python 8.04 s vs 8.23 s), because the graph is serialised and
reloaded in between.

### 4.6 Can the export be taken per file? **[MEASURED]**

`joern-export --help` offers only `--repr` and `--format`; there is **no file, method or since filter**.

`--repr=cpg --format=neo4jcsv` does shard output per method into a directory tree mirroring source
paths (`internal/provider/scip/importer.go/<method>.csv/edges_CDG_data.csv`, …), which looks like the
per-file extraction we would want. It is not usable:

| | `--repr=all` | `--repr=cpg` (per-method shards) |
|---|---|---|
| wall / RSS | 1.98 s / 0.71 GB | 6.35 s / 2.81 GB |
| output | 49 MB, 41 files | **554 MB, 129,111 files** |
| CDG rows | 60,847 | 108,897 (duplicated across shards) |
| CALL edges | 35,207 | **674** (only 72 of 2,179 method directories have any) |

The per-method projection duplicates intraprocedural edges and drops almost every call edge, because
the callee node is outside the method's subgraph. Report 07's finding stands: the single
`--repr=all --format=neo4jcsv` export is the only artifact that carries CALL + CDG + REACHING_DEF
together.

---

## 5. The smallest correct unit, per frontend

"Correct" here means: the facts this provider publishes (`control_depends_on`, `data_flows_to`,
`reads`/`writes`, and engine `calls`) are the same as they would be from the frontend-native unit.

| Frontend | Smallest unit that is correct for control/data dependence | Smallest unit correct for engine `calls` and types | Why | Evidence |
|---|---|---|---|---|
| gosrc2cpg (Go) | **module** (package is close but not exact) | module | Package-level declarations are merged into a synthetic per-package `<clinit>`; `GoGlobal` type maps are built from the whole module in a separate first phase. A file-only parse gives 98.3% CDG / 98.5% dataflow with 14% spurious edges and 14.9% wrong types. | §4.2, §4.3 [MEASURED]; §1.2 [SOURCED] |
| pysrc2cpg (Python) | **file** (exact) | package | Per-file parse reproduces CDG and REACHING_DEF exactly; 37% of non-operator call edges and 3.7% of types need the rest of the package. | §4.3 [MEASURED] |
| jssrc2cpg (TS/JS) | **file** (exact) | `tsconfig`/`package.json` project | Per-file parse reproduces CDG, REACHING_DEF, REF and all types exactly; 25% of non-operator call edges need the project. Report 10 §8 already showed that project-level splitting halves resolved calls. | §4.3 [MEASURED]; report 10 §8 |
| javasrc2cpg (Java) | **not measured** — `--exclude` does not reduce the work, so a per-file parse was never exercised | source root | The excluded file still appeared as a `FILE` node with non-external methods and the two runs' fact counts were byte-identical; only 6% wall time was saved. Consistent with `EagerSourceTypeSolver` reading the whole root. A true single-file parse from a separate input dir remains untested. | §4.3 [MEASURED]; §1.2 [SOURCED] |
| c2cpg (C/C++) | file, in a 3-file fixture with a shared header — **not validated on a real tree** | **whole project** | CDT parses each TU independently, but `FullNameUniquenessPass` needs codebase-wide knowledge and cross-TU callees become external stubs (2 of 4 call edges lost in the fixture). Header coupling is unchanged from report 10 §4a. | §4.3 [MEASURED]; §1.2 [SOURCED] |
| rust2cpg (Rust) | **Cargo workspace** | Cargo workspace | The bundled rust-analyzer generator loads the whole workspace via `cargo metadata`; a bare directory yields an empty graph (report 07), and per-crate units each pay ~0.8 GB of fixed helper overhead (report 10 §7). | reports 07, 10 [MEASURED]; §1.2 [SOURCED] |

**What is lost by going finer than the frontend-native unit** — the same four things everywhere,
in decreasing order of importance:

1. **Cross-unit call targets.** The callee becomes an `IS_EXTERNAL` stub. For import-resolved static
   calls the stub keeps the fully qualified name, so the reconciler can still alias it (report 07):
   96% (Go) and 98% (Python) of the edited file's outgoing calls survive in that recoverable form.
   TypeScript is the exception — 19% of its call edges come back pointing at a *different* method,
   which aliasing cannot repair. A file-only Go or Python run also mis-types the *receiver*, which
   changes the stub's name itself.
2. **Type accuracy** (Go 14.9%, Python 3.7%, TypeScript and Java 0%).
3. **Whole-unit synthetic constructs**: Go's per-package `<clinit>`, which simply does not exist in a
   one-file parse.
4. **Dynamic dispatch and interface resolution**, unchanged from report 07's sharding experiment.

---

## 6. The cost model

### 6.1 Measured, on the corpora above **[MEASURED]**

Editing one file today (cache key changes → whole unit re-runs):

| Unit | engine wall | peak RSS | bytes re-imported | fact rows that actually change |
|---|---|---|---|---|
| Go 37.6k lines | 4.8 s parse + 2.0 s export ≈ **6.8 s** | 1.35 GB | 49 MB CSV | 52 (0.013%) |
| Python 116k lines | 8.2 s + 6.2 s ≈ **14.4 s** | 3.30 GB | 246 MB CSV | 139 (0.008%) |
| TypeScript 42k lines | 6.7 s + 2.6 s ≈ **9.3 s** | 2.29 GB | 71 MB CSV | 275 (0.05%) |

**Cost of computing the delta itself** — this is the price of Option 1 in §8, and it is small
**[MEASURED]**. Deriving id-independent semantic keys for every fact in an export (single-threaded
Python, the same `an.py` used for every experiment above) and comparing two key sets:

| Unit | CSV size | key derivation | peak RSS | compare (sort + set difference, 861k-row family) |
|---|---|---|---|---|
| Go 37.6k lines | 49 MB | 1.06 s | 73 MB | — |
| Python 116k lines | 246 MB | 5.46 s | 250 MB | 1.04 s, 266 MB |
| TypeScript 42k lines | 71 MB | 1.46 s | 83 MB | — |

Key derivation costs roughly half the export's wall time and a fraction of its memory, and a
production implementation in Go, streaming and parallel, would be faster still. It is a rounding
error against the 6.8–14.4 s engine run it follows, so the delta computation does not eat the
saving it creates.

A hypothetical perfect per-file path, bounded by the measured floors: Go 2.5 s + 0.6 s, Python 2.5 s +
0.8 s, TypeScript 4.4 s + 1.0 s — i.e. **2.1× / 4.4× / 1.7× faster** on these units, of which
1.3–1.4 s is irreducible JVM and frontend startup.

### 6.2 Extrapolated to the sizes the plan cares about **[INFERRED from MEASURED ratios in reports 09 and 10]**

Using the measured figures for a 239k-line Go module (x/tools: parse 11.9 s at default heap, 23.2 s at
a 500 MB cap, export 4.5–5.1 s, CSV 145 MB, ~1.0 GB tree RSS capped) and a 506k-line Python tree
(redglass: parse 25.8 s default, 37 s at a 2 GB cap, 3.9 GB RSS, export 27.5 s, CSV 1.27 GB):

| Scenario | What runs today | Engine wall | Memory | Re-imported | Rows that change |
|---|---|---|---|---|---|
| Edit one file in a 240k-line Go module | whole module parse + export | **16–28 s** | ~1.0 GB (capped) to 4.8 GB (default) | 145 MB CSV | ~0.01% (INFERRED from §4.1) |
| Commit touching 5 files in the same module | identical — one unit run | 17–28 s | same | 145 MB | ~0.05% |
| Commit touching 5 files across 3 modules | 3 unit runs, serialised (`max_concurrent_heavy_analyzers = 1`) | 3 × unit cost | one unit's reservation at a time | 3 CSVs | — |
| Edit one file in a 500k-line Python tree | whole tree parse + export | **53–65 s** | 3.9 GB | 1.27 GB CSV | ~0.01% |
| Edit one file in a 500k-line Python tree, per-package units | one package parse + export | seconds (measured 2.9 s + 1.1 s for a 27-file package) | 0.6 GB | 13 MB | — |

Two consequences worth stating plainly. First, **the unit's size, not the edit's size, sets the
price** — which is why per-package Python units (already the plan's rule) matter far more for edit
latency than any incremental scheme would. Second, **the wasted work is almost all of it**: the engine
spends 17–65 s to produce an export that differs from the previous one in about one row in ten
thousand.

---

## 7. What does not exist — the honest list

- **[SOURCED]** No incremental, partial or differential mode in Joern, any release; no CLI flag, no
  API, no documented recipe. The maintainer's answer is "No, it doesn't right now".
- **[MEASURED]** No merge: `-o` on an existing cpg.bin overwrites it; `importCpg` replaces the active
  project. **[SOURCED]** flatgraph cannot append (`close()` rewrites the whole columnar file; two
  concatenated files are "trailing garbage").
- **[MEASURED]** No way to add a parsed file's AST into an existing graph. The delete half works
  (87 ms) and overlays can be recomputed in-process (685 ms), but nothing the product may run supplies
  the add half — the frontends are separate launchers with their own classpaths and always create a
  fresh graph.
- **[MEASURED]** No subset export. `--repr=cpg` shards per method but duplicates intraprocedural edges
  and drops 98% of call edges.
- **[MEASURED]** `--overlaysonly` is not a reuse primitive: a no-op on an already-overlaid Go graph,
  and fact-corrupting on Python/TypeScript graphs (adds hundreds of REF/TYPE_DECL/METHOD rows).
- **[SOURCED]** No frontend can build one file's AST without whole-unit state (`GoGlobal`,
  `EagerSourceTypeSolver`, the C accumulator, the Rust workspace load, Kotlin's `BindingContext`).
- **[SOURCED]** No vendor precedent: Qwiet/ShiftLeft, who built the technology, rebuild the whole CPG
  per scan and argue incremental analysis "usually has a high error rate".
- **[SOURCED]** No academic solution for CPGs: one unmerged 2021 master's thesis, and the incremental
  dataflow literature needs a warm process holding GB of solver state.
- **[MEASURED]** And one thing that does not exist that we might have assumed: **reproducibility**.
  Two Joern runs over identical Go source do not produce the same graph — mostly because of the
  synthetic per-package `<clinit>` (20% of dataflow rows), and residually because call resolution on
  struct-field receivers is order-sensitive. Python and TypeScript are reproducible.

---

## 8. Options, ranked

### Option 1 — Keep the frontend-native unit and the cpg cache; make the *storage* update a delta (recommended)

Change nothing about what the engine runs. On a refresh, after the unit's export is produced, compare
facts against the active generation with a semantic key and write only the rows that changed.

- **[MEASURED]** The payoff is real: 52 / 139 / 275 changed rows against 363k / 1.85M / 530k total.
  The SQLite write and FTS rewrite for a changed unit become proportional to the edit, not to the unit.
- **[MEASURED]** It is feasible: Python and TypeScript are perfectly reproducible run to run. Go is
  not, and needs two accommodations: key the synthetic per-package `<clinit>` methods by content
  rather than by position (or simply rewrite them wholesale — one method per package), and accept a
  residual churn of about a dozen rows per run from order-sensitive call resolution on struct-field
  receivers (0.004% of the unit's rows), which a delta writer rewrites harmlessly.
- The key must be id-independent — Joern renumbers node ids on every run — which we have already
  built and validated here (`CONTAINS`-based method attribution plus a positional/content key).
- **Cost accepted:** the engine still spends the full 17–65 s per edited unit. This option buys
  storage and activation cost, not engine cost. It also needs the `<clinit>` normalisation, without
  which 20% of Go dataflow rows would be rewritten on every refresh for no reason.

### Option 2 — Cut edit latency by unit sizing and scheduling, not by incrementality

The measured driver of edit cost is unit size. The plan's existing rules (Python units are packages,
TypeScript units are tsconfig projects) already reduce a 53–65 s Python refresh to ~4 s for the
package that changed. What is worth adding is the operational side: coalesce a commit's files into one
unit run (already the design), keep dependence units in the background after base activation (already
the design), and make the watch-mode debounce long enough that a burst of saves costs one run.

- **[MEASURED]** Per-package Python: 2.94 s + 1.07 s and 13 MB of CSV, versus 8.2 s + 6.2 s and 246 MB
  for the 474-file tree — and per-package units lose nothing in dependence facts (100% CDG and
  REACHING_DEF, §4.3, matching report 10 §8).
- **Cost accepted:** none beyond what the plan already accepts (cross-package calls land on aliased
  stubs).

### Option 3 — A file-level fast tier for Python, TypeScript (and Java), marked as reduced precision

When one file in an already-sealed unit changes, run a file-only parse of that file (2.5–4.4 s) and
publish only `control_depends_on`, `data_flows_to`, `reads`/`writes` for the methods in that file,
while the whole-unit refresh runs in the background.

- **[MEASURED]** For Python and TypeScript this is not an approximation: the per-method CDG and
  REACHING_DEF edges are identical to the whole-unit run, as are all types on TypeScript. The engine's
  own structure guarantees it — the dataflow overlay is one per-method pass **[SOURCED]**.
- Engine `calls` from such a run must **not** be published as `static_analysis`. For Python 98% of
  the file's outgoing calls are recoverable by full-name aliasing, but 2% carry a wrong
  `METHOD_FULL_NAME`; for TypeScript 19% are wrong. A wrong edge is worse than a missing one. That is
  acceptable because `calls` already comes from tree-sitter + SCIP (synthesis §3); the dependence
  provider's `calls` is a fallback.
- **Do not do this for Go.** 98.3%/98.5% with 14% spurious dataflow edges and 14.9% wrong types is
  an over-approximation presented as a fact, which the plan's honesty rules forbid; at best it would
  be `partial`, for a 2.1× saving on a 37k-line module.
- **Do not do this for Java** — measured 6% saving and a byte-identical graph, because `--exclude`
  does not reduce javasrc2cpg's work at all. The earlier reading that "per-file Java is exact" was
  wrong: per-file Java was never exercised.
- **Cost accepted:** a second code path, two provenance states for the same capability within one
  generation's lifetime, and a rule that file-tier facts are superseded by the unit refresh. Given
  Option 2 already brings Python/TypeScript units down to seconds, this is worth building only if
  real repositories produce units where a single refresh is tens of seconds.

### Option 4 — Build the splice on Joern internals (not now)

Delete the changed file's subtree from the cached graph, re-create its AST, re-run the global linker
passes and the per-method dataflow pass. This is exactly the recipe in joern#5865 **[SOURCED]**, and
two of its three steps are already reachable: deletion took 87 ms and the dataflow overlay 685 ms on a
37.6k-line Go graph **[MEASURED]**.

It is blocked on the middle step. Creating one file's AST requires running a frontend's
`AstCreationPass` against an existing `Cpg` object, which means putting frontend jars on a JVM
classpath and writing Scala against Joern's internal API — product-owned analysis code inside the
engine, which section 11.6 explicitly rules out ("use the engine's built-in noninteractive tools and
one exact tested argv per step, not product-owned analysis scripts"). It also inherits the frontends'
whole-unit state (`GoGlobal` is in-memory only and destructively cleared; javasrc2cpg re-solves the
whole source root) **[SOURCED]**, and every affected pass is add-only, so correctness needs a
retraction story for stubs and links **[SOURCED]**. The upstream prototype covers one frontend, has no
maintainer response, no PR, and no dataflow step.

- **Revisit if** upstream merges #5865 or an equivalent, or if a real user hits a unit where a refresh
  costs minutes and Options 1–3 are exhausted. If it is ever built, the correctness gate is the one
  the Fraunhofer thesis used: graph equality against a from-scratch build **[SOURCED]**, which the
  benchmark corpora pinned in Task 21 already give us.

### Option 5 — Ask upstream

Cheap and honest: comment on joern#5757/#5865 with the measurements in this report (per-method
dataflow is already the structure that makes this tractable; the missing piece is a frontend entry
point that creates an AST for a file list into an existing CPG) and note that the schema's `HASH`
property, reserved for exactly this, has no consumers **[SOURCED]**. This costs nothing and does not
block anything.

### Not recommended

- **Per-file units as the standard granularity.** Loses a quarter to a half of engine call edges, and
  for Go produces wrong types and spurious dataflow edges. Contradicts report 10 §8's ruling.
- **`--overlaysonly` as a cache-reuse trick.** Measured to corrupt Python and TypeScript graphs on a
  second application.
- **`--repr=cpg` per-method shards as a delta export.** 11× the bytes, 3× the time, 98% of call edges
  lost.
- **Keeping a warm Joern process to hold graphs across edits.** It would make Option 4 cheaper (the
  REPL loads a 4.9 MB graph and mutates it in ~4 s, most of which is JVM start), but section 11.6
  rules out server mode for V1, and the memory governor's model assumes one transient process tree.

---

## 9. Reproduction

All commands were run with `JAVA_HOME=~/.local/opt/bompedia-jdk-21` and
`~/.local/opt/joern-cli` on `PATH`. Timing is `/usr/bin/time -f "%e %M"` (wall seconds, peak RSS of the
largest process in the tree — the same convention as report 09; report 10's tree-summed figures add
30–130 MB for Go and TypeScript).

```bash
# baseline and edited runs
joern-parse  <corpus> --language <golang|pythonsrc|jssrc|c|javasrc> --max-num-def 40000 -o <cpg>
joern-export <cpg> --repr=all --format=neo4jcsv --out <dir>

# restricted parse (one file, or one package) keeping the real unit root
joern-parse <corpus> --language <l> --max-num-def 40000 -o <cpg> --frontend-args --exclude "<comma,separated,paths>"

# overlay staging
joern-parse <corpus> --language <l> --max-num-def 40000 --nooverlays   -o <cpg>
joern-parse <corpus> --language <l> --max-num-def 40000 --overlaysonly -o <cpg>

# in-process splice probe
joern <cpg> --script probe3.sc --param fname=<relative/path.go>
#   cpg.method.filenameExact(fname).ast.l  -> DiffGraphBuilder.removeNode -> DiffGraphApplier.applyDiff
#   clear metaData OVERLAYS, then run.ossdataflow / run.base

# per-method sharded export probe
joern-export <cpg> --repr=cpg --format=neo4jcsv --out <dir>
```

Artifacts (cpg.bin files, CSV exports, corpus copies) were deleted after the numbers were extracted,
per the research hygiene rule in report 10 §11. The comparison scripts (`an.py`, `diffkeys.py`,
`calls.py`, `typediff.py`) and the extracted counts remain in the session scratchpad.

## 10. Sources

Joern and flatgraph: <https://github.com/joernio/joern/issues/5757> ·
<https://github.com/joernio/joern/issues/5865> · <https://github.com/joernio/joern/issues/2296> ·
<https://github.com/joernio/joern/issues/6283> · <https://github.com/joernio/joern/pull/6284> ·
<https://github.com/joernio/joern/blob/master/dataflowengineoss/src/main/scala/io/joern/dataflowengineoss/layers/dataflows/OssDataFlow.scala> ·
<https://github.com/joernio/joern/blob/master/dataflowengineoss/src/main/scala/io/joern/dataflowengineoss/passes/reachingdef/ReachingDefPass.scala> ·
<https://github.com/joernio/joern/blob/master/dataflowengineoss/src/main/scala/io/joern/dataflowengineoss/queryengine/Engine.scala> ·
<https://github.com/joernio/joern/blob/master/joern-cli/frontends/x2cpg/src/main/scala/io/joern/x2cpg/X2Cpg.scala> ·
<https://github.com/joernio/joern/blob/master/joern-cli/frontends/javasrc2cpg/src/main/scala/io/joern/javasrc2cpg/JavaSrc2Cpg.scala> ·
<https://github.com/joernio/joern/blob/master/semanticcpg/src/main/scala/io/shiftleft/semanticcpg/layers/LayerCreator.scala> ·
<https://github.com/joernio/joern/blob/v4.0.627/console/src/main/scala/io/joern/console/workspacehandling/WorkspaceManager.scala> ·
<https://github.com/joernio/flatgraph/blob/master/core/src/main/scala/flatgraph/DiffGraphBuilder.scala> ·
<https://github.com/joernio/flatgraph/blob/master/README.md> ·
<https://docs.joern.io/organizing-projects/> ·
<https://github.com/ShiftLeftSecurity/codepropertygraph/blob/master/schema/src/main/scala/io/shiftleft/codepropertygraph/schema/Base.scala> ·
<https://github.com/ShiftLeftSecurity/codepropertygraph/blob/master/README.md> ·
<https://github.com/ShiftLeftSecurity/codepropertygraph/blob/master/codepropertygraph/src/main/scala/io/shiftleft/passes/CpgPass.scala>

Qwiet/ShiftLeft: <https://docs.shiftleft.io/cli/reference/analyze> ·
<https://docs.shiftleft.io/cli/reference/check-analysis-v2> ·
<https://docs.shiftleft.io/sast/workflows/github> ·
<https://web.archive.org/web/20201208134315/https://blog.shiftleft.io/how-shiftleft-is-able-to-analyze-1-million-loc-under-15-minutes-d2655dfc0f92> ·
<https://web.archive.org/web/20170615162337/https://blog.shiftleft.io/semantic-code-property-graphs-and-security-profiles-b3b5933517c1>

Other CPG/incremental work: <https://github.com/Fraunhofer-AISEC/cpg/issues/102> ·
<https://github.com/Fraunhofer-AISEC/cpg/tree/main/cpg-neo4j> ·
<https://www.sec.in.tum.de/i20/student-work/incremental-construction-of-code-property-graphs-1> ·
<https://www.grin.com/document/1146231?lang=en> · <https://www.bodden.de/pubs/ab14reviser.pdf> ·
<https://github.com/ericbodden/incremental-ifds> ·
<https://szabta89.github.io/publications/inca-ase.pdf> ·
<https://szabta89.github.io/publications/inca-pldi2021.pdf> ·
<https://github.com/souffle-lang/souffle/discussions/2487>

Comparable tools: <https://docs.github.com/en/code-security/how-tos/find-and-fix-code-vulnerabilities/scan-from-the-command-line/incremental-analysis> ·
<https://codeql.github.com/docs/ql-language-reference/annotations/> ·
<https://github.blog/changelog/2026-06-10-incremental-analysis-for-go-c-c-and-codeql-cli/> ·
<https://githubnext.com/projects/incremental-codeql/> ·
<https://github.com/facebook/infer/blob/main/infer/src/base/Config.ml> ·
<https://fbinfer.com/man/next/infer-analyze.1.html> ·
<https://glean.software/docs/implementation/incrementality/> ·
<https://kythe.io/docs/kythe-overview.html> ·
<https://github.com/sourcegraph/scip-clang/issues/183> ·
<https://rust-analyzer.github.io/book/contributing/architecture.html> ·
<https://clangd.llvm.org/design/indexing> · <https://docs.semgrep.dev/cli-reference> ·
<https://docs.checkmarx.com/en/34965-324470-sast-scanner.html> ·
<https://www.microfocus.com/documentation/fortify-static-code-analyzer-and-tools/2010/SCA_Guide_20.1.2.pdf> ·
<https://docs.sonarsource.com/sonarqube-server/10.8/analyzing-source-code/incremental-analysis/introduction.md>

Not verified: Coverity's `cov-run-desktop` interprocedural-summary reuse (vendor documentation is a
JavaScript SPA behind a redirect; no archived copy reachable). It is not load-bearing for any
recommendation here.
