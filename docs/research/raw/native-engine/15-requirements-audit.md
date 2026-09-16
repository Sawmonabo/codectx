# Native-engine research, raw evidence — the product owner's requirements audited against the post-MVP plan, and the full-retirement path

This file audits `docs/research/20-native-engine-post-mvp.md` against the product owner's stated
requirements for a native engine, and supplies the one thing that note does not: an ordered path at
the end of which the JVM-hosted engine is **gone**, with a measured condition closing every phase.

Nothing here is an opinion about effort. Every line count in this file was run on this host with
`find … -exec cat {} + | wc -l` for aggregates and explicit `wc -l` for itemized rows. Every store
figure is quoted from `14-store-counts-r3.md`, which read the reference repository's two stores with
`sqlite3 -readonly` at the active generation. The engine clone was read at the pinned tag and never
built or run.

## Citations, both forms

Every engine citation this file makes appears here with the elided form the record-facing documents
use. The body cites the elided form; this table is where the two meet.

| research form (clone-relative, at the pinned tag) | record-safe form / role name |
|---|---|
| `joern-cli/frontends/x2cpg/src/main/scala/io/joern/x2cpg/passes/controlflow/` (12 files) | `x2cpg/.../passes/controlflow/` — the control-flow package |
| `joern-cli/frontends/x2cpg/src/main/scala/io/joern/x2cpg/passes/callgraph/StaticCallLinker.scala` | `x2cpg/.../passes/callgraph/StaticCallLinker.scala` — the static call linker |
| `joern-cli/frontends/x2cpg/src/main/scala/io/joern/x2cpg/passes/callgraph/DynamicCallLinker.scala` | `x2cpg/.../passes/callgraph/DynamicCallLinker.scala` — the dynamic call linker |
| `joern-cli/frontends/x2cpg/src/main/scala/io/joern/x2cpg/passes/callgraph/MethodRefLinker.scala` | `x2cpg/.../passes/callgraph/MethodRefLinker.scala` — the method-reference linker |
| `joern-cli/frontends/x2cpg/src/main/scala/io/joern/x2cpg/passes/base/{ContainsEdgePass,MethodStubCreator,MethodDecoratorPass}.scala` | `x2cpg/.../passes/base/` — the attribution and stub passes |
| `dataflowengineoss/src/main/scala/io/joern/dataflowengineoss/passes/reachingdef/` (7 files) | `dataflowengineoss/.../passes/reachingdef/` — the reaching-definitions package |
| `console/src/main/scala/io/joern/console/cpgcreation/{C,Go,Rust,JsSrc,PythonSrc,JavaSrc}CpgGenerator.scala` | the six per-language generator drivers |
| `io.joern.x2cpg.passes.frontend.XTypeRecovery` and `…/frontendspecific/` | the type-recovery fixed point and its per-language overrides |

## Measurements this file establishes or re-establishes

| quantity | measured here | previously published |
|---|---|---|
| the control-flow package | **1,294** lines, 12 files | 1,284 (doc 20 line 98) — **wrong** |
| the passes the product consumes, subtotal | **2,843** lines | 2,833 (doc 20 lines 28, 102) — **wrong**, by the same 10 |
| the reaching-definitions package | 962 lines, 7 files | 962 — confirmed |
| the three call linkers | 297 lines (226 + 41 + 30) | 297 — confirmed |
| the attribution and stub passes | 290 lines (50 + 178 + 62) | 290 — confirmed |
| the Neo4j-CSV importer package's own source | **4,874** lines (3,446 non-test + 1,428 test) | 4,874 / 3,446 / 1,428 — confirmed |
| …the same package under this lane's aggregate method | **4,973** lines | not published |

The two importer figures differ because the aggregate method (`find … -name '*.go' -exec cat {} +`)
also reaches 99 lines of Go **fixture source** under the package's `testdata/` tree, which the
per-file `wc -l` of the package directory does not. Both are correct at their stated scope. **The
line credit for retirement is the package's own source, 4,874**, because the fixture trees are
deleted with it but are not code anybody wrote to consume the export; they are named separately in
the retirement gate so the credit cannot be read two ways.

The type-recovery override finding was re-verified directly in the six generator drivers: the
JavaScript/TypeScript, Python and Java drivers each override the post-processing hook to run type
recovery; the C, Go and Rust drivers do **not** define it at all. **For C/C++, Go and Rust the
engine's `calls` is only what the static call linker resolves from a frontend-minted full name.**
This is the fact that splits the retirement path in three, and it is the finding on which Part 2
turns.

---

## Part 1 — the audit

