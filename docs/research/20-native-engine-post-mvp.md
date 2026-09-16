
# A native dependence engine: what a port of the engine's logic to Go would actually be, sized from its source

Research note for the codectx product owner, extending `04-joern-cost-anatomy.md` (the engine's
architecture and cheaper routes) and `05-cfg-cdg-from-treesitter.md` (the CFG+CDG sizing). Research
date 2026-09-16.

**Version discipline.** Every engine citation in this note is pinned to **`v4.0.627`**
(`4bb889d96ce972e2ded50d0d5765c514c1a032cf`, 2026-09-11), the version the product actually runs — not
to `v4.0.100`, which docs 04 and 05 cited and which is 527 patch releases behind. That difference is
load-bearing: one of doc 05's two structural findings does not survive the re-check. The engine was
cloned shallow outside the product tree; that clone is the one download this research made. Nothing was
built and the engine was never run. Store figures come from `sqlite3 -readonly` on two real run
artifacts on this host. Raw evidence, including each sub-investigation's complete findings, is in the
files `00`–`14` under `raw/native-engine/`, cited by name throughout.

**Standing under the direction ruling.** `00-synthesis.md` §8 (2026-09-13, user-adopted) makes
engine-backed `dependence` the MVP backend and names the pinned benchmark corpora as the differential
oracle for *any future native engine*. This note is that future, scoped and costed now. Nothing in it
is to be started before the MVP ships, and nothing in it reverses the ruling.

---

## 0. Bottom line

Three findings decide the shape, and none of them is a line count.

1. **The product consumes the output of about 2,833 of the engine's 123,641 main Scala lines.** This is
   not a rewrite of the engine and should never be described as one. 38.6% of the engine is frontends
   the product never invokes; most of the remainder is a schema, a graph store, a query DSL, a console
   and a type-recovery system whose purpose is to make `calls` resolvable — and `calls` is the one fact
   family a native dependence engine would not take.
2. **The facts a native engine would produce are already proven file-local, by the engine itself.**
   Run on one file, the engine reproduces 100% of CDG and REACHING_DEF for Python, TypeScript, C and
   Java (`05-file-locality.md`). Subdividing a project keeps CDG 99.7% and REACHING_DEF 99.9% while
   resolved calls collapse to 46% (`03-parity-and-oracle.md`). The same split appears from both
   directions: the intraprocedural families are per-function; `calls` is not.
3. **`calls` is not replaceable by SCIP + LSP on the repository that motivated the question.** Had every
   planned precise unit run on r3, precise resolution would cover **4.87–5.25%** of its call sites
   (`14-store-counts-r3.md`). That is an argument for *keeping* the engine's call graph, not against a
   native dependence engine — and it is why the two are separated in the design.

The recommendation (§7) is a phased post-MVP plan, not a replacement: build the shared core once, ship
it first where the engine's output is least trustworthy, and buy the one number nobody has — the
differential-test cost per language — before committing to nine.

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

Full list with sources: `13-corrections.md`.

---

## 2. Source anatomy: what a port takes, and what it leaves

`12-source-anatomy-v4.0.627.md` carries the complete tables. The shape:

**123,641 main Scala lines, 149,744 test lines.** The CPG schema (codepropertygraph 1.7.74,
`build.sbt:5`) and the graph store (flatgraph) are **external sbt dependencies and are not in the
clone**; their line counts cannot be produced under the no-download rule and are deliberately not
estimated. `build.sbt` never names flatgraph — it arrives transitively.

The passes the product's four fact families actually need:

| | main lines | fact family |
|---|---|---|
| `x2cpg/.../passes/controlflow/` (12 files: CFG creation, dominators, dominance frontier, CDG) | 1,284 | `control_depends_on` |
| `dataflowengineoss/.../passes/reachingdef/` (7 files) | 962 | `data_flows_to` |
| callgraph linkers (`DynamicCallLinker` 226, `StaticCallLinker` 41, `MethodRefLinker` 30) | 297 | `calls` |
| `ContainsEdgePass` 50, `MethodStubCreator` 178, `MethodDecoratorPass` 62 | 290 | attribution and `calls` targets |
| **subtotal the product consumes** | **2,833** | |

