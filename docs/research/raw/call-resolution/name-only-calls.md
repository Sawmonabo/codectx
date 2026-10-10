# Calls no analysis can bind: what codectx should report

Research date: Oct 9, 2026 (America/New_York). Read-only research; nothing in the repository was changed.
codectx inputs read: `main` at 882da78 and the linker change at c47c5c7.

Revised Oct 9, 2026, second pass: rust-analyzer, clangd, typescript-language-server, Glean, code-review-graph
and code-graph-rag are now read at source (why they were missing is at the top of §6). What the pass changed:
- §1: one new sentence carrying the §4.3 refinement below, plus new prior art: two more tools that show labelled
  name matches by default (code-review-graph, code-graph-rag), and clangd, the one language server examined that
  returns possible callers but drops their label before it reaches the client. The recommendation for name-only
  sites is unchanged.
- §4.3 and §5.1: one refinement from the clangd finding. If several-target sites become `calls`, the fact that
  the call depends on dispatch must still reach the answer, and today's answer shape has nowhere to put it.
- §2: new rows for rust-analyzer, clangd, code-review-graph and code-graph-rag; the Glean row is now verified;
  the LSP row covers typescript-language-server. The internal survey (`docs/research/02-code-graph-tools.md`) is
  wrong about code-review-graph's tiers; see its row.
- §4.4, §4.6, §4.7, §4.9 and §5.9 cite the new findings as prior art. Their recommendations are unchanged.

Revised Oct 9, 2026, third pass: the four items the second pass left unconfirmed
were tested hands-on. Raw transcripts and outputs are under `probe/` (listed in
`dispatch-calls.md` §8). What the pass changed:
- rust-analyzer row: the `dyn Trait` and generic calls were run. They appear only under the trait method, so "who
  calls `Cat::sound`" is a bare zero. The reading held.
- clangd row: function pointers and virtual calls were run, and the raw reply was checked. No flag reaches the JSON;
  the base step climbs one level only; a qualified non-virtual call is listed as a possible caller; a call through a
  local or parameter function pointer reaches nothing.
- code-graph-rag row: the C++ template fan-out was run, read from its file output and from a real Memgraph database.
  Both edges are stored `exact`, but one of them is first written `heuristic` and then overwritten.
- Glean row and §6: `python.*` facts come from pyrefly; the Hack indexer was read in facebook/hhvm; a two-file Python
  example was indexed in the Glean demo image.
- §1: one sentence corrected. The second pass said clangd alone returns possible callers through a base method;
  jdtls, gopls and tsserver do too, unmarked (`dispatch-calls.md` §2). The recommendation for name-only sites is
  unchanged. The multi-target question (§4.3, §5.1) is answered in `dispatch-calls.md`.
- The `publish.go:383-450` and `407-450` cited below, at c47c5c7, are `publish.go:564-628` at 5ced710,
  unchanged in content.

## 1. Answer

Keep `calls` for edges an analysis can justify (by a scope, an import, a type or a signature), and stop being
silent about the rest. For every call site the linker leaves unbound, codectx should by default (a) say that the
site is open and why, and (b) offer the declarations that share the callee's name as candidates under a new
relation kind, `may_call`, with precision `heuristic` and resolution `name_only` (or `unique_name` when exactly one
compatible declaration exists). Never fold them into `calls`, and never put them in `may_refer_to`. `callers`,
`callees` and `references` list them after every precise result, ranked by closeness and paged. They are counted
exactly, and a bare "0 callers" is never returned while one exists. `impact` counts them at every level but walks
through them only when the request names `may_call`. `path` uses them only when no precise path exists, as the most
expensive hop, and says so. Do not store the name cross-product. Publish each open site once (one fact per site,
so storage grows with the source and nothing needs a cap) and work out the candidates at query time from the
existing node-name index. That way an edit in one file never changes facts stored for another, and a name with
7,000 definitions costs one count, not 7,000 stored edges. A dynamic include (`include "{$x}.php"`) follows the same
rule as an open import site with `may_import` candidates. A site bound to several justified targets (a call
through a base type) is not name-only, but if it is listed under each target, the answer must still say that
the call depends on dispatch. This matches where the field has settled. Sourcegraph, trace-mcp,
codebase-memory-mcp, code-review-graph and code-graph-rag show name-based results by default but tag each one
with its basis. code-review-graph also counts, rather than walks, the name-only sites its impact search cannot
follow. CodeQL keeps name-like callees available but outside its default call graph. GitHub dropped precise
navigation entirely in favour of name matching. The language servers examined (gopls, pyright, tsserver,
rust-analyzer) leave out what they cannot resolve and say nothing about it in the answer. For calls through a
base method, a third-pass test (Oct 9) found four servers that list them among an override's callers: clangd,
jdtls, gopls and tsserver. None marks them. clangd alone computes a "might never call" flag for them, but never
sends the flag to the client (raw reply checked). A label that does not reach the reader does no good. trace-mcp's
zero-result note and codegraph's dead-code module exist because a false zero tells an agent live code is dead.
The recommendation also respects codectx's own measurement that most call sites share a name with some
definition, so merging name matches into `calls` would make most new edges wrong (§3).

## 2. Comparison matrix

"Name candidates" means results matched on the callee's name without a scope, type or import proving the target.

