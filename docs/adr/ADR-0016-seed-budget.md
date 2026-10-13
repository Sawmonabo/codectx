# ADR-0016: The context planner reads lexical matches until no unread match can change what it packs

## Status

Accepted, 2026-10-09, by the owner's ruling. Implemented: decisions 1 to 6 in the context compiler and the search
service's in-process read, decision 7's proofs in the context tests. The survey and the line-by-line check of the
proposed stop against the code are [research 25](../research/25-seed-budget.md).

## Context

- **Every match is read and walked.** The lexical seed step pages the task text's matches to exhaustion and turns
  every hit into a seed and every hit that names a node into a walk root (`internal/context/seeds.go:317-374`). The
  comment there gives the reason: reading one page made the plan depend on where a page boundary fell.
- **Under top-k that read is quadratic.** The search index serves page n as the top 200·n
  ([ADR-0013](ADR-0013-search-index.md)), so reading m pages costs the sum of top-200 to top-200m, and every match
  still becomes a root of one walk.
- **Worked example.** Task: "fix the login timeout". The match on "the" reaches every document holding it, more than
  200,000 on a large public repository. The plan's default file budget is 200 files (`docs/configuration.md:364`).
  The 150,000th match cannot change which 200 files are packed, yet it is read and walked.
- **How a candidate is scored now.** Its score is the larger of its origin contribution and its best route, plus
  boosts (`internal/context/stream.go:915-975`). A lexical origin contributes 0 (`internal/context/rank.go:221-231`),
  and a route's product starts at the full scale whatever its root (`rank.go:181`). The boosts are active change
  120,000, associated test or contract 100,000 and centrality up to 50,000, against a seed scale of 1,000,000
  (`internal/context/compiler.go:1094-1103`). The associated-test boost fires when any admitted route used a
  `tests`, `implements`, `overrides` or `extends` edge (`rank.go:297-305`). Centrality counts the distinct edges the
  whole compile walked in the entry's package (`internal/context/rankjoin.go:527-541`). An entry's requirement comes
  from its node kind and its depth to the nearest root of any class (`internal/context/scope.go:79-120`).
- **How files are packed.** Per file, in rank order. A group stops being admitted at the file limit, is skipped when
  it is larger than one slice, and nothing is admitted after the slice limit (`internal/context/slice.go:78-93`). A
  group's size is the file's source plus each entry's measured metadata (`internal/context/budget.go:110-160`), and
  explanation paths per entry are unlimited by default (`docs/configuration.md:369`).
- **The requirement.** Complete by default, nothing to tune, no fixed count on generated data, and a ranked search
  serves top-k.

The proposal first put forward, to stop at the score level where the matches read fill the file budget and then read
again inside the kept files, is not exact as written: research 25 §4 traces six ways an unread match still changes
the plan. The decisions below are that proposal with each of the six closed.

## Decision

### 1. Each root lends what it reaches a (requirement, score) pair, and the entry takes the largest pair

- The requirement is the node kind and the depth from *that* root, capped by the root's class (a lexical root never
  makes an entry required) and by the route's shape: a route that goes up `extends`, `implements` or `overrides` from
  a subtype to its supertype and straight back down the same kind to another subtype reaches a sibling, which is
  recommended at most whatever its kind. That is the round-trip meta path P P⁻¹ that heterogeneous-graph similarity
  defines peers by [S9], and the split the language server protocol keeps between supertypes and subtypes [S10]: the
  route depends on the supertype, not on the sibling. The score is the root's weight times (the route's product plus that route's own
  route-dependent boosts).
- The entry's rank key is the lexicographic maximum of its origin pair and every root's pair. It is never a minimum
  depth over all roots combined with a maximum score taken separately, and never a sum across routes.
- Root-independent boosts (active change) are scaled by the weight of the root whose pair wins.
- A lexical hit carries its score and tier onto its seed; its origin contribution is proportional to its score
  relative to the top lexical score, and below an exact match's contribution.

With a maximum, a root whose pair is below an entry's key cannot change that entry: the aggregation is monotone, which
the stop in decision 3 needs.

**Alternatives.** Depth and score maximized separately: an entry 3 hops from a named root and 1 hop from an unread
lexical match takes its requirement from the match and its score from the named route, a key no single root gives,
and it can rise above the packed files only when the match is read. Boosts combined across routes, as the
associated-test boost is now: an unread root's route adds 100,000 to an entry already above the stop.

### 2. Centrality counts only edges walked from named roots

The centrality boost counts the distinct edges walked in the entry's package from explicit and exact roots only. This
is a ranking change, not only a correctness fix: a task with no explicit or exact seeds gets zero centrality
everywhere. The compiler's policy version changes with it.

