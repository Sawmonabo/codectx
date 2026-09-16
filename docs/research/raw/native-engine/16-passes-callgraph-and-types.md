# Native-engine research, raw evidence — the call-graph and type-recovery passes at v4.0.627

Verified against tag **v4.0.627** (`4bb889d96ce972e2ded50d0d5765c514c1a032cf`) in the engine clone at
the pinned tag. Source reading only; nothing built, nothing downloaded, the engine never run.

Counting method: aggregates via `find … -name '*.scala' -exec cat {} + | wc -l`; itemized rows via
explicit `wc -l <path>`. (`xargs wc -l | tail -1` is wrong — it reports only the last chunk's total.)
Every number below was measured for this file, not inherited.

**Citation convention.** Each row carries the research-form citation (real path, vendor names allowed)
and an ADR-safe twin with vendor tokens elided. Where a file name itself carries a vendor token the
ADR-safe twin names the **role** instead; the roles this file mints are *the parse driver*
(`joern-cli/src/main/scala/io/joern/joerncli/JoernParse.scala`) and *the per-language generator*
(`console/src/main/scala/io/joern/console/cpgcreation/*CpgGenerator.scala`).

**Scope.** Call-graph linking, type recovery, import/alias resolution, closure and capture binding.
The base and structural passes (`MethodStubCreator`, `TypeDeclStubCreator`, `ContainsEdgePass`,
`MethodDecoratorPass`, `AstLinkerPass`, `TypeRefPass`, `TypeEvalPass`, `TypeHierarchyPass`,
`FieldAccessLinkerPass`, `ParameterIndexCompatPass`) and the CFG creator are inventoried elsewhere and
are deliberately not repeated here.

---

## B0. The load-bearing finding: which pass actually produced the engine's in-repo `calls`

On the reference repository the engine resolves **18.7% of its call sites (24.1% of edges) to an
in-repo definition and 81.3% to external stubs** (`14-store-counts-r3.md` Q2), on a corpus that is
**94.75% JavaScript by call site** (`14-store-counts-r3.md` Q5). Reading the code settles which
mechanism that number can have come from, and the answer is **not the CallGraph layer**.

Three facts, each read at the tag, compose into the conclusion:

1. **Every JavaScript call site is `DYNAMIC_DISPATCH` except a platform-builtin call.**
   `astForCallExpression` has exactly two arms (`AstForExpressionsCreator.scala:95-103`): a callee
   whose source text is in `GlobalBuiltins.builtins` goes to `createBuiltinStaticCall`, and
   **everything else** goes to `handleCallNodeArgs`, which builds its `CALL` with
   `DispatchTypes.DYNAMIC_DISPATCH` (`:35`).
   This was audited across the whole `astcreation` package, not inferred from one file: every one of
   the ~52 `DispatchTypes.STATIC_DISPATCH` construction sites in
   `jssrc2cpg/.../astcreation/` names an `Operators.*` constant, an `EcmaBuiltins.arrayFactory`, or a
   literal `"<operator>.*"` string (`<operator>.iterator`, `<operator>.void`) — *except* the single
   generic builder `staticCallNode` (`astcreation/AstNodeBuilder.scala:158-172`), whose **only** caller
   is `createBuiltinStaticCall` (`AstForExpressionsCreator.scala:17-25`). Operator sites are precisely
   the nodes the product's projection excludes with `method_full_name NOT LIKE '<operator>.%'`
   (`internal/provider/dependence/neo4jcsv/scratch.go:577`).
   `StaticCallLinker` only acts on `STATIC_DISPATCH | INLINED`
   (`.../passes/callgraph/StaticCallLinker.scala:24-27`), so for JavaScript its **entire** reachable
   input is the builtin arm, whose `methodFullName` is the builtin's source text (`Math.max`,
   `JSON.parse`). Such a name matches a METHOD only where a stub creator invented one, and an invented
   stub carries no `FILENAME`.
   **`StaticCallLinker` therefore contributes zero *in-repo* `calls` for JavaScript, and what it does
   contribute lands entirely in the 81.3% external-stub bucket.**
2. **Every such call site carries `methodFullName = "<unknownFullName>"` when the CallGraph layer
   runs.** The frontend's own `callNode` helper sets
   `case DispatchTypes.STATIC_DISPATCH => name; case _ => x2cpg.Defines.DynamicCallUnknownFullName`
   (`jssrc2cpg/.../astcreation/AstNodeBuilder.scala:14-20`), and
   `Defines.DynamicCallUnknownFullName = "<unknownFullName>"` (`x2cpg/.../Defines.scala:32`). The only
   exception is an immediately-invoked function expression, where the frontend back-patches the known
   full name (`AstForExpressionsCreator.scala:35-45`, and `jssrc2cpg/.../astcreation/AstNodeBuilder.scala:119-131` for the second builder).
   `DynamicCallLinker.linkDynamicCall` opens with
   `if (call.methodFullName.equals("<empty>") || call.methodFullName.equals(DynamicCallUnknownFullName)) return`
   (`.../passes/callgraph/DynamicCallLinker.scala:176`).
   **`DynamicCallLinker` therefore returns immediately on essentially every JavaScript call site.**
3. **The product's `calls` is exactly the exported `CALL` edge set — nothing else.** The projection
   anchors a call site to `MIN(e.dst)` over `edges` with label `CALL`
   (`scratch.go:551-553`) and emits `'calls'` from that anchor alone (`scratch.go:574-577`).
   `methodFullName` is read only to exclude operator stubs; an unbound call site yields no fact at all
   (the `kindUnresolved` escape hatch exists only for writes, `rw.go:164-169`).

**Conclusion.** On the pinned argv the CallGraph layer runs *before* the frontend's post-processing
(the parse driver applies the default overlays first and calls `applyPostProcessingPasses` afterwards,
`JoernParse.scala:155,167`; layer order `Base → ControlFlow → TypeRelations → CallGraph` at
`x2cpg/.../X2Cpg.scala:383-385`). For JavaScript the layer produces essentially no CALL edge. Every
one of the 217,881 measured call sites — **both** the 18.7% in-repo and the 81.3% stub bucket — must
therefore have been produced by the **JavaScript post-processing chain**:

```
JavaScriptInheritanceNamePass → ConstClosurePass → JavaScriptImportResolverPass
  → JavaScriptTypeRecovery (2 iterations) → JavaScriptTypeHintCallLinker
  → ObjectPropertyCallLinker → NaiveCallLinker
```
(`x2cpg/.../frontendspecific/jssrc2cpg/package.scala:10-15`.)

Within that chain the CALL edge is created by exactly two passes:

