# Native-engine research, raw evidence — Joern source anatomy at v4.0.627

Verified against tag **v4.0.627** (`4bb889d96ce972e2ded50d0d5765c514c1a032cf`, grafted shallow) at
the engine clone. All paths relative to that root. Source reading only; nothing built,
nothing run.

Counting method: aggregates via `find … -exec cat {} + | wc -l`; itemized rows via explicit
`wc -l <path>`. (`xargs wc -l | tail -1` is wrong — it reports only the last chunk's total.)

---

## A1. Repository totals

| Scope | Files | Lines |
|---|---|---|
| All `*.scala` under `*/src/main/*` | 761 | **123,641** |
| All `*.scala` under `*/src/test/*` | 824 | **149,744** |
| All `*.scala` anywhere | 1,593 | 273,725 |

Test source outnumbers main source 1.21:1.

**Per-component** (main-source ordering; test column included since A4 needs it):

| Component | main | test |
|---|---|---|
| `joern-cli/frontends/x2cpg` | 15,557 | 3,348 |
| `joern-cli/frontends/swiftsrc2cpg` | 16,279 | 15,983 |
| `joern-cli/frontends/javasrc2cpg` | 9,846 | 20,498 |
| `joern-cli/frontends/rubysrc2cpg` | 7,912 | 12,776 |
| `joern-cli/frontends/pysrc2cpg` | 7,900 | 8,758 |
| `joern-cli/frontends/c2cpg` | 6,504 | 14,422 |
| `joern-cli/frontends/kotlin2cpg` | 6,487 | 15,438 |
| `joern-cli/frontends/jssrc2cpg` | 6,338 | 11,243 |
| `joern-cli/frontends/php2cpg` | 6,200 | 8,735 |
| `joern-cli/frontends/csharpsrc2cpg` | 4,428 | 6,479 |
| `joern-cli/frontends/gosrc2cpg` | 3,191 | 9,375 |
| `joern-cli/frontends/rust2cpg` | 2,945 | 9,704 |
| `joern-cli/frontends/ghidra2cpg` | 2,493 | 997 |
| `joern-cli/frontends/jimple2cpg` | 2,131 | 4,141 |
| `joern-cli/frontends/abap2cpg` | 1,755 | 1,795 |
| `semanticcpg` | 8,051 | 2,104 |
| `querydb` | 4,943 | 1,591 |
| `dataflowengineoss` | 4,675 | 535 |
| `console` | 3,415 | 1,285 |
| `joern-cli/src` | 1,518 | 429 |
| `linter-rules` (+ `linter-rules/input`) | 897 | 12 |
| `macros` | 176 | 96 |
| **Sum** | **123,641** ✓ | **149,744** ✓ |

**Non-Scala real product source** (excluding test fixtures/`testcode`):

| Path | Lines | What |
|---|---|---|
| `joern-cli/frontends/pysrc2cpg/pythonGrammar.jj` | 3,802 | JavaCC grammar; generates the Python parser at build time (`pysrc2cpg/build.sbt:17-31`) |
| `joern-cli/frontends/c2cpg/eclipse-cdt/CCorePlugin.java` | 102 | patch source for the repackaged `io.joern:eclipse-cdt-core`; not compiled into c2cpg |
| `x2cpg/src/main/javaBazel/.../JoernRunfilesLocatorJava.java` | 26 | bazel-only |
| `x2cpg/src/main/java/.../RunfilesLocatorJava.java` | 15 | |
| `macros/src/main/java/io/joern/console/q.java` | 8 | |
| `pysrc2cpg/src/main/java/io/joern/pythonparser/PositionToken.java` | 7 | |

No Kotlin, Rust, Go or JS product source in the tree. **Every AST-producing helper binary (astgen,
goastgen, rust_ast_gen, PHP-Parser) is downloaded from GitHub releases at build time.**

---

## A2. Schema and graph library — both external, not in the clone

| Thing | Pin | Where |
|---|---|---|
| codepropertygraph (node/edge classes, `Languages`, `ControlStructureTypes`, `NodeTypes`, `EdgeTypes`, `Operators`, `CpgLoader`) | `cpgVersion = "1.7.74"` | `build.sbt:5`, consumed as `"io.shiftleft" %% "codepropertygraph" % Versions.cpg` in every module |
| codepropertygraph (bazel path only) | git commit `e7b6e8da670e4b58a64ba153d197041c25fd798a` | `MODULE.bazel:4-14` |
| flatgraph (the graph store; `flatgraph.formats.neo4jcsv.Neo4jCsvExporter`, `flatgraph.Accessors`, `GNode`, `Edge`) | `0.1.34` — **bazel lock only** | `maven_install.json:612-639` |

**Plainly:** `build.sbt` never names flatgraph at all; it arrives transitively through
codepropertygraph 1.7.74. The `0.1.34` figure is what the **bazel** lockfile resolves; the version the
**sbt** build (which ships the distribution the product invokes) resolves cannot be determined from
this clone without running dependency resolution, which is a download.

**No generated node source exists in the tree.** `find` for `nodes`/`generated` directories returns
nothing; the only `io/shiftleft/...` source is `semanticcpg`'s hand-written extension DSL.
`joern-cli/build.sbt:67-76,105` corroborates.

**Line counts for flatgraph and the codepropertygraph schema cannot be produced under the
no-download rule. Not estimated.**

---

## A3. The generic passes the product depends on

Derived from the layer definitions, not from grepping pass names. The pipeline on the pinned argv is
`Base → ControlFlow → TypeRelations → CallGraph` (`x2cpg/.../X2Cpg.scala:383-385`) then `OssDataFlow`
(`DefaultOverlays.scala:20-24`), then the frontend's own post-processing (`JoernParse.scala:167`).

| Pass / file | Lines | Feeds |
|---|---|---|
| **ControlFlow layer** — `x2cpg/.../layers/ControlFlow.scala` | 39 | orchestration |
| `.../passes/controlflow/CfgCreationPass.scala` | 27 | CFG (prerequisite for CDG + REACHING_DEF) |
| `.../passes/controlflow/cfgcreation/CfgCreator.scala` | **773** | ″ |
| `.../passes/controlflow/cfgcreation/Cfg.scala` | 197 | ″ |
| `.../passes/controlflow/cfgdominator/CfgDominatorPass.scala` | 48 | `control_depends_on` |
| `.../cfgdominator/CfgDominator.scala` | 90 | ″ |
| `.../cfgdominator/CfgDominatorFrontier.scala` | 38 | ″ |
| `.../cfgdominator/{CfgAdapter,CpgCfgAdapter,ReverseCpgCfgAdapter,DomTreeAdapter}.scala` | 6 / 13 / 13 / 10 | ″ |
| `.../codepencegraph/CdgPass.scala` | **68** | **`control_depends_on` (CDG edges)** |
| `.../codepencegraph/CpgPostDomTreeAdapter.scala` | 11 | ″ |
| **Control-flow subtotal** (`passes/controlflow/`, the 12 files above; the 39-line layer definition is orchestration and sits outside the directory) | **1,294** | |
| **OssDataFlow layer** — `dataflowengineoss/.../layers/dataflows/OssDataFlow.scala` | 26 | orchestration |
| `dataflowengineoss/.../passes/reachingdef/` (7 files) | **962** | **`data_flows_to` (REACHING_DEF)** |
| **CallGraph layer** — `x2cpg/.../layers/CallGraph.scala` | 30 | orchestration |
| `.../passes/callgraph/DynamicCallLinker.scala` | 226 | **`calls`** |
| `.../passes/callgraph/StaticCallLinker.scala` | 41 | **`calls`** |
| `.../passes/callgraph/MethodRefLinker.scala` | 30 | METHOD_REF → REF |
| `.../passes/callgraph/NaiveCallLinker.scala` | 29 | **not in the CallGraph layer, but live**: it is in the JS and the Python post-processing chains (`frontendspecific/jssrc2cpg/package.scala:14`, `frontendspecific/pysrc2cpg/package.scala:21`), the two the product runs post-processing for, and its untyped bare-name joins are published at `static_analysis` precision |
| **Base layer** — `x2cpg/.../layers/Base.scala` | 39 | orchestration |
| `.../passes/base/ContainsEdgePass.scala` | 50 | **CONTAINS** (read by `CdgPass.scala:35`) |
| `.../passes/base/MethodStubCreator.scala` | 178 | METHOD stubs → `calls` targets |
| `.../passes/base/TypeDeclStubCreator.scala` | 58 | TYPE_DECL stubs |
| `.../passes/base/MethodDecoratorPass.scala` | 62 | METHOD_PARAMETER_OUT (needed by DdgGenerator) |
| `.../passes/base/AstLinkerPass.scala` | 62 | AST closure |
| `.../passes/base/FileCreationPass.scala` | 58 | **dead weight** (FILE not staged) |
| `.../passes/base/NamespaceCreator.scala` | 27 | **dead weight** |
| `.../passes/base/ParameterIndexCompatPass.scala` | 22 | AST hygiene |
| `.../passes/base/TypeRefPass.scala` | 30 | REF edges **from** TYPE nodes to their TYPE_DECL — not staged, but the second hop of `evalType` |
| `.../passes/base/TypeEvalPass.scala` | 43 | **not staged, not dead**: EVAL_TYPE is the first hop of `baseNode.evalType` (`EvalTypeAccessors.scala:44`), on which `FieldAccessLinkerPass` depends. Dead weight in the product's staging, load-bearing in the pipeline |
| **TypeRelations layer** — `x2cpg/.../layers/TypeRelations.scala` | 28 | orchestration |
| `.../passes/typerelations/TypeHierarchyPass.scala` | 33 | **dead weight** (INHERITS_FROM) |
| `.../passes/typerelations/AliasLinkerPass.scala` | 28 | **dead weight** (ALIAS_OF) |
| `.../passes/typerelations/FieldAccessLinkerPass.scala` | 89 | indirectly helps `reads`/`writes` resolution |

**Type recovery** — what makes CALL edges resolvable in dynamic languages. It is **not** in the four
overlay layers; it runs from each generator's `applyPostProcessingPasses` at `JoernParse.scala:167`.

| File | Lines | Runs for |
|---|---|---|
| `x2cpg/.../passes/frontend/XTypeRecovery.scala` | **1,331** | shared engine |
| `x2cpg/.../passes/frontend/XTypeHintCallLinker.scala` | 184 | shared |
| `x2cpg/.../passes/frontend/SymbolTable.scala` | 155 | shared |
| `x2cpg/.../passes/frontend/XInheritanceFullNamePass.scala` | 142 | shared |
| `x2cpg/.../frontendspecific/jssrc2cpg/JavaScriptTypeRecovery.scala` | 225 | JS/TS |
| `x2cpg/.../frontendspecific/jssrc2cpg/GlobalBuiltins.scala` | 1,094 | JS/TS |
| `x2cpg/.../frontendspecific/jssrc2cpg/JavaScriptImportResolverPass.scala` | 131 | JS/TS |
| `x2cpg/.../frontendspecific/pysrc2cpg/PythonTypeRecovery.scala` | 237 | Python |
| `x2cpg/.../frontendspecific/pysrc2cpg/PythonImportResolverPass.scala` | 153 | Python |
| `x2cpg/.../frontendspecific/pysrc2cpg/DynamicTypeHintFullNamePass.scala` | 99 | Python |
| `x2cpg/.../frontendspecific/javasrc2cpg/JavaTypeRecoveryPassGenerator.scala` | 66 | Java |
| **`frontendspecific/` total** | **4,082** | |

**Which of the product's six frontends actually get type recovery**
(`grep applyPostProcessingPasses console/.../cpgcreation/*.scala`):

| Generator | Overrides? | Consequence |
|---|---|---|
| `JsSrcCpgGenerator.scala:36` | yes | JS/TS CALL edges resolvable |
| `PythonSrcCpgGenerator.scala:26` | yes | Python CALL edges resolvable |
| `JavaSrcCpgGenerator.scala:16,26-30` | **gated, so no on the pinned argv** | the override runs type recovery only under `--enable-type-recovery`, which reaches the generator through the frontend-args delimiter alone; the product passes no frontend args, so **Java gets no type recovery**. Four of the six product frontends have none, not three |
| `CCpgGenerator.scala` (25 lines) | **no** | C/C++ falls back to `CpgGenerator.scala:57-58` no-op |
| `GoCpgGenerator.scala` (48 lines) | **no** | **Go gets no type recovery** |
| `RustCpgGenerator.scala` (20 lines) | **no** | **Rust gets no type recovery** |

---

## A4. Per-frontend anatomy

| Frontend | main | test | Parser | Parser lines (in-tree) | Lowering (`astcreation/`) |
|---|---|---|---|---|---|
| **x2cpg** (shared base) | 15,557 | 3,348 | n/a | n/a | holds CFG/CDG/type-recovery/AST builders |
| **c2cpg** | 6,504 | 14,422 | Eclipse CDT, `"io.joern" % "eclipse-cdt-core" % Versions.eclipseCdt` = `9.2.100.202507101054+1` (`c2cpg/build.sbt:14`) | 600 (glue only) | 4,724 |
| **javasrc2cpg** | 9,846 | 20,498 | `com.github.javaparser % javaparser-symbol-solver-core % 3.28.0` (`javasrc2cpg/build.sbt:11`) + `gradle-tooling-api 8.3`, `lombok 1.18.42` | 0 — **no `parser/` dir** | 5,589 (+1,126 `typesolvers/`) |
| **jssrc2cpg** | 6,338 | 11,243 | `@joernio/astgen` binary, `astgen_version: "3.50.1"` (`src/main/resources/application.conf`), downloaded (`build.sbt:78`) | 613 (`BabelAst.scala` 539 + glue 74) | 4,832 |
| **gosrc2cpg** | 3,191 | 9,375 | `goastgen` binary, `goastgen_version: "0.1.0"`, downloaded (`build.sbt:81`) | 172 (`ParserAst.scala` 121 + glue 51) | 2,089 |
| **pysrc2cpg** | 7,900 | 8,758 | **own in-tree parser**: JavaCC grammar `pythonGrammar.jj` (3,802) code-generated at build (`build.sbt:17-31`) | 2,550 Scala + 7 Java + 3,802 `.jj` | 0 — lowering is `PythonAstVisitor.scala` (2,462) |
| **rust2cpg** | **2,945** | **9,704** | `rust_ast_gen` binary, `rust_ast_gen_version: "0.21.6"`, downloaded (`build.sbt:41,84`) | 156 — **plus `RustNodeSyntax.scala`, which is NOT in the tree**: downloaded into `sourceManaged` (`build.sbt:96-103`) | 2,618 |

**Measurement hazard worth naming:** rust2cpg's 2,945 is **not comparable** to the others because its
node-type definitions are fetched at build time. The in-tree analogue is
`swiftsrc2cpg/.../parser/SwiftNodeSyntax.scala` at 7,264 lines — 45% of swiftsrc2cpg's whole 16,279.
rust2cpg is the only frontend that downloads *Scala source*.

**Dead weight — the 8 frontends the product never invokes** (15 frontends − 6 product frontends −
x2cpg = 8: abap2cpg, csharpsrc2cpg, ghidra2cpg, jimple2cpg, kotlin2cpg, php2cpg, rubysrc2cpg,
swiftsrc2cpg):

| | Lines |
|---|---|
| main | **47,685** |
| test | **66,344** |

That is **38.6% of all main Scala in the repository**.

---

## A5. The export path

| File | Lines |
|---|---|
| `joern-cli/src/main/scala/io/joern/joerncli/JoernExport.scala` | 234 |
| `joern-cli/src/main/scala/io/joern/joerncli/JoernParse.scala` | 202 |
| `joern-cli/src/main/scala/io/joern/joerncli/CpgBasedTool.scala` | 70 |
| `joern-cli/src/main/scala/io/joern/joerncli/DefaultOverlays.scala` | 27 |
| **whole `io/joern/joerncli/` dir (13 files)** | **1,490** |
| neo4jcsv exporter | **external** — `flatgraph.formats.neo4jcsv.Neo4jCsvExporter`, imported at `JoernExport.scala:8`. Not in the clone; line count not producible under the no-download rule. |

**The overlay question, settled.** `JoernParse.scala:91` → `applyDefaultOverlays` →
`DefaultOverlays.create` (`:159`) → `X2Cpg.applyDefaultOverlays` (Base/ControlFlow/TypeRelations/
CallGraph, `X2Cpg.scala:383-385`) **and then** `new OssDataFlow(options).run(context)` at
`DefaultOverlays.scala:23`. **REACHING_DEF is produced on the pinned argv.** Belt and braces:
`JoernExport.scala:105` calls `CpgBasedTool.addDataFlowOverlayIfNonExistent`, which re-runs
`OssDataFlow` if the overlay is missing (`CpgBasedTool.scala:28-35`). The pinned `--max-num-def 40000`
overrides `defaultMaxNumberOfDefinitions = 4000` (`DefaultOverlays.scala:11`).

**`--repr=all --format=neo4jcsv` — correcting the brief.** Enums are
`Representation { Ast, Cfg, Ddg, Cdg, Pdg, Cpg14, Cpg, All }` (`JoernExport.scala:34-36`) and
`Format { Dot, Neo4jCsv, Graphml, Graphson }` (`:48-50`). `Format.Neo4jCsv` dispatches unconditionally
to `exportWithFlatgraphFormat` (`:113-114`), which accepts **two** reprs (`:144-166`): `All` →
`exporter.runExport(cpg.graph, outDir)`; `Cpg` → per-method subgraph split. Every other repr throws
`NotImplementedError` at `:165`.

So `--repr=cpg --format=neo4jcsv` is *legal* — "the only legal way" is **false as stated**. It is
nonetheless **unusable** for the product, for three reasons: (1) it emits one output directory per
method (`:152-161`), not one flat CSV set; (2) `MethodSubGraph.edges` filters
`if nodes.contains(edge.dst)` (`:230`), so every inter-procedural CALL edge is dropped — the `calls`
family collapses; (3) the node set is `method.ast.toSet` (`:177`), so staged labels not AST-reachable
from a METHOD (TYPE_DECL, MEMBER) never appear. **`--repr=all --format=neo4jcsv` remains the only
usable way to get CDG + REACHING_DEF + CALL in CSV.**

---

## A6. rust2cpg — the blocking question, settled

**(a) It exists.** `docs/research/05-cfg-cdg-from-treesitter.md` §1.4's *"Joern has no Rust frontend"*
is **FALSE at v4.0.627**. `joern-cli/frontends/rust2cpg/` — **2,945 main / 9,704 test** Scala lines,
13 main files, 35 test files. Registered at `build.sbt:27,53` and `project/Projects.scala:33`. Parser:
`rust_ast_gen` helper binary, pinned `0.21.6` in `rust2cpg/src/main/resources/application.conf`,
downloaded from `joernio/astgen-monorepo` at `build.sbt:41,84`.

**(b) Rust is NOT skipped.** `x2cpg/src/main/scala/io/joern/x2cpg/layers/ControlFlow.scala:17-24`,
verbatim:

```scala
  def passes(cpg: Cpg): Iterator[CpgPassBase] = {
    val cfgCreationPass = cpg.metaData.language.lastOption match {
      case Some(Languages.GHIDRA) => Iterator[CpgPassBase]()
      case Some(Languages.LLVM)   => Iterator[CpgPassBase]()
      case _                      => Iterator[CpgPassBase](new CfgCreationPass(cpg))
    }
    cfgCreationPass ++ Iterator(new CfgDominatorPass(cpg), new CdgPass(cpg))
  }
```

Only GHIDRA and LLVM are skipped. Rust takes `case _` and gets `CfgCreationPass`.
`CfgCreationPass.scala:19` is `cpg.method.toArray` — no language filter.

**(c) It does emit ControlStructure nodes** — indirectly. `grep 'ControlStructure' rust2cpg/src/main/`
returns **0 hits**, but that is a false negative: rust2cpg builds them through the x2cpg helpers in
`x2cpg/.../internal/ControlStructureAstBuilder.scala`, which set the types internally.

| Rust construct | `RustVisitor.scala` | Helper | Emitted type | `CfgCreator` handles? |
|---|---|---|---|---|
| `if` / `if let` | :926, :951 | `ifThenElseAst` (`:482-500`) | `IF` (`:489`) | yes, `:169` |
| `while` / `while let` | :988, :1007 | `whileAst` | `WHILE` (`:74`) | yes, `:161` |
| `loop` | :1033-1036 | `whileAst` with literal `true` | `WHILE` | yes |
| `for` | :1061-1130 | desugared to `whileAst` + `into_iter()`/`next()` | `WHILE` | yes |
| `match` (×2) | :1813, :1826 | `matchAst` | `MATCH` (`:158`) | yes, `:181` |
| match arms | :1853 | `jumpTargetNode` | JUMP_TARGET | yes, `:107` |
| `break` | :1142 | `breakAst` 3-arity | `BREAK` (`:334`) | yes, `:157` |
| `continue` | :1135 | `continueAst` 3-arity | `CONTINUE` (`:376`) | yes, `:159` |

The five types rust2cpg emits are a **strict subset** of the 14 `CfgCreator.scala:157-184` dispatches
on, so **nothing falls through to `case _ => Cfg.empty` at `:185-186`**. It emits no `FOR`, `DO`,
`SWITCH`, `TRY`, `CATCH`, `FINALLY`, `GOTO`, `THROW`.

There is no `ELSE` node — `ControlStructureAstBuilder.scala` has no `ControlStructureTypes.ELSE` site
at all. This is **correct, not a bug**: `ifThenElseAst:493,497` attaches branches via
`withTrueBodyEdge`/`withFalseBodyEdge`, and `cfgForIfStatement` (`CfgCreator.scala:562-565`) reads them
back through `node.whenTrue`/`node.whenFalse`.

**Rust-specific fidelity losses — these are the real risk, not unhandled types:**

| Loss | Evidence |
|---|---|
| **`?` (try operator) is invisible to the CFG** — lowered to a plain CALL, no control structure, so every early return via `?` is a straight-line edge | `RustVisitor.scala:1355-1359`: `operatorCallNode(tryExpr, code(tryExpr), RustOperators.tryUnwrap, ...)` then `callAst(callNode, Seq(exprAst))` |
| **Labeled `break 'outer` / `continue 'outer` bind to the innermost loop** — the `Lifetime` label is parsed but dropped; the 3-arity `breakAst`/`continueAst` emit a bare jump with no `JumpLabel` child | `RustVisitor.scala:1135,1142` (comments at `:1133,:1139` show `Lifetime?`); `ControlStructureAstBuilder.scala:334,376` vs. the label-carrying 4-arity forms at `:306,:348` |
| **`for` loop condition is UNKNOWN**, so no `TrueEdge`/`FalseEdge` discrimination | `RustVisitor.scala:1126` `unknownNode(forExpr, ...)`; in-tree admission at `:1055-1060`: *"NB: the condition is currently UNKNOWN. A more faithful lowering would be: WHILE (TRUE) { match tmp.next() { Some(pat) => body, None => break } }"* |
| Same for `if let` / `while let` conditions | `:966`, `:1022` |
| Unhandled constructs degrade to `notHandledYet` | `rust2cpg/.../astcreation/AstCreator.scala:83`; call sites `RustVisitor.scala:246,259,358,638,702,876` |
| **No type recovery** → `calls` for Rust is whatever `StaticCallLinker` resolves from `methodFullName` | `RustCpgGenerator.scala` has no `applyPostProcessingPasses` override |
| **Zero in-tree CFG/CDG/dataflow test coverage** | all 35 test files are AST-level; `CfgAttributeTests.scala` (264 lines) is about `#[cfg(...)]` *conditional-compilation attributes*, not the control-flow graph |

Exhaustive enumeration of unmodelled Rust statement kinds is **not obtainable** under the no-download
rule: `RustNodeSyntax.scala` (the full node universe) is fetched at build time. The in-tree proxy is
`RustNodeSyntaxExtensions.scala` (104 lines) plus the `visitExpr` dispatch at `RustVisitor.scala:101-127`.

**gosrc2cpg re-check at v4.0.627 — unchanged from v4.0.100:**

| Claim | Status | Evidence |
|---|---|---|
| `case _: BaseStmt => Seq(Ast())` fall-through | **still there** | `gosrc2cpg/.../astcreation/AstForStatementsCreator.scala:48` (and `case Unknown => Seq(Ast())` at `:47`) |
| DeferStmt / GoStmt / SelectStmt unmodelled | **worse than fall-through — the node kinds do not exist** | `gosrc2cpg/.../parser/ParserAst.scala:37-50` lists 13 `BaseStmt` objects; no `DeferStmt`, `GoStmt`, `SelectStmt` or `CommClause` anywhere in the frontend |
| LabeledStmt unmodelled | **still** — declared at `ParserAst.scala:50`, never dispatched in `astsForStatement` (`:33-50`), so it hits the fall-through. Consequence: `gotoAst` **is** emitted (`AstForStatementsCreator.scala:257-261`) but no JUMP_TARGET is ever created, so `withResolvedJumpToLabel()` (`CfgCreator.scala:62`) has nothing to resolve — **Go's gotos dangle** |
| fallthrough unmodelled | **still** | `AstForStatementsCreator.scala:262-263`: `case "fallthrough" => // TODO handling for FALLTHROUGH` / `Ast()` |

**(d) Rust is fully wired into `--language`.** `Languages.RUST` (enum member in the external
codepropertygraph, referenced at `ImportCode.scala:182`), registered at
`CpgGeneratorFactory.scala:29` in `KNOWN_LANGUAGES`, dispatched at `cpgcreation/package.scala:42` →
`RustCpgGenerator`, auto-detected at `package.scala:100-101,118` (`isRustFile`: `.rs`, `cargo.toml`,
`cargo.lock`). `JoernParse.scala:109` lists it via `Languages.ALL`.

### A6 verdict

**Wired, structurally non-hollow, lossy, and untested.** rust2cpg exists and is a first-class frontend
at v4.0.627, so doc 05 is out of date on this point. Rust is not skipped in `ControlFlow.scala`, and
every ControlStructure type rust2cpg emits (WHILE, MATCH, IF, BREAK, CONTINUE) is a strict subset of
what `CfgCreator` dispatches on — nothing hits `case _ => Cfg.empty`. So the engine really will hand
the product non-empty CDG and REACHING_DEF edges for Rust; `data_flows_to` and `control_depends_on`
will not be empty. **The problem is fidelity, not wiring**: `?` — the single most common control-flow
construct in idiomatic Rust — is lowered to a plain CALL with no control structure at all, so every
early return through it is invisible to the CFG; labeled `break`/`continue` collapse onto the
innermost loop; and `for`/`if let`/`while let` conditions are UNKNOWN nodes that give the CFG no
true/false discrimination. rust2cpg also gets no type recovery on the `joern-parse` path, so `calls`
for Rust depends entirely on `methodFullName` resolution. Most importantly for a differential oracle,
**rust2cpg ships zero CFG, CDG or dataflow tests** — all 35 of its test files are AST-level. Compare
c2cpg (11 CFG/dataflow test files), jssrc2cpg (9), gosrc2cpg (11). Rust's control-flow output is
therefore **unvalidated upstream**: a Go port would have no reference oracle for it, and neither does
Joern.

---

## A7. Per-language CFG-fidelity anchor (the honest port-delta)

| Frontend | Statement-lowering file(s) | Lines |
|---|---|---|
| **c2cpg** | `c2cpg/.../astcreation/AstForStatementsCreator.scala` | **634** |
| **gosrc2cpg** | `gosrc2cpg/.../astcreation/AstForStatementsCreator.scala` | **266** |
| **javasrc2cpg** | `.../statements/AstForStatementsCreator.scala` (148) + `AstForSimpleStatementsCreator.scala` (380) + `AstForForLoopsCreator.scala` (470) | **998** |
| **jssrc2cpg** | `jssrc2cpg/.../astcreation/AstForStatementsCreator.scala` | **816** |
| **pysrc2cpg** | `PythonAstVisitor.scala` (2,462) + `PythonAstVisitorHelpers.scala` (710) — statements and expressions are one visitor | **3,172** |
| **rust2cpg** | `RustVisitor.scala` — no separate statements file | **1,960** |

Shared, language-independent floor a port must reimplement once: `CfgCreator.scala` 773 +
`Cfg.scala` 197 + `CfgCreationPass.scala` 27 + cfgdominator (7 files) 218 + codepencegraph (2 files)
79 = **1,294 lines** — the whole of `passes/controlflow/`, 12 files, confirmed by
`find … -exec cat {} + | wc -l` — plus `reachingdef/` **962** and the callgraph linkers **297**.

---

## Re-verification of the five v4.0.100 figures cited by doc 05

| File | v4.0.100 | v4.0.627 | Moved? |
|---|---|---|---|
| `x2cpg/.../passes/controlflow/cfgcreation/CfgCreator.scala` | 649 | **773** | **+124 (+19.1%)** |
| `.../cfgcreation/Cfg.scala` | 197 | **197** | no |
| `.../controlflow/CfgCreationPass.scala` | 27 | **27** | no |
| `.../cfgdominator/CfgDominator.scala` | 90 | **90** | no |
| `.../codepencegraph/CdgPass.scala` | 62 | **68** | **+6 (+9.7%)** |
| *"Joern has no Rust frontend"* | claimed | **FALSE** | rust2cpg: 2,945 main / 9,704 test |

The generic CFG/CDG core is essentially frozen across 527 patch releases — only `CfgCreator` moved
meaningfully. **Doc 05's Rust claim must be retracted.**
