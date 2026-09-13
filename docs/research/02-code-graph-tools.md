# Open-source code-graph / knowledge-graph tools for LLM agents

Research slice 02 for **codectx**. Date of survey: 2026-09-13.

## Method and one warning about the star numbers

Every tool below was checked against primary sources: the GitHub API for metadata, a `git clone --depth 1` plus source reading for the six tools whose resolution strategy actually matters, and vendor docs/papers for the rest. Where I read the resolution function itself I say so; where I'm only repeating a README claim I say that too.

**The star counts in this ecosystem are not a usable adoption proxy in 2026.** GitHub's own top-30 cohort now contains several repos created in 2026 sitting above `torvalds/linux` (`mattpocock/skills` 261k stars, created 2026-02-03; `affaan-m/ECC` 257k, created 2026-01-18 — via `gh api search/repositories?q=stars:>100000`). Against that backdrop, stars mostly measure distribution, not use. One number is anomalous even by 2026 standards: **Graphify-Labs/graphify reports 116,402 stars against 1 watcher**. Compare the controls — Serena 29,270 stars / 93 watchers (315:1), Onyx 32,061 / 160 (200:1), Potpie 5,719 / 33 (173:1), Joern 3,489 / 38 (92:1). A 116,402:1 ratio is not in the same distribution. `colbymchenry/codegraph` (70,677 / 169 = 418:1) and `tirth8205/code-review-graph` (31,377 / 102 = 307:1) are high but in-band. I report raw counts below; **treat graphify's in particular as unverified signal, not adoption.** (Speculation: star-farming or a promotion campaign; I have no direct evidence either way.)

---

## Tier A — tools whose internals directly inform codectx

### codebase-memory-mcp (DeusData) — the closest architectural twin