**Alternative.** Centrality from every root's walk. An unread match's walk adds edges to a package and raises an
already-packed entry's boost by up to 50,000, which reorders entries at the budget boundary. That breaks the
condition that an entry's grade depends only on its own routes, and no margin repairs it.

### 3. The read stops at a score below which no unread match can rank above the packed files

- Lexical matches are read in score order through the internal search call, which takes a depth and grows it until
  the floor is passed; the 200-item page is a wire bound and does not apply to this in-process call. Each step resumes
  after the last match served and doubles the depth, because a resumed read rescores the ranking above its position:
  steps of a fixed P cost about N²/(2P) document scorings for N matches, doubling about 4N. A step's peak memory,
  computed from the library's and the service's structures, must fit the query memory admission
  (`resources.query_memory_bytes`) less the compile's sort share. When a doubling does not fit, the read keeps the
  largest depth that does, never less than one page, so a small admission costs more steps and never a refusal or a
  different answer. The bound is enforced, not estimated: every index request of a step carries the bytes the step may
  still hold, and the library charges every allocation that grows with depth or position (the heaps of the segments
  scored at once, every segment's 24-byte run entries, the marked groups, any rescoring and each served result with
  the caller's copy), fits its heap to the bound beside the marked groups' bound, and ends a page early, with an exact
  prefix that says more, rather than hold more. The marked-group bound is exact: every group a later position marks
  is marked already or is a run entry the page before took. The hits are handed over and hydrated a page at a time, so a
  stop mid-step reads nothing from storage past its page. A query deadline that ends the read leaves the scope
  incomplete and names the step in the exclusions. Nothing is spooled.
- τ(next) is the largest final score anything derived from the next unread match could reach: its origin
  contribution, a route at the full scale, and every boost the root could lend. It is computed with the compiler's own
  integer functions, never with a floating ratio. With the boosts scaled by root weight (decision 1), the margin is
  the existing boost constants over the seed scale, 1.27 times the match's contribution; it is derived, not tuned.
- The read stops after the last hit scoring at or above the floor, where the floor is the lowest score at which F+1
  distinct witness files (F the plan's file budget) each hold a read match whose contribution is at least τ(next).
  "Above the stop" means a rank key greater than the pair (`recommended`, τ(next)), not a bare score.
  Ties at the floor are read across page boundaries.
- A witness is a file whose encoded source fits one slice.

This is the threshold algorithm of the middleware literature run over one sorted list, with files as the objects and
the file budget as k: it halts once k objects are known whose grade no unseen object can exceed, and it is exact when
the aggregation is monotone [S1].

**Alternatives.** Read every page (the current loop): exact by definition, but quadratic under top-k, and a walk from
every match on every broad task. A fixed count of matches, as several agent tools do [S3]: the answer then depends
on the count, not on the caller's budget, and the count is a constant nobody can justify across repositories. A score
floor or the largest gap: carries builder-chosen constants and loses recall, about 70 against 87 for a large fixed
budget [S6], and learned cut-offs do not beat a fixed depth [S7]. Stopping at the budget's score level without the
margin: an unread match one boost away from the last witness still outranks it.

### 4. After packing, a certificate confirms the stop

A witness counted at seed time can still be skipped when packed, because its group's metadata is unbounded. After
packing, the compiler checks that the packer stopped admitting (at the file limit or the slice limit) at a group whose
rank key is above (`recommended`, τ(next)). When it did not, the witness count rises by the number of witness groups
that were skipped, the read continues down to the new floor, and the plan is packed again. The check depends on rank
keys, not on pages.

### 5. A closure step completes every packed file

- **The kept-file read.** A second lexical read restricted to the files the plan keeps, through the search
  request's exact path filter, batched by its filter limit (`internal/model/query.go:252`,
  `internal/model/validate.go:48`). It is the second pass of grouped retrieval: the first pass finds the top groups,
  the second fetches what lies inside them [S4][S5].
- **The unreached components.** The kept files' nodes the walk did not reach are listed, their connected components
  over the scope relations are walked, and every lexical match inside those components becomes a root. A class's
  methods hang off it by `contains`, which the scope walk does not follow (`internal/context/scope.go:27-40`), so a
  method reachable only from an unread match in another file is otherwise missing. These components are disjoint
  from the first walk by construction. The callers of the open call sites, in any file, that may call an unreached
  node by name (ADR-0015 decision 3) start the component walk too (a caller and its site share a component over
  `calls`), so a site that only an unread match reaches is walked from that match, and the name-only candidates it
  lends to the kept files join the plan as they would in the full read.
- **Order.** Pack, close, walk from the new roots, pack again, check the certificate. The closure adds entries only to
  kept files, all keyed at or below (`recommended`, τ(next)), so it cannot reorder the head above the stop. It can push
  a witness past a slice, which the certificate catches. The order above the stop is fixed and drops are permanent,
  so the loop ends.
- **A deadline inside the closure.** The closure runs as one unit: what it admits goes straight into the plan's
  spools under a mark, which costs nothing while it is set (a spill that straddles it writes two runs instead of
  one, so each run lies on one side; the merge breaks ties by run order, so the answer is unchanged). On a compile
  that answers continuations, a query deadline that lands inside it rolls the spools back to the mark, the way a
  savepoint is rolled back [S13], and ends the call before the closure, so the continuation runs the closure whole and the plan is the uninterrupted
  compile's. A closure the deadline cuts on two legs in a row cannot fit one call; its second run narrows the scope
  on the cut, as a walk page that stalls twice does.

**Alternative.** Lexical matches as leaves, never roots: trivially exact and the cheapest exact rule, but it drops the
only graph expansion a plain-language task gets; an agent localization study lost 5.5 points of function accuracy at
10 without traversal [S8].

### 6. The answer says what was read and what is exact

- **Exact:** which files and entries rank above the stop, with their scores and order; every entry present in each
  packed file; and the packed file set whenever the file budget or the certificate decides it.
- **Stated, not exact:** below the stop, inside packed files, scores, order and explanation routes come from the
  matches read; a route from an unread root is missing from the explanations. With explanation paths unbounded, a plan
  byte-identical to the full read is impossible without the full read.
- **The notice:** "Lexical matches scoring below X were not read. N matches above it fill more than the plan's file
  budget of F, so no unread match can rank above the packed files. Every match and walk entry inside the packed files
  is included. Explanations list routes from the N matches read." The scope is complete. A certificate that cannot be
  satisfied, or a resource-limit error, still marks it incomplete (`internal/context/seeds.go:337-345`).

### 7. Proof

- **Differential.** On the fixtures, the stopped plan's ranked head above the stop, its packed file set and each
  packed file's entry set equal those of the plan built from every match (mutation: stop without the margin).
- **Centrality.** An unread match whose walk adds edges in a packed entry's package leaves the plan unchanged
  (mutation: centrality from every root).
- **Certificate.** A witness file that becomes oversized at pack time makes the read continue past the first floor
  (mutation: count witnesses at seed time only).
- **Pairs.** An entry 3 hops from a named seed and 1 hop from an unread lexical match keeps the rank key it has in the
  full read (mutation: requirement and score maximized separately).
- **Closure.** An entity in a kept file reachable only from an unread match inside a kept file, or only from an unread
  match outside the kept files, is still packed (mutations: skip the kept-file read; skip the closure walk).
- **Weak roots.** A lexical root never makes an entry required.
- **Budget monotonicity.** For a plan bound by its file budget, raising the budget never removes a function, and the
  plan is the same for any page size. An entry is charged as the entries view serves it: its fixed fields, its reasons
  and its route count spelled at its widest, never its routes. The routes are stored in full, one row each, and paged
  by reference through the `routes` view in (entry, route) order; the entry's count says how many to read. So how many
  routes reach an entry never changes whether its file fits (mutation: measure the entry with its routes). Reasons
  stay in the charge: they are bounded per entry (`MaxReasonsPerEntry` reasons of `MaxReasonBytes` each) and come from
  the route that wins the entry, so when the byte budget binds, a root that changes an entry's winning route can still
  move its group's size by at most that bound, and the property is stated exactly for file-budget-bound plans.

## Consequences

- A broad task reads matches down to a floor set by the plan's own file budget, not to the end of the index, and
  walks only from them; the walk no longer scales with how many documents hold a common word.
- Every root, whatever its weight, is walked in one walk. A node keeps each pair no other pair at it beats on every
  count the ranking can only improve with (depth, class, weight, route product, remaining derived depth, and the
  route's shape: whether it reached a sibling, whether its last hop went up a hierarchy kind and so may turn, and
  whether it entered a callee through an argument), so the largest pair of decision 1 is among the ones it keeps; the
  walk is best-first on the score, because a heavier root farther away can lend more than a lighter one next to the
  node.
- The walk crosses data flow across calls as the impact walk does: a route that entered a callee through a call's
  argument never leaves it through a return flow, which would come out at every call site's result. It is the
  two-phase slice of Horwitz, Reps and Binkley [S11] kept per route, as a label dimension: a route that has not
  descended stands for one that has, never the reverse.
- The read is deeper than a stop at the budget's score level, by the boost margin, and costs a few path-filtered reads
  and one walk over the unreached components; that walk's cost is measured in the scale proofs.
- Centrality becomes a signal of the named roots' neighbourhood. A task with no named seeds ranks by lexical score
  and routes alone.
- The compiler's policy version changes, so existing manifests are recompiled, not reused.
- Explanations list routes from the matches read; the notice says so.
- A plan's routes are a paged projection of their own, as the exclusions are. Agent context packers fill one budget
  [S3], so the routes are not free; they are served by reference, as a protocol-level resource link serves a large
  result [S12], and an entry costs a fixed header for them. A per-token knapsack would fit more routes but its
  selections are not nested as the budget grows, which breaks the rank order this record rests on.

## Sources

- [S1] Fagin, Lotem, Naor, *Optimal Aggregation Algorithms for Middleware*, JCSS 2003, https://arxiv.org/abs/cs/0204046
  — the threshold algorithm; halting once k objects reach the threshold; correctness for a monotone aggregation
  (Theorem 4.1).
- [S2] Ding, Suel, *Faster Top-k Document Retrieval Using Block-Max Indexes*, SIGIR 2011,
  https://research.engineering.nyu.edu/~suel/papers/bmw.pdf — safe early termination returns exactly the baseline's
  results; the heap's lowest score as the threshold.
- [S3] *aider/repomap.py*, https://raw.githubusercontent.com/Aider-AI/aider/main/aider/repomap.py;
  *workspaceChunkSearchService.ts*, https://raw.githubusercontent.com/microsoft/vscode-copilot-chat/main/src/platform/workspaceChunkSearch/node/workspaceChunkSearchService.ts;
  *retrieval.ts*, https://raw.githubusercontent.com/continuedev/continue/main/core/context/retrieval/retrieval.ts —
  budget fills, window-scaled counts and a relative score floor, none claiming equality with a full read.
- [S4] *Lucene grouping package*, https://lucene.apache.org/core/9_11_0/grouping/org/apache/lucene/search/grouping/package-summary.html
  — a first pass gathers the top groups, a second gathers the documents within them.
- [S5] *Collapse search results*, https://www.elastic.co/docs/reference/elasticsearch/rest-apis/collapse-search-results
  — one query per collapsed group to expand it.
- [S6] Taguchi, Maekawa, Bhutani, *Efficient Context Selection for Long-Context QA: No Tuning, No Iteration, Just
  Adaptive-k*, EMNLP 2025, https://arxiv.org/html/2506.08479v3 — largest-gap cut-off and its recall.
- [S7] Meng et al., *Ranked List Truncation for Large Language Model-based Re-Ranking*, SIGIR 2024,
  https://arxiv.org/html/2404.18185v1 — learned truncation shows no clear advantage over a fixed depth.
- [S8] Chen, Tang, Deng, Wu, Wu, Jiang, Prasanna, Cohan, Wang, *LocAgent: Graph-Guided LLM Agents for Code
  Localization*, arXiv:2503.09089v2, https://arxiv.org/html/2503.09089 — section 5.6, Table 6: with the fine-tuned
  Qwen-2.5-7B, removing the graph-traversal tool drops function-level Acc@10 from 71.53 to 66.06.
- [S12] *Model Context Protocol, tools, 2025-06-18*, https://modelcontextprotocol.io/specification/2025-06-18/server/tools
- [S13] *SQLite, SAVEPOINT*, https://www.sqlite.org/lang_savepoint.html
  — a tool result may return a `resource_link`, a URI the client fetches or subscribes to for additional context.
- [S9] Sun, Han, Yan, Yu, Wu, *PathSim: Meta Path-Based Top-K Similarity Search in Heterogeneous Information
  Networks*, VLDB 2011, https://www.vldb.org/pvldb/vol4/p992-sun.pdf — typed, directed relation paths; the inverse
  relation R⁻¹; peers defined on round-trip meta paths P P⁻¹ (pages 993-994).
- [S10] *Language Server Protocol 3.17, type hierarchy*,
  https://microsoft.github.io/language-server-protocol/specifications/lsp/3.17/specification/#textDocument_prepareTypeHierarchy
  — `typeHierarchy/supertypes` and `typeHierarchy/subtypes` are separate requests.
- [S11] Horwitz, Reps, Binkley, *Interprocedural Slicing Using Dependence Graphs*, TOPLAS 12(1), 1990,
  https://research.cs.wisc.edu/wpis/papers/toplas90.pdf — section 4.5, forward slicing in two phases: the second
  phase descends into called procedures and never ascends back out through a call site.
- [research 25](../research/25-seed-budget.md) and [raw/seed-budget](../research/raw/seed-budget/findings.md) — the
  survey, the six leaks with their code lines, the options and the full source list.