- **`XTypeHintCallLinker.linkCallToCallee`** — `builder.addEdge(call, method, EdgeTypes.CALL)`
  (`x2cpg/.../passes/frontend/XTypeHintCallLinker.scala:82`), driven by
  `call.dynamicTypeHintFullName` which type recovery wrote. When no METHOD with that full name
  exists it **invents one** (`:107-160`), marking it `isExternal = false` only when the recovered full
  name matches `^(.*(.py|.js|.rb)).*$` **and** that file is in the graph (`:110-117`); otherwise
  `isExternal = true` and the stub is parented to a `<speculatedMethods>` namespace (`:166-174`).
- **`NaiveCallLinker`** — joins every still-unlinked call to **every** METHOD sharing its bare `name`
  (`.../passes/callgraph/NaiveCallLinker.scala:15-26`), and back-writes `methodFullName` when the name
  is unique.

The `isExternal` branch at `XTypeHintCallLinker.scala:110-117` is exactly the store's observed split:
an invented stub has no `FILENAME`, so the importer records it with `file_id IS NULL` and
`resolution='import'` — the 81.3% bucket — while a stub whose recovered full name resolved onto a real
`.js` file becomes an in-repo definition — the 18.7% bucket.

**What this establishes, and what it does not.** It establishes that a port which reimplements
`StaticCallLinker` and `DynamicCallLinker` and stops there would reproduce **none** of the engine's
JavaScript call graph, and that the whole measured value of `calls` on this corpus is produced by the
type-recovery chain that doc 20 §2 files under "not taken". It does **not** establish the split
*between* `JavaScriptTypeHintCallLinker` and `NaiveCallLinker` inside the 18.7%, and that split
matters: a `NaiveCallLinker` edge is a bare-name join with no type evidence at all, and the product
publishes it at `static_analysis` precision indistinguishably from a recovered one. **The measurement
that would settle it** is a differential export of one JavaScript unit with
`NaiveCallLinker` removed from the chain, diffed on the `CALL` edge set — a one-pass change to the
post-processing list, and the cheapest experiment in this whole research. Until it is run, the share
of `calls` that rests on a bare-name join is unknown, and it bounds from above what a native
type-recovery port has to reproduce.

---

## B1. Call-graph linking

| pass | what it computes | algorithm AS WRITTEN | Scala main lines at the tag | Go estimate | what the product gains | per-language pieces | citation (research form) | citation (ADR-safe form) |
|---|---|---|---|---|---|---|---|---|
| **CallGraph layer** | orders the three linkers | fixed `Iterator(MethodRefLinker, StaticCallLinker, DynamicCallLinker)`; `dependsOn = TypeRelations` | **30** | 20–40 | nothing directly | none — shared | `joern-cli/frontends/x2cpg/src/main/scala/io/joern/x2cpg/layers/CallGraph.scala:13-22` | `x2cpg/.../layers/CallGraph.scala:13-22` |
| **`StaticCallLinker`** | CALL edge from a statically dispatched call site to every METHOD whose `fullName` equals the site's `methodFullName` | **one exact-string join, nothing more.** Batches `cpg.call` at `MAX_BATCH_SIZE` (**100**, `x2cpg/.../utils/LinkingUtil.scala:16`) and per call switches on `dispatchType`: `STATIC/INLINED` → `cpg.method.fullNameExact(call.methodFullName)`, add a CALL edge to each hit; `DYNAMIC` → no-op; anything else → warn. No index it builds itself, no fixed point, no fallback: an unmatched name yields no edge and no diagnostic. Worst case is (call sites) × (full-name index lookup). **41 lines is the whole algorithm** | **41** | 60–120 — the join is trivial; the cost is that Go has no `fullNameExact` index to ride on, so the port must build and own the `fullName → method` map and the batching itself | `codectx_callers` / `codectx_callees` / `codectx_impact` / `codectx_dependency_path` — every `calls` fact the product publishes is an exported CALL edge (`scratch.go:551-577`) | **shared entirely.** Its per-language content is not code but the frontend's `methodFullName` convention, which a port must mint itself | `.../x2cpg/passes/callgraph/StaticCallLinker.scala:17-40` | `x2cpg/.../passes/callgraph/StaticCallLinker.scala:17-40` |
| **`DynamicCallLinker`** | CALL edges from dynamically dispatched sites to the set of possible overriding implementations | **class-hierarchy analysis, SafeDispatch-style** (the header cites Jang/Tatlock/Lerner 2014). Early-exits if no `DYNAMIC_DISPATCH` call exists. Builds `typeMap: fullName→TypeDecl` over every TYPE_DECL and `methodMap: fullName→Method` over every non-`<operator>` METHOD. Then for **every (typeDecl, method-via-AST) pair** computes `validM(method.fullName) = allSubclasses(typeDecl).flatMap(staticLookup(_, method))`, where `allSubclasses` is a memoised recursive walk of `INHERITS_FROM` (cycle-guarded by a visited set, `:114-131`) and `staticLookup` matches on `name` **and** exact `signature` within the subclass's AST children. Per call site: `resolveCallInSuperClasses` splits `methodFullName` at the last `:` into (full name, signature), strips `.name`, walks all superclasses and unions any matching inherited methods into `validM`; then the `validM` hit set is partitioned by `isExternal` and **internals win when both are present** (`:185-186`); an already-present CALL edge to that target diverts to `fallbackToStaticResolution`, which is a plain `methodMap` full-name lookup, and a miss is only logged. **Not a fixed point** — one pass, terminating on the finite type set. Worst case ≈ Σ over types of (subclasses × methods-per-type) for the build, plus (dynamic call sites × superclass-chain depth) for the query | **226** | 400–700 — the hierarchy walk and caches port directly, but Go must first *have* `INHERITS_FROM`, a TYPE_DECL→METHOD AST index, and an exact `signature` string, none of which a CST supplies | on the reference corpus, **nothing measurable**: JavaScript call sites carry `<unknownFullName>` so `:176` returns before any work (§B0). It earns its keep only where a frontend writes a real `methodFullName` with a signature — i.e. Java and C/C++ | **shared**, but useless without a per-language `methodFullName`/`signature` convention and an inheritance edge set | `.../passes/callgraph/DynamicCallLinker.scala:30-221` | `x2cpg/.../passes/callgraph/DynamicCallLinker.scala:30-221` |
| **`MethodRefLinker`** | REF edge from every METHOD_REF to the METHOD its `methodFullName` names | one call to the shared `linkToSingle` helper: iterate `cpg.methodRef`, look up `methodFullNameToNode`, add one REF edge, warn on a miss | **30** (helper `LinkingUtil.scala` **150**) | 40–80 (+ the helper, shared with the base passes) | **nothing today.** REF edges *are* staged (`scratch.go:69-72`) and METHOD_REF *is* in the anchor query's label list, but that query requires the REF target be a **declaration** entity (`d.kind = 'decl'`, `scratch.go:555-556`), and this pass's target is always a METHOD. So the edge it creates never anchors. What the product would gain if the anchor admitted methods: call-through-a-function-value edges — the 82.8% unresolved bucket's dominant shape | shared | `.../passes/callgraph/MethodRefLinker.scala:14-27`; `.../x2cpg/utils/LinkingUtil.scala` | `x2cpg/.../passes/callgraph/MethodRefLinker.scala:14-27`; `x2cpg/.../utils/LinkingUtil.scala` |
| **`NaiveCallLinker`** | CALL edge from every still-unlinked call to **every** METHOD with the same bare `name` | `cpg.method.toList.groupBy(_.name)`, then for each call with no outgoing CALL edge add an edge to every method of that name; if exactly one, overwrite the call's `methodFullName` with it. No type, no signature, no scope, no arity check. Worst case (unlinked sites) × (methods sharing a name) | **29** | 40–80 | **materially, and doc 20 is wrong about it** — see below | shared | `.../passes/callgraph/NaiveCallLinker.scala:12-27` | `x2cpg/.../passes/callgraph/NaiveCallLinker.scala:12-27` |
| **`XTypeHintCallLinker`** (+ 6 subclasses) | CALL edges from recovered `dynamicTypeHintFullName`s, **inventing METHOD stubs where the target does not exist** | select calls with a non-empty `dynamicTypeHintFullName` and no callee; build `methodMap` from `cpg.method.fullNameExact(names*)`, and for each name with no hit synthesise a `NewMethod` through `MethodStubCreator.createMethodStub` (`:148-159`) with `isExternal` decided by the `.py/.js/.rb` filename regex against `cpg.file` (`:110-117`); add the CALL edge and copy the callee's return type onto the call (`:81-91`); collapse `methodFullName` to the single hit, preferring non-dummy types (`:93-105`); park orphan stubs under `<speculatedMethods>` (`:166-174`) | **184** shared; subclasses **16** (JS) / **30** (Python) / **17** (Java) / **18** (PHP) / **36** (Ruby) / **6** (Swift) | 350–600 shared + 30–80 per language — the stub-invention and the external/internal decision are the whole content, and Go has no `fullNameExact` index or `NewMethod` builder | **this is where the reference corpus's `calls` comes from** (§B0): both the 18.7% in-repo bucket and the 81.3% stub bucket. It feeds `codectx_callers`, `codectx_callees`, `codectx_impact`, `codectx_dependency_path` | shared driver; each of the six subclasses only overrides *which calls to consider* and the path separator | `.../passes/frontend/XTypeHintCallLinker.scala:21-184`; `.../frontendspecific/jssrc2cpg/JavaScriptTypeHintCallLinker.scala` | `x2cpg/.../passes/frontend/XTypeHintCallLinker.scala:21-184`; `x2cpg/.../frontendspecific/jssrc2cpg/JavaScriptTypeHintCallLinker.scala` |
| **`ObjectPropertyCallLinker`** (JS only) | recovers `methodFullName` for the `obj.fn = function(){}` / `obj.fn()` pattern within one file | regex-matches `methodFullName` against `^(?:\{.*\}|.*<returnValue>):<member>\((.*)\):.*$`, groups sites by `baseProperty.name`, then joins against assignments whose source is a METHOD_REF and target a field access, restricted to the same file, and **sets `methodFullName`** | **45** | 60–120 | **nothing.** It writes a property and never calls `addEdge`; the product's `calls` reads only CALL edges. Its output could only reach the product through a later linker, and the only one that follows is `NaiveCallLinker`, which joins on `name`, not `methodFullName` | JS/TS only | `.../frontendspecific/jssrc2cpg/ObjectPropertyCallLinker.scala:15-42` | `x2cpg/.../frontendspecific/jssrc2cpg/ObjectPropertyCallLinker.scala:15-42` |

