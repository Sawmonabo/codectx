# Native-engine research, raw evidence — call-graph construction, parallelism and the whole-run memory model

Every question below gets **one decision**, the reason, the strongest competing option steel-manned,
and the trade-off accepted. Nothing is left open. Every claim carries a paper, a reference
implementation read at file:line, or a measurement recorded in this research.

## Versions actually present on this host

| thing | pinned elsewhere | present here | consequence |
|---|---|---|---|
| `golang.org/x/tools` | doc 20 §7.2 pins **v0.50.0**; `09-native-building-blocks.md` observed v0.50.0 @ 2026-09-08 over the network | highest in the local module cache is **v0.49.0** (also v0.23.0, v0.30.0, v0.45.0, v0.47.1-pre, v0.48.0) | **Discrepancy, flagged.** Every `x/tools` citation below is read at **v0.49.0**. Resolving v0.50.0 would be a download and was not done. |
| the engine clone | `v4.0.627` (`4bb889d96ce972e2ded50d0d5765c514c1a032cf`) | same, read-only | every engine citation is at that tag |
| `github.com/tree-sitter/go-tree-sitter` | `go.mod:13` | **v0.25.0**, cgo | load-bearing for the GC decision (§3.4) |
| host the arithmetic is evaluated on | — | **16 cores** (`nproc`), 47 GiB total / 38 GiB available (`free -g`) | §3.2 |

**Citation convention.** Engine rows give the research path and, beneath, the ADR-safe form.
Library rows give the module-cache-relative path with the version, never a home-directory prefix.

---

## 1. Call-graph construction, per language family

### 1.0 What the reference repository actually measures

From `14-store-counts-r3.md`, the numbers every decision below is judged against:

| quantity | value |
|---|---|
| tree-sitter `calls`, syntax precision | 555,588 sites / 363,750 edges |
| …unresolved, callee is a member access or a value | **459,791 sites (82.8%)** |
| …ambiguous, ≈8.96 candidates each | 20,046 sites (3.6%) / 179,626 `may_refer_to` sites |
| engine `calls`, `static_analysis`, → an in-repo definition | **40,743 sites (18.7%) / 27,897 edges (24.1%)** |
| perfect precise-index run over every plausible profile | **4.87–5.25%** of call sites |
| JavaScript's share of all call sites | **94.75%** (526,393 across 3,748 files) |

**The highest call resolution any technique actually run in this research reached on this repository
is 18.7% of call sites / 24.1% of edges**, by the engine's type recovery plus its two linkers. That
is the bar a native implementation is measured against, and the only measured bar there is.

### 1.1 What the engine's own linkers are — named, because doc 20 never names them

| linker | lines | what it is, in algorithm terms |
|---|---|---|
| `StaticCallLinker` | 41 | **None of the academic algorithms.** An exact-equality name join: for a `STATIC_DISPATCH` or `INLINED` call it looks up `cpg.method.fullNameExact(call.methodFullName)` and adds a CALL edge to every match, logging when more than one method carries the name. It is the same join tree-sitter's `calls` already performs, run over a `methodFullName` a frontend computed. |
| `DynamicCallLinker` | 226 | **Class-hierarchy analysis (CHA).** It builds `validM`: for every type declaration C and every method N declared on it, the set of implementations of N over **all subclasses of C**, found by name-and-signature lookup; a dynamic call then resolves to `validM(methodFullName)`, with an override walk up the superclasses first. It explicitly **ignores the vtable/BINDING mechanism**. Its own doc comment attributes the shape to SafeDispatch (Jang, Tatlock and Lerner, NDSS 2014), itself a CHA-based devirtualisation defence. |
| `XTypeRecovery` | 1,331 | **Not points-to, and not a fixed point.** Flow-insensitive, SSA-flavoured **symbol-table type propagation** over a **fixed iteration count — 2 by default** — whose own doc comment says it exists "to propagate types across compilation units, but avoid the poor scalability of a fixed-point algorithm" and that it "does not try to converge to some fixed point but rather iterates a fixed number of times". Its call-linking role is to set `methodFullName` on a call whose receiver it typed, so that the two linkers above have a name to join on. Its final iteration optionally invents **dummy types** for partially resolved receivers. |

Research-form citations:
`joern-cli/frontends/x2cpg/src/main/scala/io/joern/x2cpg/passes/callgraph/StaticCallLinker.scala:21-34`;
`joern-cli/frontends/x2cpg/src/main/scala/io/joern/x2cpg/passes/callgraph/DynamicCallLinker.scala:20-29,62-71,87-145,174-197`;
`joern-cli/frontends/x2cpg/src/main/scala/io/joern/x2cpg/passes/frontend/XTypeRecovery.scala:25,142-144,198-199,104`.

ADR-safe form (use these in the decision record):
`x2cpg/.../passes/callgraph/StaticCallLinker.scala:21-34`;
`x2cpg/.../passes/callgraph/DynamicCallLinker.scala:20-29, 62-71, 87-145, 174-197`;
`x2cpg/.../passes/frontend/XTypeRecovery.scala:25, 142-144, 198-199, 104`.

**Two consequences for the ADR.** First, the engine's 18.7% is produced by *CHA plus a name join
plus two rounds of symbol-table type propagation* — no points-to analysis, no whole-program
constraint solving, nothing a native implementation cannot reproduce. Second,
`XTypeRecovery.scala:211-213` records that local symbols are cleared at the compilation-unit
boundary "to keep memory usage down while maximizing concurrency": the engine reached the
per-file working-set discipline of §2 independently, from the same pressure.
ADR-safe: `x2cpg/.../passes/frontend/XTypeRecovery.scala:211-213`.

### 1.2 Statically typed with a hierarchy (Java, C#-like)

**Decision: class-hierarchy analysis (CHA).**

The three candidates, each from its own paper and from the `x/tools` package doc that implements it:

