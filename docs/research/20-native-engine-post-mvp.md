
# A native dependence engine: every pass the product consumes, sized from the engine's source, with the algorithm chosen and the path that retires the engine

Research note for the codectx product owner, extending `04-joern-cost-anatomy.md` (the engine's
architecture and cheaper routes) and `05-cfg-cdg-from-treesitter.md` (the CFG+CDG sizing). Research
date 2026-09-16.

**Version discipline.** Every engine citation in this note is pinned to **`v4.0.627`**
(`4bb889d96ce972e2ded50d0d5765c514c1a032cf`, 2026-09-11), the version the product actually runs — not
to `v4.0.100`, which docs 04 and 05 cited and which is 527 patch releases behind. That difference is
load-bearing: one of doc 05's two structural findings does not survive the re-check. The engine was
cloned shallow outside the product tree; that clone is the one download this research made. Nothing was
built and the engine was never run. Store figures come from `sqlite3 -readonly` on two real run
artifacts on this host, and the reference implementations behind §7.2 were read as source in the local
Go module cache, never resolved, built or benchmarked. Raw evidence, including each
sub-investigation's complete findings, is in the files `00`–`19` under `raw/native-engine/`, cited by
name throughout.

**Standing under the direction ruling.** `00-synthesis.md` §8 (2026-09-13, user-adopted) makes
engine-backed `dependence` the MVP backend and names the pinned benchmark corpora as the differential
oracle for *any future native engine*. This note is that future, scoped, costed and **gated** now: it carries a
full-retirement path in which every pass the product consumes is ported and the JVM-hosted engine is
gone at the end, with each phase's residue named and each phase's end stated as a measured condition.
Nothing in it is to be started before the MVP ships, and nothing in it reverses the ruling.

The decision this note supports, written to stand on its own, is
[ADR-0012](../adr/ADR-0012-native-dependence-engine.md).

---

## 0. Bottom line

Five findings decide the shape, and none of them is a line count.

1. **The product consumes the output of about 2,843 of the engine's 123,641 main Scala lines**, plus a
   further 4,885 addable lines of call linking and type recovery that make its `calls` resolvable
   (§2). This is not a rewrite of the engine and should never be described as one: 38.6% of the engine
   is frontends the product never invokes, and most of the remainder is a schema, a graph store, a
   query DSL and a console.
2. **The facts a native engine would produce are already proven file-local, by the engine itself.**
   Run on one file, the engine reproduces 100% of CDG and REACHING_DEF for Python, TypeScript, C and
   Java (`05-file-locality.md`). Subdividing a project keeps CDG 99.7% and REACHING_DEF 99.9% while
   resolved calls collapse to 46% (`03-parity-and-oracle.md`). The intraprocedural families are
   per-function; `calls` needs a project scope.
3. **`calls` is not replaceable by precise indexers plus language servers on the repository that
   motivated the question.** Had every planned precise unit run on r3, precise resolution would cover
   **4.87–5.25%** of its call sites (`14-store-counts-r3.md`). So `calls` cannot be left to the
   precise tier — which is exactly why it must be **ported**, not left on the engine indefinitely.
4. **The target is much smaller than the engine's call graph looks.** Of the engine's 217,881 call
   sites on r3, only **40,743 (18.7%), and 27,897 of 115,940 edges (24.1%), resolve to an in-repo
   definition**; the other 177,138 point at external stubs and reproduce no capability. Every `calls`
   gate in this plan is stated on the in-repo-resolved subset (`15-requirements-audit.md`).
5. **Four of the six frontends get no type recovery at all**, verified in the generator drivers at the
   tag: C/C++, Go and Rust never override post-processing, and Java's override is gated on a flag the
   product never passes (`16-passes-callgraph-and-types.md` §B2). For those four the engine's `calls`
   is a name join over a frontend-minted full name — which a port reaches for 150–350 Go lines each.
   Type recovery is load-bearing for **two** frontends, the ECMAScript family and Python, and those
   carry **95.24%** of r3's call sites.

The recommendation (§7) is a **full-retirement path in seven phases**, each naming exactly what still
needs the engine, by fact family and by language, and each ending on a measured condition. The last
phase is a retirement gate with five simultaneous conditions, after which the engine, its backend
package and its import path are deleted. Nothing in it starts before the MVP ships.

---

## 1. Corrections to docs 04 and 05 at the pinned version

Doc 05's §4 estimate and §6 recommendation rest on measurements taken at `v4.0.100`. Four moved.

| Claim | At `v4.0.100` | At `v4.0.627` |
|---|---|---|
| **"Joern has no Rust frontend"** (05 §1.4, §6) | asserted | **false** — `rust2cpg`, 2,945 main / 9,704 test Scala lines |
| `CfgCreator.scala` | 649 | **773** (+19.1%) |
| `CdgPass.scala` | 62 | **68** (+9.7%) |
| `Cfg.scala` / `CfgCreationPass.scala` / `CfgDominator.scala` | 197 / 27 / 90 | **unchanged** |
| `stack-graphs` (05 §3) | live, inapplicable | **archived 2025-09-09**, and no `cdylib`/`staticlib`, so no C ABI |
| Per-language statement creators (05 §1.2, the Joern-side upper bound) | c 321, java ≈260, py 2,349, js 916, go 273 | **c 634, java 998, py 3,172**, js 816, go 266 |

The generic CFG/CDG core is essentially frozen — only `CfgCreator` moved meaningfully across 527
releases, which is itself a useful stability signal for anyone porting it. The per-language creators
moved in *both* directions, and Fraunhofer's 125-line Go delta (05's other anchor) did not move at all.
**That spread is the uncertainty in any per-language estimate and it must be stated rather than
averaged away.**

Two further corrections, to this note's own commissioning brief and to `providers-dependence.md`:

- **`--repr=cpg --format=neo4jcsv` is legal.** `Format.Neo4jCsv` dispatches to
  `exportWithFlatgraphFormat`, which accepts `All` *and* `Cpg` (`JoernExport.scala:144-166`). It is
  nonetheless unusable: one output directory per method (`:152-161`), `MethodSubGraph.edges` filters
  `if nodes.contains(edge.dst)` so every inter-procedural CALL edge is dropped (`:230`), and the node
  set is `method.ast.toSet` so TYPE_DECL and MEMBER never appear (`:177`). The correct wording is
  **"the only usable way"**, not "the only legal way".
- **The product discards the `REACHING_DEF` `VARIABLE` property.** The export carries it — every
  fixture header is `:START_ID,:END_ID,:TYPE,VARIABLE:string` — and `putEdge` keeps only
  `(label, src, dst)` (`scratch.go:413-420`). The brief's premise that the product imports "the
  variable" is wrong.

**Corrections to this note's own earlier figures**, every one re-measured against the clone:

| claim | published | measured | source |
|---|---|---|---|
| `passes/controlflow/` (12 files) | 1,284 | **1,294** | the directory aggregate; the itemised rows sum the same once `cfgdominator/` is 218 rather than 208 |
| the subtotal the product consumes | 2,833 | **2,843** | 1,294 + 962 + 297 + 290 |
| "type recovery: 5,413 lines" as a cost | 5,413 | **4,885 addable** | 5,413 is the aggregate of two directories: 1,628 of it belongs to three frontends the product never invokes, and it omits 610 shared lines the chain needs (`16-passes-callgraph-and-types.md` §B9) |
| frontends without type recovery | three (C/C++, Go, Rust) | **four** — Java's override is gated on a flag the product never passes | `16-passes-callgraph-and-types.md` §B2 |
| the bare-name call linker | "not in the layer — dead weight" | **live** in the ECMAScript and Python post-processing chains, publishing untyped joins at `static_analysis` precision | `16-passes-callgraph-and-types.md` §B3 |
| `TypeEvalPass` / `TypeRefPass` | "dead weight" | **unstaged but load-bearing**: `FieldAccessLinkerPass` resolves through `EVAL_TYPE → TYPE → REF → TYPE_DECL` | `17-passes-structural-and-cfg.md` §1 |
| importer deletion credit | −4,874 | **−4,535** | 4,874 lines of package source less the 339-line fact-key comparator, which relocates because it is the oracle (`15-requirements-audit.md`) |
| "no Go package exposes a dominance frontier" | flat negative | true of **public APIs only** — `go/ssa`'s `lift.go:99-106` has one, unexported, ~28 portable BSD-3 lines | `18-algorithms-dominance-and-dataflow.md` |
| `golang.org/x/tools` pin | v0.50.0 | **v0.49.0** is the highest readable on this host; the v0.50.0 row was a network observation and no source claim is checked against it | `18-`, `19-` |
| the engine's control-flow and dataflow tests | "7,342 lines over five families" (dataflow only) | **9,901 lines over five frontends** including control-flow tests, harness fixtures excluded — c 3,414/9 files, ECMAScript 2,157/5, Java 1,917/15, Python 1,347/1, Go 1,066/10, **Rust 0/0** | measured at the tag |
| "nine tree-sitter grammars" | nine modules implied | **nine grammar registrations from eight pinned grammar modules** (the TypeScript module serves both `typescript` and `tsx`) | `go.mod`; `internal/provider/treesitter/lang/lang.go:88-89` |

Full list of the version-drift corrections with sources: `13-corrections.md`.

---

## 2. Source anatomy: what a port takes, and what it leaves

`12-source-anatomy-v4.0.627.md` carries the complete tables; `16-passes-callgraph-and-types.md` and
`17-passes-structural-and-cfg.md` carry the per-pass inventories. The shape:

**123,641 main Scala lines, 149,744 test lines.** The CPG schema (codepropertygraph 1.7.74,
`build.sbt:5`) and the graph store (flatgraph) are **external sbt dependencies and are not in the
clone**; their line counts cannot be produced under the no-download rule and are deliberately not
estimated. `build.sbt` never names flatgraph — it arrives transitively.

**The four dependence families the product publishes today** rest on this much:

| | main lines | fact family |
|---|---|---|
| `x2cpg/.../passes/controlflow/` (12 files: CFG creation, dominators, dominance frontier, CDG) | 1,294 | `control_depends_on` |
| `dataflowengineoss/.../passes/reachingdef/` (7 files) | 962 | `data_flows_to` |
| callgraph linkers in the layer (`DynamicCallLinker` 226, `StaticCallLinker` 41, `MethodRefLinker` 30) | 297 | `calls` |
| `ContainsEdgePass` 50, `MethodStubCreator` 178, `MethodDecoratorPass` 62 | 290 | attribution and `calls` targets |
| **subtotal the product consumes directly** | **2,843** | |

**And this much more, which the previous version of this note wrote off as dead weight and which the
owner's scope names explicitly.** These are the figures that are addable to an effort table, not the
raw directory aggregates (`16-passes-callgraph-and-types.md` §B9):

| | main lines | Go estimate |
|---|---|---|
| Call-graph linking, shared (layer 30 + the three linkers 297 + the bare-name linker 29 + linking utilities 150) | **506** | 600–1,110 |
| Type recovery, shared (the recovery fixed point 1,331 + symbol table 155 + hint call linker 184 + inheritance full names 142 + two import passes 87 + stub parser 42) | **1,941** | 2,700–4,690 |
| **Shared subtotal, additive** | **2,447** | **3,300–5,800** |
| Per-language: ECMAScript family 1,684 (of which a builtin table is 1,094), Python 641, Java 113, and C/C++, Go and Rust 0 each | **2,438** | **3,550–6,050** |
| **Total, the product's six frontends** | **4,885** | **6,850–11,850** |

Held out of the sum on purpose: the closure and capture scope manager (633 Scala lines), because §7.5
already prices a scope/binding resolver as its swing factor and it must not be counted twice; and
1,628 lines of `frontendspecific/` belonging to three frontends the product never invokes. **The
familiar "5,413" figure is the measured aggregate of two directories, not the product's cost** — it
includes those 1,628 and omits 610 shared lines the recovery chain needs.

**Which frontends actually get type recovery, re-verified in the generator drivers.** Only two on the
pinned argv: the ECMAScript family and Python. C/C++, Go and Rust never override post-processing, and
Java's override is gated on `--enable-type-recovery`, which reaches the generator only through the
frontend-args delimiter — and the product passes no frontend args. So **four of six frontends get a
name join and nothing more** (`16-passes-callgraph-and-types.md` §B2). This is a material fact about
what the product pays for and what it gets.

**Where the engine's resolved `calls` actually comes from.** On r3 neither CallGraph-layer linker can
have produced the in-repo JavaScript bucket: every non-builtin JavaScript call is lowered to a dynamic
dispatch whose full name is the unknown-name sentinel, on which `DynamicCallLinker` returns at its
first line, and `StaticCallLinker`'s whole JavaScript input is platform builtins that can only match
an invented stub. The 18.7% therefore comes from the post-processing chain — the hint call linker,
which also invents the stubs, plus the bare-name linker. The split between those two is **not
determinable from source**; the experiment that would settle it needs an engine run, which this
research is forbidden to make, so it is recorded as unavailable (`16-passes-callgraph-and-types.md`).

**What is genuinely not consumed**, re-checked at the tag: `FileCreationPass` 58 and `NamespaceCreator`
27 (their labels are never staged); `TypeHierarchyPass` 33 and `AliasLinkerPass` 28 (staged labels the
importer does not map). Two passes previously on this list are **not** dead: the bare-name linker is
live in the ECMAScript and Python post-processing chains and its untyped joins are published at
`static_analysis` precision, and `TypeEvalPass` 43 with `TypeRefPass` 30 are unstaged but load-bearing
in the pipeline — `FieldAccessLinkerPass` resolves a field through `EVAL_TYPE → TYPE → REF →
TYPE_DECL` and no source comment declares the dependency (`17-passes-structural-and-cfg.md` §1). That
is precisely the class of omission that makes a port's graph quietly wrong.

**Eight frontends the product never invokes** (abap, csharp, ghidra, jimple, kotlin, php, ruby,
swift): **47,685 main and 66,344 test lines — 38.6% of all main Scala in the repository.**

**Per frontend, parser versus lowering.** Only `pysrc2cpg` owns its parser, and it owns it as a
3,802-line JavaCC grammar code-generated at build time. `c2cpg` drives Eclipse CDT
(`eclipse-cdt-core 9.2.100.202507101054+1`); `javasrc2cpg` drives `javaparser-symbol-solver-core 3.28.0`
plus a Gradle tooling API and Lombok; `jssrc2cpg`, `gosrc2cpg` and `rust2cpg` each **download a
per-platform helper binary at build time** (`@joernio/astgen 3.50.1`, `goastgen 0.1.0`,
`rust_ast_gen 0.21.6`). `rust2cpg` is the only frontend that also downloads *Scala source*
(`RustNodeSyntax.scala`, into `sourceManaged`), which makes its 2,945-line figure incomparable to its
siblings — the in-tree analogue, `swiftsrc2cpg`'s `SwiftNodeSyntax.scala`, is 7,264 lines on its own.

