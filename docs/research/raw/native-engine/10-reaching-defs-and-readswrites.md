# Native-engine research, raw evidence — the reaching-definitions pass and the reads/writes algebra

Source reading only; nothing was run. Joern paths are relative to the engine clone at
**v4.0.627**. Product paths are relative to the repository root.

## UNIT 1 — the reaching-definitions pass

### (a) The algorithm, from the code

| Claim | Evidence |
|---|---|
| Nodes are **CFG nodes**, not a separate IR. The graph is the method CFG *plus* param pseudo-nodes, because params are not in Joern's CFG | `DataFlowProblem.scala:15-19` ("method parameters are not part of our normal control flow graph… we provide a wrapper"); `ReachingDefProblem.scala:52-55`: `List(entryNode) ++ method.parameter ++ method.reversePostOrder.filter(…) ++ method.parameter.asOutput ++ List(exitNode)` |
| A `Definition` is **an `Int` — the index of a CFG node**, not a variable | `passes/reachingdef/package.scala:4` `type Definition = Int`; `ReachingDefProblem.scala:17-19` `nodeToNumber(node)`; index assigned at `:59` over `allNodesEvenUnreachable` |
| **gen**: every `METHOD_PARAMETER_IN` generates itself; every non-field-access `CALL` generates *itself (its return value) plus each of its `Call`/`Identifier` arguments* | `ReachingDefProblem.scala:177-203`; filter `hasValidGenType` at `:207-213`; field accesses excluded at `:185` "to ensure that they propagate taint unharmed" |
| **kill**: purely name-based. `kill(call) = killsForGens(gen(call))` = all other identifiers/params with the same `name`, all other calls with the same `code`, **plus every `fieldAccess` call whose AST contains an identifier of that name** | `ReachingDefProblem.scala:220-293`; the field-access rule at `:269-275` ("a reassignment `x = new Box()` should kill any previous calls to `x.value`") |
| Transfer function is the textbook one | `ReachingDefProblem.scala:169-171` `gen(n).union(x.diff(kill(n)))` |
| **Forward**, meet = **union** (may-analysis), domain = `mutable.BitSet` | `ReachingDefProblem.scala:27-30`: `meet = (x,y) => x.union(y)`, `new DataFlowProblem(…, forward=true, mutable.BitSet())`; solver entry `ReachingDefPass.scala:31` `calculateMopSolutionForwards` |
| Iteration: worklist seeded in **reverse post-order**, whole-worklist sweep per round, successors re-queued only when `out` changed, `.distinct` | `DataFlowSolver.scala:12-37` |
| IN init empty, **OUT init = gen** (Dragon-book optimization) | `ReachingDefProblem.scala:344-351` |
| Fixed point → edges: `DdgGenerator.addReachingDefEdges` emits `REACHING_DEF` with a `variable` string property, gated by `EdgeValidator.isValidEdge` | `DdgGenerator.scala:216-228`; validator `EdgeValidator.scala:14-31` |
| The solution alone is not the DDG: a `UsageAnalyzer` re-filters `in(n)` by four *string/access-path* predicates — `sameVariable ‖ isContainer ‖ isPart ‖ isAlias` | `DdgGenerator.scala:282-283`, `:288-330`, `:344-355`. `sameVariable` is literally `nodeToString(use).contains(call.code)` — substring matching on source text |
| Six emission families run per method | `DdgGenerator.scala:203-213`: entry-node edges, call-site edges, return edges, `METHOD_PARAMETER_OUT` edges, **captured-identifier/global edges** (`:170-201`), exit-node edges, lone-identifier→exit edges |
| "Lone identifier" optimization removes single-use non-local identifiers from `gen` and wires them straight to the exit node | `ReachingDefProblem.scala:297-341` + `DdgGenerator.scala:154-168` |

**Verdict.** It is a standard forward may-analysis over a bit-vector domain, but two of its three
precision-carrying parts are *not* dataflow: `kill` is name/`code` equality plus an AST scan, and
`UsageAnalyzer` is substring and access-path matching over `CODE`. A Go reimplementation reproduces
the solver in a day and spends its real budget on those two.