### B1.1 `NaiveCallLinker` is wired, and the published claim that it is dead weight is wrong

`docs/research/20-native-engine-post-mvp.md` §2 lists `NaiveCallLinker` among "six passes whose output
the importer never stages … the last not even wired into its layer", and
`12-source-anatomy-v4.0.627.md` §A3 marks it **"not in the layer — dead weight"**. Both are wrong about
the consequence. It is correctly *not* in the CallGraph layer, but it **is** in the post-processing
chain of the two frontends whose facts dominate the corpus:

```scala
// x2cpg/.../frontendspecific/jssrc2cpg/package.scala:14
List(new JavaScriptTypeHintCallLinker(cpg), ObjectPropertyCallLinker(cpg), new NaiveCallLinker(cpg))
// x2cpg/.../frontendspecific/pysrc2cpg/package.scala:20-21
new PythonTypeHintCallLinker(cpg),
new NaiveCallLinker(cpg),
```

and also in `rubysrc2cpg`, `csharpsrc2cpg` and `swiftsrc2cpg`. **On the product's pinned argv
`NaiveCallLinker` runs for JavaScript and for Python**, its output *is* CALL edges, and CALL edges
*are* staged.

The plausible alternative route was checked and **disproved**: `jssrc2cpg/.../JsSrc2Cpg.scala:8`
imports `{MethodRefLinker, NaiveCallLinker}`, but `createCpg` (`:23-43`) never constructs either — the
import is dead, and the file is 45 lines, so there is nowhere else it could be used. The frontend
binary that the parse step invokes does **not** link calls; `NaiveCallLinker` reaches JavaScript only
through the post-processing chain the parse driver applies afterwards. That matters for §B0: there is
no pre-layer CALL edge production hiding in the frontend binary. Its 29 lines are not dead weight; they are a live, untyped, unsigned, unscoped name join
whose edges the product publishes at `static_analysis` precision. That is a fact about what the
product currently promises, and it belongs in any honest account of what a port must either reproduce
or deliberately drop.

---

## B2. Type recovery

