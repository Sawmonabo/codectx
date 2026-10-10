# How many lexical matches the context planner reads: the seed budget rule

Research of 2026-10-09 behind [ADR-0016](../../../adr/ADR-0016-seed-budget.md). Code read in the repository at
commit e9c8799 (the code under `internal/` is unchanged on the branch these documents land on); paths are
repository paths. An earlier, untracked survey of how agent tools read search results is cited as "the earlier
survey"; claims taken from it are marked "via the earlier survey, not re-read". Every other outside source was
fetched during this research.

The proposal examined here had four steps, called below step 1 (carry each lexical hit's score and tier onto its
seed), step 2 (root-weighted boosts), step 3 (stop reading at a score level once the matches read fill more than the
file budget) and step 4 (a second, kept-file read).

---

## 1. The question, in plain words

`context_plan` turns a task into a packed reading list. One of its seed steps runs the task text through lexical
(BM25F) search and turns every match into a seed, and every seed that names a node into a graph-walk root. The code
reads every page of matches to the end (`internal/context/seeds.go:312-374`; the reason is in the comment at
`seeds.go:317-323`: reading one page made the plan depend on where a page boundary fell).

The search index serves exact top-k: page n is the top 200·n ([ADR-0013](../../../adr/ADR-0013-search-index.md)). Paging to
the end now costs the sum of top-200, top-400, ... top-200m, and every match still becomes a walk root.

**Example.** Task: "fix the login timeout". The OR query matches every document that contains "the", which is
200,000+ documents on kubernetes (via the earlier survey). The plan's file budget is 200 files (`docs/configuration.md:364`).
The proposal is to read matches best-first and stop once the matches read already fill more than 200 distinct files.
The match in position 150,000 is never read.

**The question has two parts.** Is that stop *exact*, meaning the plan is the same as if every match had been read? Is the
second, kept-file read (search again, restricted to the files the plan keeps) the right way to close the gap it
leaves? If the answer is no, what should replace it?

## 2. What the code does

- The lexical loop pages to exhaustion (`seeds.go:324-374`). Each hit becomes a `recommended` candidate. The hit's
  `ScoreMicros` and tier are dropped (`seeds.go:348-363`).
- A candidate's score is `max(originContribution, best route) + boosts` (`stream.go:915-919`, `stream.go:975`).
  A lexical origin contributes 0 (`rank.go:221-231`). A route's product starts at 1.0 whatever its root
  (`rank.go:181`).
- The boosts are: task identifier 200,000, only for origins the task named (`rank.go:234-241`); active change 120,000;
  associated test or contract 100,000, if **any** admitted route of the entry used a tests, implements, overrides or
  extends edge (`rank.go:297-305`); centrality 5,000 per distinct edge **this compile walked in the entry's package**,
  capped at 50,000 (`rank.go:26-34`, `rank.go:264-270`; the centrality pass counts the edges walk-wide, `rankjoin.go:527-541`).
  Constants: `compiler.go:1094-1103`.
- A walk entry's requirement comes from node kind and depth only (`scope.go:79-120`). A function one hop from any
  root is `required_symbol` (`scope.go:107-110`, `budget.go:100`).
- The packer works per file. Groups come in rank order. Required groups always go in. A non-required group is dropped
  once `files >= MaxFiles` (`slice.go:78`), skipped if it is bigger than one slice (`slice.go:80`), and everything
  after the slice limit is dropped (`slice.go:93`). A group's size is the file's encoded source, charged once, plus
  **each entry's measured metadata** (reasons, evidence paths; `budget.go:110-160`, summed at `stream.go:491`).
  Evidence paths per entry are unlimited by default (`docs/configuration.md:369`).
- The search path filter is exact for files: a hit is kept when its path equals a listed path or lies under it
  (`internal/search/search.go:897-960`). There are 64 filter values per request (`internal/model/validate.go:48`).
  Wire pages hold 200 items (`validate.go:46`).

## 3. What the field does

### 3.1 Code assistants: how much to read

