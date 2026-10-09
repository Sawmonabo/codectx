# ADR-0012: Dependence is computed in process, and the hosted engine is retired against measured conditions

## Status

Accepted, 2026-09-26. On 2026-09-25 the owner directed that the hosted engine be replaced now, not after the MVP, and
the earlier "nothing in this record is started before the MVP ships" is withdrawn. The phase order, the gates and the
retirement conditions stand.

A benchmark over a public corpus matrix revised decisions 1, 2, 5 and 6 and added decisions 9 and 10. The matrix is one
large pinned repository per language family, plus a compiler checkout. Every decision names the measurement that
confirms it. If a confirmation fails, that decision is reopened, not the whole record. The per-language control-flow
lowerings of phase 0 exist for all nine advertised languages, each with a golden table. They are not yet wired into
the worker, the wire protocol or a provider. Phase 0 is not yet claimable under its own golden rule (decision 6). A
review on 2026-09-26 found these gaps:
- most Go and JavaScript cases cite no reference anchor;
- some C cases cite none, and the TypeScript cases cite a handbook, which is not a language reference;
- several handled constructs have no case;
- no table has had its blind second derivation.

Each gap is closed before phase 0 is claimed.

**Amended 2026-10-08 (October 8, 2026), by the owner's ruling.** Testing is halted and the replacement is integrated
now, with no proofs, benchmarks, corpus differentials or engine runs. The gates that need the engine to run are
withdrawn:
- decision 6's band conditions for phases 1 to 3;
- decision 7's band;
- the seven-corpus substitution differential that was to license the substitution.

The gate actually used is the authored goldens, each table derived twice, blind, from the source text and the
language reference, plus one test pass, one review and one fix round over the integrated change. The four file-local
families move to the structural provider for every language at once, so decision 9's graduation set is not built.
The engine's importer stops publishing those four families for every language in the same change, and the engine
runs for `calls` only until the linking provider of decision 9 publishes them; it is then deleted. Decisions 1, 2, 4,
5, 8 and 10 stand unchanged. Decision 3 stands and is not yet built: both families are in the default traversal set,
but impact still excludes them (`internal/graph/cost.go:83-89`) and no program-dependence relation set joins them.

## Context

### What the product does today

The `dependence` provider runs an external analysis engine, a JVM, once per unit for the parse and once
for the export, under a heap ceiling the provider chooses ([providers-dependence](../providers-dependence.md),
[ADR-0010](ADR-0010-engine-memory.md)). It publishes five capabilities — `control_depends_on`,
`data_flows_to`, `reads`, `writes` and `calls` — every fact at `static_analysis` precision with exact
byte ranges. Those facts are what `codectx_callers`, `codectx_callees`, `codectx_dependency_path` and
`codectx_impact` walk; `codectx_symbol_info` and `codectx_references` report their precision.

The engine is reached through one backend package and consumed through a graph CSV export staged in a
private staging database ([ADR-0009](ADR-0009-import-staging.md)). It has no incremental mode: one
edited file re-runs a whole unit.

### The requirements this decision is judged against

The product owner's scope for a native replacement, decomposed into the obligations it actually
contains. Editorial substitution in square brackets; everything else is the owner's wording.

> "logic/parsing/and whatever technique is already defined in [the engine] for the graph passes:
> control-flow graph, control dependence, reaching definitions, program dependence, call-graph linking
> per language, type-recovery heuristics AND anything else that we are missing and would be a benefit
> or critical to our app […] we would literally just need to rip it out and do it in a fast and more
> performative way. And leveraging breakthrough research/algorithms/traversals/memory & cpu advanced
> techniques and ensuring proper architecture and design for this is a greenfield app: no backwards
> compat, migrations are needed, everything can be changed in place. Always choose the algorithm, db,
> design that provides and meets our requirements. Any research that proves something is better should
> be taken as truth and done when self-verified and results hold. This app should be simple so an AI
> simply uses the MCP to get what it needs immediately without wasting time on figuring out how to tune
> timeouts etc. It must work on extremely large open-source / large enterprise monorepo codebases
> (3–10× the size of [the reference repository], [a very large] enterprise codebase) and prove
> blazing fast speed and memory handling (leveraging advanced algorithms), support out of the box no
> caps, time limits, max visited nodes, etc., and leverage all tools ([the engine], LSP, SCIP,
> dependencies, providers) and provide the complete full repo mapping, repo map graph, call stack,
> flows, etc. that allows AI agents to quickly identify solutions/bugs and produce higher quality code
> and planning. Our app freezing the machine and crashing it is 100 percent a product defect. It should
> be blazing fast and memory efficient while providing ALL of its features/capabilities and dependency
> libraries like [the engine] and providers like LSP, SCIP, etc. without crashing the person's machine
> and freezing stuff up if they are working on their computer. AI using this through MCP means it will
> also be working on the computer; if our app can't work alongside other processes you failed. AND we
> must be the most advanced technology in the industry that achieves all this."

Twenty-four atomic requirements follow from that text. The ones that decide this record:

1. Control-flow graph, control dependence, reaching definitions — natively.
2. **Program dependence graph as a product surface**, which the product has never had.
3. **Call-graph linking, per language**, and **type-recovery heuristics** — both explicitly in scope.
4. **Rip it out**: the end state is the hosted engine's removal, not a reduced role for it.
5. Choose the algorithm, the store and the design that meet the requirements, and take proven research
   as truth once self-verified.
6. An AI uses the MCP surface and gets what it needs immediately, tuning nothing.
7. Works on 3–10× the reference repository; blazing fast; memory efficient.
8. **No caps, no time limits, no max-visited nodes**, out of the box.
9. Leverage *all* tools together: the engine, language servers, precise indexers, manifests, providers.
10. Complete repository mapping, repository-map graph, call stack, flows, for agents finding bugs and
    planning.
11. **Freezing or crashing the machine is a product defect**; the product must coexist with the user's
    other processes and with the agent driving it over MCP.
12. Be the most advanced technology in the industry that achieves all this.

### What was measured before anything was decided

**What the product actually consumes from the engine.** About **2,843** of the engine's 123,641 main
Scala lines produce the four dependence families: the control-flow package 1,294 (12 files), the
reaching-definitions package 962 (7 files), the three call linkers in the call-graph layer 297, and
290 lines of attribution and stub passes. A further **4,885 addable lines** — 2,447 shared and 2,438
per-language for the six frontends the product invokes — are what make its `calls` resolvable. 38.6%
of the engine (47,685 main lines) is eight frontends the product never invokes.

**What the engine's call graph is actually worth.** On the reference repository, at the active
generation: the engine emits 217,881 call sites and 115,940 edges, of which only **40,743 sites
(18.7%) and 27,897 edges (24.1%) resolve to an in-repo definition**. The other 177,138 point at
external or library stubs and reproduce no capability a consumer can act on.

**And what that percentage is divided by.** 18.7% counts **every** call site, including the ones whose
callee is a library, the platform or the language runtime — for which "no in-repo definition" is the
correct answer and not a miss. A hand-classified, seeded, language-stratified sample of 460 call sites
on that repository and 204 on a second one puts the honest denominator at **49.92% [44.42, 55.33]** of
its call sites and **42.81% [42.04, 43.57]** of the second corpus's configured language. Against that
denominator the producers resolve **28.60%** and **94.26%** respectively. The measurement, its method
and its limitations are
[20-call-ceiling-sample](../research/raw/native-engine/20-call-ceiling-sample.md); the figures enter
this record in the measurements section, and they are what decisions 2 and 6 are now stated on.

**What the precise tier could ever replace, and what that turned out to depend on.** Had every planned
precise unit run on the reference repository, precise resolution would cover **4.87–5.25%** of its
555,588 call sites. That number is a property of **that repository's configuration**, not of the
precise tier: its majority language, 94.8% of its call sites, carries no project configuration
anywhere in the tree, so no profile is planned for it. On a second corpus whose majority language does
carry one, and where the pinned indexer ran over the whole repository, the same call-site join covers
**112,554 of 135,714 call sites (82.9%)** and **91.5%** of that language's own; on a nine-language
fixture, every project configured, it covers **41 of 45** and **23 of the 23** whose callee is defined
in the fixture. The syntax tier resolves 13.6% — corrected to about 12.4% below — and leaves
**82.8% as calls through a value**; on the second corpus **82.6% of that same unresolved population
does carry a compiler-precision occurrence at the callee identifier**, so what a precise index cannot
supply is the call *site*, never the callee identity. Measured on the reference repository's largest
project, two different language servers each returned **2 references**, cold and after a 30 s settle.
So `calls` cannot be delegated to the precise tier **on an unconfigured repository** — which is the
reason it must be *ported*, not left behind — while on a configured one the join already carries it.

**Which frontends get type recovery at all.** Two, on the argv the product pins: the ECMAScript family
and Python. C/C++, Go and Rust never override post-processing, and the Java override is gated on a flag
the product does not pass. For those four the engine's `calls` is an exact full-name equality join and
nothing more. The two that do have it carry **95.24%** of the reference repository's call sites.

**That the dependence families are file-local — proved by the engine itself.** Run on one file, it
reproduces **100%** of control dependence and reaching definitions for Python, TypeScript, C and Java,
and 98.3% / 98.5% for Go with a named cause (a synthetic per-package initialiser a real package scope
does not inherit). Subdividing a project keeps control dependence at 99.7% and reaching definitions at
99.9%, while resolved calls collapse to 46%. The split is per fact family, not per language.

**What the engine costs.** On the reference repository: a 31:38 run of which the dependence phase is
**23:28 — 74%** — and one JavaScript unit alone is **≈20:20, 64% of the whole run** (two 3:20 parse
attempts to a deterministic linker crash on legal JavaScript, then nine children run one at a time).
JVM peaks 9,263 / 8,752 / 8,186 / 8,167 / 6,563 / 4,845 MB. The export is 0.65–4.95 GB of CSV, staged
at 6.6× its size.

**The run-to-run band.** The engine is not deterministic: two runs over the same unmodified tree differ
by about **0.01%**, and a claim of *equality* between two engine runs is itself a defect. Every parity
claim is bounded by that band.