| Tool | Name candidates: default | How they are labelled and kept apart | How callers / impact / dead code count them | Evidence |
|---|---|---|---|---|
| Sourcegraph (current) | **On by default as a fallback** when no precise data exists (`codeIntel.disableSearchBased: false`, "Never fall back to search-based code intelligence."). Mixing them into a precise reference list is **off by default** (`codeIntel.mixPreciseAndSearchBasedReferences: false`, "Whether to supplement precise references with search-based results."). The middle syntactic tier is on by default only on Cloud ("for cloud customers this feature is enabled by default for all repositories"). | Three provenance tiers on every API result: `PRECISE`, `SYNTACTIC`, `SEARCH_BASED`. `usagesForSymbol` results "are first grouped by provenance in the order precise, syntactic, then search-based", and search-based usages carry no symbol. The references panel shows a per-file badge, `PRECISE` or `SEARCH-BASED`, and a toggle labelled "Mix precise and search-based results" / "Hide search-based results". A file group that mixes the two keeps the precise locations and drops the search-based ones. | Find references: "may include search-based results, but only after all of the precise results have been displayed." The architecture docs say search-based results can be false positives ("references that aren't actually references") or false negatives. No dead-code feature examined. | sourcegraph/docs `admin/config/settings.mdx`, `code-navigation/precise-code-navigation.mdx`, `troubleshooting.mdx`, `admin/architecture.mdx`, `syntactic-code-navigation.mdx`. The schema is `gql/codeintel.codenav.graphql` in sourcegraph/artifacts (last commit Sep 17, 2026). Client code (`useCodeIntel.ts`, `location.ts`, `ReferencesPanel.tsx`) was read in the Sourcegraph-owned sgtest/megarepo mirror, last commit Apr 1, 2024. |
| GitHub code navigation | **Name matching only, on in every repository, no setup.** Until Dec 4, 2024, Python and TypeScript also had precise navigation (stack graphs). The docs said both were extracted automatically and users "can switch between the two"; a "search-based" link in the popover ran name matching when precise results looked wrong. | Before: two modes, each described in the docs. Precise "can give more accurate results, especially when a repository contains multiple methods or functions with the same name." Now: one mode, so nothing to label beyond the "Definition" and "Reference" tabs. | n/a | The github/docs `navigating-code-on-github.md` at 6ec89f26d9 (before) and at HEAD. Commit eac1b74c39 "Unship precise code navigation (#52258)" is dated Dec 4, 2024. The GitHub API reports `github/stack-graphs` as `archived: true`. |
| CodeQL, JavaScript | **Off by default, available on request.** `getACallee()` is `getACallee(0)`, and interprocedural data flow uses only level 0 (`FlowSteps.qll`: `f = invk.(DataFlow::InvokeNode).getACallee(0)`). For a callee reached through an indefinite global variable: same file is level 0, an externs file level 1, anywhere else level 2. Level 2 is effectively a project-wide global-name match. | An integer `imprecision` per callee: "a heuristic measure of how likely it is that `callee` is only suggested as a potential callee due to imprecise analysis of global variables". Per-site quality predicates are `isImprecise()`, `isIncomplete()` and `isUncertain()`. | Queries choose for themselves. `InconsistentNew.ql` takes the lowest imprecision and skips uncertain sites. The library guide shows how to find sites with no callee "despite the analysis being complete for this invocation", which keeps "no callee" apart from "incomplete". | github/codeql `javascript/.../dataflow/Nodes.qll`, `internal/CallGraphs.qll`, `internal/FlowSteps.qll`, `src/LanguageFeatures/InconsistentNew.ql`, `docs/.../codeql-library-for-javascript.rst`. |
| CodeQL, Python | No name-only fallback found. | Not applicable. | An unresolved call has no edge. | `DataFlowDispatch.qll` resolves calls with type trackers. Method names are looked up only along a tracked class's method resolution order (`findFunctionAccordingToMro`). |
| CodeQL, Java dead code | Not applicable. | `java/dead-function` is marked `@precision medium`. | Its help text says "a large number of false positives" means "extra entry points" are needed to "identify hidden dependencies caused by the use of a particular framework or technique". | `java/ql/src/DeadCode/DeadMethod.ql`, `DeadCodeExtraEntryPoints.inc.qhelp`. |
| Kythe | None. | `ref/call` points only at a resolved function. `ref/call/direct` is a call that "ignores indirect dispatch". A name it cannot resolve becomes a `lookup` node holding the deferred name ("in dynamic languages like Python, name resolution may depend on the runtime context"). This is a named placeholder, not a guess, much like codectx's `call:<name>` node. | Not applicable. | `kythe/docs/schema/schema.txt` (ref/call, ref/call/direct, lookup). |
| SCIP | None. | An occurrence names at most one `symbol` (optional) plus a role bitset (Definition, Import, WriteAccess, ReadAccess, Generated, Test, ForwardDefinition). The format has no call role, no candidate list and no unresolved marker. | Not applicable. | `sourcegraph/scip` `scip.proto`. |
| Glean (Meta) | **None in the open-source schemas or Glass.** | Glass builds incoming calls from the target's references (`findReferenceEntities`; `findReferenceEntitiesFast` for C++). Outgoing calls are the file's xrefs inside the function's span whose target kind is constructor, function, method or macro. Name-keyed facts are keyed by resolved names: Python `XRefViaName` says "`target` is a fully qualified name". The open-source docs index Python with scip-python; the docs I read do not say what produces `python.angle` facts. *Third pass, read and run Oct 9:* pyrefly produces them, through `pyrefly check --report-glean=…`, which Glean's `python-pyrefly` indexer drives (`glean/lang/python-pyrefly/Glean/Indexer/PythonPyrefly.hs:40-46`; the writer is pyrefly's `pyrefly/lib/report/glean/convert.rs`, read at 9855cf3 and tag 1.3.2). The scip-python path writes only `scip.*` facts (`glean/lang/scip/indexer/Glean/Indexer/SCIP.hs:82-87`). A method call's target is the receiver's static type joined with the method name (`convert.rs:529-538,744-809`). Run with pyrefly 1.3.2 and loaded into the Glean demo image (`ghcr.io/facebookincubator/glean/demo:v2024.06.10`, under podman): `def speak(a: Animal): a.sound()` gives `XRefViaName` to `animals.Animal.sound` and `CalleeToCaller {callee: animals.Animal.sound, caller: use.speak}`, with nothing toward `Dog` or `Cat`. An untyped receiver (`def untyped(x): x.sound()`) gets a made-up local name, `calls.untyped.<locals>.x.sound` (`convert.rs:823-830`), kept in `CalleeToCaller`: a named placeholder like Hack's. When `sound` is only inherited (`class Animal(Base)`), the target is the name `zoo.Animal.sound`, which has no declaration, so `codemarkup.EntityUses` of `zoo.Base.sound` returns nothing and the call is lost without a trace. Hack keeps a named placeholder: "if a method is dynamically invoked then we only know the method name not the container so we will store it as an occurrence" (`MethodOccurrence {name, className: maybe Name}`), much like Kythe's `lookup` node. *Third pass, the Hack indexer read in facebook/hhvm at cad2158* (`hphp/hack/src/typing/write_symbol_info/`, OCaml; not run, since that needs `hh_server --write-symbol-info`): for `$x->foo()` the receiver's possible types map `RIdynamic | RIerr | RIany -> UnknownClass` (`identify_symbol_service.ml:337-347`), which resolves to no declaration (`sym_def.ml:150`) and is written as `XRefTarget.Occurrence (Occurrence.Method (MethodOccurrence…))` under the comment "no symbol info - likely dynamic" (`index_xrefs.ml:289-297`), with `className: nothing` (`add_fact.ml:702-711`). Unknown classes are dropped whenever any known class is among the possible types (`identify_symbol_service.ml:327-335`), so `className: nothing` appears only when every possibility is unknown. A call on a base-typed receiver (`function speak(Animal $a) { $a->sound(); }`) points at the declared method, the class that defines it (`sym_def.ml:82-105`, `ce_origin`; test `hphp/hack/test/identify_symbol/method_inheritance.php.exp`), never at overrides. (The schema lines cited above are `hack.angle:514-522` at f7c600a.) The language-neutral layer drops it: `ConvertArgument` maps an occurrence to `nothing`, and `ConvertCallee` has no clause for one (only declaration to entity, and nothing to nothing). One Hack predicate, `ConvertCalleeForFileCall`, handles "Unresolved calls with matching resolved entities": it pairs an unresolved method occurrence with a resolved occurrence of the same method name at the same position inside the call. It is marked "**Don't use this predicate as it will go away.**" Schema `dyn.7` models "usages or references of symbols that can't be inferred via static analysis" with three states: `Unused` ("provably unused"), `Enumerated` ("possibly used but there is no guarantee") and `Used` ("provably used"). Nothing else in the repository produces or reads it. Cross-language xrefs are resolved against a second database. | No name candidates to count. Glass cuts incoming calls without saying so. Non-C++ requests default to 200 results and 2,000 ms; C++ requests to `MAXIMUM_SYMBOLS_QUERY_LIMIT = 10000` and 15,000 ms. `searchWithTimeLimit` keeps only `fst` of `search`, whose `Bool` says "whether the results were truncated", and the handler reports no truncation. | Repository at f7c600a (Oct 9, 2026): `glean/schema/source/dyn.angle:1-47`, `hack.angle:511-538`, `codemarkup.hack.angle:833-876,877-906`, `python.angle:176`; `glean/glass/Glean/Glass/Handler/Symbols.hs:840-946`, `Utils.hs:93-107`, `Handler/Documents.hs:690-767`; `glean/haxl/Haxl/DataSource/Glean.hs:93-102`; `glean/glass/if/glass.thrift:24,27`; `glean/website/docs/indexer/python-scip.md`. |
| LSP call hierarchy (spec, gopls, pyright, TypeScript, typescript-language-server) | None. | The protocol has nowhere to say "possible": `CallHierarchyIncomingCall` is `{from, fromRanges}`. gopls warns humans in its docs, not in the answer: "Dynamic calls are not included, because it is not analytically practical to detect them. So, beware that the results may not be exhaustive, and perform a References query if necessary." typescript-language-server is a pure proxy. Its call-hierarchy and references handlers forward to tsserver (`ProvideCallHierarchyIncomingCalls`, `ProvideCallHierarchyOutgoingCalls`, `References`) and only reshape the reply: `fromSpans` become `fromRanges`, a source-file item takes the file's base name, a deprecated modifier becomes `SymbolTag.Deprecated`, and definitions are dropped from references unless `includeDeclaration` is set. tsserver's behaviour therefore applies unchanged. | **Silent omission.** pyright first filters calls by name, then keeps only those whose resolved declarations equal the target, so a call on an untyped receiver resolves to nothing and drops out. TypeScript builds incoming calls from the symbol's find-all-references entries (tsserver 5.9 `getIncomingCalls` uses `FindAllReferences.findReferenceOrRenameEntries`; the Go port does the same). Both are my reading of the code, not documented behaviour. *Tested Oct 9, third pass* (pyright 1.1.414): in `def speak_u(a): a.sound()`, called as `speak_u(Dog())`, outgoing calls from `speak_u` are empty, go-to-definition on `a.sound()` returns nothing, and incoming calls on `Dog.sound` list only the typed `direct(d: Dog)`. The untyped call is dropped without a trace, as read. Calls through a base type are a different matter and are in `dispatch-calls.md`. | LSP 3.17 `callHierarchy.md`; gopls `doc/features/navigation.md`; pyright `callHierarchyProvider.ts`; TypeScript `tsc/internal/ls/callhierarchy.go` and, on branch `release-5.9`, `src/services/callHierarchy.ts:476-484`; typescript-language-server `src/lsp-server.ts`, `src/features/call-hierarchy.ts`. |
| rust-analyzer | None. | Incoming calls are the function's usages. The search is "the standard ide trick: first, we run a fast text search to get a super-set of matches. Then, we confirm each match using precise name resolution." A text match that resolution does not confirm is dropped. Asking about a trait method also returns calls bound to its impls ("is our def a trait assoc item? then we want to find all assoc items from trait impls of our trait"; test `test_trait_method_call_hierarchy` lists `S1::callee()` under `T1::callee`). References to one impl's method include only uses bound to that impl: in the references test `assoc_items_trait_impl_def`, `<()>::function` is listed and the generic `T::function` is not. Call hierarchy runs the same `usages(sema)` search, so the same holds for its incoming calls. Outgoing calls skip a method call that does not resolve (`sema.resolve_method_call(&expr)?`). For a plain call they keep only callables of kind `Function`, `TupleEnumVariant` or `TupleStruct`, so calls through a closure, a function pointer or an `Fn` value are dropped (`_ => None`). | **Silent omission.** No label and no count. *Tested Oct 9, third pass* (rust-analyzer 1.98.1, fixture `trait Animal { fn sound(&self); }`, `impl Animal for Dog`, `impl Animal for Cat`, `fn speak(a: &dyn Animal) { a.sound() }`, `fn speak_g<T: Animal>(a: &T) { a.sound() }`, `fn direct(d: &Dog) { d.sound() }`): incoming calls on the trait method `Animal::sound` return `speak`, `speak_g` and `direct`. Incoming calls on `Dog::sound` return only `direct`; on `Cat::sound` they return nothing, although `main` passes a `Cat` to both `speak` and `speak_g`. Outgoing calls from `speak` and from `speak_g` both return the trait method `Animal::sound`, never an impl; from `direct` they return `Dog::sound`. Go-to-definition on either `a.sound()` lands on the trait method; on `d.sound()` it lands on `Dog::sound`. Implementations of `Animal::sound` are both impl methods (asked on `Dog::sound`, it also returns both). No reply carries any field beyond the LSP ones. So the reading above held: the `dyn` and the generic call appear only under the trait method, and "who calls `Cat::sound`" is a bare zero. | rust-lang/rust-analyzer master, read Oct 9, 2026: `crates/ide/src/call_hierarchy.rs:60-150,672-694`, `crates/ide-db/src/search.rs:1-5,1243`, `crates/ide/src/references.rs:2414-2491`, `crates/hir/src/source_analyzer.rs:1700-1726`, `crates/hir/src/lib.rs:5822-5829` (`CallableKind`). |
| clangd | **Possible callers, on by default, for virtual methods only; their label never leaves the server.** | Incoming calls come from the index. It asks for every reference, not only calls, because "non-call references (such as address-of-function) can still be interesting as they can indicate indirect calls". For a method it also returns references to the methods it overrides, with `MightNeverCall` set. `Protocol.h:1698-1702`: "For the case of being a virtual function we also return calls to the base function. This caller might be a false positive. We currently have no way of discerning this. This is a clangd extension." But `toJSON(CallHierarchyIncomingCall)` writes only `from` and `fromRanges`, so no client sees the flag. Commit 378e4ed1ac, "[clangd] Show callers of base functions in incomingCalls (#163024)", committed Oct 20, 2025 at 10:27 PM EDT, says the extension lets "clients visualize these callers differently". Of the protocol files, its diff touched only `Protocol.h`, not `Protocol.cpp` (the rest was `XRefs.cpp`, index files and tests). Outgoing calls are the function's contained references to function-like symbols. | References for a virtual method "report decl/def of overrides and references to all overridden methods in complete type hierarchy". In a template, a dependent call is resolved by `HeuristicResolver`, which assumes "template instantiations are based on the primary template definition" ("results may not be fully accurate but that this is better than nothing"). The indexer records the reference only when exactly one target results ("FIXME: Improve overload handling"), and does not label it. Go-to-definition has a textual fallback for a word that is not a resolved identifier, or is a dependent name. It keeps only exact-name index matches ("Only consider exact name matches, including case. This is to avoid too many false positives"), fetches at most 10, and returns nothing when more than 5 match ("If we have more than 5 results, don't return anything, as confidence is too low"). A macro cannot be a call-hierarchy item: `prepareCallHierarchy` accepts functions, methods, function templates, non-local variables, fields and enum constants. *Tested Oct 9, third pass* (clangd 22.1.6, with a `compile_commands.json`). **C:** in `g() { void (*fp)(void) = f; fp(); }`, incoming calls on `f` list `g`, but the range is the `= f` assignment, not the `fp()` call; `k() { run(f); f(); }` is listed with both `run(f)` and `f()` as call ranges. The local `fp` and the parameter `cb` of `run(void (*cb)(void))` are not call-hierarchy items (`prepareCallHierarchy` returns `[]`), so the `fp()` and `cb()` calls reach nothing. A global `void (*gfp)(void) = h;` is an item: its incoming calls are `g2` (the `gfp()` call), its outgoing call is `h`, and incoming calls on `h` list the variable `gfp` as the caller. Outgoing calls from `g` list `f` (through the assignment); from `run` they list nothing. **C++:** incoming calls on `Dog::sound` return `direct` (`d.sound()`) and `speak` (`a.sound()` on an `Animal&`); on `Cat::sound`, `speak`; on `Animal::sound`, only `speak`. The raw reply for `Dog::sound` is `[{"from": {...}, "fromRanges": [...]}, ...]`: no `mightNeverCall` or any other added field reaches the JSON (the item's `data` is clangd's symbol id, LSP's round-trip field). In clangd 22.1.6's own source (commit fc4aad7b), `toJSON(CallHierarchyIncomingCall)` writes only `from` and `fromRanges` (`Protocol.cpp:1515-1517`) while the struct keeps `mightNeverCall` (`Protocol.h:1631-1635`). Two more facts from a three-level fixture (`Animal` → `Dog` → `Puppy`): the base step goes up one level only (incoming calls on `Puppy::sound` return `walk(Dog&)` but not `speak(Animal&)`; `XRefs.cpp:2435-2443` at that commit asks the index for the methods `Puppy::sound` directly overrides), and a qualified non-virtual call `a.Animal::sound()` is listed among the callers of `Dog::sound` though it can never run it. | llvm/llvm-project main, read Oct 9, 2026: `clang-tools-extra/clangd/XRefs.cpp` (`locateSymbolTextually` 672-765, `findReferences` 1740-1750, `prepareCallHierarchy` 2547, `incomingCalls` 2573-2631, `outgoingCalls` 2655-2695), `clangd/Protocol.h:1698-1702`, `clangd/Protocol.cpp:1540-1542`, `clang/lib/Index/IndexBody.cpp:186-195` (`indexDependentReference`), `clang/include/clang/Sema/HeuristicResolver.h:36-45`. |
| Joern | Not by bare name. | An unresolved call keeps a placeholder full name, `<unresolvedNamespace>.foo:<unresolvedSignature>(1)`, which records the name and argument count. A dynamic JS call is `<unknownFullName>`. Type-hint linkers join on recovered type-qualified full names and create stub methods for names missing from the graph. | Not applicable. | `x2cpg/Defines.scala`, `XTypeHintCallLinker.scala`. |
| Aider repo map | **Always on: everything is name matching**, but used only to rank context. | A definition and a reference are linked when their identifiers are equal. PageRank then weights files. An identifier defined in more than 5 files gets ×0.1, a leading `_` ×0.1, a name the user mentioned ×10. | Never answers "who calls this". | `aider/repomap.py` (lines ~440–560). |
| RepoGraph (ICLR 2025) | **Always on: every reference with a name links to every definition with that name.** | No labels. | Used only as retrieval context for an agent. | `repograph/construct_graph.py` lines 107–112; paper §3.1. |
| codebase-memory-mcp | **On by default, inside the one `CALLS` kind.** Strategies and confidences: `import_map` 0.95, `same_module` 0.90, `import_map_suffix` 0.85, `unique_name` 0.75, `suffix_match` 0.55. Fuzzy 0.40 / 0.30 is "not wired into the sequential/parallel resolvers". | Each edge carries `{callee, confidence, strategy, candidates}`, and a confidence maps to a band: `high` ≥0.7, `medium` ≥0.45, `speculative` ≥0.25. Confidence is scaled down as the candidate count grows. Names with more than 256 definitions are not resolved at all. For member calls whose receiver could not be typed (Python and the JS/TS family) and for Python bare calls shadowed by a parameter, the weak strategies (`suffix_match`, `unique_name`, `field_type_hint`) are dropped, because they "fabricate a CALLS edge" (`re.test()` -> SalesforceRestClient.test). | Dead code means "functions with zero callers, excluding entry points". It adds synthetic edges for FastAPI `Depends()`, because "Without this, FastAPI auth/DI functions appear as dead code". Its fast agent tier makes "no absence, exhaustive-impact, or dead-code claims", and "A clean coverage result means only 'no recorded gap,' never proof of completeness." | `src/pipeline/registry.c`, `src/pipeline/pass_calls.c`, `README.md`. |
| trace-mcp | **On by default.** The call graph includes every tier. | Every edge carries `resolution_tier`: `scip_resolved` > `lsp_resolved` > `ast_resolved` > `ast_inferred` > `text_matched`, scored 1.0 / 1.0 / 0.95 / 0.7 / 0.4. Answers include per-tier counts. When paging, "a tool that pages its results cuts the fuzzy end rather than an arbitrary one". | **A zero is never bare.** "`total: 0` is ambiguous: it can mean 'this symbol genuinely has no callers', or 'the index cannot see them here'." Usage, caller and impact answers attach a note built from the index's own data: file not indexed, index behind the file, no call edges for the language, too few edges, or under 50% of the language's call edges resolved. | `src/db/confidence.ts`, `src/tools/shared/resolution.ts`, `src/tools/shared/empty-note.ts`, `src/tools/framework/call-graph.ts`. |
| codegraph (colbymchenry) | **On by default for the single best name match.** The name matcher picks the highest-confidence candidate. | Edges carry `metadata.confidence` and `resolvedBy`. Bridge hops it synthesizes are `provenance: 'heuristic'`. A reference it cannot follow is kept in `unresolved_refs` with its name and a `candidates` JSON list. | **Dead code is subtractive and counted.** A symbol is not "dead" if "the index holds an unresolved reference to its name", if "another symbol of the same name IS referenced", or if "its own file writes the name more than once". Every exclusion is counted and shown. The scan has a fixed cap, `MAX_DEAD_CODE_CANDIDATES = 20000`, and says when it stopped there. | `src/graph/dead-code.ts`, `src/db/schema.sql`, `src/resolution/index.ts`, `README.md`. |
| graphify | **Conservative.** A cross-file bare call binds only when one candidate survives the tie-breakers (prefer non-test, then path proximity). Member calls are skipped. | Edges are tagged `EXTRACTED` (confidence 1.0), `INFERRED` or `AMBIGUOUS`. Unresolved member calls are parked as `metadata.unresolved_calls` (names only). "An ambiguous name still fabricates nothing." | Not examined. | `graphify/symbol_resolution.py`, `graphify/cross_repo_calls.py`, `docs/how-it-works.md`, `README.md`. |
| code-review-graph | **On by default, labelled per row.** `query_graph` and impact take `resolution`, default `"all"`. | A `CALLS` edge the parser cannot bind keeps the bare callee name as its target. A post-build pass binds it only when the candidate is in the call-site file or in exactly one file that file imports: "A globally unique name is not sufficient evidence: unrelated repositories often contain one matching helper by coincidence." Every `CALLS` and `REFERENCES` edge is classified in an indexed column, `target_resolution`: `direct` when the target names an indexed node, else `unresolved`. `callers_of` stages the bound edges, then adds edges whose bare target equals the node's name, each marked `target_resolution: "unresolved"`. The fallback is restricted to the node's language family and skips edges carrying `ambiguous_targets` or `unresolved_targets`, C++ overload sets, and receivers whose recorded binding rules the node out. Rows are then interleaved so that every caller appears once before any repeat. `callees_of` lists a bare target by name, marked `target_resolution: "unresolved"`, with no candidate list. Only when a resolver pass recorded `ambiguous_targets` or `unresolved_targets` on the edge (the C++ scoped-call pass does) does the row add `candidates` (the first 20), the exact `candidate_count` and `candidates_truncated`. | Every call answer carries `resolution_split: {direct, unresolved}`. Impact never follows a bare-name hop. It reports `unresolved_call_sites` for the changed symbols' names, "an upper bound on missed impact, not a claim that every one of these is a real call". An empty answer gets one capped note: not indexed, stale, or a known gap for the language (for example "python getattr dispatch and decorator-based registration are not statically traced"). Two fixed rules: `callers_of` on a common builtin name returns zero with a "skipped to avoid noise" summary, and `references_to` uses the name fallback only when exactly one node has that name. **Correction to the internal survey:** its EXTRACTED / INFERRED / AMBIGUOUS triple comes from a CHANGELOG entry. The code writes only `EXTRACTED` (the column default) and `INFERRED` (`scoped_resolver.py`, `event_resolver.py:107`); `AMBIGUOUS` is never assigned. Ambiguity lives in `extra.ambiguous_targets`. | tirth8205/code-review-graph, branch `staging` at 6b12d11 (Sept 18, 2026): `code_review_graph/graph.py:233-250,268-276,1225-1262,1704-1815,1876-2081,2713-2742`, `migrations.py:425-478`, `tools/query.py:204-232,318-327,433-541,641-790,1013-1101`, `tools/_common.py:136-138`, `uncertainty.py:1-21,57-167,343-376`, `scoped_resolver.py:1-45`, `CHANGELOG.md:1058`. |
| code-graph-rag | **On by default for the single best name match, stored as a real `CALLS` edge labelled `heuristic`.** | Every `CALLS`, `REFERENCES` and `INSTANTIATES` edge carries `resolution`: `exact` ("scope, import, type or signature"), `overload` ("one edge per same-named candidate"), `heuristic` ("name-only: trie suffix, wildcard import, package member"), `trace_confirmed` or `dynamic` (from runtime traces). The resolver's last stage, `_try_resolve_via_trie`, takes definitions ending in the call's last name segment and filters them: same language family; no method for a bare Rust name or for a bare Python name from another module; classes only for `new X`. It keeps one, preferring a concrete definition over an abstract one, then import distance, then name. Some sites bind every match instead. A SQL call to a routine that several schemas or files define gets a `CALLS` edge to each (`heuristic`). A C function name stored as a value (a callback or ops-table entry) that the file does not define gets a `REFERENCES` edge to every externally linked function of that name (`heuristic`). A C++ call through a template-parameter receiver goes to the method on every class that defines it. *Tested Oct 9, third pass* (commit 5f76ace, Python 3.12, tree-sitter C++ mode; libclang was not installed, so its C++ mode did not run). On `template <typename T> void speak_t(T& a) { a.sound(); }` with `Dog::sound` and `Cat::sound`, both stored edges `speak_t → Dog::sound` and `speak_t → Cat::sound` carry `resolution: exact`. The stored graph was read twice and matched: from the `cgr index -o` protobuf file and from a real Memgraph 3.3.0 database (`cgr start --update-graph --clean`, then a Cypher read). The path there is not the one the reading above described. The tool walks `speak_t`'s body twice. On the first walk `a` has no type, so name matching picks one method, `Cat::sound`, labelled `heuristic`. On the second walk `a` is typed `T` and the fan-out writes both edges as `exact`, replacing the `Cat::sound` edge's label because the key (caller, callee, line, column) is the same. So the stored label is `exact` only because the second write wins; a captured emit log shows the `heuristic` write. On a virtual call (`void speak(Animal& a) { a.sound(); }`) and on the Python `def speak(a: Animal): a.sound()`, it stores one `CALLS` edge to `Animal::sound` (`exact`) plus `OVERRIDES` edges `Dog::sound → Animal::sound` and `Cat::sound → Animal::sound`, and no edge from `speak` to either override. | `callers` and `callees` return every edge with its `resolution`, unfiltered. Only `cgr dead-code --min-resolution` sets a floor ("heuristic < overload < exact < trace_confirmed"), default none. A call no stage binds gets **no `CALLS` edge to any callee and no count** in any answer. Its module records the name in `unresolved_references`, so that a file defining it later triggers a re-parse. Two side effects remain: a JS/TS `this.M()` inside a class method emits self-dispatch `CALLS` edges to `M` on the enclosing class, and first-party functions passed as arguments to the unbound call get edges from the caller (`CALLS` in flow-traced languages, `REFERENCES` otherwise), so they stay reachable. The internal survey's "simple-name fallback" is accurate, but its line numbers (~1087, 1298, 1408) have moved, and its ~1087 quote ("Full-fallback by design") belongs to the C++ template fan-out, not the trie fallback. | vitali87/code-graph-rag main at 5f76ace (Oct 9, 2026 at 7:51 PM EDT): `codebase_rag/constants/graph.py:87-113`, `parsers/call_resolver.py:1410-1450,1506-1583,3734-3964`, `parsers/call_processor.py:1887-1918,4785-4796,4980-4990,4840-4875,5043-5070,4745-4760,6116-6160,7541-7604,9404-9416`, `parsers/import_processor.py:4037-4042`, `dead_code.py:57-74`, `cli_help.py:530-533`, `graph_query.py:464-484`, `cypher_queries.py:1064-1071`, `docs/architecture/graph-schema.md:100-121`. |
| GitNexus | **On by default.** `impact` takes `minConfidence` with "default: 0", so every edge counts. | Each edge prints its confidence (`[CALLS 90%]`). When several symbols share the *target's* name, it returns "a ranked `ambiguous` candidate list instead of guessing". | Impact includes every confidence. Its PDG mode promises "never a false-safe zero". | `gitnexus/src/mcp/tools.ts`, `README.md`. |
| Serena | None. Results come from a language server or JetBrains only. | Regex search is a separate tool, `search_for_pattern`. | "Safe delete" deletes "if there are no references to it", going by the backend's references, so a dynamic call the server cannot see does not block a delete. That is my inference from the tool contract, not something I tested. | `README.md`, `src/serena/tools/symbol_tools.py`. |
| **codectx today** | **None, by rule.** "No binding is justified by the callee's name alone" (the linker change's `docs/providers-link.md`). A site the linker cannot bind "publishes nothing here". | An unbound site keeps the structural placeholder `call:<name>` (`resolution` is `unresolved` or `import`). A site bound to two or more justified targets publishes `calls` to an ambiguous `link:` node, plus `may_refer_to` to each candidate. | **Silent today, and the problem is wider than name matching** (§5.1). Default walks leave out `may_refer_to`, so `callers` of a class-hierarchy target misses every polymorphic site that reaches it. No dead-code answer exists. | the linker change's `internal/provider/link/publish.go:407-450`; main `internal/graph/cost.go:64-78`, `internal/cli/query.go:219`. |

## 3. Measured trade-offs

| Source | What was measured | Numbers (as published) | What it means here |
|---|---|---|---|
| PyCG (Salis et al., ICSE 2021), Table IV | Precision and recall against hand-built call graphs of 5 Python projects | PyCG **99.2% precision, 69.9% recall** on average. Pyan 74.4% / 20.6%. Depends 98.7% / 14.4%. On the micro suite PyCG has no false positives in 111 of 112 tests and misses nothing in 103 of 112. | A careful static resolver still misses about 30% of real edges. Choosing to drop doubtful edges (Depends) buys precision at the cost of very low recall. Edges PyCG cannot resolve (calls on built-in types, `getattr`) are simply left out. |
| Jarvis (Huang et al., arXiv 2305.05949v5), Table V | Same idea, scored per call *chain* from entry functions | PyCG 0.99 precision / 0.76 recall on whole edge sets. In application-centred whole-program mode, scored by chains, PyCG drops to **0.19 precision** and Jarvis gets **0.35 / 0.60**. "If there exists one false positive call relation, the subsequent call relations along the generated call chain are all considered as false positives." | A wrong edge poisons every path through it. That is why name candidates must not be walked transitively in `impact` or `path` by default. |
| ACG (Feldthaus et al., ICSE 2013) | Field-based JS call graphs (one abstract location per property name, which is close to matching member calls by name) against dynamic call graphs | Per-call-site precision **at least 80% on jQuery programs, 65–75% on the two non-jQuery programs** (the intro says "≥ 66%"). Recall above 80% everywhere, above 90% for jQuery programs, "close to 100%" without frameworks. The main imprecision is "functions that are stored in properties of the same name". Naive name matching (Eclipse JSDT, "resolve a call e.f(...) to all functions named f") "fails when functions are passed as parameters". | Name-keyed member resolution can be acceptably precise on small apps. Its precision falls as same-named members multiply. The authors chose zero targets over spurious ones for IDE jump-to-definition ("arguably better than listing many spurious targets"), which is a judgement for humans, not agents. |
| Jelly with approximate interpretation (Laursen, Xu, Møller, PLDI 2024) | Recovering edges a precise-leaning JS analysis misses | 141 projects: **55.1% more call edges, 17.7% more resolved call sites, 1.5% fewer monomorphic sites**. On 36 projects with dynamic call graphs, recall **75.9% → 88.1%**, precision down 1.5%. One case went from 40.1% to 98.0%. | Even a state-of-the-art analyzer misses about a quarter of real edges. Well-targeted recovery buys recall cheaply. |
| "Call Me Maybe" / Graphia (Bhuiyan et al., arXiv 2506.18191, 2025) | Ranking candidate targets for unresolved JS sites with a graph neural network, over every function in the project | CodeQL configured "to produce precise but incomplete call graphs" left **60% of call sites unresolved**. The model ranks the correct target first in **over 42%** of unresolved cases and in the **top 5 in 72%**. The authors "advocate for a human-in-the-loop approach where analysts vet the ranked list of candidate function definitions". | The open-site problem is large, ranking matters, and the authors present results as a ranked candidate list, not as edges. |
| RepoGraph (Ouyang et al., ICLR 2025) | An agent using a graph built purely by name matching | SWE-bench Lite resolve rate rose from **27.33 to 29.67** (Agentless + GPT-4o). The graph averages **1,419.3 nodes and 26,392.1 edges**. | Even a crude name graph helps an agent find context. It was used for retrieval, not to answer "who calls this". |
| codebase-memory-mcp (vendor comments in code) | Cost and accuracy of name strategies | Linux kernel: "274 names exceed 256 candidates (`list_head` 7188, ...)" and took "~900 s of the 987 s usage-resolution CPU". Django A/B test: an unconditional weak-member guard "dropped 800 such self-rooted edges, every sampled one a real call". | Name matching costs most where it tells you least, and suppressing name edges throws away real calls. Both numbers are vendor-stated and not independently checked. |
| codectx's own research (main `docs/research/20-native-engine-post-mvp.md` §7; ADR-0012 call-graph row) | How common name collisions are on the reference repository | **70.59%** of all call sites carry a callee name that some tracked definition also carries. Among the 82.8% the syntax tier leaves unresolved, the figure is **65.68%**. The techniques reach **90.07%** of in-repo-targeted sites; class-hierarchy analysis adds **8.66 pp** and leaves a **1.27%** residue. | "Wrong on most" for a name-only resolver is the doc's own inference from the gap to the engine's 18.7% full-name join, not a precision measurement against ground truth. If the linker reaches the measured ceiling, the open-site tier is small in volume. But name matching would also fire on sites whose true target is a library (the `re.test()` case), and those are exactly its false positives. |

## 4. Recommendation for codectx

### 4.1 Default

**On, as disclosure plus labelled candidates, never merged.** Requirements: "complete answers by default,
nothing for the user to tune, never silent about what it could not do". An empty `callers` answer while a call
site names the symbol would be silent. Turning name matches into `calls` would make the graph mostly wrong (§3).
So the default answer is the precise answer plus a complete, labelled, counted list of name-only candidates.

### 4.2 Relation kind: a new `may_call`, not `may_refer_to`

Do not reuse `may_refer_to`:
- It is already an identity-reconciliation edge. `cost.go:37-46,64-68` costs it as "the weakest evidence of a
  dependency there is" and keeps it out of every default allowlist, because "walking them by default would report
  a symbol's own former name as something it depends on". The resolver uses it for alias alternatives
  (`docs/providers.md:216`, `docs/providers-treesitter.md:203`, `internal/provider/scip/importer.go:1449,1696`).
- It also carries the in-file ambiguous candidates (capped at 16, `model.MaxAmbiguousCandidates`) and the linker's
  ambiguous candidates (uncapped). Each of those is justified by a scope, type or import, per the linker's rule.
  Name-only candidates would be a fourth meaning that breaks that rule.
- Graph answers return `Relation{ID, From, Kind, To}` and no evidence (`internal/model/query.go:652-660`).
  `docs/providers-dependence.md` states the consequence: "an invented callee reads as a real dependency of the
  source" unless the answer itself says so. Only the **kind** reaches the agent, so the basis must live in the kind.

Name: **`may_call`**, parallel to `may_refer_to`, with the basis carried in `resolution`. If the owner prefers the
basis in the kind itself, `calls_by_name` reads more plainly to an agent. Either way it is one new `RelationKind`.

### 4.3 Classify every call site by what justifies it

| Basis | Example | What codectx publishes or answers |
|---|---|---|
| One justified target | direct, import, scope, a unique class-hierarchy target | `calls` (as today) |
| Several justified targets | `animal.sound()` reaching overrides; a flow union; overloads in scope; the signature-keyed indirect join | **Proposed:** each target is reached by default walks, and the answer says the call goes through dispatch. Today it is `calls` → `link:` plus `may_refer_to`, which hides these callers (§5.1). *Added in the second pass:* the first version proposed plain `calls` to each target, with the `link:` node's `resolution: ambiguous, candidates: n` kept for disclosure. That metadata does not reach the answer (`Relation{ID, From, Kind, To}`, §4.2). clangd shows the failure: it returns base-method callers flagged "might be a false positive" and then drops the flag when it writes the reply. Two ways to keep the fact: (a) a distinct kind, walked by default like `calls` (for example `calls_via_dispatch`); (b) `calls` plus a structured per-answer count of dispatch-bound sites and a notice. (a) follows §4.2's rule that the basis lives in the kind. This is the owner's call. |
| Name only | `obj.handle()` with no recovered receiver type; a call through a value flow never reached; a JS global no module declares | An **open site** fact, plus `may_call` candidates worked out at query time |
| No name to match | `obj[k]()`, `getattr(o, k)()` with a non-literal `k` | An open site with reason `computed`, counted, no candidates |
| Proven outside the project | `fmt.Println`, an import naming no module of the unit, a receiver typed as a library or builtin type, a bare name bound to a parameter | Nothing new. The placeholder stays (`import` or `unresolved`). Counted as `external`, not as a gap. |

### 4.4 What is published, and what is computed per query

- **At index time, the linker publishes one open-site node per name-only site.** The shape follows the ambiguous
  family it already mints: scope `file:<path>`, native key `link:<call-site key>`, located at the site, `Name` = the
  callee name, kind `method` for a member call and `function` otherwise. Metadata:
  `{"resolution":"unresolved","reason":"receiver_untyped|value_untracked|global_unbound|computed","callee":"<name>","shape":"bare|member|value|construct","args":<n or -1>}`.
  It also publishes a `calls` edge from the caller to that node, as for an ambiguous site. One fact per site means
  storage grows with the source, and the facts are republished with their file like every other linker fact.
- **At query time, candidates of an open site** are the declarations of the same generation that:
  - have the same name, compared as the language compares names;
  - fit the site's shape: a member site matches methods (and the function-valued fields field-based resolution
    already reads), a bare or value site matches functions, a construction matches classes;
  - belong to the same language family;
  - fit the argument count where the linker already applies `fits`, in the static families only. Do not apply it
    to JavaScript: ACG notes that "JavaScript allows arity mismatching".

  These filters rest on facts, not fixed counts. The lookup uses the existing `idx_nodes_name ON node_facts(name,
  unit_id, node_id)` (main `internal/storage/sqlite/schema.sql:558`).
- **Why query time.** Storing the cross product costs, for each site, as many edges as there are same-name
  definitions. A hub name multiplies that: codebase-memory-mcp measured `list_head` at 7,188 definitions on Linux.
  The no-cap rule forbids trimming it. Stored candidates would also break the linker's fast path, because a new
  `handle` in file B would change facts stored for file A. Query-time candidates have neither problem.
  Prior art from the second pass: rust-analyzer's find-usages takes a text search for "a super-set of matches"
  and then confirms each one by name resolution. This is the same pipeline minus the confirm step, which codectx
  cannot run on an open site, so it labels instead of dropping. code-review-graph also matches its bare-name
  fallback at read time, not at index time. code-graph-rag stores one best name match per site as a `heuristic`
  edge at index time, which is the stored alternative this section rejects.

### 4.5 Exact labels

- Relation kind `may_call` (a new value in `model.RelationKind`, Section 9.2), from the **caller** (the
  declaration enclosing the site, or the file module) to each candidate. Its id is derived the way every relation
  id is (caller, kind, target), so several sites in one caller are one relation.
- `resolution`: `name_only`, or `unique_name` when exactly one compatible declaration exists in the generation.
  Both values already exist in the Section 9.3 vocabulary (`docs/implementation-plan.md:640`) except `name_only`,
  which this would add. `unique_name` still means "may": a library method of the same name is invisible to the
  index, which is codebase-memory-mcp's `re.test()` lesson.
- Precision `heuristic` on every occurrence `codectx_references` returns for a `may_call` site. The tier exists
  already, and the context compiler weights it at 450,000 against `static_analysis` at 900,000
  (`internal/context/compiler.go:1086-1092`).
- Ordering is deterministic: every precise result first, then candidates by closeness. Same file first, then the
  same directory or package, then a module the site's file imports (or that imports the candidate's file), then
  the same unit, then elsewhere. Within each step, sites whose name has fewer compatible definitions come first,
  then a stable key. Prior art: graphify's non-test and path-proximity tie-breakers, codebase-memory-mcp's
  import-distance scoring, trace-mcp's "cut the fuzzy end".