Everything else is either dead weight or serves `calls` in dynamic languages:

- **Type recovery: 5,413 lines** (`XTypeRecovery` 1,331 plus `frontendspecific/` 4,082, of which
  `GlobalBuiltins.scala` alone is 1,094). It is not one of the four overlay layers; it runs from each
  generator's `applyPostProcessingPasses`. **Only JS/TS, Python and Java override it.** `CCpgGenerator`,
  `GoCpgGenerator` and `RustCpgGenerator` do not, so for C/C++, Go and Rust the engine's `calls` is
  whatever `StaticCallLinker` resolves from `methodFullName`. This is a material fact about what the
  product is currently paying for and getting.
- **Six passes whose output the importer never stages** (`FileCreationPass`, `NamespaceCreator`,
  `TypeEvalPass`, `TypeHierarchyPass`, `AliasLinkerPass`, `NaiveCallLinker` — the last not even wired
  into its layer): 218 lines.
- **Eight frontends the product never invokes** (abap, csharp, ghidra, jimple, kotlin, php, ruby,
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
at all**, because the product already pins nine tree-sitter grammars and the cgo binding
(`01-product-anchors.md`).

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

**Conclusion: the engine's call graph is not replaceable by SCIP + LSP on this repository.** That is
why the architecture in §7 does not ask a native engine to produce `calls`.

---

## 7. The architecture, and the phased plan

### 7.1 The pipeline

```
per file (unit of caching and invalidation)  tree-sitter CST — already built by the structural tier
  per function (unit of WORK and of MEMORY)  normalise [per-language] → CFG → post-dominators
                                             → CDG → def/use [per-language] → reaching defs → emit
calls                                        unchanged: SCIP where a profile applies, tree-sitter otherwise
```

The unit of work is a **function**; the unit of caching is a **file**. Memory per worker is one file's
CST plus one function's CFG and bitsets, so nothing in the design grows with repository size — which
is what makes "no caps, no subdivision" (`00-synthesis.md` §8) a property rather than a policy. The
engine's whole memory apparatus — per-unit heap caps, per-family resident allowances (C/C++ 2.6 GB,
Python 1.9 GB), `max_concurrent_heavy_analyzers = 1` serialisation, the single OOM retry, the
subdivision path, six failure classes reverse-engineered from a bounded stderr tail — has no native
equivalent (`04-requirements-and-engine-cost.md`).

Nor does the import path. The engine hands the product 0.65–4.95 GB of Neo4j CSV, which is staged in a
SQLite scratch database at 6.6× the export with a 256 MiB cache, a pooled surface, an exclusive lock, a
retirement rule and a paced reclaimer for its freed gigabytes. A native engine emits facts in
projection order, in process, one function at a time. **That deletes 3,446 non-test and 1,428 test Go
lines** (`internal/provider/dependence/neo4jcsv`), which belongs on the credit side of any estimate.

### 7.2 Dependencies

Two additions, both permissively licensed, both pinnable, neither reaching the network at runtime
(`09-native-building-blocks.md`): `gonum.org/v1/gonum/graph/flow` v0.17.0 (BSD-3, Lengauer-Tarjan over
any `graph.Directed`, so one dependency serves all six families) and, optionally and for the Go family
only, `golang.org/x/tools` v0.50.0 (BSD-3, `go/ssa` + `go/callgraph`). The CST layer, cgo and all nine
grammars are already pinned.

**The verified negative that sizes the build: no permissively-licensed Go package exposes
post-dominators or a dominance frontier.** `go/ssa` and staticcheck's `ir` stop at the forward
dominator tree; gonum stops at `DominatorOf`/`DominatedBy`; the one Go package computing a frontier is
GPL-3.0 and single-author v0.0.0. So the must-write list is exactly **post-dominator tree, dominance
frontier, Ferrante control dependence** — on top of gonum's dominator core.