### (b) The `--max-num-def` bound

```scala
// ReachingDefPass.scala:44
val numberOfDefinitions = transferFunction.gen.foldLeft(0)(_ + _._2.size)
// ReachingDefPass.scala:46-48
if (numberOfDefinitions > maxNumberOfDefinitions) {
  logger.warn("{} has more than {} definitions", method.fullName, maxNumberOfDefinitions); true
```

- The bound is on **Σ|gen(n)| — a count of definition *facts*** (≈ 1 per param + 1 + |valid args| per
  call). **Not** definitions × nodes, not edges. Default `4000` (`ReachingDefPass.scala:14`,
  `OssDataFlow.scala:15`, `DefaultOverlays.scala:11`); CLI `--max-num-def` at `joern-cli/…/JoernParse.scala:60-62`.
- **Timing matters and is a real finding**: `gen` and `kill` are strict `val`s
  (`ReachingDefProblem.scala:160,163`), so `ReachingDefProblem.create` (`ReachingDefPass.scala:25`)
  has *already built both* before `shouldBailOut` runs at `:26`. The bound protects only the
  fixed-point loop and `addReachingDefEdges`. The cost it does **not** bound is `killsForGens`: for
  each identifier definition it scans every call in the method and AST-traverses each `fieldAccess`
  (`ReachingDefProblem.scala:272-274`) — O(D × |calls| × |ast|).
- **What vanishes**: the early `return` at `ReachingDefPass.scala:28` skips *all* of
  `addReachingDefEdges`, so the method loses every `REACHING_DEF` edge — def-use edges, entry-node
  edges, `<RET>` edges, and the captured/global edges of `DdgGenerator.scala:170-201`. Downstream:
  zero `data_flows_to` for that method. Logged as the paired WARNs
  `"<fullName> has more than <n> definitions"` (`:47`) and `"Skipping."` (`:27`) — exactly the pair
  the product detects (`docs/providers-dependence.md:319`). The product raises 4000 → **40000**
  (`internal/provider/dependence/joern/joern.go:178,204`; `docs/providers-dependence.md:59,63`).

### (c) Itemized measurement

| Main source (`dataflowengineoss/src/main/scala/io/joern/dataflowengineoss/`) | lines |
|---|---|
| `passes/reachingdef/DdgGenerator.scala` (edge emission + UsageAnalyzer) | 367 |
| `passes/reachingdef/ReachingDefProblem.scala` (flow graph, gen/kill, lone-id opt, init) | 351 |
| `passes/reachingdef/DataFlowSolver.scala` (fwd + bwd MOP worklist) | 75 |
| `passes/reachingdef/EdgeValidator.scala` | 61 |
| `passes/reachingdef/ReachingDefPass.scala` (driver + bail-out) | 54 |
| `passes/reachingdef/DataFlowProblem.scala` (generic framework traits) | 49 |
| `passes/reachingdef/package.scala` (`type Definition = Int`) | 5 |
| **reachingdef package total** | **962** |
| `layers/dataflows/OssDataFlow.scala` (layer + options) | 26 |
| `package.scala` (`globalFromLiteral`, `identifierToFirstUsages`) | 50 |
| **Unit-1 main total** | **1,038** |

The generic framework *is* `DataFlowProblem.scala` — there is no separate solver library;
`grep -rn "ReachingDefPass\|OssDataFlow"` returns only these files plus test fixtures and
`joern-cli` drivers.

**Tests (the differential oracle).** `dataflowengineoss/src/test` contains **no reaching-def test at
all** (3 files, 535 lines: `AccessPathUsageTests.scala` 417, `FullNameSemanticsParserTests.scala` 66,
`SemanticTestCpg.scala` 52). The oracle lives in the frontends:

| Frontend dataflow test dir (under `joern-cli/frontends/`) | files | lines |
|---|---|---|
| `c2cpg/src/test/scala/io/joern/c2cpg/dataflow` | 2 | 2,351 |
| `gosrc2cpg/src/test/scala/io/joern/go2cpg/dataflow` | 10 | 1,066 |
| `javasrc2cpg/src/test/scala/io/joern/javasrc2cpg/querying/dataflow` | 14 | 1,869 |
| `jssrc2cpg/src/test/scala/io/joern/jssrc2cpg/dataflow` | 1 | 709 |
| `pysrc2cpg/src/test/scala/io/joern/pysrc2cpg/dataflow` | 1 | 1,347 |
| `rust2cpg` | **0** | **0** |
| **six product families, total** | **28** | **7,342** |