### 4.6 Per operation

- **`callers`.** Precise incoming `calls` as today. For every node the walk visits, it also lists the incoming
  `may_call` relations one hop out, but never walks through them. A request whose `relations` names `may_call`
  walks through them under the same depth, visited, edge and deadline budgets. When precise callers are zero and
  candidates exist, the answer says so instead of returning a bare zero.
- **`callees`.** Precise `calls`, plus the `calls` edge to each open-site node (its metadata gives the reason),
  plus `may_call` to that site's candidates, ranked and paged.
- **`references`** (`internal/graph/references.go:69` reads `references` and `calls`). Add the `may_call` sites
  as occurrences with precision `heuristic`, after the precise ones, as Sourcegraph does.
- **`path`.** Search precise kinds first. Only if no path exists, search again with `may_call` admitted at the
  maximum cost, 7, the same as `may_refer_to`. Return any route found as a possible path whose `may_call` hops are
  visible in `Relations`, with a notice.
- **`impact`.** Expand `ImpactRelations` as today. At each level, count the open sites whose candidates include a
  frontier node and report them as possibly affected: a count, the packages they fall in, and a cursor that lists
  them. Do not expand them unless the request names `may_call`. Jarvis's chain result is the reason.
  code-review-graph already does this for the changed symbols: its impact walk never follows a bare-name hop and
  reports `unresolved_call_sites`, "an upper bound on missed impact, not a claim that every one of these is a real
  call". This proposal counts at every level, not only for the changed symbols.