This is the structural reason the engine cannot be "made fast": a native engine would add **no parser
at all**, because the product already pins nine grammar registrations from eight grammar modules and
the cgo binding (`01-product-anchors.md`).

---

## 3. Rust: the finding that changes doc 05's argument, and what replaces it

Doc 05 §6 opens with a forced move — *"Build the shared core plus the Rust mapping. This is a
requirement, not a choice"* — justified by the absence of a Rust frontend. **That premise is dead.**

`rust2cpg` exists at `v4.0.627`, is registered in `build.sbt:27,53`, is auto-detected from `.rs` /
`cargo.toml` / `cargo.lock`, and — decisively — is **not** skipped in the control-flow layer:

```scala
// x2cpg/src/main/scala/io/joern/x2cpg/layers/ControlFlow.scala:17-24
val cfgCreationPass = cpg.metaData.language.lastOption match {
  case Some(Languages.GHIDRA) => Iterator[CpgPassBase]()
  case Some(Languages.LLVM)   => Iterator[CpgPassBase]()
  case _                      => Iterator[CpgPassBase](new CfgCreationPass(cpg))
}
```

Only GHIDRA and LLVM are skipped. `rust2cpg` emits IF, WHILE, MATCH, BREAK and CONTINUE through the
shared `ControlStructureAstBuilder` helpers — a strict subset of the 14 kinds `CfgCreator` dispatches
on, so nothing degrades to `Cfg.empty`. **Rust `control_depends_on` and `data_flows_to` are non-empty
today.**

**But the output is lossy in a way a consumer cannot see, and nothing upstream checks it.**

| Loss | Evidence |
|---|---|
| **`?` is invisible to the CFG.** The try operator — the early-return mechanism of idiomatic Rust — is lowered to a plain CALL with no control structure, so every statement after a `?` is recorded as unconditionally reachable when it is control-dependent on the `?` succeeding | `RustVisitor.scala:1355-1359` |
| Labeled `break 'outer` / `continue 'outer` bind to the **innermost** loop; the `Lifetime` label is parsed and dropped | `RustVisitor.scala:1135,1142` vs. the label-carrying 4-arity builders at `ControlStructureAstBuilder.scala:306,348` |
| `for` / `if let` / `while let` conditions are **UNKNOWN** nodes, so CDG gets no true/false discrimination. The frontend admits it in-tree: *"NB: the condition is currently UNKNOWN. A more faithful lowering would be…"* | `RustVisitor.scala:1055-1060, 966, 1022` |
| No type recovery, so `calls` depends entirely on `methodFullName` | `RustCpgGenerator.scala` |
| **Zero CFG, CDG or dataflow tests.** All 35 test files are AST-level; `CfgAttributeTests.scala` is about `#[cfg(...)]` conditional-compilation attributes, not the control-flow graph. Compare c2cpg's 11 CFG/dataflow test files and jssrc2cpg's 9 | `12-source-anatomy-v4.0.627.md` §A6 |

**This is a stronger argument for building Rust first than doc 05's was, not a weaker one.** A zero
count is visible to a consumer; a wrong edge published at `static_analysis` precision is not. It also
changes how Rust must be built: **its oracle cannot be a diff against the engine**, which would encode
the `?` bug into the corpus. Rust needs a hand-built golden corpus from the language reference.

For completeness, the Go frontend was re-checked at the same tag and is **unchanged** from doc 05's
description: the `case _: BaseStmt => Seq(Ast())` fall-through is still at
`AstForStatementsCreator.scala:48`; `DeferStmt`, `GoStmt` and `SelectStmt` do not exist as node kinds
anywhere in the frontend; `LabeledStmt` is declared but never dispatched, so `gotoAst` is emitted with
no JUMP_TARGET for `withResolvedJumpToLabel()` to bind — **Go's gotos dangle**; and `fallthrough` is
still `// TODO`.

---

## 4. The reaching-definitions pass, sized

Doc 05 sized CFG+CDG. `10-reaching-defs-and-readswrites.md` sizes the other two families the same way.

**The algorithm.** A textbook forward may-analysis over a bit-vector domain: the flow graph is the
method CFG plus parameter pseudo-nodes, because parameters are not in the CFG
(`DataFlowProblem.scala:15-19`); a `Definition` is **an `Int` — a CFG node index**, not a variable
(`package.scala:4`); the transfer function is `gen(n) ∪ (x \ kill(n))` (`ReachingDefProblem.scala:169-171`);
meet is union; the worklist is seeded in reverse post-order and re-queues successors only when `out`
changed (`DataFlowSolver.scala:12-37`).

**The `--max-num-def` bound is narrower than doc 04 implied.** It bounds **Σ|gen(n)| — a count of
definition facts** (`ReachingDefPass.scala:44-48`), not definitions × nodes, and it is evaluated
*after* `gen` and `kill` have already been built as strict `val`s, so it protects the fixed-point loop
and the edge emission but **not** `killsForGens`, which is O(D × |calls| × |ast|) and is the part that
actually scans. Exceeding it drops *every* `REACHING_DEF` edge of that method, which is why the product
publishes `data_flows_to` as `partial` with the skipped method names.

**Two of its three precision-carrying parts are not dataflow at all.** `kill` is name and `code`
equality plus an AST scan for field accesses (`ReachingDefProblem.scala:220-293`), and `UsageAnalyzer`
re-filters the solution with four predicates of which `sameVariable` is literally
`nodeToString(use).contains(call.code)` — substring matching on frontend-normalised source text
(`DdgGenerator.scala:349`). **A Go port reproduces the solver in a day and spends its real budget
here**, and it faces a genuine design fork: copy the substring hack, or use real variable identity and
accept a different output.

**The fork is now decided, and so is the solver.** §7.2 **replaces** this pass rather than porting it:
the dense `in`/`out` is Θ(N²/64) words — 2.34 GiB on a single function at N = 10⁵ — which a design
with no caps and no subdivision cannot carry, while the engine only survives it by having the cap this
plan forbids. Sparse SSA-based def-use has the same published output and no quadratic. The sizing
below is retained because it prices the *work*, which is the same either way; what changes is that the
divergences become three **named** causes the oracle must account for separately, rather than one
aggregate difference (§7.5).

**What the product keeps is much less than the pass produces.** `data_flows_to` is declaration →
declaration over an `anchors` table, with every intermediate operator, literal and `METHOD_RETURN`
collapsed, **bounded at 8 levels** (`neo4jcsv.go:74-75`), the `variable` label discarded, and globals
folded into `reaching_def capture` because the export gives globals no marker. So a port does not need
`UsageAnalyzer`'s access-path aliasing to match the *published* output — but it does need it to match
the *edge set*, because the depth-8 walk traverses those edges transitively.