| algorithm | precision | cost and state |
|---|---|---|
| **CHA** (Dean, Grove, Chambers, ECOOP 1995) | "conservatively computes the entire *implements* relation between interfaces and concrete types ahead of time … may thus include spurious call edges for types that haven't been instantiated yet, or types that are never instantiated" — `x/tools v0.49.0, go/callgraph/cha/cha.go:12-18` | one pass over all functions joined against a hierarchy table; **sound on partial programs, such as libraries without a main or test function** (`cha.go:20-22`) |
| **RTA** (Bacon and Sweeney, OOPSLA 1996) | strictly more precise: the implements relation is built "on the fly as it encounters new functions reachable from main" (`cha.go:13-15`) | an **iterative fixed point** — "each time a newly added call edge causes a new function to become reachable, the code of that function is analyzed for more call sites, address-taken functions, and runtime types. The process continues until a fixed point is reached" (`x/tools v0.49.0, go/callgraph/rta/rta.go:34-37`). Its live state is whole-program: `Reachable`, `RuntimeTypes`, `addrTakenFuncsBySig`, `dynCallSites`, `invokeSites`, `concreteTypes` (`rta.go:53-81, 90-110`) |
| **VTA** (Sundaresan, Hendren, Razafimahefa, Vallée-Rai, Lam, Gagnon, Godin, OOPSLA 2000) | highest of the three: propagates the set of types and function literals each variable can hold (`x/tools v0.49.0, go/callgraph/vta/vta.go:15-17, 50-54`) | builds a **global type propagation graph** with a node per SSA local, per struct field, per global, per array/slice/map/channel element (`vta.go:19-37`), then propagates to a fixed point; `CallGraph` takes the whole program as `funcs map[*ssa.Function]bool` (`vta.go:79-86`) |

**Why CHA wins.** The binding constraint is not precision, it is **streamability**. The product's
whole-run memory posture (§3.2) admits exactly one shape for a repository-wide computation: an
ordered join over sealed facts through the external merge sort the tree already owns
(ADR-0001 §2.3), whose bound is run buffer + fan-in × block size. CHA's entire input is two
tables — (type, supertype) and (type, method name, signature, definition) — both of which a
**per-file** worker emits and both of which the link phase reads in sorted order. RTA and VTA
cannot be expressed that way: an iterative fixed point over a whole-program worklist has to hold
the worklist and the discovered sets in heap between iterations, which is precisely the term in
repository size the posture forbids.

Secondary reason, and the one that makes the differential oracle meaningful: the engine's
`DynamicCallLinker` **is** CHA (§1.1). A native port that computes CHA is diffing like against
like — doc 20 §7.3's band-bounded oracle measures normalisation differences, not an algorithm
change.

**RTA steel-manned.** RTA is genuinely more precise, it is production code in `x/tools`, and the
no-`main` objection is weaker than it looks: seeding the reachable set with every exported method
is standard practice and would let RTA run on a library. Its precision advantage over CHA is
exactly the set of types never instantiated anywhere reachable, which in an application repository
is a real set. **It still loses**, because seeding with every exported method of every file in a
monorepo makes the reachable set very nearly everything — the precision advantage shrinks toward
zero — while the fixed point and its whole-program state are paid in full.

**Trade-off accepted.** CHA over-approximates at megamorphic call sites, and `cha.go:49-53` names
the failure exactly: "every call to a highly polymorphic and frequently used abstract method such
as `(io.Writer).Write` is assumed to call every concrete `Write` method in the program". That
fan-out has to go somewhere. It goes into a family the product already has: `may_refer_to` with a
candidate count, which on the reference repository already carries 179,626 sites at ≈8.96
candidates each (`14-store-counts-r3.md` Q1). The rule a native implementation adopts: **a
single-candidate CHA result is a `calls` edge; a multi-candidate result is `may_refer_to` with the
count** — no new vocabulary, no new storage family.

### 1.3 Statically typed without deep hierarchies (Go, Rust, C/C++)

**Decision: direct binding for the direct majority, plus RTA's signature-keyed cross-product —
without RTA's reachability fixed point — for the indirect residue.**

Concretely, two tables and one join:

1. **Direct calls** resolve from the declaration binding the file-local scope already gives:
   callee name → the unique declaration in scope. This is not an algorithm, it is name binding,
   and it is where the overwhelming majority of Go/Rust/C call sites land.
2. **Indirect calls** (through a function value, a function pointer, a trait object, a closure)
   resolve against the structure `rta.go:93-96` names: *address-taken functions grouped by
   signature*, joined against *dynamic call sites grouped by signature* — "the algorithm uses
   dynamic programming to tabulate the cross-product of the set of known address-taken functions
   with the set of known dynamic calls of the same type" (`rta.go:14-19`). Each half is a per-file
   emission; the cross-product is a sort-merge join on the signature key.
3. **Interface/trait method calls** fall back to §1.2's CHA join over the (type, method) table —
   Go interfaces and Rust traits are structural hierarchies, shallow but real.

**Why this and not full RTA.** Dropping the reachability fixed point is what makes it a join, and
it costs almost nothing here: RTA's precision gain comes from *excluding functions not reachable
from main*, and these three families have no main in a repository index. What is kept — the
signature keying — is the part that actually discriminates, because a signature match is a hard
type constraint, not a name heuristic.

**Steel-manned alternative: run `x/tools` `go/callgraph/rta` or `vta` for real, on the Go family
only.** doc 20 §7.2 already contemplates this, the licence is BSD-3, and it would give a genuinely
sound Go call graph for free. **Rejected** for the Go family as a whole-repository step for one
concrete reason: `go/ssa` and RTA require a *loaded, type-checked program*, which means the Go
toolchain resolving the module graph — a network fetch at index time, which the global constraints
forbid ("no silent downloads at runtime"). It remains correct as an *opt-in* for a workspace whose
module graph is already vendored, and that is where the ADR should leave it.

**What this gains over today.** Go, Rust and C/C++ get **no type recovery at all** in the engine
today, so their `calls` is a pure name join — the `StaticCallLinker` shape of §1.1. The gain is
that an indirect call through a function value stops resolving to *nothing* and starts resolving
to a signature-bounded candidate set, and a method call on a concrete receiver resolves exactly
rather than to every same-named method.

**Trade-off accepted, and the honest gap:** the size of that gain is **unavailable** on the
reference repository, because `snapshot_files` has **no c, cpp, go or rust rows at all**
(`14-store-counts-r3.md` Q5 item 5). The measurement that would produce it: index a repository
with a substantial Go or C/C++ population and compare in-repo-resolved call sites before and
after. Until that is run, this decision is justified by the algorithm and by `rta.go:14-19`, not
by a number on this corpus.

### 1.4 Dynamic (JavaScript/TypeScript, Python)

**Decision: no whole-program points-to analysis, in any formulation. Resolve what a file-local,
field-based propagation can resolve; publish everything else as a candidate set with its count;
and treat deeper resolution as a demand-driven query answered at read time, never at index time.**

**Why.** This family is 94.75% of the reference repository's call sites and it is where the product
loses most, so the temptation to buy precision with a heavy analysis is strongest here. Both
whole-program formulations fail the same test:

- **Andersen** (Andersen, PhD thesis, DIKU, 1994) — inclusion-based, subset constraints, cubic in
  the worst case. Its constraint graph has a node per abstract location in the **whole program**.
  That is a term in repository size, held in heap for the duration. It is the single structure the
  memory posture (ADR-0001 §3.1, and §3.2 below) exists to forbid.
- **Steensgaard** (Steensgaard, *Points-to Analysis in Almost Linear Time*, POPL 1996) —
  unification-based, near-linear, and therefore the obvious escape. **It is worse here, not
  better.** Unification merges the points-to sets of both sides of every assignment into one
  equivalence class. In JavaScript, where values flow through object literals, `module.exports`,
  prototype assignment and callback parameters, the equivalence classes collapse: a query for the
  callee of `.map` would return every function that ever reached any merged class. Its precision
  floor is below the threshold at which an answer is useful to an agent, and the store would pay
  full price in edges to publish it.

Between the two, **Andersen has the right semantics and the wrong budget; Steensgaard has the right
budget and the wrong semantics.** Neither is adoptable whole-program, which is why the decision is
about *scope*, not about which of the two.

**Whole-program versus demand-driven.** Demand-driven refinement (Sridharan and Bodík,
*Refinement-Based Context-Sensitive Points-To Analysis for Java*, PLDI 2006; and Sridharan, Gopan,
Shan, Bodík, *Demand-Driven Points-To Analysis for Java*, OOPSLA 2005) answers one query by
exploring only the part of the heap the query needs, refining a field-insensitive approximation
until the query is answered or a budget is spent. That shape fits this product exactly, because the
product's read path is already a bounded, paginated, per-query walk (ADR-0005) — a demand-driven
points-to query is the same kind of object as an impact walk. **It does not belong in the index**,
because an index-time answer has to be computed for every call site whether or not anyone asks.

**What is actually achievable at index time**, and what it costs: field-based flow analysis, in the
shape Feldthaus, Schäfer, Sridharan, Dolby and Tip published for exactly this problem (*Efficient
Construction of Approximate Call Graphs for JavaScript IDE Services*, ICSE 2013, pp. 752-761,
DOI 10.1109/ICSE.2013.6606621) — one abstract location per *field name* rather than per allocation
site, which makes the constraint set proportional to the distinct field names in a file rather than
to the program's heap. That is the per-file-bounded formulation of Andersen's semantics, and it is
the only member of this family whose working set fits §3.2's bound.

**The honest ceiling, stated as a measurement and not as an impossibility.** The highest call
resolution any technique run in this research reaches on this repository is **18.7% of call sites /
24.1% of edges** (the engine: symbol-table type propagation + CHA + a name join). Perfect precise
indexing reaches **4.87–5.25%**. Both pinned language servers on `r3/app` return **2 references**
because no `tsconfig.json` exists. **82.8% of call sites have no name-bearing callee at all** — the
callee is a member access or a value — so there is nothing for a name-based technique to resolve,
and SCIP's occurrence roles are identical for `helper(counter)` and `f = helper`
(`08-scip-empirical-six-indexers.md`, conclusion 1). What this research **cannot** support is the
stronger claim that no technique exceeds 18.7%: no field-based or demand-driven analysis was run
over this repository. That figure is recorded as **unavailable** in §5, with the measurement that
would produce it.

**Trade-off accepted.** For this family the product publishes candidate sets and counts far more
often than it publishes edges, and it says so through `may_refer_to` and the candidate count rather
than by pretending to a resolution it does not have. The precision label stays `syntax` (doc 20
§7.4). An agent asking "who calls this" on a JavaScript monorepo gets a ranked candidate set, not a
single answer, and that is the truthful output.

---

## 2. Parallelism

### 2.1 The unit of work, the unit of caching, and the unit of scheduling

**Decision: doc 20 §7.1 is right that the unit of work and of memory is the function and the unit
of caching is the file. It leaves a third unit implicit, and this note names it: the unit of
*scheduling* is the file, dispatched longest-first onto a work-stealing pool.**

**Why the file and not the function.** A function cannot be analysed until its file has been parsed,
and the parse tree is the dominant per-worker allocation. Scheduling functions independently would
either make W workers hold W copies of the same file's tree, or force a shared tree with a
cross-worker lifetime and a reference count — a synchronisation cost paid on every function, for a
structure that is already per-file by construction.

**Why per-function work variance does not force a finer unit.** It is handled by the *order*, not
the granularity: dispatching files in descending size is longest-processing-time-first, whose
makespan is bounded at (4/3 − 1/(3m)) × optimal (Graham, *Bounds on Multiprocessing Timing
Anomalies*, SIAM J. Appl. Math. 17(2), 1969, DOI 10.1137/0117039). A work-stealing deque absorbs
the residue. Both are cheaper than reference-counting a shared tree.

**Steel-manned alternative: a batch sized by node count** — accumulate functions until their summed
CFG node count reaches a target, then dispatch the batch. It equalises worker load far better than
file size does, and it directly bounds the per-worker structure. **Rejected** because the node count
is not known until the file is parsed, so the batching decision needs the very work it is
scheduling; and because a batch spanning two files reintroduces the shared-tree lifetime problem
the file unit exists to avoid.

**Trade-off accepted.** One pathological file — a single generated or minified source larger than a
worker's reservation — is one worker's problem for as long as it takes, and the other W−1 workers
carry the rest. That is the correct failure shape (it degrades one worker, not the run), and §3.3
names it as the thing that breaks first.

### 2.2 Where the analysis runs — a finding that corrects doc 20 §7.1

**Decision: the native analysis runs inside the existing structural parser worker subprocess, not in
the coordinating process.**

Doc 20 §7.1 writes the pipeline as "per file … tree-sitter CST — **already built by the structural
tier**". Read against the tree, that is not free: `internal/provider/treesitter/provider.go:5`
records that "Parsing runs in isolated worker subprocesses", `worker/worker.go:2` that the
subprocess "owns every native object (parser, tree, query, query cursor)", and
`provider.go:343-352` that a parse is one request onto a pooled worker. **The parse tree never
leaves the subprocess**; what crosses the wire is length-prefixed JSON frames of facts
(`wire/wire.go:2`). An in-process native engine would therefore have to parse every file a second
time, and `C_file` would be a genuinely new per-worker term.

Putting the analysis inside the worker removes that term and inherits four properties already
decided and already measured:

- the CST, for free, in the process that already owns it;
- `MaxWorkers` = `config.ParserWorkers()` = one per CPU (`provider.go:59`);
- `WorkerMemoryBytes`, the reservation each worker is admitted against, defaulting to
  `defaultWorkerMemory = 256 << 20` = **256 MiB** (`provider.go:84-86, 97, 157-158`);