- **Stars/activity**: 43,114 stars, 174 watchers, created 2026-02-24, pushed daily. Note a same-content mirror at `astandrik/codebase-memory-mcp`. [repo](https://github.com/DeusData/codebase-memory-mcp)
- **Parser**: tree-sitter, 162 vendored grammars compiled into a **pure C** binary (not Go, despite a vestigial `pkg/go/go.mod`; the core is `src/main.c` + `src/pipeline/*.c`). Optional "Hybrid LSP" type resolution for 12 languages, implemented in-process under `internal/cbm/lsp/` (`py_lsp.c`, `ts_lsp.c`, `rust_lsp.c`, …) rather than by shelling out to real language servers.
- **Edges**: 18 types — `CALLS`, `HTTP_CALLS`, `ASYNC_CALLS`, `IMPORTS`, `IMPLEMENTS`, `INHERITS`, `DECORATES`, `USES_TYPE`, `USAGE`, `MEMBER_OF`, `THROWS`, `READS`, `WRITES`, `TESTS`, `FILE_CHANGES_WITH`, `HANDLES`, `CONFIGURES`, `CONTAINS_*` ([arXiv:2603.27277](https://arxiv.org/html/2603.27277v1)). **No control-flow or data-dependence graph** — `READS`/`WRITES` are syntactic, not a PDG.
- **Cross-file resolution — read from source, the most valuable artifact in this survey.** `src/pipeline/registry.c` documents a prioritised strategy chain with hardcoded confidences:

  | Strategy | Confidence |
  |---|---|
  | `import_map` (callee name found in the file's import map) | 0.95 |
  | `same_module` (same file/package) | 0.90 |
  | `import_map_suffix` | 0.85 |
  | `unique_name` (exactly one candidate project-wide) | 0.75 |
  | `suffix_match` (multiple candidates, import-distance scored) | 0.55 |
  | fuzzy single / multi | 0.40 / 0.30 |

  The paper admits "**strategies 1–3 resolve ∼80% of calls in well-structured codebases**" — i.e. ~20% of emitted `CALLS` edges are `unique_name` or worse. An optional LSP override sits *above* the registry with a **0.6 confidence floor** ([`src/pipeline/lsp_resolve.h`](https://github.com/DeusData/codebase-memory-mcp/blob/main/src/pipeline/lsp_resolve.h)); that header's own comment records a shipped bug where the sequential and parallel pipelines used 0.6 vs 0.5 floors and "the same project produced different CALLS-edge attributions depending on which pipeline mode kicked in."
- **The scaling wall, quantified.** `registry.c` caps candidates at 256 with this comment: on the Linux kernel, **274 names exceed 256 candidates** (`list_head` 7188, `flags` 5520, `dev` 4374) and "accounted for ~900 s of the 987 s **usage-resolution** CPU." The measurement is from the `USAGES` pass specifically; the same registry and the same 256 cap are shared by `pass_calls.c`, `pass_parallel.c`, `pass_semantic.c` and `pass_lsp_cross.c`, so the pathology applies to `CALLS` too but was not separately measured there. Either way: 91% of that pass's CPU went to names that were unresolvable by name at all.
- **Storage**: single SQLite file, WAL, deferred index creation, bulk insert; RAM-first pipeline (in-memory SQLite, LZ4, Aho-Corasick) then flush.
- **Speed**: Django **~6 s** (49K nodes, 196K edges); Linux kernel **3 min** (28M LOC, 75K files → 4.81M nodes, 7.72M edges); "fast index" mode 1m12s. Queries <1 ms. Incremental re-index **~1.2 s**, claimed 4× vs full.
- **Incremental**: XXH3 hash + mtime/size per file; changed files' nodes deleted (edges cascade via `ON DELETE CASCADE`), re-parse only those, merge ([`pipeline_incremental.c`](https://github.com/DeusData/codebase-memory-mcp/blob/main/src/pipeline/pipeline_incremental.c)). Also ships a committed `.codebase-memory/graph.db.zst` snapshot so teammates skip the first index — with an honest warning that committing every refresh turned 20 MB into "~6 GB across ~350 commits" for one team.

### Serena — the real-resolution datapoint, and its cost

- **Stars/activity**: 29,270 stars, 93 watchers, created 2025-03-23, very active. [repo](https://github.com/oraios/serena)
- **Parser/resolution**: none of its own. It drives **real language servers over LSP** (40+ languages) via a vendored `solidlsp` layer, or a JetBrains backend. This is genuine compiler-grade resolution — `find_referencing_symbols`, `find_declaration`, `find_implementations`, `type_hierarchy`.
- **Edges**: no persistent graph at all. Serena answers *queries*; it never materialises a call graph you can traverse offline.
- **Storage**: `serena project index` exists (`src/serena/cli.py:790`) but it pre-warms a **pickled LSP cache** (`src/solidlsp/util/cache.py`), not a queryable graph.
- **The cost it pays**: its CHANGELOG is a catalogue of language-server lifecycle pain — tsserver V8 OOM mid-indexing, Metals answering from a stale index before build import finishes, two Serena processes racing the same on-disk index storage, and three new settings (`indexing_timeout`, `indexing_start_grace`, `indexing_quiet_period`) added just to know when a server is actually ready. **That is the tax codectx's LSP overlay inherits.**

### Blarify — the closest prior art to codectx's layering, and the single best number in this survey

- **Stars/activity**: 231 stars, 45 watchers, created 2024-03-20, pushed 2026-08-17. Low stars, high relevance. [repo](https://github.com/blarApp/blarify)
- **Architecture**: tree-sitter for structure + **a hybrid SCIP/LSP resolver for references** (`blarify/code_references/hybrid_resolver.py`), with modes `SCIP_ONLY`, `LSP_ONLY`, `SCIP_WITH_LSP_FALLBACK`, `AUTO`. SCIP is used only where an indexer exists (`scip-python`, `scip-typescript`); every other language falls back to LSP via a vendored `multilspy` (jedi, gopls, omnisharp, intelephense, solargraph, dart).
- **The number**: "**SCIP provides identical accuracy to LSP but with dramatically better performance** … up to **330x faster** reference resolution" ([`docs/SCIP_SETUP.md`](https://github.com/blarApp/blarify/blob/main/docs/SCIP_SETUP.md), and the same claim in `scip_helper.py`'s docstring). Vendor-stated, not independently verified — but it is the design rationale codectx's SCIP layer is built on, stated by someone who shipped both paths.
- **Edges**: `IMPORTS`, `CALLS`, `USES`, plus containment (`relationship_type.py`).
- **Storage**: Neo4j or FalkorDB. **Not embedded** — this is where codectx's SQLite choice diverges.
- **Incremental**: `project_graph_diff_creator.py` exists; README "Future Work" still lists "graceful handling of file additions, deletions, or modifications" and "parallelizing language server requests" as open. Incremental LSP is admitted as unsolved.

### RepoGraph (ICLR 2025) — the canonical citation for what name-matching costs you

- **Stars/activity**: 302 stars, created 2024-08-08, last push 2025-04-01 (dormant). [repo](https://github.com/ozyyshr/RepoGraph) · [paper](https://arxiv.org/abs/2410.14684)
- **Parser**: tree-sitter with an inline tags query (def-class, def-function, ref-call), aider-lineage.
- **Resolution — this is the whole mechanism**, from `repograph/construct_graph.py`:
  ```python
  for tag in tags_ref:
      for tag_def in tags_def:
          if tag['name'] == tag_def['name']:
              G.add_edge(tag['name'], tag_def['name'])
  ```
  A global cross-product on bare identifier text. No imports, no scoping, no receiver typing. The `get_tags_raw` path even string-patches source before parsing (`code.replace("True", "_True")`, `code.replace("print ", "yield ")`) to force tree-sitter through Python-2-era files.
- **What it costs, visible in their own stats**: SWE-bench test repos average **1,419.3 nodes and 26,392.1 edges** — an 18.6:1 edge-to-node ratio. That density is the name-collision cross-product, not real call structure. They filter stdlib/third-party names to trim it, and the paper concedes "the enhancement of accuracy in line-level is comparatively modest" with "contextual misalignment … the most prevalent error type."
- **Payoff**: +2.34 absolute resolve rate on SWE-bench Lite over Agentless/GPT-4o (27.33% → 29.67%, +8.56% relative). **A deliberately imprecise graph still helped.** That is the most important finding here.

### code-graph-rag (vitali87) — the most serious tree-sitter-only type inference I found

- **Stars/activity**: 5,132 stars, 40 watchers, created 2025-06-16, pushed daily. [repo](https://github.com/vitali87/code-graph-rag)
- **Parser**: tree-sitter, 13+ languages with per-language frontends (`parsers/py_frontend`, `go_frontend`, `java_frontend`, `csharp_frontend`, `cpp_frontend`, `js_ts`, `rs`, …).
- **Resolution**: a real `TypeInferenceEngine` plus `call_resolver.py` — receiver-chain splitting that avoids breaking on `.` inside call args/generics, constructor-inferred receiver types, inheritance walks for method lookup, re-export following (`follow_reexports`), and a `FunctionRegistryTrie`. It even models cross-language call legitimacy (`_CALLABLE_LANGUAGE_FAMILIES`: JS/TS one runtime, C++→C, Scala→Java).
- **What it gives up, from its own comments**: everything the typing can't place falls through to a **"simple-name fallback"** against the trie. `call_resolver.py:1087` — *"Full-fallback by design: … edges the precise path could not place"*; `:1298` — *"fallback for C++ to avoid dropping edges the typing can't yet recover"*; `:1408` — the risk it names is the trie fallback *"rebind[ing] it to an unrelated first-party symbol of the same name."* So: real inference on the typed path, name-matching underneath, and the tool knows it.
- **Storage**: Memgraph (primary), with a Neo4j driver adapter. In-memory graph DB, requires a server.
- **Speed/precision**: **no published indexing-time or precision numbers.** They ship an `evals/` harness (`ast_oracle.py`, `calls_trace.py`, `flow_ground_truth.py`, `dead_code.py`) but no results table in the repo. Roadmap admits dead-code false-positive reduction is "an ongoing campaign."

### codegraph (colbymchenry) — the fastest published numbers

- **Stars/activity**: 70,677 stars, 169 watchers, created 2026-01-18. Ratio in-band. [repo](https://github.com/colbymchenry/codegraph)
- **Parser**: tree-sitter via a native Rust kernel, ~20–26 languages, "one boundary crossing per file."
- **Edges**: calls, imports, inheritance, implementation, framework routing (route → handler), **dynamic-dispatch hops (callbacks, interface→impl)**, event emitters, component hierarchies.
- **Resolution**: a post-extraction reference-resolution pass (calls→defs, imports→sources, inheritance). Uncertain edges (dynamic dispatch, bridged Swift↔ObjC / React Native) are **tagged as uncertain rather than hidden**.
- **Storage**: SQLite + FTS5 at `.codegraph/codegraph.db`, WAL.
- **Speed**: Swift compiler repo, **27k files → ~100 s fresh index; one-file edit re-syncs in ~4 s**. Linux kernel (70k files) "under 12 minutes." Claims 2–7× faster re-index than competitors.
- **Incremental**: native FS events (FSEvents/inotify/ReadDirectoryChangesW), 2 s debounce, per-file staleness banners while a sync is pending.

### Potpie — graph as SDLC context, not just code

- **Stars/activity**: 5,719 stars, 33 watchers, created 2024-08-12, active. [repo](https://github.com/potpie-ai/potpie)
- **Scope shift**: now positions as "Context Graph for AI Native SDLC" — 24 entity types and 25 public edge types spanning code *plus* decisions, source history, team knowledge, and workflow integrations (GitHub, Linear, Jira, Confluence), per `docs/context-graph/ontology.md`.
- **Storage**: **embedded FalkorDB over a local file, "no Docker, server, Neo4j, or cloud"** (`docs/context-graph/index.md:75`) — with a backend matrix of `in_memory`, `embedded`, `falkordb_lite`, `falkordb`, `neo4j`.
- **Notable design**: an import-time *coherence guard* that fails startup if the derived views drift from the three catalogs — an ontology-as-contract pattern worth stealing.
- **Resolution/speed**: parsing lives in `potpie/parsing/parsing/py_graph.py` and a sandboxed `parser_runner`; **no published resolution method or indexing-time numbers.**

### graphify — huge stars, an LLM in the loop, and an honest edge-provenance tag

- **Stars/activity**: 116,402 stars / **1 watcher** (see warning above), 11,374 forks, created 2026-04-03. [repo](https://github.com/Graphify-Labs/graphify)
- **Parser**: tree-sitter per-language extractors (`graphify/extractors/*.py` — 30+ languages) for code. **The "no LLM" claim is scoped**: README:32 says code is deterministic tree-sitter, but "Docs, PDFs, images and video use your assistant's model … for a semantic pass." `tools/skillgen/fragments/references/shared/extraction-spec.md` is a prompt instructing a model to emit `calls|implements|references|cites|semantically_similar_to` edges with hand-picked confidence scores, and in `--mode deep` to "be aggressive with INFERRED edges." So code edges are deterministic; document edges are model-generated.
- **Best idea in the tool**: every edge carries `EXTRACTED` (explicit in source) / `INFERRED` (resolved) / `AMBIGUOUS`, with `EXTRACTED` pinned to confidence 1.0. Agents can filter by provenance.
- **Resolution**: `symbol_resolution.py` — "Deterministic symbol indexing and **conservative** cross-file resolution," gated so only `file_type == "code"` nodes can be callees (a Markdown heading matching an identifier must not become a call target). There's a `scip_ingest.py`, but its own docstring says it's a "simplified subset … NOT a full SCIP protobuf implementation … **Not wired to the CLI in this phase.**"
- **Admitted precision loss, verbatim**: `cross_repo_calls.py` — "A single-repo build can only bind `obj.method()` when the receiver's type is declared in that same build. When the type lives in another repository **the resolver holds the receiver type and drops the call anyway** … A two-repo call graph was therefore missing exactly the edges that make it a call graph: eight edges when the code sits in one corpus, seven after merging." Their fix parks unresolved calls as `metadata.unresolved_calls` and re-emits only when the receiver type resolves to **exactly one** declaration — "an ambiguous name still fabricates nothing."
- **Storage**: NetworkX in memory, serialised to `graph.json` / `global-graph.json`. No database.

---

## Tier B — relevant, thinner evidence

**code-review-graph** (31,377 stars / 102 watchers, created 2026-02-26, [repo](https://github.com/tirth8205/code-review-graph)) — tree-sitter, 30+ languages, **SQLite** in `.code-review-graph/`. Uses the same `EXTRACTED/INFERRED/AMBIGUOUS` confidence taxonomy as graphify (convergent or derivative; unclear which). Notable for publishing numbers *against itself*: search **MRR 0.35**, **flow detection 33% recall**. Build: Flask (83 files) 95 ms flow detection, 0.7 ms search; FastAPI (1,122 files) 128 ms / 1.5 ms. Incremental: SHA-256 per file, find dependents via graph edges, re-parse changed only — **~2.5 s for a two-file edit on a ~3,000-file Django-scale project.** Token reduction median ~65× (range 36×–376×).

**GitNexus** — canonical upstream is [abhigyanpatwari/GitNexus](https://github.com/abhigyanpatwari/GitNexus): **47,300 stars, 165 watchers** (287:1, in-band with the controls), created 2025-08-02, pushed 2026-09-13. Worth noting as a finding about the niche: search surfaces at least four near-identical "GitNexus" repos with the same description (`ZatesloFL/GitNexus` 0 stars, `acme-architecture/gitnexus` a fork of a third `nxpatterns/gitnexus`, plus others), so name-squatting/re-uploading is common enough here to make repo identity itself unreliable. Tree-sitter native (CLI) / WASM (browser), storing into **LadybugDB**, an embedded graph DB with vector support (described in a fetched summary as "formerly KuzuDB" — *rename unverified against a primary source*), native or WASM. Documented 6-stage pipeline: structure → parse → **resolution (imports, calls, heritage, constructor inference, receiver types) with "language-aware logic"** → community detection → process tracing → hybrid search. Publishes a per-language support matrix (imports / named bindings / exports / heritage / type annotations) so precision is legible per language. Incremental via `gitnexus analyze --watch` with debounce and serialised refresh. Third-party audit claims 88% fewer tool calls, 74% token savings ([rywalker](https://rywalker.com/research/code-intelligence-tools)).

**Nuanced** — **archived**. `nuanced-dev/nuanced-py` is 128 stars, `archived: true`, last push 2025-06-26; `nuanced.dev` now 308-redirects to `archive.nuanced.dev`. Python-only function call graphs built on a fork of the Jarvis call-graph generator ([nuanced-dev/jarviscg](https://github.com/nuanced-dev/jarviscg)). **The finding is the shutdown**: a well-funded, narrowly-scoped, single-language call-graph-for-agents product did not survive 2025. Speculation: single-language call graphs were too thin a wedge once agents got tree-sitter-wide tools.

**Greptile** (closed source) — indexes the repo up front, parses every file to extract "directories, files, functions, classes, variables," then a "Relationship Mapping" step connects "function calls, imports, dependencies, variable usage" ([docs](https://www.greptile.com/docs/how-greptile-works/graph-based-codebase-context)). Third-party reporting adds that they recursively generate docstrings per AST node and embed those for semantic search. **No parser, resolution mechanism, indexing time, or limitation is disclosed.** Do not infer internals from the product.

**Sourcebot** (3,936 stars, [repo](https://github.com/sourcebot-dev/sourcebot)) — **not a code graph.** Its own docs: "Code navigation is **search-based** … it uses the same code search engine and query language to **estimate** a symbol's references and definitions." Definitions come from **universal-ctags** (`sym:` filter); references are regex `\b{symbolName}\b` plus repo/language filters ([docs](https://docs.sourcebot.dev/docs/features/code-navigation)). This is the floor of the resolution taxonomy — and the most honest about it.

---

## Tier C — named in the brief but not code-graph tools

- **Onyx** (32,061 stars) — enterprise RAG/chat platform over 50+ document connectors, vector+keyword index. **No AST parsing, no call or import edges.** [repo](https://github.com/onyx-dot-app/onyx)
- **code-context / claude-context (Zilliz)** (12,522 stars, renamed, last push 2026-07-14) — AST-aware *chunking* then embeddings into Milvus/Zilliz. **Builds no graph edges whatsoever**; purely hybrid BM25 + dense retrieval. The one transferable idea is **Merkle-tree incremental indexing** for detecting changed files. [repo](https://github.com/zilliztech/claude-context)
- **CocoIndex** (11,545 stars, Rust) — tree-sitter *syntactic chunking* into pgvector; no calls/imports. The relevant part is its **incremental model**: Postgres tracks data lineage and only recomputes what changed, propagating through dependency chains. No published cache-hit numbers. [blog](https://cocoindex.io/blogs/index-code-base-for-rag)
- **Codebase-digest / CodeIndexer / "code index MCP" family** — a crowded long tail of concatenation-and-search tools (`johnhuang316/code-index-mcp`, `Consiliency/Code-Index-MCP`, `Indiejayk8s/CodeIndexer` = Milvus embeddings, `groxaxo/mcp-code-indexer` = Qdrant+SQLite). Mostly context-packing or vector search, not structural graphs.
- **Memgraph "Graph-Code"** — a demo/showcase of `code-graph-rag` on Memgraph, not an independent tool. [blog](https://memgraph.com/blog/graphrag-for-devs-coding-assistant)
- **Joern** (3,489 stars, 38 watchers) — the CPG baseline. For scale context: batch processing is known-broken past ~2,480 CPGs even at 20 GB heap ([issue #451](https://github.com/joernio/joern/issues/451)); `importCode` spawns a second JVM at the same max-heap; docs advise invoking the frontend yourself for large codebases. v4.0.0 "flatgraph" bought ~40% less memory. **Nothing in Tier A/B attempts anything close to Joern's dependence analysis** — the entire field has walked away from PDGs.

---

## Patterns

**1. Nobody in the popular tier resolves calls properly. They rank name matches and attach a confidence.** The universal shape is a ladder: import-map hit → same-module → unique-name-globally → suffix/fuzzy. codebase-memory-mcp publishes exact weights (0.95 / 0.90 / 0.85 / 0.75 / 0.55 / 0.40 / 0.30) and admits the top three cover only ~80% of calls. Sourcebot sits at the floor (ctags + `\b` regex). RepoGraph sits *below* the floor (global name cross-product). code-graph-rag and GitNexus climb highest — real receiver typing and inheritance walks — but both keep a simple-name fallback underneath, by design, to avoid dropping edges. **The cheapness comes from never needing the compiler's symbol table.**

**2. The precision given up is exactly the interprocedural part.** Method calls through a receiver whose type is declared elsewhere, dynamic dispatch, interface→impl, reflection, and cross-repo boundaries. graphify's `cross_repo_calls.py` is the clearest confession: a two-repo call graph lost *seven of eight* call edges precisely because receiver types crossed the build boundary. codegraph's honest move is to *tag* dynamic-dispatch edges as uncertain rather than suppress them.

**3. Provenance tagging is the emerging consensus for living with imprecision.** graphify's `EXTRACTED`/`INFERRED`/`AMBIGUOUS`, code-review-graph's identical triple, codebase-memory-mcp's numeric confidence + `cbm_confidence_band()` ("high" ≥0.7, "medium" ≥0.45, "speculative" ≥0.25), codegraph's uncertain-edge tags, GitNexus's per-language support matrix. **Don't hide the guess; label it and let the agent filter.**

**4. Name-keyed resolution has a quadratic wall, and it is quantified.** codebase-memory-mcp on the Linux kernel: 274 identifiers exceed 256 candidates (`list_head` 7188), and those names alone burned ~900 s of the 987 s of **usage**-resolution CPU — for edges whose confidence floors to ≤0.006 and are pure noise. Their fix is to bail out above 256 candidates, in the shared registry that also serves the call pass. **The cost of name matching is paid on exactly the identifiers where it produces nothing.**

**5. "Incremental" universally means mtime/hash-keyed per-file re-parse plus cascade delete — and that is only sound *because* edges are name-keyed.** codebase-memory-mcp: XXH3 compare → delete the file's nodes → edges cascade → re-parse → merge (~1.2 s). code-review-graph: SHA-256 → find dependents via edges → re-parse (~2.5 s on 3k files). codegraph: FS events, 2 s debounce, ~4 s per edit on a 27k-file repo. **This is the hidden trade — and flagging it as my inference, not a claim any tool makes.** A name-keyed edge can be recomputed from one file plus a global symbol index. A *resolved* edge — SCIP or LSP — has a non-local invalidation set: changing a base class or a re-export invalidates edges in files that did not change. No tool surveyed states this; I infer it from `pipeline_incremental.c`'s cascade-delete-and-reparse design (which is only sound if edges are locally re-derivable) combined with Blarify listing incremental file add/delete/modify handling as open Future Work — the one tool that *does* resolve properly is also the one that hasn't shipped incremental. The fast tools appear to have bought their fast incremental updates with the same imprecision that bought their fast full index.

**6. Storage is bifurcating toward embedded.** SQLite (codebase-memory-mcp, codegraph, code-review-graph), embedded FalkorDB (Potpie), embedded Kuzu/LadybugDB (GitNexus). Server-required graph DBs (Blarify's Neo4j, code-graph-rag's Memgraph) correlate with lower adoption. Plain JSON/NetworkX (graphify, RepoGraph) doesn't scale but ships instantly.

**7. Imprecise graphs still measurably help.** RepoGraph's +8.56% relative on SWE-bench Lite came from a *global-name-cross-product* graph. Token reductions cluster at 60–120× (code-review-graph median 65×, codebase-memory-mcp ~120×). **The bar for usefulness is far below soundness.**

---

## Implications for codectx

1. **Ship the tree-sitter base layer with an explicit confidence ladder and store the strategy name on every edge.** Copy codebase-memory-mcp's shape (`import_map` → `same_module` → `unique_name` → `suffix`) but persist `(strategy, confidence, candidate_count)` as columns, not just a score. Every serious tool converged on this; the ones that didn't (RepoGraph, Sourcebot) are the ones whose edges you can't trust. Expose a confidence floor as an MCP query parameter so the agent chooses precision vs recall per question.

2. **Cap candidate fan-out early — 256 is a validated number.** codebase-memory-mcp measured 91% of its *usage*-resolution CPU (~900 s of 987 s on the Linux kernel) going to 274 identifiers that produce only noise edges, and caps at 256 in the registry shared by every resolution pass. Put the cap in before you profile rather than after; on a kernel-scale repo it is the difference between a 3-minute index and one dominated by a few hundred hopeless identifiers.

3. **Your SCIP layer is the strongest differentiator you have, and Blarify's "identical accuracy to LSP, 330× faster" is the argument for it.** Nobody in the popular tier ships working SCIP — graphify's `scip_ingest.py` is explicitly a stub "not wired to the CLI," and Blarify (231 stars) is the only one doing it for real. A precise `CALLS` edge that costs an offline `scip-python` run, not a live language server, is a position no 30k-star competitor currently occupies.

4. **Budget for the non-local invalidation problem now, because it is what "instant" competitors avoided rather than solved.** Name-keyed edges re-derive from one file; SCIP/LSP-resolved edges do not. Concretely: store resolved edges with a provenance tag and the *inputs* they depended on (the SCIP index generation, the set of symbols consulted), and on incremental update either (a) demote stale resolved edges back to the tree-sitter ladder until the next SCIP run, or (b) mark them stale and let queries see it. Do not silently serve resolved edges from a superseded index — that's a worse failure than a labelled guess.

5. **Reframe Joern from "the graph" to "an offline enrichment pass on a chosen slice."** No popular tool attempts dependence analysis, and Joern's own scaling limits (batch broken past ~2,480 CPGs, a second JVM per `importCode`) say why. Tens of minutes is disqualifying as an index step but fine as an opt-in, per-subsystem, cached artifact — run it on the 200 files an investigation actually touches, key the result to a commit SHA, and let it expire. Positioning it as "the base layer takes seconds; dependence analysis is an on-demand deepening" turns your slowest component from a liability into the thing nobody else has.

6. **Adopt a committed, compressed graph artifact — and copy the warning, not just the feature.** codebase-memory-mcp's `.codebase-memory/graph.db.zst` means a teammate clones and gets the graph without indexing. Their documented failure — "20 MB file into gigabytes of history — one team reached ~6 GB across ~350 commits" — is the design constraint: write the artifact on release cadence, never on watcher tick, and default it to gitignored.

7. **Publish the numbers your competitors don't.** code-graph-rag has the best type inference in the field and publishes **zero** precision numbers; Potpie and Greptile publish none either. code-review-graph publishes MRR 0.35 and 33% flow recall and is more credible for it. You have SCIP ground truth available — measure tree-sitter-ladder `CALLS` edges against SCIP-resolved ones on a few real repos and publish precision/recall per strategy tier. That single table would be the most defensible claim in this entire category.

8. **The LSP overlay should be optional, capped, and per-query — not an index-time dependency.** Serena's CHANGELOG is the cautionary evidence: tsserver OOMs mid-index, Metals answering from stale indexes, two processes racing one index directory, three new timeout settings just to detect readiness. If codectx blocks indexing on language-server readiness it inherits all of it. Use LSP as an override *above* the ladder with a confidence floor (codebase-memory-mcp uses 0.6 — and shipped a bug where two code paths disagreed 0.5 vs 0.6 and produced different graphs, so define the floor in exactly one place).