### What the public corpus matrix measured

The figures above were taken on one private reference repository, and they sized the previous design: 256 MiB per
worker, a 1 GiB base, a 1 MiB arena release threshold. On 2026-09-26 the dependence core's benchmark ran over seven
public repositories, one per language family, each pinned to a commit. Each ran in one memory-capped process built from
one commit, with a row per file and per function. Two private instances were measured the same way; their rows stay out
of the repository. The pins and every aggregate are in
[21-native-engine-need-and-throughput](../research/21-native-engine-need-and-throughput.md) and its raw directory.

| language (the corpus where it is the majority) | native parse bytes per source byte, p50 | p99 | max |
|---|---|---|---|
| C (the compiler-infrastructure corpus) | 9.6 | 31.6 | 260.1 |
| C++ (the same corpus) | 18.7 | 45.0 | 300.8 |
| Go (the orchestration corpus) | 20.2 | 36.5 | 80.2 |
| Java (the search-engine corpus) | 16.9 | 26.9 | 49.3 |
| Python (the home-automation corpus) | 22.8 | 33.0 | 77.9 |
| Rust (the language's own compiler) | 22.5 | 72.6 | 147.8 |
| TypeScript (the editor corpus) | 22.4 | 38.1 | 54.8 |

Four findings follow, and each one changes a decision below.

1. **A fixed per-worker reservation is wrong in both directions.**
   - The median file of every language needs under 1 MiB of native memory.
   - The largest file measured needs 303.9 MiB: a 15.4 MiB C source.
   - Within one language, the tree's bytes per source byte run from 0.015 to about 80.
   - No per-grammar constant can size a file, and 256 MiB is over by about 300× at the median and under at the top.
2. **The lowering walk dominates the per-function cost, not the algorithms.**
   - In the Go corpus, 228,765 functions took 11.6 s to lower. Post-dominators, control dependence and def-use together
     took 0.54 s.
   - The cause is crossings into the parse-tree library, measured by a CPU profile of the Go corpus's function rows.
     Of 66 s of samples, the parse itself is 19.4 s (29%) and lowering 22.5 s (34%). The benchmark lowers every function
     twice, so lowering one function once costs half that. The walk that finds callables
     costs about 15 s more. Inside both, the dominant cost is one native call per node access: node kind, named child,
     named flag and cursor steps.
   - Resolving field names to ids, which removed a C-string allocation per field lookup, cut the Go corpus's lowering
     only from 11.6 s to 10.5 s.
3. **The structural stage's wall time goes to unit work outside the parse, and that work is serialized through the
   store's lock.**
   - A parser worker's span is its process lifetime. Its measured 6.2 s of wall against 0.13 s of processor time
     (three workers of one end-to-end test run in the capped test pass of 2026-09-25, recorded in the research note's
     raw wall-time file) is a worker busy about 2% of the time. That much is measured.
   - The serialization point was found by reading, not by profiling. In the store measured, every per-unit store call
     took one group mutex, and a unit made about ten such calls, plus one or two per extracted record. That is the
     measured reason for the store changes of decision 5, which replace the mutex with one writer goroutine.
   - How the other 98% splits between waiting on the lock, SQL, worker re-execution and the provider barrier is not
     yet measured. It is the benchmark task's first measurement.
4. **A C++ header is parsed as C.**
   - In the build measured, `.h` belonged to the C grammar alone. Decision 10 declares it in both grammars' rows
     (`internal/provider/treesitter/lang/lang.go:86-87`) and lets the repository decide.
   - In the compiler-infrastructure corpus, 74.7% of `.h` files parse with errors, against 22.8% of `.c` files.

The per-function design figure of decision 5 has a heavy tail but a small absolute size. The population is the
function rows of all nine lowerings over the matrix; two corpora's runs stopped early on the lowering panics fixed in
the same round. On every population of at least 1,000 functions, `bound_ratio` reaches p99 about 1.4–6.2. The maximum
is about 85, on a Rust function, and the largest arena measured is 26.8 MiB, on a TypeScript function.

### Why the previous plan was not enough

The post-MVP plan
([20-native-engine-post-mvp](../research/20-native-engine-post-mvp.md)) sized the control-flow and
reaching-definitions cores well and then left `calls` on the engine indefinitely, sized no port of the
call linkers or of type recovery, and closed with "whether to keep it at all is a separate, later
decision that this plan does not make". Audited against the twenty-four requirements it returned 4
`meets`, 12 `partially meets`, 5 `silent` and 3 `contradicts`. The three contradictions were exactly
requirements 3 and 4 above. A plan whose last phase does not terminate is not a retirement plan; it is
a permanent second producer, which is the compatibility layer a greenfield product is forbidden.

## Decision

### 1. The unit of work is a function; the unit of caching and scheduling is a file

```
per file (caching, invalidation, scheduling)
  inside the structural provider's existing worker subprocess, in the same walk that extracts the file
    per function (work and memory)
      normalise [per-language] → CFG → post-dominators → control dependence
        → def/use [per-language] → SSA def-use → emit
  then the tree is closed and the file's memory returned (decision 2)
per project (the one scope that is not per function)
  calls: the precise index where a profile applies; otherwise the package-scoped linker of decision 9
```

The analysis runs **inside the parser worker, in one pass with extraction**. Parsing already happens in isolated
worker subprocesses that own every native object, and only fact frames cross the wire. The worker lowers and analyses
each function in the same `serve` walk that extracts the file's structural facts. It streams each function's facts as
binary frames of ids and byte ranges, then closes the tree. A separate pass, in the coordinator or in a second worker
call, would parse every file twice. Parsing is 31% (Go) to 68% (JavaScript) of the measured per-file cost. The
worker's crash isolation is inherited. Its reservation is observed per file (decision 5), never a fixed figure.

**Alternative weighed:** keep the project as the unit, as the engine does. Rejected on the measurement
above — the dependence families are file-local, so a project-sized unit buys nothing for them, and it
is what makes a single crash cost 20 minutes and a whole unit's facts.

**Trade-off accepted:** `calls` still needs a project scope, so one family is scheduled differently
from the other four. That asymmetry is real and is why `calls` is ported last, by its own provider (decision 9).

### 2. Every algorithm is chosen against a named alternative

| question | decision | why this one, and what is given up |
|---|---|---|
| Dominators | **Cooper-Harvey-Kennedy iterative** over a dense `int32` reverse-post-order, written in the repository | the asymptotic crossover is irrelevant at the measured function-size distribution: the Go corpus's 228,765 functions have p50 7, p99 81, max 1,909 CFG nodes; the editor corpus's JavaScript p50 6, p99 102, max 11,162. Below about 10³ nodes allocation decides, and the near-linear library implementations allocate a heap node with its own map bucket per CFG node. **Given up:** the asymptotic guarantee on a pathological function, and a dependency that would have been free to import |
| Post-dominators | the **same** pass over a **reversed, exit-augmented** CFG | one code path to validate instead of two. A generic implementation reversed this way was verified sound from source — both entry points consult only the successor relation — so this is a choice, not a necessity. The augmentation is mandatory: a unique synthetic exit, plus an edge from every strongly-connected component that cannot reach it, or an infinite loop's nodes silently have no post-dominator. **Given up:** the native set will differ from the engine's *by design* on functions with unreachable-from-exit regions (29 of the Go corpus's 228,765 functions); the differential band must expect that surplus as a named cause |
| Control dependence | **Ferrante-Ottenstein-Warren** via post-dominance frontiers on the exit-augmented CFG, **without** the entry-to-exit edge | it is the textbook set. The engine computes a strict under-approximation in two provable places: the method node's single successor fails the ≥2-predecessor filter, so nothing is control-dependent on entry, and the frontier walk truncates on a missing post-immediate-dominator. **Why it matters more than it sounds:** the product's `control_depends_on` is a single-hop anchored join with no walk, unlike the depth-8 `data_flows_to` walk, so a missing edge is an unrecoverable missing fact rather than a longer path |
| Reaching definitions / def-use | **replace, do not port**: sparse SSA-based def-use, not the dense bit-vector worklist | the dense `in`/`out` is Θ(N²/64) words — **2.34 GiB on one function at N = 10⁵** — which a design with no caps cannot carry. The engine survives it only by having the cap this record forbids. The SSA construction chosen needs no dominance computation for def-use, so the port keeps one dominator computation per function, for control dependence. **Given up:** exact oracle parity. The two formulations differ in named places, so the `data_flows_to` band becomes signed and per-cause rather than one absolute difference, and the port owns φ-operand resolution |
| Address-taking | a local gets a **may-definition**, killing nothing, at the node that evaluates the operation, when its address is taken, when it is mutably borrowed (including `ref mut` pattern bindings), when a C++ reference to a non-const type is bound to it, and, in C and C++, when an array-declared local is evaluated other than as the operand of `sizeof`, `&` or a subscript | a write through the pointer is invisible without points-to analysis, which this table rules out below. A may-definition keeps every earlier killing definition reaching later uses and adds the address-taking node as the latest may-write (see May-definitions). **Given up:** a later write through a pointer or reference (`*p = 2`), whose target is unknown without points-to: making it a may-definition of every address-taken local would connect every indirect write to every such local. The write is still a may-definition of the variable it writes through (`p` in `*p = 2` and `p->f = 2`), as a write through a field or an index is of its base variable, so a later read through the same name depends on it; what is given up is the local that `p` points to. The implicit receiver borrow of a method call, which depends on the method's signature. A shared borrow of an interior-mutable type, which depends on the type. A write through a C++ reference to a const type (`const T &r = x`, a range-for `const auto &e : v`), which the language makes a read-only view, so binding one is a use only: a write through it after casting the const away, and a write to a `mutable` member, are given up, as the interior-mutable shared borrow is. A `move` closure writes its own copy, so its writes are not definitions of the outer variable |
| Value flow | a value computed at one node and used at another **always travels through a variable**: a named one, or one the lowering owns and binds to no name. A value evaluated once and read at several nodes (an iterable, a switch selector, a match scrutinee, a tested expression, a resource) is evaluated at a node of its own that defines an owned variable, and the loop head, labels, arms and bindings Use that variable. A value-producing construct lowered to nodes of its own (a switch or match expression, a conditional, a short-circuit operator in value position, a statement expression, a valued break or yield, the `?` that completes a try block, an embedded assignment, a creating node) has every node that yields its value define an owned result variable, and its consumer Uses that variable. A construct folded into one node contributes its reads to that node's own evaluation, and each lowering states which constructs it folds | copying a nested construct's reads onto its consumer cannot carry a dependence that reaches the value only through control: in `y = switch (x) { case 1 -> { while (c) { yield 2; } yield 3; } … }` the consumer post-dominates every node of the switch, so only the yielding nodes, control dependent on `c`, carry the value's dependence on it. Re-reading the names a once-evaluated value was computed from gives a false pair whenever the body rebinds or writes through one of them (`for k in d: d[k] = f(k)`). Expression-level constructs therefore behave exactly as a statement-level `if` that assigns in each branch. **Given up:** the per-node count, since a once-evaluated value and each yielding node add a node or a definition, and the name-level readability of a pair that runs through an owned variable, whose endpoints are nodes rather than a named variable |
| May-definitions | a may-definition of v at node p is a **χ**: p reads v's prior version and defines the next one. A use of v pairs with every killing definition of v that reaches it, through any number of may-definitions, and with the nearest may-definitions of v on each path to it, the ones no later may-definition of v follows on that path. p pairs with what reaches it by the same rule, whether or not it reads v in the source. A node may make several killing definitions, so a value a node yields beside the local it assigns is a killing definition; a may-definition is reserved for a write that may leave the old value in place: a write through an address, a borrow, a field, an index or a pointer, and a closure's write to an enclosing variable. After a conditional-compilation group, a read Uses every binding the name has in some build, and a definition defines each of them, killing each | pairing a use with every may-definition reaching it is quadratic on a chain of writes through one base: the TypeScript corpus's 20,013-line `largeControlFlowGraph.js`, about 20,000 consecutive `data[0] = 0;`, asked for about 2×10⁸ pairs in one function and exhausted memory. The χ encoding keeps the pair set, and the resolver's work, linear in the chain, and it loses no dependence: every earlier may-write reaches the use through the chain. A one-definition-per-node builder had forced yielded values onto may-definitions, where a χ chain would claim that one yield consumed another's value, so the builder takes several killing definitions per node. **Given up:** one hop gives the last may-writes and the reaching killing assignments, not every may-write; the rest are the chain, reached by the `data_flows_to` walk, whose depth bounds how far back an answer sees on a long chain |
| Interprocedural framework | **none** — no IFDS, no IDE | realizable-path precision is unobservable at a surface that publishes unlabelled depth-8 reachability, and the exploded supergraph reinstates exactly the whole-program resident structure decision 5 forbids |
| Incrementality | **no incremental dataflow algorithm**; a changed function is recomputed from scratch, and the invalidation boundary is the file's content hash | a function's CFG is tens to hundreds of nodes; the bookkeeping costs more than the recomputation. This is also the model the structural tier already runs |
| In-flight adjacency | plain `int32` compressed sparse row, forward and reverse, no varint | it is built and discarded inside one worker — a different object from the published per-generation adjacency of [ADR-0005](ADR-0005-graph-traversal-layout.md), which is unchanged |
| Tree access | each file's tree is **flattened once, by a constant number of native calls (one to size the array, one to fill it, one to free the fill's field table once it is copied) and none per node**, into a Go-side node array (kind, the cursor's nearest field, flags, byte range, parent, first child, next sibling) and a sparse field table, at any point before the native tree is closed, since flattening only reads it: a header's parses are flattened to weigh their errors before the structural queries run on the kept one, with no field table, since nothing reads fields there. The field table gives the array the library's own child-by-field answers: inside the fill, for every internal node, the library's public `ts_node_child_by_field_id` is asked for each field id in the language's field registry (the fields the lowerings resolve, listed once in the grammar table, which refuses a lowering any other), and every answer that is not null is kept as a (field, node index) pair. The field map it reads says more than the cursor's nearest field can: a field inherited through a hidden or an aliased child, an outer field on a node that also has a nearer one, and no field on an ERROR node. Its cost is one C-side lookup per internal node per registered field, and no crossing per node; its bytes are the answers only, counted in the array's size. Once the queries have run, the native tree is closed, and the lowering and the callable walk read the array with **no native call per node**. Every kind and field is resolved to an id once per process | per-node native calls are the measured dominant cost inside lowering and the callable walk (the context section's profile). **Alternatives:** per-call accessors, which is the measured cost; resolving ids alone, measured at under 10%; reading the library's node structures directly through unsafe pointers, which breaks on any change of the library's layout. The native tree handle is read from the binding's tree wrapper, whose only field it is, under a size assertion and a root-node equality test, so a binding change fails the tests rather than reading wrong memory; a repository-owned binding over the library's public API is the alternative, and is not needed while that check holds. **Given up:** the flat array's own bytes beside the tree while flattening, and whether it is smaller than the tree after is unknown (the tree stores leaves inline in 8 bytes). **Confirming measurement:** lowering time per function on the Go corpus, and flat bytes per source byte beside tree bytes, per language |
| Allocation | one **reset slab arena per worker**, pointer-free typed backing arrays, reused across a file's functions and **released at the file boundary** | the multiplier is on the order of 10⁶ functions, and what the arena saves is the collector's scan set. Today the arena releases at a 1 MiB threshold (`flow/arena.go`), a figure fitted to nothing and corrected here: it fired for 3 of about 351,000 functions while the arena kept 0.4–1.25 MB between them. **Given up:** manual lifetime discipline inside the worker |
| The file boundary | after each file the worker closes the tree, drops the arena's backing, returns freed C heap pages to the kernel and resets its own peak reading; the parse loop runs on one OS thread with one C heap arena | a per-file need can only be observed if the memory of the previous file is returned (decision 5). Without the return, the C allocator reuses what it already holds and the reading is censored: the resident set does not move on about 95% of files. **Given up:** the refault cost of the next file, and the arena setting is specific to one C library; both are measured before the decision is closed |
| `GOGC` / `GOMEMLIMIT` | **set neither** | the parse tree's C allocation can be counted, because the binding routes it through Go. But the memory a limit would govern in the worker is small, and the coordinator's retained records are already bounded by the sink pool. A limit would add only the thrashing risk the garbage collector's own guide describes |
| Call graph — Java and other hierarchy-typed languages | **class-hierarchy analysis** | decided on **streamability**: its inputs are two tables an ordered merge join consumes, while rapid type analysis needs a reachability fixed point and variable-type analysis a global propagation graph — both whole-program resident state decision 5 forbids. **Given up:** precision on megamorphic sites, which land in the existing ambiguous-candidate family the product already publishes with a candidate count |
| Call graph — Go, Rust, C/C++ | direct binding, plus rapid type analysis's signature-keyed address-taken × indirect-site cross-product **without** its reachability fixed point, plus class-hierarchy analysis for interfaces and traits | most calls are direct; the interesting ones go through interfaces or function values. The nine-language fixture carries all three configured, where the call-site join resolves 5 of 6 Go, 8 of 9 C/C++ and 6 of 6 Rust sites and **every** site whose callee the fixture defines. The public matrix now supplies a large corpus of each language to price the cross-product on |
| Call graph — ECMAScript family, Python | **no whole-program points-to in any formulation.** Flow-based type inference through assignments, parameters and returns over the def-use chains this table already builds; field-based resolution at index time; class-hierarchy analysis where a subtype relation exists; demand-driven resolution at read time | inclusion-based points-to has the right semantics and a forbidden budget; unification-based has the right budget and precision that collapses on these flow shapes. **The achievable ceiling is measured.** Against the honest denominator, 28.60% of in-repo-targeted sites are resolved today on an unconfigured repository; field-based resolution accounts for a further **29.73 pp** and flow-based inference for **27.41 pp**, reaching **90.07%**, with class-hierarchy analysis a further 8.66 pp and a **1.27%** residue whose callee identity depends on a call-site-specific value. Neither of the two load-bearing techniques is optional, and neither is a name join: **70.59%** of that repository's call sites carry a callee name some tracked definition also carries, so a resolver keyed on the name alone claims two sites in three and is wrong on most |

**What the engine's own linkers are.** Its static linker is none of the academic algorithms — an exact
full-name equality join. Its dynamic linker **is** class-hierarchy analysis. Its type recovery is
flow-insensitive symbol-table type propagation over a **fixed two iterations**, explicitly not a fixed
point. The measured 18.7% is class-hierarchy analysis plus a name join plus two propagation rounds.
Nothing in it is unreproducible, and no part of it is a reason to keep a JVM.

### 3. The program dependence graph becomes a surface

Control dependence and data dependence are both staged, both in the default traversal set, and keyed by the same
entity pair. The composed graph is therefore a **join in the projection**, not analysis work. Leaving the caller to
compose it by filtering relation kinds is exactly the tuning that requirement 6 forbids, so the product composes it.
This is the one capability the port **adds** rather than preserves.

Concretely:
- **No new tool.** The surface stays at its 23 tools (`internal/mcpserver/registry.go:94`).
- **One relation set.** A program-dependence relation set joins control and data dependence by entity pair.
- **Impact walks it.** Impact includes that set; today it excludes both kinds (`internal/graph/cost.go:83-89`).
- **Paged evidence.** Evidence ranges are paged per pair, like every other answer.
- **Anchored on every operand.** Control dependences on operator conditions are anchored at each operand, where the
  engine drops them.

### 4. The shared core adds no dependency

The dominator core is written in the repository, so the analysis adds **no module** to `go.mod`. The
parse-tree layer, cgo and the nine grammar registrations (from eight pinned grammar modules) are
already pinned, so a native engine adds **no parser, no runtime, no helper binary and no per-language
toolchain** — the opposite of the hosted engine, whose six frontends drive a C/C++ compiler frontend, a
Java symbol solver and three downloaded helper binaries, plus a JDK.

Parameters and locals become structural `variable` entities carrying the existing cross-provider declaration key, so
the native producer and every other producer resolve a variable to one identity.

**A negative this record corrects rather than repeats.** "No permissively-licensed Go package exposes
post-dominators or a dominance frontier" is true only of **public APIs**. One BSD-3 package computes a
Cytron dominance frontier in about 28 unexported lines that can be ported rather than invented. The
must-write list is real and smaller than the earlier plan assumed.

### 5. Memory is taken from each file's observed need, and coexistence is a mechanism

**Per function**, the design figure for the structures is **M_sparse(N) ≈ 96·N + 64 bytes**: 0.92 MiB at N = 10⁴ and
9.16 MiB at N = 10⁵. It is a **design figure, not a bound**. The control-flow CSRs and the post-dominator pass are
O(N + E), and the reaching-definitions pass also grows with:
- the variables, uses and definitions;
- the (block, variable) pairs its lookups visit;
- the pairs it emits.

So the arena is not linear in N alone, and nothing guarantees or enforces the figure. Measured over the matrix for all
nine lowerings, `bound_ratio` reaches p99 about 1.4–6.2 on every population of at least 1,000 functions, and a maximum
of about 85. The largest arena is 26.8 MiB. The benchmark
reports each function's arena bytes against the figure as `bound_ratio`, taken after the three dependence-core passes:
post-dominators, control dependence and reaching definitions. The forward dominator pass, which the dependence core
does not run, is excluded from that ratio and reported on its own.

The definition count is bounded **structurally**, not by a constant: a definition is a CFG node index, so D ≤ N. That
replaces the engine's definition cap, whose price is dropping *every* reaching-definition edge of an over-large method.

**Per file, need is observed, not assumed.**
- **Measure.** At the file boundary of decision 2, the worker returns freed pages, reads its base and resets its peak
  (the kernel's `clear_refs` value 5, then `VmHWM`). After the file it reads the peak and reports three figures in the
  file's `Done` frame: need (peak minus base), base, and anonymous resident set. The base the worker holds on the ledger
  is its anonymous resident set, which is its own, not the executable pages every worker shares; where only the whole
  resident set is reported, that is held instead. The worker measures itself: the 250 ms tree sampler misses nearly
  every parse's peak, so for need it stays only as an envelope check.
- **Where no resettable peak exists.** On a platform that offers no resettable per-process peak, need is reported as
  unavailable, never as zero: the file is reserved at its prediction and nothing is learned from it.
- **Learn.** The coordinator keeps a decaying histogram of need per source byte in 5% buckets. It is keyed per
  repository, language, grammar fingerprint, the worker's build and file-size class, and persisted in the ledger's
  observation store beside the scope peaks, since a measurement stays true whatever becomes of its generation. It
  reserves the histogram's weighted p99, which is the sample maximum below 100 observations. The half-life is counted
  in that repository's own files of that language, so the model adapts at the speed of the repository in front of it,
  whatever its size.
- **First file.** The first file of a language never seen in this repository is reserved at a **structural prior**:
  `worker base + source bytes × 107`.
  - The 107 is derived from the parse-tree runtime's own node layout, not measured on any repository. Per source byte
    it adds 80 + 8 bytes for a heap node, 16 for the C allocator's per-allocation overhead, 2 for the binding's two
    input copies and 1 for the worker's buffer.
  - It holds on all 3,827 matrix files of 64 KiB or more. It is exceeded by 9 of 74,059 files of 4 KiB or more; the
    worst is 302.8 bytes per source byte, a 10,479-byte C++ file.
  - An exceeded prior is an overrun that runs and is disclosed, never a refusal. Once a file reserved at the prior needs
    more than it in some class of a language, every later file of that language reserved at the prior runs alone among
    the parses, for the life of the parser pool: it is granted only when no other parse holds an increment, and none is
    granted beside it. The switch is disclosed, as a warning naming the language and in the parser's resource view with
    the count of files run alone.
  - A worker of a new build learns afresh: the build is part of the key, and the parent refuses a worker whose hello
    states another build.

**Per run:**

> `R_run = B_process + Σ over workers in flight (base_w + predicted_need(file_w)) + A_link`

- `B_process` is `config.BaseFootprint`. Its idle term is read from the parent's own resident set once, at load;
  only a platform that reports no resident set falls back to the recorded idle measurement.
- `predicted_need` is the learned p99 above, or the prior for a first file.
- `A_link` is the package-scoped linker's working set (decision 9).
- No term is a constant fitted to a repository.
- The figure depends on the files in flight, not on how many files the repository has.

**The ledger.** Each worker holds its observed base -- its anonymous resident set -- for its lifetime and reserves each
file's predicted increment before dispatch. The ledger ([ADR-0010](ADR-0010-engine-memory.md), `internal/admission`) gains three obligations:
- **Adjust.** `Done` adjusts a holding to what the file used, upward without waiting.
- **Forward progress.** A per-file increment is granted whenever no parse is in flight. This is the runs-alone rule
  restated per file, so a file larger than the whole allocation still runs, whole.
- **One budget.** The parser runner's budget never refuses what the ledger admitted.

**The overrun target** is at most 2% of files after the first generation, per class. Reserving the p99 expects 1% of
files to overrun once the model is warm, and the second 1% is the decaying histogram's lag behind a repository that
changes. A class that misses the target reopens the percentile, never the refusal rule: a file that overruns still
runs.

A file whose need exceeds its prediction does not fail; the overrun is counted and disclosed with its reservation, its
observed need and the drift between them (ADR-0010 decision 4). The learning loop is closed through the per-file need
observation: each file's `Done` carries its measured need, the coordinator folds it into its class's model at once,
the next file of that class is reserved from the updated model, and the ledger's observation store persists the model
with the observation it came from. The worker's process-tree spans play no part in it.

**Coexistence, as a mechanism.**
- **The allocation.** It is the smaller of two figures: available memory plus the product's own observed residency,
  less the base footprint and margin; and half of that total. It is **re-derived from the kernel's figure before every
  admission** on the ledger, not read once at composition. Counting the product's own residency means its own growth
  never throttles it.
- **The residency added back.** It is the parent's resident set plus every ledger child's live anonymous resident set,
  since a child admitted on the ledger has already taken from the kernel's figure what its reservation stands for.
  Every child the runner starts with a memory reservation (a dependence unit, an external indexer, a language server)
  is read from the runner's tree sample at each admission. The parser workers, whose room the pool holds itself, are
  the sum of their tree samples, handed over as each file's increment is reserved; where any live worker has no
  reading, they are counted instead at their base holdings plus the increments of the parses in flight. Any other
  child with no reading is left out of the sum, never counted as zero or at its reservation, so a missing reading can
  only narrow the allocation.
- **Waiting.** A file whose reservation does not fit waits at the head of the line and is admitted the moment one
  returns. Available memory falling **is** the signal that the user's editor, browser or the agent driving the product
  is competing.
- **What is absent.** There is no count of children, no per-family count, no setting, and nothing is refused.
- **Two coexistence constants, with their reason.** "Half of available" and the margin stay as coexistence constants,
  because no derivation from need exists for them. Need says what the product would use, never what the person's other
  processes will want next, and requirement 11 makes a frozen machine a product defect.
- **The model's design constants, with their reason.** None of them is fitted to a repository.
  - The p99 percentile trades overruns against held-back concurrency, and the overrun target above is stated from it.
  - The 5% bucket width bounds the prediction's rounding to one bucket.
  - The cutover from sample maximum to percentile at 100 observations is where a p99 first rests on more than one
    sample.
  - The half-life, counted in the repository's own files of the language, makes the model forget at the speed the
    repository in front of it produces evidence.

  Each is re-examined when its confirming measurement misses.
- **The alternative, steel-manned.** A cap learned from memory pressure: raise concurrency until the kernel reports
  pressure, then back off. It loses on three counts. It learns the limit by causing the pressure requirement 11
  forbids. It needs a control group per action, which an unprivileged user session on this host class cannot create.
  And the pressure signal exists on one operating system only.

**Scheduling** dispatches files largest-first by source bytes, from one shared queue:
- On one corpus's rows, the compiler checkout's, parse time ranks with source bytes at Spearman ρ 0.82–0.86 per
  language.
- A replay of those rows reaches the makespan lower bound at 16 workers, where path order is 10% over it.
- Over all seven public matrix corpora, per language with at least 30 files, per-file worker wall (parse, lowering
  and analysis) ranks with source bytes at Spearman ρ 0.82–0.99, and a replay of largest-first from one queue
  reaches the makespan lower bound, the larger of the mean load and the largest file, within 0.1% at 4, 8, 16 and
  32 workers on every corpus, where path order is up to 44% over it. Largest-first is confirmed on every class.
- Work stealing is dropped: it solves contention between per-worker queues, which one central queue does not have.
- The worker count is the smaller of the processor count and the allocation divided by observed need.
- The classic longest-processing-time bound is 4/3 − 1/(3m) of optimal.
- Per-function variance is handled by order, not by a finer grain, because a per-function dispatch unit would force a
  shared parse-tree lifetime across workers.

**Wall time is taken out of the store first.** The structural stage measured was serialized on the store's group
mutex, so worker count buys nothing without four changes, in this order:
1. **Batch the resolution.** Resolve a file's candidates in one batched read, not one or two reads per record.
2. **Move the reads.** Run the read-only steps on the reader pool against the last commit, adding one commit at each
   provider boundary.
3. **One writer.** Hand writes to a writer goroutine that owns the connection, a group-lifetime statement cache and the
   per-group seal checks.
4. **Keep the stage open.** Keep the parse stage open across the provider's whole pass over its units. A mid-provider
   drain would re-execute workers.

More concurrency is not the fix: the write-ahead log admits one writer. Passing measurements:
- stage wall ≤ 2 × Σ worker CPU ÷ workers;
- workers started ≤ the maximum in flight;
- writer busy time ÷ stage wall reported, and index build order is the next lever at 80% or more.

**The request surface is pagination, not caps.** `max_depth`, `max_visited`, `max_edges` and `max_bytes` are per-page
work budgets with a continuation cursor, every one defaulting to unlimited, and any reduction is disclosed on the
answer. They are what makes a large answer resumable; removing them would remove resumability, not a cap. The analysis
itself has none.

**At scale, disk breaks first, not memory.** The per-run figure names no file count, so it is the same at 1× and 10×.
The store and the link phase's wall grow linearly with the repository. **The paced reclaimer matters more at scale, not
less.** Deleting the staging database removes a *source* of freed gigabytes, but a retired generation of a repository
10× the reference is still about 13.6 GB. Freeing that in a burst stalls every process on a host of this class about a
minute later, after the product has reported done.

### 6. Seven phases, each with a residue and a measured end

**Amended 2026-10-08.** The band conditions of phases 1, 2 and 3 below are withdrawn (Status): no engine run gates
them. Phases 1 to 3 close together, for all nine languages, when the authored golden tables, derived twice, pass
and the integrated change has had its one test pass, review and fix round. Phase 3's zero-rows condition is met by
construction: the engine's importer no longer publishes the four families. The text below is the plan as accepted
on 2026-09-26; the phase table marks what the amendment changes.

Every gate is stated per **repository class**, never per repository. A class is claimed only on at least two instances
from different language families, and **every** instance must pass, not the average. Two classes cannot be claimed yet,
because each has one pinned instance:
- an unconfigured whole repository, whose one instance is private;
- a typed repository configured by a build step, whose one instance is the compiler-infrastructure corpus.

A second public instance of each is pinned before its gate is claimed.

**The oracle unit per language**, a rule that applies to any repository of the class:

| language | unit |
|---|---|
| Go | the whole package; a file-only parse invents 960 spurious reaching-definition edges from a synthetic package initialiser |
| C/C++ | a component directory holding its headers; header facts form their own band |
| Java | the build module |
| Python | the package |
| JavaScript/TypeScript | the project configuration, never split |
| Rust | none: authored goldens only |

**Choosing units within a class:**
- A unit's edge count × the band must be at least one edge.
- The engine's priced peak must fit the allocation.
- Take the smallest eligible unit, one at least twice its size, the largest that fits, one vendored unit and one
  generated unit.
- A unit the engine crashes on is excluded, never counted as agreement.

**Authored goldens.** An authored golden is a valid equality gate only when:
- every case cites its language-reference anchor;
- two people derive it independently;
- no case is derived from, or corrected against, any producer's output;
- every construct the lowering handles has a case.

Rust goldens are authored from the Rust Reference, with `?` on both `Result` and `Option` mandatory. Java goldens are
authored from the Java Language Specification, chapters 14 and 15. The golden tables that exist now were each derived
once, by the lowering's author. So phase 0's equality gate is not claimed until a second, blind derivation of
every case agrees. That derivation is made from the source text and the reference alone, without the lowering, its
output or the first table.

| phase | what becomes native | what still needs the engine at the end | the measured condition that ends it |
|---|---|---|---|
| **0 — shared core and every lowering** | CFG, post-dominators, control dependence, def-use; the lowering of all nine languages with their authored goldens; the one-pass worker walk and the per-file memory of decisions 1, 2 and 5 | all four families for every language (nothing is published yet) | each authored golden table reproduces **100%** of its cases and emits nothing outside them. Equality is right here and only here, because the corpus is authored rather than observed. Mandatory golden cases: the try operator must yield a control dependence, which the engine does not emit. The benchmark shows the need-derived reservation holding its overrun target over the matrix |
| **1 to 3, as amended 2026-10-08 — all four families, every language at once** | `control_depends_on`, `data_flows_to`, `reads` and `writes` for all nine advertised languages, published by the structural provider (decision 9) | **`calls` only**; the engine's importer publishes none of the four families | the authored golden tables, each derived twice, reproduce every case, and the integrated change has had one test pass, one review and one fix round. No engine run gates it. The rows for phases 1, 2 and 3 below are withdrawn |
| **1 — Go and Rust publish, the calibration gate** (withdrawn) | `control_depends_on` and `data_flows_to` for Go and Rust, published by the structural provider (decision 9) | the same two families for the other seven languages; `reads`/`writes` and `calls` everywhere | per family, the symmetric difference against the engine is **≤ the band measured first from two engine runs on the same units**, per class. Rust's gate is its authored goldens, because the engine has no Rust test corpus and emits no try-operator control dependence. Second required output: **differential-test cost per language**, the number nobody has, from which every later phase is priced |
| **2 — ECMAScript family, and the shared write algebra** (withdrawn) | all four dependence families for JavaScript, TypeScript, TSX, Go and Rust | resolved `calls` everywhere no precise profile applies; all four families for Python, Java, C/C++ | the per-family band gate per class (Rust's by its authored goldens, since no engine band exists for it), **and** on every instance of the class the native pass completes with a lower wall *and* a lower tree-summed peak than the engine on the same units, on the same 250 ms sampler |
| **3 — Python, Java, C/C++ dependence** (withdrawn) | all four dependence families for all nine advertised languages | **resolved `calls` only**, where no precise profile applies | the per-family band gate per language and class; Java control and data dependence are diffed against the engine like any other language, and only Java `calls` is authored. **And** a store query over a fresh index returns **zero** dependence-provider rows of the four dependence kinds at the active generation, which is what licenses deleting the import path |
| **4 — static call linking, the four frontends with no type recovery** | resolved `calls` for C/C++, Go, Rust **and Java**, by the package-scoped linker of decision 9 | resolved `calls` for the ECMAScript family and Python, where no precise profile applies | the native `calls` key set matches the engine's **in-repo-resolved** subset within the per-family band, on pinned corpora of each class. Rust's gate is its authored goldens. Java's gate is an **authored corpus and a capability gain**, not a parity diff, because the engine type-recovers nothing for it |
| **5 — type recovery, the two frontends that have it** | resolved `calls` for all nine languages. Order: ECMAScript family, then Python | **nothing** | per language: the in-repo-resolved band gate, **and** the two per-class call-resolution targets of the measurements section — **≥ 95%** of in-repo-targeted call sites on a configured typed repository, from the call-site join **and** this decision's inference together rather than from the join alone, and **≥ 85%** on an unconfigured or dynamic-language one, both measured by that section's sample method — while the ambiguous syntax-tier population does not rise |
| **6 — the retirement gate** | nothing is ported; the engine, its backend package and its import path are deleted | **nothing** | the five conditions below, simultaneously |

**The retirement gate — all five at once.**

1. **Parity.** For each of the nine advertised languages and each of the five published families, the native key set
   is within the per-family band measured from two engine runs on that language's pinned units, on every instance of
   every claimed class. For `calls`, the comparison is the in-repo-resolved subset. Two exemptions, where no engine
   band exists: Rust's families and Java's `calls` pass by their authored goldens instead.
2. **No capability depends on it.** On every instance of every claimed class, a fresh index with the dependence provider
   absent publishes every capability at the same state as one with it present.
3. **No resolution regression, stated per repository class against the honest denominator.**
   - **The denominator** is call sites whose callee is **defined in a tracked file**, measured by the sample method of
     the measurements section, not every call site.
   - **A configured typed repository** (a project configuration present and the precise indexer run): a fresh index
     resolves **≥ 95%** of in-repo-targeted call sites **through the call-site join together with the index-time
     inference of decision 2**, not through the join alone. The join alone measures 91.70% and the three producers'
     union 94.26%. What closes the gap is hierarchy and flow inference over the sites the indexer emitted no occurrence
     for.
   - **A typed repository without configuration, or a dynamic-language repository:** it resolves **≥ 85%** by the
     language-general inference of decision 2.
   - **Both targets are provisional.** The ≥ 95% was measured on one configured corpus, and the ≥ 85% on one
     unconfigured private instance. Each is re-derived, by the same sample method, as the lowest measured ceiling
     across at least two instances of its class. A class is claimed only when every instance meets the re-derived
     target.
   - Each class is judged on its instances, and the reference repository is one instance of the second class, never the
     definition of the target.
   - Additionally, the syntax-tier ambiguity population has not grown.
4. **Coexistence.** The whole index completes with a tree-summed peak below the standing admission allocation, **Σ reserved ÷ Σ observed
   need per class no higher than phase 0 measured on the same class, with the overrun target of decision 5 met**, and the dependence phase's share
   of the index wall below the engine's measured **74%** on the same instance.
5. **No consumer remains.** The tool lock entry, the provider's backend and its capability mapping are the only
   references left, and all are deleted in the same change: no dead consumer, no unreachable path, and no configuration
   key that nothing reads.

### 7. The oracle is the existing key algebra, and it measures a band

**Amended 2026-10-08.** The band below is withdrawn as a gate for the four file-local families (Status): it needs
two engine runs per unit, and no engine run gates their substitution. The authored goldens, derived twice, are that
gate. What follows records the design of the band as accepted.

The importer already derives an id-independent semantic key per fact. The key covers the label, its owning method's
full name, its file, the operator it was lowered from, its target name, its ordered byte ranges and, for a relation,
both endpoints' published identities. It streams the key set to a sorted file and diffs two sets in one merge pass. A
native producer is a **second producer feeding an existing algebra**, not a new framework.

**What relocates.** The key, the key set, its loader, the diff, the key writer and the delta marker move to the
differential harness when the import package is deleted. This is why the deletion credit in the measurements section is
not the whole package: **339 of those lines relocate rather than die.**

**The comparison key, normalised.**
- The owner's full name and the operator are normalised away.
- Both endpoints become the existing cross-provider declaration key, so the two producers' identities meet.
- Each line of a key file carries the digest and its pre-image, so every mismatch is classified into a signed cause.

**The named causes.**
- For `data_flows_to`: φ-depth, the engine's over-kill, substring over-connection, the depth-8 cutoff, call-site
  endpoints, the Go package initialiser, byte range only, and cross-file globals.
- For control dependence: exit augmentation, try-block modelling, and operator conditions.

**The band.** Two engine runs differ by about 0.01%, and a claim of equality between them is a defect. So every
engine-referenced gate is stated "within the band". The band is measured from two engine runs on the same units
**before** a native run is judged, and it is measured **per fact family**:
- control dependence diverges for normalisation reasons;
- reaching definitions for def/use reasons;
- `reads`/`writes` for resolution reasons.

For `data_flows_to` the band is also **signed and per-cause**, because the formulation change of decision 2 produces
divergence in both directions, and an unsigned aggregate would let an over-connection cancel a miss.

**Offline shadow.** The native side of every comparison is taken **offline, in the harness**: the engine runs whole
units, the native pass runs the same files, and nothing native is attached to a generation until its family graduates
(decision 9).

### 8. Precision names the analysis, not the toolchain

Precision describes the origin of a fact (`internal/model/facts.go`): `compiler`, `language_server`, `static_analysis`,
`syntax`, `heuristic`.

A control dependence computed from a control-flow graph and post-dominators, or a data dependence computed from def-use
chains, **is static analysis**, whichever parser produced the tree it was built from. For four of the six invoked
languages, the engine's own frontends are parsers with no type information behind them, and their facts are published
today at `static_analysis`. The native pass performs the same analysis on the same class of input, so it is published
at **`static_analysis`** too, and a consumer sees no change of label for the four dependence families. The structural
provider's evidence precision therefore becomes per family. Today it is fixed at `syntax`
(`builder.evidence` in `internal/provider/treesitter/facts.go`).

What differs between producers is endpoint *resolution*, and that is already carried separately:
- a `calls` edge whose callee comes from a precise index carries the `compiler` origin on that endpoint;
- a name-joined or hierarchy-resolved callee carries `static_analysis`;
- an ambiguous site publishes its candidate count.

The earlier plan proposed downgrading the native families to `syntax`. That would have labelled the toolchain rather
than the analysis. It would have created a consumer-visible regression for languages that have `static_analysis` today,
and it would have put two labels on one fact class for the length of every gate. It is rejected here. The label stays
what it is, and the retirement gate's parity condition is what proves the label is earned.

### 9. The structural provider publishes the four file-local families; a linking provider publishes `calls`

- **Who publishes.** The structural provider publishes `control_depends_on`, `data_flows_to`, `reads` and `writes` as
  per-file capabilities, from the worker walk of decision 1.
- **Failure scope.** A failure of the pass on a file degrades that file's four capabilities and never fails the unit.
  A lowering or analysis defect that panics is recovered per function in the worker and counts as that failure. It is
  disclosed with the function's byte range, so no construct in a user's code can take down a worker or lose the
  file's structural facts.
  The structural provider is Required, and a failed unit fails the generation
  (`internal/index/generation.go:1392-1394`).
- **`calls`.** A new package-scoped linking provider publishes `calls`. It is keyed on per-file digests of each file's
  export table and call-site table, so an edit inside a function body does not trigger a relink. Unit inputs are files
  today (`internal/index/delta/delta.go:56-72`), so this is a model addition.
- **Per-file reindexing.** The four file-local families reuse structural file units on a fingerprint match.
- **The `dependence` id is never reused.** That keeps two conditions measurable: phase 3's zero dependence-provider rows
  and retirement condition 2.
- **Graduation.** As accepted, a per-language graduation set was to decide which producer publishes during the gates.
  **Amended 2026-10-08:** it is not built. Every language moves at once, so the engine's importer drops the four
  families for every language in one change, and the structural provider publishes them for every language.

**Alternative weighed:** a native backend under the `dependence` id. Its strength is real: none of the nine
consumers keyed on that id would change. It loses on three counts:
- the phase-3 gate and retirement condition 2 become unmeasurable;
- a provider has one invalidation scope, and `dependence` is package-scoped (`Provider.Descriptor` in `internal/provider/dependence/provider.go`);
- it would parse every file a second time.

**Trade-off accepted:** the nine id-keyed consumers learn a second publisher of those families for the length of the
gates. **Amended 2026-10-08:** with no graduation set there is never a second publisher; the four families change
publisher for every language in one change.

### 10. A header's language is decided by the repository, not by its extension

In the build measured, `.h` was mapped to the C grammar alone. In the compiler-infrastructure
corpus, 74.7% of `.h` files parse with errors, against 22.8% of `.c` files. The repository, not the file name, knows
which language its headers are written in. The rule:
1. **One-language repositories.** A repository with C translation units and no C++ ones parses `.h` as C, and one with
   C++ translation units and no C ones parses `.h` as C++.
2. **Mixed repositories.** A repository with both parses `.h` with the C++ grammar first. It is the closer superset of
   the two dialects that occur in headers. A repository with neither parses `.h` with the C++ grammar first, for the
   same reason; the fallback recovers a C header.
3. **Fallback.** A header whose chosen parse has errors is parsed once with the other grammar. The parse with fewer
   error bytes is kept, and the choice is disclosed per file.

**Alternative weighed:** resolve each header's language from the translation units that include it. It is exact, but
it needs every include path, which comes from a build configuration an unconfigured repository does not have. It also
makes a file-local decision depend on a project-wide pass that runs before any header is parsed.

**Trade-off accepted:**
- a second parse of each header whose first parse has errors;
- two neighbouring headers can differ in language.

**Measurement, per class:** on every C/C++ instance, the `.h` parse-error share is no greater than the larger of that
instance's `.c` and `.cpp` shares. No C-only instance's clean `.h` parse changes: the fallback applies in every
repository, so a C-only instance's `.h` parse with errors may be replaced by a C++ parse with fewer error bytes.

## Measurements

Sizing, in Scala lines measured at the pinned tag and Go lines estimated from them:

| component | Scala at the tag | Go estimate |
|---|---|---|
| Control-flow package (12 files: CFG creation, dominators, dominance frontier, control dependence) | 1,294 | 1,200–1,800 |
| Reaching-definitions package (7 files) | 962 | 1,150–1,880 |
| Call linkers in the call-graph layer (dynamic 226, static 41, method-reference 30) | 297 | — |
| Attribution and stub passes (contains-edge 50, method stubs 178, parameter decoration 62) | 290 | — |
| **Subtotal the product consumes directly** | **2,843** | |
| Call-graph linking, shared (layer 30 + three linkers 297 + bare-name linker 29 + linking utilities 150) | 506 | 600–1,110 |
| Type recovery, shared (recovery fixed point 1,331 + symbol table 155 + hint linker 184 + inheritance names 142 + two import passes 87 + stub parser 42) | 1,941 | 2,700–4,690 |
| Type recovery / linking, per language (ECMAScript 1,684 of which a builtin table is 1,094 · Python 641 · Java 113 · C/C++, Go, Rust 0 each) | 2,438 | 3,550–6,050 |
| **Call linking and type recovery, six frontends** | **4,885** | **6,850–11,850** |
| `reads`/`writes`, shared walker plus six families | — | 1,900–3,150 |
| Per-language normalise + def/use | — | 370–920 each |
| Scope/binding resolver where no precise index covers a unit | — | 0, or 400–900 per language |
| Differential harness + corpora | — | 2,500–4,500 + 400–900 per language |
| **Credit: the import and staging package deleted** | — | **−4,535** |

Held out of the sums on purpose: a 633-line closure-and-capture scope manager, because the
scope/binding resolver row already prices it; and 1,628 lines belonging to three frontends the product
never invokes. The familiar "5,413" figure for type recovery is the aggregate of two directories, not
the product's cost. The deletion credit is **4,535**: the package is 4,874 lines of source, less the
339-line comparator that relocates to the oracle; 99 lines of Go fixture source go with it.

Engine cost on the reference repository (13,222 files, 6,663 parsed, 555,588 call sites; largest unit
4,984 files / 157.2 MB):

| | value |
|---|---|
| whole-run wall | 31:38 |
| dependence phase | 23:28 — **74%** |
| the largest JavaScript unit alone | ≈20:20 — **64% of the whole run** |
| that unit's parse attempts | 3:23 and 3:22, both to a deterministic linker crash on legal JavaScript, then 9 children serialised |
| JVM peaks | 9,263 / 8,752 / 8,186 / 8,167 / 6,563 / 4,845 MB |
| export size | 0.65–4.95 GB of CSV, staged at 6.6× |

Call resolution on the same store, at the active generation:

| | sites | edges |
|---|---|---|
| syntax-tier `calls`, `syntax` | 555,588 | 363,750 |
| …resolved, in-file or via import | 75,751 (13.6%) | 51,012 |
| …ambiguous (≈8.96 candidates each) | 20,046 (3.6%) | 10,419 |
| …**unresolved — call through a value** | **459,791 (82.8%)** | 302,319 |
| engine `calls`, `static_analysis` | 217,881 | 115,940 |
| …**resolved to an in-repo definition** | **40,743 (18.7%)** | **27,897 (24.1%)** |
| …to an external or library stub | 177,138 | 88,043 |
| a perfect precise-index run over the whole repository | **4.87–5.25%** of call sites | — |

**The denominator those percentages divide by, and the one they should.** Every share above counts
every call site, including calls whose target is a library, the platform or the runtime, for which no
in-repo definition is the correct answer. The honest denominator — call sites whose callee is
**defined in a file the repository tracks** — was measured rather than argued.

*The method, inlined so this record stands alone.* Two evidence stores, read only through the store's
command-line shell in read-only mode, the product never run against either repository. From each, a **seeded**
sample of syntax-tier call sites at the active generation, rows sorted by `(path, start, end)` inside each stratum so the
draw reproduces: **460 rows** from the reference repository stratified by language (javascript 300 of
526,393 · java 90 of 26,416 · python 45 of 2,291 · typescript 25 of 488, seed 20260916) and **204
rows** from a second corpus stratified by join status and language (seed 20260917). Allocation is
disproportionate and the estimator is stratified, each stratum weighted by its population share, with
4,000-draw bootstrap intervals. Each row's callee was classified by hand from the source at that byte
range in a read-only clone at the snapshot's own commit, as **defined in the repository** (a vendored
or bundled dependency's own source counts, because the producers resolve into those files), **a
third-party library not in the tree**, **platform or runtime**, or **undeterminable without
executing**. All 664 rows passed an offset check. Where a precise index covers the site, the compiler
answers instead of a human: the call-site join's native alias says whether an occurrence exists at the
callee identifier and whether that symbol has a definition occurrence inside a repository document.

| | reference repository — **class (b)**, no configuration for its majority language | second corpus — **class (a)**, configured and precisely indexed |
|---|---|---|
| in-repo-targeted share of call sites | **49.92%** [44.42, 55.33] | **42.81%** [42.04, 43.57] |
| …resolved by the syntax tier | 22.85% [16.70, 29.56] | 25.48% |
| …resolved by the engine | 10.89% [6.31, 15.88] | 23.83% |
| …resolved by the precise join | 0.00% (one profile ran, 17 files) | **91.70%** [90.16, 93.40] |
| …**union of the three** | **28.60%** [21.98, 35.48] | **94.26%** [92.75, 95.75] |
| what closes the rest | field-based resolution **+29.73 pp**, flow inference **+27.41 pp** → **90.07%**; hierarchy +8.66 pp; demand-driven residue 1.27% | hierarchy **+4.27 pp**, flow +1.02 pp → **100%** |

The class-(a) union decomposes as **91.70 pp from the precise join**, 0.91 pp from the syntax tier's
`in-file` state and **1.31 pp from its `import` state**, whose soundness the next paragraph measures at
9 of 38 — so the defensible floor for what is resolved *today* on that class is **92.61%**, and the
≥ 95% target is met by the join **plus** decision 2's inference, never by the join alone. The sites the
join does not answer for are not exotic: all 90 sampled from indexed files are attribute calls whose
receiver the indexer could not type, and 25 of the 38 that target repository definitions need
class-hierarchy analysis through a declared repository type.

On a nine-language fixture with every project configured and every pinned indexer run through the
product's own argv, the join covers **41 of 45** call sites and **23 of the 23** whose callee the
fixture defines — 100%, across all nine advertised languages. That fixture, not either corpus, is what
extends the class-(a) claim beyond one language family.

**Two corrections the sample forces.** The syntax tier's `in-file` state is sound in **52 of 52**
sampled rows, but its `import` state matches the callee's base name against an import statement rather
than reaching a definition, and is right **9 times in 16** and **9 times in 38** on the two corpora —
so the tier resolves about **12.4%** of the reference repository's call sites, not 13.6%, and about
**21.8%** of the second corpus's, not 29.1%. And the reference repository is not a hard case but an
**unconfigured, half-vendored** one: its dependencies are committed to the tree, which is why its
denominator is as high as 49.92% while only about a twentieth of its calls are first-party code
calling first-party code. The denominator is a property of a repository; the **shares against it** are
what transfers, and the targets are set on the shares.

Other published families on the same store: `data_flows_to` 1,983,374 sites / 1,060,579 edges;
`reads` 218,827 / 108,540; `writes` 153,194 / 129,751; `control_depends_on` 101,814 / 59,850.

File-locality fidelity, the engine measured against itself:

| language | control dependence kept | reaching definitions kept |
|---|---|---|
| Python, TypeScript | **100%** | **100%** |
| C, Java | 100% | 100% (1 spurious) |
| Go | 98.3% | 98.5% — a synthetic per-package initialiser, not a property of the language |

Under subdivision of one project: control dependence 99.7%, reaching definitions 99.9%, methods 100%,
**resolved calls 46%**.

Memory: per function the design figure **M_sparse(N) ≈ 96·N + 64 bytes** (0.92 MiB at N = 10⁴; 9.16 MiB
at N = 10⁵), against the dense formulation's 2.34 GiB at N = 10⁵. It is a design figure, not a bound
(§5); `bound_ratio` compares the arena after the three dependence-core passes against it and excludes
the forward dominator pass. Over the public matrix, all nine lowerings, it reaches p99 about 1.4–6.2 on every
population of at least 1,000 functions, and a maximum of about 85. The largest arena is 26.8 MiB. The per-run figure is the observed form of decision 5: it depends on the files in
flight, not on the repository's file count. The per-language parse-memory distribution it is learned from is the table
in the context section. Function sizes measured on this host: this repository 3,272 functions, p50 13 / p99 122
/ max 387 body lines; a large Go library 4,153 functions, p50 9 / p99 187 / max 669; a generated driver
3,609 functions, p99.9 2,499 / max 7,518.

Upstream test coverage for the differential corpus, at the tag, harness fixtures excluded: 9,901
control-flow and dataflow test lines over five frontends — C 3,414 (9 files), ECMAScript 2,157 (5),
Java 1,917 (15), Python 1,347 (1), Go 1,066 (10) — and **zero for Rust**, whose one control-flow-named
test file is about conditional-compilation attributes.

**Recorded as unavailable rather than estimated.** The split of the engine's 18.7% between its hint
linker and its bare-name linker
(the experiment needs an engine run). CFG nodes per body line, which would turn the memory bound in N
into a bound in source lines. The published crossover tables for the near-linear dominator algorithms
(both PDFs 404 at every mirror reachable from this host; one abstract was verified in place of its
tables). The per-file trim and refault cost of decision 2's file boundary, the grammar scanners' allocations for
Python, C++ and Rust (their scanners bypass the counting allocator), and the platform calls for a resettable peak
outside Linux: each is an open measurement of the benchmark task, listed with its reason in the research note of
2026-09-26.

## Alternatives considered

- **Keep the hosted engine and make it incremental.** There is no incremental API; re-applying overlays
  corrupts graphs. Measured waste: one edited file re-runs a whole unit — 16–28 s for a 240k-line Go
  module, 53–65 s for a 500k-line Python tree — to change about one fact row in ten thousand.
- **Keep the hosted engine only for `calls`, which is what the previous plan proposed.** This is the
  alternative with the strongest case: `calls` is the one family a precise index cannot supply, the
  measured precise ceiling is ~5% of call sites, and the port is the most expensive part of the work.
  It is rejected because it is not a plan with an end. It leaves a JVM, its heap sizing, its six
  reverse-engineered failure classes, its CSV export, its staging database and its crash blast radius
  in the product permanently, to serve a family of which **81.3% is external stubs**; and because the
  measurements show the remaining 18.7% is class-hierarchy analysis plus a name join plus two
  propagation rounds, none of which needs a JVM.
- **Split large units to make the engine cheaper.** Rejected by measurement: control and data
  dependence survive a split, call resolution does not — 46% of resolved calls kept.
- **Adopt a different hosted engine.** Two mature code-property-graph projects are permissively
  licensed but JVM-hosted, which reinstates the profile being left. One pattern-and-taint tool's
  community edition is intraprocedural — the same scope the product already imports, so a subprocess
  buys no capability. One query engine's extractors are proprietary and bar the use required here.
- **Take control dependence from an existing graph library.** No permissively licensed Go package
  exposes post-dominators or a dominance frontier in its public API; the one that computes a frontier
  outright is GPL-3.0. What *is* available is unexported BSD-3 source that can be ported, which is what
  decision 4 does.
- **Port the dense reaching-definitions pass as written.** It would give exact oracle parity and its
  quadratic is a *solved production problem* — the engine bounds it and publishes the overrun as
  `partial` with the skipped method names. Rejected because that solution is the cap requirement 8
  forbids, and because the bound's price is dropping every edge of the affected method rather than
  degrading smoothly.
- **A user setting for worker count, memory share or analysis depth.** Rejected by requirement 6: the
  user and the agent tune nothing. The share is a property of coexistence, stated once with its reason.

## Consequences

The product gains control dependence, data dependence, `reads`, `writes` and eventually `calls` with no
JVM, no CSV export, no staging database, no heap-cap sizing, no OOM retry, no subdivision path and no
stderr failure classifier — and it gains the program dependence graph as a surface, which it has never
had. Indexing becomes incremental per file for these families, against a backend that has no
incremental mode at any price. A run's resident memory stops naming the repository.

Against that: two producers for one relation kind during each gate, which is a
verification state only because every gate terminates on a measured condition; a differential corpus
plausibly larger than the implementation, which for Rust and Java cannot be a diff against the engine
at all and must be authored from the language reference; and per-language lowering semantics, where
every reference implementation surveyed is measurably wrong on something.

Two obligations this record creates. The per-file observation, the learned model and the ledger's adjust and
forward-progress rules of decision 5 are built, and the disclosure of [ADR-0010](ADR-0010-engine-memory.md) decision 4
is what makes an overrun of the model visible. And the store changes of decision 5 come before any worker-count gain,
because the structural stage measured was serialized on the store's lock; they are built. And the plan still publishes no native throughput target, because none can be measured before something is
built — what it publishes instead is the instrument and the comparison: phase 2 closes on a lower wall
*and* a lower tree-summed peak than the engine, on the same unit and the same 250 ms sampler.

The engine figures here are taken on one engine version, one host and one reference repository, and the native
figures on the public matrix and one host. Every gate is judged per repository class on its pinned instances, and
each figure is re-measured when the version, the host or an instance changes. No figure in this record is a constant in
code.

## Sources

- F. Chow, S. Chan, S.-M. Liu, R. Lo, M. Streich, *Effective Representation of Aliases and Indirect Memory Operations in SSA
  Form*, Compiler Construction (CC) 1996, LNCS 1060 — the χ (may-definition) and μ (may-use) operators the may-definition row adopts.
- [21-native-engine-need-and-throughput.md](../research/21-native-engine-need-and-throughput.md) and its raw directory
  [native-engine-need](../research/raw/native-engine-need/) — the public corpus matrix with its pins, the per-language
  parse-memory and per-function distributions, the per-file observation mechanisms measured on this host, the
  structural prior's derivation, the store-lock root cause with its file and line, the scheduling replay, the
  publication and oracle design, the per-language unit rule and the golden authoring procedure.
- Linux kernel documentation, *The /proc Filesystem*, the `clear_refs` and `status` tables,
  https://docs.kernel.org/filesystems/proc.html — the per-process peak reset of decision 5.
- Kubernetes, *Vertical Pod Autoscaler — recommender*, https://github.com/kubernetes/autoscaler/tree/master/vertical-pod-autoscaler
  — the decaying histogram and percentile behind the learned model.
- Microsoft, *Memory grant feedback*, https://learn.microsoft.com/sql/relational-databases/performance/intelligent-query-processing-memory-grant-feedback
  — percentile-over-history grants, and the oscillation that motivated percentile over last value.
- Buck2, *Resource control*, https://buck2.build/docs/users/advanced/resource_control/ — the pressure-learned cap
  weighed and rejected in decision 5, and the "first running action is never suspended" forward-progress rule.
- Bazel, *actions.run resource_set*, https://bazel.build/rules/lib/builtins/actions, and Pants, *Global options*,
  https://www.pantsbuild.org/stable/reference/global-options — static per-action resource figures.
- PostgreSQL, *Resource Consumption*, https://www.postgresql.org/docs/current/runtime-config-resource.html — static
  work-memory figures.
- The Go authors, *A Guide to the Go Garbage Collector*, https://go.dev/doc/gc-guide — why `GOMEMLIMIT` stays unset.
- *malloc_trim(3)* and *mallopt(3)*, https://man7.org/linux/man-pages/man3/malloc_trim.3.html,
  https://man7.org/linux/man-pages/man3/mallopt.3.html — returning freed pages at the file boundary and the one-arena
  setting.
- SQLite, *Write-Ahead Logging*, https://www.sqlite.org/wal.html, and *FAQ* item 19,
  https://www.sqlite.org/faq.html#q19 — one writer, and the commit cost behind the store changes of decision 5.
- *The Rust Reference*, https://doc.rust-lang.org/reference/, and *The Java Language Specification*, chapters 14 and 15,
  https://docs.oracle.com/javase/specs/ — the authored goldens of phases 0 and 4.

- [20-native-engine-post-mvp.md](../research/20-native-engine-post-mvp.md) — the full research note this
  record summarises: the source anatomy, the per-family sizing and the phase detail.
- [15-requirements-audit.md](../research/raw/native-engine/15-requirements-audit.md) — the twenty-four
  requirements, the audit of the previous plan against each, and the full-retirement path with its
  per-phase residues and gates.
- [16-passes-callgraph-and-types.md](../research/raw/native-engine/16-passes-callgraph-and-types.md) —
  the call linkers and type-recovery passes inventoried at the tag; which frontends get type recovery;
  where the engine's resolved calls actually come from.
- [17-passes-structural-and-cfg.md](../research/raw/native-engine/17-passes-structural-and-cfg.md) —
  the base and type-relation passes, the CFG creator's exceptional edges and jump machinery, and what a
  parse tree does not supply that the engine's CFG rides on.
- [18-algorithms-dominance-and-dataflow.md](../research/raw/native-engine/18-algorithms-dominance-and-dataflow.md)
  — the per-function algorithm decisions with their citations, the reversed-dominator proof, and the
  per-function memory bound with its arithmetic.
- [19-algorithms-callgraph-and-memory.md](../research/raw/native-engine/19-algorithms-callgraph-and-memory.md)
  — the per-language call-graph decisions, the scheduling and coexistence mechanism, and the whole-run
  memory bound evaluated at 3× and 10×.
- [14-store-counts-reference.md](../research/raw/native-engine/14-store-counts-reference.md) — every call-resolution
  figure in this record, read from two real stores with `sqlite3 -readonly` at the active generation.
- [20-call-ceiling-sample.md](../research/raw/native-engine/20-call-ceiling-sample.md) — the honest
  denominator and the per-class ceiling: the seeded stratified sample of 664 call sites over two
  corpora with every row's class and reason, the call-site join measured on the nine-language fixture
  with every pinned indexer, the per-technique ceiling with each technique mapped onto the language
  families it is stated for, and the method limitations these targets carry.
- [05-file-locality.md](../research/raw/native-engine/05-file-locality.md) and
  [03-parity-and-oracle.md](../research/raw/native-engine/03-parity-and-oracle.md) — the file-locality
  fidelity table, the subdivision parity table and the 0.01% run-to-run band.
- [02-reference-engine-cost.md](../research/raw/native-engine/02-reference-engine-cost.md) — the 31:38 run breakdown,
  the JVM peaks and the two-file reproduction of the deterministic frontend crash.
- [04-requirements-and-engine-cost.md](../research/raw/native-engine/04-requirements-and-engine-cost.md)
  — the engine's memory apparatus, its six failure classes and the staging database, each with the
  product document it comes from.
- [09-native-building-blocks.md](../research/raw/native-engine/09-native-building-blocks.md) — the
  nineteen candidate building blocks with licence and last activity, each fetched at a live URL.
- [05-cfg-cdg-from-treesitter.md](../research/05-cfg-cdg-from-treesitter.md) — the earlier CFG and
  control-dependence sizing and the precision argument this record inherits.
- [00-synthesis.md §8](../research/00-synthesis.md) — the standing direction: the hosted engine
  is the MVP backend, the pinned benchmark corpora are the differential oracle for any future native
  engine, and no analysis limit is lowered.
- [providers-dependence.md](../providers-dependence.md) — what the provider publishes, its failure
  classes, its staging database, and the fact-key algebra the oracle is built on.
- [ADR-0001](ADR-0001-scale-posture.md) — unlimited by default, bounded by page: the posture decisions 5
  and 6 sit under.
- [ADR-0005](ADR-0005-graph-traversal-layout.md) — the published per-generation adjacency and visited
  set, which a native producer changes the producer of, not the layout.
- [ADR-0009](ADR-0009-import-staging.md) — the import staging this decision eventually deletes.
- [ADR-0010](ADR-0010-engine-memory.md) — the heap-from-need rule, the half-the-host allocation and the
  observed-peak disclosure that decision 5 inherits and adapts.
- Cooper, Harvey and Kennedy, *A Simple, Fast Dominance Algorithm*, Rice University TR, 2001 — the
  iterative dominator algorithm of decision 2.
- Lengauer and Tarjan, *A Fast Algorithm for Finding Dominators in a Flowgraph*, ACM TOPLAS 1(1), 1979,
  https://doi.org/10.1145/357062.357071 — the near-linear alternative weighed against it.
- Georgiadis and Tarjan, *Finding Dominators Revisited*, and the semi-NCA variant — the experimental
  comparison the crossover claim rests on.
- Ferrante, Ottenstein and Warren, *The Program Dependence Graph and Its Use in Optimization*, ACM
  TOPLAS 9(3), 1987, https://doi.org/10.1145/24039.24041 — control dependence via post-dominance
  frontiers, and the program dependence graph of decision 3.
- Cytron, Ferrante, Rosen, Wegman and Zadeck, *Efficiently Computing Static Single Assignment Form and
  the Control Dependence Graph*, ACM TOPLAS 13(4), 1991, https://doi.org/10.1145/115372.115320 — the
  dominance-frontier recurrence and SSA placement.
- Braun, Buchwald, Hack, Leißa, Mallon and Zwinkau, *Simple and Efficient Construction of Static Single
  Assignment Form*, Compiler Construction 2013, https://doi.org/10.1007/978-3-642-37051-9_6 — the SSA
  construction of decision 2, which needs no dominance computation.
- Reps, Horwitz and Sagiv, *Precise Interprocedural Dataflow Analysis via Graph Reachability*, POPL
  1995, https://doi.org/10.1145/199448.199462, and Sagiv, Reps and Horwitz's IDE extension — the
  interprocedural framework decision 2 declines.
- Dean, Grove and Chambers, *Optimization of Object-Oriented Programs Using Static Class Hierarchy
  Analysis*, ECOOP 1995, https://doi.org/10.1007/3-540-49538-X_5 — class-hierarchy analysis.
- Bacon and Sweeney, *Fast Static Analysis of C++ Virtual Function Calls*, OOPSLA 1996,
  https://doi.org/10.1145/236337.236371 — rapid type analysis.
- Andersen, *Program Analysis and Specialization for the C Programming Language*, PhD thesis, 1994, and
  Steensgaard, *Points-to Analysis in Almost Linear Time*, POPL 1996,
  https://doi.org/10.1145/237721.237727 — the two whole-program points-to formulations decision 2
  rejects for the dynamic languages.
- Sridharan and Bodík, *Refinement-Based Context-Sensitive Points-To Analysis for Java*, PLDI 2006,
  https://doi.org/10.1145/1133981.1133978 — demand-driven resolution at read time.
- Feldthaus, Schäfer, Sridharan, Dolby and Tip, *Efficient Construction of Approximate Call Graphs for
  JavaScript IDE Services*, ICSE 2013, https://doi.org/10.1109/ICSE.2013.6606621 — field-based call-graph
  construction at index time for the ECMAScript family.
- Graham, *Bounds on Multiprocessing Timing Anomalies*, SIAM Journal on Applied Mathematics 17(2), 1969,
  https://doi.org/10.1137/0117039 — the longest-processing-time bound behind the scheduling order.
- The analysis engine's own source at tag `v4.0.627`, read read-only: the control-flow package
  (`x2cpg/.../passes/controlflow/`, 12 files, 1,294 lines), the reaching-definitions package
  (`dataflowengineoss/.../passes/reachingdef/`, 7 files, 962 lines), the call-graph layer and its four
  linkers (`x2cpg/.../passes/callgraph/`), the base and type-relation passes
  (`x2cpg/.../passes/base/` 590 lines over 10 files, `x2cpg/.../passes/typerelations/` 150 over 3), the
  type-recovery chain (`x2cpg/.../passes/frontend/` and `x2cpg/.../frontendspecific/`), the six
  generator drivers, and the parse and export drivers. Every line count in this record was produced on
  this host with `wc -l` per file or `find … -exec cat {} + | wc -l` per directory.
- Reference implementations read as source in the local Go module cache, never built, resolved or
  benchmarked: `gonum v0.17.0` `graph/flow/{control_flow_lt,control_flow_slt,doc,control_flow_bench_test}.go`;
  `x/tools v0.49.0` `go/ssa/{dom,lift}.go` and `go/callgraph/{cha,rta,vta}`; the Go 1.27.1 runtime and
  `math/bits` sources behind the allocation claims of decision 2.
- Measurement record: the reference repository's indexing runs of 2026-09-16 and the two stores they
  produced, read with `sqlite3 -readonly`; the cap sweep of 2026-09-16 recorded in
  [ADR-0010](ADR-0010-engine-memory.md).