- crash isolation: a panic on a pathological function kills one worker, which the pool replaces and
  retries exactly once (`provider.go:343-345`) — which is the native equivalent doc 20 promised for
  the engine's six reverse-engineered failure classes (`04-requirements-and-engine-cost.md`).

**Steel-manned alternative: run it in-process and re-parse.** It is simpler to build, it removes the
wire format from the hot path, and cgo tree-sitter is directly callable. **Rejected** on the double
parse — the structural phase dominates a cold index, and paying it twice is a measurable regression
for an architectural convenience.

**Trade-off accepted.** The fact stream that crosses the worker boundary grows by the dependence
families, so the wire format and the worker's own emission batching become load-bearing for a much
larger volume than they carry today.

### 2.3 Worker count

**Decision: `W = config.ParserWorkers() = config.CPUs() = runtime.NumCPU()`, and it is never a
setting.**

This is reuse, not invention: `internal/config/machine.go:19-27` already states the rule in the
product's own words — "How much CPU-bound work runs at once is a property of the machine, not a
number anyone types. Memory-bound work is admitted against the machine's one memory allocation; the
counts here are the other half of that rule, for work whose scarce resource is a core rather than a
byte." `machine.go:29-37` notes that `runtime.NumCPU` already honours CPU affinity, so a container
given two cores of a large host reads two. ADR-0010 decision 5 forbids a count or a setting for
heavy children on the memory side; this is the same ruling on the CPU side.

**Why it must not be a knob.** The owner's constraint is that an agent "simply uses the MCP to get
what it needs immediately without wasting time on figuring out how to tune timeouts etc." A worker
count exposed as a setting has no value the caller can compute — it depends on the host's cores,
its current load and the file mix — so it becomes a number copied from a README, wrong on every
machine but the one it was written on, and it converts a scheduling decision into a support
surface. ADR-0010's third rejected alternative ("a user setting for the host share") is the same
rejection for the same reason.

**Steel-manned alternative: `NumCPU − 1`, reserving a core for the user's editor.** **Rejected**,
and `machine.go:43-45` already carries the reason: "The kernel is left to share the cores between
them and the rest of the machine rather than a core being held back for it: a reserved core is idle
in the common case, where nothing else wants it." A reserved core is a permanent tax to buy a
transient benefit the scheduler already provides.

**Trade-off accepted.** Under heavy competing CPU load the product takes its fair kernel share and
the editor is slower than it would be with a reservation. Memory, not CPU, is what actually freezes
a host (`internal/paced/reclaim.go:26-34`), and that is bounded by §2.4.

### 2.4 Coexistence: what replaces ADR-0010's admission allocation when there is no JVM child

**Decision: the same allocation, the same admission gate, a different reservation. Each analysis
worker takes `WorkerMemoryBytes` from `Machine.SchedulingAllocation()` before it starts and returns
it when it stops; the allocation is re-derived from the kernel's `MemAvailable` between files; a
worker that does not fit **waits**, head-of-line, and is promoted when a running worker returns its
reservation. Nothing is refused, nothing is capped, and there is no knob.**

The mechanism, named rather than intended, and every part of it already in the tree:

| part | mechanism | where it already exists |
|---|---|---|
| the allocation | `min(available − base footprint − safety margin, available / 2)` — the host keeps at least half of what it had | `internal/provider/dependence/govern.go:175-184` |
| the fallback | `SchedulingAllocation()` returns `UnobservedAllocationBytes` where the platform hides available memory, because "an admission gate with no bound is not a gate" | `govern.go:186-196`; ADR-0010 decision 5 |
| the observation | the kernel's `MemAvailable`, "the kernel's own estimate of what can be taken" | `internal/provider/dependence/machine_linux.go:18-42` |
| the base footprint | `config.BaseFootprintBytes` = 1 GiB, one figure for the whole product, deliberately over-stated | `internal/config/machine.go:5-17` |
| the reservation | `WorkerMemoryBytes`, default 256 MiB per worker | `internal/provider/treesitter/provider.go:84-86, 97` |
| the wait | head-of-line admission on concurrency and bytes together under one lock; promotion stops at the first waiter that does not fit | ADR-0001 §2.6 |
| the reporting | every unit discloses its reservation, its ceiling and its observed peak in the resources block and in `codectx status` | ADR-0010 decision 4 |

**How the product knows it is competing with the user's editor.** It re-reads `MemAvailable` between
files and re-derives the allocation from it. That is the only signal that sees processes this
product did not start — `runtime.MemStats` sees the Go heap of one process, and a cgroup limit sees
a container, not a laptop. When the editor grows, `MemAvailable` falls, the allocation falls with
it, the sum of live reservations stops fitting, and the next worker waits instead of starting. The
product gives ground automatically and continuously, at the granularity of one file.

**Steel-manned alternative: no in-process gate at all — a Go program with a bounded live set does
not need admission, and ADR-0010's gate existed only because a JVM child's memory was invisible.**
It is a real argument: the per-worker footprint is a constant (§3.2), so `W × M_worker` is knowable
in advance and could simply be checked once at start. **Rejected** on two counts. The workers are
still subprocesses (§2.2), so their resident memory is invisible to the parent exactly as the JVM's
was — ADR-0001 [S19] is the same finding in the same place. And a check at start is not
coexistence: the editor the product must yield to is started *after* the index begins as often as
before it, and only a re-derived allocation sees that.

**Trade-off accepted.** Head-of-line admission can leave headroom idle while a large reservation
waits (ADR-0001 §2.6 states this at the function). On a memory-starved host the index runs at fewer
than `NumCPU` workers and takes longer. Slower is the correct degradation; the alternative is the
host freezing, which the owner's ruling makes a product defect.

### 2.5 What must not be introduced, and how the posture survives without it

No cap on analysis, no time limit, no max-visited bound, no user knob. The design honours
ADR-0001's "unlimited by default, bounded by page" with four mechanisms, all of which already
exist and none of which is new:

- **Admission defers, never refuses** (class C, ADR-0001 §1.2): a worker that does not fit waits.
- **Progress, not wall clock**: a wedged worker is caught by the stall detector, which "measures
  progress, not elapsed time: a subprocess that is producing bytes or consuming CPU is never
  touched, however long it runs" (ADR-0001 §2.1). A large function is slow, not stalled.
- **Bounds shape buffers, never work** (ADR-0001 §2.6): the worker's emission batch bounds a buffer;
  the file stream behind it is unbounded.
