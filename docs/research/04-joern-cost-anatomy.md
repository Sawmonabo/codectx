# Joern cost anatomy, and cheaper routes to calls / control dependence / data dependence / reads-writes

Research note for the codectx product owner. Every non-obvious claim carries a URL.
Version discipline: codectx pins `4.0.*` ([`docs/providers-joern.md`](../../../docs/providers-joern.md)), so
source citations below are pinned to tag **`v4.0.100`** unless stated otherwise, and every
performance anecdote is dated — Joern replaced its storage engine in 4.0, which invalidates
most pre-2024 memory folklore.

---

## Two findings that precede the cost question

**A. The pinned profile cannot run against any real Joern 4.0.x.**
`joern-export --repr=pdg --format=graphml` is not a supported combination. In
[`JoernExport.scala` @ v4.0.100](https://github.com/joernio/joern/blob/v4.0.100/joern-cli/src/main/scala/io/joern/joerncli/JoernExport.scala),
`Format.Graphml` dispatches to `exportWithFlatgraphFormat`, which handles only
`Representation.All` and `Representation.Cpg` and otherwise throws:

```scala
case Format.Graphml => exportWithFlatgraphFormat(cpg, representation, outDir, GraphMLExporter)
...
case other => throw new NotImplementedError(s"repr=$repr not yet supported for this format")
```

PDG, DDG, CDG, AST, CFG and CPG14 are reachable **only** through `--format=dot`, via the
layer-creator path `exportDot`. A Joern maintainer confirmed the same restriction for the CSV
sibling in [joernio/joern#2571](https://github.com/joernio/joern/issues/2571) (2023-04-25):
*"This isn't a bug unfortunately - we simply don't currently support this combination (pdg,
neo4jcsv) yet."* The code is unchanged on
[master](https://github.com/joernio/joern/blob/master/joern-cli/src/main/scala/io/joern/joerncli/JoernExport.scala).
The doc's own "unverified against a real tool" marker is doing real work here.

**B. Even if it worked, the second export is redundant.** CDG edges are produced by `CdgPass`
inside the `ControlFlow` overlay, and `REACHING_DEF` edges by `ReachingDefPass` inside
`OssDataFlow` — both applied by `joern-parse`, both persisted into `cpg.bin`, and both therefore
already inside `--repr=all`, which exports the graph with no filtering at all
(`exporter.runExport(cpg.graph, outDir)`). Details in §2. **The cheapest possible change to the
Joern provider is to delete the PDG export entirely.** It roughly halves export wall-time and
disk, removes the GraphML importer, and fixes the blocking bug in the same stroke.

---

## 1. Joern architecture: where the minutes and the gigabytes go

### 1.1 Frontends — an entire real compiler front-end per language

`joern-parse` guesses a language and shells out to a frontend. From the pinned `build.sbt` files:

| Frontend | Parser it embeds or drives | Source |
|---|---|---|
| `c2cpg` | Eclipse CDT (`io.joern % eclipse-cdt-core`) plus `org.eclipse.core.resources` / `org.eclipse.text` | [c2cpg/build.sbt @ v4.0.100](https://github.com/joernio/joern/blob/v4.0.100/joern-cli/frontends/c2cpg/build.sbt) |
| `jssrc2cpg` | downloads a per-platform [`joernio/astgen`](https://github.com/joernio/astgen) binary; astgen uses the bundled Babel parser and *the TypeScript compiler's type-checker API* for type maps ([npm readme](https://www.npmjs.com/package/@joernio/astgen)) | [jssrc2cpg/build.sbt](https://github.com/joernio/joern/blob/v4.0.100/joern-cli/frontends/jssrc2cpg/build.sbt) |
| `javasrc2cpg` | `javaparser-symbol-solver-core`, plus `gradle-tooling-api`, `lombok`, `zip4j` — i.e. a full symbol solver and a build-system probe | [javasrc2cpg/build.sbt](https://github.com/joernio/joern/blob/v4.0.100/joern-cli/frontends/javasrc2cpg/build.sbt) |
| `pysrc2cpg` | no third-party parser dependency; Joern ships its own Python parser | [pysrc2cpg/build.sbt](https://github.com/joernio/joern/blob/v4.0.100/joern-cli/frontends/pysrc2cpg/build.sbt) |
| `gosrc2cpg` | downloads a per-platform [`joernio/goastgen`](https://github.com/joernio/goastgen) binary | [gosrc2cpg/build.sbt](https://github.com/joernio/joern/blob/v4.0.100/joern-cli/frontends/gosrc2cpg/build.sbt) |

There are 13 frontends in the tree (`c2cpg csharpsrc2cpg ghidra2cpg gosrc2cpg javasrc2cpg
jimple2cpg jssrc2cpg kotlin2cpg php2cpg pysrc2cpg rubysrc2cpg swiftsrc2cpg x2cpg`).

This is the first structural reason Joern is not an "instant" indexer: a tree-sitter pass reads
bytes and produces a concrete syntax tree with no name resolution; c2cpg runs a C/C++ *compiler
front end* including preprocessing and header resolution. A worked example from a current 4.0.x
release: on the RIOT-OS tree (**1,770 `.c` + 4,383 `.h` files**, 28,830 include directives,
OpenJDK 21, `-J-Xmx8g`, Ubuntu 24.04 aarch64), CPG build took **1717.5 s** stock and **211.9 s**
after a three-line fix to a Levenshtein sort in `HeaderFileFinder`, producing a 69 MB `cpg.bin`
([joernio/joern#6280](https://github.com/joernio/joern/issues/6280), v4.0.624, 2026). Two lessons:
(a) a ~6k-file C repo is minutes, not seconds, even in the good case; (b) frontend-level
pathologies, not the graph algorithms, produced the 8x factor — so "tens of minutes on 10k files"
is entirely plausible and is partly bad luck in header resolution.

### 1.2 The overlay pipeline — what `joern-parse` actually runs

`joern-parse` is `generateCpg` then `applyDefaultOverlays`
([`JoernParse.scala` @ v4.0.100](https://github.com/joernio/joern/blob/v4.0.100/joern-cli/src/main/scala/io/joern/joerncli/JoernParse.scala)):

```scala
if (config.enhance) {
  val cpg = DefaultOverlays.create(config.outputCpgFile, config.maxNumDef)
  generator.applyPostProcessingPasses(cpg)
  cpg.close()
}
```

and [`DefaultOverlays.create`](https://github.com/joernio/joern/blob/v4.0.100/joern-cli/src/main/scala/io/joern/joerncli/DefaultOverlays.scala) is:

```scala
val cpg = CpgBasedTool.loadFromFile(storeFilename)
applyDefaultOverlays(cpg)
val context = new LayerCreatorContext(cpg)
val options = new OssDataFlowOptions(maxNumberOfDefinitions)   // default 4000
new OssDataFlow(options).run(context)
```

`X2Cpg.defaultOverlayCreators()` is `List(new Base(), new ControlFlow(), new TypeRelations(), new CallGraph())`
([X2Cpg.scala](https://github.com/joernio/joern/blob/master/joern-cli/frontends/x2cpg/src/main/scala/io/joern/x2cpg/X2Cpg.scala)).
Expanded, the pinned pipeline is:

| Layer | Passes | Produces |
|---|---|---|
| `base` | FileCreationPass, NamespaceCreator, TypeDeclStubCreator, MethodStubCreator, ParameterIndexCompatPass, MethodDecoratorPass, AstLinkerPass, **ContainsEdgePass**, TypeRefPass, TypeEvalPass ([Base.scala](https://github.com/joernio/joern/blob/v4.0.100/joern-cli/frontends/x2cpg/src/main/scala/io/joern/x2cpg/layers/Base.scala)) | `CONTAINS` |
| `controlflow` | CfgCreationPass, CfgDominatorPass, **CdgPass** ([ControlFlow.scala](https://github.com/joernio/joern/blob/v4.0.100/joern-cli/frontends/x2cpg/src/main/scala/io/joern/x2cpg/layers/ControlFlow.scala) — *"Control flow layer (including dominators and CDG edges)"*) | `CFG`, `DOMINATE`, **`CDG`** |
| `typerel` | TypeHierarchyPass, AliasLinkerPass, FieldAccessLinkerPass | `INHERITS_FROM`, `ALIAS_OF` |
| `callgraph` | MethodRefLinker, StaticCallLinker, DynamicCallLinker | **`CALL`** |
| `dataflowOss` | **ReachingDefPass** ([OssDataFlow.scala](https://github.com/joernio/joern/blob/master/dataflowengineoss/src/main/scala/io/joern/dataflowengineoss/layers/dataflows/OssDataFlow.scala)) | **`REACHING_DEF`** |

All four fact families codectx wants (`CALL`, `CDG`, `REACHING_DEF`, plus `CONTAINS` for
attribution) are produced by `joern-parse`. `joern-export` adds nothing semantic.

### 1.3 Which passes dominate

**Scope note:** the table below is a *memory* attribution. **No per-pass wall-time breakdown is
published anywhere I could find.** The only measured time pathologies in current 4.0.x are
frontend-level, not pass-level — the RIOT-OS 8× was `HeaderFileFinder`, not an overlay. Do not
read this table as a time table.

Measured on `linux4/drivers`, post-flatgraph, by a Joern maintainer in
[joernio/joern#4256](https://github.com/joernio/joern/issues/4256) (opened 2024-03-01):

| Pass | Heap buildup | Extra heap retained |
|---|---|---|
| AstCreationPass | 46 G | 9 G |
| **ReachingDefPass** | **29 G** | 4 G |
| CfgDominatorPass | 17 G | 2 G |
| ContainsEdgePass | 9 G | 1 G |
| CfgCreationPass | 9 G | 1 G |

The mechanism matters: `ReachingDefPass` is a `ForkJoinParallelCpgPass[Method]` which, per that
issue, *"creates a large intermediate buffer with all consolidated changes… ideal for our many
small passes"* — i.e. every method's data-dependence diff is accumulated in heap before being
applied. The alternative (`ConcurrentWriterCpgPass`) *"is slow with flatgraph because it creates
many small diffs, and applying a diff is rather expensive in flatgraph."* That trade is the
memory profile of a Joern run.

`ReachingDefPass` solves a forward MOP dataflow problem per method
(`new DataFlowSolver().calculateMopSolutionForwards(problem)`) and **bails out** on any method
whose total generated-definition count exceeds `maxNumberOfDefinitions` (default **4000**),
logging *"X has more than 4000 definitions"*
([ReachingDefPass.scala](https://github.com/joernio/joern/blob/v4.0.100/dataflowengineoss/src/main/scala/io/joern/dataflowengineoss/passes/reachingdef/ReachingDefPass.scala)).
`joern-parse --max-num-def N` is therefore a **real, argv-only cost knob**: lowering it trades
data-dependence completeness on huge methods for time and heap.

### 1.4 Storage: flatgraph, and what that changed

Joern 4.0.x replaced OverflowDB with [flatgraph](https://github.com/joernio/flatgraph):
*"As of joern 4.0.x we replaced overflowdb with it's successor, flatgraph"*, and crucially
*"one of overflowdb's features was the overflowing-to-disk mechanism… in practice it was too slow
to be useful, so we didn't reimplement it in flatgraph"*
([4.0.0-flatgraph changelog](https://github.com/joernio/joern/blob/master/changelog/4.0.0-flatgraph.md)).
**Joern 4.0 has no spill-to-disk. A CPG that does not fit in heap is an OOM, full stop.**

Published [flatgraph benchmarks](https://flatgraph.joern.io/benchmarks/index.html), Linux 4.1.16
(48 M nodes with 630 M node properties, 431 M edges with 115 M edge properties):

| | OverflowDB | flatgraph |
|---|---|---|
| Final heap (post-GC) | 32.90 GB | 19.95 GB |
| Time | 18.18 min | 11.97 min |
| Serialized file | 2,554.64 MB | 624.93 MB |

So the modern engine is ~40% lighter and the on-disk `cpg.bin` is zstd-compressed columnar — but
the working set is still heap-resident.

### 1.5 JVM heap guidance

The official [installation docs](https://docs.joern.io/installation/) say code analysis "can
require lots of memory, and unfortunately, the JVM does not pick up the available amount of memory
by itself", prescribe `./joern -J-Xmx${N}G`, and give the Linux kernel as the worked example: give
c2cpg ~30 GB and Joern itself ~100 GB (80 GB noted as sufficient). Note also that `importCode`
spawns a *second* JVM with the same max-memory value, so peak RSS can be double the `-Xmx`.
Community reports are consistent: [#4611](https://github.com/joernio/joern/issues/4611) advises
running the frontend directly with `c2cpg.sh -J-Xmx30208m`;
[#5479](https://github.com/joernio/joern/issues/5479) (2025-05) is a `jimple2cpg` run on ZooKeeper
OOM-killed at `-J-Xmx9988m`; [#4625](https://github.com/joernio/joern/issues/4625) (2024-05)
describes a Linux-kernel session pinning a 200 GB heap.

**Speculation (flagged):** codectx's profile reserves `memory_budget_bytes = 8589934592` (8 GiB)
but the argv never passes `-J-Xmx`. The reservation is an accounting declaration to
`internal/process`, not a JVM setting, so the child JVM uses its default max heap (typically ¼ of
physical RAM). On a 64 GB machine that is ~16 GB — more than the declared budget; on an 8 GB CI
runner it is ~2 GB, which will OOM long before the declared budget is reached. This is worth
verifying on a real install.

---

## 2. Export cost: what `--repr` and `--format` actually do

The full matrices, from [JoernExport.scala @ v4.0.100](https://github.com/joernio/joern/blob/v4.0.100/joern-cli/src/main/scala/io/joern/joerncli/JoernExport.scala):

```scala
object Representation extends Enumeration { val Ast, Cfg, Ddg, Cdg, Pdg, Cpg14, Cpg, All = Value }
object Format         extends Enumeration { val Dot, Neo4jCsv, Graphml, Graphson = Value }
```

Note the negative the task asked about by name: **there is no `callgraph` repr.** The enum above is
exhaustive — call edges are only obtainable as part of `all`/`cpg`, or indirectly inside `pdg`'s
per-method DOT dumps.

Legal combinations:

| | `dot` | `neo4jcsv` | `graphml` | `graphson` |
|---|---|---|---|---|
| `ast` `cfg` `ddg` `cdg` `pdg` `cpg14` | ✅ per-method `.dot` via layer creators | ❌ `NotImplementedError` | ❌ | ❌ |
| `cpg` | ✅ (split by method) | ✅ (split by method) | ✅ | ✅ |
| `all` | ✅ | ✅ | ✅ | ✅ |

- **`--repr=all`** is literally `exporter.runExport(cpg.graph, outDir)` — every node, every edge,
  every property, no filter. There is no "export only these edge labels" option.
- **`--repr=cpg`** calls `splitByMethod`, which for each method takes `method.ast.toSet` and all
  induced edges, writing **one output file per method**. On a 10k-file repo that is ~10⁵ files.
  Strictly worse for codectx, and it drops every cross-method edge (`subGraph.edges` keeps only
  edges whose `dst` is inside the same method's AST) — which would silently delete the `CALL`
  edges the provider exists to import.
- **`--repr=cpg14`** is the default and is DOT-only per-method; it is the CPG-as-published-in-2014
  projection, not a cheaper serialization of the full graph.
- `exitIfInvalid` refuses to run if `--out` already exists (`"Output directory X already exists."`
  → `System.exit(1)`). codectx correctly does not pre-create them.

### Does the export recompute dataflow?

`exportCpg` begins with `CpgBasedTool.addDataFlowOverlayIfNonExistent(cpg)`
([CpgBasedTool.scala @ v4.0.100](https://github.com/joernio/joern/blob/v4.0.100/joern-cli/src/main/scala/io/joern/joerncli/CpgBasedTool.scala)):

```scala
if (!cpg.metaData.overlays.exists(_ == OssDataFlow.overlayName)) {
  System.err.println("CPG does not have dataflow overlay. Calculating.")
  ...
}
```

Because `joern-parse` already ran `OssDataFlow` **and** `flatgraph.Graph.close()` writes the graph
back when `hasChangedSinceOpen`
([Graph.scala](https://github.com/joernio/flatgraph/blob/master/core/src/main/scala/flatgraph/Graph.scala)),
the overlay marker is persisted and this check is a cheap no-op in codectx's flow. **But** if the
CPG were ever produced with `--nooverlays`, each `joern-export` would recompute the whole
reaching-definitions analysis *and* — because `Using.resource` closes the graph — rewrite the
entire `cpg.bin`. Worth knowing; not currently the case.

### How big is the CSV?

No published figure exists. **Estimate (no published figure):** `cpg.bin` is zstd-compressed
columnar storage; Neo4j CSV is uncompressed text that repeats a `:START_ID`/`:END_ID` per edge row
and a full property value per cell. Anchoring on the flatgraph Linux figures (625 MB for 48 M
nodes / 431 M edges / 745 M properties), a plausible CSV expansion is **10–40×** the `.bin`. For
the RIOT-OS anchor (69 MB `cpg.bin`, ~6k files), that projects to roughly **0.7–3 GB of CSV**, and
for a 10k-file repo, single-digit GB — consistent with the "gigabytes" the product owner reports.
Treat the multiplier as an estimate to be measured, not a citation.

### Avoiding the export

`joern --script` and the CPGQL server would let a query emit only `(method, method)` pairs for the
four families, at a fraction of the I/O. **This collides head-on with a standing codectx ruling**:
the package doc states verbatim *"There is no product-owned Scala or shell helper and no
interpreter server."* Recommending either means reversing that ruling. State the trade explicitly
rather than assuming it away (see §4).

A third option avoids the ruling: **`joern-slice`** is a built-in noninteractive CLI
([JoernSlice.scala @ v4.0.100](https://github.com/joernio/joern/blob/v4.0.100/joern-cli/src/main/scala/io/joern/joerncli/JoernSlice.scala))
with `data-flow` and `usages` subcommands, `--slice-depth` (default 20), `--file-filter`,
`--method-name-filter`, `--sink-filter`, `-p/--parallelism`, and **JSON output**. It also
auto-applies overlays if missing. It is a different shape from raw edges — per-method DDG slices
and per-object usage records — but it is product-owned argv against a shipped binary, exactly like
`joern-export`.

---

## 3. Cheaper routes to the same four fact families

### 3.1 What each family actually requires

| Fact family | Minimum machinery | Cheapest sound-ish source |
|---|---|---|
| **calls** | name resolution (not dataflow) | SCIP `Definition`/reference occurrences; LSP `callHierarchy`; `go/callgraph` |
| **control dependence** | CFG + post-dominator tree, **per function, no cross-file info** | tree-sitter CST → CFG → CDG, in-process |
| **data dependence (intra)** | CFG + def/use sets, per function | tree-sitter CST → def-use; Semgrep CE taint |
| **reads / writes** | name resolution + syntactic access position | SCIP `SymbolRole` bits (see caveat) |

The key asymmetry: **control dependence and intraprocedural data dependence are strictly local**.
They need one function's syntax and nothing else. Joern charges whole-program price for them
because it computes them as overlays on a whole-program graph. That is the single largest
mispricing in the current design.

### 3.2 SCIP gives calls and *may* give reads/writes

[`scip.proto`](https://github.com/sourcegraph/scip/blob/main/scip.proto) defines role bitflags:

```proto
enum SymbolRole {
  Definition = 0x1;  Import = 0x2;
  WriteAccess = 0x4; ReadAccess = 0x8;
  Generated = 0x10;  Test = 0x20;  ForwardDefinition = 0x40;
}
```

So the **format** carries reads/writes — the fact family codectx's Joern provider explicitly
cannot publish ("Joern has no READ or WRITE edge… would be a guess").

**Caveat, and it is a big one.** A GitHub code search across the three indexers codectx already
supports found `WriteAccess`/`ReadAccess` referenced **only** in
`sourcegraph/scip-typescript`'s `src/scip.ts` — which is the generated protobuf binding file, not
call-site usage — and **not at all** in `sourcegraph/scip-go` or `sourcegraph/scip-java`. Read
that as: *the bits exist in the schema and are probably not populated by these indexers.* This is
**weak evidence** and should not be treated as settled: GitHub code search covers default branches
only, has spotty indexing, and my confirming queries (`symbol_roles`, `symbolRoles`) were cut off
by an API rate limit before completing. **Decide this by dumping a real `.scip` index and checking
whether any occurrence carries `symbol_roles & 0xC`, not from this search.** Practically,
**calls are derivable from SCIP** — a reference occurrence
inside a definition's range, resolved to the referenced symbol's definition, is a call edge at
import time with zero extra analysis — but **reads/writes are not, today, from these indexers.**

Performance anchor for SCIP: migrating the Sourcegraph monorepo from `lsif-node` to
`scip-typescript` took indexing from ~40 min across 12 parallel jobs to **~5 min in one job**, at
**1k–5k lines/second**; Sentry (>1.2 M LOC) indexes in **under 12 min**, Excalidraw in ~35 s
([Sourcegraph, announcing scip-typescript](https://sourcegraph.com/blog/announcing-scip-typescript)).

### 3.3 LSP `callHierarchy` — on-demand, zero index

`textDocument/prepareCallHierarchy` → `callHierarchy/incomingCalls` / `outgoingCalls`
([LSP 3.17](https://microsoft.github.io/language-server-protocol/specifications/lsp/3.17/specification/))
answer per symbol, on demand, from a server that is already warm. codectx already has this as an
ephemeral overlay (`internal/provider/lsp`). It is the cheapest possible *call* answer for an
interactive query and the worst possible source for a bulk precomputed relation table (N round
trips for N symbols).

### 3.4 Language-specific call graphs

`golang.org/x/tools/go/callgraph` ships four algorithms in increasing precision and cost: `static`,
[`cha`](https://pkg.go.dev/golang.org/x/tools/go/callgraph/cha),
[`rta`](https://pkg.go.dev/golang.org/x/tools/go/callgraph/rta),
[`vta`](https://pkg.go.dev/golang.org/x/tools/go/callgraph/vta). CHA is *sound on partial programs*
— libraries with no `main` — which matters for a repo indexer; RTA needs a whole program. The
`cmd/callgraph` docs report **~2.1 s for RTA** vs ~5.4 s for full points-to on the tool's own
source. Equivalents elsewhere: WALA/Soot/SootUp (Java), PyCG (Python), rust-analyzer, clangd.
Each is precise and each is a separate integration.

**Infer** deserves a separate mention because it is the one alternative with genuine incremental
analysis. Its compositional, separation-logic design analyses each function independently against
a summary, which is exactly what makes `--reactive` mode possible: the
[recommended CI flow](https://fbinfer.com/docs/next/steps-for-ci/) is to determine the modified
files and re-analyse starting from those, and the differential workflow reports only issues a
change introduced. That is the property Joern lacks (§4.3) and the reason Infer scales at Meta
([Scaling Static Analyses at Facebook, CACM 2019](https://m-cacm.acm.org/magazines/2019/8/238344-scaling-static-analyses-at-facebook/fulltext)).
It is not a drop-in for codectx — it needs a capture of real compilation commands, covers
Java/C/C++/Obj-C, and emits *issues*, not a queryable relation graph — but it is the existence
proof that "whole-program rebuild" is a Joern design choice, not a law of static analysis.

Tree-sitter-only call graphs ([Nuanced](https://github.com/nuanced-dev/nuanced-py),
[tree-sitter-analyzer](https://github.com/aimasteracc/tree-sitter-analyzer)) are fast and
name-heuristic — they mis-wire on overloads, interfaces and same-named methods. codectx already
publishes `syntax`-precision call sites from tree-sitter, so this tier is already covered.

### 3.5 Semgrep and CodeQL

Semgrep's open-source taint mode is **intraprocedural only** — taint propagators *"only work
intraprocedurally, that is, within a function or method"*; crossing functions requires the
proprietary Pro Engine (`--pro-intrafile` for interprocedural/intra-file, `--pro` for inter-file)
([Semgrep taint docs](https://semgrep.dev/docs/writing-rules/data-flow/taint-mode/overview),
[Pro Engine](https://semgrep.dev/products/pro-engine/)). Note this is *the same scope* as the
`REACHING_DEF` walk codectx imports from Joern — the provider doc is explicit that its
`data_flows_to` is "intraprocedural def-use reachability". **Joern is being paid whole-program
prices for an intraprocedural fact.**

CodeQL is not cheaper. GitHub's official
[recommended hardware](https://docs.github.com/en/code-security/reference/code-scanning/codeql/recommended-hardware-resources-for-running-codeql):
<100K LOC → 8 GB / 2 cores; 100K–1M LOC → 16 GB / 4–8 cores; >1M LOC → **64 GB / 8 cores**; SSD
with ≥14 GB in all cases. It also requires a *working build* for compiled languages, which Joern
does not. CodeQL buys soundness and a query language; it does not buy speed.

### 3.6 Order-of-magnitude comparison

10k-file repo, mixed languages. **Cells marked (est.) have no published figure and are
extrapolations; cells with a source are measured.**

| Approach | Wall time | Peak RAM | On-disk artifact | Gives calls? | ctrl dep? | data dep? | reads/writes? | Incremental? |
|---|---|---|---|---|---|---|---|---|
| tree-sitter structural (codectx today) | seconds–low minutes (est.) | ~100s MB, bounded per worker (est.) | small | syntactic only | ✗ (derivable, see §4) | ✗ (derivable) | ✗ (derivable) | yes, per file |
| SCIP indexer (`scip-typescript`) | **~5 min** for the Sourcegraph monorepo; **<12 min** for Sentry @1.2M LOC; 1k–5k LOC/s ([src](https://sourcegraph.com/blog/announcing-scip-typescript)) | Node heap; tune `--max-old-space-size` (est.) | 10s–100s MB (est.) | ✔ precise | ✗ | ✗ | schema yes / indexers **unverified** (§3.2) | per-package, not per-file |
| LSP `callHierarchy` on demand | ms per symbol after warm-up; server start is the cost | one language server (est.) | none | ✔ precise, per query | ✗ | ✗ | ✗ | inherently live |
| `go/callgraph` RTA (Go only) | **~2.1 s** on `cmd/callgraph`'s own source ([rta docs](https://pkg.go.dev/golang.org/x/tools/go/callgraph/rta)) | modest (est.) | none | ✔ | ✗ | ✗ | ✗ | no |
| Infer (compositional, Java/C/C++/Obj-C) | not published per-repo | not published | `infer-out/` summaries | ✔ | internal | ✔ inter, compositional | ✗ | **yes — `--reactive` / differential** ([CI flow](https://fbinfer.com/docs/next/steps-for-ci/)) |
| **Joern parse → `cpg.bin`** | **211.9 s** (post-fix) / **1717.5 s** (stock) for 1,770 `.c` + 4,383 `.h` files @ `-Xmx8g` ([#6280](https://github.com/joernio/joern/issues/6280)); 11.97 min for Linux 4.1.16 ([flatgraph](https://flatgraph.joern.io/benchmarks/index.html)) | dominated by AstCreationPass 46 G / ReachingDefPass 29 G buildup on linux4/drivers ([#4256](https://github.com/joernio/joern/issues/4256)); **19.95 GB** heap for Linux 4.1.16 | 69 MB (RIOT-OS); 625 MB (Linux) | ✔ | ✔ | ✔ intra | ✗ no READ/WRITE edge | **no** (§4.3) |
| **+ `--repr=all --format=neo4jcsv`** | additional full graph serialization (est. minutes) | graph must be heap-resident; no spill-to-disk since 4.0 ([changelog](https://github.com/joernio/joern/blob/master/changelog/4.0.0-flatgraph.md)) | **10–40× the `.bin` (est.)** → single-digit GB | — | — | — | — | no |
| **+ `--repr=pdg --format=graphml`** | **throws `NotImplementedError`** ([JoernExport.scala](https://github.com/joernio/joern/blob/v4.0.100/joern-cli/src/main/scala/io/joern/joerncli/JoernExport.scala), [#2571](https://github.com/joernio/joern/issues/2571)) | — | — | — | — | — | — | — |
| CodeQL DB create | not published per-repo; official guidance below | **8 / 16 / 64 GB** by <100K / 100K–1M / >1M LOC ([GitHub docs](https://docs.github.com/en/code-security/reference/code-scanning/codeql/recommended-hardware-resources-for-running-codeql)) | ≥14 GB SSD recommended | ✔ | ✔ | ✔ inter | ✔ | no; needs a build |

---

## 4. Recommendations

### 4.1 Fix the blocking bug by deleting work (do this first)

Change `PinnedDefault()` to a single export and drop `ExportPDGArgs`, `graphml.go`, and the
GraphML branch of the importer:

```
parse:  joern-parse  ${input_dir} --output ${cpg}
export: joern-export ${cpg} --repr=all --format=neo4jcsv --out ${out_dir}
```

Justification, all cited above: `--repr=pdg --format=graphml` throws; and `--repr=all` already
contains `CDG` (from `CdgPass` in the `controlflow` layer) and `REACHING_DEF` (from
`ReachingDefPass` in `dataflowOss`), both applied by `joern-parse` and persisted by
`flatgraph.Graph.close()`. This is the "one-line change to `PinnedDefault()`" the doc already
anticipates. Expected effect: the provider starts working at all, and export wall-time and disk
roughly halve.

**Verify on a real install** that `edges_CDG_*.csv` and `edges_REACHING_DEF_*.csv` appear in
`export-all`. There is a 2023 open report that Java exports lacked DDG edges in `all`
([#2480](https://github.com/joernio/joern/issues/2480)) — pre-flatgraph and unconfirmed, but it is
the one claim in this note that would invalidate the recommendation if it still reproduces.

**If that verification fails**, the fallback is `joern-export ${cpg} --repr=pdg --format=dot --out
${out_dir}` — per §2, DOT is the *only* format for which `pdg` is implemented (`exportDot` →
`new DumpPdg(PdgDumpOptions(outDirStr)).create(context)`, one `.dot` file per method). That
replaces `graphml.go` with a DOT importer rather than reinstating the broken GraphML argv, and the
run still has one export *plus* one fallback rather than today's two full-graph exports, since
`--repr=all` remains the source of `CALL`, `CONTAINS` and all node attributes. Note the DOT dumps
are per-method files, so the import side gains a file-count bound where it loses a record bound.

Consequently **deleting `graphml.go` is conditional on the verification passing.** Sequence it:
run the smoke commands first, delete second.

### 4.2 Add the two argv-only cost knobs

Both are pure `PinnedDefault()` changes, no Scala, no ruling reversal:

1. **`joern-parse --max-num-def N`** — lowers the `ReachingDefPass` bail-out from 4000. The pass is
   the #2 heap consumer (29 G buildup on linux4/drivers). Lowering it to e.g. 500 skips exactly the
   giant generated functions where a `data_flows_to` edge is least useful anyway.
2. **`--frontend-args`** (`CpgBasedTool.ARGS_DELIMITER`) — pass frontend excludes for
   `vendor/`, `node_modules/`, `third_party/`, generated code. The RIOT-OS case shows header/file
   count driving build time superlinearly; excluding vendored trees is the highest-leverage input
   reduction available.
3. Consider passing **`-J-Xmx`** explicitly rather than relying on the JVM default (§1.5,
   flagged as speculation — verify what the child JVM actually gets).

### 4.3 Joern has no incremental mode. Do not design around one.

There is no incremental CPG API. The evidence is a **feature request opened 2026-03-08 and still
open**: [joernio/joern#5865](https://github.com/joernio/joern/issues/5865), "Incremental CPG update
for javasrc2cpg (file-level re-indexing without full rebuild)". Its author reports a working
prototype at *"~350ms per file vs ~10min for a full rebuild (~1700× speedup)"* and has to ask
whether the HTTP endpoint "belongs in `joern-cli` or is out of scope". An earlier "Incremental work
on x2cpg" ([#1184](https://github.com/joernio/joern/issues/1184), 2022) is closed without shipping
one. Conclusion: **no documented support**, with the open request as the citation — not an argument
from silence.

Consequence for codectx: the current workspace-scoped, rebuild-everything unit is the *correct*
model for Joern, and the invalidation cost is irreducible. That is the strongest argument for
demoting Joern rather than tuning it.

**Per-package sharding is possible but changes the facts.** Running `joern-parse` per Go
module / per Maven module / per npm package would cut peak heap (the dominant constraint, since
4.0 has no spill-to-disk) and allow parallelism and partial invalidation. The cost: `CALL` edges
crossing package boundaries vanish, and `IS_EXTERNAL` stubs multiply. Since codectx's
`data_flows_to` and `control_depends_on` are already intraprocedural, they lose **nothing** under
sharding. Only `calls` degrades. **Recommendation: shard by package, and get cross-package calls
from SCIP instead of Joern.**

### 4.4 Which facts to keep in Joern, and which to move

| Fact | Keep in Joern? | Why |
|---|---|---|
| `calls` | **No — move to SCIP** | SCIP occurrences + `Definition` role give precise, compiler-precision call edges derivable at import with no analysis. Joern's `CALL` edges are `compiler`-grade only where the frontend resolved types; codectx already runs `scip-go`/`scip-typescript`/`scip-java`. Paying a whole-program CPG for call edges is the clearest waste. |
| `control_depends_on` | **Yes, for now** — but it is the best candidate to reimplement | Purely per-function (CFG + post-dominators). A tree-sitter CST already gives the statement structure; a per-function CFG + CDG in Go is a bounded, in-process pass with no JVM. |
| `data_flows_to` | **Yes** — hardest to replace honestly | Intraprocedural def-use. Reimplementable per-function on tree-sitter, but the assignment/field-access shapes are exactly the frontend-specific guesswork the doc already refuses to do for reads/writes. Keeping Joern here is defensible. |
| `reads` / `writes` | **Not available from Joern at all** | No READ/WRITE edge exists. SCIP's `ReadAccess`/`WriteAccess` bits are the right home — **but verify indexer support first (§3.2); the negative code-search result says they are not populated today.** |

### 4.5 The most elegant reduction, in order of cost

1. **Delete the PDG export.** Fixes the bug, halves the export. No ruling reversal. (§4.1)
2. **Add `--max-num-def` and `--frontend-args` excludes.** Argv only. (§4.2)
3. **Move `calls` to SCIP; shard Joern per package.** Joern then computes only intraprocedural
   facts on inputs small enough to fit comfortably in heap. (§4.3–4.4)
4. **Reimplement `control_depends_on` on tree-sitter.** Removes the largest per-function overlay
   from Joern's critical path and makes the fact file-scoped and incrementally invalidatable —
   matching codectx's `filesystem`/`treesitter` model instead of fighting it.
5. **Only if 1–4 are insufficient: reverse R11-1** and either (a) ship a pinned `joern-slice`
   invocation — `data-flow` mode with `--file-filter` and `--slice-depth`, JSON out, still a
   product-owned argv against a shipped binary — or (b) own one bounded `.sc` script that emits
   `(caller, callee)`, `(cdg src, dst)`, `(def, use)` triples as CSV and nothing else. Option (b)
   is by far the cheapest in I/O and by far the most expensive in policy. Present it to the
   controller as an explicit trade, not a default.

**The one-sentence answer to the product owner.** Joern is slow because it runs a real compiler
front end per language, then materializes an *entire program* in a JVM-heap-resident graph so it
can compute five overlay layers, two of which (reaching definitions, AST construction) dominate
heap, with no incremental mode and — since 4.0 — no spill to disk; and then codectx pays a second
time to serialize that whole graph to text. "Instant" indexers skip all of it: they resolve names
and stop. Of the four fact families, only intraprocedural data dependence genuinely needs what
Joern builds, and codectx is currently buying the other three at the same price.

---

*Sources are inline. Code citations pinned to `v4.0.100` unless noted. Estimates are marked
`(est.)`. Research date: 2026-09-13.*
