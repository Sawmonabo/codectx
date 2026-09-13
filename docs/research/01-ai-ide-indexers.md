# AI Coding-Assistant Indexers: What They Actually Build, and Why They Feel "Instant"

Research slice 01 for **codectx**. Compiled 2026-09-13. Every non-obvious claim carries a URL.
Claims sourced only from vendor marketing or third-party aggregators are labelled **[vendor claim]** or **[third-party]**.
Inferences I drew from reading source/APIs myself are labelled **[verified by me]** with the method.

---

## 1. Cursor

**(a) What it indexes.** Two independent indexes, neither of which is a symbol graph.

1. *Semantic index*: files are split into "syntactic chunks", each chunk embedded. Stored as `(embedding, obfuscated relative path, line range)` — never source text. ([Cursor: Securely indexing large codebases](https://cursor.com/blog/secure-codebase-indexing))
2. *Instant Grep*: a sparse n-gram (trigram-family) inverted index for regex search. ([Cursor: Fast regex search](https://cursor.com/blog/fast-regex-search))

No declarations table, no call graph, no reference resolution is published.

**(b) Parser technology.** "Syntactic chunks" implies AST-aware chunking, but Cursor does not name tree-sitter in the indexing blog — treat the chunker's internals as unknown. The regex index is purely lexical (n-grams over bytes); it does no parsing at all.

**(c) Storage.** Embeddings + metadata live in **Turbopuffer** (remote vector DB); an embedding cache keyed by chunk hash lives in AWS. Cursor's security page states: *"At our server, we chunk and embed the files, and store the embeddings in Turbopuffer"* and that they keep *"the embedding in a cache in AWS, indexed by the hash of the chunk, to ensure that indexing the same codebase a second time is much faster"* ([quoted by Simon Willison, 2025-05-11](https://simonwillison.net/2025/May/11/cursor-security/)). File paths are encrypted before upload and *"code content is never stored in plaintext"* ([Cursor docs](https://cursor.com/docs/context/codebase-indexing)). The Instant Grep index is **client-side only**, in two files: posting lists flushed sequentially to disk, plus a lookup table of n-gram hashes → offsets that is memory-mapped by the editor process ([fast-regex-search](https://cursor.com/blog/fast-regex-search)).

**(d) How it gets "instant".** This is the most instructive part of the whole survey.

- **Merkle tree sync.** Client computes SHA-256 per file plus folder hashes bottom-up. An edit changes only that file's hash and its ancestors to the root. Client and server compare roots and *"Cursor walks only the branches where hashes differ."* For a 50,000-file workspace, *"just the filenames and SHA-256 hashes add up to roughly 3.2 MB"* — so a naive full-manifest sync is already megabytes, which is why they diff the tree.
- **Content-addressed embedding cache.** Unchanged chunks never get re-embedded, across users and across machines.
- **Cross-tenant index reuse via simhash.** A new user on an already-indexed repo derives a *similarity hash* from their Merkle tree and looks it up against existing indexes, reusing them rather than rebuilding. Access control is enforced by hash challenge: *"the server filters results by checking those hashes against the client's tree. If the client can't prove it has a file, the result is dropped."*
- **Published speedups from this work** ([same blog](https://cursor.com/blog/secure-codebase-indexing)): median repo **7.87 s → 525 ms**; p90 **2.82 min → 1.87 s**; p99 **4.03 h → 21 s**. Note the p99: even Cursor's "instant" indexing was taking *four hours* before this work.
- **Regex index is pinned to a git commit**, with *"user and agent changes … stored as a layer on top of it"* — i.e. a committed base index plus a working-tree overlay. That is precisely codectx's snapshot-qualified overlay model.

**(e) Cross-file call graph / dataflow.** None published. Cursor's own framing is that semantic search *supplements* grep: *"the combination of these two leads to the best outcomes."*

**(f) Numbers.**
- Semantic search vs grep-only: *"on average 12.5% higher accuracy in answering questions (6.5%–23.5% depending on the model)"*; agent code retention **+0.3%** overall, **+2.6%** on codebases of 1,000+ files ([Cursor: Improving agent with semantic search](https://cursor.com/blog/semsearch)).
- Instant Grep: `rg` invocations *"that take more than 15 seconds"* on large monorepos; a Chromium agent workflow dropped from ~240 s to ~90 s end-to-end (~60% reduction) ([fast-regex-search](https://cursor.com/blog/fast-regex-search)).
- **[third-party]** A widely-repeated "16.8 s ripgrep vs 13 ms Instant Grep (~1,300×)" figure appears in aggregator write-ups ([dev.to](https://dev.to/alanwest/cursor-just-made-ripgrep-look-slow-heres-how-5gha)); I could not find it in Cursor's own post. Treat as unverified.

---

## 2. GitHub Copilot (workspace / repo index)

**(a) What it indexes.** A *semantic code search index* per repository, built and hosted by GitHub. Chunk embeddings, not symbols. ([GitHub docs: repository indexing](https://docs.github.com/en/copilot/concepts/context/repository-indexing))

**(b) Parser technology.** Not published for the Copilot index. Separately, GitHub's *code navigation* (the go-to-definition on github.com) uses **stack graphs** built on tree-sitter — name-binding rules expressed in a graph construction language, producing precise-ish navigation *"without requiring any configuration from the repository owner, and without tapping into a build process or other CI job"* ([GitHub: Introducing stack graphs](https://github.blog/open-source/introducing-stack-graphs/)). Language coverage is narrow (Python, Java, JS/TS, Rust `.tsg` grammars). **This is a different system from the Copilot chat index** — I found no source saying Copilot's retrieval consumes stack graphs.

**(c) Storage.** Remote, on GitHub's infrastructure. VS Code notes that *"parts of the index might be stored on your machine and parts might come from remote sources"* ([VS Code: How Copilot understands your workspace](https://code.visualstudio.com/docs/agents/reference/workspace-context)).

**(d) How it gets "instant".** *Remote precompute, amortised per repository.* The index *"only needs to be built once per repository, which means it is often instantly available"*. GitHub cut initial indexing *"from approximately 5 minutes to just a few seconds (maximum 60 seconds in some cases)"* ([changelog, 2025-03-12](https://github.blog/changelog/2025-03-12-instant-semantic-code-search-indexing-now-generally-available-for-github-copilot/); [community discussion #153841](https://github.com/orgs/community/discussions/153841)). There is no per-developer index build at all for GitHub-hosted repos.

**The critical caveat for codectx:** the index is built **from the default branch**, so it is blind to your working tree and your feature branch. VS Code patches this by *supplementing the remote result with a targeted local search of modified files*. So the "instant" index is deliberately *stale* by design, with a local delta bolted on.

**(e) Cross-file call graph / dataflow.** Not in the Copilot index. Stack graphs give cross-file *name binding* (def/ref) for a few languages, but that is a separate product surface.

---

## 3. Continue.dev

**(a) What it indexes.** Four parallel indexes over the same file walk ([DeepWiki: Continue codebase indexing](https://deepwiki.com/continuedev/continue/3.4-codebase-indexing); source at [`core/indexing/`](https://github.com/continuedev/continue/tree/main/core/indexing)):

1. `LanceDbIndex` — chunk embeddings.
2. `CodeSnippetsIndex` — **tree-sitter**, using per-language `.scm` queries, writing a `code_snippets` table with `title`, `signature`, `content`.
3. `FullTextSearchCodebaseIndex` — SQLite **FTS5**.
4. A basic chunk index.

**(b) Parser technology.** Tree-sitter (`.scm` tag queries) for the snippets index only; the vector and FTS indexes are text-level. No LSP, no compiler.

**(c) Storage.** **LanceDB** for vectors, **SQLite** for everything else, both under `~/.continue/index`. Embeddings computed locally by default via `transformers.js` ([Continue docs](https://docs.continue.dev/reference/deprecated-codebase)).

**(d) How it gets "instant".** A genuinely well-engineered incremental model worth copying:

- **`IndexTag` = (directory, branch, artifactId)**. Every artifact is indexed per branch, so branch switches do not invalidate everything.
- A `tag_catalog` table stores *last-updated timestamp and content hash (`cacheKey`) per file per tag*.
- `getComputeDeleteAddRemove()` diffs the filesystem against `tag_catalog` and emits four buckets: **compute** (new content — do the expensive work), **addTag** (content already embedded under another tag — just add a row), **delTag**, **removeTag**. Switching branches on a repo you've already indexed is therefore mostly row bookkeeping, not recomputation.
- 5 MB per-file cap.

**(e) Cross-file call graph / dataflow.** **No.** The tree-sitter layer extracts declarations only; there is no reference resolution and no edges between files.

**(f) Retrieval config & a notable retreat.** Defaults were `nRetrieve: 25` → LLM rerank → `nFinal: 5`. **`@Codebase` is now deprecated**: *"The `@Codebase` context provider has been deprecated in favor of a more integrated approach to codebase awareness"*, i.e. Agent mode's file-exploration and search tools ([Continue docs](https://docs.continue.dev/reference/deprecated-codebase), [migration guide](https://docs.continue.dev/guides/codebase-documentation-awareness)). An open-source team that built a four-index local RAG stack has walked it back toward agentic grep.

---

## 4. Sourcegraph / Cody

The only tool in this survey with **compiler-precise cross-file edges**.

**(a) What it indexes.** SCIP indexes: `Document` → `Occurrence` (range + symbol + `SymbolRole`) + `SymbolInformation`. That gives definitions, references, implementations, type-definitions, cross-repo ([scip-code.org](https://scip-code.org/)). Indexers exist for C#, VB, C++, C, Dart, Go, Java, Scala, Kotlin, PHP, Python, Ruby, Rust, TS/JS.

**(b) Parser technology.** **Real compilers/type-checkers.** `scip-typescript` drives the TS compiler; `scip-java` uses a SemanticDB compiler plugin; `scip-clang` uses Clang. Plus a **syntactic fallback** (search-based navigation) when no precise index exists.

**(c) Storage.** Protobuf `.scip` files, uploaded to a Sourcegraph instance. SCIP replaced LSIF's integer-ID graph encoding with **globally-unique symbol strings**, which is what makes it streamable and parallelisable: an indexer can *"load parts of the codebase, append index data for that part to an open file, and then move on"* with memory cleared ([SCIP DESIGN.md](https://raw.githubusercontent.com/sourcegraph/scip/main/docs/DESIGN.md)). LSIF's integer IDs meant *"off-by-one bugs in indexers cause code navigation to fail repo-wide."*

**(d) How it gets "instant".** *It doesn't, and doesn't pretend to.* Indexing is a CI-side batch job; queries are instant because the index is precomputed and uploaded out-of-band. This is the honest version of Copilot's remote-precompute trick.

**(e) Cross-file call graph / dataflow.** Cross-file def/ref/implementation edges: **yes, compiler-precise**. Dataflow: **no.** Eric Fritz's write-up of Cody's code-intelligence context service describes the exact pipeline: *"cheap syntactic analysis (via TreeSitter)"* extracts symbol names from visible code → those are *"translated into SCIP names"* → traverse the graph → return code metadata, locations and source text into the context window. Notably, *"traverse the relationship beyond definitions … references, implementations, prototypes, type-definitions"* is listed as **future work** — today they follow definition edges only ([LLM Antihallucinogen](https://www.eric-fritz.com/articles/llm-antihallucinogen)).

**(f) Numbers.**
- **Verified.** SCIP payloads compress to *"around in the range of 10%-20%"* of raw size, and the format is streaming/parallel **by design** — indexers append per-document and clear memory as they go ([DESIGN.md](https://raw.githubusercontent.com/sourcegraph/scip/main/docs/DESIGN.md)). The `Occurrence.range` packed-array encoding alone gives a *"~50% reduction in index payload size"* versus message-based encoding ([DeepWiki: sourcegraph/scip](https://deepwiki.com/sourcegraph/scip)).
- **[unverified]** "SCIP is about **8x smaller than LSIF** and can be processed **3x faster**" is widely attributed to [Announcing SCIP](https://sourcegraph.com/blog/announcing-scip), but sourcegraph.com **403s every automated fetch**, and the figure is absent from the `sourcegraph/scip` README, DESIGN.md and DeepWiki. It reached me only via a search engine's generated summary. **Do not plan against it** — confirm by opening the blog in a browser.
- SCIP payloads compress to *"around in the range of 10%-20%"* of raw size ([DESIGN.md](https://raw.githubusercontent.com/sourcegraph/scip/main/docs/DESIGN.md)).
- **[unverified]** "`scip-typescript` indexes **1k–5k lines of code per second**, a **3–10× speedup** over `lsif-node`" is attributed to [Announcing scip-typescript](https://sourcegraph.com/blog/announcing-scip-typescript). Same caveat: the blog 403s, and the [`scip-typescript` README](https://raw.githubusercontent.com/sourcegraph/scip-typescript/main/README.md) carries **no** throughput numbers. Treat as indicative only.
- **Verified, and the practically important one:** `scip-typescript`'s cross-project symbol cache *"increases the memory footprint which can cause out-of-memory errors"*; disabling it *"slows down indexing but reduces the memory footprint"* (README). **Memory, not wall-clock, is the binding constraint on precise indexing.**

---

## 5. Windsurf / Codeium (now Cognition)

**(a)–(c) What's actually documented.** Very little. The official docs (docs.windsurf.com now 307-redirects to [docs.devin.ai/desktop/context-awareness/overview](https://docs.devin.ai/desktop/context-awareness/overview)) say: the entire local codebase is indexed including unopened files; retrieval is RAG-based; a proprietary method called **M-Query** is used for codebase context; Teams/Enterprise can additionally index **remote repositories**; indexing limits scale by subscription tier. Embedding model, storage location, index format, and size limits are **not disclosed**.

**(d) How it gets "instant".** Undisclosed. Plan-tiered "indexing limits" imply hard caps rather than a scaling trick.

**(e) Cross-file call graph / dataflow.** Nothing published. Assume no.

**(f) Numbers — all unverified.** The commonly-cited figures do **not** appear on any Windsurf-controlled page I could reach:
- **[third-party]** "Riptide … 200% improvement in retrieval recall compared to traditional embedding systems" — appears on aggregator sites, e.g. [markaicode](https://markaicode.com/windsurf-flow-context-engine/). Not found in Windsurf docs.
- **[third-party]** "10,000-line project indexes in under a minute, 100,000-line project 2–5 minutes" — [arsturn](https://www.arsturn.com/blog/optimizing-codebase-searchability-with-windsurf-indexing-features). Not found in Windsurf docs.

Do not plan against these numbers.

---

## 6. Augment Code

**(a) What it indexes.** A remote real-time semantic index. Chunk embeddings are confirmed; symbol/call-graph structure is **not** claimed anywhere I could verify. Their own product page says only that they *"semantically index and map your code, understanding relationships between thousands of files"* ([augmentcode.com/context-engine](https://www.augmentcode.com/context-engine)) — no graph structure named.

**(b) Parser technology.** Not disclosed. Custom embedding models trained in-house.

**(c) Storage.** Google Cloud — *"PubSub, BigTable, and AI Hypercomputer"* — with self-hosted embedding search rather than third-party vector APIs ([Augment: A real-time index for your codebase](https://www.augmentcode.com/blog/a-real-time-index-for-your-codebase-secure-personal-scalable)).

**(d) How it gets "instant".** Server-side streaming ingest plus per-developer index sharding:
- *"within seconds of any change to your files"*, versus competitors' *"10-minute delays"*.
- *"many thousands of files per second"* ingest, so *"branch switching is handled almost instantly."*
- **Separate indices per developer**, with *"the parts of search indices that overlap between users from the same tenant"* shared — the same dedup insight as Cursor's simhash reuse, from the other direction.
- Security mirrors Cursor's hash challenge: *"the IDE must prove to the backend it knows a file's content by sending a cryptographic hash"* (Proof of Possession).

**(e) Cross-file call graph / dataflow.** Not claimed. Assume no.

**(f) Numbers.**
- **Storage, and this one matters for codectx:** *"embeddings … can easily reach 10 GB"* for all snippets in a large codebase (their blog, above). Embedding-based indexes are *not* small.
- Onboarding bulk upload handles *"100k files or more."*
- **[vendor claim]** "500,000 files, ~100ms retrieval" is widely quoted but I could **not** find it on the Augment blog or the context-engine product page. Treat as marketing, not spec.

---

## 7. Cline and Roo Code

**Cline.** Historically the canonical "no index, parse on demand" design: a `list_code_definition_names` tool that ran tree-sitter over a directory's top-level files and returned definition names only.

**[verified by me, 2026-09-13, via GitHub REST API on `cline/cline@main`, tree untruncated]:**
- `LIST_CODE_DEF = "list_code_definition_names"` is still in the tool enum at `apps/vscode/src/shared/tools.ts`.
- **No `tree-sitter` / `web-tree-sitter` dependency** in `apps/vscode/package.json`, `sdk/packages/core/package.json`, or the root `package.json`.
- No tree-sitter source paths anywhere in the 4,643-path tree (the only hits are CHANGELOG and unrelated skill docs).
- The repo *does* ship ripgrep: `apps/vscode/scripts/download-ripgrep.mjs`.

**Inference (mine, not a vendor statement):** Cline has removed or externalised its tree-sitter parsing while keeping the tool name, and its retrieval stack is now ripgrep + file listing + read. Worth re-checking before citing.

**Roo Code** (a Cline fork) went the opposite way and built a real index ([Roo Code docs: Codebase Indexing](https://roocodeinc.github.io/Roo-Code/features/codebase-indexing)):
- **(a)** Semantic code blocks — functions, classes, methods — plus markdown-header blocks, with **line-based chunking as fallback** for unsupported languages. Blocks are **100–1,000 characters**, large functions split at logical boundaries.
- **(b)** Tree-sitter for supported languages; line chunking otherwise.
- **(c)** **Qdrant** (cloud or local Docker), user-supplied. Embeddings from Gemini / OpenAI / Ollama / Mistral / Bedrock / OpenRouter / OpenAI-compatible.
- **(d)** File watching for real-time change detection, **hash-based caching** so only modified files are reprocessed, and **branch awareness**. 1 MB file cap; respects `.gitignore` and `.rooignore`. Docs warn that 10k+ file codebases *"may experience extended indexing times."*
- **(e)** No call graph, no references. Tree-sitter is used purely as a *chunk boundary detector* — a strictly weaker use than codectx's.
- **(f)** Default similarity threshold **0.4**, max **50** results per search.

---

## 8. Aider (repo map)

The most sophisticated *index-free-ish* structural approach in the set, and the closest conceptual cousin to codectx's base layer.

**(a) What it indexes.** Tree-sitter **tags**: definitions and references. **[verified by me]** in [`aider/repomap.py`](https://raw.githubusercontent.com/Aider-AI/aider/main/aider/repomap.py): tag kind is decided by capture-name prefix — `name.definition.*` → `kind = "def"`, `name.reference.*` → `kind = "ref"`. When a language's query file only has definitions, references are **backfilled with Pygments tokenisation** (i.e. lexer-level, not parser-level).

**(b) Parser technology.** Tree-sitter via `py-tree-sitter-languages`, with `.scm` tag queries; Pygments as a degraded fallback ([Building a better repository map with tree sitter](https://aider.chat/2023/10/22/repomap.html)).

**(c) Storage.** **SQLite** on disk. Cache dir `TAGS_CACHE_DIR = f".aider.tags.cache.v{CACHE_VERSION}"` (`CACHE_VERSION = 3`, or `4` with the TSL pack), keyed by filename, validated by **mtime**: the stored value is `{"mtime": file_mtime, "data": data}` and a hit requires `val.get("mtime") == file_mtime`. A separate in-process `self.map_cache` memoises rendered maps.

**(d) How it gets "instant".**
- **mtime-keyed SQLite tag cache** — reparse only touched files.
- **Ranking instead of resolution.** A graph of files (nodes) and symbol references (weighted edges) is scored with `nx.pagerank(G, weight="weight", **pers_args)` where `pers_args = dict(personalization=personalization, dangling=personalization)` — i.e. **personalized PageRank**, biased toward symbols in the current chat. (Note: the 2023 blog post never names PageRank; the code does.)
- **Binary search to the token budget.** A `while lower_bound <= upper_bound` loop bisects how many ranked tags to render until the map fits `--map-tokens` (default **1k**).

**(e) Cross-file call graph / dataflow.** **This is the key distinction.** Aider's edges are **identifier name matches**, not resolved references. A `ref` to `foo` links to *every* file that defines something called `foo`. It is a fast, language-agnostic, deliberately imprecise *relevance* signal — excellent for ranking, useless for "who actually calls this". No dataflow. No overload/receiver-type resolution.

---

## 9. Claude Code and Codex CLI (no index)

**(a)–(c)** Nothing. No index, no database, no embeddings, no storage.

**(d) How they get "instant".** By having nothing to build. Retrieval is tool calls at inference time — `Glob` (path patterns), `Grep` (ripgrep over content), `Read`, plus an `Explore` subagent that searches in its own context window and returns a summary rather than raw file dumps.

Boris Cherny (Claude Code's creator) on HN: *"Claude Code doesn't use RAG currently. In our testing we found that agentic search out-performed RAG for the kinds of things people use Code for."* ([HN item 43164253](https://news.ycombinator.com/item?id=43164253)). The rationale repeated elsewhere is that agentic search *"is simpler and doesn't have the same issues around security, privacy, staleness, and reliability."*

**Codex CLI** is the same shape: reads the working directory, excludes `node_modules` and `.gitignore`d paths, greps. There is an **open feature request** for exactly the thing it lacks — *"Codex CLI … struggles to reliably find the right places in medium to large codebases because it lacks a first-class semantic search capability"* ([openai/codex#5181](https://github.com/openai/codex/issues/5181)).

**(e)** No graph of any kind.

**(f)** The cost is real and shows up as tokens and turns, not seconds — Cursor's own data (12.5% accuracy gain from adding semantic search on top of grep, rising to +2.6% retention on 1,000+ file repos) is the cleanest published measurement of where grep-only starts to break.

---

## 10. Tabnine

**(a)–(c)** Vector embeddings over code chunks in **two separate RAG indices** — one for completions, one for chat — because *"the code representation in each index is different and fits its relevant AI model."* Stored in a **local Qdrant** under e.g. `~/.local/share/TabNine/servers/vdb/<version>/`. Compute is split: completions indexing runs on the developer's machine, chat indexing needs GPU and runs on Tabnine servers with chunks encrypted in transit. File selection is extension-filtered (1,233 supported extensions; `md`, `yaml`, `json`, `lock`, `xml`, `txt`, `csv` excluded) with no evidence of AST parsing. ([Tabnine: Personalization in depth](https://docs.tabnine.com/main/welcome/readme/personalization/tabnines-personalization-in-depth))

**(d)** Build from scratch on first run, then *"changes are monitored and the indices are incrementally updated."* Nothing more specific published.

**(e)** No call graph, no references. The Enterprise "Context Engine" adds remote repos, CI/CD, code reviews, docs and tickets ([docs](https://docs.tabnine.com/main/administering-tabnine/managing-your-team/context-engine)) — **breadth, not depth**.

**(f)** **[vendor claim]** "up to 2× accuracy, up to 80% token reduction, up to 50% faster time to resolution" ([tabnine.com](https://www.tabnine.com/)). No index-size or build-time numbers published.

---

## 11. Supermaven

The outlier: **no retrieval index at all, by design.** No chunk index, no vector DB, no parser. The pitch is a long-context model (`Babble`) that ingests the repo directly; the context window went **300,000 → 1,000,000 tokens**, with an architecture claimed to be *"more efficient than a Transformer"*, keeping *"the cost and latency the same as a Transformer with a 4,000-token context window."* Startup cost is *"10-20 seconds processing your repository."* No graph of any kind. ([Introducing Supermaven](https://supermaven.com/blog/introducing-supermaven), [Announcing Supermaven 1.0](https://supermaven.com/blog/announcing-supermaven-1.0))

**The one idea worth stealing:** Supermaven consumes **edit sequences derived from version-control diffs** rather than file snapshots — indexing *change over time* instead of *structure at a point in time*. Nobody else in this survey does that, and codectx already has the git plumbing for it.

---

## 12. Zed

**The most decision-relevant datapoint in this survey: Zed built an embeddings semantic index, then deleted it, and has not brought it back in 2.5 years.**

**Historical design (2023).** Zed reworked *"the tree-sitter query engine for parsing symbol objects for embeddings"* with options to **collapse nested objects** to cut token count while keeping hierarchical context; switched batching from span-count to **token-count**; and added an **embeddings cache to avoid re-embedding all tree-sitter spans on every file save** ([This Week at Zed #12](https://zed.dev/blog/this-week-at-zed-12)). Technically sound work.

**Removal.** PR #7367, merged 2024-03-19, checklist item *"Remove semantic index"*, release note: **"We are temporarily removing the semantic index in order to redesign it from scratch."** ([commit 8ae5a3b](https://github.com/zed-industries/zed/commit/8ae5a3b61a5e1d5b2483154ac9248fd1818a216b))

**Current state — [verified by me, 2026-09-13, GitHub contents API]:** `crates/semantic_index` returns **404**. Of Zed's **245** crates, none matches `sem*`, `embed*`, or `vector*`. "Temporarily" has lasted ~2.5 years.

**What Zed uses instead.** Editor-grade tree-sitter + LSP for outline, symbols and go-to-definition ([Zed: Configuring Languages](https://zed.dev/docs/configuring-languages)) — real infrastructure, but wired to *editor* features, not exposed as an agent retrieval index. Users are actively asking for the index back, noting agents *"read tons of files in a repo to find what they are looking for"*; as of this writing **no Zed maintainer has replied** in [discussion #52337](https://github.com/zed-industries/zed/discussions/52337).

**(e)** No call graph for agent retrieval. LSP gives precise per-query navigation but nothing is materialised.

---

# Patterns

1. **Almost nobody resolves symbols across files.** Of twelve tools, exactly **one** — Sourcegraph — builds compiler-precise cross-file def/ref edges, and even there Cody's context service traverses **definition edges only**, with references/implementations listed as future work ([eric-fritz.com](https://www.eric-fritz.com/articles/llm-antihallucinogen)). **Zero tools in this survey do dataflow, control dependence, or reads/writes analysis.** Aider's "graph" is identifier name-matching. Everything else is chunk embeddings or nothing.

2. **Tree-sitter is overwhelmingly used as a *chunk boundary detector*, not as a semantic layer.** Roo Code (100–1,000-char blocks at function/class boundaries), Continue (`code_snippets` title/signature/content), Zed's old spans, Cursor's "syntactic chunks" — all use the AST to decide *where to cut*, then throw the structure away and keep a vector. Aider and Cody are the only two that keep the structure and do something graph-shaped with it.

3. **Content-hash caching is universal; Merkle trees are the scaled-up version.** Cursor (SHA-256 Merkle tree + chunk-hash embedding cache), Continue (`cacheKey` in `tag_catalog`), Roo Code (hash-based caching), Aider (mtime-keyed SQLite), Augment (Proof-of-Possession hashes). Nobody re-derives anything they can key by content.

4. **"Instant" almost always means *someone else already paid*, or *the work was never done*.** Three distinct mechanisms: (i) **remote precompute amortised across users** — Copilot's once-per-repo index, Cursor's simhash index reuse, Augment's shared tenant index overlap; (ii) **skip semantic resolution entirely** — embeddings or n-grams over text; (iii) **build nothing, pay per query** — Claude Code, Codex CLI. Even so, Cursor's *own* p99 indexing time was **4.03 hours** before their Merkle work ([secure-codebase-indexing](https://cursor.com/blog/secure-codebase-indexing)).

5. **The committed-base-plus-working-tree-overlay pattern is converging independently.** Cursor pins the regex index to a git commit with *"user and agent changes … stored as a layer on top of it"*; Copilot indexes the **default branch** and supplements with a local search of modified files; Continue keys every artifact by `(directory, branch, artifactId)`. Nobody tries to keep a semantically-resolved index live against every keystroke.

6. **Two confirmed retreats from local semantic indexing — and a lexical turn.** Zed **deleted** its semantic index crate in 2024 and it is still absent from all 245 crates. Continue **deprecated `@Codebase`** in favour of agent tools. Both are vendor-documented. Meanwhile Cursor invested in a *lexical* n-gram index rather than more embeddings, and Anthropic ships no index at all. *(Cline dropping its tree-sitter dependency would be a third retreat, but that is my inference from the repo — see §7 — not a vendor statement; treat as suggestive, pending confirmation.)* The 2025–26 direction of travel among client-side tools is **away from local embedding indexes, toward fast lexical search plus agent loops**.

7. **But the hybrid, not the purist, wins on measurements.** Cursor's data: semantic search **supplements** grep for +12.5% accuracy, *"the combination of these two leads to the best outcomes"* ([semsearch](https://cursor.com/blog/semsearch)). Grep-only degrades specifically on large repos — exactly where retention rose +2.6%.

8. **Embedding indexes are not cheap in storage.** Augment: *"embeddings … can easily reach 10 GB"* on a large codebase. The common assumption that vectors are lighter than structural facts is wrong at scale.

9. **Precise indexing sits in a distinct cost tier from CPG, though the gap is not cleanly quantified in public sources.** Two anchors, in different units, both worth putting in front of the product owner: SCIP indexing is run as a **per-commit CI job** by everyone who does it, never interactively, and its binding constraint is **memory** (documented OOMs in `scip-typescript`); Joern on a 10k-file repo is *tens of minutes and gigabytes* (per the codectx brief, unmeasured by me). The honest statement is that both are batch-tier, and SCIP is the cheaper of the two — not a specific ratio.

---

# Implications for codectx

1. **Reframe the Joern comparison: it is not slow, it computes a strictly larger thing — and nobody else computes it.** No tool surveyed does dataflow, control dependence, or reads/writes. One does precise call/ref edges (Sourcegraph, via compilers, in CI). The honest positioning is not "we're slower than Cursor" — it's "Cursor cannot answer the questions Joern answers, at any latency." Put the (e)-column table in front of the product owner: it is the single most decision-relevant artifact in this research.

2. **Adopt the Copilot/Sourcegraph amortisation model rather than trying to make Joern fast.** Both treat expensive analysis as an **out-of-band, per-commit, per-repo batch job** whose result is shared, then patch freshness with a cheap local delta. Concretely for codectx: Joern (and ideally SCIP) should be a CI/background artifact keyed by commit SHA, importable and cacheable, never on the interactive path. Cursor's p99 of 4 hours proves even embedding vendors tolerate long builds when they run once and are reused.

3. **Mirror Cursor's commit-pinned base + working-tree overlay explicitly, and make the overlay's precision degrade gracefully.** Cursor pins the regex index to a git commit and layers agent edits on top; Copilot indexes the default branch and locally greps modified files. codectx's existing snapshot-qualified LSP overlay (commit 11bc619) is the right shape — extend the same discipline to SCIP and Joern layers: base facts are commit-qualified and cacheable, working-tree facts come from tree-sitter/LSP and are explicitly marked lower-precision in query results.

4. **Steal Continue's four-bucket incremental algorithm verbatim for the SQLite layer.** `getComputeDeleteAddRemove()` returning **compute / addTag / delTag / removeTag** against a `(directory, branch, artifactId)` tag catalog keyed on content hash is the cleanest published solution to "user switched branches, don't redo the work." It converts a branch switch from a reindex into row bookkeeping. codectx has the harder version of this problem (structural facts, not just chunks), and the same shape applies.

5. **Make the base tree-sitter layer answer in Aider's register — ranking, not resolution — and say so.** Personalized PageRank over a name-match def/ref graph, mtime-keyed SQLite cache, binary-searched to a token budget, is ~200 lines of proven design that produces useful context with **zero semantic resolution**. It is also the natural "instant" tier that makes codectx competitive on first-run latency while SCIP/Joern warm in the background. Be explicit in the API that these edges are name-matched, not resolved — Aider's imprecision is a feature only when it is labelled.

6. **Offer SCIP as the default "precise" tier and Joern as opt-in, and price them honestly in the docs.** SCIP answers the *majority* of questions users actually ask — "who calls this", "where is this defined", "who implements this" — with compiler precision, and the whole industry runs it as a batch CI job rather than interactively. Reserve Joern for what SCIP genuinely cannot answer: control/data dependence, taint, reads/writes. Publish a per-tier cost table measured **on codectx's own corpus** (tree-sitter, SCIP, Joern — wall clock, peak RSS, index bytes); no public source gives defensible SCIP throughput numbers, so measure rather than cite. Plan for per-project sharding: `scip-typescript` documents OOMs from its cross-project symbol cache, which says **memory is the binding constraint on precise indexing**, not time.

7. **Do not treat index size as a differentiator against embedding tools — it isn't.** Augment's 10 GB of embeddings for a large codebase means a multi-GB SQLite of structural facts is not out of family. The argument to make is *facts per byte*: structural facts are queryable, joinable, and explainable; vectors are none of those. **[Speculative]** codectx's SQLite for 10k files will likely land well under Augment's 10 GB for comparable repos, but I have no measurement — worth benchmarking and publishing, since no competitor has.

8. **Add a lexical fast path; it is where the market is actually moving.** Cursor built a client-side sparse n-gram index because `rg` took *"more than 15 seconds"* on monorepos, and reports a Chromium workflow dropping 240 s → 90 s. Meanwhile Claude Code and Codex CLI ship no index at all and Anthropic reports agentic search beat their own RAG. A trigram/n-gram index in the same SQLite file is a cheap, always-fresh tier that (a) makes codectx useful on second one, (b) serves the MCP grep tool that agents will reach for anyway, and (c) hedges the risk that the whole semantic-index category keeps deflating the way it did at Zed, Continue, and Cline.