| System | Rule for how many results it reads | Completeness promise | Source |
|---|---|---|---|
| Aider repo map | Binary search over the number of PageRank-ranked tags until the map is within 15% of `max_map_tokens` (`ok_err = 0.15`). Seeds are personalization weights (`100/len(fnames)`, mentioned identifiers ×10, private ×0.1, defined in more than 5 places ×0.1) | none: fills a budget | `aider/repomap.py` (read) |
| GitHub Copilot (VS Code) | `maxResults = Math.floor(sizing.tokenBudget / MAX_CHUNK_SIZE_TOKENS)`, then keeps chunks scoring at least `topScore * 0.65` | none: budget plus a relative floor | `workspaceChunkSearchService.ts` (read) |
| Continue | `nFinal = min(25, contextLength / 512 / 2)`, `nRetrieve = 2·nFinal` with a reranker. No score threshold in `retrieval.ts` | none: a count scaled to the window | `core/context/retrieval/retrieval.ts` (read) |
| Sourcegraph Cody | "We only want to pick items above a certain threshold ... e.g. a relevance score or a token budget". Retrieval aims at recall ("retrieving all relevant items"), ranking at precision | none stated | Hartman et al. 2024 (read); fixed counts 12/15 code results via the earlier survey, not re-read |
| SWE-bench BM25 baseline | "retrieve as many files as fits within the specified limit" (13k/27k/50k tokens). Oracle recall Avg 29.58 / 44.41 / 51.06, yet "models are simply ineffective at localizing problematic code" | none: budget fill | Jimenez et al., ICLR 2024 (read) |
| Agentless, LocAgent, SWE-agent, AutoCodeRover, Moatless | Fixed counts (3+3 files; BM25 top 10, 5 shown; refuse above 50/100 results; 3 full results; `max_hits=10`) or the agent decides | none | via SEED §2.2, not re-read |
| Cursor, Windsurf, Claude Code | Agent-driven search with per-call output caps; Cursor's results-per-search setting was removed | none | via the earlier survey, not re-read |

**Finding.** Shipping tools bound what they read in one of three ways: a budget fill (Aider, Copilot, SWE-bench,
Cody), a fixed or window-scaled count (Continue, Cody, LocAgent), or a score floor (Copilot 0.65). None of them
claims that the bounded read equals the full read. The proposed codectx rule is a budget fill, like the most common
pattern. Its claim to be *exact* goes beyond anything these tools promise. That claim comes from IR, below.

### 3.2 IR: stopping a ranked read early without changing the answer

- **Safe early termination.** Ding and Suel define it as returning "exactly the same results as in the naive
  baseline, i.e., the same set of documents in the same order with the same scores" (SIGIR 2011, §2.3; read).
  WAND keeps a threshold: "a total document score of at least 6.8 (the threshold) is needed in order to make it
  into the current top-k ... we use the lowest score in the heap" (§2.6; read). Block-max stores "the maximum impact
  score for each block" (abstract; read).
- **Elasticsearch block-max WAND.** "this optimization gives accurate top hits". Total hit counts become a lower
  bound past `track_total_hits`, and aggregations turn the optimization off (Grand, 2019; read).
- **Fagin's Threshold Algorithm (TA)** is the closest formal match to the proposed rule. "Define the threshold value
  τ to be t(x1, ..., xm). As soon as at least k objects have been seen whose grade is at least equal to τ, then
  halt." Theorem 4.1: "If the aggregation function t is monotone, then TA correctly finds the top k answers."
  Intuitively: "Halt as soon as you know you have seen the top k answers." (Fagin, Lotem and Naor, JCSS 2003, §4;
  read.) TA's step 1 does *random access*: once an object is seen, it fetches that object's grades from every other
  list.

**What this means for codectx.** The proposed stop is TA run over one sorted list (lexical matches in score order).
The objects are files, k is the file budget, and an object's grade is its best entry's final rank key. TA is exact
only if two things hold. First, τ must be a true upper bound on the grade of anything not yet seen. Second, each
object's grade must depend only on that object's own attributes (monotone aggregation). Section 4 checks both
against the code.