- **The page bounds the answer, not the walk** (class A): every read surface is a lossless cursor.

The one bound this design *does* introduce is `WorkerMemoryBytes`, and it is class C, not class D:
it is a **reservation** the scheduler sums against, and ADR-0001 §2.6 explicitly preserves
reservations ("a zero reservation is a broken deployment, not an unlimited one").

---

## 3. Memory at repository scale

### 3.1 Compressed sparse row: where it applies, where it does not, and the handoff

**Decision: CSR is the published per-generation graph's layout and nothing else. The in-flight
per-function structure is never CSR. What crosses from a worker to the store is an ordered stream
of facts, one function at a time, and no graph is ever materialised in a worker.**

**Where CSR applies.** The packed per-generation adjacency of ADR-0005 Decision 1: built at
generation activation from facts already sealed, in both directions, as chunked offset directories
and varint-delta edge streams, by two ordered index scans in which "heap stays O(part), never
O(nodes)". Sized in `17-graph-traversal-throughput.md` §3.1 at ≈5.35 MiB forward and ≈10.7 MiB both
directions for 477,602 nodes / 739,529 edges — ≈0.8% of that 1.36 GiB store.

**Where it does not.** A per-function CFG has tens to hundreds of nodes and fits in cache. Building
a CSR over it costs a degree histogram and a prefix sum — two passes to save an indirection on a
structure small enough that the indirection is free. Sub-lane D owns the per-function
representation; this note asserts only the boundary.

**The handoff, stated precisely:**

| | |
|---|---|
| **what crosses** | fact records only: node facts, relation facts and evidence rows, in the provider's existing batch form |
| **in what order** | per function, in emission order, functions in file order, files in the order the scheduler dispatched them; the store's sink is append-only with no secondary index during load, and every ordering is built afterwards (`04-requirements-and-engine-cost.md`, the staging-database section) |
| **what is never materialised** | a whole-file graph in a worker; a whole-repository graph anywhere; a repository-wide identity dictionary — interning is "a bounded cache flushed with each batch, never a whole-repository dictionary in heap" (ADR-0001 §2.8) |
| **when CSR is built** | at generation activation, after sealing, from the sealed facts — never by a worker |

### 3.2 The bound that makes "no caps" a property

**Decision. The whole run's resident memory is**

```
R_run  =  B_process  +  W × M_worker  +  A_link
```

| term | what it is | value, with its source |
|---|---|---|
| `B_process` | the coordinating process: one emission batch per sink, the interning cache flushed per batch, the plan's iterator. Declared for admission arithmetic as `config.BaseFootprintBytes`. | **1 GiB** declared (`internal/config/machine.go:17`). Measured whole-run process-tree peak of the existing pipeline at reference scale: **145.3 MiB** (ADR-0001 §3.3, on the *corrected reference corpus*: 10,000 files, 1,052,933 lines, 86,064,203 B — **not** the 13,222-file reference repository) |
| `W` | workers, one per core | `config.CPUs()` = `runtime.NumCPU()`. **16** on this host (`nproc`) |
| `M_worker` | one worker's reservation: its parse tree plus one function's CFG, dominator trees and dataflow bitsets | **256 MiB** (`defaultWorkerMemory = 256 << 20`, `internal/provider/treesitter/provider.go:97`) |
| `A_link` | the call-graph link phase (§1): an ordered join over sealed facts through the one external merge sort | run buffer + fan-in × block size (ADR-0001 §2.3; the published contract, `maxPartitionsInRAM × ramBufferSize`). Measured flat at **522,024 B at 30,000 inputs and 524,088 B at 300,000 inputs — +0.4% for 10× the input** (ADR-0001 §2.3) |

**The arithmetic, on this host (16 cores, 47 GiB total, 38 GiB available):**

```
W × M_worker  = 16 × 268,435,456 B  = 4,294,967,296 B  = 4.00 GiB
B_process     =      1,073,741,824 B                   = 1.00 GiB   (declared)
A_link        ≈            524,088 B                   ≈ 0.0005 GiB
R_run         ≈      5,369,233,208 B                   ≈ 5.00 GiB
```

Against the admission allocation on this host:
`min(38 − 1 − 1, 38 / 2) = min(36, 19) = 19 GiB`. **5.00 GiB of a 19 GiB allocation**: all 16
workers are admitted at once and the host keeps 33 of the 38 GiB it had.

**There is no term in repository size.** `W` is the host's cores. `M_worker` is a constant of the
build. `A_link` is a configured sort budget, measured flat across a 10× input change. `B_process`
is one batch per sink. Not one of them names the file count, the call-site count or the repository
byte count, and that — not a policy statement — is what makes "no caps, no subdivision" a property.

**Every term that could scale, named, with what bounds it:**

| candidate term | does it scale with the repository? | what bounds it |
|---|---|---|
| a worker's parse tree | **No — it scales with the largest single source file.** | `M_worker` = 256 MiB — an **admission estimate, not an enforced ceiling** (`internal/process/runner.go:342-357`; no rlimit or cgroup in `internal/process/`). Reference repository mean: 157.2 MB over 4,984 files = **31.5 KB/file**, so the reservation covers ≈8,500× the mean file (ADR-0010; `14-store-counts-r3.md`). What happens when it does not: §3.3 item 2 |
| a worker's per-function structures | **No — they scale with the largest single function.** Reaching-definition bitsets are \|N\| × \|D\| bits | Arithmetic: a 1,000-node / 1,000-definition function = 1,000,000 bits = **125 KB**; a pathological 10,000 × 10,000 function = **12.5 MB**. Both inside `M_worker`. Sub-lane D owns the exact constant |
| the CHA hierarchy + method tables (§1) | **Yes, in bytes moved — but on disk, not in heap** | the external merge sort's `A_link` bound; the phase pays repository size in wall clock and temporary disk, never in resident memory |
| the per-generation CSR | **Yes — and it is built outside any worker**, streamed, heap O(part) | ADR-0005 Decision 1; ≈10.7 MiB at reference scale, ≈107 MiB at 10× (`17-graph-traversal-throughput.md` §3.1) |
| the walk's visited bitset | **Yes — 1 bit per node, on a sparse file, resident = touched pages** | ADR-0005 Decision 2; 11.9 MiB on disk at 10⁸ nodes (`17` §4) |
| the store on disk | **Yes, linearly** | nothing in this note; it is ADR-0001 §2.8's open storage gate. See §3.3 |

### 3.3 Three and ten times the reference repository

The reference repository (`14-store-counts-r3.md`, ADR-0010): 13,222 files, 6,663 parsed, 555,588
call sites, 363,750 syntax call edges; largest unit 4,984 files / 157.2 MB of JavaScript.