| Component | Scala anchor | Go estimate |
|---|---|---|
| Flow graph, RPO numbering, bit-vector worklist | 129 | 250–400 |
| gen/kill (incl. same-name kill, field-access kill, lone-identifier optimisation) | 351 | 300–500 |
| Edge emission, use filtering, the product's anchors + depth-8 projection | 428 (+374 product-side) | 500–800 |
| Driver, bound, bail-out reporting | 80 | 100–180 |
| **shared core** | **962** | **1,150–1,880** |
| Per-language def/use rules | — | 250–450 (Go, Java) · 300–550 (Python) · 350–650 (C/C++) · 400–700 (JS/TS/TSX, Rust) |

**The estimate excludes a scope/binding resolver, and that exclusion is the largest swing factor.**
The 962 Scala lines ride on a CPG that already supplies `REF` edges, `ARGUMENT` indices,
`TYPE_FULL_NAME` and `CLOSURE_BINDING`. A tree-sitter CST supplies none of it; today the product gets
resolution from SCIP indexers. Where a precise index covers a unit the resolver costs nothing; where it
does not, it is **400–900 Go lines per language**. §6 is what decides which case applies.

---

## 5. `reads` / `writes`: the family nobody had sized

This family comes from the dependence provider **alone** — no SCIP indexer sets a write role
(`14-store-counts-r3.md` Q5, six indexers measured), and syntax cannot resolve an assignment's target.
Doc 05 did not size it and neither did doc 04. It is sized here because a native engine must reproduce
it or the product loses a capability outright.

The mechanism is the engine's **operator target algebra**: every frontend lowers every write form into
an operator call whose argument 1 is the written operand — identifier with `REF`,
`fieldAccess(base, field)`, `indexAccess(container, idx)`, `indirection(ptr)`, destructuring via temps,
and chained assignment. The product walks that shape in `rw.go` (374 lines) and resolves the innermost
identifier to a declaration, falling back to `may_refer_to` with a syntactic name.

Four gaps are already measured (`10-round3-empirical.md` §3): Go `a, b = b, a` lowers only the first
target; Rust `(a, b) = (b, a)` keeps a `tupleLiteral` target; Java static fields and Go package globals
appear as `fieldAccess` on a type or package base rather than a `REF`'d identifier; Rust `static mut`
is unresolved.

A full enumeration of the assignment-family operators is **not determinable from the checkout** —
`Operators` is generated inside codepropertygraph 1.7.74, which is not in the clone and cannot be
fetched. Two independent in-tree lower bounds agree at **13–16 distinct write-carrying operators**.
Worth recording for whoever maintains `rw.go`: `semanticcpg/.../operatorextension/package.scala:19`
lists `Operators.postIncrement` twice, so `postDecrement` is missing from `allAssignmentTypes`;
`rw.go:25` lists it explicitly, so **the product is more correct than the library here**.

Estimate: a shared target-resolution walker of **250–400** Go lines plus per-language write-form
recognition of **200–550** each, **1,900–3,150** in total for six families. `rw.go`'s 374 lines are not
a fair anchor on their own, because they consume an already-lowered, already-resolved graph; over a CST
there is no lowering to operator calls at all, and the algebra the engine's frontends implement must be
written per language. **What is not reproducible without name resolution is the precise branch** —
`writes` as method → declaration. Syntax alone yields only `may_refer_to`, which is exactly what the
four measured gaps already are.

---

## 6. `calls` without the engine: measured, on the repository that raised the question

`14-store-counts-r3.md` answers this from two real stores, every figure scoped through the active
generation (3 in both) and its unit set.

| r3-3, generation 3 | sites | edges |
|---|---|---|
| tree-sitter `calls`, `syntax` precision | **555,588** | 363,750 |
| …resolved, in-file or via import | 75,751 (13.6%) | 51,012 |
| …ambiguous (≈8.96 candidates each) | 20,046 (3.6%) | 10,419 |
| …**unresolved — call through a value** | **459,791 (82.8%)** | 302,319 |
| engine `calls`, `static_analysis` | 217,881 | 115,940 |
| …**resolved to an in-repo definition** | 40,743 (18.7%) | 27,897 (24.1%) |
| …to an external/library stub | 177,138 | 88,043 |

The one precise unit that ran was **`profile:scip-python:QA/RobotTests`** — 17 Python files, 24.3 s,
1,099 `references` and **zero `calls` of any kind**, covering 645 of the repository's 555,588 call
sites (0.116%). Three claims in the program ledger are corrected by the store: that unit was scip-python, not
one of ten TypeScript units; the `CTX_PROVIDER_OUTPUT_INVALID` belongs to the **dependence** provider,
not a precise one; and **no Java precise unit exists in either store**.

**Projection.** Had every planned precise unit run, precise resolution would cover **4.87–5.25%** of
r3's call sites. The assumption — call sites uniformly distributed across the files of a language — is
stated and then *replaced by measurement*: uniform-by-files gives 4.85%, actual per-project call-site
density gives 5.25%, and they agree only because Java at 126 sites/file happens to be nearly as dense
as JavaScript at 140.

**Where coverage is lost, with weights:**

1. **JavaScript is 94.75% of all call sites** (526,393 across 3,748 files) and the repository has **no
   `tsconfig.json` anywhere**. scip-typescript measured **0 roles on a call site** (doc 08).
2. **Java**: 26,416 sites, a `pom.xml` exists, and scip-java still emits **no occurrence at all** at a
   call site. Its engine unit also failed, so all 26,416 are syntax-only today.
3. **Call through a value: 82.8% of all sites.** The callee is a member access or a value (`.map`,
   `.then`, `.click`). There is no name-bearing occurrence for SCIP to resolve.
4. **403 `.robot`/`.resource` and 546 `.jade` files** have neither an indexer nor a grammar.
5. **No indexer emits WriteAccess**, in any of the six — so `reads`/`writes` can never come from SCIP.
6. C/C++ macro call sites and Go build-tag-excluded files are real structural gaps but carry **zero
   numeric weight in this repository**.

And the language-server route, measured on this exact project: on `r3/app` **both** the pinned
TypeScript server and the faster candidate return **2 references**, cold and after a 30 s settle,
because with no `tsconfig.json` tsserver builds an inferred project and scopes references to the open
file's import closure — *"a real limitation on JS monorepos, but not one a swap fixes"*
(`19-language-server-and-indexer-matrix.md` §2).

**Conclusion: the engine's call graph is not replaceable by a precise index plus a language server on
this repository.** In the earlier version of this note that was read as a reason to leave `calls` on
the engine. It is the opposite: because nothing else can supply it, `calls` is precisely the family a
native engine **must** take, or the engine is never retired. §7.8 ports it in two phases — a name join
for the four frontends the engine gives no type recovery, then type recovery for the two that have it
— and every gate is stated on the in-repo-resolved subset, because reproducing an external stub
reproduces no capability.

---

## 7. The architecture, the algorithms, and the full-retirement plan

### 7.1 The pipeline

```
per file (unit of caching, invalidation and SCHEDULING)
  inside the structural provider's own worker subprocess, which already owns the tree-sitter CST
  and every native object behind it — only fact frames cross the wire, so the analysis runs THERE
    per function (unit of WORK and of MEMORY)
      normalise [per-language] → CFG → post-dominators → CDG
        → def/use [per-language] → SSA def-use → emit
per project (the one scope that is not per function)
  calls: precise index where a profile applies; otherwise the native linker of §7.8 phases 4–5
```