| pass | what it computes | algorithm AS WRITTEN | Scala main lines at the tag | Go estimate | what the product gains | per-language pieces | citation (research form) | citation (ADR-safe form) |
|---|---|---|---|---|---|---|---|---|
| **`XTypeRecovery`** + its pass generator | propagates types across compilation units into `TYPE_FULL_NAME`, `DYNAMIC_TYPE_HINT_FULL_NAME` and `POSSIBLE_TYPES` | **flow-insensitive, SSA-flavoured symbol-table propagation, and explicitly NOT a fixed point**: *"does not try to converge to some fixed point but rather iterates a fixed number of times"* (`:199`). `XTypeRecoveryPassGenerator.generate()` (`:156-169`) emits **`config.iterations` separate passes**, default **2** (`XTypeRecoveryConfig(iterations = 2, enabledDummyTypes = true)`, `:25`), sharing one `XTypeRecoveryState`; `iterations < 2` gives no interprocedural recovery at all (`:200-201`), and **dummy types (`<returnValue>`, `<member>(x)`, `<indexAccess>`) are only minted on the final iteration** (`:104`). Each pass is a `ForkJoinParallelCpgPass` over compilation units (a FILE for JS/Python, a non-external METHOD for Java). Per unit: read import tags into a `SymbolTable`, visit assignments to extrapolate types, propagate to uses, and set the type of a call whose receiver is now typed; **local symbols are cleared when the unit ends**, which is what keeps it parallel. An optional post-step links now-typed `fieldAccess` calls to their MEMBER by REF (`:110-138`, wired at `:163-166`) | **1,331** | **1,800–3,000** — the symbol table and the two-pass loop port cleanly; the cost is that every `getKnownTypes` read (`:275-291`) rides on `TYPE_FULL_NAME`/`DYNAMIC_TYPE_HINT_FULL_NAME`/`POSSIBLE_TYPES` properties the CPG maintains, and on the lowered operator algebra (`Assignment`, `FieldAccess`) that a CST does not have | indirectly, **all of `calls` on this corpus** (§B0): it writes the hints `XTypeHintCallLinker` turns into CALL edges. Feeds `codectx_callers`, `codectx_callees`, `codectx_impact` | shared engine; each language supplies `compilationUnits`, `isConstructor`, the local-key mapping and the store-hooks | `.../passes/frontend/XTypeRecovery.scala:25,104,151-169,196-244` | `x2cpg/.../passes/frontend/XTypeRecovery.scala:25,104,151-169,196-244` |
| **`SymbolTable`** | the per-unit and global map from a local key to a set of candidate types | a thin `mutable.Map[K, Set[String]]` wrapper with put/append/remove and a `LocalKey` algebra (`LocalVar`, `CallAlias`) | **155** | 150–250 | via `XTypeRecovery` only | shared | `.../passes/frontend/SymbolTable.scala` | `x2cpg/.../passes/frontend/SymbolTable.scala` |
| **`XInheritanceFullNamePass`** | rewrites a TYPE_DECL's `inheritsFromTypeFullName` from a short/aliased name to the resolved full name | per compilation unit, resolve each inherited name against the imports and the file's own TYPE_DECLs, then overwrite the property so `INHERITS_FROM` can link | **142** | 200–350 | **nothing directly** — `INHERITS_FROM` is a recognised-but-unread edge (`scratch.go:78`). It matters only because `DynamicCallLinker`'s hierarchy walk reads it | shared base; **15** (JS) / **14** (Python) / **15** (Swift) subclasses | `.../passes/frontend/XInheritanceFullNamePass.scala`; `.../frontendspecific/jssrc2cpg/JavaScriptInheritanceNamePass.scala` | `x2cpg/.../passes/frontend/XInheritanceFullNamePass.scala`; `x2cpg/.../frontendspecific/jssrc2cpg/JavaScriptInheritanceNamePass.scala` |
| **`XTypeStubsParser`** | parses external type-stub files into the symbol table's seed set | small parser over a stub format; only `php2cpg` supplies stubs at the tag | **42** (+ **75** PHP) | 60–120 | nothing (PHP is not a product frontend) | PHP only | `.../passes/frontend/XTypeStubsParser.scala` | `x2cpg/.../passes/frontend/XTypeStubsParser.scala` |
| **`JavaScriptTypeRecovery`** | the JS/TS override | overrides the visitor for `require`/`module.exports`/prototype assignment and the `:`-separated path convention | **225** | 400–700 | as above | JS/TS | `.../frontendspecific/jssrc2cpg/JavaScriptTypeRecovery.scala` | `x2cpg/.../frontendspecific/jssrc2cpg/JavaScriptTypeRecovery.scala` |
| **`GlobalBuiltins`** (JS) | the built-in-name set that stops the recovery inventing a stub for `Array`, `JSON`, DOM, Node | **a hand-written literal list** | **1,094** | **1,000–1,200 — essentially a transcription, not an algorithm.** The single largest and cheapest-to-port item in this whole inventory | keeps `codectx_callees` from filling with invented stubs for platform names | JS/TS; every other language needs its own list and none exists at the tag | `.../frontendspecific/jssrc2cpg/GlobalBuiltins.scala` | `x2cpg/.../frontendspecific/jssrc2cpg/GlobalBuiltins.scala` |
| **`PythonTypeRecovery`** | the Python override | overrides for `__init__`, module-level assignment and the `.py:<module>` full-name convention | **237** | 400–700 | as above | Python | `.../frontendspecific/pysrc2cpg/PythonTypeRecovery.scala` (+ generator **17**) | `x2cpg/.../frontendspecific/pysrc2cpg/PythonTypeRecovery.scala` |
| **`DynamicTypeHintFullNamePass`** (Python) | seeds `dynamicTypeHintFullName` on parameters and returns from Python type annotations | reads the annotation the frontend attached and writes it as a hint before recovery runs | **99** | 150–250 | as above | Python | `.../frontendspecific/pysrc2cpg/DynamicTypeHintFullNamePass.scala` | `x2cpg/.../frontendspecific/pysrc2cpg/DynamicTypeHintFullNamePass.scala` |
| **`JavaTypeRecoveryPassGenerator`** | the Java override | `compilationUnits = cpg.method.isExternal(false)` — **a method, not a file**; treats a leading capital as a constructor; drops `<unresolvedNamespace>` entries after imports; suppresses `this`; appends the signature to every stored call type | **66** | 150–300 | **nothing on the pinned argv** — see §B5 | Java | `.../frontendspecific/javasrc2cpg/JavaTypeRecoveryPassGenerator.scala:27-80` | `x2cpg/.../frontendspecific/javasrc2cpg/JavaTypeRecoveryPassGenerator.scala:27-80` |

---

## B3. Import and alias resolution

