# Native-engine research, raw evidence — the structural passes and the CFG creator's edge cases

Verified against the engine clone at tag **v4.0.627** (`4bb889d96ce972e2ded50d0d5765c514c1a032cf`).
Engine paths are relative to that clone's root; product paths are relative to the repository root.
Source reading only — nothing was built, nothing was run, nothing was downloaded.

Counting method: aggregates via `find … -name '*.scala' -exec cat {} + | wc -l`; itemized rows via
explicit `wc -l <path>`. (`xargs wc -l | tail -1` is wrong — it reports only the last chunk's total.)

**Why this file exists.** The call-graph and dataflow inventories size the overlay layers that produce
`calls`, `control_depends_on` and `data_flows_to`, but they treat the **Base** and **TypeRelations**
layers as a footnote and never enumerate what the CFG creator does at the edges. A port that misses a
base pass produces a graph that is quietly missing nodes and edges the later passes read, and the
failure is silent: the passes still run, they just emit less. Sections 2–3 close the base-layer gap,
sections 4–8 close the CFG-edge-case gap.

**Vendor tokens.** Research-form citations below name real paths. Every one has an ADR-safe twin in
section 12; the ADR cites only the twin.

---

## 1. The four overlay layers and the order they run in

`X2Cpg.scala:383-385` is the only definition of the default overlay list:

```scala
  def defaultOverlayCreators(): List[LayerCreator] = {
    List(new Base(), new ControlFlow(), new TypeRelations(), new CallGraph())
  }
```

So the order is **Base → ControlFlow → TypeRelations → CallGraph**, then the dataflow layer, then the
frontend's own post-processing. The dependency declarations agree and are weaker than the order:
`ControlFlow.scala:31` and `TypeRelations.scala:20` both declare `dependsOn = List(Base.overlayName)`;
neither declares a dependency on the other, and **`TypeRelations` runs after `ControlFlow` even though
nothing in `ControlFlow` needs it**.

**Base, in run order** (`Base.scala:14-25`, verbatim order of the `Iterator`):

1. `FileCreationPass` 2. `NamespaceCreator` 3. `TypeDeclStubCreator` 4. `MethodStubCreator`
5. `ParameterIndexCompatPass` 6. `MethodDecoratorPass` 7. `AstLinkerPass` 8. `ContainsEdgePass`
9. `TypeRefPass` 10. `TypeEvalPass`

**ControlFlow** (`ControlFlow.scala:17-24`): `CfgCreationPass` (skipped only for two binary/IR
languages) then `CfgDominatorPass` then `CdgPass`.

**TypeRelations** (`TypeRelations.scala:13-14`): `TypeHierarchyPass`, `AliasLinkerPass`,
`FieldAccessLinkerPass`.

That is **every** pass in the two layers this file owns — the directories hold ten and three files
respectively and no subdirectories, so the enumeration is exhaustive, not a sample.

---

## 2. The row every Base and TypeRelations pass gets

`Scala main lines at the tag` is `wc -l` on the file, measured for this document. `staged by the
product?` is read from the label sets in `internal/provider/dependence/neo4jcsv/scratch.go:44-82`.
`Go estimate` is for a native implementation over a tree-sitter CST, which gets none of the graph the
Scala pass rides on.

| pass | what it computes | algorithm AS WRITTEN | Scala lines | Go estimate | staged by the product? | what the product gains | per-language pieces |
|---|---|---|---|---|---|---|---|
| `ContainsEdgePass` | a CONTAINS edge from every METHOD / TYPE_DECL / FILE to every CFG-relevant node beneath it in the AST | per-source BFS down `_astOut` with a worklist; emits an edge when the child is in a 14-kind destination set, and stops descending when the child is itself a source kind (so containment is to the *nearest* method, not transitive past a nested one). Parallel over sources. The in-source comment concedes it assumes the AST is a tree: "If it contains cycles, then this will give a nice endless loop with OOM" | **50** | **60–100** | **yes** — `CONTAINS` is a staged edge label | **everything.** `owners` (`scratch.go:516-523`) attributes every node to its containing method through the reversed CONTAINS index; without it every fact loses its `from` endpoint. Feeds `codectx_callers`, `codectx_callees`, `codectx_impact`, `codectx_references`, `codectx_symbol_info` | none (node-kind predicate only) |
| `MethodStubCreator` | an external METHOD node, with parameters, a BLOCK and a METHOD_RETURN, for every `methodFullName` a CALL names that no METHOD defines | groups calls by `methodFullName`, summarising each call site as `(name, fullName, signature, dispatchType, minArg, maxArg, numArg)` in insertion-ordered collections *for determinism* (`:27-29`). One consistent summary → a stub with that arity; conflicting summaries → logs `"Inconsistent/erroneous callInfo…"` and reconciles by `min=max(0,min minArg)`, `max=max maxArg`. The in-source comment on the stub builder: "hopelessly confused about the meaning of parameterCount" | **178** | **150–300** | **yes** — METHOD is staged and becomes a `kindMethod` entity (`scratch.go:542-543`) | external call targets. `codectx_callees` and `codectx_dependency_path` resolve to a named external method instead of dropping the edge; the `calls` projection (`scratch.go:574-577`) requires the target to be an `ents` row | none |
| `TypeDeclStubCreator` | an external TYPE_DECL for every TYPE with no declaration | builds a `fullName → TypeDecl` map, then one stub per unmatched `cpg.typ`; the stub's `astParentType` is the namespace block and `filename` is the unknown-file sentinel | **58** | **60–120** | **yes** — TYPE_DECL is staged | `members` (`scratch.go:562-568`) is keyed by `TYPE_DECL.full_name`; a member reached through a stub type still keys correctly. Feeds the precise `writes` branch and `codectx_find_symbol` | none |
| `MethodDecoratorPass` | one METHOD_PARAMETER_OUT per METHOD_PARAMETER_IN, plus a PARAMETER_LINK edge and an AST edge from the method | copies eight properties off the in-parameter, skips parameters that already have a link (logging a deprecation once), warns once on a parameter with no method, and adds an EVAL_TYPE edge when `typeFullName` is null | **62** | **40–80** | **partly** — `METHOD_PARAMETER_OUT` is a staged *node* label, but `PARAMETER_LINK` is a known-not-read edge (`scratch.go:78`) and the pass's AST edge is discarded, because AST edges survive only where the destination is MEMBER / LOCAL / METHOD_PARAMETER_IN (`scratch.go:462-465`) | the out-parameter is what the DDG generator writes its parameter-out edge family to, so it is an *intermediate* the depth-8 def-use walk traverses (`scratch.go:616-629`). Removing it removes a hop, and flows that needed it disappear from `codectx_dependency_path` | none |
| `AstLinkerPass` | the deferred AST edge from a METHOD / TYPE_DECL / MEMBER to its declared parent | for each such node with no AST parent, resolves `astParentType` + `astParentFullName` through the full-name maps for METHOD, TYPE_DECL and NAMESPACE_BLOCK; anything else logs `"Invalid AST_PARENT_TYPE=…"` and the edge is skipped. Frontends defer this so they can emit methods and type declarations independently | **62** | **120–250** | **yes, and load-bearing** — the TYPE_DECL→MEMBER edge it creates is exactly one of the three AST destinations the product keeps | **the precise `writes` branch.** `members` is built from `TYPE_DECL --AST--> MEMBER` (`scratch.go:564-568`) and `owners` falls back to that edge for a MEMBER (`scratch.go:520-522`); `rw.go:299-305` reads `members` through `memberOf`. No AstLinkerPass → no members map → every field write degrades to `may_refer_to` | frontends differ in how much they defer; the port must decide once |
| `ParameterIndexCompatPass` | copies `order` into `parameterIndex` when the latter is unset | one predicate (`index == default || index == -2`) and one property write | **22** | **0** | **no** — the export column the product reads is `ARGUMENT_INDEX` (`csv.go:321`), a different property | nothing directly. It matters upstream, where the parameter index is what matches a call's arguments to a callee's parameters | none |
| `TypeRefPass` | a REF edge from every TYPE node to the TYPE_DECL that declares it | batched `linkToSingle` over `NodeTypes.TYPE`, keyed on `TypeDeclFullName` | **30** | **30–60** | **no, and this is a miscount elsewhere** — the pass creates REF edges *from* TYPE nodes; it does not create TYPE_REF nodes. REF is a staged edge label, so the rows are staged, but the `anchors` query restricts REF sources to IDENTIFIER / FIELD_IDENTIFIER / METHOD_REF (`scratch.go:554-556`), and TYPE is not even a staged node label | nothing directly — **but it is a hard prerequisite of `FieldAccessLinkerPass` (section 3)** | none |
| `TypeEvalPass` | an EVAL_TYPE edge from each of twelve expression/declaration node kinds to the TYPE its `typeFullName` names | the same batched `linkToSingle`, over a twelve-label source list | **43** | **80–200** | **no** — `EVAL_TYPE` is a known-not-read edge (`scratch.go:77`) | nothing directly — **but it is the other hard prerequisite of `FieldAccessLinkerPass` (section 3)**, and it is the only thing that turns a `TYPE_FULL_NAME` *string* into a linked declaration | none |
| `FileCreationPass` | FILE nodes and SOURCE_FILE edges for namespace blocks, type declarations, methods and comments | `linkToSingle` with a create-on-miss callback that invents a FILE for an unknown name | **58** | **0** | **no** — FILE is a known-not-staged node label and `SOURCE_FILE` a known-not-read edge (`scratch.go:60,79`) | nothing. The product owns file identity itself: every node carries `FILENAME`, and `paths` (`scratch.go:508-510`) is built from that column with the owning method's filename as the fallback | none |
| `NamespaceCreator` | one NAMESPACE node per distinct namespace-block name, with a REF edge from each block | `groupBy(_.name)` then one node and n edges per group | **27** | **0** | **no** — NAMESPACE and NAMESPACE_BLOCK are both known-not-staged | nothing | none |
| `TypeHierarchyPass` | INHERITS_FROM edges from TYPE_DECL to each TYPE in `inheritsFromTypeFullName` | `linkToMultiple` over the property array, null-guarded | **33** | **40–90** | **no** — `INHERITS_FROM` is a known-not-read edge (`scratch.go:78`) | nothing today. Section 10 argues this is the single cheapest addition available | the array is filled per frontend; the shared pass is language-independent |
| `AliasLinkerPass` | ALIAS_OF edges (call-graph sub-lane's scope; cited only) | — | **28** | — | **no** — `ALIAS_OF` is known-not-read | — | — |
| `FieldAccessLinkerPass` | a REF edge from every field-access CALL to the MEMBER it reads or writes | for a call whose name is `fieldAccess` or `indirectFieldAccess`, takes argument 1 as the base and the `FIELD_IDENTIFIER`'s `canonicalName` as the field, builds `"<base's linked type full name>.<field>"` and looks up that type's member of that name. Warns on a field access with no field identifier or no base node. Overrides `linkToMultiple` **specifically to skip writing the `dstFullNameKey` property**, and only considers calls that have no outgoing REF edge yet. Its `case None if dstNodeMap(dstFullName).isDefined` arm (`:79-80`) is dead: it can only be reached when the preceding `Some` arm did not match, i.e. when the lookup returned `None`, so the guard is never true | **89** | **120–250** | **the edge is staged and never joined** — see section 3 | in principle the precise `writes` target, and `codectx_references` on a field. In practice **nothing**: the product re-derives the same fact by a weaker route | none |

**Aggregates, measured.** `passes/base/` = **590** lines over 10 files, no subdirectories.
`passes/typerelations/` = **150** lines over 3 files, no subdirectories. The three layer orchestration
files this file owns (`Base.scala` 39 + `ControlFlow.scala` 39 + `TypeRelations.scala` 28) = **106**;
`CallGraph.scala` (30) belongs to the call-graph inventory. Shared helper the linking passes ride on:
`utils/LinkingUtil.scala` = **150**.

---

## 3. The prerequisite graph the source comments do not declare — the finding

Four passes carry an in-source prerequisite comment:

- `ContainsEdgePass.scala:12-14` — "has MethodStubCreator and TypeDeclStubCreator as prerequisite".
- `MethodDecoratorPass.scala:12` — "has MethodStubCreator as prerequisite".
- `CdgPass.scala:21` — "has ContainsEdgePass and CfgDominatorPass as prerequisites".
- `TypeDeclStubCreator.scala:10` and `NamespaceCreator.scala:11` — "has no other pass as prerequisite".

**That list is incomplete, and the missing entry is the one a port will drop.**
`FieldAccessLinkerPass.scala:43` resolves the base expression with `baseNode.evalType`. That accessor
is not a property read. `EvalTypeAccessors.scala:44`, verbatim:

```scala
  private def evalType(traversal: Iterator[A]): Iterator[String] =
    traversal.flatMap(_._evalTypeOut).flatMap(_._refOut).property(Properties.FullName)
```

It walks **EVAL_TYPE → TYPE → REF → TYPE_DECL** and reads that declaration's `FULL_NAME`. EVAL_TYPE
edges are created by `TypeEvalPass` and the TYPE→TYPE_DECL REF edges by `TypeRefPass` — the last two
passes of the Base layer. So:

> **`FieldAccessLinkerPass` is hard-dependent on `TypeEvalPass` and `TypeRefPass`, and nothing in the
> source says so.** Drop either, and the field-access linker silently resolves nothing: every
> `dstMemberFullNames` returns an empty sequence, no REF edge is created, and no warning is logged
> because the empty case is the `baseNode.evalType` traversal producing zero results, not a lookup
> failure.

Two consequences for the plan:

1. **"Dead weight" is a claim about the product's staging, not about the pipeline.** `TypeEvalPass`
   and `TypeRefPass` are correctly described as producing edges the product never reads. They are
   **incorrectly** described as removable: the type-relations layer depends on them. A port that keeps
   field-access resolution must keep an equivalent base-type → declared-type mapping.
2. **The pass ordering is load-bearing and undeclared.** `TypeRelations` declares a dependency only on
   the *layer* `Base`, which happens to be enough — but only because the layer runs all ten passes.
   A port that schedules passes individually has no declared edge to preserve.

### 3.1 And the product does not use the answer

The brief for this work assumed the precise `writes` branch of `rw.go` depends on the field-access
linker. **It does not.** Traced in full:

- `rw.go:279-306` (`fieldOps` branch) takes the base (argument index 1) and the `FIELD_IDENTIFIER`,
  then calls `memberOf(base.typeFullName, name)` at `:299`.
- `memberOf` (`rw.go:317-332`) is a single primary-key read of the `members` map against the base
  node's raw **`TYPE_FULL_NAME` string**, after stripping pointer/reference decoration with
  `strings.Trim(strings.TrimPrefix(typeFullName, "&mut "), "*& ")` (`rw.go:318`).
- `members` (`scratch.go:562-568`) is built from `TYPE_DECL --AST--> MEMBER` and keyed by the type
  declaration's `full_name`.
- The linker's `CALL --REF--> MEMBER` edge **is** staged (`REF` is a staged edge label,
  `scratch.go:70`) and is **never joined**: `anchors` (`scratch.go:554-556`) admits REF edges only
  where the source label is IDENTIFIER, FIELD_IDENTIFIER or METHOD_REF, and a field access is a CALL.

So the engine computes field resolution through the *linked* type (EVAL_TYPE → TYPE → REF →
TYPE_DECL.`FULL_NAME`), publishes it on the call's REF edge, the export carries it, and the product
**stages the edge, ignores it, and re-derives the same fact from the raw `TYPE_FULL_NAME` string with
ad-hoc decoration stripping.** The re-derivation is strictly weaker: it fails on exactly the shapes
where the string and the declared full name differ, which is what the recorded gaps
(a Java static field on a type base, a Go package global on a package base, a Rust `static mut`) are.

**Decision recorded** (the brief's premise did not hold, so this file publishes what the code does):
a native port should resolve a field access through the **linked** route, not the string route. The
port already needs a type table to answer `codectx_symbol_info`; joining on a declaration id rather
than on a decorated string is the same work and strictly more correct. This is a *benefit*, not a
defect in the current product — the product's route is a deliberate simplification — but it is a
free precision gain at port time, and it is out of this file's scope to change.

---

## 4. The CFG creator's full dispatch table

Two levels. The outer dispatch is `CfgCreator.scala:99-129` on the node's *class*:

| case | line | produces |
|---|---|---|
| METHOD, METHOD_PARAMETER_IN, MODIFIER, LOCAL, TYPE_DECL, MEMBER | :101-102 | `Cfg.empty` — **declarations are not CFG nodes** |
| METHOD_REF, TYPE_REF, METHOD_RETURN | :103-104 | single-node CFG |
| any CONTROL_STRUCTURE | :105-106 | the inner dispatch below |
| JUMP_TARGET | :107-108 | `cfgForJumpTarget` (:273) |
| RETURN inside a `try` | :109-110 | `cfgForReturn(inheritFringe = true)` (:317) |
| RETURN elsewhere | :111-112 | `cfgForReturn` (:317) |
| CALL named `logicalAnd` | :113-114 | `cfgForAndExpression` (:332) |
| CALL named `logicalOr` | :115-116 | `cfgForOrExpression` (:347) |
| CALL named `conditional` | :117-118 | `cfgForConditionalExpression` (:364) |
| CALL with `DispatchTypes.INLINED` | :119-120 | `cfgForInlinedCall` (:396) — the macro-expansion case |
| BLOCK whose parent is a method / control structure / logical operator / inlined call | :121-122 | children only — **the block itself is not a CFG node** (`blockMatches`, :145-150) |
| any other BLOCK | :123-124 | children, then the block as a node |
| CALL, FIELD_IDENTIFIER, IDENTIFIER, LITERAL, BLOCK, UNKNOWN | :125-126 | children, then the node (post-order: operands precede the operation) |
| anything else | :127-128 | children only |

The inner dispatch is `cfgForControlStructure` (`:155-187`), on `controlStructureType`. **All fourteen
kinds, with the line of each case:**

| # | kind | case line | handler | what it produces |
|---|---|---|---|---|
| 1 | BREAK | :157 | `cfgForBreakStatement` :210 | entry node, **empty fringe**; records `(node, levels)` in `breaks`, or `(node, label)` in `jumpsToLabel` |
| 2 | CONTINUE | :159 | `cfgForContinueStatement` :239 | same shape, into `continues` |
| 3 | WHILE | :161 | `cfgForWhileStatement` :529 | condition → true-body → back to condition; `whenFalse` is an `else`-body appended to the fringe; exit fringe = condition's fringe as `FalseEdge` + current-level breaks |
| 4 | DO | :163 | `cfgForDoStatement` :494 | body → condition; condition's true edge back to the inner entry; continues go to the condition |
| 5 | FOR | :165 | `cfgForForStatement` :418 | init → (condition → body → update) → condition; continues go to the update if there is one, else to the loop entry |
| 6 | GOTO | :167 | `cfgForGotoStatement` :287 | entry node, empty fringe, one `jumpsToLabel` entry |
| 7 | IF | :169 | `cfgForIfStatement` :562 | condition with `TrueEdge`/`FalseEdge` to the two bodies; a missing branch contributes the condition's own fringe under that edge type |
| 8 | ELSE | :171 | `cfgForChildren` | **no semantics of its own** — the else body is reached through the `if`'s `whenFalse` |
| 9 | SWITCH | :173 | `cfgForSwitchStatement` :553 → `cfgForSwitchLike` :733 | `CaseEdge` from the condition's fringe to **every** case label; fall-through is the *absence* of a break, because each case body's fringe is concatenated |
| 10 | TRY | :175 | `cfgForTryStatement` :611 | section 5 |
| 11 | CATCH | :177 | `cfgForChildren` | no semantics of its own — wired by the `try` |
| 12 | FINALLY | :179 | `cfgForChildren` | ditto |
| 13 | MATCH | :181 | `cfgForMatchExpression` :726 → `cfgForSwitchLike` :733 | same switch machinery, but the body is first split per case by `cfgsForMatchCases` (:706), which starts a fresh `Cfg` after each non-label child — **an implicit `break` at the end of every arm** |
| 14 | THROW | :183 | `cfgForThrowStatement` :189 | section 5 |
| — | anything else | :185-186 | **`Cfg.empty`** — the construct vanishes from the CFG entirely |

`Cfg.empty` is also what an *absent* `controlStructureType` produces, and it is silent: no warning, no
counter. A frontend that invents a control-structure type the shared vocabulary does not contain gets
a node in the AST, a node the product stages (`CONTROL_STRUCTURE` is a staged node label), and **no
control-flow edges at all**.

Four edge types exist and only four (`Cfg.scala:122-134`): `TrueEdge`, `FalseEdge`, `AlwaysEdge`,
`CaseEdge`. **None of them reaches the graph.** `CfgCreator.scala:62-66`:

```scala
    cfgForMethod(entryNode).withResolvedJumpToLabel().edges.foreach { edge =>
      // TODO: we are ignoring edge.edgeType because the
      //  CFG spec doesn't define an edge type at the moment
      diffGraph.addEdge(edge.src, edge.dst, EdgeTypes.CFG)
    }
```

Every CFG edge is published untyped. The true/false distinction exists only inside the pass, where it
is consumed by the fringe algebra. A port must reproduce the *algebra*, but need not publish the type.

---

## 5. The exceptional edges — testing the "every reference gets these wrong" claim

The surrounding research names "exceptional-edge semantics" as the dominant risk of the CFG core, with
the claim that *every reference surveyed admits it gets these wrong*. **Tested against this code, the
claim is the wrong shape.** This implementation is not accidentally wrong; it is **deliberately
coarse, with the rationale written in the source**, and its coarseness is a specific, enumerable set
of dropped edges rather than an unknown error. `CfgCreator.scala:602-610`, verbatim:

> To avoid very large CFGs for try statements, only edges from the last statement in the `try` block
> to each `catch` block (and optionally the `finally` block) are created. The last statement in each
> `catch` block should then have an outgoing edge to the `finally` block if it exists (and not to any
> subsequent catch blocks), or otherwise * be part of the fringe.

### 5.1 What `cfgForTryStatement` actually builds

Structure discovery, in order (`:612-662`): the try body comes from the typed `_tryBodyOut` edge, with
`astChildren.order(1)` as a fallback that logs once; catch bodies from `_catchBodyOut`, falling back to
the AST children that are CATCH **or ELSE** control structures (`:625` — the ELSE arm is how a
Python-style `try/except/else` is modelled), else `order(2)`; the finally body from `_finallyBodyOut`,
falling back to the FINALLY children, else `order(3)`, and **`.headOption` — at most one finally**
(`:661`, "Assume there can only be one").

Three edge families (`:664-679`):

| family | source | destination | exact or approximate |
|---|---|---|---|
| try → catch | the try body's **fringe** | each catch body's entry | **approximate.** The fringe of a straight-line block is its *last* statement. A call on line 1 of a five-statement try body has **no** edge to any handler |
| catch → finally | each catch body's fringe | the finally body's entry | exact for the normal exit of a handler |
| try → finally | the try body's **fringe** | the finally body's entry | approximate in the same way |

**What is dropped, precisely:**

- **Every non-final statement of the try body has no edge to any handler.** There is no
  throw-destination threaded through the traversal, no per-call exceptional arc.
- **There is no catch → catch edge**, and none is needed under this model, because handler selection is
  not modelled at all: control reaches *every* catch body from the same source.
- **There is no exceptional exit from the `finally` block.** The finally body's normal exit becomes the
  whole try statement's fringe (`:693-697`), so a `finally` that re-raises is indistinguishable from
  one that falls through.
- **A throw does not reach any handler.** `cfgForThrowStatement` (`:189-203`) builds the CFG of the
  thrown expression, appends the throw node, and adds exactly one edge —
  `singleEdge(node, exitNode)` (`:202`) — to the **method exit**. A `throw` inside a `try` is wired
  past its own handler.
- **An empty try body collapses the whole statement to the finally body** (`:681-685`): "no catch
  block can be executed since nothing can be thrown".

### 5.2 The one compensating mechanism, and its limit

`withinATryBlock` (`:90-95`) + `cfgFor`'s `case ret: Return if withinATryBlock(ret)` (`:109-110`) make
a `return` inside a try keep its children's fringe (`cfgForReturn(inheritFringe = true)`, `:317-325`),
where a `return` elsewhere has an empty fringe. That is what lets an early `return` inside a try still
reach the `finally` block. Its limit is written into `withinATryBlock` itself: it checks
`node.parentBlock.astParent`, i.e. **one** level, so a `return` nested inside an `if` inside the try
body is not "within a try block" by this predicate and loses the edge.

### 5.3 The verdict a port needs

The honest statement is not "this gets exceptional edges wrong". It is:

> **This implementation models exceptional control flow as a single approximate arc from the try
> body's last statement to each handler, plus throw → method exit. It is sound for "the handler can
> run" and unsound for "which statement could have thrown". A port that matches it is cheap
> (three fringe joins). A port that is *more correct* — an arc from every call inside the try — is
> also cheap, and produces a strictly larger CDG, so it is a deliberate divergence, not a bug fix.**

---

## 6. The jump machinery

**Labels bind by name, method-globally, at the very end.** `Cfg.withResolvedJumpToLabel()`
(`Cfg.scala:77-98`) runs once, from `CfgCreator.run()` (`:62`), after the whole method's CFG is built.
Until then, labels accumulate in `labeledNodes: Map[String, CfgNode]` and jumps in
`jumpsToLabel: List[(CfgNode, String)]`, merged by `++` (`Cfg.scala:61-62`) and by `Cfg.from`
(`:110-114`). A duplicate label name is silently overwritten — `Map` merge, last wins.

**What produces a label.** `cfgForJumpTarget` (`:273-281`) makes the JUMP_TARGET both the entry node
and the whole fringe, then sorts it: a name beginning `case` or `default` goes to `caseLabels` (the
switch machinery), anything else to `labeledNodes`. So **the label namespace is string-prefix-based**:
a user label literally named `caseSensitive` would be routed to the switch machinery.

**What produces a jump.**

| jump | source | jump argument | result |
|---|---|---|---|
| `goto` | `cfgForGotoStatement` :287-308 | typed `_jumpArgumentOut` to a `JumpLabel` (:299-300); else a **legacy fallback that parses the label out of the `code` string** by splitting on spaces and dropping the last character (:302-306) | `jumpsToLabel` |
| labeled `break` | `cfgForBreakStatement` :222-224 | `JumpLabel` child | `jumpsToLabel` — labeled breaks are *treated as gotos* |
| numeric `break` | :225-229 | integer `Literal` = how many loop/switch levels | `breaks` |
| bare `break` | :234-235 | none | `breaks` with level **1** — the innermost loop |
| labeled / numeric / bare `continue` | :251-264 | same three shapes | `jumpsToLabel` / `continues` / `continues` at level 1 |

**Level counting** is `takeCurrentLevel` (`Cfg.scala:183-188`, keep level 1) and
`reduceAndFilterLevel` (`:190-195`, drop level 1 and decrement the rest) — each loop consumes its own
level and passes the rest outward.

**What dangles.** `Cfg.scala:78-96`:

```scala
    val edges = jumpsToLabel.flatMap {
      case (jumpToLabel, label) if label != "*" =>
        labeledNodes.get(label) match {
          case Some(labeledNode) => Some(CfgEdge(jumpToLabel, labeledNode, AlwaysEdge))
          case None =>
            logger.info("Unable to wire jump statement. Missing label {}.", label)
            None
```

An unresolved jump produces **no edge and an `info`-level log line**. Not a warning, not a counter, not
a node property. The special label `"*"` (computed goto — "labels as values") gets an edge to *every*
label in the method, which the source calls "a quick hack" and "might be an over-taint".

**Go's dangling gotos, re-verified at the tag.** `gosrc2cpg`'s statement dispatch
(`AstForStatementsCreator.scala:33-50`) has no `LabeledStmt` case; the enum member exists
(`ParserAst.scala:50`) so it reaches `case _: BaseStmt => Seq(Ast())` at **`:48`** — the empty AST.
Meanwhile `astForBranchStatement` **does** emit a goto with a real label name
(`:257-261`, `gotoAst(branchStmt, branchStmt.code, labelName)`), and `fallthrough` is dropped outright
(`:262-263`). So every Go `goto` lands in `jumpsToLabel` against an empty `labeledNodes`, takes the
`case None` arm above, and produces nothing but an `info` line. **Go's gotos dangle, silently, and the
only signal is a log level the product does not scrape.**

**What a native implementation must do instead**, in order of importance:

1. **Emit the label.** The dangle is a frontend gap, not a pass gap: the resolver works, it has
   nothing to resolve against. A port's normaliser must produce a jump target for every labeled
   statement, in every language that has one.
2. **Count the failures.** Make "unresolved jump" a per-method counter the caller sees, not an `info`
   line. A CFG missing a back edge changes the post-dominator tree and therefore the CDG.
3. **Never throw.** `:230-233` and `:259-262` raise `NotImplementedError` — "Only jump labels and
   integer literals are currently supported" — on a jump argument that is neither. A frontend bug
   therefore **kills the pass for that method**. A Go port must degrade, log and continue.
4. **Drop the `code`-string fallback.** Parsing `"goto label;"` by splitting on spaces
   (`:302-306`) is a legacy path that a greenfield port has no reason to reproduce.
5. **Decide the computed-goto policy explicitly.** Edges to all labels is a documented over-approximation;
   a port should either keep it and say so, or drop the jump and say so.

---

## 7. What the CFG rides on that a tree-sitter CST does not supply

This is the concrete content of the per-language "normalise" step. Each row is something
`CfgCreator` reads directly and a CST does not have.

| what the pass reads | where | what a CST gives instead | what the port must build |
|---|---|---|---|
| **Typed structural edges**: `_conditionOut`, `_forInitOut`, `_forUpdateOut`, `_forBodyOut`, `_doBodyOut`, `_tryBodyOut`, `_catchBodyOut`, `_finallyBodyOut`, and `whenTrue`/`whenFalse` | :434, :423, :445, :456, :497, :614, :632, :654; :531-532, :564-565 | named fields on a grammar node, with different names per grammar | a normalised control-structure record with these eight roles, filled per language. **Note:** these are exactly the labels in the product's known-not-read edge set — `CONDITION`, `TRUE_BODY`, `FALSE_BODY`, `FOR_BODY`, `FOR_INIT`, `FOR_UPDATE`, `WHILE_BODY` (`scratch.go:77-81`). The product stages none of them, so **it could not rebuild the CFG from its own staging even if it wanted to** |
| **`ORDER`**, as a total sibling order used both for traversal and as a positional fallback | `cfgForChildren` :85-86 folds `astChildren` left to right; fallbacks address children by `order(nLocals+1 … nLocals+4)` (:426, :437, :448, :459), `order(1)` (:500, :617), `order(2)` (:627), `order(3)` (:647) and the throw argument's `order(1)` (:196) | sibling order, yes — but no `ORDER` property and no convention that a `for` statement's locals are hoisted ahead of its init/condition/update/body | an explicit order assignment **and** the hoisting convention `nLocals` encodes, or (better) never rely on the positional fallback and always emit the typed role |
| **`ARGUMENT_INDEX`** | `call.argument(1)`/`(2)` at :333-334 and :348-349; `argumentOption(2)`/`(3)` at :365-367; `call.argument.l` at :397 | positional children of a call, but no index property, and no notion that the receiver is argument 0 vs 1 | an argument index on every operand. The product reads the same property for its own derivations (`csv.go:321` → `rw.go:147`), so it is needed twice over |
| **Evaluation order inside expressions** — operands before the operation | the post-order in `cfgFor` :123-126 (`cfgForChildren(node) ++ cfgForSingleNode(node)`) | nothing; a CST node is a tree, not an evaluation schedule | a per-language evaluation-order rule (C's unsequenced arguments, JS's strict left-to-right, Python's) |
| **Short-circuit operators as *canonically named calls*** | :113-118 match on `Operators.logicalAnd`, `logicalOr`, `conditional` | `&&`, `and`, `\|\|`, `or`, `?:`, `if…else` expressions, `??`, `?.` — different spellings, different node kinds | a name-normalisation table per language. This is where control dependence *inside* one statement comes from, so it is not optional |
| **`for` lowered to `while`**, and other desugarings | the `for` handler exists (:418), but `for…in` / `for…of` / `range` / `match` arms arrive pre-lowered from the frontends | the surface loop form | the lowering itself, per language |
| **Temporaries for destructuring** (`tmp0`, `_tmp_1`, `<tmp>0` in the frontends) | consumed implicitly: each lowered assignment is its own CFG node | one syntactic multiple-assignment node | temporary generation with stable names, or a decision to model multiple assignment natively |
| **`DispatchTypes.INLINED`** and an expansion sub-tree hanging off the call | :119, :396-411 | nothing — a CST sees the macro call site, never the expansion | either macro expansion or an explicit decision not to model it |
| **Which BLOCKs are CFG nodes** | `blockMatches` :145-150: a block whose parent is a method, a control structure, a logical operator or an inlined call is *transparent*; every other block is a node | every braced group is a node | the same four-way parent test, or an equivalent |
| **A single exit node per method** | `exitNode = entryNode.methodReturn` (:56); targeted by every `return` (:322) and every `throw` (:202) | nothing | a synthetic exit node. The dominator and CDG passes require exactly one |
| **Declarations excluded from the CFG** | :101-102 | declarations and statements are the same kind of tree node | the same exclusion list |
| **A per-method root for every lambda/closure** | implied by `CfgCreationPass.scala:19` (`cpg.method.toArray`) — the CFG is built once per METHOD | a nested function body inside an expression | a separate CFG root per callable, never inlined into the enclosing one |

---

## 8. Exceptional edges and the product — what is published, and what is wrong

**Does the product stage anything that depends on exceptional edges?** Indirectly, and the answer is
sharper than yes or no.

- **CFG edges themselves are staged as nothing.** `CFG` is a known-not-read edge label
  (`scratch.go:76`). The product never sees the control-flow graph; it sees only the CDG edges the
  dominator machinery derived from it. So every CFG approximation reaches the product **through** the
  post-dominator frontier, amplified or cancelled.
- **`CONTROL_STRUCTURE` and `JUMP_TARGET` are staged node labels** (`scratch.go:51`) — but **neither
  ever anchors.** `anchors` (`scratch.go:550-557`) admits exactly three sources: an entity to itself,
  a non-operator CALL to its invoked method, and an IDENTIFIER / FIELD_IDENTIFIER / METHOD_REF to the
  declaration its REF edge names. A CONTROL_STRUCTURE is none of those.
- The `control_depends_on` projection (`scratch.go:578-581`) inner-joins **both** endpoints against
  `anchors`. So **every CDG edge whose controlling node is the `try`, `if`, `while` or `switch` node
  itself is dropped.** What survives is resolved-call → resolved-call.

**The finding.** Combine that with section 5's try semantics:

> For a `try` block with more than one statement, the only edge into a handler comes from the **last
> statement of the try body**. If that last statement is a resolved call, and a call in the handler is
> a resolved call, the product publishes `control_depends_on(handler call → last try-body call)`.
> **That names the wrong controlling call**: the call that could actually have thrown — anywhere
> earlier in the try body — has no edge, so it never appears as a controller. And because
> `cfgForThrowStatement:202` edges the throw to the method exit rather than to a handler, an explicit
> `throw` inside a `try` creates **no** control dependence on its handler at all.

This is a fidelity finding about a fact the product publishes today, at
`model.PrecisionStaticAnalysis`, feeding `codectx_impact` and `codectx_dependency_path`. It is not a
crash and not an absence; it is a *plausible-looking wrong controller*, which is the worst kind. A
native port that threads a throw-destination through the traversal fixes it, and the fix is
approximately one field on the builder state plus an arc per call inside a try.

---

## 9. Go estimates and totals, addable to the effort table

| scope | Scala at the tag | Go estimate | what the Go version needs that the Scala version gets free |
|---|---|---|---|
| Base-layer passes (10 files) | **590** | **460–850** | the graph. `ContainsEdgePass` gets `_astOut`; `AstLinkerPass` gets the full-name maps; `TypeEvalPass`/`TypeRefPass` get `linkToSingle` and a TYPE node universe. Three of the ten (`FileCreationPass`, `NamespaceCreator`, `ParameterIndexCompatPass` = 107 lines) are **0** in a greenfield port: file identity is the product's, namespaces are unused, and the legacy index compatibility has nothing to be compatible with |
| TypeRelations passes, this file's two (`TypeHierarchyPass`, `FieldAccessLinkerPass`) | **122** | **160–340** | `evalType`'s two-hop traversal, which the port must replace with a real type table (section 3) |
| Layer orchestration (`Base`, `ControlFlow`, `TypeRelations`) | **106** | **60–120** | reflection-based layer registration is not needed; a slice of passes is |
| Shared linking helper (`utils/LinkingUtil.scala`) | **150** | folded into the rows above | |
| Control-flow package (12 files: creator, `Cfg`, pass, 7 dominator files, 2 CDG files) | **1,294** | **1,250–1,900** | a CPG whose nodes already carry ORDER, ARGUMENT_INDEX, typed structural edges and normalised operator names (section 7) |
| **Shared subtotal, this file's scope** | **2,112** | **1,930–3,210** | |

Per-language, the honest anchor is each frontend's statement-lowering file — the place the shared pass
relocated the per-language problem to. Re-measured at the tag for this document:

| frontend | statement-lowering file(s) | Scala lines | Go estimate for the normalise step |
|---|---|---|---|
| c2cpg | `astcreation/AstForStatementsCreator.scala` | **634** | 200–380 |
| gosrc2cpg | `astcreation/AstForStatementsCreator.scala` | **266** | 180–330 (**plus** the labeled-statement and `fallthrough` work the reference never did) |
| javasrc2cpg | `AstForStatementsCreator` 148 + `AstForSimpleStatementsCreator` 380 + `AstForForLoopsCreator` 470 | **998** | 220–400 |
| jssrc2cpg | `astcreation/AstForStatementsCreator.scala` | **816** | 280–500 (three grammars on one mapping) |
| pysrc2cpg | `PythonAstVisitor` 2,462 + `PythonAstVisitorHelpers` 710 | **3,172** | 300–560 (the Scala figure includes all expression lowering, so it is an upper bound, not a target) |
| rust2cpg | `RustVisitor.scala` (no separate statements file) | **1,960** | 300–560, highest uncertainty |
| **per-language total** | | **7,846** | **1,480–2,730** |

The Scala per-language figures are not a like-for-like target: each file also carries declaration and
expression lowering a CFG does not need, which is why every Go estimate is below its anchor.

---

## 10. What a complete repository graph needs that nobody has inventoried

Each candidate is judged on what it would feed and whether it is critical or a benefit.

| candidate | status today | what it would feed | verdict |
|---|---|---|---|
| **The program dependence graph as a joined surface** — control dependence ∧ data dependence between the same two entities | the product publishes two independent families and never joins them (`scratch.go:578-581` and `:630-636` write separate `proj` rows; `occurrences` keys on `(kind, from, to, site, op, target_name)` so the two never meet) | "B is reached only when A holds **and** B consumes A's value" — a materially stronger slice than either edge alone, and the standard PDG definition. It is the natural backing for `codectx_impact` (what actually changes if I change this) and for `codectx_dependency_path` | **Benefit, and the highest-value one.** It needs no new engine work at all: both edge families are already staged and already keyed by the same entity pair. It is a join in the projection |
| **`INHERITS_FROM`** (TYPE_DECL → TYPE) | produced by `TypeHierarchyPass` (33 lines), exported, and in the known-not-read set (`scratch.go:78`) | overriding: `codectx_callers` on an interface method cannot today reach implementations, and `codectx_impact` on a base class cannot reach subclasses. Also `codectx_repo_overview`'s type picture | **Critical for object-oriented repositories, benefit elsewhere.** It is the cheapest addition on this list: the pass already runs, the edge is already in the export, and staging it is one entry in the staged-edge set plus a projection. The only real work is deciding the relation kind it publishes |
| **`DOMINATE` / `POST_DOMINATE`** (produced by `CfgDominatorPass`) | known-not-read (`scratch.go:79`) | "does A always run before B", "does B always run after A" — the basis for must-alias and for ordering claims | **Rule out.** They are intra-procedural and node-level, and the product's entity projection collapses the statement identity they are about. Their only real consumer is the CDG, which is already computed upstream. Staging them would multiply the edge volume for a fact the projection cannot express |
| **`EVAL_TYPE`** | known-not-read (`scratch.go:77`) | the *linked* type of every expression, which is strictly better than the `TYPE_FULL_NAME` string `memberOf` parses today (section 3.1); would also give `codectx_symbol_info` a resolved type rather than a frontend-formatted string | **Benefit, medium.** It is the mechanism behind the field-resolution gain in section 3.1. Cost is real: EVAL_TYPE is one edge per expression node, so it is one of the largest families in the export |
| **`SOURCE_FILE`** (produced by `FileCreationPass`) | known-not-read (`scratch.go:79`) | nothing the product lacks: every node already carries `FILENAME` and `paths` is derived from it (`scratch.go:508-510`) | **Rule out.** Genuinely redundant |
| **`CAPTURE` / `CAPTURED_BY`** | known-not-read (`scratch.go:76`) | closure capture as an explicit edge. The product currently infers capture from a non-empty `CLOSURE_BINDING` property plus a cross-method walk (`scratch.go:632-635`) | **Benefit, small.** It would make the `reaching_def capture` detail a read fact instead of an inference. Worth noting, not worth a lane |
| **`RECEIVER`** | known-not-read (`scratch.go:76`) | which argument of a dynamic call is the receiver — the input to any real virtual-dispatch resolution | **Benefit, and it belongs to the call-graph inventory.** Named here only so it is not lost |
| **The `VARIABLE` property of every REACHING_DEF edge** | carried by the export and deliberately discarded (`scratch.go:413-420`; `edge_in` has no property column at `:172`) | `data_flows_to` rows that name the variable the flow is *through* | **Benefit.** Already recorded elsewhere in this research; repeated here only because it is the same class of decision as the rows above: an export fact the staging drops |
| **A count of dropped/unmodelled constructs per unit** | does not exist anywhere. `CfgCreator`'s `warnOnce` (`:760-765`) deduplicates *globally* across the whole run, `Cfg.scala:86` logs unresolved jumps at `info`, and the fall-through at `:185-186` is silent | an honest per-unit fidelity signal: "this unit's CFG dropped 14 constructs" | **Critical for a port, and free to add there.** The reference cannot tell you how wrong a given unit's CFG is. A native implementation that counts its own fall-throughs turns an unknowable into a published number, which is exactly what a precision label needs to rest on |

---

## 11. Corrected measurements

Every figure below was measured for this document at the tag. Where an existing document differs, the
number here is the measured one and the other is wrong.

| what | previously published | **measured at the tag** | how |
|---|---|---|---|
| `passes/controlflow/cfgdominator/` (7 files) | 208 | **218** | `find … -exec cat {} + \| wc -l`; itemized: 48 + 90 + 38 + 6 + 13 + 13 + 10 = 218 |
| Shared control-flow floor a port reimplements once | 1,284 | **1,294** | 773 + 197 + 27 + 218 + 79. It equals the `passes/controlflow/` directory aggregate over all 12 files exactly |
| `passes/controlflow/codepencegraph/` (2 files) | 79 | **79** | confirmed (68 + 11) |
| A "control-flow subtotal" of 1,294 listed beneath 13 itemized rows | 1,294 | **the rows sum to 1,333** | the 13 rows include `layers/ControlFlow.scala` (39), which is outside the `passes/controlflow/` directory. 1,294 is right for the directory and wrong as the sum of those rows; both figures are correct statements about different sets and must be labelled as such |

Unchanged and re-confirmed: `CfgCreator.scala` **773**, `Cfg.scala` **197**, `CfgCreationPass.scala`
**27**, `CdgPass.scala` **68**, `CfgDominatorPass.scala` **48**, `CfgDominator.scala` **90**,
`CfgDominatorFrontier.scala` **38**, `CfgAdapter.scala` **6**, `CpgCfgAdapter.scala` **13**,
`ReverseCpgCfgAdapter.scala` **13**, `DomTreeAdapter.scala` **10**,
`CpgPostDomTreeAdapter.scala` **11**. Base-layer and TypeRelations per-file figures are as tabulated
in section 2 and match the existing anatomy row for row.

The earlier `≈1,100` figure for this package is **not** contradicted: it is pinned to a different tag
(v4.0.100) and cannot be re-measured from this clone. The 1,294 above is the v4.0.627 figure.

Two descriptive corrections, not arithmetic ones:

1. **`TypeRefPass` does not create TYPE_REF nodes.** It creates REF edges *from* TYPE nodes *to*
   TYPE_DECL nodes. TYPE_REF is an unrelated node label (which the product does stage,
   `scratch.go:52`, for a different reason).
2. **`TypeEvalPass` and `TypeRefPass` are not removable.** "Dead weight" is accurate about the
   product's staging and wrong about the pipeline: section 3 shows the type-relations layer depends on
   both.

---

## 12. Citation index — research form and ADR-safe form

Every engine citation used above, in both forms. The ADR cites only the right-hand column; where the
filename itself carries a vendor token, the right-hand column names the role instead.

| research form | ADR-safe form |
|---|---|
| `joern-cli/frontends/x2cpg/src/main/scala/io/joern/x2cpg/X2Cpg.scala:383-385` | `x2cpg/.../X2Cpg.scala:383-385` |
| `.../x2cpg/layers/Base.scala:14-25` | `x2cpg/.../layers/Base.scala:14-25` |
| `.../x2cpg/layers/ControlFlow.scala:17-24,31` | `x2cpg/.../layers/ControlFlow.scala:17-24,31` |
| `.../x2cpg/layers/TypeRelations.scala:13-14,20` | `x2cpg/.../layers/TypeRelations.scala:13-14,20` |
| `.../x2cpg/passes/base/ContainsEdgePass.scala:12-14,21-31,41-46` | `x2cpg/.../passes/base/ContainsEdgePass.scala:12-14,21-31,41-46` |
| `.../x2cpg/passes/base/MethodStubCreator.scala:27-29,50-90,99` | `x2cpg/.../passes/base/MethodStubCreator.scala:27-29,50-90,99` |
| `.../x2cpg/passes/base/TypeDeclStubCreator.scala:10,24-34,40-56` | `x2cpg/.../passes/base/TypeDeclStubCreator.scala:10,24-34,40-56` |
| `.../x2cpg/passes/base/MethodDecoratorPass.scala:12,20-57` | `x2cpg/.../passes/base/MethodDecoratorPass.scala:12,20-57` |
| `.../x2cpg/passes/base/AstLinkerPass.scala:12-28,34-61` | `x2cpg/.../passes/base/AstLinkerPass.scala:12-28,34-61` |
| `.../x2cpg/passes/base/ParameterIndexCompatPass.scala:9-19` | `x2cpg/.../passes/base/ParameterIndexCompatPass.scala:9-19` |
| `.../x2cpg/passes/base/TypeRefPass.scala:9-29` | `x2cpg/.../passes/base/TypeRefPass.scala:9-29` |
| `.../x2cpg/passes/base/TypeEvalPass.scala:9-42` | `x2cpg/.../passes/base/TypeEvalPass.scala:9-42` |
| `.../x2cpg/passes/base/FileCreationPass.scala:15-56` | `x2cpg/.../passes/base/FileCreationPass.scala:15-56` |
| `.../x2cpg/passes/base/NamespaceCreator.scala:11,17-25` | `x2cpg/.../passes/base/NamespaceCreator.scala:11,17-25` |
| `.../x2cpg/passes/typerelations/TypeHierarchyPass.scala:14-31` | `x2cpg/.../passes/typerelations/TypeHierarchyPass.scala:14-31` |
| `.../x2cpg/passes/typerelations/FieldAccessLinkerPass.scala:36-60,63-87` | `x2cpg/.../passes/typerelations/FieldAccessLinkerPass.scala:36-60,63-87` |
| `.../x2cpg/passes/typerelations/AliasLinkerPass.scala` | `x2cpg/.../passes/typerelations/AliasLinkerPass.scala` |
| `.../x2cpg/utils/LinkingUtil.scala` | `x2cpg/.../utils/LinkingUtil.scala` |
| `.../x2cpg/passes/controlflow/CfgCreationPass.scala:17-25` | `x2cpg/.../passes/controlflow/CfgCreationPass.scala:17-25` |
| `.../x2cpg/passes/controlflow/cfgcreation/CfgCreator.scala` (all line cites in sections 4–7) | `x2cpg/.../passes/controlflow/cfgcreation/CfgCreator.scala` |
| `.../x2cpg/passes/controlflow/cfgcreation/Cfg.scala:34-98,104-195` | `x2cpg/.../passes/controlflow/cfgcreation/Cfg.scala:34-98,104-195` |
| `.../x2cpg/passes/controlflow/codepencegraph/CdgPass.scala:21,31-63` | `x2cpg/.../passes/controlflow/codepencegraph/CdgPass.scala:21,31-63` |
| `semanticcpg/src/main/scala/io/shiftleft/semanticcpg/language/types/propertyaccessors/EvalTypeAccessors.scala:43-44` | `semanticcpg/.../propertyaccessors/EvalTypeAccessors.scala:43-44` |
| `semanticcpg/src/main/scala/io/shiftleft/semanticcpg/language/operatorextension/package.scala:56` | `semanticcpg/.../operatorextension/package.scala:56` |
| `joern-cli/frontends/gosrc2cpg/src/main/scala/io/joern/gosrc2cpg/astcreation/AstForStatementsCreator.scala:33-50,257-263` | `gosrc2cpg/.../astcreation/AstForStatementsCreator.scala:33-50,257-263` |
| `joern-cli/frontends/gosrc2cpg/src/main/scala/io/joern/gosrc2cpg/parser/ParserAst.scala:50` | `gosrc2cpg/.../parser/ParserAst.scala:50` |
| `joern-cli/frontends/x2cpg/src/main/scala/io/joern/x2cpg/internal/ControlStructureAstBuilder.scala:392` | `x2cpg/.../internal/ControlStructureAstBuilder.scala:392` |
| the per-frontend statement-lowering files of section 9 | `c2cpg/`, `gosrc2cpg/`, `javasrc2cpg/`, `jssrc2cpg/`, `pysrc2cpg/`, `rust2cpg/` `.../astcreation/…` at the tag |
| the parse driver that applies the default overlays and the frontend post-processing passes, line 167 | "the parse driver, line 167, at the tag" |
| the export driver, `--repr`/`--format` dispatch | "the export driver, at the tag" |

Product citations carry no vendor token and are used as written: `scratch.go`, `rw.go`, `csv.go`,
`neo4jcsv.go`, all under `internal/provider/dependence/neo4jcsv/`.
