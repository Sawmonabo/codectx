# Native-engine research, raw evidence — dominance, control dependence and dataflow: the algorithms chosen

Source reading and text measurement only; nothing was built, compiled or run. Engine paths are
relative to the engine clone at **v4.0.627**. Library paths are relative to the module root inside
the local module cache. Product paths are relative to the repository root.

Every engine citation is given twice: the **research form** (real path) in the table, and the
**ADR-safe form** in the column beside it, which is the form the ADR lifts.

## 0. Reference versions actually present in the local module cache

| module | version(s) present | what doc 20 §7.2 / doc 09 pin | status |
|---|---|---|---|
| `gonum.org/v1/gonum` | **v0.17.0** (only) | v0.17.0 | matches |
| `golang.org/x/tools` | v0.23.0, v0.30.0, v0.45.0, v0.47.1-0.20260707181000-a299dadba899, v0.48.0, **v0.49.0** (highest) | **v0.50.0** | **discrepancy — v0.50.0 is not on this host** |

**Finding for the lead.** Doc 09's library table and its `go.mod` addition row, and doc 20 §7.2, name
`golang.org/x/tools` v0.50.0. That version is not in the local module cache, so no claim in this
research about `go/ssa` has been checked against v0.50.0. Everything below about `go/ssa` was read at
**v0.49.0**. Resolving v0.50.0 would be a download and is not permitted; the correction doc 20 needs
is either to re-pin to a version that can be read here or to mark the v0.50.0 row as unverified.

**A second correction to doc 09 and doc 20 §7.2.** Both state that no surveyed Go package exposes a
dominance frontier, and size the port as "write post-dominator tree, dominance frontier, Ferrante
control dependence". The *public-API* claim is correct, but `go/ssa` **does compute a dominance
frontier** — `x/tools v0.49.0, go/ssa/lift.go:99-106` (`buildDomFrontier`) over the Cytron bottom-up
dom-tree recurrence at `go/ssa/lift.go:79-97`. It is unexported, so it cannot be imported, but it is
28 lines of BSD-3 source that can be read and ported rather than derived. The must-write list is
therefore shorter than doc 20 §7.2 states, and §7.5's "CFG + post-dominators + CDG core" row is
sized against a harder problem than the one that exists.

---

## 1. Dominators — which algorithm

**Decision: implement Cooper-Harvey-Kennedy iterative dominance over a dense `int32` reverse-post-order
numbering, in-repository, and do NOT add `gonum.org/v1/gonum` to `go.mod`.**

### Why

**(a) The workload's sizes are two to four orders of magnitude below where asymptotics decide.**
Measured on this host, by scanning `^func ` … `^}` spans of non-test Go source:

| corpus (measured here) | functions | mean body lines | p50 | p90 | p95 | p99 | p99.9 | max |
|---|---|---|---|---|---|---|---|---|
| this repository, `internal/` + `cmd/`, non-test | 3,272 | 21.3 | 13 | 47 | 66 | 122 | 231 | 387 |
| `golang.org/x/tools` v0.49.0, `go/` + `internal/` + `gopls/`, non-test | 4,153 | 20.8 | 9 | 47 | 78 | 187 | 326 | 669 |
| `modernc.org/sqlite` v1.58.0, the three largest generated files | 3,609 | 82.1 | 43 | 154 | — | 567 | 2,499 | **7,518** |

98.6% and 96.9% of hand-written functions are ≤100 body lines. The only four-digit functions on this
host are machine-generated C-to-Go transpiler output (`modernc.org/sqlite v1.58.0,
lib/sqlite_windows_386.go:95881`, 7,518 lines; the identical function appears in
`lib/sqlite_windows.go:226665` and `lib/sqlite_g_0000000000000003.go:342189`). That is the shape of
"thousands to tens of thousands of nodes at the extreme, millions of functions".

**(b) The literature's own crossover, as far as it can be verified.** Georgiadis, Tarjan and Werneck
re-ran the comparison Cooper et al. started, with careful implementations of both Lengauer-Tarjan
variants and a new hybrid (SEMI-NCA). Their abstract, read verbatim at the URL below: *"Our results
suggest that, although the performance of all the algorithms is similar, the most consistently fast
are the simple Lengauer-Tarjan algorithm and the hybrid algorithm, and their advantage increases as
the graph gets bigger or more complicated."* That is the crossover statement in the authors' own
words: **there is no size at which the asymptotically better algorithm wins decisively at small
sizes; its advantage is a function of growth.** The numeric per-size tables are in the paper body;
the PDF 404s at the URL `go/ssa/dom.go:15-17` itself cites
(`jgaa.info/accepted/2006/GeorgiadisTarjanWerneck2006.10.1.pdf`) and at the JGAA article download
URL, so **the per-size crossover figures are unavailable** to this research. The measurement that
would produce them is reading Tables 2-6 of the paper body. Cooper-Harvey-Kennedy's own reported
speedup is likewise **unavailable**: both mirrors of `dom14.pdf` cited on this host
(`hipersoft.rice.edu`, `cs.rice.edu`) return 404 / reset. Neither number is reconstructed from
memory here, and neither is needed: see (c).

**(c) gonum's authors publish no measurement at this workload's sizes.** `gonum v0.17.0,
graph/flow/control_flow_bench_test.go:137-165` benchmarks `Dominators` on synthetic G(n,m) graphs at
n = 10³, 10⁴, 10⁵, 10⁶ with m from n to 30n, and duplication graphs at n = 10³-10⁵. The one benchmark
over real control-flow graphs, `BenchmarkDominators` at `:28-83`, reads `./testdata/flow` — and
**`graph/flow/testdata/` does not exist in the distributed module**, so `b.Skipf` at `:31-37` fires
and that benchmark never runs from the module cache. gonum's published dominator measurements are
therefore synthetic and start at 1,000 nodes; the median function here has on the order of ten.