| pass | what it computes | algorithm AS WRITTEN | Scala main lines at the tag | Go estimate | what the product gains | per-language pieces | citation (research form) | citation (ADR-safe form) |
|---|---|---|---|---|---|---|---|---|
| **`XImportResolverPass`** | resolves an IMPORT's `importedEntity`/`importedAs` to a real target | `ForkJoinParallelCpgPass` over `cpg.imports`; per import call, hand (file, entity, alias) to the language hook. **The result is written as TAG nodes**, not edges: `evaluatedImportToTag` → `newTagNodePair(x.label, x.serialize)` (`:43-44`) | **46** | 80–150 | **nothing directly** — `TAG` and `TAG_NODE_PAIR` are recognised-but-not-staged (`scratch.go:63`). Its entire value to the product is indirect: `XTypeRecovery` reads those tags to seed the symbol table | shared driver | `.../passes/frontend/XImportResolverPass.scala:15-45` | `x2cpg/.../passes/frontend/XImportResolverPass.scala:15-45` |
| **`XImportsPass`** | turns the frontend's import *calls* into IMPORT nodes | small mapping pass ahead of the resolver | **41** | 60–120 | nothing directly | shared + **43** (Python `ImportsPass`) / **28** (Ruby) | `.../passes/frontend/XImportsPass.scala` | `x2cpg/.../passes/frontend/XImportsPass.scala` |
| **`JavaScriptImportResolverPass`** | resolves `require`/ESM specifiers to in-repo files or to external packages | resolves the specifier against the code root, walking relative paths and index files, and tags the import call with the resolved entity | **131** | 250–450 — Node resolution is a real algorithm (extensions, `index.js`, `package.json` main, `node_modules` walk) and Go must own all of it | indirect, via type recovery | JS/TS | `.../frontendspecific/jssrc2cpg/JavaScriptImportResolverPass.scala` | `x2cpg/.../frontendspecific/jssrc2cpg/JavaScriptImportResolverPass.scala` |
| **`PythonImportResolverPass`** | the same for Python `import`/`from … import` | resolves the dotted entity against the file tree and the symbol table | **153** | 250–450 | indirect | Python | `.../frontendspecific/pysrc2cpg/PythonImportResolverPass.scala` | `x2cpg/.../frontendspecific/pysrc2cpg/PythonImportResolverPass.scala` |
| **`AliasLinkerPass`** | ALIAS_OF edges from TYPE_DECL to TYPE for `aliasTypeFullName` | one `linkToMultiple` call over `cpg.typeDecl`, reading the `aliasTypeFullName` property | **28** | 40–70 | **nothing, now or ever under the current projection.** `ALIAS_OF` is explicitly recognised-and-not-read (`scratch.go:76`); it is a type↔type edge and the product publishes no type-level relation | shared | `.../passes/typerelations/AliasLinkerPass.scala:12-26` | `x2cpg/.../passes/typerelations/AliasLinkerPass.scala:12-26` |

There is **no other import resolver** at the tag: `find frontendspecific -name '*ImportResolver*'`
returns exactly JS, Python, PHP and Ruby. **C/C++, Go, Rust and Java have none.**

---

## B4. Closure and capture binding

| pass / producer | what it computes | algorithm AS WRITTEN | Scala main lines at the tag | Go estimate | what the product gains | per-language pieces | citation (research form) | citation (ADR-safe form) |
|---|---|---|---|---|---|---|---|---|
| **`VariableScopeManager`** (the producer) | CLOSURE_BINDING nodes and CAPTURE edges when a nested scope references an outer variable | a scope stack; on a reference that resolves to an enclosing method's scope it mints `closureBindingNode(id, evaluationStrategy)` and adds `capturingRefNode → closureBinding` as a `CAPTURE` edge, plus a LOCAL in the capturing method carrying the same `CLOSURE_BINDING_ID` | **633** (shared scope machinery, of which the binding logic is `:380-395`) | 500–900 — a scope resolver is exactly what a CST-based port lacks and §7.5's "scope/binding resolver" swing factor already prices | see below | **shared helper, used by jssrc2cpg (4 files), pysrc2cpg (3), javasrc2cpg (2), c2cpg (5); gosrc2cpg and rust2cpg use it in ZERO files** | `.../x2cpg/datastructures/VariableScopeManager.scala:380-395`; `.../x2cpg/AstNodeBuilder.scala:479`; `.../x2cpg/Ast.scala:38,279,284` | `x2cpg/.../datastructures/VariableScopeManager.scala:380-395`; `x2cpg/.../AstNodeBuilder.scala:479` |
| **`DdgGenerator.addEdgesToCapturedIdentifiersAndParameters`** (the consumer) | the REACHING_DEF edges that cross a closure boundary | `method.parameter.foreach { param => param.capturedByMethodRef.referencedMethod.ast.isIdentifier.foreach { addEdge(param, identifier, …) } }` — i.e. it walks **CAPTURED_BY** from a parameter into the capturing method's AST. A second block does the same for "globals" reached via `globalFromLiteral` over the method's calls and returns. The in-tree comment is a confession: *"PARENT → CHILD brings no inconsistent flows, but from CHILD → PARENT we have observed inconsistencies. This form of modelling data-flow is unsound as the engine assumes REACHING_DEF edges are intraprocedural."* | part of the **962**-line `reachingdef/` package | counted with the reaching-defs core | the cross-method `data_flows_to` edges the product's depth-8 walk traverses | shared | `dataflowengineoss/.../passes/reachingdef/DdgGenerator.scala:170-200` | `dataflowengineoss/.../passes/reachingdef/DdgGenerator.scala:170-200` |
| **`ConstClosurePass`** (JS, Swift) | renames an anonymous METHOD assigned to a `const`/`let`/`var`/export/object property to `<enclosingMethod>:<name>` | four patterns over `cpg.assignment` (const, object-expression temp, export, mutable var — the last only when the name is assigned exactly once, `:67-68`); each rewrites the METHOD_REF's `methodFullName` and the METHOD's `name` **and `fullName`** | **83** (JS), **83** (Swift) | 150–250 | **materially and indirectly**: by giving an anonymous JS function a stable, file-scoped `fullName`, it is what lets `XTypeHintCallLinker`'s `isExternal` regex resolve a recovered name onto a real `.js` file (§B0). Without it more of the 18.7% would fall into the stub bucket | JS/TS and Swift only | `.../frontendspecific/jssrc2cpg/ConstClosurePass.scala:17-82` | `x2cpg/.../frontendspecific/jssrc2cpg/ConstClosurePass.scala:17-82` |

### B4.1 What the product actually stages, and what doc 20 §4's "capture" claim rests on

Read out of `scratch.go` in full, not guessed:

- **`CLOSURE_BINDING` is a staged node label** (`scratch.go:52`), and the importer reads its
  `CLOSURE_BINDING_ID` property into the `closure_binding` column (`csv.go:317`,
  `scratch.go:170,401`).
- **`CAPTURE` and `CAPTURED_BY` are recognised edge labels that the projection does NOT read**
  (`scratch.go:76`). The product never sees the capture *edges*.
- The `reaching_def capture` detail is therefore decided by two disjuncts and neither is a CAPTURE
  edge (`scratch.go:630-636`):
  1. the walk's start node and its end node have **different owning methods** (`attr.owner`), or
  2. **either endpoint entity carries a non-empty `CLOSURE_BINDING_ID`**.

