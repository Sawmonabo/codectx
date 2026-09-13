# CFG + post-dominators + CDG over tree-sitter: bounded pass, or nine compilers?

*Answering a reviewer's challenge to doc `04-joern-cost-anatomy.md` §4.5 item 4 ("Reimplement
`control_depends_on` on tree-sitter").*

Research date: 2026-09-13. Joern citations pinned to `v4.0.100`. Semgrep pinned to
`0516c0f23a3dceac5c8f5ff3fecd402af4450182` (2026-09-10). Fraunhofer CPG pinned to
`100564b77b79082e52d99e5abaa540254750f68a` (2026-09-09). LLVM pinned to `llvmorg-20.1.0`.
CodeQL figures are from `github/codeql` at HEAD via the contents API; the recursive tree API
reported `truncated: true` for that repo, so aggregate CodeQL byte counts below are **lower
bounds**, and single-file sizes were re-fetched directly. Byte counts come from the Git tree
API; line counts from the fetched files. Nothing here was run — this is a source reading.

---

## Bottom line

**The reviewer is right about the general problem and wrong about this one.** A *correct*
intraprocedural CFG for nine languages is a substantial per-language compiler effort — CodeQL and
Clang prove it. But codectx does not need a correct CFG. It needs to reproduce a specific,
deliberately coarse, partly broken artifact that Joern already publishes, consumed through a
projection that discards almost all of the resolution where the difficulty lives. Those are
different tasks by roughly an order of magnitude.

Three findings decide it:

1. **codectx's `control_depends_on` is not a statement-level fact.** It is method-target →
   method-target, derived only from CDG edges whose *both* endpoints are resolved call sites.
2. **Joern's CFG is one generic 649-line pass shared by every frontend.** The per-language work
   lives in each frontend's AST normalizer, and its CFG-relevant portion is 270–920 lines per
   language.
3. **Joern has no Rust frontend.** For one of codectx's nine languages, `control_depends_on`
   from Joern is zero today and option (e) does not change that.

Recommendation in §6. The tradeoff is not "cheap vs. expensive" — it is "a bounded fact codectx
owns and can invalidate per file, at a precision it must honestly downgrade" against "a fact
codectx rents, at `static_analysis` precision, that it cannot get for Rust at all."

---

## 0. The premise correction: what codectx actually publishes

`docs/providers-joern.md` is explicit about the projection:

> | `CDG` edge between two anchored sites | `control_depends_on` dependent target → controlling target |

and defines *anchored*: "a CALL node whose `METHOD_FULL_NAME` is not an `<operator>.` synthetic
and that has a `CALL` edge to a METHOD". `internal/provider/joern/emit.go` confirms the shape —
`stageRelations` joins `ident f ON f.method = p.from_m JOIN ident t ON t.method = p.to_m`, so both
endpoints are **methods**, and the CDG label is stamped at `emit.go:598-599`
(`kind, detail = model.RelControlDependsOn, "joern:CDG"`).

So the published fact is: *"in some function body, the call to method B is control-dependent on
the call to method A"*, with the statement identity collapsed away and the site retained only as
evidence range. Joern's CDG is computed over fine-grained CFG nodes (literals, identifiers,
operator calls); codectx keeps the small minority of those that are resolved calls.

This matters three ways, and two of them cut against the tree-sitter plan:

- **In favour:** most of Joern's CDG edges are discarded before they become facts. Divergences on
  operator nodes, identifiers, and unresolved calls are invisible to codectx by construction.
- **Against:** aggregation reduces the *count* of divergences, not their *salience*. A single
  wrongly-attributed branch containing a call to a widely-used helper collapses into one highly
  visible method-level edge. And a divergence in *which* method controls survives aggregation
  intact.
- **Against, and load-bearing:** the anchoring filter needs *resolved* call targets. A
  tree-sitter implementation inherits codectx's existing name-heuristic call resolution
  (doc 04 §3.4). The Joern provider stamps `model.PrecisionStaticAnalysis` (`emit.go`,
  `stageRelation`). A tree-sitter CDG would have an exact CFG half and a `syntax`-precision
  endpoint half. `internal/model/facts.go:94-98` offers `compiler`, `language_server`,
  `static_analysis`, `syntax`, `heuristic`. The honest label for the replacement is `syntax`,
  not `static_analysis` — **a real downgrade the recommendation must own.**