**(d) Below 10³ nodes the decision is made by constant factor, and the constant factor is
allocation.** Read at source:

- `gonum v0.17.0, graph/flow/control_flow_lt.go:16` allocates a `map[int64]int` per call;
  `:135-141` allocates one `*ltNode` **per node, each carrying its own `map[*ltNode]struct{}`
  bucket**; `:58-59` returns two more maps (`map[int64]graph.Node`,
  `map[int64][]graph.Node`). Every node is boxed as a `graph.Node` interface value. For one function
  of N nodes that is N+3 heap objects of which N+3 are pointer-bearing and N+1 are maps.
- `x/tools v0.49.0, go/ssa/dom.go:125-131` does the same algorithm with **one** allocation:
  `space := make([]*BasicBlock, 5*n)` sliced five ways into `sdom`, `parent`, `ancestor`, `preorder`
  and `buckets`. Zero maps.
- Cooper-Harvey-Kennedy over dense indices is the same shape as the x/tools allocation and
  simpler still: one `[]int32` of idom indices plus the RPO array, and an `intersect` two-finger walk
  up the idom chain. The engine's implementation is 90 lines including the intersect
  (`CfgDominator.scala`), of which the fixed point is 24 lines (`:41-64`).

At N ≈ 10-200 and 10⁶ functions, N+3 map allocations per function is 10⁷-10⁸ scannable objects
through the Go GC; a single pointer-free `[]int32` slab is zero scannable objects (§6).

**(e) Parity with the differential oracle.** The engine computes dominators **and** post-dominators
with Cooper-Harvey-Kennedy: `CfgDominator.scala:12-13` names the paper, and `CfgDominatorPass.scala:18,21`
instantiates it twice. A CHK port differs from the reference only where the CFG differs, which is
exactly the isolation doc 20 §7.3's per-family band is trying to achieve. A Lengauer-Tarjan port adds
a second axis of difference (tie-breaking in the semidominator step on irreducible graphs) that the
band would have to absorb.

### The alternative, steel-manned