- **Context plans** (`internal/context/scope.go:31-40`, `compiler.go:1061-1083`). Keep `may_call` out of
  `scopeRelations` and out of required scope. Admit candidates as ranked optional context with the lowest
  dependency weight. For example, below `imports` at 600,000, with the heuristic precision multiplier on top; the
  exact weight is the owner's choice. State in the plan how many candidates were left out of required scope.
- **Dead code.** codectx has none today (`rg` for dead/unused in `internal/mcpserver` and `internal/cli` finds
  only deadlines). If it ever adds one, adopt codegraph's subtractive rule: a symbol with any open site naming it
  is "no precise caller", never "dead", and every exclusion is counted. Glean's `dyn` schema names the same three
  states: `Unused` ("provably unused"), `Enumerated` ("possibly used but there is no guarantee") and `Used`
  ("provably used"). code-graph-rag's `--min-resolution` floor (default none) shows the alternative of a knob,
  which the requirement of nothing to tune rules out.

### 4.7 Disclosure

- **Unit-level counts on the linker's `calls` capability row.** Add `sites`, `bound_sites`, `open_sites`,
  `computed_sites` and `external_sites` beside the existing `members`, `members_without_table` and
  `dropped_table_entries`. These are exact facts. Do not use a threshold; trace-mcp's "below 50%" is a fixed cut
  this project would avoid.