### 3.3 Is the kept-file second read standard?

Yes. It is the second pass of grouped retrieval, which is TA's "random access" applied per group:

- Lucene grouping: "the first pass (FirstPassGroupingCollector) gathers the top groups ... the second pass
  (SecondPassGroupingCollector) gathers documents within those groups". A `CachingCollector` can replay the hits so
  "you only run the query once, but you pay a RAM cost" (Lucene grouping package docs; read).
- Elasticsearch `collapse` keeps the top document per key and expands each group with `inner_hits`, "sending an
  additional query for each inner_hit request for each collapsed hit". The total hit count is the uncollapsed count
  (Elasticsearch collapse docs; read).

In codectx this is `SearchRequest.Paths` (`internal/model/query.go:252`) with exact file matching
(`search.go:897-960`): ceil(F/64) = 4 requests at the default F = 200.

### 3.4 Adaptive cutoffs (score floors, gaps, learned truncation)

- **Largest gap.** Adaptive-k cuts "where the similarity drop is the largest". It searches only the top 90% and adds
  B = 5 documents. Context recall is "approximately 70" (75.74 / 68.54 / 66.16 / 67.43), while a fixed 50k budget gets
  86.79 / 87.34 / 86.88 / 76.90 (Taguchi et al., EMNLP 2025, Table 2; read).
- **Learned ranked-list truncation.** "supervised RLT methods do not show a clear advantage over using a fixed
  re-ranking depth". With stronger retrievers, "Fixed-k (20) consistently achieves the best effectiveness/efficiency
  trade-off" (Meng et al., SIGIR 2024; read).
- **Score-distribution truncation** (Arampatzis et al. 2009) and **Choppy** (2020) need fitted or trained models
  (via the earlier survey, not re-read).

**Finding.** Gap and floor cuts carry constants and lose recall. Learned cuts do not beat a fixed depth. None of them
is exact. The only exact family is threshold-based early termination (3.2), and that is what the proposal uses.

## 4. Is the proposed rule exact? Not as written. Six leaks, each with a fix

The proposal: stop at score level S once the admitted lexical seeds at or above S (ties included) cover more fitting
files than the budget; apply fix (b) so that "S is exact"; add the kept-file read; mark scope complete
(steps 2 to 4). Here it is checked against TA.

**Leak 1: S alone is not τ. The boost margin is needed even with fix (b).** A seed that was read has final score
≥ its origin contribution c (boosts are ≥ 0, `stream.go:915-919`). Anything derived from an unread match m can
score up to w(m)·(route ≤ 0.94) plus its boosts. The task-identifier boost never applies to lexical or walk origins
(`rank.go:234-241`), so the boost bound is 270,000. Without (b) the bound is c(m) + 270,000. When the
(F+1)-th file's c is below 270,000, the stop never fires and the read goes to the end. With (b), where boosts scale
by root weight, the bound is c(m)·1.27. So the condition must be TA's: **stop when F+1 distinct witness files each
have c ≥ τ(next), where τ(next) is the largest final score anything derived from the next unread match could get.**
With step 1's c proportional to the BM25F score, that means reading every hit whose score is ≥ s₍F+1₎ / 1.27, ties
included. Fix (b) is what makes the stop fire at all; it does not make S exact by itself. τ must be computed with the
same integer functions (`boostsForCounts` with every boost on, `mulScaled`), not with a float ratio.

**Leak 2: centrality depends on the whole walk, so unread roots change the scores of kept entries.** The centrality pass counts
distinct edges walked in each package (`rankjoin.go:527-541`). An unread root's walk adds edges to a package and can
raise the centrality boost of an *above-τ* entry in that package by up to 50,000 (`rank.go:264-270`). That reorders
entries near the file-budget boundary. This breaks TA's assumption that an object's grade depends only on its own
attributes, and no margin repairs it. **Fix: count only edges walked from named roots (explicit or exact) toward
centrality.** That fits step 2's "a weak root lends only weak scores". The earlier survey already offers a separate named walk
as one way to build step 2.

