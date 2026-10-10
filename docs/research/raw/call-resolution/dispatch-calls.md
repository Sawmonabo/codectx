# Calls through a base class: what "who calls Dog.sound?" should return

Research and tests: Oct 9, 2026. Nothing in the codectx repository was changed. Every language-server result
below comes from my own runs, with the raw JSON-RPC transcripts listed in §8. codectx was read at `main` 882da78
and at commit 5ced710, which carries the linker at cd16373; its several-target publication is
`publish.go:564-628` there.

The example used everywhere (each language has its own copy, §8):

```python
class Animal:      def sound(self): ...
class Dog(Animal): def sound(self): ...
class Cat(Animal): def sound(self): ...
def speak(a: Animal): a.sound()   # runs Dog.sound or Cat.sound, depending on the object
def direct(d: Dog):   d.sound()   # runs Dog.sound (or an override below Dog)
```

## 1. Answer

**"Who calls `Dog.sound`?" should return two callers, told apart:**

- `direct`, a **direct** caller: its call names `Dog.sound`.
- `speak`, a caller **through `Animal.sound`**: its call names `Animal.sound`, and runs `Dog.sound` when `a` is a
  `Dog`. Mark it as such, name the base method, and count it apart. For example: "direct: 1; through a base method:
  1 (`speak`, via `Animal.sound`, which has 2 overrides)".

**"Who calls `Animal.sound`?" should return `speak` only.** `direct` never runs `Animal.sound`, so it is not a
caller of it. (It is a *reference* to the same method family, which matters for rename, not for calls.)

**How to store it: one `calls` edge per call site, to the method the receiver's declared type names
(`speak calls Animal.sound`), plus `overrides` edges (`Dog.sound overrides Animal.sound`).** Work out the
through-base callers at query time: from `Dog.sound`, walk `overrides` up to every method it overrides, and collect
the calls into those methods that can dispatch. The answer then holds two ordinary relations,
`speak -calls-> Animal.sound` and `Dog.sound -overrides-> Animal.sound`, so the route is visible even in today's
answer shape, which carries only `{id, from, kind, to}` per relation. This is option (c). The tests support it and add six refinements:

1. **Walk every level up.** With `Animal → Dog → Puppy`, `speak(a: Animal)` is a through-base caller of
   `Puppy.sound`. jdtls and CodeQL return it; clangd climbs one level only and misses it (§2.2).
2. **Expand only calls that can dispatch.** A call that names its target exactly (`super().sound()`,
   C++ `a.Animal::sound()`, a constructed object, a final or non-virtual method) can never run an override. clangd
   lists `a.Animal::sound()` as a caller of `Dog::sound`; tsserver lists `super.sound()` inside `Puppy` as a caller
   of `Animal.sound` (§2.2). The linker records part of this today, and not enough of it to reuse: for the
   static families it binds `DetailHierarchy` when the call can reach subtypes and `DetailDirect` when the
   language says it cannot (`resolve_static.go:246-257`, `overridable` in `hierarchy.go:1345-1381`), and a
   qualified call binds `DetailDirect` (`resolve_static.go:189`). But Python `super()` binds `DetailHierarchy`
   (`resolve_python.go:322-323`), and a flow-bound site binds `DetailFlow` both for an instance that dispatches and
   for an exact constructed object (`resolve_python.go:328-338`). So the dispatch-or-exact bit has to be recorded
   on its own, not read back from the technique detail (all read at 5ced710).
3. **Never add an override's direct callers to the base method's callers.** rust-analyzer, gopls and tsserver do,
   so they list `direct(d: Dog)` as a caller of `Animal.sound`, and tsserver even as a caller of `Cat.sound`
   (§2.1). Eclipse JDT core leaves them out on purpose (bug 160301).
4. **The mark must reach the answer.** Two tools compute it and then drop it: clangd's `mightNeverCall` and Eclipse
   JDT's `isSuperInvocation()` never reach an LSP reply (§3); Eclipse uses its flag only for an off-by-default
   filter in its Search view. No language server marks these callers.
5. **Forward questions need the downward step.** `callees(speak)` should list `Animal.sound` and, marked "may
   dispatch to", its overrides. Without that, `path(speak → Dog.sound)` and a forward impact walk find nothing.
   Every language server's outgoing calls stop at the declared method (§2.1).
6. **A call whose receiver has no declared type** (`def speak_u(a): a.sound()`, with `Dog` and `Cat` objects
   flowing in) has no declared method to point at. Publish one `calls` per flow-found target, each marked with the
   size of the set ("1 of 2"). That is what the flow-based analyzers do (CodeQL Python, PyCG, Jarvis; §3). If the set is
   one class, it is a plain `calls`.

Why (c) and not a per-override edge: (c) is how every code-navigation index stores the call (CodeQL Java's static
callee, Kythe's `ref/call`, SCIP's reference to `Animal#sound()`, code-graph-rag's `CALLS`, every language server's
outgoing call). It costs one edge per call site instead of one per call site and override. And adding a subclass
adds one `overrides` edge in the new file instead of changing the facts of every file that calls the base method.
That fits ADR-0012's delta rule, under which a rebuild "republishes only the facts whose evidence lies in a file
whose bytes changed" (`docs/adr/ADR-0012-native-dependence-engine.md:680-681` at 5ced710). Whole-program analyzers (Soot, SootUp, WALA,
OPAL, CodeQL data flow, PyCG, Jarvis; all run, §3) store an edge per possible target, because their question is
"what can run here", and the flow-based ones prune targets by the objects that actually arrive. The Java ones keep
the declared target once per call site (WALA's `getDeclaredTarget()`, Soot's call statement), not on the edges;
PyCG and Jarvis keep no call-site record. That view has two costs the runs show: when `Animal.sound` is abstract or
an interface method, "who calls `Animal.sound`" is empty in all four Java tools; and when no object flows into
`speak`, the flow-based runs (Spark, RTA, OPAL points-to, PyCG, Jarvis) report no caller of any `sound` at all.
codectx can serve the "what can run" view from (c) by walking `overrides` down at query time.

## 2. What each language server returned (my runs)

### 2.1 The two-subclass example

"Through" means a call whose receiver is typed as the base (`speak`). "Direct" means `direct(d: Dog)`. "—" means
an empty answer. Every incoming-call reply had exactly the LSP fields `{from, fromRanges}`; none marked anything.