| quantity | 1× | 3× | 10× |
|---|---|---|---|
| files | 13,222 | 39,666 | 132,220 |
| parsed files | 6,663 | 19,989 | 66,630 |
| call sites | 555,588 | 1,666,764 | 5,555,880 |
| syntax call edges | 363,750 | 1,091,250 | 3,637,500 |
| largest unit | 4,984 files / 157.2 MB | 14,952 files / 471.6 MB | 49,840 files / 1,572.0 MB |
| **`R_run` from §3.2** | **5.00 GiB** | **5.00 GiB** | **5.00 GiB** |
| per-generation CSR, both directions | ≈10.7 MiB | ≈32 MiB | ≈107 MiB |
| store on disk, scaling the measured 1,464,328,192 B reference store linearly | **1.36 GiB** | **4.09 GiB** | **13.64 GiB** |

`R_run` does not move, because the formula contains no term that these columns change. The
per-worker reservation is unchanged: growing a repository adds files, it does not enlarge the
largest one.

**What breaks first, and at what size.** Not memory. In order:

1. **Disk, at roughly 5×.** The store is measured at **16.10× the eligible source bytes** against a
   3.5× budget, and at **3,603.5 bytes per indexed symbol** after the surrogate redesign
   (ADR-0001 §1.4, §2.8). Scaling the 1.36 GiB reference store linearly puts 10× at **≈13.6 GB**,
   which is not an out-of-memory failure but is a laptop's free space. This is ADR-0001 §2.8's own
   open item and is the storage wave's to close; it is named here because it is the first thing a
   native engine hits, not because a native engine causes it.
2. **A single source file whose parse tree overruns `M_worker` = 256 MiB — and it does not fail.**
   `M_worker` is an *admission estimate*, not a ceiling: `internal/process/runner.go:342-357, 391`
   admits a run while `memoryUsed + reservation` fits the runner's budget and refuses only a
   reservation larger than the whole budget; there is no `Setrlimit`, no cgroup and no RSS cap
   anywhere in `internal/process/`. So a file that overruns its reservation runs to completion while
   `W × M_worker` silently **under-states** actual resident memory — the coexistence guarantee of
   §2.4 becoming false rather than a contained crash. This is why ADR-0010 decision 4's observed-peak
   disclosure is load-bearing and not decorative (§3.4): it is the only mechanism that makes the
   drift visible. The largest single file in the reference repository is **unavailable** (§5), so the
   size at which this begins is unknown.
3. **Wall clock on the link phase (§1).** It is one external merge over the whole repository's
   hierarchy and method tables. Its memory is flat; its time is `sort(N)`. At 10× it is ten times
   as long, which is a scheduling fact, not a failure.

### 3.4 Arena allocation, `GOGC` and `GOMEMLIMIT`

**Decision: per-function structures are allocated from a per-worker arena that is *reset*, not
freed, between functions. The product sets neither `GOGC` nor `GOMEMLIMIT`.**

**The arena, and what it buys in Go specifically.** Every per-function structure — CFG nodes,
dominator and post-dominator arrays, the reaching-definition bitsets — is a slice out of one
per-worker backing buffer, reset to length zero when the function is done. In Go this buys three
things that a general-purpose allocator does not: the collector's scan work is proportional to
*pointers*, so pointer-free `[]uint64` bitsets carved out of one buffer are scanned as one object
rather than thousands; the buffer reaches a high-water mark within the first few functions and then
allocates nothing at all, so the steady-state allocation rate a worker presents to the collector is
approximately zero; and the arena's high-water mark **is** the per-function term of §3.2's
`M_worker`, directly measurable rather than inferred from heap profiles.

**Why no `GOMEMLIMIT`.** The dominant per-worker allocation is the tree-sitter parse tree, and
tree-sitter is **cgo** (`go.mod:13`, `github.com/tree-sitter/go-tree-sitter v0.25.0`;
`09-native-building-blocks.md` records it as "Go + cgo"). Memory allocated by C is not memory the Go
runtime manages, so a soft memory limit would bound the smaller half of a worker's footprint while
the half that actually grows stayed invisible to it. This is the in-process form of a finding the
product already holds: ADR-0001 [S19] records that "a *subprocess's* resident memory is entirely
invisible to it, so a managed analyzer's whole allocation must be deducted from the parent budget",
and ADR-0001 [S18] records the runtime's own guidance that the soft limit "thrashes when set too
low" and wants 5–10% headroom. Setting it would buy a backstop against the wrong half of the
footprint and risk a collection death-spiral in exchange.

**Why no `GOGC`.** With the arena, a worker's steady-state allocation rate is near zero, so `GOGC`
has almost nothing to trade. Lowering it would spend CPU to reclaim memory the arena is
deliberately holding — which is the arena's whole point — and raising it would loosen a bound that
is already set by admission.

**Steel-manned alternative: set `GOMEMLIMIT` to the admission allocation as a pure backstop.** It is
the one mechanism that turns an unexpected allocation spike into a slowdown rather than an OOM kill,
and on this host the limit (19 GiB) would sit far above the live set (5 GiB), so the thrash regime
the runtime guide warns about is not reached. **It still loses**: the spike it would catch is a
pathological parse tree, which is exactly the allocation it cannot see. It would be a safety net
strung under the wrong half of the floor, and the product would carry the belief that it was
protected.

**Trade-off accepted.** There is no runtime-level backstop. The whole guarantee rests on admission
(§2.4) and on `M_worker` being a true bound, which makes worker-peak disclosure (ADR-0010 decision
4) load-bearing rather than decorative: it is the only way a drift in `M_worker` becomes visible.

### 3.5 Disk and the store

**Decision: the peak disk footprint of an analysis run becomes the store's own growth plus one
emission batch per sink. There is no staging surface at all, and every byte the run later gives back
goes through the paced reclaimer.**

**What disappears.** The engine hands the product **0.65–4.95 GB of Neo4j CSV** (2.02 GB for
postgres, 4.95 GB for a 1.05M-line Python tree), and the importer stages the whole export in a
private SQLite database at **6.6× the export**, with a 256 MiB cache, a pooled surface, an exclusive
lock and a retirement rule (`04-requirements-and-engine-cost.md`; ADR-0009). Peak staging disk for
the largest of those is therefore ≈32.7 GB (4.95 × 6.6). A native engine emits facts in projection
order, in process, one function at a time, so **the CSV, the staging database, its cache, its pool,
its lock and its retirement rule have no equivalent**, and with them goes the 4,874-line importer
package less the 339-line fact-key comparator that relocates to the oracle — a net **4,535** lines
(`15-requirements-audit.md`).