Files that assert `REACHING_DEF`/`reachingDef` by name: `c2cpg/…/dataflow/ReachingDefTests.scala`
(128), `c2cpg/…/dataflow/DataFlowTests.scala` (2,223), `jssrc2cpg/…/dataflow/DataflowTests.scala`
(709), `php2cpg/…/dataflow/IntraMethodDataflowTests.scala` (120),
`rubysrc2cpg/…/dataflow/DoBlockTests.scala` (197),
`swiftsrc2cpg/…/dataflow/{DataFlowTests,ReachingDefTests}.scala` (1,519 + 128) — 5,024 lines.
**Finding: `rust2cpg` has no dataflow tests whatsoever at v4.0.627, so one of the product's six
families has no in-Joern differential oracle.**

### (d) What crosses the boundary into the product

`emit.go` (1,285 lines) contains **no** `reaching_def` string; the derivation is in `scratch.go` and
`rw.go`. Everything below is under `internal/provider/dependence/neo4jcsv/`.

| Question | Answer, with source |
|---|---|
| **Which endpoints?** | **Entities, not call sites and not identifiers.** `data_flows_to` is `a1.target → a2.target` over the `anchors` table (`scratch.go:631`). Entities = non-operator `METHOD` ∪ `LOCAL`/`METHOD_PARAMETER_IN`/`MEMBER` (`scratch.go:542`); anchors map identity, call site → invoked method, and identifier/`FIELD_IDENTIFIER`/`METHOD_REF` → the declaration its `REF` edge names (`scratch.go:550`). Every intermediate operator/literal/`METHOD_RETURN` node is collapsed away. `WHERE a1.target <> a2.target` drops self-flows. |
| **What survives of the walk?** | A **bounded reachability closure**, not the pass's edge set: `maxDataFlowDepth = 8` (`neo4jcsv.go:74-75`) levels through *unanchored* nodes only (`scratch.go:608,620`). A flow needing more than 8 lowering hops is silently absent. |
| **What does `variable` carry?** | In Joern: the edge's `VARIABLE` property, set at `DdgGenerator.scala:224` from `nodeToEdgeLabel` = a param's `name` or a node's `CODE` (`:245-250`), `use.code` for returns (`:117`), `"<RET>"` for return→methodReturn (`:128`), `""` from the entry node (`:52`). **In the product: nothing.** The export *does* carry it — every fixture header is `:START_ID,:END_ID,:TYPE,VARIABLE:string` (e.g. `testdata/c/edges_REACHING_DEF_header.csv`, rows like `…,REACHING_DEF,x += 2`) — and `putEdge` deliberately keeps only `(label, src, dst)` (`scratch.go:413-420`; `edge_in` schema at `:172` has no property column). The product **actively discards** a property it was handed. |
| **`reaching_def`** | Default detail on a `data_flows_to` row (`neo4jcsv.go:57`, `scratch.go:631-636`). |
| **`reaching_def capture`** | Emitted when the walk's start and end have different owning methods, **or** either endpoint has a non-empty `CLOSURE_BINDING` (`neo4jcsv.go:58`, `scratch.go:632-635`). |
| **`global`** | **Documented but never produced.** `docs/providers-dependence.md:22` lists it; no constant and no emission exists — `grep -rn 'detailGlobal\|"global"' internal/` is empty and `scratch.go:631-636` emits only the other two. Mechanistically it *cannot* be produced: Joern's global edges come from `globalFromLiteral` inside `addEdgesToCapturedIdentifiersAndParameters` (`DdgGenerator.scala:190-200`) and carry **no marker** in the export — same `REACHING_DEF` type — so the product infers a capture from `closure_binding <> ''` instead (`neo4jcsv_test.go:182-187`). **Doc/code divergence to fix: either drop `global` from the table or state that globals are reported as `reaching_def capture`.** |