---

## 1. (b) Joern's `CfgCreationPass`: one generic pass, not one per frontend

### 1.1 The language-independent core is ~1,100 lines

The entire `x2cpg` control-flow package at `v4.0.100`:

| File | Bytes | Lines |
|---|---|---|
| `passes/controlflow/cfgcreation/CfgCreator.scala` | 29,425 | **649** |
| `passes/controlflow/cfgcreation/Cfg.scala` | 7,154 | 197 |
| `passes/controlflow/CfgCreationPass.scala` | 1,087 | 27 |
| `passes/controlflow/cfgdominator/CfgDominator.scala` | 3,194 | 90 |
| `passes/controlflow/cfgdominator/*` (6 more adapters + pass) | 4,832 | — |
| `passes/controlflow/codepencegraph/CdgPass.scala` | 2,385 | 62 |
| `passes/controlflow/codepencegraph/CpgPostDomTreeAdapter.scala` | 380 | — |
| **total** | **48,457** | **≈1,100** |

There is exactly **one** `CfgCreator.scala` in main source across the whole repository. Every other
`CfgCreationPass*` path in the tree is a *test* file (`c2cpg/.../passes/cfg/CfgCreationPassTests.scala`,
four such files in `jssrc2cpg`, one in `php2cpg`). `x2cpg/layers/ControlFlow.scala` selects the pass
by language only to *skip* it:

```scala
val cfgCreationPass = cpg.metaData.language.lastOption match {
  case Some(Languages.GHIDRA) => Iterator[CpgPassBase]()
  case Some(Languages.LLVM)   => Iterator[CpgPassBase]()
  case _                      => Iterator[CpgPassBase](new CfgCreationPass(cpg))
}
cfgCreationPass ++ Iterator(new CfgDominatorPass(cpg), new CdgPass(cpg))
```