**Verdict on doc 20 §4's claim** that "globals are folded into `reaching_def capture` because the
export gives globals no marker": **correct, and it rests on disjunct (1), not on any closure
machinery.** A module-level identifier reached from inside a function has a different `attr.owner`
than the walk's start, so it takes the capture detail by the same rule a genuine closure capture does.
The two are indistinguishable in the published fact. Disjunct (2) is the only genuinely
closure-derived signal, and it is a node *property*, so it survives the fact that the CAPTURE edges are
dropped.

**Consequence for a port.** A native engine that resolves scopes properly can distinguish a global
from a capture, and would therefore emit a *different* detail string for the same source construct.
That is a differential-oracle finding, not a bug: the band for `data_flows_to` details must be allowed
to move, or the port will read as a regression on a corpus where it is strictly more correct.

**Consequence for Go and Rust.** `gosrc2cpg` and `rust2cpg` use the scope manager in **zero** files, so
they emit no CLOSURE_BINDING and no CAPTURE at all. Go closures over loop variables and Rust `move`
closures are invisible to the engine's capture modelling today; only disjunct (1) can fire for them.

---

## B5. Which of the product's six frontends actually get type recovery — re-verified, and corrected

Re-verified by reading every generator's `applyPostProcessingPasses` override at the tag, as the brief
required, rather than inheriting `12-source-anatomy-v4.0.627.md` §A3's table.

| product `--language` | generator | overrides `applyPostProcessingPasses`? | runs type recovery on the **pinned argv**? | evidence |
|---|---|---|---|---|
| `jssrc` | `JsSrcCpgGenerator` | yes | **yes** — the full JS chain, unconditionally | `console/.../cpgcreation/JsSrcCpgGenerator.scala:36-39` |
| `pythonsrc` | `PythonSrcCpgGenerator` | yes | **yes** — the full Python chain, unconditionally | `console/.../cpgcreation/PythonSrcCpgGenerator.scala:26-29` |
| `javasrc` | `JavaSrcCpgGenerator` | yes | **NO** | see below |
| `c` | `CCpgGenerator` | **no** | no — falls through to the base no-op | `console/.../cpgcreation/CCpgGenerator.scala` has no override; base at `CpgGenerator.scala:56-58` |
| `golang` | `GoCpgGenerator` | **no** | no | `console/.../cpgcreation/GoCpgGenerator.scala` |
| `rust` | `RustCpgGenerator` | **no** | no | `console/.../cpgcreation/RustCpgGenerator.scala` |

**The Java correction, and why it matters.** `JavaSrcCpgGenerator` *does* override, but the override is
conditional:

```scala
// console/src/main/scala/io/joern/console/cpgcreation/JavaSrcCpgGenerator.scala:16,26-30
private lazy val enableTypeRecovery = cmdLineArgs.exists(_ == s"--${javasrc2cpg.ParameterNames.EnableTypeRecovery}")
override def applyPostProcessingPasses(cpg: Cpg): Cpg = {
  if (enableTypeRecovery)
    javasrc2cpg.typeRecoveryPasses(cpg, typeRecoveryConfig).foreach(_.createAndApply())
  super.applyPostProcessingPasses(cpg)
}
```

`cmdLineArgs` is `config.cmdLineParams`, and the generator's config is built from **only** the
arguments after the frontend-args delimiter (`JoernParse.scala:71` splits, `:140` passes
`frontendArgs.toList` into `cpgGeneratorForLanguage`, which does `config.withArgs(args)` at
`console/.../cpgcreation/package.scala:22`). The product's pinned parse argv is
`--language <frontend> --max-num-def 40000 <source> --output <graph>`
(`internal/provider/dependence/joern/joern.go:11,178,204`) with `req.ExtraArgs` supplied by
`NeutralOptions`, which **returns nil for every frontend** (`internal/provider/dependence/joern/joern.go:186`). There is no delimiter and
no frontend argument, so `cmdLineArgs` is empty, `enableTypeRecovery` is false, and
`super.applyPostProcessingPasses` is the base no-op at `console/.../cpgcreation/CpgGenerator.scala:56-58`.

**So the honest count is four of six, not three of six: C/C++, Go, Rust *and Java* get no type
recovery on the argv the product actually runs.** `12-source-anatomy-v4.0.627.md` §A3's row
"`JavaSrcCpgGenerator.scala:26` | yes | Java" and `20-native-engine-post-mvp.md` §2's "Only JS/TS,
Python and Java override it" are wrong in effect, and the error understates how little the product is
getting for the type-recovery machinery it pays for. Independently, the reference repository's single
Java dependence unit **failed with zero records** (`14-store-counts-r3.md` Q2), so there is no
contradicting store evidence either way.

---

## B6. The languages whose `calls` is a name join, and what that means for a port

**Verified at the tag, not inherited.** For C/C++, Go, Rust and Java the entire `calls` production is
the CallGraph layer, because no post-processing runs (§B5). Within that layer:

- `StaticCallLinker` — `cpg.method.fullNameExact(call.methodFullName)`, one exact string join.
- `DynamicCallLinker` — only for `DYNAMIC_DISPATCH` sites with a real `methodFullName`; its
  `validM` construction needs TYPE_DECL→METHOD AST edges and `INHERITS_FROM`, and its
  `fallbackToStaticResolution` is itself a `methodMap` full-name lookup. For a language with no
  classes (C, Go) `validM` is empty and every site falls through to the same name join.
- `MethodRefLinker` — REF only, and the product's anchor never admits it (§B1).

**So for C/C++, Go and Rust the native call linker is a name join and nothing more, and a port reaches
parity on `calls` for those three cheaply** — the port's cost there is not the linker (60–120 Go lines,
shared) but minting the `methodFullName` convention per language, which is frontend work the port must
do anyway to key its declarations. For Java the same is true *on the pinned argv*; if the product ever
passes `--enable-type-recovery`, Java joins the expensive group.

The corollary is the uncomfortable half: the three languages where a port reaches `calls` parity
cheaply carry **zero call sites in the reference corpus** (`snapshot_files` has no c/cpp/go/rust rows,
`14-store-counts-r3.md` Q5.5), and the one language that carries 94.75% of them is the one where parity
costs the whole type-recovery chain.

---

## B7. Passes in these families whose output the product never stages

Determined from `scratch.go`'s label sets read in full (`stagedNodeLabels` `:47-54`,
`knownNodeLabels` `:59-66`, `stagedEdgeLabels` `:69-72`, `knownEdgeLabels` `:75-82`), not from a guess.