**Verdict on precision consumed.** The product keeps *reachability between declarations,
depth-bounded at 8, unlabelled, with globals folded into captures*. It discards the `variable` label,
every intermediate node identity, the distinction between the six `DdgGenerator` emission families,
and `EdgeValidator`'s semantics gating (which happened upstream, so it is inherited but not
re-derivable). A Go reimplementation therefore does **not** need `UsageAnalyzer`'s access-path
aliasing or the `<RET>`/param-out edge families to match the *published* output — but it does need
them to match the *edge set*, because the depth-8 walk traverses them transitively.

### (e) Go-line estimate, intra-procedural RD over tree-sitter

**Substrate caveat, read first.** The 962 Scala lines ride on a CPG that already supplies `REF` edges
(name resolution), `ARGUMENT` edges with indices, `TYPE_FULL_NAME`, `CLOSURE_BINDING`, `CFG` edges and
a normalized operator lowering. A tree-sitter CST supplies **none** of it. The product's existing
tree-sitter provider (3,353 non-test Go lines; per-language assets are 3–28-line `.scm` files,
165 lines total) extracts **declarations only** and has no scope/binding resolver — the product gets
resolution from SCIP indexers. **The numbers below EXCLUDE a scope/binding resolver.** Including one
adds an estimated **400–900 Go lines per language** and is the single largest swing factor.

| Component | Scala anchor | Go estimate | Basis |
|---|---|---|---|
| Flow graph + RPO numbering + bit-vector worklist solver | 129 (`DataFlowProblem` + `DataFlowSolver` + `package`) | **250–400** | ×2–3 for explicit bitsets, error paths, no collection DSL; param pseudo-nodes are the fiddly part |
| gen/kill construction (incl. same-name kill, field-access kill, lone-id opt) | 351 (`ReachingDefProblem`) | **300–500** | ×1.5 only because the CPG traversal DSL at `:272-274` becomes an explicit index in Go, which is *shorter* |
| Edge emission + use-filtering + the product's projection (anchors, depth-8 walk, capture detail) | 428 (`DdgGenerator`+`EdgeValidator`) + 374 (`rw.go` analogue) | **500–800** | `UsageAnalyzer`'s four predicates are the risk; access-path matching (`AccessPathUsageTests.scala`, 417 test lines) is a sub-project of its own |
| Pass driver, bound, logging, bail-out reporting | 54 + 26 | **100–180** | |
| **Shared core** | **962** | **1,150–1,880** | |

Per-language def/use extraction rules (assignment incl. compound and multiple-assignment; parameters
incl. defaults and receivers; member/field writes; index writes; pointer/indirection writes;
destructuring; closure captures; globals; `for`-range bindings; exception bindings):

| Family | Go lines | Dominant difficulty |
|---|---|---|
| C/C++ | **350–650** | pointer/indirection writes, `&x` escapes, macro-expanded text, two grammars |
| Go | **250–450** | `:=` vs `=`, multi-assign, range bindings, receivers, `defer`/closure capture |
| Java | **250–450** | most regular; `this.f`, enhanced-`for`, `catch` binding, lambda capture |
| JS/TS/TSX | **400–700** | three grammars, destructuring with defaults and rest, `var` hoisting, `let`/`const` TDZ |
| Python | **300–550** | `global`/`nonlocal`, tuple/starred targets, comprehension scopes, augmented assign, `with`/`except as` |
| Rust | **400–700** | pattern bindings everywhere, `ref mut`, shadowing, `&mut` through receivers, `static mut`; **and no Joern oracle to differ against** |

Tests to match Joern's oracle density: **2,500–4,500** shared + **400–900 per language** (Joern's own
is 7,342 lines over five families).

**Uncertainty list, honestly.**
1. **Name resolution is excluded and dominates.** If no SCIP indexer covers a unit, every number
   above is a floor, not an estimate.