**What the peaks become.**

| | engine today | native |
|---|---|---|
| peak disk above the store | export + 6.6 × export | **one emission batch per sink** |
| peak resident | per-unit heap cap from unit source bytes; JS/TS 48 B/source byte with the 157.2 MB unit measured at 4.57–14.32 GB tree RSS across the cap sweep (ADR-0010) | **`W × M_worker` = 4.00 GiB, constant** (§3.2) |

**Why the paced reclaimer still matters — and matters *more* at 10×.** This is the one place where
the native design's advantage shrinks rather than grows. Resident memory is flat across §3.3's
columns; **the bytes the product frees are not.** A retired generation at 10× is ≈13.6 GB of store
and CSR. `internal/paced/reclaim.go:26-34` records the measurement: "On a machine whose root
filesystem discards freed blocks and whose disk is a sparse image on its host, freeing 5 GB in one
burst left the host owing work it never reported: every disk request in flight about a minute later
waited 64 seconds, and nothing inside the machine could observe or wait for it. The same 5 GB freed
a window at a time with a data sync and this wait between windows — 152 seconds, the device counting
21 GB of discards at a peak of 464 MB in one second — caused no wait longer than 32 milliseconds in
the four minutes after. The host tolerates any amount freed at a pace and hangs on a burst."

**What would break without it:** retiring one 10×-scale generation would free ≈13.6 GB in a burst
and stall every process on the machine for about a minute, roughly a minute *after* the product
reported the run finished — unobservably, and attributed to anything but this product. That is the
owner's coexistence constraint failing in its purest form. The rate is a constant of the package
(8 MiB per window, a data sync, `FreeInterval = 250 * time.Millisecond`) and not a setting, because
"the rate is a property of what the disk underneath does with a discard, and an operator asked to
tune it would have nothing to tune it against" (`reclaim.go:36-39`). **Every free a native engine
performs — retired generations, superseded CSR blobs, abandoned emission batches — goes through
it.** Deleting the staging database removes a *source* of freed gigabytes; it does not remove the
need for the reclaimer, and at 10× the remaining sources are larger than the staging database ever
was.

---

## 4. Traversal throughput

**Decision: a native engine changes nothing about the read path. Not the layout, not the visited
set, not the ranking. No part of ADR-0005 is reopened.**

The reason is one sentence of ADR-0005 Decision 1: the packed per-generation adjacency is built
"from facts already sealed", and "because the blob is derived from facts already sealed, it is
neither a fact nor an identity: no provider version, no analysis fingerprint." A native engine
changes **which producer sealed the facts**. It does not change that they are sealed, what they
mean, their surrogate identities (ADR-0002), the two ordered scans that build the CSR, the paged
bitset visited set, the collect/commit/serve pipeline, or the external merge that ranks the answer.
`codectx_callers`, `codectx_callees`, `codectx_dependency_path` and `codectx_impact` read exactly
what they read today.

Two quantities move, and neither is a design change:

- **Edge volume.** More resolved `calls` edges and more `may_refer_to` candidate edges (§1.2) mean a
  larger CSR. It scales as `17-graph-traversal-throughput.md` §3.1 already sizes it: ≈0.8% of the
  store, ≈107 MiB at 10× the reference graph, still mmap-able.
- **Average out-degree.** ADR-0005 rejected direction-optimising BFS on the measured degree data
  (average out-degree 5.91, near-acyclic call graph, `17` §2.1). A CHA-derived graph with
  megamorphic fan-out raises that average. **The rejection stands**: the bottom-up switch fires on a
  *low-diameter, scale-free* graph, and CHA fan-out widens hubs without shortening chains. If a
  future measurement shows the frontier regularly reaching a large fraction of the node set, `17`
  §2.1 is the note to re-read — but nothing here justifies re-opening it now.

The policy requires reusing an existing solution over inventing one. This is that case, stated
plainly: **nothing changes, and the reason is that the read path was designed against sealed facts
rather than against a producer.**

---

## 5. Figures recorded as unavailable

| figure | why it is not obtainable here | the measurement that would produce it |
|---|---|---|
| The largest single source file in the reference repository | needs a query against that repository's stores, which this note did not run | `SELECT max(size_bytes) FROM snapshot_files` on the active snapshot, per language |
| The true achievable call-resolution ceiling for the JavaScript/Python family | no field-based or demand-driven points-to analysis was run over this repository; 18.7% is the highest figure any technique *actually run* reached | run an approximate field-based call-graph construction (ICSE 2013 shape) over the repository and count call sites resolved to an in-repo definition |
| The gain from §1.3's algorithm for Go, Rust and C/C++ | `snapshot_files` has **no c, cpp, go or rust rows at all** — the reference repository carries zero weight for this family | index a repository with a substantial Go or C/C++ population; compare in-repo-resolved call sites before and after |
| `M_worker`'s true high-water mark under the dependence families | requires running the native engine, which does not exist | instrument the arena's high-water mark per worker and sample it across a real index |
| Whether `x/tools` v0.50.0 differs from v0.49.0 in `cha`/`rta`/`vta` | v0.50.0 is not in the local module cache and resolving it is a download | read v0.50.0's `go/callgraph/` once it is present by some already-permitted route |

**All memory and count figures above are either read from a cited constant in the tree, quoted from
a measurement already recorded in this research with its corpus named, or computed by the arithmetic
shown. No figure in this note is an estimate presented as a measurement.**

---

## Sources