Of nineteen candidates checked at live URLs, everything else is reference-only: `stack-graphs` is
archived (2025-09-09) with no C ABI and gives name resolution, never control dependence;
`tree-sitter-graph` is a DSL dormant 21 months; Semgrep is OCaml and its CE taint is intraprocedural,
the same scope the product already imports; Fraunhofer `cpg` and flatgraph are Apache-2.0 but JVM and
reinstate the profile being left; CodeQL's extractors are proprietary.

### 7.3 The oracle

**It mostly exists.** The importer already derives an engine-id-independent semantic key per fact — the
label, its owning method's full name, its file, the operator it was lowered from, its target name, its
ordered byte ranges and, for a relation, both endpoints' published identities — streams the key set to
a sorted file and diffs two sets in one merge pass (`providers-dependence.md` §Refresh and delta). A
native engine is a **second producer feeding an existing key algebra**, not a new framework.

What remains is the corpus (pinned by commit in Task 21, as §8 already directs) and **the band**. Two
engine runs over the same unmodified tree differ by about 0.01% (`10-round3-empirical.md` §9b), and
`providers-dependence.md` makes a claim of equality between two engine runs a defect. So the oracle
measures the band from two engine runs *first* and judges the native run against it; a diff inside the
band is not a finding. Thresholds should be per family, because the families diverge for different
reasons — CDG for normalisation, REACHING_DEF for def/use, `reads`/`writes` for resolution — and one
aggregate number would hide all three.

### 7.4 Precision

`internal/model/facts.go:94-98` offers `compiler`, `language_server`, `static_analysis`, `syntax`,
`heuristic`. The engine stamps `static_analysis`; the honest label for a CST-derived CFG/CDG/def-use
with heuristically resolved endpoints is **`syntax`** (doc 05 §0). This is a product decision about
what the product promises, not an engineering one, and it is the largest non-engineering cost in the
plan. The mechanism for carrying two sources at two precisions already exists — evidence `Detail`
strings are built exactly this way today.

### 7.5 Effort

| Component | Go lines | Weeks (one lead, parallel implementers) | Dominant risk |
|---|---|---|---|
| CFG + post-dominators + CDG core | 1,200–1,800 | 2–3 | exceptional-edge semantics; every reference surveyed admits it gets these wrong |
| Reaching definitions core | 1,150–1,880 | 2–3 | `UsageAnalyzer` matches normalised text a CST does not have |
| `reads`/`writes`, shared + six families | 1,900–3,150 | 3–4 | four measured gaps; no textbook and no other tool to copy from |
| Per-language normalise + def/use | 370–920 each | 1–2 each | the Joern anchor grew unevenly; Fraunhofer's did not — that spread *is* the risk |
| Scope/binding resolver where no precise index covers a unit | 0, or 400–900 per language | 1–2 each | **the swing factor**; §6 gives r3 ≤5.25% precise coverage |
| Differential harness + corpora | 2,500–4,500 + 400–900 per language | 3–4 | larger than the implementation; the engine's own dataflow tests are 7,342 lines over five families and **zero for Rust** |
| **Credit: `neo4jcsv` importer and staging deleted** | **−4,874** | — | — |

**"AI does it in 10 minutes."** What that gets right: the algorithms are textbook with an open,
Apache-2.0 reference; the generic core really is small (the engine, Semgrep and Fraunhofer converged at
544–1,540 lines); the product already ships nine pinned grammars in the right extension point; and the
facts are already proven file-local. What it is silent on: **per-language lowering semantics**, where
every reference implementation is measurably wrong; **the differential corpus**, plausibly larger than
the implementation and unable to assert equality; and **the precision claim**, which is a promise to
users. The algorithm is a weekend. The vocabulary mapping, the oracle and the precision decision are
the project.

### 7.6 The phases

Nothing below starts before the MVP ships.

**Phase 0 — the shared core, with Rust as its first consumer.** CFG, post-dominators, CDG, reaching
definitions, emission (§4, §7.2). Rust first not because the engine gives nothing — §3 shows it gives
something — but because what it gives is wrong on the dominant construct and validated by nothing.
Rust's oracle is therefore a **hand-built golden corpus from the language reference**, never a diff
against the engine.