The unit of **work** is a function; the unit of **caching and scheduling** is a file. Memory per worker
is one file's CST plus one function's structures, so nothing in the analysis grows with repository
size — which is what makes "no caps, no time limits, no max-visited" a property rather than a policy.

**A correction to the earlier version of this note.** The CST is *not* simply "already built and
reusable": parsing runs in isolated worker subprocesses that own every native object, and nothing but
JSON fact frames crosses the wire. Running the analysis in the coordinator would re-parse every file.
It therefore runs **inside the parser worker**, inheriting its per-CPU worker count, its 256 MiB
per-worker reservation and its crash isolation (`19-algorithms-callgraph-and-memory.md` §2.2).

The engine's whole memory apparatus — per-unit heap caps, per-family resident allowances (C/C++ 2.6 GB,
Python 1.9 GB), the serialising concurrency setting, the single OOM retry, the subdivision path, six
failure classes reverse-engineered from a bounded stderr tail — has no native equivalent
(`04-requirements-and-engine-cost.md`). Nor does the import path: the engine hands the product
0.65–4.95 GB of Neo4j CSV, staged in a SQLite scratch database at 6.6× the export with a 256 MiB
cache, a pooled surface, an exclusive lock, a retirement rule and a paced reclaimer for its freed
gigabytes. A native engine emits facts in projection order, in process, one function at a time.

### 7.2 The algorithms, each decided

Every row is a decision, not a menu. The reasoning, the steel-manned alternative and the citations are
in `18-algorithms-dominance-and-dataflow.md` (per function) and
`19-algorithms-callgraph-and-memory.md` (per project and per run).

| question | decision | why this one |
|---|---|---|
| Dominators | **Cooper-Harvey-Kennedy iterative**, over a dense `int32` reverse-post-order, written in the repository | the crossover is irrelevant at the measured function-size distribution — p50 13, p99 122, max 387 body lines in this repository; p50 9 / p99 187 / max 669 in a large Go library; below ~10³ nodes allocation decides, and the near-linear implementations allocate per node |
| Post-dominators | the **same** CHK pass over a **reversed, exit-augmented** CFG | a separate near-linear implementation buys nothing at these sizes, and one pass means one code path to validate. The augmentation is mandatory: a unique synthetic EXIT, plus an edge from every strongly-connected component that cannot reach it, or an infinite loop's nodes silently have no post-dominator |
| Control dependence | **Ferrante-Ottenstein-Warren** via post-dominance frontiers on the exit-augmented CFG, **without** the ENTRY→EXIT edge | it is the textbook set. The engine computes an **approximation** in two provable places — the METHOD node's single successor fails the ≥2-predecessor filter, so nothing is control-dependent on entry, and the frontier walk truncates on a missing post-immediate-dominator. The product's `control_depends_on` is a **single-hop anchored join with no walk**, unlike the depth-8 `data_flows_to` walk, so a missing CDG edge is an unrecoverable missing fact, not a longer path |
| Reaching definitions / def-use | **replace, do not port**: sparse SSA-based def-use (Braun et al.), not the dense bit-vector worklist | the dense `in`/`out` is Θ(N²/64) words — 13.5 MiB at the largest function measured on this host and **2.33 GiB at N = 10⁵**, which "no caps" cannot carry. Braun's construction needs no dominance computation for def-use at all, so the port keeps **one** dominator computation per function, for control dependence |
| Interprocedural framework | **none**. No IFDS, no IDE | realizable-path precision is unobservable at a surface that publishes unlabelled depth-8 reachability, and the exploded supergraph reinstates the whole-program resident structure the memory rule forbids |
| Incrementality | **no incremental dataflow algorithm**; a changed function is recomputed from scratch, and the invalidation boundary is the **file's content hash** | a function's CFG is tens to hundreds of nodes; the bookkeeping costs more than the recomputation |
| In-flight adjacency | plain `int32` **compressed sparse row**, forward and reverse, no varint | it is the per-function structure, built and discarded inside one worker — a different object from the published per-generation adjacency of the graph-traversal record, which is unchanged |
| Bitsets | 64-bit words; dense `in`/`out` rows only where the dense formulation is used at all; sparse `gen`/`kill` | — |
| Allocation | one **reset slab arena per worker**, holding pointer-free typed backing arrays, with a 1 MiB release threshold | near-zero steady-state allocation rate per function; the release threshold is what stops the high-water mark falsifying §7.1's memory claim |
| `GOGC` / `GOMEMLIMIT` | **set neither** | the CST layer is cgo, so the dominant per-worker allocation is invisible to the Go runtime's limit; a runtime knob that cannot see the memory it is meant to bound is worse than none |
| Call graph — statically typed with hierarchies (Java) | **class-hierarchy analysis** | decided on **streamability**, not precision: its inputs are two tables an ordered merge join consumes, while rapid type analysis needs a reachability fixed point and variable-type analysis a global propagation graph — both whole-program resident state the memory posture forbids. Megamorphic fan-out lands in the existing ambiguous-candidate family, which the product already publishes with a candidate count |
| Call graph — Go, Rust, C/C++ | direct binding, plus the **signature-keyed address-taken × indirect-site cross-product** of rapid type analysis **without** its reachability fixed point, plus class-hierarchy analysis for interfaces and traits | most calls are direct; the interesting ones go through interfaces or function values. The gain is **unavailable** on the reference corpus, which contains no C, C++, Go or Rust file at all |
| Call graph — ECMAScript family, Python | **no whole-program points-to in any formulation.** Field-based resolution at index time; demand-driven resolution at read time | inclusion-based points-to has the right semantics and a forbidden budget; unification-based has the right budget and precision that collapses on these flow shapes. The achievable ceiling is **unavailable**; the honest anchor is that **18.7% of sites / 24.1% of edges** is the best any technique actually run on this repository reached |

**What the engine's own linkers are, named for the first time.** The static linker is none of the
academic algorithms — it is an exact full-name equality join. The dynamic linker **is**
class-hierarchy analysis, validating a method over subclasses, and cites a 2014 dispatch-hardening
paper in its own doc comment. Type recovery is flow-insensitive symbol-table type propagation over a
**fixed two iterations** — explicitly not a fixed point. So the measured 18.7% is class-hierarchy
analysis plus a name join plus two propagation rounds. Nothing in it is unreproducible.

### 7.3 Dependencies: the shared core adds none

The earlier version of this note pinned a graph library for Lengauer-Tarjan and, optionally, the Go
toolchain libraries. Under §7.2 the dominator core is written in the repository, so **the shared core
adds no module to `go.mod` at all**. A Go-family compiler-precision call graph would optionally add
`golang.org/x/tools` (BSD-3). The CST layer, cgo and the nine grammar registrations from eight pinned
grammar modules are already pinned; a native engine adds **no parser, no runtime, no helper binary and
no per-language toolchain**.

**The "verified negative" is requalified.** No permissively-licensed Go package exposes post-dominators
or a dominance frontier **in its public API** — but `go/ssa`'s `lift.go:99-106` computes a Cytron
dominance frontier over the recurrence at `:79-97`, unexported, in about 28 lines of BSD-3 source that
can be **ported rather than invented**. The must-write list is real but smaller than it looked.