**Leak 3: the associated-test boost is combined across routes.** `associatedTestOrContract` fires if **any** admitted
route used a tests/implements/overrides/extends edge (`rank.go:297-305`). An unread root's route can therefore add
100,000 to an above-τ entry. **Fix (this is what fix (b) has to mean):** each value is computed per root, as
w(root)·(route product + that route's own route-dependent boosts). The entry takes the **max** over its origin and
its routes, and root-independent boosts (active change) are scaled by the weight of the route that wins. With a max,
a route scoring below τ can never change an entry already at or above τ. That is TA's monotone aggregation.

**Leak 4: "fits one slice" is decided when packing, on the whole group.** A group's size includes every entry's
metadata (`budget.go:110-160`, `stream.go:491`), and evidence paths are unlimited by default
(`docs/configuration.md:369`). A witness file that fits when it is counted can be skipped as oversized at pack time
(`slice.go:80`). The packer then moves on to a later group, which may come from an unread match. The file's source
size alone is not an upper bound on the group, because metadata has no default bound. **Fix: count a file as a
witness only if its encoded source fits one slice. After packing, check the certificate: the packer stopped admitting
(file limit or slice limit) at a group whose rank key is above τ. If it did not, raise the witness count by the
number of witness groups that were skipped, read down to the new floor, and pack again.** This is deterministic and
does not depend on pages. It answers the page-boundary objection at `seeds.go:317-323`.

**Leak 5: the kept-file read covers lexical matches inside kept files, not walk entries that only an unread match
outside them reaches.** Packing is per file, so the full read packs every entry of a kept file, including a method
reachable only from an unread match in another file. Direction is "both" and depth is unlimited by default, so that
method is missing only when its connected component, over `scopeRelations`, holds no root that was read. This
happens in practice: a class's methods hang off the class by `contains`, which is not walked (`scope.go:27-40`;
`internal/provider/treesitter/facts.go:391-396`). **Fix, a closure step: (i) the kept-file read (step 4); (ii) list the
nodes of kept files that the walk did not reach, walk their components, and make every lexical match in those
components (search restricted to those components' files, kept only when the hit's node was reached) a root.**
Those components are disjoint from the main walk by construction, so this pass explores only them. Its cost has not
been measured (inference). After the closure, every entry of a kept file is present.

**Leak 6: the rank key is a pair, and a max over the score alone is not enough.** `lessRank` compares requirement
first and score second (`stream.go:315-333`). In one walk, an entry's depth is its distance to the nearest root of any
class (`boundaryRequirement`, `scope.go:79-120`), and its score is its best route; the two are computed separately.
Example: entry E is 3 hops from a named root, which gives (`optional`, about 350,000), and 1 hop from an unread
lexical match m. Under step 2's cap the full read gives E `recommended` from m, but its score stays 350,000 from the named
route. If τ(next) is below 350,000, E rises above the witnesses in the full read only. **Fix: each root produces a
(requirement, score) pair for the entry, and the entry takes the lexicographic max of those pairs.** Then m gives
(`recommended`, ≤ τ), which cannot lift E above the witnesses. Throughout this document, "above τ" means a rank key
greater than the pair (`recommended`, τ), not a bare score.

**Pass order with the closure.** The closure needs the kept set, and the kept set needs a pack. The loop is: pack,
closure, walk from the new roots, pack again, check the certificate. The closure adds entries only to kept files, and
all of their keys are ≤ (`recommended`, τ), so it cannot reorder the head above τ. It can push a witness past a
slice, and the certificate catches that. The order above τ is fixed and drops are permanent, so the loop terminates.