`verdict` is one of `meets`, `partially meets`, `silent`, `contradicts`. `silent` cites the section
where the requirement should have been answered. Line numbers are lines of
`docs/research/20-native-engine-post-mvp.md`.

| # | requirement | the plan's position | verdict | evidence |
|---|---|---|---|---|
| R1 | Control-flow graph, natively | §7.1 line 321 (pipeline stage); §7.2 lines 351-352 (must-write list); §7.5 line 389 (1,200–1,800 Go lines) | meets | the control-flow package is 1,294 lines / 12 files, measured; the CFG core is frozen across 527 patch releases but the CFG creator moved +19.1% (§1 line 56). Feeds no tool on its own — it is the substrate R2 and R3 are computed on |
| R2 | Control dependence | §7.1 line 322; §7.2 line 352 names Ferrante control dependence as must-write; §7.5 line 389 | meets | file-only parses keep **100%** of CDG for Python, TypeScript, C and Java and 98.3% for Go (`05-file-locality.md`); 99.7% under subdivision (`03-parity-and-oracle.md`). Feeds `codectx_callers`, `codectx_callees` and `codectx_dependency_path` through the `control_depends_on` kind, which is in the default traversal set |
| R3 | Reaching definitions | §4 lines 179-225 (algorithm, sized); §7.1 line 322; §7.5 line 390 | meets | the reaching-definitions package is 962 lines / 7 files, measured; file-only fidelity 100% for Python/TypeScript/C/Java, 99.9% under subdivision. Feeds the same three traversal tools through `data_flows_to`, also in the default set |
| R4 | **Program dependence graph as a first-class product surface** | **absent.** The only occurrence of the token in the note is line 447, where a PDG *export format* is listed as a dead end because it is unimplemented for CSV. Belongs in §7.1 lines 319-324 | **silent** | the pipeline at lines 320-322 emits four fact families and never composes control dependence with data dependence; `codectx_dependency_path`, `codectx_callers` and `codectx_callees` can already be filtered to `control_depends_on` + `data_flows_to` (both are in the default traversal set), so the walk is expressible and no surface names it |
| R5 | **Call-graph linking, per language** | §7.1 line 323 leaves `calls` on the engine "unchanged"; §0 finding 1 lines 31-32 calls it "the one fact family a native dependence engine would not take"; §7.6 lines 431-433 defers the question | **contradicts** | the linkers are **297** measured lines (dynamic 226, static 41, method-reference 30) plus 290 lines of attribution and stub passes; §7.5's effort table sizes **no** Go port of any of them. `calls` feeds `codectx_callers`, `codectx_callees`, `codectx_dependency_path` and — as a ranked expansion kind — `codectx_impact`; it is also what `codectx_symbol_info` and `codectx_references` report a precision for |
| R6 | **Type-recovery heuristics** | §2 lines 104-111 classes the 5,413 lines as "dead weight or serves `calls` in dynamic languages"; not sized anywhere in §7.5 | **contradicts** | 5,413 lines (1,331 generic + 4,082 per-language, of which a builtin table is 1,094); the C, Go and Rust drivers do not override it, so it is load-bearing for exactly three of the nine advertised languages — and those three are 97.1% of the reference repository's call sites. It feeds no tool directly; it is what makes the `calls` those four tools serve resolvable |
| R7 | "Anything else we are missing that would benefit or be critical" | §5 lines 229-260 sizes `reads`/`writes`, which the owner did not name — so the note does exceed the named list once; it states no standing method for the question | partially meets | `reads`/`writes` come from the dependence provider alone: no SCIP indexer sets a write role, measured on six indexers (`14-store-counts-r3.md` Q5). `reads` and `writes` feed `codectx_impact` (both are in its ranked expansion set) and the traversal tools |
| R8 | **Rip it out — the engine is retired** | §7.6 lines 431-433: "the engine's only remaining role is `calls` for units no precise indexer covers… Whether to keep it at all is a separate, later decision that this plan does not make" | **contradicts** | the goal is stated as retirement; the note ends with an ungated, undated residue. Part 2 replaces it |
| R9 | Do it in a faster, more performant way | §7.1 lines 326-338 argues structurally (per-function work, no CSV staging, no JVM); §7.5 is an effort table | partially meets | the note publishes **no** speed target and no native wall or RSS figure anywhere. The engine-side number it does carry is the reference-repository dependence phase at **74% of a 31:38 run**, one unit alone 64% (line 425) |
| R10 | Leverage breakthrough research, algorithms, traversals, advanced memory and CPU technique | §7.2 lines 341-358 (library survey, verified negative on post-dominators); §4 lines 183-188 (textbook forward may-analysis) | partially meets | the algorithm selection is the reference implementation's own — bit-vector worklist, union meet, reverse post-order seeding. Nineteen candidates were checked for *availability*; none was checked for *superiority*. Sizing a better formulation is not this file's scope; requiring one is |
| R11 | Greenfield, in place, no compatibility layer | §0 line 43 ("a phased post-MVP plan, not a replacement"); §7.6 lines 435-438 accept two producers for one relation kind at two precisions during each gate | partially meets | two producers behind a *measured, terminating* gate is a verification state, not a compatibility layer, and is legitimate. The note's last gate does not terminate (R8), which is what turns it into one |
| R12 | Always choose the algorithm, store and design that meet the requirements | §7.2 lines 341-346 choose the graph library and record why | partially meets | the note makes one library choice and one verified negative. It selects no store for native facts (the existing store is assumed silently) and states no selection criterion for the dataflow formulation beyond reproducing the engine's |
| R13 | Research is taken as truth when self-verified and the result holds | §7.3 lines 360-374 (the oracle); the verification ledger, lines 456-489 | meets | the ledger separates measured, read-at-the-tag, network-verified, not-determinable and weak, and demotes its own single-sample inference about reversed-edge post-dominators to "weak" (lines 486-488) |
| R14 | An AI uses the MCP surface and gets what it needs immediately, tuning nothing | belongs in §7.1/§7.4. The note never describes the MCP surface after the port | **silent** | the surface already resolves every bound from configuration, defaults them all to unlimited, and discloses any reduction it applies as a notice on the answer; the note neither inherits that obligation nor names a single tool |
| R15 | Works on 3–10× the reference repository | §7.1 lines 326-328 claim nothing in the design grows with repository size | partially meets | the per-worker claim is correct **and incomplete**: it is a claim about the analysis, and several retained structures scale with the repository (Part 3). No absolute target is stated |
| R16 | Prove blazing-fast speed | §7.5 lines 387-395 prices effort, not speed | **silent** | the note sets no throughput target, names no instrument for one, and closes no phase on one. The only time figures in it are the engine's |
| R17 | Prove memory efficiency | §7.1 lines 326-332: memory per worker is one file's syntax tree plus one function's graph and bitsets | partially meets | the shape is right and no number is attached — no per-worker byte figure, no worker count, no admission rule. The note does not cite the record that currently governs the host's memory share |
| R18 | Out of the box: no caps, no time limits, no max-visited | §7.1 lines 327-328 name "no caps, no subdivision" as a property rather than a policy | partially meets | correct for the analysis and unreconciled with the request surface: `max_depth`, `max_visited`, `max_edges` and `max_bytes` all exist on today's tools. Part 3 resolves each one, and states what replaces the engine's definition bound |
| R19 | Leverage **all** tools together — the engine, language servers, precise indexers, dependency manifests, providers | §7.1 line 323 and §6 compose the engine, precise indexers and tree-sitter for `calls` | partially meets | language servers appear only as a measured negative (§6 lines 304-308: **2 references** on the reference repository's largest project, cold and after a 30 s settle, from two different servers); the manifest provider never appears; neither is given a role in the native design |
| R20 | Complete full repo mapping, repo map graph, call stack, flows | §7.1's four families are the flow and call-stack inputs | partially meets | the repository-map surface (`codectx_repo_overview`) and the whole session family (`codectx_context_*`) are never named; the composed dependence surface is missing outright (R4) |
| R21 | Let AI agents identify solutions and bugs and plan better | nowhere. Belongs beside §7.4, which is the only consumer-facing section | **silent** | the note is written as a port and states no consumer-visible capability the port must preserve or add. `codectx_impact`, `codectx_dependency_path` and the session family are not named once |
| R22 | Freezing or crashing the person's machine is a product defect | §7.1 lines 330-332: the engine's memory apparatus "has no native equivalent" | partially meets | true of the JVM's apparatus and not a bound. What actually stalls a host of this class is a large, unpaced free of disk blocks and an unbounded resident set — neither is a JVM property, and the note proposes no native bound for either |
| R23 | Coexist with the user's other processes **and with the agent driving the product over MCP** | belongs in §7.1 lines 326-332 | **silent** | the standing rule is an admission allocation of the smaller of (available memory − base footprint − margin) and **half of available memory**, with every heavy child admitted against that one allocation and no count of children. The note cites that record nowhere and proposes no successor to it |
| R24 | Be the most advanced technology in the industry | §7.2 lines 348-358 (the verified negative and the nineteen-candidate survey) | partially meets | the survey proves a gap in what is *available* — no permissively licensed Go package exposes post-dominators or a dominance frontier. It does not establish a leading position, and §7.5 line 397 frames the ambition defensively |

**Verdict counts: 4 `meets`, 12 `partially meets`, 5 `silent`, 3 `contradicts`** over 24 rows. R5 and
R6 are one sizing decision read twice — the plan sizes no port of the call linkers or of type
recovery. R8 is a different failure: the plan declines to make the retirement decision at all. Part 2
is the replacement for all three.

---

## Part 2 — the full-retirement path

The plan under audit leaves `calls` on the engine indefinitely. The requirement names call-graph
linking per language and type-recovery heuristics explicitly, and the end state is retirement. This
is that path.

**The three-way split that makes it tractable.** `calls` is not one problem:

1. **C/C++, Go and Rust** — the engine gives them only the static linker over a frontend-minted full
   name, verified in their generator drivers. The native replacement is 41 measured Scala lines of
   linking plus the full-name synthesis, not a type system.
2. **JavaScript/TypeScript, Python and Java** — the three drivers that override post-processing.
   Here type recovery *is* the value, and it is the last thing to be ported.
3. **The target is not the engine's whole call graph.** On the reference repository the engine
   resolves **18.7% of its own call sites and 24.1% of its edges** to an in-repo definition
   (40,743 / 27,897 at the active generation); the remaining 177,138 sites point at external or
   library stubs, which reproduce no capability. **Every `calls` gate below is stated on the
   in-repo-resolved subset**, which is the only part a consumer can act on.

**The instruments.** Four, and no others; all four already exist.

- **The fact-key algebra.** The importer already derives an id-independent semantic key per fact and
  diffs two sorted key sets in one merge pass; one measured pair of independent engine runs over one
  unchanged package published the same 6,842 keys (`03-parity-and-oracle.md`). A native producer is a
  second producer feeding it.
- **The band.** Two engine runs over the same unmodified tree differ by about **0.01%**, and a claim
  of *equality* between two engine runs is itself a defect. Every engine-referenced gate below is
  "within the band", the band is measured from two engine runs on the same corpus **before** a third
  native run is judged against it, and it is measured **per fact family**, because the families
  diverge for different reasons.
- **A store query.** One `sqlite3 -readonly` conditional-aggregation pass scoped through the active
  generation's unit set, the shape `14-store-counts-r3.md` already runs.
- **Wall and process-tree resident memory**, tree-summed and sampled at 250 ms, the sampler the
  memory record's cap sweep already used.

### The phases

| phase | what is ported; what becomes native | what still needs the engine at the end of it | the measured condition that ends it | the risk that sends it back |
|---|---|---|---|---|
| **0 — the shared core, Rust first** | CFG, post-dominator tree, dominance frontier, Ferrante control dependence, the reaching-definitions solver, emission; the Rust normalisation and def/use rules. Native: `control_depends_on` and `data_flows_to` for Rust, reaching `codectx_callers`/`codectx_callees`/`codectx_dependency_path` when filtered to those kinds | `reads` and `writes` for Rust; resolved `calls` for Rust; **all four families for the other eight languages** | The native key set over a **hand-built golden corpus authored from the language reference** reproduces 100% of the golden keys and emits none outside it. Equality is correct here and only here, because the corpus is authored rather than observed — the band is a property of the engine and there is no engine run to take it from. One golden case is mandatory: the try operator must yield a control dependence, which the engine does not emit | The corpus is the only oracle and nothing checks it. Every golden case must cite the clause of the language reference it encodes, or the phase has proved a tautology |
| **1 — Go, the calibration gate** | The Go normalisation and def/use rules. Native: `control_depends_on` and `data_flows_to` for Go | `reads`/`writes` for Rust and Go; resolved `calls` for every language; all four families for the remaining seven | Per fact family, on the pinned corpus: the symmetric difference between the native key set and the engine's is **≤ the band measured first from two engine runs on that same corpus**. The phase's required second output is the number nobody has — **measured differential-test cost per language** (corpus lines authored and hours spent), from which every later phase is priced | A file-scoped corpus pollutes the band: a file-only Go parse invents **960 spurious reaching-definition edges (+14%)** from the frontend's synthetic per-package initialiser. Corpus units must be whole packages, where the same measurement is clean |
| **2 — JavaScript/TypeScript dependence, and the shared write algebra** | The ECMAScript-family normalisation and def/use; the shared operator-target walker and the write-form rules for the ECMAScript family, Go and Rust. Native: all four dependence families for JavaScript, TypeScript, TSX, Go and Rust | Resolved `calls` for every language **where no precise profile applies** — which on the reference repository is 94.75–95.13% of its call sites, since a perfect precise run reaches only 4.87–5.25%; all four families for Python, Java and C/C++ | (a) the per-family band gate of phase 1, on the pinned JavaScript/TypeScript corpus; **and** (b) the same unit that costs the engine **3:38 and a 5.44 GB process-tree peak** at its chosen 4 GiB ceiling completes natively with a **lower wall and a lower tree-summed peak**, both sampled at 250 ms by the same instrument | The four already-measured write-form gaps (swapped multiple assignment, tuple-target destructuring, static and package-global field access, unresolved mutable statics) are reproduced rather than fixed, and the reference implementation's own frontend crashes deterministically on legal JavaScript — so the engine baseline for this band is itself partial and must be scoped to the parts that completed |
| **3 — Python, Java, C/C++ dependence** | Their normalisation, def/use and write forms. Native: all four dependence families for all nine advertised languages | **Resolved `calls` only**, and only for the units **where no precise profile applies** — for JavaScript units on the reference repository that is every one of them, because it carries no TypeScript project file anywhere | (a) the per-family band gate per language; **and** (b) a store query over a fresh index shows **zero** dependence-provider rows of kind `control_depends_on`, `data_flows_to`, `reads` or `writes` at the active generation — the engine's dependence output is no longer consumed anywhere, which is what lets the import path be deleted | C/C++ is the one family with no per-file unit: the include closure is the unit. If the native def/use cannot be computed per translation unit without the closure, this phase keeps a project-scoped native pass and must say so rather than quietly widening the worker's memory |
| **4 — static call linking for the three families with no type recovery** | The static call linker, the method-reference linker and the attribution and stub passes — **361 measured Scala lines** — plus per-language full-name synthesis over the syntax tree. Native: resolved `calls` for C/C++, Go and Rust | Resolved `calls` for **JavaScript/TypeScript, Python and Java units where no precise profile applies** — the three whose drivers override post-processing, i.e. exactly where type recovery is the value. Java is the sharpest case: a project file exists and the indexer still emits no occurrence at a call site, so all 26,416 of its sites are in this residue | On a pinned corpus per language, the native `calls` key set matches the engine's **in-repo-resolved** subset within the per-family band. This phase must be gated on corpora and **not** on the reference repository, where it carries zero measured weight: that repository has no C, Go or Rust unit in either store | A full name is a frontend product, not a language property. Over a syntax tree the product mints its own, and a mismatch produces a **wrong** edge rather than a missing one — invisible to a consumer, and the same failure class already measured at 19% wrong call edges when per-file graphs were spliced |
| **5 — type recovery, per dynamic language** | The type-recovery fixed point and its per-language overrides — **5,413 measured lines**, of which a builtin table is 1,094 — and the dynamic call linker, 226 lines. Order: JavaScript/TypeScript, then Python, then Java. Native: resolved `calls` for all nine languages | **Nothing** | Per language, both must hold: (a) the native `calls` key set reproduces the engine's **in-repo-resolved** subset within the per-family band on the pinned corpus; **and** (b) on a fresh index of the reference repository, a store query returns in-repo-resolved `calls` **≥ 40,743 sites / 27,897 edges** for the languages ported, while the ambiguous syntax-tier population does not rise above its measured 20,046 sites / 179,626 candidate edges | Type recovery's output is a heuristic fixed point: "within the band" can mean faithfully reproducing wrong edges. The gate is stated on the in-repo-resolved subset and **never** on the 177,138 external stub sites, because reproducing a stub reproduces no capability |
| **6 — the retirement gate** | Nothing is ported. The engine, its backend package and its import path are deleted | Nothing | The five conditions below, all simultaneously | Below |

### The retirement gate

The JVM-hosted engine is removed from the product when **all five** hold at once:

1. **Parity.** For each of the nine advertised languages and each of the five fact families
   (`control_depends_on`, `data_flows_to`, `reads`, `writes`, `calls`), the native key set is within
   the per-family band measured from two engine runs on that language's pinned corpus. For `calls`
   the comparison is the in-repo-resolved subset.
2. **No capability depends on it.** A fresh index of the reference repository with the dependence
   provider absent publishes every capability at the same state as one with it present. No capability
   degrades to `partial` or `unavailable` for the engine's absence. Instrument: a store query over
   `generation_capabilities` at the active generation, compared between the two runs.
3. **No resolution regression.** In-repo-resolved `calls` in the fresh store is **≥ 40,743 sites and
   ≥ 27,897 edges**, and the tree-sitter ambiguity population has not grown.
4. **Coexistence.** The whole index completes with a tree-summed peak below the standing admission
   allocation (the smaller of available memory less the base footprint and margin, and half of
   available memory), with **no single reservation larger than one worker's**, and the dependence
   phase's share of the index wall is below its measured **74%**.
5. **No consumer remains.** The tool lock entry, the provider's backend and its capability mapping
   are the only references to the engine in the tree, and all are deleted in the same change — no
   dead consumer, no unreachable path, no configuration key nothing reads.

**What is deleted with it.** The Neo4j-CSV importer and staging package: **4,874 lines of package
source** (3,446 non-test + 1,428 test, verified by `wc -l` per file), plus 99 lines of Go fixture
source under its `testdata/` tree. **339 of those lines must move rather than die**: the fact-key
algebra lives inside that package, and it is the comparator the oracle in every phase above is built
on. The honest deletion credit is therefore **4,535 lines**, with 339 relocated to wherever the
native producer and the oracle share it. Everything else that exists only because the engine is an
opaque subprocess — the stderr failure classifier, the staging scratch pool, its exclusive lock, its
retirement rule, its paced reclaimer and the definition-cap skip reporting — is deleted too, and is
sized elsewhere in this directory rather than here.

---

## Part 3 — what the plan does not cover

One paragraph per `silent` row, and the two `partially meets` rows the audit found hardest.

### No caps, no time limits, no max-visited nodes, against a native engine (R18)

**The request-surface parameters are not analysis caps and must not be removed.** The classification
already adopted for scale bounds distinguishes lossless cursor pagination and pre-allocation wire
bounds, which stay, from scale refusal and work truncation, which do not. Against it:
`max_visited` and `max_edges` are documented in the traversal engine's own limit type as **per-page
work budgets, not cumulative walk ceilings** — a page that spends one stops with a continuation
cursor, the cumulative counts ride on the cursor and are reported, and the walk resumes. That is
lossless pagination. `max_depth` is the caller's own question scope: "callers within three hops" is a
different question from "all callers", not a truncation of it. `max_bytes` on the source-reading tool
is a response byte bound on the one tool that returns source. **Every one of them defaults to
unlimited** in the shipped configuration, and any reduction the engine applies is disclosed as a
notice on the answer rather than applied silently. A native engine changes none of this, and the plan
must say so, because the requirement is easy to mis-read as "delete the parameters", which would
delete resumability.

**Where an analysis cap would appear, it does not, and the reason is structural.** The pipeline's
unit of work is a function and its unit of caching is a file, so the working set is one file's syntax
tree plus one function's control-flow graph and bit vectors. There is nothing whose size is the
repository's for a cap to bound. That is why "no caps" is a property here rather than a policy, and
it is provable from the pipeline as drawn.

**What replaces the engine's definition bound: nothing, and the fixed point still terminates.** The
engine bounds the sum of generated definition facts per method and, when a method exceeds it, drops
**every** reaching-definition edge of that method — which is why the product publishes the family as
`partial` with the skipped method names. A native engine needs no such bound. The analysis is a
monotone forward may-analysis over a finite lattice: a definition is an integer control-flow node
index, so the domain is bounded by the method's own node count; the transfer function is
`gen(n) ∪ (x \ kill(n))`; meet is union; and the worklist re-queues a successor only when its `out`
set changed. The ascending chain condition gives termination in at most |nodes| × |definitions|
updates for **that method** — a bound the language and the function supply, not one the product
chooses. The honest residual is the part the engine's bound never protected either: building the
kill sets is quadratic in the method's call and syntax-node counts. That cost is bounded by the
largest **function** in the repository, never by the repository, and if a function ever makes it
visible the product reports the cost, never silently truncates the answer.

### 3–10× the reference repository (R15)

The reference repository is **13,222 files, 6,663 of them parsed, 555,588 call sites**. Three to ten
times that is **39,666–132,220 files, 19,989–66,630 parsed files and 1.67–5.56 million call sites**;
at the measured per-family shape the same multiple puts reaching-definition sites at **5.95–19.8
million** and control-dependence sites at **305,000–1,018,000**.

**Proven independent of repository size:** the analysis itself. Per worker the resident set is one
file's syntax tree plus one function's graph and bit vectors, so peak analysis memory is a function
of the largest function times the worker count, and worker count comes from the machine's cores.

**Not independent of repository size, and the plan names none of them:**

- **Fact output volume.** The reference repository's store already carries 1,983,374
  `data_flows_to` sites and 1,060,579 edges at 1×. At 10× that is roughly 19.8 million sites. A
  native engine does not reduce this; it emits the same facts without the CSV round trip.
- **The store.** Storage grows with facts, and identity width — not source volume — was already
  measured as the amplifier.
- **The differential key set.** The oracle sorts and merge-diffs one key per fact. At 10× that set is
  tens of millions of keys and must stay on disk, which the existing implementation already does.
- **The planner's unit list.** The retained plan's own unit list is already named as a
  repository-scaled structure in the standing memory inventory. It is the one retained structure that
  is neither paginated nor spooled, and a native engine does not remove it.

The plan must state each of these with its instrument, because "nothing grows with repository size"
is true of the analysis and false of the product, and the second reading is the one a reviewer will
test.

### Coexistence, and what replaces the half-the-host rule (R23)

The standing rule sizes the JVM's heap from the unit's bytes and caps the allocation the scheduler
sums reservations against at the smaller of (available memory − base footprint − margin) and **half
of available memory**; every heavy child — engine runs, external indexers, language servers — is
admitted against that one allocation, a child larger than the whole allocation runs alone rather than
being refused, and nothing counts children. With no JVM, **the allocation rule survives unchanged and
only the reservation changes**: instead of a per-source-byte heap estimate for an opaque child, a
native worker's reservation is computable — the largest function's graph and bit vectors plus one
file's syntax tree, times the worker count. That makes the reservation small, accurate and
*enforceable in process*, which is strictly better than the estimate it replaces.

Two consequences the plan must record. First, the reason the current design admits explicitly rather
than inheriting a runtime memory limit is that **a subprocess's resident memory is invisible to the
runtime's own limiter** — with the heavy work in-process that objection dies, so the runtime's soft
limit becomes usable as a second, in-process backstop beneath the admission rule, and the plan should
say whether it takes it. Second, the disclosure obligation does **not** die with the JVM: every unit
must still publish its reservation, its ceiling and its observed peak, because that disclosure is how
a host that is short of memory is read from the product's own report rather than from the kernel's.
The admission rule when the heavy child is in-process is therefore: reservations are per worker, the
worker pool is admitted against the one allocation exactly as an external child is, the pool shrinks
rather than the analysis being capped, and a single function whose reservation exceeds the whole
allocation runs alone — never subdivided, never skipped.

### The program dependence graph as a surface (R4)

The plan emits control dependence and data dependence as two independent fact families and never
composes them. The owner names the program dependence graph, which is that composition — and it is
the thing a slice is taken from. Today the composition is *expressible*: the traversal tools carry
both kinds in the default relation set, so a caller who knows to filter to `control_depends_on` and
`data_flows_to` and to pick a direction gets a dependence walk. That is exactly the tuning the
requirement forbids. What the plan must add is a first-class answer — a backward slice from a
location and a forward slice to one, composed over both families in one walk, paginated and
resumable like every other traversal — and a decision on whether it is a new tool or a mode of the
existing dependency-path tool. It costs no new analysis: the facts are already produced. It is a
surface decision the plan skipped because the plan is written as a port.

### Blazing-fast speed, proven (R16)

The plan prices effort in Go lines and weeks and never states a throughput target, an instrument or a
gate. The instruments exist: wall and tree-summed resident memory at 250 ms, and the engine-side
baselines are already measured — the dependence phase at 74% of a 31:38 index of the reference
repository, one unit at 64%, and that unit's parse at 3:38 with a 5.44 GB tree peak. The plan must
name a target against those baselines per phase, and Part 2 does so for phases 2 and 6. A phase that
cannot state a speed condition has not established the requirement it was built for.

### Tuning nothing over MCP (R14)

The plan names no tool and states no obligation about the request surface. The obligation is
inheritable and should be stated once: every bound resolves from configuration, every default is
unlimited, every reduction the engine applies is disclosed on the answer as a notice, and the native
engine adds **no** new request parameter, no timeout to tune and no mode to select. A native
`control_depends_on` that required a caller to choose a depth to be affordable would fail this
requirement even at parity.

### Helping agents find bugs and plan (R21)

The plan states no consumer-visible capability. The four fact families are inputs; what the agent
actually calls is `codectx_impact` (affected scope with completeness), `codectx_dependency_path`,
`codectx_callers`/`codectx_callees`, and the session family that budgets and seals what the agent
read. The plan must state, per phase, which of those answers changes and how — at minimum: the
precision stamp each tool reports, whether a previously deferred capability becomes immediate, and
whether any answer becomes *less* complete. Without that, a phase can pass a parity gate and still
degrade what an agent receives.

---

## Part 4 — contradictions inside the plan

Recorded with both line numbers. Fixing them is not this file's scope.

| # | the contradiction | lines |
|---|---|---|
| C1 | The control-flow package is given as 1,284 lines and the subtotal as 2,833. The package is **1,294** (12 files, measured against the clone at the pinned tag), so the subtotal is **2,843** | 98 vs 102, and the same 2,833 restated at 28 |
| C2 | Everything outside the 2,833 is classed as "dead weight or serves `calls` in dynamic languages", which puts the 5,413-line type-recovery system in the discard bucket — while the note's own §6 concludes the engine's call graph must be **kept** because nothing else resolves it. The machinery the conclusion depends on is classified as dead weight | 104-111 vs 310-311 |
| C3 | JavaScript is ordered first in phase 2 on five axes, one of which is that it is 94.75% of the reference repository's call sites where precise indexers and language servers both measure near-zero. But the design explicitly does not ask a native engine to produce `calls`, so that axis cannot be a reason to port JavaScript's **dependence** mapping first. The other four axes carry the decision on their own | 424-429 vs 323 |
| C4 | The effort table prices the scope and binding resolver as "0, or 400–900 per language" and leaves the branch open, while §4 says plainly that §6 decides which case applies — and §6 decides it: a perfect precise run over the reference repository reaches 4.87–5.25% of its call sites, so the zero branch is unreachable there. The row should carry the decided value | 393 vs 224-225 and 285 |
| C5 | The oracle is defined as a band measured from two engine runs before a third is judged, but phase 0's first consumer is Rust, whose oracle "cannot be a diff against the engine" and is a hand-built golden corpus. Phase 0 therefore has no end condition under the stated methodology. Part 2 resolves this by making equality correct for an authored corpus and only there | 368-372 vs 165-168 and 410-414 |
| C6 | The pipeline credits the deletion of the whole importer package, and the oracle section keeps that package's fact-key algebra as the comparator the native producer feeds. **339 lines of the 4,874 are counted as deleted and relied on at the same time.** The credit is 4,535 with 339 relocated | 337-338 and 395 vs 362-366 |
| C7 | The reaching-definitions package is cited under a path that places it beneath the command-line frontends directory; at the pinned tag it is a top-level module of the clone. The elision hides the difference, and a reader reconstructing the path from the note will not find it | 99 |

---

## Decisions this file made rather than referring upward

1. **The line credit for retirement is 4,874 minus the 339 relocated lines = 4,535**, with the 99
   lines of Go fixture source under the package's `testdata/` named separately. Reason: both
   measurements of the package are correct at their own scope, and publishing one number without its
   scope is how the earlier miscount happened.
2. **Equality, not a band, is the gate for an authored corpus.** The band is a property of the
   engine's non-determinism. Where no engine run is involved (phase 0), demanding a band would be
   importing a tolerance with no source.
3. **Every `calls` gate is stated on the in-repo-resolved subset.** Reproducing the 177,138
   external-stub sites reproduces no capability, and including them would let a phase pass by
   matching noise.
4. **The request-surface parameters stay.** They are pagination and wire bounds, not analysis caps,
   and removing them would remove resumability — which is the mechanism that makes an unbounded walk
   possible in the first place.

## Verification ledger

**Measured on this host.** The control-flow package (1,294 over 12 files, and each file itemized);
the reaching-definitions package (962 over 7 files, itemized); the three call linkers (226 / 41 / 30);
the attribution and stub passes (50 / 178 / 62); the importer package (4,874 by per-file `wc -l`,
4,973 by the aggregate method, the 99-line difference located in its fixture tree). Aggregates used
`find … -exec cat {} + | wc -l`; itemized rows used explicit `wc -l`. Nothing was built and the
engine was never run.

**Read at the pinned tag.** The six per-language generator drivers, for the post-processing override
(C, Go and Rust do not define it; JavaScript/TypeScript, Python and Java each do).

**Read in the product tree.** The MCP tool registry and its limit middleware; the graph engine's
limit type and its request-bound resolution; the configuration defaults for the graph bounds (all
unlimited); the relation-kind vocabulary and the default traversal set; the importer's fact-key
algebra.

**Quoted, not re-measured.** Every store figure (`14-store-counts-r3.md`, read with `sqlite3
-readonly` at the active generation); the file-locality and subdivision parity tables; the run-to-run
band; the memory cap sweep and the admission allocation rule. The dependence provider's own record is
cited **through** the raw files that quote its load-bearing sections verbatim.

**Not determinable here.** Whether a better formulation than the reference implementation's
bit-vector worklist should be adopted (R10) — that is an algorithm-selection question and is sized
elsewhere in this directory. The per-phase Go line cost of the call linkers and of type recovery —
this file establishes that they **must** be ported and measures their Scala anchors; it does not
estimate their Go size.

*Engine citations are pinned to the tag the product runs, in both forms, in the citation table above.
Every line count published here was run for this file.*