Two library facts settled from the local module cache rather than inferred: running a generic dominator
implementation on reversed CFG edges to obtain post-dominators is **sound** — both entry points consult
exactly one directed-graph method, the successor relation — and the highest version of the Go toolchain
libraries readable on this host is **v0.49.0**, not the v0.50.0 this note and
`09-native-building-blocks.md` previously pinned from a network observation. Every library citation in
the new evidence files is read at v0.49.0 and says so.

Of nineteen candidates checked at live URLs, everything else remains reference-only: one name-resolution
library is archived with no C ABI and never computed control dependence; a graph-construction DSL is
dormant; an OCaml pattern-and-taint tool's community edition is intraprocedural, the same scope the
product already imports; two JVM code-property-graph projects reinstate the runtime profile being left;
one query engine's extractors are proprietary.

### 7.4 Memory, parallelism and coexistence

**The whole-run bound, with the arithmetic** (`19-algorithms-callgraph-and-memory.md` §3.2):

> `R_run = B_process + W × M_worker + A_link`
> = 1.00 GiB + 16 × 256 MiB + 0.0005 GiB ≈ **5.00 GiB** at one worker per CPU on a 16-core host.

**No term names file count, call-site count or repository bytes**, so the figure is **identical at 1×,
3× and 10×** the reference repository. Per function the structures are bounded by
**M_sparse(N) ≤ 96·N + 64 bytes** — 0.92 MiB at N = 10⁴, which covers the largest function measured
on this host (7,518 body lines) under any lines-to-nodes factor up to about 1.3, and 9.16 MiB at
N = 10⁵. The dense formulation the engine uses needs **2.34 GiB** at that same point, which is the
whole argument for replacing it. The definition count is bounded
**structurally**, not by a policy constant: a definition is a CFG node index, so D ≤ N. That is the
replacement for the engine's definition cap, whose price is dropping every reaching-definition edge of
an over-large method.

**The honest qualification, stated here rather than in a footnote.** `M_worker` is an **admission
estimate, not an enforced ceiling**: the process runner sums reservations against a budget and there is
no resource limit or cgroup anywhere behind it. A file whose parse tree overruns its reservation does
not fail — it makes `W × M_worker` an under-estimate. So `R_run ≈ 5.00 GiB` is an **admission bound**,
and what makes its drift visible is the per-unit disclosure of reservation, ceiling and observed peak
that the engine-memory record already requires. A hard per-worker limit is not the answer — a limit
that fails a file for its size is a cap, and the engine-memory record already rules that work whose
need exceeds the allocation still runs, whole. The answer is an **observed** estimate: the existing
250 ms tree sampler measures each worker's peak, the observed peak per file-size class feeds the next
admission and is persisted with the generation, a worker whose need exceeds the allocation is admitted
alone rather than refused, and every admission discloses the drift between reservation and peak.

**Coexistence, as a mechanism rather than an intention.** Each worker takes its reservation from the
standing allocation — the smaller of available memory less the base footprint and margin, and half of
available memory — **re-derived from the kernel's available-memory figure between files**. A worker
that does not fit waits at the head of the line and is admitted the moment one returns. Available
memory falling **is** the signal that the user's editor, browser or the agent driving the product is
competing; there is no count of children, no per-family count, no setting and nothing refused. This is
the in-process successor to the half-the-host rule the engine-memory record established for a JVM child.

**Scheduling.** The dispatch unit is the **file**, longest-first with work stealing — the classic
longest-processing-time bound is 4/3 − 1/(3m) of optimal. Per-function variance is handled by *order*,
not by a finer granularity: a per-function dispatch unit would force a shared CST lifetime across
workers.

**At 3× and 10× the reference repository** (13,222 files, 6,663 parsed, 555,588 call sites; largest
unit 4,984 files / 157.2 MB): resident memory is flat, and **disk breaks first** — the store scales
linearly, 1.36 GB → 13.64 GB, and then link-phase wall clock. The packed adjacency is 107 MiB at 10×.
**The paced reclaimer matters more at 10×, not less**: deleting the staging database removes a *source*
of freed gigabytes, but a retired 10×-scale generation is still about 13.6 GB, and freeing that in a
burst stalls every process on the host about a minute later — after the product has reported done.

### 7.5 The oracle

**It mostly exists.** The importer already derives an engine-id-independent semantic key per fact — the
label, its owning method's full name, its file, the operator it was lowered from, its target name, its
ordered byte ranges and, for a relation, both endpoints' published identities — streams the key set to
a sorted file and diffs two sets in one merge pass (`providers-dependence.md` §Refresh and delta). A
native engine is a **second producer feeding an existing key algebra**, not a new framework. This is
also why the deletion credit in §7.7 is not the whole package: **339 of those lines relocate rather
than die**, because the comparator is what every gate below is built on.

What remains is the corpus (pinned by commit in the benchmark task, as `00-synthesis.md` §8 directs)
and **the band**. Two engine runs over the same unmodified tree differ by about 0.01%, and
`providers-dependence.md` makes a claim of equality between two engine runs a defect. So the oracle
measures the band from two engine runs *first* and judges the native run against it; a diff inside the
band is not a finding. Thresholds are **per family**, because the families diverge for different
reasons — CDG for normalisation, reaching definitions for def/use, `reads`/`writes` for resolution.

**One refinement the algorithm work forces.** The sparse and dense formulations of def-use differ in
three *named* places: φ-depth consumption at the product's depth-8 projection (removable by resolving
φ-operands eagerly), the engine's name-based over-kill, and the substring match in its use filter that
over-connects. So the `data_flows_to` band is **not symmetric**, and the gate needs a **signed,
per-cause breakdown** rather than one absolute difference. An unsigned aggregate would let an
over-connection cancel a miss.

### 7.6 Precision

`internal/model/facts.go:94-98` offers `compiler`, `language_server`, `static_analysis`, `syntax`,
`heuristic`, and the type's own comment says precision names the origin of a fact. A control
dependence from a CFG and post-dominators, or a data dependence from def-use chains, is static analysis
whichever parser produced the tree; the engine's own frontends for four of the six invoked languages
are parsers with no type information behind them and publish at `static_analysis` today. The native
families therefore publish at **`static_analysis`** — no consumer-visible change of label — and what
differs between producers, endpoint resolution, is carried where it already lives: the endpoint's own
origin (`compiler` from a precise index, `static_analysis` from a name or hierarchy join) and the
candidate count on an ambiguous site. Doc 05 §0's proposal to label the native families `syntax` is
withdrawn: it labelled the toolchain rather than the analysis and would have regressed languages that
carry `static_analysis` today. The retirement gate's parity condition is what proves the label is earned.

### 7.7 Effort