- **Answer-level structured counts, plus a notice.** `QueryMeta.Notices` exists
  (`internal/model/query.go:101-109`). A structured count is better for the external consumer; since the project
  has no compatibility constraints, adding one costs nothing. code-review-graph ships one: every call answer has
  `resolution_split: {direct, unresolved}`. Notice wording:
  - "`may_call`: 3 call sites in 2 callers name `handle` and no analysis bound them. They are listed by name only
    and may not call this symbol."
  - Zero case: "No caller found. In this project 8,412 of 9,950 call sites are bound; 1,538 are open, and none of
    them names `handle`." This states the facts and lets the reader judge.
  - Impact: "Level 2: 14 open call sites name a symbol at this level. Listed as possibly affected; not expanded."
- **Tool descriptions** (`internal/mcpserver/registry.go:48-58`) should say in one sentence that `may_call` is
  name only. Agents read descriptions.

### 4.8 Dynamic includes (PHP `include "{$x}.php"`, future work) under the same rule

An import site whose path the analysis cannot fold to a constant becomes an **open import site**: a node with
`{"resolution":"unresolved","reason":"computed_path","pattern":"<literal fragments, e.g. views/*.php>"}` and the
`imports` edge to it. Candidates are worked out at query time as the files whose root-relative path matches the
literal fragments. They are published as **`may_import`** with precision `heuristic`, ranked by path closeness,
paged and counted, the same way as `may_call`. A path built from constants and `__DIR__` folds to a precise
`imports` edge. A pattern with no literal fragment is an open site with no candidates, counted. The same shape
covers `require(expr)` and `import(expr)` in JavaScript, `importlib.import_module(expr)` in Python, and
`#include MACRO` in C when the macro is not defined in the file.

