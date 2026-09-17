# ADR-0012: Dependence is computed in process, and the hosted engine is retired against measured conditions

## Status

Proposed, 2026-09-16. Nothing in this record is started before the MVP ships; the hosted engine
remains the MVP backend and this decision does not reverse that. What is decided now is the shape, the
algorithms, the phase order and — the part the previous plan refused to state — the conditions under
which the hosted engine is deleted.

## Context

### What the product does today

The `dependence` provider runs an external analysis engine, a JVM, once per unit for the parse and once
for the export, under a heap ceiling the provider chooses ([providers-dependence](../providers-dependence.md),
[ADR-0010](ADR-0010-engine-memory.md)). It publishes five capabilities — `control_depends_on`,
`data_flows_to`, `reads`, `writes` and `calls` — every fact at `static_analysis` precision with exact
byte ranges. Those facts are what `codectx_callers`, `codectx_callees`, `codectx_dependency_path` and
`codectx_impact` walk; `codectx_symbol_info` and `codectx_references` report their precision.

The engine is reached through one backend package and consumed through a graph CSV export staged in a
private SQLite database ([ADR-0009](ADR-0009-import-staging.md)). It has no incremental mode: one
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
> (3–10× the size of [the reference repository], a Microsoft-scale enterprise codebase) and prove
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
in the fixture. The tree-sitter tier resolves 13.6% — corrected to about 12.4% below — and leaves
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
  inside the structural provider's existing worker subprocess, which already owns the parse tree
    per function (work and memory)
      normalise [per-language] → CFG → post-dominators → control dependence
        → def/use [per-language] → SSA def-use → emit
per project (the one scope that is not per function)
  calls: the precise index where a profile applies; otherwise the native linker of decision 6