| Component | Go lines | Weeks (parallel implementers) | Dominant risk |
|---|---|---|---|
| CFG + post-dominators + CDG core | 1,200–1,800 | 2–3 | exceptional-edge semantics, and the frontier port is 28 readable BSD-3 lines rather than an invention |
| Sparse SSA def-use core | 1,150–1,880 | 2–3 | the engine's use filter matches normalised text a CST does not have; the band is asymmetric |
| `reads`/`writes`, shared + six families | 1,900–3,150 | 3–4 | four measured gaps; no textbook and no other tool to copy from |
| Per-language normalise + def/use | 370–920 each | 1–2 each | the per-language lowering anchors grew unevenly across 527 releases — that spread *is* the risk |
| **Call-graph linking, shared** | **600–1,110** | **1–2** | a full name is a frontend product, not a language property: a mismatch produces a *wrong* edge, not a missing one |
| **Type recovery, shared** | **2,700–4,690** | **3–5** | a heuristic fixed point; "within the band" can mean faithfully reproducing wrong edges |
| **Type recovery / linking, per language** (ECMAScript 1,900–2,900 · Python 1,000–1,700 · Java 200–400 · C/C++, Go, Rust 150–350 each) | **3,550–6,050** | 1–3 each | the builtin table is 1,094 lines of transcription, not algorithm |
| Scope/binding resolver where no precise index covers a unit | 0, or 400–900 per language | 1–2 each | **the swing factor**; §6 gives r3 ≤5.25% precise coverage |
| Differential harness + corpora | 2,500–4,500 + 400–900 per language | 3–4 | larger than the implementation; the engine's own control-flow and dataflow tests are 9,901 lines over five frontends and **zero for Rust** |
| **Credit: importer and staging deleted** | **−4,535** | — | 4,874 lines of package source less the 339-line comparator that relocates to the oracle; 99 lines of Go fixture source go with it |

**"AI does it in 10 minutes."** What that gets right: the algorithms are textbook with an open,
Apache-2.0 reference; the generic core really is small; the product already ships its grammars in the
right extension point; and the facts are already proven file-local. What it is silent on: **per-language
lowering semantics**, where every reference implementation is measurably wrong; **the differential
corpus**, plausibly larger than the implementation and unable to assert equality; and **the precision
claim**, which is a promise to users. The algorithm is a weekend. The vocabulary mapping, the oracle and
the precision decision are the project.

### 7.8 The phases, and the retirement gate

Nothing below starts before the MVP ships. The full path, with each phase's gate, its residue and its
back-out risk, is `15-requirements-audit.md` Part 2; the summary:

| phase | what becomes native | what still needs the engine at the end | the measured condition that ends it |
|---|---|---|---|
| **0 — shared core, Rust first** | CFG, post-dominators, CDG, def-use, emission; the Rust mapping. `control_depends_on` and `data_flows_to` for Rust | `reads`/`writes` and `calls` for Rust; all four families for the other eight languages | The native key set reproduces **100%** of a **hand-built golden corpus authored from the language reference**, and emits nothing outside it. Equality is right here and only here, because the corpus is authored rather than observed. One golden case is mandatory: the try operator must yield a control dependence, which the engine does not emit |
| **1 — Go, the calibration gate** | the Go mapping; `control_depends_on` and `data_flows_to` for Go | the same, less Rust and Go | Per family, the symmetric difference against the engine is **≤ the band measured first from two engine runs on that same corpus**. Second required output: **differential-test cost per language**, the number nobody has, from which every later phase is priced. Corpus units must be whole packages — a file-only Go parse invents 960 spurious reaching-definition edges from the frontend's synthetic package initialiser |
| **2 — ECMAScript family, and the shared write algebra** | all four dependence families for JavaScript, TypeScript, TSX, Go and Rust | resolved `calls` everywhere no precise profile applies; all four families for Python, Java, C/C++ | the per-family band gate, **and** the same unit that costs the engine 3:38 and a 5.44 GB process-tree peak completes natively with a lower wall *and* a lower tree-summed peak, both on the same 250 ms sampler |
| **3 — Python, Java, C/C++ dependence** | all four dependence families for all nine advertised languages | **resolved `calls` only**, where no precise profile applies | the per-family band gate per language, **and** a store query over a fresh index returns **zero** dependence-provider rows of the four dependence kinds at the active generation — which is what licenses deleting the import path |
| **4 — static call linking, the four frontends with no type recovery** | resolved `calls` for C/C++, Go, Rust **and Java** | resolved `calls` for the ECMAScript family and Python, where no precise profile applies | the native `calls` key set matches the engine's **in-repo-resolved** subset within the per-family band, on pinned corpora — **not** on the reference repository, which contains no C, Go or Rust unit at all |
| **5 — type recovery, the two frontends that have it** | resolved `calls` for all nine languages. Order: ECMAScript family, then Python | **nothing** | per language: the in-repo-resolved band gate, **and** on a fresh index of the reference repository, in-repo-resolved `calls` **≥ 40,743 sites / 27,897 edges** while the ambiguous syntax-tier population does not rise above its measured 20,046 sites / 179,626 candidate edges |
| **6 — the retirement gate** | nothing is ported; the engine, its backend package and its import path are deleted | **nothing** | the five conditions below, simultaneously |

**Java moved.** It sits in phase 4, not phase 5, because its type-recovery override is gated on a flag
the product never passes: the engine gives Java a name join too. Its phase-4 gate cannot be "reproduce
the engine's type-recovered `calls`", because there are none — like Rust, Java's gate is an **authored
corpus and a capability gain**, not a parity diff. Java is the sharpest residue in the meantime: a
project file exists, the precise indexer still emits no occurrence at a call site, and all 26,416 of
its call sites are syntax-only today.

**The retirement gate — all five at once.**

1. **Parity.** For each of the nine advertised languages and each of the five published families, the
   native key set is within the per-family band measured from two engine runs on that language's
   pinned corpus. For `calls`, the comparison is the in-repo-resolved subset.
2. **No capability depends on it.** A fresh index of the reference repository with the dependence
   provider absent publishes every capability at the same state as one with it present.
3. **No resolution regression.** In-repo-resolved `calls` is **≥ 40,743 sites and ≥ 27,897 edges**, and
   the tree-sitter ambiguity population has not grown.
4. **Coexistence.** The whole index completes with a tree-summed peak below the standing admission
   allocation, **no single reservation larger than one worker's**, and the dependence phase's share of
   the index wall below its measured **74%**.
5. **No consumer remains.** The tool lock entry, the provider's backend and its capability mapping are
   the only references left, and all are deleted in the same change — no dead consumer, no unreachable
   path, no configuration key nothing reads.

**Trade-off accepted.** Two producers for one relation kind during each gate. Two producers behind a *measured, terminating* gate is a verification state, not a compatibility layer
— it is the absence of a terminating gate that would turn it into one, which is the defect this
revision removes.

### 7.9 The requirements, audited

`15-requirements-audit.md` decomposes the product owner's scope into **24 atomic requirements** and
audits the previous version of this note against each, citing the line. That audit returned 4 `meets`,
12 `partially meets`, 5 `silent` and 3 `contradicts`. The three contradictions were call-graph linking,
type-recovery heuristics and "rip it out" — one sizing omission read twice, plus a refusal to make the
retirement decision. §2 sizes the first two and §7.8 decides the third.

The five silences are answered as follows, and each answer is a change to this note rather than a note
about one:

- **The program dependence graph as a surface.** Control dependence and data dependence are both
  staged, both in the default traversal set, and keyed by the same entity pair — so the PDG is a
  **join in the projection**, not engine work (`17-passes-structural-and-cfg.md` §10). Leaving it to
  the caller to compose by filtering kinds is exactly the tuning the owner's scope forbids.
- **Tuning nothing over the MCP surface.** `max_depth`, `max_visited`, `max_edges` and `max_bytes` are
  **per-page work budgets with a continuation cursor**, every one defaulting to unlimited, and any
  reduction is disclosed on the answer. They are pagination, not analysis caps; removing them would
  remove resumability. The analysis itself has no cap at all, and §7.4 states what replaces the
  engine's definition bound.