| Server (version) | Incoming calls on `Animal.sound` | on `Dog.sound` | on `Cat.sound` | References on `Animal.sound` / `Dog.sound` (no declarations) | Implementations of `Animal.sound` | Outgoing calls from `speak` |
|---|---|---|---|---|---|---|
| pyright 1.1.414 | `speak` | `direct` | — | `a.sound()` / `d.sound()` | not supported: error -32601 "Unhandled method textDocument/implementation" | `Animal.sound` |
| tsserver (TypeScript 6.0.3 via typescript-language-server 6.0.2) | `speak`, `direct` | `speak`, `direct` | `speak`, `direct` | both: `a.sound()`, `d.sound()` and the other family members' declarations | `Animal.sound`, `Dog.sound`, `Cat.sound` | `Animal.sound` |
| TypeScript 7.0.2 native (`tsc --lsp`), extra | same as tsserver | same | same | both: `a.sound()`, `d.sound()` | same as tsserver | `Animal.sound` |
| jdtls 1.61.0 | `speak` | `direct`, `speak` | `speak` | `a.sound()` / `a.sound()`, `d.sound()` | `Dog.sound`, `Cat.sound` | `Animal.sound` |
| clangd 22.1.6 | `speak` | `direct`, `speak` | `speak` | `a.sound()` / `a.sound()`, `d.sound()`, and the `Animal::sound` declaration | `Dog::sound`, `Cat::sound` | `Animal::sound` |
| rust-analyzer 1.98.1 (`dyn Animal` and `<T: Animal>`) | `speak`, `speak_g`, `direct` | `direct` | — | `a.sound()` ×2, `d.sound()`, and both impl method names / `d.sound()` | `Dog::sound`, `Cat::sound` (asked on `Dog::sound`, also both) | `speak` and `speak_g`: the trait method `Animal::sound` |
| gopls v0.23.0 (interface) | `speak`, `direct` | `speak`, `direct` | `speak` | both: `a.Sound()`, `d.Sound()` | `Dog.Sound`, `Cat.Sound` | `Animal.Sound` (the interface method) |

Interface cases (TypeScript and Java, `interface Speaker { talk(); }` with `Parrot` and `Robot`, and
`chat(s: Speaker) { s.talk(); }`): every server lists `chat` among the incoming calls of `Speaker.talk`,
`Parrot.talk` and `Robot.talk`; implementations of `Speaker.talk` are the two classes' methods; outgoing calls from
`chat` stop at `Speaker.talk`.

What this shows:
- **Callers of an override include callers through its base in four servers** (tsserver, jdtls, clangd, gopls) and
  in none of the other two (pyright, rust-analyzer). No server marks them. With pyright and rust-analyzer, "who
  calls `Cat.sound`?" is a bare zero although `main` passes a `Cat` to `speak`.
- **Outgoing calls always stop at the declared method.** No server lists `Dog.sound` as a callee of `speak`.
- **Three servers add an override's direct callers to the base and siblings.** rust-analyzer and gopls list
  `direct(d: Dog)` under `Animal.sound`; tsserver lists it under `Animal.sound` and `Cat.sound`. Their sources say
  why: gopls documents that "the references to a **method of a concrete type** include references to corresponding
  interface methods" (gopls `doc/features/navigation.md:60-63` at 2faede3), and its call hierarchy is built from
  references. TypeScript's reference search adds "properties/methods of the same name in base classes and
  implemented interfaces definitions" to the search set and treats any reference whose base is in that set as a
  match (`src/services/findAllReferences.ts:2511-2531,2640-2650,2701-2716` at tag v6.0.3), so the whole family is
  one symbol.
- Small reply quirks: pyright repeats each `fromRanges` entry twice; jdtls sets each caller's `selectionRange` to the
  call site, not the caller's name, and lists `main` once per call site; tsserver's references with
  `includeDeclaration: false` still contain the other family members' declarations (TypeScript 7 drops them).

The C cases (Part 1, clangd) are in `name-only-calls.md` §2: an address-taken function counts its taker as a caller,
a call through a local or parameter function pointer reaches nothing, and a global function-pointer variable is
itself a call-hierarchy item.

### 2.2 Three levels, a super call and a qualified call

Fixture: `Animal → Dog → Puppy`, each overriding `sound`; `Puppy.sound` calls `super().sound()` (Python, TS, Java);
`speak(a: Animal) { a.sound() }`; `walk(d: Dog) { d.sound() }`; in C++ also `exact(Animal& a) { a.Animal::sound(); }`.

| Server | Incoming on `Puppy.sound` | Incoming on `Dog.sound` | Incoming on `Animal.sound` |
|---|---|---|---|
| jdtls | `speak`, `walk` (both levels) | `Puppy.sound` (the super call), `speak`, `walk` | `speak` |
| clangd | `walk` only (one level; misses `speak`) | `exact`, `speak`, `walk` (`exact` cannot run it) | `exact`, `speak` |
| tsserver | `speak`, `walk`, `Puppy.sound` | same three | same three (the super call in `Puppy` runs `Dog.sound`, not `Animal.sound`) |
| pyright | — | `Puppy.sound` (the super call), `walk` | `speak` |