**SEMI-NCA (Georgiadis-Tarjan-Werneck's hybrid), which is what LLVM adopted and what the paper's own
abstract calls one of the two "most consistently fast".** It computes the DFS and semidominators like
Lengauer-Tarjan, then derives immediate dominators by nearest-common-ancestor queries on the partially
built tree, giving worst-case O(n²) but near-linear observed behaviour with none of the iterative
algorithm's dependence on graph reducibility. Its case is genuine: the iterative algorithm's round
count is bounded by the loop-connectedness of the graph, so a pathological irreducible CFG — a
generated state machine, a `goto`-heavy C function, a Rust `loop`/`break 'label` nest — can force many
sweeps, and the sizes where that bites (10³-10⁴ nodes) are exactly the machine-generated functions
measured in (a) at 2,499 and 7,518 lines. SEMI-NCA has no such dependence.

**Why CHK still wins here.** The verified GTW statement is that at these scales *"the performance of
all the algorithms is similar"*; the advantage of the better algorithm "increases as the graph gets
bigger", and 99% of the graphs are under 200 lines. CHK is 90 lines with a 40-line naive checker
available as an oracle (below); SEMI-NCA is roughly three times that and needs its own link-eval
forest. And the engine's own choice is CHK, which buys oracle parity for free.

**Trade-off accepted.** The port owns ~120 lines of textbook algorithm and its tests rather than
importing a maintained BSD-3 implementation, and it accepts a worst case that is quadratic in N on an
adversarial irreducible CFG. Two mitigations make that acceptable: the test oracle already exists to
be read — `x/tools v0.49.0, go/ssa/dom.go:228-305` is a naive O(n²) dominance checker
(`// Check the entire relation.  O(n^2).` at `:278`) written for exactly this purpose — and if the
worst case ever bites, the replacement is confined to one function with one signature.

**This decision supersedes a pinned dependency.** Doc 09 lists `gonum.org/v1/gonum/graph/flow` under
"Import rather than write (3)" and its `go.mod` table adds it at v0.17.0; doc 20 §7.2 pins it and
sizes the must-write list "on top of gonum's dominator core". Under this decision **gonum is not
added at all**, and the shared core adds **zero** new modules to `go.mod`. The lead has to reconcile
doc 20 §7.2 and doc 09's addition table. The reason is not that gonum is wrong — it is a correct
Lengauer-Tarjan — but that its API shape (an interface-boxed `graph.Directed`, a per-node map bucket,
two maps out) is the one shape this workload cannot afford, and nothing about it is reusable once the
frontier computation needs an array-indexed `idom`.

| research-form citation | ADR-safe form |
|---|---|
| `joern-cli/frontends/x2cpg/src/main/scala/io/joern/x2cpg/passes/controlflow/cfgdominator/CfgDominator.scala:12-13,41-64` | `x2cpg/.../passes/controlflow/cfgdominator/CfgDominator.scala:12-13,41-64` |
| `.../controlflow/cfgdominator/CfgDominatorPass.scala:18,21` | `x2cpg/.../passes/controlflow/cfgdominator/CfgDominatorPass.scala:18,21` |

---

## 2. Post-dominators — settling doc 20's "weak" inference

**Decision: gonum's `Dominators` DOES compute post-dominators when handed a reversed view — the claim
doc 20's verification ledger flags as weak is UPGRADED to verified from source. It is nevertheless not
used, because §1 decides not to import gonum at all; the same proof applies unchanged to the
in-repository CHK routine, which is invoked on a reversed CSR.**

### The proof, from the module cache

`Dominators` takes `(root graph.Node, g graph.Directed)` (`gonum v0.17.0,
graph/flow/control_flow_lt.go:11`). `graph.Directed` (`gonum v0.17.0, graph/graph.go:108-120`) adds
`HasEdgeFromTo` and `To` on top of `graph.Graph`'s `Node`, `Nodes`, `From`, `HasEdgeBetween` and
`Edge`. The question is which of those the implementation actually consults.

**It consults exactly one: `From`.** `grep -n "\.To(\|HasEdgeFromTo\|\.Nodes()\|g\.Node("` over both
`graph/flow/control_flow_lt.go` and `graph/flow/control_flow_slt.go` returns **no match**. The only
graph access in either file is `to := g.From(v.ID())` — `control_flow_lt.go:143` and
`control_flow_slt.go:165`. Predecessors are not read from the graph at all: they are accumulated from
the observed forward edges during the DFS, `control_flow_lt.go:163` (`ltw.pred = append(ltw.pred, ltv)`).

So the algorithm depends on nothing but the successor relation the caller hands it, and a wrapper
whose `From` returns the CFG's predecessors yields the post-dominator tree. `DominatorOf` then
returns the immediate **post**-dominator. This is no longer an inference from a signature; it is a
property of the implementation at two named lines. Independent corroboration: the engine does exactly
this, with its own generic dominator routine — `CfgDominatorPass.scala:20-21` constructs
`new CfgDominator(new ReverseCpgCfgAdapter)` where the adapter swaps `_cfgOut`/`_cfgIn`
(`ReverseCpgCfgAdapter.scala:8,11`), and `:26` runs it.

### What a native implementation must do about the exit node, exactly

`Dominators` takes **one** `root`. Three consequences, each with its remedy:

1. **A unique exit is mandatory.** A CFG with several `return`/`throw`/`panic` terminators has no
   single root for the reversed graph. Add one synthetic EXIT node and an edge into it from every
   terminating node. The engine's answer is the same and is already in its data model: it roots the
   post-dominator computation at `method.methodReturn` (`CfgDominatorPass.scala:26`), a node its
   frontends wire every return into.

2. **Nodes that cannot reach the exit are silently absent, not reported.** `dfs`
   (`control_flow_lt.go:132-165`) visits only what is reachable from `root`; `dominatorOf` is filled
   only from `lt.nodes[1:]` (`:60-64`). A node an infinite loop encloses — `for {}`, a
   `while(1)` with no `break`, a region that only throws — is unreachable in the reversed graph, so
   `DominatorOf(id)` returns **nil**, indistinguishable from the root's own nil. There is no error
   and no second return value. The engine documents precisely this failure and lives with it:
   `DomTreeAdapter.scala:5-8` — *"The returned value can be None if cfgNode was the cfg entry node …
   or if cfgNode is dead code. In the post dominator case 'dead code' means code which does lead to
   the normal method exit. An example would be a thrown excpetion."*

3. **The remedy is the exit-side augmentation, and it is the same device §3 needs.** Before reversal:
   (i) add EXIT and an edge terminator→EXIT for every node with no CFG successor; (ii) compute the
   strongly connected components of the CFG and, for every non-trivial SCC from which EXIT is not
   reachable, add one edge header→EXIT, where the header is the SCC member with the smallest
   reverse-post-order number. After (i)+(ii) every node reaches EXIT, so the reversed graph is
   rooted at EXIT and the post-dominator relation is a **tree**, not a forest. Without (ii) it is a
   forest and every downstream consumer must handle a nil parent — which is where the engine's CDG
   loses edges (§3).

4. **The reversed view must supply stable, unique `int64` IDs**, because `indexOf` and both output
   maps are keyed by `Node.ID()` (`control_flow_lt.go:16,52,58-64`). For a per-function CFG the dense
   node index is that id. Note also that the returned tree holds `graph.Node` interface values, i.e.
   one boxed value per node per function — a second reason §1 declines the dependency and §6 declines
   pointer graphs.

| research-form citation | ADR-safe form |
|---|---|
| `.../controlflow/cfgdominator/ReverseCpgCfgAdapter.scala:8,11` | `x2cpg/.../passes/controlflow/cfgdominator/ReverseCpgCfgAdapter.scala:8,11` |
| `.../controlflow/cfgdominator/DomTreeAdapter.scala:5-9` | `x2cpg/.../passes/controlflow/cfgdominator/DomTreeAdapter.scala:5-9` |
| `.../controlflow/cfgdominator/CfgDominatorPass.scala:20-21,26` | `x2cpg/.../passes/controlflow/cfgdominator/CfgDominatorPass.scala:20-21,26` |

---

## 3. Control dependence — which formulation

**Decision: Ferrante-Ottenstein-Warren control dependence via post-dominance frontiers, computed on a
CFG augmented at the EXIT side only (§2 step 3), and WITHOUT FOW's ENTRY→EXIT edge.**

### The algorithm, precisely enough to implement

1. Build the per-function CFG; add EXIT and the augmentation edges of §2 step 3.
2. Compute the post-dominator tree: CHK iterative dominance (§1) on the **reversed** CSR, rooted at
   EXIT. Output: `pidom []int32`, one entry per node, total by construction after step 1.
3. Compute the post-dominance frontier with the Cooper-Harvey-Kennedy frontier recurrence:
   for each node `b` whose **reverse-graph predecessor count** (i.e. its CFG **successor** count) is
   ≥ 2, and for each such predecessor `p`, set `runner = p` and, while `runner ≠ pidom(b)`, add `b`
   to `PDF(runner)` and set `runner = pidom(runner)`. In the reversed graph a node with ≥2
   predecessors is precisely a CFG **branch** node, which is the FOW condition that control
   dependence originates only at branches.
4. Edge set: `n` is control dependent on `b` iff `b ∈ PDF(n)`. Emit one CDG edge `b → n` — controller
   to controlled — for every such pair.

The equivalent alternative formulation, Cytron et al.'s, computes the same set as the dominance
frontier of the reverse graph via the bottom-up dom-tree recurrence (`DF(u) = {v ∈ succ(u) :
idom(v) ≠ u} ∪ ⋃_{w ∈ children(u)} {v ∈ DF(w) : idom(v) ≠ u}`), which is what `x/tools v0.49.0,
go/ssa/lift.go:79-97` implements. The two produce identical frontiers. CHK's is chosen because it
needs only the `pidom` array and the reverse adjacency — no dom-tree child lists, no recursion over
the tree — which is one fewer structure in the arena (§6), and because it is what the reference
implements, which again buys oracle parity.

### What the engine computes, from its two files

`CdgPass.scala` (68 lines) and `CfgDominatorFrontier.scala` (38 lines), read in full.
`CdgPass.scala:33` constructs `new CfgDominatorFrontier(new ReverseCpgCfgAdapter, new
CpgPostDomTreeAdapter)`, `:36` runs it over `method :: method._containsOut`, and `:42` emits
`addEdge(postDomFrontierNode, node, EdgeTypes.CDG)`. `CfgDominatorFrontier.scala:20-34` is step 3
above verbatim, with `onlyJoinNodes` at `:15-16` applying the ≥2-predecessor filter and
`CpgPostDomTreeAdapter.scala:8-9` reading `pidom` off the `POST_DOMINATE` edges the previous pass
wrote.

**So the engine computes step 3 correctly, but on an un-augmented graph, and its CDG is a strict
under-approximation of the textbook FOW set in two named places.**

- **No ENTRY→EXIT edge, so nothing is control dependent on method entry.** The METHOD node is passed
  in (`CdgPass.scala:36`) but has exactly one CFG successor, so `onlyJoinNodes`
  (`CfgDominatorFrontier.scala:15-16`) rejects it and no CDG edge ever originates at it. Under
  textbook FOW the ENTRY node is given two successors (the real start and EXIT), which makes it a
  branch and makes every unconditionally-executed statement control dependent on ENTRY.
- **No EXIT-side augmentation, so the walk truncates instead of erroring.** `withIDom`
  (`CfgDominatorFrontier.scala:17-18`) drops any join node whose post-idom is `None`, and the walk at
  `:29-33` stops when `doms(currentPred)` returns `None`. Both are the forest case of §2 — code that
  does not reach the normal method exit. Every control dependence inside a non-terminating or
  always-throwing region is therefore missing from the engine's CDG, silently.

### What the product's published `control_depends_on` loses or gains under this choice

The projection is a **single-hop join requiring BOTH endpoints anchored**, with no walk:
`internal/provider/dependence/neo4jcsv/scratch.go:578-581` joins `anchors` to both ends of each CDG
edge and emits `('control_depends_on', ad.target, ac.target, e.dst, '', 'cdg', '')`. Anchors are the
entity itself, a **non-operator** call site (`scratch.go:550-553`, `method_full_name NOT LIKE
'<operator>.%'`) and an identifier/`FIELD_IDENTIFIER`/`METHOD_REF` with a `REF` edge. Contrast
`data_flows_to`, which walks up to 8 levels *through* unanchored nodes (`neo4jcsv.go:74-75`,
`scratch.go:616-622`).

That asymmetry is the whole answer, and it is visible in the measured counts on the reference
repository's stores: `control_depends_on` 101,814 sites / 59,850 relations against `data_flows_to`
1,983,374 / 1,060,579 — a 17.7× ratio (`14-store-counts-r3.md`, gen 3 figures).

- **Loses.** Because there is no walk, a missing CDG edge is a missing published fact with no
  alternate route. The engine's two under-approximations are therefore published as absence: no
  `control_depends_on` for anything inside a non-terminating region, and no
  `control_depends_on` naming the enclosing method. The EXIT-side augmentation this decision adopts
  recovers the first outright — every node gets a post-dominator, so the frontier walk never
  truncates — and that is a **net gain in published facts** over the reference.
- **Gains, and why the ENTRY edge is still refused.** Adding FOW's ENTRY→EXIT edge would make every
  unconditionally-executed anchored node control dependent on the METHOD node. The METHOD node **is**
  an entity (`scratch.go:542-543`) and therefore anchored, and `ac.target <> ad.target` would hold,
  so those edges **would be published** — a large family of rows each saying "this local is
  control dependent on the method that declares it", which carries no information a `contains`
  relation does not already carry. Refusing the ENTRY edge keeps the published set informative.

**Trade-off accepted.** The port deliberately does not compute the textbook FOW set: it computes the
FOW set on an exit-augmented CFG minus the ENTRY-rooted edges. That is one documented deviation from
the textbook, chosen because the publication boundary makes the omitted edges noise; it means a
future consumer that needs a *rooted* control-dependence tree (region formation, program slicing with
a single root) has to add the ENTRY edges back, and the place to do that is the in-flight structure,
not the published one.

| research-form citation | ADR-safe form |
|---|---|
| `joern-cli/frontends/x2cpg/src/main/scala/io/joern/x2cpg/passes/controlflow/codepencegraph/CdgPass.scala:33,36,42` | the control-dependence pass, lines 33, 36 and 42, at the pinned tag |
| `.../controlflow/cfgdominator/CfgDominatorFrontier.scala:15-18,20-34` | `x2cpg/.../passes/controlflow/cfgdominator/CfgDominatorFrontier.scala:15-18,20-34` |
| `.../controlflow/codepencegraph/CpgPostDomTreeAdapter.scala:8-9` | the post-dominator-tree adapter, lines 8-9, at the pinned tag |

---

## 4. Reaching definitions and def-use — port or replace

**Decision: REPLACE the dense bit-vector reaching-definitions pass with sparse SSA-based def-use,
built by Braun et al.'s construction, which needs no dominance computation at all. Do not port the
engine's formulation.**

### Complexity and memory, in the variables that matter

Let N = CFG nodes in the function, D = **distinct** definitions, U = uses, E = CFG edges.
The engine's `Definition` is a CFG node index (`package.scala:4`, doc 10), so the bit-vector universe
is a subset of the CFG nodes and **D ≤ N**.

| | time | memory | source |
|---|---|---|---|
| Dense bit-vector may-analysis (the engine's) | O(rounds × N × D/64) word operations; `rounds` is bounded by the loop-connectedness of the CFG under the RPO worklist seeding | **Θ(N × ⌈D/64⌉) words for `in`+`out` = Θ(N²/64) words when D ≈ N** | transfer `gen(n) ∪ (x \ kill(n))` at `ReachingDefProblem.scala:169-171`; RPO worklist `DataFlowSolver.scala:12-37` (doc 10) |
| Cytron et al. SSA + φ placement | O(N + E + |DF|) for the frontier, plus renaming; needs the **forward** dominator tree and its frontier | O(N + E + φ) | Cytron et al. 1991; the Go realisation is `x/tools v0.49.0, go/ssa/lift.go:79-106,402-470` |
| Braun et al. SSA, built directly from the AST/CFG | O(N + U) amortised; **no dominator tree and no dominance frontier** | **O(N + U)** — one value per definition, one operand list per φ | Braun et al. 2013 |

**The quadratic is the decisive term.** With `in` and `out` held as N rows of ⌈D/64⌉ 64-bit words,
`2 × N × ⌈D/64⌉ × 8` bytes, at D = N:

| N | dense `in`+`out` | SSA def-use (≈ 8U + 8N, U ≤ 2N) |
|---|---|---|
| 100 | 12.5 KiB | ≈ 2.3 KiB |
| 1,000 | 250 KiB | ≈ 23 KiB |
| 7,518 (the measured generated extreme) | **13.5 MiB** | ≈ 176 KiB |
| 10,000 | 24.0 MiB | ≈ 234 KiB |
| 100,000 | **2.33 GiB** | ≈ 2.3 MiB |

Arithmetic for the 7,518 row: ⌈7518/64⌉ = 118 words; 2 × 7518 × 118 × 8 = 14,193,984 bytes.
For the 100,000 row: ⌈100000/64⌉ = 1,563; 2 × 100000 × 1563 × 8 = 2,500,800,000 bytes.

A design that must run with **no caps and no subdivision** (doc 20 §7.1, `00-synthesis.md` §8) cannot
carry a term that reaches 2.33 GiB on one function. The engine does not have this problem because it
*does* have a cap — `--max-num-def`, default 4000, raised by the product to 40000 — and pays for it
by dropping every `REACHING_DEF` edge of an over-budget method (doc 10 §(b)). Adopting the dense
formulation means either reinstating that cap, which contradicts §7.1's no-caps property, or
accepting the quadratic. The sparse formulation makes the question disappear.

### What the choice costs at the product's boundary

The product publishes `data_flows_to` as declaration → declaration, depth-8 through unanchored nodes,
with the `variable` label discarded (doc 10 §(d); `neo4jcsv.go:74-75`, `scratch.go:603-625`). Asked
whether the two formulations produce the same published edge set: **no, and in three named places.**

1. **φ-nodes consume the depth budget.** A φ is an unanchored intermediate. A def→use pair that was
   one `REACHING_DEF` edge under the dense formulation becomes def→φ→…→use under SSA, and the walk
   at `scratch.go:616-622` spends one of its 8 levels per φ. At a join-heavy site (a loop carrying a
   variable through three nested branches) a declaration pair reachable within 8 hops under dense RD
   falls outside 8 under SSA. **This is removable, and the port must remove it:** resolve φ operands
   transitively inside the function at emit time, so that a φ never appears as a node in the exported
   edge set and a def reaches its uses in one hop exactly as before. With that done, the published
   closure is identical at every depth.
2. **The engine's `kill` over-kills; SSA does not.** `kill` is name and `code` equality plus an AST
   scan that kills every `fieldAccess` containing an identifier of the name
   (`ReachingDefProblem.scala:220-293`, doc 10). SSA uses real variable identity, so it does not kill
   `a.value` because something named `a` was reassigned in an unrelated scope. The native run
   publishes **more** edges here.
3. **`UsageAnalyzer` over-connects; SSA does not.** `sameVariable` is literally
   `nodeToString(use).contains(call.code)` — substring matching on frontend-normalised source text
   (`DdgGenerator.scala:349`, doc 10). SSA has no analogue, so the native run publishes **fewer**
   edges wherever that substring test produced a spurious match.

(2) and (3) are the fork doc 20 §4 already names ("copy the substring hack, or use real variable
identity and accept a different output"). This decision takes real variable identity. The consequence
for doc 20 §7.3's oracle is concrete and must be recorded there: the `data_flows_to` band is
**not symmetric** — the native producer is expected to gain edges from (2) and lose edges from (3),
so a per-family threshold on the *net* count would hide both. The band needs a signed, per-cause
breakdown for this family.

### Does Braun change the answer to §1, and what do the two passes share

**No, it does not change §1.** Braun et al. removes the dominance computation from *def-use*, but
control dependence still needs post-dominance (§3), so exactly one dominator routine is still
written. What it changes is the **count of dominator computations per function: one, not two.** The
forward dominator tree is not needed at all — no Cytron φ placement, no forward frontier — so the
port computes dominance once, on the reversed augmented CFG.

**What the two passes share is the reverse CSR and the arena, not the tree.** The post-dominator
computation traverses the reversed CFG; Braun's `readVariableRecursive` walks a block's
**predecessors** to find φ operands. Both are the same predecessor adjacency. The post-dominator tree
itself is consumed only by the frontier, and the SSA values only by the emit step, so neither pass
reads the other's output. That is what lets the pipeline of doc 20 §7.1 run CFG → post-dominators →
CDG and CFG → SSA def-use off one shared reverse adjacency in one arena.

---

## 5. Demand-driven and incremental

### 5.1 Interprocedural framework

**Decision: NO. Neither IFDS nor IDE is built. The port stays intraprocedural, with the closure and
global edges the engine already emits.**

Three reasons, each tied to the published surface. (i) IFDS/IDE buy **meet-over-valid-paths**
precision — results that respect call/return matching — and that distinction is invisible at a
boundary that publishes unlabelled, depth-8 bounded *reachability* between declarations and states in
its own documentation that "a dependence edge does not claim a proven end-to-end source-to-sink flow"
(`docs/providers-dependence.md`, the publication table and the paragraph beneath it). Paying for a
precision the surface cannot express is the definition of over-engineering under policy.md.
(ii) IFDS costs O(|E| · |D|³) in the exploded supergraph and requires a resident whole-program call
graph; the unit of work in this design is a **function** and the unit of caching a **file**
(doc 20 §7.1), so an IFDS solver reintroduces exactly the whole-repository resident structure the
memory rule forbids. (iii) The product's cross-function reach is already served differently: the
engine folds closure and global flows into `reaching_def capture` (doc 10 §(d)), and inter-procedural
questions are answered by the `calls` family and the graph walk, not by the dataflow pass.

**What is lost.** Nothing at the published surface today. What would be gained if the surface ever
changed to "prove that this untrusted input reaches this sink" is the whole of IFDS/IDE — that is a
different product with a different precision claim (doc 20 §7.4), and it should be decided as a
product question, not smuggled in as an algorithm choice.

### 5.2 Incremental dataflow

**Decision: NO incremental dataflow algorithm. A changed function is recomputed from scratch. The
invalidation boundary is the FILE.**

The numbers decide it. The measured p50 function body is 9-13 lines and the p99 is 122-187 (§1(a)), so
a function's CFG is tens to a couple of hundred nodes and its def-use construction is O(N + U) over
that — microseconds, from an arena that is already warm. An incremental formulation would have to
store, per function, a memoised summary plus the dependency edges that say which inputs it was
computed from, then maintain a dirty set across edits. That state is larger than the result it
protects and has to be invalidated correctly on every edit, which is a correctness surface with no
payoff at these sizes.

**The invalidation boundary is the file, identified by its content hash.** The unit of caching is a
file and the facts are file-local (doc 20 §7.1 and §7.5); a function belongs to exactly one file.
So: a file's content hash changes ⇒ every function in that file is re-analysed from scratch and every
other file's facts stand unchanged. The cost of the coarser boundary is that a one-character edit in
a 5,000-line file re-analyses all of that file's functions; the benefit is that the boundary is a
hash comparison with no cross-function bookkeeping, and the work is bounded by one file, never by the
repository. That is the same property doc 20 §7.1 claims for memory, applied to time.

---

## 6. Data layout and the memory bound

### 6.1 Adjacency

**Decision: compressed sparse row over dense `int32` node indices — `offsets []int32` of length N+1
and `targets []int32` of length E — built once per function in both directions, with NO varint and NO
delta coding.**

**The alternative, steel-manned: reuse ADR-0005's packed per-generation adjacency shape** — varint
deltas of the neighbour surrogate, a varint relation surrogate and a one-byte kind code from a
per-generation dictionary (ADR-0005, Decision 1, "Layout"). One layout, one port, one set of tests,
and a proven measurement behind it: varint decoding costs "a few nanoseconds per edge"
(`17-graph-traversal-throughput.md` §3.1) and the packed form is ≈0.8% of the store.

**Why it loses here, and the principle that separates the two.** ADR-0005's artefact is *persisted,
built once per generation, and scanned many times by many queries*; its win is locality in a ~10 MiB
blob read against a 1.36 GiB store, and its decode cost is amortised over thousands of traversals. A
per-function CFG is built once, read a handful of times (RPO numbering, post-dominators, the frontier
walk, def-use) and discarded. At N ≤ 10⁴ the whole structure is under a megabyte and already resident
in L2/L3, so varint buys no locality it does not already have, and costs a branch per edge on the hot
loop of every pass. **In-flight and published structures differ because their read counts and
lifetimes differ by orders of magnitude**; that is the sentence the ADR should carry.

Edge lists are rejected outright: every pass here is "for each node, its successors" or "for each
node, its predecessors", which an edge list can only serve after a sort or a hash per pass.

### 6.2 Bitsets

- **Word size: 64 bits (`uint64`).** It is the native word on both supported architectures and it is
  what `math/bits.TrailingZeros64` and `OnesCount64` compile to as single instructions, which is what
  the frontier and worklist scans need.
- **`in`/`out`, in the dense formulation: one flat `[]uint64` of N × ⌈D/64⌉ words, row-major**, so a
  node's row is contiguous and the union/difference is a word loop over two slices with one bounds
  check. Not `[]*bitset` and not a slice of slices: those are N pointer-bearing objects per function.
- **`gen`/`kill`: sparse, as CSR of `int32`**, never a dense row per node. |gen(n)| is 1 per parameter
  and 1 + |valid args| per call (doc 10 §(a)), and |kill(n)| is the same-name set — both O(1)-ish per
  node, so a dense row would spend ⌈D/64⌉ words to carry two bits.
- **Dense or sparse at the sizes that actually occur:** dense, decisively, for `in`/`out`. At the
  measured p99 (N ≈ 10²) ⌈D/64⌉ is 1-4 words, so a row is 8-32 bytes — smaller than the two slice
  headers a sparse representation would need to describe it. The dense representation only loses
  above D ≈ 512, which is the region the §4 decision removes.
- **Consequence of the §4 decision, stated plainly:** under sparse SSA def-use there is no N × D
  matrix at all. The bitsets that remain are the DFS/worklist visited sets and the frontier
  de-duplication set, each one row of ⌈N/64⌉ words. The specification above is kept because the ADR
  needs it if the dense path is ever reinstated, and because the frontier and SSA φ-placement sets use
  the same word-level primitives.

### 6.3 Memory per worker — the bound, with the arithmetic

Per-function structures, all `int32`/`uint64` and all from one arena:

| structure | bytes |
|---|---|
| forward CSR (`offsets` N+1, `targets` E) | 4(N+1) + 4E |
| reverse CSR | 4(N+1) + 4E |
| RPO order + inverse index | 8N |
| `pidom []int32` | 4N |
| post-dominance frontier, CSR (`Σ|PDF|` ≤ E in the common case) | 4(N+1) + 4·Σ|PDF| |
| SSA values + operand lists (§4) | ≈ 8U, U ≤ 2N |
| `in`/`out` **only if the dense formulation is used** | 16 · N · ⌈D/64⌉ |

A CFG node has at most two successors except at a switch, so E ≤ 2N + S where S is the total switch
arity; taking E ≤ 2N covers everything but switch-heavy code and S is additive, not multiplicative.
Substituting E ≤ 2N, Σ|PDF| ≤ 2N and U ≤ 2N:

> **M_sparse(N) ≤ 76·N + 32 bytes** (the decided design)
> **M_dense(N) ≤ 76·N + 16·N·⌈D/64⌉ + 32 bytes** (the engine's formulation, for comparison)

**Is D capped?** **No.** D is not bounded by a policy constant here — the engine's `--max-num-def`
(4000 default, 40000 in the product) is a cap whose price is dropping every `REACHING_DEF` edge of the
method (doc 10 §(b)), and doc 20 §7.1's no-caps property forbids that. D is bounded **structurally**
instead: a definition is a CFG node index, so **D ≤ N**, and the bound is a function of the function's
own size with no tunable in it. That is what makes "no caps" a property rather than a policy.

**Evaluated at the largest function measured on this host.** The extreme is 7,518 body lines
(`modernc.org/sqlite v1.58.0, lib/sqlite_windows_386.go:95881`). The conversion from body lines to
CFG nodes is **unavailable** from source reading — a CFG node is an expression or operand, not a line,
so N exceeds the line count by a language- and frontend-dependent factor. The measurement that would
produce it is a count of CFG nodes per method over a store, read with `sqlite3 -readonly`; this
research did not do it. The bound is therefore tabulated in N so it holds under any factor:

| N | M_sparse | M_dense (D = N) |
|---|---|---|
| 10² | 7.6 KB | 18.4 KB |
| 10³ | 76 KB | 332 KB |
| 10⁴ | 760 KB | 24.7 MB |
| 10⁵ | 7.6 MB | 2.34 GB |

At N = 10⁴, which covers the measured 7,518-line extreme under any lines-to-nodes factor up to ≈1.3,
**the decided design needs 760 KB per worker for the function's structures**, plus one file's CST.
Even at N = 10⁵ — a lines-to-nodes factor of 13 on that same function — it needs 7.6 MB. The dense
formulation needs 2.34 GB at the same point, which is the whole argument of §4 in one row.

### 6.4 Arena allocation

**Decision: yes — one reset slab arena per worker, holding pointer-free typed backing arrays
(`[]int32`, `[]uint64`), with every per-function structure a sub-slice of it and a `reset()` between
functions; the arena RELEASES its backing arrays when a function required more than 1 MiB.**

**What it buys in Go specifically.** Two distinct things, and the second is the larger.
(i) *Allocation count*: without an arena a worker allocates on the order of ten slices per function;
over 10⁶ functions that is 10⁷ allocations, each of which advances the heap-growth counter that
`GOGC` pacing responds to, so the collector runs proportionally more often for work that produces no
long-lived data. With the arena it is O(1) allocations per worker.
(ii) *GC scan cost*: Go's mark phase scans only pointer-bearing objects; a slab of `[]int32`/`[]uint64`
is pointer-free, lands in a `noscan` span, and is **never scanned at all**, whatever its size. That
is why the layout of §6.1 is index-based CSR rather than a `*node` graph: a pointer graph of 10⁴ nodes
is 10⁴ scannable objects per function, and gonum's dominator returns exactly that shape — one
`*ltNode` per node, each holding a `map[*ltNode]struct{}` (`gonum v0.17.0,
graph/flow/control_flow_lt.go:135-141`), plus two maps out (`:58-59`). Maps are among the most
expensive objects for the collector to scan, and at 10⁶ functions that cost is not recoverable from
outside the package. This is the same evidence that decided §1.

**What it costs in safety.** A structure's lifetime becomes the worker's function slot rather than the
object's. Anything that outlives the slot — an emitted fact, a diagnostic message, a byte range —
must be **copied out before `reset()`**, and a miss is not a panic: Go will not fault on a reused
slice, so the failure mode is a silently corrupted fact, which is precisely the class the policy's
critical-invariant test bar exists for. Two mitigations make it acceptable: the arena hands out
**index ranges into typed slices, never pointers**, so a stale handle is an out-of-range index rather
than a live-looking wrong value; and the emit step copies into the output buffer before the reset, on
one code path, which is the one place a test is warranted.

**The high-water-mark problem, and why the release threshold is part of the decision.** A reset slab
makes a worker's resident memory the **maximum** over every function it has processed, not the
current one. Left alone, one worker that touched a 7,518-line generated function would hold that
slab for the rest of the run, and doc 20 §7.1's claim — "memory per worker is one file's CST plus one
function's CFG and bitsets, so nothing in the design grows with repository size" — would be false as
written: the footprint would grow monotonically with what the worker had *seen*. The threshold fixes
it. Above 1 MiB the arena drops its backing arrays and re-allocates at the default size, so the
steady-state per-worker footprint is `max(1 MiB, M_sparse(N_current))` plus one file's CST — a
function of the function in hand, never of the repository. By the §6.3 table 1 MiB covers N ≈ 13,000
under the decided design, which is above the p99.9 of every corpus measured here and above the
measured generated extreme.

**Where this meets the whole-run model:** the per-worker figure above is one side of it. How many
workers run, how they are scheduled and how the totals coexist with the rest of the process is the
adjacent lane's, and the two have to be reconciled by multiplying this bound by the worker count and
adding the CST residency.

---

## Sources

| # | author | title | venue | year | URL | used for |
|---|---|---|---|---|---|---|
| A1 | Lengauer, T.; Tarjan, R. E. | A Fast Algorithm for Finding Dominators in a Flowgraph | ACM TOPLAS 1(1):121-141 | 1979 | https://doi.org/10.1145/357062.357071 | the algorithm gonum and `go/ssa` implement; §1, §2. Cited on this host at `gonum v0.17.0, graph/flow/control_flow_lt.go:13` and `x/tools v0.49.0, go/ssa/dom.go:11-13` |
| A2 | Cooper, K. D.; Harvey, T. J.; Kennedy, K. | A Simple, Fast Dominance Algorithm | Software Practice and Experience (Rice TR) | 2001 | http://www.hipersoft.rice.edu/grads/publications/dom14.pdf — **404 at fetch time; the paper's own numbers are unavailable to this research** | the iterative dominator and frontier algorithms chosen in §1 and §3. Cited on this host at `x/tools v0.49.0, go/ssa/lift.go:17-19` and, in the engine, at `CfgDominator.scala:12-13` and `CfgDominatorFrontier.scala:8-9` |
| A3 | Georgiadis, L.; Tarjan, R. E.; Werneck, R. F. | Finding Dominators in Practice | Journal of Graph Algorithms and Applications 10(1):69-94 | 2006 | https://jgaa.info/index.php/jgaa/article/view/paper119 (DOI 10.7155/jgaa.00119) — **abstract verified over the network; the per-size tables in the body are unavailable** | the experimental comparison and the SEMI-NCA hybrid; §1's crossover discussion and the steel-man |
| A4 | Ferrante, J.; Ottenstein, K. J.; Warren, J. D. | The Program Dependence Graph and Its Use in Optimization | ACM TOPLAS 9(3):319-349 | 1987 | https://doi.org/10.1145/24039.24041 | the control-dependence definition and the augmented CFG; §3 |
| A5 | Cytron, R.; Ferrante, J.; Rosen, B. K.; Wegman, M. N.; Zadeck, F. K. | Efficiently Computing Static Single Assignment Form and the Control Dependence Graph | ACM TOPLAS 13(4):451-490 | 1991 | https://doi.org/10.1145/115372.115320 | the dominance-frontier formulation of control dependence and of φ placement; §3, §4. Cited on this host at `x/tools v0.49.0, go/ssa/lift.go:14-15` |
| A6 | Braun, M.; Buchwald, S.; Hack, S.; Leißa, R.; Mallon, C.; Zwinkau, A. | Simple and Efficient Construction of Static Single Assignment Form | Compiler Construction (CC) 2013, LNCS 7791:102-122 | 2013 | https://doi.org/10.1007/978-3-642-37051-9_6 | the SSA construction chosen in §4, which needs no dominance computation |
| A7 | Reps, T.; Horwitz, S.; Sagiv, M. | Precise Interprocedural Dataflow Analysis via Graph Reachability | POPL 1995 | 1995 | https://doi.org/10.1145/199448.199462 | the IFDS framework declined in §5.1, and its O(\|E\|·\|D\|³) cost |
| A8 | Sagiv, M.; Reps, T.; Horwitz, S. | Precise Interprocedural Dataflow Analysis with Applications to Constant Propagation | TAPSOFT 1995 / TCS 167(1-2):131-170 | 1996 | https://doi.org/10.1016/0304-3975(96)00072-2 | the IDE extension declined in §5.1 |
| B1 | The Gonum Authors | `gonum.org/v1/gonum/graph/flow` (BSD-3-Clause) | module source, local module cache | v0.17.0 | — | read at `graph/flow/control_flow_lt.go`, `control_flow_slt.go`, `doc.go`, `control_flow_bench_test.go`, `graph/graph.go`; §1(c)(d), §2, §6.4 |
| B2 | The Go Authors | `golang.org/x/tools/go/ssa` (BSD-3-Clause) | module source, local module cache | v0.49.0 | — | read at `go/ssa/dom.go` and `go/ssa/lift.go`; §0, §1(d), §3, §4, and the O(n²) dominance checker used as a test oracle |
| C1 | — | the engine, at the pinned tag | engine clone | v4.0.627 | — | `CfgDominator.scala`, `CfgDominatorPass.scala`, `CfgDominatorFrontier.scala`, `CpgCfgAdapter.scala`, `ReverseCpgCfgAdapter.scala`, `DomTreeAdapter.scala`, the control-dependence pass and its post-dominator-tree adapter, all read in full; §1(e), §2, §3 |
| D1 | — | this research's own measurements | this host | 2026-09-16 | — | the three function-size distributions in §1(a); the module versions in §0; the absence of `graph/flow/testdata/`; the `grep` that returns no match over both gonum dominator files in §2 |

