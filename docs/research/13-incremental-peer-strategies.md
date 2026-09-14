# How serious code-intelligence systems stay incremental — and what codectx should copy

Date 2026-09-13. Scope: how peers make **cross-file semantic facts** (references, call
graph, control/data dependence) incremental for **(a) a local uncommitted edit** and
**(b) a new commit or branch switch**, instead of rescanning. Then a section-by-section
verdict against `docs/implementation-plan.md` and a ranked list of plan changes.

This report deliberately does **not** re-survey what these systems are — reports
[01](01-ai-ide-indexers.md), [02](02-code-graph-tools.md) and
[03](03-industrial-precise-indexers.md) already do that, and are cited rather than repeated.
The only subject here is **what happens when the code changes.**

Every external claim is marked **SOURCED** (a page was fetched and the wording read) or
**INFERRED** (my reasoning from sourced facts). Where nothing public exists, it says
"no public statement found" rather than guessing. No numeric confidence scores appear
anywhere, per the standing ruling in [00-synthesis §2 #5](00-synthesis.md).

---

## 0. The one-paragraph answer

Nobody makes a compiler-precise cross-file analysis incremental *inside the analyzer*
unless the analysis was designed to decompose — and only two systems in this survey were:
Infer (per-procedure summaries) and Glean (per-fact ownership). Everyone else gets
incrementality from **four mechanisms that live outside the analyzer**: content-addressed
keys over the input closure, early cutoff at a deliberately-coarsened intermediate value,
serving a knowingly-stale result with a labelled distance, and a two-tier product where a
cheap always-fresh tier covers for the expensive tier while it catches up. codectx's plan
already has the first (partly) and the fourth; it is missing the second and third. For a
local edit the highest-value missing mechanism is **early cutoff on a declaration-only
signature digest**; for a branch switch it is **unit retention decoupled from generation
retention**. Neither requires SCIP indexers or the dependence engine to cooperate, which
matters because — confirmed below — they will not.

---

## 1. The two scenarios, stated concretely

Both are priced against codectx's own measurements
([09](09-scaling-governance-empirical.md), [10](10-round3-empirical.md)), so the
recommendations have real numbers attached rather than adjectives.

| | Scenario (a): one local edit | Scenario (b): commit or branch switch |
|---|---|---|
| **The change** | One function body edited in one `.go` file inside a 239k-line Go module (1,283 files) | `git switch` rewrites 5,000 files; the target branch was indexed an hour ago |
| **Tree-sitter tier** | 1 file re-parsed. Negligible. | ~5,000 files re-parsed at tier-2 throughput (~10⁵–10⁶ LOC/s, [03 §10](03-industrial-precise-indexers.md)) — seconds |
| **SCIP tier today** | Whole unit re-runs: `scip-go` on x/tools = **7.3 s / 0.4 GB / 21 MB index** ([09](09-scaling-governance-empirical.md)) | Same, for every affected project root |
| **Dependence tier today** | Whole unit re-runs: parse **23.2 s** @500 MB cap + export **4.5 s** = **~28 s / ~1.0 GB tree** ([10 §1](10-round3-empirical.md)) | Same, for every affected unit |
| **Worst realistic case** | Python 506k-line unit: parse 37 s (2 GB cap) + export 27.5 s, 1.27 GB CSV ([09](09-scaling-governance-empirical.md)) | Several such units at once, serialized by `max_concurrent_heavy_analyzers = 1` |

The asymmetry is the whole problem: in scenario (a) **one changed byte costs the same as a
cold index of that unit**, and in scenario (b) the work may have been done before and
thrown away.

---

## 2. The five mechanisms peers actually use

Everything in sections 3–5 is an instance of one of these. I use these labels throughout.

| # | Mechanism | Plain-language statement | Canonical instance |
|---|---|---|---|
| **M1** | **Content-addressed input closure** | The cache key is a hash of everything the job reads, and contains no notion of "current" or "latest" — so history can revisit a state and hit | Bazel action digest; git blob SHA |
| **M2** | **Per-file artifacts, cross-file join at query time** | Never store a cross-file edge; store per-file fragments that can be stitched on demand | stack-graphs partial paths; clangd `.idx` shards |
| **M3** | **Early cutoff at a coarsened intermediate** | Recompute the cheap thing, compare it to last time, and if it is equal stop the invalidation there | salsa backdating over rust-analyzer's `ItemTree`; Bazel Skyframe change pruning |
| **M4** | **Overlay / stacking with ownership** | Keep the big old result, hide the parts the change invalidated, stack a small new result on top | Glean stacked DBs; CodeQL overlay databases |
| **M5** | **Serve stale, labelled, with a distance** | Answer from an index built at a different revision, say how far away it was, and repair coordinates if you can | Sourcegraph nearest-commit |

A sixth, **decomposition into summaries** (Infer), is the only thing that makes a
*whole-program dataflow* analysis genuinely incremental — and it is a property the analyzer
must be built with, not one you can add from outside.

---

## 3. Per-system change-handling table

| System | Unit of incrementality | Single local edit | New commit / branch switch | Mechanisms | Needs the analyzer's cooperation? |
|---|---|---|---|---|---|
| **GitHub tree-sitter tags** | git **blob** | re-parse 1 blob | cost = number of *previously unseen blobs*, not files in the commit | M1 | No |
| **GitHub stack-graphs** | file (partial paths) | re-parse 1 file; **zero** cross-file index work; stitching redone per query | same, per unseen blob | M1 + M2 | No (but per-language rule sets) |
| **Kythe** | compilation unit (`.kzip`) | that TU + every TU including the changed header | serving table **rebuilt wholesale** | M1 (extraction only) | Yes (build integration) |
| **Glean** | ownership unit (usually a filename) | reindex changed files → stack a new DB hiding those units | branches = sibling stacks over one immutable base, all queryable at once | M4 | Yes (indexer must emit ownership) |
| **Infer** | **procedure summary** | structural-identity marks changed procs; on-demand engine pulls cached callee summaries | same, seeded from `--changed-files-index` | summaries + M1 | Yes (compositional by construction) |
| **Sourcegraph precise** | `(repo, commit, root, indexer)` upload | **not handled** — uploads are keyed to a 40-char commit hash | whole index re-run per root; storage dedups per SCIP `Document`; queries fall back to nearest indexed ancestor | M1 (storage) + M5 | Partly (format cooperates; indexers do not) |
| **Sourcegraph search tier** | commit | n/a | copy previous ctags index, re-run ctags on changed files: **~20 s vs ~20 min cold** on 40M LOC | M1 | No |
| **CodeQL** | database (per extraction) | overlay DB over a cached base DB — **`build-mode: none` only** | base cached in Actions Cache, restored by key prefix | M4 | Yes (GitHub's own extractor) |
| **Semgrep Pro** | file (CE) / whole scan (interfile) | diff-aware scan drops to single-file analysis | interfile diff scanning shipped at ~1/10 cost, then was reverted | — | Yes |
| **Joern** | whole CPG | **nothing** — full `importCode` rebuild | full rebuild | none | Yes, and it does not |
| **rust-analyzer / salsa** | query `(fn, key)` | body edit does not change the `ItemTree`, so name resolution never re-runs | `cargo metadata` reload; nothing persists across restart | M3 + durability | n/a (own frontend) |
| **gopls** | package | v0.12 fine-grained invalidation: unchanged exported API stops the cascade | package-granular blast radius | M3 | n/a (own typechecker) |
| **clangd** | translation unit | re-index that TU | per-file `.idx` staleness check on disk | M1 + M2 | n/a |
| **JetBrains IntelliJ** | file (content-pure by API contract) | re-map that file; re-serialize its stub | branch switch is a **documented reindex trigger**; per-branch indexes are shelved | — | n/a |
| **tsserver** | project (Program in memory) | script version bumps, dirty region re-parsed, Program recreated reusing every other `SourceFile` | every touched file re-acquired; nothing persists | M3-ish (version gate) | n/a |
| **Cursor** | **chunk content** | Merkle walk visits only differing branches; only changed chunks re-embed | tree diff flags every rewritten file, but embeddings are cache hits on the way back | M1 | n/a |
| **Continue.dev** | content hash × `(dir, branch, artifactId)` tag | mtime gate → SHA-256 check → re-embed one file | branch switch is `addTag`/`removeTag` row bookkeeping; only genuinely new content is computed | M1 | n/a |
| **Aider repo map** | file path, validity = **mtime** | re-extract that file's tags; PageRank still global | every checked-out file misses the cache, in **both** directions | — | n/a |
| **Cline** | none — reads files on demand | always fresh | always fresh | — | n/a |

---

## 4. What each mechanism actually buys, with the evidence

### 4.1 M1 — content-addressed input closure

The single cheapest incrementality that exists, and the one with the most independent
rediscoveries.

- Bazel names an action by the digest of its serialized `Action` message, covering the
  argv, the lexicographically-sorted environment, the Merkle root over the whole input
  tree, and the platform. Two maps then do the work: action digest → result metadata, and
  a content-addressable store for output bytes.
  ([bazel.build/remote/caching](https://bazel.build/remote/caching),
  [remote_execution.proto](https://github.com/bazelbuild/remote-apis/blob/main/build/bazel/remote/execution/v2/remote_execution.proto)) — **SOURCED**
- The consequence Bazel documents explicitly is the branch-switch one: the disk cache
  exists for "sharing build artifacts when switching branches and/or working on multiple
  workspaces of the same project."
  ([bazel.build/remote/caching](https://bazel.build/remote/caching)) — **SOURCED**
- The hazard is documented too, and it is the one that bites: anything that leaks into the
  key without being a semantic input destroys hit rates — Bazel's own example is that
  "environments with different `$PATH` variables won't share cache hits."
  ([bazel.build/remote/caching](https://bazel.build/remote/caching)) — **SOURCED**
- GitHub keys parsed tags by git blob SHA-1, so "once a blob is parsed, it never needs to be
  parsed again and can be shared across any commit that includes that blob"
  ([CACM, via 03 §6](03-industrial-precise-indexers.md)) — **SOURCED**
- stack-graphs leans on the same property from the other end: the blob id "is available
  before analysis starts, [so] we can skip the storage and computation costs of redundant
  file-incremental work," and each file's subgraph is "stored once, in isolation, and reused
  however many times that file version appears in the project's history."
  ([arXiv:2211.01224](https://arxiv.org/abs/2211.01224)) — **SOURCED**. The open-source CLI
  implements it literally: `let tag = sha1(source)`, and a matching tag skips the file.
  ([cli/index.rs](https://github.com/github/stack-graphs/blob/main/tree-sitter-stack-graphs/src/cli/index.rs)) — **SOURCED**
- Kythe's `.kzip` unit digest is computed over "required inputs, arguments, outputs, source
  files, working directory, environment variables and details entries"
  ([kythe.io/docs/kythe-kzip.html](https://kythe.io/docs/kythe-kzip.html)) — **SOURCED** —
  i.e. exactly an input-closure key. A maintainer's guidance is to key an indexer-output
  cache on that digest plus the compiler and indexer binary versions, which "generally saves
  us around a third to half the work depending on how much churn there is in the corpus."
  ([kythe mailing list](https://groups.google.com/g/kythe/c/RVwJZGB_tHU)) — **SOURCED**
  (mailing list, not official docs)
- Sourcegraph applies it *below* a non-incremental producer: each SCIP `Document` payload is
  hashed and stored in a globally deduplicated table —
  `codeintel_scip_documents.payload_hash` is `UNIQUE`, commented "We use this as a unique
  value to enforce deduplication between indexes with the same document data," with
  `codeintel_scip_document_lookup` mapping `(upload_id, document_path) → document_id`.
  ([schema.codeintel.md](https://raw.githubusercontent.com/sourcegraph/sourcegraph-public-snapshot/main/internal/database/schema.codeintel.md)) — **SOURCED**
  (implementation evidence, not a documented product guarantee).
  **This is the single most transferable idea in the report**: the indexer stays
  whole-project, and the *storage* becomes per-file incremental anyway.
- Continue.dev separates identity from membership: content is identified by
  `sha256(fileContents)` as `cacheKey`, membership is a many-to-many relation to a tag
  `(directory, branch, artifactId)`, and a branch switch resolves to `addTag` row inserts
  for any content already embedded under *any* tag. Change detection is an mtime gate
  followed by an authoritative hash check, with an explicit "file contents did not change"
  branch that handles exactly the `git checkout`-moved-the-mtime case.
  ([refreshIndex.ts](https://raw.githubusercontent.com/continuedev/continue/main/core/indexing/refreshIndex.ts)) — **SOURCED**
- Cursor caches "embeddings by chunk content," so the key is finer than a file and two files
  sharing an unchanged region share cache entries.
  ([cursor.com/blog/secure-codebase-indexing](https://cursor.com/blog/secure-codebase-indexing)) — **SOURCED**
- The counter-example is instructive. Aider's tag cache uses `cache_key = fname` with
  validity `val.get("mtime") == file_mtime`
  ([repomap.py](https://raw.githubusercontent.com/Aider-AI/aider/main/aider/repomap.py)) — **SOURCED**.
  `git checkout` rewrites mtimes, so restoring byte-identical content misses, and because
  there is one entry per path there is no older version to hit on the way back —
  **INFERRED**. An mtime key is actively git-hostile; a content key is git-native for free.

**Nobody publicly keys on the git blob SHA-1 directly**, even though `git ls-tree` hands you
a content hash for every file at any ref without reading the working tree. Continue and
Cursor both read-and-hash instead — **no public statement found** for any tool doing
otherwise.

### 4.2 M2 — per-file artifacts, cross-file join at query time

stack-graphs is the clearest published statement of this idea, and also a cautionary tale.

- The structural invariant that makes it work: "for each source file, we create an isolated
  subgraph without any knowledge of, or visibility into, any other file in the program," and
  "each node and (non-virtual) edge belongs to exactly one file." Cross-file linkage happens
  only through two singleton anchors. — **SOURCED**
- What is persisted is **not** the subgraph: "Instead of saving the subgraph structure to
  persistent storage, we save this list of partial paths." A partial path is a start node,
  an end node, and a pre/postcondition over partial symbol stacks; stitching is unification
  of one path's postcondition with the next's precondition. — **SOURCED**
- The explicit design goal: "the work we do at index time is file-incremental, with **all
  non-file-incremental work happening at query time**." — **SOURCED**
  (all: [arXiv:2211.01224](https://arxiv.org/abs/2211.01224),
  [stack_graphs::partial](https://docs.rs/stack-graphs/latest/stack_graphs/partial/index.html))
- The trade is honest and worth stating: index-time cost is perfectly incremental, and
  query-time stitching is **never cached across queries**. — **SOURCED**
- Resolution is intentionally unsound: the paper permits "ambiguous and missing bindings" to
  stay useful "even in the presence of incorrect programs or an incomplete model of the
  language's semantics." — **SOURCED**. There is **no published limitation list at all** —
  do not write "stack graphs cannot do dynamic dispatch or generics"; it is not sourceable.
- **The strategic caveat.** `github/stack-graphs` is archived (`archived: true`, last push
  2025-09-09, final commit "This repository is no longer being maintained"); the last crate
  release was 0.14.1 in December 2024; and GitHub removed "precise code navigation" from its
  public docs between the 2024-12-01 and 2024-12-11 archive snapshots, with the live page
  today describing only tree-sitter search-based navigation.
  ([github/stack-graphs](https://github.com/github/stack-graphs),
  [crates.io](https://crates.io/crates/stack-graphs),
  [docs.github.com navigating-code](https://docs.github.com/en/repositories/working-with-files/using-files/navigating-code-on-github),
  [2024-12-01 snapshot](https://web.archive.org/web/20241201000000/https://docs.github.com/en/repositories/working-with-files/using-files/navigating-code-on-github)) — **SOURCED**.
  At peak, the docs table listed precise navigation for **Python and TypeScript only**; the
  OSS repo ships four language definitions (java, javascript, python, typescript). No
  successor was announced — **no public statement found**. Cite the ideas; do not cite it as
  current production practice.

### 4.3 M3 — early cutoff at a coarsened intermediate

This is the mechanism with the best evidence and the worst representation in codectx's plan.

- salsa's memo carries two stamps — the revision it was last *verified* in and the revision
  its value last *changed* in. After re-execution, if the new value equals the memoized one,
  salsa **backdates**: `changed_at` stays put, only `verified_at` advances. Downstream
  consumers see an unchanged dependency and stop.
  ([salsa algorithm](https://salsa-rs.github.io/salsa/reference/algorithm.html)) — **SOURCED**
- rust-analyzer does not get this for free — it **designs a value type that discards the
  churning information**. `ItemTree` is a per-file summary containing only items (signatures,
  not bodies), documented as an explicit "invalidation barrier," and the invariant is stated
  as "typing inside a function's body never invalidates global derived data."
  ([item_tree.rs](https://github.com/rust-lang/rust-analyzer/blob/master/crates/hir-def/src/item_tree.rs),
  [architecture.md](https://rust-analyzer.github.io/book/contributing/architecture.html)) — **SOURCED**
- A second, distinct firewall exists for whitespace: `crate_def_map_query` deliberately does
  not depend on parse trees; `block_def_map_query` absorbs the churn, re-executes for a
  single module, and its unchanged result stops the cascade.
  ([guide.html](https://rust-analyzer.github.io/book/contributing/guide.html)) — **SOURCED**
- Bazel reaches the same end state eagerly. Skyframe invalidates "the reverse transitive
  closure of the set of changed input files," then applies **change pruning**: if a rebuilt
  node's new value equals its old value, the nodes invalidated because of it are
  "resurrected." Their example is the canonical one — change a comment in a C++ file, the
  `.o` is identical, the linker is not re-run.
  ([bazel.build/reference/skyframe](https://bazel.build/reference/skyframe)) — **SOURCED**
- gopls v0.12 added fine-grained invalidation via "a simplified graph of symbol references in
  memory," so a change that does not alter a package's exported API stops cascading; memory
  savings averaged ~75% across 28 popular Go repositories
  ([go.dev/blog/gopls-scalability](https://go.dev/blog/gopls-scalability), via
  [03 §8](03-industrial-precise-indexers.md)) — **SOURCED**
- *Build Systems à la Carte* gives the vocabulary and the warning. Its rebuilder taxonomy is
  dirty bit → verifying traces → constructive traces → **deep constructive traces**, and deep
  constructive traces "cannot support early cutoff, since the results of intermediate
  computations are not considered." Bazel is constructive and keeps early cutoff; Nix and
  Buck are deep-constructive and give it up.
  ([JFP 30, 2020](https://www.microsoft.com/en-us/research/publication/build-systems-a-la-carte/)) — **SOURCED**

**The generalizable rule**, stated three ways by three systems: interpose a derived value
that is *narrow in fan-in and stable in output*, and let equality on that value — not on the
source bytes — decide whether dependents are invalidated.

### 4.4 M4 — overlay / stacking with ownership

- Glean never deletes or rewrites. `glean create --incremental <old> --exclude A,B,C` creates
  a DB that "stacks on top of `<old>`, hiding units A, B and C." Visibility is resolved at
  query time: every fact carries a `UsetId`, and a fact is visible iff its `UsetId` is in the
  computed slice.
  ([glean.software/blog/incremental](https://glean.software/blog/incremental/),
  [incrementality docs](https://glean.software/docs/implementation/incrementality/),
  [Create.hs](https://raw.githubusercontent.com/facebookincubator/Glean/main/glean/tools/gleancli/GleanCLI/Create.hs)) — **SOURCED**
- The invariant that makes hiding safe: "every fact referenced by a visible fact is also
  visible," enforced by propagating `A || B` from referrer to referee in reverse fact order;
  derived facts get a **conjunction** of their sources' owners, so they disappear exactly when
  any input does. — **SOURCED**
- **The price of admission, and the reason this cannot be retrofitted:** ownership is computed
  at base-index time by `glean complete`. Cost: ~**7%** of DB size, **2–3%** of index time for
  Python ("in the noise" for Hack), **<10%** on typical queries but **~3×** on search-heavy
  ones. — **SOURCED**
- Branch switching falls out for free: "the DB 'old' still exists and can be used
  simultaneously… We can even have many different versions of 'new', each replacing a
  different portion of 'old'," and deeper stacks form a tree whose intermediate nodes are all
  usable at once. — **SOURCED**
- Glean does **not** compute the changed set itself. For C++ header fanout it runs a Glean
  query finding all files that `#include` a changed file, "repeating that query until there
  are no more files to find."
  ([engineering.fb.com, Dec 2024](https://engineering.fb.com/2024/12/19/developer-tools/glean-open-source-code-indexing/)) — **SOURCED**
- CodeQL shipped the same shape in 2026 and it is worth knowing precisely, because it is the
  only *shipped* incremental extraction in the SAST tier. `--overlay-base` builds a full DB on
  the default branch; `--overlay-changes=<json>` builds "a lightweight 'overlay' database that
  only processes the changed files." Claim: "reduce scan times by up to 10x." Hard
  constraints: "**Overlay analysis supports only `build-mode: none` (traced builds are not
  supported)**", git ≥ 2.38.0, all files must be git-tracked.
  ([docs.github.com incremental-analysis](https://docs.github.com/en/code-security/how-tos/find-and-fix-code-vulnerabilities/scan-from-the-command-line/incremental-analysis)) — **SOURCED**
  The base DB is cached in the GitHub Actions Cache and restored **by key prefix**, because
  "it is exceedingly unlikely that the commit SHA will ever be the same."
  ([caching.ts](https://github.com/github/codeql-action/blob/main/src/overlay/caching.ts)) — **SOURCED**
  Before this, a maintainer stated flatly (2024-11-01): "No, CodeQL does not currently support
  incremental scans."
  ([codeql#17886](https://github.com/github/codeql/discussions/17886)) — **SOURCED**

### 4.5 M5 — serve stale, labelled, with a distance

Sourcegraph is the only system here that does this well, and it does it in a way codectx can
copy without touching any third-party tool.

- Mechanism: find nearby indexed commits, query those indexes on behalf of the requested
  commit, and "**adjust the resulting locations (file paths and ranges within a document)
  using the Git diff between the commits as a guide**."
  ([eric-fritz.com mirror](https://www.eric-fritz.com/articles/optimizing-commit-graph-part-1);
  the Sourcegraph original 404s) — **SOURCED**
- It was a query-time graph walk with a hop cap until 3.20; now it is precomputed.
  `lsif_nearest_uploads` stores per commit an `{upload_id => distance}` map, and
  `lsif_nearest_uploads_links` stores ancestor forwarding pointers for the >80% of commits
  whose visibility is rematerializable. Queries became "a simple single-record lookup."
  ([blog part 2](https://sourcegraph.com/blog/optimizing-a-code-intelligence-commit-graph-part-2),
  [schema.md](https://raw.githubusercontent.com/sourcegraph/sourcegraph-public-snapshot/main/internal/database/schema.md)) — **SOURCED**
- **The per-root shadowing rule is the part worth stealing**: visibility bookkeeping is
  `{UploadID, Root, Indexer, Distance}`, and "an index with a smaller commit distance can
  shadow another only if these values are equivalent." Each project root's staleness resolves
  independently — one stale root does not drag down a freshly indexed sibling. — **SOURCED**
- The horizon is the oldest upload, not a hop count: "we entirely ignore the portion of the
  commit graph that existed before the oldest known LSIF upload." **No staleness window was
  found** — nothing caps how old the serving upload may be; age is governed only by
  retention. — **SOURCED** for the horizon, **no public statement found** for a window.
- Is the user told? **The API is**: `CodeGraphData.commit` is documented as "the commit
  associated with this code graph data. In general, this will be an ancestor of the commit at
  which code graph data was requested."
  ([codeintel.codenav.graphql](https://raw.githubusercontent.com/sourcegraph/sourcegraph-public-snapshot/main/cmd/frontend/graphqlbackend/codeintel.codenav.graphql)) — **SOURCED**.
  A user-facing UI banner: **no public statement found**. What docs surface instead is the
  symptom — "the line containing the symbol was created or edited between the nearest indexed
  commit and the commit being browsed" — i.e. silent degradation to search-based results.
- **The limit of the technique**: cross-repository resolution is exact-version with no
  nearest-commit fallback — "if repository A@v1 depends on B@v2… [we] would not get a precise
  result if we instead have indexes for A@v1 and B@v1," and relaxing that was still open work.
  ([5.0 docs precise_code_navigation](https://5.0.sourcegraph.com/code_navigation/explanations/precise_code_navigation)) — **SOURCED**

### 4.6 Summaries — the only real answer for dataflow, and the one codectx cannot have

Infer is the sole system here that made a whole-program analysis genuinely incremental, and
it did so by being compositional from the start.

- "bi-abduction breaks apart a large analysis of a large program into small independent
  analyses of its procedures," and "when the full program is analyzed again because of a code
  change the analysis results of the unchanged part of the code can be reused."
  ([fbinfer.com separation-logic-and-bi-abduction](https://fbinfer.com/docs/separation-logic-and-bi-abduction/)) — **SOURCED**
- The change-detection mechanism is **structural identity of procedures**, not file
  timestamps and not a file dependency graph: `--mark-unchanged-procs` does "structural
  identity comparison of newly-captured procedures with previously-captured versions, marking
  the new procedure as unchanged if the two are equivalent," and `--incremental-analysis`
  implies it. `--changed-files-index` is the file "from which reactive analysis should
  **start**" — a seed set, not the work set, which then grows on demand.
  ([infer-capture.1](https://fbinfer.com/man/next/infer-capture.1.html),
  [infer-analyze.1](https://fbinfer.com/man/next/infer-analyze.1.html)) — **SOURCED**
- Numbers: OpenSSL (300k LOC, 700 files) full analyze **22m20s** vs `-reactive` **41s**
  ([engineering.fb.com 2017](https://engineering.fb.com/2017/09/06/android/finding-inter-procedural-bugs-at-scale-with-infer-static-analyzer/), via
  [03 §14](03-industrial-precise-indexers.md)); at Meta the diff-time target is "15-20min on a
  diff on average… includ[ing] time to check out the source repository, to build the diff, and
  to run on base and (possibly) parent commits," against ">an hour" for whole-program mode
  (CACM / [UCL Discovery](https://discovery.ucl.ac.uk/id/eprint/10084236/)) — **SOURCED**
- The organisational result is the reason to care: moving the same analysis with the same
  false-positive rate from batch to diff time took the fix rate from under 20% to "over 70%"
  (CACM) — **SOURCED**

**And now the hard constraint for codectx.** The dependence engine has no partial mode, and
this is not an oversight that might be fixed:

| Evidence | Status |
|---|---|
| Feature request [joern#5865](https://github.com/joernio/joern/issues/5865) (2026-03-08, **open**): "re-indexing a project after a source file changes requires a full `importCode` rebuild… ~10 min in our case." A working `javasrc2cpg` prototype reports **~350 ms per file vs ~10 min full rebuild**. No maintainer response, no merged PR. | SOURCED |
| [joern#5757](https://github.com/joernio/joern/issues/5757), maintainer, 2026-08-28, on incremental `joern-parse`: "**No, it doesn't right now.**" | SOURCED |
| [overflowdb#396](https://github.com/ShiftLeftSecurity/overflowdb/issues/396), storage maintainer: "Each overflowdb graph corresponds to exactly one storage file." CPG merging ([joern#2296](https://github.com/joernio/joern/issues/2296)) open since 2023, no maintainer response. | SOURCED |
| [joernio/flatgraph](https://github.com/joernio/flatgraph) has **zero** issues matching "incremental" (control search for "memory" returns results, so this is a real negative). | SOURCED |
| Qwiet AI / preZero on incremental CPG construction | **no public statement found** — `qwiet.ai/blog` now 301s to `harness.io/blog` |

The same holds one tier up, for different reasons:

| Evidence | Status |
|---|---|
| Sourcegraph's own SCIP announcement lists LSIF's "**complexity of implementing incremental indexing**" as a motivation, and notes "globally incrementing IDs make it difficult… to update an existing index with new information for only a subset of the documents." The SCIP fix is content-derived symbol **strings**. ([announcing-scip](https://sourcegraph.com/blog/announcing-scip)) | SOURCED |
| But per-file incremental indexing was framed as future work — "once implemented, SCIP users will experience shorter waiting time… because our backend only needs to index the files that have changed" — and **no public statement found** that it shipped. | SOURCED (the quote) / negative |
| The *format*, however, cooperates: the proto states that to permit "streaming consumption… the `metadata` field must appear at the start… **Other field values may appear in any order**," and that "complementary information can be merged together from multiple sources." A `Document` is self-contained given `Metadata.project_root`. ([scip.proto](https://github.com/sourcegraph/scip/blob/main/scip.proto)) | SOURCED |
| There is no blessed per-file index variant and no `scip merge` command (CLI is `lint, print, snapshot, stats, test, expt-convert`). | SOURCED |
| Semgrep: "cross-file analysis does not currently run on diff-aware (pull request or merge request) scans." It shipped the narrowed variant at v1.66.0 — "scan times reduced to approximately 1/10 of the non-differential inter-file scan" — then **reverted the public rollout at v1.66.2** "for further polishing." Whether it was ever re-enabled: **no public statement found**. ([Pro intro](https://docs.semgrep.dev/semgrep-code/semgrep-pro-engine-intro), [CHANGELOG](https://github.com/semgrep/semgrep/blob/develop/CHANGELOG.md)) | SOURCED |
| Snyk Code: the open-source client delta-uploads content-hashed bundles (`CreateBundle(ctx, fileHashes)` → server returns `missingFiles`; `ExtendBundle(bundleHash, files, removedFiles)`), which is upload dedup, not analysis incrementality. On incremental re-analysis: **no public statement found**. ([code-client-go](https://github.com/snyk/code-client-go/blob/main/bundle/bundle_manager.go)) | SOURCED / negative |

**The conclusion to carry into the plan:** codectx will never get a partial run out of the
dependence engine, and should never plan as if it might. Every incremental gain must come
from *avoiding* a whole-unit run, *reusing* a previous whole-unit result, or *serving* a
previous result with a label. That is exactly what M1, M3 and M5 do.

---

## 5. The two-tier freshness question — is codectx's model what peers do?

**The model under review:** instant file-local facts from tree-sitter, an LSP live overlay
for edited files, and unit-level SCIP/dependence refresh scheduled in the background and
published as a new generation.

**Yes — this is precisely what the field converged on, independently, several times.**

| System | Instant tier | Batch/precise tier | How the gap is communicated |
|---|---|---|---|
| Sourcegraph | Search-based (ctags/Rockskip) | Precise SCIP uploads | Explicit provenance order **Precise → Syntactic → SearchBased**, "stop when some data is available" ([syntactic-code-navigation](https://sourcegraph.com/docs/code-navigation/syntactic-code-navigation)) — **SOURCED** |
| JetBrains | "Dumb mode" — basic editing, VCS, and a growing set of `DumbAware` features | Full file + stub indexes | "While the analysis is in progress, smart IDE features might be unavailable or partially available" ([jetbrains.com/help/idea/indexing](https://www.jetbrains.com/help/idea/indexing.html)) — **SOURCED** |
| clangd | Dynamic index over open files | Background `Dex` over the project | Merge order: open > background > static ([03 §9](03-industrial-precise-indexers.md)) — **SOURCED** |
| Glean | — | Base DB | Stacked DB shadows it ([glean incremental](https://glean.software/blog/incremental/)) — **SOURCED** |
| Cursor | Instant Grep / regex layer pinned to a commit, agent edits layered on top | Embedding index | [01 §Patterns 5](01-ai-ide-indexers.md) — **SOURCED** |
| Cody | `symf` local keyword index that "detects file changes and reindexes as needed" | Server SCIP + Sourcegraph Search | ([cody local-indexing](https://sourcegraph.com/docs/cody/core-concepts/local-indexing)) — **SOURCED** |
| **codectx (planned)** | tree-sitter units + LSP overlay | SCIP + dependence units | Capability states `fresh/partial/stale/unavailable/failed`; precision vocabulary; `snapshot_coherent`/`worktree_changed`/`refresh_pending` (§13.3) |

So the shape is right, and codectx's **labelling discipline is better than most peers'** —
Sourcegraph's own docs admit their nearest-commit degradation is surfaced as a *symptom*, not
a statement, and no UI banner was found.

**Four things peers do better, stated plainly:**

1. **Their instant tier accepts unsaved editor buffers; codectx's does not.** tsserver treats
   this as first class: `OpenRequestArgs.fileContent` is documented as "used when a version of
   the file content is known to be more up to date than the one on disk," and `updateOpen`
   carries `changedFiles` as edits.
   ([protocol.ts](https://raw.githubusercontent.com/microsoft/TypeScript/release-5.9/src/server/protocol.ts)) — **SOURCED**.
   §11.5 excludes unsaved buffers from V1 "because CLI/MCP does not own the editor
   synchronization stream" — a correct call for a CLI/MCP tool, but it means **codectx's
   "instant" is saved-file instant**, and the docs should say so in those words rather than
   leaving the reader to assume keystroke latency.
2. **Their semantic tier is demand-driven, not whole-unit.** Infer's engine is "begin
   anywhere" and pulls a callee summary only when it needs one; salsa validates lazily on
   demand and matklad's framing is that "it's not the incrementality that makes an IDE fast.
   Rather, it's laziness — the ability to skip huge swaths of code altogether."
   ([three-architectures](https://rust-analyzer.github.io/blog/2020/07/20/three-architectures-for-responsive-ide.html)) — **SOURCED**.
   codectx cannot have this at the dependence tier (§4.6) but *can* have it at the scheduling
   tier, which the plan already does via `auto` = queried units first.
3. **They serve stale precise results with a distance instead of withholding them.** See §4.5
   and recommendation R2.
4. **Their expensive artifacts outlive the current pointer.** Sourcegraph keeps the default
   branch's uploads forever and branch tips for three months regardless of what is "current";
   Bazel's disk cache exists to survive branch switches; JetBrains ships prebuilt shared
   indexes generated once and "reused on other computers." See recommendation R1.

---

## 6. Technique-by-technique against the plan

Columns: **(1)** status in the plan, **(2)** what it buys in each scenario, **(3)** cost, **(4)**
whether it depends on a third-party tool cooperating.

| Technique | (1) In the plan? | (2) Buys — (a) local edit / (b) commit or branch switch | (3) Cost | (4) Third-party dependency |
|---|---|---|---|---|
| Content-addressed unit keys over the input closure | **Already.** `UnitID = H("unit-v1", provider_id, exact_provider_version, scope_key, analysis_config_hash, canonical_inputs, dependency_unit_keys)` (§9.1); `unit_inputs.content_hash` references `blobs(hash)` (§12.2); "Reuse must be demonstrated by equal UnitID and validated input/dependency membership" (§13.1) | (a) nothing — the input closure genuinely changed / (b) **everything**, if the unit still exists | paid | None |
| Sub-unit artifact cache keyed by **blob hash** (parse results, per-`Document` SCIP payloads) | **Missing.** File-local units are keyed on `scope_key` = path plus content, so a pure rename or a vendored copy re-parses; SCIP import has no per-`Document` dedup | (a) marginal / (b) branch-switch re-parse collapses to genuinely-new blobs; SCIP re-import collapses to changed documents | low for tree-sitter; moderate for SCIP document storage | None — SCIP's `Document` is self-contained and order-independent by spec |
| Expensive-unit retention decoupled from generation retention | **Partial.** `[providers.dependence] cache_bytes = 4294967296` gives the parsed-graph cache its own budget (§20, Task 11), but *sealed units* live only as long as a retained generation: `retain_generations = 3` (§20) and "units referenced by another retained generation or unit dependency remain" (§12.4) | (a) nothing / (b) **A→B→A across three generations currently discards A's SCIP and dependence units** and pays 7.3 s + ~28 s again per unit | low–moderate | None |
| Carried-over ("nearest-generation") stale units with a distance | **Missing for semantic scopes.** The vocabulary exists (`stale` in §13.3) but §12.3 states "a failure must not keep old facts whose input hashes no longer match," and §12.3 validates "all required input bindings" | (a) dependence answers stay available during the ~28 s refresh instead of going `pending`/`unavailable` / (b) same across a branch switch | moderate; the honesty rules are the hard part | None |
| Early cutoff on a declaration-only **signature digest** | **Missing.** §13.1 has reverse-dependency closure but no equality-based cutoff; `dependency_hash` is over dependency **unit keys**, which change whenever the dependency's content changes | (a) **the big one** — a body-only edit stops invalidating the SCIP and dependence units of a 239k-line module / (b) partial: a branch switch that only touches bodies becomes cheap | high — needs a per-language, conservative declaration digest plus differential tests | None (it is an *avoidance* decision, not a partial run) |
| Durability classes for inputs (vendored deps, lockfile-pinned sources, generated code) | **Missing.** §10.1 has one `source_policy_hash` for all eligible files | (a) the invalidation walk never examines dependency inputs / (b) same | low–moderate | None |
| Reverse-dependency dirty set + closure | **Already.** §13.1: "compute dependency closure with bounded queues and indexed reverse dependency lookups"; `unit_dependencies` table (§12.2) | (a)/(b) correctness, already assumed | paid | None |
| Overlay / stacking with per-fact ownership (Glean) | **Equivalent already, by a different route.** codectx's "new generation reusing every active unit plus the sealed one, then one atomic activation" (§13.1, [00-synthesis §6](00-synthesis.md)) achieves stacking at unit granularity instead of fact granularity | — | paid | Glean-style fact ownership would require the analyzer to emit it (7% DB size) — impossible here |
| Serve a coarser tier when the precise one is absent | **Already.** tree-sitter → SCIP → dependence with a precision vocabulary and §15 multipliers; typed `pending` capability state | (a)/(b) the product never stalls | paid | None |
| Per-file semantic artifacts + query-time stitching (stack graphs) | **Missing, and correctly so.** Would mean writing per-language name-resolution rule sets | (a)/(b) would make cross-file *name binding* file-incremental | very high; and the reference implementation is archived | None, but enormous per-language cost |
| Procedure summaries (Infer) for dependence | **Missing and blocked.** | (a)/(b) would be the 33× win | n/a | **Blocked**: no partial mode, maintainer-confirmed; the community prototype is unmerged |
| Partial/incremental SCIP emission | **Missing and blocked.** | (a)/(b) would remove the 7.3 s | n/a | **Blocked**: "once implemented", never announced; indexers are whole-project |
| Shared unit store across checkouts / worktrees | **Missing, deliberately.** `<workspace-key>` is a hash of the absolute workspace root, so "two checkouts of the same project never share state" (`docs/configuration.md`) | (a) nothing / (b) a second `git worktree` of the same repo currently redoes **all** work | low–moderate | None |
| A published branch-switch performance budget | **Missing.** §23.2 budgets a cold index, a no-change refresh and "ten changed medium files… p95 at most 1.5 s", but nothing for a large checkout | measurement discipline for (b) | trivial | None |

---

## 7. Ranked recommendations

Ranked by value delivered per unit of cost and risk, with unilaterally-adoptable changes
ahead of anything needing a third-party tool to cooperate. Each names the plan sections it
touches. These are proposals for the plan owner, not edits — no file other than this report
was modified.

**One ordering constraint, not a preference:** R4 depends on R2. Reusing a semantic unit across
a body-only edit means carrying a unit whose `unit_inputs` rows point at superseded blobs, and
rebinding those rows is exactly the carried-over-membership machinery R2 introduces. Everything
else on this list is independent and can be taken in any order.

---

### R1 — Give expensive sealed units their own byte-budgeted retention, independent of generation reachability
**Touches:** §12.4 (cleanup), §10.4 (retention), §20 (config), Task 12, Task 20.

Today a unit's lifetime is coupled to generation reachability: `retain_generations = 3`, and
§12.4 keeps only "units referenced by another retained generation or unit dependency." So the
sequence `switch B → edit → commit → switch A` can push branch A's generation out of the
window, and A's SCIP and dependence units — **7.3 s and ~28 s per unit** on a 239k-line Go
module — are deleted even though their inputs are about to become current again.

Every peer that solved scenario (b) decoupled these two lifetimes:

- Bazel's disk cache exists explicitly for "sharing build artifacts when switching branches
  and/or working on multiple workspaces of the same project" — **SOURCED**
- Sourcegraph's retention is a set of git-object policies, not a pointer: HEAD of the default
  branch **forever**, branch tips **3 months** (`2160h`), tags **1 year** (`8760h`), and
  cross-repo referents are never deleted
  ([5.0 data retention](https://5.0.sourcegraph.com/code_navigation/how-to/configure_data_retention),
  [envvars](https://sourcegraph.com/docs/code-navigation/envvars)) — **SOURCED**
- Nix evicts by reachability **plus a byte budget**: `--max-freed <bytes>` keeps deleting
  until that much is freed; `min-free`/`max-free` trigger a sweep when free space drops below
  a watermark and stop at a target
  ([nix-collect-garbage](https://nix.dev/manual/nix/2.24/command-ref/nix-collect-garbage.html),
  [conf-file](https://nix.dev/manual/nix/2.24/command-ref/conf-file.html)) — **SOURCED**

**Proposal.** Add a `[storage] unit_cache_bytes` budget. The concrete rule to change is
§12.4's collection step — "Remove unreachable units in reverse dependency order through the
FTS-aware store procedure" (plan line 1413): when a unit becomes unreachable from any retained
generation, skip that removal if its provider is marked expensive (`scip`, `dependence`), and
evict by LRU only when the budget is threatened — Nix's watermark shape, not a TTL. Mechanically
this is a new `units.state` value (`'retained'`) added to §12.2's
`CHECK(state IN ('building','sealed','failed','quarantined'))` (plan line 1075), which also
requires auditing every query that filters on `state = 'sealed'` so a retained unit is
selectable for reuse but never selectable as a *new* build target. Sealed facts, aliases and
search documents stay intact, so reuse is a `generation_units` insert.

**Internal precedent, which makes this cheap to argue:** `[providers.dependence] cache_bytes
= 4294967296` already gives the parsed-graph cache exactly this treatment (§20, Task 11).
This extends the same policy one layer up, to the facts rather than the engine artifact.

**Cost:** low–moderate — a state on `units`, a byte accounting, and a change to §12.4's
collection order. **Third-party dependency:** none. **Risk:** disk growth, bounded by the
budget and reported in the existing disk-category breakdown (§12.4).

---

### R2 — Make §13.3's `stale` reachable for semantic scopes: explicitly carried-over units with a provenance distance
**Touches:** §12.2 (`generation_units`), §12.3 (activation validation), §13.1, §13.3, §14/§19 result metadata, Task 5, Task 12.

Today, generation-versus-worktree staleness **is** handled and labelled — §13.3's
`snapshot_coherent` / `worktree_changed` / `refresh_pending`, and "an active generation is
coherent with its own snapshot even when the worktree is newer." That is already Sourcegraph's
query-a-revision model, and it is right.

What is missing is an **intentionally heterogeneous generation**: fresh tree-sitter units for
the current snapshot *plus* a knowingly-carried-over SCIP or dependence unit from an older
input hash, published with that scope's capability state set to `stale`. §12.3's rule — "a
failure must not keep old facts whose input hashes no longer match" — currently forecloses it.

This is what Sourcegraph does and it is the single biggest experiential difference. Their
per-root shadowing rule is the exact analogue of codectx's per-`(provider, scope_key)`
membership: `{UploadID, Root, Indexer, Distance}`, where "an index with a smaller commit
distance can shadow another only if these values are equivalent" — one stale root does not
drag down a fresh sibling. — **SOURCED**

**Proposal, framed as additive so the existing invariant survives.** Keep the ban on
*silently* mismatched facts. Add an explicit carried-over membership carrying a **provenance
distance** — at minimum the number of that unit's input files whose content hash has changed
since it sealed, and the generation it came from. Rules:

- A carried-over unit is admitted only when its scope's capability state is published as
  `stale`, with the distance in the diagnostic payload.
- `codectx` results already carry provider, precision and generation; add the distance and
  the originating snapshot so a consumer can decide. Never blend it into a `fresh` answer.
- The context compiler already refuses ephemeral overlay facts (§15); it should treat
  carried-over units the same way — visible to explicit queries, excluded from canonical
  plans, or included only with the staleness recorded in the coverage report (§16).
- Default it off for `dependence` and on for `scip` if that is the safer starting point; make
  it configurable per provider.

**The schema already permits this; only the prose forbids it.** `generation_units` is
`PRIMARY KEY(generation_id, provider_id, scope_key)` with `UNIQUE(generation_id, unit_id)` —
nothing in the DDL requires a member unit's inputs to match the generation's snapshot. What
forecloses a carried-over unit is §12.3's activation validation ("all required input
bindings") and §12.3's prose rule about failures keeping old facts. That makes R2 much smaller
than it looks: no schema migration on the membership table, one validation rule that admits an
explicitly-marked carry-over, plus a distance column and the result-metadata plumbing.

**Internal precedent:** §12.2 already has `units.source_binding IN ('verified','unverified')`,
and §11.4 already describes admitting a unit that "remains importable for discovery/quarantine
but cannot silently enter strict compiler evidence." The machinery for "in the store, visible,
but not strict evidence" exists. Report [02 §158.4](02-code-graph-tools.md) independently
reached the same recommendation: "mark them stale and let queries see it. Do not silently
serve resolved edges from a superseded index — that's a worse failure than a labelled guess."

**A later refinement, not V1:** Sourcegraph additionally rewrites file paths and ranges
through the git diff between the two revisions. That is what makes a stale index *usable*
rather than merely present, and codectx has the ingredients (both snapshots' manifests, both
blobs). Cost is real; propose it as a follow-on once carried-over units exist.

**Cost:** moderate. **Third-party dependency:** none. **Risk:** the honesty rules are the
whole design; get them wrong and the tool's central promise breaks.

---

### R3 — Key sub-unit artifacts by content hash: a parse cache and per-`Document` SCIP storage
**Touches:** §9.1 (identities), §11.3, §11.4, §12.2 (schema), Task 8, Task 9.

Two distinct changes with one idea behind them.

**(a) Parse cache keyed by `(blob hash, language, grammar version)`.** codectx's file-local
unit key includes `scope_key` (the path) and `canonical_inputs` (path + content hash), so a
pure rename, a vendored copy, or two worktrees of the same file all re-parse. GitHub's
highest-leverage decision was the opposite — "once a blob is parsed, it never needs to be
parsed again and can be shared across any commit that includes that blob"
([03 §6](03-industrial-precise-indexers.md)) — and stack-graphs' OSS CLI does the same with
`let tag = sha1(source)`. — **SOURCED**. Note this is a *cache below the unit layer*: unit
identity stays path-qualified (correct, since paths are semantically meaningful), but the
expensive artifact is fetched by content.

**(b) Store imported SCIP per `Document`, keyed by its payload hash.** This is the finding
most worth acting on. Sourcegraph's indexers are whole-project producers exactly like
codectx's, and they got per-file incrementality anyway by shredding each index into
`Document` payloads with a `UNIQUE` `payload_hash` and a `(upload_id, document_path) →
document_id` lookup. — **SOURCED**. The format cooperates: fields "may appear in any order",
streaming consumption is the documented recommendation, and complementary indexes are meant to
be merged. A re-run of `scip-go` after a one-file edit still costs 7.3 s of *indexer* time,
but the *import, normalization, alias generation and FTS write* for 1,282 unchanged documents
collapse to a hash comparison.

**One gap that must be named, or it will be found the expensive way.** The callsite-join
reconciliation ([00-synthesis §3](00-synthesis.md), [10 §2](10-round3-empirical.md)) matches a
SCIP occurrence range against a *tree-sitter* call-site range **in the same file version**;
the two range sets are only comparable when both sides are bound to the same blob. So a
`Document` payload deduped purely by `payload_hash` is not safe to inherit its join from if
the tree-sitter unit for that path was re-emitted against a different blob. Either make the
dedup key `(payload_hash, blob_hash)`, or recompute the join on import even for deduped
documents and let the dedup save only the storage and FTS work. The first is cheaper; the
second is simpler. Say which in the plan rather than leaving it implicit.

**Cost:** low for (a); moderate for (b) — a `scip_documents` dedup table and an import path
that compares before normalizing. **Third-party dependency:** none — this is what SCIP's
own design permits. **Risk:** low; it is a pure caching layer under existing identities.

---

### R4 — Early cutoff: give file-local units a declaration-only signature digest, and key semantic units on that
**Touches:** §11.1 (`InvalidationScope`, `UnitSpec`), §12.2 (`dependency_hash`), §13.1, §23.2, Task 6, Task 8, Task 12.

**This is the highest-value change for scenario (a) and the one the plan has no analogue of.**

Today, editing one line inside one function body changes that file's content hash, which
changes the file-local unit's `UnitID`, which changes the semantic unit's
`dependency_unit_keys`, which invalidates the whole SCIP and dependence unit — **~28 s and
~1.0 GB for a 239k-line Go module** — even though nothing cross-file could possibly have
changed.

Three systems solve this the same way, and all three solve it by *designing a value that
discards the churning information*:

- rust-analyzer's `ItemTree` holds items only, is documented as an "invalidation barrier",
  and yields the invariant "typing inside a function's body never invalidates global derived
  data." — **SOURCED**
- gopls v0.12 stops the cascade when a package's exported API is unchanged, via "a simplified
  graph of symbol references in memory." — **SOURCED**
- Bazel Skyframe's change pruning "resurrects" invalidated nodes when a rebuilt value equals
  its predecessor; the worked example is a comment change producing an identical `.o`. — **SOURCED**

**Proposal.** Every file-local unit emits, alongside its facts, a **signature digest**: a
canonical hash over the declarations a cross-file consumer could observe — exported and
package-visible names, signatures, types, imports, re-exports — and explicitly *not* function
bodies, comments, or local variables.

**Where the digest has to enter the identity — this is the part that must be stated precisely,
because the obvious placement does not work.** It is tempting to say "the semantic unit's
`dependency_hash` is computed over its inputs' signature digests." That is not sufficient.
§9.1 defines `UnitID = H(…, canonical_inputs, dependency_unit_keys)`, and a semantic unit's
*own* inputs are the source files themselves — so a body-only edit changes `canonical_inputs`
directly and the `UnitID` moves regardless of what `dependency_hash` does. The digest must
therefore be adopted in two places:

1. **`canonical_inputs` for semantic units is computed over per-file signature digests**, not
   raw content hashes. This is what actually holds the `UnitID` still across a body-only edit.
   `dependency_hash` follows the same rule for consistency, but it is not the load-bearing
   change.
2. **`unit_inputs` records the signature digest alongside `content_hash`.** §12.2 binds
   `unit_inputs(unit_id, file_id, content_hash)`, `node_facts`/`relation_facts`/`text_facts`
   all carry `FOREIGN KEY(unit_id, file_id) REFERENCES unit_inputs(unit_id, file_id)`
   (plan lines 1129/1160/1191), and §12.3 validates "all required input bindings" before
   publication. A semantic unit reused across a body-only edit would otherwise still record
   the *old* content hashes: it would either fail that validation against the new snapshot, or
   pass and then cite byte ranges in blobs that are not in the generation. The reused unit's
   input rows must be **rebound to the new blob hashes** while the digest column — the thing
   the identity is keyed on — stays equal.

With both in place, a body-only edit leaves every signature digest equal, the semantic
`UnitID` is unchanged, and §13.1's existing reuse rule ("Reuse must be demonstrated by equal
UnitID and validated input/dependency membership") reuses the SCIP and dependence units.

**Dependency: R4 needs R2's machinery, they are not independent.** The input rebinding in (2)
is exactly the "a unit carried into a generation whose inputs are not byte-identical to the
ones it sealed against" case that §12.3 currently forecloses and that R2 exists to express.
Sequence R2 before R4. If R4 ships alone, it needs its own rebinding-and-revalidation path,
which is most of R2 built under a different name.

**The risk, stated honestly, because it is the reason to phase this.** Soundness is
per-language and the failure mode is silent wrong answers — exactly what the plan exists to
prevent. Doc comments feed documentation facts; constant values participate in types
(`const` generics, array sizes); macros and `#[cfg]`/build tags can promote body text into
declarations; Python and TypeScript can make almost anything observable. Mitigations:

- Ship the digest **per language**, behind the Task 22 matrix, and only for languages where a
  differential fixture proves the semantic unit's facts are byte-identical across a
  body-only edit.
- Default to the conservative behaviour (digest = content hash) for any language not proven.
- Publish the capability as `fresh` only when the language is proven; otherwise the digest is
  used to schedule, and the result is labelled per R2.
- Reuse the existing regression discipline in §23.5 — an unproven digest is a correctness
  bug, not a performance tuning knob.

**Cost:** high. This is the most expensive item on the list and the most valuable; it should
be scoped as its own task, not folded into Task 12. **Third-party dependency:** none — it
decides *whether* to run the engine, never asks the engine for a partial run.

**Interaction with the settled subdivision ruling.** Finer units would also improve
incrementality, but [00-synthesis §8](00-synthesis.md) already ruled that subdivision is
last-resort crash recovery, never for memory, and never for a tsconfig project or C/C++.
R4 is offered as a *different* lever that gets a similar effect without re-litigating that —
it keeps the unit whole and makes the unit run less often.

---

### R5 — Durability classes for inputs
**Touches:** §10.1 (source policy), §13.1, §20, Task 4, Task 12.

salsa's durability levels exist because, without them, "any change to `src/lib.rs`
necessitates checking all the queries related to standard library (which adds up to about
**300ms**)" — the fix is to shard the "last changed" scalar per churn class so a workspace
edit never even *traverses* dependency-derived data. Levels are documented as LOW ("part of
the crate being edited"), MEDIUM ("a Cargo.toml file"), HIGH ("the standard library or
something from crates.io"), plus `NEVER_CHANGE`.
([durable-incrementality](https://rust-analyzer.github.io/blog/2023/07/24/durable-incrementality.html),
[durability.rs](https://github.com/salsa-rs/salsa/blob/master/src/durability.rs),
[salsa durability](https://salsa-rs.github.io/salsa/reference/durability.html)) — **SOURCED**

**Proposal.** Classify every captured file at snapshot time into a small, explicit durability
set — workspace source (volatile), manifests and lockfiles (medium), vendored dependencies /
`node_modules` / generated output (durable) — and record a per-class watermark alongside
`source_policy_hash`. The coordinator's reuse check and the watch-mode reconciliation walk
(§13.2, `reconcile_interval = "30s"`) then short-circuit on the class watermark instead of
examining every file. This also gives the 30-second reconciliation a cheap fast path on
repositories where most bytes are vendored.

**Cost:** low–moderate. **Third-party dependency:** none. **Risk:** a misclassified durable
file is a stale-facts bug; the classification must be conservative and the periodic full
reconciliation (§10.2) must still catch it, which the plan already requires.

---

### R6 — An opt-in unit store shared across checkouts of the same repository
**Touches:** `docs/configuration.md` (data directory), §12.1, §20, Task 3, Task 5.

Today `<workspace-key>` is a 16-character prefix of the hash of the absolute workspace root,
with the stated consequences that "two checkouts of the same project never share state and no
path component of the repository ever appears in the data path." The privacy rationale is
sound. The cost is that a second `git worktree` — the standard way to work two branches at
once, and therefore precisely the scenario-(b) workload — redoes every unit, including the
~28 s dependence units, for content the machine has already analyzed.

Peers share aggressively: Bazel's disk cache covers "multiple workspaces of the same
project" — **SOURCED**; JetBrains ships shared indexes "generated once and… later reused on
other computers whenever needed", including prebuilt indexes for the three latest LTS JDKs
([shared-indexes](https://www.jetbrains.com/help/idea/shared-indexes.html)) — **SOURCED**;
Cursor reuses a teammate's index rather than "rebuilding every index from scratch when someone
joins or switches machines" — **SOURCED**.

**Proposal.** Keep the default exactly as is. Add an opt-in `storage.shared_unit_store`
keyed on a repository identity that leaks no path — the first-commit OID is the obvious
candidate — holding only the content-addressed artifacts of R1 and R3, never sessions,
receipts, or generations. Two worktrees then share parses, SCIP documents and sealed
semantic units while keeping independent generations and independent workflow state.

**Cost:** low–moderate (a second store root, a lock discipline across processes, and the §12.1
"one write connection per workspace owner process" rule needs a story for the shared store).
**Third-party dependency:** none. **Risk:** cross-workspace contamination; mitigate by
restricting the shared store to content-addressed rows whose key already proves what they are.

---

### R7 — Publish a branch-switch budget and a reuse metric in §23.2
**Touches:** §23.2, §23.5, Task 21.

§23.2 budgets a cold index (≤3 min), a no-change refresh (p95 ≤250 ms, "no parser work and no
FTS body rewrite") and "ten changed medium files, base refresh" (p95 ≤1.5 s). There is **no
budget for a large checkout**, which is the workload R1, R3 and R6 exist to serve — and an
architecture property with no budget is an architecture property that regresses silently.

Comparable published anchors, for calibration:

| System | Figure | Source |
|---|---|---|
| GitHub tags, incremental index | p99 **~10 s**, p50 **~1.3 s** | CACM, via [03 §6](03-industrial-precise-indexers.md) — **SOURCED** |
| Sourcegraph symbols service, warm | **~20 s** on 40M LOC / 400k files vs ~20 min cold | [5.0 features](https://5.0.sourcegraph.com/code_navigation/explanations/features) — **SOURCED** |
| Rockskip, new commits | "**less than 1 second most of the time**" | [rockskip](https://sourcegraph.com/docs/code-navigation/rockskip) — **SOURCED** |
| Infer, per diff at Meta | **15–20 min** including checkout and build, vs >1 h whole-program | CACM — **SOURCED** |

**Proposal.** Add two rows: *"branch switch touching 5,000 files to a previously indexed
revision"* — base tier cost dominated by the count of genuinely-new blobs, and zero re-runs of
any semantic unit whose signature digests are unchanged — and *"no-op re-switch (A→B→A)"* —
reuse counts only. Add reuse-hit-rate to the §23.5 benchmark set alongside the existing
reuse/parse counts, and add the A→B→A cycle to Task 12's incremental integration scenario,
which today covers "ten changed files, a new symbol/dependency, delete/rename" but not a
checkout.

**Cost:** trivial. **Third-party dependency:** none.

---

### R8 — Document the instant tier's actual boundary: saved-file instant, not keystroke instant
**Touches:** §11.5, §19, `docs/providers-lsp.md`.

§11.5 states that "unsaved editor buffers remain outside V1 because CLI/MCP does not own the
editor synchronization stream." That is the right call for a CLI/MCP tool. But peers whose
instant tier *does* accept unsaved buffers set the user's expectation: tsserver's
`OpenRequestArgs.fileContent` is "used when a version of the file content is known to be more
up to date than the one on disk," and `UpdateOpenRequest` carries `changedFiles` as edits.
— **SOURCED**

**Proposal.** State the boundary in the user-facing wording rather than only in the design
rationale: codectx's overlay is fresh as of the last **write to disk**, and the snapshot
capture (§10.2, with its detected-change validation and `capture_consistency` field) is what
defines "now". Nothing to build; one paragraph, so that "instant" is not read as
keystroke-latency.

---

### R9 — Reverse-dependency fixpoint for include-style languages at the SCIP tier
**Touches:** §11.4, §13.1, Task 9.

Glean does not compute changed sets itself, but for C++ it computes header fanout with a
Glean query "finding all the files that `#include` one of the changed files, and then
repeating that query until there are no more files to find." — **SOURCED**. Kythe's
maintainer names the same limit from the other side: change a widely-included header and every
dependent compilation unit's digest changes. — **SOURCED**

codectx already analyses C/C++ dependence as one whole-repository unit, so this buys nothing
there. It matters only for `scip-clang`, whose unit is derived from `compile_commands.json`.
Rank it low: the plan's §13.1 already says "if a semantic provider cannot safely invalidate
locally, rerun its declared larger unit and report it," which is the safe behaviour.

---

### R10 — Warm the tip of the default branch in the background
**Touches:** §13.1, §20.

Sourcegraph's protected policy keeps the default branch's HEAD uploads forever, and
sourcegraph.com runs HEAD-only policies for all public code, so "there is only one set of
indexes covered by this policy per repository at any given time." — **SOURCED**

For a local tool this is speculative work on a laptop and conflicts with the plan's
`max_concurrent_heavy_analyzers = 1` and no-background-surprises posture. Listed for
completeness; **not recommended for V1**. R1 delivers most of the same benefit reactively and
without spending anything the user did not ask for.

---

## 8. What could not be sourced

Recorded so the gaps are not mistaken for findings.

| Question | Outcome |
|---|---|
| Stack-graphs index size per file, measured query latency, repo-scale numbers | **No public statement found** — the paper publishes goals ("under 100 ms") and no measurements |
| A published stack-graphs limitation list (dynamic dispatch, generics, type inference) | **No public statement found** — the blog raises them as open questions and never answers |
| A named successor to GitHub's precise code navigation | **No public statement found**; the docs reverted to search-based navigation with no announced replacement |
| Glean's end-to-end wall-clock cost to index a diff | **No public statement found** |
| Kythe's serving-table incremental update; how Google runs it per-commit | **No public statement found** — the reference pipeline `rm -rf`s both the graphstore and the serving table |
| Whether Semgrep's inter-file differential scanning was ever publicly re-enabled after the v1.66.2 revert | **No public statement found** (silence, not a denial) |
| Snyk Code / DeepCode incremental re-analysis or a global symbol cache | **No public statement found**; only the open-source client's hash-addressed bundle delta is evidence |
| Qwiet AI / preZero incremental CPG construction | **No public statement found**; `qwiet.ai/blog` now redirects to `harness.io/blog` |
| A Sourcegraph staleness window bounding how old a served nearest-commit upload may be | **No public statement found** — only retention governs age |
| A Sourcegraph UI banner telling the user results came from a different commit | **No public statement found** — the API field exists; the UI surfaces the symptom instead |
| Cursor's sync interval, obfuscated filenames, branch-switch behaviour | **No public statement found**; the widely-repeated figures trace to third-party write-ups |
| JetBrains shared-index artifacts keyed by content hash; per-branch indexes | **No public statement found**; the per-branch request (IDEA-79342 → IJPL-75099) is unresolved and **Shelved** |
| Any tool keying a cache on the **git blob SHA-1** directly, or using `git worktree` for index isolation | **No public statement found** for any surveyed tool — Continue and Cursor both read-and-hash instead |
| Roo Code's mechanism behind "Hash-based Caching" and "Branch Aware" | **No public statement found** — the feature bullets are the entirety of the public statement |
| What `.tsbuildinfo` stores internally | **No public statement found** — docs say only "information about the project graph" |

---

## 9. Summary of status against the plan

| Mechanism | Status |
|---|---|
| Content-addressed unit keys (M1, unit level) | **Already** — §9.1, §12.2, §13.1 |
| Content-addressed artifacts *below* the unit (M1, blob level) | **Missing** → R3 |
| Expensive-unit retention decoupled from the active pointer | **Partial** — the engine-artifact cache has it (§20); sealed units do not (§12.4) → R1 |
| Early cutoff on a coarsened intermediate (M3) | **Missing** → R4 (depends on R2) |
| Durability classes | **Missing** → R5 |
| Reverse-dependency dirty set and closure | **Already** — §13.1, `unit_dependencies` |
| Stacking / overlay (M4) | **Already, by a different route** — new generation reusing active units + atomic activation (§13.1, [00-synthesis §6](00-synthesis.md)) |
| Serve stale with a labelled distance (M5) | **Missing for semantic scopes**; the vocabulary exists (§13.3) → R2 |
| Two-tier instant + background precise, with declared precision | **Already**, and better labelled than most peers |
| Per-file semantic artifacts with query-time stitching (M2) | **Missing, correctly** — reference implementation archived, per-language cost enormous |
| Procedure summaries; partial SCIP emission | **Missing and blocked** by the third-party tools — see §4.6 |
| Shared store across checkouts | **Missing, deliberately** → R6 (opt-in only) |
| Branch-switch budget | **Missing** → R7 |