`CdgPass` is 62 lines and is pure textbook: reverse-CFG dominance frontier per method, one CDG edge
per (node, post-dominance-frontier node) pair. The CPG spec states the same
([cpg.joern.io](https://cpg.joern.io/)): "A CDG edge expresses that the destination node is control
dependent on the source node," created "automatically from the dominator and post-dominator trees."

### 1.2 The per-language cost sits in the frontend's *normalizer*

`CfgCreator.cfgFor` dispatches on **CPG node types**, not language syntax: `ControlStructure` with
a `controlStructureType` drawn from a fixed vocabulary (BREAK, CONTINUE, DO, WHILE, FOR, GOTO, IF,
ELSE, SWITCH, TRY, CATCH, FINALLY, MATCH, THROW), plus `JumpTarget`, `Return`, and three operator
specialisations (`logicalAnd`, `logicalOr`, `conditional`). The CPG spec names the mechanism:
control flow graphs can be built automatically from syntax trees "if only control structure types
supported by this specification are employed, **possibly by desugaring on the side of the language
frontend**."

So Joern did not avoid the per-language CFG problem. It *relocated* it into each frontend's
`AstForStatementsCreator`, which is the honest comparison target:

| Frontend | statement creator | bytes | lines |
|---|---|---|---|
| `gosrc2cpg` | `AstForStatementsCreator.scala` | 13,408 | 273 |
| `c2cpg` | `AstForStatementsCreator.scala` | 15,726 | 321 |
| `javasrc2cpg` | `AstForSimpleStatementsCreator` + `AstForStatementsCreator` | 12,497 | ≈260 |
| `jssrc2cpg` | `AstForStatementsCreator.scala` | 39,232 | 916 |
| `pysrc2cpg` | `PythonAstVisitor.scala` (statements + expressions) | 81,515 | 2,349 |

Those files also carry assignment operators, declarations and desugarings that a CFG does not need,
so the CFG-relevant subset is smaller than the numbers above.

### 1.3 Joern's per-language CFG fidelity is uneven and partly absent

This is the reviewer's strongest implicit assumption — that Joern's CDG is a high-fidelity
reference — and it does not survive reading the frontends.

**Go (`gosrc2cpg/astcreation/AstForStatementsCreator.scala`).** The statement dispatch has cases for
`AssignStmt, BranchStmt, BlockStmt, CaseClause, DeclStmt, ExprStmt, ForStmt, IfStmt, IncDecStmt,
RangeStmt, SwitchStmt, TypeSwitchStmt, ReturnStmt` — and then `case _: BaseStmt => Seq(Ast())`.
Consequences, all verified against the fetched files:

- `DeferStmt`, `GoStmt`, `SelectStmt` do not appear in `gosrc2cpg/parser/ParserAst.scala` at all.
  **`defer`, `go` and `select` are not modelled.**
- `LabeledStmt` *is* in `ParserAst.scala` (line 50) but has no case in the dispatch, and a search of
  the repository finds `LabeledStmt` in gosrc2cpg **only** in that enum. Labeled statements produce
  an empty AST, so there are no `JumpTarget` nodes for `goto`/labeled break to resolve against.
- `astForBranchStatement` emits `ControlStructureTypes.BREAK`/`CONTINUE` with **no `JumpLabel`
  child**. `CfgCreator.cfgForBreakStatement`'s `case None` branch then records `breaks = List((node, 1))`
  — so a Go `break outer` is wired as a break of the *innermost* loop.
- `case "fallthrough" => // TODO handling for FALLTHROUGH \n Ast()`. Dropped.

**JavaScript (`jssrc2cpg`), same generic `CfgCreator`.** `astForLabeledStatement` emits a real
`jumpTargetNode`; `astForBreakStatement` and `astForContinueStatement` each attach a `NewJumpLabel`
child. The labeled-jump path in `CfgCreator` therefore works for JS and not for Go. Same CFG pass,
different fidelity — **because fidelity is a frontend property.** Separately, JS `throw` becomes a
`<operator>.throw` CALL, not a `ControlStructureTypes.THROW`, so `cfgForThrowStatement` never fires
for JS.

**Python (`pysrc2cpg/PythonAstVisitor.scala`).** `for` is desugared into a `while`; `with` is
desugared into ~130 lines of assignment + `try/finally` around an `__exit__` call; `raise` becomes a
`<operator>.raise` CALL, not a THROW; `match` becomes a SWITCH with a "TODO add case pattern and
guard statements to cpg". Two verbatim lines:

```scala
def convert(await: ast.Await): NewNode = {
  // Since the CPG format does not provide means to model async/await,
  // we for now treat it as non existing.
  convert(await.value)
}
def convert(yieldExpr: ast.Yield): NewNode = ???
def convert(yieldFrom: ast.YieldFrom): NewNode = ???
```

`???` is Scala's `NotImplementedError`. I did **not** run this; pysrc2cpg very likely catches
per-file conversion failures, so the practical effect is plausibly "that method is degraded or
missing," not "yield is silently ignored." Treat the *direction* as certain and the *blast radius*
as unverified.

**Exceptions, in the generic pass itself.** `cfgForTryStatement` carries its own comment: "To avoid
very large CFGs for try statements, **only edges from the last statement in the `try` block** to
each `catch` block (and optionally the `finally` block) are created." And `cfgForThrowStatement`
wires the throw to the **method exit node**, not to any enclosing handler. Joern's exceptional CFG is
deliberately coarse, by design, in the shared code.

**And it is known-buggy in the field.** `CdgPass` logs "Found CDG edge starting at $nodeLabel node.
This is most likely caused by an invalid CFG." That warning is the substance of
[joernio/joern#4118](https://github.com/joernio/joern/issues/4118), filed against `javasrc2cpg` on a
real repository (`java-sec-code`), reporting BLOCK nodes with three outgoing CFG edges; the issue has
no maintainer response.

### 1.4 Joern has no Rust frontend

`joern-cli/frontends/` at `v4.0.100` contains exactly: `c2cpg, csharpsrc2cpg, ghidra2cpg, gosrc2cpg,
javasrc2cpg, jimple2cpg, jssrc2cpg, kotlin2cpg, php2cpg, pysrc2cpg, rubysrc2cpg, swiftsrc2cpg,
x2cpg`. **No Rust.** For 1 of codectx's 9 languages, `control_depends_on` from Joern is zero today,
and scoping Joern down (option e) does not change that.

Joern also uses **no tree-sitter**: zero matches for `tree-sitter`/`treesitter` in the `v4.0.100`
file tree, zero in `build.sbt`, and a repository code search returns `total_count: 0`.

---

## 2. (a) The construct ledger: which language rules actually change a CDG

The filter that matters: a construct affects CDG **only if it changes which branch condition a
statement is reachable-under** — i.e. only if it changes the post-dominator relation between two
*resolved call sites*. Constructs that only add unconditional edges, or that fan out to code the
projection discards, are free.

| Construct | Changes codectx's CDG? | Joern `v4.0.100` | Tree-sitter pass |
|---|---|---|---|
| `if` / `while` / `for` / `do` | **Yes, core** | generic `CfgCreator` | the core case; ~1 rule per grammar node |
| `switch` fallthrough (C, Go, Java) vs `match` (Rust, Python) | **Yes** | `cfgForSwitchLike` (fallthrough by omitting break) vs `cfgForMatchExpression` (implicit break); Go `fallthrough` **dropped** | two shapes, ~40 lines shared; per-language: which node kind means which |
| `break`/`continue`, unlabeled | **Yes** | generic, via level counters | shared; needs a loop/switch stack |
| `break`/`continue`, **labeled** (Go, Java, JS, Rust `'outer`) | **Yes** | works in JS; **broken in Go** | shared resolver + per-language label extraction (~10 lines each) |
| `goto` (C) | **Yes** | `cfgForGotoStatement` + `withResolvedJumpToLabel` | shared; C only |
| `return` (incl. multiple) | Yes (post-dom) | generic | shared |
| Short-circuit `&&` / `\|\|` / `?:` | **Yes** — these *create* control dependence | `cfgForAndExpression` etc. with True/False edges | shared; per-language operator node kinds |
| JS/TS **optional chaining** `a?.b()` | **Yes** — same shape as `&&` | not specially modelled | one rule, reuse the `&&` machinery |
| Rust **`?` operator** | **Yes** — conditional early return | **no Rust frontend** | the `?` rule itself is small (branch-to-return, ~20 lines), but Rust statements have no Joern baseline at all; the honest anchor is Fraunhofer's hand-written `cpg-language-rust/.../StatementHandler.kt` at 11,302 B (~280 lines), which is what §4 budgets against |
| C `throw`/Java/JS/Python exceptions | Marginal | try-fringe→catch only; `throw`→exit | match Joern's coarse model, or diverge deliberately |
| `try` / `finally` | **Yes** (finally post-dominates) | coarse (last-statement edge only) | the one genuinely fiddly shared rule |
| Java try-with-resources, Python `with` | Via desugaring | Python desugars to try/finally | ~40 lines each, or treat as a plain block |
| Go `defer` | **Barely** — deferred call runs unconditionally at every exit | **not modelled** | optional; Fraunhofer needed ~30 lines |
| Go `select` | Yes, in principle (n-way branch) | **not modelled** | ~20 lines, shape of a switch |
| Go `go f()` | No — spawns, does not branch | not modelled | treat as a call |
| Generators / `yield` | No, for intraprocedural CDG | Python: `???` | treat `yield` as an expression |
| `async` / `await` | No | Python: "treat it as non existing" | treat as an expression |
| C++ destructors / **RAII** | **No, for CDG** — destructors run on *every* path out of scope, so they post-dominate; they do not create control dependence. They matter enormously for *data* dependence, and Clang models them (155 `Dtor` mentions in `CFG.cpp`) because dataflow needs them | not modelled | **skip, deliberately** |
| Lambdas / closures | **Yes, structurally** | separate `Method` nodes | must be separate CFG roots, not inlined |
| Macros (C) | Yes | `cfgForInlinedCall` | out of scope for a CST pass |

The honest reading: **roughly six shared rules do 90% of the work** (branch, loop, jump, short-circuit,
switch/match, try-finally), and the per-language delta is a *node-kind mapping table* plus a handful
of genuinely language-specific rules — Rust `?`, Go labeled jumps, C `goto`, JS optional chaining.
The constructs the reviewer named as hardest — RAII, generators, async, `defer`, `go` — are either
CDG-irrelevant or already unmodelled by the reference implementation.

---

## 3. (c) Who has built this, and how big it is

**Semgrep** — the strongest data point for convergence. At `0516c0f2`:

| File | Bytes | Lines |
|---|---|---|
| `src/analyzing/AST_to_IL.ml` | 99,269 | 2,673 |
| `src/analyzing/CFG_build.ml` | 21,575 | **544** |
| `src/il/IL.ml` | 22,943 | — |
| `src/il/Fun_CFG.ml` | 5,935 | — |

There are **zero per-language CFG files.** `CFG_build.ml` contains no `Lang.X` branches at all (the
only language words in it are in comments), and `AST_to_IL.ml` has six. Semgrep supports ~30
languages this way. Its exception model is a real one — a `throw_destination` threaded through the
state, arcs added at each Call/Throw inside a try — and it carries its own disclaimer: "*subtle:
try/throw. The current algo is not very precise, but it's probably good enough for many analysis.*"

The counterweight is where Semgrep's per-language cost went: `Parse_<lang>_tree_sitter.ml` —
Python 73,875 B, Go 50,767 B, Java 78,884 B, TypeScript 139,062 B, Rust 148,357 B, C++ 178,287 B.
Those build a *full* generic AST (types, patterns, decorators, attributes), far more than a CFG
needs, so they are an upper bound on the normalization cost, not an estimate of it.

**Fraunhofer AISEC `cpg`** — the closest analogue to the proposal, and the tightest number. At
`100564b7`: one generic `cpg-core/.../passes/EvaluationOrderGraphPass.kt`, **1,540 lines** with 64
`fun handle` methods, covering ten language modules (cxx, go, java, jvm, llvm, python, ruby, **rust**,
typescript, ini). Per-language CFG deltas: `cpg-language-go/.../GoEvaluationOrderGraphPass.kt` —
**125 lines total**, of which `handleDeferUnaryOperator` is one method — and
`cpg-language-python/.../PythonUnreachableEOGPass.kt`, 1,740 bytes. That is the entire per-language
control-flow delta across ten languages. Its Rust frontend's hand-written `StatementHandler.kt` is
11,302 bytes (~280 lines); the module's 282 KB `rustast.kt` is generated UniFFI binding code.

**CodeQL — the honest counterexample, and it must be included.** CodeQL is *not* purely non-tree-sitter:
the repo vendors `misc/bazel/3rdparty/tree_sitter_extractors_deps/` and runs a
`tree-sitter-extractor-test.yml` workflow (Ruby, Rust). But its CFG is written **per language, in QL**:
a shared `shared/controlflow/codeql/controlflow/Cfg.qll` of 54,185 B, *plus* a per-language entry
point. The spread, fetched file-by-file:

| Language | file | bytes | on shared `Cfg.qll`? |
|---|---|---|---|
| C# | `controlflow/internal/ControlFlowGraph.qll` | 8,207 | yes |
| Ruby | `controlflow/ControlFlowGraph.qll` | 17,280 | yes |
| Rust | `controlflow/internal/ControlFlowGraphImpl.qll` (+ `Completion.qll` 7,472) | 23,375 | yes |
| Swift | `controlflow/internal/ControlFlowGraphImpl.qll` | 67,942 | yes |
| Go | `semmle/go/controlflow/ControlFlowGraphImpl.qll` | 70,431 | no — standalone |

Aggregate `controlflow/*.qll` under `*/ql/lib/`: C++ ≥248,941 B across 16 files, Go ≥138,895 B,
Python ≥92,032 B, C# ≥63,009 B, Java ≥37,935 B (lower bounds; the tree API truncated). So a
tree-sitter front end does **not** force the generic route — CodeQL parses Ruby and Rust with
tree-sitter and still writes 17–23 KB of QL per language on top of a 54 KB shared module, and 67–70 KB
where it does not share. §6 answers this dissent by not claiming CodeQL's precision.

**Clang — what compiler-grade costs for one language family.** `clang/lib/Analysis/CFG.cpp` at
`llvmorg-20.1.0` is **6,372 lines**, plus `CFG.h` at 1,580, for C/C++/Obj-C alone, with 155
destructor/RAII references. That is the price of *correct*, and it is a useful ceiling: nobody is
proposing codectx build this.

**tree-sitter-graph / stack-graphs — not applicable.** Stack graphs define "name resolution rules
for an arbitrary programming language"; no CFG, no post-dominators, no control dependence.
tree-sitter-graph is a graph-construction DSL over a parse tree, not an analysis.

**Existing tree-sitter CFG projects — real but thin.**
- [`shivasurya/code-pathfinder`](https://pkg.go.dev/github.com/shivasurya/code-pathfinder/sast-engine/graph/callgraph/cfg)
  is a **Go** implementation over tree-sitter for **Python and Go**: `cfg/builder.go` 925 lines
  (Python node kinds: `with_statement`, `except_clause`, `finally_clause`), `cfg/builder_go.go`
  19,695 B (~650 lines), `cfg/cfg.go` 375 lines, `cfg/dispatcher.go` 750 B. Tests total ~49,900 B
  against ~59,000 B of implementation — roughly 1:1. It computes iterative **dominator sets** only;
  no post-dominators, **no CDG**.
- [`bstee615/tree-climber`](https://github.com/bstee615/tree-climber): C and Java, CFG + def-use +
  reaching definitions, no CDG; README concedes "Currently, only basic program constructs are
  supported."
- [GitNexus RFC #567](https://github.com/abhigyanpatwari/GitNexus/issues/567) is an independent
  third-party plan for exactly this pass, citing "tree-climber (MIT) uses Joern's CfgCreator
  algorithm adapted for tree-sitter," Lengauer-Tarjan for post-dominators and Ferrante §3.1.1 for
  control dependence, with schedule estimates of "+1 week per language" and "Phase 4.7
  (CDG + REACHING_DEF): 3-4 weeks."

**The convergence finding.** Joern, Semgrep and Fraunhofer independently landed on the same shape:
*one* generic CFG/EOG builder (649 / 544 / 1,540 lines) over a normalized node vocabulary, with
per-language deltas measured in tens-to-hundreds of lines. CodeQL is the dissent, and it bought
precision codectx has never claimed.

---

## 4. (d) Size estimate for a Go implementation

Anchored on the four measured references above, not asserted:

| Component | Estimate (Go LOC) | Anchor |
|---|---|---|
| CFG data structure, fringe/break/continue/label bookkeeping | 250–400 | Joern `Cfg.scala` 197 |
| Generic CFG builder over a normalized node vocabulary | 700–1,000 | Joern `CfgCreator` 649; Semgrep `CFG_build.ml` 544 |
| Post-dominator tree (Lengauer-Tarjan or iterative) + dominance frontier | 150–250 | Joern `CfgDominator` 90 + adapters |
| CDG derivation + projection to anchored call sites | 100–150 | Joern `CdgPass` 62 |
| **Shared subtotal** | **1,200–1,800** | |
| Per-language node-kind mapping + specific rules — Go, Python, Java, C | **120–220 each** | Fraunhofer Go delta 125; Rust `StatementHandler` ~280 |
| JS + TS + TSX (one mapping, three grammars) | 250–400 total | `ecmascript.scm` is already shared in `lang/queries/` |
| C++ (extends C: try/catch, range-for; **skip destructors**) | 60–120 | |
| Rust (`?`, `match`, labeled loops, no reference impl) | **200–350** | highest-uncertainty item |
| **Per-language subtotal (9 languages)** | **1,200–2,000** | |
| **Implementation total** | **≈2,400–3,800** | |
| Golden-corpus tests + a differential harness against Joern | **2,500–4,500** | code-pathfinder's ~1:1 ratio; Joern's own 4 CFG test files for `jssrc2cpg` alone |
| **All-in** | **≈5,000–8,300** | |

The test line is the real cost and the thing the reviewer is actually worried about. It is plausibly
larger than the implementation, and it is the item to scope explicitly: a *differential* harness
against Joern's own output across nine languages is a much bigger commitment than a golden-corpus
harness over a fixture set codectx controls.

Note the existing tree-sitter provider already supplies the scaffolding this pass needs: per-language
query packs in `internal/provider/treesitter/lang/queries/*.scm`, a per-language `grammar` hook
(`refine`, `docstring`, `isTest`, `importNames` in `worker/grammars.go`), function bodies captured
as `@body`, and declaration containment already computed in `worker/extract.go`. The per-language
delta lands in an existing extension point, not a new subsystem.

**Where it would diverge from Joern, deliberately:**

1. **Exceptional edges.** Joern draws try→catch from the try block's *fringe* only, and throw→method
   exit. Matching that exactly is easy; being *more correct* than it (a call inside try may throw)
   produces extra CDG edges and therefore a diff. Pick one and document it.
2. **Expression-level granularity.** Joern's CFG nodes are expressions; a CST pass at statement
   granularity loses control dependences created *within* a statement — `foo(a && bar())` makes
   `bar` control-dependent on `a`. Mitigable by handling `&&`/`||`/`?:`/`?.` as branch rules
   (§2), which is why they are in the shared core rather than the per-language tables.
3. **Constructs Joern drops.** A tree-sitter pass that handles Go labeled `break` or `fallthrough`
   *correctly* diverges from Joern — in codectx's favour.
4. **Rust.** No baseline exists to match. Rules must come from the language reference.
5. **Precision label.** `syntax`, not `static_analysis` (§0).

---

## 5. (e) The alternative: keep Joern's CDG, scope Joern to intraprocedural facts

This is the conservative option and it is genuinely viable — doc 04 §4.3–4.4 already recommends
sharding Joern per package and moving `calls` to SCIP, under which `control_depends_on` and
`data_flows_to` lose nothing, because both are already intraprocedural.

What it does **not** buy:

- **No CDG cost reduction.** `ControlFlow.passes` runs `CfgCreationPass`, `CfgDominatorPass` and
  `CdgPass` over *every method* in *every shard*, unconditionally. Sharding cuts peak heap (the
  binding constraint since flatgraph removed spill-to-disk) and enables parallelism; it does not
  skip the CFG.
- **No incrementality.** Still a whole-shard JVM rebuild per change (doc 04 §4.3,
  [joernio/joern#5865](https://github.com/joernio/joern/issues/5865) open).
- **Still nothing for Rust.**
- **Still Joern's fidelity**, including Go `defer`/`select`/`fallthrough`/labeled-break, Python
  `yield`, and JS `throw` (§1.3).

What it does buy: zero new code, `static_analysis` precision retained, and no new surface to
maintain across nine grammars.

---

## 6. Recommendation

**Answering the question asked first: this is bounded engineering, not nine compilers.** The
evidence is unambiguous — Joern does it in 649 lines for thirteen frontends, Semgrep in 544 for
~thirty languages, Fraunhofer in 1,540 plus a 125-line Go delta for ten. The bounded part is the
*algorithm*; the per-language part is a node-kind mapping table.

That said, the plan is not "replace everything." It is three steps, and the first is forced:

**1. Build the shared core plus the Rust mapping. This is a requirement, not a choice.** Joern
publishes zero `control_depends_on` for Rust — there is no Rust frontend in `v4.0.100`. That gap
exists under option (e) too, so sharding Joern does not avoid this work. The core
(≈1,200–1,800 lines, §4) gets built either way; the only open question is how many mapping tables
ride on it.

**2. Keep Joern's `control_depends_on` for the seven languages it covers**, under the §5 scoping
(per-package sharding, `calls` moved to SCIP, `--max-num-def`, frontend excludes). No precision
regression for facts that exist today.

**3. Treat Go as a decision gate, not a deliverable.** Implement the Go mapping and diff it against
Joern's CDG on a real corpus. Go is the right gate because Joern's Go CFG is the weakest of the
seven (§1.3), so the diff is maximally informative, and because it yields the one number nobody
has: **differential-test cost per language**. Extend to the remaining six only if that number comes
in at the low end of §4's range.

**Alternative considered (steel-manned).** Reimplement all nine now and delete `control_depends_on`
from the Joern provider outright. Its case is strong: one uniform fact at one precision across nine
languages, file-scoped and incrementally invalidatable, matching codectx's `filesystem`/`treesitter`
unit model instead of fighting it; ~2,400–3,800 lines of Go in an existing extension point; and it
removes an entire overlay layer from the JVM critical path. The convergence evidence (Joern,
Semgrep, Fraunhofer all at 544–1,540 generic lines) says this is a bounded pass, not nine compilers.

**Why the recommendation wins.** Three constraints tip it. (i) The precision downgrade to `syntax`
is a *product* change, not an engineering one, and should not be made silently for seven languages
that currently have a `static_analysis` fact. (ii) The 2,500–4,500-line differential-test burden is
the dominant cost and it scales with the number of languages you must prove parity for — piloting
one language buys the calibration data for near-zero marginal cost. (iii) Rust needs this
regardless, so the shared core gets built either way; the only question is how many mapping tables
ride on it, and that question is cheaper to answer after the pilot than before.

**Tradeoff accepted.** codectx carries two sources of `control_depends_on` at two precisions
(`static_analysis` from Joern for seven languages, `syntax` from tree-sitter for Rust and then Go).
Consumers must be able to tell them apart — but that costs almost nothing, because the mechanism
already exists: `emit.go` builds evidence `Detail` strings exactly this way today
(`"joern:CDG"`, `"joern:REACHING_DEF intraprocedural var=" + o.variable`), so a `"treesitter:CDG"`
detail is an existing pattern, not new surface. The real accepted cost is narrower: two code paths
to maintain for one relation kind during the gate period, and a consumer-visible precision split
that must be documented in `docs/providers-joern.md` and the tree-sitter provider doc. What it
avoids is committing nine grammars' worth of CFG rules — including the exceptional-edge semantics
that every reference implementation surveyed here admits it gets wrong — on the strength of an
estimate rather than a measurement.

**The one-sentence answer to the reviewer.** The generic CFG+CDG pass is a bounded engineering task
— Joern does it in 649 lines for thirteen frontends, Semgrep in 544 for thirty languages, Fraunhofer
in 1,540 plus a 125-line Go delta for ten — but the bounded part is the *algorithm*, the per-language
part is a node-kind mapping table rather than a compiler, and the genuinely large number is the
differential test suite, which is why the right move is to build the core once, ship it where Joern
gives nothing, and prove parity on one language before claiming it on nine.

---

## Verification ledger

**Read directly (strong):** `x2cpg` `CfgCreator.scala`, `Cfg.scala`, `CfgCreationPass.scala`,
`CdgPass.scala`, `CfgDominator.scala`, `ControlFlow.scala`; `gosrc2cpg` `AstForStatementsCreator.scala`
and `ParserAst.scala`; `c2cpg` and `jssrc2cpg` `AstForStatementsCreator.scala`; `pysrc2cpg`
`PythonAstVisitor.scala`; Semgrep `CFG_build.ml` and `AST_to_IL.ml`; Fraunhofer `EvaluationOrderGraphPass.kt`
and `GoEvaluationOrderGraphPass.kt`; Clang `CFG.cpp`/`CFG.h`; code-pathfinder `cfg/builder.go` and
`cfg/cfg.go`; codectx `internal/provider/joern/emit.go`, `scratch.go`, `docs/providers-joern.md`,
`internal/provider/treesitter/worker/extract.go`, `lang/queries/*.scm`.

**Weak / caveated:** GitHub code search results (default-branch only, demonstrably incomplete — it
missed `gosrc2cpg`'s own `ControlStructureTypes.GOTO` use, which I read in the file). CodeQL
aggregate byte totals (tree API `truncated: true`; the five per-language CFG entry points in §3
were re-fetched file-by-file via the contents API and are exact, and the Ruby and C# paths that
404'd under the `ControlFlowGraphImpl.qll` naming were resolved by listing their directories).
Python
`yield = ???` blast radius (source reading, not executed). All LOC estimates in §4 are
extrapolations from the measured anchors in the adjacent column.

*Sources are inline. Research date: 2026-09-13.*
