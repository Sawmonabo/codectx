# Joern: analysis units, laziness, and bounded extraction

Answers to the three reviewer questions raised against `internal/provider/joern`. Every
non-obvious claim carries a verbatim quote and a URL. Source citations are pinned to tag
**`v4.0.100`** (codectx pins `4.0.*`). Numbers I could not find published are marked `(est.)`
or left as "unmeasured".

---

## 0. Three corrections to note 04, up front

**C1. Cross-unit `CALL` edges do not vanish under sharding — they degrade to `IS_EXTERNAL`
stubs.** Note 04 §4.3 says *"`CALL` edges crossing package boundaries vanish."* That is wrong.
`MethodStubCreator` runs in the `base` layer, **before** the `callgraph` layer, and mints a
METHOD node for every call whose `methodFullName` matches no METHOD in the CPG
([MethodStubCreator.scala](https://github.com/joernio/joern/blob/v4.0.100/joern-cli/frontends/x2cpg/src/main/scala/io/joern/x2cpg/passes/base/MethodStubCreator.scala)):

```scala
for (
  (CallSummary(name, signature, fullName, dispatchType), parameterCount) <- methodToParameterCount
  if !methodFullNameToNode.contains(fullName)
) { createMethodStub(name, fullName, signature, dispatchType, parameterCount, dstGraph) }
```

and `createMethodStub` has `isExternal: Boolean = true`. `StaticCallLinker` then resolves
against the stub, because it matches by full name alone
([StaticCallLinker.scala](https://github.com/joernio/joern/blob/v4.0.100/joern-cli/frontends/x2cpg/src/main/scala/io/joern/x2cpg/passes/callgraph/StaticCallLinker.scala)):

```scala
case DispatchTypes.STATIC_DISPATCH | DispatchTypes.INLINED =>
  val resolvedMethods = cpg.method.fullNameExact(call.methodFullName).l
  resolvedMethods.foreach(dst => builder.addEdge(call, dst, EdgeTypes.CALL))
```

So the edge survives; the target becomes an `isExternal` stub. **It does not always lose its
location, and that is a hazard, not a relief.** `createMethodStub` calls `addLineNumberInfo`,
which parses the full name:

```scala
val s = fullName.split(":")
if (s.size == 5 && Try { s(1).toInt }.isSuccess && Try { s(2).toInt }.isSuccess) {
  methodNode.filename(s(0)).lineNumber(s(1).toInt).lineNumberEnd(s(2).toInt)
}
```

For frontends whose `FULL_NAME` follows the `path:line:lineEnd:…` convention (the c2cpg/fuzzyc
shape), the stub carries `FILENAME`, `LINE_NUMBER` and `LINE_NUMBER_END`, so
`docs/providers-joern.md`'s *"`IS_EXTERNAL` methods (library targets) have no location by
design"* is conditionally false. The consequence is concrete: `emit.go` drops any method whose
`FILENAME` is not a file of the snapshot **together with every relation touching it** and reports
`CTX_SOURCE_BINDING_UNVERIFIED` → `partial`. A C/C++ stub minted from an out-of-shard header
path trips exactly that, and it is untested which `emit.go` branch such a stub takes — "Located
method" (strong key) or "External method" (`ScopeWorkspace`). This strengthens R1's "do not
shard C/C++".

Otherwise: `emit.go` already publishes external methods with `ScopeKey =
provider.ScopeWorkspace`, `NativeKey = joern:method:<FULL_NAME>` and no strong key, so a
sharded run still emits those `calls` relations — and because every method publishes an alias
`(ScopeKey, NativeKey) → NodeID`, a *different* shard that contains the callee as a located
method can resolve to the same node, provided the two frontends mint identical `FULL_NAME`
strings. Sharding therefore converts "cross-package call" from a compiler-precision fact into a
cross-unit identity-join problem, which is a much better failure mode than deletion.

**C2. `REACHING_DEF` is not purely intraprocedural.** `docs/providers-joern.md` states
`data_flows_to` "is **intraprocedural def-use reachability**". Joern's own `DdgGenerator` says
otherwise, in a comment it wrote against itself
([DdgGenerator.scala](https://github.com/joernio/joern/blob/v4.0.100/dataflowengineoss/src/main/scala/io/joern/dataflowengineoss/passes/reachingdef/DdgGenerator.scala)):

```scala
// NOTE: Below connects REACHING_DEF edges between method boundaries of closures. In the case of PARENT -> CHILD
// this brings no inconsistent flows, but from CHILD -> PARENT we have observed inconsistencies. This form of
// modelling data-flow is unsound as the engine assumes REACHING_DEF edges are intraprocedural.
// See PR #3735 on Joern for details
```

The same method also links `param.capturedByMethodRef.referencedMethod.ast.isIdentifier` and
global identifiers to first usages in other methods. codectx's walk does not require both ends
in one method, so the provider can emit a cross-method `data_flows_to` whose evidence string
says `intraprocedural`. Fix the string or constrain the walk to one `METHOD`.

**C3. Quote the right exception.** For `--repr=pdg --format=graphml` the throw that fires is
the inner one in `exportWithFlatgraphFormat`, not `exportCpg`'s outer one (all four `Format`
values are matched, so the outer `case other` is unreachable)
([JoernExport.scala](https://github.com/joernio/joern/blob/v4.0.100/joern-cli/src/main/scala/io/joern/joerncli/JoernExport.scala)):
`throw new NotImplementedError(s"repr=$repr not yet supported for this format")`.

---

## Q1. The correct analysis unit per frontend, and what a subset costs

### 1.1 The three-tier degradation mechanism

Everything in this section reduces to which of three tiers a fact falls into.

| Tier | Pass | Needs | Behaviour when the callee/type is outside the CPG |
|---|---|---|---|
| **T1 static calls** | `StaticCallLinker` | callee METHOD in *this* CPG, matched by `fullNameExact` | `MethodStubCreator` supplies an `isExternal` stub; edge exists, target has no location |
| **T2 dynamic calls** | `DynamicCallLinker` | `TypeDecl` hierarchy in *this* CPG for *precise* targets | falls back to the stub by full name; **absent only** when `methodFullName` is `<empty>`/`<unknownFullName>` |
| **T3 per-method analyses** | `CdgPass`, `ReachingDefPass` | one method's CFG only | unaffected by what else is in the CPG |

T2 is more forgiving than note 04 and my own first draft assumed. `DynamicCallLinker` computes
precise candidates from the inheritance graph it can see — *"We compute the set of possible
call-targets for each dynamic call … based on call.methodFullName, method.name and
method.signature, the inheritance hierarchy and the AST of typedecls and methods"* — so an
interface whose implementations live in another shard yields no *precise* targets. But it then
falls back
([DynamicCallLinker.scala](https://github.com/joernio/joern/blob/v4.0.100/joern-cli/frontends/x2cpg/src/main/scala/io/joern/x2cpg/passes/callgraph/DynamicCallLinker.scala)):

```scala
/** In the case where the method isn't an internal method and cannot be resolved by crawling TYPE_DECL nodes it can be
  * resolved from the map of external methods. */
private def fallbackToStaticResolution(call: Call, dstGraph: DiffGraphBuilder): Unit = {
  methodMap.get(call.methodFullName) match {
    case Some(tgtM) => dstGraph.addEdge(call, tgtM, EdgeTypes.CALL)
    case None       => printLinkingError(call)
  }
}
```

`methodMap` is filled by `initMaps()` from every non-`<operator>` method — stubs included — so a
dynamic call normally lands on the stub. Genuine absence has two causes, and neither is
sharding: the early return
`if (call.methodFullName.equals("<empty>") || call.methodFullName.equals(DynamicCallUnknownFullName)) return`,
and a `methodFullName` no stub was minted for. Both are **type-recovery failures**, which is why
`--nooverlays` (§2.3) and missing dependency jars hurt `calls` more than sharding does. Note the
linking error is logged at `info`, not `warn` — invisible at default log levels.

T3 is the good news, and it is stronger than note 04 assumed. `ReachingDefPass` is
`ForkJoinParallelCpgPass[Method]` with `generateParts() = cpg.method.toArray`, and
`ReachingDefProblem.scala` contains **no reference to `Semantics`** at all — I grepped it. The
`gen`/`kill` sets are computed from the method's own CFG. `DdgGenerator` does receive semantics,
but it deliberately declines to use them when building edges:

```scala
// For all calls, assume that input arguments
// taint corresponding output arguments
// and the return value. We filter invalid
// edges at query time (according to the given semantic).
```

That settles the reviewer's sharpest question: **REACHING_DEF edge construction is
callee-independent.** Whether `foo()` resolves to a located METHOD or an external stub does not
change the edges written into the CPG. So `data_flows_to` and `control_depends_on` survive
sharding *for every language except C/C++* (§1.2) — cited, not assumed.

### 1.2 Per-frontend units

| Frontend | Correct semantic unit | What the frontend already does | Lost on a subset |
|---|---|---|---|
| **c2cpg** | translation unit + its reachable headers | `HeaderFileFinder` indexes **every** header under the input root by *basename* and picks the Levenshtein-nearest path; `--include` (unbounded), `--define`, `--with-include-auto-discovery` (**off by default**) | **everything, including T3** — see below |
| **javasrc2cpg** | Maven/Gradle module (source root + classpath) | `EagerSourceTypeSolver` over all parsed sources, `JdkJarTypeSolver.fromJdkPath(jdkPath)` defaulting to `System.getProperty("java.home")`, `JarTypeSolver` per `--inference-jar-paths` and per fetched dependency | T2 (interface/override dispatch), external type names |
| **jssrc2cpg** | npm package / tsconfig project | astgen runs the TypeScript type-checker unless `--no-tsTypes`; `node_modules/.*` is **excluded by default** | T2 and type names; `node_modules` never contributes METHOD nodes even unsharded |
| **pysrc2cpg** | package + virtualenv | `--venvDirs`, `--ignoreVenvDir`, `--ignore-paths`, `--ignore-dir-names` | T2, type recovery quality |
| **gosrc2cpg** | **Go module** — and the frontend already shards by it | `segregateByModule` groups parsed files under each `go.mod`, *"This will also segregate modules defined inside another module"*, returning a `List[GoAstGenRunnerResult]` | cross-module calls (T1→stub, T2→absent) |
| **rubysrc2cpg** | gem / `Gemfile` project | `--download-dependencies` — *"Download the dependencies of the target project and use their symbols to resolve types"* — type stubs on by default (`useTypeStubs = true`), default ignores `spec test tests vendor` | as Python |

Sources: [c2cpg/Main.scala](https://github.com/joernio/joern/blob/v4.0.100/joern-cli/frontends/c2cpg/src/main/scala/io/joern/c2cpg/Main.scala),
[HeaderFileFinder.scala](https://github.com/joernio/joern/blob/v4.0.100/joern-cli/frontends/c2cpg/src/main/scala/io/joern/c2cpg/parser/HeaderFileFinder.scala),
[javasrc2cpg AstCreationPass.scala](https://github.com/joernio/joern/blob/v4.0.100/joern-cli/frontends/javasrc2cpg/src/main/scala/io/joern/javasrc2cpg/passes/AstCreationPass.scala),
[jssrc2cpg AstGenRunner.scala](https://github.com/joernio/joern/blob/v4.0.100/joern-cli/frontends/jssrc2cpg/src/main/scala/io/joern/jssrc2cpg/utils/AstGenRunner.scala),
[pysrc2cpg Main.scala](https://github.com/joernio/joern/blob/v4.0.100/joern-cli/frontends/pysrc2cpg/src/main/scala/io/joern/pysrc2cpg/Main.scala),
[gosrc2cpg AstGenRunner.scala](https://github.com/joernio/joern/blob/v4.0.100/joern-cli/frontends/gosrc2cpg/src/main/scala/io/joern/gosrc2cpg/utils/AstGenRunner.scala),
[rubysrc2cpg Main.scala](https://github.com/joernio/joern/blob/v4.0.100/joern-cli/frontends/rubysrc2cpg/src/main/scala/io/joern/rubysrc2cpg/Main.scala),
`DependencyDownloadConfig.parserOptions` in
[X2Cpg.scala](https://github.com/joernio/joern/blob/v4.0.100/joern-cli/frontends/x2cpg/src/main/scala/io/joern/x2cpg/X2Cpg.scala).

**c2cpg is the exception that breaks the "T3 is safe" rule.** Header resolution is not a
type-lookup bolted on after parsing; it is preprocessing, and preprocessing decides what source
text exists. `HeaderFileFinder.find` is a *basename* lookup over the whole input tree:

```scala
def find(path: String): Option[String] = File(path).nameOption.flatMap { name =>
  val matches = nameToPathMap.getOrElse(name, List())
  matches.map(_.toString).sortBy(x => Levenshtein.distance(x, path)).headOption
}
```

Two consequences. (i) Shrinking the input tree removes candidate headers, so `#include`s go
unresolved, macros stay unexpanded, and the AST — hence the CFG, hence the **CDG** — changes.
Control dependence is *not* shard-safe for C/C++. (ii) Matching by basename with a Levenshtein
tiebreak can resolve an include to the *wrong* file; this is the same code path whose sort
produced the 8× slowdown in [joernio/joern#6280](https://github.com/joernio/joern/issues/6280).
For C/C++: pass real `--include` paths through `--frontend-args`, keep the unit at least as
large as the compilation's include closure, and report any smaller shard as `partial`.

**On jssrc2cpg and `node_modules`, separate two questions.** What I verified is that
`node_modules${sep}.*` is in `AstGenDefaultIgnoreRegex`, so those files never become METHOD
nodes — true with or without sharding. What I did **not** verify is whether astgen's TypeScript
type-checker still *reads* `node_modules` from disk for import and type resolution when it is
present. It almost certainly does (that is how `tsc` resolves imports), which means codectx has
a distinct exposure: `snapshot.Materialize` materializes the pinned view, `node_modules` is
normally gitignored and therefore absent, so **TypeScript types may be degraded for every
JS/TS unit regardless of sharding.** Flagged unverified; settle it by diffing a `cpg.bin` built
with and without `node_modules` present.

**javasrc2cpg does not need a build**, which is its main advantage over CodeQL, but it needs
jars for external types. `--fetch-dependencies` is *"attempt to fetch dependencies jars for
extra type information"*, and there is a second, environment-driven route:

```scala
case FetchDependencies extends JavaSrcEnvVar(
  "JAVASRC_FETCH_DEPENDENCIES",
  "If set, javasrc2cpg will fetch dependencies regardless of the --fetch-dependencies flag.")
```

**Policy collision, stated not resolved.** Both `javasrc2cpg --fetch-dependencies` and
`gosrc2cpg --fetch-dependencies` (which drives `DownloadDependenciesPass`) require network, and
the pinned profile mandates `network = "denied"` for both analyzers — a declaration the docs
themselves admit "is not an OS sandbox". codectx's `env_allowlist = ["JAVA_HOME", "PATH"]`
usefully blocks `JAVASRC_FETCH_DEPENDENCIES` and `JAVASRC_JDK_PATH` from leaking in, and
allowing `JAVA_HOME` is what lets `System.getProperty("java.home")` find the JDK type solver.
The trade is: **deny network and accept `ANY`-typed externals and weaker dynamic dispatch, or
allow a network-enabled Joern and get real types.** There is no third option inside Joern.

### 1.3 Loss per fact family

| Fact family | C/C++ subset | Java / Go / JS / Py / Ruby subset |
|---|---|---|
| `calls` (static) | present, target may be a wrong-header stub | present, cross-unit target becomes `IS_EXTERNAL` stub (no `FILENAME`) |
| `calls` (dynamic/virtual) | degraded to stub targets; precise overrides lost | degraded to stub targets; **absent** only where type recovery left `methodFullName` unknown |
| `control_depends_on` (CDG) | **degraded** — unresolved includes change the CFG | **unchanged** — `CdgPass` is per method |
| `data_flows_to` (REACHING_DEF) | **degraded**, same reason | **unchanged** — construction is callee-independent |
| `reads` / `writes` | not published by this provider in any configuration (no READ/WRITE edge exists) | same |

---

## Q2. Can Joern be fully on-demand and cached per (unit, content hash)?

**Short answer: cacheable yes, free no, two-phase no (at `4.0.100`).**

### 2.1 Overlays *do* persist in `cpg.bin`

`joern-parse` applies them and closes the graph
([JoernParse.scala](https://github.com/joernio/joern/blob/v4.0.100/joern-cli/src/main/scala/io/joern/joerncli/JoernParse.scala)):
`val cpg = DefaultOverlays.create(config.outputCpgFile, config.maxNumDef); generator.applyPostProcessingPasses(cpg); cpg.close()`.
Flatgraph's `close` writes back exactly when something changed
([flatgraph Graph.scala](https://github.com/joernio/flatgraph/blob/master/core/src/main/scala/flatgraph/Graph.scala)):

```scala
override def close(): Unit = {
  this.closed = true
  for { storagePath <- storagePathMaybe; if hasChangedSinceOpen } {
    Serialization.writeGraph(this, storagePath)
```

So both re-application guards are cheap no-ops against a warm `cpg.bin`:
`CpgBasedTool.addDataFlowOverlayIfNonExistent` (used by `joern-export`) and
`JoernSlice.checkAndApplyOverlays`, which prints *"Default overlays are not detected, applying
defaults now"* / *"Data-flow overlay is not detected, applying now"* only when the markers are
missing. **`joern-export` and `joern-slice` run against a cached `cpg.bin` without recomputing
overlays, and without rewriting the file** (`hasChangedSinceOpen` stays false).

### 2.2 The reopen cost is a full deserialization, not an mmap

```scala
/** Instantiate a new graph with storage. If the file already exists, this will deserialize the given file into memory.
```

There is no spill-to-disk in 4.0 (removed with OverflowDB). So a cached `cpg.bin` still costs
**JVM start + full graph deserialization into heap** per query process. That is the floor, and
it is not zero: a user who never asks for dependence facts pays nothing only if codectx never
*runs* Joern, not if it keeps a CPG warm on disk. No published measurement of JVM start or
flatgraph reopen exists for Joern; **measure it** rather than estimating.

### 2.3 The two-phase lever exists — and is broken at `v4.0.100`

`joern-parse` has exactly the flags you would want:

```scala
opt[Unit]("nooverlays").text("do not apply default overlays").action((_, c) => c.copy(enhance = false))
opt[Unit]("overlaysonly").text("Only apply default overlays").action((_, c) => c.copy(enhanceOnly = true))
opt[Int]("max-num-def").text("Maximum number of definitions in per-method data flow calculation")
```

But `--overlaysonly` **NPEs at this tag**. `generator` is a module-level
`var generator: CpgGenerator = scala.compiletime.uninitialized`; with `enhanceOnly` the frontend
step short-circuits (`if (config.enhanceOnly) { Success("No generation required") }`) so
`generator` is never assigned, and the next step in the `for` comprehension calls
`generator.applyPostProcessingPasses(cpg)`. This is confirmed by the fix on `master`, which
documents the bug in its own comment
([master JoernParse.scala](https://github.com/joernio/joern/blob/master/joern-cli/src/main/scala/io/joern/joerncli/JoernParse.scala)):

```scala
// With --overlaysonly no frontend ran, so `generator` is unset. Resolve a generator from the
// CPG's language instead, so language-specific post-processing passes are still applied.
val postProcessingGenerator = Option(generator).orElse { ... }
```

A second, subtler cost: `--nooverlays` skips `applyDefaultOverlays` entirely, which means it
also skips `generator.applyPostProcessingPasses(cpg)` — the frontends' **type-recovery** passes
(`XTypeRecovery` for jssrc2cpg/pysrc2cpg/rubysrc2cpg). `JoernSlice.checkAndApplyOverlays` only
restores `Base`/`ControlFlow`/`TypeRelations`/`CallGraph` and `OssDataFlow`; it never runs
frontend post-processing. So a lazily-enhanced CPG of a dynamically typed language has
**permanently worse types, and therefore worse `CALL` edges**, than an eagerly-enhanced one.
And the lazily-applied overlay is untunable: `checkAndApplyOverlays` constructs
`new OssDataFlow(new OssDataFlowOptions())` — the 4000 default, with no `--max-num-def`
equivalent on `joern-slice`.

**Verdict on laziness.** At `4.0.*` the achievable design is: run the full `joern-parse` once
per unit, cache `cpg.bin` keyed on `(analysis unit, content hash, tool version+checksum, argv)`
— which `AnalysisConfigHash` and `Detection.ObservedVersion` already give you — and make only
*extraction* lazy. That is real: a user who never asks for dependence facts skips the export,
the import and the SQLite staging entirely, which is where the gigabytes are. Making *parsing*
lazy requires `--overlaysonly` and therefore a Joern newer than `4.0.100`.

### 2.4 `joern --script` and the servers

- **CPGQL server.** `joern --server` would let one warm JVM answer many queries, but the docs
  are explicit: *"The server exclusively implements remote access to an interpreter, it does not
  implement sandboxing"* ([docs.joern.io/server](https://docs.joern.io/server/)). Against a
  profile declaring `network = "denied"` and a package doc saying *"There is no product-owned
  Scala or shell helper and no interpreter server"*, adopting it is a ruling reversal plus a
  listening socket. Not recommended.
- **`FrontendHTTPServer`.** A per-frontend HTTP server mixed into `X2CpgMain`
  ([source](https://github.com/joernio/joern/blob/v4.0.100/joern-cli/frontends/x2cpg/src/main/scala/io/joern/x2cpg/utils/server/FrontendHTTPServer.scala)).
  I grepped every frontend `Main.scala` at this tag: **only `c2cpg` and `jssrc2cpg`** declare it
  (javasrc2cpg, gosrc2cpg, pysrc2cpg, rubysrc2cpg, csharpsrc2cpg, php2cpg, swiftsrc2cpg,
  kotlin2cpg, jimple2cpg, ghidra2cpg: zero hits). It amortizes JVM+frontend warmup but nothing
  downstream, covers two of six languages, and binds a port. Not worth the policy cost.
- **`joern --script`** buys real I/O reduction (§Q3e) at the price of owning a `.sc` file.

---

## Q3. Bounded extraction vs full CSV export

| Option | Emits | CDG? | REACHING_DEF? | Volume vs CPG | Pinned argv? |
|---|---|---|---|---|---|
| **(a) `--repr=all --format=neo4jcsv`** | every node, edge and property | ✅ | ✅ | 1.0 (whole graph, text-expanded) | ✅ |
| **(b) `joern-slice data-flow`** | `SliceNode` + `SliceEdge` JSON | ❌ | ✅ (only) | ≈ whole DDG by default | ✅ |
| **(b′) `joern-slice usages`** | per-object usage records | ❌ | ❌ | moderate | ✅ |
| **(c) `joern-export --repr=cdg\|ddg --format=dot`** | one `.dot` per method | ⚠️ filtered projection | ⚠️ filtered projection | ≈ the two edge sets, minus display-filtered nodes | ✅ |
| **(d) `joern-flow`** | pretty-printed flow strings | ❌ | ❌ | small, unparseable | ✅ but useless |
| **(e) bounded `.sc`** | exactly the triples you ask for | ✅ | ✅ | minimal | ❌ reverses R11-1 |

**(a)** is `exporter.runExport(cpg.graph, outDir)` — no filter of any kind exists. It is the
only option that also yields `CALL` and `CONTAINS`, which the provider needs for attribution.

**(b)** is narrower than the docs suggest. The docs call the data-flow slicer *"interprocedural
with traversal depth limited by a default of 20"*
([docs.joern.io/cpg-slicing](https://docs.joern.io/cpg-slicing/)), but the code emits exactly
one edge label
([DataFlowSlicing.scala](https://github.com/joernio/joern/blob/v4.0.100/dataflowengineoss/src/main/scala/io/joern/dataflowengineoss/slicing/DataFlowSlicing.scala)):

```scala
val sliceNodes      = sinks.iterator.repeat(_.ddgIn)(_.maxDepth(config.sliceDepth).emit).dedup.l
lazy val sliceEdges = sliceNodes.inE(EdgeTypes.REACHING_DEF)
  .filter(x => sliceNodesIdSet.contains(x.src.id()))
  .map { e => SliceEdge(e.src.id(), e.dst.id(), e.label) }.toSet
```

Three hard facts follow. **(i) No CDG, ever** — this kills `joern-slice` as a route to
`control_depends_on`. **(ii) `--file-filter` is not a pattern:** `cpg.file.nameExact(fileName)`,
one exact filename, even though every sibling filter (`--method-name-filter`,
`--method-parameter-filter`, `--sink-filter`) is a regex. You cannot scope one invocation to a
package. **(iii) Unfiltered, it is not bounded:** with no `--file-filter` the sink set is
`cpg.call` — *every* call — and per-sink slices are `reduceOption`-unioned into a single
in-heap `DataFlowSlice`, then written by `programSlice.toJsonPretty` =
`write(this, indent = 2, sortKeys = true)`. A pretty-printed union of the whole DDG is *larger*
than the corresponding CSV edge file, not smaller.

The upside is real though: `SliceNode` carries `parentMethod`, `parentFile`, `lineNumber`,
`columnNumber` and the CPG `id`, so a slice is self-binding — it needs no join against a node
export to attribute a fact to bytes.

**(c) is the sleeper option and it is better than note 04 implied.** `--repr=cdg` and
`--repr=ddg` route through `exportDot` → `DumpCdg` / `DumpDdg`, which write one file per method:
`cpg.method.zipWithIndex.foreach { case (method, i) => (File(options.outDir) / s"$i-cdg.dot").write(str) }`.
Two properties decide its usability:

- The filename is a bare index — no method name, no path. But the graph header is
  `digraph "<method name>"` (`namedGraphBegin`), and, decisively, **the DOT node ids are the
  real flatgraph node ids** ([DotSerializer.scala](https://github.com/joernio/joern/blob/v4.0.100/semanticcpg/src/main/scala/io/shiftleft/semanticcpg/dotgenerator/DotSerializer.scala)):
  `s""""${node.id}" [label = <${stringRepr(node)}> ]"""` and
  `s"""  "${edge.src.id}" -> "${edge.dst.id}" """`.
  So `cdg`/`ddg` DOT joins to the `--repr=all` CSV on `:ID` exactly the way the GraphML plan
  intended — and DOT is a far smaller, far simpler parse than GraphML.
- Cost: one file per method (10⁵-order file count on a large repo) and no filtering. It trades a
  record bound for a file-count bound. Because `DumpCdg`/`DumpDdg` set
  `storeOverlayName = false` and write only files, `hasChangedSinceOpen` stays false and the
  `cpg.bin` is **not** rewritten on export.

**But these are projections, not edge dumps — correct the naive reading.** Neither is
edge-identical to what `--repr=all` emits.

`--repr=ddg` routes `method.dotDdg` → `DotDdgGenerator` → `dotgenerator/DdgGenerator` (a
*different* class from the pass of the same name), which walks `cfgNode.ddgInPathElem(withInvisible = true)`
— i.e. **semantics-filtered**, which is why `DumpDdg` carries an
`(implicit semantics: Semantics = DefaultSemantics())` that `DumpCdg` does not — then rewrites
endpoints to their enclosing call and drops member-access operators
([dotgenerator/DdgGenerator.scala](https://github.com/joernio/joern/blob/v4.0.100/dataflowengineoss/src/main/scala/io/joern/dataflowengineoss/dotgenerator/DdgGenerator.scala)):

```scala
val ddgEdges = edges.flatten
  .map { edge => edge.copy(src = surroundingCall(edge.src), dst = surroundingCall(edge.dst)) }
  .filter(e => e.src != e.dst)
  .filterNot(e => e.dst.isInstanceOf[Call] && isGenericMemberAccessName(e.dst.asInstanceOf[Call].name))
```

with `surroundingCall(arg: Expression) = arg.inCall.headOption.getOrElse(node)`. The edge label
written is the string `"DDG"`, not `REACHING_DEF`.

`--repr=cdg` uses `CdgGenerator extends CfgGenerator`, whose `expand` is a raw `v._cdgOut`, but
whose inherited `generate` drops vertices via `cfgNodeShouldBeDisplayed` — excluding `Literal`,
`Identifier`, `Block`, `ControlStructure`, `JumpTarget`, `MethodParameterIn` (except a condition
identifier inside a control structure) — and collapses edges transitively through them
([CfgGenerator.scala](https://github.com/joernio/joern/blob/v4.0.100/semanticcpg/src/main/scala/io/shiftleft/semanticcpg/dotgenerator/CfgGenerator.scala)).
Since `CdgPass` emits CDG edges *from* `ControlStructure` and `Literal`/`Identifier` nodes, the
DOT view relabels rather than reproduces them.

Whether that is a defect or a feature depends on what codectx wants: the provider already does
this collapsing by hand, and `surroundingCall` is the same normalization done inside Joern with
correct CPG ids. Option (c) may be *closer* to the target shape than raw edges — but the two are
not interchangeable.

**(d) `joern-flow`** loads the CPG and prints `List(s).reachableByFlows(sources.iterator).p`, a
human-readable table. There is no machine format, it needs `src`/`dst` regexes, and its
`--src-param` option is miswired (`.action((x, c) => c.copy(dstParam = Some(x)))`). Unusable.

### 3.1 What `--max-num-def` actually does, and whether it is reported

It is **not silent**, and the earlier "silently drops" framing should be corrected —
but codectx is the thing making it silent.
[ReachingDefPass.scala](https://github.com/joernio/joern/blob/v4.0.100/dataflowengineoss/src/main/scala/io/joern/dataflowengineoss/passes/reachingdef/ReachingDefPass.scala):

```scala
override def runOnPart(dstGraph: DiffGraphBuilder, method: Method): Unit = {
  val problem = ReachingDefProblem.create(method)
  if (shouldBailOut(method, problem)) { logger.warn("Skipping."); return }
  val solution = new DataFlowSolver().calculateMopSolutionForwards(problem)
  ...
private def shouldBailOut(...): Boolean = {
  val numberOfDefinitions = transferFunction.gen.foldLeft(0)(_ + _._2.size)
  logger.info("Number of definitions for {}: {}", method.fullName, numberOfDefinitions)
  if (numberOfDefinitions > maxNumberOfDefinitions) {
    logger.warn("{} has more than {} definitions", method.fullName, maxNumberOfDefinitions)
    true
  } else false
}
```

Precisely: the method is skipped **after** its `gen` sets are built but **before** the MOP
solve, so the cost saved is the fixpoint, not the setup. The method receives **zero**
`REACHING_DEF` edges — not truncated, absent. `CdgPass` has no equivalent bail-out, so
`control_depends_on` is unaffected. It is reported as **two consecutive SLF4J WARN lines** on
the `joern-parse` process, and note that `"Skipping."` carries no method name, so any scraper
must correlate it with the preceding `"{} has more than {} definitions"` line.

**This is the one place where Joern is more honest than codectx.** The provider doc specifies
"64 MiB discarded-but-counted for parse and export" and "Child output is never logged". Joern
names every degraded method on stderr and codectx throws the bytes away — the provider then
publishes `data_flows_to` facts for a workspace in which an unknown number of methods have no
data dependence at all, with capabilities reported `fresh`.

---

## Recommendations

**R1. Shard by the frontend's own unit, not by "package", and never shard C/C++ below its
include closure.** Go: per `go.mod` — `segregateByModule` already does this internally, so a
per-module CPG matches the frontend's own model exactly. Java: per Maven/Gradle module. JS: per
`package.json`/tsconfig project. Python: per package with its venv. C/C++: do not shard; pass
real `--include` paths via `--frontend-args`. *Trade accepted:* cross-shard `calls` keep their edges
but lose precise targets — static calls land on an `isExternal` stub (T1), dynamic calls lose
override resolution and land on the same stub via `fallbackToStaticResolution` (T2);
`control_depends_on` and `data_flows_to` are unchanged for every language except C/C++, which is
now citable rather than assumed. The larger threat to `calls` is not sharding but type recovery
— see R6.

**R2. Publish cross-unit `calls` anyway, and make them joinable.** Because external targets
already emit under `(ScopeWorkspace, joern:method:<FULL_NAME>)`, sharding degrades rather than
deletes. Verify empirically that two shards mint the same `FULL_NAME` for the same callee before
relying on the join; if they do not, the fact is still sound, just unshared.

**R3. Keep one export, `--repr=all --format=neo4jcsv`, and keep `--repr=cdg|ddg --format=dot`
as the *fallback* rather than GraphML.** DOT carries real CPG node ids, so it joins to the CSV
on `:ID` with a much smaller parser than `graphml.go`, and it does not rewrite `cpg.bin`.
*Trades accepted, two:* per-method file explosion — bound it by file count and fail with
`CTX_RESOURCE_LIMIT`; and the DOT views are **display-filtered projections** (§Q3c), so
switching to them changes which facts are published, not merely their encoding. If you adopt
them, re-baseline the fixture rather than assuming edge parity with `--repr=all`.

**R4. Do not adopt `joern-slice` for this provider.** It cannot produce CDG at all, its
`--file-filter` is a single exact filename, its default scope is the entire DDG unioned in heap
and pretty-printed, and its overlay is pinned at `maxNumberOfDefinitions = 4000` with no
override. It is the right tool for an *interactive* "explain this flow" query and the wrong tool
for bulk relation extraction.

**R5. Surface the degradation instead of discarding it.** Capture `joern-parse` stderr, count
`has more than N definitions` occurrences, and report a capability such as
`data_flows_to = partial` with the count. This is a small change with a large honesty payoff and
it is the direct answer to the reviewer's question. Pair it with `--max-num-def` as a deliberate
cost knob (note 04 §4.2) — lowering it is only defensible once the skips are visible.

**R6. Treat laziness as extraction-laziness at `4.0.*`.** Cache `cpg.bin` per `(unit, content
hash, tool version)`; run export/import only when dependence facts are requested. Revisit
`--nooverlays` + `--overlaysonly` only after upgrading past the `generator` NPE, and even then
weigh the permanent loss of frontend type-recovery passes on JS/Python/Ruby.

**R7. Correct three claims** identified in §0: `data_flows_to` is not strictly intraprocedural
(C2); sharding degrades rather than deletes cross-unit calls (C1); and `IS_EXTERNAL` methods do
not universally lack a location (C1), which is a live `partial`-run hazard for C/C++.

---

### Footnotes: verifiable defects at `v4.0.100` (credibility, not argument)

- `gosrc2cpg`'s `--include-indirect-dependencies` sets the wrong field:
  `.action((_, c) => c.withFetchDependencies(true))`.
- `slicing/package.scala`'s `withMethodAnnotationFilter` writes `this.methodParamTypeFilter`.
- `JoernFlow`'s `--src-param` writes `dstParam`.
- `DataFlowSlicing.sinksEndAtExternalMethod` is dead code.
- `joern-parse --overlaysonly` throws NPE (fixed on `master`).

---

*Code citations pinned to `v4.0.100` unless a comparison to `master` is explicit. Research date:
2026-09-13.*