2. **`UsageAnalyzer` is substring matching on `CODE`** (`DdgGenerator.scala:349,352`). Reproducing its
   *behaviour* is cheap; reproducing its *results* is not, because `CODE` is frontend-normalized text
   the CST does not have. Whether a Go port should copy the substring hack or replace it with real
   variable identity is an unanswered design question, and the two differ in output.
3. **Fixed-point cost on real inputs is not determinable from source reading.** The bound is on
   Σ|gen(n)|, but the observed hot spot (`killsForGens`, O(D × calls × ast)) runs *before* the bound.
   Determining it requires profiling a run — out of scope here.
4. **Joern's lowering does work the CST does not.** `foo(new Bar())` →
   `{tmp = Bar.alloc(); tmp.init(); tmp}` (`DdgGenerator.scala:107-109`) is frontend work, not pass
   work. Whether a Go implementation lowers or special-cases is a fork worth 200–500 lines either way.
5. **Rust has no oracle.** Estimate confidence for Rust is materially lower than for the other five.

---

## UNIT 2 — reads / writes

### The operator target algebra, as measured

From `docs/research/10-round3-empirical.md` §3 (`raw/rw-fixtures`), reproduced in code at
`rw.go:10-60`: **every frontend lowers every write form into an operator call whose argument 1 is the
written operand.**

| Shape | Example | Target resolution |
|---|---|---|
| identifier with `REF` | `x += 2`, `g = x` (C/JS/Py) | direct: `REF` edge to `LOCAL`/`PARAM`/global |
| `fieldAccess(base, field)` | `s.f = 3` (all), `(*self).f` (Rust) | base identifier resolves; field via `FIELD_IDENTIFIER` + base type's `MEMBER` when typed |
| `indexAccess(container, idx)` | `s.arr[i] = 4`, `d["k"] = 6` | write to the container expression's base |
| `indirection(ptr)` | `*p = 5` (C/Go/Rust) | write through pointer `p` |
| destructuring | `a, b = b, a` (Py/JS/Rust) | temp + one assignment per element (`tmp0` / `_tmp_1` / `<tmp>0`) |
| chained | `y = x = 7` | two assignments, both targets resolve |

Go implementation: `isWriteOperator` (`rw.go:52-60`) uses a **prefix** test on
`<operator>.assignment` plus the four inc/dec ops (`rw.go:21-26`); `unwrapOps` (`:30-38`) covers
index/computed-member/indirection/addressOf/**cast**; `fieldOps` (`:42-47`) covers
fieldAccess/indirectFieldAccess/**memberAccess**/**getElementPtr**. Note `rw.go` covers a **superset**
of §3's measured table (`cast`, `getElementPtr`, `memberAccess` are not in §3) and the prefix test
admits any `assignment*` name, including PHP's two locals. Resolution walk: `resolveTarget`
(`:248-308`) → `memberOf` (`:317-332`, strips `*&` / `&mut ` decoration); reads via
`boundIdentifiers` (`:336-374`); compound/increment reads the operand it writes (`:52-59`, `:181`).
Unresolved → `may_refer_to` against a provider-local unresolved entity (`rw.go:163-172`).

### The four documented gaps, exactly as measured

1. **Go `a, b = b, a` lowers only the first target** (gosrc2cpg) — the second write is simply absent.
2. **Rust `(a, b) = (b, a)` keeps a `tupleLiteral` target** (needs unpacking) — falls to `may_refer_to`.
3. **Java static field `W.g = x` and Go package global `G = x` appear as `fieldAccess` on a
   type/package base**, not a `REF`'d identifier — resolves only if the base's `TYPE_FULL_NAME`
   happens to name a `TYPE_DECL` with that member.
4. **Rust `static mut G` is unresolved** — no declaration to bind to.

Each is a shape that falls through `resolveTarget` to the `may_refer_to` branch. The stated rule:
`writes` = method → declaration for shapes whose innermost identifier has a `REF` (precise) or whose
field resolves to a `MEMBER` (precise); otherwise `may_refer_to` with the syntactic name. Compound ops
count as both read and write.

### Assignment-family operator count

The generated `Operators` object lives in **codepropertygraph 1.7.74** (`build.sbt:5`), which is
**not in the checkout and no jar is on disk** — a full enumeration is **not determinable from source
reading**; unpacking the cpg jar or its schema would determine it, and both are blocked by the
no-downloads rule. Two independent lower bounds from the checkout agree:

- `grep -rhoE '[^A-Za-z.]Operators\.assignment[A-Za-z]*'` over all `*.scala` → **13 distinct**
  members (`assignment`, `assignmentAnd`, `assignmentArithmeticShiftRight`, `assignmentDivision`,
  `assignmentExponentiation`, `assignmentLogicalShiftRight`, `assignmentMinus`, `assignmentModulo`,
  `assignmentMultiplication`, `assignmentOr`, `assignmentPlus`, `assignmentShiftLeft`,
  `assignmentXor`).
- `semanticcpg/src/main/scala/io/shiftleft/semanticcpg/language/operatorextension/package.scala:24-32`:
  `allAssignmentTypes` = 7 explicit + `assignmentAndArithmetic` (`:9-20`) = **16 distinct**
  write-carrying operators.

Two caveats, both real defects in the reference: `<operator>.assignmentConcat` and
`<operator>.assignmentCoalesce` are **php2cpg-local names**, defined at
`joern-cli/frontends/php2cpg/src/main/scala/io/joern/php2cpg/parser/Domain.scala:44-45`, *not*
`Operators` members and *not* in `allAssignmentTypes` — the product's prefix test at `rw.go:56`
catches them anyway. And **`package.scala:19` lists `Operators.postIncrement` twice**, so
`Operators.postDecrement` (which exists — 15 uses in the checkout) is absent from both
`allAssignmentTypes` and `allArithmeticTypes`; `rw.go:25` lists it explicitly, so **the product is
more correct than the library here**.

### Go-line estimate for native `reads`/`writes` over a tree-sitter CST

| Component | Anchor | Go estimate |
|---|---|---|
| Shared target-resolution walker (unwrap/field/index/deref; `resolveTarget` + `boundIdentifiers` + `memberOf` analogues) | `rw.go` 374 lines | **250–400** |
| Per-language write-form recognition: assignment + compound + inc/dec, destructuring/tuple, index, field, deref, `for`-range binding, `catch`/`except as` binding, closure capture, receiver | no CPG lowering to lean on | **C/C++ 300–500 · Go 200–350 · Java 200–350 · JS/TS/TSX 350–550 · Python 250–450 · Rust 350–550** |
| **Total, six families** | | **1,900–3,150** |

`rw.go`'s 374 lines are *not* a fair anchor on their own: they consume an already-lowered,
already-resolved graph. Over a CST there is no lowering to operator calls at all — the algebra
Joern's frontends implement must be written per language, which is why the per-language figures
exceed the shared walker.

### What is NOT reproducible without name resolution

The entire `decl.ok` branch (`rw.go:159-162`) — i.e. **`writes` as method → declaration**. Without
`REF` edges you cannot tell `x` the local from `x` the package global (gap 3) or from a shadowed
binding; without `TYPE_FULL_NAME` + the `members` map you cannot resolve `s.f` to a `MEMBER`; without
a receiver binding you cannot resolve `(*self).f`. Syntax alone yields only the `may_refer_to` branch
with a syntactic name — which is precisely what the four measured gaps already are, and precisely the
claim `docs/providers-dependence.md:32-33` rests on: *"`reads` and `writes` come from this provider
alone: no SCIP indexer sets a write role, and syntax cannot resolve the target of an assignment."* A
native tree-sitter implementation reproduces the **write-site enumeration** in full and the **target
resolution** only as far as an external binding source (SCIP, or a purpose-built scope resolver)
carries it.

---

## Two product defects this unit surfaced (for the controller, not for this report to fix)

1. **`docs/providers-dependence.md:22` lists a `global` evidence detail that no code path emits.**
   Globals are reported as `reaching_def capture`. Either the table drops `global` or it says so.
2. **The product discards the `VARIABLE` property of every `REACHING_DEF` edge** (`scratch.go:413-420`,
   `edge_in` schema at `:172`). The brief for this lane assumed it was kept. It is not; that is a
   deliberate choice, but it means the published `data_flows_to` carries no variable name.