jdtls is the only server that is right on all three rows. Its source explains it: Eclipse JDT core matches a call on
a supertype receiver as an accurate match flagged `isSuperInvocation()` ("the method called at run-time may or may
not be the search target"), and it skips calls on a subtype that overrides the method ("since bug 160301 fix,
subclass overridden method calls are not reported"). jdtls then drops the flag (§3).

## 3. Other tools (sources and my runs)

| Tool | What the call `a.sound()` is stored as | "Who calls `Dog.sound`?" | Marked? | Option it matches | Evidence |
|---|---|---|---|---|---|
| CodeQL Java 2.27.2, **run** | One static callee per call, `Animal.sound` (`getMethod`; the database column `callableBinding` is `unique`) | `calls(Dog.sound)`: `direct` only. `polyCalls(Dog.sound)`: `direct`, `speak`. `polyCalls(Cat.sound)`: `speak`. `Animal.sound` under both: `speak` only. Three levels: `polyCalls(Puppy.sound)` is `speak`, `walk`; and because only `Puppy` objects flow in, `viableCallable` narrows `speak` and `walk` to `Puppy.sound`, so `polyCalls(Dog.sound)` there is `walk` and `Puppy.sound` (the super call), not `speak` | By which predicate you ask; `lowConfidenceDispatchTarget` for weak guesses | (c) stored, the dispatch view computed | My run: `out/codeql/java-*.csv`, `deep-java-*.csv`. Docs: "``polyCalls(Callable target)`` succeeds if this callable may call ``target`` at runtime; this is the case if it contains a call whose callee is either ``target`` or a method that ``target`` overrides." (`navigating-the-call-graph.rst:65-66`). `Member.qll:144-160`, `VirtualDispatch.qll:14-64`, data flow uses the viable targets (`DataFlowDispatch.qll:44-68`), dead code: an override is live when the method it overrides is (`DeadCode.qll:24-28`) |
| CodeQL Python 2.27.2 (python-all 7.2.7), **run** | No declared-target edge; the resolved targets | `speak` (annotated `a: Animal`) resolves to `Animal.sound`, `Dog.sound`, `Cat.sound`: the annotation gives `Animal`, and the `Dog()`/`Cat()` arguments flow in. Untyped `speak_u(Dog())` resolves to `Dog.sound`. Three levels: `speak` → `Animal.sound`, `Puppy.sound`; `walk` → `Dog.sound`, `Puppy.sound`; `super().sound()` → `Dog.sound` | No: every target has call type `CallTypeNormalMethod` | (b) with no note | My run: `out/codeql/py-targets.csv`, `deep-py-targets.csv` (`DataFlowDispatch::resolveCall`). Source: annotation as a start point (`DataFlowDispatch.qll:628-632`), upward lookup from the tracked class (`:913-917, 1038-1052`), `self` includes subclasses (`:1064-1079`) |
| CodeQL JavaScript/TypeScript (javascript-all 2.10.3), **run** | The resolved callees | `speak`'s `a.sound()` → `Animal.sound`, `Dog.sound`, `Cat.sound`; `direct`'s `d.sound()` → `Dog.sound`. Three levels: `speak` → `Animal.sound`, `Puppy.sound`; `walk` → `Dog.sound`, `Puppy.sound`; `super.sound()` → nothing | No: every callee has imprecision 0; `isIncomplete` was true on these calls but measures other gaps | (b) with no note | My run: `out/codeql/ts-targets.csv`, `deep-ts-targets.csv`. Source: "the resolved function may be overridden in a subclass and thus is not necessarily the actual target" (`Expr.qll:1044-1051`), imprecision is about global variables (`Nodes.qll:202-253`) |
| Kythe | `ref/call` to "the most specific possible node" the static type gives; `ref/call/direct` for a call that "ignores indirect dispatch" (`A::f()`); `overrides`, `overrides/transitive` | Request mode: `DIRECT_CALLERS` leaves `speak` out; `OVERRIDE_CALLERS`: "Calls to override-related functions will also be considered and each RelatedAnchor will be marked as speculative" | Yes, `speculative` per anchor | (c), exactly as its call-graph doc spells it: walk the override chain at query time | `schema.txt:1183-1222,960,1030`, `callgraph.txt:162-219`, `xref.proto:342-350,543-546` at 69141f0. The open-source table builder never fills override callers (`beam.go:177` "TODO(schroederc): override callers") |
| SCIP (format) | No call relation. The occurrence at `a.sound()` references `Animal#sound()`; `Dog#sound()` carries a relationship to `Animal#sound()` | Find references on `Dog#sound()` "should include references to the `Animal#sound()` method as well", and vice versa | The result keeps the symbol at the use site | (c), for references | `scip.proto:465-515` at 5e03215 (its example is this exact Animal/Dog case) |
| scip-typescript / scip-java / scip-python | Override relationships | | | | TS and Java set `is_implementation` and `is_reference` to every overridden ancestor method; Python sets only `is_implementation`, nearest base only (`FileIndexer.ts:423-447`, `ScipVisitor.java:319-328,484-500`, `treeVisitor.ts:350-368`) |
| Sourcegraph (public snapshot, Aug 2024) | SCIP | API docs: "References are unified across the inheritance hierarchy"; code: Find references on `Dog#sound()` follows Dog's own `is_reference` up to `Animal#sound()`, nothing inverts it, so Find references on `Animal#sound()` does not return Dog's | Only the symbol kept per result | (c) | `codeintel.codenav.graphql:809-895`, `lsifstore/locations_by_position.go:82-126`, `uploads/shared/scip_symbols.go:53-64` at c864f15. Current code is not public |
| IntelliJ IDEA (call hierarchy) | PSI resolve to the method the static type names | Callers of `Dog.sound` include `speak`: the tree also searches `findDeepestSuperMethods()` and drops calls whose receiver type is unrelated (so `Cat` receivers go) | No marker; no option | (c), computed in the tree | `CallerMethodsTreeStructure.java:100-136,193-196`, `JavaCallReferenceProcessor.java:50-63,96-104` at 3946c4b; test `JavaCallHierarchyTest.testEnclosingDeps` |
| IntelliJ Find Usages | | On `Dog.sound`, by default it searches the base method instead ("Search for base method usages", on since May 25, 2021) | Only the tab title ("Usages of base method") | (c) | `JavaFindUsagesHandler.java:144-147`, `JavaMethodFindUsagesOptions.java:13-19,112-116`; help page "Find Usages. Method Options" |
| Eclipse JDT core search | | A reference search for `Dog.sound` matches `a.sound()` on an `Animal` receiver as accurate, flagged `isSuperInvocation()`; a search for `Animal.sound` skips `d.sound()` | **Yes, in the API**: "If `true`, the method called at run-time may or may not be the search target" (`MethodReferenceMatch.java:118-130`) | (c) with a per-match mark | `MethodLocator.java:825-836,918-920`, tests in `JavaSearchBugsTests.java:7940-7959` at 5a4dd83 |
| Eclipse JDT Call Hierarchy and jdtls | | `speak` included, unmarked; the Call Hierarchy never reads `isSuperInvocation`; the Search view has an off-by-default filter "References to overridden" | Dropped | (c), mark lost | `MethodReferencesSearchRequestor.java:43-58` at d15cf87; jdtls `CallHierarchyHandler.java:210-222,298-328`, `ReferencesHandler.java:185-196` at dd5d8cf |
| clangd (source) | Index refs | Override's callers plus "calls to the base function", flagged `mightNeverCall`, one level up | Computed, never serialized | (c), mark lost | `XRefs.cpp:2387-2443`, `Protocol.h:1631-1635`, `Protocol.cpp:1515-1517` at fc4aad7b (the exact build I ran) |
| code-graph-rag 5f76ace, **run** (file and Memgraph) | `CALLS speak → Animal.sound` (`exact`), `OVERRIDES Dog.sound → Animal.sound`; no edge to either override | `callers` matches only `CALLS` into the method (`cypher_queries.py:1064-1067`), so `speak` is not returned; dead code walks `OVERRIDES` downward "to expand virtual-dispatch targets" (`dead_code.py:921-930`) | No | (c) stored, the step only in dead code | My run: `out/cgr/memgraph-cgr-cpp-virt.json`, `memgraph-cgr-py.json` |
| Soot 4.7.1, **run** (Java copy of the example) | One edge per possible target: `(src, srcUnit, tgt, kind)`; `kind` copies the bytecode instruction ("Due to explicit invokevirtual instruction"), so the direct `d.sound()` is `VIRTUAL` too. The declared target is only in the call statement | CHA: `speak` → `Animal.sound`, `Dog.sound`, `Cat.sound`, all `VIRTUAL`; Spark (points-to): `speak` → `Dog.sound`, `Cat.sound`. With nothing flowing into `speak`: CHA all three, Spark none. Abstract `Animal.sound`: no edge to it | No | (b) | My run: `out/round2/soot-{cha,spark}-fixture_*.txt`. `Kind.java:36-50`, `Edge.java:45-66`, `OnFlyCallGraphBuilder.java:629-689` at a25593e |
| SootUp 3.0.1, **run** | `Call(source, target, invokableStmt)`, no kind; one per resolved target | CHA: `speak` → all three; RTA: `speak` → `Dog.sound`, `Cat.sound`, and `Animal.sound` is not in the graph (asking `callsTo` on it throws "Vertex of Method signature … not found") | No | (b) | My run: `out/round2/sootup-{cha,rta}-fixture_*.txt`. `CallGraph.java:36-39`, `ClassHierarchyAnalysisAlgorithm.java:263-296`, `RapidTypeAnalysisAlgorithm.java:327-369` at 64ffb2b |
| WALA 1.8.0, **run** | The call site keeps the declared target and the dispatch code (`CallSiteReference.getDeclaredTarget()`, "the method declared in the invoke instruction only"; `Dispatch { VIRTUAL, SPECIAL, INTERFACE, STATIC }`); the node keeps the target set per site; each target is also a plain edge | CHA: site `pc=1 invocationCode=VIRTUAL declaredTarget=zoo.Animal.sound()V getNumberOfTargets=3`; 0-CFA: `Dog.sound`, `Cat.sound`. `getPossibleSites(speak, Dog.sound)` answers "which sites in `speak` reach `Dog.sound`" | Only the site's declared target and dispatch code | (b), with the declared target kept per site | My run: `out/round2/wala-{cha,0cfa}-fixture_*.txt`. `CallSiteReference.java:24-35,134-143,192-194`, `CallGraph.java:58-73`, `ExplicitCallGraph.java:120-125,201-232` at 45ad7c4 |
| OPAL 7.0.0, **run** | Per caller and call position, the set of targets; "direct/indirect" separates reflection-like calls, not dispatch; the export rebuilds `declaredTarget` from the bytecode | CHA: `speak pc 1 declaredTarget Lzoo/Animal;.sound()V targets [Dog, Animal, Cat]`; points-to: `[Dog, Cat]`; abstract or interface base: `[Dog, Cat]`, and callers of `Animal.sound` are empty | No (`isIncompleteCallSite` exists for unresolvable sites; not exercised) | (b) | My run: `out/round2/opal-{cha,pointsto}-fixture_*.jcg.json`. `Callees.scala:43-80,195`, `Callers.scala:45-47`, `CallGraphAnalysis.scala:141-203` at 35721f8; https://www.opal-project.de/library/api/SNAPSHOT/org/opalj/tac/cg/CallGraph.html |
| PyCG (archived, 8d5dc40), **run** | A plain set of `caller → callee` names; FASTEN metadata always `{}` | With `speak(Dog())`, `speak(Cat())` in the file: `speak` → `Dog.sound`, `Cat.sound`, no edge to `Animal.sound`. With no call to `speak`: no edges at all; the hint `a: Animal` is ignored | No | (b) | My run: `out/round2/pycg-fixture_{i,ii}.json`. `cgprocessor.py:143-155`, `base.py:336-365,514-538`, `callgraph.py:39-42`; paper https://arxiv.org/abs/2103.00587 |
| Jarvis (8871b86), **run** | Same shape as PyCG | Same as PyCG: `speak` → `Dog.sound`, `Cat.sound`; with no call to `speak`, `speak` is absent from the output | No | (b) | My run: `out/round2/jarvis-*.json`. `machinery/callgraph.py:44-67`, `processing/extProcessor.py:105-131,1323-1333`; paper https://arxiv.org/abs/2305.05949 |
| Glean, Python (pyrefly 1.3.2 into the Glean demo image, **run**) | `XRefViaName` and `CalleeToCaller {callee: animals.Animal.sound, caller: use.speak}`: the receiver's static type joined with the method name | `codemarkup.EntityUses` of `Dog.sound` returns only `d.sound()`; nothing derives through-base callers. `python.MethodOverrides` is computed at query time (a fixed walk of 10 levels, `python.angle:678-835`) but no `codemarkup.python` predicate uses it. When `sound` is only inherited, the target name has no declaration and the call is lost | No | (c) stored, no step | My run: `out/glean/pyrefly-summary.txt`, `glean-run-fixture-image-schema.txt`; `convert.rs:529-538,744-809,916-949` |
| Glean, Hack (indexer read in facebook/hhvm; run as the 2023.02.17 HHVM image) | A base-typed call points at the class that defines the method (`sym_def.ml:82-105`, `ce_origin`); `MethodOverrides` facts are stored for every override, nearest parent only. Run: `speak(Animal $a)`'s `$a->sound()` → `Animal::sound` (`FileCall` also records `receiver_type: Animal`); `direct(Dog $d)` → `Dog::sound`; a `dynamic` receiver → `MethodOccurrence {name: sound}` with no class; nothing points at `Cat::sound` | Incoming calls (Glass) look up uses of the exact declaration; overrides feed only "extends" queries, a separate request branch | No | (c) stored, no step | `codemarkup.hack.angle:78-87,368-413`, Glass `Handler/Symbols.hs:725-751` at f7c600a; hhvm at cad2158 |
| **codectx today** | Two or more targets: `calls` to a placeholder `link:` node plus `may_refer_to` to each of `Animal.sound`, `Dog.sound`, `Cat.sound`; no `overrides` is published by any native provider | Default walks skip `may_refer_to`, so `speak` is missing from callers of all three | The placeholder's metadata (`resolution: ambiguous, candidates: 3`) does not reach answers | none of the three | `publish.go:564-628`, `hierarchy.go:1276-1343` at 5ced710; `cost.go:64-89`, `facts.go:50-74,189-194`, `query.go:652-660` at 882da78 |

## 4. The options compared

Same example. "Calls-only" means a request whose relation list is `["calls"]`.

| | (a) A separate kind for dispatch calls, walked by default (`speak calls_via_dispatch Dog.sound`, and to `Cat.sound`) | (b) `calls` to each possible override, with a note ("via `Animal.sound`, 1 of 2") | (c) `calls` to the declared method plus `overrides`; callers of `Dog.sound` add callers of what it overrides, marked |
|---|---|---|---|
| Callers of `Dog.sound`, calls-only | `direct` only: the filter drops the new kind, the same undercount as today | `direct`, `speak`; the note reaches the reader only once answers carry call-site evidence  | `direct`, plus `speak` marked "through `Animal.sound`" if the override step belongs to the `callers` operation rather than to the relation list |
| Callers of `Animal.sound` | `speak` only if the declared target is also stored; (a) usually means edges to the overrides, so `Animal.sound` loses `speak` | Same problem: under CHA `speak` reaches `Animal.sound` only if it has a body, and under RTA only if an `Animal` is created. Run: with an abstract or interface base, Soot, SootUp, WALA and OPAL have no edge to `Animal.sound`, so its callers are empty. The declared target must be kept separately | `speak` |
| Impact of changing `Dog.sound` | Needs the new kind in the impact list | Follows `calls` | Needs the override step: walk `overrides` up, then incoming `calls` |
| Accuracy | As precise as the binding (CHA here) | Same set as (c) with CHA; better only with flow pruning (CodeQL's type flow, Soot's Spark, SootUp's RTA, OPAL's points-to), which in turn reports nothing when no object reaches the call | Same set as (b) with CHA, computed at query time; the mark keeps "may run" apart from "runs" |
| Storage | One edge per site and override | One edge per site and override: a base with 1,000 overrides and 5,000 calls is 5,000,000 edges | One edge per site plus one per override |
| A new subclass in another file | Changes the facts of every file that calls the base method | Same | Adds one `overrides` edge in the new file |
| Real tools | None found for calls. Kythe's `ref/call/direct` is a separate kind for the opposite case (calls that cannot dispatch). Soot keeps the bytecode invoke kind on each edge and WALA on each call site; neither means "through a base" (a direct `d.sound()` is `VIRTUAL` too) | Soot, SootUp, WALA, OPAL call graphs; CodeQL data-flow dispatch and `polyCalls`; CodeQL Python and JS; PyCG; Jarvis (all run, §3) | CodeQL Java storage, Kythe, SCIP, Sourcegraph, IntelliJ, Eclipse JDT, clangd, jdtls, code-graph-rag, and the outgoing calls of all six language servers |

Two more designs the evidence shows:

- **(d) Treat the method family as one symbol** (tsserver for classes; gopls; rust-analyzer for the trait method).
  Callers of any member are the calls bound to any member. Calls-only: `speak` and `direct` under `Dog.sound`, but
  also `direct(d: Dog)` under `Cat.sound` and `Animal.sound`, which is wrong: a `Dog` receiver never runs
  `Cat.sound`. Impact over-reports the same way. It is cheap, and it is the right model for references and rename,
  not for calls.
- **(e) Option (c), but the through-base callers only on request** (Kythe's `OVERRIDE_CALLERS`, CodeQL's
  `polyCalls` beside `calls`, IntelliJ Find Usages' "Search for base method usages", Eclipse's Search-view filter).
  The default answer for `Cat.sound` is then a bare zero, which is the false zero the product requirements rule out
  ("complete by default, nothing to tune"). codectx should take (c)'s storage and make the step the default.

## 5. Recommendation for codectx

Keep the ruling, (c), with these concrete rules. The planned linker and answer changes already cover most of it; the
differences are in **bold**.

1. **Publication.** A member call whose receiver has a declared static type publishes one `calls` to the method
   that type declares or inherits, even when it is abstract or an interface or trait method. No placeholder node, no
   `may_refer_to`, no edge per override. **Keep the dispatch bit on that `calls` edge's evidence:** `hierarchy` when
   the call can reach overrides, `direct` when it cannot (final, static, non-virtual C++, `super`, a qualified call, a
   constructed object). **Record it as its own field.** The linker's technique detail carries it only for the
   static families (`resolve_static.go:189,246-257`, `hierarchy.go:1345-1381`); Python `super()` is tagged
   `DetailHierarchy` and flow-bound sites `DetailFlow` whether they dispatch or not (`resolve_python.go:322-323,
   328-338`), so the bit cannot be read back from the detail (§1, refinement 2).
2. **Hierarchy.** Publish `extends`, `implements` and `overrides`, including Go method-set satisfaction and Rust
   trait impls. **Publish `overrides` to the method directly overridden; compute the transitive closure at query
   time** (Kythe also has `overrides/transitive` for the case where no direct edge exists).
3. **Callers.** `callers(m)` = calls into `m`, then, for every method `b` that `m` overrides **at any depth**,
   the calls into `b` **whose evidence says they can dispatch**, each row marked "through `b`" with `b`'s number of
   overrides. Counts: `direct`, `through_base`. **Never** add the direct callers of an override to the callers of a
   base. **The override step is part of the `callers` operation, not of the relation list**, so a calls-only request
   still gets the marked rows; the answer includes the `overrides` relations it used, so the route
   `speak -calls-> Animal.sound <-overrides- Dog.sound` is visible in today's `Relation{ID, From, Kind, To}`.
4. **Callees and paths (new).** `callees(speak)` = `Animal.sound`, then its overrides at any depth, marked "may
   dispatch to", counted apart. `path` and forward impact use the same downward step at the cost of an `overrides`
   hop. Without it, `path(speak → Dog.sound)` returns nothing.
5. **Impact.** Impact of `m` = callers as in rule 3, plus, when `m` is a base, the methods that override it
   (incoming `overrides`), since a signature change breaks them.
6. **References.** References of `m` walk the family both ways (SCIP `is_reference`, gopls, tsserver,
   IntelliJ): references are for rename and reading, not for call graphs.
7. **No declared type (value-flow sets).** When flow gives a receiver several exact classes and no declared type,
   publish one `calls` per class's method, with the evidence detail `flow` and the set size, and mark each answer row
   "1 of n". One class is a plain `calls`. This matches CodeQL Python, PyCG and Jarvis, which store one edge per target;
   none marks it, which is the gap codectx closes. The mark needs answers to carry call-site evidence.
8. **Costs.** The override walk is bounded by hierarchy depth, so the step adds one index lookup per level per
   visited node. Totals are exact and paged, with no cap, under the existing truncation contract.

## 6. Risks and open questions

1. **Over-approximation is the same as today's CHA.** `speak` is listed under `Cat.sound` even if no `Cat` ever
   reaches it. CodeQL narrows by flow (its three-level run drops `speak` from `polyCalls(Dog.sound)` because only
   `Puppy` objects arrive); codectx could narrow later the same way, as a filter on the through-base rows.
2. **A base outside the unit.** When `Animal` comes from a library, `overrides` points at a node the unit may not
   have. `hierarchy.overridable` already treats unknown rules as "reaches every override"; the publication needs a
   placeholder for the external method, or the through-base step finds nothing.
3. **Agents may still read "through" rows as certain.** The mark, the counts and the tool text reduce the risk.
4. **Answer shape.** The route in rule 3 is visible with today's answer shape (two relations). The counts, and the
   marks in rules 3, 4 and 7, need per-row fields, which means the structured answer change.
5. **C++ templates.** A dependent call (`template <typename T> void speak_t(T& a) { a.sound(); }`) has no declared
   type either. Unless the linker follows the instantiations (`speak_t(d)`, `speak_t(c)`), it is a name-only site
   (`name-only-calls.md`), not a dispatch call. clangd's outgoing calls for it are empty and its go-to-definition
   lists every `sound`; code-graph-rag stores the fan-out as `exact`, which overstates it (`name-only-calls.md` §2).

## 7. Sources

Primary sources only. Code is cited at the commit read; docs by title and URL.

**Protocol and language servers**
- LSP 3.18 specification, "Call Hierarchy" (gh-pages at f8c4bc98) — https://github.com/microsoft/language-server-protocol/blob/f8c4bc9834703b7317c98c8e8053a28fa4b6b997/_specifications/lsp/3.18/language/callHierarchy.md — `CallHierarchyIncomingCall` is `{from, fromRanges}`; no field can mark a possible caller.
- gopls, "Navigation features" (golang/tools at 2faede3) — https://github.com/golang/tools/blob/2faede3474a5d3b52625fbd756bf9b572c813770/gopls/doc/features/navigation.md — lines 60-63 (references cross between interface and concrete methods), 290-292 (dynamic calls not included).
- TypeScript `src/services/findAllReferences.ts` (tag v6.0.3, 050880ce) — https://github.com/microsoft/TypeScript/blob/v6.0.3/src/services/findAllReferences.ts — lines 2511-2531, 2640-2650, 2701-2716: base-class members join the search set.
- clangd `XRefs.cpp`, `Protocol.h`, `Protocol.cpp` (llvm-project at fc4aad7b, the clangd 22.1.6 build that was run) — https://github.com/llvm/llvm-project/blob/fc4aad7b5db3fff421df9a9637605b9ca5667881/clang-tools-extra/clangd/XRefs.cpp — `incomingCalls` 2370-2450; `Protocol.h:1631-1635` (`mightNeverCall`); `Protocol.cpp:1515-1517` (`toJSON` writes `from` and `fromRanges` only).
- eclipse-jdtls/eclipse.jdt.ls at dd5d8cf — https://github.com/eclipse-jdtls/eclipse.jdt.ls/tree/dd5d8cfaef9d0688ba4c3f9754cd79d25277ca99/org.eclipse.jdt.ls.core/src/org/eclipse/jdt/ls/core/internal/handlers — `CallHierarchyHandler.java:210-222,298-328`, `ReferencesHandler.java:185-196`, `ImplementationCollector.java:191-217`.

**IDEs**
- Eclipse JDT core at 5a4dd83 — https://github.com/eclipse-jdt/eclipse.jdt.core/blob/5a4dd839342e057849b378ec912c47035db65dbb/org.eclipse.jdt.core/search/org/eclipse/jdt/core/search/MethodReferenceMatch.java — `isSuperInvocation()` lines 118-130; `MethodLocator.java:825-836,918-920`; tests `JavaSearchBugsTests.java:7940-7959`.
- Eclipse JDT UI at d15cf87 — https://github.com/eclipse-jdt/eclipse.jdt.ui/tree/d15cf8713ca45e721128caf7c150f0ae12924ee6 — `callhierarchy/CallerMethodWrapper.java:142-145`, `MethodReferencesSearchRequestor.java:43-58`, `MethodCall.java:33-36`, `JavaMatchFilter.java:482-486` (the "References to overridden" filter, off by default, lines 83-85).
- Eclipse help, "Call Hierarchy View" — https://help.eclipse.org/latest/topic/org.eclipse.jdt.doc.user/reference/views/ref-call-hierarchy.htm — no option for callers through a base method.
- JetBrains/intellij-community at 3946c4b — https://github.com/JetBrains/intellij-community/blob/3946c4bf00af31d9fbd68be392153ceacc2653da/java/java-impl/src/com/intellij/ide/hierarchy/call/CallerMethodsTreeStructure.java — lines 100-136, 193-196; `JavaCallReferenceProcessor.java:50-63,96-104`; `JavaFindUsagesHandler.java:144-147`; `JavaMethodFindUsagesOptions.java:13-19,112-116`; test `JavaCallHierarchyTest.java:195` (`testEnclosingDeps`).
- IntelliJ IDEA help, "Find Usages. Method Options" — https://www.jetbrains.com/help/idea/find-usages-method-options.html — "Search for base method usages".

**Analyzers**
- CodeQL, "Navigating the call graph" (github/codeql at b5b8165) — https://codeql.github.com/docs/codeql-language-guides/navigating-the-call-graph/ — `getCallee` is the static target; `calls` vs `polyCalls` (lines 58-89 of the `.rst`).
- CodeQL Java library `Member.qll:113,144-160`, `dispatch/VirtualDispatch.qll:14-64`, `dataflow/internal/DataFlowDispatch.qll:44-68`, `deadcode/DeadCode.qll:24-28` — https://github.com/github/codeql/tree/b5b81654b6e16786e6c5669f696d423d8764e336/java/ql/lib/semmle/code/java — also read in the `codeql/java-all` 9.3.2 pack that was run.
- CodeQL Python library `semmle/python/dataflow/new/internal/DataFlowDispatch.qll` (b5b8165; `codeql/python-all` 7.2.7 as run) — https://github.com/github/codeql/blob/b5b81654b6e16786e6c5669f696d423d8764e336/python/ql/lib/semmle/python/dataflow/new/internal/DataFlowDispatch.qll — `resolveCall` 1233-1252; annotation start points 628-632; MRO lookup 913-917, 1038-1079.
- CodeQL JavaScript library `dataflow/Nodes.qll:200-261`, `dataflow/internal/CallGraphs.qll:68-72,174-181`, `Expr.qll:1044-1051` (b5b8165; `codeql/javascript-all` 2.10.3 as run) — https://github.com/github/codeql/tree/b5b81654b6e16786e6c5669f696d423d8764e336/javascript/ql/lib/semmle/javascript
- CodeQL CLI v2.27.2 — https://github.com/github/codeql-cli-binaries/releases/tag/v2.27.2 — the build that was run (zip sha256 ac0a4ea0…16dc, matching the published checksum).
- Soot (soot-oss/soot at a25593e; run: 4.7.1 jar) — https://github.com/soot-oss/soot/blob/a25593e12b7f7cbcc9eb4414e1532e557796dfed/src/main/java/soot/Kind.java — `Kind.java:36-50`; `jimple/toolkits/callgraph/Edge.java:45-66,120-132`, `OnFlyCallGraphBuilder.java:629-689,891`, `CHATransformer.java:53`, `VirtualCalls.java:147-167`.
- SootUp (soot-oss/SootUp at 64ffb2b; run: 3.0.1) — https://github.com/soot-oss/SootUp/tree/64ffb2b2b7a88a80d610a8f96629d64205243675/sootup.callgraph — `CallGraph.java:36-39,103-141`, `ClassHierarchyAnalysisAlgorithm.java:201-296`, `RapidTypeAnalysisAlgorithm.java:327-369`.
- WALA (wala/WALA at 45ad7c4; run: 1.8.0) — https://github.com/wala/WALA/blob/45ad7c4e0660ecec80d66bb59dec18aaf7c4d448/core/src/main/java/com/ibm/wala/classLoader/CallSiteReference.java — lines 24-35, 134-143, 192-194; `IInvokeInstruction.java:19-44`, `ipa/callgraph/CallGraph.java:58-73`, `impl/ExplicitCallGraph.java:120-125,201-232`, `cha/CHACallGraph.java:215-216,381-385`.
- OPAL (opalj/opal at 35721f8; run: 7.0.0) — https://github.com/opalj/opal/tree/35721f8a39628391248eccbff92a15f32d2d3032 — `br/.../properties/cg/Callees.scala:43-80,195`, `Callers.scala:45-47`, `tac/.../cg/CalleesAndCallers.scala:184-192`, `CallGraphAnalysis.scala:141-203`, `CallGraphSerializer.scala:77-135`; OPAL API docs, `CallGraph` — https://www.opal-project.de/library/api/SNAPSHOT/org/opalj/tac/cg/CallGraph.html
- Salis et al., "PyCG: Practical Call Graph Generation in Python", ICSE 2021 — https://arxiv.org/abs/2103.00587 ; source vitsalis/PyCG at 8d5dc40 — https://github.com/vitsalis/PyCG/tree/8d5dc40837803beef1d8d379fbf2cdad6cd94641 — `pycg/processing/cgprocessor.py:143-155`, `processing/base.py:336-365,514-538`, `machinery/callgraph.py:39-42`, `formats/fasten.py:254-256`.
- Huang et al., "JARVIS: Scalable and Precise Application-Centered Call Graph Construction for Python", arXiv 2305.05949v5 — https://arxiv.org/abs/2305.05949 ; source pythonJaRvis/pythonJaRvis.github.io at 8871b86 — https://github.com/pythonJaRvis/pythonJaRvis.github.io/tree/8871b86494a1aec0095686ffa543994a7361af88/Jarvis/tool/Jarvis — `machinery/callgraph.py:44-67`, `processing/extProcessor.py:105-131,1323-1333`.
**Indexing formats and code graphs**
- Kythe schema, edges `ref/call`, `ref/call/direct`, `overrides`, `overrides/transitive` — https://kythe.io/docs/schema/ (`kythe/docs/schema/schema.txt:960,1030-1031,1183-1222` at 69141f0).
- Kythe, "Callgraphs" — https://kythe.io/docs/schema/callgraph.html (`callgraph.txt:162-219`): `ref/call` to "the most specific possible node", and the override-chain query.
- Kythe `kythe/proto/xref.proto:342-350,543-546` (69141f0) — https://github.com/kythe/kythe/blob/69141f022689a611e8a4a1d9b08a3783a2e8a9ed/kythe/proto/xref.proto — `CallerKind`, `speculative`; serving `kythe/go/services/xrefs/xrefs.go:153-188`, `kythe/go/serving/pipeline/beam.go:177,234-245` (override callers never built).
- SCIP `scip.proto:465-515` (scip-code/scip at 5e03215) — https://github.com/scip-code/scip/blob/5e03215598d6d11048f53af966b6004d6804efc3/scip.proto — `is_reference` with the Animal/Dog example; `is_implementation`.
- scip-typescript `src/FileIndexer.ts:423-447` (e6d730c) — https://github.com/sourcegraph/scip-typescript/blob/e6d730c32189f14553d315b697f19bd8b60db65e/src/FileIndexer.ts ; scip-java `ScipVisitor.java:312-328,484-500` (scip-code/scip-java at f85cfc9) — https://github.com/scip-code/scip-java/tree/f85cfc90b308f7243457ec858e0438aa95142350 ; scip-python `packages/pyright-scip/src/treeVisitor.ts:329-368` (8b60bbc) — https://github.com/sourcegraph/scip-python/blob/8b60bbce1f2a4c7a517776cb395bbafb2e731e4f/packages/pyright-scip/src/treeVisitor.ts
- Sourcegraph public snapshot (archived, c864f15) — https://github.com/sourcegraph/sourcegraph-public-snapshot/blob/c864f15af264f0f456a6d8a83290b5c940715349/cmd/frontend/graphqlbackend/codeintel.codenav.graphql — lines 809-895; `internal/codeintel/codenav/internal/lsifstore/locations_by_position.go:82-126`; `internal/codeintel/uploads/shared/scip_symbols.go:53-64`. Sourcegraph docs, "Code navigation features" — https://sourcegraph.com/docs/code-navigation/features
- code-graph-rag (vitali87/code-graph-rag at 5f76ace) — https://github.com/vitali87/code-graph-rag/tree/5f76ace43930842420ac5700aa94f251d48862b3 — `cypher_queries.py:1064-1067` (callers match `CALLS` only), `dead_code.py:921-930` (override expansion for liveness); run with Memgraph 3.3.0.
- Glean (facebookincubator/Glean at f7c600a) — https://github.com/facebookincubator/Glean/tree/f7c600a68fa8db8af91db47a3b4cd16d53843d10 — `glean/lang/python-pyrefly/Glean/Indexer/PythonPyrefly.hs:40-46`, `glean/schema/source/python.angle:678-835`, `codemarkup.hack.angle:78-87,368-413`, Glass `Handler/Symbols.hs:725-751`; pyrefly `pyrefly/lib/report/glean/convert.rs` (9855cf3) — https://github.com/facebook/pyrefly/blob/9855cf3c2d25c9a4cda460c2ec49305a8ecbef71/pyrefly/lib/report/glean/convert.rs ; Hack indexer (facebook/hhvm at cad2158) — https://github.com/facebook/hhvm/tree/cad215892139dfb51d9c322aae5e7079bf1f9ced/hphp/hack/src/typing/write_symbol_info — `sym_def.ml:82-105`, `index_xrefs.ml:289-297,333-344`.

**codectx (read-only)**
- 5ced710: `internal/provider/link/publish.go:564-628` (one target: `calls`; two or more: `calls` to a `link:` node plus `may_refer_to` each), `hierarchy.go:1276-1343` (`dispatch`, CHA over the static type and its subtypes), `hierarchy.go:1345-1381` (`overridable`), `resolve_static.go:246-257` (`DetailHierarchy` vs `DetailDirect`), `provider.go:58-82` (evidence details), `docs/adr/ADR-0012-native-dependence-engine.md:670-686` (the delta rule).
- `main` 882da78 (unchanged on 5ced710): `internal/graph/cost.go:15-89` (costs; `DefaultRelations` and `ImpactRelations`, which leave out `may_refer_to`), `internal/model/facts.go:50-74` (relation kinds, including `overrides`) and `189-194` (`Relation{ID, From, Kind, To}`), `internal/model/query.go:652-660` (`GraphResult.Relations []Relation`).

## 8. Raw transcripts and outputs

Everything is under `<probe>/` (paths below are relative
to it). Each language-server run has `transcript.jsonl` (every raw JSON-RPC frame, both directions, with a
timestamp), `summary.json` (the readable results; `stable: true` means two passes gave identical answers) and
`stderr.log`. The client is `scripts/lspc.py` (`python3 -I scripts/lspc.py SPEC OUTDIR`, one server at a time,
under `flock heavy.lock`); `scripts/show.py` prints a summary and `scripts/raw.py` prints raw request and response
pairs.

| Run | Fixture | Spec | Transcript (frames) |
|---|---|---|---|
| rust-analyzer 1.98.1 | `fixtures/rust/src/main.rs` | `specs/rust.json` | `out/rust-analyzer/transcript.jsonl` (298) |
| clangd 22.1.6, C and C++ | `fixtures/clangd/{fp.c,virt.cpp,compile_commands.json}` | `specs/clangd.json` | `out/clangd/transcript.jsonl` (300) |
| gopls v0.23.0 | `fixtures/go/main.go` | `specs/gopls.json` | `out/gopls/transcript.jsonl` (129) |
| pyright 1.1.414 | `fixtures/py/animals.py` | `specs/pyright.json` | `out/pyright/transcript.jsonl` (213) |
| tsserver (TypeScript 6.0.3) via typescript-language-server 6.0.2 | `fixtures/ts/animals.ts` | `specs/tsls.json` | `out/tsserver/transcript.jsonl` (220) |
| TypeScript 7.0.2 native | `fixtures/ts/animals.ts` | `specs/ts7.json` | `out/ts7-native/transcript.jsonl` (520) |
| jdtls 1.61.0 | `fixtures/java/Main.java` | `specs/jdtls.json` | `out/jdtls/transcript.jsonl` (672) |
| clangd, three levels | `fixtures/deep-cpp/deep.cpp` | `specs/deep-clangd.json` | `out/deep-clangd/transcript.jsonl` (112) |
| pyright, three levels | `fixtures/deep-py/deep.py` | `specs/deep-pyright.json` | `out/deep-pyright/transcript.jsonl` (169) |
| tsserver, three levels | `fixtures/deep-ts/deep.ts` | `specs/deep-tsls.json` | `out/deep-tsls/transcript.jsonl` (104) |
| jdtls, three levels | `fixtures/deep-java/Main.java` | `specs/deep-jdtls.json` | `out/deep-jdtls/transcript.jsonl` (364) |

Other runs:
- CodeQL 2.27.2: queries `scripts/codeql/{java,python,js}/*.ql`, drivers `scripts/codeql/run-dbs.sh` and
  `run-queries.sh`; results `out/codeql/{java,deep-java}-{targets,callers}.csv`,
  `out/codeql/{py,deep-py,ts,deep-ts}-targets.csv`, logs beside them; databases `out/codeql/db-*`.
- code-graph-rag 5f76ace: `out/cgr/` (`index-*/index.bin` file output, `decoded-*.json`, `memgraph-*.json` read back
  from Memgraph, `emits-*.jsonl` write order); fixtures `fixtures/cgr-cpp/`, `fixtures/cgr-cpp-virt/`,
  `fixtures/cgr-py/`; scripts `scripts/cgr/`.
- Glean and pyrefly: `out/glean/` (`pyrefly-summary.txt`, `glean-run-*.txt`, `scip/`, `image-inspect.txt`);
  fixture `fixtures/glean-py/`; scripts `scripts/glean/`.
- Hack indexer: `hh_server /work --write-symbol-info /out --config symbol_write_include_hhi=false --config
  symbolindex_search_provider=NoIndex --config lazy_decl=true --config lazy_parse=true --config lazy_init2=true`
  (the command in `glean/website/docs/indexer/hack.md:40`) inside `docker.io/hhvm/hhvm:2023.02.17` (digest
  `sha256:54683d5c…`, capped at 2 GB and 2 CPUs); fixture `fixtures/hack/animals.hack`; log `out/hack/run.txt`
  ("No errors!"); facts `out/hack/json/glean_symbol_info_chunk_*.json` (`hack.*.6` predicates, the schema of that
  2023 build, not of hhvm cad2158).
- Soot, SootUp, WALA, OPAL, PyCG, Jarvis: `out/round2/` (40 files: `soot-*`, `sootup-*`, `wala-*`, `opal-*`, `pycg-*`,
  `jarvis-*`); fixtures `fixtures/round2-py/fixture_{i,ii}.py` (i: with `speak(Dog())`, `speak(Cat())` and
  `Dog().sound()`; ii: no call to `speak`) and `fixtures/round2-java/{i,ii,iii,iv}` (iii: abstract `Animal.sound`; iv:
  `Animal` an interface); scripts `scripts/round2/` (`run-all.sh` re-runs everything).

Containers: Memgraph (`docker.io/memgraph/memgraph:3.3.0`) and the Glean demo image ran under rootless podman with
names starting `dispatch-probe`; both containers and both images were removed afterwards
(`podman ps -a --filter name=dispatch-probe` and `podman images` show none). Podman was used because no
Docker engine was running.