**What stays inexact even after all six fixes, so the notice must say it.** For entries whose score is below τ,
inside packed files, scores, order and explanation routes come from the matches that were read. A route from an
unread root is missing from their evidence paths. The full read would also add every unread root's routes to the
evidence of *above-τ* entries. With evidence unlimited, those bytes alone can change what fits when the byte or slice
budget binds. A plan that is byte-identical to the full read is therefore impossible without the full read. The claim
can be exact for: which files and entries rank above τ, with their scores and order; every entry present in each
packed file; and the packed file set whenever the file budget or the certificate decides it.

## 5. Options

| | What it does | Completeness | Cost | Risk |
|---|---|---|---|---|
| **(a) Read every page** | The current loop; every match is a seed and every node-hit a root | Exact by definition | Σ top-200n over every page under top-k. On kubernetes, 200,000+ roots into one walk (via the earlier survey). The 250,000 start-node bound is no longer safe (`validate.go:50-64`) | Explanations grow with root count (unlimited evidence paths) and can push groups past a slice. Repo-scale walk on every broad task |
| **(b) Fixed k** (Cody, Continue, LocAgent) | Read the top k matches | Not exact. The answer depends on k, not on the caller's budget | Cheapest | Product rule: no count that is not the caller's. Meng 2024 shows fixed depth competes with learned cuts, but no k is published that holds across repositories |
| **(c) Proposal as written** (steps 1 to 4 as first written) | Stop at S with ties, root-weighted boosts, kept-file read, scope marked complete | **Not exact**: leaks 1-6 (§4). Step 4 closes only part of leak 5 | Reads about F+1 files' worth of matches, plus ceil(F/64) filtered reads | "Scope complete" overstates: centrality, the associated boost and the split requirement-versus-score max can reorder files at the boundary, and walk-only entries from unread matches drop silently |
| **(c′) TA-exact budget stop** (recommended) | (c) with the six fixes: stop on τ(next) with F+1 witnesses; per-root (requirement, score) pairs, lexicographic max; centrality from named roots only; witness fit by source size plus a post-pack certificate; closure (kept-file read plus walk of unreached kept-file nodes) | Exact for the ranked head above τ, the entry set of every packed file, and the packed file set when the file budget or certificate decides. Residual stated in the notice: routes from unread roots are not in explanations, and order below τ comes from the matches read | Reads to s₍F+1₎/1.27 (more than (c)); 4 filtered reads at F = 200; one walk over components not reached; rare repacks on a failed certificate | Most code. Centrality becomes a named-root signal (a policy change, behind a policy-version bump). Cost of the closure walk not measured |
| **(d) Score floor or gap** (Copilot 0.65, Adaptive-k) | Stop at a fixed ratio of the top score or at the largest gap | Not exact. Adaptive-k recall about 70 vs 87 for a large fixed budget | Cheap | Carries builder-chosen constants (0.65; B = 5, 90%). Ignores the caller's budget |
| **(e) Lexical matches as leaves, not roots** | Read with (c′)'s stop, but never walk from lexical matches | Trivially exact (an unread match adds only itself; the kept-file read finds all of them) | Cheapest exact option | Loses the only graph expansion a plain-English task gets. LocAgent's no-traversal ablation lost 5.5 points of function Acc@10 (66.06 vs 71.53, via the earlier survey, not re-read) |

## 6. Recommendation: (c′), the TA-exact budget stop

1. **It is the one rule in the field that is both budget-shaped and provably exact.** Tools fill a budget (3.1) but
   promise nothing. IR's threshold algorithms (3.2) give the promise. Combining them answers the real question
   (stop early *and* stay complete) and the page-boundary objection at `seeds.go:317-323`, because the stop is a
   score threshold, not a page edge.
2. **The kept-file read is standard.** It is the second pass of grouped retrieval (Lucene, Elasticsearch collapse
   with inner hits) and TA's random-access step. On its own it does not close leak 5, so add the closure walk.
3. **(c) as written should not ship with "scope not marked incomplete".** Leaks 2, 3 and 6 can change which files fill
   the last slots, and leak 5 drops entries without a trace. Each fix is small and needs no new constant: 1.27 comes
   from the existing §15.3 boost constants, not from tuning.