| pass | its output | staged? | does it matter? |
|---|---|---|---|
| `AliasLinkerPass` | `ALIAS_OF` edge | no — recognised, not read (`:76`) | **no.** A type↔type edge; the product publishes no type-level relation, and re-aliased TYPE_DECLs change no `calls` edge. Its 28 lines are a genuine port saving |
| `XInheritanceFullNamePass` | `inheritsFromTypeFullName` property → `INHERITS_FROM` edge | no — recognised, not read (`:78`) | **yes, indirectly.** `DynamicCallLinker`'s `allSubclasses`/`allSuperClasses` read `INHERITS_FROM` (`DynamicCallLinker.scala:122-123`). Dropping the pass does not drop a fact; it silently shrinks `validM` and therefore the call graph. A port that skips it must accept the same loss knowingly |
| `XImportResolverPass` and its four language subclasses | `TAG` / `TAG_NODE_PAIR` nodes | no — recognised, not read (`:63`) | **yes, indirectly and decisively.** The tags are the symbol table's seed for `XTypeRecovery`, which is the only producer of this corpus's `calls` (§B0). The tags themselves are worthless to the product; what they enable is everything |
| `ObjectPropertyCallLinker` | a `methodFullName` property rewrite, **no edge** | the property is staged, but the projection reads `method_full_name` only to exclude `<operator>.%` | **no.** Its work cannot reach a published fact under the current projection |
| `MethodRefLinker` | `REF` edge METHOD_REF→METHOD | the edge label **is** staged (`:71`), but the anchor query rejects it | **yes — and this is the one actionable gap in the list.** METHOD_REF is in the anchor's label set (`scratch.go:556`) yet the join demands `d.kind = 'decl'`, so a method-valued reference never anchors. Admitting `kindMethod` there would publish call-through-a-function-value edges from facts already in the store. **Out of this sub-lane's scope; flagged, not acted on** |
| `XTypeHintCallLinker`'s `<speculatedMethods>` NAMESPACE_BLOCK | `NAMESPACE_BLOCK` node + `AST` edge | `NAMESPACE_BLOCK` recognised, not staged (`:60`) | **no** — but note the stub METHODs it parents *are* staged and *are* published, as the 81.3% external bucket. The marker that says "this callee may not exist" is dropped while the callee is kept |

The last row is worth stating on its own: **the engine labels its invented callees
`<speculatedMethods>` and the product discards the label.** A user of `codectx_callees` cannot tell a
speculated stub from a real external dependency. That is a product observability gap in today's
importer, independent of any port, and it is outside this sub-lane's scope to fix.

---

## B8. What a native call graph would do instead — the shape this sizing assumes

The sizing above assumes a native engine that does **not** reproduce the engine's architecture. The
engine resolves calls by minting a per-language `methodFullName` string at AST-creation time, then
joining strings — `StaticCallLinker` is 41 lines precisely because the hard work happened in the
frontend's naming convention, and `DynamicCallLinker`'s hierarchy walk is a refinement on that string
join. A native engine has no such string, because a tree-sitter CST has no lowering pass to mint one;
it has a scope resolver instead. The shape assumed is therefore: **resolve each call site's callee
expression to a declaration through a per-file scope/binding resolver (already priced as doc 20 §7.5's
swing factor), then, where the callee is a method on a type, widen to the set of implementations using
whatever hierarchy the resolver recovered.** The widening step is where class-hierarchy analysis, rapid
type analysis and points-to differ, and this file deliberately does not choose between them — that is
the algorithm lane's call. What this file fixes for that lane is the **input contract**: the widening
gets a resolved declaration and a type set from the resolver, never a `METHOD_FULL_NAME` string, and
the output contract is a caller-declaration → callee-declaration edge with a site range, which is
exactly the tuple the product's projection already publishes (`scratch.go:574-577`). The engine's
`<speculatedMethods>` behaviour — inventing a callee when none is found — is a **choice**, not a
requirement, and a native engine that instead emits `may_refer_to` would be more honest and would cost
nothing extra, because the product already has that kind and that code path (`emit.go:637-646`).

---

## B9. Totals

Shared = one implementation serves every language. Per-language = written once per frontend.
Scala figures are measured at the tag; Go figures are ranges with the reasoning in §B1–B4.

| | Scala main lines at the tag | Go estimate |
|---|---|---|
| **Call-graph linking, shared** (`CallGraph.scala` 30 + `StaticCallLinker` 41 + `DynamicCallLinker` 226 + `MethodRefLinker` 30 + `NaiveCallLinker` 29 + `LinkingUtil` 150) | **506** | **600–1,110** |
| **Type recovery, shared** (`XTypeRecovery` 1,331 + `SymbolTable` 155 + `XTypeHintCallLinker` 184 + `XInheritanceFullNamePass` 142 + `XImportResolverPass` 46 + `XImportsPass` 41 + `XTypeStubsParser` 42) | **1,941** | **2,700–4,690** |
| **Shared subtotal (additive)** | **2,447** | **3,300–5,800** |
| *(non-additive)* **closure/capture producer, shared** (`VariableScopeManager`) | *633* | *500–900 — **do not add**: this is the scope/binding resolver doc 20 §7.5 already prices as its swing factor. It is listed so the lead can see what the engine spends on it, not so it can be summed twice* |
| per-language — **JS/TS** (`frontendspecific/jssrc2cpg/`, 9 files, incl. `GlobalBuiltins` 1,094) | **1,684** | **1,900–2,900** |
| per-language — **Python** (`frontendspecific/pysrc2cpg/`, 9 files) | **641** | **1,000–1,700** |
| per-language — **Java** (`frontendspecific/javasrc2cpg/`, 3 files) | **113** | **200–400** |
| per-language — **C/C++** | **0** | 150–350 (name join + `methodFullName` convention only) |
| per-language — **Go** | **0** | 150–350 (ditto) |
| per-language — **Rust** | **0** | 150–350 (ditto) |
| **Per-language subtotal, the product's six frontends** | **2,438** | **3,550–6,050** |
| *(not taken: `frontendspecific/` for the 3 non-product frontends — php 588, ruby 577, swift 463)* | *1,628* | *—* |
| **Total for these families, six frontends (additive)** | **4,885** | **6,850–11,850** |

Cross-check on the aggregate: `find frontendspecific -name '*.scala' -exec cat {} + | wc -l` = **4,082**
= 1,684 + 641 + 113 + 588 + 577 + 463 + 16 (the package object). ✓