```

The analysis runs **inside the parser worker**, not in the coordinator. Parsing already happens in
isolated worker subprocesses that own every native object; only fact frames cross the wire. Running the
analysis anywhere else would re-parse every file. The worker's per-CPU count, its 256 MiB reservation
and its crash isolation are inherited rather than invented.

**Alternative weighed:** keep the project as the unit, as the engine does. Rejected on the measurement
above — the dependence families are file-local, so a project-sized unit buys nothing for them, and it
is what makes a single crash cost 20 minutes and a whole unit's facts.

**Trade-off accepted:** `calls` still needs a project scope, so one family is scheduled differently
from the other four. That asymmetry is real and is why `calls` is ported last.

### 2. Every algorithm is chosen against a named alternative

| question | decision | why this one, and what is given up |
|---|---|---|
| Dominators | **Cooper-Harvey-Kennedy iterative** over a dense `int32` reverse-post-order, written in the repository | the asymptotic crossover is irrelevant at the measured function-size distribution — p50 13, p99 122, max 387 body lines in this repository; p50 9 / p99 187 / max 669 in a large Go library. Below about 10³ nodes allocation decides, and the near-linear library implementations allocate a heap node with its own map bucket per CFG node. **Given up:** the asymptotic guarantee on a pathological function, and a dependency that would have been free to import |
| Post-dominators | the **same** pass over a **reversed, exit-augmented** CFG | one code path to validate instead of two. A generic implementation reversed this way was verified sound from source — both entry points consult only the successor relation — so this is a choice, not a necessity. The augmentation is mandatory: a unique synthetic exit, plus an edge from every strongly-connected component that cannot reach it, or an infinite loop's nodes silently have no post-dominator. **Given up:** the native set will differ from the engine's *by design* on functions with unreachable-from-exit regions; the differential band must expect that surplus as a named cause |
| Control dependence | **Ferrante-Ottenstein-Warren** via post-dominance frontiers on the exit-augmented CFG, **without** the entry-to-exit edge | it is the textbook set. The engine computes a strict under-approximation in two provable places: the method node's single successor fails the ≥2-predecessor filter, so nothing is control-dependent on entry, and the frontier walk truncates on a missing post-immediate-dominator. **Why it matters more than it sounds:** the product's `control_depends_on` is a single-hop anchored join with no walk, unlike the depth-8 `data_flows_to` walk, so a missing edge is an unrecoverable missing fact rather than a longer path |
| Reaching definitions / def-use | **replace, do not port**: sparse SSA-based def-use, not the dense bit-vector worklist | the dense `in`/`out` is Θ(N²/64) words — **2.34 GiB on one function at N = 10⁵** — which a design with no caps cannot carry. The engine survives it only by having the cap this record forbids. The SSA construction chosen needs no dominance computation for def-use, so the port keeps one dominator computation per function, for control dependence. **Given up:** exact oracle parity. The two formulations differ in three named places, so the `data_flows_to` band becomes signed and per-cause rather than one absolute difference, and the port owns φ-operand resolution |
| Interprocedural framework | **none** — no IFDS, no IDE | realizable-path precision is unobservable at a surface that publishes unlabelled depth-8 reachability, and the exploded supergraph reinstates exactly the whole-program resident structure decision 5 forbids |
| Incrementality | **no incremental dataflow algorithm**; a changed function is recomputed from scratch, and the invalidation boundary is the file's content hash | a function's CFG is tens to hundreds of nodes; the bookkeeping costs more than the recomputation. This is also the model the structural tier already runs |
| In-flight adjacency | plain `int32` compressed sparse row, forward and reverse, no varint | it is built and discarded inside one worker — a different object from the published per-generation adjacency of [ADR-0005](ADR-0005-graph-traversal-layout.md), which is unchanged |
| Allocation | one **reset slab arena per worker**, pointer-free typed backing arrays, 1 MiB release threshold | the multiplier is on the order of 10⁶ functions, and what the arena saves is the collector's scan set. The release threshold is what stops the high-water mark falsifying decision 5. **Given up:** manual lifetime discipline inside the worker; revisit if the function count for a run ever falls to the order of 10⁴ |
| `GOGC` / `GOMEMLIMIT` | **set neither** | the parse-tree layer is cgo, so the dominant per-worker allocation is invisible to the Go runtime's limit. A runtime knob that cannot see the memory it is meant to bound is worse than none |
| Call graph — Java and other hierarchy-typed languages | **class-hierarchy analysis** | decided on **streamability**: its inputs are two tables an ordered merge join consumes, while rapid type analysis needs a reachability fixed point and variable-type analysis a global propagation graph — both whole-program resident state decision 5 forbids. **Given up:** precision on megamorphic sites, which land in the existing ambiguous-candidate family the product already publishes with a candidate count |
| Call graph — Go, Rust, C/C++ | direct binding, plus rapid type analysis's signature-keyed address-taken × indirect-site cross-product **without** its reachability fixed point, plus class-hierarchy analysis for interfaces and traits | most calls are direct; the interesting ones go through interfaces or function values. **The gain is no longer unavailable:** the reference corpus contains no C, C++, Go or Rust file, but a second corpus carries 1,101 Go, 54 C and 1 Rust call sites and the nine-language fixture carries all three configured, where the call-site join resolves 5 of 6 Go, 8 of 9 C/C++ and 6 of 6 Rust sites and **every** site whose callee the fixture defines. What is still unmeasured is a C, C++, Go or Rust corpus large enough to price the cross-product |
| Call graph — ECMAScript family, Python | **no whole-program points-to in any formulation.** Flow-based type inference through assignments, parameters and returns over the def-use chains this table already builds; field-based resolution at index time; class-hierarchy analysis where a subtype relation exists; demand-driven resolution at read time | inclusion-based points-to has the right semantics and a forbidden budget; unification-based has the right budget and precision that collapses on these flow shapes. **The achievable ceiling is now measured, not unavailable.** Against the honest denominator, 28.60% of in-repo-targeted sites are resolved today; field-based resolution accounts for a further **29.73 pp** and flow-based inference for **27.41 pp**, reaching **90.07%**, with class-hierarchy analysis a further 8.66 pp and a **1.27%** residue whose callee identity depends on a call-site-specific value. Neither of the two load-bearing techniques is optional, and neither is a name join: **70.59%** of that repository's call sites carry a callee name some tracked definition also carries, so a resolver keyed on the name alone claims two sites in three and is wrong on most |

**What the engine's own linkers are.** Its static linker is none of the academic algorithms — an exact
full-name equality join. Its dynamic linker **is** class-hierarchy analysis. Its type recovery is
flow-insensitive symbol-table type propagation over a **fixed two iterations**, explicitly not a fixed
point. The measured 18.7% is class-hierarchy analysis plus a name join plus two propagation rounds.
Nothing in it is unreproducible, and no part of it is a reason to keep a JVM.

### 3. The program dependence graph becomes a surface

Control dependence and data dependence are both staged, both in the default traversal set, and keyed by
the same entity pair. The composed graph is therefore a **join in the projection**, not analysis work.
Leaving the caller to compose it by filtering relation kinds is precisely the tuning requirement 6
forbids, so the product composes it. This is the one capability the port **adds** rather than preserves.

### 4. The shared core adds no dependency

The dominator core is written in the repository, so the analysis adds **no module** to `go.mod`. The
parse-tree layer, cgo and the nine grammar registrations (from eight pinned grammar modules) are
already pinned, so a native engine adds **no parser, no runtime, no helper binary and no per-language
toolchain** — the opposite of the hosted engine, whose six frontends drive a C/C++ compiler frontend, a
Java symbol solver and three downloaded helper binaries, plus a JDK.

**A negative this record corrects rather than repeats.** "No permissively-licensed Go package exposes
post-dominators or a dominance frontier" is true only of **public APIs**. One BSD-3 package computes a
Cytron dominance frontier in about 28 unexported lines that can be ported rather than invented. The
must-write list is real and smaller than the earlier plan assumed.

### 5. "No caps" is a property of the design, and coexistence is a mechanism

**Per function**, the structures are bounded by **M_sparse(N) ≤ 96·N + 64 bytes** — 0.92 MiB at
N = 10⁴, which covers the largest function measured on this host under any lines-to-nodes factor up to
about 1.3, and 9.16 MiB at N = 10⁵. The definition count is bounded **structurally**, not by a
constant: a definition is a CFG node index, so D ≤ N. That is the replacement for the engine's
definition cap, whose price is dropping *every* reaching-definition edge of an over-large method.

**Per run:**

> `R_run = B_process + W × M_worker + A_link` = 1.00 GiB + 16 × 256 MiB + 0.0005 GiB ≈ **5.00 GiB**
> at one worker per CPU on a 16-core host.

No term names file count, call-site count or repository bytes, so the figure is **identical at 1×, 3×
and 10×** the reference repository.

**The qualification, stated here rather than in a footnote.** `M_worker` is an **admission estimate,
not an enforced ceiling**: the process runner sums reservations against a budget and there is no
resource limit or control group behind it. A file whose parse tree overruns its reservation does not
fail — it makes `W × M_worker` an under-estimate. `R_run ≈ 5.00 GiB` is therefore an **admission
bound**. A hard per-worker limit is **not** the answer: a limit that fails a file for its size is the
cap requirement 8 forbids, and [ADR-0010](ADR-0010-engine-memory.md) already rules that a unit whose
need exceeds the allocation still runs, whole. The answer is to make the estimate **observed**: the
250 ms tree sampler that already measures every child's peak measures each worker's, the observed
peak per file-size class feeds the next admission in the same run and is persisted with the
generation for the next, a worker whose observed need exceeds the standing allocation is admitted
alone rather than refused, and every admission discloses reservation, observed peak and the drift
between them through decision 4 of the same record. That closes the loop the estimate leaves open
without a single refusal.

**Coexistence, as a mechanism.** Each worker takes its reservation from the standing allocation — the
smaller of available memory less the base footprint and margin, and half of available memory
([ADR-0010](ADR-0010-engine-memory.md) decisions 2 and 5) — **re-derived from the kernel's available-memory
figure between files**. A worker that does not fit waits at the head of the line and is admitted the
moment one returns. Available memory falling **is** the signal that the user's editor, browser or the
agent driving the product is competing. There is no count of children, no per-family count, no setting,
and nothing is refused.

**Scheduling** dispatches files longest-first with work stealing; the classic longest-processing-time
bound is 4/3 − 1/(3m) of optimal. Per-function variance is handled by order, not by a finer grain,
because a per-function dispatch unit would force a shared parse-tree lifetime across workers.

**The request surface is pagination, not caps.** `max_depth`, `max_visited`, `max_edges` and
`max_bytes` are per-page work budgets with a continuation cursor, every one defaulting to unlimited,
and any reduction is disclosed on the answer. They are what makes a large answer resumable; removing
them would remove resumability, not a cap. The analysis itself has none.

**At 10× scale, disk breaks first, not memory.** The store scales linearly, 1.36 GB → 13.64 GB; then
link-phase wall clock. The packed adjacency is 107 MiB at 10×. **The paced reclaimer matters more at
10×, not less**: deleting the staging database removes a *source* of freed gigabytes, but a retired
10×-scale generation is still about 13.6 GB, and freeing that in a burst stalls every process on a host
of this class about a minute later — after the product has reported done.

### 6. Seven phases, each with a residue and a measured end

| phase | what becomes native | what still needs the engine at the end | the measured condition that ends it |
|---|---|---|---|
| **0 — shared core, Rust first** | CFG, post-dominators, control dependence, def-use, emission; the Rust mapping. `control_depends_on` and `data_flows_to` for Rust | `reads`/`writes` and `calls` for Rust; all four families for the other eight languages | the native key set reproduces **100%** of a hand-built golden corpus authored from the language reference, and emits nothing outside it. Equality is right here and only here, because the corpus is authored rather than observed. One golden case is mandatory: the try operator must yield a control dependence, which the engine does not emit |
| **1 — Go, the calibration gate** | the Go mapping; `control_depends_on` and `data_flows_to` for Go | the same, less Rust and Go | per family, the symmetric difference against the engine is **≤ the band measured first from two engine runs on that same corpus**. Second required output: **differential-test cost per language**, the number nobody has, from which every later phase is priced. Corpus units must be whole packages — a file-only Go parse invents 960 spurious reaching-definition edges from a synthetic package initialiser |
| **2 — ECMAScript family, and the shared write algebra** | all four dependence families for JavaScript, TypeScript, TSX, Go and Rust | resolved `calls` everywhere no precise profile applies; all four families for Python, Java, C/C++ | the per-family band gate, **and** the same unit that costs the engine 3:38 and a 5.44 GB process-tree peak completes natively with a lower wall *and* a lower tree-summed peak, on the same 250 ms sampler |
| **3 — Python, Java, C/C++ dependence** | all four dependence families for all nine advertised languages | **resolved `calls` only**, where no precise profile applies | the per-family band gate per language, **and** a store query over a fresh index returns **zero** dependence-provider rows of the four dependence kinds at the active generation — which is what licenses deleting the import path |
| **4 — static call linking, the four frontends with no type recovery** | resolved `calls` for C/C++, Go, Rust **and Java** | resolved `calls` for the ECMAScript family and Python, where no precise profile applies | the native `calls` key set matches the engine's **in-repo-resolved** subset within the per-family band, on pinned corpora — **not** on the reference repository, which contains no C, Go or Rust unit at all; a second corpus supplies 1,101 Go, 362 Java and 54 C call sites and the nine-language fixture supplies all four languages configured, which is where the per-language argv is proved. Java's gate is an **authored corpus and a capability gain**, not a parity diff, because the engine type-recovers nothing for it |
| **5 — type recovery, the two frontends that have it** | resolved `calls` for all nine languages. Order: ECMAScript family, then Python | **nothing** | per language: the in-repo-resolved band gate, **and** the two per-class call-resolution targets of the measurements section — **≥ 95%** of in-repo-targeted call sites on a configured typed repository and **≥ 85%** on an unconfigured or dynamic-language one, measured by that section's sample method — while the ambiguous syntax-tier population does not rise above its measured 20,046 sites / 179,626 candidate edges |
| **6 — the retirement gate** | nothing is ported; the engine, its backend package and its import path are deleted | **nothing** | the five conditions below, simultaneously |

**The retirement gate — all five at once.**

1. **Parity.** For each of the nine advertised languages and each of the five published families, the
   native key set is within the per-family band measured from two engine runs on that language's pinned
   corpus. For `calls`, the comparison is the in-repo-resolved subset.
2. **No capability depends on it.** A fresh index of the reference repository with the dependence
   provider absent publishes every capability at the same state as one with it present.
3. **No resolution regression, stated per repository class against the honest denominator.** The
   denominator is call sites whose callee is **defined in a tracked file**, measured by the sample
   method of the measurements section, not every call site. On a **configured typed repository** — a
   project configuration present and the precise indexer run — a fresh index resolves **≥ 95%** of
   in-repo-targeted call sites, the precise join carrying compiler precision wherever it reaches. On a
   **typed repository without configuration, or a dynamic-language repository**, it resolves
   **≥ 85%** by the language-general inference of decision 2. The reference repository is the instance
   of the second class these numbers were measured on, never the definition of the target. The
   absolute floor this condition used to carry — 40,743 sites and 27,897 edges — is **withdrawn**: it
   was a count on one repository at one generation, and a count cannot be met on a second repository
   at all. Additionally, the tree-sitter ambiguity population has not grown.
4. **Coexistence.** The whole index completes with a tree-summed peak below the standing admission
   allocation, **no single reservation larger than one worker's**, and the dependence phase's share of
   the index wall below its measured **74%**.
5. **No consumer remains.** The tool lock entry, the provider's backend and its capability mapping are
   the only references left, and all are deleted in the same change — no dead consumer, no unreachable
   path, no configuration key nothing reads.

### 7. The oracle is the existing key algebra, and it measures a band

The importer already derives an id-independent semantic key per fact — the label, its owning method's
full name, its file, the operator it was lowered from, its target name, its ordered byte ranges and,
for a relation, both endpoints' published identities — streams the key set to a sorted file and diffs
two sets in one merge pass. A native producer is a **second producer feeding an existing algebra**, not
a new framework. This is why the deletion credit in the next section is not the whole package: **339 of
those lines relocate rather than die.**

Because two engine runs differ by about 0.01% and a claim of equality between them is a defect, every
engine-referenced gate is stated "within the band", the band is measured from two engine runs on the
same corpus **before** a third native run is judged, and it is measured **per fact family** — control
dependence diverges for normalisation reasons, reaching definitions for def/use reasons,
`reads`/`writes` for resolution reasons. For `data_flows_to` the band is additionally **signed and
per-cause**, because the formulation change of decision 2 produces divergence in both directions and an
unsigned aggregate would let an over-connection cancel a miss.

### 8. Precision names the analysis, not the toolchain

Precision describes the origin of a fact (`internal/model/facts.go`): `compiler`, `language_server`,
`static_analysis`, `syntax`, `heuristic`. A control dependence computed from a control-flow graph and
post-dominators, or a data dependence computed from def-use chains, **is static analysis** whichever
parser produced the tree it was built from. The engine's own frontends for four of the six invoked
languages are parsers with no type information behind them, and their facts are published today at
`static_analysis`; the native pass performs the same analysis on the same class of input, so it is
published at **`static_analysis`** too, and a consumer sees no change of label for the four dependence
families. What differs between producers is endpoint *resolution*, and that is already carried
separately: a `calls` edge whose callee comes from a precise index carries the `compiler` origin on
that endpoint, a name-joined or hierarchy-resolved callee carries `static_analysis`, and an ambiguous
site publishes its candidate count. The earlier plan proposed downgrading the native families to
`syntax`; that would have labelled the toolchain rather than the analysis, created a consumer-visible
regression for languages that have `static_analysis` today, and put two labels on one fact class for
the length of every gate. It is rejected here. The label remains what it is; the retirement gate's
parity condition is what proves the label is earned.

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
| tree-sitter `calls`, `syntax` | 555,588 | 363,750 |
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

*The method, inlined so this record stands alone.* Two evidence stores, read with `sqlite3 -readonly`
only, the product never run against either repository. From each, a **seeded** sample of tree-sitter
call sites at the active generation, rows sorted by `(path, start, end)` inside each stratum so the
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

Memory: per function **M_sparse(N) ≤ 96·N + 64 bytes** (0.92 MiB at N = 10⁴; 9.16 MiB at N = 10⁵),
against the dense formulation's 2.34 GiB at N = 10⁵. Per run **≈5.00 GiB** at 16 workers, identical at
1×, 3× and 10×. Function sizes measured on this host: this repository 3,272 functions, p50 13 / p99 122
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
tables). The size at which a parse tree overruns its worker's reservation.

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

Two obligations this record creates rather than discharges. The whole-run figure is an admission bound
and not an enforced one, so the observed-reservation loop of decision 5 is required work, and until
it exists the disclosure of [ADR-0010](ADR-0010-engine-memory.md) decision 4 is what makes drift
visible. And the plan still publishes no native throughput target, because none can be measured before something is
built — what it publishes instead is the instrument and the comparison: phase 2 closes on a lower wall
*and* a lower tree-summed peak than the engine, on the same unit and the same 250 ms sampler.

The constants and measurements here are taken on one engine version, one host and one reference
repository, and are re-measured when any of the three changes.

## Sources

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
- [14-store-counts-r3.md](../research/raw/native-engine/14-store-counts-r3.md) — every call-resolution
  figure in this record, read from two real stores with `sqlite3 -readonly` at the active generation.
- [20-call-ceiling-sample.md](../research/raw/native-engine/20-call-ceiling-sample.md) — the honest
  denominator and the per-class ceiling: the seeded stratified sample of 664 call sites over two
  corpora with every row's class and reason, the call-site join measured on the nine-language fixture
  with every pinned indexer, the per-technique ceiling with each technique mapped onto the language
  families it is stated for, and the method limitations these targets carry.
- [05-file-locality.md](../research/raw/native-engine/05-file-locality.md) and
  [03-parity-and-oracle.md](../research/raw/native-engine/03-parity-and-oracle.md) — the file-locality
  fidelity table, the subdivision parity table and the 0.01% run-to-run band.
- [02-r3-engine-cost.md](../research/raw/native-engine/02-r3-engine-cost.md) — the 31:38 run breakdown,
  the JVM peaks and the two-file reproduction of the deterministic frontend crash.
- [04-requirements-and-engine-cost.md](../research/raw/native-engine/04-requirements-and-engine-cost.md)
  — the engine's memory apparatus, its six failure classes and the staging database, each with the
  product document it comes from.
- [09-native-building-blocks.md](../research/raw/native-engine/09-native-building-blocks.md) — the
  nineteen candidate building blocks with licence and last activity, each fetched at a live URL.
- [05-cfg-cdg-from-treesitter.md](../research/05-cfg-cdg-from-treesitter.md) — the earlier CFG and
  control-dependence sizing and the precision argument this record inherits.
- [00-synthesis.md §8](../research/00-synthesis.md) — the standing direction ruling: the hosted engine
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