- **Blazing-fast speed, proven.** This note still publishes no native throughput target, because none
  can be measured before something is built. What it now does publish is the **instrument and the
  comparison**: phase 2's gate is a lower wall *and* a lower tree-summed peak than the engine on the
  same unit, on the same 250 ms sampler that produced the engine's figures.
- **Coexistence with the user's other processes and with the agent driving the product.** §7.4 names
  the mechanism.
- **Helping agents find bugs and plan.** The families are named against the tools they feed throughout
  §2 and `15-requirements-audit.md` Part 1: `codectx_callers`, `codectx_callees`,
  `codectx_dependency_path` and `codectx_impact` are the consumers, and the PDG join above is the one
  new capability the port adds rather than preserves.

One `partially meets` is recorded as an open gap rather than closed: **nineteen candidate building
blocks were surveyed for availability, none for superiority.** §7.2 now chooses each algorithm against
a named alternative with a cited reason, which closes it for the algorithms; it remains open for the
store, which this plan assumes rather than selects.

---

## 8. Dead ends, recorded as dead ends

`06-dead-ends.md` carries the full list with sources. In brief: incremental reuse of the engine (no
API; maintainer "No, it doesn't right now"; `--overlaysonly` corrupts graphs); splicing per-file engine
graphs (TypeScript produces 19% *wrong* `METHOD_FULL_NAME` call edges, which aliasing cannot repair);
cheaper engine exports (`--repr=pdg` unimplemented for CSV, `joern-slice` cannot emit CDG); SCIP alone
for `calls` (identical roles for calls and references, all six indexers); SCIP role bits for
`reads`/`writes` (no indexer sets WriteAccess); `stack-graphs`/`tree-sitter-graph` for CFG (neither has
one); Fraunhofer `cpg` as backend (same JVM profile); CodeQL (heavier, needs a build, extractors not
open); lowering `--max-num-def` (measured to cost completeness for nothing); and splitting a
JavaScript/TypeScript unit for cost (46% of resolved calls die).

---

## Verification ledger

**Measured on this host.** Every Scala line count (`find … -exec cat {} + | wc -l` for aggregates,
explicit `wc -l` for itemized rows — never `xargs wc -l | tail -1`, which reports one chunk); every
store figure (`sqlite3 -readonly`, scoped through `generation_units` at the active generation, one
conditional-aggregation pass per store, no index built); the product-side Go line counts and `go.mod`
contents; the function-size distributions in §7.2 (this repository 3,272 functions, p50 13 / p99 122 /
max 387 body lines; a large Go library 4,153 functions, p50 9 / p99 187 / max 669; a generated driver
3,609 functions, p99.9 2,499 / max 7,518).

**Read directly at `v4.0.627`.** `ControlFlow.scala`, `CfgCreator.scala`, `Cfg.scala`,
`CfgCreationPass.scala`, the `cfgdominator/` and `codepencegraph/` packages, the whole
`passes/reachingdef/` package, all ten `passes/base/` files, all three `passes/typerelations/` files,
`OssDataFlow.scala`, `JoernExport.scala`, `JoernParse.scala`, `DefaultOverlays.scala`,
`CpgBasedTool.scala`, the four layer definitions, the four callgraph linkers, `LinkingUtil.scala`,
`XTypeRecovery` and every file under `passes/frontend/` and `frontendspecific/`, the six product
`*CpgGenerator.scala` drivers, `EvalTypeAccessors.scala`, `VariableScopeManager.scala`,
`RustVisitor.scala`, `ControlStructureAstBuilder.scala`, `gosrc2cpg`'s `AstForStatementsCreator.scala`
and `ParserAst.scala`, every `build.sbt`, `MODULE.bazel`, `maven_install.json`. Product side:
`neo4jcsv/{scratch,rw,emit,keys,neo4jcsv}.go`, `treesitter/lang/lang.go`, `treesitter/{provider,pool}.go`
and its worker, `process/runner.go`, `config/machine.go`, `graph/{graph,traverse,cost}.go`,
`mcpserver/{registry,limits}.go`, `model/facts.go`, `paced/reclaim.go`, `docs/providers-dependence.md`,
`docs/research/00, 04, 05, 08, 10, 11, 14, 15, 17, 19`, ADR-0001, ADR-0005, ADR-0009, ADR-0010.

**Read in the local Go module cache** (read-only source; nothing was built, resolved, benchmarked or
run): `gonum v0.17.0` `graph/flow/{control_flow_lt,control_flow_slt,interval,doc,control_flow_bench_test}.go`
and `graph/graph.go`; `x/tools v0.49.0` `go/ssa/{dom,lift}.go` and `go/callgraph/{cha,rta,vta}`; the
Go 1.27.1 runtime and `math/bits` sources cited in `18-algorithms-dominance-and-dataflow.md`.

**Verified over the network.** The nineteen building-block rows of `09-native-building-blocks.md`, each
fetched for licence and last activity; the paper, venue and year of every algorithm citation in
`18-` and `19-`, except where recorded as unavailable below. Reading a public page to verify a citation
is not a download; no file, package, repository or binary was fetched, cloned or installed.

**Not determinable under the no-download rule, and not estimated.** flatgraph and codepropertygraph
line counts (external sbt dependencies, no generated node source in the tree); the flatgraph version
the *sbt* build resolves (only the bazel lock's 0.1.34 is visible); the full `Operators` enumeration;
the complete set of Rust node kinds (`RustNodeSyntax.scala` is fetched at build time); whether the Go
toolchain libraries' call-graph packages changed between v0.49.0 and v0.50.0.

**Not determinable without running something, and recorded as unavailable rather than estimated.**
The fixed-point cost of the reaching-definitions pass on real inputs — the bound is on Σ|gen(n)| but
the hot spot runs before it. The split of the engine's 18.7% in-repo-resolved bucket between the hint
call linker and the bare-name linker: the experiment is one export with the bare-name linker removed,
diffed on the CALL edge set, and it needs an engine run, which this research is forbidden to make.
CFG nodes per body line, which is what would turn §7.4's bound in N into a bound in source lines; the
measurement is a count of CFG nodes per method over a store. The published crossover tables for the
near-linear dominator algorithms: the two papers' PDFs 404 at every mirror cited on this host, and one
abstract was verified in place of its tables. The achievable call-resolution ceiling for the dynamic
languages — 18.7% of sites / 24.1% of edges is the best any technique **actually run** on this
repository reached, and it is quoted as a measurement, never as a limit. The size at which a parse
tree overruns its worker's reservation, which is what §7.4's observed-reservation loop measures first.

**Previously weak, now settled.** The inference that a generic dominator implementation can be run on
reversed CFG edges to obtain post-dominators is **verified from source**: both entry points consult
exactly one directed-graph method, the successor relation, so a reversed view is sound — with a unique
synthetic exit and an augmentation edge from every strongly-connected component that cannot reach it.

**Still weak or single-sample.** The per-language Go estimates in §2, §4, §5 and §7.7 are extrapolations
from the measured Scala anchors beside them, with the anchor spread recorded in §1. §7.4's whole-run
figure is an **admission bound**, not an enforced ceiling, for the reason stated there.

*Sources are inline. Engine citations pinned to `v4.0.627`. Raw evidence in the sibling files `00`–`19`
of this directory. Research date 2026-09-16.*