| # | author / origin | title | venue | year | URL or path where verified | used for |
|---|---|---|---|---|---|---|
| 1 | Dean, J., Grove, D., Chambers, C. | Optimization of Object-Oriented Programs Using Static Class Hierarchy Analysis | ECOOP | 1995 | cited in the implementation read at `x/tools v0.49.0, go/callgraph/cha/cha.go:8-10` | §1.2, the CHA decision |
| 2 | Bacon, D. F., Sweeney, P. F. | Fast Static Analysis of C++ Virtual Function Calls | OOPSLA | 1996 | `http://doi.acm.org/10.1145/236337.236371`, cited at `x/tools v0.49.0, go/callgraph/rta/rta.go:10-12` | §1.2 RTA steel-man; §1.3 signature cross-product |
| 3 | Sundaresan, V., Hendren, L., Razafimahefa, C., Vallée-Rai, R., Lam, P., Gagnon, E., Godin, C. | Practical Virtual Method Call Resolution for Java | OOPSLA | 2000 | `https://dl.acm.org/doi/10.1145/353171.353189` (venue and year verified over the network; `x/tools v0.49.0, go/callgraph/vta/vta.go:6-9` names authors and title but no venue) | §1.2, the VTA row |
| 4 | Jang, D., Tatlock, Z., Lerner, S. | SAFEDISPATCH: Securing C++ Virtual Calls from Memory Corruption Attacks | NDSS | 2014 | DOI 10.14722/ndss.2014.23287, cited in the engine's own doc comment at `x2cpg/.../passes/callgraph/DynamicCallLinker.scala:27-28` | §1.1, naming the engine's dynamic linker as CHA |
| 5 | Andersen, L. O. | Program Analysis and Specialization for the C Programming Language | PhD thesis, DIKU, University of Copenhagen | 1994 | not verified over the network in this note; recorded as the standard reference for inclusion-based points-to | §1.4, the inclusion-based option |
| 6 | Steensgaard, B. | Points-to Analysis in Almost Linear Time | POPL | 1996 | DOI 10.1145/237721.237727; not re-verified over the network in this note | §1.4, the unification-based option, rejected |
| 7 | Sridharan, M., Bodík, R. | Refinement-Based Context-Sensitive Points-To Analysis for Java | PLDI | 2006 | DOI 10.1145/1133981.1133978; not re-verified over the network in this note | §1.4, demand-driven at read time |
| 8 | Sridharan, M., Gopan, D., Shan, L., Bodík, R. | Demand-Driven Points-To Analysis for Java | OOPSLA | 2005 | DOI 10.1145/1094811.1094817; not re-verified over the network in this note | §1.4, same |
| 9 | Feldthaus, A., Schäfer, M., Sridharan, M., Dolby, J., Tip, F. | Efficient Construction of Approximate Call Graphs for JavaScript IDE Services | ICSE, pp. 752-761 | 2013 | `https://dl.acm.org/doi/10.1109/ICSE.2013.6606621` (authors, venue, year, pages and DOI verified over the network) | §1.4, the field-based shape chosen for index time |
| 10 | Graham, R. L. | Bounds on Multiprocessing Timing Anomalies | SIAM Journal on Applied Mathematics 17(2) | 1969 | DOI 10.1137/0117039; not re-verified over the network in this note | §2.1, longest-first scheduling |
| 11 | reference implementation | class-hierarchy analysis, production Go | — | v0.49.0 | `x/tools v0.49.0, go/callgraph/cha/cha.go:5-23, 36-75` | §1.2 precision and cost |
| 12 | reference implementation | rapid type analysis, production Go | — | v0.49.0 | `x/tools v0.49.0, go/callgraph/rta/rta.go:5-38, 51-81, 90-110` | §1.2, §1.3 |
| 13 | reference implementation | variable-type analysis, production Go | — | v0.49.0 | `x/tools v0.49.0, go/callgraph/vta/vta.go:5-55, 79-86` | §1.2, rejected on state shape |
| 14 | the engine at `v4.0.627` | static call linker | — | 2026-09-11 | `x2cpg/.../passes/callgraph/StaticCallLinker.scala:21-34` | §1.1, "none of the academic algorithms" |
| 15 | the engine at `v4.0.627` | dynamic call linker | — | 2026-09-11 | `x2cpg/.../passes/callgraph/DynamicCallLinker.scala:20-29, 62-71, 87-145, 174-197` | §1.1, identified as CHA |
| 16 | the engine at `v4.0.627` | type recovery | — | 2026-09-11 | `x2cpg/.../passes/frontend/XTypeRecovery.scala:25, 104, 142-144, 198-199, 211-213` | §1.1, fixed-iteration symbol-table propagation |
| 17 | this repository | store counts on the reference repository's stores | research note 14 | 2026-09-16 | `docs/research/raw/native-engine/14-store-counts-r3.md` | every call-resolution figure in §1 and §3.3 |
| 18 | this repository | the product requirements a native engine must meet, and what the engine costs | research note 04 | 2026-09-16 | `docs/research/raw/native-engine/04-requirements-and-engine-cost.md` | §3.1, §3.5 |
| 19 | this repository | native building blocks, fetched for licence and activity | research note 09 | 2026-09-16 | `docs/research/raw/native-engine/09-native-building-blocks.md` | version table; cgo tree-sitter in §3.4 |
| 20 | this repository | graph traversal throughput | research note 17 §2.1, §3.1, §4 | 2026-09-15 | `docs/research/17-graph-traversal-throughput.md` | §3.1, §3.2, §4 |
| 21 | this repository | scale posture: unlimited by default, bounded by page | ADR-0001 §1.4, §2.1, §2.3, §2.6, §2.8, §3.1, §3.3 | 2026-09-14 | `docs/adr/ADR-0001-scale-posture.md` | §2.4, §2.5, §3.2, §3.3 |
| 22 | this repository | graph traversal layout | ADR-0005 Decisions 1, 2, 3 | 2026-09-15 | `docs/adr/ADR-0005-graph-traversal-layout.md` | §3.1, §4 |
| 23 | this repository | the engine's heap is sized to the unit's need, and the host keeps half of what it had | ADR-0010 Decisions 2, 4, 5 | 2026-09-16 | `docs/adr/ADR-0010-engine-memory.md` | §2.4, §3.3, §3.5 |
| 24 | this repository | the product's own machine constants | — | — | `internal/config/machine.go:5-17, 19-48` | §2.3, §3.2 |
| 25 | this repository | the structural provider's worker model and reservation | — | — | `internal/provider/treesitter/provider.go:5, 59, 84-86, 97, 157-158, 343-352`; `internal/provider/treesitter/worker/worker.go:2`; `internal/provider/treesitter/wire/wire.go:2` | §2.2, §2.4, §3.2 |
| 25b | this repository | the runner's admission: a reservation is summed against a budget, never enforced as an RSS ceiling | — | — | `internal/process/runner.go:107-111, 166, 342-357, 391`; no `Setrlimit`/cgroup anywhere in `internal/process/` | §3.2, §3.3 item 2 |
| 26 | this repository | the memory allocation and the machine observation | — | — | `internal/provider/dependence/govern.go:175-196`; `internal/provider/dependence/machine_linux.go:18-42` | §2.4 |
| 27 | this repository | the paced reclaimer and its measurement | — | — | `internal/paced/reclaim.go:20-39` | §3.5 |
| 28 | this host | core count and memory | — | 2026-09-16 | `nproc` = 16; `free -g` = 47 GiB total, 38 GiB available | §3.2 arithmetic |