### 4.9 Limits that are not fixed counts

- Open-site facts: one per site, no cap. They grow with the source.
- Candidate lists are ranked lists: top-k pages with a cursor and an **exact total**, so a reader can page through
  every candidate. The caller's page limit sets the page size, nothing more.
- No bail-out above N definitions (codebase-memory-mcp stops at 256). A name with 7,188 definitions reports
  `candidates: 7188` and a ranked first page. The count itself tells the agent the match carries little meaning.
- Counting is linear in the matching sites. If the query deadline stops a count, the answer says "at least n"
  under the existing `Truncated` and `TruncationReason` contract (`internal/model/query.go:92-97`), not a silent
  partial number.
- The external recall-first consumer's "every same-name definition, capped at 25" becomes `may_call` pages it reads
  to the end, ordered by closeness, with nothing cut.
- Do not copy the fixed cuts the second pass found. code-review-graph returns zero callers for a list of builtin
  names (with a summary that says so) and lists 20 candidates (with the exact count). clangd's textual fallback
  returns nothing above 5 matches. Glass defaults incoming calls to 200 results and 2,000 ms and discards the
  truncation bit, so a cut answer looks complete.

## 5. Risks and open questions for the owner

1. **A silent undercount exists today for justified multi-target sites (verified in code).**
   - In the linker change's `publish.go:407-450`, a site with one target publishes `calls` to it. A site with two or more
     publishes `calls` to a `link:` node and `may_refer_to` to each target.
   - `may_refer_to` is excluded from `DefaultRelations` (`cost.go:64-78`), `ImpactRelations` (`cost.go:83-89`) and
     `scopeRelations` (`scope.go:31-40`). The CLI pins `calls` (`cli/query.go:219`). Nothing in `internal/graph`,
     `internal/app`, `internal/mcpserver`, `internal/cli` or `internal/context` expands it, on main or the linker change.
   - Result: `callers(Dog.sound)` omits every `animal.sound()` site that class-hierarchy analysis bound to it
     together with other overrides. Impact and context plans omit them too.
   - This is the "false 0 callers" problem for *justified* edges, and it is larger in scope than name matching. I
     recommend that default walks reach each justified target (§4.3), which is the owner's decision.
   - *Second pass:* plain `calls` per target would fix the undercount but hide that the call depends on dispatch.
     clangd is a working example of exactly that: `incomingCalls` sets `MightNeverCall` for calls through a base
     method, and `toJSON` drops it. §4.3 now gives two ways to keep the fact in the answer.
2. **The MCP and CLI callers differ (follow-up).** `codectx_callers` and `codectx_callees` with no `relations` walk
   all 20 `DefaultRelations` kinds, including `contains`, `defines` and `references` (`mcpserver/graph.go:117-121`
   → `graph/traverse.go:1075-1077`). The CLI walks `calls` only. Every rule above assumes the baseline is `calls`.
3. **The in-file candidate cap (follow-up).** The structural provider keeps at most `MaxAmbiguousCandidates = 16`
   in-file `may_refer_to` candidates and counts the rest as dropped (`model/validate.go:68`). That is disclosed,
   but it is a fixed count on generated data. Also, in-file candidates of a *member* call on an untyped receiver
   are name-only within the file, so under §4.3 they belong in `may_call`.
4. **Precision of the name tier is unmeasured for codectx.** The 70.59% figure measures how common collisions are,
   not whether candidates are correct. Before shipping, measure `may_call` precision and recall per language
   against compiler-grade ground truth on the fixtures, and publish the numbers in the provider doc. ACG's 65–84%
   on small JS apps and codectx's collision rate pull in opposite directions.
5. **Agents may still read `may_call` as a call.** The distinct kind, tool text and notices reduce the risk; they
   do not remove it. Watch for agents citing `may_call` as proof.
6. **Query cost for hub names.** Counting is one index range; listing is a top-k selection. A `callers` walk that
   lists the one-hop `may_call` fringe for every visited node makes one name lookup per node, bounded by the
   visited budget.
7. **Should candidates cross units?** A site in project A that names a method defined only in unrelated project B.
   I recommend ranking them lower rather than filtering them out. A filter on unit dependencies could be justified
   by facts, but monorepos and dynamic loading make it uncertain.
8. **Two edges per open site in `callees`.** The structural `call:<name>` placeholder plus the linker's open-site
   node, as for ambiguous sites today. Decide whether the linker's node replaces the placeholder in answers.
9. **Should `unique_name` ever become `calls`?** The evidence says no: codebase-memory-mcp had to suppress it for
   untyped receivers because library methods are invisible. code-review-graph refuses it outright: "A globally
   unique name is not sufficient evidence: unrelated repositories often contain one matching helper by
   coincidence." Yet codebase-memory-mcp's Django A/B lost 800 real self-rooted edges when it suppressed them. codectx's flow inference may already type `self.x.y()` chains, so its leftover set may differ. Measure
   before deciding.
10. **Notice or structured field.** Notices exist today. A structured `may_call` count is new wire shape but is what
    the external consumer and agents can rely on.

## 6. Sources

Every entry was read in this session. Where a claim rests on my reading of
code, or on inference without a test, the row says so.

**Still unconfirmed after the third pass.** Each item says what was tried and what a test would need.
- Glean, Hack: the indexer was read, not run. A run needs `hh_server DIR --write-symbol-info JSON`
  (`glean/website/docs/indexer/hack.md:40`); the Glean demo image does not include hh_server, and no Hack binary is
  installed here.
- Glean, Python: one oddity is unexplained. Querying `python.DeclarationUses { declaration = { func = { name = … } } }`
  directly returned nothing, while `codemarkup.EntityUses`, which is built on it, returned the expected spans. The
  image was removed before the cause was found. The case where the receiver type and the defining class sit in
  different modules (`convert.rs:551-556`) was outside the runs.