4. **(a) is the full read the code does now, and it is not a good reference either.** With unlimited evidence
   paths, 200,000 roots make the full read's own explanations big enough to change packing. Option (e) is the
   fallback if the smallest exact change is wanted, accepting the loss of expansion from lexical hits.

**Confidence.** High that (c) as written is not exact: every leak is traced to a specific line above. High that the
kept-file read is standard practice. Medium-high that (c′) is exact in the scope stated in §4. The argument is TA's
Theorem 4.1 applied to the corrected score, and the differential test below (cut vs full read on fixtures) is what
would make it self-verified. Low confidence in the cost of the closure walk; it has not been measured.

**Cost of the read itself.** Paging 200 at a time means page n costs the top
200·n, so reading to the floor costs about m²/2 page-equivalents. An internal ranked read whose depth doubles (top
400, top 800, ...) until the k-th hit is below the floor costs at most about twice the depth it needs (geometric
sum; inference). With block-max, the floor can serve as the starting threshold. The 200-item limit is a wire bound
(`validate.go:46`) and does not apply to an in-process call. The internal search call therefore takes a depth or minimum-score parameter.

## 7. What the design changes

- **Step 1** unchanged (carry `ScoreMicros` and tier; c proportional to the score relative to the top, below
  `exactContribution`).
- **Step 2** reworded. Each root gives the entry a (requirement, score) pair. The requirement is the node kind and
  depth from *that* root, capped by the root's class. The score is w(root)·(route product + that route's
  route-dependent boosts). The entry takes the **lexicographic max** of the pairs and of its origin pair, not a min
  depth plus a max score taken separately (leak 6), and never a sum across routes (fixes `rank.go:297-305` combining
  across routes). Root-independent boosts are scaled by the winning pair's root weight. **Centrality counts only
  edges walked from named roots** (`rankjoin.go:527-541`, `rank.go:264-270`). The compiler's policy version changes.
  This is a ranking change, not only a correctness fix: a task with no explicit or exact seeds gets zero centrality
  everywhere.
- **Step 3** reworded: "Read lexical matches in score order. Stop after the last hit scoring ≥ the floor, where the
  floor is the lowest score at which F+1 distinct witness files have c ≥ τ(next). τ is computed with the compiler's
  own integer functions. A witness is a file whose encoded source fits one slice. Ties at the floor are read across
  page boundaries. After packing, check the certificate (the packer stopped admitting above τ). If it fails, raise
  the witness count by the skipped witnesses and read on."
- **Step 4** widened to a closure: (i) the kept-file lexical read (`SearchRequest.Paths`, batched by 64); (ii) a walk
  from kept-file nodes the first walk did not reach, with the lexical matches in those components added as roots.
- **Notice** (a stated reason in place of an unexplained complete scope): "Lexical matches scoring below X were not
  read. N matches above it fill more than the plan's file budget of F, so no unread match can rank above the packed
  files. Every match and walk entry inside the packed files is included. Explanations list routes from the N matches
  read." Scope is complete. A failed certificate that cannot be resolved, or a resource-limit error, still marks it
  incomplete (`seeds.go:337-345`).
- **Tests:**
  - an entity in a kept file reachable only from an unread match *outside* the kept files is still packed
    (mutation: skip the closure walk);
  - an unread match's walk adds edges in a kept entry's package, and the plan is unchanged (mutation: centrality
    from all roots);
  - a witness file that becomes oversized at pack time triggers a read past the first floor (mutation: count
    witnesses at seed time only);
  - an entry 3 hops from a named seed and 1 hop from an unread lexical match keeps the same rank key as in the full
    read (mutation: requirement from the min depth over all roots, score from the max route, taken separately);
  - differential: on the fixtures, the cut plan's ranked head above τ, the packed file set and the entry set of each
    packed file equal the full read's (mutation: stop at S without the margin). This is the counterpart of the
    search index's 2,436/2,436 top-k parity fixture;
  - caveat on the property "raising the budget never removes a function": it can fail when the byte budget binds,
    because more roots add evidence bytes. The property is stated for file-budget-bound plans; bounding stored
    evidence is a separate question.
