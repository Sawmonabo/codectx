# The seed budget: how many lexical matches a context planner must read to stay exact

Research note for the codectx product owner, behind [ADR-0016](../adr/ADR-0016-seed-budget.md). Research date
2026-10-09. The full findings, the options table and every source are in
[raw/seed-budget/findings.md](raw/seed-budget/findings.md); sources are cited here as [n] from §5.

## 1. The question

The context planner's lexical seed step reads every page of the task text's matches and walks from every match
(`internal/context/seeds.go:317-374`). Under a top-k search index, page n costs the top 200·n, so the read is
quadratic in the number of pages, and a common word ("the" in "fix the login timeout") makes hundreds of thousands of
walk roots on a large repository, while the plan packs 200 files.

The proposal: read best-first and stop once the matches read fill more than the file budget, then read again inside
the kept files. Two questions: is that stop exact, meaning the plan equals the one built from every match, and is the
kept-file read the right way to close the gap it leaves?

## 2. What the field does

- **Agent code assistants bound the read without promising anything.** The surveyed tools fill a token budget
  (a repository map's binary search over ranked tags; an issue-resolution benchmark's "as many files as fit"), read a
  count scaled to the context window, or keep chunks above a fixed fraction of the top score [1][2][3][4][5]. None
  claims the bounded read equals the full one.
- **Information retrieval has the exact form.** Safe early termination returns "exactly the same results as in the
  naive baseline" [6]; block-max indexes keep a per-block maximum so a document that cannot enter the top k is skipped
  [6][7]. The threshold algorithm for middleware halts "as soon as at least k objects have been seen whose grade is at
  least equal to τ", and is correct when the aggregation function is monotone (Theorem 4.1) [8].
- **The kept-file read is standard.** Grouped retrieval runs a first pass for the top groups and a second for the
  documents inside them [9]; result collapsing expands each kept group with one more query [10]. It is the threshold
  algorithm's random-access step applied per group.
- **Adaptive cut-offs are not exact and carry constants.** Cutting at the largest score gap reaches a context recall of
  about 70 where a large fixed budget reaches about 87 [11]; learned truncation shows no clear advantage over a fixed
  depth [12].

## 3. Findings

The proposed stop is the threshold algorithm over one sorted list: files are the objects, the file budget is k, and a
file's grade is its best entry's rank key. It is exact only when the threshold bounds everything unseen and each grade
depends only on its own object. Checked against the code, six leaks break that:

1. **The stop score is not the threshold.** Boosts let an unread match's descendants outrank the last read match. The
   threshold must include the boost margin, computed with the compiler's integer functions.
2. **Centrality is walk-wide** (`internal/context/rankjoin.go:527-541`), so an unread root's walk raises a packed
   entry's boost. Count only edges walked from named roots.
3. **The associated-test boost combines routes** (`internal/context/rank.go:297-305`). Compute each value per root and
   take the maximum.
4. **A witness file can be skipped when packed,** because its group's metadata is unbounded
   (`internal/context/budget.go:110-160`, `internal/context/slice.go:80`). Count a witness by source size and check a
   certificate after packing.
5. **The kept-file read misses walk entries** reached only from an unread match outside the kept files, because the
   scope walk does not follow `contains` (`internal/context/scope.go:27-40`). Add a walk over the kept files'
   unreached components.
6. **The rank key is a pair,** requirement then score (`internal/context/stream.go:315-333`), and the two are computed
   separately. Each root must give a (requirement, score) pair, and the entry must take the lexicographic maximum.

With all six closed, the stop is exact for the ranked head above the threshold, for the entries of every packed file,
and for the packed file set when the file budget or the certificate decides it. Routes from unread roots are missing
from explanations, and order below the threshold comes from the matches read; with unbounded explanation paths, a
byte-identical plan needs the full read, so the notice states this residue.

## 4. Options and the finding

| Option | Exact? | Cost |
|---|---|---|
| Read every page | yes | quadratic under top-k; a walk from every match |
| Fixed count of matches | no; depends on the count | cheapest; a constant with no justification across repositories |
| The proposal as written | no; six leaks | about F+1 files' worth of matches plus the kept-file read |
| **The proposal with the six leaks closed** | **yes, in the scope of §3** | reads to the threshold with margin; a few filtered reads; one walk over unreached components |
| Score floor or gap | no; recall about 70 against 87 | cheap; builder-chosen constants |
| Lexical matches as leaves, never roots | trivially | cheapest exact; loses graph expansion for plain-language tasks |

Finding: the threshold stop with the six fixes, a certificate after packing, and the closure step. It is the one rule
that is both shaped by the caller's budget and provably exact. Confidence is high that the proposal as written is not
exact (each leak traces to a line) and medium-high that the corrected rule is exact in the stated scope; the
differential test, stopped plan against full read on the fixtures, is what verifies it. The closure walk's cost was not
measured.

## 5. Sources

1. *aider/repomap.py*, https://raw.githubusercontent.com/Aider-AI/aider/main/aider/repomap.py
2. *workspaceChunkSearchService.ts*, https://raw.githubusercontent.com/microsoft/vscode-copilot-chat/main/src/platform/workspaceChunkSearch/node/workspaceChunkSearchService.ts
3. *core/context/retrieval/retrieval.ts*, https://raw.githubusercontent.com/continuedev/continue/main/core/context/retrieval/retrieval.ts
4. Hartman et al., *AI-assisted Coding with Cody: Lessons from Context Retrieval and Evaluation for Code
   Recommendations*, 2024, https://arxiv.org/html/2408.05344v1
5. Jimenez et al., *SWE-bench: Can Language Models Resolve Real-World GitHub Issues?*, ICLR 2024,
   https://arxiv.org/html/2310.06770v3
6. Ding, Suel, *Faster Top-k Document Retrieval Using Block-Max Indexes*, SIGIR 2011,
   https://research.engineering.nyu.edu/~suel/papers/bmw.pdf
7. Grand, *Faster retrieval of top hits in Elasticsearch with block-max WAND*, 2019,
   https://www.elastic.co/blog/faster-retrieval-of-top-hits-in-elasticsearch-with-block-max-wand
8. Fagin, Lotem, Naor, *Optimal Aggregation Algorithms for Middleware*, JCSS 2003, https://arxiv.org/abs/cs/0204046
9. *Lucene grouping package*, https://lucene.apache.org/core/9_11_0/grouping/org/apache/lucene/search/grouping/package-summary.html
10. *Collapse search results*, https://www.elastic.co/docs/reference/elasticsearch/rest-apis/collapse-search-results
11. Taguchi, Maekawa, Bhutani, *Efficient Context Selection for Long-Context QA: No Tuning, No Iteration, Just
    Adaptive-k*, EMNLP 2025, https://arxiv.org/html/2506.08479v3
12. Meng et al., *Ranked List Truncation for Large Language Model-based Re-Ranking*, SIGIR 2024,
    https://arxiv.org/html/2404.18185v1