- code-graph-rag: its libclang C++ mode (needs the project's `cpp` extra and a `compile_commands.json`) and its jedi
  Python mode (needs jedi in the venv) were outside the runs; only the default tree-sitter modes were. Whether the first,
  untyped walk can leave a stray `heuristic` edge in other shapes (for example, when a free function is also named
  `sound`) was not tested.
- Everything else the second pass marked inferred in the rust-analyzer, clangd, code-graph-rag and Glean rows is
  now tested or read at source.

**Hands-on runs (third pass, Oct 9).** rust-analyzer 1.98.1, clangd 22.1.6, gopls v0.23.0, pyright 1.1.414,
typescript-language-server 6.0.2 with TypeScript 6.0.3, TypeScript 7.0.2 native, jdtls 1.61.0 (JDK 21.0.12),
driven by `dispatch-probe/scripts/lspc.py`; code-graph-rag 5f76ace with Memgraph 3.3.0 under podman; pyrefly 1.3.2,
scip-python 0.6.6 and the Glean demo image under podman; CodeQL CLI 2.27.2. Fixtures, transcripts and outputs:
`probe/` (index in `dispatch-calls.md` §8).

Why six tools were missing from the first pass (scope and time): I judged the LSP spec, gopls, pyright and
TypeScript enough to settle the LSP row; I knew code-review-graph and code-graph-rag only from the internal survey;
and a search found no Glean docs on the point, so I stopped at two schema hits. The second pass read all six at
source.

**Sourcegraph**
- Sourcegraph docs, "Settings" (`docs/admin/config/settings.mdx`, sourcegraph/docs) — https://github.com/sourcegraph/docs/blob/main/docs/admin/config/settings.mdx — defaults of `codeIntel.disableSearchBased` and `codeIntel.mixPreciseAndSearchBasedReferences`.
- Sourcegraph docs, "Precise Code Navigation" — https://sourcegraph.com/docs/code-navigation/precise-code-navigation — precise first, search-based "used as a fallback".
- Sourcegraph docs, "Search-based code navigation" — https://sourcegraph.com/docs/code-navigation/search-based-code-navigation — the default; "case-sensitive word-boundary cross-repository plain text search".
- Sourcegraph docs, "Troubleshooting" (`troubleshooting.mdx`) — https://github.com/sourcegraph/docs/blob/main/docs/code-navigation/troubleshooting.mdx — search-based results come "only after all of the precise results".
- Sourcegraph docs, "Architecture" (`admin/architecture.mdx`) — https://sourcegraph.com/docs/admin/architecture — search-based is the default; false positives and negatives.
- Sourcegraph docs, "Syntactic Code Navigation" — https://sourcegraph.com/docs/code-navigation/syntactic-code-navigation — priority precise > syntactic > search-based; on by default for Cloud only.
- Sourcegraph docs, "Code navigation features" (`features.mdx`) — https://github.com/sourcegraph/docs/blob/main/docs/code-navigation/features.mdx — Find references lists "both precise and search-based results".
- Sourcegraph GraphQL schema `gql/codeintel.codenav.graphql` (sourcegraph/artifacts) — https://github.com/sourcegraph/artifacts/blob/main/gql/codeintel.codenav.graphql — `CodeGraphDataProvenance`, grouping by provenance, superset definitions.
- Older Sourcegraph docs: the legacy site (docs-legacy.sourcegraph.com and docs.sourcegraph.com) now redirects to the current docs, and the old `doc/code_navigation` pages are gone from the public snapshot repository. The client source below stands in for them.
- Sourcegraph client source in the Sourcegraph-owned mirror sgtest/megarepo: `useCodeIntel.ts`, `codeintel/location.ts`, `ReferencesPanel.tsx`, `MixPreciseAndSearchBasedReferencesToggle.tsx` — https://github.com/sgtest/megarepo/tree/main/sourcegraph/client — `PRECISE` / `SEARCH-BASED` badge and toggle text. It is a mirror, not the canonical repository, which is now private.

**GitHub**
- GitHub Docs, "Navigating code on GitHub" (current) — https://docs.github.com/en/repositories/working-with-files/using-files/navigating-code-on-github — name matching only, on by default.
- github/docs source at 6ec89f26d9 (before precise was removed) — https://github.com/github/docs/blob/6ec89f26d9/content/repositories/working-with-files/using-files/navigating-code-on-github.md — two approaches, Python and TypeScript precise, the "search-based" link.
- github/docs commit eac1b74c39 "Unship precise code navigation" (Dec 4, 2024) — https://github.com/github/docs/commit/eac1b74c39 — the removal.
- github/stack-graphs repository — https://github.com/github/stack-graphs — `archived: true` per the GitHub API.

**CodeQL**
- `javascript/ql/lib/semmle/javascript/dataflow/Nodes.qll` — https://github.com/github/codeql/blob/main/javascript/ql/lib/semmle/javascript/dataflow/Nodes.qll — `getACallee(int imprecision)`, `isImprecise`, `isIncomplete`, `isUncertain`.
- `javascript/ql/lib/semmle/javascript/dataflow/internal/CallGraphs.qll` — https://github.com/github/codeql/blob/main/javascript/ql/lib/semmle/javascript/dataflow/internal/CallGraphs.qll — imprecision levels 0, 1, 2.
- `javascript/ql/lib/semmle/javascript/dataflow/internal/FlowSteps.qll` — https://github.com/github/codeql/blob/main/javascript/ql/lib/semmle/javascript/dataflow/internal/FlowSteps.qll — data flow uses `getACallee(0)`.
- `javascript/ql/src/LanguageFeatures/InconsistentNew.ql` — https://github.com/github/codeql/blob/main/javascript/ql/src/LanguageFeatures/InconsistentNew.ql — a query picking the lowest imprecision and skipping uncertain sites.
- `docs/codeql/codeql-language-guides/codeql-library-for-javascript.rst` — https://github.com/github/codeql/blob/main/docs/codeql/codeql-language-guides/codeql-library-for-javascript.rst — the call-graph quality predicates; "no callee despite complete".
- `python/ql/lib/semmle/python/dataflow/new/internal/DataFlowDispatch.qll` — https://github.com/github/codeql/blob/main/python/ql/lib/semmle/python/dataflow/new/internal/DataFlowDispatch.qll — type-tracker dispatch; no name-only fallback found.
- `java/ql/src/DeadCode/DeadMethod.ql` and `DeadCodeExtraEntryPoints.inc.qhelp` — https://github.com/github/codeql/tree/main/java/ql/src/DeadCode — dead method at precision medium; false positives from hidden dependencies.

**Indexing formats and code graphs**
- Kythe schema (`kythe/docs/schema/schema.txt`) — https://kythe.io/docs/schema/ — `ref/call`, `ref/call/direct`, `lookup` nodes.
- SCIP `scip.proto` — https://github.com/sourcegraph/scip/blob/main/scip.proto — an occurrence has one optional symbol and roles; no candidate or unresolved field.
- Glean repository at f7c600a (Oct 9, 2026) — https://github.com/facebookincubator/Glean — the whole tree was searched (`rg` for `EntityUsage`, `EntityDynamicReference`, `Glean.Schema.Dyn`, `ConvertCalleeForFileCall`, `dyn.` schema references); all 95 schema files in `glean/schema/source/` and all 46 pages in `glean/website/docs/` were searched for unresolved, heuristic, fuzzy, approximate, ambiguous, speculative, guess, by name and dynamic, and every hit was read. Files read:
  - Glean schema `hack.angle` — https://github.com/facebookincubator/Glean/blob/main/glean/schema/source/hack.angle — `MethodOccurrence` for dynamically invoked methods.
  - Glean schema `dyn.angle` (`dyn.7`) — https://github.com/facebookincubator/Glean/blob/main/glean/schema/source/dyn.angle — `Unused` / `Enumerated` / `Used`; no producer or consumer elsewhere in the repository.
  - Glean schema `codemarkup.hack.angle` — https://github.com/facebookincubator/Glean/blob/main/glean/schema/source/codemarkup.hack.angle — `ConvertCalleeForFileCall`, "Don't use this predicate as it will go away".
  - Glean schema `python.angle` — https://github.com/facebookincubator/Glean/blob/main/glean/schema/source/python.angle — `XRefViaName` keyed by fully qualified names.
  - Glass `Handler/Symbols.hs` — https://github.com/facebookincubator/Glean/blob/main/glean/glass/Glean/Glass/Handler/Symbols.hs — call hierarchy: references in, xrefs inside the span out; 200 results and 2,000 ms default for non-C++.
  - Glass `Utils.hs` and Haxl `Haxl/DataSource/Glean.hs` — https://github.com/facebookincubator/Glean/blob/main/glean/glass/Glean/Glass/Utils.hs and https://github.com/facebookincubator/Glean/blob/main/glean/haxl/Haxl/DataSource/Glean.hs — `searchWithTimeLimit` discards the truncation `Bool` that `search` returns.
  - Glass `glass.thrift` — https://github.com/facebookincubator/Glean/blob/main/glean/glass/if/glass.thrift — `MAXIMUM_SYMBOLS_QUERY_LIMIT = 10000`, `MAXIMUM_QUERY_TIME_LIMIT = 15000`.
  - Glass `Handler/Documents.hs` — https://github.com/facebookincubator/Glean/blob/main/glean/glass/Glean/Glass/Handler/Documents.hs — cross-language xrefs resolved against a second database.
  - Glean docs, "Python" indexer (`glean/website/docs/indexer/python-scip.md`) — https://glean.software/docs/indexer/scip-python/ — the open-source Python indexer is scip-python.
  - Glean docs, "Hack" indexer (`glean/website/docs/indexer/hack.md`) — https://glean.software/docs/indexer/hack/ — "The Hack indexer is built into the Hack typechecker" in facebook/hhvm.
  - Third pass: what produces `python.angle` facts. Glean's `glean/lang/python-pyrefly/Glean/Indexer/PythonPyrefly.hs:40-46` at f7c600a — https://github.com/facebookincubator/Glean/blob/f7c600a68fa8db8af91db47a3b4cd16d53843d10/glean/lang/python-pyrefly/Glean/Indexer/PythonPyrefly.hs — runs `pyrefly check --report-glean`; the writer is pyrefly's `pyrefly/lib/report/glean/convert.rs` — https://github.com/facebook/pyrefly/blob/9855cf3c2d25c9a4cda460c2ec49305a8ecbef71/pyrefly/lib/report/glean/convert.rs — and `glean/lang/scip/indexer/Glean/Indexer/SCIP.hs:82-87` shows the scip-python path derives only `scip.*`.
  - Third pass: the Hack indexer, facebook/hhvm at cad215892139dfb51d9c322aae5e7079bf1f9ced, `hphp/hack/src/typing/write_symbol_info/` (`index_xrefs.ml`, `sym_def.ml`, `add_fact.ml`) and `hphp/hack/src/client_and_server/identify_symbol_service.ml` — https://github.com/facebook/hhvm/tree/cad215892139dfb51d9c322aae5e7079bf1f9ced/hphp/hack/src/typing/write_symbol_info — when it writes a `MethodOccurrence` and what a base-typed call targets.
  - Third pass, run: Glean demo image `ghcr.io/facebookincubator/glean/demo:v2024.06.10` (digest `sha256:eec9d45a51f7c0bcc8d2260b3e519cd633034851b84cda0bf9d0f91bc8ba6a10`, about 0.6 GB compressed), pulled and run under rootless podman, then removed; pyrefly 1.3.2 and scip-python 0.6.6 on the host. Commands and outputs: `dispatch-probe/out/glean/`. The Glean docs' "The Docker image is currently not working" (https://glean.software/docs/docker/) is out of date for this tag.
- Joern `x2cpg/Defines.scala` and `XTypeHintCallLinker.scala` — https://github.com/joernio/joern/tree/master/joern-cli/frontends/x2cpg — placeholder full names; stub methods.

**Language servers**
- LSP 3.17 specification, Call Hierarchy (`_specifications/lsp/3.17/language/callHierarchy.md`) — https://microsoft.github.io/language-server-protocol/specifications/lsp/3.17/specification/#callHierarchy_incomingCalls — `{from, fromRanges}`; no confidence field.
- gopls "Navigation features" (`gopls/doc/features/navigation.md`) — https://go.googlesource.com/tools/+/refs/heads/master/gopls/doc/features/navigation.md — "Dynamic calls are not included"; signature matching for dynamic calls under Implementation.
- pyright `callHierarchyProvider.ts` — https://github.com/microsoft/pyright/blob/main/packages/pyright-internal/src/languageService/callHierarchyProvider.ts — name pre-filter, then declaration equality.
- TypeScript `tsc/internal/ls/callhierarchy.go` — https://github.com/microsoft/TypeScript/blob/main/tsc/internal/ls/callhierarchy.go — incoming calls come from find-all-references entries.
- TypeScript `src/services/callHierarchy.ts` (branch `release-5.9`) — https://github.com/microsoft/TypeScript/blob/release-5.9/src/services/callHierarchy.ts — `getIncomingCalls` from `FindAllReferences.findReferenceOrRenameEntries` (lines 476-484).
- typescript-language-server `src/lsp-server.ts` and `src/features/call-hierarchy.ts` — https://github.com/typescript-language-server/typescript-language-server/tree/master/src — call hierarchy and references forwarded to tsserver; reply reshaped only.
- rust-analyzer `crates/ide/src/call_hierarchy.rs` — https://github.com/rust-lang/rust-analyzer/blob/master/crates/ide/src/call_hierarchy.rs — incoming from usages; outgoing drops unresolved method calls and closure or pointer calls; test `test_trait_method_call_hierarchy`.
- rust-analyzer `crates/ide-db/src/search.rs` — https://github.com/rust-lang/rust-analyzer/blob/master/crates/ide-db/src/search.rs — text super-set, then confirmation by name resolution; trait impl items included.
- rust-analyzer `crates/ide/src/references.rs` — https://github.com/rust-lang/rust-analyzer/blob/master/crates/ide/src/references.rs — test `assoc_items_trait_impl_def`.
- rust-analyzer `crates/hir/src/source_analyzer.rs` and `crates/hir/src/lib.rs` — https://github.com/rust-lang/rust-analyzer/tree/master/crates/hir/src — `resolve_impl_method_or_trait_def`; `CallableKind`.
- clangd `XRefs.cpp` — https://github.com/llvm/llvm-project/blob/main/clang-tools-extra/clangd/XRefs.cpp — `incomingCalls` (every reference; base methods with `MightNeverCall`), `outgoingCalls`, `prepareCallHierarchy`, `findReferences` for virtual methods, `locateSymbolTextually`.
- clangd `Protocol.h` and `Protocol.cpp` — https://github.com/llvm/llvm-project/blob/main/clang-tools-extra/clangd/Protocol.h and https://github.com/llvm/llvm-project/blob/main/clang-tools-extra/clangd/Protocol.cpp — `mightNeverCall` declared (lines 1698-1702) but not serialized (lines 1540-1542).
- llvm-project commit 378e4ed1ac, "[clangd] Show callers of base functions in incomingCalls (#163024)" (Oct 20, 2025) — https://github.com/llvm/llvm-project/commit/378e4ed1ac — the extension and its stated purpose.
- clang `lib/Index/IndexBody.cpp` and `include/clang/Sema/HeuristicResolver.h` — https://github.com/llvm/llvm-project/blob/main/clang/lib/Index/IndexBody.cpp and https://github.com/llvm/llvm-project/blob/main/clang/include/clang/Sema/HeuristicResolver.h — one-target rule for dependent references; the primary-template heuristic.

**Papers**
- Salis et al., "PyCG: Practical Call Graph Generation in Python", ICSE 2021 — https://arxiv.org/abs/2103.00587 — Table III and Table IV numbers; limitations §III-C.
- Huang et al., "Jarvis: Scalable and Precise Application-Centered Call Graph Construction for Python", arXiv 2305.05949v5 — https://arxiv.org/abs/2305.05949 — Table V; chain-based precision.
- Feldthaus, Schäfer, Sridharan, Dolby, Tip, "Efficient Construction of Approximate Call Graphs for JavaScript IDE Services", ICSE 2013 — https://www.franktip.org/pubs/icse2013approximate.pdf — field-based precision and recall; critique of name matching; zero targets preferred over spurious ones.
- Laursen, Xu, Møller, "Reducing Static Analysis Unsoundness with Approximate Interpretation", PLDI 2024 — https://www.cs.au.dk/~amoeller/papers/approx/paper.pdf — Jelly recall from 75.9% to 88.1%.
- Bhuiyan, De Stefano, Pellegrino, Staicu, "Call Me Maybe: Enhancing JavaScript Call Graph Construction using Graph Neural Networks", arXiv 2506.18191 (2025) — https://arxiv.org/abs/2506.18191 — 60% of sites unresolved for CodeQL; 42% top-1 and 72% top-5; ranked candidates for humans to vet.
- Ouyang et al., "RepoGraph: Enhancing AI Software Engineering with Repository-level Code Graph", ICLR 2025 — https://arxiv.org/abs/2410.14684 — SWE-bench Lite from 27.33 to 29.67; graph size table.

**Agent-facing tools**
- Aider `aider/repomap.py` — https://github.com/Aider-AI/aider/blob/main/aider/repomap.py — name tags weighted by PageRank for ranking.
- RepoGraph `repograph/construct_graph.py` — https://github.com/ozyyshr/RepoGraph/blob/main/repograph/construct_graph.py — name-equality edges.
- codebase-memory-mcp `src/pipeline/registry.c`, `src/pipeline/pass_calls.c`, `README.md` — https://github.com/DeusData/codebase-memory-mcp — strategy ladder, bands, suppressions, dead-code rule, agent tiers.
- trace-mcp `src/db/confidence.ts`, `src/tools/shared/resolution.ts`, `src/tools/shared/empty-note.ts`, `src/tools/framework/call-graph.ts` — https://github.com/nikolai-vysotskyi/trace-mcp — resolution tiers; the zero-result note.
- codegraph `src/graph/dead-code.ts`, `src/db/schema.sql`, `src/resolution/index.ts`, `README.md` — https://github.com/colbymchenry/codegraph — subtractive dead-code rules; `unresolved_refs`; edge confidence.
- graphify `graphify/symbol_resolution.py`, `graphify/cross_repo_calls.py`, `docs/how-it-works.md` (branch v8) — https://github.com/Graphify-Labs/graphify — EXTRACTED / INFERRED / AMBIGUOUS; "fabricates nothing".
- GitNexus `gitnexus/src/mcp/tools.ts`, `README.md` — https://github.com/abhigyanpatwari/GitNexus — `minConfidence` default 0; ranked ambiguous target list.
- Serena `README.md`, `src/serena/tools/symbol_tools.py` — https://github.com/oraios/serena — LSP or JetBrains backends; safe delete based on references.
- code-review-graph `code_review_graph/graph.py`, `migrations.py`, `tools/query.py`, `tools/_common.py`, `uncertainty.py`, `scoped_resolver.py`, `event_resolver.py`, `CHANGELOG.md` (branch `staging` at 6b12d11) — https://github.com/tirth8205/code-review-graph/tree/6b12d11625cbec3b6773e076cb3d136464fa90e5 — bare targets, `target_resolution`, the read-time name fallback, `resolution_split`, `unresolved_call_sites`, empty-answer notes; only `EXTRACTED` and `INFERRED` are written.
- code-graph-rag `codebase_rag/constants/graph.py`, `parsers/call_resolver.py`, `parsers/call_processor.py`, `parsers/import_processor.py`, `dead_code.py`, `cli_help.py`, `graph_query.py`, `cypher_queries.py`, `docs/architecture/graph-schema.md` (main at 5f76ace) — https://github.com/vitali87/code-graph-rag/tree/5f76ace43930842420ac5700aa94f251d48862b3 — `EdgeResolution`; the trie fallback; fan-outs; no edge for unresolved calls; `--min-resolution`.
- The internal survey `docs/research/02-code-graph-tools.md` (lines 69-74, 100, 109, 138) was checked against these sources; its corrections are in the two rows of §2.

**codectx (read-only)**
- the linker change's `docs/providers-link.md` (lines 1–20, 34–82, 150–270), `internal/provider/link/provider.go` (header, `Detail*`), `internal/provider/link/publish.go:383-450` — the no-name-binding rule, the ambiguous family, the unbound site.
- the linker change's `docs/providers-treesitter.md` (lines 91–128, 180–207), `internal/provider/treesitter/facts.go:563-720` — `call:<name>` placeholders, in-file `may_refer_to`, the cap of 16.
- main `internal/model/facts.go:52-108` — relation kinds, precision tiers. Main `docs/implementation-plan.md:640` — the resolution vocabulary.
- main `internal/graph/cost.go`, `internal/graph/traverse.go:971-1077`, `internal/mcpserver/graph.go`, `internal/mcpserver/handlers.go:98-108`, `internal/mcpserver/registry.go:48-58`, `internal/cli/query.go:205-225`, `internal/app/services.go:325-345`, `internal/model/query.go:92-110,606-660` — how traversals choose relations; answer shape.
- main `internal/context/scope.go:20-39`, `internal/context/compiler.go:1061-1092` — context-plan relations and weights.
- main `internal/storage/sqlite/schema.sql:558` — the node name index.
- main `docs/providers-dependence.md:55-66` — graph answers carry no evidence.
- main `docs/adr/ADR-0012-native-dependence-engine.md` (call-graph rows) and `docs/research/20-native-engine-post-mvp.md` (§7, lines ~530–550) — codectx's measured collision and resolution figures.