- **The internal search call** gains a depth or minimum-score parameter, built in the search index and called by the
  context compiler.
- **Unchanged:** plain words are not sent to exact resolution; the configuration documentation; name-only
  candidates; the hierarchy relations in scope.

## 8. Follow-ups found (outside this question)

- Unlimited evidence paths by default (`docs/configuration.md:369`) make group sizes, and so packing, depend on how
  many routes reach an entry. Any rule that adds roots can push a file out of a byte-bound plan. A packer that charges
  explanation bytes separately from source bytes would remove the coupling.
- The earlier survey's statement "With either fix, no file the plan would have kept is lost" does not hold under
  leaks 2, 3, 4 and 6, and its "does not change which files and entries are packed" does not hold under leak 5. That
  text is not carried into the design record.

## Sources

Read during this research:
- Ding, S. and Suel, T. "Faster Top-k Document Retrieval Using Block-Max Indexes." SIGIR 2011.
  https://research.engineering.nyu.edu/~suel/papers/bmw.pdf
- Fagin, R., Lotem, A. and Naor, M. "Optimal Aggregation Algorithms for Middleware." JCSS 2003 (PODS 2001).
  https://arxiv.org/abs/cs/0204046
- Grand, A. "Faster retrieval of top hits in Elasticsearch with block-max WAND." Elastic blog, Feb 2019.
  https://www.elastic.co/blog/faster-retrieval-of-top-hits-in-elasticsearch-with-block-max-wand
- Apache Lucene, grouping package documentation (9.11).
  https://lucene.apache.org/core/9_11_0/grouping/org/apache/lucene/search/grouping/package-summary.html
- Elasticsearch, "Collapse search results."
  https://www.elastic.co/docs/reference/elasticsearch/rest-apis/collapse-search-results
- Aider, `aider/repomap.py`. https://raw.githubusercontent.com/Aider-AI/aider/main/aider/repomap.py
- Microsoft, vscode-copilot-chat `workspaceChunkSearchService.ts`.
  https://raw.githubusercontent.com/microsoft/vscode-copilot-chat/main/src/platform/workspaceChunkSearch/node/workspaceChunkSearchService.ts
- Continue, `core/context/retrieval/retrieval.ts`.
  https://raw.githubusercontent.com/continuedev/continue/main/core/context/retrieval/retrieval.ts
- Hartman, J. et al. "AI-assisted Coding with Cody: Lessons from Context Retrieval and Evaluation for Code
  Recommendations." 2024. https://arxiv.org/html/2408.05344v1
- Jimenez, C. E. et al. "SWE-bench: Can Language Models Resolve Real-World GitHub Issues?" ICLR 2024.
  https://arxiv.org/html/2310.06770v3
- Taguchi, C., Maekawa, S. and Bhutani, N. "Efficient Context Selection for Long-Context QA: No Tuning, No
  Iteration, Just Adaptive-k." EMNLP 2025. https://arxiv.org/html/2506.08479v3
- Meng, C. et al. "Ranked List Truncation for Large Language Model-based Re-Ranking." SIGIR 2024.
  https://arxiv.org/html/2404.18185v1

Cited through the earlier survey, not re-read here: Agentless
(arXiv 2407.01489), LocAgent (arXiv 2503.09089), SWE-agent (arXiv 2405.15793), AutoCodeRover (arXiv 2404.05427),
Moatless Tools (github.com/aorwall/moatless-tools), Sourcegraph Cody source counts, Cursor, Windsurf, Claude Code,
Arampatzis et al. SIGIR 2009, Choppy (arXiv 2004.13012).

Bibliographic details only (search result, not read): Broder, A. et al. "Efficient query evaluation using a
two-level retrieval process." CIKM 2003 (WAND).
https://research.ibm.com/publications/efficient-query-evaluation-using-a-two-level-retrieval-process