**Phase 1 — Go as the calibration gate.** Implement the Go mapping and diff it against the engine on a
pinned corpus, band-bounded. Go has a real upstream oracle (11 CFG/dataflow test files, 1,066 dataflow
test lines), its file-only fidelity is 98.3/98.5% with a *known* cause (the synthetic per-package
`<clinit>`, which a real package scope does not inherit), and its frontend is the weakest of the six,
so the diff is maximally informative. **This phase buys the number nobody has: differential-test cost
per language**, from which every later phase is priced. Doc 05 §6 proposed exactly this gate; it
remains right, and it is now phase 1 rather than the whole plan.

**Phase 2 and after — by measured engine cost and crash risk, JavaScript first.** JavaScript wins on
every axis simultaneously: the dependence phase is **74% of a 31:38 r3 run and `pkg:javascript:app`
alone is 64%** (two 3:20 parse attempts to a deterministic linker crash on legal JavaScript, then nine
serialised children); a two-file reproduction exists; its file-only CDG and REACHING_DEF fidelity is
**100%**; it has a real upstream oracle (709 dataflow test lines); and it is **94.75% of r3's call
sites**, where both SCIP and LSP measured near-zero. Then Python, Java and C/C++ by cost.

**Afterwards**, the engine's only remaining role is `calls` for units no precise indexer covers —
already a documented fallback. Whether to keep it at all is a separate, later decision that this plan
does not make.

**Trade-off accepted.** Two sources for one relation kind at two precisions during each gate, and a
consumer-visible downgrade from `static_analysis` to `syntax` for languages that have the former today.
What it avoids is committing nine grammars' worth of lowering rules on the strength of an estimate
rather than a measurement.

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
contents.

**Read directly at `v4.0.627`.** `ControlFlow.scala`, `CfgCreator.scala`, `Cfg.scala`,
`CfgCreationPass.scala`, the `cfgdominator/` and `codepencegraph/` packages, the whole
`passes/reachingdef/` package, `OssDataFlow.scala`, `JoernExport.scala`, `JoernParse.scala`,
`DefaultOverlays.scala`, `CpgBasedTool.scala`, the four layer definitions, the callgraph linkers,
`XTypeRecovery` and `frontendspecific/`, `RustVisitor.scala`, `ControlStructureAstBuilder.scala`,
`gosrc2cpg`'s `AstForStatementsCreator.scala` and `ParserAst.scala`, every `build.sbt`,
`MODULE.bazel`, `maven_install.json`. Product side: `neo4jcsv/{scratch,rw,emit,neo4jcsv}.go`,
`treesitter/lang/lang.go`, `model/facts.go`, `docs/providers-dependence.md`, `docs/research/00, 04, 05,
08, 10, 11, 14, 19`.

**Verified over the network.** The nineteen building-block rows of `09-native-building-blocks.md`,
each fetched for licence and last activity.

**Not determinable under the no-download rule, and not estimated.** flatgraph and codepropertygraph
line counts (external sbt dependencies, no generated node source in the tree); the flatgraph version
the *sbt* build resolves (only the bazel lock's 0.1.34 is visible); the full `Operators` enumeration;
the complete set of Rust node kinds (`RustNodeSyntax.scala` is fetched at build time).

**Not determinable from source reading.** The fixed-point cost of `ReachingDefPass` on real inputs —
the bound is on Σ|gen(n)| but the hot spot `killsForGens` runs before it; determining this needs
profiling a run, which this research did not do.

**Weak or single-sample.** Sub-agent C's inference that gonum's `Dominators` can be run on reversed CFG
edges to obtain post-dominators — it follows from the generic `graph.Directed` signature but is not a
documented feature. The per-language Go estimates in §4 and §5 are extrapolations from the measured
Scala anchors in the adjacent column, with the anchor spread recorded in §1.

*Sources are inline. Engine citations pinned to `v4.0.627`. Raw evidence in the sibling files `00`–`14`
of this directory. Research date 2026-09-16.*