**Corrections to published line figures.** `12-source-anatomy-v4.0.627.md` §A3 and
`20-native-engine-post-mvp.md` §2 give "Type recovery: 5,413 lines (`XTypeRecovery` 1,331 plus
`frontendspecific/` 4,082)". Both component numbers re-measure correctly, but **the sum is not the
product's cost**: 1,628 of the 4,082 belong to PHP, Ruby and Swift, which the product never invokes,
and the 1,331 omits the 610 further shared lines in `passes/frontend/` that the recovery chain
actually needs (`SymbolTable`, `XTypeHintCallLinker`, `XInheritanceFullNamePass`, the two import
passes, the stub parser). The figure that is addable to an effort table is **2,447 shared + 2,438
per-language for the six product frontends = 4,885**, with the 633-line scope-manager row held out
because doc 20 §7.5 already carries it.

**Reconciling `passes/frontend/` in full**, since the brief asked for every file in that directory:
the directory aggregates to **2,206** and the shared type-recovery figure above is **1,941** over seven
files. The remaining **265** are three passes that are not in these families and are deliberately
excluded — `MetaDataPass` (**52**, `NewMetaData` plus the root `NewNamespaceBlock`), `TypeNodePass`
(**77**, materialises `NewType` nodes from the frontends' registered type-full-name strings) and
`XConfigFileCreationPass` (**136**, ingests config files as `NewConfigFile` nodes). None resolves a
call, a type reference or an import, and the product stages none of their output: `META_DATA`,
`NAMESPACE_BLOCK` and `TYPE` are recognised-and-not-staged (`scratch.go:60-61`) and `CONFIG_FILE` is
not classified at all, so it is counted as an unknown label. 1,941 + 265 = 2,206. ✓ The callgraph figure "297" in the same tables
(`DynamicCallLinker` 226 + `StaticCallLinker` 41 + `MethodRefLinker` 30) re-measures correctly but
omits `NaiveCallLinker`'s 29 — which §B1.1 shows is live for JS and Python — and the 150-line
`LinkingUtil` helper both it and `MethodRefLinker` depend on. The addable callgraph figure is
**506**.

---

## Design questions decided in this file

1. **Is `NaiveCallLinker` dead weight?** Decided: **no.** It is not in the CallGraph layer, but it is
   in the post-processing chain of both frontends the product runs type recovery for. The published
   "dead weight" label is retracted here on the evidence at `frontendspecific/jssrc2cpg/package.scala:14`
   and `frontendspecific/pysrc2cpg/package.scala:21`.
2. **Does Java get type recovery?** Decided: **not on the product's pinned argv**, because the
   override is gated on a frontend flag the product never passes. Published as a correction (§B5)
   rather than inherited.
3. **Is the `calls` provenance conclusive from source alone?** Decided: **conclusive that neither
   CallGraph-layer linker can have produced it for JavaScript; not conclusive on the split between the
   type-hint linker and the naive name join.** The exact measurement that would settle the remainder
   is named in §B0 rather than estimated.
4. **Does the totals table include the scope resolver?** Decided: **listed but held out of the
   sum.** `VariableScopeManager`'s 633 lines are the scope/binding resolver doc 20 §7.5 already prices
   as its swing factor, and summing it here would make the lead add the same work twice when these
   totals reach doc 20's effort table. §B9 marks the row non-additive rather than deleting it, because
   what the engine spends on scope resolution is itself evidence for how that swing factor is priced.
5. **What about the three `passes/frontend/` files that are not type recovery?** Decided: **excluded,
   and reconciled explicitly** (§B9), so that 1,941 against the directory's 2,206 is a stated scoping
   decision rather than a silent 265-line gap.
6. **How are `GlobalBuiltins`' 1,094 lines sized?** Decided: as a **transcription**, ~1:1, not as
   algorithm work — it is a literal name list, and treating it as engineering effort would inflate the
   type-recovery estimate by a fifth for no reason.

---

## ADR-safe twins for this file's prose citations

The tables above carry their ADR-safe twin in their last column. The citations that appear in prose or
in a quoted code comment carry theirs here, so that every vendor-token occurrence in this file has one.
Where the file name itself carries a vendor token the twin names the role.

| research form (as used above) | ADR-safe twin |
|---|---|
| `joern-cli/src/main/scala/io/joern/joerncli/JoernParse.scala:71,140,155,167` | **the parse driver**, lines 71 and 140 (argument split and generator construction) and lines 155 and 167 (default overlays, then post-processing), at the tag |
| `joern-cli/frontends/jssrc2cpg/src/main/scala/io/joern/jssrc2cpg/astcreation/AstForExpressionsCreator.scala:35,35-45` | `jssrc2cpg/.../astcreation/AstForExpressionsCreator.scala:35,35-45` |
| `joern-cli/frontends/jssrc2cpg/src/main/scala/io/joern/jssrc2cpg/astcreation/AstNodeBuilder.scala:14-20,119-131` | `jssrc2cpg/.../astcreation/AstNodeBuilder.scala:14-20,119-131` |
| `joern-cli/frontends/x2cpg/src/main/scala/io/joern/x2cpg/Defines.scala:32` | `x2cpg/.../Defines.scala:32` |
| `joern-cli/frontends/x2cpg/src/main/scala/io/joern/x2cpg/X2Cpg.scala:383-385` | `x2cpg/.../X2Cpg.scala:383-385` |
| `console/src/main/scala/io/joern/console/cpgcreation/CpgGenerator.scala:56-58` | **the per-language generator**'s base class, lines 56-58 — the post-processing hook's default, which returns the graph unchanged |
| `console/src/main/scala/io/joern/console/cpgcreation/JavaSrcCpgGenerator.scala:16,26-30` | **the per-language generator** for Java, line 16 and lines 26-30 — the flag test and the conditional post-processing override |
| `console/src/main/scala/io/joern/console/cpgcreation/{JsSrc,PythonSrc,C,Go,Rust}CpgGenerator.scala` | **the per-language generator** for JavaScript (lines 36-39), Python (lines 26-29), C/C++, Go and Rust (no override at the tag) |
| `console/src/main/scala/io/joern/console/cpgcreation/package.scala:22` | **the generator factory**, line 22 — `config.withArgs(args)`, the only place a frontend argument reaches a generator |
| `joern-cli/frontends/javasrc2cpg/src/main/scala/io/joern/javasrc2cpg/Main.scala:133-135` | `javasrc2cpg/.../Main.scala:133-135` — where the type-recovery flag is declared |
| `internal/provider/dependence/joern/joern.go:11,178,186,204` | **the product's engine backend**, `internal/provider/dependence/<engine>/`, lines 11, 178, 186 and 204 — the documented argv, `Argv`, `NeutralOptions` and `Parse` |

Every other path cited in this file (`x2cpg/...`, `dataflowengineoss/...`, `semanticcpg/...`,
`internal/provider/dependence/neo4jcsv/...`) carries no vendor token and is quoted identically in both
forms.
